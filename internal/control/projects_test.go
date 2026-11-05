// SPDX-License-Identifier: AGPL-3.0-only

package control

import (
	"slices"
	"strings"
	"testing"

	"connectrpc.com/connect"

	dockwellv1 "github.com/dockwell/dockwell/gen/dockwell/v1"
	"github.com/dockwell/dockwell/gen/dockwell/v1/dockwellv1connect"
	"github.com/dockwell/dockwell/internal/docker"
	"github.com/dockwell/dockwell/internal/store"
	"github.com/dockwell/dockwell/internal/store/storetest"
)

// Each file with a service called db, as WordPress and Gitea both have.
const (
	blogFile = "services:\n  db:\n    image: ghcr.io/acme/shop:1.4\n"
	gitFile  = "name: git\nservices:\n  db:\n    image: ghcr.io/acme/worker:1.0\n"
)

func kindsOf(c *dockwellv1.Change) []string {
	var out []string
	for _, r := range c.GetPlan().GetResources() {
		out = append(out, r.GetResource().GetKind()+" "+r.GetResource().GetName())
	}
	return out
}

// dbScope is where a change's db service goes.
func dbScope(c *dockwellv1.Change) string {
	for _, r := range c.GetPlan().GetResources() {
		if r.GetResource().GetKind() == "service" && r.GetResource().GetName() == "db" {
			return r.GetResource().GetScope()
		}
	}
	return ""
}

