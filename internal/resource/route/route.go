// SPDX-License-Identifier: AGPL-3.0-only

// Package route is the "route" kind: a domain, or a path under it, sent to a
// port of a service in the route's environment or a port on the server itself,
// over HTTPS, or over plain HTTP when TLS ends before the server. What runs it on
// a server is an Ingress, which names no proxy (internal/ingress). See
// docs/adr/0024-the-caddy-ingress-driver.md and
// docs/adr/0030-routes-behind-a-tunnel-or-a-proxy.md.
package route

import (
	"context"
	"errors"
	"fmt"
	"net"
	"slices"
	"strconv"
	"strings"
	"time"

	"google.golang.org/protobuf/proto"

	resourcev1 "github.com/dockwell/dockwell/gen/dockwell/resource/v1alpha1"
	"github.com/dockwell/dockwell/internal/id"
	"github.com/dockwell/dockwell/internal/ingress"
	"github.com/dockwell/dockwell/internal/resource"
	"github.com/dockwell/dockwell/internal/resource/service"
)

// Kind is the route kind.
const Kind resource.Kind = "route"

// RestartDowntime is how long what a proxy serves is down as it restarts on a
// newer version, at most: the 30 s it gets to stop before Docker kills it, for
// what is in flight, and 5 s to start again.
const RestartDowntime = 35 * time.Second

// Ingress is what runs routes on a server. On the control plane, which only
// reads and checks specs, there is none.
type Ingress interface {
	// Observe returns the route as the proxy has it, or nil if it has none.
	Observe(ctx context.Context, ref resource.Ref) (*resourcev1.Route, error)
	// Check refuses what stops the route running on this server, such as
	// another proxy holding its public ports.
	Check(ctx context.Context, ref resource.Ref, desired []resource.Desired) error
	// Judge says what the step does to the ingress, given every resource the
	// plan touches on this server, and what DNS says of the domain.
	Judge(ctx context.Context, ref resource.Ref, spec *resourcev1.Route, desired []resource.Desired) (*resource.IngressEffect, *resource.DomainCheck, error)
	// Returning names stopped containers that Docker starts again when it next
	// restarts, and that may then take the proxy's ports. It only warns.
	Returning(ctx context.Context, ref resource.Ref, spec *resourcev1.Route, desired []resource.Desired) []string
	// Listening names what listens on a port on the server, or says nothing does.
	// With neither, it couldn't say. It only informs.
	Listening(ctx context.Context, port uint32) (who string, nothing bool)
	// Apply makes the proxy serve the route, or stop serving it when spec is
	// nil, and keeps connections up across the change.
	Apply(ctx context.Context, ref resource.Ref, spec *resourcev1.Route, effect *resource.IngressEffect) error
	// Verify confirms the proxy is up and has the route.
	Verify(ctx context.Context, ref resource.Ref) error
	// Ready says why the ingress can't run on this server now.
	Ready(ctx context.Context) error
	// ReadyKey is what the answer is about, shared with what else asks it.
	ReadyKey() any
}

// Driver is the route kind. Reserved is the panel's domain, on the control
// plane and on the agent beside it: no route may claim it or a name under it.
type Driver struct {
	Ingress  Ingress
	Reserved string
}

var (
	_ resource.Driver        = (*Driver)(nil)
	_ resource.Referrer      = (*Driver)(nil)
	_ resource.Claimer       = (*Driver)(nil)
	_ resource.TakesOver     = (*Driver)(nil)
	_ resource.SingleServer  = (*Driver)(nil)
	_ resource.Checker       = (*Driver)(nil)
	_ resource.Judge         = (*Driver)(nil)
	_ resource.Readier       = (*Driver)(nil)
	_ resource.SharedReadier = (*Driver)(nil)
	_ resource.HostAccesser  = (*Driver)(nil)
	_ resource.WeakensClaims = (*Driver)(nil)
)

// Kind returns "route".
func (*Driver) Kind() resource.Kind { return Kind }

// Level is the environment: a route serves one of its services.
func (*Driver) Level() resource.Level { return resource.LevelEnvironment }

// IDPrefix is "rte".
func (*Driver) IDPrefix() string { return id.Route }

// New returns an empty Route.
func (*Driver) New() proto.Message { return &resourcev1.Route{} }

