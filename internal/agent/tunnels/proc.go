// SPDX-License-Identifier: AGPL-3.0-only

package tunnels

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net"
	"os"
	"path"
	"runtime"
	"slices"
	"strconv"
	"strings"
	"time"

	"golang.org/x/sys/unix"
)

// Reading inside a process, as the agent does for cloudflared: its files through its
// own root, so a container can't point the agent at a host file, and its /ready from
// inside its own network (ADR 0104, section 3).

// ErrNotRegular is a token or config file that isn't a regular file of 4 KB at most.
var ErrNotRegular = errors.New("not a regular file of 4 KB or less")

// ErrLeaves is a path that would lead out of the process's root, or through a link
// where links can't be followed safely.
var ErrLeaves = errors.New("the path leads through a link or out of its root")

// ReadInRoot reads name inside the root of the process pid, which must still have
// started at start. An absolute name resolves in the process's root, and a relative
// one beneath its working directory. Links resolve inside that root, never out of
// it (openat2's RESOLVE_IN_ROOT, Linux 5.6 and later); where openat2 is missing, any
// link in the path is refused. Only a regular file of MaxFile bytes at most is read,
// checked on the opened descriptor, and no error quotes what it holds.
func ReadInRoot(pid int, start uint64, name string) ([]byte, error) {
	procDir, err := unix.Open("/proc/"+strconv.Itoa(pid), unix.O_PATH|unix.O_DIRECTORY|unix.O_CLOEXEC, 0)
	if err != nil {
		return nil, fmt.Errorf("can't open process %d: %w", pid, err)
	}
	defer unix.Close(procDir)
	base := "root"
	if !path.IsAbs(name) {
		base = "cwd"
	}
	dir, err := unix.Openat(procDir, base, unix.O_PATH|unix.O_DIRECTORY|unix.O_CLOEXEC, 0)
	if err != nil {
		return nil, fmt.Errorf("can't open process %d's %s: %w", pid, base, err)
	}
	defer unix.Close(dir)
	b, err := readBeneath(dir, name, !path.IsAbs(name))
	if err != nil {
		return nil, err
	}
	// A PID that changed hands while the file was read names another process.
	if now, err := startOf(pid); err != nil || now != start {
		clear(b)
		return nil, fmt.Errorf("process %d isn't the one that was read", pid)
	}
	return b, nil
}

// readBeneath reads name under dir: inside it as a root, or, for relative, beneath
// it with no ".." out.
func readBeneath(dir int, name string, relative bool) ([]byte, error) {
	if relative && slices.Contains(strings.Split(name, "/"), "..") {
		return nil, ErrLeaves
	}
	clean := strings.TrimPrefix(path.Clean("/"+name), "/")
	if clean == "" {
		return nil, ErrNotRegular
	}
	// Links resolve inside the root, or beneath the working directory, never out of
	// it; a mount inside it, as a Docker secret or a bind-mounted file is, may be
	// crossed (the review's M1).
	resolve := uint64(unix.RESOLVE_IN_ROOT | unix.RESOLVE_NO_MAGICLINKS)
	if relative {
		resolve = unix.RESOLVE_BENEATH | unix.RESOLVE_NO_MAGICLINKS
	}
	fd, err := unix.Openat2(dir, clean, &unix.OpenHow{Flags: unix.O_PATH | unix.O_CLOEXEC | unix.O_NOFOLLOW, Resolve: resolve})
	if errors.Is(err, unix.ENOSYS) {
		fd, err = walkNoLinks(dir, clean)
	}
	if err != nil {
		if errors.Is(err, unix.EXDEV) || errors.Is(err, unix.ELOOP) {
			return nil, ErrLeaves
		}
		return nil, fmt.Errorf("can't open the file: %w", err)
	}
	defer unix.Close(fd)
	var st unix.Stat_t
	if err := unix.Fstat(fd, &st); err != nil || st.Mode&unix.S_IFMT != unix.S_IFREG || st.Size > MaxFile {
		return nil, ErrNotRegular
	}
	// Opened again through the handle, for reading, which never waits: a FIFO
	// can't hold the agent up, as fstat said it's a regular file.
	rfd, err := unix.Open("/proc/self/fd/"+strconv.Itoa(fd), unix.O_RDONLY|unix.O_NONBLOCK|unix.O_NOCTTY|unix.O_CLOEXEC, 0)
	if err != nil {
		return nil, fmt.Errorf("can't read the file: %w", err)
	}
	f := os.NewFile(uintptr(rfd), "tunnel-file")
	defer f.Close()
	return io.ReadAll(io.LimitReader(f, MaxFile))
}

