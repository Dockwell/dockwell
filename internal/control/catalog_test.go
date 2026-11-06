// SPDX-License-Identifier: AGPL-3.0-only

package control

import (
	"slices"
	"strings"
	"testing"

	"connectrpc.com/connect"

	resourcev1 "github.com/dockwell/dockwell/gen/dockwell/resource/v1alpha1"
	dockwellv1 "github.com/dockwell/dockwell/gen/dockwell/v1"
	"github.com/dockwell/dockwell/gen/dockwell/v1/dockwellv1connect"
	"github.com/dockwell/dockwell/internal/docker"
	"github.com/dockwell/dockwell/internal/docker/dockertest"
	"github.com/dockwell/dockwell/internal/store"
	"github.com/dockwell/dockwell/internal/store/storetest"
)

// pullsAnything has a fake engine find whatever an entry pins, as a registry would.
func pullsAnything(e *dockertest.Engine) {
	e.Find = func(string) (dockertest.Image, bool) { return dockertest.Image{}, true }
}

func catalogClient(b *browser) dockwellv1connect.CatalogServiceClient {
	return dockwellv1connect.NewCatalogServiceClient(b, b.origin, connect.WithProtoJSON())
}

func installApp(t *testing.T, cat dockwellv1connect.CatalogServiceClient, req *dockwellv1.InstallCatalogEntryRequest) *dockwellv1.Change {
	t.Helper()
	res, err := cat.InstallCatalogEntry(t.Context(), connect.NewRequest(req))
	if err != nil {
		t.Fatalf("InstallCatalogEntry: %v", err)
	}
	return res.Msg.GetChange()
}

// An app installs as a project of its own through a plan: its password made and
// sealed, used by reference, and on no response; its port one nothing has taken; and
// the install pinned, so the catalog knows it. A second install lands beside it, and
// undoing one takes all of it away.
func TestInstallingFromTheCatalog(t *testing.T) { storetest.Each(t, testInstallingFromTheCatalog) }

