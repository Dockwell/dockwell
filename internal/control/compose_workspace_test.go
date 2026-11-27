// SPDX-License-Identifier: AGPL-3.0-only

package control

import (
	"slices"
	"strings"
	"testing"

	"connectrpc.com/connect"

	agentv1 "github.com/dockwell/dockwell/gen/dockwell/agent/v1"
	resourcev1 "github.com/dockwell/dockwell/gen/dockwell/resource/v1alpha1"
	v1 "github.com/dockwell/dockwell/gen/dockwell/v1"
	"github.com/dockwell/dockwell/gen/dockwell/v1/dockwellv1connect"
	"github.com/dockwell/dockwell/internal/docker"
	"github.com/dockwell/dockwell/internal/resource/container"
	"github.com/dockwell/dockwell/internal/store"
	"github.com/dockwell/dockwell/internal/store/storetest"
)

func TestComposeWorkspaceOnBothStores(t *testing.T) {
	t.Parallel()
	storetest.Each(t, func(t *testing.T, backend store.Backend) {
		f := startFull(t, backend, nil)
		b := signedIn(t, f)
		engine := f.addDockerServer(t)
		server := f.lastDocker.id.ServerID
		changes := changeClient(b, b.origin)
		compose := dockwellv1connect.NewComposeServiceClient(b, b.origin, connect.WithProtoJSON())
		inventory := dockwellv1connect.NewDockerServiceClient(b, b.origin, connect.WithProtoJSON())
		const secret = "workspace-private-value"
		const file = `name: workspace-fixture
services:
  web:
    image: ghcr.io/acme/shop:1.4
    environment:
      PRIVATE_TOKEN: ${PRIVATE_TOKEN}
  cache:
    image: ghcr.io/acme/worker:1.0
    volumes:
      - cache-data:/data
volumes:
  cache-data:
`
		importPlan, err := compose.ImportCompose(t.Context(), connect.NewRequest(&v1.ImportComposeRequest{
			Compose: file, Variables: map[string]string{"PRIVATE_TOKEN": secret}, Servers: []string{server},
		}))
		if err != nil {
			t.Fatal(err)
		}
		imported := importPlan.Msg.GetChange()
		applyConfirmed(t, changes, imported)
		env := ""
		for _, item := range imported.GetPlan().GetResources() {
			if item.GetResource().GetKind() == "service" {
				env = item.GetResource().GetScope()
				break
			}
		}
		engineProject := docker.Project(env)
		get := func() *v1.GetComposeWorkspaceResponse {
			t.Helper()
			got, err := compose.GetComposeWorkspace(t.Context(), connect.NewRequest(&v1.GetComposeWorkspaceRequest{EnvironmentId: env}))
			if err != nil {
				t.Fatal(err)
			}
			return got.Msg
		}
		base := get()
		yaml := base.GetWorkspace().GetYaml()
		const placeholder = "PRIVATE_TOKEN: ${PRIVATE_TOKEN}"
		const envBlock = "    environment:\n      " + placeholder + "\n"
		if strings.Contains(base.String(), secret) || strings.Contains(yaml, engineProject) ||
			strings.Contains(yaml, container.LabelSet) || !strings.HasPrefix(yaml, "name: workspace-fixture\n") ||
			!strings.Contains(yaml, placeholder) ||
			!slices.ContainsFunc(base.GetOmissions(), func(omission *v1.ComposeWorkspaceOmission) bool {
				return omission.GetName() == "web" && omission.GetPath() == "environment.PRIVATE_TOKEN" && omission.GetSealed()
			}) {
			t.Fatalf("workspace projection did not retain a sealed value by name")
		}
		plan := func(from *v1.GetComposeWorkspaceResponse, draft *string, removals ...*v1.ComposeWorkspaceRemoval) (*v1.PlanComposeWorkspaceResponse, error) {
			res, err := compose.PlanComposeWorkspace(t.Context(), connect.NewRequest(&v1.PlanComposeWorkspaceRequest{
				EnvironmentId: env, Snapshot: from.GetSnapshot(), Yaml: draft, ExplicitRemovals: removals,
			}))
			if err != nil {
				return nil, err
			}
			return res.Msg, nil
		}
		if _, err := plan(base, &yaml); reasonOf(err) != "compose_workspace_unchanged" {
			t.Fatalf("reviewing the same projection: %v", err)
		}
		wrongName := strings.Replace(yaml, "name: workspace-fixture", "name: second-stack", 1)
		if _, err := plan(base, &wrongName); connect.CodeOf(err) != connect.CodeInvalidArgument {
			t.Fatalf("renaming the stack through the editor: %v", err)
		}
		label := strings.Replace(yaml, "    image: ghcr.io/acme/shop:1.4",
			"    image: ghcr.io/acme/shop:1.4\n    labels:\n      "+container.LabelSet+": '[]'", 1)
		refusal, err := plan(base, &label)
		if err != nil || refusal.GetRefusal().GetLine() <= 0 || !strings.Contains(refusal.GetRefusal().GetMessage(), "reserved label") {
			t.Fatalf("inserting internal labels: %v, %v", refusal, err)
		}
		incompatible := strings.Replace(yaml, "image: ghcr.io/acme/shop:1.4", "image: ghcr.io/acme/shop:1.4\n    env_file: [an.env]", 1)
		refusal, err = plan(base, &incompatible)
		if err != nil || refusal.GetRefusal().GetLine() <= 0 || !strings.Contains(refusal.GetRefusal().GetMessage(), "env_file") {
			t.Fatalf("unsupported keyed line: %v, %v", refusal, err)
		}
		edit := strings.Replace(yaml, "ghcr.io/acme/shop:1.4", "ghcr.io/acme/shop:1.5", 1)
		planned, err := plan(base, &edit)
		if err != nil || planned.GetChange() == nil || planned.GetRefusal() != nil {
			t.Fatalf("edit plan: %v, %v", planned, err)
		}
		diffs := planned.GetChange().GetPlan().GetResources()
		if len(diffs) != 1 || diffs[0].GetResource().GetName() != "web" || strings.Contains(planned.String(), secret) {
			t.Fatalf("edit did not plan just web: %v", diffs)
		}
		applyConfirmed(t, changes, planned.GetChange())
		web := engine.Serving(engineProject + "-web-1")
		if web == nil || !slices.Contains(web.Config.Env, "PRIVATE_TOKEN="+secret) {
			t.Fatal("the new revision did not keep its environment value on the serving instance")
		}
		if _, err := plan(base, &edit); reasonOf(err) != "compose_workspace_stale" {
			t.Fatalf("reviewing a stale base: %v", err)
		}
		base = get()
		yaml = base.GetWorkspace().GetYaml()
		moved := strings.ReplaceAll(yaml, envBlock, "")
		moved = strings.Replace(moved, "    image: ghcr.io/acme/worker:1.0\n", "    image: ghcr.io/acme/worker:1.0\n"+envBlock, 1)
		if result, err := plan(base, &moved); connect.CodeOf(err) != connect.CodeInvalidArgument ||
			!strings.Contains(err.Error(), "kept value in a new location") || result != nil {
			t.Fatalf("a placeholder moved to another service: %v, %v", result, err)
		}

		const replacement = "workspace-replacement-value"
		update := strings.Replace(yaml, placeholder, "PRIVATE_TOKEN: "+replacement, 1)
		changed, err := plan(base, &update)
		if err != nil || changed.GetChange() == nil || !strings.Contains(changed.String(), "PRIVATE_TOKEN") ||
			strings.Contains(changed.String(), secret) || strings.Contains(changed.String(), replacement) {
			t.Fatalf("replace a sealed value with a redacted plan: %v, %v", changed, err)
		}
		applyConfirmed(t, changes, changed.GetChange())
		web = engine.Serving(engineProject + "-web-1")
		if web == nil || !slices.Contains(web.Config.Env, "PRIVATE_TOKEN="+replacement) {
			t.Fatal("the replacement did not reach the next container")
		}

		base = get()
		yaml = base.GetWorkspace().GetYaml()
		if !strings.Contains(yaml, placeholder) || strings.Contains(yaml, replacement) {
			t.Fatal("the replacement was not sealed")
		}
		remove := strings.ReplaceAll(yaml, envBlock, "")
		removed, err := plan(base, &remove)
		if err != nil || removed.GetChange() == nil || !strings.Contains(removed.String(), "PRIVATE_TOKEN") ||
			strings.Contains(removed.String(), replacement) {
			t.Fatalf("remove a sealed value with a redacted plan: %v, %v", removed, err)
		}
		applyConfirmed(t, changes, removed.GetChange())
		web = engine.Serving(engineProject + "-web-1")
		if web == nil || slices.ContainsFunc(web.Config.Env, func(entry string) bool { return strings.HasPrefix(entry, "PRIVATE_TOKEN=") }) {
			t.Fatal("the removed key survived in the next container")
		}

		managedRows, err := inventory.ListDockerObjects(t.Context(), connect.NewRequest(&v1.ListDockerObjectsRequest{
			ServerIds: []string{server}, Kind: v1.DockerObjectKind_DOCKER_OBJECT_KIND_COMPOSE,
		}))
		if err != nil || !slices.ContainsFunc(managedRows.Msg.GetObjects(), func(row *v1.DockerObject) bool {
			return row.GetCompose().GetEnvironmentId() == env && row.GetObservedAt() != nil
		}) {
			t.Fatalf("stack's last change was not listed: %v, %v", managedRows, err)
		}
		// Restarting the stack is one plan, with no revision or restart for
		// a service the operator has saved as stopped.
		stop := serviceEdit("web", "", nil)
		stop.Resource.Scope = env
		services := dockwellv1connect.NewServiceServiceClient(b, b.origin, connect.WithProtoJSON())
		currentServices, err := services.ListServices(t.Context(), connect.NewRequest(&v1.ListServicesRequest{Environment: env}))
		if err != nil {
			t.Fatal(err)
		}
		for _, service := range currentServices.Msg.GetServices() {
			if service.GetResource().GetName() == "web" {
				stop.BaseRevision = service.GetRevision()
			}
		}
		stop.GetSpec().GetService().GetApp().DesiredRunState = resourcev1.DesiredRunState_DESIRED_RUN_STATE_STOPPED
		applyConfirmed(t, changes, propose(t, changes, "Stop web", stop))
		restart, err := changes.RestartService(t.Context(), connect.NewRequest(&v1.RestartServiceRequest{Id: env}))
		if err != nil {
			t.Fatal(err)
		}
		steps := restart.Msg.GetChange().GetPlan().GetServers()[0].GetSteps()
		if len(steps) != 1 || steps[0].GetResource().GetName() != "cache" || steps[0].GetRestart() == "" {
			t.Fatalf("stack restart steps = %v", steps)
		}
		applyConfirmed(t, changes, restart.Msg.GetChange())
		stopped, err := engine.Client().InspectContainer(t.Context(), web.ID)
		if err != nil || stopped.State.Running {
			t.Fatal("restarting the stack started a service saved as stopped")
		}
		// A stack from elsewhere is grouped by its Compose labels. Its source is
		// a path hint, and one-off administrative jobs don't count as services.
		for _, instance := range []struct{ suffix, service, once string }{
			{"a", "web", "false"}, {"b", "web", "false"}, {"c", "job", "true"},
		} {
			engine.AddContainer(docker.Container{
				ID: strings.Repeat(instance.suffix, 64), Name: "/outside-" + instance.suffix,
				Config: docker.Config{Image: "alpine", Labels: map[string]string{
					docker.LabelProject: "outside-fixture", docker.LabelService: instance.service,
					docker.LabelOneoff: instance.once, docker.LabelWorkingDir: "/srv/compose-fixture",
					"test.unexpected.label": secret,
				}},
			})
		}
		key := &v1.DockerObjectKey{ServerId: server, EngineId: "outside:outside-fixture", Kind: v1.DockerObjectKind_DOCKER_OBJECT_KIND_COMPOSE}
		got, err := inventory.GetDockerObject(t.Context(), connect.NewRequest(&v1.GetDockerObjectRequest{Key: key}))
		if err != nil || got.Msg.GetObject().GetCompose().GetTotalServices() != 1 ||
			got.Msg.GetObject().GetObservedAt() == nil ||
			got.Msg.GetObject().GetCompose().GetSourceDirectory() != "/srv/compose-fixture" || strings.Contains(got.Msg.String(), secret) {
			t.Fatalf("outside stack membership or projection: %v, %v", got, err)
		}
		current := get()
		deletes, err := plan(current, nil,
			&v1.ComposeWorkspaceRemoval{Kind: "service", Name: "web"},
			&v1.ComposeWorkspaceRemoval{Kind: "service", Name: "cache"},
			&v1.ComposeWorkspaceRemoval{Kind: "network", Name: "default"},
		)
		if err != nil || deletes.GetChange() == nil {
			t.Fatalf("delete stack: %v, %v", deletes, err)
		}
		applyConfirmed(t, changes, deletes.GetChange())
		if engine.Volume(engineProject+"_cache-data") == nil {
			t.Fatal("deleting the stack removed its volume")
		}
	})
}

