// SPDX-License-Identifier: AGPL-3.0-only

// Package dnsproviders writes DNS records through a provider's API for the control
// plane: the live check of a provider's token, and the TXT records that prove a
// name by DNS-01. A provider's token stays here, on the control plane, and goes to
// no server. See docs/adr/0088-certificates-by-dns-01-and-wildcard-domains.md.
package dnsproviders

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"errors"
	"fmt"
	"slices"
	"strings"
	"sync"
	"time"

	resourcev1 "github.com/dockwell/dockwell/gen/dockwell/resource/v1alpha1"
	"github.com/dockwell/dockwell/internal/resource/dnsprovider"
	"github.com/dockwell/dockwell/internal/secrets"
	"github.com/dockwell/dockwell/internal/store"
)

// Zone is a zone a token reaches, as the provider names it.
type Zone struct {
	ID, Name string
}

// TXT is a TXT record, by its ID at the provider, with its value.
type TXT struct {
	ID, Value string
}

// API is what Dockwell asks of a DNS provider's API.
type API interface {
	// Verify says whether the token is one the provider takes.
	Verify(ctx context.Context) error
	// Zones lists the zones the token reaches.
	Zones(ctx context.Context) ([]Zone, error)
	// AppendTXT adds a TXT record of value at name, and returns its ID.
	AppendTXT(ctx context.Context, zone Zone, name, value string) (string, error)
	// TXTAt lists the TXT records at name.
	TXTAt(ctx context.Context, zone Zone, name string) ([]TXT, error)
	// Delete removes a record by its ID.
	Delete(ctx context.Context, zone Zone, id string) error
}

// Providers reads the DNS providers an admin connected, and reaches their APIs.
type Providers struct {
	db     *store.DB
	keeper *secrets.Keeper
	// Cloudflare reaches Cloudflare's API with a token: its own, through
	// netguard, unless a test's.
	Cloudflare func(token string) API
}

// New reads providers from db, opened with keeper.
func New(db *store.DB, keeper *secrets.Keeper) *Providers {
	return &Providers{db: db, keeper: keeper, Cloudflare: func(token string) API { return NewCloudflare(token) }}
}

// Saved is a provider as saved, with its token: it goes no further than this
// package.
type Saved struct {
	ID, Name string
	Revision int
	Spec     *resourcev1.DnsProvider
}

// All returns every saved provider, sorted by name.
func (p *Providers) All(ctx context.Context) ([]Saved, error) {
	var out []Saved
	err := p.db.View(ctx, func(tx *store.Tx) error {
		stored, err := tx.ListKind(ctx, string(dnsprovider.Kind))
		if err != nil {
			return err
		}
		for i := range stored {
			r := &stored[i]
			if r.Spec == nil {
				continue
			}
			spec, err := p.keeper.Decode(ctx, tx, store.InRevision(r.ID, r.Kind, r.Revision), r.Spec)
			if err != nil {
				return fmt.Errorf("can't read the DNS provider %s: %w", r.Name, err)
			}
			if d, ok := spec.(*resourcev1.DnsProvider); ok {
				out = append(out, Saved{ID: r.ID, Name: r.Name, Revision: r.Revision, Spec: d})
			}
		}
		return nil
	})
	slices.SortFunc(out, func(a, b Saved) int { return strings.Compare(a.Name, b.Name) })
	return out, err
}

// Info is a provider as anyone may see it: no token.
type Info struct {
	ID, Name, Provider string
	Zones              []string
}

// Shown returns every provider without its token, sorted by name. The token is
// opened with the rest of the spec, as a spec is sealed whole (ADR 0022), and
// dropped as soon as the name and zones are read; nothing here returns it.
func (p *Providers) Shown(ctx context.Context) ([]Info, error) {
	all, err := p.All(ctx)
	out := make([]Info, 0, len(all))
	for i := range all {
		s := &all[i]
		out = append(out, Info{ID: s.ID, Name: s.Name, Provider: s.Spec.GetProvider(), Zones: s.Spec.GetZones()})
		s.Spec.ApiToken = ""
	}
	return out, err
}

