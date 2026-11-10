// SPDX-License-Identifier: AGPL-3.0-only

package alerts

import (
	"bytes"
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"math"
	"net/http"
	"net/http/httptest"
	"runtime"
	"strconv"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"google.golang.org/protobuf/types/known/timestamppb"

	resourcev1 "github.com/dockwell/dockwell/gen/dockwell/resource/v1alpha1"
	dockwellv1 "github.com/dockwell/dockwell/gen/dockwell/v1"
	"github.com/dockwell/dockwell/internal/store"
)

var t0 = time.Date(2026, 11, 7, 12, 0, 0, 0, time.UTC)

func evaluator(now *time.Time) *Evaluator {
	e := &Evaluator{Now: func() time.Time { return *now }}
	e.init()
	return e
}

func diskRule(duration string) rule {
	return rule{id: "alr_disk", name: "disk-full", spec: &resourcev1.AlertRule{
		Condition: resourcev1.AlertRule_CONDITION_DISK_FULL, Threshold: 90, Duration: duration,
	}}
}

// A condition fires once it has held for its rule's duration, by its signal's
// clock where it has one; what's clear resolves; what's unknown keeps what's open;
// and what's gone resolves, unless it's dark, or a notice's.
func TestWhatFiresAndWhatResolves(t *testing.T) {
	now := t0
	e := evaluator(&now)
	r := diskRule("5m")
	full := finding{thing: "disk:srv_a:/", verdict: holds, title: "web-1's disk / is 93% full"}

	opens, resolves := e.judge(r, []finding{full}, nil, now, false)
	if len(opens) != 0 || len(resolves) != 0 {
		t.Fatalf("at once, %v opens and %v resolves; want it pending", opens, resolves)
	}
	now = now.Add(4 * time.Minute)
	if opens, _ = e.judge(r, []finding{full}, nil, now, false); len(opens) != 0 {
		t.Fatalf("after 4 minutes of 5, %v opens", opens)
	}
	now = now.Add(time.Minute)
	opens, _ = e.judge(r, []finding{full}, nil, now, false)
	if len(opens) != 1 || opens[0].Thing != full.thing || opens[0].Title != full.title || opens[0].RuleName != "disk-full" {
		t.Fatalf("after 5 minutes, %v opens", opens)
	}
	open := []store.Alert{opens[0]}

	if opens, resolves = e.judge(r, []finding{full}, open, now, false); len(opens) != 0 || len(resolves) != 0 {
		t.Errorf("while it holds, %v opens and %v resolves", opens, resolves)
	}
	if _, resolves = e.judge(r, []finding{{thing: full.thing, verdict: unknown}}, open, now, false); len(resolves) != 0 {
		t.Errorf("what can't be read resolved %v", resolves)
	}
	if _, resolves = e.judge(r, []finding{{thing: "disk:srv_a:", prefix: true, verdict: unknown}}, open, now, false); len(resolves) != 0 {
		t.Errorf("a server whose stats can't be read resolved %v", resolves)
	}
	if _, resolves = e.judge(r, nil, open, now, false); len(resolves) != 1 || resolves[0].quiet {
		t.Errorf("what's gone resolves %v, want it resolved, with its message", resolves)
	}
	if _, resolves = e.judge(r, nil, open, now, true); len(resolves) != 0 {
		t.Errorf("a notice's alert resolved as gone: %v", resolves)
	}
	// What's clear resolves once it has been for the rule's duration, so a dip
	// doesn't close the alert and open another.
	clean := []finding{{thing: full.thing, verdict: fine}}
	if _, resolves = e.judge(r, clean, open, now, false); len(resolves) != 0 {
		t.Errorf("what's just come clear resolved %v at once", resolves)
	}
	if _, resolves = e.judge(r, []finding{full}, open, now.Add(4*time.Minute), false); len(resolves) != 0 {
		t.Errorf("what holds again resolved %v", resolves)
	}
	if _, resolves = e.judge(r, clean, open, now.Add(5*time.Minute), false); len(resolves) != 0 {
		t.Errorf("a dip's clock went on through what held: %v", resolves)
	}
	if _, resolves = e.judge(r, clean, open, now.Add(10*time.Minute), false); len(resolves) != 1 {
		t.Errorf("what's been clear for the rule's 5 minutes resolves %v", resolves)
	}

	// A signal that says since when fires by that, as a restart forgets nothing of it.
	since := full
	since.thing, since.since = "disk:srv_b:/", now.Add(-10*time.Minute)
	if opens, _ = e.judge(r, []finding{since}, nil, now, false); len(opens) != 1 {
		t.Errorf("a condition its signal says has held for 10 minutes didn't fire: %v", opens)
	}
	// What resolved before it fired leaves nothing pending.
	brief := full
	brief.thing = "disk:srv_c:/"
	e.judge(r, []finding{brief}, nil, now, false)
	e.judge(r, []finding{{thing: brief.thing, verdict: fine}}, nil, now, false)
	now = now.Add(time.Hour)
	if opens, _ = e.judge(r, []finding{brief}, nil, now, false); len(opens) != 0 {
		t.Errorf("a condition that held, cleared and holds again fired at once: %v", opens)
	}
}

