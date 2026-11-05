// SPDX-License-Identifier: AGPL-3.0-only

// Package logs reads a service's logs on its server, for the control plane to
// pass on: from Docker for what it has, and from a directory of the agent's
// own for the containers a change removed. Log lines pass through here and
// sit nowhere else: what a reader can't take is dropped and counted, never
// queued without a limit. See docs/adr/0025-logs-per-deploy.md.
package logs

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"log/slog"
	"slices"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"google.golang.org/protobuf/proto"
	"google.golang.org/protobuf/types/known/timestamppb"

	agentv1 "github.com/dockwell/dockwell/gen/dockwell/agent/v1"
	dockwellv1 "github.com/dockwell/dockwell/gen/dockwell/v1"
	"github.com/dockwell/dockwell/internal/docker"
	"github.com/dockwell/dockwell/internal/resource/container"
)

// Limits of reading logs on one server. See also budget.go.
const (
	// MaxSourcesPerStream bounds how many containers one reader reads at once.
	MaxSourcesPerStream = 32
	// searchLimit bounds how long one search reads.
	searchLimit = 30 * time.Second
	// searchTail is how many of a container's newest lines a search reads.
	searchTail = 1_000_000
	// matchBytes bounds what a search holds of one container while it reads on,
	// newest kept: lines cost what lineCost says, whatever their text.
	matchBytes = 2 << 20
	// streamMatchBytes bounds it for all of one stream's containers. So a server
	// holds at most this for each of its readers, however many containers a
	// service has.
	streamMatchBytes = 8 << 20
	// minMatchBytes is the least a container gets of that, which holds a line
	// of the longest kind a few times over.
	minMatchBytes = 64 << 10
	// heldLineBytes is about what a line held in memory costs beyond its text:
	// the message, its time, its text's header, and its place in the ring.
	heldLineBytes = 200
	// chunkBytes is how much a message to the control plane holds.
	chunkBytes = 32 << 10
	// flushEvery is how long a line waits to be sent at most. With the control
	// plane's own gathering and the panel's, a line reaches a browser well
	// within a second.
	flushEvery = 50 * time.Millisecond
	// queued is how many messages wait for the network before a live stream
	// starts to drop: 256 KiB in all.
	queued = 8
	// scanEvery is how often a following reader looks for containers that
	// started or stopped.
	scanEvery = 500 * time.Millisecond
	// idleLive is how long a container's log can stay quiet before what it had
	// counts as read: its first line can take longer.
	idleLive, idleFirst = 250 * time.Millisecond, time.Second
	// gapEvery is how often a stream says what it dropped of one source, at
	// most, so a flood doesn't fill the view with markers.
	gapEvery = time.Second
)

// Reader reads logs on this server. It is safe for concurrent use.
type Reader struct {
	Client  *docker.Client
	Archive *Archive
	Logger  *slog.Logger
	// LiveRate, TotalRate and ShedRate replace the limits, in bytes a second,
	// in tests.
	LiveRate, TotalRate, ShedRate int
	// StuckAfter replaces how long a message can wait for a reader, in tests.
	StuckAfter time.Duration
	// SearchLines replaces how many lines a search reads of a container, in tests.
	SearchLines int

	placesMu sync.Mutex
	places   map[*stream]bool

	scansOnce sync.Once
	scans     chan struct{}
}

func (r *Reader) logger() *slog.Logger {
	if r.Logger != nil {
		return r.Logger
	}
	return slog.Default()
}

// Sources lists the containers a service has logs from, newest first.
func (r *Reader) Sources(ctx context.Context, ref *dockwellv1.ResourceRef, forgottenUpTo int64) ([]*dockwellv1.LogSource, error) {
	found, err := r.sources(ctx, docker.Project(ref.GetScope()), ref.GetName(), true, forgottenUpTo)
	if err != nil {
		return nil, err
	}
	out := make([]*dockwellv1.LogSource, len(found))
	for i, s := range found {
		out[i] = s.proto
	}
	return out, nil
}

// source is one place logs can be read from.
type source struct {
	proto *dockwellv1.LogSource
	// id is the container's full ID, for Docker; kept what the archive has.
	id   string
	tty  bool
	kept *Kept
}

// Forgotten says which services' kept logs no reader may see any more.
func (r *Reader) Forgotten(services []*agentv1.ForgottenService) {
	if r.Archive != nil {
		r.Archive.Forgotten(services)
	}
}

// sources lists a service's containers: those Docker has, and, with kept, those
// the archive has that Docker no longer does, leaving out what it kept of revisions
// up to forgottenUpTo.
func (r *Reader) sources(ctx context.Context, project, service string, kept bool, forgottenUpTo int64) ([]*source, error) {
	list, err := r.Client.ListContainers(ctx, map[string][]string{
		"label": {docker.LabelProject + "=" + project, docker.LabelService + "=" + service},
	})
	if err != nil {
		return nil, err
	}
	out := make([]*source, 0, len(list))
	have := map[string]bool{}
	for _, s := range list {
		if s.Labels[docker.LabelOneoff] == "True" {
			continue
		}
		c, err := r.Client.InspectContainer(ctx, s.ID)
		if docker.IsNotFound(err) {
			continue
		}
		if err != nil {
			return nil, err
		}
		have[shortID(c.ID)] = true
		out = append(out, &source{proto: dockerSource(c), id: c.ID, tty: c.Config.Tty})
	}
	if kept && r.Archive != nil {
		for _, k := range r.Archive.List(project, service) {
			if !have[k.ContainerID] && (forgottenUpTo == 0 || k.Revision > forgottenUpTo) {
				out = append(out, &source{proto: keptSource(k), kept: k})
			}
		}
	}
	slices.SortStableFunc(out, func(a, b *source) int {
		return b.proto.GetCreateTime().AsTime().Compare(a.proto.GetCreateTime().AsTime())
	})
	return out, nil
}

// theReplica is the replica of every container of a service, as a service has one on
// a server. A rollout gives each new container the next number (ADR 0027), and they
// take turns as that one replica, so a number isn't what tells replicas apart.
const theReplica = 1

