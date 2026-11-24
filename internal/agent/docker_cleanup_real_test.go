// SPDX-License-Identifier: AGPL-3.0-only

package agent

import (
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"

	agentv1 "github.com/dockwell/dockwell/gen/dockwell/agent/v1"
	v1 "github.com/dockwell/dockwell/gen/dockwell/v1"
	"github.com/dockwell/dockwell/internal/docker"
	"github.com/dockwell/dockwell/internal/docker/dockertest"
	dwid "github.com/dockwell/dockwell/internal/id"
	"github.com/dockwell/dockwell/internal/resource"
	"github.com/dockwell/dockwell/internal/resource/host"
)

func TestCleanupRemovesOneBuildCacheRecordOnRealDocker(t *testing.T) {
	client := dockertest.Real(t)
	if _, err := client.Info(t.Context()); err != nil {
		t.Fatal(err)
	}
	if !client.ExactCacheRemoval() {
		t.Skip("exact cache removal requires Docker Engine Community 27.3.1, 28.5.2 or 29.4.1")
	}
	socket := os.Getenv(dockertest.EnvReal)
	if socket == "1" {
		socket = docker.DefaultSocket
	}
	repo := cleanupRealRepo(t)
	c := cleanupCalls(t, client)
	list := func() []docker.DiskUsageCache {
		t.Helper()
		rows, err := client.BuildCacheUsage(t.Context())
		if err != nil {
			t.Fatal(err)
		}
		return rows
	}
	newRecord := func(label string, before []docker.DiskUsageCache) docker.DiskUsageCache {
		t.Helper()
		ref := repo + ":" + label
		dir := t.TempDir()
		writeCleanupPayload(t, dir, ref)
		if err := os.WriteFile(filepath.Join(dir, "Dockerfile"), []byte("FROM scratch\nCOPY payload /payload\n"), 0o600); err != nil {
			t.Fatal(err)
		}
		ctx, cancel := context.WithTimeout(t.Context(), time.Minute)
		defer cancel()
		cmd := exec.CommandContext(ctx, "docker", "--host", "unix://"+socket, "build", "--tag", ref, dir)
		cmd.Env = append(os.Environ(), "DOCKER_BUILDKIT=1")
		if out, err := cmd.CombinedOutput(); err != nil {
			t.Fatalf("build %s: %v\n%s", ref, err, out)
		}
		image, err := client.InspectImage(ctx, ref)
		if err != nil {
			t.Fatal(err)
		}
		cleanupRealImage(t, client, image.ID)
		if err := client.RemoveReviewedImage(ctx, image.ID); err != nil {
			t.Fatal(err)
		}
		var candidate docker.DiskUsageCache
		for _, row := range list() {
			if row.Type != "regular" || row.LastUsedAt == nil || cacheCleanupReason(row, true) != reasonIdle ||
				slices.ContainsFunc(before, func(old docker.DiskUsageCache) bool { return old.ID == row.ID }) {
				continue
			}
			record := row
			t.Cleanup(func() {
				ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
				defer cancel()
				if _, err := client.RemoveBuildCache(ctx, record.ID, record.LastUsedAt); err != nil {
					t.Errorf("clean up test cache record %s: %v", record.ID, err)
				}
			})
			candidate = row
		}
		if candidate.ID == "" {
			t.Fatalf("build %s created no idle private regular cache record", ref)
		}
		return candidate
	}
	first := newRecord("first", list())
	second := newRecord("second", list())
	before := list()
	var target *v1.DockerCleanupTarget
	for _, row := range cleanupCandidates(t, c, nil) {
		if row.GetEngineId() == first.ID {
			target = row
		}
	}
	if target == nil {
		t.Fatalf("preview did not offer cache record %s", first.ID)
	}
	receipts, err := applyCleanupFixture(t, c, []*v1.DockerCleanupTarget{target}, nil)
	if err != nil || len(receipts) != 1 || receipts[0].GetState() != receiptRemoved || c.cleanupDisabled() {
		t.Fatalf("exact cache cleanup: %v, %v", receipts, err)
	}
	after := list()
	for _, row := range before {
		present := slices.ContainsFunc(after, func(remaining docker.DiskUsageCache) bool { return row.ID == remaining.ID })
		if present != (row.ID != first.ID) {
			t.Errorf("cache record %s: presence after pruning %s = %t", row.ID, first.ID, present)
		}
	}
	if !slices.ContainsFunc(after, func(row docker.DiskUsageCache) bool { return row.ID == second.ID }) {
		t.Fatalf("second build's cache record %s was removed", second.ID)
	}
}

