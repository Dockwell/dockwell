// SPDX-License-Identifier: AGPL-3.0-only

// Package netguard is the one guarded way to an address a person names, such as a
// webhook, a chat channel's server, a catalog source or a task's request (#341). It
// refuses what such a request must never reach: link-local, multicast, unspecified,
// broadcast and reserved addresses, and where clouds keep their metadata services,
// and, unless a caller allows them, this server's own addresses and its private
// networks. It checks the address a connection is about to be made to, after any
// name is resolved, so DNS that answers one thing when a plan is made and another
// when a request is sent gets nothing. It takes no proxy from the environment,
// through which the check would see the proxy alone, follows no redirect, bounds
// how long an answer takes, and caps what an answer reads.
//
// It knows NAT64's well-known prefix, 64:ff9b::/96, and checks the address one
// carries. A network-specific NAT64 prefix can't be known without asking the network
// (RFC 7050), so an address under one is checked as the IPv6 address it is.
//
// A test that needs a server on loopback passes its own client to what it tests,
// rather than the guard making an exception.
package netguard

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
	"strings"
	"syscall"
	"time"
)

// Options say what a caller allows beyond the public internet. The zero Options
// allow the internet alone, with DefaultMaxBody and DefaultTimeout.
type Options struct {
	// Private allows the private networks a server is on: RFC 1918, unique local
	// IPv6 and carrier-grade NAT, for a caller where an admin named one, such as a
	// webhook on the install's own network. What nothing may reach stays refused
	// whatever it says.
	Private bool
	// Loopback allows this server's own addresses as well, and names under
	// localhost, for a caller where an admin named one.
	Loopback bool
	// Local refuses the internet, for a connection without TLS, which mustn't
	// cross it whatever a name led to: what's left is what Private and Loopback
	// allow.
	Local bool
	// MaxBody caps what an answer's body reads, in bytes: past it, reading fails
	// with ErrTooLarge. 0 is DefaultMaxBody.
	MaxBody int64
	// Timeout bounds waiting for an answer's headers, and the whole request, body
	// and all. 0 is DefaultTimeout.
	Timeout time.Duration
}

const (
	// DefaultMaxBody is what an answer reads unless the caller says.
	DefaultMaxBody = 1 << 20
	// DefaultTimeout is how long a request takes at most unless the caller says.
	DefaultTimeout = 30 * time.Second
	// DialTimeout bounds connecting, and the TLS handshake.
	DialTimeout = 10 * time.Second
	// maxHeaderBytes caps an answer's headers.
	maxHeaderBytes = 64 << 10
)

var (
	// ErrForbidden is a connection refused for where it led: errors.Is finds it in
	// what a client or dialer returns. Its words are a *Refused's.
	ErrForbidden = errors.New("the address is one Dockwell sends nothing to")
	// ErrTooLarge is an answer longer than the client reads.
	ErrTooLarge = errors.New("the answer is longer than Dockwell reads")
)

// Refused is a connection refused for where it led, with why in words.
type Refused struct {
	Addr netip.Addr
	// Why is what the address is, such as "a link-local address".
	Why string
}

func (r *Refused) Error() string {
	return fmt.Sprintf("%s is %s, so Dockwell sends nothing there", r.Addr, r.Why)
}

// Is makes a Refused ErrForbidden.
func (r *Refused) Is(target error) bool { return target == ErrForbidden }

// Words that say what an address is.
const (
	whyMetadata   = "where a cloud keeps its metadata service"
	whyLinkLocal  = "a link-local address"
	whyMulticast  = "a multicast address"
	whyUnset      = "an unspecified address"
	whyBroadcast  = "a broadcast address"
	whyReserved   = "a reserved address"
	whyLoopback   = "this server's own address"
	whyPrivate    = "an address on a private network"
	whyMetaOnLink = "a link-local address, where a cloud keeps its metadata service"
	whyInternet   = "on the internet, where nothing goes without TLS"
)

// metadataNames are the names clouds give their metadata services.
var metadataNames = []string{"metadata.google.internal", "metadata.goog", "metadata"}

