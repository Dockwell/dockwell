// SPDX-License-Identifier: AGPL-3.0-only

// Package gitlabconnection is the "gitlab.connection" kind: a GitLab instance
// URL and access token used by the control plane to report CI deploy commit
// statuses (ADR 0111). It lives in the organization, on the control plane alone:
// its token is sealed at rest and never leaves the control plane.
package gitlabconnection

import (
	"errors"
	"fmt"
	"net/url"
	"slices"
	"strings"
	"unicode"

	"google.golang.org/protobuf/proto"

	resourcev1 "github.com/dockwell/dockwell/gen/dockwell/resource/v1alpha1"
	"github.com/dockwell/dockwell/internal/resource"
)

// Kind is the GitLab connection resource kind.
const Kind resource.Kind = "gitlab.connection"

// Type is the kind.
type Type struct{}

var (
	_ resource.Type       = Type{}
	_ resource.Rotates    = Type{}
	_ resource.Keeper     = Type{}
	_ resource.NewChecker = Type{}
)

// Kind returns "gitlab.connection".
func (Type) Kind() resource.Kind { return Kind }

// Level is the organization: only an install admin connects GitLab.
func (Type) Level() resource.Level { return resource.LevelOrganization }

// IDPrefix is "glc".
func (Type) IDPrefix() string { return "glc" }

// New returns an empty GitLabConnection.
func (Type) New() proto.Message { return &resourcev1.GitLabConnection{} }

// RotatesSensitive says setting a token always plans as a change, never
// compared with what is stored (ADR 0022).
func (Type) RotatesSensitive() {}

// CheckName takes what fits in a URL and on a command line.
func (Type) CheckName(name string) error {
	return resource.CheckLabel("GitLab connection", name, "gitlab-com")
}

const (
	maxURL        = 512
	maxToken      = 1024
	maxPathPrefix = 256
)

// NormalizeURL parses and normalizes a GitLab base URL (HTTPS, lowercase host,
// no userinfo, query or fragment, no trailing slash).
func NormalizeURL(raw string) (string, error) {
	s := strings.TrimSpace(raw)
	if s == "" {
		return "", errors.New("a GitLab connection needs its URL, such as https://gitlab.com")
	}
	if !strings.Contains(s, "://") {
		s = "https://" + s
	}
	if len(s) > maxURL {
		return "", fmt.Errorf("a GitLab URL is %d characters at most", maxURL)
	}
	u, err := url.Parse(s)
	switch {
	case err != nil, u.Host == "", u.Opaque != "":
		return "", fmt.Errorf("%q isn't a GitLab URL. Use an HTTPS address such as https://gitlab.com", s)
	case u.Scheme != "https":
		return "", fmt.Errorf("%s isn't HTTPS, so Dockwell would send the access token in the clear. Use an HTTPS address", s)
	case u.User != nil:
		return "", errors.New("a GitLab URL holds no user or password. Put the access token in its own field")
	case u.RawQuery != "" || u.Fragment != "" || strings.ContainsAny(s, "?#\\"):
		return "", errors.New("a GitLab URL holds no query or fragment. Give the instance base address alone")
	case strings.ContainsFunc(u.Path, unicode.IsControl):
		return "", errors.New("a GitLab URL can't contain control characters")
	}
	u.Host = strings.ToLower(u.Host)
	u.Path = strings.TrimRight(u.Path, "/")
	u.RawPath = ""
	return u.String(), nil
}

// NormalizePathPrefix cleans an optional group or project path prefix.
func NormalizePathPrefix(raw string) (string, error) {
	p := strings.Trim(strings.TrimSpace(raw), "/")
	if p == "" {
		return "", nil
	}
	if len(p) > maxPathPrefix {
		return "", fmt.Errorf("a GitLab path prefix is %d characters at most", maxPathPrefix)
	}
	if strings.ContainsAny(p, "?#\\") || strings.ContainsFunc(p, func(r rune) bool { return unicode.IsControl(r) || unicode.IsSpace(r) }) {
		return "", errors.New("a GitLab path prefix is a group or project path such as acme or acme/web")
	}
	for _, seg := range strings.Split(p, "/") {
		if seg == "" || seg == "." || seg == ".." {
			return "", errors.New("a GitLab path prefix can't contain empty or relative path segments")
		}
	}
	return p, nil
}

// Validate checks and normalizes a GitLabConnection spec without leaking the token.
func (Type) Validate(spec proto.Message) error {
	c := spec.(*resourcev1.GitLabConnection) //nolint:forcetypeassert // the kind's own message
	normalized, err := NormalizeURL(c.GetInstanceUrl())
	if err != nil {
		return err
	}
	c.InstanceUrl = normalized
	prefix, err := NormalizePathPrefix(c.GetPathPrefix())
	if err != nil {
		return err
	}
	c.PathPrefix = prefix
	if tok := c.GetToken(); tok != "" {
		if len(tok) > maxToken || strings.ContainsFunc(tok, func(r rune) bool { return unicode.IsControl(r) || unicode.IsSpace(r) }) {
			return fmt.Errorf("a GitLab access token is one line of up to %d characters with no spaces", maxToken)
		}
	}
	for _, p := range c.GetProjects() {
		if !strings.HasPrefix(p, "prj_") {
			return fmt.Errorf("%q isn't a project's ID. A GitLab connection names projects by ID, such as prj_…", p)
		}
	}
	projects := slices.Clone(c.GetProjects())
	slices.Sort(projects)
	c.Projects = slices.Compact(projects)
	return nil
}

// CheckNew requires an access token when creating a new GitLab connection.
func (Type) CheckNew(spec proto.Message) error {
	c := spec.(*resourcev1.GitLabConnection) //nolint:forcetypeassert // Validate saw it
	if c.GetToken() == "" {
		return errors.New("a GitLab connection needs an access token with read_repository or api scope")
	}
	return nil
}

// Keep retains the saved token on update only while the instance URL stays the
// same, so Dockwell never sends a stored token to a different host.
func (Type) Keep(current, desired proto.Message) (resource.Kept, error) {
	was, ok := current.(*resourcev1.GitLabConnection)
	is, same := desired.(*resourcev1.GitLabConnection)
	if !ok || !same || is.GetToken() != "" {
		return resource.Kept{}, nil
	}
	if is.GetInstanceUrl() != was.GetInstanceUrl() {
		return resource.Kept{}, errors.New("the GitLab URL changed, so supply the access token again: Dockwell won't send a saved token to another host")
	}
	is.Token = was.GetToken()
	return resource.Kept{Reason: "kept_token", Words: "keeps the GitLab token it was saved with, as the update left that out"}, nil
}

// Reaches reports whether a GitLab connection may be used for projectID.
// An empty projects list reaches every project.
func Reaches(c *resourcev1.GitLabConnection, projectID string) bool {
	return len(c.GetProjects()) == 0 || slices.Contains(c.GetProjects(), projectID)
}