// A Compose file gets its own project, so a second file with a service of the
// same name runs beside the first, on a network of its own, rather than
// planning a change to it (#77).
func TestEachComposeFileGetsItsOwnProject(t *testing.T) {
	storetest.Each(t, func(t *testing.T, backend store.Backend) {
		f := startFull(t, backend, nil)
		b := signedIn(t, f)
		e := f.addDockerServer(t)
		c := changeClient(b, b.origin)
		cc := dockwellv1connect.NewComposeServiceClient(b, b.origin, connect.WithProtoJSON())
		imports := func(req *dockwellv1.ImportComposeRequest) (*dockwellv1.Change, error) {
			t.Helper()
			res, err := cc.ImportCompose(t.Context(), connect.NewRequest(req))
			if err != nil {
				return nil, err
			}
			return res.Msg.GetChange(), nil
		}

		// With nothing named, the first file goes in the default project, which
		// has no apps yet.
		first, err := imports(&dockwellv1.ImportComposeRequest{Compose: blogFile})
		if err != nil {
			t.Fatal(err)
		}
		if got := first.GetSummary(); got != "Run a Compose file" {
			t.Errorf("the first file's summary is %q", got)
		}
		if got := kindsOf(first); !slices.Equal(got, []string{"network default", "service db"}) {
			t.Errorf("the first file plans %v", got)
		}
		applied(t, c, first)
		defaultEnv := dbScope(first)

		// The second, with nothing named, would change that db, so it is refused.
		_, err = imports(&dockwellv1.ImportComposeRequest{Compose: blogFile})
		if reasonOf(err) != "needs_project" || !strings.Contains(err.Error(), "default already holds db") ||
			!strings.Contains(err.Error(), "pass project") || !strings.Contains(err.Error(), "name: shop") {
			t.Fatalf("a second file with nothing named = %v", err)
		}

		// One that names itself makes a project in one plan, as creates, with a
		// db of its own beside the first.
		second, err := imports(&dockwellv1.ImportComposeRequest{Compose: gitFile})
		if err != nil {
			t.Fatal(err)
		}
		if got := kindsOf(second); !slices.Equal(got, []string{"project git", "environment production", "network default", "service db"}) {
			t.Errorf("the second file plans %v", got)
		}
		if got := second.GetPlan().GetSummary(); got.GetCreate() != 4 || got.GetChange() != 0 || got.GetReplace() != 0 {
			t.Errorf("the second file's plan counts %v; want creates alone, and nothing of the first changed", got)
		}
		if got := second.GetSummary(); got != "Run git from a Compose file" {
			t.Errorf("the second file's summary is %q", got)
		}
		if got := applied(t, c, second); got.GetState() != dockwellv1.ChangeState_CHANGE_STATE_APPLIED {
			t.Fatalf("the second file ended %v: %s", got.GetState(), got.GetError())
		}
		gitEnv := dbScope(second)
		if gitEnv == defaultEnv || gitEnv == "" {
			t.Fatalf("the second file's environment is %q, and the first's %q", gitEnv, defaultEnv)
		}

		// Side by side on Docker, each on its own network, as under docker compose.
		one := e.Serving(docker.Project(defaultEnv) + "-db-1")
		two := e.Serving(docker.Project(gitEnv) + "-db-1")
		if one == nil || two == nil || one.Config.Image == two.Config.Image {
			t.Fatalf("Docker has %v", e.Containers())
		}
		if _, on := two.NetworkSettings.Networks[docker.Project(gitEnv)+"_default"]; !on {
			t.Errorf("git's db is on %v, want a network of its own", two.NetworkSettings.Networks)
		}
		if _, on := two.NetworkSettings.Networks[docker.Project(defaultEnv)+"_default"]; on {
			t.Errorf("git's db can reach the first file's: %v", two.NetworkSettings.Networks)
		}

		// Naming a project that exists imports into it, and the plan shows updates.
		again, err := imports(&dockwellv1.ImportComposeRequest{Project: "git", Compose: strings.Replace(gitFile, "worker:1.0", "shop:1.5", 1)})
		if err != nil {
			t.Fatal(err)
		}
		if got := again.GetPlan().GetSummary(); got.GetCreate() != 0 || got.GetReplace()+got.GetChange() != 1 {
			t.Errorf("importing into git again counts %v; want one update and no creates", got)
		}
		if dbScope(again) != gitEnv {
			t.Errorf("importing into git again goes to %s, want %s", dbScope(again), gitEnv)
		}
		if _, err := c.DiscardChange(t.Context(), connect.NewRequest(&dockwellv1.DiscardChangeRequest{Id: again.GetId()})); err != nil {
			t.Fatal(err)
		}

		// A request's project wins over the file's name.
		named, err := imports(&dockwellv1.ImportComposeRequest{Project: "default", Compose: gitFile})
		if err != nil {
			t.Fatal(err)
		}
		if dbScope(named) != defaultEnv {
			t.Errorf("project default with name: git goes to %s, want %s", dbScope(named), defaultEnv)
		}
		if _, err := c.DiscardChange(t.Context(), connect.NewRequest(&dockwellv1.DiscardChangeRequest{Id: named.GetId()})); err != nil {
			t.Fatal(err)
		}

		// A file exported from a project goes back to it: its name is the
		// environment's Compose project, which is no project's name.
		ex, err := cc.ExportCompose(t.Context(), connect.NewRequest(&dockwellv1.ExportComposeRequest{Environment: gitEnv}))
		if err != nil {
			t.Fatal(err)
		}
		back, err := imports(&dockwellv1.ImportComposeRequest{Compose: ex.Msg.GetCompose()})
		if err != nil {
			t.Fatal(err)
		}
		if dbScope(back) != gitEnv {
			t.Errorf("the export goes to %s, want %s", dbScope(back), gitEnv)
		}
		if _, err := c.DiscardChange(t.Context(), connect.NewRequest(&dockwellv1.DiscardChangeRequest{Id: back.GetId()})); err != nil {
			t.Fatal(err)
		}
		_, err = imports(&dockwellv1.ImportComposeRequest{Compose: "name: env_01kat4zr6e3v0y5xbkzb5d9q6m\nservices:\n  db:\n    image: ghcr.io/acme/shop:1.4\n"})
		if reasonOf(err) != "invalid" || !strings.Contains(err.Error(), "exported from elsewhere") {
			t.Errorf("a file exported from another install = %v", err)
		}

		// A name that can't be a project's says what to do, about the file.
		_, err = imports(&dockwellv1.ImportComposeRequest{Compose: "name: my_app\nservices:\n  db:\n    image: ghcr.io/acme/shop:1.4\n"})
		if reasonOf(err) != "invalid" || !strings.Contains(err.Error(), "names itself my_app") || !strings.Contains(err.Error(), "Change name:") {
			t.Errorf("a file named my_app = %v", err)
		}

		// A project made with an environment of its own starts with that one.
		staged, err := imports(&dockwellv1.ImportComposeRequest{Project: "shop", Environment: "staging", Compose: blogFile})
		if err != nil {
			t.Fatal(err)
		}
		if got := kindsOf(staged); !slices.Contains(got, "environment staging") || slices.Contains(got, "environment production") {
			t.Errorf("a project with environment staging plans %v", got)
		}

		// Undoing the import takes the project away with what is in it.
		applied(t, c, staged)
		undo, err := c.UndoChange(t.Context(), connect.NewRequest(&dockwellv1.UndoChangeRequest{Id: staged.GetId()}))
		if err != nil {
			t.Fatal(err)
		}
		if got := applied(t, c, undo.Msg.GetChange()); got.GetState() != dockwellv1.ChangeState_CHANGE_STATE_APPLIED {
			t.Fatalf("undoing the import ended %v: %s", got.GetState(), got.GetError())
		}
		// Imported again, it continues its history, and so does its environment.
		remade, err := imports(&dockwellv1.ImportComposeRequest{Project: "shop", Environment: "staging", Compose: blogFile})
		if err != nil {
			t.Fatal(err)
		}
		if dbScope(remade) != dbScope(staged) {
			t.Errorf("imported again, shop's environment is %s, was %s", dbScope(remade), dbScope(staged))
		}
	})
}

