// SPDX-License-Identifier: AGPL-3.0-only

package terminal

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	agentv1 "github.com/dockwell/dockwell/gen/dockwell/agent/v1"
	dockwellv1 "github.com/dockwell/dockwell/gen/dockwell/v1"
	"github.com/dockwell/dockwell/internal/docker"
	"github.com/dockwell/dockwell/internal/docker/dockertest"
)

// A Docker check can hold startup across a timeout tick, before a shell exists.
func startingManager(t *testing.T, checkErr error) (*Manager, <-chan struct{}, func()) {
	t.Helper()
	e := dockertest.New(t)
	e.AddLocalImage("app:1", dockertest.Image{Shell: "/bin/sh"})
	e.AddContainer(docker.Container{Name: "/app", Config: docker.Config{Image: "app:1"}})
	d := e.Client()
	t.Cleanup(d.Close)
	entered, release := make(chan struct{}, MaxOpen), make(chan struct{})
	unblock := sync.OnceFunc(func() { close(release) })
	m := New(Options{
		Docker: d, Shell: []string{"/bin/sh"}, Home: t.TempDir(),
		AsGoodAsRoot: func(ctx context.Context, _ string) (bool, error) {
			entered <- struct{}{}
			select {
			case <-release:
				return false, checkErr
			case <-ctx.Done():
				return false, ctx.Err()
			}
		},
	})
	t.Cleanup(func() { unblock(); m.closeAll() })
	return m, entered, unblock
}

func startingOpen(t *testing.T, m *Manager, id string) <-chan error {
	t.Helper()
	done := make(chan error, 1)
	go func() {
		_, err := m.Open(t.Context(), &agentv1.OpenTerminalCall{Id: id, Container: "app", Columns: 80, Rows: 24})
		done <- err
	}()
	return done
}

func waitForStartup(t *testing.T, entered <-chan struct{}) {
	t.Helper()
	select {
	case <-entered:
	case <-time.After(10 * time.Second):
		t.Fatal("the shell didn't start opening")
	}
}

func openingResult(t *testing.T, done <-chan error) error {
	t.Helper()
	select {
	case err := <-done:
		return err
	case <-time.After(10 * time.Second):
		t.Fatal("opening the shell didn't finish")
		return nil
	}
}

func TestATerminalStillOpeningDoesNotExpire(t *testing.T) {
	m, entered, release := startingManager(t, nil)
	done := startingOpen(t, m, "trm_1")
	waitForStartup(t, entered)
	if got := m.Known(); len(got) != 0 {
		t.Fatalf("reported terminals before their shells started: %v", got)
	}
	// Its timestamps and process aren't set yet. A tick must leave it alone.
	m.expire()
	if _, err := m.Close("trm_1", dockwellv1.TerminalEndReason_TERMINAL_END_REASON_CLOSED); !errors.Is(err, ErrNotFound) {
		t.Fatalf("closing a shell still opening = %v", err)
	}
	release()
	if err := openingResult(t, done); err != nil {
		t.Fatal(err)
	}
	m.expire()
	s := attach(t, m, 0)
	s.typed("echo started-$((1))\n")
	s.waitFor("started-1\r\n")
}

func TestStartingTerminalsKeepTheirPlaces(t *testing.T) {
	m, entered, release := startingManager(t, nil)
	var done []<-chan error
	for i := range MaxOpen {
		done = append(done, startingOpen(t, m, fmt.Sprintf("trm_%d", i)))
		waitForStartup(t, entered)
	}
	for _, id := range []string{"trm_0", "trm_extra"} {
		if _, err := m.Open(t.Context(), &agentv1.OpenTerminalCall{Id: id}); err == nil {
			t.Fatalf("opened %s while all places were taken", id)
		}
	}
	release()
	for _, one := range done {
		if err := openingResult(t, one); err != nil {
			t.Fatal(err)
		}
	}
	if got := m.Known(); len(got) != MaxOpen {
		t.Fatalf("reported %d terminals after opening %d", len(got), MaxOpen)
	}
}

