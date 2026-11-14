// SPDX-License-Identifier: AGPL-3.0-only

// Package certificates gets certificates by DNS-01 for the control plane, renews
// them, keeps them sealed in the store, and gives each server those its routes
// need (ADR 0088). By default a server has a certificate of its own, with a key of
// its own, for exactly the names its routes prove by DNS; a shared certificate,
// such as a wildcard, is an admin's choice, for the projects it names. A DNS
// provider's token stays here.
package certificates

import (
	"cmp"
	"context"
	"crypto/x509"
	"encoding/pem"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"slices"
	"strings"
	"sync"
	"time"

	"golang.org/x/net/publicsuffix"

	resourcev1 "github.com/dockwell/dockwell/gen/dockwell/resource/v1alpha1"
	"github.com/dockwell/dockwell/internal/control/dnsproviders"
	"github.com/dockwell/dockwell/internal/control/registrylogins"
	"github.com/dockwell/dockwell/internal/ingress"
	"github.com/dockwell/dockwell/internal/resource/certificate"
	"github.com/dockwell/dockwell/internal/resource/dnsprovider"
	"github.com/dockwell/dockwell/internal/resource/route"
	"github.com/dockwell/dockwell/internal/revision"
	"github.com/dockwell/dockwell/internal/secrets"
	"github.com/dockwell/dockwell/internal/store"
)

// Waiter waits until a record is answered where a CA will look for it: the zone's
// own nameservers.
type Waiter interface {
	Wait(ctx context.Context, name, value string) error
}

// Manager gets, renews and keeps certificates by DNS-01.
type Manager struct {
	DB     *store.DB
	Keeper *secrets.Keeper
	DNS    *dnsproviders.Providers
	// Directory is the ACME directory: Let's Encrypt's unless set.
	Directory string
	// HTTP reaches the CA, with the roots it's trusted by.
	HTTP *http.Client
	// Propagation waits for each challenge record; nil waits for none, as a test's
	// CA reads its own DNS.
	Propagation Waiter
	// PanelDomains are the panel's domains, as it's served and as it's saved, which
	// a certificate that reaches them takes to no server but the control server,
	// whose ID ControlServer says.
	PanelDomains  func() []string
	ControlServer func() string
	Logger        *slog.Logger
	Now           func() time.Time
	// ProviderAPI reaches a saved provider's API by its name: through DNS unless a
	// test's.
	ProviderAPI func(ctx context.Context, name string) (dnsproviders.API, error)
	// Agents are the servers to give certificates to; nil gives none.
	Agents Agents

	// pushed is what each server was last given, as its holders and their
	// generations.
	pushedMu sync.Mutex
	pushed   map[string]string

	once sync.Once
	// working holds a place while a tick runs, so ticks never overlap.
	working sync.Mutex
}

func (m *Manager) now() time.Time {
	if m.Now != nil {
		return m.Now()
	}
	return time.Now()
}

func (m *Manager) logger() *slog.Logger {
	if m.Logger != nil {
		return m.Logger
	}
	return slog.Default()
}

func (m *Manager) controlServer() string {
	if m.ControlServer != nil {
		return m.ControlServer()
	}
	return ""
}

// Every is how often the manager looks at what's due.
const Every = 30 * time.Second

// Run looks at what's due every Every, until ctx is done, cleaning up what a crash
// left mid-order first.
func (m *Manager) Run(ctx context.Context) {
	t := time.NewTicker(Every)
	defer t.Stop()
	for {
		if err := m.Tick(ctx); err != nil && ctx.Err() == nil {
			m.logger().Warn("can't look at certificates by DNS; it tries again", "error", err.Error())
		}
		select {
		case <-ctx.Done():
			return
		case <-t.C:
		}
	}
}

// Holder names what has a certificate of its own: a server, srv_…, for the names
// its routes prove by DNS, or a shared certificate, crt_….
type Holder = string

// want is what one holder's certificate should cover.
type want struct {
	Holder Holder
	Name   string
	// Names, and the provider that proves each.
	Names    []string
	Provider map[string]string
	// Servers are where its key goes, and Projects whose routes it serves.
	Servers  []string
	Projects map[string]bool
}

