// SPDX-License-Identifier: AGPL-3.0-only

package members

// Managing members: changing what someone holds, removing them, transferring
// ownership and confirming the protections (ADR 0049, "Managing members").

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"slices"
	"strings"

	"connectrpc.com/connect"
	"google.golang.org/protobuf/types/known/timestamppb"

	dockwellv1 "github.com/dockwell/dockwell/gen/dockwell/v1"
	"github.com/dockwell/dockwell/internal/control/authapi"
	"github.com/dockwell/dockwell/internal/control/place"
	"github.com/dockwell/dockwell/internal/revision"
	"github.com/dockwell/dockwell/internal/scope"
	"github.com/dockwell/dockwell/internal/store"
)

// Changes discards the changes someone removed had planned. *revision.Engine is one.
type Changes interface {
	Discard(ctx context.Context, changeID string, by revision.By) error
}

// dockwell is Dockwell acting on its own, as it does when it discards what a
// removed person planned.
var dockwell = revision.By{Actor: revision.SystemActor, Via: revision.ViaAutomatic}

// memberNow reads a person as the members list has them.
func (h *Handler) memberNow(ctx context.Context, tx *store.Tx, userID, asking string) (*dockwellv1.Member, error) {
	u, err := tx.GetUser(ctx, userID)
	if err != nil {
		return nil, err
	}
	in, err := h.people.SignInOf(ctx, tx, &u)
	if err != nil {
		return nil, err
	}
	sessions, err := tx.ListActiveSessions(ctx, u.ID, h.now())
	if err != nil {
		return nil, err
	}
	m := member(&u, in, sessions, u.ID == asking)
	if m.Grants, err = projectGrants(ctx, tx, u.ID); err != nil {
		return nil, err
	}
	h.joined(ctx, tx, m)
	return m, nil
}

// joined says who invited someone, when they joined and from where, for
// someone who joined with an invite.
func (h *Handler) joined(ctx context.Context, tx *store.Tx, m *dockwellv1.Member) {
	inv, err := tx.AcceptedInvite(ctx, m.GetUser().GetId())
	if err != nil || inv.AcceptedAt == nil {
		return
	}
	m.InvitedBy, m.JoinTime, m.JoinedFrom = h.people.ActorName(ctx, tx, inv.CreatedBy), timestamppb.New(*inv.AcceptedAt), inv.AcceptedFrom
}

// projectOf resolves the project a request names, or the install for none.
func (h *Handler) projectOf(ctx context.Context, ref string) (string, error) {
	if ref == "" {
		return store.Install, nil
	}
	projectID := store.Install
	err := h.db.View(ctx, func(tx *store.Tx) error {
		var err error
		projectID, err = place.Project(ctx, tx, ref)
		return err
	})
	if errors.Is(err, store.ErrNotFound) {
		return "", authapi.ReasonError(connect.CodeNotFound, fmt.Errorf("there's no project %s. Check its name, or leave project out for the whole install", ref), "not_found")
	}
	if err != nil {
		return "", authapi.AuthError(ctx, h.logger, err)
	}
	return projectID, nil
}

// SetMemberRole gives someone a role in a place, or takes it back.
func (h *Handler) SetMemberRole(ctx context.Context, req *connect.Request[dockwellv1.SetMemberRoleRequest]) (*connect.Response[dockwellv1.SetMemberRoleResponse], error) {
	p, _ := authapi.PrincipalFrom(ctx)
	client, _ := authapi.ClientFrom(ctx)
	m := req.Msg
	projectID, err := h.projectOf(ctx, m.GetProject())
	if err != nil {
		return nil, err
	}
	role := ""
	if m.GetRole() != dockwellv1.Role_ROLE_UNSPECIFIED {
		role = authapi.RoleName(m.GetRole())
	}
	changed, err := h.people.SetMemberRole(ctx, client, p, m.GetUserId(), role, projectID, m.GetProof())
	if err != nil {
		return nil, authapi.AuthError(ctx, h.logger, err)
	}
	res := &dockwellv1.SetMemberRoleResponse{
		Lowered:        changed.Lowered,
		SessionsEnded:  int32(changed.Ended.Sessions),   //nolint:gosec // a person's sessions
		TokensRevoked:  int32(changed.Ended.Tokens),     //nolint:gosec // a person's tokens
		InvitesRevoked: int32(changed.Invites),          //nolint:gosec // a person's invites
		HooksDeleted:   int32(len(changed.Ended.Hooks)), //nolint:gosec // a person's hooks
	}
	err = h.db.View(ctx, func(tx *store.Tx) error {
		var err error
		res.Member, err = h.memberNow(ctx, tx, m.GetUserId(), p.User.ID)
		return err
	})
	if err != nil {
		return nil, authapi.AuthError(ctx, h.logger, err)
	}
	return connect.NewResponse(res), nil
}