type fleet []*dockwellv1.Server

func (f fleet) List() []*dockwellv1.Server { return f }

// A server that can't be reached counts from when the control plane started, if
// that's later than when it was last heard; a server's disks and memory are read
// from its stats while they're fresh, and are unknown, disk by disk, when not.
func TestWhatServersSay(t *testing.T) {
	now := t0
	e := evaluator(&now)
	stats := func(age time.Duration) *dockwellv1.ServerStats {
		return &dockwellv1.ServerStats{
			Time: timestamppb.New(now.Add(-age)), MemoryUsedBytes: 95, MemoryTotalBytes: 100,
			Disks: []*dockwellv1.DiskUsage{{MountPoint: "/", UsedBytes: 93, SizeBytes: 100}, {MountPoint: "/data", UsedBytes: 10, SizeBytes: 100}},
		}
	}
	e.Fleet = fleet{
		{Id: "srv_a", Name: "web-1", Connection: dockwellv1.Connection_CONNECTION_CONNECTED, Stats: stats(5 * time.Second)},
		{Id: "srv_b", Name: "web-2", Connection: dockwellv1.Connection_CONNECTION_CONNECTED, Stats: stats(10 * time.Minute)},
		{Id: "srv_c", Name: "web-3", Connection: dockwellv1.Connection_CONNECTION_UNREACHABLE, LastSeenTime: timestamppb.New(now.Add(-time.Hour))},
		{Id: "srv_d", Name: "web-4", Connection: dockwellv1.Connection_CONNECTION_PENDING},
	}
	sig := &signals{e: e, now: now.Add(time.Minute)}
	got := map[string]finding{}
	for _, f := range sig.unreachable() {
		got[f.thing] = f
	}
	if f := got["srv:srv_c"]; f.verdict != holds || !f.since.Equal(e.started) || !strings.Contains(f.detail, "systemctl restart dockwell") {
		t.Errorf("an unreachable server reads %+v, counted from the start, %v", f, e.started)
	}
	if got["srv:srv_a"].verdict != fine || len(got) != 3 {
		t.Errorf("servers read %v: want a connected one clear, and one that hasn't joined left out", got)
	}
	disks := map[string]finding{}
	for _, f := range sig.disks(diskRule("")) {
		disks[f.thing] = f
	}
	if f := disks["disk:srv_a:/"]; f.verdict != holds || f.title != "web-1's disk / is 93% full" {
		t.Errorf("a full disk reads %+v", f)
	}
	if disks["disk:srv_a:/data"].verdict != fine {
		t.Errorf("a disk with room reads %+v", disks["disk:srv_a:/data"])
	}
	if f := disks["disk:srv_b:"]; !f.prefix || f.verdict != unknown {
		t.Errorf("a server whose stats are old reads %+v, want its disks unknown", f)
	}
	mem := sig.memory(rule{spec: &resourcev1.AlertRule{Condition: resourcev1.AlertRule_CONDITION_MEMORY_HIGH, Threshold: 90}})
	if mem[0].verdict != holds || mem[0].title != "web-1 uses 95% of its memory" || mem[1].verdict != unknown {
		t.Errorf("memory reads %+v", mem)
	}
}

