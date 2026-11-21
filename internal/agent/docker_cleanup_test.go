// SPDX-License-Identifier: AGPL-3.0-only

package agent

import (
	"errors"
	"net/http"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"google.golang.org/protobuf/proto"

	agentv1 "github.com/dockwell/dockwell/gen/dockwell/agent/v1"
	v1 "github.com/dockwell/dockwell/gen/dockwell/v1"
	"github.com/dockwell/dockwell/internal/agent/collect"
	"github.com/dockwell/dockwell/internal/agent/enroll"
	"github.com/dockwell/dockwell/internal/build"
	"github.com/dockwell/dockwell/internal/catalog/snapshot"
	"github.com/dockwell/dockwell/internal/docker"
	"github.com/dockwell/dockwell/internal/docker/dockertest"
	dwid "github.com/dockwell/dockwell/internal/id"
	"github.com/dockwell/dockwell/internal/ingress/caddy"
	"github.com/dockwell/dockwell/internal/resource"
	"github.com/dockwell/dockwell/internal/resource/host"
	"github.com/dockwell/dockwell/internal/resource/service"
	"github.com/dockwell/dockwell/internal/website"
)

func TestDockerCleanupRecoversWithoutRepeatingPendingRemoval(t *testing.T) {
	engine := dockertest.New(t)
	client := engine.Client()
	now := time.Now().UTC().Truncate(time.Second).Add(-time.Hour)
	idle := docker.DiskUsageCache{ID: strings.Repeat("k", 25), CreatedAt: now, LastUsedAt: &now, Size: 2048}
	engine.SetBuildCache(idle)
	dir := t.TempDir()
	c := newCalls(t.Context(), Config{
		Dir: dir, Identity: &enroll.Identity{ServerID: "srv_cleanup"}, Docker: client,
		disks: &diskUse{docker: client, collector: collect.New(), now: time.Now},
	}, nil, nil)
	t.Cleanup(c.end)
	answer, err := c.dockerCleanupPreview(t.Context(), &agentv1.DockerMaintenancePreviewCall{Fresh: true})
	if err != nil {
		t.Fatal(err)
	}
	var target *v1.DockerCleanupTarget
	for _, row := range answer.GetTargets() {
		if row.GetEngineId() == idle.ID {
			target = row
		}
	}
	if target.GetEvidence() == "" {
		t.Fatalf("idle fixture was not eligible: %v", answer)
	}
	target.ServerId = "srv_cleanup"
	plan := &v1.DockerMaintenance{ChangeId: dwid.New(dwid.Change), Targets: []*v1.DockerCleanupTarget{target}}
	receiptDir := filepath.Join(dir, "docker-cleanup")
	if err := os.MkdirAll(receiptDir, 0o700); err != nil {
		t.Fatal(err)
	}
	file := filepath.Join(receiptDir, cleanupEvidence(plan.GetChangeId())+".json")
	pending := &v1.DockerReceipt{Target: target, State: receiptUnknown}
	if err := saveDockerReceipts(file, []*v1.DockerReceipt{pending}); err != nil {
		t.Fatal(err)
	}
	// The target was already removed, but no response reached the agent.
	engine.SetBuildCache()
	var receipt *v1.DockerReceipt
	ctx := resource.WithOutcome(t.Context(), func(out resource.Outcome) {
		if len(out.DockerReceipts) > 0 {
			receipt = out.DockerReceipts[0]
		}
	})
	err = c.applyDockerMaintenance(ctx, resource.Ref{Kind: host.Kind, Name: host.Name, Scope: target.ServerId}, plan, nil)
	if err == nil || receipt.GetState() != receiptUnknown || receipt.AfterBytes == nil || receipt.GetAfterBytes() != 0 {
		t.Fatalf("interrupted cleanup: %v, %v", receipt, err)
	}
	for _, request := range engine.Seen() {
		if strings.HasSuffix(request, "/build/prune") {
			t.Fatalf("uncertain removal was retried: %s", request)
		}
	}
}

