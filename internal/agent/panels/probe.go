// SPDX-License-Identifier: AGPL-3.0-only

package panels

import (
	"cmp"
	"context"
	"crypto/tls"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/netip"
	"net/url"
	"regexp"
	"strconv"
	"strings"
	"sync"
	"syscall"
	"time"

	"github.com/dockwell/dockwell/internal/resource"
)

// The probe of the domain handover (ADR 0089): before anything stops, each domain
// nothing else held is asked for through the old proxy, from the server, over
// HTTPS with its name as SNI, with a GET for / and each path a rule names for it.
// Only what anyone would get counts as open: a 2xx, or a redirect on the same host
// to anything but a sign-in. A login or an address limit answers otherwise, as
// 401, 403 or 407, a redirect to another host or a sign-in, or a TLS handshake
// that fails, as one asking for a client certificate does, and so does an app
// that's down, with a 5xx: each holds the domain back. A 404 says nothing either
// way, so its domain can be chosen, and isn't chosen already. Nothing the proxy
// answers is followed.
//
// It's asked on an address of the server's own that isn't loopback, a public one
// first, so the proxy sees it come from that address and not from Docker's
// gateway, which an allowlist of private networks lets in. An address limit that
// lets in this server, or the network it's on, still can't be seen from here, and
// what it was asked from goes with what it found, for a person to weigh.

// proxyPort is where the old proxy listens for HTTPS on the server.
const proxyPort = "443"

// probeHost is the address of the server's own the probe asks the proxy on: a
// public address on any interface but loopback and the bridges containers run on,
// or, with none, a private one, or, with none, loopback.
func probeHost() string {
	ifaces, err := net.Interfaces()
	if err != nil {
		return "127.0.0.1"
	}
	var public, private netip.Addr
	for _, ifc := range ifaces {
		if ifc.Flags&net.FlagUp == 0 || ifc.Flags&net.FlagLoopback != 0 || containerBridge(ifc.Name) {
			continue
		}
		addrs, err := ifc.Addrs()
		if err != nil {
			continue
		}
		for _, a := range addrs {
			ipn, ok := a.(*net.IPNet)
			if !ok {
				continue
			}
			ip, ok := netip.AddrFromSlice(ipn.IP)
			if !ok || !ip.Unmap().Is4() && !ip.IsGlobalUnicast() {
				continue
			}
			ip = ip.Unmap()
			switch {
			case resource.IsPublicAddress(ip) && (!public.IsValid() || ip.Is4() && !public.Is4()):
				public = ip
			case !resource.IsPublicAddress(ip) && ip.IsPrivate() && !private.IsValid():
				private = ip
			}
		}
	}
	switch {
	case public.IsValid():
		return public.String()
	case private.IsValid():
		return private.String()
	}
	return "127.0.0.1"
}

// containerBridge reports an interface Docker or another container runtime made.
func containerBridge(name string) bool {
	for _, p := range []string{"docker", "br-", "veth", "cni", "flannel", "cali", "virbr", "podman"} {
		if strings.HasPrefix(name, p) {
			return true
		}
	}
	return false
}

// probeWait is how long one request may take, and probeWorkers how many run at
// once.
const (
	probeWait    = 5 * time.Second
	probeWorkers = 8
)

// probeAsk is one request: a path of a domain, for the check at index check. to
// is where the domain's route redirects, where it comes as one (ADR 0070).
type probeAsk struct {
	check  int
	domain string
	path   string
	to     string
}

// loginPath is a path a redirect to a sign-in goes to: login, sign-in, auth, SSO,
// OAuth, SAML, CAS and the paths the common forward-auth servers answer on.
var loginPath = sync.OnceValue(func() *regexp.Regexp {
	return regexp.MustCompile(`(?i)(^|[/._-])(log-?in|sign-?in|sign_in|auth|authorize|authenticate|oauth2?|sso|saml|cas|idp|session|authelia|authentik|\.?pomerium|outpost\.goauthentik\.io|cdn-cgi/access)([/._?-]|$)`)
})

// comesBack reports a redirect that says where to come back to, as a sign-in's
// does: rd, redirect, next, return and their kin in its query.
func comesBack(q url.Values) bool {
	for k := range q {
		switch strings.ToLower(k) {
		case "rd", "redirect", "redirect_uri", "redirect_url", "redirect_to", "next", "return", "return_to", "returnto", "returnurl", "return_url", "continue", "came_from":
			return true
		}
	}
	return false
}

// probeAll asks for each in parallel, a few at a time, each in order.
func (r Reader) probeAll(ctx context.Context, asks []probeAsk) []resource.PanelProbe {
	out := make([]resource.PanelProbe, len(asks))
	if len(asks) == 0 {
		return out
	}
	if r.ProbeAddr == "" {
		r.ProbeAddr = net.JoinHostPort(probeHost(), proxyPort)
	}
	next := make(chan int)
	var wg sync.WaitGroup
	for range min(probeWorkers, len(asks)) {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for i := range next {
				out[i] = r.probe(ctx, asks[i].domain, asks[i].path, asks[i].to)
			}
		}()
	}
	for i := range asks {
		next <- i
	}
	close(next)
	wg.Wait()
	return out
}

