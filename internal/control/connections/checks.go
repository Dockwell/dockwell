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
// after, with a check that writes nothing (ADR 0099): storage's one signed list, and
// a mail server's sign-in, which sends nothing. Each goes through the server's agent,
// as Check's does, so it reaches only what the agent's guard lets a check reach.
//
// A connection is checked from each server an app linked to it runs on, as the app
// reaches it, or from the control server where none does, as its keys go nowhere
// else. One check of a connection runs at a time, and Slots at once in all. A
// provider that says it's had too many requests, or that refuses the keys or the
// sign-in, is asked half as often each time, down to once in Every times 4, until
// it answers again: a revoked password tried every 6 hours could lock its account
// (ADR 0099's checks back off after a refusal).
type Checker struct {
	s *Service
	// Every is how often each connection is checked again; Tick how often the
	// schedule is looked at; Slots how many connections are checked at once.
	Every time.Duration
	Tick  time.Duration
	Slots int
	// Now is the clock, for a test; nil is time.Now.
	Now func() time.Time

	// saved, servers and probe read the saved connections, say where one is
	// checked from, and check it from a server: the store's, the fleet's and an
	// agent's, but in a test.
	listed  func(ctx context.Context) ([]Saved, error)
	saved   func(ctx context.Context) ([]Saved, error)
	servers func(ctx context.Context, name string) []string
	probe   func(ctx context.Context, c Saved, server string, full bool) (*dockwellv1.CheckConnectionResponse, error)

	mu      sync.Mutex
	results map[string]map[string]kept // by connection name, then server ID
	plans   map[string]*plan           // by connection name
	running sync.WaitGroup
	slots   chan struct{}
	// looked is once the first look after a start has run: a connection it finds
	// after that was saved since, and gets the full check.
	looked bool
}

// kept is a connection's last check from one server, and the revision it checked.
type kept struct {
	revision int
	check    *dockwellv1.CheckConnectionResponse
}

// plan is when a connection is checked next.
type plan struct {
	// busy is while a check of it runs.
	busy bool
	// revision is the revision last checked, and tried when, by the schedule.
	revision int
	tried    time.Time
	// limited is how many checks in a row a provider has turned away, for its rate
	// or the keys, up to 2: each puts the next twice as far off.
	limited int
	// fresh is a connection saved since the first look after a start, not checked
	// yet: its first check is the full one.
	fresh bool
}

// Every's and the rest's defaults.
const (
	checkEvery = 6 * time.Hour
	checkTick  = time.Minute
	checkSlots = 2
	// checkWait is how long one check may take, from a server's agent.
	checkWait = 2 * time.Minute
)

