// SPDX-License-Identifier: AGPL-3.0-only

package revision

// Check 2 of docs/adr/0048-members-as-far-as-one-owner-goes.md: what the
// person behind a change may do with each of its items, wherever the change
// came from, as docs/adr/0049-roles-and-invites-by-project.md says.

import (
	"cmp"
	"context"
	"errors"
	"slices"
	"strings"

	resourcev1 "github.com/dockwell/dockwell/gen/dockwell/resource/v1alpha1"
	"github.com/dockwell/dockwell/internal/control/place"
	"github.com/dockwell/dockwell/internal/fault"
	"github.com/dockwell/dockwell/internal/resource"
	"github.com/dockwell/dockwell/internal/resource/container"
	"github.com/dockwell/dockwell/internal/scope"
	"github.com/dockwell/dockwell/internal/store"
)

var (
	// ErrNeedsRole is a change, or a step of one, that the role of whoever acts
	// doesn't cover where its items are.
	ErrNeedsRole = errors.New("your role doesn't cover this change")
	// ErrOutOfReach is an item in a place the person acting holds no role in. It
	// says what a missing one would, so it tells nothing of the place.
	ErrOutOfReach = errors.New("there's nothing by that ID or name here, or not in a project you hold. Check it, or ask an admin")
)

// RequireRoles has the engine refuse a person's or an API token's change whose
// context carries no caller (place.WithCaller), as the API's always does.
// control.New asks it of the engine it serves, so the control plane and its
// tests both have it. Without it, as in the engine's own tests, only a change
// with a caller is checked. Dockwell's own changes are never checked.
func (e *Engine) RequireRoles() { e.roles = true }

// act is what a check of roles is for.
type act int

const (
	planning act = iota
	applying
	discarding
)

// spot is where an item is, for a check of roles, and why it's an admin's, if
// it is whatever a project's grants say.
type spot struct {
	place.Place
	admins string
}

// checkRoles checks what the person acting may do with each item, by its place
// before and after the change, the stricter of the two: in reach at all, and
// with the role the act needs there. Planning needs a developer, as a change in
// a protected environment waits; applying needs an admin where an item is
// protected, where the plan reaches past a container or keeps such reach while
// it runs something else, and for what belongs to no project. Discarding
// someone else's plan needs what applying it would in its places, an admin
// where an item is protected or is an admin's to make; what it would reach on
// a server doesn't count, as discarding runs nothing there (ADR 0049). The
// proposer may always discard theirs. c is nil while planning.
func (e *Engine) checkRoles(ctx context.Context, tx *store.Tx, actor string, c *store.Change, items []item, a act) error {
	caller, ok := place.CallerFrom(ctx)
	person := aPerson(actor)
	switch {
	case !ok && person && e.roles:
		e.logger.ErrorContext(ctx, "a person's change reached the engine without who they are", "actor", actor)
		return fault.New(ErrNeedsRole, "can't check your role for this change, so it didn't happen. Try again, and report it if it keeps happening")
	case !ok:
		return nil // Dockwell's own, or the engine's own tests
	}
	own := false
	if a == discarding && c != nil {
		var err error
		if own, err = sameProposer(ctx, tx, caller, c.Actor); err != nil {
			return err
		}
	}
	// What each item reaches on its server that only an admin may give it, or
	// keeps while it runs something else.
	var plan *Plan
	if a == applying && c != nil {
		var err error
		if plan, err = DecodePlan(c.Plan); err != nil {
			return err
		}
	}
	byID := make(map[string]*item, len(items))
	for i := range items {
		byID[items[i].id] = &items[i]
	}
	for i := range items {
		it := &items[i]
		at, err := e.spotOf(ctx, tx, it, byID)
		if err != nil {
			return err
		}
		held := place.Holds(caller.Grants, at.Place)
		if it.follows {
			// What the change took in through a value it changes is Dockwell's own item:
			// it needs no role where it is, but its protection holds, so applying it there
			// waits for an admin or the owner (ADR 0084).
			if a != planning && !own && at.Protected && store.RoleRank(held) < store.RoleRank(store.RoleAdmin) {
				return &Refusal{act: a, Held: held, Needs: store.RoleAdmin, Why: "it restarts an app in a protected environment, which uses what this changes"}
			}
			continue
		}
		if held == "" && it.current == nil && at.admins != "" {
			// Something new that only an admin makes, such as a project: nothing
			// to find, so the role is what's missing.
			return &Refusal{act: a, Held: place.Holds(caller.Grants, place.Install), Needs: store.RoleAdmin, Why: at.admins}
		}
		if held == "" {
			return fault.New(ErrOutOfReach, "%s", ErrOutOfReach.Error())
		}
		if own {
			continue
		}
		// What it points at in another environment needs a role there too (ADR 0084).
		if err := e.reachRoles(ctx, tx, caller.Grants, it, byID, a); err != nil {
			return err
		}
		needs, why := store.RoleDeveloper, ""
		switch {
		case at.admins != "":
			needs, why = store.RoleAdmin, at.admins
		case a != planning && at.Protected:
			needs, why = store.RoleAdmin, "it changes "+it.ref.Name+" in a protected environment"
		case plan != nil && store.RoleRank(held) < store.RoleRank(store.RoleAdmin):
			reach, err := e.reachedOnServers(ctx, tx, plan, it, at)
			if err != nil {
				return err
			}
			if reach != "" {
				needs, why = store.RoleAdmin, reach
			}
		}
		if store.RoleRank(held) < store.RoleRank(needs) {
			return &Refusal{act: a, Held: held, Needs: needs, Why: why}
		}
	}
	return nil
}