func testInstallingFromTheCatalog(t *testing.T, backend store.Backend) {
	f := startFull(t, backend, nil)
	b := signedIn(t, f)
	e := f.addDockerServer(t)
	pullsAnything(e)
	c := changeClient(b, b.origin)
	cat := catalogClient(b)

	entries, err := cat.ListCatalogEntries(t.Context(), connect.NewRequest(&dockwellv1.ListCatalogEntriesRequest{}))
	if err != nil {
		t.Fatal(err)
	}
	if len(entries.Msg.GetEntries()) < 4 {
		t.Fatalf("the catalog has %d entries", len(entries.Msg.GetEntries()))
	}
	first, err := cat.GetCatalogEntry(t.Context(), connect.NewRequest(&dockwellv1.GetCatalogEntryRequest{Id: "postgres"}))
	if err != nil {
		t.Fatal(err)
	}
	if first.Msg.GetProject() != "postgres" || first.Msg.GetValues()["ADMIN_PORT"] != "8081" {
		t.Errorf("GetCatalogEntry suggests %q and %v", first.Msg.GetProject(), first.Msg.GetValues())
	}

	planned := installApp(t, cat, &dockwellv1.InstallCatalogEntryRequest{Id: "postgres"})
	if want := "Install PostgreSQL 17.11 as postgres"; planned.GetSummary() != want {
		t.Errorf("summary %q, want %q", planned.GetSummary(), want)
	}
	var order []string
	for _, r := range planned.GetPlan().GetResources() {
		order = append(order, r.GetResource().GetKind()+" "+r.GetResource().GetName())
	}
	for _, want := range []string{"project postgres", "environment production", "secret POSTGRES_PASSWORD", "volume data", "service db", "service adminer"} {
		if !slices.Contains(order, want) {
			t.Errorf("the plan has %v, and no %s", order, want)
		}
	}
	if slices.Index(order, "service db") > slices.Index(order, "service adminer") {
		t.Errorf("planned %v: adminer before the database it depends on", order)
	}
	// What a person needs to know is in the plan, so it's there before they apply it,
	// from the panel and the CLI alike.
	var steps string
	for _, n := range planned.GetPlan().GetNotes() {
		if n.GetReason() == "first_steps" {
			steps = n.GetMessage()
		}
	}
	if !strings.Contains(steps, "POSTGRES_PASSWORD") {
		t.Errorf("the plan doesn't say what to do first: %v", planned.GetPlan().GetNotes())
	}
	if planned.GetPlan().GetDestructive() || planned.GetPlan().GetHostAccess() {
		t.Errorf("destructive %t, host access %t, for an app that reaches nothing", planned.GetPlan().GetDestructive(), planned.GetPlan().GetHostAccess())
	}
	applied := apply(t, c, planned)
	if applied.GetState() != dockwellv1.ChangeState_CHANGE_STATE_APPLIED {
		t.Fatalf("the install ended %v: %s", applied.GetState(), applied.GetError())
	}

	var env string
	for _, r := range planned.GetPlan().GetResources() {
		if r.GetResource().GetKind() == "service" {
			env = r.GetResource().GetScope()
		}
	}
	project := docker.Project(env)
	db := e.Container(project + "-db-1")
	if db == nil {
		t.Fatalf("no database container: %v", e.Containers())
	}
	var password string
	for _, kv := range db.Config.Env {
		if v, ok := strings.CutPrefix(kv, "POSTGRES_PASSWORD="); ok {
			password = v
		}
	}
	if len(password) != 32 {
		t.Fatalf("the database got a password of %d characters", len(password))
	}
	if strings.Contains(planned.String(), password) || strings.Contains(applied.String(), password) {
		t.Error("a plan holds the generated password")
	}
	adminer := e.Container(project + "-adminer-1")
	if adminer == nil || !strings.Contains(adminer.Config.Image, "@sha256:") {
		t.Fatalf("adminer runs %+v, not pinned by digest", adminer)
	}
	if got := adminer.HostConfig.PortBindings["8080/tcp"]; len(got) != 1 || got[0].HostPort != "8081" {
		t.Errorf("adminer publishes %+v", got)
	}

	// The password is the project's secret, and the database uses it by name.
	v := variablesClient(b, b.origin)
	list, err := v.ListVariables(t.Context(), connect.NewRequest(&dockwellv1.ListVariablesRequest{Project: "postgres"}))
	if err != nil {
		t.Fatal(err)
	}
	if len(list.Msg.GetVariables()) != 1 || !list.Msg.GetVariables()[0].GetSecret() || list.Msg.GetVariables()[0].GetUsedBy()[0].GetService() != "db" {
		t.Errorf("the project's secrets: %v", list.Msg.GetVariables())
	}

	installs, err := cat.ListCatalogInstalls(t.Context(), connect.NewRequest(&dockwellv1.ListCatalogInstallsRequest{}))
	if err != nil {
		t.Fatal(err)
	}
	if len(installs.Msg.GetInstalls()) != 1 {
		t.Fatalf("installs: %v", installs.Msg.GetInstalls())
	}
	pinned := installs.Msg.GetInstalls()[0]
	if pinned.GetPin().GetEntry() != "postgres" || pinned.GetPin().GetRelease() == 0 || pinned.GetPin().GetRepository() == "" ||
		pinned.GetPin().GetValues()["ADMIN_PORT"] != "8081" || pinned.GetWebServiceId() == "" || pinned.GetEnvironmentId() != env {
		t.Errorf("the install is pinned as %v", pinned)
	}
	if _, ok := pinned.GetPin().GetValues()["POSTGRES_PASSWORD"]; ok {
		t.Error("the pin keeps the password")
	}

	// The password is sealed: nothing in the database reads as it, in any encoding.
	for table, rows := range storetest.Dump(t, f.db) {
		for _, row := range rows {
			for _, enc := range encodings(password) {
				if strings.Contains(row, enc) {
					t.Errorf("%s holds the generated password, as %q", table, enc)
				}
			}
		}
	}

	// An edit replaces a whole spec, and renaming a project doesn't forget where it came from.
	apply(t, c, propose(t, c, "Rename it", &dockwellv1.Edit{
		Resource: &dockwellv1.ResourceRef{Kind: "project", Name: "postgres"},
		Spec:     &resourcev1.Spec{Kind: &resourcev1.Spec_Project{Project: &resourcev1.Project{DisplayName: "Our database"}}},
	}))
	installs, err = cat.ListCatalogInstalls(t.Context(), connect.NewRequest(&dockwellv1.ListCatalogInstallsRequest{}))
	if err != nil {
		t.Fatal(err)
	}
	if got := installs.Msg.GetInstalls(); len(got) != 1 || got[0].GetPin().GetEntry() != "postgres" {
		t.Errorf("after a rename, installs: %v", got)
	}

	// A second goes beside it, on a port the first hasn't taken, and an install never
	// changes what's there.
	second, err := cat.GetCatalogEntry(t.Context(), connect.NewRequest(&dockwellv1.GetCatalogEntryRequest{Id: "postgres"}))
	if err != nil {
		t.Fatal(err)
	}
	if second.Msg.GetProject() != "postgres-2" || second.Msg.GetValues()["ADMIN_PORT"] != "8082" {
		t.Errorf("a second install suggests %q and %v", second.Msg.GetProject(), second.Msg.GetValues())
	}
	_, err = cat.InstallCatalogEntry(t.Context(), connect.NewRequest(&dockwellv1.InstallCatalogEntryRequest{Id: "postgres", Project: "postgres"}))
	if connect.CodeOf(err) != connect.CodeAlreadyExists || !strings.Contains(err.Error(), "a project named postgres is here already") {
		t.Errorf("installing over a project = %v", err)
	}
	beside := installApp(t, cat, &dockwellv1.InstallCatalogEntryRequest{Id: "postgres"})
	if got := apply(t, c, beside); got.GetState() != dockwellv1.ChangeState_CHANGE_STATE_APPLIED {
		t.Fatalf("the second install ended %v: %s", got.GetState(), got.GetError())
	}

	// Undoing it takes the whole install away, data included, so it asks first.
	undo, err := c.UndoChange(t.Context(), connect.NewRequest(&dockwellv1.UndoChangeRequest{Id: beside.GetId()}))
	if err != nil {
		t.Fatal(err)
	}
	if !undo.Msg.GetChange().GetPlan().GetDestructive() {
		t.Errorf("undoing an install destroys its volume, and the plan doesn't say so: %v", undo.Msg.GetChange().GetPlan())
	}
	applyConfirmed(t, c, undo.Msg.GetChange())
	installs, err = cat.ListCatalogInstalls(t.Context(), connect.NewRequest(&dockwellv1.ListCatalogInstallsRequest{}))
	if err != nil {
		t.Fatal(err)
	}
	if len(installs.Msg.GetInstalls()) != 1 {
		t.Errorf("after the undo, installs: %v", installs.Msg.GetInstalls())
	}

	// Its name stays out of use, as the server keeps its volume for 7 days, which a
	// new password couldn't open.
	again, err := cat.GetCatalogEntry(t.Context(), connect.NewRequest(&dockwellv1.GetCatalogEntryRequest{Id: "postgres"}))
	if err != nil {
		t.Fatal(err)
	}
	if again.Msg.GetProject() != "postgres-3" {
		t.Errorf("after an undo it suggests %q", again.Msg.GetProject())
	}
	_, err = cat.InstallCatalogEntry(t.Context(), connect.NewRequest(&dockwellv1.InstallCatalogEntryRequest{Id: "postgres", Project: "postgres-2"}))
	if connect.CodeOf(err) != connect.CodeAlreadyExists || !strings.Contains(err.Error(), "its data may come back") {
		t.Errorf("installing over what was undone = %v", err)
	}
}

