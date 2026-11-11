// SPDX-License-Identifier: AGPL-3.0-only

// Package checks runs the install's checks of a URL from the control plane, each
// on its own clock, and keeps how each stands in memory (ADR 0066).
package checks

import (
	"bytes"
	"context"
	"crypto/x509"
	"errors"
	"fmt"
	"hash/fnv"
	"io"
	"log/slog"
	"net/http"
	"net/url"
	"slices"
	"strconv"
	"strings"
	"sync"
	"time"
	"unicode/utf8"

	"google.golang.org/protobuf/proto"
	"google.golang.org/protobuf/types/known/durationpb"
	"google.golang.org/protobuf/types/known/timestamppb"

	resourcev1 "github.com/dockwell/dockwell/gen/dockwell/resource/v1alpha1"
	dockwellv1 "github.com/dockwell/dockwell/gen/dockwell/v1"
	"github.com/dockwell/dockwell/internal/netguard"
	"github.com/dockwell/dockwell/internal/resource/alert"
	"github.com/dockwell/dockwell/internal/secrets"
	"github.com/dockwell/dockwell/internal/store"
)

const (
	// Slots is how many checks run at once.
	Slots = 16
	// KeepRuns is how many of a check's last runs it keeps, for the panel.
	KeepRuns = 10
	// MaxRead is the most of an answer a run reads, only to look for a keyword.
	MaxRead = 64 << 10
	// reload is how often the runner reads the install's checks again.
	reload = 2 * time.Second
	// From is where a check runs from, in words, until checks run from servers.
	From = "the control server"
	// Unreadable is the problem of a check whose spec Dockwell can't read.
	Unreadable = "Dockwell can't read this check's spec, so it doesn't run. Change the check, or remove it and make it again"
	// spreadNew is how far apart checks found new in one read start, so checks
	// made by one plan don't all ask at once.
	spreadNew = 2 * time.Second
	// UpAfter is how many runs in a row have to pass for a check that's down to read
	// up again, as down_after failed runs made it down: so a site that fails two runs
	// in three stays one outage, at any interval (ADR 0066, decided in review).
	UpAfter = 2
	// MaxSaid is the most a run keeps of what it says, in bytes: what a far end
	// controls, as a certificate's names or where a redirect leads, can't make a
	// run large, as each check keeps its last runs and every list carries them.
	MaxSaid = 300
	// AlertThing starts what a check's alert is about, before the check's ID.
	AlertThing = "chk:"
	// skippedWords is what a run that couldn't start says.
	skippedWords = "skipped: all 16 runs at once were taken, as checks waited out their timeouts"
)

// Doer sends a request: netguard's client, or a test's. It follows no redirect: a
// check takes a redirect as the answer.
type Doer interface {
	Do(*http.Request) (*http.Response, error)
}

// Guard is what a check may reach from the control plane: the internet alone, or
// with inside, the private networks the control server is on too, as an alert
// channel's test may; never this server's loopback, nor a metadata or link-local
// address. No redirect followed, and no more read than a keyword needs.
func Guard(inside bool) Doer {
	return netguard.Client(netguard.Options{Private: inside, MaxBody: MaxRead, Timeout: alert.MaxCheckTimeout})
}

// Status is how a check stands, for the API and the alerts it raises.
type Status struct {
	ID, Name string
	Spec     *resourcev1.AlertCheck
	State    dockwellv1.AlertCheckState
	// Since is when it came to stand so; for a check that's down, when the runs
	// that made it so began to fail.
	Since time.Time
	// Runs are its last runs, newest first.
	Runs []*dockwellv1.AlertCheckRun
	// Problem is why it can't run now, such as a spec Dockwell can't read; while
	// there's one, the check keeps the state it had.
	Problem string
}

// Decoder reads a stored spec: the install's secrets.Keeper.
type Decoder interface {
	Decode(ctx context.Context, tx *store.Tx, at store.SpecAt, stored []byte) (proto.Message, error)
}

var _ Decoder = (*secrets.Keeper)(nil)

