// SPDX-License-Identifier: AGPL-3.0-only

package control

import (
	"bytes"
	"errors"
	"slices"
	"strings"
	"testing"

	"connectrpc.com/connect"
	"google.golang.org/protobuf/encoding/protojson"
	"google.golang.org/protobuf/proto"

	resourcev1 "github.com/dockwell/dockwell/gen/dockwell/resource/v1alpha1"
	dockwellv1 "github.com/dockwell/dockwell/gen/dockwell/v1"
	"github.com/dockwell/dockwell/gen/dockwell/v1/dockwellv1connect"
	"github.com/dockwell/dockwell/internal/docker"
	"github.com/dockwell/dockwell/internal/docker/dockertest"
	"github.com/dockwell/dockwell/internal/resource/link"
	"github.com/dockwell/dockwell/internal/secrets"
	"github.com/dockwell/dockwell/internal/store"
	"github.com/dockwell/dockwell/internal/store/storetest"
)

func TestCustomConnectionMapsAndRotatesAcrossApps(t *testing.T) {
	t.Parallel()
	storetest.Each(t, func(t *testing.T, backend store.Backend) {
		f := startFull(t, backend, nil)
		b := signedIn(t, f)
		engine := f.addDockerServer(t)
		pullsAnything(engine)
		env := listedEnv(t, b)
		project := envProject(t, f, env)
		changes := changeClient(b, b.origin)
		conn := dockwellv1connect.NewConnectionServiceClient(b, b.origin, connect.WithProtoJSON())
		const (
			first = "local-test-secret-first"
			next  = "local-test-secret-next"
			name  = "example-api"
		)
		noSecret := func(message proto.Message) {
			t.Helper()
			raw, err := protojson.Marshal(message)
			if err != nil {
				t.Fatal(err)
			}
			if strings.Contains(string(raw), first) || strings.Contains(string(raw), next) {
				t.Errorf("%T exposes a Custom secret", message)
			}
		}
		for _, app := range []string{"reader", "worker"} {
			apply(t, changes, propose(t, changes, "Run "+app, &dockwellv1.Edit{
				Resource: &dockwellv1.ResourceRef{Name: app},
				Servers:  []string{f.lastDocker.id.ServerID},
				Spec: &resourcev1.Spec{Kind: &resourcev1.Spec_Service{Service: &resourcev1.Service{
					Runs: &resourcev1.Service_App{App: &resourcev1.Container{Image: "example.com/sample:1"}},
				}}},
			}))
		}

		custom := &resourcev1.ConnectionCustom{Initial: "EA", Fields: []*resourcev1.ConnectionCustomField{
			{Name: "api_key", Type: resourcev1.ConnectionCustomType_CONNECTION_CUSTOM_TYPE_SECRET, SecretValue: proto.String(first)},
			{Name: "endpoint", Type: resourcev1.ConnectionCustomType_CONNECTION_CUSTOM_TYPE_URL, Value: "https://api.example.com"},
			{Name: "optional", Type: resourcev1.ConnectionCustomType_CONNECTION_CUSTOM_TYPE_SECRET},
		}}
		create := &dockwellv1.CreateCustomConnectionRequest{
			Name: name, DisplayName: "Example API", Custom: custom, Shared: true,
			Reach: resourcev1.ConnectionReach_CONNECTION_REACH_PROJECTS, Projects: []string{project},
		}
		created, err := conn.CreateCustomConnection(t.Context(), connect.NewRequest(create))
		if err != nil {
			t.Fatal(err)
		}
		noSecret(created.Msg)
		saved := apply(t, changes, created.Msg.GetChange())
		noSecret(saved)
		requireCustomApplied(t, saved)
		if _, err := conn.CreateCustomConnection(t.Context(), connect.NewRequest(create)); connect.CodeOf(err) != connect.CodeInvalidArgument {
			t.Errorf("creating the same Custom connection twice: %v", err)
		}

		list := func() *dockwellv1.ConnectionInfo {
			t.Helper()
			r, err := conn.ListConnections(t.Context(), connect.NewRequest(&dockwellv1.ListConnectionsRequest{}))
			if err != nil {
				t.Fatal(err)
			}
			noSecret(r.Msg)
			if len(r.Msg.GetConnections()) != 1 {
				t.Fatalf("listed %d connections, want one", len(r.Msg.GetConnections()))
			}
			return r.Msg.GetConnections()[0]
		}
		got := list()
		fields := got.GetCustom().GetFields()
		if len(fields) != 3 || !fields[0].GetSet() || fields[0].GetValue() != "" ||
			fields[0].GetDefaultEnv() != "EXAMPLE_API_API_KEY" || fields[2].GetSet() {
			t.Fatalf("listing Custom fields: %v", fields)
		}
		org := orgID(t, f)
		if err := f.store.View(t.Context(), func(tx *store.Tx) error {
			r, err := tx.GetResource(t.Context(), "connection", org, name)
			if err != nil {
				return err
			}
			if !secrets.Sealed(r.Spec) || bytes.Contains(r.Spec, []byte(first)) {
				t.Error("the connection spec was not sealed")
			}
			return nil
		}); err != nil {
			t.Fatal(err)
		}

		linkApp := func(app string) *dockwellv1.Change {
			t.Helper()
			r, err := conn.ConnectCustom(t.Context(), connect.NewRequest(&dockwellv1.ConnectCustomRequest{
				Project: project, Environment: env, App: app, Connection: name, Fields: map[string]string{"api_key": "APP_TOKEN"},
			}))
			if err != nil {
				t.Fatal(err)
			}
			noSecret(r.Msg)
			return r.Msg.GetChange()
		}
		for _, app := range []string{"reader", "worker"} {
			change := linkApp(app)
			hasMapping := slices.ContainsFunc(change.GetPlan().GetResources(), func(r *dockwellv1.PlannedResource) bool {
				return slices.ContainsFunc(r.GetSets(), func(s *resourcev1.LinkSet) bool {
					return s.GetCustomEnv() == "APP_TOKEN" && s.GetCustomField() == "api_key" && s.GetCustomSecret()
				})
			})
			if !hasMapping {
				t.Error("the plan doesn't describe the secret's mapping")
			}
			requireCustomApplied(t, apply(t, changes, change))
		}
		assertCustomEnv(t, engine, env, "reader", first)
		assertCustomEnv(t, engine, env, "worker", first)
		info := list()
		if len(info.GetUses()) != 2 || info.GetUses()[0].GetCustomSets()["APP_TOKEN"] != "api_key" {
			t.Errorf("connection uses: %v", info.GetUses())
		}
		if _, err := conn.ConnectCustom(t.Context(), connect.NewRequest(&dockwellv1.ConnectCustomRequest{
			Project: project, Environment: env, App: "reader", Connection: name,
		})); err == nil {
			t.Error("a second connection link to the same app was planned")
		}

		var secretID string
		if err := f.store.View(t.Context(), func(tx *store.Tx) error {
			backing := link.CustomValue(link.CustomName("reader", name), "APP_TOKEN")
			r, err := tx.GetResource(t.Context(), "secret", env, backing)
			secretID = r.ID
			return err
		}); err != nil {
			t.Fatal(err)
		}
		variables := dockwellv1connect.NewVariableServiceClient(b, b.origin, connect.WithProtoJSON())
		if revealed, err := variables.RevealSecret(t.Context(), connect.NewRequest(&dockwellv1.RevealSecretRequest{Id: secretID})); connect.CodeOf(err) != connect.CodePermissionDenied || !strings.Contains(err.Error(), "Custom connection") {
			t.Errorf("revealing a Custom secret directly: reply %t, error %v", revealed != nil, err)
		}

		custom.Fields[0].SecretValue = proto.String(next)
		spec := &resourcev1.ConnectionSpec{
			DisplayName: create.GetDisplayName(), Kind: resourcev1.LinkKind_LINK_KIND_CUSTOM,
			Reach: create.GetReach(), Projects: create.GetProjects(), Shared: true, Custom: custom,
		}
		rotation := propose(t, changes, "Rotate the Custom API key", connectionEdit(name, spec))
		noSecret(rotation)
		apps := map[string]bool{}
		for _, planned := range rotation.GetPlan().GetResources() {
			if planned.GetResource().GetKind() == "service" {
				apps[planned.GetResource().GetName()] = true
			}
		}
		if len(apps) != 2 || !apps["reader"] || !apps["worker"] {
			t.Errorf("rotation plans apps %v", apps)
		}
		rotated := apply(t, changes, rotation)
		noSecret(rotated)
		requireCustomApplied(t, rotated)
		assertCustomEnv(t, engine, env, "reader", next)
		assertCustomEnv(t, engine, env, "worker", next)

		audit := dockwellv1connect.NewAuditServiceClient(b, b.origin, connect.WithProtoJSON())
		log, err := audit.ListAuditEvents(t.Context(), connect.NewRequest(&dockwellv1.ListAuditEventsRequest{PageSize: 100}))
		if err != nil {
			t.Fatal(err)
		}
		noSecret(log.Msg)
		if !slices.ContainsFunc(log.Msg.GetEvents(), func(e *dockwellv1.AuditEvent) bool {
			return e.GetAction() == "change.applied" && e.GetSubject() == rotation.GetId()
		}) {
			t.Error("the applied rotation is absent from the audit log")
		}

		disconnected, err := conn.DisconnectCustom(t.Context(), connect.NewRequest(&dockwellv1.DisconnectCustomRequest{
			Project: project, Environment: env, App: "reader", Connection: name,
		}))
		if err != nil {
			t.Fatal(err)
		}
		noSecret(disconnected.Msg)
		requireCustomApplied(t, apply(t, changes, disconnected.Msg.GetChange()))
		assertCustomEnv(t, engine, env, "reader", "")
		assertCustomEnv(t, engine, env, "worker", next)

		hold := aMember(t, f)
		hold(store.Grant{ProjectID: project, Role: store.RoleViewer})
		member := list()
		if member.GetRevision() != 0 || member.GetCustom().GetFields()[1].GetValue() != "" {
			t.Error("a viewer sees private connection values")
		}
		_, err = conn.ConnectCustom(t.Context(), connect.NewRequest(&dockwellv1.ConnectCustomRequest{
			Project: project, Environment: env, App: "reader", Connection: name,
		}))
		needsRole(t, "a viewer linking a Custom connection", err)
		hold(store.Grant{ProjectID: project, Role: store.RoleDeveloper})
		_, err = conn.CreateCustomConnection(t.Context(), connect.NewRequest(create))
		needsRole(t, "a developer creating a Custom connection", err)
	})
}

