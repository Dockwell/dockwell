// SPDX-License-Identifier: AGPL-3.0-only

package control

import (
	"archive/tar"
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"connectrpc.com/connect"

	agentv1 "github.com/dockwell/dockwell/gen/dockwell/agent/v1"
	v1 "github.com/dockwell/dockwell/gen/dockwell/v1"
	"github.com/dockwell/dockwell/gen/dockwell/v1/dockwellv1connect"
	"github.com/dockwell/dockwell/internal/agent/artifact"
	agentfiles "github.com/dockwell/dockwell/internal/agent/files"
	"github.com/dockwell/dockwell/internal/control/containers"
	"github.com/dockwell/dockwell/internal/store"
)

func TestDockerArtifactImportAndExportOnBothStores(t *testing.T) {
	t.Parallel()
	eachStoreParallel(t, func(t *testing.T, backend store.Backend) {
		f := startFull(t, backend, nil)
		b := signedIn(t, f)
		f.addDockerServer(t)
		agent := f.lastDocker
		agent.stop()
		clockStart := time.Now()
		var elapsed atomic.Int64
		agent.artifactClock = func() time.Time { return clockStart.Add(time.Duration(elapsed.Load())) }
		agent.disk = agentfiles.New(agent.dataDir, nil, nil)
		agent.connect(t, f)
		server := agent.id.ServerID
		agentClient, closeAgent, err := agent.id.Client()
		if err != nil {
			t.Fatal(err)
		}
		t.Cleanup(closeAgent)
		api := dockwellv1connect.NewDockerServiceClient(b, b.origin, connect.WithProtoJSON())
		ctx := t.Context()
		changes := changeClient(b, b.origin)
		data, id := dockerArchiveFixture(t)
		info, err := api.BeginDockerArtifactUpload(ctx, connect.NewRequest(&v1.BeginDockerArtifactUploadRequest{
			ServerId: server, Kind: artifact.Import, SizeBytes: uint64(len(data)),
		}))
		if err != nil {
			t.Fatal(err)
		}
		upload := info.Msg.GetArtifactId()
		endpoint := func(id string) string { return b.origin + "/v1/docker/artifacts/" + server + "/" + id + "/bytes" }
		part := func(offset, end int, prefix string, want int) {
			t.Helper()
			req, err := http.NewRequestWithContext(ctx, http.MethodPut, fmt.Sprintf("%s?offset=%d", endpoint(upload), offset), bytes.NewReader(data[offset:end]))
			if err != nil {
				t.Fatal(err)
			}
			req.Header.Set("Content-Type", "application/octet-stream")
			req.Header.Set(containers.ArtifactPrefixHeader, prefix)
			resp, err := b.Do(req)
			if err != nil {
				t.Fatal(err)
			}
			defer resp.Body.Close()
			if resp.StatusCode != want {
				body, _ := io.ReadAll(resp.Body)
				t.Fatalf("PUT: %d: %s", resp.StatusCode, body)
			}
			if want == http.StatusTooManyRequests && resp.Header.Get("Dockwell-Error") != artifact.TransferSlotsReason {
				t.Fatal("a busy lane did not report a retryable transfer-slot refusal")
			}
		}
		seam := 1024
		release := holdArchiveTransferSlots(t, agent.disk)
		part(0, seam, "", http.StatusTooManyRequests)
		release()
		part(0, seam, "", http.StatusOK)
		part(seam, len(data), "wrong prefix", http.StatusPreconditionFailed)
		sha := sha256.Sum256(data[:seam])
		part(seam, len(data), hex.EncodeToString(sha[:]), http.StatusOK)
		sealed, err := api.FinishDockerArtifactUpload(ctx, connect.NewRequest(&v1.FinishDockerArtifactUploadRequest{ServerId: server, ArtifactId: upload}))
		if err != nil || sealed.Msg.GetState() != artifact.Checking {
			t.Fatalf("queue seal: %v %v", sealed, err)
		}
		lease := func() *connect.Request[v1.GetDockerArtifactRequest] {
			return connect.NewRequest(&v1.GetDockerArtifactRequest{ServerId: server, ArtifactId: upload})
		}
		for deadline := time.Now().Add(5 * time.Second); ; {
			sealed, err = api.GetDockerArtifact(ctx, lease())
			if err != nil || sealed.Msg.GetState() != artifact.Checking || time.Now().After(deadline) {
				break
			}
			time.Sleep(10 * time.Millisecond)
		}
		if err != nil || sealed.Msg.GetState() != artifact.Ready || len(sealed.Msg.GetImages()) != 1 {
			t.Fatalf("seal: %v %v", sealed, err)
		}
		const tag = "fixture/archives:approved"
		load, err := api.PlanDockerMaintenance(ctx, connect.NewRequest(&v1.PlanDockerMaintenanceRequest{ServerIds: []string{server}, Operation: &v1.PlanDockerMaintenanceRequest_Load{Load: &v1.DockerImageLoad{
			ArtifactId: upload, SelectedImageIds: []string{id}, Tags: []*v1.DockerImageImportTag{{ImageId: id, Alias: tag}},
		}}}))
		if err != nil {
			t.Fatal(err)
		}
		elapsed.Add(int64(16 * time.Minute))
		pinStatus := func(changeID, artifactID string, ended int) {
			t.Helper()
			pin := &agentv1.DockerArtifactPin{ChangeId: changeID, ArtifactId: artifactID}
			got, err := agentClient.CheckDockerArtifactPins(ctx, connect.NewRequest(&agentv1.CheckDockerArtifactPinsRequest{Pins: []*agentv1.DockerArtifactPin{pin}}))
			if err != nil || len(got.Msg.GetAbsent()) != 0 || len(got.Msg.GetEnded()) != ended {
				t.Fatalf("archive pin reconciliation: %v %v", got, err)
			}
		}
		pinStatus(load.Msg.GetChange().GetId(), upload, 0)
		if _, err := api.AbortDockerArtifact(ctx, connect.NewRequest(&v1.AbortDockerArtifactRequest{ServerId: server, ArtifactId: upload})); connect.CodeOf(err) != connect.CodeFailedPrecondition {
			t.Fatalf("a pending plan's lease was abortable: %v", err)
		}
		release = holdArchiveTransferSlots(t, agent.disk)
		review := load.Msg.GetChange()
		if _, err := changes.ApplyChange(ctx, connect.NewRequest(&v1.ApplyChangeRequest{
			Id: review.GetId(), Fingerprint: review.GetPlan().GetFingerprint(),
		})); err != nil {
			t.Fatal(err)
		}
		// The apply stays in progress with the lease pinned and finishes when
		// a slot opens. The agent's clock-based test covers the file deadline.
		waiting, err := changes.GetChange(ctx, connect.NewRequest(&v1.GetChangeRequest{Id: review.GetId()}))
		if err != nil || waiting.Msg.GetChange().GetState() != v1.ChangeState_CHANGE_STATE_APPLYING {
			t.Fatalf("archive apply didn't wait for a transfer slot: %v %v", waiting, err)
		}
		pinStatus(review.GetId(), upload, 0)
		release()
		waitApplied(t, changes, review.GetId())
		pinStatus(review.GetId(), upload, 1)
		if _, err := api.GetDockerArtifact(ctx, lease()); connect.CodeOf(err) != connect.CodeNotFound {
			t.Fatalf("applied import still present: %v", err)
		}
		ex, err := api.PlanDockerMaintenance(ctx, connect.NewRequest(&v1.PlanDockerMaintenanceRequest{ServerIds: []string{server}, Operation: &v1.PlanDockerMaintenanceRequest_Export{Export: &v1.DockerImageExport{
			SourceKey: &v1.DockerImageSourceKey{ServerId: server, ImageId: id}, Tags: []string{tag},
		}}}))
		if err != nil {
			t.Fatal(err)
		}
		exportID := ex.Msg.GetChange().GetPlan().GetResources()[0].GetDockerMaintenance().GetImageAction().GetStagedSourceId()
		leases := func() *connect.Request[v1.ListDockerArtifactsRequest] {
			return connect.NewRequest(&v1.ListDockerArtifactsRequest{ServerId: server})
		}
		listed, err := api.ListDockerArtifacts(ctx, leases())
		if err != nil || len(listed.Msg.GetArtifacts()) != 1 || listed.Msg.GetArtifacts()[0].GetArtifactId() != exportID {
			t.Fatalf("reviewed export lease: %v %v", listed, err)
		}
		token := createToken(t, b, v1.TokenScope_TOKEN_SCOPE_READ_WRITE)
		script := changeClient(bearer{token: token, rt: b.client.Transport}, b.origin)
		if _, err := script.DiscardChange(ctx, connect.NewRequest(&v1.DiscardChangeRequest{Id: ex.Msg.GetChange().GetId()})); err != nil {
			t.Fatal(err)
		}
		pinStatus(ex.Msg.GetChange().GetId(), exportID, 1)
		listed, err = api.ListDockerArtifacts(ctx, leases())
		if err != nil || len(listed.Msg.GetArtifacts()) != 0 {
			t.Fatalf("discarded export lease: %v %v", listed, err)
		}
		ex, err = api.PlanDockerMaintenance(ctx, connect.NewRequest(&v1.PlanDockerMaintenanceRequest{ServerIds: []string{server}, Operation: &v1.PlanDockerMaintenanceRequest_Export{Export: &v1.DockerImageExport{
			SourceKey: &v1.DockerImageSourceKey{ServerId: server, ImageId: id}, Tags: []string{tag},
		}}}))
		if err != nil {
			t.Fatal(err)
		}
		exportID = ex.Msg.GetChange().GetPlan().GetResources()[0].GetDockerMaintenance().GetImageAction().GetStagedSourceId()
		elapsed.Add(int64(16 * time.Minute))
		applyConfirmed(t, changes, ex.Msg.GetChange())
		get := func(who *browser, at, ifRange string, want int) ([]byte, string) {
			t.Helper()
			req, err := http.NewRequestWithContext(ctx, http.MethodGet, endpoint(exportID), http.NoBody)
			if err != nil {
				t.Fatal(err)
			}
			req.Header.Set("Range", at)
			req.Header.Set("If-Range", ifRange)
			resp, err := who.Do(req)
			if err != nil {
				t.Fatal(err)
			}
			defer resp.Body.Close()
			data, err := io.ReadAll(resp.Body)
			if err != nil || resp.StatusCode != want {
				t.Fatalf("download: %d %s %v", resp.StatusCode, data, err)
			}
			return data, resp.Header.Get("ETag")
		}
		whole, etag := get(b, "bytes=0-1", `"stale"`, http.StatusOK)
		head, _ := get(b, "bytes=0-511", etag, http.StatusPartialContent)
		tail, _ := get(b, "bytes=512-", etag, http.StatusPartialContent)
		if !bytes.Equal(append(head, tail...), whole) || len(etag) != 66 || !bytes.Contains(whole, []byte(tag)) || bytes.Contains(whole, []byte("original")) {
			t.Fatal("reviewed or resumed archive did not match")
		}
		other := newBrowser(t, b.origin, f.api, nil)
		other.auth = b.auth
		other.signIn(t)
		_, _ = get(other, "", "", http.StatusNotFound)
	})
}

