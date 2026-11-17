// SPDX-License-Identifier: AGPL-3.0-only

package dnsproviders

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/url"
	"strconv"
	"strings"
)

// What publishing a domain asks of Cloudflare beyond DNS-01's TXT records: a
// token's own rights, a zone's account and SSL mode, any record at a name, and a
// tunnel's configuration and connections (ADR 0104). Every call here reads, or
// writes one thing it names; none of them makes a credential.

// Rights are a token's own record: what it may do, as Cloudflare lists it.
type Rights struct {
	ID, Name, Status string
	Policies         []Policy
}

// Policy is one of a token's policies: the rights it grants or denies, and where.
type Policy struct {
	// Effect is "allow" or "deny".
	Effect string
	// Groups are its permission groups, by ID and Cloudflare's own name.
	Groups []PermissionGroup
	// Resources are what it covers, as Cloudflare scopes it: an account, or every
	// zone of one, or a zone, with their IDs in them.
	Resources map[string]any
}

// PermissionGroup is one right, by its ID, which doesn't change, and its name in
// the API, which says Write where the dashboard says Edit.
type PermissionGroup struct {
	ID, Name string
}

// VerifyAccountToken verifies a token an account owns, at that account's own
// address, and returns its ID and status. A token a user owns verifies at
// /user/tokens/verify, which refuses one an account owns.
func (c *Cloudflare) VerifyAccountToken(ctx context.Context, account string) (id, status string, err error) {
	var got struct {
		ID     string `json:"id"`
		Status string `json:"status"`
	}
	if _, err := c.call(ctx, http.MethodGet, "/accounts/"+url.PathEscape(account)+"/tokens/verify", nil, &got); err != nil {
		return "", "", err
	}
	return got.ID, got.Status, nil
}

// AccountTokenRights reads the token with id, of account, which needs API Tokens
// Read (or Write) on the account. It reads only a token's own record here: the
// check passes the ID its own verify returned.
func (c *Cloudflare) AccountTokenRights(ctx context.Context, account, id string) (Rights, error) {
	var got struct {
		ID       string `json:"id"`
		Name     string `json:"name"`
		Status   string `json:"status"`
		Policies []struct {
			Effect string `json:"effect"`
			Groups []struct {
				ID   string `json:"id"`
				Name string `json:"name"`
			} `json:"permission_groups"`
			Resources map[string]any `json:"resources"`
		} `json:"policies"`
	}
	if _, err := c.call(ctx, http.MethodGet, "/accounts/"+url.PathEscape(account)+"/tokens/"+url.PathEscape(id), nil, &got); err != nil {
		return Rights{}, err
	}
	out := Rights{ID: got.ID, Name: got.Name, Status: got.Status}
	for _, p := range got.Policies {
		pol := Policy{Effect: p.Effect, Resources: p.Resources}
		for _, g := range p.Groups {
			pol.Groups = append(pol.Groups, PermissionGroup{ID: g.ID, Name: g.Name})
		}
		out.Policies = append(out.Policies, pol)
	}
	return out, nil
}

// ZoneInfo is a zone as Cloudflare reads it: its name and the account that holds it.
type ZoneInfo struct {
	ID, Name, Account string
}

// ZoneByID reads the zone with id.
func (c *Cloudflare) ZoneByID(ctx context.Context, id string) (ZoneInfo, error) {
	var got struct {
		ID      string `json:"id"`
		Name    string `json:"name"`
		Account struct {
			ID string `json:"id"`
		} `json:"account"`
	}
	if _, err := c.call(ctx, http.MethodGet, "/zones/"+url.PathEscape(id), nil, &got); err != nil {
		return ZoneInfo{}, err
	}
	return ZoneInfo{ID: got.ID, Name: strings.ToLower(got.Name), Account: got.Account.ID}, nil
}

