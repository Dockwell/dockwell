// SPDX-License-Identifier: AGPL-3.0-only

// Package dockertest is a fake Docker Engine for tests: an HTTP server on
// a unix socket that speaks the endpoints the docker package uses, and
// behaves as the engine does where Dockwell can tell. Containers take what
// they leave out from their image, as the engine fills them in; volumes a
// container mounts are made when missing; a volume or network in use can't
// be removed.
package dockertest

import (
	"cmp"
	"context"
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"maps"
	"net"
	"net/http"
	"net/http/httptest"
	"net/netip"
	"os"
	"path/filepath"
	"regexp"
	"slices"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/dockwell/dockwell/internal/docker"
)

// Image is an image the fake can pull.
type Image struct {
	Config docker.Config
	// Address is where a running container of it says it is, in place of an address on
	// its network, so a test can answer a real check from there.
	Address string
	// Volumes are paths the image declares, which get anonymous volumes.
	Volumes []string
	// Exits makes a container of it stop right after it starts, with
	// exit code 1.
	Exits bool
	// Unhealthy makes its health check fail, and Starting keeps it
	// starting, neither passing nor failing yet.
	Unhealthy, Starting bool
	// CheckSays is what a failing health check prints.
	CheckSays string
	// Size is what pulling it downloads, in bytes.
	Size int64
	// Digest is what its registry calls it. Without one it has one of its own, so
	// a tag moves when a test gives it an image with another.
	Digest string
	// PullError fails a pull of it, with what a registry would say.
	PullError string
	// Architecture is what it's built for: amd64, as the fake server is, if it's empty.
	Architecture string
	// PullTime is how long a pull of it takes, reporting along the way.
	PullTime time.Duration
	// Prints is what a container of it writes to stdout as it starts.
	Prints []string
	// Exec runs a command in a container of it, and says how it exited. Nil has
	// no commands, as an image without a shell.
	Exec func(Exec) int
	// Shell is the path of the shell a container of it has, such as "/bin/sh",
	// which an exec with a terminal runs as this machine's /bin/sh on a
	// pseudo-terminal of its own. Empty has none, as a distroless image.
	Shell string
	// Login, as "username:password", keeps it private: a pull, and asking
	// whether one can, find it only with that login in X-Registry-Auth, as a
	// private image's registry does (ADR 0086).
	Login string
}

// Engine is a fake Docker Engine.
type Engine struct {
	memoryKills []memoryKill
	// ahead is how far the engine's clock is ahead of this machine's, for what it
	// stamps its events and its containers' states with (SetClockAhead).
	ahead time.Duration
	// events are what the engine says happened to its containers, the newest
	// docker.EventsHeld of them, as Docker keeps them: in memory, and gone when
	// the engine is (DaemonKilled).
	events []event
	// LiveRestore makes /info say containers keep running while the engine
	// restarts.
	LiveRestore bool
	// EventsFail makes GET /events fail.
	EventsFail bool

	// Socket is where it listens.
	Socket string
	// APIVersion and MinAPIVersion are what /version reports.
	APIVersion, MinAPIVersion string
	// DockerRoot is where /info says the engine keeps its data:
	// /var/lib/docker unless set.
	DockerRoot string
	// Rootless makes /info say the engine runs as an ordinary user.
	Rootless bool
	// NoNewPrivileges makes /info say the engine sets no-new-privileges for
	// every container.
	NoNewPrivileges bool
	// UsernsRemap makes /info say the engine has userns-remap, and has it
	// refuse a container on the host network that isn't in the server's user
	// namespace.
	UsernsRemap bool
	// PickSubnet, when set, is the subnet a network with none configured gets,
	// as the engine would pick one that is free on the server.
	PickSubnet string
	// nextSubnet is the next subnet a network with none configured gets.
	nextSubnet int
	// LoggingDriver is what /info says keeps containers' logs: json-file
	// unless set.
	LoggingDriver string
	// Registries are the registries /info says its daemon.json names, as insecure
	// ones, reached over plain HTTP.
	Registries []string
	// Mirrors are the registry-mirrors /info says its daemon.json names.
	Mirrors []string
	// nextHost is how many addresses it has given out.
	nextHost int

	srv *httptest.Server

	mu sync.Mutex
	// published is every image a pull has seen, by digest: a registry keeps old
	// ones until they are forgotten.
	published map[string]Image
	intercept func(*http.Request) error
	// Registry has the images a pull can find, by reference.
	Registry map[string]Image
	// Find, when set, finds an image Registry doesn't have.
	Find func(ref string) (Image, bool)
	// RegistryDown has the registry not answer, so the engine can't say what it
	// has.
	RegistryDown bool
	// Hub is what a search of Docker Hub finds, by name.
	Hub []docker.Found
	// NCPU is how many CPUs the fake server has. Zero means 8.
	NCPU int
	// BindError, when set, says why a server path can't be mounted as a
	// container starts, or nothing when it can.
	BindError  func(source string) string
	images     map[string]*image // pulled, by reference and by ID
	containers map[string]*container
	volumes    map[string]*docker.Volume
	networks   map[string]*docker.Network
	// Pulls counts pulls, and Creates containers created.
	Pulls, Creates int
	clock          int64 // orders what is made
	pids           int   // gives running containers process IDs
	// Requests logs each request, as "METHOD /path".
	Requests []string
	// Logins logs each X-Registry-Auth sent, as "image as username:password".
	Logins []string
	// BuilderLogins logs each login a builder's Docker config.json held, as
	// "registry as username:password".
	BuilderLogins []string
	// followers counts the requests following a log now.
	followers atomic.Int64
	// Imports counts images made with docker import, and Loads those docker load
	// took.
	Imports, Loads int
	// buildCache is what the stand-in for BuildKit has built, by step.
	buildCache map[string]bool
	builders   []docker.CreateRequest
	// railpackCopies counts the containers that copied Railpack into its volume.
	railpackCopies int
	// miseSeeds counts the containers that fetched mise into it.
	miseSeeds int
	execs     map[string]*execRun
	helpers   []docker.CreateRequest
	starts    map[string]int
	// volumeRoot is where what's in each volume is kept, once one is asked for.
	volumeRoot string
	// VolumeSizes is what /system/df says each volume holds, by name: nothing
	// unless set. ContainerSizes is what each container wrote to its own layer,
	// by name without the slash.
	VolumeSizes, ContainerSizes map[string]int64
}

type image struct {
	// imported says it came from docker import, so a container of it is a helper.
	imported bool
	// created is when it was made, for an image a build made.
	created time.Time
	id      string
	digest  string
	repo    string
	Image
}

type container struct {
	docker.Container
	img     *image
	created int64 // when it was made, in a count that only goes up
	// memorySwap is the swap Docker holds for it: twice a memory limit it was
	// made with, and none without one.
	memorySwap int64
	log        containerLog
	// reports is what its inspect says besides what the client has a field for, by
	// section, as a real engine says far more than it reads.
	reports map[string]map[string]any
	// started closes as a helper starts, and exited as it ends.
	started, exited chan struct{}
	// shutDown says the engine stopped it as the engine itself was stopped
	// (ShuttingDown), so Reboot brings it back as one that ran.
	shutDown bool
	// mountinfo is what a kernel would list as its mounts, as they were when it
	// started (Engine.Mountinfo).
	mountinfo string
}

// reported are what a real engine says of a container nobody asked anything of, key
// by key, which a client has no field for or leaves empty. A reader of what a
// container runs with has to pass them all, as it does on a server.
var reported = map[string]map[string]any{
	"HostConfig": {
		"ContainerIDFile": "", "LogConfig": map[string]any{"Type": "json-file", "Config": map[string]any{}}, "AutoRemove": false,
		"VolumeDriver": "", "ConsoleSize": []int{0, 0}, "CgroupnsMode": "private", "Dns": []string{}, "DnsOptions": []string{},
		"DnsSearch": []string{}, "IpcMode": "private", "Cgroup": "", "OomScoreAdj": 0, "PidMode": "", "PublishAllPorts": false,
		"UTSMode": "", "ShmSize": 64 << 20, "Runtime": "runc", "Isolation": "", "CpuShares": 0, "CgroupParent": "", "BlkioWeight": 0,
		"BlkioWeightDevice": []any{}, "BlkioDeviceReadBps": []any{}, "BlkioDeviceWriteBps": []any{}, "BlkioDeviceReadIOps": []any{},
		"BlkioDeviceWriteIOps": []any{}, "CpuPeriod": 0, "CpuQuota": 0, "CpuRealtimePeriod": 0, "CpuRealtimeRuntime": 0, "CpusetCpus": "",
		"CpusetMems": "", "Devices": []any{}, "OomKillDisable": false, "Ulimits": []any{}, "CpuCount": 0, "CpuPercent": 0,
		"IOMaximumIOps": 0, "IOMaximumBandwidth": 0, "MaskedPaths": []string{"/proc/kcore", "/sys/firmware"}, "ReadonlyPaths": []string{"/proc/bus"},
	},
	"Config": {"Domainname": "", "AttachStdin": false, "AttachStdout": true, "AttachStderr": true, "OpenStdin": false, "StdinOnce": false, "Tty": false},
}

// inspected is what an inspect of c answers: what the client asked for, as it asked,
// and what a real engine fills in around that.
func (c *container) inspected() any {
	b, err := json.Marshal(c.Container)
	if err != nil {
		return c.Container
	}
	var out map[string]any
	if err := json.Unmarshal(b, &out); err != nil {
		return c.Container
	}
	section := func(name string) map[string]any {
		into, _ := out[name].(map[string]any)
		if into == nil {
			into = map[string]any{}
			out[name] = into
		}
		return into
	}
	for name, keys := range reported {
		into := section(name)
		for k, v := range keys {
			if _, set := into[k]; !set {
				into[k] = v
			}
		}
	}
	host, config := section("HostConfig"), section("Config")
	if _, set := host["NetworkMode"]; !set {
		host["NetworkMode"] = "bridge"
	}
	if c.memorySwap != 0 {
		host["MemorySwap"] = c.memorySwap
	}
	if _, set := config["Hostname"]; !set && len(c.ID) >= 12 {
		config["Hostname"] = c.ID[:12]
	}
	for name, keys := range c.reports {
		maps.Copy(section(name), keys)
	}
	return out
}

