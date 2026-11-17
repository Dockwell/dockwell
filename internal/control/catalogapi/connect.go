// SPDX-License-Identifier: AGPL-3.0-only

package catalogapi

import (
	"cmp"
	"context"
	"fmt"
	"log/slog"
	"maps"
	"slices"
	"strings"

	"google.golang.org/protobuf/proto"

	catalogv1 "github.com/dockwell/dockwell/gen/dockwell/catalog/v1alpha1"
	resourcev1 "github.com/dockwell/dockwell/gen/dockwell/resource/v1alpha1"
	dockwellv1 "github.com/dockwell/dockwell/gen/dockwell/v1"
	"github.com/dockwell/dockwell/internal/catalog"
	"github.com/dockwell/dockwell/internal/control/connections"
	"github.com/dockwell/dockwell/internal/resource/connection"
	"github.com/dockwell/dockwell/internal/resource/link"
	"github.com/dockwell/dockwell/internal/resource/variable"
	"github.com/dockwell/dockwell/internal/revision"
	"github.com/dockwell/dockwell/internal/scope"
	"github.com/dockwell/dockwell/internal/store"
)

// An install meets a need of a kind beyond the databases with a connection the app's
// project may use: a link named for the need reads the connection's values into the
// names the need sets (ADR 0099).

// ConnectionChecks checks a connection's bucket before an install links an app to it
// (ADR 0099).
type ConnectionChecks interface {
	// CheckBeforeLinking checks the connection name's bucket under prefix from
	// server's agent, on the networks of the app env and app name where it runs
	// already, or else on a network made for the check, trying presigned URLs where
	// presign asks. It refuses a bucket that fails, in the check's words; checked is
	// false, with no error, where nothing can check it now.
	CheckBeforeLinking(ctx context.Context, name, server, env, app, prefix string, presign bool) (checked bool, err error)
}

// CheckWith has an install check each connection it links an app to, through c,
// before it plans.
func (h *Handler) CheckWith(c ConnectionChecks) { h.checks = c }

// checkConnection checks the connection a need is met by from server, as the app
// will reach it, before the install plans: a bucket that fails refuses the plan, and
// one nothing can check now plans with a note that says so.
func (h *Handler) checkConnection(ctx context.Context, e *catalogv1.Entry, w catalog.Way, server, project string) ([]revision.Note, error) {
	checked := false
	if h.checks != nil {
		presign := slices.Contains(catalog.NeedFeatures(w.Need), catalogv1.Feature_FEATURE_PRESIGNED_URLS)
		env, app, err := h.appOf(ctx, project, w.Need)
		if err != nil {
			return nil, err
		}
		if checked, err = h.checks.CheckBeforeLinking(ctx, w.Connection.Name, server, env, app, w.Connection.Prefix, presign); err != nil {
			return nil, err
		}
	}
	if checked {
		return nil, nil
	}
	return []revision.Note{{Reason: ConnectionUncheckedNote, Message: uncheckedWords(e, w)}}, nil
}

// appOf is where an install's app runs already, for a check from its networks: the
// environment of project, by name or ID, and the service there that reads a name n
// sets. Both are empty for a project an install is about to make.
func (h *Handler) appOf(ctx context.Context, project string, n *catalogv1.Need) (env, app string, err error) {
	if project == "" || h.db == nil {
		return "", "", nil
	}
	names := make([]string, 0, len(n.GetSets()))
	for _, s := range n.GetSets() {
		names = append(names, s.GetName())
	}
	err = h.db.View(ctx, func(tx *store.Tx) error {
		tree, err := scope.Load(ctx, tx)
		if err != nil {
			return err
		}
		for _, p := range tree.Projects {
			if p.Name != project && p.ID != project {
				continue
			}
			for _, e := range p.Environments {
				if env == "" || e.Name == scope.DefaultEnvironment {
					env = e.ID
				}
			}
		}
		if env == "" || len(names) == 0 {
			return nil
		}
		users, err := tx.UsersOf(ctx, env, "", names...)
		if err != nil {
			return err
		}
		for i := range users {
			if u := &users[i]; u.Kind == "service" && u.Scope == env {
				app = u.Name
				return nil
			}
		}
		return nil
	})
	if app == "" {
		env = ""
	}
	return env, app, err
}