// PlanMemberRemoval reads what removing someone would end.
func (h *Handler) PlanMemberRemoval(ctx context.Context, req *connect.Request[dockwellv1.PlanMemberRemovalRequest]) (*connect.Response[dockwellv1.PlanMemberRemovalResponse], error) {
	p, _ := authapi.PrincipalFrom(ctx)
	plan, err := h.people.PlanMemberRemoval(ctx, p, req.Msg.GetUserId())
	if err != nil {
		return nil, authapi.AuthError(ctx, h.logger, err)
	}
	res := &dockwellv1.PlanMemberRemovalResponse{Sessions: int32(plan.Sessions)} //nolint:gosec // a person's sessions
	for i := range plan.Tokens {
		t := &plan.Tokens[i]
		token := &dockwellv1.RemovalToken{Id: t.ID, Name: t.Name}
		if t.LastUsedAt != nil {
			token.LastUsedTime = timestamppb.New(*t.LastUsedAt)
		}
		switch t.Role {
		case store.RoleDeveloper:
			token.Role = dockwellv1.TokenRole_TOKEN_ROLE_DEVELOPER
		case store.RoleViewer:
			token.Role = dockwellv1.TokenRole_TOKEN_ROLE_VIEWER
		}
		res.Tokens = append(res.Tokens, token)
	}
	for i := range plan.Changes {
		c := &plan.Changes[i]
		res.Changes = append(res.Changes, &dockwellv1.RemovalChange{Id: c.ID, Summary: c.Summary, CreateTime: timestamppb.New(c.CreatedAt)})
	}
	for i := range plan.Links {
		res.Links = append(res.Links, &dockwellv1.RemovalLink{Provider: plan.Links[i].Provider, Account: plan.Links[i].Account})
	}
	err = h.db.View(ctx, func(tx *store.Tx) error {
		for i := range plan.Invites {
			res.Invites = append(res.Invites, h.inviteProto(ctx, tx, &plan.Invites[i]))
		}
		for i := range plan.Hooks {
			hook := &dockwellv1.RemovalHook{ServiceId: plan.Hooks[i].ResourceID, ServiceName: plan.Hooks[i].ResourceID}
			if r, err := tx.GetResourceByID(ctx, plan.Hooks[i].ResourceID); err == nil {
				hook.ServiceName = r.Name
			}
			res.Hooks = append(res.Hooks, hook)
		}
		// Each token's projects by their names now; one that's gone has none (ADR 0071).
		for i := range plan.Tokens {
			for _, projectID := range plan.Tokens[i].Projects {
				p := &dockwellv1.TokenProject{Id: projectID}
				if r, err := tx.GetResourceByID(ctx, projectID); err == nil && r.Spec != nil {
					p.Name = r.Name
				}
				res.Tokens[i].Projects = append(res.Tokens[i].Projects, p)
			}
		}
		var err error
		res.Member, err = h.memberNow(ctx, tx, req.Msg.GetUserId(), p.User.ID)
		return err
	})
	if err != nil {
		return nil, authapi.AuthError(ctx, h.logger, err)
	}
	return connect.NewResponse(res), nil
}

