// SPDX-License-Identifier: AGPL-3.0-only

package store

import (
	"context"
	"fmt"
	"slices"
	"strings"
	"sync"
	"time"
)

// The timeline: changes and the audit events that belong beside them, in
// one list, newest first. See docs/adr/0020-the-event-timeline.md.

// TimelineFilter narrows the timeline. Every field that is set must hold.
type TimelineFilter struct {
	// Changes includes changes. Actions includes the audit events with
	// these actions. With neither, the timeline is empty.
	Changes bool
	Actions []string
	// ServerID keeps the changes whose plans touch the server, and the
	// audit events whose subject it is.
	ServerID string
	// ResourceID keeps the changes to the resource or to what lives in
	// it, its environments and their services for a project, and of the audit
	// events only those with ResourceActions about it or what lives in it.
	ResourceID string
	// ResourceActions are the Actions a ResourceID keeps, where an event's
	// subject is a resource, as when a service's health changes.
	ResourceActions []string
	// Actor keeps what one actor did, as the store names actors, such as
	// "user:usr_…". A change counts for whoever proposed it and whoever
	// applied it.
	Actor string
	// ActorPrefixes keeps what actors whose names start with one of these
	// did, such as "user:"; a prefix without a colon is a whole name, such
	// as "dockwell".
	ActorPrefixes []string
}

// TimelineCursor is a place in the timeline: an entry's time and ID.
type TimelineCursor struct {
	At time.Time
	ID string
}

// TimelineEntry is a change or an audit event, with its time on the
// timeline: when a change started applying, or was planned until then.
type TimelineEntry struct {
	At     time.Time
	Change *Change // without its items
	Event  *Event
}

// ID is the entry's change's or event's ID.
func (e TimelineEntry) ID() string {
	if e.Change != nil {
		return e.Change.ID
	}
	return e.Event.ID
}

// TimelineQuery says which part of the timeline to read.
type TimelineQuery struct {
	Filter TimelineFilter
	// Before reads the entries older than this place, newest first. After
	// reads those newer than it, oldest first, to page forward; a place
	// without an ID starts at its time. With neither, the newest.
	Before *TimelineCursor
	After  *TimelineCursor
	// ChangeIDs keeps only these changes, and no audit events.
	ChangeIDs []string
	// Discarded adds the changes the filter keeps that were discarded, each
	// at when it was, so a reader paging forward finds what left the
	// timeline as well as what joined it.
	Discarded bool
	// Limit caps the entries read. Zero reads them all.
	Limit int
}

// TimelinePlace is where an entry is on the timeline, without the entry.
type TimelinePlace struct {
	At time.Time
	ID string
	// Change says it is a change, not an audit event.
	Change bool
	// Discarded says the change left the timeline at At. Only a query
	// with Discarded finds these.
	Discarded bool
}

// A change's time on the timeline.
const changeTime = `COALESCE(c.applied_at, c.created_at)`

// Timeline reads entries from the timeline. Discarded changes aren't on
// it, nor Dockwell's own changes that revert nothing: today, only the one
// that made an install's defaults, which ADR 0014 leaves off the timeline.
func (t *Tx) Timeline(ctx context.Context, q TimelineQuery) ([]TimelineEntry, error) {
	places, err := t.TimelinePlaces(ctx, q)
	if err != nil {
		return nil, err
	}
	return t.TimelineEntries(ctx, places)
}

// TimelinePlaces reads where entries are on the timeline, as Timeline
// would read them, without reading the entries: TimelineEntries reads
// those, so a reader can skip what it has already.
func (t *Tx) TimelinePlaces(ctx context.Context, q TimelineQuery) ([]TimelinePlace, error) {
	union, args := timelineUnion(q)
	if union == "" {
		return nil, nil
	}
	query := `SELECT at, id, source FROM (` + union + `) entries`
	if q.Before == nil && q.After != nil {
		query += ` ORDER BY at, id`
	} else {
		query += ` ORDER BY at DESC, id DESC`
	}
	if q.Limit > 0 {
		query += ` LIMIT ?`
		args = append(args, q.Limit)
	}
	var out []TimelinePlace
	rows, err := t.query(ctx, query, args...)
	if err != nil {
		return nil, fmt.Errorf("can't read the timeline: %w", err)
	}
	defer rows.Close()
	for rows.Next() {
		var at int64
		var p TimelinePlace
		var source string
		if err := rows.Scan(&at, &p.ID, &source); err != nil {
			return nil, fmt.Errorf("can't read the timeline: %w", err)
		}
		p.At, p.Change, p.Discarded = fromMillis(at), source != "event", source == "discarded"
		out = append(out, p)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("can't read the timeline: %w", err)
	}
	return out, nil
}

