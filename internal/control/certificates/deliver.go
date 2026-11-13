// SPDX-License-Identifier: AGPL-3.0-only

package certificates

import (
	"context"
	"encoding/json"
	"slices"
	"strings"

	"github.com/dockwell/dockwell/internal/control/agentapi"
	"github.com/dockwell/dockwell/internal/id"
	"github.com/dockwell/dockwell/internal/revision"
	"github.com/dockwell/dockwell/internal/store"
)

// Loader is a server's agent, as certificates by DNS go to it.
type Loader interface {
	LoadsCertificates() bool
	PutCertificates(ctx context.Context, certs []agentapi.Certificate) (int, error)
}

// Agents are the servers whose agents are connected now.
type Agents interface {
	Connected() []string
	Loader(server string) (Loader, bool)
}

// ActionDeliver is a certificate's key going to a server, on its timeline (ADR
// 0088): which server, and which names, never the key.
const ActionDeliver = "certificate.deliver"

// Reconnected forgets what a server was given, so it's given it again, as its
// agent may have come back with nothing.
func (m *Manager) Reconnected(server string) {
	m.pushedMu.Lock()
	defer m.pushedMu.Unlock()
	delete(m.pushed, server)
}

// deliver gives each connected server whose agent loads them the certificates its
// routes need, each once an admin has let it have that one. What failed to go goes
// again at the next tick, from the store: nothing is ordered again for it.
func (m *Manager) deliver(ctx context.Context, wanted map[Holder]*want, st map[string]*held) {
	if m.Agents == nil {
		return
	}
	var confirmed map[string]map[string]bool
	if err := m.DB.View(ctx, func(tx *store.Tx) error {
		var err error
		confirmed, err = revision.KeysConfirmed(ctx, tx)
		return err
	}); err != nil {
		return
	}
	for _, server := range m.Agents.Connected() {
		loader, ok := m.Agents.Loader(server)
		if !ok || !loader.LoadsCertificates() {
			continue
		}
		var holders []Holder
		for h, w := range wanted {
			if slices.Contains(w.Servers, server) && st[h] != nil && st[h].Gen > 0 && confirmed[server][h] {
				holders = append(holders, h)
			}
		}
		slices.Sort(holders)
		key := make([]string, 0, len(holders))
		for _, h := range holders {
			key = append(key, h+"@"+st[h].Serial+"/"+strings.Join(wanted[h].Serves[server], ","))
		}
		now := strings.Join(key, ",")
		m.pushedMu.Lock()
		was, sent := m.pushed[server]
		m.pushedMu.Unlock()
		if sent && was == now {
			continue
		}
		// The whole set or nothing, as what's given replaces what the server has.
		certs := make([]agentapi.Certificate, 0, len(holders))
		whole := true
		for _, h := range holders {
			k, names, chain, err := m.issuedParts(ctx, h)
			if err != nil {
				m.logger().Warn("can't open a certificate to give a server; it tries again", "holder", h, "server", server, "error", err.Error())
				whole = false
				break
			}
			certs = append(certs, agentapi.Certificate{ID: h, Name: wanted[h].Name, Names: names, Serves: wanted[h].Serves[server], Chain: chain, Key: k})
		}
		if whole {
			_, err := loader.PutCertificates(ctx, certs)
			if err != nil {
				m.logger().Warn("can't give a server its certificates by DNS; it tries again", "server", server, "error", err.Error())
				whole = false
			}
		}
		for i := range certs {
			clear(certs[i].Key)
		}
		if !whole {
			continue
		}
		m.pushedMu.Lock()
		if m.pushed == nil {
			m.pushed = map[string]string{}
		}
		m.pushed[server] = now
		m.pushedMu.Unlock()
		m.recordDeliveries(ctx, server, holders, st)
	}
}

// recordDeliveries keeps each key a server gets, by its certificate's serial, until
// the certificate ends, and puts it on the server's timeline the first time: a key
// sent again, as its agent reconnected or the control plane restarted, is the same
// key. A server keeps what it was given, even once it's replaced, so each such key
// is revoked if the server goes (ADR 0088).
func (m *Manager) recordDeliveries(ctx context.Context, server string, holders []Holder, st map[string]*held) {
	_ = m.DB.Update(context.WithoutCancel(ctx), func(tx *store.Tx) error {
		gv, err := givenKeys(ctx, tx)
		if err != nil {
			return err
		}
		fresh := false
		for _, h := range holders {
			s := st[h]
			g := gv[s.Serial]
			if g == nil {
				g = &given{Holder: h, Gen: s.Gen, Names: s.Names, NotAfter: s.NotAfter}
				gv[s.Serial] = g
			}
			if slices.Contains(g.Servers, server) {
				continue
			}
			g.Servers = append(g.Servers, server)
			fresh = true
			detail, _ := json.Marshal(map[string]any{"holder": h, "serial": s.Serial, "names": s.Names})
			if err := tx.RecordEvent(ctx, store.Event{
				ID: id.New(id.Event), At: m.now(), Actor: revision.SystemActor, Action: ActionDeliver,
				Subject: server, Target: server, Detail: string(detail), Surface: store.SurfaceAutomatic,
			}); err != nil {
				return err
			}
		}
		if !fresh {
			return nil
		}
		return putJSON(ctx, tx, givenSetting, gv)
	})
}
