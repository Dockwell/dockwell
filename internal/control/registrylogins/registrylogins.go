// SPDX-License-Identifier: AGPL-3.0-only

// Package registrylogins picks the registry login a pull, a check for updates or
// a read for a form signs in with, and opens it for the one call that sends it to
// a server, sealed (ADR 0086). It never logs a login, and nothing it says holds one.
package registrylogins

import (
	"context"
	"errors"
	"fmt"
	"slices"
	"strconv"
	"strings"

	"connectrpc.com/connect"
	"google.golang.org/protobuf/proto"

	resourcev1 "github.com/dockwell/dockwell/gen/dockwell/resource/v1alpha1"
	"github.com/dockwell/dockwell/internal/control/authapi"
	"github.com/dockwell/dockwell/internal/control/place"
	"github.com/dockwell/dockwell/internal/id"
	"github.com/dockwell/dockwell/internal/registry"
	"github.com/dockwell/dockwell/internal/resource"
	"github.com/dockwell/dockwell/internal/resource/registrylogin"
	"github.com/dockwell/dockwell/internal/scope"
	"github.com/dockwell/dockwell/internal/secrets"
	"github.com/dockwell/dockwell/internal/store"
)

// Logins reads the saved registry logins.
type Logins struct {
	db     *store.DB
	keeper *secrets.Keeper
	// ECR gets Amazon ECR's sign-in tokens.
	ECR *ECR
}

// New reads logins from db, opened with keeper.
func New(db *store.DB, keeper *secrets.Keeper) *Logins {
	return &Logins{db: db, keeper: keeper, ECR: NewECR()}
}

// Saved is a login as saved, with what signs in: it goes no further than a call to
// an agent, sealed.
type Saved struct {
	ID, Name string
	// Revision is the login's revision, which a plan's fingerprint covers.
	Revision int
	Spec     *resourcev1.RegistryLogin
}

// ErrNoLogin is a named login that isn't there, or that the project may not use,
// in words that don't say which.
var ErrNoLogin = errors.New("there's no such registry login this app may use")

// All reads every saved login, sorted by name.
func (l *Logins) All(ctx context.Context) ([]Saved, error) {
	var out []Saved
	err := l.db.View(ctx, func(tx *store.Tx) error {
		var err error
		out, err = l.all(ctx, tx)
		return err
	})
	return out, err
}

func (l *Logins) all(ctx context.Context, tx *store.Tx) ([]Saved, error) {
	stored, err := tx.ListKind(ctx, string(registrylogin.Kind))
	if err != nil {
		return nil, err
	}
	out := make([]Saved, 0, len(stored))
	for i := range stored {
		r := &stored[i]
		if r.Spec == nil {
			continue
		}
		spec, err := l.keeper.Decode(ctx, tx, store.InRevision(r.ID, r.Kind, r.Revision), r.Spec)
		if err != nil {
			return nil, fmt.Errorf("can't read the registry login %s: %w", r.Name, err)
		}
		login, ok := spec.(*resourcev1.RegistryLogin)
		if !ok {
			continue
		}
		out = append(out, Saved{ID: r.ID, Name: r.Name, Revision: r.Revision, Spec: login})
	}
	slices.SortFunc(out, func(a, b Saved) int { return strings.Compare(a.Name, b.Name) })
	return out, nil
}

// Allows says whether a login lets project's apps pull with it: one that names no
// project lets every project.
func Allows(s *resourcev1.RegistryLogin, project string) bool {
	return len(s.GetProjects()) == 0 || slices.Contains(s.GetProjects(), project)
}

// ProjectOf is the project a scope is in: an environment's, or a project itself.
func ProjectOf(ctx context.Context, tx *store.Tx, scopeID string) (string, error) {
	prefix, _, _ := strings.Cut(scopeID, "_")
	switch prefix {
	case id.Project:
		return scopeID, nil
	case id.Environment:
		env, err := tx.GetResourceByID(ctx, scopeID)
		if errors.Is(err, store.ErrNotFound) {
			// One a change makes as it goes: its project is new too, so no login
			// names it yet, and only those every project may use are its.
			return "", nil
		}
		if err != nil {
			return "", err
		}
		if env.Kind != string(scope.EnvironmentKind) {
			return "", fmt.Errorf("%s isn't an environment", scopeID)
		}
		return env.Scope, nil
	}
	return "", nil
}

// Pick is the login a pull of image signs in with, for project's apps: the one
// named, or with none named, the only one for the image's registry the project may
// use. ok is false for none, as for a public image. A login named that isn't there,
// or that the project may not use, is ErrNoLogin.
func (l *Logins) Pick(ctx context.Context, project, image, named string) (Saved, bool, error) {
	ref, err := registry.Parse(image)
	if err != nil {
		if named != "" {
			return Saved{}, false, err
		}
		// What can't be read as an image has no registry: the plan says why.
		return Saved{}, false, nil
	}
	all, err := l.All(ctx)
	if err != nil {
		return Saved{}, false, err
	}
	return pick(all, project, ref.Registry, named)
}

