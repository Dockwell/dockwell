// SPDX-License-Identifier: AGPL-3.0-only

package files

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"golang.org/x/sys/unix"

	agentv1 "github.com/dockwell/dockwell/gen/dockwell/agent/v1"
	dockwellv1 "github.com/dockwell/dockwell/gen/dockwell/v1"
)

// noAccounts keeps a test to the homes it names, whatever this machine's are.
func (d *fakeDisk) noAccounts(homes ...string) {
	var passwd strings.Builder
	for i, h := range homes {
		passwd.WriteString("user" + itoa(i) + ":x:" + itoa(2000+i) + ":" + itoa(2000+i) + "::" + h + ":/bin/sh\n")
	}
	d.server.accounts = func() (string, string) { return passwd.String(), "" }
}

func (d *fakeDisk) make(t *testing.T, p string, kind dockwellv1.FileType, confirmed string) *agentv1.MakeFileResult {
	t.Helper()
	r, err := d.server.Make(t.Context(), p, kind, confirmed)
	if err != nil {
		t.Fatalf("Make(%s): %v", p, err)
	}
	mustSend(t, r)
	return r
}

func (d *fakeDisk) move(t *testing.T, from, to, confirmedFrom, confirmedTo string) *agentv1.MoveFileResult {
	t.Helper()
	r, err := d.server.Move(t.Context(), from, to, confirmedFrom, confirmedTo)
	if err != nil {
		t.Fatalf("Move(%s, %s): %v", from, to, err)
	}
	mustSend(t, r)
	return r
}

func (d *fakeDisk) delete(t *testing.T, p string, forGood bool, confirmed string) *agentv1.DeleteFileResult {
	t.Helper()
	r, err := d.server.Delete(t.Context(), p, forGood, confirmed)
	if err != nil {
		t.Fatalf("Delete(%s): %v", p, err)
	}
	mustSend(t, r)
	return r
}

func there(p string) bool {
	_, err := os.Lstat(p)
	return err == nil
}

func modeOf(t *testing.T, p string) os.FileMode {
	t.Helper()
	info, err := os.Lstat(p)
	if err != nil {
		t.Fatal(err)
	}
	return info.Mode()
}

func TestMakingAFileOrAFolder(t *testing.T) {
	d := newDisk(t)
	d.noAccounts()
	site := filepath.Join(d.root, "site")
	if err := os.MkdirAll(site, 0o755); err != nil {
		t.Fatal(err)
	}
	old := unix.Umask(0o077)
	defer unix.Umask(old)

	made := d.make(t, filepath.Join(site, "index.html"), dockwellv1.FileType_FILE_TYPE_FILE, "").GetMade()
	if made.GetPath() != filepath.Join(site, "index.html") || made.GetFile().GetType() != dockwellv1.FileType_FILE_TYPE_FILE || made.GetWayBackIn() {
		t.Fatalf("made %v", made)
	}
	if mode := modeOf(t, filepath.Join(site, "index.html")); mode.Perm() != 0o644 {
		t.Errorf("a new file has %v, whatever the umask", mode)
	}
	folder := d.make(t, filepath.Join(site, "assets"), dockwellv1.FileType_FILE_TYPE_DIRECTORY, "").GetMade()
	if folder.GetFile().GetType() != dockwellv1.FileType_FILE_TYPE_DIRECTORY {
		t.Fatalf("made %v", folder)
	}
	if mode := modeOf(t, filepath.Join(site, "assets")); mode.Perm() != 0o755 || !mode.IsDir() {
		t.Errorf("a new folder has %v", mode)
	}
	// It never replaces, a file or a folder, or a link that leads nowhere.
	link(t, filepath.Join(d.root, "nowhere"), filepath.Join(site, "dangling"))
	for _, name := range []string{"index.html", "assets", "dangling"} {
		got := d.make(t, filepath.Join(site, name), dockwellv1.FileType_FILE_TYPE_FILE, "").GetRefusal()
		if got.GetKind() != agentv1.FileRefusal_KIND_EXISTS {
			t.Errorf("making %s over what's there: %v", name, got)
		}
	}
	if there(filepath.Join(d.root, "nowhere")) {
		t.Error("a make followed a link that leads nowhere")
	}
	// A path reads where it leads, and the answer says so.
	link(t, site, filepath.Join(d.root, "www"))
	via := d.make(t, filepath.Join(d.root, "www", "about.html"), dockwellv1.FileType_FILE_TYPE_FILE, "").GetMade()
	if via.GetPath() != filepath.Join(site, "about.html") || !there(filepath.Join(site, "about.html")) {
		t.Errorf("made through a link: %v", via)
	}
	// In what isn't a folder, nothing.
	if got := d.make(t, filepath.Join(site, "index.html", "x"), dockwellv1.FileType_FILE_TYPE_FILE, "").GetRefusal(); got.GetKind() != agentv1.FileRefusal_KIND_NOT_A_DIRECTORY {
		t.Errorf("in a file: %v", got)
	}
	// What a read refuses, a make does too.
	for _, p := range []string{filepath.Join(d.data, "control", "evil"), filepath.Join(d.docker, "containers", "x"), filepath.Join(site, ".pgpass")} {
		if got := d.make(t, p, dockwellv1.FileType_FILE_TYPE_FILE, "").GetRefusal(); got.GetKind() != agentv1.FileRefusal_KIND_REFUSED {
			t.Errorf("making %s: %v", p, got)
		}
	}
	for _, p := range []string{"/", filepath.Join(site, strings.Repeat("x", MaxName+1)), "relative"} {
		if _, err := d.server.Make(t.Context(), p, dockwellv1.FileType_FILE_TYPE_FILE, ""); err == nil {
			t.Errorf("making %q goes ahead", p)
		}
	}
	if _, err := d.server.Make(t.Context(), filepath.Join(site, "x"), dockwellv1.FileType_FILE_TYPE_SYMLINK, ""); err == nil {
		t.Error("making a link goes ahead")
	}
}

