// SPDX-License-Identifier: AGPL-3.0-only

package revision

import (
	"context"
	"fmt"
	"slices"

	"google.golang.org/protobuf/proto"

	resourcev1 "github.com/dockwell/dockwell/gen/dockwell/resource/v1alpha1"
	"github.com/dockwell/dockwell/internal/fault"
	"github.com/dockwell/dockwell/internal/resource"
	"github.com/dockwell/dockwell/internal/resource/signinprovider"
	"github.com/dockwell/dockwell/internal/store"
)

// routeKind is what a route is, as the engine reads one apart.
const routeKind resource.Kind = "route"

// fillSignIn fills in what a route that signs people in needs of its provider on
// its server, from the provider as the change leaves it, or as it's saved: its
// issuer, client and secret, what it asks for, its groups claim, and its revision,
// so a plan made with another reads as stale. What an edit says of them is
// replaced, as only an admin's provider decides them. The route's project must be
// one the provider lets use it (ADR 0087).
func (e *Engine) fillSignIn(ctx context.Context, tx *store.Tx, items []item, ref resource.Ref, r *resourcev1.Route) error {
	s := r.GetAccess().GetSignIn()
	if s == nil {
		return nil
	}
	where, name := r.GetDomain()+r.GetPath(), s.GetProvider()
	p, revision, err := e.providerAfter(ctx, tx, items, name, true)
	switch {
	case err != nil:
		return err
	case revision < 0:
		return fmt.Errorf("the change deletes the sign-in provider %s, which %s signs people in with. Take sign-in off it, or pick another provider, in the same change", name, where)
	case p == nil:
		return fmt.Errorf("%s signs people in with %q, and there's no sign-in provider by that name. "+
			"Pick one of the providers in Settings, Integrations, or ask an admin to add it", where, name)
	}
	project := projectOfScope(ctx, tx, items, ref.Scope)
	if len(p.GetProjects()) > 0 && !slices.Contains(p.GetProjects(), project) {
		return fmt.Errorf("%s's project may not use the sign-in provider %s. Pick another, or ask an admin to let this project use it", where, name)
	}
	// At a provider anyone has an account at, anyone at a mailbox domain is anyone at
	// all (#598's final check, L3).
	if signinprovider.Public(p.GetIssuer()) {
		for _, d := range s.GetEmailDomains() {
			if signinprovider.Mailbox(d) {
				return fmt.Errorf("%s lets in anyone at %s, where anyone can make an address, and %s takes anyone's account, so it would let in the world. Name each person at %s by email", where, d, name, d)
			}
		}
	}
	s.Filled = &resourcev1.RouteSignInProvider{
		Issuer: p.GetIssuer(), ClientId: p.GetClientId(), ClientSecret: p.GetClientSecret(),
		Scopes: slices.Clone(p.GetScopes()), GroupsClaim: p.GetGroupsClaim(), AnyAccount: p.GetAnyAccount(),
		Revision: int64(revision),
	}
	return nil
}

// providerAfter is a sign-in provider as a change leaves it, with the revision it
// will have: the change's own edit of it, or what's saved. Its revision is -1 where
// the change deletes it, and nil and 0 where there's none. Without spec, it reads
// the revision alone.
func (e *Engine) providerAfter(ctx context.Context, tx *store.Tx, items []item, name string, spec bool) (*resourcev1.SigninProvider, int, error) {
	if i := slices.IndexFunc(items, func(it item) bool { return it.ref.Kind == signinprovider.Kind && it.ref.Name == name }); i >= 0 {
		it := &items[i]
		if it.spec == nil {
			return nil, -1, nil
		}
		revision := it.base
		if it.changes() {
			revision++
		}
		p, _ := it.spec.(*resourcev1.SigninProvider)
		return p, revision, nil
	}
	stored, err := tx.ListKind(ctx, string(signinprovider.Kind))
	if err != nil {
		return nil, 0, err
	}
	j := slices.IndexFunc(stored, func(p store.Resource) bool { return p.Name == name && p.Spec != nil })
	if j < 0 {
		return nil, 0, nil
	}
	saved := &stored[j]
	if !spec {
		return &resourcev1.SigninProvider{}, saved.Revision, nil
	}
	decoded, err := e.decode(ctx, tx, store.InRevision(saved.ID, saved.Kind, saved.Revision), saved.Spec)
	if err != nil {
		return nil, 0, fmt.Errorf("can't read the sign-in provider %s: %w", name, err)
	}
	p, ok := decoded.(*resourcev1.SigninProvider)
	if !ok {
		return nil, 0, fmt.Errorf("the sign-in provider %s isn't one", name)
	}
	return p, saved.Revision, nil
}

