// SPDX-License-Identifier: AGPL-3.0-only

package connections

import (
	"cmp"
	"context"
	"errors"
	"fmt"
	"maps"
	"slices"
	"strconv"
	"strings"

	"connectrpc.com/connect"
	"google.golang.org/protobuf/proto"

	resourcev1 "github.com/dockwell/dockwell/gen/dockwell/resource/v1alpha1"
	dockwellv1 "github.com/dockwell/dockwell/gen/dockwell/v1"
	"github.com/dockwell/dockwell/internal/control/agentapi"
	"github.com/dockwell/dockwell/internal/control/authapi"
	"github.com/dockwell/dockwell/internal/control/changes"
	"github.com/dockwell/dockwell/internal/control/place"
	"github.com/dockwell/dockwell/internal/probe"
	"github.com/dockwell/dockwell/internal/resource"
	"github.com/dockwell/dockwell/internal/resource/connection"
	"github.com/dockwell/dockwell/internal/resource/link"
	"github.com/dockwell/dockwell/internal/resource/variable"
	"github.com/dockwell/dockwell/internal/scope"
	"github.com/dockwell/dockwell/internal/store"
)

// Changes plans what a call changes, as a change with a plan.
type Changes interface {
	Propose(ctx context.Context, summary string, edits []*dockwellv1.Edit, field func(*scope.ChoiceError, string) string, also changes.Also) (*dockwellv1.Change, error)
}

// storageParts are the parts ConnectStorage sets: each a value read from the
// connection, but path style, which it writes in the app's words, and the prefix,
// which it writes as the link's. A link that sets the prefix's name keeps its app's
// files under it; one that sets none counts as at the bucket's root (link.Kept).
var storageParts = map[resourcev1.LinkPart]bool{
	resourcev1.LinkPart_LINK_PART_ENDPOINT: true, resourcev1.LinkPart_LINK_PART_REGION: true, resourcev1.LinkPart_LINK_PART_BUCKET: true,
	resourcev1.LinkPart_LINK_PART_ACCESS_KEY_ID: true, resourcev1.LinkPart_LINK_PART_SECRET_KEY: true, resourcev1.LinkPart_LINK_PART_PUBLIC_URL: true,
	resourcev1.LinkPart_LINK_PART_PATH_STYLE: true, resourcev1.LinkPart_LINK_PART_KEY_PREFIX: true,
}

// app is an app's environment and what its service reads, as ConnectStorage needs.
type app struct {
	project, env, envName, orgID string
	service, serviceID           string
	revision                     int
	// reads are the names of its environment the app's service reads, each with
	// whether it's a secret's.
	reads map[string]bool
	// has are the names its environment holds, each with whether it's a secret.
	has map[string]bool
}

