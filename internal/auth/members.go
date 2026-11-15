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
	"time"

	"github.com/dockwell/dockwell/internal/fault"
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
// the role and the place. Lowering what they hold ends their sessions and
// revokes their tokens, and their open invites when they're no longer an admin.
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
	// What can be refused without the passkey is refused before its proof is spent.
	check := func(tx *store.Tx) (store.User, []store.Grant, error) {
		if err := s.manager(ctx, tx, p); err != nil {
			return store.User{}, nil, err
		}
		u, err := s.managed(ctx, tx, p, userID, "given another role")
		if err != nil {
			return store.User{}, nil, err
		}
		grants, err := tx.Grants(ctx, userID)
		if err != nil {
			return store.User{}, nil, err
		}
		if role == "" {
			if roleAt(grants, projectID) == "" {
				return store.User{}, nil, fmt.Errorf("%w: %s holds nothing there to take back. Read Members again", ErrNotFound, u.Name)
			}
			if len(grants) == 1 {
				return store.User{}, nil, fault.New(ErrLastGrant, "that's the last thing %s holds, and someone who holds nothing can only sign out. Remove them instead", u.Name)
			}
		}
		return u, grants, nil
	}
	err := s.db.View(ctx, func(tx *store.Tx) error {
		_, _, err := check(tx)
		return err
	})
	if err != nil {
		return RoleChange{}, err
	}
	if err := s.UseStepUp(ctx, client, p, proof, StepUpChangeRole, RoleTarget(userID, role, projectID)); err != nil {
		return RoleChange{}, err
	}
	now := s.now()
	var out RoleChange
	err = s.db.Update(ctx, func(tx *store.Tx) error {
		u, before, err := check(tx)
		if err != nil {
			return err
		}
		out = RoleChange{User: u, Previous: roleAt(before, projectID)}
		wasAdmin := store.RoleRank(roleAt(before, store.Install)) >= store.RoleRank(store.RoleAdmin)
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
	s.logger.InfoContext(ctx, "someone's role changed", slog.String("user", userID), slog.String("role", role), slog.String("was", out.Previous), slog.String("by", p.Actor()))
	return out, nil
}

// RemovalPlan is what removing someone would end.
type RemovalPlan struct {
	User     store.User
	Grants   []store.Grant
	Sessions int
	Tokens   []store.APIToken
	Invites  []store.Invite
	// Changes are what they planned and didn't apply, themselves or with a token.
	Changes []store.Change
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
	actors := []string{"user:" + userID}
	for i := range out.Tokens {
		actors = append(actors, "token:"+out.Tokens[i].ID)
	}
	for _, actor := range actors {
		planned, err := tx.PlannedChangesBy(ctx, actor)
		if err != nil {
			return RemovalPlan{}, err
		}
		out.Changes = append(out.Changes, planned...)
	}
	return out, nil
}

// PlanMemberRemoval reads what removing someone would end, for whoever
// removes them to see first. It changes nothing.
func (s *Service) PlanMemberRemoval(ctx context.Context, p Principal, userID string) (RemovalPlan, error) {
	var out RemovalPlan
	err := s.db.View(ctx, func(tx *store.Tx) (err error) {
		out, err = s.plan(ctx, tx, p, userID, s.now())
		return err
	})
	return out, err
}

// RemoveMember removes a person, with a step-up bound to them: in one
// transaction their sign-in goes, their sessions end, their API tokens and the
// open invites they made are revoked, and they hold nothing. It returns what
// went, the changes they'd planned among it, for the caller to discard: the
// engine owns changes, and a change left planned is checked against whoever
// applies it, never against who planned it.
func (s *Service) RemoveMember(ctx context.Context, client Client, p Principal, userID, proof string) (RemovalPlan, error) {
	if _, err := s.PlanMemberRemoval(ctx, p, userID); err != nil {
		return RemovalPlan{}, err
	}
	if err := s.UseStepUp(ctx, client, p, proof, StepUpRemoveMember, userID); err != nil {
		return RemovalPlan{}, err
	}
	now := s.now()
	var out RemovalPlan
	err := s.db.Update(ctx, func(tx *store.Tx) error {
		var err error
		if out, err = s.plan(ctx, tx, p, userID, now); err != nil {
			return err
		}
		invites, err := s.revokeInvitesBy(ctx, tx, client, p, userID, now)
		if err != nil {
			return err
		}
		ended, err := tx.RemoveUser(ctx, userID, p.Actor(), now)
		if errors.Is(err, store.ErrOwner) {
			return fault.New(ErrTheOwner, "%s is the owner, who isn't removed: ownership moves by a transfer first", out.User.Name)
		}
		if err != nil {
			return err
		}
		detail := memberDetail{
			Email: out.User.Email, Name: out.User.Name, Role: Highest(out.Grants),
			Sessions: ended.Sessions, Tokens: ended.Tokens, Invites: invites, Changes: len(out.Changes),
		}
		return s.auditFrom(ctx, tx, p.Actor(), client.Surface(p), "member.remove", userID, detail.String())
	})
	if err != nil {
		return RemovalPlan{}, err
	}
	s.logger.InfoContext(ctx, "someone was removed", slog.String("user", userID), slog.String("email", out.User.Email), slog.String("by", p.Actor()))
	return out, nil
}

// TransferOwnership makes someone who signs in here already the owner, with a
// step-up bound to them, which only the owner makes. In one transaction the
// new owner gets the grant, the old one becomes an admin, and the CLI's local
// token that acted for the old owner is revoked; or nothing changes.
func (s *Service) TransferOwnership(ctx context.Context, client Client, p Principal, toUserID, proof string) (store.User, error) {
	check := func(tx *store.Tx) (store.User, error) {
		owner, err := tx.Owner(ctx)
		if err != nil && !errors.Is(err, store.ErrNotFound) {
			return store.User{}, err
		}
		if owner.ID != p.User.ID {
			return store.User{}, ErrOwnerOnly
		}
		if toUserID == p.User.ID {
			return store.User{}, fmt.Errorf("%w: you're the owner already. Choose someone else who signs in here", ErrInvalid)
		}
		to, err := tx.GetUser(ctx, toUserID)
		if errors.Is(err, store.ErrNotFound) || err == nil && to.RemovedAt != nil {
			return store.User{}, fmt.Errorf("%w: nobody signs in here as %s, and the owner is someone who does. Read Members again", ErrNotFound, toUserID)
		}
		return to, err
	}
	err := s.db.View(ctx, func(tx *store.Tx) error {
		_, err := check(tx)
		return err
	})
	if err != nil {
		return store.User{}, err
	}
	if err := s.UseStepUp(ctx, client, p, proof, StepUpTransferOwnership, toUserID); err != nil {
		return store.User{}, err
	}
	now := s.now()
	var to store.User
	err = s.db.Update(ctx, func(tx *store.Tx) error {
		var err error
		if to, err = check(tx); err != nil {
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
// with how each stood, as the caller read them, in the audit log. It's recorded
// once and kept; confirming again records it again.
func (s *Service) ConfirmProtections(ctx context.Context, client Client, p Principal, standing string) (Confirmed, error) {
	c := Confirmed{At: s.now(), By: p.Actor()}
	raw, err := json.Marshal(c)
	if err != nil {
		return Confirmed{}, err
	}
	err = s.db.Update(ctx, func(tx *store.Tx) error {
		if err := s.manager(ctx, tx, p); err != nil {
			return err
		}
		if err := tx.PutSetting(ctx, protectionsKey, raw); err != nil {
			return err
		}
		return s.auditFrom(ctx, tx, p.Actor(), client.Surface(p), "member.protections_confirm", "", standing)
	})
	return c, err
}