// Docker's data opens only as far as a volume's files, and only those change.
func TestOnlyAVolumesFilesChange(t *testing.T) {
	d := newDisk(t)
	d.noAccounts()
	data := filepath.Join(d.docker, "volumes", "web", "_data")
	if got := d.make(t, filepath.Join(data, "new.html"), dockwellv1.FileType_FILE_TYPE_FILE, "").GetMade(); got == nil {
		t.Error("can't make a file in a volume")
	}
	for _, p := range []string{filepath.Join(d.docker, "volumes", "web"), data, filepath.Join(d.docker, "volumes", "metadata.db")} {
		if got := d.delete(t, p, false, "").GetRefusal(); got.GetKind() != agentv1.FileRefusal_KIND_REFUSED {
			t.Errorf("deleting %s: %v", p, got)
		}
		if !there(p) {
			t.Errorf("%s went", p)
		}
	}
	if got := d.make(t, filepath.Join(d.docker, "volumes", "fake"), dockwellv1.FileType_FILE_TYPE_DIRECTORY, "").GetRefusal(); got.GetKind() != agentv1.FileRefusal_KIND_REFUSED {
		t.Errorf("making a volume by hand: %v", got)
	}
}

func TestMovingAndRenaming(t *testing.T) {
	d := newDisk(t)
	d.noAccounts()
	site, other := filepath.Join(d.root, "site"), filepath.Join(d.root, "other")
	write(t, filepath.Join(site, "index.html"), "<h1>hi</h1>")
	write(t, filepath.Join(site, "old.html"), "old")
	write(t, filepath.Join(other, "keep.txt"), "keep")

	renamed := d.move(t, filepath.Join(site, "index.html"), filepath.Join(site, "home.html"), "", "").GetMoved()
	if renamed.GetTo() != filepath.Join(site, "home.html") || there(filepath.Join(site, "index.html")) {
		t.Fatalf("renamed: %v", renamed)
	}
	moved := d.move(t, filepath.Join(site, "home.html"), filepath.Join(other, "home.html"), "", "").GetMoved()
	if moved.GetFrom() != filepath.Join(site, "home.html") || moved.GetTo() != filepath.Join(other, "home.html") {
		t.Fatalf("moved: %v", moved)
	}
	if text, _ := os.ReadFile(filepath.Join(other, "home.html")); string(text) != "<h1>hi</h1>" {
		t.Errorf("it holds %q", text)
	}
	// Never over what's there.
	if got := d.move(t, filepath.Join(site, "old.html"), filepath.Join(other, "keep.txt"), "", "").GetRefusal(); got.GetKind() != agentv1.FileRefusal_KIND_EXISTS {
		t.Errorf("over what's there: %v", got)
	}
	if text, _ := os.ReadFile(filepath.Join(other, "keep.txt")); string(text) != "keep" {
		t.Errorf("what was there holds %q", text)
	}
	// A link moves as itself, and what it leads to stays.
	link(t, filepath.Join(other, "keep.txt"), filepath.Join(site, "shortcut"))
	d.move(t, filepath.Join(site, "shortcut"), filepath.Join(site, "renamed-shortcut"), "", "")
	if target, err := os.Readlink(filepath.Join(site, "renamed-shortcut")); err != nil || target != filepath.Join(other, "keep.txt") || !there(filepath.Join(other, "keep.txt")) {
		t.Errorf("the link is %q, %v", target, err)
	}
	// A folder can't go inside itself.
	if got := d.move(t, other, filepath.Join(other, "inside"), "", "").GetRefusal(); got == nil || !strings.Contains(got.GetMessage(), "inside itself") {
		t.Errorf("into itself: %v", got)
	}
	// Into or out of what's refused, nothing.
	if got := d.move(t, filepath.Join(site, "old.html"), filepath.Join(d.data, "old.html"), "", "").GetRefusal(); got.GetKind() != agentv1.FileRefusal_KIND_REFUSED {
		t.Errorf("into the data directory: %v", got)
	}
	if got := d.move(t, d.data, filepath.Join(d.root, "stolen"), "", "").GetRefusal(); got.GetKind() != agentv1.FileRefusal_KIND_REFUSED {
		t.Errorf("the data directory itself: %v", got)
	}
	// Nor what holds it.
	if got := d.move(t, d.root, d.root+"-moved", "", "").GetRefusal(); got.GetKind() != agentv1.FileRefusal_KIND_REFUSED || !strings.Contains(got.GetMessage(), "holds") {
		t.Errorf("what holds the data directory: %v", got)
	}
	if !there(d.data) {
		t.Error("the data directory moved")
	}
	if got := d.move(t, filepath.Join(site, "nothing"), filepath.Join(site, "x"), "", "").GetRefusal(); got.GetKind() != agentv1.FileRefusal_KIND_NOT_FOUND {
		t.Errorf("what isn't there: %v", got)
	}
}

