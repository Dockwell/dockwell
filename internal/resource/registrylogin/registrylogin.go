// SPDX-License-Identifier: AGPL-3.0-only

// Package registrylogin is the "registry.login" kind: what signs in to a container
// registry, so an app can pull an image the registry keeps private. It lives in the
// organization, on the control plane alone: what signs in goes to a server only
// for the call that pulls, and from there only to its registry and the token
// service it was saved with. See docs/adr/0086-registry-logins-for-private-images.md.
package registrylogin

import (
	"errors"
	"fmt"
	"regexp"
	"strings"

	"google.golang.org/protobuf/proto"

	resourcev1 "github.com/dockwell/dockwell/gen/dockwell/resource/v1alpha1"
	"github.com/dockwell/dockwell/internal/registry"
	"github.com/dockwell/dockwell/internal/resource"
)

// Kind is the registry login kind.
const Kind resource.Kind = "registry.login"

// Type is the kind.
type Type struct{}

var (
	_ resource.Type       = Type{}
	_ resource.Rotates    = Type{}
	_ resource.Keeper     = Type{}
	_ resource.NewChecker = Type{}
)

// Kind returns "registry.login".
func (Type) Kind() resource.Kind { return Kind }

// Level is the organization: an admin saves a login, and says which projects'
// apps may pull with it.
func (Type) Level() resource.Level { return resource.LevelOrganization }

// IDPrefix is "rgl".
func (Type) IDPrefix() string { return "rgl" }

// New returns an empty RegistryLogin.
func (Type) New() proto.Message { return &resourcev1.RegistryLogin{} }

// RotatesSensitive says setting what signs in always plans as a change, never
// compared with what's there, so a plan can't confirm a guess at it (ADR 0022).
func (Type) RotatesSensitive() {}

// CheckName takes what fits in a URL and on a command line.
func (Type) CheckName(name string) error {
	return resource.CheckLabel("registry login", name, "ghcr-ci")
}

// ecrHost is an Amazon ECR registry: its account, then its region.
var ecrHost = regexp.MustCompile(`^(\d{12})\.dkr\.ecr\.([a-z]{2}(?:-[a-z]+)+-\d+)\.amazonaws\.com(\.cn)?$`)

// awsRegion is an AWS region's name, such as eu-west-1.
var awsRegion = regexp.MustCompile(`^[a-z]{2}(-[a-z]+)+-\d+$`)

// IsRegion says whether s is an AWS region's name, such as eu-west-1, which goes into
// the address of ECR's API.
func IsRegion(s string) bool { return awsRegion.MatchString(s) }

// ECRRegion is the region of an Amazon ECR registry's host, and whether it's one.
func ECRRegion(host string) (string, bool) {
	m := ecrHost.FindStringSubmatch(host)
	if m == nil {
		return "", false
	}
	return m[2], true
}

// Host writes a registry as an image names it: lowercase, with no scheme or path,
// and Docker Hub's hosts as docker.io. It's empty for what isn't a host.
func Host(s string) string {
	s = strings.ToLower(strings.TrimSpace(s))
	s = strings.TrimPrefix(strings.TrimPrefix(s, "https://"), "http://")
	s, _, _ = strings.Cut(s, "/")
	switch s {
	case "index.docker.io", "registry-1.docker.io", "registry.hub.docker.com", "hub.docker.com":
		return registry.Hub
	}
	if s == "" || s == registry.Hub {
		return s
	}
	// What Docker reads as a registry's host, as an image's first part.
	ref, err := registry.Parse(s + "/a")
	if err != nil || ref.Registry != s {
		return ""
	}
	return s
}