// TimelineTally is how many entries a filter keeps: its changes, and its audit
// events by action.
type TimelineTally struct {
	Changes int
	Actions map[string]int
}

// TimelineCounts tallies the entries the filter keeps, as Timeline would read
// them all, in one grouped query: discarded changes, which it leaves out, aren't
// counted (#639).
func (t *Tx) TimelineCounts(ctx context.Context, f TimelineFilter) (TimelineTally, error) {
	tally := TimelineTally{Actions: map[string]int{}}
	union, args := timelineUnion(TimelineQuery{Filter: f})
	if union == "" {
		return tally, nil
	}
	rows, err := t.query(ctx, `SELECT source, action, COUNT(*) FROM (`+union+`) entries GROUP BY source, action`, args...)
	if err != nil {
		return tally, fmt.Errorf("can't count the timeline: %w", err)
	}
	defer rows.Close()
	for rows.Next() {
		var source, action string
		var n int
		if err := rows.Scan(&source, &action, &n); err != nil {
			return tally, fmt.Errorf("can't count the timeline: %w", err)
		}
		if source == "event" {
			tally.Actions[action] += n
		} else {
			tally.Changes += n
		}
	}
	if err := rows.Err(); err != nil {
		return tally, fmt.Errorf("can't count the timeline: %w", err)
	}
	return tally, nil
}

// timelineUnion is the query for the timeline's places a query keeps, from each
// table it reads, unordered, with its arguments; empty where it keeps none.
func timelineUnion(q TimelineQuery) (string, []any) {
	f := q.Filter
	var parts []string
	var args []any
	where := func(w []string) string { return strings.Join(w, " AND ") }
	place := func(at, id string, w []string) []string {
		switch {
		case q.Before != nil:
			ms := millis(q.Before.At)
			args = append(args, ms, ms, q.Before.ID)
			return append(w, "("+at+" < ? OR "+at+" = ? AND "+id+" < ?)")
		case q.After != nil:
			ms := millis(q.After.At)
			args = append(args, ms, ms, q.After.ID)
			return append(w, "("+at+" > ? OR "+at+" = ? AND "+id+" > ?)")
		}
		return w
	}
	actors := func(columns ...string) []string {
		var w []string
		if f.Actor != "" {
			var alts []string
			for _, c := range columns {
				alts = append(alts, c+" = ?")
				args = append(args, f.Actor)
			}
			w = append(w, "("+strings.Join(alts, " OR ")+")")
		}
		if len(f.ActorPrefixes) > 0 {
			var alts []string
			for _, c := range columns {
				for _, p := range f.ActorPrefixes {
					if strings.HasSuffix(p, ":") {
						alts = append(alts, c+" LIKE ?")
						args = append(args, p+"%")
					} else {
						alts = append(alts, c+" = ?")
						args = append(args, p)
					}
				}
			}
			w = append(w, "("+strings.Join(alts, " OR ")+")")
		}
		return w
	}
	// The changes the filter keeps, in one state or out of one.
	// Dockwell's own changes are bookkeeping, as a server's host is, and stay off it,
	// but for a rollback, and a change to a catalog source: what a person can install
	// moving as its branch moves is theirs to see, and to undo (ADR 0077).
	changes := func(state string) []string {
		w := []string{state, "NOT (c.actor = ? AND c.reverts IS NULL AND NOT EXISTS (SELECT 1 FROM change_items i WHERE i.change_id = c.id AND i.kind = ?))"}
		args = append(args, ChangeDiscarded, systemActor, catalogSourceKind)
		if f.ServerID != "" {
			w = append(w, "c.id IN (SELECT change_id FROM change_servers WHERE server_id = ?)")
			args = append(args, f.ServerID)
		}
		if f.ResourceID != "" {
			w = append(w, `c.id IN (SELECT change_id FROM change_items WHERE resource_id = ? OR scope = ?
				OR scope IN (SELECT id FROM resources WHERE scope = ?))`)
			args = append(args, f.ResourceID, f.ResourceID, f.ResourceID)
		}
		if len(q.ChangeIDs) > 0 {
			w = append(w, "c.id IN ("+marks(len(q.ChangeIDs))+")")
			for _, id := range q.ChangeIDs {
				args = append(args, id)
			}
		}
		return append(w, actors("c.actor", "c.applied_by")...)
	}

	if f.Changes {
		w := place(changeTime, "c.id", changes("c.state <> ?"))
		parts = append(parts, `SELECT `+changeTime+` AS at, c.id AS id, 'change' AS source, '' AS action FROM changes c WHERE `+where(w))
		if q.Discarded {
			w := place("e.at", "c.id", changes("c.state = ?"))
			parts = append(parts, `SELECT e.at AS at, c.id AS id, 'discarded' AS source, '' AS action FROM changes c
				JOIN audit_events e ON e.subject = c.id AND e.action = 'change.discarded' WHERE `+where(w))
		}
	}
	actions := f.Actions
	if f.ResourceID != "" {
		actions = slices.DeleteFunc(slices.Clone(f.Actions), func(a string) bool { return !slices.Contains(f.ResourceActions, a) })
	}
	if len(actions) > 0 && len(q.ChangeIDs) == 0 {
		w := []string{"action IN (" + marks(len(actions)) + ")"}
		for _, a := range actions {
			args = append(args, a)
		}
		if f.ResourceID != "" {
			w = append(w, `(subject = ? OR subject IN (SELECT id FROM resources WHERE scope = ?)
				OR subject IN (SELECT id FROM resources WHERE scope IN (SELECT id FROM resources WHERE scope = ?)))`)
			args = append(args, f.ResourceID, f.ResourceID, f.ResourceID)
		}
		if f.ServerID != "" {
			w = append(w, "target = ?")
			args = append(args, f.ServerID)
		}
		w = append(w, actors("actor")...)
		w = place("at", "id", w)
		parts = append(parts, `SELECT at, id, 'event' AS source, action FROM audit_events WHERE `+where(w))
	}
	if len(parts) == 0 {
		return "", nil
	}
	return strings.Join(parts, " UNION ALL "), args
}

