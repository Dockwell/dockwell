// SPDX-License-Identifier: AGPL-3.0-only

package control

import (
	"os"
	"path/filepath"
	"regexp"
	"slices"
	"strings"
	"testing"

	"connectrpc.com/connect"
	"google.golang.org/protobuf/proto"

	resourcev1 "github.com/dockwell/dockwell/gen/dockwell/resource/v1alpha1"
	dockwellv1 "github.com/dockwell/dockwell/gen/dockwell/v1"
	"github.com/dockwell/dockwell/gen/dockwell/v1/dockwellv1connect"
	"github.com/dockwell/dockwell/internal/backup/s3/s3test"
	"github.com/dockwell/dockwell/internal/catalog"
	"github.com/dockwell/dockwell/internal/docker"
	"github.com/dockwell/dockwell/internal/docker/dockertest"
	"github.com/dockwell/dockwell/internal/store"
)

// publicEntry is the test entry with a second port, HTTP_PORT, published as a
// proxy's 80 is, which the entry declares public when public says so.
func publicEntry(release int, version, image string, public bool) string {
	return publicEntryWith(release, version, image, public, "")
}

// publicEntryWith is publicEntry with more services, as testEntry's extra.
func publicEntryWith(release int, version, image string, public bool, extra string) string {
	e := strings.Replace(testEntry(release, version, image, extra), "  - name: PASSWORD\n",
		"  - name: HTTP_PORT\n    label: HTTP port\n    type: INPUT_TYPE_PORT\n    defaultValue: \"18180\"\n  - name: PASSWORD\n", 1)
	if public {
		e = strings.Replace(e, "    defaultValue: \"18180\"\n", "    defaultValue: \"18180\"\n    public: true\n", 1)
	}
	return strings.Replace(e, "        - \"${PORT}:80\"\n", "        - \"${PORT}:80\"\n        - \"${HTTP_PORT}:8080\"\n", 1)
}

// startPublic is startUpgrading for an entry whose install may ask a person to
// approve a public port: applied with its plan's fingerprint, as the panel does.
func startPublic(t *testing.T, entry string) *upgrading {
	t.Helper()
	u := &upgrading{t: t, dir: t.TempDir()}
	useCatalogDir(t, u.dir)
	u.release(entry)
	u.f = startFull(t, store.SQLite, nil)
	u.b = signedIn(t, u.f)
	u.e = u.f.addDockerServer(t)
	pullsAnything(u.e)
	u.c = changeClient(u.b, u.b.origin)
	u.cat = catalogClient(u.b)
	applyReviewed(t, u.c, installApp(t, u.cat, &dockwellv1.InstallCatalogEntryRequest{Id: "thing"}))
	for _, r := range mustList(t, u.b).GetResources() {
		u.env = r.GetId()
	}
	return u
}

// stored is a service of the install as the store holds it.
func (u *upgrading) stored(name string) *resourcev1.Service {
	u.t.Helper()
	var spec proto.Message
	if err := u.f.store.View(u.t.Context(), func(tx *store.Tx) error {
		r, err := tx.GetResource(u.t.Context(), "service", u.env, name)
		if err != nil {
			return err
		}
		spec, err = u.f.keeper.Decode(u.t.Context(), tx, store.InRevision(r.ID, r.Kind, r.Revision), r.Spec)
		return err
	}); err != nil {
		u.t.Fatal(err)
	}
	return proto.CloneOf(spec.(*resourcev1.Service))
}

// addresses are the addresses web's ports are published on, by host port.
func (u *upgrading) addresses() map[uint32]string {
	u.t.Helper()
	out := map[uint32]string{}
	for _, p := range u.stored("web").GetApp().GetPorts() {
		out[p.GetHostPort()] = p.GetHostIp()
	}
	return out
}

// publicAccess are the public ports a plan asks a person to approve, as "service
// port".
func publicAccess(ch *dockwellv1.Change) []string {
	var out []string
	for _, r := range ch.GetPlan().GetResources() {
		for _, a := range r.GetHostAccess() {
			if a.GetKind() == dockwellv1.HostAccessKind_HOST_ACCESS_KIND_PUBLIC_PORT {
				out = append(out, a.GetDetail()+" "+a.GetPath())
			}
		}
	}
	slices.Sort(out)
	return out
}

