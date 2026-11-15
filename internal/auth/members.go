// SPDX-License-Identifier: AGPL-3.0-only

package auth

// Managing members: changing what someone holds, removing them, transferring
// ownership, and the protections confirmed before anyone else signs in
// (docs/adr/0049-roles-and-invites-by-project.md, "Managing members"). Each is
// access, not managed state, so it's audited, not a revision.

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"slices"
	"time"

	"github.com/dockwell/dockwell/internal/fault"
	"github.com/dockwell/dockwell/internal/oidc"
	"github.com/dockwell/dockwell/internal/store"
)

// Member errors.
var (
	// ErrOnYourself is changing or removing yourself: someone else does that.
	ErrOnYourself = errors.New("on yourself")
	// ErrTheOwner is changing or removing the owner, whose role moves only by a transfer.
	ErrTheOwner = errors.New("the owner")
	// ErrOwnerOnly is a transfer of ownership by anyone but the owner.
	ErrOwnerOnly = errors.New("only the owner transfers ownership, and you aren't the owner. Ask them")
	// ErrAdminOnly is managing members by someone who isn't an admin or the owner.
	ErrAdminOnly = errors.New("only an admin or the owner manages who signs in here, and you're neither. Ask one of them")
	// ErrLastGrant is taking back the last thing someone holds, which is removing them.
	ErrLastGrant = errors.New("last grant")
	// ErrProtectionsUnconfirmed is a first invite before anyone confirmed which
	// environments are protected.
	ErrProtectionsUnconfirmed = errors.New("nobody has confirmed which environments are protected yet, and that decides whose changes wait once someone else signs in. Look at each in Members, in the panel, and confirm them there, then invite")
)

// roleNone names taking back what someone holds in a place, in a step-up's target.
const roleNone = "none"

// protectionsKey is the setting that records the protections were confirmed.
const protectionsKey = "members.protections_confirmed"

// RoleTarget is what a role change's step-up confirms: the person, the role
// and the place, apart by spaces, the role being "none" to take it back and
// the place "install" for the whole install.
func RoleTarget(userID, role, projectID string) string {
	if role == "" {
		role = roleNone
	}
	place := projectID
	if place == store.Install {
		place = "install"
	}
	return userID + " " + role + " " + place
}

// RoleChange is what changing someone's role did.
type RoleChange struct {
	User store.User
	// Previous is what they held in that place before, or "".
	Previous string
	// Lowered says the change took something from them, which ended their
	// sessions and tokens, and their invites if they're no longer an admin.
	Lowered bool
	Ended   store.Ended
	Invites int
}

// manager checks p may manage members: an admin or the owner, as their grants
// stand in tx. The API's gate checks it first; this holds whatever calls.
func (s *Service) manager(ctx context.Context, tx *store.Tx, p Principal) error {
	grants, err := tx.Grants(ctx, p.User.ID)
	if err != nil {
		return err
	}
	for _, g := range grants {
		if g.ProjectID == store.Install && store.RoleRank(g.Role) >= store.RoleRank(store.RoleAdmin) {
			return nil
		}
	}
	return ErrAdminOnly
}

// managed reads the person a call manages, and refuses yourself, the owner,
// nobody and someone removed already.
func (s *Service) managed(ctx context.Context, tx *store.Tx, p Principal, userID, doing string) (store.User, error) {
	if userID == p.User.ID {
		return store.User{}, fault.New(ErrOnYourself, "nobody is %s by themselves, so a slip can't take your own access. Ask another admin or the owner", doing)
	}
	u, err := tx.GetUser(ctx, userID)
	if errors.Is(err, store.ErrNotFound) || err == nil && u.RemovedAt != nil {
		return store.User{}, fmt.Errorf("%w: nobody signs in here as %s. Read Members again", ErrNotFound, userID)
	}
	if err != nil {
		return store.User{}, err
	}
	if u.Role == store.RoleOwner {
		return store.User{}, fault.New(ErrTheOwner, "%s is the owner, who isn't %s: ownership moves by a transfer, which only the owner makes", u.Name, doing)
	}
	return u, nil
}

// roleAt is the role grants give in exactly one place, or "".
func roleAt(grants []store.Grant, projectID string) string {
	for _, g := range grants {
		if g.ProjectID == projectID {
			return g.Role
		}
	}
	return ""
}

