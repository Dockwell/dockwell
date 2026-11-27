// SPDX-License-Identifier: AGPL-3.0-only

package control

import (
	"bytes"
	"context"
	"encoding/json"
	"log/slog"
	"strings"
	"testing"

	"connectrpc.com/connect"

	spec "github.com/dockwell/dockwell/gen/dockwell/resource/v1alpha1"
	v1 "github.com/dockwell/dockwell/gen/dockwell/v1"
	"github.com/dockwell/dockwell/gen/dockwell/v1/dockwellv1connect"
	"github.com/dockwell/dockwell/internal/docker"
	"github.com/dockwell/dockwell/internal/docker/dockertest"
	"github.com/dockwell/dockwell/internal/store"
)

func TestComposeTemplateRevisionAndSecretOnBothStores(t *testing.T) {
	t.Parallel()
	eachStoreParallel(t, func(t *testing.T, backend store.Backend) {
		logs := &syncBuffer{}
		f := startFullLogging(t, backend, nil, slog.New(slog.NewTextHandler(logs, nil)))
		b := signedIn(t, f)
		f.addDockerServer(t)
		f.addDockerServer(t)
		server := f.lastDocker.id.ServerID
		changes := changeClient(b, b.origin)
		compose := dockwellv1connect.NewComposeServiceClient(b, b.origin, connect.WithProtoJSON())
		resources := dockwellv1connect.NewResourceServiceClient(b, b.origin, connect.WithProtoJSON())
		projects, err := resources.ListResources(t.Context(), connect.NewRequest(&v1.ListResourcesRequest{Kind: "project"}))
		if err != nil || len(projects.Msg.GetResources()) != 1 {
			t.Fatalf("projects: %v, %v", projects, err)
		}
		project := projects.Msg.GetResources()[0].GetId()
		const secret = "template-value-must-not-escape"
		check := func(v any) {
			t.Helper()
			body, err := json.Marshal(v)
			if err != nil || bytes.Contains(body, []byte(secret)) {
				t.Fatal("a template response or stored row contains the secret", err)
			}
		}
		prepare := func(file string) *v1.PrepareComposeTemplateResponse {
			t.Helper()
			got, err := compose.PrepareComposeTemplate(t.Context(), connect.NewRequest(&v1.PrepareComposeTemplateRequest{
				ProjectId: project, Source: &v1.PrepareComposeTemplateRequest_ComposeYaml{ComposeYaml: file},
			}))
			if err != nil {
				t.Fatal(err)
			}
			check(got.Msg)
			return got.Msg
		}
		const file = "services:\n  web:\n    image: ghcr.io/acme/shop:1.4\n    environment:\n      PRIVATE_TOKEN: ${PRIVATE_TOKEN}\n"
		bad := prepare(strings.Replace(file, "${PRIVATE_TOKEN}", secret, 1))
		if bad.GetRefusal().GetLine() != 5 || bad.GetDraft() != nil {
			t.Fatalf("literal refusal: %v", bad)
		}
		draft := prepare(file)
		if len(draft.GetDraft().GetInputs()) != 1 {
			t.Fatalf("draft: %v", draft)
		}
		template := &v1.ResourceRef{Scope: project, Kind: "compose.template", Name: "shop"}
		save := func(typed *spec.ComposeTemplate, base int64, remove bool) *v1.Change {
			t.Helper()
			edit := &v1.Edit{Resource: template, BaseRevision: base, Delete: remove}
			if typed != nil {
				edit.Spec = &spec.Spec{Kind: &spec.Spec_ComposeTemplate{ComposeTemplate: typed}}
			}
			c := propose(t, changes, "Save a template", edit)
			check(c)
			applyConfirmed(t, changes, c)
			return c
		}
		newEdit := &v1.Edit{Resource: template, Spec: &spec.Spec{Kind: &spec.Spec_ComposeTemplate{ComposeTemplate: draft.GetDraft()}}}
		waiting := propose(t, changes, "Another review of the same new template", newEdit)
		first := save(draft.GetDraft(), 0, false)
		if _, err := changes.ApplyChange(t.Context(), connect.NewRequest(&v1.ApplyChangeRequest{
			Id: waiting.GetId(), Fingerprint: waiting.GetPlan().GetFingerprint(),
		})); err == nil {
			t.Fatal("a concurrent review applied over the first template")
		}
		templateID := first.GetPlan().GetResources()[0].GetId()
		save(prepare(strings.Replace(file, "1.4", "1.5", 1)).GetDraft(), 1, false)
		got, err := compose.GetComposeTemplate(t.Context(), connect.NewRequest(&v1.GetComposeTemplateRequest{Id: templateID, Revision: 1, HistoryPageSize: 10}))
		if err != nil || len(got.Msg.GetHistory()) != 2 || !strings.Contains(got.Msg.GetSpec().GetComposeYaml(), "1.4") {
			t.Fatalf("older template: %v, %v", got, err)
		}
		check(got.Msg)
		list, err := compose.ListComposeTemplates(t.Context(), connect.NewRequest(&v1.ListComposeTemplatesRequest{ProjectId: project, Query: "shop"}))
		if err != nil || len(list.Msg.GetTemplates()) != 1 || list.Msg.GetTemplates()[0].GetRevision() != 2 {
			t.Fatalf("library: %v, %v", list, err)
		}
		check(list.Msg)
		plan, err := compose.PlanFromComposeTemplate(t.Context(), connect.NewRequest(&v1.PlanFromComposeTemplateRequest{
			Id: templateID, Revision: 1, Destination: &v1.ComposeTemplateDestination{ServerId: server, EnvironmentName: "from-template"},
			Inputs: []*v1.ComposeTemplateValue{{Name: "PRIVATE_TOKEN", Choice: &v1.ComposeTemplateValue_Value{Value: secret}}},
		}))
		if err != nil || plan.Msg.GetChange() == nil {
			t.Fatalf("plan: %v, %v", plan, err)
		}
		check(plan.Msg)
		change := plan.Msg.GetChange()
		save(nil, 2, true)
		applyConfirmed(t, changes, change)
		var env string
		for _, pr := range change.GetPlan().GetResources() {
			if pr.GetResource().GetKind() == "environment" {
				env = pr.GetId()
			}
		}
		if env == "" {
			t.Fatal("new environment is absent from plan")
		}
		workspace, err := compose.GetComposeWorkspace(t.Context(), connect.NewRequest(&v1.GetComposeWorkspaceRequest{EnvironmentId: env}))
		if err != nil || !strings.Contains(workspace.Msg.GetWorkspace().GetYaml(), "1.4") {
			t.Fatalf("workspace: %v %v", workspace, err)
		}
		check(workspace.Msg)
		fromStack, err := compose.PrepareComposeTemplate(t.Context(), connect.NewRequest(&v1.PrepareComposeTemplateRequest{
			ProjectId: project, Source: &v1.PrepareComposeTemplateRequest_Stack{Stack: &v1.ComposeTemplateStackSource{EnvironmentId: env}},
		}))
		if err != nil || len(fromStack.Msg.GetDraft().GetInputs()) != 1 ||
			fromStack.Msg.GetDraft().GetInputs()[0].GetKind() != spec.ComposeTemplateInputKind_COMPOSE_TEMPLATE_INPUT_KIND_SECRET {
			t.Fatalf("draft from saved stack: %v %v", fromStack, err)
		}
		check(fromStack.Msg)
		if err := f.store.View(t.Context(), func(tx *store.Tx) error {
			for _, scope := range []string{project, env} {
				rows, err := tx.RevisionsIn(context.Background(), scope, "")
				if err != nil {
					return err
				}
				for _, row := range rows {
					check(row)
					if bytes.Contains(row.Spec, []byte(secret)) {
						t.Fatal("raw revision holds the secret")
					}
				}
			}
			row, err := tx.GetChange(t.Context(), change.GetId())
			if err == nil {
				check(row)
				for _, item := range row.Items {
					if bytes.Contains(item.Spec, []byte(secret)) {
						t.Fatal("raw change item holds the secret")
					}
				}
			}
			events, auditErr := tx.ListEvents(t.Context(), 100)
			if auditErr != nil {
				return auditErr
			}
			check(events)
			return err
		}); err != nil {
			t.Fatal(err)
		}
		if strings.Contains(logs.String(), secret) {
			t.Fatal("the template input reached the control-plane log")
		}
	})
}

