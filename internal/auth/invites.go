// SPDX-License-Identifier: AGPL-3.0-only

package auth

// Invites: an admin or the owner asks someone, by email, to sign in with a role
// in a place, and they accept with a passkey at the panel's domain
// (docs/adr/0048-members-as-far-as-one-owner-goes.md,
// docs/adr/0049-roles-and-invites-by-project.md). An invite is access, not
// managed state, so it's audited, not a revision.

import (
	"context"
	"crypto/rand"
	"crypto/subtle"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"strings"
	"time"

	"github.com/go-webauthn/webauthn/webauthn"

	"github.com/dockwell/dockwell/internal/fault"
	"github.com/dockwell/dockwell/internal/id"
	"github.com/dockwell/dockwell/internal/store"
)

// How long an invite works: a week for a developer or a viewer, a day for an
// admin, whose link is a bearer credential to the install (ADR 0049).
const (
	inviteLasts      = 7 * 24 * time.Hour
	adminInviteLasts = 24 * time.Hour
)

// Invite errors.
var (
	// ErrInviteRejected is an invite secret that's unknown, used, revoked or
	// expired, said alike so it tells nothing of which.
	ErrInviteRejected = errors.New("this invite is invalid, already used, revoked or expired. Ask whoever invited you for a new one")
	// ErrInviteNeedsDomain is an invite on an install whose panel has no
	// domain: the link uses it, and the passkey is made there.
	ErrInviteNeedsDomain = errors.New("invites need the panel's domain, as the link uses it and the invited person's passkey is made there. Set DOCKWELL_DOMAIN for the service, then invite")
	// ErrInviteOwner is an invite as owner: ownership moves by transfer.
	ErrInviteOwner = errors.New("nobody is invited as owner: ownership moves by transfer, to someone who signs in here already. Invite them as an admin")
	// ErrMemberExists is an invite to someone who signs in here already.
	ErrMemberExists = errors.New("signs in here already")
	// ErrSignedIn is accepting an invite in a browser someone is signed in on:
	// accepting only ever makes a new person.
	ErrSignedIn = errors.New("signed in")
	// ErrInviteElsewhere is an invite opened at an address other than the
	// panel's domain, where the install keeps serving.
	ErrInviteElsewhere = errors.New("an invite works only at the panel's domain")
)

// InviteTarget is what an invite's step-up confirms: the email, the role and
// the place, apart by spaces, the place being "install" for the whole install.
func InviteTarget(email, role, projectID string) string {
	place := projectID
	if place == store.Install {
		place = "install"
	}
	return email + " " + role + " " + place
}

// normalizeInviteTarget writes a step-up target's email as CreateInvite will:
// "Ada@Example.com developer prj_…" becomes "ada@example.com developer prj_…".
func normalizeInviteTarget(target string) (string, error) {
	email, rest, ok := strings.Cut(strings.TrimSpace(target), " ")
	if !ok {
		return "", fmt.Errorf("%w: an invite's passkey confirms an email, a role and a place", ErrInvalid)
	}
	email, err := normalizeEmail(email)
	if err != nil {
		return "", err
	}
	return email + " " + rest, nil
}

// MadeInvite is an invite just made, with its link, shown once.
type MadeInvite struct {
	Invite store.Invite
	Link   string
}

// memberDetail is what a member's audit event records, for Activity to say.
type memberDetail struct {
	Email   string `json:"email"`
	Name    string `json:"name,omitempty"`
	Role    string `json:"role"`
	Project string `json:"project,omitempty"`
	From    string `json:"from,omitempty"`
}

func (d memberDetail) String() string {
	b, _ := json.Marshal(d)
	return string(b)
}

// MemberDetail is what a member's audit event recorded: who, as what, where,
// and for someone who joined, the address they joined from.
type MemberDetail struct {
	Email, Name, Role, Project, From string
}

// ReadMemberDetail reads what a member's audit event recorded.
func ReadMemberDetail(detail string) (MemberDetail, bool) {
	var d memberDetail
	if err := json.Unmarshal([]byte(detail), &d); err != nil || d.Email == "" {
		return MemberDetail{}, false
	}
	return MemberDetail(d), true
}

