// SPDX-License-Identifier: AGPL-3.0-only

// Package cftest is a stand-in for Cloudflare's API, as much of it as Dockwell uses:
// a token's verify and its own rights, its zones with their accounts and SSL modes,
// a zone's DNS records of every type, and an account's tunnels, each with a
// versioned configuration document and its connectors. Tests and the panel's
// end-to-end run reach it in place of Cloudflare (ADR 0088, ADR 0104). It behaves as
// Cloudflare was checked to on 2026-11-17 (ADR 0104, "Verified").
package cftest

import (
	"bytes"
	"cmp"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"net/http"
	"reflect"
	"slices"
	"strings"
	"sync"
)

// Account is the account every zone and tunnel of the stand-in is in.
const Account = "acc-0123456789abcdef"

// API is the stand-in. Tokens says what each token may do; a token not in it is
// refused, as Cloudflare refuses one it doesn't know.
type API struct {
	mu sync.Mutex
	// Tokens are the tokens it takes, by token.
	Tokens map[string]Token
	// Zones are the zones it holds, by name.
	Zones []string
	// SSL is each zone's SSL/TLS mode; "full" where it says none.
	SSL map[string]string
	// records are the records written, by zone, then ID.
	records map[string]map[string]Record
	// Tunnels are the account's tunnels, by ID.
	Tunnels map[string]*Tunnel
	next    int
	// Asked logs each request, as "METHOD path", with no token in it.
	Asked []string
	// LoseWrites writes each record, then answers 502, as a write whose answer
	// was lost on its way back.
	LoseWrites bool
	// FailCreate answers a write of a record at a name it says with 500, writing
	// nothing, as a POST that failed after the delete before it landed.
	FailCreate func(name string) bool
	// TooMany answers each request it says yes to with 429, as Cloudflare's rate
	// limit does, given "METHOD path".
	TooMany func(asked string) bool
	// BeforePut runs as a tunnel's configuration is written, before it's taken, with
	// the stand-in unlocked: a test writes there, as someone in the dashboard would
	// between Dockwell's read and its write.
	BeforePut func(tunnel string)
}

// Token is what a token may do.
type Token struct {
	// Reads are the zones it reaches; Writes those it may write DNS in.
	Reads, Writes []string
	// Account is a token an account owns, which /user/tokens/verify calls invalid,
	// as Cloudflare does: it verifies at its account's address alone.
	Account bool
	// Rights are its permission groups by their names in the API, such as "DNS
	// Write" or "Cloudflare Tunnel Write", which its own record lists. Nil gives
	// "Zone Read", with "DNS Write" where it writes.
	Rights []string
	// ID is its ID; one is made up where it's empty.
	ID string
}

// Record is a DNS record.
type Record struct {
	ID      string `json:"id"`
	Type    string `json:"type"`
	Name    string `json:"name"`
	Content string `json:"content"`
	Proxied bool   `json:"proxied"`
	TTL     int    `json:"ttl"`
	Comment string `json:"comment"`
}

// Tunnel is one of the account's tunnels.
type Tunnel struct {
	ID, Name string
	// Local says its routes are a file on its server, not kept here.
	Local bool
	// Config is its document; nil for one with no routes yet.
	Config json.RawMessage
	// Version moves by one for each write that changes Config.
	Version int
	// Connectors are the cloudflared runs that hold it, each with the addresses
	// its connections came from.
	Connectors []Connector
	// Deleted says it was deleted: it still reads, with deleted_at, as Cloudflare's
	// do, and its configuration answers 404 with 1055.
	Deleted bool
}

// Connector is one run of cloudflared, by the ID its /ready gives.
type Connector struct {
	ID        string
	OriginIPs []string
}

// IDs are the permission groups' IDs Cloudflare gives them, for those Dockwell
// checks: read from a token's own record on 2026-11-17 for the writes, made up for
// the rest.
var IDs = map[string]string{ //nolint:gosec // permission groups' public IDs, not credentials
	"DNS Write":               "4755a26eedb94da69e1066d98aa820be",
	"Zone Write":              "e6d2666161e84845a636613608cee8d5",
	"Zone Settings Write":     "3030687196b94b638145a3953da2b699",
	"Cloudflare Tunnel Write": "c07321b023e944ff818fec44d8203567",
	"Cloudflare One Connector: cloudflared Write": "037b9e348b3b42d4b46ea2fcb1cfb3e7",
	"Account API Tokens Write":                    "5bc3f8b21c554832afc660159ab75fa4",
	"Access: Service Tokens Write":                "a1c0fec57cf94af79479a6d827fa518c",
}