// CheckName requires a DNS label, such as www or api-v2.
func (*Driver) CheckName(name string) error { return resource.CheckLabel("route", name, "www") }

// reservedTLDs are names no public certificate authority issues for, by the
// special-use lists of RFC 6761 and 9476, and ICANN's .internal.
var reservedTLDs = map[string]bool{
	"localhost": true, "local": true, "internal": true, "test": true, "example": true,
	"invalid": true, "onion": true, "alt": true, "arpa": true,
}

// CheckDomain refuses what can't be a route's domain, saying what to write
// instead. Only letters, digits, dashes and dots get through it, so a domain
// can't carry a Caddy placeholder or any other config.
func CheckDomain(d string) error {
	switch {
	case d == "":
		return errors.New("a route needs a domain, such as app.example.com")
	case strings.HasPrefix(d, "*"):
		return fmt.Errorf("%s is a wildcard, which needs a DNS-01 certificate, and Dockwell gets certificates over HTTP-01 and TLS-ALPN-01. Add a route for each name, such as app.example.com", d)
	case net.ParseIP(strings.Trim(d, "[]")) != nil:
		return fmt.Errorf("%s is an IP address, and a route needs a name that a certificate authority issues for. Point a domain's DNS record at this server, and use that, such as app.example.com", d)
	case strings.HasSuffix(d, "."):
		return fmt.Errorf("%s ends in a dot. Leave it off, as in %s", d, strings.TrimSuffix(d, "."))
	case len(d) > 253:
		return fmt.Errorf("the domain %s… is longer than the 253 characters DNS allows. Use a shorter name", d[:40])
	}
	labels := strings.Split(d, ".")
	if len(labels) < 2 {
		return fmt.Errorf("%s needs a dot, such as app.example.com, as a certificate authority issues only for public names", d)
	}
	for _, l := range labels {
		if err := checkLabel(d, l); err != nil {
			return err
		}
	}
	last := labels[len(labels)-1]
	if _, err := strconv.Atoi(last); err == nil {
		return fmt.Errorf("%s ends in a number, as an IP address does, and a route needs a name that a certificate authority issues for, such as app.example.com", d)
	}
	if reservedTLDs[last] {
		return fmt.Errorf("%s is under .%s, which is reserved, so no public certificate authority issues for it. Use a public domain, such as app.example.com", d, last)
	}
	return nil
}

func checkLabel(domain, l string) error {
	if l == "" || len(l) > 63 || l[0] == '-' || l[len(l)-1] == '-' {
		return fmt.Errorf("%s isn't a valid domain: each part between dots has 1 to 63 characters, and starts and ends with a letter or digit, as in app.example.com", domain)
	}
	for _, r := range l {
		switch {
		case r >= 'a' && r <= 'z', r >= '0' && r <= '9', r == '-':
		case r >= 'A' && r <= 'Z':
			return fmt.Errorf("%s has capitals. Write it in lowercase, as in %s", domain, strings.ToLower(domain))
		case r > 127:
			return fmt.Errorf("%s isn't plain ASCII. Write an international name in punycode, as in xn--bcher-kva.example", domain)
		default:
			return fmt.Errorf("%s has a %q, and a domain takes only letters, digits, dashes and dots, as in app.example.com", domain, r)
		}
	}
	return nil
}

// CheckPath refuses what can't be a route's path, saying what to write
// instead. Only unreserved characters get through it, so a path can't carry
// a Caddy placeholder or wildcard, and capitals can't compare differently
// here and in Caddy's matcher, which ignores case.
func CheckPath(p string) error {
	if p == "" {
		return nil
	}
	if !strings.HasPrefix(p, "/") {
		return fmt.Errorf("the path %s needs to start with a /, as in /api", p)
	}
	if len(p) > 1024 {
		return fmt.Errorf("the path %s… is longer than 1024 characters. Use a shorter one", p[:40])
	}
	if strings.HasSuffix(p, "/") {
		return fmt.Errorf("the path %s ends in a /. Leave it off: %s also covers what is under it", p, strings.TrimSuffix(p, "/"))
	}
	for _, seg := range strings.Split(p[1:], "/") {
		if seg == "" {
			return fmt.Errorf("the path %s has an empty part. Write it with one / between parts, as in /api/v1", p)
		}
		if seg == "." || seg == ".." {
			return fmt.Errorf("the path %s has a %s in it, which browsers remove before they send it. Write the path it means, as in /api/v1", p, seg)
		}
		for _, r := range seg {
			switch {
			case r >= 'a' && r <= 'z', r >= '0' && r <= '9', r == '-', r == '_', r == '.', r == '~':
			case r >= 'A' && r <= 'Z':
				return fmt.Errorf("the path %s has capitals, and paths match whatever their case. Write it in lowercase, as in %s", p, strings.ToLower(p))
			default:
				return fmt.Errorf("the path %s has a %q, and a path takes only lowercase letters, digits, dashes, underscores, dots and tildes between its slashes, as in /api/v1", p, r)
			}
		}
	}
	return nil
}