func newChecker(s *Service) *Checker {
	c := &Checker{s: s, Every: checkEvery, Tick: checkTick, Slots: checkSlots}
	c.listed = func(ctx context.Context) ([]Saved, error) { return s.connections.Revisions(ctx) }
	c.saved = func(ctx context.Context) ([]Saved, error) { return s.connections.All(ctx) }
	c.servers = c.from
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
// for them; Wait waits.
func (c *Checker) Check(ctx context.Context) {
	listed, err := c.listed(ctx)
	if err != nil {
		if ctx.Err() == nil {
			c.logger().WarnContext(ctx, "can't read the connections to check them; the next look tries again", slog.String("error", err.Error()))
		}
		return
	}
	now := c.clock()
	// What's due, by name, at its revision: a look at names and revisions alone,
	// which opens no secret, as nothing is due most minutes.
	due := map[string]int{}
	full := map[string]bool{}
	c.mu.Lock()
	saved := map[string]bool{}
	for _, conn := range listed {
		saved[conn.Name] = true
	}
	// A connection that's gone keeps no checks.
	for name := range c.results {
		if !saved[name] {
			delete(c.results, name)
		}
	}
	for name := range c.plans {
		if !saved[name] {
			delete(c.plans, name)
		}
	}
	if c.plans == nil {
		c.plans = map[string]*plan{}
	}
	for _, conn := range listed {
		p := c.plans[conn.Name]
		if p == nil {
			// One the first look after a start finds may be saved long since: it's
			// read. One a later look finds was saved since, and gets the full check.
			p = &plan{fresh: c.looked}
			c.plans[conn.Name] = p
		}
		if !p.busy && c.due(p, conn.Revision, now) {
			due[conn.Name] = conn.Revision
			// Saved or changed since: the full check, as Check's, which writes and
			// reads an object. On its schedule: a read that writes nothing (ADR 0099).
			full[conn.Name] = p.fresh || (p.revision != 0 && p.revision != conn.Revision)
		}
	}
	c.looked = true
	c.mu.Unlock()
	if len(due) == 0 {
		return
	}
	all, err := c.saved(ctx)
	if err != nil {
		if ctx.Err() == nil {
			c.logger().WarnContext(ctx, "can't read the connections to check them; the next look tries again", slog.String("error", err.Error()))
		}
		return
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.slots == nil {
		c.slots = make(chan struct{}, max(1, c.Slots))
	}
	for _, conn := range all {
		p := c.plans[conn.Name]
		if rev, ok := due[conn.Name]; !ok || rev != conn.Revision || p == nil || p.busy {
			continue
		}
		servers := c.servers(ctx, conn.Name)
		if len(servers) == 0 {
			// Nothing can check it now, so nothing went out: the next look asks again.
			continue
		}
		p.busy = true
		c.running.Add(1)
		go c.checkOne(ctx, conn, servers, full[conn.Name])
	}
}

// Wait returns once the checks started have.
func (c *Checker) Wait() { c.running.Wait() }

// due says a connection is checked now: it's never been, it's changed since, or its
// last check is Every old, or more after a provider turned checks away.
func (c *Checker) due(p *plan, revision int, now time.Time) bool {
	if p.revision != revision || p.tried.IsZero() {
		return true
	}
	return !now.Before(p.tried.Add(c.Every << p.limited))
}

// from is the servers a connection is checked from now, by ID: each connected one an
// app linked to it runs on, or the control server where none does.
func (c *Checker) from(ctx context.Context, name string) []string {
	if c.s.fleet == nil {
		return nil
	}
	runs, err := c.s.connections.ServersOf(ctx, name)
	if err != nil {
		return nil
	}
	var linked, control []string
	for _, srv := range c.s.fleet.List() {
		if srv.GetConnection() != dockwellv1.Connection_CONNECTION_CONNECTED {
			continue
		}
		if runs[srv.GetId()] {
			linked = append(linked, srv.GetId())
		}
		if srv.GetControlServer() {
			control = append(control, srv.GetId())
		}
	}
	if len(linked) > 0 {
		slices.Sort(linked)
		return linked
	}
	return control
}

// checkOne checks a connection from each of servers, in turn, in one of the slots.
func (c *Checker) checkOne(ctx context.Context, conn Saved, servers []string, full bool) {
	defer c.running.Done()
	name, revision := conn.Name, conn.Revision
	got := map[string]kept{}
	limited := false
	defer func() {
		c.mu.Lock()
		defer c.mu.Unlock()
		p := c.plans[name]
		if p == nil {
			return // removed while it ran
		}
		p.busy, p.fresh = false, false
		p.revision, p.tried = revision, c.clock()
		switch {
		case limited:
			p.limited = min(p.limited+1, 2)
		case len(got) > 0:
			p.limited = 0
		}
		if len(got) == 0 {
			return
		}
		// What it found replaces what was kept, so a server that no longer runs a
		// linked app keeps none.
		if c.results == nil {
			c.results = map[string]map[string]kept{}
		}
		c.results[name] = got
	}()
	select {
	case c.slots <- struct{}{}:
	case <-ctx.Done():
		return
	}
	defer func() { <-c.slots }()
	for _, srv := range servers {
		one, cancel := context.WithTimeout(ctx, checkWait)
		answer, err := c.probe(one, conn, srv, full)
		cancel()
		if err != nil {
			if ctx.Err() == nil {
				c.logger().WarnContext(ctx, "can't check the connection now; it's checked again at the next schedule",
					slog.String("connection", name), slog.String("server", srv), slog.String("error", err.Error()))
			}
			continue
		}
		got[srv] = kept{revision: revision, check: answer}
		if TurnedAway(answer) {
			limited = true
			break // the provider asked for fewer, or refused: the other servers wait too
		}
	}
}

// record keeps a check of a saved connection, as saved, from a server: one a
// person ran, which leaves the schedule as it is, but for a provider's limit.
func (c *Checker) record(name string, revision int, server string, check *dockwellv1.CheckConnectionResponse) {
	if revision == 0 || check == nil {
		return
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.results == nil {
		c.results = map[string]map[string]kept{}
	}
	if c.results[name] == nil {
		c.results[name] = map[string]kept{}
	}
	c.results[name][server] = kept{revision: revision, check: proto.CloneOf(check)}
	if p := c.plans[name]; p != nil && TurnedAway(check) {
		p.limited = min(p.limited+1, 2)
	}
}

// Of is a connection's checks of revision, newest first, each its own copy.
func (c *Checker) Of(name string, revision int) []*dockwellv1.CheckConnectionResponse {
	c.mu.Lock()
	defer c.mu.Unlock()
	var out []*dockwellv1.CheckConnectionResponse
	for _, k := range c.results[name] {
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
// it may not, or a mail server's 530, 534 or 535. A scheduled read that AccessDenied
// answered passed, so it isn't one.
func Refused(r *dockwellv1.CheckConnectionResponse) bool {
	if r.GetPassed() {
		return false
	}
	switch r.GetCode() {
	case "InvalidAccessKeyId", "SignatureDoesNotMatch", "Unauthorized", "AccessDenied", "InvalidToken", "ExpiredToken":
		return true
	}
	code := r.GetCode()
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
