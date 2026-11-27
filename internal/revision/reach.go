// SPDX-License-Identifier: AGPL-3.0-only

package revision

import (
	"cmp"
	"context"
	"errors"
	"fmt"
	"net/url"
	"slices"
	"strconv"
	"strings"

	"google.golang.org/protobuf/encoding/protojson"
	"google.golang.org/protobuf/proto"

	resourcev1 "github.com/dockwell/dockwell/gen/dockwell/resource/v1alpha1"
	dockwellv1 "github.com/dockwell/dockwell/gen/dockwell/v1"
	"github.com/dockwell/dockwell/internal/control/place"
	"github.com/dockwell/dockwell/internal/database"
	"github.com/dockwell/dockwell/internal/fault"
	"github.com/dockwell/dockwell/internal/human"
	"github.com/dockwell/dockwell/internal/resource"
	"github.com/dockwell/dockwell/internal/resource/alert"
	"github.com/dockwell/dockwell/internal/resource/backuppolicy"
	"github.com/dockwell/dockwell/internal/resource/connection"
	"github.com/dockwell/dockwell/internal/resource/link"
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
		add(s.GetMeetsEnvironment(), "link\x00"+s.GetEngine().String()+"\x00"+s.GetService()+"\x00"+s.GetDatabaseName()+"\x00"+s.GetUser(),
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
func (e *Engine) reachRoles(ctx context.Context, tx *store.Tx, grants []store.Grant, items []item, it *item, byID map[string]*item, a act) error {
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
		admin, err := e.reachesClickHouseCredentials(ctx, tx, items, it)
		if err != nil {
			return err
		}
		if admin {
			needs, why = store.RoleAdmin, r.what+" using its existing ClickHouse credentials"
		}
		if store.RoleRank(held) < store.RoleRank(needs) {
			return &Refusal{act: a, Held: held, Needs: needs, Why: why}
		}
	}
	return nil
}

// ClickHouse links reuse the image user's credentials, so sharing them across
// environments needs an administrator on the source.
func (e *Engine) reachesClickHouseCredentials(ctx context.Context, tx *store.Tx, items []item, it *item) (bool, error) {
	if l, ok := it.spec.(*resourcev1.Link); ok {
		return l.GetEngine() == resourcev1.LinkEngine_LINK_ENGINE_CLICKHOUSE, nil
	}
	if v := variable.FromOf(it.spec).GetValue(); v != nil {
		spec, err := e.specAfter(ctx, tx, items, resource.Ref{Kind: linkKind, Scope: it.ref.Scope, Name: v.GetLink()})
		l, _ := spec.(*resourcev1.Link)
		return l.GetEngine() == resourcev1.LinkEngine_LINK_ENGINE_CLICKHOUSE, err
	}
	return false, nil
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
			if err := e.checkConnectionReached(ctx, tx, items, it, s.GetConnection()); err != nil {
				return err
			}
			if err := e.checkLinkNotBackup(ctx, tx, items, s.GetConnection()); err != nil {
				return err
			}
			if err := e.checkLinkNotAlertChannel(ctx, tx, items, s); err != nil {
				return err
			}
			return e.checkLinkPrefix(ctx, tx, items, it, s)
		}
		if err := e.checkLinkUser(ctx, tx, items, it, s); err != nil {
			return err
		}
		return e.checkLinkServer(ctx, tx, items, it, s)
	case *resourcev1.AlertChannel:
		if em := s.GetEmail(); em != nil {
			if em.GetConnection() != "" {
				return e.checkAlertChannelConnection(ctx, tx, items, it, em)
			}
			if em.GetUsername() != "" && em.GetPassword() == "" {
				return fault.New(ErrInvalid, "alert channel %s signs in as %s and needs its password", it.ref.Name, em.GetUsername())
			}
		}
	case *resourcev1.BackupDestination:
		if err := e.checkDestinationTarget(ctx, tx, items, it, s); err != nil {
			return err
		}
		if s.GetConnection() != "" {
			return e.checkDestinationConnection(ctx, tx, items, it, s)
		}
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
				met = e.metByWebsiteSubpath(ctx, tx, items, it.ref, m)
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

// metByWebsiteSubpath reports whether a cross-environment member on a website's "site"
// network corresponds to one of that website's rules.subpath_proxies service_id entries
// in the state after this change (L1).
func (e *Engine) metByWebsiteSubpath(ctx context.Context, tx *store.Tx, items []item, netRef resource.Ref, m *resourcev1.NetworkMember) bool {
	if netRef.Name != "site" {
		return false
	}
	idxStr, ok := strings.CutPrefix(m.GetAlias(), "subpath-")
	if !ok {
		return false
	}
	idx, err := strconv.Atoi(idxStr)
	if err != nil || idx < 0 {
		return false
	}
	projectID := projectOfScope(ctx, tx, items, netRef.Scope)
	if projectID == "" {
		return false
	}
	var projSpec *resourcev1.Project
	for i := range items {
		if items[i].id == projectID && items[i].ref.Kind == scope.ProjectKind {
			projSpec, _ = items[i].spec.(*resourcev1.Project)
			break
		}
	}
	if projSpec == nil {
		if pr, err := tx.GetResourceByID(ctx, projectID); err == nil && pr.Kind == string(scope.ProjectKind) && pr.Spec != nil {
			if spec, err := e.specAfter(ctx, tx, items, resource.Ref{Kind: scope.ProjectKind, Scope: pr.Scope, Name: pr.Name}); err == nil {
				projSpec, _ = spec.(*resourcev1.Project)
			}
		}
	}
	proxies := projSpec.GetWebsite().GetRules().GetSubpathProxies()
	if idx >= len(proxies) {
		return false
	}
	targetID := proxies[idx].GetServiceId()
	if targetID == "" {
		return false
	}
	for i := range items {
		if items[i].id == targetID && items[i].ref.Kind == serviceKind {
			return items[i].spec != nil && items[i].ref.Scope == m.GetEnvironment() && items[i].ref.Name == m.GetService()
		}
	}
	svc, err := tx.GetResourceByID(ctx, targetID)
	if err != nil || svc.Kind != string(serviceKind) || svc.Spec == nil {
		return false
	}
	return svc.Scope == m.GetEnvironment() && svc.Name == m.GetService()
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

// checkConnectionLinks refuses a change that deletes a connection, or narrows it away
// from a project, while a link there still names it, though no value reads through
// the link: the link would name a connection that's gone or out of its reach, and no
// later edit of it would plan (#710). Disconnecting the link first, or in the same
// change, lets it go. Each refusal names where the link is, as the person narrowing
// or deleting the connection is on it, not in a project.
func (e *Engine) checkConnectionLinks(ctx context.Context, tx *store.Tx, items []item, it *item) error {
	if it.ref.Kind != variable.ConnectionKind || it.current == nil {
		return nil
	}
	links, err := e.allLinks(ctx, tx, items)
	if err != nil {
		return err
	}
	after, _ := it.spec.(*resourcev1.ConnectionSpec)
	for i := range links {
		l := &links[i]
		if l.GetConnection() != it.ref.Name {
			continue
		}
		where := whichScope(ctx, tx, l.ref.Scope)
		if it.spec == nil {
			// An undo can't take an edit of its own, so it's told only to disconnect first.
			also := ", or in the same change"
			if it.reverses {
				also = ""
			}
			return fault.New(ErrInvalid, "link %s in %s still uses the connection %s, which this change deletes. Disconnect it on the connection's page first%s",
				l.ref.Name, where, it.ref.Name, also)
		}
		if before, _ := it.current.(*resourcev1.ConnectionSpec); before.GetKind() != after.GetKind() {
			return fault.New(ErrInvalid, "link %s in %s still uses the connection %s, which this change makes another kind of connection. Disconnect it on the connection's page first, or add a connection of the new kind",
				l.ref.Name, where, it.ref.Name)
		}
		// Each app was told the connection's security in its own words, which a
		// link writes once, so it can't change under them.
		if before, _ := it.current.(*resourcev1.ConnectionSpec); writesSecurity(l.Link) &&
			connection.Security(before.GetSmtp()) != connection.Security(after.GetSmtp()) {
			return fault.New(ErrInvalid, "link %s in %s tells its app how the connection %s is secured, in the app's own words, so its security stays while the link uses it. Disconnect it on the connection's page first, or add a connection with the new security",
				l.ref.Name, where, it.ref.Name)
		}
		if project := projectOfScope(ctx, tx, items, l.ref.Scope); !connection.Reaches(after, project) {
			name := whichScope(ctx, tx, project)
			return fault.New(ErrInvalid, "link %s in %s still uses the connection %s, which this change takes away from %s. Disconnect it on the connection's page first, or keep %s in its reach",
				l.ref.Name, where, it.ref.Name, name, name)
		}
	}
	return nil
}

// checkDatabaseLinks refuses a change that deletes a logical database (or a
// database user whose owned database is in use) while any app still uses that
// database via a link, via the user's password secret (directly or through a
// derived secret/variable), via the engine's admin credentials, via a secret,
// variable, or environment value whose connection string names the database on
// the engine (by service name, alias, container name, or published address), or
// via a plain variable/env value naming the database on an app that links to or
// reaches the engine, unless its database server is deleted in the same change
// or those references are removed in the same change (H3).
func (e *Engine) checkDatabaseLinks(ctx context.Context, tx *store.Tx, items []item, it *item) error {
	if it.spec != nil || it.current == nil {
		return nil
	}
	var svc, dbName, userSecret string
	switch cur := it.current.(type) {
	case *resourcev1.LogicalDatabase:
		svc = cur.GetService()
		dbName = database.LogicalName(it.ref.Name, cur)
	case *resourcev1.DatabaseUser:
		if cur.GetTakenOn() {
			return nil
		}
		svc = cur.GetService()
		dbName = database.Owned(it.ref.Name, cur)
		userSecret = cur.GetPassword()
	default:
		return nil
	}
	if svc == "" || dbName == "" {
		return nil
	}
	// If the database server service itself is deleted in the same change,
	// sharedgoing/refuseLinkedGoing handles server-level link rules.
	svcAfter, err := e.specAfter(ctx, tx, items, resource.Ref{Kind: serviceKind, Scope: it.ref.Scope, Name: svc})
	if err != nil {
		return err
	}
	if svcAfter == nil {
		return nil
	}
	dbSvc, _ := svcAfter.(*resourcev1.Service)
	env := it.ref.Scope

	hosts, adminSecret := e.engineHostsAndAdmin(ctx, tx, env, svc, dbSvc)

	// 1. Refuse while any link names this database; also collect services linked to svc.
	links, err := e.allLinks(ctx, tx, items)
	if err != nil {
		return err
	}
	linkedToEngine := map[string]bool{}
	for i := range links {
		l := &links[i]
		if l.meets() != env || l.GetService() != svc {
			continue
		}
		if l.GetDatabaseName() == dbName {
			where := whichScope(ctx, tx, l.ref.Scope)
			return fault.New(ErrInvalid, "link %s in %s still uses the database %s on %s, which this change deletes. Disconnect that app first",
				l.ref.Name, where, dbName, svc)
		}
		if l.ref.Scope == env {
			linkedToEngine[l.ref.Name] = true
		}
	}

	// 2. When deleting a database.user, refuse while any app uses its password
	// secret (directly or through a derived secret/variable template).
	if userSecret != "" {
		apps, err := e.appsUsingValue(ctx, tx, items, resource.Ref{Kind: variable.SecretKind, Scope: env, Name: userSecret}, svc)
		if err != nil {
			return err
		}
		if len(apps) > 0 {
			return fault.New(ErrInvalid, "%s still uses the database %s on %s (via secret %s), which this change deletes. Stop using %s in %s first",
				strings.Join(apps, ", "), dbName, svc, userSecret, userSecret, strings.Join(apps, ", "))
		}
	}

	// 3. Refuse while any app uses a secret or variable whose value/template names
	// this database on svc (by service name, alias, container name, or address).
	for _, kind := range []resource.Kind{variable.SecretKind, variable.VariableKind} {
		stored, err := tx.ListResources(ctx, env, string(kind))
		if err != nil {
			return err
		}
		refs := make([]resource.Ref, 0, len(stored))
		for i := range stored {
			refs = append(refs, resource.Ref{Kind: kind, Scope: env, Name: stored[i].Name})
		}
		for i := range items {
			if items[i].ref.Kind == kind && items[i].ref.Scope == env && !slices.Contains(refs, items[i].ref) {
				refs = append(refs, items[i].ref)
			}
		}
		for _, ref := range refs {
			spec, err := e.specAfter(ctx, tx, items, ref)
			if err != nil {
				return err
			}
			if spec == nil || !specNamesDatabaseHosts(spec, hosts, dbName) {
				continue
			}
			apps, err := e.appsUsingValue(ctx, tx, items, ref, svc)
			if err != nil {
				return err
			}
			if len(apps) > 0 {
				return fault.New(ErrInvalid, "%s still uses %s %s, which names the database %s on %s that this change deletes. Update or remove %s first",
					strings.Join(apps, ", "), ref.Kind, ref.Name, dbName, svc, ref.Name)
			}
		}
	}

	// 4. Find apps signing in with the engine's admin password secret (if any).
	var adminApps []string
	if adminSecret != "" {
		if adminApps, err = e.appsUsingValue(ctx, tx, items, resource.Ref{Kind: variable.SecretKind, Scope: env, Name: adminSecret}, svc); err != nil {
			return err
		}
	}

	// 5. Inspect every other service in the environment for:
	//    (a) literal/resolved URLs or DSNs naming dbName on any host/alias/address of svc,
	//    (b) plain env/variable/secret values equal to dbName on a service that links to or reaches svc,
	//    (c) apps signing in as the engine's admin without explicitly targeting another database.
	svcs, err := tx.ListResources(ctx, env, string(serviceKind))
	if err != nil {
		return err
	}
	svcRefs := make([]resource.Ref, 0, len(svcs))
	for i := range svcs {
		svcRefs = append(svcRefs, resource.Ref{Kind: serviceKind, Scope: env, Name: svcs[i].Name})
	}
	for i := range items {
		if items[i].ref.Kind == serviceKind && items[i].ref.Scope == env && !slices.Contains(svcRefs, items[i].ref) {
			svcRefs = append(svcRefs, items[i].ref)
		}
	}
	slices.SortFunc(svcRefs, func(a, b resource.Ref) int { return cmp.Compare(a.Name, b.Name) })
	var adminRefusal error
	for _, ref := range svcRefs {
		if ref.Name == svc {
			continue
		}
		spec, err := e.specAfter(ctx, tx, items, ref)
		if err != nil {
			return err
		}
		s, ok := spec.(*resourcev1.Service)
		if !ok {
			continue
		}
		entries, err := e.serviceResolvedEnv(ctx, tx, items, env, s)
		if err != nil {
			return err
		}
		usesAdmin := slices.Contains(adminApps, ref.Name)
		reachesEngine := usesAdmin || linkedToEngine[ref.Name]
		namesOtherDB := false
		var plainDBKey string
		for _, kv := range entries {
			if database.ValueNamesDatabaseHosts(kv.val, hosts, dbName) {
				return fault.New(ErrInvalid, "service %s sets %s to the database %s on %s, which this change deletes. Update %s first",
					ref.Name, kv.key, dbName, svc, ref.Name)
			}
			if database.ValueReachesHost(kv.val, hosts) {
				reachesEngine = true
			}
			trimmed := strings.TrimSpace(kv.val)
			if trimmed == dbName && plainDBKey == "" {
				plainDBKey = kv.key
			}
			if database.ValueNamesOtherDatabase(kv.val, hosts, dbName) ||
				(database.IsDBNameEnvKey(kv.key) && trimmed != "" && trimmed != dbName) {
				namesOtherDB = true
			}
		}
		if reachesEngine && plainDBKey != "" {
			return fault.New(ErrInvalid, "service %s sets %s to %s while reaching %s, which this change deletes. Update %s first",
				ref.Name, plainDBKey, dbName, svc, ref.Name)
		}
		if usesAdmin && !namesOtherDB && adminRefusal == nil {
			adminRefusal = fault.New(ErrInvalid, "service %s signs in to %s with its admin password (%s), so deleting %s could break it. Switch %s to its own database user or remove %s first",
				ref.Name, svc, adminSecret, dbName, ref.Name, adminSecret)
		}
	}
	if adminRefusal != nil {
		return adminRefusal
	}
	return nil
}

// databaseDeleteNotes warns in the plan when a database deletion passes static
// checks but Dockwell can't verify every app that reaches the engine (for
// example, an app that connects to the engine without naming a database in its
// variables, or a logical database whose apps may read their database name from
// a config file) (H3).
func (e *Engine) databaseDeleteNotes(ctx context.Context, tx *store.Tx, items []item) ([]Note, error) {
	var notes []Note
	for i := range items {
		it := &items[i]
		if it.spec != nil || it.current == nil {
			continue
		}
		var svc, dbName, owner string
		isLogical := false
		switch cur := it.current.(type) {
		case *resourcev1.LogicalDatabase:
			svc = cur.GetService()
			dbName = database.LogicalName(it.ref.Name, cur)
			owner = cur.GetOwner()
			isLogical = true
		case *resourcev1.DatabaseUser:
			if cur.GetTakenOn() {
				continue
			}
			svc = cur.GetService()
			dbName = database.Owned(it.ref.Name, cur)
		default:
			continue
		}
		if svc == "" || dbName == "" {
			continue
		}
		svcAfter, err := e.specAfter(ctx, tx, items, resource.Ref{Kind: serviceKind, Scope: it.ref.Scope, Name: svc})
		if err != nil {
			return nil, err
		}
		if svcAfter == nil {
			continue
		}
		dbSvc, _ := svcAfter.(*resourcev1.Service)
		env := it.ref.Scope
		hosts, _ := e.engineHostsAndAdmin(ctx, tx, env, svc, dbSvc)
		// If deleting a logical database whose owner is a managed database.user,
		// apps using that owner's password secret can reach this database too.
		var ownerApps []string
		if isLogical && owner != "" {
			if ownerSpec, oerr := e.specAfter(ctx, tx, items, resource.Ref{Kind: database.UserKind, Scope: env, Name: owner}); oerr == nil {
				if u, ok := ownerSpec.(*resourcev1.DatabaseUser); ok && u.GetService() == svc && u.GetPassword() != "" {
					if apps, aerr := e.appsUsingValue(ctx, tx, items, resource.Ref{Kind: variable.SecretKind, Scope: env, Name: u.GetPassword()}, svc); aerr == nil {
						ownerApps = apps
					}
				}
			}
		}
		links, err := e.allLinks(ctx, tx, items)
		if err != nil {
			return nil, err
		}
		linkedWithoutDB := map[string]bool{}
		for j := range links {
			l := &links[j]
			if l.meets() == env && l.GetService() == svc && l.GetDatabaseName() == "" && l.ref.Scope == env {
				linkedWithoutDB[l.ref.Name] = true
			}
		}
		svcs, err := tx.ListResources(ctx, env, string(serviceKind))
		if err != nil {
			return nil, err
		}
		svcRefs := make([]resource.Ref, 0, len(svcs))
		for j := range svcs {
			svcRefs = append(svcRefs, resource.Ref{Kind: serviceKind, Scope: env, Name: svcs[j].Name})
		}
		for j := range items {
			if items[j].ref.Kind == serviceKind && items[j].ref.Scope == env && !slices.Contains(svcRefs, items[j].ref) {
				svcRefs = append(svcRefs, items[j].ref)
			}
		}
		slices.SortFunc(svcRefs, func(a, b resource.Ref) int { return cmp.Compare(a.Name, b.Name) })
		var uncertain []string
		otherApps := 0
		for _, ref := range svcRefs {
			if ref.Name == svc {
				continue
			}
			spec, err := e.specAfter(ctx, tx, items, ref)
			if err != nil {
				return nil, err
			}
			s, ok := spec.(*resourcev1.Service)
			if !ok {
				continue
			}
			otherApps++
			entries, err := e.serviceResolvedEnv(ctx, tx, items, env, s)
			if err != nil {
				return nil, err
			}
			reaches := linkedWithoutDB[ref.Name] || slices.Contains(ownerApps, ref.Name)
			namesOtherDB := false
			for _, kv := range entries {
				if database.ValueReachesHost(kv.val, hosts) {
					reaches = true
				}
				trimmed := strings.TrimSpace(kv.val)
				if database.ValueNamesOtherDatabase(kv.val, hosts, dbName) ||
					(database.IsDBNameEnvKey(kv.key) && trimmed != "" && trimmed != dbName) {
					namesOtherDB = true
				}
			}
			if reaches && !namesOtherDB {
				uncertain = append(uncertain, ref.Name)
			}
		}
		if len(uncertain) > 0 {
			notes = append(notes, Note{
				Reason: "database_delete_unverified",
				Ref:    it.ref,
				Names:  uncertain,
				Message: fmt.Sprintf("%s can reach %s without naming a database in its variables, so Dockwell can't tell whether it uses %s. Check before applying; Undo brings %s back for 7 days",
					human.List(uncertain), svc, dbName, dbName),
			})
		} else if isLogical && otherApps > 0 {
			notes = append(notes, Note{
				Reason: "database_delete_unverified",
				Ref:    it.ref,
				Message: fmt.Sprintf("Dockwell checked links, variables, and active connections for %s on %s, but can't see database names kept in app config files. Undo brings %s back for 7 days",
					dbName, svc, dbName),
			})
		}
	}
	return notes, nil
}

type envEntry struct {
	key string
	val string
}

func (e *Engine) serviceResolvedEnv(ctx context.Context, tx *store.Tx, items []item, env string, s *resourcev1.Service) ([]envEntry, error) {
	var out []envEntry
	for k, v := range s.GetApp().GetEnv() {
		out = append(out, envEntry{key: k, val: v})
	}
	for envKey, varName := range s.GetVariables() {
		name := cmp.Or(varName, envKey)
		spec, err := e.specAfter(ctx, tx, items, resource.Ref{Kind: variable.VariableKind, Scope: env, Name: name})
		if err != nil {
			return nil, err
		}
		if v, ok := spec.(*resourcev1.Variable); ok {
			if val := v.GetValue(); val != "" {
				out = append(out, envEntry{key: envKey, val: val})
			}
			if t := v.GetFrom().GetTemplate(); t != nil && t.GetText() != "" {
				out = append(out, envEntry{key: envKey, val: t.GetText()})
			}
		}
	}
	for envKey, secName := range s.GetSecrets() {
		name := cmp.Or(secName, envKey)
		spec, err := e.specAfter(ctx, tx, items, resource.Ref{Kind: variable.SecretKind, Scope: env, Name: name})
		if err != nil {
			return nil, err
		}
		if v, ok := spec.(*resourcev1.Secret); ok {
			if val := v.GetValue(); val != "" {
				out = append(out, envEntry{key: envKey, val: val})
			}
			if t := v.GetFrom().GetTemplate(); t != nil && t.GetText() != "" {
				out = append(out, envEntry{key: envKey, val: t.GetText()})
			}
		}
	}
	slices.SortFunc(out, func(a, b envEntry) int {
		if c := cmp.Compare(a.key, b.key); c != 0 {
			return c
		}
		return cmp.Compare(a.val, b.val)
	})
	return out, nil
}

func (e *Engine) engineHostsAndAdmin(ctx context.Context, tx *store.Tx, env, svc string, dbSvc *resourcev1.Service) (hosts []string, adminSecret string) {
	add := func(h string) {
		h = strings.TrimSpace(h)
		if h != "" && !slices.Contains(hosts, h) {
			hosts = append(hosts, h)
		}
	}
	add(svc)
	add(svc + "-1")
	add(env + "-" + svc + "-1")
	if dbSvc == nil {
		return hosts, ""
	}
	app := dbSvc.GetApp()
	for _, a := range app.GetAliases() {
		add(a)
	}
	for _, a := range app.GetNetworkAliases() {
		add(a)
	}
	add(app.GetHostname())
	if tree, serr := scope.Load(ctx, tx); serr == nil {
		for _, p := range tree.Projects {
			for _, envItem := range p.Environments {
				if envItem.ID == env && p.Name != "" && envItem.Name != "" {
					add(p.Name + "." + envItem.Name + "." + svc + "-1")
				}
			}
		}
	}
	// Collect admin password secret name from the database service spec.
	for _, key := range []string{"POSTGRES_PASSWORD", "MARIADB_ROOT_PASSWORD", "MYSQL_ROOT_PASSWORD"} {
		if secName, ok := dbSvc.GetSecrets()[key]; ok {
			adminSecret = cmp.Or(secName, key)
			break
		}
	}
	// Include published port host:port addresses (and bare IPs when published on default engine port).
	var defaultPort uint32
	if eng := database.Recognise(app.GetImage()); eng != nil {
		defaultPort = eng.Port
	}
	ports := app.GetPorts()
	if len(ports) > 0 {
		var serverAddrs []string
		addAddr := func(a string) {
			a = strings.TrimSpace(a)
			if a != "" && a != "0.0.0.0" && a != "::" && !slices.Contains(serverAddrs, a) {
				serverAddrs = append(serverAddrs, a)
			}
		}
		addAddr("127.0.0.1")
		addAddr("localhost")
		addAddr("host.docker.internal")
		if r, rerr := tx.GetResource(ctx, string(serviceKind), env, svc); rerr == nil && r.ID != "" {
			if deps, derr := tx.Deployments(ctx, r.ID, r.Revision); derr == nil {
				reports, _ := tx.ServerReports(ctx)
				for i := range deps {
					d := &deps[i]
					if raw, perr := tx.GetSetting(ctx, store.SettingPublicAddress(d.Server)); perr == nil && len(raw) > 0 {
						var netCfg dockwellv1.ServerNetwork
						if protojson.Unmarshal(raw, &netCfg) == nil {
							addAddr(netCfg.GetIpv4().GetAddress())
							addAddr(netCfg.GetIpv6().GetAddress())
						}
					}
					if rep, ok := reports[d.Server]; ok && len(rep.Inventory) > 0 {
						var inv dockwellv1.ServerInventory
						if proto.Unmarshal(rep.Inventory, &inv) == nil {
							addAddr(inv.GetOutboundAddress())
							for _, a := range inv.GetAddresses() {
								addAddr(a)
							}
						}
					}
				}
			}
		}
		for _, p := range ports {
			hp := p.GetHostPort()
			if hp == 0 {
				continue
			}
			ps := strconv.FormatUint(uint64(hp), 10)
			if hip := strings.TrimSpace(p.GetHostIp()); hip != "" && hip != "0.0.0.0" && hip != "::" {
				add(hip + ":" + ps)
				if defaultPort > 0 && hp == defaultPort {
					add(hip)
				}
			}
			for _, a := range serverAddrs {
				add(a + ":" + ps)
				if defaultPort > 0 && hp == defaultPort {
					add(a)
				}
			}
		}
	}
	return hosts, adminSecret
}

// specNamesDatabaseHosts reports whether a secret or variable spec's literal
// value or template text contains a connection string targeting (hosts, dbName).
func specNamesDatabaseHosts(spec proto.Message, hosts []string, dbName string) bool {
	switch v := spec.(type) {
	case *resourcev1.Secret:
		if database.ValueNamesDatabaseHosts(v.GetValue(), hosts, dbName) {
			return true
		}
		if t := v.GetFrom().GetTemplate(); t != nil && database.ValueNamesDatabaseHosts(t.GetText(), hosts, dbName) {
			return true
		}
	case *resourcev1.Variable:
		if database.ValueNamesDatabaseHosts(v.GetValue(), hosts, dbName) {
			return true
		}
		if t := v.GetFrom().GetTemplate(); t != nil && database.ValueNamesDatabaseHosts(t.GetText(), hosts, dbName) {
			return true
		}
	}
	return false
}

// appsUsingValue finds all services (excluding ignoreSvc) that still use start
// (a secret or variable) once the change applies, either directly or through
// derived secrets and variables.
func (e *Engine) appsUsingValue(ctx context.Context, tx *store.Tx, items []item, start resource.Ref, ignoreSvc string) ([]string, error) {
	visited := map[resource.Ref]bool{start: true}
	queue := []resource.Ref{start}
	var apps []string
	for len(queue) > 0 {
		cur := queue[0]
		queue = queue[1:]

		// Collect candidate dependents from the store and from the change's edits.
		var candidates []resource.Ref
		users, err := tx.UsersOf(ctx, cur.Scope, string(cur.Kind), cur.Name)
		if err != nil {
			return nil, err
		}
		for i := range users {
			ref := resource.Ref{Kind: resource.Kind(users[i].Kind), Scope: users[i].Scope, Name: users[i].Name}
			if !slices.Contains(candidates, ref) {
				candidates = append(candidates, ref)
			}
		}
		for i := range items {
			if items[i].spec == nil {
				continue
			}
			ref := items[i].ref
			if ref.Kind != serviceKind && ref.Kind != variable.SecretKind && ref.Kind != variable.VariableKind {
				continue
			}
			if !slices.Contains(candidates, ref) {
				candidates = append(candidates, ref)
			}
		}

		for _, cand := range candidates {
			if cand.Kind == serviceKind && cand.Scope == start.Scope && cand.Name == ignoreSvc {
				continue
			}
			spec, err := e.specAfter(ctx, tx, items, cand)
			if err != nil {
				return nil, err
			}
			if spec == nil {
				continue
			}
			if !specReferencesRef(cand.Scope, spec, cur) {
				continue
			}
			switch cand.Kind {
			case serviceKind:
				if !slices.Contains(apps, cand.Name) {
					apps = append(apps, cand.Name)
				}
			case variable.SecretKind, variable.VariableKind:
				if !visited[cand] {
					visited[cand] = true
					queue = append(queue, cand)
				}
			}
		}
	}
	slices.Sort(apps)
	return apps, nil
}

// specReferencesRef reports whether spec (at atScope) directly references target
// (a secret or variable).
func specReferencesRef(atScope string, spec proto.Message, target resource.Ref) bool {
	switch v := spec.(type) {
	case *resourcev1.Service:
		if atScope != target.Scope {
			return false
		}
		if target.Kind == variable.SecretKind {
			return usesSecret(v, target.Name)
		}
		if target.Kind == variable.VariableKind {
			for as, n := range v.GetVariables() {
				if cmp.Or(n, as) == target.Name {
					return true
				}
			}
		}
		return false
	case *resourcev1.Secret, *resourcev1.Variable:
		from := variable.FromOf(spec)
		if from == nil {
			return false
		}
		if val := from.GetValue(); val != nil {
			srcScope := cmp.Or(val.GetEnvironment(), atScope)
			srcKind := variable.VariableKind
			if val.GetSecret() {
				srcKind = variable.SecretKind
			}
			return srcScope == target.Scope && srcKind == target.Kind && val.GetName() == target.Name
		}
		if tmpl := from.GetTemplate(); tmpl != nil && atScope == target.Scope {
			for _, p := range tmpl.GetNames() {
				pKind := variable.VariableKind
				if p.GetSecret() {
					pKind = variable.SecretKind
				}
				if pKind == target.Kind && p.GetName() == target.Name {
					return true
				}
			}
		}
	}
	return false
}

// destinationKind is the backup destination kind.
const destinationKind resource.Kind = "backup.destination"

// aDestination is a backup destination as it is once the change applies, with its ref.
type aDestination struct {
	*resourcev1.BackupDestination
	ref resource.Ref
}

// allDestinations is every backup destination as it is once the change applies.
func (e *Engine) allDestinations(ctx context.Context, tx *store.Tx, items []item) ([]aDestination, error) {
	if t, _ := e.kinds.Type(destinationKind); t == nil {
		return nil, nil
	}
	stored, err := tx.ListKind(ctx, string(destinationKind))
	if err != nil {
		return nil, err
	}
	refs := make([]resource.Ref, 0, len(stored)+len(items))
	for _, r := range stored {
		refs = append(refs, resource.Ref{Kind: destinationKind, Scope: r.Scope, Name: r.Name})
	}
	for i := range items {
		if items[i].ref.Kind == destinationKind && !slices.Contains(refs, items[i].ref) {
			refs = append(refs, items[i].ref)
		}
	}
	out := make([]aDestination, 0, len(refs))
	for _, ref := range refs {
		spec, err := e.specAfter(ctx, tx, items, ref)
		if err != nil {
			return nil, err
		}
		if d, ok := spec.(*resourcev1.BackupDestination); ok {
			out = append(out, aDestination{BackupDestination: d, ref: ref})
		}
	}
	return out, nil
}

// checkLinkNotBackup refuses a link to a connection that any backup destination
// uses: an app's key, on every server it runs on, could otherwise delete the
// backups (ADR 0099).
func (e *Engine) checkLinkNotBackup(ctx context.Context, tx *store.Tx, items []item, connName string) error {
	dests, err := e.allDestinations(ctx, tx, items)
	if err != nil {
		return err
	}
	for i := range dests {
		if dests[i].GetConnection() == connName {
			return fault.New(ErrInvalid, "the connection %s is used for backups, so no app can link to it: an app's key could delete the backups. Add a separate S3 connection for the app",
				connName)
		}
	}
	return nil
}

// checkDestinationConnection refuses a backup destination naming a connection that
// isn't an unshared S3 connection using HTTPS (except loopback) whose current keys
// no app has received (ADR 0099).
func (e *Engine) checkDestinationConnection(ctx context.Context, tx *store.Tx, items []item, it *item, d *resourcev1.BackupDestination) error {
	org, err := tx.Organization(ctx)
	if err != nil {
		return err
	}
	name := d.GetConnection()
	spec, err := e.specAfter(ctx, tx, items, resource.Ref{Kind: variable.ConnectionKind, Scope: org.ID, Name: name})
	if err != nil {
		return err
	}
	c, ok := spec.(*resourcev1.ConnectionSpec)
	if !ok {
		return fault.New(ErrInvalid, "backup destination %s uses connection %s, which isn't on this install. Add %s first, in this change or an earlier one",
			it.ref.Name, name, name)
	}
	if c.GetKind() != resourcev1.LinkKind_LINK_KIND_S3 {
		return fault.New(ErrInvalid, "backup destination %s names the connection %s, which isn't S3 storage. Pick an S3 connection, or add one on Connections",
			it.ref.Name, name)
	}
	if err := backuppolicy.CheckTransport(c.GetS3().GetEndpoint(), c.GetS3().GetBucket(), c.GetS3().GetPathStyle()); err != nil {
		return fault.New(ErrInvalid, "backup destination %s can't use the connection %s: its endpoint %s. Change %s to HTTPS, or pick another S3 connection",
			it.ref.Name, name, err.Error(), name)
	}
	if c.GetShared() {
		return fault.New(ErrInvalid, "backup destination %s can't use the shared connection %s, because a connection backups use serves no app. Add a dedicated S3 connection for backups, or unshare %s first",
			it.ref.Name, name, name)
	}
	links, err := e.allLinks(ctx, tx, items)
	if err != nil {
		return err
	}
	for i := range links {
		if links[i].GetConnection() == name {
			where := whichScope(ctx, tx, links[i].ref.Scope)
			return fault.New(ErrInvalid, "backup destination %s can't use the connection %s, because link %s in %s uses it and an app's key could delete the backups. Add a separate S3 connection for backups",
				it.ref.Name, name, links[i].ref.Name, where)
		}
	}
	return e.checkConnectionNotUsedByApp(ctx, tx, items, org.ID, name, c)
}

// appKeyRefusal is the refusal when a backup destination tries to adopt a
// connection whose current keys an app has already received (ADR 0099, #831).
const appKeyRefusal = "An app received this connection's keys. Paste a key made for backups alone, and revoke the one the app had at your provider."

// revInterval is a span of revisions during which a connection held one key pair.
type revInterval struct {
	start store.Revision
	end   *store.Revision
}

// revBefore reports whether revision a was committed strictly before revision b:
// by timestamp first, and within the same millisecond by change ID (ULID order).
func revBefore(a, b store.Revision) bool {
	if !a.CreatedAt.Equal(b.CreatedAt) {
		return a.CreatedAt.Before(b.CreatedAt)
	}
	return a.ChangeID < b.ChangeID
}

// checkConnectionNotUsedByApp refuses adopting a connection for backups if any app
// has received its current keys: a link named it while it held those keys (including
// one being deleted in this same change), an app secret holds the current secret key,
// or an environment's own values held its bucket since its keys were last set.
func (e *Engine) checkConnectionNotUsedByApp(ctx context.Context, tx *store.Tx, items []item, orgID, connName string, afterC *resourcev1.ConnectionSpec) error {
	s3c := afterC.GetS3()
	if s3c == nil {
		return nil
	}
	curAK, curSK := s3c.GetAccessKeyId(), s3c.GetSecretKey()
	var (
		intervals  []revInterval
		keysSetRev *store.Revision
	)
	connRes, err := tx.GetResource(ctx, string(variable.ConnectionKind), orgID, connName)
	if err != nil && !errors.Is(err, store.ErrNotFound) {
		return err
	}
	if err == nil {
		revs, err := tx.PageRevisions(ctx, connRes.ID, 0, 1000)
		if err != nil {
			return err
		}
		slices.Reverse(revs)
		var open *revInterval
		for i := range revs {
			r := revs[i]
			matched := false
			if r.Spec != nil {
				spec, err := e.decode(ctx, tx, store.InRevision(r.ID, r.Kind, r.Revision), r.Spec)
				if err != nil {
					return err
				}
				if cs, ok := spec.(*resourcev1.ConnectionSpec); ok && cs.GetKind() == resourcev1.LinkKind_LINK_KIND_S3 && cs.GetS3() != nil {
					matched = cs.GetS3().GetAccessKeyId() == curAK && cs.GetS3().GetSecretKey() == curSK
				}
			}
			switch {
			case matched && open == nil:
				iv := revInterval{start: r}
				open = &iv
			case !matched && open != nil:
				end := r
				open.end = &end
				intervals = append(intervals, *open)
				open = nil
			}
		}
		if open != nil {
			intervals = append(intervals, *open)
			start := open.start
			keysSetRev = &start
		}
	}

	// 1. Check historical and currently live links against every interval where
	// this connection held its current keys.
	if len(intervals) > 0 {
		linkRevs, err := tx.RevisionsOfKind(ctx, string(linkKind))
		if err != nil {
			return err
		}
		for i := range linkRevs {
			lr := linkRevs[i]
			if lr.Spec == nil {
				continue
			}
			spec, err := e.decode(ctx, tx, store.InRevision(lr.ID, lr.Kind, lr.Revision), lr.Spec)
			if err != nil {
				return err
			}
			l, ok := spec.(*resourcev1.Link)
			if !ok || l.GetConnection() != connName {
				continue
			}
			var linkEnd *store.Revision
			if i+1 < len(linkRevs) && linkRevs[i+1].ID == lr.ID {
				next := linkRevs[i+1]
				linkEnd = &next
			}
			for j := range intervals {
				iv := &intervals[j]
				if (linkEnd == nil || !revBefore(*linkEnd, iv.start)) && (iv.end == nil || !revBefore(*iv.end, lr)) {
					return fault.New(ErrInvalid, "%s", appKeyRefusal)
				}
			}
		}
	}

	// 2. Check every live secret after this change: if any environment holds the
	// connection's current secret key directly (e.g. copied by DisconnectStorage),
	// an app still holds these keys.
	if curSK != "" {
		storedSecrets, err := tx.ListKind(ctx, string(variable.SecretKind))
		if err != nil {
			return err
		}
		secretRefs := make([]resource.Ref, 0, len(storedSecrets))
		for i := range storedSecrets {
			secretRefs = append(secretRefs, resource.Ref{Kind: variable.SecretKind, Scope: storedSecrets[i].Scope, Name: storedSecrets[i].Name})
		}
		for i := range items {
			if items[i].ref.Kind == variable.SecretKind && !slices.Contains(secretRefs, items[i].ref) {
				secretRefs = append(secretRefs, items[i].ref)
			}
		}
		for _, ref := range secretRefs {
			spec, err := e.specAfter(ctx, tx, items, ref)
			if err != nil {
				return err
			}
			if sec, ok := spec.(*resourcev1.Secret); ok && sec.GetFrom() == nil && sec.GetValue() == curSK {
				return fault.New(ErrInvalid, "%s", appKeyRefusal)
			}
		}
	}

	// 3. Check whether the connection is held (some environment's own plain
	// variables name its endpoint and bucket) since its current keys were last set.
	if ep, bkt := s3c.GetEndpoint(), s3c.GetBucket(); ep != "" && bkt != "" {
		type heldMark struct {
			hasEP, hasBkt bool
			epNew, bktNew bool
			epRev, bktRev *store.Revision
		}
		byEnv := map[string]*heldMark{}
		mark := func(env string) *heldMark {
			if byEnv[env] == nil {
				byEnv[env] = &heldMark{}
			}
			return byEnv[env]
		}
		inChange := map[resource.Ref]*item{}
		for i := range items {
			if items[i].ref.Kind == variable.VariableKind {
				inChange[items[i].ref] = &items[i]
			}
		}
		storedVars, err := tx.ListKind(ctx, string(variable.VariableKind))
		if err != nil {
			return err
		}
		for i := range storedVars {
			r := &storedVars[i]
			ref := resource.Ref{Kind: variable.VariableKind, Scope: r.Scope, Name: r.Name}
			if _, edited := inChange[ref]; edited {
				continue
			}
			spec, err := e.decode(ctx, tx, store.InRevision(r.ID, r.Kind, r.Revision), r.Spec)
			if err != nil {
				return err
			}
			v, ok := spec.(*resourcev1.Variable)
			if !ok || v.GetFrom() != nil {
				continue
			}
			if v.GetValue() != ep && v.GetValue() != bkt {
				continue
			}
			rev, err := tx.GetRevision(ctx, r.ID, r.Revision)
			if err != nil {
				return err
			}
			m := mark(r.Scope)
			if v.GetValue() == ep && (!m.hasEP || m.epRev != nil && revBefore(*m.epRev, rev)) {
				cp := rev
				m.hasEP, m.epRev = true, &cp
			}
			if v.GetValue() == bkt && (!m.hasBkt || m.bktRev != nil && revBefore(*m.bktRev, rev)) {
				cp := rev
				m.hasBkt, m.bktRev = true, &cp
			}
		}
		for _, it := range inChange {
			v, ok := it.spec.(*resourcev1.Variable)
			if !ok || v.GetFrom() != nil {
				continue
			}
			m := mark(it.ref.Scope)
			if v.GetValue() == ep {
				m.hasEP, m.epNew = true, true
			}
			if v.GetValue() == bkt {
				m.hasBkt, m.bktNew = true, true
			}
		}
		for _, m := range byEnv {
			if !m.hasEP || !m.hasBkt {
				continue
			}
			if m.epNew || m.bktNew {
				return fault.New(ErrInvalid, "%s", appKeyRefusal)
			}
			if keysSetRev != nil {
				if (m.epRev != nil && !revBefore(*m.epRev, *keysSetRev)) || (m.bktRev != nil && !revBefore(*m.bktRev, *keysSetRev)) {
					return fault.New(ErrInvalid, "%s", appKeyRefusal)
				}
			}
		}
	}
	return nil
}

// backupTarget is the effective storage location of a backup destination, whether
// inline or connection-backed.
type backupTarget struct {
	endpoint  string
	region    string
	bucket    string
	pathStyle bool
	sftp      bool
	sftpUser  string
	sftpPath  string
}

func normalizeTarget(endpoint, region, bucket string, pathStyle bool) backupTarget {
	trimmed := strings.TrimRight(endpoint, "/")
	ep := trimmed
	if u, err := url.Parse(trimmed); err == nil && u.Host != "" {
		u.RawQuery = ""
		u.ForceQuery = false
		u.Fragment = ""
		ep = u.String()
	}
	return backupTarget{
		endpoint:  ep,
		region:    cmp.Or(region, "auto"),
		bucket:    bucket,
		pathStyle: pathStyle,
	}
}

func targetOfDestination(d *resourcev1.BackupDestination, lookupConn func(string) (*resourcev1.ConnectionSpec, error)) (backupTarget, bool, error) {
	if d == nil {
		return backupTarget{}, false, nil
	}
	if sftp := d.GetSftp(); sftp != nil {
		return backupTarget{
			sftp: true, endpoint: backuppolicy.SFTPConfig(sftp).Address(), sftpUser: sftp.GetUser(), sftpPath: sftp.GetPath(),
		}, true, nil
	}
	if d.GetConnection() == "" {
		return normalizeTarget(d.GetEndpoint(), d.GetRegion(), d.GetBucket(), true), true, nil
	}
	c, err := lookupConn(d.GetConnection())
	if err != nil {
		return backupTarget{}, false, err
	}
	if c == nil || c.GetS3() == nil {
		return backupTarget{}, false, nil
	}
	s3c := c.GetS3()
	return normalizeTarget(s3c.GetEndpoint(), s3c.GetRegion(), s3c.GetBucket(), s3c.GetPathStyle()), true, nil
}

// checkDestinationTarget refuses any destination edit (including one bundled with
// a connection edit, or recreating a deleted destination name) that changes the
// effective endpoint, region, bucket or path style while backups exist on that
// destination (ADR 0099 line 175, #831).
func (e *Engine) checkDestinationTarget(ctx context.Context, tx *store.Tx, items []item, it *item, afterD *resourcev1.BackupDestination) error {
	if afterD.GetSftp() != nil {
		control, err := tx.ControlBackup(ctx)
		if err != nil {
			return err
		}
		if control.Destination == it.ref.Name {
			return fault.New(ErrInvalid, "backup destination %s is selected for control plane backups, which require S3-compatible storage. Change the control plane's destination first", it.ref.Name)
		}
	}
	allBackups, err := tx.ListBackups(ctx, store.BackupFilter{})
	if err != nil {
		return err
	}
	if !destinationHasBackups(allBackups, it.ref.Name) {
		return nil
	}
	org, err := tx.Organization(ctx)
	if err != nil {
		return err
	}
	afterTarget, haveAfter, err := targetOfDestination(afterD, func(name string) (*resourcev1.ConnectionSpec, error) {
		spec, err := e.specAfter(ctx, tx, items, resource.Ref{Kind: variable.ConnectionKind, Scope: org.ID, Name: name})
		if err != nil {
			return nil, err
		}
		c, _ := spec.(*resourcev1.ConnectionSpec)
		return c, nil
	})
	if err != nil || !haveAfter {
		return err
	}

	var (
		beforeTarget backupTarget
		haveBefore   bool
	)
	if beforeD, ok := it.current.(*resourcev1.BackupDestination); ok && beforeD != nil {
		beforeTarget, haveBefore, err = targetOfDestination(beforeD, func(name string) (*resourcev1.ConnectionSpec, error) {
			loaded, err := e.load(ctx, tx, resource.Ref{Kind: variable.ConnectionKind, Scope: org.ID, Name: name})
			if err != nil {
				return nil, err
			}
			c, _ := loaded.current.(*resourcev1.ConnectionSpec)
			return c, nil
		})
		if err != nil {
			return err
		}
	} else if it.base > 0 && it.id != "" {
		// Recreating a destination name that was deleted while remote copies still exist:
		// compare against the destination's last non-deleted revision so deleting and
		// recreating under the same name cannot repoint existing remote copies.
		revs, err := tx.PageRevisions(ctx, it.id, 0, 50)
		if err != nil {
			return err
		}
		var (
			prevDest  *resourcev1.BackupDestination
			deletedAt *store.Revision
		)
		for i := range revs {
			if revs[i].Spec == nil {
				cp := revs[i]
				deletedAt = &cp
				continue
			}
			spec, err := e.decode(ctx, tx, store.InRevision(revs[i].ID, revs[i].Kind, revs[i].Revision), revs[i].Spec)
			if err != nil {
				return err
			}
			if d, ok := spec.(*resourcev1.BackupDestination); ok {
				prevDest = d
				break
			}
		}
		if prevDest != nil {
			beforeTarget, haveBefore, err = targetOfDestination(prevDest, func(name string) (*resourcev1.ConnectionSpec, error) {
				cr, err := tx.GetResource(ctx, string(variable.ConnectionKind), org.ID, name)
				if errors.Is(err, store.ErrNotFound) {
					return nil, nil
				}
				if err != nil {
					return nil, err
				}
				crevs, err := tx.PageRevisions(ctx, cr.ID, 0, 50)
				if err != nil {
					return nil, err
				}
				for j := range crevs {
					if crevs[j].Spec == nil {
						continue
					}
					if deletedAt != nil && (crevs[j].ChangeID == deletedAt.ChangeID || !revBefore(crevs[j], *deletedAt)) {
						continue
					}
					spec, err := e.decode(ctx, tx, store.InRevision(crevs[j].ID, crevs[j].Kind, crevs[j].Revision), crevs[j].Spec)
					if err != nil {
						return nil, err
					}
					if c, ok := spec.(*resourcev1.ConnectionSpec); ok {
						return c, nil
					}
				}
				return nil, nil
			})
			if err != nil {
				return err
			}
		}
	}
	if !haveBefore {
		return fault.New(ErrInvalid, "backup destination %s still has backups, and Dockwell can't verify its earlier storage location to match them. Add a new destination under another name, or delete the backups on %s first",
			it.ref.Name, it.ref.Name)
	}
	if beforeTarget != afterTarget {
		if beforeTarget.sftp || afterTarget.sftp {
			return fault.New(ErrInvalid, "backup destination %s can't change its storage location while backups depend on it. Add a new destination, or delete the backups on %s first", it.ref.Name, it.ref.Name)
		}
		return fault.New(ErrInvalid, "backup destination %s can't change its endpoint, region, bucket or path style while it has backups there, because restores fetch from where each backup went. Add a new destination for the new bucket, or delete the backups on %s first",
			it.ref.Name, it.ref.Name)
	}
	return nil
}

// checkConnectionDestinations refuses deleting, retyping, marking shared, using
// plain HTTP away from loopback, rotating back to app-held keys, or moving the
// bucket/endpoint/region/path_style of a connection while a backup destination
// names it (ADR 0099). Moving the storage target is refused only when the
// destination has backups stored or in flight; rotating to a fresh key is allowed.
func (e *Engine) checkConnectionDestinations(ctx context.Context, tx *store.Tx, items []item, it *item) error {
	if it.ref.Kind != variable.ConnectionKind {
		return nil
	}
	dests, err := e.allDestinations(ctx, tx, items)
	if err != nil {
		return err
	}
	after, _ := it.spec.(*resourcev1.ConnectionSpec)
	before, _ := it.current.(*resourcev1.ConnectionSpec)
	var (
		allBackups    []store.Backup
		loadedBackups bool
		checkedKeys   bool
	)
	editedInChange := func(ref resource.Ref) bool {
		for k := range items {
			if items[k].ref == ref {
				return true
			}
		}
		return false
	}
	for i := range dests {
		d := &dests[i]
		if d.GetConnection() != it.ref.Name {
			continue
		}
		if it.spec == nil {
			also := ", or in the same change"
			if it.reverses {
				also = ""
			}
			return fault.New(ErrInvalid, "backup destination %s still uses the connection %s, which this change deletes. Remove or update %s first%s",
				d.ref.Name, it.ref.Name, d.ref.Name, also)
		}
		if after.GetKind() != resourcev1.LinkKind_LINK_KIND_S3 {
			return fault.New(ErrInvalid, "backup destination %s still uses the connection %s, which this change makes another kind of connection. Update or remove %s first, or add a connection of the new kind",
				d.ref.Name, it.ref.Name, d.ref.Name)
		}
		if after.GetShared() {
			return fault.New(ErrInvalid, "the connection %s can't be marked shared while backup destination %s uses it, because a connection backups use serves no app. Point %s at another connection first, or keep %s unshared",
				it.ref.Name, d.ref.Name, d.ref.Name, it.ref.Name)
		}
		// When d is itself edited or recreated in this change, checkDestinationTarget
		// validates d's effective target before vs after (including restoring a retired
		// destination and its connection's original bucket together).
		if !editedInChange(d.ref) && before != nil && before.GetS3() != nil && after.GetS3() != nil && movedBackupTarget(before.GetS3(), after.GetS3()) {
			if !loadedBackups {
				if allBackups, err = tx.ListBackups(ctx, store.BackupFilter{}); err != nil {
					return err
				}
				loadedBackups = true
			}
			if destinationHasBackups(allBackups, d.ref.Name) {
				return fault.New(ErrInvalid, "the connection %s can't change its endpoint, region, bucket or path style while backup destination %s has backups there, because restores fetch from where each backup went. Add a new connection for the new bucket, or delete the backups on %s first",
					it.ref.Name, d.ref.Name, d.ref.Name)
			}
		}
		if err := backuppolicy.CheckTransport(after.GetS3().GetEndpoint(), after.GetS3().GetBucket(), after.GetS3().GetPathStyle()); err != nil {
			return fault.New(ErrInvalid, "the connection %s can't use that endpoint while backup destination %s uses it: its endpoint %s. Keep %s on HTTPS, or point %s at another connection first",
				it.ref.Name, d.ref.Name, err.Error(), it.ref.Name, d.ref.Name)
		}
		if !checkedKeys && keysChanged(before.GetS3(), after.GetS3()) {
			checkedKeys = true
			if err := e.checkConnectionNotUsedByApp(ctx, tx, items, it.ref.Scope, it.ref.Name, after); err != nil {
				return err
			}
		}
	}
	return nil
}

func keysChanged(from, to *resourcev1.ConnectionS3) bool {
	if from == nil || to == nil {
		return to != nil
	}
	return from.GetAccessKeyId() != to.GetAccessKeyId() || from.GetSecretKey() != to.GetSecretKey()
}

// alertChannelKind is the alert channel kind.
const alertChannelKind resource.Kind = alert.ChannelKind

// anAlertChannel is an alert channel as it is once the change applies, with its ref.
type anAlertChannel struct {
	*resourcev1.AlertChannel
	ref resource.Ref
}

// callerIsInstallAdmin reports whether ctx carries no caller (system/test) or an
// install-level admin. Below install admin, fail-closed errors name no other
// channel or connection.
func callerIsInstallAdmin(ctx context.Context) bool {
	c, ok := place.CallerFrom(ctx)
	if !ok {
		return true
	}
	return store.RoleRank(place.Holds(c.Grants, place.Install)) >= store.RoleRank(store.RoleAdmin)
}

// allAlertChannels is every alert channel as it is once the change applies.
// It fails closed on any decode error, redacting channel names below install admin.
func (e *Engine) allAlertChannels(ctx context.Context, tx *store.Tx, items []item) ([]anAlertChannel, error) {
	if t, _ := e.kinds.Type(alertChannelKind); t == nil {
		return nil, nil
	}
	stored, err := tx.ListKind(ctx, string(alertChannelKind))
	if err != nil {
		return nil, err
	}
	refs := make([]resource.Ref, 0, len(stored)+len(items))
	for _, r := range stored {
		refs = append(refs, resource.Ref{Kind: alertChannelKind, Scope: r.Scope, Name: r.Name})
	}
	for i := range items {
		if items[i].ref.Kind == alertChannelKind && !slices.Contains(refs, items[i].ref) {
			refs = append(refs, items[i].ref)
		}
	}
	out := make([]anAlertChannel, 0, len(refs))
	for _, ref := range refs {
		spec, err := e.specAfter(ctx, tx, items, ref)
		if err != nil {
			if !callerIsInstallAdmin(ctx) {
				return nil, fault.New(ErrUnavailable, "can't check whether an alert channel uses this connection. Ask an install admin to check the alert channels")
			}
			return nil, err
		}
		if ch, ok := spec.(*resourcev1.AlertChannel); ok {
			out = append(out, anAlertChannel{AlertChannel: ch, ref: ref})
		}
	}
	return out, nil
}

// checkLinkNotAlertChannel refuses a link to a mail connection that any alert
// channel uses, or whose (host, user, password) credential equals an alert
// connection's: an app holding those credentials could otherwise send mail as
// the alert sender (ADR 0099, #833). S3 links skip the alert-channel scan (M1).
func (e *Engine) checkLinkNotAlertChannel(ctx context.Context, tx *store.Tx, items []item, l *resourcev1.Link) error {
	connName := l.GetConnection()
	org, err := tx.Organization(ctx)
	if err != nil {
		return err
	}
	spec, err := e.specAfter(ctx, tx, items, resource.Ref{Kind: variable.ConnectionKind, Scope: org.ID, Name: connName})
	if err != nil {
		return err
	}
	connSpec, _ := spec.(*resourcev1.ConnectionSpec)
	if l.GetKind() != resourcev1.LinkKind_LINK_KIND_SMTP && connSpec.GetKind() != resourcev1.LinkKind_LINK_KIND_SMTP {
		return nil
	}
	channels, err := e.allAlertChannels(ctx, tx, items)
	if err != nil {
		return err
	}
	var alertConns []string
	for i := range channels {
		acName := channels[i].GetEmail().GetConnection()
		if acName == "" {
			continue
		}
		if acName == connName {
			return fault.New(ErrInvalid, "the connection %s is used for alert email, so no app can link to it: an app could otherwise send mail as the alert sender. Add a separate mail connection for the app",
				connName)
		}
		if !slices.Contains(alertConns, acName) {
			alertConns = append(alertConns, acName)
		}
	}
	if connSpec.GetKind() == resourcev1.LinkKind_LINK_KIND_SMTP && connSpec.GetSmtp().GetPassword() != "" {
		for _, acName := range alertConns {
			acSpec, err := e.specAfter(ctx, tx, items, resource.Ref{Kind: variable.ConnectionKind, Scope: org.ID, Name: acName})
			if err != nil {
				if !callerIsInstallAdmin(ctx) {
					return fault.New(ErrUnavailable, "can't check whether an alert channel uses this connection. Ask an install admin to check the alert channels")
				}
				return err
			}
			if ac, ok := acSpec.(*resourcev1.ConnectionSpec); ok && ac.GetKind() == resourcev1.LinkKind_LINK_KIND_SMTP &&
				connection.SameSMTPCredential(connSpec.GetSmtp(), ac.GetSmtp()) {
				return fault.New(ErrInvalid, "the connection %s uses the same mail account credentials as an alert email connection, so no app can link to it: an app could otherwise send mail as the alert sender. Use separate mail credentials for the app",
					connName)
			}
		}
	}
	return nil
}

// checkAlertChannelConnection refuses an alert channel naming a connection that
// isn't an unshared SMTP connection passing alert-email TLS rules whose current
// password no app holds (ADR 0099, #833).
func (e *Engine) checkAlertChannelConnection(ctx context.Context, tx *store.Tx, items []item, it *item, em *resourcev1.AlertEmail) error {
	org, err := tx.Organization(ctx)
	if err != nil {
		return err
	}
	name := em.GetConnection()
	spec, err := e.specAfter(ctx, tx, items, resource.Ref{Kind: variable.ConnectionKind, Scope: org.ID, Name: name})
	if err != nil {
		return err
	}
	c, ok := spec.(*resourcev1.ConnectionSpec)
	if !ok {
		return fault.New(ErrInvalid, "alert channel %s uses connection %s, which isn't on this install. Add %s first, in this change or an earlier one",
			it.ref.Name, name, name)
	}
	if c.GetKind() != resourcev1.LinkKind_LINK_KIND_SMTP || c.GetSmtp() == nil {
		return fault.New(ErrInvalid, "alert channel %s names the connection %s, which isn't a mail server. Pick a mail connection, or add one on Connections",
			it.ref.Name, name)
	}
	smtp := c.GetSmtp()
	if err := alert.CheckEmailTransport(smtp.GetHost(), resourcev1.AlertEmail_Security(connection.Security(smtp)), smtp.GetUser() != ""); err != nil {
		return fault.New(ErrInvalid, "alert channel %s can't use the connection %s: %s. Update %s, or pick another mail connection",
			it.ref.Name, name, err.Error(), name)
	}
	if c.GetShared() {
		return fault.New(ErrInvalid, "alert channel %s can't use the shared connection %s, because a connection alerts use serves no app. Add a dedicated mail connection for alerts, or unshare %s first",
			it.ref.Name, name, name)
	}
	links, err := e.allLinks(ctx, tx, items)
	if err != nil {
		return err
	}
	for i := range links {
		if links[i].GetConnection() == name {
			where := whichScope(ctx, tx, links[i].ref.Scope)
			return fault.New(ErrInvalid, "alert channel %s can't use the connection %s, because link %s in %s uses it and an app could otherwise send mail as the alert sender. Add a separate mail connection for alerts",
				it.ref.Name, name, links[i].ref.Name, where)
		}
	}
	return e.checkMailConnectionNotUsedByApp(ctx, tx, items, org.ID, name, c)
}

// checkMailConnectionNotUsedByApp refuses adopting or rotating an alert mail
// connection onto a credential an app holds, without any fixed-horizon history
// scan (ADR 0099, #833): it checks links removed in the same change (unless the
// connection's password rotates in that change) and consults the shared
// connection.SMTPHeldByApp predicate across all app-linked SMTP connections and
// live plain secrets (including recognized smtp:// and smtps:// URLs).
func (e *Engine) checkMailConnectionNotUsedByApp(ctx context.Context, tx *store.Tx, items []item, orgID, connName string, afterC *resourcev1.ConnectionSpec) error {
	m := afterC.GetSmtp()
	if m == nil || m.GetPassword() == "" {
		return nil
	}
	refuse := func() error {
		return fault.New(ErrInvalid, "an app holds the password of connection %s. Paste a password made for alerts alone on %s, and revoke the one the app had at your mail provider",
			connName, connName)
	}
	// 1. If this change deletes or repoints a link that named connName, and doesn't
	// rotate connName's password to a different value in the same change, the app
	// that link served already received this password.
	var connItem *item
	for i := range items {
		if items[i].ref.Kind == variable.ConnectionKind && items[i].ref.Name == connName {
			connItem = &items[i]
			break
		}
	}
	passwordRotated := false
	if connItem != nil {
		if beforeC, ok := connItem.current.(*resourcev1.ConnectionSpec); ok && beforeC.GetSmtp() != nil {
			passwordRotated = beforeC.GetSmtp().GetPassword() != m.GetPassword()
		}
	}
	if !passwordRotated {
		for i := range items {
			if items[i].ref.Kind != linkKind {
				continue
			}
			if beforeL, ok := items[i].current.(*resourcev1.Link); ok && beforeL.GetConnection() == connName {
				return refuse()
			}
		}
	}
	// 2. Check the shared predicate across every app-linked SMTP connection
	// (same account under another name) and every live plain secret (literal
	// password or recognized smtp:// / smtps:// URL).
	links, err := e.allLinks(ctx, tx, items)
	if err != nil {
		return err
	}
	var linkedNames []string
	for i := range links {
		if n := links[i].GetConnection(); n != "" && n != connName && !slices.Contains(linkedNames, n) {
			linkedNames = append(linkedNames, n)
		}
	}
	linkedAppConns := make([]*resourcev1.ConnectionSmtp, 0, len(linkedNames))
	for _, n := range linkedNames {
		spec, err := e.specAfter(ctx, tx, items, resource.Ref{Kind: variable.ConnectionKind, Scope: orgID, Name: n})
		if err != nil {
			return err
		}
		if cs, ok := spec.(*resourcev1.ConnectionSpec); ok && cs.GetKind() == resourcev1.LinkKind_LINK_KIND_SMTP && cs.GetSmtp() != nil {
			linkedAppConns = append(linkedAppConns, cs.GetSmtp())
		}
	}
	storedSecrets, err := tx.ListKind(ctx, string(variable.SecretKind))
	if err != nil {
		return err
	}
	secretRefs := make([]resource.Ref, 0, len(storedSecrets))
	for i := range storedSecrets {
		secretRefs = append(secretRefs, resource.Ref{Kind: variable.SecretKind, Scope: storedSecrets[i].Scope, Name: storedSecrets[i].Name})
	}
	for i := range items {
		if items[i].ref.Kind == variable.SecretKind && !slices.Contains(secretRefs, items[i].ref) {
			secretRefs = append(secretRefs, items[i].ref)
		}
	}
	plainSecrets := make([]string, 0, len(secretRefs))
	for _, ref := range secretRefs {
		spec, err := e.specAfter(ctx, tx, items, ref)
		if err != nil {
			return err
		}
		if sec, ok := spec.(*resourcev1.Secret); ok && sec.GetFrom() == nil && sec.GetValue() != "" {
			plainSecrets = append(plainSecrets, sec.GetValue())
		}
	}
	if connection.SMTPHeldByApp(m, linkedAppConns, plainSecrets) {
		return refuse()
	}
	return nil
}

// checkConnectionAlertChannels refuses deleting, retyping, marking shared,
// breaking alert-email TLS rules, or rotating back to an app-held credential on
// an SMTP connection while an alert channel names it, and refuses rotating an
// app-linked SMTP connection onto an alert connection's credential (ADR 0099,
// #833). S3-only connection edits skip the alert-channel scan (M1).
func (e *Engine) checkConnectionAlertChannels(ctx context.Context, tx *store.Tx, items []item, it *item) error {
	if it.ref.Kind != variable.ConnectionKind {
		return nil
	}
	after, _ := it.spec.(*resourcev1.ConnectionSpec)
	before, _ := it.current.(*resourcev1.ConnectionSpec)
	if before.GetKind() != resourcev1.LinkKind_LINK_KIND_SMTP && after.GetKind() != resourcev1.LinkKind_LINK_KIND_SMTP {
		return nil
	}
	channels, err := e.allAlertChannels(ctx, tx, items)
	if err != nil {
		return err
	}
	checkedPassword := false
	usedByAlert := false
	var otherAlertConns []string
	for i := range channels {
		ch := &channels[i]
		acName := ch.GetEmail().GetConnection()
		if acName == "" {
			continue
		}
		if acName != it.ref.Name {
			if !slices.Contains(otherAlertConns, acName) {
				otherAlertConns = append(otherAlertConns, acName)
			}
			continue
		}
		usedByAlert = true
		if it.spec == nil {
			also := ", or in the same change"
			if it.reverses {
				also = ""
			}
			return fault.New(ErrInvalid, "alert channel %s still uses the connection %s, which this change deletes. Remove or update %s first%s",
				ch.ref.Name, it.ref.Name, ch.ref.Name, also)
		}
		if after.GetKind() != resourcev1.LinkKind_LINK_KIND_SMTP || after.GetSmtp() == nil {
			return fault.New(ErrInvalid, "alert channel %s still uses the connection %s, which this change makes another kind of connection. Update or remove %s first, or add a connection of the new kind",
				ch.ref.Name, it.ref.Name, ch.ref.Name)
		}
		if after.GetShared() {
			return fault.New(ErrInvalid, "the connection %s can't be marked shared while alert channel %s uses it, because a connection alerts use serves no app. Point %s at another connection first, or keep %s unshared",
				it.ref.Name, ch.ref.Name, ch.ref.Name, it.ref.Name)
		}
		afterSMTP := after.GetSmtp()
		if err := alert.CheckEmailTransport(afterSMTP.GetHost(), resourcev1.AlertEmail_Security(connection.Security(afterSMTP)), afterSMTP.GetUser() != ""); err != nil {
			return fault.New(ErrInvalid, "the connection %s can't use those mail settings while alert channel %s uses it: %s. Keep %s on TLS, or point %s at another connection first",
				it.ref.Name, ch.ref.Name, err.Error(), it.ref.Name, ch.ref.Name)
		}
		if !checkedPassword && smtpCredentialIdentityChanged(before.GetSmtp(), afterSMTP) {
			checkedPassword = true
			if err := e.checkMailConnectionNotUsedByApp(ctx, tx, items, it.ref.Scope, it.ref.Name, after); err != nil {
				return err
			}
		}
	}
	// If this edit leaves an app-linked SMTP connection with the same (host, user,
	// password) credential as an alert connection, refuse it (H3, #833).
	if !usedByAlert && len(otherAlertConns) > 0 && after != nil && after.GetKind() == resourcev1.LinkKind_LINK_KIND_SMTP && after.GetSmtp().GetPassword() != "" {
		links, err := e.allLinks(ctx, tx, items)
		if err != nil {
			return err
		}
		hasAppLink := slices.ContainsFunc(links, func(l aLink) bool { return l.GetConnection() == it.ref.Name })
		if hasAppLink {
			for _, acName := range otherAlertConns {
				acSpec, err := e.specAfter(ctx, tx, items, resource.Ref{Kind: variable.ConnectionKind, Scope: it.ref.Scope, Name: acName})
				if err != nil {
					return err
				}
				if ac, ok := acSpec.(*resourcev1.ConnectionSpec); ok && ac.GetKind() == resourcev1.LinkKind_LINK_KIND_SMTP &&
					connection.SameSMTPCredential(after.GetSmtp(), ac.GetSmtp()) {
					return fault.New(ErrInvalid, "the connection %s serves an app link and can't use the same mail account credentials as an alert email connection, because an app could otherwise send mail as the alert sender. Use separate mail credentials for %s",
						it.ref.Name, it.ref.Name)
				}
			}
		}
	}
	return nil
}

func smtpCredentialIdentityChanged(from, to *resourcev1.ConnectionSmtp) bool {
	ca, okA := connection.CredentialOf(from)
	cb, okB := connection.CredentialOf(to)
	if !okB {
		return false
	}
	return !okA || ca != cb
}

// movedBackupTarget says whether an edit changes where a connection's S3 requests go
// (endpoint, region, bucket or path style), as distinct from rotating its keys.
func movedBackupTarget(from, to *resourcev1.ConnectionS3) bool {
	return normalizeTarget(from.GetEndpoint(), from.GetRegion(), from.GetBucket(), from.GetPathStyle()) !=
		normalizeTarget(to.GetEndpoint(), to.GetRegion(), to.GetBucket(), to.GetPathStyle())
}

// destinationHasBackups reports whether a destination has any backup stored there or
// currently running or sending to it.
func destinationHasBackups(all []store.Backup, name string) bool {
	for i := range all {
		b := &all[i]
		if b.Destination != name {
			continue
		}
		if b.CopyKey != "" || b.State == store.BackupRunning || (b.State == store.BackupDone && b.CopyError == "") {
			return true
		}
	}
	return false
}

// writesSecurity says a link to a mail server tells its app the connection's security,
// which it writes in the app's words.
func writesSecurity(l *resourcev1.Link) bool {
	return slices.ContainsFunc(l.GetSets(), func(s *resourcev1.LinkSet) bool { return s.GetPart() == resourcev1.LinkPart_LINK_PART_SECURITY })
}

// checkLinkPrefix refuses a link to a connection where its app would keep its files
// where another link's app keeps theirs, made by an install, Connect storage or by
// hand alike: their apps would overwrite and delete each other's files. Where an app
// keeps its files is its link's prefix only where the link sets the name that tells
// the app it; otherwise it's the bucket's root (link.Kept, the review's H1). It looks
// only at a link that's new, or moves to another connection or prefix, or stops or
// starts telling its app the prefix, so a link from before keeps its place as it is.
// The refusal names no other project, as the person linking may not see it.
func (e *Engine) checkLinkPrefix(ctx context.Context, tx *store.Tx, items []item, it *item, l *resourcev1.Link) error {
	was, _ := it.current.(*resourcev1.Link)
	// A link from before links named the setting that tells their app the prefix,
	// which gains it now, as its install's upgrade gives it (the re-check's N1).
	gains := was != nil && was.GetConnection() == l.GetConnection() && !link.KeepsToPrefix(was) && link.KeepsToPrefix(l)
	if was != nil && was.GetConnection() == l.GetConnection() && was.GetPrefix() == l.GetPrefix() {
		// Unchanged, or only gaining or losing the record of the prefix: nothing
		// moves. Gaining it is an upgrade recording what its app kept to already;
		// losing it, as undoing that upgrade does, puts back a link that was accepted,
		// and only makes it read as the root from then on (#772's last check, C2).
		return nil
	}
	links, err := e.allLinks(ctx, tx, items)
	if err != nil {
		return err
	}
	if was, ok := it.current.(*resourcev1.Link); !ok || was.GetConnection() != l.GetConnection() {
		if err := e.checkOneApp(ctx, tx, items, it, l, links); err != nil {
			return err
		}
	}
	// A mail server keeps no files, so apps a shared one serves have nothing to keep
	// apart.
	if l.GetKind() == resourcev1.LinkKind_LINK_KIND_SMTP {
		return nil
	}
	// The suggestion comes from links in places the person may see alone, so it says
	// nothing of another project's prefixes (the slice 1 re-check, N3), and is offered
	// only where no project holds it, so taking it plans (the slice 3 review, L3).
	// Otherwise the refusal asks for a prefix of its own; it can't help saying the one
	// asked for is taken.
	caller, person := place.CallerFrom(ctx)
	var taken, seen []string
	for i := range links {
		other := &links[i]
		if other.ref == it.ref || other.GetConnection() != l.GetConnection() {
			continue
		}
		// Where a link from before takes its app's own prefix as it gains the record,
		// the links from before beside it are read by their prefixes, as they were
		// accepted then, so it's checked against them as any prefix was (#772's last
		// check, C1). Anything else reads a link that names no setting as the root.
		kept := link.Kept(other.Link)
		if gains && !link.KeepsToPrefix(other.Link) {
			kept = other.GetPrefix()
		}
		taken = append(taken, kept)
		if !person || place.Holds(caller.Grants, place.Place{Project: projectOfScope(ctx, tx, items, other.ref.Scope)}) != "" {
			seen = append(seen, kept)
		}
	}
	meets := func(p string) bool {
		return slices.ContainsFunc(taken, func(t string) bool { return link.PrefixesMeet(p, t) })
	}
	want := link.Kept(l)
	if !meets(want) {
		return nil
	}
	if !link.KeepsToPrefix(l) && l.GetPrefix() != "" {
		return fault.New(ErrInvalid, "link %s doesn't name the setting that tells its app the prefix %s, so nothing keeps its files apart from another app's on the connection %s. Name that setting on the link, or give it a connection of its own",
			it.ref.Name, l.GetPrefix(), l.GetConnection())
	}
	where := cmp.Or(want, "the bucket's root")
	if free := link.FreePrefix(want, seen); free != want && !meets(free) && !slices.Contains(taken, "") {
		return fault.New(ErrInvalid, "link %s keeps its files under %s on the connection %s, where another app keeps its files already, so each would overwrite the other's. Pick another prefix, such as %s",
			it.ref.Name, where, l.GetConnection(), free)
	}
	// Beside a link whose app keeps to the bucket's root, no prefix keeps them apart
	// (the slice 1 re-check, N2).
	if slices.Contains(taken, "") {
		return fault.New(ErrInvalid, "link %s keeps its files under %s on the connection %s, where another app's link doesn't say which prefix its app keeps to, so nothing keeps their files apart. Give it a connection of its own",
			it.ref.Name, where, l.GetConnection())
	}
	return fault.New(ErrInvalid, "link %s keeps its files under %s on the connection %s, where another app keeps its files already, so each would overwrite the other's. Pick a prefix of its own",
		it.ref.Name, where, l.GetConnection())
}

// checkOneApp refuses a link that brings another app to a connection that isn't
// shared: a connection serves one app unless an install admin shares it (ADR 0099).
// An app is a service (appsOf), so one app's links to it for two needs are one app. It's
// asked only of a link that's new or moves to the connection, so apps that share one
// from before keep working, and the same change may share it. The words name no
// other project, as the person linking may not see it.
func (e *Engine) checkOneApp(ctx context.Context, tx *store.Tx, items []item, it *item, l *resourcev1.Link, links []aLink) error {
	org, err := tx.Organization(ctx)
	if err != nil {
		return err
	}
	spec, err := e.specAfter(ctx, tx, items, resource.Ref{Kind: variable.ConnectionKind, Scope: org.ID, Name: l.GetConnection()})
	if err != nil {
		return err
	}
	if c, ok := spec.(*resourcev1.ConnectionSpec); !ok || c.GetShared() {
		return nil
	}
	mine, err := e.appsOf(ctx, tx, items, &aLink{Link: l, ref: it.ref})
	if err != nil {
		return err
	}
	others, err := e.appsOn(ctx, tx, items, links, l.GetConnection(), it.ref)
	if err != nil {
		return err
	}
	for app := range others {
		if !slices.ContainsFunc(mine, func(m string) bool { return sameApp(m, app) }) {
			return fault.New(ErrInvalid, "the connection %s serves another app already, and isn't shared. Give this app a connection of its own, or have an admin share %s",
				l.GetConnection(), l.GetConnection())
		}
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
	case ref.GetPart() == resourcev1.LinkPart_LINK_PART_KEY_PREFIX && !recordsPrefix(l, it.ref.Name):
		return fault.New(ErrInvalid, "%s %s reads the prefix of link %s, and the link doesn't name %s as the setting that tells its app the prefix. Name it on the link",
			it.ref.Kind, it.ref.Name, ref.GetLink(), it.ref.Name)
	}
	return e.checkConnectionReached(ctx, tx, items, it, ref.GetName())
}

// checkLinkUser refuses a link that names a database user another link names, so
// the user a link reads the password of is the one made for it, never another app's.
func (e *Engine) checkLinkUser(ctx context.Context, tx *store.Tx, items []item, it *item, l *resourcev1.Link) error {
	if l.GetUser() == "" || l.GetEngine() == resourcev1.LinkEngine_LINK_ENGINE_CLICKHOUSE {
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
				if l.ref.Scope != env && l.meets() == env && l.GetService() == server && l.GetEngine() != resourcev1.LinkEngine_LINK_ENGINE_CLICKHOUSE {
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
	if had, ok := it.current.(*resourcev1.Link); ok && had.GetMeetsEnvironment() == env && had.GetService() == l.GetService() && had.GetEngine() == l.GetEngine() {
		return nil
	}
	spec, err := e.specAfter(ctx, tx, items, resource.Ref{Kind: serviceKind, Scope: env, Name: l.GetService()})
	if err != nil {
		return err
	}
	svc, _ := spec.(*resourcev1.Service)
	found, ok := database.FromSpec(svc)
	if l.GetEngine() == resourcev1.LinkEngine_LINK_ENGINE_CLICKHOUSE || ok && found.Engine.ClickHouse() {
		if l.GetEngine() != resourcev1.LinkEngine_LINK_ENGINE_CLICKHOUSE || !ok || !found.Engine.ClickHouse() || found.PasswordSecret == "" {
			return fault.New(ErrInvalid, "link %s needs a ClickHouse service with a password kept in a secret to reach another environment", it.ref.Name)
		}
		return nil
	}
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
	if l.GetEngine() == resourcev1.LinkEngine_LINK_ENGINE_CLICKHOUSE {
		spec, err := e.specAfter(ctx, tx, items, resource.Ref{Kind: serviceKind, Scope: v.GetEnvironment(), Name: l.GetService()})
		if err != nil {
			return err
		}
		svc, _ := spec.(*resourcev1.Service)
		found, ok := database.FromSpec(svc)
		if !ok || !found.Engine.ClickHouse() || found.PasswordSecret != v.GetName() {
			return refuse(v.GetName() + " isn't the ClickHouse service's password")
		}
		return nil
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
	linked, err := e.load(ctx, tx, it.forLink)
	if err != nil {
		return err
	}
	l, ok := linked.current.(*resourcev1.Link)
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
