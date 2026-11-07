// SPDX-License-Identifier: AGPL-3.0-only

package fleet

import (
	"encoding/json"
	"errors"
	"log/slog"
	"strings"
	"testing"
	"time"

	"google.golang.org/protobuf/proto"
	"google.golang.org/protobuf/types/known/timestamppb"

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

	shapeInternet = dockwellv1.NetworkShape_NETWORK_SHAPE_INTERNET
	shapeCloud    = dockwellv1.NetworkShape_NETWORK_SHAPE_CLOUD_NAT
	shapeLocal    = dockwellv1.NetworkShape_NETWORK_SHAPE_LOCAL_NAT
	shapeCarrier  = dockwellv1.NetworkShape_NETWORK_SHAPE_CARRIER_NAT
	shapeUnknown  = dockwellv1.NetworkShape_NETWORK_SHAPE_UNSPECIFIED

	aws    = dockwellv1.CloudProvider_CLOUD_PROVIDER_AWS
	azure  = dockwellv1.CloudProvider_CLOUD_PROVIDER_AZURE
	oracle = dockwellv1.CloudProvider_CLOUD_PROVIDER_ORACLE

	cantTell = dockwellv1.DomainCheckState_DOMAIN_CHECK_STATE_CANT_TELL
	here     = dockwellv1.DomainCheckState_DOMAIN_CHECK_STATE_POINTS_HERE
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

func dnsFor(domain string, state dockwellv1.DomainCheckState, found ...string) *dockwellv1.DomainCheck {
	return &dockwellv1.DomainCheck{Domain: domain, State: state, Found: found, ReadTime: timestamppb.Now()}
}

func issued(domain string, state dockwellv1.DomainCheckState, found ...string) *dockwellv1.DomainStatus {
	return &dockwellv1.DomainStatus{
		Domain:      domain,
		Certificate: &dockwellv1.Certificate{State: dockwellv1.CertificateState_CERTIFICATE_STATE_ISSUED},
		Dns:         dnsFor(domain, state, found...),
	}
}

func set(t *testing.T, h *Hub, address string) {
	t.Helper()
	if _, err := h.SetPublicAddress(t.Context(), "srv_A", address, false, Setting{Actor: "user:usr_1", Surface: "panel", Who: &dockwellv1.Actor{Name: "Mira Chen"}}); err != nil {
		t.Fatal(err)
	}
}

// Each source takes over from the one below it, from the bottom of the order up, so
// the order is what decides, and a higher one going lets the next come back.
func TestSourcesWinInTheirOrder(t *testing.T) {
	h, _ := newHub(t, "cloud-1")
	attached(t, h, "10.0.0.25")
	if got := network(t, h).GetIpv4(); got != nil {
		t.Fatalf("with nothing known, it says %v", got)
	}
	steps := []struct {
		name     string
		do       func()
		address  string
		source   dockwellv1.PublicAddressSource
		unproven bool
	}{
		{"looked up", func() { h.RecordNetwork("srv_A", &agentv1.Network{LookedUpAddress: "198.51.100.1"}) }, "198.51.100.1", sourceLookedUp, true},
		{"metadata the machine doesn't confirm", func() {
			h.RecordNetwork("srv_A", &agentv1.Network{Provider: aws, MetadataAddress: "198.51.100.2", LookedUpAddress: "198.51.100.1"})
		}, "198.51.100.2", sourceMetadata, true},
		{"seen", func() { h.SeenConnecting("srv_A", "198.51.100.3:41000") }, "198.51.100.3", sourceSeen, true},
		{"metadata the machine confirms", func() {
			h.RecordNetwork("srv_A", &agentv1.Network{Provider: aws, MachineProvider: aws, MetadataAddress: "198.51.100.2", LookedUpAddress: "198.51.100.1"})
		}, "198.51.100.2", sourceMetadata, false},
		{"proved", func() {
			h.RecordDomains("srv_A", []*dockwellv1.DomainStatus{issued("app.example.com", cantTell, "198.51.100.4")})
		}, "198.51.100.4", sourceDomain, false},
		{"on an interface", func() { attached(t, h, "10.0.0.25", "198.51.100.5") }, "198.51.100.5", sourceInterface, false},
		{"set by a person", func() { set(t, h, "198.51.100.6") }, "198.51.100.6", sourcePerson, false},
	}
	for _, step := range steps {
		step.do()
		n := network(t, h).GetIpv4()
		if n.GetAddress() != step.address || n.GetSource() != step.source || n.GetUnproven() != step.unproven {
			t.Fatalf("%s: got %v", step.name, n)
		}
		if n.GetKnownTime() == nil {
			t.Errorf("%s: it doesn't say when", step.name)
		}
	}
	if got := network(t, h).GetIpv4().GetSetBy().GetName(); got != "Mira Chen" {
		t.Errorf("set by %q", got)
	}
	if _, err := h.SetPublicAddress(t.Context(), "srv_A", "", false, Setting{Actor: "user:usr_1", Surface: "panel"}); err != nil {
		t.Fatal(err)
	}
	if n := network(t, h).GetIpv4(); n.GetAddress() != "198.51.100.5" || n.GetSource() != sourceInterface {
		t.Errorf("cleared, it reads %v", n)
	}
}

func TestOnlyAPublicAddressSeenConnectingCounts(t *testing.T) {
	h, _ := newHub(t, "hz-1")
	attached(t, h, "10.0.0.5")
	for _, from := range []string{"127.0.0.1:50000", "10.0.0.9:50000", "100.64.0.9:50000", "[::1]:50000", "bad"} {
		h.SeenConnecting("srv_A", from)
		if got := network(t, h).GetIpv4(); got != nil {
			t.Errorf("from %s, it says %v", from, got)
		}
	}
	h.SeenConnecting("srv_A", "[::ffff:198.51.100.3]:50000")
	if got := network(t, h).GetIpv4().GetAddress(); got != "198.51.100.3" {
		t.Errorf("it says %q", got)
	}
}

func TestADomainProvesAnAddressOnlyWhereItCan(t *testing.T) {
	old := issued("a.example.com", cantTell, "198.51.100.1")
	old.Dns.ReadTime = nil
	for _, tc := range []struct {
		name   string
		status *dockwellv1.DomainStatus
		want   string
	}{
		{"behind NAT, with a certificate", issued("a.example.com", cantTell, "203.0.113.50"), "203.0.113.50"},
		{"an A beside an AAAA", issued("a.example.com", cantTell, "203.0.113.50", "2001:db8::9"), "203.0.113.50"},
		{"Cloudflare's range, which is shared", issued("a.example.com", cantTell, "104.16.0.1"), ""},
		{"two A records, which could be anyone's", issued("a.example.com", cantTell, "198.51.100.1", "198.51.100.2"), ""},
		{"a record that points elsewhere", issued("a.example.com", dockwellv1.DomainCheckState_DOMAIN_CHECK_STATE_ELSEWHERE, "198.51.100.1"), ""},
		{"an agent that doesn't say when it read DNS", old, ""},
		{
			"no certificate yet",
			&dockwellv1.DomainStatus{Domain: "a.example.com", Certificate: &dockwellv1.Certificate{State: dockwellv1.CertificateState_CERTIFICATE_STATE_PENDING}, Dns: dnsFor("a.example.com", cantTell, "198.51.100.1")},
			"",
		},
		{
			"none asked for, behind a tunnel",
			&dockwellv1.DomainStatus{Domain: "a.example.com", Certificate: &dockwellv1.Certificate{State: dockwellv1.CertificateState_CERTIFICATE_STATE_NONE}},
			"",
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			h, _ := newHub(t, "cloud-1")
			attached(t, h, "10.0.0.25")
			h.RecordDomains("srv_A", []*dockwellv1.DomainStatus{tc.status})
			if got := network(t, h).GetIpv4().GetAddress(); got != tc.want {
				t.Errorf("it says %q, want %q", got, tc.want)
			}
		})
	}
}