// New is a stand-in that holds zones, and takes token for each of them, reading and
// writing.
func New(token string, zones ...string) *API {
	return &API{Tokens: map[string]Token{token: {Reads: zones, Writes: zones}}, Zones: zones}
}

// Records are the records in zone now, by name.
func (a *API) Records(zone string) []Record {
	a.mu.Lock()
	defer a.mu.Unlock()
	var out []Record
	for _, r := range a.records[zone] {
		out = append(out, r)
	}
	slices.SortFunc(out, func(x, y Record) int { return strings.Compare(x.Name+x.Type, y.Name+y.Type) })
	return out
}

// Put writes a record in zone as someone in the dashboard would, and returns its ID.
func (a *API) Put(zone string, r Record) string {
	a.mu.Lock()
	defer a.mu.Unlock()
	return a.put(zone, r)
}

func (a *API) put(zone string, r Record) string {
	if a.records == nil {
		a.records = map[string]map[string]Record{}
	}
	if a.records[zone] == nil {
		a.records[zone] = map[string]Record{}
	}
	a.next++
	r.ID = fmt.Sprintf("rec-%d", a.next)
	if r.TTL == 0 {
		r.TTL = 1
	}
	a.records[zone][r.ID] = r
	return r.ID
}

// Remove deletes a record by its ID, as someone in the dashboard would.
func (a *API) Remove(zone, id string) {
	a.mu.Lock()
	defer a.mu.Unlock()
	delete(a.records[zone], id)
}

// AddTunnel adds a tunnel to the account, as the dashboard makes one.
func (a *API) AddTunnel(t *Tunnel) {
	a.mu.Lock()
	defer a.mu.Unlock()
	if a.Tunnels == nil {
		a.Tunnels = map[string]*Tunnel{}
	}
	a.Tunnels[t.ID] = t
}

// Document is a tunnel's document now, and its version.
func (a *API) Document(tunnel string) (json.RawMessage, int) {
	a.mu.Lock()
	defer a.mu.Unlock()
	t := a.Tunnels[tunnel]
	if t == nil {
		return nil, 0
	}
	return bytes.Clone(t.Config), t.Version
}

// SetDocument writes a tunnel's whole document, as the dashboard saves one.
func (a *API) SetDocument(tunnel string, doc json.RawMessage) {
	a.mu.Lock()
	defer a.mu.Unlock()
	a.setDocument(a.Tunnels[tunnel], doc)
}

func (a *API) setDocument(t *Tunnel, doc json.RawMessage) {
	if !sameJSON(t.Config, doc) {
		t.Version++
	}
	t.Config = bytes.Clone(doc)
}

// zoneID is a zone's ID, as Cloudflare gives one.
func zoneID(zone string) string { return "zone-" + strings.ReplaceAll(zone, ".", "-") }

// ZoneID is a zone's ID in the stand-in.
func ZoneID(zone string) string { return zoneID(zone) }

func (a *API) zoneOf(id string) string {
	for _, z := range a.Zones {
		if zoneID(z) == id {
			return z
		}
	}
	return ""
}

func (t Token) rights() []string {
	if t.Rights != nil {
		return t.Rights
	}
	out := []string{"Zone Read"}
	if len(t.Writes) > 0 {
		out = append(out, "DNS Write")
	}
	return out
}

func (t Token) has(names ...string) bool {
	return slices.ContainsFunc(t.rights(), func(r string) bool { return slices.Contains(names, r) })
}

// Calls are the requests asked so far, as Asked logs them.
func (a *API) Calls() []string {
	a.mu.Lock()
	defer a.mu.Unlock()
	return slices.Clone(a.Asked)
}

// Limit sets TooMany while requests may be served.
func (a *API) Limit(tooMany func(asked string) bool) {
	a.mu.Lock()
	a.TooMany = tooMany
	a.mu.Unlock()
}