// A message says the fact, what it means and the fix, then where the panel shows
// it; several of a rule go as one.
func TestAMessage(t *testing.T) {
	r := &resourcev1.AlertRule{Condition: resourcev1.AlertRule_CONDITION_DISK_FULL, Threshold: 90, Severity: resourcev1.AlertRule_SEVERITY_WARNING}
	a := store.Alert{ID: "alt_1", Condition: int32(resourcev1.AlertRule_CONDITION_DISK_FULL), Title: "web-1's disk / is 93% full", Detail: "It's over 90%. Free space with docker system prune.", Path: "/servers/srv_a", OpenedAt: t0}
	one := Message{Status: "firing", Rule: r, RuleName: "disk-full", Alerts: []store.Alert{a}, Panel: "https://panel.example.com", At: t0}
	if want := "web-1's disk / is 93% full\nIt's over 90%. Free space with docker system prune.\nhttps://panel.example.com/servers/srv_a"; one.Text() != want {
		t.Errorf("one alert reads %q, want %q", one.Text(), want)
	}
	b := a
	b.Title = "web-2's disk / is 95% full"
	two := Message{Status: "firing", Rule: r, Alerts: []store.Alert{a, b}, At: t0}
	if two.Title() != "2 disks are over 90% full" || !strings.Contains(two.Text(), "- web-2's disk / is 95% full") || strings.Contains(two.Text(), "https://") {
		t.Errorf("two read %q", two.Text())
	}
	resolved := t0.Add(12 * time.Minute)
	a.ResolvedAt = &resolved
	done := Message{Status: "resolved", Rule: r, Alerts: []store.Alert{a}, At: resolved}
	if !strings.HasPrefix(done.Text(), "Resolved: web-1's disk / is 93% full\nIt held for 12 minutes.") {
		t.Errorf("a resolved alert reads %q", done.Text())
	}
}

// A webhook gets JSON, signed with the channel's secret, and how it answered is
// said in a line; one that fails is tried three times; and nothing goes to a
// link-local address.
func TestAWebhook(t *testing.T) {
	var got payload
	var signature string
	var fails atomic.Int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		if r.URL.Path == "/down" {
			fails.Add(1)
			w.WriteHeader(http.StatusServiceUnavailable)
			return
		}
		signature = r.Header.Get("Dockwell-Signature")
		_ = json.Unmarshal(body, &got)
		if want := Signature("s3cret", t0, body); signature != want {
			t.Errorf("the signature is %q, want %q", signature, want)
		}
		w.WriteHeader(http.StatusAccepted)
	}))
	defer srv.Close()
	s := &Sender{}
	r := &resourcev1.AlertRule{Condition: resourcev1.AlertRule_CONDITION_SERVER_UNREACHABLE, Severity: resourcev1.AlertRule_SEVERITY_CRITICAL}
	m := Message{Status: "firing", Rule: r, RuleName: "server-unreachable", At: t0, Alerts: []store.Alert{{ID: "alt_1", Title: "web-1 can't be reached", ServerID: "srv_a", OpenedAt: t0}}}
	hook := func(url string) *resourcev1.AlertChannel {
		return &resourcev1.AlertChannel{To: &resourcev1.AlertChannel_Webhook{Webhook: &resourcev1.AlertWebhook{Url: url, Secret: "s3cret"}}}
	}

	res := s.Once(t.Context(), hook(srv.URL+"/hook"), m)
	if !res.OK || res.Said != "202 Accepted" {
		t.Errorf("a webhook that takes it says %+v", res)
	}
	if got.Status != "firing" || got.Rule.Name != "server-unreachable" || got.Rule.Severity != "critical" || len(got.Alerts) != 1 || got.Alerts[0].Title != "web-1 can't be reached" {
		t.Errorf("the webhook got %+v", got)
	}
	if res := s.Once(t.Context(), hook(srv.URL+"/down"), m); res.OK || res.Said != "503 Service Unavailable" || fails.Load() != 1 {
		t.Errorf("one that's down says %+v after %d tries; the outbox tries again, not the sender", res, fails.Load())
	}
	if res := s.Once(t.Context(), hook("http://169.254.169.254/latest"), m); res.OK || !strings.Contains(res.Said, "link-local") {
		t.Errorf("a link-local address says %+v", res)
	}
	if res := s.Once(t.Context(), hook(srv.URL+"/hook?token=t0ken-in-the-url"), m); strings.Contains(res.Said, "t0ken") {
		t.Errorf("what the far end answered holds the URL's token: %q", res.Said)
	}
}

