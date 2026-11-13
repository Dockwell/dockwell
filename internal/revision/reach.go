// SPDX-License-Identifier: AGPL-3.0-only

package revision

import (
	"cmp"
	"context"
	"errors"
	"slices"

	"google.golang.org/protobuf/proto"

	resourcev1 "github.com/dockwell/dockwell/gen/dockwell/resource/v1alpha1"
	"github.com/dockwell/dockwell/internal/control/place"
	"github.com/dockwell/dockwell/internal/database"
	"github.com/dockwell/dockwell/internal/fault"
	"github.com/dockwell/dockwell/internal/resource"
	"github.com/dockwell/dockwell/internal/resource/variable"
	"github.com/dockwell/dockwell/internal/scope"
	"github.com/dockwell/dockwell/internal/store"
)

// What reaches another environment (ADR 0084). Four fields of an environment's
// items name another environment: a link's meets_environment, a value's from, a
// network's members and a database user's copy_from. Each is what a link alone
// may say, and only as the link it goes with says it, so an item can't be pointed
// at anything else there: checkReaches checks each against its link, whoever's
// change it is. And as each reaches into that environment, a person's change that
// points one there needs a role there too, as an edit there would (reachRoles).

// reach is a field of an item's spec that names another environment.
type reach struct {
	env string
	// what says what reaches it, as the end of "only ... can plan this, as".
	what string
}

// reachesOf is what an item's spec reaches in other environments than its own, by
// a key that changes with what's reached.
func reachesOf(ref resource.Ref, spec proto.Message) map[string]reach {
	out := map[string]reach{}
	add := func(env, key, what string) {
		if env != "" && env != ref.Scope {
			out[env+"\x00"+key] = reach{env: env, what: what}
		}
	}
	switch s := spec.(type) {
	case *resourcev1.Link:
		add(s.GetMeetsEnvironment(), "link\x00"+s.GetService()+"\x00"+s.GetDatabaseName()+"\x00"+s.GetUser(),
			"link "+ref.Name+" reaches "+s.GetService()+" in another environment")
	case *resourcev1.Variable, *resourcev1.Secret:
		if v := variable.FromOf(s).GetValue(); v != nil {
			add(v.GetEnvironment(), "value\x00"+v.GetName()+"\x00"+v.GetLink(),
				string(ref.Kind)+" "+ref.Name+" reads "+v.GetName()+" of another environment")
		}
	case *resourcev1.Network:
		for _, m := range s.GetMembers() {
			add(m.GetEnvironment(), "member\x00"+m.GetService(),
				"network "+ref.Name+" takes in "+m.GetService()+" of another environment")
		}
	case *resourcev1.DatabaseUser:
		if c := s.GetCopyFrom(); c != nil {
			add(c.GetEnvironment(), "copy\x00"+c.GetService()+"\x00"+c.GetDatabase(),
				"database user "+ref.Name+" copies "+c.GetDatabase()+" from another environment")
		}
	}
	return out
}

// newReaches is what an item's change points at in other environments that its
// spec as it is doesn't: what a person needs a role for there.
func newReaches(it *item) []reach {
	if it.spec == nil {
		return nil
	}
	had := map[string]reach{}
	if it.current != nil {
		had = reachesOf(it.ref, it.current)
	}
	var out []reach
	now := reachesOf(it.ref, it.spec)
	keys := make([]string, 0, len(now))
	for k := range now {
		if _, ok := had[k]; !ok {
			keys = append(keys, k)
		}
	}
	slices.Sort(keys)
	for _, k := range keys {
		out = append(out, now[k])
	}
	return out
}

// reachRoles refuses a person's change that points an item at another environment
// where they hold no role, or not the one an edit there would need. It answers as
// if the environment weren't there to one who holds nothing there.
func (e *Engine) reachRoles(ctx context.Context, tx *store.Tx, grants []store.Grant, it *item, byID map[string]*item, a act) error {
	for _, r := range newReaches(it) {
		at, err := e.envSpotByID(ctx, tx, r.env, byID)
		if errors.Is(err, store.ErrNotFound) {
			return fault.New(ErrOutOfReach, "%s", ErrOutOfReach.Error())
		}
		if err != nil {
			return err
		}
		held := place.Holds(grants, at.Place)
		if held == "" {
			return fault.New(ErrOutOfReach, "%s", ErrOutOfReach.Error())
		}
		needs := store.RoleDeveloper
		why := r.what + ", where you need a role too"
		if a != planning && at.Protected {
			needs, why = store.RoleAdmin, r.what+", which is protected"
		}
		if store.RoleRank(held) < store.RoleRank(needs) {
			return &Refusal{act: a, Held: held, Needs: needs, Why: why}
		}
	}
	return nil
}

