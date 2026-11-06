// SPDX-License-Identifier: AGPL-3.0-only

package files

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"golang.org/x/sys/unix"

	agentv1 "github.com/dockwell/dockwell/gen/dockwell/agent/v1"
	"github.com/dockwell/dockwell/internal/datadir"
)

const (
	// maxLinks bounds the symlinks one walk follows, as the kernel's limit does.
	maxLinks = 40
	// maxPath is the longest path a call takes, as the kernel's limit is.
	maxPath = 4096
)

// What each refused place is. A refusal is one of these, then why it says so.
const (
	whyDataDir    = "Dockwell's own data, with its keys and its state"
	whyDocker     = "Docker's own data, with each container's environment and the secrets in it"
	whyVolumeOpts = "what Docker keeps beside a volume, whose options can hold a password"
	whyContainerd = "containerd's own data, with each container's environment and the secrets in it"
	whyProc       = "the kernel's view of the server's processes, with each one's environment"
	whySys        = "the kernel's devices and settings"
	whyDev        = "the server's devices, its disks among them"
	whyRun        = "what running services keep for now, each container's environment among it"
	whyDockerCopy = "a copy of what Docker keeps of a container or a volume, with the secrets in it"
)

// shutPlaces are the server's own directories no one reads through here.
var shutPlaces = []struct{ path, why string }{
	{"/proc", whyProc},
	{"/sys", whySys},
	{"/dev", whyDev},
	{"/run", whyRun},
	{"/var/run", whyRun}, // a link to /run on most servers, but not all
	{"/var/lib/containerd", whyContainerd},
}

// identity is a directory, whatever path reaches it: a bind mount has the same
// one as what it mounts, and a symlink leads to it.
type identity struct{ dev, ino uint64 }

func identify(st *unix.Stat_t) identity { return identity{st.Dev, st.Ino} }

// place is where a walk stands in Docker's data root, which is shut but for each
// volume's files, and which is one rule each level down.
type place int

const (
	free          place = iota // nothing about here is Docker's
	dockerRoot                 // in Docker's data root: only volumes opens
	dockerVolumes              // in its volumes: each volume opens
	oneVolume                  // in one volume: only _data opens
)

// guard is what's refused on this server, as of one call.
type guard struct {
	shut   map[identity]string // why each directory is shut
	places map[identity]place  // the directories Docker's data root is made of
}

// guard reads what's refused from the server, as it stands now: the paths
// these are at can change between calls, and a restart of Docker can move its
// data root.
func (s *Server) guard(ctx context.Context) *guard {
	g := &guard{shut: map[identity]string{}, places: map[identity]place{}}
	shut := func(path, why string) {
		var st unix.Stat_t
		if unix.Stat(path, &st) == nil {
			g.shut[identify(&st)] = why
		}
	}
	for _, p := range shutPlaces {
		shut(p.path, p.why)
	}
	shut(datadir.SystemRoot, whyDataDir)
	if s.dataDir != "" {
		shut(s.dataDir, whyDataDir)
	}
	for _, root := range s.dockerRoots(ctx) {
		var st unix.Stat_t
		if unix.Stat(root, &st) != nil {
			continue
		}
		g.places[identify(&st)] = dockerRoot
		// What's in it is shut by what it is too, wherever else it is mounted.
		entries, err := os.ReadDir(root)
		if err != nil {
			continue
		}
		for _, e := range entries {
			if e.Name() != "volumes" {
				shut(filepath.Join(root, e.Name()), whyDocker)
				continue
			}
			if unix.Stat(filepath.Join(root, "volumes"), &st) == nil {
				g.places[identify(&st)] = dockerVolumes
			}
		}
	}
	return g
}

// enter says what stepping from a directory at place at into an entry means:
// where that stands, or why it is refused. It reads what the entry is, not how
// it was reached.
func (g *guard) enter(at place, name string, st *unix.Stat_t) (place, string) {
	id := identify(st)
	if why, ok := g.shut[id]; ok {
		return free, why
	}
	switch at {
	case dockerRoot:
		if name != "volumes" {
			return free, whyDocker
		}
		return dockerVolumes, ""
	case dockerVolumes:
		if st.Mode&unix.S_IFMT != unix.S_IFDIR {
			return free, whyVolumeOpts
		}
		return oneVolume, ""
	case oneVolume:
		if name != "_data" {
			return free, whyVolumeOpts
		}
		return free, ""
	case free:
	}
	return g.places[id], ""
}

// step is one directory on the way down a path, or the last part of it, open
// but not readable: O_PATH says where it is, and reads nothing.
type step struct {
	fd    int
	name  string
	place place
}

// walked is where a walk ended: each part from / down, every one checked,
// open, and with no symlink left in it.
type walked struct {
	steps []step
	st    unix.Stat_t // what the last part is
}

func (w *walked) close() {
	for _, s := range w.steps {
		_ = unix.Close(s.fd)
	}
	w.steps = nil
}

func (w *walked) last() step { return w.steps[len(w.steps)-1] }

// up forgets the last part, but never /.
func (w *walked) up() {
	if len(w.steps) > 1 {
		_ = unix.Close(w.last().fd)
		w.steps = w.steps[:len(w.steps)-1]
	}
}

// toRoot forgets everything but /.
func (w *walked) toRoot() {
	for len(w.steps) > 1 {
		w.up()
	}
}

