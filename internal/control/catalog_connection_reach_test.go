// SPDX-License-Identifier: AGPL-3.0-only

package control

import (
	"io"
	"os"
	"path/filepath"
	"regexp"
	"slices"
	"strings"
	"testing"

	"connectrpc.com/connect"
	"google.golang.org/protobuf/encoding/protojson"

	resourcev1 "github.com/dockwell/dockwell/gen/dockwell/resource/v1alpha1"
	dockwellv1 "github.com/dockwell/dockwell/gen/dockwell/v1"
	"github.com/dockwell/dockwell/gen/dockwell/v1/dockwellv1connect"
	"github.com/dockwell/dockwell/internal/control/catalogapi"
	"github.com/dockwell/dockwell/internal/docker"
	"github.com/dockwell/dockwell/internal/docker/dockertest"
	"github.com/dockwell/dockwell/internal/store"
	"github.com/dockwell/dockwell/internal/store/storetest"
)

// ghostWithStorage puts the Ghost entry with an optional bucket in the test's
// catalog, and says where, with the entry as it is.
func ghostWithStorage(t *testing.T) (string, []byte) {
	t.Helper()
	dir := t.TempDir()
	useCatalogDir(t, dir)
	raw, err := os.ReadFile(filepath.Join("testdata", "ghost-storage.yaml"))
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "ghost.yaml"), raw, 0o600); err != nil {
		t.Fatal(err)
	}
	return dir, raw
}

// A catalog entry offers only connections that reach every project: an install
// makes a new project, and the request's project is never one whose connections
// its caller may read. So a viewer of one project can't learn, by naming another,
// a connection only that one may use, which ListConnections hides from them too
// (the slice 1 review, H1).
func TestAnEntryOffersNoOtherProjectsConnection(t *testing.T) {
	t.Parallel()
	ghostWithStorage(t)
	f := startFull(t, store.SQLite, nil)
	b := signedIn(t, f)
	pullsAnything(f.addDockerServer(t))
	project := envProject(t, f, listedEnv(t, b))
	c := changeClient(b, b.origin)
	apply(t, c, propose(t, c, "Add the data project", &dockwellv1.Edit{
		Resource: &dockwellv1.ResourceRef{Name: "data"},
		Spec:     &resourcev1.Spec{Kind: &resourcev1.Spec_Project{Project: &resourcev1.Project{DisplayName: "Data"}}},
	}))
	var dataProject string
	if err := f.store.View(t.Context(), func(tx *store.Tx) error {
		org, err := tx.Organization(t.Context())
		if err != nil {
			return err
		}
		p, err := tx.GetResource(t.Context(), "project", org.ID, "data")
		dataProject = p.ID
		return err
	}); err != nil {
		t.Fatal(err)
	}
	apply(t, c, propose(t, c, "Add theirs", connectionEdit("theirs-private-name", &resourcev1.ConnectionSpec{
		DisplayName: "Theirs private label", Kind: resourcev1.LinkKind_LINK_KIND_S3,
		Reach: resourcev1.ConnectionReach_CONNECTION_REACH_PROJECTS, Projects: []string{dataProject},
		S3: &resourcev1.ConnectionS3{Endpoint: "https://s3.example.com", Bucket: "theirs", AccessKeyId: "AKIDTHEIRS", SecretKey: "theirs-secret"},
	})))
	hold := aMember(t, f)
	hold(store.Grant{ProjectID: project, Role: store.RoleViewer})
	cat := catalogClient(b)
	cc := dockwellv1connect.NewConnectionServiceClient(b, b.origin, connect.WithProtoJSON())
	listed, err := cc.ListConnections(t.Context(), connect.NewRequest(&dockwellv1.ListConnectionsRequest{}))
	if err != nil {
		t.Fatal(err)
	}
	if raw, _ := protojson.Marshal(listed.Msg); strings.Contains(string(raw), "theirs") {
		t.Fatalf("ListConnections shows theirs to a viewer: %s", raw)
	}
	for _, named := range []string{"data", dataProject} {
		got, err := cat.GetCatalogEntry(t.Context(), connect.NewRequest(&dockwellv1.GetCatalogEntryRequest{Id: "ghost", Project: named}))
		if err != nil {
			t.Fatalf("a viewer reads the entry for project %s: %v", named, err)
		}
		for _, o := range got.Msg.GetNeeds() {
			if o.GetConnection() == "theirs-private-name" {
				t.Errorf("a viewer of another project reads %q (%q) through the entry for project %s", o.GetConnection(), o.GetConnectionLabel(), named)
			}
		}
	}
}