// signInsHold checks, as a change applies, that each route it signs in with a
// provider was filled in from the provider as it is now, or as the change leaves it,
// and that it takes in every route that signs in with a provider it edits. A plan
// made before another change edited the provider, or before a route started using it,
// would otherwise put back what that change took away: every account let in, a
// project, an issuer (#598's review, New 1). Plans don't run out, so it's stale.
func (e *Engine) signInsHold(ctx context.Context, tx *store.Tx, c store.Change, items []item) error {
	for i := range items {
		it := &items[i]
		r, ok := it.spec.(*resourcev1.Route)
		if !ok || r.GetAccess().GetSignIn() == nil {
			continue
		}
		s := r.GetAccess().GetSignIn()
		_, revision, err := e.providerAfter(ctx, tx, items, s.GetProvider(), false)
		if err != nil {
			return err
		}
		if int64(revision) != s.GetFilled().GetRevision() {
			return fault.New(ErrStale, "the sign-in provider %s changed after %s was planned, so %s would sign people in as it was. Propose the change again",
				s.GetProvider(), c.ID, it.ref.Name)
		}
	}
	changed := map[string]bool{}
	in := map[resource.Ref]bool{}
	for i := range items {
		it := &items[i]
		in[it.ref] = true
		if it.ref.Kind == signinprovider.Kind && it.changes() {
			changed[it.ref.Name] = true
		}
	}
	if len(changed) == 0 {
		return nil
	}
	stored, err := tx.ListKind(ctx, string(routeKind))
	if err != nil {
		return err
	}
	for j := range stored {
		ref := resource.Ref{Kind: routeKind, Scope: stored[j].Scope, Name: stored[j].Name}
		if in[ref] || stored[j].Spec == nil {
			continue
		}
		spec, err := e.decode(ctx, tx, store.InRevision(stored[j].ID, stored[j].Kind, stored[j].Revision), stored[j].Spec)
		if err != nil {
			return err
		}
		if r, ok := spec.(*resourcev1.Route); ok && r.GetAccess().GetSignIn() != nil && changed[r.GetAccess().GetSignIn().GetProvider()] {
			return fault.New(ErrStale, "%s started signing people in with the sign-in provider %s after %s was planned, so it would keep what the provider was. Propose the change again",
				ref.Name, r.GetAccess().GetSignIn().GetProvider(), c.ID)
		}
	}
	return nil
}

// withRoutesSigningIn takes in every route that signs people in with a provider the
// change edits or deletes, as the route has it now, and fills each in again from the
// provider as the change leaves it, as it does the change's own routes: who may sign
// in, the client and a rotated secret reach each route's server in the same plan, so
// no route goes on letting in someone the provider no longer does. A route the
// provider leaves it can't take, such as one that names no one where the provider no
// longer lets in every account, or one in a project it no longer lets use it, refuses
// the change, by name (ADR 0087).
func (e *Engine) withRoutesSigningIn(ctx context.Context, tx *store.Tx, items []item, named map[resource.Ref]bool) ([]item, error) {
	changed := map[string]bool{}
	for i := range items {
		if it := &items[i]; it.ref.Kind == signinprovider.Kind && it.changes() {
			changed[it.ref.Name] = true
		}
	}
	if len(changed) == 0 {
		return items, nil
	}
	signsInWith := func(spec proto.Message) bool {
		r, ok := spec.(*resourcev1.Route)
		return ok && r.GetAccess().GetSignIn() != nil && changed[r.GetAccess().GetSignIn().GetProvider()]
	}
	refill := func(it *item) error {
		if err := e.fillSignIn(ctx, tx, items, it.ref, it.spec.(*resourcev1.Route)); err != nil {
			return fault.New(ErrInvalid, "%s: %w", it.ref, err)
		}
		if err := e.kinds.Validate(it.ref.Kind, it.spec); err != nil {
			return fault.New(ErrInvalid, "%s signs people in with a provider this change edits, and can't as the change leaves it: %w", it.ref, err)
		}
		var err error
		it.encoded, err = resource.Encode(it.spec)
		return err
	}
	// The change's own routes were filled in as each edit was read, perhaps before the
	// provider's edit was.
	for i := range items {
		if it := &items[i]; it.ref.Kind == routeKind && signsInWith(it.spec) {
			if err := refill(it); err != nil {
				return items, err
			}
		}
	}
	stored, err := tx.ListKind(ctx, string(routeKind))
	if err != nil {
		return items, err
	}
	for j := range stored {
		ref := resource.Ref{Kind: routeKind, Scope: stored[j].Scope, Name: stored[j].Name}
		if named[ref] || stored[j].Spec == nil {
			continue
		}
		it, err := e.load(ctx, tx, ref)
		if err != nil {
			return items, err
		}
		if !signsInWith(it.current) {
			continue
		}
		it.spec = proto.Clone(it.current)
		if err := refill(&it); err != nil {
			return items, err
		}
		if it.servers, err = e.place(ctx, tx, it, nil); err != nil {
			return items, err
		}
		named[ref] = true
		items = append(items, it)
	}
	return items, nil
}