// SSLMode reads a zone's SSL/TLS mode, such as "flexible", "full" or "strict", and
// whether it may be changed. It needs Zone Settings Read.
func (c *Cloudflare) SSLMode(ctx context.Context, zone Zone) (mode string, editable bool, err error) {
	var got struct {
		Value    string `json:"value"`
		Editable bool   `json:"editable"`
	}
	if _, err := c.call(ctx, http.MethodGet, "/zones/"+url.PathEscape(zone.ID)+"/settings/ssl", nil, &got); err != nil {
		return "", false, err
	}
	return got.Value, got.Editable, nil
}

// Record is a DNS record as Cloudflare holds it: everything undo needs to write it
// back as it was.
type Record struct {
	ID      string `json:"id,omitempty"`
	Type    string `json:"type"`
	Name    string `json:"name"`
	Content string `json:"content"`
	Proxied bool   `json:"proxied"`
	// TTL is in seconds; 1 is Cloudflare's "automatic".
	TTL      int    `json:"ttl"`
	Comment  string `json:"comment"`
	Priority *int   `json:"priority,omitempty"`
}

// RecordsAt lists every record at name, a full name in zone, of every type.
func (c *Cloudflare) RecordsAt(ctx context.Context, zone Zone, name string) ([]Record, error) {
	return c.records(ctx, zone, url.Values{"name": {name}})
}

// RecordsCommented lists the records in zone whose comment starts with prefix, as
// Dockwell's own records' comments do.
func (c *Cloudflare) RecordsCommented(ctx context.Context, zone Zone, prefix string) ([]Record, error) {
	return c.records(ctx, zone, url.Values{"comment.startswith": {prefix}})
}

// maxRecordPages bounds a listing of records, 100 at a time.
const maxRecordPages = 20

func (c *Cloudflare) records(ctx context.Context, zone Zone, q url.Values) ([]Record, error) {
	var out []Record
	for page := 1; page <= maxRecordPages; page++ {
		q.Set("per_page", "100")
		q.Set("page", strconv.Itoa(page))
		var got []Record
		env, err := c.call(ctx, http.MethodGet, "/zones/"+url.PathEscape(zone.ID)+"/dns_records?"+q.Encode(), nil, &got)
		if err != nil {
			return nil, err
		}
		out = append(out, got...)
		if env.Info == nil || page >= env.Info.TotalPages {
			break
		}
	}
	return out, nil
}

// RecordByID reads one record.
func (c *Cloudflare) RecordByID(ctx context.Context, zone Zone, id string) (Record, error) {
	var got Record
	_, err := c.call(ctx, http.MethodGet, "/zones/"+url.PathEscape(zone.ID)+"/dns_records/"+url.PathEscape(id), nil, &got)
	return got, err
}

// CreateRecord writes r in zone and returns it as Cloudflare holds it, with its ID.
func (c *Cloudflare) CreateRecord(ctx context.Context, zone Zone, r Record) (Record, error) {
	r.ID = ""
	var got Record
	if _, err := c.call(ctx, http.MethodPost, "/zones/"+url.PathEscape(zone.ID)+"/dns_records", r, &got); err != nil {
		return Record{}, err
	}
	if got.ID == "" {
		return Record{}, &APIError{Provider: cloudflareName, Message: "wrote the record and said nothing of its ID"}
	}
	return got, nil
}

// Tunnel is a Cloudflare tunnel as its account lists it.
type Tunnel struct {
	ID     string `json:"id"`
	Name   string `json:"name"`
	Status string `json:"status"`
	// ConfigSource is "cloudflare" for a tunnel managed in the dashboard or the
	// API, and "local" for one whose routes are a file on its server.
	ConfigSource string `json:"config_src"`
	AccountTag   string `json:"account_tag"`
}

// TunnelByID reads one tunnel of account.
func (c *Cloudflare) TunnelByID(ctx context.Context, account, id string) (Tunnel, error) {
	var got Tunnel
	_, err := c.call(ctx, http.MethodGet, tunnelPath(account, id), nil, &got)
	return got, err
}