// Two links to one connection keep to prefixes apart, however they're made: one
// whose prefix is another's, holds it or sits inside it is refused, with a free one
// to pick, while one beside it plans. A link at the bucket's root meets every other
// (the slice 1 review, H2).
func TestLinksToAConnectionKeepToPrefixesApart(t *testing.T) {
	t.Parallel()
	f := startFull(t, store.SQLite, nil)
	b := signedIn(t, f)
	env := listedEnv(t, b)
	c := changeClient(b, b.origin)
	linkTo := func(name, prefix string) *dockwellv1.Edit {
		return &dockwellv1.Edit{
			Resource: &dockwellv1.ResourceRef{Kind: "link", Scope: env, Name: name},
			Spec: &resourcev1.Spec{Kind: &resourcev1.Spec_Link{Link: &resourcev1.Link{
				Label: "Storage", Connection: "uploads", Kind: resourcev1.LinkKind_LINK_KIND_S3, Prefix: prefix,
				Sets: []*resourcev1.LinkSet{{Name: "S3_BUCKET_" + strings.ToUpper(strings.ReplaceAll(name, "-", "_")), Part: resourcev1.LinkPart_LINK_PART_BUCKET}},
			}}},
		}
	}
	apply(t, c, propose(t, c, "Add uploads", connectionEdit("uploads", &resourcev1.ConnectionSpec{
		DisplayName: "Uploads", Kind: resourcev1.LinkKind_LINK_KIND_S3, Reach: resourcev1.ConnectionReach_CONNECTION_REACH_EVERY,
		S3: &resourcev1.ConnectionS3{Endpoint: "https://s3.example.com", Bucket: "uploads", AccessKeyId: "AKIDEXAMPLE", SecretKey: connectionSecret},
	}), linkTo("storage", "ghost/")))

	for prefix, want := range map[string]string{
		"ghost/":       "Pick another prefix, such as ghost-2/",
		"ghost/media/": "Pick another prefix, such as ghost-2/",
		"":             "under the bucket's root",
	} {
		_, err := c.ProposeChange(t.Context(), connect.NewRequest(&dockwellv1.ProposeChangeRequest{Summary: "Link media", Edits: []*dockwellv1.Edit{linkTo("media", prefix)}}))
		if err == nil || !strings.Contains(err.Error(), "where another app keeps its files already, so each would overwrite the other's") || !strings.Contains(err.Error(), want) {
			t.Errorf("a second link under %q: %v", prefix, err)
		}
	}
	apply(t, c, propose(t, c, "Link media beside it", linkTo("media", "ghost-2/")))
	// A link keeps the prefix it has through an edit, and may move to a free one.
	apply(t, c, propose(t, c, "Move media", linkTo("media", "media/")))
}

// A connection serves one app: once one install keeps its files on it, a second
// can't take it, and is told why, so two apps never write at one bucket's root. The
// sheet offers it with that why (the slice 1 review, H3).
func TestASecondInstallCantTakeAConnectionThatServesOne(t *testing.T) {
	t.Parallel()
	_, raw := ghostWithStorage(t)
	f := startFull(t, store.SQLite, nil)
	b := signedIn(t, f)
	e := f.addDockerServer(t)
	pullsAnything(e)
	e.Registry[regexp.MustCompile(`image: (mysql:\S+)`).FindStringSubmatch(string(raw))[1]] = dockertest.Image{Config: docker.Config{Cmd: []string{"mysqld"}}}
	c := changeClient(b, b.origin)
	apply(t, c, propose(t, c, "Add uploads", connectionEdit("uploads", &resourcev1.ConnectionSpec{
		DisplayName: "Uploads", Kind: resourcev1.LinkKind_LINK_KIND_S3, Reach: resourcev1.ConnectionReach_CONNECTION_REACH_EVERY,
		S3: &resourcev1.ConnectionS3{Endpoint: "https://s3.example.com", Region: "eu-1", Bucket: "uploads", AccessKeyId: "AKIDEXAMPLE", SecretKey: connectionSecret},
	})))
	cat := catalogClient(b)
	installed := installApp(t, cat, &dockwellv1.InstallCatalogEntryRequest{Id: "ghost", Project: "site", Needs: map[string]string{"storage": "connection:uploads"}})
	if done := apply(t, c, installed); done.GetState() != dockwellv1.ChangeState_CHANGE_STATE_APPLIED {
		t.Fatalf("installing Ghost as site ended %v: %s", done.GetState(), done.GetError())
	}
	_, err := cat.InstallCatalogEntry(t.Context(), connect.NewRequest(&dockwellv1.InstallCatalogEntryRequest{Id: "ghost", Project: "shop", Needs: map[string]string{"storage": "connection:uploads"}}))
	if connect.CodeOf(err) != connect.CodeInvalidArgument || !strings.Contains(err.Error(), "it serves another app already, and this app can't keep to a prefix of the bucket, so their files would mix") {
		t.Errorf("a second Ghost on uploads: %v", err)
	}
	got, err := cat.GetCatalogEntry(t.Context(), connect.NewRequest(&dockwellv1.GetCatalogEntryRequest{Id: "ghost"}))
	if err != nil {
		t.Fatal(err)
	}
	for _, o := range got.Msg.GetNeeds() {
		if o.GetConnection() == "uploads" && !strings.Contains(o.GetWhy(), "it serves another app already") {
			t.Errorf("the sheet offers uploads as %v", o)
		}
	}
}

