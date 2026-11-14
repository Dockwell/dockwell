// SPDX-License-Identifier: AGPL-3.0-only

package domains

import (
	"context"
	"encoding/json"
	"errors"
	"slices"
	"time"

	"connectrpc.com/connect"
	"google.golang.org/protobuf/types/known/timestamppb"

	dockwellv1 "github.com/dockwell/dockwell/gen/dockwell/v1"
	"github.com/dockwell/dockwell/internal/control/authapi"
	"github.com/dockwell/dockwell/internal/control/dnsproviders"
	"github.com/dockwell/dockwell/internal/control/place"
	"github.com/dockwell/dockwell/internal/id"
	"github.com/dockwell/dockwell/internal/resource/dnsprovider"
	"github.com/dockwell/dockwell/internal/store"
)

// ListDnsProviders returns the DNS providers an admin connected, never a token. The
// authorization layer trims what's below an admin sees (authapi's trim).
//
//nolint:revive // the generated DomainServiceHandler names it
func (h *Handler) ListDnsProviders(ctx context.Context, _ *connect.Request[dockwellv1.ListDnsProvidersRequest]) (*connect.Response[dockwellv1.ListDnsProvidersResponse], error) {
	out := &dockwellv1.ListDnsProvidersResponse{}
	if h.DNS == nil {
		return connect.NewResponse(out), nil
	}
	shown, err := h.DNS.Shown(ctx)
	if err != nil {
		return nil, err
	}
	for _, p := range shown {
		out.Providers = append(out.Providers, &dockwellv1.DnsProviderInfo{Id: p.ID, Name: p.Name, Provider: p.Provider, Zones: p.Zones})
	}
	return connect.NewResponse(out), nil
}

// CheckDnsProvider checks a token from the control plane, as typed or as a saved
// provider has it, and saves nothing. The token goes to its provider's API alone.
//
//nolint:revive // the generated DomainServiceHandler names it
func (h *Handler) CheckDnsProvider(ctx context.Context, req *connect.Request[dockwellv1.CheckDnsProviderRequest]) (*connect.Response[dockwellv1.CheckDnsProviderResponse], error) {
	if h.DNS == nil {
		return nil, connect.NewError(connect.CodeUnimplemented, errors.New("this control plane keeps no DNS providers"))
	}
	m := req.Msg
	provider, token, zones := m.GetProvider(), m.GetApiToken(), m.GetZones()
	var saved dnsproviders.Saved
	if name := m.GetName(); name != "" {
		var err error
		saved, err = h.DNS.Named(ctx, name)
		if err != nil {
			return nil, connect.NewError(connect.CodeNotFound, err)
		}
		if provider == "" {
			provider = saved.Spec.GetProvider()
		}
		if token == "" {
			if provider != saved.Spec.GetProvider() {
				return nil, authapi.Invalid(errors.New("the check moves the provider to another provider's API, so it needs a token for that one"))
			}
			token = saved.Spec.GetApiToken()
		}
	}
	if provider == "" {
		provider = dnsprovider.Cloudflare
	}
	if token == "" {
		return nil, authapi.Invalid(errors.New("a check needs the provider's API token, or a saved provider's name"))
	}
	clean := make([]string, 0, len(zones))
	for _, z := range zones {
		zone := dnsprovider.Zone(z)
		if zone == "" {
			return nil, authapi.Invalid(errors.New("\"" + z + "\" isn't a zone. Write the domain the provider holds, such as example.com"))
		}
		clean = append(clean, zone)
	}
	if len(clean) > 50 {
		return nil, authapi.Invalid(errors.New("a check writes in up to 50 zones at once"))
	}
	api, err := h.DNS.APIFor(provider, token)
	if err != nil {
		return nil, authapi.Invalid(err)
	}
	r := dnsproviders.Check(ctx, api, clean)
	// A saved provider's token, used again, is on Activity: who, which, and where it
	// wrote. One as typed isn't saved anywhere yet, as a form's test of storage isn't.
	if saved.ID != "" && token == saved.Spec.GetApiToken() {
		if err := h.recordCheck(ctx, saved, clean, r); err != nil {
			return nil, err
		}
	}
	out := &dockwellv1.CheckDnsProviderResponse{State: stateOf(r.State), Reaches: r.Reaches, Detail: r.Detail}
	for _, z := range r.Zones {
		out.Zones = append(out.Zones, &dockwellv1.DnsZoneCheck{Zone: z.Zone, Writes: z.Writes, Detail: z.Detail})
	}
	return connect.NewResponse(out), nil
}

