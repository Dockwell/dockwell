// SPDX-License-Identifier: AGPL-3.0-only

package files

import (
	"context"
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strings"
	"sync/atomic"
	"syscall"
	"testing"
	"time"
	"unicode/utf8"

	"golang.org/x/sys/unix"
	"google.golang.org/protobuf/proto"

	agentv1 "github.com/dockwell/dockwell/gen/dockwell/agent/v1"
	dockwellv1 "github.com/dockwell/dockwell/gen/dockwell/v1"
)

// fakeDisk is a server's data directory and Docker's, in a test's directory.
type fakeDisk struct {
	root, data, docker string
	server             *Server
}

func newDisk(t *testing.T) *fakeDisk {
	t.Helper()
	// EvalSymlinks: on some machines the test's directory is behind a link, and
	// paths in answers have every link followed.
	root, err := filepath.EvalSymlinks(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	d := &fakeDisk{root: root, data: filepath.Join(root, "data"), docker: filepath.Join(root, "docker")}
	write(t, filepath.Join(d.data, "control", "key"), "the install's key")
	write(t, filepath.Join(d.docker, "containers", "abc", "config.v2.json"), `{"Env":["DATABASE_PASSWORD=hunter2"]}`)
	write(t, filepath.Join(d.docker, "overlay2", "abc", "merged", "etc", "passwd"), "root")
	write(t, filepath.Join(d.docker, "volumes", "metadata.db"), "bolt")
	write(t, filepath.Join(d.docker, "volumes", "web", "opts.json"), `{"o":"password=hunter2"}`)
	write(t, filepath.Join(d.docker, "volumes", "web", "_data", "index.html"), "<h1>hello</h1>")
	d.server = New(d.data, nil, func(context.Context) (string, error) { return d.docker, nil })
	return d
}

func write(t *testing.T, path, content string) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte(content), 0o644); err != nil {
		t.Fatal(err)
	}
}

func link(t *testing.T, target, path string) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(target, path); err != nil {
		t.Fatal(err)
	}
}

func (d *fakeDisk) list(t *testing.T, p string) *agentv1.ListFilesResult {
	t.Helper()
	r, err := d.server.List(t.Context(), p)
	if err != nil {
		t.Fatalf("List(%s): %v", p, err)
	}
	mustSend(t, r)
	return r
}

func (d *fakeDisk) read(t *testing.T, p string) *agentv1.ReadFileResult {
	t.Helper()
	r, err := d.server.Read(t.Context(), p)
	if err != nil {
		t.Fatalf("Read(%s): %v", p, err)
	}
	mustSend(t, r)
	return r
}

// mustSend checks an answer can go on the stream at all: a string that isn't
// UTF-8 fails to marshal, which would leave the control plane waiting.
func mustSend(t *testing.T, m proto.Message) {
	t.Helper()
	if _, err := proto.Marshal(m); err != nil {
		t.Fatalf("this answer can't be sent: %v", err)
	}
}

func names(l *dockwellv1.ListFilesResponse) []string {
	var out []string
	for _, e := range l.GetEntries() {
		out = append(out, e.GetName())
	}
	return out
}

func entryNamed(t *testing.T, l *dockwellv1.ListFilesResponse, name string) *dockwellv1.FileEntry {
	t.Helper()
	for _, e := range l.GetEntries() {
		if e.GetName() == name {
			return e
		}
	}
	t.Fatalf("no entry %q in %v", name, names(l))
	return nil
}

func TestAListingSaysWhatADirectoryHolds(t *testing.T) {
	d := newDisk(t)
	dir := filepath.Join(d.root, "site")
	write(t, filepath.Join(dir, "index.php"), "<?php echo 'hi';")
	if err := os.Chmod(filepath.Join(dir, "index.php"), 0o640); err != nil {
		t.Fatal(err)
	}
	write(t, filepath.Join(dir, "wp-admin", "admin.php"), "x")
	link(t, "index.php", filepath.Join(dir, "home.php"))
	link(t, "wp-admin", filepath.Join(dir, "admin"))
	link(t, "gone", filepath.Join(dir, "broken"))
	if err := syscall.Mkfifo(filepath.Join(dir, "pipe"), 0o600); err != nil {
		t.Fatal(err)
	}

	got := d.list(t, dir)
	l := got.GetListing()
	if l == nil {
		t.Fatalf("refused: %v", got.GetRefusal())
	}
	if want := []string{"admin", "broken", "home.php", "index.php", "pipe", "wp-admin"}; !slices.Equal(names(l), want) {
		t.Fatalf("entries are %v, want %v", names(l), want)
	}
	if l.GetPath() != dir || l.GetDirectory().GetType() != dockwellv1.FileType_FILE_TYPE_DIRECTORY {
		t.Errorf("the listing is of %q, a %v", l.GetPath(), l.GetDirectory().GetType())
	}
	file := entryNamed(t, l, "index.php")
	if file.GetType() != dockwellv1.FileType_FILE_TYPE_FILE || file.GetSizeBytes() != 16 || file.GetMode() != "-rw-r-----" {
		t.Errorf("index.php reads as %v", file)
	}
	if file.GetOwner() == "" || file.GetGroup() == "" || file.GetModifyTime() == nil {
		t.Errorf("index.php has no owner, group or time: %v", file)
	}
	if e := entryNamed(t, l, "home.php"); e.GetType() != dockwellv1.FileType_FILE_TYPE_SYMLINK ||
		e.GetLinkTarget() != "index.php" || e.GetLinkType() != dockwellv1.FileType_FILE_TYPE_FILE {
		t.Errorf("home.php reads as %v", e)
	}
	if e := entryNamed(t, l, "admin"); e.GetLinkType() != dockwellv1.FileType_FILE_TYPE_DIRECTORY {
		t.Errorf("admin leads to %v, want a directory", e.GetLinkType())
	}
	if e := entryNamed(t, l, "broken"); e.GetLinkTarget() != "gone" || e.GetLinkType() != dockwellv1.FileType_FILE_TYPE_UNSPECIFIED {
		t.Errorf("a link that leads nowhere reads as %v", e)
	}
	if e := entryNamed(t, l, "pipe"); e.GetType() != dockwellv1.FileType_FILE_TYPE_OTHER || e.GetMode() != "prw-------" {
		t.Errorf("a FIFO reads as %v", e)
	}
}

func TestAListingFollowsASymlinkToADirectory(t *testing.T) {
	d := newDisk(t)
	write(t, filepath.Join(d.root, "real", "a.txt"), "a")
	link(t, "real", filepath.Join(d.root, "there"))
	l := d.list(t, filepath.Join(d.root, "there")).GetListing()
	if l.GetPath() != filepath.Join(d.root, "real") || !slices.Equal(names(l), []string{"a.txt"}) {
		t.Fatalf("listed %q: %v", l.GetPath(), names(l))
	}
}

func TestANameThatIsntUTF8DoesntStopAListing(t *testing.T) {
	d := newDisk(t)
	dir := filepath.Join(d.root, "site")
	write(t, filepath.Join(dir, "plain.txt"), "x")
	if err := os.WriteFile(dir+"/caf\xe9.txt", []byte("x"), 0o644); err != nil {
		t.Skipf("this file system won't take a name that isn't UTF-8: %v", err)
	}
	link(t, "tar\xe9", filepath.Join(dir, "link"))
	l := d.list(t, dir).GetListing()
	if got := entryNamed(t, l, "caf�.txt"); !got.GetUnreadableName() {
		t.Errorf("the name isn't marked: %v", got)
	}
	if entryNamed(t, l, "plain.txt").GetUnreadableName() {
		t.Error("a plain name is marked")
	}
	if got := entryNamed(t, l, "link"); got.GetLinkTarget() != "tar�" {
		t.Errorf("the link's target reads %q", got.GetLinkTarget())
	}
	// A link can lead through such a name, and what's said of it has to send.
	write(t, dir+"/caf\xe9/a.txt", "x")
	link(t, "caf\xe9/a.txt", filepath.Join(dir, "through"))
	r := d.read(t, filepath.Join(dir, "through"))
	if !strings.HasSuffix(r.GetFile().GetPath(), "caf�/a.txt") {
		t.Errorf("read %q", r.GetFile().GetPath())
	}
}

