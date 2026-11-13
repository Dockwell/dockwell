// SPDX-License-Identifier: AGPL-3.0-only

package catalogapi

import (
	"cmp"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"maps"
	"slices"
	"time"

	"google.golang.org/protobuf/proto"

	resourcev1 "github.com/dockwell/dockwell/gen/dockwell/resource/v1alpha1"
	"github.com/dockwell/dockwell/internal/catalog"
	"github.com/dockwell/dockwell/internal/compose"
	"github.com/dockwell/dockwell/internal/human"
	"github.com/dockwell/dockwell/internal/resource"
	"github.com/dockwell/dockwell/internal/resource/service"
	"github.com/dockwell/dockwell/internal/revision"
	"github.com/dockwell/dockwell/internal/scope"
	"github.com/dockwell/dockwell/internal/store"
)

// settingShown records how the apps installed before Dockwell showed an app's plain
// values were looked at (ADR 0081): "made" once nothing is left, or what waits for a
// server that was away and what was left hidden for good (shownState).
const settingShown = "catalog.shown-env"

// Showing the values installed apps set waits a little after the control plane starts,
// for its servers' agents to connect. While an app waits for a server that's away, it
// looks every ShowEvery for that server to be back, which reads and plans nothing
// until it is.
var (
	ShowFirst = time.Minute
	ShowEvery = time.Minute
)

// shownState is what's left of showing installs' plain values, by service ID.
type shownState struct {
	// Waiting are on a server that was away when last looked.
	Waiting []string `json:"waiting,omitempty"`
	// Skipped are left hidden for good: showing them would have changed what their
	// server runs, as after drift, or its plan failed. Nothing proposes them again.
	Skipped []string `json:"skipped,omitempty"`
}

// readShown reads what's left, and whether nothing is.
func (h *Handler) readShown(ctx context.Context) (st shownState, first, done bool, err error) {
	err = h.db.View(ctx, func(tx *store.Tx) error {
		b, err := tx.GetSetting(ctx, settingShown)
		switch {
		case errors.Is(err, store.ErrNotFound):
			first = true
			return nil
		case err != nil:
			return err
		case string(b) == "made":
			done = true
			return nil
		}
		return json.Unmarshal(b, &st)
	})
	return st, first, done, err
}

// KeepShowing shows the plain values of apps installed from a catalog before Dockwell
// showed them, once, as Dockwell's own change (ShowPlainValues): first ShowFirst after
// it starts, then every ShowEvery while an app waits for a server that's away, until
// ctx ends or nothing is left.
func (f *Follower) KeepShowing(ctx context.Context, connected func(server string) bool) {
	wait := ShowFirst
	for {
		select {
		case <-ctx.Done():
			return
		case <-time.After(wait):
		}
		done, err := f.ShowPlainValues(ctx, connected)
		if err != nil && ctx.Err() == nil {
			f.h.logger.ErrorContext(ctx, "can't show the plain values of installed apps; it tries again later", slog.String("error", err.Error()))
		}
		if done {
			return
		}
		wait = ShowEvery
	}
}

// shownEdit is one service whose plain values a change shows.
type shownEdit struct {
	id      string
	edit    revision.Edit
	project string // as it shows
	names   []string
}

