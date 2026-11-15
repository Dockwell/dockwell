// SPDX-License-Identifier: AGPL-3.0-only

package revision

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"reflect"
	"slices"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"google.golang.org/protobuf/proto"

	"github.com/dockwell/dockwell/internal/resource"
	"github.com/dockwell/dockwell/internal/store"
)

// readsOf counts the reads of change id's stored plan until stop: only its plan
// counts, not one of another test's changes.
func readsOf(id string) (count func() int32, stop func()) {
	its := []byte(`"` + id + `"`)
	var n atomic.Int32
	heard := func(b []byte) {
		if bytes.Contains(b, its) {
			n.Add(1)
		}
	}
	planDecoded.Store(&heard)
	return n.Load, func() { planDecoded.Store(nil) }
}

// openPorts is n edits that each open a port on srv_A, from port up.
func openPorts(n int, port uint32) []Edit {
	edits := make([]Edit, n)
	for i := range edits {
		edits[i] = rule("srv_A", fmt.Sprintf("rule-%d-%d", n, i), port+uint32(i))
	}
	return edits
}

// sameForFortyAsForTwo fails t unless reads counts as many reads of a plan for 40
// items as for 2, and some for either.
func sameForFortyAsForTwo(t *testing.T, doing string, reads func(n int, port uint32) int32) {
	t.Helper()
	two, forty := reads(2, 2000), reads(40, 3000)
	t.Logf("reads of the change's plan: %d %s 2 items, %d %s 40", two, doing, forty, doing)
	if two == 0 {
		t.Fatalf("%s read no plan, so nothing here counts how often it does", doing)
	}
	if forty != two {
		t.Errorf("%s 40 items read the change's plan %d times, and 2 items %d: it reads it again for each item", doing, forty, two)
	}
}

// Applying a change reads its stored plan as often for 40 items as for 2: once for
// all its items, not about 13 times for each, which made applying grow with the
// square of a change's size (#686).
func TestApplyingReadsAPlanAsOftenForFortyItemsAsForTwo(t *testing.T) {
	e, _, _ := setup(t, store.SQLite, "srv_A")
	sameForFortyAsForTwo(t, "applying", func(n int, port uint32) int32 {
		t.Helper()
		p := propose(t, e, fmt.Sprintf("Open %d ports", n), openPorts(n, port)...)
		count, stop := readsOf(p.Change)
		defer stop()
		apply(t, e, p)
		return count()
	})
}

// Checking whether a person may apply a change, as the API does before it applies,
// reads its plan as often for 40 items as for 2 (#706).
func TestCheckingApplyReadsAPlanAsOftenForFortyItemsAsForTwo(t *testing.T) {
	e, _, _ := setup(t, store.SQLite, "srv_A")
	sameForFortyAsForTwo(t, "checking", func(n int, port uint32) int32 {
		t.Helper()
		p := propose(t, e, fmt.Sprintf("Open %d ports", n), openPorts(n, port)...)
		count, stop := readsOf(p.Change)
		defer stop()
		if err := e.CheckApply(t.Context(), p.Change); err != nil {
			t.Fatal(err)
		}
		return count()
	})
}

// Resuming a change left applying when the control plane stopped reads its plan as
// often for 40 items as for 2 (#706).
func TestResumingReadsAPlanAsOftenForFortyItemsAsForTwo(t *testing.T) {
	sameForFortyAsForTwo(t, "resuming", func(n int, port uint32) int32 {
		t.Helper()
		e, f, db := setup(t, store.SQLite, "srv_A")
		plain := f["srv_A"].agent
		// The first step outlasts the control plane's grace, so the change stays
		// applying with every step left.
		var held atomic.Bool
		started := make(chan struct{}, 1)
		f["srv_A"].agent = hooked{Agent: plain, apply: func(ctx context.Context, _ resource.Step, _ proto.Message) error {
			if !held.CompareAndSwap(false, true) {
				return nil
			}
			started <- struct{}{}
			<-ctx.Done()
			return ctx.Err()
		}}
		p := propose(t, e, fmt.Sprintf("Open %d ports", n), openPorts(n, port)...)
		if err := e.Start(t.Context(), p.Change, By{Actor: "user:usr_mira", Via: ViaPanel}); err != nil {
			t.Fatal(err)
		}
		<-started
		grace, cancel := context.WithTimeout(t.Context(), 50*time.Millisecond)
		defer cancel()
		if err := e.Stop(grace); err != nil {
			t.Fatalf("Stop = %v", err)
		}
		if c := stored(t, db, p.Change); c.State != store.ChangeApplying {
			t.Fatalf("a change stopped partway is %s, not applying", c.State)
		}
		f["srv_A"].agent = plain
		count, stop := readsOf(p.Change)
		defer stop()
		next := newEngine(db, f)
		if err := next.Resume(t.Context()); err != nil {
			t.Fatalf("Resume = %v", err)
		}
		next.Wait()
		if c := stored(t, db, p.Change); c.State != store.ChangeApplied {
			t.Fatalf("the resumed change is %s, not applied", c.State)
		}
		return count()
	})
}

