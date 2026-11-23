// SPDX-License-Identifier: AGPL-3.0-only

package agent

import (
	"bytes"
	"encoding/json"
	"errors"
	"net/http"
	"os"
	"path/filepath"
	"slices"
	"strconv"
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
	file := filepath.Join(receiptDir, cleanupEvidence(plan.GetChangeId())+".ndjson")
	pending := &v1.DockerReceipt{Target: target, State: receiptUnknown}
	j, err := openDockerReceiptJournal(file, "", nil, false)
	if err != nil {
		t.Fatal(err)
	}
	if err := j.append(0, pending); err != nil {
		t.Fatal(err)
	}
	if err := j.close(); err != nil {
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

func TestDockerCleanupReceiptJournalRepairsTornTail(t *testing.T) {
	targets := []*v1.DockerCleanupTarget{{Name: "one"}, {Name: "two"}}
	removed := &v1.DockerReceipt{Target: targets[0], State: receiptRemoved}
	pending := &v1.DockerReceipt{Target: targets[1], State: receiptUnknown}
	for _, tc := range []struct {
		name   string
		prefix []*v1.DockerReceipt
		next   *v1.DockerReceipt
	}{
		{name: "after valid record", prefix: []*v1.DockerReceipt{removed}, next: pending},
		{name: "first record", next: removed},
	} {
		t.Run(tc.name, func(t *testing.T) {
			dir := t.TempDir()
			path, legacy := filepath.Join(dir, "change.ndjson"), filepath.Join(dir, "change.json")
			var data bytes.Buffer
			for i, receipt := range tc.prefix {
				if err := json.NewEncoder(&data).Encode(dockerReceiptRecord{Index: i, Receipt: receipt}); err != nil {
					t.Fatal(err)
				}
			}
			validSize := data.Len()
			data.WriteString(`{"index":`)
			if err := os.WriteFile(path, data.Bytes(), 0o600); err != nil {
				t.Fatal(err)
			}
			existing, restore, err := readDockerReceipts(path, legacy, targets)
			if err != nil || restore || len(existing) != len(tc.prefix) {
				t.Fatalf("read torn journal = %v, %t, %v", existing, restore, err)
			}
			journal, err := openDockerReceiptJournal(path, "", existing, restore)
			if err != nil {
				t.Fatal(err)
			}
			if stat, err := os.Stat(path); err != nil || stat.Size() != int64(validSize) {
				t.Fatalf("journal size before append = %v, %v, want %d", stat, err, validSize)
			}
			if err := journal.append(len(existing), tc.next); err != nil {
				t.Fatal(err)
			}
			if err := journal.close(); err != nil {
				t.Fatal(err)
			}
			got, _, err := readDockerReceipts(path, legacy, targets)
			if err != nil || len(got) != len(existing)+1 || !proto.Equal(got[len(existing)], tc.next) {
				t.Fatalf("read repaired journal = %v, %v", got, err)
			}
		})
	}
}

func TestDockerCleanupReceiptJournalReadsLegacyAndKeepsRollbackCheckpoint(t *testing.T) {
	dir := t.TempDir()
	path, legacy := filepath.Join(dir, "change.ndjson"), filepath.Join(dir, "change.json")
	targets := []*v1.DockerCleanupTarget{{Name: "one"}, {Name: "two"}, {Name: "three"}}
	first := &v1.DockerReceipt{Target: targets[0], State: receiptRemoved}
	second := &v1.DockerReceipt{Target: targets[1], State: receiptRemoved}
	third := &v1.DockerReceipt{Target: targets[2], State: receiptUnknown}
	writeLegacy := func(receipts []*v1.DockerReceipt) {
		t.Helper()
		data, err := json.Marshal(receipts)
		if err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(legacy, data, 0o600); err != nil {
			t.Fatal(err)
		}
	}
	writeLegacy([]*v1.DockerReceipt{first})
	got, restore, err := readDockerReceipts(path, legacy, targets)
	if err != nil || !restore || len(got) != 1 || !proto.Equal(got[0], first) {
		t.Fatalf("read legacy receipts = %v, %t, %v", got, restore, err)
	}
	journal, err := openDockerReceiptJournal(path, legacy, got, restore)
	if err != nil {
		t.Fatal(err)
	}
	if err := journal.append(1, second); err != nil {
		t.Fatal(err)
	}
	if err := journal.close(); err != nil {
		t.Fatal(err)
	}
	got, restore, err = readDockerReceipts(path, legacy, targets)
	if err != nil || restore || len(got) != 2 || !proto.Equal(got[1], second) {
		t.Fatalf("read NDJSON receipts = %v, %t, %v", got, restore, err)
	}
	data, err := os.ReadFile(legacy)
	var checkpoint []*v1.DockerReceipt
	if err != nil || json.Unmarshal(data, &checkpoint) != nil || len(checkpoint) != 2 || !proto.Equal(checkpoint[1], second) {
		t.Fatalf("legacy rollback checkpoint = %s, %v", data, err)
	}
	// The old agent handles the next target after rollback, then the new
	// agent resumes from that checkpoint without retrying the pending target.
	writeLegacy(append(checkpoint, third))
	got, restore, err = readDockerReceipts(path, legacy, targets)
	if err != nil || !restore || len(got) != 3 || !proto.Equal(got[2], third) {
		t.Fatalf("read checkpoint after rollback = %v, %t, %v", got, restore, err)
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
	if _, _, err := c.cfg.disks.snapshotMode(t.Context(), true, true); err == nil {
		t.Fatal("the background measurement must use the preview byte limit")
	}
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
		{"29.4.1", "Docker Engine - Community", true},
		{"29.4.2", "Docker Engine - Community", false},
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

func TestDockerCleanupRemovesUnusedVolumesAndProtectsUsedOrHeld(t *testing.T) {
	c, engine := cleanupFixture(t)
	ctx, client := t.Context(), engine.Client()
	anonName := strings.Repeat("a", 64)
	oldCreated := "2026-10-01T00:00:00Z"
	engine.AddVolume(docker.Volume{
		Name: anonName, Driver: "local", Scope: "local", CreatedAt: oldCreated,
		Labels: map[string]string{"com.docker.volume.anonymous": ""},
	})
	engine.AddVolume(docker.Volume{Name: "unused-named", Driver: "local", Scope: "local", CreatedAt: oldCreated})
	engine.AddVolume(docker.Volume{Name: "stopped-mounted", Driver: "local", Scope: "local", CreatedAt: oldCreated})
	engine.AddVolume(docker.Volume{Name: "managed-vol", Driver: "local", Scope: "local", CreatedAt: oldCreated})
	engine.AddVolume(docker.Volume{Name: "backed-up-vol", Driver: "local", Scope: "local", CreatedAt: oldCreated})
	engine.AddVolume(docker.Volume{Name: "rollback-vol", Driver: "local", Scope: "local", CreatedAt: oldCreated})
	engine.AddVolume(docker.Volume{Name: "kept-vol", Driver: "local", Scope: "local", CreatedAt: oldCreated})
	engine.AddVolume(docker.Volume{Name: "remote-vol", Driver: "nfs", Scope: "local", CreatedAt: oldCreated})
	engine.AddVolume(docker.Volume{Name: "labeled-vol", Driver: "local", Scope: "local", CreatedAt: oldCreated, Labels: map[string]string{docker.OwnPrefix + "volume": "vol_1"}})
	scopedManaged := docker.ScopedName("env_1", "data")
	engine.AddVolume(docker.Volume{Name: scopedManaged, Driver: "local", Scope: "local", CreatedAt: oldCreated, Labels: map[string]string{docker.LabelProject: "env_1", docker.LabelVolume: "data"}})
	engine.VolumeSizes = map[string]int64{
		anonName:       4 << 20,
		"unused-named": 8 << 20,
	}
	engine.AddContainer(docker.Container{
		Name:   "/stopped-user",
		State:  docker.State{Status: "exited"},
		Mounts: []docker.MountPoint{{Type: "volume", Name: "stopped-mounted", Destination: "/data"}},
	})
	c.cfg.DockerVolumeKept = func(name string) (bool, error) { return name == "kept-vol", nil }

	preview, err := c.dockerCleanupPreview(ctx, &agentv1.DockerMaintenancePreviewCall{
		Fresh:            true,
		Filters:          &v1.DockerCleanupFilters{Volumes: true, Kinds: []v1.DockerCleanupKind{cleanupVolume}},
		ProtectedVolumes: []string{"managed-vol"},
		BackupVolumes:    []string{"backed-up-vol"},
		RollbackVolumes:  []string{"rollback-vol"},
	})
	if err != nil {
		t.Fatal(err)
	}
	byName := map[string]*v1.DockerCleanupTarget{}
	for _, row := range preview.GetTargets() {
		if row.GetKind() != cleanupVolume {
			t.Fatalf("unexpected non-volume target in kind-filtered preview: %v", row)
		}
		byName[row.GetName()] = row
	}
	if row := byName[anonName]; row == nil || row.GetEvidence() == "" || row.GetReason() != reasonAnonymous || row.GetEstimatedBytes() != 4<<20 {
		t.Fatalf("anonymous volume target: %v", row)
	}
	if row := byName["unused-named"]; row == nil || row.GetEvidence() == "" || row.GetReason() != reasonUnused || row.GetEstimatedBytes() != 8<<20 {
		t.Fatalf("named unused volume target: %v", row)
	}
	wantProtected := map[string]v1.DockerCleanupReason{
		"stopped-mounted": reasonInUse,
		"managed-vol":     reasonDesired,
		"labeled-vol":     reasonDesired,
		scopedManaged:     reasonDesired,
		"backed-up-vol":   reasonBackup,
		"rollback-vol":    reasonRollback,
		"kept-vol":        reasonRetained,
		"remote-vol":      v1.DockerCleanupReason_DOCKER_CLEANUP_REASON_EXACT_REMOVAL_UNAVAILABLE,
	}
	for name, want := range wantProtected {
		row := byName[name]
		if row == nil || row.GetEvidence() != "" || row.GetReason() != want {
			t.Errorf("volume %s: got %v, want protected reason %v", name, row, want)
		}
	}

	targets := []*v1.DockerCleanupTarget{byName[anonName], byName["unused-named"]}
	for _, target := range targets {
		target.ServerId = "srv_cleanup"
	}
	before := len(engine.Seen())
	receipts, err := applyCleanupFixture(t, c, targets, &agentv1.DockerCleanupProtection{
		Volumes: []string{"managed-vol"}, BackupVolumes: []string{"backed-up-vol"}, RollbackVolumes: []string{"rollback-vol"},
	})
	if err != nil || len(receipts) != 2 {
		t.Fatalf("apply volume cleanup: %v, %v", receipts, err)
	}
	for _, r := range receipts {
		if r.GetState() != receiptRemoved || r.GetRecheckedEngineId() != r.GetTarget().GetEngineId() || r.AfterBytes == nil || r.GetAfterBytes() != 0 {
			t.Fatalf("volume receipt: %v", r)
		}
	}
	for _, gone := range []string{anonName, "unused-named"} {
		if _, err := client.InspectVolume(ctx, gone); !docker.IsNotFound(err) {
			t.Fatalf("volume %s still exists: %v", gone, err)
		}
	}
	for name := range wantProtected {
		if _, err := client.InspectVolume(ctx, name); err != nil {
			t.Fatalf("protected volume %s was removed: %v", name, err)
		}
	}
	for _, req := range engine.Seen()[before:] {
		if strings.Contains(req, "force=1") || strings.Contains(req, "force=true") || strings.Contains(req, "/volumes/prune") {
			t.Fatalf("forbidden force or daemon-wide volume prune: %s", req)
		}
	}
}

func TestDockerCleanupRefusesChangedVolumes(t *testing.T) {
	for _, scenario := range []string{"stopped container mounted", "recreated volume", "new backup protection", "new desired volume", "retained for recovery"} {
		t.Run(scenario, func(t *testing.T) {
			c, engine := cleanupFixture(t)
			ctx := t.Context()
			engine.AddVolume(docker.Volume{Name: "candidate-vol", Driver: "local", Scope: "local", CreatedAt: "2026-10-01T00:00:00Z"})
			targets := cleanupCandidates(t, c, &v1.DockerCleanupFilters{Volumes: true, Kinds: []v1.DockerCleanupKind{cleanupVolume}})
			if len(targets) != 1 {
				t.Fatalf("volume candidates: %v", targets)
			}
			target := targets[0]
			protection := &agentv1.DockerCleanupProtection{}
			switch scenario {
			case "stopped container mounted":
				engine.AddContainer(docker.Container{
					Name:   "/late-user",
					State:  docker.State{Status: "exited"},
					Mounts: []docker.MountPoint{{Type: "volume", Name: "candidate-vol", Destination: "/data"}},
				})
			case "recreated volume":
				if err := engine.Client().RemoveVolume(ctx, "candidate-vol"); err != nil {
					t.Fatal(err)
				}
				engine.AddVolume(docker.Volume{Name: "candidate-vol", Driver: "local", Scope: "local", CreatedAt: "2026-10-02T00:00:00Z"})
			case "new backup protection":
				protection.BackupVolumes = []string{"candidate-vol"}
			case "new desired volume":
				protection.Volumes = []string{"candidate-vol"}
			case "retained for recovery":
				c.cfg.DockerVolumeKept = func(name string) (bool, error) { return name == "candidate-vol", nil }
			}
			before := len(engine.Seen())
			receipts, err := applyCleanupFixture(t, c, []*v1.DockerCleanupTarget{target}, protection)
			if err == nil || len(receipts) != 1 || receipts[0].GetState() != receiptChanged {
				t.Fatalf("changed volume %s: %v, %v", scenario, receipts, err)
			}
			for _, req := range engine.Seen()[before:] {
				if strings.HasPrefix(req, "DELETE /") && strings.Contains(req, "/volumes/") {
					t.Fatalf("changed volume dispatched DELETE: %s", req)
				}
			}
		})
	}
}

func TestDockerCleanupVolumeMinAgeUsesAgentPreview(t *testing.T) {
	c, engine := cleanupFixture(t)
	previewAt := time.Date(2026, 11, 22, 12, 0, 0, 0, time.UTC)
	now := previewAt
	c.cfg.disks.now = func() time.Time { return now }

	recent := docker.Volume{
		Name: "fresh-vol", Driver: "local", Scope: "local",
		CreatedAt: previewAt.Add(-30 * time.Second).Format(time.RFC3339),
		Labels:    map[string]string{"owner": "test"},
	}
	aged := docker.Volume{
		Name: "aged-vol", Driver: "local", Scope: "local",
		CreatedAt: previewAt.Add(-70 * time.Second).Format(time.RFC3339),
	}
	engine.AddVolume(recent)
	engine.AddVolume(aged)
	engine.VolumeSizes = map[string]int64{recent.Name: 1}
	preview, err := c.dockerCleanupPreview(t.Context(), &agentv1.DockerMaintenancePreviewCall{
		Fresh: true, Filters: &v1.DockerCleanupFilters{Volumes: true, Kinds: []v1.DockerCleanupKind{cleanupVolume}},
	})
	if err != nil {
		t.Fatal(err)
	}
	byName := map[string]*v1.DockerCleanupTarget{}
	for _, row := range preview.GetTargets() {
		row.ServerId = "srv_cleanup"
		byName[row.GetName()] = row
	}
	fresh := byName[recent.Name]
	if fresh.GetEvidence() != "" || fresh.GetReason() != reasonRecentlyCreated {
		t.Fatalf("fresh volume should be protected as RECENTLY_CREATED, got %v", fresh)
	}
	valid := byName[aged.Name]
	if valid.GetEvidence() == "" || valid.GetReason() != reasonUnused {
		t.Fatalf("aged volume should be eligible, got %v", valid)
	}

	// The control plane clock is 20s behind. Its ULID makes aged-vol appear
	// 50s old even though it was old enough when the agent reviewed it.
	controlTime := previewAt.Add(-20 * time.Second)
	var receipts []*v1.DockerReceipt
	ctx := resource.WithOutcome(t.Context(), func(out resource.Outcome) { receipts = out.DockerReceipts })
	apply := func(target *v1.DockerCleanupTarget, seed string) error {
		receipts = nil
		plan := &v1.DockerMaintenance{ChangeId: dwid.At(dwid.Change, controlTime, seed), Targets: []*v1.DockerCleanupTarget{target}}
		return c.applyDockerMaintenance(ctx, resource.Ref{Kind: host.Kind, Name: host.Name, Scope: "srv_cleanup"}, plan, &agentv1.DockerCleanupProtection{})
	}
	if err := apply(valid, "h1-skew"); err != nil || len(receipts) != 1 || receipts[0].GetState() != receiptRemoved {
		t.Fatalf("volume reviewed before a skewed plan ULID = %v, %v", receipts, err)
	}

	// Recreate the volume with exactly the same metadata. Only its contents
	// differ, so the inventory key still matches. Simulate a submitted target
	// that bypassed the preview exclusion and wait until it would pass a floor
	// calculated at apply time.
	if err := engine.Client().RemoveVolume(t.Context(), recent.Name); err != nil {
		t.Fatal(err)
	}
	engine.AddVolume(recent)
	engine.VolumeSizes = map[string]int64{recent.Name: 99}
	now = previewAt.Add(2 * time.Minute)
	replacement := engine.Volume(recent.Name)
	if key := docker.VolumeInventoryKey(replacement); key != fresh.GetEngineId() {
		t.Fatalf("replacement key = %q, want reviewed key %q", key, fresh.GetEngineId())
	}
	fresh.Reason = reasonUnused
	fresh.Evidence = volumeCleanupEvidence(fresh.GetEngineId(), fresh.GetName(), fresh.GetReason(), previewAt)
	before := len(engine.Seen())
	if err := apply(fresh, "h1-same-key-replacement"); err == nil || len(receipts) != 1 || receipts[0].GetState() != receiptChanged {
		t.Fatalf("same-key volume below the preview age floor = %v, %v", receipts, err)
	}
	if receipts[0].GetRecheckedEngineId() != fresh.GetEngineId() || receipts[0].GetBeforeBytes() != 99 {
		t.Fatalf("recheck did not see the same identity with different contents: %v", receipts[0])
	}
	for _, req := range engine.Seen()[before:] {
		if strings.HasPrefix(req, "DELETE /") && strings.Contains(req, "/volumes/") {
			t.Fatalf("same-key replacement reached DELETE: %s", req)
		}
	}
}

func TestDockerCleanupStrictAnonymousClassification(t *testing.T) {
	c, engine := cleanupFixture(t)
	old := "2026-10-01T00:00:00Z"
	realAnon := strings.Repeat("a", 64)
	upperHex := strings.Repeat("A", 64)
	swarmHex := strings.Repeat("b", 64)

	engine.AddVolume(docker.Volume{Name: realAnon, Driver: "local", Scope: "local", CreatedAt: old, Labels: map[string]string{"com.docker.volume.anonymous": ""}})
	engine.AddVolume(docker.Volume{Name: "db-restored", Driver: "local", Scope: "local", CreatedAt: old, Labels: map[string]string{"com.docker.volume.anonymous": ""}})
	engine.AddVolume(docker.Volume{Name: upperHex, Driver: "local", Scope: "local", CreatedAt: old, Labels: map[string]string{"com.docker.volume.anonymous": ""}})
	engine.AddVolume(docker.Volume{Name: swarmHex, Driver: "local", Scope: "local", CreatedAt: old, Labels: map[string]string{"com.docker.volume.anonymous": "", "com.docker.stack.namespace": "prod"}})

	preview, err := c.dockerCleanupPreview(t.Context(), &agentv1.DockerMaintenancePreviewCall{
		Fresh: true, Filters: &v1.DockerCleanupFilters{Volumes: true, Kinds: []v1.DockerCleanupKind{cleanupVolume}},
	})
	if err != nil {
		t.Fatal(err)
	}
	byName := map[string]*v1.DockerCleanupTarget{}
	for _, row := range preview.GetTargets() {
		byName[row.GetName()] = row
	}
	if got := byName[realAnon]; got == nil || got.GetReason() != reasonAnonymous || got.GetEvidence() == "" {
		t.Fatalf("real anonymous volume = %v, want ANONYMOUS", got)
	}
	for _, named := range []string{"db-restored", upperHex, swarmHex} {
		got := byName[named]
		if got == nil || got.GetReason() != reasonUnused || got.GetEvidence() == "" {
			t.Fatalf("volume %s = %v, want opt-in named UNUSED (never ANONYMOUS)", named, got)
		}
	}
}

func TestDockerCleanupProtectsComposeVolumes(t *testing.T) {
	c, engine := cleanupFixture(t)
	old := "2026-10-01T00:00:00Z"
	// Both default-named and custom-named Compose volumes with project down (0 containers).
	engine.AddVolume(docker.Volume{
		Name: "shop_db", Driver: "local", Scope: "local", CreatedAt: old,
		Labels: map[string]string{docker.LabelProject: "shop", docker.LabelVolume: "db"},
	})
	engine.AddVolume(docker.Volume{
		Name: "shop-db", Driver: "local", Scope: "local", CreatedAt: old,
		Labels: map[string]string{docker.LabelProject: "shop", docker.LabelVolume: "db"},
	})
	// Another Compose project with an exited container so Dockwell knows the stack ID.
	engine.AddVolume(docker.Volume{
		Name: "blog-data", Driver: "local", Scope: "local", CreatedAt: old,
		Labels: map[string]string{docker.LabelProject: "blog", docker.LabelVolume: "data"},
	})
	engine.AddContainer(docker.Container{
		Name:   "/blog-web-1",
		State:  docker.State{Status: "exited"},
		Config: docker.Config{Labels: map[string]string{docker.LabelProject: "blog", docker.LabelService: "web"}},
	})

	preview, err := c.dockerCleanupPreview(t.Context(), &agentv1.DockerMaintenancePreviewCall{
		Fresh: true, Filters: &v1.DockerCleanupFilters{Volumes: true, Kinds: []v1.DockerCleanupKind{cleanupVolume}},
	})
	if err != nil {
		t.Fatal(err)
	}
	byName := map[string]*v1.DockerCleanupTarget{}
	for _, row := range preview.GetTargets() {
		byName[row.GetName()] = row
	}
	for _, name := range []string{"shop_db", "shop-db"} {
		row := byName[name]
		if row == nil || row.GetEvidence() != "" || row.GetReason() != reasonCompose || row.GetComposeProject() != "shop" || row.GetComposeId() != "" {
			t.Fatalf("down Compose volume %s = %v, want protected COMPOSE project=shop compose_id=''", name, row)
		}
		// Even a forged apply target for a Compose volume must be refused at apply.
		forged := &v1.DockerCleanupTarget{
			ServerId: "srv_cleanup", Kind: cleanupVolume, EngineId: row.GetEngineId(), Name: name,
			Reason: reasonUnused, Evidence: volumeCleanupEvidence(row.GetEngineId(), name, reasonUnused, c.cleanupNow()),
		}
		receipts, err := applyCleanupFixture(t, c, []*v1.DockerCleanupTarget{forged}, &agentv1.DockerCleanupProtection{})
		if err == nil || len(receipts) != 1 || receipts[0].GetState() != receiptChanged {
			t.Fatalf("forged Compose volume %s apply: receipts=%v err=%v", name, receipts, err)
		}
	}
	blog := byName["blog-data"]
	if blog == nil || blog.GetReason() != reasonCompose || blog.GetComposeProject() != "blog" || blog.GetComposeId() != "outside:blog" {
		t.Fatalf("known Compose volume = %v, want COMPOSE project=blog compose_id=outside:blog", blog)
	}
}

func TestDockerCleanupSwarmManagerBlocksVolumeRemoval(t *testing.T) {
	c, engine := cleanupFixture(t)
	engine.SwarmNodes = 1
	engine.SwarmControlAvailable = true
	old := "2026-10-01T00:00:00Z"
	engine.AddVolume(docker.Volume{Name: "shared-local-swarm-vol", Driver: "local", Scope: "local", CreatedAt: old})
	engine.AddLocalImage("", dockertest.Image{Size: 1024})
	engine.AddNetwork(docker.Network{Name: "idle-net", Driver: "bridge", Scope: "local"})

	preview, err := c.dockerCleanupPreview(t.Context(), &agentv1.DockerMaintenancePreviewCall{
		Fresh: true, Filters: &v1.DockerCleanupFilters{Volumes: true, Networks: true},
	})
	if err != nil {
		t.Fatal(err)
	}
	var volRow, imgRow, netRow *v1.DockerCleanupTarget
	for _, row := range preview.GetTargets() {
		switch row.GetKind() {
		case cleanupVolume:
			if row.GetName() == "shared-local-swarm-vol" {
				volRow = row
			}
		case cleanupImage:
			imgRow = row
		case cleanupNetwork:
			if row.GetName() == "idle-net" {
				netRow = row
			}
		default:
		}
	}
	if volRow == nil || volRow.GetEvidence() != "" || volRow.GetReason() != reasonSwarmManager {
		t.Fatalf("Swarm manager volume = %v, want protected SWARM_MANAGER", volRow)
	}
	if imgRow == nil || imgRow.GetEvidence() == "" || netRow == nil || netRow.GetEvidence() == "" {
		t.Fatalf("images and networks should remain eligible on Swarm manager: img=%v net=%v", imgRow, netRow)
	}
	// Images and networks still clean up on a Swarm manager.
	imgRow.ServerId, netRow.ServerId = "srv_cleanup", "srv_cleanup"
	if receipts, err := applyCleanupFixture(t, c, []*v1.DockerCleanupTarget{imgRow, netRow}, &agentv1.DockerCleanupProtection{}); err != nil || len(receipts) != 2 {
		t.Fatalf("non-volume cleanup on Swarm manager failed: %v, %v", receipts, err)
	}
	// Forged volume apply is refused and never dispatches DELETE /volumes/...
	forged := &v1.DockerCleanupTarget{
		ServerId: "srv_cleanup", Kind: cleanupVolume, EngineId: volRow.GetEngineId(), Name: "shared-local-swarm-vol",
		Reason: reasonUnused, Evidence: volumeCleanupEvidence(volRow.GetEngineId(), "shared-local-swarm-vol", reasonUnused, c.cleanupNow()),
	}
	before := len(engine.Seen())
	receipts, err := applyCleanupFixture(t, c, []*v1.DockerCleanupTarget{forged}, &agentv1.DockerCleanupProtection{})
	if err == nil || len(receipts) != 1 || receipts[0].GetState() != receiptChanged {
		t.Fatalf("Swarm manager forged volume apply = %v, %v", receipts, err)
	}
	for _, req := range engine.Seen()[before:] {
		if strings.HasPrefix(req, "DELETE /") && strings.Contains(req, "/volumes/") {
			t.Fatalf("Swarm manager dispatched volume DELETE: %s", req)
		}
	}
}

func TestDockerCleanupRequiresInactiveSwarmState(t *testing.T) {
	for _, info := range []string{`{}`, `{"Swarm":{}}`} {
		t.Run(info, func(t *testing.T) {
			c, engine := cleanupFixture(t)
			engine.AddVolume(docker.Volume{Name: "local-vol", Driver: "local", Scope: "local", CreatedAt: "2026-10-01T00:00:00Z"})
			filters := &v1.DockerCleanupFilters{Volumes: true, Kinds: []v1.DockerCleanupKind{cleanupVolume}}
			targets := cleanupCandidates(t, c, filters)
			if len(targets) != 1 {
				t.Fatalf("expected volume candidate on an inactive Swarm, got %v", targets)
			}
			engine.RawInfo = info
			preview, err := c.dockerCleanupPreview(t.Context(), &agentv1.DockerMaintenancePreviewCall{Fresh: true, Filters: filters})
			if err != nil || len(preview.GetTargets()) != 1 || preview.GetTargets()[0].GetReason() != reasonSwarmManager || preview.GetTargets()[0].GetEvidence() != "" {
				t.Fatalf("preview with incomplete Engine info = %v, %v", preview, err)
			}
			before := len(engine.Seen())
			receipts, err := applyCleanupFixture(t, c, targets, &agentv1.DockerCleanupProtection{})
			if err == nil || len(receipts) != 1 || receipts[0].GetState() != receiptChanged {
				t.Fatalf("apply with incomplete Engine info = %v, %v", receipts, err)
			}
			for _, req := range engine.Seen()[before:] {
				if strings.HasPrefix(req, "DELETE /") && strings.Contains(req, "/volumes/") {
					t.Fatalf("Engine without inactive state received %s", req)
				}
			}
		})
	}
}

func TestDockerCleanupRechecksImageMountsWithFullList(t *testing.T) {
	c, engine := cleanupFixture(t)
	engine.AddLocalImage("", dockertest.Image{Size: 1024})
	targets := cleanupCandidates(t, c, &v1.DockerCleanupFilters{Kinds: []v1.DockerCleanupKind{cleanupImage}})
	if len(targets) != 1 {
		t.Fatalf("expected an image candidate, got %v", targets)
	}
	otherImageID := "sha256:" + strings.Repeat("f", 64)
	engine.AddLocalImage("worker:1", dockertest.Image{})
	engine.AddContainer(docker.Container{
		Name:   "/uses-image-mount",
		Config: docker.Config{Image: "worker:1"},
		Mounts: []docker.MountPoint{{Type: "image", Source: otherImageID}},
	})
	var lists atomic.Int32
	engine.Intercept(func(r *http.Request) error {
		if strings.HasSuffix(r.URL.Path, "/containers/json") && r.URL.Query().Get("filters") == "" && lists.Add(1) == 2 {
			return errors.New("final container scan unavailable")
		}
		return nil
	})
	receipts, err := applyCleanupFixture(t, c, targets, &agentv1.DockerCleanupProtection{})
	if err == nil || len(receipts) != 1 || receipts[0].GetState() != receiptFailed || lists.Load() != 2 {
		t.Fatalf("final recheck with image mounts = %v, %v (%d full lists)", receipts, err, lists.Load())
	}
	for _, req := range engine.Seen() {
		if strings.HasPrefix(req, "DELETE /") && strings.Contains(req, "/images/") {
			t.Fatalf("failed image occupancy check allowed %s", req)
		}
	}
}

func TestDockerCleanupUnknownVolumePausesAndDistinguishesReplacement(t *testing.T) {
	c, engine := cleanupFixture(t)
	old := "2026-10-01T00:00:00Z"
	engine.AddVolume(docker.Volume{Name: "vol-one", Driver: "local", Scope: "local", CreatedAt: old})
	engine.AddVolume(docker.Volume{Name: "vol-two", Driver: "local", Scope: "local", CreatedAt: old})
	targets := cleanupCandidates(t, c, &v1.DockerCleanupFilters{Volumes: true, Kinds: []v1.DockerCleanupKind{cleanupVolume}})
	if len(targets) != 2 {
		t.Fatalf("got %d targets, want 2", len(targets))
	}
	// Ensure vol-one is first.
	slices.SortFunc(targets, func(a, b *v1.DockerCleanupTarget) int { return strings.Compare(a.GetName(), b.GetName()) })

	// Simulate an ambiguous lost reply on vol-one's DELETE.
	engine.Intercept(func(r *http.Request) error {
		if r.Method == http.MethodDelete && strings.HasSuffix(r.URL.Path, "/volumes/vol-one") {
			return errors.New("connection reset by peer")
		}
		return nil
	})
	plan := &v1.DockerMaintenance{ChangeId: dwid.New(dwid.Change), Targets: targets}
	var receipts []*v1.DockerReceipt
	ctx := resource.WithOutcome(t.Context(), func(out resource.Outcome) { receipts = out.DockerReceipts })
	err := c.applyDockerMaintenance(ctx, resource.Ref{Kind: host.Kind, Name: host.Name, Scope: "srv_cleanup"}, plan, &agentv1.DockerCleanupProtection{})
	if !errors.Is(err, errCleanupDisabled) {
		t.Fatalf("expected errCleanupDisabled on unknown volume receipt, got %v", err)
	}
	// Dispatch must have stopped after vol-one; vol-two must stay untouched.
	if len(receipts) != 1 || receipts[0].GetState() != receiptUnknown ||
		receipts[0].GetReason() != v1.DockerReceiptReason_DOCKER_RECEIPT_REASON_VOLUME_UNVERIFIED {
		t.Fatalf("unknown volume receipt = %v", receipts)
	}
	if engine.Volume("vol-two") == nil {
		t.Fatal("second queued volume was removed after unknown volume outcome")
	}
	// Replace vol-one with a new volume of the same name and replay the plan:
	// reconciliation must distinguish the replacement from the reviewed volume
	// and never attribute the replacement's bytes to the reviewed receipt.
	engine.Intercept(nil)
	if err := engine.Client().RemoveVolume(t.Context(), "vol-one"); err != nil {
		t.Fatal(err)
	}
	engine.AddVolume(docker.Volume{Name: "vol-one", Driver: "local", Scope: "local", CreatedAt: "2026-11-22T12:00:00Z"})
	engine.VolumeSizes = map[string]int64{"vol-one": 99 << 20}
	receipts = nil
	err = c.applyDockerMaintenance(ctx, resource.Ref{Kind: host.Kind, Name: host.Name, Scope: "srv_cleanup"}, plan, &agentv1.DockerCleanupProtection{})
	if !errors.Is(err, errCleanupDisabled) {
		t.Fatalf("replay while paused = %v, want errCleanupDisabled", err)
	}
	if len(receipts) != 1 || receipts[0].GetState() != receiptUnknown ||
		receipts[0].GetReason() != v1.DockerReceiptReason_DOCKER_RECEIPT_REASON_VOLUME_REPLACED ||
		receipts[0].AfterBytes != nil {
		t.Fatalf("replayed unknown volume receipt with replacement = %v", receipts)
	}
	// Subsequent preview reports pause, and a new cleanup plan is refused while paused.
	paused, err := c.dockerCleanupPreview(t.Context(), &agentv1.DockerMaintenancePreviewCall{Fresh: true})
	if err != nil || paused.GetPause() == nil {
		t.Fatalf("paused preview = %v, %v", paused, err)
	}
	if _, err := applyCleanupFixture(t, c, targets[1:], &agentv1.DockerCleanupProtection{}); !errors.Is(err, errCleanupDisabled) {
		t.Fatalf("subsequent apply while paused = %v, want errCleanupDisabled", err)
	}
	// Resume clears the pause cleanly.
	resumePlan := &v1.DockerMaintenance{ChangeId: dwid.New(dwid.Change), ResumeCleanupToken: paused.GetPause().GetToken()}
	if err := c.applyDockerMaintenance(t.Context(), resource.Ref{Kind: host.Kind, Name: host.Name, Scope: "srv_cleanup"}, resumePlan, nil); err != nil {
		t.Fatalf("resume pause: %v", err)
	}
	afterResume, err := c.dockerCleanupPreview(t.Context(), &agentv1.DockerMaintenancePreviewCall{Fresh: true})
	if err != nil || afterResume.GetPause() != nil {
		t.Fatalf("after resume still paused: %v, %v", afterResume, err)
	}
	// The original replay still reports Unknown, with no new record, pause,
	// or delete after the acknowledgment has survived a reload from disk.
	journal := filepath.Join(c.cfg.Dir, "docker-cleanup", cleanupEvidence(plan.GetChangeId())+".ndjson")
	saved, err := os.ReadFile(journal)
	if err != nil || !bytes.Contains(saved, []byte(`"reason":5`)) {
		t.Fatalf("replacement reconciliation was not persisted: %s, %v", saved, err)
	}
	before := len(engine.Seen())
	receipts = nil
	err = c.applyDockerMaintenance(ctx, resource.Ref{Kind: host.Kind, Name: host.Name, Scope: "srv_cleanup"}, plan, &agentv1.DockerCleanupProtection{})
	if err == nil || errors.Is(err, errCleanupDisabled) || c.cleanupDisabled() || len(receipts) != 1 || receipts[0].GetState() != receiptUnknown {
		t.Fatalf("replay after acknowledgment = %v, %v, paused=%t", receipts, err, c.cleanupDisabled())
	}
	for _, req := range engine.Seen()[before:] {
		if strings.HasPrefix(req, "DELETE /") && strings.Contains(req, "/volumes/") {
			t.Fatalf("acknowledged replay dispatched %s", req)
		}
	}
	if data, err := os.ReadFile(journal); err != nil || !bytes.Equal(data, saved) {
		t.Fatalf("replay changed the journal without a new observation: %v", err)
	}
}

func TestDockerCleanupLargeBatchMeasurements(t *testing.T) {
	c, engine := cleanupFixture(t)
	old := time.Date(2026, 10, 12, 12, 0, 0, 0, time.UTC)
	// Seed 250 busy containers on the Engine to simulate a dense host.
	for i := range 250 {
		engine.AddContainer(docker.Container{
			Name:  "/busy-workload-" + strconv.Itoa(i),
			State: docker.State{Status: "running", Running: true},
		})
	}
	// Mixed batch at the 200-target per-server cap: 100 images, 60 volumes, 20 networks, 20 build cache records.
	for i := range 100 {
		engine.AddLocalImage("", dockertest.Image{Size: int64(4096 + i)})
	}
	for i := range 60 {
		engine.AddVolume(docker.Volume{
			Name: "batch-vol-" + strconv.Itoa(i), Driver: "local", Scope: "local",
			CreatedAt: old.Format(time.RFC3339),
		})
	}
	for i := range 20 {
		engine.AddNetwork(docker.Network{Name: "batch-net-" + strconv.Itoa(i), Driver: "bridge", Scope: "local"})
	}
	cacheRecords := make([]docker.DiskUsageCache, 0, 20)
	for i := range 20 {
		cacheRecords = append(cacheRecords, docker.DiskUsageCache{
			ID: "batchcacheid0000000000" + strconv.Itoa(100+i), Size: int64(8192 + i), CreatedAt: old, LastUsedAt: &old,
		})
	}
	engine.SetBuildCache(cacheRecords...)

	var unfilteredLists, filteredLists int
	engine.Intercept(func(r *http.Request) error {
		if r.Method == http.MethodGet && strings.HasSuffix(r.URL.Path, "/containers/json") {
			if r.URL.Query().Get("filters") == "" {
				unfilteredLists++
				time.Sleep(15 * time.Millisecond) // slow full container scan
			} else {
				filteredLists++
			}
		}
		return nil
	})

	t0 := time.Now()
	targets := cleanupCandidates(t, c, &v1.DockerCleanupFilters{Volumes: true, Networks: true})
	previewDur := time.Since(t0)
	if len(targets) != 200 {
		t.Fatalf("mixed preview returned %d candidates, want 200", len(targets))
	}
	plan := &v1.DockerMaintenance{ChangeId: dwid.New(dwid.Change), Targets: targets}
	protoBytes, err := proto.Marshal(plan)
	if err != nil {
		t.Fatal(err)
	}
	unfilteredBeforeApply := unfilteredLists
	t1 := time.Now()
	receipts, err := applyCleanupFixture(t, c, targets, &agentv1.DockerCleanupProtection{})
	applyDur := time.Since(t1)
	if err != nil || len(receipts) != 200 {
		t.Fatalf("apply 200 mixed targets: %v (receipts=%d)", err, len(receipts))
	}
	unfilteredDuringApply := unfilteredLists - unfilteredBeforeApply
	if unfilteredDuringApply > 1 {
		t.Fatalf("expected at most 1 unfiltered /containers/json scan during apply, got %d (filtered=%d)", unfilteredDuringApply, filteredLists)
	}
	t.Logf("mixed cleanup scale n=200 (250 containers): plan_proto_bytes=%d preview=%s apply=%s unfiltered_apply=%d filtered_apply=%d",
		len(protoBytes), previewDur.Round(time.Millisecond), applyDur.Round(time.Millisecond), unfilteredDuringApply, filteredLists)
}
