// SPDX-License-Identifier: AGPL-3.0-only

package certificates

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"time"

	"github.com/dockwell/dockwell/internal/store"
)

// What the control plane keeps of certificates by DNS, in the store's settings. The
// ACME account's key and each certificate's key are sealed as a spec is (ADR 0022);
// what's left says nothing secret.
const (
	accountSetting = "certificates.account"
	stateSetting   = "certificates.state"
	journalSetting = "certificates.journal"
	// ordersSetting keeps when each registered domain was ordered for, a week of
	// it, for the weekly cap.
	ordersSetting = "certificates.orders"
	// deliveredSetting keeps which keys, by serial, each server was given, so each
	// goes on its timeline once, whatever restarts.
	deliveredSetting = "certificates.delivered"
	// sealedKind is what a sealed blob here is bound to, beside its holder and its
	// generation.
	sealedKind = "certificate.issued"
	// accountHolder is the ACME account's holder, for its sealing.
	accountHolder = "acme-account"
)

func issuedSetting(holder string) string { return "certificates.issued." + holder }

// account is the install's ACME account: its key, and its URL once registered.
type account struct {
	Key []byte `json:"key"`
	URL string `json:"url,omitempty"`
	Gen int    `json:"gen"`
}

// issued is a certificate the CA issued, with its key.
type issued struct {
	Chain string `json:"chain"`
	// Key is the private key in PEM, kept as bytes to be cleared after use.
	Key   []byte   `json:"key"`
	Names []string `json:"names"`
	// Serial is the leaf's serial number, in hex, which tells one key from
	// another whatever holds it.
	Serial   string    `json:"serial"`
	NotAfter time.Time `json:"not_after"`
	IssuedAt time.Time `json:"issued_at"`
}

// held is where one holder's certificate stands: nothing secret.
type held struct {
	// Gen is the generation of what's issued, which its sealing is bound to.
	Gen   int      `json:"gen,omitempty"`
	Names []string `json:"names,omitempty"`
	// Wanted are the names last asked for, which Names may be fewer than where one
	// failed and was Dropped.
	Wanted   []string  `json:"wanted,omitempty"`
	Dropped  []string  `json:"dropped,omitempty"`
	Serial   string    `json:"serial,omitempty"`
	NotAfter time.Time `json:"not_after,omitzero"`
	IssuedAt time.Time `json:"issued_at,omitzero"`
	// RenewAt is when it renews: in the CA's window where it gives one (RFC 9773).
	RenewAt time.Time `json:"renew_at,omitzero"`
	// WindowNext is when the CA is next asked for its renewal window.
	WindowNext time.Time `json:"window_next,omitzero"`
	// LastOrder is when it was last ordered, which the next order waits on.
	LastOrder time.Time `json:"last_order,omitzero"`
	// Message says why the last try failed, in the provider's or the CA's words.
	Message  string    `json:"message,omitempty"`
	Failures int       `json:"failures,omitempty"`
	Next     time.Time `json:"next,omitzero"`
}

// noted is a challenge record about to be written, or written, which a crash
// mid-order would leave in a zone: the control plane deletes what's noted as it
// starts (ADR 0088).
type noted struct {
	Provider string    `json:"provider"`
	ZoneID   string    `json:"zone_id"`
	Zone     string    `json:"zone"`
	Name     string    `json:"name"`
	Value    string    `json:"value"`
	ID       string    `json:"id,omitempty"`
	At       time.Time `json:"at"`
}

func getJSON(ctx context.Context, tx *store.Tx, key string, into any) error {
	b, err := tx.GetSetting(ctx, key)
	if errors.Is(err, store.ErrNotFound) {
		return nil
	}
	if err != nil {
		return err
	}
	if err := json.Unmarshal(b, into); err != nil {
		return fmt.Errorf("can't read %s: %w", key, err)
	}
	return nil
}

func putJSON(ctx context.Context, tx *store.Tx, key string, v any) error {
	b, err := json.Marshal(v)
	if err != nil {
		return err
	}
	return tx.PutSetting(ctx, key, b)
}

// states reads every holder's state.
func states(ctx context.Context, tx *store.Tx) (map[string]*held, error) {
	out := map[string]*held{}
	err := getJSON(ctx, tx, stateSetting, &out)
	return out, err
}

// orders reads when each registered domain was ordered for.
func orders(ctx context.Context, tx *store.Tx) (map[string][]time.Time, error) {
	out := map[string][]time.Time{}
	err := getJSON(ctx, tx, ordersSetting, &out)
	return out, err
}

// delivered reads which keys, by serial, each server was given.
func delivered(ctx context.Context, tx *store.Tx) (map[string][]string, error) {
	out := map[string][]string{}
	err := getJSON(ctx, tx, deliveredSetting, &out)
	return out, err
}

// journal reads what challenge records are noted.
func journal(ctx context.Context, tx *store.Tx) ([]noted, error) {
	var out []noted
	err := getJSON(ctx, tx, journalSetting, &out)
	return out, err
}

// sealTo seals v for holder at gen, where tx keeps its data key.
func (m *Manager) sealTo(ctx context.Context, tx *store.Tx, holder string, gen int, v any) ([]byte, error) {
	b, err := json.Marshal(v)
	if err != nil {
		return nil, err
	}
	defer clear(b)
	return m.Keeper.Seal(ctx, tx, store.SpecAt{ResourceID: holder, Kind: sealedKind, Revision: gen}, b)
}

// openFrom opens what sealTo sealed for holder at gen into into.
func (m *Manager) openFrom(ctx context.Context, tx *store.Tx, holder string, gen int, sealed []byte, into any) error {
	b, err := m.Keeper.Open(ctx, tx, store.SpecAt{ResourceID: holder, Kind: sealedKind, Revision: gen}, sealed)
	if err != nil {
		return err
	}
	defer clear(b)
	return json.Unmarshal(b, into)
}

// loadIssued opens holder's issued certificate, if it has one.
func (m *Manager) loadIssued(ctx context.Context, tx *store.Tx, holder string, gen int) (*issued, error) {
	if gen == 0 {
		return nil, nil
	}
	b, err := tx.GetSetting(ctx, issuedSetting(holder))
	if errors.Is(err, store.ErrNotFound) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	var out issued
	if err := m.openFrom(ctx, tx, holder, gen, b, &out); err != nil {
		return nil, fmt.Errorf("can't open the certificate of %s: %w", holder, err)
	}
	return &out, nil
}