// A form hears the name a file gives itself, even while it can't read further.
func TestCheckComposeSaysWhatAFileIsCalled(t *testing.T) {
	storetest.Each(t, func(t *testing.T, backend store.Backend) {
		f := startFull(t, backend, nil)
		b := signedIn(t, f)
		cc := dockwellv1connect.NewComposeServiceClient(b, b.origin, connect.WithProtoJSON())
		check := func(file string, vars map[string]string) *dockwellv1.CheckComposeResponse {
			t.Helper()
			res, err := cc.CheckCompose(t.Context(), connect.NewRequest(&dockwellv1.CheckComposeRequest{Compose: file, Variables: vars}))
			if err != nil {
				t.Fatal(err)
			}
			return res.Msg
		}
		if got := check(gitFile, nil).GetName(); got != "git" {
			t.Errorf("the name = %q", got)
		}
		if got := check("name: git\nservices:\n  db:\n    image: ${MISSING}\n", nil); got.GetName() != "git" || got.GetRefusal() == nil {
			t.Errorf("a file that needs a variable = %v", got)
		}
		if got := check(blogFile, nil).GetName(); got != "" {
			t.Errorf("a file with no name = %q", got)
		}
		if got := check("name: ${PROJECT}\nservices:\n  db:\n    image: a\n", nil).GetName(); got != "" {
			t.Errorf("a file named by a variable with no value = %q", got)
		}
		if got := check("name: ${PROJECT}\nservices:\n  db:\n    image: a\n", map[string]string{"PROJECT": "blog"}).GetName(); got != "blog" {
			t.Errorf("a file named by a variable = %q", got)
		}
	})
}