// revokeInvitesBy revokes the open invites a person made, each audited, and
// says how many: nobody joins on the word of someone who no longer invites.
func (s *Service) revokeInvitesBy(ctx context.Context, tx *store.Tx, client Client, p Principal, userID string, now time.Time) (int, error) {
	open, err := tx.ListOpenInvites(ctx, now)
	if err != nil {
		return 0, err
	}
	n := 0
	for i := range open {
		if open[i].CreatedBy != "user:"+userID {
			continue
		}
		if err := tx.RevokeInvite(ctx, open[i].ID, p.Actor(), now); err != nil {
			return 0, err
		}
		if err := s.auditFrom(ctx, tx, p.Actor(), client.Surface(p), "member.invite_revoke", open[i].ID,
			memberDetail{Email: open[i].Email, Role: open[i].Role, Project: open[i].ProjectID}.String()); err != nil {
			return 0, err
		}
		n++
	}
	return n, nil
}

// SetMemberRole gives a person role in the place projectID names, an existing
// project the caller checked, or store.Install, in place of what they held
// there; an empty role takes it back. It takes a step-up bound to the person,
// the role and the place, spent in the write once everything else holds, so a
// refusal leaves the proof for another try. Lowering what they hold ends their
// sessions and revokes their tokens, and their open invites when they're no
// longer an admin. Any change ends their pages' streams once it commits, and a
// lowering every stream they have.
func (s *Service) SetMemberRole(ctx context.Context, client Client, p Principal, userID, role, projectID, proof string) (RoleChange, error) {
	switch role {
	case store.RoleOwner:
		return RoleChange{}, fmt.Errorf("%w: nobody is made the owner by a role change: ownership moves by a transfer, which only the owner makes", ErrInvalid)
	case store.RoleAdmin, store.RoleDeveloper, store.RoleViewer, "":
	default:
		return RoleChange{}, fmt.Errorf("%w: a role is admin, developer or viewer", ErrInvalid)
	}
	if role == store.RoleAdmin && projectID != store.Install {
		return RoleChange{}, fmt.Errorf("%w: an admin holds the whole install, so make them one there rather than in a project", ErrInvalid)
	}
	now := s.now()
	var out RoleChange
	err := s.db.Update(ctx, func(tx *store.Tx) error {
		if err := s.manager(ctx, tx, p); err != nil {
			return err
		}
		u, err := s.managed(ctx, tx, p, userID, "given another role")
		if err != nil {
			return err
		}
		before, err := tx.Grants(ctx, userID)
		if err != nil {
			return err
		}
		wasAdmin := store.RoleRank(roleAt(before, store.Install)) >= store.RoleRank(store.RoleAdmin)
		// An admin holds the whole install, which covers every project; a project's
		// role beside it would outlast a later lowering nobody chose it for.
		if wasAdmin && projectID != store.Install {
			return fmt.Errorf("%w: %s is an admin, who holds every project already. Change what they hold on the whole install instead", ErrInvalid, u.Name)
		}
		if role == "" {
			if roleAt(before, projectID) == "" {
				return fmt.Errorf("%w: %s holds nothing there to take back. Read Members again", ErrNotFound, u.Name)
			}
			if len(before) == 1 {
				return fault.New(ErrLastGrant, "that's the last thing %s holds, and someone who holds nothing can only sign out. Remove them instead", u.Name)
			}
		}
		if err := s.UseStepUpIn(ctx, tx, client, p, proof, StepUpChangeRole, RoleTarget(userID, role, projectID)); err != nil {
			return err
		}
		out = RoleChange{User: u, Previous: roleAt(before, projectID)}
		switch role {
		case "":
			if err := tx.RemoveGrant(ctx, userID, projectID); err != nil {
				return err
			}
		case store.RoleAdmin:
			// An admin holds the install, which covers every project they held.
			for _, g := range before {
				if g.ProjectID != store.Install {
					if err := tx.RemoveGrant(ctx, userID, g.ProjectID); err != nil {
						return err
					}
				}
			}
			fallthrough
		default:
			if err := tx.SetGrant(ctx, store.Grant{UserID: userID, ProjectID: projectID, Role: role, CreatedAt: now, CreatedBy: p.Actor()}); err != nil {
				return err
			}
		}
		out.Lowered = store.RoleRank(role) < store.RoleRank(out.Previous)
		if out.Lowered {
			if out.Ended, err = tx.EndAccess(ctx, userID, now); err != nil {
				return err
			}
			if err := s.auditHooksEnded(ctx, tx, p, userID, out.Ended.Hooks, "its maker's role was lowered"); err != nil {
				return err
			}
			if wasAdmin && role != store.RoleAdmin {
				if out.Invites, err = s.revokeInvitesBy(ctx, tx, client, p, userID, now); err != nil {
					return err
				}
			}
		}
		detail := memberDetail{
			Email: u.Email, Name: u.Name, Role: role, Project: projectID, Previous: out.Previous,
			Sessions: out.Ended.Sessions, Tokens: out.Ended.Tokens, Invites: out.Invites,
		}
		return s.auditFrom(ctx, tx, p.Actor(), client.Surface(p), "member.role", userID, detail.String())
	})
	if err != nil {
		return RoleChange{}, err
	}
	// Their streams were narrowed to what they held, so none outlives the change,
	// whichever way it went; what they left running goes on unless it took something.
	s.endStreamsOf(out.Lowered, userID)
	s.logger.InfoContext(ctx, "someone's role changed", slog.String("user", userID), slog.String("role", role), slog.String("was", out.Previous), slog.String("by", p.Actor()))
	return out, nil
}

