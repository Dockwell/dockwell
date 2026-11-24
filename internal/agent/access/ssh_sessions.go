// SPDX-License-Identifier: AGPL-3.0-only

package access

import (
	"context"
	"errors"
	"io/fs"
	"net/netip"
	"strconv"
	"strings"
)

// SSHSession identifies a signed-in OpenSSH connection by its root monitor
// and kernel socket. Neither a PID alone nor a client-supplied name identifies it.
type SSHSession struct {
	PID         int
	StartTicks  uint64
	Inode       string
	User, TTY   string
	Local, Peer netip.AddrPort
}

// SSHConnections reads authenticated SSH connections on the given listener
// ports. Current listener ports are used when no explicit ports are supplied.
func (r *PinReader) SSHConnections(ctx context.Context, ports []uint16) ([]SSHSession, error) {
	if len(ports) == 0 {
		pins, err := r.Read(ctx)
		if err != nil {
			return nil, err
		}
		ports = pins.SSHPorts
	}
	holders, err := r.sshSockets()
	if err != nil {
		return nil, err
	}
	var sockets []socket
	for _, file := range []string{"net/tcp", "net/tcp6"} {
		err := r.scan(file, holders, &sockets, map[uint16]bool{})
		if file == "net/tcp6" && errors.Is(err, fs.ErrNotExist) {
			continue
		}
		if err != nil {
			return nil, err
		}
	}
	var sessions []SSHSession
	for _, s := range sockets {
		if err := ctx.Err(); err != nil {
			return nil, err
		}
		if !s.signedIn || !hasSSHPort(ports, s.local.Port()) {
			continue
		}
		if session, ok := r.sshSession(s); ok {
			sessions = append(sessions, session)
		}
	}
	return sessions, nil
}

func hasSSHPort(ports []uint16, port uint16) bool {
	for _, p := range ports {
		if p == port {
			return true
		}
	}
	return false
}

func (r *PinReader) sshSession(s socket) (SSHSession, bool) {
	for _, monitor := range s.holders {
		if monitor.uid != 0 || (monitor.name != "sshd" && monitor.name != "sshd-session") {
			continue
		}
		who, ok := strings.CutSuffix(monitor.title, " [priv]")
		if isRootSession(monitor.title) {
			who, ok = "root", true
		}
		if !ok || who == "" {
			continue
		}
		pid, _ := strconv.Atoi(monitor.pid)
		b, err := fs.ReadFile(r.Proc, monitor.pid+"/stat")
		closeParen := strings.LastIndexByte(string(b), ')')
		if err != nil || pid <= 1 || closeParen < 0 {
			continue
		}
		fields := strings.Fields(string(b[closeParen+1:]))
		if len(fields) < 20 {
			continue
		}
		ticks, err := strconv.ParseUint(fields[19], 10, 64)
		if err != nil || ticks == 0 {
			continue
		}
		var tty string
		for _, h := range s.holders {
			if value, matches := strings.CutPrefix(h.title, who+"@"); matches && strings.HasPrefix(value, "pts/") {
				if _, err := strconv.ParseUint(strings.TrimPrefix(value, "pts/"), 10, 32); err == nil {
					tty = value
					break
				}
			}
		}
		return SSHSession{PID: pid, StartTicks: ticks, Inode: s.inode, User: who, TTY: tty, Local: s.local, Peer: s.peer}, true
	}
	return SSHSession{}, false
}
