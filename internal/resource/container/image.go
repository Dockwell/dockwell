// SPDX-License-Identifier: AGPL-3.0-only

package container

import (
	"fmt"
	"regexp"
	"strings"
	"unicode"
)

// An image reference, as Docker reads one: [registry/]name[:tag][@digest].
// See the grammar in github.com/distribution/reference, which Docker uses.
var (
	pathPartRE  = regexp.MustCompile(`^[a-z0-9]+(?:(?:[._]|__|-+)[a-z0-9]+)*$`)
	domainPart  = `(?:[a-zA-Z0-9]|[a-zA-Z0-9][a-zA-Z0-9-]*[a-zA-Z0-9])`
	registryRE  = regexp.MustCompile(`^(?:` + domainPart + `(?:\.` + domainPart + `)*|\[[0-9a-fA-F:.]+\])(?::[0-9]+)?$`)
	tagRE       = regexp.MustCompile(`^[\w][\w.-]{0,127}$`)
	digestRE    = regexp.MustCompile(`^[A-Za-z][A-Za-z0-9]*(?:[-_+.][A-Za-z][A-Za-z0-9]*)*:[0-9a-fA-F]{32,}$`)
	maxImageLen = 255
)

// CheckImage says why ref isn't an image reference Docker takes, such as
// nginx:1.27, ghcr.io/acme/web:1.4.2 or nginx@sha256:<64 hex digits>, so a
// mistake is refused when a change is proposed, not when the server pulls.
func CheckImage(ref string) error {
	if strings.TrimSpace(ref) == "" {
		return fmt.Errorf("a container needs an image, such as nginx:1.27")
	}
	if i := strings.IndexFunc(ref, unicode.IsSpace); i >= 0 {
		return fmt.Errorf("the image %q has a space in it, which no image reference has. Write it as name:tag, with nothing between, such as nginx:1.27", ref)
	}
	rest, digest, hasDigest := strings.Cut(ref, "@")
	if hasDigest && !digestRE.MatchString(digest) {
		return fmt.Errorf("the image %q has the digest %q, which isn't one Docker takes. Write it as sha256: and the 64 hex digits, such as nginx@sha256:%s",
			ref, digest, strings.Repeat("0", 64))
	}
	name, tag := rest, ""
	if i := strings.LastIndex(rest, ":"); i > strings.LastIndex(rest, "/") {
		name, tag = rest[:i], rest[i+1:]
		if !tagRE.MatchString(tag) {
			return fmt.Errorf("the image %q has the tag %q, which isn't one Docker takes. A tag is up to 128 letters, digits, underscores, dots and dashes, "+
				"and doesn't start with a dot or a dash, such as 1.27 or v2-alpine", ref, tag)
		}
	}
	if name == "" {
		return fmt.Errorf("the image %q has no name before its tag or digest. Name it, such as nginx:1.27", ref)
	}
	if len(name) > maxImageLen {
		return fmt.Errorf("the image %q has a name of %d characters, and Docker takes up to %d", ref, len(name), maxImageLen)
	}
	parts := strings.Split(name, "/")
	// The first part is a registry if it looks like a host: with a dot or
	// a port, or localhost.
	if first := parts[0]; len(parts) > 1 && (strings.ContainsAny(first, ".:") || first == "localhost" || first != strings.ToLower(first)) {
		if !registryRE.MatchString(first) {
			return fmt.Errorf("the image %q names the registry %q, which isn't a host Docker takes. Write it as a host name, with a port if it needs one, such as ghcr.io or registry.acme.dev:5000",
				ref, first)
		}
		parts = parts[1:]
	}
	for _, p := range parts {
		switch {
		case pathPartRE.MatchString(p):
		case pathPartRE.MatchString(strings.ToLower(p)):
			return fmt.Errorf("the image %q has capital letters in its name, which Docker doesn't take. Write it in lowercase, as %s",
				ref, strings.Replace(ref, name, lowerPath(name, parts), 1))
		case p == "":
			return fmt.Errorf("the image %q has an empty part between slashes in its name. Write it as [registry/]name:tag, such as ghcr.io/acme/web:1.4.2", ref)
		default:
			return fmt.Errorf("the image %q has %q in its name, which Docker doesn't take: each part between slashes is lowercase letters and digits, "+
				"with dots, underscores or dashes between them, such as acme/web-app:1.4.2", ref, p)
		}
	}
	return nil
}

// lowerPath is name with its path, the parts after any registry, in
// lowercase.
func lowerPath(name string, path []string) string {
	p := strings.Join(path, "/")
	return strings.TrimSuffix(name, p) + strings.ToLower(p)
}
