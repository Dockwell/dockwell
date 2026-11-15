// SPDX-License-Identifier: AGPL-3.0-only

// Package githubapp is the "github.app" kind: a GitHub App Dockwell made through
// GitHub's manifest flow, whose installations let apps build from the repositories
// they reach, and deploy on push. It lives in the organization, on the control
// plane alone: its private key and webhook secret go to no server. See
// docs/adr/0098-connecting-github-as-an-app.md.
package githubapp

import (
	"cmp"
	"errors"
	"fmt"
	"regexp"
	"slices"
	"strings"

	"google.golang.org/protobuf/proto"

	resourcev1 "github.com/dockwell/dockwell/gen/dockwell/resource/v1alpha1"
	"github.com/dockwell/dockwell/internal/resource"
)

// Kind is the GitHub App kind.
const Kind resource.Kind = "github.app"

// maxReaches bounds the installations one App says the projects of.
const maxReaches = 500

// slugLike is what GitHub makes an App's slug of: lowercase letters, digits and
// hyphens.
var slugLike = regexp.MustCompile(`^[a-z0-9](?:[a-z0-9-]{0,62}[a-z0-9])?$`)

// hookIDLike is the ID in an App's webhook's address.
var hookIDLike = regexp.MustCompile(`^ghw_[A-Za-z0-9]{16,64}$`)

// Type is the kind.
type Type struct{}

var (
	_ resource.Type       = Type{}
	_ resource.Rotates    = Type{}
	_ resource.Keeper     = Type{}
	_ resource.NewChecker = Type{}
)

// Kind returns "github.app".
func (Type) Kind() resource.Kind { return Kind }

// Level is the organization: only an admin connects GitHub.
func (Type) Level() resource.Level { return resource.LevelOrganization }

// IDPrefix is "gha".
func (Type) IDPrefix() string { return "gha" }

// New returns an empty GitHubApp.
func (Type) New() proto.Message { return &resourcev1.GitHubApp{} }

// RotatesSensitive says a new key or webhook secret always plans as a change,
// never compared with what's there, so a plan can't confirm a guess at either
// (ADR 0022).
func (Type) RotatesSensitive() {}

var _ resource.ElsewhereActor = Type{}

// ActsElsewhere says why undoing a change to an App can't put back what it did on
// GitHub (ADR 0098): connecting made the App there, removing it uninstalled it,
// GitHub has a new webhook secret, and an old key may be deleted there. Which
// projects an installation reaches is Dockwell's alone, and undoes.
func (Type) ActsElsewhere(current, desired proto.Message) string {
	cur, _ := current.(*resourcev1.GitHubApp)
	des, _ := desired.(*resourcev1.GitHubApp)
	switch {
	case des == nil && cur == nil:
		return ""
	case cur == nil:
		return "connecting GitHub made the App on GitHub, which an undo can't take back. Remove it in Settings, Integrations, then delete it on GitHub"
	case des == nil:
		return "removing the GitHub App uninstalled it on GitHub, which an undo can't put back. Connect GitHub again from Settings, Integrations"
	case cur.GetPrivateKey() != des.GetPrivateKey():
		return "the GitHub App's old key may be deleted on GitHub already, so an undo can't put it back. Give it a new key from Settings, Integrations"
	case cur.GetWebhookSecret() != des.GetWebhookSecret():
		return "GitHub has the App's new webhook secret, which an undo can't take back. Give it a new one from Settings, Integrations"
	}
	return ""
}

// CheckName takes the App's slug, as GitHub made it.
func (Type) CheckName(name string) error {
	return resource.CheckLabel("GitHub App", name, "dockwell-deploy")
}

