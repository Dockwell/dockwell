// SPDX-License-Identifier: AGPL-3.0-only

package caddy

import (
	"encoding/json"
	"os"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/dockwell/dockwell/internal/docker"
	"github.com/dockwell/dockwell/internal/docker/dockertest"
	"github.com/dockwell/dockwell/internal/ingress"
)

func onEngine(t *testing.T) (*Container, *dockertest.Engine) {
	t.Helper()
	e := dockertest.New(t)
	e.Registry[Image] = dockertest.Image{}
	e.Registry["registry.example.com/library/caddy@"+Digest] = dockertest.Image{}
	return &Container{Client: e.Client(), Dir: t.TempDir()}, e
}

// Everything the ADR says of the container, as Docker is asked for it: what is
// real on Docker is for CI's Service end to end on Docker job to prove.
func TestCaddysContainerIsAsTheADRSays(t *testing.T) {
	c, e := onEngine(t)
	if err := c.Run(t.Context(), "/config/bootstrap.json"); err != nil {
		t.Fatal(err)
	}
	got := e.Container(ingress.ContainerName)
	if got == nil {
		t.Fatal("no container")
	}
	if got.Config.Image != Image || !strings.HasSuffix(got.Config.Image, "@"+Digest) {
		t.Errorf("Image = %q, want it by digest", got.Config.Image)
	}
	if got.HostConfig.NetworkMode != "host" {
		t.Errorf("NetworkMode = %q, want the server's own network, for clients' real addresses and HTTP/3", got.HostConfig.NetworkMode)
	}
	if got.HostConfig.RestartPolicy.Name != "unless-stopped" {
		t.Errorf("RestartPolicy = %q, want it back after a reboot", got.HostConfig.RestartPolicy.Name)
	}
	if !slices.Equal(got.Config.Cmd, []string{"caddy", "run", "--config", "/config/bootstrap.json", "--resume"}) {
		t.Errorf("Cmd = %v, want it resuming its last config", got.Config.Cmd)
	}
	if !slices.Equal(got.HostConfig.CapDrop, []string{"ALL"}) || !slices.Equal(got.HostConfig.CapAdd, []string{"NET_BIND_SERVICE"}) {
		t.Errorf("capabilities drop %v, add %v", got.HostConfig.CapDrop, got.HostConfig.CapAdd)
	}
	if !got.HostConfig.ReadonlyRootfs {
		t.Error("the root filesystem is writable")
	}
	wantUser := strconv.Itoa(os.Geteuid()) + ":" + strconv.Itoa(os.Getegid())
	if os.Geteuid() == 0 {
		wantUser = strconv.Itoa(ProxyUID) + ":" + strconv.Itoa(ProxyUID)
	}
	if got.Config.User != wantUser {
		t.Errorf("User = %q, want %q", got.Config.User, wantUser)
	}
	if got.Config.User == "0:0" {
		t.Error("Caddy runs as root, which on the server's network reaches abstract sockets")
	}
	if len(got.HostConfig.PortBindings) != 0 {
		t.Errorf("it publishes %v, on a host network", got.HostConfig.PortBindings)
	}
	for _, name := range []string{"data", "config", "run"} {
		bind := filepath.Join(c.Dir, name) + ":"
		if !slices.ContainsFunc(got.HostConfig.Binds, func(b string) bool { return strings.HasPrefix(b, bind) && strings.HasSuffix(b, ":Z") }) {
			t.Errorf("Binds = %v, want %s from the data directory, relabelled for SELinux", got.HostConfig.Binds, name)
		}
	}
	// What Caddy reads as it starts is root's, and it can't change it.
	if boot := filepath.Join(c.Dir, bootDirName) + ":" + containerBoot + ":ro,Z"; !slices.Contains(got.HostConfig.Binds, boot) {
		t.Errorf("Binds = %v, want %s", got.HostConfig.Binds, boot)
	}
	// With no config to read, Caddy's default admin address is TCP.
	if want := "CADDY_ADMIN=unix//run/dockwell/admin.sock|0600"; !slices.Contains(got.Config.Env, want) {
		t.Errorf("Env = %v, want %s", got.Config.Env, want)
	}
	if l := got.HostConfig.LogConfig; l == nil || l.Config["max-size"] == "" {
		t.Errorf("LogConfig = %+v, want its logs bounded", l)
	}
	if got.HostConfig.Memory != 1<<30 || got.HostConfig.MemorySwap != 1<<30 {
		t.Errorf("memory %d, with swap %d, want 1 GiB and no swap", got.HostConfig.Memory, got.HostConfig.MemorySwap)
	}
	if got.Config.Labels[versionLabel] != Tag {
		t.Errorf("Labels = %v, want the version it runs", got.Config.Labels)
	}
	st, err := c.Status(t.Context())
	if err != nil || !st.Running || !st.Exists || st.Outdated {
		t.Errorf("Status = %+v, %v", st, err)
	}
}