func TestFailedStartupReleasesItsPlace(t *testing.T) {
	want := errors.New("the container check failed")
	m, entered, release := startingManager(t, want)
	done := startingOpen(t, m, "trm_1")
	waitForStartup(t, entered)
	release()
	if err := openingResult(t, done); !errors.Is(err, want) {
		t.Fatalf("opening = %v, want %v", err, want)
	}
	m.expire()
	if got := m.Known(); len(got) != 0 {
		t.Fatalf("reported a shell whose startup failed: %v", got)
	}
	for i := range MaxOpen {
		open(t, m, fmt.Sprintf("trm_%d", i))
	}
}

func TestStoppingWaitsForTerminalStartup(t *testing.T) {
	m, entered, release := startingManager(t, nil)
	opened := startingOpen(t, m, "trm_starting")
	waitForStartup(t, entered)
	stopped := make(chan struct{})
	go func() { m.closeAll(); close(stopped) }()
	// Wait for shutdown to stop accepting opens, while Docker still holds one.
	deadline := time.Now().Add(10 * time.Second)
	for {
		m.mu.Lock()
		stopping := m.stopping
		m.mu.Unlock()
		if stopping {
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("the manager didn't start stopping")
		}
		time.Sleep(time.Millisecond)
	}
	select {
	case <-stopped:
		t.Fatal("the manager stopped with a shell still opening")
	default:
	}
	if _, err := m.Open(t.Context(), &agentv1.OpenTerminalCall{Id: "trm_extra"}); err == nil {
		t.Fatal("opened another terminal while stopping")
	}
	release()
	if err := openingResult(t, opened); err != nil {
		t.Fatal(err)
	}
	select {
	case <-stopped:
	case <-time.After(10 * time.Second):
		t.Fatal("the manager didn't stop after startup finished")
	}
	if end := takeEnded(t, m, "trm_starting").GetEnd(); end.GetReason() != dockwellv1.TerminalEndReason_TERMINAL_END_REASON_AGENT_RESTARTED {
		t.Errorf("ended with %v", end)
	}
}

func TestExactTerminalValidatesAndPinsTheExec(t *testing.T) {
	e := dockertest.New(t)
	e.AddLocalImage("fixture:1", dockertest.Image{Shell: "/bin/sh"})
	id := "0123456789abcdef0123456789abcdef0123456789abcdef0123456789abcdef"
	e.AddContainer(docker.Container{ID: id, Name: "/fixture", Config: docker.Config{Image: "fixture:1"}})
	var checked string
	m := New(Options{Docker: e.Client(), AsGoodAsRoot: func(_ context.Context, got string) (bool, error) {
		checked = got
		return true, nil
	}})
	t.Cleanup(m.closeAll)
	if _, err := m.Open(t.Context(), &agentv1.OpenTerminalCall{Id: "trm_short", ContainerId: id[:12], Argv: []string{"/bin/sh"}}); err == nil {
		t.Fatal("an ID prefix opened a terminal")
	}
	request := &agentv1.OpenTerminalCall{Id: "trm_exact", ContainerId: id, Argv: []string{"/bin/sh", "-i"}}
	asked, err := m.Open(t.Context(), request)
	if err != nil || !asked.GetNeedsStepUp() || checked != id || len(m.Known()) != 0 {
		t.Fatalf("exact terminal must hold for confirmation: %v, %v, checked %s", asked, err, checked)
	}
	request.Confirmed = true
	opened, err := m.Open(t.Context(), request)
	if err != nil || opened.GetContainerId() != id || opened.GetContainerName() != "fixture" {
		t.Fatalf("exact terminal: %v, %v", opened, err)
	}
}

func TestExactTerminalRejectsAnExecBoundToADifferentContainer(t *testing.T) {
	e := dockertest.New(t)
	var started atomic.Bool
	e.Intercept(func(request *http.Request) error {
		if strings.Contains(request.URL.Path, "/exec/") && strings.HasSuffix(request.URL.Path, "/start") {
			started.Store(true)
		}
		return nil
	})
	e.AddLocalImage("fixture:1", dockertest.Image{Shell: "/bin/sh"})
	id := e.AddContainer(docker.Container{Name: "/fixture", Config: docker.Config{Image: "fixture:1"}}).ID
	m := New(Options{Docker: e.Client(), AsGoodAsRoot: func(_ context.Context, _ string) (bool, error) {
		e.Remove(id)
		e.AddContainer(docker.Container{Name: "/" + id, Config: docker.Config{Image: "fixture:1"}})
		return false, nil
	}})
	t.Cleanup(m.closeAll)
	_, err := m.Open(t.Context(), &agentv1.OpenTerminalCall{Id: "trm_exact", ContainerId: id, Argv: []string{"/bin/sh"}})
	if !errors.Is(err, docker.ErrExecContainerChanged) || len(m.Known()) != 0 || started.Load() {
		t.Fatalf("a terminal must open in the instance whose access was checked: %v", err)
	}
}

