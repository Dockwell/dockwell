// SPDX-License-Identifier: AGPL-3.0-only

package checks

import (
	"context"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/tls"
	"crypto/x509"
	"crypto/x509/pkix"
	"fmt"
	"io"
	"math/big"
	"net"
	"net/http"
	"net/http/httptest"
	"slices"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"
	"unicode/utf8"

	"google.golang.org/protobuf/proto"

	resourcev1 "github.com/dockwell/dockwell/gen/dockwell/resource/v1alpha1"
	dockwellv1 "github.com/dockwell/dockwell/gen/dockwell/v1"
	"github.com/dockwell/dockwell/internal/resource/alert"
)

// site answers each path as a check meets addresses: a page, a redirect, an error,
// and one that takes its time.
func site(t *testing.T) *httptest.Server {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/":
			fmt.Fprint(w, "<h1>Welcome to the shop</h1>")
		case "/moved":
			// Followed, this would read as down: a check takes the redirect itself as up.
			http.Redirect(w, r, "/broken", http.StatusFound)
		case "/away":
			http.Redirect(w, r, "https://elsewhere.example.com/", http.StatusFound)
		case "/broken":
			w.WriteHeader(http.StatusBadGateway)
		case "/slow":
			time.Sleep(1500 * time.Millisecond)
		default:
			http.NotFound(w, r)
		}
	}))
	t.Cleanup(srv.Close)
	return srv
}

// unfollowing is a test site's client, which follows no redirect, as netguard's doesn't.
func unfollowing(srv *httptest.Server) *http.Client {
	c := srv.Client()
	c.CheckRedirect = func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }
	return c
}

// checkOf fills in a check's defaults as a plan does. The test's site is on this
// machine's loopback, which a plan refuses, so its address stands in as a name.
func checkOf(t *testing.T, c *resourcev1.AlertCheck) *resourcev1.AlertCheck {
	t.Helper()
	address := c.GetAddress()
	c.Address = "https://shop.example.com/"
	if err := (alert.Check{}).Validate(c); err != nil {
		t.Fatalf("Validate(%v): %v", c, err)
	}
	c.Address = address
	return c
}

func TestARunSaysWhetherAnAddressAnswersAsItShould(t *testing.T) {
	srv := site(t)
	for _, tc := range []struct {
		name  string
		check *resourcev1.AlertCheck
		ok    bool
		said  string
	}{
		{"a page", &resourcev1.AlertCheck{Address: srv.URL + "/"}, true, ""},
		{"a redirect, not followed", &resourcev1.AlertCheck{Address: srv.URL + "/moved"}, true, ""},
		{"an error", &resourcev1.AlertCheck{Address: srv.URL + "/broken"}, false, "502, outside 200 to 399"},
		{"a page that isn't there", &resourcev1.AlertCheck{Address: srv.URL + "/gone"}, false, "404, outside 200 to 399"},
		{"a keyword it holds", &resourcev1.AlertCheck{Address: srv.URL + "/", Keyword: "Welcome"}, true, ""},
		{"a keyword it lacks", &resourcev1.AlertCheck{Address: srv.URL + "/", Keyword: "Sold out"}, false, `it doesn't hold "Sold out"`},
		{"a keyword that must be absent", &resourcev1.AlertCheck{Address: srv.URL + "/", Keyword: "Welcome", KeywordAbsent: true}, false, `it holds "Welcome"`},
		{"no answer in time", &resourcev1.AlertCheck{Address: srv.URL + "/slow", Timeout: "1s"}, false, "no answer within 1s"},
		{"a keyword where it moved", &resourcev1.AlertCheck{Address: srv.URL + "/away", Keyword: "Welcome"}, false, "it moved to elsewhere.example.com"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			run := Probe(t.Context(), unfollowing(srv), checkOf(t, tc.check), nil)
			if run.GetOk() != tc.ok || run.GetSaid() != tc.said {
				t.Errorf("ok %t, said %q; want %t, %q", run.GetOk(), run.GetSaid(), tc.ok, tc.said)
			}
			if run.GetTime() == nil || run.GetTook() == nil && tc.ok {
				t.Errorf("a run without its time: %v", run)
			}
		})
	}
}

// From the control plane, a check reaches what an alert channel's test reaches,
// never the control server's own loopback: what listens there isn't for a check to
// read, and its answer would say whether something does.
func TestTheGuardKeepsAChecksRunOffThisServersLoopback(t *testing.T) {
	asked := 0
	srv := httptest.NewServer(http.HandlerFunc(func(http.ResponseWriter, *http.Request) { asked++ }))
	t.Cleanup(srv.Close)
	run := Probe(t.Context(), Guard(true), checkOf(t, &resourcev1.AlertCheck{Address: srv.URL + "/"}), nil)
	if run.GetOk() || run.GetSaid() == "" || strings.Contains(run.GetSaid(), srv.URL) {
		t.Errorf("a check of this server's loopback read %v", run)
	}
	if asked != 0 {
		t.Errorf("the loopback was asked %d times", asked)
	}
}