// metadata are the addresses clouds keep their metadata services at: AWS's, GCP's,
// Azure's and most others' link-local one, AWS's over IPv6, and Alibaba Cloud's.
var metadata = map[netip.Addr]string{
	netip.MustParseAddr("169.254.169.254"): whyMetaOnLink,
	netip.MustParseAddr("fd00:ec2::254"):   whyMetadata,
	netip.MustParseAddr("100.100.100.200"): whyMetadata,
}

var (
	cgnat     = netip.MustParsePrefix("100.64.0.0/10")
	thisNet   = netip.MustParsePrefix("0.0.0.0/8")
	broadcast = netip.MustParseAddr("255.255.255.255")
	// nat64 carries an IPv4 address in its last 32 bits, which a gateway reaches.
	nat64 = netip.MustParsePrefix("64:ff9b::/96")
	// reserved are ranges no request a person names has any business with, which a
	// host with a tunnel up could reach: IPv4-compatible IPv6, site-local, 6to4,
	// Teredo, local-use NAT64, the discard prefix, IETF assignments, and class E.
	reserved = []netip.Prefix{
		netip.MustParsePrefix("::/96"),
		netip.MustParsePrefix("fec0::/10"),
		netip.MustParsePrefix("2002::/16"),
		netip.MustParsePrefix("2001::/32"),
		netip.MustParsePrefix("64:ff9b:1::/48"),
		netip.MustParsePrefix("100::/64"),
		netip.MustParsePrefix("192.0.0.0/24"),
		netip.MustParsePrefix("240.0.0.0/4"),
	}
)

// Forbidden says why options refuse a host, an address or a name, with or without
// a port, or "" when they allow it, for a check when a plan is made. A name is
// refused here only as a metadata service's, or under localhost where this server's
// own addresses aren't allowed; what it resolves to is checked as it's dialed.
func Forbidden(host string, o Options) string {
	if h, _, err := net.SplitHostPort(host); err == nil {
		host = h
	}
	host = strings.TrimSuffix(strings.ToLower(strings.Trim(host, "[]")), ".")
	if addr, err := netip.ParseAddr(host); err == nil {
		return refuses(addr, o)
	}
	for _, name := range metadataNames {
		if host == name {
			return whyMetadata
		}
	}
	if (host == "localhost" || strings.HasSuffix(host, ".localhost")) && !o.Loopback {
		return whyLoopback
	}
	return ""
}

// refuses says why options refuse an address, or "".
func refuses(addr netip.Addr, o Options) string {
	addr = addr.Unmap().WithZone("")
	// A NAT64 address is what it carries, which its gateway reaches.
	if nat64.Contains(addr) {
		b := addr.As16()
		addr = netip.AddrFrom4([4]byte{b[12], b[13], b[14], b[15]})
	}
	if why, ok := metadata[addr]; ok {
		return why
	}
	switch {
	case addr.IsLinkLocalUnicast():
		return whyLinkLocal
	case addr.IsMulticast():
		return whyMulticast
	case addr.IsUnspecified(), thisNet.Contains(addr):
		return whyUnset
	case addr == broadcast:
		return whyBroadcast
	case !addr.IsLoopback() && reservedAddr(addr):
		return whyReserved
	}
	private := addr.IsPrivate() || cgnat.Contains(addr)
	switch {
	case o.Local && !addr.IsLoopback() && !private:
		return whyInternet
	case addr.IsLoopback() && !o.Loopback:
		return whyLoopback
	case private && !o.Private:
		return whyPrivate
	}
	return ""
}

func reservedAddr(addr netip.Addr) bool {
	for _, p := range reserved {
		if p.Contains(addr) {
			return true
		}
	}
	return false
}

// DialFunc connects as a net.Dialer's DialContext does, through the guard.
type DialFunc func(ctx context.Context, network, address string) (net.Conn, error)

// Dialer connects only where options allow, checked on the address it's about to
// connect to, whatever a name resolved to: for what isn't HTTP, such as SMTP. It's
// a func, so nothing can take the check off it.
func Dialer(o Options) DialFunc {
	d := &net.Dialer{
		Timeout: DialTimeout,
		Control: func(_, address string, _ syscall.RawConn) error {
			host, _, err := net.SplitHostPort(address)
			if err != nil {
				return err
			}
			addr, err := netip.ParseAddr(host)
			if err != nil {
				return fmt.Errorf("%s isn't an address to connect to: %w", host, ErrForbidden)
			}
			if why := refuses(addr, o); why != "" {
				return &Refused{Addr: addr.Unmap().WithZone(""), Why: why}
			}
			return nil
		},
	}
	return d.DialContext
}

