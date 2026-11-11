// SPDX-License-Identifier: AGPL-3.0-only

package registry

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"runtime"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

// toBase sends every request to a stand-in, whatever host it names.
type toBase struct{ base *url.URL }

func (d toBase) Do(req *http.Request) (*http.Response, error) {
	req.URL.Scheme, req.URL.Host = d.base.Scheme, d.base.Host
	return http.DefaultClient.Do(req)
}

func standIn(t *testing.T, h http.Handler) *Client {
	t.Helper()
	srv := httptest.NewServer(h)
	t.Cleanup(srv.Close)
	base, _ := url.Parse(srv.URL)
	return New(Options{Secure: toBase{base}, Plain: toBase{base}, Public: toBase{base}})
}

// A registry that lists page after page of tags, each 4 MiB of short names,
// costs a read two pages, a few MiB of heap, and less than its time.
func TestTagListsAreBounded(t *testing.T) {
	var page strings.Builder
	page.WriteString(`{"name":"acme/web","tags":[`)
	for i := 0; page.Len() < 4<<20; i++ {
		if i > 0 {
			page.WriteByte(',')
		}
		fmt.Fprintf(&page, `"%x"`, i%4096)
	}
	page.WriteString(`]}`)
	body := []byte(page.String())
	var pages atomic.Int32
	c := standIn(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if strings.HasSuffix(r.URL.Path, "/tags/list") {
			n := pages.Add(1)
			w.Header().Set("Link", fmt.Sprintf(`</v2/acme/web/tags/list?last=%d&n=1000>; rel="next"`, n))
			_, _ = w.Write(body)
			return
		}
		http.NotFound(w, r)
	}))

	runtime.GC()
	var before runtime.MemStats
	runtime.ReadMemStats(&before)
	var peak atomic.Uint64
	stop := make(chan struct{})
	var watching sync.WaitGroup
	watching.Go(func() {
		var m runtime.MemStats
		for {
			select {
			case <-stop:
				return
			case <-time.After(2 * time.Millisecond):
			}
			runtime.ReadMemStats(&m)
			if m.HeapInuse > peak.Load() {
				peak.Store(m.HeapInuse)
			}
		}
	})
	start := time.Now()
	ctx, cancel := context.WithTimeout(context.Background(), 8*time.Second)
	defer cancel()
	tags, err := c.Tags(ctx, Ref{Registry: "registry.example.com", Repository: "acme/web"})
	close(stop)
	watching.Wait()
	took := time.Since(start)
	if err != nil {
		t.Fatal(err)
	}
	if got := pages.Load(); got != maxTagNames/tagsPerPage {
		t.Errorf("read %d pages, want %d", got, maxTagNames/tagsPerPage)
	}
	if len(tags) == 0 || len(tags) > MaxTags {
		t.Errorf("listed %d tags", len(tags))
	}
	if grew := int64(peak.Load()) - int64(before.HeapInuse); grew > 32<<20 {
		t.Errorf("the heap grew by %d MiB as it read", grew>>20)
	}
	if took > 4*time.Second {
		t.Errorf("listing took %s", took)
	}
}

// Pages that come slowly are read until the read's time is up, and what came
// stands.
func TestTagListsStopAtTheDeadline(t *testing.T) {
	var pages atomic.Int32
	c := standIn(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if strings.HasSuffix(r.URL.Path, "/tags/list") {
			n := pages.Add(1)
			time.Sleep(300 * time.Millisecond)
			w.Header().Set("Link", fmt.Sprintf(`</v2/acme/web/tags/list?last=%d&n=1000>; rel="next"`, n))
			_, _ = fmt.Fprintf(w, `{"tags":["1.%d"]}`, n)
			return
		}
		http.NotFound(w, r)
	}))
	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()
	start := time.Now()
	tags, err := c.Tags(ctx, Ref{Registry: "registry.example.com", Repository: "acme/web"})
	if took := time.Since(start); took > 2*time.Second {
		t.Errorf("listing took %s with a second to do it in", took)
	}
	if err != nil || len(tags) == 0 {
		t.Errorf("Tags = %v, %v; want what came in time", tags, err)
	}
}

