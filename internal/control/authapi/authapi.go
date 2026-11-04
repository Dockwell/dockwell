// SPDX-License-Identifier: AGPL-3.0-only

// Package authapi serves dockwell.v1.AuthService and guards the rest of the
// API listener: Guard checks the host and origin of every request, and
// Interceptor requires a session for every procedure that isn't public.
package authapi

import (
	"context"
	"errors"
	"log/slog"
	"strconv"
	"strings"

	"connectrpc.com/connect"
	"google.golang.org/protobuf/types/known/timestamppb"

	dockwellv1 "github.com/dockwell/dockwell/gen/dockwell/v1"
	"github.com/dockwell/dockwell/gen/dockwell/v1/dockwellv1connect"
	"github.com/dockwell/dockwell/internal/auth"
	"github.com/dockwell/dockwell/internal/store"
)

// Config configures a Handler.
type Config struct {
	Auth   *auth.Service
	Logger *slog.Logger
	// ControlServer returns the control server as the fleet sees it, for the
	// claim screen and the sign-in screen. Optional.
	ControlServer func() *dockwellv1.Server
	// PublicAddress is this server's public IP address, such as
	// "203.0.113.7", for pointing a domain at. Empty when it isn't known.
	PublicAddress string
}

// Handler implements AuthService.
type Handler struct {
	auth          *auth.Service
	logger        *slog.Logger
	controlServer func() *dockwellv1.Server
	publicAddress string
}

var _ dockwellv1connect.AuthServiceHandler = (*Handler)(nil)

// New returns a Handler.
func New(cfg Config) *Handler {
	if cfg.ControlServer == nil {
		cfg.ControlServer = func() *dockwellv1.Server { return nil }
	}
	return &Handler{auth: cfg.Auth, logger: cfg.Logger, controlServer: cfg.ControlServer, publicAddress: cfg.PublicAddress}
}

func (h *Handler) client(ctx context.Context) (auth.Client, error) {
	c, ok := ClientFrom(ctx)
	if !ok {
		return auth.Client{}, connect.NewError(connect.CodeInternal, errors.New("the request didn't pass the API guard"))
	}
	return c, nil
}

// principal returns the signed-in user behind a call. AuthService and
// TokenService need a session, so it refuses an API token, as Interceptor
// already has.
func principal(ctx context.Context) (auth.Principal, error) {
	p, ok := PrincipalFrom(ctx)
	if !ok {
		return auth.Principal{}, ReasonError(connect.CodeUnauthenticated, auth.ErrUnauthenticated, ReasonUnauthenticated)
	}
	if p.Token != nil {
		return auth.Principal{}, ReasonError(connect.CodePermissionDenied, auth.ErrNeedsSession, ReasonNeedsSession)
	}
	return p, nil
}

// GetAuthState describes the install and this request's session.
func (h *Handler) GetAuthState(ctx context.Context, _ *connect.Request[dockwellv1.GetAuthStateRequest]) (*connect.Response[dockwellv1.GetAuthStateResponse], error) {
	client, err := h.client(ctx)
	if err != nil {
		return nil, err
	}
	claimed, err := h.auth.Claimed(ctx)
	if err != nil {
		return nil, h.rpcError(ctx, err)
	}
	msg := &dockwellv1.GetAuthStateResponse{
		Claimed: claimed,
		RpId:    client.Site.RPID,
		Domain:  h.auth.Sites().Domain,
	}
	if srv := h.controlServer(); srv != nil {
		msg.ServerName = srv.GetName()
	}
	res := connect.NewResponse(msg)
	if p, ok := PrincipalFrom(ctx); ok {
		msg.User = userProto(p.User)
		msg.SessionId = p.Session.ID
		msg.SignInMethod = methodProto(p.Session.Method)
	} else if req, ok := ctx.Value(requestKey{}).(request); ok && req.token != "" && !sessionUnchecked(ctx) {
		// A cookie for a session that ended: clear it. One that couldn't be
		// checked stays, as its session may still work.
		res.Header().Add("Set-Cookie", client.Site.ClearCookie().String())
	}
	return res, nil
}

