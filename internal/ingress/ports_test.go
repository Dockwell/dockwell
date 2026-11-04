// SPDX-License-Identifier: AGPL-3.0-only

package ingress

import (
	"context"
	"fmt"
	"net/netip"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"

	"github.com/dockwell/dockwell/internal/docker"
	"github.com/dockwell/dockwell/internal/docker/dockertest"
	"github.com/dockwell/dockwell/internal/resource"
)

// proc is a /proc to test with.
type proc struct {
	t    *testing.T
	root string
}

func newProc(t *testing.T) *proc {
	t.Helper()
	p := &proc{t: t, root: t.TempDir()}
	if err := os.MkdirAll(filepath.Join(p.root, "net"), 0o700); err != nil {
		t.Fatal(err)
	}
	return p
}

const procHeader = "  sl  local_address rem_address   st tx_queue rx_queue tr tm->when retrnsmt   uid  timeout inode\n"

// listen adds a socket on a port to the kernel's table.
func (p *proc) listen(table string, port int, state, inode string) {
	p.t.Helper()
	path := filepath.Join(p.root, "net", table)
	b, err := os.ReadFile(path)
	if err != nil {
		b = []byte(procHeader)
	}
	line := fmt.Sprintf("   0: 00000000:%04X 00000000:0000 %s 00000000:00000000 00:00000000 00000000     0        0 %s 1 0000000000000000 100 0 0 10 0\n", port, state, inode)
	if err := os.WriteFile(path, append(b, line...), 0o600); err != nil {
		p.t.Fatal(err)
	}
}

// process adds a process that holds sockets.
func (p *proc) process(pid int, comm, cgroup string, inodes ...string) {
	p.t.Helper()
	dir := filepath.Join(p.root, strconv.Itoa(pid))
	if err := os.MkdirAll(filepath.Join(dir, "fd"), 0o700); err != nil {
		p.t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "comm"), []byte(comm+"\n"), 0o600); err != nil {
		p.t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "cgroup"), []byte(cgroup+"\n"), 0o600); err != nil {
		p.t.Fatal(err)
	}
	for i, inode := range inodes {
		if err := os.Symlink("socket:["+inode+"]", filepath.Join(dir, "fd", strconv.Itoa(i+3))); err != nil {
			p.t.Fatal(err)
		}
	}
}

func holdersOn(t *testing.T, e *dockertest.Engine, p *proc) *Holders {
	t.Helper()
	return &Holders{Client: e.Client(), Proc: p.root, PID: 99}
}

func TestFreePortsHaveNoHolders(t *testing.T) {
	p := newProc(t)
	p.listen("tcp", 22, "0A", "1")
	p.listen("tcp", 80, "01", "2") // an established connection, not a listener
	h := holdersOn(t, dockertest.New(t), p)
	got, err := h.Of(t.Context(), DefaultPorts)
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 0 {
		t.Errorf("Of = %+v, want none", got)
	}
}

// The way this project's owner will first meet it: 1Panel's OpenResty on the
// server, as a container on the host network.
func TestAProxyInAContainerIsNamed(t *testing.T) {
	e := dockertest.New(t)
	c := e.AddContainer(docker.Container{Name: "/1Panel-openresty-abc12"})
	p := newProc(t)
	p.listen("tcp", 80, "0A", "11")
	p.listen("tcp6", 443, "0A", "12")
	p.listen("udp", 443, "07", "13")
	p.process(1234, "nginx", "0::/system.slice/docker-"+c.ID+".scope", "11", "12", "13")
	got, err := holdersOn(t, e, p).Of(t.Context(), DefaultPorts)
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 3 {
		t.Fatalf("Of = %+v, want each port", got)
	}
	for _, h := range got {
		if !strings.Contains(h.Words, "nginx") || !strings.Contains(h.Words, "1Panel-openresty-abc12") {
			t.Errorf("%+v doesn't name the process and its container", h)
		}
	}
	err = portsTaken(got, "Caddy")
	for _, want := range []string{"Port 80 is held by nginx, in the container 1Panel-openresty-abc12", "Caddy can't start", "keeps running", "Stop that proxy, or publish the service's port and point that proxy at it"} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("the refusal %q doesn't say %q", err, want)
		}
	}
}

func TestAProcessOnTheServerIsNamed(t *testing.T) {
	p := newProc(t)
	p.listen("tcp", 80, "0A", "21")
	p.process(50, "apache2", "0::/system.slice/apache2.service", "21")
	got, err := holdersOn(t, dockertest.New(t), p).Of(t.Context(), DefaultPorts)
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 1 || got[0].Words != "apache2" || got[0].Port.Number != 80 {
		t.Errorf("Of = %+v", got)
	}
}