// reachedOnServers says what of a plan's item, applied, reaches on its servers
// that only an admin may give it, whatever a project's grants say, or keeps
// while it runs something else; or "" for nothing. A volume Dockwell didn't
// make is an admin's too unless its environment mounts it already, as it may
// be another project's data, such as a taken-on stack's (ADR 0049).
func (e *Engine) reachedOnServers(ctx context.Context, tx *store.Tx, plan *Plan, it *item, at spot) (string, error) {
	for i := range plan.Resources {
		r := &plan.Resources[i]
		if r.Ref != it.ref {
			continue
		}
		for j := range r.HostAccess {
			if why := installWide(r.HostAccess[j]); why != "" {
				return it.ref.Name + " " + why + ", on a server every project there shares", nil
			}
		}
		for j := range r.Keeps {
			if r.Keeps[j].AsGoodAsRoot() {
				return it.ref.Name + " would run something else while it gets as much as root on its server", nil
			}
			if why := installWide(r.Keeps[j]); why != "" {
				return it.ref.Name + " would run another image, command or entrypoint while it " + why, nil
			}
		}
		for j := range r.HostAccess {
			a := r.HostAccess[j]
			if a.Kind != resource.AccessExternalVolume {
				continue
			}
			mounted, err := e.mountedIn(ctx, tx, at.Environment, it.ref, a.Path)
			if err != nil {
				return "", err
			}
			if !mounted {
				return it.ref.Name + " mounts the volume " + a.Path + ", which Dockwell didn't make and nothing in its environment mounts yet, so it may be another project's data", nil
			}
		}
	}
	return "", nil
}

// mountedIn reports a volume Dockwell didn't make, by name, that something
// other than self in an environment mounts as it stands now.
func (e *Engine) mountedIn(ctx context.Context, tx *store.Tx, env string, self resource.Ref, name string) (bool, error) {
	if env == "" {
		return false, nil
	}
	for _, kind := range e.kinds.Kinds() {
		t, err := e.kinds.Type(kind)
		if err != nil || t.Level() != resource.LevelEnvironment {
			continue
		}
		stored, err := tx.ListKind(ctx, string(kind))
		if err != nil {
			return false, err
		}
		for _, r := range stored {
			ref := resource.Ref{Kind: kind, Scope: r.Scope, Name: r.Name}
			if r.Scope != env || ref == self || r.Spec == nil {
				continue
			}
			spec, err := e.decode(ctx, tx, store.InRevision(r.ID, r.Kind, r.Revision), r.Spec)
			if err != nil {
				return false, err
			}
			access := e.kinds.HostAccess(kind, spec)
			for i := range access {
				if access[i].Kind == resource.AccessExternalVolume && access[i].Path == name {
					return true, nil
				}
			}
		}
	}
	return false, nil
}

