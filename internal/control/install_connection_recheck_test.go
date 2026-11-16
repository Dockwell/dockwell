// SPDX-License-Identifier: AGPL-3.0-only

package control

import (
	"log/slog"
	"os"
	"path/filepath"
	"regexp"
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
)

func recheckSetup(t *testing.T, raw []byte) (*fullServer, *browser, dockwellv1connect.ChangeServiceClient, dockwellv1connect.CatalogServiceClient, *dockertest.Engine) {
	t.Helper()
	f := startFullLogging(t, store.SQLite, nil, slog.New(slog.NewTextHandler(t.Output(), &slog.HandlerOptions{Level: slog.LevelError})))
	b := signedIn(t, f)
	e := f.addDockerServer(t)
	pullsAnything(e)
	e.Registry[regexp.MustCompile(`image: (mysql:\S+)`).FindStringSubmatch(string(raw))[1]] = dockertest.Image{Config: docker.Config{Cmd: []string{"mysqld"}}}
	c := changeClient(b, b.origin)
	apply(t, c, propose(t, c, "Add uploads", connectionEdit("uploads", &resourcev1.ConnectionSpec{
		DisplayName: "Uploads", Kind: resourcev1.LinkKind_LINK_KIND_S3, Reach: resourcev1.ConnectionReach_CONNECTION_REACH_EVERY,
		S3: &resourcev1.ConnectionS3{Endpoint: "https://s3.example.com", Region: "eu-1", Bucket: "uploads", AccessKeyId: "AKIDEXAMPLE", SecretKey: connectionSecret, PathStyle: true},
	})))
	return f, b, c, catalogClient(b), e
}

func recheckLink(env, name, prefix string) *dockwellv1.Edit {
	return &dockwellv1.Edit{
		Resource: &dockwellv1.ResourceRef{Kind: "link", Scope: env, Name: name},
		Spec: &resourcev1.Spec{Kind: &resourcev1.Spec_Link{Link: &resourcev1.Link{
			Label: "Storage", Connection: "uploads", Kind: resourcev1.LinkKind_LINK_KIND_S3, Prefix: prefix,
			Sets: []*resourcev1.LinkSet{{Name: "S3_BUCKET_" + strings.ToUpper(name), Part: resourcev1.LinkPart_LINK_PART_BUCKET}},
		}}},
	}
}

func recheckBump(t *testing.T, dir, raw string) {
	t.Helper()
	next := strings.NewReplacer("release: 5", "release: 6", `version: "6.72.0"`, `version: "6.72.1"`, "retries: 24", "retries: 30").Replace(raw)
	if err := os.WriteFile(filepath.Join(dir, "ghost.yaml"), []byte(next), 0o600); err != nil {
		t.Fatal(err)
	}
}

func installEnv(t *testing.T, cat dockwellv1connect.CatalogServiceClient, project string) string {
	t.Helper()
	list, err := cat.ListCatalogInstalls(t.Context(), connect.NewRequest(&dockwellv1.ListCatalogInstallsRequest{}))
	if err != nil {
		t.Fatal(err)
	}
	for _, in := range list.Msg.GetInstalls() {
		if in.GetProject() == project {
			return in.GetEnvironmentId()
		}
	}
	t.Fatalf("no install %s", project)
	return ""
}

