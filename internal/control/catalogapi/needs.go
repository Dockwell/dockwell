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
	"github.com/dockwell/dockwell/internal/resource/variable"
	"github.com/dockwell/dockwell/internal/revision"
	"github.com/dockwell/dockwell/internal/store"
)

// An entry's needs are met as it's installed, and as its pin says when it's upgraded
// (ADR 0084). For now a need is met by a new one beside the app.

// lookup finds a catalog entry by its ID, as listed, for a need's new one from the
// catalog, with the source it's in.
func (s sourced) lookup() catalog.Lookup {
	return func(id string) (*catalogv1.Entry, string, bool) {
		e, from, err := s.entryIn(id, "")
		if err != nil {
			return nil, "", false
		}
		return e, from.catalog, true
	}
}

// needOptions are the ways to meet each of an entry's needs, the default first for
// each, for GetCatalogEntry.
func needOptions(e *catalogv1.Entry, ways map[string][]catalog.Way) []*dockwellv1.CatalogNeedOption {
	var out []*dockwellv1.CatalogNeedOption
	for _, n := range e.GetNeeds() {
		for i := range ways[n.GetName()] {
			w := &ways[n.GetName()][i]
			o := &dockwellv1.CatalogNeedOption{Need: n.GetName(), Choice: w.Choice(), Engine: w.Engine, Version: w.Version, Why: w.Why}
			if c := w.Connection; c != nil {
				o.Connection, o.ConnectionLabel = c.Name, c.Label
				o.ConnectionShared = c.Shared && c.Serves
			}
			if w.From != nil {
				o.Entry, o.EntryName = w.From.GetId(), w.From.GetName()
			}
			if c := w.Existing; c != nil {
				o.Project, o.Environment, o.Service, o.ServiceId = c.Project, c.Environment, c.Service, c.ServiceID
				o.SharedWith, o.Servers = c.SharedWith, c.ServerNames
				o.KeepsApart = w.KeepsApart()
				o.EndsApart, _ = w.EndsApart()
				if w.UpgradeRelease > 0 {
					o.UpgradeInstall, o.UpgradeRelease, o.UpgradeApp = c.ProjectID, w.UpgradeRelease, w.UpgradeApp
				}
			}
			out = append(out, o)
		}
	}
	return out
}

