// SPDX-License-Identifier: AGPL-3.0-only

package agent

import (
	"context"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"slices"
	"strconv"

	"golang.org/x/sys/unix"
	"google.golang.org/protobuf/types/known/timestamppb"

	dockwellv1 "github.com/dockwell/dockwell/gen/dockwell/v1"
	"github.com/dockwell/dockwell/internal/agent/access"
	"github.com/dockwell/dockwell/internal/resource"
)

func (r *sshReader) proc() fs.FS { return os.DirFS(r.rootPath("/proc")) }

func (r *sshReader) sessions(ctx context.Context, ports []uint16) ([]access.SSHSession, error) {
	if r.tools.Off {
		return nil, nil
	}
	return r.sshPins().SSHConnections(ctx, ports)
}

func (r *sshReader) sshPins() *access.PinReader {
	return &access.PinReader{Proc: r.proc(), Tools: r.tools, Passwd: func() ([]byte, error) {
		return readSSHFile(r.rootPath("/etc/passwd"), maxSSHKeysFile)
	}}
}

func sshSessionID(s *access.SSHSession) string {
	return fmt.Sprintf("%d:%d:%s", s.PID, s.StartTicks, s.Inode)
}

func (r *sshReader) sessionCanDisconnect(s *access.SSHSession) bool {
	// A TTY-less session can be forwarding the panel. Every SSH session on a
	// control server without a panel address can carry the panel's tunnel.
	if r.panelOverSSH || r.tools.Root != "" || s.TTY == "" || s.Peer.Addr().IsLoopback() {
		return false
	}
	proc := r.proc()
	for pid, seen := strconv.Itoa(os.Getpid()), 0; pid != "" && pid != "0" && seen < 128; pid, seen = procParent(proc, pid), seen+1 {
		if pid == strconv.Itoa(s.PID) {
			return false
		}
		if pid == "1" {
			return true
		}
	}
	return false
}

func (r *sshReader) listSSHSessions(ctx context.Context) (*dockwellv1.ListServerSshSessionsResponse, error) {
	sessions, err := r.sessions(ctx, nil)
	if err != nil {
		return nil, err
	}
	slices.SortFunc(sessions, func(a, b access.SSHSession) int { return b.PID - a.PID })
	out := &dockwellv1.ListServerSshSessionsResponse{}
	for i := range min(len(sessions), 250) {
		s := &sessions[i]
		item := &dockwellv1.SshSession{Id: sshSessionID(s), User: s.User, Address: s.Peer.Addr().Unmap().String(), Tty: s.TTY, CanDisconnect: r.sessionCanDisconnect(s)}
		if at := procStarted(r.proc(), strconv.Itoa(s.PID)); !at.IsZero() {
			item.Since = timestamppb.New(at)
		}
		out.Sessions = append(out.Sessions, item)
	}
	return out, nil
}

func (r *sshReader) disconnectSSH(ctx context.Context, id string) error {
	sessions, err := r.sessions(ctx, nil)
	if err != nil {
		return err
	}
	for i := range sessions {
		s := &sessions[i]
		if sshSessionID(s) != id {
			continue
		}
		if !r.sessionCanDisconnect(s) {
			return sshDisconnectRefusal("this SSH connection may carry Dockwell access. Close it from its SSH client")
		}
		return r.disconnectSSHProcess(ctx, s.PID, id)
	}

	return sshDisconnectRefusal("the SSH connection has already ended. Refresh the session list")
}

func (r *sshReader) disconnectSSHProcess(ctx context.Context, pid int, id string) error {
	fd, err := unix.PidfdOpen(pid, 0)
	if err != nil {
		return sshDisconnectRefusal(fmt.Sprintf("can't identify the SSH session safely: %v", err))
	}
	defer func() { _ = unix.Close(fd) }()
	after, err := r.sessions(ctx, nil)
	if err != nil {
		return err
	}
	for i := range after {
		if current := &after[i]; sshSessionID(current) == id && r.sessionCanDisconnect(current) {
			return unix.PidfdSendSignal(fd, unix.SIGTERM, nil, 0)
		}
	}
	return sshDisconnectRefusal("the SSH connection has already ended. Refresh the session list")
}

func sshDisconnectRefusal(message string) error {
	return resource.PlainlyBecause(resource.ReasonSSHRefused, message, errors.New(message))
}
