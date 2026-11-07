// SPDX-License-Identifier: AGPL-3.0-only

package files

import (
	"bytes"
	"context"
	"io"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"time"

	"golang.org/x/sys/unix"

	"github.com/dockwell/dockwell/internal/datadir"
)

// Docker's data root holds each container's environment, so not knowing where it
// is must never mean serving it. Three things keep it shut, each covering what the
// one before can't:
//   - the engine says where it is, each time it answers;
//   - what it said is kept on disk, so a restart with Docker away, or a root that
//     moved, still knows;
//   - a directory that looks like one is one, and the files it holds are refused
//     by what they are, for a root no one has named, and for a copy of them. What
//     looks like one is strict enough to leave a project alone: see dockerShaped.

const (
	// defaultDockerRoot is where Docker keeps its data unless told otherwise.
	defaultDockerRoot = "/var/lib/docker"
	// dockerAsks bounds asking Docker where it keeps its data. Docker may not
	// be running, and files don't need it.
	dockerAsks = 2 * time.Second
	// maxRoots is how many places a server remembers. A root moves once or twice
	// in a server's life, so this only stops the list growing without end.
	maxRoots = 16
	// maxRootsBytes bounds reading the list back.
	maxRootsBytes = 64 << 10
)

// rootsFile keeps where Docker has said it keeps its data, one path to a line,
// oldest first, beside the agent's identity.
func (s *Server) rootsFile() string {
	return filepath.Join(datadir.Dir{Root: s.dataDir}.Agent(), "docker-roots")
}

// loadRoots reads what this server has heard before. A file that's gone or
// unreadable says nothing, and what looks like Docker's stays shut regardless.
func (s *Server) loadRoots() []string {
	f, err := os.Open(s.rootsFile())
	if err != nil {
		return nil
	}
	defer f.Close()
	text, err := io.ReadAll(io.LimitReader(f, maxRootsBytes))
	if err != nil {
		return nil
	}
	return slices.DeleteFunc(strings.Split(string(text), "\n"), func(line string) bool { return line == "" })
}

// dockerRoots are where Docker keeps its data, or has: the default, and every
// place it has said, on this agent's life or an earlier one.
func (s *Server) dockerRoots(ctx context.Context) []string {
	if s.dockerRoot != nil {
		ctx, cancel := context.WithTimeout(ctx, dockerAsks)
		defer cancel()
		if root, err := s.dockerRoot(ctx); err == nil && root != "" {
			s.remember(filepath.Clean(root))
		}
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	return append([]string{defaultDockerRoot}, s.roots...)
}

// remember keeps a place Docker said, on disk too, if it's new. The write is
// outside mu: a disk that stalls here holds this call, which is left behind like
// any other, and nothing that waits on mu, which must never wait on a disk.
func (s *Server) remember(root string) {
	s.mu.Lock()
	known := root == defaultDockerRoot || slices.Contains(s.roots, root) || strings.Contains(root, "\n")
	if !known {
		s.roots = append(s.roots, root)
		if len(s.roots) > maxRoots {
			s.roots = s.roots[len(s.roots)-maxRoots:]
		}
	}
	s.mu.Unlock()
	if known {
		return
	}
	if err := s.saveRoots(); err != nil {
		s.logger.Warn("can't keep where Docker's data is, so after a restart with Docker away it goes by what it looks like", "error", err.Error())
	}
}

// saveRoots writes the list as it stands, whole, so a failure leaves the one before
// it. One save runs at a time, and what's newest goes last.
func (s *Server) saveRoots() error {
	s.saving.Lock()
	defer s.saving.Unlock()
	s.mu.Lock()
	roots := strings.Join(s.roots, "\n")
	s.mu.Unlock()
	path := s.rootsFile()
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		return err
	}
	tmp := path + ".new"
	if err := os.WriteFile(tmp, []byte(roots+"\n"), 0o600); err != nil {
		return err
	}
	return os.Rename(tmp, path)
}

// dockerDirs are what Docker keeps beside containers in its data root.
var dockerDirs = []string{"image", "network", "overlay2", "volumes", "plugins", "swarm", "buildkit", "runtimes"}

// dockerShaped reports a directory that holds what Docker's data root does: its
// containers, and at least two more of what the engine makes as it first starts. A
// project with a containers and a volumes directory of its own isn't one. A link
// counts as what it leads to, as a root can keep a part of itself on another disk.
func dockerShaped(dir int) bool {
	if !holdsDir(dir, "containers") {
		return false
	}
	found := 0
	for _, name := range dockerDirs {
		if holdsDir(dir, name) {
			found++
		}
	}
	return found >= 2
}

// holdsDir reports a directory in dir, or a link to one.
func holdsDir(dir int, name string) bool {
	var st unix.Stat_t
	return unix.Fstatat(dir, name, &st, 0) == nil && st.Mode&unix.S_IFMT == unix.S_IFDIR
}

// holdsDockerState finds what Docker keeps of a container or a volume, by the
// names it gives them, whatever the file is called now: a volume's options, or a
// container's configuration, which docker inspect prints too.
func holdsDockerState(text []byte) bool {
	return bytes.Contains(text, []byte(`"MountOpts"`)) ||
		bytes.Contains(text, []byte(`"ResolvConfPath"`)) && bytes.Contains(text, []byte(`"Env"`))
}