// meetNeeds is an entry as an install that meets its needs runs it: as asked, by
// need, or as the install's pin says, or else by each need's first way. It refuses a
// value given for a name a need sets, as Dockwell sets each from what meets it. A need
// met by a server Dockwell runs gets its names there for project (share).
//
// What a new one comes from is pinned by digest, as the app is, through server, the
// one it goes on, before the install keeps its service in its pin; the notes say what
// that pinned. A need the install's pin hasn't met, as one a release adds, gets a note
// that an automatic update leaves for a person, as it makes a database unasked.
//
// running are the secrets an install's services use, by catalog.InsideKey, which a
// value with a secret inside keeps (catalog.NeedAs). taken are services that stay
// beside the app under names of their own, so a server it shares answers to
// <need>-link where the need's name is one of them.
func (h *Handler) meetNeeds(ctx context.Context, s sourced, e *catalogv1.Entry, values, asked map[string]string, pinned map[string]*resourcev1.PinnedNeed, candidates []catalog.Candidate, project, server string, running map[string]string, taken ...string) (*catalog.Needed, []revision.Note, error) {
	names := catalog.NeedNames(e)
	for _, name := range slices.Sorted(maps.Keys(values)) {
		if names[name] {
			return nil, nil, authapi.ReasonError(connect.CodeInvalidArgument,
				fmt.Errorf("%s is set by what meets %s's needs, so it takes no value of its own. Leave it out", name, e.GetName()), "invalid")
		}
	}
	conns, unread, err := h.connectionWays(ctx, e, project, asked)
	if err != nil {
		return nil, nil, err
	}
	met, err := catalog.Meet(e, catalog.WithConnections(e, catalog.WaysWith(e, s.lookup(), candidates, ""), conns), asked, pinned)
	if err != nil {
		// A connection the install reads that the control plane can't open says so,
		// rather than that the project can't use it.
		for _, p := range pinned {
			if slices.Contains(unread, p.GetConnection()) {
				err = fmt.Errorf("Dockwell can't read the connection %s, which the install's storage reads, so it stays on its release. Check that the control plane runs with the secrets key it was saved with", p.GetConnection()) //nolint:staticcheck // it starts with a name
			}
		}
		return nil, nil, authapi.ReasonError(connect.CodeInvalidArgument, err, "invalid")
	}
	notes := unreadNotes(unread)
	for _, name := range slices.Sorted(maps.Keys(met)) {
		w := met[name]
		if w.From != nil {
			from, pins, err := h.pinImages(ctx, w.From, server, nil)
			if err != nil {
				return nil, nil, err
			}
			w.From = from
			met[name] = w
			notes = append(notes, pins...)
		}
		// A link to a connection the install makes is checked first, from where the
		// app is to run; one nothing can check now plans with a note that says so, with
		// what the app needs the bucket to allow (ADR 0099).
		if c := w.Connection; c != nil && pinned[name].GetConnection() != c.Name {
			checked, err := h.checkConnection(ctx, e, w, server, project)
			if err != nil {
				return nil, nil, err
			}
			notes = append(notes, checked...)
		}
		// An optional need a release adds is left unset: the app runs as it did.
		if _, had := pinned[name]; pinned != nil && !had && !w.Unset {
			notes = append(notes, revision.Note{Reason: NewNeedNote, Message: newNeedWords(e, name, w)})
		}
	}
	for _, name := range slices.Sorted(maps.Keys(met)) {
		if w := met[name]; w.Shares() && slices.Contains(taken, w.Need.GetName()) {
			w.Alias = w.Need.GetName() + "-link"
			met[name] = w
		}
	}
	if err := h.share(ctx, project, met); err != nil {
		return nil, nil, err
	}
	needed, err := catalog.NeedAs(e, met, running)
	if err != nil {
		return nil, nil, authapi.ReasonError(connect.CodeInvalidArgument, err, "invalid")
	}
	return needed, notes, nil
}

// NeedRestartNote is a plan note's reason for a server the install runs already that
// a release makes a need of, met by it, as it was: its container is made again, on its
// own volume, as its settings come from the need from then on (ADR 0084).
const NeedRestartNote = "catalog_need_restart"

// restartsForNeeds says, of each need of the release the install's own server meets
// for the first time, that the server restarts once, so it surprises no one.
func restartsForNeeds(e *catalogv1.Entry, pin *resourcev1.CatalogPin, needed *catalog.Needed) []revision.Note {
	var out []revision.Note
	for _, n := range e.GetNeeds() {
		own := n.GetService()
		if own == "" || pin.GetNeeds()[n.GetName()] != nil || !strings.Contains(string(pin.GetFile()), "\n  "+own+":\n") {
			continue
		}
		p := needed.Pinned[n.GetName()]
		if p == nil || p.GetEntry() != "" || p.GetEnvironment() != "" {
			continue
		}
		out = append(out, revision.Note{
			Reason:  NeedRestartNote,
			Message: cmp.Or(catalog.EngineName(catalogv1.Engine(p.GetEngine())), own) + " restarts once to read its settings from the need",
		})
	}
	return out
}

// ConnectionUncheckedNote is a plan note's reason for a link to a connection the
// install makes, whose bucket Dockwell hasn't checked from where the app runs.
const ConnectionUncheckedNote = "catalog_connection_unchecked"