func dockerSource(c *docker.Container) *dockwellv1.LogSource {
	l := c.Config.Labels
	revision, _ := strconv.ParseInt(l[container.LabelRevision], 10, 64)
	s := &dockwellv1.LogSource{
		Id:            shortID(c.ID),
		Replica:       theReplica,
		ContainerName: container.PlainName(c.Name),
		Revision:      revision,
		ChangeId:      l[container.LabelChange],
		Image:         c.Config.Image,
		State:         dockerState(c.State),
		CreateTime:    stamp(c.Created),
		StartTime:     stamp(c.State.StartedAt),
	}
	if !c.State.Running && !c.State.FinishedAt.IsZero() {
		s.FinishTime = stamp(c.State.FinishedAt)
		s.ExitCode = int32(c.State.ExitCode) //nolint:gosec // an exit code is small
	}
	if lc := c.HostConfig.LogConfig; lc != nil {
		s.Unbounded = lc.Unbounded()
	}
	return s
}

func dockerState(s docker.State) dockwellv1.LogSourceState {
	switch {
	case s.Restarting:
		return dockwellv1.LogSourceState_LOG_SOURCE_STATE_RESTARTING
	case s.Running:
		return dockwellv1.LogSourceState_LOG_SOURCE_STATE_RUNNING
	case s.Status == "created":
		return dockwellv1.LogSourceState_LOG_SOURCE_STATE_CREATED
	}
	return dockwellv1.LogSourceState_LOG_SOURCE_STATE_STOPPED
}

func keptSource(k *Kept) *dockwellv1.LogSource {
	s := &dockwellv1.LogSource{
		Id:            k.ContainerID,
		Replica:       theReplica,
		ContainerName: k.Name,
		Revision:      k.Revision,
		ChangeId:      k.Change,
		Image:         k.Image,
		State:         dockwellv1.LogSourceState_LOG_SOURCE_STATE_KEPT,
		CreateTime:    stamp(k.Created),
		StartTime:     stamp(k.Started),
		FinishTime:    stamp(k.Finished),
		ExitCode:      int32(k.ExitCode), //nolint:gosec // an exit code is small
		Kept:          dockwellv1.LogKept_LOG_KEPT_REPLACED,
		CutShort:      k.CutShort,
	}
	if k.Failed {
		s.Kept = dockwellv1.LogKept_LOG_KEPT_FAILED
	}
	return s
}

func stamp(t time.Time) *timestamppb.Timestamp {
	if t.IsZero() {
		return nil
	}
	return timestamppb.New(t)
}

// Stream reads what a logs call asks for, and passes it to send, until it has
// read it all, or follows until ctx ends. send may block: what follows drops
// what it can't send, and says how much, and what reads the past waits. A send
// that stays blocked past a limit ends the stream, with send still blocked, as
// no reader is there: ending it is then up to send's own context.
func (r *Reader) Stream(ctx context.Context, call *agentv1.LogsCall, send func(*agentv1.SendLogsRequest) error) error {
	if err := checkCall(call); err != nil {
		return err
	}
	ctx, cancel := context.WithCancelCause(ctx)
	defer cancel(nil)
	s := &stream{
		r: r, call: call, ctx: ctx, cancel: cancel, start: time.Now(),
		project: docker.Project(call.GetResource().GetScope()), service: call.GetResource().GetName(), reader: call.GetReader(),
		out:  make(chan *agentv1.SendLogsRequest, queued),
		rate: newBucket(),
		shed: &pacer{},
		open: map[string]*openChunk{}, gaps: map[string]*gap{}, states: map[string]*sourceState{},
		said: map[string]string{}, goneSaid: map[string]bool{},
	}
	s.from = timeOf(call.GetSince())
	if last := call.GetLast(); last != nil {
		// By this server's clock, which its lines' times are on.
		s.from = s.start.Add(-last.AsDuration())
	}
	if err := r.join(s); err != nil {
		return err
	}
	defer r.leave(s)
	s.filter = newFilter(call)
	for _, c := range call.GetAfter() {
		s.state(c.GetSourceId()).after = c
	}
	for _, c := range call.GetBefore() {
		st := s.state(c.GetSourceId())
		st.before, st.beforeCount = c.GetTime().AsTime(), c.GetCount()
	}
	stuck := r.StuckAfter
	if stuck <= 0 {
		stuck = stuckAfter
	}
	// Sending has a goroutine of its own, so a network that blocks stalls
	// neither reading nor the drop counts.
	sent := make(chan struct{})
	go func() {
		defer close(sent)
		watch := time.AfterFunc(stuck, func() { cancel(ErrStuck) })
		defer watch.Stop()
		watch.Stop()
		for m := range s.out {
			watch.Reset(stuck)
			err := send(m)
			watch.Stop()
			if err != nil {
				cancel(err)
				// Drain what is left, so nothing waits on it.
				for m = range s.out {
					_ = m
				}
				return
			}
		}
	}()
	err := s.run()
	s.flush(true)
	close(s.out)
	if errors.Is(context.Cause(ctx), ErrStuck) {
		return ErrStuck
	}
	<-sent
	if cause := context.Cause(ctx); cause != nil && !errors.Is(cause, context.Canceled) {
		return cause
	}
	return err
}

// stream is one logs call.
type stream struct {
	r       *Reader
	call    *agentv1.LogsCall
	ctx     context.Context
	cancel  context.CancelCauseFunc
	start   time.Time
	project string
	service string
	reader  string
	// from is when lines are wanted from, by this server's clock.
	from   time.Time
	filter *filter

	out chan *agentv1.SendLogsRequest
	// rate is this stream's share of what the server carries live, and shared
	// says it is less than a stream can have, as others are reading too.
	rate   *bucket
	shared atomic.Bool
	// shed is this stream's share of what the server reads and doesn't send.
	shed *pacer

	mu       sync.Mutex
	open     map[string]*openChunk
	order    []string // sources with open chunks, in the order they opened
	openSize int
	gaps     map[string]*gap
	updates  []*dockwellv1.LogSource
	scans    []*agentv1.LogScan
	caught   bool // to say so with the next message
	// startSaid says the first message has gone, which says when the stream began.
	startSaid bool

	flushMu sync.Mutex // sends go out in the order they were built

	wg sync.WaitGroup
	// The rest belong to the goroutine that scans.
	states   map[string]*sourceState
	said     map[string]string // what was last said of each source
	goneSaid map[string]bool   // sources said to be gone
	ringEach atomic.Int64      // what a search holds of each container, in bytes
	reading  atomic.Int32      // how many sources are being read now
	upSaid   atomic.Bool       // caught up has been said
}

