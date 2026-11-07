// SPDX-License-Identifier: AGPL-3.0-only

package resource

import (
	"bytes"
	"encoding/csv"
	"encoding/json"
	"net/netip"
	"os"
	"slices"
	"strings"
	"testing"
)

// The panel's isPrivateAddress reads this table too, so the two can't drift apart.
func TestWhichAddressesArePublic(t *testing.T) {
	raw, err := os.ReadFile("testdata/addresses.json")
	if err != nil {
		t.Fatal(err)
	}
	var table []struct {
		Address string `json:"address"`
		Public  bool   `json:"public"`
	}
	if err := json.Unmarshal(raw, &table); err != nil {
		t.Fatal(err)
	}
	for _, tc := range table {
		if got := IsPublicAddress(netip.MustParseAddr(tc.Address)); got != tc.Public {
			t.Errorf("IsPublicAddress(%s) = %v, want %v", tc.Address, got, tc.Public)
		}
	}
}

// The registries are IANA's own, and this holds the definition to their "Globally
// Reachable" column at both ends of every range, and the table to it too, so the
// panel answers for every range as well. A column that says neither leaves it to
// the range around it.
func TestEveryRangeFollowsIANA(t *testing.T) {
	type row struct {
		prefix  netip.Prefix
		reached *bool
	}
	var rows []row
	for _, file := range []string{"testdata/iana-special-registry-v4.csv", "testdata/iana-special-registry-v6.csv"} {
		raw, err := os.ReadFile(file)
		if err != nil {
			t.Fatal(err)
		}
		records, err := csv.NewReader(bytes.NewReader(raw)).ReadAll()
		if err != nil {
			t.Fatal(err)
		}
		for _, record := range records[1:] {
			word, _, _ := strings.Cut(record[8], " ")
			var reached *bool
			if word == "True" || word == "False" {
				reached = new(bool)
				*reached = word == "True"
			}
			for _, block := range strings.Split(record[0], ",") {
				spelled, _, _ := strings.Cut(strings.TrimSpace(block), " ")
				rows = append(rows, row{netip.MustParsePrefix(spelled), reached})
			}
		}
	}
	if len(rows) < 50 {
		t.Fatalf("read %d ranges from the registries", len(rows))
	}
	want := func(a netip.Addr) bool {
		a = a.Unmap()
		for _, doc := range []string{"198.51.100.0/24", "203.0.113.0/24", "2001:db8::/32"} {
			if netip.MustParsePrefix(doc).Contains(a) {
				return true // tests and examples use them as a server's public address
			}
		}
		bits, reached := -1, true
		for _, r := range rows {
			if r.reached != nil && r.prefix.Bits() > bits && r.prefix.Contains(a) {
				bits, reached = r.prefix.Bits(), *r.reached
			}
		}
		return reached
	}
	raw, err := os.ReadFile("testdata/addresses.json")
	if err != nil {
		t.Fatal(err)
	}
	var table []struct {
		Address string `json:"address"`
		Public  bool   `json:"public"`
	}
	if err := json.Unmarshal(raw, &table); err != nil {
		t.Fatal(err)
	}
	inTable := map[string]bool{}
	for _, tc := range table {
		inTable[tc.Address] = tc.Public
	}
	for _, r := range rows {
		first := r.prefix.Masked().Addr()
		raw := first.AsSlice()
		for bit := r.prefix.Bits(); bit < len(raw)*8; bit++ {
			raw[bit/8] |= 0x80 >> (bit % 8)
		}
		last, _ := netip.AddrFromSlice(raw)
		for _, a := range []netip.Addr{first, last} {
			if got := IsPublicAddress(a); got != want(a) {
				t.Errorf("IsPublicAddress(%s) = %v, want %v, in %s", a, got, want(a), r.prefix)
			}
			public, listed := inTable[a.String()]
			if !listed {
				t.Errorf("the table has no row for %s, an end of %s, so the panel isn't held to it", a, r.prefix)
			} else if public != want(a) {
				t.Errorf("the table says %s is %v, want %v", a, public, want(a))
			}
		}
	}
}

