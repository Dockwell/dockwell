// SPDX-License-Identifier: AGPL-3.0-only

package settings

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net"
	"net/netip"
	"slices"
	"strings"
	"time"

	"connectrpc.com/connect"
	"google.golang.org/protobuf/proto"
	"google.golang.org/protobuf/types/known/timestamppb"

	resourcev1 "github.com/dockwell/dockwell/gen/dockwell/resource/v1alpha1"
	dockwellv1 "github.com/dockwell/dockwell/gen/dockwell/v1"
	"github.com/dockwell/dockwell/internal/auth"
	"github.com/dockwell/dockwell/internal/control/authapi"
	"github.com/dockwell/dockwell/internal/id"
	"github.com/dockwell/dockwell/internal/resource/route"
	"github.com/dockwell/dockwell/internal/store"
)

// The panel's domain, set from Settings (ADR 0063).
const (
	// KeyPanelDomain is where the setting is kept.
	KeyPanelDomain = "panel_domain"
	// ActionPanelDomain is its event.
	ActionPanelDomain = "settings.panel_domain"
)

// resolveWait bounds looking a domain up.
const resolveWait = 5 * time.Second

// restartAfter is how long the control plane waits, once a save's answer is
// written, before it restarts, so the answer reaches the panel first.
const restartAfter = 300 * time.Millisecond

// Domain is what setting the panel's domain needs beyond the store.
type Domain struct {
	// Auth checks the passkey a save, a sign-in link and the step-up floor take,
	// and makes the link.
	Auth *auth.Service
	// Resolve looks a name's addresses up, as the control plane resolves names.
	// Nil looks nothing up.
	Resolve func(ctx context.Context, host string) ([]netip.Addr, error)
	// PublicAddresses are the control server's, where a domain's records lead.
	PublicAddresses func() []string
	// PortsHeld says what holds port 80 or 443 on the control server other than
	// Dockwell or its Caddy. Nil reads nothing.
	PortsHeld func(ctx context.Context) []*dockwellv1.PanelPortHolder
	// Restart starts the control plane again, as it was started, to serve a
	// domain saved: it's called once the save's answer is written.
	Restart func()
	// Saved hears a domain as it's saved, so no route claims it before the
	// restart serves it.
	Saved func(domain string)
}

// WithDomain lets the handler set the panel's domain.
func (h *Handler) WithDomain(d Domain) *Handler {
	h.domain = d
	return h
}

// PanelDomain reads the domain the setting holds, as dockwell run does as it
// starts, or "" when none is set.
func PanelDomain(ctx context.Context, db *store.DB) (string, error) {
	var d string
	err := db.View(ctx, func(tx *store.Tx) error {
		var err error
		d, err = getString(ctx, tx, KeyPanelDomain)
		return err
	})
	return d, err
}

// ClearPanelDomain clears the panel's domain from the control server, as
// dockwell panel-domain --clear does when the panel can't be reached to stop
// using it there (ADR 0063), and says what it was. It's on the activity log.
func ClearPanelDomain(ctx context.Context, db *store.DB, actor string, now time.Time) (string, error) {
	var previous string
	err := db.Update(ctx, func(tx *store.Tx) error {
		var err error
		if previous, err = getString(ctx, tx, KeyPanelDomain); err != nil || previous == "" {
			return err
		}
		if err := tx.DeleteSetting(ctx, KeyPanelDomain); err != nil {
			return err
		}
		detail, err := json.Marshal(Change{Previous: previous})
		if err != nil {
			return err
		}
		return tx.RecordEvent(ctx, store.Event{ID: id.New(id.Event), At: now, Actor: actor, Action: ActionPanelDomain, Surface: store.SurfaceCLI, Detail: string(detail)})
	})
	return previous, err
}

// Takeable refuses a domain the panel can't take: one against the rules a
// domain has, and one a route serves, or one a route serves a name under, as a
// passkey for the domain works under it too (ADR 0024). Empty when it can.
func Takeable(ctx context.Context, tx *store.Tx, domain string) (string, error) {
	if err := auth.ValidDomain(domain); err != nil {
		return err.Error(), nil //nolint:nilerr // the refusal is the answer, not a failure to read
	}
	routes, err := tx.ListKind(ctx, string(route.Kind))
	if err != nil {
		return "", err
	}
	for _, r := range routes {
		plain, err := tx.Open(ctx, store.InRevision(r.ID, r.Kind, r.Revision), r.Spec)
		if err != nil {
			return "", fmt.Errorf("can't read the route %s: %w", r.Name, err)
		}
		spec := &resourcev1.Route{}
		if err := proto.Unmarshal(plain, spec); err != nil {
			return "", fmt.Errorf("can't read the route %s: %w", r.Name, err)
		}
		if route.UnderOrIs(spec.GetDomain(), domain) {
			return fmt.Sprintf("the route %s serves %s, so the panel can't take %s: a passkey for that name works under it too. Delete the route first, or give the panel another domain",
				r.Name, spec.GetDomain(), domain), nil
		}
	}
	return "", nil
}

