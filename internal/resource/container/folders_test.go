// SPDX-License-Identifier: AGPL-3.0-only

package container

import (
	"os"
	"path/filepath"
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
	for in, want := range map[string]string{"82:82": "82:82", "0082:082": "82:82", " 1000:1000 ": "1000:1000", "0:0": "0:0"} {
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