// path is the path walked so far, as text: a link's target can lead through
// names that aren't UTF-8, and text is what goes out in a message.
func (w *walked) path() string {
	var b strings.Builder
	for _, s := range w.steps[1:] {
		b.WriteByte('/')
		b.WriteString(s.name)
	}
	if b.Len() == 0 {
		return "/"
	}
	return strings.ToValidUTF8(b.String(), "\uFFFD")
}

// parts splits a path into what's between its slashes.
func parts(p string) []string {
	return strings.FieldsFunc(p, func(r rune) bool { return r == '/' })
}

// walk follows p from / a part at a time, as the kernel would, but itself: each
// part opens from the descriptor of the one before, never through a symlink, and
// each is checked as it is reached. So what's checked is what's open, a link
// swapped behind it changes nothing, and no spelling, link or bind mount reaches
// a refused directory. p has no . or .. of its own; a link's target can.
func (g *guard) walk(p string) (*walked, *agentv1.FileRefusal) {
	root, err := unix.Open("/", unix.O_PATH|unix.O_DIRECTORY|unix.O_CLOEXEC, 0)
	if err != nil {
		return nil, denied("/", err)
	}
	w := &walked{steps: []step{{fd: root}}}
	if err := unix.Fstat(root, &w.st); err != nil {
		w.close()
		return nil, denied("/", err)
	}
	if refusal := g.follow(w, parts(p)); refusal != nil {
		w.close()
		return nil, refusal
	}
	return w, nil
}

func (g *guard) follow(w *walked, todo []string) *agentv1.FileRefusal {
	links := 0
	for len(todo) > 0 {
		name := todo[0]
		todo = todo[1:]
		switch name {
		case ".":
			continue
		case "..":
			w.up()
			if err := unix.Fstat(w.last().fd, &w.st); err != nil {
				return denied(w.path(), err)
			}
			continue
		}
		at := w.last()
		fd, err := unix.Openat(at.fd, name, unix.O_PATH|unix.O_NOFOLLOW|unix.O_CLOEXEC, 0)
		if err != nil {
			return failed(joined(w.path(), name), err)
		}
		var st unix.Stat_t
		if err := unix.Fstat(fd, &st); err != nil {
			_ = unix.Close(fd)
			return denied(joined(w.path(), name), err)
		}
		if st.Mode&unix.S_IFMT == unix.S_IFLNK {
			target, err := readLink(fd)
			_ = unix.Close(fd)
			if err != nil {
				return denied(joined(w.path(), name), err)
			}
			links++
			if links > maxLinks {
				return &agentv1.FileRefusal{
					Kind: agentv1.FileRefusal_KIND_NOT_FOUND, Path: joined(w.path(), name),
					Message: fmt.Sprintf("%s follows more than %d symlinks, so it leads nowhere. Check where its links point", joined(w.path(), name), maxLinks),
				}
			}
			if strings.HasPrefix(target, "/") {
				w.toRoot()
			}
			todo = append(parts(target), todo...)
			continue
		}
		into, why := g.enter(at.place, name, &st)
		if why != "" {
			_ = unix.Close(fd)
			return refused(joined(w.path(), name), why)
		}
		if into == free && st.Mode&unix.S_IFMT == unix.S_IFDIR && dockerShaped(fd) {
			into = dockerRoot
		}
		w.steps = append(w.steps, step{fd: fd, name: name, place: into})
		w.st = st
	}
	return nil
}

// joined is name in dir, as text.
func joined(dir, name string) string {
	name = strings.ToValidUTF8(name, "\uFFFD")
	if dir == "/" {
		return "/" + name
	}
	return dir + "/" + name
}

// readLink reads the symlink a descriptor opened with O_PATH|O_NOFOLLOW holds.
func readLink(fd int) (string, error) {
	buf := make([]byte, maxPath)
	n, err := unix.Readlinkat(fd, "", buf)
	if err != nil {
		return "", err
	}
	return string(buf[:n]), nil
}

// refused is a path Dockwell doesn't open, and why.
func refused(path, why string) *agentv1.FileRefusal {
	return &agentv1.FileRefusal{
		Kind: agentv1.FileRefusal_KIND_REFUSED, Path: path,
		Message: fmt.Sprintf("%s is %s, so Dockwell doesn't open it. Connect to the server over SSH to work with it", path, why),
	}
}

// denied is a path the server keeps from the agent.
func denied(path string, err error) *agentv1.FileRefusal {
	return &agentv1.FileRefusal{
		Kind: agentv1.FileRefusal_KIND_DENIED, Path: path,
		Message: fmt.Sprintf("the agent can't read %s: %v. Check its permissions on the server, or how it is mounted", path, err),
	}
}

// failed is what opening a part of a path comes to.
func failed(path string, err error) *agentv1.FileRefusal {
	if errors.Is(err, unix.ENOENT) || errors.Is(err, unix.ENOTDIR) || errors.Is(err, unix.ENAMETOOLONG) {
		return &agentv1.FileRefusal{
			Kind: agentv1.FileRefusal_KIND_NOT_FOUND, Path: path,
			Message: fmt.Sprintf("there's nothing at %s on this server. Check the path, and where any symlink on the way points", path),
		}
	}
	return denied(path, err)
}