// Use another database, on a Ghost whose storage reads a connection, plans nothing
// for the storage's names or its link: each keeps reading the connection (the
// slice 1 review, H4).
func TestUseAnotherDatabaseKeepsWhatReadsAConnection(t *testing.T) {
	t.Parallel()
	_, raw := ghostWithStorage(t)
	f := startFull(t, store.SQLite, nil)
	b := signedIn(t, f)
	e := f.addDockerServer(t)
	pullsAnything(e)
	mysqlImage := regexp.MustCompile(`image: (mysql:\S+)`).FindStringSubmatch(string(raw))[1]
	e.Registry[mysqlImage] = dockertest.Image{Volumes: []string{"/var/lib/mysql"}, Config: docker.Config{Cmd: []string{"mysqld"}, Env: []string{"MYSQL_MAJOR=8.4"}}}
	e.Registry["mysql:8.4"] = dockertest.Image{
		Volumes: []string{"/var/lib/mysql"}, Config: docker.Config{Cmd: []string{"mysqld"}, Env: []string{"MYSQL_MAJOR=8.4"}},
		Exec: func(x dockertest.Exec) int { _, _ = io.ReadAll(x.Stdin); return 0 },
	}
	c := changeClient(b, b.origin)
	compose := dockwellv1connect.NewComposeServiceClient(b, b.origin, connect.WithProtoJSON())
	apply(t, c, importCompose(t, compose, "services:\n  mysql:\n    image: mysql:8.4\n    environment:\n      MYSQL_ROOT_PASSWORD: ${ROOT}\n    volumes:\n      - mysql-data:/var/lib/mysql\nvolumes:\n  mysql-data: {}\n", map[string]string{"ROOT": "the-administrators"}, false))
	apply(t, c, propose(t, c, "Add uploads", connectionEdit("uploads", &resourcev1.ConnectionSpec{
		DisplayName: "Uploads", Kind: resourcev1.LinkKind_LINK_KIND_S3, Reach: resourcev1.ConnectionReach_CONNECTION_REACH_EVERY,
		S3: &resourcev1.ConnectionS3{Endpoint: "https://s3.example.com", Region: "eu-1", Bucket: "uploads", AccessKeyId: "AKIDEXAMPLE", SecretKey: connectionSecret},
	})))
	cat := catalogClient(b)
	installed := installApp(t, cat, &dockwellv1.InstallCatalogEntryRequest{Id: "ghost", Project: "site", Needs: map[string]string{"storage": "connection:uploads"}})
	if done := apply(t, c, installed); done.GetState() != dockwellv1.ChangeState_CHANGE_STATE_APPLIED {
		t.Fatalf("installing Ghost ended %v: %s", done.GetState(), done.GetError())
	}
	entry, err := cat.GetCatalogEntry(t.Context(), connect.NewRequest(&dockwellv1.GetCatalogEntryRequest{Id: "ghost"}))
	if err != nil {
		t.Fatal(err)
	}
	var choice string
	for _, o := range entry.Msg.GetNeeds() {
		if o.GetNeed() == "db" && strings.HasPrefix(o.GetChoice(), "service:") && o.GetWhy() == "" && o.GetProject() != "site" {
			choice = o.GetChoice()
		}
	}
	if choice == "" {
		t.Fatalf("no MySQL to move to: %v", entry.Msg.GetNeeds())
	}
	res, err := cat.SetCatalogNeed(t.Context(), connect.NewRequest(&dockwellv1.SetCatalogNeedRequest{Id: "site", Need: "db", Choice: choice, StartEmpty: true}))
	if err != nil {
		t.Fatalf("Use another database: %v", err)
	}
	for _, r := range res.Msg.GetChange().GetPlan().GetResources() {
		kind, name := r.GetResource().GetKind(), r.GetResource().GetName()
		if ((kind == "variable" || kind == "secret") && strings.HasPrefix(name, "S3_") || kind == "link" && name == "storage") && r.GetAction() != dockwellv1.PlanAction_PLAN_ACTION_NONE {
			t.Errorf("moving Ghost's database plans %v for %s %s: %s", r.GetAction(), kind, name, protojson.Format(r))
		}
	}
}

