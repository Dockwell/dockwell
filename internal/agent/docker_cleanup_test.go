// SPDX-License-Identifier: AGPL-3.0-only

package agent

import (
	"bytes"
	"encoding/json"
	"errors"
	"net"
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
	for _, tc := range []struct {
		scenario string
		want     v1.DockerReceiptReason
	}{
		{"dangling retagged", receiptReasonReferenceMoved},
		{"tag moved", receiptReasonReferenceMoved},
		{"network ID reassigned", receiptReasonReferenceMoved},
		{"stopped image user", receiptReasonInUse},
		{"stopped network user", receiptReasonInUse},
		{"new rollback digest", receiptReasonProtected},
		{"new desired tag", receiptReasonProtected},
		{"new desired network", receiptReasonProtected},
	} {
		t.Run(tc.scenario, func(t *testing.T) {
			scenario := tc.scenario
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
			if err != nil || len(receipts) != 1 || receipts[0].GetState() != receiptChanged ||
				receipts[0].GetRecheckedEngineId() != rechecked || receipts[0].GetReason() != tc.want {
				t.Fatalf("changed target %s: %v, %v (want reason %v)", rechecked, receipts, err, tc.want)
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
		if err != nil || len(receipts) != 1 || receipts[0].GetState() != receiptChanged ||
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
		Reason: reasonUnused, ImageTags: tags, ImageRecords: tags, Evidence: imageCleanupEvidence(image.ID, tags, nil, tags),
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
	for _, tc := range []struct {
		scenario string
		want     v1.DockerReceiptReason
	}{
		{"stopped container mounted", receiptReasonInUse},
		{"recreated volume", v1.DockerReceiptReason_DOCKER_RECEIPT_REASON_VOLUME_REPLACED},
		{"new backup protection", receiptReasonProtected},
		{"new desired volume", receiptReasonProtected},
		{"retained for recovery", receiptReasonProtected},
	} {
		t.Run(tc.scenario, func(t *testing.T) {
			scenario := tc.scenario
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
			if err != nil || len(receipts) != 1 || receipts[0].GetState() != receiptChanged || receipts[0].GetReason() != tc.want {
				t.Fatalf("changed volume %s: %v, %v (want reason %v)", scenario, receipts, err, tc.want)
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
	if err := apply(fresh, "h1-same-key-replacement"); err != nil || len(receipts) != 1 ||
		receipts[0].GetState() != receiptChanged || receipts[0].GetReason() != receiptReasonNewer {
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
		if err != nil || len(receipts) != 1 || receipts[0].GetState() != receiptChanged || receipts[0].GetReason() != receiptReasonProtected {
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
	if !errors.Is(err, errCleanupSwarm) || len(receipts) != 0 {
		t.Fatalf("Swarm manager forged volume apply = %v, %v, want errCleanupSwarm with 0 receipts", receipts, err)
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
			if !errors.Is(err, errCleanupSwarm) || len(receipts) != 0 {
				t.Fatalf("apply with incomplete Engine info = %v, %v, want errCleanupSwarm with 0 receipts", receipts, err)
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

func TestDockerCleanupCanonicalPinnedImageRefs(t *testing.T) {
	c, _ := cleanupFixture(t)
	imageID := "sha256:" + strings.Repeat("a", 64)
	for _, tc := range []struct {
		name        string
		dfTags      []string
		dfDigests   []string
		inspTags    []string
		inspDigests []string
		wantName    string
		wantDigest  string
	}{
		{
			name:        "single-segment pinned containerd image",
			dfTags:      []string{"busybox:1.36@" + imageID},
			dfDigests:   []string{"busybox@" + imageID},
			inspTags:    []string{"busybox:1.36@" + imageID},
			inspDigests: []string{"busybox:1.36@" + imageID},
			wantName:    "busybox:1.36",
			wantDigest:  "busybox@" + imageID,
		},
		{
			name:        "docker.io library pinned image",
			dfTags:      []string{"docker.io/library/busybox:1.36@" + imageID},
			dfDigests:   []string{"busybox@" + imageID},
			inspTags:    []string{"busybox:1.36@" + imageID},
			inspDigests: []string{"docker.io/library/busybox:1.36@" + imageID},
			wantName:    "busybox:1.36",
			wantDigest:  "busybox@" + imageID,
		},
		{
			name:        "custom registry with port pinned image",
			dfTags:      []string{"example.com:5000/team/app:1.2@" + imageID},
			dfDigests:   []string{"example.com:5000/team/app@" + imageID},
			inspTags:    []string{"example.com:5000/team/app:1.2@" + imageID},
			inspDigests: []string{"example.com:5000/team/app:1.2@" + imageID},
			wantName:    "example.com:5000/team/app:1.2",
			wantDigest:  "example.com:5000/team/app@" + imageID,
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			fromDF := c.imageCleanupTarget(docker.DiskUsageImage{
				ID: imageID, RepoTags: tc.dfTags, RepoDigests: tc.dfDigests, Size: 4096,
			}, dockerOccupancy{}, nil)
			fromInspect := c.imageCleanupTarget(docker.DiskUsageImage{
				ID: imageID, RepoTags: tc.inspTags, RepoDigests: tc.inspDigests, Size: 4096,
			}, dockerOccupancy{}, nil)
			if !proto.Equal(fromDF, fromInspect) {
				t.Fatalf("df and inspect targets differ:\ndf:      %v\ninspect: %v", fromDF, fromInspect)
			}
			if fromDF.GetName() != tc.wantName || fromDF.GetReason() != reasonUnused ||
				len(fromDF.GetImageTags()) != 0 || !slices.Equal(fromDF.GetImageDigests(), []string{tc.wantDigest}) ||
				fromDF.GetEvidence() == "" {
				t.Fatalf("unexpected canonical target: %v", fromDF)
			}
		})
	}

	t.Run("multiple records and repositories stay protected", func(t *testing.T) {
		twoRecords := c.imageCleanupTarget(docker.DiskUsageImage{
			ID:          imageID,
			RepoTags:    []string{"busybox:1.36@" + imageID, "busybox:1.36"},
			RepoDigests: []string{"busybox@" + imageID},
		}, dockerOccupancy{}, nil)
		if twoRecords.GetReason() != reasonUnavailable || twoRecords.GetEvidence() != "" {
			t.Fatalf("two containerd records must stay unavailable: %v", twoRecords)
		}
		twoRepos := c.imageCleanupTarget(docker.DiskUsageImage{
			ID:          imageID,
			RepoTags:    []string{"busybox:1.36@" + imageID},
			RepoDigests: []string{"example.com/other@" + imageID},
		}, dockerOccupancy{}, nil)
		if twoRepos.GetReason() != v1.DockerCleanupReason_DOCKER_CLEANUP_REASON_MULTIPLE_REPOSITORIES || twoRepos.GetEvidence() != "" {
			t.Fatalf("two repositories must stay protected: %v", twoRepos)
		}
	})

	t.Run("preview from df and apply via inspect remove pinned image", func(t *testing.T) {
		pinnedRef := "busybox:1.36@" + imageID
		canonicalDigest := "busybox@" + imageID
		var deleted atomic.Bool
		var inspectedDigest atomic.Bool
		sockDir, err := os.MkdirTemp("", "dw-pin-")
		if err != nil {
			t.Fatal(err)
		}
		if len(filepath.Join(sockDir, "d.sock")) >= 100 {
			_ = os.RemoveAll(sockDir)
			sockDir, err = os.MkdirTemp("/tmp", "dw-pin-")
			if err != nil {
				t.Fatal(err)
			}
		}
		t.Cleanup(func() { _ = os.RemoveAll(sockDir) })
		sock := filepath.Join(sockDir, "d.sock")
		rootDir := t.TempDir()
		ln, err := net.Listen("unix", sock)
		if err != nil {
			t.Fatal(err)
		}
		srv := &http.Server{Handler: http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			w.Header().Set("Content-Type", "application/json")
			path := r.URL.Path
			if i := strings.Index(path, "/"); i == 0 && strings.HasPrefix(path, "/v1.") {
				if j := strings.Index(path[1:], "/"); j >= 0 {
					path = path[1+j:]
				}
			}
			switch {
			case r.Method == http.MethodGet && path == "/version":
				_ = json.NewEncoder(w).Encode(map[string]any{"ApiVersion": "1.52", "Version": "29.4.1", "Platform": map[string]string{"Name": "Docker Engine - Community"}})
			case r.Method == http.MethodGet && path == "/info":
				_ = json.NewEncoder(w).Encode(map[string]any{"DockerRootDir": rootDir, "Swarm": map[string]any{"LocalNodeState": "inactive"}})
			case r.Method == http.MethodGet && path == "/containers/json":
				_ = json.NewEncoder(w).Encode([]any{})
			case r.Method == http.MethodGet && path == "/system/df":
				var images []docker.DiskUsageImage
				if !deleted.Load() {
					images = []docker.DiskUsageImage{{
						ID: imageID, Size: 4096, RepoTags: []string{pinnedRef}, RepoDigests: []string{canonicalDigest},
					}}
				}
				_ = json.NewEncoder(w).Encode(docker.DiskUsage{Images: images})
			case r.Method == http.MethodGet && strings.HasPrefix(path, "/images/") && strings.HasSuffix(path, "/json"):
				ref := strings.TrimSuffix(strings.TrimPrefix(path, "/images/"), "/json")
				if deleted.Load() || (ref != imageID && ref != canonicalDigest && ref != pinnedRef) {
					w.WriteHeader(http.StatusNotFound)
					_ = json.NewEncoder(w).Encode(map[string]string{"message": "No such image"})
					return
				}
				if ref == canonicalDigest {
					inspectedDigest.Store(true)
				}
				// Containerd inspect reports name:tag@sha256:X in both RepoTags and RepoDigests.
				_ = json.NewEncoder(w).Encode(map[string]any{
					"Id":          imageID,
					"RepoTags":    []string{pinnedRef},
					"RepoDigests": []string{pinnedRef},
					"Config":      map[string]any{},
					"Size":        4096,
				})
			case r.Method == http.MethodDelete && path == "/images/"+imageID:
				deleted.Store(true)
				_ = json.NewEncoder(w).Encode([]map[string]string{{"Deleted": imageID}})
			default:
				w.WriteHeader(http.StatusNotFound)
				_ = json.NewEncoder(w).Encode(map[string]string{"message": "not found: " + r.Method + " " + path})
			}
		})}
		go func() { _ = srv.Serve(ln) }()
		t.Cleanup(func() { _ = srv.Close() })

		live := cleanupCalls(t, docker.New(sock))
		targets := cleanupCandidates(t, live, &v1.DockerCleanupFilters{TaggedImages: true})
		if len(targets) != 1 || targets[0].GetName() != "busybox:1.36" ||
			!slices.Equal(targets[0].GetImageDigests(), []string{canonicalDigest}) {
			t.Fatalf("pinned preview targets = %v", targets)
		}
		receipts, err := applyCleanupFixture(t, live, targets, &agentv1.DockerCleanupProtection{})
		if err != nil || len(receipts) != 1 || receipts[0].GetState() != receiptRemoved ||
			!deleted.Load() || !inspectedDigest.Load() {
			t.Fatalf("pinned apply = %v, %v (deleted=%t inspectedDigest=%t)", receipts, err, deleted.Load(), inspectedDigest.Load())
		}
	})

	t.Run("replacing pinned record with another pinned or digest-only record reads as REFERENCE_MOVED", func(t *testing.T) {
		for _, tc := range []struct {
			name        string
			afterTags   []string
			afterDigest []string
		}{
			{
				name:        "different pinned tag for same digest",
				afterTags:   []string{"busybox:1.36.1@" + imageID},
				afterDigest: []string{"busybox:1.36.1@" + imageID},
			},
			{
				name:        "containerd digest-only record for same digest",
				afterTags:   []string{"busybox@" + imageID},
				afterDigest: []string{"busybox@" + imageID},
			},
			{
				name:        "classic digest-only record for same digest",
				afterTags:   nil,
				afterDigest: []string{"busybox@" + imageID},
			},
		} {
			t.Run(tc.name, func(t *testing.T) {
				origRef := "busybox:1.36@" + imageID
				canonicalDigest := "busybox@" + imageID
				var replaced atomic.Bool
				var deleted atomic.Bool
				sockDir, err := os.MkdirTemp("", "dw-h1-")
				if err != nil {
					t.Fatal(err)
				}
				if len(filepath.Join(sockDir, "d.sock")) >= 100 {
					_ = os.RemoveAll(sockDir)
					sockDir, err = os.MkdirTemp("/tmp", "dw-h1-")
					if err != nil {
						t.Fatal(err)
					}
				}
				t.Cleanup(func() { _ = os.RemoveAll(sockDir) })
				sock := filepath.Join(sockDir, "d.sock")
				rootDir := t.TempDir()
				ln, err := net.Listen("unix", sock)
				if err != nil {
					t.Fatal(err)
				}
				srv := &http.Server{Handler: http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
					w.Header().Set("Content-Type", "application/json")
					path := r.URL.Path
					if i := strings.Index(path, "/"); i == 0 && strings.HasPrefix(path, "/v1.") {
						if j := strings.Index(path[1:], "/"); j >= 0 {
							path = path[1+j:]
						}
					}
					switch {
					case r.Method == http.MethodGet && path == "/version":
						_ = json.NewEncoder(w).Encode(map[string]any{"ApiVersion": "1.52", "Version": "29.4.1", "Platform": map[string]string{"Name": "Docker Engine - Community"}})
					case r.Method == http.MethodGet && path == "/info":
						_ = json.NewEncoder(w).Encode(map[string]any{"DockerRootDir": rootDir, "Swarm": map[string]any{"LocalNodeState": "inactive"}})
					case r.Method == http.MethodGet && path == "/containers/json":
						_ = json.NewEncoder(w).Encode([]any{})
					case r.Method == http.MethodGet && path == "/system/df":
						tags, digests := []string{origRef}, []string{canonicalDigest}
						if replaced.Load() {
							tags, digests = tc.afterTags, []string{canonicalDigest}
						}
						_ = json.NewEncoder(w).Encode(docker.DiskUsage{Images: []docker.DiskUsageImage{{
							ID: imageID, Size: 4096, RepoTags: tags, RepoDigests: digests,
						}}})
					case r.Method == http.MethodGet && strings.HasPrefix(path, "/images/") && strings.HasSuffix(path, "/json"):
						// Moby resolves digest lookups by repo+digest even when the original pinned tag spelling is gone.
						tags, digests := []string{origRef}, []string{origRef}
						if replaced.Load() {
							tags, digests = tc.afterTags, tc.afterDigest
						}
						_ = json.NewEncoder(w).Encode(map[string]any{
							"Id":          imageID,
							"RepoTags":    tags,
							"RepoDigests": digests,
							"Config":      map[string]any{},
							"Size":        4096,
						})
					case r.Method == http.MethodDelete && strings.HasPrefix(path, "/images/"):
						deleted.Store(true)
						_ = json.NewEncoder(w).Encode([]map[string]string{{"Deleted": imageID}})
					default:
						w.WriteHeader(http.StatusNotFound)
					}
				})}
				go func() { _ = srv.Serve(ln) }()
				t.Cleanup(func() { _ = srv.Close() })

				live := cleanupCalls(t, docker.New(sock))
				targets := cleanupCandidates(t, live, &v1.DockerCleanupFilters{TaggedImages: true})
				if len(targets) != 1 || !slices.Equal(targets[0].GetImageRecords(), []string{origRef}) {
					t.Fatalf("preview targets = %v, want image_records=[%s]", targets, origRef)
				}
				replaced.Store(true)
				receipts, err := applyCleanupFixture(t, live, targets, &agentv1.DockerCleanupProtection{})
				if err != nil || len(receipts) != 1 || receipts[0].GetState() != receiptChanged ||
					receipts[0].GetReason() != receiptReasonReferenceMoved || deleted.Load() {
					t.Fatalf("substituted pinned record apply = %v, %v (deleted=%t), want CHANGED/REFERENCE_MOVED and no delete", receipts, err, deleted.Load())
				}
			})
		}
	})
}

func TestDockerCleanupSwarmStopNoChangedReceiptsAndReplay(t *testing.T) {
	t.Run("initial Swarm activation writes no receipts and replay refuses again", func(t *testing.T) {
		c, engine := cleanupFixture(t)
		engine.AddVolume(docker.Volume{Name: "swarm-vol", Driver: "local", Scope: "local", CreatedAt: "2026-10-01T00:00:00Z"})
		engine.AddLocalImage("", dockertest.Image{Size: 1024})
		targets := cleanupCandidates(t, c, &v1.DockerCleanupFilters{Volumes: true})
		if len(targets) != 2 {
			t.Fatalf("candidates = %d, want 2", len(targets))
		}
		for _, target := range targets {
			target.ServerId = "srv_cleanup"
		}
		plan := &v1.DockerMaintenance{ChangeId: dwid.New(dwid.Change), Targets: targets}
		ref := resource.Ref{Kind: host.Kind, Name: host.Name, Scope: "srv_cleanup"}

		engine.SwarmLocalNodeState = "active"
		for attempt := range 2 {
			before := len(engine.Seen())
			var receipts []*v1.DockerReceipt
			ctx := resource.WithOutcome(t.Context(), func(out resource.Outcome) { receipts = out.DockerReceipts })
			err := c.applyDockerMaintenance(ctx, ref, plan, &agentv1.DockerCleanupProtection{})
			if !errors.Is(err, errCleanupSwarm) || len(receipts) != 0 {
				t.Fatalf("attempt %d: got receipts=%v err=%v, want errCleanupSwarm and 0 receipts", attempt, receipts, err)
			}
			for _, req := range engine.Seen()[before:] {
				if strings.HasPrefix(req, "DELETE ") {
					t.Fatalf("attempt %d: dispatched %s while Swarm active", attempt, req)
				}
			}
		}
	})

	t.Run("late Swarm activation before volume final check stops batch and blocks replay", func(t *testing.T) {
		var infoCalls atomic.Int32
		var deleted atomic.Bool
		volName := "late-swarm-vol"
		imgID := "sha256:" + strings.Repeat("d", 64)
		sockDir, err := os.MkdirTemp("", "dw-sw-")
		if err != nil {
			t.Fatal(err)
		}
		if len(filepath.Join(sockDir, "d.sock")) >= 100 {
			_ = os.RemoveAll(sockDir)
			sockDir, err = os.MkdirTemp("/tmp", "dw-sw-")
			if err != nil {
				t.Fatal(err)
			}
		}
		t.Cleanup(func() { _ = os.RemoveAll(sockDir) })
		sock := filepath.Join(sockDir, "d.sock")
		rootDir := t.TempDir()
		vol := docker.Volume{Name: volName, Driver: "local", Scope: "local", Mountpoint: "/var/lib/docker/volumes/" + volName + "/_data", CreatedAt: "2026-10-01T00:00:00Z"}
		var armLateSwarm atomic.Bool
		ln, err := net.Listen("unix", sock)
		if err != nil {
			t.Fatal(err)
		}
		srv := &http.Server{Handler: http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			w.Header().Set("Content-Type", "application/json")
			path := r.URL.Path
			if i := strings.Index(path, "/"); i == 0 && strings.HasPrefix(path, "/v1.") {
				if j := strings.Index(path[1:], "/"); j >= 0 {
					path = path[1+j:]
				}
			}
			switch {
			case r.Method == http.MethodGet && path == "/version":
				_ = json.NewEncoder(w).Encode(map[string]any{"ApiVersion": "1.52", "Version": "29.4.1", "Platform": map[string]string{"Name": "Docker Engine - Community"}})
			case r.Method == http.MethodGet && path == "/info":
				state := "inactive"
				if armLateSwarm.Load() {
					// First /info during apply is the batch pre-check (inactive); second is the late final check (active).
					if infoCalls.Add(1) >= 2 {
						state = "active"
					}
				}
				_ = json.NewEncoder(w).Encode(map[string]any{"DockerRootDir": rootDir, "Swarm": map[string]any{"LocalNodeState": state}})
			case r.Method == http.MethodGet && path == "/containers/json":
				_ = json.NewEncoder(w).Encode([]any{})
			case r.Method == http.MethodGet && path == "/system/df":
				_ = json.NewEncoder(w).Encode(docker.DiskUsage{
					Images:  []docker.DiskUsageImage{{ID: imgID, Size: 1024}},
					Volumes: []docker.DiskUsageVolume{{Name: volName, Mountpoint: vol.Mountpoint, CreatedAt: vol.CreatedAt, UsageData: &docker.VolumeUsage{Size: 2048}}},
				})
			case r.Method == http.MethodGet && path == "/volumes":
				_ = json.NewEncoder(w).Encode(map[string]any{"Volumes": []docker.Volume{vol}})
			case r.Method == http.MethodGet && path == "/volumes/"+volName:
				_ = json.NewEncoder(w).Encode(vol)
			case r.Method == http.MethodGet && path == "/images/"+imgID+"/json":
				_ = json.NewEncoder(w).Encode(map[string]any{"Id": imgID, "Config": map[string]any{}, "Size": 1024})
			case r.Method == http.MethodDelete:
				deleted.Store(true)
				w.WriteHeader(http.StatusOK)
			default:
				w.WriteHeader(http.StatusNotFound)
			}
		})}
		go func() { _ = srv.Serve(ln) }()
		t.Cleanup(func() { _ = srv.Close() })

		live := cleanupCalls(t, docker.New(sock))
		targets := cleanupCandidates(t, live, &v1.DockerCleanupFilters{Volumes: true})
		// Put volume first, image second so late Swarm activation happens while the image is still pending.
		slices.SortFunc(targets, func(a, b *v1.DockerCleanupTarget) int {
			if a.GetKind() == cleanupVolume && b.GetKind() != cleanupVolume {
				return -1
			}
			if a.GetKind() != cleanupVolume && b.GetKind() == cleanupVolume {
				return 1
			}
			return 0
		})
		if len(targets) != 2 || targets[0].GetKind() != cleanupVolume || targets[1].GetKind() != cleanupImage {
			t.Fatalf("targets = %v, want [volume, image]", targets)
		}
		for _, target := range targets {
			target.ServerId = "srv_cleanup"
		}
		plan := &v1.DockerMaintenance{ChangeId: dwid.New(dwid.Change), Targets: targets}
		ref := resource.Ref{Kind: host.Kind, Name: host.Name, Scope: "srv_cleanup"}

		armLateSwarm.Store(true)
		for attempt := range 2 {
			var receipts []*v1.DockerReceipt
			ctx := resource.WithOutcome(t.Context(), func(out resource.Outcome) { receipts = out.DockerReceipts })
			err := live.applyDockerMaintenance(ctx, ref, plan, &agentv1.DockerCleanupProtection{})
			if !errors.Is(err, errCleanupSwarm) || len(receipts) != 0 || deleted.Load() {
				t.Fatalf("attempt %d: receipts=%v err=%v deleted=%t, want errCleanupSwarm, 0 receipts, no delete", attempt, receipts, err, deleted.Load())
			}
		}
	})

	t.Run("Swarm activation during volume preparation writes durable Failed receipt and blocks replay", func(t *testing.T) {
		var swarmActiveAfterPrepare atomic.Bool
		var postPrepareTriggered atomic.Bool
		var deleted atomic.Bool
		var agentDir string
		volName := "prep-swarm-vol"
		imgID := "sha256:" + strings.Repeat("c", 64)
		sockDir, err := os.MkdirTemp("", "dw-swp-")
		if err != nil {
			t.Fatal(err)
		}
		if len(filepath.Join(sockDir, "d.sock")) >= 100 {
			_ = os.RemoveAll(sockDir)
			sockDir, err = os.MkdirTemp("/tmp", "dw-swp-")
			if err != nil {
				t.Fatal(err)
			}
		}
		t.Cleanup(func() { _ = os.RemoveAll(sockDir) })
		sock := filepath.Join(sockDir, "d.sock")
		rootDir := t.TempDir()
		vol := docker.Volume{Name: volName, Driver: "local", Scope: "local", Mountpoint: "/var/lib/docker/volumes/" + volName + "/_data", CreatedAt: "2026-10-01T00:00:00Z"}
		ln, err := net.Listen("unix", sock)
		if err != nil {
			t.Fatal(err)
		}
		srv := &http.Server{Handler: http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			w.Header().Set("Content-Type", "application/json")
			path := r.URL.Path
			if i := strings.Index(path, "/"); i == 0 && strings.HasPrefix(path, "/v1.") {
				if j := strings.Index(path[1:], "/"); j >= 0 {
					path = path[1+j:]
				}
			}
			switch {
			case r.Method == http.MethodGet && path == "/version":
				_ = json.NewEncoder(w).Encode(map[string]any{"ApiVersion": "1.52", "Version": "29.4.1", "Platform": map[string]string{"Name": "Docker Engine - Community"}})
			case r.Method == http.MethodGet && path == "/info":
				state := "inactive"
				if swarmActiveAfterPrepare.Load() && agentDir != "" {
					// Trigger only after prepare() has written the pending receipt and pause file.
					if _, statErr := os.Stat(filepath.Join(agentDir, "docker-cleanup", "disabled")); statErr == nil {
						state = "active"
						postPrepareTriggered.Store(true)
					}
				}
				_ = json.NewEncoder(w).Encode(map[string]any{"DockerRootDir": rootDir, "Swarm": map[string]any{"LocalNodeState": state}})
			case r.Method == http.MethodGet && path == "/containers/json":
				_ = json.NewEncoder(w).Encode([]any{})
			case r.Method == http.MethodGet && path == "/system/df":
				_ = json.NewEncoder(w).Encode(docker.DiskUsage{
					Images:  []docker.DiskUsageImage{{ID: imgID, Size: 1024}},
					Volumes: []docker.DiskUsageVolume{{Name: volName, Mountpoint: vol.Mountpoint, CreatedAt: vol.CreatedAt, UsageData: &docker.VolumeUsage{Size: 2048}}},
				})
			case r.Method == http.MethodGet && path == "/volumes":
				_ = json.NewEncoder(w).Encode(map[string]any{"Volumes": []docker.Volume{vol}})
			case r.Method == http.MethodGet && path == "/volumes/"+volName:
				_ = json.NewEncoder(w).Encode(vol)
			case r.Method == http.MethodGet && path == "/images/"+imgID+"/json":
				_ = json.NewEncoder(w).Encode(map[string]any{"Id": imgID, "Config": map[string]any{}, "Size": 1024})
			case r.Method == http.MethodDelete:
				deleted.Store(true)
				w.WriteHeader(http.StatusOK)
			default:
				w.WriteHeader(http.StatusNotFound)
			}
		})}
		go func() { _ = srv.Serve(ln) }()
		t.Cleanup(func() { _ = srv.Close() })

		live := cleanupCalls(t, docker.New(sock))
		agentDir = live.cfg.Dir
		targets := cleanupCandidates(t, live, &v1.DockerCleanupFilters{Volumes: true})
		slices.SortFunc(targets, func(a, b *v1.DockerCleanupTarget) int {
			if a.GetKind() == cleanupVolume && b.GetKind() != cleanupVolume {
				return -1
			}
			if a.GetKind() != cleanupVolume && b.GetKind() == cleanupVolume {
				return 1
			}
			return 0
		})
		if len(targets) != 2 || targets[0].GetKind() != cleanupVolume || targets[1].GetKind() != cleanupImage {
			t.Fatalf("targets = %v, want [volume, image]", targets)
		}
		for _, target := range targets {
			target.ServerId = "srv_cleanup"
		}
		plan := &v1.DockerMaintenance{ChangeId: dwid.New(dwid.Change), Targets: targets}
		ref := resource.Ref{Kind: host.Kind, Name: host.Name, Scope: "srv_cleanup"}

		swarmActiveAfterPrepare.Store(true)
		var receipts []*v1.DockerReceipt
		ctx := resource.WithOutcome(t.Context(), func(out resource.Outcome) { receipts = out.DockerReceipts })
		err = live.applyDockerMaintenance(ctx, ref, plan, &agentv1.DockerCleanupProtection{})
		if !errors.Is(err, errCleanupSwarm) || !postPrepareTriggered.Load() || deleted.Load() || live.cleanupDisabled() {
			t.Fatalf("post-prepare Swarm apply: err=%v triggered=%t deleted=%t paused=%t, want errCleanupSwarm, triggered, no delete, not paused", err, postPrepareTriggered.Load(), deleted.Load(), live.cleanupDisabled())
		}
		if len(receipts) != 1 || receipts[0].GetState() != receiptFailed || receipts[0].GetReason() != receiptReasonProtected {
			t.Fatalf("post-prepare Swarm receipts = %v, want durable [FAILED/PROTECTED]", receipts)
		}

		// Even after Swarm turns inactive again, replay refuses because the Failed receipt is durable.
		swarmActiveAfterPrepare.Store(false)
		var replayReceipts []*v1.DockerReceipt
		rctx := resource.WithOutcome(t.Context(), func(out resource.Outcome) { replayReceipts = out.DockerReceipts })
		err = live.applyDockerMaintenance(rctx, ref, plan, &agentv1.DockerCleanupProtection{})
		if err == nil || deleted.Load() {
			t.Fatalf("replay after post-prepare Swarm failure: err=%v deleted=%t, want refusal and no delete", err, deleted.Load())
		}
		if len(replayReceipts) != 1 || replayReceipts[0].GetState() != receiptFailed || replayReceipts[0].GetReason() != receiptReasonProtected {
			t.Fatalf("replay receipts = %v, want [FAILED/PROTECTED]", replayReceipts)
		}
	})
}

func TestDockerCleanupLegacyEvidenceCompatibility(t *testing.T) {
	c, engine := cleanupFixture(t)
	imageID := "sha256:" + strings.Repeat("e", 64)
	pinnedRef := "busybox:1.36@" + imageID
	canonicalDigest := "busybox@" + imageID
	ref := resource.Ref{Kind: host.Kind, Name: host.Name, Scope: "srv_cleanup"}

	// Direction 1: old agent reviewed plan -> new agent refuses with explicit stale-plan error, never "unsupported", no delete.
	legacyPinned := &v1.DockerCleanupTarget{
		ServerId:     "srv_cleanup",
		Kind:         cleanupImage,
		EngineId:     imageID,
		Name:         imageID,
		Reason:       reasonUnused,
		ImageDigests: []string{pinnedRef, canonicalDigest},
		Evidence:     legacyImageCleanupEvidence(imageID, nil, []string{pinnedRef, canonicalDigest}),
	}
	legacyDangling := &v1.DockerCleanupTarget{
		ServerId: "srv_cleanup",
		Kind:     cleanupImage,
		EngineId: imageID,
		Name:     imageID,
		Reason:   reasonDangling,
		Evidence: legacyImageCleanupEvidence(imageID, nil, nil),
	}
	for _, tc := range []struct {
		name   string
		target *v1.DockerCleanupTarget
	}{
		{name: "legacy pinned image", target: legacyPinned},
		{name: "legacy dangling image", target: legacyDangling},
	} {
		t.Run(tc.name, func(t *testing.T) {
			before := len(engine.Seen())
			plan := &v1.DockerMaintenance{ChangeId: dwid.New(dwid.Change), Targets: []*v1.DockerCleanupTarget{tc.target}}
			err := c.applyDockerMaintenance(t.Context(), ref, plan, &agentv1.DockerCleanupProtection{})
			if err == nil || err.Error() != "The cleanup was reviewed by an older agent. Review cleanup again" {
				t.Fatalf("legacy plan error = %v, want explicit older-agent stale-plan error", err)
			}
			if strings.Contains(strings.ToLower(err.Error()), "unsupported") {
				t.Fatalf("legacy plan error must not say unsupported: %v", err)
			}
			if len(engine.Seen()) != before {
				t.Fatalf("legacy plan touched Docker: %v", engine.Seen()[before:])
			}
		})
	}

	// Direction 2: new agent reviewed targets never match the old agent's v1 evidence check, so a rolled-back agent refuses before deleting.
	for _, tc := range []struct {
		name string
		raw  docker.DiskUsageImage
	}{
		{name: "new pinned", raw: docker.DiskUsageImage{ID: imageID, RepoTags: []string{pinnedRef}, RepoDigests: []string{canonicalDigest}}},
		{name: "new tagged", raw: docker.DiskUsageImage{ID: imageID, RepoTags: []string{"busybox:1.36"}}},
		{name: "new dangling", raw: docker.DiskUsageImage{ID: imageID}},
	} {
		t.Run(tc.name+" rejected by legacy validator", func(t *testing.T) {
			target := c.imageCleanupTarget(tc.raw, dockerOccupancy{}, nil)
			if target.GetEvidence() == "" {
				t.Fatalf("expected eligible new target, got %v", target)
			}
			if target.GetEvidence() == legacyImageCleanupEvidence(target.GetEngineId(), target.GetImageTags(), target.GetImageDigests()) {
				t.Fatalf("new v2 evidence matched legacy v1 evidence for %s", tc.name)
			}
			if isLegacyImageCleanupTarget(target) {
				t.Fatalf("new v2 target was misclassified as legacy: %v", target)
			}
		})
	}
}

func TestDockerCleanupFailedJournalAppendDoesNotReportOrDeleteOnReplay(t *testing.T) {
	if os.Geteuid() == 0 {
		t.Skip("chmod 0500 does not block root writes")
	}
	var cacheInUse atomic.Bool
	var pruned atomic.Bool
	var makeDirReadOnly atomic.Bool
	var cleanupDir string

	used := time.Now().UTC().Truncate(time.Second).Add(-time.Hour)
	cacheID := strings.Repeat("b", 25)
	busyImgID := "sha256:" + strings.Repeat("a", 64)
	var imgInUse atomic.Bool

	sockDir, err := os.MkdirTemp("", "dw-jfail-")
	if err != nil {
		t.Fatal(err)
	}
	if len(filepath.Join(sockDir, "d.sock")) >= 100 {
		_ = os.RemoveAll(sockDir)
		sockDir, err = os.MkdirTemp("/tmp", "dw-jfail-")
		if err != nil {
			t.Fatal(err)
		}
	}
	t.Cleanup(func() { _ = os.RemoveAll(sockDir) })
	sock := filepath.Join(sockDir, "d.sock")
	rootDir := t.TempDir()
	ln, err := net.Listen("unix", sock)
	if err != nil {
		t.Fatal(err)
	}
	srv := &http.Server{Handler: http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		path := r.URL.Path
		if i := strings.Index(path, "/"); i == 0 && strings.HasPrefix(path, "/v1.") {
			if j := strings.Index(path[1:], "/"); j >= 0 {
				path = path[1+j:]
			}
		}
		switch {
		case r.Method == http.MethodGet && path == "/version":
			_ = json.NewEncoder(w).Encode(map[string]any{"ApiVersion": "1.52", "Version": "29.4.1", "Platform": map[string]string{"Name": "Docker Engine - Community"}})
		case r.Method == http.MethodGet && path == "/info":
			_ = json.NewEncoder(w).Encode(map[string]any{"DockerRootDir": rootDir, "Swarm": map[string]any{"LocalNodeState": "inactive"}})
		case r.Method == http.MethodGet && path == "/containers/json":
			ctrs := []any{}
			if imgInUse.Load() {
				ctrs = append(ctrs, map[string]any{"Id": strings.Repeat("1", 64), "Image": busyImgID, "ImageID": busyImgID, "State": "running", "NetworkSettings": map[string]any{}})
			}
			_ = json.NewEncoder(w).Encode(ctrs)
		case r.Method == http.MethodGet && path == "/system/df":
			if r.URL.Query().Get("type") == "build-cache" && makeDirReadOnly.CompareAndSwap(true, false) {
				// Trigger on target B's final BuildCacheUsage (/system/df?type=build-cache) after target A's Changed receipt is durable.
				cacheInUse.Store(true)
				_ = os.Chmod(cleanupDir, 0o500)
			}
			_ = json.NewEncoder(w).Encode(docker.DiskUsage{
				Images:     []docker.DiskUsageImage{{ID: busyImgID, Size: 1024}},
				BuildCache: []docker.DiskUsageCache{{ID: cacheID, Type: "regular", CreatedAt: used, LastUsedAt: &used, Size: 512, InUse: cacheInUse.Load()}},
			})
		case r.Method == http.MethodGet && path == "/images/"+busyImgID+"/json":
			_ = json.NewEncoder(w).Encode(map[string]any{"Id": busyImgID, "Config": map[string]any{}, "Size": 1024})
		case r.Method == http.MethodPost && path == "/build/prune":
			pruned.Store(true)
			_ = json.NewEncoder(w).Encode(map[string]any{"CachesDeleted": []string{cacheID}})
		default:
			w.WriteHeader(http.StatusNotFound)
		}
	})}
	go func() { _ = srv.Serve(ln) }()
	t.Cleanup(func() { _ = srv.Close() })

	live := cleanupCalls(t, docker.New(sock))
	cleanupDir = filepath.Join(live.cfg.Dir, "docker-cleanup")
	t.Cleanup(func() { _ = os.Chmod(cleanupDir, 0o700) })

	targets := cleanupCandidates(t, live, nil)
	// Order [A=image, B=cache] so A writes a durable Changed receipt and B fails its Changed append.
	slices.SortFunc(targets, func(a, b *v1.DockerCleanupTarget) int {
		if a.GetKind() == cleanupImage && b.GetKind() != cleanupImage {
			return -1
		}
		if a.GetKind() != cleanupImage && b.GetKind() == cleanupImage {
			return 1
		}
		return 0
	})
	if len(targets) != 2 || targets[0].GetKind() != cleanupImage || targets[1].GetKind() != cleanupCache {
		t.Fatalf("targets = %v, want [image, cache]", targets)
	}
	for _, target := range targets {
		target.ServerId = "srv_cleanup"
	}
	plan := &v1.DockerMaintenance{ChangeId: dwid.New(dwid.Change), Targets: targets}
	ref := resource.Ref{Kind: host.Kind, Name: host.Name, Scope: "srv_cleanup"}

	// Make A in-use before apply so A gets a durable Changed receipt, then trigger journal failure on B.
	imgInUse.Store(true)
	makeDirReadOnly.Store(true)
	var reported []*v1.DockerReceipt
	ctx := resource.WithOutcome(t.Context(), func(out resource.Outcome) { reported = out.DockerReceipts })
	err = live.applyDockerMaintenance(ctx, ref, plan, &agentv1.DockerCleanupProtection{})
	if err == nil {
		t.Fatal("expected journal write error on target B")
	}
	// Only A (which reached the journal) may be reported; B must not be reported.
	if len(reported) != 1 || reported[0].GetTarget().GetEngineId() != busyImgID || reported[0].GetState() != receiptChanged {
		t.Fatalf("reported receipts after failed append = %v, want only durable [A(CHANGED)]", reported)
	}

	// Restore journal writability and make B eligible again. Replay must NOT delete B (same process or restarted agent).
	if err := os.Chmod(cleanupDir, 0o700); err != nil {
		t.Fatal(err)
	}
	cacheInUse.Store(false)
	for _, tc := range []struct {
		name  string
		agent *calls
	}{
		{name: "same agent replay", agent: live},
		{name: "restarted agent replay", agent: func() *calls {
			restarted := cleanupCalls(t, docker.New(sock))
			restarted.cfg.Dir = live.cfg.Dir
			return restarted
		}()},
	} {
		t.Run(tc.name, func(t *testing.T) {
			var replayReported []*v1.DockerReceipt
			rctx := resource.WithOutcome(t.Context(), func(out resource.Outcome) { replayReported = out.DockerReceipts })
			if err := tc.agent.applyDockerMaintenance(rctx, ref, plan, &agentv1.DockerCleanupProtection{}); err == nil {
				t.Fatal("replay after failed append succeeded; expected review-again error")
			}
			if pruned.Load() {
				t.Fatal("replay deleted target B after its Changed receipt failed to append")
			}
			if len(replayReported) != 1 || replayReported[0].GetTarget().GetEngineId() != busyImgID {
				t.Fatalf("replay reported = %v, want only durable [A(CHANGED)]", replayReported)
			}
		})
	}
}

func TestDockerCleanupCacheChangedReasonDistinguishesNewerFromOtherDrift(t *testing.T) {
	c, engine := cleanupFixture(t)
	used := time.Now().UTC().Truncate(time.Second).Add(-time.Hour)
	rec := docker.DiskUsageCache{ID: strings.Repeat("d", 25), CreatedAt: used, LastUsedAt: &used, Size: 512, UsageCount: 1}
	engine.SetBuildCache(rec)

	targets := cleanupCandidates(t, c, &v1.DockerCleanupFilters{Kinds: []v1.DockerCleanupKind{cleanupCache}})
	if len(targets) != 1 {
		t.Fatalf("cache candidates = %v", targets)
	}

	// Size changes while LastUsedAt stays identical -> plain CHANGED (reason UNSPECIFIED), not NEWER.
	resized := rec
	resized.Size = 1024
	engine.SetBuildCache(resized)
	receipts, err := applyCleanupFixture(t, c, targets, nil)
	if err != nil || len(receipts) != 1 || receipts[0].GetState() != receiptChanged || receipts[0].GetReason() != 0 {
		t.Fatalf("resized idle cache receipt = %v, %v, want CHANGED with UNSPECIFIED reason", receipts, err)
	}

	// LastUsedAt advances -> CHANGED with NEWER.
	later := used.Add(5 * time.Minute)
	newer := rec
	newer.LastUsedAt = &later
	engine.SetBuildCache(newer)
	receipts, err = applyCleanupFixture(t, c, targets, nil)
	if err != nil || len(receipts) != 1 || receipts[0].GetState() != receiptChanged || receipts[0].GetReason() != receiptReasonNewer {
		t.Fatalf("newer idle cache receipt = %v, %v, want CHANGED with NEWER reason", receipts, err)
	}

	// Absent reviewed LastUsedAt becoming present at apply -> plain CHANGED (UNSPECIFIED reason), not NEWER.
	noUseRec := docker.DiskUsageCache{ID: strings.Repeat("e", 25), CreatedAt: used, LastUsedAt: nil, Size: 512, UsageCount: 0}
	engine.SetBuildCache(noUseRec)
	noUseTargets := cleanupCandidates(t, c, &v1.DockerCleanupFilters{Kinds: []v1.DockerCleanupKind{cleanupCache}})
	if len(noUseTargets) != 1 || noUseTargets[0].GetLastUsedAt() != nil {
		t.Fatalf("absent LastUsedAt cache candidates = %v", noUseTargets)
	}
	withUse := noUseRec
	withUse.LastUsedAt = &used
	engine.SetBuildCache(withUse)
	receipts, err = applyCleanupFixture(t, c, noUseTargets, nil)
	if err != nil || len(receipts) != 1 || receipts[0].GetState() != receiptChanged || receipts[0].GetReason() != 0 {
		t.Fatalf("absent-to-present LastUsedAt cache receipt = %v, %v, want CHANGED with UNSPECIFIED reason", receipts, err)
	}
}

func TestDockerCleanupLibrarySubpathTagNotRewritten(t *testing.T) {
	c, _ := cleanupFixture(t)
	imageID := "sha256:" + strings.Repeat("7", 64)

	t.Run("library/team/widget stays distinct from team/widget", func(t *testing.T) {
		mixed := c.imageCleanupTarget(docker.DiskUsageImage{
			ID:          imageID,
			RepoTags:    []string{"library/team/widget:stable"},
			RepoDigests: []string{"team/widget@" + imageID},
		}, dockerOccupancy{}, nil)
		if mixed.GetReason() != v1.DockerCleanupReason_DOCKER_CLEANUP_REASON_MULTIPLE_REPOSITORIES || mixed.GetEvidence() != "" {
			t.Fatalf("library/team/widget + team/widget = %v, want MULTIPLE_REPOSITORIES", mixed)
		}
	})

	for _, reportedTag := range []string{
		"library/team/widget:stable",
		"docker.io/library/team/widget:stable",
	} {
		t.Run("classic tag "+reportedTag+" previews and removes without rewriting lookup", func(t *testing.T) {
			var deleted atomic.Bool
			var inspectedExact atomic.Bool
			var inspectedRewritten atomic.Bool
			sockDir, err := os.MkdirTemp("", "dw-lib-")
			if err != nil {
				t.Fatal(err)
			}
			if len(filepath.Join(sockDir, "d.sock")) >= 100 {
				_ = os.RemoveAll(sockDir)
				sockDir, err = os.MkdirTemp("/tmp", "dw-lib-")
				if err != nil {
					t.Fatal(err)
				}
			}
			t.Cleanup(func() { _ = os.RemoveAll(sockDir) })
			sock := filepath.Join(sockDir, "d.sock")
			rootDir := t.TempDir()
			ln, err := net.Listen("unix", sock)
			if err != nil {
				t.Fatal(err)
			}
			srv := &http.Server{Handler: http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				w.Header().Set("Content-Type", "application/json")
				path := r.URL.Path
				if i := strings.Index(path, "/"); i == 0 && strings.HasPrefix(path, "/v1.") {
					if j := strings.Index(path[1:], "/"); j >= 0 {
						path = path[1+j:]
					}
				}
				switch {
				case r.Method == http.MethodGet && path == "/version":
					_ = json.NewEncoder(w).Encode(map[string]any{"ApiVersion": "1.52", "Version": "29.4.1", "Platform": map[string]string{"Name": "Docker Engine - Community"}})
				case r.Method == http.MethodGet && path == "/info":
					_ = json.NewEncoder(w).Encode(map[string]any{"DockerRootDir": rootDir, "Swarm": map[string]any{"LocalNodeState": "inactive"}})
				case r.Method == http.MethodGet && path == "/containers/json":
					_ = json.NewEncoder(w).Encode([]any{})
				case r.Method == http.MethodGet && path == "/system/df":
					var images []docker.DiskUsageImage
					if !deleted.Load() {
						images = []docker.DiskUsageImage{{
							ID:       imageID,
							Size:     4096,
							RepoTags: []string{reportedTag},
						}}
					}
					_ = json.NewEncoder(w).Encode(docker.DiskUsage{Images: images})
				case r.Method == http.MethodGet && strings.HasPrefix(path, "/images/") && strings.HasSuffix(path, "/json"):
					ref := strings.TrimSuffix(strings.TrimPrefix(path, "/images/"), "/json")
					if ref == "team/widget:stable" {
						inspectedRewritten.Store(true)
					}
					if deleted.Load() || (ref != imageID && ref != reportedTag) {
						w.WriteHeader(http.StatusNotFound)
						_ = json.NewEncoder(w).Encode(map[string]string{"message": "No such image"})
						return
					}
					if ref == reportedTag {
						inspectedExact.Store(true)
					}
					_ = json.NewEncoder(w).Encode(map[string]any{
						"Id":       imageID,
						"RepoTags": []string{reportedTag},
						"Config":   map[string]any{},
						"Size":     4096,
					})
				case r.Method == http.MethodDelete && path == "/images/"+imageID:
					deleted.Store(true)
					_ = json.NewEncoder(w).Encode([]map[string]string{{"Deleted": imageID}})
				default:
					w.WriteHeader(http.StatusNotFound)
				}
			})}
			go func() { _ = srv.Serve(ln) }()
			t.Cleanup(func() { _ = srv.Close() })

			live := cleanupCalls(t, docker.New(sock))
			targets := cleanupCandidates(t, live, &v1.DockerCleanupFilters{TaggedImages: true})
			if len(targets) != 1 || !slices.Equal(targets[0].GetImageTags(), []string{reportedTag}) ||
				!slices.Equal(targets[0].GetImageRecords(), []string{"library/team/widget:stable"}) {
				t.Fatalf("preview targets = %v, want exact tag %q and normalized record library/team/widget:stable", targets, reportedTag)
			}
			receipts, err := applyCleanupFixture(t, live, targets, &agentv1.DockerCleanupProtection{})
			if err != nil || len(receipts) != 1 || receipts[0].GetState() != receiptRemoved ||
				!deleted.Load() || !inspectedExact.Load() || inspectedRewritten.Load() {
				t.Fatalf("apply = %v, %v (deleted=%t exact=%t rewritten=%t)", receipts, err, deleted.Load(), inspectedExact.Load(), inspectedRewritten.Load())
			}
		})
	}
}

