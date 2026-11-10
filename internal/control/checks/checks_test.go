// SPDX-License-Identifier: AGPL-3.0-only

package checks

import (
	"context"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

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

// Unless a check says it's inside the network, it reaches the internet alone, so a
// name that leads to a private address reads failing, and says why. Nothing is
// sent there: netguard refuses the address before it dials.
func TestACheckReachesThePrivateNetworkOnlyFromInside(t *testing.T) {
	run := Probe(t.Context(), Guard(false), &resourcev1.AlertCheck{Address: "http://10.255.255.1/", Timeout: "2s", ExpectFrom: 200, ExpectTo: 399}, nil)
	if run.GetOk() || !strings.Contains(run.GetSaid(), "private network") {
		t.Errorf("a check of a private address from outside read %v", run)
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