func TestAListingStopsAtItsLimits(t *testing.T) {
	d := newDisk(t)
	t.Run("entries", func(t *testing.T) {
		dir := filepath.Join(d.root, "many")
		if err := os.Mkdir(dir, 0o755); err != nil {
			t.Fatal(err)
		}
		for i := range MaxEntries + 5 {
			f, err := os.Create(filepath.Join(dir, "f"+strings.Repeat("0", 3)+itoa(i)))
			if err != nil {
				t.Fatal(err)
			}
			_ = f.Close()
		}
		l := d.list(t, dir).GetListing()
		if len(l.GetEntries()) != MaxEntries || !l.GetTruncated() {
			t.Fatalf("got %d entries, truncated %t", len(l.GetEntries()), l.GetTruncated())
		}
	})
	t.Run("bytes", func(t *testing.T) {
		dir := filepath.Join(d.root, "long")
		if err := os.Mkdir(dir, 0o755); err != nil {
			t.Fatal(err)
		}
		target := strings.Repeat("x/", 2000)
		for i := range 1200 { // over 4 MiB of targets, with far fewer entries than the limit
			link(t, target, filepath.Join(dir, "l"+itoa(i)))
		}
		r := d.list(t, dir)
		l := r.GetListing()
		if !l.GetTruncated() || len(l.GetEntries()) == 1200 {
			t.Fatalf("got %d entries, truncated %t", len(l.GetEntries()), l.GetTruncated())
		}
		if size := proto.Size(r); size > 5<<20 {
			t.Fatalf("a listing is %d bytes, too near the stream's limit", size)
		}
	})
}

func itoa(i int) string {
	var b []byte
	for {
		b = append([]byte{byte('0' + i%10)}, b...)
		if i /= 10; i == 0 {
			return string(b)
		}
	}
}

func TestReadingAFile(t *testing.T) {
	d := newDisk(t)
	big := strings.Repeat("0123456789abcde\n", MaxFileBytes/16)
	cases := []struct {
		name, content string
		want          string
		binary, cut   bool
	}{
		{name: "text", content: "server {\n  listen 80;\n}\n", want: "server {\n  listen 80;\n}\n"},
		{name: "empty", content: "", want: ""},
		{name: "chinese", content: "你好，世界\n", want: "你好，世界\n"},
		{name: "exactly the limit", content: big, want: big},
		{name: "one byte over", content: big + "x", want: big, cut: true},
		{name: "a character the limit splits", content: big[:MaxFileBytes-1] + "好", want: big[:MaxFileBytes-1], cut: true},
		{name: "a NUL", content: "text\x00more", binary: true},
		{name: "Latin-1", content: "caf\xe9\n", binary: true},
		{name: "binary and huge", content: strings.Repeat("\x7fELF\x00", MaxFileBytes), binary: true},
		{name: "a document that mentions a key", content: "Paste -----BEGIN PRIVATE KEY----- here\n", want: "Paste -----BEGIN PRIVATE KEY----- here\n"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			path := filepath.Join(d.root, "read", strings.ReplaceAll(c.name, " ", "-"))
			write(t, path, c.content)
			got := d.read(t, path)
			f := got.GetFile()
			if f == nil {
				t.Fatalf("refused: %v", got.GetRefusal())
			}
			if f.GetContent() != c.want || f.GetBinary() != c.binary || f.GetTruncated() != c.cut {
				t.Fatalf("read %d bytes, binary %t, truncated %t; want %d, %t, %t", len(f.GetContent()), f.GetBinary(), f.GetTruncated(), len(c.want), c.binary, c.cut)
			}
			if f.GetFile().GetSizeBytes() != uint64(len(c.content)) || f.GetPath() != path {
				t.Errorf("the file reads as %v", f.GetFile())
			}
		})
	}
}

func refusalOf(t *testing.T, d *fakeDisk, p string) *agentv1.FileRefusal {
	t.Helper()
	r := d.read(t, p)
	if r.GetRefusal() == nil {
		t.Fatalf("read %s: %q", p, r.GetFile().GetContent())
	}
	return r.GetRefusal()
}

func TestWhatHoldsASecretIsRefused(t *testing.T) {
	d := newDisk(t)
	allowed := filepath.Join(d.root, "app")
	write(t, filepath.Join(allowed, "ok.txt"), "fine")
	key := filepath.Join(d.data, "control", "key")
	// What a compromised app can leave in a volume, and what else leads there.
	link(t, key, filepath.Join(allowed, "absolute"))
	link(t, "../data/control/key", filepath.Join(allowed, "relative"))
	link(t, "absolute", filepath.Join(allowed, "chain"))
	link(t, d.data, filepath.Join(allowed, "data"))
	link(t, "/proc/self/environ", filepath.Join(allowed, "environ"))
	link(t, filepath.Join(d.docker, "containers"), filepath.Join(allowed, "containers"))
	link(t, "/proc/self/root/"+strings.TrimPrefix(key, "/"), filepath.Join(allowed, "magic"))
	write(t, filepath.Join(allowed, ".pgpass"), "db:5432:*:dockwell:hunter2\n")
	write(t, filepath.Join(allowed, "id_ed25519"), "-----BEGIN OPENSSH PRIVATE KEY-----\nb3BlbnNzaC1rZXk=\n-----END OPENSSH PRIVATE KEY-----\n")
	write(t, filepath.Join(allowed, "tls.pem"), "-----BEGIN CERTIFICATE-----\nMIIB\n-----END CERTIFICATE-----\n-----BEGIN RSA PRIVATE KEY-----\r\nMIIE\r\n")

	for _, p := range []string{
		d.data,
		key,
		filepath.Join(d.docker, "containers", "abc", "config.v2.json"),
		filepath.Join(d.docker, "overlay2"),
		filepath.Join(d.docker, "volumes", "metadata.db"),
		filepath.Join(d.docker, "volumes", "web", "opts.json"),
		"/proc/self/environ",
		"/proc/1/root/etc/passwd",
		"/proc/self/root/etc/passwd",
		"/sys/kernel/notes",
		"/dev/null",
		"/dev/sda",
		"/run/containerd/io.containerd.runtime.v2.task/moby/abc/config.json",
		"/var/run/containerd/containerd.sock",
		"/etc/shadow",
		"/etc/gshadow",
		filepath.Join(allowed, "absolute"),
		filepath.Join(allowed, "relative"),
		filepath.Join(allowed, "chain"),
		filepath.Join(allowed, "data", "control", "key"),
		filepath.Join(allowed, "environ"),
		filepath.Join(allowed, "containers", "abc", "config.v2.json"),
		filepath.Join(allowed, "magic"),
		filepath.Join(allowed, ".pgpass"),
		filepath.Join(allowed, "id_ed25519"),
		filepath.Join(allowed, "tls.pem"),
	} {
		t.Run(p, func(t *testing.T) {
			got := refusalOf(t, d, p)
			if got.GetKind() != agentv1.FileRefusal_KIND_REFUSED {
				t.Fatalf("refused as %v: %s", got.GetKind(), got.GetMessage())
			}
			if !strings.Contains(got.GetMessage(), "so Dockwell doesn't open it. Connect to the server over SSH") {
				t.Errorf("it doesn't say why, or what to do: %s", got.GetMessage())
			}
			if strings.Contains(got.GetMessage(), "hunter2") {
				t.Errorf("a refusal gave a value away: %s", got.GetMessage())
			}
		})
	}
	if d.read(t, filepath.Join(allowed, "ok.txt")).GetFile() == nil {
		t.Error("a plain file beside them is refused")
	}
}

