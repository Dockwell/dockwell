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
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"sync"
	"time"

	"connectrpc.com/connect"
	"google.golang.org/protobuf/types/known/durationpb"

	agentv1 "github.com/dockwell/dockwell/gen/dockwell/agent/v1"
	"github.com/dockwell/dockwell/gen/dockwell/agent/v1/agentv1connect"
	"github.com/dockwell/dockwell/internal/buildinfo"
	"github.com/dockwell/dockwell/internal/control/fleet"
	"github.com/dockwell/dockwell/internal/control/identity"
	"github.com/dockwell/dockwell/internal/human"
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
	fleet  *fleet.Hub
	agents *Agents
	logger *slog.Logger
	// silence is how long a stream may say nothing before it is closed, and
	// recheck how often a connected server's revocation is checked.
	silence time.Duration
	recheck time.Duration
}

var _ agentv1connect.AgentServiceHandler = (*Handler)(nil)

// Stream timing.
const (
	// UnreachableAfter is how long an agent may stay silent before its
	// stream is closed. Agents send stats every fleet.StatsInterval.
	UnreachableAfter = 30 * time.Second
	revocationCheck  = 30 * time.Second
)

// Option adjusts a Handler.
type Option func(*Handler)

// WithStreamTiming sets how long a stream may stay silent, and how often a
// connected server's revocation is checked.
func WithStreamTiming(silence, recheck time.Duration) Option {
	return func(h *Handler) { h.silence, h.recheck = silence, recheck }
}

// WithAgents makes each stream's agent reachable through agents, for the
// engine's calls. Without it, streams only report.
func WithAgents(agents *Agents) Option {
	return func(h *Handler) { h.agents = agents }
}

// NewHandler returns the agent listener's root handler.
func NewHandler(auth *identity.Authority, hub *fleet.Hub, logger *slog.Logger, opts ...Option) http.Handler {
	h := &Handler{auth: auth, fleet: hub, logger: logger, silence: UnreachableAfter, recheck: revocationCheck}
	for _, o := range opts {
		o(h)
	}
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
	// Listed straight away, as waiting for its agent, rather than when it first connects.
	h.fleet.Joined(joined.Server)
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

// Connect keeps a server's stream: a hello, then stats, until the agent
// goes away, goes quiet, or the server is removed.
func (h *Handler) Connect(ctx context.Context, stream *connect.BidiStream[agentv1.ConnectRequest, agentv1.ConnectResponse]) error {
	peer, err := requireServer(ctx)
	if err != nil {
		return err
	}
	srv, err := h.auth.CheckConnection(ctx, peer.ServerID, peer.Serial)
	if err != nil {
		return h.refuse(ctx, peer, err)
	}

	// Receive in the background, so silence and revocation can end the
	// stream, before the hello as much as after it.
	msgs := make(chan *agentv1.ConnectRequest)
	recvErr := make(chan error, 1)
	go func() {
		for {
			msg, err := stream.Receive()
			if err != nil {
				recvErr <- err
				return
			}
			select {
			case msgs <- msg:
			case <-ctx.Done():
				return
			}
		}
	}()
	silence := time.NewTimer(h.silence)
	defer silence.Stop()
	silent := func() error {
		return connect.NewError(connect.CodeDeadlineExceeded,
			fmt.Errorf("the agent sent nothing for %s, so the control plane closed the stream", human.Duration(h.silence)))
	}

	var hello *agentv1.Hello
	select {
	case <-ctx.Done():
		return nil
	case err := <-recvErr:
		if errors.Is(err, io.EOF) {
			return nil
		}
		return err
	case <-silence.C:
		return silent()
	case first := <-msgs:
		if hello = first.GetHello(); hello == nil {
			return connect.NewError(connect.CodeInvalidArgument, errors.New("a stream must start with a hello"))
		}
	}
	silence.Reset(h.silence)

	detach, err := h.fleet.Attach(ctx, srv, hello.GetInventory(), hello.GetBuild())
	if err != nil {
		return h.rpcError(ctx, err)
	}
	defer detach()
	if err := stream.Send(&agentv1.ConnectResponse{Message: &agentv1.ConnectResponse_Welcome{Welcome: &agentv1.Welcome{
		ServerName:    srv.Name,
		StatsInterval: durationpb.New(fleet.StatsInterval),
	}}}); err != nil {
		return err
	}
	log := h.logger.With(slog.String("server", peer.ServerID), slog.String("name", srv.Name))
	log.InfoContext(ctx, "server connected", slog.String("agent", hello.GetBuild().GetVersion()),
		slog.String("build", string(buildinfo.EditionOf(hello.GetBuild()))), slog.Any("kinds", hello.GetKinds()),
		slog.Int("unavailable_kinds", len(hello.GetUnavailable())))
	// The engine reaches the agent's drivers over the stream from now on.
	// Sends after the welcome go through the session, one at a time.
	var session *Session
	if h.agents != nil {
		session = h.agents.open(peer.ServerID, srv.Name, hello, stream.Send)
		defer session.close()
	}

	recheck := time.NewTicker(h.recheck)
	defer recheck.Stop()
	for {
		select {
		case <-ctx.Done():
			return nil
		case err := <-recvErr:
			if errors.Is(err, io.EOF) {
				return nil
			}
			return err
		case msg := <-msgs:
			silence.Reset(h.silence)
			switch {
			case msg.GetStats() != nil:
				h.fleet.Record(peer.ServerID, msg.GetStats())
			case msg.GetResult() != nil && session != nil:
				session.deliver(msg.GetResult())
			case msg.GetProgress() != nil && session != nil:
				session.progressed(msg.GetProgress())
			case msg.GetKinds() != nil && session != nil:
				k := msg.GetKinds()
				grew := session.setKinds(k.GetKinds(), k.GetUnavailable())
				log.InfoContext(ctx, "server's kinds changed", slog.Any("kinds", k.GetKinds()), slog.Int("unavailable_kinds", len(k.GetUnavailable())))
				if grew {
					// What waited for a kind it can run now, such as a
					// rollback waiting for Docker, goes on, as when it
					// connects.
					h.agents.connected(peer.ServerID)
				}
			case msg.GetDraining() != nil && session != nil:
				session.drain()
			case msg.GetDraining() != nil:
				// No calls go on a stream without a session, so it is
				// drained already. Answer at once, or the agent waits its
				// whole drain limit before it moves to the new stream.
				if err := stream.Send(&agentv1.ConnectResponse{Message: &agentv1.ConnectResponse_Drained{Drained: &agentv1.Drained{}}}); err != nil {
					return err
				}
			}
		case <-silence.C:
			log.WarnContext(ctx, "closing a silent stream", slog.String("silent_for", human.Duration(h.silence)))
			return silent()
		case <-recheck.C:
			_, err := h.auth.CheckConnection(ctx, peer.ServerID, peer.Serial)
			if errors.Is(err, identity.ErrRevoked) || errors.Is(err, identity.ErrUnknownServer) || errors.Is(err, identity.ErrStaleCertificate) {
				log.InfoContext(ctx, "closing a stream the server may no longer hold", slog.String("reason", err.Error()))
				return h.refuse(ctx, peer, err)
			}
		}
	}
}

// refuse answers a stream the server may not hold. A server that was
// removed leaves the fleet too, so watchers drop it rather than show it as
// unreachable.
func (h *Handler) refuse(ctx context.Context, peer Peer, err error) error {
	if errors.Is(err, identity.ErrRevoked) || errors.Is(err, identity.ErrUnknownServer) {
		h.fleet.Removed(peer.ServerID)
	}
	return h.rpcError(ctx, err)
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