func TestDockerImageDetailSendsSafeHistoryAndOrderedLayerSizes(t *testing.T) {
	engine := dockertest.New(t)
	client := engine.Client()
	layer := "sha256:" + strings.Repeat("a", 64)
	engine.AddLocalImage("example/image:1", dockertest.Image{Size: 1024, Created: time.Now().UTC(), Layer: layer})
	stored, err := client.InspectImage(t.Context(), "example/image:1")
	if err != nil {
		t.Fatal(err)
	}
	c := &calls{cfg: Config{Docker: client, disks: &diskUse{docker: client, collector: collect.New(), now: time.Now}}}
	result, err := c.dockerImageDetail(t.Context(), stored.ID)
	if err != nil {
		t.Fatal(err)
	}
	image := result.GetObject().GetImage()
	if image.GetPlatform() != "linux/amd64" || len(image.GetLayers()) != 1 ||
		image.GetLayers()[0].GetDiffId() != layer || image.GetLayers()[0].GetSizeBytes() != 1024 || len(image.GetHistory()) != 1 {
		t.Fatalf("missing typed local image information: %v", image)
	}
	data, err := proto.Marshal(result)
	if err != nil || strings.Contains(string(data), "fixture-history") {
		t.Fatalf("image history exposed raw text or did not serialize: %v", err)
	}
}

func TestDockerCleanupRemovesOnlyReviewedArtifacts(t *testing.T) {
	c, engine := cleanupFixture(t)
	client, ctx := engine.Client(), t.Context()
	engine.AddLocalImage("", dockertest.Image{Size: 11})
	engine.AddLocalImage("fixture/app:1", dockertest.Image{Size: 22})
	tagged, err := client.InspectImage(ctx, "fixture/app:1")
	if err != nil {
		t.Fatal(err)
	}
	network, err := client.CreateNetwork(ctx, &docker.NetworkCreate{Name: "fixture-network"})
	if err != nil {
		t.Fatal(err)
	}
	engine.AddLocalImage("dockwell-build:local", dockertest.Image{Size: 44})
	engine.AddLocalImage("", dockertest.Image{Size: 55, Config: docker.Config{Labels: map[string]string{docker.LabelBuilder: "true"}}})
	engine.AddVolume(docker.Volume{Name: "fixture-backup"})
	defaults := cleanupCandidates(t, c, nil)
	if len(defaults) != 1 || defaults[0].GetReason() != reasonDangling {
		t.Fatalf("default cleanup candidates: %v", defaults)
	}
	selected := cleanupCandidates(t, c, &v1.DockerCleanupFilters{TaggedImages: true, Networks: true})
	if len(selected) != 3 {
		t.Fatalf("opt-in cleanup candidates: %v", selected)
	}
	before := len(engine.Seen())
	receipts, err := applyCleanupFixture(t, c, selected, &agentv1.DockerCleanupProtection{})
	if err != nil || len(receipts) != 3 {
		t.Fatalf("cleanup: %v, %v", receipts, err)
	}
	for _, receipt := range receipts {
		id := receipt.GetTarget().GetEngineId()
		if receipt.GetState() != receiptRemoved || receipt.GetRecheckedEngineId() != id {
			t.Errorf("receipt for %s: %v", id, receipt)
		}
		if receipt.GetTarget().GetKind() == cleanupImage &&
			(receipt.GetBeforeBytes() != receipt.GetTarget().GetEstimatedBytes() || receipt.AfterBytes == nil || receipt.GetAfterBytes() != 0) {
			t.Errorf("byte measurement for %s: %v", id, receipt)
		}
	}
	if _, err := client.InspectImage(ctx, tagged.ID); !docker.IsNotFound(err) {
		t.Fatalf("unused tagged image still exists: %v", err)
	}
	var counts int
	var deletes []string
	for _, request := range engine.Seen()[before:] {
		if strings.HasSuffix(request, "/system/df") {
			counts++
		}
		if strings.HasPrefix(request, "DELETE ") {
			deletes = append(deletes, request)
		}
	}
	if counts != 2 {
		t.Fatalf("cleanup made %d disk counts, want one before the plan and one after", counts)
	}
	if len(deletes) != 3 || !slices.ContainsFunc(deletes, func(req string) bool { return strings.HasSuffix(req, "/networks/"+network) }) ||
		!slices.ContainsFunc(deletes, func(req string) bool { return strings.HasSuffix(req, "/images/"+tagged.ID) }) {
		t.Fatalf("cleanup routes: %v", deletes)
	}
}