// Runner runs the install's checks.
type Runner struct {
	DB     *store.DB
	Keeper Decoder
	// Client runs checks of the internet, and Inside those inside the network.
	// Nil is Guard's.
	Client, Inside Doer
	Logger         *slog.Logger
	// Now is the time. Nil is time.Now.
	Now func() time.Time

	once   sync.Once
	mu     sync.Mutex
	checks map[string]*tracked
	loaded bool
	slots  chan struct{}
	// warned is the revision of each check a warning said can't be read, so each
	// is said once, not at every read.
	warned map[string]int
}

type tracked struct {
	Status
	next    time.Time
	running bool
	fails   int
	failing time.Time
	// passes are the runs in a row that passed since a check that's down last
	// failed.
	passes int
}

// start starts a check again, knowing nothing of its runs: waiting for its first,
// or down since its alert opened while that's open, so it takes UpAfter passes in
// a row to resolve, as before a restart or an edit, and not the first alone.
func (t *tracked) start(downSince map[string]time.Time) {
	t.State, t.Since, t.Runs, t.fails, t.failing, t.passes = dockwellv1.AlertCheckState_ALERT_CHECK_STATE_WAITING, time.Time{}, nil, 0, time.Time{}, 0
	if since, ok := downSince[t.ID]; ok {
		t.State, t.Since = dockwellv1.AlertCheckState_ALERT_CHECK_STATE_DOWN, since
	}
}

func (r *Runner) now() time.Time {
	if r.Now != nil {
		return r.Now()
	}
	return time.Now()
}

func (r *Runner) init() {
	r.once.Do(func() {
		r.checks, r.warned = map[string]*tracked{}, map[string]int{}
		r.slots = make(chan struct{}, Slots)
		if r.Client == nil {
			r.Client = Guard(false)
		}
		if r.Inside == nil {
			r.Inside = Guard(true)
		}
		if r.Logger == nil {
			r.Logger = slog.New(slog.DiscardHandler)
		}
	})
}

// Run runs every check as it falls due, until ctx ends.
func (r *Runner) Run(ctx context.Context) {
	r.init()
	tick := time.NewTicker(time.Second)
	defer tick.Stop()
	var read time.Time
	for {
		if r.now().Sub(read) >= reload {
			if err := r.Load(ctx); err != nil && ctx.Err() == nil {
				r.Logger.WarnContext(ctx, "can't read the install's checks", slog.Any("error", err))
			}
			read = r.now()
		}
		r.Due(ctx)
		select {
		case <-ctx.Done():
			return
		case <-tick.C:
		}
	}
}

// readCheck is a check as Load reads it from the store.
type readCheck struct {
	id, name string
	spec     *resourcev1.AlertCheck
	problem  string
}

// starting reports whether any check read starts again: new to the runner, read
// again after it couldn't be, or changed.
func (r *Runner) starting(checks []readCheck) bool {
	r.mu.Lock()
	defer r.mu.Unlock()
	for i := range checks {
		c := &checks[i]
		if c.problem != "" {
			continue
		}
		if t := r.checks[c.id]; t == nil || t.Spec == nil || !proto.Equal(t.Spec, c.spec) {
			return true
		}
	}
	return false
}

