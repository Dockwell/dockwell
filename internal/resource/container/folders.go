// SPDX-License-Identifier: AGPL-3.0-only

package container

import (
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"

	"golang.org/x/sys/unix"

	resourcev1 "github.com/dockwell/dockwell/gen/dockwell/resource/v1alpha1"
	"github.com/dockwell/dockwell/internal/docker"
	"github.com/dockwell/dockwell/internal/resource"
)

// FolderMaker makes a server path a mount names when it isn't there, owned as the
// mount says (Mount.make_owner), so Docker doesn't make it root's, and gives a
// volume's empty top folder its owner the same way.
type FolderMaker interface {
	Make(path string, uid, gid uint32) error
	// Empty says whether a volume's folder holds nothing, and Own gives it to uid
	// and gid.
	Empty(path string) (bool, error)
	Own(path string, uid, gid uint32) error
}

// HostFolders makes folders on the agent's own server.
type HostFolders struct{}

// madeHook runs between making a folder and opening it, in tests alone, to swap
// something else in where a racer would.
var madeHook func(path string)

// errMoved says something took the place of a folder the agent made before it
// could give it its owner, so it touched nothing.
var errMoved = errors.New("something took its place before Dockwell could give it its owner, so Dockwell left it alone. Check who can write the folder above it, then apply again")

// Make makes path, with mode 0755, owned by uid and gid, when nothing is there. The
// folders above it that aren't there are made as Docker would make them, root's. A
// folder that's there keeps its owner, whoever that is.
//
// It goes by handle, never by name: it walks down from / opening each folder with
// O_NOFOLLOW, makes what's missing with mkdirat in the folder above, and gives the
// folder its owner and mode through a handle on it. So whoever can write a folder
// on the way can't swap in a link, between one step and the next, to have the
// agent, which runs as root, give a file elsewhere away. A link on the way is
// refused, as the path a mount names has been resolved already.
func (HostFolders) Make(path string, uid, gid uint32) error {
	clean := filepath.Clean(path)
	if !filepath.IsAbs(clean) || clean == "/" {
		return fmt.Errorf("can't make %s: Dockwell makes only a folder under /, by its full path", path)
	}
	parts := strings.Split(strings.TrimPrefix(clean, "/"), "/")
	dir, err := unix.Open("/", unix.O_PATH|unix.O_DIRECTORY|unix.O_CLOEXEC, 0)
	if err != nil {
		return fmt.Errorf("can't open / to make %s: %w", path, err)
	}
	defer func() { _ = unix.Close(dir) }()
	at := ""
	for _, name := range parts[:len(parts)-1] {
		at += "/" + name
		next, err := openFolder(dir, name)
		if errors.Is(err, unix.ENOENT) {
			// As Docker makes them: a website's web server walks them as nobody's user.
			if err := unix.Mkdirat(dir, name, 0o755); err != nil && !errors.Is(err, unix.EEXIST) {
				return fmt.Errorf("can't make %s, above %s: %w", at, path, err)
			}
			next, err = openFolder(dir, name)
		}
		if err != nil {
			return onTheWay(at, path, err)
		}
		_ = unix.Close(dir)
		dir = next
	}
	last := parts[len(parts)-1]
	var st unix.Stat_t
	if err := unix.Fstatat(dir, last, &st, unix.AT_SYMLINK_NOFOLLOW); err == nil || !errors.Is(err, unix.ENOENT) {
		return nil // there, or the mount will say what's wrong with it
	}
	if err := unix.Mkdirat(dir, last, 0o755); err != nil {
		if errors.Is(err, unix.EEXIST) {
			return nil
		}
		return fmt.Errorf("can't make %s: %w", path, err)
	}
	if madeHook != nil {
		madeHook(clean)
	}
	fd, err := unix.Openat(dir, last, unix.O_RDONLY|unix.O_DIRECTORY|unix.O_NOFOLLOW|unix.O_CLOEXEC, 0)
	if err != nil {
		return fmt.Errorf("made %s, but %w", path, errMoved)
	}
	defer func() { _ = unix.Close(fd) }()
	if err := madeHere(fd); err != nil {
		return fmt.Errorf("made %s, but %w", path, err)
	}
	if err := unix.Fchown(fd, int(uid), int(gid)); err != nil {
		if errors.Is(err, unix.EPERM) {
			return fmt.Errorf("made %s, but the agent can't give it to %s, as it doesn't run as root. Run sudo chown %s %s, then apply again", path, resource.FormatOwner(uid, gid), resource.FormatOwner(uid, gid), path)
		}
		return fmt.Errorf("made %s, but can't give it to %s: %w", path, resource.FormatOwner(uid, gid), err)
	}
	// The mode, as the umask had its say in mkdirat. A website's web server reads it as nobody's user.
	if err := unix.Fchmod(fd, 0o755); err != nil {
		return fmt.Errorf("made %s, but can't open it to everyone: %w", path, err)
	}
	return nil
}