func (a *API) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	a.mu.Lock()
	locked := true
	defer func() {
		if locked {
			a.mu.Unlock()
		}
	}()
	a.Asked = append(a.Asked, r.Method+" "+r.URL.Path)
	if a.TooMany != nil && a.TooMany(r.Method+" "+r.URL.Path) {
		fail(w, http.StatusTooManyRequests, 971, "Please wait and consider throttling your request speed")
		return
	}
	token, ok := strings.CutPrefix(r.Header.Get("Authorization"), "Bearer ")
	t, known := a.Tokens[token]
	if !ok || !known {
		fail(w, http.StatusBadRequest, 1000, "Invalid API Token")
		return
	}
	if t.ID == "" {
		sum := sha256.Sum256([]byte(token))
		t.ID = "tok" + hex.EncodeToString(sum[:])[:29]
	}
	path := strings.TrimPrefix(r.URL.Path, "/client/v4")
	parts := strings.Split(strings.Trim(path, "/"), "/")
	switch {
	case r.Method == http.MethodGet && path == "/user/tokens/verify" && t.Account:
		fail(w, http.StatusUnauthorized, 1000, "Invalid API Token")
	case r.Method == http.MethodGet && path == "/user/tokens/verify":
		succeed(w, map[string]string{"id": t.ID, "status": "active"}, nil)
	case r.Method == http.MethodGet && len(parts) == 3 && parts[0] == "user" && parts[1] == "tokens" && !t.Account:
		a.ownRecord(w, t, parts[2], "API Tokens Read", "API Tokens Write")
	case r.Method == http.MethodGet && path == "/zones":
		var zones []map[string]any
		for _, z := range a.Zones {
			if slices.Contains(t.Reads, z) {
				zones = append(zones, map[string]any{"id": zoneID(z), "name": z, "account": map[string]string{"id": Account}})
			}
		}
		succeed(w, zones, map[string]int{"page": 1, "total_pages": 1})
	case strings.HasPrefix(path, "/zones/"):
		a.zone(w, r, t, parts[1:])
	case len(parts) >= 2 && parts[0] == "accounts" && parts[1] != Account:
		fail(w, http.StatusForbidden, 9109, "Unauthorized to access requested resource")
	case len(parts) == 4 && parts[2] == "tokens" && parts[3] == "verify" && r.Method == http.MethodGet:
		if !t.Account {
			fail(w, http.StatusUnauthorized, 1000, "Invalid API Token")
			return
		}
		succeed(w, map[string]string{"id": t.ID, "status": "active"}, nil)
	case len(parts) == 4 && parts[2] == "tokens" && r.Method == http.MethodGet && t.Account:
		a.ownRecord(w, t, parts[3], "Account API Tokens Read", "Account API Tokens Write")
	case len(parts) >= 3 && parts[2] == "cfd_tunnel":
		locked = false
		a.tunnels(w, r, t, parts[3:])
	default:
		fail(w, http.StatusNotFound, 7003, "Could not route to "+path+", perhaps your object identifier is invalid?")
	}
}

// ownRecord answers a token's own record, which it reads with one of rights.
func (a *API) ownRecord(w http.ResponseWriter, t Token, id string, rights ...string) {
	if id != t.ID || !t.has(rights...) {
		fail(w, http.StatusForbidden, 9109, "Unauthorized to access requested resource")
		return
	}
	var groups []map[string]string
	for _, name := range t.rights() {
		groupID := IDs[name]
		if groupID == "" {
			groupID = fmt.Sprintf("%x", name)
		}
		groups = append(groups, map[string]string{"id": groupID, "name": name})
	}
	succeed(w, map[string]any{
		"id": t.ID, "name": "Dockwell", "status": "active",
		"policies": []any{map[string]any{
			"effect": "allow", "permission_groups": groups,
			"resources": map[string]any{"com.cloudflare.api.account." + Account: "*"},
		}},
	}, nil)
}

func (a *API) zone(w http.ResponseWriter, r *http.Request, t Token, parts []string) {
	zone := a.zoneOf(parts[0])
	if zone == "" || !slices.Contains(t.Reads, zone) {
		fail(w, http.StatusForbidden, 10000, "Authentication error")
		return
	}
	switch {
	case len(parts) == 1 && r.Method == http.MethodGet:
		succeed(w, map[string]any{"id": zoneID(zone), "name": zone, "account": map[string]string{"id": Account}}, nil)
	case len(parts) == 3 && parts[1] == "settings" && parts[2] == "ssl" && r.Method == http.MethodGet:
		if !t.has("Zone Settings Read", "Zone Settings Write") {
			fail(w, http.StatusForbidden, 9109, "Unauthorized to access requested resource")
			return
		}
		mode := a.SSL[zone]
		if mode == "" {
			mode = "full"
		}
		succeed(w, map[string]any{"id": "ssl", "value": mode, "editable": true}, nil)
	case len(parts) == 3 && parts[1] == "settings" && parts[2] == "ssl" && r.Method == http.MethodPatch:
		if !t.has("Zone Settings Write") {
			fail(w, http.StatusForbidden, 9109, "Unauthorized to access requested resource")
			return
		}
		var body struct {
			Value string `json:"value"`
		}
		if err := json.NewDecoder(r.Body).Decode(&body); err != nil || !slices.Contains([]string{"off", "flexible", "full", "strict"}, body.Value) {
			fail(w, http.StatusBadRequest, 1007, "Invalid value for zone setting ssl")
			return
		}
		if a.SSL == nil {
			a.SSL = map[string]string{}
		}
		a.SSL[zone] = body.Value
		succeed(w, map[string]any{"id": "ssl", "value": body.Value, "editable": true}, nil)
	case len(parts) >= 2 && parts[1] == "dns_records":
		a.records2(w, r, t, zone, parts[2:])
	default:
		fail(w, http.StatusNotFound, 7003, "Could not route")
	}
}

