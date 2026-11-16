// SPDX-License-Identifier: AGPL-3.0-only

package control

import (
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"testing"

	"connectrpc.com/connect"

	resourcev1 "github.com/dockwell/dockwell/gen/dockwell/resource/v1alpha1"
	dockwellv1 "github.com/dockwell/dockwell/gen/dockwell/v1"
	"github.com/dockwell/dockwell/gen/dockwell/v1/dockwellv1connect"
	"github.com/dockwell/dockwell/internal/control/catalogapi"
	"github.com/dockwell/dockwell/internal/docker"
	"github.com/dockwell/dockwell/internal/docker/dockertest"
	"github.com/dockwell/dockwell/internal/store"
)

// An install never widens a connection that names other projects: the install
// sheet's Add a connection reaches exactly the project the install makes, so one
// that names another is an admin's choice on Connections, and an install that asks
// for it is refused in words that say why and what to do (slice 2 review, H2).
func TestAnInstallRefusesAConnectionThatNamesAnotherProject(t *testing.T) {
	t.Parallel()
	_, raw := ghostWithStorage(t)
	f := startFull(t, store.SQLite, nil)
	b := signedIn(t, f)
	e := f.addDockerServer(t)
	pullsAnything(e)
	e.Registry[regexp.MustCompile(`image: (mysql:\S+)`).FindStringSubmatch(string(raw))[1]] = dockertest.Image{Config: docker.Config{Cmd: []string{"mysqld"}}}
	other := envProject(t, f, listedEnv(t, b))
	c := changeClient(b, b.origin)
	apply(t, c, propose(t, c, "Add fresh", connectionEdit("fresh", &resourcev1.ConnectionSpec{
		DisplayName: "Fresh", Kind: resourcev1.LinkKind_LINK_KIND_S3,
		Reach: resourcev1.ConnectionReach_CONNECTION_REACH_PROJECTS, Projects: []string{other},
		S3: &resourcev1.ConnectionS3{Endpoint: "https://s3.example.com", Bucket: "fresh", AccessKeyId: "AKIDEXAMPLE", SecretKey: connectionSecret},
	})))
	cat := catalogClient(b)
	_, err := cat.InstallCatalogEntry(t.Context(), connect.NewRequest(&dockwellv1.InstallCatalogEntryRequest{Id: "ghost", Project: "site", Needs: map[string]string{"storage": "connection:fresh"}}))
	if connect.CodeOf(err) != connect.CodeInvalidArgument || !strings.Contains(err.Error(), "the connection fresh doesn't reach Ghost's project, so nothing was planned. Let every project use it on Connections") {
		t.Errorf("installing Ghost on a connection that names another project: %v", err)
	}
}

// Moving storage to another bucket, starting empty, leaves the app's files behind,
// and the plan says so, as Use another database's does, so a person who applies it,
// or runs set-need -y, is told (slice 2 review, M2).
func TestStartingEmptyOnAnotherBucketIsSaidInThePlan(t *testing.T) {
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
	if done := apply(t, c, installApp(t, cat, &dockwellv1.InstallCatalogEntryRequest{Id: "ghost", Project: "site", Needs: map[string]string{"storage": "connection:uploads"}})); done.GetState() != dockwellv1.ChangeState_CHANGE_STATE_APPLIED {
		t.Fatalf("installing Ghost ended %v: %s", done.GetState(), done.GetError())
	}
	res, err := cat.SetCatalogNeed(t.Context(), connect.NewRequest(&dockwellv1.SetCatalogNeedRequest{Id: "site", Need: "storage", Choice: "connection:media", StartEmpty: true}))
	if err != nil {
		t.Fatal(err)
	}
	if notes := notesOf(res.Msg.GetChange(), catalogapi.StartsEmptyNote); len(notes) != 1 ||
		notes[0] != "site's files stay in uploads under site/, as Dockwell copies none across; it starts empty on media" {
		t.Errorf("starting empty on media plans with %v", res.Msg.GetChange().GetPlan().GetNotes())
	}
}

