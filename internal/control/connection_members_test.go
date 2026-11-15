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
	"github.com/dockwell/dockwell/internal/store"
)

// A developer, then a viewer, of one project tries every way at a connection that
// reaches another project. It isn't listed, checked, linked or changed, and every
// refusal reads as for a connection that isn't there, so trying names tells them
// nothing (ADR 0099).
func TestAConnectionOfAnotherProjectReadsAsNotThere(t *testing.T) {
	t.Parallel()
	f := startFull(t, store.SQLite, nil)
	b := signedIn(t, f)
	e := f.addDockerServer(t)
	pullsAnything(e)
	env := listedEnv(t, b)
	project := envProject(t, f, env)
	c := changeClient(b, b.origin)
	cc := dockwellv1connect.NewConnectionServiceClient(b, b.origin, connect.WithProtoJSON())
	// The app's environment open, as staging is, where a developer applies.
	apply(t, c, propose(t, c, "Open production", &dockwellv1.Edit{
		Resource: &dockwellv1.ResourceRef{Kind: "environment", Scope: project, Name: "production"},
		Spec:     &resourcev1.Spec{Kind: &resourcev1.Spec_Environment{Environment: &resourcev1.Environment{DisplayName: "Production", Protection: resourcev1.Protection_PROTECTION_OPEN}}},
	}))

	notes := &dockwellv1.Edit{
		Resource: &dockwellv1.ResourceRef{Name: "notes"},
		Spec: &resourcev1.Spec{Kind: &resourcev1.Spec_Service{Service: &resourcev1.Service{
			Runs:    &resourcev1.Service_App{App: &resourcev1.Container{Image: "acme/notes:1"}},
			Secrets: map[string]string{"S3_SECRET_ACCESS_KEY": "S3_SECRET_ACCESS_KEY"},
		}}},
		Servers: []string{f.lastDocker.id.ServerID},
	}
	apply(t, c, propose(t, c, "Run the notes app", secretSpec("S3_SECRET_ACCESS_KEY", "old-secret"), notes))
	for _, name := range e.Containers() {
		if strings.Contains(name, "notes") {
			got, err := e.Client().InspectContainer(t.Context(), name)
			if err != nil {
				t.Fatal(err)
			}
			for n := range got.NetworkSettings.Networks {
				e.AddNetwork(docker.Network{Name: n, IPAM: docker.IPAM{Config: []docker.IPAMConfig{{Subnet: "127.0.0.0/8"}}}})
			}
		}
	}
	srv := s3test.New(t)
	apply(t, c, propose(t, c, "Add ours", connectionEdit("ours", &resourcev1.ConnectionSpec{
		DisplayName: "Ours", Kind: resourcev1.LinkKind_LINK_KIND_S3,
		Reach: resourcev1.ConnectionReach_CONNECTION_REACH_PROJECTS, Projects: []string{project},
		S3: &resourcev1.ConnectionS3{Endpoint: srv.URL(), Bucket: "ours", AccessKeyId: "AKIDOURS", SecretKey: connectionSecret, PathStyle: true},
	})))
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
		DisplayName: "Theirs", Kind: resourcev1.LinkKind_LINK_KIND_S3,
		Reach: resourcev1.ConnectionReach_CONNECTION_REACH_PROJECTS, Projects: []string{dataProject},
		S3: &resourcev1.ConnectionS3{Endpoint: srv.URL(), Bucket: "theirs", AccessKeyId: "AKIDTHEIRS", SecretKey: "theirs-secret", PathStyle: true},
	})))

	hold := aMember(t, f)
	for _, role := range []string{store.RoleDeveloper, store.RoleViewer} {
		hold(store.Grant{ProjectID: project, Role: role})
		who := role + " of one project"

		// The list shows ours, by name alone.
		listed, err := cc.ListConnections(t.Context(), connect.NewRequest(&dockwellv1.ListConnectionsRequest{}))
		if err != nil {
			t.Fatalf("%s lists: %v", who, err)
		}
		raw, _ := protojson.Marshal(listed.Msg)
		if strings.Contains(string(raw), "theirs") || strings.Contains(string(raw), srv.URL()) {
			t.Errorf("%s lists %s", who, raw)
		}
		// Narrowed to the other project, nothing.
		if got, err := cc.ListConnections(t.Context(), connect.NewRequest(&dockwellv1.ListConnectionsRequest{Project: "data"})); err == nil && len(got.Msg.GetConnections()) > 0 {
			t.Errorf("%s narrowed to data lists %v", who, got.Msg.GetConnections())
		}
		// Check is an admin's.
		if _, err := cc.CheckConnection(t.Context(), connect.NewRequest(&dockwellv1.CheckConnectionRequest{Name: "theirs-private-name"})); err == nil {
			t.Errorf("%s checks theirs", who)
		}
		// ConnectStorage to theirs, and to one that isn't there, read the same.
		_, errTheirs := cc.ConnectStorage(t.Context(), connect.NewRequest(&dockwellv1.ConnectStorageRequest{
			Project: project, App: "notes", Connection: "theirs-private-name",
			Sets: map[string]resourcev1.LinkPart{"S3_SECRET_ACCESS_KEY": resourcev1.LinkPart_LINK_PART_SECRET_KEY},
		}))
		_, errNone := cc.ConnectStorage(t.Context(), connect.NewRequest(&dockwellv1.ConnectStorageRequest{
			Project: project, App: "notes", Connection: "no-such-name",
			Sets: map[string]resourcev1.LinkPart{"S3_SECRET_ACCESS_KEY": resourcev1.LinkPart_LINK_PART_SECRET_KEY},
		}))
		if role == store.RoleDeveloper && (connect.CodeOf(errTheirs) != connect.CodeNotFound || connect.CodeOf(errNone) != connect.CodeNotFound) {
			t.Errorf("%s ConnectStorage: theirs %v, none %v", who, errTheirs, errNone)
		}
		// A link by hand to theirs, and to one that isn't there, read the same.
		linkTo := func(name string) error {
			_, err := c.ProposeChange(t.Context(), connect.NewRequest(&dockwellv1.ProposeChangeRequest{Summary: "Probe a name", Edits: []*dockwellv1.Edit{{
				Resource: &dockwellv1.ResourceRef{Kind: "link", Scope: env, Name: "guess"},
				Spec: &resourcev1.Spec{Kind: &resourcev1.Spec_Link{Link: &resourcev1.Link{
					Label: "Storage", Connection: name, Kind: resourcev1.LinkKind_LINK_KIND_S3,
					Sets: []*resourcev1.LinkSet{{Name: "S3_BUCKET", Part: resourcev1.LinkPart_LINK_PART_BUCKET}},
				}}},
			}}}))
			return err
		}
		eTheirs, eNone := linkTo("theirs-private-name"), linkTo("no-such-name")
		if eTheirs == nil || eNone == nil || strings.ReplaceAll(eTheirs.Error(), "theirs-private-name", "X") != strings.ReplaceAll(eNone.Error(), "no-such-name", "X") {
			t.Errorf("%s tells a connection of another project from none by a link:\n  theirs: %v\n  none:   %v", who, eTheirs, eNone)
		}
		// Rotating ours is an admin's of the install.
		_, err = c.ProposeChange(t.Context(), connect.NewRequest(&dockwellv1.ProposeChangeRequest{Summary: "Rotate", Edits: []*dockwellv1.Edit{
			connectionEdit("ours", &resourcev1.ConnectionSpec{
				DisplayName: "Ours", Kind: resourcev1.LinkKind_LINK_KIND_S3,
				Reach: resourcev1.ConnectionReach_CONNECTION_REACH_EVERY,
				S3:    &resourcev1.ConnectionS3{Endpoint: srv.URL(), Bucket: "ours", AccessKeyId: "AKIDOURS", SecretKey: "mine-now", PathStyle: true},
			}),
		}}))
		if err == nil {
			t.Errorf("%s changes ours", who)
		}
	}
}