// M1 with a need that sets a path style and a key prefix: after a Disconnect, an
// upgrade keeps both values the environment holds.
func TestRecheckDisconnectKeepsPrefixAndPathStyle(t *testing.T) {
	t.Parallel()
	dir, raw := ghostWithStorage(t)
	s := strings.Replace(string(raw), "        part: PART_SECRET_KEY\n",
		"        part: PART_SECRET_KEY\n      - name: S3_PATH_STYLE\n        part: PART_PATH_STYLE\n        words:\n          - { value: \"on\", word: \"true\" }\n          - { value: \"off\", word: \"false\" }\n      - name: S3_PREFIX\n        part: PART_KEY_PREFIX\n", 1)
	s = strings.Replace(s, "        storage__s3__secretAccessKey: ${S3_SECRET_ACCESS_KEY}\n",
		"        storage__s3__secretAccessKey: ${S3_SECRET_ACCESS_KEY}\n        storage__s3__forcePathStyle: ${S3_PATH_STYLE}\n        storage__s3__prefix: ${S3_PREFIX}\n", 1)
	if err := os.WriteFile(filepath.Join(dir, "ghost.yaml"), []byte(s), 0o600); err != nil {
		t.Fatal(err)
	}
	_, b, c, cat, e := recheckSetup(t, raw)
	installed := installApp(t, cat, &dockwellv1.InstallCatalogEntryRequest{Id: "ghost", Project: "site", Needs: map[string]string{"storage": "connection:uploads"}})
	if done := apply(t, c, installed); done.GetState() != dockwellv1.ChangeState_CHANGE_STATE_APPLIED {
		t.Fatalf("install ended %v: %s", done.GetState(), done.GetError())
	}
	cc := dockwellv1connect.NewConnectionServiceClient(b, b.origin, connect.WithProtoJSON())
	off, err := cc.DisconnectStorage(t.Context(), connect.NewRequest(&dockwellv1.DisconnectStorageRequest{Project: "site", Link: "storage"}))
	if err != nil {
		t.Fatalf("disconnect: %v", err)
	}
	if done := apply(t, c, off.Msg.GetChange()); done.GetState() != dockwellv1.ChangeState_CHANGE_STATE_APPLIED {
		t.Fatalf("disconnect ended %v: %s", done.GetState(), done.GetError())
	}
	recheckBump(t, dir, s)
	res, err := cat.UpgradeCatalogInstall(t.Context(), connect.NewRequest(&dockwellv1.UpgradeCatalogInstallRequest{Id: "site", SkipBackup: true}))
	if err != nil {
		t.Fatalf("upgrade: %v", err)
	}
	for _, r := range res.Msg.GetChange().GetPlan().GetResources() {
		kind, name := r.GetResource().GetKind(), r.GetResource().GetName()
		if (kind == "link" || strings.HasPrefix(name, "S3_") || name == "STORAGE_DRIVER") && r.GetAction() != dockwellv1.PlanAction_PLAN_ACTION_NONE {
			t.Errorf("after a disconnect, the upgrade plans %v for %s %s", r.GetAction(), kind, name)
		}
	}
	if done := apply(t, c, res.Msg.GetChange()); done.GetState() != dockwellv1.ChangeState_CHANGE_STATE_APPLIED {
		t.Fatalf("upgrade ended %v: %s", done.GetState(), done.GetError())
	}
	var env []string
	for _, name := range e.Containers() {
		if strings.Contains(name, "ghost") {
			if got, err := e.Client().InspectContainer(t.Context(), name); err == nil {
				env = got.Config.Env
			}
		}
	}
	for _, want := range []string{"storage__s3__prefix=site/", "storage__s3__forcePathStyle=true", "storage__active=s3"} {
		if !slices.Contains(env, want) {
			t.Errorf("after disconnect and upgrade, Ghost runs without %s: %v", want, env)
		}
	}
}

// M1's rule: a developer can't Disconnect an install's connection-met storage, and
// it's refused at the call, not only in the panel.
func TestRecheckDeveloperDisconnectOfInstallStorage(t *testing.T) {
	t.Parallel()
	_, raw := ghostWithStorage(t)
	f, b, c, cat, _ := recheckSetup(t, raw)
	installed := installApp(t, cat, &dockwellv1.InstallCatalogEntryRequest{Id: "ghost", Project: "site", Needs: map[string]string{"storage": "connection:uploads"}})
	if done := apply(t, c, installed); done.GetState() != dockwellv1.ChangeState_CHANGE_STATE_APPLIED {
		t.Fatalf("install ended %v: %s", done.GetState(), done.GetError())
	}
	project := envProjectByName(t, f, "site")
	hold := aMember(t, f)
	hold(store.Grant{ProjectID: project, Role: store.RoleDeveloper})
	cc := dockwellv1connect.NewConnectionServiceClient(b, b.origin, connect.WithProtoJSON())
	off, err := cc.DisconnectStorage(t.Context(), connect.NewRequest(&dockwellv1.DisconnectStorageRequest{Project: "site", Link: "storage"}))
	t.Logf("developer disconnect: err=%v", err)
	if err == nil {
		done := apply(t, c, off.Msg.GetChange())
		t.Errorf("a developer's disconnect of an install's storage planned; apply ended %v: %s", done.GetState(), done.GetError())
	}
}

// H3: an install keeps its connection through an upgrade while another project's
// link reads it too.
func TestRecheckUpgradeKeepsItsConnectionBesideAnother(t *testing.T) {
	t.Parallel()
	dir, raw := ghostWithStorage(t)
	_, b, c, cat, _ := recheckSetup(t, raw)
	defaultEnv := listedEnv(t, b)
	installed := installApp(t, cat, &dockwellv1.InstallCatalogEntryRequest{Id: "ghost", Project: "site", Needs: map[string]string{"storage": "connection:uploads"}})
	if done := apply(t, c, installed); done.GetState() != dockwellv1.ChangeState_CHANGE_STATE_APPLIED {
		t.Fatalf("install ended %v: %s", done.GetState(), done.GetError())
	}
	apply(t, c, propose(t, c, "Link other", recheckLink(defaultEnv, "other", "other/")))
	recheckBump(t, dir, string(raw))
	res, err := cat.UpgradeCatalogInstall(t.Context(), connect.NewRequest(&dockwellv1.UpgradeCatalogInstallRequest{Id: "site", SkipBackup: true}))
	if err != nil {
		t.Fatalf("upgrade beside another project's link: %v", err)
	}
	for _, r := range res.Msg.GetChange().GetPlan().GetResources() {
		if r.GetResource().GetKind() == "link" && r.GetAction() != dockwellv1.PlanAction_PLAN_ACTION_NONE {
			t.Errorf("upgrade plans %v for link %s", r.GetAction(), r.GetResource().GetName())
		}
	}
	if done := apply(t, c, res.Msg.GetChange()); done.GetState() != dockwellv1.ChangeState_CHANGE_STATE_APPLIED {
		t.Fatalf("upgrade ended %v: %s", done.GetState(), done.GetError())
	}
}

