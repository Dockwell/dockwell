// SPDX-License-Identifier: AGPL-3.0-only

package authapi

// Check 3 of docs/adr/0048-members-as-far-as-one-owner-goes.md: where each
// object a call names belongs, against what the caller holds there; and what a
// list or a stream returns, narrowed to the caller's places
// (docs/adr/0049-roles-and-invites-by-project.md).

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"regexp"
	"slices"
	"strings"

	"connectrpc.com/connect"
	"google.golang.org/protobuf/proto"
	"google.golang.org/protobuf/reflect/protoreflect"

	dockwellv1 "github.com/dockwell/dockwell/gen/dockwell/v1"
	"github.com/dockwell/dockwell/gen/dockwell/v1/dockwellv1connect"
	"github.com/dockwell/dockwell/internal/control/place"
	"github.com/dockwell/dockwell/internal/human"
	"github.com/dockwell/dockwell/internal/id"
	"github.com/dockwell/dockwell/internal/store"
)

// idLike is how an object's ID reads, its kind's prefix and a ULID, as against
// a name.
var idLike = regexp.MustCompile(`^[a-z]{3}_[0-9A-Za-z]{10,}$`)

// notObjects are fields named like IDs that name no object of a project.
var notObjects = map[string]bool{"actor_id": true, "ceremony_id": true, "session_id": true, "rp_id": true}

// idNames says what a request's id holds when it's a name, not an ID: a
// service's or task's, in its project and environment, a project's, or a website's
// project by name or domain. Elsewhere an id that isn't an ID, such as a
// catalog entry's, names nothing of a project.
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
	dockwellv1connect.DatabaseServiceGetDatabaseProcedure:          "service",
	dockwellv1connect.WebsiteServiceUpdateWebsiteProcedure:         "website",
	dockwellv1connect.WebsiteServiceDeleteWebsiteProcedure:         "website",
	dockwellv1connect.TaskServiceGetTaskProcedure:                  "task",
	dockwellv1connect.TaskServiceListTaskRunsProcedure:             "task",
	dockwellv1connect.TaskServiceRunTaskProcedure:                  "task",
}