// ConnectStorage checks a connection's bucket from an app's network on its server,
// then plans the link and the app's names reading it, in one change (ADR 0099).
func (s *Service) ConnectStorage(ctx context.Context, req *connect.Request[dockwellv1.ConnectStorageRequest]) (*connect.Response[dockwellv1.ConnectStorageResponse], error) {
	m := req.Msg
	if s.changes == nil || s.db == nil {
		return nil, connect.NewError(connect.CodeUnimplemented, errors.New("this control plane plans no changes"))
	}
	a, err := s.appOf(ctx, m.GetProject(), m.GetEnvironment(), m.GetApp())
	if err != nil {
		return nil, err
	}
	conn, backupUsed, ok, err := s.connections.ForLinking(ctx, m.GetConnection(), a.project)
	if err != nil {
		return nil, err
	}
	if !ok {
		// One the project may not use reads as not there, as it can't be named.
		return nil, authapi.ReasonError(connect.CodeNotFound, fmt.Errorf("there's no connection %s this project may use. Ask an admin to add one, or let this project use it", m.GetConnection()), "not_found")
	}
	if conn.Spec.GetKind() != resourcev1.LinkKind_LINK_KIND_S3 {
		return nil, authapi.Invalid(fmt.Errorf("the connection %s is a mail server, and Connect storage links S3 storage. Set up the app's mail on its page instead", conn.Name))
	}
	if backupUsed {
		return nil, authapi.Invalid(fmt.Errorf("the connection %s is used for backups, so no app can link to it: an app's key could delete the backups. Add a separate S3 connection for the app", conn.Name))
	}
	linkName := cmp.Or(m.GetLink(), "storage")
	if err := resource.CheckLabel("link", linkName, "storage"); err != nil {
		return nil, authapi.Invalid(err)
	}
	prefix := m.GetPrefix()
	if prefix == "" {
		// Under the app's name, or the first after it no other link to the connection
		// keeps to, as the engine refuses two that meet.
		uses, err := s.connections.Uses(ctx)
		if err != nil {
			return nil, err
		}
		var taken []string
		for _, u := range uses[conn.Name] {
			taken = append(taken, KeptPrefix(u))
		}
		prefix = link.FreePrefix(m.GetApp()+"/", taken)
	}
	if err := link.CheckPrefix(prefix); err != nil {
		return nil, authapi.Invalid(err)
	}
	if len(m.GetSets()) == 0 {
		return nil, authapi.Invalid(errors.New("say which of the app's names hold what, such as S3_BUCKET=bucket"))
	}
	for _, name := range slices.Sorted(maps.Keys(m.GetSets())) {
		part := m.GetSets()[name]
		_, read := a.reads[name]
		secret, has := a.has[name]
		_, key, ok := connection.Part(conn.Spec, part)
		switch {
		case !storageParts[part]:
			return nil, authapi.Invalid(fmt.Errorf("%s is set as %s, which storage doesn't have. Pick its endpoint, region, bucket, keys, public address, path style or prefix", name, part))
		case !read:
			return nil, authapi.Invalid(fmt.Errorf("%s doesn't read %s, so setting it changes nothing. Pick a name it reads", m.GetApp(), name))
		case ok && key && has && !secret:
			return nil, authapi.Invalid(fmt.Errorf("%s is a variable, and would hold a key, which plans show. Keep it as a secret first", name))
		}
	}

	// From where the app is, as it will reach the bucket, before anything's planned.
	check, err := s.checkFrom(ctx, a, conn.Spec, prefix, m.GetPresignedUrls())
	if err != nil {
		return nil, err
	}
	if !check.GetPassed() {
		return nil, authapi.ReasonError(connect.CodeFailedPrecondition, fmt.Errorf("%s, so nothing was planned. %s", checkWords(check), checkFix(check)), "check_failed")
	}

	spec := &resourcev1.Link{Label: "Storage", Connection: conn.Name, Kind: resourcev1.LinkKind_LINK_KIND_S3, Prefix: prefix}
	if m.GetPresignedUrls() {
		spec.Features = []resourcev1.LinkFeature{resourcev1.LinkFeature_LINK_FEATURE_PRESIGNED_URLS}
	}
	edits := []*dockwellv1.Edit{{
		Resource: &dockwellv1.ResourceRef{Kind: "link", Scope: a.env, Name: linkName},
		Spec:     &resourcev1.Spec{Kind: &resourcev1.Spec_Link{Link: spec}},
	}}
	for _, name := range slices.Sorted(maps.Keys(m.GetSets())) {
		part := m.GetSets()[name]
		spec.Sets = append(spec.Sets, &resourcev1.LinkSet{Name: name, Part: part})
		edits = append(edits, valueEdit(a, conn, linkName, name, part))
	}
	summary := fmt.Sprintf("Connect %s to %s", m.GetApp(), cmp.Or(conn.Spec.GetDisplayName(), conn.Name))
	change, err := s.changes.Propose(ctx, summary, edits, nil, changes.Also{})
	if err != nil {
		return nil, err
	}
	return connect.NewResponse(&dockwellv1.ConnectStorageResponse{Change: change, Check: check}), nil
}

// valueEdit is the edit of one name the link sets: a value read from the connection
// through the link, the prefix too, which the link owns, a secret for a key or for a
// name that's one already, or for path style, the app's word for it.
func valueEdit(a *app, conn Saved, linkName, name string, part resourcev1.LinkPart) *dockwellv1.Edit {
	if part == resourcev1.LinkPart_LINK_PART_PATH_STYLE {
		word := "false"
		if conn.Spec.GetS3().GetPathStyle() {
			word = "true"
		}
		return &dockwellv1.Edit{
			Resource: &dockwellv1.ResourceRef{Kind: string(variable.VariableKind), Scope: a.env, Name: name},
			Spec:     &resourcev1.Spec{Kind: &resourcev1.Spec_Variable{Variable: &resourcev1.Variable{Value: word}}},
		}
	}
	from := &resourcev1.ValueFrom{Source: &resourcev1.ValueFrom_Connection{Connection: &resourcev1.ConnectionRef{
		Scope: a.orgID, Name: conn.Name, Part: part, Link: linkName,
	}}}
	_, key, _ := connection.Part(conn.Spec, part)
	if secret, has := a.has[name]; key || has && secret {
		return &dockwellv1.Edit{
			Resource: &dockwellv1.ResourceRef{Kind: string(variable.SecretKind), Scope: a.env, Name: name},
			Spec:     &resourcev1.Spec{Kind: &resourcev1.Spec_Secret{Secret: &resourcev1.Secret{From: from}}},
		}
	}
	return &dockwellv1.Edit{
		Resource: &dockwellv1.ResourceRef{Kind: string(variable.VariableKind), Scope: a.env, Name: name},
		Spec:     &resourcev1.Spec{Kind: &resourcev1.Spec_Variable{Variable: &resourcev1.Variable{From: from}}},
	}
}