// reachFirst checks, below an admin, that each edit is somewhere the person
// acting may plan, before anything else about the edits is checked: out of
// reach answers as missing does, a scope that isn't there answers the same,
// and so no check that follows describes another project, such as what an
// environment still holds or which route serves a domain (ADR 0049). An admin
// reaches every place, so their edits get the checks' own words. What it
// can't place yet, it leaves to checkRoles, once the change is planned.
func (e *Engine) reachFirst(ctx context.Context, tx *store.Tx, actor string, edits []Edit) error {
	caller, ok := place.CallerFrom(ctx)
	if !ok || store.RoleRank(place.Holds(caller.Grants, place.Install)) >= store.RoleRank(store.RoleAdmin) {
		return nil
	}
	items := make([]item, 0, len(edits))
	for i := range edits {
		it, err := e.load(ctx, tx, edits[i].Ref)
		if err != nil {
			return err
		}
		it.id = cmp.Or(it.id, edits[i].ID)
		it.spec = edits[i].Spec
		items = append(items, it)
	}
	err := e.checkRoles(ctx, tx, actor, nil, items, planning)
	if errors.Is(err, store.ErrNotFound) {
		return fault.New(ErrOutOfReach, "%s", ErrOutOfReach.Error())
	}
	return err
}

// capsHeld refuses a change that would take the install past the most it keeps
// of a kind (resource.Capped), counting what the change makes and deletes. A
// plan checks it, and the change's commit checks it again.
func (e *Engine) capsHeld(ctx context.Context, tx *store.Tx, items []item) error {
	more := map[resource.Kind]int{}
	for i := range items {
		switch it := &items[i]; {
		case it.current == nil && it.spec != nil:
			more[it.ref.Kind]++
		case it.current != nil && it.spec == nil:
			more[it.ref.Kind]--
		}
	}
	for kind, n := range more {
		if n <= 0 {
			continue
		}
		t, err := e.kinds.Type(kind)
		if err != nil {
			continue
		}
		c, ok := t.(resource.Capped)
		if !ok {
			continue
		}
		have, err := tx.ListKind(ctx, string(kind))
		if err != nil {
			return err
		}
		if len(have)+n > c.Cap() {
			return fault.New(ErrInvalid, "%s", c.CapWords(len(have)))
		}
	}
	return nil
}

// portsOutOfReach refuses, below an admin, a port on a server that something
// out of the person's reach publishes there, in words that name neither it nor
// its project, before the server's own check, which names what holds a port
// (ADR 0048). What's in reach the server's check words as it always has.
func (e *Engine) portsOutOfReach(ctx context.Context, tx *store.Tx, items []item) error {
	caller, ok := place.CallerFrom(ctx)
	if !ok || store.RoleRank(place.Holds(caller.Grants, place.Install)) >= store.RoleRank(store.RoleAdmin) {
		return nil
	}
	type wanted struct {
		it     *item
		server string
		port   *resourcev1.PortMapping
	}
	var wants []wanted
	inChange := make(map[resource.Ref]bool, len(items))
	for i := range items {
		inChange[items[i].ref] = true
		for _, server := range items[i].servers {
			for _, p := range runs(items[i].spec).GetPorts() {
				if p.GetHostPort() != 0 {
					wants = append(wants, wanted{it: &items[i], server: server, port: p})
				}
			}
		}
	}
	if len(wants) == 0 {
		return nil
	}
	for _, kind := range e.kinds.Kinds() {
		if _, ok := e.imageNamer(kind); !ok {
			continue
		}
		stored, err := tx.ListKind(ctx, string(kind))
		if err != nil {
			return err
		}
		for _, r := range stored {
			ref := resource.Ref{Kind: kind, Scope: r.Scope, Name: r.Name}
			if inChange[ref] || r.Spec == nil || inReach(ctx, tx, r.Scope) {
				continue
			}
			spec, err := e.decode(ctx, tx, store.InRevision(r.ID, r.Kind, r.Revision), r.Spec)
			if err != nil {
				return err
			}
			holder := runs(spec)
			for _, w := range wants {
				if !container.Publishes(holder, w.port) {
					continue
				}
				there, err := e.load(ctx, tx, ref)
				if err != nil {
					return err
				}
				if !slices.Contains(there.placed, w.server) {
					continue
				}
				words := container.PortWords(w.port)
				return fault.New(ErrInvalid, "%s%s on %s is in use by a project you can't see, so %s can't publish it. Choose another port on the server, or ask an admin",
					strings.ToUpper(words[:1]), words[1:], serverName(ctx, tx, w.server), w.it.ref.Name)
			}
		}
	}
	return nil
}