// connectionWays are the connections that may meet an install's needs in project,
// by its name or ID: those that reach every project, and, for a project that's there
// already, those that name it. A project an install makes is new, so a connection
// it names can't reach it yet. Each keeps the app's objects under the project's name,
// and one named for the install's environment, as "uploads-production" is, is the
// default among several.
//
// It reads none for an entry with no need a connection meets, and leaves out one
// the control plane can't open, naming it in unread, so that connection fails no
// other app's sheet, install or upgrade (the slice 1 review, M3).
//
// An install that asks for a connection that reaches no project yet by name, in
// asked, is offered it, as one an admin added from the install sheet: its plan gives
// the connection the project it makes (Widens, ADR 0099). An install is an admin's,
// and nothing else asks. One that names other projects is never widened.
func (h *Handler) connectionWays(ctx context.Context, e *catalogv1.Entry, project string, asked map[string]string) ([]catalog.ConnectionWay, []string, error) {
	if h.db == nil || h.keeper == nil || !slices.ContainsFunc(e.GetNeeds(), func(n *catalogv1.Need) bool { return n.GetKind() == catalogv1.Kind_KIND_S3 }) {
		return nil, nil, nil
	}
	projectID, projectName := "", project
	// The environments' names on the install, which a set of connections is named for.
	environments := map[string]bool{}
	if err := h.db.View(ctx, func(tx *store.Tx) error {
		tree, err := scope.Load(ctx, tx)
		if err != nil {
			return err
		}
		for _, p := range tree.Projects {
			if project != "" && (p.Name == project || p.ID == project) {
				projectID, projectName = p.ID, p.Name
			}
			for _, e := range p.Environments {
				environments[e.Name] = true
			}
		}
		return nil
	}); err != nil {
		return nil, nil, err
	}
	conns := connections.NewConnections(h.db, h.keeper)
	all, unread, err := conns.Readable(ctx)
	if err != nil {
		return nil, nil, err
	}
	for _, name := range unread {
		h.logger.WarnContext(ctx, "can't read a connection, so no install is offered it. Check that the control plane runs with the secrets key it was saved with", slog.String("connection", name))
	}
	uses, err := conns.Uses(ctx)
	if err != nil {
		return nil, nil, err
	}
	values, err := conns.OwnValues(ctx, projectID)
	if err != nil {
		return nil, nil, err
	}
	var out []catalog.ConnectionWay
	for i := range all {
		c := &all[i]
		if c.Spec.GetKind() != resourcev1.LinkKind_LINK_KIND_S3 {
			continue
		}
		reaches := c.Spec.GetReach() == resourcev1.ConnectionReach_CONNECTION_REACH_EVERY || projectID != "" && connection.Reaches(c.Spec, projectID)
		// Only one that reaches no project yet, as the install sheet saves one: a
		// connection that names other projects keeps to them, so an install never
		// widens what an admin chose for them.
		reachesNone := c.Spec.GetReach() == resourcev1.ConnectionReach_CONNECTION_REACH_UNSPECIFIED ||
			c.Spec.GetReach() == resourcev1.ConnectionReach_CONNECTION_REACH_PROJECTS && len(c.Spec.GetProjects()) == 0
		widens := !reaches && reachesNone && projectID == "" && project != "" && slices.Contains(slices.Collect(maps.Values(asked)), catalog.ChoiceConnection+c.Name)
		if !reaches && !widens {
			continue
		}
		// Every link counts, the project's own other apps' too: an upgrade keeps an
		// install's own pinned connection by another path (the slice 1 re-check, N1).
		var taken []string
		for _, u := range uses[c.Name] {
			// Where its app keeps its files: an app no link tells a prefix keeps them
			// at the root, so nothing keeps another apart from it (the review's H1).
			taken = append(taken, connections.KeptPrefix(u))
		}
		// A connection serves one app unless an admin shared it: then a need that keeps
		// to a prefix may take it beside apps that keep to theirs, under a prefix none
		// of them holds (ADR 0099).
		held := connections.Holds(values, c.Spec)
		out = append(out, catalog.ConnectionWay{
			Name: c.Name, Label: cmp.Or(c.Spec.GetDisplayName(), c.Name), PathStyle: c.Spec.GetS3().GetPathStyle(),
			Prefix: link.FreePrefix(prefixFor(projectName), taken),
			Named:  namedForEnvironment(c.Name, all, environments),
			Serves: len(taken) > 0 || held,
			Shared: c.Spec.GetShared(),
			Apart:  !held && !slices.Contains(taken, ""),
			Widens: widens,
		})
	}
	return out, unread, nil
}

