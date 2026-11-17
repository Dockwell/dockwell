// SPDX-License-Identifier: AGPL-3.0-only

package tunnels

import (
	"context"
	"encoding/base64"
	"errors"
	"io/fs"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"
	"testing"
	"testing/fstest"

	"golang.org/x/sys/unix"
	"google.golang.org/protobuf/encoding/prototext"

	dockwellv1 "github.com/dockwell/dockwell/gen/dockwell/v1"
)

const (
	tunnelID = "327d9d41-b009-413d-8494-7eeeefb3ed2a"
	account  = "274faa1d111af716a33922b34ee5381d"
	secret   = "c2VjcmV0LXRoYXQtbXVzdC1uZXZlci1sZWF2ZS10aGUtc2VydmVy"
)

func token(t *testing.T) string {
	t.Helper()
	return base64.StdEncoding.EncodeToString([]byte(`{"a":"` + account + `","t":"` + tunnelID + `","s":"` + secret + `"}`))
}

// proc builds a fake /proc with the agent's own network, and processes added by add.
type proc struct {
	fs fstest.MapFS
}

func newProc() *proc {
	return &proc{fs: fstest.MapFS{"self/ns/net": {Data: []byte("net:[4026531840]"), Mode: fs.ModeSymlink}}}
}

func (p *proc) add(pid, name string, start int, args, environ []string, net, cgroup string) {
	p.fs[pid+"/stat"] = &fstest.MapFile{Data: []byte(pid + " (" + name + ") S 1 1 1 0 -1 4194560 100 0 0 0 1 1 0 0 20 0 9 0 " + itoa(start) + " 1000 100")}
	p.fs[pid+"/cmdline"] = &fstest.MapFile{Data: []byte(strings.Join(args, "\x00") + "\x00")}
	p.fs[pid+"/environ"] = &fstest.MapFile{Data: []byte(strings.Join(environ, "\x00") + "\x00")}
	p.fs[pid+"/ns/net"] = &fstest.MapFile{Data: []byte(net), Mode: fs.ModeSymlink}
	p.fs[pid+"/cgroup"] = &fstest.MapFile{Data: []byte(cgroup)}
}

func itoa(n int) string { return strconv.Itoa(n) }