// Rule 1 and H2: a way out is named with the caveat wherever it's seen, a cloud
// included, as an Azure NAT gateway, an AWS private subnet and a proxy routing everything
// out of an Oracle server all go out somewhere the internet can't come back in.
func TestAWayOutIsNeverNamedWithoutTheCaveat(t *testing.T) {
	const out = "203.0.113.7"
	for _, tc := range []struct {
		name  string
		setup func(*Hub)
	}{
		{"looked up behind a router", func(h *Hub) {
			h.RecordNetwork("srv_A", &agentv1.Network{LookedUpAddress: out})
		}},
		{"seen behind a router", func(h *Hub) {
			h.RecordNetwork("srv_A", &agentv1.Network{})
			h.SeenConnecting("srv_A", out+":5000")
		}},
		{"an Azure NAT gateway, with no address in metadata", func(h *Hub) {
			h.RecordNetwork("srv_A", &agentv1.Network{Provider: azure, MachineProvider: azure, LookedUpAddress: out})
		}},
		{"an AWS private subnet, seen", func(h *Hub) {
			h.RecordNetwork("srv_A", &agentv1.Network{Provider: aws, MachineProvider: aws})
			h.SeenConnecting("srv_A", out+":5000")
		}},
		{"Oracle, with a proxy routing everything out", func(h *Hub) {
			h.RecordNetwork("srv_A", &agentv1.Network{Provider: oracle, MachineProvider: oracle, LookedUpAddress: out})
		}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			h, _ := newHub(t, "cloud-1")
			attached(t, h, "10.0.0.25")
			tc.setup(h)
			n := network(t, h).GetIpv4()
			if n.GetAddress() != out || !n.GetUnproven() {
				t.Fatalf("it reads %v", n)
			}
			told := h.PublicAddress("srv_A").GetAddresses()
			if len(told) != 1 || told[0].GetAddress() != out || told[0].GetWayIn() {
				t.Errorf("the agent is told %v", told)
			}
		})
	}
}

