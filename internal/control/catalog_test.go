// SPDX-License-Identifier: AGPL-3.0-only

package control

import (
	"fmt"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"

	"connectrpc.com/connect"

	resourcev1 "github.com/dockwell/dockwell/gen/dockwell/resource/v1alpha1"
	dockwellv1 "github.com/dockwell/dockwell/gen/dockwell/v1"
	"github.com/dockwell/dockwell/gen/dockwell/v1/dockwellv1connect"
	"github.com/dockwell/dockwell/internal/catalog"
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
	// What it installs, part by part: what a person opens first, then the other apps,
	// then the databases, each with its image by tag, its volumes and what it reaches
	// on the server, which the plan marks red when it's as good as root.
	for id, want := range map[string][]string{
		"postgres":    {"adminer adminer:6.2.0 database=false opens=true [] []", "db postgres:17.11 database=true opens=false [data] []"},
		"jellyfin":    {"jellyfin jellyfin/jellyfin:12.3 database=false opens=true [config cache] [HOST_ACCESS_KIND_SERVER_PATH /srv/media read-only from MEDIA_DIR]"},
		"plausible":   {"plausible ghcr.io/plausible/community-edition:v3.2.1 database=false opens=true [data] []", "db postgres:17.11 database=true opens=false [db] []", "events clickhouse/clickhouse-server:24.12.6.70-alpine database=true opens=false [events] []"},
		"coder":       {"coder ghcr.io/coder/coder:v2.38.6 database=false opens=true [home] [HOST_ACCESS_KIND_DOCKER_SOCKET /var/run/docker.sock root]", "db postgres:17.11 database=true opens=false [data] []"},
		"cloudflared": {"tunnel cloudflare/cloudflared:2026.10.3 database=false opens=false [] [HOST_ACCESS_KIND_HOST_NETWORK  root]"},
		// An input no one has given yet still says what it runs.
		"nextcloud": {"nextcloud nextcloud:35.0.2-apache database=false opens=true [html] []", "db postgres:17.11 database=true opens=false [data] []"},
	} {
		got, err := cat.GetCatalogEntry(t.Context(), connect.NewRequest(&dockwellv1.GetCatalogEntryRequest{Id: id}))
		if err != nil {
			t.Fatal(err)
		}
		if parts := describeParts(got.Msg.GetParts()); !slices.Equal(parts, want) {
			t.Errorf("%s says it installs %q, want %q", id, parts, want)
		}
	}

	planned := installApp(t, cat, &dockwellv1.InstallCatalogEntryRequest{Id: "postgres", Companions: map[string]bool{"adminer": true}})
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
	// A name asked for that's taken moves on to the next that's free.
	for asked, want := range map[string]string{"postgres": "postgres-2", "postgres-2": "postgres-2", "mydb": "mydb", "Not A Name": "postgres-2"} {
		got, err := cat.GetCatalogEntry(t.Context(), connect.NewRequest(&dockwellv1.GetCatalogEntryRequest{Id: "postgres", Project: asked}))
		if err != nil {
			t.Fatal(err)
		}
		if got.Msg.GetProject() != want {
			t.Errorf("from %q it suggests %q, want %q", asked, got.Msg.GetProject(), want)
		}
	}
	// It takes a password of a person's own in place of the one it would generate, and
	// seals it as it would that one.
	const own = "my own password 1"
	beside := installApp(t, cat, &dockwellv1.InstallCatalogEntryRequest{Id: "postgres", Values: map[string]string{"POSTGRES_PASSWORD": own}})
	besideApplied := apply(t, c, beside)
	if besideApplied.GetState() != dockwellv1.ChangeState_CHANGE_STATE_APPLIED {
		t.Fatalf("the second install ended %v: %s", besideApplied.GetState(), besideApplied.GetError())
	}
	if strings.Contains(beside.String(), own) || strings.Contains(besideApplied.String(), own) {
		t.Error("a plan holds a password a person gave")
	}
	var besideEnv string
	for _, r := range beside.GetPlan().GetResources() {
		if r.GetResource().GetKind() == "service" {
			besideEnv = r.GetResource().GetScope()
		}
	}
	if got := envOf(e.Container(docker.Project(besideEnv)+"-db-1"), "POSTGRES_PASSWORD"); got != own {
		t.Errorf("the database got %q as its password, not the one given", got)
	}
	for table, rows := range storetest.Dump(t, f.db) {
		for _, row := range rows {
			for _, enc := range encodings(own) {
				if strings.Contains(row, enc) {
					t.Errorf("%s holds the password a person gave, as %q", table, enc)
				}
			}
		}
	}
	for asked, want := range map[string]string{"postgres": "postgres-3", "postgres-2": "postgres-3"} {
		got, err := cat.GetCatalogEntry(t.Context(), connect.NewRequest(&dockwellv1.GetCatalogEntryRequest{Id: "postgres", Project: asked}))
		if err != nil {
			t.Fatal(err)
		}
		if got.Msg.GetProject() != want {
			t.Errorf("with two installed, from %q it suggests %q, want %q", asked, got.Msg.GetProject(), want)
		}
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

// envOf is what a container's environment gives a name.
func envOf(c *docker.Container, name string) string {
	if c == nil {
		return ""
	}
	for _, kv := range c.Config.Env {
		if v, ok := strings.CutPrefix(kv, name+"="); ok {
			return v
		}
	}
	return ""
}

// A value a person types for a secret is used by reference only where the file names
// the secret, even when another service's value is the same text, which a generated
// one never is.
func TestAnOwnSecretTakesNothingElsesPlace(t *testing.T) {
	dir := t.TempDir()
	t.Setenv(catalog.DirEnv, dir)
	const same = "hello-there-1"
	side := "    side:\n      image: thing:1@sha256:" + digest1 + "\n      environment:\n        GREETING: " + same + "\n"
	if err := os.WriteFile(filepath.Join(dir, "thing.yaml"), []byte(testEntry(1, "1.0", "thing:1@sha256:"+digest1, side)), 0o600); err != nil {
		t.Fatal(err)
	}
	f := startFull(t, store.SQLite, nil)
	b := signedIn(t, f)
	e := f.addDockerServer(t)
	pullsAnything(e)
	c := changeClient(b, b.origin)
	cat := catalogClient(b)
	planned := installApp(t, cat, &dockwellv1.InstallCatalogEntryRequest{Id: "thing", Values: map[string]string{"PASSWORD": same}})
	if got := apply(t, c, planned); got.GetState() != dockwellv1.ChangeState_CHANGE_STATE_APPLIED {
		t.Fatalf("the install ended %v: %s", got.GetState(), got.GetError())
	}
	var env string
	for _, r := range planned.GetPlan().GetResources() {
		if r.GetResource().GetKind() == "service" {
			env = r.GetResource().GetScope()
		}
	}
	project := docker.Project(env)
	if got := envOf(e.Newest(project, "web"), "PASSWORD"); got != same {
		t.Errorf("web's PASSWORD is %q", got)
	}
	if got := envOf(e.Newest(project, "side"), "GREETING"); got != same {
		t.Errorf("side's GREETING is %q", got)
	}
	v := variablesClient(b, b.origin)
	list, err := v.ListVariables(t.Context(), connect.NewRequest(&dockwellv1.ListVariablesRequest{Project: "thing"}))
	if err != nil {
		t.Fatal(err)
	}
	for _, variable := range list.Msg.GetVariables() {
		for _, use := range variable.GetUsedBy() {
			if use.GetService() == "side" {
				t.Errorf("side uses %s, which only web's file names", variable.GetName())
			}
		}
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
		{&dockwellv1.InstallCatalogEntryRequest{Id: "postgres", Values: map[string]string{"ADMIN_PORT": "99999"}, Companions: map[string]bool{"adminer": true}}, connect.CodeInvalidArgument, "Adminer port takes a port"},
		{&dockwellv1.InstallCatalogEntryRequest{Id: "postgres", Values: map[string]string{"ADMIN_PORT": "9000"}}, connect.CodeInvalidArgument, "ADMIN_PORT is Adminer's, which this install leaves out"},
		{&dockwellv1.InstallCatalogEntryRequest{Id: "postgres", Companions: map[string]bool{"pgweb": true}}, connect.CodeInvalidArgument, "PostgreSQL has no companion pgweb"},
		{&dockwellv1.InstallCatalogEntryRequest{Id: "postgres", Values: map[string]string{"POSTGRES_PASSWORD": "mine"}}, connect.CodeInvalidArgument, "Database password takes at least 8 characters"},
		{&dockwellv1.InstallCatalogEntryRequest{Id: "umami", Values: map[string]string{"POSTGRES_PASSWORD": "p@ss/word#12"}}, connect.CodeInvalidArgument, "as the app writes it into a URL"},
		{&dockwellv1.InstallCatalogEntryRequest{Id: "postgres", Project: "Not A Name"}, connect.CodeInvalidArgument, "project"},
	} {
		_, err := cat.InstallCatalogEntry(t.Context(), connect.NewRequest(tc.req))
		if connect.CodeOf(err) != tc.code || !strings.Contains(err.Error(), tc.want) {
			t.Errorf("InstallCatalogEntry(%v) = %v, want %v saying %s", tc.req, err, tc.code, tc.want)
		}
	}

	// T3 Code takes no approval: it reaches nothing on the server, and nothing reaches it.
	agents := installApp(t, cat, &dockwellv1.InstallCatalogEntryRequest{Id: "t3code"})
	if agents.GetPlan().GetHostAccess() || agents.GetPlan().GetDestructive() {
		t.Errorf("host access %t, destructive %t, for T3 Code", agents.GetPlan().GetHostAccess(), agents.GetPlan().GetDestructive())
	}
	if _, err := changeClient(b, b.origin).DiscardChange(t.Context(), connect.NewRequest(&dockwellv1.DiscardChangeRequest{Id: agents.GetId()})); err != nil {
		t.Fatal(err)
	}

	const token = "a-tunnel-token-from-cloudflare"
	planned := installApp(t, cat, &dockwellv1.InstallCatalogEntryRequest{Id: "cloudflared", Values: map[string]string{"TUNNEL_TOKEN": token}})
	if !planned.GetPlan().GetHostAccess() {
		t.Errorf("a tunnel on the server's own network plans no host access: %v", planned.GetPlan())
	}
	if strings.Contains(planned.String(), token) {
		t.Error("the plan holds the token")
	}
	applyAsRoot(t, b, planned)
	var tunnel *docker.Container
	for _, name := range e.Containers() {
		if strings.HasSuffix(name, ".tunnel-1") {
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

// describeParts says each part of an install in one line: its service, image, whether
// it's a database or what a person opens, its volumes, and what it reaches on the
// server, read-only or as good as root, with the input a path comes from.
func describeParts(parts []*dockwellv1.CatalogPart) []string {
	var out []string
	for _, p := range parts {
		var reaches []string
		for _, a := range p.GetHostAccess() {
			r := a.GetKind().String() + " " + a.GetPath()
			if a.GetReadOnly() {
				r += " read-only"
			}
			if a.GetSameAsRoot() {
				r += " root"
			}
			if in := p.GetPathInputs()[a.GetPath()]; in != "" {
				r += " from " + in
			}
			reaches = append(reaches, r)
		}
		out = append(out, fmt.Sprintf("%s %s database=%t opens=%t %v %v", p.GetService(), p.GetImage(), p.GetDatabase(), p.GetOpens(), p.GetVolumes(), reaches))
	}
	return out
}

// An entry whose parts can't be read still opens, with nothing said of its parts: what
// it installs is something to read before the plan, which says it again.
func TestAnEntryWhosePartsCantBeReadStillOpens(t *testing.T) {
	dir := t.TempDir()
	t.Setenv(catalog.DirEnv, dir)
	if err := os.WriteFile(filepath.Join(dir, "odd.yaml"), []byte(`id: odd
name: Odd
summary: Something whose parts don't read
category: storage
version: "1.0"
release: 1
license: AGPL-3.0-only
appLicense: MIT
source:
  repository: https://github.com/Dockwell/dockwell
inputs:
  - name: CPUS
    label: CPUs
    type: INPUT_TYPE_TEXT
compose: |
  services:
    web:
      image: nginx:1.27@sha256:aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa
      cpus: ${CPUS}
`), 0o600); err != nil {
		t.Fatal(err)
	}
	f := startFull(t, store.SQLite, nil)
	b := signedIn(t, f)
	got, err := catalogClient(b).GetCatalogEntry(t.Context(), connect.NewRequest(&dockwellv1.GetCatalogEntryRequest{Id: "odd"}))
	if err != nil {
		t.Fatalf("an entry whose parts don't read = %v, want it opened", err)
	}
	if got.Msg.GetEntry().GetName() != "Odd" || len(got.Msg.GetParts()) != 0 {
		t.Errorf("it opens as %v, with parts %v", got.Msg.GetEntry().GetName(), got.Msg.GetParts())
	}
}

// asRoot says, for each entry that reaches its server beyond its own containers,
// whether that's as good as root, which takes a passkey to apply. One new to the
// catalog needs a row here, so someone decides which it is.
var asRoot = map[string]bool{
	"cloudflared": true,  // the server's own network
	"coder":       true,  // Docker's socket
	"jellyfin":    false, // a media folder, read-only
}

// An entry that reaches its server beyond its containers waits for a person, on every
// store: a token can plan its install, and is refused applying it with needs_session,
// and a person with no passkey is asked for one when what it reaches is as good as root.
func TestAnEntryThatReachesItsServerWaitsForAPerson(t *testing.T) {
	storetest.Each(t, testAnEntryThatReachesItsServerWaitsForAPerson)
}

func testAnEntryThatReachesItsServerWaitsForAPerson(t *testing.T, backend store.Backend) {
	f := startFull(t, backend, nil)
	b := signedIn(t, f)
	e := f.addDockerServer(t)
	pullsAnything(e)
	token := bearer{token: createToken(t, b, dockwellv1.TokenScope_TOKEN_SCOPE_READ_WRITE), rt: b.client.Transport}
	byToken := dockwellv1connect.NewCatalogServiceClient(token, b.origin, connect.WithProtoJSON())
	tokenChanges, person := changeClient(token, b.origin), changeClient(b, b.origin)
	c, err := catalog.Load("")
	if err != nil {
		t.Fatal(err)
	}
	reaching := 0
	for _, entry := range c.All() {
		if entry.GetHostAccess() == "" {
			continue
		}
		reaching++
		t.Run(entry.GetId(), func(t *testing.T) {
			root, known := asRoot[entry.GetId()]
			if !known {
				t.Fatalf("%s reaches its server, and asRoot doesn't say whether that's as good as root. Add it", entry.GetId())
			}
			values := map[string]string{}
			for _, in := range entry.GetInputs() {
				if catalog.Asks(in) && in.GetDefaultValue() == "" && !in.GetOptional() {
					values[in.GetName()] = "someone@example.com"
					if catalog.Secret(in) {
						values[in.GetName()] = "typed-by-a-person-xyz"
					}
				}
			}
			res, err := byToken.InstallCatalogEntry(t.Context(), connect.NewRequest(&dockwellv1.InstallCatalogEntryRequest{Id: entry.GetId(), Values: values}))
			if err != nil {
				t.Fatalf("a token can't plan it: %v", err)
			}
			ch := res.Msg.GetChange()
			if !ch.GetPlan().GetHostAccess() {
				t.Errorf("its plan doesn't say it reaches the server: %v", ch.GetPlan())
			}
			fingerprint := ch.GetPlan().GetFingerprint()
			if err := askedBy(t, tokenChanges, ch, fingerprint); reasonOf(err) != "needs_session" {
				t.Errorf("a token applying it = %v, want needs_session", err)
			}
			err = askedBy(t, person, ch, fingerprint)
			switch {
			case root && reasonOf(err) != "needs_step_up":
				t.Errorf("a person with no passkey applying it = %v, want needs_step_up", err)
			case !root && err != nil:
				t.Errorf("a person applying it = %v, want it applied, as it isn't as good as root", err)
			}
			if root {
				if _, err := person.DiscardChange(t.Context(), connect.NewRequest(&dockwellv1.DiscardChangeRequest{Id: ch.GetId()})); err != nil {
					t.Fatal(err)
				}
			}
		})
	}
	if reaching < len(asRoot) {
		t.Errorf("%d entries reach their server, and asRoot names %d", reaching, len(asRoot))
	}
}

// testEntry is an entry a test can move on, as a newer release of the catalog would.
func testEntry(release int, version, image, extra string) string {
	return fmt.Sprintf(`id: thing
name: Thing
summary: Something to upgrade
category: storage
version: %q
release: %d
license: AGPL-3.0-only
appLicense: MIT
source:
  repository: https://github.com/Dockwell/dockwell
web:
  service: web
inputs:
  - name: PORT
    label: Port
    type: INPUT_TYPE_PORT
    defaultValue: "18100"
  - name: PASSWORD
    label: Password
    type: INPUT_TYPE_SECRET
    generate: 32
compose: |
  services:
    web:
      image: %s
      environment:
        PASSWORD: ${PASSWORD}
      ports:
        - "${PORT}:80"
      volumes:
        - data:/data
%s  volumes:
    data: {}
`, version, release, image, extra)
}

// A newer release shows as one, and upgrading is a plan that says no backup is taken,
// keeps the password and what a person added, and takes the pin along. An undo puts
// it all back, and an install that's up to date has nothing to upgrade.
func TestUpgradingAnInstall(t *testing.T) { storetest.Each(t, testUpgradingAnInstall) }

func testUpgradingAnInstall(t *testing.T, backend store.Backend) {
	dir := t.TempDir()
	t.Setenv(catalog.DirEnv, dir)
	write := func(release int, version, image, extra string) {
		t.Helper()
		if err := os.WriteFile(filepath.Join(dir, "thing.yaml"), []byte(testEntry(release, version, image, extra)), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	const first, second = "thing:1@sha256:" + digest1, "thing:2@sha256:" + digest2
	write(1, "1.0", first, "")

	f := startFull(t, backend, nil)
	b := signedIn(t, f)
	e := f.addDockerServer(t)
	pullsAnything(e)
	c := changeClient(b, b.origin)
	cat := catalogClient(b)
	installed := func() *dockwellv1.CatalogInstall {
		t.Helper()
		res, err := cat.ListCatalogInstalls(t.Context(), connect.NewRequest(&dockwellv1.ListCatalogInstallsRequest{}))
		if err != nil {
			t.Fatal(err)
		}
		if len(res.Msg.GetInstalls()) != 1 {
			t.Fatalf("installs: %v", res.Msg.GetInstalls())
		}
		return res.Msg.GetInstalls()[0]
	}
	apply(t, c, installApp(t, cat, &dockwellv1.InstallCatalogEntryRequest{Id: "thing"}))
	var env string
	for _, r := range mustList(t, b).GetResources() {
		env = r.GetId()
	}
	web := e.Newest(docker.Project(env), "web")
	var password string
	for _, kv := range web.Config.Env {
		if v, ok := strings.CutPrefix(kv, "PASSWORD="); ok {
			password = v
		}
	}
	if len(password) != 32 {
		t.Fatalf("a password of %d characters", len(password))
	}

	if installed().GetUpgrade() != nil {
		t.Error("an install of the newest release has an upgrade")
	}
	_, err := cat.UpgradeCatalogInstall(t.Context(), connect.NewRequest(&dockwellv1.UpgradeCatalogInstallRequest{Id: "thing"}))
	if connect.CodeOf(err) != connect.CodeFailedPrecondition || reasonOf(err) != "up_to_date" {
		t.Errorf("upgrading what's up to date = %v", err)
	}

	// A person adds a port, which the entry doesn't know of.
	services := dockwellv1connect.NewServiceServiceClient(b, b.origin, connect.WithProtoJSON())
	added, err := services.AddServicePort(t.Context(), connect.NewRequest(&dockwellv1.AddServicePortRequest{Id: "web", HostPort: 18200, ContainerPort: 8080, Project: "thing"}))
	if err != nil {
		t.Fatal(err)
	}
	apply(t, c, added.Msg.GetChange())

	write(2, "2.0", second, "")
	if got := installed().GetUpgrade(); got.GetVersion() != "2.0" || got.GetRelease() != 2 {
		t.Errorf("a newer release reads as %v", got)
	}
	res, err := cat.UpgradeCatalogInstall(t.Context(), connect.NewRequest(&dockwellv1.UpgradeCatalogInstallRequest{Id: "thing"}))
	if err != nil {
		t.Fatal(err)
	}
	planned := res.Msg.GetChange()
	if want := "Upgrade thing from 1.0 to 2.0"; planned.GetSummary() != want {
		t.Errorf("summary %q, want %q", planned.GetSummary(), want)
	}
	var said string
	for _, n := range planned.GetPlan().GetNotes() {
		if n.GetReason() == "backup_first" {
			said = n.GetMessage()
		}
		if n.GetReason() == "updates_project" {
			t.Errorf("an upgrade reads as a file landing on a project: %s", n.GetMessage())
		}
	}
	if !strings.Contains(said, "backs up this app's volumes before it changes anything") {
		t.Errorf("the plan doesn't say a backup comes first: %v", planned.GetPlan().GetNotes())
	}
	if strings.Contains(planned.String(), password) {
		t.Error("the plan holds the password")
	}
	if planned.GetPlan().GetDestructive() {
		t.Errorf("an upgrade that keeps its volume is destructive: %v", planned.GetPlan())
	}
	if got := apply(t, c, planned); got.GetState() != dockwellv1.ChangeState_CHANGE_STATE_APPLIED {
		t.Fatalf("the upgrade ended %v: %s", got.GetState(), got.GetError())
	}
	web = e.Newest(docker.Project(env), "web")
	if web.Config.Image != second {
		t.Errorf("after the upgrade it runs %s", web.Config.Image)
	}
	if !slices.Contains(web.Config.Env, "PASSWORD="+password) {
		t.Errorf("the upgrade changed the password: %v", web.Config.Env)
	}
	for port, want := range map[string]string{"80/tcp": "18100", "8080/tcp": "18200"} {
		if got := web.HostConfig.PortBindings[port]; len(got) != 1 || got[0].HostPort != want {
			t.Errorf("after the upgrade, port %s is %+v, want %s", port, got, want)
		}
	}
	if got := installed(); got.GetUpgrade() != nil || got.GetPin().GetVersion() != "2.0" || got.GetPin().GetRelease() != 2 || got.GetPin().GetValues()["PORT"] != "18100" {
		t.Errorf("after the upgrade, the install reads %v", got)
	}

	// An undo puts back the version and the pin, so it offers the upgrade again.
	undo, err := c.UndoChange(t.Context(), connect.NewRequest(&dockwellv1.UndoChangeRequest{Id: planned.GetId()}))
	if err != nil {
		t.Fatal(err)
	}
	if got := apply(t, c, undo.Msg.GetChange()); got.GetState() != dockwellv1.ChangeState_CHANGE_STATE_APPLIED {
		t.Fatalf("the undo ended %v: %s", got.GetState(), got.GetError())
	}
	if web = e.Newest(docker.Project(env), "web"); web.Config.Image != first {
		t.Errorf("after the undo it runs %s", web.Config.Image)
	}
	if got := installed(); got.GetPin().GetRelease() != 1 || got.GetUpgrade().GetRelease() != 2 {
		t.Errorf("after the undo, the install reads %v", got)
	}

	// A release that adds a service runs it, and its volume.
	const cache = "    cache:\n      image: cache:1@sha256:" + digest1 + "\n      volumes:\n        - cached:/cache\n"
	withVolume := strings.Replace(testEntry(3, "3.0", second, cache), "    data: {}\n", "    data: {}\n    cached: {}\n", 1)
	if err := os.WriteFile(filepath.Join(dir, "thing.yaml"), []byte(withVolume), 0o600); err != nil {
		t.Fatal(err)
	}
	res, err = cat.UpgradeCatalogInstall(t.Context(), connect.NewRequest(&dockwellv1.UpgradeCatalogInstallRequest{Id: "thing"}))
	if err != nil {
		t.Fatal(err)
	}
	if got := apply(t, c, res.Msg.GetChange()); got.GetState() != dockwellv1.ChangeState_CHANGE_STATE_APPLIED {
		t.Fatalf("the upgrade ended %v: %s", got.GetState(), got.GetError())
	}
	if e.Newest(docker.Project(env), "cache") == nil {
		t.Errorf("the upgrade didn't add the service its release adds: %v", e.Containers())
	}
	// And one that drops it takes it away, and leaves its data.
	write(4, "4.0", second, "")
	res, err = cat.UpgradeCatalogInstall(t.Context(), connect.NewRequest(&dockwellv1.UpgradeCatalogInstallRequest{Id: "thing"}))
	if err != nil {
		t.Fatal(err)
	}
	if res.Msg.GetChange().GetPlan().GetDestructive() {
		t.Errorf("an upgrade that drops a service and keeps its volume is destructive: %v", res.Msg.GetChange().GetPlan())
	}
	if got := apply(t, c, res.Msg.GetChange()); got.GetState() != dockwellv1.ChangeState_CHANGE_STATE_APPLIED {
		t.Fatalf("the upgrade ended %v: %s", got.GetState(), got.GetError())
	}
	if gone := e.Newest(docker.Project(env), "cache"); gone != nil {
		t.Errorf("the service its release dropped runs on: %+v", gone.Name)
	}
	if e.Volume(docker.Project(env)+"_cached") == nil {
		t.Error("dropping the service took its volume")
	}
}

// upgrading is an install of the test entry, ready to take a person's changes and a
// release.
type upgrading struct {
	t   *testing.T
	dir string
	e   *dockertest.Engine
	c   dockwellv1connect.ChangeServiceClient
	cat dockwellv1connect.CatalogServiceClient
	b   *browser
	f   *fullServer
	env string
}

const firstRelease, secondRelease = "thing:1@sha256:" + digest1, "thing:2@sha256:" + digest2

func startUpgrading(t *testing.T, backend store.Backend, entry string) *upgrading {
	t.Helper()
	return startUpgradingWith(t, backend, entry, &dockwellv1.InstallCatalogEntryRequest{Id: "thing"})
}

// startUpgradingWith is startUpgrading with an install request of its own.
func startUpgradingWith(t *testing.T, backend store.Backend, entry string, install *dockwellv1.InstallCatalogEntryRequest) *upgrading {
	t.Helper()
	u := &upgrading{t: t, dir: t.TempDir()}
	t.Setenv(catalog.DirEnv, u.dir)
	u.release(entry)
	f := startFull(t, backend, nil)
	u.b = signedIn(t, f)
	u.f = f
	u.e = f.addDockerServer(t)
	pullsAnything(u.e)
	u.c = changeClient(u.b, u.b.origin)
	u.cat = catalogClient(u.b)
	apply(t, u.c, installApp(t, u.cat, install))
	for _, r := range mustList(t, u.b).GetResources() {
		u.env = r.GetId()
	}
	return u
}

func (u *upgrading) release(entry string) {
	u.t.Helper()
	if err := os.WriteFile(filepath.Join(u.dir, "thing.yaml"), []byte(entry), 0o600); err != nil {
		u.t.Fatal(err)
	}
}

func (u *upgrading) web() *docker.Container {
	u.t.Helper()
	return u.e.Newest(docker.Project(u.env), "web")
}

func (u *upgrading) upgrade() *dockwellv1.Change {
	u.t.Helper()
	res, err := u.cat.UpgradeCatalogInstall(u.t.Context(), connect.NewRequest(&dockwellv1.UpgradeCatalogInstallRequest{Id: "thing"}))
	if err != nil {
		u.t.Fatalf("upgrade: %v", err)
	}
	return res.Msg.GetChange()
}

func (u *upgrading) applied(ch *dockwellv1.Change) {
	u.t.Helper()
	if got := apply(u.t, u.c, ch); got.GetState() != dockwellv1.ChangeState_CHANGE_STATE_APPLIED {
		u.t.Fatalf("%s ended %v: %s", ch.GetSummary(), got.GetState(), got.GetError())
	}
}

func (u *upgrading) edit(summary, name string, spec *resourcev1.Service) {
	u.t.Helper()
	p, err := u.c.ProposeChange(u.t.Context(), connect.NewRequest(&dockwellv1.ProposeChangeRequest{Summary: summary, Edits: []*dockwellv1.Edit{{
		Resource: &dockwellv1.ResourceRef{Kind: "service", Scope: u.env, Name: name},
		Spec:     &resourcev1.Spec{Kind: &resourcev1.Spec_Service{Service: spec}},
	}}}))
	if err != nil {
		u.t.Fatal(err)
	}
	u.applied(p.Msg.GetChange())
}

func (u *upgrading) install() *dockwellv1.CatalogInstall {
	u.t.Helper()
	res, err := u.cat.ListCatalogInstalls(u.t.Context(), connect.NewRequest(&dockwellv1.ListCatalogInstallsRequest{}))
	if err != nil {
		u.t.Fatal(err)
	}
	return res.Msg.GetInstalls()[0]
}

func planHas(ch *dockwellv1.Change, kind, name string) bool {
	return slices.ContainsFunc(ch.GetPlan().GetResources(), func(r *dockwellv1.PlannedResource) bool {
		return r.GetResource().GetKind() == kind && r.GetResource().GetName() == name && r.GetAction() != dockwellv1.PlanAction_PLAN_ACTION_NONE
	})
}

// A person's own app in the install's environment isn't the file's, so an upgrade
// leaves it alone, and an undo too.
func TestAnUpgradeLeavesAPersonsAppAlone(t *testing.T) {
	storetest.Each(t, testAnUpgradeLeavesAPersonsAppAlone)
}

func testAnUpgradeLeavesAPersonsAppAlone(t *testing.T, backend store.Backend) {
	u := startUpgrading(t, backend, testEntry(1, "1.0", firstRelease, ""))
	u.edit("My app", "mine", &resourcev1.Service{Runs: &resourcev1.Service_App{App: &resourcev1.Container{Image: "mine:1"}}})
	u.release(testEntry(2, "2.0", secondRelease, ""))
	up := u.upgrade()
	if planHas(up, "service", "mine") {
		t.Errorf("the upgrade plans to change a person's app: %v", up.GetPlan().GetResources())
	}
	u.applied(up)
	if u.e.Newest(docker.Project(u.env), "mine") == nil {
		t.Error("the upgrade took a person's app away")
	}
	if u.web().Config.Image != secondRelease {
		t.Errorf("it runs %s", u.web().Config.Image)
	}
}

// What a person changed stays through an upgrade, and through its undo.
func TestAnUpgradeKeepsWhatAPersonChanged(t *testing.T) {
	storetest.Each(t, testAnUpgradeKeepsWhatAPersonChanged)
}

func testAnUpgradeKeepsWhatAPersonChanged(t *testing.T, backend store.Backend) {
	u := startUpgrading(t, backend, testEntry(1, "1.0", firstRelease, ""))
	spec := func(image string) *resourcev1.Service {
		return &resourcev1.Service{
			Secrets: map[string]string{"PASSWORD": "PASSWORD"},
			Runs: &resourcev1.Service_App{App: &resourcev1.Container{
				Image: image, Networks: []string{"default"}, Restart: resourcev1.RestartPolicy_RESTART_POLICY_UNLESS_STOPPED,
				Ports:     []*resourcev1.PortMapping{{HostPort: 18100, ContainerPort: 80, HostIp: "127.0.0.1"}},
				Mounts:    []*resourcev1.Mount{{Source: &resourcev1.Mount_Volume{Volume: "data"}, Target: "/data"}},
				Resources: &resourcev1.Resources{Memory: "512M"},
				Command:   []string{"serve", "--tuned"},
			}},
		}
	}
	u.edit("Close it", "web", spec(firstRelease))
	check := func(when, image string) {
		t.Helper()
		web := u.web()
		if web.Config.Image != image {
			t.Errorf("%s it runs %s", when, web.Config.Image)
		}
		if got := web.HostConfig.PortBindings["80/tcp"]; len(got) != 1 || got[0].HostIP != "127.0.0.1" {
			t.Errorf("%s port 80 binds %+v, want loopback alone", when, got)
		}
		if web.HostConfig.Memory != 512<<20 || !slices.Equal(web.Config.Cmd, []string{"serve", "--tuned"}) {
			t.Errorf("%s its memory is %d and its command %v", when, web.HostConfig.Memory, web.Config.Cmd)
		}
	}
	u.release(testEntry(2, "2.0", secondRelease, ""))
	up := u.upgrade()
	u.applied(up)
	check("after the upgrade", secondRelease)
	undo, err := u.c.UndoChange(t.Context(), connect.NewRequest(&dockwellv1.UndoChangeRequest{Id: up.GetId()}))
	if err != nil {
		t.Fatal(err)
	}
	u.applied(undo.Msg.GetChange())
	check("after the undo", firstRelease)
}

// What an install sets of a service's memory, CPUs and restarts is what it runs with,
// in place of the entry's, and an upgrade keeps it, as it keeps what a person changes
// on the app. The entry's own show on its parts, and a service it doesn't have is
// refused by name.
func TestAnInstallsLimitsLastThroughItsUpgrades(t *testing.T) {
	storetest.Each(t, testAnInstallsLimitsLastThroughItsUpgrades)
}

func testAnInstallsLimitsLastThroughItsUpgrades(t *testing.T, backend store.Backend) {
	u := startUpgradingWith(t, backend, testEntry(1, "1.0", firstRelease, ""), &dockwellv1.InstallCatalogEntryRequest{
		Id:      "thing",
		Memory:  map[string]string{"web": "1G"},
		Cpus:    map[string]string{"web": "0.5"},
		Restart: map[string]resourcev1.RestartPolicy{"web": resourcev1.RestartPolicy_RESTART_POLICY_ALWAYS},
	})
	check := func(when, image string) {
		t.Helper()
		web := u.web()
		if web.Config.Image != image || web.HostConfig.Memory != 1<<30 || web.HostConfig.NanoCPUs != 5e8 || web.HostConfig.RestartPolicy.Name != "always" {
			t.Errorf("%s it runs %s with %d bytes, %d nano-CPUs and restart %q, want 1G, 0.5 and always",
				when, web.Config.Image, web.HostConfig.Memory, web.HostConfig.NanoCPUs, web.HostConfig.RestartPolicy.Name)
		}
	}
	check("once installed", firstRelease)
	u.release(testEntry(2, "2.0", secondRelease, ""))
	u.applied(u.upgrade())
	check("after the upgrade", secondRelease)

	got, err := u.cat.GetCatalogEntry(t.Context(), connect.NewRequest(&dockwellv1.GetCatalogEntryRequest{Id: "thing"}))
	if err != nil {
		t.Fatal(err)
	}
	// Its file says no restart, which Compose reads as never.
	if web := got.Msg.GetParts()[0]; web.GetService() != "web" || web.GetMemory() != "" || web.GetRestart() != resourcev1.RestartPolicy_RESTART_POLICY_NO {
		t.Errorf("the entry's own read %q, memory %q and restart %v, want none and never", web.GetService(), web.GetMemory(), web.GetRestart())
	}
	_, err = u.cat.InstallCatalogEntry(t.Context(), connect.NewRequest(&dockwellv1.InstallCatalogEntryRequest{
		Id: "thing", Project: "thing-two", Memory: map[string]string{"nope": "1G"},
	}))
	if connect.CodeOf(err) != connect.CodeInvalidArgument || !strings.Contains(err.Error(), "no service called nope") || !strings.Contains(err.Error(), "Name one of web") {
		t.Errorf("limits for a service the entry doesn't have: %v", err)
	}
}

// A release can take away what it gave, and what a person took away doesn't come back.
func TestAReleaseCanTakeAwayWhatItGave(t *testing.T) {
	storetest.Each(t, testAReleaseCanTakeAwayWhatItGave)
}

func testAReleaseCanTakeAwayWhatItGave(t *testing.T, backend store.Backend) {
	had := func(release int, version, image string) string {
		s := testEntry(release, version, image, "")
		s = strings.Replace(s, `- "${PORT}:80"`, "- \"${PORT}:80\"\n        - \"18101:9090\"", 1)
		return strings.Replace(s, "PASSWORD: ${PASSWORD}", "PASSWORD: ${PASSWORD}\n        DEBUG_TRUST: all", 1)
	}
	u := startUpgrading(t, backend, had(1, "1.0", firstRelease))
	u.release(testEntry(2, "2.0", secondRelease, ""))
	u.applied(u.upgrade())
	web := u.web()
	if _, open := web.HostConfig.PortBindings["9090/tcp"]; open {
		t.Errorf("the port the release closed is open: %v", web.HostConfig.PortBindings)
	}
	if slices.Contains(web.Config.Env, "DEBUG_TRUST=all") {
		t.Errorf("the variable the release dropped is set: %v", web.Config.Env)
	}
}

// A new version that won't start hurts nothing a person set: the old one runs again with
// the password it had, the install stays on its release, and the upgrade is still there to try.
func TestAFailedUpgradeGoesBackToWhatRan(t *testing.T) {
	storetest.Each(t, testAFailedUpgradeGoesBackToWhatRan)
}

func testAFailedUpgradeGoesBackToWhatRan(t *testing.T, backend store.Backend) {
	u := startUpgrading(t, backend, testEntry(1, "1.0", firstRelease, ""))
	password := func() string {
		t.Helper()
		for _, kv := range u.web().Config.Env {
			if v, ok := strings.CutPrefix(kv, "PASSWORD="); ok {
				return v
			}
		}
		t.Fatal("web has no password")
		return ""
	}
	before := password()
	u.e.Find = func(ref string) (dockertest.Image, bool) {
		return dockertest.Image{Exits: strings.Contains(ref, "thing:2")}, true
	}
	u.release(testEntry(2, "2.0", secondRelease, ""))
	up := u.upgrade()
	if got := apply(t, u.c, up); got.GetState() != dockwellv1.ChangeState_CHANGE_STATE_FAILED {
		t.Fatalf("an upgrade that won't start ended %v, so this proves nothing", got.GetState())
	}
	var failed *dockwellv1.Change
	for deadline := time.Now().Add(10 * time.Second); time.Now().Before(deadline); time.Sleep(10 * time.Millisecond) {
		res, err := u.c.GetChange(t.Context(), connect.NewRequest(&dockwellv1.GetChangeRequest{Id: up.GetId()}))
		if err != nil {
			t.Fatal(err)
		}
		if failed = res.Msg.GetChange(); failed.GetRollbackStatus().GetState() == dockwellv1.RollbackState_ROLLBACK_STATE_APPLIED {
			break
		}
	}
	if got := failed.GetRollbackStatus().GetState(); got != dockwellv1.RollbackState_ROLLBACK_STATE_APPLIED {
		t.Fatalf("the failed upgrade's rollback reads %v", got)
	}
	if got := u.web(); got.Config.Image != firstRelease || got.State.Status != "running" {
		t.Errorf("web runs %s, %s, after the rollback", got.Config.Image, got.State.Status)
	}
	if password() != before {
		t.Error("the rollback changed web's password")
	}
	if got := u.install(); got.GetPin().GetRelease() != 1 || got.GetUpgrade().GetRelease() != 2 {
		t.Errorf("the install reads pin %d, upgrade %d, after a failed upgrade", got.GetPin().GetRelease(), got.GetUpgrade().GetRelease())
	}
	// The sheet says what this left in the data from what the plan said.
	if !slices.ContainsFunc(failed.GetPlan().GetNotes(), func(n *dockwellv1.PlanNote) bool { return n.GetReason() == "backup_first" }) {
		t.Errorf("the failed upgrade's plan lost its backup_first note: %v", failed.GetPlan().GetNotes())
	}
}

// Undoing an upgrade puts the old version back on what the new one left, so the plan says
// that before anyone applies it, and an undo of anything else doesn't.
func TestUndoingAnUpgradeWarnsOfItsData(t *testing.T) {
	storetest.Each(t, testUndoingAnUpgradeWarnsOfItsData)
}

func testUndoingAnUpgradeWarnsOfItsData(t *testing.T, backend store.Backend) {
	u := startUpgrading(t, backend, testEntry(1, "1.0", firstRelease, ""))
	var names []string
	said := func(id string) string {
		t.Helper()
		undo, err := u.c.UndoChange(t.Context(), connect.NewRequest(&dockwellv1.UndoChangeRequest{Id: id}))
		if err != nil {
			t.Fatal(err)
		}
		var out string
		for _, n := range undo.Msg.GetChange().GetPlan().GetNotes() {
			if n.GetReason() == "undo_backup_first" {
				out, names = n.GetMessage(), n.GetNames()
			}
		}
		if _, err := u.c.DiscardChange(t.Context(), connect.NewRequest(&dockwellv1.DiscardChangeRequest{Id: undo.Msg.GetChange().GetId()})); err != nil {
			t.Fatal(err)
		}
		return out
	}
	p, err := u.c.ProposeChange(t.Context(), connect.NewRequest(&dockwellv1.ProposeChangeRequest{Summary: "Add one", Edits: []*dockwellv1.Edit{{
		Resource: &dockwellv1.ResourceRef{Kind: "service", Scope: u.env, Name: "mine"},
		Spec:     &resourcev1.Spec{Kind: &resourcev1.Spec_Service{Service: &resourcev1.Service{Runs: &resourcev1.Service_App{App: &resourcev1.Container{Image: "mine:1"}}}}},
	}}}))
	if err != nil {
		t.Fatal(err)
	}
	u.applied(p.Msg.GetChange())
	if got := said(p.Msg.GetChange().GetId()); got != "" {
		t.Errorf("an undo of another change warns of data: %s", got)
	}
	u.release(testEntry(2, "2.0", secondRelease, ""))
	up := u.upgrade()
	u.applied(up)
	bc := backupClient(u.b, u.b.origin)
	taken := backupsOf(t, bc)
	if len(taken) != 1 {
		t.Fatalf("backups = %v", taken)
	}
	id := taken[0].GetId()
	got := said(up.GetId())
	when := taken[0].GetStartedAt().AsTime().Format("2006-01-02 15:04") + " UTC"
	for _, want := range []string{"may refuse it or misread it", "backed it up first, in " + id, when, "restore that from Backups"} {
		if !strings.Contains(got, want) {
			t.Errorf("an undo of an upgrade says %q, with no %q", got, want)
		}
	}

	// A surface with words of its own gets when, then which.
	if at, err := time.Parse(time.RFC3339, names[0]); err != nil || !at.Equal(taken[0].GetStartedAt().AsTime().Truncate(time.Second)) || !slices.Equal(names[1:], []string{id}) {
		t.Errorf("an undo of an upgrade names %v, %v", names, err)
	}

	// Once it's gone, the undo can't point at it.
	if _, err := bc.DeleteBackup(t.Context(), connect.NewRequest(&dockwellv1.DeleteBackupRequest{Id: id})); err != nil {
		t.Fatal(err)
	}
	if got := said(up.GetId()); !strings.Contains(got, "took first is gone") || strings.Contains(got, id) {
		t.Errorf("with its backup deleted, an undo says %q", got)
	}
}

// An upgrade backs up what it may change before it touches anything, as a step of its
// plan. A backup that fails stops it there, and asking for none says so.
func TestAnUpgradeBacksUpFirst(t *testing.T) { storetest.Each(t, testAnUpgradeBacksUpFirst) }

func testAnUpgradeBacksUpFirst(t *testing.T, backend store.Backend) {
	u := startUpgrading(t, backend, testEntry(1, "1.0", firstRelease, ""))
	bc := backupClient(u.b, u.b.origin)
	u.release(testEntry(2, "2.0", secondRelease, ""))

	up := u.upgrade()
	steps := up.GetPlan().GetServers()[0].GetSteps()
	first := slices.IndexFunc(steps, func(s *dockwellv1.PlanStep) bool { return s.GetBackupFirst() != nil })
	replace := slices.IndexFunc(steps, func(s *dockwellv1.PlanStep) bool { return s.GetAction() == dockwellv1.PlanAction_PLAN_ACTION_REPLACE })
	if first < 0 || replace < 0 || first > replace {
		t.Fatalf("the backup isn't before what it protects: %v", steps)
	}
	if up.GetPlan().GetDestructive() {
		t.Error("a backup first makes the plan read as destructive")
	}

	// A backup that can't run stops it there, with nothing changed.
	binary := u.f.lastDocker.backups.Binary
	u.f.lastDocker.backups.Binary = filepath.Join(t.TempDir(), "gone")
	w := watch(t, u.c, up.GetId())
	if _, err := u.c.ApplyChange(t.Context(), connect.NewRequest(&dockwellv1.ApplyChangeRequest{Id: up.GetId()})); err != nil {
		t.Fatal(err)
	}
	failed := waitWatch(t, w).last()
	u.f.lastDocker.backups.Binary = binary
	if failed.GetState() != dockwellv1.ChangeState_CHANGE_STATE_FAILED || !strings.Contains(failed.GetError(), "nothing else in this change ran") {
		t.Fatalf("the upgrade ended %v: %s", failed.GetState(), failed.GetError())
	}
	if got := u.web(); got.Config.Image != firstRelease {
		t.Errorf("a failed backup let the upgrade run: web runs %s", got.Config.Image)
	}

	// Planned again, it takes the backup and goes on.
	again := u.upgrade()
	u.applied(again)
	if got := u.web(); got.Config.Image != secondRelease {
		t.Errorf("web runs %s", got.Config.Image)
	}
	res, err := bc.ListBackups(t.Context(), connect.NewRequest(&dockwellv1.ListBackupsRequest{}))
	if err != nil {
		t.Fatal(err)
	}
	if got := res.Msg.GetBackups(); len(got) != 1 || got[0].GetState() != dockwellv1.BackupState_BACKUP_STATE_DONE || got[0].GetSizeBytes() == 0 {
		t.Fatalf("after the upgrade, backups = %v", got)
	}
}

func TestAnUpgradeCanSkipItsBackupAndSaysSo(t *testing.T) {
	storetest.Each(t, func(t *testing.T, backend store.Backend) {
		u := startUpgrading(t, backend, testEntry(1, "1.0", firstRelease, ""))
		u.release(testEntry(2, "2.0", secondRelease, ""))
		res, err := u.cat.UpgradeCatalogInstall(t.Context(), connect.NewRequest(&dockwellv1.UpgradeCatalogInstallRequest{Id: "thing", SkipBackup: true}))
		if err != nil {
			t.Fatal(err)
		}
		plan := res.Msg.GetChange().GetPlan()
		if !slices.ContainsFunc(plan.GetNotes(), func(n *dockwellv1.PlanNote) bool {
			return n.GetReason() == "no_backup" && strings.Contains(n.GetMessage(), "takes no backup, as you asked")
		}) {
			t.Errorf("notes = %v", plan.GetNotes())
		}
		if slices.ContainsFunc(plan.GetServers()[0].GetSteps(), func(s *dockwellv1.PlanStep) bool { return s.GetBackupFirst() != nil }) {
			t.Error("it backs up what it was told not to")
		}
	})
}

// A release that changes nothing that runs, such as a typo in its words, offers nothing,
// and one that changes what runs at the same version still does.
func TestAReleaseOffersAnUpgradeOnlyIfWhatRunsChanges(t *testing.T) {
	storetest.Each(t, testAReleaseOffersAnUpgradeOnlyIfWhatRunsChanges)
}

func testAReleaseOffersAnUpgradeOnlyIfWhatRunsChanges(t *testing.T, backend store.Backend) {
	u := startUpgrading(t, backend, testEntry(1, "1.0", firstRelease, ""))
	install := u.install
	u.release(strings.Replace(testEntry(2, "1.0", firstRelease, ""), "Something to upgrade", "Something to upgrade, spelled right", 1))
	if got := install().GetUpgrade(); got != nil {
		t.Errorf("a release that changes nothing that runs offers %v", got)
	}
	_, err := u.cat.UpgradeCatalogInstall(t.Context(), connect.NewRequest(&dockwellv1.UpgradeCatalogInstallRequest{Id: "thing"}))
	if reasonOf(err) != "up_to_date" {
		t.Errorf("upgrading to a release that changes nothing = %v", err)
	}
	u.release(testEntry(3, "1.0", secondRelease, ""))
	if got := install().GetUpgrade(); got.GetRelease() != 3 || got.GetVersion() != "1.0" {
		t.Errorf("a release that changes what runs at one version offers %v", got)
	}
}

// An install from before Dockwell kept its file has nothing to merge from, so it stays
// as it is, and says why.
func TestAnInstallWithNoRecordStaysAsItIs(t *testing.T) {
	storetest.Each(t, testAnInstallWithNoRecordStaysAsItIs)
}

func testAnInstallWithNoRecordStaysAsItIs(t *testing.T, backend store.Backend) {
	u := startUpgrading(t, backend, testEntry(1, "1.0", firstRelease, ""))
	p, err := u.c.ProposeChange(t.Context(), connect.NewRequest(&dockwellv1.ProposeChangeRequest{Summary: "As it was", Edits: []*dockwellv1.Edit{{
		Resource: &dockwellv1.ResourceRef{Kind: "project", Name: "thing"},
		Spec: &resourcev1.Spec{Kind: &resourcev1.Spec_Project{Project: &resourcev1.Project{Catalog: &resourcev1.CatalogPin{
			Entry: "thing", Version: "1.0", Release: 1, Values: map[string]string{"PORT": "18100"},
		}}}},
	}}}))
	if err != nil {
		t.Fatal(err)
	}
	u.applied(p.Msg.GetChange())
	u.release(testEntry(2, "2.0", secondRelease, ""))
	_, err = u.cat.UpgradeCatalogInstall(t.Context(), connect.NewRequest(&dockwellv1.UpgradeCatalogInstallRequest{Id: "thing"}))
	if connect.CodeOf(err) != connect.CodeFailedPrecondition || reasonOf(err) != "no_record" || !strings.Contains(err.Error(), "can't tell that from what you changed") {
		t.Errorf("upgrading with no record = %v", err)
	}
	if u.web().Config.Image != firstRelease {
		t.Errorf("it runs %s", u.web().Config.Image)
	}
}

const (
	digest1 = "1111111111111111111111111111111111111111111111111111111111111111"
	digest2 = "2222222222222222222222222222222222222222222222222222222222222222"
)

// mustList lists the one environment of the project "thing".
func mustList(t *testing.T, b *browser) *dockwellv1.ListResourcesResponse {
	t.Helper()
	r := dockwellv1connect.NewResourceServiceClient(b, b.origin, connect.WithProtoJSON())
	projects, err := r.ListResources(t.Context(), connect.NewRequest(&dockwellv1.ListResourcesRequest{Kind: "project"}))
	if err != nil {
		t.Fatal(err)
	}
	for _, p := range projects.Msg.GetResources() {
		if p.GetResource().GetName() == "thing" {
			envs, err := r.ListResources(t.Context(), connect.NewRequest(&dockwellv1.ListResourcesRequest{Kind: "environment", Scope: p.GetId()}))
			if err != nil {
				t.Fatal(err)
			}
			return envs.Msg
		}
	}
	t.Fatal("no project thing")
	return nil
}

// A database's admin tool is left out unless a person turns it on: the install runs
// the database alone, the pin says so, and the tool comes and goes later as a plan,
// each with an undo, at the release it's on.
func TestAnInstallsCompanionsComeAndGo(t *testing.T) {
	f := startFull(t, store.SQLite, nil)
	b := signedIn(t, f)
	e := f.addDockerServer(t)
	pullsAnything(e)
	c := changeClient(b, b.origin)
	cat := catalogClient(b)

	planned := installApp(t, cat, &dockwellv1.InstallCatalogEntryRequest{Id: "postgres"})
	for _, r := range planned.GetPlan().GetResources() {
		if r.GetResource().GetName() == "adminer" {
			t.Errorf("left out, the plan still makes %s %s", r.GetResource().GetKind(), r.GetResource().GetName())
		}
	}
	if got := apply(t, c, planned); got.GetState() != dockwellv1.ChangeState_CHANGE_STATE_APPLIED {
		t.Fatalf("the install ended %v: %s", got.GetState(), got.GetError())
	}
	var env string
	for _, r := range planned.GetPlan().GetResources() {
		if r.GetResource().GetKind() == "service" {
			env = r.GetResource().GetScope()
		}
	}
	project := docker.Project(env)
	installed := func() *dockwellv1.CatalogInstall {
		t.Helper()
		res, err := cat.ListCatalogInstalls(t.Context(), connect.NewRequest(&dockwellv1.ListCatalogInstallsRequest{}))
		if err != nil || len(res.Msg.GetInstalls()) != 1 {
			t.Fatalf("installs: %v, %v", res, err)
		}
		return res.Msg.GetInstalls()[0]
	}
	if got := installed(); got.GetCompanions()["adminer"] || got.GetPin().GetCompanions()["adminer"] ||
		len(got.GetPin().GetCompanions()) != 1 || got.GetPin().GetValues()["ADMIN_PORT"] != "" || strings.Contains(got.GetFirstSteps(), "dminer") {
		t.Errorf("left out, the install reads %v", got)
	}
	if e.Container(project+"-adminer-1") != nil {
		t.Error("left out, Adminer runs")
	}
	set := func(on bool, values map[string]string) (*dockwellv1.Change, error) {
		res, err := cat.SetCatalogCompanion(t.Context(), connect.NewRequest(&dockwellv1.SetCatalogCompanionRequest{Id: "postgres", Service: "adminer", On: on, Values: values}))
		if err != nil {
			return nil, err
		}
		return res.Msg.GetChange(), nil
	}
	if _, err := set(false, nil); connect.CodeOf(err) != connect.CodeFailedPrecondition || reasonOf(err) != "unchanged" {
		t.Errorf("taking away what isn't there = %v", err)
	}
	if _, err := cat.SetCatalogCompanion(t.Context(), connect.NewRequest(&dockwellv1.SetCatalogCompanionRequest{Id: "postgres", Service: "pgweb", On: true})); connect.CodeOf(err) != connect.CodeInvalidArgument {
		t.Errorf("a companion it hasn't = %v", err)
	}

	// Added, it takes the port it's given, and nothing else changes.
	add, err := set(true, map[string]string{"ADMIN_PORT": "18081"})
	if err != nil {
		t.Fatal(err)
	}
	if add.GetSummary() != "Add Adminer to postgres" {
		t.Errorf("adding reads %q", add.GetSummary())
	}
	for _, r := range add.GetPlan().GetResources() {
		switch name := r.GetResource().GetKind() + " " + r.GetResource().GetName(); {
		case name == "service adminer" && r.GetAction() != dockwellv1.PlanAction_PLAN_ACTION_CREATE:
			t.Errorf("adding plans %v for Adminer", r.GetAction())
		case name != "service adminer" && name != "project postgres" && r.GetAction() != dockwellv1.PlanAction_PLAN_ACTION_NONE:
			t.Errorf("adding Adminer also plans %v for %s", r.GetAction(), name)
		}
	}
	for _, n := range add.GetPlan().GetNotes() {
		if n.GetReason() == "backup_first" || n.GetReason() == "no_backup" {
			t.Errorf("adding a companion talks of a backup: %v", n)
		}
	}
	if got := apply(t, c, add); got.GetState() != dockwellv1.ChangeState_CHANGE_STATE_APPLIED {
		t.Fatalf("adding Adminer ended %v: %s", got.GetState(), got.GetError())
	}
	adminer := e.Container(project + "-adminer-1")
	if adminer == nil {
		t.Fatalf("added, Adminer doesn't run: %v", e.Containers())
	}
	if got := adminer.HostConfig.PortBindings["8080/tcp"]; len(got) != 1 || got[0].HostPort != "18081" {
		t.Errorf("added, Adminer publishes %+v", got)
	}
	if got := installed(); !got.GetCompanions()["adminer"] || !got.GetPin().GetCompanions()["adminer"] || got.GetPin().GetValues()["ADMIN_PORT"] != "18081" {
		t.Errorf("added, the install reads %v", got)
	}
	if _, err := set(true, nil); reasonOf(err) != "unchanged" {
		t.Errorf("adding what's there = %v", err)
	}

	// Taken away, it goes, and the database stays as it was.
	db := e.Newest(project, "db")
	remove, err := set(false, nil)
	if err != nil {
		t.Fatal(err)
	}
	if remove.GetSummary() != "Remove Adminer from postgres" {
		t.Errorf("taking it away reads %q", remove.GetSummary())
	}
	applyConfirmed(t, c, remove)
	if e.Container(project+"-adminer-1") != nil {
		t.Error("taken away, Adminer still runs")
	}
	if got := e.Newest(project, "db"); got == nil || got.ID != db.ID {
		t.Error("taking Adminer away replaced the database's container")
	}
	if got := installed(); got.GetCompanions()["adminer"] || got.GetPin().GetValues()["ADMIN_PORT"] != "" {
		t.Errorf("taken away, the install reads %v", got)
	}
}

// An install from before companions keeps what it ran through an upgrade to a release
// that marks one; a new install of that release leaves it out, and keeps it out.
func TestAnUpgradeKeepsTheCompanionsChosen(t *testing.T) {
	dir := t.TempDir()
	t.Setenv(catalog.DirEnv, dir)
	tool := "    tool:\n      image: tool:1@sha256:" + digest1 + "\n      ports:\n        - \"${TOOL_PORT}:80\"\n"
	write := func(release int, image string, companion bool) {
		t.Helper()
		entry := testEntry(release, fmt.Sprintf("%d.0", release), image, tool)
		entry = strings.Replace(entry, "inputs:\n", "inputs:\n  - name: TOOL_PORT\n    label: Tool port\n    type: INPUT_TYPE_PORT\n    defaultValue: \"18300\"\n", 1)
		if companion {
			entry = strings.Replace(entry, "compose: |\n", "companions:\n  - service: tool\n    label: Tool\n    purpose: Something beside it\n    inputs: [TOOL_PORT]\ncompose: |\n", 1)
		}
		if err := os.WriteFile(filepath.Join(dir, "thing.yaml"), []byte(entry), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	write(1, "thing:1@sha256:"+digest1, false)
	f := startFull(t, store.SQLite, nil)
	b := signedIn(t, f)
	e := f.addDockerServer(t)
	pullsAnything(e)
	c := changeClient(b, b.origin)
	cat := catalogClient(b)
	install := func(project string) string {
		t.Helper()
		planned := installApp(t, cat, &dockwellv1.InstallCatalogEntryRequest{Id: "thing", Project: project})
		if got := apply(t, c, planned); got.GetState() != dockwellv1.ChangeState_CHANGE_STATE_APPLIED {
			t.Fatalf("installing %s ended %v: %s", project, got.GetState(), got.GetError())
		}
		for _, r := range planned.GetPlan().GetResources() {
			if r.GetResource().GetKind() == "service" {
				return docker.Project(r.GetResource().GetScope())
			}
		}
		t.Fatal("the install planned no service")
		return ""
	}
	upgrade := func(project string) *dockwellv1.Change {
		t.Helper()
		res, err := cat.UpgradeCatalogInstall(t.Context(), connect.NewRequest(&dockwellv1.UpgradeCatalogInstallRequest{Id: project, SkipBackup: true}))
		if err != nil {
			t.Fatal(err)
		}
		return res.Msg.GetChange()
	}
	before := install("before")
	if e.Newest(before, "tool") == nil {
		t.Fatal("an entry with no companions doesn't run every service")
	}

	write(2, "thing:2@sha256:"+digest2, true)
	planned := upgrade("before")
	for _, r := range planned.GetPlan().GetResources() {
		if r.GetResource().GetName() == "tool" && r.GetAction() == dockwellv1.PlanAction_PLAN_ACTION_DELETE {
			t.Error("an upgrade takes away a companion an install from before companions ran")
		}
	}
	apply(t, c, planned)
	if e.Newest(before, "tool") == nil {
		t.Error("after the upgrade, the tool it ran is gone")
	}

	after := install("after")
	if e.Newest(after, "tool") != nil {
		t.Error("a new install runs a companion that's off by default")
	}
	write(3, "thing:1@sha256:"+digest1, true)
	planned = upgrade("after")
	for _, r := range planned.GetPlan().GetResources() {
		if r.GetResource().GetName() == "tool" {
			t.Errorf("an upgrade of an install that left the tool out plans %v for it", r.GetAction())
		}
	}
	apply(t, c, planned)
	if e.Newest(after, "tool") != nil {
		t.Error("after its upgrade, an install that left the tool out runs it")
	}
	res, err := cat.ListCatalogInstalls(t.Context(), connect.NewRequest(&dockwellv1.ListCatalogInstallsRequest{}))
	if err != nil {
		t.Fatal(err)
	}
	for _, in := range res.Msg.GetInstalls() {
		if want := in.GetProject() == "before"; in.GetCompanions()["tool"] != want || in.GetPin().GetCompanions()["tool"] != want {
			t.Errorf("%s reads tool as %v, pinned %v", in.GetProject(), in.GetCompanions(), in.GetPin().GetCompanions())
		}
	}
}
