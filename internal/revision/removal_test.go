// SPDX-License-Identifier: AGPL-3.0-only

package revision

import (
	"errors"
	"slices"
	"strings"
	"sync"
	"testing"

	"github.com/dockwell/dockwell/internal/resource"
	"github.com/dockwell/dockwell/internal/store"
	"github.com/dockwell/dockwell/internal/store/storetest"
)

// heardRemoved notes each server an engine says left the fleet.
type heardRemoved struct {
	mu      sync.Mutex
	servers []string
}

func hearRemovals(e *Engine) *heardRemoved {
	h := &heardRemoved{}
	e.OnRemoved(func(server string) {
		h.mu.Lock()
		defer h.mu.Unlock()
		h.servers = append(h.servers, server)
	})
	return h
}

func (h *heardRemoved) heard() []string {
	h.mu.Lock()
	defer h.mu.Unlock()
	return slices.Clone(h.servers)
}

func revoked(t *testing.T, db *store.DB, server string) bool {
	t.Helper()
	var gone bool
	_ = db.View(t.Context(), func(tx *store.Tx) error {
		s, err := tx.GetServer(t.Context(), server)
		gone = err == nil && s.RevokedAt != nil
		return err
	})
	return gone
}

func removeServer(t *testing.T, e *Engine, r RemoveServer) *Plan {
	t.Helper()
	p, err := e.RemoveServer(t.Context(), r, By{Actor: "usr_test"})
	if err != nil {
		t.Fatal(err)
	}
	return p
}

func hasNote(p *Plan, reason, says string) bool {
	return slices.ContainsFunc(p.Notes, func(n Note) bool { return n.Reason == reason && strings.Contains(n.Message, says) })
}

// A server's removal is a change with a plan that leaves its apps running by
// default: it takes no step on the server, lets go of what's there, revokes the
// server as its last act, and can't be undone (ADR 0090).
func TestRemovingAServerLeavesItsAppsRunning(t *testing.T) {
	storetest.Each(t, func(t *testing.T, b store.Backend) {
		e, f, db := setup(t, b, "srv_A", "srv_B")
		heard := hearRemovals(e)
		env := production(t, db)
		web, api := app(env, "nginx:1.27", "srv_B"), named("api", env, "api:1", "srv_A", "srv_B")
		apply(t, e, propose(t, e, "Run web and api", web, api))

		p := removeServer(t, e, RemoveServer{Server: "srv_B"})
		if p.Removes == nil || p.Removes.Server != "srv_B" || p.Removes.Compromised || !p.Destructive() {
			t.Fatalf("the plan removes %+v, destructive %v", p.Removes, p.Destructive())
		}
		for _, sp := range p.Servers {
			if sp.Server == "srv_B" && len(sp.Steps) > 0 {
				t.Errorf("leaving its apps running takes steps on it: %+v", sp.Steps)
			}
		}
		if !hasNote(p, "stays_on_server", "the app web") || !hasNote(p, "stays_on_server", "the app api") || !hasNote(p, "removal_final", "srv_B") {
			t.Errorf("the plan's notes: %+v", p.Notes)
		}
		if revoked(t, db, "srv_B") {
			t.Fatal("planning a removal revoked the server")
		}
		apply(t, e, p)
		if !revoked(t, db, "srv_B") || revoked(t, db, "srv_A") || !slices.Equal(heard.heard(), []string{"srv_B"}) {
			t.Fatalf("after the removal: revoked %v, heard %v", revoked(t, db, "srv_B"), heard.heard())
		}
		if running(t, f["srv_B"], web.Ref) != "nginx:1.27" || running(t, f["srv_A"], api.Ref) != "api:1" {
			t.Error("what ran stopped")
		}
		if d := deployments(t, db, api.Ref); !slices.Equal(d, []string{"srv_A@2=live", "srv_A@1=superseded", "srv_B@1=superseded"}) {
			t.Errorf("api's deployments after the removal: %v", d)
		}
		var events []store.Event
		_ = db.View(t.Context(), func(tx *store.Tx) error {
			var err error
			events, err = tx.ListEvents(t.Context(), 50)
			return err
		})
		if !slices.ContainsFunc(events, func(ev store.Event) bool {
			return ev.Action == "server.revoke" && ev.Subject == "srv_B" && ev.Detail == p.Change
		}) {
			t.Error("the server's timeline doesn't say it was revoked")
		}
		if _, err := e.Undo(t.Context(), p.Change, By{Actor: "usr_test"}); !errors.Is(err, ErrCantUndo) || !strings.Contains(err.Error(), "can't be undone") {
			t.Errorf("undoing a removal: %v", err)
		}
	})
}

