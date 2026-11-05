// SPDX-License-Identifier: AGPL-3.0-only

package logs

import (
	"context"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"
)

// serveGuarded runs a handler behind Guard, with a short stall, as a real server
// does: only a real connection can stall. Browsers read over HTTP/2 and curl over
// HTTP/1.1, which treat a deadline differently, so each test takes both.
func serveGuarded(t *testing.T, h2 bool, stall time.Duration, handler http.HandlerFunc) *httptest.Server {
	t.Helper()
	h := &Handler{options: Options{WriteStall: stall}, pages: map[string]int{}}
	srv := httptest.NewUnstartedServer(h.Guard(handler))
	if h2 {
		srv.EnableHTTP2 = true
		srv.StartTLS()
	} else {
		srv.Start()
	}
	t.Cleanup(srv.CloseClientConnections)
	t.Cleanup(srv.Close)
	return srv
}

func bothProtocols(t *testing.T, test func(t *testing.T, h2 bool)) {
	t.Helper()
	t.Run("HTTP/1.1", func(t *testing.T) { test(t, false) })
	t.Run("HTTP/2", func(t *testing.T) { test(t, true) })
}

// A reader that never reads costs what it holds for only so long. Without this, a
// write blocks for as long as the client stays connected.
func TestAWriteToAReaderThatStoppedGivesUp(t *testing.T) {
	bothProtocols(t, func(t *testing.T, h2 bool) {
		result := make(chan error, 1)
		srv := serveGuarded(t, h2, 300*time.Millisecond, func(w http.ResponseWriter, _ *http.Request) {
			_, err := w.Write(make([]byte, 64<<20))
			result <- err
		})
		req, err := http.NewRequestWithContext(t.Context(), http.MethodGet, srv.URL, http.NoBody)
		if err != nil {
			t.Fatal(err)
		}
		// Asks, and reads nothing of the answer.
		res, err := srv.Client().Do(req)
		if err != nil {
			t.Fatal(err)
		}
		defer res.Body.Close()
		select {
		case err := <-result:
			if err == nil {
				t.Error("a write of 64 MiB to nobody succeeded")
			}
		case <-time.After(20 * time.Second):
			t.Fatal("a write to a reader that stopped never gave up")
		}
	})
}

// A slow reader is still a reader, however long its page takes: only a write that
// makes no progress gives up. This one takes far longer than the stall.
func TestASlowReaderReadsItAll(t *testing.T) {
	bothProtocols(t, func(t *testing.T, h2 bool) {
		// A system's send buffer can take megabytes at once, and only makes room
		// for a write once a third of it has gone, so a step takes longer than it
		// looks. This reads far faster than a page needs, and takes longer than the
		// stall in all.
		const size, stall = 24 << 20, 2 * time.Second
		result := make(chan error, 1)
		srv := serveGuarded(t, h2, stall, func(w http.ResponseWriter, _ *http.Request) {
			_, err := w.Write(make([]byte, size))
			result <- err
		})
		req, err := http.NewRequestWithContext(t.Context(), http.MethodGet, srv.URL, http.NoBody)
		if err != nil {
			t.Fatal(err)
		}
		res, err := srv.Client().Do(req)
		if err != nil {
			t.Fatal(err)
		}
		defer res.Body.Close()
		buf := make([]byte, 256<<10)
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
		if took := time.Since(start); took < stall {
			t.Fatalf("it read in %s, so it says nothing of being slower than the stall", took)
		}
		if err := <-result; err != nil {
			t.Errorf("the write ended with %v", err)
		}
	})
}

// A stream that has nothing to say stays open: a limit on a stalled write isn't a
// limit on a request. HTTP/2 ends a stream at a deadline left behind, write or none.
func TestAQuietStreamOutlivesTheStall(t *testing.T) {
	bothProtocols(t, func(t *testing.T, h2 bool) {
		const stall = 100 * time.Millisecond
		srv := serveGuarded(t, h2, stall, func(w http.ResponseWriter, _ *http.Request) {
			_, _ = w.Write([]byte("first\n"))
			_ = http.NewResponseController(w).Flush()
			time.Sleep(4 * stall)
			_, _ = w.Write([]byte("second\n"))
		})
		req, err := http.NewRequestWithContext(t.Context(), http.MethodGet, srv.URL, http.NoBody)
		if err != nil {
			t.Fatal(err)
		}
		res, err := srv.Client().Do(req)
		if err != nil {
			t.Fatal(err)
		}
		defer res.Body.Close()
		body, err := io.ReadAll(res.Body)
		if err != nil || string(body) != "first\nsecond\n" {
			t.Errorf("read %q, %v", body, err)
		}
	})
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
