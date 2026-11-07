// SPDX-License-Identifier: AGPL-3.0-only

package container

import (
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"syscall"

	resourcev1 "github.com/dockwell/dockwell/gen/dockwell/resource/v1alpha1"
	"github.com/dockwell/dockwell/internal/resource"
)

// FolderMaker makes a server path a mount names when it isn't there, owned as the
// mount says (Mount.make_owner), so Docker doesn't make it root's.
type FolderMaker interface {
	Make(path string, uid, gid uint32) error
}

// HostFolders makes folders on the agent's own server.
type HostFolders struct{}

// Make makes path, with mode 0755, owned by uid and gid, when nothing is there. The
// folders above it that aren't there are made as Docker would make them, root's. A
// folder that's there keeps its owner, whoever that is.
func (HostFolders) Make(path string, uid, gid uint32) error {
	if _, err := os.Lstat(path); err == nil || !errors.Is(err, fs.ErrNotExist) {
		return nil // there, or the mount will say what's wrong with it
	}
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return fmt.Errorf("can't make the folders above %s: %w", path, err)
	}
	if err := os.Mkdir(path, 0o755); err != nil {
		if errors.Is(err, fs.ErrExist) {
			return nil
		}
		return fmt.Errorf("can't make %s: %w", path, err)
	}
	if err := os.Chown(path, int(uid), int(gid)); err != nil {
		if errors.Is(err, syscall.EPERM) {
			return fmt.Errorf("made %s, but the agent can't give it to %s, as it doesn't run as root. Run sudo chown %s %s, then apply again", path, resource.FormatOwner(uid, gid), resource.FormatOwner(uid, gid), path)
		}
		return fmt.Errorf("made %s, but can't give it to %s: %w", path, resource.FormatOwner(uid, gid), err)
	}
	// The mode, as the umask had its say in Mkdir.
	if err := os.Chmod(path, 0o755); err != nil {
		return fmt.Errorf("made %s, but can't open it to everyone: %w", path, err)
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
