// SPDX-License-Identifier: AGPL-3.0-only

package health

import (
	"cmp"
	"context"
	"log/slog"
	"slices"
	"strconv"
	"strings"
	"sync"
	"time"

	"google.golang.org/protobuf/types/known/durationpb"
	"google.golang.org/protobuf/types/known/timestamppb"

	dockwellv1 "github.com/dockwell/dockwell/gen/dockwell/v1"
	"github.com/dockwell/dockwell/internal/docker"
)

// What a Monitor holds itself to. It runs on every server, so its cost has a
// ceiling whatever a server runs: each container costs a few short strings, a
// check holds no goroutine between runs, and no more of them run at once than
// it has workers, so a server of hung apps makes checks late, and takes no more
// memory.
const (
	// DefaultScan is how often a Monitor lists the server's containers.
	DefaultScan = 5 * time.Second
	// DefaultRefresh is how often it says how checks went, when no state
	// changed, so "the last check, and when" stays true.
	DefaultRefresh = 30 * time.Second
	// DefaultWorkers is how many checks it makes at once.
	DefaultWorkers = 16
	// MaxInstances is how many containers it watches. Any more read as nobody
	// having said how they are, which is true, and it says so once in its log.
	MaxInstances = 4096
	// perInstance is how much a list of containers slows it down per container,
	// as listing them is the costly part.
	perInstance = 10 * time.Millisecond
	// inspects is how many containers it reads in full per scan, beyond those
	// whose state just changed, oldest read first, to read what a command check
	// last printed and when a container last started.
	inspects = 8
	// downScan is how often it looks while Docker doesn't answer.
	downScan = 30 * time.Second
	// DefaultCallTimeout bounds one call to Docker. A Docker that takes the
	// connection and doesn't answer, as a stuck storage driver makes it, is one
	// that can't say.
	DefaultCallTimeout = 10 * time.Second
	// staleScans is how many scans' time may pass since the last good list before
	// what only Docker can say reads as not known, whatever the call is doing.
	staleScans = 3
	// freshFor is the least a state may stand for with nothing new heard: a report
	// goes out at Refresh's pace, so anything shorter would flicker.
	freshFor = 90 * time.Second
	// slowCheck is how long a check has to take to go in the slow lane.
	slowCheck = time.Second
	// servicePrefix starts the Compose project of what lives in an environment,
	// its ID in lowercase. Compose's other projects on the server are their own.
	servicePrefix = "env_"
)

// Monitor watches the containers of a server's services, for as long as they
// run: it reads what Docker's own checks say, and makes the checks the agent
// makes. It holds nothing it can't read again, so it starts over with the agent,
// and needs no control plane to go on.
type Monitor struct {
	Client *docker.Client
	Logger *slog.Logger
	// Scan, Refresh, CallTimeout and Workers are the defaults above, when zero.
	Scan, Refresh, CallTimeout time.Duration
	Workers                    int
	// Probe makes a check. Nil means Run.
	Probe func(ctx context.Context, c *Check, address string) (Result, error)

	mu      sync.Mutex
	watched map[string]*watched // by container ID
	busy    int                 // checks under way
	slow    int                 // of those, what took its time last time
	warned  bool
	dirty   bool // something to say that Report hasn't said
	said    time.Time
	signal  chan struct{}
	wake    chan struct{} // a check ended, or a list came, so one may be due
	once    sync.Once
	// scannedAt is when a list last came. Until one has, nothing can be said.
	scannedAt time.Time
	// blindSince is since when Docker hasn't been answering. Zero while it is.
	blindSince time.Time
	// started is when the monitor started, which is since when Docker hasn't
	// answered, until a first list has come.
	started time.Time
	// onList is told each list as it comes (OnList).
	onList func(ctx context.Context, at time.Time, list []docker.ContainerSummary)
}

// OnList has fn told each list of the server's containers as it comes, with when
// it was read, so the agent can note how what it keeps a record of stands without
// a second look of its own (ADR 0054). fn runs on the scan's own loop, so it
// should be quick, and a later fn takes the place of an earlier one.
func (m *Monitor) OnList(fn func(ctx context.Context, at time.Time, list []docker.ContainerSummary)) {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.onList = fn
}

