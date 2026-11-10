// SPDX-License-Identifier: AGPL-3.0-only

package opener_test

import (
	"bufio"
	"context"
	"fmt"
	"io"
	"log/slog"
	"net"
	"net/http"
	"net/http/httptest"
	"net/url"
	"regexp"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/dockwell/dockwell/internal/auth"
	"github.com/dockwell/dockwell/internal/control/opener"
)

var localhost = auth.Site{RPID: "localhost", Origin: "http://localhost:7600"}

// stand is an opener in front of an app that says what it was sent, with a clock, a
// panel session the test moves, and what it follows of each, which the test ends.
type stand struct {
	o       *opener.Opener
	handler http.Handler
	now     time.Time
	mu      sync.Mutex
	live    bool
	seen    []*http.Request
	dialed  []opener.Target
	follows []context.CancelFunc
	// sets are the Set-Cookie lines the app answers with.
	sets []string
}

func newStand(t *testing.T) *stand {
	t.Helper()
	s := &stand{now: time.Date(2026, 11, 10, 12, 0, 0, 0, time.UTC), live: true, sets: []string{
		"phpMyAdmin=app-session; Path=/",
		"spread=1; Domain=localhost; Path=/",
		"dockwell_session=planted; Path=/",
	}}
	app := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("Upgrade") == "echo" {
			echo(w)
			return
		}
		s.mu.Lock()
		s.seen = append(s.seen, r.Clone(context.Background()))
		sets := s.sets
		s.mu.Unlock()
		for _, line := range sets {
			w.Header().Add("Set-Cookie", line)
		}
		_, _ = io.WriteString(w, "the app at "+r.URL.Path)
	}))
	t.Cleanup(app.Close)
	s.o = &opener.Opener{
		Dial: func(ctx context.Context, target opener.Target) (net.Conn, error) {
			s.mu.Lock()
			s.dialed = append(s.dialed, target)
			s.mu.Unlock()
			return (&net.Dialer{}).DialContext(ctx, "tcp", strings.TrimPrefix(app.URL, "http://"))
		},
		Live:   func(context.Context, auth.Client, string) bool { s.mu.Lock(); defer s.mu.Unlock(); return s.live },
		Key:    []byte("a key for the test, of no meaning"),
		Logger: slog.New(slog.DiscardHandler),
		Now:    func() time.Time { s.mu.Lock(); defer s.mu.Unlock(); return s.now },
	}
	s.handler = opener.Dispatch(s.o, http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusTeapot) // the panel
	}))
	return s
}

func (s *stand) advance(d time.Duration) { s.mu.Lock(); s.now = s.now.Add(d); s.mu.Unlock() }

// follow is what a ticket follows, as auth.Service.Follow would, which ends when the
// test calls endFollows: a sign-out, a revoked session or a lost role.
func (s *stand) follow() (context.Context, func()) {
	ctx, cancel := context.WithCancel(context.Background())
	s.mu.Lock()
	s.follows = append(s.follows, cancel)
	s.mu.Unlock()
	return ctx, cancel
}

func (s *stand) endFollows() {
	s.mu.Lock()
	defer s.mu.Unlock()
	for _, cancel := range s.follows {
		cancel()
	}
}

func (s *stand) ticket(t *testing.T, target opener.Target) (link, origin string) {
	t.Helper()
	link, origin, _, err := s.o.Ticket("usr_1", "ses_1", localhost, target, s.follow)
	if err != nil {
		t.Fatal(err)
	}
	return link, origin
}

// echo answers an upgrade to "echo" by saying back each line it's sent, as a
// WebSocket would carry them.
func echo(w http.ResponseWriter) {
	conn, rw, err := http.NewResponseController(w).Hijack()
	if err != nil {
		return
	}
	defer conn.Close()
	_, _ = rw.WriteString("HTTP/1.1 101 Switching Protocols\r\nUpgrade: echo\r\nConnection: Upgrade\r\n\r\n")
	_ = rw.Flush()
	for {
		line, err := rw.ReadString('\n')
		if err != nil {
			return
		}
		_, _ = rw.WriteString("echo " + line)
		_ = rw.Flush()
	}
}