// A daemon can set no-new-privileges for every container, and the image's binary
// binds low ports with a file capability, which that setting ignores. Moby takes
// this from the container's own options after the daemon's (parseSecurityOpt, in
// daemon_unix.go), so it holds on such a daemon, and leaves the rest as they were.
func TestCaddySaysNoNewPrivileges(t *testing.T) {
	c := &Container{Dir: t.TempDir()}
	got := c.request("caddy@sha256:abc", "/etc/dockwell/bootstrap.json")
	if !slices.Contains(got.HostConfig.SecurityOpt, "no-new-privileges=false") {
		t.Errorf("SecurityOpt = %v, want no-new-privileges=false", got.HostConfig.SecurityOpt)
	}
}

// An engine with userns-remap refuses the host network to a container in a user
// namespace (Moby's daemon_unix.go), so Caddy says it stays in the server's.
func TestCaddyRunsOnAnEngineWithUsernsRemap(t *testing.T) {
	c, e := onEngine(t)
	e.UsernsRemap = true
	if err := c.Run(t.Context(), "/etc/dockwell/bootstrap.json"); err != nil {
		t.Fatalf("Run = %v", err)
	}
	if got := e.Container(ingress.ContainerName); got == nil || got.HostConfig.UsernsMode != "host" {
		t.Errorf("the container is %+v", got)
	}
}

// What a person sees of the version it runs, and #78 starts from.
func TestStatusSaysWhichVersionItRuns(t *testing.T) {
	c, e := onEngine(t)
	if err := c.Run(t.Context(), "/etc/dockwell/bootstrap.json"); err != nil {
		t.Fatal(err)
	}
	st, err := c.Status(t.Context())
	if err != nil || st.Version != Tag || st.Pinned != Tag || st.Image != Image || st.Outdated {
		t.Errorf("Status = %+v, %v", st, err)
	}
	e.Edit(ingress.ContainerName, func(got *docker.Container) {
		got.Config.Image = "caddy@sha256:0000"
		got.Config.Labels[versionLabel] = "2.11.6-alpine"
	})
	st, err = c.Status(t.Context())
	if err != nil || st.Version != "2.11.6-alpine" || st.Pinned != Tag || !st.Outdated {
		t.Errorf("Status of an older one = %+v, %v", st, err)
	}
}

// Docker keeps how many times it started Caddy again, and whether the last was for
// its memory until it starts it, and after that only in its events. That's all that
// says why every site dropped for a moment, and it's read once for each restart.
func TestStatusSaysCaddyRestartedForMemory(t *testing.T) {
	c, e := onEngine(t)
	if err := c.Run(t.Context(), "/etc/dockwell/bootstrap.json"); err != nil {
		t.Fatal(err)
	}
	if st, err := c.Status(t.Context()); err != nil || st.Restarts != 0 || st.OutOfMemory {
		t.Errorf("Status of a fresh one = %+v, %v", st, err)
	}
	// Killed, and not started again yet: its state says.
	e.Edit(ingress.ContainerName, func(got *docker.Container) {
		got.RestartCount = 1
		got.State.OOMKilled = true
	})
	if st, err := c.Status(t.Context()); err != nil || st.Restarts != 1 || !st.OutOfMemory {
		t.Errorf("Status killed for memory = %+v, %v", st, err)
	}
	// Started again: its state doesn't, and its events do.
	started := time.Now()
	e.KillForMemory(ingress.ContainerName, started.Add(-time.Second))
	e.Edit(ingress.ContainerName, func(got *docker.Container) {
		got.RestartCount = 2
		got.State.OOMKilled = false
		got.State.StartedAt = started
	})
	if st, err := c.Status(t.Context()); err != nil || st.Restarts != 2 || !st.OutOfMemory || !st.Running {
		t.Errorf("Status after a restart for memory = %+v, %v", st, err)
	}
	asked := func() int {
		return len(slices.DeleteFunc(slices.Clone(e.Requests), func(r string) bool { return !strings.HasSuffix(r, "/events") }))
	}
	if st, err := c.Status(t.Context()); err != nil || !st.OutOfMemory || asked() != 1 {
		t.Errorf("Status read again = %+v, %v, with %d reads of its events, want one", st, err, asked())
	}
	// Started again for something else, long after: its events say nothing of that.
	e.Edit(ingress.ContainerName, func(got *docker.Container) {
		got.RestartCount = 3
		got.State.StartedAt = started.Add(time.Hour)
	})
	if st, err := c.Status(t.Context()); err != nil || st.Restarts != 3 || st.OutOfMemory {
		t.Errorf("Status after a restart for something else = %+v, %v", st, err)
	}
}