// A test never reaches this server's own addresses, whatever a channel may, so a
// test, which shows what answered, can't read what listens on the control server's
// loopback; the outbox reaches them, as a channel planned to one asks.
func TestATestNeverReachesLoopback(t *testing.T) {
	var heard atomic.Int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		heard.Add(1)
		w.WriteHeader(http.StatusAccepted)
	}))
	defer srv.Close()
	c := &resourcev1.AlertChannel{To: &resourcev1.AlertChannel_Webhook{Webhook: &resourcev1.AlertWebhook{Url: srv.URL + "/hook?token=t0ken"}}}
	m := Message{Status: "test", At: t0}
	s := &Sender{}
	r := s.Test(t.Context(), c, m)
	if r.OK || !strings.Contains(r.Said, "is this server's own address, so Dockwell sends nothing there") || strings.Contains(r.Said, "t0ken") || heard.Load() != 0 {
		t.Errorf("a test to loopback says %+v, and the server heard %d", r, heard.Load())
	}
	if r := s.Once(t.Context(), c, m); !r.OK || heard.Load() != 1 {
		t.Errorf("the outbox to loopback says %+v", r)
	}
}

// What evaluation keeps, at 1,000 servers with 20 services each, every one of them
// pending at once, the worst there is (ADR 0044).
func TestEvaluationAt1000Servers(t *testing.T) {
	if testing.Short() {
		t.Skip("measures memory")
	}
	now := t0
	e := evaluator(&now)
	rules := []rule{
		{id: "alr_down", name: "app-down", spec: &resourcev1.AlertRule{Condition: resourcev1.AlertRule_CONDITION_SERVICE_DOWN, Duration: "1h"}},
		{id: "alr_srv", name: "server-unreachable", spec: &resourcev1.AlertRule{Condition: resourcev1.AlertRule_CONDITION_SERVER_UNREACHABLE, Duration: "1h"}},
		{id: "alr_disk", name: "disk-full", spec: &resourcev1.AlertRule{Condition: resourcev1.AlertRule_CONDITION_DISK_FULL, Duration: "1h"}},
		{id: "alr_mem", name: "memory-high", spec: &resourcev1.AlertRule{Condition: resourcev1.AlertRule_CONDITION_MEMORY_HIGH, Duration: "1h"}},
		{id: "alr_cert", name: "certificate-expiring", spec: &resourcev1.AlertRule{Condition: resourcev1.AlertRule_CONDITION_CERTIFICATE_EXPIRING, Duration: "1h"}},
	}
	var before, after runtime.MemStats
	runtime.GC()
	runtime.ReadMemStats(&before)
	took := judgeAt1000(e, rules, now)
	runtime.GC()
	runtime.ReadMemStats(&after)
	kept := int64(after.HeapAlloc) - int64(before.HeapAlloc)
	t.Logf("%d pending, %.1f MB kept, judged in %v", len(e.pending), float64(kept)/(1<<20), took)
	if len(e.pending) != 24000 {
		t.Errorf("%d pending, want 24,000", len(e.pending))
	}
	if kept > 8<<20 {
		t.Errorf("evaluation keeps %.1f MB at 1,000 servers, want under 8", float64(kept)/(1<<20))
	}
}

