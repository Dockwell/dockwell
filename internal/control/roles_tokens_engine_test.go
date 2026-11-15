// SPDX-License-Identifier: AGPL-3.0-only

package control

import (
	"errors"
	"strings"
	"testing"
	"time"

	"connectrpc.com/connect"

	resourcev1 "github.com/dockwell/dockwell/gen/dockwell/resource/v1alpha1"
	dockwellv1 "github.com/dockwell/dockwell/gen/dockwell/v1"
	"github.com/dockwell/dockwell/gen/dockwell/v1/dockwellv1connect"
	"github.com/dockwell/dockwell/internal/backup/s3/s3test"
	"github.com/dockwell/dockwell/internal/resource"
	"github.com/dockwell/dockwell/internal/store"
	"github.com/dockwell/dockwell/internal/store/storetest"
)

// stepUpAsked is the step-up a refusal asks for, if it does.
func stepUpAsked(err error) *dockwellv1.StepUpNeeded {
	var asked *dockwellv1.StepUpNeeded
	if cerr := new(connect.Error); errors.As(err, &cerr) {
		for _, d := range cerr.Details() {
			if v, derr := d.Value(); derr == nil {
				if s, ok := v.(*dockwellv1.StepUpNeeded); ok {
					asked = s
				}
			}
		}
	}
	return asked
}

// waitApplied waits for a change to apply.
func waitApplied(t *testing.T, c dockwellv1connect.ChangeServiceClient, id string) {
	t.Helper()
	eventually(t, "the change to apply", func() bool {
		res, err := c.GetChange(t.Context(), connect.NewRequest(&dockwellv1.GetChangeRequest{Id: id}))
		if err != nil {
			t.Fatal(err)
		}
		switch res.Msg.GetChange().GetState() {
		case dockwellv1.ChangeState_CHANGE_STATE_APPLIED:
			return true
		case dockwellv1.ChangeState_CHANGE_STATE_FAILED, dockwellv1.ChangeState_CHANGE_STATE_DISCARDED:
			t.Fatalf("the change ended %v", res.Msg.GetChange().GetState())
		}
		return false
	})
}

// A token narrowed to a project holds an admin's say in it, its protected
// environments included, and nothing of the whole install's: what its change
// reaches past its project, such as keeping a server path while it runs another
// repository, or opening an environment, waits for a person, saying the token's
// projects are why, where an admin's token for every project applies it. A new
// tag that keeps the path is a token's to apply (ADR 0071).
func TestANarrowedTokenWaitsForAPersonPastItsProject(t *testing.T) {
	t.Parallel()
	storetest.Each(t, func(t *testing.T, backend store.Backend) {
		f := startFull(t, backend, nil)
		b := signedIn(t, f)
		registerImages(f.addDockerServer(t))
		c := changeClient(b, b.origin)
		config := &resourcev1.Mount{Source: &resourcev1.Mount_HostPath{HostPath: "/srv/app/config"}, Target: "/etc/app", ReadOnly: true}
		web := func(image string) *dockwellv1.Edit {
			return &dockwellv1.Edit{
				Resource: &dockwellv1.ResourceRef{Name: "web"},
				Spec: &resourcev1.Spec{Kind: &resourcev1.Spec_Service{Service: &resourcev1.Service{
					Runs: &resourcev1.Service_App{App: &resourcev1.Container{Image: image, Mounts: []*resourcev1.Mount{config}}},
				}}},
			}
		}
		applyBy := func(c dockwellv1connect.ChangeServiceClient, ch *dockwellv1.Change) error {
			_, err := c.ApplyChange(t.Context(), connect.NewRequest(&dockwellv1.ApplyChangeRequest{Id: ch.GetId(), Fingerprint: ch.GetPlan().GetFingerprint()}))
			return err
		}
		mounted := propose(t, c, "Mount the config", web("ghcr.io/acme/web:1.0"))
		if err := applyBy(c, mounted); err != nil {
			t.Fatal(err)
		}
		waitApplied(t, c, mounted.GetId())

		narrowed, _ := narrowedToken(t, b, &dockwellv1.CreateTokenRequest{Projects: []string{"default"}})
		script := changeClient(bearer{token: narrowed, rt: b.client.Transport}, b.origin)
		wide := changeClient(bearer{token: createToken(t, b, dockwellv1.TokenScope_TOKEN_SCOPE_READ_WRITE), rt: b.client.Transport}, b.origin)

		// A new tag keeps the path as it is: the narrowed token applies it, in its project's production.
		tag := propose(t, script, "Deploy 1.1", web("ghcr.io/acme/web:1.1"))
		if err := applyBy(script, tag); err != nil {
			t.Fatalf("the narrowed token deploying a new tag that keeps the path: %v", err)
		}
		waitApplied(t, c, tag.GetId())

		// Another repository with the path kept is an admin's of the whole install.
		err := applyBy(script, propose(t, script, "Run another web", web("ghcr.io/acme/ops:1.0")))
		if connect.CodeOf(err) != connect.CodePermissionDenied || reasonOf(err) != "needs_role" ||
			!strings.Contains(err.Error(), "this API token acts in its projects alone, never as an admin of the whole install") ||
			!strings.Contains(err.Error(), "Use a token made for every project you hold") {
			t.Errorf("the narrowed token applying another repository that keeps a server path: %v, want it to wait, saying its projects are why", err)
		}
		if err := applyBy(wide, propose(t, wide, "Run another web", web("ghcr.io/acme/ops:1.0"))); err != nil {
			t.Errorf("an admin's token for every project applying the same: %v", err)
		}

		// An environment's protection is an admin's of the whole install too.
		_, err = script.ProposeChange(t.Context(), connect.NewRequest(&dockwellv1.ProposeChangeRequest{
			Summary: "Open production", Edits: []*dockwellv1.Edit{{
				Resource: &dockwellv1.ResourceRef{Kind: "environment", Name: "production"},
				Spec: &resourcev1.Spec{Kind: &resourcev1.Spec_Environment{Environment: &resourcev1.Environment{
					Protection: resourcev1.Protection_PROTECTION_OPEN,
				}}},
			}},
		}))
		if connect.CodeOf(err) != connect.CodePermissionDenied || !strings.Contains(err.Error(), "this API token acts in its projects alone") {
			t.Errorf("the narrowed token planning production open: %v, want it refused, saying its projects are why", err)
		}
	})
}

