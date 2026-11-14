// SPDX-License-Identifier: AGPL-3.0-only

package dnsproviders

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/dockwell/dockwell/internal/control/dnsproviders/cftest"
)

const token = "cf-token-0123456789abcdefghijklmnopqrstu"

func standIn(t *testing.T, api *cftest.API) API {
	t.Helper()
	srv := httptest.NewServer(api)
	t.Cleanup(srv.Close)
	return &Cloudflare{Token: token, Base: srv.URL + "/client/v4", Client: srv.Client()}
}

// A token that may write DNS in a zone writes a TXT record there, reads it back,
// and deletes it, leaving the zone as it was (ADR 0088).
func TestACheckWritesARecordAndDeletesIt(t *testing.T) {
	cf := cftest.New(token, "example.com", "example.org")
	got := Check(t.Context(), standIn(t, cf), []string{"example.com"})
	if got.State != Writes || len(got.Zones) != 1 || !got.Zones[0].Writes {
		t.Fatalf("check = %+v", got)
	}
	if strings.Join(got.Reaches, ",") != "example.com,example.org" {
		t.Errorf("the token reaches %q", got.Reaches)
	}
	if left := cf.Records("example.com"); len(left) != 0 {
		t.Errorf("the check left %v in the zone", left)
	}
	wrote := false
	for _, a := range cf.Asked {
		wrote = wrote || strings.HasPrefix(a, "POST /client/v4/zones/zone-example-com/dns_records")
	}
	if !wrote {
		t.Errorf("the check wrote nothing: %q", cf.Asked)
	}
}

// With no zone asked for, it lists what the token reaches and writes nothing.
func TestACheckWithNoZoneListsWhatTheTokenReaches(t *testing.T) {
	cf := cftest.New(token, "example.com")
	got := Check(t.Context(), standIn(t, cf), nil)
	if got.State != Reaches || strings.Join(got.Reaches, ",") != "example.com" {
		t.Errorf("check = %+v", got)
	}
	for _, a := range cf.Asked {
		if strings.HasPrefix(a, "POST ") {
			t.Errorf("a check with no zone wrote: %s", a)
		}
	}
}

// What the provider refuses, it says in its own words, and a Global API Key never
// goes to it.
func TestACheckSaysWhatTheProviderSaid(t *testing.T) {
	readOnly := cftest.New(token, "example.com", "example.org")
	readOnly.Tokens[token] = cftest.Token{Reads: []string{"example.com", "example.org"}, Writes: []string{"example.org"}}
	for what, c := range map[string]struct {
		api   *cftest.API
		token string
		zones []string
		state State
		says  string
	}{
		"a token it doesn't know": {cftest.New("another", "example.com"), token, []string{"example.com"}, Refused, "Cloudflare: Invalid API Token (1000)"},
		"a token that only reads": {readOnly, token, []string{"example.com", "example.org"}, CantWrite, "Cloudflare: Authentication error (10000)"},
		"a zone it doesn't reach": {cftest.New(token, "example.com"), token, []string{"example.net"}, CantWrite, "doesn't reach example.net"},
		"a Global API Key":        {cftest.New(token, "example.com"), "0123456789abcdef0123456789abcdef01234", nil, Refused, "Global API Key"},
	} {
		srv := httptest.NewServer(c.api)
		got := Check(t.Context(), &Cloudflare{Token: c.token, Base: srv.URL + "/client/v4", Client: srv.Client()}, c.zones)
		srv.Close()
		if got.State != c.state || !strings.Contains(got.Detail, c.says) {
			t.Errorf("%s: %+v", what, got)
		}
		if strings.Contains(got.Detail, c.token) {
			t.Errorf("%s: the check says the token: %s", what, got.Detail)
		}
	}
	if got := Check(t.Context(), &Cloudflare{Token: token, Base: "http://127.0.0.1:1", Client: http.DefaultClient}, nil); got.State != Unreachable {
		t.Errorf("a provider that isn't there = %+v", got)
	}
}

// A record that was written goes, whatever reading it back said.
func TestACheckDeletesWhatItWroteWhateverHappens(t *testing.T) {
	cf := cftest.New(token, "example.com")
	api := &failRead{API: standIn(t, cf)}
	got := Check(t.Context(), api, []string{"example.com"})
	if got.State != CantWrite {
		t.Errorf("check = %+v", got)
	}
	if left := cf.Records("example.com"); len(left) != 0 {
		t.Errorf("the check left %v in the zone after a failed read", left)
	}
}

type failRead struct {
	API
	reads int
}

// TXTAt fails the read that follows the write, and answers what's there after.
func (f *failRead) TXTAt(ctx context.Context, zone Zone, name string) ([]TXT, error) {
	f.reads++
	if f.reads == 1 {
		return nil, &APIError{Provider: "Cloudflare", Status: 500, Code: 1001, Message: "Internal error"}
	}
	return f.API.TXTAt(ctx, zone, name)
}

// A write whose answer was lost may have been written all the same, so what's at its
// name goes too; one Cloudflare refused wrote nothing. (Review of #605, L3.)
func TestACheckCleansUpAWriteWhoseAnswerWasLost(t *testing.T) {
	cf := cftest.New(token, "example.com")
	cf.LoseWrites = true
	got := Check(t.Context(), standIn(t, cf), []string{"example.com"})
	if got.State != CantWrite {
		t.Errorf("check = %+v", got)
	}
	if left := cf.Records("example.com"); len(left) != 0 {
		t.Errorf("a write whose answer was lost left %v in the zone", left)
	}
}

// A token an account owns verifies at its account's address alone, so one that reads
// zones is good where /user/tokens/verify calls it invalid. (Review of #605, L4.)
func TestAnAccountsTokenIsGood(t *testing.T) {
	cf := cftest.New(token, "example.com")
	cf.Tokens[token] = cftest.Token{Reads: []string{"example.com"}, Writes: []string{"example.com"}, Account: true}
	if got := Check(t.Context(), standIn(t, cf), []string{"example.com"}); got.State != Writes {
		t.Errorf("an account's token = %+v", got)
	}
	if got := Check(t.Context(), standIn(t, cftest.New("another", "example.com")), nil); got.State != Refused {
		t.Errorf("a token Cloudflare doesn't know = %+v", got)
	}
}

// Many zones fit in a check's time, a few at a time, each written and deleted.
func TestACheckWritesInManyZones(t *testing.T) {
	var zones []string
	for i := range 12 {
		zones = append(zones, "z"+strings.Repeat("a", i+1)+".example.com")
	}
	cf := cftest.New(token, zones...)
	got := Check(t.Context(), standIn(t, cf), zones)
	if got.State != Writes || len(got.Zones) != len(zones) {
		t.Fatalf("check = %+v", got)
	}
	for i, z := range got.Zones {
		if z.Zone != zones[i] || !z.Writes {
			t.Errorf("zone %d = %+v", i, z)
		}
		if left := cf.Records(z.Zone); len(left) != 0 {
			t.Errorf("%s holds %v", z.Zone, left)
		}
	}
}