func (a *API) records2(w http.ResponseWriter, r *http.Request, t Token, zone string, parts []string) {
	if a.records == nil {
		a.records = map[string]map[string]Record{}
	}
	if a.records[zone] == nil {
		a.records[zone] = map[string]Record{}
	}
	q := r.URL.Query()
	// An ID that isn't a record's has no route at Cloudflare (checked 2026-11-17).
	if len(parts) == 1 && strings.ContainsAny(parts[0], ":%") {
		fail(w, http.StatusBadRequest, 7000, "No route for that URI")
		return
	}
	switch {
	case r.Method == http.MethodGet && len(parts) == 0:
		var out []Record
		for _, rec := range a.records[zone] {
			if n := q.Get("name"); n != "" && rec.Name != n {
				continue
			}
			if ty := q.Get("type"); ty != "" && rec.Type != ty {
				continue
			}
			if c := q.Get("comment.startswith"); c != "" && !strings.HasPrefix(rec.Comment, c) {
				continue
			}
			if c := q.Get("content"); c != "" && rec.Content != c {
				continue
			}
			if q.Get("proxied") == "true" && !rec.Proxied {
				continue
			}
			out = append(out, rec)
		}
		slices.SortFunc(out, func(x, y Record) int { return strings.Compare(x.ID, y.ID) })
		succeed(w, out, map[string]int{"page": 1, "total_pages": 1})
	case r.Method == http.MethodGet && len(parts) == 1:
		rec, ok := a.records[zone][parts[0]]
		if !ok {
			fail(w, http.StatusNotFound, 81044, "Record does not exist.")
			return
		}
		succeed(w, rec, nil)
	case !slices.Contains(t.Writes, zone):
		fail(w, http.StatusForbidden, 10000, "Authentication error")
	case r.Method == http.MethodPost && len(parts) == 0:
		var body Record
		if err := json.NewDecoder(r.Body).Decode(&body); err != nil || body.Type == "" || body.Content == "" {
			fail(w, http.StatusBadRequest, 9005, "Content for the record is invalid")
			return
		}
		if body.Name != zone && !strings.HasSuffix(body.Name, "."+zone) {
			fail(w, http.StatusBadRequest, 9007, "Record name is not in the zone")
			return
		}
		// A CNAME sits beside TXT and MX, and neither it nor an address beside an
		// address or a CNAME (Verified, 4).
		addressLike := func(ty string) bool { return ty == "A" || ty == "AAAA" || ty == "CNAME" }
		for _, rec := range a.records[zone] {
			if rec.Name == body.Name && addressLike(rec.Type) && addressLike(body.Type) && (rec.Type == "CNAME" || body.Type == "CNAME") {
				fail(w, http.StatusBadRequest, 81053, "An A, AAAA, or CNAME record with that host already exists.")
				return
			}
		}
		if a.FailCreate != nil && a.FailCreate(body.Name) {
			fail(w, http.StatusInternalServerError, 10000, "Internal server error")
			return
		}
		id := a.put(zone, body)
		if a.LoseWrites {
			w.WriteHeader(http.StatusBadGateway)
			return
		}
		succeed(w, a.records[zone][id], nil)
	case r.Method == http.MethodDelete && len(parts) == 1:
		if _, ok := a.records[zone][parts[0]]; !ok {
			fail(w, http.StatusNotFound, 81044, "Record does not exist.")
			return
		}
		delete(a.records[zone], parts[0])
		succeed(w, map[string]string{"id": parts[0]}, nil)
	default:
		fail(w, http.StatusMethodNotAllowed, 10000, "Method not allowed")
	}
}