// appOf reads an app's environment, by its project and its own name or ID, which the
// caller holds a role in, and what its service reads: one they don't reads as not
// there.
func (s *Service) appOf(ctx context.Context, projectRef, envRef, name string) (*app, error) {
	out := &app{reads: map[string]bool{}, has: map[string]bool{}}
	err := s.db.View(ctx, func(tx *store.Tx) error {
		if err := envOf(ctx, tx, projectRef, envRef, out); err != nil {
			return err
		}
		r, err := tx.GetResource(ctx, "service", out.env, name)
		if errors.Is(err, store.ErrNotFound) || err == nil && r.Spec == nil {
			return authapi.ReasonError(connect.CodeNotFound, fmt.Errorf("there's no app %s in %s", name, out.envName), "not_found")
		}
		if err != nil {
			return err
		}
		out.service, out.serviceID, out.revision = name, r.ID, r.Revision
		spec, err := s.connections.keeper.Decode(ctx, tx, store.InRevision(r.ID, r.Kind, r.Revision), r.Spec)
		if err != nil {
			return err
		}
		svc, ok := spec.(*resourcev1.Service)
		if !ok {
			return fmt.Errorf("%s isn't an app", name)
		}
		for _, n := range svc.GetVariables() {
			out.reads[n] = false
		}
		for _, n := range svc.GetSecrets() {
			out.reads[n] = true
		}
		return hasIn(ctx, tx, out)
	})
	return out, err
}

// envOf finds an environment, by its project and its own name or ID, in a project
// the caller holds a role in: one they don't reads as not there.
func envOf(ctx context.Context, tx *store.Tx, projectRef, envRef string, out *app) error {
	tree, err := scope.Load(ctx, tx)
	if err != nil {
		return err
	}
	out.orgID = tree.Organization.ID
	caller, hasCaller := place.CallerFrom(ctx)
	var envs []scope.Environment
	for _, p := range tree.Projects {
		if projectRef != p.ID && projectRef != p.Name {
			continue
		}
		if hasCaller && place.Holds(caller.Grants, place.Place{Project: p.ID}) == "" {
			continue
		}
		out.project = p.ID
		for _, e := range p.Environments {
			if envRef == "" || envRef == e.ID || envRef == e.Name {
				envs = append(envs, e)
			}
		}
	}
	switch {
	case out.project == "":
		return authapi.ReasonError(connect.CodeNotFound, fmt.Errorf("there's no project %s", projectRef), "not_found")
	case len(envs) == 0:
		return authapi.Invalid(fmt.Errorf("the project has no environment %s", envRef))
	case len(envs) > 1:
		return authapi.Invalid(errors.New("the project has more than one environment. Say which the app is in"))
	}
	out.env, out.envName = envs[0].ID, envs[0].Name
	return nil
}

// hasIn reads the names an environment holds, each with whether it's a secret.
func hasIn(ctx context.Context, tx *store.Tx, out *app) error {
	for kind, secret := range map[string]bool{string(variable.VariableKind): false, string(variable.SecretKind): true} {
		rs, err := tx.ListResources(ctx, out.env, kind)
		if err != nil {
			return err
		}
		for i := range rs {
			if rs[i].Spec != nil {
				out.has[rs[i].Name] = secret
			}
		}
	}
	return nil
}