func TestDockerCleanupRefusesChangedArtifacts(t *testing.T) {
	for _, scenario := range []string{"dangling retagged", "tag moved", "network ID reassigned", "stopped image user", "stopped network user", "new rollback digest", "new desired tag", "new desired network"} {
		t.Run(scenario, func(t *testing.T) {
			c, engine := cleanupFixture(t)
			client, ctx := engine.Client(), t.Context()
			tag := ""
			if scenario == "tag moved" || scenario == "new desired tag" {
				tag = "fixture/app:1"
			}
			engine.AddLocalImage(tag, dockertest.Image{Size: 64})
			n, err := client.CreateNetwork(ctx, &docker.NetworkCreate{Name: "fixture-network"})
			if err != nil {
				t.Fatal(err)
			}
			kind := cleanupImage
			if strings.Contains(scenario, "network") {
				kind = cleanupNetwork
			}
			targets := cleanupCandidates(t, c, &v1.DockerCleanupFilters{TaggedImages: true, Networks: true})
			i := slices.IndexFunc(targets, func(row *v1.DockerCleanupTarget) bool { return row.GetKind() == kind })
			if i < 0 {
				t.Fatalf("no eligible %s: %v", kind, targets)
			}
			target := targets[i]
			id := target.GetEngineId()
			rechecked := id
			protection := &agentv1.DockerCleanupProtection{}
			switch scenario {
			case "dangling retagged":
				if err := client.TagImage(ctx, id, "fixture/app:2"); err != nil {
					t.Fatal(err)
				}
			case "tag moved":
				engine.AddLocalImage(tag, dockertest.Image{Size: 128})
				other, err := client.InspectImage(ctx, tag)
				if err != nil {
					t.Fatal(err)
				}
				rechecked = other.ID
			case "network ID reassigned":
				if err := client.RemoveNetwork(ctx, n); err != nil {
					t.Fatal(err)
				}
				// A full ID can also be the name of a different network.
				rechecked, err = client.CreateNetwork(ctx, &docker.NetworkCreate{Name: n})
				if err != nil {
					t.Fatal(err)
				}
			case "stopped image user":
				engine.AddContainer(docker.Container{Name: "/outside", Image: id, Config: docker.Config{Image: id}, State: docker.State{Status: "exited"}})
			case "stopped network user":
				engine.AddContainer(docker.Container{
					Name: "/outside", State: docker.State{Status: "exited"},
					NetworkSettings: docker.ContainerNetworkSettings{Networks: map[string]*docker.EndpointSettings{
						"fixture-network": {NetworkID: n},
					}},
				})
			case "new rollback digest":
				protection.Images = []string{id}
			case "new desired tag":
				protection.Images = []string{"docker.io/fixture/app:1"}
			case "new desired network":
				protection.Networks = []string{"fixture-network"}
			}
			before := len(engine.Seen())
			receipts, err := applyCleanupFixture(t, c, []*v1.DockerCleanupTarget{target}, protection)
			if err == nil || len(receipts) != 1 || receipts[0].GetState() != receiptChanged || receipts[0].GetRecheckedEngineId() != rechecked {
				t.Fatalf("changed target %s: %v, %v", rechecked, receipts, err)
			}
			for _, request := range engine.Seen()[before:] {
				if strings.HasPrefix(request, "DELETE ") {
					t.Errorf("changed artifact dispatched for removal: %s", request)
				}
			}
		})
	}
}

func cleanupFixture(t *testing.T) (*calls, *dockertest.Engine) {
	t.Helper()
	engine := dockertest.New(t)
	return cleanupCalls(t, engine.Client()), engine
}