// A folder that is or holds a mount isn't moved or deleted, as that would reach into
// what's mounted.
func TestAMountStaysWhereItIs(t *testing.T) {
	d := newDisk(t)
	d.noAccounts()
	disk := filepath.Join(d.root, "srv", "disk")
	write(t, filepath.Join(disk, "data.db"), "rows")
	d.server.mounts = func() string {
		return "36 35 98:0 / " + disk + " rw,noatime master:1 - ext4 /dev/sdb1 rw\n"
	}
	for _, p := range []string{disk, filepath.Join(d.root, "srv")} {
		if got := d.delete(t, p, true, "").GetRefusal(); got.GetKind() != agentv1.FileRefusal_KIND_REFUSED || !strings.Contains(got.GetMessage(), "mounted") {
			t.Errorf("deleting %s: %v", p, got)
		}
		if got := d.move(t, p, p+"-moved", "", "").GetRefusal(); got.GetKind() != agentv1.FileRefusal_KIND_REFUSED {
			t.Errorf("moving %s: %v", p, got)
		}
	}
	// What's in a mount is a person's files like any others.
	if got := d.delete(t, filepath.Join(disk, "data.db"), false, "").GetDeleted(); got == nil {
		t.Error("can't delete a file on a mount")
	}
}

// What lets someone back in takes a passkey to make, move, delete or put back, for
// where it is with every link followed: at either end of a move, and for a folder,
// whatever home it is or holds.
func TestAWayBackInTakesAPasskeyToChange(t *testing.T) {
	d := newDisk(t)
	home := filepath.Join(d.root, "home", "deploy")
	// One account's home isn't there yet.
	d.noAccounts(home, filepath.Join(d.root, "home", "deploy2"))
	ssh := filepath.Join(d.root, "root", ".ssh")
	keys := filepath.Join(ssh, "authorized_keys")
	write(t, keys, "ssh-ed25519 AAAA admin\n")
	write(t, filepath.Join(home, "notes.txt"), "notes")
	plain := filepath.Join(d.root, "site")
	write(t, filepath.Join(plain, "keys.txt"), "ssh-ed25519 AAAA stranger\n")
	link(t, ssh, filepath.Join(plain, "via"))

	asks := func(what string, r *agentv1.FileRefusal) {
		t.Helper()
		if r.GetKind() != agentv1.FileRefusal_KIND_NEEDS_STEP_UP || !strings.Contains(r.GetMessage(), "takes your passkey") {
			t.Errorf("%s: %v", what, r)
		}
	}
	asks("making in .ssh through a link", d.make(t, filepath.Join(plain, "via", "authorized_keys2"), dockwellv1.FileType_FILE_TYPE_FILE, "").GetRefusal())
	asks("deleting authorized_keys", d.delete(t, keys, false, "").GetRefusal())
	asks("deleting it with a passkey for another path", d.delete(t, keys, false, filepath.Join(plain, "via", "authorized_keys")).GetRefusal())
	asks("renaming a file to authorized_keys", d.move(t, filepath.Join(plain, "keys.txt"), filepath.Join(plain, "authorized_keys"), "", "").GetRefusal())
	asks("moving a file into .ssh", d.move(t, filepath.Join(plain, "keys.txt"), filepath.Join(ssh, "keys.txt"), "", "").GetRefusal())
	asks("moving authorized_keys out", d.move(t, keys, filepath.Join(plain, "old-keys"), "", "").GetRefusal())
	asks("deleting a home", d.delete(t, home, false, "").GetRefusal())
	asks("deleting what holds a home", d.delete(t, filepath.Join(d.root, "home"), false, "").GetRefusal())
	asks("moving a folder in as a home", d.move(t, plain, filepath.Join(d.root, "home", "deploy2"), "", "").GetRefusal())
	if !there(keys) || !there(filepath.Join(plain, "keys.txt")) || !there(home) {
		t.Fatal("something changed with no passkey")
	}
	// A folder in a home is its owner's business, as for a save.
	if got := d.delete(t, filepath.Join(home, "notes.txt"), false, "").GetDeleted(); got == nil || got.GetWayBackIn() {
		t.Errorf("a file in a home: %v", got)
	}
	// With a passkey for where it is, it goes ahead, and says what it was.
	made := d.make(t, filepath.Join(plain, "via", "authorized_keys2"), dockwellv1.FileType_FILE_TYPE_FILE, filepath.Join(ssh, "authorized_keys2")).GetMade()
	if made == nil || !made.GetWayBackIn() || made.GetPath() != filepath.Join(ssh, "authorized_keys2") {
		t.Errorf("made with a passkey: %v", made)
	}
	moved := d.move(t, filepath.Join(plain, "keys.txt"), filepath.Join(ssh, "keys.txt"), filepath.Join(plain, "keys.txt"), filepath.Join(ssh, "keys.txt")).GetMoved()
	if moved == nil || !moved.GetWayBackIn() {
		t.Errorf("moved with a passkey: %v", moved)
	}
	deleted := d.delete(t, keys, false, keys).GetDeleted()
	if deleted == nil || !deleted.GetWayBackIn() || there(keys) {
		t.Fatalf("deleted with a passkey: %v", deleted)
	}
	// Putting it back asks again, for where it goes.
	back, err := d.server.Restore(t.Context(), deleted.GetDeleted().GetId(), "")
	if err != nil {
		t.Fatal(err)
	}
	asks("putting authorized_keys back", back.GetRefusal())
	if back, _ = d.server.Restore(t.Context(), deleted.GetDeleted().GetId(), keys); back.GetRestored() == nil || !back.GetRestored().GetWayBackIn() {
		t.Errorf("put back with a passkey: %v", back)
	}
	if text, _ := os.ReadFile(keys); string(text) != "ssh-ed25519 AAAA admin\n" {
		t.Errorf("put back, it holds %q", text)
	}
}