// RemovalPlan is what removing someone would end, and once they're removed,
// what it did end.
type RemovalPlan struct {
	User     store.User
	Grants   []store.Grant
	Sessions int
	Tokens   []store.APIToken
	Invites  []store.Invite
	// Changes are what they planned and didn't apply, themselves or with any
	// token they made, whatever its state now.
	Changes []store.Change
	// Links are their accounts at sign-in providers, which are deleted, with the
	// provider's name where it could be read before the transaction (ADR 0095).
	Links []LinkView
	// Hooks are the deploy-on-push hooks they made, which are deleted (ADR 0046).
	Hooks []store.GitHook
	// Ended and InvitesRevoked are what the removal's transaction ended, as its
	// audit event says, set once they're removed.
	Ended          store.Ended
	InvitesRevoked int
}

// plan reads what removing userID would end, as things stand in tx.
func (s *Service) plan(ctx context.Context, tx *store.Tx, p Principal, userID string, now time.Time) (RemovalPlan, error) {
	if err := s.manager(ctx, tx, p); err != nil {
		return RemovalPlan{}, err
	}
	u, err := s.managed(ctx, tx, p, userID, "removed")
	if err != nil {
		return RemovalPlan{}, err
	}
	out := RemovalPlan{User: u}
	if out.Grants, err = tx.Grants(ctx, userID); err != nil {
		return RemovalPlan{}, err
	}
	sessions, err := tx.ListActiveSessions(ctx, userID, now)
	if err != nil {
		return RemovalPlan{}, err
	}
	out.Sessions = len(sessions)
	links, err := tx.ListProviderLinks(ctx, userID)
	if err != nil {
		return RemovalPlan{}, err
	}
	for i := range links {
		out.Links = append(out.Links, LinkView{ProviderLink: links[i]})
	}
	if out.Tokens, err = tx.ListLiveAPITokens(ctx, userID, now); err != nil {
		return RemovalPlan{}, err
	}
	open, err := tx.ListOpenInvites(ctx, now)
	if err != nil {
		return RemovalPlan{}, err
	}
	for i := range open {
		if open[i].CreatedBy == "user:"+userID {
			out.Invites = append(out.Invites, open[i])
		}
	}
	if out.Changes, err = tx.PlannedChangesOf(ctx, userID); err != nil {
		return RemovalPlan{}, err
	}
	if out.Hooks, err = tx.GitHooksOf(ctx, userID); err != nil {
		return RemovalPlan{}, err
	}
	return out, nil
}

// nameLinks names each of a plan's links by its provider, from providers read
// before the plan's transaction, as nothing reads the providers inside one
// (#646's review, H1); a provider that's gone is named by its issuer's host.
func nameLinks(links []LinkView, providers []Provider) {
	for i := range links {
		l := &links[i]
		if j := slices.IndexFunc(providers, func(pr Provider) bool { return pr.ID == l.ProviderID }); j >= 0 {
			l.Provider = providers[j].Label()
		} else {
			l.Provider = oidc.HostOf(l.Issuer)
		}
	}
}