// CreateInvite invites email to sign in with role in the place projectID names,
// an existing project the caller checked, or store.Install. It takes a step-up
// bound to the email, the role and the place, and the panel's domain, which the
// link uses. A newer invite to the same email replaces an open one.
func (s *Service) CreateInvite(ctx context.Context, client Client, p Principal, email, role, projectID, proof string) (MadeInvite, error) {
	email, err := normalizeEmail(email)
	if err != nil {
		return MadeInvite{}, err
	}
	switch role {
	case store.RoleOwner:
		return MadeInvite{}, ErrInviteOwner
	case store.RoleAdmin, store.RoleDeveloper, store.RoleViewer:
	default:
		return MadeInvite{}, fmt.Errorf("%w: invite someone as an admin, a developer or a viewer", ErrInvalid)
	}
	if role == store.RoleAdmin && projectID != store.Install {
		return MadeInvite{}, fmt.Errorf("%w: an admin holds the whole install, so invite them to it rather than to a project", ErrInvalid)
	}
	if s.sites.Domain == "" {
		return MadeInvite{}, ErrInviteNeedsDomain
	}
	// Someone who signs in already is told so before the passkey's proof is spent on them.
	if err := s.db.View(ctx, func(tx *store.Tx) error { return s.memberAlready(ctx, tx, email) }); err != nil {
		return MadeInvite{}, err
	}
	if err := s.UseStepUp(p, proof, StepUpInvite, InviteTarget(email, role, projectID)); err != nil {
		return MadeInvite{}, err
	}
	now := s.now()
	// 256 random bits, as a link's (ADR 0049).
	secret, hash := newToken()
	lasts := inviteLasts
	if role == store.RoleAdmin {
		lasts = adminInviteLasts
	}
	inv := store.Invite{
		ID: id.New(id.Invite), TokenHash: hash, Email: email, Role: role, ProjectID: projectID,
		CreatedAt: now, CreatedBy: p.Actor(), ExpiresAt: now.Add(lasts),
	}
	err = s.db.Update(ctx, func(tx *store.Tx) error {
		// Again, with the write: they may have joined since.
		if err := s.memberAlready(ctx, tx, email); err != nil {
			return err
		}
		open, err := tx.ListOpenInvites(ctx, now)
		if err != nil {
			return err
		}
		for i := range open {
			if open[i].Email == email {
				if err := tx.RevokeInvite(ctx, open[i].ID, p.Actor(), now); err != nil {
					return err
				}
				// The one it replaces stops working, which is on record as any revoke is.
				if err := s.auditFrom(ctx, tx, p.Actor(), client.Surface(p), "member.invite_revoke", open[i].ID,
					memberDetail{Email: open[i].Email, Role: open[i].Role, Project: open[i].ProjectID}.String()); err != nil {
					return err
				}
			}
		}
		if err := tx.CreateInvite(ctx, inv); err != nil {
			return err
		}
		return s.auditFrom(ctx, tx, p.Actor(), client.Surface(p), "member.invite", inv.ID,
			memberDetail{Email: email, Role: role, Project: projectID}.String())
	})
	if err != nil {
		return MadeInvite{}, err
	}
	return MadeInvite{Invite: inv, Link: "https://" + s.sites.Domain + "/invite#" + secret}, nil
}

// memberAlready refuses inviting someone who signs in here already.
func (s *Service) memberAlready(ctx context.Context, tx *store.Tx, email string) error {
	if _, err := tx.GetUserByEmail(ctx, email); err == nil {
		return fault.New(ErrMemberExists, "%s signs in here already. Change what they hold in Members instead", email)
	} else if !errors.Is(err, store.ErrNotFound) {
		return err
	}
	return nil
}

// RevokeInvite stops an open invite working.
func (s *Service) RevokeInvite(ctx context.Context, client Client, p Principal, inviteID string) error {
	return s.db.Update(ctx, func(tx *store.Tx) error {
		inv, err := tx.GetInvite(ctx, inviteID)
		if err != nil {
			return err
		}
		if err := tx.RevokeInvite(ctx, inviteID, p.Actor(), s.now()); err != nil {
			return err
		}
		return s.auditFrom(ctx, tx, p.Actor(), client.Surface(p), "member.invite_revoke", inv.ID,
			memberDetail{Email: inv.Email, Role: inv.Role, Project: inv.ProjectID}.String())
	})
}

// InviteInfo is what an invite offers, for the page that accepts it.
type InviteInfo struct {
	Email, Role, ProjectID, ProjectName, InvitedBy string
	ExpiresAt                                      time.Time
}