// A variable's value written into a command is read back from there by anyone
// who can read the service. It isn't blanked, as seeing a command is useful,
// so the file's check, the import and the plan each say so.
func TestAComposeFileSaysWhereItExposesAVariable(t *testing.T) {
	storetest.Each(t, func(t *testing.T, backend store.Backend) {
		f := startFull(t, backend, nil)
		b := signedIn(t, f)
		f.addDockerServer(t)
		c := changeClient(b, b.origin)
		cc := dockwellv1connect.NewComposeServiceClient(b, b.origin, connect.WithProtoJSON())
		s := dockwellv1connect.NewServiceServiceClient(b, b.origin, connect.WithProtoJSON())
		const secret = "hunter2-redis"
		const file = `services:
  shop:
    image: ghcr.io/acme/shop:1.4
    command: ["shop", "--requirepass", "${REDIS_PASSWORD}"]
    environment:
      SAFE: ${SAFE_VALUE:-x}
    labels:
      owner: ${OWNER:-ops}
      password: ${LABEL_SECRET}
    healthcheck:
      test: ["CMD", "check", "$PROBE_TOKEN"]
`
		vars := map[string]string{"REDIS_PASSWORD": secret, "LABEL_SECRET": "l", "PROBE_TOKEN": "p"}
		// A form goes by what could come, so OWNER counts though it has a default: a
		// value lands there once it's given. A plan goes by the values it has.
		want := []string{"LABEL_SECRET/shop/labels", "OWNER/shop/labels", "PROBE_TOKEN/shop/healthcheck", "REDIS_PASSWORD/shop/command"}
		given := []string{"LABEL_SECRET/shop/labels", "PROBE_TOKEN/shop/healthcheck", "REDIS_PASSWORD/shop/command"}
		names := func(list []*dockwellv1.ExposedVariable) []string {
			out := make([]string, len(list))
			for i, e := range list {
				if e.GetResource().GetKind() != "service" {
					t.Errorf("%s is held by a %s", e.GetName(), e.GetResource().GetKind())
				}
				out[i] = e.GetName() + "/" + e.GetResource().GetName() + "/" + e.GetField()
			}
			slices.Sort(out)
			return out
		}

		// Before anyone types a value, the form can warn.
		before, err := cc.CheckCompose(t.Context(), connect.NewRequest(&dockwellv1.CheckComposeRequest{Compose: file}))
		if err != nil {
			t.Fatal(err)
		}
		if got := names(before.Msg.GetExposedVariables()); !slices.Equal(got, want) {
			t.Errorf("before it has values, the check says %v, want %v", got, want)
		}
		checked, err := cc.CheckCompose(t.Context(), connect.NewRequest(&dockwellv1.CheckComposeRequest{Compose: file, Variables: vars}))
		if err != nil {
			t.Fatal(err)
		}
		if got := names(checked.Msg.GetExposedVariables()); !slices.Equal(got, want) {
			t.Errorf("the check says %v, want %v", got, want)
		}

		res, err := cc.ImportCompose(t.Context(), connect.NewRequest(&dockwellv1.ImportComposeRequest{Project: "shop", Compose: file, Variables: vars}))
		if err != nil {
			t.Fatal(err)
		}
		if got := names(res.Msg.GetExposedVariables()); !slices.Equal(got, given) {
			t.Errorf("the import says %v, want %v", got, given)
		}
		notes := res.Msg.GetChange().GetPlan().GetNotes()
		if len(notes) != len(given) {
			t.Fatalf("the plan's notes are %v", notes)
		}
		var note *dockwellv1.PlanNote
		for _, n := range notes {
			if n.GetNames()[0] == "REDIS_PASSWORD" {
				note = n
			}
		}
		if note == nil || note.GetReason() != "exposed_variable" ||
			!strings.Contains(note.GetMessage(), "anyone who can read this change or export this environment can read it") ||
			!strings.Contains(note.GetMessage(), "set it in environment and have the command read it there, with sh -c") ||
			note.GetResource().GetName() != "shop" {
			t.Errorf("the note says %v", note)
		}
		if strings.Contains(note.GetMessage(), secret) {
			t.Error("the note holds the value")
		}
		// GetChange reads the same notes back.
		got, err := c.GetChange(t.Context(), connect.NewRequest(&dockwellv1.GetChangeRequest{Id: res.Msg.GetChange().GetId()}))
		if err != nil || len(got.Msg.GetChange().GetPlan().GetNotes()) != len(given) {
			t.Errorf("GetChange's notes = %v, %v", got.Msg.GetChange().GetPlan().GetNotes(), err)
		}

		// What the notes warn of is real: a reader sees it in the command, and
		// not in the environment.
		applied(t, c, res.Msg.GetChange())
		list, err := s.ListServices(t.Context(), connect.NewRequest(&dockwellv1.ListServicesRequest{}))
		if err != nil {
			t.Fatal(err)
		}
		app := list.Msg.GetServices()[0].GetSpec().GetApp()
		if !slices.Contains(app.GetCommand(), secret) {
			t.Errorf("the command reads %v; the warning would be false", app.GetCommand())
		}
		if v := app.GetEnv()["SAFE"]; v != "" {
			t.Errorf("the environment shows %q", v)
		}

		// An escaped $$ exposes nothing, and neither does what goes in environment.
		quiet := "name: quiet\nservices:\n  a:\n    image: ghcr.io/acme/shop:1.4\n    command: [\"run\", \"$$HOME\"]\n    environment:\n      KEY: ${KEY}\n"
		q, err := cc.CheckCompose(t.Context(), connect.NewRequest(&dockwellv1.CheckComposeRequest{Compose: quiet, Variables: map[string]string{"KEY": "v"}}))
		if err != nil {
			t.Fatal(err)
		}
		if got := q.Msg.GetExposedVariables(); len(got) != 0 {
			t.Errorf("$$ and environment expose %v", got)
		}
	})
}

