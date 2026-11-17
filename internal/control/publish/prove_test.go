// SPDX-License-Identifier: AGPL-3.0-only

package publish

import (
	"context"
	"net/http/httptest"
	"strings"
	"testing"

	dockwellv1 "github.com/dockwell/dockwell/gen/dockwell/v1"
	"github.com/dockwell/dockwell/internal/control/dnsproviders"
	"github.com/dockwell/dockwell/internal/control/dnsproviders/cftest"
)

const (
	proveToken  = "cf-token-0123456789abcdefghijklmnopqrstu"
	proveTunnel = "327d9d41-b009-413d-8494-7eeeefb3ed2a"
)

// proving is a Publisher whose server runs the runs given, against Cloudflare's
// stand-in holding the tunnel with connectors.
func proving(t *testing.T, runs []*dockwellv1.CloudflaredTunnel, connectors ...cftest.Connector) (*Publisher, *target) {
	t.Helper()
	api := cftest.New(proveToken, "example.com")
	api.Tokens[proveToken] = cftest.Token{Reads: []string{"example.com"}, Writes: []string{"example.com"}, Account: true, Rights: []string{"Zone Read", "DNS Write", "Cloudflare Tunnel Write"}}
	api.AddTunnel(&cftest.Tunnel{ID: proveTunnel, Name: "web-1", Connectors: connectors})
	srv := httptest.NewServer(api)
	t.Cleanup(srv.Close)
	cf := &dnsproviders.Cloudflare{Token: proveToken, Base: srv.URL + "/client/v4", Client: srv.Client()}
	p := &Publisher{Tunnels: func(context.Context, string) ([]*dockwellv1.CloudflaredTunnel, error) { return runs, nil }}
	return p, &target{cf: cf, tunnels: cf, zone: dnsproviders.Zone{ID: "zone-example-com", Name: "example.com", Account: cftest.Account}}
}

// The run whose connector Cloudflare lists is the one proved, whatever its PID: a
// process of a lower PID with no /ready decides nothing (the review's M2).
func TestTheProofTakesTheRunCloudflareKnows(t *testing.T) {
	fake := &dockwellv1.CloudflaredTunnel{TunnelId: proveTunnel, Pid: 10, Reaches: "http://host.docker.internal:80", NetworkRange: "172.30.0.0/16"}
	known := &dockwellv1.CloudflaredTunnel{TunnelId: proveTunnel, Pid: 20, ConnectorId: "connector-1", Reaches: "http://127.0.0.1:80"}
	facts := Facts{Name: "hz-1", WaysOut: []string{"203.0.113.10"}}

	// Runs that reach apps differently leave no one service to write.
	p, tg := proving(t, []*dockwellv1.CloudflaredTunnel{fake, known}, cftest.Connector{ID: "connector-1"})
	if _, err := p.proveTunnel(t.Context(), "hz-1", tg, proveTunnel, facts); err == nil || !strings.Contains(err.Error(), "reach apps differently") {
		t.Fatalf("runs that disagree: %v", err)
	}

	// Alike but for a connector ID, the one Cloudflare lists is proved, and one run
	// with none beside one with one is refused.
	alike := &dockwellv1.CloudflaredTunnel{TunnelId: proveTunnel, Pid: 10, Reaches: known.GetReaches()}
	p, tg = proving(t, []*dockwellv1.CloudflaredTunnel{alike, known}, cftest.Connector{ID: "connector-1"})
	if _, err := p.proveTunnel(t.Context(), "hz-1", tg, proveTunnel, facts); err == nil || !strings.Contains(err.Error(), "can't read one of its runs") {
		t.Fatalf("a run with no connector ID beside one with one: %v", err)
	}
	second := &dockwellv1.CloudflaredTunnel{TunnelId: proveTunnel, Pid: 10, ConnectorId: "connector-0", Reaches: known.GetReaches()}
	p, tg = proving(t, []*dockwellv1.CloudflaredTunnel{second, known}, cftest.Connector{ID: "connector-1"})
	if got, err := p.proveTunnel(t.Context(), "hz-1", tg, proveTunnel, facts); err != nil || got.GetPid() != 20 {
		t.Fatalf("proved %v, %v; want PID 20", got, err)
	}
}

// Addresses prove a tunnel only where no run gave a connector ID and no other
// server leaves from the same address (ADR 0104, section 3).
func TestTheProofByAddress(t *testing.T) {
	run := &dockwellv1.CloudflaredTunnel{TunnelId: proveTunnel, Pid: 10, Reaches: "http://127.0.0.1:80"}
	from := cftest.Connector{ID: "connector-1", OriginIPs: []string{"203.0.113.10"}}
	facts := Facts{Name: "hz-1", WaysOut: []string{"203.0.113.10"}}

	p, tg := proving(t, []*dockwellv1.CloudflaredTunnel{run}, from)
	if got, err := p.proveTunnel(t.Context(), "hz-1", tg, proveTunnel, facts); err != nil || got != run {
		t.Fatalf("by its address alone: %v, %v", got, err)
	}
	facts.SharesWith = []string{"hz-2"}
	if _, err := p.proveTunnel(t.Context(), "hz-1", tg, proveTunnel, facts); err == nil || err.Error() != "hz-1 and hz-2 share an address, so Dockwell can't tell which runs the tunnel" {
		t.Fatalf("behind a shared address: %v", err)
	}
	facts.SharesWith = nil
	p, tg = proving(t, []*dockwellv1.CloudflaredTunnel{run}, from, cftest.Connector{ID: "connector-2", OriginIPs: []string{"198.51.100.7"}})
	if _, err := p.proveTunnel(t.Context(), "hz-1", tg, proveTunnel, facts); err == nil || !strings.Contains(err.Error(), "somewhere else too") {
		t.Fatalf("connected from somewhere else too: %v", err)
	}
}

// The names a zone's mode changes leave out names on a tunnel, with the panel's
// first and the names Dockwell doesn't publish next, which are those at risk under
// Full (strict) (the review's M3).
func TestZoneModeNamesPanelFirst(t *testing.T) {
	p := &Publisher{Install: "ins_1", Panel: func(context.Context) PanelNames { return PanelNames{Served: []string{"panel.example.com"}} }}
	all, atRisk := p.zoneModeNames(t.Context(), []dnsproviders.Record{
		{Name: "wiki.example.com", Content: proveTunnel + ".cfargotunnel.com", Proxied: true, Comment: "dockwell ins_1 rte_1"},
		{Name: "docs.example.com", Content: "203.0.113.10", Proxied: true, Comment: "dockwell ins_1 rte_2"},
		{Name: "vpn.example.com", Content: "203.0.113.9", Proxied: true, Comment: "by hand"},
		{Name: "panel.example.com", Content: "203.0.113.10", Proxied: true},
	})
	if got := strings.Join(all, " "); got != "panel.example.com vpn.example.com docs.example.com" {
		t.Errorf("every name: %s", got)
	}
	if got := strings.Join(atRisk, " "); got != "panel.example.com vpn.example.com" {
		t.Errorf("at risk: %s", got)
	}
}
