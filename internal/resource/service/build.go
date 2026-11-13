// SPDX-License-Identifier: AGPL-3.0-only

package service

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"maps"
	"net"
	"net/url"
	"path"
	"regexp"
	"slices"
	"strconv"
	"strings"
	"unicode"

	resourcev1 "github.com/dockwell/dockwell/gen/dockwell/resource/v1alpha1"
	"github.com/dockwell/dockwell/internal/docker"
	"github.com/dockwell/dockwell/internal/registry"
	"github.com/dockwell/dockwell/internal/resource"
	"github.com/dockwell/dockwell/internal/resource/binding"
	"github.com/dockwell/dockwell/internal/resource/variable"
)

// Builder builds the image a service's build names, on its server (ADR 0046). The
// agent has one; the control plane, which builds nothing, has none.
type Builder interface {
	// Has reports whether the server has the image.
	Has(ctx context.Context, image string) (bool, error)
	// Build makes req.Image from its source, unless the server has it. A build that
	// fails, or is cancelled, has touched nothing that runs.
	Build(ctx context.Context, req BuildRequest) error
}

// BuildRequest is what a step asks its server to build.
type BuildRequest struct {
	// Service is the service it builds for, and Change the change it is a step of.
	Service resource.Ref
	Change  string
	// Image is what it makes.
	Image string
	// URL, Ref and Commit say what it builds.
	URL, Ref, Commit string
	// Context, Dockerfile, Target and Args say how.
	Context, Dockerfile, Target string
	Args                        map[string]string
	// Builder is what builds it: resource.BuilderAuto, BuilderDockerfile or
	// BuilderRailpack. BuildCommand and StartCommand stand in for what Railpack
	// finds.
	Builder, BuildCommand, StartCommand string
	// Secrets are the values of its secrets, by ID, which go no further than the
	// build. SecretIDs are their IDs, which a reading of the commit has without
	// their values.
	Secrets   map[string]string
	SecretIDs []string
	// OwnCommand says the service starts with a command of its own, so the
	// commit needn't say what starts it.
	OwnCommand bool
	// Logins are the registry logins its base images may pull with, one for each
	// registry, which go no further than the build (ADR 0086). Recheck checks each
	// registry's challenge again, as a build leaves the queue, and keeps those it
	// still may hand BuildKit.
	Logins  []registry.Auth
	Recheck func(context.Context, []registry.Auth) ([]registry.Auth, error)
}

// Request is what a build of svc asks its server to build, all but its secrets.
func Request(svc *resourcev1.Service) BuildRequest {
	b := svc.GetBuild()
	g := b.GetGit()
	app := svc.GetApp()
	return BuildRequest{
		Image: app.GetImage(),
		URL:   g.GetUrl(), Ref: g.GetRef(), Commit: g.GetCommit(),
		Context: b.GetContext(), Dockerfile: b.GetDockerfile(), Target: b.GetTarget(), Args: b.GetArgs(),
		Builder: builderOf(b.GetBuilder()), BuildCommand: b.GetBuildCommand(), StartCommand: b.GetStartCommand(),
		SecretIDs:  slices.Sorted(maps.Keys(b.GetSecrets())),
		OwnCommand: len(app.GetCommand()) > 0 || len(app.GetEntrypoint()) > 0,
	}
}

// RailpackNames checks that a build's arguments and secrets can reach Railpack
// (resource.RailpackNames).
func RailpackNames(args map[string]string, secrets []string) error {
	return resource.RailpackNames(args, secrets)
}

// builderOf is what builds an image, as a request says it.
func builderOf(b resourcev1.ImageBuild_Builder) string {
	switch b {
	case resourcev1.ImageBuild_BUILDER_DOCKERFILE:
		return resource.BuilderDockerfile
	case resourcev1.ImageBuild_BUILDER_RAILPACK:
		return resource.BuilderRailpack
	case resourcev1.ImageBuild_BUILDER_UNSPECIFIED:
	}
	return resource.BuilderAuto
}

// Bounds on a build's settings, so a spec stays a size a plan and a label can hold.
const (
	maxCommand    = 1024
	maxURL        = 2048
	maxRef        = 255
	maxPath       = 1024
	maxArgs       = 64
	maxArgName    = 128
	maxArgValue   = 4096
	maxSecrets    = 32
	maxBuildLabel = 16 << 10
)

