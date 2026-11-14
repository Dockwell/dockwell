// SPDX-License-Identifier: AGPL-3.0-only

package panels

import (
	"context"
	"crypto/tls"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
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
// HTTPS with its name as SNI, for / and each path a rule names for it. What a
// login or an address limit answers holds it back: 401, 403 or 407, a redirect to
// another host or to a login path, or a TLS handshake that fails, as one asking
// for a client certificate does. Nothing the proxy answers is followed.

// proxyAddr is where the old proxy listens for HTTPS on the server.
const proxyAddr = "127.0.0.1:443"

// probeWait is how long one request may take, and probeWorkers how many run at
// once.
const (
	probeWait    = 5 * time.Second
	probeWorkers = 8
)

// probeAsk is one request: a path of a domain, for the check at index check.
type probeAsk struct {
	check  int
	domain string
	path   string
}

// loginPath is a path a redirect to a sign-in goes to: login, sign-in, auth, SSO,
// OAuth, SAML, CAS and the paths the common forward-auth servers answer on.
var loginPath = sync.OnceValue(func() *regexp.Regexp {
	return regexp.MustCompile(`(?i)(^|[/._-])(log-?in|sign-?in|sign_in|auth|authorize|oauth2?|sso|saml|cas|session|outpost\.goauthentik\.io|cdn-cgi/access)([/._?-]|$)`)
})

// probeAll asks for each in parallel, a few at a time, each in order.
func (r Reader) probeAll(ctx context.Context, asks []probeAsk) []resource.PanelProbe {
	out := make([]resource.PanelProbe, len(asks))
	next := make(chan int)
	var wg sync.WaitGroup
	for range min(probeWorkers, len(asks)) {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for i := range next {
				out[i] = r.probe(ctx, asks[i].domain, asks[i].path)
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

// probe asks the proxy for a domain's path, and says what answered.
func (r Reader) probe(ctx context.Context, domain, p string) resource.PanelProbe {
	out := resource.PanelProbe{Path: p}
	addr := r.ProbeAddr
	if addr == "" {
		addr = proxyAddr
	}
	dialer := &net.Dialer{Timeout: probeWait}
	client := &http.Client{
		Timeout: probeWait,
		Transport: &http.Transport{
			// Every request goes to the proxy on this server, whatever the domain's DNS
			// says: it's the proxy's answer that's asked for.
			DialContext: func(ctx context.Context, network, _ string) (net.Conn, error) {
				return dialer.DialContext(ctx, network, addr)
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
	switch {
	case resp.StatusCode == http.StatusUnauthorized, resp.StatusCode == http.StatusForbidden, resp.StatusCode == http.StatusProxyAuthRequired:
		out.Answer = code
	case resp.StatusCode >= 300 && resp.StatusCode < 400:
		loc, err := resp.Location()
		switch {
		case err != nil:
			out.Answer = code + " to nowhere Dockwell can read"
		case !strings.EqualFold(loc.Hostname(), domain):
			out.Answer = fmt.Sprintf("%s to another host, %s", code, loc.Hostname())
		case loginPath().MatchString(loc.Path):
			out.Answer = fmt.Sprintf("%s to a sign-in, %s", code, loc.Path)
		default:
			out.Answer, out.Open = fmt.Sprintf("%s to %s", code, loc.Path), true
		}
	default:
		out.Answer, out.Open = code, true
	}
	return out
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
