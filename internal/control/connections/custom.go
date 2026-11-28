// SPDX-License-Identifier: AGPL-3.0-only

package connections

import (
	"cmp"
	"context"
	"errors"
	"fmt"

	"connectrpc.com/connect"
	"google.golang.org/protobuf/proto"

	resourcev1 "github.com/dockwell/dockwell/gen/dockwell/resource/v1alpha1"
	dockwellv1 "github.com/dockwell/dockwell/gen/dockwell/v1"
	"github.com/dockwell/dockwell/internal/control/authapi"
	"github.com/dockwell/dockwell/internal/control/changes"
	"github.com/dockwell/dockwell/internal/resource"
	"github.com/dockwell/dockwell/internal/resource/connection"
	"github.com/dockwell/dockwell/internal/resource/link"
	"github.com/dockwell/dockwell/internal/resource/variable"
	"github.com/dockwell/dockwell/internal/store"
)

// CreateCustomConnection plans creation with an unused connection name.
func (s *Service) CreateCustomConnection(ctx context.Context, req *connect.Request[dockwellv1.CreateCustomConnectionRequest]) (*connect.Response[dockwellv1.CreateCustomConnectionResponse], error) {
	m := req.Msg
	ref := resource.Ref{Kind: connection.Kind, Name: m.GetName()}
	if err := s.db.View(ctx, func(tx *store.Tx) error {
		org, err := tx.Organization(ctx)
		ref.Scope = org.ID
		return err
	}); err != nil {
		return nil, err
	}
	spec := &resourcev1.ConnectionSpec{
		DisplayName: m.GetDisplayName(), Kind: resourcev1.LinkKind_LINK_KIND_CUSTOM,
		Custom: m.GetCustom(), Reach: m.GetReach(), Projects: m.GetProjects(), Shared: m.GetShared(),
	}
	edit := &dockwellv1.Edit{
		Resource: resourceRef(ref), Spec: &resourcev1.Spec{Kind: &resourcev1.Spec_Connection{Connection: spec}},
	}
	summary := "Add " + cmp.Or(m.GetDisplayName(), m.GetName())
	change, err := s.changes.Propose(ctx, summary, []*dockwellv1.Edit{edit}, nil, changes.Also{MustCreate: map[resource.Ref]bool{ref: true}})
	if err != nil {
		return nil, err
	}
	return connect.NewResponse(&dockwellv1.CreateCustomConnectionResponse{Change: change}), nil
}

