// SPDX-License-Identifier: AGPL-3.0-only

package access

import (
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"testing/fstest"

	"github.com/dockwell/dockwell/internal/agent/hosttool"
)

const tcpHeader = "  sl  local_address rem_address   st tx_queue rx_queue tr tm->when retrnsmt   uid  timeout inode\n"

// proc is a stand-in for /proc: processes, each with its name, its parent and
// the sockets it holds, and the kernel's socket tables.
type proc struct {
	fs fstest.MapFS
}

func newProc() *proc { return &proc{fs: fstest.MapFS{"net/tcp": {Data: []byte(tcpHeader)}}} }

// process adds a process, with its parent's ID and the socket inodes it holds.
func (p *proc) process(pid, name, parent string, inodes ...string) *proc {
	p.fs[pid+"/comm"] = &fstest.MapFile{Data: []byte(name + "\n")}
	p.fs[pid+"/stat"] = &fstest.MapFile{Data: []byte(pid + " (" + name + ") S " + parent + " 1 1 0 -1\n")}
	for i, inode := range inodes {
		p.fs[fmt.Sprintf("%s/fd/%d", pid, i+3)] = &fstest.MapFile{Data: []byte("socket:[" + inode + "]"), Mode: os.ModeSymlink}
	}
	return p
}

// socket adds a line to net/tcp, or net/tcp6.
func (p *proc) socket(file, line string) *proc {
	if p.fs[file] == nil {
		p.fs[file] = &fstest.MapFile{Data: []byte(tcpHeader)}
	}
	p.fs[file].Data = append(p.fs[file].Data, []byte(line+"\n")...)
	return p
}

// tools is a tool runner whose systemctl prints, for each socket unit it's
// asked about, what units has for it, a blank line between, and whose sshd
// prints what it's given.
func tools(t *testing.T, units map[string]string, sshd string) *hosttool.Runner {
	t.Helper()
	root := t.TempDir()
	write := func(dir, script string) {
		path := filepath.Join(root, dir)
		if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(path, []byte("#!/bin/sh\n"+script), 0o755); err != nil {
			t.Fatal(err)
		}
	}
	if units != nil {
		var script strings.Builder
		script.WriteString("first=1\nfor unit in \"$@\"; do\n  case \"$unit\" in\n")
		for unit, out := range units {
			script.WriteString("  " + unit + ") [ $first = 1 ] || echo; first=0; cat <<'EOF'\n" + out + "\nEOF\n  ;;\n")
		}
		script.WriteString("  *.socket) [ $first = 1 ] || echo; first=0; echo ;;\n  esac\ndone\n")
		write("usr/bin/systemctl", script.String())
	}
	if sshd != "" {
		write("usr/sbin/sshd", "cat <<'EOF'\n"+sshd+"\nEOF\n")
	}
	return &hosttool.Runner{Root: root}
}