func TestCleanupRemovesADanglingImageByIDOnRealDocker(t *testing.T) {
	client := dockertest.Real(t)
	c := cleanupCalls(t, client)
	tag := cleanupRealRepo(t) + ":test"
	dangling := importCleanupImage(t, client, tag, "old")
	replacement := importCleanupImage(t, client, tag, "replacement")
	current, err := client.InspectImage(t.Context(), dangling)
	if err != nil || len(current.RepoTags) != 0 || len(current.RepoDigests) != 0 {
		t.Fatalf("fixture image is not dangling: %v, %v", current, err)
	}
	var target *v1.DockerCleanupTarget
	for _, row := range cleanupCandidates(t, c, nil) {
		if row.GetEngineId() == dangling && row.GetReason() == reasonDangling {
			target = row
		}
	}
	if target == nil {
		t.Fatalf("preview did not offer dangling image %s", dangling)
	}
	receipts, err := applyCleanupFixture(t, c, []*v1.DockerCleanupTarget{target}, &agentv1.DockerCleanupProtection{})
	if err != nil || len(receipts) != 1 || receipts[0].GetState() != receiptRemoved || receipts[0].GetRecheckedEngineId() != dangling {
		t.Fatalf("dangling image cleanup: %v, %v", receipts, err)
	}
	if _, err := client.InspectImage(t.Context(), dangling); !docker.IsNotFound(err) {
		t.Fatalf("dangling image wasn't removed: %v", err)
	}
	if kept, err := client.InspectImage(t.Context(), tag); err != nil || kept.ID != replacement {
		t.Fatalf("replacement tag changed: %v, %v", kept, err)
	}
}