// An install publishes each of a catalog app's ports on the server alone, as the
// floor checks, but one its entry declares public, which answers on every address,
// and which the plan asks a person to approve (ADR 0101).
func TestAnInstallsPortsAnswerOnTheServerAloneButThoseItDeclaresPublic(t *testing.T) {
	t.Parallel()
	u := startPublic(t, publicEntry(1, "1.0", firstRelease, true))
	if got := u.addresses(); got[18100] != "127.0.0.1" || got[18180] != "" {
		t.Errorf("web publishes %v, want 18100 on 127.0.0.1 and 18180 on every address", got)
	}
	// What the install planned asked a person for the public port alone.
	u.release(publicEntry(1, "1.0", firstRelease, true))
	planned := installApp(t, u.cat, &dockwellv1.InstallCatalogEntryRequest{Id: "thing", Project: "thing-two", Values: map[string]string{"PORT": "18101", "HTTP_PORT": "18181"}})
	if got := publicAccess(planned); !slices.Equal(got, []string{"web 18181"}) || !planned.GetPlan().GetHostAccess() {
		t.Errorf("the install asks for public ports %v (host access %v), want web 18181", got, planned.GetPlan().GetHostAccess())
	}
}

// An entry that declares nothing public publishes every port on the server alone,
// whatever its file says, and its plan asks for no public port (ADR 0101).
func TestAnInstallWithNothingPublicAsksForNoPublicPort(t *testing.T) {
	t.Parallel()
	u := startUpgrading(t, store.SQLite, publicEntry(1, "1.0", firstRelease, false))
	if got := u.addresses(); got[18100] != "127.0.0.1" || got[18180] != "127.0.0.1" {
		t.Errorf("web publishes %v, want both on 127.0.0.1", got)
	}
}

// An install whose console answered on every address, as one made before ADR 0101
// did, has it answer on the server alone after an upgrade, which the plan says, with
// how to reach it now; the port it declares public stays public and asks nothing
// again. The move itself leaves an automatic update for a person (N2).
func TestAnUpgradeMovesAConsoleToTheServerAloneAndSaysSo(t *testing.T) {
	t.Parallel()
	u := startPublic(t, publicEntry(1, "1.0", firstRelease, true))
	u.asBefore("web", publicEntry(1, "1.0", firstRelease, true))
	if got := u.addresses(); got[18100] != "" {
		t.Fatalf("web publishes %v before the upgrade", got)
	}
	u.release(publicEntry(2, "2.0", secondRelease, true))
	up := u.upgrade()
	n := noteOf(up, "port_to_server_alone")
	if want := "web's port 18100 answers on this server alone after this, not on every address. Reach it with the SSH tunnel on its page, or add a domain to web."; n.GetMessage() != want {
		t.Errorf("the plan's note: %q, want %q", n.GetMessage(), want)
	}
	if got := publicAccess(up); len(got) > 0 || up.GetPlan().GetHostAccess() {
		t.Errorf("the upgrade asks for public ports %v (host access %v), where they're public already", got, up.GetPlan().GetHostAccess())
	}
	u.applied(up)
	if got := u.addresses(); got[18100] != "127.0.0.1" || got[18180] != "" {
		t.Errorf("after the upgrade web publishes %v, want 18100 on 127.0.0.1 and 18180 on every address", got)
	}
}

// A release that declares a port public which the install publishes on the server
// alone asks a person to approve it in the upgrade's plan (ADR 0101).
func TestAReleaseThatDeclaresAPortPublicAsksForIt(t *testing.T) {
	t.Parallel()
	u := startUpgrading(t, store.SQLite, publicEntry(1, "1.0", firstRelease, false))
	u.release(publicEntry(2, "2.0", secondRelease, true))
	up := u.upgrade()
	if got := publicAccess(up); !slices.Equal(got, []string{"web 18180"}) || !up.GetPlan().GetHostAccess() {
		t.Errorf("the upgrade asks for public ports %v (host access %v), want web 18180", got, up.GetPlan().GetHostAccess())
	}
	if n := noteOf(up, "port_to_server_alone"); n != nil {
		t.Errorf("the plan moves a port to the server alone: %s", n.GetMessage())
	}
	applyReviewed(t, u.c, up)
	if got := u.addresses(); got[18100] != "127.0.0.1" || got[18180] != "" {
		t.Errorf("after the upgrade web publishes %v, want 18100 on 127.0.0.1 and 18180 on every address", got)
	}
}

