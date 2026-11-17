// SPDX-License-Identifier: AGPL-3.0-only

package catalogapi

import (
	"cmp"
	"context"
	"errors"
	"fmt"
	"maps"
	"net/mail"
	"net/url"
	"regexp"
	"slices"
	"strings"

	"connectrpc.com/connect"

	"google.golang.org/protobuf/proto"

	catalogv1 "github.com/dockwell/dockwell/gen/dockwell/catalog/v1alpha1"
	resourcev1 "github.com/dockwell/dockwell/gen/dockwell/resource/v1alpha1"
	dockwellv1 "github.com/dockwell/dockwell/gen/dockwell/v1"
	"github.com/dockwell/dockwell/internal/catalog"
	"github.com/dockwell/dockwell/internal/control/authapi"
	"github.com/dockwell/dockwell/internal/control/changes"
	"github.com/dockwell/dockwell/internal/control/holds"
	"github.com/dockwell/dockwell/internal/resource"
	"github.com/dockwell/dockwell/internal/resource/appowner"
	"github.com/dockwell/dockwell/internal/store"
)

// ownerAsk is what an install of an entry whose owner Dockwell makes takes from
// the request: the owner's email, theirs or the person's installing it, and the
// user that starts from it (ADR 0105).
type ownerAsk struct {
	// steps says Dockwell makes the owner.
	steps bool
	email string
	// values are the request's values, with the owner's email and user added.
	values map[string]string
	// entry is the entry to fill.
	entry *catalogv1.Entry
}

// askOwner works out what an install takes for its owner. An install by a person
// starts from their email; one by a token with none given waits for one, held.
func askOwner(ctx context.Context, entry *catalogv1.Entry, m *dockwellv1.InstallCatalogEntryRequest) ownerAsk {
	o := entry.GetOwner()
	ask := ownerAsk{steps: len(o.GetSteps()) > 0, values: maps.Clone(m.GetValues()), entry: entry}
	if !ask.steps {
		return ask
	}
	if ask.values == nil {
		ask.values = map[string]string{}
	}
	person := ""
	if p, ok := authapi.PrincipalFrom(ctx); ok && p.Token == nil && p.Hook == nil {
		person = p.User.Email
	}
	ask.email = strings.TrimSpace(cmp.Or(m.GetOwnerEmail(), ask.values[o.GetEmailInput()], person))
	if in := o.GetEmailInput(); in != "" && ask.email != "" {
		ask.values[in] = ask.email
	}
	if in := o.GetUserInput(); in != "" && strings.TrimSpace(ask.values[in]) == "" && ask.email != "" {
		ask.values[in] = userFrom(ask.email, o.GetUserPattern())
	}
	return ask
}

// userFrom is a user name from an email's local part, with what pattern doesn't take
// left out, as Gitea takes no @.
func userFrom(email, pattern string) string {
	local, _, _ := strings.Cut(email, "@")
	if pattern == "" {
		return local
	}
	re, err := regexp.Compile(pattern)
	if err != nil || re.MatchString(local) {
		return local
	}
	var b strings.Builder
	for _, r := range local {
		if re.MatchString(b.String() + string(r)) {
			b.WriteRune(r)
		}
	}
	return cmp.Or(b.String(), "owner")
}

// ownerEdit is the app.owner an install makes: who the owner is, and never how
// they're made, which the entry at the install's release says as the owner is made
// (ADR 0105).
func ownerEdit(entry *catalogv1.Entry, ask ownerAsk, filled *catalog.Filled) *dockwellv1.Edit {
	o := entry.GetOwner()
	if !ask.steps {
		return nil
	}
	owner := &resourcev1.AppOwner{
		Service:  o.GetService(),
		Email:    ask.email,
		User:     filled.Plain[o.GetUserInput()],
		Password: o.GetPasswordInput(),
		App:      entry.GetName(),
	}
	return &dockwellv1.Edit{
		Resource: &dockwellv1.ResourceRef{Kind: string(appowner.Kind), Name: appowner.Name},
		Spec:     &resourcev1.Spec{Kind: &resourcev1.Spec_AppOwner{AppOwner: owner}},
	}
}

// ErrNoRelease is an install whose catalog no longer has the release it runs, whose
// steps Dockwell can't read to make its owner.
var ErrNoRelease = errors.New("no such release")