// A service goes by its name where that is enough to say which, and two files
// can each have one called db.
func TestAServiceGoesByNameWhereThatSaysWhich(t *testing.T) {
	storetest.Each(t, func(t *testing.T, backend store.Backend) {
		f := startFull(t, backend, nil)
		b := signedIn(t, f)
		f.addDockerServer(t)
		c := changeClient(b, b.origin)
		cc := dockwellv1connect.NewComposeServiceClient(b, b.origin, connect.WithProtoJSON())
		s := dockwellv1connect.NewServiceServiceClient(b, b.origin, connect.WithProtoJSON())
		run := func(project, file string) {
			t.Helper()
			res, err := cc.ImportCompose(t.Context(), connect.NewRequest(&dockwellv1.ImportComposeRequest{Project: project, Compose: file}))
			if err != nil {
				t.Fatal(err)
			}
			applied(t, c, res.Msg.GetChange())
		}
		run("blog", blogFile)
		run("git", gitFile)

		// One project says which.
		deployed, err := s.DeployService(t.Context(), connect.NewRequest(&dockwellv1.DeployServiceRequest{Id: "db", Project: "blog", Image: "ghcr.io/acme/shop:1.5"}))
		if err != nil {
			t.Fatal(err)
		}
		if got := deployed.Msg.GetChange().GetPlan().GetResources()[0].GetResource().GetName(); got != "db" {
			t.Errorf("the deploy is of %q", got)
		}
		applied(t, c, deployed.Msg.GetChange())
		revisions, err := s.ListServiceRevisions(t.Context(), connect.NewRequest(&dockwellv1.ListServiceRevisionsRequest{Id: "db", Project: "blog"}))
		if err != nil || len(revisions.Msg.GetRevisions()) != 2 {
			t.Errorf("db of blog's revisions = %v, %v", revisions, err)
		}

		// Nothing to say which is refused, saying where each one is.
		_, err = s.DeployService(t.Context(), connect.NewRequest(&dockwellv1.DeployServiceRequest{Id: "db", Image: "ghcr.io/acme/shop:1.5"}))
		if reasonOf(err) != "invalid" || !strings.Contains(err.Error(), "more than one service is named db: in blog and git") {
			t.Errorf("deploying db with nothing to say which = %v", err)
		}
		_, err = s.RollBackService(t.Context(), connect.NewRequest(&dockwellv1.RollBackServiceRequest{Id: "nope", Revision: 1}))
		if reasonOf(err) != "not_found" || strings.Contains(err.Error(), "ListServices") {
			t.Errorf("rolling back no service = %v; it should say so without naming an RPC", err)
		}
	})
}

