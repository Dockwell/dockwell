// SPDX-License-Identifier: AGPL-3.0-only

package netguard

import (
	"context"
	"errors"
	"io"
	"math"
	"net"
	"net/http"
	"net/http/httptest"
	"net/netip"
	"strings"
	"sync/atomic"
	"testing"
	"time"
)

// What nothing may reach is refused whatever a caller allows; private networks and
// this server's own addresses only where a caller allows each; and the public
// internet always, however an address is written. Without TLS, not the internet:
// only what else a caller allows.
func TestWhatEachAddressIs(t *testing.T) {
	for _, tc := range []struct {
		host string
		// Why each refuses it, or "": the internet alone, private networks, private
		// networks and loopback, and those two alone, as for a connection without
		// TLS.
		public, private, loopback, local string
	}{
		{"203.0.113.10", "", "", "", whyInternet},
		{"2001:db8::10", "", "", "", whyInternet},
		{"example.com", "", "", "", ""},
		{"169.254.169.254", whyMetaOnLink, whyMetaOnLink, whyMetaOnLink, whyMetaOnLink},
		{"169.254.169.254:80", whyMetaOnLink, whyMetaOnLink, whyMetaOnLink, whyMetaOnLink},
		{"[::ffff:169.254.169.254]", whyMetaOnLink, whyMetaOnLink, whyMetaOnLink, whyMetaOnLink},
		{"64:ff9b::a9fe:a9fe", whyMetaOnLink, whyMetaOnLink, whyMetaOnLink, whyMetaOnLink},
		{"100.100.100.200", whyMetadata, whyMetadata, whyMetadata, whyMetadata},
		{"fd00:ec2::254", whyMetadata, whyMetadata, whyMetadata, whyMetadata},
		{"metadata.google.internal", whyMetadata, whyMetadata, whyMetadata, whyMetadata},
		{"Metadata.Google.Internal.", whyMetadata, whyMetadata, whyMetadata, whyMetadata},
		{"169.254.10.1", whyLinkLocal, whyLinkLocal, whyLinkLocal, whyLinkLocal},
		{"fe80::1", whyLinkLocal, whyLinkLocal, whyLinkLocal, whyLinkLocal},
		{"fe80::1%eth0", whyLinkLocal, whyLinkLocal, whyLinkLocal, whyLinkLocal},
		{"224.0.0.251", whyMulticast, whyMulticast, whyMulticast, whyMulticast},
		{"ff02::1", whyMulticast, whyMulticast, whyMulticast, whyMulticast},
		{"0.0.0.0", whyUnset, whyUnset, whyUnset, whyUnset},
		{"0.1.2.3", whyUnset, whyUnset, whyUnset, whyUnset},
		{"::", whyUnset, whyUnset, whyUnset, whyUnset},
		{"255.255.255.255", whyBroadcast, whyBroadcast, whyBroadcast, whyBroadcast},
		{"240.0.0.1", whyReserved, whyReserved, whyReserved, whyReserved},
		// Reserved, for a host with a tunnel up: IPv4-compatible, site-local, 6to4,
		// Teredo, local-use NAT64, the discard prefix and IETF assignments.
		{"::7f00:1", whyReserved, whyReserved, whyReserved, whyReserved},
		{"fec0::1", whyReserved, whyReserved, whyReserved, whyReserved},
		{"2002:7f00:1::1", whyReserved, whyReserved, whyReserved, whyReserved},
		{"2001:0:4136:e378::1", whyReserved, whyReserved, whyReserved, whyReserved},
		{"64:ff9b:1::a9fe:a9fe", whyReserved, whyReserved, whyReserved, whyReserved},
		{"100::1", whyReserved, whyReserved, whyReserved, whyReserved},
		{"192.0.0.170", whyReserved, whyReserved, whyReserved, whyReserved},
		{"127.0.0.1", whyLoopback, whyLoopback, "", ""},
		{"[::1]:80", whyLoopback, whyLoopback, "", ""},
		{"::1", whyLoopback, whyLoopback, "", ""},
		{"[::ffff:127.0.0.1]", whyLoopback, whyLoopback, "", ""},
		{"localhost", whyLoopback, whyLoopback, "", ""},
		{"api.localhost", whyLoopback, whyLoopback, "", ""},
		{"10.0.0.5", whyPrivate, "", "", ""},
		{"10.0.0.5:9000", whyPrivate, "", "", ""},
		{"172.16.3.4", whyPrivate, "", "", ""},
		{"192.168.1.20", whyPrivate, "", "", ""},
		{"fd12:3456::1", whyPrivate, "", "", ""},
		{"100.64.0.7", whyPrivate, "", "", ""},
		{"64:ff9b::a00:5", whyPrivate, "", "", ""},
	} {
		for _, c := range []struct {
			o    Options
			want string
		}{
			{Options{}, tc.public},
			{Options{Private: true}, tc.private},
			{Options{Private: true, Loopback: true}, tc.loopback},
			{Options{Local: true, Private: true, Loopback: true}, tc.local},
		} {
			if got := Forbidden(tc.host, c.o); got != c.want {
				t.Errorf("Forbidden(%s, %+v) = %q, want %q", tc.host, c.o, got, c.want)
			}
		}
	}
}