// New starts a fake engine on a socket in a test's temporary directory,
// with Docker's default bridge network, and stops it when the test ends.
func New(t testing.TB) *Engine {
	t.Helper()
	// A socket's path must stay short, under 108 bytes on Linux.
	dir, err := filepath.Abs(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	if len(dir) > 80 {
		dir = shortDir(t)
	}
	e := &Engine{
		Socket:     filepath.Join(dir, "docker.sock"),
		APIVersion: "1.47", MinAPIVersion: "1.24",
		Registry:   map[string]Image{},
		published:  map[string]Image{},
		images:     map[string]*image{},
		containers: map[string]*container{},
		volumes:    map[string]*docker.Volume{},
		execs:      map[string]*execRun{},
		buildCache: map[string]bool{},
		starts:     map[string]int{},
		networks: map[string]*docker.Network{
			"bridge": {ID: newID(), Name: "bridge", Driver: "bridge", Options: map[string]string{}, Labels: map[string]string{}},
			"host":   {ID: newID(), Name: "host", Driver: "host", Options: map[string]string{}, Labels: map[string]string{}},
		},
	}
	l, err := (&net.ListenConfig{}).Listen(context.Background(), "unix", e.Socket)
	if err != nil {
		t.Fatal(err)
	}
	e.srv = httptest.NewUnstartedServer(http.HandlerFunc(e.serve))
	_ = e.srv.Listener.Close()
	e.srv.Listener = l
	e.srv.Start()
	t.Cleanup(e.srv.Close)
	return e
}

func shortDir(t testing.TB) string {
	t.Helper()
	dir, err := os.MkdirTemp("", "dw")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.RemoveAll(dir) })
	return dir
}

// Client returns a client for the engine.
func (e *Engine) Client() *docker.Client { return docker.New(e.Socket) }

func newID() string {
	b := make([]byte, 32)
	_, _ = rand.Read(b)
	return hex.EncodeToString(b)
}

// eventTime reads a time as Docker's events take one: seconds, and nanoseconds if
// it has them.
func eventTime(s string) time.Time {
	seconds, nanos, _ := strings.Cut(s, ".")
	sec, _ := strconv.ParseInt(seconds, 10, 64)
	nsec, _ := strconv.ParseInt((nanos + "000000000")[:9], 10, 64)
	return time.Unix(sec, nsec)
}

// memoryKill is the kernel killing something in a container for its memory limit.
type memoryKill struct {
	id string
	at time.Time
}

// SetClockAhead has the engine's clock run ahead of this machine's by d from now
// on, as a server whose clock was set wrong at boot runs Docker; 0 puts it back,
// as the clock stepping back does. What it stamped before keeps its time.
func (e *Engine) SetClockAhead(d time.Duration) {
	e.mu.Lock()
	defer e.mu.Unlock()
	e.ahead = d
}

// wall is the engine's clock now. The caller holds the lock.
func (e *Engine) wall() time.Time { return time.Now().Add(e.ahead) }

// event is one thing the engine says happened to a container.
type event struct {
	action, id string
	at         time.Time
}

// emit records what happened to c, in order, as Docker's events do. The caller
// holds the lock.
func (e *Engine) emit(c *container, actions ...string) {
	for _, a := range actions {
		e.clock++
		e.events = append(e.events, event{action: a, id: c.ID, at: e.wall().Add(time.Duration(e.clock) * time.Nanosecond)})
	}
	if over := len(e.events) - docker.EventsHeld; over > 0 {
		e.events = append([]event(nil), e.events[over:]...)
	}
}

func writeJSON(w http.ResponseWriter, status int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(v)
}

func fail(w http.ResponseWriter, status int, format string, args ...any) {
	writeJSON(w, status, map[string]string{"message": fmt.Sprintf(format, args...)})
}

var versioned = regexp.MustCompile(`^/v\d+\.\d+(/.*)$`)

// Intercept calls fn before the engine answers each request, outside its
// lock, so fn can hold a request up, as an engine that hangs, or make
// changes in the middle of one. An error fn returns answers the request
// instead, as the engine failing it, with status 500.
func (e *Engine) Intercept(fn func(r *http.Request) error) {
	e.mu.Lock()
	defer e.mu.Unlock()
	e.intercept = fn
}

func (e *Engine) serve(w http.ResponseWriter, r *http.Request) {
	e.mu.Lock()
	intercept := e.intercept
	e.mu.Unlock()
	if intercept != nil {
		if err := intercept(r); err != nil {
			fail(w, http.StatusInternalServerError, "%v", err)
			return
		}
	}
	if e.serveLogs(w, r) {
		return // it may follow for as long as its client reads, so it holds no lock
	}
	if e.serveStream(w, r) {
		return
	}
	if r.Method == http.MethodPost && strings.HasSuffix(r.URL.Path, "/images/load") {
		// It reads as long as what writes the archive takes, as a build does, so it
		// holds no lock until it has it all.
		e.loadImage(w, r)
		return
	}
	e.mu.Lock()
	defer e.mu.Unlock()
	path := r.URL.Path
	e.Requests = append(e.Requests, r.Method+" "+path)
	if m := versioned.FindStringSubmatch(path); m != nil {
		path = m[1]
	}
	parts := strings.Split(strings.Trim(path, "/"), "/")
	switch {
	case path == "/_ping":
		_, _ = w.Write([]byte("OK"))
	case path == "/version":
		writeJSON(w, http.StatusOK, map[string]string{"Version": "27.3.1", "ApiVersion": e.APIVersion, "MinAPIVersion": e.MinAPIVersion, "Arch": "amd64"})
	case path == "/events":
		e.serveEvents(w, r)
	case path == "/info":
		root := e.DockerRoot
		if root == "" {
			root = "/var/lib/docker"
		}
		var options []string
		if e.Rootless {
			options = append(options, "name=rootless")
		}
		if e.NoNewPrivileges {
			options = append(options, "name=no-new-privileges")
		}
		if e.UsernsRemap {
			options = append(options, "name=userns")
		}
		driver := e.LoggingDriver
		if driver == "" {
			driver = docker.LogDriverJSONFile
		}
		index := map[string]any{"docker.io": map[string]any{"Secure": true}}
		for _, r := range e.Registries {
			index[r] = map[string]any{"Secure": false}
		}
		writeJSON(w, http.StatusOK, map[string]any{"DockerRootDir": root, "SecurityOptions": options, "NCPU": e.ncpu(), "LoggingDriver": driver, "LiveRestoreEnabled": e.LiveRestore, "RegistryConfig": map[string]any{"IndexConfigs": index, "Mirrors": e.Mirrors}})
	case parts[0] == "containers":
		e.serveContainers(w, r, parts[1:])
	case parts[0] == "exec" && len(parts) == 3 && parts[2] == "json":
		e.inspectExec(w, parts[1])
	case parts[0] == "exec" && len(parts) == 3 && parts[2] == "resize" && r.Method == http.MethodPost:
		e.resizeExec(w, r, parts[1])
	case r.Method == http.MethodGet && path == "/images/search":
		e.serveSearch(w, r)
	case parts[0] == "images":
		e.serveImages(w, r, path)
	case parts[0] == "distribution" && len(parts) > 2 && parts[len(parts)-1] == "json":
		e.serveDistribution(w, r, strings.Join(parts[1:len(parts)-1], "/"))
	case parts[0] == "volumes":
		e.serveVolumes(w, r, parts[1:])
	case r.Method == http.MethodGet && path == "/system/df":
		writeJSON(w, http.StatusOK, e.diskUsage())
	case parts[0] == "networks":
		e.serveNetworks(w, r, parts[1:])
	default:
		fail(w, http.StatusNotFound, "page not found")
	}
}

// serveSearch answers a search of Docker Hub from Hub, with the images whose
// names hold the term, limit at most, as the engine does.
func (e *Engine) serveSearch(w http.ResponseWriter, r *http.Request) {
	if e.RegistryDown {
		fail(w, http.StatusInternalServerError, "Get \"https://index.docker.io/v1/search\": net/http: request canceled while waiting for connection")
		return
	}
	term := strings.ToLower(r.URL.Query().Get("term"))
	limit, err := strconv.Atoi(r.URL.Query().Get("limit"))
	if err != nil || limit <= 0 {
		limit = 25
	}
	out := []docker.Found{}
	for _, f := range e.Hub {
		if strings.Contains(f.Name, term) && len(out) < limit {
			out = append(out, f)
		}
	}
	writeJSON(w, http.StatusOK, out)
}

// diskUsage is what /system/df says: each pulled image at its size, each layer
// counted once, so an image under two tags is one; each container's own layer as
// ContainerSizes has it; and each volume as VolumeSizes has it, with how many
// containers use it.
func (e *Engine) diskUsage() docker.DiskUsage {
	var out docker.DiskUsage
	seen := map[*image]bool{}
	for _, img := range e.images {
		if seen[img] {
			continue
		}
		seen[img] = true
		used := int64(0)
		for _, c := range e.containers {
			if c.img == img {
				used++
			}
		}
		out.LayersSize += img.Size
		out.Images = append(out.Images, docker.DiskUsageImage{ID: img.id, Size: img.Size, Containers: used})
	}
	for _, c := range e.containers {
		out.Containers = append(out.Containers, docker.DiskUsageContainer{ID: c.ID, SizeRw: e.ContainerSizes[strings.TrimPrefix(c.Name, "/")]})
	}
	for name := range e.volumes {
		v := docker.DiskUsageVolume{Name: name, Mountpoint: "/var/lib/docker/volumes/" + name + "/_data", UsageData: &docker.VolumeUsage{Size: e.VolumeSizes[name]}}
		for _, c := range e.containers {
			if slices.ContainsFunc(c.Mounts, func(m docker.MountPoint) bool { return m.Name == name }) {
				v.UsageData.RefCount++
			}
		}
		out.Volumes = append(out.Volumes, v)
	}
	slices.SortFunc(out.Images, func(a, b docker.DiskUsageImage) int { return strings.Compare(a.ID, b.ID) })
	slices.SortFunc(out.Containers, func(a, b docker.DiskUsageContainer) int { return strings.Compare(a.ID, b.ID) })
	slices.SortFunc(out.Volumes, func(a, b docker.DiskUsageVolume) int { return strings.Compare(a.Name, b.Name) })
	return out
}

// ncpu is how many CPUs the fake server has.
func (e *Engine) ncpu() int {
	if e.NCPU > 0 {
		return e.NCPU
	}
	return 8
}

// failStart fails a start as the engine does, which leaves why in the container's
// state, until it next starts.
func failStart(w http.ResponseWriter, c *container, code int, format string, args ...any) {
	c.State.Error = fmt.Sprintf(format, args...)
	fail(w, code, "%s", c.State.Error)
}

// FailedToStart leaves the container named name as the engine leaves one it tried
// to start, as after a reboot, and couldn't: stopped, with why in its state.
func (e *Engine) FailedToStart(name, why string) {
	e.mu.Lock()
	defer e.mu.Unlock()
	for _, c := range e.containers {
		if strings.TrimPrefix(c.Name, "/") == name {
			if c.State.Running {
				c.log.end()
				e.detach(c)
			}
			c.State = docker.State{Status: "exited", ExitCode: 128, Error: why, StartedAt: c.State.StartedAt, FinishedAt: e.wall().UTC()}
		}
	}
}

// AddLocalImage puts an image on the fake server that no registry has, as one
// built there is.
func (e *Engine) AddLocalImage(ref string, img Image) {
	e.mu.Lock()
	defer e.mu.Unlock()
	i := &image{id: "sha256:" + newID(), Image: img}
	e.images[docker.WithTag(ref)], e.images[i.id] = i, i
}