// The guard goes by what a directory is, so another name for it is no way in. A
// bind mount, which a test can't make, has the identity of what it mounts, as a
// data directory named through a symlink here does, where no prefix of the
// walked path matches the configured one.
func TestAPlaceIsRefusedByWhatItIsNotHowItIsNamed(t *testing.T) {
	d := newDisk(t)
	configured := filepath.Join(d.root, "elsewhere", "dockwell")
	link(t, d.data, configured)
	d.server = New(configured, nil, func(context.Context) (string, error) { return filepath.Join(d.root, "elsewhere", "docker"), nil })
	link(t, d.docker, filepath.Join(d.root, "elsewhere", "docker"))
	for _, p := range []string{
		filepath.Join(d.data, "control", "key"),
		filepath.Join(d.docker, "containers", "abc", "config.v2.json"),
	} {
		if got := refusalOf(t, d, p); got.GetKind() != agentv1.FileRefusal_KIND_REFUSED {
			t.Errorf("%s: %v", p, got)
		}
	}
}

func TestWhatsRefusedIsMarkedInAListing(t *testing.T) {
	d := newDisk(t)
	write(t, filepath.Join(d.root, ".pgpass"), "x")
	write(t, filepath.Join(d.root, "plain"), "x")
	l := d.list(t, d.root).GetListing()
	for name, want := range map[string]bool{"data": true, "docker": false, ".pgpass": true, "plain": false} {
		if got := entryNamed(t, l, name).GetRefused(); got != want {
			t.Errorf("%s refused: %t, want %t", name, got, want)
		}
	}
	root := d.list(t, "/").GetListing()
	for _, name := range []string{"proc", "sys", "dev", "run"} {
		if !entryNamed(t, root, name).GetRefused() {
			t.Errorf("/%s isn't marked", name)
		}
	}
	if entryNamed(t, root, "etc").GetRefused() {
		t.Error("/etc is marked")
	}
}

func TestDockersDataOpensOnlyAsFarAsAVolumesFiles(t *testing.T) {
	d := newDisk(t)
	root := d.list(t, d.docker).GetListing()
	if !slices.Equal(names(root), []string{"containers", "overlay2", "volumes"}) {
		t.Fatalf("Docker's root lists %v", names(root))
	}
	if !entryNamed(t, root, "containers").GetRefused() || entryNamed(t, root, "volumes").GetRefused() {
		t.Errorf("Docker's root reads as %v", root.GetEntries())
	}
	volumes := d.list(t, filepath.Join(d.docker, "volumes")).GetListing()
	if !entryNamed(t, volumes, "metadata.db").GetRefused() || entryNamed(t, volumes, "web").GetRefused() {
		t.Errorf("the volumes read as %v", volumes.GetEntries())
	}
	web := d.list(t, filepath.Join(d.docker, "volumes", "web")).GetListing()
	if !entryNamed(t, web, "opts.json").GetRefused() || entryNamed(t, web, "_data").GetRefused() {
		t.Errorf("a volume reads as %v", web.GetEntries())
	}
	got := d.read(t, filepath.Join(d.docker, "volumes", "web", "_data", "index.html"))
	if got.GetFile().GetContent() != "<h1>hello</h1>" {
		t.Fatalf("a volume's file reads as %v", got)
	}
}

func TestDockerStoppingKeepsWhatItSaidLast(t *testing.T) {
	d := newDisk(t)
	answer := d.docker
	d.server = New(d.data, nil, func(context.Context) (string, error) {
		if answer == "" {
			return "", errors.New("Docker isn't running")
		}
		return answer, nil
	})
	config := filepath.Join(d.docker, "containers", "abc", "config.v2.json")
	refusalOf(t, d, config)
	answer = ""
	if got := refusalOf(t, d, config); got.GetKind() != agentv1.FileRefusal_KIND_REFUSED {
		t.Fatalf("with Docker stopped, its data reads as %v", got)
	}
}

func TestPathsThatDontResolve(t *testing.T) {
	d := newDisk(t)
	write(t, filepath.Join(d.root, "file"), "x")
	link(t, "gone", filepath.Join(d.root, "broken"))
	link(t, "loop", filepath.Join(d.root, "loop"))
	if err := syscall.Mkfifo(filepath.Join(d.root, "pipe"), 0o600); err != nil {
		t.Fatal(err)
	}
	link(t, "/dev/zero", filepath.Join(d.root, "zeros"))
	for _, c := range []struct {
		path string
		kind agentv1.FileRefusal_Kind
		says string
	}{
		{filepath.Join(d.root, "nothing"), agentv1.FileRefusal_KIND_NOT_FOUND, "there's nothing at"},
		{filepath.Join(d.root, "file", "under"), agentv1.FileRefusal_KIND_NOT_FOUND, "there's nothing at"},
		{filepath.Join(d.root, "broken"), agentv1.FileRefusal_KIND_NOT_FOUND, "there's nothing at"},
		{filepath.Join(d.root, "loop"), agentv1.FileRefusal_KIND_NOT_FOUND, "more than 40 symlinks"},
		{filepath.Join(d.root, "pipe"), agentv1.FileRefusal_KIND_NOT_A_FILE, "a FIFO"},
		{d.root, agentv1.FileRefusal_KIND_NOT_A_FILE, "a directory"},
		{filepath.Join(d.root, "zeros"), agentv1.FileRefusal_KIND_REFUSED, "devices"},
	} {
		t.Run(c.path, func(t *testing.T) {
			ctx, cancel := context.WithTimeout(t.Context(), 5*time.Second)
			defer cancel()
			r, err := d.server.Read(ctx, c.path)
			if err != nil {
				t.Fatal(err)
			}
			got := r.GetRefusal()
			if got.GetKind() != c.kind || !strings.Contains(got.GetMessage(), c.says) {
				t.Fatalf("got %v", r)
			}
		})
	}
	if got := d.list(t, filepath.Join(d.root, "file")).GetRefusal(); got.GetKind() != agentv1.FileRefusal_KIND_NOT_A_DIRECTORY {
		t.Errorf("listing a file: %v", got)
	}
}

func TestWhatTheServerKeepsFromTheAgentSaysSo(t *testing.T) {
	if os.Geteuid() == 0 {
		t.Skip("root reads everything here, as the agent does on a server")
	}
	d := newDisk(t)
	shut := filepath.Join(d.root, "shut")
	write(t, filepath.Join(shut, "file"), "x")
	if err := os.Chmod(shut, 0); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.Chmod(shut, 0o755) })
	if got := refusalOf(t, d, filepath.Join(shut, "file")); got.GetKind() != agentv1.FileRefusal_KIND_DENIED {
		t.Errorf("reading through a shut directory: %v", got)
	}
	if got := d.list(t, shut).GetRefusal(); got.GetKind() != agentv1.FileRefusal_KIND_DENIED {
		t.Errorf("listing a shut directory: %v", got)
	}
}

func TestAPathMustSayWhatItMeans(t *testing.T) {
	d := newDisk(t)
	for _, p := range []string{"", "etc", "./etc", "/etc/../root", "/etc/.", "/etc/\x00shadow", "/etc/\nshadow", "/" + strings.Repeat("a", maxPath)} {
		if _, err := d.server.List(t.Context(), p); err == nil {
			t.Errorf("List(%q) went ahead", p)
		}
		if _, err := d.server.Read(t.Context(), p); err == nil {
			t.Errorf("Read(%q) went ahead", p)
		}
	}
	for _, p := range []string{"/", "/etc", "//etc", "/etc/", "/opt/my..file"} {
		if err := CheckPath(p); err != nil {
			t.Errorf("CheckPath(%q): %v", p, err)
		}
	}
}

