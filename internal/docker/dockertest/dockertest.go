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
	"strings"
	"sync"
	"testing"

	"github.com/dockwell/dockwell/internal/docker"
)

// Image is an image the fake can pull.
type Image struct {
	Config docker.Config
	// Volumes are paths the image declares, which get anonymous volumes.
	Volumes []string
	// Exits makes a container of it stop right after it starts, with
	// exit code 1.
	Exits bool
	// Unhealthy makes its health check fail, and Starting keeps it
	// starting, neither passing nor failing yet.
	Unhealthy, Starting bool
	// Size is what pulling it downloads, in bytes.
	Size int64
}

// Engine is a fake Docker Engine.
type Engine struct {
	// Socket is where it listens.
	Socket string
	// APIVersion and MinAPIVersion are what /version reports.
	APIVersion, MinAPIVersion string
	// DockerRoot is where /info says the engine keeps its data:
	// /var/lib/docker unless set.
	DockerRoot string

	srv *httptest.Server

	mu        sync.Mutex
	intercept func(*http.Request) error
	// Registry has the images a pull can find, by reference.
	Registry   map[string]Image
	images     map[string]*image // pulled, by reference and by ID
	containers map[string]*container
	volumes    map[string]*docker.Volume
	networks   map[string]*docker.Network
	// Pulls counts pulls, and Creates containers created.
	Pulls, Creates int
	clock          int64 // orders what is made
	// Requests logs each request, as "METHOD /path".
	Requests []string
}

type image struct {
	id string
	Image
}

type container struct {
	docker.Container
	img     *image
	created int64 // when it was made, in a count that only goes up
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
		images:     map[string]*image{},
		containers: map[string]*container{},
		volumes:    map[string]*docker.Volume{},
		networks: map[string]*docker.Network{
			"bridge": {ID: newID(), Name: "bridge", Driver: "bridge", Options: map[string]string{}, Labels: map[string]string{}},
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
		writeJSON(w, http.StatusOK, map[string]string{"Version": "27.3.1", "ApiVersion": e.APIVersion, "MinAPIVersion": e.MinAPIVersion})
	case path == "/info":
		root := e.DockerRoot
		if root == "" {
			root = "/var/lib/docker"
		}
		writeJSON(w, http.StatusOK, map[string]string{"DockerRootDir": root})
	case parts[0] == "containers":
		e.serveContainers(w, r, parts[1:])
	case parts[0] == "images":
		e.serveImages(w, r, path)
	case parts[0] == "volumes":
		e.serveVolumes(w, r, parts[1:])
	case parts[0] == "networks":
		e.serveNetworks(w, r, parts[1:])
	default:
		fail(w, http.StatusNotFound, "page not found")
	}
}

