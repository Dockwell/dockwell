// SPDX-License-Identifier: AGPL-3.0-only

package dnsproviders

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"time"

	"github.com/dockwell/dockwell/internal/netguard"
	"github.com/dockwell/dockwell/internal/resource/dnsprovider"
)

// CloudflareAPI is Cloudflare's API, which a test may point elsewhere.
const CloudflareAPI = "https://api.cloudflare.com/client/v4"

// Doer sends a request, as http.Client's Do does.
type Doer interface {
	Do(*http.Request) (*http.Response, error)
}

// Cloudflare writes records through Cloudflare's API, with an API token.
type Cloudflare struct {
	// Token is the API token, which goes in the Authorization header alone.
	Token string
	// Base is the API's address: CloudflareAPI unless a test's.
	Base string
	// Client reaches it: netguard's, for the internet alone, unless a test's.
	Client Doer
}

// NewCloudflare reaches Cloudflare's own API, through netguard, with token.
func NewCloudflare(token string) *Cloudflare {
	return &Cloudflare{Token: token, Base: CloudflareAPI, Client: netguard.Client(netguard.Options{MaxBody: 4 << 20, Timeout: 15 * time.Second})}
}

// cloudflareName is the provider's name in what it says.
const cloudflareName = "Cloudflare"

// APIError is what a provider's API said when it said no, in its own words.
type APIError struct {
	Provider string
	Status   int
	Code     int
	Message  string
}

func (e *APIError) Error() string {
	if e.Code != 0 {
		return fmt.Sprintf("%s: %s (%d)", e.Provider, e.Message, e.Code)
	}
	return fmt.Sprintf("%s: %s", e.Provider, e.Message)
}

// Refused says the provider refused the token itself, as opposed to one action
// the token may not take: Cloudflare's "Invalid API Token" and "Invalid request
// headers".
func (e *APIError) Refused() bool {
	return e.Status == http.StatusUnauthorized || e.Code == 1000 || e.Code == 6003
}

// ErrGlobalKey is a Cloudflare Global API Key offered where an API token goes, which
// is never sent.
var ErrGlobalKey = dnsprovider.ErrGlobalKey

// envelope is the shape of every answer from Cloudflare's API.
type envelope struct {
	Success bool            `json:"success"`
	Errors  []apiMessage    `json:"errors"`
	Result  json.RawMessage `json:"result"`
	Info    *struct {
		Page       int `json:"page"`
		TotalPages int `json:"total_pages"`
	} `json:"result_info"`
}

type apiMessage struct {
	Code    int    `json:"code"`
	Message string `json:"message"`
}

func (c *Cloudflare) call(ctx context.Context, method, path string, body, into any) (*envelope, error) {
	var payload io.Reader
	if body != nil {
		b, err := json.Marshal(body)
		if err != nil {
			return nil, err
		}
		payload = bytes.NewReader(b)
	}
	req, err := http.NewRequestWithContext(ctx, method, strings.TrimSuffix(c.Base, "/")+path, payload)
	if err != nil {
		return nil, err
	}
	req.Header.Set("Authorization", "Bearer "+c.Token)
	req.Header.Set("Accept", "application/json")
	if body != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	resp, err := c.Client.Do(req)
	if err != nil {
		return nil, fmt.Errorf("can't reach Cloudflare: %s", netguard.Words(err))
	}
	defer resp.Body.Close()
	raw, err := io.ReadAll(io.LimitReader(resp.Body, 4<<20))
	if err != nil {
		return nil, fmt.Errorf("can't read Cloudflare's answer: %w", err)
	}
	var env envelope
	if err := json.Unmarshal(raw, &env); err != nil {
		return nil, &APIError{Provider: cloudflareName, Status: resp.StatusCode, Message: fmt.Sprintf("answered %d with something that isn't its API's JSON", resp.StatusCode)}
	}
	if !env.Success || resp.StatusCode >= 300 {
		e := &APIError{Provider: cloudflareName, Status: resp.StatusCode, Message: http.StatusText(resp.StatusCode)}
		if len(env.Errors) > 0 {
			e.Code, e.Message = env.Errors[0].Code, oneLine(env.Errors[0].Message)
		}
		return nil, e
	}
	if into != nil {
		if err := json.Unmarshal(env.Result, into); err != nil {
			return nil, fmt.Errorf("can't read Cloudflare's answer: %w", err)
		}
	}
	return &env, nil
}

