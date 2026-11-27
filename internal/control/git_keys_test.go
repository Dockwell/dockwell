// SPDX-License-Identifier: AGPL-3.0-only

package control

import (
	"bytes"
	"fmt"
	"os"
	"slices"
	"strings"
	"testing"

	"connectrpc.com/connect"

	resourcev1 "github.com/dockwell/dockwell/gen/dockwell/resource/v1alpha1"
	dockwellv1 "github.com/dockwell/dockwell/gen/dockwell/v1"
	"github.com/dockwell/dockwell/gen/dockwell/v1/dockwellv1connect"
	"github.com/dockwell/dockwell/internal/control/authapi"
	"github.com/dockwell/dockwell/internal/docker"
	"github.com/dockwell/dockwell/internal/gitssh/gitsshtest"
	"github.com/dockwell/dockwell/internal/store"
	"github.com/dockwell/dockwell/internal/store/storetest"
)

func gitKeysClient(b *browser) dockwellv1connect.GitDeployKeyServiceClient {
	return dockwellv1connect.NewGitDeployKeyServiceClient(b, b.origin, connect.WithProtoJSON())
}

// TestSSHDeployKeyFullLifecycle exercises creating a project-scoped Ed25519
// deploy key, host-key pinning, auth failure before adding the public key,
// listing branches, detecting and building an app over SSH (streaming a
// verified pack from control plane to agent), rotating the key safely,
// enforcing admin + step-up on host-key change, project isolation, audit logs,
// deletion protection, and zero private-key leakage in the database dump.
func TestSSHDeployKeyFullLifecycle(t *testing.T) {
	t.Parallel()
	storetest.Each(t, func(t *testing.T, backend store.Backend) {
		g := newGitBench(t, backend)
		ctx := t.Context()
		_ = g.repo.Commit("main", "Initial private SSH commit", map[string]string{
			"Dockerfile": webDockerfile,
			"app.js":     "v1",
		})
		g.repo.Commit("release", "Release branch", map[string]string{
			"Dockerfile": webDockerfile,
			"app.js":     "release-v1",
		})
		// Ensure HEAD points at main.
		commit1 := g.repo.Commit("main", "Second commit on main", map[string]string{
			"app.js": "v2",
		})

		srv, err := gitsshtest.NewServer("127.0.0.1:0", "acme/web.git", g.repo.BareDir())
		if err != nil {
			t.Fatalf("start ssh git server: %v", err)
		}
		t.Cleanup(func() { _ = srv.Close() })

		sshURL := fmt.Sprintf("ssh://git@%s/acme/web.git", srv.Addr)
		keys := gitKeysClient(g.b)
		defaultProjectID := envProject(t, g.f, listedEnv(t, g.b))

		// 1. Creating a deploy key requires step-up proof.
		if _, err := keys.CreateGitDeployKey(ctx, connect.NewRequest(&dockwellv1.CreateGitDeployKeyRequest{
			Project:    defaultProjectID,
			Repository: sshURL,
		})); reasonOf(err) != authapi.ReasonNeedsStepUp {
			t.Fatalf("CreateGitDeployKey without step-up: %v, want %s", err, authapi.ReasonNeedsStepUp)
		}

		createdRes, err := keys.CreateGitDeployKey(ctx, connect.NewRequest(&dockwellv1.CreateGitDeployKeyRequest{
			Project:    defaultProjectID,
			Repository: sshURL,
			Proof:      stepUpFor(t, g.b, dockwellv1.StepUpAction_STEP_UP_ACTION_CREATE_GIT_DEPLOY_KEY, defaultProjectID),
		}))
		if err != nil {
			t.Fatalf("CreateGitDeployKey: %v", err)
		}
		key := createdRes.Msg.GetKey()
		if !strings.HasPrefix(key.GetId(), "gdk_") || !strings.HasPrefix(key.GetPublicKey(), "ssh-ed25519 ") || !strings.HasPrefix(key.GetKeyFingerprint(), "SHA256:") {
			t.Fatalf("created key looks wrong: %+v", key)
		}
		if key.GetHostKeyPinned() {
			t.Fatalf("new key should not have a pinned host key yet")
		}
		if key.GetGitLogin() != "ssh:"+key.GetName() {
			t.Fatalf("git_login = %q, want ssh:%s", key.GetGitLogin(), key.GetName())
		}

		// 2. First test sees unverified host key and returns its algorithm + fingerprint.
		test1, err := keys.TestGitDeployKey(ctx, connect.NewRequest(&dockwellv1.TestGitDeployKeyRequest{Id: key.GetId()}))
		if err != nil {
			t.Fatalf("TestGitDeployKey (unpinned): %v", err)
		}
		wantAlgo, wantFP := srv.HostKey()
		if test1.Msg.GetStatus() != dockwellv1.GitDeployKeyTestStatus_GIT_DEPLOY_KEY_TEST_STATUS_HOST_KEY_UNVERIFIED {
			t.Fatalf("status = %v, want HOST_KEY_UNVERIFIED", test1.Msg.GetStatus())
		}
		if test1.Msg.GetObservedHostKeyAlgorithm() != wantAlgo || test1.Msg.GetObservedHostKeyFingerprint() != wantFP {
			t.Fatalf("observed host key = %s %s, want %s %s", test1.Msg.GetObservedHostKeyAlgorithm(), test1.Msg.GetObservedHostKeyFingerprint(), wantAlgo, wantFP)
		}

		// 3. Accept host key before adding the public key: pins host key, fails with AUTH_FAILED.
		test2, err := keys.TestGitDeployKey(ctx, connect.NewRequest(&dockwellv1.TestGitDeployKeyRequest{
			Id:                 key.GetId(),
			AcceptHostKey:      true,
			HostKeyAlgorithm:   wantAlgo,
			HostKeyFingerprint: wantFP,
		}))
		if err != nil {
			t.Fatalf("TestGitDeployKey (accept host key, no auth): %v", err)
		}
		if test2.Msg.GetStatus() != dockwellv1.GitDeployKeyTestStatus_GIT_DEPLOY_KEY_TEST_STATUS_AUTH_FAILED {
			t.Fatalf("status = %v, want AUTH_FAILED", test2.Msg.GetStatus())
		}
		if !test2.Msg.GetKey().GetHostKeyPinned() || test2.Msg.GetKey().GetHostKeyFingerprint() != wantFP {
			t.Fatalf("host key was not pinned after acceptance: %+v", test2.Msg.GetKey())
		}

		// 4. Add the public key to the SSH server and test again: succeeds with branches.
		if err := srv.SetAuthorizedKeys(key.GetPublicKey()); err != nil {
			t.Fatalf("SetAuthorizedKeys: %v", err)
		}
		test3, err := keys.TestGitDeployKey(ctx, connect.NewRequest(&dockwellv1.TestGitDeployKeyRequest{Id: key.GetId()}))
		if err != nil {
			t.Fatalf("TestGitDeployKey (ok): %v", err)
		}
		if test3.Msg.GetStatus() != dockwellv1.GitDeployKeyTestStatus_GIT_DEPLOY_KEY_TEST_STATUS_OK {
			t.Fatalf("status = %v (%s), want OK", test3.Msg.GetStatus(), test3.Msg.GetMessage())
		}
		if test3.Msg.GetDefaultBranch() != "main" || !slices.Contains(test3.Msg.GetBranches(), "main") || !slices.Contains(test3.Msg.GetBranches(), "release") {
			t.Fatalf("branches = %v (default %q), want main and release", test3.Msg.GetBranches(), test3.Msg.GetDefaultBranch())
		}

		// 5. ListRepositoryBranches and DetectBuild work over SSH with the deploy key.
		brRes, err := g.builds.ListRepositoryBranches(ctx, connect.NewRequest(&dockwellv1.ListRepositoryBranchesRequest{
			Url:      sshURL,
			Project:  defaultProjectID,
			GitLogin: key.GetGitLogin(),
		}))
		if err != nil {
			t.Fatalf("ListRepositoryBranches over SSH: %v", err)
		}
		if brRes.Msg.GetGitLogin() != key.GetGitLogin() || !slices.Contains(brRes.Msg.GetBranches(), "main") {
			t.Fatalf("ListRepositoryBranches = %+v", brRes.Msg)
		}
		detRes, err := g.builds.DetectBuild(ctx, connect.NewRequest(&dockwellv1.DetectBuildRequest{
			Project: defaultProjectID,
			Build: &resourcev1.ImageBuild{
				Source:   &resourcev1.ImageBuild_Git{Git: &resourcev1.GitSource{Url: sshURL, Ref: "main"}},
				GitLogin: key.GetGitLogin(),
			},
		}))
		if err != nil {
			t.Fatalf("DetectBuild over SSH: %v", err)
		}
		if detRes.Msg.GetDetection().GetBuilder() != resourcev1.ImageBuild_BUILDER_DOCKERFILE {
			t.Fatalf("DetectBuild builder = %v, want DOCKERFILE", detRes.Msg.GetDetection())
		}

		// 6. Plan and build a real service from the private SSH repository.
		sshEdit := &dockwellv1.Edit{
			Resource: &dockwellv1.ResourceRef{Kind: "service", Name: "sshweb"},
			Spec: &resourcev1.Spec{Kind: &resourcev1.Spec_Service{Service: &resourcev1.Service{
				Runs: &resourcev1.Service_App{App: &resourcev1.Container{Ports: []*resourcev1.PortMapping{{HostPort: 8085, ContainerPort: 8080}}}},
				Build: &resourcev1.ImageBuild{
					Source:   &resourcev1.ImageBuild_Git{Git: &resourcev1.GitSource{Url: sshURL, Ref: "main"}},
					GitLogin: key.GetGitLogin(),
					Args:     map[string]string{"NODE_ENV": "production"},
				},
			}}},
		}
		plan := g.propose(t, sshEdit)
		step := buildStep(plan)
		if step == nil || step.GetCommit() != commit1 || step.GetGitLogin() != key.GetGitLogin() {
			t.Fatalf("SSH build plan step = %+v, want commit %s login %s", step, commit1, key.GetGitLogin())
		}
		applied := g.done(t, plan)
		if applied.GetState() != dockwellv1.ChangeState_CHANGE_STATE_APPLIED {
			t.Fatalf("SSH build change ended %v", applied.GetState())
		}
		web := g.service(t, "", "sshweb")
		running := g.e.Newest(docker.Project(web.GetResource().GetScope()), "sshweb")
		if running == nil || !running.State.Running {
			t.Fatalf("SSH-built container not running: %+v", running)
		}
		revs := g.revisions(t, web.GetId())
		if b := revs[0].GetBuild(); b.GetState() != dockwellv1.BuildState_BUILD_STATE_DONE || b.GetCommit() != commit1 || b.GetCommitSubject() != "Second commit on main" {
			t.Fatalf("SSH build record = %+v", b)
		}

		// 7. Cross-project isolation: another project cannot use default project's deploy key.
		otherProj := resourceIn(t, apply(t, g.c, propose(t, g.c, "Make other project", &dockwellv1.Edit{
			Resource: &dockwellv1.ResourceRef{Kind: "project", Name: "other"},
			Spec:     &resourcev1.Spec{Kind: &resourcev1.Spec_Project{Project: &resourcev1.Project{}}},
		})), "project")
		otherEnv := resourceIn(t, apply(t, g.c, propose(t, g.c, "Add prod to other", &dockwellv1.Edit{
			Resource: &dockwellv1.ResourceRef{Kind: "environment", Scope: otherProj, Name: "prod"},
			Spec:     &resourcev1.Spec{Kind: &resourcev1.Spec_Environment{Environment: &resourcev1.Environment{}}},
		})), "environment")
		otherKeys, err := keys.ListGitDeployKeys(ctx, connect.NewRequest(&dockwellv1.ListGitDeployKeysRequest{Project: otherProj}))
		if err != nil || len(otherKeys.Msg.GetKeys()) != 0 {
			t.Fatalf("other project listed keys = %v, %v; want empty", otherKeys, err)
		}
		crossEdit := &dockwellv1.Edit{
			Resource: &dockwellv1.ResourceRef{Kind: "service", Scope: otherEnv, Name: "stolen"},
			Spec: &resourcev1.Spec{Kind: &resourcev1.Spec_Service{Service: &resourcev1.Service{
				Runs: &resourcev1.Service_App{App: &resourcev1.Container{Ports: []*resourcev1.PortMapping{{HostPort: 8086, ContainerPort: 8080}}}},
				Build: &resourcev1.ImageBuild{
					Source:   &resourcev1.ImageBuild_Git{Git: &resourcev1.GitSource{Url: sshURL, Ref: "main"}},
					GitLogin: key.GetGitLogin(),
				},
			}}},
		}
		if _, err := g.c.ProposeChange(ctx, connect.NewRequest(&dockwellv1.ProposeChangeRequest{Summary: "Cross-project SSH", Edits: []*dockwellv1.Edit{crossEdit}})); err == nil {
			t.Fatalf("cross-project deploy key usage succeeded; want refusal")
		}

		// 8. Delete is refused while sshweb uses the key.
		if _, err := keys.DeleteGitDeployKey(ctx, connect.NewRequest(&dockwellv1.DeleteGitDeployKeyRequest{
			Id:    key.GetId(),
			Proof: stepUpFor(t, g.b, dockwellv1.StepUpAction_STEP_UP_ACTION_DELETE_GIT_DEPLOY_KEY, key.GetId()),
		})); connect.CodeOf(err) != connect.CodeFailedPrecondition {
			t.Fatalf("DeleteGitDeployKey while in use = %v, want FailedPrecondition", err)
		}

		// 9. Rotation: BeginRotate keeps old key active until Test with use_pending succeeds.
		rotStart, err := keys.BeginRotateGitDeployKey(ctx, connect.NewRequest(&dockwellv1.BeginRotateGitDeployKeyRequest{
			Id:    key.GetId(),
			Proof: stepUpFor(t, g.b, dockwellv1.StepUpAction_STEP_UP_ACTION_ROTATE_GIT_DEPLOY_KEY, key.GetId()),
		}))
		if err != nil {
			t.Fatalf("BeginRotateGitDeployKey: %v", err)
		}
		pendingPub := rotStart.Msg.GetKey().GetPendingPublicKey()
		if !strings.HasPrefix(pendingPub, "ssh-ed25519 ") || pendingPub == key.GetPublicKey() {
			t.Fatalf("pending key invalid: %q", pendingPub)
		}
		// Testing pending key before adding it to host fails with AUTH_FAILED and leaves old key active.
		rotFail, err := keys.TestGitDeployKey(ctx, connect.NewRequest(&dockwellv1.TestGitDeployKeyRequest{
			Id:         key.GetId(),
			UsePending: true,
		}))
		if err != nil || rotFail.Msg.GetStatus() != dockwellv1.GitDeployKeyTestStatus_GIT_DEPLOY_KEY_TEST_STATUS_AUTH_FAILED || rotFail.Msg.GetPromoted() {
			t.Fatalf("pending test before host update = %+v, %v; want AUTH_FAILED & Promoted=false", rotFail, err)
		}
		// Old key still works for deploys while rotation is pending.
		commit2 := g.repo.Commit("main", "Third commit while rotating", map[string]string{"app.js": "v3"})
		dep1, err := g.s.DeployService(ctx, connect.NewRequest(&dockwellv1.DeployServiceRequest{Id: web.GetId()}))
		if err != nil {
			t.Fatalf("DeployService during pending rotation: %v", err)
		}
		g.done(t, dep1.Msg.GetChange())
		if got := g.revisions(t, web.GetId())[0].GetBuild().GetCommit(); got != commit2 {
			t.Fatalf("deploy during rotation built %s, want %s", got, commit2)
		}
		// Now authorize the new key (and keep old temporarily), test with use_pending -> promotes!
		if err := srv.SetAuthorizedKeys(key.GetPublicKey(), pendingPub); err != nil {
			t.Fatalf("SetAuthorizedKeys(old+new): %v", err)
		}
		rotOK, err := keys.TestGitDeployKey(ctx, connect.NewRequest(&dockwellv1.TestGitDeployKeyRequest{
			Id:         key.GetId(),
			UsePending: true,
		}))
		if err != nil || rotOK.Msg.GetStatus() != dockwellv1.GitDeployKeyTestStatus_GIT_DEPLOY_KEY_TEST_STATUS_OK || !rotOK.Msg.GetPromoted() {
			t.Fatalf("pending test after host update = %+v, %v; want OK & Promoted=true", rotOK, err)
		}
		if rotOK.Msg.GetOldKeyFingerprint() != key.GetKeyFingerprint() || rotOK.Msg.GetKey().GetPendingPublicKey() != "" {
			t.Fatalf("promoted response = %+v", rotOK.Msg)
		}
		// Remove old key from server: new key works, old key is gone.
		if err := srv.SetAuthorizedKeys(pendingPub); err != nil {
			t.Fatalf("SetAuthorizedKeys(new only): %v", err)
		}
		commit3 := g.repo.Commit("main", "Fourth commit after rotation", map[string]string{"app.js": "v4"})
		dep2, err := g.s.DeployService(ctx, connect.NewRequest(&dockwellv1.DeployServiceRequest{Id: web.GetId()}))
		if err != nil {
			t.Fatalf("DeployService after rotation: %v", err)
		}
		g.done(t, dep2.Msg.GetChange())
		if got := g.revisions(t, web.GetId())[0].GetBuild().GetCommit(); got != commit3 {
			t.Fatalf("deploy after rotation built %s, want %s", got, commit3)
		}

		// 10. Host key change stops fetches and requires admin + step-up to accept.
		newAlgo, newFP, err := srv.RotateHostKeyECDSA()
		if err != nil {
			t.Fatalf("RotateHostKeyECDSA: %v", err)
		}
		hkFail, err := keys.TestGitDeployKey(ctx, connect.NewRequest(&dockwellv1.TestGitDeployKeyRequest{Id: key.GetId()}))
		if err != nil || hkFail.Msg.GetStatus() != dockwellv1.GitDeployKeyTestStatus_GIT_DEPLOY_KEY_TEST_STATUS_HOST_KEY_CHANGED {
			t.Fatalf("test after host key change = %+v, %v; want HOST_KEY_CHANGED", hkFail, err)
		}
		if hkFail.Msg.GetObservedHostKeyAlgorithm() != newAlgo || hkFail.Msg.GetObservedHostKeyFingerprint() != newFP {
			t.Fatalf("observed changed host key = %s %s, want %s %s", hkFail.Msg.GetObservedHostKeyAlgorithm(), hkFail.Msg.GetObservedHostKeyFingerprint(), newAlgo, newFP)
		}
		// DeployService also refuses while host key is mismatched.
		if _, err := g.s.DeployService(ctx, connect.NewRequest(&dockwellv1.DeployServiceRequest{Id: web.GetId()})); err == nil {
			t.Fatalf("DeployService succeeded after host key change; want refusal")
		}
		// Accepting a changed host key without step-up proof is refused even for owner.
		if _, err := keys.TestGitDeployKey(ctx, connect.NewRequest(&dockwellv1.TestGitDeployKeyRequest{
			Id:                 key.GetId(),
			AcceptHostKey:      true,
			HostKeyAlgorithm:   newAlgo,
			HostKeyFingerprint: newFP,
		})); reasonOf(err) != authapi.ReasonNeedsStepUp {
			t.Fatalf("accept changed host key without step-up: %v, want %s", err, authapi.ReasonNeedsStepUp)
		}
		// A project developer cannot accept a changed host key.
		hold := aMember(t, g.f)
		hold(in(defaultProjectID, store.RoleDeveloper))
		if _, err := keys.TestGitDeployKey(ctx, connect.NewRequest(&dockwellv1.TestGitDeployKeyRequest{
			Id:                 key.GetId(),
			AcceptHostKey:      true,
			HostKeyAlgorithm:   newAlgo,
			HostKeyFingerprint: newFP,
		})); connect.CodeOf(err) != connect.CodePermissionDenied {
			t.Fatalf("developer accepting changed host key: %v, want PermissionDenied", err)
		}
		// Elevate member to admin: accepting with step-up succeeds.
		hold(in(store.Install, store.RoleAdmin))
		hkAccepted, err := keys.TestGitDeployKey(ctx, connect.NewRequest(&dockwellv1.TestGitDeployKeyRequest{
			Id:                 key.GetId(),
			AcceptHostKey:      true,
			HostKeyAlgorithm:   newAlgo,
			HostKeyFingerprint: newFP,
			Proof:              stepUpFor(t, g.b, dockwellv1.StepUpAction_STEP_UP_ACTION_ACCEPT_GIT_HOST_KEY, key.GetId()),
		}))
		if err != nil || hkAccepted.Msg.GetStatus() != dockwellv1.GitDeployKeyTestStatus_GIT_DEPLOY_KEY_TEST_STATUS_OK {
			t.Fatalf("admin accept changed host key = %+v, %v; want OK", hkAccepted, err)
		}
		if hkAccepted.Msg.GetKey().GetHostKeyChanged() || hkAccepted.Msg.GetKey().GetHostKeyFingerprint() != newFP {
			t.Fatalf("key after accepting changed host key = %+v", hkAccepted.Msg.GetKey())
		}

		// 11. Remove service, then delete deploy key succeeds.
		g.done(t, g.propose(t, &dockwellv1.Edit{
			Resource: &dockwellv1.ResourceRef{Kind: "service", Scope: web.GetResource().GetScope(), Name: "sshweb"},
			Delete:   true,
		}))
		delRes, err := keys.DeleteGitDeployKey(ctx, connect.NewRequest(&dockwellv1.DeleteGitDeployKeyRequest{
			Id:    key.GetId(),
			Proof: stepUpFor(t, g.b, dockwellv1.StepUpAction_STEP_UP_ACTION_DELETE_GIT_DEPLOY_KEY, key.GetId()),
		}))
		if err != nil {
			t.Fatalf("DeleteGitDeployKey after service removal: %v", err)
		}
		if delRes.Msg.GetOldKeyFingerprint() != rotOK.Msg.GetKey().GetKeyFingerprint() {
			t.Fatalf("deleted old_key_fingerprint = %q, want %q", delRes.Msg.GetOldKeyFingerprint(), rotOK.Msg.GetKey().GetKeyFingerprint())
		}

		// 12. Verify audit events recorded and zero private-key leakage anywhere.
		var actions []string
		err = g.f.store.View(ctx, func(tx *store.Tx) error {
			evs, err := tx.ListEvents(ctx, 1000)
			if err != nil {
				return err
			}
			for _, e := range evs {
				if strings.HasPrefix(e.Action, "git_deploy_key.") {
					actions = append(actions, e.Action)
				}
				if strings.Contains(e.Detail, "PRIVATE KEY") {
					t.Errorf("audit event leaked private key: %+v", e)
				}
			}
			return nil
		})
		if err != nil {
			t.Fatal(err)
		}
		for _, want := range []string{
			"git_deploy_key.created",
			"git_deploy_key.host_key_pinned",
			"git_deploy_key.rotate_started",
			"git_deploy_key.rotated",
			"git_deploy_key.host_key_changed_accepted",
			"git_deploy_key.deleted",
		} {
			if !slices.Contains(actions, want) {
				t.Errorf("missing audit action %q in %v", want, actions)
			}
		}

		// 13. Verify SQLite file on disk contains no unsealed private key material.
		if backend == store.SQLite {
			raw, err := os.ReadFile(g.f.db)
			if err != nil {
				t.Fatalf("read sqlite file: %v", err)
			}
			if bytes.Contains(raw, []byte("OPENSSH PRIVATE KEY")) {
				t.Fatalf("sqlite database file contains plaintext private key")
			}
		}
	})
}