// image finds a pulled image as the engine takes a reference: by its name
// and tag, then its ID, then, for a reference without a tag, a prefix of
// one ID alone. So an image named by hex digits alone, such as cafe, can
// find an image whose ID starts with them when cafe:latest isn't pulled.
func (e *Engine) image(ref string) *image {
	// A digest names what it names, whatever tag is written with it.
	if name, digest, ok := strings.Cut(ref, "@"); ok {
		if img, known := e.images[repoOf(name)+"@"+digest]; known {
			return img
		}
		return nil
	}
	if img, ok := e.images[docker.WithTag(ref)]; ok {
		return img
	}
	if img, ok := e.images[ref]; ok {
		return img
	}
	if img, ok := e.images["sha256:"+ref]; ok {
		return img
	}
	if strings.ContainsAny(ref, ":/@") {
		return nil
	}
	var found *image
	for key, img := range e.images {
		if strings.HasPrefix(key, "sha256:"+ref) {
			if found != nil && found != img {
				return nil
			}
			found = img
		}
	}
	return found
}

func (e *Engine) serveImages(w http.ResponseWriter, r *http.Request, path string) {
	switch {
	case r.Method == http.MethodPost && path == "/images/create" && r.URL.Query().Get("fromSrc") == "-":
		e.importImage(w, r)
	case r.Method == http.MethodGet && path == "/images/json":
		e.listImages(w, r)
	case r.Method == http.MethodDelete:
		e.removeImage(w, strings.TrimPrefix(path, "/images/"), truthy(r.URL.Query().Get("force")))
	case r.Method == http.MethodPost && path == "/images/create":
		ref := r.URL.Query().Get("fromImage")
		if tag := r.URL.Query().Get("tag"); tag != "" {
			ref += ":" + tag
		}
		e.Pulls++
		w.Header().Set("Content-Type", "application/json")
		enc := json.NewEncoder(w)
		if !e.signedIn(r, ref) {
			_ = enc.Encode(map[string]any{
				"error":       "pull access denied for " + ref + ", repository does not exist or may require 'docker login': denied: requested access to the resource is denied",
				"errorDetail": map[string]string{"message": "pull access denied for " + ref + ", repository does not exist or may require 'docker login': denied: requested access to the resource is denied"},
			})
			return
		}
		src, ok := e.Registry[ref]
		if _, digest, pinned := strings.Cut(ref, "@"); pinned && !ok {
			src, ok = e.published[digest]
		}
		if !ok && e.Find != nil {
			src, ok = e.Find(ref)
		}
		if !ok {
			_ = enc.Encode(map[string]any{
				"error":       "pull access denied for " + ref + ", repository does not exist or may require 'docker login'",
				"errorDetail": map[string]string{"message": "pull access denied for " + ref + ", repository does not exist or may require 'docker login'"},
			})
			return
		}
		if src.PullError != "" {
			_ = enc.Encode(map[string]any{"error": src.PullError, "errorDetail": map[string]string{"message": src.PullError}})
			return
		}
		_ = enc.Encode(map[string]string{"status": "Pulling from " + ref})
		if src.Size > 0 {
			for _, part := range []int64{src.Size / 4, src.Size / 2, src.Size} {
				_ = enc.Encode(map[string]any{"status": "Downloading", "id": "layer1", "progressDetail": map[string]int64{"current": part, "total": src.Size}})
				if src.PullTime > 0 {
					// The lock stays free meanwhile, as a slow pull holds up nothing else.
					if f, ok := w.(http.Flusher); ok {
						f.Flush()
					}
					e.mu.Unlock()
					time.Sleep(src.PullTime / 3)
					e.mu.Lock()
				}
			}
			_ = enc.Encode(map[string]any{"status": "Pull complete", "id": "layer1"})
		}
		digest := digestOf(ref, src)
		if have := e.images[ref]; have != nil && have.digest == digest {
			have.Image = src
			_ = enc.Encode(map[string]string{"status": "Status: Image is up to date for " + ref})
			return
		}
		repo, _, _ := strings.Cut(ref, "@")
		img := &image{id: "sha256:" + newID(), digest: digest, repo: repoOf(repo), Image: src}
		e.images[ref], e.images[img.id], e.images[img.repo+"@"+digest] = img, img, img
		e.published[digest] = src
		_ = enc.Encode(map[string]string{"status": "Status: Downloaded newer image for " + ref})
	case r.Method == http.MethodPost && strings.HasSuffix(path, "/tag"):
		source := strings.TrimSuffix(strings.TrimPrefix(path, "/images/"), "/tag")
		img := e.image(source)
		if img == nil {
			fail(w, http.StatusNotFound, "No such image: %s", source)
			return
		}
		e.images[r.URL.Query().Get("repo")+":"+r.URL.Query().Get("tag")] = img
		w.WriteHeader(http.StatusCreated)
	case r.Method == http.MethodGet && strings.HasSuffix(path, "/json"):
		ref := strings.TrimSuffix(strings.TrimPrefix(path, "/images/"), "/json")
		img := e.image(ref)
		if img == nil {
			fail(w, http.StatusNotFound, "No such image: %s", ref)
			return
		}
		if img.Architecture != "" {
			writeJSON(w, http.StatusOK, docker.Image{ID: img.id, Config: &img.Config, Architecture: img.Architecture})
			return
		}
		cfg := img.Config
		if len(img.Volumes) > 0 {
			cfg.Volumes = map[string]struct{}{}
			for _, v := range img.Volumes {
				cfg.Volumes[v] = struct{}{}
			}
		}
		out := docker.Image{ID: img.id, Config: &cfg, Created: img.created, Size: img.Size}
		// An image built here has none.
		if img.digest != "" {
			out.RepoDigests = []string{img.repo + "@" + img.digest}
		}
		writeJSON(w, http.StatusOK, out)
	default:
		fail(w, http.StatusNotFound, "page not found")
	}
}

// runsAttached reports an image whose containers run as a helper does: in the
// engine, for as long as something attaches.
func (i *image) runsAttached() bool {
	return i.imported || i.repo == BuildKitRepository || i.repo == RailpackRepository
}

// Builders are what was asked of each container of BuildKit's image.
func (e *Engine) Builders() []docker.CreateRequest {
	e.mu.Lock()
	defer e.mu.Unlock()
	return append([]docker.CreateRequest(nil), e.builders...)
}

// digestOf is what a registry calls an image.
func digestOf(ref string, src Image) string {
	if _, digest, pinned := strings.Cut(ref, "@"); pinned {
		return digest
	}
	if src.Digest != "" {
		return src.Digest
	}
	sum := sha256.Sum256([]byte(ref))
	return "sha256:" + hex.EncodeToString(sum[:])
}

// repoOf is a reference's name, without its tag.
func repoOf(ref string) string {
	if i := strings.LastIndex(ref, ":"); i > strings.LastIndex(ref, "/") {
		return ref[:i]
	}
	return ref
}

// Forget takes an image out of the registry and off the server, as a registry's
// retention and an image prune do.
func (e *Engine) Forget(digest string) {
	e.mu.Lock()
	defer e.mu.Unlock()
	delete(e.published, digest)
	for ref, img := range e.images {
		if img.digest == digest {
			delete(e.images, ref)
		}
	}
	for ref := range e.Registry {
		if digestOf(ref, e.Registry[ref]) == digest {
			delete(e.Registry, ref)
		}
	}
}

// serveDistribution says what a registry has of a reference, with nothing pulled.
func (e *Engine) serveDistribution(w http.ResponseWriter, r *http.Request, ref string) {
	if e.RegistryDown {
		fail(w, http.StatusInternalServerError, "Get \"https://registry-1.docker.io/v2/\": net/http: request canceled while waiting for connection")
		return
	}
	if !e.signedIn(r, ref) {
		fail(w, http.StatusUnauthorized, "Head \"https://example.invalid/v2/%s/manifests/latest\": unauthorized: authentication required", ref)
		return
	}
	digest := ""
	if _, pinned, ok := strings.Cut(ref, "@"); ok {
		if _, known := e.published[pinned]; known {
			digest = pinned
		}
	}
	src, ok := e.Registry[ref]
	if !ok && e.Find != nil {
		src, ok = e.Find(ref)
	}
	if ok {
		digest = digestOf(ref, src)
	}
	if digest == "" {
		fail(w, http.StatusNotFound, "manifest unknown: %s", ref)
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"Descriptor": map[string]any{"digest": digest}})
}

// named finds a container by its exact name, as the engine checks a name
// is free.
func (e *Engine) named(name string) *container {
	for _, c := range e.containers {
		if c.Name == "/"+name {
			return c
		}
	}
	return nil
}

// errAmbiguous is a reference that more than one ID starts with, which
// the engine refuses as an invalid parameter since Docker 23.
type errAmbiguous struct{ ref string }

func (e errAmbiguous) Error() string { return "multiple IDs found with provided prefix: " + e.ref }

// findContainer finds a container as the engine takes a reference: by its
// full ID, then its exact name, then a prefix of one ID alone. So a name
// made of hex digits, such as db, can find a container whose ID starts
// with it when none is named db.
func (e *Engine) findContainer(ref string) (*container, error) {
	if c, ok := e.containers[ref]; ok {
		return c, nil
	}
	if c := e.named(ref); c != nil {
		return c, nil
	}
	var found *container
	for id, c := range e.containers {
		if ref != "" && strings.HasPrefix(id, ref) {
			if found != nil {
				return nil, errAmbiguous{ref}
			}
			found = c
		}
	}
	return found, nil
}

