// SPDX-License-Identifier: AGPL-3.0-only

package dnsproviders

import (
	"context"
	"errors"
	"net/http"
	"net/url"
	"slices"
	"strings"
)

// One Cloudflare token may carry every right publishing needs, and each is checked
// and shown on its own, from the token's own record, never by writing (ADR 0104,
// section 6, amending ADR 0099).

// Have is whether a token has a capability.
type Have int

// What a capability's check can come to.
const (
	// Unknown is a right that couldn't be read: never taken as present.
	Unknown Have = iota
	// Has is a right its record grants.
	Has
	// Lacks is a right its record doesn't grant.
	Lacks
)

// Capabilities are what a token can do for publishing, each on its own.
type Capabilities struct {
	// DNS writes records in its zones; Tunnels publishes through the account's
	// tunnels; ZoneSettings reads a zone's SSL mode; OwnRights reads its own record,
	// which makes the rest checkable.
	DNS, Tunnels, ZoneSettings, OwnRights Have
	// MakesTokens says it holds a right that makes credentials, which a token
	// Dockwell holds shouldn't (ADR 0099).
	MakesTokens bool
	// TunnelCount is how many tunnels it may edit, where it may: Tunnel Edit covers
	// every tunnel of its account.
	TunnelCount int
	// Account is the account it was verified at, for a token an account owns; empty
	// for a user's.
	Account string
	// FixURL is Cloudflare's page for the token, where a missing right is added.
	FixURL string
}

// Rights by the IDs of their permission groups, as a token's record lists them, with
// their names in the API as the fallback for a right whose ID isn't known here: the
// API says Write where Cloudflare's dashboard says Edit.
var (
	dnsWrite = rightSet{
		ids:   []string{"4755a26eedb94da69e1066d98aa820be"},
		names: []string{"DNS Write"},
	}
	tunnelWrite = rightSet{
		ids:   []string{"c07321b023e944ff818fec44d8203567", "037b9e348b3b42d4b46ea2fcb1cfb3e7"},
		names: []string{"Cloudflare Tunnel Write", "Cloudflare One Connector: cloudflared Write"},
	}
	zoneSettings = rightSet{
		ids:   []string{"3030687196b94b638145a3953da2b699"},
		names: []string{"Zone Settings Write", "Zone Settings Read"},
	}
	// makesCredentials are rights that make credentials: other tokens, and Access's
	// service tokens.
	makesCredentials = rightSet{
		ids:   []string{"5bc3f8b21c554832afc660159ab75fa4", "a1c0fec57cf94af79479a6d827fa518c"},
		names: []string{"Account API Tokens Write", "API Tokens Write", "Access: Service Tokens Write"},
	}
)

type rightSet struct{ ids, names []string }

// in says an allowed policy of r grants one of the set, and no denying one takes
// it away.
func (s rightSet) in(r Rights) bool {
	match := func(g PermissionGroup) bool {
		return slices.Contains(s.ids, g.ID) || slices.ContainsFunc(s.names, func(n string) bool { return strings.EqualFold(n, g.Name) })
	}
	allowed := false
	for _, p := range r.Policies {
		if !slices.ContainsFunc(p.Groups, match) {
			continue
		}
		if p.Effect == "deny" {
			return false
		}
		allowed = true
	}
	return allowed
}

func have(b bool) Have {
	if b {
		return Has
	}
	return Lacks
}

// tokenPages are Cloudflare's pages where a token's rights are changed: a user's,
// and an account's.
const (
	userTokensPage    = "https://dash.cloudflare.com/profile/api-tokens" //nolint:gosec // a page's address, not a credential
	accountTokensPage = "https://dash.cloudflare.com/%s/api-tokens"      //nolint:gosec // a page's address, not a credential
)

// CheckCapabilities works out what the token can do for publishing, each right on
// its own, from its own record: a user's token at /user/tokens, an account's at its
// account's address, tried for each account of its zones. dnsWrites is what the DNS
// check found by writing, which stands for DNS where the record can't be read.
func (c *Cloudflare) CheckCapabilities(ctx context.Context, accounts []string, dnsWrites bool) Capabilities {
	out := Capabilities{DNS: Unknown, Tunnels: Unknown, ZoneSettings: Unknown, OwnRights: Unknown, FixURL: userTokensPage}
	if dnsWrites {
		out.DNS = Has
	}
	var rights Rights
	var err error
	if id, ok := c.userTokenID(ctx); ok {
		rights, err = c.userTokenRights(ctx, id)
	} else {
		err = errors.New("the token verified at no account")
		for _, account := range accounts {
			id, status, verr := c.VerifyAccountToken(ctx, account)
			if verr != nil || status != "active" {
				continue
			}
			out.Account, out.FixURL = account, strings.Replace(accountTokensPage, "%s", url.PathEscape(account), 1)
			rights, err = c.AccountTokenRights(ctx, account, id)
			break
		}
	}
	if err != nil {
		var api *APIError
		if errors.As(err, &api) && (api.Status == http.StatusForbidden || api.Code == 9109) {
			out.OwnRights = Lacks
		}
		// What it can be seen to read is read; what it may write isn't known.
		if n, terr := c.tunnelCount(ctx, accounts); terr == nil {
			out.TunnelCount = n
		}
		return out
	}
	out.OwnRights = Has
	out.DNS = have(dnsWrite.in(rights) || dnsWrites)
	out.Tunnels = have(tunnelWrite.in(rights))
	out.ZoneSettings = have(zoneSettings.in(rights))
	out.MakesTokens = makesCredentials.in(rights)
	if out.Tunnels == Has {
		if n, terr := c.tunnelCount(ctx, accounts); terr == nil {
			out.TunnelCount = n
		}
	}
	return out
}

// userTokenID is a user's token's ID, from /user/tokens/verify, which refuses a token
// an account owns.
func (c *Cloudflare) userTokenID(ctx context.Context) (string, bool) {
	var got struct {
		ID     string `json:"id"`
		Status string `json:"status"`
	}
	if _, err := c.call(ctx, http.MethodGet, "/user/tokens/verify", nil, &got); err != nil || got.ID == "" || got.Status != "active" {
		return "", false
	}
	return got.ID, true
}

// userTokenRights reads a user's token's own record, which needs API Tokens Read.
func (c *Cloudflare) userTokenRights(ctx context.Context, id string) (Rights, error) {
	return c.tokenRights(ctx, "/user/tokens/"+url.PathEscape(id))
}

func (c *Cloudflare) tunnelCount(ctx context.Context, accounts []string) (int, error) {
	n := 0
	var last error
	for _, account := range accounts {
		tunnels, err := c.Tunnels(ctx, account)
		if err != nil {
			last = err
			continue
		}
		n += len(tunnels)
	}
	if n == 0 && last != nil {
		return 0, last
	}
	return n, nil
}