// privateAddr is one of this machine's addresses on a private network, or the test
// is skipped on a machine without one.
func privateAddr(t *testing.T) string {
	t.Helper()
	addrs, err := net.InterfaceAddrs()
	if err != nil {
		t.Skip("this machine's addresses can't be read:", err)
	}
	for _, a := range addrs {
		if n, ok := a.(*net.IPNet); ok && n.IP.IsPrivate() && n.IP.To4() != nil {
			return n.IP.String()
		}
	}
	t.Skip("this machine has no private network address")
	return ""
}

// Unless a check says it's inside the network, it reaches the internet alone, so
// an address on a private network reads failing, says why and how to reach it, and
// nothing is sent there: netguard refuses it before it dials. Inside the network,
// the same address answers.
func TestACheckReachesThePrivateNetworkOnlyFromInside(t *testing.T) {
	ln, err := net.Listen("tcp", net.JoinHostPort(privateAddr(t), "0"))
	if err != nil {
		t.Skip("can't listen on this machine's private address:", err)
	}
	var asked atomic.Int32
	srv := httptest.NewUnstartedServer(http.HandlerFunc(func(http.ResponseWriter, *http.Request) { asked.Add(1) }))
	srv.Listener.Close()
	srv.Listener = ln
	srv.Start()
	t.Cleanup(srv.Close)
	c := &resourcev1.AlertCheck{Address: srv.URL + "/", Timeout: "2s", ExpectFrom: 200, ExpectTo: 399}
	run := Probe(t.Context(), Guard(false), c, nil)
	if run.GetOk() || !strings.Contains(run.GetSaid(), "private network") || !strings.HasSuffix(run.GetSaid(), "Turn on Inside the network for this check to reach it") {
		t.Errorf("a check of a private address from outside read %v", run)
	}
	if asked.Load() != 0 {
		t.Fatalf("a check from outside reached the private network %d times", asked.Load())
	}
	c.InsideNetwork = true
	if run := Probe(t.Context(), Guard(true), c, nil); !run.GetOk() || asked.Load() != 1 {
		t.Errorf("a check inside the network read %v, asked %d times", run, asked.Load())
	}
}

// asker answers every request as up, and keeps the hosts it was asked.
type asker struct {
	mu    sync.Mutex
	hosts []string
}

func (a *asker) Do(req *http.Request) (*http.Response, error) {
	a.mu.Lock()
	a.hosts = append(a.hosts, req.URL.Hostname())
	a.mu.Unlock()
	return &http.Response{StatusCode: http.StatusOK, Body: io.NopCloser(strings.NewReader("")), Request: req}, nil
}

// A check reaches what it says through the client for it: the internet's unless
// it's inside the network (ADR 0066).
func TestEachCheckRunsThroughItsOwnClient(t *testing.T) {
	internet, inside := &asker{}, &asker{}
	r := &Runner{Client: internet, Inside: inside, Now: time.Now}
	r.init()
	r.checks["chk_out"] = &tracked{Status: Status{ID: "chk_out", Spec: checkOf(t, &resourcev1.AlertCheck{Address: "https://shop.example.com/"})}}
	r.checks["chk_in"] = &tracked{Status: Status{ID: "chk_in", Spec: checkOf(t, &resourcev1.AlertCheck{Address: "http://intranet.example.com/", InsideNetwork: true})}}
	for _, id := range []string{"chk_out", "chk_in"} {
		if st, ok := r.RunNow(t.Context(), id); !ok || !st.Runs[0].GetOk() {
			t.Fatalf("%s didn't run: %+v", id, st)
		}
	}
	if !slices.Equal(internet.hosts, []string{"shop.example.com"}) || !slices.Equal(inside.hosts, []string{"intranet.example.com"}) {
		t.Errorf("the internet's client asked %q, the network's %q", internet.hosts, inside.hosts)
	}
}