// A service with copies on two servers has one desired state, but its restart
// is an operation on the server in the workspace. New resources go there too.
func TestComposeWorkspaceServerScope(t *testing.T) {
	t.Parallel()
	storetest.Each(t, func(t *testing.T, backend store.Backend) {
		f := startFull(t, backend, nil)
		b := signedIn(t, f)
		local := f.addDockerServer(t)
		server := f.lastDocker.id.ServerID
		remote := f.addServerNamed(t, "second-fixture", nil, nil)
		other := f.lastDocker.id.ServerID
		changes := changeClient(b, b.origin)
		client := dockwellv1connect.NewComposeServiceClient(b, b.origin, connect.WithProtoJSON())

		imported, err := client.ImportCompose(t.Context(), connect.NewRequest(&v1.ImportComposeRequest{
			Compose: "name: placement-fixture\nservices:\n  web:\n    image: alpine:latest\n",
			Servers: []string{server, other},
		}))
		if err != nil {
			t.Fatal(err)
		}
		applyConfirmed(t, changes, imported.Msg.GetChange())
		var env string
		for _, item := range imported.Msg.GetChange().GetPlan().GetResources() {
			if item.GetResource().GetKind() == "service" {
				env = item.GetResource().GetScope()
			}
		}
		open := func() *v1.GetComposeWorkspaceResponse {
			t.Helper()
			got, err := client.GetComposeWorkspace(t.Context(), connect.NewRequest(&v1.GetComposeWorkspaceRequest{EnvironmentId: env}))
			if err != nil {
				t.Fatal(err)
			}
			return got.Msg
		}
		addWorker := func() string {
			t.Helper()
			doc := open().GetWorkspace().GetYaml()
			return strings.Replace(doc, "services:\n", "services:\n  worker:\n    image: alpine:latest\n", 1)
		}
		base := open()
		draft := addWorker()
		request := &v1.PlanComposeWorkspaceRequest{EnvironmentId: env, Snapshot: base.GetSnapshot(), Yaml: &draft}
		if _, err := client.PlanComposeWorkspace(t.Context(), connect.NewRequest(request)); connect.CodeOf(err) != connect.CodeInvalidArgument {
			t.Fatalf("ambiguous new placement: %v", err)
		}
		request.ServerId = server
		added, err := client.PlanComposeWorkspace(t.Context(), connect.NewRequest(request))
		if err != nil || added.Msg.GetChange() == nil {
			t.Fatalf("plan adding worker: %v, %v", added, err)
		}
		applyConfirmed(t, changes, added.Msg.GetChange())
		if row := open().GetWorkspace().GetResources(); !slices.ContainsFunc(row, func(r *v1.ComposeWorkspaceResource) bool {
			return r.GetName() == "worker" && slices.Equal(r.GetServerIds(), []string{server})
		}) {
			t.Fatalf("worker placement: %v", row)
		}
		request = &v1.PlanComposeWorkspaceRequest{
			EnvironmentId: env, Snapshot: open().GetSnapshot(), ServerId: other,
			ExplicitRemovals: []*v1.ComposeWorkspaceRemoval{{Kind: "service", Name: "worker"}},
		}
		if _, err := client.PlanComposeWorkspace(t.Context(), connect.NewRequest(request)); connect.CodeOf(err) != connect.CodeInvalidArgument {
			t.Fatalf("accepted a removal outside the viewed server: %v", err)
		}

		primaryName := docker.Project(env) + "-web-1"
		localBefore, remoteBefore := local.Serving(primaryName), remote.Serving(primaryName)
		if localBefore == nil || remoteBefore == nil {
			t.Fatal("the fixture did not deploy web on both servers")
		}
		restart, err := changes.RestartService(t.Context(), connect.NewRequest(&v1.RestartServiceRequest{Id: env, ServerId: server}))
		if err != nil {
			t.Fatal(err)
		}
		plans := restart.Msg.GetChange().GetPlan().GetServers()
		if len(plans) != 1 || plans[0].GetServerId() != server {
			t.Fatalf("restart should plan on just the viewed server: %v", plans)
		}
		applyConfirmed(t, changes, restart.Msg.GetChange())
		localAfter, remoteAfter := local.Serving(primaryName), remote.Serving(primaryName)
		if localAfter == nil || remoteAfter == nil || localAfter.State.StartedAt.Equal(localBefore.State.StartedAt) ||
			remoteAfter.ID != remoteBefore.ID || !remoteAfter.State.StartedAt.Equal(remoteBefore.State.StartedAt) {
			t.Fatal("restart did not stay on the viewed server")
		}
	})
}