// wants reads what each holder's certificate should cover, from the routes that
// prove their domains by DNS, the providers and the shared certificates, and the
// zones each provider may write in.
func (m *Manager) wants(ctx context.Context) (map[Holder]*want, map[string][]string, error) {
	out := map[Holder]*want{}
	zones := map[string][]string{}
	err := m.DB.View(ctx, func(tx *store.Tx) error {
		shared, err := m.shared(ctx, tx)
		if err != nil {
			return err
		}
		providers, err := m.providerSpecs(ctx, tx)
		if err != nil {
			return err
		}
		for name, p := range providers {
			zones[name] = p.GetZones()
		}
		routes, err := tx.ListKind(ctx, string(route.Kind))
		if err != nil {
			return err
		}
		panels := m.panelDomains()
		for i := range routes {
			r := &routes[i]
			if r.Spec == nil {
				continue
			}
			spec, err := m.Keeper.Decode(ctx, tx, store.InRevision(r.ID, r.Kind, r.Revision), r.Spec)
			if err != nil {
				return err
			}
			rt, ok := spec.(*resourcev1.Route)
			if !ok || rt.GetTls() == ingress.TLSOff || rt.GetProof() != route.ProofDNS {
				continue
			}
			project, err := registrylogins.ProjectOf(ctx, tx, r.Scope)
			if err != nil {
				return err
			}
			placed, err := tx.Placement(ctx, r.ID, r.Revision)
			if err != nil {
				return err
			}
			for _, server := range placed {
				m.place(out, shared, providers, rt.GetDomain(), project, server, panels)
			}
		}
		return nil
	})
	for _, w := range out {
		slices.Sort(w.Names)
		w.Names = slices.Compact(w.Names)
		slices.Sort(w.Servers)
		w.Servers = slices.Compact(w.Servers)
	}
	return out, zones, err
}

// panelDomains are the panel's domains, served and saved.
func (m *Manager) panelDomains() []string {
	if m.PanelDomains != nil {
		return m.PanelDomains()
	}
	return nil
}

func (m *Manager) shared(ctx context.Context, tx *store.Tx) ([]certificate.Shared, error) {
	stored, err := tx.ListKind(ctx, string(certificate.Kind))
	if err != nil {
		return nil, err
	}
	var out []certificate.Shared
	for i := range stored {
		r := &stored[i]
		if r.Spec == nil {
			continue
		}
		spec, err := m.Keeper.Decode(ctx, tx, store.InRevision(r.ID, r.Kind, r.Revision), r.Spec)
		if err != nil {
			return nil, err
		}
		if c, ok := spec.(*resourcev1.DnsCertificate); ok {
			out = append(out, certificate.Shared{ID: r.ID, Name: r.Name, Spec: c})
		}
	}
	slices.SortFunc(out, func(a, b certificate.Shared) int { return strings.Compare(a.Name, b.Name) })
	return out, nil
}

// providerSpecs are the providers' specs, by name, for their zones and projects:
// their tokens go no further than an order.
func (m *Manager) providerSpecs(ctx context.Context, tx *store.Tx) (map[string]*resourcev1.DnsProvider, error) {
	stored, err := tx.ListKind(ctx, string(dnsprovider.Kind))
	if err != nil {
		return nil, err
	}
	out := map[string]*resourcev1.DnsProvider{}
	for i := range stored {
		r := &stored[i]
		if r.Spec == nil {
			continue
		}
		spec, err := m.Keeper.Decode(ctx, tx, store.InRevision(r.ID, r.Kind, r.Revision), r.Spec)
		if err != nil {
			return nil, err
		}
		if p, ok := spec.(*resourcev1.DnsProvider); ok {
			p.ApiToken = ""
			out[r.Name] = p
		}
	}
	return out, nil
}

// place puts a route's domain on server under the certificate that serves it there:
// a shared one that covers it and serves its project, as certificate.Serving picks,
// whose names its provider's zones hold, and otherwise the server's own, through a
// provider that holds its zone and proves its project's names.
func (m *Manager) place(out map[Holder]*want, shared []certificate.Shared, specs map[string]*resourcev1.DnsProvider, domain, project, server string, panels []string) {
	if c, ok := certificate.Serving(shared, domain, project, server, m.controlServer(), panels); ok {
		p := specs[c.Spec.GetDnsProvider()]
		if p == nil || certificate.Outside(c.Spec.GetDomains(), p.GetZones()) != "" {
			return
		}
		w := out[c.ID]
		if w == nil {
			w = &want{Holder: c.ID, Name: c.Name, Names: c.Spec.GetDomains(), Provider: map[string]string{}, Projects: map[string]bool{}}
			for _, n := range w.Names {
				w.Provider[n] = c.Spec.GetDnsProvider()
			}
			out[c.ID] = w
		}
		w.Servers = append(w.Servers, server)
		w.Projects[project] = true
		return
	}
	provider := ""
	names := make([]string, 0, len(specs))
	for name := range specs {
		names = append(names, name)
	}
	slices.Sort(names)
	for _, name := range names {
		spec := specs[name]
		if dnsprovider.Serves(spec, project) && certificate.ZoneOf(spec.GetZones(), domain) != "" {
			provider = name
			break
		}
	}
	if provider == "" {
		return
	}
	w := out[server]
	if w == nil {
		w = &want{Holder: server, Name: server, Provider: map[string]string{}, Projects: map[string]bool{}}
		out[server] = w
	}
	w.Projects[project] = true
	if len(w.Names) >= certificate.MaxNames && !slices.Contains(w.Names, domain) {
		return
	}
	w.Names = append(w.Names, domain)
	w.Provider[domain] = provider
	w.Servers = []string{server}
}