// H3 inside one project: a release that adds required storage takes, by default, a
// connection another app of the same project already keeps its files on.
func TestRecheckNewNeedTakesAConnectionItsOwnProjectUses(t *testing.T) {
	t.Parallel()
	dir, raw := ghostWithStorage(t)
	full := string(raw)
	start := strings.Index(full, "  - name: storage\n")
	end := strings.Index(full, "compose: |")
	base := full[:start] + full[end:]
	base = regexp.MustCompile(`(?m)^ {8}storage__.*\n`).ReplaceAllString(base, "")
	if err := os.WriteFile(filepath.Join(dir, "ghost.yaml"), []byte(base), 0o600); err != nil {
		t.Fatal(err)
	}
	_, _, c, cat, _ := recheckSetup(t, raw)
	installed := installApp(t, cat, &dockwellv1.InstallCatalogEntryRequest{Id: "ghost", Project: "site"})
	if done := apply(t, c, installed); done.GetState() != dockwellv1.ChangeState_CHANGE_STATE_APPLIED {
		t.Fatalf("install ended %v: %s", done.GetState(), done.GetError())
	}
	if os.Getenv("RECHECK_NO_MEDIA") == "" {
		apply(t, c, propose(t, c, "Link media", recheckLink(installEnv(t, cat, "site"), "media", "media/")))
	}
	required := strings.Replace(full, "    kind: KIND_S3\n    optional: true\n", "    kind: KIND_S3\n", 1)
	required = regexp.MustCompile(`      - name: STORAGE_DRIVER\n        part: PART_MET\n        words:\n.*\n.*\n`).ReplaceAllString(required, "")
	required = strings.Replace(required, "${STORAGE_DRIVER}", "s3", 1)
	recheckBump(t, dir, required)
	res, err := cat.UpgradeCatalogInstall(t.Context(), connect.NewRequest(&dockwellv1.UpgradeCatalogInstallRequest{Id: "site", SkipBackup: true}))
	if err != nil {
		t.Logf("upgrade refused: %v", err)
		return
	}
	for _, r := range res.Msg.GetChange().GetPlan().GetResources() {
		if r.GetResource().GetKind() == "link" && r.GetResource().GetName() == "storage" && r.GetAction() == dockwellv1.PlanAction_PLAN_ACTION_CREATE {
			t.Errorf("a release adding required storage takes uploads, which media in the same project uses, by default")
		}
	}
	for _, n := range res.Msg.GetChange().GetPlan().GetNotes() {
		t.Logf("note %s: %s", n.GetReason(), n.GetMessage())
	}
}

// H2: a link at the bucket's root blocks every other link, and the words.
func TestRecheckRootLinkBlocksAll(t *testing.T) {
	t.Parallel()
	f := startFull(t, store.SQLite, nil)
	b := signedIn(t, f)
	env := listedEnv(t, b)
	c := changeClient(b, b.origin)
	apply(t, c, propose(t, c, "Add uploads", connectionEdit("uploads", &resourcev1.ConnectionSpec{
		DisplayName: "Uploads", Kind: resourcev1.LinkKind_LINK_KIND_S3, Reach: resourcev1.ConnectionReach_CONNECTION_REACH_EVERY,
		S3: &resourcev1.ConnectionS3{Endpoint: "https://s3.example.com", Bucket: "uploads", AccessKeyId: "AKIDEXAMPLE", SecretKey: connectionSecret},
	}), recheckLink(env, "root", "")))
	_, err := c.ProposeChange(t.Context(), connect.NewRequest(&dockwellv1.ProposeChangeRequest{Summary: "Link ghost", Edits: []*dockwellv1.Edit{recheckLink(env, "ghost", "ghost/")}}))
	if err == nil || !strings.Contains(err.Error(), "Give it a connection of its own") {
		t.Errorf("a link beside one at the root: %v, want it refused, with a connection of its own as the fix", err)
	}
	// Renaming the label of the root link keeps it.
	apply(t, c, propose(t, c, "Keep root", recheckLink(env, "root", "")))
}