// UnderOrIs reports a domain that is name, or under it.
func UnderOrIs(domain, name string) bool {
	return domain == name || strings.HasSuffix(domain, "."+name)
}

// Validate checks a route and normalizes its path: / is the whole domain, as
// empty is.
func (d *Driver) Validate(spec proto.Message) error {
	r := spec.(*resourcev1.Route)
	if r.GetPath() == "/" {
		r.Path = ""
	}
	if err := CheckDomain(r.GetDomain()); err != nil {
		return err
	}
	if err := CheckPath(r.GetPath()); err != nil {
		return err
	}
	if err := checkTLS(r); err != nil {
		return err
	}
	if d.Reserved != "" && UnderOrIs(r.GetDomain(), d.Reserved) {
		if r.GetTls() == ingress.TLSOff {
			return fmt.Errorf("%s can't be served without a certificate, as the panel's passkeys and cookies need HTTPS on its own domain, %s, and the names under it. Choose another name for this route", r.GetDomain(), d.Reserved)
		}
		if r.GetDomain() == d.Reserved {
			return fmt.Errorf("%s is the panel's own domain, so no route can claim it: the panel's sign-in depends on it. Choose another name for this route", r.GetDomain())
		}
		return fmt.Errorf("%s is under the panel's own domain, %s, where browsers offer the panel's passkeys to anything that asks. Choose a name that isn't under it", r.GetDomain(), d.Reserved)
	}
	proxies, err := ingress.NormalizeTrustedProxies(r.GetTrustedProxies())
	if err != nil {
		return err
	}
	r.TrustedProxies = proxies
	if len(proxies) == 0 {
		r.TrustedProxies = nil
	}
	if r.GetPlainForAnyone() && r.GetTls() != ingress.TLSOff {
		return errors.New("a route's plain_for_anyone says who answers over plain HTTP, and this route takes a certificate. Set tls to off with it, or leave it out")
	}
	if r.GetHostPort() != 0 {
		return checkHostPort(r)
	}
	if err := resource.CheckLabel("service", r.GetService(), "web"); err != nil {
		if r.GetService() == "" {
			return errors.New("a route needs a service to send to, in its environment, such as web, or a port on the server itself in host_port")
		}
		return err
	}
	if p := r.GetPort(); p == 0 || p > 65535 {
		return fmt.Errorf("a route needs the port its service listens on inside its container, from 1 to 65535, such as 8080, not %d", p)
	}
	return nil
}

// dockerAPIPorts are Docker's own ports for its API over TCP. Nothing else has a
// claim on them, and a route to one is root for whoever finds it.
var dockerAPIPorts = []uint32{2375, 2376}

// checkTLS refuses a tls that means nothing: a certificate is what a route gets
// unless it says TLS ends before this server.
func checkTLS(r *resourcev1.Route) error {
	if t := r.GetTls(); t != "" && t != ingress.TLSOff {
		return fmt.Errorf("a route's tls is %q, and it takes only %q, when TLS ends before this server, as at a tunnel or a proxy in front of it. Leave it empty to get a certificate", t, ingress.TLSOff)
	}
	return nil
}

