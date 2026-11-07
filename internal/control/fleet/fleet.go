// SPDX-License-Identifier: AGPL-3.0-only

// Package fleet holds the live state of every server: whether its agent is
// connected, what the server is, and its recent stats. Agents' streams
// write to it, and the API reads and watches it.
//
// Stats live only in memory, in a short ring per server for sparklines.
// What a server is (its inventory and agent build) is also written to the
// store, so an offline server still shows its OS and hardware after a
// restart.
package fleet

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"slices"
	"sync"
	"time"

	"google.golang.org/protobuf/proto"
	"google.golang.org/protobuf/types/known/timestamppb"

	agentv1 "github.com/dockwell/dockwell/gen/dockwell/agent/v1"
	dockwellv1 "github.com/dockwell/dockwell/gen/dockwell/v1"
	"github.com/dockwell/dockwell/internal/store"
)

// Tuning.
const (
	// StatsInterval is how often agents send stats.
	StatsInterval = 5 * time.Second
	// HistoryLength is how many samples are kept per server.
	HistoryLength = 60
	// HistoryWindow is how far back the samples kept go: 5 min. A server
	// that stops reporting keeps its last samples, but once it reports
	// again, samples from before a gap this long are dropped, so the
	// history never draws an outage as part of a trend.
	HistoryWindow = HistoryLength * StatsInterval
	// refreshEvery is how often what's known of a server's network is worked out
	// again with nothing new heard, as what's known of it can run out.
	refreshEvery = time.Minute
	// watcherBuffer is how far a watcher may fall behind before it is
	// dropped; it reconnects and gets a fresh snapshot.
	watcherBuffer = 256
)

// ErrNotFound reports an unknown server.
var ErrNotFound = errors.New("no such server")

// Update is one change to the fleet.
type Update struct {
	Server  *dockwellv1.Server
	Removed bool
}

type live struct {
	srv       store.Server
	inventory *dockwellv1.ServerInventory
	build     *dockwellv1.BuildInfo
	history   []*dockwellv1.ServerStats // oldest first, at most HistoryLength, within HistoryWindow of the newest
	streams   int
	lastSeen  time.Time
	// connectedAt is when the first of the open streams opened; zero
	// while there are none. A stream that opens alongside, as when the
	// agent moves to a renewed certificate, leaves it alone.
	connectedAt time.Time
	// ingress is what the server last said of its ingress. It is lost with the
	// control plane, as the agent says it again when its stream opens.
	ingress *dockwellv1.IngressStatus
	// held counts a stream that ended as its agent moved to a new one, until the
	// next opens or it runs out.
	held *time.Timer
	// dockerUnreadableSince is since when its agent hasn't been able to read
	// Docker, as it last said, on the server's clock. Each stream says it anew.
	dockerUnreadableSince *timestamppb.Timestamp
	// person is what a person set of its public addresses, with who and when: only
	// that, of what's known of its network, is kept in the store.
	person *dockwellv1.ServerNetwork
	// seen is the public address its agent last connected from, and seenAt when. It
	// goes with its stream, as do outside and domains, which its next stream says anew.
	seen   string
	seenAt time.Time
	// outside is what it last found out for itself beyond its interfaces. Nil
	// until it has said, which an agent from before this never does.
	outside *agentv1.Network
	// domains is what it last said of its routes' domains.
	domains []*dockwellv1.DomainStatus
	// proofs are what each domain's certificate showed. Unlike what's above, they stay
	// when its stream goes, and through a restart.
	proofs map[string]*proof
	// network is what all of that comes to, and refreshed when that was worked out.
	network   *dockwellv1.ServerNetwork
	refreshed time.Time
	// told are who watches for its agent's public addresses changing, one for
	// each stream.
	told map[chan struct{}]struct{}
}

func newLive(s store.Server) *live {
	return &live{srv: s, proofs: map[string]*proof{}, told: map[chan struct{}]struct{}{}}
}

