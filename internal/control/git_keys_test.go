// SPDX-License-Identifier: AGPL-3.0-only

package control

import (
	"bytes"
	"context"
	"fmt"
	"log/slog"
	"net/http/httptest"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	"connectrpc.com/connect"
	"google.golang.org/protobuf/encoding/protojson"
	"google.golang.org/protobuf/proto"

	resourcev1 "github.com/dockwell/dockwell/gen/dockwell/resource/v1alpha1"
	dockwellv1 "github.com/dockwell/dockwell/gen/dockwell/v1"
	"github.com/dockwell/dockwell/gen/dockwell/v1/dockwellv1connect"
	"github.com/dockwell/dockwell/internal/control/authapi"
	"github.com/dockwell/dockwell/internal/control/cistatus"
	"github.com/dockwell/dockwell/internal/control/gitlab"
	"github.com/dockwell/dockwell/internal/control/gitlab/gltest"
	"github.com/dockwell/dockwell/internal/control/gitproviders"
	logapi "github.com/dockwell/dockwell/internal/control/logs"
	"github.com/dockwell/dockwell/internal/docker"
	"github.com/dockwell/dockwell/internal/gitref"
	"github.com/dockwell/dockwell/internal/gitssh"
	"github.com/dockwell/dockwell/internal/gitssh/gitsshtest"
	"github.com/dockwell/dockwell/internal/gittest"
	"github.com/dockwell/dockwell/internal/id"
	"github.com/dockwell/dockwell/internal/resource"
	"github.com/dockwell/dockwell/internal/resource/gitdeploykey"
	"github.com/dockwell/dockwell/internal/resource/service"
	"github.com/dockwell/dockwell/internal/revision"
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
		g.e.SetDockerRoot(t.TempDir())
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
		// Pinning alone (before auth succeeds) must not mark the key verified (L1).
		if test2.Msg.GetKey().GetVerified() {
			t.Fatalf("key should not be verified after AUTH_FAILED pin-only test")
		}
		// Pinning a host key must not report private_key as modified (L4), and
		// ChangeService.UndoChange must refuse undoing it (H1).
		pinChange, err := g.c.GetChange(ctx, connect.NewRequest(&dockwellv1.GetChangeRequest{Id: test2.Msg.GetChangeId()}))
		if err != nil {
			t.Fatalf("GetChange(pin): %v", err)
		}
		for _, rp := range pinChange.Msg.GetChange().GetPlan().GetResources() {
			for _, fc := range rp.GetChanges() {
				if fc.GetPath() == "private_key" || fc.GetPath() == "pending_private_key" {
					t.Fatalf("host-key pin change reported %s as modified: %+v", fc.GetPath(), fc)
				}
			}
		}
		if _, err := g.c.UndoChange(ctx, connect.NewRequest(&dockwellv1.UndoChangeRequest{Id: test2.Msg.GetChangeId()})); err == nil {
			t.Fatalf("UndoChange on host-key pin succeeded; want refusal")
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
		if !test3.Msg.GetKey().GetVerified() || test3.Msg.GetKey().GetVerifyTime() == nil {
			t.Fatalf("key should be verified after OK test: %+v", test3.Msg.GetKey())
		}
		if test3.Msg.GetDefaultBranch() != "main" || !slices.Contains(test3.Msg.GetBranches(), "main") || !slices.Contains(test3.Msg.GetBranches(), "release") {
			t.Fatalf("branches = %v (default %q), want main and release", test3.Msg.GetBranches(), test3.Msg.GetDefaultBranch())
		}

		// 4b. Direct ChangeService.ProposeChange on git.deploy_key is always refused (H1),
		// for create, re-pin, rotate, and delete, for both developers and owners.
		rogueKP, err := gitssh.GenerateKey()
		if err != nil {
			t.Fatalf("GenerateKey: %v", err)
		}
		for _, tc := range []struct {
			name string
			edit *dockwellv1.Edit
		}{
			{
				name: "create",
				edit: &dockwellv1.Edit{
					Resource: &dockwellv1.ResourceRef{Kind: "git.deploy_key", Scope: defaultProjectID, Name: "rogue-key"},
					Spec: &resourcev1.Spec{Kind: &resourcev1.Spec_GitDeployKey{GitDeployKey: &resourcev1.GitDeployKey{
						Repository:     sshURL,
						KeyType:        rogueKP.KeyType,
						PublicKey:      rogueKP.PublicKey,
						KeyFingerprint: rogueKP.Fingerprint,
						PrivateKey:     rogueKP.PrivateKeyPEM,
					}}},
				},
			},
			{
				name: "re-pin",
				edit: &dockwellv1.Edit{
					Resource:     &dockwellv1.ResourceRef{Kind: "git.deploy_key", Scope: defaultProjectID, Name: key.GetName()},
					BaseRevision: test3.Msg.GetKey().GetRevision(),
					Spec: &resourcev1.Spec{Kind: &resourcev1.Spec_GitDeployKey{GitDeployKey: &resourcev1.GitDeployKey{
						Repository:         sshURL,
						HostKeyAlgorithm:   "ssh-ed25519",
						HostKeyFingerprint: rogueKP.Fingerprint,
					}}},
				},
			},
			{
				name: "rotate",
				edit: &dockwellv1.Edit{
					Resource:     &dockwellv1.ResourceRef{Kind: "git.deploy_key", Scope: defaultProjectID, Name: key.GetName()},
					BaseRevision: test3.Msg.GetKey().GetRevision(),
					Spec: &resourcev1.Spec{Kind: &resourcev1.Spec_GitDeployKey{GitDeployKey: &resourcev1.GitDeployKey{
						Repository:     sshURL,
						KeyType:        rogueKP.KeyType,
						PublicKey:      rogueKP.PublicKey,
						KeyFingerprint: rogueKP.Fingerprint,
						PrivateKey:     rogueKP.PrivateKeyPEM,
					}}},
				},
			},
			{
				name: "delete",
				edit: &dockwellv1.Edit{
					Resource:     &dockwellv1.ResourceRef{Kind: "git.deploy_key", Scope: defaultProjectID, Name: key.GetName()},
					BaseRevision: test3.Msg.GetKey().GetRevision(),
					Delete:       true,
				},
			},
		} {
			if _, err := g.c.ProposeChange(ctx, connect.NewRequest(&dockwellv1.ProposeChangeRequest{
				Summary: "Bypass git-keys (" + tc.name + ")",
				Edits:   []*dockwellv1.Edit{tc.edit},
			})); connect.CodeOf(err) != connect.CodeInvalidArgument {
				t.Fatalf("ProposeChange(%s) on git.deploy_key = %v, want InvalidArgument", tc.name, err)
			}
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
		// N13: temporary pack files are removed as soon as the agent finishes reading them.
		if entries, err := os.ReadDir(filepath.Join(g.f.config.DataDir, "ssh-packs")); err == nil {
			for _, e := range entries {
				if strings.HasPrefix(e.Name(), "dockwell-ssh-pack-") {
					t.Fatalf("leftover SSH pack temp file after build: %s", e.Name())
				}
			}
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

		// 10. Host key change observed during a background fetch (no user principal)
		// persists host_key_changed = true on the deploy key (L3), stops fetches,
		// and requires admin + step-up to accept.
		newAlgo, newFP, err := srv.RotateHostKeyECDSA()
		if err != nil {
			t.Fatalf("RotateHostKeyECDSA: %v", err)
		}
		if _, err := g.f.engine.Propose(ctx, revision.Proposal{
			Summary: "Background SSH resolve check",
			Actor:   revision.SystemActor,
			Via:     revision.ViaAutomatic,
			Edits: []revision.Edit{{
				Ref: resource.Ref{Kind: service.Kind, Scope: web.GetResource().GetScope(), Name: "sshweb"},
				Spec: &resourcev1.Service{
					Runs: &resourcev1.Service_App{App: &resourcev1.Container{Ports: []*resourcev1.PortMapping{{HostPort: 8085, ContainerPort: 8080}}}},
					Build: &resourcev1.ImageBuild{
						Source:   &resourcev1.ImageBuild_Git{Git: &resourcev1.GitSource{Url: sshURL, Ref: "main"}},
						GitLogin: key.GetGitLogin(),
					},
				},
			}},
		}); err == nil {
			t.Fatalf("background engine.Propose succeeded after host key change; want refusal")
		}
		listedChanged, err := keys.ListGitDeployKeys(ctx, connect.NewRequest(&dockwellv1.ListGitDeployKeysRequest{Project: defaultProjectID}))
		if err != nil || len(listedChanged.Msg.GetKeys()) != 1 {
			t.Fatalf("ListGitDeployKeys after background host key change: %v, %v", listedChanged, err)
		}
		obsKey := listedChanged.Msg.GetKeys()[0]
		if !obsKey.GetHostKeyChanged() || obsKey.GetObservedHostKeyAlgorithm() != newAlgo || obsKey.GetObservedHostKeyFingerprint() != newFP {
			t.Fatalf("background fetch did not record observed host key change: %+v (want %s %s)", obsKey, newAlgo, newFP)
		}

		// N7: a planned git.deploy_key change left behind (e.g. if ApplyOwned failed
		// to start) cannot be applied or discarded directly via ChangeService by
		// either an owner or a developer (tested on both SQLite and Postgres).
		leftoverPlan, err := g.f.engine.Propose(ctx, revision.Proposal{
			Summary: "Simulated leftover host-key pin",
			Actor:   revision.SystemActor,
			Via:     revision.ViaAPI,
			Edits: []revision.Edit{{
				Ref:  resource.Ref{Kind: gitdeploykey.Kind, Scope: defaultProjectID, Name: key.GetName()},
				Base: int(obsKey.GetRevision()),
				Spec: &resourcev1.GitDeployKey{
					Repository:         obsKey.GetRepository(),
					Host:               obsKey.GetHost(),
					Port:               obsKey.GetPort(),
					User:               obsKey.GetUser(),
					KeyType:            obsKey.GetKeyType(),
					PublicKey:          obsKey.GetPublicKey(),
					KeyFingerprint:     obsKey.GetKeyFingerprint(),
					HostKeyAlgorithm:   newAlgo,
					HostKeyFingerprint: newFP,
					CreatedAt:          obsKey.GetCreateTime().AsTime().Unix(),
				},
			}},
		})
		if err != nil {
			t.Fatalf("simulate leftover git.deploy_key plan: %v", err)
		}
		if _, err := g.c.ApplyChange(ctx, connect.NewRequest(&dockwellv1.ApplyChangeRequest{Id: leftoverPlan.Change})); connect.CodeOf(err) != connect.CodeInvalidArgument {
			t.Fatalf("owner ApplyChange on planned git.deploy_key = %v, want InvalidArgument", err)
		}
		if _, err := g.c.DiscardChange(ctx, connect.NewRequest(&dockwellv1.DiscardChangeRequest{Id: leftoverPlan.Change})); connect.CodeOf(err) != connect.CodeInvalidArgument {
			t.Fatalf("owner DiscardChange on planned git.deploy_key = %v, want InvalidArgument", err)
		}

		// N11: if the host intermittently presents the old pinned key again, a
		// passing TestGitDeployKey must NOT clear the "Host key changed" warning.
		srv.RestoreInitialHostKey()
		intermittentOK, err := keys.TestGitDeployKey(ctx, connect.NewRequest(&dockwellv1.TestGitDeployKeyRequest{Id: key.GetId()}))
		if err != nil || intermittentOK.Msg.GetStatus() != dockwellv1.GitDeployKeyTestStatus_GIT_DEPLOY_KEY_TEST_STATUS_OK {
			t.Fatalf("intermittent TestGitDeployKey = %+v, %v; want OK", intermittentOK, err)
		}
		if !intermittentOK.Msg.GetKey().GetHostKeyChanged() || intermittentOK.Msg.GetKey().GetObservedHostKeyFingerprint() != newFP {
			t.Fatalf("passing test cleared host_key_changed warning (N11): %+v", intermittentOK.Msg.GetKey())
		}
		// Rotate back to a changed ECDSA host key for the rest of the check.
		newAlgo, newFP, err = srv.RotateHostKeyECDSA()
		if err != nil {
			t.Fatalf("RotateHostKeyECDSA (second): %v", err)
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
		// A project developer cannot accept a changed host key and cannot bypass via ProposeChange, ApplyChange, or DiscardChange.
		hold := aMember(t, g.f)
		hold(in(defaultProjectID, store.RoleDeveloper))
		if _, err := g.c.ApplyChange(ctx, connect.NewRequest(&dockwellv1.ApplyChangeRequest{Id: leftoverPlan.Change})); connect.CodeOf(err) != connect.CodeInvalidArgument {
			t.Fatalf("developer ApplyChange on planned git.deploy_key = %v, want InvalidArgument", err)
		}
		if _, err := g.c.DiscardChange(ctx, connect.NewRequest(&dockwellv1.DiscardChangeRequest{Id: leftoverPlan.Change})); connect.CodeOf(err) != connect.CodeInvalidArgument {
			t.Fatalf("developer DiscardChange on planned git.deploy_key = %v, want InvalidArgument", err)
		}
		if err := g.f.engine.Discard(ctx, leftoverPlan.Change, revision.By{Actor: revision.SystemActor, Via: revision.ViaAutomatic}); err != nil {
			t.Fatalf("cleanup simulated leftover plan: %v", err)
		}
		if _, err := g.c.ProposeChange(ctx, connect.NewRequest(&dockwellv1.ProposeChangeRequest{
			Summary: "Developer bypass host key pin",
			Edits: []*dockwellv1.Edit{{
				Resource: &dockwellv1.ResourceRef{Kind: "git.deploy_key", Scope: defaultProjectID, Name: key.GetName()},
				Spec: &resourcev1.Spec{Kind: &resourcev1.Spec_GitDeployKey{GitDeployKey: &resourcev1.GitDeployKey{
					Repository:         sshURL,
					HostKeyAlgorithm:   newAlgo,
					HostKeyFingerprint: newFP,
				}}},
			}},
		})); connect.CodeOf(err) != connect.CodeInvalidArgument {
			t.Fatalf("developer ProposeChange on git.deploy_key = %v, want InvalidArgument", err)
		}
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

// TestSSHDeployKeyConcurrencyCap verifies cross-project fairness, FIFO waiting
// for transient contention, and bounded busy refusal ("too_many_readings") on
// sustained contention across projects (H2, N8).
func TestSSHDeployKeyConcurrencyCap(t *testing.T) {
	t.Parallel()
	g := newGitBench(t, store.SQLite)
	ctx := t.Context()
	g.repo.Commit("main", "Initial commit", map[string]string{"Dockerfile": webDockerfile})

	srv, err := gitsshtest.NewServer("127.0.0.1:0", "/srv/git/app.git", g.repo.BareDir())
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = srv.Close() })

	keys := gitKeysClient(g.b)
	projA := envProject(t, g.f, listedEnv(t, g.b))
	projB := resourceIn(t, apply(t, g.c, propose(t, g.c, "Make second project", &dockwellv1.Edit{
		Resource: &dockwellv1.ResourceRef{Kind: "project", Name: "projb"},
		Spec:     &resourcev1.Spec{Kind: &resourcev1.Spec_Project{Project: &resourcev1.Project{}}},
	})), "project")

	makeKey := func(projectID string) *dockwellv1.GitDeployKeyInfo {
		t.Helper()
		created, err := keys.CreateGitDeployKey(ctx, connect.NewRequest(&dockwellv1.CreateGitDeployKeyRequest{
			Project:    projectID,
			Repository: srv.URL(),
			Proof:      stepUpFor(t, g.b, dockwellv1.StepUpAction_STEP_UP_ACTION_CREATE_GIT_DEPLOY_KEY, projectID),
		}))
		if err != nil {
			t.Fatalf("CreateGitDeployKey(%s): %v", projectID, err)
		}
		return created.Msg.GetKey()
	}
	keyA := makeKey(projA)
	keyB := makeKey(projB)
	if err := srv.SetAuthorizedKeys(keyA.GetPublicKey(), keyB.GetPublicKey()); err != nil {
		t.Fatal(err)
	}
	algo, fp := srv.HostKey()
	for _, k := range []*dockwellv1.GitDeployKeyInfo{keyA, keyB} {
		if _, err := keys.TestGitDeployKey(ctx, connect.NewRequest(&dockwellv1.TestGitDeployKeyRequest{
			Id:                 k.GetId(),
			AcceptHostKey:      true,
			HostKeyAlgorithm:   algo,
			HostKeyFingerprint: fp,
		})); err != nil {
			t.Fatalf("initial TestGitDeployKey(%s): %v", k.GetId(), err)
		}
	}

	// 1. Short delay (< defaultListWait = 2s): a second call on project A waits
	// in FIFO order and succeeds rather than failing immediately.
	srv.SetDelay(200 * time.Millisecond)
	var wg sync.WaitGroup
	shortErrs := make([]error, 2)
	for i := range 2 {
		wg.Add(1)
		go func() {
			defer wg.Done()
			_, shortErrs[i] = keys.TestGitDeployKey(ctx, connect.NewRequest(&dockwellv1.TestGitDeployKeyRequest{Id: keyA.GetId()}))
		}()
	}
	wg.Wait()
	for i, err := range shortErrs {
		if err != nil {
			t.Fatalf("short-wait TestGitDeployKey[%d] = %v, want nil", i, err)
		}
	}

	// 2. Long delay (> defaultListWait = 2s): project A holds its 1 listing slot
	// so its second call hits ResourceExhausted ("too_many_readings"), while
	// project B still gets the second listing slot concurrently and succeeds (N8).
	srv.SetDelay(2300 * time.Millisecond)
	defer srv.SetDelay(0)

	errsA := make([]error, 2)
	var errB error
	for i := range 2 {
		wg.Add(1)
		go func() {
			defer wg.Done()
			_, errsA[i] = keys.TestGitDeployKey(ctx, connect.NewRequest(&dockwellv1.TestGitDeployKeyRequest{Id: keyA.GetId()}))
		}()
	}
	wg.Add(1)
	go func() {
		defer wg.Done()
		_, errB = keys.TestGitDeployKey(ctx, connect.NewRequest(&dockwellv1.TestGitDeployKeyRequest{Id: keyB.GetId()}))
	}()
	wg.Wait()

	if errB != nil {
		t.Fatalf("project B TestGitDeployKey while project A saturated its cap = %v, want nil (N8 fairness)", errB)
	}
	var okA, busyA int
	for _, err := range errsA {
		switch {
		case err == nil:
			okA++
		case connect.CodeOf(err) == connect.CodeResourceExhausted && reasonOf(err) == "too_many_readings":
			busyA++
		default:
			t.Fatalf("unexpected error from concurrent TestGitDeployKey: %v (code %v, reason %q)", err, connect.CodeOf(err), reasonOf(err))
		}
	}
	if okA != 1 || busyA != 1 {
		t.Fatalf("project A concurrent TestGitDeployKey: ok=%d busy=%d, want ok=1 busy=1", okA, busyA)
	}
}

// TestDockerImageBuildOverSSHOnBothStores verifies standalone Docker image
// builds (Docker -> Images -> Build) from an SSH repository using a project
// deploy key: the verified pack is fetched on the control plane and streamed to
// the building server, cross-project keys and changed host keys are refused,
// and temporary pack files are cleaned up.
func TestDockerImageBuildOverSSHOnBothStores(t *testing.T) {
	t.Parallel()
	storetest.Each(t, func(t *testing.T, backend store.Backend) {
		g := newGitBench(t, backend)
		g.e.SetDockerRoot(t.TempDir())
		ctx := t.Context()
		commit1 := g.repo.Commit("main", "Standalone Docker SSH build", map[string]string{
			"Dockerfile": "FROM scratch\nRUN echo ssh-docker-image\n",
		})
		srv, err := gitsshtest.NewServer("127.0.0.1:0", "acme/img.git", g.repo.BareDir())
		if err != nil {
			t.Fatalf("start ssh git server: %v", err)
		}
		t.Cleanup(func() { _ = srv.Close() })

		sshURL := fmt.Sprintf("ssh://git@%s/acme/img.git", srv.Addr)
		keys := gitKeysClient(g.b)
		dockerAPI := dockwellv1connect.NewDockerServiceClient(g.b, g.b.origin, connect.WithProtoJSON())
		serverID := g.f.lastDocker.id.ServerID
		defaultProjectID := envProject(t, g.f, listedEnv(t, g.b))

		createdRes, err := keys.CreateGitDeployKey(ctx, connect.NewRequest(&dockwellv1.CreateGitDeployKeyRequest{
			Project:    defaultProjectID,
			Repository: sshURL,
			Proof:      stepUpFor(t, g.b, dockwellv1.StepUpAction_STEP_UP_ACTION_CREATE_GIT_DEPLOY_KEY, defaultProjectID),
		}))
		if err != nil {
			t.Fatalf("CreateGitDeployKey: %v", err)
		}
		key := createdRes.Msg.GetKey()
		if err := srv.SetAuthorizedKeys(key.GetPublicKey()); err != nil {
			t.Fatalf("SetAuthorizedKeys: %v", err)
		}
		algo, fp := srv.HostKey()
		testOK, err := keys.TestGitDeployKey(ctx, connect.NewRequest(&dockwellv1.TestGitDeployKeyRequest{
			Id:                 key.GetId(),
			AcceptHostKey:      true,
			HostKeyAlgorithm:   algo,
			HostKeyFingerprint: fp,
		}))
		if err != nil || testOK.Msg.GetStatus() != dockwellv1.GitDeployKeyTestStatus_GIT_DEPLOY_KEY_TEST_STATUS_OK {
			t.Fatalf("TestGitDeployKey = %+v, %v", testOK, err)
		}

		// 1. Plan and apply a standalone Docker image build from the SSH repo.
		planned, err := dockerAPI.PlanDockerMaintenance(ctx, connect.NewRequest(&dockwellv1.PlanDockerMaintenanceRequest{
			ServerIds: []string{serverID},
			Operation: &dockwellv1.PlanDockerMaintenanceRequest_Build{Build: &dockwellv1.DockerImageBuild{
				Output:            "example/ssh-image",
				CredentialProject: defaultProjectID,
				Git: &dockwellv1.DockerImageGitSource{
					Repository: sshURL,
					Ref:        "main",
					Project:    defaultProjectID,
				},
			}},
		}))
		if err != nil {
			t.Fatalf("PlanDockerMaintenance over SSH: %v", err)
		}
		ch := planned.Msg.GetChange()
		action := ch.GetPlan().GetResources()[0].GetDockerMaintenance().GetImageAction()
		if action.GetGitCommit() != commit1 || action.GetBuild().GetGit().GetGitLogin() != key.GetGitLogin() || action.GetDockerfileSha256() == "" {
			t.Fatalf("standalone SSH image plan = %+v, want commit %s login %s", action, commit1, key.GetGitLogin())
		}
		applied := g.done(t, ch)
		if applied.GetState() != dockwellv1.ChangeState_CHANGE_STATE_APPLIED {
			t.Fatalf("standalone SSH image build ended %v: %s", applied.GetState(), applied.GetError())
		}
		if _, err := g.e.Client().InspectImage(ctx, action.GetBuild().GetOutput()); err != nil {
			t.Fatalf("built SSH image missing on server: %v", err)
		}
		if entries, err := os.ReadDir(filepath.Join(g.f.config.DataDir, "ssh-packs")); err == nil {
			for _, e := range entries {
				if strings.HasPrefix(e.Name(), "dockwell-ssh-pack-") {
					t.Fatalf("leftover SSH pack temp file after Docker image build: %s", e.Name())
				}
			}
		}

		// 2. Refusal when the deploy key belongs to another project.
		otherProj := resourceIn(t, apply(t, g.c, propose(t, g.c, "Make other project", &dockwellv1.Edit{
			Resource: &dockwellv1.ResourceRef{Kind: "project", Name: "other-docker"},
			Spec:     &resourcev1.Spec{Kind: &resourcev1.Spec_Project{Project: &resourcev1.Project{}}},
		})), "project")
		for _, login := range []string{key.GetGitLogin(), ""} {
			if _, err := dockerAPI.PlanDockerMaintenance(ctx, connect.NewRequest(&dockwellv1.PlanDockerMaintenanceRequest{
				ServerIds: []string{serverID},
				Operation: &dockwellv1.PlanDockerMaintenanceRequest_Build{Build: &dockwellv1.DockerImageBuild{
					Output:            "example/stolen-ssh",
					CredentialProject: otherProj,
					Git: &dockwellv1.DockerImageGitSource{
						Repository: sshURL,
						Ref:        "main",
						Project:    otherProj,
						GitLogin:   login,
					},
				}},
			})); err == nil {
				t.Fatalf("cross-project Docker image build (login=%q) succeeded; want refusal", login)
			}
		}

		// 3. Refusal when host key changes.
		if _, _, err := srv.RotateHostKeyECDSA(); err != nil {
			t.Fatalf("RotateHostKeyECDSA: %v", err)
		}
		if _, err := dockerAPI.PlanDockerMaintenance(ctx, connect.NewRequest(&dockwellv1.PlanDockerMaintenanceRequest{
			ServerIds: []string{serverID},
			Operation: &dockwellv1.PlanDockerMaintenanceRequest_Build{Build: &dockwellv1.DockerImageBuild{
				Output:            "example/changed-host",
				CredentialProject: defaultProjectID,
				Git: &dockwellv1.DockerImageGitSource{
					Repository: sshURL,
					Ref:        "main",
					Project:    defaultProjectID,
					GitLogin:   key.GetGitLogin(),
				},
			}},
		})); err == nil {
			t.Fatalf("Docker image build succeeded after SSH host key change; want refusal")
		}
	})
}

// TestGitLabConnectionPickerAndPrivateBuildOnBothStores verifies GitLab
// connection scope checks (read_repository is enough for builds), project-scoped
// project listings, HTTPS branch listing, control-plane verified pack streaming
// for app builds and standalone Docker image builds, host isolation (token never
// sent to another host), project reach isolation, and zero token leakage.
func TestGitLabConnectionPickerAndPrivateBuildOnBothStores(t *testing.T) {
	t.Parallel()
	storetest.Each(t, func(t *testing.T, backend store.Backend) {
		repo := gittest.Start(t).Repo("web")
		_ = repo.Commit("main", "Initial private GitLab commit", map[string]string{
			"Dockerfile": webDockerfile,
			"app.js":     "v1",
		})
		repo.Commit("release", "Release branch", map[string]string{
			"Dockerfile": webDockerfile,
			"app.js":     "release-v1",
		})
		commitMain := repo.Commit("main", "Second commit on GitLab main", map[string]string{
			"app.js":             "v2",
			"Dockerfile.scratch": "FROM scratch\nRUN echo gitlab-docker-image\n",
		})

		gl := gltest.New()
		gl.SetProjects([]gltest.Project{
			{PathWithNamespace: "acme/web", WebURL: "https://gitlab.example.com/acme/web", DefaultBranch: "main"},
			{PathWithNamespace: "other/app", WebURL: "https://gitlab.example.com/other/app", DefaultBranch: "main"},
		})
		gl.SetRepo("acme/web", repo.BareDir())
		glSrv := httptest.NewServer(gl)
		t.Cleanup(func() {
			glSrv.Close()
			gl.Close()
		})

		f := startFullConfig(t, backend, nil, slog.New(slog.DiscardHandler), nil, logapi.Options{}, func(cfg *Config) {
			cfg.GitLab = GitLabConfig{Base: glSrv.URL, Client: glSrv.Client()}
		})
		b := signedIn(t, f)
		e := f.addDockerServer(t)
		e.SetDockerRoot(t.TempDir())
		ctx := t.Context()
		c := changeClient(b, b.origin)
		s := dockwellv1connect.NewServiceServiceClient(b, b.origin, connect.WithProtoJSON())
		builds := dockwellv1connect.NewBuildServiceClient(b, b.origin, connect.WithProtoJSON())
		gp := dockwellv1connect.NewGitProviderServiceClient(b, b.origin, connect.WithProtoJSON())
		dockerAPI := dockwellv1connect.NewDockerServiceClient(b, b.origin, connect.WithProtoJSON())
		defaultEnv := listedEnv(t, b)
		defaultProjectID := envProject(t, f, defaultEnv)

		// 1. Scope checks: api -> full capabilities; read_api+read_repository ->
		// build_private+list_projects; read_repository alone -> build_private only;
		// read_api alone -> MISSING_SCOPE (L2).
		chkAPI, err := gp.CheckGitLabConnection(ctx, connect.NewRequest(&dockwellv1.CheckGitLabConnectionRequest{
			Url: "https://gitlab.example.com", Token: gltest.TokenAPI,
		}))
		if err != nil || chkAPI.Msg.GetState() != dockwellv1.GitLabConnectionState_GIT_LAB_CONNECTION_STATE_CONNECTED ||
			!chkAPI.Msg.GetCapabilities().GetBuildPrivate() || !chkAPI.Msg.GetCapabilities().GetListProjects() || !chkAPI.Msg.GetCapabilities().GetPostStatuses() {
			t.Fatalf("CheckGitLabConnection(api) = %+v, %v", chkAPI, err)
		}
		chkRepo, err := gp.CheckGitLabConnection(ctx, connect.NewRequest(&dockwellv1.CheckGitLabConnectionRequest{
			Url: "https://gitlab.example.com", Token: gltest.TokenReadRepo,
		}))
		if err != nil || chkRepo.Msg.GetState() != dockwellv1.GitLabConnectionState_GIT_LAB_CONNECTION_STATE_CONNECTED ||
			!chkRepo.Msg.GetCapabilities().GetBuildPrivate() || !chkRepo.Msg.GetCapabilities().GetListProjects() || chkRepo.Msg.GetCapabilities().GetPostStatuses() {
			t.Fatalf("CheckGitLabConnection(read_api+read_repository) = %+v, %v", chkRepo, err)
		}
		chkRepoOnly, err := gp.CheckGitLabConnection(ctx, connect.NewRequest(&dockwellv1.CheckGitLabConnectionRequest{
			Url: "https://gitlab.example.com", Token: gltest.TokenRepoOnly,
		}))
		if err != nil || chkRepoOnly.Msg.GetState() != dockwellv1.GitLabConnectionState_GIT_LAB_CONNECTION_STATE_CONNECTED ||
			!chkRepoOnly.Msg.GetCapabilities().GetBuildPrivate() || chkRepoOnly.Msg.GetCapabilities().GetListProjects() || chkRepoOnly.Msg.GetCapabilities().GetPostStatuses() {
			t.Fatalf("CheckGitLabConnection(read_repository) = %+v, %v", chkRepoOnly, err)
		}
		chkReadOnly, err := gp.CheckGitLabConnection(ctx, connect.NewRequest(&dockwellv1.CheckGitLabConnectionRequest{
			Url: "https://gitlab.example.com", Token: gltest.TokenReadOnly,
		}))
		if err != nil || chkReadOnly.Msg.GetState() != dockwellv1.GitLabConnectionState_GIT_LAB_CONNECTION_STATE_MISSING_SCOPE ||
			chkReadOnly.Msg.GetCapabilities().GetBuildPrivate() || !chkReadOnly.Msg.GetCapabilities().GetListProjects() || chkReadOnly.Msg.GetCapabilities().GetPostStatuses() {
			t.Fatalf("CheckGitLabConnection(read_api only) = %+v, %v", chkReadOnly, err)
		}

		// 2. Connect GitLab scoped ONLY to defaultProjectID using read_api+read_repository
		// (proving read_repository is enough without api scope).
		otherProj := resourceIn(t, apply(t, c, propose(t, c, "Make other project", &dockwellv1.Edit{
			Resource: &dockwellv1.ResourceRef{Kind: "project", Name: "gl-other"},
			Spec:     &resourcev1.Spec{Kind: &resourcev1.Spec_Project{Project: &resourcev1.Project{}}},
		})), "project")
		otherEnv := resourceIn(t, apply(t, c, propose(t, c, "Make other env", &dockwellv1.Edit{
			Resource: &dockwellv1.ResourceRef{Kind: "environment", Scope: otherProj, Name: "prod"},
			Spec:     &resourcev1.Spec{Kind: &resourcev1.Spec_Environment{Environment: &resourcev1.Environment{}}},
		})), "environment")

		connProp, err := c.ProposeChange(ctx, connect.NewRequest(&dockwellv1.ProposeChangeRequest{
			Summary: "Connect GitLab for default project",
			Edits: []*dockwellv1.Edit{{
				Resource: &dockwellv1.ResourceRef{Kind: "gitlab.connection", Name: "corp-gl"},
				Spec: &resourcev1.Spec{Kind: &resourcev1.Spec_GitlabConnection{GitlabConnection: &resourcev1.GitLabConnection{
					InstanceUrl: "https://gitlab.example.com",
					Token:       gltest.TokenReadRepo,
					PathPrefix:  "acme",
					Projects:    []string{defaultProjectID},
				}}},
			}},
		}))
		if err != nil {
			t.Fatalf("ProposeChange(gitlab.connection): %v", err)
		}
		doneChange(t, c, connProp.Msg.GetChange())

		// 3. ListGitLabProjects returns acme/web (filtered by path_prefix) with default_branch
		// for defaultProjectID, and nothing for otherProj (project reach enforced).
		listedA, err := gp.ListGitLabProjects(ctx, connect.NewRequest(&dockwellv1.ListGitLabProjectsRequest{Project: defaultProjectID}))
		if err != nil || len(listedA.Msg.GetProjects()) != 1 || listedA.Msg.GetProjects()[0].GetPathWithNamespace() != "acme/web" || listedA.Msg.GetProjects()[0].GetDefaultBranch() != "main" {
			t.Fatalf("ListGitLabProjects(default) = %+v, %v", listedA, err)
		}
		listedB, err := gp.ListGitLabProjects(ctx, connect.NewRequest(&dockwellv1.ListGitLabProjectsRequest{Project: otherProj}))
		if err != nil || len(listedB.Msg.GetProjects()) != 0 || len(listedB.Msg.GetConnections()) != 0 {
			t.Fatalf("ListGitLabProjects(other) = %+v, %v; want empty", listedB, err)
		}

		// 4. ListRepositoryBranches and DetectBuild read through the GitLab connection's token.
		repoURL := "https://gitlab.example.com/acme/web"
		brRes, err := builds.ListRepositoryBranches(ctx, connect.NewRequest(&dockwellv1.ListRepositoryBranchesRequest{
			Url:     repoURL,
			Project: defaultProjectID,
		}))
		if err != nil {
			t.Fatalf("ListRepositoryBranches(GitLab): %v", err)
		}
		if brRes.Msg.GetGitLogin() != "gitlab:corp-gl" || brRes.Msg.GetDefaultBranch() != "main" || !slices.Contains(brRes.Msg.GetBranches(), "release") {
			t.Fatalf("ListRepositoryBranches(GitLab) = %+v", brRes.Msg)
		}

		detRes, err := builds.DetectBuild(ctx, connect.NewRequest(&dockwellv1.DetectBuildRequest{
			Project: defaultProjectID,
			Build: &resourcev1.ImageBuild{
				Source:   &resourcev1.ImageBuild_Git{Git: &resourcev1.GitSource{Url: repoURL, Ref: "main"}},
				GitLogin: "gitlab:corp-gl",
			},
		}))
		if err != nil {
			t.Fatalf("DetectBuild(GitLab): %v", err)
		}
		if detRes.Msg.GetDetection().GetBuilder() != resourcev1.ImageBuild_BUILDER_DOCKERFILE {
			t.Fatalf("DetectBuild(GitLab) = %+v", detRes.Msg)
		}

		// 5. Plan and build an app from the private GitLab project: plan names gitlab:corp-gl,
		// build streams verified pack from control plane, and token never leaks.
		appProp, err := c.ProposeChange(ctx, connect.NewRequest(&dockwellv1.ProposeChangeRequest{
			Summary: "Build private GitLab app",
			Edits: []*dockwellv1.Edit{{
				Resource: &dockwellv1.ResourceRef{Kind: "service", Scope: defaultEnv, Name: "glweb"},
				Spec: &resourcev1.Spec{Kind: &resourcev1.Spec_Service{Service: &resourcev1.Service{
					Runs: &resourcev1.Service_App{App: &resourcev1.Container{Ports: []*resourcev1.PortMapping{{HostPort: 8087, ContainerPort: 8080}}}},
					Build: &resourcev1.ImageBuild{
						Source: &resourcev1.ImageBuild_Git{Git: &resourcev1.GitSource{Url: repoURL, Ref: "main"}},
					},
				}}},
			}},
		}))
		if err != nil {
			t.Fatalf("ProposeChange(GitLab app): %v", err)
		}
		plan := appProp.Msg.GetChange()
		step := buildStep(plan)
		if step == nil || step.GetCommit() != commitMain || step.GetGitLogin() != "gitlab:corp-gl" {
			t.Fatalf("GitLab build plan step = %+v, want commit %s login gitlab:corp-gl", step, commitMain)
		}
		assertNoGLToken := func(label string, msg proto.Message) {
			t.Helper()
			raw, err := protojson.Marshal(msg)
			if err != nil {
				t.Fatal(err)
			}
			if strings.Contains(string(raw), gltest.TokenReadRepo) {
				t.Fatalf("%s leaked GitLab access token", label)
			}
		}
		assertNoGLToken("ProposeChange(app)", appProp.Msg)
		applied := doneChange(t, c, plan)
		if applied.GetState() != dockwellv1.ChangeState_CHANGE_STATE_APPLIED {
			t.Fatalf("GitLab app build ended %v: %s", applied.GetState(), applied.GetError())
		}
		listedSvcs, err := s.ListServices(ctx, connect.NewRequest(&dockwellv1.ListServicesRequest{}))
		if err != nil {
			t.Fatalf("ListServices: %v", err)
		}
		var glSvc *dockwellv1.ServiceInfo
		for _, svc := range listedSvcs.Msg.GetServices() {
			if svc.GetResource().GetName() == "glweb" {
				glSvc = svc
				break
			}
		}
		if glSvc == nil {
			t.Fatalf("glweb not listed in ListServices")
		}
		running := e.Newest(docker.Project(glSvc.GetResource().GetScope()), "glweb")
		if running == nil || !running.State.Running {
			t.Fatalf("GitLab-built container not running: %+v", running)
		}
		if entries, err := os.ReadDir(filepath.Join(f.config.DataDir, "ssh-packs")); err == nil {
			for _, ent := range entries {
				if strings.HasPrefix(ent.Name(), "dockwell-git-pack-") {
					t.Fatalf("leftover GitLab pack temp file after build: %s", ent.Name())
				}
			}
		}

		// 6. Standalone Docker image build from the private GitLab project also uses the verified pack stream.
		dockerPlan, err := dockerAPI.PlanDockerMaintenance(ctx, connect.NewRequest(&dockwellv1.PlanDockerMaintenanceRequest{
			ServerIds: []string{f.lastDocker.id.ServerID},
			Operation: &dockwellv1.PlanDockerMaintenanceRequest_Build{Build: &dockwellv1.DockerImageBuild{
				Output:            "example/gl-image",
				DockerfilePath:    "Dockerfile.scratch",
				CredentialProject: defaultProjectID,
				Git: &dockwellv1.DockerImageGitSource{
					Repository: repoURL,
					Ref:        "main",
					Project:    defaultProjectID,
					GitLogin:   "gitlab:corp-gl",
				},
			}},
		}))
		if err != nil {
			t.Fatalf("PlanDockerMaintenance(GitLab): %v", err)
		}
		imgAction := dockerPlan.Msg.GetChange().GetPlan().GetResources()[0].GetDockerMaintenance().GetImageAction()
		if imgAction.GetBuild().GetGit().GetGitLogin() != "gitlab:corp-gl" || imgAction.GetGitCommit() != commitMain {
			t.Fatalf("GitLab Docker image plan = %+v", imgAction)
		}
		doneChange(t, c, dockerPlan.Msg.GetChange())
		if _, err := e.Client().InspectImage(ctx, imgAction.GetBuild().GetOutput()); err != nil {
			t.Fatalf("GitLab-built Docker image missing on server: %v", err)
		}

		// 7. Token is never sent to another host, wrong path prefix, or cleartext HTTP.
		for _, badURL := range []string{
			"https://other.example.com/acme/web",
			"https://gitlab.example.com/other/app",
			"http://gitlab.example.com/acme/web",
		} {
			if _, err := builds.ListRepositoryBranches(ctx, connect.NewRequest(&dockwellv1.ListRepositoryBranchesRequest{
				Url:      badURL,
				Project:  defaultProjectID,
				GitLogin: "gitlab:corp-gl",
			})); err == nil {
				t.Fatalf("ListRepositoryBranches(%s) with gitlab:corp-gl succeeded; want refusal", badURL)
			}
		}

		// 8. Cross-project refusal: otherProj cannot use gitlab:corp-gl for app builds
		// or standalone Docker image builds (L5).
		if _, err := c.ProposeChange(ctx, connect.NewRequest(&dockwellv1.ProposeChangeRequest{
			Summary: "Cross-project GitLab build",
			Edits: []*dockwellv1.Edit{{
				Resource: &dockwellv1.ResourceRef{Kind: "service", Scope: otherEnv, Name: "gl-stolen"},
				Spec: &resourcev1.Spec{Kind: &resourcev1.Spec_Service{Service: &resourcev1.Service{
					Runs: &resourcev1.Service_App{App: &resourcev1.Container{Ports: []*resourcev1.PortMapping{{HostPort: 8088, ContainerPort: 8080}}}},
					Build: &resourcev1.ImageBuild{
						Source:   &resourcev1.ImageBuild_Git{Git: &resourcev1.GitSource{Url: repoURL, Ref: "main"}},
						GitLogin: "gitlab:corp-gl",
					},
				}}},
			}},
		})); err == nil {
			t.Fatalf("cross-project GitLab build succeeded; want refusal")
		}
		if _, err := dockerAPI.PlanDockerMaintenance(ctx, connect.NewRequest(&dockwellv1.PlanDockerMaintenanceRequest{
			ServerIds: []string{f.lastDocker.id.ServerID},
			Operation: &dockwellv1.PlanDockerMaintenanceRequest_Build{Build: &dockwellv1.DockerImageBuild{
				Output:            "example/stolen-gl",
				DockerfilePath:    "Dockerfile.scratch",
				CredentialProject: otherProj,
				Git: &dockwellv1.DockerImageGitSource{
					Repository: repoURL,
					Ref:        "main",
					Project:    otherProj,
					GitLogin:   "gitlab:corp-gl",
				},
			}},
		})); err == nil {
			t.Fatalf("cross-project Docker image build with gitlab:corp-gl succeeded; want refusal (L5)")
		}

		// 8b. L4: gitlab: login with no project/environment scope in context is refused.
		glSources := &gitproviders.Sources{Base: &gitref.Resolver{Client: glSrv.Client()}}
		glSources.SetApps(f.server.apps)
		if _, _, err := glSources.Resolve(revision.WithGitLogin(ctx, "gitlab:corp-gl"), repoURL, "main"); err == nil {
			t.Fatalf("gitproviders.Sources.Resolve with gitlab: login and no scope succeeded; want refusal (L4)")
		}

		// 9. Verify SQLite file on disk contains no unsealed GitLab token.
		if backend == store.SQLite {
			raw, err := os.ReadFile(f.db)
			if err != nil {
				t.Fatal(err)
			}
			if bytes.Contains(raw, []byte(gltest.TokenReadRepo)) {
				t.Fatalf("sqlite database file contains plaintext GitLab token")
			}
		}
	})
}

type rewritePublicGitSources struct {
	base *gitref.Resolver
	from string
	to   string
}

func (r *rewritePublicGitSources) rewrite(u string) string {
	return strings.Replace(u, r.from, r.to, 1)
}

func (r *rewritePublicGitSources) Resolve(ctx context.Context, rawURL, ref string) (string, string, error) {
	return r.base.Resolve(ctx, r.rewrite(rawURL), ref)
}

func (r *rewritePublicGitSources) Branches(ctx context.Context, rawURL string) (string, []string, error) {
	return r.base.Branches(ctx, r.rewrite(rawURL))
}

// TestGitLabPublicRepoStaysPublicAndScopeIsolation covers review M1, M2, and L5:
//   - M1: An existing public GitLab app keeps git_login == "" and redeploys on
//     the agent with no token even after a GitLab connection is added for that
//     host, while an unlisted private repo answering 401 anonymously still pins
//     the connection.
//   - M2: A read_repository-only token getting 403 from ListGitLabProjects
//     reports listing_problem=MISSING_READ_API without marking the connection
//     refused; a CI commit status 403 marks status_scope_missing=true without
//     poisoning Connected build health.
//   - L5: GitLab pack fetch and SSH pack fetch from the same project share the
//     1-per-project fetch gate (second waits until first releases).
func TestGitLabPublicRepoStaysPublicAndScopeIsolation(t *testing.T) {
	t.Parallel()
	storetest.Each(t, func(t *testing.T, backend store.Backend) {
		gitSrv := gittest.Start(t)
		pubRepo := gitSrv.Repo("public")
		pubCommit1 := pubRepo.Commit("main", "Public GitLab v1", map[string]string{
			"Dockerfile": webDockerfile,
			"app.js":     "public-v1",
		})
		privRepo := gitSrv.Repo("secret")
		privCommit := privRepo.Commit("main", "Unlisted private GitLab v1", map[string]string{
			"Dockerfile": webDockerfile,
			"app.js":     "secret-v1",
		})

		gl := gltest.New()
		gl.SetProjects([]gltest.Project{}) // neither repo is in the listed projects
		gl.SetPublicRepo("acme/public", pubRepo.BareDir())
		gl.SetRepo("acme/secret", privRepo.BareDir())
		glSrv := httptest.NewServer(gl)
		t.Cleanup(func() {
			glSrv.Close()
			gl.Close()
		})

		f := startFullConfig(t, backend, nil, slog.New(slog.DiscardHandler), nil, logapi.Options{}, func(cfg *Config) {
			cfg.GitLab = GitLabConfig{Base: glSrv.URL, Client: glSrv.Client()}
			cfg.Sources = &rewritePublicGitSources{
				base: &gitref.Resolver{Client: glSrv.Client()},
				from: "https://gitlab.example.com",
				to:   glSrv.URL,
			}
		})
		b := signedIn(t, f)
		e := f.addDockerServer(t)
		e.SetDockerRoot(t.TempDir())
		e.GitURLRewrite = func(u string) string {
			return strings.Replace(u, "https://gitlab.example.com", glSrv.URL, 1)
		}
		ctx := t.Context()
		c := changeClient(b, b.origin)
		s := dockwellv1connect.NewServiceServiceClient(b, b.origin, connect.WithProtoJSON())
		builds := dockwellv1connect.NewBuildServiceClient(b, b.origin, connect.WithProtoJSON())
		gp := dockwellv1connect.NewGitProviderServiceClient(b, b.origin, connect.WithProtoJSON())
		defaultEnv := listedEnv(t, b)
		defaultProjectID := envProject(t, f, defaultEnv)

		// 1. Deploy an existing public GitLab app BEFORE any GitLab connection exists.
		publicURL := "https://gitlab.example.com/acme/public"
		initProp, err := c.ProposeChange(ctx, connect.NewRequest(&dockwellv1.ProposeChangeRequest{
			Summary: "Deploy public GitLab app",
			Edits: []*dockwellv1.Edit{{
				Resource: &dockwellv1.ResourceRef{Kind: "service", Scope: defaultEnv, Name: "pubweb"},
				Spec: &resourcev1.Spec{Kind: &resourcev1.Spec_Service{Service: &resourcev1.Service{
					Runs: &resourcev1.Service_App{App: &resourcev1.Container{Ports: []*resourcev1.PortMapping{{HostPort: 8091, ContainerPort: 8080}}}},
					Build: &resourcev1.ImageBuild{
						Source: &resourcev1.ImageBuild_Git{Git: &resourcev1.GitSource{Url: publicURL, Ref: "main"}},
					},
				}}},
			}},
		}))
		if err != nil {
			t.Fatalf("ProposeChange(public before connection): %v", err)
		}
		if step := buildStep(initProp.Msg.GetChange()); step == nil || step.GetCommit() != pubCommit1 || step.GetGitLogin() != "" {
			t.Fatalf("initial public build step = %+v, want commit %s with empty git_login", step, pubCommit1)
		}
		doneChange(t, c, initProp.Msg.GetChange())

		// 2. Connect GitLab on the same host with a read_repository-only token (no read_api, no api).
		connProp, err := c.ProposeChange(ctx, connect.NewRequest(&dockwellv1.ProposeChangeRequest{
			Summary: "Connect repo-only GitLab",
			Edits: []*dockwellv1.Edit{{
				Resource: &dockwellv1.ResourceRef{Kind: "gitlab.connection", Name: "repo-only-gl"},
				Spec: &resourcev1.Spec{Kind: &resourcev1.Spec_GitlabConnection{GitlabConnection: &resourcev1.GitLabConnection{
					InstanceUrl: "https://gitlab.example.com",
					Token:       gltest.TokenRepoOnly,
				}}},
			}},
		}))
		if err != nil {
			t.Fatalf("ProposeChange(repo-only gitlab.connection): %v", err)
		}
		doneChange(t, c, connProp.Msg.GetChange())

		// Check connection so it starts in CONNECTED state with BuildPrivate=true, ListProjects=false, PostStatuses=false.
		chk, err := gp.CheckGitLabConnection(ctx, connect.NewRequest(&dockwellv1.CheckGitLabConnectionRequest{
			Name: "repo-only-gl",
			Url:  "https://gitlab.example.com",
		}))
		if err != nil || chk.Msg.GetState() != dockwellv1.GitLabConnectionState_GIT_LAB_CONNECTION_STATE_CONNECTED {
			t.Fatalf("CheckGitLabConnection(repo-only-gl) = %+v, %v", chk, err)
		}

		// 3. M1: Redeploy the existing public GitLab app unchanged -> keeps git_login == ""
		// and builds on the agent with zero authenticated GitLab calls!
		pubCommit2 := pubRepo.Commit("main", "Public GitLab v2", map[string]string{"app.js": "public-v2"})
		brPub, err := builds.ListRepositoryBranches(ctx, connect.NewRequest(&dockwellv1.ListRepositoryBranchesRequest{
			Url:     publicURL,
			Project: defaultProjectID,
		}))
		if err != nil || brPub.Msg.GetGitLogin() != "" {
			t.Fatalf("ListRepositoryBranches(public) = %+v, %v; want empty git_login (M1)", brPub, err)
		}
		listedSvcs, err := s.ListServices(ctx, connect.NewRequest(&dockwellv1.ListServicesRequest{}))
		if err != nil {
			t.Fatal(err)
		}
		var pubSvcID string
		for _, svc := range listedSvcs.Msg.GetServices() {
			if svc.GetResource().GetName() == "pubweb" {
				pubSvcID = svc.GetId()
			}
		}
		redep, err := s.DeployService(ctx, connect.NewRequest(&dockwellv1.DeployServiceRequest{Id: pubSvcID}))
		if err != nil {
			t.Fatalf("DeployService(public after connection): %v", err)
		}
		if step := buildStep(redep.Msg.GetChange()); step == nil || step.GetCommit() != pubCommit2 || step.GetGitLogin() != "" {
			t.Fatalf("public redeploy build step = %+v, want commit %s with empty git_login (M1)", step, pubCommit2)
		}
		doneChange(t, c, redep.Msg.GetChange())
		if gl.GitCalls() != 0 {
			t.Fatalf("public GitLab build made %d authenticated Git calls, want 0 (M1)", gl.GitCalls())
		}

		// 4. M1: Unlisted private repo answers 401 anonymously -> pins gitlab:repo-only-gl and builds!
		secretURL := "https://gitlab.example.com/acme/secret"
		privProp, err := c.ProposeChange(ctx, connect.NewRequest(&dockwellv1.ProposeChangeRequest{
			Summary: "Deploy unlisted private GitLab app",
			Edits: []*dockwellv1.Edit{{
				Resource: &dockwellv1.ResourceRef{Kind: "service", Scope: defaultEnv, Name: "secweb"},
				Spec: &resourcev1.Spec{Kind: &resourcev1.Spec_Service{Service: &resourcev1.Service{
					Runs: &resourcev1.Service_App{App: &resourcev1.Container{Ports: []*resourcev1.PortMapping{{HostPort: 8092, ContainerPort: 8080}}}},
					Build: &resourcev1.ImageBuild{
						Source: &resourcev1.ImageBuild_Git{Git: &resourcev1.GitSource{Url: secretURL, Ref: "main"}},
					},
				}}},
			}},
		}))
		if err != nil {
			t.Fatalf("ProposeChange(unlisted private): %v", err)
		}
		if step := buildStep(privProp.Msg.GetChange()); step == nil || step.GetCommit() != privCommit || step.GetGitLogin() != "gitlab:repo-only-gl" {
			t.Fatalf("private build step = %+v, want commit %s login gitlab:repo-only-gl (M1)", step, privCommit)
		}
		doneChange(t, c, privProp.Msg.GetChange())
		if gl.GitCalls() == 0 {
			t.Fatalf("private GitLab build made 0 authenticated Git calls")
		}

		// 5. M2 (listing 403): ListGitLabProjects with read_repository-only token returns
		// listing_problem=MISSING_READ_API while ListGitProviders stays CONNECTED (never AUTH_FAILED).
		listed, err := gp.ListGitLabProjects(ctx, connect.NewRequest(&dockwellv1.ListGitLabProjectsRequest{Project: defaultProjectID}))
		if err != nil || len(listed.Msg.GetConnections()) != 1 {
			t.Fatalf("ListGitLabProjects(repo-only) = %+v, %v", listed, err)
		}
		pconn := listed.Msg.GetConnections()[0]
		if pconn.GetListingProblem() != dockwellv1.GitLabListingProblem_GIT_LAB_LISTING_PROBLEM_MISSING_READ_API {
			t.Fatalf("listing_problem = %v, want MISSING_READ_API (M2)", pconn.GetListingProblem())
		}
		if pconn.GetState() != dockwellv1.GitLabConnectionState_GIT_LAB_CONNECTION_STATE_CONNECTED {
			t.Fatalf("picker connection state = %v, want CONNECTED (M2)", pconn.GetState())
		}
		provs, err := gp.ListGitProviders(ctx, connect.NewRequest(&dockwellv1.ListGitProvidersRequest{}))
		if err != nil || len(provs.Msg.GetGitlabConnections()) != 1 {
			t.Fatalf("ListGitProviders = %+v, %v", provs, err)
		}
		if provs.Msg.GetGitlabConnections()[0].GetState() != dockwellv1.GitLabConnectionState_GIT_LAB_CONNECTION_STATE_CONNECTED {
			t.Fatalf("ListGitProviders state after listing 403 = %v, want CONNECTED (M2)", provs.Msg.GetGitlabConnections()[0].GetState())
		}

		// 6. M2 (CI status 403): Delivering a CI commit status with read_repository-only
		// token hits 403, records status_scope_missing=true, and leaves connection State CONNECTED!
		listedSvcs, err = s.ListServices(ctx, connect.NewRequest(&dockwellv1.ListServicesRequest{}))
		if err != nil {
			t.Fatal(err)
		}
		var secSvcID string
		for _, svc := range listedSvcs.Msg.GetServices() {
			if svc.GetResource().GetName() == "secweb" {
				secSvcID = svc.GetId()
			}
		}
		ciChgID := id.New(id.Change)
		now := time.Now().UTC()
		if err := f.store.Revise(ctx, func(tx *store.RevisionTx) error {
			if err := tx.CreateChange(ctx, store.Change{
				ID: ciChgID, Summary: "CI deploy secweb", Actor: "tok_ci", State: store.ChangePlanned,
				CreatedAt: now, Plan: []byte(`{}`), Fingerprint: "fp-ci",
			}); err != nil {
				return err
			}
			if err := tx.RecordCIDeploy(ctx, store.CIDeployOrigin{
				CIDeployKey: store.CIDeployKey{
					Service: secSvcID,
					Digest:  "sha256:" + strings.Repeat("b", 64),
					Commit:  privCommit,
				},
				Provider:   int32(dockwellv1.DeployProvider_DEPLOY_PROVIDER_GITLAB_CI),
				Repository: secretURL,
				Ref:        "refs/heads/main",
				Image:      "registry.gitlab.example.com/acme/secret:1",
			}, ciChgID); err != nil {
				return err
			}
			_, err := tx.EnqueueCIStatusForChange(ctx, ciChgID, store.CIStatusHealthy, store.CIStatusEnqueueOpts{
				ServiceName:       "secweb",
				EnvironmentName:   "production",
				AllowedRepository: secretURL,
				Summary:           "Healthy",
			}, now)
			return err
		}); err != nil {
			t.Fatal(err)
		}
		worker := cistatus.New(cistatus.Config{
			DB:      f.store,
			Targets: ciStatusTargets{apps: f.server.apps},
			GitLab:  &gitlab.Client{HTTP: glSrv.Client(), TestBase: glSrv.URL},
		})
		if err := worker.DeliverOnce(ctx); err != nil {
			t.Fatalf("cistatus.DeliverOnce: %v", err)
		}
		provs, err = gp.ListGitProviders(ctx, connect.NewRequest(&dockwellv1.ListGitProvidersRequest{}))
		if err != nil {
			t.Fatal(err)
		}
		glConn := provs.Msg.GetGitlabConnections()[0]
		if glConn.GetState() != dockwellv1.GitLabConnectionState_GIT_LAB_CONNECTION_STATE_CONNECTED || !glConn.GetStatusScopeMissing() {
			t.Fatalf("after CI status 403: state=%v status_scope_missing=%v, want CONNECTED & true (M2)", glConn.GetState(), glConn.GetStatusScopeMissing())
		}
	})
}

// TestSharedPackFetchGateBetweenSSHAndGitLab verifies L5: a GitLab pack fetch
// and an SSH pack fetch for the same project share the 1-per-project fetch gate
// (the second waits until the first releases, while another project is not blocked).
func TestSharedPackFetchGateBetweenSSHAndGitLab(t *testing.T) {
	t.Parallel()
	g := newGitBench(t, store.SQLite)
	ctx := t.Context()
	commit := g.repo.Commit("main", "Shared gate commit", map[string]string{"Dockerfile": webDockerfile})

	sshSrv, err := gitsshtest.NewServer("127.0.0.1:0", "acme/web.git", g.repo.BareDir())
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = sshSrv.Close() })

	keys := gitKeysClient(g.b)
	projA := envProject(t, g.f, listedEnv(t, g.b))
	projB := resourceIn(t, apply(t, g.c, propose(t, g.c, "Make projB", &dockwellv1.Edit{
		Resource: &dockwellv1.ResourceRef{Kind: "project", Name: "projb-gate"},
		Spec:     &resourcev1.Spec{Kind: &resourcev1.Spec_Project{Project: &resourcev1.Project{}}},
	})), "project")

	created, err := keys.CreateGitDeployKey(ctx, connect.NewRequest(&dockwellv1.CreateGitDeployKeyRequest{
		Project:    projA,
		Repository: sshSrv.URL(),
		Proof:      stepUpFor(t, g.b, dockwellv1.StepUpAction_STEP_UP_ACTION_CREATE_GIT_DEPLOY_KEY, projA),
	}))
	if err != nil {
		t.Fatal(err)
	}
	key := created.Msg.GetKey()
	if err := sshSrv.SetAuthorizedKeys(key.GetPublicKey()); err != nil {
		t.Fatal(err)
	}
	algo, fp := sshSrv.HostKey()
	if _, err := keys.TestGitDeployKey(ctx, connect.NewRequest(&dockwellv1.TestGitDeployKeyRequest{
		Id:                 key.GetId(),
		AcceptHostKey:      true,
		HostKeyAlgorithm:   algo,
		HostKeyFingerprint: fp,
	})); err != nil {
		t.Fatal(err)
	}

	// Simulate an active GitLab pack fetch holding projA's pack-fetch slot via AcquirePackFetch.
	releaseGL, err := g.f.server.deployKeys.AcquirePackFetch(ctx, projA)
	if err != nil {
		t.Fatalf("AcquirePackFetch(projA): %v", err)
	}
	// Another project (projB) is NOT blocked by projA.
	releaseB, err := g.f.server.deployKeys.AcquirePackFetch(ctx, projB)
	if err != nil {
		releaseGL()
		t.Fatalf("AcquirePackFetch(projB) while projA held: %v", err)
	}
	releaseB()

	// Start an SSH FetchPackIn in the SAME project (projA): it MUST block until releaseGL() runs.
	sshDone := make(chan error, 1)
	go func() {
		_, _, cleanup, ferr := g.f.server.deployKeys.FetchPackIn(ctx, key.GetGitLogin(), sshSrv.URL(), "main", commit, projA)
		if cleanup != nil {
			cleanup()
		}
		sshDone <- ferr
	}()
	select {
	case err := <-sshDone:
		releaseGL()
		t.Fatalf("SSH FetchPackIn in projA completed while GitLab pack slot was held (err=%v); want shared 1-per-project gate", err)
	case <-time.After(150 * time.Millisecond):
		// Expected: SSH fetch is waiting on the shared gate!
	}
	releaseGL()
	select {
	case err := <-sshDone:
		if err != nil {
			t.Fatalf("SSH FetchPackIn after GitLab release failed: %v", err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("SSH FetchPackIn did not unblock after GitLab slot released")
	}
}
