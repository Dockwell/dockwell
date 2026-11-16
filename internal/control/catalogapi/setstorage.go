// SPDX-License-Identifier: AGPL-3.0-only

package catalogapi

import (
	"cmp"
	"context"
	"errors"
	"fmt"
	"maps"
	"slices"
	"strings"

	"connectrpc.com/connect"
	"google.golang.org/protobuf/proto"

	catalogv1 "github.com/dockwell/dockwell/gen/dockwell/catalog/v1alpha1"
	resourcev1 "github.com/dockwell/dockwell/gen/dockwell/resource/v1alpha1"
	dockwellv1 "github.com/dockwell/dockwell/gen/dockwell/v1"
	"github.com/dockwell/dockwell/internal/catalog"
	"github.com/dockwell/dockwell/internal/control/authapi"
	"github.com/dockwell/dockwell/internal/control/composeapi"
	"github.com/dockwell/dockwell/internal/control/connections"
	"github.com/dockwell/dockwell/internal/control/place"
	"github.com/dockwell/dockwell/internal/resource/variable"
	"github.com/dockwell/dockwell/internal/revision"
	"github.com/dockwell/dockwell/internal/store"
)

// setStorageNeed plans an installed app's storage need met by a connection, from the
// app's need card (ADR 0099): one it ran without, as an optional need installs, one
// its own values meet since a Disconnect, or another connection. The connection is
// checked first, from the app's server, as an install's is. Its files stay in the
// bucket it uses now, as Dockwell copies none to another, so moving off one is
// refused with cant_copy unless the request asks to start empty.
func (h *Handler) setStorageNeed(ctx context.Context, s sourced, f found, entry *catalogv1.Entry, need *catalogv1.Need, m *dockwellv1.SetCatalogNeedRequest) (*connect.Response[dockwellv1.SetCatalogNeedResponse], error) {
	// Below an install admin, it's refused before any check reaches the bucket: it
	// changes the install's pin, which is an admin's (slice 2 review, L1).
	if c, ok := place.CallerFrom(ctx); ok && store.RoleRank(place.Holds(c.Grants, place.Install)) < store.RoleRank(store.RoleAdmin) {
		return nil, authapi.ReasonError(connect.CodePermissionDenied, errors.New("an app's storage is connected by an admin, as it changes what the install is pinned to, so nothing was planned. Ask an admin or the owner"), "needs_role")
	}
	install := f.install
	pin := install.GetPin()
	was := pin.GetNeeds()[need.GetName()]
	what := strings.ToLower(cmp.Or(need.GetLabel(), need.GetName()))
	name, ok := strings.CutPrefix(m.GetChoice(), catalog.ChoiceConnection)
	// Back onto the bucket its own values name, as after a Disconnect, its files are
	// there already: nothing starts empty (slice 2 review, L3).
	back := false
	if ok && was.GetOwn() {
		var err error
		if back, err = h.namesBucket(ctx, install.GetEnvironmentId(), name); err != nil {
			return nil, h.internal(ctx, err)
		}
	}
	switch {
	case !ok || name == "":
		return nil, authapi.ReasonError(connect.CodeInvalidArgument, fmt.Errorf("%s's %s takes a connection, named as connection:<name>, as its install offers them, so nothing was planned. To stop using one, disconnect it on the app's Environment tab", install.GetProject(), what), "invalid")
	case was.GetConnection() == name:
		return nil, authapi.ReasonError(connect.CodeFailedPrecondition, fmt.Errorf("%s's %s is on the connection %s already", install.GetProject(), what, name), "unchanged")
	case back:
	case (was.GetConnection() != "" || was.GetOwn()) && !m.GetStartEmpty():
		return nil, authapi.ReasonError(connect.CodeFailedPrecondition, fmt.Errorf("%s keeps its files in the bucket its %s uses now, and Dockwell copies none to another, so nothing was planned. Start empty on %s to go on, or copy them across first", install.GetProject(), what, name), "cant_copy")
	case !m.GetStartEmpty():
		// On its own disk, an installed app has files there already, which a bucket
		// doesn't have: it starts empty on it (slice 2 review, M3).
		return nil, authapi.ReasonError(connect.CodeFailedPrecondition, fmt.Errorf("%s keeps its files on its own disk, and Dockwell copies none to a bucket, so nothing was planned. Start empty on %s to go on, or copy them across first", install.GetProject(), name), "cant_copy")
	}
	// The entry as the install runs it, every need but this met as the pin says.
	ran := proto.CloneOf(entry)
	ran.Compose, ran.Version, ran.Release = string(pin.GetFile()), pin.GetVersion(), pin.GetRelease()
	pinned := maps.Clone(pin.GetNeeds())
	delete(pinned, need.GetName())
	server := h.serverOf(ctx, install.GetEnvironmentId())
	// Each value with a secret inside keeps the secret what runs names it by (#570).
	needed, notes, err := h.meetNeeds(ctx, s, ran, nil, map[string]string{need.GetName(): m.GetChoice()}, pinned, h.candidates(ctx), install.GetProject(), server, f.running())
	if err != nil {
		return nil, err
	}
	// Back onto its own bucket, its files are where it kept them only under the prefix
	// it kept to: where the need sets one and its own value isn't the prefix the link
	// sets, it starts empty there, which it must be asked to (slice 2 re-check, N4).
	if back {
		kept, set, err := h.ownPrefix(ctx, install.GetEnvironmentId(), need)
		if err != nil {
			return nil, h.internal(ctx, err)
		}
		if to := needed.Links[need.GetName()].GetPrefix(); set && kept != to {
			back = false
			if !m.GetStartEmpty() {
				return nil, authapi.ReasonError(connect.CodeFailedPrecondition, fmt.Errorf("%s keeps its files under %s in that bucket, and the link keeps to %s, and Dockwell copies none across, so nothing was planned. Start empty on %s to go on", install.GetProject(), cmp.Or(kept, "its root"), cmp.Or(to, "its root"), name), "cant_copy")
			}
		}
	}
	// The need isn't new: the install had it at this release, met another way, though
	// it's out of what meetNeeds was told was pinned (slice 2 review, M1).
	notes = slices.DeleteFunc(notes, func(n revision.Note) bool { return n.Reason == NewNeedNote })
	// Starting empty is said where it's applied, as Use another database says it
	// (slice 2 review, M2).
	if words := startsEmptyWords(install.GetProject(), was, name); !back {
		notes = append(notes, revision.Note{Reason: StartsEmptyNote, Message: words})
	}
	choice := catalog.Resolve(needed.Entry, pin)
	chosen, err := catalog.Choose(needed.Entry, choice)
	if err != nil {
		return nil, h.internal(ctx, err)
	}
	free, err := h.whatsFree(ctx, needed.Entry, "")
	if err != nil {
		return nil, h.internal(ctx, err)
	}
	// What the needs set is written anew, as the connection meets it.
	carried := proto.CloneOf(pin)
	for _, n := range needed.Plain {
		delete(carried.Values, n)
	}
	filled, err := catalog.Carry(chosen, carried, nil, free.values, f.held)
	if err == nil {
		err = filled.Link(needed.Plain)
	}
	if err != nil {
		return nil, authapi.ReasonError(connect.CodeInvalidArgument, err, "invalid")
	}
	old, _, err := ranFile(pin, f, needed.InsideKeys)
	if err != nil {
		return nil, h.internal(ctx, err)
	}
	oldVariables, oldStands, err := catalog.Given(old)
	if err != nil {
		return nil, h.internal(ctx, err)
	}
	oldLinked, err := catalog.StandFor(oldVariables, needed.Plain)
	if err != nil {
		return nil, h.internal(ctx, err)
	}
	// A connection points the need's names at itself, whatever they held. A value
	// with a secret inside that holds what a person gave it stays theirs, as through
	// Use another database: the need doesn't touch it (#570's recheck, N3).
	written, kept, err := h.neededEdits(ctx, needed, filled, func(name string) bool {
		return needed.Inside[name] && f.theirs(name, pin)
	})
	if err != nil {
		return nil, err
	}
	notes = append(notes, kept...)
	newPin := pinTo(entry, s.origins[entry.GetId()], filled, choice)
	newPin.Version, newPin.Release, newPin.File, newPin.Needs, newPin.Written = pin.GetVersion(), pin.GetRelease(), pin.GetFile(), needed.Pinned, needed.Written
	change, err := h.imports.PlanUpgrade(ctx, composeapi.Upgrade{
		Project:     install.GetProjectId(),
		Environment: install.GetEnvironmentId(),
		Summary:     fmt.Sprintf("Connect %s's %s to %s", install.GetProject(), what, name),
		FromRelease: pin.GetVersion(),
		Release:     pin.GetVersion(),
		Name:        f.display,
		File:        []byte(chosen.GetCompose()),
		Variables:   filled.Variables,
		Secrets:     filled.Secrets,
		Stands:      filled.Stands(),
		Pin:         newPin,
		Companion:   true,

		Old:          old.GetFile(),
		OldVariables: oldVariables,
		OldStands:    oldStands,
		Notes:        notes,
		Needs:        composeapi.Needs{Variables: filled.Linked, Stands: filled.LinkStands, OldStands: oldLinked, Links: needed.Links, Extra: written},
	})
	if err != nil {
		return nil, err
	}
	return connect.NewResponse(&dockwellv1.SetCatalogNeedResponse{Change: change}), nil
}

