// SPDX-License-Identifier: AGPL-3.0-only

package terminal

import (
	"bufio"
	"os"
	"path/filepath"
	"strconv"
	"strings"
)

// defaultPath is the PATH a shell starts with, before its profile adds to it.
const defaultPath = "/usr/local/sbin:/usr/local/bin:/usr/sbin:/usr/bin:/sbin:/bin"

// account is who a shell runs as.
type account struct {
	name, home, shell string
}

// accountOf is the user uid as /etc/passwd has it: its name, home and login shell,
// or what a server has when that says nothing usable. The agent runs as root, so
// its shells are root's.
func accountOf(passwd string, uid int) account {
	a := account{name: "root", home: "/root"}
	if uid != 0 {
		a = account{name: strconv.Itoa(uid), home: "/"}
	}
	if f, err := os.Open(passwd); err == nil { //nolint:gosec // /etc/passwd, or a test's
		defer func() { _ = f.Close() }()
		lines := bufio.NewScanner(f)
		for lines.Scan() {
			fields := strings.Split(lines.Text(), ":")
			if len(fields) < 7 || fields[2] != strconv.Itoa(uid) {
				continue
			}
			a.name = fields[0]
			if strings.HasPrefix(fields[5], "/") {
				a.home = fields[5]
			}
			if usable(fields[6]) {
				a.shell = fields[6]
			}
			break
		}
	}
	if a.shell == "" {
		for _, s := range []string{"/bin/bash", "/bin/sh"} {
			if usable(s) {
				a.shell = s
				break
			}
		}
	}
	return a
}

// usable reports a shell that's there and runs, and isn't one that refuses a
// login, as /usr/sbin/nologin does.
func usable(shell string) bool {
	if !strings.HasPrefix(shell, "/") {
		return false
	}
	switch filepath.Base(shell) {
	case "nologin", "false", "true":
		return false
	}
	st, err := os.Stat(shell)
	return err == nil && st.Mode().IsRegular() && st.Mode().Perm()&0o111 != 0
}

// environment is what a shell starts with: its own, never the agent's, which holds
// GOMEMLIMIT, which would bind every Go program run in it, and can hold a proxy's
// password. The login shell's profile adds the rest.
func environment(a account) []string {
	return []string{
		"TERM=xterm-256color",
		"COLORTERM=truecolor",
		"HOME=" + a.home,
		"USER=" + a.name,
		"LOGNAME=" + a.name,
		"SHELL=" + a.shell,
		"PATH=" + defaultPath,
		"LANG=C.UTF-8",
	}
}