// What the far end controls, as the names its certificate holds and where a
// redirect leads, can't make a run large: a run keeps a few hundred bytes of
// words, cut on a character.
func TestARunKeepsAFewHundredBytesOfWhatTheFarEndSays(t *testing.T) {
	long := strings.Repeat(strings.Repeat("a", 60)+".", 40) + "example"
	names := make([]string, 99)
	for i := range names {
		names[i] = long
	}
	for _, tc := range []struct {
		name  string
		names []string
		said  string
	}{
		{"a certificate for other names", names, "its certificate isn't valid for shop.example.com"},
		{"a certificate no authority signed", []string{"shop.example.com"}, "its certificate isn't signed by an authority the control server trusts"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			srv := httptest.NewUnstartedServer(http.HandlerFunc(func(http.ResponseWriter, *http.Request) {}))
			srv.TLS = &tls.Config{Certificates: []tls.Certificate{selfSigned(t, tc.names)}}
			srv.StartTLS()
			t.Cleanup(srv.Close)
			to := srv.Listener.Addr().String()
			var d net.Dialer
			client := &http.Client{Transport: &http.Transport{DialContext: func(ctx context.Context, network, _ string) (net.Conn, error) {
				return d.DialContext(ctx, network, to)
			}}}
			run := Probe(t.Context(), client, checkOf(t, &resourcev1.AlertCheck{Address: "https://shop.example.com/"}), nil)
			if run.GetOk() || run.GetSaid() != tc.said {
				t.Errorf("said %d bytes: %.200q; want %q", len(run.GetSaid()), run.GetSaid(), tc.said)
			}
		})
	}
	t.Run("a redirect to a long name", func(t *testing.T) {
		host := strings.Repeat("b", 60000) + ".example"
		srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
			w.Header().Set("Location", "https://"+host+"/")
			w.WriteHeader(http.StatusFound)
		}))
		t.Cleanup(srv.Close)
		run := Probe(t.Context(), unfollowing(srv), checkOf(t, &resourcev1.AlertCheck{Address: srv.URL + "/", Keyword: "Welcome"}), nil)
		if said := run.GetSaid(); len(said) > MaxSaid || !strings.HasPrefix(said, "it moved to bbb") || !strings.HasSuffix(said, "…") {
			t.Errorf("said %d bytes: %.200q", len(said), said)
		}
	})
	t.Run("a redirect to a host that isn't UTF-8", func(t *testing.T) {
		srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
			w.Header().Set("Location", "https://%ff%fe.example/")
			w.WriteHeader(http.StatusFound)
		}))
		t.Cleanup(srv.Close)
		run := Probe(t.Context(), unfollowing(srv), checkOf(t, &resourcev1.AlertCheck{Address: srv.URL + "/", Keyword: "Welcome"}), nil)
		if said := run.GetSaid(); !utf8.ValidString(said) || !strings.HasPrefix(said, "it moved to ") {
			t.Errorf("said %q", said)
		}
		if _, err := proto.Marshal(run); err != nil {
			t.Errorf("a run with those words can't be sent: %v", err)
		}
	})
	t.Run("words cut on a character", func(t *testing.T) {
		said := clip(strings.Repeat("é", 400), MaxSaid)
		if len(said) > MaxSaid || !utf8.ValidString(said) || !strings.HasSuffix(said, "é…") {
			t.Errorf("clipped to %d bytes: %q", len(said), said)
		}
		if said := clip("short", MaxSaid); said != "short" {
			t.Errorf("clip cut what fits: %q", said)
		}
	})
}

func selfSigned(t *testing.T, names []string) tls.Certificate {
	t.Helper()
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	tmpl := &x509.Certificate{
		SerialNumber: big.NewInt(1), Subject: pkix.Name{CommonName: "a test site"},
		NotBefore: time.Now().Add(-time.Hour), NotAfter: time.Now().Add(time.Hour), DNSNames: names,
	}
	der, err := x509.CreateCertificate(rand.Reader, tmpl, tmpl, &key.PublicKey, key)
	if err != nil {
		t.Fatal(err)
	}
	return tls.Certificate{Certificate: [][]byte{der}, PrivateKey: key}
}

// A run cut short as the control plane stops says nothing of the address: the
// check keeps how it stood.
func TestARunCutShortByAStopIsDropped(t *testing.T) {
	srv := site(t)
	r := &Runner{Client: unfollowing(srv), Now: time.Now}
	r.init()
	r.checks["chk_1"] = &tracked{Status: Status{ID: "chk_1", Spec: checkOf(t, &resourcev1.AlertCheck{Address: srv.URL + "/slow", Timeout: "5s"}), State: dockwellv1.AlertCheckState_ALERT_CHECK_STATE_UP}}
	ctx, cancel := context.WithTimeout(t.Context(), 200*time.Millisecond)
	defer cancel()
	st, _ := r.RunNow(ctx, "chk_1")
	if len(st.Runs) != 0 || st.State != dockwellv1.AlertCheckState_ALERT_CHECK_STATE_UP {
		t.Errorf("a run cut short by a stop stands as %+v", st)
	}
}

// The runner never waits for a slot: with every slot taken, a check that's due
// is skipped at once, says so, and keeps how it stood.
func TestARunThatCantStartIsSkippedNotWaitedFor(t *testing.T) {
	r := &Runner{Now: time.Now}
	r.init()
	for range Slots {
		r.slots <- struct{}{}
	}
	spec := checkOf(t, &resourcev1.AlertCheck{Address: "https://shop.example.com/"})
	r.checks["chk_1"] = &tracked{Status: Status{ID: "chk_1", Name: "shop", Spec: spec, State: dockwellv1.AlertCheckState_ALERT_CHECK_STATE_UP}}
	ctx, cancel := context.WithTimeout(t.Context(), 5*time.Second)
	defer cancel()
	start := time.Now()
	r.Due(ctx)
	if took := time.Since(start); took > time.Second {
		t.Fatalf("Due waited %v for a slot", took)
	}
	st, _ := r.Status("chk_1")
	if len(st.Runs) != 1 || !st.Runs[0].GetSkipped() || st.Runs[0].GetOk() || st.State != dockwellv1.AlertCheckState_ALERT_CHECK_STATE_UP {
		t.Errorf("a check that couldn't start stands as %+v", st)
	}
}