// What's read is what was checked: a file swapped in after the walk isn't read
// in its place, even when the agent is slow to get to it.
func TestAFileSwappedAfterItsCheckIsntRead(t *testing.T) {
	d := newDisk(t)
	p := filepath.Join(d.root, "file")
	write(t, p, "checked")
	trail := &trail{s: d.server}
	w, refusal := d.server.guard(t.Context()).walk(trail, p)
	if refusal != nil {
		t.Fatal(refusal)
	}
	defer w.close()
	if err := os.Remove(p); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(filepath.Join(d.data, "control", "key"), p); err != nil {
		t.Fatal(err)
	}
	if _, _, got := readText(trail, w); got == nil {
		t.Fatal("a link swapped in was read")
	}
	if err := os.Remove(p); err != nil {
		t.Fatal(err)
	}
	if err := syscall.Mkfifo(p, 0o600); err != nil {
		t.Fatal(err)
	}
	done := make(chan struct{})
	go func() {
		defer close(done)
		if _, _, got := readText(trail, w); got == nil {
			t.Error("a FIFO swapped in was read")
		}
	}()
	select {
	case <-done:
	case <-time.After(5 * time.Second):
		t.Fatal("a FIFO swapped in hung the read")
	}
}

func TestCallsTakeTurns(t *testing.T) {
	d := newDisk(t)
	for range slots {
		release, err := d.server.Take(t.Context())
		if err != nil {
			t.Fatal(err)
		}
		t.Cleanup(release)
	}
	ctx, cancel := context.WithTimeout(t.Context(), 20*time.Millisecond)
	defer cancel()
	if _, err := d.server.Take(ctx); !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("a fifth call went ahead: %v", err)
	}
}

func TestModeStrings(t *testing.T) {
	for mode, want := range map[uint32]string{
		syscall.S_IFREG | 0o644:  "-rw-r--r--",
		syscall.S_IFDIR | 0o755:  "drwxr-xr-x",
		syscall.S_IFDIR | 0o1777: "drwxrwxrwt",
		syscall.S_IFREG | 0o4755: "-rwsr-xr-x",
		syscall.S_IFREG | 0o2644: "-rw-r-Sr--",
		syscall.S_IFLNK | 0o777:  "lrwxrwxrwx",
		syscall.S_IFSOCK | 0o660: "srw-rw----",
		syscall.S_IFCHR | 0o666:  "crw-rw-rw-",
	} {
		if got := modeString(mode); got != want {
			t.Errorf("mode %o reads %s, want %s", mode, got, want)
		}
	}
}

// A root nobody has named yet, as when Docker doesn't answer and this agent has
// never heard from it, is shut by what it is.
func TestDockersDataIsShutWhenNoOneHasSaidWhereItIs(t *testing.T) {
	d := newDisk(t)
	moved := filepath.Join(d.root, "elsewhere", "docker")
	config := filepath.Join(moved, "containers", "abc", "config.v2.json")
	opts := filepath.Join(moved, "volumes", "web", "opts.json")
	layer := filepath.Join(moved, "overlay2", "abc", "merged", "app.env")
	write(t, config, `{"Env":["DATABASE_PASSWORD=hunter2"]}`)
	write(t, opts, `{"o":"password=hunter2"}`)
	write(t, layer, "DATABASE_PASSWORD=hunter2\n")
	files := filepath.Join(moved, "volumes", "web", "_data", "index.html")
	write(t, files, "<h1>hello</h1>")
	d.server = New(d.data, nil, func(context.Context) (string, error) { return "", errors.New("Docker isn't answering") })
	for _, p := range []string{config, opts, layer} {
		if got := refusalOf(t, d, p); got.GetKind() != agentv1.FileRefusal_KIND_REFUSED {
			t.Errorf("%s: %v", p, got)
		}
	}
	if got := d.read(t, files).GetFile().GetContent(); got != "<h1>hello</h1>" {
		t.Errorf("a volume's files read as %q", got)
	}
}

// Where Docker keeps its data outlives the agent, so a root with nothing about
// it to tell it by, as it has only just started, stays shut after a restart.
func TestARestartKeepsWhereDockersDataIs(t *testing.T) {
	d := newDisk(t)
	root := filepath.Join(d.root, "bare")
	secret := filepath.Join(root, "overlay2", "abc", "app.env")
	write(t, secret, "DATABASE_PASSWORD=hunter2\n")
	answer := root
	ask := func(context.Context) (string, error) {
		if answer == "" {
			return "", errors.New("Docker isn't answering")
		}
		return answer, nil
	}
	d.server = New(d.data, nil, ask)
	if got := refusalOf(t, d, secret); got.GetKind() != agentv1.FileRefusal_KIND_REFUSED {
		t.Fatalf("with Docker answering: %v", got)
	}
	answer = ""
	d.server = New(d.data, nil, ask)
	if got := refusalOf(t, d, secret); got.GetKind() != agentv1.FileRefusal_KIND_REFUSED {
		t.Fatalf("after a restart with Docker away: %v", got)
	}
	// A root that moves leaves the old one behind, with what's in it.
	moved := filepath.Join(d.root, "bare2")
	if err := os.Mkdir(moved, 0o755); err != nil {
		t.Fatal(err)
	}
	answer = moved
	d.server = New(d.data, nil, ask)
	if got := refusalOf(t, d, secret); got.GetKind() != agentv1.FileRefusal_KIND_REFUSED {
		t.Errorf("once Docker's data moved, the old place reads as %v", got)
	}
	st, err := os.Stat(filepath.Join(d.data, "agent", "docker-roots"))
	if err != nil {
		t.Fatal(err)
	}
	if st.Mode().Perm() != 0o600 {
		t.Errorf("the list of roots is mode %v", st.Mode())
	}
}

func TestRememberingRootsStaysBounded(t *testing.T) {
	d := newDisk(t)
	root := ""
	d.server = New(d.data, nil, func(context.Context) (string, error) { return root, nil })
	for i := range maxRoots + 4 {
		root = filepath.Join(d.root, "root"+itoa(i))
		d.server.dockerRoots(t.Context())
	}
	again := New(d.data, nil, nil)
	if got := again.dockerRoots(t.Context()); len(got) != maxRoots+1 || got[len(got)-1] != root {
		t.Fatalf("after a restart it knows %v", got)
	}
}

// Those files are refused by what they are, wherever a copy turns up.
func TestWhatDockerKeepsIsRefusedWhereverItTurnsUp(t *testing.T) {
	d := newDisk(t)
	dir := filepath.Join(d.root, "backup")
	for path, content := range map[string]string{
		"config.v2.json":        `{"Env":["DATABASE_PASSWORD=hunter2"]}`,
		"hostconfig.json":       `{"Binds":null}`,
		"volumes/web/opts.json": `{"o":"password=hunter2"}`,
		"inspect.json":          `[{"ResolvConfPath":"/var/lib/docker/containers/abc/resolv.conf","Config":{"Env":["A=1"]}}]`,
		"renamed.txt":           `{"MountType":"cifs","MountOpts":"username=a,password=hunter2"}`,
	} {
		p := filepath.Join(dir, path)
		write(t, p, content)
		if got := refusalOf(t, d, p); got.GetKind() != agentv1.FileRefusal_KIND_REFUSED {
			t.Errorf("%s: %v", path, got)
		}
	}
	plain := filepath.Join(dir, "opts.json")
	write(t, plain, `{"theme":"dark"}`)
	if d.read(t, plain).GetFile() == nil {
		t.Error("an app's own opts.json is refused")
	}
	l := d.list(t, dir).GetListing()
	if !entryNamed(t, l, "config.v2.json").GetRefused() || entryNamed(t, l, "opts.json").GetRefused() {
		t.Errorf("the listing reads as %v", l.GetEntries())
	}
}