// watched is what a Monitor keeps of one container.
type watched struct {
	project, service, name string
	number                 int32
	kind                   dockwellv1.HealthCheckKind
	check                  *Check
	address                string
	started                time.Time
	state                  dockwellv1.HealthState
	since                  time.Time
	last                   *Result
	failing                int32
	nextCheck, lastCounted time.Time
	probing                bool
	dockerState, status    string
	seen                   bool      // in the latest list
	readAt                 time.Time // when it was last read in full
	slow                   bool      // its last check took its time
	finished               bool      // it exited 0, and nothing starts it again
}

func (m *Monitor) init() {
	m.once.Do(func() {
		m.signal = make(chan struct{}, 1)
		m.wake = make(chan struct{}, 1)
		m.watched = map[string]*watched{}
		m.started = time.Now()
	})
}

func (m *Monitor) logger() *slog.Logger {
	if m.Logger != nil {
		return m.Logger
	}
	return slog.Default()
}

// Changed signals when Report has something to say.
func (m *Monitor) Changed() <-chan struct{} {
	m.init()
	return m.signal
}

// DockerUnreadableSince is since when Docker hasn't answered: a list failed, and
// none has come since, or none has come yet, as when the agent starts with Docker
// down or hung, which is since the monitor started. It is the zero time while
// Docker answers.
func (m *Monitor) DockerUnreadableSince() time.Time {
	m.init()
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.blindSince.IsZero() && m.scannedAt.IsZero() {
		return m.started
	}
	return m.blindSince
}

// ScannedAt is when a list of the server's containers last came from Docker: the
// zero time until one has.
func (m *Monitor) ScannedAt() time.Time {
	m.init()
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.scannedAt
}

// Report is how each container is, sorted by project, service and number. It is
// nil until a list has come from Docker, as a monitor that has read nothing can't
// say what runs. What only Docker can say reads as not known while Docker doesn't
// answer. The checks the agent makes go on saying what they find.
func (m *Monitor) Report() []*dockwellv1.InstanceHealth {
	m.init()
	m.mu.Lock()
	defer m.mu.Unlock()
	now := time.Now()
	// Having read nothing is said too, on the same pace, as Docker not answering.
	m.dirty, m.said = false, now
	if m.scannedAt.IsZero() {
		return nil
	}
	blind := m.isBlind()
	out := make([]*dockwellv1.InstanceHealth, 0, len(m.watched))
	for id, w := range m.watched {
		h := &dockwellv1.InstanceHealth{
			Project: w.project, Service: w.service, Number: w.number, Container: w.name, ContainerId: id, State: w.state, Check: w.kind,
			Failing: w.failing, Since: timestamppb.New(w.since), Finished: w.finished,
		}
		if r := w.last; r != nil {
			h.Last = &dockwellv1.HealthResult{Passed: r.Passed, Output: r.Output, Time: timestamppb.New(r.At)}
			if r.Took > 0 {
				h.Last.Took = durationpb.New(r.Took)
			}
		}
		confirmed, goodFor := m.confirmed(w)
		h.ConfirmedAt, h.GoodFor = timestamppb.New(confirmed), durationpb.New(goodFor)
		if w.check == nil && blind {
			h.State, h.Since = dockwellv1.HealthState_HEALTH_STATE_UNKNOWN, timestamppb.New(m.blindFrom(now))
		} else if now.Sub(confirmed) > goodFor && !confirmed.IsZero() {
			h.State = dockwellv1.HealthState_HEALTH_STATE_UNKNOWN
		}
		out = append(out, h)
	}
	slices.SortFunc(out, func(a, b *dockwellv1.InstanceHealth) int {
		return cmp.Or(strings.Compare(a.GetProject(), b.GetProject()), strings.Compare(a.GetService(), b.GetService()), cmp.Compare(a.GetNumber(), b.GetNumber()))
	})
	return out
}