func TestAMirrorChangesTheRepositoryAlone(t *testing.T) {
	c, e := onEngine(t)
	c.Mirror = "registry.example.com/library/caddy"
	if err := c.Run(t.Context(), "/config/bootstrap.json"); err != nil {
		t.Fatal(err)
	}
	got := e.Container(ingress.ContainerName).Config.Image
	if got != "registry.example.com/library/caddy@"+Digest {
		t.Errorf("Image = %q, want the mirror's repository at the pinned digest", got)
	}
	if e.Pulls != 1 {
		t.Errorf("%d pulls", e.Pulls)
	}
}

func TestAPullThatFailsSaysHowToMirror(t *testing.T) {
	c, _ := onEngine(t)
	c.Mirror = "registry.example.com/nothing/here"
	err := c.Pull(t.Context())
	if err == nil || !strings.Contains(err.Error(), "registry-mirrors") || !strings.Contains(err.Error(), "DOCKWELL_INGRESS_IMAGE") {
		t.Errorf("Pull = %v, want it saying how to use a mirror", err)
	}
}

func TestAnotherVersionReadsAsOutdatedAndIsReplaced(t *testing.T) {
	c, e := onEngine(t)
	if err := c.Run(t.Context(), "/config/bootstrap.json"); err != nil {
		t.Fatal(err)
	}
	first := e.Container(ingress.ContainerName).ID
	e.Edit(ingress.ContainerName, func(got *docker.Container) { got.Config.Image = "caddy@sha256:0000" })
	st, err := c.Status(t.Context())
	if err != nil || !st.Outdated {
		t.Errorf("Status = %+v, %v, want outdated", st, err)
	}
	if err := c.Run(t.Context(), "/config/bootstrap.json"); err != nil {
		t.Fatal(err)
	}
	got := e.Container(ingress.ContainerName)
	if got.ID == first || got.Config.Image != Image {
		t.Errorf("after running again: %s, %s", got.ID, got.Config.Image)
	}
}

func TestAContainerOfTheNameThatIsntOursIsLeftAlone(t *testing.T) {
	c, e := onEngine(t)
	e.AddContainer(docker.Container{Name: "/" + ingress.ContainerName, Config: docker.Config{Image: "nginx"}, State: docker.State{Running: true}})
	if _, err := c.Status(t.Context()); err == nil || !strings.Contains(err.Error(), "didn't make it") {
		t.Errorf("Status = %v, want it refused, saying so", err)
	}
	if err := c.Run(t.Context(), "/config/bootstrap.json"); err == nil {
		t.Error("Run replaced a container Dockwell didn't make")
	}
	if err := c.Remove(t.Context()); err == nil {
		t.Error("Remove took a container Dockwell didn't make")
	}
	if got := e.Container(ingress.ContainerName); got == nil || got.Config.Image != "nginx" {
		t.Errorf("the container is now %+v", got)
	}
}