// HTTP is a client for addresses people name. Its errors name a URL by its scheme
// and host alone, as a webhook's path or query can hold what signs in to it.
type HTTP struct {
	c *http.Client
}

// Client is an HTTP client for addresses people name: it dials as Dialer does,
// takes no proxy from the environment, verifies certificates with TLS 1.2 or later,
// follows no redirect, as a far end that moves says so in its answer, waits Timeout
// at most, reads 64 KiB of headers and MaxBody of a body at most, and its errors
// hold no byte of an answer. Say an error with Words.
//
// It asks for an answer compressed and reads it decompressed, so MaxBody caps what
// it decompresses to. A caller that sets Accept-Encoding itself gets the compressed
// bytes, which MaxBody caps as they come, and must cap its own decompression.
func Client(o Options) *HTTP {
	limit := o.MaxBody
	if limit <= 0 {
		limit = DefaultMaxBody
	}
	timeout := o.Timeout
	if timeout <= 0 {
		timeout = DefaultTimeout
	}
	return &HTTP{c: &http.Client{
		Timeout: timeout,
		Transport: &capped{
			limit: limit,
			next: &http.Transport{
				DialContext:            Dialer(o),
				TLSClientConfig:        &tls.Config{MinVersion: tls.VersionTLS12},
				TLSHandshakeTimeout:    DialTimeout,
				ResponseHeaderTimeout:  timeout,
				MaxResponseHeaderBytes: maxHeaderBytes,
				DisableKeepAlives:      true,
				// No proxy from the environment: through one, the dial check would
				// see the proxy alone, and the name could lead anywhere.
				Proxy: nil,
			},
		},
		CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse },
	}}
}

// Do sends a request, as http.Client's Do does.
func (h *HTTP) Do(req *http.Request) (*http.Response, error) {
	resp, err := h.c.Do(req)
	var u *url.Error
	if errors.As(err, &u) {
		u.URL = req.URL.Scheme + "://" + req.URL.Host
	}
	return resp, err
}

// Words say why a request or a connection didn't go, in a few words a person can
// read and a log can keep: never a URL, which can hold what signs in, and nothing
// of an answer.
func Words(err error) string {
	var refused *Refused
	var dns *net.DNSError
	var cert *tls.CertificateVerificationError
	var op *net.OpError
	switch {
	case err == nil:
		return ""
	case errors.As(err, &refused):
		return refused.Error()
	case errors.Is(err, ErrTooLarge):
		return ErrTooLarge.Error()
	case errors.As(err, &dns) && dns.IsNotFound:
		return "no such host: " + dns.Name
	case errors.Is(err, syscall.ECONNREFUSED):
		return "connection refused"
	case errors.Is(err, context.DeadlineExceeded), errors.Is(err, os.ErrDeadlineExceeded):
		return "no answer in time"
	case errors.As(err, &cert):
		return "its certificate isn't valid: " + cert.Err.Error()
	case errors.As(err, &op) && op.Err != nil:
		return op.Err.Error()
	}
	return "it couldn't be reached"
}

// capped is a transport whose answers read at most limit bytes.
type capped struct {
	next  http.RoundTripper
	limit int64
}

func (c *capped) RoundTrip(req *http.Request) (*http.Response, error) {
	resp, err := c.next.RoundTrip(req)
	if err != nil {
		return nil, err
	}
	resp.Body = &cappedBody{ReadCloser: resp.Body, left: c.limit}
	return resp, nil
}

// cappedBody fails once more than its limit has come, rather than ending early, so
// a caller never takes part of an answer for all of it.
type cappedBody struct {
	io.ReadCloser
	left int64
}

func (b *cappedBody) Read(p []byte) (int, error) {
	if b.left < 0 {
		return 0, ErrTooLarge
	}
	// One byte past the limit says there's more; compared so a limit near the
	// largest int64 doesn't overflow.
	if int64(len(p))-1 > b.left {
		p = p[:b.left+1]
	}
	n, err := b.ReadCloser.Read(p)
	b.left -= int64(n)
	if b.left < 0 {
		return n + int(b.left), ErrTooLarge
	}
	return n, err
}
