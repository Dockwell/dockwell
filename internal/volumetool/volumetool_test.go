// SPDX-License-Identifier: AGPL-3.0-only

package volumetool

import (
	"archive/tar"
	"bytes"
	"compress/gzip"
	"errors"
	"io"
	"os"
	"path/filepath"
	"strings"
	"syscall"
	"testing"
	"time"

	"golang.org/x/sys/unix"
)

func write(t *testing.T, path, content string, mode os.FileMode) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte(content), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.Chmod(path, mode); err != nil {
		t.Fatal(err)
	}
}

func pack(t *testing.T, dir string) []byte {
	t.Helper()
	var out bytes.Buffer
	if _, err := Pack(&out, dir, nil); err != nil {
		t.Fatalf("Pack: %v", err)
	}
	return out.Bytes()
}

// chowned records what a restore would give each file, which only root can.
func chowned(got map[string][2]int, base *string) hooks {
	return hooks{lchown: func(path string, uid, gid int) error {
		rel, err := filepath.Rel(*base, path)
		if err != nil {
			return err
		}
		got[rel] = [2]int{uid, gid}
		return nil
	}}
}

// A restore brings back what a volume is, as well as what its files say.
func TestARestoreBringsBackWhatTheVolumeWas(t *testing.T) {
	src, dst := t.TempDir(), t.TempDir()
	at := time.Date(2026, 3, 1, 12, 30, 15, 123456789, time.UTC)
	write(t, filepath.Join(src, "plain.txt"), "hello", 0o644)
	write(t, filepath.Join(src, "secret", "key.pem"), "private", 0o600)
	write(t, filepath.Join(src, "bin", "run"), "#!/bin/sh\n", 0o755)
	write(t, filepath.Join(src, "bin", "setuid"), "x", 0o755|os.ModeSetuid)
	write(t, filepath.Join(src, "big.bin"), strings.Repeat("0123456789abcdef", 1<<16), 0o640)
	for _, p := range []string{"plain.txt", "secret/key.pem"} {
		if err := os.Chtimes(filepath.Join(src, p), at, at); err != nil {
			t.Fatal(err)
		}
	}
	if err := os.Mkdir(filepath.Join(src, "empty"), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.Mkdir(filepath.Join(src, "shared"), 0o777); err != nil {
		t.Fatal(err)
	}
	if err := os.Chmod(filepath.Join(src, "shared"), 0o777|os.ModeSticky); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink("plain.txt", filepath.Join(src, "link")); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink("/etc/shadow", filepath.Join(src, "outward")); err != nil {
		t.Fatal(err)
	}
	if err := os.Link(filepath.Join(src, "plain.txt"), filepath.Join(src, "secret", "hard")); err != nil {
		t.Fatal(err)
	}
	if err := syscall.Mkfifo(filepath.Join(src, "pipe"), 0o640); err != nil {
		t.Fatal(err)
	}
	if err := os.Chmod(src, 0o750); err != nil {
		t.Fatal(err)
	}
	xattrs := unix.Setxattr(filepath.Join(src, "plain.txt"), "user.dockwell", []byte("kept\x00binary"), 0) == nil
	if err := os.Chtimes(filepath.Join(src, "secret"), at, at); err != nil {
		t.Fatal(err)
	}

	write(t, filepath.Join(dst, "stale", "gone.txt"), "from before", 0o644)
	stats, err := Unpack(bytes.NewReader(pack(t, src)), dst, nil)
	if err != nil {
		t.Fatalf("Unpack: %v", err)
	}
	if stats.Bytes == 0 || stats.Files < 10 || !stats.Done {
		t.Errorf("stats = %+v", stats)
	}

	if _, err := os.Lstat(filepath.Join(dst, "stale")); !errors.Is(err, os.ErrNotExist) {
		t.Errorf("what was in the volume before is still there: %v", err)
	}
	for _, leftover := range []string{stagingDir, oldDir} {
		if _, err := os.Lstat(filepath.Join(dst, leftover)); !errors.Is(err, os.ErrNotExist) {
			t.Errorf("%s stayed behind: %v", leftover, err)
		}
	}
	for _, c := range []struct {
		name string
		mode os.FileMode
	}{
		{"plain.txt", 0o644},
		{"secret/key.pem", 0o600},
		{"bin/run", 0o755},
		{"bin/setuid", 0o755 | os.ModeSetuid},
		{"empty", 0o700 | os.ModeDir},
		{"shared", 0o777 | os.ModeDir | os.ModeSticky},
		{"pipe", 0o640 | os.ModeNamedPipe},
	} {
		fi, err := os.Lstat(filepath.Join(dst, c.name))
		if err != nil {
			t.Errorf("%s: %v", c.name, err)
			continue
		}
		if fi.Mode() != c.mode {
			t.Errorf("%s has mode %v, want %v", c.name, fi.Mode(), c.mode)
		}
	}
	if b, err := os.ReadFile(filepath.Join(dst, "big.bin")); err != nil || len(b) != 16<<16 {
		t.Errorf("big.bin came back as %d bytes: %v", len(b), err)
	}
	for name, want := range map[string]string{"link": "plain.txt", "outward": "/etc/shadow"} {
		if got, err := os.Readlink(filepath.Join(dst, name)); err != nil || got != want {
			t.Errorf("%s leads to %q, %v, want %q", name, got, err, want)
		}
	}
	a, _ := os.Stat(filepath.Join(dst, "plain.txt"))
	b, _ := os.Stat(filepath.Join(dst, "secret", "hard"))
	if !os.SameFile(a, b) {
		t.Error("a hard link came back as two files")
	}
	if fi, _ := os.Stat(filepath.Join(dst, "secret", "key.pem")); !fi.ModTime().Equal(at) {
		t.Errorf("a file's time came back as %v, want %v", fi.ModTime(), at)
	}
	if fi, _ := os.Stat(filepath.Join(dst, "secret")); !fi.ModTime().Equal(at) {
		t.Errorf("a directory's time came back as %v, want %v", fi.ModTime(), at)
	}
	if fi, _ := os.Stat(dst); fi.Mode().Perm() != 0o750 {
		t.Errorf("the volume's own directory has mode %v, want 0750", fi.Mode().Perm())
	}
	if xattrs {
		buf := make([]byte, 64)
		n, err := unix.Getxattr(filepath.Join(dst, "plain.txt"), "user.dockwell", buf)
		if err != nil || string(buf[:n]) != "kept\x00binary" {
			t.Errorf("an extended attribute came back as %q, %v", buf[:n], err)
		}
	}
}

// Owners travel as numbers, and each is set as it was: the numbers mean what they
// mean to the app, whose users the server may not have.
func TestOwnersTravelAsNumbers(t *testing.T) {
	src := t.TempDir()
	write(t, filepath.Join(src, "data", "file"), "x", 0o600)
	uid, gid := os.Getuid(), os.Getgid()
	tr := tar.NewReader(mustGunzip(t, pack(t, src)))
	seen := 0
	for {
		hdr, err := tr.Next()
		if errors.Is(err, io.EOF) {
			break
		}
		if err != nil {
			t.Fatal(err)
		}
		seen++
		if hdr.Uid != uid || hdr.Gid != gid || hdr.Uname != "" || hdr.Gname != "" {
			t.Errorf("%s: uid %d gid %d uname %q gname %q", hdr.Name, hdr.Uid, hdr.Gid, hdr.Uname, hdr.Gname)
		}
	}
	if seen != 3 {
		t.Errorf("the archive holds %d entries, want the volume, a directory and a file", seen)
	}

	// An archive that says uid 999 gives uid 999.
	var archive bytes.Buffer
	gz := gzip.NewWriter(&archive)
	tw := tar.NewWriter(gz)
	for _, hdr := range []*tar.Header{
		{Name: "./", Typeflag: tar.TypeDir, Mode: 0o700, Uid: 999, Gid: 999, Format: tar.FormatPAX},
		{Name: "pg_hba.conf", Typeflag: tar.TypeReg, Mode: 0o600, Uid: 999, Gid: 998, Size: 1, Format: tar.FormatPAX},
	} {
		if err := tw.WriteHeader(hdr); err != nil {
			t.Fatal(err)
		}
		if hdr.Size > 0 {
			if _, err := tw.Write([]byte("x")); err != nil {
				t.Fatal(err)
			}
		}
	}
	if err := tw.Close(); err != nil {
		t.Fatal(err)
	}
	if err := gz.Close(); err != nil {
		t.Fatal(err)
	}
	dst := t.TempDir()
	got := map[string][2]int{}
	base := filepath.Join(dst, stagingDir)
	if _, err := unpack(&archive, dst, nil, chowned(got, &base)); err != nil {
		t.Fatal(err)
	}
	if got["pg_hba.conf"] != [2]int{999, 998} {
		t.Errorf("a file got owner %v", got["pg_hba.conf"])
	}
	if got[".."] != [2]int{999, 999} {
		t.Errorf("the volume got owner %v, in %v", got[".."], got)
	}
}

func mustGunzip(t *testing.T, b []byte) io.Reader {
	t.Helper()
	gz, err := gzip.NewReader(bytes.NewReader(b))
	if err != nil {
		t.Fatal(err)
	}
	return gz
}

// A restore that can't finish leaves the volume as it was, whatever is wrong
// with the archive.
func TestABadArchiveLeavesTheVolumeAsItWas(t *testing.T) {
	src := t.TempDir()
	write(t, filepath.Join(src, "new.txt"), strings.Repeat("new data", 4096), 0o644)
	good := pack(t, src)

	damaged := bytes.Clone(good)
	damaged[len(damaged)/2] ^= 0xff
	damagedTail := bytes.Clone(good)
	damagedTail[len(damagedTail)-6] ^= 0xff // the gzip's check, past the tar's end

	for name, archive := range map[string][]byte{
		"empty":          {},
		"not an archive": []byte("this is not gzip"),
		"cut short":      good[:len(good)/2],
		"cut at the end": good[:len(good)-4],
		"damaged":        damaged,
		"bad checksum":   damagedTail,
	} {
		t.Run(name, func(t *testing.T) {
			dst := t.TempDir()
			write(t, filepath.Join(dst, "keep", "me.txt"), "still here", 0o644)
			if _, err := Unpack(bytes.NewReader(archive), dst, nil); err == nil {
				t.Fatal("Unpack took a bad archive")
			}
			if b, err := os.ReadFile(filepath.Join(dst, "keep", "me.txt")); err != nil || string(b) != "still here" {
				t.Errorf("the volume changed: %q, %v", b, err)
			}
			if entries, _ := os.ReadDir(dst); len(entries) != 1 {
				t.Errorf("it left things behind: %v", entries)
			}
			if _, err := Verify(bytes.NewReader(archive)); err == nil {
				t.Error("Verify took a bad archive")
			}
		})
	}
	if stats, err := Verify(bytes.NewReader(good)); err != nil || stats.Files != 1 || !stats.Done {
		t.Errorf("Verify of a good archive = %+v, %v", stats, err)
	}
}

func TestAnArchiveCantReachOutsideTheVolume(t *testing.T) {
	for _, hdr := range []*tar.Header{
		{Name: "../outside", Typeflag: tar.TypeReg, Mode: 0o644},
		{Name: "/etc/outside", Typeflag: tar.TypeReg, Mode: 0o644},
		{Name: "up/../../outside", Typeflag: tar.TypeReg, Mode: 0o644},
		{Name: "hard", Typeflag: tar.TypeLink, Linkname: "../../etc/passwd", Mode: 0o644},
	} {
		t.Run(hdr.Name, func(t *testing.T) {
			var archive bytes.Buffer
			gz := gzip.NewWriter(&archive)
			tw := tar.NewWriter(gz)
			if err := tw.WriteHeader(&tar.Header{Name: "./", Typeflag: tar.TypeDir, Mode: 0o755}); err != nil {
				t.Fatal(err)
			}
			if err := tw.WriteHeader(hdr); err != nil {
				t.Fatal(err)
			}
			_ = tw.Close()
			_ = gz.Close()
			parent := t.TempDir()
			dst := filepath.Join(parent, "volume")
			if err := os.Mkdir(dst, 0o755); err != nil {
				t.Fatal(err)
			}
			if _, err := Unpack(&archive, dst, nil); err == nil {
				t.Error("it unpacked outside the volume")
			}
			if entries, _ := os.ReadDir(parent); len(entries) != 1 {
				t.Errorf("something landed beside the volume: %v", entries)
			}
		})
	}
}

// An entry can't go through a symlink an earlier entry made, as root would write
// wherever it leads.
func TestAnArchiveCantGoThroughALink(t *testing.T) {
	var archive bytes.Buffer
	gz := gzip.NewWriter(&archive)
	tw := tar.NewWriter(gz)
	outside := t.TempDir()
	for _, hdr := range []*tar.Header{
		{Name: "./", Typeflag: tar.TypeDir, Mode: 0o755},
		{Name: "away", Typeflag: tar.TypeSymlink, Linkname: outside, Mode: 0o777},
		{Name: "away/planted", Typeflag: tar.TypeReg, Mode: 0o644},
	} {
		if err := tw.WriteHeader(hdr); err != nil {
			t.Fatal(err)
		}
	}
	_ = tw.Close()
	_ = gz.Close()
	if _, err := Unpack(&archive, t.TempDir(), nil); err == nil {
		t.Error("it wrote through a symlink")
	}
	if entries, _ := os.ReadDir(outside); len(entries) != 0 {
		t.Errorf("something landed where the link leads: %v", entries)
	}
}

// A restore that was cut short and runs again finishes the job, and what it moved
// aside goes, as the copy taken before it holds that.
func TestARestoreRunsAgainOverWhatACrashLeft(t *testing.T) {
	src := t.TempDir()
	write(t, filepath.Join(src, "new.txt"), "new", 0o644)
	archive := pack(t, src)

	dst := t.TempDir()
	write(t, filepath.Join(dst, oldDir, "moved.txt"), "moved aside", 0o644)
	write(t, filepath.Join(dst, stagingDir, "half.txt"), "half unpacked", 0o644)
	write(t, filepath.Join(dst, "not-yet-moved.txt"), "still here", 0o644)
	if _, err := Unpack(bytes.NewReader(archive), dst, nil); err != nil {
		t.Fatal(err)
	}
	entries, err := os.ReadDir(dst)
	if err != nil {
		t.Fatal(err)
	}
	if len(entries) != 1 || entries[0].Name() != "new.txt" {
		t.Errorf("the volume holds %v, want only new.txt", entries)
	}
}

// A volume a restore left in two places can't be backed up as if it were whole.
func TestABackupRefusesAVolumeInTwoPlaces(t *testing.T) {
	dir := t.TempDir()
	write(t, filepath.Join(dir, oldDir, "moved.txt"), "moved aside", 0o644)
	if _, err := Pack(io.Discard, dir, nil); err == nil {
		t.Error("it backed up half a volume")
	}
}

// What a restore left of itself is never in a backup.
func TestABackupLeavesOutWhatARestoreLeft(t *testing.T) {
	dir := t.TempDir()
	write(t, filepath.Join(dir, stagingDir, "half.txt"), "half", 0o644)
	write(t, filepath.Join(dir, "real.txt"), "real", 0o644)
	stats, err := Pack(io.Discard, dir, nil)
	if err != nil {
		t.Fatal(err)
	}
	if stats.Files != 2 { // the volume, and real.txt
		t.Errorf("it took %d entries", stats.Files)
	}
}

// A file that goes or changes during a copy is what a running app does, so it's
// counted, and the copy goes on. Only what can't be read stops it.
func TestWhatChangesDuringACopyIsCounted(t *testing.T) {
	dir := t.TempDir()
	write(t, filepath.Join(dir, "shrinks"), "0123456789", 0o644)
	var out bytes.Buffer
	gz := gzip.NewWriter(&out)
	p := &packer{root: dir, tw: tar.NewWriter(gz), seen: map[inode]string{}}
	if err := p.add("gone-already"); err != nil {
		t.Fatalf("a file that went stopped the copy: %v", err)
	}
	f, err := os.Open(filepath.Join(dir, "shrinks"))
	if err != nil {
		t.Fatal(err)
	}
	defer f.Close()
	fi, err := f.Stat()
	if err != nil {
		t.Fatal(err)
	}
	if err := p.tw.WriteHeader(&tar.Header{Name: "shrinks", Typeflag: tar.TypeReg, Mode: 0o644, Size: fi.Size()}); err != nil {
		t.Fatal(err)
	}
	if err := os.Truncate(filepath.Join(dir, "shrinks"), 4); err != nil {
		t.Fatal(err)
	}
	if err := p.copyFile("shrinks", f, fi); err != nil {
		t.Fatalf("a file that got shorter stopped the copy: %v", err)
	}
	if err := p.tw.Close(); err != nil {
		t.Fatalf("the entry wasn't padded to its size: %v", err)
	}
	if p.stats.Changed != 2 {
		t.Errorf("Changed = %d, want 2", p.stats.Changed)
	}
}

func TestEmptyLeavesTheVolumeEmpty(t *testing.T) {
	dir := t.TempDir()
	write(t, filepath.Join(dir, "a", "b", "c"), "x", 0o444)
	write(t, filepath.Join(dir, ".hidden"), "x", 0o644)
	if err := Empty(dir); err != nil {
		t.Fatal(err)
	}
	if entries, _ := os.ReadDir(dir); len(entries) != 0 {
		t.Errorf("it left %v", entries)
	}
}

func TestMainPacksAndUnpacks(t *testing.T) {
	src, dst := t.TempDir(), t.TempDir()
	write(t, filepath.Join(src, "file"), "through the command", 0o644)
	var archive, errs bytes.Buffer
	if code := Main([]string{"pack", src}, nil, &archive, &errs); code != 0 {
		t.Fatalf("pack exited %d: %s", code, errs.String())
	}
	if !strings.Contains(errs.String(), `"done":true`) {
		t.Errorf("pack's last line = %q", errs.String())
	}
	errs.Reset()
	if code := Main([]string{"unpack", dst}, &archive, io.Discard, &errs); code != 0 {
		t.Fatalf("unpack exited %d: %s", code, errs.String())
	}
	if b, _ := os.ReadFile(filepath.Join(dst, "file")); string(b) != "through the command" {
		t.Errorf("file = %q", b)
	}
	errs.Reset()
	if code := Main([]string{"unpack", dst}, strings.NewReader("junk"), io.Discard, &errs); code != 1 || !strings.Contains(errs.String(), "doesn't open") {
		t.Errorf("a bad archive: exit %d, %q", code, errs.String())
	}
	if code := Main([]string{"nonsense"}, nil, io.Discard, io.Discard); code != 2 {
		t.Errorf("no directory: exit %d", code)
	}
}