func TestCleanupRemovesPinnedImageOnRealDocker(t *testing.T) {
	client := dockertest.Real(t)
	ctx, cancel := context.WithTimeout(t.Context(), 2*time.Minute)
	defer cancel()
	const (
		digest     = "sha256:73aaf090f3d85aa34ee199857f03fa3a95c8ede2ffd4cc2cdb5b94e566b11662"
		pinnedRef  = "busybox:1.36@" + digest
		wantDigest = "busybox@" + digest
	)
	preImages, err := client.InventoryImages(ctx)
	if err != nil {
		t.Fatalf("list images before pinned pull: %v", err)
	}
	preIDs := make(map[string]bool, len(preImages))
	for _, row := range preImages {
		preIDs[row.ID] = true
		if row.ID == digest || slices.Contains(row.RepoTags, pinnedRef) ||
			slices.ContainsFunc(row.RepoDigests, func(ref string) bool { return strings.HasSuffix(ref, "@"+digest) }) {
			t.Skipf("pinned test image %s is already present on this Docker daemon (%s); skipping so the test never removes a pre-existing image", pinnedRef, row.ID)
		}
	}
	if existing, err := client.InspectImage(ctx, pinnedRef); err == nil {
		t.Skipf("pinned test image %s is already present on this Docker daemon (%s); skipping so the test never removes a pre-existing image", pinnedRef, existing.ID)
	}
	if existing, err := client.InspectImage(ctx, wantDigest); err == nil {
		t.Skipf("pinned test image %s is already present on this Docker daemon (%s); skipping so the test never removes a pre-existing image", wantDigest, existing.ID)
	}

	if err := client.PullImage(ctx, pinnedRef, nil); err != nil {
		t.Fatalf("pull pinned image %s: %v", pinnedRef, err)
	}
	info, err := client.Info(ctx)
	if err != nil {
		t.Fatalf("read engine info: %v", err)
	}
	containerd := info.ContainerdImageStore()
	pulled, err := client.InspectImage(ctx, pinnedRef)
	if err != nil {
		t.Fatalf("inspect pinned image %s: %v", pinnedRef, err)
	}
	if preIDs[pulled.ID] {
		// On a classic store, a pre-existing dangling image can share the config ID.
		// Never delete an image ID that existed before the pull; untag only our digest ref and skip.
		_ = client.UntagImage(ctx, wantDigest)
		t.Skipf("image ID %s was already present on this Docker daemon before pulling %s; skipping so the test never removes a pre-existing image", pulled.ID, pinnedRef)
	}
	if !docker.IsImageID(pulled.ID) {
		t.Fatalf("pulled image ID %q is not a full Docker image ID", pulled.ID)
	}
	pulledID := pulled.ID
	t.Cleanup(func() {
		cleanCtx, cleanCancel := context.WithTimeout(context.Background(), 30*time.Second)
		defer cleanCancel()
		if err := client.RemoveReviewedImage(cleanCtx, pulledID); err != nil && !docker.IsNotFound(err) {
			t.Errorf("clean up pinned image %s: %v", pulledID, err)
		}
	})

	df, err := client.CleanupDiskUsage(ctx)
	if err != nil {
		t.Fatalf("disk usage after pinned pull: %v", err)
	}
	var dfRow *docker.DiskUsageImage
	for i := range df.Images {
		if df.Images[i].ID == pulled.ID {
			dfRow = &df.Images[i]
			break
		}
	}
	if dfRow == nil {
		t.Fatalf("/system/df did not include pulled pinned image %s", pulled.ID)
	}
	listed, err := client.InventoryImages(ctx)
	if err != nil {
		t.Fatalf("list images after pinned pull: %v", err)
	}
	var listRow *docker.InventoryImage
	for i := range listed {
		if listed[i].ID == pulled.ID {
			listRow = &listed[i]
			break
		}
	}
	if listRow == nil {
		t.Fatalf("/images/json did not include pulled pinned image %s", pulled.ID)
	}
	if containerd {
		if pulled.ID != digest {
			t.Fatalf("containerd pinned image ID = %q, want %q", pulled.ID, digest)
		}
		if !slices.Contains(dfRow.RepoTags, pinnedRef) || !slices.Contains(listRow.RepoTags, pinnedRef) || !slices.Contains(pulled.RepoTags, pinnedRef) {
			t.Fatalf("containerd pinned record %q missing from RepoTags (df=%v list=%v inspect=%v)", pinnedRef, dfRow.RepoTags, listRow.RepoTags, pulled.RepoTags)
		}
		if !slices.Contains(dfRow.RepoDigests, wantDigest) || !slices.Contains(listRow.RepoDigests, wantDigest) {
			t.Fatalf("containerd /system/df or /images/json RepoDigests missing %q (df=%v list=%v)", wantDigest, dfRow.RepoDigests, listRow.RepoDigests)
		}
		if !slices.Contains(pulled.RepoDigests, pinnedRef) || slices.Equal(dfRow.RepoDigests, pulled.RepoDigests) {
			t.Fatalf("containerd inspect RepoDigests did not show expected endpoint divergence (df=%v inspect=%v)", dfRow.RepoDigests, pulled.RepoDigests)
		}
	} else {
		if slices.Contains(pulled.RepoTags, pinnedRef) || slices.Contains(dfRow.RepoTags, pinnedRef) {
			t.Fatalf("classic store unexpectedly put pinned ref %q in RepoTags (df=%v inspect=%v)", pinnedRef, dfRow.RepoTags, pulled.RepoTags)
		}
		if !slices.Contains(dfRow.RepoDigests, wantDigest) || !slices.Contains(listRow.RepoDigests, wantDigest) || !slices.Contains(pulled.RepoDigests, wantDigest) {
			t.Fatalf("classic store RepoDigests missing %q (df=%v list=%v inspect=%v)", wantDigest, dfRow.RepoDigests, listRow.RepoDigests, pulled.RepoDigests)
		}
	}

	c := cleanupCalls(t, client)
	var target *v1.DockerCleanupTarget
	for _, row := range cleanupCandidates(t, c, &v1.DockerCleanupFilters{TaggedImages: true}) {
		if row.GetKind() == cleanupImage && row.GetEngineId() == pulled.ID {
			target = row
		}
	}
	if target == nil {
		t.Fatalf("preview did not offer pinned image %s (%s)", pinnedRef, pulled.ID)
	}
	if target.GetReason() != reasonUnused || len(target.GetImageTags()) != 0 ||
		!slices.Equal(target.GetImageDigests(), []string{wantDigest}) {
		t.Fatalf("pinned image target = %v, want UNUSED with digest %s and no movable tags", target, wantDigest)
	}
	if containerd {
		if target.GetName() != "busybox:1.36" || !slices.Equal(target.GetImageRecords(), []string{pinnedRef}) {
			t.Fatalf("containerd pinned image target = name=%q records=%v, want busybox:1.36 and [%s]", target.GetName(), target.GetImageRecords(), pinnedRef)
		}
	} else {
		if target.GetName() != pulled.ID || len(target.GetImageRecords()) != 0 {
			t.Fatalf("classic pinned image target = name=%q records=%v, want %q and empty records", target.GetName(), target.GetImageRecords(), pulled.ID)
		}
	}

	receipts, err := applyCleanupFixture(t, c, []*v1.DockerCleanupTarget{target}, &agentv1.DockerCleanupProtection{})
	if err != nil || len(receipts) != 1 || receipts[0].GetState() != receiptRemoved || receipts[0].GetRecheckedEngineId() != pulled.ID {
		t.Fatalf("pinned image cleanup: %v, %v", receipts, err)
	}
	if _, err := client.InspectImage(ctx, pulled.ID); !docker.IsNotFound(err) {
		t.Fatalf("pinned image wasn't removed: %v", err)
	}
}