// CheckInvite reads an invite without spending it.
func (s *Service) CheckInvite(ctx context.Context, client Client, secret string) (InviteInfo, error) {
	if !s.begins.take(sourceKey(client.Addr), s.now()) {
		return InviteInfo{}, ErrTooManyRequests
	}
	if err := s.atTheDomain(client); err != nil {
		return InviteInfo{}, err
	}
	var info InviteInfo
	err := s.db.View(ctx, func(tx *store.Tx) error {
		inv, err := s.openInvite(ctx, tx, secret)
		if err != nil {
			return err
		}
		info = InviteInfo{Email: inv.Email, Role: inv.Role, ProjectID: inv.ProjectID, ExpiresAt: inv.ExpiresAt, InvitedBy: s.ActorName(ctx, tx, inv.CreatedBy)}
		if inv.ProjectID != store.Install {
			if r, err := tx.GetResourceByID(ctx, inv.ProjectID); err == nil {
				info.ProjectName = r.Name
			}
		}
		return nil
	})
	if errors.Is(err, ErrInviteRejected) {
		s.logger.InfoContext(ctx, "refused an invite", slog.String("from", client.Addr))
	}
	return info, err
}

// atTheDomain refuses an invite anywhere but the panel's domain, where the
// install keeps serving, so the passkey isn't made at a tunnel that goes away.
func (s *Service) atTheDomain(client Client) error {
	if s.sites.Domain == "" || client.Site.RPID != s.sites.Domain {
		if s.sites.Domain == "" {
			return ErrInviteNeedsDomain
		}
		return fault.New(ErrInviteElsewhere, "this invite works only at the panel's domain, https://%s, where the install keeps serving. Open the link there", s.sites.Domain)
	}
	return nil
}

// openInvite reads the invite a secret names, if it still works.
func (s *Service) openInvite(ctx context.Context, tx *store.Tx, secret string) (store.Invite, error) {
	inv, err := tx.GetInviteByHash(ctx, hashToken(secret))
	if errors.Is(err, store.ErrNotFound) {
		return store.Invite{}, ErrInviteRejected
	}
	if err != nil {
		return store.Invite{}, err
	}
	if !inv.Open(s.now()) {
		return store.Invite{}, ErrInviteRejected
	}
	// Its place is checked each time it's opened: an invite to a project that's gone
	// since would make someone who holds nothing, so it's refused as any other is.
	if inv.ProjectID != store.Install {
		r, err := tx.GetResourceByID(ctx, inv.ProjectID)
		if errors.Is(err, store.ErrNotFound) || err == nil && (r.Spec == nil || r.Kind != projectKind) {
			return store.Invite{}, ErrInviteRejected
		}
		if err != nil {
			return store.Invite{}, err
		}
	}
	return inv, nil
}

// projectKind is a project's kind, as internal/scope names it.
const projectKind = "project"

// ActorName is who an actor is, by name, for words: a person's name, or for an
// API token, its maker's.
func (s *Service) ActorName(ctx context.Context, tx *store.Tx, actor string) string {
	userID := ""
	switch {
	case len(actor) > 5 && actor[:5] == "user:":
		userID = actor[5:]
	case len(actor) > 6 && actor[:6] == "token:":
		if tok, err := tx.GetAPIToken(ctx, actor[6:]); err == nil {
			userID = tok.UserID
		}
	}
	if u, err := tx.GetUser(ctx, userID); err == nil {
		return u.Name
	}
	return "Dockwell"
}

// BeginAcceptInvite starts creating the passkey of the person an invite names.
// The invite must still work; it's spent only by FinishAcceptInvite.
func (s *Service) BeginAcceptInvite(ctx context.Context, client Client, secret, name string) (Ceremony, error) {
	now := s.now()
	if !s.begins.take(sourceKey(client.Addr), now) {
		return Ceremony{}, ErrTooManyRequests
	}
	if err := s.atTheDomain(client); err != nil {
		return Ceremony{}, err
	}
	rp, err := s.relyingParty(client.Site)
	if err != nil {
		return Ceremony{}, err
	}
	if name, err = normalizeName(name); err != nil {
		return Ceremony{}, err
	}
	var inv store.Invite
	err = s.db.View(ctx, func(tx *store.Tx) error {
		if inv, err = s.openInvite(ctx, tx, secret); err != nil {
			return err
		}
		return s.notAMember(ctx, tx, inv.Email)
	})
	if err != nil {
		return Ceremony{}, err
	}
	handle := make([]byte, handleSize)
	_, _ = rand.Read(handle)
	person := store.User{ID: id.New(id.User), Email: inv.Email, Name: name, WebAuthnHandle: handle, CreatedAt: now}
	creation, session, err := rp.BeginRegistration(&waUser{user: person}, webauthn.WithRegistrationOrigin(client.Site.Origin))
	if err != nil {
		return Ceremony{}, fmt.Errorf("can't start creating a passkey: %w", err)
	}
	options, err := json.Marshal(creation.Response)
	if err != nil {
		return Ceremony{}, err
	}
	key := s.ceremonies.put(&ceremony{
		kind: ceremonyInvite, site: client.Site, session: *session, expires: now.Add(ceremonyTTL),
		user: person, claimHash: hashToken(secret),
	}, now)
	return Ceremony{ID: key, Options: options}, nil
}

