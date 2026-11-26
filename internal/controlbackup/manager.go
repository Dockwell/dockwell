// SPDX-License-Identifier: AGPL-3.0-only

package controlbackup

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"encoding/json"
	"errors"
	"io"
	"log/slog"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"time"

	"github.com/dockwell/dockwell/internal/auth"
	"github.com/dockwell/dockwell/internal/backup/keep"
	"github.com/dockwell/dockwell/internal/buildinfo"
	"github.com/dockwell/dockwell/internal/control/agentapi"
	"github.com/dockwell/dockwell/internal/id"
	"github.com/dockwell/dockwell/internal/seal"
	"github.com/dockwell/dockwell/internal/store"
)

// These actions appear on Activity with the schedule and deliveries.
const (
	ActionRequested          = "control_backup.requested"
	ActionTaken              = "control_backup.taken"
	ActionSent               = "control_backup.sent"
	ActionFailed             = "control_backup.failed"
	ActionDestination        = "control_backup.destination"
	ActionDismissDestination = "control_backup.dismiss_destination"
	ActionServers            = "control_backup.servers"
	ActionShow               = "control_backup.show_passphrase"
	ActionSet                = "control_backup.set_passphrase"
	ActionConfirm            = "control_backup.confirm_passphrase"

	offsitePlace = "offsite"
	largeBundle  = 256 << 20
)

// Actions are the control backup entries visible to install admins.
var Actions = []string{ActionRequested, ActionTaken, ActionSent, ActionFailed, ActionDestination, ActionDismissDestination, ActionServers, ActionShow, ActionSet, ActionConfirm}

// DestinationResolver reads storage coordinates and the current sealed credentials.
type DestinationResolver func(context.Context, *store.Tx, string) (string, map[string]string, error)

// Config supplies the live store and authorized delivery paths.
type Config struct {
	DB          *store.DB
	Auth        *auth.Service
	Key         *seal.Key
	KeyFile     string
	Install     string
	ControlID   string
	Address     string
	Build       buildinfo.Info
	Logger      *slog.Logger
	Agents      *agentapi.Agents
	Destination DestinationResolver
	LocalS3     bool
	Now         func() time.Time
}

// Manager owns the schedule and API for one running control plane.
type Manager struct {
	cfg      Config
	wake     chan struct{}
	running  atomic.Bool
	deriving atomic.Bool
}

// New makes a backup manager. Init prepares persistent settings.
func New(cfg Config) *Manager {
	if cfg.Now == nil {
		cfg.Now = time.Now
	}
	return &Manager{cfg: cfg, wake: make(chan struct{}, 1)}
}

func (m *Manager) recoveryPurpose() string { return "control backup recovery/" + m.cfg.Install }

func (m *Manager) sealRecovery(r Recovery) ([]byte, error) {
	raw, err := json.Marshal(r)
	if err != nil {
		return nil, err
	}
	defer clear(raw)
	return m.cfg.Key.Seal(m.recoveryPurpose(), raw), nil
}

func (m *Manager) openRecovery(s store.ControlBackup) (Recovery, error) {
	var r Recovery
	raw, err := m.cfg.Key.Open(m.recoveryPurpose(), s.Recovery)
	if err != nil {
		return r, err
	}
	defer clear(raw)
	if err := json.Unmarshal(raw, &r); err != nil {
		return r, err
	}
	if !r.KDF.valid() || len(r.Base) != keySize || r.ID != s.RecoveryID || !validID("phrase", r.ID) || r.Phrase == "" || r.SetAt.IsZero() {
		clear(r.Base)
		return Recovery{}, errors.New("the stored recovery passphrase could not be read")
	}
	return r, nil
}

// Init is called before serving requests. Restarts use the already-derived key.
func (m *Manager) Init(ctx context.Context) error {
	if m.cfg.KeyFile != "" {
		if err := sweepBackups(filepath.Dir(m.cfg.KeyFile), m.cfg.Now()); err != nil {
			m.cfg.Logger.WarnContext(ctx, "could not remove stale control plane backup work", "error", err)
		}
	}
	var s store.ControlBackup
	if err := m.cfg.DB.View(ctx, func(tx *store.Tx) error {
		var err error
		s, err = tx.ControlBackup(ctx)
		return err
	}); err != nil {
		return err
	}
	if s.GenerationID != "" {
		return nil
	}
	r := NewRecovery(GeneratePhrase(), m.cfg.Now())
	defer clear(r.Base)
	sealed, err := m.sealRecovery(r)
	if err != nil {
		return err
	}
	var random [16]byte
	_, _ = rand.Read(random[:])
	generationID := "gen_" + id.Crockford(random[:])
	return m.cfg.DB.Update(ctx, func(tx *store.Tx) error { return tx.InitControlBackup(ctx, generationID, r.ID, sealed) })
}