// DisconnectStorage plans letting go of an app's link to a connection, in one
// change: each name it sets keeps the value it reads now, as a plain variable or a
// secret, as it is one now, and the link goes (ADR 0099's Detach, as ADR 0084's).
// Path style is the app's own word already, so it stays as it is.
func (s *Service) DisconnectStorage(ctx context.Context, req *connect.Request[dockwellv1.DisconnectStorageRequest]) (*connect.Response[dockwellv1.DisconnectStorageResponse], error) {
	m := req.Msg
	if s.changes == nil || s.db == nil {
		return nil, connect.NewError(connect.CodeUnimplemented, errors.New("this control plane plans no changes"))
	}
	a := &app{has: map[string]bool{}}
	var l *resourcev1.Link
	// The names that read the connection through this link now, each with its kind:
	// one a person has set otherwise since is theirs, and stays as it is.
	reading := map[string]string{}
	// The environment's apps, by name, as one that reads a key that goes stops reading it.
	apps := map[string]*resourcev1.Service{}
	// The project, by name, and its spec, when it's a catalog install's, whose pin
	// says a connection meets the need the link is named for.
	var projectName string
	var project *resourcev1.Project
	var projectRevision int64
	err := s.db.View(ctx, func(tx *store.Tx) error {
		if err := envOf(ctx, tx, m.GetProject(), m.GetEnvironment(), a); err != nil {
			return err
		}
		r, err := tx.GetResource(ctx, string(link.Kind), a.env, m.GetLink())
		notThere := authapi.ReasonError(connect.CodeNotFound, fmt.Errorf("there's no link %s to a connection in %s. Its connection's page lists them", m.GetLink(), a.envName), "not_found")
		switch {
		case errors.Is(err, store.ErrNotFound), err == nil && r.Spec == nil:
			return notThere
		case err != nil:
			return err
		}
		spec, err := s.connections.keeper.Decode(ctx, tx, store.InRevision(r.ID, r.Kind, r.Revision), r.Spec)
		if err != nil {
			return fmt.Errorf("can't read the link %s: %w", m.GetLink(), err)
		}
		got, ok := spec.(*resourcev1.Link)
		if !ok || got.GetConnection() == "" {
			return notThere
		}
		l = got
		for _, set := range got.GetSets() {
			for _, kind := range []string{string(variable.SecretKind), string(variable.VariableKind)} {
				v, err := tx.GetResource(ctx, kind, a.env, set.GetName())
				if errors.Is(err, store.ErrNotFound) || err == nil && v.Spec == nil {
					continue
				}
				if err != nil {
					return err
				}
				spec, err := s.connections.keeper.Decode(ctx, tx, store.InRevision(v.ID, v.Kind, v.Revision), v.Spec)
				if err != nil {
					return fmt.Errorf("can't read %s: %w", set.GetName(), err)
				}
				var from *resourcev1.ValueFrom
				switch x := spec.(type) {
				case *resourcev1.Secret:
					from = x.GetFrom()
				case *resourcev1.Variable:
					from = x.GetFrom()
				}
				if c := from.GetConnection(); c != nil && c.GetName() == got.GetConnection() && c.GetLink() == m.GetLink() {
					reading[set.GetName()] = kind
				}
			}
		}
		pr, err := tx.GetResourceByID(ctx, a.project)
		if err != nil {
			return err
		}
		if pr.Spec != nil {
			spec, err := s.connections.keeper.Decode(ctx, tx, store.InRevision(pr.ID, pr.Kind, pr.Revision), pr.Spec)
			if err != nil {
				return fmt.Errorf("can't read the project %s: %w", pr.Name, err)
			}
			if p, ok := spec.(*resourcev1.Project); ok && p.GetCatalog().GetNeeds()[m.GetLink()].GetConnection() == got.GetConnection() {
				projectName, project, projectRevision = pr.Name, p, int64(pr.Revision)
			}
		}
		services, err := tx.ListResources(ctx, a.env, serviceKind)
		if err != nil {
			return err
		}
		for i := range services {
			r := &services[i]
			if r.Spec == nil {
				continue
			}
			spec, err := s.connections.keeper.Decode(ctx, tx, store.InRevision(r.ID, r.Kind, r.Revision), r.Spec)
			if err != nil {
				return fmt.Errorf("can't read the app %s: %w", r.Name, err)
			}
			if svc, ok := spec.(*resourcev1.Service); ok {
				apps[r.Name] = svc
			}
		}
		return nil
	})
	if err != nil {
		return nil, err
	}
	// Storage a catalog install's connection gave it is the install's, which an admin
	// or the owner changes: below one, it's refused here, in short words, rather than
	// in the engine's about projects (slice 1 re-check, N4; the slice 3 review, L5).
	if admin, _ := callerProjects(ctx); project != nil && !admin {
		return nil, authapi.ReasonError(connect.CodePermissionDenied, fmt.Errorf("%s's storage comes from its catalog install, so an admin or the owner disconnects it, and nothing was planned. Ask one of them", projectName), "needs_role")
	}
	all, err := s.connections.All(ctx)
	if err != nil {
		return nil, err
	}
	i := slices.IndexFunc(all, func(c Saved) bool { return c.Name == l.GetConnection() })
	if i < 0 {
		return nil, fmt.Errorf("the link %s names the connection %s, which isn't there", m.GetLink(), l.GetConnection())
	}
	conn := all[i]
	// A connection's keys are the install's: only an install admin may read them, so
	// only theirs keeps them, as secrets of the environment. Below one, the names that
	// hold a key go, and the app runs without them, as its plan says.
	admin, _ := callerProjects(ctx)
	edits := []*dockwellv1.Edit{{Resource: &dockwellv1.ResourceRef{Kind: string(link.Kind), Scope: a.env, Name: m.GetLink()}, Delete: true}}
	var lost []string
	sets := slices.Clone(l.GetSets())
	slices.SortFunc(sets, func(x, y *resourcev1.LinkSet) int { return strings.Compare(x.GetName(), y.GetName()) })
	for _, set := range sets {
		kind, reads := reading[set.GetName()]
		if !reads {
			continue
		}
		value, key, ok := connection.Part(conn.Spec, set.GetPart())
		if set.GetPart() == resourcev1.LinkPart_LINK_PART_KEY_PREFIX {
			// The prefix is the link's, which the app keeps as it was.
			value, key, ok = l.GetPrefix(), false, true
		}
		if !ok {
			return nil, fmt.Errorf("the link %s sets %s as %s, which the connection doesn't have", m.GetLink(), set.GetName(), set.GetPart())
		}
		ref := &dockwellv1.ResourceRef{Kind: kind, Scope: a.env, Name: set.GetName()}
		switch {
		case key && !admin:
			edits = append(edits, &dockwellv1.Edit{Resource: ref, Delete: true})
			lost = append(lost, set.GetName())
		case key || kind == string(variable.SecretKind):
			// A key is a secret whatever the name was, so no plan shows it.
			ref.Kind = string(variable.SecretKind)
			edits = append(edits, &dockwellv1.Edit{Resource: ref, Spec: &resourcev1.Spec{Kind: &resourcev1.Spec_Secret{Secret: &resourcev1.Secret{Value: value}}}})
		default:
			edits = append(edits, &dockwellv1.Edit{Resource: ref, Spec: &resourcev1.Spec{Kind: &resourcev1.Spec_Variable{Variable: &resourcev1.Variable{Value: value}}}})
		}
	}
	// Each app that reads a name that goes stops reading it, in the same change, as
	// nothing may read a value that's gone.
	for _, name := range slices.Sorted(maps.Keys(apps)) {
		svc := apps[name]
		drop := func(m map[string]string) bool {
			was := len(m)
			maps.DeleteFunc(m, func(_, from string) bool { return slices.Contains(lost, from) })
			return len(m) != was
		}
		svc = proto.CloneOf(svc)
		svc.Revisions = nil
		if dv, ds := drop(svc.GetVariables()), drop(svc.GetSecrets()); dv || ds {
			edits = append(edits, &dockwellv1.Edit{
				Resource: &dockwellv1.ResourceRef{Kind: serviceKind, Scope: a.env, Name: name},
				Spec:     &resourcev1.Spec{Kind: &resourcev1.Spec_Service{Service: svc}},
			})
		}
	}
	// A catalog install's pin says its need keeps the app's own values from now on,
	// so no upgrade, an automatic one least of all, links it to the connection again,
	// and the keys that went stay out of its environment (the slice 1 review, M1).
	if project != nil {
		project = proto.CloneOf(project)
		pinned := project.GetCatalog().GetNeeds()[m.GetLink()]
		pinned.Connection, pinned.Own, pinned.LeftOut = "", true, slices.Concat(pinned.GetLeftOut(), lost)
		edits = append(edits, &dockwellv1.Edit{
			Resource: &dockwellv1.ResourceRef{Kind: string(scope.ProjectKind), Scope: a.orgID, Name: projectName},
			Spec:     &resourcev1.Spec{Kind: &resourcev1.Spec_Project{Project: project}},
			// An upgrade landing between this read and the plan isn't undone (N5).
			BaseRevision: projectRevision,
		})
	}
	summary := fmt.Sprintf("Disconnect %s from %s, keeping its values", m.GetLink(), cmp.Or(conn.Spec.GetDisplayName(), conn.Name))
	if len(lost) > 0 {
		summary = fmt.Sprintf("Disconnect %s from %s, keeping its values but its keys: %s go, and the app runs without them",
			m.GetLink(), cmp.Or(conn.Spec.GetDisplayName(), conn.Name), strings.Join(lost, " and "))
	}
	change, err := s.changes.Propose(ctx, summary, edits, nil, changes.Also{})
	if err != nil {
		return nil, err
	}
	return connect.NewResponse(&dockwellv1.DisconnectStorageResponse{Change: change}), nil
}

