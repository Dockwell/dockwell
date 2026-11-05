// SPDX-License-Identifier: AGPL-3.0-only

package caddy

import (
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"
)

// Caddy's data, config and run directories are its own, and the agent runs as
// root, so a compromised Caddy can leave anything in them for root to trip on:
// a link to /etc/shadow where a file is written, or to /dev/zero or a FIFO where
// one is read. Everything here that touches those directories goes through
// these, which stay inside the directory, refuse anything but a regular file,
// and bound what they read. What is Dockwell's alone lives where Caddy can't
// write at all (bootDir), or isn't mounted into its container at all.

// Bounds on what is read from Caddy's directories, each past what a real file
// comes to: its config with the most routes a server may have is under 1 MB, and
// a certificate's chain a few KB.
const (
	maxConfigFile = 64 << 20
	maxCertFile   = 1 << 20
)

// errNotRegular is what's in a place where Dockwell expects a file, and isn't
// one, or is a link to one.
var errNotRegular = errors.New("isn't a regular file")

// openRegular opens a regular file in root for reading, without following a
// link there, and without waiting on a FIFO. An os.Root keeps it inside root, but
// follows a link that stays there, so what stands at the name is checked before
// and after the open, and it must be the file that was opened.
func openRegular(root *os.Root, name string) (*os.File, error) {
	before, err := root.Lstat(name)
	if err != nil {
		return nil, err
	}
	if !before.Mode().IsRegular() {
		return nil, fmt.Errorf("%s %w", name, errNotRegular)
	}
	f, err := root.OpenFile(name, os.O_RDONLY|syscall.O_NONBLOCK, 0)
	if err != nil {
		return nil, err
	}
	opened, err := f.Stat()
	if err == nil && (!opened.Mode().IsRegular() || !os.SameFile(before, opened)) {
		err = fmt.Errorf("%s %w", name, errNotRegular)
	}
	if err != nil {
		_ = f.Close()
		return nil, err
	}
	return f, nil
}

// readIn reads a regular file under dir, of at most limit bytes. Anything else
// is an error, which errors.Is(err, os.ErrNotExist) reports for no file at all.
func readIn(dir, name string, limit int64) ([]byte, error) {
	root, err := os.OpenRoot(dir)
	if err != nil {
		return nil, err
	}
	defer root.Close()
	f, err := openRegular(root, name)
	if err != nil {
		return nil, err
	}
	defer f.Close()
	b, err := io.ReadAll(io.LimitReader(f, limit+1))
	if err != nil {
		return nil, err
	}
	if int64(len(b)) > limit {
		return nil, fmt.Errorf("%s is over %d bytes, far more than Caddy writes there", name, limit)
	}
	return b, nil
}

// maxTempScan bounds how many names are looked through for a temp file left
// behind. Caddy's directories are its to fill, and they hold a handful.
const maxTempScan = 4096

// removeTemps removes what writeIn leaves of name if it dies in mid-write. It
// goes through names alone, in a directory opened without waiting on a FIFO, and
// removes what stands there, never through it.
func removeTemps(root *os.Root, name string) error {
	parent, base := filepath.Dir(name), filepath.Base(name)
	dir, err := root.OpenFile(parent, os.O_RDONLY|syscall.O_NONBLOCK|syscall.O_DIRECTORY, 0)
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return nil
		}
		return err
	}
	defer dir.Close()
	entries, err := dir.ReadDir(maxTempScan)
	if err != nil && !errors.Is(err, io.EOF) {
		return err
	}
	for _, e := range entries {
		if strings.HasPrefix(e.Name(), "."+base+".") && strings.HasSuffix(e.Name(), ".tmp") {
			if err := root.Remove(filepath.Join(parent, e.Name())); err != nil && !errors.Is(err, os.ErrNotExist) {
				return err
			}
		}
	}
	return nil
}

// owner is who a written file belongs to. A negative uid leaves it with the
// agent.
type owner struct{ uid, gid int }

// agentOwns leaves a file with the agent.
var agentOwns = owner{uid: -1, gid: -1}

// proxyOwns gives a file to Caddy's user, if the agent can.
func proxyOwns() owner {
	uid, gid, chown := proxyUser()
	if !chown {
		return agentOwns
	}
	return owner{uid: uid, gid: gid}
}

// writeIn replaces a file under dir, whole or not at all: what stands at name,
// even a link, is replaced and never followed. It reaches the disk before it
// takes the name, so a power cut leaves the old file or the new one, which is
// more than Caddy's own autosave does.
func writeIn(dir, name string, b []byte, mode os.FileMode, who owner) error {
	root, err := os.OpenRoot(dir)
	if err != nil {
		return err
	}
	defer root.Close()
	parent := filepath.Dir(name)
	if parent != "." {
		if err := root.MkdirAll(parent, 0o700); err != nil {
			return err
		}
		if who.uid >= 0 {
			if err := root.Lchown(parent, who.uid, who.gid); err != nil {
				return err
			}
		}
	}
	// What a crash left of an earlier write goes too: it may hold what the file no
	// longer does, such as the panel's key.
	if err := removeTemps(root, name); err != nil {
		return err
	}
	tmp := filepath.Join(parent, "."+filepath.Base(name)+"."+strconv.Itoa(os.Getpid())+".tmp")
	f, err := root.OpenFile(tmp, os.O_WRONLY|os.O_CREATE|os.O_EXCL|syscall.O_NOFOLLOW, mode)
	if err != nil {
		return err
	}
	done := false
	defer func() {
		if !done {
			_ = f.Close()
			_ = root.Remove(tmp)
		}
	}()
	if who.uid >= 0 {
		if err := f.Chown(who.uid, who.gid); err != nil {
			return err
		}
	}
	// The mode given to create passes through the umask.
	if err := f.Chmod(mode); err != nil {
		return err
	}
	if _, err := f.Write(b); err != nil {
		return err
	}
	if err := f.Sync(); err != nil {
		return err
	}
	if err := f.Close(); err != nil {
		return err
	}
	done = true
	if err := root.Rename(tmp, name); err != nil {
		_ = root.Remove(tmp)
		return err
	}
	if d, err := root.OpenFile(parent, os.O_RDONLY|syscall.O_NONBLOCK|syscall.O_DIRECTORY, 0); err == nil {
		_ = d.Sync()
		_ = d.Close()
	}
	return nil
}