// providersToName reads the providers for naming links, and does without them
// where they can't be read: the names are for reading.
func (s *Service) providersToName(ctx context.Context) []Provider {
	providers, err := s.allProviders(ctx, false)
	if err != nil {
		s.logger.WarnContext(ctx, "can't read the sign-in providers to name someone's links", slog.Any("error", err))
	}
	return providers
}

// PlanMemberRemoval reads what removing someone would end, for whoever
// removes them to see first. It changes nothing.
func (s *Service) PlanMemberRemoval(ctx context.Context, p Principal, userID string) (RemovalPlan, error) {
	providers := s.providersToName(ctx)
	var out RemovalPlan
	err := s.db.View(ctx, func(tx *store.Tx) (err error) {
		out, err = s.plan(ctx, tx, p, userID, s.now())
		return err
	})
	nameLinks(out.Links, providers)
	return out, err
}

// RemoveMember removes a person, with a step-up bound to them, spent in the
// write once everything else holds: in one transaction their sign-in goes, with
// their links at sign-in providers, each audited, their sessions end, their API
// tokens and the open invites they made are revoked, and they hold nothing.
// Their open streams end once it commits. It returns what went, the changes
// they'd planned among it, for the caller to discard: the engine owns changes,
// and a change left planned is checked against whoever applies it, never
// against who planned it.
func (s *Service) RemoveMember(ctx context.Context, client Client, p Principal, userID, proof string) (RemovalPlan, error) {
	providers := s.providersToName(ctx)
	now := s.now()
	var out RemovalPlan
	err := s.db.Update(ctx, func(tx *store.Tx) error {
		var err error
		if out, err = s.plan(ctx, tx, p, userID, now); err != nil {
			return err
		}
		if err := s.UseStepUpIn(ctx, tx, client, p, proof, StepUpRemoveMember, userID); err != nil {
			return err
		}
		if out.InvitesRevoked, err = s.revokeInvitesBy(ctx, tx, client, p, userID, now); err != nil {
			return err
		}
		// Each link deleted is on record as an unlink is (ADR 0095).
		nameLinks(out.Links, providers)
		for i := range out.Links {
			l := &out.Links[i]
			if err := s.audit(ctx, tx, p.Actor(), "provider_link.delete", userID,
				fmt.Sprintf("%s at %s, link %s, with its person removed", l.Account, l.Provider, l.ID)); err != nil {
				return err
			}
		}
		out.Ended, err = tx.RemoveUser(ctx, userID, p.Actor(), now)
		if errors.Is(err, store.ErrOwner) {
			return fault.New(ErrTheOwner, "%s is the owner, who isn't removed: ownership moves by a transfer first", out.User.Name)
		}
		if err != nil {
			return err
		}
		if err := s.auditHooksEnded(ctx, tx, p, userID, out.Ended.Hooks, "its maker was removed"); err != nil {
			return err
		}
		detail := memberDetail{
			Email: out.User.Email, Name: out.User.Name, Role: Highest(out.Grants),
			Sessions: out.Ended.Sessions, Tokens: out.Ended.Tokens, Invites: out.InvitesRevoked, Changes: len(out.Changes), Links: out.Ended.Links,
		}
		return s.auditFrom(ctx, tx, p.Actor(), client.Surface(p), "member.remove", userID, detail.String())
	})
	if err != nil {
		return RemovalPlan{}, err
	}
	s.endStreamsOf(true, userID)
	s.logger.InfoContext(ctx, "someone was removed", slog.String("user", userID), slog.String("email", out.User.Email), slog.String("by", p.Actor()))
	return out, nil
}