// Tick orders what's due, renews what's in its window, and forgets what's no
// longer wanted, then gives each server what it needs.
func (m *Manager) Tick(ctx context.Context) error {
	m.working.Lock()
	defer m.working.Unlock()
	m.once.Do(func() { m.cleanupNoted(ctx) })
	wanted, zones, err := m.wants(ctx)
	if err != nil {
		return err
	}
	var st map[string]*held
	var ordered map[string][]time.Time
	if err := m.DB.View(ctx, func(tx *store.Tx) error {
		if st, err = states(ctx, tx); err != nil {
			return err
		}
		ordered, err = orders(ctx, tx)
		return err
	}); err != nil {
		return err
	}
	now := m.now()
	holders := make([]Holder, 0, len(wanted))
	for h := range wanted {
		holders = append(holders, h)
	}
	slices.Sort(holders)
	for _, h := range holders {
		w, s := wanted[h], st[h]
		if s == nil {
			s = &held{}
			st[h] = s
		}
		m.checkWindow(ctx, h, s, now)
		if !dueNow(s, w.Names, now) {
			continue
		}
		if busy := atCap(ordered, w.Names, now); busy != "" {
			s.Message = fmt.Sprintf("Waiting: Dockwell has ordered %d certificates for %s this week, the most it orders, so Let's Encrypt's limit is left for the rest. It orders again as the oldest is a week old", weeklyCap, busy)
			continue
		}
		m.issue(ctx, w, s, zones, ordered)
	}
	// What's no longer wanted goes, with its key.
	var gone []Holder
	for h := range st {
		if wanted[h] == nil {
			gone = append(gone, h)
		}
	}
	if err := m.DB.Update(ctx, func(tx *store.Tx) error {
		for _, h := range gone {
			delete(st, h)
			if err := tx.DeleteSetting(ctx, issuedSetting(h)); err != nil {
				return err
			}
		}
		if err := putJSON(ctx, tx, ordersSetting, prune(ordered, now)); err != nil {
			return err
		}
		if err := m.forgetRemoved(ctx, tx); err != nil {
			return err
		}
		return putJSON(ctx, tx, stateSetting, st)
	}); err != nil {
		return err
	}
	m.deliver(ctx, wanted, st)
	return nil
}

// dueNow says whether a holder's certificate, as s stands, is ordered now for names:
// never yet issued, its names changed, one dropped, or time to renew; once the last
// failure's wait is over, and, for names that changed, an hour after the last order,
// so routes coming and going don't use up the CA's limits.
func dueNow(s *held, names []string, now time.Time) bool {
	due := s.Gen == 0 || !slices.Equal(s.Wanted, names) || len(s.Dropped) > 0 || !now.Before(s.RenewAt)
	switch {
	case !due || now.Before(s.Next):
		return false
	case s.Gen > 0 && now.Before(s.LastOrder.Add(orderEvery)) && now.Before(s.RenewAt):
		return false
	}
	return true
}

// orderEvery is how often one holder's certificate is ordered again for names that
// changed, at most, and droppedEvery how often for a name whose proof failed.
const (
	orderEvery   = time.Hour
	droppedEvery = 24 * time.Hour
)

// weeklyCap is how many certificates Dockwell orders for one registered domain in a
// week: under Let's Encrypt's 50, which Caddy's and the panel's share.
const weeklyCap = 20

// registered is a name's registered domain, as Let's Encrypt counts its limits.
func registered(name string) string {
	host := strings.TrimPrefix(name, "*.")
	if r, err := publicsuffix.EffectiveTLDPlusOne(host); err == nil {
		return r
	}
	return host
}