// Validate refuses what can't be a GitHub App Dockwell made, in words that hold
// nothing of its key or secret.
func (Type) Validate(spec proto.Message) error {
	a := spec.(*resourcev1.GitHubApp) //nolint:forcetypeassert // the kind's own message
	switch {
	case a.GetAppId() < 0:
		return errors.New("a GitHub App's ID is a positive number")
	case a.GetSlug() != "" && !slugLike.MatchString(a.GetSlug()):
		return fmt.Errorf("%q isn't a GitHub App's slug, as its page's address names it", a.GetSlug())
	case strings.ContainsAny(a.GetWebhookSecret()+a.GetPreviousWebhookSecret(), " \t\r\n"):
		return errors.New("the webhook secret has a space or a line break in it")
	case a.GetPrivateKey() != "" && !strings.Contains(a.GetPrivateKey(), "PRIVATE KEY-----"):
		return errors.New("the private key isn't one, as GitHub gives it: a .pem file that begins -----BEGIN RSA PRIVATE KEY-----")
	case a.GetHookId() != "" && !hookIDLike.MatchString(a.GetHookId()):
		return errors.New("the App's webhook ID isn't one Dockwell makes, ghw_ and letters and digits")
	case len(a.GetReaches()) > maxReaches:
		return fmt.Errorf("a GitHub App says which projects up to %d installations reach, and this says %d", maxReaches, len(a.GetReaches()))
	}
	seen := map[int64]bool{}
	for _, r := range a.GetReaches() {
		id := r.GetInstallationId()
		switch {
		case id <= 0:
			return errors.New("an installation's ID is a positive number, as GitHub gives it")
		case seen[id]:
			return fmt.Errorf("installation %d is named twice. Say each installation's projects once", id)
		}
		seen[id] = true
		for _, p := range r.GetProjects() {
			if !strings.HasPrefix(p, "prj_") {
				return fmt.Errorf("%q isn't a project's ID. An installation names the projects it reaches by ID, such as prj_…", p)
			}
		}
		projects := slices.Clone(r.GetProjects())
		slices.Sort(projects)
		r.Projects = slices.Compact(projects)
	}
	slices.SortFunc(a.Reaches, func(x, y *resourcev1.GitHubInstallationReach) int {
		return cmp.Compare(x.GetInstallationId(), y.GetInstallationId())
	})
	return nil
}

// CheckNew asks a new App for all GitHub gave for it: its ID, slug, key and
// webhook secret.
func (Type) CheckNew(spec proto.Message) error {
	a := spec.(*resourcev1.GitHubApp) //nolint:forcetypeassert // Validate saw it
	if a.GetAppId() == 0 || a.GetSlug() == "" || a.GetPrivateKey() == "" || a.GetWebhookSecret() == "" || a.GetHookId() == "" {
		return errors.New("a GitHub App is connected from Settings, Integrations, which makes it on GitHub: it can't be written by hand")
	}
	return nil
}

// Keep holds on to what an update leaves out, so an edit can say which projects an
// installation reaches with nothing else: the App's ID and names, its key and its
// secrets. An update that names another App, or another owner, is refused: that's
// another connection.
func (Type) Keep(current, desired proto.Message) (resource.Kept, error) {
	was, ok := current.(*resourcev1.GitHubApp)
	is, same := desired.(*resourcev1.GitHubApp)
	if !ok || !same {
		return resource.Kept{}, nil
	}
	switch {
	case is.GetAppId() != 0 && is.GetAppId() != was.GetAppId():
		return resource.Kept{}, errors.New("that's another GitHub App. Connect it as one of its own")
	case is.GetOwner() != "" && is.GetOwner() != was.GetOwner():
		return resource.Kept{}, errors.New("the App's owner is GitHub's to say. Connect another App for another account")
	}
	kept := false
	keep := func(field *string, from string) {
		if *field == "" && from != "" {
			*field, kept = from, true
		}
	}
	is.AppId = was.GetAppId()
	keep(&is.Slug, was.GetSlug())
	keep(&is.Owner, was.GetOwner())
	is.OwnerIsOrganization = was.GetOwnerIsOrganization()
	keep(&is.ClientId, was.GetClientId())
	is.HookId = was.GetHookId()
	if is.GetPrivateKey() == "" {
		keep(&is.PrivateKey, was.GetPrivateKey())
		is.KeyFingerprint = was.GetKeyFingerprint()
	}
	if is.GetWebhookSecret() == "" {
		keep(&is.WebhookSecret, was.GetWebhookSecret())
		is.PreviousWebhookSecret, is.PreviousUntil = was.GetPreviousWebhookSecret(), was.GetPreviousUntil()
	}
	if !kept {
		return resource.Kept{}, nil
	}
	return resource.Kept{Reason: "kept_key", Words: "keeps the key and webhook secret GitHub gave it"}, nil
}

// Reaches says whether installation reaches project's apps: every project, unless
// the App says which.
func Reaches(a *resourcev1.GitHubApp, installation int64, project string) bool {
	for _, r := range a.GetReaches() {
		if r.GetInstallationId() == installation {
			return slices.Contains(r.GetProjects(), project)
		}
	}
	return true
}