// uncheckedWords say that Dockwell hasn't checked a connection an install links to,
// what that means, and what the app needs the bucket to allow.
func uncheckedWords(e *catalogv1.Entry, w catalog.Way) string {
	words := fmt.Sprintf("Dockwell hasn't checked the connection %s from where %s runs yet, so a bucket that refuses %s shows only once it runs", cmp.Or(w.Connection.Label, w.Connection.Name), e.GetName(), e.GetName())
	if fs := catalog.NeedFeatures(w.Need); len(fs) > 0 {
		words += fmt.Sprintf(". %s needs it to allow %s", e.GetName(), catalog.FeatureWords(fs))
	}
	return words
}

// newNeedWords say what meeting a need a release adds does. Met by the entry's own
// server, as a release that moves the database an app ran beside it onto a need does,
// it's that server: one whose image makes the app a user of its own, as MySQL's and
// MariaDB's do, is then one other apps can share (ADR 0084). Met by a connection,
// it names the connection (ADR 0099).
func newNeedWords(e *catalogv1.Entry, name string, w catalog.Way) string {
	if c := w.Connection; c != nil {
		// A bucket of a connection, not a server beside the app (the slice 1 review, M2).
		return fmt.Sprintf("%s %s needs %s, which it didn't before, so it keeps its files on the connection %s", e.GetName(), e.GetVersion(), strings.ToLower(cmp.Or(w.Need.GetLabel(), name)), cmp.Or(c.Label, c.Name))
	}
	if w.From == nil && w.Existing == nil {
		engine := cmp.Or(catalog.EngineName(w.Engine), "database")
		if w.Engine == catalogv1.Engine_ENGINE_MYSQL || w.Engine == catalogv1.Engine_ENGINE_MARIADB {
			return fmt.Sprintf("%s %s takes %s as a need from this release, met by its own %s, which you can then share with other apps, each with a user of its own", e.GetName(), e.GetVersion(), needWord(e, name), engine)
		}
		return fmt.Sprintf("%s %s takes %s as a need from this release, met by its own %s", e.GetName(), e.GetVersion(), needWord(e, name), engine)
	}
	what := "its own"
	if w.From != nil {
		what = "a new " + strings.TrimSpace(catalog.EngineName(w.Engine)+" "+w.Version)
	}
	return fmt.Sprintf("%s %s needs %s, which it didn't before, so %s runs beside it", e.GetName(), e.GetVersion(), needWord(e, name), what)
}

// NewNeedNote is a plan note's reason for a release that needs what the install didn't,
// which an automatic update leaves for a person.
const NewNeedNote = "catalog_new_need"

// needWord is a need as a person reads it, its label in lower case: "a database".
func needWord(e *catalogv1.Entry, name string) string {
	for _, n := range e.GetNeeds() {
		if n.GetName() == name {
			return "a " + strings.ToLower(cmp.Or(n.GetLabel(), n.GetName()))
		}
	}
	return name
}

// keepEdited keeps each value a need sets as a person changed it in the install's
// environment: an upgrade writes only what Dockwell wrote before, where it's still
// that, so DB_ADDRESS set by hand to another server stays so (ADR 0084).
func (h *Handler) keepEdited(ctx context.Context, environment string, pin *resourcev1.CatalogPin, filled *catalog.Filled) error {
	return h.db.View(ctx, func(tx *store.Tx) error {
		for _, name := range slices.Sorted(maps.Keys(filled.Linked)) {
			was, ok := pin.GetValues()[name]
			if !ok {
				continue
			}
			r, err := tx.GetResource(ctx, string(variable.VariableKind), environment, name)
			switch {
			case errors.Is(err, store.ErrNotFound) || err == nil && r.Spec == nil:
				continue
			case err != nil:
				return err
			}
			// A variable is never sealed (ADR 0022).
			v := &resourcev1.Variable{}
			if err := proto.Unmarshal(r.Spec, v); err != nil {
				return fmt.Errorf("can't read the variable %s: %w", name, err)
			}
			if v.GetValue() != was {
				filled.Linked[name] = v.GetValue()
			}
		}
		return nil
	})
}