// Hub is the fleet's live state.
type Hub struct {
	db     *store.DB
	logger *slog.Logger
	now    func() time.Time

	// saving keeps a server's proofs landing in the order they were weighed. It's
	// taken before mu, and held across a write to the store, which mu never is.
	saving sync.Mutex

	mu       sync.Mutex
	servers  map[string]*live
	control  string
	watchers map[chan Update]struct{}
	// controlMoved signals that the control server's public address may have changed.
	controlMoved chan struct{}
}

// New loads the servers the store knows.
func New(ctx context.Context, db *store.DB, logger *slog.Logger) (*Hub, error) {
	h := &Hub{db: db, logger: logger, now: time.Now, servers: map[string]*live{}, watchers: map[chan Update]struct{}{}, controlMoved: make(chan struct{}, 1)}
	err := db.View(ctx, func(tx *store.Tx) error {
		servers, err := tx.ListServers(ctx)
		if err != nil {
			return err
		}
		reports, err := tx.ServerReports(ctx)
		if err != nil {
			return err
		}
		for i := range servers {
			s := &servers[i]
			l := newLive(*s)
			if s.LastSeenAt != nil {
				l.lastSeen = *s.LastSeenAt
			}
			if r, ok := reports[s.ID]; ok {
				l.inventory, l.build = decodeReport(r, logger, s.ID)
			}
			if l.person, err = loadPublicAddress(ctx, tx, logger, s.ID); err != nil {
				return err
			}
			if l.proofs, err = loadProofs(ctx, tx, logger, s.ID); err != nil {
				return err
			}
			h.refreshNetworkLocked(s.ID, l)
			h.servers[s.ID] = l
		}
		return nil
	})
	if err != nil {
		return nil, fmt.Errorf("can't load the fleet: %w", err)
	}
	return h, nil
}

// SetControlServer marks the server that runs the control plane.
func (h *Hub) SetControlServer(id string) {
	h.mu.Lock()
	defer h.mu.Unlock()
	h.control = id
	h.controlMovedLocked()
}

// ControlServer returns the server that runs the control plane, without
// history, or nil before it is known.
func (h *Hub) ControlServer() *dockwellv1.Server {
	h.mu.Lock()
	defer h.mu.Unlock()
	l, ok := h.servers[h.control]
	if !ok {
		return nil
	}
	return h.toProtoLocked(h.control, l, false)
}

// Joined adds a server that has just joined.
func (h *Hub) Joined(s store.Server) {
	h.mu.Lock()
	defer h.mu.Unlock()
	if _, ok := h.servers[s.ID]; !ok {
		h.servers[s.ID] = newLive(s)
	}
	h.notifyLocked(s.ID, false)
}

// Removed drops a server.
func (h *Hub) Removed(id string) {
	h.mu.Lock()
	defer h.mu.Unlock()
	l, ok := h.servers[id]
	if !ok {
		return
	}
	delete(h.servers, id)
	if l.held != nil {
		l.held.Stop()
	}
	h.broadcastLocked(Update{Server: h.toProtoLocked(id, l, false), Removed: true})
}

// Attach records the stream of srv's agent opening with a hello. srv is
// the server as the caller just read it, so a server that joined after the
// hub loaded needs no second read. The returned function records the
// stream closing.
func (h *Hub) Attach(ctx context.Context, srv store.Server, inventory *dockwellv1.ServerInventory, build *dockwellv1.BuildInfo) (detach func(), err error) {
	id := srv.ID
	now := h.now()
	report := store.ServerReport{}
	if report.Inventory, err = proto.Marshal(inventory); err != nil {
		return nil, err
	}
	if report.AgentBuild, err = proto.Marshal(build); err != nil {
		return nil, err
	}
	if err := h.db.Update(ctx, func(tx *store.Tx) error { return tx.SaveServerReport(ctx, id, report, now) }); err != nil {
		return nil, err
	}

	h.mu.Lock()
	l, ok := h.servers[id]
	if !ok {
		l = newLive(srv)
		h.servers[id] = l
	}
	l.inventory, l.build, l.lastSeen = inventory, build, now
	h.refreshNetworkLocked(id, l)
	if l.streams == 0 {
		l.connectedAt = now
		l.dockerUnreadableSince = nil
	}
	l.streams++
	if l.held != nil {
		// The stream it held for has come, so it holds no more.
		l.held.Stop()
		l.held = nil
		l.streams--
	}
	h.notifyLocked(id, true)
	h.mu.Unlock()

	var once sync.Once
	return func() { once.Do(func() { h.detach(id) }) }, nil
}

