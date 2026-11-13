// SPDX-License-Identifier: AGPL-3.0-only

package container

import (
	"fmt"
	"os"
	"path/filepath"
	"runtime"
	"slices"
	"strings"
	"syscall"
	"testing"

	resourcev1 "github.com/dockwell/dockwell/gen/dockwell/resource/v1alpha1"
	"github.com/dockwell/dockwell/internal/docker"
	"github.com/dockwell/dockwell/internal/docker/dockertest"
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

type folders struct {
	made, owned, modes, seeded []string
	// full are the volume folders with something in them.
	full map[string]bool
}

func (f *folders) Make(path string, uid, gid uint32) error {
	f.made = append(f.made, path+" "+resource.FormatOwner(uid, gid))
	return nil
}

func (f *folders) Empty(path string) (bool, error) { return !f.full[path], nil }

func (f *folders) Own(path string, uid, gid uint32) error {
	f.owned = append(f.owned, path+" "+resource.FormatOwner(uid, gid))
	return nil
}

func (f *folders) Mode(path string, mode uint32) error {
	f.modes = append(f.modes, fmt.Sprintf("%s %04o", path, mode))
	return nil
}

func (f *folders) Seed(path string, files []*resourcev1.MountSeed, own bool, uid, gid uint32) error {
	for _, s := range files {
		f.seeded = append(f.seeded, path+"/"+s.GetPath())
	}
	return nil
}

// A volume's folder is read for whether it holds anything, and given its owner,
// through a handle on it, refusing a link in its place (ADR 0085).
func TestAVolumesFolder(t *testing.T) {
	uid, gid := uint32(os.Getuid()), uint32(os.Getgid())
	empty := t.TempDir()
	if is, err := (HostFolders{}).Empty(empty); err != nil || !is {
		t.Errorf("an empty folder = %v, %v", is, err)
	}
	full := t.TempDir()
	if err := os.WriteFile(filepath.Join(full, "app.db"), []byte("data"), 0o600); err != nil {
		t.Fatal(err)
	}
	if is, err := (HostFolders{}).Empty(full); err != nil || is {
		t.Errorf("a folder with data = %v, %v", is, err)
	}
	if err := (HostFolders{}).Own(empty, uid, gid); err != nil {
		t.Errorf("giving a folder to its owner: %v", err)
	}
	if other := uid + 1; os.Geteuid() != 0 {
		if err := (HostFolders{}).Own(empty, other, other); err == nil || !strings.Contains(err.Error(), "sudo chown") {
			t.Errorf("giving away a folder without root = %v, want how to do it by hand", err)
		}
	}
	link := filepath.Join(t.TempDir(), "link")
	if err := os.Symlink(empty, link); err != nil {
		t.Fatal(err)
	}
	if _, err := (HostFolders{}).Empty(link); err == nil {
		t.Errorf("a link in a volume's place was followed")
	}
	if err := (HostFolders{}).Own(link, uid, gid); err == nil {
		t.Errorf("a link in a volume's place was followed")
	}
	if err := (HostFolders{}).Own("relative", uid, gid); err == nil {
		t.Errorf("a relative path was taken")
	}
}

// Only a volume Dockwell made, kept where Docker keeps volumes and empty before the
// container is made, is given its owner, once the container is made: one with data,
// one Docker's own, one another container mounts, and one a driver keeps elsewhere
// are left alone, and one Docker makes with the container is given its owner too.
func TestOnlyANewVolumeIsGivenItsOwner(t *testing.T) {
	e := dockertest.New(t)
	at := Target{Name: "env_x-api-1", Project: "env_x", Service: "api", Scoped: true}
	owned := func(name string) *resourcev1.Mount {
		return &resourcev1.Mount{Source: &resourcev1.Mount_Volume{Volume: name}, Target: "/" + name, MakeOwner: "1000:1000"}
	}
	for _, v := range []string{"data", "kept", "shared"} {
		e.AddVolume(docker.Volume{Name: at.VolumeName(owned(v)), Driver: "local", Mountpoint: "/var/lib/docker/volumes/" + v + "/_data"})
	}
	// L2: an empty volume another container mounts is that container's to fill.
	e.AddContainer(docker.Container{Name: "/other", Mounts: []docker.MountPoint{{Type: "volume", Name: at.VolumeName(owned("shared")), Destination: "/shared"}}})
	e.AddVolume(docker.Volume{Name: at.VolumeName(owned("share")), Driver: "local", Options: map[string]string{"type": "nfs"}, Mountpoint: "/var/lib/docker/volumes/share/_data"})
	f := &folders{full: map[string]bool{"/var/lib/docker/volumes/kept/_data": true}}
	d := &Docker{Client: e.Client(), Folders: f}
	theirs := owned("theirs")
	theirs.External = true
	spec := &resourcev1.Container{Mounts: []*resourcev1.Mount{
		owned("data"), owned("kept"), owned("share"), theirs, owned("made"), owned("shared"),
		{Source: &resourcev1.Mount_Volume{Volume: "plain"}, Target: "/plain"},
	}}
	give, err := d.ownVolumes(t.Context(), at, spec)
	if err != nil {
		t.Fatal(err)
	}
	if len(f.owned) != 0 {
		t.Errorf("owned %v before the container is made, which Docker would undo", f.owned)
	}
	// As Docker makes the container, it makes the volume that wasn't there.
	e.AddVolume(docker.Volume{Name: at.VolumeName(owned("made")), Driver: "local", Mountpoint: "/var/lib/docker/volumes/made/_data"})
	if err := give(t.Context()); err != nil {
		t.Fatal(err)
	}
	want := []string{"/var/lib/docker/volumes/data/_data 1000:1000", "/var/lib/docker/volumes/made/_data 1000:1000"}
	if !slices.Equal(f.owned, want) {
		t.Errorf("owned %v, want %v", f.owned, want)
	}
	if give, err := (&Docker{Client: e.Client()}).ownVolumes(t.Context(), at, spec); err != nil || give(t.Context()) != nil {
		t.Errorf("with no folder maker, %v", err)
	}
}

// What a volume starts with is written by handle, each file anew, so a link put in a
// folder's place is refused and a file there already isn't written over; the volume's
// folder takes its mode the same way (ADR 0085).
func TestAVolumeStartsWithItsFiles(t *testing.T) {
	uid, gid := uint32(os.Getuid()), uint32(os.Getgid())
	vol := t.TempDir()
	files := []*resourcev1.MountSeed{
		{Path: "Caddyfile", Content: ":80\n", Mode: 0o644},
		{Path: "sites/a/run.sh", Content: "#!/bin/sh\n", Mode: 0o755},
	}
	if err := (HostFolders{}).Seed(vol, files, true, uid, gid); err != nil {
		t.Fatal(err)
	}
	for _, f := range files {
		st, err := os.Stat(filepath.Join(vol, f.GetPath()))
		if err != nil || st.Mode().Perm() != os.FileMode(f.GetMode()) {
			t.Errorf("%s is %v, %v", f.GetPath(), st, err)
		}
		if b, _ := os.ReadFile(filepath.Join(vol, f.GetPath())); string(b) != f.GetContent() {
			t.Errorf("%s holds %q", f.GetPath(), b)
		}
	}
	if err := (HostFolders{}).Seed(vol, files[:1], true, uid, gid); err == nil {
		t.Errorf("a file that's there was written over")
	}
	elsewhere := t.TempDir()
	linked := t.TempDir()
	if err := os.Symlink(elsewhere, filepath.Join(linked, "sites")); err != nil {
		t.Fatal(err)
	}
	if err := (HostFolders{}).Seed(linked, files[1:], true, uid, gid); err == nil {
		t.Errorf("a link in a folder's place was followed")
	}
	if entries, _ := os.ReadDir(elsewhere); len(entries) > 0 {
		t.Errorf("a file went where the link led: %v", entries)
	}
	if err := (HostFolders{}).Mode(vol, 0o770); err != nil {
		t.Fatal(err)
	}
	if st, _ := os.Stat(vol); st.Mode().Perm() != 0o770 {
		t.Errorf("the volume's folder is %v", st.Mode())
	}
}

// A new volume starts with its files before the container is made, so Docker copies
// nothing of the image's into it, and takes its owner and mode once the container is
// made; one with something in it is left as it is (ADR 0085).
func TestANewVolumeIsPrepared(t *testing.T) {
	e := dockertest.New(t)
	at := Target{Name: "env_x-api-1", Project: "env_x", Service: "api", Scoped: true}
	seed := []*resourcev1.MountSeed{{Path: "app.conf", Content: "x", Mode: 0o644}}
	conf := &resourcev1.Mount{Source: &resourcev1.Mount_Volume{Volume: "conf"}, Target: "/conf", Seed: seed, MakeOwner: "1000:1000"}
	logs := &resourcev1.Mount{Source: &resourcev1.Mount_Volume{Volume: "logs"}, Target: "/logs", MakeMode: 0o777}
	kept := &resourcev1.Mount{Source: &resourcev1.Mount_Volume{Volume: "kept"}, Target: "/kept", Seed: seed, MakeMode: 0o777}
	made := &resourcev1.Mount{Source: &resourcev1.Mount_Volume{Volume: "made"}, Target: "/made", Seed: seed}
	for _, v := range []string{"conf", "logs", "kept"} {
		e.AddVolume(docker.Volume{Name: at.VolumeName(&resourcev1.Mount{Source: &resourcev1.Mount_Volume{Volume: v}}), Driver: "local", Mountpoint: "/v/" + v})
	}
	f := &folders{full: map[string]bool{"/v/kept": true}}
	d := &Docker{Client: e.Client(), Folders: f}
	give, err := d.ownVolumes(t.Context(), at, &resourcev1.Container{Mounts: []*resourcev1.Mount{conf, logs, kept, made}})
	if err != nil {
		t.Fatal(err)
	}
	if !slices.Equal(f.seeded, []string{"/v/conf/app.conf"}) || len(f.owned) != 0 || len(f.modes) != 0 {
		t.Errorf("before the container: seeded %v, owned %v, modes %v", f.seeded, f.owned, f.modes)
	}
	e.AddVolume(docker.Volume{Name: at.VolumeName(made), Driver: "local", Mountpoint: "/v/made"})
	if err := give(t.Context()); err != nil {
		t.Fatal(err)
	}
	if !slices.Equal(f.seeded, []string{"/v/conf/app.conf", "/v/made/app.conf"}) {
		t.Errorf("seeded %v", f.seeded)
	}
	if !slices.Equal(f.owned, []string{"/v/conf 1000:1000"}) || !slices.Equal(f.modes, []string{"/v/logs 0777"}) {
		t.Errorf("after the container: owned %v, modes %v", f.owned, f.modes)
	}
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
	spec := &resourcev1.Container{Image: "nginx:1.27", Mounts: []*resourcev1.Mount{{Source: &resourcev1.Mount_Volume{Volume: "data"}, Target: "/data", MakeOwner: "0082:82"}}}
	if err := validateMounts(spec); err != nil || spec.GetMounts()[0].GetMakeOwner() != "82:82" {
		t.Errorf("a volume's make_owner = %q, %v; want 82:82", spec.GetMounts()[0].GetMakeOwner(), err)
	}
	spec = &resourcev1.Container{Image: "nginx:1.27", Mounts: []*resourcev1.Mount{{Source: &resourcev1.Mount_Volume{Volume: "data"}, Target: "/data", External: true, MakeOwner: "82:82"}}}
	if err := validateMounts(spec); err == nil || !strings.Contains(err.Error(), "Docker's own") {
		t.Errorf("Docker's own volume with a make_owner = %v", err)
	}
	spec = &resourcev1.Container{Image: "nginx:1.27", Mounts: []*resourcev1.Mount{{Source: &resourcev1.Mount_HostPath{HostPath: "/srv/site"}, Target: "/srv", MakeOwner: "0082:0082"}}}
	if err := validateMounts(spec); err != nil || spec.GetMounts()[0].GetMakeOwner() != "82:82" {
		t.Errorf("a server path's make_owner = %q, %v; want 82:82", spec.GetMounts()[0].GetMakeOwner(), err)
	}
}
