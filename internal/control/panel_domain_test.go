// SPDX-License-Identifier: AGPL-3.0-only

package control

import (
	"context"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/tls"
	"crypto/x509"
	"log/slog"
	"math/big"
	"net"
	"net/http"
	"net/netip"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"connectrpc.com/connect"

	dockwellv1 "github.com/dockwell/dockwell/gen/dockwell/v1"
	"github.com/dockwell/dockwell/gen/dockwell/v1/dockwellv1connect"
	"github.com/dockwell/dockwell/internal/control/authapi"
	logapi "github.com/dockwell/dockwell/internal/control/logs"
	"github.com/dockwell/dockwell/internal/control/settings"
	"github.com/dockwell/dockwell/internal/store"
)

// panelDomainStand is a control plane whose names resolve as a test says, whose
// ports 80 and 443 something else holds, and whose restarts are counted.
func panelDomainStand(t *testing.T) (*fullServer, *atomic.Int32, *atomic.Value) {
	t.Helper()
	var restarts atomic.Int32
	var saved atomic.Value
	f := startFullConfig(t, store.SQLite, nil, slog.New(slog.DiscardHandler), nil, logapi.Options{}, func(c *Config) {
		c.PanelDomain = PanelDomain{
			Resolve: func(_ context.Context, host string) ([]netip.Addr, error) {
				switch host {
				case "panel.example.com":
					return []netip.Addr{netip.MustParseAddr("192.0.2.10")}, nil
				case "elsewhere.example.com":
					return []netip.Addr{netip.MustParseAddr("198.51.100.7")}, nil
				}
				return nil, &net.DNSError{Err: "no such host", Name: host, IsNotFound: true}
			},
			PublicAddresses: func() []string { return []string{"192.0.2.10", "2001:db8::10"} },
			PortsHeld: func(context.Context) []*dockwellv1.PanelPortHolder {
				return []*dockwellv1.PanelPortHolder{{Port: 80, Program: "nginx"}}
			},
			Restart: func() { restarts.Add(1) },
			Saved:   func(d string) { saved.Store(d) },
		}
	})
	return f, &restarts, &saved
}

// A domain is checked as it's typed: whether the panel can take it, where its
// records lead, and what holds the ports its certificate needs (ADR 0063).
func TestAPanelDomainIsCheckedAsItsTyped(t *testing.T) {
	f, _, _ := panelDomainStand(t)
	b := signedIn(t, f)
	c := dockwellv1connect.NewSettingsServiceClient(b, b.origin, connect.WithProtoJSON())
	check := func(domain string) *dockwellv1.CheckPanelDomainResponse {
		t.Helper()
		res, err := c.CheckPanelDomain(t.Context(), connect.NewRequest(&dockwellv1.CheckPanelDomainRequest{Domain: domain}))
		if err != nil {
			t.Fatal(err)
		}
		return res.Msg
	}
	here := check(" Panel.Example.com. ")
	if here.GetRefusal() != "" || !here.GetPointsHere() || strings.Join(here.GetAddresses(), ",") != "192.0.2.10" ||
		len(here.GetPortsHeld()) != 1 || here.GetPortsHeld()[0].GetProgram() != "nginx" {
		t.Errorf("a domain that leads here reads %v", here)
	}
	if away := check("elsewhere.example.com"); away.GetPointsHere() || away.GetRefusal() != "" || strings.Join(away.GetPublicAddresses(), ",") != "192.0.2.10,2001:db8::10" {
		t.Errorf("a domain that leads elsewhere reads %v", away)
	}
	if none := check("nowhere.example.com"); none.GetDnsProblem() != "it has no A or AAAA record" || none.GetRefusal() != "" {
		t.Errorf("a domain with no records reads %v", none)
	}
	if ip := check("192.0.2.10"); !strings.Contains(ip.GetRefusal(), "is an IP address") {
		t.Errorf("an address as the domain reads %v", ip)
	}
}

