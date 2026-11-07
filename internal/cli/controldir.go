// SPDX-License-Identifier: AGPL-3.0-only

//go:build !agent

package cli

import (
	"errors"
	"fmt"
	"io/fs"
	"os"
	"os/exec"
	"os/user"
	"path/filepath"
	"strings"

	"golang.org/x/term"

	"github.com/dockwell/dockwell/internal/datadir"
	"github.com/dockwell/dockwell/internal/store"
)

// geteuid is os.Geteuid, lookupUser user.Lookup, and lookPath exec.LookPath,
// which tests replace to stand in for root, the user who ran sudo, and a
// server with sudo or without it.
var (
	geteuid    = os.Geteuid
	lookupUser = user.Lookup
	lookPath   = exec.LookPath
)

// controlDir resolves the data directory of the control plane that command,
// such as "dockwell claim", opens beside it on the control server. It is
// --data-dir when one is given, and otherwise the default for this user, with
// two exceptions, as a person on a server they signed in to as someone other
// than root meets them both:
//
//   - A user other than root looks in a directory of their own, so where the
//     system service keeps the control plane, in /var/lib/dockwell, they look
//     there too. When they can read it, that is the one; when only root can,
//     the error says to run command with sudo.
//   - Root looks in the system's, so a control plane that the user who ran
//     sudo started themselves is in their own directory, not root's. The error
//     says to run command as them, as root writing there would leave files
//     they can't open.
//
// What isn't a control plane in either is left to controlState, which says so.
func (g *globalFlags) controlDir(stdio IO, command string) (datadir.Dir, error) {
	if g.dataDir != "" {
		return datadir.Dir{Root: g.dataDir}, nil
	}
	system := datadir.Dir{Root: systemDataDir}
	if geteuid() == 0 {
		if name, own := sudoUserDir(stdio.Getenv); !hasControlPlane(system) && own.Root != "" && hasControlPlane(own) {
			return system, fmt.Errorf("there's no control plane in %s, but there's one in %s, which %s runs. Run %s as %s, without sudo, so the files it changes stay theirs",
				system.Root, own.Root, name, command, name)
		}
		return system, nil
	}
	dir, err := datadir.Default(stdio.Getenv)
	if err != nil || hasControlPlane(dir) || dir.Root == system.Root {
		return dir, err
	}
	_, err = os.Stat(system.Control())
	switch {
	case err == nil && hasControlPlane(system):
		// Its directory can be open to all, and its key, as the service leaves
		// it, to root alone.
		f, err := os.Open(system.SealKey())
		if errors.Is(err, fs.ErrPermission) {
			return dir, onlyRoot(system, command)
		}
		if err == nil {
			_ = f.Close()
		}
		return system, nil
	case errors.Is(err, fs.ErrPermission):
		return dir, onlyRoot(system, command)
	}
	return dir, nil
}

// onlyRoot says that only root can open the control plane in system, which
// command, run as someone else, needs.
func onlyRoot(system datadir.Dir, command string) error {
	return fmt.Errorf("dockwell keeps its state in %s on this server, where only root can look, so %s can't open the control plane as you. "+
		"Run the same command again with sudo in front of it", system.Root, command)
}

// sudoUserDir is the data directory dockwell run uses by default for the user
// who ran sudo, if any: their own, as XDG_STATE_HOME doesn't reach root
// through sudo, in ~/.local/state.
func sudoUserDir(getenv func(string) string) (string, datadir.Dir) {
	name := getenv("SUDO_USER")
	if name == "" || name == "root" {
		return "", datadir.Dir{}
	}
	u, err := lookupUser(name)
	if err != nil || u.HomeDir == "" {
		return name, datadir.Dir{}
	}
	return name, datadir.Dir{Root: filepath.Join(u.HomeDir, ".local", "state", "dockwell")}
}

// claimCommand is the command that prints a new claim link for the control
// plane in dir, with its state at location, for whoever runs it as this process
// runs: with sudo for root, as the system service runs, unless there's no sudo
// here, as on Alpine, where you're root already; and with --data-dir when dir
// isn't where that user's dockwell looks by default. It never names a database,
// whose URL may hold a password: one on Postgres needs DOCKWELL_DATABASE in the
// shell too, and sudo leaves that out unless it's told to keep it.
func claimCommand(dir datadir.Dir, location string, getenv func(string) string) string {
	command := "dockwell claim"
	var def datadir.Dir
	if geteuid() == 0 {
		def = datadir.Dir{Root: systemDataDir}
		if _, err := lookPath("sudo"); err == nil {
			sudo := "sudo "
			if store.IsPostgres(location) {
				sudo = "sudo --preserve-env=" + envDatabase + " "
			}
			command = sudo + command
		}
	} else if d, err := datadir.Default(getenv); err == nil {
		def = d
	}
	if filepath.Clean(dir.Root) != filepath.Clean(def.Root) {
		command += " --data-dir " + shellWord(dir.Root)
	}
	return command
}

// shellWord quotes s for a POSIX shell when it needs it.
func shellWord(s string) string {
	if s != "" && strings.Trim(s, "abcdefghijklmnopqrstuvwxyzABCDEFGHIJKLMNOPQRSTUVWXYZ0123456789/._-+:@") == "" {
		return s
	}
	return "'" + strings.ReplaceAll(s, "'", `'\''`) + "'"
}

// outIsTerminal reports whether standard output is a terminal.
func (stdio IO) outIsTerminal() bool {
	if stdio.OutIsTerminal != nil {
		return stdio.OutIsTerminal()
	}
	f, ok := stdio.Out.(*os.File)
	return ok && term.IsTerminal(int(f.Fd()))
}