// A port a person opened to every address on purpose, after the install, stays as
// they set it through an upgrade, and the plan doesn't say it moves (ADR 0101).
func TestAPortAPersonOpenedStaysThroughAnUpgrade(t *testing.T) {
	t.Parallel()
	u := startPublic(t, publicEntry(1, "1.0", firstRelease, true))
	svc := u.stored("web")
	for _, p := range svc.GetApp().GetPorts() {
		p.HostIp = ""
	}
	u.applied(propose(t, u.c, "Open the console", &dockwellv1.Edit{
		Resource: &dockwellv1.ResourceRef{Kind: "service", Scope: u.env, Name: "web"},
		Spec:     &resourcev1.Spec{Kind: &resourcev1.Spec_Service{Service: svc}},
	}))
	u.release(publicEntry(2, "2.0", secondRelease, true))
	up := u.upgrade()
	if n := noteOf(up, "port_to_server_alone"); n != nil {
		t.Errorf("the plan says a port the person opened moves: %s", n.GetMessage())
	}
	u.applied(up)
	if got := u.addresses(); got[18100] != "" {
		t.Errorf("after the upgrade web publishes %v, want 18100 on every address, as the person set it", got)
	}
}

// asBefore makes an install look as one made before ADR 0101 does: service's ports
// on every address, and the pin's file as entry wrote it, which said no address.
func (u *upgrading) asBefore(service, entry string) {
	u.t.Helper()
	u.asBeforeIn("thing", service, entry)
}

// asBeforeIn is asBefore for the install whose project is named project.
func (u *upgrading) asBeforeIn(project, service, entry string) {
	u.t.Helper()
	svc := u.stored(service)
	for _, p := range svc.GetApp().GetPorts() {
		p.HostIp = ""
	}
	raw, err := catalog.Parse([]byte(entry))
	if err != nil {
		u.t.Fatal(err)
	}
	var found store.Resource
	var spec proto.Message
	if err := u.f.store.View(u.t.Context(), func(tx *store.Tx) error {
		list, err := tx.ListKind(u.t.Context(), "project")
		for i := range list {
			if list[i].Name == project {
				found = list[i]
			}
		}
		if err != nil {
			return err
		}
		spec, err = u.f.keeper.Decode(u.t.Context(), tx, store.InRevision(found.ID, found.Kind, found.Revision), found.Spec)
		return err
	}); err != nil {
		u.t.Fatal(err)
	}
	p := proto.CloneOf(spec.(*resourcev1.Project))
	p.Catalog.File = []byte(raw.GetCompose())
	u.applied(propose(u.t, u.c, "As before", &dockwellv1.Edit{
		Resource: &dockwellv1.ResourceRef{Kind: "service", Scope: u.env, Name: service},
		Spec:     &resourcev1.Spec{Kind: &resourcev1.Spec_Service{Service: svc}},
	}, &dockwellv1.Edit{
		Resource: &dockwellv1.ResourceRef{Kind: "project", Scope: found.Scope, Name: found.Name},
		Spec:     &resourcev1.Spec{Kind: &resourcev1.Spec_Project{Project: p}},
	}))
}

// toolEntry is publicEntry with a companion, tool, on its own port.
func toolEntry(release int, version, image string) string {
	tool := "    tool:\n      image: tool:1@sha256:" + digest1 + "\n      ports:\n        - \"127.0.0.1:${TOOL_PORT}:80\"\n"
	e := publicEntryWith(release, version, image, true, tool)
	e = strings.Replace(e, "inputs:\n", "inputs:\n  - name: TOOL_PORT\n    label: Tool port\n    type: INPUT_TYPE_PORT\n    defaultValue: \"18300\"\n", 1)
	return strings.Replace(e, "compose: |\n", "companions:\n  - service: tool\n    label: Tool\n    purpose: Something beside it\n    inputs: [TOOL_PORT]\ncompose: |\n", 1)
}

// Adding a companion to an install from before moves its console to the server
// alone, as an upgrade does, and its plan says so (#752's review, M1).
func TestACompanionChangeSaysAConsoleMovesToTheServerAlone(t *testing.T) {
	t.Parallel()
	u := startPublic(t, toolEntry(1, "1.0", firstRelease))
	u.asBefore("web", toolEntry(1, "1.0", firstRelease))
	res, err := u.cat.SetCatalogCompanion(t.Context(), connect.NewRequest(&dockwellv1.SetCatalogCompanionRequest{Id: "thing", Service: "tool", On: true}))
	if err != nil {
		t.Fatal(err)
	}
	if n := noteOf(res.Msg.GetChange(), "port_to_server_alone"); n == nil || !strings.HasPrefix(n.GetMessage(), "web's port 18100 ") {
		t.Errorf("the plan's note: %v", n.GetMessage())
	}
	u.applied(res.Msg.GetChange())
	if got := u.addresses(); got[18100] != "127.0.0.1" || got[18180] != "" {
		t.Errorf("after adding the tool web publishes %v", got)
	}
}

