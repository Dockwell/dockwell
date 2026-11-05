// SPDX-License-Identifier: AGPL-3.0-only

package logs

import (
	"context"
	"errors"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"
)

// serveGuarded runs a handler behind Guard, with a short stall, as a real server
// does: only a real connection can stall.
func serveGuarded(t *testing.T, stall time.Duration, handler http.HandlerFunc) *httptest.Server {
	t.Helper()
	h := &Handler{options: Options{WriteStall: stall}, pages: map[string]int{}}
	srv := httptest.NewServer(h.Guard(handler))
	t.Cleanup(srv.CloseClientConnections)
	t.Cleanup(srv.Close)
	return srv
}

// A reader that never reads costs what it holds for only so long. Without this, a
// write blocks for as long as the client stays connected.
func TestAWriteToAReaderThatStoppedGivesUp(t *testing.T) {
	result := make(chan error, 1)
	srv := serveGuarded(t, 300*time.Millisecond, func(w http.ResponseWriter, _ *http.Request) {
		_, err := w.Write(make([]byte, 64<<20))
		result <- err
	})
	conn, err := net.Dial("tcp", srv.Listener.Addr().String())
	if err != nil {
		t.Fatal(err)
	}
	defer conn.Close()
	_ = conn.(*net.TCPConn).SetReadBuffer(4096)
	if _, err := conn.Write([]byte("GET / HTTP/1.1\r\nHost: x\r\n\r\n")); err != nil {
		t.Fatal(err)
	}
	select {
	case err := <-result:
		if err == nil {
			t.Error("a write of 64 MiB to nobody succeeded")
		}
	case <-time.After(10 * time.Second):
		t.Fatal("a write to a reader that stopped never gave up")
	}
}

// A slow reader is still a reader, however long its page takes: only a write that
// makes no progress gives up.
func TestASlowReaderReadsItAll(t *testing.T) {
	const size = 2 << 20
	result := make(chan error, 1)
	srv := serveGuarded(t, 500*time.Millisecond, func(w http.ResponseWriter, _ *http.Request) {
		_, err := w.Write(make([]byte, size))
		result <- err
	})
	res, err := http.Get(srv.URL)
	if err != nil {
		t.Fatal(err)
	}
	defer res.Body.Close()
	buf := make([]byte, 128<<10)
	total := 0
	start := time.Now()
	for {
		n, err := res.Body.Read(buf)
		total += n
		if errors.Is(err, io.EOF) {
			break
		}
		if err != nil {
			t.Fatalf("after %d bytes: %v", total, err)
		}
		time.Sleep(50 * time.Millisecond)
	}
	if total != size {
		t.Errorf("read %d of %d bytes", total, size)
	}
	if took := time.Since(start); took < 500*time.Millisecond {
		t.Fatalf("it read in %s, so it says nothing of being slower than the stall", took)
	}
	if err := <-result; err != nil {
		t.Errorf("the write ended with %v", err)
	}
}

// A stream that has nothing to say stays open: a limit on a stalled write isn't a
// limit on a request.
func TestAQuietStreamOutlivesTheStall(t *testing.T) {
	const stall = 100 * time.Millisecond
	srv := serveGuarded(t, stall, func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write([]byte("first\n"))
		_ = http.NewResponseController(w).Flush()
		time.Sleep(4 * stall)
		_, _ = w.Write([]byte("second\n"))
	})
	res, err := http.Get(srv.URL)
	if err != nil {
		t.Fatal(err)
	}
	defer res.Body.Close()
	body, err := io.ReadAll(res.Body)
	if err != nil || string(body) != "first\nsecond\n" {
		t.Errorf("read %q, %v", body, err)
	}
}

// One reader has only so many pages at once, and all of them together only so many,
// and a page holds its place until it has been written.
func TestPagesAreLimited(t *testing.T) {
	h := &Handler{options: Options{WriteStall: time.Second}, pages: map[string]int{}}
	taken := func(who string) (func(), error) {
		held := &pageHold{}
		ctx := context.WithValue(context.Background(), pageHoldKey{}, held)
		if err := h.takePageFor(ctx, who); err != nil {
			return nil, err
		}
		return held.release, nil
	}
	var releases []func()
	for range maxPagesPerReader {
		release, err := taken("token:a")
		if err != nil {
			t.Fatal(err)
		}
		releases = append(releases, release)
	}
	if _, err := taken("token:a"); err == nil {
		t.Errorf("a reader had %d pages at once", maxPagesPerReader+1)
	}
	for range maxPages - maxPagesPerReader {
		release, err := taken("token:b" + time.Now().String())
		if err != nil {
			t.Fatal(err)
		}
		releases = append(releases, release)
	}
	if _, err := taken("token:c"); err == nil {
		t.Errorf("there were %d pages at once", maxPages+1)
	}
	for _, release := range releases {
		release()
	}
	if len(h.pages) != 0 {
		t.Errorf("after they were written, %v", h.pages)
	}
	if _, err := taken("token:c"); err != nil {
		t.Errorf("after they were written, a page = %v", err)
	}
}