// notAMember refuses an invite to someone who signs in here already, as
// accepting only ever makes a new person.
func (s *Service) notAMember(ctx context.Context, tx *store.Tx, email string) error {
	if _, err := tx.GetUserByEmail(ctx, email); err == nil {
		return fault.New(ErrMemberExists, "%s signs in here already, so this invite can't make them again. Sign in as them, or ask whoever invited you to change what they hold in Members", email)
	} else if !errors.Is(err, store.ErrNotFound) {
		return err
	}
	return nil
}

// FinishAcceptInvite checks the new passkey, then in one transaction spends the
// invite, makes the person with the role it names in its place and the
// passkey, and signs them in. Two acceptances racing can't both succeed: the
// invite is spent with a conditional update, and an email signs in once.
func (s *Service) FinishAcceptInvite(ctx context.Context, client Client, ceremonyID, secret string, credential []byte, passkeyName string) (SignedIn, error) {
	now := s.now()
	cer, err := s.ceremonies.take(ceremonyID, ceremonyInvite, now)
	if err != nil {
		return SignedIn{}, err
	}
	if cer.site != client.Site || subtle.ConstantTimeCompare(cer.claimHash, hashToken(secret)) != 1 {
		return SignedIn{}, ErrCeremonyExpired
	}
	cred, err := s.verifyNewPasskey(client.Site, &waUser{user: cer.user}, cer.session, credential)
	if err != nil {
		s.logger.InfoContext(ctx, "refused the passkey of an invite", slog.String("from", client.Addr), slog.Any("error", err))
		return SignedIn{}, err
	}
	var signedIn SignedIn
	err = s.db.Update(ctx, func(tx *store.Tx) error {
		inv, err := s.openInvite(ctx, tx, secret)
		if err != nil {
			return err
		}
		if err := s.notAMember(ctx, tx, inv.Email); err != nil {
			return err
		}
		person := cer.user
		if err := tx.CreateUser(ctx, person); errors.Is(err, store.ErrConflict) {
			return fault.New(ErrMemberExists, "%s signs in here already, so this invite can't make them again", inv.Email)
		} else if err != nil {
			return err
		}
		if err := tx.SetGrant(ctx, store.Grant{UserID: person.ID, ProjectID: inv.ProjectID, Role: inv.Role, CreatedAt: now, CreatedBy: inv.CreatedBy}); err != nil {
			return err
		}
		if err := tx.AcceptInvite(ctx, inv.ID, person.ID, now); errors.Is(err, store.ErrNotFound) {
			return ErrInviteRejected
		} else if err != nil {
			return err
		}
		pk, err := s.storePasskey(ctx, tx, person, client.Site, cred, passkeyName)
		if err != nil {
			return err
		}
		actor := "user:" + person.ID
		detail := memberDetail{Email: person.Email, Name: person.Name, Role: inv.Role, Project: inv.ProjectID, From: client.remoteIP()}
		if err := s.auditFrom(ctx, tx, actor, client.Surface(Principal{User: person}), "member.join", inv.ID, detail.String()); err != nil {
			return err
		}
		if err := s.auditOn(ctx, tx, actor, "passkey.add", person.ID, pk.ID, pk.ID+" for "+pk.RPID); err != nil {
			return err
		}
		signedIn, err = s.startSession(ctx, tx, person, client, MethodInvite)
		return err
	})
	if err != nil {
		if errors.Is(err, ErrInviteRejected) || errors.Is(err, ErrMemberExists) {
			s.logger.WarnContext(ctx, "refused an invite", slog.String("from", client.Addr), slog.Any("error", err))
		}
		return SignedIn{}, err
	}
	s.logger.InfoContext(ctx, "someone joined with an invite", slog.String("user", signedIn.User.ID), slog.String("email", signedIn.User.Email))
	return signedIn, nil
}