// With Docker's userland proxy off a published port has no listening socket,
// yet Docker's rules take its traffic, so a bind would succeed and serve none.
func TestAPublishedPortIsNamedThoughNothingListens(t *testing.T) {
	e := dockertest.New(t)
	e.AddContainer(docker.Container{
		Name: "/shop", State: docker.State{Running: true, Status: "running"},
		HostConfig: docker.HostConfig{PortBindings: map[string][]docker.PortBinding{"8443/tcp": {{HostPort: "443"}}}},
	})
	got, err := holdersOn(t, e, newProc(t)).Of(t.Context(), DefaultPorts)
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 1 || got[0].Port != (Port{Number: 443}) || !strings.Contains(got[0].Words, "the container shop") {
		t.Errorf("Of = %+v", got)
	}
}

// An agent that isn't root can't read other users' processes, so it says which
// port, and how to find out.
func TestWhatTheAgentCantNameItSaysHowToFind(t *testing.T) {
	p := newProc(t)
	p.listen("tcp", 80, "0A", "31")
	got, err := holdersOn(t, dockertest.New(t), p).Of(t.Context(), DefaultPorts)
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 1 || !got[0].Unknown {
		t.Fatalf("Of = %+v", got)
	}
	if msg := portsTaken(got, "Caddy").Error(); !strings.Contains(msg, "ss -ltnp 'sport = :80'") {
		t.Errorf("%q doesn't say how to find what holds it", msg)
	}
}

func TestThisProcessIsThePanel(t *testing.T) {
	p := newProc(t)
	p.listen("tcp6", 443, "0A", "41")
	p.process(99, "dockwell", "0::/system.slice/dockwell.service", "41")
	got, err := holdersOn(t, dockertest.New(t), p).Of(t.Context(), DefaultPorts)
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 1 || !got[0].Self {
		t.Errorf("Of = %+v, want this process", got)
	}
}

func TestAContainerIDIsFoundInEitherCgroupForm(t *testing.T) {
	id := strings.Repeat("ab12", 16)
	for _, line := range []string{
		"0::/system.slice/docker-" + id + ".scope",
		"12:cpu:/docker/" + id,
		"0::/docker/" + id,
	} {
		m := containerID.FindStringSubmatch(line)
		if len(m) != 2 || m[1] != id {
			t.Errorf("%q gave %v", line, m)
		}
	}
	if containerID.MatchString("0::/user.slice/user-1000.slice") {
		t.Error("a user's cgroup read as a container's")
	}
}

func TestDNSSaysWhatItSees(t *testing.T) {
	addrs := func(list ...string) []netip.Addr {
		var out []netip.Addr
		for _, a := range list {
			out = append(out, netip.MustParseAddr(a))
		}
		return out
	}
	for _, tc := range []struct {
		name  string
		found []netip.Addr
		err   error
		here  []netip.Addr
		want  resource.DomainState
	}{
		{"a record here", addrs("203.0.113.9"), nil, addrs("203.0.113.9", "10.0.0.5"), resource.DomainHere},
		{"one of two here", addrs("198.51.100.1", "2001:db8::9"), nil, addrs("2001:db8::9"), resource.DomainHere},
		{"an IPv4-mapped address here", addrs("::ffff:203.0.113.9"), nil, addrs("203.0.113.9"), resource.DomainHere},
		{"no record", nil, nil, addrs("203.0.113.9"), resource.DomainNoRecord},
		{"elsewhere", addrs("198.51.100.1"), nil, addrs("203.0.113.9"), resource.DomainElsewhere},
		{"behind NAT", addrs("198.51.100.1"), nil, addrs("10.0.0.5"), resource.DomainCantTell},
		{"a private record elsewhere", addrs("10.0.0.9"), nil, addrs("10.0.0.5"), resource.DomainElsewhere},
		{"DNS failing", nil, context.DeadlineExceeded, addrs("203.0.113.9"), resource.DomainUnknown},
	} {
		t.Run(tc.name, func(t *testing.T) {
			d := &DNS{
				Lookup: func(context.Context, string) ([]netip.Addr, error) { return tc.found, tc.err },
				Addrs:  func() ([]netip.Addr, error) { return tc.here, nil },
			}
			got := d.Check(t.Context(), "app.example.com")
			if got.State != tc.want {
				t.Errorf("State = %d, want %d: %+v", got.State, tc.want, got)
			}
		})
	}
}