// Tunnels lists account's tunnels that aren't deleted.
func (c *Cloudflare) Tunnels(ctx context.Context, account string) ([]Tunnel, error) {
	var out []Tunnel
	for page := 1; page <= maxRecordPages; page++ {
		var got []Tunnel
		q := url.Values{"is_deleted": {"false"}, "per_page": {"100"}, "page": {strconv.Itoa(page)}}
		env, err := c.call(ctx, http.MethodGet, "/accounts/"+url.PathEscape(account)+"/cfd_tunnel?"+q.Encode(), nil, &got)
		if err != nil {
			return nil, err
		}
		out = append(out, got...)
		if env.Info == nil || page >= env.Info.TotalPages {
			break
		}
	}
	return out, nil
}

// CreateTunnel makes a remotely managed tunnel named name, with secret, base64 of
// 32 or more bytes the caller generated and sealed first. Cloudflare's answer holds
// the secret again, in its credentials file: only the ID and name are read from it,
// and the answer is dropped (ADR 0104, section 9).
func (c *Cloudflare) CreateTunnel(ctx context.Context, account, name, secret string) (Tunnel, error) {
	var got Tunnel
	body := map[string]string{"name": name, "config_src": "cloudflare", "tunnel_secret": secret}
	_, err := c.call(ctx, http.MethodPost, "/accounts/"+url.PathEscape(account)+"/cfd_tunnel", body, &got)
	return got, err
}

// DeleteTunnel deletes a tunnel with no connector running.
func (c *Cloudflare) DeleteTunnel(ctx context.Context, account, id string) error {
	_, err := c.call(ctx, http.MethodDelete, tunnelPath(account, id), nil, nil)
	return err
}

func tunnelPath(account, id string) string {
	return "/accounts/" + url.PathEscape(account) + "/cfd_tunnel/" + url.PathEscape(id)
}

// TunnelConfig is a remotely managed tunnel's configuration as read: its version,
// where its routes are managed, and the document itself, raw, so a write can put
// back exactly what was read with one rule changed (ADR 0104, section 4).
type TunnelConfig struct {
	Version int    `json:"version"`
	Source  string `json:"source"`
	// Config is the "config" object as Cloudflare sent it, or "null" for a tunnel
	// with no routes yet.
	Config json.RawMessage `json:"config"`
}

// Empty says the tunnel has no routes yet: no document to splice into.
func (t TunnelConfig) Empty() bool {
	trimmed := bytes.TrimSpace(t.Config)
	return len(trimmed) == 0 || bytes.Equal(trimmed, []byte("null"))
}

// ReadTunnelConfig reads a tunnel's configuration.
func (c *Cloudflare) ReadTunnelConfig(ctx context.Context, account, id string) (TunnelConfig, error) {
	var got TunnelConfig
	_, err := c.call(ctx, http.MethodGet, tunnelPath(account, id)+"/configurations", nil, &got)
	return got, err
}

// WriteTunnelConfig replaces a tunnel's whole configuration with config, raw, and
// returns the version Cloudflare gave it. Cloudflare checks no version: whoever
// writes last wins, which is why the caller reads, splices and checks first.
func (c *Cloudflare) WriteTunnelConfig(ctx context.Context, account, id string, config json.RawMessage) (TunnelConfig, error) {
	if !json.Valid(config) {
		return TunnelConfig{}, errors.New("the tunnel's configuration to write isn't JSON")
	}
	var got TunnelConfig
	_, err := c.call(ctx, http.MethodPut, tunnelPath(account, id)+"/configurations", map[string]json.RawMessage{"config": config}, &got)
	return got, err
}

// Connector is one cloudflared running a tunnel, by the ID it reports on /ready,
// with its connections to Cloudflare's edge.
type Connector struct {
	ID          string       `json:"id"`
	Version     string       `json:"version"`
	Connections []Connection `json:"conns"`
}

// Connection is one of a connector's connections, with the address it came from.
type Connection struct {
	Colo     string `json:"colo_name"`
	OriginIP string `json:"origin_ip"`
}

// TunnelConnectors lists the connectors running a tunnel now.
func (c *Cloudflare) TunnelConnectors(ctx context.Context, account, id string) ([]Connector, error) {
	var got []Connector
	_, err := c.call(ctx, http.MethodGet, tunnelPath(account, id)+"/connections", nil, &got)
	return got, err
}