// atCap is the registered domain of names that has had weeklyCap orders this week,
// or "".
func atCap(ordered map[string][]time.Time, names []string, now time.Time) string {
	for _, n := range names {
		r := registered(n)
		count := 0
		for _, t := range ordered[r] {
			if now.Sub(t) < 7*24*time.Hour {
				count++
			}
		}
		if count >= weeklyCap {
			return r
		}
	}
	return ""
}

// prune keeps the orders of the last week.
func prune(ordered map[string][]time.Time, now time.Time) map[string][]time.Time {
	out := map[string][]time.Time{}
	for r, times := range ordered {
		for _, t := range times {
			if now.Sub(t) < 7*24*time.Hour {
				out[r] = append(out[r], t)
			}
		}
	}
	return out
}

// countOrder counts an order for names' registered domains.
func countOrder(ordered map[string][]time.Time, names []string, now time.Time) {
	seen := map[string]bool{}
	for _, n := range names {
		if r := registered(n); !seen[r] {
			seen[r] = true
			ordered[r] = append(ordered[r], now)
		}
	}
}

// forgetRemoved drops what an admin let servers that are gone have.
func (m *Manager) forgetRemoved(ctx context.Context, tx *store.Tx) error {
	confirmed, err := revision.KeysConfirmed(ctx, tx)
	if err != nil || len(confirmed) == 0 {
		return err
	}
	servers, err := tx.ListServers(ctx)
	if err != nil {
		return err
	}
	live := map[string]bool{}
	for i := range servers {
		if servers[i].RevokedAt == nil {
			live[servers[i].ID] = true
		}
	}
	var gone []string
	for s := range confirmed {
		if !live[s] {
			gone = append(gone, s)
		}
	}
	if len(gone) == 0 {
		return nil
	}
	return revision.ForgetKeys(ctx, tx, gone...)
}

// checkWindow asks the CA when to renew what's issued, once a day, as ARI's window
// can move (RFC 9773).
func (m *Manager) checkWindow(ctx context.Context, h Holder, s *held, now time.Time) {
	if s.Gen == 0 || now.Before(s.WindowNext) {
		return
	}
	s.WindowNext = now.Add(24 * time.Hour)
	_, _, chainPEM, err := m.issuedParts(ctx, h)
	if err != nil {
		return
	}
	leaf := leafOf(chainPEM)
	if leaf == nil {
		return
	}
	start, end, retry, ok := m.renewalWindow(ctx, leaf)
	if retry > 0 {
		s.WindowNext = now.Add(max(retry, time.Hour))
	}
	if ok {
		s.RenewAt = windowAt(start, end)
	}
}

// issue orders w's certificate, records it on success, and backs off on failure. A
// name whose authorization fails is dropped, with why, and the rest ordered.
func (m *Manager) issue(ctx context.Context, w *want, s *held, zones map[string][]string, ordered map[string][]time.Time) {
	now := m.now()
	names := slices.Clone(w.Names)
	// Wanted stays the names of what's issued until an order for these succeeds, so
	// a failed one is tried again.
	s.LastOrder = now
	var dropped []string
	got, err := m.orderFor(ctx, w, names, zones)
	countOrder(ordered, names, now)
	var failed *authzFailed
	if errors.As(err, &failed) && len(failed.Names) < len(names) {
		dropped = failed.Names
		names = slices.DeleteFunc(names, func(n string) bool { return slices.Contains(dropped, n) })
		if atCap(ordered, names, now) == "" {
			got, err = m.orderFor(ctx, w, names, zones)
			countOrder(ordered, names, now)
		}
	}
	if err != nil {
		s.Failures++
		s.Message = sentence(err.Error())
		s.Next = now.Add(backoff(s.Failures))
		m.logger().Warn("can't get a certificate by DNS; it tries again", "holder", w.Holder, "names", strings.Join(w.Names, ","), "again_in", s.Next.Sub(now).String(), "error", err.Error())
		return
	}
	defer clear(got.Key)
	start, end, hasWindow := time.Time{}, time.Time{}, false
	if leaf := leafOf([]byte(got.Chain)); leaf != nil {
		start, end, _, hasWindow = m.renewalWindow(ctx, leaf)
	}
	gen := s.Gen + 1
	err = m.DB.Update(ctx, func(tx *store.Tx) error {
		b, err := m.sealTo(ctx, tx, w.Holder, gen, got)
		if err != nil {
			return err
		}
		return tx.PutSetting(ctx, issuedSetting(w.Holder), b)
	})
	if err != nil {
		s.Failures++
		s.Message = sentence(err.Error())
		s.Next = now.Add(backoff(s.Failures))
		return
	}
	at := renewAt(got, start, end, hasWindow)
	*s = held{
		Gen: gen, Names: got.Names, Wanted: slices.Clone(w.Names), Serial: got.Serial, NotAfter: got.NotAfter, IssuedAt: got.IssuedAt,
		RenewAt: at, WindowNext: now.Add(24 * time.Hour), LastOrder: now,
	}
	if len(dropped) > 0 {
		// What failed is tried again once a day, with the rest, as a new order.
		s.Dropped = dropped
		s.Message = fmt.Sprintf("Issued without %s: %s", strings.Join(dropped, ", "), sentence(failed.Err.Error()))
		s.Next = now.Add(droppedEvery)
	}
	m.logger().Info("got a certificate by DNS", "holder", w.Holder, "names", strings.Join(got.Names, ","), "until", got.NotAfter.Format(time.DateOnly))
}

