// SPDX-License-Identifier: AGPL-3.0-only

package servers

import (
	"context"
	"errors"
	"fmt"
	"net/netip"
	"net/url"
	"strings"
	"time"

	"connectrpc.com/connect"
	"google.golang.org/protobuf/types/known/timestamppb"

	dockwellv1 "github.com/dockwell/dockwell/gen/dockwell/v1"
	"github.com/dockwell/dockwell/internal/auth"
	"github.com/dockwell/dockwell/internal/control/authapi"
	"github.com/dockwell/dockwell/internal/control/identity"
	"github.com/dockwell/dockwell/internal/human"
	"github.com/dockwell/dockwell/internal/id"
	"github.com/dockwell/dockwell/internal/resource"
	"github.com/dockwell/dockwell/internal/store"
)

// Tickets lets s make join tickets with ident, each confirmed with a passkey that
// people checks. Without it, CreateJoinTicket and GetJoinTicket say to make one on
// the control server instead.
func (s *Service) Tickets(ident *identity.Authority, people *auth.Service) *Service {
	s.ident, s.people = ident, people
	return s
}

// errNoTickets is what a control plane that can't make tickets says.
var errNoTickets = errors.New("this control plane makes join tickets only on its server: run dockwell token create there")

// CreateJoinTicket makes a ticket that adds one server, once a passkey confirms it.
func (s *Service) CreateJoinTicket(ctx context.Context, req *connect.Request[dockwellv1.CreateJoinTicketRequest]) (*connect.Response[dockwellv1.CreateJoinTicketResponse], error) {
	p, ok := authapi.PrincipalFrom(ctx)
	if !ok {
		return nil, authapi.ReasonError(connect.CodeUnauthenticated, auth.ErrUnauthenticated, authapi.ReasonUnauthenticated)
	}
	if s.ident == nil || s.people == nil {
		return nil, connect.NewError(connect.CodeUnimplemented, errNoTickets)
	}
	ttl := identity.DefaultTicketTTL
	if n := req.Msg.GetTtlSeconds(); n > 0 {
		ttl = time.Duration(n) * time.Second
	}
	if ttl > identity.MaxTicketTTL {
		return nil, authapi.Invalid(fmt.Errorf("a join ticket works for %s at most, not %s. Ask for less", human.Duration(identity.MaxTicketTTL), human.Duration(ttl)))
	}
	// The action is on the person's own account, so its target is them, as
	// BeginStepUp fills it in.
	if req.Msg.GetProof() == "" {
		s.people.AskedForStepUp(p, auth.StepUpAddServer, p.User.ID, func() {})
		return nil, authapi.ReasonError(connect.CodePermissionDenied, auth.ErrNeedsStepUp, authapi.ReasonNeedsStepUp)
	}
	giveBack, err := s.people.HoldStepUp(p, req.Msg.GetProof(), auth.StepUpAddServer, p.User.ID)
	if err != nil {
		return nil, authapi.ReasonError(connect.CodePermissionDenied, err, authapi.ReasonNeedsStepUp)
	}
	urls, err := s.ident.JoinURLs(ctx)
	if err != nil {
		giveBack()
		return nil, err
	}
	others := make([]string, 0, len(urls)-1)
	for _, u := range urls[1:] {
		others = append(others, u.URL)
	}
	tk, err := s.ident.CreateTicket(ctx, urls[0].URL, p.Actor(), ttl, others...)
	if err != nil {
		giveBack()
		return nil, err
	}
	return connect.NewResponse(&dockwellv1.CreateJoinTicketResponse{
		Ticket: &dockwellv1.JoinTicket{
			Id:         tk.TokenID,
			State:      dockwellv1.JoinTicketState_JOIN_TICKET_STATE_WAITING,
			ExpireTime: timestamppb.New(tk.ExpiresAt),
			Addresses:  joinAddresses(urls),
		},
		Value: tk.Value,
	}), nil
}

// GetJoinTicket says where a ticket is in its life, and who joined with it.
func (s *Service) GetJoinTicket(ctx context.Context, req *connect.Request[dockwellv1.GetJoinTicketRequest]) (*connect.Response[dockwellv1.GetJoinTicketResponse], error) {
	if s.ident == nil {
		return nil, connect.NewError(connect.CodeUnimplemented, errNoTickets)
	}
	ref := strings.TrimSpace(req.Msg.GetId())
	if !strings.HasPrefix(ref, id.JoinToken+"_") {
		return nil, authapi.Invalid(fmt.Errorf("%q isn't a join ticket's ID, which starts %s_. Use the one CreateJoinTicket returned", ref, id.JoinToken))
	}
	var tok store.JoinToken
	var name string
	err := s.db.View(ctx, func(tx *store.Tx) (err error) {
		if tok, err = tx.GetJoinToken(ctx, ref); err != nil || tok.UsedBy == "" {
			return err
		}
		// The server that joined, as the store has it: the fleet hears of it only once its agent connects.
		srv, err := tx.GetServer(ctx, tok.UsedBy)
		if errors.Is(err, store.ErrNotFound) {
			return nil
		}
		name = srv.Name
		return err
	})
	if errors.Is(err, store.ErrNotFound) {
		return nil, connect.NewError(connect.CodeNotFound, fmt.Errorf("there's no join ticket %s. Make one with CreateJoinTicket", ref))
	}
	if err != nil {
		return nil, err
	}
	out := &dockwellv1.JoinTicket{Id: tok.ID, ExpireTime: timestamppb.New(tok.ExpiresAt)}
	switch {
	case tok.UsedAt != nil:
		out.State = dockwellv1.JoinTicketState_JOIN_TICKET_STATE_JOINED
		out.ServerId, out.ServerName = tok.UsedBy, name
	case !time.Now().Before(tok.ExpiresAt):
		out.State = dockwellv1.JoinTicketState_JOIN_TICKET_STATE_EXPIRED
	default:
		out.State = dockwellv1.JoinTicketState_JOIN_TICKET_STATE_WAITING
	}
	return connect.NewResponse(&dockwellv1.GetJoinTicketResponse{Ticket: out}), nil
}

// joinAddresses says who reaches each address a ticket names: a name or a public
// address the internet comes in at reaches anywhere; a public address the control
// server goes out from may not lead back in; a private one reaches its own
// network alone. dockwell token create says the same in words.
func joinAddresses(urls []identity.JoinURL) []*dockwellv1.JoinAddress {
	out := make([]*dockwellv1.JoinAddress, 0, len(urls))
	for _, u := range urls {
		out = append(out, &dockwellv1.JoinAddress{Url: u.URL, Reach: joinReach(u)})
	}
	return out
}

func joinReach(u identity.JoinURL) dockwellv1.JoinReach {
	parsed, err := url.Parse(u.URL)
	if err != nil {
		return dockwellv1.JoinReach_JOIN_REACH_UNSPECIFIED
	}
	ip, err := netip.ParseAddr(parsed.Hostname())
	switch {
	case err != nil:
		return dockwellv1.JoinReach_JOIN_REACH_ANYWHERE
	case resource.IsPublicAddress(ip) && u.WayIn:
		return dockwellv1.JoinReach_JOIN_REACH_ANYWHERE
	case resource.IsPublicAddress(ip):
		return dockwellv1.JoinReach_JOIN_REACH_WAY_OUT
	default:
		return dockwellv1.JoinReach_JOIN_REACH_SAME_NETWORK
	}
}