// Disconnect keeps a catalog install's storage values as its own, and its pin says
// so: the next upgrade, an automatic one included, links the app to the connection
// again no more, and plans nothing for those values, which the app keeps running
// with (the slice 1 review, M1).
func TestAnUpgradeKeepsADisconnectedInstallsOwnValues(t *testing.T) {
	t.Parallel()
	dir, raw := ghostWithStorage(t)
	f := startFull(t, store.SQLite, nil)
	b := signedIn(t, f)
	e := f.addDockerServer(t)
	pullsAnything(e)
	e.Registry[regexp.MustCompile(`image: (mysql:\S+)`).FindStringSubmatch(string(raw))[1]] = dockertest.Image{Config: docker.Config{Cmd: []string{"mysqld"}}}
	c := changeClient(b, b.origin)
	apply(t, c, propose(t, c, "Add uploads", connectionEdit("uploads", &resourcev1.ConnectionSpec{
		DisplayName: "Uploads", Kind: resourcev1.LinkKind_LINK_KIND_S3, Reach: resourcev1.ConnectionReach_CONNECTION_REACH_EVERY,
		S3: &resourcev1.ConnectionS3{Endpoint: "https://s3.example.com", Region: "eu-1", Bucket: "uploads", AccessKeyId: "AKIDEXAMPLE", SecretKey: connectionSecret},
	})))
	cat := catalogClient(b)
	installed := installApp(t, cat, &dockwellv1.InstallCatalogEntryRequest{Id: "ghost", Project: "site", Needs: map[string]string{"storage": "connection:uploads"}})
	if done := apply(t, c, installed); done.GetState() != dockwellv1.ChangeState_CHANGE_STATE_APPLIED {
		t.Fatalf("installing Ghost ended %v: %s", done.GetState(), done.GetError())
	}
	cc := dockwellv1connect.NewConnectionServiceClient(b, b.origin, connect.WithProtoJSON())
	off, err := cc.DisconnectStorage(t.Context(), connect.NewRequest(&dockwellv1.DisconnectStorageRequest{Project: "site", Link: "storage"}))
	if err != nil {
		t.Fatalf("disconnect: %v", err)
	}
	if done := apply(t, c, off.Msg.GetChange()); done.GetState() != dockwellv1.ChangeState_CHANGE_STATE_APPLIED {
		t.Fatalf("disconnecting ended %v: %s", done.GetState(), done.GetError())
	}
	list, err := cat.ListCatalogInstalls(t.Context(), connect.NewRequest(&dockwellv1.ListCatalogInstallsRequest{}))
	if err != nil {
		t.Fatal(err)
	}
	for _, in := range list.Msg.GetInstalls() {
		if p := in.GetPin().GetNeeds()["storage"]; in.GetProject() == "site" && (!p.GetOwn() || p.GetConnection() != "" || p.GetPrefix() != "site/") {
			t.Errorf("after a disconnect, the pin says %v", p)
		}
	}

	next := strings.NewReplacer("release: 5", "release: 6", `version: "6.72.0"`, `version: "6.72.1"`, "retries: 24", "retries: 30").Replace(string(raw))
	if err := os.WriteFile(filepath.Join(dir, "ghost.yaml"), []byte(next), 0o600); err != nil {
		t.Fatal(err)
	}
	res, err := cat.UpgradeCatalogInstall(t.Context(), connect.NewRequest(&dockwellv1.UpgradeCatalogInstallRequest{Id: "site", SkipBackup: true}))
	if err != nil {
		t.Fatalf("upgrade: %v", err)
	}
	for _, r := range res.Msg.GetChange().GetPlan().GetResources() {
		kind, name := r.GetResource().GetKind(), r.GetResource().GetName()
		if (kind == "link" || strings.HasPrefix(name, "S3_")) && r.GetAction() != dockwellv1.PlanAction_PLAN_ACTION_NONE {
			t.Errorf("after a disconnect, the upgrade plans %v for %s %s", r.GetAction(), kind, name)
		}
	}
	if done := apply(t, c, res.Msg.GetChange()); done.GetState() != dockwellv1.ChangeState_CHANGE_STATE_APPLIED {
		t.Fatalf("upgrading Ghost ended %v: %s", done.GetState(), done.GetError())
	}
	var env []string
	for _, name := range e.Containers() {
		if strings.Contains(name, "ghost") {
			got, err := e.Client().InspectContainer(t.Context(), name)
			if err == nil {
				env = got.Config.Env
			}
		}
	}
	for _, want := range []string{"storage__active=s3", "storage__s3__bucket=uploads", "storage__s3__secretAccessKey=" + connectionSecret} {
		if !slices.Contains(env, want) {
			t.Errorf("upgraded after a disconnect, Ghost runs without %s: %v", want, env)
		}
	}
}

