// SPDX-License-Identifier: AGPL-3.0-only

package control

import (
	"crypto/sha256"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"connectrpc.com/connect"
	"google.golang.org/protobuf/encoding/protojson"
	"google.golang.org/protobuf/proto"

	resourcev1 "github.com/dockwell/dockwell/gen/dockwell/resource/v1alpha1"
	v1 "github.com/dockwell/dockwell/gen/dockwell/v1"
	"github.com/dockwell/dockwell/gen/dockwell/v1/dockwellv1connect"
	"github.com/dockwell/dockwell/internal/agent"
	"github.com/dockwell/dockwell/internal/agent/logs"
	"github.com/dockwell/dockwell/internal/control/authapi"
	"github.com/dockwell/dockwell/internal/docker/dockertest"
	"github.com/dockwell/dockwell/internal/store"
	"github.com/dockwell/dockwell/internal/store/storetest"
)

func TestDockerTagReviewOnBothStores(t *testing.T) {
	t.Parallel()
	storetest.Each(t, func(t *testing.T, backend store.Backend) {
		f := startFull(t, backend, nil)
		b := signedIn(t, f)
		engine := f.addDockerServer(t)
		server := f.lastDocker.id.ServerID
		engine.AddLocalImage("fixture/moving:old", dockertest.Image{})
		engine.AddLocalImage("fixture/moving:new", dockertest.Image{})
		source, err := engine.Client().InspectImage(t.Context(), "fixture/moving:new")
		if err != nil {
			t.Fatal(err)
		}
		old, err := engine.Client().InspectImage(t.Context(), "fixture/moving:old")
		if err != nil {
			t.Fatal(err)
		}
		api := dockwellv1connect.NewDockerServiceClient(b, b.origin, connect.WithProtoJSON())
		changes := changeClient(b, b.origin)
		planFrom := func(imageID, tag string, move bool) (*v1.Change, error) {
			t.Helper()
			response, err := api.PlanDockerMaintenance(t.Context(), connect.NewRequest(&v1.PlanDockerMaintenanceRequest{
				ServerIds: []string{server}, Operation: &v1.PlanDockerMaintenanceRequest_Tag{Tag: &v1.DockerImageTag{
					SourceKey: &v1.DockerImageSourceKey{ServerId: server, ImageId: imageID}, Destination: tag, MoveExisting: move,
				}},
			}))
			if err != nil {
				return nil, err
			}
			return response.Msg.GetChange(), nil
		}
		plan := func(tag string, move bool) *v1.Change {
			t.Helper()
			got, err := planFrom(source.ID, tag, move)
			if err != nil {
				t.Fatal(err)
			}
			return got
		}
		inventory := func() *v1.ListDockerObjectsResponse {
			t.Helper()
			res, err := api.ListDockerObjects(t.Context(), connect.NewRequest(&v1.ListDockerObjectsRequest{
				ServerIds: []string{server}, Kind: v1.DockerObjectKind_DOCKER_OBJECT_KIND_IMAGE,
				Filter: &v1.DockerObjectFilter{Search: "fixture/moving:copy"},
			}))
			if err != nil {
				t.Fatal(err)
			}
			return res.Msg
		}
		if len(inventory().GetObjects()) != 0 {
			t.Fatal("test alias exists before tagging")
		}
		add := plan("fixture/moving:copy", false)
		if _, err := changes.ApplyChange(t.Context(), connect.NewRequest(&v1.ApplyChangeRequest{Id: add.GetId()})); reasonOf(err) != "needs_confirmation" {
			t.Fatalf("adding a tag without reviewing the fingerprint: %v", err)
		}
		token := createToken(t, b, v1.TokenScope_TOKEN_SCOPE_READ_WRITE)
		withToken := changeClient(bearer{token: token, rt: b.client.Transport}, b.origin)
		if _, err := withToken.ApplyChange(t.Context(), connect.NewRequest(&v1.ApplyChangeRequest{Id: add.GetId(), Fingerprint: add.GetPlan().GetFingerprint()})); reasonOf(err) != authapi.ReasonNeedsSession {
			t.Fatalf("adding a tag with an API token: %v", err)
		}
		applyConfirmed(t, changes, add)
		if rows := inventory().GetObjects(); len(rows) != 1 || rows[0].GetKey().GetEngineId() != source.ID {
			t.Fatalf("inventory did not refresh after tagging: %v", rows)
		}
		if ch := plan("fixture/moving:copy", false); ch != nil {
			t.Fatalf("same alias returned a change")
		}
		stale := plan("fixture/moving:drift", false)
		engine.AddLocalImage("fixture/moving:drift", dockertest.Image{})
		drifted, err := engine.Client().InspectImage(t.Context(), "fixture/moving:drift")
		if err != nil {
			t.Fatal(err)
		}
		staleWatch := watchWithin(t, changes, stale.GetId(), 15*time.Second)
		if _, err := changes.ApplyChange(t.Context(), connect.NewRequest(&v1.ApplyChangeRequest{
			Id: stale.GetId(), Fingerprint: stale.GetPlan().GetFingerprint(),
		})); err != nil {
			t.Fatal(err)
		}
		refused := waitWatchWithin(t, staleWatch, 15*time.Second).last()
		if refused.GetState() != v1.ChangeState_CHANGE_STATE_FAILED || len(refused.GetSteps()) != 1 || len(refused.GetSteps()[0].GetDockerImageReceipts()) != 1 ||
			refused.GetSteps()[0].GetDockerImageReceipts()[0].GetReason() != v1.DockerImageReason_DOCKER_IMAGE_REASON_STALE {
			t.Fatalf("an intervening alias was not refused: %v", refused)
		}
		if kept, err := engine.Client().InspectImage(t.Context(), "fixture/moving:drift"); err != nil || kept.ID != drifted.ID {
			t.Fatalf("the intervening alias was changed: %v, %v", kept, err)
		}

		move := plan("fixture/moving:old", true)
		fingerprint := move.GetPlan().GetFingerprint()
		req := &v1.ApplyChangeRequest{Id: move.GetId(), Fingerprint: fingerprint}
		if _, err := changes.ApplyChange(t.Context(), connect.NewRequest(req)); reasonOf(err) != authapi.ReasonNeedsStepUp {
			t.Fatalf("moving a tag did not require an exact passkey: %v", err)
		}
		req.Proof = stepUpFor(t, b, v1.StepUpAction_STEP_UP_ACTION_APPROVE_CHANGE, add.GetId()+":"+add.GetPlan().GetFingerprint())
		if _, err := changes.ApplyChange(t.Context(), connect.NewRequest(req)); reasonOf(err) != authapi.ReasonNeedsStepUp {
			t.Fatalf("moving a tag accepted a passkey for another change: %v", err)
		}
		req.Proof = stepUpFor(t, b, v1.StepUpAction_STEP_UP_ACTION_APPROVE_CHANGE, move.GetId()+":"+fingerprint)
		watch := watchWithin(t, changes, move.GetId(), 15*time.Second)
		if _, err := changes.ApplyChange(t.Context(), connect.NewRequest(req)); err != nil {
			t.Fatal(err)
		}
		finished := waitWatchWithin(t, watch, 15*time.Second).last()
		receipts := finished.GetSteps()[0].GetDockerImageReceipts()
		if finished.GetState() != v1.ChangeState_CHANGE_STATE_APPLIED || len(receipts) != 1 || receipts[0].GetPreviousImageId() != old.ID || receipts[0].GetImageKey().GetImageId() != source.ID {
			t.Fatalf("tag receipt = %v, %v", finished.GetState(), receipts)
		}
		if kept, err := engine.Client().InspectImage(t.Context(), old.ID); err != nil || kept.ID != old.ID {
			t.Fatalf("moving the alias destroyed its previous image: %v", err)
		}
		reverse, err := planFrom(old.ID, "fixture/moving:old", true)
		if err != nil {
			t.Fatal(err)
		}
		if _, err := changes.ApplyChange(t.Context(), connect.NewRequest(&v1.ApplyChangeRequest{
			Id: reverse.GetId(), Fingerprint: reverse.GetPlan().GetFingerprint(), Proof: req.GetProof(),
		})); reasonOf(err) != authapi.ReasonNeedsStepUp {
			t.Fatalf("a consumed passkey authorized another alias move: %v", err)
		}

		compose := dockwellv1connect.NewComposeServiceClient(b, b.origin, connect.WithProtoJSON())
		apply(t, changes, importCompose(t, compose, "name: image-pin\nservices:\n  web:\n    image: ghcr.io/acme/shop:1.4\n", nil, false))
		if _, err := planFrom(source.ID, "ghcr.io/acme/shop:1.4", true); connect.CodeOf(err) != connect.CodeFailedPrecondition || !strings.Contains(err.Error(), "a kept workload uses this tag") {
			t.Fatalf("managed alias was offered for retargeting: %v", err)
		}

		// Both reviews see the same vacant alias, but only one of them can assign it.
		const raceTag = "fixture/moving:concurrent"
		one := plan(raceTag, false)
		two, err := planFrom(old.ID, raceTag, false)
		if err != nil {
			t.Fatal(err)
		}
		before := len(engine.Seen())
		start := make(chan struct{})
		type result struct {
			change *v1.Change
			err    error
		}
		results := make(chan result, 2)
		for _, change := range []*v1.Change{one, two} {
			go func() {
				<-start
				_, err := changes.ApplyChange(t.Context(), connect.NewRequest(&v1.ApplyChangeRequest{Id: change.GetId(), Fingerprint: change.GetPlan().GetFingerprint()}))
				results <- result{change, err}
			}()
		}
		close(start)
		applied := 0
		for range 2 {
			result := <-results
			if result.err != nil {
				if reasonOf(result.err) != "busy" {
					t.Fatal(result.err)
				}
				continue
			}
			done := waitWatchWithin(t, watchWithin(t, changes, result.change.GetId(), 15*time.Second), 15*time.Second).last()
			if done.GetState() == v1.ChangeState_CHANGE_STATE_APPLIED {
				applied++
			} else if step := done.GetSteps()[0]; len(step.GetDockerImageReceipts()) != 1 || step.GetDockerImageReceipts()[0].GetReason() != v1.DockerImageReason_DOCKER_IMAGE_REASON_STALE {
				t.Fatalf("competing review didn't report a stale binding: %v", done)
			}
		}
		writes := 0
		for _, call := range engine.Seen()[before:] {
			if strings.HasPrefix(call, "POST ") && strings.HasSuffix(call, "/tag") {
				writes++
			}
		}
		if applied != 1 || writes != 1 {
			t.Fatalf("same alias got %d applied changes and %d Engine writes", applied, writes)
		}
	})
}

