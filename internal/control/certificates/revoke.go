// SPDX-License-Identifier: AGPL-3.0-only

package certificates

import (
	"context"
	"encoding/json"
	"encoding/pem"
	"errors"
	"fmt"
	"slices"
	"strings"
	"time"

	"golang.org/x/crypto/acme"

	"github.com/dockwell/dockwell/internal/id"
	"github.com/dockwell/dockwell/internal/revision"
	"github.com/dockwell/dockwell/internal/store"
)

// ActionRevoke is a certificate's key revoked as a server that had it went, on that
// server's timeline (ADR 0088): which certificate and its names, never the key.
const ActionRevoke = "certificate.revoke"

// revokeTimeout bounds one revocation.
const revokeTimeout = 30 * time.Second

// revokeDue revokes the keys servers that are gone had, each with its own key and
// the reason keyCompromise, so the CA refuses that key from then on too; once the
// key's holder has another, so the servers left aren't serving a revoked one while
// they could have a new one (ADR 0088). One a server removed as compromised had
// goes at once, whatever its holder has (ADR 0090).
func (m *Manager) revokeDue(ctx context.Context, wanted map[Holder]*want, st map[string]*held) {
	var gv map[string]*given
	if err := m.DB.View(ctx, func(tx *store.Tx) error {
		var err error
		gv, err = givenKeys(ctx, tx)
		return err
	}); err != nil {
		m.logger().Warn("can't read which keys to revoke; it tries again", "error", err.Error())
		return
	}
	now := m.now()
	var due []string
	for serial, g := range gv {
		if len(g.Gone) == 0 || now.Before(g.Next) {
			continue
		}
		// A key a compromised server had goes at once; otherwise once its holder has another.
		if s := st[g.Holder]; !g.Compromised && wanted[g.Holder] != nil && s != nil && s.Serial == serial {
			continue
		}
		due = append(due, serial)
	}
	if len(due) == 0 {
		return
	}
	slices.Sort(due)
	c, reached := m.client(ctx)
	var revoked []string
	for _, serial := range due {
		g := gv[serial]
		err := reached
		if err == nil {
			err = m.revoke(ctx, c, serial, g)
		}
		if err != nil {
			if g.Failures == 0 {
				g.FailingSince = now
			}
			g.Failures++
			g.Message = sentence(err.Error())
			g.Next = now.Add(backoff(g.Failures))
			m.logger().Warn("can't revoke the key of a server that's gone; it tries again", "holder", g.Holder, "serial", serial, "servers", strings.Join(g.Gone, ","), "again_in", g.Next.Sub(now).String(), "error", err.Error())
			continue
		}
		revoked = append(revoked, serial)
		m.logger().Info("revoked the key of a server that's gone", "holder", g.Holder, "serial", serial, "servers", strings.Join(g.Gone, ","))
	}
	if err := m.DB.Update(context.WithoutCancel(ctx), func(tx *store.Tx) error {
		for _, serial := range revoked {
			g := gv[serial]
			detail, _ := json.Marshal(map[string]any{"holder": g.Holder, "serial": serial, "names": g.Names, "reason": "key_compromise"})
			for _, server := range g.Gone {
				if err := tx.RecordEvent(ctx, store.Event{
					ID: id.New(id.Event), At: now, Actor: revision.SystemActor, Action: ActionRevoke,
					Subject: server, Target: server, Detail: string(detail), Surface: store.SurfaceAutomatic,
				}); err != nil {
					return err
				}
			}
			if g.Retired {
				if err := tx.DeleteSetting(ctx, retiredSetting(serial)); err != nil {
					return err
				}
			}
			delete(gv, serial)
		}
		return putJSON(ctx, tx, givenSetting, gv)
	}); err != nil {
		m.logger().Warn("can't keep which keys were revoked; it revokes them again", "error", err.Error())
	}
}

// revoke revokes serial's certificate, signed with its own key.
func (m *Manager) revoke(ctx context.Context, c *acme.Client, serial string, g *given) error {
	var kept *issued
	if err := m.DB.View(ctx, func(tx *store.Tx) error {
		var err error
		kept, err = m.openGiven(ctx, tx, serial, g)
		return err
	}); err != nil {
		return err
	}
	defer clear(kept.Key)
	block, _ := pem.Decode([]byte(kept.Chain))
	if block == nil {
		return fmt.Errorf("the certificate kept for %s holds no certificate", strings.Join(g.Names, ", "))
	}
	key, err := parseKey(kept.Key)
	if err != nil {
		return err
	}
	ctx, cancel := context.WithTimeout(ctx, revokeTimeout)
	defer cancel()
	// A certificate the CA has revoked already is as good.
	if err := c.RevokeCert(ctx, key, block.Bytes, acme.CRLReasonKeyCompromise); err != nil && !alreadyRevoked(err) {
		return fmt.Errorf("revoking the certificate for %s: %w", strings.Join(g.Names, ", "), caError(err))
	}
	return nil
}