// serverName is a server's name, or its ID if it can't be read.
func serverName(ctx context.Context, tx *store.Tx, id string) string {
	if s, err := tx.GetServer(ctx, id); err == nil {
		return s.Name
	}
	return id
}

// installWide says what an item reaches that ADR 0048 makes an admin's,
// whatever someone's project grants say, or "" for what isn't: any server
// path, read-only or not; Docker's socket; the server's network; privileged
// mode, a capability or a group; a volume from a driver; a network Dockwell
// didn't make or one past the bridge; a proxy a route trusts; the panel's
// domain; a port on the server itself; a container Dockwell didn't make; and
// anything else as good as root. Claiming another environment's domain,
// serving plain HTTP to anyone, turning HTTPS off and a volume Dockwell didn't
// make stay a person's to approve, in a project they may apply in.
func installWide(a resource.Access) string {
	if a.AsGoodAsRoot() {
		return "gets as much as root on its server"
	}
	switch a.Kind {
	case resource.AccessPath:
		return "reaches the server path " + a.Path
	case resource.AccessMount:
		return "mounts what a volume driver reaches"
	case resource.AccessNetworkDriver, resource.AccessExternalNetwork:
		return "joins a network past the server's own bridge"
	case resource.AccessCapability, resource.AccessGroup, resource.AccessPrivileged:
		return "runs with more than Docker gives a container"
	case resource.AccessTrustedProxy:
		return "trusts a proxy for where a request came from"
	case resource.AccessPanelDomain:
		return "moves the panel's domain"
	case resource.AccessServerPort:
		return "sends to the port " + a.Path + " on the server itself"
	case resource.AccessTakesOn:
		return "takes on a container Dockwell didn't make"
	case resource.AccessDocker, resource.AccessNetwork, resource.AccessBackupOf,
		resource.AccessDomain, resource.AccessPlainForAnyone, resource.AccessHTTPSOff, resource.AccessExternalVolume,
		resource.AccessServerShell: // as good as root, which the first check holds
	}
	return ""
}

// CheckApply checks, before anything else is asked of them, that the caller
// in ctx may apply changeID, as applying it checks again: the API asks it
// before a passkey, so nobody confirms what their role refuses.
func (e *Engine) CheckApply(ctx context.Context, changeID string) error {
	return e.db.View(ctx, func(tx *store.Tx) error {
		c, err := tx.GetChange(ctx, changeID)
		if errors.Is(err, store.ErrNotFound) {
			return noSuchChange(changeID)
		}
		if err != nil || c.State != store.ChangePlanned {
			return err
		}
		items, err := e.placed(ctx, tx, c)
		if err != nil {
			return err
		}
		return e.checkRoles(ctx, tx, c.Actor, &c, items, applying)
	})
}

// Refusal is an act the person's role doesn't cover where an item is: the
// role it needs, the one they hold there, and why it needs that. It is
// ErrNeedsRole.
type Refusal struct {
	act         act
	Held, Needs string
	// Why says what makes it need that role, as a clause, such as "it changes
	// web in a protected environment". Empty when the role is all there is to it.
	Why string
}

func (r *Refusal) who() string {
	if r.Needs == store.RoleDeveloper {
		return "a developer or more"
	}
	return "an admin or the owner"
}

func (r *Refusal) Error() string {
	verb := map[act]string{planning: "plan this", applying: "apply this", discarding: "discard someone else's plan here"}[r.act]
	if r.Why != "" {
		return "only " + r.who() + " can " + verb + ", as " + r.Why + ", and " + youHold(r.Held) + ", so it didn't happen. Ask an admin or the owner"
	}
	return "only " + r.who() + " can " + verb + ", and " + youHold(r.Held) + ", so it didn't happen. Ask an admin to change your role"
}

// Is makes a Refusal ErrNeedsRole.
func (r *Refusal) Is(target error) bool { return target == ErrNeedsRole }

// Waits says why a plan the person can't apply waits, and for whom: what
// makes it theirs, then that they apply it, then what to do meanwhile.
func (r *Refusal) Waits() string {
	if r.Why == "" {
		return "Only " + r.who() + " can apply this, and " + youHold(r.Held) + ". Leave it planned for them, or ask an admin to change your role"
	}
	return "As " + r.Why + ", " + r.who() + " applies this. Leave it planned for them"
}

// youHold says what the person holds: "you're a developer", or none.
func youHold(role string) string {
	switch role {
	case "":
		return "you hold no role on the install"
	case store.RoleAdmin, store.RoleOwner:
		return "you're an " + role
	}
	return "you're a " + role
}