func (s *stream) searchLines() int {
	if s.r.SearchLines > 0 {
		return s.r.SearchLines
	}
	return searchTail
}

// key says which service a stream reads, for dividing what a server carries.
func (s *stream) key() string { return s.project + "/" + s.service }

type openChunk struct {
	chunk *dockwellv1.LogChunk
}

// gap counts what a stream dropped of one source since it last said so.
type gap struct {
	reason      dockwellv1.LogGapReason
	lines       uint64
	bytes       uint64
	first, last [40]byte
	firstN      int
	lastN       int
	said        time.Time
	// told is a marker to pass on as it is, as opposed to counts of what was
	// dropped here: lines that are gone, or matches a read let go.
	told *dockwellv1.LogGap
}

// sourceState is how reading one source stands.
type sourceState struct {
	// src is the source as last listed, which only the goroutine that scans
	// reads. A reader gets its own, as the next scan replaces this one.
	src *source
	// runRead is when the run the last read covered started, which also only
	// that goroutine reads: a run since then has lines no read has seen.
	runRead int64
	// live is set once what the source had when the call came has been read.
	live     atomic.Bool
	reading  atomic.Bool
	lastRead atomic.Int64 // unix nanoseconds
	started  atomic.Bool  // a line has been read
	// waiting says its reader is held up by the network, not by Docker, so its
	// log going quiet says nothing of having read all it has.
	waiting atomic.Bool
	// problem says why its log can't be read, and it won't be tried again.
	problem atomic.Pointer[string]
	// unread says its last read never got to the log, so it counts for nothing.
	unread atomic.Bool
	// cursor is the last line kept, for following it again.
	cursorMu sync.Mutex
	cursor   time.Time
	count    uint32
	after    *agentv1.LogCursor // from the call: what was sent before
	// before and beforeCount, from the call, are what a page already holds: the
	// lines after that time, and the newest beforeCount lines at it.
	before      time.Time
	beforeCount uint32
	// level is what the last line said, for the lines that carry it on.
	level [3]dockwellv1.LogLevel
	tail  int
}

func (s *stream) state(id string) *sourceState {
	st := s.states[id]
	if st == nil {
		st = &sourceState{}
		s.states[id] = st
	}
	return st
}

// run reads every source, and with follow keeps looking for more.
func (s *stream) run() error {
	if err := s.scan(true); err != nil {
		return err
	}
	if !s.call.GetFollow() {
		done := make(chan struct{})
		go func() {
			s.wg.Wait()
			close(done)
		}()
		tick := time.NewTicker(flushEvery)
		defer tick.Stop()
		for {
			select {
			case <-done:
				s.setCaught()
				return nil
			case <-tick.C:
				s.flush(false)
			case <-s.ctx.Done():
				<-done
				return nil
			}
		}
	}
	flush := time.NewTicker(flushEvery)
	defer flush.Stop()
	scan := time.NewTicker(scanEvery)
	defer scan.Stop()
	defer s.wg.Wait()
	failures := 0
	for {
		select {
		case <-s.ctx.Done():
			return nil
		case <-flush.C:
			s.checkCaughtUp()
			s.flush(false)
		case <-scan.C:
			err := s.scan(false)
			switch {
			case err == nil:
				failures = 0
			case s.ctx.Err() != nil:
				return nil
			default:
				// Docker may be restarting, which also ends its follows. Say
				// so only if that lasts, and let the control plane try again.
				failures++
				if failures >= 6 {
					s.cancel(err)
					return err
				}
			}
		}
	}
}

// scan finds the sources there are, says what changed of them, and starts
// reading those not being read.
func (s *stream) scan(first bool) error {
	found, err := s.r.sources(s.ctx, s.project, s.service, true, s.call.GetForgottenUpTo())
	if err != nil {
		return err
	}
	var changed []*dockwellv1.LogSource
	present := map[string]bool{}
	wanted := 0
	for _, src := range found {
		id := src.proto.GetId()
		present[id] = true
		if !s.filter.wantsSource(src.proto) {
			continue
		}
		wanted++
		st := s.state(id)
		st.src = src
		// What a search holds of each container depends on how many there are.
		s.ringEach.Store(int64(max(min(matchBytes, streamMatchBytes/wanted), minMatchBytes)))
		roomless := !s.startReading(st, src, first)
		switch why := st.problem.Load(); {
		case why != nil:
			src.proto.LogError = *why
		case roomless:
			src.proto.LogError = "This stream reads at most " + strconv.Itoa(MaxSourcesPerStream) + " containers at once, so this one's lines don't show. Choose one replica or deploy to read it"
		}
		if text := describe(src.proto); text != s.said[id] {
			s.said[id] = text
			changed = append(changed, src.proto)
		}
	}
	// A source a stream followed before that has gone without being kept:
	// what it wrote since is gone, and the reader should know.
	for id, st := range s.states {
		if present[id] {
			continue
		}
		if st.after != nil && !s.goneSaid[id] {
			s.goneSaid[id] = true
			s.gone(id, st.after.GetTime(), nil)
		}
		// And it isn't running, whatever the reader last heard of it.
		if st.src != nil {
			left := proto.Clone(st.src.proto).(*dockwellv1.LogSource)
			left.State = dockwellv1.LogSourceState_LOG_SOURCE_STATE_GONE
			if text := describe(left); text != s.said[id] {
				s.said[id] = text
				changed = append(changed, left)
			}
		}
		// So a follow that outlives many deploys doesn't remember them all.
		if st.src != nil && !st.reading.Load() {
			delete(s.states, id)
			delete(s.said, id)
			delete(s.goneSaid, id)
		}
	}
	if len(changed) > 0 {
		s.mu.Lock()
		s.updates = append(s.updates, changed...)
		s.mu.Unlock()
	}
	if first {
		// Say what there is at once, even before a line, so a reader can show
		// where the lines will come from.
		s.flush(false)
	}
	return nil
}