// Load reads the install's checks. One made since the last read runs at once, so
// the person who made it sees its first answer; those read as the control plane
// starts are spread over their interval, so checks made together don't run
// together. A check whose spec changed starts again, waiting for its next run.
func (r *Runner) Load(ctx context.Context) error {
	r.init()
	var checks []readCheck
	// Since when each check's alert has been open, for one that starts again.
	downSince := map[string]time.Time{}
	if err := r.DB.View(ctx, func(tx *store.Tx) error {
		stored, err := tx.ListKind(ctx, string(alert.CheckKind))
		if err != nil {
			return err
		}
		for i := range stored {
			s := &stored[i]
			spec, err := r.Keeper.Decode(ctx, tx, store.InRevision(s.ID, s.Kind, s.Revision), s.Spec)
			c, ok := spec.(*resourcev1.AlertCheck)
			if err != nil || !ok {
				// It stays, with why, and keeps the alert it had open: what can't
				// be read isn't up. Said once for each revision, not at each read.
				r.mu.Lock()
				if r.warned[s.ID] != s.Revision {
					r.warned[s.ID] = s.Revision
					r.Logger.WarnContext(ctx, "can't read a check", slog.String("check", s.Name), slog.Any("error", err))
				}
				r.mu.Unlock()
				checks = append(checks, readCheck{id: s.ID, name: s.Name, problem: Unreadable})
				continue
			}
			checks = append(checks, readCheck{id: s.ID, name: s.Name, spec: c})
		}
		return nil
	}); err != nil {
		return err
	}
	if r.starting(checks) {
		if err := r.DB.View(ctx, func(tx *store.Tx) error {
			open, err := tx.OpenAlerts(ctx)
			if err != nil {
				return err
			}
			for i := range open {
				if id, ok := strings.CutPrefix(open[i].Thing, AlertThing); ok {
					if was, seen := downSince[id]; !seen || open[i].OpenedAt.Before(was) {
						downSince[id] = open[i].OpenedAt
					}
				}
			}
			return nil
		}); err != nil {
			return err
		}
	}
	now := r.now()
	r.mu.Lock()
	defer r.mu.Unlock()
	seen := map[string]bool{}
	fresh := 0
	for _, s := range checks {
		spec := s.spec
		seen[s.id] = true
		t := r.checks[s.id]
		switch {
		case s.problem != "":
			if t == nil {
				t = &tracked{Status: Status{ID: s.id, Name: s.name, State: dockwellv1.AlertCheckState_ALERT_CHECK_STATE_WAITING}}
				r.checks[s.id] = t
			}
			t.Name, t.Problem = s.name, s.problem
		case t == nil:
			// Those made since the last read start a few seconds apart; those
			// read as the control plane starts, over their interval.
			next := now.Add(time.Duration(fresh) * spreadNew)
			fresh++
			if !r.loaded {
				next = now.Add(spread(s.id, alert.Every(spec)))
			}
			t = &tracked{Status: Status{ID: s.id, Name: s.name, Spec: spec}, next: next}
			t.start(downSince)
			r.checks[s.id] = t
		case t.Spec == nil:
			// It can be read again: it starts again.
			t.Spec, t.Name, t.Problem = spec, s.name, ""
			t.start(downSince)
			t.next = now
		case !proto.Equal(t.Spec, spec):
			t.Spec, t.Name = spec, s.name
			t.start(downSince)
			t.next = now
		default:
			t.Name, t.Problem = s.name, ""
		}
	}
	for id := range r.checks {
		if !seen[id] {
			delete(r.checks, id)
			delete(r.warned, id)
		}
	}
	r.loaded = true
	return nil
}

// spread is where in its interval a check runs, the same each start.
func spread(id string, every time.Duration) time.Duration {
	h := fnv.New32a()
	_, _ = h.Write([]byte(id))
	return time.Duration(h.Sum32()) % every
}

// Due starts each check whose time has come, Slots at a time at most. It never
// waits for a slot: a run that can't start is skipped, says so, and the check
// runs again at its next interval, so one slow minute doesn't hold up reading
// the checks again, nor the checks after it.
func (r *Runner) Due(ctx context.Context) {
	r.init()
	now := r.now()
	r.mu.Lock()
	defer r.mu.Unlock()
	for _, t := range r.checks {
		if t.running || t.Spec == nil || t.Problem != "" || now.Before(t.next) {
			continue
		}
		select {
		case r.slots <- struct{}{}:
		default:
			t.next = now.Add(alert.Every(t.Spec))
			t.Runs = slices.Insert(t.Runs, 0, &dockwellv1.AlertCheckRun{Time: timestamppb.New(now), Skipped: true, Said: skippedWords})
			if len(t.Runs) > KeepRuns {
				t.Runs = t.Runs[:KeepRuns]
			}
			continue
		}
		t.running = true
		go func() {
			defer func() { <-r.slots }()
			r.run(ctx, t)
		}()
	}
}

