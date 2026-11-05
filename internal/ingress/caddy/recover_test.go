// SPDX-License-Identifier: AGPL-3.0-only

package caddy

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"syscall"
	"testing"
	"time"

	"github.com/dockwell/dockwell/internal/ingress"
)

// A compromised Caddy owns /config, so root must not write anything through a
// path there: the bootstrap it would have followed a link for is in a directory
// Caddy can't reach.
func TestWritingTheBootstrapDoesntFollowALinkOfCaddys(t *testing.T) {
	dir := shortDir(t)
	if err := EnsureDirs(dir); err != nil {
		t.Fatal(err)
	}
	victim := filepath.Join(t.TempDir(), "key")
	if err := os.WriteFile(victim, []byte("the control plane's key"), 0o600); err != nil {
		t.Fatal(err)
	}
	for _, name := range []string{bootstrapName, acmeRootsName} {
		if err := os.Symlink(victim, filepath.Join(dir, "config", name)); err != nil {
			t.Fatal(err)
		}
	}
	roots := filepath.Join(dir, "roots.pem")
	if err := os.WriteFile(roots, []byte("roots"), 0o600); err != nil {
		t.Fatal(err)
	}
	c := &Caddy{Dir: dir, Runtime: &process{dir: dir}, ACMERoots: roots}
	if err := c.prepare(nil); err != nil {
		t.Fatal(err)
	}
	if b, _ := os.ReadFile(victim); string(b) != "the control plane's key" {
		t.Errorf("the agent wrote through Caddy's link, and the file it leads to now reads %.40q. As root it would give that file to Caddy's user too", b)
	}
	if b, err := os.ReadFile(filepath.Join(dir, bootDirName, bootstrapName)); err != nil || !json.Valid(b) {
		t.Errorf("the bootstrap is %q, %v", b, err)
	}
	info, err := os.Stat(filepath.Join(dir, bootDirName, bootstrapName))
	if err != nil {
		t.Fatal(err)
	}
	if info.Mode().Perm() != 0o644 {
		t.Errorf("the bootstrap has mode %v, and Caddy's user needs to read it, and nobody to write it", info.Mode().Perm())
	}
	if boot, _ := os.Stat(filepath.Join(dir, bootDirName)); boot.Mode().Perm() != 0o755 {
		t.Errorf("the boot directory has mode %v", boot.Mode().Perm())
	}
}

