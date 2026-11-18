// SPDX-License-Identifier: AGPL-3.0-only

package connections

import (
	"context"
	"log/slog"
	"slices"
	"strings"
	"sync"
	"time"

	"google.golang.org/protobuf/proto"

	dockwellv1 "github.com/dockwell/dockwell/gen/dockwell/v1"
)

// Checker checks each saved connection on a schedule and keeps each one's last
// check from each server, in memory (#799): once it's saved or changed, with Check's
// full check, which writes, reads and deletes an object or signs in; and every Every
// after, with a check that writes nothing (ADR 0099): storage's signed read, and a
// mail server's sign-in, which sends nothing. Each goes through the server's agent,
// as Check's does, so it reaches only what the agent's guard lets a check reach.
//
// A connection is checked from each server an app linked to it runs on, on that
// app's networks, as the app reaches it, or from the control server where none
// does, as its keys go nowhere else. One check of a connection runs at a time, and
// one at a time on each server, so the agent's other place is always a person's
// Check's or an install's (#799's review, M4). A provider that says it's had too many
// requests, or that refuses the keys or the sign-in, is asked half as often each
// time, down to once in Every times 4, until it answers again: a revoked password
// tried every 6 hours could lock its account. A check no server could run is owed,
// and tried again at the next look, then less often while it can't run (M1).
type Checker struct {
	s *Service
	// Every is how often each connection is checked again, and Tick how often the
	// schedule is looked at.
	Every time.Duration
	Tick  time.Duration
	// Now is the clock, for a test; nil is time.Now.
	Now func() time.Time

	// listed, saved, targets and probe read the saved connections, say where one
	// is checked from, and check it there: the store's, the fleet's and an agent's,
	// but in a test.
	listed  func(ctx context.Context) ([]Saved, error)
	saved   func(ctx context.Context) ([]Saved, []string, error)
	targets func(ctx context.Context, name string) []Target
	probe   func(ctx context.Context, c Saved, at Target, full bool) (*dockwellv1.CheckConnectionResponse, error)

	mu sync.Mutex
	// results and plans are by connection ID, so one deleted and saved again under
	// its name starts afresh (#799's review, L2); results then by server ID.
	results map[string]map[string]kept
	plans   map[string]*plan
	running sync.WaitGroup
	// slots holds one place for each server a check of the schedule runs on.
	slots map[string]chan struct{}
	// looked is once the first look after a start has run: a connection it finds
	// after that was saved since, and gets the full check.
	looked bool
	// unread are the connections the control plane can't open, said once each.
	unread map[string]bool
}

// kept is a connection's last check from one server, and the revision it checked.
type kept struct {
	revision int
	check    *dockwellv1.CheckConnectionResponse
}

// plan is when a connection is checked next.
type plan struct {
	// busy is while a check of it runs; gone, that it was deleted meanwhile.
	busy, gone bool
	// revision is the revision the plan is for, and checked whether a check of it
	// was answered, at tried, by the schedule.
	revision int
	checked  bool
	tried    time.Time
	// owed is the full check, for a connection saved or changed since the first
	// look after a start, until a server answers it (#799's review, M1).
	owed bool
	// retry is when a check no server could run is tried again, and missed how many
	// in a row couldn't run, each putting the next twice as far off, from Tick.
	retry  time.Time
	missed int
	// limited is how many checks in a row a provider has turned away, for its rate
	// or the keys, up to 2: each puts the next twice as far off.
	limited int
}

// Every's and the rest's defaults.
const (
	checkEvery = 6 * time.Hour
	checkTick  = time.Minute
	// checkWait is how long one check may take, from a server's agent.
	checkWait = 2 * time.Minute
	// maxMissed caps how far off a check no server could run is put: 2^6 ticks, an
	// hour.
	maxMissed = 6
)

func newChecker(s *Service) *Checker {
	c := &Checker{s: s, Every: checkEvery, Tick: checkTick}
	c.listed = func(ctx context.Context) ([]Saved, error) { return s.connections.Revisions(ctx) }
	c.saved = func(ctx context.Context) ([]Saved, []string, error) { return s.connections.Readable(ctx) }
	c.targets = c.from
	c.probe = s.checkSaved
	return c
}