func TestDatabaseConsoleValidatesAndOpensRecognisedEngine(t *testing.T) {
	e := dockertest.New(t)
	e.AddLocalImage("postgres:17", dockertest.Image{Shell: "/bin/sh"})
	e.AddLocalImage("nginx:1.27", dockertest.Image{Shell: "/bin/sh"})
	e.AddContainer(docker.Container{Name: "/pg-1", Config: docker.Config{Image: "postgres:17"}})
	e.AddContainer(docker.Container{Name: "/web-1", Config: docker.Config{Image: "nginx:1.27"}})
	m := New(Options{
		Docker: e.Client(),
		AsGoodAsRoot: func(_ context.Context, _ string) (bool, error) {
			return false, nil
		},
	})
	t.Cleanup(m.closeAll)

	// Host terminal cannot request database_console.
	if _, err := m.Open(t.Context(), &agentv1.OpenTerminalCall{Id: "trm_host", DatabaseConsole: true}); err == nil {
		t.Fatal("host terminal accepted database_console")
	}
	// Non-database container is refused.
	if _, err := m.Open(t.Context(), &agentv1.OpenTerminalCall{Id: "trm_web", Container: "web-1", DatabaseConsole: true}); err == nil {
		t.Fatal("non-database container opened a database console")
	}
	// Recognised database container opens with client title.
	res, err := m.Open(t.Context(), &agentv1.OpenTerminalCall{Id: "trm_pg", Container: "pg-1", DatabaseConsole: true, Database: "shop", Columns: 80, Rows: 24})
	if err != nil {
		t.Fatalf("database console failed: %v", err)
	}
	if res.GetTitle() != "psql/shop@pg-1" {
		t.Errorf("title = %q, want %q", res.GetTitle(), "psql/shop@pg-1")
	}

	// L2 & L3: Redis with password only in --requirepass passes REDISCLI_AUTH
	// and REDISCLI_HISTFILE=/dev/null to the exec create request.
	var mu sync.Mutex
	var gotRedisBody string
	e.AddLocalImage("redis:7", dockertest.Image{Shell: "/bin/sh"})
	e.AddContainer(docker.Container{
		Name: "/redis-1",
		Config: docker.Config{
			Image: "redis:7",
			Cmd:   []string{"redis-server", "--requirepass", "cmd-only-secret"},
		},
	})
	e.Intercept(func(r *http.Request) error {
		if r.Method == http.MethodPost && strings.HasSuffix(r.URL.Path, "/exec") {
			b, _ := io.ReadAll(r.Body)
			mu.Lock()
			gotRedisBody = string(b)
			mu.Unlock()
			r.Body = io.NopCloser(strings.NewReader(string(b)))
		}
		return nil
	})
	redisRes, err := m.Open(t.Context(), &agentv1.OpenTerminalCall{Id: "trm_redis", Container: "redis-1", DatabaseConsole: true, Columns: 80, Rows: 24})
	if err != nil {
		t.Fatalf("redis console failed: %v", err)
	}
	if redisRes.GetTitle() != "redis-cli@redis-1" {
		t.Errorf("redis title = %q, want redis-cli@redis-1", redisRes.GetTitle())
	}
	mu.Lock()
	body := gotRedisBody
	mu.Unlock()
	if !strings.Contains(body, "REDISCLI_AUTH=cmd-only-secret") {
		t.Errorf("L2: redis console exec body missing REDISCLI_AUTH=cmd-only-secret; got %s", body)
	}
	if !strings.Contains(body, "REDISCLI_HISTFILE=/dev/null") {
		t.Errorf("L3: redis console exec body missing REDISCLI_HISTFILE=/dev/null; got %s", body)
	}
}