func read(t *testing.T, r *PinReader) Pins {
	t.Helper()
	pins, err := r.Read(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	return pins
}

// The pins come from the kernel's socket tables: the ports the SSH server
// listens on, and the peers of its sessions from outside, over IPv4 and IPv6,
// and the ports systemd holds for it.
func TestPinsComeFromTheSocketTable(t *testing.T) {
	p := newProc().
		process("100", "sshd", "1", "1001").
		process("200", "sshd-session", "100", "1002", "1003", "1005").
		process("300", "nginx", "1", "1004").
		// sshd on 22; a session from 203.0.113.7 to 10.0.0.5; one over loopback,
		// as from a tunnel on the server; nginx on 80; and a session over IPv6,
		// from 2001:db8::7.
		socket("net/tcp", "   0: 00000000:0016 00000000:0000 0A 00000000:00000000 00:00000000 00000000     0        0 1001").
		socket("net/tcp", "   1: 0500000A:0016 077100CB:D431 01 00000000:00000000 00:00000000 00000000     0        0 1002").
		socket("net/tcp", "   2: 0100007F:0016 0100007F:D432 01 00000000:00000000 00:00000000 00000000     0        0 1003").
		socket("net/tcp", "   3: 00000000:0050 00000000:0000 0A 00000000:00000000 00:00000000 00000000     0        0 1004").
		socket("net/tcp6", "   0: B80D0120000000000000000005000000:0016 B80D0120000000000000000007000000:D433 01 00000000:00000000 00:00000000 00000000     0        0 1005")
	r := &PinReader{Proc: p.fs, Tools: tools(t, map[string]string{"ssh.socket": "0.0.0.0:2222 (Stream)\n[::]:2222 (Stream)"}, ""), Own: []uint16{7600}}
	pins := read(t, r)
	if got := fmt.Sprint(pins.SSHPorts); got != "[22 2222]" {
		t.Errorf("SSH's ports %v, want 22 and the socket unit's 2222, not nginx's 80", got)
	}
	if got := fmt.Sprint(pins.SSHFrom); got != "[203.0.113.7/32 2001:db8::7/128]" {
		t.Errorf("SSH from %v, want the sessions from outside alone", got)
	}
	rules := pins.Rules()
	if len(rules) != 2 || rules[0].Name != "pin ssh" || fmt.Sprint(rules[0].Sources) != fmt.Sprint(pins.SSHFrom) || rules[1].Name != "pin control plane" {
		t.Errorf("pins as rules %+v", rules)
	}
}

// A session's other sockets aren't SSH: a -L forward's connection out, an X11
// listener on loopback, and a -R listener a session holds count as neither a
// port of SSH's nor a session from outside; nor does a port sshd holds on
// loopback alone.
func TestASessionsForwardsArentSSH(t *testing.T) {
	p := newProc().
		process("100", "sshd", "1", "1001", "1007").
		process("200", "sshd-session", "100", "1002", "1003", "1004", "1006").
		process("201", "sshd", "100", "1005"). // an older sshd's session, named as its server is
		socket("net/tcp", "   0: 00000000:0016 00000000:0000 0A 00000000:00000000 00:00000000 00000000     0        0 1001").
		// the session itself, from 127.0.0.1, a tunnel on the server
		socket("net/tcp", "   1: 0100007F:0016 0100007F:D431 01 00000000:00000000 00:00000000 00000000     0        0 1002").
		// ssh -L 5432:198.51.100.20:5432, out from 192.0.2.5:45678
		socket("net/tcp", "   2: 050200C0:B26E 146433C6:1538 01 00000000:00000000 00:00000000 00000000     0        0 1003").
		// X11 forwarding's listener, 127.0.0.1:6010
		socket("net/tcp", "   3: 0100007F:177A 00000000:0000 0A 00000000:00000000 00:00000000 00000000     0        0 1004").
		// ssh -R 0.0.0.0:8022, with GatewayPorts on, by a session
		socket("net/tcp", "   4: 00000000:1F56 00000000:0000 0A 00000000:00000000 00:00000000 00000000     0        0 1005").
		// a connection to the -R listener, from outside
		socket("net/tcp", "   5: 0500000A:1F56 146433C6:D435 01 00000000:00000000 00:00000000 00000000     0        0 1006").
		// sshd itself on 127.0.0.1:2222 too, which nobody outside reaches
		socket("net/tcp", "   6: 0100007F:08AE 00000000:0000 0A 00000000:00000000 00:00000000 00000000     0        0 1007")
	pins := read(t, &PinReader{Proc: p.fs})
	if got := fmt.Sprint(pins.SSHPorts); got != "[22]" {
		t.Errorf("SSH's ports %v, want 22 alone", got)
	}
	if len(pins.SSHFrom) != 0 {
		t.Errorf("SSH from %v, want no session from outside", pins.SSHFrom)
	}
}

// SSH is found however the server runs it: Dropbear; sshd.socket, where systemd
// holds the port; and, where nothing Dockwell knows holds it, the ports sshd -T
// names.
func TestSSHIsFoundHoweverItRuns(t *testing.T) {
	listen22 := "   0: 00000000:0016 00000000:0000 0A 00000000:00000000 00:00000000 00000000     0        0 1001"
	cases := map[string]struct {
		proc      *proc
		systemctl map[string]string
		sshd      string
		want      string
	}{
		"dropbear": {
			proc: newProc().process("100", "dropbear", "1", "1001").socket("net/tcp", listen22),
			want: "[22]",
		},
		"sshd.socket": {
			proc:      newProc().process("1", "systemd", "0", "1001").socket("net/tcp", listen22),
			systemctl: map[string]string{"sshd.socket": "[::]:22 (Stream)"},
			want:      "[22]",
		},
		"inetd, with sshd -T": {
			proc: newProc().process("50", "inetd", "1", "1001").socket("net/tcp", listen22),
			sshd: "port 22\nport 2200\naddressfamily any",
			want: "[22 2200]",
		},
		"none": {proc: newProc(), want: "[]"},
	}
	for name, c := range cases {
		t.Run(name, func(t *testing.T) {
			r := &PinReader{Proc: c.proc.fs}
			if c.systemctl != nil || c.sshd != "" {
				r.Tools = tools(t, c.systemctl, c.sshd)
			}
			if got := fmt.Sprint(read(t, r).SSHPorts); got != c.want {
				t.Errorf("SSH's ports %v, want %s", got, c.want)
			}
		})
	}
}

// A /proc that can't be read is an error, not a server with no SSH: the checks
// then refuse to narrow it, and no change loads.
func TestAnUnreadableProcIsAnError(t *testing.T) {
	broken := map[string]fs.FS{
		"no processes":    errFS{fs: newProc().fs, fail: "."},
		"no socket table": errFS{fs: newProc().fs, fail: "net/tcp"},
	}
	for name, fsys := range broken {
		if _, err := (&PinReader{Proc: fsys}).Read(t.Context()); err == nil {
			t.Errorf("%s: no error", name)
		}
	}
	if pins, err := (&PinReader{Proc: errFS{fail: "."}, Own: []uint16{7600}}).Read(t.Context()); err == nil || fmt.Sprint(pins.Own) != "[7600]" {
		t.Errorf("an unreadable /proc: %v, own ports %v, want an error and 7600 still", err, pins.Own)
	}
	noIPv6 := newProc().fs
	if _, err := (&PinReader{Proc: noIPv6}).Read(t.Context()); err != nil {
		t.Errorf("a kernel with no IPv6: %v", err)
	}
	r := &PinReader{Proc: errFS{fail: "."}}
	m := &Manager{Dir: t.TempDir(), Table: &table{}, Pins: r.Read}
	if err := m.CheckPolicy(policy); err == nil || !strings.Contains(err.Error(), "won't narrow it") {
		t.Errorf("the policy's check with /proc unreadable: %v", err)
	}
}

// errFS fails to open one name, and reads the rest from fs.
type errFS struct {
	fs   fs.FS
	fail string
}

func (e errFS) Open(name string) (fs.File, error) {
	if name == e.fail || e.fs == nil {
		return nil, &fs.PathError{Op: "open", Path: name, Err: errors.New("permission denied")}
	}
	return e.fs.Open(name)
}

func (e errFS) ReadDir(name string) ([]fs.DirEntry, error) {
	if name == e.fail || e.fs == nil {
		return nil, &fs.PathError{Op: "readdir", Path: name, Err: errors.New("permission denied")}
	}
	return fs.ReadDir(e.fs, name)
}
