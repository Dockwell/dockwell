// SPDX-License-Identifier: AGPL-3.0-only

// Package reconciled puts on the timeline what a server's agent did, or couldn't
// do, on its own while the control plane was away (ADR 0054).
package reconciled

import (
	"context"
	"encoding/json"
	"fmt"
	"time"

	agentv1 "github.com/dockwell/dockwell/gen/dockwell/agent/v1"
	"github.com/dockwell/dockwell/internal/health"
	"github.com/dockwell/dockwell/internal/id"
	"github.com/dockwell/dockwell/internal/resource/resourcepb"
	"github.com/dockwell/dockwell/internal/store"
)

// The timeline's actions for what an agent did on its own.
const (
	ActionMade    = "server.reconciled_made"
	ActionStarted = "server.reconciled_started"
	ActionWaiting = "server.reconciled_waiting"
	ActionDropped = "server.reconciled_dropped"
)

// Actions are all of them.
var Actions = []string{ActionMade, ActionStarted, ActionWaiting, ActionDropped}

var actions = map[agentv1.LocalAction]string{
	agentv1.LocalAction_LOCAL_ACTION_MADE: ActionMade, agentv1.LocalAction_LOCAL_ACTION_STARTED: ActionStarted,
	agentv1.LocalAction_LOCAL_ACTION_WAITING: ActionWaiting,
}

// Told is what an event of these keeps: the resource, and why, never a value or a
// spec.
type Told struct {
	Kind    string `json:"kind,omitempty"`
	Scope   string `json:"scope,omitempty"`
	Name    string `json:"name,omitempty"`
	Reason  string `json:"reason,omitempty"`
	Dropped int    `json:"dropped,omitempty"`
}

// ReadTold reads an event's detail.
func ReadTold(detail string) (Told, bool) {
	var t Told
	return t, json.Unmarshal([]byte(detail), &t) == nil
}

// The bounds of what an agent says, which is made fit to show before it reaches a
// timeline anyone with the read scope sees: an agent that was taken over could say
// anything.
const (
	maxEntries = 500
	maxName    = 200
	// maxAge is how far back an entry's time may go, and none goes past now.
	maxAge = 30 * 24 * time.Hour
)

// Recorder puts what each server's agent did on its own on the timeline.
type Recorder struct {
	DB  *store.DB
	Now func() time.Time
}

// Heard records what server's agent sent, each entry once, however often it's
// sent: an entry's event is named by the server and the entry's own ID.
func (r *Recorder) Heard(ctx context.Context, server string, rec *agentv1.Reconciled) error {
	now := time.Now()
	if r.Now != nil {
		now = r.Now()
	}
	at := func(t time.Time) time.Time {
		switch {
		case t.IsZero() || t.After(now):
			return now
		case now.Sub(t) > maxAge:
			return now.Add(-maxAge)
		}
		return t
	}
	return r.DB.Update(ctx, func(tx *store.Tx) error {
		entries := rec.GetEntries()
		for _, e := range entries[:min(len(entries), maxEntries)] {
			action, ok := actions[e.GetAction()]
			if !ok || e.GetId() == "" {
				continue
			}
			ref := resourcepb.FromRef(e.GetResource())
			told := Told{
				Kind: fit(string(ref.Kind)), Scope: fit(ref.Scope), Name: fit(ref.Name), Reason: health.Clean(e.GetReason(), nil),
			}
			// Named by the time the agent sent, not the one shown, which depends on
			// when it's heard, so an entry sent again is one event.
			sent := e.GetTime().AsTime()
			if err := record(ctx, tx, server, id.At(id.Event, sent, server+"\x00"+fit(e.GetId())), at(sent), action, told); err != nil {
				return err
			}
		}
		if n := rec.GetDropped(); n > 0 {
			// Named by the newest entry dropped, with no time, so the same count sent
			// again is one event, whenever it's heard.
			if err := record(ctx, tx, server, id.At(id.Event, time.Time{}, server+"\x00dropped\x00"+fit(rec.GetDroppedTo())), now, ActionDropped, Told{Dropped: int(min(n, 1<<20))}); err != nil {
				return err
			}
		}
		return nil
	})
}

func record(ctx context.Context, tx *store.Tx, server, eventID string, at time.Time, action string, told Told) error {
	detail, err := json.Marshal(told)
	if err != nil {
		return err
	}
	if _, err := tx.RecordEventOnce(ctx, store.Event{
		ID: eventID, At: at, Actor: "server:" + server, Action: action, Subject: server, Target: server,
		Surface: store.SurfaceAutomatic, Detail: string(detail),
	}); err != nil {
		return fmt.Errorf("can't record what %s did on its own: %w", server, err)
	}
	return nil
}

// fit is a name an agent sent, made one short printable line.
func fit(s string) string {
	s = health.Clean(s, nil)
	if r := []rune(s); len(r) > maxName {
		s = string(r[:maxName])
	}
	return s
}