func cleanupCalls(t *testing.T, client *docker.Client) *calls {
	t.Helper()
	c := newCalls(t.Context(), Config{
		Dir: t.TempDir(), Identity: &enroll.Identity{ServerID: "srv_cleanup"}, Docker: client,
		disks: &diskUse{docker: client, collector: collect.New(), now: time.Now},
	}, nil, nil)
	t.Cleanup(c.end)
	return c
}

func cleanupCandidates(t *testing.T, c *calls, filters *v1.DockerCleanupFilters) []*v1.DockerCleanupTarget {
	t.Helper()
	preview, err := c.dockerCleanupPreview(t.Context(), &agentv1.DockerMaintenancePreviewCall{Fresh: true, Filters: filters})
	if err != nil {
		t.Fatal(err)
	}
	var out []*v1.DockerCleanupTarget
	for _, row := range preview.GetTargets() {
		if row.GetEvidence() != "" {
			row.ServerId = "srv_cleanup"
			out = append(out, row)
		}
	}
	return out
}

func applyCleanupFixture(t *testing.T, c *calls, targets []*v1.DockerCleanupTarget, protection *agentv1.DockerCleanupProtection) ([]*v1.DockerReceipt, error) {
	t.Helper()
	var receipts []*v1.DockerReceipt
	ctx := resource.WithOutcome(t.Context(), func(out resource.Outcome) { receipts = out.DockerReceipts })
	err := c.applyDockerMaintenance(ctx, resource.Ref{Kind: host.Kind, Name: host.Name, Scope: "srv_cleanup"},
		&v1.DockerMaintenance{ChangeId: dwid.New(dwid.Change), Targets: targets}, protection)
	return receipts, err
}

func TestDockerCleanupKeepsInfrastructureWithoutContainers(t *testing.T) {
	cloudflared, err := snapshot.CloudflaredImage()
	if err != nil {
		t.Fatal(err)
	}
	pins := []string{build.Image, build.RailpackFrontend, caddy.Image, website.Image}
	for _, runtime := range service.Runtimes() {
		pins = append(pins, runtime.Image)
	}
	for _, runtime := range website.PHPRuntimes() {
		pins = append(pins, runtime.Image)
	}
	for _, pin := range slices.Clone(pins) {
		_, digest, _ := strings.Cut(pin, "@")
		pins = append(pins, "fixture/mirror@"+digest)
	}
	digest := "sha256:" + strings.Repeat("e", 64)
	for _, repo := range []string{
		"cloudflare/cloudflared", "ghcr.io/cloudflare/cloudflared", "moby/buildkit",
		"ghcr.io/railwayapp/railpack-frontend", docker.ProbeRepository, caddy.Repository, "dockwell-volume-tool",
	} {
		pins = append(pins, repo+"@"+digest)
	}
	ingress := "fixture/custom-ingress@sha256:" + strings.Repeat("f", 64)
	pins = append(pins, ingress, cloudflared)
	for _, pin := range slices.Compact(pins) {
		t.Run(pin, func(t *testing.T) {
			c, engine := cleanupFixture(t)
			c.cfg.DockerIngressImage = ingress
			_, manifest, _ := strings.Cut(pin, "@")
			ref := docker.Repository(pin) + "@" + manifest
			engine.AddLocalImage(ref, dockertest.Image{Size: 64})
			for _, optIn := range []bool{false, true} {
				preview, err := c.dockerCleanupPreview(t.Context(), &agentv1.DockerMaintenancePreviewCall{
					Fresh: true, Filters: &v1.DockerCleanupFilters{TaggedImages: optIn},
				})
				if err != nil || len(preview.GetTargets()) != 1 || preview.GetTargets()[0].GetReason() != reasonDesired ||
					preview.GetTargets()[0].GetEvidence() != "" {
					t.Fatalf("infrastructure image with no container must stay protected: %v, %v", preview, err)
				}
			}
			row := imageRow(nil, docker.DiskUsageImage{RepoDigests: []string{ref}})
			if row.GetImage().GetState() != v1.DockerImageState_DOCKER_IMAGE_STATE_UNUSED {
				t.Fatalf("digest-only image is not dangling: %v", row)
			}
		})
	}
}

