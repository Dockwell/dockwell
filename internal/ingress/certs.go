// SPDX-License-Identifier: AGPL-3.0-only

package ingress

import (
	"context"
	"fmt"
	"log/slog"
	"maps"
	"regexp"
	"slices"
	"strings"
	"sync"
	"time"

	"google.golang.org/protobuf/proto"
	"google.golang.org/protobuf/types/known/timestamppb"

	dockwellv1 "github.com/dockwell/dockwell/gen/dockwell/v1"
	"github.com/dockwell/dockwell/internal/resource"
	"github.com/dockwell/dockwell/internal/resource/resourcepb"
)

// CertPhase is where a certificate stands.
type CertPhase int

// Certificate phases.
const (
	CertPending CertPhase = iota
	CertIssued
	CertFailed
)

// Problem is why a certificate failed.
type Problem int

// Problems.
const (
	ProblemNone Problem = iota
	ProblemDNS
	ProblemConnection
	ProblemWrongServer
	ProblemRateLimited
	ProblemCAA
	ProblemRefused
	ProblemOther
)

// CertState is where a domain's certificate stands on the proxy.
type CertState struct {
	Phase CertPhase
	// Problem and Detail say why the last try failed, Detail in the
	// certificate authority's words. They can go with an issued certificate,
	// when renewing it fails while it still serves.
	Problem Problem
	Detail  string
	// NotAfter is when an issued certificate expires.
	NotAfter time.Time
}

// acmeProblem finds an ACME problem type (RFC 8555) in what a proxy logs,
// which goes by type, as the wording varies by certificate authority and the
// type doesn't.
var acmeProblem = regexp.MustCompile(`urn:ietf:params:acme:error:([A-Za-z]+)(?: - )?(.*)`)

// ClassifyFailure reads a proxy's word on a failed try. A try cancelled by a
// reload, which starts it again, isn't a failure, and says false.
func ClassifyFailure(text string) (Problem, string, bool) {
	if strings.Contains(text, "context canceled") {
		return ProblemNone, "", false
	}
	const limit = 400
	m := acmeProblem.FindStringSubmatch(text)
	if m == nil {
		return ofWords(text), cut(text, limit), true
	}
	detail := cut(strings.TrimSpace(m[2]), limit)
	switch m[1] {
	case "dns":
		return ProblemDNS, detail, true
	case "connection", "tls":
		return ProblemConnection, detail, true
	case "unauthorized", "incorrectResponse":
		return ProblemWrongServer, detail, true
	case "rateLimited":
		return ProblemRateLimited, detail, true
	case "caa":
		return ProblemCAA, detail, true
	case "rejectedIdentifier":
		return ProblemRefused, detail, true
	}
	return ofWords(detail), detail, true
}

// dnsWords are how a certificate authority says DNS failed under a problem
// type that doesn't, as Pebble says "malformed".
var dnsWords = []string{"could not resolve", "error occurred while resolving", "nxdomain", "no valid a records", "no such host", "dns problem", "servfail"}

// ofWords is the problem a message without a telling type describes.
func ofWords(text string) Problem {
	lower := strings.ToLower(text)
	if slices.ContainsFunc(dnsWords, func(w string) bool { return strings.Contains(lower, w) }) {
		return ProblemDNS
	}
	return ProblemOther
}

func cut(s string, n int) string {
	if len(s) <= n {
		return s
	}
	return s[:n] + "…"
}

// certMessage says where a certificate stands, for people: what is happening
// or went wrong, what follows, and how to fix it.
func certMessage(domain string, st CertState, check *resource.DomainCheck) string {
	retry := "Dockwell tries again by itself."
	switch {
	case st.Phase == CertIssued && st.Problem == ProblemNone:
		return ""
	case st.Phase == CertIssued:
		return fmt.Sprintf("Renewing the certificate for %s failed, though the one it has still serves until %s. %s %s",
			domain, st.NotAfter.UTC().Format("2 January 2006"), problemWords(domain, st, check), retry)
	case st.Phase == CertPending:
		if check != nil && check.State == resource.DomainNoRecord {
			return fmt.Sprintf("%s has no DNS record yet, so its certificate can't come and it doesn't answer over HTTPS. %s Dockwell tries as soon as it sees the record.", domain, pointHere(check))
		}
		if check != nil && check.State == resource.DomainElsewhere {
			return fmt.Sprintf("DNS for %s points at %s, not this server, so its certificate can't come and it doesn't answer over HTTPS here. %s Dockwell tries as soon as it sees the record.",
				domain, strings.Join(check.Found, " and "), pointHere(check))
		}
		return fmt.Sprintf("Getting a certificate for %s. It answers over HTTPS once that's done, which usually takes under a minute.", domain)
	}
	return fmt.Sprintf("%s doesn't answer over HTTPS yet: %s %s", domain, problemWords(domain, st, check), retry)
}