// Rule 2 and M3: anything on a LAN can answer at the metadata address, so only the
// machine's own word makes it count.
func TestMetadataCountsOnlyWhereTheMachineSaysSo(t *testing.T) {
	const address = "198.51.100.2"
	for _, tc := range []struct {
		name     string
		machine  dockwellv1.CloudProvider
		way      bool
		shape    dockwellv1.NetworkShape
		provider dockwellv1.CloudProvider
	}{
		{"the machine names that cloud", aws, true, shapeCloud, aws},
		{"the machine names no cloud", dockwellv1.CloudProvider_CLOUD_PROVIDER_UNSPECIFIED, false, shapeLocal, dockwellv1.CloudProvider_CLOUD_PROVIDER_UNSPECIFIED},
		{"the machine names another cloud", oracle, false, shapeCloud, oracle},
	} {
		t.Run(tc.name, func(t *testing.T) {
			h, _ := newHub(t, "lan-1")
			attached(t, h, "192.168.1.20")
			h.RecordNetwork("srv_A", &agentv1.Network{Provider: aws, MachineProvider: tc.machine, MetadataAddress: address})
			n := network(t, h)
			if n.GetIpv4().GetAddress() != address || n.GetIpv4().GetUnproven() == tc.way {
				t.Errorf("it reads %v, want a way in: %t", n.GetIpv4(), tc.way)
			}
			if n.GetShape() != tc.shape || n.GetProvider() != tc.provider {
				t.Errorf("shape %v on %v", n.GetShape(), n.GetProvider())
			}
		})
	}
}

// Rule 3 and H2: on a cloud, what the provider says beats how the server goes out.
func TestOnACloudMetadataBeatsAWayOut(t *testing.T) {
	h, _ := newHub(t, "cloud-1")
	attached(t, h, "10.0.0.25")
	h.SeenConnecting("srv_A", "203.0.113.7:5000")
	h.RecordNetwork("srv_A", &agentv1.Network{Provider: aws, MachineProvider: aws, MetadataAddress: "198.51.100.2", LookedUpAddress: "203.0.113.8"})
	if n := network(t, h).GetIpv4(); n.GetAddress() != "198.51.100.2" || n.GetSource() != sourceMetadata || n.GetUnproven() {
		t.Errorf("it reads %v", n)
	}
}

