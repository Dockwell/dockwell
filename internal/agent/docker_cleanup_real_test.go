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