func describe(p *dockwellv1.LogSource) string {
	return fmt.Sprint(p.GetState(), p.GetExitCode(), p.GetFinishTime().AsTime().UnixNano(), p.GetLogError())
}

// gone notes that a source's lines from one time to another are no longer
// anywhere.
func (s *stream) gone(id string, from, to *timestamppb.Timestamp) {
	s.mu.Lock()
	defer s.mu.Unlock()
	g := s.gaps[id]
	if g == nil {
		g = &gap{}
		s.gaps[id] = g
	}
	g.told = &dockwellv1.LogGap{Reason: dockwellv1.LogGapReason_LOG_GAP_REASON_GONE, StartTime: from, EndTime: to}
}

// letGo notes that a read to the end found more matches than it holds of a
// source, and let these older ones go.
func (s *stream) letGo(id string, lines uint64, from, to *timestamppb.Timestamp) {
	s.mu.Lock()
	defer s.mu.Unlock()
	g := s.gaps[id]
	if g == nil {
		g = &gap{}
		s.gaps[id] = g
	}
	g.told = &dockwellv1.LogGap{Reason: dockwellv1.LogGapReason_LOG_GAP_REASON_MATCH_LIMIT, Lines: lines, StartTime: from, EndTime: to}
}

// wantsKept says whether what the archive holds is read: when asked, or when
// the filters name something in particular.
func (s *stream) wantsKept() bool {
	c := s.call
	return c.GetIncludeKept() || c.GetRevision() != 0 || c.GetChangeId() != "" || len(c.GetSourceIds()) > 0 || len(c.GetAfter()) > 0
}

// startReading starts a goroutine for a source, unless one is at it or it has
// nothing more to say. It says false when the source has to wait for room, as
// the stream reads as many as it can at once, and the next scan tries again.
func (s *stream) startReading(st *sourceState, src *source, first bool) bool {
	if st.problem.Load() != nil || st.reading.Load() {
		return true
	}
	state := src.proto.GetState()
	// One that hasn't started has written nothing, so looking at it reads nothing
	// of it. A deploy makes its container a while before starting it, as the old
	// one stops first, and it may then fail and be gone by the next look.
	if state == dockwellv1.LogSourceState_LOG_SOURCE_STATE_CREATED {
		return true
	}
	kept := src.kept != nil
	run := src.proto.GetStartTime().AsTime().UnixNano()
	again := st.lastRead.Load() != 0
	if again && !st.unread.Load() && (!s.call.GetFollow() || (state != dockwellv1.LogSourceState_LOG_SOURCE_STATE_RUNNING && run == st.runRead)) {
		return true // it was read to its end, and nothing more will come
	}
	// A container that went since the call came was what a follower was there to
	// see, as when a deploy fails and is rolled back in between two looks.
	// Otherwise what was kept isn't read unless asked for.
	if s.outsideTime(st, src.proto) || (kept && !s.wantsKept() && src.kept.wentBefore(s.start)) {
		return true
	}
	if s.reading.Load() >= MaxSourcesPerStream {
		return false
	}
	if !again {
		st.tail = int(s.call.GetTail())
		if !first {
			st.tail = 0 // a container that started since is read from its start
		}
	}
	s.reading.Add(1)
	st.reading.Store(true)
	st.unread.Store(false)
	st.runRead = run
	st.lastRead.Store(time.Now().UnixNano())
	s.wg.Go(func() {
		defer s.reading.Add(-1)
		defer st.reading.Store(false)
		if kept {
			s.readKept(st, src)
		} else {
			s.readDocker(st, src)
		}
	})
	return true
}

// since is when a source's lines are wanted from: what the call asks, or, for a
// stream that carries on and has no place in this source, when it left off.
func (s *stream) since(st *sourceState) time.Time {
	since := s.from
	if st.after == nil {
		if t := timeOf(s.call.GetResumeFrom()); t.After(since) {
			since = t
		}
	}
	return since
}

// outsideTime reports a container whose life is outside the call's time range.
func (s *stream) outsideTime(st *sourceState, p *dockwellv1.LogSource) bool {
	if t := s.since(st); !t.IsZero() && p.GetFinishTime() != nil && p.GetFinishTime().AsTime().Before(t) {
		return true
	}
	if t := s.call.GetUntil(); t != nil && p.GetCreateTime() != nil && p.GetCreateTime().AsTime().After(t.AsTime()) {
		return true
	}
	return false
}