func holdArchiveTransferSlots(t *testing.T, files *agentfiles.Server) func() {
	t.Helper()
	var releases []func()
	free := sync.OnceFunc(func() {
		for _, release := range releases {
			release()
		}
	})
	t.Cleanup(free)
	for range 3 {
		release, err := files.TakeTransfer(t.Context())
		if err != nil {
			t.Fatal(err)
		}
		releases = append(releases, release)
	}
	return free
}

func dockerArchiveFixture(t *testing.T) ([]byte, string) {
	t.Helper()
	return dockerArchiveWithAliases(t, "fixture/archives:original")
}

func dockerArchiveWithAliases(t *testing.T, aliases ...string) ([]byte, string) {
	t.Helper()
	config := []byte(`{"architecture":"amd64","os":"linux","rootfs":{"type":"layers","diff_ids":[]}}`)
	digest := sha256.Sum256(config)
	name := hex.EncodeToString(digest[:]) + ".json"
	manifest, err := json.Marshal([]map[string]any{{"Config": name, "RepoTags": aliases, "Layers": []string{}}})
	if err != nil {
		t.Fatal(err)
	}
	var b bytes.Buffer
	w := tar.NewWriter(&b)
	for _, p := range []struct {
		name string
		body []byte
	}{{name, config}, {"manifest.json", manifest}} {
		if err := w.WriteHeader(&tar.Header{Name: p.name, Mode: 0o600, Size: int64(len(p.body))}); err != nil {
			t.Fatal(err)
		}
		if _, err := w.Write(p.body); err != nil {
			t.Fatal(err)
		}
	}
	if err := w.Close(); err != nil {
		t.Fatal(err)
	}
	return b.Bytes(), "sha256:" + hex.EncodeToString(digest[:])
}
