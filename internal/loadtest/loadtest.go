// SPDX-License-Identifier: AGPL-3.0-only

// Package loadtest is what shows a rollout drops nothing: an app that answers
// every kind of request a rollout has to carry, and a load of them that counts
// each. The tests against real Caddy and make docker-e2e, through
// scripts/e2e-load, both use it, so what is measured on one means the same on the
// other. It is never part of a release.
package loadtest

import (
	"bufio"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/http/httptrace"
	"slices"
	"strings"
	"sync"
	"sync/atomic"
	"time"
)

// Kinds are the kinds of request a load makes.
var Kinds = []string{"short", "fresh", "browser", "post-http1", "post-http2", "slow", "stream", "websocket"}

// App is what answers: one instance of a service.
type App struct {
	// Name says which instance answered.
	Name string
	// Ready says how the app stands, as a status and a line of text. Nil is 200.
	Ready func() (int, string)

	mu    sync.Mutex
	conns map[net.Conn]bool
}

// Handler answers each kind of request.
func (a *App) Handler() http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("/healthz", func(w http.ResponseWriter, _ *http.Request) {
		status, say := http.StatusOK, "ok"
		if a.Ready != nil {
			status, say = a.Ready()
		}
		w.WriteHeader(status)
		_, _ = io.WriteString(w, say)
	})
	mux.HandleFunc("/who", func(w http.ResponseWriter, r *http.Request) {
		_, _ = io.Copy(io.Discard, r.Body)
		_ = json.NewEncoder(w).Encode(map[string]string{"name": a.Name})
	})
	mux.HandleFunc("/slow", func(w http.ResponseWriter, r *http.Request) {
		select {
		case <-r.Context().Done():
			return
		case <-time.After(time.Second):
		}
		_, _ = io.WriteString(w, "slow done")
	})
	mux.HandleFunc("/stream", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		for i := range 10 {
			_, _ = fmt.Fprintf(w, "data: %d\n\n", i)
			w.(http.Flusher).Flush()
			select {
			case <-r.Context().Done():
				return
			case <-time.After(100 * time.Millisecond):
			}
		}
	})
	mux.HandleFunc("/ws", a.webSocket)
	return mux
}

// webSocket says it upgrades, and echoes what comes. It is enough of one for what
// is in question, a connection Caddy has to hold open, and the client's side reads
// each echo.
func (a *App) webSocket(w http.ResponseWriter, _ *http.Request) {
	conn, rw, err := w.(http.Hijacker).Hijack()
	if err != nil {
		return
	}
	a.mu.Lock()
	if a.conns == nil {
		a.conns = map[net.Conn]bool{}
	}
	a.conns[conn] = true
	a.mu.Unlock()
	defer func() {
		a.mu.Lock()
		delete(a.conns, conn)
		a.mu.Unlock()
		_ = conn.Close()
	}()
	_, _ = rw.WriteString("HTTP/1.1 101 Switching Protocols\r\nUpgrade: websocket\r\nConnection: Upgrade\r\n\r\n")
	_ = rw.Flush()
	_, _ = io.Copy(conn, rw)
}

// CloseWebSockets ends what an http.Server doesn't know of, as a process that
// ends does.
func (a *App) CloseWebSockets() {
	a.mu.Lock()
	defer a.mu.Unlock()
	for c := range a.conns {
		_ = c.Close()
	}
}

// Tally counts how each kind of request went, and keeps apart what a reload of
// Caddy costs anyway: a new connection on its way in, cut before anything answered.
// Caddy binds a new socket with SO_REUSEPORT on each reload, and closing the old
// one resets what is in its queue. That isn't the rollout's doing, nothing can
// configure it away, and no request has got anywhere (ADR 0027). A connection that
// had already answered isn't on its way in, so its loss is a failure.
type Tally struct {
	mu     sync.Mutex
	ok     map[string]int
	bad    map[string]int
	resets map[string]int
	said   map[string]*Said
	served map[string]int
}