// ActionCheck is a saved provider's token checked again, on Activity: who checked
// which provider, in which zones (ADR 0088).
const ActionCheck = "dns_provider.check"

// ErrNoProvider is a named provider that isn't there.
var ErrNoProvider = errors.New("there's no DNS provider of that name. List them with ListDnsProviders")

// Named returns the saved provider called name.
func (p *Providers) Named(ctx context.Context, name string) (Saved, error) {
	all, err := p.All(ctx)
	if err != nil {
		return Saved{}, err
	}
	for _, s := range all {
		if s.Name == name {
			return s, nil
		}
	}
	return Saved{}, ErrNoProvider
}

// APIFor reaches the API of a provider's kind with its token.
func (p *Providers) APIFor(provider, token string) (API, error) {
	if provider != dnsprovider.Cloudflare {
		return nil, fmt.Errorf("a DNS provider writes through Cloudflare's API for now, and %q isn't it", provider)
	}
	return p.Cloudflare(token), nil
}

// Off is an API that's never reached, saying why, as in a test with no stand-in.
type Off string

// Verify says why the provider isn't reached.
func (o Off) Verify(context.Context) error { return errors.New(string(o)) }

// Zones says why the provider isn't reached.
func (o Off) Zones(context.Context) ([]Zone, error) { return nil, errors.New(string(o)) }

// AppendTXT says why the provider isn't reached.
func (o Off) AppendTXT(context.Context, Zone, string, string) (string, error) {
	return "", errors.New(string(o))
}

// TXTAt says why the provider isn't reached.
func (o Off) TXTAt(context.Context, Zone, string) ([]TXT, error) { return nil, errors.New(string(o)) }

// Delete says why the provider isn't reached.
func (o Off) Delete(context.Context, Zone, string) error { return errors.New(string(o)) }

// State is what a check of a token came to, taken together.
type State int

// What a check can come to.
const (
	// Writes is a token that wrote and deleted a record in every zone asked for.
	Writes State = iota + 1
	// Refused is a token the provider wouldn't take.
	Refused
	// CantWrite is a good token that couldn't write in a zone asked for, or
	// reaches none.
	CantWrite
	// Unreachable is a provider that couldn't be reached.
	Unreachable
	// Reaches is a good token, with no zone asked for, so nothing written.
	Reaches
)

// ZoneCheck is what writing a record in one zone came to.
type ZoneCheck struct {
	Zone   string
	Writes bool
	Detail string
}

// Result is what a check found.
type Result struct {
	State   State
	Reaches []string
	Zones   []ZoneCheck
	Detail  string
}

// checkTimeout bounds a whole check, as a form waits on it.
const checkTimeout = 30 * time.Second

// Check verifies a token, lists the zones it reaches, and in each of zones writes a
// TXT record, reads it back and deletes it, whatever else happens. It never says
// the token.
func Check(ctx context.Context, api API, zones []string) Result {
	ctx, cancel := context.WithTimeout(ctx, checkTimeout)
	defer cancel()
	if err := api.Verify(ctx); err != nil {
		return failed(err)
	}
	reached, err := api.Zones(ctx)
	if err != nil {
		return failed(err)
	}
	out := Result{}
	byName := map[string]Zone{}
	for _, z := range reached {
		out.Reaches = append(out.Reaches, z.Name)
		byName[z.Name] = z
	}
	slices.Sort(out.Reaches)
	if len(zones) == 0 {
		if len(reached) == 0 {
			out.State, out.Detail = CantWrite, "The token reaches no zone. Give it Zone:Read and DNS:Edit for your zones"
			return out
		}
		out.State = Reaches
		return out
	}
	out.State = Writes
	out.Zones = make([]ZoneCheck, len(zones))
	// A few zones at a time, so up to 50 fit in the check's time.
	var wg sync.WaitGroup
	slots := make(chan struct{}, zonesAtOnce)
	for i, name := range zones {
		out.Zones[i].Zone = name
		z, ok := byName[name]
		if !ok {
			out.Zones[i].Detail = "The token doesn't reach " + name + ". Give it Zone:Read and DNS:Edit for it"
			continue
		}
		wg.Go(func() {
			slots <- struct{}{}
			defer func() { <-slots }()
			out.Zones[i].Writes, out.Zones[i].Detail = writeAndDelete(ctx, api, z)
		})
	}
	wg.Wait()
	for _, zc := range out.Zones {
		if !zc.Writes {
			out.State = CantWrite
			if out.Detail == "" {
				out.Detail = zc.Detail
			}
		}
	}
	return out
}