// After a Disconnect, the app keeps writing in the bucket with its own values, so the
// connection serves it still, and the next install isn't offered it, as slice 1's H3
// would otherwise come back through a Disconnect; the app itself may go back onto it
// (slice 2 review, M6).
func TestADisconnectedAppStillHoldsTheBucket(t *testing.T) {
	t.Parallel()
	_, raw := ghostWithStorage(t)
	f := startFull(t, store.SQLite, nil)
	b := signedIn(t, f)
	e := f.addDockerServer(t)
	pullsAnything(e)
	e.Registry[regexp.MustCompile(`image: (mysql:\S+)`).FindStringSubmatch(string(raw))[1]] = dockertest.Image{Config: docker.Config{Cmd: []string{"mysqld"}}}
	c := changeClient(b, b.origin)
	apply(t, c, propose(t, c, "Add uploads", connectionEdit("uploads", &resourcev1.ConnectionSpec{
		Kind: resourcev1.LinkKind_LINK_KIND_S3, Reach: resourcev1.ConnectionReach_CONNECTION_REACH_EVERY,
		S3: &resourcev1.ConnectionS3{Endpoint: "https://s3.example.com", Region: "eu-1", Bucket: "uploads", AccessKeyId: "AKIDEXAMPLE", SecretKey: connectionSecret},
	})))
	cat := catalogClient(b)
	if done := apply(t, c, installApp(t, cat, &dockwellv1.InstallCatalogEntryRequest{Id: "ghost", Project: "site", Needs: map[string]string{"storage": "connection:uploads"}})); done.GetState() != dockwellv1.ChangeState_CHANGE_STATE_APPLIED {
		t.Fatalf("installing Ghost ended %v: %s", done.GetState(), done.GetError())
	}
	cc := dockwellv1connect.NewConnectionServiceClient(b, b.origin, connect.WithProtoJSON())
	off, err := cc.DisconnectStorage(t.Context(), connect.NewRequest(&dockwellv1.DisconnectStorageRequest{Project: "site", Link: "storage"}))
	if err != nil {
		t.Fatal(err)
	}
	apply(t, c, off.Msg.GetChange())
	got, err := cat.GetCatalogEntry(t.Context(), connect.NewRequest(&dockwellv1.GetCatalogEntryRequest{Id: "ghost"}))
	if err != nil {
		t.Fatal(err)
	}
	offered := false
	for _, o := range got.Msg.GetNeeds() {
		if o.GetConnection() == "uploads" {
			offered = true
			if !strings.Contains(o.GetWhy(), "it serves another app already") {
				t.Errorf("site still writes in uploads with its own values, but uploads is offered as %v", o)
			}
		}
	}
	if !offered {
		t.Error("uploads isn't offered at all")
	}
	// ListConnections says so too, naming no project, for the card (slice 2 re-check, N2).
	listed, err := cc.ListConnections(t.Context(), connect.NewRequest(&dockwellv1.ListConnectionsRequest{}))
	if err != nil {
		t.Fatal(err)
	}
	for _, conn := range listed.Msg.GetConnections() {
		if conn.GetName() == "uploads" && (!conn.GetHeld() || len(conn.GetUses()) != 0) {
			t.Errorf("after the Disconnect, uploads lists as held %v with uses %v", conn.GetHeld(), conn.GetUses())
		}
	}
	// site goes back onto the bucket its own values name, with no files to leave.
	res, err := cat.SetCatalogNeed(t.Context(), connect.NewRequest(&dockwellv1.SetCatalogNeedRequest{Id: "site", Need: "storage", Choice: "connection:uploads"}))
	if err != nil {
		t.Fatalf("connecting site back to the bucket it writes in: %v", err)
	}
	if notes := notesOf(res.Msg.GetChange(), catalogapi.StartsEmptyNote); len(notes) != 0 {
		t.Errorf("going back onto its own bucket says %v", notes)
	}
}