func TestRemovingCaddyKeepsItsData(t *testing.T) {
	c, e := onEngine(t)
	if err := c.Run(t.Context(), "/config/bootstrap.json"); err != nil {
		t.Fatal(err)
	}
	cert := filepath.Join(c.Dir, "data", "caddy", "certificates", "x", "x.crt")
	if err := os.MkdirAll(filepath.Dir(cert), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(cert, []byte("cert"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := c.Remove(t.Context()); err != nil {
		t.Fatal(err)
	}
	if e.Container(ingress.ContainerName) != nil {
		t.Error("the container stayed")
	}
	if _, err := os.Stat(cert); err != nil {
		t.Errorf("the certificate went with it: %v", err)
	}
}

// The bootstrap config never leaves the admin API on Caddy's default, TCP on
// the server's loopback.
func TestTheBootstrapNamesTheSocket(t *testing.T) {
	c := &Caddy{Dir: t.TempDir(), Runtime: &Container{}}
	b, err := c.bootstrap(nil, false)
	if err != nil {
		t.Fatal(err)
	}
	var config struct {
		Admin struct {
			Listen string `json:"listen"`
		} `json:"admin"`
		Apps struct {
			HTTP struct {
				GracePeriod string `json:"grace_period"`
				Servers     map[string]struct {
					Strict bool     `json:"strict_sni_host"`
					Listen []string `json:"listen"`
				} `json:"servers"`
			} `json:"http"`
			TLS json.RawMessage `json:"tls"`
		} `json:"apps"`
	}
	if err := json.Unmarshal(b, &config); err != nil {
		t.Fatal(err)
	}
	if config.Admin.Listen != "unix//run/dockwell/admin.sock|0600" {
		t.Errorf("admin.listen = %q", config.Admin.Listen)
	}
	if config.Apps.HTTP.GracePeriod == "" {
		t.Error("no grace period, so an old config wouldn't let a long request finish")
	}
	s := config.Apps.HTTP.Servers[serverName]
	if !s.Strict || !slices.Equal(s.Listen, []string{":443"}) {
		t.Errorf("the server is %+v", s)
	}
	if len(config.Apps.TLS) != 0 {
		t.Errorf("the default issuer is Caddy's own, Let's Encrypt, with no config: %s", config.Apps.TLS)
	}
}

// Nothing a spec holds can be anything but the value it is in the JSON.
func TestARoutesJSONIsBuiltFromValues(t *testing.T) {
	r := routeJSON(ingress.Route{ID: "dockwell.env.www", Domain: `a"},{"evil":"`, Path: "/x", Upstreams: []string{"172.30.0.2:80"}})
	b, err := json.Marshal(r)
	if err != nil {
		t.Fatal(err)
	}
	var back route
	if err := json.Unmarshal(b, &back); err != nil {
		t.Fatal(err)
	}
	if back.Match[0].Host[0] != `a"},{"evil":"` || !slices.Equal(back.Match[0].Path, []string{"/x", "/x/*"}) {
		t.Errorf("read back %+v", back)
	}
	h := back.Handle[0]
	if h.StreamCloseDelay == "" || h.FlushInterval != -1 {
		t.Errorf("the handler is %+v, and a reload would close its WebSockets, or its streams would wait", h)
	}
}

func TestLogsSayWhereACertificateStands(t *testing.T) {
	path := filepath.Join(t.TempDir(), tlsLogName)
	write := func(lines ...string) {
		f, err := os.OpenFile(path, os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0o600)
		if err != nil {
			t.Fatal(err)
		}
		defer f.Close()
		for _, l := range lines {
			if _, err := f.WriteString(l + "\n"); err != nil {
				t.Fatal(err)
			}
		}
	}
	var l logReader
	read := func(domain string) ingress.CertState {
		t.Helper()
		if err := l.read(path); err != nil {
			t.Fatal(err)
		}
		return l.state(domain)
	}
	if st := read("a.example.com"); st.Phase != ingress.CertPending {
		t.Errorf("with no log, %+v", st)
	}
	write(`{"logger":"tls.obtain","msg":"obtaining certificate","identifier":"a.example.com"}`)
	if st := read("a.example.com"); st.Phase != ingress.CertPending {
		t.Errorf("obtaining reads %+v", st)
	}
	write(`{"logger":"tls.obtain","msg":"could not get certificate from issuer","identifier":"a.example.com","issuer":"x","error":"HTTP 400 urn:ietf:params:acme:error:connection - Timeout during connect (likely firewall problem)"}`)
	st := read("a.example.com")
	if st.Phase != ingress.CertFailed || st.Problem != ingress.ProblemConnection || !strings.Contains(st.Detail, "firewall") {
		t.Errorf("a failure reads %+v", st)
	}
	// A reload cancels a try, which starts again: that isn't a failure.
	write(`{"logger":"tls.obtain","msg":"could not get certificate from issuer","identifier":"b.example.com","error":"context canceled"}`)
	if st := read("b.example.com"); st.Phase != ingress.CertPending {
		t.Errorf("a cancelled try reads %+v", st)
	}
	// A line half written, then finished.
	f, err := os.OpenFile(path, os.O_APPEND|os.O_WRONLY, 0o600)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := f.WriteString(`{"logger":"tls.obtain","msg":"certificate obtained successfully","identifier":"a.exam`); err != nil {
		t.Fatal(err)
	}
	if st := read("a.example.com"); st.Phase != ingress.CertFailed {
		t.Errorf("half a line changed it to %+v", st)
	}
	if _, err := f.WriteString("ple.com\"}\n"); err != nil {
		t.Fatal(err)
	}
	_ = f.Close()
	if st := read("a.example.com"); st.Phase != ingress.CertIssued {
		t.Errorf("a whole line reads %+v", st)
	}
	// Rolled: it starts again from the top.
	if err := os.WriteFile(path, []byte(`{"msg":"could not get certificate from issuer","identifier":"c.example.com","error":"HTTP 429 urn:ietf:params:acme:error:rateLimited - too many"}`+"\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if st := read("c.example.com"); st.Phase != ingress.CertFailed || st.Problem != ingress.ProblemRateLimited {
		t.Errorf("after a roll, %+v", st)
	}
}