// Empty says whether path, a local volume's top folder where Docker keeps it, holds
// nothing. It opens the folder itself, refusing a link.
func (HostFolders) Empty(path string) (bool, error) {
	fd, err := openVolume(path)
	if err != nil {
		return false, err
	}
	defer func() { _ = unix.Close(fd) }()
	dup, err := unix.Dup(fd)
	if err != nil {
		return false, err
	}
	f := os.NewFile(uintptr(dup), "volume")
	defer f.Close()
	names, err := f.Readdirnames(1)
	if err != nil && !errors.Is(err, io.EOF) {
		return false, err
	}
	return len(names) == 0, nil
}

// Own gives path, a local volume's top folder where Docker keeps it, to uid and gid,
// and nothing inside it. It opens the folder itself, refusing a link, and changes it
// through that handle; the folders above it are Docker's, which only root writes.
func (HostFolders) Own(path string, uid, gid uint32) error {
	fd, err := openVolume(path)
	if err != nil {
		return err
	}
	defer func() { _ = unix.Close(fd) }()
	if err := unix.Fchown(fd, int(uid), int(gid)); err != nil {
		if errors.Is(err, unix.EPERM) {
			return fmt.Errorf("can't give the volume's folder %s to %s, as the agent doesn't run as root. Run sudo chown %s %s, then apply again", path, resource.FormatOwner(uid, gid), resource.FormatOwner(uid, gid), path)
		}
		return fmt.Errorf("can't give the volume's folder %s to %s: %w", path, resource.FormatOwner(uid, gid), err)
	}
	return nil
}

// openVolume opens a volume's top folder by its full path, refusing a link in its
// place.
func openVolume(path string) (int, error) {
	clean := filepath.Clean(path)
	if !filepath.IsAbs(clean) || clean == "/" {
		return -1, fmt.Errorf("can't open %s: Dockwell opens only a volume's folder, by its full path", path)
	}
	fd, err := unix.Open(clean, unix.O_RDONLY|unix.O_DIRECTORY|unix.O_NOFOLLOW|unix.O_CLOEXEC, 0)
	if err != nil {
		return -1, fmt.Errorf("can't open the volume's folder %s: %w", path, err)
	}
	return fd, nil
}

// openFolder opens name in dir as a folder, refusing a link.
func openFolder(dir int, name string) (int, error) {
	return unix.Openat(dir, name, unix.O_PATH|unix.O_DIRECTORY|unix.O_NOFOLLOW|unix.O_CLOEXEC, 0)
}

// onTheWay says why a folder above the one to make can't be walked through.
func onTheWay(at, path string, err error) error {
	switch {
	case errors.Is(err, unix.ELOOP), errors.Is(err, unix.ENOTDIR):
		return fmt.Errorf("can't make %s: %s is a link or a file, not a folder, and Dockwell follows no link to make one. Use the path it leads to, or make the folder by hand", path, at)
	default:
		return fmt.Errorf("can't make %s: can't open %s: %w", path, at, err)
	}
}

