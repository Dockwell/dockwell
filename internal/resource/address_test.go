// SPDX-License-Identifier: AGPL-3.0-only

package resource

import (
	"encoding/json"
	"net/netip"
	"os"
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

func TestPointHereNeverNamesAPrivateAddress(t *testing.T) {
	for _, tc := range []struct {
		name string
		here []string
		want string
	}{
		{"one private address, as Oracle's", []string{"10.0.0.25"}, "Point an A record at this server's public address. Dockwell only sees 10.0.0.25 here, which is private, so the internet can't reach it."},
		{"two, and one shared", []string{"10.0.0.25", "100.64.3.2"}, "Point an A record at this server's public address. Dockwell only sees 10.0.0.25 and 100.64.3.2 here, which are private, so the internet can't reach it."},
		{"a public one beside a private one", []string{"10.0.0.5", "203.0.113.9"}, "Point an A or AAAA record at this server, 203.0.113.9."},
		{"two public", []string{"203.0.113.9", "2001:db8::9"}, "Point an A or AAAA record at this server, 203.0.113.9 or 2001:db8::9."},
		{"none known", nil, "Point an A or AAAA record at this server's public address."},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if got := PointHere(tc.here); got != tc.want {
				t.Errorf("got %q, want %q", got, tc.want)
			}
		})
	}
}

func TestBehindNATMeansOnlyPrivateAddresses(t *testing.T) {
	for _, tc := range []struct {
		here []string
		want bool
	}{
		{[]string{"10.0.0.25"}, true},
		{[]string{"100.64.3.2", "192.168.1.2"}, true},
		{[]string{"10.0.0.5", "203.0.113.9"}, false},
		{nil, false},
	} {
		if got := BehindNAT(tc.here); got != tc.want {
			t.Errorf("BehindNAT(%s) = %v, want %v", strings.Join(tc.here, ", "), got, tc.want)
		}
	}
}
