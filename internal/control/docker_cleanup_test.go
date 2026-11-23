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

func dockerCleanupMeasured(t *testing.T, client dockwellv1connect.DockerServiceClient, request *v1.PreviewDockerCleanupRequest) *v1.PreviewDockerCleanupResponse {
	t.Helper()
	var preview *v1.PreviewDockerCleanupResponse
	eventually(t, "the Docker cleanup count is ready", func() bool {
		res, err := client.PreviewDockerCleanup(t.Context(), connect.NewRequest(request))
		if err != nil {
			t.Fatal(err)
		}
		preview = res.Msg
		return !slices.ContainsFunc(preview.GetUnavailable(), func(row *v1.DockerUnavailable) bool {
			return row.GetState() == v1.DockerReadState_DOCKER_READ_STATE_UNSPECIFIED
		})
	})
	return preview
}

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
			return dockerCleanupMeasured(t, dockerRPC, &v1.PreviewDockerCleanupRequest{ServerIds: []string{server}})
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
			return dockerCleanupMeasured(t, client, &v1.PreviewDockerCleanupRequest{
				ServerIds: serverIDs, Filters: &v1.DockerCleanupFilters{TaggedImages: true},
			})
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

func TestDockerCleanupVolumesAndLargeBatchEndToEnd(t *testing.T) {
	t.Parallel()
	storetest.Each(t, func(t *testing.T, backend store.Backend) {
		f := startFull(t, backend, nil)
		b := signedIn(t, f)
		engine := f.addDockerServer(t)
		server := f.lastDocker.id.ServerID
		anonVol := strings.Repeat("b", 64)
		old := time.Now().UTC().Add(-5 * time.Minute).Format(time.RFC3339)
		engine.AddVolume(docker.Volume{
			Name: anonVol, Driver: "local", Scope: "local", CreatedAt: old,
			Labels: map[string]string{"com.docker.volume.anonymous": ""},
		})
		engine.AddVolume(docker.Volume{Name: "named-unused", Driver: "local", Scope: "local", CreatedAt: old})
		engine.AddVolume(docker.Volume{Name: "stopped-vol", Driver: "local", Scope: "local", CreatedAt: old})
		engine.VolumeSizes = map[string]int64{anonVol: 2 << 20, "named-unused": 5 << 20}
		engine.AddContainer(docker.Container{
			Name:   "/exited-vol-holder",
			State:  docker.State{Status: "exited"},
			Mounts: []docker.MountPoint{{Type: "volume", Name: "stopped-vol", Destination: "/data"}},
		})
		// Add 25 dangling images to verify removing more than the old 12-item cap in one plan.
		for i := range 25 {
			engine.AddLocalImage("", dockertest.Image{Size: int64(4096 + i)})
		}

		dockerRPC := dockwellv1connect.NewDockerServiceClient(b, b.origin, connect.WithProtoJSON())
		changeRPC := changeClient(b, b.origin)
		filters := &v1.DockerCleanupFilters{Volumes: true}
		p := dockerCleanupMeasured(t, dockerRPC, &v1.PreviewDockerCleanupRequest{
			ServerIds: []string{server}, Filters: filters, PageSize: 200,
		})
		if p.GetMaxTargetsTotal() != 500 || len(p.GetServerLimits()) != 1 || p.GetServerLimits()[0].GetMaxTargets() != 200 {
			t.Fatalf("unexpected server limits: total=%d limits=%v", p.GetMaxTargetsTotal(), p.GetServerLimits())
		}
		if !slices.Contains(p.GetServerLimits()[0].GetSupportedKinds(), v1.DockerCleanupKind_DOCKER_CLEANUP_KIND_VOLUME) {
			t.Fatalf("supported kinds missing VOLUME: %v", p.GetServerLimits()[0].GetSupportedKinds())
		}
		if len(p.GetCandidates()) != 27 {
			t.Fatalf("candidates = %d, want 27 (25 dangling images + 2 unused volumes)", len(p.GetCandidates()))
		}
		if !slices.ContainsFunc(p.GetExclusions(), func(row *v1.DockerCleanupTarget) bool {
			return row.GetKind() == v1.DockerCleanupKind_DOCKER_CLEANUP_KIND_VOLUME &&
				row.GetName() == "stopped-vol" &&
				row.GetReason() == v1.DockerCleanupReason_DOCKER_CLEANUP_REASON_IN_USE
		}) {
			t.Fatalf("stopped container volume was not protected: %v", p.GetExclusions())
		}
		var sawAnon, sawNamed bool
		for _, c := range p.GetCandidates() {
			if c.GetKind() == v1.DockerCleanupKind_DOCKER_CLEANUP_KIND_VOLUME {
				if c.GetName() == anonVol && c.GetReason() == v1.DockerCleanupReason_DOCKER_CLEANUP_REASON_ANONYMOUS {
					sawAnon = true
				}
				if c.GetName() == "named-unused" && c.GetReason() == v1.DockerCleanupReason_DOCKER_CLEANUP_REASON_UNUSED {
					sawNamed = true
				}
			}
		}
		if !sawAnon || !sawNamed {
			t.Fatalf("missing volume candidates anon=%t named=%t", sawAnon, sawNamed)
		}

		res, err := dockerRPC.PlanDockerMaintenance(t.Context(), connect.NewRequest(&v1.PlanDockerMaintenanceRequest{
			ServerIds: []string{server}, ExpectedSnapshot: p.GetSnapshot(),
			Operation: &v1.PlanDockerMaintenanceRequest_Cleanup{Cleanup: &v1.DockerCleanupSelection{
				Filters: filters, Targets: p.GetCandidates(),
			}},
		}))
		if err != nil {
			t.Fatalf("plan 27 targets (25 images + 2 volumes): %v", err)
		}
		ch := res.Msg.GetChange()
		reviewedEvidence := make(map[string]string)
		for _, target := range p.GetCandidates() {
			if target.GetKind() == v1.DockerCleanupKind_DOCKER_CLEANUP_KIND_VOLUME {
				reviewedEvidence[target.GetEngineId()] = target.GetEvidence()
			}
		}
		for _, step := range ch.GetPlan().GetServers()[0].GetSteps() {
			for _, target := range step.GetDockerMaintenance().GetTargets() {
				if evidence, ok := reviewedEvidence[target.GetEngineId()]; ok && target.GetEvidence() != evidence {
					t.Fatalf("plan replaced reviewed agent timestamp for %s: %q != %q", target.GetName(), target.GetEvidence(), evidence)
				}
			}
		}
		watch := watchWithin(t, changeRPC, ch.GetId(), 20*time.Second)
		req := &v1.ApplyChangeRequest{
			Id: ch.GetId(), Fingerprint: ch.GetPlan().GetFingerprint(),
			Proof: stepUpFor(t, b, v1.StepUpAction_STEP_UP_ACTION_APPROVE_CHANGE, ch.GetId()+":"+ch.GetPlan().GetFingerprint()),
		}
		if _, err := changeRPC.ApplyChange(t.Context(), connect.NewRequest(req)); err != nil {
			t.Fatal(err)
		}
		finished := waitWatchWithin(t, watch, 20*time.Second).last()
		if finished.GetState() != v1.ChangeState_CHANGE_STATE_APPLIED {
			t.Fatalf("cleanup change state = %v, err = %s", finished.GetState(), finished.GetError())
		}
		receipts := finished.GetSteps()[0].GetDockerReceipts()
		if len(receipts) != 27 {
			t.Fatalf("receipts = %d, want 27", len(receipts))
		}
		for _, r := range receipts {
			if r.GetState() != v1.DockerReceiptState_DOCKER_RECEIPT_STATE_REMOVED {
				t.Fatalf("non-removed receipt: %v", r)
			}
		}
		if engine.Volume(anonVol) != nil || engine.Volume("named-unused") != nil {
			t.Fatal("unused volumes were not removed")
		}
		if engine.Volume("stopped-vol") == nil {
			t.Fatal("volume mounted by stopped container was removed")
		}
	})
}
