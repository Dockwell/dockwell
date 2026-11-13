// SPDX-License-Identifier: AGPL-3.0-only

package revision

import (
	"cmp"
	"context"
	"errors"
	"fmt"
	"slices"
	"strings"

	"github.com/dockwell/dockwell/internal/fault"
	"github.com/dockwell/dockwell/internal/human"
	"github.com/dockwell/dockwell/internal/id"
	"github.com/dockwell/dockwell/internal/resource"
	"github.com/dockwell/dockwell/internal/store"
)

// Removing a server (ADR 0090): a change with a plan whose edits take what's on the
// server off it, moved, stopped or let go of, and which revokes the server as its
// last act, or as it commits for a server that may be compromised. What it lets go
// of takes no step there: the engine treats the server as removed for it, as ADR
// 0014 does once a server is revoked.

// Removal is the server a change takes out of the fleet.
type Removal struct {
	Server string
	Name   string `json:",omitempty"`
	// Compromised revokes it as the change commits, and takes no step on it.
	Compromised bool `json:",omitempty"`
}

// What happens to the apps a server runs as it's removed.
const (
	// AppsLeave lets go of them, running: the default.
	AppsLeave = "leave"
	// AppsMove runs them on another server, and takes them off this one.
	AppsMove = "move"
	// AppsStop takes them off the server.
	AppsStop = "stop"
)

// RemoveServer is a removal a person asks for.
type RemoveServer struct {
	Server string
	// Apps is what happens to what the server runs: AppsLeave, AppsMove or
	// AppsStop. Empty is AppsLeave.
	Apps string
	// MoveTo is the server AppsMove runs them on.
	MoveTo      string
	Compromised bool
	Summary     string
	// Notes are what the plan says beside its steps, such as which certificates
	// it re-issues and revokes, which the control plane knows.
	Notes []Note
}

// stopsOnServer are the kinds whose copy on a server a removal takes off it with a
// step when they stop or move: what runs, and what routes to it. The rest, data
// and the host's settings, stay as they are.
var stopsOnServer = []resource.Kind{"service", "container", "route", "task", "network", "link"}

// RemoveServer plans taking a server out of the fleet (ADR 0090).
func (e *Engine) RemoveServer(ctx context.Context, r RemoveServer, by By) (*Plan, error) {
	apps := cmp.Or(r.Apps, AppsLeave)
	switch {
	case apps != AppsLeave && apps != AppsMove && apps != AppsStop:
		return nil, fault.New(ErrInvalid, "%q isn't what can happen to a server's apps as it's removed: leave, move or stop", r.Apps)
	case r.Compromised && apps == AppsStop:
		return nil, fault.New(ErrInvalid, "a server that may be compromised can't be trusted to stop its apps, so it takes no step: leave them, or move them to another server")
	case apps == AppsMove && r.MoveTo == "":
		return nil, fault.New(ErrInvalid, "say which server the apps move to")
	case apps == AppsMove && r.MoveTo == r.Server:
		return nil, fault.New(ErrInvalid, "the apps can't move to the server that's removed. Choose another")
	}
	e.pmu.Lock()
	control := e.controlServer
	e.pmu.Unlock()
	if r.Server != "" && r.Server == control {
		return nil, fault.New(ErrInvalid, "the control plane runs on this server, so it can't be removed. Move the control plane to another server first")
	}
	var name string
	var edits []Edit
	err := e.db.View(ctx, func(tx *store.Tx) error {
		srv, err := tx.GetServer(ctx, r.Server)
		if errors.Is(err, store.ErrNotFound) || err == nil && srv.RevokedAt != nil {
			return fault.New(ErrInvalid, "there's no server %s in the fleet. List them with ListServers", r.Server)
		}
		if err != nil {
			return err
		}
		name = srv.Name
		if apps == AppsMove {
			to, err := tx.GetServer(ctx, r.MoveTo)
			if errors.Is(err, store.ErrNotFound) || err == nil && to.RevokedAt != nil {
				return fault.New(ErrInvalid, "there's no server %s in the fleet to move the apps to. Choose one from ListServers", r.MoveTo)
			}
			if err != nil {
				return err
			}
		}
		placed, err := tx.ListPlaced(ctx, r.Server)
		if err != nil {
			return err
		}
		for i := range placed {
			ref := resource.Ref{Kind: resource.Kind(placed[i].Kind), Scope: placed[i].Scope, Name: placed[i].Name}
			it, err := e.load(ctx, tx, ref)
			if err != nil {
				return err
			}
			edits = append(edits, removalEdit(it, r.Server, apps, r.MoveTo, r.Compromised))
		}
		return nil
	})
	if err != nil {
		return nil, err
	}
	summary := strings.TrimSpace(r.Summary)
	if summary == "" {
		summary = "Remove " + name
		if r.Compromised {
			summary += ", which may be compromised"
		}
	}
	notes := slices.Clone(r.Notes)
	if stays := stayNames(edits); len(stays) > 0 {
		notes = append(notes, Note{
			Reason: "stays_on_server", Names: stays,
			Message: fmt.Sprintf("What stays on %s as it is, which Dockwell lets go of: %s", name, human.List(stays)),
		})
	}
	notes = append(notes, Note{
		Reason: "removal_final", Names: []string{name},
		Message: name + " can't come back once it's removed: it joins again with a new ticket, as a new server",
	})
	return e.propose(ctx, Proposal{
		Summary: summary, Actor: by.Actor, Via: by.Via, Edits: edits, Notes: notes,
		Removes: &Removal{Server: r.Server, Name: name, Compromised: r.Compromised},
	}, "", false)
}

