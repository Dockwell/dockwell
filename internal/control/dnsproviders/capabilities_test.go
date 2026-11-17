// SPDX-License-Identifier: AGPL-3.0-only

package dnsproviders

import (
	"net/http/httptest"
	"testing"

	"github.com/dockwell/dockwell/internal/control/dnsproviders/cftest"
)

func capsOf(t *testing.T, tok cftest.Token, tunnels int) Capabilities {
	t.Helper()
	api := cftest.New("token", "example.com")
	tok.Reads, tok.Writes = []string{"example.com"}, []string{"example.com"}
	api.Tokens["token"] = tok
	for i := range tunnels {
		api.AddTunnel(&cftest.Tunnel{ID: "00000000-0000-4000-8000-00000000000" + string(rune('1'+i)), Name: "web"})
	}
	srv := httptest.NewServer(api)
	t.Cleanup(srv.Close)
	cf := &Cloudflare{Token: "token", Base: srv.URL + "/client/v4", Client: srv.Client()}
	return cf.CheckCapabilities(t.Context(), []string{cftest.Account}, true)
}

// Each right is checked on its own, from the token's own record (ADR 0104, N5).
func TestCapabilities(t *testing.T) {
	all := capsOf(t, cftest.Token{Account: true, Rights: []string{"Zone Read", "DNS Write", "Cloudflare Tunnel Write", "Zone Settings Read", "Account API Tokens Read"}}, 3)
	if all.DNS != Has || all.Tunnels != Has || all.ZoneSettings != Has || all.OwnRights != Has || all.MakesTokens || all.TunnelCount != 3 ||
		all.Account != cftest.Account || all.FixURL != "https://dash.cloudflare.com/"+cftest.Account+"/api-tokens" {
		t.Errorf("an account's token with every right: %+v", all)
	}

	noTunnel := capsOf(t, cftest.Token{Account: true, Rights: []string{"Zone Read", "DNS Write", "Account API Tokens Read"}}, 1)
	if noTunnel.Tunnels != Lacks || noTunnel.ZoneSettings != Lacks || noTunnel.DNS != Has {
		t.Errorf("a token without the tunnel right: %+v", noTunnel)
	}

	// A token that can't read its own record: what's proven by writing stands, and
	// nothing else is taken as present.
	blind := capsOf(t, cftest.Token{Account: true, Rights: []string{"Zone Read", "DNS Write", "Cloudflare Tunnel Write"}}, 2)
	if blind.OwnRights != Lacks || blind.Tunnels != Unknown || blind.ZoneSettings != Unknown || blind.DNS != Has || blind.TunnelCount != 2 {
		t.Errorf("a token that can't read its own rights: %+v", blind)
	}

	// A token that can make tokens says so.
	maker := capsOf(t, cftest.Token{Account: true, Rights: []string{"Zone Read", "DNS Write", "Cloudflare Tunnel Write", "Account API Tokens Write"}}, 0)
	if !maker.MakesTokens || maker.OwnRights != Has {
		t.Errorf("a token that makes tokens: %+v", maker)
	}

	// A user's token reads its own record at /user/tokens.
	user := capsOf(t, cftest.Token{Rights: []string{"Zone Read", "DNS Write", "Cloudflare Tunnel Write", "API Tokens Read"}}, 0)
	if user.OwnRights != Has || user.Tunnels != Has || user.Account != "" || user.FixURL != "https://dash.cloudflare.com/profile/api-tokens" {
		t.Errorf("a user's token: %+v", user)
	}
}

// A policy that denies a right takes it away, whatever another allows.
func TestADenyingPolicyWins(t *testing.T) {
	r := Rights{Policies: []Policy{
		{Effect: "allow", Groups: []PermissionGroup{{ID: "c07321b023e944ff818fec44d8203567", Name: "Cloudflare Tunnel Write"}}},
		{Effect: "deny", Groups: []PermissionGroup{{ID: "c07321b023e944ff818fec44d8203567", Name: "Cloudflare Tunnel Write"}}},
	}}
	if tunnelWrite.in(r) {
		t.Error("a denied right")
	}
	// By its ID alone, whatever its name says.
	if !dnsWrite.in(Rights{Policies: []Policy{{Effect: "allow", Groups: []PermissionGroup{{ID: "4755a26eedb94da69e1066d98aa820be", Name: "renamed"}}}}}) {
		t.Error("a right by its ID")
	}
}