// The owner sets the panel's domain with a passkey just now, and the control
// plane restarts to serve it once the answer is out; no route can claim it from
// the save. A move to another domain waits for a later slice; clearing it goes
// back to loopback alone. Both are on the activity log, and an admin can't.
func TestTheOwnerSetsThePanelsDomainWithAPasskey(t *testing.T) {
	f, restarts, saved := panelDomainStand(t)
	b := signedIn(t, f)
	c := dockwellv1connect.NewSettingsServiceClient(b, b.origin, connect.WithProtoJSON())
	set := func(domain, proof string) (*dockwellv1.SetPanelDomainResponse, error) {
		res, err := c.SetPanelDomain(t.Context(), connect.NewRequest(&dockwellv1.SetPanelDomainRequest{Domain: domain, Proof: proof}))
		if err != nil {
			return nil, err
		}
		return res.Msg, nil
	}
	if _, err := set("panel.example.com", ""); reasonOf(err) != authapi.ReasonNeedsStepUp {
		t.Fatalf("setting the domain without a passkey: %v, want needs_step_up", err)
	}
	if _, err := set("panel.example.com", stepUpFor(t, b, dockwellv1.StepUpAction_STEP_UP_ACTION_SET_PANEL_DOMAIN, "other.example.com")); reasonOf(err) != authapi.ReasonNeedsStepUp {
		t.Errorf("a proof for another domain: %v, want needs_step_up", err)
	}
	got, err := set("panel.example.com", stepUpFor(t, b, dockwellv1.StepUpAction_STEP_UP_ACTION_SET_PANEL_DOMAIN, "panel.example.com"))
	if err != nil {
		t.Fatal(err)
	}
	if !got.GetRestarting() || got.GetSettings().GetPanel().GetSetDomain() != "panel.example.com" {
		t.Errorf("the save reads %v", got)
	}
	if d, _ := saved.Load().(string); d != "panel.example.com" {
		t.Errorf("the routes were told %q is the panel's", d)
	}
	// The restart comes once the answer is out, and once.
	deadline := time.Now().Add(5 * time.Second)
	for restarts.Load() == 0 && time.Now().Before(deadline) {
		time.Sleep(20 * time.Millisecond)
	}
	if n := restarts.Load(); n != 1 {
		t.Fatalf("restarted %d times, want once", n)
	}
	if again, err := set("panel.example.com", ""); err != nil || again.GetRestarting() {
		t.Errorf("saving the same domain again: %v, %v; want nothing to do", again, err)
	}

	if _, err := set("elsewhere.example.com", stepUpFor(t, b, dockwellv1.StepUpAction_STEP_UP_ACTION_SET_PANEL_DOMAIN, "elsewhere.example.com")); reasonOf(err) != "domain_move" {
		t.Errorf("a move to another domain: %v, want domain_move", err)
	}
	if _, err := set("", stepUpFor(t, b, dockwellv1.StepUpAction_STEP_UP_ACTION_SET_PANEL_DOMAIN, "panel.example.com")); err != nil {
		t.Fatalf("clearing the domain: %v", err)
	}
	read, err := c.GetSettings(t.Context(), connect.NewRequest(&dockwellv1.GetSettingsRequest{}))
	if err != nil {
		t.Fatal(err)
	}
	if read.Msg.GetSettings().GetPanel().GetSetDomain() != "" || read.Msg.GetSettings().GetPanel().GetDomainFrom() != dockwellv1.PanelDomainFrom_PANEL_DOMAIN_FROM_NONE {
		t.Errorf("after clearing it, the panel reads %v", read.Msg.GetSettings().GetPanel())
	}
	events := listEvents(t, dockwellv1connect.NewEventServiceClient(b, b.origin), &dockwellv1.ListEventsRequest{
		Kinds: []dockwellv1.EventKind{dockwellv1.EventKind_EVENT_KIND_SETTINGS},
	})
	var said []string
	for _, e := range events {
		said = append(said, e.GetSetting().GetSetting()+":"+e.GetSetting().GetPrevious()+">"+e.GetSetting().GetValue())
	}
	if want := "panel_domain:panel.example.com>, panel_domain:>panel.example.com"; strings.Join(said, ", ") != want {
		t.Errorf("the timeline has %v, want %v", said, want)
	}

	// From the control server, dockwell panel-domain --clear does the same.
	if _, err := set("panel.example.com", stepUpFor(t, b, dockwellv1.StepUpAction_STEP_UP_ACTION_SET_PANEL_DOMAIN, "panel.example.com")); err != nil {
		t.Fatal(err)
	}
	if was, err := settings.ClearPanelDomain(t.Context(), f.store, "cli:root@web-1", time.Now()); err != nil || was != "panel.example.com" {
		t.Errorf("clearing it locally: %q, %v", was, err)
	}

	// Only the owner: the domain decides where everyone signs in.
	hold := aMember(t, f)
	hold(onInstall(store.RoleAdmin))
	if _, err := set("panel.example.com", stepUpFor(t, b, dockwellv1.StepUpAction_STEP_UP_ACTION_SET_PANEL_DOMAIN, "panel.example.com")); reasonOf(err) != authapi.ReasonNeedsRole || !strings.Contains(err.Error(), "only the owner") {
		t.Errorf("an admin setting the domain: %v, want only the owner", err)
	}
}

