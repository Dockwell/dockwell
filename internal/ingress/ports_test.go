// SPDX-License-Identifier: AGPL-3.0-only

package ingress

import (
	"context"
	"fmt"
	"net/netip"
	"os"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"testing"
	"time"

	agentv1 "github.com/dockwell/dockwell/gen/dockwell/agent/v1"
	dockwellv1 "github.com/dockwell/dockwell/gen/dockwell/v1"
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
		{"behind carrier-grade NAT", addrs("198.51.100.1"), nil, addrs("100.64.0.5"), resource.DomainCantTell},
		{"behind NAT, with mihomo's TUN up", addrs("203.0.113.51"), nil, addrs("172.16.0.5", "198.18.0.1"), resource.DomainCantTell},
		{"a shared record is no public address", addrs("100.64.0.9"), nil, addrs("10.0.0.5"), resource.DomainElsewhere},
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

// Aliyun behind 1:1 NAT with mihomo's TUN up on its default, 198.18.0.1: DNS is
// right, and neither address here is one to point it at.
func TestATunsAddressIsNowhereToPointDNS(t *testing.T) {
	d := &DNS{
		Lookup: func(context.Context, string) ([]netip.Addr, error) {
			return []netip.Addr{netip.MustParseAddr("203.0.113.51")}, nil
		},
		Addrs: func() ([]netip.Addr, error) {
			return []netip.Addr{netip.MustParseAddr("172.16.0.5"), netip.MustParseAddr("198.18.0.1")}, nil
		},
	}
	check := d.Check(t.Context(), "app.example.com")
	if check.State != resource.DomainCantTell {
		t.Errorf("state = %d, want it can't tell, as no address here is public", check.State)
	}
	msg := certMessage("app.example.com", CertState{Phase: CertFailed, Problem: ProblemDNS}, check)
	if strings.Contains(msg, "server, 198.18.0.1") {
		t.Errorf("it says to point DNS at the TUN's address: %q", msg)
	}
}

// A TUN device is a program's, by kind, whatever address it took, which the kernel
// tells from a file only such a device has. A PPPoE link is point-to-point too,
// and holds a server's real address, so that isn't how to tell.
func TestATunIsToldByKindNotByItsAddress(t *testing.T) {
	root := t.TempDir()
	for _, name := range []string{"eth0", "ppp0", "wg0", "Meta"} {
		if err := os.MkdirAll(filepath.Join(root, name), 0o755); err != nil {
			t.Fatal(err)
		}
	}
	if err := os.WriteFile(filepath.Join(root, "Meta", "tun_flags"), []byte("0x1001\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	was := sysClassNet
	sysClassNet = root
	t.Cleanup(func() { sysClassNet = was })
	all := []ownInterface{
		{name: "eth0", addrs: []netip.Addr{netip.MustParseAddr("172.16.0.5")}},
		{name: "docker0", addrs: []netip.Addr{netip.MustParseAddr("172.17.0.1")}},
		{name: "Meta", addrs: []netip.Addr{netip.MustParseAddr("11.0.0.1")}},
		{name: "ppp0", addrs: []netip.Addr{netip.MustParseAddr("198.51.100.20")}},
	}
	if got, want := addrWords(serving(all)), []string{"172.16.0.5", "198.51.100.20"}; !slices.Equal(got, want) {
		t.Errorf("a domain could point at %v, want %v", got, want)
	}
	for _, tc := range []struct{ name, outbound, want string }{
		{"the route goes through the TUN", "11.0.0.1", "172.16.0.5"},
		{"the route goes through eth0", "172.16.0.5", "172.16.0.5"},
		{"the route goes through a bridge, which isn't a TUN", "172.17.0.1", "172.17.0.1"},
		{"the address is on no interface", "192.168.9.9", "192.168.9.9"},
		{"not an address", "127.0.0.1", "127.0.0.1"},
	} {
		if got := ownAddress(tc.outbound, all); got != tc.want {
			t.Errorf("%s: %q, want %q", tc.name, got, tc.want)
		}
	}
	if got := ownAddress("11.0.0.1", all[2:3]); got != "11.0.0.1" {
		t.Errorf("with nothing else there, it went with %q", got)
	}
}

// A public address the control plane knows beyond the interfaces reads as this
// server, and only a way in makes a record that points anywhere else point elsewhere.
func TestDNSReadsAPublicAddressKnownBeyondTheInterfaces(t *testing.T) {
	addrs := func(list ...string) []netip.Addr {
		out := make([]netip.Addr, len(list))
		for i, s := range list {
			out[i] = netip.MustParseAddr(s)
		}
		return out
	}
	public := netip.MustParseAddr("203.0.113.50")
	for _, tc := range []struct {
		name  string
		found []netip.Addr
		wayIn bool
		want  resource.DomainState
	}{
		{"the record points at it", addrs("203.0.113.50"), true, resource.DomainHere},
		{"the record points at it, as a way out", addrs("203.0.113.50"), false, resource.DomainHere},
		{"another address, with a way in known", addrs("198.51.100.1"), true, resource.DomainElsewhere},
		{"another address, with only a way out known", addrs("198.51.100.1"), false, resource.DomainCantTell},
		{"no record yet", nil, true, resource.DomainNoRecord},
	} {
		t.Run(tc.name, func(t *testing.T) {
			d := &DNS{
				Lookup: func(context.Context, string) ([]netip.Addr, error) { return tc.found, nil },
				Addrs:  func() ([]netip.Addr, error) { return addrs("10.0.0.25"), nil },
				Public: public, WayIn: tc.wayIn,
			}
			got := d.Check(t.Context(), "app.example.com")
			if got.State != tc.want {
				t.Errorf("State = %d, want %d: %+v", got.State, tc.want, got)
			}
			if got.Public != public.String() || !slices.Contains(got.Here, public.String()) {
				t.Errorf("it doesn't carry the address: %+v", got)
			}
		})
	}
}

// The words say what's known of the way in: none of a server behind NAT as it was,
// and the plain record once something proved it.
func TestTheWordsUseThePublicAddress(t *testing.T) {
	proved := &resource.DomainCheck{State: resource.DomainNoRecord, Public: "203.0.113.50", Here: []string{"10.0.0.25", "203.0.113.50"}}
	router := &resource.DomainCheck{State: resource.DomainNoRecord, Public: "203.0.113.7", Unproven: true, Here: []string{"192.168.1.20", "203.0.113.7"}}
	carrier := &resource.DomainCheck{State: resource.DomainNoRecord, Carrier: true, Here: []string{"100.72.1.2"}}
	pending := CertState{Phase: CertPending}
	for _, tc := range []struct {
		name  string
		check *resource.DomainCheck
		says  string
		never string
	}{
		{"known", proved, "Point an A record at this server, 203.0.113.50.", "10.0.0.25"},
		{"looked up, not on a cloud", router, "Point an A record at 203.0.113.7, which is how this server reaches the internet. If it's behind a router, that record won't reach it without a forwarded port, and a tunnel needs neither.", "192.168.1.20"},
		{"behind carrier NAT", carrier, "so a DNS record can't reach it, and neither can a forwarded port", "Point an A record"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			msg := certMessage("app.example.com", pending, tc.check)
			if !strings.Contains(msg, tc.says) || strings.Contains(msg, tc.never) {
				t.Errorf("it says %q", msg)
			}
		})
	}
	msg := certMessage("app.example.com", CertState{Phase: CertFailed, Problem: ProblemConnection}, proved)
	if !strings.Contains(msg, "behind NAT") {
		t.Errorf("knowing its address doesn't put it on an interface: %q", msg)
	}
}

// As the control plane comes to know the address, a domain waiting on it reads
// again by it, with nobody waiting a minute to see.
func TestAPublicAddressArrivingChangesWhatADomainSays(t *testing.T) {
	s := newSetup(t)
	found := []netip.Addr{netip.MustParseAddr("203.0.113.50")}
	s.DNS = &DNS{
		Lookup: func(context.Context, string) ([]netip.Addr, error) { return found, nil },
		Addrs:  func() ([]netip.Addr, error) { return []netip.Addr{netip.MustParseAddr("10.0.0.25")}, nil },
	}
	s.apply(t, "www", spec("app.example.com", "", "web", 80))
	s.proxy.certs = map[string]CertState{"app.example.com": {Phase: CertPending}}
	s.watchOnce(t.Context())
	if got := s.Domains()[0].GetDns().GetState(); got != dockwellv1.DomainCheckState_DOMAIN_CHECK_STATE_CANT_TELL {
		t.Fatalf("behind NAT with nothing known, it reads %v", got)
	}
	s.SetPublicAddress(&agentv1.PublicAddress{Address: "203.0.113.50", WayIn: true})
	select {
	case <-s.watchWakes():
	default:
		t.Error("nothing woke the watch")
	}
	s.watchOnce(t.Context())
	if got := s.Domains()[0].GetDns(); got.GetState() != dockwellv1.DomainCheckState_DOMAIN_CHECK_STATE_POINTS_HERE || got.GetPublicAddress() != "203.0.113.50" {
		t.Errorf("once known, it reads %v", got)
	}
	if s.proxy.retries != 1 {
		t.Errorf("tried again %d times, with DNS now reading as here", s.proxy.retries)
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
		{"Get \"https://acme.example/dir\": dial tcp: connect: connection refused", ProblemCantReachCA},
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
		{"no record", CertState{Phase: CertPending}, &resource.DomainCheck{State: resource.DomainNoRecord, Here: []string{"203.0.113.9"}}, []string{"no DNS record", "Point an A record", "203.0.113.9"}},
		{"elsewhere", CertState{Phase: CertPending}, here, []string{"points at 198.51.100.1", "203.0.113.9"}},
		{"dns", CertState{Phase: CertFailed, Problem: ProblemDNS}, here, []string{"couldn't find", "Point an A record", "tries again"}},
		{"no record, behind NAT", CertState{Phase: CertPending}, &resource.DomainCheck{State: resource.DomainNoRecord, Here: []string{"10.0.0.25"}}, []string{"no DNS record", "this server's public address", "only sees 10.0.0.25 here, which is private"}},
		{"elsewhere, behind NAT", CertState{Phase: CertPending}, &resource.DomainCheck{State: resource.DomainElsewhere, Found: []string{"10.9.9.9"}, Here: []string{"10.0.0.25"}}, []string{"this server's public address", "only sees 10.0.0.25 here, which is private"}},
		{"a firewall", CertState{Phase: CertFailed, Problem: ProblemConnection}, nil, []string{"port 80 or 443", "firewall", "Open both", "tries again"}},
		{"a firewall, behind NAT", CertState{Phase: CertFailed, Problem: ProblemConnection}, &resource.DomainCheck{State: resource.DomainCantTell, Found: []string{"203.0.113.50"}, Here: []string{"10.0.0.25"}}, []string{"behind NAT", "the cloud provider's", "the machine's own", "ufw or iptables", "Open both ports in each", "Behind a home or office router, forward both to this machine's private address, 10.0.0.25", "tries again"}},
		{"behind a router, with a VPN beside it", CertState{Phase: CertFailed, Problem: ProblemConnection}, &resource.DomainCheck{State: resource.DomainCantTell, Here: []string{"192.168.1.20", "100.72.1.2"}}, []string{"forward both to this machine's private address, 192.168.1.20."}},
		{"a firewall, under carrier-grade NAT", CertState{Phase: CertFailed, Problem: ProblemConnection}, &resource.DomainCheck{State: resource.DomainCantTell, Here: []string{"100.72.1.2"}}, []string{"carrier-grade NAT", "no forwarded port can reach it", "a tunnel is the way"}},
		{"DNS on three addresses", CertState{Phase: CertPending}, &resource.DomainCheck{State: resource.DomainElsewhere, Found: []string{"198.51.100.1", "198.51.100.2", "198.51.100.3"}, Here: []string{"203.0.113.9"}}, []string{"points at 198.51.100.1, 198.51.100.2 and 198.51.100.3, not this server"}},
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

// A control plane runs its agent in one process, so a panic here would take
// the control plane down with it.
func TestAnUnreadableProcNetIsAnErrorNotAPanic(t *testing.T) {
	if os.Geteuid() == 0 {
		t.Skip("root reads a file of any mode")
	}
	dir := t.TempDir()
	if err := os.MkdirAll(filepath.Join(dir, "net"), 0o700); err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(dir, "net", "tcp")
	if err := os.WriteFile(path, nil, 0o000); err != nil {
		t.Fatal(err)
	}
	h := &Holders{Proc: dir}
	_, err := h.sockets(func(netip.Addr) bool { return true })
	if err == nil || !strings.Contains(err.Error(), path) {
		t.Errorf("sockets = %v, want an error naming %s", err, path)
	}
}

// The authority never answered, so nothing is known of the domain, and fixing
// its DNS is the wrong advice.
func TestAResolverFailureHereIsntBlamedOnTheDomain(t *testing.T) {
	for _, text := range []string{
		`making new ACME client: registering account [] with server: provisioning client: performing request: Get "https://acme-v02.api.letsencrypt.org/directory": dial tcp: lookup acme-v02.api.letsencrypt.org on 127.0.0.53:53: no such host`,
		`performing request: Get "https://acme-v02.api.letsencrypt.org/directory": dial tcp 172.65.32.248:443: i/o timeout`,
		`performing request: Get "https://acme-v02.api.letsencrypt.org/directory": tls: failed to verify certificate: x509: certificate signed by unknown authority`,
	} {
		problem, detail, failed := ClassifyFailure(text)
		if !failed || problem != ProblemCantReachCA {
			t.Errorf("ClassifyFailure(%q) = %v, %v, want that this server can't reach the authority", text, problem, failed)
		}
		msg := certMessage("app.example.com", CertState{Phase: CertFailed, Problem: problem, Detail: detail}, nil)
		if !strings.Contains(msg, "this server's own DNS resolver and outbound network") || strings.Contains(msg, "A or AAAA") {
			t.Errorf("for %q it says: %s", text, msg)
		}
	}
	// The authority's own words about the domain still read as DNS, even when
	// they say "no such host".
	problem, _, _ := ClassifyFailure(`urn:ietf:params:acme:error:dns - no valid A records found for app.example.com: no such host`)
	if problem != ProblemDNS {
		t.Errorf("the authority's own DNS failure reads as %v", problem)
	}
}

// The record may be there, and no A record fixes nameservers that don't answer.
func TestAFailedDNSLookupIsntNoRecord(t *testing.T) {
	for _, tc := range []struct{ name, text, want string }{
		{"CAA", `HTTP 400 urn:ietf:params:acme:error:dns - DNS problem: SERVFAIL looking up CAA for example.com - the domain's nameservers may be malfunctioning`, "SERVFAIL looking up CAA"},
		{"A", `HTTP 400 urn:ietf:params:acme:error:dns - DNS problem: SERVFAIL looking up A for app.example.com - the domain's nameservers may be malfunctioning`, "SERVFAIL looking up A"},
		{"timeout", `HTTP 400 urn:ietf:params:acme:error:dns - DNS problem: query timed out looking up CAA for example.com`, "looking up CAA"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			problem, detail, _ := ClassifyFailure(tc.text)
			msg := certMessage("app.example.com", CertState{Phase: CertFailed, Problem: problem, Detail: detail}, nil)
			if !strings.Contains(msg, tc.want) || !strings.Contains(msg, "nameservers answer") || strings.Contains(msg, "A or AAAA") {
				t.Errorf("it says: %s", msg)
			}
		})
	}
	// With no record at all, an A record is the fix.
	problem, detail, _ := ClassifyFailure(`urn:ietf:params:acme:error:dns - DNS problem: NXDOMAIN looking up A for app.example.com`)
	if msg := certMessage("app.example.com", CertState{Phase: CertFailed, Problem: problem, Detail: detail}, nil); !strings.Contains(msg, "A or AAAA") {
		t.Errorf("for NXDOMAIN it says: %s", msg)
	}
}

// A failing renewal says what's wrong in a sentence of its own.
func TestARenewalThatFailsStartsItsSentence(t *testing.T) {
	msg := certMessage("app.example.com", CertState{Phase: CertIssued, Problem: ProblemConnection, NotAfter: time.Date(2027, 2, 3, 0, 0, 0, 0, time.UTC)}, nil)
	if !strings.Contains(msg, "until 3 February 2027. The certificate authority couldn't reach") {
		t.Errorf("it reads %q", msg)
	}
}
