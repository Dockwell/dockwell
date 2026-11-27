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
	"errors"
	"fmt"
	"net"
	"net/url"
	"regexp"
	"strconv"
	"strings"
	"unicode"
)

// DefaultPort is the default SSH port.
const DefaultPort uint32 = 22

// DefaultUser is the default SSH username for Git repositories.
const DefaultUser = "git"

const maxURL = 2048

// scpLike matches SCP-style SSH repository addresses such as
// git@github.com:acme/web.git or deploy@git.example.com:team/sub/app.
var scpLike = regexp.MustCompile(`^([A-Za-z0-9._-]+)@([^:/?#\s]+):(.+)$`)

// Endpoint is a parsed SSH Git repository endpoint.
type Endpoint struct {
	User         string
	Host         string
	Port         uint32
	RepoPath     string
	CanonicalURL string
}

// Address returns host:port for dialing.
func (e Endpoint) Address() string {
	port := e.Port
	if port == 0 {
		port = DefaultPort
	}
	return net.JoinHostPort(e.Host, strconv.FormatUint(uint64(port), 10))
}

// Shown returns a human-readable repository identifier without scheme or .git.
func (e Endpoint) Shown() string {
	p := strings.TrimSuffix(strings.TrimPrefix(e.RepoPath, "/"), ".git")
	if e.Port != 0 && e.Port != DefaultPort {
		return fmt.Sprintf("%s:%d/%s", e.Host, e.Port, p)
	}
	return e.Host + "/" + p
}

// IsSSH reports whether raw looks like an SSH repository address (ssh:// or
// SCP-style user@host:path).
func IsSSH(raw string) bool {
	s := strings.TrimSpace(raw)
	if strings.HasPrefix(s, "ssh://") {
		return true
	}
	if strings.Contains(s, "://") {
		return false
	}
	return scpLike.MatchString(s)
}

// ParseSSH parses and normalizes an SSH Git repository URL (ssh:// or SCP-style
// user@host:path). Passwords in the URL, query strings, fragments, spaces and
// control characters are refused.
func ParseSSH(raw string) (Endpoint, error) {
	s := strings.TrimSpace(raw)
	if s == "" {
		return Endpoint{}, errors.New("a build needs its repository's URL, such as ssh://git@gitlab.com/acme/web.git")
	}
	if len(s) > maxURL {
		return Endpoint{}, fmt.Errorf("a repository's URL holds up to %d characters", maxURL)
	}
	if strings.IndexFunc(s, func(r rune) bool { return unicode.IsSpace(r) || unicode.IsControl(r) }) >= 0 {
		return Endpoint{}, fmt.Errorf("%q isn't a repository's URL: it has a space or control character in it", s)
	}
	if m := scpLike.FindStringSubmatch(s); m != nil && !strings.Contains(s, "://") {
		user, host, repoPath := m[1], strings.ToLower(m[2]), m[3]
		return buildEndpoint(user, host, DefaultPort, repoPath)
	}
	if !strings.HasPrefix(s, "ssh://") {
		return Endpoint{}, fmt.Errorf("%q isn't an SSH repository URL. Write one as ssh://git@host/owner/repo.git or git@host:owner/repo.git", raw)
	}
	u, err := url.Parse(s)
	if err != nil {
		return Endpoint{}, fmt.Errorf("%q isn't a valid SSH repository URL", raw)
	}
	if u.User != nil {
		if _, hasPass := u.User.Password(); hasPass {
			return Endpoint{}, errors.New("a repository's URL can't hold a password or a token: plans, history and the server would show it")
		}
	}
	if u.RawQuery != "" || u.Fragment != "" || strings.HasSuffix(s, "?") || strings.HasSuffix(s, "#") {
		return Endpoint{}, errors.New("a repository's URL can't have a ? or a # in it: set the branch in its own field")
	}
	host := strings.ToLower(u.Hostname())
	port := DefaultPort
	if ps := u.Port(); ps != "" {
		n, err := strconv.ParseUint(ps, 10, 16)
		if err != nil || n == 0 {
			return Endpoint{}, fmt.Errorf("%q isn't a valid SSH port. Use a port from 1 to 65535", ps)
		}
		port = uint32(n)
	}
	user := DefaultUser
	if u.User != nil && u.User.Username() != "" {
		user = u.User.Username()
	}
	repoPath := strings.TrimPrefix(u.Path, "/")
	return buildEndpoint(user, host, port, repoPath)
}

// ParseOrConvertSSH parses an SSH repository URL, or converts an HTTPS/HTTP
// repository URL (such as https://gitlab.com/acme/web) into its SSH equivalent
// (ssh://git@gitlab.com/acme/web.git).
func ParseOrConvertSSH(raw string) (Endpoint, error) {
	s := strings.TrimSpace(raw)
	if IsSSH(s) {
		return ParseSSH(s)
	}
	if s == "" {
		return Endpoint{}, errors.New("enter a repository address, such as git@gitlab.com:acme/web.git")
	}
	if !strings.Contains(s, "://") {
		s = "https://" + s
	}
	u, err := url.Parse(s)
	if err != nil || (u.Scheme != "https" && u.Scheme != "http") || u.Hostname() == "" {
		return Endpoint{}, fmt.Errorf("%q isn't a repository URL. Write one as git@host:owner/repo.git or https://host/owner/repo", raw)
	}
	if u.User != nil {
		return Endpoint{}, errors.New("a repository's URL can't hold a password or a token: plans, history and the server would show it")
	}
	if u.RawQuery != "" || u.Fragment != "" {
		return Endpoint{}, errors.New("a repository's URL can't have a ? or a # in it: set the branch in its own field")
	}
	repoPath := strings.Trim(u.Path, "/")
	if repoPath == "" {
		return Endpoint{}, fmt.Errorf("%q names a host and no repository", raw)
	}
	if !strings.HasSuffix(repoPath, ".git") {
		repoPath += ".git"
	}
	return buildEndpoint(DefaultUser, strings.ToLower(u.Hostname()), DefaultPort, repoPath)
}