// oneLine is s on one line, cut to 300 bytes, as a provider's words are shown.
func oneLine(s string) string {
	s = strings.Join(strings.Fields(s), " ")
	if len(s) > 300 {
		s = s[:300]
	}
	return s
}

// Verify says whether the token is a working API token.
func (c *Cloudflare) Verify(ctx context.Context) error {
	if dnsprovider.IsGlobalKey(c.Token) {
		return ErrGlobalKey
	}
	var got struct {
		Status string `json:"status"`
	}
	if _, err := c.call(ctx, http.MethodGet, "/user/tokens/verify", nil, &got); err != nil {
		// A token an account owns verifies only at its account's own address, which
		// isn't known yet, and here reads as invalid (1000): it's good if it reads zones.
		var api *APIError
		if errors.As(err, &api) && api.Code == 1000 {
			if _, zerr := c.call(ctx, http.MethodGet, "/zones?per_page=1", nil, nil); zerr == nil {
				return nil
			}
		}
		return err
	}
	if got.Status != "active" {
		return &APIError{Provider: cloudflareName, Status: http.StatusUnauthorized, Message: "the token is " + oneLine(got.Status) + ", not active"}
	}
	return nil
}

// maxZonePages bounds how many pages of zones are read, 50 at a time.
const maxZonePages = 20

// Zones lists the zones the token reaches.
func (c *Cloudflare) Zones(ctx context.Context) ([]Zone, error) {
	var out []Zone
	for page := 1; page <= maxZonePages; page++ {
		var got []struct {
			ID   string `json:"id"`
			Name string `json:"name"`
		}
		env, err := c.call(ctx, http.MethodGet, "/zones?per_page=50&page="+strconv.Itoa(page), nil, &got)
		if err != nil {
			return nil, err
		}
		for _, z := range got {
			out = append(out, Zone{ID: z.ID, Name: strings.ToLower(z.Name)})
		}
		if env.Info == nil || page >= env.Info.TotalPages {
			break
		}
	}
	return out, nil
}

// AppendTXT adds a TXT record of value at name, a full name in zone, and returns
// its ID.
func (c *Cloudflare) AppendTXT(ctx context.Context, zone Zone, name, value string) (string, error) {
	var got struct {
		ID string `json:"id"`
	}
	body := map[string]any{"type": "TXT", "name": name, "content": strconv.Quote(value), "ttl": 60}
	if _, err := c.call(ctx, http.MethodPost, "/zones/"+url.PathEscape(zone.ID)+"/dns_records", body, &got); err != nil {
		return "", err
	}
	if got.ID == "" {
		return "", &APIError{Provider: cloudflareName, Message: "wrote the record and said nothing of its ID"}
	}
	return got.ID, nil
}

// TXTAt lists the TXT records at name, a full name in zone, as the API reads them.
func (c *Cloudflare) TXTAt(ctx context.Context, zone Zone, name string) ([]TXT, error) {
	var got []struct {
		ID      string `json:"id"`
		Content string `json:"content"`
	}
	q := url.Values{"type": {"TXT"}, "name": {name}}
	if _, err := c.call(ctx, http.MethodGet, "/zones/"+url.PathEscape(zone.ID)+"/dns_records?"+q.Encode(), nil, &got); err != nil {
		return nil, err
	}
	out := make([]TXT, 0, len(got))
	for _, r := range got {
		out = append(out, TXT{ID: r.ID, Value: strings.Trim(r.Content, `"`)})
	}
	return out, nil
}

// Delete removes the record with id from zone.
func (c *Cloudflare) Delete(ctx context.Context, zone Zone, id string) error {
	_, err := c.call(ctx, http.MethodDelete, "/zones/"+url.PathEscape(zone.ID)+"/dns_records/"+url.PathEscape(id), nil, nil)
	return err
}