// A connection the control plane can't open fails no install: the sheet leaves it
// out, and an install's plan says it wasn't offered and how to fix that, while the
// install goes on with a connection it can read (the slice 1 review, M3).
func TestAConnectionThatCantBeReadFailsNoInstall(t *testing.T) {
	t.Parallel()
	_, raw := ghostWithStorage(t)
	f := startFull(t, store.SQLite, nil)
	b := signedIn(t, f)
	e := f.addDockerServer(t)
	pullsAnything(e)
	e.Registry[regexp.MustCompile(`image: (mysql:\S+)`).FindStringSubmatch(string(raw))[1]] = dockertest.Image{Config: docker.Config{Cmd: []string{"mysqld"}}}
	c := changeClient(b, b.origin)
	s3 := func(bucket string) *resourcev1.ConnectionSpec {
		return &resourcev1.ConnectionSpec{
			Kind: resourcev1.LinkKind_LINK_KIND_S3, Reach: resourcev1.ConnectionReach_CONNECTION_REACH_EVERY,
			S3: &resourcev1.ConnectionS3{Endpoint: "https://s3.example.com", Bucket: bucket, AccessKeyId: "AKIDEXAMPLE", SecretKey: connectionSecret},
		}
	}
	apply(t, c, propose(t, c, "Add the buckets", connectionEdit("uploads", s3("uploads")), connectionEdit("broken", s3("broken"))))
	storetest.Exec(t, f.db, `DROP TRIGGER revisions_never_change`)
	storetest.Exec(t, f.db, `UPDATE revisions SET spec = x'0a0a' WHERE resource_id IN (SELECT id FROM resources WHERE kind = 'connection' AND name = 'broken')`)

	cat := catalogClient(b)
	got, err := cat.GetCatalogEntry(t.Context(), connect.NewRequest(&dockwellv1.GetCatalogEntryRequest{Id: "ghost"}))
	if err != nil {
		t.Fatalf("a connection that can't be read fails the sheet: %v", err)
	}
	for _, o := range got.Msg.GetNeeds() {
		if o.GetConnection() == "broken" {
			t.Errorf("the sheet offers a connection it can't read: %v", o)
		}
	}
	installed := installApp(t, cat, &dockwellv1.InstallCatalogEntryRequest{Id: "ghost", Project: "site", Needs: map[string]string{"storage": "connection:uploads"}})
	if notes := notesOf(installed, catalogapi.ConnectionUnreadNote); len(notes) != 1 || !strings.Contains(notes[0], "Dockwell can't read the connection broken, so it wasn't offered") {
		t.Errorf("the install's plan says %v", notes)
	}
	if done := apply(t, c, installed); done.GetState() != dockwellv1.ChangeState_CHANGE_STATE_APPLIED {
		t.Fatalf("installing Ghost ended %v: %s", done.GetState(), done.GetError())
	}
}

