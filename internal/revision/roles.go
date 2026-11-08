// SPDX-License-Identifier: AGPL-3.0-only

package revision

// Check 2 of docs/adr/0048-members-as-far-as-one-owner-goes.md: what the
// person behind a change may do with each of its items, wherever the change
// came from, as docs/adr/0049-roles-and-invites-by-project.md says.

import (
	"context"
	"errors"
	"strings"

	resourcev1 "github.com/dockwell/dockwell/gen/dockwell/resource/v1alpha1"
	"github.com/dockwell/dockwell/internal/control/place"
	"github.com/dockwell/dockwell/internal/fault"
	"github.com/dockwell/dockwell/internal/resource"
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
// context carries no caller (place.WithCaller), as the API's always does. Without
// it, as in the engine's own tests, only a change with a caller is checked.
// Dockwell's own changes are never checked.
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
// protected, where the plan reaches past a container, and for what belongs to
// no project. Discarding someone else's plan needs what applying would; the
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
	// What each item reaches on its server that only an admin may give it.
	install := map[resource.Ref]string{}
	if a == applying && c != nil {
		plan, err := DecodePlan(c.Plan)
		if err != nil {
			return err
		}
		for i := range plan.Resources {
			for j := range plan.Resources[i].HostAccess {
				if why := installWide(plan.Resources[i].HostAccess[j]); why != "" {
					install[plan.Resources[i].Ref] = why
					break
				}
			}
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
		if held == "" && it.current == nil && at.admins != "" {
			// Something new that only an admin makes, such as a project: nothing
			// to find, so the role is what's missing.
			return refusal(a, place.Holds(caller.Grants, place.Install), store.RoleAdmin, at.admins)
		}
		if held == "" {
			return fault.New(ErrOutOfReach, "%s", ErrOutOfReach.Error())
		}
		if own {
			continue
		}
		needs, why := store.RoleDeveloper, ""
		switch {
		case at.admins != "":
			needs, why = store.RoleAdmin, at.admins
		case a != planning && at.Protected:
			needs, why = store.RoleAdmin, "it changes "+it.ref.Name+" in a protected environment, which waits for an admin or the owner to apply it"
		case install[it.ref] != "":
			needs, why = store.RoleAdmin, it.ref.Name+" "+install[it.ref]+", on a server every project there shares"
		}
		if store.RoleRank(held) < store.RoleRank(needs) {
			return refusal(a, held, needs, why)
		}
	}
	return nil
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
		resource.AccessDomain, resource.AccessPlainForAnyone, resource.AccessHTTPSOff, resource.AccessExternalVolume:
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

// refusal says what the act needs, what the person holds, and why.
func refusal(a act, held, needs, why string) error {
	verb := map[act]string{planning: "plan this", applying: "apply this", discarding: "discard someone else's plan here"}[a]
	who := "an admin or the owner"
	if needs == store.RoleDeveloper {
		who = "a developer or more"
	}
	if why != "" {
		return fault.New(ErrNeedsRole, "only %s can %s, as %s, and %s, so it didn't happen. Ask an admin or the owner", who, verb, why, youHold(held))
	}
	return fault.New(ErrNeedsRole, "only %s can %s, and %s, so it didn't happen. Ask an admin to change your role", who, verb, youHold(held))
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
	switch {
	case after == nil:
	case before == nil && after.GetProtection() != scope.DefaultProtection(it.ref.Name):
		s.admins = "making " + it.ref.Name + " " + protectionWords(after) + " is an admin's, as its name would have it otherwise"
	case before != nil && scope.Protected(it.ref.Name, before) != scope.Protected(it.ref.Name, after):
		s.admins = "making " + it.ref.Name + " " + protectionWords(after) + " is an admin's"
	}
	return s
}

func protectionWords(e *resourcev1.Environment) string {
	if e.GetProtection() == resourcev1.Protection_PROTECTION_PROTECTED {
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
	caller, ok := place.CallerFrom(ctx)
	if !ok {
		return Approval{}, nil
	}
	var out Approval
	err := e.db.View(ctx, func(tx *store.Tx) error {
		c, err := tx.GetChange(ctx, changeID)
		if errors.Is(err, store.ErrNotFound) {
			return noSuchChange(changeID)
		}
		if err != nil || c.State != store.ChangePlanned {
			return err
		}
		mine, err := sameProposer(ctx, tx, caller, c.Actor)
		if err != nil {
			return err
		}
		// A plan Dockwell made has no other person to approve it for.
		out.Theirs = !mine && aPerson(c.Actor)
		out.Protected, err = e.protectedChange(ctx, tx, c)
		return err
	})
	return out, err
}