// problemWords says what went wrong, and how to fix it.
func problemWords(domain string, st CertState, check *resource.DomainCheck) string {
	switch st.Problem {
	case ProblemDNS:
		return fmt.Sprintf("the certificate authority couldn't find %s in DNS. %s", domain, pointHere(check))
	case ProblemConnection:
		return fmt.Sprintf("the certificate authority couldn't reach this server for %s on port 80 or 443, which a firewall often blocks. Open both, in the server's firewall and at your provider.", domain)
	case ProblemWrongServer:
		return fmt.Sprintf("something other than this server answered for %s. %s If a CDN or another proxy is in front, turn it off until the certificate comes.", domain, pointHere(check))
	case ProblemRateLimited:
		return fmt.Sprintf("the certificate authority's rate limit was hit: %s Wait until it lifts.", sentence(st.Detail))
	case ProblemCAA:
		return fmt.Sprintf("a CAA record on %s forbids this certificate authority. Add one that allows it, such as issue \"letsencrypt.org\", or remove the record.", domain)
	case ProblemRefused:
		return fmt.Sprintf("the certificate authority won't issue for %s: %s Choose another name.", domain, sentence(st.Detail))
	case ProblemNone, ProblemOther:
	}
	return fmt.Sprintf("the certificate authority couldn't issue it: %s If this goes on, see docker logs %s.", sentence(st.Detail), ContainerName)
}

func pointHere(check *resource.DomainCheck) string {
	if check != nil && len(check.Here) > 0 {
		return "Point an A or AAAA record at this server, " + strings.Join(check.Here, " or ") + "."
	}
	return "Point an A or AAAA record at this server."
}

func sentence(s string) string {
	s = strings.TrimSpace(s)
	if s == "" {
		return "it gave no reason."
	}
	if !strings.HasSuffix(s, ".") {
		s += "."
	}
	return s
}

var phases = map[CertPhase]dockwellv1.CertificateState{
	CertPending: dockwellv1.CertificateState_CERTIFICATE_STATE_PENDING,
	CertIssued:  dockwellv1.CertificateState_CERTIFICATE_STATE_ISSUED,
	CertFailed:  dockwellv1.CertificateState_CERTIFICATE_STATE_FAILED,
}

var problems = map[Problem]dockwellv1.CertificateProblem{
	ProblemNone:        dockwellv1.CertificateProblem_CERTIFICATE_PROBLEM_UNSPECIFIED,
	ProblemDNS:         dockwellv1.CertificateProblem_CERTIFICATE_PROBLEM_DNS,
	ProblemConnection:  dockwellv1.CertificateProblem_CERTIFICATE_PROBLEM_CONNECTION,
	ProblemWrongServer: dockwellv1.CertificateProblem_CERTIFICATE_PROBLEM_WRONG_SERVER,
	ProblemRateLimited: dockwellv1.CertificateProblem_CERTIFICATE_PROBLEM_RATE_LIMITED,
	ProblemCAA:         dockwellv1.CertificateProblem_CERTIFICATE_PROBLEM_CAA,
	ProblemRefused:     dockwellv1.CertificateProblem_CERTIFICATE_PROBLEM_REFUSED,
	ProblemOther:       dockwellv1.CertificateProblem_CERTIFICATE_PROBLEM_OTHER,
}

// statusBoard is what the agent last said of each domain, and tells the agent
// when it changes.
type statusBoard struct {
	mu      sync.Mutex
	list    []*dockwellv1.DomainStatus
	checks  map[string]*resource.DomainCheck
	checked map[string]time.Time
	signal  chan struct{}
}

func (s *statusBoard) changed() chan struct{} {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.signal == nil {
		s.signal = make(chan struct{}, 1)
	}
	return s.signal
}

// publish keeps a new list, and signals if it differs.
func (s *statusBoard) publish(list []*dockwellv1.DomainStatus) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if slices.EqualFunc(s.list, list, func(a, b *dockwellv1.DomainStatus) bool { return proto.Equal(a, b) }) {
		return
	}
	s.list = list
	if s.signal == nil {
		s.signal = make(chan struct{}, 1)
	}
	select {
	case s.signal <- struct{}{}:
	default:
	}
}

