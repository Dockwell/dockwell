// SPDX-License-Identifier: AGPL-3.0-only

// Package gitssh is Dockwell's pure-Go Git-over-SSH client (ADR 0113). It speaks
// git-upload-pack (protocol v2 with protocol v1 fallback) directly over
// golang.org/x/crypto/ssh, dials through netguard, pins host keys by key type
// and OpenSSH SHA-256 fingerprint before offering credentials, holds private
// keys only in memory, and verifies fetched packs with internal/gitfetch.
//
// It runs only on the control plane and is never linked into the agent build.
package gitssh

import (
	"strings"

	"github.com/dockwell/dockwell/internal/resource/service"
)

// DefaultPort is the default SSH port.
const DefaultPort = service.DefaultSSHPort

// DefaultUser is the default SSH username for Git repositories.
const DefaultUser = service.DefaultSSHUser

// Endpoint is a parsed SSH Git repository endpoint (shared with
// internal/resource/service so validation and dialing never drift).
type Endpoint = service.SSHEndpoint

// IsSSH reports whether raw looks like an SSH repository address (ssh:// or
// SCP-style user@host:path).
func IsSSH(raw string) bool { return service.IsSSHRepository(raw) }

// ParseSSH parses and normalizes an SSH Git repository URL (ssh:// or SCP-style
// user@host:path), preserving absolute vs relative repository paths.
func ParseSSH(raw string) (Endpoint, error) { return service.ParseSSHRepository(raw) }

// ParseOrConvertSSH parses an SSH repository URL, or converts an HTTPS/HTTP
// repository URL into its SSH equivalent.
func ParseOrConvertSSH(raw string) (Endpoint, error) { return service.ParseOrConvertSSHRepository(raw) }

// SameRepo reports whether two SSH repository addresses refer to the same
// repository on the same host and port.
func SameRepo(a, b string) bool { return service.SameSSHRepository(a, b) }

// DeployKeysPage returns the web settings URL where read-only deploy keys are
// added for recognized Git hosts and the display name of the host.
func DeployKeysPage(repo string) (pageURL, hostName string) { return service.SSHDeployKeysPage(repo) }

// shellQuote wraps a validated repository path in single quotes for the remote
// git-upload-pack command. ParseSSH already forbids single quotes and shell
// metacharacters, so this is defense in depth.
func shellQuote(s string) string {
	return "'" + strings.ReplaceAll(s, "'", `'\''`) + "'"
}
