// SPDX-License-Identifier: AGPL-3.0-only

package store_test

import (
	"errors"
	"testing"
	"time"

	"github.com/dockwell/dockwell/internal/store"
	"github.com/dockwell/dockwell/internal/store/storetest"
)

// An account at an issuer links to one person, and a person links one account at
// each provider; a session keeps the link it began from, and deleting the link
// ends those sessions, and no others (ADR 0095).
func TestAProviderLinkIsOneAccountForOnePerson(t *testing.T) {
	storetest.Each(t, func(t *testing.T, b store.Backend) {
		db, _ := storetest.Open(t, b)
		ctx := t.Context()
		now := time.Date(2026, 11, 14, 9, 0, 0, 0, time.UTC)
		link := func(id, user, provider, subject string) store.ProviderLink {
			return store.ProviderLink{
				ID: id, UserID: user, ProviderID: provider, Issuer: "https://auth.example.com/", Subject: subject,
				Account: subject + "@example.com", CreatedAt: now,
			}
		}
		session := func(id, link string) store.Session {
			return store.Session{
				ID: id, TokenHash: []byte("hash-" + id), UserID: "usr_mira", RPID: "panel.example.com", Method: "provider",
				CreatedAt: now, LastSeenAt: now, ExpiresAt: now.Add(time.Hour), ProviderLinkID: link,
			}
		}
		err := db.Update(ctx, func(tx *store.Tx) error {
			for _, u := range []store.User{
				{ID: "usr_mira", Email: "mira@example.com", Name: "Mira", Role: store.RoleOwner, WebAuthnHandle: []byte("m"), CreatedAt: now},
				{ID: "usr_ada", Email: "ada@example.com", Name: "Ada", WebAuthnHandle: []byte("a"), CreatedAt: now},
			} {
				if err := tx.CreateUser(ctx, u); err != nil {
					return err
				}
			}
			if err := tx.CreateProviderLink(ctx, link("plk_mira", "usr_mira", "sip_ours", "mira")); err != nil {
				return err
			}
			if err := tx.CreateProviderLink(ctx, link("plk_taken", "usr_ada", "sip_ours", "mira")); !errors.Is(err, store.ErrConflict) {
				t.Errorf("an account linked to someone else links again: %v", err)
			}
			if err := tx.CreateProviderLink(ctx, link("plk_second", "usr_mira", "sip_ours", "mira2")); !errors.Is(err, store.ErrConflict) {
				t.Errorf("a second account at one provider links: %v", err)
			}
			for _, s := range []store.Session{session("ses_by_link", "plk_mira"), session("ses_by_passkey", "")} {
				if err := tx.CreateSession(ctx, s); err != nil {
					return err
				}
			}
			return tx.UseProviderLink(ctx, "plk_mira", now.Add(time.Minute))
		})
		if err != nil {
			t.Fatal(err)
		}
		err = db.Update(ctx, func(tx *store.Tx) error {
			got, err := tx.GetProviderLinkByAccount(ctx, "https://auth.example.com/", "mira")
			if err != nil || got.UserID != "usr_mira" || got.LastUsedAt == nil || !got.LastUsedAt.Equal(now.Add(time.Minute)) {
				t.Errorf("the link reads %+v, %v", got, err)
			}
			if _, err := tx.GetProviderLinkByAccount(ctx, "https://other.example.com/", "mira"); !errors.Is(err, store.ErrNotFound) {
				t.Errorf("the same subject at another issuer reads %v", err)
			}
			if s, err := tx.GetSession(ctx, "ses_by_link"); err != nil || s.ProviderLinkID != "plk_mira" {
				t.Errorf("the session reads %+v, %v", s, err)
			}
			if _, err := tx.DeleteProviderLink(ctx, "usr_ada", "plk_mira", now); !errors.Is(err, store.ErrNotFound) {
				t.Errorf("someone else deletes the link: %v", err)
			}
			ended, err := tx.DeleteProviderLink(ctx, "usr_mira", "plk_mira", now.Add(2*time.Minute))
			if err == nil && (len(ended) != 1 || ended[0] != "ses_by_link") {
				t.Errorf("deleting the link ends the sessions %v", ended)
			}
			return err
		})
		if err != nil {
			t.Fatal(err)
		}
		_ = db.Update(ctx, func(tx *store.Tx) error {
			if s, _ := tx.GetSession(ctx, "ses_by_link"); s.RevokedAt == nil {
				t.Error("a session the deleted link began goes on")
			}
			if s, _ := tx.GetSession(ctx, "ses_by_passkey"); s.RevokedAt != nil {
				t.Error("a session a passkey began ends with the link")
			}
			if links, err := tx.ListProviderLinks(ctx, "usr_mira"); err != nil || len(links) != 0 {
				t.Errorf("mira's links are %+v, %v", links, err)
			}
			return nil
		})
	})
}