func pick(all []Saved, project, host, named string) (Saved, bool, error) {
	if named != "" {
		for _, s := range all {
			if s.Name != named || !Allows(s.Spec, project) {
				continue
			}
			if s.Spec.GetRegistry() != host {
				return Saved{}, false, authapi.ReasonError(connect.CodeFailedPrecondition,
					fmt.Errorf("the registry login %s signs in to %s, and the image is on %s. Pick a login for %s, or none", named, s.Spec.GetRegistry(), host, host), "registry_login_elsewhere")
			}
			return s, true, nil
		}
		return Saved{}, false, authapi.ReasonError(connect.CodeNotFound,
			fmt.Errorf("there's no registry login %s this app may use. List them with ListRegistryLogins, or ask an admin to let its project use one", named), "registry_login_not_found")
	}
	var found []Saved
	for _, s := range all {
		if s.Spec.GetRegistry() == host && Allows(s.Spec, project) {
			found = append(found, s)
		}
	}
	if len(found) != 1 {
		// None, or several, which the app picks from by name.
		return Saved{}, false, nil
	}
	return found[0], true, nil
}

// Usable lists the logins for host that project may use, by name.
func Usable(all []Saved, project, host string) []string {
	var out []string
	for _, s := range all {
		if s.Spec.GetRegistry() == host && Allows(s.Spec, project) {
			out = append(out, s.Name)
		}
	}
	return out
}

// Open is what a call sends to an agent of a saved login: a username and password,
// or for ECR, its current token.
func (l *Logins) Open(ctx context.Context, s Saved) (registry.Auth, error) {
	a := registry.Auth{
		Name: s.Name, Registry: s.Spec.GetRegistry(), Username: s.Spec.GetUsername(), Password: s.Spec.GetPassword(), TokenHost: s.Spec.GetTokenHost(),
		Version: s.ID + "@" + strconv.Itoa(s.Revision),
	}
	if e := s.Spec.GetEcr(); e != nil {
		token, err := l.ECR.Token(ctx, s.Spec.GetRegistry(), e)
		if err != nil {
			return registry.Auth{}, fmt.Errorf("can't get Amazon ECR's sign-in token for the registry login %s: %w", s.Name, err)
		}
		a.Username, a.Password, a.TokenHost = "AWS", token, ""
	}
	return a, nil
}

// Of is the image a spec pulls, and the login it names, for the kinds that pull
// one: an app's container. An app Dockwell builds pulls nothing (ADR 0046).
func Of(spec proto.Message) (image, named string, ok bool) {
	switch s := spec.(type) {
	case *resourcev1.Container:
		return s.GetImage(), s.GetRegistryLogin(), s.GetImage() != ""
	case *resourcev1.Service:
		if s.GetBuild() != nil {
			return "", "", false
		}
		c := s.GetApp()
		return c.GetImage(), c.GetRegistryLogin(), c.GetImage() != ""
	}
	return "", "", false
}

// For is the login ref's pull signs in with, as its spec says, opened for one call.
// named, when set, is the one its plan picked, which an apply uses and no other.
// ok is false for none.
func (l *Logins) For(ctx context.Context, ref resource.Ref, spec proto.Message, planned string) (registry.Auth, bool, error) {
	image, named, ok := Of(spec)
	if !ok {
		return registry.Auth{}, false, nil
	}
	var project string
	err := l.db.View(ctx, func(tx *store.Tx) error {
		var err error
		project, err = ProjectOf(ctx, tx, ref.Scope)
		return err
	})
	if err != nil {
		return registry.Auth{}, false, err
	}
	if planned != "" {
		named = planned
	}
	s, ok, err := l.Pick(ctx, project, image, named)
	if err != nil || !ok {
		return registry.Auth{}, false, err
	}
	a, err := l.Open(ctx, s)
	return a, err == nil, err
}

// ProjectFor is the project a request names, by its environment's ID, or by the
// project's own ID or name. Empty for none, whose apps may use only the logins
// every project may.
func (l *Logins) ProjectFor(ctx context.Context, environment, project string) (string, error) {
	var out string
	err := l.db.View(ctx, func(tx *store.Tx) error {
		if strings.HasPrefix(environment, id.Environment+"_") {
			got, err := ProjectOf(ctx, tx, environment)
			out = got
			return err
		}
		if project == "" {
			return nil
		}
		got, err := place.Project(ctx, tx, project)
		out = got
		return err
	})
	if errors.Is(err, store.ErrNotFound) {
		return "", authapi.ReasonError(connect.CodeNotFound, errors.New("there's no such project or environment. List them to find its name"), "not_found")
	}
	return out, err
}