// A delete goes to Recently deleted, from where it can be put back as it was, for 7
// days, or deleted for good.
func TestRecentlyDeleted(t *testing.T) {
	d := newDisk(t)
	d.noAccounts()
	site := filepath.Join(d.root, "site")
	write(t, filepath.Join(site, "index.html"), "<h1>hi</h1>")
	write(t, filepath.Join(site, "assets", "app.css"), "body{}")
	outside := filepath.Join(d.root, "outside")
	write(t, filepath.Join(outside, "precious.txt"), "precious")
	link(t, outside, filepath.Join(site, "assets", "to-outside"))
	start := time.Date(2026, 11, 7, 12, 0, 0, 0, time.UTC)
	d.server.now = func() time.Time { return start }

	file := d.delete(t, filepath.Join(site, "index.html"), false, "").GetDeleted()
	if file.GetPath() != filepath.Join(site, "index.html") || file.GetDeleted().GetSizeBytes() != 11 || there(filepath.Join(site, "index.html")) {
		t.Fatalf("deleted %v", file)
	}
	if got := file.GetDeleted().GetPurgeTime().AsTime(); !got.Equal(start.Add(KeepDeleted)) {
		t.Errorf("it goes at %v", got)
	}
	d.server.now = func() time.Time { return start.Add(time.Minute) }
	folder := d.delete(t, filepath.Join(site, "assets"), false, "").GetDeleted()
	if folder == nil || there(filepath.Join(site, "assets")) {
		t.Fatalf("deleted %v", folder)
	}
	listed, err := d.server.ListDeleted()
	if err != nil {
		t.Fatal(err)
	}
	if got := listed.GetListing().GetDeleted(); len(got) != 2 || got[0].GetPath() != filepath.Join(site, "assets") || got[0].GetType() != dockwellv1.FileType_FILE_TYPE_DIRECTORY {
		t.Fatalf("Recently deleted, newest first: %v", got)
	}
	// It's out of Files' reach while it's there.
	if got := d.list(t, d.data).GetRefusal(); got.GetKind() != agentv1.FileRefusal_KIND_REFUSED {
		t.Errorf("Recently deleted can be listed: %v", got)
	}

	// Back where it was, as it was, never over what's there now.
	write(t, filepath.Join(site, "index.html"), "a new one")
	busy, _ := d.server.Restore(t.Context(), file.GetDeleted().GetId(), "")
	if busy.GetRefusal().GetKind() != agentv1.FileRefusal_KIND_EXISTS {
		t.Errorf("over what's there now: %v", busy)
	}
	if err := os.Remove(filepath.Join(site, "index.html")); err != nil {
		t.Fatal(err)
	}
	back, _ := d.server.Restore(t.Context(), file.GetDeleted().GetId(), "")
	if back.GetRestored().GetPath() != filepath.Join(site, "index.html") {
		t.Fatalf("put back: %v", back)
	}
	if text, _ := os.ReadFile(filepath.Join(site, "index.html")); string(text) != "<h1>hi</h1>" {
		t.Errorf("put back, it holds %q", text)
	}
	if again, _ := d.server.Restore(t.Context(), file.GetDeleted().GetId(), ""); again.GetRefusal().GetKind() != agentv1.FileRefusal_KIND_NOT_FOUND {
		t.Errorf("put back twice: %v", again)
	}
	for _, id := range []string{"../../etc", strings.Repeat("A", 25)} {
		if _, err := d.server.Restore(t.Context(), id, ""); err == nil {
			t.Errorf("an ID of %q goes ahead", id)
		}
	}

	// Deleted for good from the list, it's gone, and so is all it held, but not what a
	// link in it leads to.
	purged, err := d.server.Purge(folder.GetDeleted().GetId())
	if err != nil || purged.GetPurged().GetPath() != filepath.Join(site, "assets") {
		t.Fatalf("purged %v, %v", purged, err)
	}
	if listed, _ := d.server.ListDeleted(); len(listed.GetListing().GetDeleted()) != 0 {
		t.Errorf("still listed: %v", listed)
	}
	d.server.prune(t.Context())
	if left := deletedEntries(t, d); len(left) != 0 {
		t.Errorf("Recently deleted holds %v", left)
	}
	if text, _ := os.ReadFile(filepath.Join(outside, "precious.txt")); string(text) != "precious" {
		t.Errorf("what a link in it leads to holds %q", text)
	}

	// After 7 days it goes by itself.
	kept := d.delete(t, filepath.Join(site, "index.html"), false, "").GetDeleted()
	d.server.prune(t.Context())
	if listed, _ := d.server.ListDeleted(); len(listed.GetListing().GetDeleted()) != 1 {
		t.Fatalf("pruned too soon: %v", listed)
	}
	d.server.now = func() time.Time { return start.Add(KeepDeleted + time.Hour) }
	d.server.prune(t.Context())
	if listed, _ := d.server.ListDeleted(); len(listed.GetListing().GetDeleted()) != 0 {
		t.Errorf("kept past 7 days: %v", listed)
	}
	if back, _ := d.server.Restore(t.Context(), kept.GetDeleted().GetId(), ""); back.GetRefusal().GetKind() != agentv1.FileRefusal_KIND_NOT_FOUND {
		t.Errorf("put back after 7 days: %v", back)
	}
}