// spotOf is where an item is: an environment's own, the environment of what
// lives in one, read from the change's own items first, or the install's for
// what's on a server or in the organization. Its environment is protected when
// it is so before or after the change.
func (e *Engine) spotOf(ctx context.Context, tx *store.Tx, it *item, byID map[string]*item) (spot, error) {
	switch it.ref.Kind {
	case scope.ProjectKind:
		return spot{Place: place.Place{Project: it.id}, admins: "making, changing or deleting a project is an admin's"}, nil
	case scope.EnvironmentKind:
		return envSpot(it), nil
	}
	switch it.level {
	case resource.LevelEnvironment:
		if env, ok := byID[it.ref.Scope]; ok && env.ref.Kind == scope.EnvironmentKind {
			s := envSpot(env)
			s.admins = ""
			return s, nil
		}
		env, err := tx.GetResourceByID(ctx, it.ref.Scope)
		if err != nil {
			return spot{}, err
		}
		return spot{Place: place.Place{Project: env.Scope, Environment: env.ID, Protected: scope.ProtectedSpec(env.Name, env.Spec)}}, nil
	case resource.LevelServer:
		return spot{admins: "what runs on a server alone, outside a project, is an admin's"}, nil
	case resource.LevelProject, resource.LevelOrganization:
	}
	return spot{admins: "what belongs to no project is an admin's"}, nil
}

// envSpot is an environment item's own place, protected if it is before or
// after. Making one protected other than as its name has it, or changing
// whether it is, is an admin's (ADR 0049).
func envSpot(it *item) spot {
	before, _ := it.current.(*resourcev1.Environment)
	after, _ := it.spec.(*resourcev1.Environment)
	protected := before != nil && scope.Protected(it.ref.Name, before) || after != nil && scope.Protected(it.ref.Name, after)
	s := spot{Place: place.Place{Project: it.ref.Scope, Environment: it.id, Protected: protected}}
	// Unset, as on one from before protection was written, or brought back by an
	// undo, it reads as its name has it.
	byName := scope.DefaultProtection(it.ref.Name) == resourcev1.Protection_PROTECTION_PROTECTED
	switch {
	case after == nil:
	case before == nil && scope.Protected(it.ref.Name, after) != byName:
		s.admins = "making " + it.ref.Name + " " + protectionWords(it.ref.Name, after) + " is an admin's, as its name would have it otherwise"
	case before != nil && scope.Protected(it.ref.Name, before) != scope.Protected(it.ref.Name, after):
		s.admins = "making " + it.ref.Name + " " + protectionWords(it.ref.Name, after) + " is an admin's"
	}
	return s
}

func protectionWords(name string, e *resourcev1.Environment) string {
	if scope.Protected(name, e) {
		return "protected"
	}
	return "open"
}

// aPerson reports an actor that's a person, as themselves or with an API token.
func aPerson(actor string) bool {
	return strings.HasPrefix(actor, "user:") || strings.HasPrefix(actor, "token:")
}

// sameProposer reports a change proposed by the caller: as themselves, or with
// an API token they made.
func sameProposer(ctx context.Context, tx *store.Tx, caller place.Caller, actor string) (bool, error) {
	kind, actorID, _ := strings.Cut(actor, ":")
	switch kind {
	case "user":
		return actorID == caller.UserID, nil
	case "token":
		tok, err := tx.GetAPIToken(ctx, actorID)
		if errors.Is(err, store.ErrNotFound) {
			return false, nil
		}
		return err == nil && tok.UserID == caller.UserID, err
	}
	return false, nil
}

// placed are a change's items as far as checking roles needs: each as it is
// now, with what the change asks of an environment. Unlike current, it reads a
// plan that went stale, as discarding one has to work.
func (e *Engine) placed(ctx context.Context, tx *store.Tx, c store.Change) ([]item, error) {
	items := make([]item, 0, len(c.Items))
	for i := range c.Items {
		ci := &c.Items[i]
		ref := resource.Ref{Kind: resource.Kind(ci.Kind), Scope: ci.Scope, Name: ci.Name}
		it, err := e.load(ctx, tx, ref)
		if err != nil {
			return nil, err
		}
		if it.id == "" {
			it.id = ci.ResourceID
		}
		it.follows = ci.Follows
		if ref.Kind == scope.EnvironmentKind && ci.Spec != nil {
			// An environment's spec holds nothing sealed. One that can't be read
			// counts as protected after.
			if it.spec, err = e.kinds.Decode(ref.Kind, ci.Spec); err != nil {
				it.spec = &resourcev1.Environment{Protection: resourcev1.Protection_PROTECTION_PROTECTED}
			}
		}
		items = append(items, it)
	}
	return items, nil
}