// image finds a pulled image as the engine takes a reference: by its name
// and tag, then its ID, then, for a reference without a tag, a prefix of
// one ID alone. So an image named by hex digits alone, such as cafe, can
// find an image whose ID starts with them when cafe:latest isn't pulled.
func (e *Engine) image(ref string) *image {
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
	case r.Method == http.MethodPost && path == "/images/create":
		ref := r.URL.Query().Get("fromImage")
		if tag := r.URL.Query().Get("tag"); tag != "" {
			ref += ":" + tag
		}
		e.Pulls++
		w.Header().Set("Content-Type", "application/json")
		enc := json.NewEncoder(w)
		src, ok := e.Registry[ref]
		if !ok {
			_ = enc.Encode(map[string]any{
				"error":       "pull access denied for " + ref + ", repository does not exist or may require 'docker login'",
				"errorDetail": map[string]string{"message": "pull access denied for " + ref + ", repository does not exist or may require 'docker login'"},
			})
			return
		}
		_ = enc.Encode(map[string]string{"status": "Pulling from " + ref})
		if src.Size > 0 {
			for _, part := range []int64{src.Size / 4, src.Size / 2, src.Size} {
				_ = enc.Encode(map[string]any{"status": "Downloading", "id": "layer1", "progressDetail": map[string]int64{"current": part, "total": src.Size}})
			}
			_ = enc.Encode(map[string]any{"status": "Pull complete", "id": "layer1"})
		}
		img := &image{id: "sha256:" + newID(), Image: src}
		e.images[ref], e.images[img.id] = img, img
		_ = enc.Encode(map[string]string{"status": "Status: Downloaded newer image for " + ref})
	case r.Method == http.MethodGet && strings.HasSuffix(path, "/json"):
		ref := strings.TrimSuffix(strings.TrimPrefix(path, "/images/"), "/json")
		img := e.image(ref)
		if img == nil {
			fail(w, http.StatusNotFound, "No such image: %s", ref)
			return
		}
		cfg := img.Config
		writeJSON(w, http.StatusOK, docker.Image{ID: img.id, Config: &cfg})
	default:
		fail(w, http.StatusNotFound, "page not found")
	}
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
		writeJSON(w, http.StatusOK, c.Container)
	case r.Method == http.MethodPost && action == "start":
		if c.State.Running {
			w.WriteHeader(http.StatusNotModified)
			return
		}
		for name := range c.NetworkSettings.Networks {
			if e.networks[name] == nil {
				fail(w, http.StatusNotFound, "network %s not found", name)
				return
			}
		}
		if err := e.portsFree(c); err != nil {
			fail(w, http.StatusInternalServerError, "%v", err)
			return
		}
		c.State = docker.State{Status: "running", Running: true}
		if c.img.Exits {
			c.State = docker.State{Status: "exited", ExitCode: 1}
		}
		if hc := c.Config.Healthcheck; hc != nil && len(hc.Test) > 0 && hc.Test[0] != "NONE" && c.State.Running {
			c.State.Health = &docker.Health{Status: "healthy"}
			switch {
			case c.img.Unhealthy:
				c.State.Health = &docker.Health{Status: "unhealthy", FailingStreak: 3}
			case c.img.Starting:
				c.State.Health = &docker.Health{Status: "starting"}
			}
		}
		e.attach(c)
		w.WriteHeader(http.StatusNoContent)
	case r.Method == http.MethodPost && action == "stop":
		if !c.State.Running {
			w.WriteHeader(http.StatusNotModified)
			return
		}
		c.State = docker.State{Status: "exited"}
		e.detach(c)
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
		var body struct{ RestartPolicy docker.RestartPolicy }
		if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
			fail(w, http.StatusBadRequest, "%v", err)
			return
		}
		c.HostConfig.RestartPolicy = body.RestartPolicy
		writeJSON(w, http.StatusOK, map[string]any{"Warnings": []string{}})
	case r.Method == http.MethodDelete && action == "":
		if c.State.Running && r.URL.Query().Get("force") != "true" {
			fail(w, http.StatusConflict, "cannot remove container %s: container is running: stop the container before removing or force remove", c.Name)
			return
		}
		e.detach(c)
		delete(e.containers, c.ID)
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
// must match; volume is a volume's name, or where something is mounted.
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
	}) && passes("volume", func(v string) bool {
		return slices.ContainsFunc(c.Mounts, func(m docker.MountPoint) bool { return m.Name == v || m.Source == v || m.Destination == v })
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
			out = append(out, docker.ContainerSummary{ID: c.ID, Names: []string{c.Name}, Labels: c.Config.Labels, Created: c.created})
		}
	}
	slices.SortFunc(out, func(a, b docker.ContainerSummary) int { return int(b.Created - a.Created) })
	writeJSON(w, http.StatusOK, out)
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

// attach and detach keep each network's list of running containers.
func (e *Engine) attach(c *container) {
	for name := range c.NetworkSettings.Networks {
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
	c.ID = newID()
	c.Name = "/" + name
	c.Image = img.id
	c.Config = merge(req.Config, img.Config)
	c.HostConfig = req.HostConfig
	c.State = docker.State{Status: "created"}
	mode := req.HostConfig.NetworkMode
	if mode == "" || mode == "default" {
		mode = "bridge"
		c.HostConfig.NetworkMode = "bridge"
	}
	// The engine takes networks as references, and reports them by name.
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
		c.NetworkSettings.Networks[n.Name] = s
	}
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
		for _, c := range e.containers {
			if _, on := c.NetworkSettings.Networks[n.Name]; on {
				fail(w, http.StatusForbidden, "error while removing network: network %s id %s has active endpoints", n.Name, n.ID)
				return
			}
		}
		delete(e.networks, n.Name)
		w.WriteHeader(http.StatusNoContent)
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
		s := req.EndpointConfig
		c.NetworkSettings.Networks[n.Name] = &s
		if c.State.Running {
			e.attach(c)
		}
		w.WriteHeader(http.StatusOK)
	default:
		fail(w, http.StatusNotFound, "page not found")
	}
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

// byNameOrID finds a container by its exact name or full ID, for tests.
func (e *Engine) byNameOrID(key string) *container {
	if c, ok := e.containers[key]; ok {
		return c
	}
	return e.named(key)
}

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

// Stop stops a container behind Dockwell's back.
func (e *Engine) Stop(name string) {
	e.mu.Lock()
	defer e.mu.Unlock()
	if c := e.byNameOrID(name); c != nil {
		c.State = docker.State{Status: "exited"}
		e.detach(c)
	}
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
	e.containers[c.ID] = ctr
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