// checkFrom checks a connection's bucket from the app's network, on the server the
// app runs on.
func (s *Service) checkFrom(ctx context.Context, a *app, c *resourcev1.ConnectionSpec, prefix string, presign bool) (*dockwellv1.CheckConnectionResponse, error) {
	var placed []string
	if err := s.db.View(ctx, func(tx *store.Tx) error {
		var err error
		placed, err = tx.Placement(ctx, a.serviceID, a.revision)
		return err
	}); err != nil {
		return nil, err
	}
	if len(placed) == 0 {
		return nil, authapi.ReasonError(connect.CodeFailedPrecondition, errors.New("the app runs on no server yet, so its storage can't be checked from where it is. Deploy it first"), "not_deployed")
	}
	session, srv, err := s.session(ctx, placed[0])
	if err != nil {
		return nil, err
	}
	got, err := session.ProbeConnection(ctx, a.env, a.service, probeOf(c, prefix, presign))
	switch {
	case errors.Is(err, agentapi.ErrOlderAgent):
		return nil, connect.NewError(connect.CodeFailedPrecondition, fmt.Errorf("%s can't check a connection: %w", srv.GetName(), err))
	case err != nil:
		var se *resource.ServerError
		if errors.As(err, &se) {
			err = se.Err
		}
		return nil, authapi.ReasonError(connect.CodeUnavailable, fmt.Errorf("%s can't check the app's storage now: %w", srv.GetName(), err), authapi.ReasonUnavailable)
	}
	return &dockwellv1.CheckConnectionResponse{
		Passed: got.GetPassed(), Step: got.GetStep(), Code: got.GetCode(), Status: got.GetStatus(),
		Message: got.GetMessage(), Words: got.GetWords(), Region: got.GetRegion(), Server: srv.GetName(),
	}, nil
}