func TestFind(t *testing.T) {
	p := newProc()
	container := strings.Repeat("ab", 32)
	// On the host, its token on its command line.
	p.add("100", "cloudflared", 500, []string{"/usr/bin/cloudflared", "tunnel", "--no-autoupdate", "run", "--token", token(t)}, nil, "net:[4026531840]", "0::/system.slice/cloudflared.service\n")
	// In a container on a network of its own, its token in its environment beside the
	// container's other secrets, and its metrics on every interface.
	p.add("200", "cloudflared", 600, []string{"cloudflared", "tunnel", "--metrics", "0.0.0.0:2000", "run"},
		[]string{"PATH=/usr/bin", "TUNNEL_TOKEN=" + token(t), "DB_PASSWORD=hunter2-not-kept"}, "net:[4026532999]", "0::/system.slice/docker-"+container+".scope\n")
	p.fs["200/net/route"] = &fstest.MapFile{Data: []byte("Iface\tDestination\tGateway\tFlags\tRefCnt\tUse\tMetric\tMask\tMTU\tWindow\tIRTT\n" +
		"eth0\t00000000\t010012AC\t0003\t0\t0\t0\t00000000\t0\t0\t0\n" +
		"eth0\t000012AC\t00000000\t0001\t0\t0\t0\t0000FFFF\t0\t0\t0\n")}
	// Its token in a file.
	p.add("300", "cloudflared", 700, []string{"cloudflared", "tunnel", "run", "--token-file", "/run/secrets/tunnel"}, nil, "net:[4026531840]", "")
	// Locally managed, named in its config by its credentials file.
	p.add("400", "cloudflared", 800, []string{"cloudflared", "tunnel", "--config", "/etc/cloudflared/config.yml", "run", "home"}, nil, "net:[4026531840]", "")
	// cloudflared that runs no tunnel, and something else.
	p.add("500", "cloudflared", 900, []string{"cloudflared", "access", "tcp", "--hostname", "ssh.example.com"}, nil, "net:[4026531840]", "")
	p.add("501", "cloudflared", 900, []string{"cloudflared", "tunnel", "list"}, nil, "net:[4026531840]", "")
	p.add("600", "nginx", 900, []string{"nginx", "tunnel", "run"}, nil, "net:[4026531840]", "")

	var readFiles []string
	var asked []string
	f := &Finder{
		Proc: p.fs,
		ReadIn: func(pid int, start uint64, name string) ([]byte, error) {
			readFiles = append(readFiles, itoa(pid)+":"+name)
			switch {
			case pid == 300 && name == "/run/secrets/tunnel":
				return []byte(token(t) + "\n"), nil
			case pid == 400 && name == "/etc/cloudflared/config.yml":
				return []byte("tunnel: home\ncredentials-file: /root/.cloudflared/" + tunnelID + ".json\ningress:\n  - service: http_status:404\n"), nil
			}
			return nil, os.ErrNotExist
		},
		ReadyIn: func(_ context.Context, pid int, addr string) (Ready, error) {
			asked = append(asked, itoa(pid)+"@"+addr)
			if pid == 200 && addr != "127.0.0.1:2000" {
				return Ready{}, errors.New("nothing there")
			}
			return Ready{Connections: 4, ConnectorID: "connector-" + itoa(pid)}, nil
		},
	}
	got := f.Find(context.Background())
	if len(got) != 4 {
		t.Fatalf("found %d tunnels, want 4: %v", len(got), got)
	}
	want := []*dockwellv1.CloudflaredTunnel{
		{TunnelId: tunnelID, AccountTag: account, ConnectorId: "connector-100", Reaches: ReachesFromHost, Pid: 100},
		{TunnelId: tunnelID, AccountTag: account, ConnectorId: "connector-200", Reaches: ReachesFromNetwork, NetworkRange: "172.18.0.0/16", ContainerId: container, Pid: 200},
		{TunnelId: tunnelID, AccountTag: account, ConnectorId: "connector-300", Reaches: ReachesFromHost, Pid: 300},
		{TunnelId: tunnelID, ManagedInFile: true, ConnectorId: "connector-400", Reaches: ReachesFromHost, Pid: 400},
	}
	for i := range want {
		if prototext.Format(got[i]) != prototext.Format(want[i]) {
			t.Errorf("tunnel %d:\n got %v\nwant %v", i, got[i], want[i])
		}
	}
	// Neither the token's secret nor anything else of an environment leaves.
	for _, g := range got {
		text := prototext.Format(g)
		if strings.Contains(text, secret) || strings.Contains(text, "hunter2") || strings.Contains(text, token(t)) {
			t.Fatalf("a secret left in %s", text)
		}
	}
	// The credentials file is never opened, and the container's metrics are asked
	// where it said, on loopback inside its network.
	for _, r := range readFiles {
		if strings.HasSuffix(r, ".json") {
			t.Errorf("read %s", r)
		}
	}
	if !strings.Contains(strings.Join(asked, " "), "200@127.0.0.1:2000") {
		t.Errorf("asked %v", asked)
	}
}

// changing is a /proc whose stat for a PID says a new start the second time it's
// read, as a PID taken by another process while the first was read.
type changing struct {
	fstest.MapFS
	reads map[string]int
}

func (c *changing) Open(name string) (fs.File, error) {
	if strings.HasSuffix(name, "/stat") {
		c.reads[name]++
		if c.reads[name] > 1 {
			return fstest.MapFS{name: {Data: []byte("100 (cloudflared) S 1 1 1 0 -1 4194560 100 0 0 0 1 1 0 0 20 0 9 0 999 1000 100")}}.Open(name)
		}
	}
	return c.MapFS.Open(name)
}

func TestFindDropsAProcessThatChangedHands(t *testing.T) {
	p := newProc()
	p.add("100", "cloudflared", 500, []string{"cloudflared", "tunnel", "run", "--token", token(t)}, nil, "net:[4026531840]", "")
	f := &Finder{Proc: &changing{MapFS: p.fs, reads: map[string]int{}}}
	if got := f.Find(context.Background()); len(got) != 0 {
		t.Fatalf("a PID that changed hands as it was read should be dropped: %v", got)
	}
}