// OwnerRun is what the agent runs to make an environment's install's owner: the
// steps of its entry at the release it runs, read from its catalog now, never from
// the app.owner, with who the owner is and where the app is reached (ADR 0103,
// 0105). It returns the plain values the steps name and the secrets they name, by
// input name, as the environment's secrets' names, which the caller reads.
func (h *Handler) OwnerRun(ctx context.Context, environment string) (*resourcev1.OwnerRun, map[string]string, []string, error) {
	s, err := h.catalog(ctx)
	if err != nil {
		return nil, nil, nil, err
	}
	var pin *resourcev1.CatalogPin
	var owner *installOwner
	err = h.db.View(ctx, func(tx *store.Tx) error {
		installs, err := h.installs(ctx, tx, s)
		if err != nil {
			return err
		}
		for _, in := range installs {
			if in.GetEnvironmentId() == environment {
				pin = in.GetPin()
			}
		}
		owner, err = h.ownerIn(ctx, tx, environment)
		return err
	})
	if err != nil {
		return nil, nil, nil, err
	}
	if pin == nil || owner == nil {
		return nil, nil, nil, fmt.Errorf("%w: this environment has no catalog install whose owner Dockwell makes", store.ErrNotFound)
	}
	entry, known := s.entryFor(pin)
	if !known || entry.GetRelease() != pin.GetRelease() {
		return nil, nil, nil, fmt.Errorf("%w: its catalog no longer has release %d of %s, whose steps make its owner. Upgrade it, then try again", ErrNoRelease, pin.GetRelease(), pin.GetEntry())
	}
	o := entry.GetOwner()
	if len(o.GetSteps()) == 0 {
		return nil, nil, nil, fmt.Errorf("%w: release %d of %s doesn't say how its owner is made", ErrNoRelease, pin.GetRelease(), pin.GetEntry())
	}
	run := &resourcev1.OwnerRun{
		Service: o.GetService(), Email: owner.spec.GetEmail(), User: owner.spec.GetUser(), Password: o.GetPasswordInput(),
		Steps: cloneSteps(o.GetSteps()), Done: proto.CloneOf(o.GetDone()), MadeWhenDone: o.GetMadeWhenDone(),
	}
	if su := entry.GetSignups(); len(su.GetSteps()) > 0 && !pin.GetSignupsOpen() {
		run.SignupsSteps, run.SignupsCheck = cloneSteps(su.GetSteps()), proto.CloneOf(su.GetCheck())
	}
	if u, err := url.Parse(pin.GetAddress()); err == nil && u.Host != "" {
		run.Host, run.Https = u.Host, u.Scheme == "https"
	}
	named := catalog.OwnerInputs(entry)
	plain := map[string]string{}
	var secretNames []string
	for _, in := range entry.GetInputs() {
		name := in.GetName()
		if !named[name] {
			continue
		}
		if catalog.Secret(in) {
			secretNames = append(secretNames, name)
			continue
		}
		plain[name] = pin.GetValues()[name]
	}
	if in := o.GetEmailInput(); in != "" {
		plain[in] = run.GetEmail()
	}
	if in := o.GetUserInput(); in != "" {
		plain[in] = run.GetUser()
	}
	if o.GetKnownUser() != "" || o.GetKnownPassword() != "" {
		plain["KNOWN_USER"], plain["KNOWN_PASSWORD"] = o.GetKnownUser(), o.GetKnownPassword()
	}
	slices.Sort(secretNames)
	return run, plain, secretNames, nil
}

func cloneSteps(steps []*resourcev1.OwnerStep) []*resourcev1.OwnerStep {
	out := make([]*resourcev1.OwnerStep, len(steps))
	for i, s := range steps {
		out[i] = proto.CloneOf(s)
	}
	return out
}

// closedSignups is what an install writes in a service's environment to close its
// sign-ups, where its owner is made and a variable closes them, and the person
// didn't ask for them open: by service, then variable.
func closedSignups(entry *catalogv1.Entry, ask ownerAsk, signupsOpen bool) map[string]map[string]string {
	s := entry.GetSignups()
	if !ask.steps || signupsOpen || s.GetVariable() == "" {
		return nil
	}
	return map[string]map[string]string{s.GetService(): {s.GetVariable(): s.GetClosed()}}
}

