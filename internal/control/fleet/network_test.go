// SPDX-License-Identifier: AGPL-3.0-only

package fleet

import (
	"errors"
	"log/slog"
	"strings"
	"testing"

	agentv1 "github.com/dockwell/dockwell/gen/dockwell/agent/v1"
	dockwellv1 "github.com/dockwell/dockwell/gen/dockwell/v1"
	"github.com/dockwell/dockwell/internal/store"
)

const (
	sourcePerson    = dockwellv1.PublicAddressSource_PUBLIC_ADDRESS_SOURCE_SET_BY_PERSON
	sourceInterface = dockwellv1.PublicAddressSource_PUBLIC_ADDRESS_SOURCE_ON_INTERFACE
	sourceDomain    = dockwellv1.PublicAddressSource_PUBLIC_ADDRESS_SOURCE_PROVED_BY_DOMAIN
	sourceSeen      = dockwellv1.PublicAddressSource_PUBLIC_ADDRESS_SOURCE_SEEN_BY_CONTROL_PLANE
	sourceMetadata  = dockwellv1.PublicAddressSource_PUBLIC_ADDRESS_SOURCE_CLOUD_METADATA
	sourceLookedUp  = dockwellv1.PublicAddressSource_PUBLIC_ADDRESS_SOURCE_LOOKED_UP
)

func attached(t *testing.T, h *Hub, addresses ...string) {
	t.Helper()
	inv := &dockwellv1.ServerInventory{Addresses: addresses}
	if _, err := h.Attach(t.Context(), store.Server{ID: "srv_A"}, inv, &dockwellv1.BuildInfo{}); err != nil {
		t.Fatal(err)
	}
}

func network(t *testing.T, h *Hub) *dockwellv1.ServerNetwork {
	t.Helper()
	s, err := h.Get("srv_A")
	if err != nil {
		t.Fatal(err)
	}
	return s.GetNetwork()
}

func issued(domain string, state dockwellv1.DomainCheckState, found ...string) *dockwellv1.DomainStatus {
	return &dockwellv1.DomainStatus{
		Domain:      domain,
		Certificate: &dockwellv1.Certificate{State: dockwellv1.CertificateState_CERTIFICATE_STATE_ISSUED},
		Dns:         &dockwellv1.DomainCheck{Domain: domain, State: state, Found: found},
	}
}

const (
	cantTell = dockwellv1.DomainCheckState_DOMAIN_CHECK_STATE_CANT_TELL
	here     = dockwellv1.DomainCheckState_DOMAIN_CHECK_STATE_POINTS_HERE
)

