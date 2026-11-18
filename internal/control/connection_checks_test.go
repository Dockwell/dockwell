// SPDX-License-Identifier: AGPL-3.0-only

package control

import (
	"testing"
	"time"

	"connectrpc.com/connect"

	resourcev1 "github.com/dockwell/dockwell/gen/dockwell/resource/v1alpha1"
	dockwellv1 "github.com/dockwell/dockwell/gen/dockwell/v1"
	"github.com/dockwell/dockwell/gen/dockwell/v1/dockwellv1connect"
	"github.com/dockwell/dockwell/internal/backup/s3/s3test"
	"github.com/dockwell/dockwell/internal/probe"
	"github.com/dockwell/dockwell/internal/store"
	"github.com/dockwell/dockwell/internal/store/storetest"
)

// Each connection is checked from the control server: read at the first look,
// with Check's full check once it changes, and read again every 6 hours, never
// between; a person's Check of it as saved
// is kept too. Its list says its last check: an admin's with where it ran and what
// it found, and below an admin whether it passed and when, alone (#799).
func TestAConnectionIsCheckedOnItsScheduleAndListedWithItsLastCheck(t *testing.T) {
	t.Parallel()
	storetest.Each(t, func(t *testing.T, backend store.Backend) {
		f := startFull(t, backend, nil)
		b := signedIn(t, f)
		f.addDockerServer(t)
		f.config.Fleet.SetControlServer(f.lastDocker.id.ServerID)
		env := listedEnv(t, b)
		project := envProject(t, f, env)
		srv := s3test.New(t)
		c := changeClient(b, b.origin)
		cc := dockwellv1connect.NewConnectionServiceClient(b, b.origin, connect.WithProtoJSON())
		spec := func(name string) *resourcev1.ConnectionSpec {
			return &resourcev1.ConnectionSpec{
				DisplayName: name, Kind: resourcev1.LinkKind_LINK_KIND_S3,
				Reach: resourcev1.ConnectionReach_CONNECTION_REACH_PROJECTS, Projects: []string{project},
				S3: &resourcev1.ConnectionS3{Endpoint: srv.URL(), Bucket: "uploads", AccessKeyId: "AKIDEXAMPLE", SecretKey: connectionSecret, PathStyle: true},
			}
		}
		apply(t, c, propose(t, c, "Add the uploads bucket", connectionEdit("uploads", spec("Uploads"))))

		checker := f.server.connectionChecks
		clock := time.Unix(1_800_000_000, 0)
		checker.Now = func() time.Time { return clock }
		checks := func() []*dockwellv1.CheckConnectionResponse {
			t.Helper()
			res, err := cc.ListConnections(t.Context(), connect.NewRequest(&dockwellv1.ListConnectionsRequest{}))
			if err != nil {
				t.Fatal(err)
			}
			if len(res.Msg.GetConnections()) != 1 {
				t.Fatalf("listed %v", res.Msg.GetConnections())
			}
			return res.Msg.GetConnections()[0].GetChecks()
		}
		look := func() {
			checker.Check(t.Context())
			checker.Wait()
		}
		if got := checks(); len(got) != 0 {
			t.Errorf("before any check, it lists %v", got)
		}

		// Saved, it's checked at the next look, from the control server: the stand-in
		// is on loopback, which a check from a network of its own is refused.
		look()
		first := checks()
		// The first look after a start reads it, as nothing says what changed while
		// the control plane was stopped: a list, which writes nothing (ADR 0099).
		if len(first) != 1 || first[0].GetServer() != "hz-1" || first[0].GetStep() != probe.StepList || !first[0].GetTime().AsTime().Equal(clock) {
			t.Fatalf("after the first look, it lists %v", first)
		}

		// Not again within 6 hours.
		clock = clock.Add(5 * time.Hour)
		look()
		if got := checks(); len(got) != 1 || !got[0].GetTime().AsTime().Equal(first[0].GetTime().AsTime()) {
			t.Errorf("5 hours on, it lists %v, want the first check still", got)
		}

		// Changed, at once; and its older revision's check isn't shown as the new one's.
		apply(t, c, propose(t, c, "Rename uploads", connectionEdit("uploads", spec("Uploads, renamed"))))
		if got := checks(); len(got) != 0 {
			t.Errorf("changed, it lists the older revision's checks %v", got)
		}
		clock = clock.Add(time.Minute)
		look()
		changed := checks()
		// Changed, it gets Check's full check, which starts by writing.
		if len(changed) != 1 || !changed[0].GetTime().AsTime().Equal(clock) || changed[0].GetStep() != probe.StepWrite {
			t.Fatalf("changed, it lists %v, want the full check at %v", changed, clock)
		}

		// 6 hours after, again.
		clock = clock.Add(6 * time.Hour)
		look()
		if got := checks(); len(got) != 1 || !got[0].GetTime().AsTime().Equal(clock) || got[0].GetStep() != probe.StepList {
			t.Errorf("6 hours on, it lists %v, want a read at %v", got, clock)
		}

		// A person's Check of it as saved is kept; one with its keys typed isn't.
		clock = clock.Add(time.Minute)
		if _, err := cc.CheckConnection(t.Context(), connect.NewRequest(&dockwellv1.CheckConnectionRequest{Name: "uploads"})); err != nil {
			t.Fatal(err)
		}
		pressed := clock
		if got := checks(); len(got) != 1 || !got[0].GetTime().AsTime().Equal(pressed) {
			t.Errorf("after Check, it lists %v, want a check at %v", got, pressed)
		}
		clock = clock.Add(time.Minute)
		if _, err := cc.CheckConnection(t.Context(), connect.NewRequest(&dockwellv1.CheckConnectionRequest{
			Name: "uploads", S3: &resourcev1.ConnectionS3{Endpoint: srv.URL(), Bucket: "uploads", AccessKeyId: "AKIDEXAMPLE", SecretKey: "typed", PathStyle: true},
		})); err != nil {
			t.Fatal(err)
		}
		if got := checks(); len(got) != 1 || !got[0].GetTime().AsTime().Equal(pressed) {
			t.Errorf("a check of what's typed is kept: %v", got)
		}

		// Below an admin, whether it passed and when, alone.
		aMember(t, f)(store.Grant{ProjectID: project, Role: store.RoleViewer})
		trimmed := checks()
		if len(trimmed) != 1 || trimmed[0].GetTime() == nil || !trimmed[0].GetTime().AsTime().Equal(pressed) {
			t.Fatalf("a viewer lists %v", trimmed)
		}
		k := trimmed[0]
		if k.GetServer() != "" || k.GetStep() != "" || k.GetCode() != "" || k.GetStatus() != 0 || k.GetMessage() != "" || k.GetWords() != "" || k.GetRegion() != "" {
			t.Errorf("a viewer reads %v, want passed and time alone", k)
		}
	})
}
