// SPDX-License-Identifier: AGPL-3.0-only

package control

import (
	"slices"
	"strings"
	"testing"

	"connectrpc.com/connect"
	"google.golang.org/protobuf/encoding/protojson"

	resourcev1 "github.com/dockwell/dockwell/gen/dockwell/resource/v1alpha1"
	dockwellv1 "github.com/dockwell/dockwell/gen/dockwell/v1"
	"github.com/dockwell/dockwell/gen/dockwell/v1/dockwellv1connect"
	"github.com/dockwell/dockwell/internal/backup/s3/s3test"
	"github.com/dockwell/dockwell/internal/docker"
	"github.com/dockwell/dockwell/internal/resource/connection"
	"github.com/dockwell/dockwell/internal/store"
)

// An app that reads S3 names is connected to a connection's bucket: the bucket is
// checked from the app's network on its server first, then one change links the app
// to the connection, its names reading the connection through the link, a key as a
// secret, and the app runs with them. No key is in the plan; a new key reaches every
// app linked to the connection in one plan; a check that fails, or a connection the
// project may not use, plans nothing; a value from a connection that no link writes
// is refused, whoever proposes it (ADR 0099).
func TestAnAppIsConnectedToStorage(t *testing.T) {
	t.Parallel()
	f := startFull(t, store.SQLite, nil)
	b := signedIn(t, f)
	e := f.addDockerServer(t)
	pullsAnything(e)
	env := listedEnv(t, b)
	project := envProject(t, f, env)
	c := changeClient(b, b.origin)
	cc := dockwellv1connect.NewConnectionServiceClient(b, b.origin, connect.WithProtoJSON())

	// The app, reading its storage's names, with values of its own for now.
	notes := &dockwellv1.Edit{
		Resource: &dockwellv1.ResourceRef{Name: "notes"},
		Spec: &resourcev1.Spec{Kind: &resourcev1.Spec_Service{Service: &resourcev1.Service{
			Runs:      &resourcev1.Service_App{App: &resourcev1.Container{Image: "acme/notes:1"}},
			Variables: map[string]string{"S3_ENDPOINT": "S3_ENDPOINT", "S3_BUCKET": "S3_BUCKET", "S3_FORCE_PATH_STYLE": "S3_FORCE_PATH_STYLE"},
			Secrets:   map[string]string{"S3_ACCESS_KEY_ID": "S3_ACCESS_KEY_ID", "S3_SECRET_ACCESS_KEY": "S3_SECRET_ACCESS_KEY"},
		}}},
		Servers: []string{f.lastDocker.id.ServerID},
	}
	apply(t, c, propose(t, c, "Run the notes app",
		variableSpec("S3_ENDPOINT", "http://old.example.com"), variableSpec("S3_BUCKET", "old"), variableSpec("S3_FORCE_PATH_STYLE", "false"),
		secretSpec("S3_ACCESS_KEY_ID", "old-id"), secretSpec("S3_SECRET_ACCESS_KEY", "old-secret"), notes))
	appEnv := func(app string) []string {
		t.Helper()
		for _, name := range e.Containers() {
			if strings.Contains(name, app) {
				got, err := e.Client().InspectContainer(t.Context(), name)
				if err != nil {
					t.Fatal(err)
				}
				return got.Config.Env
			}
		}
		t.Fatalf("the %s app doesn't run", app)
		return nil
	}
	// Its network holds this machine's loopback, where the fake S3 listens.
	onLoopback := func(app string) {
		t.Helper()
		for _, name := range e.Containers() {
			if !strings.Contains(name, app) {
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
	}
	onLoopback("notes")

	srv := s3test.New(t)
	apply(t, c, propose(t, c, "Add the uploads bucket", connectionEdit("uploads", &resourcev1.ConnectionSpec{
		DisplayName: "Uploads", Kind: resourcev1.LinkKind_LINK_KIND_S3,
		Reach: resourcev1.ConnectionReach_CONNECTION_REACH_PROJECTS, Projects: []string{project},
		S3: &resourcev1.ConnectionS3{Endpoint: srv.URL(), Bucket: "uploads", AccessKeyId: "AKIDEXAMPLE", SecretKey: connectionSecret, PathStyle: true},
	})))
	sets := map[string]resourcev1.LinkPart{
		"S3_ENDPOINT": resourcev1.LinkPart_LINK_PART_ENDPOINT, "S3_BUCKET": resourcev1.LinkPart_LINK_PART_BUCKET,
		"S3_ACCESS_KEY_ID": resourcev1.LinkPart_LINK_PART_ACCESS_KEY_ID, "S3_SECRET_ACCESS_KEY": resourcev1.LinkPart_LINK_PART_SECRET_KEY,
		"S3_FORCE_PATH_STYLE": resourcev1.LinkPart_LINK_PART_PATH_STYLE,
	}
	connectIt := func() (*connect.Response[dockwellv1.ConnectStorageResponse], error) {
		return cc.ConnectStorage(t.Context(), connect.NewRequest(&dockwellv1.ConnectStorageRequest{
			Project: project, App: "notes", Connection: "uploads", Sets: sets, PresignedUrls: true,
		}))
	}

	// A check that fails plans nothing, and says the fact and the fix.
	srv.Refuse = "AccessDenied"
	_, err := connectIt()
	srv.Refuse = ""
	if connect.CodeOf(err) != connect.CodeFailedPrecondition || !strings.Contains(err.Error(), "AccessDenied") || !strings.Contains(err.Error(), "Check the connection's keys") {
		t.Errorf("a bucket that refuses the check: %v", err)
	}

	planned, err := connectIt()
	if err != nil {
		t.Fatal(err)
	}
	if !planned.Msg.GetCheck().GetPassed() {
		t.Errorf("planned on a check that says %v", planned.Msg.GetCheck())
	}
	if raw, _ := protojson.Marshal(planned.Msg); strings.Contains(string(raw), connectionSecret) {
		t.Error("the plan holds the secret key")
	}
	if done := apply(t, c, planned.Msg.GetChange()); done.GetState() != dockwellv1.ChangeState_CHANGE_STATE_APPLIED {
		t.Fatalf("connecting the app ended %v: %s", done.GetState(), done.GetError())
	}
	got := appEnv("notes")
	for _, want := range []string{"S3_ENDPOINT=" + srv.URL(), "S3_BUCKET=uploads", "S3_ACCESS_KEY_ID=AKIDEXAMPLE", "S3_SECRET_ACCESS_KEY=" + connectionSecret, "S3_FORCE_PATH_STYLE=true"} {
		if !slices.Contains(got, want) {
			t.Errorf("the app runs without %s: %v", want, got)
		}
	}

	// A second app reads the same bucket through a link of its own, by names of its own.
	gallery := &dockwellv1.Edit{
		Resource: &dockwellv1.ResourceRef{Name: "gallery"},
		Spec: &resourcev1.Spec{Kind: &resourcev1.Spec_Service{Service: &resourcev1.Service{
			Runs:      &resourcev1.Service_App{App: &resourcev1.Container{Image: "acme/gallery:1"}},
			Variables: map[string]string{"MEDIA_ENDPOINT": "MEDIA_ENDPOINT", "MEDIA_BUCKET": "MEDIA_BUCKET"},
			Secrets:   map[string]string{"MEDIA_KEY_ID": "MEDIA_KEY_ID", "MEDIA_SECRET": "MEDIA_SECRET"},
		}}},
		Servers: []string{f.lastDocker.id.ServerID},
	}
	apply(t, c, propose(t, c, "Run the gallery app",
		variableSpec("MEDIA_ENDPOINT", "http://old.example.com"), variableSpec("MEDIA_BUCKET", "old"),
		secretSpec("MEDIA_KEY_ID", "old-id"), secretSpec("MEDIA_SECRET", "old-secret"), gallery))
	onLoopback("gallery")
	second, err := cc.ConnectStorage(t.Context(), connect.NewRequest(&dockwellv1.ConnectStorageRequest{
		Project: project, App: "gallery", Connection: "uploads", Link: "media", Prefix: "gallery/",
		Sets: map[string]resourcev1.LinkPart{
			"MEDIA_ENDPOINT": resourcev1.LinkPart_LINK_PART_ENDPOINT, "MEDIA_BUCKET": resourcev1.LinkPart_LINK_PART_BUCKET,
			"MEDIA_KEY_ID": resourcev1.LinkPart_LINK_PART_ACCESS_KEY_ID, "MEDIA_SECRET": resourcev1.LinkPart_LINK_PART_SECRET_KEY,
		},
	}))
	if err != nil {
		t.Fatal(err)
	}
	if done := apply(t, c, second.Msg.GetChange()); done.GetState() != dockwellv1.ChangeState_CHANGE_STATE_APPLIED {
		t.Fatalf("connecting the gallery ended %v: %s", done.GetState(), done.GetError())
	}
	if got := appEnv("gallery"); !slices.Contains(got, "MEDIA_SECRET="+connectionSecret) {
		t.Errorf("the gallery runs without the connection's key: %v", got)
	}

	// A new key for the connection reaches both apps in one plan, which holds no key.
	const newKey = "a-new-secret-key"
	plan := propose(t, c, "Rotate the uploads key", connectionEdit("uploads", &resourcev1.ConnectionSpec{
		DisplayName: "Uploads", Kind: resourcev1.LinkKind_LINK_KIND_S3,
		Reach: resourcev1.ConnectionReach_CONNECTION_REACH_PROJECTS, Projects: []string{project},
		S3: &resourcev1.ConnectionS3{Endpoint: srv.URL(), Bucket: "uploads", AccessKeyId: "AKIDEXAMPLE", SecretKey: newKey, PathStyle: true},
	}))
	if raw, _ := protojson.Marshal(plan); strings.Contains(string(raw), newKey) || strings.Contains(string(raw), connectionSecret) {
		t.Error("the rotation's plan holds a key")
	}
	var restarted []string
	for _, srv := range plan.GetPlan().GetServers() {
		for _, step := range srv.GetSteps() {
			restarted = append(restarted, step.GetResource().GetName())
		}
	}
	for _, app := range []string{"notes", "gallery"} {
		if !slices.Contains(restarted, app) {
			t.Errorf("the rotation's plan leaves out %s: it has steps for %v", app, restarted)
		}
	}
	if rotated := apply(t, c, plan); rotated.GetState() != dockwellv1.ChangeState_CHANGE_STATE_APPLIED {
		t.Fatalf("rotating the key ended %v: %s", rotated.GetState(), rotated.GetError())
	}
	if got := appEnv("notes"); !slices.Contains(got, "S3_SECRET_ACCESS_KEY="+newKey) {
		t.Errorf("after the key's rotation, the notes app runs with %v", got)
	}
	if got := appEnv("gallery"); !slices.Contains(got, "MEDIA_SECRET="+newKey) {
		t.Errorf("after the key's rotation, the gallery runs with %v", got)
	}

	// A value from a connection that no link writes is refused, made by hand.
	_, err = c.ProposeChange(t.Context(), connect.NewRequest(&dockwellv1.ProposeChangeRequest{Summary: "Read it by hand", Edits: []*dockwellv1.Edit{{
		Resource: &dockwellv1.ResourceRef{Kind: "secret", Name: "STOLEN"},
		Spec: &resourcev1.Spec{Kind: &resourcev1.Spec_Secret{Secret: &resourcev1.Secret{From: &resourcev1.ValueFrom{Source: &resourcev1.ValueFrom_Connection{Connection: &resourcev1.ConnectionRef{
			Scope: orgID(t, f), Name: "uploads", Part: resourcev1.LinkPart_LINK_PART_SECRET_KEY, Link: "nothing",
		}}}}}},
	}}}))
	if connect.CodeOf(err) != connect.CodeInvalidArgument || !strings.Contains(err.Error(), "only a link writes") {
		t.Errorf("a value from a connection with no link: %v", err)
	}

	// A connection an app reads can't stop reaching the app's project: the change
	// takes in what reads it, which may no longer.
	_, err = c.ProposeChange(t.Context(), connect.NewRequest(&dockwellv1.ProposeChangeRequest{Summary: "Reach no project", Edits: []*dockwellv1.Edit{
		connectionEdit("uploads", &resourcev1.ConnectionSpec{
			DisplayName: "Uploads", Kind: resourcev1.LinkKind_LINK_KIND_S3,
			S3: &resourcev1.ConnectionS3{Endpoint: srv.URL(), Bucket: "uploads", PathStyle: true},
		}),
	}}))
	if connect.CodeOf(err) != connect.CodeInvalidArgument || !strings.Contains(err.Error(), "which isn't one this project can use") {
		t.Errorf("narrowing a connection an app reads: %v", err)
	}

	// Nor can it go while an app reads it, which the refusal names.
	_, err = c.ProposeChange(t.Context(), connect.NewRequest(&dockwellv1.ProposeChangeRequest{Summary: "Delete it", Edits: []*dockwellv1.Edit{
		{Resource: &dockwellv1.ResourceRef{Kind: string(connection.Kind), Name: "uploads"}, Delete: true},
	}}))
	if connect.CodeOf(err) != connect.CodeInvalidArgument || !strings.Contains(err.Error(), "uploads") {
		t.Errorf("deleting a connection an app reads: %v", err)
	}

	// A connection the project may not use reads as not there.
	apply(t, c, propose(t, c, "Add a bucket for no project", connectionEdit("elsewhere", &resourcev1.ConnectionSpec{
		DisplayName: "Elsewhere", Kind: resourcev1.LinkKind_LINK_KIND_S3,
		S3: &resourcev1.ConnectionS3{Endpoint: srv.URL(), Bucket: "elsewhere", AccessKeyId: "AKIDOTHER", SecretKey: "another-secret", PathStyle: true},
	})))
	_, err = cc.ConnectStorage(t.Context(), connect.NewRequest(&dockwellv1.ConnectStorageRequest{
		Project: project, App: "notes", Connection: "elsewhere", Sets: sets,
	}))
	if connect.CodeOf(err) != connect.CodeNotFound {
		t.Errorf("connecting to a connection the project may not use: %v", err)
	}
}

// orgID is the install's organization's ID.
func orgID(t *testing.T, f *fullServer) string {
	t.Helper()
	var out string
	if err := f.store.View(t.Context(), func(tx *store.Tx) error {
		org, err := tx.Organization(t.Context())
		out = org.ID
		return err
	}); err != nil {
		t.Fatal(err)
	}
	return out
}