// Using another database for an install from before moves its port to the server
// alone, as an upgrade does, and its plan says so (#752's review, M1).
func TestAChangeOfNeedSaysAPortMovesToTheServerAlone(t *testing.T) {
	t.Parallel()
	blog := strings.Replace(blogEntry, "\"127.0.0.1:${PORT}:80\"", "\"${PORT}:80\"", 1)
	dir := t.TempDir()
	useCatalogDir(t, dir)
	if err := os.WriteFile(filepath.Join(dir, "blog.yaml"), []byte(blog), 0o600); err != nil {
		t.Fatal(err)
	}
	f := startFull(t, store.SQLite, nil)
	b := signedIn(t, f)
	e := f.addDockerServer(t)
	own := &pgState{roles: map[string]string{"postgres": ""}, databases: map[string]string{"postgres": "postgres"}, sizes: map[string]int64{"blog": 12 << 20}}
	shared := &pgState{roles: map[string]string{"postgres": ""}, databases: map[string]string{"postgres": "postgres"}}
	c, err := catalog.Load("")
	if err != nil {
		t.Fatal(err)
	}
	postgres, _ := c.Entry("postgres")
	e.Registry[regexp.MustCompile(`image: (postgres:\S+)`).FindStringSubmatch(postgres.GetCompose())[1]] = own.image()
	e.Registry["postgres:17"] = shared.image()
	e.Registry["ghcr.io/acme/shop:1.4"] = dockertest.Image{Config: docker.Config{Cmd: []string{"shop"}}}
	e.Registry["redis:7"] = dockertest.Image{Config: docker.Config{Cmd: []string{"redis-server"}}}
	e.Registry["ghcr.io/acme/blog:1@sha256:"+digest1] = dockertest.Image{Config: docker.Config{Cmd: []string{"blog"}}}
	u := &upgrading{t: t, dir: dir, e: e, b: b, f: f, c: changeClient(b, b.origin), cat: catalogClient(b)}
	compose := dockwellv1connect.NewComposeServiceClient(b, b.origin, connect.WithProtoJSON())
	apply(t, u.c, importCompose(t, compose, databaseStack, map[string]string{"DB_PASSWORD": "the-administrators"}, false))
	installed := installApp(t, u.cat, &dockwellv1.InstallCatalogEntryRequest{Id: "blog"})
	u.applied(installed)
	for _, r := range installed.GetPlan().GetResources() {
		if r.GetResource().GetKind() == "link" {
			u.env = r.GetResource().GetScope()
		}
	}
	u.asBeforeIn("blog", "app", blog)
	entry, err := u.cat.GetCatalogEntry(t.Context(), connect.NewRequest(&dockwellv1.GetCatalogEntryRequest{Id: "blog"}))
	if err != nil {
		t.Fatal(err)
	}
	var choice string
	for _, o := range entry.Msg.GetNeeds() {
		if o.GetService() == "db" && o.GetProject() != "blog" && o.GetWhy() == "" {
			choice = o.GetChoice()
		}
	}
	res, err := u.cat.SetCatalogNeed(t.Context(), connect.NewRequest(&dockwellv1.SetCatalogNeedRequest{Id: "blog", Need: "db", Choice: choice}))
	if err != nil {
		t.Fatal(err)
	}
	if n := noteOf(res.Msg.GetChange(), "port_to_server_alone"); n == nil || !strings.HasPrefix(n.GetMessage(), "app's port 18190 ") {
		t.Errorf("the plan's note: %v", n.GetMessage())
	}
}

// A release that only takes public away from a port is an upgrade, though its file
// reads the same, and it moves the port to the server alone, with the note (#752's
// review, L2).
func TestAReleaseThatTakesPublicAwayIsAnUpgrade(t *testing.T) {
	t.Parallel()
	// Its console written on the server alone, so its file is as the install runs it
	// and the pin keeps it as written.
	written := func(release int, public bool) string {
		return strings.Replace(publicEntry(release, "1.0", firstRelease, public), "\"${PORT}:80\"", "\"127.0.0.1:${PORT}:80\"", 1)
	}
	u := startPublic(t, written(1, true))
	u.release(written(2, false))
	res, err := u.cat.ListCatalogInstalls(t.Context(), connect.NewRequest(&dockwellv1.ListCatalogInstallsRequest{}))
	if err != nil {
		t.Fatal(err)
	}
	if got := res.Msg.GetInstalls()[0].GetUpgrade(); got.GetRelease() != 2 {
		t.Errorf("the install offers %v, want release 2", got)
	}
	up := u.upgrade()
	if n := noteOf(up, "port_to_server_alone"); n == nil || !strings.HasPrefix(n.GetMessage(), "web's port 18180 answers on this server alone") {
		t.Errorf("the plan's note: %v", n.GetMessage())
	}
	u.applied(up)
	if got := u.addresses(); got[18180] != "127.0.0.1" {
		t.Errorf("after the upgrade web publishes %v, want 18180 on 127.0.0.1", got)
	}
}

