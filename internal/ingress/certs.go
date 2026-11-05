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
	// ProblemCantReachCA is this server failing to reach the certificate
	// authority at all, so nothing about the domain is known yet.
	ProblemCantReachCA
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
	// None says it has no certificate and asks for none, as TLS ends before this
	// server. Phase says nothing then.
	None bool
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
		if cantReachCA(text) {
			return ProblemCantReachCA, cut(text, limit), true
		}
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

// localWords are what this server's own request to the certificate authority
// fails with, which never carries an ACME problem type: the authority didn't
// answer, so it has said nothing of the domain. Each is a part of a Go network
// error, or of what the ACME client wraps one in.
var localWords = []string{"dial tcp", "dial udp", "lookup ", "connection refused", "connection reset", "network is unreachable", "no route to host", "i/o timeout", "tls handshake", "x509:", "performing request", "making new acme client", "registering account"}

// cantReachCA reports a failure of this server's own request to the
// certificate authority. It is asked only of words with no ACME problem type,
// so a resolver failure here isn't blamed on the domain's DNS.
func cantReachCA(text string) bool {
	lower := strings.ToLower(text)
	return slices.ContainsFunc(localWords, func(w string) bool { return strings.Contains(lower, w) })
}

// lookupFailed are how a certificate authority says its DNS lookup broke, as
// opposed to finding no record: the domain's nameservers didn't answer, so no
// A record would help.
var lookupFailed = []string{"servfail", "looking up caa", "timeout", "refused", "nameservers may be malfunctioning"}

func dnsLookupFailed(detail string) bool {
	lower := strings.ToLower(detail)
	return slices.ContainsFunc(lookupFailed, func(w string) bool { return strings.Contains(lower, w) })
}

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
	case st.None:
		return "None: TLS ends before this server, so it has no certificate here and asks for none."
	case st.Phase == CertIssued && st.Problem == ProblemNone:
		return ""
	case st.Phase == CertIssued:
		return fmt.Sprintf("Renewing the certificate for %s failed, though the one it has still serves until %s. %s %s",
			domain, st.NotAfter.UTC().Format("2 January 2006"), upperFirst(problemWords(domain, st, check)), retry)
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
	case ProblemCantReachCA:
		return fmt.Sprintf("this server couldn't reach the certificate authority: %s Nothing is known yet about %s, so its DNS may be fine. Check this server's own DNS resolver and outbound network.", sentence(st.Detail), domain)
	case ProblemDNS:
		if dnsLookupFailed(st.Detail) {
			return fmt.Sprintf("the certificate authority's DNS lookup for %s failed: %s Check that the domain's nameservers answer; adding an A record won't help until they do.", domain, sentence(st.Detail))
		}
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

// offWords say what a route's domain can do instead, when its certificate can't
// come because something in front of the server answers for it. They aren't for
// the panel's own domain, which can't be served this way.
const offWords = "If this server sits behind a tunnel, or a proxy that ends TLS, it needs no certificate: set tls to off on its routes."

// routeCertMessage is certMessage for a route's domain, which also says when the
// setting for a server behind a tunnel or a proxy may be the fix. A limit, a CAA
// record and a refused name aren't what a tunnel causes.
func routeCertMessage(domain string, st CertState, check *resource.DomainCheck) string {
	msg := certMessage(domain, st, check)
	switch {
	case st.None, msg == "":
		return msg
	case st.Phase == CertPending:
		if check == nil || check.State != resource.DomainElsewhere {
			return msg
		}
	case st.Problem == ProblemRateLimited, st.Problem == ProblemCAA, st.Problem == ProblemRefused:
		return msg
	}
	return msg + " " + offWords
}

func pointHere(check *resource.DomainCheck) string {
	if check != nil && len(check.Here) > 0 {
		return "Point an A or AAAA record at this server, " + strings.Join(check.Here, " or ") + "."
	}
	return "Point an A or AAAA record at this server."
}

// upperFirst starts a sentence made of words that may follow a colon elsewhere.
func upperFirst(s string) string {
	if s == "" {
		return s
	}
	return strings.ToUpper(s[:1]) + s[1:]
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
	ProblemCantReachCA: dockwellv1.CertificateProblem_CERTIFICATE_PROBLEM_CANT_REACH_CA,
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
	// panel is what was last logged of the panel's own certificate.
	panel string
	// problem is why the ingress isn't serving, as Reconcile last found it.
	problem *problem
	// outdated is whether the proxy read as outdated when it was last looked at.
	outdated bool
	// ownPorts is what was last logged of routes to ports Dockwell listens on.
	ownPorts string
	// ingress is what was last published of the ingress.
	ingress *dockwellv1.IngressStatus
}

// problem is why a server's ingress isn't serving.
type problem struct {
	kind    dockwellv1.IngressProblem
	message string
	holders []string
}

// report keeps why the ingress isn't serving, or nil once it is.
func (m *Manager) report(p *problem) {
	m.status.mu.Lock()
	defer m.status.mu.Unlock()
	m.status.problem = p
}

// Ingress is where this server's ingress stands, or nil for a server with no
// routes, whose ports nothing here wants.
func (m *Manager) Ingress() *dockwellv1.IngressStatus {
	m.status.mu.Lock()
	defer m.status.mu.Unlock()
	return m.status.ingress
}