// unreadNotes say, in an install's plan, which connections no need was offered, as
// the control plane can't open them.
func unreadNotes(unread []string) []revision.Note {
	var out []revision.Note
	for _, name := range unread {
		out = append(out, revision.Note{
			Reason:  ConnectionUnreadNote,
			Message: fmt.Sprintf("Dockwell can't read the connection %s, so it wasn't offered for the app's storage. Check that the control plane runs with the secrets key it was saved with", name),
		})
	}
	return out
}

// ConnectionReachNote is a plan note's reason for a connection an install gives the
// project it makes, as one an admin added from the install sheet (ADR 0099).
const ConnectionReachNote = "catalog_connection_reach"

// reachNotes say, in an install's plan, which connections it gives the project it
// makes, so the reach is said, never implied.
func reachNotes(names []string, project string) []revision.Note {
	var out []revision.Note
	for _, name := range names {
		out = append(out, revision.Note{
			Reason:  ConnectionReachNote,
			Message: fmt.Sprintf("The connection %s reaches the project %s from this install on, so its apps may use it", name, project),
		})
	}
	return out
}

// ConnectionUnreadNote is a plan note's reason for a connection an install wasn't
// offered, as the control plane can't open it.
const ConnectionUnreadNote = "catalog_connection_unread"

// namedForEnvironment says a connection is named for an install's environment as
// one of a set Add connection names for each environment of a service: its name is
// "<stem>-production", and another connection's is "<stem>-<another>", such as
// uploads-staging (ADR 0099). A name that merely ends in -production, as
// backups-production alone does, isn't (the slice 1 review, L1).
//
// The sibling's word is an environment's name on the install, as staging is, so
// backups-production beside backups-offsite isn't a set (slice 1 re-check, N6).
func namedForEnvironment(name string, all []connections.Saved, environments map[string]bool) bool {
	stem, ok := strings.CutSuffix(name, "-"+scope.DefaultEnvironment)
	if !ok || stem == "" {
		return false
	}
	return slices.ContainsFunc(all, func(c connections.Saved) bool {
		other, sibling := strings.CutPrefix(c.Name, stem+"-")
		return sibling && other != scope.DefaultEnvironment && environments[other]
	})
}

// prefixFor is where an install's objects go in a connection's bucket: under its
// project's name, so two installs that share a bucket keep apart.
func prefixFor(project string) string {
	if project == "" {
		return ""
	}
	return project + "/"
}