// Said is one thing that went wrong: how often, and first when, to read against
// what a rollout was doing then.
type Said struct {
	What  string    `json:"what"`
	Count int       `json:"count"`
	First time.Time `json:"first"`
}

// mostSaid bounds what a Tally keeps of what went wrong: what each says varies,
// and a few are enough to read.
const mostSaid = 20

// NewTally returns a Tally with nothing on it.
func NewTally() *Tally {
	return &Tally{ok: map[string]int{}, bad: map[string]int{}, resets: map[string]int{}, said: map[string]*Said{}, served: map[string]int{}}
}

// Pass counts a request that went as it should.
func (l *Tally) Pass(kind string) {
	l.mu.Lock()
	defer l.mu.Unlock()
	l.ok[kind]++
}

// Fail counts a request that didn't, and why.
func (l *Tally) Fail(kind, why string) {
	l.mu.Lock()
	defer l.mu.Unlock()
	l.bad[kind]++
	what := kind + ": " + why
	if s := l.said[what]; s != nil {
		s.Count++
	} else if len(l.said) < mostSaid {
		l.said[what] = &Said{What: what, Count: 1, First: time.Now()}
	}
}

// Reset counts a new connection cut before anything answered.
func (l *Tally) Reset(kind string) {
	l.mu.Lock()
	defer l.mu.Unlock()
	l.resets[kind]++
}

// Served counts an answer from an instance.
func (l *Tally) Served(name string) {
	l.mu.Lock()
	defer l.mu.Unlock()
	l.served[name]++
}

// Passed says how many requests of a kind went as they should.
func (l *Tally) Passed(kind string) int {
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.ok[kind]
}

// Failures says how many requests failed.
func (l *Tally) Failures() int {
	l.mu.Lock()
	defer l.mu.Unlock()
	n := 0
	for _, v := range l.bad {
		n += v
	}
	return n
}

// Resets says how many new connections were cut before anything answered.
func (l *Tally) Resets() int {
	l.mu.Lock()
	defer l.mu.Unlock()
	n := 0
	for _, v := range l.resets {
		n += v
	}
	return n
}

// ServedBy says how many answers came from each instance.
func (l *Tally) ServedBy() map[string]int {
	l.mu.Lock()
	defer l.mu.Unlock()
	out := make(map[string]int, len(l.served))
	for k, v := range l.served {
		out[k] = v
	}
	return out
}

// Errors says what went wrong, first first.
func (l *Tally) Errors() []Said {
	l.mu.Lock()
	defer l.mu.Unlock()
	out := make([]Said, 0, len(l.said))
	for _, s := range l.said {
		out = append(out, *s)
	}
	slices.SortFunc(out, func(a, b Said) int { return a.First.Compare(b.First) })
	return out
}

func (l *Tally) String() string {
	l.mu.Lock()
	defer l.mu.Unlock()
	var b strings.Builder
	for _, k := range Kinds {
		fmt.Fprintf(&b, "%s: %d passed, %d failed, %d reset on the way in. ", k, l.ok[k], l.bad[k], l.resets[k])
	}
	return b.String()
}

// Report is a Tally to hand on.
type Report struct {
	Passed   map[string]int `json:"passed"`
	Failed   int            `json:"failed"`
	Reset    int            `json:"reset"`
	Errors   []Said         `json:"errors,omitempty"`
	ServedBy map[string]int `json:"servedBy"`
	Summary  string         `json:"summary"`
}

// Report says what it counted.
func (l *Tally) Report() Report {
	passed := map[string]int{}
	for _, k := range Kinds {
		passed[k] = l.Passed(k)
	}
	return Report{Passed: passed, Failed: l.Failures(), Reset: l.Resets(), Errors: l.Errors(), ServedBy: l.ServedBy(), Summary: l.String()}
}