// heldAtInstall says an install starts held: its first visitor makes its owner, and
// nothing makes it first; or it's converted from another store, with web, and its
// entry says no way it's owned: no owner steps, no admin its environment makes
// (sign_in with a password input), and not none. A password everyone knows, which
// nothing changes, is no way (ADR 0105).
func heldAtInstall(entry *catalogv1.Entry, converted bool) bool {
	o := entry.GetOwner()
	if len(o.GetSteps()) > 0 || o.GetNone() {
		return false
	}
	if entry.GetFirstVisitorOwns() {
		return true
	}
	return converted && entry.GetWeb().GetService() != "" && entry.GetSignIn().GetPasswordInput() == ""
}

// OwnerRetrier has a failed owner tried again (owners.Runner).
type OwnerRetrier interface {
	Retry(ctx context.Context, environment string) (store.AppOwnerState, error)
	// SetUp says whether a failed owner's app read set up, by its done, while a
	// person set it up through its setup link.
	SetUp(environment string) bool
}

// SetHolds has the handler work out whether an install is held with hs, and try a
// failed owner again with owners (ADR 0105).
func (h *Handler) SetHolds(hs *holds.Holds, owners OwnerRetrier) {
	h.holds, h.owners = hs, owners
}

// ownerUnmade says whether an environment has an owner that isn't made or opened.
func (h *Handler) ownerUnmade(ctx context.Context, environment string) (bool, error) {
	var out bool
	err := h.db.View(ctx, func(tx *store.Tx) error {
		owner, err := h.ownerIn(ctx, tx, environment)
		out = owner != nil && !store.OwnerFinal(owner.state.State)
		return err
	})
	return out, err
}

// installOwner is an install's app.owner, and what's observed of it.
type installOwner struct {
	res   store.Resource
	spec  *resourcev1.AppOwner
	state store.AppOwnerState
}