// Each source takes over from the one below it, from the bottom of the order up,
// so the order is what decides, and a higher one going lets the next come back.
func TestSourcesWinInTheirOrder(t *testing.T) {
	h, _ := newHub(t, "oci-1")
	attached(t, h, "10.0.0.25")
	if got := network(t, h).GetPublicAddress(); got != "" {
		t.Fatalf("with nothing known, it says %q", got)
	}
	steps := []struct {
		name    string
		do      func()
		address string
		source  dockwellv1.PublicAddressSource
	}{
		{"looked up", func() { h.RecordNetwork("srv_A", &agentv1.Network{LookedUpAddress: "198.51.100.1"}) }, "198.51.100.1", sourceLookedUp},
		{"metadata", func() {
			h.RecordNetwork("srv_A", &agentv1.Network{MetadataAddress: "198.51.100.2", LookedUpAddress: "198.51.100.1"})
		}, "198.51.100.2", sourceMetadata},
		{"seen", func() { h.SeenConnecting("srv_A", "198.51.100.3:41000") }, "198.51.100.3", sourceSeen},
		{"proved", func() {
			h.RecordDomains("srv_A", []*dockwellv1.DomainStatus{issued("app.example.com", cantTell, "198.51.100.4")})
		}, "198.51.100.4", sourceDomain},
		{"on an interface", func() { attached(t, h, "10.0.0.25", "198.51.100.5") }, "198.51.100.5", sourceInterface},
		{"set by a person", func() {
			if _, err := h.SetPublicAddress(t.Context(), "srv_A", "198.51.100.6", Setting{Actor: "user:usr_1", Surface: "panel", Who: &dockwellv1.Actor{Name: "Mira Chen"}}); err != nil {
				t.Fatal(err)
			}
		}, "198.51.100.6", sourcePerson},
	}
	for _, step := range steps {
		step.do()
		n := network(t, h)
		if n.GetPublicAddress() != step.address || n.GetSource() != step.source {
			t.Fatalf("%s: got %s by %v", step.name, n.GetPublicAddress(), n.GetSource())
		}
		if n.GetKnownTime() == nil {
			t.Errorf("%s: it doesn't say when", step.name)
		}
	}
	if got := network(t, h).GetSetBy().GetName(); got != "Mira Chen" {
		t.Errorf("set by %q", got)
	}
	if _, err := h.SetPublicAddress(t.Context(), "srv_A", "", Setting{Actor: "user:usr_1", Surface: "panel"}); err != nil {
		t.Fatal(err)
	}
	if n := network(t, h); n.GetPublicAddress() != "198.51.100.5" || n.GetSource() != sourceInterface {
		t.Errorf("cleared, it reads %s by %v", n.GetPublicAddress(), n.GetSource())
	}
}

func TestOnlyAPublicAddressSeenConnectingCounts(t *testing.T) {
	h, _ := newHub(t, "hz-1")
	attached(t, h, "10.0.0.5")
	for _, from := range []string{"127.0.0.1:50000", "10.0.0.9:50000", "100.64.0.9:50000", "[::1]:50000", "bad"} {
		h.SeenConnecting("srv_A", from)
		if got := network(t, h).GetPublicAddress(); got != "" {
			t.Errorf("from %s, it says %q", from, got)
		}
	}
	h.SeenConnecting("srv_A", "[::ffff:198.51.100.3]:50000")
	if got := network(t, h).GetPublicAddress(); got != "198.51.100.3" {
		t.Errorf("it says %q", got)
	}
}

func TestADomainProvesAnAddressOnlyWhereItCan(t *testing.T) {
	for _, tc := range []struct {
		name   string
		status *dockwellv1.DomainStatus
		want   string
	}{
		{"behind NAT, with a certificate", issued("a.example.com", cantTell, "203.0.113.50"), "203.0.113.50"},
		{"an A beside an AAAA", issued("a.example.com", cantTell, "203.0.113.50", "2001:db8::9"), "203.0.113.50"},
		{"only an AAAA", issued("a.example.com", cantTell, "2001:db8::9"), "2001:db8::9"},
		{"Cloudflare's range, which is shared", issued("a.example.com", cantTell, "104.16.0.1"), ""},
		{"two A records, which could be anyone's", issued("a.example.com", cantTell, "198.51.100.1", "198.51.100.2"), ""},
		{"a record that points elsewhere", issued("a.example.com", dockwellv1.DomainCheckState_DOMAIN_CHECK_STATE_ELSEWHERE, "198.51.100.1"), ""},
		{
			"no certificate yet",
			&dockwellv1.DomainStatus{Domain: "a.example.com", Certificate: &dockwellv1.Certificate{State: dockwellv1.CertificateState_CERTIFICATE_STATE_PENDING}, Dns: &dockwellv1.DomainCheck{State: cantTell, Found: []string{"198.51.100.1"}}},
			"",
		},
		{
			"none asked for, behind a tunnel",
			&dockwellv1.DomainStatus{Domain: "a.example.com", Certificate: &dockwellv1.Certificate{State: dockwellv1.CertificateState_CERTIFICATE_STATE_NONE}},
			"",
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			h, _ := newHub(t, "oci-1")
			attached(t, h, "10.0.0.25")
			h.RecordDomains("srv_A", []*dockwellv1.DomainStatus{tc.status})
			if got := network(t, h).GetPublicAddress(); got != tc.want {
				t.Errorf("it says %q, want %q", got, tc.want)
			}
		})
	}
}

