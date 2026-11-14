// SPDX-License-Identifier: AGPL-3.0-only

package signin

import (
	"bufio"
	"errors"
	"io"
	"log/slog"
	"net"
	"net/http"
	"strings"
	"sync"
	"testing"
	"time"
)

// Serve answers each request on a connection the proxy keeps, with what the
// handler set, its length said so the next request can follow, and a body past
// what any ask has ends the connection.
func TestServeAnswersOnAConnectionKept(t *testing.T) {
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = ln.Close() })
	go func() {
		_ = Serve(ln, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			switch r.URL.Path {
			case "/route/in":
				w.Header().Set(UserHeader, "mira@example.com")
				w.WriteHeader(http.StatusOK)
			case "/route/out":
				w.Header().Set("Location", "https://auth.example.com/authorize")
				w.WriteHeader(http.StatusFound)
			default:
				_, _ = io.WriteString(w, "not here")
			}
		}), time.Second, slog.New(slog.DiscardHandler))
	}()
	conn, err := net.Dial("tcp", ln.Addr().String())
	if err != nil {
		t.Fatal(err)
	}
	defer conn.Close()
	r := bufio.NewReader(conn)
	// asked is what an answer said, read whole.
	type asked struct {
		status int
		header http.Header
		body   string
		length int64
	}
	ask := func(path, body string) asked {
		t.Helper()
		req, _ := http.NewRequest(http.MethodGet, "http://agent"+path, strings.NewReader(body))
		if err := req.Write(conn); err != nil {
			t.Fatal(err)
		}
		res, err := http.ReadResponse(r, req)
		if err != nil {
			t.Fatalf("%s: %v", path, err)
		}
		defer res.Body.Close()
		b, _ := io.ReadAll(res.Body)
		return asked{status: res.StatusCode, header: res.Header, body: string(b), length: res.ContentLength}
	}
	if res := ask("/route/in", ""); res.status != http.StatusOK || res.header.Get(UserHeader) != "mira@example.com" || res.length != 0 {
		t.Errorf("signed in answers %d %v", res.status, res.header)
	}
	if res := ask("/route/out", ""); res.status != http.StatusFound || res.header.Get("Location") != "https://auth.example.com/authorize" {
		t.Errorf("on the same connection, a navigation answers %d %v", res.status, res.header)
	}
	if res := ask("/other", ""); res.status != http.StatusOK || res.body != "not here" {
		t.Errorf("a body answers %d %q", res.status, res.body)
	}
	_ = ask("/route/in", strings.Repeat("x", maxRequest+10))
	_ = conn.SetReadDeadline(time.Now().Add(2 * time.Second))
	if _, err := r.ReadByte(); err == nil {
		t.Error("after a body past the bound, the connection stays open")
	}
}

// A handler that panics is logged, and its connection closed with no answer, so the
// proxy answers 502 and the agent goes on answering; headers past net/http's bound
// end the connection; and closing the listener closes the connections it kept
// (#598's final check, M1, L1 and L4).
func TestServeSurvivesAPanicAndBoundsWhatItReads(t *testing.T) {
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	said := &lockedWriter{w: &strings.Builder{}}
	logger := slog.New(slog.NewTextHandler(said, nil))
	done := make(chan error, 1)
	go func() {
		done <- Serve(ln, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			if r.URL.Path == "/route/boom" {
				panic("a route that isn't there")
			}
			w.WriteHeader(http.StatusOK)
		}), 2*time.Second, logger)
	}()
	dial := func() (net.Conn, *bufio.Reader) {
		t.Helper()
		conn, err := net.Dial("tcp", ln.Addr().String())
		if err != nil {
			t.Fatal(err)
		}
		t.Cleanup(func() { _ = conn.Close() })
		_ = conn.SetReadDeadline(time.Now().Add(5 * time.Second))
		return conn, bufio.NewReader(conn)
	}
	closed := func(r *bufio.Reader) bool {
		_, err := r.ReadByte()
		return err != nil
	}
	// status reads an answer's status, and its body to the end.
	status := func(r *bufio.Reader) (int, error) {
		res, err := http.ReadResponse(r, nil)
		if err != nil {
			return 0, err
		}
		defer res.Body.Close()
		_, _ = io.Copy(io.Discard, res.Body)
		return res.StatusCode, nil
	}

	conn, r := dial()
	_, _ = io.WriteString(conn, "GET /route/boom HTTP/1.1\r\nHost: agent\r\n\r\n")
	if !closed(r) {
		t.Error("a handler that panics answers")
	}
	if logged := said.String(); !strings.Contains(logged, "a route that isn't there") {
		t.Errorf("the panic isn't logged: %s", logged)
	}
	conn, r = dial()
	_, _ = io.WriteString(conn, "GET /route/in HTTP/1.1\r\nHost: agent\r\n\r\n")
	if got, err := status(r); err != nil || got != http.StatusOK {
		t.Fatalf("after a panic, the next connection gets %d, %v", got, err)
	}

	conn, r = dial()
	_, _ = io.WriteString(conn, "GET /route/in HTTP/1.1\r\nHost: agent\r\nX-Big: "+strings.Repeat("x", maxHeader)+"\r\n\r\n")
	if got, err := status(r); err == nil && got == http.StatusOK {
		t.Error("headers past the bound are answered")
	}

	kept, keptReader := dial()
	_, _ = io.WriteString(kept, "GET /route/in HTTP/1.1\r\nHost: agent\r\n\r\n")
	if got, err := status(keptReader); err != nil || got != http.StatusOK {
		t.Fatalf("a kept connection gets %d, %v", got, err)
	}
	_ = ln.Close()
	if err := <-done; !errors.Is(err, net.ErrClosed) {
		t.Errorf("Serve stops with %v", err)
	}
	if !closed(keptReader) {
		t.Error("a connection kept from before the listener closed stays open")
	}
}

// lockedWriter is a writer a logger and a test share.
type lockedWriter struct {
	mu sync.Mutex
	w  *strings.Builder
}

func (l *lockedWriter) String() string {
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.w.String()
}

func (l *lockedWriter) Write(b []byte) (int, error) {
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.w.Write(b)
}