// removalEdit is what a removal does to one resource on the server leaving: a
// host's own goes with it; what's in an environment moves, stops or is let go of,
// as apps says, keeping its copies on other servers.
func removalEdit(it item, server, apps, moveTo string, compromised bool) Edit {
	left := slices.DeleteFunc(slices.Clone(it.placed), func(s string) bool { return s == server })
	edit := Edit{Ref: it.ref, Spec: it.current, Servers: left, Forget: true}
	if it.level == resource.LevelServer {
		edit.Spec, edit.Servers = nil, nil
		return edit
	}
	stops := slices.Contains(stopsOnServer, it.ref.Kind)
	switch apps {
	case AppsMove:
		if !slices.Contains(edit.Servers, moveTo) {
			edit.Servers = append(edit.Servers, moveTo)
		}
		edit.Forget = compromised || !stops
	case AppsStop:
		edit.Forget = !stops
	}
	if len(edit.Servers) == 0 {
		edit.Spec, edit.Servers = nil, nil
	}
	return edit
}

// kindWords are what a plan calls each kind of what a removal lets go of. A
// server's host is its settings, said once.
var kindWords = map[resource.Kind]string{
	"service": "the app", "container": "the container", "volume": "the volume", "route": "the route", "task": "the task",
	"network": "the network", "link": "the link", "database.user": "the database user", "firewall.rule": "the firewall rule",
	"fail2ban.jail": "the jail",
}

// stayNames are what a removal lets go of on its server, in words.
func stayNames(edits []Edit) []string {
	var out []string
	for i := range edits {
		if !edits[i].Forget {
			continue
		}
		ref := edits[i].Ref
		switch words, ok := kindWords[ref.Kind]; {
		case ref.Kind == "server.host":
			out = append(out, "its host's settings")
		case ref.Kind == "firewall.policy":
			out = append(out, "its firewall's policy")
		case ok:
			out = append(out, words+" "+ref.Name)
		default:
			out = append(out, fmt.Sprintf("%s %s", ref.Kind, ref.Name))
		}
	}
	return out
}

// leaving is the server the items' change takes out of the fleet, if it does, and
// marks it removed for each item that takes no step there: those it lets go of, and
// every one where the server may be compromised.
func leaving(items []item) *Removal {
	var r *Removal
	for i := range items {
		if items[i].removal != nil {
			r = items[i].removal
			break
		}
	}
	if r == nil {
		return nil
	}
	for i := range items {
		it := &items[i]
		if (!it.forget && !r.Compromised) || slices.Contains(it.gone, r.Server) {
			continue
		}
		if slices.Contains(it.placed, r.Server) || slices.Contains(it.servers, r.Server) {
			it.gone = append(it.gone, r.Server)
		}
	}
	return r
}

// removalIn is the server change c takes out of the fleet, as its plan says.
func removalIn(c store.Change) *Removal {
	p, err := DecodePlan(c.Plan)
	if err != nil {
		return nil
	}
	return p.Removes
}

// forgetIn says change c lets go of ref on the server it removes.
func forgetIn(c store.Change, ref resource.Ref) bool {
	p, err := DecodePlan(c.Plan)
	if err != nil {
		return false
	}
	return slices.ContainsFunc(p.Resources, func(r ResourcePlan) bool { return r.Ref == ref && r.Forget })
}

// OnRemoved says what hears of each server a change took out of the fleet, as it's
// revoked: its agent's stream closes, and its certificates' keys are replaced.
func (e *Engine) OnRemoved(fn func(server string)) { e.onRemoved = fn }

// revoke takes server out of the fleet in tx, for the change c: it's refused from
// then on, what's said of it goes, and its timeline says so (ADR 0090).
func (e *Engine) revoke(ctx context.Context, tx *store.Tx, c store.Change, server string) error {
	srv, err := tx.GetServer(ctx, server)
	if err != nil {
		return err
	}
	if srv.RevokedAt != nil {
		return nil
	}
	now := e.now()
	if err := tx.RevokeServer(ctx, server, now); err != nil {
		return err
	}
	if err := tx.DeleteSetting(ctx, store.SettingPublicAddress(server)); err != nil {
		return err
	}
	if err := tx.DeleteSetting(ctx, store.SettingDomainProofs(server)); err != nil {
		return err
	}
	actor, via := c.Actor, c.ProposedVia
	if c.AppliedBy != "" {
		actor, via = c.AppliedBy, c.AppliedVia
	}
	return tx.RecordEvent(ctx, store.Event{ID: id.New(id.Event), At: now, Actor: actor, Action: "server.revoke", Subject: server, Target: server, Detail: c.ID, Surface: via})
}

// removed tells what hears of it that a server left the fleet.
func (e *Engine) removed(server string) {
	if e.onRemoved != nil {
		e.onRemoved(server)
	}
}