// Rule 4 and H1: a certificate proves where a server answered, and a record moves. A
// read gone stale proves nothing, and neither does one a newer way out disagrees with.
func TestAProofGoesStale(t *testing.T) {
	const proved, moved = "198.51.100.4", "203.0.113.7"
	longAgo := timestamppb.New(time.Now().Add(-proofFresh - time.Hour))
	withRead := func(read *timestamppb.Timestamp) []*dockwellv1.DomainStatus {
		status := issued("app.example.com", cantTell, proved)
		status.Dns.ReadTime = read
		return []*dockwellv1.DomainStatus{status}
	}
	assert := func(t *testing.T, h *Hub, address string, source dockwellv1.PublicAddressSource, state dockwellv1.Reachable) {
		t.Helper()
		n := network(t, h)
		if n.GetIpv4().GetAddress() != address || n.GetIpv4().GetSource() != source || n.GetReachable() != state {
			t.Errorf("it reads %v, reachable %v", n.GetIpv4(), n.GetReachable())
		}
	}
	t.Run("a read that's too old proves nothing, and says nothing of reaching it", func(t *testing.T) {
		h, _ := newHub(t, "home-1")
		attached(t, h, "192.168.1.20")
		h.RecordNetwork("srv_A", &agentv1.Network{LookedUpAddress: moved, LookedUpTime: longAgo})
		h.RecordDomains("srv_A", withRead(longAgo))
		assert(t, h, moved, sourceLookedUp, dockwellv1.Reachable_REACHABLE_NOT_CHECKED)
	})
	t.Run("at home, a newer way out overtakes it, as DDNS moved the address", func(t *testing.T) {
		h, _ := newHub(t, "home-1")
		attached(t, h, "192.168.1.20")
		h.RecordDomains("srv_A", withRead(timestamppb.New(time.Now().Add(-time.Hour))))
		h.RecordNetwork("srv_A", &agentv1.Network{LookedUpAddress: moved, LookedUpTime: timestamppb.Now()})
		assert(t, h, moved, sourceLookedUp, dockwellv1.Reachable_REACHABLE_NOT_CHECKED)
	})
	t.Run("at home, an older way out doesn't", func(t *testing.T) {
		h, _ := newHub(t, "home-1")
		attached(t, h, "192.168.1.20")
		h.RecordNetwork("srv_A", &agentv1.Network{LookedUpAddress: moved, LookedUpTime: timestamppb.New(time.Now().Add(-time.Hour))})
		h.RecordDomains("srv_A", withRead(timestamppb.Now()))
		assert(t, h, proved, sourceDomain, dockwellv1.Reachable_REACHABLE_YES)
	})
	t.Run("on a cloud, a way out is allowed to differ", func(t *testing.T) {
		h, _ := newHub(t, "cloud-1")
		attached(t, h, "10.0.0.25")
		h.RecordDomains("srv_A", withRead(timestamppb.New(time.Now().Add(-time.Hour))))
		h.RecordNetwork("srv_A", &agentv1.Network{Provider: aws, MachineProvider: aws, LookedUpAddress: moved, LookedUpTime: timestamppb.Now()})
		assert(t, h, proved, sourceDomain, dockwellv1.Reachable_REACHABLE_YES)
	})
	t.Run("what's seen overtakes it too", func(t *testing.T) {
		h, _ := newHub(t, "home-1")
		attached(t, h, "192.168.1.20")
		h.RecordNetwork("srv_A", &agentv1.Network{})
		h.RecordDomains("srv_A", withRead(timestamppb.New(time.Now().Add(-time.Hour))))
		h.SeenConnecting("srv_A", moved+":5000")
		assert(t, h, moved, sourceSeen, dockwellv1.Reachable_REACHABLE_NOT_CHECKED)
	})
	t.Run("it runs out with nothing new heard", func(t *testing.T) {
		h, _ := newHub(t, "home-1")
		attached(t, h, "192.168.1.20")
		h.RecordNetwork("srv_A", &agentv1.Network{})
		h.RecordDomains("srv_A", withRead(timestamppb.Now()))
		assert(t, h, proved, sourceDomain, dockwellv1.Reachable_REACHABLE_YES)
		h.now = func() time.Time { return time.Now().Add(proofFresh + time.Hour) }
		h.Record("srv_A", stats(1))
		if got := network(t, h).GetIpv4(); got != nil {
			t.Errorf("it still reads %v", got)
		}
	})
}

