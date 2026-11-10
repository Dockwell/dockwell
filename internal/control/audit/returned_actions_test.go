// SPDX-License-Identifier: AGPL-3.0-only

package audit

import (
	"maps"
	"slices"
	"testing"
)

func TestTheScanFollowsReturnedActions(t *testing.T) {
	found, problems := scanActions(t, map[string]string{
		"internal/store/store.go": `package store
type Event struct{ Action string }
type Tx struct{}
func (t *Tx) RecordEvent(e Event) error { return nil }`,
		"internal/one/one.go": `package one
const Done = "job.done"
func Choose(failed bool) (string, error) {
  _ = func() string { return unknownAction() }
  if failed { return "job.failed", nil }
  return Done, nil
}
func Maybe(skip bool) (string, error) {
  if skip { return "", nil }
  return Choose(false)
}
func Identity(action string) string { return action }`,
		"internal/two/two.go": `package two
import (
  "` + module + `/internal/one"
  "` + module + `/internal/store"
)
type H struct{ tx *store.Tx }
func (h *H) record(action string) error { return h.tx.RecordEvent(store.Event{Action: action}) }
func (h *H) run(skip bool) error {
  action, err := one.Maybe(skip)
  if err != nil || action == "" { return err }
  return h.record(action)
}
func (h *H) declared(skip bool) error {
  var action, err = one.Maybe(skip)
  if "" == action || err != nil { return err }
  return h.record(action)
}
func (h *H) direct() error { return h.forward("job.noted") }
func (h *H) forward(action string) error { return h.record(one.Identity(action)) }
func other() (error, string) { return nil, "job.selected" }
func delegated() (error, string) { return other() }
func (h *H) selected() error { var _, action = delegated(); return h.record(action) }
func repeated(first bool) string { if first { return one.Done }; return one.Done }
func (h *H) recordRepeated() error { return h.record(string(repeated(true))) }`,
	})
	want := []string{"job.done", "job.failed", "job.noted", "job.selected"}
	if got := slices.Sorted(maps.Keys(found)); !slices.Equal(got, want) {
		t.Errorf("found %v, want %v", got, want)
	}
	if len(problems) != 0 {
		t.Errorf("problems: %v", problems)
	}
}

func TestReturnedActionsStillRefuseWhatTheScanCannotProve(t *testing.T) {
	for name, body := range map[string]string{
		"unknown return": `func choose() string { return runtimeAction() }
func run(tx *store.Tx) error { return tx.RecordEvent(store.Event{Action: choose()}) }`,
		"recursive return": `func choose() string { return choose() }
func run(tx *store.Tx) error { return tx.RecordEvent(store.Event{Action: choose()}) }`,
		"recursive local": `func run(tx *store.Tx) error { var action string; action = action; return tx.RecordEvent(store.Event{Action: action}) }`,
		"unguarded empty return": `func choose() (string, error) { return "", nil }
func run(tx *store.Tx) error { action, _ := choose(); return tx.RecordEvent(store.Event{Action: action}) }`,
		"empty guard with another condition": `func choose() (string, error) { return "", nil }
func run(tx *store.Tx) error { action, err := choose(); if err != nil && action == "" { return err }; return tx.RecordEvent(store.Event{Action: action}) }`,
		"assignment after empty guard": `func choose() (string, error) { return "", nil }
func run(tx *store.Tx) error { action, err := choose(); if action == "" { return err }; action = ""; return tx.RecordEvent(store.Event{Action: action}) }`,
		"guard of a shadowed action": `func choose() (string, error) { return "", nil }
func run(tx *store.Tx) error { action, err := choose(); if action := "job.other"; action == "" { return err }; return tx.RecordEvent(store.Event{Action: action}) }`,
		"empty guard without a return": `func choose() (string, error) { return "", nil }
func run(tx *store.Tx) error { action, _ := choose(); if action == "" { println("empty") }; return tx.RecordEvent(store.Event{Action: action}) }`,
		"jump past empty guard": `func choose() (string, error) { return "", nil }
func run(tx *store.Tx) error { action, _ := choose(); goto record; if action == "" { return nil }; record: return tx.RecordEvent(store.Event{Action: action}) }`,
		"closure writes after empty guard": `func choose() (string, error) { return "", nil }
func run(tx *store.Tx) error { action, _ := choose(); reset := func() { action = "" }; if action == "" { return nil }; reset(); return tx.RecordEvent(store.Event{Action: action}) }`,
		"pointer writes after empty guard": `func choose() (string, error) { return "", nil }
func run(tx *store.Tx) error { action, _ := choose(); pointer := &action; if action == "" { return nil }; *pointer = ""; return tx.RecordEvent(store.Event{Action: action}) }`,
		"overwritten helper parameter": `func choose(action string) string { action = runtimeAction(); return action }
func run(tx *store.Tx) error { return tx.RecordEvent(store.Event{Action: choose("job.done")}) }`,
		"guard of a mutable global": `var action = ""
func reset() { action = "" }
func run(tx *store.Tx) error { if action == "" { return nil }; reset(); return tx.RecordEvent(store.Event{Action: action}) }`,
		"invalid action name": `func choose() string { return "not an action" }
func run(tx *store.Tx) error { return tx.RecordEvent(store.Event{Action: choose()}) }`,
	} {
		t.Run(name, func(t *testing.T) {
			_, problems := scanActions(t, map[string]string{
				"internal/store/store.go": `package store
type Event struct{ Action string }
type Tx struct{}
func (t *Tx) RecordEvent(e Event) error { return nil }`,
				"internal/one/one.go": "package one\nimport \"" + module + "/internal/store\"\n" + body,
			})
			if len(problems) == 0 {
				t.Fatal("the scan accepted an action it cannot prove")
			}
		})
	}
}