func TestCleanupKeepsAnImageWithTwoTagsOnRealDocker(t *testing.T) {
	client := dockertest.Real(t)
	c := cleanupCalls(t, client)
	repo := cleanupRealRepo(t)
	tags := []string{repo + ":first", repo + ":second"}
	image := importCleanupImage(t, client, tags[0], "two tags")
	if err := client.TagImage(t.Context(), image, tags[1]); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
		defer cancel()
		if err := client.RemoveImage(ctx, tags[1]); err != nil && !docker.IsNotFound(err) {
			t.Errorf("clean up test tag: %v", err)
		}
	})
	assertCleanupMultiTagProtected(t, c, image)
	for _, tag := range tags {
		if kept, err := client.InspectImage(t.Context(), tag); err != nil || kept.ID != image {
			t.Fatalf("test tag %s changed: %v, %v", tag, kept, err)
		}
	}
}

func TestCleanupRemovesUnusedVolumesAndManyImagesOnRealDocker(t *testing.T) {
	client := dockertest.Real(t)
	ctx := t.Context()
	c := cleanupCalls(t, client)
	repo := cleanupRealRepo(t)

	// Create 20 dangling images (well above the old 12-item ceiling) by replacing a single tag.
	dangling := make([]string, 0, 20)
	tag := repo + ":batch"
	for i := range 20 {
		dangling = append(dangling, importCleanupImage(t, client, tag, strings.Repeat("b", i+1)))
	}
	// One final image keeps the tag so all 20 prior images are dangling.
	replacement := importCleanupImage(t, client, tag, "keep-tag")

	// Create a genuine Moby anonymous volume (empty name -> lowercase 64-hex + anonymous marker)
	// and a named unused volume.
	createdAnon, err := client.CreateVolume(ctx, &docker.VolumeCreate{Driver: "local"})
	if err != nil {
		t.Fatal(err)
	}
	anonVol := createdAnon.Name
	namedVol := "cleanup-named-" + strings.ToLower(dockertest.Scope(t))
	t.Cleanup(func() {
		cleanCtx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
		defer cancel()
		if err := client.RemoveVolume(cleanCtx, anonVol); err != nil && !docker.IsNotFound(err) {
			t.Errorf("clean up anonymous volume %s: %v", anonVol, err)
		}
	})
	if _, err := client.CreateVolume(ctx, &docker.VolumeCreate{Name: namedVol, Driver: "local"}); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		cleanCtx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
		defer cancel()
		if err := client.RemoveVolume(cleanCtx, namedVol); err != nil && !docker.IsNotFound(err) {
			t.Errorf("clean up named volume %s: %v", namedVol, err)
		}
	})

	// Freshly created volumes (<60s old) stay protected until minimum age passes.
	freshPreview, err := c.dockerCleanupPreview(ctx, &agentv1.DockerMaintenancePreviewCall{
		Fresh: true, Filters: &v1.DockerCleanupFilters{Volumes: true, Kinds: []v1.DockerCleanupKind{cleanupVolume}},
	})
	if err != nil {
		t.Fatal(err)
	}
	for _, row := range freshPreview.GetTargets() {
		if row.GetName() == anonVol || row.GetName() == namedVol {
			if row.GetEvidence() != "" || row.GetReason() != reasonRecentlyCreated {
				t.Fatalf("fresh volume %s should be protected as RECENTLY_CREATED, got %v", row.GetName(), row)
			}
		}
	}

	// Advance the cleanup clock past the 60s minimum age to verify eligibility and removal.
	agedNow := time.Now().Add(2 * time.Minute)
	c.cfg.disks.now = func() time.Time { return agedNow }

	// Default filter (Volumes=false) keeps named volumes out of candidates.
	for _, row := range cleanupCandidates(t, c, nil) {
		if row.GetKind() == cleanupVolume && row.GetName() == namedVol {
			t.Fatalf("named volume was eligible without volume filter: %v", row)
		}
	}

	candidates := cleanupCandidates(t, c, &v1.DockerCleanupFilters{Volumes: true})
	var selected []*v1.DockerCleanupTarget
	var sawAnon, sawNamed bool
	danglingSet := map[string]bool{}
	for _, id := range dangling {
		danglingSet[id] = true
	}
	for _, row := range candidates {
		switch {
		case row.GetKind() == cleanupImage && danglingSet[row.GetEngineId()] && row.GetReason() == reasonDangling:
			selected = append(selected, row)
		case row.GetKind() == cleanupVolume && row.GetName() == anonVol:
			if row.GetReason() != reasonAnonymous {
				t.Fatalf("anonymous volume reason = %v, want ANONYMOUS", row.GetReason())
			}
			sawAnon = true
			selected = append(selected, row)
		case row.GetKind() == cleanupVolume && row.GetName() == namedVol:
			if row.GetReason() != reasonUnused {
				t.Fatalf("named volume reason = %v, want UNUSED (opt-in, not safe default)", row.GetReason())
			}
			sawNamed = true
			selected = append(selected, row)
		}
	}
	if !sawAnon || !sawNamed || len(selected) != 22 {
		t.Fatalf("selected %d targets (anon=%t named=%t), want 22", len(selected), sawAnon, sawNamed)
	}

	for _, target := range selected {
		target.ServerId = "srv_cleanup"
	}
	plan := &v1.DockerMaintenance{ChangeId: dwid.At(dwid.Change, agedNow, "real-docker-cleanup"), Targets: selected}
	var receipts []*v1.DockerReceipt
	outCtx := resource.WithOutcome(ctx, func(out resource.Outcome) { receipts = out.DockerReceipts })
	err = c.applyDockerMaintenance(outCtx, resource.Ref{Kind: host.Kind, Name: host.Name, Scope: "srv_cleanup"}, plan, &agentv1.DockerCleanupProtection{})
	if err != nil || len(receipts) != 22 {
		t.Fatalf("apply 22 targets: receipts=%d err=%v", len(receipts), err)
	}
	for _, r := range receipts {
		if r.GetState() != receiptRemoved {
			t.Fatalf("target %s not removed: %v", r.GetTarget().GetEngineId(), r)
		}
	}
	for _, id := range dangling {
		if _, err := client.InspectImage(ctx, id); !docker.IsNotFound(err) {
			t.Fatalf("dangling image %s still exists: %v", id, err)
		}
	}
	if kept, err := client.InspectImage(ctx, tag); err != nil || kept.ID != replacement {
		t.Fatalf("replacement image changed: %v, %v", kept, err)
	}
	for _, vol := range []string{anonVol, namedVol} {
		if _, err := client.InspectVolume(ctx, vol); !docker.IsNotFound(err) {
			t.Fatalf("volume %s still exists: %v", vol, err)
		}
	}
}