// CheckClaim checks a claim link and describes the server it claims.
func (h *Handler) CheckClaim(ctx context.Context, req *connect.Request[dockwellv1.CheckClaimRequest]) (*connect.Response[dockwellv1.CheckClaimResponse], error) {
	client, err := h.client(ctx)
	if err != nil {
		return nil, err
	}
	expires, err := h.auth.CheckClaim(ctx, client, req.Msg.GetToken())
	if err != nil {
		return nil, h.rpcError(ctx, err)
	}
	return connect.NewResponse(&dockwellv1.CheckClaimResponse{
		Server:        h.controlServer(),
		PublicAddress: h.publicAddress,
		ExpireTime:    timestamppb.New(expires),
	}), nil
}

// BeginClaim starts creating the owner's passkey.
func (h *Handler) BeginClaim(ctx context.Context, req *connect.Request[dockwellv1.BeginClaimRequest]) (*connect.Response[dockwellv1.BeginClaimResponse], error) {
	client, err := h.client(ctx)
	if err != nil {
		return nil, err
	}
	m := req.Msg
	cer, err := h.auth.BeginClaim(ctx, client, m.GetToken(), m.GetName(), m.GetEmail())
	if err != nil {
		return nil, h.rpcError(ctx, err)
	}
	return connect.NewResponse(&dockwellv1.BeginClaimResponse{CeremonyId: cer.ID, OptionsJson: string(cer.Options)}), nil
}

// FinishClaim creates the owner and signs them in.
func (h *Handler) FinishClaim(ctx context.Context, req *connect.Request[dockwellv1.FinishClaimRequest]) (*connect.Response[dockwellv1.FinishClaimResponse], error) {
	client, err := h.client(ctx)
	if err != nil {
		return nil, err
	}
	m := req.Msg
	signedIn, err := h.auth.FinishClaim(ctx, client, m.GetCeremonyId(), m.GetToken(), []byte(m.GetCredentialJson()), m.GetPasskeyName())
	if err != nil {
		return nil, h.rpcError(ctx, err)
	}
	return withCookie(connect.NewResponse(&dockwellv1.FinishClaimResponse{User: userProto(signedIn.User)}), client.Site, signedIn)
}

// BeginPasskeySignIn starts a passkey sign-in.
func (h *Handler) BeginPasskeySignIn(ctx context.Context, _ *connect.Request[dockwellv1.BeginPasskeySignInRequest]) (*connect.Response[dockwellv1.BeginPasskeySignInResponse], error) {
	client, err := h.client(ctx)
	if err != nil {
		return nil, err
	}
	cer, err := h.auth.BeginSignIn(ctx, client)
	if err != nil {
		return nil, h.rpcError(ctx, err)
	}
	return connect.NewResponse(&dockwellv1.BeginPasskeySignInResponse{CeremonyId: cer.ID, OptionsJson: string(cer.Options)}), nil
}

// FinishPasskeySignIn signs in with a passkey.
func (h *Handler) FinishPasskeySignIn(ctx context.Context, req *connect.Request[dockwellv1.FinishPasskeySignInRequest]) (*connect.Response[dockwellv1.FinishPasskeySignInResponse], error) {
	client, err := h.client(ctx)
	if err != nil {
		return nil, err
	}
	signedIn, err := h.auth.FinishSignIn(ctx, client, req.Msg.GetCeremonyId(), []byte(req.Msg.GetCredentialJson()))
	if err != nil {
		return nil, h.rpcError(ctx, err)
	}
	return withCookie(connect.NewResponse(&dockwellv1.FinishPasskeySignInResponse{User: userProto(signedIn.User)}), client.Site, signedIn)
}