func TestDecodeToken(t *testing.T) {
	for _, enc := range []*base64.Encoding{base64.StdEncoding, base64.RawStdEncoding, base64.URLEncoding, base64.RawURLEncoding} {
		tok := enc.EncodeToString([]byte(`{"a":"` + account + `","t":"` + strings.ToUpper(tunnelID) + `","s":"` + secret + `"}`))
		a, id, ok := decodeToken(tok)
		if !ok || a != account || id != tunnelID {
			t.Errorf("%v: %q %q %v", enc, a, id, ok)
		}
	}
	for _, bad := range []string{"", "not base64!", base64.StdEncoding.EncodeToString([]byte(`{"a":"x","t":"not-a-uuid","s":"y"}`)), base64.StdEncoding.EncodeToString([]byte(`{"t":"` + tunnelID + `"}`))} {
		if _, _, ok := decodeToken(bad); ok {
			t.Errorf("%q should not decode", bad)
		}
	}
}

func TestMetricsAddrs(t *testing.T) {
	for in, want := range map[string]string{
		"":               strings.Join(DefaultMetrics, " "),
		"localhost:2000": "127.0.0.1:2000",
		"0.0.0.0:2000":   "127.0.0.1:2000",
		"[::]:2000":      "127.0.0.1:2000",
		"10.0.0.5:2000":  "10.0.0.5:2000",
		"localhost:0":    strings.Join(DefaultMetrics, " "),
	} {
		if got := strings.Join(metricsAddrs(in), " "); got != want {
			t.Errorf("%q: %q, want %q", in, got, want)
		}
	}
}

// root makes a directory that stands for a container's root, and opens it as the
// agent opens /proc/<pid>/root.
func root(t *testing.T) (dir string, fd int) {
	t.Helper()
	dir = t.TempDir()
	fd, err := unix.Open(dir, unix.O_PATH|unix.O_DIRECTORY|unix.O_CLOEXEC, 0)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { unix.Close(fd) })
	return dir, fd
}

func TestReadBeneathStaysInItsRoot(t *testing.T) {
	dir, fd := root(t)
	must := func(err error) {
		t.Helper()
		if err != nil {
			t.Fatal(err)
		}
	}
	must(os.MkdirAll(filepath.Join(dir, "run"), 0o755))
	must(os.WriteFile(filepath.Join(dir, "run", "token"), []byte("the-token\n"), 0o600))
	// An absolute link a container made, to a host file: it resolves inside the
	// root, where there's no such file, never to the host's.
	host := filepath.Join(t.TempDir(), "host-secret")
	must(os.WriteFile(host, []byte("host"), 0o600))
	must(os.Symlink(host, filepath.Join(dir, "run", "out")))
	must(os.Symlink("../../../../../../"+strings.TrimPrefix(host, "/"), filepath.Join(dir, "run", "up")))
	must(os.WriteFile(filepath.Join(dir, "run", "big"), []byte(strings.Repeat("x", MaxFile+1)), 0o600))
	must(syscall.Mkfifo(filepath.Join(dir, "run", "fifo"), 0o600))

	if b, err := readBeneath(fd, "/run/token", false); err != nil || string(b) != "the-token\n" {
		t.Fatalf("a token in its root: %q %v", b, err)
	}
	for _, name := range []string{"/run/out", "/run/up"} {
		if b, err := readBeneath(fd, name, false); err == nil {
			t.Errorf("%s read %q from outside its root", name, b)
		}
	}
	for _, name := range []string{"/run/big", "/run/fifo", "/run"} {
		if _, err := readBeneath(fd, name, false); !errors.Is(err, ErrNotRegular) {
			t.Errorf("%s: %v", name, err)
		}
	}
	if _, err := readBeneath(fd, "../etc/passwd", true); !errors.Is(err, ErrLeaves) {
		t.Errorf("a relative path out: %v", err)
	}
	// Where openat2 is missing, any link is refused.
	if _, err := walkNoLinks(fd, "run/out"); !errors.Is(err, ErrLeaves) {
		t.Errorf("a link without openat2: %v", err)
	}
	if got, err := walkNoLinks(fd, "run/token"); err != nil {
		t.Errorf("a plain path without openat2: %v", err)
	} else {
		unix.Close(got)
	}
}

func TestReadInRootOfThisProcess(t *testing.T) {
	file := filepath.Join(t.TempDir(), "token")
	if err := os.WriteFile(file, []byte("abc"), 0o600); err != nil {
		t.Fatal(err)
	}
	start, err := startOf(os.Getpid())
	if err != nil {
		t.Skip("no /proc here")
	}
	if b, err := ReadInRoot(os.Getpid(), start, file); err != nil || string(b) != "abc" {
		t.Fatalf("%q %v", b, err)
	}
	if _, err := ReadInRoot(os.Getpid(), start+1, file); err == nil {
		t.Fatal("a start that doesn't match should be refused")
	}
}
