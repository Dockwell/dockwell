// SPDX-License-Identifier: AGPL-3.0-only

package service

import (
	"errors"
	"fmt"
	"net"
	"net/url"
	"strconv"
	"strings"
	"unicode"

	"github.com/dockwell/dockwell/internal/lazyre"
)

// DefaultSSHPort is the default SSH port.
const DefaultSSHPort uint32 = 22

// DefaultSSHUser is the default SSH username for Git repositories.
const DefaultSSHUser = "git"

// scpRepository matches SCP-style SSH repository addresses such as
// git@github.com:acme/web.git, git@host:/srv/git/app.git or
// deploy@git.example.com:team/sub/app.
var scpRepository = lazyre.Regexp(`^([A-Za-z0-9._-]+)@([^:/?#\s]+):(.+)$`)

// SSHEndpoint is a parsed SSH Git repository endpoint. RepoPath preserves
// whether the path is absolute (leading "/", as in ssh://git@host/srv/git/app.git
// or git@host:/srv/git/app.git) or relative (no leading "/", as in
// git@host:acme/web.git or ssh://git@host/~/acme/web.git).
type SSHEndpoint struct {
	User         string
	Host         string
	Port         uint32
	RepoPath     string
	CanonicalURL string
}

// Address returns host:port for dialing.
func (e SSHEndpoint) Address() string {
	port := e.Port
	if port == 0 {
		port = DefaultSSHPort
	}
	return net.JoinHostPort(e.Host, strconv.FormatUint(uint64(port), 10))
}

// Shown returns a human-readable repository identifier without scheme,
// leading slash, or .git suffix.
func (e SSHEndpoint) Shown() string {
	p := strings.TrimSuffix(strings.TrimPrefix(e.RepoPath, "/"), ".git")
	if e.Port != 0 && e.Port != DefaultSSHPort {
		return fmt.Sprintf("%s:%d/%s", e.Host, e.Port, p)
	}
	return e.Host + "/" + p
}

// IsSSHRepository reports whether raw is an SSH Git repository address (ssh://
// or SCP-style user@host:path).
func IsSSHRepository(raw string) bool {
	s := strings.TrimSpace(raw)
	if strings.HasPrefix(s, "ssh://") {
		return true
	}
	if strings.Contains(s, "://") {
		return false
	}
	return scpRepository().MatchString(s)
}

// ParseSSHRepository parses and normalizes an SSH Git repository address
// (ssh:// or SCP-style user@host:path). Passwords in the URL, query strings,
// fragments, spaces, control characters and leading-hyphen paths are refused.
func ParseSSHRepository(raw string) (SSHEndpoint, error) {
	s := strings.TrimSpace(raw)
	if s == "" {
		return SSHEndpoint{}, errors.New("a build needs its repository's URL, such as ssh://git@gitlab.com/acme/web.git")
	}
	if len(s) > maxURL {
		return SSHEndpoint{}, fmt.Errorf("a repository's URL holds up to %d characters", maxURL)
	}
	if strings.IndexFunc(s, func(r rune) bool { return unicode.IsSpace(r) || unicode.IsControl(r) }) >= 0 {
		return SSHEndpoint{}, fmt.Errorf("%q isn't a repository's URL: it has a space or control character in it", s)
	}
	if m := scpRepository().FindStringSubmatch(s); m != nil && !strings.Contains(s, "://") {
		user, host, repoPath := m[1], strings.ToLower(m[2]), m[3]
		abs := strings.HasPrefix(repoPath, "/")
		return buildSSHEndpoint(user, host, DefaultSSHPort, repoPath, abs)
	}
	if !strings.HasPrefix(s, "ssh://") {
		return SSHEndpoint{}, fmt.Errorf("%q isn't an SSH repository URL. Write one as ssh://git@host/owner/repo.git or git@host:owner/repo.git", raw)
	}
	u, err := url.Parse(s)
	if err != nil {
		return SSHEndpoint{}, fmt.Errorf("%q isn't a valid SSH repository URL", raw)
	}
	if u.User != nil {
		if _, hasPass := u.User.Password(); hasPass {
			return SSHEndpoint{}, errors.New("a repository's URL can't hold a password or a token: plans, history and the server would show it")
		}
	}
	if u.RawQuery != "" || u.Fragment != "" || strings.HasSuffix(s, "?") || strings.HasSuffix(s, "#") {
		return SSHEndpoint{}, errors.New("a repository's URL can't have a ? or a # in it: set the branch in its own field")
	}
	host := strings.ToLower(u.Hostname())
	port := DefaultSSHPort
	if ps := u.Port(); ps != "" {
		n, err := strconv.ParseUint(ps, 10, 16)
		if err != nil || n == 0 {
			return SSHEndpoint{}, fmt.Errorf("%q isn't a valid SSH port. Use a port from 1 to 65535", ps)
		}
		port = uint32(n)
	}
	user := DefaultSSHUser
	if u.User != nil && u.User.Username() != "" {
		user = u.User.Username()
	}
	// Git treats ssh://user@host/~/path as relative to ~user, and
	// ssh://user@host/path as the absolute path /path.
	if rel, ok := strings.CutPrefix(u.Path, "/~/"); ok {
		return buildSSHEndpoint(user, host, port, rel, false)
	}
	return buildSSHEndpoint(user, host, port, u.Path, true)
}