// readDocker reads one container's log from Docker, to its end. A follow ends
// when the container stops.
func (s *stream) readDocker(st *sourceState, src *source) {
	st.cursorMu.Lock()
	cursor, skip := st.cursor, st.count
	st.cursorMu.Unlock()
	follow := s.call.GetFollow()
	o := docker.LogOptions{Follow: follow, Until: timeOf(s.call.GetUntil()), Since: s.since(st)}
	resuming := false
	switch {
	case !cursor.IsZero():
		// The same container, followed again after it restarted.
		o.Since = cursor
	case st.after != nil:
		cursor, skip = st.after.GetTime().AsTime(), st.after.GetCount()
		o.Since = cursor
		resuming = true
	}
	if !st.before.IsZero() {
		o.Until = st.before
	}
	// A read of no lines has nothing to do, whatever it would have filtered.
	if !follow && st.tail < 0 && cursor.IsZero() {
		return
	}
	// What has to look at every line to know which are the newest it wants:
	// a filter, and a tail that ends before the log does, as Docker's tail
	// counts back from the end of the log, whatever until says. It holds the
	// newest it finds, within what ringEach says, and sends them at the end.
	searching := !follow && s.filter.searching()
	ringed := searching || (!follow && st.tail > 0 && !o.Until.IsZero())
	switch {
	case !cursor.IsZero():
	case ringed:
		// One more than it reads, to know whether there are more.
		o.Tail = s.searchLines() + 1
	default:
		o.Tail = st.tail
	}
	ctx := s.ctx
	giveScan := func() {}
	if ringed {
		if !s.r.takeScan(s.ctx) {
			return
		}
		giveScan = sync.OnceFunc(s.r.giveScan)
		defer giveScan()
		var stop context.CancelFunc
		ctx, stop = context.WithTimeout(ctx, searchLimit)
		defer stop()
	}
	rd, err := s.r.Client.ContainerLogs(ctx, src.id, src.tty, o)
	if err != nil {
		s.readFailed(st, src, err)
		return
	}
	defer rd.Close()
	if follow && o.Tail < 0 {
		st.live.Store(true)
	}
	var ring *lineRing
	if ringed {
		want := max(st.tail, 0)
		if want > 0 {
			want += int(st.beforeCount)
		}
		ring = newLineRing(want, int(s.ringEach.Load()))
	}
	scan := &agentv1.LogScan{SourceId: src.proto.GetId()}
	for {
		e, err := rd.Next()
		if err != nil {
			if !errors.Is(err, io.EOF) && ctx.Err() == nil {
				s.r.logger().Warn("a container's log stopped partway", slog.String("container", src.proto.GetContainerName()), slog.String("error", err.Error()))
			}
			if ringed && ctx.Err() != nil && s.ctx.Err() == nil {
				scan.CutShort, scan.TimedOut = true, true
			}
			break
		}
		st.lastRead.Store(time.Now().UnixNano())
		st.started.Store(true)
		// A live stream with no place to find, the path a flood takes, reads a
		// line's time only for a line it keeps.
		var t time.Time
		timed := false
		if !cursor.IsZero() || !st.before.IsZero() || !st.live.Load() || ringed {
			t, timed = docker.ParseLogTime(e.Time)
		}
		if timed && !cursor.IsZero() {
			switch {
			case t.Before(cursor):
				continue
			case t.Equal(cursor) && skip > 0:
				skip--
				continue
			}
			if resuming {
				resuming = false
				s.checkRotated(src, cursor, t)
			}
			cursor = time.Time{}
		}
		if timed && !st.before.IsZero() && t.After(st.before) {
			continue
		}
		if timed && follow && !st.live.Load() && t.After(s.start) {
			st.live.Store(true)
		}
		if ringed {
			scan.Lines++
			if timed && scan.GetOldestTime() == nil {
				scan.OldestTime = timestamppb.New(t)
			}
			if level, ok := s.filter.matches(st, e); ok {
				ring.add(newLine(e, t, level))
			}
			continue
		}
		s.offer(st, src, e, t)
	}
	if ringed {
		// What is left only waits to be sent, which may take as long as its
		// reader takes, so another search can have this one's turn.
		giveScan()
		if most := uint64(s.searchLines()); scan.GetLines() > most { //nolint:gosec // a small limit
			scan.Lines, scan.CutShort = most, true
		}
		ring.dropHeld(st.before, st.beforeCount)
		if ring.overflow > 0 {
			s.letGo(src.proto.GetId(), ring.overflow, scan.GetOldestTime(), ring.oldestTime())
		}
		ring.each(func(l *dockwellv1.LogLine) { s.addLine(st, src, l, false) })
		scan.Older = ring.dropped
		s.mu.Lock()
		s.scans = append(s.scans, scan)
		s.mu.Unlock()
	}
}

// checkRotated says lines are gone when the first line after a cursor is much
// later than it, and is the oldest line Docker still has.
func (s *stream) checkRotated(src *source, cursor, first time.Time) {
	if first.Sub(cursor) < time.Second {
		return
	}
	ctx, cancel := context.WithTimeout(s.ctx, 5*time.Second)
	defer cancel()
	rd, err := s.r.Client.ContainerLogs(ctx, src.id, src.tty, docker.LogOptions{})
	if err != nil {
		return
	}
	defer rd.Close()
	e, err := rd.Next()
	if err != nil {
		return
	}
	if oldest, ok := docker.ParseLogTime(e.Time); ok && oldest.After(cursor) {
		s.gone(src.proto.GetId(), timestamppb.New(cursor), timestamppb.New(oldest))
	}
}

// readFailed handles a log that can't be read: of a container gone since it was
// listed, which the next scan sees, or of one that keeps no log to read, which
// is said once.
func (s *stream) readFailed(st *sourceState, src *source, err error) {
	if s.ctx.Err() != nil {
		return
	}
	// It has gone, or Docker is restarting: the next look tries again, in what the
	// server kept of it if it has gone.
	if docker.IsNotFound(err) || docker.IsUnavailable(err) {
		st.unread.Store(true)
		return
	}
	s.r.logger().Warn("can't read a container's log", slog.String("container", src.proto.GetContainerName()), slog.String("error", err.Error()))
	// By its ID, as Docker may have it under another name for now, as a rollout stops it.
	why := fmt.Sprintf("Docker can't read this container's log: %v. Check its logging driver with docker inspect %s", err, src.proto.GetId())
	st.problem.Store(&why)
}

// keptFailed handles a kept log that can't be read. The container has gone, so
// its driver has nothing to do with it, and what a reader hears never names a
// path on the server.
func (s *stream) keptFailed(st *sourceState, src *source, err error) {
	if s.ctx.Err() != nil {
		return
	}
	s.r.logger().Warn("can't read a kept log", slog.String("container", src.proto.GetContainerName()), slog.String("error", err.Error()))
	why := "The server can't read what it kept of this container: " + keptProblem(err) + ". Its other logs aren't affected. Check the agent's log on this server"
	if errors.Is(err, fs.ErrNotExist) {
		why = "The server removed what it kept of this container while it was being read, as it keeps a log for at most 30 days, and within a budget. Read a newer deploy's logs instead"
	}
	st.problem.Store(&why)
}

// keptProblem says what is wrong with a kept log, without the path it is at.
func keptProblem(err error) string {
	var path *fs.PathError
	if errors.As(err, &path) {
		return path.Err.Error()
	}
	return err.Error()
}