// Loaded reports whether the runner has read the install's checks yet: until it
// has, it knows nothing of how any stands.
func (r *Runner) Loaded() bool {
	r.init()
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.loaded
}

// RunNow runs a check at once and waits for its answer, as a test does.
func (r *Runner) RunNow(ctx context.Context, id string) (Status, bool) {
	r.init()
	r.mu.Lock()
	t := r.checks[id]
	if t == nil || t.running || t.Spec == nil || t.Problem != "" {
		r.mu.Unlock()
		return Status{}, false
	}
	t.running = true
	r.mu.Unlock()
	r.run(ctx, t)
	st, ok := r.Status(id)
	return st, ok
}

func (r *Runner) run(ctx context.Context, t *tracked) {
	r.mu.Lock()
	spec := proto.CloneOf(t.Spec)
	r.mu.Unlock()
	client := r.Client
	if spec.GetInsideNetwork() {
		client = r.Inside
	}
	run := Probe(ctx, client, spec, r.now)
	r.mu.Lock()
	defer r.mu.Unlock()
	t.running = false
	if ctx.Err() != nil {
		return // cut short as the control plane stops: it says nothing of the address
	}
	if !proto.Equal(spec, t.Spec) {
		return // it changed while it ran: the next run is of what it is now
	}
	t.next = run.GetTime().AsTime().Add(alert.Every(spec))
	t.Runs = slices.Insert(t.Runs, 0, run)
	if len(t.Runs) > KeepRuns {
		t.Runs = t.Runs[:KeepRuns]
	}
	was := t.State
	down := t.State == dockwellv1.AlertCheckState_ALERT_CHECK_STATE_DOWN
	switch {
	case run.GetOk() && down:
		// Down until UpAfter runs in a row pass.
		t.passes++
		if t.passes >= UpAfter {
			t.State, t.passes, t.fails, t.failing = dockwellv1.AlertCheckState_ALERT_CHECK_STATE_UP, 0, 0, time.Time{}
		}
	case run.GetOk():
		t.State, t.fails, t.failing = dockwellv1.AlertCheckState_ALERT_CHECK_STATE_UP, 0, time.Time{}
	case down:
		// Still down, and the passes start again.
		t.passes = 0
	default:
		if t.fails == 0 {
			t.failing = run.GetTime().AsTime()
		}
		t.fails++
		t.State = dockwellv1.AlertCheckState_ALERT_CHECK_STATE_FAILING
		if t.fails >= int(max(spec.GetDownAfter(), 1)) {
			t.State = dockwellv1.AlertCheckState_ALERT_CHECK_STATE_DOWN
		}
	}
	if t.State != was {
		t.Since = run.GetTime().AsTime()
		if t.State == dockwellv1.AlertCheckState_ALERT_CHECK_STATE_DOWN {
			t.Since = t.failing
		}
	}
}