// SignInWithCode signs in with an authenticator or recovery code.
func (h *Handler) SignInWithCode(ctx context.Context, req *connect.Request[dockwellv1.SignInWithCodeRequest]) (*connect.Response[dockwellv1.SignInWithCodeResponse], error) {
	client, err := h.client(ctx)
	if err != nil {
		return nil, err
	}
	out, err := h.auth.SignInWithCode(ctx, client, req.Msg.GetEmail(), req.Msg.GetCode())
	if err != nil {
		return nil, h.rpcError(ctx, err)
	}
	return withCookie(connect.NewResponse(&dockwellv1.SignInWithCodeResponse{
		User:              userProto(out.User),
		UsedRecoveryCode:  out.UsedRecoveryCode,
		RecoveryCodesLeft: int32(out.RecoveryCodesLeft), //nolint:gosec // at most ten
	}), client.Site, out.SignedIn)
}

// SignInWithLink signs in with a link from dockwell recover.
func (h *Handler) SignInWithLink(ctx context.Context, req *connect.Request[dockwellv1.SignInWithLinkRequest]) (*connect.Response[dockwellv1.SignInWithLinkResponse], error) {
	client, err := h.client(ctx)
	if err != nil {
		return nil, err
	}
	signedIn, err := h.auth.SignInWithLink(ctx, client, req.Msg.GetToken())
	if err != nil {
		return nil, h.rpcError(ctx, err)
	}
	return withCookie(connect.NewResponse(&dockwellv1.SignInWithLinkResponse{User: userProto(signedIn.User)}), client.Site, signedIn)
}

// SignOut ends this session and clears its cookie.
func (h *Handler) SignOut(ctx context.Context, _ *connect.Request[dockwellv1.SignOutRequest]) (*connect.Response[dockwellv1.SignOutResponse], error) {
	client, err := h.client(ctx)
	if err != nil {
		return nil, err
	}
	p, err := principal(ctx)
	if err != nil {
		return nil, err
	}
	if err := h.auth.SignOut(ctx, p); err != nil {
		return nil, h.rpcError(ctx, err)
	}
	res := connect.NewResponse(&dockwellv1.SignOutResponse{})
	res.Header().Add("Set-Cookie", client.Site.ClearCookie().String())
	return res, nil
}

// ListSessions lists the caller's sessions.
func (h *Handler) ListSessions(ctx context.Context, _ *connect.Request[dockwellv1.ListSessionsRequest]) (*connect.Response[dockwellv1.ListSessionsResponse], error) {
	p, err := principal(ctx)
	if err != nil {
		return nil, err
	}
	all, err := h.auth.ListSessions(ctx, p)
	if err != nil {
		return nil, h.rpcError(ctx, err)
	}
	out := make([]*dockwellv1.Session, 0, len(all))
	for i := range all {
		s := &all[i]
		out = append(out, &dockwellv1.Session{
			Id:           s.ID,
			UserAgent:    s.UserAgent,
			Address:      s.LastIP,
			CreateTime:   timestamppb.New(s.CreatedAt),
			LastSeenTime: timestamppb.New(s.LastSeenAt),
			ExpireTime:   timestamppb.New(auth.SessionEnd(*s)),
			Current:      s.ID == p.Session.ID,
			SignInMethod: methodProto(s.Method),
			RpId:         s.RPID,
		})
	}
	return connect.NewResponse(&dockwellv1.ListSessionsResponse{Sessions: out}), nil
}

// RevokeSession ends one of the caller's sessions. Revoking the current one
// also clears its cookie.
func (h *Handler) RevokeSession(ctx context.Context, req *connect.Request[dockwellv1.RevokeSessionRequest]) (*connect.Response[dockwellv1.RevokeSessionResponse], error) {
	client, err := h.client(ctx)
	if err != nil {
		return nil, err
	}
	p, err := principal(ctx)
	if err != nil {
		return nil, err
	}
	if err := h.auth.RevokeSession(ctx, p, req.Msg.GetId()); err != nil {
		return nil, h.rpcError(ctx, err)
	}
	res := connect.NewResponse(&dockwellv1.RevokeSessionResponse{})
	if req.Msg.GetId() == p.Session.ID {
		res.Header().Add("Set-Cookie", client.Site.ClearCookie().String())
	}
	return res, nil
}