// A sign-in link to the panel's domain is made only where the panel has one with
// its certificate, for the owner, with a passkey just now; its token rides after
// the "#".
func TestASignInLinkLeadsToThePanelsDomain(t *testing.T) {
	var certified atomic.Bool
	f := startFullConfig(t, store.SQLite, fixedDomain("panel.example.com"), slog.New(slog.DiscardHandler), nil, logapi.Options{}, func(c *Config) {
		c.PanelURL = "https://panel.example.com"
		c.PanelDomain = PanelDomain{From: dockwellv1.PanelDomainFrom_PANEL_DOMAIN_FROM_SETTING, Certified: certified.Load}
	})
	b := signedIn(t, f)
	c := dockwellv1connect.NewSettingsServiceClient(b, b.origin, connect.WithProtoJSON())
	link := func() (*dockwellv1.CreatePanelSignInLinkResponse, error) {
		res, err := c.CreatePanelSignInLink(t.Context(), connect.NewRequest(&dockwellv1.CreatePanelSignInLinkRequest{
			Proof: stepUpFor(t, b, dockwellv1.StepUpAction_STEP_UP_ACTION_SIGN_IN_ON_PANEL_DOMAIN, "panel.example.com"),
		}))
		if err != nil {
			return nil, err
		}
		return res.Msg, nil
	}
	if _, err := link(); reasonOf(err) != "not_certified" {
		t.Errorf("a link before the certificate: %v, want not_certified", err)
	}
	certified.Store(true)
	got, err := link()
	if err != nil {
		t.Fatal(err)
	}
	token, ok := strings.CutPrefix(got.GetUrl(), "https://panel.example.com/recover#")
	if !ok || len(token) < 20 || got.GetExpireTime().AsTime().Before(time.Now().Add(10*time.Minute)) {
		t.Errorf("the link reads %v", got)
	}
}

// fixedDomain is the panel's domain as an HTTPS source says it, with a
// certificate of its own making, serving nothing over plain HTTP.
type fixedDomain string

func (d fixedDomain) Domain() string { return string(d) }

func (d fixedDomain) TLSConfig() *tls.Config {
	key, _ := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	tmpl := &x509.Certificate{SerialNumber: big.NewInt(1), DNSNames: []string{string(d)}, NotBefore: time.Now().Add(-time.Hour), NotAfter: time.Now().Add(time.Hour)}
	der, _ := x509.CreateCertificate(rand.Reader, tmpl, tmpl, &key.PublicKey, key)
	cert := tls.Certificate{Certificate: [][]byte{der}, PrivateKey: key}
	return &tls.Config{MinVersion: tls.VersionTLS12, Certificates: []tls.Certificate{cert}}
}

func (fixedDomain) HTTPHandler() http.Handler { return http.NotFoundHandler() }
