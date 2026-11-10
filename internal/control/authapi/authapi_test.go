// SPDX-License-Identifier: AGPL-3.0-only

package authapi

import (
	"errors"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strconv"
	"testing"
	"time"

	"connectrpc.com/connect"

	"github.com/dockwell/dockwell/internal/auth"
)

// Waiting as long as Retry-After says is always enough.
func TestSlowDownRetryAfterRoundsUp(t *testing.T) {
	h := New(Config{Logger: slog.New(slog.DiscardHandler)})
	for wait, want := range map[time.Duration]string{
		300 * time.Millisecond:  "1",
		time.Second:             "1",
		1200 * time.Millisecond: "2",
		15 * time.Minute:        "900",
	} {
		var cerr *connect.Error
		if !errors.As(h.rpcError(t.Context(), &auth.SlowDown{Wait: wait}), &cerr) {
			t.Fatal("not a connect error")
		}
		if got := cerr.Meta().Get("Retry-After"); got != want || cerr.Meta().Get(ReasonKey) != "slow_down" {
			t.Errorf("a wait of %s: Retry-After %q, reason %q, want %q and slow_down", wait, got, cerr.Meta().Get(ReasonKey), want)
		}
	}
}

// On localhost, where the session cookie can't take the __Host- prefix, a request
// that carries it twice is refused rather than one of them chosen, as only a page on
// another origin under localhost, such as an app opened through the panel, could set
// the second (ADR 0061).
func TestGuardRefusesTwoSessionCookiesOnLocalhost(t *testing.T) {
	reached := false
	h := Guard(auth.Sites{}, slog.New(slog.DiscardHandler), http.HandlerFunc(func(http.ResponseWriter, *http.Request) { reached = true }))
	for _, tc := range []struct {
		cookies int
		want    int
	}{{1, http.StatusOK}, {2, http.StatusBadRequest}} {
		reached = false
		r := httptest.NewRequest(http.MethodGet, "/", http.NoBody)
		r.Host, r.RemoteAddr = "localhost:7600", "127.0.0.1:50000"
		for i := range tc.cookies {
			r.AddCookie(&http.Cookie{Name: auth.LoopbackCookieName, Value: "token-" + strconv.Itoa(i)})
		}
		w := httptest.NewRecorder()
		h.ServeHTTP(w, r)
		if w.Code != tc.want || reached != (tc.want == http.StatusOK) || tc.want != http.StatusOK && w.Header().Get(ReasonKey) != ReasonTwoSessions {
			t.Errorf("%d session cookies: %d, reached %t, reason %q", tc.cookies, w.Code, reached, w.Header().Get(ReasonKey))
		}
	}
}
