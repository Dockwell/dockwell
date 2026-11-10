// SPDX-License-Identifier: AGPL-3.0-only

package agent

import (
	"context"
	"fmt"
	"slices"
	"time"

	"github.com/dockwell/dockwell/internal/agent/desired"
	"github.com/dockwell/dockwell/internal/docker"
)

// Why a container that should run isn't running is something the agent has to
// know, not guess from its being down (ADR 0054). Docker's state can't say: a
// container a person stopped and one Docker stopped as it came back from being
// killed both read exited, with no error. What Docker says happened can:
//
//   - A person's docker stop, docker kill or docker compose stop leaves "kill"
//     (and "stop") after the container's "start".
//   - A container that ends by itself leaves "die" alone.
//   - An engine killed and started again, without live-restore, stops what
//     outlived it and says "kill", "stop" and "die" of each, as of a person's
//     stop. But it remembers nothing from before, and no container runs under
//     it that it didn't start, so there the stop has no "start" before it.
//
// That last tells the two apart only while Docker still holds everything since
// it started: it keeps its last docker.EventsHeld events, of every kind. Past
// that, a stop with no start before it could be either, and the agent says so.
// This is what a real engine was seen to do in CI (docs/testing/chaos.md).

// eventsMargin is how far under what Docker holds a count of its events has to
// be for them to be all there are since it started.
const eventsMargin = 16

// lostNote is what the journal says of a start the agent made for this.
const lostNote = "Docker stopped it as Docker came back from being killed, which nobody asked for, so the agent started it."

// whyDown says why a service whose containers are ids, none of which runs, isn't
// running, from what Docker says happened. ids[0] is the one that stands for it;
// the rest are what else answers to it and isn't leaving, as what a rollout that
// failed left behind, or a twin made by hand. It says: someone stopped it, or it
// ended by itself (Stopped); Docker stopped it as it came back (Lost); or why
// the agent can't tell (Unsure). It reads Docker's events once, up to now, and
// holds nothing after. An error is Docker not answering: nothing is settled, and
// the next look asks again.
//
// With more than one, a stop someone made of any of them is a stop of the
// service: it's the one that ran that a person stops, whichever stands now.
// Otherwise the agent can't say which of them is the service, so it starts
// none, and says so.
func whyDown(ctx context.Context, c *docker.Client, ids []string, now time.Time) (desired.Standing, error) {
	events, err := c.PastEvents(ctx, now)
	if err != nil {
		return desired.Standing{}, err
	}
	// What happened to each since it last started, or in all Docker holds.
	type seen struct{ started, stopped, died bool }
	of := make([]seen, len(ids))
	for i := range events {
		e := &events[i]
		k := slices.Index(ids, e.ID)
		if e.Type != "container" || k < 0 {
			continue
		}
		switch e.Action {
		case "start":
			of[k] = seen{started: true}
		case "kill", "stop":
			of[k].stopped = true
		case "die":
			of[k].died = true
		}
	}
	for _, s := range of {
		if s.stopped && s.started {
			return desired.Standing{Stopped: true}, nil
		}
	}
	if len(ids) > 1 {
		return desired.Standing{Unsure: unsure("more than one container answers to it, none of them running, and nothing says someone stopped any of them, so the agent can't say which is the one to start")}, nil
	}
	started, stopped, died := of[0].started, of[0].stopped, of[0].died
	whole := len(events) < docker.EventsHeld-eventsMargin
	switch {
	case stopped && started:
		return desired.Standing{Stopped: true}, nil
	case stopped:
		// Stopped, with no start before it in what Docker holds.
		info, err := c.Info(ctx)
		if err != nil {
			return desired.Standing{}, err
		}
		switch {
		case info.LiveRestoreEnabled:
			// It ran on through the engine's restart, so the engine didn't stop
			// it as it came back: someone did.
			return desired.Standing{Stopped: true}, nil
		case whole:
			return desired.Standing{Lost: true}, nil
		}
		return desired.Standing{Unsure: unsure("Docker no longer holds what happened before it was stopped, so that can have been a person, or Docker as it came back from a restart")}, nil
	case died:
		// It ended by itself, and its restart policy left it there.
		return desired.Standing{Stopped: true}, nil
	case whole:
		return desired.Standing{Unsure: unsure("it wasn't running when Docker last started, and Docker keeps nothing from before then")}, nil
	}
	return desired.Standing{Unsure: unsure("Docker no longer holds what happened to it")}, nil
}

// unsure is the whole of what Activity says of a container the agent leaves
// down for not knowing: what, why, and what to do.
func unsure(why string) string {
	return fmt.Sprintf("It isn't running, and the agent can't tell whether someone stopped it: %s. "+
		"The agent leaves it as it is. Start it from the control plane if it should run", why)
}