// Validate refuses what can't sign in, in words that hold nothing of what does.
func (Type) Validate(spec proto.Message) error {
	l := spec.(*resourcev1.RegistryLogin) //nolint:forcetypeassert // the kind's own message
	host := Host(l.GetRegistry())
	switch {
	case l.GetRegistry() == "":
		return errors.New("a registry login needs its registry's host, such as ghcr.io, or docker.io for Docker Hub")
	case host == "":
		return fmt.Errorf("%q isn't a registry's host. Write it as an image names it, such as ghcr.io or registry.example.com:5000", l.GetRegistry())
	}
	l.Registry = host
	if l.GetTokenHost() != "" {
		th := Host(l.GetTokenHost())
		if th == "" || th == registry.Hub {
			return fmt.Errorf("%q isn't a host. A login's token service is the host its registry names for tokens, such as auth.docker.io: check the login to fill it in", l.GetTokenHost())
		}
		l.TokenHost = th
	}
	for _, p := range l.GetProjects() {
		if !strings.HasPrefix(p, "prj_") {
			return fmt.Errorf("%q isn't a project's ID. A login names the projects that may use it by ID, such as prj_…", p)
		}
	}
	region, isECR := ECRRegion(host)
	if e := l.GetEcr(); e != nil {
		switch {
		case !isECR:
			return fmt.Errorf("an AWS key signs in to Amazon ECR alone, and %s isn't an ECR registry. Name it as <account>.dkr.ecr.<region>.amazonaws.com", host)
		case l.GetUsername() != "" || l.GetPassword() != "":
			return errors.New("an ECR login signs in with its AWS key alone. Leave its username and password out")
		case e.GetRegion() == "":
			e.Region = region
		case !IsRegion(e.GetRegion()):
			return fmt.Errorf("%q isn't an AWS region. Leave the region out, and it's read from the registry", e.GetRegion())
		case e.GetRegion() != region:
			return fmt.Errorf("%s is in %s, and the login's region is %s. Leave the region out, and it's read from the registry", host, region, e.GetRegion())
		}
		return nil
	}
	if isECR {
		return fmt.Errorf("%s is Amazon ECR's, which signs in with an AWS key that may get its token, as a token lasts 12 hours. Give the key as the login's ECR key, not a username and password", host)
	}
	if l.GetUsername() == "" {
		return fmt.Errorf("a login for %s needs the username that signs in", host)
	}
	return nil
}

// CheckNew asks a new login for what signs in. An update can leave it out, which
// Keep fills in, so Validate can't.
func (Type) CheckNew(spec proto.Message) error {
	l := spec.(*resourcev1.RegistryLogin) //nolint:forcetypeassert // Validate saw it
	if e := l.GetEcr(); e != nil {
		if e.GetAccessKey() == "" || e.GetSecretKey() == "" {
			return errors.New("an ECR login needs an AWS access key and its secret")
		}
		return nil
	}
	if l.GetPassword() == "" {
		return fmt.Errorf("a login for %s needs a password, or better a token that only reads images", l.GetRegistry())
	}
	return nil
}

// Keep holds on to what signs in when an update leaves it out, so a form can
// change which projects use a login without asking for its token again. It keeps
// only while where it goes stays: the same registry, and the same token service,
// as a password is never sent somewhere it wasn't saved to go (ADR 0086).
func (Type) Keep(current, desired proto.Message) (resource.Kept, error) {
	was, ok := current.(*resourcev1.RegistryLogin)
	is, same := desired.(*resourcev1.RegistryLogin)
	if !ok || !same {
		return resource.Kept{}, nil
	}
	if is.GetEcr() != nil || was.GetEcr() != nil {
		return keepECR(was, is)
	}
	if is.GetPassword() != "" || was.GetPassword() == "" {
		return resource.Kept{}, nil
	}
	if Host(is.GetRegistry()) != Host(was.GetRegistry()) || Host(is.GetTokenHost()) != Host(was.GetTokenHost()) || is.GetUsername() != was.GetUsername() {
		return resource.Kept{}, errors.New("the login moves to another registry, token service or user, so it needs its password again: Dockwell won't send what it kept to where it hasn't sent it before")
	}
	is.Password = was.GetPassword()
	return resource.Kept{Reason: "kept_password", Words: "keeps the password it was saved with, as the update left that out"}, nil
}

func keepECR(was, is *resourcev1.RegistryLogin) (resource.Kept, error) {
	from, to := was.GetEcr(), is.GetEcr()
	if from == nil || to == nil || (to.GetAccessKey() != "" && to.GetSecretKey() != "") {
		return resource.Kept{}, nil
	}
	if Host(is.GetRegistry()) != Host(was.GetRegistry()) {
		return resource.Kept{}, errors.New("the login moves to another registry, so it needs its AWS key again")
	}
	if to.GetAccessKey() != "" && to.GetAccessKey() != from.GetAccessKey() {
		return resource.Kept{}, errors.New("a new access key needs its secret too")
	}
	to.AccessKey, to.SecretKey = from.GetAccessKey(), from.GetSecretKey()
	return resource.Kept{Reason: "kept_password", Words: "keeps the AWS key it was saved with, as the update left that out"}, nil
}