// TimelineEntries reads the entries at places, in their order, with a
// query for each table. It leaves out discarded changes, and any entry
// that is gone.
func (t *Tx) TimelineEntries(ctx context.Context, places []TimelinePlace) ([]TimelineEntry, error) {
	var changeIDs, eventIDs []any
	for _, p := range places {
		switch {
		case p.Discarded:
		case p.Change:
			changeIDs = append(changeIDs, p.ID)
		default:
			eventIDs = append(eventIDs, p.ID)
		}
	}
	changes := map[string]*Change{}
	if len(changeIDs) > 0 {
		rows, err := t.query(ctx, `SELECT `+changeColumns+` FROM changes WHERE id IN (`+marks(len(changeIDs))+`)`, changeIDs...)
		if err != nil {
			return nil, fmt.Errorf("can't read the timeline's changes: %w", err)
		}
		for rows.Next() {
			c, err := scanChange(rows)
			if err != nil {
				_ = rows.Close()
				return nil, fmt.Errorf("can't read the timeline's changes: %w", err)
			}
			changes[c.ID] = &c
		}
		if err := rows.Close(); err != nil {
			return nil, fmt.Errorf("can't read the timeline's changes: %w", err)
		}
	}
	events := map[string]*Event{}
	if len(eventIDs) > 0 {
		rows, err := t.query(ctx, `SELECT `+eventColumns+` FROM audit_events WHERE id IN (`+marks(len(eventIDs))+`)`, eventIDs...)
		if err != nil {
			return nil, fmt.Errorf("can't read the timeline's events: %w", err)
		}
		for rows.Next() {
			e, err := scanEvent(rows)
			if err != nil {
				_ = rows.Close()
				return nil, fmt.Errorf("can't read the timeline's events: %w", err)
			}
			events[e.ID] = &e
		}
		if err := rows.Close(); err != nil {
			return nil, fmt.Errorf("can't read the timeline's events: %w", err)
		}
	}
	out := make([]TimelineEntry, 0, len(places))
	for _, p := range places {
		if p.Discarded {
			continue
		}
		e := TimelineEntry{At: p.At, Change: changes[p.ID], Event: events[p.ID]}
		if e.Change != nil || e.Event != nil {
			out = append(out, e)
		}
	}
	return out, nil
}

// systemActor is how the revision store names Dockwell as an actor.
const systemActor = "dockwell"

// catalogSourceKind is a catalog source's kind, whose changes by Dockwell the
// timeline shows.
const catalogSourceKind = "catalog.source"

func marks(n int) string { return strings.Repeat("?, ", n-1) + "?" }

// ChangeServers returns the servers a change's plan touches, removed ones
// included, by ID.
func (t *Tx) ChangeServers(ctx context.Context, changeID string) ([]string, error) {
	rows, err := t.query(ctx, `SELECT server_id FROM change_servers WHERE change_id = ? ORDER BY server_id`, changeID)
	if err != nil {
		return nil, fmt.Errorf("can't read the servers of change %s: %w", changeID, err)
	}
	defer rows.Close()
	var out []string
	for rows.Next() {
		var s string
		if err := rows.Scan(&s); err != nil {
			return nil, fmt.Errorf("can't read the servers of change %s: %w", changeID, err)
		}
		out = append(out, s)
	}
	return out, rows.Err()
}