// TransferOwnership makes someone who signs in here already the owner, with a
// step-up bound to them, which only the owner makes, spent in the write once
// everything else holds. In one transaction the new owner gets the grant, the
// old one becomes an admin, and the CLI's local token that acted for the old
// owner is revoked; or nothing changes.
func (s *Service) TransferOwnership(ctx context.Context, client Client, p Principal, toUserID, proof string) (store.User, error) {
	now := s.now()
	var to store.User
	err := s.db.Update(ctx, func(tx *store.Tx) error {
		owner, err := tx.Owner(ctx)
		if err != nil && !errors.Is(err, store.ErrNotFound) {
			return err
		}
		if owner.ID != p.User.ID {
			return ErrOwnerOnly
		}
		if toUserID == p.User.ID {
			return fmt.Errorf("%w: you're the owner already. Choose someone else who signs in here", ErrInvalid)
		}
		to, err = tx.GetUser(ctx, toUserID)
		if errors.Is(err, store.ErrNotFound) || err == nil && to.RemovedAt != nil {
			return fmt.Errorf("%w: nobody signs in here as %s, and the owner is someone who does. Read Members again", ErrNotFound, toUserID)
		}
		if err != nil {
			return err
		}
		if err := s.UseStepUpIn(ctx, tx, client, p, proof, StepUpTransferOwnership, toUserID); err != nil {
			return err
		}
		if err := tx.RevokeLocalTokens(ctx, p.User.ID, now); err != nil {
			return err
		}
		// The owner holds the install, which covers every project they held.
		held, err := tx.Grants(ctx, toUserID)
		if err != nil {
			return err
		}
		for _, g := range held {
			if g.ProjectID != store.Install {
				if err := tx.RemoveGrant(ctx, toUserID, g.ProjectID); err != nil {
					return err
				}
			}
		}
		if err := tx.TransferOwnership(ctx, toUserID, now, p.Actor()); err != nil {
			return err
		}
		detail := "made " + to.Email + " the owner, in place of " + p.User.Email + ", who is an admin now"
		return s.auditFrom(ctx, tx, p.Actor(), client.Surface(p), "owner.transfer", toUserID, detail)
	})
	if err != nil {
		return store.User{}, err
	}
	// Both hold something else now: the new owner the whole install, and the old
	// one an admin's role, which still covers what either left running.
	s.endStreamsOf(false, toUserID, p.User.ID)
	s.logger.InfoContext(ctx, "ownership was transferred", slog.String("to", toUserID), slog.String("from", p.User.ID))
	to.Role = store.RoleOwner
	return to, nil
}

// Confirmed is the record that someone confirmed which environments are protected.
type Confirmed struct {
	At time.Time `json:"at"`
	By string    `json:"by"`
}

// ProtectionsConfirmed reads the record that the protections were confirmed, if there is one.
func (s *Service) ProtectionsConfirmed(ctx context.Context, tx *store.Tx) (Confirmed, bool, error) {
	raw, err := tx.GetSetting(ctx, protectionsKey)
	if errors.Is(err, store.ErrNotFound) || err == nil && len(raw) == 0 {
		return Confirmed{}, false, nil
	}
	if err != nil {
		return Confirmed{}, false, err
	}
	var c Confirmed
	if err := json.Unmarshal(raw, &c); err != nil {
		return Confirmed{}, false, fmt.Errorf("can't read the record that the protections were confirmed: %w", err)
	}
	return c, true, nil
}

// protectionsFirst refuses a first invite until the protections were
// confirmed: while one person signs in, nobody else is asked in before someone
// looked at whose changes will wait.
func (s *Service) protectionsFirst(ctx context.Context, tx *store.Tx) error {
	n, err := tx.CountUsers(ctx)
	if err != nil || n > 1 {
		return err
	}
	if _, ok, err := s.ProtectionsConfirmed(ctx, tx); err != nil || ok {
		return err
	}
	return ErrProtectionsUnconfirmed
}

// ConfirmProtections records that p looked at which environments are protected,
// with how each stood when it's recorded, as standing reads them in the same
// transaction, in the audit log. It's recorded once and kept: confirming again
// changes nothing and returns the record there is (ADR 0049).
func (s *Service) ConfirmProtections(ctx context.Context, client Client, p Principal, standing func(*store.Tx) (string, error)) (Confirmed, error) {
	var c Confirmed
	err := s.db.Update(ctx, func(tx *store.Tx) error {
		if err := s.manager(ctx, tx, p); err != nil {
			return err
		}
		kept, ok, err := s.ProtectionsConfirmed(ctx, tx)
		if err != nil || ok {
			c = kept
			return err
		}
		stood, err := standing(tx)
		if err != nil {
			return err
		}
		c = Confirmed{At: s.now(), By: p.Actor()}
		raw, err := json.Marshal(c)
		if err != nil {
			return err
		}
		if err := tx.PutSetting(ctx, protectionsKey, raw); err != nil {
			return err
		}
		return s.auditFrom(ctx, tx, p.Actor(), client.Surface(p), "member.protections_confirm", "", stood)
	})
	return c, err
}
