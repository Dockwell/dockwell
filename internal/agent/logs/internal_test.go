// SPDX-License-Identifier: AGPL-3.0-only

package logs

import (
	"testing"
)

func TestContainsFold(t *testing.T) {
	for _, tc := range []struct {
		text, query string
		want        bool
	}{
		{"GET /Cart 200", "cart", true},
		{"GET /CART 200", "cart", true},
		{"nothing here", "cart", false},
		{"car", "cart", false},
		{"", "cart", false},
		{"anything", "", true},
		{"xxcart", "cart", true},
		{"cartxx", "cart", true},
		{"cCart", "cart", true},
		{"1234", "23", true},
	} {
		if got := containsFold([]byte(tc.text), []byte(tc.query)); got != tc.want {
			t.Errorf("containsFold(%q, %q) = %v, want %v", tc.text, tc.query, got, tc.want)
		}
	}
}
