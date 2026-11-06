// SPDX-License-Identifier: AGPL-3.0-only

package ingress

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"maps"
	"os"
	"path/filepath"
	"slices"
	"time"

	"github.com/dockwell/dockwell/internal/human"
	"github.com/dockwell/dockwell/internal/resource"
)

// Limits on what a server's routes can ask of its proxy and of the certificate
// authority, past what any server needs, so a write token can't use them up.
const (
	// MaxRoutes is the most routes a server may have. Caddy's memory grows with
	// them, 153 MB at 1,000 as measured, and each change rewrites them all.
	MaxRoutes = 1000
	// MaxPendingDomains is how many domains on a server may wait for their first
	// certificate at once. Let's Encrypt allows 300 new orders per account per 3
	// hours, and that one account is what the panel's domain needs too. Names
	// that don't resolve never leave this state, which is what makes a burst of
	// them dangerous.
	MaxPendingDomains = 50
	// MaxAskedDomains is how many domains a server may have asked the certificate
	// authority about within PendingWindow, with no certificate come yet, whether
	// their routes are still there or not. Removing a route doesn't take back what
	// was asked, so without this, adding names and removing them in turns gets past
	// MaxPendingDomains, and past Let's Encrypt's 300 new orders per account in 3
	// hours. Half of that leaves the rest for the panel's domain and for renewals.
	// It counts names with no certificate yet, not orders, which is its limit: a
	// name added and removed again counts once, and one stops counting as its
	// certificate comes, whatever orders each took (docs/security/ingress.md).
	MaxAskedDomains = 150
	// PendingWindow is how long a domain counts as pending, from when it was
	// added. It is that limit's window: by then its first order is out of the
	// count, and Caddy retries a failure on the staging directory and backs off,
	// so a domain that stays failed costs little. It must not block others for
	// ever.
	PendingWindow = 3 * time.Hour
	// applyMargin is how far past these limits a step may go as it applies. A
	// plan's check counts what it removes, and its makes run before its deletes, so
	// a plan that swaps routes at a limit goes past it for a moment. A step knows
	// only itself, so this is what it allows for. What it still stops is plans made
	// a while ago, which saw fewer than there are now, adding up without end.
	applyMargin = 2
)

// domainsFile keeps when each domain with no certificate yet was added.
const domainsFile = "domains.json"

func (m *Manager) now() time.Time {
	if m.Now != nil {
		return m.Now()
	}
	return time.Now()
}

// addedAt reads when each domain was added. It is lost only with the ingress's
// directory, and a domain with no record doesn't count, as it was added by
// something from before, whose order is long gone.
func (m *Manager) addedAt() (map[string]time.Time, error) {
	b, err := os.ReadFile(filepath.Join(m.Dir, domainsFile))
	switch {
	case errors.Is(err, os.ErrNotExist):
		return map[string]time.Time{}, nil
	case err != nil:
		return nil, fmt.Errorf("can't read when this server's domains were added: %w", err)
	}
	out := map[string]time.Time{}
	if err := json.Unmarshal(b, &out); err != nil {
		// It only ever delays a domain's being counted, and writes are whole, so
		// this isn't worth stopping a route for.
		return map[string]time.Time{}, nil //nolint:nilerr // see above
	}
	return out, nil
}

func (m *Manager) saveAdded(added map[string]time.Time) error {
	b, err := json.Marshal(added)
	if err != nil {
		return err
	}
	if err := os.MkdirAll(m.Dir, 0o700); err != nil {
		return fmt.Errorf("can't create %s: %w", m.Dir, err)
	}
	if err := writeDurably(filepath.Join(m.Dir, domainsFile), b); err != nil {
		return fmt.Errorf("can't record when a domain was added: %w", err)
	}
	return nil
}

// noteDomain records when a domain was added, the first time.
func (m *Manager) noteDomain(ctx context.Context, domain string) error {
	added, err := m.addedAt()
	if err != nil {
		return err
	}
	if _, known := added[domain]; known {
		return nil
	}
	// One whose certificate is there already asks nothing of the authority.
	if certs, err := m.Proxy.Certificates(ctx, []string{domain}); err == nil && certs[domain].Phase == CertIssued {
		return nil
	}
	added[domain] = m.now()
	return m.saveAdded(added)
}

// pendingDomains are the domains on this server that count as waiting for a
// certificate: added within PendingWindow, with no certificate yet. A domain that
// takes no certificate asks nothing of the authority, so it never counts.
func (m *Manager) pendingDomains(ctx context.Context, have []Route) ([]string, error) {
	waiting, _, err := m.domainsAsked(ctx, have)
	return waiting, err
}

// domainsAsked are the domains this server asked the certificate authority about
// within PendingWindow that have no certificate yet: waiting are those its routes
// still serve with one, and asked all of them, the ones since removed or served
// without a certificate too, as Caddy asked for them all the same. It drops what
// no longer counts, so what is recorded stays no more than this, and the first
// time a domain stops counting.
func (m *Manager) domainsAsked(ctx context.Context, have []Route) (waiting, asked []string, err error) {
	added, err := m.addedAt()
	if err != nil {
		return nil, nil, err
	}
	on := map[string]bool{}
	for k := range have {
		r := &have[k]
		if !r.Plain {
			on[r.Domain] = true
		}
	}
	cutoff := m.now().Add(-PendingWindow)
	for d, at := range added {
		if at.After(cutoff) {
			asked = append(asked, d)
		}
	}
	certs, err := m.Proxy.Certificates(ctx, asked)
	if err != nil {
		return nil, nil, err
	}
	asked = slices.DeleteFunc(asked, func(d string) bool { return certs[d].Phase == CertIssued })
	slices.Sort(asked)
	kept := make(map[string]time.Time, len(asked))
	for _, d := range asked {
		kept[d] = added[d]
		if on[d] {
			waiting = append(waiting, d)
		}
	}
	if !maps.Equal(kept, added) {
		if err := m.saveAdded(kept); err != nil {
			return nil, nil, err
		}
	}
	return waiting, asked, nil
}