// envSpotByID is an environment's place, as the change leaves it.
func (e *Engine) envSpotByID(ctx context.Context, tx *store.Tx, id string, byID map[string]*item) (spot, error) {
	if env, ok := byID[id]; ok && env.ref.Kind == "environment" {
		s := envSpot(env)
		s.admins = ""
		return s, nil
	}
	env, err := tx.GetResourceByID(ctx, id)
	if err != nil {
		return spot{}, err
	}
	if env.Kind != "environment" || env.Spec == nil {
		return spot{}, store.ErrNotFound
	}
	return spot{Place: place.Place{Project: env.Scope, Environment: env.ID, Protected: scope.ProtectedSpec(env.Name, env.Spec)}}, nil
}

// specAfter is what ref's spec is once the change applies: the change's own, or
// what's stored. Nil for nothing, or a kind this engine doesn't have.
func (e *Engine) specAfter(ctx context.Context, tx *store.Tx, items []item, ref resource.Ref) (proto.Message, error) {
	for i := range items {
		if items[i].ref == ref {
			return items[i].spec, nil
		}
	}
	if t, _ := e.kinds.Type(ref.Kind); t == nil {
		return nil, nil
	}
	it, err := e.load(ctx, tx, ref)
	return it.current, err
}

// linksAfter are the links of an environment once the change applies, by name.
func (e *Engine) linksAfter(ctx context.Context, tx *store.Tx, items []item, env string) (map[string]*resourcev1.Link, error) {
	out := map[string]*resourcev1.Link{}
	if t, _ := e.kinds.Type(linkKind); t == nil {
		return out, nil
	}
	names, err := liveIn(ctx, tx, items, env, linkKind)
	if err != nil {
		return nil, err
	}
	for _, name := range names {
		spec, err := e.specAfter(ctx, tx, items, resource.Ref{Kind: linkKind, Scope: env, Name: name})
		if err != nil {
			return nil, err
		}
		if l, ok := spec.(*resourcev1.Link); ok {
			out[name] = l
		}
	}
	return out, nil
}

// checkReaches refuses an item that names another environment other than as a link
// of its own environment says, whoever's change it is (ADR 0084):
//   - a value from another environment is the password of the database user its
//     link made there, read through that link;
//   - a network's member from another environment is the service a link of the
//     network's environment meets;
//   - a database user made as a copy of a database copies the one an app's link
//     reads now, which the same change points at this user.
func (e *Engine) checkReaches(ctx context.Context, tx *store.Tx, items []item, it *item) error {
	if it.spec == nil {
		return nil
	}
	switch s := it.spec.(type) {
	case *resourcev1.Link:
		return e.checkLinkUser(ctx, tx, items, it, s)
	case *resourcev1.Variable, *resourcev1.Secret:
		return e.checkValueFrom(ctx, tx, items, it)
	case *resourcev1.Network:
		var links map[string]*resourcev1.Link
		for _, m := range s.GetMembers() {
			if m.GetEnvironment() == "" || m.GetEnvironment() == it.ref.Scope {
				continue
			}
			if links == nil {
				var err error
				if links, err = e.linksAfter(ctx, tx, items, it.ref.Scope); err != nil {
					return err
				}
			}
			met := false
			for _, l := range links {
				met = met || l.GetMeetsEnvironment() == m.GetEnvironment() && l.GetService() == m.GetService()
			}
			if !met {
				return fault.New(ErrInvalid, "network %s takes in %s of another environment, which no link of its environment meets. A network takes in another environment's service only for a link to it, which Dockwell makes as an app is linked to it",
					it.ref.Name, m.GetService())
			}
		}
	case *resourcev1.DatabaseUser:
		return checkCopyFrom(items, it, s)
	}
	return nil
}

// checkLinkUser refuses a link that names a database user another link names, so
// the user a link reads the password of is the one made for it, never another app's.
func (e *Engine) checkLinkUser(ctx context.Context, tx *store.Tx, items []item, it *item, l *resourcev1.Link) error {
	if l.GetUser() == "" {
		return nil
	}
	if had, ok := it.current.(*resourcev1.Link); ok && had.GetUser() == l.GetUser() && had.GetService() == l.GetService() && had.GetMeetsEnvironment() == l.GetMeetsEnvironment() {
		return nil
	}
	env := cmp.Or(l.GetMeetsEnvironment(), it.ref.Scope)
	stored, err := tx.ListKind(ctx, string(linkKind))
	if err != nil {
		return err
	}
	refs := make([]resource.Ref, 0, len(stored)+len(items))
	for _, r := range stored {
		refs = append(refs, resource.Ref{Kind: linkKind, Scope: r.Scope, Name: r.Name})
	}
	for i := range items {
		if items[i].ref.Kind == linkKind && !slices.Contains(refs, items[i].ref) {
			refs = append(refs, items[i].ref)
		}
	}
	for _, ref := range refs {
		if ref == it.ref {
			continue
		}
		spec, err := e.specAfter(ctx, tx, items, ref)
		if err != nil {
			return err
		}
		o, ok := spec.(*resourcev1.Link)
		if ok && o.GetUser() == l.GetUser() && o.GetService() == l.GetService() && cmp.Or(o.GetMeetsEnvironment(), ref.Scope) == env {
			return fault.New(ErrInvalid, "link %s names %s's user %s, which another link reads already. A link reads only the user made for it", it.ref.Name, l.GetService(), l.GetUser())
		}
	}
	return nil
}