// A change a capped or narrowed token plans is someone else's to everyone, its
// maker included: in a protected environment, the maker applying it in the panel
// approves it, with the plan's fingerprint and a passkey for it. A change their
// token for every project planned stays their own, as on main (ADR 0071).
func TestApplyingANarrowedTokensChangeApprovesIt(t *testing.T) {
	t.Parallel()
	storetest.Each(t, func(t *testing.T, backend store.Backend) {
		f := startFull(t, backend, nil)
		b := signedIn(t, f)
		f.addDockerServer(t)
		c := changeClient(b, b.origin)
		apply(t, c, propose(t, c, "Run web", runImage("ghcr.io/acme/shop:1.4")))

		capped, _ := narrowedToken(t, b, &dockwellv1.CreateTokenRequest{Role: dockwellv1.TokenRole_TOKEN_ROLE_DEVELOPER})
		theirs := propose(t, changeClient(bearer{token: capped, rt: b.client.Transport}, b.origin), "Deploy web", runImage("ghcr.io/acme/shop:1.5"))
		read, err := c.GetChange(t.Context(), connect.NewRequest(&dockwellv1.GetChangeRequest{Id: theirs.GetId()}))
		if err != nil || !read.Msg.GetChange().GetProtected() || !read.Msg.GetChange().GetApproves() {
			t.Errorf("the capped token's plan in production reads protected %v, approves %v to its maker, %v; want approving it",
				read.Msg.GetChange().GetProtected(), read.Msg.GetChange().GetApproves(), err)
		}
		fingerprint := theirs.GetPlan().GetFingerprint()
		_, err = c.ApplyChange(t.Context(), connect.NewRequest(&dockwellv1.ApplyChangeRequest{Id: theirs.GetId(), Fingerprint: fingerprint}))
		if asked := stepUpAsked(err); asked.GetAction() != dockwellv1.StepUpAction_STEP_UP_ACTION_APPROVE_CHANGE || asked.GetTarget() != theirs.GetId()+":"+fingerprint {
			t.Errorf("its maker applying it with no passkey: %v, asking %v; want approving %s:%s", err, asked, theirs.GetId(), fingerprint)
		}
		proof := stepUpFor(t, b, dockwellv1.StepUpAction_STEP_UP_ACTION_APPROVE_CHANGE, theirs.GetId()+":"+fingerprint)
		if _, err := c.ApplyChange(t.Context(), connect.NewRequest(&dockwellv1.ApplyChangeRequest{Id: theirs.GetId(), Fingerprint: fingerprint, Proof: proof})); err != nil {
			t.Errorf("its maker approving it with the fingerprint and a passkey: %v", err)
		}

		wide := createToken(t, b, dockwellv1.TokenScope_TOKEN_SCOPE_READ_WRITE)
		mine := propose(t, changeClient(bearer{token: wide, rt: b.client.Transport}, b.origin), "Deploy web", runImage("ghcr.io/acme/shop:1.4"))
		if mine.GetApproves() {
			t.Error("a plan made with the maker's token for every project asks them to approve it")
		}
	})
}