// get asks for url, and says only whether it could.
func get(c *HTTP, url string) error {
	req, err := http.NewRequestWithContext(context.Background(), http.MethodGet, url, http.NoBody)
	if err != nil {
		return err
	}
	resp, err := c.Do(req)
	if err == nil {
		_ = resp.Body.Close()
	}
	return err
}

// The check is on the address a connection is about to be made to, after a name
// resolves, so a name that leads somewhere refused gets nothing, as one whose DNS
// changed between a plan and a send would. An error names the URL by its scheme and
// host alone, as a path or a query can hold what signs in.
func TestANameIsCheckedWhereItLeads(t *testing.T) {
	var heard atomic.Int32
	srv := httptest.NewServer(http.HandlerFunc(func(http.ResponseWriter, *http.Request) { heard.Add(1) }))
	defer srv.Close()
	_, port, _ := net.SplitHostPort(srv.Listener.Addr().String())
	byName := "http://localhost:" + port + "/hook?token=s3cret"

	for _, o := range []Options{{}, {Private: true}} {
		err := get(Client(o), byName)
		var refused *Refused
		if !errors.Is(err, ErrForbidden) || !errors.As(err, &refused) || !refused.Addr.IsLoopback() || refused.Why != whyLoopback {
			t.Fatalf("a name for this server, %+v = %v, want it refused at the address it resolved to", o, err)
		}
		if strings.Contains(err.Error(), "s3cret") || strings.Contains(err.Error(), "/hook") {
			t.Errorf("an error names the URL whole: %v", err)
		}
		if w := Words(err); w != refused.Error() || !strings.Contains(w, "is this server's own address, so Dockwell sends nothing there") {
			t.Errorf("a refusal's words are %q", w)
		}
	}
	if heard.Load() != 0 {
		t.Errorf("the server heard %d requests it was refused", heard.Load())
	}

	if err := get(Client(Options{Private: true, Loopback: true}), byName); err != nil {
		t.Fatalf("with this server allowed, a name for it = %v", err)
	}
	// What nothing may reach stays refused with everything allowed.
	var refused *Refused
	if err := get(Client(Options{Private: true, Loopback: true}), "http://169.254.169.254/latest/meta-data/"); !errors.As(err, &refused) || refused.Why != whyMetaOnLink {
		t.Errorf("the metadata service = %v, want it refused", err)
	}
}

// Without TLS, the internet is refused, and loopback too unless a caller allows it,
// as a test of a channel doesn't.
func TestWithoutTLSOnlyWhatsAllowedNearby(t *testing.T) {
	for host, want := range map[string]string{
		"203.0.113.10": whyInternet,
		"127.0.0.1":    whyLoopback,
		"localhost":    whyLoopback,
		"10.0.0.5":     "",
	} {
		if got := Forbidden(host, Options{Local: true, Private: true}); got != want {
			t.Errorf("Forbidden(%s) without TLS, private allowed = %q, want %q", host, got, want)
		}
	}
	if got := Forbidden("10.0.0.5", Options{Local: true}); got != whyPrivate {
		t.Errorf("without TLS and nothing allowed, a private address = %q", got)
	}
}

// What isn't HTTP, such as SMTP, dials through the same check.
func TestADialerChecksToo(t *testing.T) {
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer ln.Close()
	go func() {
		for {
			c, err := ln.Accept()
			if err != nil {
				return
			}
			_ = c.Close()
		}
	}()
	if _, err := Dialer(Options{Private: true})(t.Context(), "tcp", ln.Addr().String()); !errors.Is(err, ErrForbidden) {
		t.Errorf("dialing this server = %v, want it refused", err)
	}
	c, err := Dialer(Options{Loopback: true})(t.Context(), "tcp", ln.Addr().String())
	if err != nil {
		t.Fatalf("dialing this server, allowed: %v", err)
	}
	_ = c.Close()
}

// A far end that moves says so in its answer, which is the caller's to read; the
// client goes nowhere it points.
func TestNoRedirectIsFollowed(t *testing.T) {
	var elsewhere atomic.Int32
	mux := http.NewServeMux()
	mux.HandleFunc("/moved", func(w http.ResponseWriter, r *http.Request) { http.Redirect(w, r, "/elsewhere", http.StatusFound) })
	mux.HandleFunc("/elsewhere", func(http.ResponseWriter, *http.Request) { elsewhere.Add(1) })
	srv := httptest.NewServer(mux)
	defer srv.Close()
	req, _ := http.NewRequestWithContext(t.Context(), http.MethodGet, srv.URL+"/moved", http.NoBody)
	resp, err := Client(Options{Loopback: true}).Do(req)
	if err != nil {
		t.Fatal(err)
	}
	_ = resp.Body.Close()
	if resp.StatusCode != http.StatusFound || elsewhere.Load() != 0 {
		t.Errorf("a redirect answered %d and was followed %d times", resp.StatusCode, elsewhere.Load())
	}
}