// confirmed says when a container's state was last known to hold, and how old that
// may be and still stand: for a check the agent makes, when it last finished one,
// and its own pace with room for a server that is behind; for anything else, and for
// what isn't running, which no check looks at, what the last list said.
func (m *Monitor) confirmed(w *watched) (time.Time, time.Duration) {
	if w.check == nil || w.last == nil || w.dockerState != dockerRunning {
		return m.scannedAt, max(freshFor, staleScans*m.scanEvery(len(m.watched)))
	}
	return w.last.At, max(freshFor, 2*w.check.IntervalOr()+w.check.TimeoutOr()+m.refreshEvery())
}

// isBlind reports Docker not answering: a list failed, which one that hangs does when
// it runs out of time.
func (m *Monitor) isBlind() bool { return !m.blindSince.IsZero() }

// blindFrom is since when it has been blind, for a report.
func (m *Monitor) blindFrom(now time.Time) time.Time {
	if !m.blindSince.IsZero() {
		return m.blindSince
	}
	return now
}

func (m *Monitor) scanEvery(n int) time.Duration {
	every := m.Scan
	if every <= 0 {
		every = DefaultScan
	}
	return max(every, time.Duration(n)*perInstance)
}

func (m *Monitor) refreshEvery() time.Duration {
	if m.Refresh > 0 {
		return m.Refresh
	}
	return DefaultRefresh
}

func (m *Monitor) callTimeout() time.Duration {
	if m.CallTimeout > 0 {
		return m.CallTimeout
	}
	return DefaultCallTimeout
}

// tell says there is something new.
func (m *Monitor) tell() {
	select {
	case m.signal <- struct{}{}:
	default:
	}
}

// Run watches until ctx is done. Scanning has a loop of its own, as a Docker that
// hangs mustn't stop the checks the agent makes, which need nothing of Docker.
func (m *Monitor) Run(ctx context.Context) {
	m.init()
	done := make(chan struct{})
	go func() {
		defer close(done)
		m.scanLoop(ctx)
	}()
	defer func() { <-done }()
	timer := time.NewTimer(0)
	defer timer.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-m.wake:
			if !timer.Stop() {
				select {
				case <-timer.C:
				default:
				}
			}
		case <-timer.C:
		}
		now := time.Now()
		next := m.startChecks(ctx, now)
		m.refresh(now)
		wait := m.refreshEvery()
		if !next.IsZero() {
			wait = min(wait, next.Sub(now))
		}
		timer.Reset(max(wait, checkFloor))
	}
}

// dockerRunning is the word Docker's list has for a container that runs, and isn't
// paused or restarting.
const dockerRunning = "running"

// checkFloor keeps the loop from spinning on a check due right now.
const checkFloor = time.Millisecond

func (m *Monitor) scanLoop(ctx context.Context) {
	for {
		wait := m.scan(ctx)
		m.nudge()
		select {
		case <-ctx.Done():
			return
		case <-time.After(wait):
		}
	}
}

// nudge wakes the checks' loop.
func (m *Monitor) nudge() {
	select {
	case m.wake <- struct{}{}:
	default:
	}
}

// refresh says how things are once in a while, whether or not anything changed: a
// report's arrival is how the control plane knows the agent is still looking. One
// that has read nothing yet says that Docker hasn't answered, on the same pace.
func (m *Monitor) refresh(now time.Time) {
	m.mu.Lock()
	due := now.Sub(m.said) >= m.refreshEvery()
	m.mu.Unlock()
	if due {
		m.tell()
	}
}

