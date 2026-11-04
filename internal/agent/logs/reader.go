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
	"log/slog"
	"slices"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"google.golang.org/protobuf/types/known/timestamppb"

	agentv1 "github.com/dockwell/dockwell/gen/dockwell/agent/v1"
	dockwellv1 "github.com/dockwell/dockwell/gen/dockwell/v1"
	"github.com/dockwell/dockwell/internal/docker"
	"github.com/dockwell/dockwell/internal/resource/container"
)

// Limits of reading logs on one server.
const (
	// MaxStreams is how many log readers one agent serves at once. Each one
	// keeps a connection to Docker, which decodes its log for it.
	MaxStreams = 16
	// MaxSourcesPerStream bounds how many containers one reader follows.
	MaxSourcesPerStream = 32
	// LiveBytesPerSecond is what one live stream carries, sustained: at about
	// 150 bytes a line, over 13,000 lines a second, far past what anyone
	// reads, and small enough that a noisy container can't take up a small
	// server's network or CPU.
	LiveBytesPerSecond = 2 << 20
	// TotalBytesPerSecond is what all live streams on a server carry between
	// them.
	TotalBytesPerSecond = 4 << 20
	// sheddingBytesPerSecond is how fast a stream reads Docker while it drops
	// what it reads. Counting lines costs little each, and a writer far past this
	// would otherwise take a core just to be counted. A writer past it is behind
	// what Docker rotates away within a second or so, and what goes that way isn't
	// counted, so a marker's count can be short at such a rate.
	sheddingBytesPerSecond = 64 << 20
	// searchLimit bounds how long one search reads.
	searchLimit = 30 * time.Second
	// searchTail is how many of a container's newest lines a search reads.
	searchTail = 1_000_000
	// matchBytes bounds the matches a search holds of one container while
	// it reads on, newest kept.
	matchBytes = 2 << 20
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

// ErrBusy means too many readers are at this server's logs.
var ErrBusy = errors.New("too many readers are following logs on this server, at most " + strconv.Itoa(MaxStreams) + ". Close one, and Dockwell tries again")

// Reader reads logs on this server. It is safe for concurrent use.
type Reader struct {
	Client  *docker.Client
	Archive *Archive
	Logger  *slog.Logger
	// LiveRate and TotalRate replace the limits, in bytes a second, in tests.
	LiveRate, TotalRate int

	active atomic.Int32
	total  bucketOnce
}

type bucketOnce struct {
	once sync.Once
	b    *bucket
}

func (r *Reader) totalBucket() *bucket {
	r.total.once.Do(func() {
		rate := r.TotalRate
		if rate <= 0 {
			rate = TotalBytesPerSecond
		}
		r.total.b = newBucket(rate)
	})
	return r.total.b
}

func (r *Reader) logger() *slog.Logger {
	if r.Logger != nil {
		return r.Logger
	}
	return slog.Default()
}

// Sources lists the containers a service has logs from, newest first.
func (r *Reader) Sources(ctx context.Context, ref *dockwellv1.ResourceRef) ([]*dockwellv1.LogSource, error) {
	found, err := r.sources(ctx, docker.Project(ref.GetScope()), ref.GetName(), true)
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

// sources lists a service's containers: those Docker has, and, with kept, those
// the archive has that Docker no longer does.
func (r *Reader) sources(ctx context.Context, project, service string, kept bool) ([]*source, error) {
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
			if !have[k.ContainerID] {
				out = append(out, &source{proto: keptSource(k), kept: k})
			}
		}
	}
	slices.SortStableFunc(out, func(a, b *source) int {
		return b.proto.GetCreateTime().AsTime().Compare(a.proto.GetCreateTime().AsTime())
	})
	return out, nil
}