func (e *Engine) serveContainers(w http.ResponseWriter, r *http.Request, parts []string) {
	if len(parts) == 1 && parts[0] == "create" && r.Method == http.MethodPost {
		e.create(w, r)
		return
	}
	if len(parts) == 0 {
		fail(w, http.StatusNotFound, "page not found")
		return
	}
	if parts[0] == "json" && r.Method == http.MethodGet {
		e.list(w, r)
		return
	}
	c, err := e.findContainer(parts[0])
	switch {
	case err != nil:
		fail(w, http.StatusBadRequest, "%v", err)
		return
	case c == nil:
		fail(w, http.StatusNotFound, "No such container: %s", parts[0])
		return
	}
	action := ""
	if len(parts) > 1 {
		action = parts[1]
	}
	switch {
	case r.Method == http.MethodGet && action == "json":
		e.process(c)
		writeJSON(w, http.StatusOK, c.inspected())
	case r.Method == http.MethodPost && action == "exec":
		e.createExec(w, r, c)
	case r.Method == http.MethodHead && action == "archive":
		e.statPath(w, r, c)
	case r.Method == http.MethodPost && action == "start":
		if c.State.Running {
			w.WriteHeader(http.StatusNotModified)
			return
		}
		e.starts[strings.TrimPrefix(c.Name, "/")]++
		if c.img.runsAttached() {
			c.State = docker.State{Status: "running", Running: true, StartedAt: e.wall().UTC()}
			e.emit(c, "start")
			close(c.started)
			w.WriteHeader(http.StatusNoContent)
			return
		}
		for name, ep := range c.NetworkSettings.Networks {
			if e.networks[name] == nil && name != "host" {
				failStart(w, c, http.StatusNotFound, "network %s not found", name)
				return
			}
			// A stopped container keeps the ID of its network, by which the
			// engine looks it up, and that is gone if the network was made again.
			if ep != nil && ep.NetworkID != "" && ep.NetworkID != e.networks[name].ID {
				failStart(w, c, http.StatusNotFound, "network %s not found", ep.NetworkID)
				return
			}
		}
		if err := e.portsFree(c); err != nil {
			failStart(w, c, http.StatusInternalServerError, "%v", err)
			return
		}
		if err := e.addressesFree(c); err != nil {
			failStart(w, c, http.StatusInternalServerError, "%v", err)
			return
		}
		if e.BindError != nil {
			for _, m := range c.Mounts {
				if said := e.BindError(m.Source); m.Type == "bind" && said != "" {
					failStart(w, c, http.StatusBadRequest, "error while creating mount source path '%s': %s", m.Source, said)
					return
				}
			}
		}
		// The engine fills in a container's ID for a network as it first joins it.
		for name, ep := range c.NetworkSettings.Networks {
			if ep != nil && ep.NetworkID == "" && e.networks[name] != nil {
				ep.NetworkID = e.networks[name].ID
			}
		}
		e.clock++
		now := e.wall().Add(time.Duration(e.clock) * time.Nanosecond).UTC()
		c.State = docker.State{Status: "running", Running: true, StartedAt: now}
		c.log.start()
		c.log.write(docker.Stdout, c.img.Prints...)
		e.emit(c, "start")
		if c.img.Exits {
			c.State = docker.State{Status: "exited", ExitCode: 1, StartedAt: now, FinishedAt: now}
			c.log.end()
			e.emit(c, "die")
		}
		if hc := c.Config.Healthcheck; hc != nil && len(hc.Test) > 0 && hc.Test[0] != "NONE" && c.State.Running {
			c.State.Health = &docker.Health{Status: "healthy", Log: []docker.HealthRun{{End: c.State.StartedAt, Output: "ok\n"}}}
			switch {
			case c.img.Unhealthy:
				c.State.Health = &docker.Health{Status: "unhealthy", FailingStreak: 3, Log: []docker.HealthRun{{End: c.State.StartedAt, ExitCode: 1, Output: c.img.CheckSays + "\n"}}}
			case c.img.Starting:
				c.State.Health = &docker.Health{Status: "starting"}
			}
		}
		e.attach(c)
		if c.started != nil {
			close(c.started)
		}
		w.WriteHeader(http.StatusNoContent)
	case r.Method == http.MethodPost && action == "stop":
		if !c.State.Running {
			w.WriteHeader(http.StatusNotModified)
			return
		}
		c.State = docker.State{Status: "exited", StartedAt: c.State.StartedAt, FinishedAt: e.wall().UTC()}
		c.log.end()
		e.detach(c)
		e.emit(c, "kill", "stop", "die")
		if c.exited != nil && !c.img.imported {
			close(c.exited)
		}
		w.WriteHeader(http.StatusNoContent)
	case r.Method == http.MethodPost && action == "pause":
		if !c.State.Running || c.State.Paused {
			fail(w, http.StatusConflict, "Container %s is not running", c.ID)
			return
		}
		c.State.Paused, c.State.Status = true, "paused"
		w.WriteHeader(http.StatusNoContent)
	case r.Method == http.MethodPost && action == "unpause":
		if !c.State.Paused {
			fail(w, http.StatusConflict, "Container %s is not paused", c.ID)
			return
		}
		c.State.Paused, c.State.Status = false, "running"
		w.WriteHeader(http.StatusNoContent)
	case r.Method == http.MethodPost && action == "kill":
		if !c.State.Running {
			fail(w, http.StatusConflict, "Container %s is not running", c.ID)
			return
		}
		c.State = docker.State{Status: "exited", ExitCode: 137, StartedAt: c.State.StartedAt, FinishedAt: e.wall().UTC()}
		c.log.end()
		e.emit(c, "kill", "die")
		e.detach(c)
		w.WriteHeader(http.StatusNoContent)
	case r.Method == http.MethodPost && action == "x-dockwell-health":
		// Only here, for a test that drives this engine from outside its process: it
		// has no checks to run, so it's told how one went.
		status, output := r.URL.Query().Get("status"), r.URL.Query().Get("output")
		h := &docker.Health{Status: status, Log: []docker.HealthRun{{End: time.Now(), Output: output}}}
		if status == "unhealthy" {
			h.FailingStreak, h.Log[0].ExitCode = 3, 1
		}
		c.State.Health = h
		w.WriteHeader(http.StatusNoContent)
	case r.Method == http.MethodPost && action == "rename":
		name := r.URL.Query().Get("name")
		if e.named(name) != nil {
			fail(w, http.StatusConflict, "Conflict. The container name \"/%s\" is already in use", name)
			return
		}
		c.Name = "/" + name
		w.WriteHeader(http.StatusNoContent)
	case r.Method == http.MethodPost && action == "update":
		var body struct {
			docker.ContainerUpdate
			MemorySwap int64 `json:"MemorySwap"`
		}
		if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
			fail(w, http.StatusBadRequest, "%v", err)
			return
		}
		// As the engine has it: a memory limit past the swap it holds needs a new
		// swap, and a limit of zero leaves it as it is.
		h := &c.HostConfig
		if body.Memory != 0 && body.Memory > c.memorySwap && body.MemorySwap == 0 {
			fail(w, http.StatusConflict, "Conflicting options: Memory limit should be smaller than already set memoryswap limit, update the memoryswap at the same time")
			return
		}
		if body.NanoCPUs != 0 {
			h.NanoCPUs = body.NanoCPUs
		}
		if body.Memory != 0 {
			h.Memory = body.Memory
		}
		if body.MemorySwap != 0 {
			c.memorySwap = body.MemorySwap
		}
		if body.MemoryReservation != 0 {
			h.MemoryReservation = body.MemoryReservation
		}
		h.RestartPolicy = body.RestartPolicy
		writeJSON(w, http.StatusOK, map[string]any{"Warnings": []string{}})
	case r.Method == http.MethodDelete && action == "":
		if c.State.Running && !truthy(r.URL.Query().Get("force")) {
			fail(w, http.StatusConflict, "cannot remove container %s: container is running: stop the container before removing or force remove", c.Name)
			return
		}
		e.detach(c)
		c.log.end()
		delete(e.containers, c.ID)
		if truthy(r.URL.Query().Get("v")) {
			// As the engine does: the anonymous volumes it made for this container go
			// with it, unless something else mounts one now.
			for _, m := range c.Mounts {
				v := e.volumes[m.Name]
				if m.Type != "volume" || v == nil {
					continue
				}
				if _, anonymous := v.Labels["com.docker.volume.anonymous"]; anonymous && e.volumeInUse(m.Name) == "" {
					delete(e.volumes, m.Name)
				}
			}
		}
		w.WriteHeader(http.StatusNoContent)
	default:
		fail(w, http.StatusNotFound, "page not found")
	}
}

// filters reads a list's filters as the engine takes them: the current
// form, {"name": {"^/db$": true}}, or the older {"name": ["^/db$"]}.
func filters(r *http.Request) (map[string][]string, error) {
	raw := r.URL.Query().Get("filters")
	if raw == "" {
		return nil, nil
	}
	var current map[string]map[string]bool
	if err := json.Unmarshal([]byte(raw), &current); err == nil {
		out := map[string][]string{}
		for k, vs := range current {
			for v := range vs {
				out[k] = append(out[k], v)
			}
		}
		return out, nil
	}
	var legacy map[string][]string
	if err := json.Unmarshal([]byte(raw), &legacy); err != nil {
		return nil, fmt.Errorf("invalid filter: %w", err)
	}
	return legacy, nil
}

// matches reports whether a container passes a list's filters, as the
// engine has them: name is a regular expression on its name, with or
// without the leading slash; label is a key, or key=value, and every label
// must match; volume is a volume's name, or where something is mounted;
// network is a network's name, or a prefix of the ID a container has for it, which
// it has only once it first starts, as Moby v28.5.2's daemon/list.go reads it.
func (c *container) matches(f map[string][]string) bool {
	passes := func(key string, ok func(string) bool) bool {
		vs, set := f[key]
		return !set || slices.ContainsFunc(vs, ok)
	}
	for _, v := range f["label"] {
		k, val, hasValue := strings.Cut(v, "=")
		if got, ok := c.Config.Labels[k]; !ok || hasValue && got != val {
			return false
		}
	}
	return passes("name", func(v string) bool {
		re, err := regexp.Compile(v)
		return err == nil && (re.MatchString(c.Name) || re.MatchString(strings.TrimPrefix(c.Name, "/")))
	}) && passes("id", func(v string) bool {
		re, err := regexp.Compile(v)
		return err == nil && re.MatchString(c.ID)
	}) && passes("volume", func(v string) bool {
		return slices.ContainsFunc(c.Mounts, func(m docker.MountPoint) bool { return m.Name == v || m.Source == v || m.Destination == v })
	}) && passes("network", func(v string) bool {
		if _, on := c.NetworkSettings.Networks[v]; on {
			return true
		}
		for _, ep := range c.NetworkSettings.Networks {
			if ep != nil && strings.HasPrefix(ep.NetworkID, v) {
				return true
			}
		}
		return false
	})
}

// list lists containers, as GET /containers/json does. Without all, only
// running ones.
func (e *Engine) list(w http.ResponseWriter, r *http.Request) {
	f, err := filters(r)
	if err != nil {
		fail(w, http.StatusBadRequest, "%v", err)
		return
	}
	all := r.URL.Query().Get("all")
	out := []docker.ContainerSummary{}
	for _, c := range e.containers {
		if (all == "" || all == "0" || all == "false") && !c.State.Running {
			continue
		}
		if c.matches(f) {
			out = append(out, docker.ContainerSummary{
				ID: c.ID, Names: []string{c.Name}, Labels: c.Config.Labels, Created: c.created, Image: e.listedImage(c), Ports: summaryPorts(c), Mounts: c.Mounts,
				State: c.State.Status, Status: statusLine(c), NetworkSettings: docker.SummaryNetworks{Networks: c.NetworkSettings.Networks},
			})
		}
	}
	slices.SortFunc(out, func(a, b docker.ContainerSummary) int { return int(b.Created - a.Created) })
	writeJSON(w, http.StatusOK, out)
}

// listedImage is what a list says a container runs: the name it was made from
// while that still means the image it runs, or else the image's ID, as Moby v28.5.2's
// daemon/list.go has it in refreshImage.
func (e *Engine) listedImage(c *container) string {
	if img := e.image(c.Config.Image); c.Image == "" || img != nil && img.id == c.Image {
		return c.Config.Image // AddContainer can make one of no image in particular
	}
	return c.Image
}