func sweepBackups(root string, now time.Time) error {
	entries, err := os.ReadDir(root)
	if err != nil {
		return err
	}
	var result error
	for _, entry := range entries {
		if entry.IsDir() && strings.HasPrefix(entry.Name(), ".control-backup-") {
			info, err := entry.Info()
			if err == nil && info.ModTime().Before(now.Add(-time.Hour)) {
				err = os.RemoveAll(filepath.Join(root, entry.Name()))
			}
			result = errors.Join(result, err)
		}
	}
	return result
}

func due(s store.ControlBackup, now time.Time) time.Time {
	if s.LastAttempt.UnixMilli() == 0 {
		if s.Requested || s.FirstChangedAt.UnixMilli() == 0 {
			return now
		}
		return earliest(s.ChangedAt.Add(time.Minute), s.FirstChangedAt.Add(10*time.Minute))
	}
	last := earliest(s.LastAttempt, now)
	hourly := last.Add(time.Hour)
	if s.Hourly {
		return hourly
	}
	floor := last.Add(10 * time.Minute)
	if s.Requested {
		return floor
	}
	if s.Position == s.LastPosition {
		return hourly
	}
	afterWrites := earliest(s.ChangedAt.Add(time.Minute), s.FirstChangedAt.Add(10*time.Minute))
	return earliest(hourly, latest(floor, afterWrites))
}

func earliest(a, b time.Time) time.Time {
	if a.Before(b) {
		return a
	}
	return b
}

func latest(a, b time.Time) time.Time {
	if a.After(b) {
		return a
	}
	return b
}

func (m *Manager) wakeUp() {
	select {
	case m.wake <- struct{}{}:
	default:
	}
}

// Run keeps the schedule until the control plane stops. One run is in flight at
// a time. A request and the debounce state survive a process restart.
func (m *Manager) Run(ctx context.Context) {
	for ctx.Err() == nil {
		wait, err := m.runDue(ctx)
		if err != nil && ctx.Err() == nil {
			m.cfg.Logger.WarnContext(ctx, "control plane backup could not finish", "error", err)
		}
		timer := time.NewTimer(wait)
		select {
		case <-ctx.Done():
		case <-timer.C:
		case <-m.wake:
		}
		timer.Stop()
	}
}

// runDue returns the next wait, capped so managed writes and agent joins are seen promptly.
func (m *Manager) runDue(ctx context.Context) (time.Duration, error) {
	const maxWait = time.Minute
	if !m.running.CompareAndSwap(false, true) {
		return maxWait, nil
	}
	defer m.running.Store(false)
	now := m.cfg.Now()
	var s store.ControlBackup
	if err := m.cfg.DB.View(ctx, func(tx *store.Tx) error {
		var err error
		s, err = tx.ControlBackup(ctx)
		return err
	}); err != nil {
		return maxWait, err
	}
	if wait := due(s, now).Sub(now); wait > 0 {
		return min(maxWait, wait), nil
	}
	if s.Destination == "" && !s.Requested && !m.hasAgent(s) {
		return maxWait, nil
	}
	started := false
	err := m.cfg.DB.Update(ctx, func(tx *store.Tx) error {
		var err error
		s, err = tx.ControlBackup(ctx)
		if err != nil || now.Before(due(s, now)) {
			return err
		}
		started = true
		return tx.BeginControlBackupAttempt(ctx, now)
	})
	if err != nil || !started {
		return maxWait, err
	}
	back, err := m.take(ctx, now)
	if err != nil {
		m.result(ctx, "", store.ControlBackupPlace{
			Generation: s.Generation, Problem: "The control server could not seal a backup, so no new copy left this server. Check its disk and logs",
		}, ActionFailed)
		return maxWait, err
	}
	defer back.close()
	if err := m.cfg.DB.Update(ctx, func(tx *store.Tx) error {
		if err := tx.RecordControlBackupAttempt(ctx, back.position, now, back.size > largeBundle); err != nil {
			return err
		}
		return tx.RecordEvent(ctx, store.Event{ID: id.New(id.Event), At: now, Actor: "system", Action: ActionTaken, Subject: back.header.Bundle})
	}); err != nil {
		return maxWait, err
	}
	offsiteDone := make(chan struct{})
	go func() {
		defer close(offsiteDone)
		m.deliverOffsite(ctx, back.state, back)
	}()
	m.deliverAgents(ctx, back.state, back)
	<-offsiteDone
	return 0, nil
}