func TestComposeInventoryCursorSurvivesHealthChanges(t *testing.T) {
	t.Parallel()
	f := startFull(t, store.SQLite, nil)
	b := signedIn(t, f)
	engine := f.addDockerServer(t)
	server := f.lastDocker.id.ServerID
	inventory := dockwellv1connect.NewDockerServiceClient(b, b.origin, connect.WithProtoJSON())
	for _, name := range []string{"first", "second"} {
		engine.AddContainer(docker.Container{
			Name:   "/" + name,
			Config: docker.Config{Labels: map[string]string{docker.LabelProject: name, docker.LabelService: "web"}},
		})
	}
	list := func(token string) *v1.ListDockerObjectsResponse {
		t.Helper()
		result, err := inventory.ListDockerObjects(t.Context(), connect.NewRequest(&v1.ListDockerObjectsRequest{
			ServerIds: []string{server}, Kind: v1.DockerObjectKind_DOCKER_OBJECT_KIND_COMPOSE, PageSize: 1, PageToken: token,
		}))
		if err != nil {
			t.Fatal(err)
		}
		return result.Msg
	}
	first := list("")
	if len(first.GetObjects()) != 1 || first.GetNextPageToken() == "" {
		t.Fatalf("first page: %v", first)
	}
	engine.SetHealth("second", "unhealthy", "fixture diagnostic")
	second := list(first.GetNextPageToken())
	if len(second.GetUnavailable()) > 0 || len(second.GetObjects()) != 1 ||
		second.GetObjects()[0].GetName() != "second" || second.GetObjects()[0].GetCompose().GetServicesNeedingAttention() != 1 {
		t.Fatalf("health change did not stay on the affected row: %v", second)
	}

	agent := f.lastDocker
	agent.stop()
	agent.hello = func(hello *agentv1.Hello) { hello.DockerCapabilities.ComposeMembers = false }
	agent.connect(t, f)
	older := list("")
	if len(older.GetObjects()) != 0 || len(older.GetUnavailable()) != 1 ||
		older.GetUnavailable()[0].GetState() != v1.DockerReadState_DOCKER_READ_STATE_AGENT_OUTDATED {
		t.Fatalf("an older agent was asked to scan Compose membership: %v", older)
	}
}

func TestComposeWorkspaceProtocolAllocations(t *testing.T) {
	t.Parallel()
	request := (&v1.PlanComposeWorkspaceRequest{}).ProtoReflect().Descriptor()
	if !request.ReservedRanges().Has(4) || !request.ReservedNames().Has("kept_values") ||
		request.Fields().ByName("server_id").Number() != 6 {
		t.Fatal("the dropped request field must stay reserved, and server_id uses tag 6")
	}
	if v1.DockerObjectKind_DOCKER_OBJECT_KIND_COMPOSE != 2 ||
		(&v1.DockerObject{}).ProtoReflect().Descriptor().Fields().ByName("compose").Number() != 11 ||
		(&v1.DockerContainer{}).ProtoReflect().Descriptor().Fields().ByName("compose_directory").Number() != 25 ||
		(&agentv1.DockerInventoryListCall{}).ProtoReflect().Descriptor().Fields().ByName("compose_members").Number() != 7 ||
		(&agentv1.DockerCapabilities{}).ProtoReflect().Descriptor().Fields().ByName("compose_members").Number() != 8 {
		t.Fatal("Compose inventory allocations differ from ADR 0107")
	}
}