// madeHere checks that a handle is on the folder the agent just made: its own, and
// empty. Another folder swapped into its place, someone else's or with something
// in it, is left as it is.
func madeHere(fd int) error {
	var st unix.Stat_t
	if err := unix.Fstat(fd, &st); err != nil {
		return err
	}
	if int(st.Uid) != os.Geteuid() {
		return errMoved
	}
	dup, err := unix.Dup(fd)
	if err != nil {
		return err
	}
	f := os.NewFile(uintptr(dup), "made")
	defer f.Close()
	if names, _ := f.Readdirnames(1); len(names) > 0 {
		return errMoved
	}
	return nil
}

// makeFolders makes each server path a spec mounts with a make_owner, where it
// isn't there, through the path it leads to.
func (d *Docker) makeFolders(spec *resourcev1.Container) error {
	if d.Folders == nil {
		return nil
	}
	for _, m := range spec.GetMounts() {
		p, owner := m.GetHostPath(), m.GetMakeOwner()
		if p == "" || owner == "" {
			continue
		}
		uid, gid, err := resource.ParseOwner(owner)
		if err != nil {
			return err
		}
		if to, err := resolve(p); err == nil {
			p = to
		}
		if err := d.Folders.Make(p, uid, gid); err != nil {
			return err
		}
	}
	return nil
}

// ownVolumes finds each volume a spec mounts with a make_owner that holds nothing
// yet, before the container is made, and returns what gives each its owner once it
// is. Docker makes a new volume's folder root's, then, as it makes the container,
// copies in what the image has at the mount's target, with that folder's owner, so an
// app that runs as another user can't write it; 1Panel's scripts give the folder
// away for that. A volume with anything in it keeps its owner, so an app's data is
// never given away, and so does one a driver keeps elsewhere, such as an NFS share
// (ADR 0085).
func (d *Docker) ownVolumes(ctx context.Context, t Target, spec *resourcev1.Container) (func(context.Context) error, error) {
	type fresh struct {
		name, volume string
		uid, gid     uint32
	}
	var todo []fresh
	if d.Folders != nil {
		for _, m := range spec.GetMounts() {
			if m.GetVolume() == "" || m.GetMakeOwner() == "" || m.GetExternal() {
				continue
			}
			uid, gid, err := resource.ParseOwner(m.GetMakeOwner())
			if err != nil {
				return nil, err
			}
			name := t.VolumeName(m)
			v, err := d.Client.InspectVolume(ctx, name)
			switch {
			case docker.IsNotFound(err):
				// Docker makes it with the container, new.
			case err != nil:
				return nil, fmt.Errorf("can't read the volume %s to give it to %s: %w", m.GetVolume(), m.GetMakeOwner(), err)
			case v.Driver != "local" || len(v.Options) > 0 || v.Mountpoint == "":
				continue
			default:
				empty, err := d.Folders.Empty(v.Mountpoint)
				if err != nil {
					return nil, fmt.Errorf("the volume %s: %w", m.GetVolume(), err)
				}
				if !empty {
					continue
				}
			}
			todo = append(todo, fresh{name: name, volume: m.GetVolume(), uid: uid, gid: gid})
		}
	}
	return func(ctx context.Context) error {
		for _, f := range todo {
			v, err := d.Client.InspectVolume(ctx, f.name)
			if err != nil {
				return fmt.Errorf("can't read the volume %s to give it to %s: %w", f.volume, resource.FormatOwner(f.uid, f.gid), err)
			}
			if v.Driver != "local" || len(v.Options) > 0 || v.Mountpoint == "" {
				continue
			}
			if err := d.Folders.Own(v.Mountpoint, f.uid, f.gid); err != nil {
				return fmt.Errorf("the volume %s: %w", f.volume, err)
			}
		}
		return nil
	}, nil
}