// statusLine says how a container is, as a list has it for people, with its
// health check's state as the engine words it.
func statusLine(c *container) string {
	switch {
	case c.State.Restarting:
		return "Restarting (1) 2 seconds ago"
	case !c.State.Running:
		return fmt.Sprintf("Exited (%d) 2 seconds ago", c.State.ExitCode)
	case c.State.Health == nil:
		return "Up 3 minutes"
	case c.State.Health.Status == "starting":
		return "Up 3 seconds (health: starting)"
	}
	return "Up 3 minutes (" + c.State.Health.Status + ")"
}

// summaryPorts are the ports a running container publishes, as a list has them:
// a stopped one has none, and no address means every one of each family.
func summaryPorts(c *container) []docker.SummaryPort {
	if !c.State.Running {
		return nil
	}
	var out []docker.SummaryPort
	for port, bs := range c.HostConfig.PortBindings {
		for _, b := range bs {
			n, err := strconv.Atoi(b.HostPort)
			if err == nil && n > 0 && n < 65536 {
				addresses := []string{b.HostIP}
				if b.HostIP == "" {
					addresses = []string{"0.0.0.0", "::"}
				}
				for _, ip := range addresses {
					out = append(out, docker.SummaryPort{IP: ip, PrivatePort: privatePort(port), PublicPort: uint16(n), Type: proto(port)})
				}
			}
		}
	}
	return out
}

// addressesFree fails a container whose static address a running container
// has on the network, as the engine fails starting it.
func (e *Engine) addressesFree(c *container) error {
	for name, ep := range c.NetworkSettings.Networks {
		if ep == nil || ep.IPAMConfig == nil || ep.IPAMConfig.IPv4Address == "" {
			continue
		}
		for _, o := range e.containers {
			if o == c || !o.State.Running {
				continue
			}
			if oe := o.NetworkSettings.Networks[name]; oe != nil && oe.IPAMConfig != nil && oe.IPAMConfig.IPv4Address == ep.IPAMConfig.IPv4Address {
				return fmt.Errorf("address already in use: %s on %s, by %s", ep.IPAMConfig.IPv4Address, name, o.Name)
			}
		}
	}
	return nil
}

// portsFree fails a container whose ports another running container, or
// another of its own, has bound, as the engine fails starting it.
func (e *Engine) portsFree(c *container) error {
	type binding struct{ port, proto, ip string }
	bindings := func(c *container) []binding {
		var out []binding
		for port, bs := range c.HostConfig.PortBindings {
			for _, b := range bs {
				if b.HostPort != "" {
					out = append(out, binding{b.HostPort, proto(port), b.HostIP})
				}
			}
		}
		return out
	}
	var bound []binding
	for _, o := range e.containers {
		if o != c && o.State.Running {
			bound = append(bound, bindings(o)...)
		}
	}
	for _, b := range bindings(c) {
		for _, o := range bound {
			if b.port == o.port && b.proto == o.proto && overlap(b.ip, o.ip) {
				return fmt.Errorf("driver failed programming external connectivity on endpoint %s: Bind for %s:%s failed: port is already allocated",
					strings.TrimPrefix(c.Name, "/"), cmp.Or(b.ip, "0.0.0.0"), b.port)
			}
		}
		bound = append(bound, b)
	}
	return nil
}

// overlap reports whether two addresses a port is published on can't both
// be bound: the same one, or every address and any other of its family.
// No address binds every one, IPv4 and IPv6.
func overlap(a, b string) bool {
	if a == "" || b == "" || a == b {
		return true
	}
	x, err1 := netip.ParseAddr(a)
	y, err2 := netip.ParseAddr(b)
	if err1 != nil || err2 != nil || x.Is4() != y.Is4() {
		return false
	}
	return x.IsUnspecified() || y.IsUnspecified()
}

func proto(port string) string {
	_, p, _ := strings.Cut(port, "/")
	return p
}

// truthy reads a query's flag as Moby v28.5.2's httputils.BoolValue does: anything
// but these is true, so force=1 forces as force=true does.
func truthy(v string) bool {
	switch strings.ToLower(strings.TrimSpace(v)) {
	case "", "0", "no", "false", "none":
		return false
	}
	return true
}

// privatePort is the container's side of a port such as "80/tcp".
func privatePort(port string) uint16 {
	number, _, _ := strings.Cut(port, "/")
	n, _ := strconv.ParseUint(number, 10, 16)
	return uint16(n)
}

// attach and detach keep each network's list of running containers.
func (e *Engine) attach(c *container) {
	for name, ep := range c.NetworkSettings.Networks {
		if ep != nil && c.State.Running {
			ep.IPAddress = cmp.Or(c.img.Address, e.addressOn(name, ep))
		}
		if n := e.networks[name]; n != nil {
			if n.Containers == nil {
				n.Containers = map[string]struct {
					Name string `json:"Name"`
				}{}
			}
			n.Containers[c.ID] = struct {
				Name string `json:"Name"`
			}{strings.TrimPrefix(c.Name, "/")}
		}
	}
}

func (e *Engine) detach(c *container) {
	for _, n := range e.networks {
		delete(n.Containers, c.ID)
	}
	for _, ep := range c.NetworkSettings.Networks {
		if ep != nil {
			ep.IPAddress = ""
		}
	}
}

// addressOn is the address a running container gets on a network: the one it
// asked for, or the next free one, as the engine gives out.
func (e *Engine) addressOn(name string, ep *docker.EndpointSettings) string {
	if a := ep.IPAMConfig; a != nil && a.IPv4Address != "" {
		return a.IPv4Address
	}
	if ep.IPAddress != "" {
		return ep.IPAddress
	}
	e.nextHost++
	subnet := "172.17.0.0/16"
	if n := e.networks[name]; n != nil && len(n.IPAM.Config) > 0 && n.IPAM.Config[0].Subnet != "" {
		subnet = n.IPAM.Config[0].Subnet
	}
	prefix, err := netip.ParsePrefix(subnet)
	if err != nil {
		return ""
	}
	a := prefix.Addr().Next()
	for range e.nextHost {
		a = a.Next()
	}
	return a.String()
}

func (e *Engine) create(w http.ResponseWriter, r *http.Request) {
	var req docker.CreateRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		fail(w, http.StatusBadRequest, "%v", err)
		return
	}
	name := r.URL.Query().Get("name")
	if e.named(name) != nil {
		fail(w, http.StatusConflict, "Conflict. The container name \"/%s\" is already in use by another container", name)
		return
	}
	img := e.image(req.Image)
	if img == nil {
		fail(w, http.StatusNotFound, "No such image: %s", req.Image)
		return
	}
	c := &container{img: img}
	if img.runsAttached() {
		c.started, c.exited = make(chan struct{}), make(chan struct{})
	}
	if img.imported {
		e.helpers = append(e.helpers, req)
	}
	if img.repo == BuildKitRepository {
		e.builders = append(e.builders, req)
	}
	if !img.imported && img.Exec != nil && req.Labels[docker.LabelOneoff] == "True" {
		// A one-off container, as docker compose run makes, runs its command
		// through Exec once something attaches, and ends with it.
		c.started, c.exited = make(chan struct{}), make(chan struct{})
	}
	c.ID = newID()
	c.Created = time.Now().UTC()
	c.log.ended = true // a follow ends at once until it starts
	c.Name = "/" + name
	c.Image = img.id
	c.Config = merge(req.Config, img.Config)
	c.HostConfig = req.HostConfig
	if c.HostConfig.ShmSize == 0 {
		c.HostConfig.ShmSize = 64 << 20 // Docker reports what it took
	}
	c.memorySwap = 2 * req.HostConfig.Memory
	c.State = docker.State{Status: "created"}
	if e.UsernsRemap && req.HostConfig.NetworkMode == "host" && req.HostConfig.UsernsMode != "host" {
		fail(w, http.StatusBadRequest, "cannot share the host's network namespace when user namespaces are enabled")
		return
	}
	mode := req.HostConfig.NetworkMode
	if mode == "" || mode == "default" {
		mode = "bridge"
		c.HostConfig.NetworkMode = "bridge"
	}
	if h := req.HostConfig; h.NanoCPUs > int64(e.ncpu())*1e9 {
		fail(w, http.StatusBadRequest, "Range of CPUs is from 0.01 to %d.00, as there are only %d CPUs available", e.ncpu(), e.ncpu())
		return
	}
	// The engine takes networks as references, and reports them by name. The
	// server's own network is one it has from the start.
	if mode == "host" {
		c.NetworkSettings.Networks = map[string]*docker.EndpointSettings{"host": {}}
		e.finishCreate(w, c, &req)
		return
	}
	if mode == "none" {
		e.finishCreate(w, c, &req)
		return
	}
	n := e.network(w, mode)
	if n == nil {
		return
	}
	c.NetworkSettings.Networks = map[string]*docker.EndpointSettings{n.Name: {}}
	for ref, s := range req.NetworkingConfig.EndpointsConfig {
		n := e.network(w, ref)
		if n == nil {
			return
		}
		if n.Name == "bridge" && s != nil && len(s.Aliases) > 0 {
			fail(w, http.StatusBadRequest, "network-scoped alias is supported only for containers in user defined networks")
			return
		}
		c.NetworkSettings.Networks[n.Name] = s
	}
	e.finishCreate(w, c, &req)
}

// finishCreate mounts what the new container mounts, and records it.
func (e *Engine) finishCreate(w http.ResponseWriter, c *container, req *docker.CreateRequest) {
	img := c.img
	covered := map[string]bool{}
	for _, m := range req.HostConfig.Mounts {
		covered[m.Target] = true
		mp := docker.MountPoint{Type: m.Type, Source: m.Source, Destination: m.Target, RW: !m.ReadOnly}
		if m.Type == "volume" {
			e.ensureVolume(m.Source, nil)
			mp.Name = m.Source
		}
		c.Mounts = append(c.Mounts, mp)
	}
	for _, b := range req.HostConfig.Binds {
		parts := strings.Split(b, ":")
		if len(parts) < 2 {
			fail(w, http.StatusBadRequest, "invalid volume specification: '%s'", b)
			return
		}
		covered[parts[1]] = true
		mp := docker.MountPoint{Type: "bind", Source: parts[0], Destination: parts[1], RW: true}
		if len(parts) > 2 {
			for _, mode := range strings.Split(parts[2], ",") {
				if !bindModes[mode] {
					fail(w, http.StatusBadRequest, "invalid mode: %s", mode)
					return
				}
				mp.RW = mp.RW && mode != "ro"
			}
		}
		if !strings.HasPrefix(parts[0], "/") {
			e.ensureVolume(parts[0], nil)
			mp.Type, mp.Name = "volume", parts[0]
		}
		c.Mounts = append(c.Mounts, mp)
	}
	for _, path := range img.Volumes {
		if !covered[path] {
			v := e.ensureVolume(newID(), map[string]string{"com.docker.volume.anonymous": ""})
			c.Mounts = append(c.Mounts, docker.MountPoint{Type: "volume", Name: v.Name, Destination: path, RW: true})
		}
	}
	e.clock++
	c.created = e.clock
	e.containers[c.ID] = c
	e.Creates++
	writeJSON(w, http.StatusCreated, map[string]any{"Id": c.ID, "Warnings": []string{}})
}