func TestComposeTemplateRoles(t *testing.T) {
	t.Parallel()
	eachStoreParallel(t, func(t *testing.T, backend store.Backend) {
		s := newReachStand(t, backend)
		ctx := t.Context()
		client := dockwellv1connect.NewComposeServiceClient(s.b, s.b.origin, connect.WithProtoJSON())
		const yaml = "services:\n  web:\n    image: ghcr.io/acme/shop:1.4\n"
		ref := &v1.ResourceRef{Kind: "compose.template", Scope: s.blogID, Name: "shop"}
		proposeTemplate := func(base int64) (*connect.Response[v1.ProposeChangeResponse], error) {
			description := "A portable stack"
			if base > 0 {
				description = "An updated template"
			}
			return s.c.ProposeChange(ctx, connect.NewRequest(&v1.ProposeChangeRequest{Summary: "Save template", Edits: []*v1.Edit{{
				Resource: ref, BaseRevision: base,
				Spec: &spec.Spec{Kind: &spec.Spec_ComposeTemplate{ComposeTemplate: &spec.ComposeTemplate{ComposeYaml: yaml, Description: description}}},
			}}}))
		}
		s.hold(in(s.blogID, store.RoleDeveloper))
		created, err := proposeTemplate(0)
		if err != nil {
			t.Fatal(err)
		}
		applyConfirmed(t, s.c, created.Msg.GetChange())
		templateID := resourceIn(t, created.Msg.GetChange(), "compose.template")
		use := func() (*connect.Response[v1.PlanFromComposeTemplateResponse], error) {
			return client.PlanFromComposeTemplate(ctx, connect.NewRequest(&v1.PlanFromComposeTemplateRequest{
				Id: templateID, Revision: 1, Destination: &v1.ComposeTemplateDestination{ServerId: s.server, EnvironmentName: "from-template"},
			}))
		}
		for _, role := range []string{store.RoleViewer, store.RoleDeveloper, "outsider"} {
			t.Run(role, func(t *testing.T) {
				if role == "outsider" {
					s.hold(in(s.defaultID, store.RoleDeveloper))
				} else {
					s.hold(in(s.blogID, role))
				}
				list, listErr := client.ListComposeTemplates(ctx, connect.NewRequest(&v1.ListComposeTemplatesRequest{ProjectId: s.blogID}))
				get, getErr := client.GetComposeTemplate(ctx, connect.NewRequest(&v1.GetComposeTemplateRequest{Id: templateID}))
				history, historyErr := client.GetComposeTemplate(ctx, connect.NewRequest(&v1.GetComposeTemplateRequest{
					Id: templateID, Revision: 1, HistoryPageSize: 10,
				}))
				draft, prepErr := client.PrepareComposeTemplate(ctx, connect.NewRequest(&v1.PrepareComposeTemplateRequest{
					ProjectId: s.blogID, Source: &v1.PrepareComposeTemplateRequest_ComposeYaml{ComposeYaml: yaml},
				}))
				proposal, proposeErr := proposeTemplate(1)
				planned, planErr := use()
				if role == "outsider" {
					notFound(t, "list", listErr)
					notFound(t, "get", getErr)
					notFound(t, "history", historyErr)
					notFound(t, "prepare", prepErr)
					notFound(t, "propose", proposeErr)
					notFound(t, "use", planErr)
					return
				}
				if listErr != nil || len(list.Msg.GetTemplates()) != 1 || getErr != nil || get.Msg.GetSpec() == nil ||
					historyErr != nil || len(history.Msg.GetHistory()) != 1 {
					t.Fatalf("template reads: list=%v get=%v history=%v", listErr, getErr, historyErr)
				}
				if role == store.RoleViewer {
					needsRole(t, "prepare", prepErr)
					needsRole(t, "propose", proposeErr)
					needsRole(t, "use", planErr)
					return
				}
				if prepErr != nil || draft.Msg.GetDraft() == nil || proposeErr != nil || planErr != nil || planned.Msg.GetChange() == nil {
					t.Fatalf("template writes: prepare=%v propose=%v use=%v", prepErr, proposeErr, planErr)
				}
				applyConfirmed(t, s.c, planned.Msg.GetChange())
				applyConfirmed(t, s.c, proposal.Msg.GetChange())
				undone, err := s.c.UndoChange(ctx, connect.NewRequest(&v1.UndoChangeRequest{Id: proposal.Msg.GetChange().GetId()}))
				if err != nil {
					t.Fatal(err)
				}
				applyConfirmed(t, s.c, undone.Msg.GetChange())
			})
		}
	})
}