// RevokeOtherSessions ends the caller's other sessions.
func (h *Handler) RevokeOtherSessions(ctx context.Context, _ *connect.Request[dockwellv1.RevokeOtherSessionsRequest]) (*connect.Response[dockwellv1.RevokeOtherSessionsResponse], error) {
	p, err := principal(ctx)
	if err != nil {
		return nil, err
	}
	n, err := h.auth.RevokeOtherSessions(ctx, p)
	if err != nil {
		return nil, h.rpcError(ctx, err)
	}
	return connect.NewResponse(&dockwellv1.RevokeOtherSessionsResponse{Revoked: int32(n)}), nil //nolint:gosec // a count of sessions
}

// ListPasskeys lists the caller's passkeys.
func (h *Handler) ListPasskeys(ctx context.Context, _ *connect.Request[dockwellv1.ListPasskeysRequest]) (*connect.Response[dockwellv1.ListPasskeysResponse], error) {
	p, err := principal(ctx)
	if err != nil {
		return nil, err
	}
	all, err := h.auth.ListPasskeys(ctx, p)
	if err != nil {
		return nil, h.rpcError(ctx, err)
	}
	out := make([]*dockwellv1.Passkey, 0, len(all))
	for i := range all {
		out = append(out, passkeyProto(&all[i]))
	}
	return connect.NewResponse(&dockwellv1.ListPasskeysResponse{Passkeys: out}), nil
}

// BeginAddPasskey starts adding a passkey for this address.
func (h *Handler) BeginAddPasskey(ctx context.Context, req *connect.Request[dockwellv1.BeginAddPasskeyRequest]) (*connect.Response[dockwellv1.BeginAddPasskeyResponse], error) {
	client, err := h.client(ctx)
	if err != nil {
		return nil, err
	}
	p, err := principal(ctx)
	if err != nil {
		return nil, err
	}
	cer, err := h.auth.BeginAddPasskey(ctx, client, p, req.Msg.GetProof())
	if err != nil {
		return nil, h.rpcError(ctx, err)
	}
	return connect.NewResponse(&dockwellv1.BeginAddPasskeyResponse{CeremonyId: cer.ID, OptionsJson: string(cer.Options)}), nil
}

// BeginStepUp starts confirming an action with a passkey.
func (h *Handler) BeginStepUp(ctx context.Context, req *connect.Request[dockwellv1.BeginStepUpRequest]) (*connect.Response[dockwellv1.BeginStepUpResponse], error) {
	client, err := h.client(ctx)
	if err != nil {
		return nil, err
	}
	p, err := principal(ctx)
	if err != nil {
		return nil, err
	}
	action, ok := stepUpActions[req.Msg.GetAction()]
	if !ok {
		return nil, Invalid(errors.New("say which action the passkey confirms, such as revealing a secret"))
	}
	cer, err := h.auth.BeginStepUp(ctx, client, p, action, req.Msg.GetTarget())
	if err != nil {
		return nil, h.rpcError(ctx, err)
	}
	return connect.NewResponse(&dockwellv1.BeginStepUpResponse{CeremonyId: cer.ID, OptionsJson: string(cer.Options)}), nil
}

var stepUpActions = map[dockwellv1.StepUpAction]auth.StepUpAction{
	dockwellv1.StepUpAction_STEP_UP_ACTION_REVEAL_SECRET:             auth.StepUpRevealSecret,
	dockwellv1.StepUpAction_STEP_UP_ACTION_ADD_PASSKEY:               auth.StepUpAddPasskey,
	dockwellv1.StepUpAction_STEP_UP_ACTION_REMOVE_PASSKEY:            auth.StepUpRemovePasskey,
	dockwellv1.StepUpAction_STEP_UP_ACTION_SET_UP_CODES:              auth.StepUpSetUpCodes,
	dockwellv1.StepUpAction_STEP_UP_ACTION_DISABLE_CODES:             auth.StepUpDisableCodes,
	dockwellv1.StepUpAction_STEP_UP_ACTION_REGENERATE_RECOVERY_CODES: auth.StepUpRecoveryCodes,
	dockwellv1.StepUpAction_STEP_UP_ACTION_CREATE_TOKEN:              auth.StepUpCreateToken,
}

