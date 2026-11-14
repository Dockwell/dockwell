// SPDX-License-Identifier: AGPL-3.0-only

package control

import (
	"slices"
	"strings"
	"testing"

	"connectrpc.com/connect"
	"google.golang.org/protobuf/encoding/protojson"

	resourcev1 "github.com/dockwell/dockwell/gen/dockwell/resource/v1alpha1"
	dockwellv1 "github.com/dockwell/dockwell/gen/dockwell/v1"
	"github.com/dockwell/dockwell/gen/dockwell/v1/dockwellv1connect"
	"github.com/dockwell/dockwell/internal/resource/dnsprovider"
	"github.com/dockwell/dockwell/internal/store"
)

// dnsToken is the token Cloudflare's stand-in takes, for example.com and
// example.org.
const dnsToken = "cf-dns-token-s3cret-0123456789abcdefghij"

func dnsEdit(name string, p *resourcev1.DnsProvider) *dockwellv1.Edit {
	return &dockwellv1.Edit{
		Resource: &dockwellv1.ResourceRef{Kind: string(dnsprovider.Kind), Name: name},
		Spec:     &resourcev1.Spec{Kind: &resourcev1.Spec_DnsProvider{DnsProvider: p}},
	}
}

// An admin checks a DNS provider's token live, which writes a TXT record in each
// zone and deletes it, and saves the provider as a change. Its token is in no plan,
// change, event or answer; a developer sees names and zones alone, and can't check
// or save one (ADR 0088).
func TestAnAdminConnectsADNSProvider(t *testing.T) {
	f := startFull(t, store.SQLite, nil)
	b := signedIn(t, f)
	c := changeClient(b, b.origin)
	dc := dockwellv1connect.NewDomainServiceClient(b, b.origin, connect.WithProtoJSON())

	// As typed: the zones the token reaches, then a record written and deleted.
	listed, err := dc.CheckDnsProvider(t.Context(), connect.NewRequest(&dockwellv1.CheckDnsProviderRequest{Provider: "cloudflare", ApiToken: dnsToken}))
	if err != nil || listed.Msg.GetState() != dockwellv1.DnsProviderState_DNS_PROVIDER_STATE_REACHES || !slices.Equal(listed.Msg.GetReaches(), []string{"example.com", "example.org"}) {
		t.Fatalf("a check with no zone = %v, %v", listed, err)
	}
	wrote, err := dc.CheckDnsProvider(t.Context(), connect.NewRequest(&dockwellv1.CheckDnsProviderRequest{Provider: "cloudflare", ApiToken: dnsToken, Zones: []string{"Example.com"}}))
	if err != nil || wrote.Msg.GetState() != dockwellv1.DnsProviderState_DNS_PROVIDER_STATE_WRITES {
		t.Fatalf("a check of example.com = %v, %v", wrote, err)
	}
	if left := f.cloudflare.Records("example.com"); len(left) != 0 {
		t.Errorf("the check left %v in the zone", left)
	}
	wrong, err := dc.CheckDnsProvider(t.Context(), connect.NewRequest(&dockwellv1.CheckDnsProviderRequest{Provider: "cloudflare", ApiToken: "not-the-token", Zones: []string{"example.com"}}))
	if err != nil || wrong.Msg.GetState() != dockwellv1.DnsProviderState_DNS_PROVIDER_STATE_REFUSED || !strings.Contains(wrong.Msg.GetDetail(), "Invalid API Token") {
		t.Errorf("a wrong token = %v, %v", wrong, err)
	}

	planned := propose(t, c, "Write DNS through Cloudflare", dnsEdit("cloudflare", &resourcev1.DnsProvider{Provider: "cloudflare", Zones: []string{"example.com"}, ApiToken: dnsToken}))
	done := apply(t, c, planned)
	for _, text := range []string{protojson.Format(planned), protojson.Format(done)} {
		if strings.Contains(text, dnsToken) {
			t.Errorf("a change holds the DNS token: %s", text)
		}
	}
	events, err := eventClient(b, b.origin).ListEvents(t.Context(), connect.NewRequest(&dockwellv1.ListEventsRequest{}))
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(protojson.Format(events.Msg), dnsToken) {
		t.Error("Activity holds the DNS token")
	}

	// A saved provider is checked again with its own token, which is on Activity:
	// who checked which provider, and where it wrote (review of #605, L5).
	again, err := dc.CheckDnsProvider(t.Context(), connect.NewRequest(&dockwellv1.CheckDnsProviderRequest{Name: "cloudflare", Zones: []string{"example.com"}}))
	if err != nil || again.Msg.GetState() != dockwellv1.DnsProviderState_DNS_PROVIDER_STATE_WRITES {
		t.Errorf("checking the saved provider again = %v, %v", again, err)
	}
	checkedEvents, err := eventClient(b, b.origin).ListEvents(t.Context(), connect.NewRequest(&dockwellv1.ListEventsRequest{}))
	if err != nil {
		t.Fatal(err)
	}
	var check *dockwellv1.Event
	for _, e := range checkedEvents.Msg.GetEvents() {
		if e.GetAction() == "dns_provider.check" {
			check = e
		}
	}
	if check == nil || check.GetSubject().GetName() != "cloudflare" {
		t.Errorf("Activity says nothing of the saved provider's check: %v", check)
	}
	if strings.Contains(protojson.Format(checkedEvents.Msg), dnsToken) {
		t.Error("Activity holds the DNS token after a check")
	}
	// Its zones change without the token again.
	apply(t, c, propose(t, c, "And example.org", dnsEdit("cloudflare", &resourcev1.DnsProvider{Provider: "cloudflare", Zones: []string{"example.com", "example.org"}})))
	providers, err := dc.ListDnsProviders(t.Context(), connect.NewRequest(&dockwellv1.ListDnsProvidersRequest{}))
	if err != nil {
		t.Fatal(err)
	}
	if p := providers.Msg.GetProviders(); len(p) != 1 || p[0].GetName() != "cloudflare" || p[0].GetProvider() != "cloudflare" || !slices.Equal(p[0].GetZones(), []string{"example.com", "example.org"}) {
		t.Errorf("an admin lists %v", p)
	}
	if strings.Contains(protojson.Format(providers.Msg), dnsToken) {
		t.Error("the list holds the DNS token")
	}
	kept, err := dc.CheckDnsProvider(t.Context(), connect.NewRequest(&dockwellv1.CheckDnsProviderRequest{Name: "cloudflare", Zones: []string{"example.org"}}))
	if err != nil || kept.Msg.GetState() != dockwellv1.DnsProviderState_DNS_PROVIDER_STATE_WRITES {
		t.Errorf("the token wasn't kept through the update: %v, %v", kept, err)
	}

	// Below an admin: names and zones alone, nothing to check or save.
	hold := aMember(t, f)
	hold(onInstall(store.RoleDeveloper))
	trimmed, err := dc.ListDnsProviders(t.Context(), connect.NewRequest(&dockwellv1.ListDnsProvidersRequest{}))
	if err != nil {
		t.Fatal(err)
	}
	if p := trimmed.Msg.GetProviders(); len(p) != 1 || p[0].GetName() != "cloudflare" || len(p[0].GetZones()) != 2 || p[0].GetProvider() != "" {
		t.Errorf("a developer lists %v", p)
	}
	_, err = dc.CheckDnsProvider(t.Context(), connect.NewRequest(&dockwellv1.CheckDnsProviderRequest{Name: "cloudflare"}))
	needsRole(t, "a developer checking a DNS provider", err)
	_, err = c.ProposeChange(t.Context(), connect.NewRequest(&dockwellv1.ProposeChangeRequest{Summary: "Mine", Edits: []*dockwellv1.Edit{
		dnsEdit("mine", &resourcev1.DnsProvider{Provider: "cloudflare", Zones: []string{"example.com"}, ApiToken: dnsToken}),
	}}))
	if err == nil {
		t.Error("a developer planned a DNS provider")
	}
}

// A Global API Key can do anything the account can, so it's refused as a provider's
// token wherever one is saved, not only by the live check (review of #605, M2).
func TestAGlobalAPIKeyIsNeverSaved(t *testing.T) {
	f := startFull(t, store.SQLite, nil)
	b := signedIn(t, f)
	c := changeClient(b, b.origin)
	const globalKey = "0123456789abcdef0123456789abcdef01234"
	_, err := c.ProposeChange(t.Context(), connect.NewRequest(&dockwellv1.ProposeChangeRequest{Summary: "Global", Edits: []*dockwellv1.Edit{
		dnsEdit("cloudflare", &resourcev1.DnsProvider{Provider: "cloudflare", Zones: []string{"example.com"}, ApiToken: globalKey}),
	}}))
	if connect.CodeOf(err) != connect.CodeInvalidArgument || !strings.Contains(err.Error(), "Global API Key") || strings.Contains(err.Error(), globalKey) {
		t.Errorf("planning a provider with a Global API Key = %v", err)
	}
	for _, a := range f.cloudflare.Asked {
		t.Errorf("a plan reached Cloudflare: %s", a)
	}
}