func TestPasswordHashesAreRefusedByWhatTheyHold(t *testing.T) {
	d := newDisk(t)
	sha := "root:$6$saltsalt$abcdefghijklmnopqrstuvwxyz:19000:0:99999:7:::\n"
	for name, content := range map[string]string{
		"var/backups/shadow.bak": sha,
		"etc/shadow~":            sha,
		"etc/shadow.bak":         sha,
		"mnt/root/etc/shadow":    sha,
		"yescrypt":               "daemon:*:19000::::::\nmira:$y$j9T$abcdefgh$ijklmnopqrstuvwxyz:19000:0:99999:7:::\n",
		"bcrypt":                 "mira:$2b$12$abcdefghijklmnopqrstuv:19000::::::\n",
		"locked":                 "mira:!$6$saltsalt$abcdefgh:19000::::::\n",
		"htpasswd":               "mira:$apr1$abcdefgh$ijklmnopqrstuv\n",
	} {
		p := filepath.Join(d.root, "hashes", name)
		write(t, p, content)
		if got := refusalOf(t, d, p); got.GetKind() != agentv1.FileRefusal_KIND_REFUSED || !strings.Contains(got.GetMessage(), "password hashes") {
			t.Errorf("%s: %v", name, got)
		}
	}
	for name, content := range map[string]string{
		"passwd":   "root:x:0:0:root:/root:/bin/bash\ndaemon:x:1:1:daemon:/usr/sbin:/usr/sbin/nologin\n",
		"shell":    "export PATH=$HOME/bin:$PATH\nfoo:$(bar)\n",
		"yaml":     "image: nginx:$TAG\nport: $PORT$\n",
		"comment":  "# root:$6$saltsalt$abcdefgh\n",
		"prices":   "total: $5$ and more\n",
		"makefile": "all:$(OBJ)\n\t$(CC) -o $@ $^\n",
	} {
		p := filepath.Join(d.root, "plain", name)
		write(t, p, content)
		if d.read(t, p).GetFile() == nil {
			t.Errorf("%s reads as hashes", name)
		}
	}
}

func TestAPrivateKeyIsRefusedHoweverItIsSetIn(t *testing.T) {
	d := newDisk(t)
	const bom = "\xef\xbb\xbf"
	for name, content := range map[string]string{
		"column-0":   "-----BEGIN PRIVATE KEY-----\nMIIE\n",
		"indented":   "tls:\n  key: |\n    -----BEGIN PRIVATE KEY-----\n    MIIE\n",
		"tab":        "\t-----BEGIN RSA PRIVATE KEY-----\nMIIE\n",
		"bom":        bom + "-----BEGIN OPENSSH PRIVATE KEY-----\nMIIE\n",
		"bom-spaces": bom + "  -----BEGIN EC PRIVATE KEY-----\r\nMIIE\r\n",
		"crlf":       "a\r\n\t  -----BEGIN ENCRYPTED PRIVATE KEY-----  \r\nMIIE\r\n",
		"after-cert": "-----BEGIN CERTIFICATE-----\nMIIB\n-----END CERTIFICATE-----\n   -----BEGIN PRIVATE KEY-----\nMIIE\n",
	} {
		p := filepath.Join(d.root, "keys", name)
		write(t, p, content)
		if got := refusalOf(t, d, p); got.GetKind() != agentv1.FileRefusal_KIND_REFUSED {
			t.Errorf("%s: %v", name, got)
		}
	}
	for name, content := range map[string]string{
		"mention":     "Paste -----BEGIN PRIVATE KEY----- here\n",
		"public":      "  -----BEGIN PUBLIC KEY-----\nMIIB\n",
		"certificate": "\t-----BEGIN CERTIFICATE-----\nMIIB\n",
	} {
		p := filepath.Join(d.root, "plain-keys", name)
		write(t, p, content)
		if d.read(t, p).GetFile() == nil {
			t.Errorf("%s reads as a key", name)
		}
	}
}

func TestPrintable(t *testing.T) {
	for in, want := range map[string]string{
		"/var/www/index.php": "/var/www/index.php",
		"/srv/你好/a b.txt":    "/srv/你好/a b.txt",
		"/a\nb":              "/a\uFFFDb",
		"/a\x1b[31mb":        "/a\uFFFD[31mb",
		"/a\u202eb":          "/a\uFFFDb",
		"/a\xe9b":            "/a\uFFFDb",
	} {
		if got := Printable(in); got != want {
			t.Errorf("Printable(%q) = %q, want %q", in, got, want)
		}
	}
	long := Printable(strings.Repeat("好", maxPrintable))
	if !strings.HasSuffix(long, "…") || !utf8.ValidString(long) || len(long) > maxPrintable+len("…") {
		t.Errorf("a long text comes to %d bytes", len(long))
	}
}

// savable reads a file, for the version a save takes.
// versionOf is what a read of the text at p would say, which anyone who knows
// what's there can work out.
func versionOf(t *testing.T, p, text string) string {
	t.Helper()
	var st unix.Stat_t
	if err := unix.Stat(p, &st); err != nil {
		t.Fatal(err)
	}
	return version(p, &st, []byte(text))
}

func savable(t *testing.T, d *fakeDisk, p string) string {
	t.Helper()
	v := d.read(t, p).GetFile().GetVersion()
	if v == "" {
		t.Fatalf("%s has no version to save over", p)
	}
	return v
}

func (d *fakeDisk) save(t *testing.T, p, content, version string) *agentv1.SaveFileResult {
	t.Helper()
	r, err := d.server.Save(t.Context(), p, content, version, "evt_save")
	if err != nil {
		t.Fatalf("Save(%s): %v", p, err)
	}
	mustSend(t, r)
	return r
}

func spares(t *testing.T, dir string) []string {
	t.Helper()
	found, err := filepath.Glob(filepath.Join(dir, ".dockwell-save-*"))
	if err != nil {
		t.Fatal(err)
	}
	return found
}

func TestSavingAFile(t *testing.T) {
	d := newDisk(t)
	p := filepath.Join(d.root, "site", "nginx.conf")
	write(t, p, "listen 80;\n")
	if err := os.Chmod(p, 0o640); err != nil {
		t.Fatal(err)
	}
	before, err := os.Stat(p)
	if err != nil {
		t.Fatal(err)
	}
	got := d.save(t, p, "listen 8080;\n", savable(t, d, p))
	saved := got.GetSaved()
	if saved == nil {
		t.Fatalf("refused: %v", got.GetRefusal())
	}
	content, err := os.ReadFile(p)
	if err != nil {
		t.Fatal(err)
	}
	if string(content) != "listen 8080;\n" {
		t.Fatalf("the file holds %q", content)
	}
	after, err := os.Stat(p)
	if err != nil {
		t.Fatal(err)
	}
	// A mount holds the inode, so a container that mounts this file sees the save.
	if !os.SameFile(before, after) {
		t.Error("the save replaced the file, so what mounts it keeps reading the old one")
	}
	if after.Mode() != before.Mode() {
		t.Errorf("its mode went from %v to %v", before.Mode(), after.Mode())
	}
	if saved.GetFile().GetSizeBytes() != 13 || saved.GetVersion() != d.read(t, p).GetFile().GetVersion() {
		t.Errorf("saved as %v", saved)
	}
	if left := spares(t, filepath.Dir(p)); len(left) != 0 {
		t.Errorf("the save left %v", left)
	}
	// What it gives is what the next save takes.
	if got := d.save(t, p, "shorter\n", saved.GetVersion()); got.GetSaved() == nil {
		t.Errorf("a second save: %v", got)
	}
	if content, _ := os.ReadFile(p); string(content) != "shorter\n" {
		t.Errorf("a shorter save left %q", content)
	}
}