// FinishStepUp checks the passkey's answer and returns a proof.
func (h *Handler) FinishStepUp(ctx context.Context, req *connect.Request[dockwellv1.FinishStepUpRequest]) (*connect.Response[dockwellv1.FinishStepUpResponse], error) {
	client, err := h.client(ctx)
	if err != nil {
		return nil, err
	}
	p, err := principal(ctx)
	if err != nil {
		return nil, err
	}
	up, err := h.auth.FinishStepUp(ctx, client, p, req.Msg.GetCeremonyId(), []byte(req.Msg.GetCredentialJson()))
	if err != nil {
		return nil, h.rpcError(ctx, err)
	}
	return connect.NewResponse(&dockwellv1.FinishStepUpResponse{Proof: up.Proof, ExpireTime: timestamppb.New(up.Expires)}), nil
}

// FinishAddPasskey saves a new passkey.
func (h *Handler) FinishAddPasskey(ctx context.Context, req *connect.Request[dockwellv1.FinishAddPasskeyRequest]) (*connect.Response[dockwellv1.FinishAddPasskeyResponse], error) {
	client, err := h.client(ctx)
	if err != nil {
		return nil, err
	}
	p, err := principal(ctx)
	if err != nil {
		return nil, err
	}
	m := req.Msg
	pk, err := h.auth.FinishAddPasskey(ctx, client, p, m.GetCeremonyId(), []byte(m.GetCredentialJson()), m.GetName())
	if err != nil {
		return nil, h.rpcError(ctx, err)
	}
	return connect.NewResponse(&dockwellv1.FinishAddPasskeyResponse{Passkey: passkeyProto(&pk)}), nil
}

// RemovePasskey removes a passkey.
func (h *Handler) RemovePasskey(ctx context.Context, req *connect.Request[dockwellv1.RemovePasskeyRequest]) (*connect.Response[dockwellv1.RemovePasskeyResponse], error) {
	p, err := principal(ctx)
	if err != nil {
		return nil, err
	}
	if err := h.auth.RemovePasskey(ctx, p, req.Msg.GetId(), req.Msg.GetProof()); err != nil {
		return nil, h.rpcError(ctx, err)
	}
	return connect.NewResponse(&dockwellv1.RemovePasskeyResponse{}), nil
}

// GetTotp describes code sign-in.
func (h *Handler) GetTotp(ctx context.Context, _ *connect.Request[dockwellv1.GetTotpRequest]) (*connect.Response[dockwellv1.GetTotpResponse], error) {
	p, err := principal(ctx)
	if err != nil {
		return nil, err
	}
	st, err := h.auth.TOTP(ctx, p)
	if err != nil {
		return nil, h.rpcError(ctx, err)
	}
	t := &dockwellv1.Totp{
		Enabled:           st.Enabled,
		RecoveryCodesLeft: int32(st.RecoveryCodesLeft), //nolint:gosec // at most ten
		FailedAttempts:    int32(st.FailedAttempts),    //nolint:gosec // a small count
	}
	if st.LockedAt != nil {
		t.LockTime = timestamppb.New(*st.LockedAt)
	}
	return connect.NewResponse(&dockwellv1.GetTotpResponse{Totp: t}), nil
}

// BeginTotpSetup makes a new authenticator secret.
func (h *Handler) BeginTotpSetup(ctx context.Context, req *connect.Request[dockwellv1.BeginTotpSetupRequest]) (*connect.Response[dockwellv1.BeginTotpSetupResponse], error) {
	p, err := principal(ctx)
	if err != nil {
		return nil, err
	}
	setup, err := h.auth.BeginTOTP(ctx, p, req.Msg.GetProof())
	if err != nil {
		return nil, h.rpcError(ctx, err)
	}
	return connect.NewResponse(&dockwellv1.BeginTotpSetupResponse{Secret: setup.Secret, Uri: setup.URI}), nil
}