// get sends a request for target to the stand, from this machine, with cookies.
func (s *stand) get(t *testing.T, target string, cookies ...*http.Cookie) *httptest.ResponseRecorder {
	t.Helper()
	u, err := url.Parse(target)
	if err != nil {
		t.Fatal(err)
	}
	r := httptest.NewRequest(http.MethodGet, u.RequestURI(), http.NoBody)
	r.Host = u.Host
	r.RemoteAddr = "127.0.0.1:50000"
	for _, c := range cookies {
		r.AddCookie(c)
	}
	w := httptest.NewRecorder()
	s.handler.ServeHTTP(w, r)
	return w
}

var target = opener.Target{ServiceID: "svc_01phpmyadmin", Service: "phpmyadmin", Project: "env_1", ProjectID: "prj_1", Server: "srv_1", Port: 80}

// A ticket opens the app once, on an origin of its own, as a cookie of that origin,
// and the app gets what a reverse proxy in front of it would send, without the
// panel's cookies, and sets none past its origin or by the panel's names.
func TestATicketOpensTheAppOnAnOriginOfItsOwn(t *testing.T) {
	s := newStand(t)
	link, origin, expires, err := s.o.Ticket("usr_1", "ses_1", localhost, target, s.follow)
	if err != nil {
		t.Fatal(err)
	}
	if !regexp.MustCompile(`^http://[a-z2-7]{10}\.localhost:7600$`).MatchString(origin) || !strings.HasPrefix(link, origin+opener.Path+"?ticket=") {
		t.Fatalf("Ticket = %s at %s", link, origin)
	}
	if !expires.Equal(s.now.Add(time.Minute)) {
		t.Errorf("the ticket works until %v", expires)
	}
	if again, _ := s.ticket(t, target); !strings.HasPrefix(again, origin) {
		t.Errorf("a second ticket for the service has another origin: %s", again)
	}
	opened := s.get(t, link)
	if opened.Code != http.StatusSeeOther || opened.Header().Get("Location") != "/" {
		t.Fatalf("opening = %d to %q", opened.Code, opened.Header().Get("Location"))
	}
	cookie := opened.Result().Cookies()[0]
	if cookie.Name != opener.CookieName || !cookie.HttpOnly || cookie.Domain != "" || cookie.SameSite != http.SameSiteLaxMode {
		t.Fatalf("the cookie is %+v", cookie)
	}
	if again := s.get(t, link); again.Code != http.StatusForbidden {
		t.Errorf("a ticket opened a second time: %d", again.Code)
	}
	w := s.get(t, origin+"/index.php", cookie, &http.Cookie{Name: auth.LoopbackCookieName, Value: "the-panels"}, &http.Cookie{Name: "pma", Value: "kept"})
	if w.Code != http.StatusOK || w.Body.String() != "the app at /index.php" {
		t.Fatalf("the app answered %d: %s", w.Code, w.Body)
	}
	s.mu.Lock()
	sent := s.seen[0]
	dialed := s.dialed
	s.mu.Unlock()
	if sent.Host != strings.TrimPrefix(origin, "http://") || sent.Header.Get("X-Forwarded-Host") != sent.Host || sent.Header.Get("X-Forwarded-Proto") != "http" {
		t.Errorf("the app was sent Host %q, X-Forwarded-Host %q, X-Forwarded-Proto %q", sent.Host, sent.Header.Get("X-Forwarded-Host"), sent.Header.Get("X-Forwarded-Proto"))
	}
	if got := sent.Header.Get("Cookie"); got != "pma=kept" {
		t.Errorf("the app was sent the cookies %q", got)
	}
	if len(dialed) != 1 || dialed[0] != target {
		t.Errorf("it dialed %v", dialed)
	}
	var set []string
	for _, c := range w.Result().Cookies() {
		set = append(set, c.Name)
	}
	if strings.Join(set, ",") != "phpMyAdmin" {
		t.Errorf("the app set the cookies %v", set)
	}
}