// scan reads the list of containers, and returns how long until the next. The list
// says where each container is, and that goes straight to what a check dials, as a
// container that restarted between two scans may have a new address, and another
// container its old one.
func (m *Monitor) scan(ctx context.Context) time.Duration {
	list, err := m.list(ctx)
	if err != nil {
		if !docker.IsUnavailable(err) && ctx.Err() == nil {
			m.logger().WarnContext(ctx, "can't list the server's containers, so their health isn't known for now; the agent tries again", slog.String("error", err.Error()))
		}
		m.mu.Lock()
		first := m.blindSince.IsZero()
		if first {
			m.blindSince = time.Now()
			if m.scannedAt.IsZero() {
				m.blindSince = m.started // it hasn't answered since the start
			}
		}
		m.dirty = true
		m.mu.Unlock()
		if first {
			m.tell()
		}
		return min(downScan, max(m.scanEvery(0), downScan/time.Duration(staleScans*2)))
	}
	type read struct {
		id string
		w  *watched
	}
	var toRead []read
	now := time.Now()
	m.mu.Lock()
	wasBlind := !m.blindSince.IsZero() || m.scannedAt.IsZero()
	m.blindSince, m.scannedAt = time.Time{}, now
	for _, w := range m.watched {
		w.seen = false
	}
	changed := wasBlind
	var quiet []read
	for k := range list {
		s := &list[k]
		project := s.Labels[docker.LabelProject]
		if !strings.HasPrefix(project, servicePrefix) || s.Labels[docker.LabelOneoff] == "True" {
			continue
		}
		w := m.watched[s.ID]
		if w == nil {
			if len(m.watched) >= MaxInstances {
				if !m.warned {
					m.warned = true
					m.logger().WarnContext(ctx, "this server has more containers than the agent watches, so the health of the rest reads as not known", slog.Int("watched", MaxInstances))
				}
				continue
			}
			w = &watched{
				project: project, service: s.Labels[docker.LabelService], number: int32(s.Number()), //nolint:gosec // a container's number
				name: strings.TrimPrefix(firstName(s.Names), "/"), state: dockwellv1.HealthState_HEALTH_STATE_UNKNOWN, since: time.Now(),
			}
			if label := s.Labels[Label]; label != "" {
				if c, err := Parse(label); err == nil {
					w.check = c
					w.kind = kindOf(c)
				}
			}
			m.watched[s.ID] = w
			toRead = append(toRead, read{s.ID, w})
		} else {
			if s.State != w.dockerState || healthWord(s.Status) != healthWord(w.status) {
				toRead = append(toRead, read{s.ID, w})
			} else {
				quiet = append(quiet, read{s.ID, w})
			}
			w.name = strings.TrimPrefix(firstName(s.Names), "/")
		}
		w.seen = true
		w.dockerState, w.status = s.State, s.Status
		m.listed(w, s.State)
		if a := Address(s.NetworkSettings.Networks); a != "" {
			w.address = a
		}
	}
	for id, w := range m.watched {
		if !w.seen {
			delete(m.watched, id)
			changed = true
		}
	}
	// The ones read longest ago, so a server of many gets round them all.
	slices.SortFunc(quiet, func(a, b read) int { return a.w.readAt.Compare(b.w.readAt) })
	quiet = quiet[:min(len(quiet), inspects)]
	if changed {
		m.dirty = true
	}
	n := len(m.watched)
	onList := m.onList
	m.mu.Unlock()
	if changed {
		m.tell()
	}
	if onList != nil {
		onList(ctx, now, list)
	}
	for _, r := range slices.Concat(toRead, quiet) {
		m.read(ctx, r.id, r.w)
	}
	return m.scanEvery(n)
}

// list reads the list of containers, for no longer than a call may take.
func (m *Monitor) list(ctx context.Context) ([]docker.ContainerSummary, error) {
	ctx, cancel := context.WithTimeout(ctx, m.callTimeout())
	defer cancel()
	return m.Client.ListContainers(ctx, map[string][]string{"label": {docker.LabelProject}})
}

func firstName(names []string) string {
	if len(names) == 0 {
		return ""
	}
	return names[0]
}

func kindOf(c *Check) dockwellv1.HealthCheckKind {
	if c.HTTP != nil {
		return dockwellv1.HealthCheckKind_HEALTH_CHECK_KIND_HTTP
	}
	return dockwellv1.HealthCheckKind_HEALTH_CHECK_KIND_TCP
}

// healthWord is what a list's status line says of a health check, as Docker has
// worded it for years: it is what tells a container's state changed, without
// reading each one in full each time.
func healthWord(status string) string {
	switch {
	case strings.Contains(status, "(healthy)"):
		return "healthy"
	case strings.Contains(status, "(unhealthy)"):
		return "unhealthy"
	case strings.Contains(status, "(health: starting)"):
		return "starting"
	}
	return ""
}