func deletedEntries(t *testing.T, d *fakeDisk) []string {
	t.Helper()
	entries, err := os.ReadDir(d.server.deletedDir())
	if err != nil {
		t.Fatal(err)
	}
	var out []string
	for _, e := range entries {
		out = append(out, e.Name())
	}
	return out
}

// Deleted for good, it's gone at once, however large, and removed after.
func TestDeletingForGood(t *testing.T) {
	d := newDisk(t)
	d.noAccounts()
	site := filepath.Join(d.root, "site")
	write(t, filepath.Join(site, "big", "a", "b", "c.log"), "lines")
	write(t, filepath.Join(site, "one.log"), "lines")
	outside := filepath.Join(d.root, "outside")
	write(t, filepath.Join(outside, "precious.txt"), "precious")
	link(t, outside, filepath.Join(site, "big", "to-outside"))
	link(t, filepath.Join(outside, "precious.txt"), filepath.Join(site, "a-link"))

	for _, name := range []string{"big", "one.log", "a-link"} {
		got := d.delete(t, filepath.Join(site, name), true, "").GetDeleted()
		if got == nil || got.GetDeleted() != nil || there(filepath.Join(site, name)) {
			t.Fatalf("deleting %s for good: %v", name, got)
		}
	}
	if listed, _ := d.server.ListDeleted(); len(listed.GetListing().GetDeleted()) != 0 {
		t.Errorf("what's deleted for good is listed: %v", listed)
	}
	d.server.prune(t.Context())
	if left := deletedEntries(t, d); len(left) != 0 {
		t.Errorf("Recently deleted holds %v", left)
	}
	if text, _ := os.ReadFile(filepath.Join(outside, "precious.txt")); string(text) != "precious" {
		t.Errorf("what a link led to holds %q", text)
	}
}

