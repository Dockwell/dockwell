// SPDX-License-Identifier: AGPL-3.0-only

package control

import (
	"slices"
	"strings"
	"testing"
	"time"

	"connectrpc.com/connect"

	v1 "github.com/dockwell/dockwell/gen/dockwell/v1"
	"github.com/dockwell/dockwell/gen/dockwell/v1/dockwellv1connect"
	"github.com/dockwell/dockwell/internal/catalog/snapshot"
	"github.com/dockwell/dockwell/internal/control/authapi"
	"github.com/dockwell/dockwell/internal/docker"
	"github.com/dockwell/dockwell/internal/docker/dockertest"
	"github.com/dockwell/dockwell/internal/store"
	"github.com/dockwell/dockwell/internal/store/storetest"
)

func TestDockerCleanupExactScopeAndReceipts(t *testing.T) {
	t.Parallel()
	storetest.Each(t, func(t *testing.T, backend store.Backend) {
		f := startFull(t, backend, nil)
		b := signedIn(t, f)
		engine := f.addDockerServer(t)
		server := f.lastDocker.id.ServerID
		used := time.Now().UTC().Truncate(time.Second).Add(-time.Hour)
		idle := docker.DiskUsageCache{ID: strings.Repeat("a", 25), Size: 2048, CreatedAt: used, LastUsedAt: &used}
		busy := docker.DiskUsageCache{ID: strings.Repeat("b", 25), Size: 4096, InUse: true, CreatedAt: used, LastUsedAt: &used}
		engine.SetBuildCache(idle, busy)

		dockerRPC := dockwellv1connect.NewDockerServiceClient(b, b.origin, connect.WithProtoJSON())
		changeRPC := changeClient(b, b.origin)
		preview := func() *v1.PreviewDockerCleanupResponse {
			t.Helper()
			res, err := dockerRPC.PreviewDockerCleanup(t.Context(), connect.NewRequest(&v1.PreviewDockerCleanupRequest{ServerIds: []string{server}}))
			if err != nil {
				t.Fatal(err)
			}
			return res.Msg
		}
		plan := func() *v1.Change {
			t.Helper()
			p := preview()
			if len(p.GetCandidates()) != 1 || p.GetCandidates()[0].GetEngineId() != idle.ID ||
				!slices.ContainsFunc(p.GetExclusions(), func(target *v1.DockerCleanupTarget) bool {
					return target.GetEngineId() == busy.ID && target.GetReason() == v1.DockerCleanupReason_DOCKER_CLEANUP_REASON_IN_USE
				}) {
				t.Fatalf("Docker cleanup preview %v", p)
			}
			res, err := dockerRPC.PlanDockerMaintenance(t.Context(), connect.NewRequest(&v1.PlanDockerMaintenanceRequest{
				ServerIds: []string{server}, ExpectedSnapshot: p.GetSnapshot(),
				Operation: &v1.PlanDockerMaintenanceRequest_Cleanup{Cleanup: &v1.DockerCleanupSelection{Targets: p.GetCandidates()}},
			}))
			if err != nil {
				t.Fatal(err)
			}
			ch := res.Msg.GetChange()
			if len(ch.GetPlan().GetServers()) != 1 ||
				len(ch.GetPlan().GetServers()[0].GetSteps()[0].GetDockerMaintenance().GetTargets()) != 1 {
				t.Fatalf("not an exact revisionless plan: %v", ch)
			}
			return ch
		}
		apply := func(ch *v1.Change) *v1.Change {
			t.Helper()
			watch := watchWithin(t, changeRPC, ch.GetId(), 15*time.Second)
			req := &v1.ApplyChangeRequest{Id: ch.GetId(), Fingerprint: ch.GetPlan().GetFingerprint()}
			_, err := changeRPC.ApplyChange(t.Context(), connect.NewRequest(req))
			if reasonOf(err) != authapi.ReasonNeedsStepUp {
				t.Fatalf("Docker cleanup didn't require its own passkey: %v", err)
			}
			req.Proof = stepUpFor(t, b, v1.StepUpAction_STEP_UP_ACTION_APPROVE_CHANGE, ch.GetId()+":"+req.GetFingerprint())
			if _, err := changeRPC.ApplyChange(t.Context(), connect.NewRequest(req)); err != nil {
				t.Fatal(err)
			}
			return waitWatchWithin(t, watch, 15*time.Second).last()
		}

		stale := plan()
		active := idle
		active.InUse = true
		engine.SetBuildCache(active, busy)
		failed := apply(stale)
		if failed.GetState() != v1.ChangeState_CHANGE_STATE_FAILED || !slices.ContainsFunc(failed.GetSteps(), func(s *v1.StepProgress) bool {
			return len(s.GetDockerReceipts()) == 1 && s.GetDockerReceipts()[0].GetState() == v1.DockerReceiptState_DOCKER_RECEIPT_STATE_CHANGED
		}) {
			t.Fatalf("changed cache was not refused: %v", failed)
		}
		engine.SetBuildCache(idle, busy)
		finished := apply(plan())
		if finished.GetState() != v1.ChangeState_CHANGE_STATE_APPLIED {
			t.Fatalf("Docker cleanup failed: %v", finished)
		}
		receipt := finished.GetSteps()[0].GetDockerReceipts()[0]
		if receipt.GetState() != v1.DockerReceiptState_DOCKER_RECEIPT_STATE_REMOVED ||
			receipt.GetBeforeBytes() != 2048 || receipt.AfterBytes == nil || receipt.GetAfterBytes() != 0 {
			t.Fatalf("unexpected receipt: %v", receipt)
		}
		count := 0
		for _, request := range engine.Seen() {
			if strings.HasSuffix(request, "/build/prune") {
				count++
			}
			if strings.Contains(request, "/system/prune") || strings.HasPrefix(request, "DELETE ") {
				t.Fatalf("cleanup used %q", request)
			}
		}
		if count != 1 || len(preview().GetCandidates()) != 0 {
			t.Fatalf("cleanup dispatched %d prune calls", count)
		}
	})
}