func TestDockerCleanupApplyWithField13Dropped(t *testing.T) {
	imageID := "sha256:" + strings.Repeat("8", 64)
	for _, tc := range []struct {
		name           string
		dfTags         []string
		dfDigests      []string
		inspTags       []string
		inspDigests    []string
		changedTags    []string
		changedDigests []string
	}{
		{
			name:           "pinned containerd image",
			dfTags:         []string{"busybox:1.36@" + imageID},
			dfDigests:      []string{"busybox@" + imageID},
			inspTags:       []string{"busybox:1.36@" + imageID},
			inspDigests:    []string{"busybox:1.36@" + imageID},
			changedTags:    []string{"busybox:1.36.1@" + imageID},
			changedDigests: []string{"busybox:1.36.1@" + imageID},
		},
		{
			name:           "tagged classic image",
			dfTags:         []string{"library/team/widget:stable"},
			inspTags:       []string{"library/team/widget:stable"},
			changedTags:    []string{"library/team/widget:next"},
			changedDigests: nil,
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			for _, mutate := range []bool{false, true} {
				mode := "unchanged removes"
				if mutate {
					mode = "changed refuses REFERENCE_MOVED"
				}
				t.Run(mode, func(t *testing.T) {
					var changed atomic.Bool
					var deleted atomic.Bool
					sockDir, err := os.MkdirTemp("", "dw-f13-")
					if err != nil {
						t.Fatal(err)
					}
					if len(filepath.Join(sockDir, "d.sock")) >= 100 {
						_ = os.RemoveAll(sockDir)
						sockDir, err = os.MkdirTemp("/tmp", "dw-f13-")
						if err != nil {
							t.Fatal(err)
						}
					}
					t.Cleanup(func() { _ = os.RemoveAll(sockDir) })
					sock := filepath.Join(sockDir, "d.sock")
					rootDir := t.TempDir()
					ln, err := net.Listen("unix", sock)
					if err != nil {
						t.Fatal(err)
					}
					srv := &http.Server{Handler: http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
						w.Header().Set("Content-Type", "application/json")
						path := r.URL.Path
						if i := strings.Index(path, "/"); i == 0 && strings.HasPrefix(path, "/v1.") {
							if j := strings.Index(path[1:], "/"); j >= 0 {
								path = path[1+j:]
							}
						}
						switch {
						case r.Method == http.MethodGet && path == "/version":
							_ = json.NewEncoder(w).Encode(map[string]any{"ApiVersion": "1.52", "Version": "29.4.1", "Platform": map[string]string{"Name": "Docker Engine - Community"}})
						case r.Method == http.MethodGet && path == "/info":
							_ = json.NewEncoder(w).Encode(map[string]any{"DockerRootDir": rootDir, "Swarm": map[string]any{"LocalNodeState": "inactive"}})
						case r.Method == http.MethodGet && path == "/containers/json":
							_ = json.NewEncoder(w).Encode([]any{})
						case r.Method == http.MethodGet && path == "/system/df":
							var images []docker.DiskUsageImage
							if !deleted.Load() {
								tags, digests := tc.dfTags, tc.dfDigests
								if changed.Load() {
									tags, digests = tc.changedTags, tc.dfDigests
								}
								images = []docker.DiskUsageImage{{
									ID: imageID, Size: 4096, RepoTags: tags, RepoDigests: digests,
								}}
							}
							_ = json.NewEncoder(w).Encode(docker.DiskUsage{Images: images})
						case r.Method == http.MethodGet && strings.HasPrefix(path, "/images/") && strings.HasSuffix(path, "/json"):
							ref := strings.TrimSuffix(strings.TrimPrefix(path, "/images/"), "/json")
							if deleted.Load() {
								w.WriteHeader(http.StatusNotFound)
								_ = json.NewEncoder(w).Encode(map[string]string{"message": "No such image"})
								return
							}
							tags, digests := tc.inspTags, tc.inspDigests
							if changed.Load() {
								tags, digests = tc.changedTags, tc.changedDigests
								if ref != imageID && !slices.Contains(tags, ref) && !slices.Contains(tc.dfDigests, ref) {
									w.WriteHeader(http.StatusNotFound)
									_ = json.NewEncoder(w).Encode(map[string]string{"message": "No such image"})
									return
								}
							}
							_ = json.NewEncoder(w).Encode(map[string]any{
								"Id":          imageID,
								"RepoTags":    tags,
								"RepoDigests": digests,
								"Config":      map[string]any{},
								"Size":        4096,
							})
						case r.Method == http.MethodDelete && path == "/images/"+imageID:
							deleted.Store(true)
							_ = json.NewEncoder(w).Encode([]map[string]string{{"Deleted": imageID}})
						default:
							w.WriteHeader(http.StatusNotFound)
						}
					})}
					go func() { _ = srv.Serve(ln) }()
					t.Cleanup(func() { _ = srv.Close() })

					live := cleanupCalls(t, docker.New(sock))
					targets := cleanupCandidates(t, live, &v1.DockerCleanupFilters{TaggedImages: true})
					if len(targets) != 1 || len(targets[0].GetImageRecords()) == 0 {
						t.Fatalf("preview targets = %v, want 1 target with image_records", targets)
					}
					// Simulate an older control plane stripping unknown field 13 on JSON persistence.
					targets[0].ImageRecords = nil
					if mutate {
						changed.Store(true)
					}
					receipts, err := applyCleanupFixture(t, live, targets, &agentv1.DockerCleanupProtection{})
					if mutate {
						if err != nil || len(receipts) != 1 || receipts[0].GetState() != receiptChanged ||
							receipts[0].GetReason() != receiptReasonReferenceMoved || deleted.Load() {
							t.Fatalf("dropped field 13 changed apply = %v, %v (deleted=%t), want CHANGED/REFERENCE_MOVED and no delete", receipts, err, deleted.Load())
						}
					} else {
						if err != nil || len(receipts) != 1 || receipts[0].GetState() != receiptRemoved || !deleted.Load() {
							t.Fatalf("dropped field 13 unchanged apply = %v, %v (deleted=%t), want REMOVED", receipts, err, deleted.Load())
						}
					}
				})
			}
		})
	}
}