// Probe makes one run of a check: a GET, with no redirect followed, that reads the
// answer only as far as its keyword needs, and keeps nothing of it but MaxSaid
// bytes of words.
func Probe(ctx context.Context, client Doer, spec *resourcev1.AlertCheck, now func() time.Time) (run *dockwellv1.AlertCheckRun) {
	if now == nil {
		now = time.Now
	}
	start := now()
	run = &dockwellv1.AlertCheckRun{Time: timestamppb.New(start)}
	defer func() { run.Said = clip(run.Said, MaxSaid) }()
	timeout := alert.Timeout(spec)
	ctx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, spec.GetAddress(), http.NoBody)
	if err != nil {
		run.Said = "the address doesn't read as a URL"
		return run
	}
	req.Header.Set("User-Agent", "Dockwell-Check/1 (+https://github.com/Dockwell/dockwell)")
	res, err := client.Do(req)
	run.Took = durationpb.New(now().Sub(start))
	if err != nil {
		run.Said = words(ctx, err, timeout, req.URL.Hostname())
		return run
	}
	defer res.Body.Close()
	run.Status = uint32(res.StatusCode) //nolint:gosec // a status is three digits
	if res.StatusCode < int(spec.GetExpectFrom()) || res.StatusCode > int(spec.GetExpectTo()) {
		run.Said = fmt.Sprintf("%d, outside %d to %d", res.StatusCode, spec.GetExpectFrom(), spec.GetExpectTo())
		return run
	}
	if k := spec.GetKeyword(); k != "" {
		if res.StatusCode >= 300 && res.StatusCode < 400 {
			// A redirect holds no page to look in: say where it went.
			to := "another address"
			if loc, err := res.Location(); err == nil && loc.Host != "" {
				to = loc.Host
			}
			run.Said = "it moved to " + to
			return run
		}
		body, err := io.ReadAll(io.LimitReader(res.Body, MaxRead))
		if err != nil && !errors.Is(err, netguard.ErrTooLarge) {
			run.Said = words(ctx, err, timeout, req.URL.Hostname())
			return run
		}
		held := bytes.Contains(body, []byte(k))
		switch {
		case !held && !spec.GetKeywordAbsent():
			run.Said = "it doesn't hold " + strconv.Quote(k)
			return run
		case held && spec.GetKeywordAbsent():
			run.Said = "it holds " + strconv.Quote(k)
			return run
		}
	}
	run.Ok = true
	return run
}

// words says why a run got no answer, with nothing of a URL past its host.
func words(ctx context.Context, err error, timeout time.Duration, host string) string {
	if errors.Is(ctx.Err(), context.DeadlineExceeded) {
		return "no answer within " + alert.FormatDuration(timeout)
	}
	var u *url.Error
	if errors.As(err, &u) {
		err = u.Err
	}
	// A certificate's own words list every name it holds: what matters is that
	// it isn't valid for the address asked.
	var name x509.HostnameError
	var authority x509.UnknownAuthorityError
	var refused *netguard.Refused
	switch {
	case errors.As(err, &name):
		return "its certificate isn't valid for " + host
	case errors.As(err, &authority):
		return "its certificate isn't signed by an authority the control server trusts"
	case errors.As(err, &refused) && netguard.Forbidden(refused.Addr.String(), netguard.Options{Private: true}) == "":
		// What a check inside the network would reach.
		return netguard.Words(err) + ". Turn on Inside the network for this check to reach it"
	}
	return netguard.Words(err)
}

// clip keeps s to at most n bytes, cut on a rune, with an ellipsis where it's cut.
// Bytes that aren't UTF-8, which a far end can put in a redirect's host, become U+FFFD
// first: a run's words go into alerts, which neither Postgres nor protobuf takes otherwise.
func clip(s string, n int) string {
	s = strings.ToValidUTF8(s, "\uFFFD")
	if len(s) <= n {
		return s
	}
	cut := n - len("…")
	for cut > 0 && !utf8.RuneStart(s[cut]) {
		cut--
	}
	return s[:cut] + "…"
}

// Statuses are how every check stands, by name.
func (r *Runner) Statuses() []Status {
	r.init()
	r.mu.Lock()
	defer r.mu.Unlock()
	out := make([]Status, 0, len(r.checks))
	for _, t := range r.checks {
		out = append(out, t.copy())
	}
	slices.SortFunc(out, func(a, b Status) int { return compare(a.Name, b.Name) })
	return out
}

// Status is how one check stands.
func (r *Runner) Status(id string) (Status, bool) {
	r.init()
	r.mu.Lock()
	defer r.mu.Unlock()
	t := r.checks[id]
	if t == nil {
		return Status{}, false
	}
	return t.copy(), true
}

func (t *tracked) copy() Status {
	s := t.Status
	s.Spec = proto.CloneOf(t.Spec)
	s.Runs = slices.Clone(t.Runs)
	return s
}

func compare(a, b string) int {
	switch {
	case a < b:
		return -1
	case a > b:
		return 1
	}
	return 0
}
