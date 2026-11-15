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
	"github.com/dockwell/dockwell/internal/resource/connection"
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
		if s.GetConnection() != "" {
			return e.checkConnectionReached(ctx, tx, items, it, s.GetConnection())
		}
		if err := e.checkLinkUser(ctx, tx, items, it, s); err != nil {
			return err
		}
		return e.checkLinkServer(ctx, tx, items, it, s)
	case *resourcev1.Variable, *resourcev1.Secret:
		if c := variable.FromOf(s).GetConnection(); c != nil {
			return e.checkConnectionValue(ctx, tx, items, it, c)
		}
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
		if err := checkCopyFrom(items, it, s); err != nil {
			return err
		}
		return e.checkTakenOn(ctx, tx, items, it, s)
	case *resourcev1.Service:
		return e.checkServiceSecrets(ctx, tx, items, it, s)
	}
	return nil
}

// checkConnectionReached refuses a link to a connection that isn't there, or doesn't
// reach the link's project: reaching a connection is reading it, as whoever may
// change an app that links to it can read what it's given, so only the projects it
// names, or every one where it says so, may (ADR 0099). Both are refused in the
// same words, as a project a connection doesn't reach sees it as not there: a
// developer can't learn other projects' connections by trying names.
func (e *Engine) checkConnectionReached(ctx context.Context, tx *store.Tx, items []item, it *item, name string) error {
	org, err := tx.Organization(ctx)
	if err != nil {
		return err
	}
	spec, err := e.specAfter(ctx, tx, items, resource.Ref{Kind: variable.ConnectionKind, Scope: org.ID, Name: name})
	if err != nil {
		return err
	}
	if c, ok := spec.(*resourcev1.ConnectionSpec); !ok || !connection.Reaches(c, projectOfScope(ctx, tx, items, it.ref.Scope)) {
		return connectionUnusable(it, name)
	}
	return nil
}

// connectionUnusable is the one refusal of a connection that isn't there and of one
// that doesn't reach the item's project, wherever it's looked up: a link naming it,
// or a value read from it, pinned or checked.
func connectionUnusable(it *item, name string) error {
	return fault.New(ErrInvalid, "%s %s names the connection %s, which isn't one this project can use. Pick one on Connections, or ask an admin", it.ref.Kind, it.ref.Name, name)
}

// checkConnectionValue refuses a value from a connection that no link in its
// environment writes, as a link alone may say what reaches a connection, and so
// only as it says: the link names the connection, of the organization, and reaches
// it from this project (ADR 0099).
func (e *Engine) checkConnectionValue(ctx context.Context, tx *store.Tx, items []item, it *item, ref *resourcev1.ConnectionRef) error {
	org, err := tx.Organization(ctx)
	if err != nil {
		return err
	}
	spec, err := e.specAfter(ctx, tx, items, resource.Ref{Kind: linkKind, Scope: it.ref.Scope, Name: ref.GetLink()})
	if err != nil {
		return err
	}
	l, ok := spec.(*resourcev1.Link)
	switch {
	case ref.GetScope() != org.ID:
		return fault.New(ErrInvalid, "%s %s reads a connection of another organization, which isn't this install's", it.ref.Kind, it.ref.Name)
	case !ok:
		return fault.New(ErrInvalid, "%s %s takes its value from a connection, which only a link writes, and its environment has no link %s", it.ref.Kind, it.ref.Name, ref.GetLink())
	case l.GetConnection() != ref.GetName():
		return fault.New(ErrInvalid, "%s %s reads the connection %s through link %s, which names %s", it.ref.Kind, it.ref.Name, ref.GetName(), ref.GetLink(), cmp.Or(l.GetConnection(), "none"))
	}
	return e.checkConnectionReached(ctx, tx, items, it, ref.GetName())
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
	links, err := e.allLinks(ctx, tx, items)
	if err != nil {
		return err
	}
	for _, o := range links {
		if o.ref != it.ref && o.GetUser() == l.GetUser() && o.GetService() == l.GetService() && o.meets() == env {
			return fault.New(ErrInvalid, "link %s names %s's user %s, which another link reads already. A link reads only the user made for it", it.ref.Name, l.GetService(), l.GetUser())
		}
	}
	return nil
}