// Nothing but a certificate authority's try says yes or no. What's looked up, seen
// or read from metadata is an address, and says nothing of the way in.
func TestReachableIsNeverAGuess(t *testing.T) {
	h, _ := newHub(t, "oci-1")
	attached(t, h, "10.0.0.25")
	assert := func(step string, want dockwellv1.Reachable, domain string) {
		t.Helper()
		n := network(t, h)
		if n.GetReachable() != want || n.GetReachableDomain() != domain {
			t.Errorf("%s: reachable %v by %q, want %v by %q", step, n.GetReachable(), n.GetReachableDomain(), want, domain)
		}
	}
	assert("knowing nothing", dockwellv1.Reachable_REACHABLE_NOT_CHECKED, "")
	h.RecordNetwork("srv_A", &agentv1.Network{LookedUpAddress: "203.0.113.50", MetadataAddress: "203.0.113.50"})
	assert("looked up, and in metadata", dockwellv1.Reachable_REACHABLE_NOT_CHECKED, "")
	h.SeenConnecting("srv_A", "203.0.113.50:50000")
	assert("seen connecting", dockwellv1.Reachable_REACHABLE_NOT_CHECKED, "")

	failed := &dockwellv1.DomainStatus{
		Domain:      "app.example.com",
		Certificate: &dockwellv1.Certificate{State: dockwellv1.CertificateState_CERTIFICATE_STATE_FAILED, Problem: dockwellv1.CertificateProblem_CERTIFICATE_PROBLEM_CONNECTION},
		Dns:         &dockwellv1.DomainCheck{State: here, Found: []string{"203.0.113.50"}},
	}
	h.RecordDomains("srv_A", []*dockwellv1.DomainStatus{failed})
	assert("a failed try at that address", dockwellv1.Reachable_REACHABLE_NO, "app.example.com")

	elsewhere := copied(failed)
	elsewhere.Dns.Found = []string{"198.51.100.1"}
	h.RecordDomains("srv_A", []*dockwellv1.DomainStatus{elsewhere})
	assert("a failed try at another address", dockwellv1.Reachable_REACHABLE_NOT_CHECKED, "")

	h.RecordDomains("srv_A", []*dockwellv1.DomainStatus{failed, issued("www.example.com", here, "203.0.113.50")})
	assert("a certificate, beside a failed try", dockwellv1.Reachable_REACHABLE_YES, "www.example.com")
}

func copied(s *dockwellv1.DomainStatus) *dockwellv1.DomainStatus {
	return &dockwellv1.DomainStatus{Domain: s.GetDomain(), Certificate: s.GetCertificate(), Dns: &dockwellv1.DomainCheck{State: s.GetDns().GetState(), Found: s.GetDns().GetFound()}}
}