func TestDockerCleanupFailedFinalCacheCheckPersistsAndBlocksReplay(t *testing.T) {
	var failBuildCacheDF atomic.Bool
	var pruned atomic.Bool
	used := time.Now().UTC().Truncate(time.Second).Add(-time.Hour)
	cacheID := strings.Repeat("f", 25)

	sockDir, err := os.MkdirTemp("", "dw-cfail-")
	if err != nil {
		t.Fatal(err)
	}
	if len(filepath.Join(sockDir, "d.sock")) >= 100 {
		_ = os.RemoveAll(sockDir)
		sockDir, err = os.MkdirTemp("/tmp", "dw-cfail-")
		if err != nil {
			t.Fatal(err)
		}
	}
	t.Cleanup(func() { _ = os.RemoveAll(sockDir) })
	sock := filepath.Join(sockDir, "d.sock")
	rootDir := t.TempDir()
	ln, err := net.Listen("unix", sock)
	if err != nil {
		t.Fatal(err)
	}
	srv := &http.Server{Handler: http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		path := r.URL.Path
		if i := strings.Index(path, "/"); i == 0 && strings.HasPrefix(path, "/v1.") {
			if j := strings.Index(path[1:], "/"); j >= 0 {
				path = path[1+j:]
			}
		}
		switch {
		case r.Method == http.MethodGet && path == "/version":
			_ = json.NewEncoder(w).Encode(map[string]any{"ApiVersion": "1.52", "Version": "29.4.1", "Platform": map[string]string{"Name": "Docker Engine - Community"}})
		case r.Method == http.MethodGet && path == "/info":
			_ = json.NewEncoder(w).Encode(map[string]any{"DockerRootDir": rootDir, "Swarm": map[string]any{"LocalNodeState": "inactive"}})
		case r.Method == http.MethodGet && path == "/containers/json":
			_ = json.NewEncoder(w).Encode([]any{})
		case r.Method == http.MethodGet && path == "/system/df":
			if r.URL.Query().Get("type") == "build-cache" && failBuildCacheDF.Load() {
				w.WriteHeader(http.StatusInternalServerError)
				_ = json.NewEncoder(w).Encode(map[string]string{"message": "build cache scan failed"})
				return
			}
			_ = json.NewEncoder(w).Encode(docker.DiskUsage{
				BuildCache: []docker.DiskUsageCache{{ID: cacheID, Type: "regular", CreatedAt: used, LastUsedAt: &used, Size: 512}},
			})
		case r.Method == http.MethodPost && path == "/build/prune":
			pruned.Store(true)
			_ = json.NewEncoder(w).Encode(map[string]any{"CachesDeleted": []string{cacheID}})
		default:
			w.WriteHeader(http.StatusNotFound)
		}
	})}
	go func() { _ = srv.Serve(ln) }()
	t.Cleanup(func() { _ = srv.Close() })

	live := cleanupCalls(t, docker.New(sock))
	targets := cleanupCandidates(t, live, &v1.DockerCleanupFilters{Kinds: []v1.DockerCleanupKind{cleanupCache}})
	if len(targets) != 1 {
		t.Fatalf("cache candidates = %v, want 1", targets)
	}
	targets[0].ServerId = "srv_cleanup"
	plan := &v1.DockerMaintenance{ChangeId: dwid.New(dwid.Change), Targets: targets}
	ref := resource.Ref{Kind: host.Kind, Name: host.Name, Scope: "srv_cleanup"}

	failBuildCacheDF.Store(true)
	var reported []*v1.DockerReceipt
	ctx := resource.WithOutcome(t.Context(), func(out resource.Outcome) { reported = out.DockerReceipts })
	if err := live.applyDockerMaintenance(ctx, ref, plan, nil); err == nil {
		t.Fatal("expected error when final BuildCacheUsage fails")
	}
	if len(reported) != 1 || reported[0].GetState() != receiptFailed || pruned.Load() {
		t.Fatalf("failed final cache check receipts = %v (pruned=%t), want [FAILED] and no prune", reported, pruned.Load())
	}

	// Restore Engine health and replay on both the same agent and a restarted agent: replay must refuse and never prune.
	failBuildCacheDF.Store(false)
	for _, tc := range []struct {
		name  string
		agent *calls
	}{
		{name: "same agent replay", agent: live},
		{name: "restarted agent replay", agent: func() *calls {
			restarted := cleanupCalls(t, docker.New(sock))
			restarted.cfg.Dir = live.cfg.Dir
			return restarted
		}()},
	} {
		t.Run(tc.name, func(t *testing.T) {
			var replayReported []*v1.DockerReceipt
			rctx := resource.WithOutcome(t.Context(), func(out resource.Outcome) { replayReported = out.DockerReceipts })
			if err := tc.agent.applyDockerMaintenance(rctx, ref, plan, nil); err == nil {
				t.Fatal("replay after durable Failed cache receipt succeeded; want refusal")
			}
			if pruned.Load() {
				t.Fatal("replay pruned build cache after durable Failed receipt")
			}
			if len(replayReported) != 1 || replayReported[0].GetState() != receiptFailed {
				t.Fatalf("replay receipts = %v, want [FAILED]", replayReported)
			}
		})
	}
}

