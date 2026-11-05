// SPDX-License-Identifier: AGPL-3.0-only

package logs

import (
	"context"
	"errors"
	"fmt"
	"slices"
	"sync"
	"time"

	agentv1 "github.com/dockwell/dockwell/gen/dockwell/agent/v1"
)

// What a server spends on reading logs, whoever reads them.
const (
	// MaxStreams is how many log readers one agent serves at once. Each one
	// keeps a connection to Docker, which decodes its log for it.
	MaxStreams = 16
	// MaxStreamsPerReader is how many of those one reader can hold, so that no
	// one token can shut everyone else out during an incident.
	MaxStreamsPerReader = 8
	// keptForLive is how many places only a follow can take, so that searches
	// and reads to the end, which a script can start in any number, always
	// leave a place for someone reading live.
	keptForLive = 4
	// maxScans is how many containers this server searches at once. A search
	// reads as fast as it can, so more would only take more of its CPU; the rest
	// wait their turn, and their 30 s start when it comes.
	maxScans = 4
	// LiveBytesPerSecond is what one live stream carries, sustained: at about
	// 150 bytes a line, over 13,000 lines a second, far past what anyone
	// reads, and small enough that a noisy container can't take up a small
	// server's network or CPU.
	LiveBytesPerSecond = 2 << 20
	// TotalBytesPerSecond is what all live streams on a server carry between
	// them.
	TotalBytesPerSecond = 4 << 20
	// shedBytesPerSecond is how fast all a server's follows read Docker between
	// them, for lines they don't send: those they drop, and those their filters
	// don't take. Looking at a line costs little, and a writer far past this
	// would otherwise take a core just to be looked at. A writer past it is
	// behind what Docker rotates away within a second or so, and what goes that
	// way isn't counted, so a marker's count can be short at such a rate.
	shedBytesPerSecond = 64 << 20
	// stuckAfter is how long one message can wait for a reader before the stream
	// lets go of its place. The control plane's own limit on a stalled write is
	// shorter, so this is for a stall between the two.
	stuckAfter = time.Minute
)

// ErrBusy means too many readers are at this server's logs.
var ErrBusy = fmt.Errorf("too many readers are following logs on this server, at most %d. Close one, and Dockwell tries again", MaxStreams)

// ErrStuck means a stream ended because its reader stopped taking lines.
var ErrStuck = errors.New("this reader stopped taking lines, so the server let go of its place. It carries on from where it was once it reads again")

var (
	errBusyReader = fmt.Errorf("you have %d streams of logs open on this server, the most one reader can. Close one, and Dockwell tries again", MaxStreamsPerReader)
	errBusySearch = fmt.Errorf("too many searches are reading logs on this server, at most %d, as the other places wait for someone reading live. Try again in a moment", MaxStreams-keptForLive)
)

// What a call may name, which the agent checks whatever the control plane did, as
// each named source costs it memory and work. A next page or a stream that carries on
// names only sources it read, so a real one is within these.
const (
	// MaxNamedSources is the most sources a call names in each of its lists.
	MaxNamedSources = 64
	// MaxLevels is more than there are levels, for a list that repeats one.
	MaxLevels = 8
	// maxSourceID is far longer than a source's ID, the start of a container's.
	maxSourceID = 64
	// maxQuery is the longest search a call asks for, which travels down the main stream.
	maxQuery = 1 << 10
)

// ValidSourceID reports what can be a source's ID.
func ValidSourceID(id string) bool {
	if id == "" || len(id) > maxSourceID {
		return false
	}
	for i := range len(id) {
		switch c := id[i]; {
		case c >= '0' && c <= '9', c >= 'a' && c <= 'z', c >= 'A' && c <= 'Z', c == '-', c == '_', c == '.':
		default:
			return false
		}
	}
	return true
}

// checkCall refuses a call that names more than a server reads for one.
func checkCall(call *agentv1.LogsCall) error {
	refuse := func(what string, n, limit int) error {
		return fmt.Errorf("this call names %d %s, and a server takes at most %d for one, so it read nothing. The control plane checks this first, so check that the two are the same version", n, what, limit)
	}
	for _, list := range []int{len(call.GetAfter()), len(call.GetBefore()), len(call.GetSourceIds())} {
		if list > MaxNamedSources {
			return refuse("sources", list, MaxNamedSources)
		}
	}
	if n := len(call.GetLevels()); n > MaxLevels {
		return refuse("levels", n, MaxLevels)
	}
	if n := len(call.GetQuery()); n > maxQuery {
		return refuse("bytes of search", n, maxQuery)
	}
	for _, c := range slices.Concat(call.GetAfter(), call.GetBefore()) {
		if !ValidSourceID(c.GetSourceId()) {
			return errors.New("this call names a source that isn't one, so it read nothing. The control plane checks this first, so check that the two are the same version")
		}
	}
	for _, id := range call.GetSourceIds() {
		if !ValidSourceID(id) {
			return errors.New("this call names a source that isn't one, so it read nothing. The control plane checks this first, so check that the two are the same version")
		}
	}
	return nil
}