func dockerSource(c *docker.Container) *dockwellv1.LogSource {
	l := c.Config.Labels
	number, _ := strconv.Atoi(l[docker.LabelNumber])
	revision, _ := strconv.ParseInt(l[container.LabelRevision], 10, 64)
	s := &dockwellv1.LogSource{
		Id:            shortID(c.ID),
		Replica:       int32(max(number, 1)), //nolint:gosec // a replica's number is small
		ContainerName: strings.TrimPrefix(c.Name, "/"),
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
		Replica:       int32(k.Number), //nolint:gosec // a replica's number is small
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
// what it can't send, and says how much, and what reads the past waits.
func (r *Reader) Stream(ctx context.Context, call *agentv1.LogsCall, send func(*agentv1.SendLogsRequest) error) error {
	if r.active.Add(1) > MaxStreams {
		r.active.Add(-1)
		return ErrBusy
	}
	defer r.active.Add(-1)
	liveRate := r.LiveRate
	if liveRate <= 0 {
		liveRate = LiveBytesPerSecond
	}
	ctx, cancel := context.WithCancelCause(ctx)
	defer cancel(nil)
	s := &stream{
		r: r, call: call, ctx: ctx, cancel: cancel, start: time.Now(),
		project: docker.Project(call.GetResource().GetScope()), service: call.GetResource().GetName(),
		out:   make(chan *agentv1.SendLogsRequest, queued),
		rate:  newBucket(liveRate),
		total: r.totalBucket(),
		open:  map[string]*openChunk{}, gaps: map[string]*gap{}, states: map[string]*sourceState{},
		said: map[string]string{}, goneSaid: map[string]bool{},
	}
	s.filter = newFilter(call)
	for _, c := range call.GetAfter() {
		s.state(c.GetSourceId()).after = c
	}
	for _, c := range call.GetBefore() {
		s.state(c.GetSourceId()).before = c.GetTime().AsTime()
	}
	// Sending has a goroutine of its own, so a network that blocks stalls
	// neither reading nor the drop counts.
	sent := make(chan struct{})
	go func() {
		defer close(sent)
		for m := range s.out {
			if err := send(m); err != nil {
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
	filter  *filter

	out         chan *agentv1.SendLogsRequest
	rate, total *bucket

	mu       sync.Mutex
	open     map[string]*openChunk
	order    []string // sources with open chunks, in the order they opened
	openSize int
	gaps     map[string]*gap
	updates  []*dockwellv1.LogSource
	scans    []*agentv1.LogScan
	caught   bool // to say so with the next message

	flushMu sync.Mutex // sends go out in the order they were built

	wg sync.WaitGroup
	// The rest belong to the goroutine that scans.
	states   map[string]*sourceState
	said     map[string]string // what was last said of each source
	goneSaid map[string]bool   // sources said to be gone
	reading  int               // how many sources have been read
	upSaid   atomic.Bool       // caught up has been said
}

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
	// gone says lines are gone, as opposed to dropped here.
	gone *dockwellv1.LogGap
}

// sourceState is how reading one source stands.
type sourceState struct {
	// src is the source as last listed, which only the goroutine that scans
	// reads. A reader gets its own, as the next scan replaces this one.
	src *source
	// live is set once what the source had when the call came has been read.
	live     atomic.Bool
	reading  atomic.Bool
	lastRead atomic.Int64 // unix nanoseconds
	started  atomic.Bool  // a line has been read
	// problem says why its log can't be read, and it won't be tried again.
	problem atomic.Pointer[string]
	// cursor is the last line kept, for following it again.
	cursorMu sync.Mutex
	cursor   time.Time
	count    uint32
	after    *agentv1.LogCursor // from the call: what was sent before
	before   time.Time          // from the call: what a page already holds
	// level is what the last line said, for the lines that carry it on.
	level [3]dockwellv1.LogLevel
	tail  int
	// shed and shedSince pace reading while lines are dropped.
	shed      int
	shedSince time.Time
}

// pace sleeps as needed to read no faster than sheddingBytesPerSecond while
// dropping, a chunk at a time.
func (st *sourceState) pace(ctx context.Context, size int) {
	const chunk = 1 << 20
	st.shed += size
	if st.shed < chunk {
		return
	}
	st.shed = 0
	now := time.Now()
	if !st.shedSince.IsZero() {
		want := time.Second * chunk / sheddingBytesPerSecond
		if wait := want - now.Sub(st.shedSince); wait > 0 && wait < time.Second {
			select {
			case <-time.After(wait):
			case <-ctx.Done():
			}
			now = time.Now()
		}
	}
	st.shedSince = now
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
	found, err := s.r.sources(s.ctx, s.project, s.service, true)
	if err != nil {
		return err
	}
	var changed []*dockwellv1.LogSource
	present := map[string]bool{}
	for _, src := range found {
		id := src.proto.GetId()
		present[id] = true
		if !s.filter.wantsSource(src.proto) {
			continue
		}
		st := s.state(id)
		st.src = src
		if why := st.problem.Load(); why != nil {
			src.proto.LogError = *why
		}
		if text := describe(src.proto); text != s.said[id] {
			s.said[id] = text
			changed = append(changed, src.proto)
		}
		s.startReading(st, src, first)
	}
	// A source a stream followed before that has gone without being kept:
	// what it wrote since is gone, and the reader should know.
	for id, st := range s.states {
		if !present[id] && st.after != nil && !s.goneSaid[id] {
			s.goneSaid[id] = true
			s.gone(id, st.after.GetTime(), nil)
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
	g.gone = &dockwellv1.LogGap{Reason: dockwellv1.LogGapReason_LOG_GAP_REASON_GONE, StartTime: from, EndTime: to}
}

// wantsKept says whether what the archive holds is read: when asked, or when
// the filters name something in particular.
func (s *stream) wantsKept() bool {
	c := s.call
	return c.GetIncludeKept() || c.GetRevision() != 0 || c.GetChangeId() != "" || len(c.GetSourceIds()) > 0 || len(c.GetAfter()) > 0
}

// startReading starts a goroutine for a source, unless one is at it, it has
// nothing more to say, or it is past the limit.
func (s *stream) startReading(st *sourceState, src *source, first bool) {
	if st.problem.Load() != nil || st.reading.Load() {
		return
	}
	kept := src.kept != nil
	again := st.lastRead.Load() != 0
	if again && (kept || !s.call.GetFollow() || src.proto.GetState() != dockwellv1.LogSourceState_LOG_SOURCE_STATE_RUNNING) {
		return // it was read to its end, and nothing more will come
	}
	// A container that came and went since the call came was what a follower
	// was there to see, as when a deploy fails and is rolled back in between
	// two looks. Otherwise what was kept isn't read unless asked for.
	if s.outsideTime(st, src.proto) || (kept && !s.wantsKept() && src.proto.GetCreateTime().AsTime().Before(s.start)) {
		return
	}
	if !again {
		if s.reading >= MaxSourcesPerStream {
			return
		}
		s.reading++
		st.tail = int(s.call.GetTail())
		if !first {
			st.tail = 0 // a container that started since is read from its start
		}
	}
	st.reading.Store(true)
	st.lastRead.Store(time.Now().UnixNano())
	s.wg.Go(func() {
		defer st.reading.Store(false)
		if kept {
			s.readKept(st, src)
		} else {
			s.readDocker(st, src)
		}
	})
}

// since is when a source's lines are wanted from: what the call asks, or, for a
// stream that carries on and has no place in this source, when it left off.
func (s *stream) since(st *sourceState) time.Time {
	since := timeOf(s.call.GetSince())
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
	// What has to look at every line to know which are the newest it wants:
	// a filter, and a tail that ends before the log does, as Docker's tail
	// counts back from the end of the log, whatever until says. It holds the
	// newest it finds, within matchBytes, and sends them at the end.
	searching := !follow && s.filter.searching()
	ringed := searching || (!follow && st.tail > 0 && !o.Until.IsZero())
	switch {
	case !cursor.IsZero():
	case ringed:
		o.Tail = searchTail
	default:
		o.Tail = st.tail
	}
	ctx := s.ctx
	if ringed {
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
		want := int(s.call.GetTail())
		if want <= 0 {
			want = searchTail
		}
		ring = newLineRing(want)
	}
	scan := &agentv1.LogScan{SourceId: src.proto.GetId()}
	for {
		e, err := rd.Next()
		if err != nil {
			if !errors.Is(err, io.EOF) && ctx.Err() == nil {
				s.r.logger().Warn("a container's log stopped partway", slog.String("container", src.proto.GetContainerName()), slog.String("error", err.Error()))
			}
			if ringed && ctx.Err() != nil && s.ctx.Err() == nil {
				scan.CutShort = true
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
		if timed && !st.before.IsZero() && !t.Before(st.before) {
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
		ring.each(func(l *dockwellv1.LogLine) { s.addLine(src, l, false) })
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
	if s.ctx.Err() != nil || docker.IsNotFound(err) || docker.IsUnavailable(err) {
		return
	}
	s.r.logger().Warn("can't read a container's log", slog.String("container", src.proto.GetContainerName()), slog.String("error", err.Error()))
	why := fmt.Sprintf("Docker can't read this container's log: %v. Check its logging driver with docker inspect %s", err, src.proto.GetContainerName())
	st.problem.Store(&why)
}

// offer takes a line read from Docker. A live stream that can't send it counts
// it, in a way that allocates nothing, as a flood is all such lines. What reads
// the past waits instead: those lines are still there to read. t is zero when
// the line's time hasn't been read yet.
func (s *stream) offer(st *sourceState, src *source, e *docker.LogEntry, t time.Time) {
	live := s.call.GetFollow() && st.live.Load()
	level, filtered := dockwellv1.LogLevel_LOG_LEVEL_UNSPECIFIED, s.filter.searching()
	if filtered {
		var ok bool
		if level, ok = s.filter.matches(st, e); !ok {
			return
		}
	}
	if live {
		size := len(e.Text) + e.Cut
		if reason := s.admit(size + lineOverhead); reason != dockwellv1.LogGapReason_LOG_GAP_REASON_UNSPECIFIED {
			s.drop(src.proto.GetId(), e, reason, size)
			st.pace(s.ctx, e.Size)
			return
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
	s.addLine(src, newLine(e, t, level), live)
}

// lineOverhead is about what a line costs beyond its text, on the wire.
const lineOverhead = 32

// admit says why a live line can't go now, or nothing when it can.
func (s *stream) admit(size int) dockwellv1.LogGapReason {
	if len(s.out) == cap(s.out) {
		return dockwellv1.LogGapReason_LOG_GAP_REASON_READER_BEHIND
	}
	if !s.rate.take(size) {
		return dockwellv1.LogGapReason_LOG_GAP_REASON_TOO_FAST
	}
	if !s.total.take(size) {
		s.rate.give(size)
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
// and otherwise waiting for the network.
func (s *stream) addLine(src *source, line *dockwellv1.LogLine, live bool) {
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
	if full {
		s.flush(!live)
	}
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
	for _, id := range s.order {
		m.Chunks = append(m.Chunks, s.open[id].chunk)
		delete(s.open, id)
	}
	s.order = s.order[:0]
	s.openSize = 0
	// Each source's gap comes before its lines, or alone, once it is due.
	for id, g := range s.gaps {
		if g.gone == nil && (g.lines == 0 || now.Sub(g.said) < gapEvery) {
			continue
		}
		i := slices.IndexFunc(m.Chunks, func(c *dockwellv1.LogChunk) bool { return c.GetSourceId() == id })
		if i < 0 {
			i = len(m.Chunks)
			m.Chunks = append(m.Chunks, &dockwellv1.LogChunk{SourceId: id})
		}
		m.Chunks[i].Gap = g.proto()
		*g = gap{said: now}
	}
	if len(m.GetChunks()) == 0 && len(m.GetSources()) == 0 && len(m.GetScans()) == 0 && !m.GetCaughtUp() {
		return nil
	}
	return m
}

func (g *gap) proto() *dockwellv1.LogGap {
	if g.gone != nil {
		return g.gone
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
		if st.reading.Load() && now-last < idle.Nanoseconds() {
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
	var cursor time.Time
	var skip uint32
	if st.after != nil {
		cursor, skip = st.after.GetTime().AsTime(), st.after.GetCount()
	}
	within := func(t time.Time) bool {
		switch {
		case !since.IsZero() && t.Before(since):
			return false
		case !until.IsZero() && t.After(until):
			return false
		case !st.before.IsZero() && !t.Before(st.before):
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
	want := int64(s.call.GetTail())
	// How many lines match, read once, says where the tail starts, with
	// nothing held in memory, as this log doesn't change.
	start := int64(0)
	if want > 0 {
		total := int64(0)
		err := k.read(s.ctx, func(l *keptLine) bool {
			if within(l.time) {
				if _, ok := s.filter.matches(st, entry(l)); ok {
					total++
				}
			}
			return true
		})
		if err != nil {
			s.readFailed(st, src, err)
			return
		}
		st.level = [3]dockwellv1.LogLevel{}
		start = max(total-want, 0)
		scan.Older = start > 0
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
		if n > start {
			s.addLine(src, newLine(e, l.time, level), false)
		}
		return true
	})
	if err != nil {
		s.readFailed(st, src, err)
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
	query    []byte
}

func newFilter(c *agentv1.LogsCall) *filter {
	f := &filter{revision: c.GetRevision(), change: c.GetChangeId(), query: bytes.ToLower([]byte(c.GetQuery()))}
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
func (f *filter) searching() bool { return f.levels != nil || len(f.query) > 0 }

// matches reports a line the filter takes, with its level, and notes the level
// for the lines that carry it on.
func (f *filter) matches(st *sourceState, e *docker.LogEntry) (dockwellv1.LogLevel, bool) {
	level := lineLevel(st, e)
	if f.levels != nil && !f.levels[level] {
		return level, false
	}
	return level, len(f.query) == 0 || containsFold(e.Text, f.query)
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

// lineLevel reads a line's level: its own, or the one it carries on from.
func lineLevel(st *sourceState, e *docker.LogEntry) dockwellv1.LogLevel {
	level := ReadLevel(e.Text)
	if level == dockwellv1.LogLevel_LOG_LEVEL_UNSPECIFIED && isContinuation(e.Text) {
		return st.level[e.Stream%3]
	}
	st.level[e.Stream%3] = level
	return level
}

// containsFold reports text holding query, which is in lowercase, in any case.
// It handles ASCII case without allocating; other letters match as written.
func containsFold(text, query []byte) bool {
	if len(query) == 0 {
		return true
	}
	if len(text) < len(query) {
		return false
	}
	first := query[0]
	upper := first
	if 'a' <= first && first <= 'z' {
		upper = first - ('a' - 'A')
	}
	for i := 0; i+len(query) <= len(text); {
		j := indexEither(text[i:len(text)-len(query)+1], first, upper)
		if j < 0 {
			return false
		}
		i += j
		match := true
		for k := 1; k < len(query); k++ {
			c := text[i+k]
			if 'A' <= c && c <= 'Z' {
				c += 'a' - 'A'
			}
			if c != query[k] {
				match = false
				break
			}
		}
		if match {
			return true
		}
		i++
	}
	return false
}

func indexEither(b []byte, a, c byte) int {
	x := bytes.IndexByte(b, a)
	if a == c {
		return x
	}
	if x >= 0 {
		b = b[:x]
	}
	if y := bytes.IndexByte(b, c); y >= 0 {
		return y
	}
	return x
}

// lineRing holds a search's newest matches, within matchBytes.
type lineRing struct {
	lines []*dockwellv1.LogLine
	want  int
	size  int
	// dropped says older matches were let go.
	dropped bool
}

func newLineRing(want int) *lineRing { return &lineRing{want: want} }

func (r *lineRing) add(l *dockwellv1.LogLine) {
	r.lines = append(r.lines, l)
	r.size += len(l.GetText())
	for len(r.lines) > r.want || (r.size > matchBytes && len(r.lines) > 1) {
		r.size -= len(r.lines[0].GetText())
		r.lines[0] = nil
		r.lines = r.lines[1:]
		r.dropped = true
	}
}

func (r *lineRing) each(fn func(*dockwellv1.LogLine)) {
	for _, l := range r.lines {
		fn(l)
	}
}

// bucket limits a rate, in bytes a second.
type bucket struct {
	mu     sync.Mutex
	rate   float64
	burst  float64
	tokens float64
	last   time.Time
}

func newBucket(rate int) *bucket {
	// A burst of a quarter of a second's worth: enough not to cut a normal
	// burst of lines, little enough to hold the rate.
	burst := float64(rate) / 4
	return &bucket{rate: float64(rate), burst: burst, tokens: burst, last: time.Now()}
}

func (b *bucket) take(n int) bool {
	b.mu.Lock()
	defer b.mu.Unlock()
	now := time.Now()
	b.tokens = min(b.burst, b.tokens+now.Sub(b.last).Seconds()*b.rate)
	b.last = now
	if b.tokens < float64(n) {
		return false
	}
	b.tokens -= float64(n)
	return true
}

func (b *bucket) give(n int) {
	b.mu.Lock()
	defer b.mu.Unlock()
	b.tokens = min(b.burst, b.tokens+float64(n))
}
