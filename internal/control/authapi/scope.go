// SPDX-License-Identifier: AGPL-3.0-only

package authapi

// Check 3 of docs/adr/0048-members-as-far-as-one-owner-goes.md: where each
// object a call names belongs, against what the caller holds there; and what a
// list or a stream returns, narrowed to the caller's places
// (docs/adr/0049-roles-and-invites-by-project.md).

import (
	"context"
	"errors"
	"regexp"
	"strings"

	"connectrpc.com/connect"
	"google.golang.org/protobuf/proto"
	"google.golang.org/protobuf/reflect/protoreflect"

	dockwellv1 "github.com/dockwell/dockwell/gen/dockwell/v1"
	"github.com/dockwell/dockwell/gen/dockwell/v1/dockwellv1connect"
	"github.com/dockwell/dockwell/internal/control/place"
	"github.com/dockwell/dockwell/internal/id"
	"github.com/dockwell/dockwell/internal/store"
)

// idLike is how an object's ID reads, its kind's prefix and a ULID, as against
// a name.
var idLike = regexp.MustCompile(`^[a-z]{3}_[0-9A-Za-z]{10,}$`)

// notObjects are fields named like IDs that name no object of a project.
var notObjects = map[string]bool{"actor_id": true, "ceremony_id": true, "session_id": true, "rp_id": true}

// idNames says what a request's id holds when it's a name, not an ID: a
// service's, in its project and environment, or a project's. Elsewhere an id
// that isn't an ID, such as a catalog entry's, names nothing of a project.
var idNames = map[string]string{
	dockwellv1connect.ServiceServiceListServiceRevisionsProcedure:  "service",
	dockwellv1connect.ServiceServiceDeployServiceProcedure:         "service",
	dockwellv1connect.ServiceServiceRollBackServiceProcedure:       "service",
	dockwellv1connect.ServiceServiceAddServicePortProcedure:        "service",
	dockwellv1connect.ChangeServiceRestartServiceProcedure:         "service",
	dockwellv1connect.LogServiceListLogSourcesProcedure:            "service",
	dockwellv1connect.LogServiceReadLogsProcedure:                  "service",
	dockwellv1connect.LogServiceWatchLogsProcedure:                 "service",
	dockwellv1connect.CatalogServiceUpgradeCatalogInstallProcedure: "project",
}

// aboutTheRequest are what a response holds about the one thing its request
// named, which checkScope checked there, or about servers, seen by name and
// state: a change's steps, a service's log chunks and its servers, a Compose
// file's variables, servers drift couldn't check. The filter leaves them as
// they are, where it would otherwise leave out what it can't place.
var aboutTheRequest = map[protoreflect.FullName]bool{
	"dockwell.v1.StepUpdate":      true,
	"dockwell.v1.LogChunk":        true,
	"dockwell.v1.ServerLogStatus": true,
	"dockwell.v1.ComposeVariable": true,
	"dockwell.v1.UncheckedServer": true,
}

// containerLogs are the calls that, given a server, read a container there
// Dockwell didn't make, which is the install's, and an admin's (ADR 0048).
var containerLogs = map[string]bool{
	dockwellv1connect.LogServiceListLogSourcesProcedure: true,
	dockwellv1connect.LogServiceReadLogsProcedure:       true,
	dockwellv1connect.LogServiceWatchLogsProcedure:      true,
}

// errNotHere is what a call naming something outside the caller's places gets:
// the same as for something that was never there, so neither it nor its
// project's name leaks.
var errNotHere = errors.New("there's nothing by that ID or name here, or not in a project you hold. Check it, or ask an admin")

// ReasonNotFound says what a call names isn't there, or isn't in the caller's places.
const ReasonNotFound = "not_found"

// refs are what a request names.
type refs struct {
	ids                 []string
	project, env, named string
	// server is a server it names by name or ID.
	server string
}

