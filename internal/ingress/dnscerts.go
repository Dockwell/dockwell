// SPDX-License-Identifier: AGPL-3.0-only

package ingress

import (
	"context"
	"crypto/tls"
	"crypto/x509"
	"encoding/json"
	"fmt"
	"log/slog"
	"math"
	"slices"
	"strings"
	"sync"
	"time"

	"github.com/dockwell/dockwell/internal/resource"
)

// DNSCert is a certificate the control plane proved by DNS-01 for this server's
// routes, with its key, in PEM (ADR 0088).
type DNSCert struct {
	ID string `json:"id"`
	// Name is what a domain's state says serves it.
	Name string `json:"name"`
	// Names are what it covers, and Serves the routes' domains here it serves,
	// which the proxy gets no certificate of its own for.
	Names  []string `json:"names"`
	Serves []string `json:"serves"`
	Chain  []byte   `json:"chain"`
	Key    []byte   `json:"key"`
	// NotAfter is when it ends, from its chain.
	NotAfter time.Time `json:"not_after"`
}

// CertKeeper keeps the certificates by DNS on the server's disk, sealed, so the
// agent has them while the control plane is away (ADR 0054, ADR 0088).
type CertKeeper interface {
	KeepCertificates(plain []byte) error
	// KeptCertificates is what was kept, or nil for nothing.
	KeptCertificates() ([]byte, error)
}

const (
	// WarnBefore is how long before its end a certificate by DNS the control plane
	// hasn't renewed is warned of: in its domains' state, and once a day in the log.
	WarnBefore = 14 * 24 * time.Hour
	// FallBackBefore is how long before its end, with the control plane away, the
	// names it serves that HTTP-01 can reach go back to the proxy to get for itself.
	FallBackBefore = 7 * 24 * time.Hour
	// warnEvery is how often the log says so.
	warnEvery = 24 * time.Hour
)

// byDNS is what the manager holds of the certificates by DNS.
type byDNS struct {
	mu     sync.Mutex
	loaded bool
	certs  []DNSCert
	// fallback are the names given back to the proxy, as their certificate nears its
	// end with the control plane away.
	fallback map[string]bool
	// warned is when the log last said each certificate nears its end.
	warned map[string]time.Time
}

// readCert checks that a certificate's chain and key make a pair, and reads when it
// ends.
func readCert(c *DNSCert) error {
	pair, err := tls.X509KeyPair(c.Chain, c.Key)
	if err != nil {
		return fmt.Errorf("the certificate %s, for %s, doesn't load: %w", c.Name, strings.Join(c.Names, ", "), err)
	}
	leaf := pair.Leaf
	if leaf == nil {
		if leaf, err = x509.ParseCertificate(pair.Certificate[0]); err != nil {
			return fmt.Errorf("the certificate %s doesn't read: %w", c.Name, err)
		}
	}
	c.NotAfter = leaf.NotAfter
	return nil
}

// PutCertificates keeps the certificates by DNS this server's routes are served
// with, in place of those it had, on disk too, and loads them into the proxy if it
// runs. It says how many it loaded: none while the proxy doesn't run, as they're
// loaded as it starts.
func (m *Manager) PutCertificates(ctx context.Context, certs []DNSCert) (int, error) {
	for i := range certs {
		if err := readCert(&certs[i]); err != nil {
			return 0, err
		}
	}
	if err := m.mu.lock(ctx); err != nil {
		return 0, err
	}
	defer m.mu.unlock()
	m.loadCertificates()
	now := m.now()
	b := &m.bydns
	b.mu.Lock()
	b.certs = certs
	// A name the control plane renewed goes back to its certificate.
	for name := range b.fallback {
		if c, ok := servedBy(certs, name); !ok || c.NotAfter.Sub(now) > FallBackBefore {
			delete(b.fallback, name)
		}
	}
	b.mu.Unlock()
	if err := m.keepCertificates(certs); err != nil {
		return 0, err
	}
	st, err := m.Proxy.Status(ctx)
	if err != nil {
		return 0, err
	}
	if !st.Running || !Active(m.Dir) {
		return 0, nil
	}
	if err := m.applyCertificates(ctx); err != nil {
		return 0, err
	}
	m.watchNow()
	return len(certs), nil
}

// servedBy is the certificate that serves name.
func servedBy(certs []DNSCert, name string) (DNSCert, bool) {
	for i := range certs {
		if slices.Contains(certs[i].Serves, name) {
			return certs[i], true
		}
	}
	return DNSCert{}, false
}

// keepCertificates keeps certs on disk, sealed.
func (m *Manager) keepCertificates(certs []DNSCert) error {
	if m.Certs == nil {
		return nil
	}
	b, err := json.Marshal(certs)
	if err != nil {
		return err
	}
	defer clear(b)
	if err := m.Certs.KeepCertificates(b); err != nil {
		return fmt.Errorf("can't keep this server's certificates by DNS on its disk: %w", err)
	}
	return nil
}