// StartsEmptyNote is a plan note's reason for an app that starts empty where its need
// is met now, its files left where they were, as Dockwell copies none across.
const StartsEmptyNote = "catalog_need_starts_empty"

// startsEmptyWords say where an app's files stay as its storage moves to the
// connection to, and that it starts empty there.
func startsEmptyWords(project string, was *resourcev1.PinnedNeed, to string) string {
	switch {
	case was.GetConnection() != "":
		return fmt.Sprintf("%s's files stay in %s under %s, as Dockwell copies none across; it starts empty on %s", project, was.GetConnection(), cmp.Or(was.GetPrefix(), "the bucket's root"), to)
	case was.GetOwn():
		return fmt.Sprintf("%s's files stay in the bucket its own settings name, as Dockwell copies none across; it starts empty on %s", project, to)
	}
	return fmt.Sprintf("%s's files stay on its own disk, as Dockwell copies none across; it starts empty on %s", project, to)
}

// namesBucket says the plain values of the environment env name the connection
// name's endpoint and bucket, as a Disconnect leaves them.
func (h *Handler) namesBucket(ctx context.Context, env, name string) (bool, error) {
	all, _, err := connections.NewConnections(h.db, h.keeper).Readable(ctx)
	if err != nil {
		return false, err
	}
	i := slices.IndexFunc(all, func(c connections.Saved) bool { return c.Name == name })
	if i < 0 {
		return false, nil
	}
	values := map[string]map[string]bool{env: {}}
	err = h.db.View(ctx, func(tx *store.Tx) error {
		stored, err := tx.ListResources(ctx, env, string(variable.VariableKind))
		if err != nil {
			return err
		}
		for j := range stored {
			r := &stored[j]
			if r.Spec == nil {
				continue
			}
			spec, err := h.keeper.Decode(ctx, tx, store.InRevision(r.ID, r.Kind, r.Revision), r.Spec)
			if err != nil {
				continue
			}
			if v, ok := spec.(*resourcev1.Variable); ok && v.GetFrom() == nil && v.GetValue() != "" {
				values[env][v.GetValue()] = true
			}
		}
		return nil
	})
	return err == nil && connections.Holds(values, all[i].Spec), err
}

// ownPrefix is the key prefix the app's own values keep to, in env, where the need
// sets one: the plain value of the name it sets as the prefix. set is false where
// the need sets none, or the environment holds no such value.
func (h *Handler) ownPrefix(ctx context.Context, env string, n *catalogv1.Need) (prefix string, set bool, err error) {
	i := slices.IndexFunc(n.GetSets(), func(s *catalogv1.NeedSet) bool { return s.GetPart() == catalogv1.Part_PART_KEY_PREFIX })
	if i < 0 {
		return "", false, nil
	}
	err = h.db.View(ctx, func(tx *store.Tx) error {
		r, err := tx.GetResource(ctx, string(variable.VariableKind), env, n.GetSets()[i].GetName())
		if errors.Is(err, store.ErrNotFound) || err == nil && r.Spec == nil {
			return nil
		}
		if err != nil {
			return err
		}
		spec, err := h.keeper.Decode(ctx, tx, store.InRevision(r.ID, r.Kind, r.Revision), r.Spec)
		if err != nil {
			return err
		}
		if v, ok := spec.(*resourcev1.Variable); ok && v.GetFrom() == nil {
			prefix, set = v.GetValue(), true
		}
		return nil
	})
	return prefix, set, err
}