// requested reads what a request names, from its own fields.
func requested(procedure string, m protoreflect.Message) (refs, protoreflect.FieldDescriptor) {
	var out refs
	var projectField protoreflect.FieldDescriptor
	fields := m.Descriptor().Fields()
	for k := range fields.Len() {
		fd := fields.Get(k)
		if fd.Kind() != protoreflect.StringKind {
			continue
		}
		name := string(fd.Name())
		var vals []string
		if fd.IsList() {
			list := m.Get(fd).List()
			for j := range list.Len() {
				vals = append(vals, list.Get(j).String())
			}
		} else if v := m.Get(fd).String(); v != "" {
			vals = []string{v}
		}
		switch {
		case notObjects[name]:
		case name == "project":
			projectField = fd
			if len(vals) > 0 {
				out.project = vals[0]
			}
		case name == "environment":
			for _, v := range vals {
				if idLike.MatchString(v) {
					out.ids = append(out.ids, v)
				} else {
					out.env = v
				}
			}
		case name == "server":
			if len(vals) > 0 {
				out.server = vals[0]
			}
			for _, v := range vals {
				if idLike.MatchString(v) {
					out.ids = append(out.ids, v)
				}
			}
		case name == "id" || strings.HasSuffix(name, "_id") || strings.HasSuffix(name, "_ids") || name == "scope" || name == "target":
			for _, v := range vals {
				switch {
				case idLike.MatchString(v):
					out.ids = append(out.ids, v)
				case name == "id" && idNames[procedure] == "project":
					out.project = v
				case name == "id" && idNames[procedure] == "service":
					out.named = v
				}
			}
		}
	}
	return out, projectField
}

// projectsOf are the projects grants hold, one each.
func projectsOf(grants []store.Grant) []string {
	var out []string
	for _, g := range grants {
		if g.ProjectID != store.Install {
			out = append(out, g.ProjectID)
		}
	}
	return out
}

// checkScope checks where each object a request names belongs against what the
// caller holds there. Admins and the owner hold every place. Someone who holds
// projects alone and names a service or an environment by name without its
// project gets their one project filled in, or is asked which.
func (i *interceptor) checkScope(ctx context.Context, procedure string, msg any) error {
	r := roleRules[procedure]
	if r.reach != dockwellv1.Reach_REACH_PROJECT && r.reach != dockwellv1.Reach_REACH_NAMED {
		return nil
	}
	grants, ok := grantsFrom(ctx)
	if !ok {
		return nil
	}
	install := place.Holds(grants, place.Install)
	if store.RoleRank(install) >= store.RoleRank(store.RoleAdmin) {
		return nil
	}
	pm, ok := msg.(proto.Message)
	if !ok {
		return nil
	}
	m := pm.ProtoReflect()
	ref, projectField := requested(procedure, m)
	if install == "" && ref.project == "" && (ref.named != "" || ref.env != "") {
		held := projectsOf(grants)
		if len(held) != 1 || projectField == nil {
			return ReasonError(connect.CodeInvalidArgument, errors.New("say which project, as you hold more than one, or none by that name"), ReasonNotFound)
		}
		ref.project = held[0]
		m.Set(projectField, protoreflect.ValueOfString(held[0]))
	}
	var places []place.Place
	if containerLogs[procedure] && ref.server != "" {
		// A container Dockwell didn't make, by its server: the install's.
		places = append(places, place.Install)
		ref.named = ""
	}
	err := i.svc.View(ctx, func(tx *store.Tx) error {
		for _, objectID := range ref.ids {
			if strings.HasPrefix(objectID, id.Server+"_") {
				continue // a server is named by projects, and seen by name (ADR 0049)
			}
			got, err := place.Of(ctx, tx, objectID)
			if err != nil {
				return err
			}
			places = append(places, got...)
		}
		if ref.project == "" {
			return nil
		}
		projectID, err := place.Project(ctx, tx, ref.project)
		if err != nil {
			return err
		}
		env, found, err := place.Environment(ctx, tx, projectID, ref.env)
		switch {
		case err != nil && !errors.Is(err, store.ErrNotFound):
			return err
		case found:
			places = append(places, env)
		default:
			places = append(places, place.Place{Project: projectID})
		}
		return nil
	})
	switch {
	case errors.Is(err, store.ErrNotFound), errors.Is(err, place.ErrUnknown):
		return ReasonError(connect.CodeNotFound, errNotHere, ReasonNotFound)
	case err != nil:
		i.logger.ErrorContext(ctx, "can't say where what a call names belongs", "error", err)
		return ReasonError(connect.CodeUnavailable, errors.New("can't check what you may reach right now. Try again in a moment"), ReasonUnavailable)
	}
	for _, p := range places {
		held := place.Holds(grants, p)
		if held == "" {
			return ReasonError(connect.CodeNotFound, errNotHere, ReasonNotFound)
		}
		needs := roleOf(r.needs)
		if p.Protected {
			needs = roleOf(r.needsProtected)
		}
		if store.RoleRank(held) < store.RoleRank(needs) {
			p, _ := PrincipalFrom(ctx)
			return ReasonError(connect.CodePermissionDenied, roleRefusal(p, held, needs), ReasonNeedsRole)
		}
	}
	return nil
}