// offer takes a line read from Docker. A live stream that can't send it counts
// it, in a way that allocates nothing, as a flood is all such lines. What reads
// the past waits instead: those lines are still there to read. t is zero when
// the line's time hasn't been read yet.
func (s *stream) offer(st *sourceState, src *source, e *docker.LogEntry, t time.Time) {
	follow := s.call.GetFollow()
	live := follow && st.live.Load()
	level, filtered := dockwellv1.LogLevel_LOG_LEVEL_UNSPECIFIED, s.filter.searching()
	if filtered {
		var ok bool
		if level, ok = s.filter.matches(st, e); !ok {
			// It costs as much to read as a line that is dropped.
			if follow {
				s.shed.wait(s.ctx, e.Size)
			}
			return
		}
	}
	if live {
		// A line costs what it carries, so one longer than a burst still comes. What
		// is cut from it was read and goes nowhere, as a dropped line's was.
		if reason := s.admit(len(e.Text) + lineOverhead); reason != dockwellv1.LogGapReason_LOG_GAP_REASON_UNSPECIFIED {
			s.drop(src.proto.GetId(), e, reason, len(e.Text)+e.Cut)
			s.shed.wait(s.ctx, e.Size)
			return
		}
		if e.Cut > 0 {
			s.shed.wait(s.ctx, e.Cut)
		}
	}
	if t.IsZero() {
		var ok bool
		if t, ok = docker.ParseLogTime(e.Time); !ok {
			t = time.Now()
		}
	}
	if !filtered {
		level = lineLevel(st, e)
	}
	st.cursorMu.Lock()
	if t.Equal(st.cursor) {
		st.count++
	} else {
		st.cursor, st.count = t, 1
	}
	st.cursorMu.Unlock()
	s.addLine(st, src, newLine(e, t, level), live)
}

// lineOverhead is about what a line costs beyond its text, on the wire.
const lineOverhead = 32

// admit says why a live line can't go now, or nothing when it can.
func (s *stream) admit(size int) dockwellv1.LogGapReason {
	if len(s.out) == cap(s.out) {
		return dockwellv1.LogGapReason_LOG_GAP_REASON_READER_BEHIND
	}
	if !s.rate.take(size) {
		if s.shared.Load() {
			return dockwellv1.LogGapReason_LOG_GAP_REASON_SHARED
		}
		return dockwellv1.LogGapReason_LOG_GAP_REASON_TOO_FAST
	}
	return dockwellv1.LogGapReason_LOG_GAP_REASON_UNSPECIFIED
}

// drop counts a line that won't be sent.
func (s *stream) drop(id string, e *docker.LogEntry, reason dockwellv1.LogGapReason, size int) {
	s.mu.Lock()
	g := s.gaps[id]
	if g == nil {
		g = &gap{}
		s.gaps[id] = g
	}
	if g.reason == dockwellv1.LogGapReason_LOG_GAP_REASON_UNSPECIFIED || reason == dockwellv1.LogGapReason_LOG_GAP_REASON_READER_BEHIND {
		g.reason = reason
	}
	g.lines++
	g.bytes += uint64(size) //nolint:gosec // a size isn't negative
	if g.firstN == 0 {
		g.firstN = copy(g.first[:], e.Time)
	}
	g.lastN = copy(g.last[:], e.Time)
	s.mu.Unlock()
}

// addLine adds a line to its source's message. A full message goes out: a
// follow's if there is room, as admit stops taking lines while there is none,
// and otherwise waiting for the network. While it waits, the source isn't
// counted as having gone quiet: only Docker's silence says it has read all.
func (s *stream) addLine(st *sourceState, src *source, line *dockwellv1.LogLine, live bool) {
	id := src.proto.GetId()
	s.mu.Lock()
	c := s.open[id]
	if c == nil {
		c = &openChunk{chunk: &dockwellv1.LogChunk{SourceId: id}}
		s.open[id] = c
		s.order = append(s.order, id)
	}
	c.chunk.Lines = append(c.chunk.Lines, line)
	s.openSize += len(line.GetText()) + lineOverhead
	full := s.openSize >= chunkBytes
	s.mu.Unlock()
	if !full {
		return
	}
	if live {
		s.flush(false)
		return
	}
	st.waiting.Store(true)
	s.flush(true)
	st.lastRead.Store(time.Now().UnixNano())
	st.waiting.Store(false)
}

// flush sends what has gathered. Waiting, it goes out whatever the network
// says; otherwise it goes only if there is room, and stays for the next try.
// One flush runs at a time, so messages go out in the order they were built.
func (s *stream) flush(wait bool) {
	if wait {
		s.flushMu.Lock()
	} else if !s.flushMu.TryLock() {
		return
	}
	defer s.flushMu.Unlock()
	if !wait && len(s.out) == cap(s.out) {
		return
	}
	m := s.gather()
	if m == nil {
		return
	}
	select {
	case s.out <- m:
	case <-s.ctx.Done():
	}
}

// gather builds the next message from what has gathered, and the gaps that are
// due. It returns nil when there is nothing to say.
func (s *stream) gather() *agentv1.SendLogsRequest {
	s.mu.Lock()
	defer s.mu.Unlock()
	now := time.Now()
	m := &agentv1.SendLogsRequest{Stream: s.call.GetStream(), Sources: s.updates, Scans: s.scans, CaughtUp: s.caught}
	s.updates, s.scans, s.caught = nil, nil, false
	chunks := make(map[string]int, len(s.order))
	for _, id := range s.order {
		chunks[id] = len(m.Chunks)
		m.Chunks = append(m.Chunks, s.open[id].chunk)
		delete(s.open, id)
	}
	s.order = s.order[:0]
	s.openSize = 0
	// Each source's gap comes before its lines, or alone, once it is due.
	for id, g := range s.gaps {
		// With its reader gone, nothing it says goes anywhere.
		if s.ctx.Err() != nil {
			break
		}
		if g.told == nil && (g.lines == 0 || now.Sub(g.said) < gapEvery) {
			continue
		}
		i, ok := chunks[id]
		if !ok {
			i = len(m.Chunks)
			chunks[id] = i
			m.Chunks = append(m.Chunks, &dockwellv1.LogChunk{SourceId: id})
		}
		m.Chunks[i].Gap = g.proto()
		*g = gap{said: now}
	}
	if len(m.GetChunks()) == 0 && len(m.GetSources()) == 0 && len(m.GetScans()) == 0 && !m.GetCaughtUp() {
		return nil
	}
	if !s.startSaid {
		s.startSaid = true
		m.Started = timestamppb.New(s.start)
	}
	return m
}