// ShowPlainValues shows each value that an app installed from a catalog before
// Dockwell showed them set from its entry and that can't be a secret, as Dockwell's
// own change applied at once (ADR 0081). A value shows only where it still is what
// the entry's file gives with the values the install kept, which hold no secret, so
// it came from the entry's public text or an input that isn't a secret, and neither
// its name nor its shape says it could be one. Anything else stays hidden: what a
// person changed since, and every app not installed from a catalog.
//
// It changes only what reads show, which no server sees, so nothing restarts. A
// service that already shows something was decided at its install, and is left.
// The first look takes every install; after it, only what waited for a server that
// was away, once that server is back, so a look with nothing it can apply proposes
// nothing. What it can't show, as a plan that would change what a server runs, is
// remembered and never proposed again. done says nothing is left, which is
// recorded, so it never runs again.
func (f *Follower) ShowPlainValues(ctx context.Context, connected func(server string) bool) (done bool, err error) {
	h := f.h
	st, first, done, err := h.readShown(ctx)
	if err != nil || done {
		return done, err
	}
	var only map[string]bool
	if !first {
		only = make(map[string]bool, len(st.Waiting))
		for _, id := range st.Waiting {
			only[id] = true
		}
	}
	ready, away, err := h.plainValues(ctx, connected, only, st.Skipped)
	if err != nil {
		return false, err
	}
	if !first && len(ready) == 0 && len(away) == len(st.Waiting) {
		return false, nil // the servers it waits for are still away
	}
	st.Skipped = append(st.Skipped, f.applyShown(ctx, ready)...)
	st.Waiting = away
	return len(away) == 0, h.db.Update(ctx, func(tx *store.Tx) error {
		if len(away) == 0 {
			return tx.PutSetting(ctx, settingShown, []byte("made"))
		}
		b, err := json.Marshal(st)
		if err != nil {
			return err
		}
		return tx.PutSetting(ctx, settingShown, b)
	})
}

// applyShown applies what ready names, as one change, and says which it left hidden.
// Should that change fail to plan or apply, or do anything on a server, as when one
// drifted, which is no part of showing a value, each service goes on its own: those
// that plan nothing apply, and the rest are left hidden, each said in the log. One
// that fails costs only itself.
func (f *Follower) applyShown(ctx context.Context, ready []shownEdit) (skipped []string) {
	if len(ready) == 0 {
		return nil
	}
	if len(ready) > 1 {
		if err := f.applyOne(ctx, ready); err == nil {
			f.h.logger.InfoContext(ctx, "showed installed apps' plain values", slog.Int("services", len(ready)))
			return nil
		}
	}
	for i := range ready {
		if err := f.applyOne(ctx, ready[i:i+1]); err != nil {
			f.h.logger.WarnContext(ctx, "left an app's plain values hidden; check it for drift",
				slog.String("service", ready[i].edit.Ref.Name), slog.String("project", ready[i].project), slog.String("error", err.Error()))
			skipped = append(skipped, ready[i].id)
			continue
		}
		f.h.logger.InfoContext(ctx, "showed an installed app's plain values", slog.String("service", ready[i].edit.Ref.Name))
	}
	return skipped
}

// errChangesServer is a plan that would change what a server runs.
var errChangesServer = errors.New("showing its values would change what its server runs")

// applyOne proposes and applies one change for ready, and discards it when it plans
// anything on a server or fails to apply.
func (f *Follower) applyOne(ctx context.Context, ready []shownEdit) error {
	by := revision.By{Actor: revision.SystemActor, Via: revision.ViaAutomatic}
	plan, err := f.proposeShown(ctx, ready)
	if err != nil {
		return err
	}
	if !stepsNothing(plan) {
		return errors.Join(errChangesServer, f.engine.Discard(ctx, plan.Change, by))
	}
	if err := f.engine.Apply(ctx, plan.Change); err != nil {
		_ = f.engine.Discard(context.WithoutCancel(ctx), plan.Change, by)
		return fmt.Errorf("can't apply the change that shows installed apps' plain values: %w", err)
	}
	return nil
}

func (f *Follower) proposeShown(ctx context.Context, ready []shownEdit) (*revision.Plan, error) {
	edits := make([]revision.Edit, len(ready))
	notes := make([]revision.Note, len(ready))
	for i := range ready {
		r := &ready[i]
		edits[i] = r.edit
		notes[i] = revision.Note{
			Reason: "shows_plain_values", Ref: r.edit.Ref, Names: r.names,
			Message: fmt.Sprintf("It shows %s of %s in %s as they are: each is what the app's catalog entry set, and none can be a secret. Nothing restarts", human.List(r.names), r.edit.Ref.Name, r.project),
		}
	}
	plan, err := f.engine.Propose(ctx, revision.Proposal{
		Summary: "Show the values installed apps set that aren't secrets",
		Actor:   revision.SystemActor, Via: revision.ViaAutomatic, Edits: edits, Notes: notes,
	})
	if err != nil {
		return nil, fmt.Errorf("can't plan showing installed apps' plain values: %w", err)
	}
	return plan, nil
}