func TestDockerReviewedPullAndRepositoryLookup(t *testing.T) {
	t.Parallel()
	storetest.Each(t, func(t *testing.T, backend store.Backend) {
		const (
			host = "ghcr.io"
			repo = "fixture/reviewed"
			ref  = host + "/" + repo + ":edge"
		)
		type image struct{ config, manifest, id, digest string }
		build := func(version string) image {
			config := fmt.Sprintf(`{"os":"linux","architecture":"amd64","fixture":%q}`, version)
			id := fmt.Sprintf("sha256:%x", sha256.Sum256([]byte(config)))
			manifest := fmt.Sprintf(`{"schemaVersion":2,"config":{"digest":%q}}`, id)
			return image{config, manifest, id, fmt.Sprintf("sha256:%x", sha256.Sum256([]byte(manifest)))}
		}
		first, second := build("first"), build("second")
		var moved, movedRealm atomic.Bool
		var callsToMovedRealm atomic.Int32
		const tokenHost = "approved-token-host.invalid"
		const registryToken = "fixture-repository-token"
		reg := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			if r.URL.Path == "/wrong-token" {
				callsToMovedRealm.Add(1)
				w.WriteHeader(http.StatusUnauthorized)
				return
			}
			if r.URL.Path == "/token" {
				if user, password, ok := r.BasicAuth(); !ok || user != loginUser || password != loginPassword {
					w.WriteHeader(http.StatusUnauthorized)
					return
				}
				_ = json.NewEncoder(w).Encode(map[string]string{"token": registryToken})
				return
			}
			if r.Header.Get("Authorization") != "Bearer "+registryToken {
				realm := "https://" + tokenHost + "/token"
				if movedRealm.Load() && r.URL.Path == "/v2/" {
					realm = "https://different-token-host.invalid/wrong-token"
				}
				w.Header().Set("Www-Authenticate", fmt.Sprintf(`Bearer realm=%q`, realm))
				w.WriteHeader(http.StatusUnauthorized)
				return
			}
			switch r.URL.Path {
			case "/v2/":
			case "/v2/_catalog":
				if r.URL.Query().Get("last") == "" {
					w.Header().Set("Link", `</v2/_catalog?n=1&last=fixture%2Ffirst>; rel="next"`)
					_ = json.NewEncoder(w).Encode(map[string][]string{"repositories": {"fixture/first"}})
				} else {
					_ = json.NewEncoder(w).Encode(map[string][]string{"repositories": {repo}})
				}
			case "/v2/" + repo + "/manifests/edge":
				selected := first
				if moved.Load() {
					selected = second
				}
				w.Header().Set("Docker-Content-Digest", selected.digest)
				_, _ = io.WriteString(w, selected.manifest)
			case "/v2/" + repo + "/blobs/" + first.id:
				_, _ = io.WriteString(w, first.config)
			case "/v2/" + repo + "/blobs/" + second.id:
				_, _ = io.WriteString(w, second.config)
			default:
				http.NotFound(w, r)
			}
		}))
		t.Cleanup(reg.Close)
		f := startFull(t, backend, nil)
		b := signedIn(t, f)
		engine := f.addDockerServer(t)
		srv := f.lastDocker
		server := srv.id.ServerID
		srv.stop()
		waitConnected(t, f, server, false)
		for _, entry := range []image{first, second} {
			engine.Registry[host+"/"+repo+"@"+entry.digest] = dockertest.Image{ConfigDigest: entry.id, Digest: entry.digest, Login: loginUser + ":" + loginPassword}
		}
		srv.images = agent.ImageReads{Test: reg.URL}
		srv.run(t)
		waitConnected(t, f, server, true)

		changes := changeClient(b, b.origin)
		images := dockwellv1connect.NewImageServiceClient(b, b.origin, connect.WithProtoJSON())
		dockerAPI := dockwellv1connect.NewDockerServiceClient(b, b.origin, connect.WithProtoJSON())
		saveLogin := func(password string) {
			t.Helper()
			apply(t, changes, propose(t, changes, "Save fixture login", loginEdit("fixture-login", &resourcev1.RegistryLogin{
				Registry: host, Username: loginUser, Password: password, TokenHost: tokenHost,
			})))
		}
		saveLogin(loginPassword)
		catalog := func(token string) *v1.ListRegistryRepositoriesResponse {
			t.Helper()
			got, err := images.ListRegistryRepositories(t.Context(), connect.NewRequest(&v1.ListRegistryRepositoriesRequest{
				Registry: host, Server: server, NamePrefix: repo, PageToken: token, PageSize: 1,
			}))
			if err != nil || !got.Msg.GetEnumerationSupported() {
				t.Fatalf("repository lookup failed: %v", err)
			}
			return got.Msg
		}
		page := catalog("")
		if len(page.GetRepositories()) != 0 || page.GetNextPageToken() != "fixture/first" {
			t.Fatalf("filtered page lost its continuation: %v", page)
		}
		if rows := catalog(page.GetNextPageToken()).GetRepositories(); len(rows) != 1 || rows[0].GetName() != repo {
			t.Fatalf("unexpected second page: %v", rows)
		}

		plan := func(expected *string) (*v1.PlanDockerMaintenanceResponse, error) {
			got, err := dockerAPI.PlanDockerMaintenance(t.Context(), connect.NewRequest(&v1.PlanDockerMaintenanceRequest{
				ServerIds: []string{server}, Operation: &v1.PlanDockerMaintenanceRequest_Pull{Pull: &v1.DockerImagePull{
					Reference: ref, ExpectedLocalImageId: expected,
				}},
			}))
			if err != nil {
				return nil, err
			}
			if text := protojson.Format(got.Msg); strings.Contains(text, loginPassword) || strings.Contains(text, registryToken) {
				t.Fatal("the pull review contains a credential")
			}
			return got.Msg, nil
		}
		witness := func(change *v1.Change, proof string) *v1.Change {
			t.Helper()
			w := watchWithin(t, changes, change.GetId(), 15*time.Second)
			if _, err := changes.ApplyChange(t.Context(), connect.NewRequest(&v1.ApplyChangeRequest{
				Id: change.GetId(), Fingerprint: change.GetPlan().GetFingerprint(), Proof: proof,
			})); err != nil {
				t.Fatal(err)
			}
			done := waitWatchWithin(t, w, 15*time.Second).last()
			if text := protojson.Format(done); strings.Contains(text, loginPassword) || strings.Contains(text, registryToken) {
				t.Fatal("the pull outcome contains a credential")
			}
			return done
		}
		review, err := plan(nil)
		if err != nil || review.GetChange() == nil || review.GetDigestCheck().GetCandidate().GetManifestDigest() != first.digest ||
			review.GetDigestCheck().GetCandidate().GetConfigDigest() != first.id {
			t.Fatalf("pull preview failed: %v", err)
		}
		moved.Store(true)
		stale := witness(review.GetChange(), "")
		if stale.GetState() != v1.ChangeState_CHANGE_STATE_FAILED || len(stale.GetSteps()) != 1 || len(stale.GetSteps()[0].GetDockerImageReceipts()) != 1 ||
			stale.GetSteps()[0].GetDockerImageReceipts()[0].GetReason() != v1.DockerImageReason_DOCKER_IMAGE_REASON_STALE || len(engine.SentLogins()) != 0 {
			t.Fatal("a change at the registry was not refused before downloading")
		}
		moved.Store(false)
		review, err = plan(nil)
		if err != nil || review.GetChange() == nil {
			t.Fatalf("pull review before rotation: %v", err)
		}
		saveLogin(loginPassword + "-rotated")
		rotated := witness(review.GetChange(), "")
		if rotated.GetState() != v1.ChangeState_CHANGE_STATE_FAILED || !strings.Contains(rotated.GetError(), "registry login changed") || len(engine.SentLogins()) != 0 {
			t.Fatalf("a rotated login reached Docker: %v", rotated)
		}
		saveLogin(loginPassword)
		review, err = plan(nil)
		if err != nil || review.GetChange() == nil {
			t.Fatalf("pull review before changed token service: %v", err)
		}
		movedRealm.Store(true)
		changedRealm := witness(review.GetChange(), "")
		if changedRealm.GetState() != v1.ChangeState_CHANGE_STATE_FAILED || len(changedRealm.GetSteps()) != 1 || len(changedRealm.GetSteps()[0].GetDockerImageReceipts()) != 1 ||
			changedRealm.GetSteps()[0].GetDockerImageReceipts()[0].GetReason() != v1.DockerImageReason_DOCKER_IMAGE_REASON_CREDENTIAL_REFUSED ||
			len(engine.SentLogins()) != 0 || callsToMovedRealm.Load() != 0 {
			t.Fatalf("a login escaped the reviewed token host: %v", changedRealm)
		}
		movedRealm.Store(false)
		review, err = plan(nil)
		if err != nil || review.GetChange() == nil {
			t.Fatalf("new pull review failed: %v", err)
		}
		assertReceipt := func(done *v1.Change, pulled image, previous string) {
			t.Helper()
			if done.GetState() != v1.ChangeState_CHANGE_STATE_APPLIED || len(done.GetSteps()) != 1 {
				t.Fatalf("pull did not finish: %v", done)
			}
			receipts := done.GetSteps()[0].GetDockerImageReceipts()
			if len(receipts) != 1 || receipts[0].GetImageKey().GetImageId() != pulled.id || receipts[0].GetDigest() != pulled.digest || receipts[0].GetPreviousImageId() != previous {
				t.Fatalf("incorrect pull receipt: %v", receipts)
			}
		}
		assertReceipt(witness(review.GetChange(), ""), first, "")
		current, err := plan(&first.id)
		if err != nil || current.GetChange() != nil || !current.GetDigestCheck().GetSameContent() || current.GetDigestCheck().GetCurrent().GetManifestDigest() != first.digest {
			t.Fatalf("the installed manifest was not up to date: %v", err)
		}
		moved.Store(true)
		review, err = plan(&first.id)
		change := review.GetChange()
		if err != nil || change == nil || review.GetDigestCheck().GetCurrent().GetConfigDigest() != first.id ||
			review.GetDigestCheck().GetCurrent().GetManifestDigest() != "" || review.GetDigestCheck().GetCandidate().GetManifestDigest() != second.digest {
			t.Fatalf("updated registry tag was not compared: %v", err)
		}
		fp := change.GetPlan().GetFingerprint()
		if _, err := changes.ApplyChange(t.Context(), connect.NewRequest(&v1.ApplyChangeRequest{Id: change.GetId(), Fingerprint: fp})); reasonOf(err) != authapi.ReasonNeedsStepUp {
			t.Fatalf("replacing the local tag by pulling did not require a passkey: %v", err)
		}
		proof := stepUpFor(t, b, v1.StepUpAction_STEP_UP_ACTION_APPROVE_CHANGE, change.GetId()+":"+fp)
		assertReceipt(witness(change, proof), second, first.id)
		stored, err := changes.GetChange(t.Context(), connect.NewRequest(&v1.GetChangeRequest{Id: change.GetId()}))
		if err != nil || strings.Contains(protojson.Format(stored.Msg), loginPassword) {
			t.Fatalf("the saved pull outcome could not be read without credentials: %v", err)
		}
		assertReceipt(stored.Msg.GetChange(), second, first.id)
		logins := engine.SentLogins()
		if len(logins) != 2 || !strings.HasPrefix(logins[0], host+"/"+repo+"@"+first.digest+" as ") || !strings.HasPrefix(logins[1], host+"/"+repo+"@"+second.digest+" as ") {
			t.Fatal("Docker received registry credentials outside the two reviewed digest pulls")
		}
	})
}

