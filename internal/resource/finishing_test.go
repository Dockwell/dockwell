// SPDX-License-Identifier: AGPL-3.0-only

package resource_test

import (
	"context"
	"testing"
	"time"

	"github.com/dockwell/dockwell/internal/resource"
)

// What a step finishes outlives its call, and it says so to whoever runs it.
func TestWhatAStepFinishesOutlivesItsCall(t *testing.T) {
	call, end := context.WithCancel(t.Context())
	o := resource.NewOvertime()
	ctx, cancel := resource.Finishing(resource.WithOvertime(call, o), time.Hour)
	defer cancel()
	end()
	if err := ctx.Err(); err != nil {
		t.Fatalf("it ended with its call: %v", err)
	}
	if wait := o.Leave(time.Minute); wait < 59*time.Minute {
		t.Fatalf("it was to be waited for, and whoever runs it has %s", wait)
	}
	if err := ctx.Err(); err != nil {
		t.Fatalf("it was left with time to go: %v", err)
	}
}

// Past its bound and the grace it's left behind, and nothing it does goes on.
func TestAStepLeftBehindFinishesNothing(t *testing.T) {
	o := resource.NewOvertime()
	call := resource.WithOvertime(t.Context(), o)
	under, cancel := resource.Finishing(call, time.Nanosecond)
	defer cancel()
	time.Sleep(time.Millisecond)
	if wait := o.Leave(0); wait != 0 {
		t.Fatalf("it had %s left, past its bound", wait)
	}
	select {
	case <-under.Done():
	case <-time.After(3 * time.Second):
		t.Fatal("what it was finishing went on")
	}
	after, cancel := resource.Finishing(call, time.Hour)
	defer cancel()
	if after.Err() == nil {
		t.Fatal("it began something new after it was left behind, which has to be over before it starts")
	}
}

// One that nothing runs, as outside an agent, has its time and no more.
func TestFinishingWithNobodyRunningTheStep(t *testing.T) {
	call, end := context.WithCancel(t.Context())
	ctx, cancel := resource.Finishing(call, time.Hour)
	defer cancel()
	end()
	if err := ctx.Err(); err != nil {
		t.Fatalf("it ended with its call: %v", err)
	}
	if _, ok := ctx.Deadline(); !ok {
		t.Fatal("it has no bound")
	}
}