func TestDockerCleanupKeepsMultipleRepositoriesAndOptInDigest(t *testing.T) {
	c, engine := cleanupFixture(t)
	ctx, client := t.Context(), engine.Client()
	engine.AddLocalImage("fixture/first:1", dockertest.Image{Size: 64})
	image, err := client.InspectImage(ctx, "fixture/first:1")
	if err != nil {
		t.Fatal(err)
	}
	if err := client.TagImage(ctx, image.ID, "fixture/second:1"); err != nil {
		t.Fatal(err)
	}
	digest := "fixture/digest@sha256:" + strings.Repeat("c", 64)
	engine.AddLocalImage(digest, dockertest.Image{Size: 64})
	if got := cleanupCandidates(t, c, nil); len(got) != 0 {
		t.Fatalf("default cleanup includes a named image: %v", got)
	}
	answer, err := c.dockerCleanupPreview(ctx, &agentv1.DockerMaintenancePreviewCall{
		Fresh: true, Filters: &v1.DockerCleanupFilters{TaggedImages: true},
	})
	if err != nil {
		t.Fatal(err)
	}
	for _, row := range answer.GetTargets() {
		if row.GetEngineId() == image.ID && (row.GetEvidence() != "" ||
			row.GetReason() != v1.DockerCleanupReason_DOCKER_CLEANUP_REASON_MULTIPLE_REPOSITORIES) {
			t.Fatalf("multi-repository image could be removed: %v", row)
		}
	}
	targets := cleanupCandidates(t, c, &v1.DockerCleanupFilters{TaggedImages: true})
	if len(targets) != 1 || !slices.Equal(targets[0].GetImageDigests(), []string{digest}) || targets[0].GetReason() != reasonUnused {
		t.Fatalf("opt-in digest scope: %v", targets)
	}
	receipts, err := applyCleanupFixture(t, c, targets, &agentv1.DockerCleanupProtection{})
	if err != nil || len(receipts) != 1 || receipts[0].GetState() != receiptRemoved {
		t.Fatalf("digest-only removal: %v, %v", receipts, err)
	}
}

func TestDockerCleanupPausesForUnreviewedCacheRemovals(t *testing.T) {
	for _, mode := range []string{"response only", "inventory only"} {
		t.Run(mode, func(t *testing.T) {
			c, engine := cleanupFixture(t)
			first, second := cleanupCachePair()
			if mode == "response only" {
				engine.ReportExtraCacheID = second.ID
			} else {
				engine.IgnoreBuildCacheID, engine.HideExtraCacheDeletes = true, true
				// The prune can remove a record whose size Docker could not measure.
				second.Size = -1
			}
			engine.SetBuildCache(first, second)
			targets := cleanupCandidates(t, c, nil)
			if len(targets) == 0 || targets[0].GetEngineId() != first.ID {
				t.Fatalf("cache candidates: %v", targets)
			}
			receipts, err := applyCleanupFixture(t, c, targets[:1], &agentv1.DockerCleanupProtection{})
			reason := v1.DockerReceiptReason_DOCKER_RECEIPT_REASON_EXTRA_CACHE_REMOVED
			if !errors.Is(err, errCleanupDisabled) || len(receipts) != 1 || receipts[0].GetState() != receiptFailed ||
				receipts[0].GetReason() != reason {
				t.Fatalf("unexpected deletion: %v, %v", receipts, err)
			}
			engine.AddLocalImage("", dockertest.Image{Size: 64})
			answer, err := c.dockerCleanupPreview(t.Context(), &agentv1.DockerMaintenancePreviewCall{})
			pause := answer.GetPause()
			if err != nil || !docker.IsFullID(pause.GetToken()) || pause.GetReason() != reason || len(answer.GetTargets()) != 0 {
				t.Fatalf("paused preview: %v, %v", answer, err)
			}
			if _, err := applyCleanupFixture(t, c, targets[:1], &agentv1.DockerCleanupProtection{}); !errors.Is(err, errCleanupDisabled) {
				t.Fatalf("paused cleanup still applies: %v", err)
			}
			ref := resource.Ref{Kind: host.Kind, Name: host.Name, Scope: "srv_cleanup"}
			resume := &v1.DockerMaintenance{ChangeId: dwid.New(dwid.Change), ResumeCleanupToken: cleanupEvidence("different pause")}
			if err := c.applyDockerMaintenance(t.Context(), ref, resume, nil); err == nil || !c.cleanupDisabled() {
				t.Fatalf("different pause was cleared: %v", err)
			}
			resume.ResumeCleanupToken = pause.GetToken()
			before := len(engine.Seen())
			if err := c.applyDockerMaintenance(t.Context(), ref, resume, nil); err != nil {
				t.Fatal(err)
			}
			if len(engine.Seen()) != before || c.cleanupDisabled() {
				t.Fatal("re-enabling cleanup must clear the pause without calling Docker")
			}
			if got := cleanupCandidates(t, c, nil); !slices.ContainsFunc(got, func(target *v1.DockerCleanupTarget) bool {
				return target.GetKind() == cleanupImage
			}) {
				t.Fatalf("re-enabled cleanup did not list a dangling image: %v", got)
			}
		})
	}
}

