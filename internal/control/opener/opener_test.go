// SPDX-License-Identifier: AGPL-3.0-only

package opener_test

import (
	"context"
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
	"github.com/dockwell/dockwell/internal/store"
)

var localhost = auth.Site{RPID: "localhost", Origin: "http://localhost:7600"}

// stand is an opener in front of an app that says what it was sent, with a clock and
// a panel session the test moves.
type stand struct {
	o       *opener.Opener
	handler http.Handler
	now     time.Time
	mu      sync.Mutex
	live    bool
	role    string
	seen    []*http.Request
	dialed  []opener.Target
}

func newStand(t *testing.T) *stand {
	t.Helper()
	s := &stand{now: time.Date(2026, 11, 10, 12, 0, 0, 0, time.UTC), live: true, role: store.RoleDeveloper}
	app := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		s.mu.Lock()
		s.seen = append(s.seen, r.Clone(context.Background()))
		s.mu.Unlock()
		http.SetCookie(w, &http.Cookie{Name: "phpMyAdmin", Value: "app-session", Path: "/"})
		w.Header().Add("Set-Cookie", "spread=1; Domain=localhost; Path=/")
		w.Header().Add("Set-Cookie", "dockwell_session=planted; Path=/")
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
		Live: func(context.Context, auth.Client, string) bool { s.mu.Lock(); defer s.mu.Unlock(); return s.live },
		Role: func(context.Context, string, string) (string, error) {
			s.mu.Lock()
			defer s.mu.Unlock()
			return s.role, nil
		},
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
	link, origin, expires, err := s.o.Ticket("usr_1", "ses_1", localhost, target)
	if err != nil {
		t.Fatal(err)
	}
	if !regexp.MustCompile(`^http://[a-z2-7]{10}\.localhost:7600$`).MatchString(origin) || !strings.HasPrefix(link, origin+opener.Path+"?ticket=") {
		t.Fatalf("Ticket = %s at %s", link, origin)
	}
	if !expires.Equal(s.now.Add(time.Minute)) {
		t.Errorf("the ticket works until %v", expires)
	}
	if again, _, _, _ := s.o.Ticket("usr_1", "ses_1", localhost, target); !strings.HasPrefix(again, origin) {
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
	if _, _, _, err := s.o.Ticket("usr_1", "ses_1", auth.Site{RPID: "panel.example.com", Origin: "https://panel.example.com", Secure: true}, target); err == nil {
		t.Error("a panel on its domain issued a ticket")
	}
	link, origin, _, _ := s.o.Ticket("usr_1", "ses_1", localhost, target)
	s.advance(2 * time.Minute)
	if w := s.get(t, link); w.Code != http.StatusForbidden {
		t.Errorf("a ticket past its minute opened: %d", w.Code)
	}
	link, _, _, _ = s.o.Ticket("usr_1", "ses_1", localhost, target)
	other := opener.Target{ServiceID: "svc_01grafana", Service: "grafana", Server: "srv_1", Port: 3000}
	_, otherOrigin, _, _ := s.o.Ticket("usr_1", "ses_1", localhost, other)
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

// What was opened closes with the panel session that opened it, and with the role
// that let it, as checked every 30 s.
func TestAnOpenedAppEndsWithItsSessionOrRole(t *testing.T) {
	for _, end := range []string{"session", "role"} {
		t.Run(end, func(t *testing.T) {
			s := newStand(t)
			link, origin, _, _ := s.o.Ticket("usr_1", "ses_1", localhost, target)
			cookie := s.get(t, link).Result().Cookies()[0]
			if w := s.get(t, origin+"/", cookie); w.Code != http.StatusOK {
				t.Fatalf("it didn't open: %d", w.Code)
			}
			s.mu.Lock()
			if end == "session" {
				s.live = false
			} else {
				s.role = store.RoleViewer
			}
			s.mu.Unlock()
			if w := s.get(t, origin+"/", cookie); w.Code != http.StatusOK {
				t.Errorf("it closed before its next check: %d", w.Code)
			}
			s.advance(31 * time.Second)
			if w := s.get(t, origin+"/", cookie); w.Code != http.StatusUnauthorized {
				t.Errorf("it stayed open: %d", w.Code)
			}
		})
	}
}