func TestTemplatePrepareDoesNotOpenTheSourceSpec(t *testing.T) {
	t.Parallel()
	eachStoreParallel(t, func(t *testing.T, backend store.Backend) {
		f := startFull(t, backend, nil)
		b := signedIn(t, f)
		f.addDockerServer(t)
		changes := changeClient(b, b.origin)
		client := dockwellv1connect.NewComposeServiceClient(b, b.origin, connect.WithProtoJSON())
		placed := apply(t, changes, propose(t, changes, "Place an app", runImage("ghcr.io/acme/shop:1.4")))
		env := placed.GetPlan().GetResources()[0].GetResource().GetScope()
		project := envProject(t, f, env)
		const value = "template-source-secret-must-stay-sealed"
		service := &spec.Service{
			Runs:    &spec.Service_App{App: &spec.Container{Image: "ghcr.io/acme/shop:1.4"}},
			Secrets: map[string]string{"DBPW": "DBPW", "OLD_PW": "DBPW"},
		}
		ref := &v1.ResourceRef{Kind: "service", Scope: env, Name: "web"}
		save := func(command bool) *v1.Change {
			t.Helper()
			if command {
				service.GetApp().Command = []string{"sh", "-c", "printf '%s' ${DBPW}"}
				service.GetApp().References = []string{"command"}
			}
			edits := []*v1.Edit{{Resource: ref, Spec: &spec.Spec{Kind: &spec.Spec_Service{Service: service}}}}
			if !command {
				edits = append(edits, &v1.Edit{
					Resource: &v1.ResourceRef{Kind: "secret", Scope: env, Name: "DBPW"},
					Spec:     &spec.Spec{Kind: &spec.Spec_Secret{Secret: &spec.Secret{Value: value}}},
				})
			}
			return apply(t, changes, propose(t, changes, "Save workspace", edits...))
		}
		prepare := func() *v1.PrepareComposeTemplateResponse {
			t.Helper()
			got, err := client.PrepareComposeTemplate(t.Context(), connect.NewRequest(&v1.PrepareComposeTemplateRequest{
				ProjectId: project, Source: &v1.PrepareComposeTemplateRequest_Stack{Stack: &v1.ComposeTemplateStackSource{EnvironmentId: env}},
			}))
			if err != nil {
				t.Fatal(err)
			}
			body, err := json.Marshal(got.Msg)
			if err != nil || bytes.Contains(body, []byte(value)) {
				t.Fatal("prepare exposed a stored value", err)
			}
			return got.Msg
		}
		save(false)
		first := prepare()
		if first.GetDraft() == nil || len(first.GetDraft().GetInputs()) != 1 ||
			first.GetDraft().GetInputs()[0].GetKind() != spec.ComposeTemplateInputKind_COMPOSE_TEMPLATE_INPUT_KIND_SECRET {
			t.Fatalf("two aliases of a sealed value should share one secret input: %v", first)
		}
		created := save(true)
		err := f.store.Revise(t.Context(), func(tx *store.RevisionTx) error {
			// These features are deliberately read by name, without decoding their specs.
			for kind, name := range map[string]string{"backup.policy": "nightly", "task": "refresh", "link": "database"} {
				if _, err := tx.AddRevision(t.Context(), store.NewRevision{
					Kind: kind, ID: "res_" + name, Scope: env, Name: name, Spec: []byte{}, Change: created.GetId(),
				}); err != nil {
					return err
				}
			}
			return nil
		})
		if err != nil {
			t.Fatal(err)
		}
		// A separate projection is sealed to a different purpose. Substituting
		// the old spec into a new revision makes reading it fail authentication.
		ctx := t.Context()
		err = f.store.Revise(ctx, func(tx *store.RevisionTx) error {
			row, err := tx.GetResource(ctx, "service", env, ref.GetName())
			if err != nil {
				return err
			}
			body, err := tx.ComposeSource(ctx, row)
			if err != nil {
				return err
			}
			plain, err := f.keeper.Open(ctx, tx.Tx, store.InComposeSource(row.ID, row.Kind, row.Revision), body)
			if err != nil {
				return err
			}
			body, err = f.keeper.Seal(ctx, tx.Tx, store.InComposeSource(row.ID, row.Kind, row.Revision+1), plain)
			if err != nil {
				return err
			}
			_, err = tx.AddRevision(ctx, store.NewRevision{
				Kind: row.Kind, Scope: env, Name: row.Name, Change: row.ChangeID,
				Spec: row.Spec, ComposeSource: body,
			})
			return err
		})
		if err != nil {
			t.Fatal(err)
		}
		refused := prepare()
		if refused.GetDraft() != nil || !strings.Contains(refused.GetRefusal().GetMessage(), "command") ||
			refused.GetRefusal().GetLine() == 0 {
			t.Fatalf("a sealed command use should be refused at its field: %v", refused)
		}
		found := make(map[string]string)
		for _, omission := range refused.GetOmissions() {
			found[omission.GetName()] = omission.GetPath()
		}
		if found["nightly"] != "backup schedule" || found["refresh"] != "scheduled task" || found["database"] != "link" {
			t.Fatalf("missing named omissions: %v", found)
		}
	})
}