// join gives a stream a place among this server's readers, and a share of what
// the server carries live.
func (r *Reader) join(s *stream) error {
	r.placesMu.Lock()
	defer r.placesMu.Unlock()
	if len(r.places) >= MaxStreams {
		return ErrBusy
	}
	mine, past := 0, 0
	for o := range r.places {
		if s.reader != "" && o.reader == s.reader {
			mine++
		}
		if !o.call.GetFollow() {
			past++
		}
	}
	switch {
	case mine >= MaxStreamsPerReader:
		return errBusyReader
	case !s.call.GetFollow() && past >= MaxStreams-keptForLive:
		return errBusySearch
	}
	if r.places == nil {
		r.places = map[*stream]bool{}
	}
	r.places[s] = true
	r.reshare()
	return nil
}

// leave gives up a stream's place, and hands its share to those left.
func (r *Reader) leave(s *stream) {
	r.placesMu.Lock()
	defer r.placesMu.Unlock()
	delete(r.places, s)
	r.reshare()
}

// reshare divides what the server carries live evenly between the services
// being followed, then each service's evenly between its followers. So a
// service that writes little loses nothing however noisy another is, and the
// shares add up to the server's limit, with no budget for them to fight over.
// What a follow may read and not send is divided evenly between the follows.
func (r *Reader) reshare() {
	liveRate, totalRate := r.LiveRate, r.TotalRate
	if liveRate <= 0 {
		liveRate = LiveBytesPerSecond
	}
	if totalRate <= 0 {
		totalRate = TotalBytesPerSecond
	}
	shedRate := r.ShedRate
	if shedRate <= 0 {
		shedRate = shedBytesPerSecond
	}
	followers := map[string]int{}
	following := 0
	for s := range r.places {
		if s.call.GetFollow() {
			followers[s.key()]++
			following++
		}
	}
	for s := range r.places {
		if !s.call.GetFollow() {
			continue
		}
		share := totalRate / len(followers) / followers[s.key()]
		s.shared.Store(share < liveRate)
		s.rate.set(min(share, liveRate))
		s.shed.set(shedRate / following)
	}
}

// takeScan waits for this server's turn to search a container, until ctx ends,
// and says whether it came.
func (r *Reader) takeScan(ctx context.Context) bool {
	r.scansOnce.Do(func() { r.scans = make(chan struct{}, maxScans) })
	select {
	case r.scans <- struct{}{}:
		return true
	case <-ctx.Done():
		return false
	}
}

func (r *Reader) giveScan() { <-r.scans }

// bucket limits a rate, in bytes a second.
type bucket struct {
	mu     sync.Mutex
	rate   float64
	burst  float64
	tokens float64
	last   time.Time
}

func newBucket() *bucket { return &bucket{last: time.Now()} }

// set changes the rate. A burst is a quarter of a second's worth: enough not to
// cut a normal burst of lines, little enough to hold the rate.
func (b *bucket) set(rate int) {
	b.mu.Lock()
	defer b.mu.Unlock()
	b.rate = float64(rate)
	b.burst = b.rate / 4
	if b.last.IsZero() || b.tokens > b.burst {
		b.tokens = b.burst
	}
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

// pacer slows what reads lines it won't send, a stretch at a time, to a rate in
// bytes a second. Going through it costs a count, with no allocation, so a line
// that goes nowhere stays cheap.
type pacer struct {
	mu    sync.Mutex
	rate  float64
	owed  int
	since time.Time
}

// paceEvery is how many bytes go between looks at the time.
const paceEvery = 256 << 10

func (p *pacer) set(rate int) {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.rate = float64(rate)
}

// wait notes size bytes read and not sent, and sleeps as needed to keep to the
// rate. Sources of one stream wait in turn, so the rate is the stream's.
func (p *pacer) wait(ctx context.Context, size int) {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.owed += size
	if p.owed < paceEvery || p.rate <= 0 {
		return
	}
	now := time.Now()
	if !p.since.IsZero() {
		want := time.Duration(float64(p.owed) / p.rate * float64(time.Second))
		if wait := want - now.Sub(p.since); wait > 0 {
			timer := time.NewTimer(min(wait, time.Second))
			defer timer.Stop()
			select {
			case <-timer.C:
			case <-ctx.Done():
			}
			now = time.Now()
		}
	}
	p.owed, p.since = 0, now
}