func TestPointHereNeverNamesAPrivateAddress(t *testing.T) {
	for _, tc := range []struct {
		name  string
		check DomainCheck
		want  string
	}{
		{"one private address, as Oracle's", DomainCheck{Here: []string{"10.0.0.25"}}, "Point an A record at this server's public address. Dockwell only sees 10.0.0.25 here, which is private, so the internet can't reach it."},
		{"two, and one shared", DomainCheck{Here: []string{"10.0.0.25", "100.64.3.2"}}, "Point an A record at this server's public address. Dockwell only sees 10.0.0.25 and 100.64.3.2 here, which are private, so the internet can't reach it."},
		{"a public one beside a private one", DomainCheck{Here: []string{"10.0.0.5", "203.0.113.9"}}, "Point an A record at this server, 203.0.113.9."},
		{"only IPv6", DomainCheck{Here: []string{"10.0.0.5", "2001:db8::9"}}, "Point an AAAA record at this server, 2001:db8::9."},
		{"two public", DomainCheck{Here: []string{"203.0.113.9", "2001:db8::9"}}, "Point an A or AAAA record at this server, 203.0.113.9 or 2001:db8::9."},
		{"none known", DomainCheck{}, "Point an A or AAAA record at this server's public address."},
		{"one known beyond its interfaces", DomainCheck{Public: []string{"203.0.113.50"}, Here: []string{"10.0.0.25", "203.0.113.50"}}, "Point an A record at this server, 203.0.113.50."},
		{"one a person set, over an interface's", DomainCheck{Public: []string{"198.51.100.7"}, Here: []string{"203.0.113.9", "198.51.100.7"}}, "Point an A record at this server, 198.51.100.7."},
		{"a looked-up address, not on a known cloud", DomainCheck{Public: []string{"203.0.113.7"}, Unproven: []string{"203.0.113.7"}, Here: []string{"192.168.1.20", "203.0.113.7"}}, "Point an A record at 203.0.113.7, which is how this server reaches the internet. If it's behind a router, that record won't reach it without a forwarded port, and a tunnel needs neither."},
		{"behind carrier NAT", DomainCheck{Carrier: true, Here: []string{"100.64.3.2"}}, "This server is behind carrier NAT (100.64.3.2), so a DNS record can't reach it, and neither can a forwarded port. Use a tunnel, which needs neither."},
		{"a TUN's, as mihomo's, is no public address", DomainCheck{Here: []string{"172.16.0.5", "198.18.0.1"}}, "Point an A record at this server's public address. Dockwell only sees 172.16.0.5 and 198.18.0.1 here, which are private, so the internet can't reach it."},
		{"three, as the panel lists them", DomainCheck{Here: []string{"10.0.0.5", "10.0.0.6", "10.0.0.7"}}, "Point an A record at this server's public address. Dockwell only sees 10.0.0.5, 10.0.0.6 and 10.0.0.7 here, which are private, so the internet can't reach it."},
		{"three public", DomainCheck{Here: []string{"203.0.113.1", "203.0.113.2", "2001:db8::9"}}, "Point an A or AAAA record at this server, 203.0.113.1, 203.0.113.2 or 2001:db8::9."},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if got := tc.check.PointHere(); got != tc.want {
				t.Errorf("got %q, want %q", got, tc.want)
			}
		})
	}
}

func TestBehindNATMeansOnlyPrivateAddresses(t *testing.T) {
	for _, tc := range []struct {
		public []string
		here   []string
		want   bool
	}{
		{nil, []string{"10.0.0.25"}, true},
		{nil, []string{"100.64.3.2", "192.168.1.2"}, true},
		{[]string{"203.0.113.50"}, []string{"10.0.0.25", "203.0.113.50"}, true},
		{nil, []string{"172.16.0.5", "198.18.0.1"}, true},
		{nil, []string{"10.0.0.5", "203.0.113.9"}, false},
		{[]string{"203.0.113.9"}, []string{"203.0.113.9"}, false},
		{nil, nil, false},
	} {
		check := &DomainCheck{Public: tc.public, Here: tc.here}
		if got := check.BehindNAT(); got != tc.want {
			t.Errorf("BehindNAT of %v among %s = %v, want %v", tc.public, strings.Join(tc.here, ", "), got, tc.want)
		}
	}
}