func TestDockerCleanupReceiptReasons(t *testing.T) {
	t.Run("Engine refused image by ID", func(t *testing.T) {
		c, engine := cleanupFixture(t)
		const tag = "fixture/image:tested"
		engine.AddLocalImage(tag, dockertest.Image{Size: 64})
		targets := cleanupCandidates(t, c, &v1.DockerCleanupFilters{TaggedImages: true})
		if len(targets) != 1 {
			t.Fatalf("image candidates: %v", targets)
		}
		engine.Intercept(func(r *http.Request) error {
			if r.Method == http.MethodDelete && strings.HasSuffix(r.URL.Path, "/images/"+targets[0].GetEngineId()) {
				engine.AddContainer(docker.Container{Config: docker.Config{Image: tag}})
			}
			return nil
		})
		receipts, err := applyCleanupFixture(t, c, targets, &agentv1.DockerCleanupProtection{})
		if err == nil || len(receipts) != 1 || receipts[0].GetState() != receiptChanged ||
			receipts[0].GetReason() != v1.DockerReceiptReason_DOCKER_RECEIPT_REASON_IMAGE_ID_REFUSED {
			t.Fatalf("Engine refusal: %v, %v", receipts, err)
		}
	})
	t.Run("unverified prune", func(t *testing.T) {
		c, engine := cleanupFixture(t)
		first, _ := cleanupCachePair()
		engine.SetBuildCache(first)
		targets := cleanupCandidates(t, c, nil)
		var pruned atomic.Bool
		engine.Intercept(func(r *http.Request) error {
			if strings.HasSuffix(r.URL.Path, "/build/prune") {
				pruned.Store(true)
			}
			if pruned.Load() && r.URL.Query().Get("type") == "build-cache" {
				return errors.New("fixture lost Docker's cache listing")
			}
			return nil
		})
		receipts, err := applyCleanupFixture(t, c, targets, nil)
		reason := v1.DockerReceiptReason_DOCKER_RECEIPT_REASON_CACHE_UNVERIFIED
		if !errors.Is(err, errCleanupDisabled) || len(receipts) != 1 || receipts[0].GetReason() != reason {
			t.Fatalf("unverified prune: %v, %v", receipts, err)
		}
		pause, err := c.readCleanupPause()
		if err != nil || pause.GetReason() != reason {
			t.Fatalf("unverified prune pause: %v, %v", pause, err)
		}
	})
}

func cleanupCachePair() (docker.DiskUsageCache, docker.DiskUsageCache) {
	now := time.Now().UTC().Add(-time.Hour)
	first := docker.DiskUsageCache{ID: strings.Repeat("a", 25), CreatedAt: now, LastUsedAt: &now, Size: 64}
	second := first
	second.ID = strings.Repeat("b", 25)
	return first, second
}