// Target is what a load goes to.
type Target struct {
	// Domain is the site, as a request names it.
	Domain string
	// Client makes a client with connections of its own that reach it.
	Client func() *http.Client
	// ClientHTTP1 is the same, speaking only HTTP/1.1, where a kept connection has
	// nothing to tell a client it is going away: a request sent as it closes is lost.
	ClientHTTP1 func() *http.Client
	// Dial opens a connection that reaches it over TLS, speaking HTTP/1.1, for a
	// WebSocket.
	Dial func(ctx context.Context) (net.Conn, error)
}

// onTheWayIn is the error of a connection nothing answered on, as what a closed
// socket's queue held gets.
func onTheWayIn(err error) bool {
	msg := err.Error()
	return strings.Contains(msg, "connection reset by peer") || strings.HasSuffix(msg, "EOF") || strings.Contains(msg, "broken pipe")
}

// Run makes every kind of request until stop closes, and counts each. Wait on what
// it returns for those in flight to end.
func (t Target) Run(ctx context.Context, l *Tally, stop <-chan struct{}) *sync.WaitGroup {
	wg := &sync.WaitGroup{}
	running := func() bool {
		select {
		case <-stop:
			return false
		default:
			return ctx.Err() == nil
		}
	}
	// send makes a request. A connection it had already used isn't on its way in.
	// Go's client tries a GET again on a new one where it is safe to, and never a
	// POST, as nobody can say whether that was taken, so a POST is what shows what a
	// kept connection loses.
	send := func(c *http.Client, method, kind, path string, check func([]byte) string) {
		var reused atomic.Bool
		trace := &httptrace.ClientTrace{GotConn: func(i httptrace.GotConnInfo) { reused.Store(i.Reused) }}
		var body io.Reader = http.NoBody
		if method == http.MethodPost {
			body = strings.NewReader(`{"order":1}`)
		}
		req, err := http.NewRequestWithContext(httptrace.WithClientTrace(ctx, trace), method, "https://"+t.Domain+path, body)
		if err != nil {
			l.Fail(kind, err.Error())
			return
		}
		res, err := c.Do(req)
		if err != nil {
			if ctx.Err() != nil {
				return
			}
			if !reused.Load() && onTheWayIn(err) {
				l.Reset(kind)
				return
			}
			l.Fail(kind, err.Error())
			return
		}
		defer res.Body.Close()
		b, err := io.ReadAll(res.Body)
		switch {
		case err != nil:
			l.Fail(kind, err.Error())
		case res.StatusCode != http.StatusOK:
			l.Fail(kind, fmt.Sprintf("%d %s", res.StatusCode, b))
		default:
			if why := check(b); why != "" {
				l.Fail(kind, why)
				return
			}
			l.Pass(kind)
		}
	}
	get := func(c *http.Client, kind, path string, check func([]byte) string) {
		send(c, http.MethodGet, kind, path, check)
	}
	who := func(b []byte) string {
		var w map[string]string
		if err := json.Unmarshal(b, &w); err != nil {
			return "an answer that isn't the app's: " + string(b)
		}
		l.Served(w["name"])
		return ""
	}
	// Short requests over kept connections, as a browser's.
	shared := t.Client()
	for range 8 {
		wg.Go(func() {
			for running() {
				get(shared, "short", "/who", who)
				time.Sleep(2 * time.Millisecond)
			}
		})
	}
	// POSTs over kept connections, with nothing to send them again, on each protocol.
	postHTTP2 := t.Client()
	for range 4 {
		wg.Go(func() {
			for running() {
				send(postHTTP2, http.MethodPost, "post-http2", "/who", who)
				time.Sleep(2 * time.Millisecond)
			}
		})
	}
	if t.ClientHTTP1 != nil {
		postHTTP1 := t.ClientHTTP1()
		for range 4 {
			wg.Go(func() {
				for running() {
					send(postHTTP1, http.MethodPost, "post-http1", "/who", who)
					time.Sleep(2 * time.Millisecond)
				}
			})
		}
	}
	// Short requests each on a connection of its own, which meet every reload.
	fresh := t.Client()
	fresh.Transport.(*http.Transport).DisableKeepAlives = true
	for range 4 {
		wg.Go(func() {
			for running() {
				get(fresh, "fresh", "/who", who)
				time.Sleep(5 * time.Millisecond)
			}
		})
	}
	// Clients that come and stay a while, each on a connection of its own: what
	// a browser does, and what a reload can catch mid-handshake.
	for i := range 32 {
		wg.Go(func() {
			time.Sleep(time.Duration(i) * 40 * time.Millisecond)
			for running() {
				c := t.Client()
				for range 100 {
					if !running() {
						break
					}
					get(c, "browser", "/who", who)
					time.Sleep(10 * time.Millisecond)
				}
				c.CloseIdleConnections()
			}
		})
	}
	// Requests that take a while, across a drain.
	for range 4 {
		wg.Go(func() {
			for running() {
				get(shared, "slow", "/slow", func(b []byte) string {
					if string(b) != "slow done" {
						return "ended with " + string(b)
					}
					return ""
				})
			}
		})
	}
	// Server-sent events, each a second long.
	for range 2 {
		wg.Go(func() {
			for running() {
				get(shared, "stream", "/stream", func(b []byte) string {
					if n := strings.Count(string(b), "data:"); n != 10 {
						return fmt.Sprintf("got %d events of 10", n)
					}
					return ""
				})
			}
		})
	}
	// WebSockets that last 1.5 s, shorter than a drain, echoing all the while.
	for range 2 {
		wg.Go(func() {
			for running() {
				switch why := t.WebSocket(ctx, 1500*time.Millisecond); {
				case ctx.Err() != nil:
					return
				case why == CutOnTheWayIn:
					l.Reset("websocket")
				case why != "":
					l.Fail("websocket", why)
				default:
					l.Pass("websocket")
				}
			}
		})
	}
	return wg
}