// bindModes are the options the engine takes in a bind on Linux.
var bindModes = map[string]bool{
	"rw": true, "ro": true, "z": true, "Z": true, "nocopy": true,
	"shared": true, "rshared": true, "slave": true, "rslave": true, "private": true, "rprivate": true,
	"consistent": true, "cached": true, "delegated": true,
}

// merge fills in what a container leaves out from its image, as the
// engine does.
func merge(c, img docker.Config) docker.Config {
	if c.User == "" {
		c.User = img.User
	}
	if c.WorkingDir == "" {
		c.WorkingDir = img.WorkingDir
	}
	if len(c.Entrypoint) == 0 {
		if len(c.Cmd) == 0 {
			c.Cmd = img.Cmd
		}
		if c.Entrypoint == nil {
			c.Entrypoint = img.Entrypoint
		}
	}
	have := map[string]bool{}
	for _, kv := range c.Env {
		k, _, _ := strings.Cut(kv, "=")
		have[k] = true
	}
	for _, kv := range img.Env {
		if k, _, _ := strings.Cut(kv, "="); !have[k] {
			c.Env = append(c.Env, kv)
		}
	}
	labels := maps.Clone(img.Labels)
	if labels == nil {
		labels = map[string]string{}
	}
	maps.Copy(labels, c.Labels)
	c.Labels = labels
	if ih := img.Healthcheck; ih != nil {
		if c.Healthcheck == nil {
			hc := *ih
			c.Healthcheck = &hc
		} else {
			hc := c.Healthcheck
			if len(hc.Test) == 0 {
				hc.Test = ih.Test
			}
			if hc.Interval == 0 {
				hc.Interval = ih.Interval
			}
			if hc.Timeout == 0 {
				hc.Timeout = ih.Timeout
			}
			if hc.StartPeriod == 0 {
				hc.StartPeriod = ih.StartPeriod
			}
			if hc.Retries == 0 {
				hc.Retries = ih.Retries
			}
		}
	}
	if c.ExposedPorts == nil {
		c.ExposedPorts = map[string]struct{}{}
	}
	maps.Copy(c.ExposedPorts, img.ExposedPorts)
	return c
}

func (e *Engine) ensureVolume(name string, labels map[string]string) *docker.Volume {
	if v, ok := e.volumes[name]; ok {
		return v
	}
	if labels == nil {
		labels = map[string]string{}
	}
	v := &docker.Volume{Name: name, Driver: "local", Options: map[string]string{}, Labels: labels, CreatedAt: "2026-11-04T10:00:00Z"}
	e.volumes[name] = v
	return v
}

// volumeInUse reports a container that mounts the volume.
func (e *Engine) volumeInUse(name string) string {
	for _, c := range e.containers {
		for _, m := range c.Mounts {
			if m.Name == name {
				return c.ID
			}
		}
	}
	return ""
}

func (e *Engine) serveVolumes(w http.ResponseWriter, r *http.Request, parts []string) {
	switch {
	case len(parts) == 0 && r.Method == http.MethodGet:
		list := make([]*docker.Volume, 0, len(e.volumes))
		for _, name := range slices.Sorted(maps.Keys(e.volumes)) {
			list = append(list, e.volumes[name])
		}
		writeJSON(w, http.StatusOK, map[string]any{"Volumes": list})
	case r.Method == http.MethodPost && len(parts) == 1 && parts[0] == "create":
		var req docker.VolumeCreate
		if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
			fail(w, http.StatusBadRequest, "%v", err)
			return
		}
		if v, ok := e.volumes[req.Name]; ok {
			driver := req.Driver
			if driver == "" {
				driver = "local"
			}
			if driver != v.Driver {
				fail(w, http.StatusConflict, "create %s: volume name must be unique", req.Name)
				return
			}
			writeJSON(w, http.StatusCreated, v)
			return
		}
		v := e.ensureVolume(req.Name, maps.Clone(req.Labels))
		if req.Driver != "" {
			v.Driver = req.Driver
		}
		if req.DriverOpts != nil {
			v.Options = maps.Clone(req.DriverOpts)
		}
		writeJSON(w, http.StatusCreated, v)
	case len(parts) == 1 && r.Method == http.MethodGet:
		v, ok := e.volumes[parts[0]]
		if !ok {
			fail(w, http.StatusNotFound, "get %s: no such volume", parts[0])
			return
		}
		writeJSON(w, http.StatusOK, v)
	case len(parts) == 1 && r.Method == http.MethodDelete:
		if _, ok := e.volumes[parts[0]]; !ok {
			fail(w, http.StatusNotFound, "get %s: no such volume", parts[0])
			return
		}
		if id := e.volumeInUse(parts[0]); id != "" {
			fail(w, http.StatusConflict, "remove %s: volume is in use - [%s]", parts[0], id)
			return
		}
		delete(e.volumes, parts[0])
		w.WriteHeader(http.StatusNoContent)
	default:
		fail(w, http.StatusNotFound, "page not found")
	}
}

// findNetwork finds a network as the engine takes a reference: by its
// full ID, then its exact name, then a prefix of one ID alone.
func (e *Engine) findNetwork(ref string) (*docker.Network, error) {
	var found *docker.Network
	for _, n := range e.networks {
		if n.ID == ref {
			return n, nil
		}
	}
	if n, ok := e.networks[ref]; ok {
		return n, nil
	}
	for _, n := range e.networks {
		if ref != "" && strings.HasPrefix(n.ID, ref) {
			if found != nil {
				return nil, fmt.Errorf("network %s is ambiguous (2 matches found based on ID prefix)", ref)
			}
			found = n
		}
	}
	return found, nil
}

// subnetInUse reports a subnet that overlaps a network's.
func (e *Engine) subnetInUse(subnet string) bool {
	want, err := netip.ParsePrefix(subnet)
	if err != nil {
		return false
	}
	for _, n := range e.networks {
		for _, c := range n.IPAM.Config {
			if have, err := netip.ParsePrefix(c.Subnet); err == nil && have.Overlaps(want) {
				return true
			}
		}
	}
	return false
}

// network answers a request for the network ref, or fails it.
func (e *Engine) network(w http.ResponseWriter, ref string) *docker.Network {
	n, err := e.findNetwork(ref)
	switch {
	case err != nil:
		fail(w, http.StatusBadRequest, "%v", err)
	case n == nil:
		fail(w, http.StatusNotFound, "network %s not found", ref)
	}
	return n
}

func (e *Engine) serveNetworks(w http.ResponseWriter, r *http.Request, parts []string) {
	switch {
	case r.Method == http.MethodPost && len(parts) == 1 && parts[0] == "create":
		var req docker.NetworkCreate
		if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
			fail(w, http.StatusBadRequest, "%v", err)
			return
		}
		if e.networks[req.Name] != nil {
			fail(w, http.StatusConflict, "network with name %s already exists", req.Name)
			return
		}
		n := &docker.Network{ID: newID(), Name: req.Name, Driver: req.Driver, Internal: req.Internal, Options: maps.Clone(req.Options), Labels: maps.Clone(req.Labels)}
		if req.IPAM != nil {
			// As the default IPAM driver refuses it.
			for _, c := range req.IPAM.Config {
				if e.subnetInUse(c.Subnet) {
					fail(w, http.StatusForbidden, "invalid pool request: Pool overlaps with other one on this address space")
					return
				}
			}
			n.IPAM = *req.IPAM
			for i := range n.IPAM.Config {
				if prefix, err := netip.ParsePrefix(n.IPAM.Config[i].Subnet); err == nil && n.IPAM.Config[i].Gateway == "" {
					n.IPAM.Config[i].Gateway = prefix.Addr().Next().String()
				}
			}
		} else {
			// What the engine picks: free on this server, and different each time.
			e.nextSubnet++
			subnet := cmp.Or(e.PickSubnet, fmt.Sprintf("172.%d.0.0/16", 17+e.nextSubnet))
			gateway := ""
			if prefix, err := netip.ParsePrefix(subnet); err == nil {
				gateway = prefix.Addr().Next().String()
			}
			n.IPAM.Config = []docker.IPAMConfig{{Subnet: subnet, Gateway: gateway}}
		}
		if n.Driver == "" {
			n.Driver = "bridge"
		}
		if n.Options == nil {
			n.Options = map[string]string{}
		}
		if n.Labels == nil {
			n.Labels = map[string]string{}
		}
		e.networks[req.Name] = n
		writeJSON(w, http.StatusCreated, map[string]string{"Id": n.ID})
	case len(parts) == 1 && r.Method == http.MethodGet:
		n := e.network(w, parts[0])
		if n == nil {
			return
		}
		writeJSON(w, http.StatusOK, n)
	case len(parts) == 1 && r.Method == http.MethodDelete:
		n := e.network(w, parts[0])
		if n == nil {
			return
		}
		// A stopped container has no endpoint, as moby's prune and remove count
		// them, so it doesn't hold a network. It keeps its settings all the same.
		for _, c := range e.containers {
			if _, on := c.NetworkSettings.Networks[n.Name]; on && c.State.Running {
				fail(w, http.StatusForbidden, "error while removing network: network %s id %s has active endpoints", n.Name, n.ID)
				return
			}
		}
		delete(e.networks, n.Name)
		w.WriteHeader(http.StatusNoContent)
	case len(parts) == 0 && r.Method == http.MethodGet:
		list := make([]docker.Network, 0, len(e.networks))
		for _, n := range e.networks {
			list = append(list, *n)
		}
		writeJSON(w, http.StatusOK, list)
	case len(parts) == 2 && parts[1] == "connect" && r.Method == http.MethodPost:
		n := e.network(w, parts[0])
		if n == nil {
			return
		}
		var req struct {
			Container      string
			EndpointConfig docker.EndpointSettings
		}
		if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
			fail(w, http.StatusBadRequest, "%v", err)
			return
		}
		c, err := e.findContainer(req.Container)
		switch {
		case err != nil:
			fail(w, http.StatusBadRequest, "%v", err)
			return
		case c == nil:
			fail(w, http.StatusNotFound, "No such container: %s", req.Container)
			return
		}
		if _, on := c.NetworkSettings.Networks[n.Name]; on {
			fail(w, http.StatusForbidden, "endpoint with name %s already exists in network %s", strings.TrimPrefix(c.Name, "/"), n.Name)
			return
		}
		s := req.EndpointConfig
		if a := s.IPAMConfig; a != nil && a.IPv4Address != "" {
			if len(n.IPAM.Config) == 0 || n.IPAM.Config[0].Subnet == "" {
				fail(w, http.StatusForbidden, "user specified IP address is supported only when connecting to networks with user configured subnets")
				return
			}
			if prefix, err := netip.ParsePrefix(n.IPAM.Config[0].Subnet); err == nil {
				if addr, err := netip.ParseAddr(a.IPv4Address); err != nil || !prefix.Contains(addr) {
					fail(w, http.StatusForbidden, "no configured subnet contains IP address %s", a.IPv4Address)
					return
				}
			}
		}
		if c.NetworkSettings.Networks == nil {
			c.NetworkSettings.Networks = map[string]*docker.EndpointSettings{}
		}
		if c.State.Running {
			s.NetworkID = n.ID
		}
		c.NetworkSettings.Networks[n.Name] = &s
		if c.State.Running {
			if err := e.addressesFree(c); err != nil {
				delete(c.NetworkSettings.Networks, n.Name)
				fail(w, http.StatusForbidden, "%v", err)
				return
			}
			e.attach(c)
		}
		w.WriteHeader(http.StatusOK)
	case len(parts) == 2 && parts[1] == "disconnect" && r.Method == http.MethodPost:
		n := e.network(w, parts[0])
		if n == nil {
			return
		}
		var req struct {
			Container string
		}
		if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
			fail(w, http.StatusBadRequest, "%v", err)
			return
		}
		c, err := e.findContainer(req.Container)
		switch {
		case err != nil:
			fail(w, http.StatusBadRequest, "%v", err)
			return
		case c == nil:
			fail(w, http.StatusNotFound, "No such container: %s", req.Container)
			return
		}
		delete(c.NetworkSettings.Networks, n.Name)
		delete(n.Containers, c.ID)
		w.WriteHeader(http.StatusOK)
	default:
		fail(w, http.StatusNotFound, "page not found")
	}
}

