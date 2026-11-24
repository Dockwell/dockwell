// SPDX-License-Identifier: AGPL-3.0-only

package firewall

import (
	"context"
	"crypto/sha256"
	"errors"
	"fmt"
	"log/slog"
	"net/netip"
	"strings"
	"time"

	"connectrpc.com/connect"
	"google.golang.org/protobuf/proto"

	dockwellv1 "github.com/dockwell/dockwell/gen/dockwell/v1"
	"github.com/dockwell/dockwell/internal/auth"
	"github.com/dockwell/dockwell/internal/control/agentapi"
	"github.com/dockwell/dockwell/internal/control/authapi"
	"github.com/dockwell/dockwell/internal/id"
	"github.com/dockwell/dockwell/internal/store"
)

// SSH access operations recorded on the server activity timeline.
const (
	EventSSHKeyAdded      = "ssh.key_add"
	EventSSHKeyRemoved    = "ssh.key_remove"
	EventSSHDisconnected  = "ssh.disconnect"
	EventSSHAddressBanned = "ssh.ban"
)

// WithSSHChanges enables passkey-protected SSH operations and their audit log.
func (h *Handler) WithSSHChanges(a *auth.Service, db *store.DB, logger *slog.Logger) *Handler {
	h.auth, h.db, h.logger = a, db, logger
	return h
}

func (h *Handler) sshWrite(ctx context.Context, proof, target string, action auth.StepUpAction, wire dockwellv1.StepUpAction) (auth.Principal, error) {
	p, ok := authapi.PrincipalFrom(ctx)
	if !ok || p.Token != nil || h.auth == nil || h.db == nil {
		return p, connect.NewError(connect.CodePermissionDenied, errors.New("sign in to manage SSH access"))
	}
	client, _ := authapi.ClientFrom(ctx)
	if proof != "" {
		if _, err := h.auth.HoldStepUp(ctx, client, p, proof, action, target); err == nil {
			return p, nil
		}
	}
	err := authapi.ReasonError(connect.CodePermissionDenied, auth.ErrNeedsStepUp, authapi.ReasonNeedsStepUp)
	if detail, e := connect.NewErrorDetail(&dockwellv1.StepUpNeeded{Action: wire, Target: target}); e == nil {
		err.AddDetail(detail)
	}
	return p, err
}

func (h *Handler) sshRecord(ctx context.Context, p auth.Principal, server, action, subject, detail string) {
	client, _ := authapi.ClientFrom(ctx)
	err := h.db.Update(ctx, func(tx *store.Tx) error {
		return tx.RecordEvent(ctx, store.Event{ID: id.New(id.Event), At: time.Now(), Actor: p.Actor(), Target: server, Action: action, Subject: subject, Detail: detail, Surface: client.Surface(p)})
	})
	if err != nil && h.logger != nil {
		h.logger.ErrorContext(ctx, "can't record an SSH access operation", slog.String("action", action), slog.String("server", server), slog.String("error", err.Error()))
	}
}

// ListServerSshKeys reads SSH from the server's agent.
//
//nolint:revive // generated service method name
func (h *Handler) ListServerSshKeys(ctx context.Context, req *connect.Request[dockwellv1.ListServerSshKeysRequest]) (*connect.Response[dockwellv1.ListServerSshKeysResponse], error) {
	return sshRead(ctx, h, req.Msg.GetServerId(), req.Msg, (*agentapi.Session).ListSSHKeys)
}

// ListServerSshSessions reads SSH from the server's agent.
//
//nolint:revive // generated service method name
func (h *Handler) ListServerSshSessions(ctx context.Context, req *connect.Request[dockwellv1.ListServerSshSessionsRequest]) (*connect.Response[dockwellv1.ListServerSshSessionsResponse], error) {
	return sshRead(ctx, h, req.Msg.GetServerId(), req.Msg, (*agentapi.Session).ListSSHSessions)
}

// ListServerSshLogins reads SSH from the server's agent.
//
//nolint:revive // generated service method name
func (h *Handler) ListServerSshLogins(ctx context.Context, req *connect.Request[dockwellv1.ListServerSshLoginsRequest]) (*connect.Response[dockwellv1.ListServerSshLoginsResponse], error) {
	return sshRead(ctx, h, req.Msg.GetServerId(), req.Msg, (*agentapi.Session).ListSSHLogins)
}

func sshRead[Req, Res any](ctx context.Context, h *Handler, serverID string, req *Req, run func(*agentapi.Session, context.Context, *Req) (*Res, error)) (*connect.Response[Res], error) {
	srv, session, err := h.session(ctx, serverID, "how SSH lets people in")
	if err != nil {
		return nil, err
	}
	out, err := run(session, ctx, req)
	if err != nil {
		return nil, readError(srv, "SSH", "how SSH lets people in", err)
	}
	return connect.NewResponse(out), nil
}