// checkWords says what a failed check found, as a fact.
func checkWords(c *dockwellv1.CheckConnectionResponse) string {
	return checkWordsFrom(c, "the app's server")
}

// checkWordsFrom is checkWords for a check from who, such as "the control server",
// where that isn't the app's server.
func checkWordsFrom(c *dockwellv1.CheckConnectionResponse, who string) string {
	Who := strings.ToUpper(who[:1]) + who[1:]
	if mailStep(c.GetStep()) {
		return mailWords(c, who, Who)
	}
	switch c.GetCode() {
	case probe.CodeRefused:
		return Who + " sends nothing to that address: " + cmp.Or(c.GetWords(), "it's one Dockwell keeps the app from")
	case probe.CodeTimeout:
		return "The bucket didn't answer from " + who + " in time"
	case probe.CodeTLS:
		return "The bucket's address has a certificate " + who + " doesn't trust"
	case probe.CodeUnreachable:
		return Who + " can't reach the bucket: " + cmp.Or(c.GetWords(), "nothing answered")
	case probe.CodeMismatch:
		return "The bucket gave back something other than what the check wrote"
	}
	what := "The bucket refused the check at its " + cmp.Or(c.GetStep(), "first") + " step, with " + cmp.Or(c.GetCode(), strconv.Itoa(int(c.GetStatus())))
	if m := strings.TrimSpace(c.GetMessage()); m != "" {
		what += ": " + strings.TrimSuffix(m, ".")
	}
	return what
}

// checkFix says what to do about a failed check, in a line.
func checkFix(c *dockwellv1.CheckConnectionResponse) string {
	return checkFixFrom(c, "the app's server")
}