func TestASaveNoticesAFileThatChanged(t *testing.T) {
	d := newDisk(t)
	p := filepath.Join(d.root, "site", "app.env")
	write(t, p, "A=1\n")
	version := savable(t, d, p)
	write(t, p, "A=2\n") // someone else, same size, within the same clock tick
	got := d.save(t, p, "A=3\n", version)
	if got.GetRefusal().GetKind() != agentv1.FileRefusal_KIND_CHANGED || !strings.Contains(got.GetRefusal().GetMessage(), "changed since you opened it") {
		t.Fatalf("got %v", got)
	}
	if content, _ := os.ReadFile(p); string(content) != "A=2\n" {
		t.Fatalf("the file holds %q", content)
	}
	if left := spares(t, filepath.Dir(p)); len(left) != 0 {
		t.Errorf("a refused save left %v", left)
	}
	// A file that grew past what can be read is changed too, not cut short.
	write(t, p, strings.Repeat("x", MaxFileBytes+1))
	if got := d.save(t, p, "A=3\n", version); got.GetRefusal().GetKind() != agentv1.FileRefusal_KIND_CHANGED {
		t.Errorf("a file that grew: %v", got)
	}
}

func TestASaveRefusesWhatAReadRefuses(t *testing.T) {
	d := newDisk(t)
	allowed := filepath.Join(d.root, "app")
	key := filepath.Join(d.data, "control", "key")
	link(t, key, filepath.Join(allowed, "planted"))
	write(t, filepath.Join(allowed, ".pgpass"), "x")
	write(t, filepath.Join(allowed, "tls.key"), "-----BEGIN PRIVATE KEY-----\nMIIE\n-----END PRIVATE KEY-----\n")
	// What goes by what it holds, each with the version a save would take, as a
	// version can be worked out by whoever knows the text.
	held := map[string]string{
		"indented.yaml": "tls:\n  key: |\n    -----BEGIN PRIVATE KEY-----\n    MIIE\n",
		"shadow.bak":    "root:$6$saltsalt$abcdefghijklmnop:19000:0:99999:7:::\n",
		"inspect.json":  `[{"ResolvConfPath":"/x","Config":{"Env":["A=1"]}}]`,
	}
	for name, text := range held {
		p := filepath.Join(allowed, name)
		write(t, p, text)
		if got := d.save(t, p, "written", versionOf(t, p, text)); got.GetRefusal().GetKind() != agentv1.FileRefusal_KIND_REFUSED {
			t.Errorf("saving over %s: %v", name, got)
		}
		if content, _ := os.ReadFile(p); string(content) != text {
			t.Errorf("%s holds %q", name, content)
		}
	}
	// Docker's data, with no one having said where it is.
	moved := filepath.Join(d.root, "elsewhere", "docker")
	config := filepath.Join(moved, "containers", "abc", "config.v2.json")
	opts := filepath.Join(moved, "volumes", "web", "opts.json")
	write(t, config, `{"Env":["A=1"]}`)
	write(t, opts, `{"o":"password=x"}`)
	dark := New(d.data, nil, func(context.Context) (string, error) { return "", errors.New("Docker isn't answering") })
	for p, text := range map[string]string{config: `{"Env":["A=1"]}`, opts: `{"o":"password=x"}`} {
		r, err := dark.Save(t.Context(), p, "written", versionOf(t, p, text), "evt_save")
		if err != nil || r.GetRefusal().GetKind() != agentv1.FileRefusal_KIND_REFUSED {
			t.Errorf("saving %s with Docker's root unknown: %v, %v", p, r, err)
		}
	}
	stale := strings.Repeat("0", 64)
	for _, p := range []string{
		key,
		filepath.Join(allowed, "planted"),
		filepath.Join(d.docker, "containers", "abc", "config.v2.json"),
		filepath.Join(d.docker, "volumes", "web", "opts.json"),
		"/proc/self/environ",
		"/etc/shadow",
		filepath.Join(allowed, ".pgpass"),
		filepath.Join(allowed, "tls.key"),
	} {
		if got := d.save(t, p, "written", stale); got.GetRefusal().GetKind() != agentv1.FileRefusal_KIND_REFUSED {
			t.Errorf("saving %s: %v", p, got)
		}
	}
	if content, _ := os.ReadFile(key); string(content) != "the install's key" {
		t.Fatalf("the key holds %q", content)
	}
}

func TestASaveWritesThroughALinkToWhereItLeads(t *testing.T) {
	d := newDisk(t)
	target := filepath.Join(d.root, "app", "real.conf")
	write(t, target, "a\n")
	link(t, "real.conf", filepath.Join(d.root, "app", "current.conf"))
	via := filepath.Join(d.root, "app", "current.conf")
	got := d.save(t, via, "b\n", savable(t, d, via))
	if got.GetSaved().GetPath() != target {
		t.Fatalf("got %v", got)
	}
	if st, err := os.Lstat(via); err != nil || st.Mode()&os.ModeSymlink == 0 {
		t.Errorf("the link went: %v %v", st, err)
	}
	if content, _ := os.ReadFile(target); string(content) != "b\n" {
		t.Errorf("the file holds %q", content)
	}
}

func TestASaveLeavesWhatIsntAFile(t *testing.T) {
	d := newDisk(t)
	dir := filepath.Join(d.root, "app")
	write(t, filepath.Join(dir, "a"), "x")
	stale := strings.Repeat("0", 64)
	if got := d.save(t, dir, "x", stale); got.GetRefusal().GetKind() != agentv1.FileRefusal_KIND_NOT_A_FILE {
		t.Errorf("a directory: %v", got)
	}
	if got := d.save(t, filepath.Join(dir, "nothing"), "x", stale); got.GetRefusal().GetKind() != agentv1.FileRefusal_KIND_NOT_FOUND {
		t.Errorf("nothing there: %v", got)
	}
	if _, err := d.server.Save(t.Context(), filepath.Join(dir, "a"), strings.Repeat("x", MaxFileBytes+1), stale, "evt_save"); err == nil {
		t.Error("a save over the limit went ahead")
	}
	if _, err := d.server.Save(t.Context(), dir+"/../a", "x", stale, "evt_save"); err == nil {
		t.Error("a save with .. in its path went ahead")
	}
}

// A person never saw a binary here, so its digest proves nothing: anyone can work
// out that of a well-known one.
func TestASaveLeavesABinaryAlone(t *testing.T) {
	d := newDisk(t)
	p := filepath.Join(d.root, "app", "tool")
	binary := "\x7fELF\x00\x01"
	write(t, p, binary)
	got := d.save(t, p, "#!/bin/sh\n", versionOf(t, p, binary))
	if got.GetRefusal().GetKind() != agentv1.FileRefusal_KIND_NOT_A_FILE || !strings.Contains(got.GetRefusal().GetMessage(), "isn't a text file") {
		t.Fatalf("got %v", got)
	}
	if content, _ := os.ReadFile(p); string(content) != binary {
		t.Fatalf("the file holds %q", content)
	}
}

func TestASaveTheServerKeepsOutLeavesTheFile(t *testing.T) {
	if os.Geteuid() == 0 {
		t.Skip("root writes everything here")
	}
	d := newDisk(t)
	dir := filepath.Join(d.root, "app")
	p := filepath.Join(dir, "conf")
	write(t, p, "a\n")
	version := savable(t, d, p)
	if err := os.Chmod(dir, 0o555); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.Chmod(dir, 0o755) })
	got := d.save(t, p, "b\n", version)
	if got.GetRefusal().GetKind() != agentv1.FileRefusal_KIND_DENIED || !strings.Contains(got.GetRefusal().GetMessage(), "nothing was saved") {
		t.Fatalf("got %v", got)
	}
	if content, _ := os.ReadFile(p); string(content) != "a\n" {
		t.Fatalf("the file holds %q", content)
	}
}