// probe asks the proxy for a domain's path, and says what answered. A domain that
// comes as a redirect to to is open only where the proxy answers with that
// redirect, the path kept: anything else, a page included, isn't what its route
// will answer, so a guard could be behind it.
func (r Reader) probe(ctx context.Context, domain, p, to string) resource.PanelProbe {
	out := resource.PanelProbe{Path: p}
	addr := cmp.Or(r.ProbeAddr, net.JoinHostPort(probeHost(), proxyPort))
	dialer := &net.Dialer{Timeout: probeWait}
	client := &http.Client{
		Timeout: probeWait,
		Transport: &http.Transport{
			// Every request goes to the proxy on this server, whatever the domain's DNS
			// says: it's the proxy's answer that's asked for. Where it came from is
			// what the proxy saw, unless something between rewrote it.
			DialContext: func(ctx context.Context, network, _ string) (net.Conn, error) {
				conn, err := dialer.DialContext(ctx, network, addr)
				if err == nil {
					if a, ok := conn.LocalAddr().(*net.TCPAddr); ok {
						out.From = a.IP.String()
					}
				}
				return conn, err
			},
			// The proxy's certificate may be one it hasn't got yet, or its own default:
			// what's asked is how it answers, not whom.
			TLSClientConfig:   &tls.Config{ServerName: domain, InsecureSkipVerify: true, MinVersion: tls.VersionTLS12}, //nolint:gosec // the proxy on this server is asked how it answers, not whom; nothing it says is trusted
			DisableKeepAlives: true,
			Proxy:             nil,
		},
		CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse },
	}
	u := &url.URL{Scheme: "https", Host: domain, Path: p}
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, u.String(), http.NoBody)
	if err != nil {
		out.Answer = "no request: " + err.Error()
		return out
	}
	req.Header.Set("User-Agent", "Dockwell handover check")
	resp, err := client.Do(req)
	if err != nil {
		out.Answer = probeFailure(err)
		return out
	}
	defer resp.Body.Close()
	_, _ = io.Copy(io.Discard, io.LimitReader(resp.Body, 64<<10))
	code := strconv.Itoa(resp.StatusCode)
	if to != "" {
		out.Answer, out.Open = redirectAnswer(resp, code, p, to)
		return out
	}
	switch {
	case resp.StatusCode >= 200 && resp.StatusCode < 300:
		out.Answer, out.Open = code, true
	case resp.StatusCode >= 300 && resp.StatusCode < 400:
		loc, err := resp.Location()
		switch {
		case err != nil:
			out.Answer = code + " to nowhere Dockwell can read"
		case !strings.EqualFold(loc.Hostname(), domain):
			out.Answer = fmt.Sprintf("%s to another host, %s", code, loc.Hostname())
		case loginPath().MatchString(loc.Path) || comesBack(loc.Query()):
			out.Answer = fmt.Sprintf("%s to a sign-in, %s", code, loc.Path)
		default:
			out.Answer, out.Open = fmt.Sprintf("%s to %s", code, loc.Path), true
		}
	case resp.StatusCode == http.StatusNotFound:
		out.Answer, out.Unsure = code, true
	default:
		// 401, 403 and 407 are a login's or an address limit's; a 5xx is an app
		// that's down, which could be guarded behind it; any other says nothing
		// Dockwell can count on.
		out.Answer = code
	}
	return out
}

// redirectAnswer says how a domain that comes as a redirect to to answered a
// request for p, and whether that's the redirect its route will answer with.
func redirectAnswer(resp *http.Response, code, p, to string) (string, bool) {
	want, err := url.Parse(to)
	if err != nil {
		return code + ", and Dockwell can't read where its redirect goes", false
	}
	if resp.StatusCode < 300 || resp.StatusCode >= 400 {
		return fmt.Sprintf("%s, where its redirect would send it to %s", code, want.Host), false
	}
	loc, err := resp.Location()
	switch {
	case err != nil:
		return code + " to nowhere Dockwell can read", false
	case loc.Scheme != "https" || !strings.EqualFold(loc.Host, want.Host) || loc.EscapedPath() != (&url.URL{Path: p}).EscapedPath():
		return fmt.Sprintf("%s to %s, where its redirect would send it to %s%s", code, loc.Redacted(), want.Host, p), false
	}
	return fmt.Sprintf("%s to %s, as its redirect does", code, want.Host), true
}

// probeFailure says why a request through the proxy got no answer.
func probeFailure(err error) string {
	var certErr *tls.CertificateVerificationError
	var alert tls.AlertError
	var netErr net.Error
	switch {
	case errors.Is(err, syscall.ECONNREFUSED):
		return "nothing, as nothing listens on port 443"
	case errors.As(err, &alert), errors.As(err, &certErr), strings.Contains(err.Error(), "tls:"):
		return "no TLS handshake, as one asking for a client certificate does"
	case errors.As(err, &netErr) && netErr.Timeout():
		return fmt.Sprintf("nothing within %s", probeWait)
	}
	return "nothing: " + err.Error()
}