// prefixClash makes uploads with a link at hidden in the default project, which a
// developer of data can't see, and, where seen isn't empty, one in data itself; then
// that developer links media to ghost/ in data. It returns what the developer's
// proposal says, with a way to propose another prefix as the same developer.
func prefixClash(t *testing.T, hidden, seen string) (link func(prefix string) error, err error) {
	t.Helper()
	f := startFull(t, store.SQLite, nil)
	b := signedIn(t, f)
	env := listedEnv(t, b)
	c := changeClient(b, b.origin)
	linkTo := func(scope, name, prefix string) *dockwellv1.Edit {
		return &dockwellv1.Edit{
			Resource: &dockwellv1.ResourceRef{Kind: "link", Scope: scope, Name: name},
			Spec: &resourcev1.Spec{Kind: &resourcev1.Spec_Link{Link: &resourcev1.Link{
				Label: "Storage", Connection: "uploads", Kind: resourcev1.LinkKind_LINK_KIND_S3, Prefix: prefix,
				Sets: []*resourcev1.LinkSet{{Name: "S3_BUCKET_" + strings.ToUpper(name), Part: resourcev1.LinkPart_LINK_PART_BUCKET}},
			}}},
		}
	}
	apply(t, c, propose(t, c, "Add uploads", connectionEdit("uploads", &resourcev1.ConnectionSpec{
		Kind: resourcev1.LinkKind_LINK_KIND_S3, Reach: resourcev1.ConnectionReach_CONNECTION_REACH_EVERY,
		S3: &resourcev1.ConnectionS3{Endpoint: "https://s3.example.com", Bucket: "uploads", AccessKeyId: "AKIDEXAMPLE", SecretKey: connectionSecret},
	}), linkTo(env, "hidden", hidden), &dockwellv1.Edit{
		Resource: &dockwellv1.ResourceRef{Name: "data"},
		Spec:     &resourcev1.Spec{Kind: &resourcev1.Spec_Project{Project: &resourcev1.Project{DisplayName: "Data"}}},
	}))
	data := envProjectByName(t, f, "data")
	var dataEnv string
	if err := f.store.View(t.Context(), func(tx *store.Tx) error {
		r, err := tx.GetResource(t.Context(), "environment", data, "production")
		dataEnv = r.ID
		return err
	}); err != nil {
		t.Fatal(err)
	}
	if seen != "" {
		apply(t, c, propose(t, c, "Their own link", linkTo(dataEnv, "seen", seen)))
	}
	hold := aMember(t, f)
	hold(store.Grant{ProjectID: data, Role: store.RoleDeveloper})
	link = func(prefix string) error {
		_, err := c.ProposeChange(t.Context(), connect.NewRequest(&dockwellv1.ProposeChangeRequest{Summary: "Link media", Edits: []*dockwellv1.Edit{linkTo(dataEnv, "media", prefix)}}))
		return err
	}
	return link, link("ghost/")
}

// A prefix refusal suggests one from the links the person sees, where no project
// holds it, so taking it plans; its words name no project and no other app (the
// slice 3 review, L3, and its re-check, N3).
func TestAPrefixSuggestionIsFreeEverywhere(t *testing.T) {
	t.Parallel()
	link, err := prefixClash(t, "ghost-3/", "ghost/")
	if err == nil || !strings.HasSuffix(strings.TrimSpace(err.Error()), "such as ghost-2/") || strings.Contains(err.Error(), "default") || strings.Contains(err.Error(), "hidden") {
		t.Fatalf("a developer's clash: %v", err)
	}
	if err := link("ghost-2/"); err != nil {
		t.Errorf("taking the suggested ghost-2/: %v", err)
	}
}

// Below an admin, a clash with another project's link is refused with no suggestion
// drawn from prefixes the person can't see, so "such as ghost-3/" never says that
// ghost-2/ is taken elsewhere (the slice 1 re-check, N3).
func TestAPrefixRefusalSuggestsOnlyFromWhatThePersonSees(t *testing.T) {
	t.Parallel()
	_, err := prefixClash(t, "ghost/", "")
	if err == nil || !strings.Contains(err.Error(), "Pick a prefix of its own") || strings.Contains(err.Error(), "such as") {
		t.Errorf("a developer's clash with another project's prefix: %v", err)
	}
}

// Where the prefix the person's own links suggest is held in a project they can't
// see, the refusal suggests none rather than one that fails or says it's held (the
// slice 3 re-check, N3).
func TestAPrefixHeldOutOfSightIsNotSuggested(t *testing.T) {
	t.Parallel()
	_, err := prefixClash(t, "ghost-2/", "ghost/")
	if err == nil || !strings.Contains(err.Error(), "Pick a prefix of its own") || strings.Contains(err.Error(), "such as") {
		t.Errorf("a developer's clash beside a hidden ghost-2/: %v", err)
	}
}