// What the agent writes into Caddy's directories replaces what stands there, and
// never follows it.
func TestWriteInReplacesALinkRatherThanFollowingIt(t *testing.T) {
	dir := shortDir(t)
	victim := filepath.Join(t.TempDir(), "shadow")
	if err := os.WriteFile(victim, []byte("root:x"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.Mkdir(filepath.Join(dir, "caddy"), 0o700); err != nil {
		t.Fatal(err)
	}
	target := filepath.Join(dir, "caddy", "autosave.json")
	if err := os.Symlink(victim, target); err != nil {
		t.Fatal(err)
	}
	if err := writeIn(dir, "caddy/autosave.json", []byte("{}"), 0o600, agentOwns); err != nil {
		t.Fatal(err)
	}
	if b, _ := os.ReadFile(victim); string(b) != "root:x" {
		t.Errorf("it wrote through the link: %q", b)
	}
	if info, err := os.Lstat(target); err != nil || !info.Mode().IsRegular() {
		t.Errorf("what stands there now is %v, %v", info, err)
	}
	// A directory that leads out of it is refused.
	if err := os.Remove(target); err != nil {
		t.Fatal(err)
	}
	if err := os.Remove(filepath.Join(dir, "caddy")); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(filepath.Dir(victim), filepath.Join(dir, "caddy")); err != nil {
		t.Fatal(err)
	}
	if err := writeIn(dir, "caddy/shadow", []byte("{}"), 0o600, agentOwns); err == nil {
		t.Error("it wrote through a directory that leads out of Caddy's")
	}
	if b, _ := os.ReadFile(victim); string(b) != "root:x" {
		t.Errorf("it wrote through the directory's link: %q", b)
	}
}

// Each of these would hang the agent or fill its memory, and on a control server
// the control plane is in the same process.
func TestNothingCaddyLeavesCanBeReadAsAFile(t *testing.T) {
	dir := shortDir(t)
	outside := filepath.Join(t.TempDir(), "secret")
	if err := os.WriteFile(outside, []byte("secret"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "plain"), []byte("ok"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := syscall.Mkfifo(filepath.Join(dir, "fifo"), 0o600); err != nil {
		t.Fatal(err)
	}
	for name, to := range map[string]string{"zero": "/dev/zero", "out": outside, "inside": "plain"} {
		if err := os.Symlink(to, filepath.Join(dir, name)); err != nil {
			t.Fatal(err)
		}
	}
	if err := os.Mkdir(filepath.Join(dir, "sub"), 0o700); err != nil {
		t.Fatal(err)
	}
	if b, err := readIn(dir, "plain", 10); err != nil || string(b) != "ok" {
		t.Errorf("a plain file read %q, %v", b, err)
	}
	if _, err := readIn(dir, "plain", 1); err == nil {
		t.Error("a file over its bound read")
	}
	for _, name := range []string{"fifo", "zero", "out", "inside", "sub"} {
		done := make(chan error, 1)
		go func() {
			_, err := readIn(dir, name, 1<<20)
			done <- err
		}()
		select {
		case err := <-done:
			if err == nil {
				t.Errorf("%s read as a file", name)
			}
		case <-time.After(5 * time.Second):
			t.Fatalf("reading %s hangs", name)
		}
	}
}

func TestTheTLSLogCantHangOrFillTheAgent(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, tlsLogName)
	if err := os.Symlink("/dev/zero", path); err != nil {
		t.Fatal(err)
	}
	var l logReader
	if err := l.read(path); err == nil {
		t.Error("a log that is a link to /dev/zero read")
	}
	if err := os.Remove(path); err != nil {
		t.Fatal(err)
	}
	if err := syscall.Mkfifo(path, 0o600); err != nil {
		t.Fatal(err)
	}
	done := make(chan error, 1)
	go func() { done <- l.read(path) }()
	select {
	case err := <-done:
		if err == nil {
			t.Error("a log that is a FIFO read")
		}
	case <-time.After(5 * time.Second):
		t.Fatal("reading a log that is a FIFO hangs")
	}
	if err := os.Remove(path); err != nil {
		t.Fatal(err)
	}

	// A log with no end to its line is read a bound at a time, and gets past it.
	var b bytes.Buffer
	b.Write(bytes.Repeat([]byte("x"), logReadLimit+10))
	b.WriteString("\n")
	b.WriteString(`{"msg":"certificate obtained successfully","identifier":"a.example.com"}` + "\n")
	if err := os.WriteFile(path, b.Bytes(), 0o600); err != nil {
		t.Fatal(err)
	}
	for range 3 {
		if err := l.read(path); err != nil {
			t.Fatal(err)
		}
	}
	if st := l.state("a.example.com"); st.Phase != ingress.CertIssued {
		t.Errorf("the line after a line too long reads %+v", st)
	}

	// Names it never heard of can't fill its memory.
	b.Reset()
	for i := range logNames * 2 {
		fmt.Fprintf(&b, `{"msg":"certificate obtained successfully","identifier":"n-%d.example.com"}`+"\n", i)
	}
	if err := os.WriteFile(path, b.Bytes(), 0o600); err != nil {
		t.Fatal(err)
	}
	var fresh logReader
	if err := fresh.read(path); err != nil {
		t.Fatal(err)
	}
	if n := len(fresh.last); n > logNames {
		t.Errorf("it remembers %d names, over its bound of %d", n, logNames)
	}
}

// A certificate that isn't a regular file reads as none.
func TestALinkedCertificateReadsAsNone(t *testing.T) {
	dir := shortDir(t)
	if err := EnsureDirs(dir); err != nil {
		t.Fatal(err)
	}
	d := filepath.Join(dir, "data", "caddy", "certificates", "acme.example-directory", "a.example.com")
	if err := os.MkdirAll(d, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink("/dev/zero", filepath.Join(d, "a.example.com.crt")); err != nil {
		t.Fatal(err)
	}
	c := &Caddy{Dir: dir, Runtime: &process{dir: dir}}
	if _, ok := c.issued("a.example.com"); ok {
		t.Error("a link to /dev/zero read as a certificate")
	}
	if err := os.Remove(filepath.Join(d, "a.example.com.crt")); err != nil {
		t.Fatal(err)
	}
	if err := syscall.Mkfifo(filepath.Join(d, "a.example.com.crt"), 0o600); err != nil {
		t.Fatal(err)
	}
	done := make(chan struct{})
	go func() {
		c.issued("a.example.com")
		close(done)
	}()
	select {
	case <-done:
	case <-time.After(5 * time.Second):
		t.Fatal("reading a certificate that is a FIFO hangs")
	}
}

// The panel's socket is in a directory Caddy owns, so by the time it is given to
// Caddy's user, what stands at its name may be a link to anything.
func TestOwningTheSocketDoesntFollowALink(t *testing.T) {
	dir := shortDir(t)
	victim := filepath.Join(t.TempDir(), "key")
	if err := os.WriteFile(victim, []byte("key"), 0o640); err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(dir, "panel.sock")
	if err := os.Symlink(victim, path); err != nil {
		t.Fatal(err)
	}
	if err := OwnSocket(path); err == nil {
		t.Error("it changed a mode through a link that leads out of the directory")
	}
	if info, _ := os.Stat(victim); info.Mode().Perm() != 0o640 {
		t.Errorf("the file the link leads to now has mode %v", info.Mode().Perm())
	}
}

// truncated is what a full disk leaves, and half a file what a power cut can.
func damage(t *testing.T, c *realCaddy, with []byte) {
	t.Helper()
	if err := os.WriteFile(c.autosaveFile(), with, 0o600); err != nil {
		t.Fatal(err)
	}
}

// answersOnTCP says what Caddy's default admin address answers.
func answersOnTCP(t *testing.T) string {
	t.Helper()
	client := http.Client{Timeout: time.Second}
	res, err := client.Get("http://127.0.0.1:2019/config/")
	if err != nil {
		return ""
	}
	defer res.Body.Close()
	b, _ := io.ReadAll(res.Body)
	return string(b)
}

// With nothing in its last config, Caddy starts with no config, and its admin API
// where its environment says: never on TCP, where any process could load one.
func TestAnEmptyLastConfigNeverOpensAdminOnTCPOnRealCaddy(t *testing.T) {
	bin := needCaddy(t)
	dir := shortDir(t)
	proc := &process{bin: bin, dir: dir}
	c := &Caddy{Dir: dir, Runtime: proc, HTTPPort: freePort(t), HTTPSPort: freePort(t), issuerForTest: map[string]any{"module": "internal"}}
	if err := EnsureDirs(dir); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(filepath.Dir(c.autosaveFile()), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(c.autosaveFile(), nil, 0o600); err != nil {
		t.Fatal(err)
	}
	if err := c.prepare(nil); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { proc.stop(syscall.SIGKILL) })
	// As Docker does after a reboot: the same command, and no agent in it.
	if err := proc.Run(t.Context(), filepath.Join(proc.Paths().Boot, bootstrapName)); err != nil {
		t.Fatal(err)
	}
	socket := filepath.Join(dir, "run", adminSocketName)
	for range 50 {
		if _, err := os.Stat(socket); err == nil {
			break
		}
		time.Sleep(100 * time.Millisecond)
	}
	info, err := os.Stat(socket)
	if err != nil {
		out, _ := os.ReadFile(filepath.Join(dir, "caddy.out"))
		t.Fatalf("its socket isn't there: %v\n%s", err, out)
	}
	if info.Mode().Perm() != 0o600 {
		t.Errorf("its socket has mode %v, want 0600", info.Mode().Perm())
	}
	if got := answersOnTCP(t); got != "" {
		t.Errorf("Caddy's admin API answers on TCP with %q, so any process on the server can load a config", got)
	}
}

// The whole path: routes on Caddy, a power cut that empties its last config, and
// Docker starting it again with nothing, with no agent there. The agent comes
// back and loads its own copy into the Caddy that is running.
func TestAnEmptyLastConfigComesBackOnRealCaddy(t *testing.T) {
	c := startCaddy(t)
	web := backend(t, "web")
	put(t, c, aRoute("dockwell.env.www", "app.example.net", "", web))
	_ = c.waitServing(t, "app.example.net", "/who").Body.Close()

	c.proc.stop(syscall.SIGKILL)
	damage(t, c, nil)
	if err := c.proc.Run(t.Context(), filepath.Join(c.proc.Paths().Boot, bootstrapName)); err != nil {
		t.Fatal(err)
	}
	waitAdmin(t, c)
	if routes, err := c.Routes(t.Context()); err != nil || len(routes) != 0 {
		t.Fatalf("started from an empty file, Routes = %+v, %v, want none", routes, err)
	}
	recovered, err := c.Recover(t.Context())
	if err != nil || !recovered {
		t.Fatalf("Recover = %v, %v", recovered, err)
	}
	if got := c.whoAt(t, "app.example.net", "/who"); got["name"] != "web" {
		t.Errorf("after recovering, it serves %v", got)
	}
	if got := answersOnTCP(t); got != "" {
		t.Errorf("after recovering, the admin API answers on TCP: %q", got)
	}
	// It wrote its file again, as loading a config does.
	b, missing, err := c.lastConfig()
	if err != nil || missing || damaged(b) {
		t.Errorf("the last config is %.40q, missing %v, %v", b, missing, err)
	}
	if again, err := c.Recover(t.Context()); err != nil || again {
		t.Errorf("Recover again = %v, %v, want nothing to do", again, err)
	}
}

// Half a file makes Caddy exit on every start, and Docker start it again for
// ever. With Caddy not answering, the agent puts the file right, so that the next
// start works, whether Docker's or its own.
func TestACutShortLastConfigComesBackOnRealCaddy(t *testing.T) {
	for name, cut := range map[string]func([]byte) []byte{
		"half a file":    func(b []byte) []byte { return b[:len(b)/2] },
		"zeros":          func(b []byte) []byte { return make([]byte, len(b)) },
		"a null":         func([]byte) []byte { return []byte("null\n") },
		"no file at all": func([]byte) []byte { return nil },
	} {
		t.Run(name, func(t *testing.T) {
			c := startCaddy(t)
			web := backend(t, "web")
			put(t, c, aRoute("dockwell.env.www", "app.example.net", "", web))
			_ = c.waitServing(t, "app.example.net", "/who").Body.Close()
			c.proc.stop(syscall.SIGKILL)
			whole, err := os.ReadFile(c.autosaveFile())
			if err != nil {
				t.Fatal(err)
			}
			if name == "no file at all" {
				if err := os.Remove(c.autosaveFile()); err != nil {
					t.Fatal(err)
				}
			} else {
				damage(t, c, cut(whole))
			}
			recovered, err := c.Recover(t.Context())
			if err != nil || !recovered {
				t.Fatalf("Recover = %v, %v", recovered, err)
			}
			if routes, err := c.Routes(t.Context()); err != nil || len(routes) != 1 {
				t.Errorf("with Caddy down, Routes = %+v, %v", routes, err)
			}
			if err := c.proc.Run(t.Context(), filepath.Join(c.proc.Paths().Boot, bootstrapName)); err != nil {
				t.Fatal(err)
			}
			if got := c.whoAt(t, "app.example.net", "/who"); got["name"] != "web" {
				t.Errorf("it serves %v", got)
			}
		})
	}
}

// What Start does when the agent starts Caddy itself.
func TestStartPutsADamagedLastConfigRightOnRealCaddy(t *testing.T) {
	c := startCaddy(t)
	put(t, c, aRoute("dockwell.env.www", "app.example.net", "", backend(t, "web")))
	_ = c.waitServing(t, "app.example.net", "/who").Body.Close()
	c.proc.stop(syscall.SIGKILL)
	whole, err := os.ReadFile(c.autosaveFile())
	if err != nil {
		t.Fatal(err)
	}
	damage(t, c, whole[:len(whole)/2])
	if err := c.Start(t.Context(), nil); err != nil {
		t.Fatal(err)
	}
	if got := c.whoAt(t, "app.example.net", "/who"); got["name"] != "web" {
		t.Errorf("it serves %v", got)
	}
}

// With Caddy running, what it runs is the truth, and only its next start is at
// risk, so that is what goes back in the file.
func TestADamagedFileBehindARunningCaddyIsWrittenAgainOnRealCaddy(t *testing.T) {
	c := startCaddy(t)
	put(t, c, aRoute("dockwell.env.www", "app.example.net", "", backend(t, "web")))
	_ = c.waitServing(t, "app.example.net", "/who").Body.Close()
	damage(t, c, nil)
	recovered, err := c.Recover(t.Context())
	if err != nil || !recovered {
		t.Fatalf("Recover = %v, %v", recovered, err)
	}
	b, missing, err := c.lastConfig()
	if err != nil || missing || !strings.Contains(string(b), "app.example.net") {
		t.Errorf("the last config is %.60q, missing %v, %v", b, missing, err)
	}
	if got := c.whoAt(t, "app.example.net", "/who"); got["name"] != "web" {
		t.Errorf("it serves %v", got)
	}
}

// With no copy either, as if it had been deleted, Caddy still starts, from its
// bootstrap, rather than exiting for ever.
func TestADamagedLastConfigWithNoCopyStartsFromTheBootstrapOnRealCaddy(t *testing.T) {
	c := startCaddy(t)
	put(t, c, aRoute("dockwell.env.www", "app.example.net", "", backend(t, "web")))
	_ = c.waitServing(t, "app.example.net", "/who").Body.Close()
	c.proc.stop(syscall.SIGKILL)
	whole, err := os.ReadFile(c.autosaveFile())
	if err != nil {
		t.Fatal(err)
	}
	damage(t, c, whole[:len(whole)/2])
	if err := c.forgetGood(); err != nil {
		t.Fatal(err)
	}
	recovered, err := c.Recover(t.Context())
	if err != nil || !recovered {
		t.Fatalf("Recover = %v, %v", recovered, err)
	}
	if _, err := os.Stat(c.autosaveFile() + ".damaged"); err != nil {
		t.Errorf("the damaged file wasn't kept for someone to read: %v", err)
	}
	if err := c.Start(t.Context(), nil); err != nil {
		out, _ := os.ReadFile(filepath.Join(c.Dir, "caddy.out"))
		t.Fatalf("%v\n%s", err, out)
	}
	if routes, err := c.Routes(t.Context()); err != nil || len(routes) != 0 {
		t.Errorf("Routes = %+v, %v, want none", routes, err)
	}
}

// A copy that went missing comes back on the next look, not the next change, as
// nothing else would say so until the day it's needed.
func TestAMissingCopyIsWrittenAgainOnRealCaddy(t *testing.T) {
	c := startCaddy(t)
	put(t, c, aRoute("dockwell.env.www", "app.example.net", "", backend(t, "web")))
	path := filepath.Join(c.Dir, goodFile)
	if err := os.Remove(path); err != nil {
		t.Fatal(err)
	}
	if _, err := c.Recover(t.Context()); err != nil {
		t.Fatal(err)
	}
	if b := c.readGood(); b == nil || !strings.Contains(string(b), "app.example.net") {
		t.Errorf("the copy is %.60q", b)
	}
}

// A copy is never what puts the admin API back on TCP.
func TestACopyMustListenOnTheSocket(t *testing.T) {
	dir := shortDir(t)
	c := &Caddy{Dir: dir, Runtime: &process{dir: dir}}
	if err := EnsureDirs(dir); err != nil {
		t.Fatal(err)
	}
	good := `{"admin":{"listen":"` + adminListen(c.Runtime.Paths()) + `"},"apps":{"http":{}}}`
	for config, want := range map[string]bool{
		good: true,
		`{"admin":{"listen":"localhost:2019"},"apps":{"http":{}}}`: false,
		`{"apps":{"http":{}}}`: false,
		`{"admin":{"listen":"` + adminListen(c.Runtime.Paths()) + `"}}`: false,
		`null`: false,
		``:     false,
	} {
		if got := c.usable([]byte(config)); got != want {
			t.Errorf("usable(%s) = %v, want %v", config, got, want)
		}
	}
	if err := c.keepGood([]byte(`{"admin":{"listen":"localhost:2019"},"apps":{"http":{}}}`)); err != nil {
		t.Fatal(err)
	}
	if b := c.readGood(); b != nil {
		t.Errorf("kept %s", b)
	}
	// One written there by something else isn't used either.
	if err := os.WriteFile(filepath.Join(dir, goodFile), []byte(`{"admin":{"listen":"localhost:2019"},"apps":{"http":{}}}`), 0o600); err != nil {
		t.Fatal(err)
	}
	if b := c.readGood(); b != nil {
		t.Errorf("read %s", b)
	}
}

// A route on a stale control plane's ID, for the same name, stays first and goes
// on serving, while the new route's plan applies and verifies.
func TestAStaleRouteDoesntShadowANewOneOnRealCaddy(t *testing.T) {
	c := startCaddy(t)
	put(t, c, aRoute("dockwell.env_old.www", "app.example.net", "", backend(t, "old")))
	put(t, c, aRoute("dockwell.env_old.api", "app.example.net", "/api", backend(t, "old-api")))
	put(t, c, aRoute("dockwell.env_old.keep", "other.example.net", "", backend(t, "old-other")))
	put(t, c, aRoute("dockwell.env_new.www", "app.example.net", "", backend(t, "new")))
	if got := c.whoAt(t, "app.example.net", "/who")["name"]; got != "new" {
		t.Errorf("app.example.net serves %q, though the plan for the new route applied", got)
	}
	// The other environment's path under it can't stay either: a domain lives in
	// one environment, and it would go on taking /api.
	if got := c.whoAt(t, "app.example.net", "/api/who")["name"]; got != "new" {
		t.Errorf("app.example.net/api serves %q", got)
	}
	// Another name, which nothing has claimed, is left alone.
	if got := c.whoAt(t, "other.example.net", "/who")["name"]; got != "old-other" {
		t.Errorf("other.example.net serves %q", got)
	}
	// A path in the same environment isn't stale.
	put(t, c, aRoute("dockwell.env_new.api", "app.example.net", "/api", backend(t, "new-api")))
	put(t, c, aRoute("dockwell.env_new.www", "app.example.net", "", backend(t, "new-again")))
	if got := c.whoAt(t, "app.example.net", "/api/who")["name"]; got != "new-api" {
		t.Errorf("app.example.net/api serves %q after its neighbour changed", got)
	}
}

// With Caddy down the route comes out of what it serves when it starts again.
func TestARouteComesOutOfTheLastConfigWithCaddyDownOnRealCaddy(t *testing.T) {
	c := startCaddy(t)
	web := backend(t, "web")
	put(t, c, aRoute("dockwell.env.a", "a.example.net", "", web))
	put(t, c, aRoute("dockwell.env.b", "b.example.net", "", web))
	_ = c.waitServing(t, "b.example.net", "/who").Body.Close()
	c.proc.stop(syscall.SIGKILL)

	if err := c.DeleteRoute(t.Context(), "dockwell.env.a"); err != nil {
		t.Fatal(err)
	}
	routes, err := c.Routes(t.Context())
	if err != nil || len(routes) != 1 || routes[0].ID != "dockwell.env.b" {
		t.Fatalf("with Caddy down, Routes = %+v, %v", routes, err)
	}
	// What the agent would put back, too.
	if b := c.readGood(); b == nil || strings.Contains(string(b), "a.example.net") {
		t.Errorf("the copy is %s", b)
	}
	if err := c.proc.Run(t.Context(), filepath.Join(c.proc.Paths().Boot, bootstrapName)); err != nil {
		t.Fatal(err)
	}
	if got := c.whoAt(t, "b.example.net", "/who")["name"]; got != "web" {
		t.Errorf("b.example.net serves %q", got)
	}
	if res, err := c.client("a.example.net").Get("https://a.example.net/who"); err == nil {
		_ = res.Body.Close()
		t.Error("the route removed with Caddy down is back")
	}
}

// docker pause does this: the socket accepts a connection, and nothing answers.
func TestAHungAdminAPIIsBoundedOnRealCaddy(t *testing.T) {
	c := startCaddy(t)
	put(t, c, aRoute("dockwell.env.www", "app.example.net", "", backend(t, "web")))
	_ = c.waitServing(t, "app.example.net", "/who").Body.Close()
	c.AdminWait = 300 * time.Millisecond
	cont := func() { _ = c.proc.cmd.Process.Signal(syscall.SIGCONT) }
	if err := c.proc.cmd.Process.Signal(syscall.SIGSTOP); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(cont)

	start := time.Now()
	routes, err := c.Routes(t.Context())
	if took := time.Since(start); took > 3*time.Second {
		t.Errorf("reading routes from a hung Caddy took %v", took)
	}
	if err != nil || len(routes) != 1 {
		t.Errorf("Routes = %+v, %v, want its last config", routes, err)
	}
	if err := c.PutRoute(t.Context(), aRoute("dockwell.env.b", "b.example.net", "", "127.0.0.1:1")); err == nil {
		t.Error("a change went into a hung Caddy")
	}
	err = c.DeleteRoute(t.Context(), "dockwell.env.www")
	if err == nil || !strings.Contains(err.Error(), "docker unpause") {
		t.Errorf("DeleteRoute = %v, want it to say what to do", err)
	}
	if routes, err := c.Routes(t.Context()); err != nil || len(routes) != 1 {
		t.Errorf("after a removal it refused, Routes = %+v, %v", routes, err)
	}
	cont()
	if got := c.whoAt(t, "app.example.net", "/who")["name"]; got != "web" {
		t.Errorf("after waking, it serves %q", got)
	}
}

func waitAdmin(t *testing.T, c *realCaddy) {
	t.Helper()
	ctx, cancel := context.WithTimeout(t.Context(), 20*time.Second)
	defer cancel()
	for ctx.Err() == nil {
		d := net.Dialer{}
		conn, err := d.DialContext(ctx, "unix", filepath.Join(c.Dir, "run", adminSocketName))
		if err == nil {
			_ = conn.Close()
			return
		}
		time.Sleep(50 * time.Millisecond)
	}
	out, _ := os.ReadFile(filepath.Join(c.Dir, "caddy.out"))
	t.Fatalf("caddy's admin API never answered\n%s", out)
}

// With another network on the subnet its routes dial, every route answers 503,
// ahead of what it dials, and the panel stays up: it isn't a route. The routes
// themselves don't change, so they don't read as drift, and come straight back.
func TestClosedRoutesAnswer503AndThePanelDoesntOnRealCaddy(t *testing.T) {
	bin := needCaddy(t)
	dir := shortDir(t)
	if err := EnsureDirs(dir); err != nil {
		t.Fatal(err)
	}
	fakePanel(t, dir)
	proc := &process{bin: bin, dir: dir}
	caddy := &Caddy{Dir: dir, Runtime: proc, HTTPPort: freePort(t), HTTPSPort: freePort(t), issuerForTest: map[string]any{"module": "internal"}}
	t.Cleanup(func() { proc.stop(syscall.SIGKILL) })
	site := &ingress.PanelSite{Domain: "panel.example.net"}
	if err := caddy.Start(t.Context(), site); err != nil {
		t.Fatal(err)
	}
	c := &realCaddy{Caddy: caddy, proc: proc, http: caddy.HTTPPort, tls: caddy.HTTPSPort}
	put(t, c, aRoute("dockwell.env.www", "app.example.net", "", backend(t, "web")))
	_ = c.waitServing(t, "app.example.net", "/who").Body.Close()
	_ = c.waitServing(t, "panel.example.net", "/who").Body.Close()

	if err := c.SetClosed(t.Context(), true); err != nil {
		t.Fatal(err)
	}
	if res := c.waitServing(t, "app.example.net", "/who"); res.StatusCode != http.StatusServiceUnavailable {
		t.Errorf("a closed route answers %d", res.StatusCode)
	} else {
		b, _ := io.ReadAll(res.Body)
		_ = res.Body.Close()
		if strings.Contains(string(b), "web") {
			t.Errorf("a closed route reached its service: %s", b)
		}
	}
	if got := c.whoAt(t, "panel.example.net", "/who"); got["proto"] == "" {
		t.Errorf("the panel's site answers %v with the routes closed", got)
	}
	routes, err := c.Routes(t.Context())
	if err != nil || len(routes) != 1 || routes[0].ID != "dockwell.env.www" || routes[0].Edited {
		t.Errorf("with the routes closed, Routes = %+v, %v", routes, err)
	}
	// It survives a restart with no agent, as it must: nothing else would stop
	// Caddy dialling whatever has those addresses.
	proc.stop(syscall.SIGKILL)
	if err := proc.Run(t.Context(), filepath.Join(proc.Paths().Boot, bootstrapName)); err != nil {
		t.Fatal(err)
	}
	if res := c.waitServing(t, "app.example.net", "/who"); res.StatusCode != http.StatusServiceUnavailable {
		t.Errorf("after a restart, a closed route answers %d", res.StatusCode)
	} else {
		_ = res.Body.Close()
	}
	if err := c.SetClosed(t.Context(), false); err != nil {
		t.Fatal(err)
	}
	if got := c.whoAt(t, "app.example.net", "/who")["name"]; got != "web" {
		t.Errorf("once opened, it serves %q", got)
	}

	// With Caddy down it goes in the last config, and comes up that way.
	proc.stop(syscall.SIGKILL)
	if err := c.SetClosed(t.Context(), true); err != nil {
		t.Fatal(err)
	}
	if err := proc.Run(t.Context(), filepath.Join(proc.Paths().Boot, bootstrapName)); err != nil {
		t.Fatal(err)
	}
	if res := c.waitServing(t, "app.example.net", "/who"); res.StatusCode != http.StatusServiceUnavailable {
		t.Errorf("closed with Caddy down, a route answers %d", res.StatusCode)
	} else {
		_ = res.Body.Close()
	}
}