func TestDockerPullAgainFindsTheInstalledPlatform(t *testing.T) {
	t.Parallel()
	storetest.Each(t, func(t *testing.T, backend store.Backend) {
		for _, snapshotter := range []bool{false, true} {
			mode := "classic"
			if snapshotter {
				mode = "containerd"
			}
			t.Run(mode, func(t *testing.T) { reviewedMultiPlatformPull(t, backend, snapshotter) })
		}
	})
}

func reviewedMultiPlatformPull(t *testing.T, backend store.Backend, snapshotter bool) {
	t.Helper()
	const (
		repo = "ghcr.io/fixture/index"
		path = "/v2/fixture/index"
		ref  = repo + ":edge"
	)
	digest := func(s string) string { return fmt.Sprintf("sha256:%x", sha256.Sum256([]byte(s))) }
	config := `{"os":"linux","architecture":"amd64"}`
	otherConfig := `{"os":"linux","architecture":"arm64"}`
	nextConfig := `{"os":"linux","architecture":"amd64","fixture":"next"}`
	manifest := func(config string) string {
		return fmt.Sprintf(`{"schemaVersion":2,"config":{"digest":%q}}`, digest(config))
	}
	index := func(native string) string {
		return fmt.Sprintf(`{"schemaVersion":2,"manifests":[{"digest":%q,"platform":{"os":"linux","architecture":"arm64"}},{"digest":%q,"platform":{"os":"linux","architecture":"amd64"}}]}`, digest(manifest(otherConfig)), digest(manifest(native)))
	}
	localIndex := digest(index(config))
	var moved atomic.Bool
	mux := http.NewServeMux()
	for _, body := range []string{config, otherConfig, nextConfig} {
		mux.HandleFunc(path+"/blobs/"+digest(body), func(w http.ResponseWriter, _ *http.Request) {
			_, _ = io.WriteString(w, body)
		})
		m := manifest(body)
		mux.HandleFunc(path+"/manifests/"+digest(m), func(w http.ResponseWriter, _ *http.Request) {
			_, _ = io.WriteString(w, m)
		})
	}
	mux.HandleFunc(path+"/manifests/edge", func(w http.ResponseWriter, _ *http.Request) {
		selected := config
		if moved.Load() {
			selected = nextConfig
		}
		_, _ = io.WriteString(w, index(selected))
	})
	reg := httptest.NewServer(mux)
	t.Cleanup(reg.Close)

	f := startFull(t, backend, nil)
	b := signedIn(t, f)
	engine := f.addServerNamed(t, "image-fixture", nil, func(e *dockertest.Engine, _ *logs.Archive) {
		e.ContainerdImageStore = snapshotter
		e.Registry[ref] = dockertest.Image{Digest: localIndex, ConfigDigest: digest(config), Index: true}
		e.Registry[repo+"@"+digest(manifest(nextConfig))] = dockertest.Image{Digest: digest(manifest(nextConfig)), ConfigDigest: digest(nextConfig)}
	})
	srv := f.lastDocker
	server := srv.id.ServerID
	srv.stop()
	waitConnected(t, f, server, false)
	srv.images = agent.ImageReads{Test: reg.URL}
	srv.run(t)
	waitConnected(t, f, server, true)
	if err := engine.Client().PullImage(t.Context(), ref, nil); err != nil {
		t.Fatal(err)
	}
	oldID := digest(config)
	if snapshotter {
		oldID = localIndex
	}
	api := dockwellv1connect.NewDockerServiceClient(b, b.origin, connect.WithProtoJSON())
	plan := func() *v1.PlanDockerMaintenanceResponse {
		t.Helper()
		got, err := api.PlanDockerMaintenance(t.Context(), connect.NewRequest(&v1.PlanDockerMaintenanceRequest{
			ServerIds: []string{server}, Operation: &v1.PlanDockerMaintenanceRequest_Pull{Pull: &v1.DockerImagePull{
				Reference: ref, ExpectedLocalImageId: proto.String(oldID),
			}},
		}))
		if err != nil {
			t.Fatal(err)
		}
		return got.Msg
	}
	same := plan()
	current, candidate := same.GetDigestCheck().GetCurrent(), same.GetDigestCheck().GetCandidate()
	if same.GetChange() != nil || !same.GetDigestCheck().GetSameContent() ||
		current.GetIndexDigest() != localIndex || current.GetManifestDigest() != "" ||
		candidate.GetIndexDigest() != localIndex || candidate.GetManifestDigest() != digest(manifest(config)) {
		t.Fatalf("the installed platform didn't compare index to index: %v", same.GetDigestCheck())
	}
	if snapshotter && current.GetConfigDigest() != "" || !snapshotter && current.GetConfigDigest() != oldID {
		t.Fatalf("the image store ID was reported as the wrong kind of digest: %v", current)
	}

	moved.Store(true)
	changed := plan()
	change := changed.GetChange()
	changeAPI := changeClient(b, b.origin)
	fp := change.GetPlan().GetFingerprint()
	if change == nil || changed.GetDigestCheck().GetSameContent() {
		t.Fatalf("the changed platform manifests didn't produce a review: %v", changed.GetDigestCheck())
	}
	req := &v1.ApplyChangeRequest{Id: change.GetId(), Fingerprint: fp}
	if _, err := changeAPI.ApplyChange(t.Context(), connect.NewRequest(req)); reasonOf(err) != authapi.ReasonNeedsStepUp {
		t.Fatalf("retargeting the alias didn't require a passkey: %v", err)
	}
	req.Proof = stepUpFor(t, b, v1.StepUpAction_STEP_UP_ACTION_APPROVE_CHANGE, change.GetId()+":"+fp)
	watch := watchWithin(t, changeAPI, change.GetId(), 15*time.Second)
	if _, err := changeAPI.ApplyChange(t.Context(), connect.NewRequest(req)); err != nil {
		t.Fatal(err)
	}
	done := waitWatchWithin(t, watch, 15*time.Second).last()
	if done.GetState() != v1.ChangeState_CHANGE_STATE_APPLIED || len(done.GetSteps()) != 1 || len(done.GetSteps()[0].GetDockerImageReceipts()) != 1 {
		t.Fatalf("replacement did not apply: %v", done)
	}
	newID := digest(nextConfig)
	if snapshotter {
		newID = digest(manifest(nextConfig))
	}
	receipt := done.GetSteps()[0].GetDockerImageReceipts()[0]
	if receipt.GetImageKey().GetImageId() != newID || receipt.GetPreviousImageId() != oldID {
		t.Fatalf("replacement recorded the wrong image store IDs: %v", receipt)
	}
	if kept, err := engine.Client().InspectImage(t.Context(), ref); err != nil || kept.ID != newID {
		t.Fatalf("alias after replacing the platform image: %v, %v", kept, err)
	}
}