// Rule 5 and M1: an AAAA on an interface says nothing of an A record.
func TestEachFamilyIsPickedApart(t *testing.T) {
	h, _ := newHub(t, "home-1")
	attached(t, h, "10.0.0.5", "2001:db8::5")
	h.RecordNetwork("srv_A", &agentv1.Network{LookedUpAddress: "203.0.113.7"})
	n := network(t, h)
	if n.GetIpv4().GetAddress() != "203.0.113.7" || n.GetIpv4().GetSource() != sourceLookedUp || !n.GetIpv4().GetUnproven() {
		t.Errorf("IPv4 reads %v", n.GetIpv4())
	}
	if n.GetIpv6().GetAddress() != "2001:db8::5" || n.GetIpv6().GetSource() != sourceInterface || n.GetIpv6().GetUnproven() {
		t.Errorf("IPv6 reads %v", n.GetIpv6())
	}
	if n.GetShape() == shapeInternet {
		t.Error("a public IPv6 address alone reads as on the internet")
	}
	told := h.PublicAddress("srv_A").GetAddresses()
	if len(told) != 1 || told[0].GetAddress() != "203.0.113.7" {
		t.Errorf("the agent is told %v, and has what's on its interfaces", told)
	}

	// A person's word on one family leaves the other as it is.
	set(t, h, "2001:db8::9")
	n = network(t, h)
	if n.GetIpv6().GetSource() != sourcePerson || n.GetIpv4().GetSource() != sourceLookedUp {
		t.Errorf("it reads %v", n)
	}
	set(t, h, "198.51.100.6")
	if _, err := h.SetPublicAddress(t.Context(), "srv_A", "", true, Setting{Actor: "user:usr_1", Surface: "panel"}); err != nil {
		t.Fatal(err)
	}
	n = network(t, h)
	if n.GetIpv4().GetAddress() != "198.51.100.6" || n.GetIpv6().GetAddress() != "2001:db8::5" {
		t.Errorf("clearing IPv6 left %v", n)
	}
}