// CutOnTheWayIn is what WebSocket says of one whose connection nothing answered.
const CutOnTheWayIn = "cut on the way in"

// WebSocket opens one, echoes through it for as long as it lasts, and says what
// went wrong, if anything did.
func (t Target) WebSocket(ctx context.Context, lasts time.Duration) string {
	conn, err := t.Dial(ctx)
	if err != nil {
		if onTheWayIn(err) {
			return CutOnTheWayIn
		}
		return "dial: " + err.Error()
	}
	defer conn.Close()
	_, _ = fmt.Fprintf(conn, "GET /ws HTTP/1.1\r\nHost: %s\r\nUpgrade: websocket\r\nConnection: Upgrade\r\nSec-WebSocket-Key: x\r\nSec-WebSocket-Version: 13\r\n\r\n", t.Domain)
	br := bufio.NewReader(conn)
	_ = conn.SetDeadline(time.Now().Add(10 * time.Second))
	status, err := br.ReadString('\n')
	if err != nil && status == "" && onTheWayIn(err) {
		return CutOnTheWayIn
	}
	if err != nil || !strings.Contains(status, "101") {
		return fmt.Sprintf("the upgrade answered %q, %v", status, err)
	}
	for {
		line, err := br.ReadString('\n')
		if err != nil {
			return "reading the upgrade: " + err.Error()
		}
		if line == "\r\n" {
			break
		}
	}
	end := time.Now().Add(lasts)
	for i := 0; time.Now().Before(end); i++ {
		msg := fmt.Sprintf("m%d\n", i)
		_ = conn.SetDeadline(time.Now().Add(5 * time.Second))
		if _, err := io.WriteString(conn, msg); err != nil {
			return fmt.Sprintf("after %d echoes: %v", i, err)
		}
		got, err := br.ReadString('\n')
		if err != nil || got != msg {
			return fmt.Sprintf("after %d echoes: read %q, %v", i, got, err)
		}
		time.Sleep(50 * time.Millisecond)
	}
	return ""
}