// A token narrowed to a project doesn't run a task, which runs a shell as root
// on its servers, which every project there shares: it's refused with
// token_places, saying so, where a token for every project gets past that
// (ADR 0071).
func TestANarrowedTokenRunsNoTask(t *testing.T) {
	t.Parallel()
	f := startFull(t, store.SQLite, nil)
	b := signedIn(t, f)
	narrowed, _ := narrowedToken(t, b, &dockwellv1.CreateTokenRequest{Projects: []string{"default"}})
	run := func(secret string) error {
		_, err := dockwellv1connect.NewTaskServiceClient(bearer{token: secret, rt: b.client.Transport}, b.origin, connect.WithProtoJSON()).
			RunTask(t.Context(), connect.NewRequest(&dockwellv1.RunTaskRequest{Id: "tsk_nightly"}))
		return err
	}
	err := run(narrowed)
	if connect.CodeOf(err) != connect.CodePermissionDenied || reasonOf(err) != "token_places" ||
		!strings.Contains(err.Error(), "running a task runs a shell as root on its servers") {
		t.Errorf("the narrowed token running a task: %v, want token_places, saying a task reaches past its project", err)
	}
	if err := run(createToken(t, b, dockwellv1.TokenScope_TOKEN_SCOPE_READ_WRITE)); reasonOf(err) == "token_places" {
		t.Errorf("a token for every project running a task: %v, want it past the token's places", err)
	}
}

// What's in a project out of a narrowed token's reach answers as what isn't there
// does: the same code, reason and words, for a change and for a service
// (ADR 0049, ADR 0071).
func TestOutOfReachAnswersAsMissingForANarrowedToken(t *testing.T) {
	t.Parallel()
	storetest.Each(t, func(t *testing.T, backend store.Backend) {
		f := startFull(t, backend, nil)
		b := signedIn(t, f)
		f.addDockerServer(t)
		c := changeClient(b, b.origin)
		web := apply(t, c, propose(t, c, "Run web", runImage("ghcr.io/acme/shop:1.4")))
		webID := web.GetPlan().GetResources()[0].GetId()
		apply(t, c, propose(t, c, "Make blog", &dockwellv1.Edit{
			Resource: &dockwellv1.ResourceRef{Kind: "project", Name: "blog"},
			Spec:     &resourcev1.Spec{Kind: &resourcev1.Spec_Project{Project: &resourcev1.Project{}}},
		}))
		secret, _ := narrowedToken(t, b, &dockwellv1.CreateTokenRequest{Projects: []string{"blog"}})
		script := bearer{token: secret, rt: b.client.Transport}
		same := func(what string, outside, missing error, outsideID, missingID string) {
			t.Helper()
			if connect.CodeOf(outside) != connect.CodeNotFound || connect.CodeOf(outside) != connect.CodeOf(missing) || reasonOf(outside) != reasonOf(missing) ||
				strings.ReplaceAll(outside.Error(), outsideID, "X") != strings.ReplaceAll(missing.Error(), missingID, "X") {
				t.Errorf("%s out of reach: %v\nwhere one that isn't there: %v", what, outside, missing)
			}
		}
		const noChange, noService = "chg_01NOTHERE0000000000000000", "svc_01NOTHERE0000000000000000"
		changes := changeClient(script, b.origin)
		_, outside := changes.GetChange(t.Context(), connect.NewRequest(&dockwellv1.GetChangeRequest{Id: web.GetId()}))
		_, missing := changes.GetChange(t.Context(), connect.NewRequest(&dockwellv1.GetChangeRequest{Id: noChange}))
		same("a change", outside, missing, web.GetId(), noChange)
		services := dockwellv1connect.NewServiceServiceClient(script, b.origin, connect.WithProtoJSON())
		_, outside = services.ListServiceRevisions(t.Context(), connect.NewRequest(&dockwellv1.ListServiceRevisionsRequest{Id: webID}))
		_, missing = services.ListServiceRevisions(t.Context(), connect.NewRequest(&dockwellv1.ListServiceRevisionsRequest{Id: noService}))
		same("a service", outside, missing, webID, noService)
	})
}

