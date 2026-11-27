// SPDX-License-Identifier: AGPL-3.0-only

package agent

import (
	"errors"
	"testing"
	"testing/synctest"

	agentv1 "github.com/dockwell/dockwell/gen/dockwell/agent/v1"
	v1 "github.com/dockwell/dockwell/gen/dockwell/v1"
	"github.com/dockwell/dockwell/internal/agent/artifact"
	"github.com/dockwell/dockwell/internal/agent/files"
	"github.com/dockwell/dockwell/internal/id"
	"github.com/dockwell/dockwell/internal/resource"
)

func TestArchiveApplyWaitsPastFileTransferTimeout(t *testing.T) {
	dir := t.TempDir()
	spool, err := artifact.Open(dir)
	if err != nil {
		t.Fatal(err)
	}
	synctest.Test(t, func(t *testing.T) {
		disk := files.New(dir, nil, nil)
		disk.Embedded = true
		agent := &calls{cfg: Config{Dir: dir, Files: disk, artifacts: spool}}
		var held []func()
		defer func() {
			for _, release := range held {
				release()
			}
		}()
		// Fill the shared lane as file transfers would.
		for range 3 {
			release, err := disk.TakeTransfer(t.Context())
			if err != nil {
				t.Fatal(err)
			}
			held = append(held, release)
		}

		plan := &v1.DockerMaintenance{
			ChangeId: id.New(id.Change),
			ImageAction: &v1.DockerImageAction{
				Load: &v1.DockerImageLoad{},
			},
		}
		applied := make(chan error, 1)
		go func() {
			_, err := agent.applyArchiveAction(t.Context(), resource.Ref{}, plan, &agentv1.DockerCleanupProtection{})
			applied <- err
		}()
		synctest.Wait()

		// The fake clock advances until a regular file transfer gives up.
		if release, err := disk.TakeTransfer(t.Context()); !errors.Is(err, files.ErrBusy) {
			if release != nil {
				release()
			}
			t.Fatalf("file transfer with all places held: %v", err)
		}
		synctest.Wait()
		select {
		case err := <-applied:
			t.Fatalf("archive apply ended while all places were held: %v", err)
		default:
		}

		held[0]()
		held = held[1:]
		if err := <-applied; !errors.Is(err, artifact.ErrUnavailable) {
			t.Fatalf("archive apply after a place opened: %v, want a missing staged archive", err)
		}
	})
}