// publishIngress reads where the ingress stands, and signals if that changed.
func (m *Manager) publishIngress(ctx context.Context) {
	var out *dockwellv1.IngressStatus
	if Active(m.Dir) {
		st, err := m.Proxy.Status(ctx)
		if err != nil {
			return // Docker is down, and the agent says so already
		}
		out = &dockwellv1.IngressStatus{
			Proxy: m.Proxy.Name(), Running: st.Running, Outdated: st.Outdated, Version: st.Version, PinnedVersion: st.Pinned,
		}
		m.status.mu.Lock()
		p := m.status.problem
		m.status.mu.Unlock()
		switch {
		case p != nil:
			out.Problem, out.Message, out.Holders = p.kind, p.message, p.holders
		case !st.Running:
			out.Problem = dockwellv1.IngressProblem_INGRESS_PROBLEM_NOT_RUNNING
			out.Message = fmt.Sprintf("%s isn't running on this server, so its routes don't answer. The agent starts it within a minute. If it doesn't, see why with docker logs %s", m.Proxy.Name(), ContainerName)
		}
	}
	b := &m.status
	b.mu.Lock()
	defer b.mu.Unlock()
	if proto.Equal(b.ingress, out) {
		return
	}
	b.ingress = out
	if b.signal == nil {
		b.signal = make(chan struct{}, 1)
	}
	select {
	case b.signal <- struct{}{}:
	default:
	}
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
		case <-m.watchWakes():
		}
	}
}

// watchWakes signals Watch to read again now, as someone waits on what a route that
// has just changed says of itself, and a settled server otherwise waits a minute.
func (m *Manager) watchWakes() chan struct{} {
	m.wakesOnce.Do(func() { m.wakes = make(chan struct{}, 1) })
	return m.wakes
}

// watchNow has Watch read again now, without waiting for it.
func (m *Manager) watchNow() {
	select {
	case m.watchWakes() <- struct{}{}:
	default:
	}
}

// watchOnce reads where each domain stands, says so if that changed, and has
// the proxy try again for a domain whose DNS has just come to point here, as
// it may otherwise wait hours between tries. It reports whether any domain is
// still without a certificate.
func (m *Manager) watchOnce(ctx context.Context) (waiting bool) {
	m.publishIngress(ctx)
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
	// A domain has a certificate if any of its routes wants one, as a plan refuses
	// the two together, and it can only be for the moment a plan's steps take.
	domains := map[string]bool{}
	for i := range routes {
		domains[routes[i].Domain] = domains[routes[i].Domain] || !routes[i].Plain
	}
	var names []string
	for _, name := range slices.Sorted(maps.Keys(domains)) {
		if domains[name] {
			names = append(names, name)
		}
	}
	certs, err := m.Proxy.Certificates(ctx, names)
	if err != nil {
		if ctx.Err() == nil {
			m.logger().WarnContext(ctx, "can't read where the ingress's certificates stand", slog.String("error", err.Error()))
		}
		return false
	}
	retry := false
	list := make([]*dockwellv1.DomainStatus, 0, len(domains))
	for _, name := range slices.Sorted(maps.Keys(domains)) {
		if !domains[name] {
			// Its DNS points at whatever is in front, which says nothing here.
			list = append(list, &dockwellv1.DomainStatus{Domain: name, Certificate: certProto(name, CertState{None: true}, nil)})
			continue
		}
		st := certs[name]
		check, nowHere := m.domainCheck(ctx, name, st)
		retry = retry || (nowHere && st.Phase != CertIssued)
		waiting = waiting || st.Phase != CertIssued
		list = append(list, &dockwellv1.DomainStatus{Domain: name, Certificate: certProto(name, st, check), Dns: resourcepb.DomainCheck(check)})
	}
	m.status.publish(list)
	m.reportPanel(ctx)
	if retry {
		m.logger().InfoContext(ctx, "a domain's DNS now points here, so the ingress tries for its certificate at once")
		if err := m.Proxy.Retry(ctx); err != nil && ctx.Err() == nil {
			m.logger().WarnContext(ctx, "can't have the ingress try for a certificate now; it tries again by itself", slog.String("error", err.Error()))
		}
	}
	return waiting
}

// reportPanel logs where the panel's own certificate stands when that changes.
// It isn't a route, so the API has no word of it, and someone who can't reach
// the panel by its domain reads the server's log.
func (m *Manager) reportPanel(ctx context.Context) {
	site := m.panelSite(ctx)
	if site == nil {
		return
	}
	certs, err := m.Proxy.Certificates(ctx, []string{site.Domain})
	if err != nil {
		return
	}
	st := certs[site.Domain]
	msg := certMessage(site.Domain, st, nil)
	b := &m.status
	b.mu.Lock()
	changed := msg != b.panel
	b.panel = msg
	b.mu.Unlock()
	if !changed || msg == "" {
		return
	}
	if st.Phase == CertFailed {
		m.logger().WarnContext(ctx, "the panel's domain doesn't answer over HTTPS; reach the panel through an SSH tunnel", slog.String("why", msg))
		return
	}
	m.logger().InfoContext(ctx, "the panel's certificate is on its way", slog.String("domain", site.Domain))
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
	out := &dockwellv1.Certificate{State: phases[st.Phase], Problem: problems[st.Problem], Message: routeCertMessage(domain, st, check)}
	if st.None {
		out.State = dockwellv1.CertificateState_CERTIFICATE_STATE_NONE
	}
	if !st.NotAfter.IsZero() {
		out.ExpireTime = timestamppb.New(st.NotAfter)
	}
	return out
}