// What doesn't open: a panel away from localhost, a ticket past its minute or for
// another app's origin, no cookie, and a request that isn't from this machine.
func TestWhatAnOpenerRefuses(t *testing.T) {
	s := newStand(t)
	if _, _, _, err := s.o.Ticket("usr_1", "ses_1", auth.Site{RPID: "panel.example.com", Origin: "https://panel.example.com", Secure: true}, target, s.follow); err == nil {
		t.Error("a panel on its domain issued a ticket")
	}
	link, origin := s.ticket(t, target)
	s.advance(2 * time.Minute)
	if w := s.get(t, link); w.Code != http.StatusForbidden {
		t.Errorf("a ticket past its minute opened: %d", w.Code)
	}
	link, _ = s.ticket(t, target)
	other := opener.Target{ServiceID: "svc_01grafana", Service: "grafana", Server: "srv_1", Port: 3000}
	_, otherOrigin := s.ticket(t, other)
	if w := s.get(t, otherOrigin+strings.TrimPrefix(link, origin)); w.Code != http.StatusForbidden {
		t.Errorf("a ticket opened another app's origin: %d", w.Code)
	}
	if w := s.get(t, origin+"/"); w.Code != http.StatusUnauthorized {
		t.Errorf("no cookie got %d", w.Code)
	}
	r := httptest.NewRequest(http.MethodGet, "/", http.NoBody)
	r.Host = strings.TrimPrefix(origin, "http://")
	r.RemoteAddr = "203.0.113.9:40000"
	w := httptest.NewRecorder()
	s.handler.ServeHTTP(w, r)
	if w.Code != http.StatusTeapot {
		t.Errorf("a request from elsewhere for an app's origin went to the opener: %d", w.Code)
	}
	first, second := s.o.Label(target.ServiceID), s.o.Label(target.ServiceID)
	if first != second || first == s.o.Label(other.ServiceID) {
		t.Error("labels aren't one for each service")
	}
}

// What was opened closes at once when what it follows ends, its panel session
// signed out or revoked, or the role that let it gone; and within a check of the
// panel session, as each request counts as a use of it, once that has expired.
func TestAnOpenedAppEndsWithItsSessionOrRole(t *testing.T) {
	t.Run("followed", func(t *testing.T) {
		s := newStand(t)
		link, origin := s.ticket(t, target)
		cookie := s.get(t, link).Result().Cookies()[0]
		if w := s.get(t, origin+"/", cookie); w.Code != http.StatusOK {
			t.Fatalf("it didn't open: %d", w.Code)
		}
		s.endFollows()
		if w := s.get(t, origin+"/", cookie); w.Code != http.StatusUnauthorized {
			t.Errorf("it stayed open once its session or role ended: %d", w.Code)
		}
	})
	t.Run("expired", func(t *testing.T) {
		s := newStand(t)
		link, origin := s.ticket(t, target)
		cookie := s.get(t, link).Result().Cookies()[0]
		s.mu.Lock()
		s.live = false
		s.mu.Unlock()
		if w := s.get(t, origin+"/", cookie); w.Code != http.StatusOK {
			t.Errorf("it closed before its next check: %d", w.Code)
		}
		s.advance(31 * time.Second)
		if w := s.get(t, origin+"/", cookie); w.Code != http.StatusUnauthorized {
			t.Errorf("it stayed open: %d", w.Code)
		}
	})
	t.Run("ended before it opened", func(t *testing.T) {
		s := newStand(t)
		ended := func() (context.Context, func()) {
			ctx, cancel := context.WithCancel(context.Background())
			cancel()
			return ctx, cancel
		}
		link, _, _, _ := s.o.Ticket("usr_1", "ses_1", localhost, target, ended)
		if w := s.get(t, link); w.Code != http.StatusForbidden {
			t.Errorf("a ticket opened for a session that had ended: %d", w.Code)
		}
	})
}