// ParseOrConvertSSHRepository parses an SSH repository address, or converts an
// HTTPS/HTTP repository URL (such as https://gitlab.com/acme/web) into its SSH
// equivalent (ssh://git@gitlab.com/acme/web.git).
func ParseOrConvertSSHRepository(raw string) (SSHEndpoint, error) {
	s := strings.TrimSpace(raw)
	if IsSSHRepository(s) {
		return ParseSSHRepository(s)
	}
	if s == "" {
		return SSHEndpoint{}, errors.New("enter a repository address, such as git@gitlab.com:acme/web.git")
	}
	if !strings.Contains(s, "://") {
		s = "https://" + s
	}
	u, err := url.Parse(s)
	if err != nil || (u.Scheme != "https" && u.Scheme != "http") || u.Hostname() == "" {
		return SSHEndpoint{}, fmt.Errorf("%q isn't a repository URL. Write one as git@host:owner/repo.git or https://host/owner/repo", raw)
	}
	if u.User != nil {
		return SSHEndpoint{}, errors.New("a repository's URL can't hold a password or a token: plans, history and the server would show it")
	}
	if u.RawQuery != "" || u.Fragment != "" {
		return SSHEndpoint{}, errors.New("a repository's URL can't have a ? or a # in it: set the branch in its own field")
	}
	repoPath := strings.Trim(u.Path, "/")
	if repoPath == "" {
		return SSHEndpoint{}, fmt.Errorf("%q names a host and no repository", raw)
	}
	if !strings.HasSuffix(repoPath, ".git") {
		repoPath += ".git"
	}
	return buildSSHEndpoint(DefaultSSHUser, strings.ToLower(u.Hostname()), DefaultSSHPort, "/"+repoPath, true)
}

