// SPDX-License-Identifier: AGPL-3.0-only

// Package datadir lays out where dockwell keeps its files on a server.
//
//	<root>/control/key        the key that seals secrets at rest (0600)
//	<root>/control/state.db   the control plane's state
//	<root>/control/acme/      the ACME account key and the panel's certificates (0700)
//	<root>/agent/agent.json   the agent's identity and its control plane
//	<root>/agent/server.key   the agent's private key (0600)
//	<root>/agent/server.crt   the agent's certificate
//	<root>/agent/ca.crt       the control plane's CA
//
// The root is /var/lib/dockwell for the system service, and a per-user
// state directory otherwise, so development needs no root.
package datadir

import (
	"fmt"
	"os"
	"path/filepath"
)

// SystemRoot is where the system service keeps its data.
const SystemRoot = "/var/lib/dockwell"

// Dir is one data directory.
type Dir struct {
	Root string
}

// Default returns the data directory for the current user: SystemRoot for
// root, and $XDG_STATE_HOME/dockwell (or ~/.local/state/dockwell) otherwise.
func Default(getenv func(string) string) (Dir, error) {
	if os.Geteuid() == 0 {
		return Dir{Root: SystemRoot}, nil
	}
	if state := getenv("XDG_STATE_HOME"); state != "" {
		return Dir{Root: filepath.Join(state, "dockwell")}, nil
	}
	home, err := os.UserHomeDir()
	if err != nil {
		return Dir{}, fmt.Errorf("can't find a data directory: %w. Pass --data-dir", err)
	}
	return Dir{Root: filepath.Join(home, ".local", "state", "dockwell")}, nil
}

// Control is the control plane's directory.
func (d Dir) Control() string { return filepath.Join(d.Root, "control") }

// Agent is the agent's directory.
func (d Dir) Agent() string { return filepath.Join(d.Root, "agent") }

// SealKey is the key-encryption key. It is there once the install is.
func (d Dir) SealKey() string { return filepath.Join(d.Control(), "key") }

// NewSealKey is where the first start keeps the key it makes until the
// install is on record, when the key moves to SealKey. A first start that
// fails partway leaves it here, and the next start uses it.
func (d Dir) NewSealKey() string { return filepath.Join(d.Control(), "key.new") }

// StateDB is the control plane's database.
func (d Dir) StateDB() string { return filepath.Join(d.Control(), "state.db") }

// ACME holds the ACME account key and the panel's certificates.
func (d Dir) ACME() string { return filepath.Join(d.Control(), "acme") }

// Ensure creates dir with owner-only permissions.
func Ensure(dir string) error {
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return fmt.Errorf("can't create %s: %w", dir, err)
	}
	return nil
}