// An app held to the server alone for now, as starting empty on another database
// holds it, asks nothing again at an upgrade for a port its running release declared
// public, which a person approved then (#752's review, L3).
func TestAHeldAppsUpgradeAsksNothingAgainForAPublicPort(t *testing.T) {
	t.Parallel()
	u := startPublic(t, publicEntry(1, "1.0", firstRelease, true))
	svc := u.stored("web")
	for _, p := range svc.GetApp().GetPorts() {
		p.Held = true
	}
	u.applied(propose(t, u.c, "Hold it", &dockwellv1.Edit{
		Resource: &dockwellv1.ResourceRef{Kind: "service", Scope: u.env, Name: "web"},
		Spec:     &resourcev1.Spec{Kind: &resourcev1.Spec_Service{Service: svc}},
	}))
	u.release(publicEntry(2, "2.0", secondRelease, true))
	up := u.upgrade()
	if got := publicAccess(up); len(got) > 0 || up.GetPlan().GetHostAccess() {
		t.Errorf("a held app's upgrade asks for public ports %v (host access %v)", got, up.GetPlan().GetHostAccess())
	}
}

// Connecting an install's storage from before moves its port to the server alone, as
// an upgrade does, and its plan says so (#752's review, M1, for #759's way).
func TestAStorageConnectionSaysAPortMovesToTheServerAlone(t *testing.T) {
	t.Parallel()
	dir, raw := ghostWithStorage(t)
	open := strings.Replace(string(raw), "\"127.0.0.1:${PORT}:2368\"", "\"${PORT}:2368\"", 1)
	if err := os.WriteFile(filepath.Join(dir, "ghost.yaml"), []byte(open), 0o600); err != nil {
		t.Fatal(err)
	}
	useAgentChecks(t)
	f := startFull(t, store.SQLite, nil)
	b := signedIn(t, f)
	e := f.addDockerServer(t)
	pullsAnything(e)
	e.Registry[regexp.MustCompile(`image: (mysql:\S+)`).FindStringSubmatch(open)[1]] = dockertest.Image{Config: docker.Config{Cmd: []string{"mysqld"}}}
	u := &upgrading{t: t, dir: dir, e: e, b: b, f: f, c: changeClient(b, b.origin), cat: catalogClient(b)}
	installed := installApp(t, u.cat, &dockwellv1.InstallCatalogEntryRequest{Id: "ghost", Project: "site"})
	u.applied(installed)
	for _, r := range installed.GetPlan().GetResources() {
		if r.GetResource().GetKind() == "service" && r.GetResource().GetName() == "ghost" {
			u.env = r.GetResource().GetScope()
		}
	}
	u.asBeforeIn("site", "ghost", open)
	for _, name := range e.Containers() {
		if !strings.Contains(name, "ghost") {
			continue
		}
		got, err := e.Client().InspectContainer(t.Context(), name)
		if err != nil {
			t.Fatal(err)
		}
		for net := range got.NetworkSettings.Networks {
			e.AddNetwork(docker.Network{Name: net, IPAM: docker.IPAM{Config: []docker.IPAMConfig{{Subnet: "127.0.0.0/8"}}}})
		}
	}
	srv := s3test.New(t)
	u.applied(propose(t, u.c, "Add uploads", connectionEdit("uploads", &resourcev1.ConnectionSpec{
		DisplayName: "Uploads", Kind: resourcev1.LinkKind_LINK_KIND_S3, Reach: resourcev1.ConnectionReach_CONNECTION_REACH_EVERY,
		S3: &resourcev1.ConnectionS3{Endpoint: srv.URL(), Bucket: "uploads", AccessKeyId: "AKIDEXAMPLE", SecretKey: connectionSecret, PathStyle: true},
	})))
	res, err := u.cat.SetCatalogNeed(t.Context(), connect.NewRequest(&dockwellv1.SetCatalogNeedRequest{Id: "site", Need: "storage", Choice: "connection:uploads", StartEmpty: true}))
	if err != nil {
		t.Fatal(err)
	}
	if n := noteOf(res.Msg.GetChange(), "port_to_server_alone"); n == nil || !strings.HasPrefix(n.GetMessage(), "ghost's port ") {
		t.Errorf("the plan's note: %v", n.GetMessage())
	}
}