func TestDockerCleanupCachePrunesOnlyReviewedIdleRecord(t *testing.T) {
	for _, mode := range []string{"untargeted idle", "in use", "newer"} {
		t.Run(mode, func(t *testing.T) {
			c, engine := cleanupFixture(t)
			first, second := cleanupCachePair()
			switch mode {
			case "in use":
				second.InUse = true
			case "newer":
				lastUsed := second.LastUsedAt.Add(time.Minute)
				second.LastUsedAt = &lastUsed
			default:
			}
			if mode != "untargeted idle" {
				engine.ConcurrentCacheRemoval = second.ID
			}
			engine.SetBuildCache(first, second)
			targets := cleanupCandidates(t, c, nil)
			if len(targets) == 0 || targets[0].GetEngineId() != first.ID {
				t.Fatalf("cache candidates: %v", targets)
			}
			receipts, err := applyCleanupFixture(t, c, targets[:1], nil)
			if err != nil || len(receipts) != 1 || receipts[0].GetState() != receiptRemoved || c.cleanupDisabled() {
				t.Fatalf("targeted prune: %v, %v", receipts, err)
			}
			remaining, err := engine.Client().BuildCacheUsage(t.Context())
			want := 0
			if mode == "untargeted idle" {
				want = 1
			}
			if err != nil || len(remaining) != want || want == 1 && remaining[0].ID != second.ID {
				t.Fatalf("remaining cache: %v, %v", remaining, err)
			}
		})
	}
}

func TestDockerCleanupPreviewBoundsRunningContainers(t *testing.T) {
	c, engine := cleanupFixture(t)
	for range 1200 {
		engine.AddContainer(docker.Container{})
	}
	preview := func() error {
		_, err := c.dockerCleanupPreview(t.Context(), &agentv1.DockerMaintenancePreviewCall{Fresh: true})
		return err
	}
	if err := preview(); err != nil {
		t.Fatalf("preview refused 1200 running containers: %v", err)
	}
	engine.AddContainer(docker.Container{})
	if err := preview(); !errors.Is(err, errCleanupSize) {
		t.Fatalf("preview did not refuse 1201 running containers: %v", err)
	}
}

func TestDockerCleanupPreviewByteCapDoesNotAffectDiskUse(t *testing.T) {
	c, engine := cleanupFixture(t)
	engine.AddLocalImage("fixture/image:1", dockertest.Image{Size: 64, Config: docker.Config{
		Labels: map[string]string{"fixture": strings.Repeat("x", 8<<20)},
	}})
	preview := func(fresh bool) {
		t.Helper()
		_, err := c.dockerCleanupPreview(t.Context(), &agentv1.DockerMaintenancePreviewCall{Fresh: fresh})
		if err == nil || !strings.Contains(err.Error(), "8 MiB") {
			t.Fatalf("preview did not report the byte limit: %v", err)
		}
	}
	preview(true)
	if got, err := c.cfg.disks.read(t.Context()); err != nil || got.GetImages() != 1 {
		t.Fatalf("regular disk-use read was capped or poisoned by the preview: %v, %v", got, err)
	}
	// Expire only the preview error so a cached ordinary read cannot mask
	// the fact that preview must make its own bounded read.
	c.cfg.disks.mu.Lock()
	c.cfg.disks.errAt = time.Time{}
	c.cfg.disks.mu.Unlock()
	preview(false)
}

func TestDockerCleanupCacheRequiresVerifiedEngine(t *testing.T) {
	for _, tc := range []struct {
		version, platform string
		allowed           bool
	}{
		{"27.3.1", "Docker Engine - Community", true},
		{"28.5.2", "Docker Engine - Community", true},
		{"28.5.3", "Docker Engine - Community", false},
		{"27.3.1", "compatibility daemon", false},
	} {
		t.Run(tc.version+"/"+tc.platform, func(t *testing.T) {
			c, engine := cleanupFixture(t)
			engine.EngineVersion, engine.EnginePlatform = tc.version, tc.platform
			now := time.Now().UTC().Add(-time.Hour)
			engine.SetBuildCache(docker.DiskUsageCache{ID: strings.Repeat("a", 25), CreatedAt: now, LastUsedAt: &now, Size: 64})
			if got := cleanupCandidates(t, c, nil); (len(got) == 1) != tc.allowed {
				t.Fatalf("wrong prune eligibility: %v", got)
			}
		})
	}
}