// SetExits makes a container of an image stop right after it starts, or not,
// for the pulls and starts that come after.
func (e *Engine) SetExits(ref string, exits bool) {
	e.mu.Lock()
	defer e.mu.Unlock()
	if i, ok := e.Registry[ref]; ok {
		i.Exits = exits
		e.Registry[ref] = i
	}
	if i := e.image(ref); i != nil {
		i.Exits = exits
	}
}

// SetDockerRoot moves where /info says the engine keeps its data, as
// restarting Docker with another data root does.
func (e *Engine) SetDockerRoot(root string) {
	e.mu.Lock()
	defer e.mu.Unlock()
	e.DockerRoot = root
}

// SentLogins is each X-Registry-Auth a pull or an ask was sent, as
// "image as username:password".
func (e *Engine) SentLogins() []string {
	e.mu.Lock()
	defer e.mu.Unlock()
	return append([]string(nil), e.Logins...)
}

// GivenBuilders is each login a builder was given in Docker's config.json, as
// "registry as username:password", Docker Hub as "docker.io".
func (e *Engine) GivenBuilders() []string {
	e.mu.Lock()
	defer e.mu.Unlock()
	return append([]string(nil), e.BuilderLogins...)
}

// Seen lists the requests the engine has had, as "METHOD /path".
func (e *Engine) Seen() []string {
	e.mu.Lock()
	defer e.mu.Unlock()
	return slices.Clone(e.Requests)
}

// Container returns a container by name, as the engine reports it, or
// nil.
func (e *Engine) Container(name string) *docker.Container {
	e.mu.Lock()
	defer e.mu.Unlock()
	c := e.byNameOrID(name)
	if c == nil {
		return nil
	}
	out := c.Container
	return &out
}

// Serving is what reads as the service whose first container has this name: its
// newest, as a rollout moves a service up its numbers. A name no service's first
// container has is just that container's.
func (e *Engine) Serving(first string) *docker.Container {
	e.mu.Lock()
	var project, service string
	for _, c := range e.containers {
		l := c.Config.Labels
		if l[docker.LabelService] != "" && docker.InstanceName(l[docker.LabelProject], l[docker.LabelService], 1) == first {
			project, service = l[docker.LabelProject], l[docker.LabelService]
			break
		}
	}
	e.mu.Unlock()
	if service == "" {
		return e.Container(first)
	}
	return e.Newest(project, service)
}

// Newest returns the container of a service with the highest number, as the
// engine reports it, or nil.
func (e *Engine) Newest(project, service string) *docker.Container {
	e.mu.Lock()
	defer e.mu.Unlock()
	var best *container
	bestNumber := -1
	for _, c := range e.containers {
		if c.Config.Labels[docker.LabelProject] != project || c.Config.Labels[docker.LabelService] != service {
			continue
		}
		n, _ := strconv.Atoi(c.Config.Labels[docker.LabelNumber])
		if n > bestNumber {
			best, bestNumber = c, n
		}
	}
	if best == nil {
		return nil
	}
	out := best.Container
	return &out
}

// Instances lists the names of a service's containers, sorted.
func (e *Engine) Instances(project, service string) []string {
	e.mu.Lock()
	defer e.mu.Unlock()
	var out []string
	for _, c := range e.containers {
		if c.Config.Labels[docker.LabelProject] == project && c.Config.Labels[docker.LabelService] == service {
			out = append(out, strings.TrimPrefix(c.Name, "/"))
		}
	}
	slices.Sort(out)
	return out
}

// byNameOrID finds a container by its exact name or full ID, for tests. A name
// of the form Compose gives a service's container, env_…-web-1, that no
// container has finds the one with those labels, under its own name, as
// Dockwell names a service's container after its project and environment where
// the control plane says them, default.production.web-1 (ADR 0035), and finds
// it by its labels. Only tests ask this: what the engine answers goes by names.
func (e *Engine) byNameOrID(key string) *container {
	if c, ok := e.containers[key]; ok {
		return c
	}
	if c := e.named(key); c != nil {
		return c
	}
	m := composeForm.FindStringSubmatch(key)
	if m == nil {
		return nil
	}
	for _, c := range e.containers {
		l := c.Config.Labels
		if l[docker.LabelProject] == m[1] && l[docker.LabelService] == m[2] && l[docker.LabelNumber] == m[3] && !forNow.MatchString(c.Name) {
			return c
		}
	}
	return nil
}

var (
	// composeForm is the name Compose gives a service's container in an
	// environment's project: project, service and number.
	composeForm = regexp.MustCompile(`^(env_[0-9a-z]{26})-(.+)-(\d+)$`)
	// forNow starts a name a rollout gives a container for now.
	forNow = regexp.MustCompile(`^/?(?:stopping_)?[0-9a-f]{12}_`)
)

// Containers lists the containers' names, sorted.
func (e *Engine) Containers() []string {
	e.mu.Lock()
	defer e.mu.Unlock()
	var out []string
	for _, c := range e.containers {
		out = append(out, strings.TrimPrefix(c.Name, "/"))
	}
	slices.Sort(out)
	return out
}

// Edit changes a container behind Dockwell's back, as someone running
// docker by hand would.
func (e *Engine) Edit(name string, edit func(*docker.Container)) {
	e.mu.Lock()
	defer e.mu.Unlock()
	if c := e.byNameOrID(name); c != nil {
		edit(&c.Container)
	}
}

// Reports has a container's inspect say keys of a section, such as HostConfig,
// that the client has no field for.
func (e *Engine) Reports(name, section string, keys map[string]any) {
	e.mu.Lock()
	defer e.mu.Unlock()
	c := e.byNameOrID(name)
	if c == nil {
		return
	}
	if c.reports == nil {
		c.reports = map[string]map[string]any{}
	}
	if c.reports[section] == nil {
		c.reports[section] = map[string]any{}
	}
	maps.Copy(c.reports[section], keys)
}

// KillForMemory records the kernel killing something in a container, as its events
// would. It changes nothing else, so a test says what its state reads as.
func (e *Engine) KillForMemory(name string, at time.Time) {
	e.mu.Lock()
	defer e.mu.Unlock()
	if c := e.byNameOrID(name); c != nil {
		e.memoryKills = append(e.memoryKills, memoryKill{id: c.ID, at: at})
	}
}

// serveEvents answers GET /events for the kind of event Dockwell asks after, and
// closes, as Docker does when asked up to now.
func (e *Engine) serveEvents(w http.ResponseWriter, r *http.Request) {
	if e.EventsFail {
		http.Error(w, "the daemon is busy", http.StatusInternalServerError)
		return
	}
	var filters map[string][]string
	if f := r.URL.Query().Get("filters"); f != "" {
		if err := json.Unmarshal([]byte(f), &filters); err != nil {
			http.Error(w, err.Error(), http.StatusBadRequest)
			return
		}
	}
	since, until := eventTime(r.URL.Query().Get("since")), eventTime(r.URL.Query().Get("until"))
	w.WriteHeader(http.StatusOK)
	wants := func(key, v string) bool { return len(filters[key]) == 0 || slices.Contains(filters[key], v) }
	for _, ev := range e.events {
		if ev.at.Before(since) || ev.at.After(until) || !wants("container", ev.id) || !wants("event", ev.action) || !wants("type", "container") {
			continue
		}
		_ = json.NewEncoder(w).Encode(map[string]any{
			"Type": "container", "Action": ev.action, "Actor": map[string]string{"ID": ev.id}, "id": ev.id,
			"time": ev.at.Unix(), "timeNano": ev.at.UnixNano(),
		})
	}
	for _, kill := range e.memoryKills {
		if slices.Contains(filters["container"], kill.id) && slices.Contains(filters["event"], "oom") && !kill.at.Before(since) && !kill.at.After(until) {
			_ = json.NewEncoder(w).Encode(map[string]string{"Type": "container", "Action": "oom", "id": kill.id})
		}
	}
}

// SetHealth moves a container's health check on, as its next run would, with
// what it printed.
func (e *Engine) SetHealth(name, status, output string) {
	e.mu.Lock()
	defer e.mu.Unlock()
	c := e.byNameOrID(name)
	if c == nil {
		return
	}
	run := docker.HealthRun{End: time.Now(), Output: output}
	h := &docker.Health{Status: status}
	if status == "unhealthy" {
		run.ExitCode, h.FailingStreak = 1, 3
	}
	h.Log = []docker.HealthRun{run}
	c.State.Health = h
}

// SetRestarting makes a container keep restarting, as an exiting one with a
// restart policy does.
func (e *Engine) SetRestarting(name string, exitCode int) {
	e.mu.Lock()
	defer e.mu.Unlock()
	if c := e.byNameOrID(name); c != nil {
		c.State = docker.State{Status: "restarting", Restarting: true, ExitCode: exitCode}
		c.RestartCount++
	}
}

// Stop stops a container behind Dockwell's back.
func (e *Engine) Stop(name string) {
	e.mu.Lock()
	defer e.mu.Unlock()
	if c := e.byNameOrID(name); c != nil {
		if c.State.Running {
			e.emit(c, "kill", "stop", "die")
		}
		c.State = docker.State{Status: "exited", StartedAt: c.State.StartedAt, FinishedAt: e.wall().UTC()}
		c.log.end()
		e.detach(c)
	}
}

// Kill kills a container behind Dockwell's back, as docker kill does.
func (e *Engine) Kill(name string) {
	e.mu.Lock()
	defer e.mu.Unlock()
	if c := e.byNameOrID(name); c != nil && c.State.Running {
		c.State = docker.State{Status: "exited", ExitCode: 137, StartedAt: c.State.StartedAt, FinishedAt: e.wall().UTC()}
		c.log.end()
		e.detach(c)
		e.emit(c, "kill", "die")
	}
}