// EditServerSshKey adds or removes a public key with a fresh passkey.
//
//nolint:revive // generated service method name
func (h *Handler) EditServerSshKey(ctx context.Context, req *connect.Request[dockwellv1.EditServerSshKeyRequest]) (*connect.Response[dockwellv1.EditServerSshKeyResponse], error) {
	m := req.Msg
	action, target := EventSSHKeyRemoved, "remove:"+m.GetRemoveFingerprint()
	if public := m.GetPublicKey(); public != "" {
		if strings.Contains(public, "PRIVATE KEY") || strings.HasPrefix(public, "PuTTY-User-Key-File") {
			return nil, authapi.Invalid(errors.New("this is a private key. Paste the public key instead"))
		}
		if len(public) > 16<<10 {
			return nil, authapi.Invalid(errors.New("paste one OpenSSH public key under 16 KiB"))
		}
		action, target = EventSSHKeyAdded, fmt.Sprintf("add:%x", sha256.Sum256([]byte(public)))
	} else if fp := m.GetRemoveFingerprint(); len(fp) != 50 || !strings.HasPrefix(fp, "SHA256:") {
		return nil, authapi.Invalid(errors.New("choose a key by its SHA256 fingerprint"))
	}
	if m.GetAccount() == "" || len(m.GetAccount()) > 64 || strings.ContainsAny(m.GetAccount(), ":\n\r") {
		return nil, authapi.Invalid(errors.New("choose a local SSH account"))
	}
	srv, session, err := h.session(ctx, m.GetServerId(), "which keys are authorized")
	if err != nil {
		return nil, err
	}
	p, err := h.sshWrite(ctx, m.GetProof(), srv.GetId()+":"+m.GetAccount()+":"+target, auth.StepUpEditSSHKey, dockwellv1.StepUpAction_STEP_UP_ACTION_EDIT_SSH_KEY)
	if err != nil {
		return nil, err
	}
	ctx = context.WithoutCancel(ctx)
	edit := proto.CloneOf(m)
	edit.Proof = ""
	out, err := session.EditSSHKey(ctx, edit)
	if err != nil {
		return nil, readError(srv, "SSH keys", "change an authorized key", err)
	}
	h.sshRecord(ctx, p, srv.GetId(), action, m.GetAccount(), out.GetKey().GetFingerprint())
	return connect.NewResponse(out), nil
}

// DisconnectServerSshSession disconnects an identified SSH session.
//
//nolint:revive // generated service method name
func (h *Handler) DisconnectServerSshSession(ctx context.Context, req *connect.Request[dockwellv1.DisconnectServerSshSessionRequest]) (*connect.Response[dockwellv1.DisconnectServerSshSessionResponse], error) {
	m := req.Msg
	if m.GetSessionId() == "" || len(m.GetSessionId()) > 96 {
		return nil, authapi.Invalid(errors.New("choose an SSH session from the session list"))
	}
	srv, session, err := h.session(ctx, m.GetServerId(), "which SSH sessions are open")
	if err != nil {
		return nil, err
	}
	p, err := h.sshWrite(ctx, m.GetProof(), srv.GetId()+":"+m.GetSessionId(), auth.StepUpDisconnectSSHSession, dockwellv1.StepUpAction_STEP_UP_ACTION_DISCONNECT_SSH_SESSION)
	if err != nil {
		return nil, err
	}
	ctx = context.WithoutCancel(ctx)
	out, err := session.DisconnectSSHSession(ctx, &dockwellv1.DisconnectServerSshSessionRequest{SessionId: m.GetSessionId()})
	if err != nil {
		return nil, readError(srv, "SSH sessions", "disconnect an SSH session", err)
	}
	h.sshRecord(ctx, p, srv.GetId(), EventSSHDisconnected, m.GetSessionId(), "")
	return connect.NewResponse(out), nil
}

// BanServerSshAddress bans a failed SSH login's address in fail2ban.
//
//nolint:revive // generated service method name
func (h *Handler) BanServerSshAddress(ctx context.Context, req *connect.Request[dockwellv1.BanServerSshAddressRequest]) (*connect.Response[dockwellv1.BanServerSshAddressResponse], error) {
	m := req.Msg
	addr, err := netip.ParseAddr(m.GetAddress())
	if err != nil || addr.Zone() != "" || !addr.IsGlobalUnicast() || addr.Unmap().String() == callersAddress(ctx) {
		return nil, authapi.Invalid(errors.New("choose a failed SSH address that isn't your current address"))
	}
	address := addr.Unmap().String()
	srv, session, err := h.session(ctx, m.GetServerId(), "which addresses failed SSH sign-in")
	if err != nil {
		return nil, err
	}
	p, err := h.sshWrite(ctx, m.GetProof(), srv.GetId()+":"+address, auth.StepUpBanSSHAddress, dockwellv1.StepUpAction_STEP_UP_ACTION_BAN_SSH_ADDRESS)
	if err != nil {
		return nil, err
	}
	ctx = context.WithoutCancel(ctx)
	out, err := session.BanSSHAddress(ctx, &dockwellv1.BanServerSshAddressRequest{Address: address})
	if err != nil {
		return nil, readError(srv, "fail2ban", "ban this SSH address", err)
	}
	h.sshRecord(ctx, p, srv.GetId(), EventSSHAddressBanned, address, "")
	return connect.NewResponse(out), nil
}