// Back onto the bucket its own values name, an app whose need sets a key prefix finds
// its files only under the prefix it kept to: where its own value for it isn't the
// prefix the link keeps to, it starts empty there, asked for and said (slice 2
// re-check, N4).
func TestGoingBackOntoItsOwnBucketComparesThePrefix(t *testing.T) {
	t.Parallel()
	dir, raw := ghostWithStorage(t)
	prefixed := strings.NewReplacer(
		"      - name: S3_SECRET_ACCESS_KEY\n        part: PART_SECRET_KEY", "      - name: S3_SECRET_ACCESS_KEY\n        part: PART_SECRET_KEY\n      - name: S3_PREFIX\n        part: PART_KEY_PREFIX",
		"        storage__s3__secretAccessKey: ${S3_SECRET_ACCESS_KEY}", "        storage__s3__secretAccessKey: ${S3_SECRET_ACCESS_KEY}\n        storage__s3__prefix: ${S3_PREFIX}",
	).Replace(string(raw))
	if err := os.WriteFile(filepath.Join(dir, "ghost.yaml"), []byte(prefixed), 0o600); err != nil {
		t.Fatal(err)
	}
	f := startFull(t, store.SQLite, nil)
	b := signedIn(t, f)
	e := f.addDockerServer(t)
	pullsAnything(e)
	e.Registry[regexp.MustCompile(`image: (mysql:\S+)`).FindStringSubmatch(string(raw))[1]] = dockertest.Image{Config: docker.Config{Cmd: []string{"mysqld"}}}
	c := changeClient(b, b.origin)
	apply(t, c, propose(t, c, "Add uploads", connectionEdit("uploads", &resourcev1.ConnectionSpec{
		Kind: resourcev1.LinkKind_LINK_KIND_S3, Reach: resourcev1.ConnectionReach_CONNECTION_REACH_EVERY,
		S3: &resourcev1.ConnectionS3{Endpoint: "https://s3.example.com", Region: "eu-1", Bucket: "uploads", AccessKeyId: "AKIDEXAMPLE", SecretKey: connectionSecret},
	})))
	cat := catalogClient(b)
	if done := apply(t, c, installApp(t, cat, &dockwellv1.InstallCatalogEntryRequest{Id: "ghost", Project: "site", Needs: map[string]string{"storage": "connection:uploads"}})); done.GetState() != dockwellv1.ChangeState_CHANGE_STATE_APPLIED {
		t.Fatalf("installing Ghost ended %v: %s", done.GetState(), done.GetError())
	}
	cc := dockwellv1connect.NewConnectionServiceClient(b, b.origin, connect.WithProtoJSON())
	off, err := cc.DisconnectStorage(t.Context(), connect.NewRequest(&dockwellv1.DisconnectStorageRequest{Project: "site", Link: "storage"}))
	if err != nil {
		t.Fatal(err)
	}
	apply(t, c, off.Msg.GetChange())
	// Someone points its own prefix elsewhere in the bucket.
	list, err := cat.ListCatalogInstalls(t.Context(), connect.NewRequest(&dockwellv1.ListCatalogInstallsRequest{}))
	if err != nil {
		t.Fatal(err)
	}
	var env string
	for _, in := range list.Msg.GetInstalls() {
		if in.GetProject() == "site" {
			env = in.GetEnvironmentId()
		}
	}
	apply(t, c, propose(t, c, "Move site's prefix", &dockwellv1.Edit{
		Resource: &dockwellv1.ResourceRef{Kind: "variable", Scope: env, Name: "S3_PREFIX"},
		Spec:     &resourcev1.Spec{Kind: &resourcev1.Spec_Variable{Variable: &resourcev1.Variable{Value: "elsewhere/"}}},
	}))
	_, err = cat.SetCatalogNeed(t.Context(), connect.NewRequest(&dockwellv1.SetCatalogNeedRequest{Id: "site", Need: "storage", Choice: "connection:uploads"}))
	if reasonOf(err) != "cant_copy" || !strings.Contains(err.Error(), "site keeps its files under elsewhere/ in that bucket, and the link keeps to site/") {
		t.Errorf("going back under another prefix: %v", err)
	}
	res, err := cat.SetCatalogNeed(t.Context(), connect.NewRequest(&dockwellv1.SetCatalogNeedRequest{Id: "site", Need: "storage", Choice: "connection:uploads", StartEmpty: true}))
	if err != nil {
		t.Fatal(err)
	}
	if notes := notesOf(res.Msg.GetChange(), catalogapi.StartsEmptyNote); len(notes) != 1 {
		t.Errorf("going back under another prefix, starting empty, says %v", res.Msg.GetChange().GetPlan().GetNotes())
	}
}