// checkHostPort refuses a route that names a port on the server and a service
// too, as only one of them can be where it sends. Which ports are Dockwell's own
// is the server's to say (Ingress.Check).
func checkHostPort(r *resourcev1.Route) error {
	if r.GetHostPort() > 65535 {
		return fmt.Errorf("a route's host_port is a port on the server itself, from 1 to 65535, such as 8080, not %d", r.GetHostPort())
	}
	if slices.Contains(dockerAPIPorts, r.GetHostPort()) {
		return fmt.Errorf("port %d is Docker's API, which is root on the server to anyone who reaches it, so no route may send to it. If you run something else there, move it to another port", r.GetHostPort())
	}
	if r.GetService() != "" || r.GetPort() != 0 {
		return fmt.Errorf("this route names a service and the server's port %d, and a route sends to one or the other. Leave out service and port, or host_port", r.GetHostPort())
	}
	return nil
}

// References is the service the route sends to. What that service reaches on
// its server isn't the route's: it sends traffic there, and has no part in
// what the service mounts.
func (*Driver) References(spec proto.Message) []resource.Reference {
	s := spec.(*resourcev1.Route).GetService()
	if s == "" {
		return nil
	}
	return []resource.Reference{{Kind: service.Kind, Name: s, Verb: "serve", Apart: true}}
}

// ReferenceKinds is services, the one kind a route names.
func (*Driver) ReferenceKinds() []resource.Kind { return []resource.Kind{service.Kind} }

// HostAccess is what a person approves of a route: a port on the server itself,
// which reaches past containers, and each proxy it believes on where a request
// came from. Everything else a route does, a token can (ADR 0030).
func (*Driver) HostAccess(spec proto.Message) []resource.Access {
	r := spec.(*resourcev1.Route)
	var out []resource.Access
	if p := r.GetHostPort(); p != 0 {
		a := resource.Access{Kind: resource.AccessServerPort, Path: ingress.ServerHost + ":" + strconv.FormatUint(uint64(p), 10), Detail: Where(r)}
		if r.GetTls() == ingress.TLSOff {
			a.Type = "http"
		}
		out = append(out, a)
	}
	for _, proxy := range r.GetTrustedProxies() {
		out = append(out, resource.Access{Kind: resource.AccessTrustedProxy, Path: proxy})
	}
	if r.GetPlainForAnyone() {
		out = append(out, resource.Access{Kind: resource.AccessPlainForAnyone, Path: r.GetDomain()})
	}
	return out
}

// Weak reports a route that takes no certificate, against one that does.
func (*Driver) Weak(spec proto.Message) bool { return plain(spec) }

// Weakens is what a person approves as a domain with a certificate goes off HTTPS.
func (*Driver) Weakens(group string) resource.Access {
	return resource.Access{Kind: resource.AccessHTTPSOff, Path: group}
}

// Claims are the route's domain and path, which one route alone may hold, and
// its domain, which one environment alone may.
func (*Driver) Claims(spec proto.Message) []resource.Claim {
	r := spec.(*resourcev1.Route)
	c := resource.Claim{
		Key: r.GetDomain() + r.GetPath(), Group: r.GetDomain(),
		Words: Where(r), GroupWords: "the domain " + r.GetDomain(), MannerWords: "takes a certificate",
	}
	if plain(r) {
		c.Manner, c.MannerWords = ingress.TLSOff, "takes none"
	}
	return []resource.Claim{c}
}

// TakesOver is what a person approves as an environment claims a domain
// another one held.
func (*Driver) TakesOver(group, held string) resource.Access {
	return resource.Access{Kind: resource.AccessDomain, Path: group, Detail: held}
}

// OneServer says a route runs on one server, until the control plane issues
// certificates for a fleet.
func (*Driver) OneServer() string {
	return "a certificate comes over HTTP-01 or TLS-ALPN-01 to the server its domain points at, which fails some of the time from two"
}

// Where is a route's domain and path, as people read them.
func Where(r *resourcev1.Route) string { return r.GetDomain() + r.GetPath() }

// Observe asks the ingress.
func (d *Driver) Observe(ctx context.Context, ref resource.Ref) (proto.Message, error) {
	if d.Ingress == nil {
		return nil, errors.New("this control plane has no ingress of its own to read")
	}
	r, err := d.Ingress.Observe(ctx, ref)
	if err != nil || r == nil {
		return nil, err
	}
	return r, nil
}

