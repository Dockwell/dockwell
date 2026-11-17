// SPDX-License-Identifier: AGPL-3.0-only

package tunnels

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

func TestReadReady(t *testing.T) {
	answer := func(status int, body string) string {
		srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			if r.URL.Path != "/ready" {
				http.NotFound(w, r)
				return
			}
			w.WriteHeader(status)
			_, _ = w.Write([]byte(body))
		}))
		t.Cleanup(srv.Close)
		return strings.TrimPrefix(srv.URL, "http://")
	}
	ctx := context.Background()

	got, err := ReadReady(ctx, answer(200, `{"status":200,"readyConnections":4,"connectorId":"35b2c1f1-bf24-46cf-96c5-2fe411119f9a"}`), nil)
	if err != nil || got.Connections != 4 || got.ConnectorID != "35b2c1f1-bf24-46cf-96c5-2fe411119f9a" {
		t.Fatalf("a connected cloudflared: %+v %v", got, err)
	}
	// With no connection up yet, cloudflared answers 503 and still says who it is.
	got, err = ReadReady(ctx, answer(503, `{"status":503,"readyConnections":0,"connectorId":"8023cbee-f002-48ee-9639-751cb6f77a44"}`), nil)
	if err != nil || got.Connections != 0 || got.ConnectorID == "" {
		t.Fatalf("a cloudflared still connecting: %+v %v", got, err)
	}
	// Something else on the port isn't taken for cloudflared.
	for _, body := range []string{`ok`, `{"status":200}`, `{"readyConnections":2}`} {
		if _, err := ReadReady(ctx, answer(200, body), nil); !errors.Is(err, ErrNotCloudflared) {
			t.Errorf("%s: %v", body, err)
		}
	}
	// A redirect isn't followed, so nothing else is asked.
	redirect := httptest.NewServer(http.RedirectHandler("http://192.0.2.1/ready", http.StatusFound))
	defer redirect.Close()
	if _, err := ReadReady(ctx, strings.TrimPrefix(redirect.URL, "http://"), nil); !errors.Is(err, ErrNotCloudflared) {
		t.Errorf("a redirect: %v", err)
	}
}
