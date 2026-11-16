// SPDX-License-Identifier: AGPL-3.0-only

package control

import (
	"regexp"
	"slices"
	"strings"
	"testing"

	"connectrpc.com/connect"
	"google.golang.org/protobuf/encoding/protojson"

	resourcev1 "github.com/dockwell/dockwell/gen/dockwell/resource/v1alpha1"
	dockwellv1 "github.com/dockwell/dockwell/gen/dockwell/v1"
	"github.com/dockwell/dockwell/gen/dockwell/v1/dockwellv1connect"
	"github.com/dockwell/dockwell/internal/backup/s3/s3test"
	"github.com/dockwell/dockwell/internal/catalog"
	"github.com/dockwell/dockwell/internal/control/catalogapi"
	"github.com/dockwell/dockwell/internal/docker"
	"github.com/dockwell/dockwell/internal/docker/dockertest"
	"github.com/dockwell/dockwell/internal/store"
)

// An install checks a connection's bucket before it links the app to it, from the
// server the app is to run on, on a network made for the check: one that fails
// plans nothing, in the check's words, and leaves nothing behind on the server
// (ADR 0099, slice 2).
func TestAnInstallChecksItsConnectionFirst(t *testing.T) {
	t.Parallel()
	_, raw := ghostWithStorage(t)
	useAgentChecks(t)
	f := startFull(t, store.SQLite, nil)
	b := signedIn(t, f)
	e := f.addDockerServer(t)
	pullsAnything(e)
	e.Registry[regexp.MustCompile(`image: (mysql:\S+)`).FindStringSubmatch(string(raw))[1]] = dockertest.Image{Config: docker.Config{Cmd: []string{"mysqld"}}}
	c := changeClient(b, b.origin)
	// This machine's loopback, which a check from a network of its own refuses at once.
	apply(t, c, propose(t, c, "Add uploads", connectionEdit("uploads", &resourcev1.ConnectionSpec{
		DisplayName: "Uploads", Kind: resourcev1.LinkKind_LINK_KIND_S3, Reach: resourcev1.ConnectionReach_CONNECTION_REACH_EVERY,
		S3: &resourcev1.ConnectionS3{Endpoint: "http://127.0.0.1:9", Bucket: "uploads", AccessKeyId: "AKIDEXAMPLE", SecretKey: connectionSecret, PathStyle: true},
	})))
	cat := catalogClient(b)
	_, err := cat.InstallCatalogEntry(t.Context(), connect.NewRequest(&dockwellv1.InstallCatalogEntryRequest{Id: "ghost", Project: "site", Needs: map[string]string{"storage": "connection:uploads"}}))
	if connect.CodeOf(err) != connect.CodeFailedPrecondition || reasonOf(err) != "check_failed" ||
		!strings.Contains(err.Error(), "The app's server sends nothing to that address: 127.0.0.1 is this server's own address") ||
		!strings.Contains(err.Error(), "Nothing was planned, as hz-1 checked the connection uploads first. Check the connection's endpoint") || strings.Contains(err.Error(), connectionSecret) {
		t.Errorf("installing Ghost on a bucket that fails its check: %v", err)
	}
	// The check ran on a network of its own, and took it away after.
	networks, err := e.Client().ListNetworks(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	for _, n := range networks {
		if strings.HasPrefix(n.Name, docker.ProbeRepository) {
			t.Errorf("the check left the network %s", n.Name)
		}
	}
	// Optional, Ghost still installs without it.
	if _, err := cat.InstallCatalogEntry(t.Context(), connect.NewRequest(&dockwellv1.InstallCatalogEntryRequest{Id: "ghost", Project: "site"})); err != nil {
		t.Errorf("installing Ghost on its own disk: %v", err)
	}
}

// An installed app's storage is connected from its need card: Ghost, installed on its
// own disk, takes a connection in one plan and runs with its values, the pin keeping
// the connection; moving to another leaves its files in the first bucket, so it's
// refused until asked to start empty; and the same connection, or no connection,
// plans nothing (ADR 0099, slice 2).
func TestAnInstalledAppIsConnectedFromItsNeedCard(t *testing.T) {
	t.Parallel()
	_, raw := ghostWithStorage(t)
	f := startFull(t, store.SQLite, nil)
	b := signedIn(t, f)
	e := f.addDockerServer(t)
	pullsAnything(e)
	e.Registry[regexp.MustCompile(`image: (mysql:\S+)`).FindStringSubmatch(string(raw))[1]] = dockertest.Image{Config: docker.Config{Cmd: []string{"mysqld"}}}
	c := changeClient(b, b.origin)
	bucket := func(name string) *dockwellv1.Edit {
		return connectionEdit(name, &resourcev1.ConnectionSpec{
			Kind: resourcev1.LinkKind_LINK_KIND_S3, Reach: resourcev1.ConnectionReach_CONNECTION_REACH_EVERY,
			S3: &resourcev1.ConnectionS3{Endpoint: "https://s3.example.com", Region: "eu-1", Bucket: name, AccessKeyId: "AKIDEXAMPLE", SecretKey: connectionSecret},
		})
	}
	apply(t, c, propose(t, c, "Add the buckets", bucket("uploads"), bucket("media")))
	cat := catalogClient(b)
	if done := apply(t, c, installApp(t, cat, &dockwellv1.InstallCatalogEntryRequest{Id: "ghost", Project: "site"})); done.GetState() != dockwellv1.ChangeState_CHANGE_STATE_APPLIED {
		t.Fatalf("installing Ghost ended %v: %s", done.GetState(), done.GetError())
	}
	ghostEnv := func() []string {
		t.Helper()
		for _, name := range slices.Backward(e.Containers()) {
			if strings.Contains(name, "ghost") {
				got, err := e.Client().InspectContainer(t.Context(), name)
				if err != nil {
					t.Fatal(err)
				}
				return got.Config.Env
			}
		}
		t.Fatal("Ghost doesn't run")
		return nil
	}
	pinOf := func() *resourcev1.PinnedNeed {
		t.Helper()
		list, err := cat.ListCatalogInstalls(t.Context(), connect.NewRequest(&dockwellv1.ListCatalogInstallsRequest{}))
		if err != nil {
			t.Fatal(err)
		}
		for _, in := range list.Msg.GetInstalls() {
			if in.GetProject() == "site" {
				return in.GetPin().GetNeeds()["storage"]
			}
		}
		return nil
	}
	set := func(choice string, startEmpty bool) (*dockwellv1.Change, error) {
		res, err := cat.SetCatalogNeed(t.Context(), connect.NewRequest(&dockwellv1.SetCatalogNeedRequest{Id: "site", Need: "storage", Choice: choice, StartEmpty: startEmpty}))
		if err != nil {
			return nil, err
		}
		return res.Msg.GetChange(), nil
	}

	// Its files on its own disk stay there, so it starts empty only when asked (slice 2 review, M3).
	if _, err := set("connection:uploads", false); reasonOf(err) != "cant_copy" || !strings.Contains(err.Error(), "site keeps its files on its own disk") {
		t.Errorf("connecting Ghost's storage without starting empty: %v", err)
	}
	planned, err := set("connection:uploads", true)
	if err != nil {
		t.Fatalf("connecting Ghost's storage: %v", err)
	}
	if notes := notesOf(planned, catalogapi.StartsEmptyNote); len(notes) != 1 || notes[0] != "site's files stay on its own disk, as Dockwell copies none across. It starts empty on uploads" {
		t.Errorf("connecting says %v", notes)
	}
	if planned.GetSummary() != "Connect site's storage to uploads" || len(notesOf(planned, catalogapi.ConnectionUncheckedNote)) != 1 {
		t.Errorf("connecting plans %q with %v", planned.GetSummary(), planned.GetPlan().GetNotes())
	}
	// Ghost had storage at this release, on its own disk: nothing says it's new (slice 2 review, M1).
	if notes := notesOf(planned, catalogapi.NewNeedNote); len(notes) != 0 {
		t.Errorf("connecting says the release adds storage: %v", notes)
	}
	if raw, _ := protojson.Marshal(planned); strings.Contains(string(raw), connectionSecret) {
		t.Error("the plan holds the secret key")
	}
	if done := apply(t, c, planned); done.GetState() != dockwellv1.ChangeState_CHANGE_STATE_APPLIED {
		t.Fatalf("connecting Ghost's storage ended %v: %s", done.GetState(), done.GetError())
	}
	for _, want := range []string{"storage__active=s3", "storage__s3__bucket=uploads", "storage__s3__secretAccessKey=" + connectionSecret} {
		if env := ghostEnv(); !slices.Contains(env, want) {
			t.Errorf("connected, Ghost runs without %s: %v", want, env)
		}
	}
	if p := pinOf(); p.GetConnection() != "uploads" || p.GetPrefix() != "site/" || p.GetUnset() {
		t.Errorf("connected, the pin says %v", p)
	}

	// The same connection plans nothing; no connection is the Environment tab's.
	if _, err := set("connection:uploads", false); connect.CodeOf(err) != connect.CodeFailedPrecondition || reasonOf(err) != "unchanged" {
		t.Errorf("connecting it to the connection it's on: %v", err)
	}
	if _, err := set(catalog.ChoiceUnset, false); connect.CodeOf(err) != connect.CodeInvalidArgument || !strings.Contains(err.Error(), "disconnect it on the app's Environment tab") {
		t.Errorf("leaving its storage unset: %v", err)
	}
	// Another bucket leaves its files in this one, so it starts empty only when asked.
	if _, err := set("connection:media", false); connect.CodeOf(err) != connect.CodeFailedPrecondition || reasonOf(err) != "cant_copy" ||
		!strings.Contains(err.Error(), "Start empty on media to go on") {
		t.Errorf("moving to another bucket: %v", err)
	}
	moved, err := set("connection:media", true)
	if err != nil {
		t.Fatalf("moving to another bucket, starting empty: %v", err)
	}
	if done := apply(t, c, moved); done.GetState() != dockwellv1.ChangeState_CHANGE_STATE_APPLIED {
		t.Fatalf("moving Ghost's storage ended %v: %s", done.GetState(), done.GetError())
	}
	if env := ghostEnv(); !slices.Contains(env, "storage__s3__bucket=media") {
		t.Errorf("moved, Ghost runs with %v", env)
	}
	if p := pinOf(); p.GetConnection() != "media" {
		t.Errorf("moved, the pin says %v", p)
	}
	// uploads serves nothing now, so another install may take it.
	got, err := cat.GetCatalogEntry(t.Context(), connect.NewRequest(&dockwellv1.GetCatalogEntryRequest{Id: "ghost"}))
	if err != nil {
		t.Fatal(err)
	}
	for _, o := range got.Msg.GetNeeds() {
		if o.GetConnection() == "uploads" && o.GetWhy() != "" {
			t.Errorf("after the move, uploads is offered as %v", o)
		}
	}

	// Below an install admin, it's refused before any check runs (slice 2 review, L1).
	hold := aMember(t, f)
	hold(store.Grant{ProjectID: envProjectByName(t, f, "site"), Role: store.RoleDeveloper})
	if _, err := set("connection:uploads", true); connect.CodeOf(err) != connect.CodePermissionDenied || !strings.Contains(err.Error(), "an app's storage is connected by an admin") {
		t.Errorf("a developer connecting Ghost's storage: %v", err)
	}
	hold(store.Grant{ProjectID: store.Install, Role: store.RoleAdmin})

	// Disconnected, its values are its own; a connection again starts empty too.
	cc := dockwellv1connect.NewConnectionServiceClient(b, b.origin, connect.WithProtoJSON())
	off, err := cc.DisconnectStorage(t.Context(), connect.NewRequest(&dockwellv1.DisconnectStorageRequest{Project: "site", Link: "storage"}))
	if err != nil {
		t.Fatal(err)
	}
	apply(t, c, off.Msg.GetChange())
	if _, err := set("connection:uploads", false); reasonOf(err) != "cant_copy" {
		t.Errorf("connecting storage on its own values: %v", err)
	}
	again, err := set("connection:uploads", true)
	if err != nil {
		t.Fatalf("connecting storage on its own values, starting empty: %v", err)
	}
	if done := apply(t, c, again); done.GetState() != dockwellv1.ChangeState_CHANGE_STATE_APPLIED {
		t.Fatalf("connecting it again ended %v: %s", done.GetState(), done.GetError())
	}
	if p := pinOf(); p.GetConnection() != "uploads" || p.GetOwn() {
		t.Errorf("connected again, the pin says %v", p)
	}
	if env := ghostEnv(); !slices.Contains(env, "storage__s3__bucket=uploads") {
		t.Errorf("connected again, Ghost runs with %v", env)
	}
}

// A connection an admin adds from the install sheet reaches the app's project (ADR
// 0099): it reaches none until then, so no sheet offers it, but the install that asks
// for it by name gives it the project it makes, in its own plan, which says so; the
// app then runs on it.
func TestAConnectionAddedForAnInstallReachesItsProject(t *testing.T) {
	t.Parallel()
	_, raw := ghostWithStorage(t)
	f := startFull(t, store.SQLite, nil)
	b := signedIn(t, f)
	e := f.addDockerServer(t)
	pullsAnything(e)
	e.Registry[regexp.MustCompile(`image: (mysql:\S+)`).FindStringSubmatch(string(raw))[1]] = dockertest.Image{Config: docker.Config{Cmd: []string{"mysqld"}}}
	c := changeClient(b, b.origin)
	// As the install sheet's Add a connection saves it: for no project yet.
	apply(t, c, propose(t, c, "Add fresh", connectionEdit("fresh", &resourcev1.ConnectionSpec{
		DisplayName: "Fresh", Kind: resourcev1.LinkKind_LINK_KIND_S3,
		S3: &resourcev1.ConnectionS3{Endpoint: "https://s3.example.com", Bucket: "fresh", AccessKeyId: "AKIDEXAMPLE", SecretKey: connectionSecret},
	})))
	cat := catalogClient(b)
	got, err := cat.GetCatalogEntry(t.Context(), connect.NewRequest(&dockwellv1.GetCatalogEntryRequest{Id: "ghost"}))
	if err != nil {
		t.Fatal(err)
	}
	for _, o := range got.Msg.GetNeeds() {
		if o.GetConnection() == "fresh" {
			t.Errorf("the sheet offers a connection that reaches no project: %v", o)
		}
	}
	installed := installApp(t, cat, &dockwellv1.InstallCatalogEntryRequest{Id: "ghost", Project: "site", Needs: map[string]string{"storage": "connection:fresh"}})
	if notes := notesOf(installed, catalogapi.ConnectionReachNote); len(notes) != 1 || notes[0] != "The connection fresh reaches the project site from this install on, so its apps may use it" {
		t.Errorf("the install's plan says %v", notes)
	}
	if done := apply(t, c, installed); done.GetState() != dockwellv1.ChangeState_CHANGE_STATE_APPLIED {
		t.Fatalf("installing Ghost on fresh ended %v: %s", done.GetState(), done.GetError())
	}
	site := envProjectByName(t, f, "site")
	cc := dockwellv1connect.NewConnectionServiceClient(b, b.origin, connect.WithProtoJSON())
	listed, err := cc.ListConnections(t.Context(), connect.NewRequest(&dockwellv1.ListConnectionsRequest{}))
	if err != nil {
		t.Fatal(err)
	}
	for _, conn := range listed.Msg.GetConnections() {
		if conn.GetName() == "fresh" && (conn.GetReach() != resourcev1.ConnectionReach_CONNECTION_REACH_PROJECTS || !slices.Equal(conn.GetProjects(), []string{site})) {
			t.Errorf("after the install, fresh reaches %v %v, want site's project %s", conn.GetReach(), conn.GetProjects(), site)
		}
	}
	var env []string
	for _, name := range e.Containers() {
		if strings.Contains(name, "ghost") {
			if got, err := e.Client().InspectContainer(t.Context(), name); err == nil {
				env = got.Config.Env
			}
		}
	}
	if !slices.Contains(env, "storage__s3__bucket=fresh") {
		t.Errorf("Ghost runs with %v", env)
	}
}

// An app that runs already is checked from its own networks, as Connect storage
// checks it: a bucket the app reaches there, as one beside it, checks as reached,
// where a network made for the check would refuse it (slice 2 review, M4).
func TestAnInstalledAppsConnectionIsCheckedFromItsNetwork(t *testing.T) {
	t.Parallel()
	_, raw := ghostWithStorage(t)
	useAgentChecks(t)
	f := startFull(t, store.SQLite, nil)
	b := signedIn(t, f)
	e := f.addDockerServer(t)
	pullsAnything(e)
	e.Registry[regexp.MustCompile(`image: (mysql:\S+)`).FindStringSubmatch(string(raw))[1]] = dockertest.Image{Config: docker.Config{Cmd: []string{"mysqld"}}}
	c := changeClient(b, b.origin)
	cat := catalogClient(b)
	if done := apply(t, c, installApp(t, cat, &dockwellv1.InstallCatalogEntryRequest{Id: "ghost", Project: "site"})); done.GetState() != dockwellv1.ChangeState_CHANGE_STATE_APPLIED {
		t.Fatalf("installing Ghost ended %v: %s", done.GetState(), done.GetError())
	}
	// Ghost's network holds this machine's loopback, where the stand-in bucket listens.
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
	apply(t, c, propose(t, c, "Add uploads", connectionEdit("uploads", &resourcev1.ConnectionSpec{
		DisplayName: "Uploads", Kind: resourcev1.LinkKind_LINK_KIND_S3, Reach: resourcev1.ConnectionReach_CONNECTION_REACH_EVERY,
		S3: &resourcev1.ConnectionS3{Endpoint: srv.URL(), Bucket: "uploads", AccessKeyId: "AKIDEXAMPLE", SecretKey: connectionSecret, PathStyle: true},
	})))
	res, err := cat.SetCatalogNeed(t.Context(), connect.NewRequest(&dockwellv1.SetCatalogNeedRequest{Id: "site", Need: "storage", Choice: "connection:uploads", StartEmpty: true}))
	if err != nil {
		t.Fatalf("connecting Ghost's storage, checked from its network: %v", err)
	}
	if notes := notesOf(res.Msg.GetChange(), catalogapi.ConnectionUncheckedNote); len(notes) != 0 {
		t.Errorf("checked from Ghost's network, the plan still says %v", notes)
	}
	if len(srv.Authorizations) == 0 {
		t.Error("the bucket was asked nothing")
	}
	if left := srv.Objects(); len(left) != 0 {
		t.Errorf("the check left %v", left)
	}
}
