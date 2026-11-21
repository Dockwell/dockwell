// SPDX-License-Identifier: AGPL-3.0-only

package agent

import (
	"fmt"
	"net/http"
	"runtime"
	"slices"
	"strconv"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	agentv1 "github.com/dockwell/dockwell/gen/dockwell/agent/v1"
	v1 "github.com/dockwell/dockwell/gen/dockwell/v1"
	"github.com/dockwell/dockwell/internal/docker"
	"github.com/dockwell/dockwell/internal/docker/dockertest"
)

func TestDockerWatchLeaseAndCap(t *testing.T) {
	e := dockertest.New(t)
	d := e.Client()
	defer d.Close()
	c := newCalls(t.Context(), Config{Docker: d}, func(*agentv1.ConnectRequest) error { return nil }, nil)
	defer c.end()
	for n := range 8 {
		if err := c.watchInventory(&agentv1.DockerInventoryWatchCall{SubscriptionId: strconv.Itoa(n)}); err != nil {
			t.Fatal(err)
		}
	}
	w := &c.inventory
	w.mu.Lock()
	done := w.done
	first := w.held["0"]
	w.mu.Unlock()
	if err := c.watchInventory(&agentv1.DockerInventoryWatchCall{SubscriptionId: "ninth"}); err == nil {
		t.Fatal("nine simultaneous watches opened")
	}
	w.mu.Lock()
	w.held["0"] = inventoryWatch{expires: time.Now().Add(5 * time.Second)}
	w.mu.Unlock()
	if err := c.watchInventory(&agentv1.DockerInventoryWatchCall{SubscriptionId: "0"}); err != nil {
		t.Fatal(err)
	}
	w.mu.Lock()
	renewed := w.held["0"].expires
	for n := range 8 {
		w.held[strconv.Itoa(n)] = inventoryWatch{expires: time.Now().Add(-time.Second)}
	}
	w.mu.Unlock()
	if renewed.Before(first.expires) || time.Until(renewed) < time.Minute {
		t.Fatal("renewal did not restore the lease")
	}
	select {
	case <-done:
	case <-time.After(5 * time.Second):
		t.Fatal("the follower kept running after all leases had expired")
	}
	w.mu.Lock()
	defer w.mu.Unlock()
	if len(w.held) != 0 || w.cancel != nil {
		t.Fatal("expired watches are still held")
	}
}

func TestDockerWatchUpdatesHealthWithoutRereadingAndEndsWhenClosed(t *testing.T) {
	e := dockertest.New(t)
	d := e.Client()
	defer d.Close()
	paused := runtime.NumGoroutine()
	id := e.AddContainer(docker.Container{Name: "/fixture"}).ID
	var reads atomic.Int32
	e.Intercept(func(request *http.Request) error {
		if strings.Contains(request.URL.Path, "/containers/") && strings.HasSuffix(request.URL.Path, "/json") {
			reads.Add(1)
		}
		if strings.HasSuffix(request.URL.Path, "/events") {
			filters := request.URL.Query().Get("filters")
			if !strings.Contains(filters, `"event":[`) || strings.Contains(filters, "exec_") {
				return fmt.Errorf("inventory event filters: %s", filters)
			}
		}
		return nil
	})
	updates := make(chan *agentv1.DockerInventoryChanged, 32)
	c := newCalls(t.Context(), Config{Docker: d}, func(msg *agentv1.ConnectRequest) error {
		select {
		case updates <- msg.GetDockerInventoryChanged():
		case <-t.Context().Done():
		}
		return nil
	}, nil)
	defer c.end()
	if err := c.watchInventory(&agentv1.DockerInventoryWatchCall{SubscriptionId: "viewer", VisibleIds: []string{id}}); err != nil {
		t.Fatal(err)
	}
	c.inventory.mu.Lock()
	done := c.inventory.done
	c.inventory.mu.Unlock()
	deadline := time.NewTimer(6 * time.Second)
	defer deadline.Stop()
	for {
		select {
		case msg := <-updates:
			if msg.GetEventStreamReady() && msg.GetResync() {
				goto watching
			}
		case <-deadline.C:
			t.Fatal("Docker's event stream did not open")
		}
	}

watching:
	actions := []string{"exec_create: healthcheck", "exec_start: healthcheck", "exec_die"}
	for range 8 {
		e.EmitContainerEvents(id, actions...)
	}
	e.SetHealth(id, "unhealthy", "private health details")
	for {
		select {
		case msg := <-updates:
			if msg.GetResync() {
				t.Fatal("exec and health events invalidated the inventory")
			}
			if slices.ContainsFunc(msg.GetHealthUpdates(), func(update *v1.DockerHealthUpdate) bool {
				return update.GetKey().GetEngineId() == id && update.GetHealth() == v1.DockerHealth_DOCKER_HEALTH_UNHEALTHY
			}) {
				goto stop
			}
		case <-deadline.C:
			t.Fatal("health change did not update the watched row")
		}
	}

stop:
	// Give a delayed resync time to arrive, even if the noise came right
	// after the opening notification.
	quiet := time.NewTimer(dockerResyncInterval + 3*time.Second)
	defer quiet.Stop()
	for {
		select {
		case msg := <-updates:
			if msg.GetResync() {
				t.Fatal("exec and health events invalidated the inventory after coalescing")
			}
		case <-quiet.C:
			goto closed
		}
	}

closed:
	if got := reads.Load(); got != 0 {
		t.Fatalf("%d inventory reads after health and exec events", got)
	}
	if err := c.watchInventory(&agentv1.DockerInventoryWatchCall{SubscriptionId: "viewer", Stop: true}); err != nil {
		t.Fatal(err)
	}
	select {
	case <-done:
	case <-time.After(3 * time.Second):
		t.Fatal("follower or Engine event reader is still running")
	}
	d.Close()
	until := time.Now().Add(2 * time.Second)
	for runtime.NumGoroutine() > paused && time.Now().Before(until) {
		time.Sleep(10 * time.Millisecond)
	}
	if got := runtime.NumGoroutine(); got > paused {
		t.Fatalf("goroutines after viewer closed: %d, before %d", got, paused)
	}
}