func TestCleanupKeepsStoppedContainerVolumeOnRealDocker(t *testing.T) {
	client := dockertest.Real(t)
	ctx := t.Context()
	c := cleanupCalls(t, client)
	tag := cleanupRealRepo(t) + ":stopped-vol"
	_ = importCleanupImage(t, client, tag, "stopped-vol")

	volName := "cleanup-stopped-" + strings.ToLower(dockertest.Scope(t))
	if _, err := client.CreateVolume(ctx, &docker.VolumeCreate{Name: volName, Driver: "local"}); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		cleanCtx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
		defer cancel()
		if err := client.RemoveVolume(cleanCtx, volName); err != nil && !docker.IsNotFound(err) {
			t.Errorf("clean up volume %s: %v", volName, err)
		}
	})

	ctrName := "cleanup-stopped-ctr-" + strings.ToLower(dockertest.Scope(t))
	ctrID, err := client.CreateContainer(ctx, ctrName, &docker.CreateRequest{
		Config: docker.Config{
			Image: tag,
			Cmd:   []string{"/payload"},
		},
		HostConfig: docker.HostConfig{
			Mounts: []docker.Mount{{Type: "volume", Source: volName, Target: "/data"}},
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		cleanCtx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
		defer cancel()
		if err := client.RemoveContainer(cleanCtx, ctrID, false); err != nil && !docker.IsNotFound(err) {
			t.Errorf("clean up container %s: %v", ctrID, err)
		}
	})

	preview, err := c.dockerCleanupPreview(ctx, &agentv1.DockerMaintenancePreviewCall{
		Fresh:   true,
		Filters: &v1.DockerCleanupFilters{Volumes: true, Kinds: []v1.DockerCleanupKind{cleanupVolume}},
	})
	if err != nil {
		t.Fatal(err)
	}
	var found *v1.DockerCleanupTarget
	for _, row := range preview.GetTargets() {
		if row.GetKind() == cleanupVolume && row.GetName() == volName {
			found = row
		}
	}
	if found == nil || found.GetEvidence() != "" || found.GetReason() != reasonInUse {
		t.Fatalf("stopped container volume = %v, want protected IN_USE", found)
	}
	if _, err := client.InspectVolume(ctx, volName); err != nil {
		t.Fatalf("stopped container volume missing: %v", err)
	}
}

