// SPDX-License-Identifier: AGPL-3.0-only

package controlbackup

import (
	"cmp"
	"context"
	"errors"
	"slices"
	"strings"
	"time"

	"golang.org/x/sync/errgroup"

	agentv1 "github.com/dockwell/dockwell/gen/dockwell/agent/v1"
	"github.com/dockwell/dockwell/internal/control/agentapi"
	"github.com/dockwell/dockwell/internal/store"
)

type candidate struct {
	id        string
	session   *agentapi.Session
	room      int64
	prior     time.Time
	preferred bool
	err       error
}

func (m *Manager) hasAgent(s store.ControlBackup) bool {
	if m.cfg.Agents == nil || m.cfg.ControlID == "" {
		return false
	}
	for _, server := range m.cfg.Agents.Connected() {
		if session, connected := m.cfg.Agents.Session(server); connected && server != m.cfg.ControlID && !slices.Contains(s.Excluded, server) && session.KeepsControlBackups() {
			return true
		}
	}
	return false
}

func (m *Manager) deliverAgents(ctx context.Context, s store.ControlBackup, b takenBundle) {
	if m.cfg.Agents == nil || m.cfg.ControlID == "" {
		return
	}
	prior := make(map[string]time.Time)
	if err := m.cfg.DB.View(ctx, func(tx *store.Tx) error {
		current, err := tx.ControlBackup(ctx)
		if err != nil {
			return err
		}
		s.Preferred, s.Excluded = current.Preferred, current.Excluded
		places, err := tx.ControlBackupPlaces(ctx, s.Generation)
		for i := range places {
			p := &places[i]
			if !strings.HasPrefix(p.Place, offsitePlace+":") && !p.SentAt.IsZero() {
				prior[p.Place] = p.SentAt
			}
		}
		return err
	}); err != nil {
		return
	}
	var choices []*candidate
	for _, server := range m.cfg.Agents.Connected() {
		if server == m.cfg.ControlID || slices.Contains(s.Excluded, server) {
			continue
		}
		session, ok := m.cfg.Agents.Session(server)
		if ok && session.KeepsControlBackups() {
			choices = append(choices, &candidate{id: server, session: session, prior: prior[server], preferred: slices.Contains(s.Preferred, server)})
		}
	}
	check, cancel := context.WithTimeout(ctx, 8*time.Second)
	var probes errgroup.Group
	probes.SetLimit(16)
	for _, pick := range choices {
		if check.Err() != nil {
			break
		}
		probes.Go(func() error {
			pick.room, pick.err = pick.session.ControlBackupRoom(check, b.header.Install, b.header.Generation)
			return nil
		})
	}
	_ = probes.Wait()
	cancel()
	choices = slices.DeleteFunc(choices, func(pick *candidate) bool {
		if pick.room >= b.size {
			return false
		}
		if pick.preferred || !pick.prior.IsZero() {
			problem := "The backup does not fit on this server. Free some disk space"
			if pick.err != nil {
				problem = "Could not check available room on this server"
			}
			m.recordDelivery(ctx, b, store.ControlBackupPlace{Place: pick.id, Target: pick.id}, errors.New(problem))
		}
		return true
	})
	slices.SortFunc(choices, func(a, b *candidate) int {
		if a.preferred != b.preferred {
			if a.preferred {
				return -1
			}
			return 1
		}
		if n := b.prior.Compare(a.prior); n != 0 {
			return n
		}
		if n := cmp.Compare(b.room, a.room); n != 0 {
			return n
		}
		return cmp.Compare(a.id, b.id)
	})
	send, cancel := context.WithTimeout(ctx, storageTimeout)
	defer cancel()
	var kept int
	for _, pick := range choices {
		if kept == 2 || send.Err() != nil {
			break
		}
		result, err := pick.session.KeepControlBackup(send, &agentv1.KeepControlBackupCall{
			Install: b.header.Install, Generation: b.header.Generation, Name: b.name,
			Size: b.size, Sha256: b.sum,
		}, b.file)
		m.recordDelivery(ctx, b, store.ControlBackupPlace{Place: pick.id, Target: pick.id, Path: result.GetPath(), Problem: result.GetPruneProblem()}, err)
		if err == nil {
			kept++
		}
	}
}