// Adding a port plans it through the same gate as any edit, keeps what no
// response carries, and refuses what is there already.
func TestAddingAPortKeepsTheRest(t *testing.T) {
	storetest.Each(t, func(t *testing.T, backend store.Backend) {
		f := startFull(t, backend, nil)
		b := signedIn(t, f)
		e := f.addDockerServer(t)
		c := changeClient(b, b.origin)
		s := dockwellv1connect.NewServiceServiceClient(b, b.origin, connect.WithProtoJSON())
		const secret = "s3cret-pw"
		made, err := c.ProposeChange(t.Context(), connect.NewRequest(&dockwellv1.ProposeChangeRequest{
			Summary: "Run the shop", Edits: []*dockwellv1.Edit{serviceEdit("shop", "ghcr.io/acme/shop:1.4", map[string]string{"API_KEY": secret})},
		}))
		if err != nil {
			t.Fatal(err)
		}
		applied(t, c, made.Msg.GetChange())
		info := onlyService(t, s)

		if _, err := s.AddServicePort(t.Context(), connect.NewRequest(&dockwellv1.AddServicePortRequest{Id: info.GetId()})); reasonOf(err) != "invalid" {
			t.Errorf("adding no port = %v", err)
		}
		res, err := s.AddServicePort(t.Context(), connect.NewRequest(&dockwellv1.AddServicePortRequest{Id: info.GetId(), HostPort: 8080, ContainerPort: 80}))
		if err != nil {
			t.Fatal(err)
		}
		change := res.Msg.GetChange()
		if change.GetSummary() != "Publish port 8080 on shop" || change.GetState() != dockwellv1.ChangeState_CHANGE_STATE_PLANNED {
			t.Errorf("the plan is %v", change)
		}
		if strings.Contains(change.String(), secret) {
			t.Error("the plan carries a secret")
		}
		applied(t, c, change)
		ct := e.Serving(docker.Project(info.GetResource().GetScope()) + "-shop-1")
		if ct == nil || !hasEnv(ct.Config.Env, "API_KEY="+secret) {
			t.Fatalf("adding a port lost the secret: %+v", ct)
		}
		after := onlyService(t, s).GetSpec().GetApp().GetPorts()
		if len(after) != 1 || after[0].GetHostPort() != 8080 || after[0].GetContainerPort() != 80 {
			t.Errorf("the service publishes %v", after)
		}
		if _, err := s.AddServicePort(t.Context(), connect.NewRequest(&dockwellv1.AddServicePortRequest{Id: info.GetId(), HostPort: 8080, ContainerPort: 81})); reasonOf(err) != "invalid" ||
			!strings.Contains(err.Error(), "already publishes port 8080") {
			t.Errorf("adding a port it publishes = %v", err)
		}
	})
}