// ConfirmTotpSetup turns code sign-in on.
func (h *Handler) ConfirmTotpSetup(ctx context.Context, req *connect.Request[dockwellv1.ConfirmTotpSetupRequest]) (*connect.Response[dockwellv1.ConfirmTotpSetupResponse], error) {
	p, err := principal(ctx)
	if err != nil {
		return nil, err
	}
	codes, err := h.auth.ConfirmTOTP(ctx, p, req.Msg.GetCode())
	if err != nil {
		return nil, h.rpcError(ctx, err)
	}
	return connect.NewResponse(&dockwellv1.ConfirmTotpSetupResponse{RecoveryCodes: codes}), nil
}

// CancelTotpSetup forgets the setup this session began.
func (h *Handler) CancelTotpSetup(ctx context.Context, _ *connect.Request[dockwellv1.CancelTotpSetupRequest]) (*connect.Response[dockwellv1.CancelTotpSetupResponse], error) {
	p, err := principal(ctx)
	if err != nil {
		return nil, err
	}
	if err := h.auth.CancelTOTP(ctx, p); err != nil {
		return nil, h.rpcError(ctx, err)
	}
	return connect.NewResponse(&dockwellv1.CancelTotpSetupResponse{}), nil
}

// DisableTotp turns code sign-in off.
func (h *Handler) DisableTotp(ctx context.Context, req *connect.Request[dockwellv1.DisableTotpRequest]) (*connect.Response[dockwellv1.DisableTotpResponse], error) {
	p, err := principal(ctx)
	if err != nil {
		return nil, err
	}
	if err := h.auth.DisableTOTP(ctx, p, req.Msg.GetProof()); err != nil {
		return nil, h.rpcError(ctx, err)
	}
	return connect.NewResponse(&dockwellv1.DisableTotpResponse{}), nil
}

// RegenerateRecoveryCodes replaces the recovery codes.
func (h *Handler) RegenerateRecoveryCodes(ctx context.Context, req *connect.Request[dockwellv1.RegenerateRecoveryCodesRequest]) (*connect.Response[dockwellv1.RegenerateRecoveryCodesResponse], error) {
	p, err := principal(ctx)
	if err != nil {
		return nil, err
	}
	codes, err := h.auth.RegenerateRecoveryCodes(ctx, p, req.Msg.GetProof())
	if err != nil {
		return nil, h.rpcError(ctx, err)
	}
	return connect.NewResponse(&dockwellv1.RegenerateRecoveryCodesResponse{RecoveryCodes: codes}), nil
}

// UnlockCodeSignIn lifts a lock on code sign-in.
func (h *Handler) UnlockCodeSignIn(ctx context.Context, _ *connect.Request[dockwellv1.UnlockCodeSignInRequest]) (*connect.Response[dockwellv1.UnlockCodeSignInResponse], error) {
	p, err := principal(ctx)
	if err != nil {
		return nil, err
	}
	if err := h.auth.UnlockCodes(ctx, p); err != nil {
		return nil, h.rpcError(ctx, err)
	}
	return connect.NewResponse(&dockwellv1.UnlockCodeSignInResponse{}), nil
}

// withCookie sets the new session's cookie on res.
func withCookie[T any](res *connect.Response[T], site auth.Site, s auth.SignedIn) (*connect.Response[T], error) {
	cookie, err := s.Cookie(site)
	if err != nil {
		return nil, connect.NewError(connect.CodeFailedPrecondition, err)
	}
	res.Header().Add("Set-Cookie", cookie)
	return res, nil
}

// ReasonKey is the error metadata key that names why a call failed, such as
// "code_rejected", so clients can explain it in their own words and
// language. The message says the same in English. AuthService's errors
// carry one, and so does every call refused for want of a session.
const ReasonKey = "Dockwell-Error"