// Rule 5 of ADR 0034: what's known of one family says nothing of the other, so each
// record is told apart, and a way out keeps its caveat beside a way in.
func TestEachFamilyHasItsOwnWords(t *testing.T) {
	const caveat = "which is how this server reaches the internet. If it's behind a router, that record won't reach it without a forwarded port, and a tunnel needs neither."
	for _, tc := range []struct {
		name  string
		check DomainCheck
		want  string
	}{
		{
			"an AAAA on an interface, and only a way out for an A",
			DomainCheck{Public: []string{"203.0.113.7"}, Unproven: []string{"203.0.113.7"}, Here: []string{"10.0.0.5", "2001:db8::5", "203.0.113.7"}},
			"Point an AAAA record at this server, 2001:db8::5. Point an A record at 203.0.113.7, " + caveat,
		},
		{
			"a way in told for each",
			DomainCheck{Public: []string{"203.0.113.50", "2001:db8::9"}, Here: []string{"10.0.0.25", "203.0.113.50", "2001:db8::9"}},
			"Point an A or AAAA record at this server, 203.0.113.50 or 2001:db8::9.",
		},
		{
			"a person's IPv4 address, over one on an interface, keeps the interface's IPv6",
			DomainCheck{Public: []string{"198.51.100.7"}, Here: []string{"203.0.113.9", "2001:db8::5", "198.51.100.7"}},
			"Point an A or AAAA record at this server, 198.51.100.7 or 2001:db8::5.",
		},
		{
			"carrier words wait until nothing can be named",
			DomainCheck{Carrier: true, Public: []string{"198.51.100.7"}, Here: []string{"100.72.1.2", "198.51.100.7"}},
			"Point an A record at this server, 198.51.100.7.",
		},
		{
			"a private address told by an agent gone wrong is never named",
			DomainCheck{Public: []string{"10.0.0.9"}, Here: []string{"10.0.0.5"}},
			"Point an A record at this server's public address. Dockwell only sees 10.0.0.5 here, which is private, so the internet can't reach it.",
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if got := tc.check.PointHere(); got != tc.want {
				t.Errorf("got %q, want %q", got, tc.want)
			}
		})
	}
}

// Under carrier-grade NAT every address it has is shared. A VPN's beside an address
// of the server's own, as Tailscale's, says nothing of that.
func TestCarrierNATMeansOnlySharedAddresses(t *testing.T) {
	for _, tc := range []struct {
		here    []string
		carrier bool
		forward []string
	}{
		{[]string{"100.72.1.2"}, true, nil},
		{[]string{"100.72.1.2", "100.64.0.1"}, true, nil},
		{[]string{"192.168.1.20", "100.72.1.2"}, false, []string{"192.168.1.20"}},
		{[]string{"192.168.1.20", "10.0.0.5"}, false, []string{"192.168.1.20", "10.0.0.5"}},
		{[]string{"192.168.1.20", "198.18.0.1"}, false, []string{"192.168.1.20", "198.18.0.1"}},
		{[]string{"203.0.113.9"}, false, nil},
		{nil, false, nil},
	} {
		if got := BehindCarrierNAT(tc.here); got != tc.carrier {
			t.Errorf("BehindCarrierNAT(%v) = %v, want %v", tc.here, got, tc.carrier)
		}
		if got := ForwardTo(tc.here); !slices.Equal(got, tc.forward) {
			t.Errorf("ForwardTo(%v) = %v, want %v", tc.here, got, tc.forward)
		}
	}
}