// A registry sends Dockwell nowhere but its own /v2/ and its realm, and to the
// server's own networks only from there: not a token's realm, nor a manifest
// that moves, nor a blob kept at a metadata service.
func TestRegistriesSendDockwellNowhereElse(t *testing.T) {
	var mu sync.Mutex
	var seen []string
	inner := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		seen = append(seen, r.Method+" "+r.URL.RequestURI())
		mu.Unlock()
		http.NotFound(w, r)
	}))
	defer inner.Close()
	outer := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case strings.Contains(r.URL.Path, "/manifests/realm"):
			w.Header().Set("Www-Authenticate", `Bearer realm="`+inner.URL+`/admin/flush?all=1",service="x"`)
			w.WriteHeader(http.StatusUnauthorized)
		case strings.Contains(r.URL.Path, "/manifests/moved"):
			w.Header().Set("Location", inner.URL+"/v1/kv/secret?recurse=true")
			w.WriteHeader(http.StatusTemporaryRedirect)
		case strings.Contains(r.URL.Path, "/manifests/here"):
			// Even on its own host, a manifest isn't followed.
			w.Header().Set("Location", "/v2/acme/web/manifests/elsewhere")
			w.WriteHeader(http.StatusTemporaryRedirect)
		case strings.Contains(r.URL.Path, "/manifests/"):
			w.Header().Set("Docker-Content-Digest", "sha256:"+strings.Repeat("1", 64))
			_, _ = fmt.Fprintf(w, `{"config":{"digest":"sha256:%s","size":10}}`, strings.Repeat("2", 64))
		case strings.Contains(r.URL.Path, "/blobs/"):
			w.Header().Set("Location", "http://169.254.169.254/latest/meta-data/")
			w.WriteHeader(http.StatusTemporaryRedirect)
		default:
			http.NotFound(w, r)
		}
	}))
	defer outer.Close()
	host := strings.TrimPrefix(outer.URL, "http://")
	// The clients a server's agent uses, and loopback over plain HTTP, as Docker
	// takes 127.0.0.0/8 to be insecure.
	c := New(Options{Insecure: func(string) bool { return true }})
	ctx := context.Background()
	for _, tag := range []string{"realm", "moved", "here"} {
		_, err := c.Head(ctx, Ref{Registry: host, Repository: "acme/web", Tag: tag})
		if !errors.Is(err, ErrRefused) {
			t.Errorf("a registry that sends Dockwell elsewhere (%s) = %v, want ErrRefused", tag, err)
		}
		if errors.Is(err, ErrDown) {
			t.Errorf("where a registry sends Dockwell reads as the registry being down (%s)", tag)
		}
	}
	if _, _, err := c.Config(ctx, Ref{Registry: host, Repository: "acme/web", Tag: "1"}, Platform{OS: "linux", Architecture: "amd64"}); !errors.Is(err, ErrRefused) {
		t.Errorf("a blob kept at the metadata service = %v, want ErrRefused", err)
	}
	if _, err := New(Options{}).Head(ctx, Ref{Registry: "169.254.169.254", Repository: "acme/web", Tag: "1"}); !errors.Is(err, ErrRefused) {
		t.Errorf("a registry at the metadata service = %v, want ErrRefused", err)
	}
	mu.Lock()
	defer mu.Unlock()
	if len(seen) > 0 {
		t.Errorf("the server's own network saw %q", seen)
	}
}

// A config is a few kilobytes: one larger than MaxConfig isn't read, whatever
// its manifest says of it.
func TestConfigsAreSmall(t *testing.T) {
	for _, tc := range []struct{ said, sent int }{{MaxConfig + 1, 10}, {100, MaxConfig + 1}} {
		c := standIn(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			switch {
			case strings.Contains(r.URL.Path, "/manifests/"):
				_, _ = fmt.Fprintf(w, `{"config":{"digest":"sha256:%s","size":%d}}`, strings.Repeat("2", 64), tc.said)
			case strings.Contains(r.URL.Path, "/blobs/"):
				_, _ = w.Write([]byte(`{"config":{"Env":["A=` + strings.Repeat("x", tc.sent) + `"]}}`))
			}
		}))
		_, _, err := c.Config(context.Background(), Ref{Registry: "registry.example.com", Repository: "acme/web", Tag: "1"}, Platform{OS: "linux", Architecture: "amd64"})
		if !errors.Is(err, ErrTooLarge) || errors.Is(err, ErrDown) {
			t.Errorf("a config said to be %d bytes and sent as %d = %v, want ErrTooLarge", tc.said, tc.sent, err)
		}
	}
}

// What tokens take stays within tokenBytes, the oldest going first.
func TestTokensAreKeptWithinABudget(t *testing.T) {
	c := New(Options{})
	now := time.Now()
	for i := range 200 {
		c.keepToken(fmt.Sprintf("https://registry.example.com acme/app%d", i), token{value: strings.Repeat("t", 4<<10), until: now.Add(time.Minute), at: now.Add(time.Duration(i))})
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.held > tokenBytes {
		t.Errorf("tokens take %d bytes, more than %d", c.held, tokenBytes)
	}
	if _, ok := c.tokens["https://registry.example.com acme/app199"]; !ok {
		t.Errorf("the newest token went first")
	}
}

// Only a registry that didn't answer is down: an odd answer says something of
// one image, and a mirror that answers keeps Docker Hub from reading as down.
func TestErrorsSayWhetherARegistryAnswered(t *testing.T) {
	closed := httptest.NewServer(http.NotFoundHandler())
	closedURL, _ := url.Parse(closed.URL)
	closed.Close()
	if _, err := New(Options{Secure: toBase{closedURL}}).Head(context.Background(), Ref{Registry: "registry.example.com", Repository: "acme/web", Tag: "1"}); !errors.Is(err, ErrDown) {
		t.Errorf("a registry that took no connection = %v, want ErrDown", err)
	}
	broken := standIn(t, http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		http.Error(w, "broken", http.StatusInternalServerError)
	}))
	if _, err := broken.Head(context.Background(), Ref{Registry: "registry.example.com", Repository: "acme/web", Tag: "1"}); !errors.Is(err, ErrUnreachable) || errors.Is(err, ErrDown) {
		t.Errorf("a registry that answered 500 = %v, want ErrUnreachable and not ErrDown", err)
	}
	// Docker Hub, through a mirror that answers and a registry that doesn't.
	hub := doerFunc(func(req *http.Request) (*http.Response, error) {
		if req.URL.Host == "mirror.example.com" {
			return &http.Response{StatusCode: http.StatusNotFound, Body: io.NopCloser(strings.NewReader("")), Header: http.Header{}}, nil
		}
		return nil, errors.New("dial tcp: connection refused")
	})
	c := New(Options{Mirrors: []string{"https://mirror.example.com"}, Secure: hub, Plain: hub, Public: hub})
	if _, err := c.Head(context.Background(), Ref{Registry: Hub, Repository: "library/nginx", Tag: "1"}); !errors.Is(err, ErrNotFound) {
		t.Errorf("Docker Hub through a mirror that answered = %v, want the mirror's ErrNotFound", err)
	}
}

type doerFunc func(*http.Request) (*http.Response, error)

func (f doerFunc) Do(req *http.Request) (*http.Response, error) { return f(req) }