// judgeAt1000 judges what 1,000 servers with 20 services each find, and leaves
// nothing but what the evaluator keeps.
func judgeAt1000(e *Evaluator, rules []rule, now time.Time) time.Duration {
	findings := make([][]finding, len(rules))
	for s := range 1000 {
		srv := fmt.Sprintf("srv_01kat4zr6e3v0y5xbkzb5d9%03d", s)
		for v := range 20 {
			findings[0] = append(findings[0], finding{thing: fmt.Sprintf("svc:svc_01kat4zr6e3v0y5xbkzb5d%05d@%s", s*20+v, srv), verdict: holds})
		}
		findings[1] = append(findings[1], finding{thing: "srv:" + srv, verdict: holds})
		findings[2] = append(findings[2], finding{thing: "disk:" + srv + ":/", verdict: holds})
		findings[3] = append(findings[3], finding{thing: "mem:" + srv, verdict: holds})
		findings[4] = append(findings[4], finding{thing: "cert:shop.example.com@" + srv, verdict: holds})
	}
	start := time.Now()
	for i, r := range rules {
		e.judge(r, findings[i], nil, now, false)
	}
	return time.Since(start)
}

// A proxy in the environment is no way past the dial check: a webhook goes
// straight to where it points, and the proxy hears nothing (ADR 0044).
func TestAWebhookGoesThroughNoProxy(t *testing.T) {
	var proxied atomic.Int32
	proxy := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		proxied.Add(1)
		w.WriteHeader(http.StatusOK)
	}))
	defer proxy.Close()
	var reached atomic.Int32
	hook := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		reached.Add(1)
		w.WriteHeader(http.StatusAccepted)
	}))
	defer hook.Close()
	t.Setenv("HTTP_PROXY", proxy.URL)
	t.Setenv("http_proxy", proxy.URL)
	c := &resourcev1.AlertChannel{To: &resourcev1.AlertChannel_Webhook{Webhook: &resourcev1.AlertWebhook{Url: hook.URL + "/hook"}}}
	if r := (&Sender{}).Once(t.Context(), c, Message{Status: "test", At: t0}); !r.OK || reached.Load() != 1 || proxied.Load() != 0 {
		t.Errorf("with a proxy set, the webhook said %+v, was reached %d times, and the proxy %d", r, reached.Load(), proxied.Load())
	}
}

type certAt time.Time

func (c certAt) Domain(string, string) (*dockwellv1.DomainStatus, bool) {
	return &dockwellv1.DomainStatus{Certificate: &dockwellv1.Certificate{
		State: dockwellv1.CertificateState_CERTIFICATE_STATE_ISSUED, ExpireTime: timestamppb.New(time.Time(c)),
	}}, true
}

// "Within 14 days" holds from 14 days out, and says how long is left in words
// that read right at one day and none.
func TestACertificateEndsWithinItsDays(t *testing.T) {
	now := t0
	e := evaluator(&now)
	for left, want := range map[time.Duration]string{
		14*24*time.Hour + time.Minute:  "",
		14*24*time.Hour - time.Minute:  "shop.example.com's certificate ends in 14 days",
		13*24*time.Hour + 12*time.Hour: "shop.example.com's certificate ends in 14 days",
		20 * time.Hour:                 "shop.example.com's certificate ends within a day",
		-time.Hour:                     "shop.example.com's certificate has ended",
	} {
		e.Domains = certAt(now.Add(left))
		s := &signals{e: e, now: now}
		f := s.certificate("cert:shop.example.com@srv_a", "shop.example.com", "srv_a", "web-1", "rte_1", true, 14)
		if f.title != want || (want != "") != (f.verdict == holds) {
			t.Errorf("a certificate %v from its end reads %v %q, want %q", left, f.verdict, f.title, want)
		}
	}
}

