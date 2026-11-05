// SPDX-License-Identifier: AGPL-3.0-only

package health

import (
	"context"
	"encoding/json"
	"errors"
	"log/slog"
	"slices"
	"sync"
	"time"

	dockwellv1 "github.com/dockwell/dockwell/gen/dockwell/v1"
	"github.com/dockwell/dockwell/internal/docker"
	"github.com/dockwell/dockwell/internal/id"
	"github.com/dockwell/dockwell/internal/resource/service"
	"github.com/dockwell/dockwell/internal/store"
)

// Action is what the audit log calls a change of a service's health. Its subject
// is the service, its target the server, and its detail a Told.
const Action = "service.health"

// What keeps the timeline readable, whatever a service does (ADR 0027).
const (
	// Dwell is how long a state must last to be told, so a stop-first deploy's
	// few seconds aren't news, as the change tells of those, and a check that
	// fails once and passes again isn't either.
	Dwell = 30 * time.Second
	// HoldOff is the least time between two events of a service on a server. A
	// service that flaps is told at this pace, each with how many times it
	// changed in between.
	HoldOff = 5 * time.Minute
	// flapsToTell is how many times a service has to go between good and bad to be
	// told as flapping, whatever Dwell says: one that never stays anywhere for
	// that long would otherwise never be told at all.
	flapsToTell = 3
	// recordEvery is how often it looks at what has come due.
	recordEvery = 5 * time.Second
)

// Told is what an event says, as the audit log keeps it in its detail: the one
// kind of detail that leaves the store, as its writer here is its whole source.
type Told struct {
	State    dockwellv1.HealthState `json:"state"`
	Previous dockwellv1.HealthState `json:"previous"`
	Output   string                 `json:"output,omitempty"`
	Flips    int32                  `json:"flips,omitempty"`
}

// ReadTold reads what an event says.
func ReadTold(detail string) (Told, bool) {
	var t Told
	if json.Unmarshal([]byte(detail), &t) != nil {
		return Told{}, false
	}
	return t, true
}

// Recorder hears how services are, and tells the timeline when it matters.
type Recorder struct {
	DB     *store.DB
	Logger *slog.Logger
	// Now is the time. Nil means time.Now.
	Now func() time.Time

	mu      sync.Mutex
	tracked map[key]*track
}

type key struct{ server, project, service string }

// track is where a service on a server stands.
type track struct {
	state      dockwellv1.HealthState
	since      time.Time
	output     string
	told       dockwellv1.HealthState // as the timeline last had it
	toldAt     time.Time
	flips      int32 // what changed since it was last told
	askedStore bool  // it has read what the timeline last had
	// wasGood says which way it last stood, of good and bad, for counting flips:
	// a state that says neither, as an agent that can't say, doesn't count.
	wasGood, stood bool
	// badState and badOutput are the last bad state, and what it said, as what a
	// service flapping faster than Dwell is told as.
	badState  dockwellv1.HealthState
	badOutput string
}

func (r *Recorder) now() time.Time {
	if r.Now != nil {
		return r.Now()
	}
	return time.Now()
}

// Heard takes in a server's report. It is cheap: it reads and writes the store
// only for what has come due, and never for what stays as it is.
func (r *Recorder) Heard(server string, instances []*dockwellv1.InstanceHealth) {
	byService := map[key][]*dockwellv1.InstanceHealth{}
	for _, i := range instances {
		k := key{server, i.GetProject(), i.GetService()}
		byService[k] = append(byService[k], i)
	}
	now := r.now()
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.tracked == nil {
		r.tracked = map[key]*track{}
	}
	for k, list := range byService {
		t := r.tracked[k]
		if t == nil {
			t = &track{state: dockwellv1.HealthState_HEALTH_STATE_UNSPECIFIED, since: now}
			r.tracked[k] = t
		}
		t.see(Best(list), lastOutput(list), now)
	}
	// A service with no container left doesn't run, if it is there at all, which
	// recording it finds out.
	for k, t := range r.tracked {
		if k.server == server && byService[k] == nil {
			t.see(dockwellv1.HealthState_HEALTH_STATE_NOT_RUNNING, "the service has no container on this server", now)
		}
	}
}

func (t *track) see(state dockwellv1.HealthState, output string, now time.Time) {
	if output != "" {
		t.output = output
	}
	if bad(state) {
		t.badState, t.badOutput = state, output
	}
	if t.state == state {
		return
	}
	if good(state) || bad(state) {
		if t.stood && t.wasGood != good(state) {
			t.flips++
		}
		t.stood, t.wasGood = true, good(state)
	}
	t.state, t.since = state, now
}

// good is a state that isn't news when it stays; bad is one that is. A check that
// hasn't finished yet, and an agent that can't say, are neither.
func good(s dockwellv1.HealthState) bool {
	return s == dockwellv1.HealthState_HEALTH_STATE_HEALTHY || s == dockwellv1.HealthState_HEALTH_STATE_NO_CHECK
}

func bad(s dockwellv1.HealthState) bool {
	return s == dockwellv1.HealthState_HEALTH_STATE_UNHEALTHY || s == dockwellv1.HealthState_HEALTH_STATE_NOT_RUNNING ||
		s == dockwellv1.HealthState_HEALTH_STATE_PAUSED
}

func lastOutput(list []*dockwellv1.InstanceHealth) string {
	best := Best(list)
	for _, i := range list {
		if i.GetState() == best {
			return i.GetLast().GetOutput()
		}
	}
	return ""
}