// Plan is what a route's step is, from the specs alone, as the control plane
// plans an offline server. A route changes in place, with nothing stopped,
// whatever about it changes. It says when a route starts serving plain HTTP,
// which a plan says plainly, and needs no person.
func (*Driver) Plan(actual, desired proto.Message, _ []resource.Change) (resource.Decision, error) {
	switch {
	case desired == nil:
		return resource.Decision{Action: resource.ActionDelete}, nil
	case actual == nil:
		return withAnswers(resource.Decision{Action: resource.ActionCreate, Unencrypted: plain(desired)}, desired), nil
	}
	return withAnswers(resource.Decision{Action: resource.ActionUpdate, Unencrypted: plain(desired) && !plain(actual)}, desired), nil
}

// withAnswers says who a route answers, when it isn't what every route answers:
// anyone, over HTTPS, sent to its service.
func withAnswers(dec resource.Decision, spec proto.Message) resource.Decision {
	r := spec.(*resourcev1.Route)
	port := r.GetHostPort()
	if !plain(r) && port == 0 {
		return dec
	}
	dec.Answers = &resource.RouteAnswers{Reach: resource.ReachAnyone, Plain: plain(r)}
	switch {
	case !plain(r) || r.GetPlainForAnyone():
	case len(r.GetTrustedProxies()) > 0:
		dec.Answers.Reach, dec.Answers.Proxies = resource.ReachProxies, r.GetTrustedProxies()
	default:
		dec.Answers.Reach = resource.ReachThisServer
	}
	if port != 0 {
		dec.ServerPort = &resource.ServerPortCheck{Port: port}
	}
	return dec
}

// plain reports a route that takes no certificate.
func plain(spec proto.Message) bool { return spec.(*resourcev1.Route).GetTls() == ingress.TLSOff }

// Judge is Plan with what the server knows: what the step does to its
// ingress, how long that takes, and what DNS says of the domain.
func (d *Driver) Judge(ctx context.Context, ref resource.Ref, actual, desired proto.Message, changes []resource.Change) (resource.Decision, error) {
	dec, err := d.Plan(actual, desired, changes)
	if err != nil || d.Ingress == nil {
		return dec, err
	}
	var spec *resourcev1.Route
	if desired != nil {
		spec = desired.(*resourcev1.Route)
	}
	effect, check, err := d.Ingress.Judge(ctx, ref, spec, resource.DesiredIn(ctx))
	if err != nil {
		return dec, err
	}
	dec.Ingress, dec.Domain = effect, check
	dec.Returning = d.Ingress.Returning(ctx, ref, spec, resource.DesiredIn(ctx))
	if c := dec.ServerPort; c != nil {
		c.Listening, c.Nothing = d.Ingress.Listening(ctx, c.Port)
	}
	if effect != nil && effect.Change == resource.IngressRestarts {
		dec.Effect = resource.EffectRestart
		dec.Downtime = RestartDowntime
	}
	return dec, nil
}

// Check refuses what stops the route running here, for the whole of what
// the plan touches on this server.
func (d *Driver) Check(ctx context.Context, ref resource.Ref, _ proto.Message) error {
	if d.Ingress == nil {
		return nil
	}
	return d.Ingress.Check(ctx, ref, resource.DesiredIn(ctx))
}

// Apply makes the ingress serve the route, or stop serving it.
func (d *Driver) Apply(ctx context.Context, step resource.Step, desired proto.Message) error {
	if d.Ingress == nil {
		return errors.New("this control plane has no ingress of its own to change")
	}
	var spec *resourcev1.Route
	if desired != nil {
		spec = desired.(*resourcev1.Route)
	}
	return d.Ingress.Apply(ctx, step.Ref, spec, step.Ingress)
}

// Verify confirms the ingress is up and has the route.
func (d *Driver) Verify(ctx context.Context, ref resource.Ref, _ proto.Message) error {
	if d.Ingress == nil {
		return errors.New("this control plane has no ingress of its own to check")
	}
	return d.Ingress.Verify(ctx, ref)
}

// Ready says why routes can't run on this server now.
func (d *Driver) Ready(ctx context.Context) error {
	if d.Ingress == nil {
		return nil
	}
	return d.Ingress.Ready(ctx)
}

// ReadyKey is the ingress's.
func (d *Driver) ReadyKey() any {
	if d.Ingress == nil {
		return d
	}
	return d.Ingress.ReadyKey()
}