func TestAFailureIsReadByItsProblemType(t *testing.T) {
	for _, tc := range []struct {
		text string
		want Problem
	}{
		{"HTTP 400 urn:ietf:params:acme:error:dns - DNS problem: NXDOMAIN looking up A for app.example.com", ProblemDNS},
		{"HTTP 400 urn:ietf:params:acme:error:connection - Timeout during connect (likely firewall problem)", ProblemConnection},
		{"HTTP 400 urn:ietf:params:acme:error:tls - remote error: tls: internal error", ProblemConnection},
		{"HTTP 403 urn:ietf:params:acme:error:unauthorized - Invalid response from http://app.example.com/.well-known", ProblemWrongServer},
		{"HTTP 429 urn:ietf:params:acme:error:rateLimited - too many certificates (5) already issued, retry after 2026-11-05", ProblemRateLimited},
		{"HTTP 403 urn:ietf:params:acme:error:caa - CAA record for app.example.com prevents issuance", ProblemCAA},
		{"HTTP 400 urn:ietf:params:acme:error:rejectedIdentifier - Invalid identifiers requested", ProblemRefused},
		{"HTTP 400 urn:ietf:params:acme:error:malformed - Could not resolve URL \"app.example.com\"", ProblemDNS},
		{"HTTP 500 urn:ietf:params:acme:error:serverInternal - the CA is having a bad day", ProblemOther},
		{"Get \"https://acme.example/dir\": dial tcp: connect: connection refused", ProblemOther},
	} {
		got, detail, failed := ClassifyFailure(tc.text)
		if !failed || got != tc.want {
			t.Errorf("ClassifyFailure(%q) = %d, %v, want %d", tc.text, got, failed, tc.want)
		}
		if detail == "" {
			t.Errorf("ClassifyFailure(%q) kept no detail", tc.text)
		}
	}
	if _, _, failed := ClassifyFailure("context canceled"); failed {
		t.Error("a try a reload cancelled reads as a failure")
	}
}

// A person must never be left wondering: each state says what is happening,
// and a failure says what to do.
func TestEachCertificateStateSaysWhatToDo(t *testing.T) {
	here := &resource.DomainCheck{State: resource.DomainElsewhere, Found: []string{"198.51.100.1"}, Here: []string{"203.0.113.9"}}
	for _, tc := range []struct {
		name  string
		st    CertState
		check *resource.DomainCheck
		says  []string
	}{
		{"on its way", CertState{Phase: CertPending}, nil, []string{"Getting a certificate", "over HTTPS"}},
		{"no record", CertState{Phase: CertPending}, &resource.DomainCheck{State: resource.DomainNoRecord, Here: []string{"203.0.113.9"}}, []string{"no DNS record", "Point an A or AAAA record", "203.0.113.9"}},
		{"elsewhere", CertState{Phase: CertPending}, here, []string{"points at 198.51.100.1", "203.0.113.9"}},
		{"dns", CertState{Phase: CertFailed, Problem: ProblemDNS}, here, []string{"couldn't find", "Point an A or AAAA record", "tries again"}},
		{"a firewall", CertState{Phase: CertFailed, Problem: ProblemConnection}, nil, []string{"port 80 or 443", "firewall", "Open both", "tries again"}},
		{"another server", CertState{Phase: CertFailed, Problem: ProblemWrongServer}, here, []string{"something other than this server", "CDN"}},
		{"a rate limit", CertState{Phase: CertFailed, Problem: ProblemRateLimited, Detail: "retry after 2026-11-05"}, nil, []string{"rate limit", "retry after 2026-11-05", "Wait"}},
		{"CAA", CertState{Phase: CertFailed, Problem: ProblemCAA}, nil, []string{"CAA record", "letsencrypt.org"}},
		{"refused", CertState{Phase: CertFailed, Problem: ProblemRefused, Detail: "policy forbids it"}, nil, []string{"policy forbids it", "another name"}},
		{"anything else", CertState{Phase: CertFailed, Problem: ProblemOther, Detail: "bad day"}, nil, []string{"bad day", "docker logs dockwell-ingress"}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			msg := certMessage("app.example.com", tc.st, tc.check)
			for _, want := range tc.says {
				if !strings.Contains(msg, want) {
					t.Errorf("%q doesn't say %q", msg, want)
				}
			}
		})
	}
	if msg := certMessage("app.example.com", CertState{Phase: CertIssued}, nil); msg != "" {
		t.Errorf("an issued certificate says %q", msg)
	}
}