// apiOf reaches the API of the saved provider called name.
func (m *Manager) apiOf(ctx context.Context, name string) (dnsproviders.API, error) {
	if m.ProviderAPI != nil {
		return m.ProviderAPI(ctx, name)
	}
	p, err := m.DNS.Named(ctx, name)
	if err != nil {
		return nil, err
	}
	return m.DNS.APIFor(p.Spec.GetProvider(), p.Spec.GetApiToken())
}

// orderFor reaches each provider names need, in the zones it was saved with alone,
// and orders.
func (m *Manager) orderFor(ctx context.Context, w *want, names []string, saved map[string][]string) (*issued, error) {
	s := &solver{m: m, providers: map[string]dnsproviders.API{}, zones: map[string][]dnsproviders.Zone{}}
	for _, name := range names {
		p := w.Provider[name]
		if s.providers[p] != nil {
			continue
		}
		api, err := m.apiOf(ctx, p)
		if err != nil {
			return nil, err
		}
		reached, err := api.Zones(ctx)
		if err != nil {
			return nil, err
		}
		allowed := saved[p]
		var zones []dnsproviders.Zone
		for _, z := range reached {
			if allowed == nil || slices.Contains(allowed, z.Name) {
				zones = append(zones, z)
			}
		}
		s.providers[p], s.zones[p] = api, zones
	}
	c, err := m.client(ctx)
	if err != nil {
		return nil, err
	}
	ctx, cancel := context.WithTimeout(ctx, orderTimeout)
	defer cancel()
	return m.order(ctx, c, names, func(name string) string { return w.Provider[name] }, s)
}

// orderTimeout bounds one order: its records' propagation and the CA's checks.
const orderTimeout = 5 * time.Minute

// backoff is how long after its nth failure a holder waits: from 2 minutes, doubling
// to 6 hours, so no more than 5 tries an hour, under Let's Encrypt's limit on failed
// validations.
func backoff(failures int) time.Duration {
	d := 2 * time.Minute
	for i := 1; i < failures && d < 6*time.Hour; i++ {
		d *= 2
	}
	return min(d, 6*time.Hour)
}

func leafOf(chain []byte) *x509.Certificate {
	b, _ := pem.Decode(chain)
	if b == nil {
		return nil
	}
	leaf, err := x509.ParseCertificate(b.Bytes)
	if err != nil {
		return nil
	}
	return leaf
}

func sentence(s string) string {
	if s == "" {
		return s
	}
	return strings.ToUpper(s[:1]) + s[1:]
}

// note writes down a challenge record before it's written, and its ID once it is.
func (m *Manager) note(ctx context.Context, n noted, id string) error {
	return m.DB.Update(context.WithoutCancel(ctx), func(tx *store.Tx) error {
		list, err := journal(ctx, tx)
		if err != nil {
			return err
		}
		list = slices.DeleteFunc(list, func(o noted) bool { return o.Provider == n.Provider && o.Name == n.Name && o.Value == n.Value })
		n.ID = cmp.Or(id, n.ID)
		return putJSON(ctx, tx, journalSetting, append(list, n))
	})
}

// forget crosses a record off the journal once it's deleted.
func (m *Manager) forget(ctx context.Context, n noted) error {
	return m.DB.Update(ctx, func(tx *store.Tx) error {
		list, err := journal(ctx, tx)
		if err != nil {
			return err
		}
		list = slices.DeleteFunc(list, func(o noted) bool { return o.Provider == n.Provider && o.Name == n.Name && o.Value == n.Value })
		return putJSON(ctx, tx, journalSetting, list)
	})
}

