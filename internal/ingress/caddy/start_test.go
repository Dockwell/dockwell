// SPDX-License-Identifier: AGPL-3.0-only

package caddy

import (
	"context"
	"net"
	"net/http"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/dockwell/dockwell/internal/ingress"
)

// earlyRefuser is a Caddy that answers on its admin socket before it has its
// config, refusing what isn't there yet, and then exits, as a real one does when a
// port it needs is taken.
type earlyRefuser struct {
	dir     string
	mu      sync.Mutex
	running bool
	asked   int
	srv     *http.Server
}

func (e *earlyRefuser) Paths() Paths {
	return Paths{Data: filepath.Join(e.dir, "data"), Config: filepath.Join(e.dir, "config"), Run: filepath.Join(e.dir, "run"), Boot: filepath.Join(e.dir, bootDirName)}
}

func (e *earlyRefuser) Status(context.Context) (ingress.Status, error) {
	e.mu.Lock()
	defer e.mu.Unlock()
	return ingress.Status{Exists: e.srv != nil, Running: e.running}, nil
}

func (e *earlyRefuser) Pull(context.Context) error { return nil }

func (e *earlyRefuser) Run(ctx context.Context, _ string) error {
	ln, err := (&net.ListenConfig{}).Listen(ctx, "unix", filepath.Join(e.dir, "run", adminSocketName))
	if err != nil {
		return err
	}
	e.mu.Lock()
	defer e.mu.Unlock()
	e.running = true
	e.srv = &http.Server{ReadHeaderTimeout: time.Second, Handler: http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		e.mu.Lock()
		e.asked++
		if e.asked >= 2 {
			e.running = false // it couldn't load its config
		}
		e.mu.Unlock()
		w.WriteHeader(http.StatusBadRequest)
		_, _ = w.Write([]byte(`{"error":"invalid traversal path at: config/apps"}`))
	})}
	go func() { _ = e.srv.Serve(ln) }()
	return nil
}

func (e *earlyRefuser) Abandon(context.Context) error { return e.Remove(context.Background()) }

func (e *earlyRefuser) Remove(context.Context) error {
	e.mu.Lock()
	defer e.mu.Unlock()
	if e.srv != nil {
		_ = e.srv.Close()
	}
	e.running = false
	return nil
}

// A Caddy that refuses what isn't there yet and then exits didn't start, and says
// so, with where to read why. Taking its first refusal for the answer said the
// admin API refused the agent, which a port taken is no part of.
func TestACaddyThatExitsAsItStartsIsntSaidToRefuse(t *testing.T) {
	dir := shortDir(t)
	rt := &earlyRefuser{dir: dir}
	t.Cleanup(func() { _ = rt.Remove(context.Background()) })
	c := &Caddy{Dir: dir, Runtime: rt}
	err := c.Start(t.Context(), nil, false)
	if err == nil || !strings.Contains(err.Error(), "didn't start") || !strings.Contains(err.Error(), "it exited") || strings.Contains(err.Error(), "refused") {
		t.Errorf("Start = %v, want Caddy not starting, as it exited", err)
	}
	rt.mu.Lock()
	defer rt.mu.Unlock()
	if rt.asked < 2 {
		t.Errorf("it asked %d times, so this proves nothing", rt.asked)
	}
}