// LiveResource is a resource that exists now, with when its latest
// revision was made.
type LiveResource struct {
	Resource
	UpdatedAt time.Time
}

// A resource's place in time: the ULID that ends its ID, whatever its
// kind's prefix, which says when the ID was made.
const resourceMade = `substr(r.id, length(r.id) - 25)`

// PageResources returns at most limit resources that exist now, oldest
// first, after the one with ID after: of one kind, or every kind when kind
// is empty, and in one scope, or every scope when scope is empty.
func (t *Tx) PageResources(ctx context.Context, kind, scope, after string, limit int) ([]LiveResource, error) {
	made := ""
	if len(after) >= 26 {
		made = after[len(after)-26:]
	}
	rows, err := t.query(ctx, `SELECT `+resourceColumns+`, v.created_at`+latest+`
		WHERE v.spec IS NOT NULL AND (r.kind = ? OR ? = '') AND (r.scope = ? OR ? = '')
		AND (`+resourceMade+` > ? OR `+resourceMade+` = ? AND r.id > ?)
		ORDER BY `+resourceMade+`, r.id LIMIT ?`, kind, kind, scope, scope, made, made, after, limit)
	if err != nil {
		return nil, fmt.Errorf("can't list resources: %w", err)
	}
	defer rows.Close()
	var out []LiveResource
	for rows.Next() {
		var r LiveResource
		var deleted bool
		var at int64
		if err := rows.Scan(&r.ID, &r.Kind, &r.Scope, &r.Name, &r.Revision, &r.Spec, &deleted, &r.ChangeID, &at); err != nil {
			return nil, fmt.Errorf("can't list resources: %w", err)
		}
		r.Spec, r.UpdatedAt = spec(r.Spec, deleted), fromMillis(at)
		out = append(out, r)
	}
	return out, rows.Err()
}

// NamedResource is a resource that exists now, with when its latest
// revision was made and who made the change that made it.
type NamedResource struct {
	LiveResource
	Actor string // as the change has it, such as "user:usr_…"
}

// PageByName returns at most limit resources that exist now in scope, of
// any of kinds, by name and then kind, after the one with name and kind
// after, with the actor of each latest revision's change.
func (t *Tx) PageByName(ctx context.Context, scope string, kinds []string, after NamedResource, limit int) ([]NamedResource, error) {
	if len(kinds) == 0 {
		return nil, nil
	}
	args := []any{scope}
	for _, k := range kinds {
		args = append(args, k)
	}
	args = append(args, after.Name, after.Name, after.Name, after.Kind, limit)
	rows, err := t.query(ctx, `SELECT `+resourceColumns+`, v.created_at, c.actor`+latest+`
		JOIN changes c ON c.id = v.change_id
		WHERE r.scope = ? AND r.kind IN (`+marks(len(kinds))+`) AND v.spec IS NOT NULL
		AND (? = '' OR r.name > ? OR r.name = ? AND r.kind > ?)
		ORDER BY r.name, r.kind LIMIT ?`, args...)
	if err != nil {
		return nil, fmt.Errorf("can't list the resources of %s: %w", scope, err)
	}
	defer rows.Close()
	var out []NamedResource
	for rows.Next() {
		var r NamedResource
		var deleted bool
		var at int64
		if err := rows.Scan(&r.ID, &r.Kind, &r.Scope, &r.Name, &r.Revision, &r.Spec, &deleted, &r.ChangeID, &at, &r.Actor); err != nil {
			return nil, fmt.Errorf("can't list the resources of %s: %w", scope, err)
		}
		r.Spec, r.UpdatedAt = spec(r.Spec, deleted), fromMillis(at)
		out = append(out, r)
	}
	return out, rows.Err()
}

// signal wakes whoever waits on it, all at once, each time it fires.
type signal struct {
	mu sync.Mutex
	ch chan struct{}
}

func (s *signal) wait() <-chan struct{} {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.ch == nil {
		s.ch = make(chan struct{})
	}
	return s.ch
}

func (s *signal) fire() {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.ch != nil {
		close(s.ch)
		s.ch = nil
	}
}

// Recorded returns a channel that closes once a write that recorded an
// audit event commits in this process. Call it again for the next one.
// Another process sharing the store, such as dockwell recover, doesn't
// close it: whoever needs those events reads the store again from time to
// time as well.
func (db *DB) Recorded() <-chan struct{} { return db.recorded.wait() }