// cleanupNoted deletes what a crash mid-order left in a zone: each noted record,
// by its ID or by its name and value where it was never answered.
func (m *Manager) cleanupNoted(ctx context.Context) {
	var list []noted
	if err := m.DB.View(ctx, func(tx *store.Tx) error {
		var err error
		list, err = journal(ctx, tx)
		return err
	}); err != nil || len(list) == 0 {
		return
	}
	for _, n := range list {
		api, err := m.apiOf(ctx, n.Provider)
		if errors.Is(err, dnsproviders.ErrNoProvider) {
			_ = m.forget(ctx, n)
			continue
		}
		if err != nil {
			continue
		}
		zone := dnsproviders.Zone{ID: n.ZoneID, Name: n.Zone}
		ids := []string{n.ID}
		if n.ID == "" {
			ids = nil
			found, err := api.TXTAt(ctx, zone, n.Name)
			if err != nil {
				continue
			}
			for _, t := range found {
				if t.Value == n.Value {
					ids = append(ids, t.ID)
				}
			}
		}
		var errs []error
		for _, id := range ids {
			errs = append(errs, api.Delete(ctx, zone, id))
		}
		if err := errors.Join(errs...); err != nil {
			m.logger().Warn("can't delete a challenge record a crash left; delete it at the provider", "name", n.Name, "error", err.Error())
			continue
		}
		_ = m.forget(ctx, n)
		m.logger().Info("deleted a challenge record a crash left", "name", n.Name)
	}
}

// errNotIssued is a holder with no certificate yet.
var errNotIssued = errors.New("no certificate yet")

// issuedParts opens holder's certificate: its key, names and chain.
func (m *Manager) issuedParts(ctx context.Context, holder Holder) (key []byte, names []string, chain []byte, err error) {
	err = m.DB.View(ctx, func(tx *store.Tx) error {
		st, err := states(ctx, tx)
		if err != nil {
			return err
		}
		s := st[holder]
		if s == nil || s.Gen == 0 {
			return errNotIssued
		}
		c, err := m.loadIssued(ctx, tx, holder, s.Gen)
		if err != nil {
			return err
		}
		if c == nil {
			return errNotIssued
		}
		key, names, chain = c.Key, c.Names, []byte(c.Chain)
		return nil
	})
	if err != nil {
		return nil, nil, nil, fmt.Errorf("the certificate of %s: %w", holder, err)
	}
	return key, names, chain, nil
}

// Standing is where one certificate by DNS stands, for the API: nothing secret.
type Standing struct {
	// Holder is a shared certificate's ID, crt_…, or a server's, srv_…, for its own.
	Holder   Holder
	Name     string
	Names    []string
	Provider string
	Issued   bool
	Failed   bool
	NotAfter time.Time
	Message  string
	Next     time.Time
	// Projects are those whose routes it serves.
	Projects []string
}

// List says where each certificate by DNS stands: those wanted now, issued or not.
func (m *Manager) List(ctx context.Context) ([]Standing, error) {
	wanted, _, err := m.wants(ctx)
	if err != nil {
		return nil, err
	}
	var st map[string]*held
	if err := m.DB.View(ctx, func(tx *store.Tx) error {
		st, err = states(ctx, tx)
		return err
	}); err != nil {
		return nil, err
	}
	now := m.now()
	out := make([]Standing, 0, len(wanted))
	for h, w := range wanted {
		s := st[h]
		if s == nil {
			s = &held{}
		}
		providers := map[string]bool{}
		for _, n := range w.Names {
			providers[w.Provider[n]] = true
		}
		var names []string
		for p := range providers {
			names = append(names, p)
		}
		slices.Sort(names)
		var projects []string
		for p := range w.Projects {
			projects = append(projects, p)
		}
		slices.Sort(projects)
		one := Standing{Holder: h, Name: w.Name, Names: w.Names, Provider: strings.Join(names, ", "), Message: s.Message, Projects: projects}
		switch {
		case s.Gen > 0 && now.Before(s.NotAfter):
			one.Issued, one.NotAfter, one.Next = true, s.NotAfter, s.RenewAt
			if s.Message != "" {
				one.Next = s.Next
			}
		case s.Message != "":
			one.Failed, one.Next = true, s.Next
		default:
			one.Next = s.Next
		}
		out = append(out, one)
	}
	slices.SortFunc(out, func(a, b Standing) int { return cmp.Compare(a.Name, b.Name) })
	return out, nil
}