func TestComposeTemplateGoesWithItsProject(t *testing.T) {
	t.Parallel()
	eachStoreParallel(t, func(t *testing.T, backend store.Backend) {
		f := startFull(t, backend, nil)
		b := signedIn(t, f)
		changes := changeClient(b, b.origin)
		project := &v1.ResourceRef{Kind: "project", Name: "template-library"}
		created := propose(t, changes, "Create library project", &v1.Edit{
			Resource: project,
			Spec:     &spec.Spec{Kind: &spec.Spec_Project{Project: &spec.Project{}}},
		})
		applyConfirmed(t, changes, created)
		var projectID string
		for _, row := range created.GetPlan().GetResources() {
			if row.GetResource().GetKind() == "project" {
				projectID = row.GetId()
			}
		}
		if projectID == "" {
			t.Fatal("project is absent from its own plan")
		}
		template := propose(t, changes, "Save a template", &v1.Edit{
			Resource: &v1.ResourceRef{Scope: projectID, Kind: "compose.template", Name: "shop"},
			Spec: &spec.Spec{Kind: &spec.Spec_ComposeTemplate{ComposeTemplate: &spec.ComposeTemplate{
				ComposeYaml: "services:\n  web:\n    image: nginx:1.27\n",
			}}},
		})
		applyConfirmed(t, changes, template)
		templateID := template.GetPlan().GetResources()[0].GetId()
		_, err := changes.ProposeChange(t.Context(), connect.NewRequest(&v1.ProposeChangeRequest{
			Summary: "Delete the project", Edits: []*v1.Edit{{Resource: project, Delete: true}},
		}))
		if err == nil || !strings.Contains(err.Error(), "Open Compose templates") || strings.Contains(err.Error(), "compose.templates delete") {
			t.Fatalf("project deletion should point to its panel actions: %v", err)
		}
		retire := propose(t, changes, "Retire the library and its project", &v1.Edit{
			Resource: project, Delete: true, WithContents: true,
		})
		found := false
		for _, row := range retire.GetPlan().GetResources() {
			found = found || row.GetId() == templateID && row.GetAction() == v1.PlanAction_PLAN_ACTION_DELETE
		}
		if !found {
			t.Fatal("review did not include the project's template")
		}
		applyConfirmed(t, changes, retire)
	})
}