func stateOf(s dnsproviders.State) dockwellv1.DnsProviderState {
	switch s {
	case dnsproviders.Writes:
		return dockwellv1.DnsProviderState_DNS_PROVIDER_STATE_WRITES
	case dnsproviders.Refused:
		return dockwellv1.DnsProviderState_DNS_PROVIDER_STATE_REFUSED
	case dnsproviders.CantWrite:
		return dockwellv1.DnsProviderState_DNS_PROVIDER_STATE_CANT_WRITE
	case dnsproviders.Unreachable:
		return dockwellv1.DnsProviderState_DNS_PROVIDER_STATE_UNREACHABLE
	case dnsproviders.Reaches:
		return dockwellv1.DnsProviderState_DNS_PROVIDER_STATE_REACHES
	}
	return dockwellv1.DnsProviderState_DNS_PROVIDER_STATE_UNSPECIFIED
}

// checked is what Activity keeps of a check of a saved provider: never its token.
type checked struct {
	Zones []string `json:"zones,omitempty"`
	State string   `json:"state"`
}

// recordCheck puts a check of a saved provider on Activity.
func (h *Handler) recordCheck(ctx context.Context, saved dnsproviders.Saved, zones []string, r dnsproviders.Result) error {
	p, ok := authapi.PrincipalFrom(ctx)
	if !ok {
		return nil
	}
	client, _ := authapi.ClientFrom(ctx)
	detail, err := json.Marshal(checked{Zones: zones, State: stateOf(r.State).String()})
	if err != nil {
		return err
	}
	return h.db.Update(context.WithoutCancel(ctx), func(tx *store.Tx) error {
		return tx.RecordEvent(ctx, store.Event{
			ID: id.New(id.Event), At: time.Now(), Actor: p.Actor(), Action: dnsproviders.ActionCheck,
			Subject: saved.ID, Target: saved.ID, Detail: string(detail), Surface: client.Surface(p),
		})
	})
}

// ListCertificates says where each certificate by DNS stands, never its key. A
// server's own is named for its server.
func (h *Handler) ListCertificates(ctx context.Context, _ *connect.Request[dockwellv1.ListCertificatesRequest]) (*connect.Response[dockwellv1.ListCertificatesResponse], error) {
	out := &dockwellv1.ListCertificatesResponse{}
	if h.Certificates == nil {
		return connect.NewResponse(out), nil
	}
	list, err := h.Certificates.List(ctx)
	if err != nil {
		return nil, err
	}
	admin, mine := callerProjects(ctx)
	for i := range list {
		s := &list[i]
		// Below an admin, those that serve a project the caller reads alone; a role
		// on the whole install reads them all.
		if !admin && mine != nil && !slices.ContainsFunc(s.Projects, func(p string) bool { return mine[p] }) {
			continue
		}
		c := &dockwellv1.CertificateInfo{Id: s.Holder, Name: s.Name, Domains: s.Names, DnsProvider: s.Provider, Message: s.Message}
		if h.servers != nil {
			if srv, err := h.servers.Get(s.Holder); err == nil && srv != nil {
				c.Name = srv.GetName()
			}
		}
		switch {
		case s.Issued:
			c.State = dockwellv1.CertificateState_CERTIFICATE_STATE_ISSUED
			c.ExpireTime = timestamppb.New(s.NotAfter)
		case s.Failed:
			c.State = dockwellv1.CertificateState_CERTIFICATE_STATE_FAILED
		default:
			c.State = dockwellv1.CertificateState_CERTIFICATE_STATE_PENDING
		}
		if !s.Next.IsZero() {
			c.NextTime = timestamppb.New(s.Next)
		}
		out.Certificates = append(out.Certificates, c)
	}
	return connect.NewResponse(out), nil
}

// callerProjects says whether the caller is an admin of the install, and otherwise
// which projects they hold a role in: nil for a role on the whole install.
func callerProjects(ctx context.Context) (admin bool, projects map[string]bool) {
	c, ok := place.CallerFrom(ctx)
	if !ok {
		return true, nil
	}
	if store.RoleRank(place.Holds(c.Grants, place.Install)) >= store.RoleRank(store.RoleAdmin) {
		return true, nil
	}
	projects = map[string]bool{}
	for _, g := range c.Grants {
		if g.ProjectID == store.Install {
			return false, nil
		}
		projects[g.ProjectID] = true
	}
	return false, projects
}