// tunnels answers the account's tunnels, with the stand-in locked, but unlocked while
// BeforePut runs.
func (a *API) tunnels(w http.ResponseWriter, r *http.Request, t Token, parts []string) {
	defer a.mu.Unlock()
	reads := t.has("Cloudflare Tunnel Read", "Cloudflare Tunnel Write", "Cloudflare One Connector: cloudflared Write")
	writes := t.has("Cloudflare Tunnel Write", "Cloudflare One Connector: cloudflared Write")
	if !reads || r.Method != http.MethodGet && !writes {
		fail(w, http.StatusForbidden, 10000, "Authentication error")
		return
	}
	if a.Tunnels == nil {
		a.Tunnels = map[string]*Tunnel{}
	}
	if len(parts) == 0 {
		switch r.Method {
		case http.MethodGet:
			var out []map[string]any
			for _, tun := range a.Tunnels {
				out = append(out, a.tunnelJSON(tun))
			}
			slices.SortFunc(out, func(x, y map[string]any) int { return strings.Compare(x["id"].(string), y["id"].(string)) })
			succeed(w, out, map[string]int{"page": 1, "total_pages": 1})
		case http.MethodPost:
			var body struct {
				Name   string `json:"name"`
				Secret string `json:"tunnel_secret"`
			}
			if err := json.NewDecoder(r.Body).Decode(&body); err != nil || body.Name == "" || body.Secret == "" {
				fail(w, http.StatusBadRequest, 1001, "Invalid tunnel")
				return
			}
			a.next++
			tun := &Tunnel{ID: fmt.Sprintf("00000000-0000-4000-8000-%012d", a.next), Name: body.Name}
			a.Tunnels[tun.ID] = tun
			out := a.tunnelJSON(tun)
			out["credentials_file"] = map[string]string{"AccountTag": Account, "TunnelID": tun.ID, "TunnelSecret": body.Secret}
			succeed(w, out, nil)
		default:
			fail(w, http.StatusMethodNotAllowed, 10000, "Method not allowed")
		}
		return
	}
	tun := a.Tunnels[parts[0]]
	if tun == nil {
		fail(w, http.StatusNotFound, 1003, "Tunnel not found")
		return
	}
	switch {
	case len(parts) == 1 && r.Method == http.MethodGet:
		succeed(w, a.tunnelJSON(tun), nil)
	case len(parts) == 1 && r.Method == http.MethodDelete:
		if len(tun.Connectors) > 0 {
			fail(w, http.StatusBadRequest, 1022, "This tunnel has active connections. Please stop all cloudflared replicas, or wait a few minutes for connections to close, then try again.")
			return
		}
		tun.Deleted = true
		succeed(w, a.tunnelJSON(tun), nil)
	case tun.Deleted && len(parts) == 2 && parts[1] == "configurations":
		fail(w, http.StatusNotFound, 1055, "Tunnel not found")
	case len(parts) == 2 && parts[1] == "connections" && r.Method == http.MethodGet:
		var out []map[string]any
		for _, c := range tun.Connectors {
			var conns []map[string]string
			for _, ip := range c.OriginIPs {
				conns = append(conns, map[string]string{"colo_name": "yvr01", "origin_ip": ip})
			}
			out = append(out, map[string]any{"id": c.ID, "version": "2026.5.2", "conns": conns})
		}
		succeed(w, out, nil)
	case len(parts) == 2 && parts[1] == "configurations" && r.Method == http.MethodGet:
		source := "cloudflare"
		if tun.Local {
			source = "local"
		}
		config := tun.Config
		if config == nil {
			config = json.RawMessage("null")
		}
		succeed(w, map[string]any{"tunnel_id": tun.ID, "version": tun.Version, "config": config, "source": source}, nil)
	case len(parts) == 2 && parts[1] == "configurations" && r.Method == http.MethodPut:
		var body struct {
			Config json.RawMessage `json:"config"`
		}
		if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
			fail(w, http.StatusBadRequest, 1001, "Bad Configuration")
			return
		}
		if msg := validate(body.Config); msg != "" {
			fail(w, http.StatusBadRequest, 1001, "Bad Configuration: Validation failed: "+msg)
			return
		}
		if a.BeforePut != nil {
			a.mu.Unlock()
			a.BeforePut(tun.ID)
			a.mu.Lock()
		}
		a.setDocument(tun, body.Config)
		succeed(w, map[string]any{"tunnel_id": tun.ID, "version": tun.Version, "config": tun.Config, "source": "cloudflare"}, nil)
	default:
		fail(w, http.StatusNotFound, 7003, "Could not route")
	}
}

