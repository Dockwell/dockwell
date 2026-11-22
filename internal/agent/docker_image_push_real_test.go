// SPDX-License-Identifier: AGPL-3.0-only

package agent

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"strings"
	"testing"
	"time"

	agentv1 "github.com/dockwell/dockwell/gen/dockwell/agent/v1"
	v1 "github.com/dockwell/dockwell/gen/dockwell/v1"
	"github.com/dockwell/dockwell/internal/agent/artifact"
	"github.com/dockwell/dockwell/internal/delivery"
	"github.com/dockwell/dockwell/internal/docker/dockertest"
	"github.com/dockwell/dockwell/internal/id"
	"github.com/dockwell/dockwell/internal/registry"
	"github.com/dockwell/dockwell/internal/resource"
	"github.com/dockwell/dockwell/internal/resource/host"
)

func TestReviewedImagePushOnRealDocker(t *testing.T) {
	d := dockertest.Real(t)
	ctx, cancel := context.WithTimeout(t.Context(), 4*time.Minute)
	defer cancel()
	f := startImageRegistry(ctx, t)
	alias := cleanupRealRepo(t) + ":alias"
	other := cleanupRealRepo(t) + ":other"
	first := importCleanupImage(t, d, alias, "reviewed image")
	second := importCleanupImage(t, d, other, "replacement image")
	c := cleanupCalls(t, d)
	var err error
	c.cfg.artifacts, err = artifact.Open(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	c.delivery, err = delivery.NewKey()
	if err != nil {
		t.Fatal(err)
	}
	transport := &dropPushResponse{next: f.http.Transport}
	f.http.Transport = transport
	c.cfg.images = newImages(d, ImageReads{}, time.Now)
	c.cfg.images.newClient = f.client
	t.Cleanup(c.cfg.images.forget)

	const owner = "fixture/session"
	ref := resource.Ref{Kind: host.Kind, Scope: c.server, Name: host.Name}
	key := func(image string) *v1.DockerImageSourceKey {
		return &v1.DockerImageSourceKey{ServerId: c.server, ImageId: image}
	}
	plan := func(image, repo, tag string) (*v1.DockerMaintenance, error) {
		t.Helper()
		facts, descriptor, err := c.prepareImagePush(ctx, owner, &v1.DockerImageExport{SourceKey: key(image)})
		if err != nil {
			t.Fatal(err)
		}
		repository := f.address + "/" + repo
		descriptor.Repository = repository
		destination := repository + ":" + tag
		if tag == "" {
			destination = repository + "@" + descriptor.ManifestDigest
		}
		p := &v1.DockerMaintenance{ChangeId: id.New(id.Change), ImageAction: &v1.DockerImageAction{
			Push:           &v1.DockerImagePush{SourceKey: key(image), Destination: destination, DigestOnly: tag == ""},
			ApprovedLogins: []*v1.DockerImageLoginRef{{Name: f.auth.Name, Host: f.address}},
			StagedSourceId: facts.ArtifactId, SourceSha256: facts.Sha256, Descriptor_: descriptor,
		}}
		if err := c.previewImagePush(ctx, ref, p, sealImageFixture(t, c, ref, p, f.auth)); err != nil {
			_, _ = c.cfg.artifacts.Abort(facts.ArtifactId, owner)
			return nil, err
		}
		if _, err := c.cfg.artifacts.Pin(&agentv1.DockerArtifactPin{ArtifactId: facts.ArtifactId, ChangeId: p.GetChangeId()}, owner); err != nil {
			t.Fatal(err)
		}
		return p, nil
	}
	apply := func(p *v1.DockerMaintenance, want v1.DockerImageOutcome) {
		t.Helper()
		receipt, err := c.applyImagePush(ctx, ref, p, sealImageFixture(t, c, ref, p, f.auth))
		if receipt.GetOutcome() != want || (want == v1.DockerImageOutcome_DOCKER_IMAGE_OUTCOME_UNKNOWN) != errors.Is(err, registry.ErrPushUnknown) ||
			err != nil && want != v1.DockerImageOutcome_DOCKER_IMAGE_OUTCOME_UNKNOWN {
			t.Fatalf("push receipt = %v, %v", receipt, err)
		}
	}
	wantPlan := func(image, repo, tag string) *v1.DockerMaintenance {
		t.Helper()
		p, err := plan(image, repo, tag)
		if err != nil {
			t.Fatal(err)
		}
		return p
	}
	verify := func(p *v1.DockerMaintenance) {
		t.Helper()
		a := p.GetImageAction()
		target, err := registry.Parse(a.GetPush().GetDestination())
		if err != nil {
			t.Fatal(err)
		}
		req, err := http.NewRequestWithContext(ctx, http.MethodGet, "https://"+f.address+"/v2/"+target.Repository+"/manifests/"+target.Reference(), http.NoBody)
		if err != nil {
			t.Fatal(err)
		}
		req.SetBasicAuth(f.auth.Username, f.auth.Password)
		req.Header.Set("Accept", registry.OCIManifest)
		r, err := f.http.Do(req)
		if err != nil {
			t.Fatal(err)
		}
		defer r.Body.Close()
		var manifest struct {
			Config struct{ Digest string }
		}
		if err := json.NewDecoder(r.Body).Decode(&manifest); err != nil || r.StatusCode != http.StatusOK ||
			r.Header.Get("Docker-Content-Digest") != a.GetDescriptor_().GetManifestDigest() ||
			manifest.Config.Digest != a.GetDescriptor_().GetConfigDigest() {
			t.Fatalf("wrong manifest: HTTP %d, %s, %v, %v", r.StatusCode, r.Header.Get("Docker-Content-Digest"), manifest, err)
		}
	}

	exact := wantPlan(first, "fixture/push", "review")
	t.Cleanup(func() {
		ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
		defer cancel()
		if err := d.UntagImage(ctx, alias); err != nil {
			t.Errorf("remove moved fixture alias: %v", err)
		}
	})
	if err := d.TagImage(ctx, second, alias); err != nil {
		t.Fatal(err)
	}
	apply(exact, v1.DockerImageOutcome_DOCKER_IMAGE_OUTCOME_APPLIED)
	verify(exact)
	apply(exact, v1.DockerImageOutcome_DOCKER_IMAGE_OUTCOME_APPLIED)
	if _, err := plan(second, "fixture/push", "review"); !errors.Is(err, registry.ErrPushConflict) {
		t.Fatalf("conflicting destination = %v", err)
	}

	claimed := wantPlan(second, "fixture/push", "claimed")
	f.moveManifest(ctx, t, "fixture/push", "review", "claimed")
	before := transport.manifests
	refused, err := c.applyImagePush(ctx, ref, claimed, sealImageFixture(t, c, ref, claimed, f.auth))
	if !errors.Is(err, registry.ErrPushConflict) || refused.GetOutcome() != v1.DockerImageOutcome_DOCKER_IMAGE_OUTCOME_REFUSED ||
		refused.GetReason() != v1.DockerImageReason_DOCKER_IMAGE_REASON_STALE || transport.manifests != before {
		t.Fatalf("tag claimed after review: %v, %v", refused, err)
	}

	byDigest := wantPlan(second, "fixture/push", "")
	apply(byDigest, v1.DockerImageOutcome_DOCKER_IMAGE_OUTCOME_APPLIED)
	verify(byDigest)

	uncertain := wantPlan(first, "fixture/push", "unconfirmed")
	transport.drop = true
	apply(uncertain, v1.DockerImageOutcome_DOCKER_IMAGE_OUTCOME_UNKNOWN)
	apply(uncertain, v1.DockerImageOutcome_DOCKER_IMAGE_OUTCOME_UNKNOWN)
	if transport.dropped != 1 {
		t.Fatalf("manifest PUT sent %d times after dropping the response", transport.dropped)
	}
	verify(uncertain)
	check := &v1.DockerMaintenance{ImageAction: &v1.DockerImageAction{
		Push:           &v1.DockerImagePush{Destination: uncertain.GetImageAction().GetPush().GetDestination()},
		ApprovedLogins: uncertain.GetImageAction().GetApprovedLogins(),
	}}
	if err := c.previewImagePush(ctx, ref, check, sealImageFixture(t, c, ref, check, f.auth)); err != nil ||
		check.GetImageAction().GetDescriptor_().GetManifestDigest() != uncertain.GetImageAction().GetDescriptor_().GetManifestDigest() {
		t.Fatalf("read-only check = %v, %v", check, err)
	}
}

type dropPushResponse struct {
	next      http.RoundTripper
	drop      bool
	dropped   int
	manifests int
}

func (d *dropPushResponse) RoundTrip(req *http.Request) (*http.Response, error) {
	response, err := d.next.RoundTrip(req)
	if req.Method != http.MethodPut || !strings.Contains(req.URL.Path, "/manifests/") {
		return response, err
	}
	d.manifests++
	if err == nil && d.drop {
		_ = response.Body.Close()
		d.dropped++
		return nil, context.DeadlineExceeded
	}
	return response, err
}