func (g *gap) proto() *dockwellv1.LogGap {
	if g.told != nil {
		return g.told
	}
	out := &dockwellv1.LogGap{Reason: g.reason, Lines: g.lines, Bytes: g.bytes}
	if t, ok := docker.ParseLogTime(g.first[:g.firstN]); ok {
		out.StartTime = timestamppb.New(t)
	}
	if t, ok := docker.ParseLogTime(g.last[:g.lastN]); ok {
		out.EndTime = timestamppb.New(t)
	}
	return out
}

// setCaught notes everything asked for has been read.
func (s *stream) setCaught() {
	if s.upSaid.CompareAndSwap(false, true) {
		s.mu.Lock()
		s.caught = true
		s.mu.Unlock()
	}
}

// checkCaughtUp notes, once, that every source that was there when the call
// came has been read up to now, so what each sends next is live. Docker gives
// no sign of reaching the end of what it has, so a pause says so.
func (s *stream) checkCaughtUp() {
	if s.upSaid.Load() {
		return
	}
	now := time.Now().UnixNano()
	for _, st := range s.states {
		if st.src == nil || st.live.Load() {
			continue
		}
		idle := idleFirst
		if st.started.Load() {
			idle = idleLive
		}
		last := st.lastRead.Load()
		if st.reading.Load() && (st.waiting.Load() || now-last < idle.Nanoseconds()) {
			return
		}
		st.live.Store(true)
	}
	s.setCaught()
}

// readKept reads what the archive has of a removed container, to its end.
func (s *stream) readKept(st *sourceState, src *source) {
	k := src.kept
	since, until := s.since(st), timeOf(s.call.GetUntil())
	// From where a read of the container left off, which may have been of Docker,
	// or else from where the call says.
	st.cursorMu.Lock()
	cursor, skip := st.cursor, st.count
	st.cursorMu.Unlock()
	if cursor.IsZero() && st.after != nil {
		cursor, skip = st.after.GetTime().AsTime(), st.after.GetCount()
	}
	// A read of no lines has nothing to do, whatever it would have filtered.
	if st.tail < 0 && cursor.IsZero() {
		st.live.Store(true)
		return
	}
	within := func(t time.Time) bool {
		switch {
		case !since.IsZero() && t.Before(since):
			return false
		case !until.IsZero() && t.After(until):
			return false
		case !st.before.IsZero() && t.After(st.before):
			return false
		case !cursor.IsZero() && t.Before(cursor):
			return false
		}
		return true
	}
	e := &docker.LogEntry{}
	entry := func(l *keptLine) *docker.LogEntry {
		e.Stream, e.Text, e.Cut = l.stream, l.text, l.cut
		return e
	}
	searching := s.filter.searching()
	scan := &agentv1.LogScan{SourceId: src.proto.GetId()}
	want := int64(max(st.tail, 0))
	// How many lines match, read once, says where the tail starts and where
	// what a page holds begins, with nothing held in memory, as this log
	// doesn't change.
	start, end := int64(0), int64(-1)
	if want > 0 || !st.before.IsZero() {
		if !s.r.takeScan(s.ctx) {
			return
		}
		total, held := int64(0), int64(0)
		err := k.read(s.ctx, func(l *keptLine) bool {
			if within(l.time) {
				if _, ok := s.filter.matches(st, entry(l)); ok {
					total++
					if l.time.Equal(st.before) {
						held++
					}
				}
			}
			return true
		})
		s.r.giveScan()
		if err != nil {
			s.keptFailed(st, src, err)
			return
		}
		st.level = [3]dockwellv1.LogLevel{}
		end = total - min(held, int64(st.beforeCount))
		if want > 0 {
			start = max(end-want, 0)
		}
		scan.Older = start > 0
	}
	if !k.HoleTo.IsZero() && within(k.HoleTo) {
		s.gone(src.proto.GetId(), timestamppb.New(k.HoleFrom), timestamppb.New(k.HoleTo))
	}
	n := int64(0)
	err := k.read(s.ctx, func(l *keptLine) bool {
		st.lastRead.Store(time.Now().UnixNano())
		st.started.Store(true)
		if !within(l.time) {
			return true
		}
		if !cursor.IsZero() && l.time.Equal(cursor) && skip > 0 {
			skip--
			return true
		}
		if searching {
			scan.Lines++
			if scan.GetOldestTime() == nil {
				scan.OldestTime = timestamppb.New(l.time)
			}
		}
		level, ok := s.filter.matches(st, entry(l))
		if !ok {
			return true
		}
		n++
		if n > start && (end < 0 || n <= end) {
			s.addLine(st, src, newLine(e, l.time, level), false)
		}
		return true
	})
	if err != nil {
		s.keptFailed(st, src, err)
		return
	}
	if searching || scan.GetOlder() {
		s.mu.Lock()
		s.scans = append(s.scans, scan)
		s.mu.Unlock()
	}
	st.live.Store(true)
}

func timeOf(t *timestamppb.Timestamp) time.Time {
	if t == nil {
		return time.Time{}
	}
	return t.AsTime()
}

// filter says which sources and lines a call wants, and makes lines of them.
type filter struct {
	sources  map[string]bool
	revision int64
	change   string
	levels   map[dockwellv1.LogLevel]bool
	query    *foldSearch
}

func newFilter(c *agentv1.LogsCall) *filter {
	f := &filter{revision: c.GetRevision(), change: c.GetChangeId(), query: newFoldSearch(c.GetQuery())}
	if len(c.GetSourceIds()) > 0 {
		f.sources = map[string]bool{}
		for _, id := range c.GetSourceIds() {
			f.sources[id] = true
		}
	}
	if len(c.GetLevels()) > 0 {
		f.levels = map[dockwellv1.LogLevel]bool{}
		for _, l := range c.GetLevels() {
			f.levels[l] = true
		}
		// A line with no level counts as info.
		if f.levels[dockwellv1.LogLevel_LOG_LEVEL_INFO] {
			f.levels[dockwellv1.LogLevel_LOG_LEVEL_UNSPECIFIED] = true
		}
	}
	return f
}