// normalDomain is a domain as it's kept: lower case, with no spaces or final dot.
func normalDomain(d string) string {
	return strings.TrimSuffix(strings.ToLower(strings.TrimSpace(d)), ".")
}

// CheckPanelDomain says whether the panel can take a domain, and what stands in
// the way of its certificate, as it's typed.
func (h *Handler) CheckPanelDomain(ctx context.Context, req *connect.Request[dockwellv1.CheckPanelDomainRequest]) (*connect.Response[dockwellv1.CheckPanelDomainResponse], error) {
	d := normalDomain(req.Msg.GetDomain())
	out := &dockwellv1.CheckPanelDomainResponse{}
	if d == "" {
		return nil, connect.NewError(connect.CodeInvalidArgument, errors.New("a domain to check is needed, such as panel.example.com"))
	}
	if err := h.db.View(ctx, func(tx *store.Tx) error {
		var err error
		out.Refusal, err = Takeable(ctx, tx, d)
		return err
	}); err != nil {
		return nil, err
	}
	if out.Refusal != "" {
		return connect.NewResponse(out), nil
	}
	if h.domain.PublicAddresses != nil {
		out.PublicAddresses = h.domain.PublicAddresses()
	}
	if h.domain.Resolve != nil {
		look, cancel := context.WithTimeout(ctx, resolveWait)
		addrs, err := h.domain.Resolve(look, d)
		cancel()
		switch {
		case err != nil:
			out.DnsProblem = dnsWords(err)
		default:
			for _, a := range addrs {
				a = a.Unmap()
				out.Addresses = append(out.Addresses, a.String())
				out.PointsHere = out.PointsHere || slices.Contains(out.PublicAddresses, a.String())
			}
		}
	}
	if h.domain.PortsHeld != nil {
		out.PortsHeld = h.domain.PortsHeld(ctx)
	}
	return connect.NewResponse(out), nil
}

// dnsWords says why a name didn't resolve, without the resolver's own address.
func dnsWords(err error) string {
	var d *net.DNSError
	switch {
	case errors.As(err, &d) && d.IsNotFound:
		return "it has no A or AAAA record"
	case errors.As(err, &d) && d.IsTimeout, errors.Is(err, context.DeadlineExceeded):
		return "no DNS server answered in time"
	case errors.As(err, &d):
		return d.Err
	}
	return err.Error()
}

// owner refuses anyone but the owner, signed in: the panel's domain decides
// where everyone signs in.
func owner(ctx context.Context, doing string) (auth.Principal, error) {
	return ownerOf(ctx, fmt.Sprintf("only the owner can %s, as the panel's domain decides where everyone signs in, so it didn't happen. Ask the owner", doing))
}

// ownerOf is the owner in a session, or refuses anyone else, saying refused.
func ownerOf(ctx context.Context, refused string) (auth.Principal, error) {
	p, ok := authapi.PrincipalFrom(ctx)
	if !ok {
		return auth.Principal{}, authapi.ReasonError(connect.CodeUnauthenticated, auth.ErrUnauthenticated, authapi.ReasonUnauthenticated)
	}
	if p.Token != nil {
		return auth.Principal{}, authapi.ReasonError(connect.CodePermissionDenied, auth.ErrNeedsSession, authapi.ReasonNeedsSession)
	}
	if p.User.Role != store.RoleOwner {
		return auth.Principal{}, authapi.ReasonError(connect.CodePermissionDenied, errors.New(refused), authapi.ReasonNeedsRole)
	}
	return p, nil
}

