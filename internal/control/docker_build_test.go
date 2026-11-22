// SPDX-License-Identifier: AGPL-3.0-only

package control

import (
	"os"
	"path/filepath"
	"testing"
	"time"

	"connectrpc.com/connect"

	v1 "github.com/dockwell/dockwell/gen/dockwell/v1"
	"github.com/dockwell/dockwell/gen/dockwell/v1/dockwellv1connect"
	agentfiles "github.com/dockwell/dockwell/internal/agent/files"
	"github.com/dockwell/dockwell/internal/store"
	"github.com/dockwell/dockwell/internal/store/storetest"
)

func TestDockerImageBuildReviewAndCursorOnBothStores(t *testing.T) {
	t.Parallel()
	storetest.Each(t, func(t *testing.T, backend store.Backend) {
		f := startFull(t, backend, nil)
		b := signedIn(t, f)
		engine := f.addDockerServer(t)
		engine.SetDockerRoot(t.TempDir())
		agent := f.lastDocker
		agent.stop()
		agent.disk = agentfiles.New(agent.dataDir, nil, nil)
		agent.connect(t, f)
		ctx := t.Context()
		server := agent.id.ServerID
		api := dockwellv1connect.NewDockerServiceClient(b, b.origin, connect.WithProtoJSON())
		changes := changeClient(b, b.origin)
		folder := t.TempDir()
		file := filepath.Join(folder, "Dockerfile")
		if err := os.WriteFile(file, []byte("FROM scratch\nRUN echo fixture-build-line\n"), 0o600); err != nil {
			t.Fatal(err)
		}
		planned, err := api.PlanDockerMaintenance(ctx, connect.NewRequest(&v1.PlanDockerMaintenanceRequest{
			ServerIds: []string{server},
			Operation: &v1.PlanDockerMaintenanceRequest_Build{Build: &v1.DockerImageBuild{FolderPath: folder, Output: "fixture/local"}},
		}))
		if err != nil {
			t.Fatal(err)
		}
		ch := planned.Msg.GetChange()
		action := ch.GetPlan().GetResources()[0].GetDockerMaintenance().GetImageAction()
		if action.GetDockerfileSha256() == "" || action.GetSourceSha256() == "" || action.GetBuild().GetOutput() == "fixture/local" {
			t.Fatalf("the build plan did not freeze its inputs: %v", action)
		}
		// Apply must read the reviewed copy even when the original folder changes.
		if err := os.WriteFile(file, []byte("FROM scratch\nRUN exit 99\n"), 0o600); err != nil {
			t.Fatal(err)
		}
		if _, err := changes.ApplyChange(ctx, connect.NewRequest(&v1.ApplyChangeRequest{Id: ch.GetId(), Fingerprint: ch.GetPlan().GetFingerprint()})); err != nil {
			t.Fatal(err)
		}
		deadline := time.Now().Add(15 * time.Second)
		for time.Now().Before(deadline) {
			current, err := changes.GetChange(ctx, connect.NewRequest(&v1.GetChangeRequest{Id: ch.GetId()}))
			if err != nil {
				t.Fatal(err)
			}
			switch c := current.Msg.GetChange(); c.GetState() {
			case v1.ChangeState_CHANGE_STATE_APPLIED:
				goto applied
			case v1.ChangeState_CHANGE_STATE_APPLYING:
				time.Sleep(20 * time.Millisecond)
			default:
				t.Fatalf("image build ended %s: %s", c.GetState(), c.GetError())
			}
		}
		t.Fatal("the image build did not finish")
	applied:
		if _, err := engine.Client().InspectImage(ctx, action.GetBuild().GetOutput()); err != nil {
			t.Fatal(err)
		}
		buildID := ""
		var cursor int64
		for range 2 {
			stream, err := api.WatchDockerImageBuild(ctx, connect.NewRequest(&v1.WatchDockerImageBuildRequest{ChangeId: ch.GetId(), ServerId: server, AfterLine: cursor}))
			if err != nil {
				t.Fatal(err)
			}
			var seen *v1.Build
			for stream.Receive() {
				seen = stream.Msg().GetBuild()
				for _, line := range stream.Msg().GetLines() {
					if line.GetNumber() <= cursor {
						t.Fatalf("duplicated build log line %d after %d", line.GetNumber(), cursor)
					}
					cursor = line.GetNumber()
				}
			}
			if err := stream.Err(); err != nil {
				t.Fatal(err)
			}
			_ = stream.Close()
			buildID = seen.GetId()
			if seen.GetState() != v1.BuildState_BUILD_STATE_DONE || cursor == 0 {
				t.Fatalf("build log has no completed build or lines: %v, cursor %d", seen, cursor)
			}
		}
		builds := dockwellv1connect.NewBuildServiceClient(b, b.origin, connect.WithProtoJSON())
		if _, err := builds.GetBuild(ctx, connect.NewRequest(&v1.GetBuildRequest{Id: buildID})); connect.CodeOf(err) != connect.CodeNotFound {
			t.Fatalf("the app build service exposed a standalone build: %v", err)
		}
		stream, err := api.WatchDockerImageBuild(ctx, connect.NewRequest(&v1.WatchDockerImageBuildRequest{ChangeId: ch.GetId(), ServerId: "srv_other"}))
		if err == nil {
			defer stream.Close()
			if stream.Receive() {
				t.Fatal("the watch returned a build for the wrong server")
			}
			err = stream.Err()
		}
		if connect.CodeOf(err) != connect.CodeNotFound {
			t.Fatalf("wrong-server build watch: %v", err)
		}
	})
}
