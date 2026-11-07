// SPDX-License-Identifier: AGPL-3.0-only

package domains

import (
	"context"
	"log/slog"
	"net"
	"net/netip"
	"slices"
	"testing"
	"time"

	"connectrpc.com/connect"

	agentv1 "github.com/dockwell/dockwell/gen/dockwell/agent/v1"
	dockwellv1 "github.com/dockwell/dockwell/gen/dockwell/v1"
	"github.com/dockwell/dockwell/internal/control/authapi"
)

// aServer is one with a private address of its own, Docker's bridge, a TUN's address
// and a public address the control plane knows beyond its interfaces.
type aServer struct{}

func (aServer) Get(id string) (*dockwellv1.Server, error) {
	return &dockwellv1.Server{Id: id, Inventory: &dockwellv1.ServerInventory{Addresses: []string{"10.0.0.25", "172.17.0.1", "198.18.0.1"}}}, nil
}

func (aServer) PublicAddress(string) *agentv1.PublicAddress {
	return &agentv1.PublicAddress{Addresses: []*agentv1.KnownAddress{{Address: "203.0.113.50", WayIn: true}}}
}

func names(m map[string]string) func(context.Context, string) ([]netip.Addr, error) {
	return func(_ context.Context, name string) ([]netip.Addr, error) {
		s, ok := m[name]
		if !ok {
			return nil, &net.DNSError{Err: "no such host", Name: name, IsNotFound: true}
		}
		return []netip.Addr{netip.MustParseAddr(s)}, nil
	}
}

func check(t *testing.T, h *Handler, domain string) (*dockwellv1.DomainCheck, error) {
	t.Helper()
	res, err := h.CheckDomain(t.Context(), connect.NewRequest(&dockwellv1.CheckDomainRequest{ServerId: "srv_A", Domain: domain}))
	if err != nil {
		return nil, err
	}
	return res.Msg.GetCheck(), nil
}

// A record leads to this server only at an address the internet can reach, and what a
// name resolves to inside some network is shown only when it's the server's own.
func TestADomainIsCheckedByWhatTheInternetReaches(t *testing.T) {
	h := New(nil, nil, nil, aServer{}, slog.New(slog.DiscardHandler))
	h.Lookup = names(map[string]string{
		"here.example.com":   "203.0.113.50",
		"lan.example.com":    "10.0.0.25",
		"bridge.example.com": "172.17.0.1",
		"inside.example.com": "10.9.9.9",
	})
	for domain, want := range map[string]struct {
		state dockwellv1.DomainCheckState
		found []string
	}{
		"here.example.com":   {dockwellv1.DomainCheckState_DOMAIN_CHECK_STATE_POINTS_HERE, []string{"203.0.113.50"}},
		"lan.example.com":    {dockwellv1.DomainCheckState_DOMAIN_CHECK_STATE_ELSEWHERE, []string{"10.0.0.25"}},
		"bridge.example.com": {dockwellv1.DomainCheckState_DOMAIN_CHECK_STATE_ELSEWHERE, []string{"172.17.0.1"}},
		"inside.example.com": {dockwellv1.DomainCheckState_DOMAIN_CHECK_STATE_ELSEWHERE, nil},
		"none.example.com":   {dockwellv1.DomainCheckState_DOMAIN_CHECK_STATE_NO_RECORD, nil},
	} {
		got, err := check(t, h, domain)
		if err != nil {
			t.Fatal(err)
		}
		if got.GetState() != want.state || !slices.Equal(got.GetFound(), want.found) {
			t.Errorf("%s reads %v with %v, want %v with %v", domain, got.GetState(), got.GetFound(), want.state, want.found)
		}
	}
}

func TestACallerChecksSixtyNamesAMinute(t *testing.T) {
	h := New(nil, nil, nil, aServer{}, slog.New(slog.DiscardHandler))
	h.Lookup = names(nil)
	for range checksPerMinute {
		if _, err := check(t, h, "app.example.com"); err != nil {
			t.Fatal(err)
		}
	}
	_, err := check(t, h, "app.example.com")
	if connect.CodeOf(err) != connect.CodeResourceExhausted {
		t.Fatalf("one more: %v", err)
	}
	var cerr *connect.Error
	if !errorAs(err, &cerr) || cerr.Meta().Get(authapi.ReasonKey) != "too_many_requests" {
		t.Errorf("it says %v", err)
	}
}

func errorAs(err error, target **connect.Error) bool {
	ce, ok := err.(*connect.Error) //nolint:errorlint // CheckDomain returns it as it is
	*target = ce
	return ok
}

func TestEachCallerHasItsOwnMinute(t *testing.T) {
	c := newCallers(2)
	now := time.Now()
	c.now = func() time.Time { return now }
	for i, want := range []bool{true, true, false} {
		if got := c.allow("token:a"); got != want {
			t.Errorf("check %d in a minute: allowed %t", i+1, got)
		}
	}
	if !c.allow("token:b") {
		t.Error("another caller was held back")
	}
	now = now.Add(time.Minute)
	if !c.allow("token:a") {
		t.Error("a new minute didn't let it check again")
	}
}