var (
	argName  = regexp.MustCompile(`^[A-Za-z_][A-Za-z0-9_]*$`)
	secretID = regexp.MustCompile(`^[A-Za-z0-9_][A-Za-z0-9_.-]*$`)
	target   = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9_.-]*$`)
	commitID = regexp.MustCompile(`^(?:[0-9a-f]{40}|[0-9a-f]{64})$`)
)

// CheckBuild checks build settings as a service's spec would hold them, and writes
// them one way, as checkBuild does.
func CheckBuild(b *resourcev1.ImageBuild) error {
	return checkBuild(&resourcev1.Service{Build: b})
}

// checkBuild checks a service's build, and writes what means the same one way: a
// URL as people paste it, such as github.com/acme/web, with its https://, and a
// Dockerfile named Dockerfile as none, its default.
func checkBuild(svc *resourcev1.Service) error {
	b := svc.GetBuild()
	g := b.GetGit()
	if g == nil {
		return errors.New("a build needs a source: a Git repository's URL, such as https://github.com/acme/web")
	}
	u, err := CheckRepository(g.GetUrl())
	if err != nil {
		return err
	}
	g.Url = u
	if err := checkGitRef(g.GetRef()); err != nil {
		return err
	}
	if ref := strings.ToLower(g.GetRef()); commitID.MatchString(ref) {
		g.Ref = ref
	}
	if c := strings.ToLower(g.GetCommit()); c != "" {
		if !commitID.MatchString(c) {
			return fmt.Errorf("%q isn't a commit: a build's commit is its full ID, 40 hex digits. Leave it out to build what the branch names", g.GetCommit())
		}
		g.Commit = c
	}
	if b.Context, err = buildPath("context", b.GetContext()); err != nil {
		return err
	}
	if b.Dockerfile, err = buildPath("Dockerfile", b.GetDockerfile()); err != nil {
		return err
	}
	if b.Dockerfile == "Dockerfile" {
		b.Dockerfile = ""
	}
	switch b.GetBuilder() {
	case resourcev1.ImageBuild_BUILDER_UNSPECIFIED, resourcev1.ImageBuild_BUILDER_DOCKERFILE, resourcev1.ImageBuild_BUILDER_RAILPACK:
	default:
		return fmt.Errorf("%d isn't a builder: a build takes its Dockerfile, Railpack, or leaves it unset to take the Dockerfile when there is one", b.GetBuilder())
	}
	railpack := b.GetBuilder() == resourcev1.ImageBuild_BUILDER_RAILPACK
	if b.GetBuilder() == resourcev1.ImageBuild_BUILDER_DOCKERFILE && (b.GetBuildCommand() != "" || b.GetStartCommand() != "") {
		return errors.New("a build with its Dockerfile runs what the Dockerfile says, so it takes no build or start command. Put them in the Dockerfile, or build with Railpack")
	}
	for what, c := range map[string]string{"build command": b.GetBuildCommand(), "start command": b.GetStartCommand()} {
		if len(c) > maxCommand || strings.ContainsAny(c, "\x00\n\r") {
			return fmt.Errorf("the build's %s is one line of up to %d characters", what, maxCommand)
		}
	}
	if railpack && (b.GetDockerfile() != "" || b.GetTarget() != "") {
		return errors.New("a build with Railpack takes no Dockerfile path or stage, as Railpack builds with no Dockerfile. Leave them out, or build with the Dockerfile")
	}
	if t := b.GetTarget(); t != "" && (len(t) > maxArgName || !target.MatchString(t)) {
		return fmt.Errorf("%q isn't a stage a Dockerfile can name. Use letters, digits, ., _ and -, as in FROM node:22 AS build", t)
	}
	if len(b.GetArgs()) > maxArgs {
		return fmt.Errorf("a build takes up to %d build arguments, and this has %d", maxArgs, len(b.GetArgs()))
	}
	for k, v := range b.GetArgs() {
		if len(k) > maxArgName || !argName.MatchString(k) {
			return fmt.Errorf("%q isn't a build argument's name. Start with a letter or _, then letters, digits and _, as ARG names it", k)
		}
		if len(v) > maxArgValue || strings.ContainsRune(v, 0) {
			return fmt.Errorf("the build argument %s holds up to %d bytes of text", k, maxArgValue)
		}
	}
	if railpack {
		// Railpack gives both to the build as environment variables.
		if err := RailpackNames(b.GetArgs(), slices.Sorted(maps.Keys(b.GetSecrets()))); err != nil {
			return err
		}
	} else if b.GetBuilder() == resourcev1.ImageBuild_BUILDER_UNSPECIFIED {
		// It may build with Railpack, which the plan says once it reads the commit.
		for _, id := range slices.Sorted(maps.Keys(b.GetSecrets())) {
			if err := resource.RailpackSecret(id); err != nil {
				return err
			}
		}
	}
	if len(b.GetSecrets()) > maxSecrets {
		return fmt.Errorf("a build takes up to %d secrets, and this has %d", maxSecrets, len(b.GetSecrets()))
	}
	if err := checkBuildLogins(b.GetRegistryLogins()); err != nil {
		return err
	}
	for id, name := range b.GetSecrets() {
		if len(id) > maxArgName || !secretID.MatchString(id) {
			return fmt.Errorf("%q isn't a build secret's ID. Use letters, digits, ., _ and -, as RUN --mount=type=secret,id= names it", id)
		}
		if name == "" {
			continue
		}
		if err := variable.CheckName("secret", name); err != nil {
			return err
		}
	}
	if len(b.GetArgs()) > 0 || b.GetTarget() != "" {
		// The label that records the build has to fit on a container.
		size := len(g.GetUrl()) + len(g.GetRef()) + len(b.GetContext()) + len(b.GetDockerfile()) + len(b.GetBuildCommand()) + len(b.GetStartCommand())
		for k, v := range b.GetArgs() {
			size += len(k) + len(v)
		}
		if size > maxBuildLabel {
			return fmt.Errorf("the build's settings come to %d bytes, and a build holds up to %d. Move what's long into the repository", size, maxBuildLabel)
		}
	}
	image := svc.GetApp().GetImage()
	if image != "" && !docker.IsBuilt(image) {
		return fmt.Errorf("a service that builds runs the image its build makes, so it can't name %s too. Leave the image out to build, or remove the build to run %s instead", image, image)
	}
	return nil
}

// CheckRepository checks a repository's URL, and gives it as Dockwell keeps it: over
// HTTPS, or HTTP, with a host and a path, and nothing that signs in. One without a
// scheme, as people paste github.com/acme/web, is HTTPS.
func CheckRepository(raw string) (string, error) {
	s := strings.TrimSpace(raw)
	if s == "" {
		return "", errors.New("a build needs its repository's URL, such as https://github.com/acme/web")
	}
	if len(s) > maxURL {
		return "", fmt.Errorf("a repository's URL holds up to %d characters", maxURL)
	}
	if strings.IndexFunc(s, func(r rune) bool { return unicode.IsSpace(r) || unicode.IsControl(r) }) >= 0 {
		return "", fmt.Errorf("%q isn't a repository's URL: it has a space in it", s)
	}
	if strings.HasPrefix(s, "git@") || strings.HasPrefix(s, "ssh://") || strings.HasPrefix(s, "git://") {
		return "", fmt.Errorf("can't fetch %s yet: Dockwell fetches a public repository over HTTPS, such as https://github.com/acme/web. SSH and private repositories come next", s)
	}
	if !strings.Contains(s, "://") {
		s = "https://" + s
	}
	u, err := url.Parse(s)
	switch {
	case err != nil:
		return "", fmt.Errorf("%q isn't a repository's URL. Write one as https://github.com/acme/web", raw)
	case u.Scheme != "https" && u.Scheme != "http":
		return "", fmt.Errorf("can't fetch %s: Dockwell fetches a repository over HTTPS, or HTTP on a private network", raw)
	case u.User != nil:
		return "", errors.New("a repository's URL can't hold a user, a password or a token: plans, history and the server would show it. Private repositories come next")
	case u.Host == "" || u.Hostname() == "":
		return "", fmt.Errorf("%q has no host. Write one as https://github.com/acme/web", raw)
	case u.RawQuery != "" || u.Fragment != "" || strings.HasSuffix(s, "?") || strings.HasSuffix(s, "#"):
		return "", errors.New("a repository's URL can't have a ? or a # in it: set the branch in its own field")
	}
	p := strings.TrimRight(u.EscapedPath(), "/")
	if p == "" {
		return "", fmt.Errorf("%q names a host and no repository. Write one as https://github.com/acme/web", raw)
	}
	if u.Scheme == "http" && !PrivateHost(u.Hostname()) {
		return "", fmt.Errorf("can't fetch %s over plain HTTP: anyone between the server and %s could choose what's built. Use HTTPS, or HTTP to a server on a private network, such as http://10.0.0.5/acme/web", raw, u.Hostname())
	}
	return u.Scheme + "://" + strings.ToLower(u.Host) + p, nil
}

// PrivateHost reports a host a repository may be fetched from over plain HTTP: an
// address on a private network, RFC 1918's or IPv6's unique local ones, or this
// machine, or a name only a private network answers, as .local and localhost are.
// Where a name leads is checked again as it's fetched.
func PrivateHost(host string) bool {
	if ip := net.ParseIP(host); ip != nil {
		return ip.IsPrivate() || ip.IsLoopback()
	}
	h := strings.ToLower(strings.TrimSuffix(host, "."))
	return h == "localhost" || strings.HasSuffix(h, ".local")
}

// checkGitRef checks a branch or a tag as Git names them, short or in full, or a
// commit's ID. Nothing else under refs/: a pull request's head, refs/pull/7/head,
// is whatever a fork pushed, and its branch check would pass.
func checkGitRef(ref string) error {
	if ref == "" || commitID.MatchString(strings.ToLower(ref)) {
		return nil
	}
	bad := func(why string) error {
		return fmt.Errorf("%q isn't a branch or a tag: %s", ref, why)
	}
	if strings.HasPrefix(ref, "refs/") && !strings.HasPrefix(ref, "refs/heads/") && !strings.HasPrefix(ref, "refs/tags/") {
		return bad("a build follows a branch or a tag of the repository, and what else is under refs/, such as a pull request's head, is what anyone with a fork pushed. Build the branch it merges into, or name a commit")
	}
	switch {
	case len(ref) > maxRef:
		return bad(fmt.Sprintf("Git names one in up to %d characters", maxRef))
	case strings.HasPrefix(ref, "-"), strings.HasPrefix(ref, "/"), strings.HasSuffix(ref, "/"), strings.HasSuffix(ref, "."):
		return bad("a name can't start with - or /, or end with / or .")
	case strings.Contains(ref, ".."), strings.Contains(ref, "//"), strings.Contains(ref, "@{"), strings.HasSuffix(ref, ".lock"):
		return bad("a name can't hold .., //, @{ or end with .lock")
	case strings.ContainsAny(ref, " ~^:?*[\\"), strings.IndexFunc(ref, unicode.IsControl) >= 0:
		return bad("a name can't hold a space or any of ~ ^ : ? * [ \\")
	}
	for _, part := range strings.Split(ref, "/") {
		if strings.HasPrefix(part, ".") {
			return bad("no part of a name can start with .")
		}
	}
	return nil
}

// buildPath checks a path in the repository, and writes it cleaned, with no ./ in
// front: "" for the top.
func buildPath(what, p string) (string, error) {
	if p == "" {
		return "", nil
	}
	if len(p) > maxPath || strings.IndexFunc(p, unicode.IsControl) >= 0 || strings.ContainsAny(p, ",=") {
		return "", fmt.Errorf("%q isn't a path a build can take for its %s: keep it under %d characters, with no , or =", p, what, maxPath)
	}
	if strings.HasPrefix(p, "/") {
		return "", fmt.Errorf("the build's %s is a path in the repository, such as web, so it can't start with /", what)
	}
	for _, part := range strings.Split(p, "/") {
		if part == ".." {
			return "", fmt.Errorf("the build's %s can't reach outside the repository with ../", what)
		}
	}
	c := path.Clean(p)
	if c == "." {
		c = ""
	}
	return c, nil
}

// buildWriter names the code a service that builds runs, for what it writes (ADR
// 0046), where one that runs an image is named by its image's repository, so that
// another tag of it, as CI deploys, is the same code. One that builds is its
// repository and how it builds: the folder,
// the Dockerfile, the stage and the build arguments, as ARG BASE in FROM $BASE
// reaches any image, and with Railpack the builder and the commands it runs. A new commit of the branch it follows is the same code, as a
// new tag is, and the build checks the commit is on the branch; a commit the build
// names as its ref is checked against no branch, so each is code of its own.
//
// A plan says it to people, so a build reads as its repository and a digest of how
// it builds, such as github.com/acme/web@build:5d41402abc4b2a76: 64 bits, which no
// one can choose a build's settings to match another's by.
func buildWriter(svc *resourcev1.Service) string {
	b := svc.GetBuild()
	g := b.GetGit()
	parts := []string{"git", g.GetUrl(), "context=" + b.GetContext(), "dockerfile=" + b.GetDockerfile(), "target=" + b.GetTarget()}
	// What builds it, and with Railpack the commands that build and start it, are
	// its code as much as its Dockerfile is.
	if b.GetBuilder() != resourcev1.ImageBuild_BUILDER_UNSPECIFIED || b.GetBuildCommand() != "" || b.GetStartCommand() != "" {
		parts = append(parts, "builder="+builderOf(b.GetBuilder()), "build="+b.GetBuildCommand(), "start="+b.GetStartCommand())
	}
	for _, k := range slices.Sorted(maps.Keys(b.GetArgs())) {
		parts = append(parts, "arg:"+k+"="+b.GetArgs()[k])
	}
	if ref := strings.ToLower(g.GetRef()); commitID.MatchString(ref) {
		parts = append(parts, "commit="+ref)
	}
	// What the image starts is part of its code: the service's own command and
	// entrypoint take the place of the start command its build gave it.
	app := svc.GetApp()
	if len(app.GetCommand()) > 0 || len(app.GetEntrypoint()) > 0 {
		parts = append(parts, "command="+strconv.Quote(strings.Join(app.GetCommand(), "\x00")), "entrypoint="+strconv.Quote(strings.Join(app.GetEntrypoint(), "\x00")))
	}
	// Each part quoted, so no value can make two builds read the same.
	for i := range parts {
		parts[i] = strconv.Quote(parts[i])
	}
	sum := sha256.Sum256([]byte(strings.Join(parts, " ")))
	return shownRepo(g.GetUrl()) + "@build:" + hex.EncodeToString(sum[:8])
}

// shownRepo is a repository's URL as people read it, with no scheme or .git.
func shownRepo(repo string) string {
	return strings.TrimSuffix(strings.TrimPrefix(strings.TrimPrefix(repo, "https://"), "http://"), ".git")
}

// buildPlan is what building a service's image takes, as a plan says it.
func buildPlan(svc *resourcev1.Service) *resource.BuildPlan {
	b := svc.GetBuild()
	g := b.GetGit()
	return &resource.BuildPlan{
		URL: g.GetUrl(), Ref: g.GetRef(), Commit: g.GetCommit(),
		Context: b.GetContext(), Dockerfile: b.GetDockerfile(), Image: svc.GetApp().GetImage(),
		Builder: builderOf(b.GetBuilder()),
	}
}

// builds reports a step that makes the app's container anew from an image it
// hasn't run before, which a service with a build has to build first.
func builds(actual, desired *resourcev1.Service, dec resource.Decision) bool {
	if desired.GetBuild() == nil || dec.Action != resource.ActionCreate && dec.Action != resource.ActionReplace {
		return false
	}
	return actual == nil || actual.GetApp().GetImage() != desired.GetApp().GetImage()
}

// build builds the image a service's app runs, on the agent, when its server hasn't
// it. One that fails says the step touched nothing, so what runs is what ran.
func (d *Driver) build(ctx context.Context, step resource.Step, svc *resourcev1.Service) error {
	b := svc.GetBuild()
	if b == nil || d.Builds == nil || step.Action != resource.ActionCreate && step.Action != resource.ActionReplace {
		return nil
	}
	secrets, err := binding.BuildSecrets(ctx, svc)
	if err != nil {
		return err
	}
	defer clear(secrets)
	req := Request(svc)
	req.Service, req.Change, req.Secrets = step.Ref, resource.DeployOf(ctx).Change, secrets
	req.Logins, req.Recheck = registry.BuildAuthsOf(ctx), registry.BuildRecheckOf(ctx)
	err = d.Builds.Build(ctx, req)
	if err != nil {
		resource.Report(ctx, resource.Outcome{Untouched: true})
	}
	return err
}

// maxBuildLogins is the most registry logins a build names.
const maxBuildLogins = 8

// checkBuildLogins checks the registry logins a build names for its base images:
// each a login's name, once (ADR 0086).
func checkBuildLogins(names []string) error {
	if len(names) > maxBuildLogins {
		return fmt.Errorf("a build names up to %d registry logins, and this names %d", maxBuildLogins, len(names))
	}
	seen := map[string]bool{}
	for _, n := range names {
		if err := resource.CheckLabel("registry login", n, "ghcr-ci"); err != nil {
			return err
		}
		if seen[n] {
			return fmt.Errorf("a build names the registry login %s twice. Name it once", n)
		}
		seen[n] = true
	}
	return nil
}