func TestDockerCleanupPartialBatchRemovesUnchangedTargets(t *testing.T) {
	c, engine := cleanupFixture(t)
	client, ctx := engine.Client(), t.Context()

	engine.AddLocalImage("", dockertest.Image{Size: 1024})
	engine.AddLocalImage("fixture/busy:1", dockertest.Image{Size: 2048})
	engine.AddLocalImage("fixture/gone:1", dockertest.Image{Size: 3072})
	busyImg, err := client.InspectImage(ctx, "fixture/busy:1")
	if err != nil {
		t.Fatal(err)
	}
	goneImg, err := client.InspectImage(ctx, "fixture/gone:1")
	if err != nil {
		t.Fatal(err)
	}
	netID, err := client.CreateNetwork(ctx, &docker.NetworkCreate{Name: "fixture-keep-going"})
	if err != nil {
		t.Fatal(err)
	}
	used := time.Now().UTC().Truncate(time.Second).Add(-time.Hour)
	cacheRec := docker.DiskUsageCache{ID: strings.Repeat("c", 25), CreatedAt: used, LastUsedAt: &used, Size: 512}
	engine.SetBuildCache(cacheRec)

	targets := cleanupCandidates(t, c, &v1.DockerCleanupFilters{TaggedImages: true, Networks: true})
	if len(targets) != 5 {
		t.Fatalf("candidates = %d, want 5", len(targets))
	}
	// Mutate three targets in different ways while leaving the dangling image and network untouched.
	engine.AddContainer(docker.Container{
		Name: "/new-user", Image: busyImg.ID, Config: docker.Config{Image: busyImg.ID}, State: docker.State{Status: "exited"},
	})
	if err := client.RemoveReviewedImage(ctx, goneImg.ID); err != nil {
		t.Fatal(err)
	}
	newer := used.Add(10 * time.Minute)
	updatedCache := cacheRec
	updatedCache.LastUsedAt = &newer
	engine.SetBuildCache(updatedCache)

	plan := &v1.DockerMaintenance{ChangeId: dwid.New(dwid.Change), Targets: targets}
	var receipts []*v1.DockerReceipt
	outCtx := resource.WithOutcome(ctx, func(out resource.Outcome) { receipts = out.DockerReceipts })
	ref := resource.Ref{Kind: host.Kind, Name: host.Name, Scope: "srv_cleanup"}
	if err := c.applyDockerMaintenance(outCtx, ref, plan, &agentv1.DockerCleanupProtection{}); err != nil {
		t.Fatalf("partial batch with only Changed targets should end done, got err: %v", err)
	}
	if len(receipts) != 5 {
		t.Fatalf("receipts = %d, want 5", len(receipts))
	}
	byID := map[string]*v1.DockerReceipt{}
	for _, r := range receipts {
		byID[r.GetTarget().GetEngineId()] = r
	}
	if r := byID[busyImg.ID]; r.GetState() != receiptChanged || r.GetReason() != receiptReasonInUse {
		t.Fatalf("in-use image receipt = %v, want CHANGED/IN_USE", r)
	}
	if r := byID[goneImg.ID]; r.GetState() != receiptChanged || r.GetReason() != receiptReasonGone {
		t.Fatalf("gone image receipt = %v, want CHANGED/GONE", r)
	}
	if r := byID[cacheRec.ID]; r.GetState() != receiptChanged || r.GetReason() != receiptReasonNewer {
		t.Fatalf("newer cache receipt = %v, want CHANGED/NEWER", r)
	}
	if r := byID[netID]; r.GetState() != receiptRemoved {
		t.Fatalf("unchanged network receipt = %v, want REMOVED", r)
	}
	var removedDangling int
	for _, r := range receipts {
		if r.GetTarget().GetKind() == cleanupImage && r.GetTarget().GetReason() == reasonDangling && r.GetState() == receiptRemoved {
			removedDangling++
		}
	}
	if removedDangling != 1 {
		t.Fatalf("unchanged dangling image was not removed: %v", receipts)
	}
	if _, err := client.InspectImage(ctx, busyImg.ID); err != nil {
		t.Fatalf("changed in-use image was removed: %v", err)
	}
	if _, err := client.InspectNetwork(ctx, netID); !docker.IsNotFound(err) {
		t.Fatalf("unchanged network still exists: %v", err)
	}

	// Replaying the completed partial batch succeeds without dispatching any new deletes.
	beforeReplay := len(engine.Seen())
	receipts = nil
	if err := c.applyDockerMaintenance(outCtx, ref, plan, &agentv1.DockerCleanupProtection{}); err != nil || len(receipts) != 5 {
		t.Fatalf("replay of settled partial batch = %v, %v", receipts, err)
	}
	for _, req := range engine.Seen()[beforeReplay:] {
		if strings.HasPrefix(req, "DELETE ") || strings.HasSuffix(req, "/build/prune") {
			t.Fatalf("replay dispatched %s", req)
		}
	}
}