// An upgraded connection, such as a WebSocket, closes when what its session follows
// ends, rather than outlive a sign-out or a lost role.
func TestAnUpgradedConnectionEndsWithItsSession(t *testing.T) {
	s := newStand(t)
	front := httptest.NewServer(s.handler)
	defer front.Close()
	addr := strings.TrimPrefix(front.URL, "http://")
	link, origin := s.ticket(t, target)
	host := strings.TrimPrefix(origin, "http://")
	cookie := s.get(t, link).Result().Cookies()[0]

	conn, err := net.Dial("tcp", addr)
	if err != nil {
		t.Fatal(err)
	}
	defer conn.Close()
	_, _ = fmt.Fprintf(conn, "GET /ws HTTP/1.1\r\nHost: %s\r\nCookie: %s=%s\r\nConnection: Upgrade\r\nUpgrade: echo\r\n\r\n", host, cookie.Name, cookie.Value)
	br := bufio.NewReader(conn)
	if status, _ := br.ReadString('\n'); !strings.Contains(status, "101") {
		t.Fatalf("the upgrade answered %q", status)
	}
	for {
		if l, err := br.ReadString('\n'); err != nil || l == "\r\n" {
			break
		}
	}
	say := func(what string) (string, error) {
		_, _ = fmt.Fprintln(conn, what)
		_ = conn.SetReadDeadline(time.Now().Add(5 * time.Second))
		got, err := br.ReadString('\n')
		return strings.TrimSpace(got), err
	}
	if got, err := say("one"); got != "echo one" {
		t.Fatalf("the open connection answered %q, %v", got, err)
	}
	s.endFollows()
	if got, err := say("two"); err == nil {
		t.Errorf("the upgraded connection outlived its session: it answered %q", got)
	}
}

// The browser's cookies reach the app as they were sent, and the app's reach the
// browser as it wrote them, a value with a comma, a space or JSON in it and one for
// its own host by Domain too; what isn't its own host's, or is Dockwell's, doesn't.
func TestCookiesPassAsTheyAre(t *testing.T) {
	s := newStand(t)
	link, origin := s.ticket(t, target)
	host := strings.TrimPrefix(origin, "http://")
	label, _, _ := strings.Cut(host, ":")
	s.mu.Lock()
	s.sets = []string{
		`json={"a":1}; Path=/`,
		`spaced=a b; Path=/`,
		"own=1; Domain=" + label + "; Path=/",
		"dotted=1; Path=/; Domain=." + label,
		"ok=1; Path=/; HttpOnly",
		"spread=1; Domain=localhost; Path=/",
		"other=1; Domain=abcdefghij.localhost",
		"dockwell_session=planted; Path=/",
		"__Host-dockwell_session=planted; Path=/; Secure",
		"dockwell_open=planted; Path=/",
	}
	s.mu.Unlock()
	cookie := s.get(t, link).Result().Cookies()[0]
	r := httptest.NewRequest(http.MethodGet, "/", http.NoBody)
	r.Host = host
	r.RemoteAddr = "127.0.0.1:50000"
	r.Header.Set("Cookie", `list=a,b; spaced=a b; dockwell_session=the-panels; json={"a":1}; quoted="q"; `+cookie.Name+"="+cookie.Value+"; wp-settings-1=libraryContent%3Dbrowse")
	w := httptest.NewRecorder()
	s.handler.ServeHTTP(w, r)
	if w.Code != http.StatusOK {
		t.Fatalf("the app answered %d: %s", w.Code, w.Body)
	}
	s.mu.Lock()
	sent := s.seen[len(s.seen)-1].Header.Values("Cookie")
	s.mu.Unlock()
	if want := `list=a,b; spaced=a b; json={"a":1}; quoted="q"; wp-settings-1=libraryContent%3Dbrowse`; strings.Join(sent, " | ") != want {
		t.Errorf("the app was sent Cookie %q, want %q", sent, want)
	}
	want := []string{`json={"a":1}; Path=/`, `spaced=a b; Path=/`, "own=1; Domain=" + label + "; Path=/", "dotted=1; Path=/; Domain=." + label, "ok=1; Path=/; HttpOnly"}
	if got := w.Header().Values("Set-Cookie"); strings.Join(got, " | ") != strings.Join(want, " | ") {
		t.Errorf("the browser was sent Set-Cookie %q, want %q", got, want)
	}
}