// loadCertificates reads what was kept on disk, once, as the agent starts.
func (m *Manager) loadCertificates() {
	b := &m.bydns
	b.mu.Lock()
	defer b.mu.Unlock()
	if b.loaded {
		return
	}
	b.loaded = true
	if m.Certs == nil {
		return
	}
	plain, err := m.Certs.KeptCertificates()
	if err != nil {
		m.logger().Warn("can't read the certificates by DNS this server kept; it serves what the proxy has until the control plane sends them again", slog.String("error", err.Error()))
		return
	}
	if plain == nil {
		return
	}
	defer clear(plain)
	var certs []DNSCert
	if err := json.Unmarshal(plain, &certs); err != nil {
		m.logger().Warn("the certificates by DNS this server kept don't read; it serves what the proxy has until the control plane sends them again", slog.String("error", err.Error()))
		return
	}
	b.certs = certs
}

// applyCertificates loads the certificates by DNS into the proxy, with the names
// they serve, which the proxy gets none of its own for. A certificate whose every
// name went back to the proxy isn't loaded. The caller holds m.mu.
func (m *Manager) applyCertificates(ctx context.Context) error {
	m.loadCertificates()
	b := &m.bydns
	b.mu.Lock()
	var certs []DNSCert
	var skip []string
	for i := range b.certs {
		c := b.certs[i]
		var serves []string
		for _, n := range c.Serves {
			if !b.fallback[n] {
				serves = append(serves, n)
			}
		}
		if len(serves) == 0 && len(c.Serves) > 0 {
			continue
		}
		certs = append(certs, c)
		skip = append(skip, serves...)
	}
	b.mu.Unlock()
	slices.Sort(skip)
	return m.Proxy.SetCertificates(ctx, certs, slices.Compact(skip))
}

// HearControlPlane gives the manager what says whether the control plane is there,
// for what a certificate by DNS near its end falls back to. Until it's given one,
// the control plane is taken to be there.
func (m *Manager) HearControlPlane(up func() bool) { m.controlUp.Store(&up) }

// controlPlaneUp says whether the control plane is there now.
func (m *Manager) controlPlaneUp() bool {
	up := m.controlUp.Load()
	return up == nil || (*up)()
}

// servedByDNS are the names a certificate by DNS serves now, by name: those not
// given back to the proxy.
func (m *Manager) servedByDNS() map[string]DNSCert {
	m.loadCertificates()
	b := &m.bydns
	b.mu.Lock()
	defer b.mu.Unlock()
	out := map[string]DNSCert{}
	for i := range b.certs {
		for _, n := range b.certs[i].Serves {
			if !b.fallback[n] {
				out[n] = b.certs[i]
			}
		}
	}
	return out
}

// dnsState is where a name a certificate by DNS serves stands: issued until its
// end, and, from WarnBefore, saying the control plane hasn't renewed it.
func dnsState(c DNSCert, now time.Time) CertState {
	st := CertState{Phase: CertIssued, NotAfter: c.NotAfter, ByDNS: c.Name}
	left := c.NotAfter.Sub(now)
	switch {
	case left <= 0:
		st.Phase, st.Problem = CertFailed, ProblemOther
		st.Warning = fmt.Sprintf("Expired on %s, and the control plane hasn't renewed it, so it doesn't answer over HTTPS. Check that the control plane runs and reaches this server.", c.NotAfter.UTC().Format("2 January 2006"))
	case left <= WarnBefore:
		st.Warning = fmt.Sprintf("Expires in %s, and the control plane hasn't renewed it. Check that the control plane runs and reaches this server.", daysLeft(left))
	}
	return st
}

// daysLeft says how long is left, in days.
func daysLeft(d time.Duration) string {
	days := int(math.Ceil(d.Hours() / 24))
	if days == 1 {
		return "1 day"
	}
	return fmt.Sprintf("%d days", days)
}

// watchCertificates warns of each certificate by DNS nearing its end, once a day
// in the log, and, with the control plane away, gives the names HTTP-01 can reach
// back to the proxy within FallBackBefore of the end: those whose DNS points here,
// as checks says. It says whether any went back.
func (m *Manager) watchCertificates(ctx context.Context, checks map[string]*resource.DomainCheck) bool {
	now := m.now()
	up := m.controlPlaneUp()
	b := &m.bydns
	b.mu.Lock()
	defer b.mu.Unlock()
	if b.warned == nil {
		b.warned = map[string]time.Time{}
	}
	back := false
	for i := range b.certs {
		c := &b.certs[i]
		left := c.NotAfter.Sub(now)
		if left > WarnBefore {
			continue
		}
		if now.Sub(b.warned[c.ID]) >= warnEvery {
			b.warned[c.ID] = now
			m.logger().WarnContext(ctx, "a certificate by DNS nears its end, and the control plane hasn't renewed it; check that the control plane runs and reaches this server",
				slog.String("certificate", c.Name), slog.String("names", strings.Join(c.Names, ",")), slog.String("ends", c.NotAfter.UTC().Format(time.RFC3339)))
		}
		if up || left > FallBackBefore {
			continue
		}
		for _, n := range c.Serves {
			if b.fallback[n] || checks[n] == nil || checks[n].State != resource.DomainHere {
				continue
			}
			if b.fallback == nil {
				b.fallback = map[string]bool{}
			}
			b.fallback[n], back = true, true
			m.logger().WarnContext(ctx, "the control plane is away and a certificate by DNS nears its end, so the ingress gets one for a name it serves by HTTP itself, until the control plane renews it",
				slog.String("certificate", c.Name), slog.String("name", n), slog.String("ends", c.NotAfter.UTC().Format(time.RFC3339)))
		}
	}
	return back
}