func (a *API) tunnelJSON(t *Tunnel) map[string]any {
	src, status := "cloudflare", "inactive"
	if t.Local {
		src = "local"
	}
	if len(t.Connectors) > 0 {
		status = "healthy"
	}
	out := map[string]any{"id": t.ID, "name": t.Name, "status": status, "config_src": src, "account_tag": Account, "deleted_at": nil}
	if t.Deleted {
		out["status"], out["deleted_at"] = "inactive", "2026-11-17T09:00:00Z"
	}
	return out
}

// EditInPlace changes the record id in zone as an edit in the dashboard does,
// keeping its ID.
func (a *API) EditInPlace(zone, id string, change func(*Record)) {
	a.mu.Lock()
	defer a.mu.Unlock()
	if r, ok := a.records[zone][id]; ok {
		change(&r)
		a.records[zone][id] = r
	}
}

// Mode is the zone's SSL mode, as the dashboard shows it.
func (a *API) Mode(zone string) string {
	a.mu.Lock()
	defer a.mu.Unlock()
	return cmp.Or(a.SSL[zone], "full")
}

// SetMode sets the zone's SSL mode, as someone in the dashboard does.
func (a *API) SetMode(zone, mode string) {
	a.mu.Lock()
	defer a.mu.Unlock()
	if a.SSL == nil {
		a.SSL = map[string]string{}
	}
	a.SSL[zone] = mode
}

// OnPut sets BeforePut while requests may be served.
func (a *API) OnPut(before func(tunnel string)) {
	a.mu.Lock()
	a.BeforePut = before
	a.mu.Unlock()
}

// SetToken sets what token may do while requests may be served; nil revokes it.
func (a *API) SetToken(token string, t *Token) {
	a.mu.Lock()
	defer a.mu.Unlock()
	if t == nil {
		delete(a.Tokens, token)
		return
	}
	a.Tokens[token] = *t
}

// DeleteTunnel deletes the tunnel id, as the dashboard does once its connections
// closed.
func (a *API) DeleteTunnel(id string) {
	a.mu.Lock()
	defer a.mu.Unlock()
	if t := a.Tunnels[id]; t != nil {
		t.Deleted, t.Connectors = true, nil
	}
}

// validate checks a document as Cloudflare does: a catch-all last and nowhere
// else, and a wildcard only as the leftmost label (Verified, 2).
func validate(doc json.RawMessage) string {
	var c struct {
		Ingress []struct {
			Hostname, Path, Service string
		} `json:"ingress"`
	}
	if err := json.Unmarshal(doc, &c); err != nil {
		return "the document isn't JSON"
	}
	if len(c.Ingress) == 0 {
		return "The last ingress rule must match all URLs (i.e. it should not have a hostname or path filter)"
	}
	for i, r := range c.Ingress {
		all := r.Hostname == "" && r.Path == ""
		if all && i < len(c.Ingress)-1 {
			return fmt.Sprintf("Rule #%d is matching the hostname '', but this will match every hostname, meaning the rules which follow it will never be triggered.", i+1)
		}
		if !all && i == len(c.Ingress)-1 {
			return "The last ingress rule must match all URLs (i.e. it should not have a hostname or path filter)"
		}
		if strings.Count(r.Hostname, "*") > 1 || strings.Contains(r.Hostname, "*") && !strings.HasPrefix(r.Hostname, "*.") {
			return `Hostname patterns can have at most one wildcard character ("*") and it can only be used for subdomains, e.g. "*.example.com"`
		}
		if r.Service == "" {
			return "a rule has no service"
		}
	}
	return ""
}

func sameJSON(a, b json.RawMessage) bool {
	var x, y any
	_ = json.Unmarshal(a, &x)
	_ = json.Unmarshal(b, &y)
	return reflect.DeepEqual(x, y)
}

func succeed(w http.ResponseWriter, result, info any) {
	w.Header().Set("Content-Type", "application/json")
	body := map[string]any{"success": true, "errors": []any{}, "result": result}
	if info != nil {
		body["result_info"] = info
	}
	_ = json.NewEncoder(w).Encode(body)
}

func fail(w http.ResponseWriter, status, code int, message string) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(map[string]any{"success": false, "errors": []any{map[string]any{"code": code, "message": message}}, "result": nil})
}