func (c *Checker) clock() time.Time {
	if c.Now != nil {
		return c.Now()
	}
	return time.Now()
}

// Run checks each connection when it's due, until ctx ends, and returns once the
// checks it started have.
func (c *Checker) Run(ctx context.Context) {
	defer c.running.Wait()
	every := time.NewTicker(c.Tick)
	defer every.Stop()
	for {
		c.Check(ctx)
		select {
		case <-ctx.Done():
			return
		case <-every.C:
		}
	}
}

// Check starts a check of each connection that's due, and returns without waiting
// for them; Wait waits. It holds its lock only over what it keeps: the store and the
// fleet are read outside it (#799's review, L4).
func (c *Checker) Check(ctx context.Context) {
	listed, err := c.listed(ctx)
	if err != nil {
		if ctx.Err() == nil {
			c.logger().WarnContext(ctx, "can't read the connections to check them; the next look tries again", slog.String("error", err.Error()))
		}
		return
	}
	now := c.clock()
	due, full := c.dueNow(listed, now)
	if len(due) == 0 {
		return
	}
	all, unread, err := c.saved(ctx)
	if err != nil {
		if ctx.Err() == nil {
			c.logger().WarnContext(ctx, "can't read the connections to check them; the next look tries again", slog.String("error", err.Error()))
		}
		return
	}
	c.sayUnread(ctx, unread)
	type start struct {
		conn Saved
		at   []Target
	}
	var starts []start
	opened := map[string]bool{}
	for _, conn := range all {
		opened[conn.ID] = true
		if rev, ok := due[conn.ID]; !ok || rev != conn.Revision {
			continue
		}
		if at := c.targets(ctx, conn.Name); len(at) > 0 {
			starts = append(starts, start{conn, at})
		}
		// Nothing can check it now, so nothing went out: the next look asks again.
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.slots == nil {
		c.slots = map[string]chan struct{}{}
	}
	// One that can't be opened waits as one no server could check does, rather
	// than open every connection again each minute.
	for id := range due {
		if p := c.plans[id]; p != nil && !opened[id] {
			c.missedOne(p, now)
		}
	}
	for _, s := range starts {
		p := c.plans[s.conn.ID]
		if p == nil || p.busy || p.gone || p.revision != s.conn.Revision {
			continue
		}
		for _, t := range s.at {
			if c.slots[t.Server] == nil {
				c.slots[t.Server] = make(chan struct{}, 1)
			}
		}
		p.busy = true
		c.running.Add(1)
		go c.checkOne(ctx, s.conn, s.at, full[s.conn.ID])
	}
}

// dueNow says, under the lock, which connections are due, by ID at their revision,
// and which of those get the full check; it drops what's gone, but for one whose
// check is running, which goes once it ends.
func (c *Checker) dueNow(listed []Saved, now time.Time) (due map[string]int, full map[string]bool) {
	c.mu.Lock()
	defer c.mu.Unlock()
	due, full = map[string]int{}, map[string]bool{}
	saved := map[string]bool{}
	for _, conn := range listed {
		saved[conn.ID] = true
	}
	for id, p := range c.plans {
		if saved[id] {
			continue
		}
		if p.busy {
			p.gone = true
			continue
		}
		delete(c.plans, id)
		delete(c.results, id)
	}
	if c.plans == nil {
		c.plans = map[string]*plan{}
	}
	for _, conn := range listed {
		p := c.plans[conn.ID]
		if p == nil {
			// One the first look after a start finds may be saved long since: it's
			// read. One a later look finds was saved since, and is owed the full check.
			p = &plan{revision: conn.Revision, owed: c.looked}
			c.plans[conn.ID] = p
		}
		if p.revision != conn.Revision && !p.busy {
			// Changed since, checked or not: the full check is owed, at once.
			p.revision, p.owed, p.checked = conn.Revision, true, false
			p.retry, p.missed = time.Time{}, 0
		}
		if !p.busy && c.due(p, now) {
			due[conn.ID] = conn.Revision
			full[conn.ID] = p.owed
		}
	}
	c.looked = true
	return due, full
}

// due says a connection is checked now: one no server could check, once its wait
// is over; one not checked at its revision; one whose last check is Every old, or
// more after a provider turned checks away.
func (c *Checker) due(p *plan, now time.Time) bool {
	if !p.retry.IsZero() {
		return !now.Before(p.retry)
	}
	if !p.checked {
		return true
	}
	return !now.Before(p.tried.Add(c.Every << p.limited))
}

// missedOne puts off a connection no server could check, by Tick at first, then
// twice as long each time, up to an hour: at the next look, then less often while
// its servers can't run it, as an agent busy with checks of its own.
func (c *Checker) missedOne(p *plan, now time.Time) {
	p.missed = min(p.missed+1, maxMissed)
	p.retry = now.Add(c.Tick << (p.missed - 1))
}

// sayUnread logs each connection the control plane can't open, once, so the rest
// are checked and the log doesn't grow each minute (#799's review, L1).
func (c *Checker) sayUnread(ctx context.Context, names []string) {
	c.mu.Lock()
	defer c.mu.Unlock()
	now := map[string]bool{}
	for _, name := range names {
		now[name] = true
		if c.unread[name] {
			continue
		}
		c.logger().WarnContext(ctx, "can't open the connection to check it, so it isn't checked until it's saved again. Save its keys again from its page",
			slog.String("connection", name))
	}
	c.unread = now
}

// Wait returns once the checks started have.
func (c *Checker) Wait() { c.running.Wait() }

// from is where a connection is checked from now: each connected server an app
// linked to it runs on, on that app's networks, or the control server, on a network
// of the check's own, where none does.
func (c *Checker) from(ctx context.Context, name string) []Target {
	if c.s.fleet == nil {
		return nil
	}
	linked, err := c.s.connections.Targets(ctx, name)
	if err != nil {
		return nil
	}
	var on, control []Target
	for _, srv := range c.s.fleet.List() {
		if srv.GetConnection() != dockwellv1.Connection_CONNECTION_CONNECTED {
			continue
		}
		if t, ok := linked[srv.GetId()]; ok {
			on = append(on, t)
		}
		if srv.GetControlServer() {
			control = append(control, Target{Server: srv.GetId()})
		}
	}
	if len(on) > 0 {
		slices.SortFunc(on, func(a, b Target) int { return strings.Compare(a.Server, b.Server) })
		return on
	}
	return control
}

// checkOne checks a connection from each of its targets, in turn, each in its
// server's one place.
func (c *Checker) checkOne(ctx context.Context, conn Saved, targets []Target, full bool) {
	defer c.running.Done()
	got := map[string]kept{}
	limited := false
	defer c.done(conn, targets, full, got, &limited)
	for _, t := range targets {
		c.mu.Lock()
		slot := c.slots[t.Server]
		c.mu.Unlock()
		select {
		case slot <- struct{}{}:
		case <-ctx.Done():
			return
		}
		one, cancel := context.WithTimeout(ctx, checkWait)
		answer, err := c.probe(one, conn, t, full)
		cancel()
		<-slot
		if err != nil {
			if ctx.Err() == nil {
				c.logger().WarnContext(ctx, "can't check the connection now; it's tried again at the next look",
					slog.String("connection", conn.Name), slog.String("server", t.Server), slog.String("error", err.Error()))
			}
			continue
		}
		got[t.Server] = kept{revision: conn.Revision, check: answer}
		if TurnedAway(answer) {
			limited = true
			break // the provider asked for fewer, or refused: the other servers wait too
		}
	}
}

// done keeps what a check found. One no server answered isn't done: a full check
// stays owed and it's tried again soon (#799's review, M1). Each server's last
// result stays until that server answers again, or stops being one it's checked
// from (L3).
func (c *Checker) done(conn Saved, targets []Target, full bool, got map[string]kept, limited *bool) {
	c.mu.Lock()
	defer c.mu.Unlock()
	p := c.plans[conn.ID]
	if p == nil {
		return
	}
	p.busy = false
	if p.gone {
		delete(c.plans, conn.ID)
		delete(c.results, conn.ID)
		return
	}
	now := c.clock()
	from := map[string]bool{}
	for _, t := range targets {
		from[t.Server] = true
	}
	if c.results == nil {
		c.results = map[string]map[string]kept{}
	}
	held := c.results[conn.ID]
	for srv := range held {
		if !from[srv] {
			delete(held, srv)
		}
	}
	if len(got) == 0 {
		c.missedOne(p, now)
		return
	}
	if held == nil {
		held = map[string]kept{}
		c.results[conn.ID] = held
	}
	for srv, k := range got {
		held[srv] = k
	}
	if p.revision != conn.Revision {
		return // changed while it ran: the next look checks the new one
	}
	p.checked, p.tried = true, now
	p.retry, p.missed = time.Time{}, 0
	if full {
		p.owed = false
	}
	if *limited {
		p.limited = min(p.limited+1, 2)
	} else {
		p.limited = 0
	}
}

// record keeps a check of a saved connection, as saved, from a server: one a
// person ran, which leaves the schedule as it is, but for a provider's limit.
func (c *Checker) record(conn Saved, server string, check *dockwellv1.CheckConnectionResponse) {
	if conn.ID == "" || conn.Revision == 0 || check == nil {
		return
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.results == nil {
		c.results = map[string]map[string]kept{}
	}
	if c.results[conn.ID] == nil {
		c.results[conn.ID] = map[string]kept{}
	}
	c.results[conn.ID][server] = kept{revision: conn.Revision, check: proto.CloneOf(check)}
	if p := c.plans[conn.ID]; p != nil && TurnedAway(check) {
		p.limited = min(p.limited+1, 2)
	}
}

// Of is a connection's checks of revision, by its ID, newest first, each its own
// copy.
func (c *Checker) Of(id string, revision int) []*dockwellv1.CheckConnectionResponse {
	c.mu.Lock()
	defer c.mu.Unlock()
	var out []*dockwellv1.CheckConnectionResponse
	for _, k := range c.results[id] {
		if k.revision == revision {
			out = append(out, proto.CloneOf(k.check))
		}
	}
	slices.SortFunc(out, func(a, b *dockwellv1.CheckConnectionResponse) int {
		if n := b.GetTime().AsTime().Compare(a.GetTime().AsTime()); n != 0 {
			return n
		}
		return strings.Compare(a.GetServer(), b.GetServer())
	})
	return out
}

// Limited says a check was turned away for the provider's rate: S3's 429 or its
// SlowDown and kin, or a mail server's 421, or a 4xx with a 4.7 code.
func Limited(r *dockwellv1.CheckConnectionResponse) bool {
	if r.GetPassed() {
		return false
	}
	if r.GetStatus() == 429 {
		return true
	}
	code := r.GetCode()
	switch code {
	case "SlowDown", "TooManyRequests", "Throttling", "ThrottlingException", "RequestLimitExceeded", "TooManyRequestsException", "RateLimitExceeded":
		return true
	}
	return strings.HasPrefix(code, "421") || strings.HasPrefix(code, "4") && strings.Contains(code, " 4.7.")
}

// Refused says a check's keys or sign-in were refused: S3's word that the key or
// its signature is wrong (R2 says Unauthorized for a key it doesn't know), or that
// it may not, or a mail server's 530, 534 or 535. A scheduled read that can't
// confirm the keys without writing, as AccessDenied answered both its reads, isn't
// one: the keys may work, and reads don't lock an account.
func Refused(r *dockwellv1.CheckConnectionResponse) bool {
	if r.GetPassed() {
		return false
	}
	code := r.GetCode()
	switch code {
	case "AccessDenied":
		return r.GetStep() != "list"
	case "InvalidAccessKeyId", "SignatureDoesNotMatch", "Unauthorized", "InvalidToken", "ExpiredToken":
		return true
	}
	return strings.HasPrefix(code, "530") || strings.HasPrefix(code, "534") || strings.HasPrefix(code, "535")
}

// TurnedAway says the provider limited or refused a check, so the next waits.
func TurnedAway(r *dockwellv1.CheckConnectionResponse) bool { return Limited(r) || Refused(r) }

func (c *Checker) logger() *slog.Logger {
	if c.s.connections != nil && c.s.connections.Logger != nil {
		return c.s.connections.Logger
	}
	return slog.New(slog.DiscardHandler)
}
