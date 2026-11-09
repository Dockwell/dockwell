// SPDX-License-Identifier: AGPL-3.0-only

package terminal

import (
	"context"
	"errors"
	"fmt"
	"sync"
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