// RemoveMember removes someone, then discards the changes they'd planned.
func (h *Handler) RemoveMember(ctx context.Context, req *connect.Request[dockwellv1.RemoveMemberRequest]) (*connect.Response[dockwellv1.RemoveMemberResponse], error) {
	p, _ := authapi.PrincipalFrom(ctx)
	client, _ := authapi.ClientFrom(ctx)
	gone, err := h.people.RemoveMember(ctx, client, p, req.Msg.GetUserId(), req.Msg.GetProof())
	if err != nil {
		return nil, authapi.AuthError(ctx, h.logger, err)
	}
	// What the removal's transaction ended, as its audit event says.
	res := &dockwellv1.RemoveMemberResponse{
		SessionsEnded:  int32(gone.Ended.Sessions),   //nolint:gosec // a person's sessions
		TokensRevoked:  int32(gone.Ended.Tokens),     //nolint:gosec // a person's tokens
		InvitesRevoked: int32(gone.InvitesRevoked),   //nolint:gosec // a person's invites
		LinksDeleted:   int32(gone.Ended.Links),      //nolint:gosec // a person's provider links
		HooksDeleted:   int32(len(gone.Ended.Hooks)), //nolint:gosec // a person's hooks
	}
	// They're gone already, whatever becomes of these: a change left planned is
	// checked against whoever applies it, never against who planned it.
	for i := range gone.Changes {
		id := gone.Changes[i].ID
		if h.changes == nil {
			res.ChangesLeft = append(res.ChangesLeft, id)
			continue
		}
		if err := h.changes.Discard(context.WithoutCancel(ctx), id, dockwell); err != nil {
			h.logger.WarnContext(ctx, "couldn't discard a change someone removed had planned; it stays planned",
				slog.String("change", id), slog.String("user", req.Msg.GetUserId()), slog.Any("error", err))
			res.ChangesLeft = append(res.ChangesLeft, id)
			continue
		}
		res.ChangesDiscarded++
	}
	return connect.NewResponse(res), nil
}

// TransferOwnership makes someone else the owner.
func (h *Handler) TransferOwnership(ctx context.Context, req *connect.Request[dockwellv1.TransferOwnershipRequest]) (*connect.Response[dockwellv1.TransferOwnershipResponse], error) {
	p, _ := authapi.PrincipalFrom(ctx)
	client, _ := authapi.ClientFrom(ctx)
	to, err := h.people.TransferOwnership(ctx, client, p, req.Msg.GetUserId(), req.Msg.GetProof())
	if err != nil {
		return nil, authapi.AuthError(ctx, h.logger, err)
	}
	res := &dockwellv1.TransferOwnershipResponse{}
	err = h.db.View(ctx, func(tx *store.Tx) error {
		var err error
		res.Member, err = h.memberNow(ctx, tx, to.ID, p.User.ID)
		return err
	})
	if err != nil {
		return nil, authapi.AuthError(ctx, h.logger, err)
	}
	return connect.NewResponse(res), nil
}

// ConfirmProtections records that the caller looked at which environments are
// protected, with how each stood when it's recorded, read in the same
// transaction. A second confirmation returns the first.
func (h *Handler) ConfirmProtections(ctx context.Context, _ *connect.Request[dockwellv1.ConfirmProtectionsRequest]) (*connect.Response[dockwellv1.ConfirmProtectionsResponse], error) {
	p, _ := authapi.PrincipalFrom(ctx)
	client, _ := authapi.ClientFrom(ctx)
	done, err := h.people.ConfirmProtections(ctx, client, p, func(tx *store.Tx) (string, error) {
		tree, err := scope.Load(ctx, tx)
		if err != nil {
			return "", err
		}
		var standing []string
		for _, project := range tree.Projects {
			for _, env := range project.Environments {
				word := "open"
				if env.Protected {
					word = "protected"
				}
				standing = append(standing, project.Name+"/"+env.Name+" "+word)
			}
		}
		slices.Sort(standing)
		return strings.Join(standing, ", "), nil
	})
	if err != nil {
		return nil, authapi.AuthError(ctx, h.logger, err)
	}
	return connect.NewResponse(&dockwellv1.ConfirmProtectionsResponse{ConfirmTime: timestamppb.New(done.At)}), nil
}
