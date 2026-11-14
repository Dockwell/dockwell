// SPDX-License-Identifier: AGPL-3.0-only

// Package cftest is a stand-in for Cloudflare's API, as much of it as Dockwell
// writes DNS with: a token's verify, its zones, and a zone's TXT records. Tests
// and the panel's end-to-end run reach it in place of Cloudflare (ADR 0088).
package cftest

import (
	"encoding/json"
	"fmt"
	"net/http"
	"slices"
	"strings"
	"sync"
)

// API is the stand-in. Tokens says what each token may do; a token not in it is
// refused, as Cloudflare refuses one it doesn't know.
type API struct {
	mu sync.Mutex
	// Tokens are the tokens it takes, by token.
	Tokens map[string]Token
	// Zones are the zones it holds, by name.
	Zones []string
	// records are the TXT records written, by zone, then ID.
	records map[string]map[string]Record
	next    int
	// Asked logs each request, as "METHOD path", with no token in it.
	Asked []string
	// LoseWrites writes each record, then answers 502, as a write whose answer
	// was lost on its way back.
	LoseWrites bool
}

// Token is what a token may do.
type Token struct {
	// Reads are the zones it reaches; Writes those it may write DNS in.
	Reads, Writes []string
	// Account is a token an account owns, which /user/tokens/verify calls invalid,
	// as Cloudflare does: it verifies at its account's address alone.
	Account bool
}

// Record is a TXT record written.
type Record struct {
	Name, Content string
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
	slices.SortFunc(out, func(x, y Record) int { return strings.Compare(x.Name, y.Name) })
	return out
}

// zoneID is a zone's ID, as Cloudflare gives one.
func zoneID(zone string) string { return "zone-" + strings.ReplaceAll(zone, ".", "-") }

func (a *API) zoneOf(id string) string {
	for _, z := range a.Zones {
		if zoneID(z) == id {
			return z
		}
	}
	return ""
}

func (a *API) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	a.mu.Lock()
	defer a.mu.Unlock()
	a.Asked = append(a.Asked, r.Method+" "+r.URL.Path)
	token, ok := strings.CutPrefix(r.Header.Get("Authorization"), "Bearer ")
	t, known := a.Tokens[token]
	if !ok || !known {
		fail(w, http.StatusBadRequest, 1000, "Invalid API Token")
		return
	}
	path := strings.TrimPrefix(r.URL.Path, "/client/v4")
	switch {
	case r.Method == http.MethodGet && path == "/user/tokens/verify" && t.Account:
		fail(w, http.StatusUnauthorized, 1000, "Invalid API Token")
	case r.Method == http.MethodGet && path == "/user/tokens/verify":
		succeed(w, map[string]string{"id": "tok", "status": "active"}, nil)
	case r.Method == http.MethodGet && path == "/zones":
		var zones []map[string]string
		for _, z := range a.Zones {
			if slices.Contains(t.Reads, z) {
				zones = append(zones, map[string]string{"id": zoneID(z), "name": z})
			}
		}
		succeed(w, zones, map[string]int{"page": 1, "total_pages": 1})
	case strings.HasPrefix(path, "/zones/"):
		a.records2(w, r, t, strings.Split(strings.TrimPrefix(path, "/zones/"), "/"))
	default:
		fail(w, http.StatusNotFound, 7003, "Could not route to "+path+", perhaps your object identifier is invalid?")
	}
}

func (a *API) records2(w http.ResponseWriter, r *http.Request, t Token, parts []string) {
	zone := a.zoneOf(parts[0])
	if zone == "" || len(parts) < 2 || parts[1] != "dns_records" || !slices.Contains(t.Reads, zone) {
		fail(w, http.StatusForbidden, 10000, "Authentication error")
		return
	}
	if a.records == nil {
		a.records = map[string]map[string]Record{}
	}
	if a.records[zone] == nil {
		a.records[zone] = map[string]Record{}
	}
	switch {
	case r.Method == http.MethodGet && len(parts) == 2:
		var out []map[string]string
		for id, rec := range a.records[zone] {
			if rec.Name == r.URL.Query().Get("name") {
				out = append(out, map[string]string{"id": id, "type": "TXT", "name": rec.Name, "content": rec.Content})
			}
		}
		succeed(w, out, nil)
	case !slices.Contains(t.Writes, zone):
		fail(w, http.StatusForbidden, 10000, "Authentication error")
	case r.Method == http.MethodPost && len(parts) == 2:
		var body struct {
			Type, Name, Content string
		}
		if err := json.NewDecoder(r.Body).Decode(&body); err != nil || body.Type != "TXT" {
			fail(w, http.StatusBadRequest, 9005, "Content for TXT record is invalid")
			return
		}
		if body.Name != zone && !strings.HasSuffix(body.Name, "."+zone) {
			fail(w, http.StatusBadRequest, 9007, "Record name is not in the zone")
			return
		}
		a.next++
		id := fmt.Sprintf("rec-%d", a.next)
		a.records[zone][id] = Record{Name: body.Name, Content: body.Content}
		if a.LoseWrites {
			w.WriteHeader(http.StatusBadGateway)
			return
		}
		succeed(w, map[string]string{"id": id}, nil)
	case r.Method == http.MethodDelete && len(parts) == 3:
		if _, ok := a.records[zone][parts[2]]; !ok {
			fail(w, http.StatusNotFound, 81044, "Record does not exist.")
			return
		}
		delete(a.records[zone], parts[2])
		succeed(w, map[string]string{"id": parts[2]}, nil)
	default:
		fail(w, http.StatusMethodNotAllowed, 10000, "Method not allowed")
	}
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