func buildSSHEndpoint(user, host string, port uint32, rawPath string, abs bool) (SSHEndpoint, error) {
	if !validSSHHost(host) {
		return SSHEndpoint{}, fmt.Errorf("%q isn't a valid SSH host", host)
	}
	if user == "" || len(user) > 128 || strings.ContainsAny(user, " \t\r\n:@/'\"\\") || strings.IndexFunc(user, unicode.IsControl) >= 0 {
		return SSHEndpoint{}, fmt.Errorf("%q isn't a valid SSH username", user)
	}
	trimmed := strings.TrimRight(rawPath, "/")
	if abs {
		trimmed = strings.TrimPrefix(trimmed, "/")
	}
	if trimmed == "" || len(trimmed) > 1024 {
		return SSHEndpoint{}, errors.New("the SSH URL names a host and no repository path")
	}
	if strings.HasPrefix(trimmed, "-") {
		return SSHEndpoint{}, fmt.Errorf("%q isn't a valid repository path", rawPath)
	}
	if strings.ContainsAny(trimmed, " \t\r\n'\"\\`$;|&<>?*") || strings.IndexFunc(trimmed, unicode.IsControl) >= 0 {
		return SSHEndpoint{}, fmt.Errorf("%q isn't a valid repository path", rawPath)
	}
	for part := range strings.SplitSeq(trimmed, "/") {
		if part == ".." || part == "." || part == "" {
			return SSHEndpoint{}, fmt.Errorf("%q isn't a valid repository path", rawPath)
		}
	}
	if port == 0 {
		port = DefaultSSHPort
	}
	hostPart := host
	isIPv6 := strings.Contains(hostPart, ":")
	if isIPv6 {
		hostPart = "[" + hostPart + "]"
	}
	repoPath := trimmed
	var canon string
	if abs {
		repoPath = "/" + trimmed
		if port != DefaultSSHPort {
			canon = fmt.Sprintf("ssh://%s@%s:%d/%s", user, hostPart, port, trimmed)
		} else {
			canon = fmt.Sprintf("ssh://%s@%s/%s", user, hostPart, trimmed)
		}
	} else {
		switch {
		case port != DefaultSSHPort:
			canon = fmt.Sprintf("ssh://%s@%s:%d/~/%s", user, hostPart, port, trimmed)
		case isIPv6:
			canon = fmt.Sprintf("ssh://%s@%s/~/%s", user, hostPart, trimmed)
		default:
			canon = fmt.Sprintf("%s@%s:%s", user, host, trimmed)
		}
	}
	return SSHEndpoint{
		User:         user,
		Host:         host,
		Port:         port,
		RepoPath:     repoPath,
		CanonicalURL: canon,
	}, nil
}

func validSSHHost(host string) bool {
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

// SameSSHRepository reports whether two SSH repository addresses refer to the
// same repository on the same host and port (ignoring .git suffix, trailing
// slashes, and host case, while keeping absolute /path distinct from relative
// path).
func SameSSHRepository(a, b string) bool {
	ea, errA := ParseSSHRepository(a)
	eb, errB := ParseSSHRepository(b)
	if errA == nil && errB == nil {
		pa := strings.TrimSuffix(ea.RepoPath, ".git")
		pb := strings.TrimSuffix(eb.RepoPath, ".git")
		return ea.Host == eb.Host && ea.Port == eb.Port && pa == pb
	}
	return false
}

// SSHDeployKeysPage returns the web settings URL where read-only deploy keys
// are added for recognized Git hosts (GitHub, GitLab, Bitbucket, Codeberg,
// Gitea, Forgejo) and the display name of the host (empty if unrecognized).
// Paths match web/src/features/apps/gitHost.ts.
func SSHDeployKeysPage(repo string) (pageURL, hostName string) {
	ep, err := ParseOrConvertSSHRepository(repo)
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
		return base + "/settings/keys", "GitHub"
	case host == "gitlab.com" || hasHostLabel(host, "gitlab"):
		return base + "/-/settings/repository#js-deploy-keys-settings", "GitLab"
	case host == "bitbucket.org":
		return base + "/admin/access-keys", "Bitbucket"
	case host == "codeberg.org":
		return base + "/settings/keys", "Codeberg"
	case hasHostLabel(host, "gitea"):
		return base + "/settings/keys", "Gitea"
	case hasHostLabel(host, "forgejo"):
		return base + "/settings/keys", "Forgejo"
	}
	return "", ""
}

func hasHostLabel(host, label string) bool {
	for part := range strings.SplitSeq(host, ".") {
		if part == label {
			return true
		}
	}
	return false
}