func TestFamiliarImageNameFollowsDockersRules(t *testing.T) {
	for in, want := range map[string]string{
		"busybox":                          "busybox",
		"library/busybox":                  "busybox",
		"docker.io/library/busybox":        "busybox",
		"index.docker.io/library/busybox":  "busybox",
		"docker.io/library/team/widget":    "library/team/widget",
		"library/team/widget":              "library/team/widget",
		"team/widget":                      "team/widget",
		"docker.io/team/widget":            "team/widget",
		"ghcr.io/team/widget":              "ghcr.io/team/widget",
		"localhost/widget":                 "localhost/widget",
		"registry.example.com:5000/widget": "registry.example.com:5000/widget",
	} {
		if got := familiarImageName(in); got != want {
			t.Errorf("familiarImageName(%q) = %q, want %q", in, got, want)
		}
	}
	for in, want := range map[string]string{
		"busybox:1.36@sha256:abc":                     "busybox:1.36@sha256:abc",
		"docker.io/library/busybox:1.36":              "busybox:1.36",
		"registry.example.com:5000/widget@sha256:abc": "registry.example.com:5000/widget@sha256:abc",
	} {
		if got := canonicalImageRecord(in); got != want {
			t.Errorf("canonicalImageRecord(%q) = %q, want %q", in, got, want)
		}
	}
	if got := canonicalDigestRef("docker.io/library/busybox:1.36@sha256:abc"); got != "busybox@sha256:abc" {
		t.Errorf("canonicalDigestRef = %q", got)
	}
	if got := pinnedTagName("busybox:1.36@sha256:abc"); got != "busybox:1.36" {
		t.Errorf("pinnedTagName = %q", got)
	}
}