func TestCleanupProtectsComposeAndCopiedMarkerVolumesOnRealDocker(t *testing.T) {
	client := dockertest.Real(t)
	ctx := t.Context()
	c := cleanupCalls(t, client)
	scope := strings.ToLower(dockertest.Scope(t))
	composeProj := "cleanupproj-" + scope
	defaultComposeVol := composeProj + "_db"
	customComposeVol := "custom-db-" + scope
	copiedMarkerVol := "copied-anon-" + scope

	removeVol := func(name string) {
		t.Cleanup(func() {
			cleanCtx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
			defer cancel()
			if err := client.RemoveVolume(cleanCtx, name); err != nil && !docker.IsNotFound(err) {
				t.Errorf("clean up volume %s: %v", name, err)
			}
		})
	}
	for _, spec := range []docker.VolumeCreate{
		{Name: defaultComposeVol, Driver: "local", Labels: map[string]string{docker.LabelProject: composeProj, docker.LabelVolume: "db"}},
		{Name: customComposeVol, Driver: "local", Labels: map[string]string{docker.LabelProject: composeProj, docker.LabelVolume: "db"}},
		{Name: copiedMarkerVol, Driver: "local", Labels: map[string]string{"com.docker.volume.anonymous": ""}},
	} {
		if _, err := client.CreateVolume(ctx, &spec); err != nil {
			t.Fatal(err)
		}
		removeVol(spec.Name)
	}

	agedNow := time.Now().Add(2 * time.Minute)
	c.cfg.disks.now = func() time.Time { return agedNow }
	preview, err := c.dockerCleanupPreview(ctx, &agentv1.DockerMaintenancePreviewCall{
		Fresh: true, Filters: &v1.DockerCleanupFilters{Volumes: true, Kinds: []v1.DockerCleanupKind{cleanupVolume}},
	})
	if err != nil {
		t.Fatal(err)
	}
	byName := map[string]*v1.DockerCleanupTarget{}
	for _, row := range preview.GetTargets() {
		byName[row.GetName()] = row
	}
	for _, name := range []string{defaultComposeVol, customComposeVol} {
		row := byName[name]
		if row == nil || row.GetEvidence() != "" || row.GetReason() != reasonCompose || row.GetComposeProject() != composeProj {
			t.Fatalf("Compose volume %s = %v, want protected COMPOSE project=%s", name, row, composeProj)
		}
	}
	if copied := byName[copiedMarkerVol]; copied == nil || copied.GetEvidence() == "" || copied.GetReason() != reasonUnused {
		t.Fatalf("hand-named volume with copied marker = %v, want opt-in named UNUSED (never ANONYMOUS)", copied)
	}
}