// What a person types is checked before anything is planned, and an app that needs a
// token takes it as a secret, by reference, and asks a person to approve what it reaches.
func TestInstallRefusalsAndHostAccess(t *testing.T) {
	storetest.Each(t, testInstallRefusalsAndHostAccess)
}

func testInstallRefusalsAndHostAccess(t *testing.T, backend store.Backend) {
	f := startFull(t, backend, nil)
	b := signedIn(t, f)
	e := f.addDockerServer(t)
	pullsAnything(e)
	cat := catalogClient(b)
	for _, tc := range []struct {
		req  *dockwellv1.InstallCatalogEntryRequest
		code connect.Code
		want string
	}{
		{&dockwellv1.InstallCatalogEntryRequest{Id: "nothing"}, connect.CodeNotFound, "the catalog has no app nothing"},
		{&dockwellv1.InstallCatalogEntryRequest{Id: "cloudflared"}, connect.CodeInvalidArgument, "Tunnel token needs a value"},
		{&dockwellv1.InstallCatalogEntryRequest{Id: "postgres", Values: map[string]string{"ADMIN_PORT": "99999"}}, connect.CodeInvalidArgument, "Adminer port takes a port"},
		{&dockwellv1.InstallCatalogEntryRequest{Id: "postgres", Values: map[string]string{"POSTGRES_PASSWORD": "mine"}}, connect.CodeInvalidArgument, "takes no value for POSTGRES_PASSWORD"},
		{&dockwellv1.InstallCatalogEntryRequest{Id: "postgres", Project: "Not A Name"}, connect.CodeInvalidArgument, "project"},
	} {
		_, err := cat.InstallCatalogEntry(t.Context(), connect.NewRequest(tc.req))
		if connect.CodeOf(err) != tc.code || !strings.Contains(err.Error(), tc.want) {
			t.Errorf("InstallCatalogEntry(%v) = %v, want %v saying %s", tc.req, err, tc.code, tc.want)
		}
	}

	const token = "a-tunnel-token-from-cloudflare"
	planned := installApp(t, cat, &dockwellv1.InstallCatalogEntryRequest{Id: "cloudflared", Values: map[string]string{"TUNNEL_TOKEN": token}})
	if !planned.GetPlan().GetHostAccess() {
		t.Errorf("a tunnel on the server's own network plans no host access: %v", planned.GetPlan())
	}
	if strings.Contains(planned.String(), token) {
		t.Error("the plan holds the token")
	}
	applyConfirmed(t, changeClient(b, b.origin), planned)
	var tunnel *docker.Container
	for _, name := range e.Containers() {
		if strings.HasSuffix(name, "-tunnel-1") {
			tunnel = e.Container(name)
		}
	}
	if tunnel == nil || !slices.Contains(tunnel.Config.Env, "TUNNEL_TOKEN="+token) {
		t.Errorf("the tunnel runs as %+v", tunnel)
	}
	list, err := variablesClient(b, b.origin).ListVariables(t.Context(), connect.NewRequest(&dockwellv1.ListVariablesRequest{Project: "cloudflared"}))
	if err != nil {
		t.Fatal(err)
	}
	if vars := list.Msg.GetVariables(); len(vars) != 1 || !vars[0].GetSecret() || vars[0].GetName() != "TUNNEL_TOKEN" {
		t.Errorf("the project's secrets: %v", vars)
	}
}
