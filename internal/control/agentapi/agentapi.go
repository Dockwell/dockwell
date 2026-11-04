// SPDX-License-Identifier: AGPL-3.0-only

// Package agentapi serves dockwell.agent.v1.AgentService on the control
// plane's agent listener.
//
// The listener asks every client for a certificate and verifies any it is
// given against the install's CA. Join is the one method that works
// without one; every other method needs a verified server certificate,
// and the server it names must still be active.
package agentapi

import (
	"context"
	"crypto/tls"
	"errors"
	"log/slog"
	"net/http"
	"sync"
	"time"

	"connectrpc.com/connect"

	agentv1 "github.com/dockwell/dockwell/gen/dockwell/agent/v1"
	"github.com/dockwell/dockwell/gen/dockwell/agent/v1/agentv1connect"
	"github.com/dockwell/dockwell/internal/control/identity"
	"github.com/dockwell/dockwell/internal/pki"
	"github.com/dockwell/dockwell/internal/serve"
)

type peerKey struct{}

// Peer is the server a request's verified client certificate names.
type Peer struct {
	ServerID string
	// Serial is the serial of the certificate it connected with.
	Serial string
}

// PeerFrom returns the verified server behind a request, if any.
func PeerFrom(ctx context.Context) (Peer, bool) {
	p, ok := ctx.Value(peerKey{}).(Peer)
	return p, ok
}

// Handler implements AgentService.
type Handler struct {
	auth   *identity.Authority
	logger *slog.Logger
}

var _ agentv1connect.AgentServiceHandler = (*Handler)(nil)

// NewHandler returns the agent listener's root handler.
func NewHandler(auth *identity.Authority, logger *slog.Logger) http.Handler {
	h := &Handler{auth: auth, logger: logger}
	mux := http.NewServeMux()
	mux.Handle(agentv1connect.NewAgentServiceHandler(h, serve.HandlerOptions(logger)...))
	return serve.Recover(logger, identify(auth, mux))
}

// identify puts the server a verified client certificate names into the
// request's context. Revocation is checked by the methods that need it.
func identify(auth *identity.Authority, next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.TLS != nil && len(r.TLS.VerifiedChains) > 0 {
			leaf := r.TLS.VerifiedChains[0][0]
			if id, err := auth.CA().ServerID(leaf); err == nil {
				p := Peer{ServerID: id, Serial: pki.SerialString(leaf)}
				r = r.WithContext(context.WithValue(r.Context(), peerKey{}, p))
			}
		}
		next.ServeHTTP(w, r)
	})
}

// Join spends a join ticket for a new server.
func (h *Handler) Join(ctx context.Context, req *connect.Request[agentv1.JoinRequest]) (*connect.Response[agentv1.JoinResponse], error) {
	m := req.Msg
	joined, err := h.auth.Join(ctx, m.GetTokenId(), m.GetSecret(), m.GetCsr(), m.GetHostname(), req.Peer().Addr)
	if err != nil {
		return nil, h.rpcError(ctx, err)
	}
	return connect.NewResponse(&agentv1.JoinResponse{
		ServerId:      joined.Server.ID,
		ServerName:    joined.Server.Name,
		Certificate:   joined.Cert.Raw,
		CaCertificate: h.auth.CA().Certificate().Raw,
	}), nil
}

// RenewCertificate signs a fresh certificate for the calling server.
func (h *Handler) RenewCertificate(ctx context.Context, req *connect.Request[agentv1.RenewCertificateRequest]) (*connect.Response[agentv1.RenewCertificateResponse], error) {
	peer, err := requireServer(ctx)
	if err != nil {
		return nil, err
	}
	cert, err := h.auth.Renew(ctx, peer.ServerID, peer.Serial, req.Msg.GetCsr())
	if err != nil {
		return nil, h.rpcError(ctx, err)
	}
	h.logger.InfoContext(ctx, "server renewed its certificate", slog.String("server", peer.ServerID), slog.Time("not_after", cert.NotAfter))
	return connect.NewResponse(&agentv1.RenewCertificateResponse{Certificate: cert.Raw}), nil
}

func requireServer(ctx context.Context) (Peer, error) {
	p, ok := PeerFrom(ctx)
	if !ok {
		return Peer{}, connect.NewError(connect.CodeUnauthenticated,
			errors.New("this method needs the server's certificate. Join the server first with dockwell join"))
	}
	return p, nil
}

// rpcError maps the authority's errors to codes. Anything unexpected is a
// fault on this side: it is logged, and the caller, who may not be
// authenticated, learns nothing about it.
func (h *Handler) rpcError(ctx context.Context, err error) error {
	switch {
	case errors.Is(err, identity.ErrTicketRejected), errors.Is(err, identity.ErrRevoked),
		errors.Is(err, identity.ErrUnknownServer), errors.Is(err, identity.ErrStaleCertificate):
		return connect.NewError(connect.CodePermissionDenied, err)
	case errors.Is(err, identity.ErrBadRequest):
		return connect.NewError(connect.CodeInvalidArgument, err)
	default:
		h.logger.ErrorContext(ctx, "agent request failed", slog.String("error", err.Error()))
		return connect.NewError(connect.CodeInternal, errors.New("the control plane couldn't complete the request; try again, and check its logs if it keeps failing"))
	}
}

// TLSConfig is the agent listener's TLS configuration: TLS 1.3, a serving
// certificate from the install's CA for hosts, and client certificates
// verified against the same CA when given.
func TLSConfig(auth *identity.Authority, hosts []string) *tls.Config {
	sc := &servingCert{ca: auth.CA(), hosts: hosts, now: time.Now}
	return &tls.Config{
		MinVersion:     tls.VersionTLS13,
		ClientAuth:     tls.VerifyClientCertIfGiven,
		ClientCAs:      auth.CA().Pool(),
		GetCertificate: sc.get,
	}
}

// servingCert issues the agent listener's certificate and reissues it when
// it is two thirds through its life, so a long-running control plane never
// serves an expired one.
type servingCert struct {
	ca    *pki.CA
	hosts []string
	now   func() time.Time

	mu   sync.Mutex
	cert *tls.Certificate
}

func (s *servingCert) get(*tls.ClientHelloInfo) (*tls.Certificate, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.cert == nil || !s.now().Before(pki.RenewAt(s.cert.Leaf)) {
		cert, err := s.ca.IssueServing(s.hosts, s.now())
		if err != nil {
			return nil, err
		}
		s.cert = &cert
	}
	return s.cert, nil
}