func TestDockerCleanupPrunesOldReceiptsWithoutReplayingPlans(t *testing.T) {
	c, engine := cleanupFixture(t)
	engine.AddLocalImage("", dockertest.Image{Size: 64})
	targets := cleanupCandidates(t, c, nil)
	now := time.Now()
	then := now.Add(-31 * 24 * time.Hour)
	change := dwid.At(dwid.Change, then, "old cleanup")
	dir := filepath.Join(c.cfg.Dir, "docker-cleanup")
	if err := os.MkdirAll(dir, 0o700); err != nil {
		t.Fatal(err)
	}
	old := filepath.Join(dir, cleanupEvidence(change)+".json")
	fresh := filepath.Join(dir, cleanupEvidence("fresh")+".json")
	for file, last := range map[string]time.Time{old: then, fresh: now, c.cleanupBlockFile(): then} {
		if err := os.WriteFile(file, nil, 0o600); err != nil {
			t.Fatal(err)
		}
		if err := os.Chtimes(file, last, last); err != nil {
			t.Fatal(err)
		}
	}
	pruneDockerReceipts(c.cfg.Dir, now)
	if _, err := os.Stat(old); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("expired receipt remains: %v", err)
	}
	if _, err := os.Stat(fresh); err != nil || !c.cleanupDisabled() {
		t.Fatalf("pruning removed a recent receipt or the safety marker: %v", err)
	}
	protection := &agentv1.DockerCleanupProtection{}
	err := c.applyDockerMaintenance(t.Context(), resource.Ref{Kind: host.Kind, Name: host.Name, Scope: "srv_cleanup"},
		&v1.DockerMaintenance{ChangeId: change, Targets: targets}, protection)
	if err == nil || !strings.Contains(err.Error(), "expired") {
		t.Fatalf("old plan could be replayed: %v", err)
	}
}

func TestDockerCleanupProtectsSeveralTagsInOneRepository(t *testing.T) {
	c, engine := cleanupFixture(t)
	client, ctx := engine.Client(), t.Context()
	engine.AddLocalImage("fixture/app:1", dockertest.Image{Size: 64})
	image, err := client.InspectImage(ctx, "fixture/app:1")
	if err != nil {
		t.Fatal(err)
	}
	if err := client.TagImage(ctx, image.ID, "fixture/app:stable"); err != nil {
		t.Fatal(err)
	}
	assertCleanupMultiTagProtected(t, c, image.ID)
	tags := []string{"fixture/app:1", "fixture/app:stable"}
	forged := &v1.DockerCleanupTarget{
		ServerId: "srv_cleanup", Kind: cleanupImage, EngineId: image.ID,
		Reason: reasonUnused, ImageTags: tags, Evidence: imageCleanupEvidence(image.ID, tags, nil),
	}
	receipts, err := applyCleanupFixture(t, c, []*v1.DockerCleanupTarget{forged}, &agentv1.DockerCleanupProtection{})
	if err == nil || !strings.Contains(err.Error(), "unsupported target") || len(receipts) != 0 {
		t.Fatalf("forged multi-tag cleanup wasn't rejected before rechecking: %v, %v", receipts, err)
	}
}

func assertCleanupMultiTagProtected(t *testing.T, c *calls, imageID string) {
	t.Helper()
	preview, err := c.dockerCleanupPreview(t.Context(), &agentv1.DockerMaintenancePreviewCall{
		Fresh: true, Filters: &v1.DockerCleanupFilters{TaggedImages: true},
	})
	if err != nil {
		t.Fatal(err)
	}
	for _, row := range preview.GetTargets() {
		if row.GetEngineId() == imageID {
			if row.GetEvidence() != "" || row.GetReason() != v1.DockerCleanupReason_DOCKER_CLEANUP_REASON_MULTIPLE_TAGS {
				t.Fatalf("expected multi-tag protection: %v", row)
			}
			return
		}
	}
	t.Fatal("multi-tag image missing from preview")
}