// An answer reads up to its cap, and one longer fails rather than being cut short,
// with nothing of what it said in the error. A cap as large as can be reads.
func TestAnAnswerIsCapped(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		n := map[string]int{"/fits": 1024, "/over": 1025}[r.URL.Path]
		_, _ = io.WriteString(w, strings.Repeat("s3cret-", 200)[:n])
	}))
	defer srv.Close()
	for _, c := range []struct {
		max   int64
		path  string
		wantN int
		over  bool
	}{
		{1024, "/fits", 1024, false},
		{1024, "/over", 1024, true},
		{math.MaxInt64, "/over", 1025, false},
	} {
		req, _ := http.NewRequestWithContext(t.Context(), http.MethodGet, srv.URL+c.path, http.NoBody)
		resp, err := Client(Options{Loopback: true, MaxBody: c.max}).Do(req)
		if err != nil {
			t.Fatal(err)
		}
		body, err := io.ReadAll(resp.Body)
		_ = resp.Body.Close()
		if len(body) != c.wantN || errors.Is(err, ErrTooLarge) != c.over || (err != nil && !c.over) || (err != nil && strings.Contains(err.Error(), "s3cret")) {
			t.Errorf("%s capped at %d read %d bytes and %v", c.path, c.max, len(body), err)
		}
	}
	if Client(Options{}).c.Transport.(*capped).limit != DefaultMaxBody {
		t.Errorf("an answer's cap unless a caller says isn't %d", DefaultMaxBody)
	}
}

// An answer's headers are capped too.
func TestAnAnswersHeadersAreCapped(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		for i := range 200 {
			w.Header().Add("X-Padding", strings.Repeat("x", 500)+string(rune('a'+i%26)))
		}
	}))
	defer srv.Close()
	if err := get(Client(Options{Loopback: true}), srv.URL); err == nil {
		t.Errorf("100 KB of headers were taken")
	}
}

// A far end that takes the connection and never answers is waited for Timeout,
// DefaultTimeout unless a caller says.
func TestAnAnswerIsWaitedForSoLong(t *testing.T) {
	never := make(chan struct{})
	srv := httptest.NewServer(http.HandlerFunc(func(http.ResponseWriter, *http.Request) { <-never }))
	defer srv.Close()
	defer close(never)
	start := time.Now()
	err := get(Client(Options{Loopback: true, Timeout: 300 * time.Millisecond}), srv.URL)
	if err == nil || time.Since(start) > 5*time.Second || Words(err) != "no answer in time" {
		t.Errorf("a far end that never answers = %v after %v, words %q", err, time.Since(start), Words(err))
	}
	c := Client(Options{}).c
	if c.Timeout != DefaultTimeout || c.Transport.(*capped).next.(*http.Transport).ResponseHeaderTimeout != DefaultTimeout {
		t.Errorf("unless a caller says, a request waits %v", c.Timeout)
	}
}

// A proxy in the environment is no way past the check, which would see the proxy
// alone. net/http never proxies loopback, so this reads the transport: no proxy at
// all, whatever the environment says.
func TestNoProxyFromTheEnvironment(t *testing.T) {
	t.Setenv("HTTP_PROXY", "http://203.0.113.9:3128")
	t.Setenv("HTTPS_PROXY", "http://203.0.113.9:3128")
	tr, ok := Client(Options{}).c.Transport.(*capped).next.(*http.Transport)
	if !ok || tr.Proxy != nil {
		t.Fatalf("the client's transport takes a proxy from the environment")
	}
	if tr.TLSClientConfig.MinVersion < 0x0303 || tr.MaxResponseHeaderBytes != maxHeaderBytes {
		t.Errorf("the client takes TLS older than 1.2, or headers past %d bytes", maxHeaderBytes)
	}
}

// A refusal's words name the address it led to, never what was asked for, which
// may hold a token; Words says each error a person might see in a few words.
func TestARefusalNamesTheAddressAlone(t *testing.T) {
	r := &Refused{Addr: netip.MustParseAddr("10.0.0.5"), Why: whyPrivate}
	if r.Error() != "10.0.0.5 is an address on a private network, so Dockwell sends nothing there" || !errors.Is(r, ErrForbidden) {
		t.Errorf("a refusal says %q", r.Error())
	}
	_, err := Dialer(Options{Loopback: true})(t.Context(), "tcp", "127.0.0.1:1")
	if Words(err) != "connection refused" {
		t.Errorf("nothing listening reads %q", Words(err))
	}
	if Words(ErrTooLarge) != "the answer is longer than Dockwell reads" || Words(errors.New("x")) != "it couldn't be reached" || Words(nil) != "" {
		t.Errorf("Words reads %q, %q, %q", Words(ErrTooLarge), Words(errors.New("x")), Words(nil))
	}
}