// A write that fails partway puts the file back. One that can't says where the text
// the file had is, as that's what can't be had again: the browser has what was typed.
func TestAWriteThatFailsPartwayPutsTheFileBack(t *testing.T) {
	d := newDisk(t)
	dir := filepath.Join(d.root, "app")
	p := filepath.Join(dir, "conf")
	write(t, p, "before\n")
	version := savable(t, d, p)
	full := errors.New("no space left on device")

	calls := 0
	putIn = func(f *os.File, text []byte) error {
		calls++
		if calls == 1 {
			if err := overwrite(f, text[:1]); err != nil { // cut short, as a full disk leaves it
				t.Fatal(err)
			}
			return full
		}
		return overwrite(f, text)
	}
	t.Cleanup(func() { putIn = overwrite })
	got := d.save(t, p, "after\n", version)
	if got.GetRefusal().GetKind() != agentv1.FileRefusal_KIND_DENIED || !strings.Contains(got.GetRefusal().GetMessage(), "nothing was saved") {
		t.Fatalf("got %v", got)
	}
	if content, _ := os.ReadFile(p); string(content) != "before\n" {
		t.Fatalf("the file holds %q", content)
	}
	if left := spares(t, dir); len(left) != 0 {
		t.Errorf("left %v", left)
	}

	calls = 0
	putIn = func(f *os.File, text []byte) error {
		calls++
		if calls == 1 {
			_ = overwrite(f, text[:1])
		}
		return full
	}
	got = d.save(t, p, "after\n", version)
	if content, _ := os.ReadFile(p); string(content) != "a" {
		t.Fatalf("the file holds %q, not what a write cut short leaves", content)
	}
	left := spares(t, dir)
	if len(left) != 1 {
		t.Fatalf("the text the file had isn't beside it: %v", left)
	}
	if !strings.Contains(got.GetRefusal().GetMessage(), filepath.Base(left[0])) {
		t.Errorf("it doesn't say where it is: %s", got.GetRefusal().GetMessage())
	}
	kept, err := os.ReadFile(left[0])
	if err != nil {
		t.Fatal(err)
	}
	st, err := os.Stat(left[0])
	if err != nil {
		t.Fatal(err)
	}
	if string(kept) != "before\n" || st.Mode().Perm() != 0o600 {
		t.Errorf("the spare holds %q, mode %v", kept, st.Mode())
	}
}

// A limit on how large a file can grow fails a write as a full disk does, in the
// kernel, partway through, and it leaves room for the copy, as a disk can. It's set
// on a process of its own, as it holds for everything in it.
func TestAWriteTheDiskCantHoldLeavesTheFileAsItWas(t *testing.T) {
	const limit = 4096
	if os.Getenv("DOCKWELL_TEST_FILE_LIMIT") == "" {
		run := exec.Command(os.Args[0], "-test.run=^"+t.Name()+"$", "-test.v")
		run.Env = append(os.Environ(), "DOCKWELL_TEST_FILE_LIMIT=1")
		out, err := run.CombinedOutput()
		if err != nil || !strings.Contains(string(out), "PASS") {
			t.Fatalf("%v\n%s", err, out)
		}
		return
	}
	d := newDisk(t)
	dir := filepath.Join(d.root, "app")
	p := filepath.Join(dir, "conf")
	write(t, p, "before\n")
	version := savable(t, d, p)
	if err := unix.Setrlimit(unix.RLIMIT_FSIZE, &unix.Rlimit{Cur: limit, Max: limit}); err != nil {
		t.Fatal(err)
	}
	got := d.save(t, p, strings.Repeat("x", 2*limit), version)
	if got.GetRefusal().GetKind() != agentv1.FileRefusal_KIND_DENIED || !strings.Contains(got.GetRefusal().GetMessage(), "nothing was saved") {
		t.Fatalf("got %v", got)
	}
	if content, _ := os.ReadFile(p); string(content) != "before\n" {
		t.Fatalf("the file holds %d bytes, starting %.20q", len(content), content)
	}
	if left := spares(t, dir); len(left) != 0 {
		t.Errorf("left %v", left)
	}
}

// A browser's decoder takes the mark off the start of what it reads, so the text
// that comes back doesn't have it, and nobody can see to have taken it out. The
// version that comes back is of what's there, so the next save goes through.
func TestASaveKeepsTheMarkAFileStartsWith(t *testing.T) {
	d := newDisk(t)
	p := filepath.Join(d.root, "app.ini")
	write(t, p, "\xef\xbb\xbfold\n")
	got := d.save(t, p, "new\n", savable(t, d, p))
	if got.GetSaved() == nil {
		t.Fatalf("got %v", got)
	}
	if content, _ := os.ReadFile(p); string(content) != "\xef\xbb\xbfnew\n" {
		t.Fatalf("the file holds %q", content)
	}
	if now := savable(t, d, p); got.GetSaved().GetVersion() != now {
		t.Errorf("the save says version %s, and the file reads as %s", got.GetSaved().GetVersion(), now)
	}
	if again := d.save(t, p, "newer\n", got.GetSaved().GetVersion()); again.GetSaved() == nil {
		t.Errorf("a second save over what the first returned: %v", again)
	}
	// One sent with its mark, as a field gets it in JSON, gets one.
	if got := d.save(t, p, "\ufeffnewest\n", savable(t, d, p)); got.GetSaved() == nil {
		t.Fatalf("got %v", got)
	}
	if content, _ := os.ReadFile(p); string(content) != "\xef\xbb\xbfnewest\n" {
		t.Fatalf("the file holds %q", content)
	}
	// A file without one doesn't gain one.
	plain := filepath.Join(d.root, "plain.ini")
	write(t, plain, "old\n")
	if got := d.save(t, plain, "new\n", savable(t, d, plain)); got.GetSaved() == nil {
		t.Fatalf("got %v", got)
	}
	if content, _ := os.ReadFile(plain); string(content) != "new\n" {
		t.Fatalf("the file holds %q", content)
	}
}

// A read names the file it read, so a link swapped in after it isn't what it read,
// whatever that link's text. A person who sees that goes on, or doesn't, on purpose.
func TestAFileSwappedSinceItWasReadIsntSavedOver(t *testing.T) {
	const text = "deploy_key: |\n  ssh-ed25519 AAAA\n"
	for name, swap := range map[string]func(t *testing.T, p, keys string){
		"for a link to another file": func(t *testing.T, p, keys string) {
			t.Helper()
			if err := os.Remove(p); err != nil {
				t.Fatal(err)
			}
			if err := os.Symlink(keys, p); err != nil {
				t.Fatal(err)
			}
		},
		"for a link to another name of the same file": func(t *testing.T, p, keys string) {
			t.Helper()
			if err := os.Remove(keys); err != nil {
				t.Fatal(err)
			}
			if err := os.Link(p, keys); err != nil {
				t.Fatal(err)
			}
			if err := os.Remove(p); err != nil {
				t.Fatal(err)
			}
			if err := os.Symlink(keys, p); err != nil {
				t.Fatal(err)
			}
		},
		"for another file by a rename": func(t *testing.T, p, _ string) {
			t.Helper()
			next := p + ".new"
			write(t, next, text)
			if err := os.Rename(next, p); err != nil {
				t.Fatal(err)
			}
		},
	} {
		t.Run(name, func(t *testing.T) {
			d := newDisk(t)
			p := filepath.Join(d.root, "volume", "config.yml")
			write(t, p, text)
			keys := filepath.Join(d.root, "root", ".ssh", "authorized_keys")
			write(t, keys, text) // the same text, so only what the version covers can tell
			read := savable(t, d, p)
			swap(t, p, keys)
			got := d.save(t, p, "what a person typed\n", read).GetRefusal()
			if got.GetKind() != agentv1.FileRefusal_KIND_CHANGED {
				t.Fatalf("got %v", got)
			}
			if content, _ := os.ReadFile(keys); string(content) != text {
				t.Fatalf("the save went through a link swapped in: %q", content)
			}
			if content, _ := os.ReadFile(p); string(content) != text {
				t.Fatalf("the file holds %q", content)
			}
		})
	}
}