// zonesAtOnce is how many zones a check writes in at once.
const zonesAtOnce = 4

// writeAndDelete writes a TXT record of its own in zone, reads it back, and
// deletes it, saying what went wrong in the provider's words. A write that failed
// other than by the provider refusing it may have been written all the same, so
// what's at its name goes too.
func writeAndDelete(ctx context.Context, api API, zone Zone) (bool, string) {
	tag := make([]byte, 6)
	_, _ = rand.Read(tag)
	name := "_dockwell-check-" + hex.EncodeToString(tag) + "." + zone.Name
	value := "dockwell check " + hex.EncodeToString(tag)
	// What's written goes whatever happens, with a context of its own, as the
	// check's may be done.
	cleanup, cancel := context.WithTimeout(context.WithoutCancel(ctx), 15*time.Second)
	defer cancel()
	id, err := api.AppendTXT(ctx, zone, name, value)
	if err != nil {
		if !refusedWrite(err) {
			if left := deleteAt(cleanup, api, zone, name); left != nil {
				return false, words(err) + ". It may have written " + name + ", and couldn't delete it: delete it at the provider"
			}
		}
		return false, words(err)
	}
	found, readErr := api.TXTAt(ctx, zone, name)
	if err := api.Delete(cleanup, zone, id); err != nil {
		return false, "It wrote a record and couldn't delete it, " + name + ": " + words(err) + ". Delete it at the provider, and give the token DNS:Edit"
	}
	switch {
	case readErr != nil:
		return false, words(readErr)
	case !slices.ContainsFunc(found, func(t TXT) bool { return t.Value == value }):
		return false, "It wrote a record that then wasn't there, " + name
	}
	return true, ""
}

// refusedWrite says the provider answered a write with a refusal of its own, 4xx,
// so nothing was written.
func refusedWrite(err error) bool {
	var api *APIError
	return errors.As(err, &api) && api.Status >= 400 && api.Status < 500
}

// deleteAt deletes every TXT record at name, as one written by a write whose answer
// was lost, saying what it couldn't.
func deleteAt(ctx context.Context, api API, zone Zone, name string) error {
	found, err := api.TXTAt(ctx, zone, name)
	if err != nil {
		return err
	}
	var errs []error
	for _, t := range found {
		errs = append(errs, api.Delete(ctx, zone, t.ID))
	}
	return errors.Join(errs...)
}

// failed is a check that stopped before any zone, with why.
func failed(err error) Result {
	var api *APIError
	switch {
	case errors.Is(err, ErrGlobalKey):
		return Result{State: Refused, Detail: sentence(err.Error())}
	case errors.As(err, &api) && api.Refused():
		return Result{State: Refused, Detail: api.Error()}
	case errors.As(err, &api):
		return Result{State: CantWrite, Detail: api.Error()}
	}
	return Result{State: Unreachable, Detail: sentence(err.Error())}
}

// words is an error as a check shows it: a provider's in its own words.
func words(err error) string {
	var api *APIError
	if errors.As(err, &api) {
		return api.Error()
	}
	return sentence(err.Error())
}

func sentence(s string) string {
	if s == "" {
		return s
	}
	return strings.ToUpper(s[:1]) + s[1:]
}