// narrows reports a caller whose lists and streams are narrowed to their places:
// someone who holds projects alone. A grant on the install covers every project.
func narrows(ctx context.Context, procedure string) ([]store.Grant, bool) {
	if roleRules[procedure].reach != dockwellv1.Reach_REACH_PROJECT {
		return nil, false
	}
	grants, ok := grantsFrom(ctx)
	if !ok || place.Holds(grants, place.Install) != "" {
		return nil, false
	}
	return grants, true
}

// filter narrows what a response holds to the caller's places: each element of
// its lists, and for a stream's message, the one it carries, which is dropped
// whole, as skip says. Something it can't place is left out.
func (i *interceptor) filter(ctx context.Context, procedure string, msg any) (skip bool, err error) {
	grants, ok := narrows(ctx, procedure)
	if !ok {
		return false, nil
	}
	pm, ok := msg.(proto.Message)
	if !ok {
		return false, nil
	}
	m := pm.ProtoReflect()
	seen := map[string][]place.Place{}
	err = i.svc.View(ctx, func(tx *store.Tx) error {
		visible := func(e protoreflect.Message) (bool, error) {
			if aboutTheRequest[e.Descriptor().FullName()] {
				return true, nil
			}
			ids := elementIDs(e)
			if len(ids) == 0 {
				return false, nil
			}
			for _, objectID := range ids {
				got, ok := seen[objectID]
				if !ok {
					var err error
					got, err = place.Of(ctx, tx, objectID)
					if errors.Is(err, store.ErrNotFound) || errors.Is(err, place.ErrUnknown) {
						got, err = nil, nil
					}
					if err != nil {
						return false, err
					}
					seen[objectID] = got
				}
				// A change shows only when every one of its items is in reach (ADR 0049).
				if len(got) == 0 {
					return false, nil
				}
				for _, p := range got {
					if place.Holds(grants, p) == "" {
						return false, nil
					}
				}
			}
			return true, nil
		}
		fields := m.Descriptor().Fields()
		for k := range fields.Len() {
			fd := fields.Get(k)
			if fd.Kind() != protoreflect.MessageKind || !m.Has(fd) {
				continue
			}
			if fd.IsList() {
				list := m.Mutable(fd).List()
				kept := 0
				for j := range list.Len() {
					ok, err := visible(list.Get(j).Message())
					if err != nil {
						return err
					}
					if ok {
						list.Set(kept, list.Get(j))
						kept++
					}
				}
				list.Truncate(kept)
				continue
			}
			if fd.IsMap() {
				continue
			}
			// A stream's message, which carries one thing.
			if strings.HasPrefix(procedure, "/dockwell.v1.") && strings.Contains(procedure, "/Watch") {
				ok, err := visible(m.Get(fd).Message())
				if err != nil {
					return err
				}
				if !ok {
					skip = true
				}
			}
		}
		return nil
	})
	return skip, err
}

// elementIDs are what an element of a list says it belongs to: its change, its
// own ID, its environment, its resource's scope, or what it's about.
func elementIDs(e protoreflect.Message) []string {
	get := func(m protoreflect.Message, name string) protoreflect.Value {
		fd := m.Descriptor().Fields().ByName(protoreflect.Name(name))
		if fd == nil || fd.IsList() || fd.IsMap() || !m.Has(fd) {
			return protoreflect.Value{}
		}
		return m.Get(fd)
	}
	str := func(m protoreflect.Message, name string) string {
		fd := m.Descriptor().Fields().ByName(protoreflect.Name(name))
		if fd == nil || fd.Kind() != protoreflect.StringKind || fd.IsList() {
			return ""
		}
		return m.Get(fd).String()
	}
	if c := get(e, "change"); c.IsValid() {
		if v := str(c.Message(), "id"); v != "" {
			return []string{v}
		}
	}
	if v := str(e, "id"); idLike.MatchString(v) && !strings.HasPrefix(v, id.Event+"_") {
		return []string{v}
	}
	if v := str(e, "environment_id"); v != "" {
		return []string{v}
	}
	if r := get(e, "resource"); r.IsValid() {
		if v := str(r.Message(), "scope"); idLike.MatchString(v) {
			return []string{v}
		}
	}
	if s := get(e, "subject"); s.IsValid() {
		if v := str(s.Message(), "id"); idLike.MatchString(v) {
			return []string{v}
		}
	}
	if v := str(e, "scope"); idLike.MatchString(v) {
		return []string{v}
	}
	return nil
}