// Domains is where each of this server's routes' domains stands, sorted.
func (m *Manager) Domains() []*dockwellv1.DomainStatus {
	m.status.mu.Lock()
	defer m.status.mu.Unlock()
	return slices.Clone(m.status.list)
}

// DomainsChanged signals when Domains differs from the last time it did.
func (m *Manager) DomainsChanged() <-chan struct{} { return m.status.changed() }

// Timing of watching certificates: quick while one is on its way, as that is
// when someone is waiting for it.
const (
	watchPending = 5 * time.Second
	watchSettled = time.Minute
	// dnsEvery is how often DNS is read for a domain without a certificate.
	dnsEvery = 30 * time.Second
)

// Watch keeps Domains up to date until ctx is done.
func (m *Manager) Watch(ctx context.Context) {
	for {
		wait := watchSettled
		if m.watchOnce(ctx) {
			wait = watchPending
		}
		select {
		case <-ctx.Done():
			return
		case <-time.After(wait):
		}
	}
}

// watchOnce reads where each domain stands, says so if that changed, and has
// the proxy try again for a domain whose DNS has just come to point here, as
// it may otherwise wait hours between tries. It reports whether any domain is
// still without a certificate.
func (m *Manager) watchOnce(ctx context.Context) (waiting bool) {
	if !Active(m.Dir) {
		m.status.publish(nil)
		return false
	}
	routes, err := m.Proxy.Routes(ctx)
	if err != nil {
		if ctx.Err() == nil {
			m.logger().DebugContext(ctx, "can't read the ingress's routes", slog.String("error", err.Error()))
		}
		return false
	}
	domains := map[string]bool{}
	for _, r := range routes {
		domains[r.Domain] = true
	}
	names := slices.Sorted(maps.Keys(domains))
	certs, err := m.Proxy.Certificates(ctx, names)
	if err != nil {
		if ctx.Err() == nil {
			m.logger().WarnContext(ctx, "can't read where the ingress's certificates stand", slog.String("error", err.Error()))
		}
		return false
	}
	retry := false
	list := make([]*dockwellv1.DomainStatus, 0, len(names))
	for _, name := range names {
		st := certs[name]
		check, nowHere := m.domainCheck(ctx, name, st)
		retry = retry || (nowHere && st.Phase != CertIssued)
		waiting = waiting || st.Phase != CertIssued
		list = append(list, &dockwellv1.DomainStatus{Domain: name, Certificate: certProto(name, st, check), Dns: resourcepb.DomainCheck(check)})
	}
	m.status.publish(list)
	if retry {
		m.logger().InfoContext(ctx, "a domain's DNS now points here, so the ingress tries for its certificate at once")
		if err := m.Proxy.Retry(ctx); err != nil && ctx.Err() == nil {
			m.logger().WarnContext(ctx, "can't have the ingress try for a certificate now; it tries again by itself", slog.String("error", err.Error()))
		}
	}
	return waiting
}

// domainCheck is what DNS says of a domain, read again every dnsEvery while it
// has no certificate. nowHere says this read found it pointing here, as the
// one before didn't.
func (m *Manager) domainCheck(ctx context.Context, name string, st CertState) (check *resource.DomainCheck, nowHere bool) {
	b := &m.status
	b.mu.Lock()
	if b.checks == nil {
		b.checks, b.checked = map[string]*resource.DomainCheck{}, map[string]time.Time{}
	}
	last, read := b.checks[name], b.checked[name]
	b.mu.Unlock()
	if st.Phase == CertIssued && last != nil {
		return last, false
	}
	if last != nil && time.Since(read) < dnsEvery {
		return last, false
	}
	check = m.dns().Check(ctx, name)
	b.mu.Lock()
	b.checks[name], b.checked[name] = check, time.Now()
	b.mu.Unlock()
	return check, last != nil && last.State != resource.DomainHere && check.State == resource.DomainHere
}

func certProto(domain string, st CertState, check *resource.DomainCheck) *dockwellv1.Certificate {
	out := &dockwellv1.Certificate{State: phases[st.Phase], Problem: problems[st.Problem], Message: certMessage(domain, st, check)}
	if !st.NotAfter.IsZero() {
		out.ExpireTime = timestamppb.New(st.NotAfter)
	}
	return out
}