// Going back to a revision that matches what runs is refused, so it can't
// mark the change that got there as undone. The revisions say which match,
// and which revision a rollback put back; and a rollback of a service with
// data says the data stays.
func TestARollbackThatWouldDoNothingIsRefused(t *testing.T) {
	storetest.Each(t, func(t *testing.T, backend store.Backend) {
		f := startFull(t, backend, nil)
		b := signedIn(t, f)
		f.addDockerServer(t)
		c := changeClient(b, b.origin)
		cc := dockwellv1connect.NewComposeServiceClient(b, b.origin, connect.WithProtoJSON())
		s := dockwellv1connect.NewServiceServiceClient(b, b.origin, connect.WithProtoJSON())
		file := func(tag string) string {
			return "services:\n  web:\n    image: ghcr.io/acme/shop:" + tag + "\n    volumes: [\"data:/data\", \"/srv/uploads:/uploads\"]\nvolumes:\n  data: {}\n"
		}
		run := func(tag string) {
			t.Helper()
			res, err := cc.ImportCompose(t.Context(), connect.NewRequest(&dockwellv1.ImportComposeRequest{Project: "shop", Compose: file(tag)}))
			if err != nil {
				t.Fatal(err)
			}
			if got := applied(t, c, res.Msg.GetChange()); got.GetState() != dockwellv1.ChangeState_CHANGE_STATE_APPLIED {
				t.Fatalf("running %s ended %v: %s", tag, got.GetState(), got.GetError())
			}
		}
		run("1.4")
		run("1.5")
		web := onlyService(t, s)

		// r3 puts back what r1 had.
		back, err := s.RollBackService(t.Context(), connect.NewRequest(&dockwellv1.RollBackServiceRequest{Id: web.GetId(), Revision: 1}))
		if err != nil {
			t.Fatal(err)
		}
		kept := noteOf(back.Msg.GetChange(), "volumes_stay")
		if kept == nil || !slices.Equal(kept.GetNames(), []string{"/srv/uploads", "data"}) ||
			!strings.Contains(kept.GetMessage(), "A rollback puts back its spec, not its data") {
			t.Errorf("the rollback's notes = %v", back.Msg.GetChange().GetPlan().GetNotes())
		}
		if got := applied(t, c, back.Msg.GetChange()); got.GetState() != dockwellv1.ChangeState_CHANGE_STATE_APPLIED {
			t.Fatalf("the rollback ended %v: %s", got.GetState(), got.GetError())
		}
		list, err := s.ListServiceRevisions(t.Context(), connect.NewRequest(&dockwellv1.ListServiceRevisionsRequest{Id: web.GetId()}))
		if err != nil {
			t.Fatal(err)
		}
		revs := list.Msg.GetRevisions()
		if len(revs) != 3 || !revs[2].GetMatchesLatest() || revs[1].GetMatchesLatest() || !revs[0].GetMatchesLatest() {
			t.Errorf("which match = r3 %t, r2 %t, r1 %t", revs[0].GetMatchesLatest(), revs[1].GetMatchesLatest(), revs[2].GetMatchesLatest())
		}
		if revs[0].GetRolledBackTo() != 1 || revs[1].GetRolledBackTo() != 0 {
			t.Errorf("r3 went back to %d, r2 to %d", revs[0].GetRolledBackTo(), revs[1].GetRolledBackTo())
		}

		// r1 matches r3: going back to it would change nothing, so it is refused,
		// and r3's change keeps its undo.
		_, err = s.RollBackService(t.Context(), connect.NewRequest(&dockwellv1.RollBackServiceRequest{Id: web.GetId(), Revision: 1}))
		if reasonOf(err) != "cant_undo" || !strings.Contains(err.Error(), "already runs what revision 1 had") {
			t.Fatalf("going back to a revision that matches = %v", err)
		}
		latest, err := c.GetChange(t.Context(), connect.NewRequest(&dockwellv1.GetChangeRequest{Id: revs[0].GetChange().GetId()}))
		if err != nil || latest.Msg.GetChange().GetUndoneBy() != "" {
			t.Errorf("r3's change = %v, %v; want it not undone", latest, err)
		}
		// r2 differs, so it plans.
		if _, err := s.RollBackService(t.Context(), connect.NewRequest(&dockwellv1.RollBackServiceRequest{Id: web.GetId(), Revision: 2})); err != nil {
			t.Errorf("going back to r2 = %v", err)
		}
	})
}