// What's on another filesystem than Dockwell's data can't be put back, so a delete
// says so, and deleting it for good takes it out of the way at once. Here Dockwell's
// data is what's elsewhere, as it's the one place a test can put on another disk.
func TestWhatsOnAnotherDiskIsDeletedForGood(t *testing.T) {
	d := newDisk(t)
	d.noAccounts()
	elsewhere, err := os.MkdirTemp("/dev/shm", "dockwell-test-")
	if err != nil {
		t.Skipf("no /dev/shm here: %v", err)
	}
	t.Cleanup(func() { _ = os.RemoveAll(elsewhere) })
	var a, b unix.Stat_t
	if unix.Stat(elsewhere, &a) != nil || unix.Stat(d.root, &b) != nil || a.Dev == b.Dev {
		t.Skip("/dev/shm is on the same filesystem as the test's folder here")
	}
	d.server = New(elsewhere, nil, func(context.Context) (string, error) { return d.docker, nil })
	d.noAccounts()
	site := filepath.Join(d.root, "site")
	write(t, filepath.Join(site, "index.html"), "hi")
	write(t, filepath.Join(site, "cache", "a", "x"), "x")

	got := d.delete(t, filepath.Join(site, "index.html"), false, "").GetRefusal()
	if got.GetKind() != agentv1.FileRefusal_KIND_FOR_GOOD || !strings.Contains(got.GetMessage(), "can't be put back") || !there(filepath.Join(site, "index.html")) {
		t.Fatalf("a delete on another disk: %v", got)
	}
	if left := deletedEntries(t, d); len(left) != 0 {
		t.Errorf("a delete that didn't happen left %v", left)
	}
	if d.delete(t, filepath.Join(site, "index.html"), true, "").GetDeleted() == nil || there(filepath.Join(site, "index.html")) {
		t.Fatal("a file wasn't deleted for good")
	}
	if d.delete(t, filepath.Join(site, "cache"), true, "").GetDeleted() == nil || there(filepath.Join(site, "cache")) {
		t.Fatal("a folder wasn't deleted for good")
	}
	// It waits beside where it was, out of the way, until it's removed.
	if waiting, _ := filepath.Glob(filepath.Join(site, asidePrefix+"*")); len(waiting) != 1 {
		t.Errorf("waiting to be removed: %v", waiting)
	}
	d.server.prune(t.Context())
	if left, _ := os.ReadDir(site); len(left) != 0 {
		t.Errorf("left beside it: %v", left)
	}
	if left := deletedEntries(t, d); len(left) != 0 {
		t.Errorf("Recently deleted holds %v", left)
	}
}