// A rollback runs on the plan it is stored with, whether it runs as it is made or
// resumed later, so no reader of it can tell the two apart (#706).
func TestARollbackReadsItsStoredPlanFreshOrResumed(t *testing.T) {
	e, f, db := setup(t, store.SQLite, "srv_A", "srv_B")
	// srv_B goes away as srv_A's step runs: the change fails, and its rollback waits
	// for srv_B, applying.
	f["srv_A"].agent = hooked{Agent: f["srv_A"].agent, before: func(resource.Step) { f["srv_B"].online.Store(false) }}
	onB := web("nginx:1.27")
	onB.Ref.Scope = "srv_B"
	p := propose(t, e, "Open http and run web", onA(), onB)

	var mu sync.Mutex
	var read [][]byte
	heard := func(b []byte) {
		mu.Lock()
		defer mu.Unlock()
		read = append(read, bytes.Clone(b))
	}
	planDecoded.Store(&heard)
	defer planDecoded.Store(nil)
	// readOf takes the plans read since it was last asked that are the change id's.
	readOf := func(id string) [][]byte {
		mu.Lock()
		defer mu.Unlock()
		its := []byte(`"` + id + `"`)
		var out [][]byte
		for _, b := range read {
			if bytes.Contains(b, its) {
				out = append(out, b)
			}
		}
		read = nil
		return out
	}
	readIt := func(reads [][]byte, plan []byte) bool {
		return slices.ContainsFunc(reads, func(b []byte) bool { return bytes.Equal(b, plan) })
	}

	var failed *FailedError
	if err := e.Apply(t.Context(), p.Change); !errors.As(err, &failed) {
		t.Fatalf("apply = %v", err)
	}
	rb := stored(t, db, failed.Rollback)
	if rb.State != store.ChangeApplying || len(rb.Plan) == 0 {
		t.Fatalf("the rollback while srv_B is away is %s, with a plan of %d bytes", rb.State, len(rb.Plan))
	}
	if fresh := readOf(failed.Rollback); !readIt(fresh, rb.Plan) {
		t.Errorf("the rollback ran as it was made without reading the plan it's stored with (%d reads of its plan)", len(fresh))
	}
	f["srv_B"].online.Store(true)
	if err := e.Resume(t.Context()); err != nil {
		t.Fatal(err)
	}
	e.Wait()
	if resumed := readOf(failed.Rollback); !readIt(resumed, rb.Plan) {
		t.Errorf("the resumed rollback didn't read the plan it's stored with (%d reads of its plan)", len(resumed))
	}
	if c := stored(t, db, failed.Rollback); c.State != store.ChangeApplied {
		t.Errorf("the resumed rollback is %s, not applied", c.State)
	}
}

// A plan stored by an earlier Dockwell still reads whole. Each reader of a change's
// plan reads it from one full decode (#686), so a stored plan that failed one, as
// it would if a field of Plan or of what it holds changed its type, would give
// every reader nothing: no restart, restore, install, role check or resume would
// read its part (#706). No field has changed its type so far, and each file in
// testdata/plans holds that: a plan with every field set, as the Dockwell of its
// date stored it. One that stops decoding needs its field kept readable, or a
// lenient fallback in readPlan, before the change lands.
//
// DOCKWELL_PLAN_SNAPSHOT=testdata/plans/<date>.json writes one for today's Plan.
func TestAPlanStoredEarlierStillReadsWhole(t *testing.T) {
	if out := os.Getenv("DOCKWELL_PLAN_SNAPSHOT"); out != "" {
		b, err := json.MarshalIndent(everyField(reflect.TypeFor[Plan](), 16).Interface(), "", "  ")
		if err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(out, append(b, '\n'), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	files, err := filepath.Glob(filepath.Join("testdata", "plans", "*.json"))
	if err != nil || len(files) == 0 {
		t.Fatalf("no stored plans in testdata/plans (%v)", err)
	}
	for _, name := range files {
		b, err := os.ReadFile(name)
		if err != nil {
			t.Fatal(err)
		}
		r := readPlan(store.Change{ID: "chg_stored", Plan: b})
		if r.plan == nil {
			t.Errorf("%s no longer reads, so nothing reads any part of a plan stored as it is: %v. Keep the field's old type readable, or give readPlan a lenient fallback for it", name, r.err)
			continue
		}
		if len(r.plan.Resources) == 0 || len(r.at) == 0 {
			t.Errorf("%s read with no resources, so it holds nothing of what they ask", name)
		}
	}
}

// everyField is a value of type t with each exported field set, nested up to depth
// levels deep, as no real plan sets them all at once.
func everyField(t reflect.Type, depth int) reflect.Value {
	v := reflect.New(t).Elem()
	if depth == 0 {
		return v
	}
	switch t.Kind() {
	case reflect.Pointer:
		v.Set(reflect.New(t.Elem()))
		v.Elem().Set(everyField(t.Elem(), depth-1))
	case reflect.Struct:
		if t == reflect.TypeFor[time.Time]() {
			v.Set(reflect.ValueOf(time.Date(2026, 11, 15, 12, 0, 0, 0, time.UTC)))
			return v
		}
		for i := range t.NumField() {
			if f := t.Field(i); f.IsExported() && f.Tag.Get("json") != "-" {
				v.Field(i).Set(everyField(f.Type, depth-1))
			}
		}
	case reflect.Slice:
		if t == reflect.TypeFor[json.RawMessage]() {
			v.SetBytes([]byte(`{}`))
			return v
		}
		v.Set(reflect.Append(v, everyField(t.Elem(), depth-1)))
	case reflect.Map:
		v.Set(reflect.MakeMap(t))
		v.SetMapIndex(everyField(t.Key(), depth-1), everyField(t.Elem(), depth-1))
	case reflect.String:
		v.SetString("x")
	case reflect.Bool:
		v.SetBool(true)
	case reflect.Int, reflect.Int8, reflect.Int16, reflect.Int32, reflect.Int64:
		v.SetInt(1)
	case reflect.Uint, reflect.Uint8, reflect.Uint16, reflect.Uint32, reflect.Uint64:
		v.SetUint(1)
	case reflect.Float32, reflect.Float64:
		v.SetFloat(1.5)
	default:
	}
	return v
}