func TestEachShapeIsToldApart(t *testing.T) {
	oracle := &agentv1.Network{Provider: dockwellv1.CloudProvider_CLOUD_PROVIDER_ORACLE, LookedUpAddress: "203.0.113.50"}
	home := &agentv1.Network{LookedUpAddress: "203.0.113.7"}
	for _, tc := range []struct {
		name      string
		addresses []string
		outbound  string
		network   *agentv1.Network
		want      dockwellv1.NetworkShape
	}{
		{"a public address on an interface", []string{"10.0.0.2", "203.0.113.52"}, "203.0.113.52", &agentv1.Network{}, dockwellv1.NetworkShape_NETWORK_SHAPE_INTERNET},
		{"a private interface and metadata, as on Oracle", []string{"10.0.0.25"}, "10.0.0.25", oracle, dockwellv1.NetworkShape_NETWORK_SHAPE_CLOUD_NAT},
		{"a cloud whose network uses the shared range", []string{"100.64.0.5"}, "100.64.0.5", oracle, dockwellv1.NetworkShape_NETWORK_SHAPE_CLOUD_NAT},
		{"a private interface and no metadata", []string{"192.168.1.20"}, "192.168.1.20", home, dockwellv1.NetworkShape_NETWORK_SHAPE_LOCAL_NAT},
		{"leaving from a shared address", []string{"100.72.1.2"}, "100.72.1.2", home, dockwellv1.NetworkShape_NETWORK_SHAPE_CARRIER_NAT},
		{"a VPN's shared address beside a home one", []string{"192.168.1.20", "100.72.1.2"}, "192.168.1.20", home, dockwellv1.NetworkShape_NETWORK_SHAPE_LOCAL_NAT},
		{"an agent that doesn't say where it leaves from, on a shared address", []string{"100.72.1.2"}, "", home, dockwellv1.NetworkShape_NETWORK_SHAPE_CARRIER_NAT},
		{"before its agent has said what answers", []string{"192.168.1.20"}, "192.168.1.20", nil, dockwellv1.NetworkShape_NETWORK_SHAPE_UNSPECIFIED},
		{"before its agent has said anything", nil, "", nil, dockwellv1.NetworkShape_NETWORK_SHAPE_UNSPECIFIED},
	} {
		t.Run(tc.name, func(t *testing.T) {
			h, _ := newHub(t, "hz-1")
			inv := &dockwellv1.ServerInventory{Addresses: tc.addresses, OutboundAddress: tc.outbound}
			if _, err := h.Attach(t.Context(), store.Server{ID: "srv_A"}, inv, &dockwellv1.BuildInfo{}); err != nil {
				t.Fatal(err)
			}
			if tc.network != nil {
				h.RecordNetwork("srv_A", tc.network)
			}
			if got := network(t, h).GetShape(); got != tc.want {
				t.Errorf("shape %v, want %v", got, tc.want)
			}
		})
	}
}

// What an agent goes by decides its words, so a way out is never offered as a way
// in, and behind carrier NAT isn't offered at all.
func TestWhatAnAgentIsTold(t *testing.T) {
	const looked = "203.0.113.7"
	for _, tc := range []struct {
		name      string
		addresses []string
		network   *agentv1.Network
		seen      string
		set       string
		want      *agentv1.PublicAddress
	}{
		{"on an interface: it has that", []string{"203.0.113.52"}, &agentv1.Network{}, "", "", &agentv1.PublicAddress{}},
		{"looked up, not on a cloud", []string{"192.168.1.20"}, &agentv1.Network{LookedUpAddress: looked}, "", "", &agentv1.PublicAddress{Address: looked, Unproven: true}},
		{"seen, not on a cloud", []string{"192.168.1.20"}, &agentv1.Network{}, looked + ":5000", "", &agentv1.PublicAddress{Address: looked, Unproven: true}},
		{"looked up, on a known cloud", []string{"10.0.0.25"}, &agentv1.Network{Provider: dockwellv1.CloudProvider_CLOUD_PROVIDER_ORACLE, LookedUpAddress: looked}, "", "", &agentv1.PublicAddress{Address: looked}},
		{"metadata is a way in", []string{"10.0.0.25"}, &agentv1.Network{Provider: dockwellv1.CloudProvider_CLOUD_PROVIDER_AWS, MetadataAddress: looked}, "", "", &agentv1.PublicAddress{Address: looked, WayIn: true}},
		{"behind carrier NAT, a way out is nothing to point at", []string{"100.72.1.2"}, &agentv1.Network{LookedUpAddress: looked}, "", "", &agentv1.PublicAddress{CarrierNat: true}},
		{"behind carrier NAT, a person may know better", []string{"100.72.1.2"}, &agentv1.Network{LookedUpAddress: looked}, "", "198.51.100.6", &agentv1.PublicAddress{Address: "198.51.100.6", WayIn: true}},
		{"a person, behind a router", []string{"192.168.1.20"}, &agentv1.Network{LookedUpAddress: looked}, "", "198.51.100.6", &agentv1.PublicAddress{Address: "198.51.100.6", WayIn: true}},
		{"nothing known", []string{"192.168.1.20"}, &agentv1.Network{}, "", "", &agentv1.PublicAddress{}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			h, _ := newHub(t, "hz-1")
			attached(t, h, tc.addresses...)
			h.RecordNetwork("srv_A", tc.network)
			if tc.seen != "" {
				h.SeenConnecting("srv_A", tc.seen)
			}
			if tc.set != "" {
				if _, err := h.SetPublicAddress(t.Context(), "srv_A", tc.set, Setting{Actor: "user:usr_1", Surface: "panel"}); err != nil {
					t.Fatal(err)
				}
			}
			got := h.PublicAddress("srv_A")
			if got.GetAddress() != tc.want.GetAddress() || got.GetWayIn() != tc.want.GetWayIn() || got.GetUnproven() != tc.want.GetUnproven() || got.GetCarrierNat() != tc.want.GetCarrierNat() {
				t.Errorf("told %v, want %v", got, tc.want)
			}
		})
	}
}

