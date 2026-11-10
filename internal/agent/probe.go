// SPDX-License-Identifier: AGPL-3.0-only

package agent

import (
	"context"
	"crypto/tls"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/netip"
	"net/url"
	"os"
	"strconv"
	"syscall"
	"time"

	agentv1 "github.com/dockwell/dockwell/gen/dockwell/agent/v1"
)

// ProbeTimeout is how long a probe waits for an answer: what a visitor would wait,
// and no more.
const ProbeTimeout = 5 * time.Second

// errLinkLocal refuses an address where a cloud keeps its metadata service.
var errLinkLocal = errors.New("that's a link-local address, where a cloud keeps its metadata service, so Dockwell doesn't ask it")

// probeDialer dials as a probe does: to no link-local address, whatever a name
// resolves to.
var probeDialer = &net.Dialer{
	Timeout: ProbeTimeout,
	Control: func(_, address string, _ syscall.RawConn) error {
		host, _, err := net.SplitHostPort(address)
		if err != nil {
			return err
		}
		if ip, err := netip.ParseAddr(host); err == nil && (ip.Unmap().IsLinkLocalUnicast() || ip.Unmap().IsLinkLocalMulticast()) {
			return errLinkLocal
		}
		return nil
	},
}

// Probe asks whether what a website sends to answers, from this server: a port as
// the ingress dials it, or a URL as a website's web server sends to it. It reads
// nothing of what answers but its status line.
func Probe(ctx context.Context, call *agentv1.ProbeCall) *agentv1.ProbeResult {
	ctx, cancel := context.WithTimeout(ctx, ProbeTimeout)
	defer cancel()
	if port := call.GetPort(); port != 0 {
		conn, err := probeDialer.DialContext(ctx, "tcp", net.JoinHostPort("127.0.0.1", strconv.Itoa(int(port))))
		if err != nil {
			if errors.Is(err, syscall.ECONNREFUSED) {
				return &agentv1.ProbeResult{Said: fmt.Sprintf("nothing listens on port %d", port)}
			}
			return &agentv1.ProbeResult{Said: probeWords(err)}
		}
		_ = conn.Close()
		return &agentv1.ProbeResult{Answered: true, Said: fmt.Sprintf("something listens on port %d", port)}
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, call.GetUrl(), http.NoBody)
	if err != nil || (req.URL.Scheme != "http" && req.URL.Scheme != "https") {
		return &agentv1.ProbeResult{Said: "that isn't a URL to ask"}
	}
	req.Header.Set("User-Agent", "dockwell-probe")
	client := &http.Client{
		Transport: &http.Transport{
			DialContext:         probeDialer.DialContext,
			TLSClientConfig:     &tls.Config{MinVersion: tls.VersionTLS12},
			TLSHandshakeTimeout: ProbeTimeout,
			DisableKeepAlives:   true,
		},
		CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse },
	}
	resp, err := client.Do(req)
	if err != nil {
		return &agentv1.ProbeResult{Said: probeWords(err)}
	}
	_, _ = io.CopyN(io.Discard, resp.Body, 4096)
	_ = resp.Body.Close()
	return &agentv1.ProbeResult{Answered: resp.StatusCode < http.StatusInternalServerError, Said: resp.Status}
}

// probeWords says why a probe found nothing, in a few words.
func probeWords(err error) string {
	var dns *net.DNSError
	switch {
	case errors.Is(err, errLinkLocal):
		return errLinkLocal.Error()
	case errors.As(err, &dns) && dns.IsNotFound:
		return "no such host: " + dns.Name
	case errors.Is(err, syscall.ECONNREFUSED):
		return "connection refused"
	case errors.Is(err, context.DeadlineExceeded), errors.Is(err, os.ErrDeadlineExceeded):
		return fmt.Sprintf("no answer in %d seconds", int(ProbeTimeout/time.Second))
	}
	var cert *tls.CertificateVerificationError
	if errors.As(err, &cert) {
		return "its certificate isn't valid: " + cert.Err.Error()
	}
	var op *net.OpError
	if errors.As(err, &op) && op.Err != nil {
		return op.Err.Error()
	}
	return err.Error()
}

// LoopbackProbe is Probe for a server that asks nothing beyond itself, as under a
// test: a port on its loopback, and no URL elsewhere, as its name would be looked
// up. What it doesn't ask, it answers with nothing, which says nothing either way.
func LoopbackProbe(ctx context.Context, call *agentv1.ProbeCall) *agentv1.ProbeResult {
	if call.GetUrl() != "" {
		u, err := url.Parse(call.GetUrl())
		if err != nil {
			return nil
		}
		if ip, err := netip.ParseAddr(u.Hostname()); err != nil || !ip.IsLoopback() {
			return nil
		}
	}
	return Probe(ctx, call)
}
