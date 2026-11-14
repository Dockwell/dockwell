// SPDX-License-Identifier: AGPL-3.0-only

package certificates

import (
	"context"
	"errors"
	"fmt"
	"net"
	"slices"
	"strings"
	"time"
)

// Nameservers waits until every one of a zone's own nameservers answers a record,
// as a CA asks them, not a resolver that may have cached the name's absence.
type Nameservers struct {
	// Within is how long it waits: 2 minutes unless set.
	Within time.Duration
	// Every is how often it asks: every 5 seconds unless set.
	Every time.Duration
}

// Wait asks each nameserver of the zone that holds name for its TXT records until
// every one has value.
func (n Nameservers) Wait(ctx context.Context, name, value string) error {
	within, every := n.Within, n.Every
	if within == 0 {
		within = 2 * time.Minute
	}
	if every == 0 {
		every = 5 * time.Second
	}
	ctx, cancel := context.WithTimeout(ctx, within)
	defer cancel()
	servers, err := nameserversOf(ctx, name)
	if err != nil {
		return err
	}
	pending := slices.Clone(servers)
	for {
		pending = slices.DeleteFunc(pending, func(ns string) bool { return answers(ctx, ns, name, value) })
		if len(pending) == 0 {
			return nil
		}
		select {
		case <-ctx.Done():
			return fmt.Errorf("the zone's nameservers %s didn't answer %s's challenge record within %s; check the DNS provider's zone", strings.Join(pending, ", "), name, within)
		case <-time.After(every):
		}
	}
}

// nameserversOf are the nameservers of the zone that holds name: the first of name
// and the names above it that has NS records.
func nameserversOf(ctx context.Context, name string) ([]string, error) {
	labels := strings.Split(strings.TrimSuffix(name, "."), ".")
	for i := range len(labels) - 1 {
		candidate := strings.Join(labels[i:], ".")
		ns, err := net.DefaultResolver.LookupNS(ctx, candidate)
		if err == nil && len(ns) > 0 {
			out := make([]string, 0, len(ns))
			for _, n := range ns {
				out = append(out, strings.TrimSuffix(n.Host, "."))
			}
			slices.Sort(out)
			return out, nil
		}
	}
	return nil, errors.New("no nameservers answer for " + name)
}

// answers says whether nameserver ns has value among name's TXT records.
func answers(ctx context.Context, ns, name, value string) bool {
	addrs, err := net.DefaultResolver.LookupHost(ctx, ns)
	if err != nil || len(addrs) == 0 {
		return false
	}
	r := &net.Resolver{PreferGo: true, Dial: func(ctx context.Context, network, _ string) (net.Conn, error) {
		var d net.Dialer
		return d.DialContext(ctx, network, net.JoinHostPort(addrs[0], "53"))
	}}
	ctx, cancel := context.WithTimeout(ctx, 5*time.Second)
	defer cancel()
	txts, err := r.LookupTXT(ctx, name)
	return err == nil && slices.Contains(txts, value)
}