func TestDockerPullRefusesAHubLoginOnAMirroredEngineAtReview(t *testing.T) {
	t.Parallel()
	storetest.Each(t, func(t *testing.T, backend store.Backend) {
		f := startFull(t, backend, nil)
		b := signedIn(t, f)
		engine := f.addServerNamed(t, "mirror-fixture", nil, func(e *dockertest.Engine, _ *logs.Archive) {
			e.Mirrors = []string{"https://mirror.invalid"}
		})
		changes := changeClient(b, b.origin)
		apply(t, changes, propose(t, changes, "Save fixture login", loginEdit("hub-fixture", &resourcev1.RegistryLogin{
			Registry: "docker.io", Username: loginUser, Password: loginPassword,
		})))
		imageAPI := dockwellv1connect.NewImageServiceClient(b, b.origin, connect.WithProtoJSON())
		listed, err := imageAPI.ListRegistryLogins(t.Context(), connect.NewRequest(&v1.ListRegistryLoginsRequest{}))
		if err != nil || len(listed.Msg.GetLogins()) != 1 {
			t.Fatalf("load the fixture login: %v", err)
		}
		dockerAPI := dockwellv1connect.NewDockerServiceClient(b, b.origin, connect.WithProtoJSON())
		_, err = dockerAPI.PlanDockerMaintenance(t.Context(), connect.NewRequest(&v1.PlanDockerMaintenanceRequest{
			ServerIds: []string{f.lastDocker.id.ServerID}, Operation: &v1.PlanDockerMaintenanceRequest_Pull{Pull: &v1.DockerImagePull{
				Reference: "fixture/private:latest", LoginId: listed.Msg.GetLogins()[0].GetId(),
			}},
		}))
		if reasonOf(err) != "registry_login_mirrors" || !strings.Contains(err.Error(), "Docker Hub mirrors are enabled") || len(engine.SentLogins()) != 0 {
			t.Fatalf("a login on a mirrored Engine didn't give the review-time reason: %v", err)
		}
	})
}