func requireCustomApplied(t *testing.T, change *dockwellv1.Change) {
	t.Helper()
	if change.GetState() != dockwellv1.ChangeState_CHANGE_STATE_APPLIED {
		t.Fatalf("%s ended %s: %s", change.GetSummary(), change.GetState(), change.GetError())
	}
}

func assertCustomEnv(t *testing.T, engine *dockertest.Engine, env, app, want string) {
	t.Helper()
	got := engine.Newest(docker.Project(env), app)
	if got == nil {
		t.Fatalf("%s's container is absent", app)
	}
	wantSetting := "APP_TOKEN=" + want
	has := slices.ContainsFunc(got.Config.Env, func(entry string) bool {
		if want == "" {
			return strings.HasPrefix(entry, "APP_TOKEN=")
		}
		return entry == wantSetting
	})
	if has != (want != "") {
		t.Errorf("%s's Custom environment does not match its mapping", app)
	}
}

func TestCustomConnectionMappingGuardsAndCleanup(t *testing.T) {
	t.Parallel()
	storetest.Each(t, func(t *testing.T, backend store.Backend) {
		s := newReachStand(t, backend)
		conn := dockwellv1connect.NewConnectionServiceClient(s.b, s.b.origin, connect.WithProtoJSON())
		const name = "example-api"
		spec := &resourcev1.ConnectionSpec{
			Kind: resourcev1.LinkKind_LINK_KIND_CUSTOM, Reach: resourcev1.ConnectionReach_CONNECTION_REACH_PROJECTS,
			Projects: []string{s.defaultID},
			Custom: &resourcev1.ConnectionCustom{Fields: []*resourcev1.ConnectionCustomField{
				{Name: "key", Type: resourcev1.ConnectionCustomType_CONNECTION_CUSTOM_TYPE_SECRET, SecretValue: proto.String("local-test-secret")},
				{Name: "region", Type: resourcev1.ConnectionCustomType_CONNECTION_CUSTOM_TYPE_TEXT, Value: "local"},
			}},
		}
		requireCustomApplied(t, apply(t, s.c, propose(t, s.c, "Save Custom connection", connectionEdit(name, spec))))
		linkApp := func(project, env, app string, fields map[string]string) (*dockwellv1.Change, error) {
			r, err := conn.ConnectCustom(t.Context(), connect.NewRequest(&dockwellv1.ConnectCustomRequest{
				Project: project, Environment: env, App: app, Connection: name, Fields: fields,
			}))
			if err != nil {
				return nil, err
			}
			return r.Msg.GetChange(), nil
		}
		invalid := func(err error, detail string) {
			t.Helper()
			if connect.CodeOf(err) != connect.CodeInvalidArgument || !strings.Contains(err.Error(), detail) {
				t.Errorf("want invalid argument containing %q, got %v", detail, err)
			}
		}
		_, err := linkApp(s.defaultID, s.production, "web", map[string]string{"key": "B"})
		invalid(err, "web already has B")
		_, err = linkApp(s.blogID, s.staging, "api", nil)
		notFound(t, "linking outside the connection's reach", err)
		mapped, err := linkApp(s.defaultID, s.production, "web", nil)
		if err != nil {
			t.Fatal(err)
		}
		requireCustomApplied(t, apply(t, s.c, mapped))

		l := link.CustomName("web", name)
		secret := link.CustomValue(l, "EXAMPLE_API_KEY")
		variable := link.CustomValue(l, "EXAMPLE_API_REGION")
		direct := &resourcev1.ValueFrom{Source: &resourcev1.ValueFrom_Connection{Connection: &resourcev1.ConnectionRef{
			Scope: orgID(t, s.f), Name: name, Link: l, CustomField: "key",
		}}}
		for _, test := range []struct {
			name, env, detail string
			kind              *resourcev1.Spec
		}{
			{
				"VARIABLE_ALIAS", s.production, "not a field mapping owned by link",
				&resourcev1.Spec{Kind: &resourcev1.Spec_Variable{Variable: &resourcev1.Variable{From: direct}}},
			},
			{
				"SECRET_ALIAS", s.production, "not a field mapping owned by link",
				&resourcev1.Spec{Kind: &resourcev1.Spec_Secret{Secret: &resourcev1.Secret{From: direct}}},
			},
			{
				"TEMPLATE_ALIAS", s.production, "belongs to the app's Custom mapping",
				&resourcev1.Spec{Kind: &resourcev1.Spec_Secret{Secret: &resourcev1.Secret{
					From: &resourcev1.ValueFrom{Source: &resourcev1.ValueFrom_Template{Template: &resourcev1.ValueTemplate{
						Text: "${" + secret + "}", Names: []*resourcev1.TemplateName{{Name: secret, Secret: true}},
					}}},
				}}},
			},
			{
				"CROSS_ENV_ALIAS", s.staging, "belongs to the app's Custom mapping",
				&resourcev1.Spec{Kind: &resourcev1.Spec_Secret{Secret: &resourcev1.Secret{
					From: &resourcev1.ValueFrom{Source: &resourcev1.ValueFrom_Value{Value: &resourcev1.ValueRef{
						Environment: s.production, Name: secret, Secret: true, Link: l,
					}}},
				}}},
			},
		} {
			_, err := s.c.ProposeChange(t.Context(), connect.NewRequest(&dockwellv1.ProposeChangeRequest{
				Summary: "Try an alias",
				Edits:   []*dockwellv1.Edit{{Resource: &dockwellv1.ResourceRef{Scope: test.env, Name: test.name}, Spec: test.kind}},
			}))
			invalid(err, test.detail)
		}

		bad := proto.CloneOf(spec)
		bad.Custom.Fields[0].Type = resourcev1.ConnectionCustomType_CONNECTION_CUSTOM_TYPE_TEXT
		bad.Custom.Fields[0].SecretValue = nil
		_, err = s.c.ProposeChange(t.Context(), connect.NewRequest(&dockwellv1.ProposeChangeRequest{
			Summary: "Change the kind of a mapped field", Edits: []*dockwellv1.Edit{connectionEdit(name, bad)},
		}))
		invalid(err, "before removing the field or changing its secrecy")

		spec.Reach = resourcev1.ConnectionReach_CONNECTION_REACH_EVERY
		spec.Projects = nil
		spec.Custom.Fields[0].SecretValue = nil
		spec.Custom.Fields = append(spec.Custom.Fields, &resourcev1.ConnectionCustomField{
			Name: "next", Type: resourcev1.ConnectionCustomType_CONNECTION_CUSTOM_TYPE_TEXT, Value: "new",
		})
		requireCustomApplied(t, apply(t, s.c, propose(t, s.c, "Expand reach and add a field", connectionEdit(name, spec))))
		_, err = linkApp(s.blogID, s.staging, "api", nil)
		invalid(err, "serves another app already")

		s.hold(in(s.blogID, store.RoleDeveloper))
		list, err := conn.ListConnections(t.Context(), connect.NewRequest(&dockwellv1.ListConnectionsRequest{Project: s.blogID}))
		if err != nil {
			t.Fatal(err)
		}
		got := list.Msg.GetConnections()
		if len(got) != 1 || !got[0].GetCustom().GetInUse() || len(got[0].GetUses()) != 0 {
			t.Fatalf("the other project's connection must report use without naming its app or link: %v", got)
		}
		s.hold(onInstall(store.RoleAdmin))
		list, err = conn.ListConnections(t.Context(), connect.NewRequest(&dockwellv1.ListConnectionsRequest{}))
		if err != nil {
			t.Fatal(err)
		}
		if uses := list.Msg.GetConnections()[0].GetUses(); len(uses) != 1 || len(uses[0].GetCustomSets()) != 2 {
			t.Errorf("the existing mapping changed when a field was added: %v", uses)
		}

		requireCustomApplied(t, apply(t, s.c, propose(t, s.c, "Remove the mapped app", &dockwellv1.Edit{
			Resource: &dockwellv1.ResourceRef{Kind: "service", Scope: s.production, Name: "web"}, Delete: true,
		})))
		if err := s.f.store.View(t.Context(), func(tx *store.Tx) error {
			for kind, name := range map[string]string{"link": l, "secret": secret, "variable": variable} {
				r, err := tx.GetResource(t.Context(), kind, s.production, name)
				if errors.Is(err, store.ErrNotFound) {
					continue
				}
				if err != nil {
					return err
				}
				if r.Spec != nil {
					t.Errorf("deleting the app left its Custom %s", kind)
				}
			}
			return nil
		}); err != nil {
			t.Fatal(err)
		}
	})
}
