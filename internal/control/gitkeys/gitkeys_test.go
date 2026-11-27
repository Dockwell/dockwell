// SPDX-License-Identifier: AGPL-3.0-only

package gitkeys

import (
	"context"
	"errors"
	"log/slog"
	"os"
	"path/filepath"
	"slices"
	"sync"
	"testing"
	"time"

	"connectrpc.com/connect"
)

func TestSSHGateFairnessAcrossProjects(t *testing.T) {
	g := sshGate{listWait: 60 * time.Millisecond}
	ctx := t.Context()

	// Project A takes its 1 listing slot and 1 fetch slot.
	listA, err := g.acquireList(ctx, "prj_a", "gdk_a1")
	if err != nil {
		t.Fatalf("project A first list: %v", err)
	}
	fetchA, err := g.acquireFetch(ctx, "prj_a", "gdk_a1")
	if err != nil {
		t.Fatalf("project A first fetch: %v", err)
	}

	// Project B immediately gets a listing slot and a fetch slot even while A
	// holds its full per-project allowance (N8 fairness).
	listB, err := g.acquireList(ctx, "prj_b", "gdk_b1")
	if err != nil {
		t.Fatalf("project B list while A holds its cap: %v", err)
	}
	fetchB, err := g.acquireFetch(ctx, "prj_b", "gdk_b1")
	if err != nil {
		t.Fatalf("project B fetch while A holds its cap: %v", err)
	}

	// Project A trying a second listing waits listWait then returns ResourceExhausted.
	start := time.Now()
	if _, err := g.acquireList(ctx, "prj_a", "gdk_a2"); connect.CodeOf(err) != connect.CodeResourceExhausted {
		t.Fatalf("project A second list = %v, want ResourceExhausted", err)
	}
	if waited := time.Since(start); waited < 40*time.Millisecond {
		t.Fatalf("project A second list failed immediately after %v; want bounded wait", waited)
	}

	// Releasing is idempotent and restores slots cleanly.
	listA()
	listA()
	fetchA()
	listB()
	fetchB()
	again, err := g.acquireList(ctx, "prj_a", "gdk_a1")
	if err != nil {
		t.Fatalf("re-acquire after release: %v", err)
	}
	again()
}

func TestSSHGateFIFOWaitAndBuildFetchWait(t *testing.T) {
	g := sshGate{listWait: 80 * time.Millisecond}
	ctx := t.Context()

	// 1. Fill both global fetch slots (prj_a and prj_b).
	fetchA, err := g.acquireFetch(ctx, "prj_a", "gdk_a")
	if err != nil {
		t.Fatal(err)
	}
	fetchB, err := g.acquireFetch(ctx, "prj_b", "gdk_b")
	if err != nil {
		t.Fatal(err)
	}

	// 2. Queue prj_c then prj_d as build fetches (wait past listWait until freed,
	// in FIFO order).
	var mu sync.Mutex
	var order []string
	cQueued := make(chan struct{})
	dQueued := make(chan struct{})
	cDone := make(chan error, 1)
	dDone := make(chan error, 1)

	go func() {
		close(cQueued)
		rel, err := g.acquireFetch(ctx, "prj_c", "gdk_c")
		if err != nil {
			cDone <- err
			return
		}
		mu.Lock()
		order = append(order, "prj_c")
		mu.Unlock()
		time.Sleep(25 * time.Millisecond)
		rel()
		cDone <- nil
	}()
	<-cQueued
	// Ensure prj_c is registered in the waiter queue before prj_d arrives.
	deadline := time.Now().Add(time.Second)
	for {
		g.mu.Lock()
		n := len(g.waiters)
		g.mu.Unlock()
		if n >= 1 || time.Now().After(deadline) {
			break
		}
		time.Sleep(time.Millisecond)
	}
	go func() {
		close(dQueued)
		rel, err := g.acquireFetch(ctx, "prj_d", "gdk_d")
		if err != nil {
			dDone <- err
			return
		}
		mu.Lock()
		order = append(order, "prj_d")
		mu.Unlock()
		rel()
		dDone <- nil
	}()
	<-dQueued
	deadline = time.Now().Add(time.Second)
	for {
		g.mu.Lock()
		n := len(g.waiters)
		g.mu.Unlock()
		if n >= 2 || time.Now().After(deadline) {
			break
		}
		time.Sleep(time.Millisecond)
	}

	// Hold past listWait (80ms) to prove build fetches wait on their own context
	// rather than failing at listWait.
	time.Sleep(110 * time.Millisecond)
	fetchA()
	if err := <-cDone; err != nil {
		t.Fatalf("prj_c build fetch failed while waiting: %v", err)
	}
	fetchB()
	if err := <-dDone; err != nil {
		t.Fatalf("prj_d build fetch failed while waiting: %v", err)
	}
	if !slices.Equal(order, []string{"prj_c", "prj_d"}) {
		t.Fatalf("waiter wake order = %v, want [prj_c prj_d]", order)
	}

	// 3. A queued build fetch respects context cancellation and does not leak a slot.
	holdA, err := g.acquireFetch(ctx, "prj_a", "gdk_a")
	if err != nil {
		t.Fatal(err)
	}
	cancelCtx, cancel := context.WithTimeout(ctx, 40*time.Millisecond)
	defer cancel()
	if _, err := g.acquireFetch(cancelCtx, "prj_a", "gdk_a2"); !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("cancelled fetch wait = %v, want DeadlineExceeded", err)
	}
	holdA()

	// 4. Interactive listing waits in order and succeeds if freed before listWait.
	holdL, err := g.acquireList(ctx, "prj_a", "gdk_a")
	if err != nil {
		t.Fatal(err)
	}
	go func() {
		time.Sleep(25 * time.Millisecond)
		holdL()
	}()
	relL, err := g.acquireList(ctx, "prj_a", "gdk_a")
	if err != nil {
		t.Fatalf("interactive list freed before listWait failed: %v", err)
	}
	relL()
}

func TestSweepPackTempDirRemovesStalePacks(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "ssh-packs")
	if err := os.MkdirAll(dir, 0o700); err != nil {
		t.Fatal(err)
	}
	stale := filepath.Join(dir, "dockwell-ssh-pack-stale123")
	keep := filepath.Join(dir, "unrelated-file")
	if err := os.WriteFile(stale, []byte("old"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(keep, []byte("keep"), 0o600); err != nil {
		t.Fatal(err)
	}
	sweepPackTempDir(dir, slog.New(slog.DiscardHandler))
	if _, err := os.Stat(stale); !os.IsNotExist(err) {
		t.Fatalf("stale pack file was not swept: %v", err)
	}
	if _, err := os.Stat(keep); err != nil {
		t.Fatalf("unrelated file should remain: %v", err)
	}
}
