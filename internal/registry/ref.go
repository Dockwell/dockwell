// SPDX-License-Identifier: AGPL-3.0-only

package registry

import (
	"errors"
	"fmt"
	"regexp"
	"strings"
	"sync"
)

// Hub is what Docker Hub is called in a Ref.
const Hub = "docker.io"

// Ref is an image reference taken apart, as Docker reads one.
type Ref struct {
	// Registry is the host, such as "ghcr.io" or "localhost:5000", and Hub for
	// Docker Hub.
	Registry string
	// Repository is its path there, such as "library/nginx" or "acme/web".
	Repository string
	// Tag and Digest, either or both. With neither, it's "latest".
	Tag, Digest string
}

// The grammars, compiled on first use: the agent build starts with them unbuilt
// (cmd/dockwell's TestTheAgentBuildAllocatesLittleAsItStarts).
var (
	// A repository's path: lowercase parts, as the distribution spec has them.
	pathPart = lazyRe(`^[a-z0-9]+(?:(?:[._]|__|-+)[a-z0-9]+)*$`)
	tagRe    = lazyRe(`^[A-Za-z0-9_][A-Za-z0-9_.-]{0,127}$`)
	digestRe = lazyRe(`^[a-z0-9]+(?:[.+_-][a-z0-9]+)*:[a-zA-Z0-9=_-]{32,}$`)
	// A registry's host, as Docker's grammar has it: a name of dotted labels, or
	// an IPv6 address in brackets, and a port.
	hostRe = lazyRe(`^(?:[A-Za-z0-9](?:[A-Za-z0-9-]*[A-Za-z0-9])?(?:\.[A-Za-z0-9](?:[A-Za-z0-9-]*[A-Za-z0-9])?)*|\[[0-9A-Fa-f:.]+\])(?::[0-9]{1,5})?$`)
)

// lazyRe is a pattern compiled the first time it's asked for.
func lazyRe(pattern string) func() *regexp.Regexp {
	return sync.OnceValue(func() *regexp.Regexp { return regexp.MustCompile(pattern) })
}

// ErrBadRef is a reference no registry could hold.
var ErrBadRef = errors.New("that isn't an image reference")

// Parse takes a reference apart as Docker does: its first part is a registry
// when it holds a dot or a colon, or is localhost, and a name of one part is
// one of Docker Hub's official images.
func Parse(s string) (Ref, error) {
	s = strings.TrimSpace(s)
	if s == "" || len(s) > 512 || strings.ContainsAny(s, " \t\n") {
		return Ref{}, fmt.Errorf("%w: %q", ErrBadRef, s)
	}
	var r Ref
	name := s
	if at := strings.Index(name, "@"); at >= 0 {
		r.Digest, name = name[at+1:], name[:at]
		if !digestRe().MatchString(r.Digest) {
			return Ref{}, fmt.Errorf("%w: %q has a digest that isn't one", ErrBadRef, s)
		}
	}
	if colon := strings.LastIndex(name, ":"); colon > strings.LastIndex(name, "/") {
		r.Tag, name = name[colon+1:], name[:colon]
		if !tagRe().MatchString(r.Tag) {
			return Ref{}, fmt.Errorf("%w: %q has a tag that isn't one", ErrBadRef, s)
		}
	}
	first, rest, hasPath := strings.Cut(name, "/")
	if hasPath && NamesAHost(first) {
		if !hostRe().MatchString(first) {
			return Ref{}, fmt.Errorf("%w: %q names a registry that isn't a host", ErrBadRef, s)
		}
		r.Registry, name = strings.ToLower(first), rest
	} else {
		r.Registry = Hub
	}
	if r.Registry == "index.docker.io" || r.Registry == "registry-1.docker.io" {
		r.Registry = Hub
	}
	if r.Registry == Hub && !strings.Contains(name, "/") {
		name = "library/" + name
	}
	for part := range strings.SplitSeq(name, "/") {
		if !pathPart().MatchString(part) {
			return Ref{}, fmt.Errorf("%w: %q has a name that isn't one; use lowercase letters, digits, dots, dashes and underscores", ErrBadRef, s)
		}
	}
	r.Repository = name
	return r, nil
}

// NamesAHost says whether a reference's first part is a registry's host, as
// Docker reads one: it holds a dot or a colon, or is localhost.
func NamesAHost(first string) bool {
	return strings.ContainsAny(first, ".:") || first == "localhost"
}

// Reference is what to ask a registry for: the digest, else the tag, else
// "latest".
func (r Ref) Reference() string {
	switch {
	case r.Digest != "":
		return r.Digest
	case r.Tag != "":
		return r.Tag
	}
	return "latest"
}

// Name is the repository as people write it: "nginx" for Docker Hub's
// official one, "acme/web" for another of Docker Hub's, and with its registry
// for the rest, such as "ghcr.io/acme/web".
func (r Ref) Name() string {
	if r.Registry == Hub {
		return strings.TrimPrefix(r.Repository, "library/")
	}
	return r.Registry + "/" + r.Repository
}

// String is the reference as people write it, with its tag and digest.
func (r Ref) String() string {
	s := r.Name()
	if r.Tag != "" {
		s += ":" + r.Tag
	}
	if r.Digest != "" {
		s += "@" + r.Digest
	}
	return s
}