// Reasons for refusing a call on any service of the API listener.
const (
	// ReasonUnauthenticated: the call needs a session and came with none.
	ReasonUnauthenticated = "unauthenticated"
	// ReasonSessionEnded: the call came with a session cookie, but the
	// session ended (signed out, revoked, idle, too old, or made on another
	// address), or a stream's session ended while it was open.
	ReasonSessionEnded = "session_ended"
	// ReasonOrigin: a browser sent the call from another origin, or a write
	// with a session cookie didn't say it came from the panel.
	ReasonOrigin = "origin"
	// ReasonMisdirected: the call was for a host the panel doesn't answer on.
	ReasonMisdirected = "misdirected"
	// ReasonUnavailable: the session or API token couldn't be checked just
	// now.
	ReasonUnavailable = "unavailable"
	// ReasonTokenRejected: the call came with an API token that is unknown,
	// revoked or expired, or a stream's token stopped working while it was
	// open.
	ReasonTokenRejected = "token_rejected"
	// ReasonTokenScope: the call came with a read-only API token, and makes
	// changes.
	ReasonTokenScope = "token_scope"
	// ReasonTokenLogs: the call reads logs, and came with an API token made
	// without the grant to.
	ReasonTokenLogs = "token_logs"
	// ReasonNeedsSession: the call came with an API token, and no token may
	// make it: it is destructive, or manages sign-in or tokens.
	ReasonNeedsSession = "needs_session"
	// ReasonNeedsStepUp: the call needs a fresh passkey assertion for it,
	// from FinishStepUp, and its proof is missing, used, expired, or for
	// something else.
	ReasonNeedsStepUp = "needs_step_up"
	// ReasonNeedsFreshSignIn refuses adding a passkey that no passkey for
	// this address can confirm, in a session not signed in just now. Sign
	// in again, then add it.
	ReasonNeedsFreshSignIn = "needs_fresh_sign_in"
)

// ReasonError is a Connect error that names its reason under ReasonKey.
func ReasonError(code connect.Code, err error, reason string) *connect.Error {
	cerr := connect.NewError(code, err)
	cerr.Meta().Set(ReasonKey, reason)
	return cerr
}

// Invalid is an invalid argument, for the caller to fix: the reason
// "invalid", with a message that says how.
func Invalid(err error) *connect.Error {
	return ReasonError(connect.CodeInvalidArgument, err, "invalid")
}

// codes maps the auth package's errors to API codes and reasons. Their
// messages are written for people; anything else is logged and reported as
// internal.
var codes = []struct {
	err    error
	code   connect.Code
	reason string
}{
	{auth.ErrUnauthenticated, connect.CodeUnauthenticated, ReasonUnauthenticated},
	{auth.ErrPasskeyRejected, connect.CodeUnauthenticated, "passkey_rejected"},
	{auth.ErrPasskeyCopied, connect.CodeUnauthenticated, "passkey_copied"},
	{auth.ErrCodeRejected, connect.CodeUnauthenticated, "code_rejected"},
	{auth.ErrCodeReused, connect.CodeUnauthenticated, "code_reused"},
	{auth.ErrCodesLocked, connect.CodePermissionDenied, "codes_locked"},
	{auth.ErrClaimLinkRejected, connect.CodePermissionDenied, "claim_link_rejected"},
	{auth.ErrSignInLinkRejected, connect.CodePermissionDenied, "sign_in_link_rejected"},
	{auth.ErrClaimed, connect.CodeFailedPrecondition, "claimed"},
	{auth.ErrCeremonyExpired, connect.CodeFailedPrecondition, "ceremony_expired"},
	{auth.ErrLastWayIn, connect.CodeFailedPrecondition, "last_way_in"},
	{auth.ErrNeedsHostname, connect.CodeFailedPrecondition, "needs_hostname"},
	{auth.ErrNoTOTP, connect.CodeFailedPrecondition, "no_totp"},
	{auth.ErrNoPendingTOTP, connect.CodeFailedPrecondition, "no_pending_totp"},
	{auth.ErrTOTPSetupEnded, connect.CodeFailedPrecondition, "totp_setup_ended"},
	{auth.ErrTOTPMismatch, connect.CodeInvalidArgument, "totp_mismatch"},
	{auth.ErrPasskeyExists, connect.CodeAlreadyExists, "passkey_exists"},
	{auth.ErrTooManyRequests, connect.CodeResourceExhausted, "too_many_requests"},
	{auth.ErrNeedsSession, connect.CodePermissionDenied, ReasonNeedsSession},
	{auth.ErrNeedsStepUp, connect.CodePermissionDenied, ReasonNeedsStepUp},
	{auth.ErrNoPasskeyHere, connect.CodeFailedPrecondition, "no_passkey_here"},
	{auth.ErrNeedsFreshSignIn, connect.CodeFailedPrecondition, ReasonNeedsFreshSignIn},
}