func cleanupRealRepo(t *testing.T) string {
	t.Helper()
	return "fixture/cleanup-" + strings.ToLower(dockertest.Scope(t))
}

func importCleanupImage(t *testing.T, client *docker.Client, tag, contents string) string {
	t.Helper()
	file := writeCleanupPayload(t, t.TempDir(), tag+contents)
	if err := client.ImportFiles(t.Context(), tag, []docker.ImageFile{{Name: "/payload", Path: file, Mode: 0o600}}); err != nil {
		t.Fatal(err)
	}
	image, err := client.InspectImage(t.Context(), tag)
	if err != nil {
		t.Fatal(err)
	}
	cleanupRealImage(t, client, image.ID)
	return image.ID
}

func writeCleanupPayload(t *testing.T, dir, contents string) string {
	t.Helper()
	file := filepath.Join(dir, "payload")
	if err := os.WriteFile(file, []byte(contents), 0o600); err != nil {
		t.Fatal(err)
	}
	return file
}

func cleanupRealImage(t *testing.T, client *docker.Client, imageID string) {
	t.Helper()
	t.Cleanup(func() {
		ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
		defer cancel()
		if err := client.RemoveReviewedImage(ctx, imageID); err != nil && !docker.IsNotFound(err) {
			t.Errorf("clean up test image %s: %v", imageID, err)
		}
	})
}
