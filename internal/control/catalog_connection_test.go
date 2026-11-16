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
	"google.golang.org/protobuf/encoding/protojson"
	"google.golang.org/protobuf/proto"

	resourcev1 "github.com/dockwell/dockwell/gen/dockwell/resource/v1alpha1"
	dockwellv1 "github.com/dockwell/dockwell/gen/dockwell/v1"
	"github.com/dockwell/dockwell/internal/catalog"
	"github.com/dockwell/dockwell/internal/control/catalogapi"
	"github.com/dockwell/dockwell/internal/docker"
	"github.com/dockwell/dockwell/internal/docker/dockertest"
	"github.com/dockwell/dockwell/internal/store"
)

// An install takes a connection where the app needs storage: the sheet offers each
// one the new project may use, and the install links the app to it by name, each
// name the need sets reading the connection through the link, a key as a secret, so
// no plan holds a key and the app runs with the connection's values. The pin keeps
// the connection and the prefix, and an upgrade keeps both, planning nothing new for
// them; a connection that names projects can't reach the project an install makes
// (ADR 0099).
func TestAnInstallTakesAConnection(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	useCatalogDir(t, dir)
	raw, err := os.ReadFile(filepath.Join("testdata", "ghost-storage.yaml"))
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "ghost.yaml"), raw, 0o600); err != nil {
		t.Fatal(err)
	}
	f := startFull(t, store.SQLite, nil)
	b := signedIn(t, f)
	e := f.addDockerServer(t)
	pullsAnything(e)
	e.Registry[regexp.MustCompile(`image: (mysql:\S+)`).FindStringSubmatch(string(raw))[1]] = dockertest.Image{Config: docker.Config{Cmd: []string{"mysqld"}}}
	cat := catalogClient(b)
	c := changeClient(b, b.origin)

	apply(t, c, propose(t, c, "Add the buckets",
		connectionEdit("uploads", &resourcev1.ConnectionSpec{
			DisplayName: "Uploads", Kind: resourcev1.LinkKind_LINK_KIND_S3, Reach: resourcev1.ConnectionReach_CONNECTION_REACH_EVERY,
			S3: &resourcev1.ConnectionS3{Endpoint: "https://s3.example.com", Region: "eu-1", Bucket: "uploads", AccessKeyId: "AKIDEXAMPLE", SecretKey: connectionSecret},
		}),
		connectionEdit("named", &resourcev1.ConnectionSpec{
			Kind: resourcev1.LinkKind_LINK_KIND_S3, Reach: resourcev1.ConnectionReach_CONNECTION_REACH_PROJECTS, Projects: []string{envProject(t, f, listedEnv(t, b))},
			S3: &resourcev1.ConnectionS3{Endpoint: "https://s3.example.com", Bucket: "named", AccessKeyId: "AKIDEXAMPLE", SecretKey: connectionSecret},
		})))

	got, err := cat.GetCatalogEntry(t.Context(), connect.NewRequest(&dockwellv1.GetCatalogEntryRequest{Id: "ghost"}))
	if err != nil {
		t.Fatal(err)
	}
	var storage []string
	for _, o := range got.Msg.GetNeeds() {
		if o.GetNeed() == "storage" {
			storage = append(storage, o.GetChoice())
			if o.GetChoice() == "connection:uploads" && (o.GetConnection() != "uploads" || o.GetConnectionLabel() != "Uploads" || o.GetWhy() != "") {
				t.Errorf("the sheet offers uploads as %v", o)
			}
		}
	}
	if !slices.Equal(storage, []string{catalog.ChoiceUnset, "connection:uploads"}) {
		t.Fatalf("the sheet offers storage as %v", storage)
	}
	if _, err := cat.InstallCatalogEntry(t.Context(), connect.NewRequest(&dockwellv1.InstallCatalogEntryRequest{
		Id: "ghost", Project: "blog", Needs: map[string]string{"storage": "connection:named"},
	})); err == nil {
		t.Error("a new project took a connection that names other projects")
	}

	installed := installApp(t, cat, &dockwellv1.InstallCatalogEntryRequest{Id: "ghost", Project: "site", Needs: map[string]string{"storage": "connection:uploads"}})
	if raw, _ := protojson.Marshal(installed); strings.Contains(string(raw), connectionSecret) || strings.Contains(string(raw), "AKIDEXAMPLE") {
		t.Error("the install's plan holds a key")
	}
	// Its plan says the bucket isn't checked yet, and what Ghost needs it to allow.
	if notes := notesOf(installed, catalogapi.ConnectionUncheckedNote); len(notes) != 1 ||
		notes[0] != "Dockwell hasn't checked the connection Uploads from where Ghost runs yet, so a bucket that refuses Ghost shows only once it runs. Ghost needs it to allow signed URLs" {
		t.Errorf("the install's plan says %v", notes)
	}
	if done := apply(t, c, installed); done.GetState() != dockwellv1.ChangeState_CHANGE_STATE_APPLIED {
		t.Fatalf("installing Ghost ended %v: %s", done.GetState(), done.GetError())
	}
	ghostEnv := func() []string {
		t.Helper()
		for _, name := range slices.Backward(e.Containers()) {
			if strings.Contains(name, "ghost") {
				c, err := e.Client().InspectContainer(t.Context(), name)
				if err != nil {
					t.Fatal(err)
				}
				return c.Config.Env
			}
		}
		t.Fatal("Ghost doesn't run")
		return nil
	}
	env := ghostEnv()
	for _, want := range []string{
		"storage__active=s3", "storage__s3__endpoint=https://s3.example.com", "storage__s3__region=eu-1", "storage__s3__bucket=uploads",
		"storage__s3__accessKeyId=AKIDEXAMPLE", "storage__s3__secretAccessKey=" + connectionSecret,
	} {
		if !slices.Contains(env, want) {
			t.Errorf("Ghost runs without %s: %v", want, env)
		}
	}

	list, err := cat.ListCatalogInstalls(t.Context(), connect.NewRequest(&dockwellv1.ListCatalogInstallsRequest{}))
	if err != nil {
		t.Fatal(err)
	}
	var envID string
	for _, in := range list.Msg.GetInstalls() {
		if in.GetProject() == "site" {
			envID = in.GetEnvironmentId()
			if p := in.GetPin().GetNeeds()["storage"]; p.GetConnection() != "uploads" || p.GetPrefix() != "site/" {
				t.Errorf("the install pins storage as %v", p)
			}
		}
	}
	var link resourcev1.Link
	reads := map[string]*resourcev1.ConnectionRef{}
	if err := f.store.View(t.Context(), func(tx *store.Tx) error {
		r, err := tx.GetResource(t.Context(), "link", envID, "storage")
		if err != nil {
			return err
		}
		if err := proto.Unmarshal(r.Spec, &link); err != nil {
			return err
		}
		for _, name := range []string{"S3_ENDPOINT", "S3_REGION", "S3_BUCKET"} {
			var v resourcev1.Variable
			if r, err := tx.GetResource(t.Context(), "variable", envID, name); err == nil && proto.Unmarshal(r.Spec, &v) == nil {
				reads[name] = v.GetFrom().GetConnection()
			}
		}
		for _, name := range []string{"S3_ACCESS_KEY_ID", "S3_SECRET_ACCESS_KEY"} {
			r, err := tx.GetResource(t.Context(), "secret", envID, name)
			if err != nil {
				return err
			}
			opened, err := f.keeper.Open(t.Context(), tx, store.SpecAt{ResourceID: r.ID, Kind: r.Kind, Revision: r.Revision}, r.Spec)
			if err != nil {
				return err
			}
			var v resourcev1.Secret
			if err := proto.Unmarshal(opened, &v); err != nil {
				return err
			}
			if v.GetValue() != "" {
				t.Errorf("the secret %s holds a value of its own", name)
			}
			reads[name] = v.GetFrom().GetConnection()
		}
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	if link.GetConnection() != "uploads" || link.GetPrefix() != "site/" || len(link.GetSets()) != 5 ||
		!slices.Equal(link.GetFeatures(), []resourcev1.LinkFeature{resourcev1.LinkFeature_LINK_FEATURE_PRESIGNED_URLS}) {
		t.Errorf("the install links storage as %v", &link)
	}
	for _, name := range []string{"S3_ENDPOINT", "S3_REGION", "S3_BUCKET", "S3_ACCESS_KEY_ID", "S3_SECRET_ACCESS_KEY"} {
		if r := reads[name]; r.GetName() != "uploads" || r.GetLink() != "storage" {
			t.Errorf("%s reads %v", name, r)
		}
	}

	// An upgrade keeps it, and plans nothing new for the link or what reads it.
	next := strings.NewReplacer("release: 5", "release: 6", `version: "6.72.0"`, `version: "6.72.1"`, "retries: 24", "retries: 30").Replace(string(raw))
	if err := os.WriteFile(filepath.Join(dir, "ghost.yaml"), []byte(next), 0o600); err != nil {
		t.Fatal(err)
	}
	res, err := cat.UpgradeCatalogInstall(t.Context(), connect.NewRequest(&dockwellv1.UpgradeCatalogInstallRequest{Id: "site", SkipBackup: true}))
	if err != nil {
		t.Fatal(err)
	}
	if notes := notesOf(res.Msg.GetChange(), catalogapi.ConnectionUncheckedNote); len(notes) != 0 {
		t.Errorf("the upgrade's plan says %v", notes)
	}
	for _, r := range res.Msg.GetChange().GetPlan().GetResources() {
		kind := r.GetResource().GetKind()
		if (kind == "link" || kind == "variable" || kind == "secret") && r.GetAction() != dockwellv1.PlanAction_PLAN_ACTION_NONE {
			t.Errorf("upgrading Ghost plans %v for %s %s", r.GetAction(), kind, r.GetResource().GetName())
		}
	}
	if done := apply(t, c, res.Msg.GetChange()); done.GetState() != dockwellv1.ChangeState_CHANGE_STATE_APPLIED {
		t.Fatalf("upgrading Ghost ended %v: %s", done.GetState(), done.GetError())
	}
	if env := ghostEnv(); !slices.Contains(env, "storage__s3__secretAccessKey="+connectionSecret) {
		t.Errorf("upgraded, Ghost runs with %v", env)
	}
}