// connectionEdits are the names an install's needs met by a connection set, each a
// variable or a secret of the app's environment that reads the connection through
// the link named for its need. It takes the value of no meaning Fill made for each
// out of filled, so the install keeps none of it, and the app's services use each by
// reference.
func (h *Handler) connectionEdits(ctx context.Context, needed *catalog.Needed, filled *catalog.Filled) ([]*dockwellv1.Edit, error) {
	// The names the app's own values meet, as a Disconnect left them, stay as they
	// are: the install writes none of them.
	for name, secret := range needed.Kept {
		if !secret {
			delete(filled.Linked, name)
		} else if _, ok := filled.Secrets[name]; ok {
			delete(filled.Secrets, name)
			filled.Held[name] = filled.Variables[name]
		}
	}
	if len(needed.Read) == 0 {
		return nil, nil
	}
	var org string
	if err := h.db.View(ctx, func(tx *store.Tx) error {
		o, err := tx.Organization(ctx)
		org = o.ID
		return err
	}); err != nil {
		return nil, err
	}
	var out []*dockwellv1.Edit
	for _, name := range slices.Sorted(maps.Keys(needed.Read)) {
		r := needed.Read[name]
		from := &resourcev1.ValueFrom{Source: &resourcev1.ValueFrom_Connection{Connection: &resourcev1.ConnectionRef{
			Scope: org, Name: r.Connection, Part: r.Part, Link: r.Link,
		}}}
		if r.Secret {
			if _, ok := filled.Secrets[name]; ok {
				delete(filled.Secrets, name)
				filled.Held[name] = filled.Variables[name]
			}
			out = append(out, &dockwellv1.Edit{
				Resource: &dockwellv1.ResourceRef{Kind: string(variable.SecretKind), Name: name},
				Spec:     &resourcev1.Spec{Kind: &resourcev1.Spec_Secret{Secret: &resourcev1.Secret{From: from}}},
			})
			continue
		}
		delete(filled.Linked, name)
		out = append(out, &dockwellv1.Edit{
			Resource: &dockwellv1.ResourceRef{Kind: string(variable.VariableKind), Name: name},
			Spec:     &resourcev1.Spec{Kind: &resourcev1.Spec_Variable{Variable: &resourcev1.Variable{From: from}}},
		})
	}
	return out, nil
}

// PrefixKeptNote is a plan note's reason where an upgrade takes the prefix an app
// keeps its files under from its own setting, as a person changed it, rather than the
// one its install chose (ADR 0099).
const PrefixKeptNote = "catalog_prefix_kept"

// prefixesKept is an install's pinned needs with each storage need's prefix where its
// app keeps its files now. A link from before links named the setting that tells
// their app the prefix wrote that setting as a value of its own, which a person may
// have changed since: that value is where the files are, so the upgrade that makes
// the link name the setting takes it as the link's prefix, and the engine checks it
// against the other apps on the connection as any new prefix (#772's last check, C1).
// It changes nothing where the value is the link's prefix, or the link names the
// setting already, and says in a note where it does.
func (h *Handler) prefixesKept(ctx context.Context, env, project string, e *catalogv1.Entry, pinned map[string]*resourcev1.PinnedNeed) (map[string]*resourcev1.PinnedNeed, []revision.Note, error) {
	if h.db == nil || env == "" {
		return pinned, nil, nil
	}
	out := maps.Clone(pinned)
	var notes []revision.Note
	err := h.db.View(ctx, func(tx *store.Tx) error {
		for _, n := range e.GetNeeds() {
			p := pinned[n.GetName()]
			i := slices.IndexFunc(n.GetSets(), func(s *catalogv1.NeedSet) bool { return s.GetPart() == catalogv1.Part_PART_KEY_PREFIX })
			if p.GetConnection() == "" || i < 0 {
				continue
			}
			setting := n.GetSets()[i].GetName()
			r, err := tx.GetResource(ctx, string(variable.LinkKind), env, n.GetName())
			if err != nil {
				continue // no link to take it from
			}
			var l resourcev1.Link
			if proto.Unmarshal(r.Spec, &l) != nil || l.GetConnection() == "" || link.KeepsToPrefix(&l) {
				continue
			}
			r, err = tx.GetResource(ctx, string(variable.VariableKind), env, setting)
			if err != nil {
				continue
			}
			var v resourcev1.Variable
			if proto.Unmarshal(r.Spec, &v) != nil || v.GetFrom() != nil || v.GetValue() == l.GetPrefix() {
				continue
			}
			kept := proto.CloneOf(p)
			kept.Prefix = v.GetValue()
			out[n.GetName()] = kept
			notes = append(notes, revision.Note{Reason: PrefixKeptNote, Message: fmt.Sprintf("%s keeps its files under %s, as its %s says, and its link %s keeps to that prefix from now on, in place of %s",
				project, cmp.Or(v.GetValue(), "the bucket's root"), setting, n.GetName(), cmp.Or(l.GetPrefix(), "the bucket's root"))})
		}
		return nil
	})
	return out, notes, err
}