// Run records what comes due, until ctx is done.
func (r *Recorder) Run(ctx context.Context) {
	tick := time.NewTicker(recordEvery)
	defer tick.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-tick.C:
			r.Record(ctx)
		}
	}
}

// Record tells the timeline of what has come due, and first reads what it last
// said of each service it hasn't heard of since this control plane started, so
// a restart neither repeats a state nor misses one that ended while it was away.
func (r *Recorder) Record(ctx context.Context) {
	now := r.now()
	type due struct {
		key   key
		event store.Event
		told  Told
	}
	r.mu.Lock()
	keys := make([]key, 0, len(r.tracked))
	for k, t := range r.tracked {
		if !t.askedStore {
			keys = append(keys, k)
		}
	}
	r.mu.Unlock()
	if len(keys) == 0 && !r.anyDue(now) {
		return
	}
	var services map[string]string
	revisions := map[string]int{}
	if err := r.DB.View(ctx, func(tx *store.Tx) error {
		list, err := tx.ListKind(ctx, string(service.Kind))
		if err != nil {
			return err
		}
		services = make(map[string]string, len(list))
		for i := range list {
			services[docker.Project(list[i].Scope)+"/"+list[i].Name] = list[i].ID
			revisions[list[i].ID] = list[i].Revision
		}
		return nil
	}); err != nil {
		r.fail(ctx, err)
		return
	}
	// What the timeline last said of those it hasn't asked about yet.
	last := map[key]store.Event{}
	if len(keys) > 0 {
		err := r.DB.View(ctx, func(tx *store.Tx) error {
			for _, k := range keys {
				subject, ok := services[k.project+"/"+k.service]
				if !ok {
					continue
				}
				e, found, err := tx.LatestEvent(ctx, Action, subject, k.server)
				if err != nil {
					return err
				}
				if found {
					last[k] = e
				}
			}
			return nil
		})
		if err != nil {
			r.fail(ctx, err)
			return
		}
	}
	var out []due
	r.mu.Lock()
	for _, k := range keys {
		t := r.tracked[k]
		if t == nil {
			continue
		}
		t.askedStore = true
		if e, ok := last[k]; ok {
			if told, ok := ReadTold(e.Detail); ok {
				t.told, t.toldAt = told.State, e.At
			}
		}
	}
	for k, t := range r.tracked {
		if !t.askedStore {
			continue
		}
		subject, known := services[k.project+"/"+k.service]
		if !known {
			delete(r.tracked, k) // deleted, or not yet committed: its next report brings it back
			continue
		}
		if !t.due(now) {
			continue
		}
		told := Told{State: t.state, Previous: t.told, Output: t.output, Flips: max(t.flips-1, 0)}
		if t.flapping(now) && t.badState != dockwellv1.HealthState_HEALTH_STATE_UNSPECIFIED {
			// What it keeps falling to is the news, whichever way it stands now.
			told.State, told.Output = t.badState, t.badOutput
		}
		detail, err := json.Marshal(told)
		if err != nil {
			continue
		}
		out = append(out, due{k, store.Event{
			ID: id.New(id.Event), At: now, Actor: "dockwell", Action: Action, Subject: subject, Target: k.server,
			Detail: string(detail), Surface: store.SurfaceAutomatic,
		}, told})
	}
	r.mu.Unlock()
	if len(out) == 0 {
		return
	}
	err := r.DB.Update(ctx, func(tx *store.Tx) error {
		for i := range out {
			// A server speaks for what runs on it, and for no other. The API reads
			// its reports by placement too, and a timeline is as plain about it.
			placed, err := tx.Placement(ctx, out[i].event.Subject, revisions[out[i].event.Subject])
			if err != nil {
				return err
			}
			if !slices.Contains(placed, out[i].key.server) {
				continue
			}
			if err := tx.RecordEvent(ctx, out[i].event); err != nil {
				return err
			}
		}
		return nil
	})
	if err != nil {
		r.fail(ctx, err)
		return
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	for i := range out {
		if t := r.tracked[out[i].key]; t != nil {
			t.told, t.toldAt, t.flips = out[i].told.State, now, 0
		}
	}
}

// anyDue reports whether anything has come due, without reading the store.
func (r *Recorder) anyDue(now time.Time) bool {
	r.mu.Lock()
	defer r.mu.Unlock()
	for _, t := range r.tracked {
		if t.askedStore && t.due(now) {
			return true
		}
	}
	return false
}

// due reports a state worth telling: one that has lasted, and isn't what the
// timeline has, with the last event long enough ago. What the timeline has never
// told of, it tells only if it is bad: a service that has always been fine
// isn't news.
func (t *track) due(now time.Time) bool {
	switch {
	case now.Sub(t.toldAt) < HoldOff:
		return false
	case t.flapping(now):
		return true
	case now.Sub(t.since) < Dwell:
		return false
	case bad(t.state):
		return t.state != t.told
	case good(t.state):
		return bad(t.told)
	}
	return false
}

// flapping reports a service going between good and bad faster than it stays in
// either, so Dwell would never let it be told.
func (t *track) flapping(now time.Time) bool {
	return t.flips >= flapsToTell && now.Sub(t.since) < Dwell
}

func (r *Recorder) fail(ctx context.Context, err error) {
	if errors.Is(err, context.Canceled) || ctx.Err() != nil {
		return
	}
	logger := r.Logger
	if logger == nil {
		logger = slog.Default()
	}
	logger.WarnContext(ctx, "can't record a change of a service's health on the timeline; the control plane tries again", slog.String("error", err.Error()))
}