func TestReachableIsNeverAGuess(t *testing.T) {
	h, _ := newHub(t, "cloud-1")
	attached(t, h, "10.0.0.25")
	assert := func(step string, want dockwellv1.Reachable, domain string) {
		t.Helper()
		n := network(t, h)
		if n.GetReachable() != want || n.GetReachableDomain() != domain {
			t.Errorf("%s: reachable %v by %q, want %v by %q", step, n.GetReachable(), n.GetReachableDomain(), want, domain)
		}
	}
	assert("knowing nothing", dockwellv1.Reachable_REACHABLE_NOT_CHECKED, "")
	h.RecordNetwork("srv_A", &agentv1.Network{Provider: aws, MachineProvider: aws, LookedUpAddress: "203.0.113.50", MetadataAddress: "203.0.113.50"})
	assert("looked up, and in metadata", dockwellv1.Reachable_REACHABLE_NOT_CHECKED, "")
	h.SeenConnecting("srv_A", "203.0.113.50:50000")
	assert("seen connecting", dockwellv1.Reachable_REACHABLE_NOT_CHECKED, "")

	failed := &dockwellv1.DomainStatus{
		Domain:      "app.example.com",
		Certificate: &dockwellv1.Certificate{State: dockwellv1.CertificateState_CERTIFICATE_STATE_FAILED, Problem: dockwellv1.CertificateProblem_CERTIFICATE_PROBLEM_CONNECTION},
		Dns:         dnsFor("app.example.com", here, "203.0.113.50"),
	}
	h.RecordDomains("srv_A", []*dockwellv1.DomainStatus{failed})
	assert("a failed try at that address", dockwellv1.Reachable_REACHABLE_NO, "app.example.com")

	elsewhere := proto.Clone(failed).(*dockwellv1.DomainStatus) //nolint:forcetypeassert // Clone gives back what it's given
	elsewhere.Dns.Found = []string{"198.51.100.1"}
	h.RecordDomains("srv_A", []*dockwellv1.DomainStatus{elsewhere})
	assert("a failed try at another address", dockwellv1.Reachable_REACHABLE_NOT_CHECKED, "")

	h.RecordDomains("srv_A", []*dockwellv1.DomainStatus{failed, issued("www.example.com", here, "203.0.113.50")})
	assert("a certificate, beside a failed try", dockwellv1.Reachable_REACHABLE_YES, "www.example.com")
	if network(t, h).GetReachableTime() == nil {
		t.Error("it doesn't say as of when")
	}

	// L2: a record among several may be anyone's, as for a proof.
	h.RecordDomains("srv_A", []*dockwellv1.DomainStatus{issued("www.example.com", here, "203.0.113.50", "198.51.100.1")})
	assert("a record that holds the address among several", dockwellv1.Reachable_REACHABLE_NOT_CHECKED, "")
}

