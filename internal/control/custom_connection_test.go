// SPDX-License-Identifier: AGPL-3.0-only

package control

import (
	"bytes"
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