// walkNoLinks opens clean under dir one name at a time, refusing any link, where
// openat2 is missing.
func walkNoLinks(dir int, clean string) (int, error) {
	cur, err := unix.Dup(dir)
	if err != nil {
		return -1, err
	}
	parts := strings.Split(clean, "/")
	for i, part := range parts {
		if part == ".." || part == "" {
			_ = unix.Close(cur)
			return -1, ErrLeaves
		}
		flags := unix.O_PATH | unix.O_NOFOLLOW | unix.O_CLOEXEC
		if i < len(parts)-1 {
			flags |= unix.O_DIRECTORY
		}
		next, err := unix.Openat(cur, part, flags, 0)
		_ = unix.Close(cur)
		if err != nil {
			return -1, err
		}
		var st unix.Stat_t
		if err := unix.Fstat(next, &st); err != nil || st.Mode&unix.S_IFMT == unix.S_IFLNK {
			_ = unix.Close(next)
			return -1, ErrLeaves
		}
		cur = next
	}
	return cur, nil
}

// startOf reads when the process pid started, in ticks since boot.
func startOf(pid int) (uint64, error) {
	_, start, ok := stat(os.DirFS("/proc"), strconv.Itoa(pid))
	if !ok {
		return 0, errors.New("no such process")
	}
	return start, nil
}

// ReadyInNetwork asks cloudflared's /ready at addr from inside the network of the
// process pid, which a container's cloudflared listens in. The connection's socket
// is made inside that network on a thread of its own, which goes back to the agent's
// network after; a thread that can't go back is never used again.
func ReadyInNetwork(ctx context.Context, pid int, addr string) (Ready, error) {
	// A cloudflared on the agent's own network is asked from here, as nothing needs
	// entering.
	if theirs, err := os.Readlink("/proc/" + strconv.Itoa(pid) + "/ns/net"); err == nil {
		if own, err := os.Readlink("/proc/thread-self/ns/net"); err == nil && own == theirs {
			return ReadReady(ctx, addr, nil)
		}
	}
	return ReadReady(ctx, addr, func(ctx context.Context, network, address string) (net.Conn, error) {
		return dialIn(ctx, pid, network, address)
	})
}

func dialIn(ctx context.Context, pid int, network, address string) (net.Conn, error) {
	target, err := unix.Open("/proc/"+strconv.Itoa(pid)+"/ns/net", unix.O_RDONLY|unix.O_CLOEXEC, 0)
	if err != nil {
		return nil, fmt.Errorf("can't open process %d's network: %w", pid, err)
	}
	defer unix.Close(target)
	type dialed struct {
		conn net.Conn
		err  error
	}
	done := make(chan dialed, 1)
	go func() {
		runtime.LockOSThread()
		own, err := unix.Open("/proc/thread-self/ns/net", unix.O_RDONLY|unix.O_CLOEXEC, 0)
		if err != nil {
			runtime.UnlockOSThread()
			done <- dialed{err: err}
			return
		}
		defer unix.Close(own)
		if err := unix.Setns(target, unix.CLONE_NEWNET); err != nil {
			runtime.UnlockOSThread()
			done <- dialed{err: fmt.Errorf("can't enter process %d's network: %w", pid, err)}
			return
		}
		// The socket belongs to the network it's made in, wherever it's used after.
		conn, derr := (&net.Dialer{Timeout: 2 * time.Second}).DialContext(ctx, network, address)
		if err := unix.Setns(own, unix.CLONE_NEWNET); err != nil {
			// Left locked, the thread ends with this goroutine and is never reused.
			if conn != nil {
				_ = conn.Close()
			}
			done <- dialed{err: fmt.Errorf("can't leave process %d's network: %w", pid, err)}
			return
		}
		runtime.UnlockOSThread()
		done <- dialed{conn: conn, err: derr}
	}()
	select {
	case d := <-done:
		return d.conn, d.err
	case <-ctx.Done():
		go func() {
			if d := <-done; d.conn != nil {
				_ = d.conn.Close()
			}
		}()
		return nil, ctx.Err()
	}
}