func buildEndpoint(user, host string, port uint32, repoPath string) (Endpoint, error) {
	if !validHost(host) {
		return Endpoint{}, fmt.Errorf("%q isn't a valid SSH host", host)
	}
	if user == "" || len(user) > 128 || strings.ContainsAny(user, " \t\r\n:@/'\"\\") || strings.IndexFunc(user, unicode.IsControl) >= 0 {
		return Endpoint{}, fmt.Errorf("%q isn't a valid SSH username", user)
	}
	repoPath = strings.TrimRight(repoPath, "/")
	trimmed := strings.TrimPrefix(repoPath, "/")
	if trimmed == "" || len(trimmed) > 1024 {
		return Endpoint{}, errors.New("the SSH URL names a host and no repository path")
	}
	if strings.ContainsAny(trimmed, " \t\r\n'\"\\`$;|&<>?*") || strings.IndexFunc(trimmed, unicode.IsControl) >= 0 {
		return Endpoint{}, fmt.Errorf("%q isn't a valid repository path", repoPath)
	}
	for part := range strings.SplitSeq(trimmed, "/") {
		if part == ".." || part == "." || part == "" {
			return Endpoint{}, fmt.Errorf("%q isn't a valid repository path", repoPath)
		}
	}
	if port == 0 {
		port = DefaultPort
	}
	hostPart := host
	if strings.Contains(hostPart, ":") {
		hostPart = "[" + hostPart + "]"
	}
	canon := fmt.Sprintf("ssh://%s@%s/%s", user, hostPart, trimmed)
	if port != DefaultPort {
		canon = fmt.Sprintf("ssh://%s@%s:%d/%s", user, hostPart, port, trimmed)
	}
	return Endpoint{
		User:         user,
		Host:         host,
		Port:         port,
		RepoPath:     trimmed,
		CanonicalURL: canon,
	}, nil
}

func validHost(host string) bool {
	if host == "" || len(host) > 253 {
		return false
	}
	if net.ParseIP(host) != nil {
		return true
	}
	h := strings.TrimSuffix(host, ".")
	if h == "" {
		return false
	}
	for label := range strings.SplitSeq(h, ".") {
		if label == "" || len(label) > 63 || label[0] == '-' || label[len(label)-1] == '-' {
			return false
		}
		for _, ch := range label {
			if (ch < 'a' || ch > 'z') && (ch < '0' || ch > '9') && ch != '-' {
				return false
			}
		}
	}
	return true
}

// SameRepo reports whether two repository addresses refer to the same
// repository on the same host (ignoring .git suffix, trailing slashes, and
// case of the host). For SSH URLs, ports must match too.
func SameRepo(a, b string) bool {
	ea, errA := ParseSSH(a)
	eb, errB := ParseSSH(b)
	if errA == nil && errB == nil {
		return ea.Host == eb.Host && ea.Port == eb.Port &&
			strings.TrimSuffix(ea.RepoPath, ".git") == strings.TrimSuffix(eb.RepoPath, ".git")
	}
	return false
}

// DeployKeysPage returns the web settings URL where read-only deploy keys are
// added for recognized Git hosts (GitHub, GitLab, Bitbucket, Codeberg, Gitea,
// Forgejo) and the display name of the host (empty if unrecognized).
func DeployKeysPage(repo string) (pageURL, hostName string) {
	ep, err := ParseOrConvertSSH(repo)
	if err != nil {
		return "", ""
	}
	host := ep.Host
	p := strings.TrimSuffix(strings.TrimPrefix(ep.RepoPath, "/"), ".git")
	if p == "" {
		return "", ""
	}
	base := "https://" + host + "/" + p
	switch {
	case host == "github.com" || strings.HasPrefix(host, "github."):
		return base + "/settings/keys/new", "GitHub"
	case host == "gitlab.com" || hasLabel(host, "gitlab"):
		return base + "/-/settings/repository#js-deploy-keys-settings", "GitLab"
	case host == "bitbucket.org":
		return base + "/admin/access-keys/", "Bitbucket"
	case host == "codeberg.org":
		return base + "/settings/keys", "Codeberg"
	case hasLabel(host, "gitea"):
		return base + "/settings/keys", "Gitea"
	case hasLabel(host, "forgejo"):
		return base + "/settings/keys", "Forgejo"
	}
	return "", ""
}

func hasLabel(host, label string) bool {
	for part := range strings.SplitSeq(host, ".") {
		if part == label {
			return true
		}
	}
	return false
}

// shellQuote wraps a validated repository path in single quotes for the remote
// git-upload-pack command. buildEndpoint already forbids single quotes and
// shell metacharacters, so this is defense in depth.
func shellQuote(s string) string {
	return "'" + strings.ReplaceAll(s, "'", `'\''`) + "'"
}