// ownerIn reads an environment's app.owner, nil where it has none.
func (h *Handler) ownerIn(ctx context.Context, tx *store.Tx, environment string) (*installOwner, error) {
	r, err := tx.GetResource(ctx, string(appowner.Kind), environment, appowner.Name)
	if errors.Is(err, store.ErrNotFound) || err == nil && r.Spec == nil {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	spec, err := h.keeper.Decode(ctx, tx, store.InRevision(r.ID, r.Kind, r.Revision), r.Spec)
	if err != nil {
		return nil, err
	}
	o, _ := spec.(*resourcev1.AppOwner)
	states, err := tx.AppOwnerStates(ctx, []string{r.ID})
	if err != nil {
		return nil, err
	}
	st, ok := states[r.ID]
	if !ok {
		st = store.AppOwnerState{ResourceID: r.ID, Environment: environment, State: store.OwnerWaiting}
		if o.GetEmail() == "" && o.GetUser() == "" {
			st.State = store.OwnerWaitingEmail
		}
	}
	return &installOwner{res: r, spec: o, state: st}, nil
}

// ownerProto is an install's owner as the API says it.
func ownerProto(o *installOwner) *dockwellv1.CatalogOwner {
	if o == nil {
		return nil
	}
	state := map[string]dockwellv1.CatalogOwnerState{
		store.OwnerWaitingEmail: dockwellv1.CatalogOwnerState_CATALOG_OWNER_STATE_WAITING_EMAIL,
		store.OwnerWaiting:      dockwellv1.CatalogOwnerState_CATALOG_OWNER_STATE_WAITING,
		store.OwnerMade:         dockwellv1.CatalogOwnerState_CATALOG_OWNER_STATE_MADE,
		store.OwnerFailed:       dockwellv1.CatalogOwnerState_CATALOG_OWNER_STATE_FAILED,
		store.OwnerOpened:       dockwellv1.CatalogOwnerState_CATALOG_OWNER_STATE_OPENED,
	}[o.state.State]
	out := &dockwellv1.CatalogOwner{Email: o.spec.GetEmail(), User: o.spec.GetUser(), State: state, Error: o.state.Error, Rotating: o.state.Rotation != ""}
	// A made owner's error is what kept its last new password from being set.
	if o.state.State == store.OwnerMade {
		out.Error = o.state.RotateError
	}
	return out
}

// RetryCatalogOwner tries again to make an install's owner where it failed: the
// next look reads whether it's made first (ADR 0105).
func (h *Handler) RetryCatalogOwner(ctx context.Context, req *connect.Request[dockwellv1.RetryCatalogOwnerRequest]) (*connect.Response[dockwellv1.RetryCatalogOwnerResponse], error) {
	s, err := h.catalog(ctx)
	if err != nil {
		return nil, err
	}
	found, err := h.find(ctx, s, req.Msg.GetId())
	if err != nil {
		return nil, err
	}
	if h.owners == nil {
		return nil, connect.NewError(connect.CodeUnavailable, errors.New("this control plane doesn't make apps' owners"))
	}
	// Trying again takes a setup link made for doing it by hand away, first, so a
	// retry that runs has done that (ADR 0105; the review's L1).
	if err := h.clearSetups(ctx, found.install); err != nil {
		return nil, err
	}
	st, err := h.owners.Retry(ctx, found.install.GetEnvironmentId())
	if errors.Is(err, store.ErrNotFound) {
		return nil, authapi.ReasonError(connect.CodeFailedPrecondition, fmt.Errorf("there's nothing to try again: Dockwell doesn't make %s's owner", found.install.GetProject()), "no_owner")
	}
	if err != nil {
		return nil, h.internal(ctx, err)
	}
	var owner *dockwellv1.CatalogOwner
	err = h.db.View(ctx, func(tx *store.Tx) error {
		o, err := h.ownerIn(ctx, tx, found.install.GetEnvironmentId())
		if o != nil {
			o.state = st
		}
		owner = ownerProto(o)
		return err
	})
	if err != nil {
		return nil, h.internal(ctx, err)
	}
	return connect.NewResponse(&dockwellv1.RetryCatalogOwnerResponse{Owner: owner}), nil
}

// SetCatalogOwnerEmail plans giving the owner's email to an install a token made
// without one. Once it's applied, Dockwell makes the owner (ADR 0105).
func (h *Handler) SetCatalogOwnerEmail(ctx context.Context, req *connect.Request[dockwellv1.SetCatalogOwnerEmailRequest]) (*connect.Response[dockwellv1.SetCatalogOwnerEmailResponse], error) {
	email := strings.TrimSpace(req.Msg.GetEmail())
	if a, err := mail.ParseAddress(email); err != nil || a.Address != email {
		return nil, authapi.Invalid(fmt.Errorf("%q isn't an email address, such as you@example.com", email))
	}
	s, err := h.catalog(ctx)
	if err != nil {
		return nil, err
	}
	found, err := h.find(ctx, s, req.Msg.GetId())
	if err != nil {
		return nil, err
	}
	install := found.install
	var o *installOwner
	if err := h.db.View(ctx, func(tx *store.Tx) (err error) {
		o, err = h.ownerIn(ctx, tx, install.GetEnvironmentId())
		return err
	}); err != nil {
		return nil, h.internal(ctx, err)
	}
	if o == nil || o.spec.GetEmail() != "" || store.OwnerFinal(o.state.State) {
		return nil, authapi.ReasonError(connect.CodeFailedPrecondition, fmt.Errorf("%s isn't waiting for its owner's email", install.GetProject()), "not_waiting")
	}
	spec := proto.CloneOf(o.spec)
	spec.Email = email
	if entry, known := s.entryFor(install.GetPin()); known {
		if eo := entry.GetOwner(); eo.GetUserInput() != "" && spec.GetUser() == "" {
			spec.User = userFrom(email, eo.GetUserPattern())
		}
	}
	edit := &dockwellv1.Edit{
		Resource: &dockwellv1.ResourceRef{Kind: string(appowner.Kind), Scope: install.GetEnvironmentId(), Name: appowner.Name},
		Spec:     &resourcev1.Spec{Kind: &resourcev1.Spec_AppOwner{AppOwner: spec}},
	}
	change, err := h.changes.Propose(ctx, fmt.Sprintf("Make %s's owner as %s", install.GetProject(), email), []*dockwellv1.Edit{edit}, nil, changes.Also{Base: o.res.Revision, Owners: []resource.Ref{{Kind: appowner.Kind, Scope: install.GetEnvironmentId(), Name: appowner.Name}}})
	if err != nil {
		return nil, err
	}
	return connect.NewResponse(&dockwellv1.SetCatalogOwnerEmailResponse{Change: change}), nil
}