// checkValueFrom refuses a value from another environment that isn't the password of
// the user its link made there.
func (e *Engine) checkValueFrom(ctx context.Context, tx *store.Tx, items []item, it *item) error {
	v := variable.FromOf(it.spec).GetValue()
	if v == nil || v.GetEnvironment() == "" || v.GetEnvironment() == it.ref.Scope {
		return nil
	}
	refuse := func(why string) error {
		return fault.New(ErrInvalid, "%s %s takes its value from another environment, which only a link writes, as the password of the user it made there, and %s. Link the app to what it needs, and the link writes it",
			it.ref.Kind, it.ref.Name, why)
	}
	spec, err := e.specAfter(ctx, tx, items, resource.Ref{Kind: linkKind, Scope: it.ref.Scope, Name: v.GetLink()})
	if err != nil {
		return err
	}
	l, ok := spec.(*resourcev1.Link)
	switch {
	case v.GetLink() == "" || !ok:
		return refuse("its environment has no link " + cmp.Or(v.GetLink(), "named"))
	case l.GetMeetsEnvironment() != v.GetEnvironment():
		return refuse("link " + v.GetLink() + " meets nothing in that environment")
	case !v.GetSecret() || l.GetUser() == "":
		return refuse("link " + v.GetLink() + " made no user there whose password it is")
	}
	spec, err = e.specAfter(ctx, tx, items, resource.Ref{Kind: database.UserKind, Scope: v.GetEnvironment(), Name: l.GetUser()})
	if err != nil {
		return err
	}
	u, ok := spec.(*resourcev1.DatabaseUser)
	if !ok || u.GetService() != l.GetService() || u.GetPassword() != v.GetName() {
		return refuse(v.GetName() + " isn't the password of " + l.GetUser() + ", the user link " + v.GetLink() + " made there")
	}
	return nil
}

// checkCopyFrom refuses a new database user made as a copy of a database that isn't
// the one a link reads now, which the same change points at this user: what
// moving an app's database makes. A system database is never copied.
func checkCopyFrom(items []item, it *item, u *resourcev1.DatabaseUser) error {
	c := u.GetCopyFrom()
	if c == nil {
		return nil
	}
	if had, ok := it.current.(*resourcev1.DatabaseUser); ok && proto.Equal(had.GetCopyFrom(), c) {
		return nil
	}
	if database.SystemDatabase(c.GetDatabase()) {
		return fault.New(ErrInvalid, "database user %s copies %s, which its server keeps for itself. Dockwell copies only an app's own database", it.ref.Name, c.GetDatabase())
	}
	for i := range items {
		l := &items[i]
		was, wok := l.current.(*resourcev1.Link)
		now, nok := l.spec.(*resourcev1.Link)
		if l.ref.Kind != linkKind || !wok || !nok {
			continue
		}
		reads := cmp.Or(was.GetMeetsEnvironment(), l.ref.Scope) == c.GetEnvironment() && was.GetService() == c.GetService() && was.GetDatabaseName() == c.GetDatabase()
		moves := cmp.Or(now.GetMeetsEnvironment(), l.ref.Scope) == it.ref.Scope && now.GetService() == u.GetService() && now.GetUser() == it.ref.Name
		if reads && moves {
			return nil
		}
	}
	return fault.New(ErrInvalid, "database user %s copies %s of %s, and no link in this change moves from it to %s. A database is copied only as an app's link moves to another, as Use another database on the app's page does",
		it.ref.Name, c.GetDatabase(), c.GetService(), it.ref.Name)
}