// What a read of it says after that names where it leads now, for a person to see.
func TestAReadSaysWhereALinkSwappedInLeads(t *testing.T) {
	d := newDisk(t)
	p := filepath.Join(d.root, "volume", "config.yml")
	write(t, p, "text")
	keys := filepath.Join(d.root, "root", ".ssh", "authorized_keys")
	write(t, keys, "text")
	first := d.read(t, p).GetFile()
	if err := os.Remove(p); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(keys, p); err != nil {
		t.Fatal(err)
	}
	now := d.read(t, p).GetFile()
	if first.GetPath() != p || now.GetPath() != keys || first.GetVersion() == now.GetVersion() {
		t.Errorf("first %s at %s, now %s at %s", first.GetVersion(), first.GetPath(), now.GetVersion(), now.GetPath())
	}
}

// saveOver is the last step of a save, once the file has been checked.
func saveOver(ctx context.Context, t *trail, w *walked) *agentv1.FileRefusal {
	var began atomic.Bool
	return t.s.putText(ctx, t, w, version(w.path(), &w.st, []byte("checked")), []byte("checked"), []byte("written"), &began)
}

// A file that goes from under a save isn't written, and neither is what took its
// place: the agent saves over what it read.
func TestAFileSwappedAfterItsCheckIsntSavedOver(t *testing.T) {
	d := newDisk(t)
	p := filepath.Join(d.root, "file")
	write(t, p, "checked")
	key := filepath.Join(d.data, "control", "key")
	trail := &trail{s: d.server}
	w, refusal := d.server.guard(t.Context()).walk(trail, p)
	if refusal != nil {
		t.Fatal(refusal)
	}
	defer w.close()
	if err := os.Remove(p); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(key, p); err != nil {
		t.Fatal(err)
	}
	if got := saveOver(t.Context(), trail, w); got == nil {
		t.Fatal("wrote through a link swapped in")
	}
	if content, _ := os.ReadFile(key); string(content) != "the install's key" {
		t.Fatalf("the key holds %q", content)
	}
	if err := os.Remove(p); err != nil {
		t.Fatal(err)
	}
	write(t, p, "another file altogether")
	if got := saveOver(t.Context(), trail, w); got.GetKind() != agentv1.FileRefusal_KIND_CHANGED {
		t.Fatalf("wrote over a file swapped in: %v", got)
	}
	if content, _ := os.ReadFile(p); string(content) != "another file altogether" {
		t.Fatalf("the file holds %q", content)
	}
	if left := spares(t, d.root); len(left) != 0 {
		t.Errorf("left %v", left)
	}
}

// A project can hold directories called containers and volumes. It isn't Docker's.
func TestAProjectWithDockersNamesIsAProject(t *testing.T) {
	d := newDisk(t)
	stack := filepath.Join(d.root, "srv", "stack")
	compose := filepath.Join(stack, "docker-compose.yml")
	dockerfile := filepath.Join(stack, "containers", "web", "Dockerfile")
	sql := filepath.Join(stack, "volumes", "db", "init.sql")
	write(t, compose, "services: {}\n")
	write(t, dockerfile, "FROM nginx\n")
	write(t, sql, "select 1;\n")
	d.server = New(d.data, nil, func(context.Context) (string, error) { return "", errors.New("Docker isn't answering") })
	for _, p := range []string{compose, dockerfile, sql} {
		if got := d.read(t, p); got.GetFile() == nil {
			t.Errorf("%s: %v", p, got.GetRefusal())
		}
	}
	for _, e := range d.list(t, stack).GetListing().GetEntries() {
		if e.GetRefused() {
			t.Errorf("%s is marked as refused", e.GetName())
		}
	}
	// Plenty of Docker's names, but no containers, is no root either.
	other := filepath.Join(d.root, "srv", "site")
	write(t, filepath.Join(other, "image", "logo.png.txt"), "x")
	write(t, filepath.Join(other, "network", "hosts"), "x")
	write(t, filepath.Join(other, "volumes", "db", "init.sql"), "x")
	write(t, filepath.Join(other, "index.html"), "x")
	if got := d.read(t, filepath.Join(other, "index.html")); got.GetFile() == nil {
		t.Errorf("a site with no containers: %v", got.GetRefusal())
	}
}

// A real root can keep a part of itself elsewhere, and is still one, which a check
// that doesn't follow links can't see. Each of these holds what a container printed
// or was given.
func TestARootWithPartsElsewhereIsStillOne(t *testing.T) {
	d := newDisk(t)
	root := filepath.Join(d.root, "bigdisk", "docker")
	secrets := []string{
		filepath.Join(root, "containers", "abc", "mounts", "secrets", "db_password"),
		filepath.Join(root, "containers", "abc", "abc-json.log"),
		filepath.Join(root, "overlay2", "abc", "diff", "app", ".env"),
	}
	for _, p := range secrets {
		write(t, p, "hunter2\n")
	}
	write(t, filepath.Join(root, "image", "overlay2", "repositories.json"), "{}")
	write(t, filepath.Join(d.root, "elsewhere", "vols", "web", "_data", "index.html"), "<h1>hello</h1>")
	link(t, filepath.Join(d.root, "elsewhere", "vols"), filepath.Join(root, "volumes"))
	d.server = New(d.data, nil, func(context.Context) (string, error) { return "", errors.New("Docker isn't answering") })
	for _, p := range secrets {
		if got := refusalOf(t, d, p); got.GetKind() != agentv1.FileRefusal_KIND_REFUSED {
			t.Errorf("%s: %v", p, got)
		}
	}
	// With links as the only way it adds up: it's the link that makes it a root.
	sparse := filepath.Join(d.root, "bigdisk", "sparse")
	log := filepath.Join(sparse, "containers", "abc", "abc-json.log")
	write(t, log, "hunter2\n")
	write(t, filepath.Join(sparse, "image", "x"), "x")
	link(t, filepath.Join(d.root, "elsewhere", "vols"), filepath.Join(sparse, "volumes"))
	if got := refusalOf(t, d, log); got.GetKind() != agentv1.FileRefusal_KIND_REFUSED {
		t.Errorf("with volumes a link: %v", got)
	}
	// And with containers the link, what's left of the root is still shut.
	moved := filepath.Join(d.root, "bigdisk", "moved")
	diff := filepath.Join(moved, "overlay2", "abc", "diff", ".env")
	write(t, diff, "hunter2\n")
	write(t, filepath.Join(moved, "image", "x"), "x")
	write(t, filepath.Join(d.root, "elsewhere", "ctrs", "abc", "f"), "x")
	link(t, filepath.Join(d.root, "elsewhere", "ctrs"), filepath.Join(moved, "containers"))
	if got := refusalOf(t, d, diff); got.GetKind() != agentv1.FileRefusal_KIND_REFUSED {
		t.Errorf("with containers a link: %v", got)
	}
}

// A root with its containers elsewhere: they're walked by where they really are.
func TestARootWhoseContainersAreElsewhereIsStillOne(t *testing.T) {
	d := newDisk(t)
	big := filepath.Join(d.root, "big")
	secret := filepath.Join(big, "containers", "abc", "abc-json.log")
	write(t, secret, "hunter2\n")
	write(t, filepath.Join(big, "image", "x"), "x")
	write(t, filepath.Join(big, "overlay2", "x"), "x")
	root := filepath.Join(d.root, "docker2")
	write(t, filepath.Join(root, "volumes", "web", "_data", "x"), "x")
	link(t, filepath.Join(big, "containers"), filepath.Join(root, "containers"))
	d.server = New(d.data, nil, func(context.Context) (string, error) { return "", errors.New("Docker isn't answering") })
	if got := refusalOf(t, d, filepath.Join(root, "containers", "abc", "abc-json.log")); got.GetKind() != agentv1.FileRefusal_KIND_REFUSED {
		t.Errorf("through the link: %v", got)
	}
}