// read reads a container in full, and says how it is now.
func (m *Monitor) read(ctx context.Context, id string, w *watched) {
	ctx, cancel := context.WithTimeout(ctx, m.callTimeout())
	defer cancel()
	c, err := m.Client.InspectContainer(ctx, id)
	if err != nil {
		return // the next list says whether it has gone
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.watched[id] != w {
		return
	}
	w.readAt = time.Now()
	s := c.State
	restarted := !c.State.StartedAt.IsZero() && !c.State.StartedAt.Equal(w.started)
	w.started = c.State.StartedAt
	w.address = Address(c.NetworkSettings.Networks)
	if w.check == nil {
		switch hc := c.Config.Healthcheck; {
		case hc != nil && len(hc.Test) > 0 && hc.Test[0] != "NONE":
			w.kind = dockwellv1.HealthCheckKind_HEALTH_CHECK_KIND_COMMAND
		default:
			w.kind = dockwellv1.HealthCheckKind_HEALTH_CHECK_KIND_NONE
		}
	}
	if s.Running || s.Restarting {
		w.finished = false
	}
	switch {
	case s.Restarting:
		m.foundOut(w, "the container keeps restarting: it last exited with code "+strconv.Itoa(s.ExitCode))
		m.setState(w, dockwellv1.HealthState_HEALTH_STATE_UNHEALTHY)
	case s.Paused:
		w.failing = 0
		m.foundOut(w, pausedWords)
		m.setState(w, dockwellv1.HealthState_HEALTH_STATE_PAUSED)
	case !s.Running:
		w.failing = 0
		m.foundOut(w, "the container isn't running: it is "+cmp.Or(s.Status, "stopped")+", with exit code "+strconv.Itoa(s.ExitCode))
		if f := finished(c); f != w.finished {
			w.finished, m.dirty = f, true
		}
		m.setState(w, dockwellv1.HealthState_HEALTH_STATE_NOT_RUNNING)
	case w.check != nil:
		// The checks say. A container that started again begins again.
		if restarted {
			w.failing, w.lastCounted, w.nextCheck = 0, time.Time{}, time.Time{}
			m.setState(w, dockwellv1.HealthState_HEALTH_STATE_STARTING)
		} else if w.state != dockwellv1.HealthState_HEALTH_STATE_HEALTHY && w.state != dockwellv1.HealthState_HEALTH_STATE_UNHEALTHY {
			m.setState(w, dockwellv1.HealthState_HEALTH_STATE_STARTING)
		}
	case w.kind == dockwellv1.HealthCheckKind_HEALTH_CHECK_KIND_NONE || s.Health == nil:
		w.last, w.failing = nil, 0
		m.setState(w, dockwellv1.HealthState_HEALTH_STATE_NO_CHECK)
	default:
		if r, ok := FromDocker(s.Health, SpecEnv(c.Config.Env, c.Config.Labels)); ok && (w.last == nil || !r.At.Equal(w.last.At)) {
			w.last = &r
			m.dirty = true
		}
		w.failing = int32(s.Health.FailingStreak) //nolint:gosec // a count of checks
		switch s.Health.Status {
		case "healthy":
			m.setState(w, dockwellv1.HealthState_HEALTH_STATE_HEALTHY)
		case "unhealthy":
			m.setState(w, dockwellv1.HealthState_HEALTH_STATE_UNHEALTHY)
		default:
			m.setState(w, dockwellv1.HealthState_HEALTH_STATE_STARTING)
		}
	}
}

// finished reports a container that ran to its end: it exited 0, and Docker won't
// start it again, as a one-shot job such as a migration does once its work is done.
func finished(c *docker.Container) bool {
	if c.State.ExitCode != 0 || c.State.Status != "exited" {
		return false
	}
	switch c.HostConfig.RestartPolicy.Name {
	case "", "no", "on-failure":
		return true
	}
	return false
}

// pausedWords is what a paused container comes to. Docker counts it as running and
// stops its checks, so what they last said is as it left it, and says nothing.
const pausedWords = "the container is paused, so it serves nothing and no check runs. Unpause it with docker unpause"

// listed reads what the list says of a container that doesn't run. That stands
// whether or not a read gets through, which only says more about it. What runs, or
// is restarting, takes a read to say what it comes to.
func (m *Monitor) listed(w *watched, state string) {
	switch state {
	case dockerRunning, "restarting", "":
	case "paused":
		if w.state != dockwellv1.HealthState_HEALTH_STATE_PAUSED {
			w.last, w.failing = &Result{Output: pausedWords, At: time.Now()}, 0
			m.setState(w, dockwellv1.HealthState_HEALTH_STATE_PAUSED)
		}
	default:
		if w.state != dockwellv1.HealthState_HEALTH_STATE_NOT_RUNNING {
			w.last, w.failing = &Result{Output: "the container isn't running: it is " + state, At: time.Now()}, 0
			m.setState(w, dockwellv1.HealthState_HEALTH_STATE_NOT_RUNNING)
		}
	}
}

// foundOut records what a read found out that no check did. What's new goes out at once:
// a list has said it doesn't run by now, and this is why, such as how it exited. What
// isn't new says nothing, as each read finds the same.
func (m *Monitor) foundOut(w *watched, output string) {
	if w.last != nil && w.last.Output == output {
		w.last.At = time.Now()
		return
	}
	w.last = &Result{Output: output, At: time.Now()}
	m.dirty = true
	m.tell()
}

// setState moves a container's state on, and says so at once.
func (m *Monitor) setState(w *watched, state dockwellv1.HealthState) {
	if w.state == state {
		return
	}
	w.state, w.since = state, time.Now()
	m.dirty = true
	m.tell()
}

// startChecks starts the checks that are due, as many as there are workers, and
// says when the next one is due; the zero time for none that a worker's ending
// doesn't start. What took its time last time gets at most half the workers, so
// checks that hang can't hold up one that would have said a service has died.
func (m *Monitor) startChecks(ctx context.Context, now time.Time) time.Time {
	workers := m.Workers
	if workers <= 0 {
		workers = DefaultWorkers
	}
	slowLane := max(workers/2, 1)
	m.mu.Lock()
	defer m.mu.Unlock()
	var next time.Time
	for _, w := range m.watched {
		if w.check == nil || w.probing || w.dockerState != dockerRunning || w.address == "" {
			continue
		}
		if now.Before(w.nextCheck) {
			if next.IsZero() || w.nextCheck.Before(next) {
				next = w.nextCheck
			}
			continue
		}
		if m.busy >= workers || w.slow && m.slow >= slowLane {
			continue // a worker's ending wakes the loop
		}
		m.busy++
		w.probing = true
		wasSlow := w.slow
		if wasSlow {
			m.slow++
		}
		go m.check(ctx, w, w.check, w.address, wasSlow)
	}
	return next
}

// check makes one check, and takes in how it went.
func (m *Monitor) check(ctx context.Context, w *watched, c *Check, address string, wasSlow bool) {
	probe := m.Probe
	if probe == nil {
		probe = Run
	}
	began := time.Now()
	r, err := probe(ctx, c, address)
	defer m.nudge()
	m.mu.Lock()
	defer m.mu.Unlock()
	m.busy--
	if wasSlow {
		m.slow--
	}
	w.probing = false
	if err != nil || w.dockerState != dockerRunning {
		return // ctx ended, or what it asked has stopped running, so it says nothing now
	}
	now := time.Now()
	w.slow = now.Sub(began) >= min(slowCheck, c.TimeoutOr())
	w.nextCheck = now.Add(c.IntervalOr())
	w.last = &r
	m.dirty = true
	if r.Passed {
		w.failing = 0
		m.setState(w, dockwellv1.HealthState_HEALTH_STATE_HEALTHY)
		return
	}
	// A failure in the start period doesn't count, as Docker has it.
	if now.Sub(w.started) < c.StartPeriod {
		return
	}
	w.failing++
	if int(w.failing) >= c.RetriesOr() {
		m.setState(w, dockwellv1.HealthState_HEALTH_STATE_UNHEALTHY)
	}
}