// serviceFields are the calls whose service field names a service to read, by
// its name or its ID. Elsewhere a service field names one to make.
var serviceFields = map[string]bool{
	dockwellv1connect.HealthServiceListHealthProcedure:     true,
	dockwellv1connect.ComposeServiceExportComposeProcedure: true,
	dockwellv1connect.DomainServiceListDomainsProcedure:    true,
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

// requestBound are the calls whose whole response is about the one thing their
// request named, which checkScope checked: a service's log sources, the
// services and exposed variables a Compose file makes where it goes, a
// service's revisions, each whole, though a revision's change may have reached
// another project too. The filter leaves them whole.
var requestBound = map[string]bool{
	dockwellv1connect.LogServiceListLogSourcesProcedure:           true,
	dockwellv1connect.LogServiceReadLogsProcedure:                 true,
	dockwellv1connect.LogServiceWatchLogsProcedure:                true,
	dockwellv1connect.ComposeServiceCheckComposeProcedure:         true,
	dockwellv1connect.ComposeServiceImportComposeProcedure:        true,
	dockwellv1connect.ServiceServiceListServiceRevisionsProcedure: true,
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

// reasonChoice says a request left out something there's more than one of,
// with a dockwellv1.Choice detail listing what to choose from.
const reasonChoice = "choice"

// refs are what a request names.
type refs struct {
	ids                 []string
	project, env, named string
	// website is a website's project, by ID, name or domain.
	website string
	// task is a task's ID or name, resolved before the handler runs.
	task string
	// server is a server it names by name or ID.
	server string
}

// requestFields are the request's own fields that say where it is.
type requestFields struct {
	project, environment protoreflect.FieldDescriptor
}

// requested reads what a request names, from its own fields.
func requested(procedure string, m protoreflect.Message) (refs, requestFields) {
	var out refs
	var at requestFields
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
			at.project = fd
			if len(vals) > 0 {
				out.project = vals[0]
			}
		case name == "environment":
			at.environment = fd
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
		case name == "service" && serviceFields[procedure]:
			for _, v := range vals {
				if idLike.MatchString(v) {
					out.ids = append(out.ids, v)
				} else {
					out.named = v
				}
			}
		case name == "id" || strings.HasSuffix(name, "_id") || strings.HasSuffix(name, "_ids") || name == "scope" || name == "target":
			for _, v := range vals {
				switch {
				case name == "id" && idNames[procedure] == "website":
					out.website = v
				case name == "id" && idNames[procedure] == "task":
					out.task = v
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
	return out, at
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

// anAdmin reports grants that make an admin or the owner, who hold every place.
func anAdmin(grants []store.Grant) bool {
	return store.RoleRank(place.Holds(grants, place.Install)) >= store.RoleRank(store.RoleAdmin)
}

// reaches reports a place in the caller's reach below an admin: a project's,
// which a grant on it or on the install covers. The install's own places are
// an admin's alone (ADR 0048).
func reaches(grants []store.Grant, p place.Place) bool {
	return p.Project != "" && place.Holds(grants, p) != ""
}

// errCantCheck is what a call gets when where it reaches can't be checked.
func (i *interceptor) errCantCheck(ctx context.Context, procedure string, err error) error {
	i.logger.ErrorContext(ctx, "can't say where what a call names belongs", slog.String("procedure", procedure), slog.Any("error", err))
	return ReasonError(connect.CodeUnavailable, errors.New("can't check what you may reach right now, so this didn't happen. Try again in a moment"), ReasonUnavailable)
}

// checkScope checks where each object a request names belongs against what the
// caller holds there. Admins and the owner hold every place. Below them, what
// belongs to no project is an admin's whatever the call needs elsewhere, and a
// request that names no project is made to mean one of the caller's (fillIn).
func (i *interceptor) checkScope(ctx context.Context, procedure string, msg any) error {
	r := roleRules[procedure]
	if r.reach != dockwellv1.Reach_REACH_PROJECT && r.reach != dockwellv1.Reach_REACH_NAMED {
		return nil
	}
	grants, ok := grantsFrom(ctx)
	if !ok {
		// The gate puts them there for every call that needs a role, which is every
		// call of a project's: without them, nothing here can say yes.
		return i.errCantCheck(ctx, procedure, errors.New("the caller's grants weren't read"))
	}
	if anAdmin(grants) {
		return nil
	}
	pm, ok := msg.(proto.Message)
	if !ok {
		return i.errCantCheck(ctx, procedure, fmt.Errorf("a request of type %T", msg))
	}
	m := pm.ProtoReflect()
	ref, at := requested(procedure, m)
	var places []place.Place
	byServer := containerLogs[procedure] && ref.server != ""
	if byServer {
		// A container Dockwell didn't make, by its server: the install's.
		places = append(places, place.Install)
		ref.named = ""
	}
	err := i.svc.View(ctx, func(tx *store.Tx) error {
		if ref.task != "" {
			// Resolve a name only in reachable projects, including when a project
			// was supplied. The handler then reads this exact task by ID. Its
			// server filter is checked against that task's approved placement.
			found, err := place.FindTask(ctx, tx, func(p place.Place) bool { return reaches(grants, p) },
				ref.task, m.Get(at.project).String(), m.Get(at.environment).String())
			if err != nil {
				return err
			}
			switch len(found) {
			case 0:
				return store.ErrNotFound
			case 1:
				places = append(places, found[0].Place)
				m.Set(m.Descriptor().Fields().ByName("id"), protoreflect.ValueOfString(found[0].ResourceID))
				return nil
			default:
				return namedChoice(found, "a task named "+ref.task)
			}
		}
		if ref.website != "" {
			p, err := place.Website(ctx, tx, ref.website)
			if err != nil {
				return err
			}
			places = append(places, p)
			// The handler uses this exact project after its alias has been checked.
			m.Set(m.Descriptor().Fields().ByName("id"), protoreflect.ValueOfString(p.Project))
		}
		if !byServer {
			if err := ownProject(ctx, tx, r, grants, &ref, m, at); err != nil {
				return err
			}
			if err := fillIn(ctx, tx, grants, &ref, m, at); err != nil {
				return err
			}
		}
		pl := place.NewPlacer(tx)
		for _, objectID := range ref.ids {
			if place.Named(objectID) {
				continue // servers and destinations are seen by name (ADR 0049)
			}
			got, err := pl.Of(ctx, objectID)
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
	var said *connect.Error
	switch {
	case errors.Is(err, store.ErrNotFound), errors.Is(err, place.ErrUnknown):
		return ReasonError(connect.CodeNotFound, errNotHere, ReasonNotFound)
	case errors.As(err, &said):
		return said
	case err != nil:
		return i.errCantCheck(ctx, procedure, err)
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
		if p.Project == "" {
			// What belongs to no project is an admin's, whatever the call needs
			// where projects are (ADR 0048).
			needs = store.RoleAdmin
		}
		if store.RoleRank(held) < store.RoleRank(needs) {
			who, _ := PrincipalFrom(ctx)
			return ReasonError(connect.CodePermissionDenied, roleRefusal(who, held, needs), ReasonNeedsRole)
		}
	}
	return nil
}

// ownProject makes a plan that names nothing, for someone who holds projects
// alone, one in their projects: their one project, or they're asked which of
// theirs. Otherwise it would go where the API puts a plan that says nothing,
// the default project or a new one, neither of which is theirs.
func ownProject(ctx context.Context, tx *store.Tx, r rule, grants []store.Grant, ref *refs, m protoreflect.Message, at requestFields) error {
	if r.effect == dockwellv1.Effect_EFFECT_READ || at.project == nil || ref.project != "" || len(ref.ids) > 0 ||
		ref.named != "" || ref.env != "" || place.Holds(grants, place.Install) != "" {
		return nil
	}
	held := projectsOf(grants)
	if len(held) == 1 {
		ref.project = held[0]
		m.Set(at.project, protoreflect.ValueOfString(held[0]))
		return nil
	}
	found, err := place.Find(ctx, tx, func(p place.Place) bool { return reaches(grants, p) }, "", "")
	if err != nil {
		return err
	}
	var names []string
	for _, f := range found {
		if !slices.Contains(names, f.ProjectName) {
			names = append(names, f.ProjectName)
		}
	}
	return choice(&dockwellv1.Choice{Level: "project", Field: "project", Options: names}, heldProjects(names, "this is for"))
}

// fillIn makes a request that names a service or an environment by name, but
// no project, mean one in the caller's places, so neither what the API finds
// nor what it says names another project. The name is found among the
// caller's places, as the API would find it, and the request is made to name
// where it is; found in several, the caller is asked which of theirs.
func fillIn(ctx context.Context, tx *store.Tx, grants []store.Grant, ref *refs, m protoreflect.Message, at requestFields) error {
	if ref.project != "" || at.project == nil || ref.named == "" && ref.env == "" {
		return nil
	}
	set := func(fd protoreflect.FieldDescriptor, v string) {
		if fd != nil {
			m.Set(fd, protoreflect.ValueOfString(v))
		}
	}
	found, err := place.Find(ctx, tx, func(p place.Place) bool { return reaches(grants, p) }, ref.named, ref.env)
	if err != nil {
		return err
	}
	switch len(found) {
	case 0:
		return store.ErrNotFound
	case 1:
		ref.project, ref.env = found[0].Project, found[0].Environment
		set(at.project, found[0].Project)
		set(at.environment, found[0].Environment)
		return nil
	}
	what := "an environment named " + ref.env
	if ref.named != "" {
		what = "a service named " + ref.named
	}
	return namedChoice(found, what)
}

// namedChoice asks which reachable project or environment holds a name found
// in several. Its options say nothing of projects outside the caller's reach.
func namedChoice(found []place.Found, what string) error {
	var projects, envs []string
	for _, f := range found {
		if !slices.Contains(projects, f.ProjectName) {
			projects = append(projects, f.ProjectName)
		}
		envs = append(envs, f.EnvironmentName)
	}
	if len(projects) > 1 {
		return choice(&dockwellv1.Choice{Level: "project", Field: "project", Options: projects},
			fmt.Errorf("%s is in more than one of your projects, %s, so say which one you mean", what, human.List(projects)))
	}
	return choice(&dockwellv1.Choice{Level: "environment", Field: "environment", Options: envs, Project: projects[0]},
		fmt.Errorf("%s is in more than one environment of %s, %s, so say which one you mean", what, projects[0], human.List(envs)))
}

// choice is a request's choice to make, with what to choose from in a detail.
func choice(detail *dockwellv1.Choice, err error) error {
	cerr := ReasonError(connect.CodeInvalidArgument, err, reasonChoice)
	if d, derr := connect.NewErrorDetail(detail); derr == nil {
		cerr.AddDetail(d)
	}
	return cerr
}

// theirProject answers a handler's question of which project for someone who
// holds one project alone, by setting the field it names to theirs, which is
// then checked as checkScope checks what a request names. A question of which
// comes from reading the request, before anything changes, so the call can
// be made again. It reports whether it answered.
func (i *interceptor) theirProject(ctx context.Context, procedure string, msg any, err error) (bool, error) {
	var cerr *connect.Error
	if !errors.As(err, &cerr) || cerr.Meta().Get(ReasonKey) != reasonChoice {
		return false, nil
	}
	grants, _ := grantsFrom(ctx)
	held := projectsOf(grants)
	if place.Holds(grants, place.Install) != "" || len(held) != 1 {
		return false, nil
	}
	pm, ok := msg.(proto.Message)
	if !ok {
		return false, nil
	}
	for _, d := range cerr.Details() {
		v, derr := d.Value()
		c, ok := v.(*dockwellv1.Choice)
		if derr != nil || !ok || c.GetLevel() != "project" {
			continue
		}
		if !setPath(pm.ProtoReflect(), c.GetField(), held[0]) {
			return false, nil
		}
		return true, i.checkScope(ctx, procedure, msg)
	}
	return false, nil
}

// setPath sets the string field a path such as "edits[0].resource.scope" names
// in m, and reports whether there was one.
func setPath(m protoreflect.Message, path, value string) bool {
	parts := strings.Split(path, ".")
	for k, part := range parts {
		name, index, listed := strings.Cut(part, "[")
		fd := m.Descriptor().Fields().ByName(protoreflect.Name(name))
		if fd == nil {
			return false
		}
		if k == len(parts)-1 {
			if listed || fd.Kind() != protoreflect.StringKind || fd.IsList() {
				return false
			}
			m.Set(fd, protoreflect.ValueOfString(value))
			return true
		}
		if fd.Kind() != protoreflect.MessageKind || fd.IsMap() || fd.IsList() != listed {
			return false
		}
		if !listed {
			m = m.Mutable(fd).Message()
			continue
		}
		var at int
		if _, err := fmt.Sscanf(strings.TrimSuffix(index, "]"), "%d", &at); err != nil {
			return false
		}
		list := m.Mutable(fd).List()
		if at < 0 || at >= list.Len() {
			return false
		}
		m = list.Get(at).Message()
	}
	return false
}

// narrowChoice keeps a handler's question of which project to the caller's
// projects, below an admin: its options would otherwise name every project.
func (i *interceptor) narrowChoice(ctx context.Context, procedure string, err error) error {
	var cerr *connect.Error
	if !errors.As(err, &cerr) || cerr.Meta().Get(ReasonKey) != reasonChoice {
		return err
	}
	grants, ok := grantsFrom(ctx)
	if ok && anAdmin(grants) {
		return err
	}
	field := "project"
	for _, d := range cerr.Details() {
		v, derr := d.Value()
		if c, ok := v.(*dockwellv1.Choice); derr == nil && ok {
			if c.GetLevel() != "project" {
				return err // an environment of a project they reach, or a server, seen by name
			}
			field = cmpOr(c.GetField(), field)
		}
	}
	var names []string
	verr := i.svc.View(ctx, func(tx *store.Tx) error {
		found, err := place.Find(ctx, tx, func(p place.Place) bool { return reaches(grants, p) }, "", "")
		for _, f := range found {
			if !slices.Contains(names, f.ProjectName) {
				names = append(names, f.ProjectName)
			}
		}
		return err
	})
	if verr != nil {
		return i.errCantCheck(ctx, procedure, verr)
	}
	if len(names) == 0 {
		return ReasonError(connect.CodeNotFound, errNotHere, ReasonNotFound)
	}
	return choice(&dockwellv1.Choice{Level: "project", Field: field, Options: names}, heldProjects(names, "you mean"))
}

// heldProjects asks which of the caller's projects a request means.
func heldProjects(names []string, what string) error {
	if len(names) == 1 {
		return fmt.Errorf("say which project %s: you hold %s", what, names[0])
	}
	return fmt.Errorf("you hold %d projects, %s, so say which one %s", len(names), human.List(names), what)
}

func cmpOr(a, b string) string {
	if a != "" {
		return a
	}
	return b
}

// narrows reports a caller whose lists and streams are narrowed to their
// places: anyone below an admin. A grant on the install covers every project's
// places, never the install's own (ADR 0049). Without the caller's grants, it
// narrows to nothing.
func narrows(ctx context.Context, procedure string) ([]store.Grant, bool) {
	if roleRules[procedure].reach != dockwellv1.Reach_REACH_PROJECT || requestBound[procedure] {
		return nil, false
	}
	grants, ok := grantsFrom(ctx)
	if ok && anAdmin(grants) {
		return nil, false
	}
	return grants, true
}

// filter narrows what a response holds to the caller's places: each element of
// its lists, the IDs a stream says were removed, and for a stream's message,
// the one it carries, which is dropped whole, as skip says. Something it can't
// place is left out. A stream's message the narrowing left with nothing isn't
// sent, as an empty one would say something happened elsewhere.
func (i *interceptor) filter(ctx context.Context, procedure string, msg any) (skip bool, err error) {
	grants, ok := narrows(ctx, procedure)
	if !ok {
		return false, nil
	}
	pm, ok := msg.(proto.Message)
	if !ok {
		return true, nil
	}
	m := pm.ProtoReflect()
	stream := strings.Contains(procedure, "/Watch")
	narrowed := false
	err = i.svc.View(ctx, func(tx *store.Tx) error {
		pl := place.NewPlacer(tx)
		seen := map[string]bool{}
		within := func(objectID string) (bool, error) {
			if ok, done := seen[objectID]; done {
				return ok, nil
			}
			got, err := pl.Of(ctx, objectID)
			if errors.Is(err, store.ErrNotFound) || errors.Is(err, place.ErrUnknown) {
				got, err = nil, nil
			}
			if err != nil {
				return false, err
			}
			// A change shows only when every one of its items is in reach (ADR 0049).
			ok := len(got) > 0
			for _, p := range got {
				ok = ok && reaches(grants, p)
			}
			seen[objectID] = ok
			return ok, nil
		}
		visible := func(e protoreflect.Message) (bool, error) {
			if aboutTheRequest[e.Descriptor().FullName()] {
				return true, nil
			}
			if e.Descriptor().FullName() == "dockwell.v1.Domain" {
				return narrowDomain(e, within, func(objectID string) ([]place.Place, error) { return pl.Of(ctx, objectID) })
			}
			ids := elementIDs(e)
			if len(ids) == 0 {
				return false, nil
			}
			for _, objectID := range ids {
				if ok, err := within(objectID); !ok || err != nil {
					return false, err
				}
			}
			return true, nil
		}
		fields := m.Descriptor().Fields()
		for k := range fields.Len() {
			fd := fields.Get(k)
			switch {
			case fd.IsMap() || !m.Has(fd):
			case fd.IsList() && fd.Kind() == protoreflect.StringKind && fd.Name() == "removed":
				// What a stream says left: each is placed, as what it sent was.
				list := m.Mutable(fd).List()
				kept := 0
				for j := range list.Len() {
					ok, err := within(list.Get(j).String())
					if err != nil {
						return err
					}
					if ok {
						list.Set(kept, list.Get(j))
						kept++
					}
				}
				narrowed = narrowed || kept < list.Len()
				list.Truncate(kept)
			case fd.Kind() != protoreflect.MessageKind:
			case fd.IsList():
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
				narrowed = narrowed || kept < list.Len()
				list.Truncate(kept)
			case stream:
				// A stream's message, which carries one thing.
				ok, err := visible(m.Get(fd).Message())
				if err != nil {
					return err
				}
				skip = skip || !ok
			}
		}
		return nil
	})
	if stream && narrowed && proto.Size(pm) == 0 {
		skip = true
	}
	return skip, err
}

// narrowDomain keeps a domain's routes in the caller's places, and the domain
// if any is left, with the environment of its first. The engine keeps a
// domain's routes in one environment, so this holds only if that changes.
func narrowDomain(d protoreflect.Message, within func(string) (bool, error), placeOf func(string) ([]place.Place, error)) (bool, error) {
	routes := d.Descriptor().Fields().ByName("routes")
	env := d.Descriptor().Fields().ByName("environment_id")
	if routes == nil || env == nil {
		return false, nil
	}
	list := d.Mutable(routes).List()
	kept := 0
	for j := range list.Len() {
		r := list.Get(j).Message()
		routeID := r.Get(r.Descriptor().Fields().ByName("id")).String()
		ok, err := within(routeID)
		if err != nil {
			return false, err
		}
		if !ok {
			continue
		}
		if kept == 0 {
			got, err := placeOf(routeID)
			if err != nil {
				return false, err
			}
			d.Set(env, protoreflect.ValueOfString(got[0].Environment))
		}
		list.Set(kept, list.Get(j))
		kept++
	}
	list.Truncate(kept)
	return kept > 0, nil
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
	if e.Descriptor().FullName() == "dockwell.v1.TaskRun" {
		return []string{str(e, "task_id")}
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

// openPage opens the page token a caller whose lists are narrowed sends back,
// sealed by sealPage. Their next page token says where the next page starts,
// which may be an object out of their reach, so it goes out sealed for them.
func (i *interceptor) openPage(ctx context.Context, procedure string, msg any) error {
	m, fd, token := pageToken(ctx, procedure, msg, "page_token")
	if token == "" {
		return nil
	}
	who, _ := PrincipalFrom(ctx)
	plain, err := i.svc.OpenPageToken(who.User.ID, token)
	if err != nil {
		return Invalid(err)
	}
	m.Set(fd, protoreflect.ValueOfString(plain))
	return nil
}

// sealPage seals a narrowed list's next page token for its caller.
func (i *interceptor) sealPage(ctx context.Context, procedure string, msg any) {
	m, fd, token := pageToken(ctx, procedure, msg, "next_page_token")
	if token == "" {
		return
	}
	who, _ := PrincipalFrom(ctx)
	m.Set(fd, protoreflect.ValueOfString(i.svc.SealPageToken(who.User.ID, token)))
}

// pageToken is msg's token in field, for a call narrowed to its caller's places.
func pageToken(ctx context.Context, procedure string, msg any, field protoreflect.Name) (protoreflect.Message, protoreflect.FieldDescriptor, string) {
	if _, narrowed := narrows(ctx, procedure); !narrowed {
		return nil, nil, ""
	}
	pm, ok := msg.(proto.Message)
	if !ok {
		return nil, nil, ""
	}
	m := pm.ProtoReflect()
	fd := m.Descriptor().Fields().ByName(field)
	if fd == nil || fd.Kind() != protoreflect.StringKind || fd.IsList() {
		return nil, nil, ""
	}
	return m, fd, m.Get(fd).String()
}

// InstallRole is the role the caller holds on the whole install, as the gate
// read it for this call: their own, or an API token's maker's. It's empty for
// none, and when the gate read nothing.
func InstallRole(ctx context.Context) string {
	grants, _ := grantsFrom(ctx)
	return place.Holds(grants, place.Install)
}