// aLink is a link as it is once the change applies, with its ref.
type aLink struct {
	*resourcev1.Link
	ref resource.Ref
}

// meets is the environment of the service the link meets.
func (l aLink) meets() string { return cmp.Or(l.GetMeetsEnvironment(), l.ref.Scope) }

// allLinks is every link of every environment as it is once the change applies.
func (e *Engine) allLinks(ctx context.Context, tx *store.Tx, items []item) ([]aLink, error) {
	stored, err := tx.ListKind(ctx, string(linkKind))
	if err != nil {
		return nil, err
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
	out := make([]aLink, 0, len(refs))
	for _, ref := range refs {
		spec, err := e.specAfter(ctx, tx, items, ref)
		if err != nil {
			return nil, err
		}
		if l, ok := spec.(*resourcev1.Link); ok {
			out = append(out, aLink{Link: l, ref: ref})
		}
	}
	return out, nil
}

// checkTakenOn refuses taking on a user for a database a link to its server names
// for another user, as a catalog install's link does for the user its image made,
// which no database.user records: the agent knows only its records, so the database
// is kept here for whatever plans it (ADR 0084).
func (e *Engine) checkTakenOn(ctx context.Context, tx *store.Tx, items []item, it *item, u *resourcev1.DatabaseUser) error {
	if !u.GetTakenOn() || it.current != nil {
		return nil
	}
	db := database.Owned(it.ref.Name, u)
	links, err := e.allLinks(ctx, tx, items)
	if err != nil {
		return err
	}
	for _, l := range links {
		if l.meets() == it.ref.Scope && l.GetService() == u.GetService() && l.GetDatabaseName() == db && l.GetUser() != it.ref.Name {
			return fault.New(ErrInvalid, "the database %s on %s is kept for another app, through link %s. Pick the app's own database, or connect a new one", db, u.GetService(), l.ref.Name)
		}
	}
	return nil
}

// checkServiceSecrets refuses a service that starts signing in with a database
// server's administrator password while a link of another environment meets that
// server, as checkLinkServer refuses the link made after it: the app's user there
// would be reachable through the service (ADR 0084).
func (e *Engine) checkServiceSecrets(ctx context.Context, tx *store.Tx, items []item, it *item, svc *resourcev1.Service) error {
	had, _ := it.current.(*resourcev1.Service)
	env := it.ref.Scope
	var links []aLink
	for as, name := range svc.GetSecrets() {
		name = cmp.Or(name, as)
		if had != nil && usesSecret(had, name) {
			continue
		}
		servers, err := e.serversWith(ctx, tx, items, env, name)
		if err != nil {
			return err
		}
		for _, server := range servers {
			if server == it.ref.Name {
				continue
			}
			if links == nil {
				if links, err = e.allLinks(ctx, tx, items); err != nil {
					return err
				}
			}
			for _, l := range links {
				if l.ref.Scope != env && l.meets() == env && l.GetService() == server {
					return fault.New(ErrInvalid, "%s would sign in with %s's administrator password, and link %s of another environment meets %s, so another app's user there could be reached through %s. Give %s a password of its own",
						it.ref.Name, server, l.ref.Name, server, it.ref.Name, it.ref.Name)
				}
			}
		}
	}
	return nil
}

// serversWith is the database servers of an environment whose administrator's
// password is the secret name, once the change applies.
func (e *Engine) serversWith(ctx context.Context, tx *store.Tx, items []item, env, name string) ([]string, error) {
	var names []string
	for i := range items {
		if svc, ok := items[i].spec.(*resourcev1.Service); ok && items[i].ref.Scope == env && usesSecret(svc, name) {
			names = append(names, items[i].ref.Name)
		}
	}
	users, err := tx.UsersOf(ctx, env, "secret", name)
	if err != nil {
		return nil, err
	}
	for i := range users {
		if users[i].Kind == string(serviceKind) && users[i].Scope == env && !slices.Contains(names, users[i].Name) {
			names = append(names, users[i].Name)
		}
	}
	var out []string
	for _, n := range names {
		spec, err := e.specAfter(ctx, tx, items, resource.Ref{Kind: serviceKind, Scope: env, Name: n})
		if err != nil {
			return nil, err
		}
		if svc, ok := spec.(*resourcev1.Service); ok {
			if found, ok := database.FromSpec(svc); ok && found.PasswordSecret == name {
				out = append(out, n)
			}
		}
	}
	return out, nil
}

// checkLinkServer refuses a link across environments to a server whose
// administrator's password another service signs in with, as an app's user there
// would then be reachable through that service, whoever plans it: ConnectDatabase and
// an install's choice refuse it before this, and a change made by hand meets it here
// (ADR 0084). A link that meets what it met before is left as it is.
func (e *Engine) checkLinkServer(ctx context.Context, tx *store.Tx, items []item, it *item, l *resourcev1.Link) error {
	env := l.GetMeetsEnvironment()
	if env == "" || env == it.ref.Scope {
		return nil
	}
	if had, ok := it.current.(*resourcev1.Link); ok && had.GetMeetsEnvironment() == env && had.GetService() == l.GetService() {
		return nil
	}
	spec, err := e.specAfter(ctx, tx, items, resource.Ref{Kind: serviceKind, Scope: env, Name: l.GetService()})
	if err != nil {
		return err
	}
	svc, ok := spec.(*resourcev1.Service)
	if !ok {
		return nil
	}
	found, ok := database.FromSpec(svc)
	if !ok || found.PasswordSecret == "" {
		return nil
	}
	refuse := func(by string) error {
		return fault.New(ErrInvalid, "link %s meets %s, whose administrator's password %s signs in with too, so another app's user there could be reached through %s. Give %s a user of its own first",
			it.ref.Name, l.GetService(), by, by, by)
	}
	for i := range items {
		other, ok := items[i].spec.(*resourcev1.Service)
		if ok && items[i].ref.Scope == env && items[i].ref.Name != l.GetService() && usesSecret(other, found.PasswordSecret) {
			return refuse(items[i].ref.Name)
		}
	}
	users, err := tx.UsersOf(ctx, env, "secret", found.PasswordSecret)
	if err != nil {
		return err
	}
	for i := range users {
		u := &users[i]
		if u.Kind != string(serviceKind) || u.Scope == env && u.Name == l.GetService() {
			continue
		}
		// One this change takes the secret away from no longer signs in with it.
		if spec, err := e.specAfter(ctx, tx, items, resource.Ref{Kind: serviceKind, Scope: u.Scope, Name: u.Name}); err != nil {
			return err
		} else if other, ok := spec.(*resourcev1.Service); ok && !usesSecret(other, found.PasswordSecret) {
			continue
		}
		return refuse(u.Name)
	}
	return nil
}

// usesSecret reports a service that uses the secret name of its environment.
func usesSecret(svc *resourcev1.Service, name string) bool {
	for as, n := range svc.GetSecrets() {
		if cmp.Or(n, as) == name {
			return true
		}
	}
	return false
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
// the change applies; on a server in a protected environment, an admin where that
// server is (ADR 0084, ADR 0071). To one who holds nothing where the link is, it
// reads as not there.
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
		// The role where the server is alone: a token narrowed to the link's project holds
		// admin there, which never counts on another project's protected server (ADR 0071).
		needs, why = store.RoleAdmin, "it restores a database on a server in a protected environment"
		held = place.Holds(grants, server.Place)
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