func TestComposeTemplateDeploysPinnedRevisionOnRealDocker(t *testing.T) {
	d := dockertest.Real(t)
	f := storageControl(t, store.SQLite, d, "")
	client := dockwellv1connect.NewComposeServiceClient(f.person, f.person.origin, connect.WithProtoJSON())
	resources := dockwellv1connect.NewResourceServiceClient(f.person, f.person.origin, connect.WithProtoJSON())
	projects, err := resources.ListResources(t.Context(), connect.NewRequest(&v1.ListResourcesRequest{Kind: "project"}))
	if err != nil || len(projects.Msg.GetResources()) != 1 {
		t.Fatalf("projects: %v, %v", projects, err)
	}
	project := projects.Msg.GetResources()[0].GetId()
	const file = `services:
  web:
    image: busybox:1.37
    command: ["sleep", "600"]
    environment:
      API_TOKEN: ${API_TOKEN}
    networks: [front]
    volumes: [data:/data]
networks:
  front: {}
volumes:
  data: {}
`
	ref := &v1.ResourceRef{Scope: project, Kind: "compose.template", Name: "real-template"}
	save := func(file string, base int64) string {
		t.Helper()
		e := &v1.Edit{Resource: ref, BaseRevision: base, Delete: file == ""}
		if file != "" {
			e.Spec = &spec.Spec{Kind: &spec.Spec_ComposeTemplate{ComposeTemplate: &spec.ComposeTemplate{ComposeYaml: file}}}
		}
		c := propose(t, f.changes, "Change the template fixture", e)
		applyConfirmed(t, f.changes, c)
		return c.GetPlan().GetResources()[0].GetId()
	}
	id := save(file, 0)
	save(strings.Replace(file, "1.37", "1.36", 1), 1)
	made, err := client.PlanFromComposeTemplate(t.Context(), connect.NewRequest(&v1.PlanFromComposeTemplateRequest{
		Id: id, Revision: 1, Destination: &v1.ComposeTemplateDestination{EnvironmentName: "real-template", ServerId: f.server.id.ServerID},
		Inputs: []*v1.ComposeTemplateValue{{Name: "API_TOKEN", Choice: &v1.ComposeTemplateValue_Generate{Generate: true}}},
	}))
	if err != nil || made.Msg.GetChange() == nil {
		t.Fatalf("template plan: %v %v", made, err)
	}
	c := made.Msg.GetChange()
	env := storageScope(t, c, "environment", "real-template").GetId()
	removeStorageFixture(t, d, env, []string{"front"}, []string{"data"})
	applyConfirmed(t, f.changes, c)
	got := f.workload(t, env)
	_, hasNetwork := got.NetworkSettings.Networks[docker.ScopedName(docker.Project(env), "front")]
	if got.Config.Image != "busybox:1.37" || !hasNetwork || len(got.Mounts) != 1 || got.Mounts[0].Destination != "/data" {
		t.Fatal("workload doesn't have the reviewed image, network and volume")
	}
	bound := false
	for _, entry := range got.Config.Env {
		if value, yes := strings.CutPrefix(entry, "API_TOKEN="); yes && len(value) >= 26 {
			bound = true
		}
	}
	if !bound {
		t.Fatal("the generated secret was not bound")
	}
	save("", 2)
	held := f.workload(t, env)
	if held.ID != got.ID || !held.State.Running {
		t.Fatal("template removal replaced or stopped its workload")
	}
}