func TestAnAgentHearsWhenItsAddressChanges(t *testing.T) {
	h, _ := newHub(t, "hz-1")
	attached(t, h, "10.0.0.5")
	changed := h.PublicAddressChanged("srv_A")
	select {
	case <-changed:
	default:
	}
	h.RecordNetwork("srv_A", &agentv1.Network{LookedUpAddress: "198.51.100.1"})
	select {
	case <-changed:
	default:
		t.Error("a new address didn't signal")
	}
	h.RecordDomains("srv_A", nil)
	select {
	case <-changed:
		t.Error("nothing new signalled")
	default:
	}
}

func TestAPersonsAddressIsCheckedKeptAndOnRecord(t *testing.T) {
	h, db := newHub(t, "hz-1")
	by := Setting{Actor: "user:usr_1", Surface: "panel", Who: &dockwellv1.Actor{Name: "Mira Chen"}}
	for address, says := range map[string]string{
		"10.0.0.5":        "is private",
		"100.64.1.1":      "is private",
		"127.0.0.1":       "is private",
		"app.example.com": "isn't an IP address",
	} {
		_, err := h.SetPublicAddress(t.Context(), "srv_A", address, by)
		if !errors.Is(err, ErrNotAPublicAddress) || !strings.Contains(err.Error(), says) {
			t.Errorf("%s: %v", address, err)
		}
	}
	if _, err := h.SetPublicAddress(t.Context(), "srv_Z", "198.51.100.6", by); !errors.Is(err, ErrNotFound) {
		t.Errorf("an unknown server: %v", err)
	}
	srv, err := h.SetPublicAddress(t.Context(), "srv_A", "::ffff:198.51.100.6", by)
	if err != nil {
		t.Fatal(err)
	}
	if got := srv.GetNetwork().GetPublicAddress(); got != "198.51.100.6" {
		t.Errorf("it reads %q", got)
	}

	again, err := New(t.Context(), db, slog.New(slog.DiscardHandler))
	if err != nil {
		t.Fatal(err)
	}
	if s, err := again.Get("srv_A"); err != nil || s.GetNetwork().GetSource() != sourcePerson || s.GetNetwork().GetSetBy().GetName() != "Mira Chen" {
		t.Errorf("after a restart: %v (%v)", s.GetNetwork(), err)
	}

	var found store.Event
	var recorded bool
	if err := db.View(t.Context(), func(tx *store.Tx) (err error) {
		found, recorded, err = tx.LatestEvent(t.Context(), ActionPublicAddress, "srv_A", "srv_A")
		return err
	}); err != nil {
		t.Fatal(err)
	}
	if !recorded || found.Actor != "user:usr_1" || !strings.Contains(found.Detail, "198.51.100.6") {
		t.Errorf("on record: %t, %+v", recorded, found)
	}
}