// Hold has a server go on reading as connected after its stream ends, as its agent
// moves to a new one, as it does each time its certificate is renewed: until the
// next opens, which it does within a second, or for grace, when it reads as gone.
// Call it before the stream's detach.
func (h *Hub) Hold(id string, grace time.Duration) {
	h.mu.Lock()
	defer h.mu.Unlock()
	l, ok := h.servers[id]
	if !ok || l.held != nil {
		return
	}
	l.streams++
	var held *time.Timer
	held = time.AfterFunc(grace, func() {
		h.mu.Lock()
		defer h.mu.Unlock()
		if l.held != held || h.servers[id] != l {
			return
		}
		l.held = nil
		h.detachLocked(id, l)
	})
	l.held = held
}

func (h *Hub) detach(id string) {
	h.mu.Lock()
	defer h.mu.Unlock()
	l, ok := h.servers[id]
	if !ok {
		return
	}
	h.detachLocked(id, l)
}

func (h *Hub) detachLocked(id string, l *live) {
	l.streams--
	if l.streams == 0 {
		l.connectedAt = time.Time{}
		h.logger.Info("server disconnected", slog.String("server", id), slog.String("name", l.srv.Name))
		// What it said of itself was so while it was connected, and nothing says it
		// still is. Its next stream says it anew.
		l.seen, l.seenAt, l.outside, l.domains = "", time.Time{}, nil, nil
		h.refreshNetworkLocked(id, l)
	}
	h.notifyLocked(id, false)
	// Best effort: the stream is gone, and last_seen only refines the UI.
	go func(at time.Time) {
		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		_ = h.db.Update(ctx, func(tx *store.Tx) error { return tx.TouchServer(ctx, id, at) })
	}(l.lastSeen)
}

// RecordIngress keeps what a server says of its ingress, and tells whoever is
// watching when it changes.
func (h *Hub) RecordIngress(id string, ingress *dockwellv1.IngressStatus) {
	h.mu.Lock()
	defer h.mu.Unlock()
	l, ok := h.servers[id]
	if !ok || proto.Equal(l.ingress, ingress) {
		return
	}
	l.ingress = ingress
	h.notifyLocked(id, false)
}

// RecordDocker keeps since when a server's agent hasn't been able to read Docker,
// as it says with each report of its services' health, and tells whoever is watching
// when that changes. Nil is that it can.
func (h *Hub) RecordDocker(id string, since *timestamppb.Timestamp) {
	h.mu.Lock()
	defer h.mu.Unlock()
	l, ok := h.servers[id]
	if !ok || proto.Equal(l.dockerUnreadableSince, since) {
		return
	}
	l.dockerUnreadableSince = since
	h.notifyLocked(id, false)
}

// Record adds a stats sample from a connected server.
func (h *Hub) Record(id string, stats *dockwellv1.ServerStats) {
	h.mu.Lock()
	defer h.mu.Unlock()
	l, ok := h.servers[id]
	if !ok {
		return
	}
	l.lastSeen = h.now()
	// Samples carry the agent's clock, so the window is measured on it
	// too, and a control plane whose clock differs drops nothing it
	// shouldn't.
	if t := stats.GetTime(); t != nil {
		cutoff := t.AsTime().Add(-HistoryWindow)
		l.history = slices.DeleteFunc(l.history, func(s *dockwellv1.ServerStats) bool {
			return s.GetTime() != nil && !s.GetTime().AsTime().After(cutoff)
		})
	}
	// Nothing else may arrive while a domain's proof runs out.
	if l.lastSeen.Sub(l.refreshed) >= refreshEvery {
		h.refreshNetworkLocked(id, l)
	}
	l.history = append(l.history, stats)
	if over := len(l.history) - HistoryLength; over > 0 {
		l.history = slices.Delete(l.history, 0, over)
	}
	h.notifyLocked(id, false)
}