// ConnectCustom plans an app-specific mapping and the app that reads it.
func (s *Service) ConnectCustom(ctx context.Context, req *connect.Request[dockwellv1.ConnectCustomRequest]) (*connect.Response[dockwellv1.ConnectCustomResponse], error) {
	m := req.Msg
	a, err := s.appOf(ctx, m.GetProject(), m.GetEnvironment(), m.GetApp())
	if err != nil {
		return nil, err
	}
	conn, _, _, ok, err := s.connections.ForLinking(ctx, m.GetConnection(), a.project)
	if err != nil {
		return nil, err
	}
	if !ok || conn.Spec.GetCustom() == nil {
		return nil, authapi.ReasonError(connect.CodeNotFound, errors.New("this project has no Custom connection by that name. Pick one the project may use"), "not_found")
	}
	for name := range m.GetFields() {
		if connection.CustomField(conn.Spec, name) == nil {
			return nil, authapi.Invalid(errors.New("a mapped field has changed. Choose the connection again to load its current fields"))
		}
	}

	name := link.CustomName(a.service, conn.Name)
	label := cmp.Or(conn.Spec.GetDisplayName(), conn.Name)
	l := &resourcev1.Link{Label: label, Kind: resourcev1.LinkKind_LINK_KIND_CUSTOM, CustomApp: a.service, Connection: conn.Name}
	ref := resource.Ref{Kind: link.Kind, Scope: a.env, Name: name}
	creates := map[resource.Ref]bool{ref: true}
	edits := []*dockwellv1.Edit{{Resource: resourceRef(ref), Spec: &resourcev1.Spec{Kind: &resourcev1.Spec_Link{Link: l}}}}
	app := proto.CloneOf(a.spec)
	for _, field := range conn.Spec.GetCustom().GetFields() {
		env, customized := m.GetFields()[field.GetName()]
		if !customized {
			env = connection.DefaultEnv(conn.Name, field.GetName())
		}
		if err := variable.CheckName("environment variable", env); err != nil {
			return nil, authapi.Invalid(err)
		}
		if _, has := app.GetApp().GetEnv()[env]; has || usesEnv(app, env) {
			return nil, authapi.Invalid(fmt.Errorf("%s already has %s. Give the field another environment name", a.service, env))
		}
		backing := link.CustomValue(name, env)
		secret := connection.CustomSecret(field)
		l.Sets = append(l.Sets, &resourcev1.LinkSet{Name: backing, CustomEnv: env, CustomField: field.GetName(), CustomSecret: secret})
		from := &resourcev1.ValueFrom{Source: &resourcev1.ValueFrom_Connection{Connection: &resourcev1.ConnectionRef{
			Scope: a.orgID, Name: conn.Name, Link: name, CustomField: field.GetName(),
		}}}
		kind := variable.VariableKind
		spec := &resourcev1.Spec{Kind: &resourcev1.Spec_Variable{Variable: &resourcev1.Variable{From: from}}}
		if secret {
			if app.Secrets == nil {
				app.Secrets = map[string]string{}
			}
			app.Secrets[env] = backing
			kind, spec = variable.SecretKind, &resourcev1.Spec{Kind: &resourcev1.Spec_Secret{Secret: &resourcev1.Secret{From: from}}}
		} else {
			if app.Variables == nil {
				app.Variables = map[string]string{}
			}
			app.Variables[env] = backing
		}
		value := resource.Ref{Kind: kind, Scope: a.env, Name: backing}
		creates[value] = true
		edits = append(edits, &dockwellv1.Edit{Resource: resourceRef(value), Spec: spec})
	}
	edits = append(edits, customAppEdit(a, app))
	change, err := s.changes.Propose(ctx, fmt.Sprintf("Connect %s to %s", a.service, label), edits, nil, changes.Also{MustCreate: creates})
	if err != nil {
		return nil, err
	}
	return connect.NewResponse(&dockwellv1.ConnectCustomResponse{Change: change}), nil
}

// DisconnectCustom plans removal of a mapping and its app-specific values.
func (s *Service) DisconnectCustom(ctx context.Context, req *connect.Request[dockwellv1.DisconnectCustomRequest]) (*connect.Response[dockwellv1.DisconnectCustomResponse], error) {
	m := req.Msg
	a, err := s.appOf(ctx, m.GetProject(), m.GetEnvironment(), m.GetApp())
	if err != nil {
		return nil, err
	}
	name := link.CustomName(a.service, m.GetConnection())
	l := &resourcev1.Link{}
	var base int
	if err := s.db.View(ctx, func(tx *store.Tx) error {
		r, err := tx.GetResource(ctx, string(link.Kind), a.env, name)
		if errors.Is(err, store.ErrNotFound) || err == nil && r.Spec == nil {
			return authapi.ReasonError(connect.CodeNotFound, errors.New("the app has no link to that Custom connection"), "not_found")
		}
		if err != nil {
			return err
		}
		base = r.Revision
		return proto.Unmarshal(r.Spec, l)
	}); err != nil {
		return nil, err
	}
	if l.GetKind() != resourcev1.LinkKind_LINK_KIND_CUSTOM || l.GetCustomApp() != a.service || l.GetConnection() != m.GetConnection() {
		return nil, authapi.Invalid(errors.New("the link belongs to a different connection. Refresh the app's Variables"))
	}
	app := proto.CloneOf(a.spec)
	edits := []*dockwellv1.Edit{{
		Resource: resourceRef(resource.Ref{Kind: link.Kind, Scope: a.env, Name: name}), Delete: true, BaseRevision: int64(base),
	}}
	for _, set := range l.GetSets() {
		kind := variable.VariableKind
		if set.GetCustomSecret() {
			kind = variable.SecretKind
		}
		delete(app.Variables, set.GetCustomEnv())
		delete(app.Secrets, set.GetCustomEnv())
		edits = append(edits, &dockwellv1.Edit{
			Resource: resourceRef(resource.Ref{Kind: kind, Scope: a.env, Name: set.GetName()}), Delete: true,
		})
	}
	edits = append(edits, customAppEdit(a, app))
	change, err := s.changes.Propose(ctx, fmt.Sprintf("Disconnect %s from %s", a.service, l.GetLabel()), edits, nil, changes.Also{})
	if err != nil {
		return nil, err
	}
	return connect.NewResponse(&dockwellv1.DisconnectCustomResponse{Change: change}), nil
}