// Several of a rule's alerts read as what they have in common, and one closed
// because its rule went says so.
func TestWhatAMessageSaysOfManyAndOfOneClosed(t *testing.T) {
	down := func(n int) []store.Alert {
		out := make([]store.Alert, n)
		for i := range out {
			out[i] = store.Alert{ID: fmt.Sprintf("alt_%d", i), Condition: int32(resourcev1.AlertRule_CONDITION_SERVICE_DOWN), Title: fmt.Sprintf("app-%d isn't running on web-1", i), OpenedAt: t0}
		}
		return out
	}
	r := &resourcev1.AlertRule{Condition: resourcev1.AlertRule_CONDITION_SERVICE_DOWN}
	if got := (Message{Status: "firing", Rule: r, Alerts: down(30), At: t0}).Title(); got != "30 apps are down" {
		t.Errorf("30 apps down reads %q", got)
	}
	addresses := down(3)
	for i := range addresses {
		addresses[i].Condition = int32(resourcev1.AlertRule_CONDITION_CHECK_DOWN)
	}
	checkDown := &resourcev1.AlertRule{Condition: resourcev1.AlertRule_CONDITION_CHECK_DOWN}
	if got := (Message{Status: "firing", Rule: checkDown, Alerts: addresses, At: t0}).Title(); got != "3 addresses are down" {
		t.Errorf("3 addresses down reads %q", got)
	}
	gone := down(1)[0]
	at := t0.Add(time.Hour)
	gone.ResolvedAt, gone.Ended = &at, "removed"
	m := Message{Status: "resolved", RuleName: "app-down", Alerts: []store.Alert{gone}, At: at}
	if !strings.Contains(m.Text(), "Its rule was removed") || strings.Contains(m.Text(), "It held for") {
		t.Errorf("an alert closed with its rule reads %q", m.Text())
	}
}

// fromDockwell is docs/webhooks.md's check, as it's printed there, so the page
// says what the signature is.
func fromDockwell(r *http.Request, secret []byte) ([]byte, bool) {
	body, err := io.ReadAll(io.LimitReader(r.Body, 1<<20))
	if err != nil {
		return nil, false
	}
	var t, v1 string
	for _, part := range strings.Split(r.Header.Get("Dockwell-Signature"), ",") {
		k, v, _ := strings.Cut(part, "=")
		switch k {
		case "t":
			t = v
		case "v1":
			v1 = v
		}
	}
	sent, err := strconv.ParseInt(t, 10, 64)
	if err != nil || math.Abs(time.Since(time.Unix(sent, 0)).Minutes()) > 5 {
		return nil, false
	}
	got, err := hex.DecodeString(v1)
	if err != nil {
		return nil, false
	}
	mac := hmac.New(sha256.New, secret)
	mac.Write([]byte(t + "."))
	mac.Write(body)
	return body, hmac.Equal(got, mac.Sum(nil))
}

// What a webhook signs, the check docs/webhooks.md prints takes; and it refuses
// another secret, a changed body, and a request from long ago.
func TestTheDocumentedCheckTakesWhatsSigned(t *testing.T) {
	var ok, wrong atomic.Int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		raw, _ := io.ReadAll(r.Body)
		r.Body = io.NopCloser(strings.NewReader(string(raw)))
		if body, good := fromDockwell(r, []byte("s3cret")); good && bytes.Equal(body, raw) {
			ok.Add(1)
		}
		r.Body = io.NopCloser(strings.NewReader(string(raw)))
		if _, good := fromDockwell(r, []byte("another")); good {
			wrong.Add(1)
		}
		w.WriteHeader(http.StatusNoContent)
	}))
	defer srv.Close()
	c := &resourcev1.AlertChannel{To: &resourcev1.AlertChannel_Webhook{Webhook: &resourcev1.AlertWebhook{Url: srv.URL, Secret: "s3cret"}}}
	if r := (&Sender{}).Once(t.Context(), c, Message{Status: "test", At: time.Now()}); !r.OK || ok.Load() != 1 || wrong.Load() != 0 {
		t.Errorf("the documented check took %d of 1, and %d with another secret", ok.Load(), wrong.Load())
	}
	at := time.Now().Add(-10 * time.Minute)
	body := []byte(`{"status":"test"}`)
	old := httptest.NewRequest(http.MethodPost, "/", strings.NewReader(string(body)))
	old.Header.Set("Dockwell-Signature", Signature("s3cret", at, body))
	if _, good := fromDockwell(old, []byte("s3cret")); good {
		t.Errorf("the documented check took a request signed 10 minutes ago")
	}
	changed := httptest.NewRequest(http.MethodPost, "/", strings.NewReader(`{"status":"firing"}`))
	changed.Header.Set("Dockwell-Signature", Signature("s3cret", time.Now(), body))
	if _, good := fromDockwell(changed, []byte("s3cret")); good {
		t.Errorf("the documented check took a body other than the one signed")
	}
}