// protectedChange reports a change with an item in a protected environment,
// before or after, for the API's approval of someone else's change.
func (e *Engine) protectedChange(ctx context.Context, tx *store.Tx, c store.Change) (bool, error) {
	items, err := e.placed(ctx, tx, c)
	if err != nil {
		return false, err
	}
	byID := make(map[string]*item, len(items))
	for i := range items {
		byID[items[i].id] = &items[i]
	}
	for i := range items {
		at, err := e.spotOf(ctx, tx, &items[i], byID)
		if err != nil {
			return false, err
		}
		if at.Protected {
			return true, nil
		}
	}
	return false, nil
}

// Approval is what applying a change needs beyond a role: whether it's
// someone else's with an item in a protected environment, which an admin or
// the owner approves in a session, with the plan's fingerprint and a passkey
// (ADR 0049).
type Approval struct {
	// Protected says an item is in a protected environment, before or after.
	Protected bool
	// Theirs says another person proposed it.
	Theirs bool
}

// ApprovalOf says what applying changeID needs of the caller in ctx beyond a
// role. A context with no caller is Dockwell's own, which needs nothing.
func (e *Engine) ApprovalOf(ctx context.Context, changeID string) (Approval, error) {
	var out Approval
	err := e.db.View(ctx, func(tx *store.Tx) error {
		c, err := tx.GetChange(ctx, changeID)
		if errors.Is(err, store.ErrNotFound) {
			return noSuchChange(changeID)
		}
		if err != nil {
			return err
		}
		out, err = e.approvalIn(ctx, tx, c)
		return err
	})
	return out, err
}

func (e *Engine) approvalIn(ctx context.Context, tx *store.Tx, c store.Change) (Approval, error) {
	caller, ok := place.CallerFrom(ctx)
	if !ok || c.State != store.ChangePlanned {
		return Approval{}, nil
	}
	mine, err := sameProposer(ctx, tx, caller, c.Actor)
	if err != nil {
		return Approval{}, err
	}
	// A plan Dockwell made has no other person to approve it for.
	out := Approval{Theirs: !mine && aPerson(c.Actor)}
	out.Protected, err = e.protectedChange(ctx, tx, c)
	return out, err
}

// Applying is what applying a planned change asks of the person reading it.
type Applying struct {
	// Approves says applying it approves someone else's change in a protected
	// environment, which takes a session, the plan's fingerprint and a passkey.
	Approves bool
	// Waits says why it waits for an admin or the owner, for someone below an
	// admin whose role covers the rest of it (Refusal.Waits), or "".
	Waits string
}

// ApplyingOf says, within tx, what applying changeID asks of the caller in ctx,
// as ApplyChange would ask it, so a plan can say so before anyone tries. A
// context with no caller, or a change that isn't planned, asks nothing.
func (e *Engine) ApplyingOf(ctx context.Context, tx *store.Tx, changeID string) (Applying, error) {
	if _, ok := place.CallerFrom(ctx); !ok {
		return Applying{}, nil
	}
	// Read whole: a list's changes come without their items and plan.
	c, err := tx.GetChange(ctx, changeID)
	if err != nil || c.State != store.ChangePlanned {
		return Applying{}, err
	}
	items, err := e.placed(ctx, tx, c)
	if err != nil {
		return Applying{}, err
	}
	var refused *Refusal
	switch err := e.checkRoles(ctx, tx, c.Actor, &c, items, applying); {
	case errors.As(err, &refused) && refused.Needs == store.RoleAdmin:
		return Applying{Waits: refused.Waits()}, nil
	case errors.As(err, &refused):
		return Applying{}, nil // a viewer applies nothing: no one's waiting on them
	case errors.Is(err, ErrOutOfReach):
		return Applying{}, nil // not theirs to read, so a list doesn't show it
	case err != nil:
		return Applying{}, err
	}
	approval, err := e.approvalIn(ctx, tx, c)
	return Applying{Approves: approval.Protected && approval.Theirs}, err
}