// List returns every server, oldest first, without history.
func (h *Hub) List() []*dockwellv1.Server {
	h.mu.Lock()
	defer h.mu.Unlock()
	return h.snapshotLocked(false)
}

// Get returns one server with its history.
func (h *Hub) Get(id string) (*dockwellv1.Server, error) {
	h.mu.Lock()
	defer h.mu.Unlock()
	l, ok := h.servers[id]
	if !ok {
		return nil, fmt.Errorf("server %s: %w", id, ErrNotFound)
	}
	return h.toProtoLocked(id, l, true), nil
}

// Watch returns every server with history, and a channel of changes from
// then on. The channel closes when ctx is done, or if the watcher falls so
// far behind that it should start again from a fresh snapshot.
func (h *Hub) Watch(ctx context.Context) ([]*dockwellv1.Server, <-chan Update) {
	ch := make(chan Update, watcherBuffer)
	h.mu.Lock()
	snapshot := h.snapshotLocked(true)
	h.watchers[ch] = struct{}{}
	h.mu.Unlock()
	go func() {
		<-ctx.Done()
		h.mu.Lock()
		defer h.mu.Unlock()
		if _, ok := h.watchers[ch]; ok {
			delete(h.watchers, ch)
			close(ch)
		}
	}()
	return snapshot, ch
}

func (h *Hub) snapshotLocked(history bool) []*dockwellv1.Server {
	ids := make([]string, 0, len(h.servers))
	for id := range h.servers {
		ids = append(ids, id)
	}
	slices.Sort(ids) // IDs sort by creation time
	out := make([]*dockwellv1.Server, 0, len(ids))
	for _, id := range ids {
		out = append(out, h.toProtoLocked(id, h.servers[id], history))
	}
	return out
}

// notifyLocked tells watchers about a server's latest state. withHistory
// sends the whole history, for a server that has just connected.
func (h *Hub) notifyLocked(id string, withHistory bool) {
	l, ok := h.servers[id]
	if !ok || len(h.watchers) == 0 {
		return
	}
	h.broadcastLocked(Update{Server: h.toProtoLocked(id, l, withHistory)})
}

func (h *Hub) broadcastLocked(u Update) {
	for ch := range h.watchers {
		select {
		case ch <- u:
		default:
			// Too far behind: drop it, and it starts again from a snapshot.
			delete(h.watchers, ch)
			close(ch)
		}
	}
}

func (h *Hub) toProtoLocked(id string, l *live, withHistory bool) *dockwellv1.Server {
	s := &dockwellv1.Server{
		Id:            id,
		Name:          l.srv.Name,
		CreateTime:    timestamppb.New(l.srv.CreatedAt),
		Inventory:     l.inventory,
		AgentBuild:    l.build,
		ControlServer: id == h.control,
		Ingress:       l.ingress,
		Network:       l.network,
	}
	switch {
	case l.streams > 0:
		s.Connection = dockwellv1.Connection_CONNECTION_CONNECTED
		s.ConnectTime = timestamppb.New(l.connectedAt)
		s.DockerUnreadableSince = l.dockerUnreadableSince
	case l.inventory != nil:
		s.Connection = dockwellv1.Connection_CONNECTION_UNREACHABLE
	default:
		s.Connection = dockwellv1.Connection_CONNECTION_PENDING
	}
	if !l.lastSeen.IsZero() {
		s.LastSeenTime = timestamppb.New(l.lastSeen)
	}
	if n := len(l.history); n > 0 {
		s.Stats = l.history[n-1]
		if withHistory {
			s.History = slices.Clone(l.history)
		}
	}
	return s
}

func decodeReport(r store.ServerReport, logger *slog.Logger, id string) (*dockwellv1.ServerInventory, *dockwellv1.BuildInfo) {
	inv, build := &dockwellv1.ServerInventory{}, &dockwellv1.BuildInfo{}
	if err := proto.Unmarshal(r.Inventory, inv); err != nil {
		logger.Warn("can't read a server's stored inventory", slog.String("server", id), slog.String("error", err.Error()))
		inv = nil
	}
	if err := proto.Unmarshal(r.AgentBuild, build); err != nil {
		build = nil
	}
	return inv, build
}