func customAppEdit(a *app, spec *resourcev1.Service) *dockwellv1.Edit {
	return &dockwellv1.Edit{
		Resource: resourceRef(resource.Ref{Kind: "service", Scope: a.env, Name: a.service}), BaseRevision: int64(a.revision),
		Spec: &resourcev1.Spec{Kind: &resourcev1.Spec_Service{Service: spec}},
	}
}

func usesEnv(app *resourcev1.Service, env string) bool {
	_, v := app.GetVariables()[env]
	_, s := app.GetSecrets()[env]
	return v || s
}

func resourceRef(r resource.Ref) *dockwellv1.ResourceRef {
	return &dockwellv1.ResourceRef{Kind: string(r.Kind), Scope: r.Scope, Name: r.Name}
}

func customInfo(c Saved, admin bool) *dockwellv1.ConnectionCustomInfo {
	spec := c.Spec.GetCustom()
	if spec == nil {
		return nil
	}
	info := &dockwellv1.ConnectionCustomInfo{Initial: spec.GetInitial(), HasCheck: spec.GetCheck() != nil}
	if admin {
		info.Check = spec.GetCheck()
	}
	for _, f := range spec.GetFields() {
		field := &dockwellv1.ConnectionCustomFieldInfo{
			Name: f.GetName(), Type: f.GetType(), DefaultEnv: connection.DefaultEnv(c.Name, f.GetName()),
		}
		if connection.CustomSecret(f) {
			field.Set = f.GetSecretValue() != ""
		} else if admin {
			field.Value = f.GetValue()
		}
		info.Fields = append(info.Fields, field)
	}
	return info
}

func (s *Service) toCheckCustom(ctx context.Context, req *dockwellv1.CheckConnectionRequest) (*resourcev1.ConnectionSpec, error) {
	spec := &resourcev1.ConnectionSpec{Kind: resourcev1.LinkKind_LINK_KIND_CUSTOM, Custom: &resourcev1.ConnectionCustom{Check: req.GetCustom()}}
	if req.GetName() != "" {
		saved, _, _, ok, err := s.connections.ForLinking(ctx, req.GetName(), "")
		if err != nil {
			return nil, err
		}
		if !ok || saved.Spec.GetCustom() == nil {
			return nil, authapi.ReasonError(connect.CodeNotFound, errors.New("there's no Custom connection by that name"), "not_found")
		}
		if req.GetCustom() == nil {
			spec.Custom.Check = saved.Spec.GetCustom().GetCheck()
			spec.SavedBelowAdmin = saved.Spec.GetSavedBelowAdmin()
		}
	}
	if spec.GetCustom().GetCheck() == nil {
		return nil, authapi.Invalid(errors.New("this Custom connection has no check. Add an HTTPS or TCP check first"))
	}
	if err := connection.CustomProbe(spec.GetCustom().GetCheck()).Validate(); err != nil {
		return nil, authapi.Invalid(err)
	}
	return spec, nil
}