// SetPanelDomain saves the panel's domain, or clears it, then restarts the
// control plane to serve it, once the answer is written.
func (h *Handler) SetPanelDomain(ctx context.Context, req *connect.Request[dockwellv1.SetPanelDomainRequest]) (*connect.Response[dockwellv1.SetPanelDomainResponse], error) {
	p, err := owner(ctx, "set the panel's domain")
	if err != nil {
		return nil, err
	}
	if h.serving.From == dockwellv1.PanelDomainFrom_PANEL_DOMAIN_FROM_RUN {
		return nil, authapi.ReasonError(connect.CodeFailedPrecondition, fmt.Errorf(
			"dockwell run was started with the domain %s, by --domain or DOCKWELL_DOMAIN, which wins over Settings, so it didn't change. Change it where dockwell run is started", h.serving.Domain), "domain_from_run")
	}
	d := normalDomain(req.Msg.GetDomain())
	var previous string
	var refusal string
	var applying bool
	if err := h.db.View(ctx, func(tx *store.Tx) error {
		var err error
		if previous, err = getString(ctx, tx, KeyPanelDomain); err != nil {
			return err
		}
		if d != "" {
			if refusal, err = Takeable(ctx, tx, d); err != nil {
				return err
			}
		}
		applying, err = tx.AnyApplying(ctx)
		return err
	}); err != nil {
		return nil, err
	}
	switch {
	case refusal != "":
		return nil, authapi.ReasonError(connect.CodeInvalidArgument, errors.New(refusal), "domain_refused")
	case d == previous:
		s, err := h.current(ctx)
		if err != nil {
			return nil, err
		}
		return connect.NewResponse(&dockwellv1.SetPanelDomainResponse{Settings: s}), nil
	case previous != "" && d != "":
		return nil, authapi.ReasonError(connect.CodeFailedPrecondition, fmt.Errorf(
			"the panel can't move from %s to %s yet: people sign in on %s with passkeys made there, and a move that keeps them signing in comes later. Keep %s for now",
			previous, d, previous, previous), "domain_move")
	case applying:
		return nil, authapi.ReasonError(connect.CodeFailedPrecondition, errors.New(
			"a change is applying, and the restart would stop it partway, so the domain didn't change. Save once it ends"), "change_applying")
	}
	if h.domain.Auth == nil {
		return nil, connect.NewError(connect.CodeUnavailable, errors.New("this control plane can't check a passkey, so the domain didn't change"))
	}
	// The passkey confirms the domain set, or the one cleared.
	target := d
	if target == "" {
		target = previous
	}
	client, _ := authapi.ClientFrom(ctx)
	if err := h.domain.Auth.UseStepUp(ctx, client, p, req.Msg.GetProof(), auth.StepUpSetPanelDomain, target); err != nil {
		return nil, authapi.StepUpRefused(err)
	}
	s, err := h.set(ctx, KeyPanelDomain, ActionPanelDomain, d)
	if err != nil {
		return nil, err
	}
	if h.domain.Saved != nil {
		h.domain.Saved(d)
	}
	restarting := h.domain.Restart != nil
	if restarting {
		// The answer goes out first: the request's context ends once it's written.
		go func() {
			<-ctx.Done()
			time.Sleep(restartAfter)
			h.domain.Restart()
		}()
	}
	return connect.NewResponse(&dockwellv1.SetPanelDomainResponse{Settings: s, Restarting: restarting}), nil
}

// current reads the settings as they are.
func (h *Handler) current(ctx context.Context) (*dockwellv1.Settings, error) {
	var out *dockwellv1.Settings
	err := h.db.View(ctx, func(tx *store.Tx) error {
		var err error
		out, err = h.read(ctx, tx)
		return err
	})
	return out, err
}

// CreatePanelSignInLink makes the owner a one-time link that signs in on the
// panel's domain, once Dockwell holds its certificate.
func (h *Handler) CreatePanelSignInLink(ctx context.Context, req *connect.Request[dockwellv1.CreatePanelSignInLinkRequest]) (*connect.Response[dockwellv1.CreatePanelSignInLinkResponse], error) {
	p, err := owner(ctx, "make a sign-in link to the panel's domain")
	if err != nil {
		return nil, err
	}
	d := h.serving.Domain
	switch {
	case d == "" || h.serving.URL == "":
		return nil, authapi.ReasonError(connect.CodeFailedPrecondition, errors.New(
			"the panel has no domain, so there's nowhere for a link to lead. Set one under Settings, General"), "no_domain")
	case h.serving.Certified != nil && !h.serving.Certified():
		return nil, authapi.ReasonError(connect.CodeFailedPrecondition, fmt.Errorf(
			"there's no certificate for %s yet, so a link there wouldn't open. Ask again once it has one", d), "not_certified")
	case h.domain.Auth == nil:
		return nil, connect.NewError(connect.CodeUnavailable, errors.New("this control plane can't make sign-in links"))
	}
	client, _ := authapi.ClientFrom(ctx)
	if err := h.domain.Auth.UseStepUp(ctx, client, p, req.Msg.GetProof(), auth.StepUpSignInOnPanelDomain, d); err != nil {
		return nil, authapi.StepUpRefused(err)
	}
	link, _, err := h.domain.Auth.CreateSignInLink(ctx, p.User.Email, "user:"+p.User.ID)
	if err != nil {
		return nil, err
	}
	// The token goes after the "#", which no server log or Referer keeps.
	return connect.NewResponse(&dockwellv1.CreatePanelSignInLinkResponse{
		Url:        strings.TrimSuffix(h.serving.URL, "/") + "/recover#" + link.Token,
		ExpireTime: timestamppb.New(link.ExpiresAt),
	}), nil
}
