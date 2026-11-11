// SPDX-License-Identifier: AGPL-3.0-only

package docker

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"time"
)

// WentPastMemory reports whether the kernel killed something in a container for
// going past its memory limit between two times. A container's own state can't say
// that of one a restart policy brought back, as Docker clears OOMKilled as it starts
// it. Its events keep it, for as long as the few hundred Docker holds reach back.
func (c *Client) WentPastMemory(ctx context.Context, id string, from, to time.Time) (bool, error) {
	filters, err := json.Marshal(map[string][]string{"type": {"container"}, "container": {id}, "event": {"oom"}})
	if err != nil {
		return false, err
	}
	path, err := c.versioned(ctx, "/events")
	if err != nil {
		return false, err
	}
	// With an end in the past or now, Docker sends what it has and closes. An end
	// cut to the second would stop before an event from this one, which a restart
	// that's just happened is.
	query := url.Values{"since": {eventTime(from)}, "until": {eventTime(to)}, "filters": {string(filters)}}
	res, err := c.send(ctx, http.MethodGet, path, query, nil)
	if err != nil {
		return false, err
	}
	defer res.Body.Close()
	defer discard(res.Body)
	var event struct {
		Action string `json:"Action"`
	}
	switch err := json.NewDecoder(res.Body).Decode(&event); {
	case errors.Is(err, io.EOF):
		return false, nil
	case err != nil:
		return false, fmt.Errorf("can't read Docker's events: %w", err)
	}
	return event.Action == "oom", nil
}

// eventTime writes a time as Docker's events take it: seconds, and nanoseconds.
func eventTime(t time.Time) string { return fmt.Sprintf("%d.%09d", t.Unix(), t.Nanosecond()) }

// EventsHeld is how many events Docker is taken to hold at most, to answer for
// the past: its engine keeps the last 256, of every kind, in memory, and loses
// them when it stops.
const EventsHeld = 256

// Event is one thing Docker says happened.
type Event struct {
	// Type is what it happened to, such as "container", and Action what, such
	// as "start", "kill", "stop", "die" or "destroy".
	Type, Action string
	// ID is the container's, or whatever else it happened to.
	ID string
	// At is when.
	At time.Time
}

// PastEvents is what Docker still holds of what happened up to now, oldest
// first, of every kind: at most EventsHeld of them, and none from before the
// engine last started. It asks up to now, so Docker answers and closes; nothing
// stays open.
func (c *Client) PastEvents(ctx context.Context, now time.Time) ([]Event, error) {
	path, err := c.versioned(ctx, "/events")
	if err != nil {
		return nil, err
	}
	// A second on, as an end cut to the second stops before what's just happened.
	query := url.Values{"since": {"0"}, "until": {eventTime(now.Add(time.Second))}}
	res, err := c.send(ctx, http.MethodGet, path, query, nil)
	if err != nil {
		return nil, err
	}
	defer res.Body.Close()
	defer discard(res.Body)
	var out []Event
	dec := json.NewDecoder(res.Body)
	for {
		var e struct {
			Type   string `json:"Type"`
			Action string `json:"Action"`
			Actor  struct {
				ID string `json:"ID"`
			} `json:"Actor"`
			Time     int64 `json:"time"`
			TimeNano int64 `json:"timeNano"`
		}
		switch err := dec.Decode(&e); {
		case errors.Is(err, io.EOF):
			return out, nil
		case err != nil:
			return nil, fmt.Errorf("can't read Docker's events: %w", err)
		}
		at := time.Unix(0, e.TimeNano)
		if e.TimeNano == 0 {
			at = time.Unix(e.Time, 0)
		}
		out = append(out, Event{Type: e.Type, Action: e.Action, ID: e.Actor.ID, At: at})
		if len(out) > 4*EventsHeld {
			// More than Docker is known to hold: read no further than is useful.
			return out, nil
		}
	}
}
