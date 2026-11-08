// SPDX-License-Identifier: AGPL-3.0-only

package container

import (
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"syscall"
	"testing"

	resourcev1 "github.com/dockwell/dockwell/gen/dockwell/resource/v1alpha1"
	"github.com/dockwell/dockwell/internal/resource"
)

func TestAFolderIsMadeAsItsMountSays(t *testing.T) {
	uid, gid := uint32(os.Getuid()), uint32(os.Getgid())
	root := t.TempDir()
	made := filepath.Join(root, "var", "www", "shop")
	if err := (HostFolders{}).Make(made, uid, gid); err != nil {
		t.Fatalf("making %s: %v", made, err)
	}
	st, err := os.Stat(made)
	if err != nil || !st.IsDir() || st.Mode().Perm() != 0o755 {
		t.Fatalf("made %v, %v; want a folder with mode 0755", st, err)
	}
	if sys := st.Sys().(*syscall.Stat_t); sys.Uid != uid || sys.Gid != gid {
		t.Errorf("made it %d:%d, want %d:%d", sys.Uid, sys.Gid, uid, gid)
	}

	// One that's there keeps what it has.
	there := filepath.Join(root, "there")
	if err := os.Mkdir(there, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := (HostFolders{}).Make(there, uid, gid); err != nil {
		t.Fatal(err)
	}
	if st, _ := os.Stat(there); st.Mode().Perm() != 0o700 {
		t.Errorf("a folder that was there is %v now, want it as it was", st.Mode().Perm())
	}
}

// modeAndOwner is a file's mode and owner, to see that nothing touched it.
func modeAndOwner(t *testing.T, path string) string {
	t.Helper()
	st, err := os.Lstat(path)
	if err != nil {
		t.Fatal(err)
	}
	sys := st.Sys().(*syscall.Stat_t)
	return st.Mode().String() + " " + resource.FormatOwner(sys.Uid, sys.Gid)
}

// Whoever can write the folder above can swap the folder the agent just made for a
// link, to have the agent, as root, give a file elsewhere away. It goes by handle,
// so the file the link leads to stays as it was, and it says why it stopped.
func TestASwappedFolderGivesNothingAway(t *testing.T) {
	root := t.TempDir()
	parent := filepath.Join(root, "home-alice")
	if err := os.Mkdir(parent, 0o755); err != nil {
		t.Fatal(err)
	}
	victim := filepath.Join(root, "shadow")
	if err := os.WriteFile(victim, []byte("secret"), 0o600); err != nil {
		t.Fatal(err)
	}
	before := modeAndOwner(t, victim)
	site := filepath.Join(parent, "www")
	uid, gid := uint32(os.Getuid()), uint32(os.Getgid())

	// A link swapped in between making the folder and giving it its owner.
	madeHook = func(path string) {
		if err := os.Rename(path, path+"-moved"); err != nil {
			t.Error(err)
		}
		if err := os.Symlink(victim, path); err != nil {
			t.Error(err)
		}
	}
	defer func() { madeHook = nil }()
	err := (HostFolders{}).Make(site, uid, gid)
	if err == nil || !strings.Contains(err.Error(), "took its place") {
		t.Errorf("Make with a link swapped in = %v, want it to stop and say so", err)
	}
	if got := modeAndOwner(t, victim); got != before {
		t.Errorf("the file the link leads to is %s now, was %s", got, before)
	}

	// Another folder swapped in, with something in it, is left as it is too.
	if err := os.Remove(site); err != nil {
		t.Fatal(err)
	}
	theirs := filepath.Join(root, "theirs")
	if err := os.MkdirAll(filepath.Join(theirs, "keep"), 0o700); err != nil {
		t.Fatal(err)
	}
	madeHook = func(path string) {
		_ = os.Remove(path)
		if err := os.Rename(theirs, path); err != nil {
			t.Error(err)
		}
	}
	if err := (HostFolders{}).Make(site, uid, gid); err == nil || !strings.Contains(err.Error(), "took its place") {
		t.Errorf("Make with another folder swapped in = %v", err)
	}
	if st, _ := os.Stat(site); st.Mode().Perm() != 0o700 {
		t.Errorf("the folder swapped in is %v now, want it as it was", st.Mode().Perm())
	}
}

// The race the review found, run as it ran it: a racer swaps the new folder for a
// link as fast as it can. The file the link leads to is never touched.
func TestARacerSwappingInALinkWinsNothing(t *testing.T) {
	dir := t.TempDir()
	parent := filepath.Join(dir, "home-alice")
	if err := os.Mkdir(parent, 0o755); err != nil {
		t.Fatal(err)
	}
	victim := filepath.Join(dir, "shadow")
	site := filepath.Join(parent, "www")
	moved := filepath.Join(parent, "www-moved")
	won, tries := 0, 2000
	for range tries {
		_ = os.Remove(site)
		_ = os.RemoveAll(moved)
		if err := os.WriteFile(victim, []byte("secret"), 0o600); err != nil {
			t.Fatal(err)
		}
		_ = os.Chmod(victim, 0o600)
		stop, done := make(chan struct{}), make(chan struct{})
		go func() {
			runtime.LockOSThread()
			defer close(done)
			for {
				select {
				case <-stop:
					return
				default:
				}
				if os.Rename(site, moved) == nil {
					_ = os.Symlink(victim, site)
					return
				}
			}
		}()
		_ = (HostFolders{}).Make(site, uint32(os.Getuid()), uint32(os.Getgid()))
		close(stop)
		<-done
		if st, _ := os.Stat(victim); st.Mode().Perm() != 0o600 {
			won++
		}
	}
	if won > 0 {
		t.Errorf("the racer changed the file the link leads to in %d of %d tries", won, tries)
	}
}

// A link on the way to the folder is refused, so nothing is made where it leads.
func TestALinkOnTheWayIsNotFollowed(t *testing.T) {
	root := t.TempDir()
	elsewhere := filepath.Join(root, "etc")
	if err := os.Mkdir(elsewhere, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(elsewhere, filepath.Join(root, "sites")); err != nil {
		t.Fatal(err)
	}
	err := (HostFolders{}).Make(filepath.Join(root, "sites", "www"), uint32(os.Getuid()), uint32(os.Getgid()))
	if err == nil || !strings.Contains(err.Error(), "is a link or a file, not a folder") {
		t.Errorf("Make through a link = %v", err)
	}
	if _, err := os.Lstat(filepath.Join(elsewhere, "www")); !os.IsNotExist(err) {
		t.Errorf("Make made a folder where the link leads: %v", err)
	}
}

type folders struct{ made []string }

func (f *folders) Make(path string, uid, gid uint32) error {
	f.made = append(f.made, path+" "+resource.FormatOwner(uid, gid))
	return nil
}

func TestOnlyAServerPathWithAnOwnerIsMade(t *testing.T) {
	f := &folders{}
	d := &Docker{Folders: f}
	spec := &resourcev1.Container{Mounts: []*resourcev1.Mount{
		{Source: &resourcev1.Mount_HostPath{HostPath: "/srv/a"}, Target: "/a", MakeOwner: "82:82"},
		{Source: &resourcev1.Mount_HostPath{HostPath: "/srv/b"}, Target: "/b"},
		{Source: &resourcev1.Mount_Volume{Volume: "data"}, Target: "/data"},
	}}
	if err := d.makeFolders(spec); err != nil {
		t.Fatal(err)
	}
	if len(f.made) != 1 || f.made[0] != "/srv/a 82:82" {
		t.Errorf("made %v, want /srv/a for 82:82 alone", f.made)
	}
	if err := (&Docker{}).makeFolders(spec); err != nil {
		t.Errorf("with no maker, %v", err)
	}
}

func TestAnOwner(t *testing.T) {
	for _, tc := range []struct{ in, want string }{{"82:82", "82:82"}, {"0082:082", "82:82"}, {" 1000:1000 ", "1000:1000"}, {"0:0", "0:0"}} {
		in, want := tc.in, tc.want
		uid, gid, err := resource.ParseOwner(in)
		if err != nil || resource.FormatOwner(uid, gid) != want {
			t.Errorf("resource.ParseOwner(%q) = %d:%d, %v; want %s", in, uid, gid, err, want)
		}
	}
	for _, in := range []string{"82", "www-data:www-data", "-1:82", "+82:82", "82:", ":82", "4294967295:0", "99999999999:0"} {
		if _, _, err := resource.ParseOwner(in); err == nil {
			t.Errorf("resource.ParseOwner(%q) took it", in)
		}
	}
	spec := &resourcev1.Container{Image: "nginx:1.27", Mounts: []*resourcev1.Mount{{Source: &resourcev1.Mount_Volume{Volume: "data"}, Target: "/data", MakeOwner: "82:82"}}}
	if err := validateMounts(spec); err == nil {
		t.Errorf("a volume with a make_owner was taken")
	}
	spec = &resourcev1.Container{Image: "nginx:1.27", Mounts: []*resourcev1.Mount{{Source: &resourcev1.Mount_HostPath{HostPath: "/srv/site"}, Target: "/srv", MakeOwner: "0082:0082"}}}
	if err := validateMounts(spec); err != nil || spec.GetMounts()[0].GetMakeOwner() != "82:82" {
		t.Errorf("a server path's make_owner = %q, %v; want 82:82", spec.GetMounts()[0].GetMakeOwner(), err)
	}
}
