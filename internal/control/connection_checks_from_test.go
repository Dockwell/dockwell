// SPDX-License-Identifier: AGPL-3.0-only

package control

import (
	"slices"
	"strings"
	"testing"
	"time"

	"connectrpc.com/connect"

	resourcev1 "github.com/dockwell/dockwell/gen/dockwell/resource/v1alpha1"
	dockwellv1 "github.com/dockwell/dockwell/gen/dockwell/v1"
	"github.com/dockwell/dockwell/gen/dockwell/v1/dockwellv1connect"
	"github.com/dockwell/dockwell/internal/backup/s3/s3test"
	"github.com/dockwell/dockwell/internal/docker"
	"github.com/dockwell/dockwell/internal/probe"
	"github.com/dockwell/dockwell/internal/store"
	"github.com/dockwell/dockwell/internal/store/storetest"
)

// A connection no app is linked to is checked from the control server, on a network
// of the check's own; once an app is linked, from the app's server, on the app's
// networks and under its link's prefix, as the app reaches it, and ConnectStorage
// checks it (#799's review, M2). The stand-in listens on loopback, which only the
// app's network here takes, so the scheduled read passes only from there, and writes
// nothing.
func TestAConnectionIsCheckedFromWhereItsAppIs(t *testing.T) {
	t.Parallel()
	storetest.Each(t, func(t *testing.T, backend store.Backend) {
		f := startFull(t, backend, nil)
		b := signedIn(t, f)
		e := f.addDockerServer(t)
		pullsAnything(e)
		f.config.Fleet.SetControlServer(f.lastDocker.id.ServerID)
		env := listedEnv(t, b)
		project := envProject(t, f, env)
		c := changeClient(b, b.origin)
		cc := dockwellv1connect.NewConnectionServiceClient(b, b.origin, connect.WithProtoJSON())

		notes := &dockwellv1.Edit{
			Resource: &dockwellv1.ResourceRef{Name: "notes"},
			Spec: &resourcev1.Spec{Kind: &resourcev1.Spec_Service{Service: &resourcev1.Service{
				Runs:      &resourcev1.Service_App{App: &resourcev1.Container{Image: "acme/notes:1"}},
				Variables: map[string]string{"S3_ENDPOINT": "S3_ENDPOINT", "S3_BUCKET": "S3_BUCKET", "S3_PREFIX": "S3_PREFIX"},
				Secrets:   map[string]string{"S3_ACCESS_KEY_ID": "S3_ACCESS_KEY_ID", "S3_SECRET_ACCESS_KEY": "S3_SECRET_ACCESS_KEY"},
			}}},
			Servers: []string{f.lastDocker.id.ServerID},
		}
		apply(t, c, propose(t, c, "Run the notes app",
			variableSpec("S3_ENDPOINT", "http://old.example.com"), variableSpec("S3_BUCKET", "old"), variableSpec("S3_PREFIX", ""),
			secretSpec("S3_ACCESS_KEY_ID", "old-id"), secretSpec("S3_SECRET_ACCESS_KEY", "old-secret"), notes))
		// Its network holds this machine's loopback, where the fake S3 listens.
		for _, name := range e.Containers() {
			if !strings.Contains(name, "notes") {
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
		apply(t, c, propose(t, c, "Add the uploads bucket", connectionEdit("uploads", &resourcev1.ConnectionSpec{
			DisplayName: "Uploads", Kind: resourcev1.LinkKind_LINK_KIND_S3,
			Reach: resourcev1.ConnectionReach_CONNECTION_REACH_PROJECTS, Projects: []string{project},
			S3: &resourcev1.ConnectionS3{Endpoint: srv.URL(), Bucket: "uploads", AccessKeyId: "AKIDEXAMPLE", SecretKey: connectionSecret, PathStyle: true},
		})))

		checker := f.server.connectionChecks
		clock := time.Unix(1_800_000_000, 0)
		checker.Now = func() time.Time { return clock }
		look := func() *dockwellv1.CheckConnectionResponse {
			t.Helper()
			checker.Check(t.Context())
			checker.Wait()
			res, err := cc.ListConnections(t.Context(), connect.NewRequest(&dockwellv1.ListConnectionsRequest{}))
			if err != nil {
				t.Fatal(err)
			}
			checks := res.Msg.GetConnections()[0].GetChecks()
			if len(checks) != 1 {
				t.Fatalf("it lists %v, want one server's check", checks)
			}
			return checks[0]
		}

		// No app linked: from the control server's own network, which loopback isn't on.
		if got := look(); got.GetPassed() || got.GetCode() != probe.CodeRefused || got.GetStep() != probe.StepList {
			t.Errorf("with no app linked, it checks as %v, want refused from a network of its own", got)
		}

		// Linked, as ConnectStorage links it, from the app's network.
		planned, err := cc.ConnectStorage(t.Context(), connect.NewRequest(&dockwellv1.ConnectStorageRequest{
			Project: project, App: "notes", Connection: "uploads",
			Sets: map[string]resourcev1.LinkPart{
				"S3_ENDPOINT": resourcev1.LinkPart_LINK_PART_ENDPOINT, "S3_BUCKET": resourcev1.LinkPart_LINK_PART_BUCKET,
				"S3_ACCESS_KEY_ID": resourcev1.LinkPart_LINK_PART_ACCESS_KEY_ID, "S3_SECRET_ACCESS_KEY": resourcev1.LinkPart_LINK_PART_SECRET_KEY,
				"S3_PREFIX": resourcev1.LinkPart_LINK_PART_KEY_PREFIX,
			},
		}))
		if err != nil {
			t.Fatal(err)
		}
		apply(t, c, planned.Msg.GetChange())
		srv.Set("/uploads/notes/a.png", []byte("a"))
		methods := len(srv.Methods)
		clock = clock.Add(6 * time.Hour)
		got := look()
		if !got.GetPassed() || got.GetStep() != probe.StepList || got.GetServer() != "hz-1" {
			t.Errorf("linked, it checks as %v, want a passed read from the app's network on hz-1", got)
		}
		if read := srv.Methods[methods:]; len(read) == 0 || slices.ContainsFunc(read, func(m string) bool { return m != "GET" }) {
			t.Errorf("the read sent %v, want GETs alone", read)
		}
		if objects := srv.Objects(); len(objects) != 1 {
			t.Errorf("the read changed the bucket to %v", objects)
		}
	})
}