// firstToGo is how long until the first of these domains stops counting, to the
// minute, rounded up.
func (m *Manager) firstToGo(names []string) time.Duration {
	added, err := m.addedAt()
	if err != nil || len(names) == 0 {
		return PendingWindow
	}
	first := PendingWindow
	for _, d := range names {
		if at, ok := added[d]; ok {
			first = min(first, at.Add(PendingWindow).Sub(m.now()))
		}
	}
	if first%time.Minute != 0 {
		first = first.Truncate(time.Minute) + time.Minute
	}
	return max(first, time.Minute)
}

// checkLimits refuses what would take this server past its limits. A plan that
// only removes routes or edits them adds nothing, so it passes whatever the count:
// removing a route must always work. margin is how many times over the limits
// this may go, which is 1 for a whole plan, and applyMargin for one step of it.
func (m *Manager) checkLimits(ctx context.Context, desired []resource.Desired, margin int) error {
	have, err := m.Proxy.Routes(ctx)
	if err != nil {
		return err
	}
	ids, domains, after := map[string]bool{}, map[string]bool{}, map[string]bool{}
	touched := map[string]bool{}
	for _, d := range desired {
		if d.Ref.Kind == routeKind {
			touched[RouteID(d.Ref)] = true
		}
	}
	for k := range have {
		r := &have[k]
		ids[r.ID] = true
		// One that takes no certificate has asked the authority for nothing, so a
		// route giving it one asks for something new.
		if !r.Plain {
			domains[r.Domain] = true
		}
		if !touched[r.ID] {
			after[r.Domain] = true
		}
	}
	added := 0
	var fresh []string
	for _, d := range desired {
		spec := routeSpec(d)
		if spec == nil {
			continue
		}
		after[spec.GetDomain()] = true
		if !ids[RouteID(d.Ref)] {
			added++
		}
		name := spec.GetDomain()
		if spec.GetTls() != TLSOff && !domains[name] && !slices.Contains(fresh, name) {
			fresh = append(fresh, name)
		}
	}
	if added > 0 {
		if left, _, _ := routesAfter(have, desired); left > MaxRoutes*margin {
			return fmt.Errorf("this change would give this server %d routes, and a server takes at most %d, as each one costs its proxy memory and every change rewrites them all. Delete routes you don't need, or put these on another server",
				left, MaxRoutes)
		}
	}
	if len(fresh) == 0 {
		return nil
	}
	// One whose certificate is there already asks nothing of the authority, so an
	// undo that puts a route back isn't held up by what others were adding.
	certs, err := m.Proxy.Certificates(ctx, fresh)
	if err != nil {
		return err
	}
	fresh = slices.DeleteFunc(fresh, func(d string) bool { return certs[d].Phase == CertIssued })
	if len(fresh) == 0 {
		return nil
	}
	pending, asked, err := m.domainsAsked(ctx, have)
	if err != nil {
		return err
	}
	// What was asked stays asked, whether the route went or not, so this counts
	// what this plan removes, and a name counted already isn't counted twice. A
	// plan's steps each add their own as they apply, so this holds without a margin.
	if again := len(slices.DeleteFunc(slices.Clone(fresh), func(d string) bool { return slices.Contains(asked, d) })); len(asked)+again > MaxAskedDomains {
		return fmt.Errorf("%d domains were added to this server in the last %d hours with no certificate come yet, some of them since removed or served without one, and this change adds %d more, past the limit of %d. Each asked the certificate authority for a certificate, which removing its route doesn't take back, and Let's Encrypt allows 300 new orders for each account in %d hours, the panel's domain's too. Point a domain's DNS here before you add it, or wait: the first of them stops counting in %s",
			len(asked), int(PendingWindow.Hours()), again, MaxAskedDomains, int(PendingWindow.Hours()), human.Duration(m.firstToGo(asked)))
	}
	// What this plan takes off the server asks nothing more of the authority, so
	// fixing a typo in a domain leaves the count as it was.
	pending = slices.DeleteFunc(pending, func(d string) bool { return !after[d] })
	if len(pending)+len(fresh) <= MaxPendingDomains*margin {
		return nil
	}
	if len(pending) == 0 {
		return fmt.Errorf("this change adds %d new domains at once, and a server takes at most %d waiting for a certificate, as a burst of names that don't resolve can use up the certificate authority's limit for the whole server, the panel's domain too. Split it into changes of up to %d, and make the next once their certificates have come. A domain stops counting %d hours after it was added, whether or not its certificate came",
			len(fresh), MaxPendingDomains, MaxPendingDomains, int(PendingWindow.Hours()))
	}
	return fmt.Errorf("%d domains on this server are waiting for a certificate, and this change adds %d more, past the limit of %d, as a burst of names that don't resolve can use up the certificate authority's limit for the whole server, the panel's domain too. Point their DNS here, or delete the routes you don't need, such as %s, or split this change. A domain stops counting %d hours after it was added, whether or not its certificate came",
		len(pending), len(fresh), MaxPendingDomains, human.List(sampleOf(pending)), int(PendingWindow.Hours()))
}

// sampleOf is a few of a long list, for a message.
func sampleOf(names []string) []string {
	const most = 5
	if len(names) <= most {
		return names
	}
	return append(slices.Clone(names[:most]), fmt.Sprintf("%d more", len(names)-most))
}