// Something else in its place by the time it's removed stays: only what was deleted goes.
func TestWhatTakesTheDeletedsPlaceStays(t *testing.T) {
	d := newDisk(t)
	d.noAccounts()
	site := filepath.Join(d.root, "site")
	write(t, filepath.Join(site, "precious", "x"), "precious")
	if err := os.MkdirAll(d.server.deletedDir(), 0o700); err != nil {
		t.Fatal(err)
	}
	var st unix.Stat_t
	if err := unix.Stat(filepath.Join(site, "precious"), &st); err != nil {
		t.Fatal(err)
	}
	// A note says a folder waits beside site to be removed, and what's there now is
	// something else.
	aside := filepath.Join(site, asidePrefix+"X")
	if err := os.Rename(filepath.Join(site, "precious"), aside); err != nil {
		t.Fatal(err)
	}
	id := strings.Repeat("A", 26)
	root, err := d.server.openDeleted()
	if err != nil {
		t.Fatal(err)
	}
	defer unix.Close(root)
	entryDir, refusal := newEntry(root, id, &about{Path: site, ForGood: true, Aside: aside, Dev: st.Dev, Ino: st.Ino + 1}, site)
	if refusal != nil {
		t.Fatal(refusal)
	}
	_ = unix.Close(entryDir)
	d.server.prune(t.Context())
	if text, _ := os.ReadFile(filepath.Join(aside, "x")); string(text) != "precious" {
		t.Errorf("what took its place holds %q", text)
	}
	if left := deletedEntries(t, d); len(left) != 0 {
		t.Errorf("Recently deleted holds %v", left)
	}
}

// A move across disks says so, as a rename can't make one.
func TestAMoveToAnotherDiskSaysSo(t *testing.T) {
	src, dst := &target{where: "/srv/site/a.txt"}, &target{where: "/mnt/disk/a.txt"}
	if got := unmoved(src, dst, unix.EXDEV); got.GetKind() != agentv1.FileRefusal_KIND_OTHER_DISK || !strings.Contains(got.GetMessage(), "different disks") {
		t.Errorf("= %v", got)
	}
}

// A path that stops answering as a delete waits on it stops the delete before
// anything changes, and says nothing did.
func TestADeleteThatWaitsOnAPathSaysNothingChanged(t *testing.T) {
	d := newDisk(t)
	d.noAccounts()
	stuck := filepath.Join(d.root, "nfs", "file")
	write(t, stuck, "x")
	release := make(chan struct{})
	defer close(release)
	d.server.AnswerWithin = 50 * time.Millisecond
	d.server.Touching = func(p string) {
		if p == stuck {
			<-release
		}
	}
	got, err := d.server.Delete(context.Background(), stuck, false, "")
	if err != nil {
		t.Fatal(err)
	}
	if r := got.GetRefusal(); r.GetKind() != agentv1.FileRefusal_KIND_NOT_ANSWERING || !strings.HasSuffix(r.GetMessage(), "Nothing changed") {
		t.Errorf("= %v", r)
	}
}
