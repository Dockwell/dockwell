// SPDX-License-Identifier: AGPL-3.0-only

package control

import (
	"strings"
	"testing"

	"connectrpc.com/connect"
	"google.golang.org/protobuf/encoding/protojson"
	"google.golang.org/protobuf/proto"

	resourcev1 "github.com/dockwell/dockwell/gen/dockwell/resource/v1alpha1"
	dockwellv1 "github.com/dockwell/dockwell/gen/dockwell/v1"
	"github.com/dockwell/dockwell/gen/dockwell/v1/dockwellv1connect"
	"github.com/dockwell/dockwell/internal/control/dnsproviders/cftest"
	"github.com/dockwell/dockwell/internal/store"
)

// tunnelToken is a second token, for tunnels alone.
const tunnelToken = "cf-tunnel-token-s3cret-0123456789abcdefg"

func capabilityOf(r *dockwellv1.CheckDnsProviderResponse, c dockwellv1.Capability) *dockwellv1.ProviderCapability {
	for _, pc := range r.GetCapabilities() {
		if pc.GetCapability() == c {
			return pc
		}
	}
	return nil
}

// A provider's check says what its token can do for publishing, each right on its
// own, read from its own record, with each zone's SSL mode and account; a second
// token answers for tunnels; no token is in any answer (ADR 0104, section 6).
func TestTheCheckSaysWhatATokenCanPublish(t *testing.T) {
	t.Parallel()
	f := startFull(t, store.SQLite, nil)
	b := signedIn(t, f)
	dc := dockwellv1connect.NewDomainServiceClient(b, b.origin, connect.WithProtoJSON())
	cf := f.cloudflare
	cf.Tokens[dnsToken] = cftest.Token{
		Reads: []string{"example.com", "example.org"}, Writes: []string{"example.com", "example.org"}, Account: true,
		Rights: []string{"Zone Read", "DNS Write", "Zone Settings Read", "Account API Tokens Read"},
	}
	cf.SSL = map[string]string{"example.org": "flexible"}
	cf.AddTunnel(&cftest.Tunnel{ID: "327d9d41-b009-413d-8494-7eeeefb3ed2a", Name: "web-1"})
	cf.AddTunnel(&cftest.Tunnel{ID: "427d9d41-b009-413d-8494-7eeeefb3ed2a", Name: "web-2"})

	got, err := dc.CheckDnsProvider(t.Context(), connect.NewRequest(&dockwellv1.CheckDnsProviderRequest{Provider: "cloudflare", ApiToken: dnsToken, Zones: []string{"example.com", "example.org"}}))
	if err != nil {
		t.Fatal(err)
	}
	m := got.Msg
	if own := capabilityOf(m, dockwellv1.Capability_CAPABILITY_OWN_RIGHTS); own.GetState() != dockwellv1.CapabilityState_CAPABILITY_STATE_HAS {
		t.Errorf("own rights: %v", own)
	}
	tun := capabilityOf(m, dockwellv1.Capability_CAPABILITY_TUNNELS)
	if tun.GetState() != dockwellv1.CapabilityState_CAPABILITY_STATE_LACKS || tun.GetDetail() != "The token lacks Cloudflare Tunnel edit" ||
		tun.GetFixUrl() != "https://dash.cloudflare.com/"+cftest.Account+"/api-tokens" {
		t.Errorf("tunnels: %v", tun)
	}
	if m.GetMakesTokens() {
		t.Error("a token without API Tokens Write doesn't make tokens")
	}
	for _, z := range m.GetZones() {
		want := map[string]string{"example.com": "full", "example.org": "flexible"}[z.GetZone()]
		if z.GetSslMode() != want || z.GetAccountId() != cftest.Account || !z.GetWrites() {
			t.Errorf("zone %v", z)
		}
	}

	// Saved with a second token for tunnels, the check of the saved provider has it
	// answer for them, and nothing shows either token.
	cf.Tokens[tunnelToken] = cftest.Token{Reads: []string{"example.com"}, Account: true, Rights: []string{"Cloudflare Tunnel Write", "Account API Tokens Read"}}
	c := changeClient(b, b.origin)
	apply(t, c, propose(t, c, "Write DNS and publish through Cloudflare", dnsEdit("cloudflare", &resourcev1.DnsProvider{
		Provider: "cloudflare", Zones: []string{"example.com"}, ApiToken: dnsToken, TunnelApiToken: tunnelToken,
	})))
	saved, err := dc.CheckDnsProvider(t.Context(), connect.NewRequest(&dockwellv1.CheckDnsProviderRequest{Name: "cloudflare", Zones: []string{"example.com"}}))
	if err != nil {
		t.Fatal(err)
	}
	if tun := capabilityOf(saved.Msg, dockwellv1.Capability_CAPABILITY_TUNNELS); tun.GetState() != dockwellv1.CapabilityState_CAPABILITY_STATE_HAS || saved.Msg.GetTunnels() != 2 {
		t.Errorf("with a second token: %v, %d tunnels", tun, saved.Msg.GetTunnels())
	}
	listed, err := dc.ListDnsProviders(t.Context(), connect.NewRequest(&dockwellv1.ListDnsProvidersRequest{}))
	if err != nil || len(listed.Msg.GetProviders()) != 1 || !listed.Msg.GetProviders()[0].GetTunnelApiTokenSet() {
		t.Fatalf("listed: %v %v", listed, err)
	}
	for _, msg := range []proto.Message{m, saved.Msg, listed.Msg} {
		text := protojson.Format(msg)
		if strings.Contains(text, dnsToken) || strings.Contains(text, tunnelToken) {
			t.Fatalf("a token in an answer: %s", text)
		}
	}
}