func (h *Handler) rpcError(ctx context.Context, err error) error {
	var slow *auth.SlowDown
	if errors.As(err, &slow) {
		cerr := connect.NewError(connect.CodeResourceExhausted, slow)
		cerr.Meta().Set(ReasonKey, "slow_down")
		cerr.Meta().Set("Retry-After", strconv.Itoa(int(slow.RetryAfter().Seconds())))
		return cerr
	}
	for _, c := range codes {
		if errors.Is(err, c.err) {
			cerr := connect.NewError(c.code, c.err)
			cerr.Meta().Set(ReasonKey, c.reason)
			return cerr
		}
	}
	// Wrapped with details for the reader: report only the details.
	for _, c := range []struct {
		err    error
		code   connect.Code
		reason string
	}{{auth.ErrInvalid, connect.CodeInvalidArgument, "invalid"}, {auth.ErrNotFound, connect.CodeNotFound, "not_found"}} {
		if errors.Is(err, c.err) {
			cerr := connect.NewError(c.code, errors.New(strings.TrimPrefix(err.Error(), c.err.Error()+": ")))
			cerr.Meta().Set(ReasonKey, c.reason)
			return cerr
		}
	}
	if errors.Is(err, context.Canceled) {
		return connect.NewError(connect.CodeCanceled, err)
	}
	h.logger.ErrorContext(ctx, "auth call failed", slog.Any("error", err))
	return connect.NewError(connect.CodeInternal, errors.New("internal error"))
}

func userProto(u store.User) *dockwellv1.User {
	role := dockwellv1.Role_ROLE_UNSPECIFIED
	if u.Role == store.RoleOwner {
		role = dockwellv1.Role_ROLE_OWNER
	}
	return &dockwellv1.User{Id: u.ID, Name: u.Name, Email: u.Email, Role: role, CreateTime: timestamppb.New(u.CreatedAt)}
}

func methodProto(m string) dockwellv1.SignInMethod {
	switch m {
	case auth.MethodClaim:
		return dockwellv1.SignInMethod_SIGN_IN_METHOD_CLAIM
	case auth.MethodPasskey:
		return dockwellv1.SignInMethod_SIGN_IN_METHOD_PASSKEY
	case auth.MethodAuthenticator:
		return dockwellv1.SignInMethod_SIGN_IN_METHOD_AUTHENTICATOR_CODE
	case auth.MethodRecoveryCode:
		return dockwellv1.SignInMethod_SIGN_IN_METHOD_RECOVERY_CODE
	case auth.MethodLink:
		return dockwellv1.SignInMethod_SIGN_IN_METHOD_LINK
	default:
		return dockwellv1.SignInMethod_SIGN_IN_METHOD_UNSPECIFIED
	}
}

func passkeyProto(pk *auth.Passkey) *dockwellv1.Passkey {
	out := &dockwellv1.Passkey{
		Id:            pk.ID,
		Name:          pk.Name,
		RpId:          pk.RPID,
		CreateTime:    timestamppb.New(pk.CreatedAt),
		Synced:        pk.Synced(),
		SuspectedCopy: pk.CloneWarningAt != nil,
	}
	if pk.LastUsedAt != nil {
		out.LastUsedTime = timestamppb.New(*pk.LastUsedAt)
	}
	return out
}