func alreadyRevoked(err error) bool {
	var ae *acme.Error
	return errors.As(err, &ae) && ae.ProblemType == "urn:ietf:params:acme:error:alreadyRevoked"
}

// openGiven opens the certificate kept for serial: apart once it's retired, and its
// holder's own otherwise, or where a retiring didn't keep.
func (m *Manager) openGiven(ctx context.Context, tx *store.Tx, serial string, g *given) (*issued, error) {
	where := issuedSetting(g.Holder)
	if g.Retired {
		where = retiredSetting(serial)
	}
	b, err := tx.GetSetting(ctx, where)
	if errors.Is(err, store.ErrNotFound) && g.Retired {
		b, err = tx.GetSetting(ctx, issuedSetting(g.Holder))
	}
	if errors.Is(err, store.ErrNotFound) {
		return nil, fmt.Errorf("the certificate for %s that a server that's gone had isn't kept, so it can't be revoked here. It ends on %s", strings.Join(g.Names, ", "), g.NotAfter.Format(time.DateOnly))
	}
	if err != nil {
		return nil, err
	}
	var out issued
	if err := m.openFrom(ctx, tx, g.Holder, g.Gen, b, &out); err != nil {
		return nil, fmt.Errorf("can't open the certificate for %s: %w", strings.Join(g.Names, ", "), err)
	}
	if out.Serial != serial {
		clear(out.Key)
		return nil, fmt.Errorf("the certificate for %s that a server that's gone had isn't kept, so it can't be revoked here. It ends on %s", strings.Join(g.Names, ", "), g.NotAfter.Format(time.DateOnly))
	}
	return &out, nil
}

// RemovalNotes say what a server's removal does to certificates by DNS, for its
// plan: which shared ones it re-issues, as the server has their keys now, and
// which keys it had it revokes (ADR 0088, ADR 0090).
func (m *Manager) RemovalNotes(ctx context.Context, server, name string) ([]revision.Note, error) {
	var gv map[string]*given
	var st map[string]*held
	if err := m.DB.View(ctx, func(tx *store.Tx) error {
		var err error
		if gv, err = givenKeys(ctx, tx); err != nil {
			return err
		}
		st, err = states(ctx, tx)
		return err
	}); err != nil {
		return nil, err
	}
	serials := make([]string, 0, len(gv))
	for serial := range gv {
		serials = append(serials, serial)
	}
	slices.Sort(serials)
	var reissue, revoke []string
	for _, serial := range serials {
		g := gv[serial]
		if !slices.Contains(g.Servers, server) {
			continue
		}
		names := strings.Join(g.Names, ", ")
		if !slices.Contains(revoke, names) {
			revoke = append(revoke, names)
		}
		if s := st[g.Holder]; g.Holder != server && s != nil && s.Serial == serial && !slices.Contains(reissue, names) {
			reissue = append(reissue, names)
		}
	}
	var out []revision.Note
	if len(reissue) > 0 {
		out = append(out, revision.Note{
			Reason: "reissues_certificates", Names: reissue,
			Message: "Re-issues with new keys, for the servers left: " + strings.Join(reissue, "; "),
		})
	}
	if len(revoke) > 0 {
		out = append(out, revision.Note{
			Reason: "revokes_certificates", Names: revoke,
			Message: "Revokes the certificates' keys " + name + " had: " + strings.Join(revoke, "; "),
		})
	}
	return out, nil
}

// Revocation is a key a server that's gone had, still to be revoked: waiting for
// its holder's next key, or failing (ADR 0090).
type Revocation struct {
	Serial string
	// Names are the certificate's, and Servers the servers gone that had it, by ID.
	Names   []string
	Servers []string
	// Compromised says one of them went as a server that may be compromised.
	Compromised bool
	// Message says why it failed, in the CA's words, since Since; empty while it
	// hasn't. Next is when it's tried again.
	Message string
	Since   time.Time
	Next    time.Time
}

// Revocations are the keys still to be revoked, by serial, for an alert to say
// which fail.
func (m *Manager) Revocations(ctx context.Context) ([]Revocation, error) {
	var gv map[string]*given
	if err := m.DB.View(ctx, func(tx *store.Tx) error {
		var err error
		gv, err = givenKeys(ctx, tx)
		return err
	}); err != nil {
		return nil, err
	}
	var out []Revocation
	for serial, g := range gv {
		if len(g.Gone) == 0 {
			continue
		}
		out = append(out, Revocation{
			Serial: serial, Names: slices.Clone(g.Names), Servers: slices.Clone(g.Gone), Compromised: g.Compromised,
			Message: g.Message, Since: g.FailingSince, Next: g.Next,
		})
	}
	slices.SortFunc(out, func(a, b Revocation) int { return strings.Compare(a.Serial, b.Serial) })
	return out, nil
}