// BuildLogins are the logins ref's build may pull its base images with: those it
// names in ImageBuild.registry_logins, and no other, as a build's Dockerfile can read
// any image a login it's given can (ADR 0086). Naming none gives it none; one the
// project may not use, two for one registry, or one for a registry on the loopback,
// which a build's steps share, refuses the plan. A build Railpack makes is given
// none, as its images are Railpack's. planned, when set, is what the plan named,
// which an apply uses and no other.
func (l *Logins) BuildLogins(ctx context.Context, ref resource.Ref, spec proto.Message, planned []string) ([]Saved, error) {
	svc, ok := spec.(*resourcev1.Service)
	if !ok || svc.GetBuild() == nil || svc.GetBuild().GetBuilder() == resourcev1.ImageBuild_BUILDER_RAILPACK {
		return nil, nil
	}
	named := svc.GetBuild().GetRegistryLogins()
	if planned != nil {
		named = planned
	}
	if len(named) == 0 {
		return nil, nil
	}
	var project string
	err := l.db.View(ctx, func(tx *store.Tx) error {
		var err error
		project, err = ProjectOf(ctx, tx, ref.Scope)
		return err
	})
	if err != nil {
		return nil, err
	}
	all, err := l.All(ctx)
	if err != nil {
		return nil, err
	}
	return pickBuild(all, project, named, planned != nil)
}

func pickBuild(all []Saved, project string, named []string, applying bool) ([]Saved, error) {
	usable := map[string]Saved{}
	for _, s := range all {
		if Allows(s.Spec, project) {
			usable[s.Name] = s
		}
	}
	chosen := map[string]Saved{}
	out := make([]Saved, 0, len(named))
	for _, n := range named {
		s, ok := usable[n]
		switch {
		case !ok && applying:
			return nil, authapi.ReasonError(connect.CodeFailedPrecondition,
				fmt.Errorf("the plan builds with the registry login %s, which this app may no longer use. Plan it again", n), "registry_login_not_found")
		case !ok:
			return nil, authapi.ReasonError(connect.CodeNotFound,
				fmt.Errorf("there's no registry login %s this app may use. List them with ListRegistryLogins, or ask an admin to let its project use one", n), "registry_login_not_found")
		case registry.OnLoopback(s.Spec.GetRegistry()):
			return nil, authapi.ReasonError(connect.CodeFailedPrecondition,
				fmt.Errorf("the registry login %s signs in to %s, on the server's own loopback, which a build's steps share, so a build can't use it: a step listening there would get it. Name another, or none", n, s.Spec.GetRegistry()), "registry_login_loopback")
		}
		if was, twice := chosen[s.Spec.GetRegistry()]; twice {
			return nil, authapi.ReasonError(connect.CodeFailedPrecondition,
				fmt.Errorf("the build names %s and %s, which both sign in to %s. Name one", was.Name, n, s.Spec.GetRegistry()), "registry_login_twice")
		}
		chosen[s.Spec.GetRegistry()] = s
		out = append(out, s)
	}
	slices.SortFunc(out, func(a, b Saved) int { return strings.Compare(a.Name, b.Name) })
	return out, nil
}

// BuildNames names the logins ref's build may pull its base images with, for its
// plan, without opening any, and says which revision of which saved login each is.
func (l *Logins) BuildNames(ctx context.Context, ref resource.Ref, spec proto.Message) (names, versions []string, err error) {
	saved, err := l.BuildLogins(ctx, ref, spec, nil)
	if err != nil {
		return nil, nil, err
	}
	for _, s := range saved {
		names = append(names, s.Name)
		versions = append(versions, s.ID+"@"+strconv.Itoa(s.Revision))
	}
	return names, versions, nil
}

// ForBuild opens the logins a step's build pulls its base images with: those its
// plan named, and no other.
func (l *Logins) ForBuild(ctx context.Context, ref resource.Ref, spec proto.Message, planned, versions []string) ([]registry.Auth, error) {
	saved, err := l.BuildLogins(ctx, ref, spec, planned)
	if err != nil {
		return nil, err
	}
	if err := samePlanned(saved, planned, versions); err != nil {
		return nil, err
	}
	out := make([]registry.Auth, 0, len(saved))
	for _, s := range saved {
		a, err := l.Open(ctx, s)
		if err != nil {
			return nil, err
		}
		out = append(out, a)
	}
	return out, nil
}

// samePlanned refuses a login saved again since its plan, under its name: each is
// the revision of the login the plan saw, and another is another login as far as
// the plan goes.
func samePlanned(saved []Saved, planned, versions []string) error {
	byName := map[string]string{}
	for i, n := range planned {
		if i < len(versions) {
			byName[n] = versions[i]
		}
	}
	for _, s := range saved {
		if want, ok := byName[s.Name]; !ok || want != s.ID+"@"+strconv.Itoa(s.Revision) {
			return authapi.ReasonError(connect.CodeFailedPrecondition,
				fmt.Errorf("the registry login %s changed since this plan was made. Plan it again", s.Name), "registry_login_changed")
		}
	}
	return nil
}
