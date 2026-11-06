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
	// fails once and passes again isn't either. A state that lasts is told then,
	// with when it came about.
	Dwell = 30 * time.Second
	// FlapWindow is how far back changes count for a service that keeps
	// changing. One that went between good and bad flapsToTell times in it is
	// told as one event, which changes as it goes on, until it has stayed one way
	// for this long.
	FlapWindow = 5 * time.Minute
	// flapsToTell is how many times a service has to go between good and bad in
	// FlapWindow to be told as one that keeps changing, whatever Dwell says: one
	// that never stays anywhere for that long would otherwise never be told at all.
	flapsToTell = 3
	// flapEvery is how often the event of one that keeps changing changes, at
	// most.
	flapEvery = Dwell
	// recordEvery is how often it looks at what has come due.
	recordEvery = 5 * time.Second
	// flipsKept bounds what a track keeps of when it changed.
	flipsKept = 64
)

// Told is what an event says, as the audit log keeps it in its detail: the one
// kind of detail that leaves the store, as its writer here is its whole source.
type Told struct {
	State    dockwellv1.HealthState `json:"state"`
	Previous dockwellv1.HealthState `json:"previous"`
	Output   string                 `json:"output,omitempty"`
	// Flips is how many times it went between good and bad since Since, for a
	// service that keeps changing, which this one event stands for.
	Flips int32 `json:"flips,omitempty"`
	// Since is when it came to State, as this control plane heard, or when it
	// began to keep changing. Events before it was kept have none.
	Since *time.Time `json:"since,omitempty"`
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
	askedStore bool                   // it has read what the timeline last had
	// wasGood says which way it last stood, of good and bad, for counting flips:
	// a state that says neither, as an agent that can't say, doesn't count.
	wasGood, stood bool
	// flips is when it went between good and bad, within FlapWindow.
	flips []time.Time
	// badState and badOutput are the last bad state, and what it said, as what a
	// service that keeps changing is told as.
	badState  dockwellv1.HealthState
	badOutput string
	// flap is the event of one that keeps changing, while it does.
	flap *flap
}

// flap is the one event that stands for a service that keeps changing.
type flap struct {
	event store.Event
	told  Told
	// flips is how many times it changed since the event's Since.
	flips int32
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
			t.flips = append(t.flips, now)
			if over := len(t.flips) - flipsKept; over > 0 {
				t.flips = slices.Delete(t.flips, 0, over)
			}
			if t.flap != nil {
				t.flap.flips++
			}
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

// due is an event to write: a new one, or the one of a service that keeps
// changing, again.
type due struct {
	key   key
	event store.Event
	told  Told
	// again is the event of one that keeps changing, written before.
	again bool
}

// Record tells the timeline of what has come due, and first reads what it last
// said of each service it hasn't heard of since this control plane started, so
// a restart neither repeats a state nor misses one that ended while it was away.
func (r *Recorder) Record(ctx context.Context) {
	now := r.now()
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
				t.told = told.State
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
		if d, ok := t.next(now); ok {
			if !d.again {
				d.event = store.Event{ID: id.New(id.Event), Actor: "dockwell", Action: Action, Subject: subject, Target: k.server, Surface: store.SurfaceAutomatic}
			}
			detail, err := json.Marshal(d.told)
			if err != nil {
				continue
			}
			d.key, d.event.At, d.event.Detail = k, now, string(detail)
			out = append(out, d)
		}
	}
	r.mu.Unlock()
	if len(out) == 0 {
		return
	}
	err := r.DB.Update(ctx, func(tx *store.Tx) error {
		for i := range out {
			if out[i].again {
				if err := tx.UpdateEvent(ctx, out[i].event); err != nil && !errors.Is(err, store.ErrNotFound) {
					return err
				}
				continue
			}
			// A server speaks for what runs on it, and for no other. The API reads
			// its reports by placement too, and a timeline is as plain about it.
			placed, err := tx.Placement(ctx, out[i].event.Subject, revisions[out[i].event.Subject])
			if err != nil {
				return err
			}
			if !slices.Contains(placed, out[i].key.server) {
				out[i].event.ID = ""
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
		t := r.tracked[out[i].key]
		if t == nil || out[i].event.ID == "" {
			continue
		}
		t.told = out[i].told.State
		switch {
		case out[i].again && t.flap != nil:
			t.flap.event, t.flap.told = out[i].event, out[i].told
		case out[i].told.Flips > 0:
			t.flap = &flap{event: out[i].event, told: out[i].told, flips: out[i].told.Flips}
		}
	}
}

// anyDue reports whether anything has come due, without reading the store.
func (r *Recorder) anyDue(now time.Time) bool {
	r.mu.Lock()
	defer r.mu.Unlock()
	for _, t := range r.tracked {
		if !t.askedStore {
			continue
		}
		if _, ok := t.next(now); ok {
			return true
		}
	}
	return false
}

// next is what the timeline should hear of a service now, if anything. A state
// that lasts and isn't what the timeline has is told, with when it came about.
// What the timeline has never told of, it tells only if it is bad: a service that
// has always been fine isn't news. One that keeps changing is told as one event,
// which changes no more often than flapEvery for as long as it goes on, and says
// what it keeps falling to and how often since it began. It is told without the
// event's ID or time, which the caller gives it.
func (t *track) next(now time.Time) (due, bool) {
	recent := t.flipsWithin(now)
	if t.flap != nil {
		// It goes on until it has stayed one way for FlapWindow, and no flip is left within it.
		if recent > 0 {
			told := t.flap.told
			told.Flips = t.flap.flips
			told.State, told.Output = t.fellTo(told)
			if told == t.flap.told || now.Sub(t.flap.event.At) < flapEvery {
				return due{}, false
			}
			return due{event: t.flap.event, told: told, again: true}, true
		}
		// It has stayed one way long enough to have stopped changing.
		t.flap = nil
	}
	if recent >= flapsToTell {
		since := t.flips[len(t.flips)-recent]
		// recent is at most flipsKept, which an int32 holds.
		told := Told{Previous: t.told, Output: t.output, Flips: int32(min(recent, flipsKept)), Since: &since}
		told.State, told.Output = t.fellTo(told)
		return due{told: told}, true
	}
	if now.Sub(t.since) < Dwell {
		return due{}, false
	}
	if bad(t.state) && t.state != t.told || good(t.state) && bad(t.told) {
		since := t.since
		return due{told: Told{State: t.state, Previous: t.told, Output: t.output, Since: &since}}, true
	}
	return due{}, false
}

// fellTo is what a service that keeps changing is told as: what it keeps falling
// to, whichever way it stands now, as that is the news.
func (t *track) fellTo(told Told) (dockwellv1.HealthState, string) {
	if t.badState != dockwellv1.HealthState_HEALTH_STATE_UNSPECIFIED {
		return t.badState, t.badOutput
	}
	return t.state, told.Output
}

// flipsWithin counts the times it went between good and bad within FlapWindow,
// and forgets those before.
func (t *track) flipsWithin(now time.Time) int {
	i := 0
	for i < len(t.flips) && now.Sub(t.flips[i]) >= FlapWindow {
		i++
	}
	if i > 0 {
		t.flips = slices.Delete(t.flips, 0, i)
	}
	return len(t.flips)
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