// stepsNothing reports a plan with nothing to do on any server.
func stepsNothing(plan *revision.Plan) bool {
	for _, s := range plan.Servers {
		for i := range s.Steps {
			if s.Steps[i].Action != resource.ActionNone {
				return false
			}
		}
	}
	return true
}

// plainValues finds what each catalog install's services can show, each service
// whose servers are all connected in ready, and in away the IDs of those on a server
// that isn't. With only, it looks at those services alone; it never looks at one in
// skipped.
func (h *Handler) plainValues(ctx context.Context, connected func(string) bool, only map[string]bool, skipped []string) (ready []shownEdit, away []string, err error) {
	err = h.db.View(ctx, func(tx *store.Tx) error {
		tree, err := scope.Load(ctx, tx)
		if err != nil {
			return err
		}
		for _, p := range tree.Projects {
			pin, err := h.pinOf(ctx, tx, tree.Organization.ID, p.Name)
			if err != nil || len(pin.GetFile()) == 0 {
				// Not an install, or one from before Dockwell kept its file, which can't say
				// where a value came from.
				if errors.Is(err, store.ErrNotFound) {
					err = nil
				}
				if err != nil {
					return err
				}
				continue
			}
			given, err := entryValues(ctx, pin)
			if err != nil {
				h.logger.WarnContext(ctx, "can't read an install's file again, so its values stay hidden", slog.String("project", p.Name), slog.String("error", err.Error()))
				continue
			}
			for _, env := range p.Environments {
				list, err := tx.ListResources(ctx, env.ID, string(service.Kind))
				if err != nil {
					return err
				}
				for i := range list {
					r := list[i]
					if only != nil && !only[r.ID] || slices.Contains(skipped, r.ID) {
						continue
					}
					decoded, err := h.keeper.Decode(ctx, tx, store.InRevision(r.ID, r.Kind, r.Revision), r.Spec)
					if err != nil {
						return err
					}
					svc, ok := decoded.(*resourcev1.Service)
					if !ok || len(svc.GetShownEnv()) > 0 {
						continue
					}
					names := provablyPlain(svc.GetApp().GetEnv(), given[r.Name])
					if len(names) == 0 {
						continue
					}
					servers, err := tx.Placement(ctx, r.ID, r.Revision)
					if err != nil {
						return err
					}
					if slices.ContainsFunc(servers, func(s string) bool { return !connected(s) }) {
						away = append(away, r.ID)
						continue
					}
					next := proto.CloneOf(svc)
					next.ShownEnv = names
					ready = append(ready, shownEdit{
						id:      r.ID,
						edit:    revision.Edit{Ref: resource.Ref{Kind: service.Kind, Scope: env.ID, Name: r.Name}, Spec: next, Base: r.Revision},
						project: cmp.Or(p.DisplayName, p.Name),
						names:   names,
					})
				}
			}
		}
		return nil
	})
	return ready, away, err
}

// entryValues is what an install's file gives each of its services' own environment,
// by service, read with the values its pin kept and a value of no meaning for each
// secret, which no service keeps as its own, as it uses the secret by reference.
func entryValues(ctx context.Context, pin *resourcev1.CatalogPin) (map[string]map[string]string, error) {
	variables, _, err := catalog.Given(pin)
	if err != nil {
		return nil, err
	}
	stack, err := compose.Read(ctx, compose.Input{File: pin.GetFile(), Variables: variables})
	if err != nil {
		return nil, err
	}
	out := make(map[string]map[string]string, len(stack.Services))
	for _, s := range stack.Services {
		out[s.Name] = s.Spec.GetApp().GetEnv()
	}
	return out, nil
}

// provablyPlain names the values of env that are what the entry's file gives, from
// given, and that can't be a secret, sorted.
func provablyPlain(env, given map[string]string) []string {
	var out []string
	for _, name := range slices.Sorted(maps.Keys(env)) {
		if v, ok := given[name]; ok && v == env[name] && !catalog.MaySecret(name, v) {
			out = append(out, name)
		}
	}
	return out
}