func TestEachShapeIsToldApart(t *testing.T) {
	cloud := &agentv1.Network{Provider: oracle, MachineProvider: oracle, LookedUpAddress: "203.0.113.50"}
	home := &agentv1.Network{LookedUpAddress: "203.0.113.7"}
	for _, tc := range []struct {
		name      string
		addresses []string
		outbound  string
		network   *agentv1.Network
		want      dockwellv1.NetworkShape
	}{
		{"a public IPv4 address on an interface", []string{"10.0.0.2", "198.51.100.9"}, "198.51.100.9", &agentv1.Network{}, shapeInternet},
		{"a public IPv6 address and a private IPv4 one", []string{"10.0.0.5", "2001:db8::5"}, "10.0.0.5", home, shapeLocal},
		{"IPv6 alone says nothing of the shape", []string{"2001:db8::5"}, "2001:db8::5", home, shapeUnknown},
		{"a private interface, on a machine that names a cloud", []string{"10.0.0.25"}, "10.0.0.25", cloud, shapeCloud},
		{"the machine names a cloud, whose metadata didn't answer", []string{"10.0.0.25"}, "10.0.0.25", &agentv1.Network{MachineProvider: oracle}, shapeCloud},
		{"a cloud whose network uses the shared range", []string{"100.64.0.5"}, "100.64.0.5", cloud, shapeCloud},
		{"a private interface and no cloud", []string{"192.168.1.20"}, "192.168.1.20", home, shapeLocal},
		{"leaving from a shared address", []string{"100.72.1.2"}, "100.72.1.2", home, shapeCarrier},
		{"a VPN's shared address beside a home one", []string{"192.168.1.20", "100.72.1.2"}, "192.168.1.20", home, shapeLocal},
		{"an agent that doesn't say where it leaves from, on a shared address", []string{"100.72.1.2"}, "", home, shapeCarrier},
		{"before its agent has said what answers", []string{"192.168.1.20"}, "192.168.1.20", nil, shapeUnknown},
		{"before its agent has said anything", nil, "", nil, shapeUnknown},
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

// What an agent goes by decides its words.
func TestWhatAnAgentIsTold(t *testing.T) {
	const looked = "203.0.113.7"
	type known = agentv1.KnownAddress
	for _, tc := range []struct {
		name      string
		addresses []string
		network   *agentv1.Network
		set       string
		want      []*known
		carrier   bool
	}{
		{"on an interface: it has that", []string{"198.51.100.9"}, &agentv1.Network{}, "", nil, false},
		{"behind carrier NAT, a way out is nothing to point at", []string{"100.72.1.2"}, &agentv1.Network{LookedUpAddress: looked}, "", nil, true},
		{"behind carrier NAT, a person may know better", []string{"100.72.1.2"}, &agentv1.Network{LookedUpAddress: looked}, "198.51.100.6", []*known{{Address: "198.51.100.6", WayIn: true}}, false},
		{"behind carrier NAT with an AAAA, that's a way in", []string{"100.72.1.2", "2001:db8::5"}, &agentv1.Network{LookedUpAddress: looked}, "", nil, false},
		{"a person, behind a router", []string{"192.168.1.20"}, &agentv1.Network{LookedUpAddress: looked}, "198.51.100.6", []*known{{Address: "198.51.100.6", WayIn: true}}, false},
		{"metadata that counts is a way in", []string{"10.0.0.25"}, &agentv1.Network{Provider: aws, MachineProvider: aws, MetadataAddress: looked}, "", []*known{{Address: looked, WayIn: true}}, false},
		{"nothing known", []string{"192.168.1.20"}, &agentv1.Network{}, "", nil, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			h, _ := newHub(t, "hz-1")
			attached(t, h, tc.addresses...)
			h.RecordNetwork("srv_A", tc.network)
			if tc.set != "" {
				set(t, h, tc.set)
			}
			got := h.PublicAddress("srv_A")
			want := &agentv1.PublicAddress{Addresses: tc.want, CarrierNat: tc.carrier}
			if !proto.Equal(got, want) {
				t.Errorf("told %v, want %v", got, want)
			}
		})
	}
}

// L4: two streams at once, as an agent moves from one to the next, each hear of it.
func TestEveryStreamHearsWhenTheAddressChanges(t *testing.T) {
	h, _ := newHub(t, "hz-1")
	attached(t, h, "10.0.0.5")
	first, stopFirst := h.WatchPublicAddress("srv_A")
	second, stopSecond := h.WatchPublicAddress("srv_A")
	defer stopSecond()
	heard := func(what string, c <-chan struct{}, want bool) {
		t.Helper()
		select {
		case <-c:
			if !want {
				t.Errorf("%s heard something", what)
			}
		default:
			if want {
				t.Errorf("%s heard nothing", what)
			}
		}
	}
	h.RecordNetwork("srv_A", &agentv1.Network{LookedUpAddress: "198.51.100.1"})
	heard("the old stream", first, true)
	heard("the new stream", second, true)
	h.RecordDomains("srv_A", nil)
	heard("the new stream, with nothing new", second, false)
	stopFirst()
	h.RecordNetwork("srv_A", &agentv1.Network{LookedUpAddress: "198.51.100.2"})
	heard("the stream that left", first, false)
	heard("the stream that stays", second, true)
}

// L7: what an agent said of itself goes with its last stream, and the next says it anew.
func TestWhatAnAgentSaidGoesWithItsStream(t *testing.T) {
	h, _ := newHub(t, "home-1")
	detach, err := h.Attach(t.Context(), store.Server{ID: "srv_A"}, &dockwellv1.ServerInventory{Addresses: []string{"192.168.1.20"}}, &dockwellv1.BuildInfo{})
	if err != nil {
		t.Fatal(err)
	}
	h.RecordNetwork("srv_A", &agentv1.Network{LookedUpAddress: "203.0.113.7", Provider: aws, MachineProvider: aws})
	h.SeenConnecting("srv_A", "203.0.113.8:5000")
	h.RecordDomains("srv_A", []*dockwellv1.DomainStatus{issued("app.example.com", cantTell, "203.0.113.9")})
	if got := network(t, h); got.GetIpv4().GetAddress() != "203.0.113.9" || got.GetReachable() != dockwellv1.Reachable_REACHABLE_YES || got.GetProvider() != aws {
		t.Fatalf("while connected it reads %v", got)
	}
	set(t, h, "2001:db8::9")
	detach()
	got := network(t, h)
	if got.GetIpv4() != nil || got.GetReachable() != dockwellv1.Reachable_REACHABLE_NOT_CHECKED || got.GetProvider() != dockwellv1.CloudProvider_CLOUD_PROVIDER_UNSPECIFIED {
		t.Errorf("gone, it reads %v", got)
	}
	if got.GetIpv6().GetAddress() != "2001:db8::9" {
		t.Errorf("what a person set went too: %v", got)
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
		_, err := h.SetPublicAddress(t.Context(), "srv_A", address, false, by)
		if !errors.Is(err, ErrNotAPublicAddress) || !strings.Contains(err.Error(), says) {
			t.Errorf("%s: %v", address, err)
		}
	}
	if _, err := h.SetPublicAddress(t.Context(), "srv_Z", "198.51.100.6", false, by); !errors.Is(err, ErrNotFound) {
		t.Errorf("an unknown server: %v", err)
	}
	srv, err := h.SetPublicAddress(t.Context(), "srv_A", "::ffff:198.51.100.6", false, by)
	if err != nil {
		t.Fatal(err)
	}
	if got := srv.GetNetwork().GetIpv4().GetAddress(); got != "198.51.100.6" {
		t.Errorf("it reads %q", got)
	}

	again, err := New(t.Context(), db, slog.New(slog.DiscardHandler))
	if err != nil {
		t.Fatal(err)
	}
	if s, err := again.Get("srv_A"); err != nil || s.GetNetwork().GetIpv4().GetSource() != sourcePerson || s.GetNetwork().GetIpv4().GetSetBy().GetName() != "Mira Chen" {
		t.Errorf("after a restart: %v (%v)", s.GetNetwork(), err)
	}

	latest := func(action string) AddressChange {
		t.Helper()
		var found store.Event
		var recorded bool
		if err := db.View(t.Context(), func(tx *store.Tx) (err error) {
			found, recorded, err = tx.LatestEvent(t.Context(), action, "srv_A", "srv_A")
			return err
		}); err != nil {
			t.Fatal(err)
		}
		if !recorded || found.Actor != "user:usr_1" {
			t.Fatalf("on record: %t, %+v", recorded, found)
		}
		var change AddressChange
		if err := json.Unmarshal([]byte(found.Detail), &change); err != nil {
			t.Fatal(err)
		}
		return change
	}
	if got := latest(ActionPublicAddress); got != (AddressChange{Address: "198.51.100.6"}) {
		t.Errorf("first set: %+v", got)
	}
	// L5: what it replaced goes on record, so it can be put back.
	if _, err := h.SetPublicAddress(t.Context(), "srv_A", "198.51.100.7", false, by); err != nil {
		t.Fatal(err)
	}
	if got := latest(ActionPublicAddress); got != (AddressChange{Address: "198.51.100.7", Previous: "198.51.100.6"}) {
		t.Errorf("set again: %+v", got)
	}
	if _, err := h.SetPublicAddress(t.Context(), "srv_A", "", false, by); err != nil {
		t.Fatal(err)
	}
	if got := latest(ActionPublicAddressClear); got != (AddressChange{Previous: "198.51.100.7"}) {
		t.Errorf("cleared: %+v", got)
	}
}

// L6: one row that can't be read can't keep a control plane from starting.
func TestAnUnreadableSettingLeavesTheAddressUnset(t *testing.T) {
	_, db := newHub(t, "hz-1", "hz-2")
	if err := db.Update(t.Context(), func(tx *store.Tx) error {
		return tx.PutSetting(t.Context(), store.SettingPublicAddress("srv_A"), []byte("not json"))
	}); err != nil {
		t.Fatal(err)
	}
	h, err := New(t.Context(), db, slog.New(slog.DiscardHandler))
	if err != nil {
		t.Fatalf("the control plane didn't start: %v", err)
	}
	if len(h.List()) != 2 {
		t.Errorf("it knows %d servers", len(h.List()))
	}
	if got := network(t, h).GetIpv4(); got != nil {
		t.Errorf("it reads %v", got)
	}
}