// checkFixFrom is checkFix for a check from who.
func checkFixFrom(c *dockwellv1.CheckConnectionResponse, who string) string {
	if mailStep(c.GetStep()) {
		return mailFix(c, who)
	}
	switch c.GetCode() {
	case "SignatureDoesNotMatch", "InvalidAccessKeyId", "AccessDenied":
		return "Check the connection's keys, and that they may write in its bucket"
	case "NoSuchBucket":
		return "Check the connection's bucket"
	case probe.CodeRefused, probe.CodeUnreachable, probe.CodeTimeout:
		return "Check the connection's endpoint, and that " + who + " can reach it"
	case probe.CodeTLS:
		return "Check the connection's endpoint"
	}
	return "Check the connection on Connections"
}

// mailStep says a check stopped at one of a mail server's steps.
func mailStep(step string) bool {
	switch step {
	case probe.StepConnect, probe.StepHello, probe.StepTLS, probe.StepSignIn:
		return true
	}
	return false
}

// mailWords is checkWordsFrom for a mail server: what stopped it, by its reply code
// alone, never the server's words, which may say the user back.
func mailWords(c *dockwellv1.CheckConnectionResponse, who, subject string) string {
	switch {
	case c.GetCode() == probe.CodeTimeout && c.GetStep() == probe.StepHello:
		// It took the connection and said nothing, as a server that wants TLS from
		// the start does to a plain one (the SMTP review's L3).
		return "The mail server took the connection from " + who + " and answered nothing, so apps can't send mail through it as set"
	case c.GetCode() == probe.CodeTimeout && c.GetStep() == probe.StepTLS:
		return "The mail server took the connection from " + who + " and never finished TLS, so apps can't send mail through it as set"
	}
	switch c.GetCode() {
	case probe.CodeRefused:
		return subject + " sends nothing to that address: " + cmp.Or(c.GetWords(), "it's one Dockwell keeps the app from")
	case probe.CodeTimeout:
		return "The mail server didn't answer from " + who + " in time"
	case probe.CodeUnreachable:
		return subject + " can't reach the mail server: " + cmp.Or(c.GetWords(), "nothing answered")
	case probe.CodeTLS:
		return "The mail server has a certificate " + who + " doesn't trust"
	case probe.CodeNoStartTLS:
		return "The mail server doesn't offer STARTTLS"
	case probe.CodeClear:
		return "The connection signs in without TLS to an address that isn't private, so Dockwell sent no password"
	case probe.CodeNoSignIn:
		return "The mail server signs no one in"
	case probe.CodeMechanisms:
		return "The mail server signs in only in ways Dockwell doesn't, which are PLAIN and LOGIN"
	}
	switch c.GetStep() {
	case probe.StepSignIn:
		return "The mail server refused the user and password, with " + cmp.Or(c.GetCode(), "no code")
	case probe.StepTLS:
		return "The mail server refused STARTTLS, with " + cmp.Or(c.GetCode(), "no code")
	}
	return "The mail server refused the check at its " + c.GetStep() + " step, with " + cmp.Or(c.GetCode(), "no code")
}

// mailFix is checkFixFrom for a mail server.
func mailFix(c *dockwellv1.CheckConnectionResponse, who string) string {
	switch {
	case c.GetCode() == probe.CodeTimeout && c.GetStep() == probe.StepHello:
		return "If its port wants TLS from the start, as 465 does, choose TLS"
	case c.GetCode() == probe.CodeTimeout && c.GetStep() == probe.StepTLS:
		return "If its port wants a plain start, as 587 does, choose STARTTLS"
	case c.GetCode() == probe.CodeTimeout || c.GetCode() == probe.CodeUnreachable:
		return "Check the connection's host and port, and that " + who + " can reach it. Many providers take port 2525 where 587 is blocked"
	case c.GetCode() == probe.CodeRefused, c.GetCode() == probe.CodeTLS:
		return "Check the connection's host"
	case c.GetCode() == probe.CodeNoStartTLS:
		return "Use TLS on port 465, or a server that offers STARTTLS"
	case c.GetCode() == probe.CodeClear:
		return "Use STARTTLS or TLS"
	case c.GetCode() == probe.CodeNoSignIn:
		return "Clear the connection's user, or use a server that signs one in"
	case c.GetStep() == probe.StepSignIn:
		return "Check the connection's user and password"
	}
	return "Check the connection on Connections"
}