// What counts or adds up across projects counts a narrowed token's projects
// alone, as lists are narrowed after the call and counts escape that: Activity's
// counts agree with the events it lists, a backup destination says nothing of
// another project's backups, and a server reads by name and state (ADR 0049,
// ADR 0071).
func TestANarrowedTokenCountsItsProjectsAlone(t *testing.T) {
	t.Parallel()
	storetest.Each(t, func(t *testing.T, backend store.Backend) {
		f := startFull(t, backend, nil)
		b := signedIn(t, f)
		f.addDockerServer(t)
		c := changeClient(b, b.origin)
		web := apply(t, c, propose(t, c, "Run web", runImage("ghcr.io/acme/shop:1.4")))
		apply(t, c, propose(t, c, "Make blog", &dockwellv1.Edit{
			Resource: &dockwellv1.ResourceRef{Kind: "project", Name: "blog"},
			Spec:     &resourcev1.Spec{Kind: &resourcev1.Spec_Project{Project: &resourcev1.Project{}}},
		}))
		apply(t, c, propose(t, c, "Add r2", destinationEdit(s3test.New(t), "s3cr3t")))
		servers, err := dockwellv1connect.NewServerServiceClient(b, b.origin, connect.WithProtoJSON()).
			ListServers(t.Context(), connect.NewRequest(&dockwellv1.ListServersRequest{}))
		if err != nil || len(servers.Msg.GetServers()) == 0 {
			t.Fatalf("listing servers: %v, %v", servers, err)
		}
		serverID := servers.Msg.GetServers()[0].GetId()
		// A backup of the default project's web, copied to r2.
		envID := web.GetPlan().GetResources()[0].GetResource().GetScope()
		if err := f.store.Update(t.Context(), func(tx *store.Tx) error {
			now := time.Now()
			backup := store.Backup{
				ID: "bkp_web", EnvironmentID: envID, Volume: "data", ServerID: serverID, State: store.BackupDone, Method: resource.BackupCopy,
				StartedAt: now, FinishedAt: &now, Destination: "r2", CopyKey: "acme/bkp_web", CopyBytes: 2048,
			}
			if err := tx.CreateBackup(t.Context(), backup); err != nil {
				return err
			}
			return tx.SetBackupCopy(t.Context(), backup)
		}); err != nil {
			t.Fatal(err)
		}

		secret, _ := narrowedToken(t, b, &dockwellv1.CreateTokenRequest{Projects: []string{"blog"}})
		script := bearer{token: secret, rt: b.client.Transport}

		// Activity: what it counts is what it lists, and fewer than the owner's.
		counted, err := eventClient(script, b.origin).CountEvents(t.Context(), connect.NewRequest(&dockwellv1.CountEventsRequest{}))
		if err != nil {
			t.Fatal(err)
		}
		listed := listEvents(t, eventClient(script, b.origin), &dockwellv1.ListEventsRequest{})
		all, err := eventClient(b, b.origin).CountEvents(t.Context(), connect.NewRequest(&dockwellv1.CountEventsRequest{}))
		if err != nil {
			t.Fatal(err)
		}
		if int(counted.Msg.GetTotal()) != len(listed) || counted.Msg.GetTotal() >= all.Msg.GetTotal() {
			t.Errorf("the narrowed token counts %d events and lists %d, where the owner counts %d; want what it lists, fewer than the owner's",
				counted.Msg.GetTotal(), len(listed), all.Msg.GetTotal())
		}

		// A destination: the owner reads the backup at it, the token nothing of it.
		destination := func(c connect.HTTPClient) *dockwellv1.BackupDestinationInfo {
			t.Helper()
			res, err := dockwellv1connect.NewBackupServiceClient(c, b.origin, connect.WithProtoJSON()).
				ListBackupDestinations(t.Context(), connect.NewRequest(&dockwellv1.ListBackupDestinationsRequest{}))
			if err != nil || len(res.Msg.GetDestinations()) != 1 {
				t.Fatalf("listing destinations: %v, %v", res, err)
			}
			return res.Msg.GetDestinations()[0]
		}
		if d := destination(b); d.GetBackups() != 1 || d.GetUsedBytes() != 2048 {
			t.Errorf("the owner reads r2 holding %d backups in %d bytes, want the one", d.GetBackups(), d.GetUsedBytes())
		}
		if d := destination(script); d.GetName() != "r2" || d.GetBackups() != 0 || d.GetUsedBytes() != 0 {
			t.Errorf("the narrowed token reads r2 as %v, want its name, and nothing of another project's backups", d)
		}

		// A server, by ID: its name and state alone.
		server, err := dockwellv1connect.NewServerServiceClient(script, b.origin, connect.WithProtoJSON()).
			GetServer(t.Context(), connect.NewRequest(&dockwellv1.GetServerRequest{Id: serverID}))
		if err != nil || server.Msg.GetServer().GetName() == "" || server.Msg.GetServer().GetStats() != nil || server.Msg.GetServer().GetInventory() != nil {
			t.Errorf("the narrowed token reads the server as %v, %v; want its name and state alone", server, err)
		}
	})
}