func TestDockerCleanupResumesAfterPasskeyAndKeepsCatalogPin(t *testing.T) {
	t.Parallel()
	storetest.Each(t, func(t *testing.T, backend store.Backend) {
		f := startFull(t, backend, nil)
		b := signedIn(t, f)
		engine := f.addDockerServer(t)
		serverIDs := []string{f.lastDocker.id.ServerID}
		client := dockwellv1connect.NewDockerServiceClient(b, b.origin, connect.WithProtoJSON())
		changes := changeClient(b, b.origin)
		preview := func() *v1.PreviewDockerCleanupResponse {
			t.Helper()
			p, err := client.PreviewDockerCleanup(t.Context(), connect.NewRequest(&v1.PreviewDockerCleanupRequest{
				ServerIds: serverIDs, Filters: &v1.DockerCleanupFilters{TaggedImages: true},
			}))
			if err != nil {
				t.Fatal(err)
			}
			return p.Msg
		}
		plan := func(operation *v1.PlanDockerMaintenanceRequest) *v1.Change {
			t.Helper()
			operation.ServerIds = serverIDs
			ch, err := client.PlanDockerMaintenance(t.Context(), connect.NewRequest(operation))
			if err != nil {
				t.Fatal(err)
			}
			return ch.Msg.GetChange()
		}
		apply := func(ch *v1.Change) *v1.Change {
			t.Helper()
			req := &v1.ApplyChangeRequest{Id: ch.GetId(), Fingerprint: ch.GetPlan().GetFingerprint()}
			if _, err := changes.ApplyChange(t.Context(), connect.NewRequest(req)); reasonOf(err) != authapi.ReasonNeedsStepUp {
				t.Fatalf("maintenance approval did not require a passkey: %v", err)
			}
			watch := watchWithin(t, changes, ch.GetId(), 15*time.Second)
			req.Proof = stepUpFor(t, b, v1.StepUpAction_STEP_UP_ACTION_APPROVE_CHANGE, ch.GetId()+":"+req.GetFingerprint())
			if _, err := changes.ApplyChange(t.Context(), connect.NewRequest(req)); err != nil {
				t.Fatal(err)
			}
			return waitWatchWithin(t, watch, 15*time.Second).last()
		}

		pin, err := snapshot.CloudflaredImage()
		if err != nil {
			t.Fatal(err)
		}
		_, digest, ok := strings.Cut(pin, "@sha256:")
		if !ok || len(digest) != 64 {
			t.Fatalf("catalog cloudflared image is not digest-pinned: %q", pin)
		}
		mirror := "fixture/tunnel-mirror@sha256:" + digest
		engine.AddLocalImage(mirror, dockertest.Image{Size: 1024})
		mirrored, err := engine.Client().InspectImage(t.Context(), mirror)
		if err != nil {
			t.Fatal(err)
		}
		now := time.Now().UTC().Add(-time.Hour)
		idle := docker.DiskUsageCache{ID: strings.Repeat("a", 25), CreatedAt: now, LastUsedAt: &now, Size: 2048}
		engine.ReportExtraCacheID = strings.Repeat("b", 25)
		engine.SetBuildCache(idle)
		p := preview()
		if !slices.ContainsFunc(p.GetExclusions(), func(row *v1.DockerCleanupTarget) bool {
			return row.GetEngineId() == mirrored.ID && row.GetEvidence() == ""
		}) || len(p.GetCandidates()) != 1 || p.GetCandidates()[0].GetEngineId() != idle.ID {
			t.Fatalf("catalog pin was not protected as a mirrored digest: %v", p)
		}
		failed := apply(plan(&v1.PlanDockerMaintenanceRequest{
			ExpectedSnapshot: p.GetSnapshot(),
			Operation: &v1.PlanDockerMaintenanceRequest_Cleanup{Cleanup: &v1.DockerCleanupSelection{
				Filters: &v1.DockerCleanupFilters{TaggedImages: true}, Targets: p.GetCandidates(),
			}},
		}))
		if failed.GetState() != v1.ChangeState_CHANGE_STATE_FAILED {
			t.Fatalf("collateral removal did not fail the plan: %v", failed)
		}
		pauses := preview().GetPauses()
		if len(pauses) != 1 || pauses[0].GetReason() != v1.DockerReceiptReason_DOCKER_RECEIPT_REASON_EXTRA_CACHE_REMOVED {
			t.Fatalf("missing cache incident: %v", pauses)
		}
		resumed := apply(plan(&v1.PlanDockerMaintenanceRequest{
			Operation: &v1.PlanDockerMaintenanceRequest_ResumeCleanupToken{ResumeCleanupToken: pauses[0].GetToken()},
		}))
		if resumed.GetState() != v1.ChangeState_CHANGE_STATE_APPLIED || len(preview().GetPauses()) != 0 {
			t.Fatalf("passkey-approved recovery did not clear the pause: %v", resumed)
		}
	})
}
