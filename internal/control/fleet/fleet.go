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

	dockwellv1 "github.com/dockwell/dockwell/gen/dockwell/v1"
	"github.com/dockwell/dockwell/internal/store"
)

// Tuning.
const (
	// StatsInterval is how often agents send stats.
	StatsInterval = 5 * time.Second
	// HistoryLength is how many samples are kept per server: 5 min.
	HistoryLength = 60
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
	history   []*dockwellv1.ServerStats // oldest first, at most HistoryLength
	streams   int
	lastSeen  time.Time
}

// Hub is the fleet's live state.
type Hub struct {
	db     *store.DB
	logger *slog.Logger
	now    func() time.Time

	mu       sync.Mutex
	servers  map[string]*live
	control  string
	watchers map[chan Update]struct{}
}

// New loads the servers the store knows.
func New(ctx context.Context, db *store.DB, logger *slog.Logger) (*Hub, error) {
	h := &Hub{db: db, logger: logger, now: time.Now, servers: map[string]*live{}, watchers: map[chan Update]struct{}{}}
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
			l := &live{srv: *s}
			if s.LastSeenAt != nil {
				l.lastSeen = *s.LastSeenAt
			}
			if r, ok := reports[s.ID]; ok {
				l.inventory, l.build = decodeReport(r, logger, s.ID)
			}
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
}

// Joined adds a server that has just joined.
func (h *Hub) Joined(s store.Server) {
	h.mu.Lock()
	defer h.mu.Unlock()
	if _, ok := h.servers[s.ID]; !ok {
		h.servers[s.ID] = &live{srv: s}
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
	h.broadcastLocked(Update{Server: h.toProtoLocked(id, l, false), Removed: true})
}

// Attach records an agent's stream opening with a hello. The returned
// function records it closing.
func (h *Hub) Attach(ctx context.Context, id string, inventory *dockwellv1.ServerInventory, build *dockwellv1.BuildInfo) (detach func(), err error) {
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

	// A server that joined after the hub loaded isn't in memory yet.
	var joined store.Server
	if err := h.db.View(ctx, func(tx *store.Tx) error {
		var err error
		joined, err = tx.GetServer(ctx, id)
		return err
	}); err != nil {
		return nil, err
	}

	h.mu.Lock()
	l, ok := h.servers[id]
	if !ok {
		l = &live{srv: joined}
		h.servers[id] = l
	}
	l.inventory, l.build, l.lastSeen = inventory, build, now
	l.streams++
	h.notifyLocked(id, true)
	h.mu.Unlock()

	var once sync.Once
	return func() { once.Do(func() { h.detach(id) }) }, nil
}

func (h *Hub) detach(id string) {
	h.mu.Lock()
	defer h.mu.Unlock()
	l, ok := h.servers[id]
	if !ok {
		return
	}
	l.streams--
	if l.streams == 0 {
		h.logger.Info("server disconnected", slog.String("server", id), slog.String("name", l.srv.Name))
	}
	h.notifyLocked(id, false)
	// Best effort: the stream is gone, and last_seen only refines the UI.
	go func(at time.Time) {
		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		_ = h.db.Update(ctx, func(tx *store.Tx) error { return tx.TouchServer(ctx, id, at) })
	}(l.lastSeen)
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
	}
	switch {
	case l.streams > 0:
		s.Connection = dockwellv1.Connection_CONNECTION_CONNECTED
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