// Exit ends a container on its own, with code, as its process returning does:
// Docker says it died, and nothing stopped or killed it. A restart policy that
// would bring it back isn't acted out.
func (e *Engine) Exit(name string, code int) {
	e.mu.Lock()
	defer e.mu.Unlock()
	if c := e.byNameOrID(name); c != nil && c.State.Running {
		c.State = docker.State{Status: "exited", ExitCode: code, StartedAt: c.State.StartedAt, FinishedAt: e.wall().UTC()}
		c.log.end()
		e.detach(c)
		e.emit(c, "die")
	}
}

// DaemonKilled is the engine killed with no warning and started again, without
// live-restore, as a real one was seen to behave (docs/testing/chaos.md): it
// remembers no event from before; it stops every container that outlived it,
// saying kill, stop and die of each as it does of one a person stops, and
// leaving each exited with no error; and it starts again only those whose
// policy is always, whether they ran or not. With LiveRestore, what ran keeps
// running and nothing is said.
func (e *Engine) DaemonKilled() {
	e.mu.Lock()
	defer e.mu.Unlock()
	e.events, e.memoryKills = nil, nil
	if e.LiveRestore {
		return
	}
	for _, id := range slices.Sorted(maps.Keys(e.containers)) {
		c := e.containers[id]
		if c.State.Running {
			c.State = docker.State{Status: "exited", StartedAt: c.State.StartedAt, FinishedAt: e.wall().UTC()}
			c.log.end()
			e.detach(c)
			e.emit(c, "kill", "stop", "die")
		}
	}
	for _, id := range slices.Sorted(maps.Keys(e.containers)) {
		c := e.containers[id]
		if c.HostConfig.RestartPolicy.Name == "always" && c.State.Status == "exited" {
			e.clock++
			c.State = docker.State{Status: "running", Running: true, StartedAt: e.wall().Add(time.Duration(e.clock) * time.Nanosecond).UTC()}
			c.log.start()
			e.attach(c)
			e.emit(c, "start")
		}
	}
}

// Reboot is the server, or Docker, starting again: what ran comes back as its
// restart policy says, with a new start time, and what was stopped, or only
// made, stays as it is.
func (e *Engine) Reboot() {
	e.mu.Lock()
	defer e.mu.Unlock()
	// A new engine remembers nothing of the last one's events.
	e.events = nil
	for _, id := range slices.Sorted(maps.Keys(e.containers)) {
		c := e.containers[id]
		policy := c.HostConfig.RestartPolicy.Name
		ran := c.State.Running || c.shutDown
		c.shutDown = false
		back := policy == "always" || ran && policy != "" && policy != "no" && policy != "on-failure"
		if !back {
			if c.State.Running {
				c.State = docker.State{Status: "exited", ExitCode: 255}
				e.detach(c)
			}
			continue
		}
		e.clock++
		health := c.State.Health
		c.State = docker.State{Status: "running", Running: true, StartedAt: e.wall().Add(time.Duration(e.clock) * time.Nanosecond).UTC(), Health: health}
		e.attach(c)
		e.emit(c, "start")
	}
}

// ShuttingDown is the engine asked to stop, as a server shutting down asks it: it
// stops each container that runs, saying kill, stop and die of each while it
// still answers, and doesn't count them stopped by anyone, so Reboot brings back
// those whose policy has them run.
func (e *Engine) ShuttingDown() {
	e.mu.Lock()
	defer e.mu.Unlock()
	for _, id := range slices.Sorted(maps.Keys(e.containers)) {
		c := e.containers[id]
		if !c.State.Running {
			continue
		}
		c.State = docker.State{Status: "exited", StartedAt: c.State.StartedAt, FinishedAt: e.wall().UTC()}
		c.shutDown = true
		c.log.end()
		e.detach(c)
		e.emit(c, "kill", "stop", "die")
	}
}

// NeverStarted leaves a container as docker create does: made, and not started.
func (e *Engine) NeverStarted(name string) {
	e.mu.Lock()
	defer e.mu.Unlock()
	if c := e.byNameOrID(name); c != nil {
		c.State = docker.State{Status: "created"}
		c.log.end()
		e.detach(c)
	}
}

// Remove removes a container behind Dockwell's back.
func (e *Engine) Remove(name string) {
	e.mu.Lock()
	defer e.mu.Unlock()
	if c := e.byNameOrID(name); c != nil {
		if c.State.Running {
			e.emit(c, "kill", "die")
		}
		e.emit(c, "destroy")
		e.detach(c)
		delete(e.containers, c.ID)
	}
}

// VolumeNames lists the volumes the engine has, by name, sorted.
func (e *Engine) VolumeNames() []string {
	e.mu.Lock()
	defer e.mu.Unlock()
	return slices.Sorted(maps.Keys(e.volumes))
}

// Volume returns a volume by name, or nil.
func (e *Engine) Volume(name string) *docker.Volume {
	e.mu.Lock()
	defer e.mu.Unlock()
	v, ok := e.volumes[name]
	if !ok {
		return nil
	}
	out := *v
	return &out
}

// Network returns a network by name, or nil.
func (e *Engine) Network(name string) *docker.Network {
	e.mu.Lock()
	defer e.mu.Unlock()
	n, ok := e.networks[name]
	if !ok {
		return nil
	}
	out := *n
	return &out
}

// AddContainer makes a container behind Dockwell's back, as someone
// running docker by hand would. An ID left out is made up, and one given
// as a prefix, such as "db", is filled out to 64 hex digits. It starts out
// running on Docker's bridge network unless it says otherwise.
func (e *Engine) AddContainer(c docker.Container) *docker.Container {
	e.mu.Lock()
	defer e.mu.Unlock()
	c.ID += newID()[len(c.ID):]
	if c.State.Status == "" {
		c.State = docker.State{Status: "running", Running: true}
	}
	if c.NetworkSettings.Networks == nil {
		c.NetworkSettings.Networks = map[string]*docker.EndpointSettings{"bridge": {}}
	}
	img := e.image(c.Config.Image)
	if img == nil {
		img = &image{}
	}
	e.clock++
	ctr := &container{Container: c, img: img, created: e.clock}
	ctr.log.ended = !c.State.Running
	e.containers[c.ID] = ctr
	e.process(ctr)
	e.attach(ctr)
	out := c
	return &out
}

// AddVolume makes a volume behind Dockwell's back.
func (e *Engine) AddVolume(v docker.Volume) {
	e.mu.Lock()
	defer e.mu.Unlock()
	e.volumes[v.Name] = &v
}

// AddNetwork makes a network behind Dockwell's back.
func (e *Engine) AddNetwork(n docker.Network) {
	e.mu.Lock()
	defer e.mu.Unlock()
	n.ID += newID()[len(n.ID):] // made up, or filled out from a prefix
	e.networks[n.Name] = &n
}

// ComposeFilters are the filters docker compose lists a project's
// containers with, as its getDefaultFilters in pkg/compose/containers.go
// has them at v2.38.2: the project's label, a config hash, and not one
// off, with the service's label when it names one. A container these
// don't find, docker compose ps leaves out and docker compose up fails
// on, as its name is taken.
func ComposeFilters(project string, service ...string) map[string][]string {
	labels := []string{docker.LabelProject + "=" + project, docker.LabelConfigHash, docker.LabelOneoff + "=False"}
	if len(service) == 1 {
		labels = append(labels, docker.LabelService+"="+service[0])
	}
	return map[string][]string{"label": labels}
}

// hostMountinfo is the server's own mounts, as the fake's Mountinfo has them: one
// disk, at /.
const hostMountinfo = "22 1 8:1 / / rw,relatime shared:1 - ext4 /dev/sda1 rw\n"

// process gives a running container a process ID, as Docker's State.Pid, and a
// list of what it has mounted, as a kernel would have mounted it as it started: a
// bind through a link is where the link led then, whatever it says since. A container
// that stops loses both, and gets them again as it runs. e.mu is held.
func (e *Engine) process(c *container) {
	if !c.State.Running {
		c.State.Pid, c.mountinfo = 0, ""
		return
	}
	if c.State.Pid != 0 {
		return
	}
	e.pids++
	c.State.Pid = 40000 + e.pids
	var b strings.Builder
	b.WriteString("600 500 0:90 / / rw - overlay overlay rw\n")
	for i, m := range c.Mounts {
		dev, root, fstype := "8:1", "", "ext4"
		switch m.Type {
		case "bind":
			root = resolved(m.Source)
		case "volume":
			v := e.volumes[m.Name]
			var options map[string]string
			if v != nil {
				options = v.Options
			}
			switch kind := options["type"]; {
			case len(options) == 0:
				root = "/var/lib/docker/volumes/" + m.Name + "/_data"
			case strings.Contains(","+options["o"]+",", ",bind,") || strings.Contains(","+options["o"]+",", ",rbind,"):
				root = resolved(filepath.Clean("/" + options["device"]))
			case options["device"] == "/dev/sda1":
				root, fstype = "/", kind
			default:
				dev, root, fstype = fmt.Sprintf("0:%d", 100+i), "/", kind
			}
		default:
			continue
		}
		fmt.Fprintf(&b, "%d 600 %s %s %s rw - %s none rw\n", 601+i, dev, root, m.Destination, fstype)
	}
	c.mountinfo = b.String()
}

// resolved is where p leads through links now, or p where it leads nowhere.
func resolved(p string) string {
	if to, err := filepath.EvalSymlinks(p); err == nil {
		return to
	}
	return filepath.Clean(p)
}

// Mountinfo is what a running container with process pid has mounted, as its
// /proc/pid/mountinfo would list it, and the server's own for pid 0. It stands in
// for the kernel, for an agent's root check (agent.MountTable).
func (e *Engine) Mountinfo(pid int) (string, error) {
	if pid == 0 {
		return hostMountinfo, nil
	}
	e.mu.Lock()
	defer e.mu.Unlock()
	for _, c := range e.containers {
		if c.State.Running && c.State.Pid == pid {
			return c.mountinfo, nil
		}
	}
	return "", fmt.Errorf("open /proc/%d/mountinfo: no such file or directory", pid)
}

// signedIn says whether a request may have ref: one with no Login may, and one with
// a Login may with that login in X-Registry-Auth. It logs what login came.
func (e *Engine) signedIn(r *http.Request, ref string) bool {
	given := ""
	if h := r.Header.Get("X-Registry-Auth"); h != "" {
		var a struct{ Username, Password string }
		if raw, err := base64.URLEncoding.DecodeString(h); err == nil && json.Unmarshal(raw, &a) == nil {
			given = a.Username + ":" + a.Password
		}
		e.Logins = append(e.Logins, ref+" as "+given)
	}
	src, ok := e.Registry[ref]
	if !ok && e.Find != nil {
		src, ok = e.Find(ref)
	}
	return !ok || src.Login == "" || src.Login == given
}
