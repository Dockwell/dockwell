// SPDX-License-Identifier: AGPL-3.0-only

package control

import (
	"strings"
	"testing"

	"connectrpc.com/connect"
	"google.golang.org/protobuf/encoding/protojson"

	resourcev1 "github.com/dockwell/dockwell/gen/dockwell/resource/v1alpha1"
	dockwellv1 "github.com/dockwell/dockwell/gen/dockwell/v1"
	"github.com/dockwell/dockwell/gen/dockwell/v1/dockwellv1connect"
	"github.com/dockwell/dockwell/internal/backup/s3/s3test"
	"github.com/dockwell/dockwell/internal/docker"
	"github.com/dockwell/dockwell/internal/docker/dockertest"
	"github.com/dockwell/dockwell/internal/resource/connection"
	"github.com/dockwell/dockwell/internal/store"
)

const connectionSecret = "the-connections-secret-key"

func connectionEdit(name string, c *resourcev1.ConnectionSpec) *dockwellv1.Edit {
	return &dockwellv1.Edit{
		Resource: &dockwellv1.ResourceRef{Kind: string(connection.Kind), Name: name},
		Spec:     &resourcev1.Spec{Kind: &resourcev1.Spec_Connection{Connection: c}},
	}
}

// An admin saves a connection to a bucket as a change, lists it with everything but
// its keys, and checks it from an app's network through the app's server, as the app
// would reach it. Its secret key is in no plan, change or answer, and a saved one
// moved elsewhere needs its key again. Below an admin, a developer sees connections
// their projects may use by name alone, and neither checks nor saves one (ADR 0099).
func TestAConnectionIsSavedListedAndChecked(t *testing.T) {
	t.Parallel()
	f := startFull(t, store.SQLite, nil)
	b := signedIn(t, f)
	e := f.addDockerServer(t)
	env := listedEnv(t, b)
	project := envProject(t, f, env)
	dproject := docker.Project(env)
	// The app's network holds this machine's loopback, where the fake S3 listens.
	e.AddNetwork(docker.Network{Name: dproject + "_default", IPAM: docker.IPAM{Config: []docker.IPAMConfig{{Subnet: "127.0.0.0/8"}}}})
	e.AddLocalImage("ghcr.io/acme/notes:1", dockertest.Image{Config: docker.Config{Cmd: []string{"notes"}}})
	client := e.Client()
	id, err := client.CreateContainer(t.Context(), dproject+"-notes-1", &docker.CreateRequest{
		Config:     docker.Config{Image: "ghcr.io/acme/notes:1", Labels: map[string]string{docker.LabelProject: dproject, docker.LabelService: "notes", docker.LabelNumber: "1"}},
		HostConfig: docker.HostConfig{NetworkMode: dproject + "_default"},
	})
	if err != nil {
		t.Fatal(err)
	}
	if err := client.StartContainer(t.Context(), id); err != nil {
		t.Fatal(err)
	}
	srv := s3test.New(t)
	c := changeClient(b, b.origin)
	cc := dockwellv1connect.NewConnectionServiceClient(b, b.origin, connect.WithProtoJSON())

	saved := apply(t, c, propose(t, c, "Add the uploads bucket", connectionEdit("uploads", &resourcev1.ConnectionSpec{
		DisplayName: "Uploads", Kind: resourcev1.LinkKind_LINK_KIND_S3,
		Reach: resourcev1.ConnectionReach_CONNECTION_REACH_PROJECTS, Projects: []string{project},
		S3: &resourcev1.ConnectionS3{Endpoint: srv.URL(), Bucket: "uploads", AccessKeyId: "AKIDEXAMPLE", SecretKey: connectionSecret, PathStyle: true},
	})))
	if saved.GetState() != dockwellv1.ChangeState_CHANGE_STATE_APPLIED {
		t.Fatalf("saving the connection ended %v: %s", saved.GetState(), saved.GetError())
	}
	if raw, _ := protojson.Marshal(saved); strings.Contains(string(raw), connectionSecret) {
		t.Error("the change holds the secret key")
	}

	listed, err := cc.ListConnections(t.Context(), connect.NewRequest(&dockwellv1.ListConnectionsRequest{}))
	if err != nil {
		t.Fatal(err)
	}
	if l := listed.Msg.GetConnections(); len(l) != 1 || l[0].GetBucket() != "uploads" || l[0].GetEndpoint() != srv.URL() || len(l[0].GetProjects()) != 1 {
		t.Fatalf("an admin lists %v", l)
	}
	if raw, _ := protojson.Marshal(listed.Msg); strings.Contains(string(raw), connectionSecret) || strings.Contains(string(raw), "AKIDEXAMPLE") {
		t.Error("the list holds a key")
	}

	// Checked from the app's network, as the app reaches it.
	got, err := cc.CheckConnection(t.Context(), connect.NewRequest(&dockwellv1.CheckConnectionRequest{Name: "uploads", Environment: env, App: "notes", Presign: true, Prefix: "notes/"}))
	if err != nil {
		t.Fatal(err)
	}
	if !got.Msg.GetPassed() || got.Msg.GetServer() == "" {
		t.Errorf("the saved connection, from the app's network, checks as %v", got.Msg)
	}
	if left := srv.Objects(); len(left) != 0 {
		t.Errorf("the check left %v", left)
	}
	// What the storage refuses comes back as its code.
	srv.Refuse = "SignatureDoesNotMatch"
	got, err = cc.CheckConnection(t.Context(), connect.NewRequest(&dockwellv1.CheckConnectionRequest{Name: "uploads", Environment: env, App: "notes"}))
	srv.Refuse = ""
	if err != nil {
		t.Fatal(err)
	}
	if got.Msg.GetPassed() || got.Msg.GetCode() != "SignatureDoesNotMatch" || got.Msg.GetMessage() != "" {
		t.Errorf("a refused key checks as %v", got.Msg)
	}
	if raw, _ := protojson.Marshal(got.Msg); strings.Contains(string(raw), connectionSecret) {
		t.Error("the check's answer holds the secret key")
	}
	// A saved connection checked somewhere else needs its key again.
	_, err = cc.CheckConnection(t.Context(), connect.NewRequest(&dockwellv1.CheckConnectionRequest{Name: "uploads", S3: &resourcev1.ConnectionS3{Endpoint: "https://elsewhere.example.com"}}))
	if connect.CodeOf(err) != connect.CodeInvalidArgument || !strings.Contains(err.Error(), "needs its secret key again") {
		t.Errorf("checking a saved connection at another endpoint: %v", err)
	}
	// A prefix that climbs out of the bucket is refused before the check runs.
	_, err = cc.CheckConnection(t.Context(), connect.NewRequest(&dockwellv1.CheckConnectionRequest{Name: "uploads", Prefix: "notes/../../other/"}))
	if connect.CodeOf(err) != connect.CodeInvalidArgument || !strings.Contains(err.Error(), "isn't a prefix in a bucket") {
		t.Errorf("checking under a prefix out of the bucket: %v", err)
	}

	// Below an admin: names alone, of what reaches their project, and nothing to
	// check or save.
	hold := aMember(t, f)
	hold(store.Grant{ProjectID: project, Role: store.RoleDeveloper})
	trimmed, err := cc.ListConnections(t.Context(), connect.NewRequest(&dockwellv1.ListConnectionsRequest{}))
	if err != nil {
		t.Fatal(err)
	}
	if l := trimmed.Msg.GetConnections(); len(l) != 1 || l[0].GetName() != "uploads" || l[0].GetEndpoint() != "" || l[0].GetBucket() != "" || len(l[0].GetProjects()) != 0 {
		t.Errorf("a developer of its project lists %v", l)
	}
	_, err = cc.CheckConnection(t.Context(), connect.NewRequest(&dockwellv1.CheckConnectionRequest{Name: "uploads"}))
	needsRole(t, "a developer checking a connection", err)
	_, err = c.ProposeChange(t.Context(), connect.NewRequest(&dockwellv1.ProposeChangeRequest{Summary: "Another", Edits: []*dockwellv1.Edit{
		connectionEdit("mine", &resourcev1.ConnectionSpec{Kind: resourcev1.LinkKind_LINK_KIND_S3, S3: &resourcev1.ConnectionS3{Endpoint: srv.URL(), Bucket: "mine", AccessKeyId: "a", SecretKey: "b"}}),
	}}))
	if err == nil {
		t.Error("a developer planned a connection")
	}
	hold(store.Grant{ProjectID: "prj_01jwd8r6q4wexz3c6v0wzm5k8g", Role: store.RoleDeveloper})
	if other, err := cc.ListConnections(t.Context(), connect.NewRequest(&dockwellv1.ListConnectionsRequest{})); err != nil || len(other.Msg.GetConnections()) != 0 {
		t.Errorf("a developer of another project lists %v, %v", other, err)
	}
}