func (f *filter) wantsSource(p *dockwellv1.LogSource) bool {
	switch {
	case f.sources != nil && !f.sources[p.GetId()]:
		return false
	case f.revision != 0 && p.GetRevision() != f.revision:
		return false
	case f.change != "" && p.GetChangeId() != f.change:
		return false
	}
	return true
}

// searching reports a filter that has to read lines to tell, so a tail can't
// be read from Docker directly.
func (f *filter) searching() bool { return f.levels != nil || !f.query.empty() }

// matches reports a line the filter takes, with its level, and notes the level
// for the lines that carry it on.
func (f *filter) matches(st *sourceState, e *docker.LogEntry) (dockwellv1.LogLevel, bool) {
	level := lineLevel(st, e)
	if f.levels != nil && !f.levels[level] {
		return level, false
	}
	return level, f.query.in(e.Text)
}

// newLine makes a line to send. Its text must be valid UTF-8 to go in a message,
// and what a container prints needn't be.
func newLine(e *docker.LogEntry, t time.Time, level dockwellv1.LogLevel) *dockwellv1.LogLine {
	stream := dockwellv1.LogStream_LOG_STREAM_STDOUT
	if e.Stream == docker.Stderr {
		stream = dockwellv1.LogStream_LOG_STREAM_STDERR
	}
	return &dockwellv1.LogLine{
		Time: timestamppb.New(t), Stream: stream, Level: level,
		Text: strings.ToValidUTF8(string(e.Text), "\uFFFD"), CutBytes: uint32(e.Cut), //nolint:gosec // a line's size fits
	}
}

// lineLevel reads a line's level: its own, or the one it carries on from. A
// blank line, as between a panic's message and its stack, leaves that as it was.
func lineLevel(st *sourceState, e *docker.LogEntry) dockwellv1.LogLevel {
	if len(bytes.TrimSpace(e.Text)) == 0 {
		return st.level[e.Stream%3]
	}
	level := ReadLevel(e.Text)
	if level == dockwellv1.LogLevel_LOG_LEVEL_UNSPECIFIED && isContinuation(e.Text) {
		return st.level[e.Stream%3]
	}
	st.level[e.Stream%3] = level
	return level
}

// foldSearch finds a query in text, in any case, in time linear in the text
// whatever both hold, without allocating, as it runs on every line of a search.
// It handles ASCII case; other letters match as written.
type foldSearch struct {
	query []byte
	// next says where to carry on in the query after a mismatch, as Knuth,
	// Morris and Pratt do.
	next []int32
}

// asciiLower maps a byte to its lowercase, for ASCII letters.
var asciiLower = func() (t [256]byte) {
	for i := range t {
		t[i] = byte(i)
		if 'A' <= i && i <= 'Z' {
			t[i] += 'a' - 'A'
		}
	}
	return t
}()

func newFoldSearch(query string) *foldSearch {
	f := &foldSearch{query: make([]byte, len(query)), next: make([]int32, len(query))}
	for i := range len(query) {
		f.query[i] = asciiLower[query[i]]
	}
	k := int32(0)
	for i := 1; i < len(f.query); i++ {
		for k > 0 && f.query[i] != f.query[k] {
			k = f.next[k-1]
		}
		if f.query[i] == f.query[k] {
			k++
		}
		f.next[i] = k
	}
	return f
}

func (f *foldSearch) empty() bool { return len(f.query) == 0 }

// in reports text holding the query.
func (f *foldSearch) in(text []byte) bool {
	if len(f.query) == 0 {
		return true
	}
	if len(text) < len(f.query) {
		return false
	}
	k := int32(0)
	for _, c := range text {
		c = asciiLower[c]
		for k > 0 && c != f.query[k] {
			k = f.next[k-1]
		}
		if c == f.query[k] {
			k++
			if int(k) == len(f.query) {
				return true
			}
		}
	}
	return false
}

// lineRing holds a search's newest matches, within a limit of how many and how
// much memory they take. Every line costs more than its text, so short lines
// are held to the limit too.
type lineRing struct {
	lines    []*dockwellv1.LogLine
	want     int // zero means as many as fit
	limit    int
	size     int
	dropped  bool   // older matches were let go, whatever for
	overflow uint64 // how many for lack of room, not because they weren't asked for
}

func newLineRing(want, limit int) *lineRing { return &lineRing{want: want, limit: limit} }

func lineCost(l *dockwellv1.LogLine) int { return len(l.GetText()) + heldLineBytes }

func (r *lineRing) add(l *dockwellv1.LogLine) {
	r.lines = append(r.lines, l)
	r.size += lineCost(l)
	for (r.want > 0 && len(r.lines) > r.want) || (r.size > r.limit && len(r.lines) > 1) {
		if r.size > r.limit {
			r.overflow++
		}
		r.size -= lineCost(r.lines[0])
		r.lines[0] = nil
		r.lines = r.lines[1:]
		r.dropped = true
	}
}

// dropHeld leaves out the newest lines at a time, up to count: those a page
// before this one already holds.
func (r *lineRing) dropHeld(at time.Time, count uint32) {
	for count > 0 && len(r.lines) > 0 {
		last := r.lines[len(r.lines)-1]
		if !last.GetTime().AsTime().Equal(at) {
			return
		}
		r.size -= lineCost(last)
		r.lines[len(r.lines)-1] = nil
		r.lines = r.lines[:len(r.lines)-1]
		count--
	}
}

// oldestTime is the time of the oldest line held, if any.
func (r *lineRing) oldestTime() *timestamppb.Timestamp {
	if len(r.lines) == 0 {
		return nil
	}
	return r.lines[0].GetTime()
}

func (r *lineRing) each(fn func(*dockwellv1.LogLine)) {
	for _, l := range r.lines {
		fn(l)
	}
}