// Moving a server's apps runs each on the server chosen before taking it off this
// one; stopping takes them off; either revokes the server once its steps passed
// (ADR 0090).
func TestRemovingAServerMovesOrStopsItsApps(t *testing.T) {
	storetest.Each(t, func(t *testing.T, b store.Backend) {
		for _, apps := range []string{AppsMove, AppsStop} {
			t.Run(apps, func(t *testing.T) {
				e, f, db := setup(t, b, "srv_A", "srv_B")
				env := production(t, db)
				web := app(env, "nginx:1.27", "srv_B")
				// A host's own setting stays as it is, beside the step that takes web off.
				apply(t, e, propose(t, e, "Run web on srv_B", web, rule("srv_B", "http", 80)))

				p := removeServer(t, e, RemoveServer{Server: "srv_B", Apps: apps, MoveTo: "srv_A"})
				var onB []resource.Action
				for _, sp := range p.Servers {
					if sp.Server == "srv_B" {
						for _, s := range sp.Steps {
							onB = append(onB, s.Action)
						}
					}
				}
				if !slices.Equal(onB, []resource.Action{resource.ActionDelete}) {
					t.Errorf("on srv_B the plan takes %v, want web off it", onB)
				}
				apply(t, e, p)
				if !revoked(t, db, "srv_B") {
					t.Error("the server wasn't revoked")
				}
				if got := running(t, f["srv_B"], web.Ref); got != "" {
					t.Errorf("web still runs on srv_B: %s", got)
				}
				want := ""
				if apps == AppsMove {
					want = "nginx:1.27"
				}
				if got := running(t, f["srv_A"], web.Ref); got != want {
					t.Errorf("on srv_A web runs %q, want %q", got, want)
				}
			})
		}
	})
}

// A server that may be compromised is revoked as the change commits, before any
// step, and the change takes none on it: its apps move or stay, and can't be
// stopped. The control server can't be removed (ADR 0090).
func TestACompromisedServerIsRevokedAsTheChangeCommits(t *testing.T) {
	storetest.Each(t, func(t *testing.T, b store.Backend) {
		e, f, db := setup(t, b, "srv_A", "srv_B")
		heard := hearRemovals(e)
		env := production(t, db)
		web := app(env, "nginx:1.27", "srv_B")
		apply(t, e, propose(t, e, "Run web on srv_B", web))

		if _, err := e.RemoveServer(t.Context(), RemoveServer{Server: "srv_B", Apps: AppsStop, Compromised: true}, By{Actor: "usr_test"}); !errors.Is(err, ErrInvalid) {
			t.Errorf("stopping a compromised server's apps: %v", err)
		}
		e.SetControlServer("srv_A")
		if _, err := e.RemoveServer(t.Context(), RemoveServer{Server: "srv_A"}, By{Actor: "usr_test"}); !errors.Is(err, ErrInvalid) || !strings.Contains(err.Error(), "control plane runs on this server") {
			t.Errorf("removing the control server: %v", err)
		}

		p := removeServer(t, e, RemoveServer{Server: "srv_B", Apps: AppsMove, MoveTo: "srv_A", Compromised: true})
		for _, sp := range p.Servers {
			if sp.Server == "srv_B" && len(sp.Steps) > 0 {
				t.Errorf("the plan takes steps on a compromised server: %+v", sp.Steps)
			}
		}
		f["srv_B"].online.Store(false)
		apply(t, e, p)
		if !revoked(t, db, "srv_B") || !slices.Equal(heard.heard(), []string{"srv_B"}) {
			t.Errorf("revoked %v, heard %v", revoked(t, db, "srv_B"), heard.heard())
		}
		if running(t, f["srv_A"], web.Ref) != "nginx:1.27" {
			t.Error("web didn't move to srv_A")
		}
		if running(t, f["srv_B"], web.Ref) != "nginx:1.27" {
			t.Error("a step reached the compromised server")
		}
		if _, err := e.RemoveServer(t.Context(), RemoveServer{Server: "srv_B"}, By{Actor: "usr_test"}); !errors.Is(err, ErrInvalid) {
			t.Errorf("removing a removed server: %v", err)
		}
	})
}