type takenBundle struct {
	header   Header
	name     string
	file     *os.File
	dir      string
	sum      []byte
	size     int64
	position int64
	state    store.ControlBackup
}

func (b takenBundle) close() {
	_ = b.file.Close()
	_ = os.RemoveAll(b.dir)
}

func (m *Manager) take(ctx context.Context, now time.Time) (takenBundle, error) {
	root := filepath.Dir(m.cfg.KeyFile)
	dir, err := os.MkdirTemp(root, ".control-backup-")
	if err != nil {
		return takenBundle{}, err
	}
	keepDir := false
	defer func() {
		if !keepDir {
			_ = os.RemoveAll(dir)
		}
	}()
	path := filepath.Join(dir, "state.db")
	snapshot, err := m.cfg.DB.SnapshotControl(ctx, path)
	if err != nil {
		return takenBundle{}, err
	}
	s := snapshot.Backup
	if snapshot.Install != m.cfg.Install {
		return takenBundle{}, errors.New("the snapshot belongs to a different installation")
	}
	r, err := m.openRecovery(s)
	if err != nil {
		return takenBundle{}, err
	}
	defer clear(r.Base)
	in, err := os.Open(path) //nolint:gosec // this run’s private snapshot
	if err != nil {
		return takenBundle{}, err
	}
	defer in.Close()
	info, err := in.Stat()
	if err != nil {
		return takenBundle{}, err
	}
	if err := os.Remove(path); err != nil {
		return takenBundle{}, err
	}
	key, err := os.ReadFile(m.cfg.KeyFile)
	if err != nil {
		return takenBundle{}, err
	}
	defer clear(key)
	out, err := os.CreateTemp(dir, "sealed-")
	if err != nil {
		return takenBundle{}, err
	}
	defer func() {
		if !keepDir {
			_ = out.Close()
		}
	}()
	h := Header{
		Install: m.cfg.Install, Bundle: id.New("cbk"), Generation: s.Generation, GenerationID: s.GenerationID,
		Schema: snapshot.Schema, Version: m.cfg.Build.Version, Commit: m.cfg.Build.Commit, Taken: now.UTC(),
	}
	sum := sha256.New()
	if err := SealBundle(io.MultiWriter(out, sum), in, info.Size(), key, h, r); err != nil {
		return takenBundle{}, err
	}
	if err := in.Close(); err != nil {
		return takenBundle{}, err
	}
	sealed, err := out.Stat()
	if err != nil {
		return takenBundle{}, err
	}
	keepDir = true
	return takenBundle{
		header: h, name: keep.Name(now, h.Bundle, r.ID), file: out, dir: dir,
		sum: sum.Sum(nil), size: sealed.Size(), position: s.Position, state: s,
	}, nil
}

// EventDetail describes a delivery without including storage credentials.
type EventDetail struct {
	Place     string `json:"place,omitempty"`
	Problem   string `json:"problem,omitempty"`
	PlaceName string `json:"place_name,omitempty"`
}

func (m *Manager) result(ctx context.Context, bundle string, p store.ControlBackupPlace, action string) {
	detail, _ := json.Marshal(EventDetail{Place: p.Place, PlaceName: p.Name, Problem: p.Problem})
	err := m.cfg.DB.Update(ctx, func(tx *store.Tx) error {
		if p.Place == "" {
			if err := tx.RecordControlBackupProblem(ctx, p.Problem); err != nil {
				return err
			}
		} else if err := tx.RecordControlBackupPlace(ctx, p); err != nil {
			return err
		}
		return tx.RecordEvent(ctx, store.Event{ID: id.New(id.Event), At: m.cfg.Now(), Actor: "system", Action: action, Subject: bundle, Detail: string(detail)})
	})
	if err != nil && ctx.Err() == nil {
		m.cfg.Logger.WarnContext(ctx, "could not record where a control plane backup went", "error", err)
	}
}

func (m *Manager) recordDelivery(ctx context.Context, b takenBundle, p store.ControlBackupPlace, err error) {
	p.Generation, p.RecoveryID = b.header.Generation, b.state.RecoveryID
	if err != nil {
		p.Problem = err.Error()
		if errors.Is(err, errDestinationChanged) {
			p.Problem = destinationChanged
		}
		m.result(ctx, b.header.Bundle, p, ActionFailed)
		return
	}
	p.SentAt, p.File = m.cfg.Now(), b.name
	m.result(ctx, b.header.Bundle, p, ActionSent)
}