// forLinkRoles checks a person's change to an item made for an app's link (Edit.For),
// a restore of the app's database alone on a server it may share: the link has to
// meet that server, whose service keeps its data in the item's volume, and the role
// it needs is where the link is, a developer, or an admin where that's protected and
// the change applies; a protected server's environment holds too, as an admin's
// (ADR 0084). To one who holds nothing where the link is, it reads as not there.
func (e *Engine) forLinkRoles(ctx context.Context, tx *store.Tx, grants []store.Grant, it *item, server spot, byID map[string]*item, a act) error {
	link, err := e.load(ctx, tx, it.forLink)
	if err != nil {
		return err
	}
	l, ok := link.current.(*resourcev1.Link)
	if !ok || cmp.Or(l.GetMeetsEnvironment(), it.forLink.Scope) != it.ref.Scope {
		return fault.New(ErrInvalid, "link %s doesn't meet the server %s is on, so nothing of it is done for that link", it.forLink.Name, it.ref.Name)
	}
	svc, err := e.load(ctx, tx, resource.Ref{Kind: "service", Scope: it.ref.Scope, Name: l.GetService()})
	if err != nil {
		return err
	}
	mounts := svc.current != nil && slices.ContainsFunc(e.kinds.References("service", svc.current), func(r resource.Reference) bool {
		return r.Kind == it.ref.Kind && r.Name == it.ref.Name
	})
	if !mounts {
		return fault.New(ErrInvalid, "%s isn't where %s, which link %s meets, keeps its data, so nothing of it is done for that link", it.ref.Name, l.GetService(), it.forLink.Name)
	}
	at, err := e.envSpotByID(ctx, tx, it.forLink.Scope, byID)
	if errors.Is(err, store.ErrNotFound) {
		return fault.New(ErrOutOfReach, "%s", ErrOutOfReach.Error())
	}
	if err != nil {
		return err
	}
	held := place.Holds(grants, at.Place)
	if held == "" {
		return fault.New(ErrOutOfReach, "%s", ErrOutOfReach.Error())
	}
	needs, why := store.RoleDeveloper, ""
	switch {
	case a != planning && at.Protected:
		needs, why = store.RoleAdmin, "it restores "+it.forLink.Name+"'s database for an app in a protected environment"
	case a != planning && server.Protected:
		needs, why = store.RoleAdmin, "it restores a database on a server in a protected environment"
		held = maxRole(held, place.Holds(grants, server.Place))
	}
	if store.RoleRank(held) < store.RoleRank(needs) {
		return &Refusal{act: a, Held: held, Needs: needs, Why: why}
	}
	return nil
}

// linkedRestoreRoles checks a restore of a whole volume that a server keeps its data
// in, where links of other environments meet that server: it takes back what each of
// their apps wrote since, so it needs a role in each of those environments, a
// developer, and an admin where one is protected and the change applies, is undone
// or is discarded by someone else, wherever it was planned (ADR 0084). A restore of
// one database alone, made for its link, is forLinkRoles'.
func (e *Engine) linkedRestoreRoles(ctx context.Context, tx *store.Tx, grants []store.Grant, it *item, byID map[string]*item, a act) error {
	if it.restore == nil || it.forLink != (resource.Ref{}) || it.ref.Kind != volumeKind {
		return nil
	}
	links, err := tx.ListKind(ctx, "link")
	if err != nil {
		return err
	}
	mounts := map[string]bool{} // by service: it keeps its data in the volume
	for _, r := range links {
		l := &resourcev1.Link{}
		// A link is never sealed: it names values, and holds none.
		if r.Spec == nil || r.Scope == it.ref.Scope || proto.Unmarshal(r.Spec, l) != nil || cmp.Or(l.GetMeetsEnvironment(), r.Scope) != it.ref.Scope {
			continue
		}
		in, seen := mounts[l.GetService()]
		if !seen {
			svc, err := e.load(ctx, tx, resource.Ref{Kind: "service", Scope: it.ref.Scope, Name: l.GetService()})
			if err != nil {
				return err
			}
			in = svc.current != nil && slices.ContainsFunc(e.kinds.References("service", svc.current), func(u resource.Reference) bool {
				return u.Kind == it.ref.Kind && u.Name == it.ref.Name
			})
			mounts[l.GetService()] = in
		}
		if !in {
			continue
		}
		at, err := e.envSpotByID(ctx, tx, r.Scope, byID)
		if errors.Is(err, store.ErrNotFound) {
			continue // its environment is gone, and its app with it
		}
		if err != nil {
			return err
		}
		held := place.Holds(grants, at.Place)
		needs, why := store.RoleDeveloper, "it restores "+it.ref.Name+" whole, which takes back what an app in another project wrote to it since"
		if a != planning && at.Protected {
			needs, why = store.RoleAdmin, "it restores "+it.ref.Name+" whole, which takes back what an app in a protected environment wrote to it since"
		}
		if store.RoleRank(held) < store.RoleRank(needs) {
			return &Refusal{act: a, Held: held, Needs: needs, Why: why}
		}
	}
	return nil
}

// maxRole is the higher of two roles.
func maxRole(a, b string) string {
	if store.RoleRank(b) > store.RoleRank(a) {
		return b
	}
	return a
}
