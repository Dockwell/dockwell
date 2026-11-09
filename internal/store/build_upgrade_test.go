// SPDX-License-Identifier: AGPL-3.0-only

package store_test

import (
	"reflect"
	"testing"
	"time"

	"google.golang.org/protobuf/proto"
	"google.golang.org/protobuf/types/known/timestamppb"

	dockwellv1 "github.com/dockwell/dockwell/gen/dockwell/v1"
	"github.com/dockwell/dockwell/internal/store"
	"github.com/dockwell/dockwell/internal/store/storetest"
)

// Version 25 already keeps scheduled tasks' runs. Builds must upgrade that
// database without replacing its history, and keep their own logs and detection.
func TestBuildMigrationsKeepTaskRuns(t *testing.T) {
	storetest.Each(t, func(t *testing.T, backend store.Backend) {
		ctx := t.Context()
		location := storetest.New(t, backend)
		old, err := store.OpenAt(ctx, location, 25)
		if err != nil {
			t.Fatal(err)
		}
		defer old.Close()
		at := time.Date(2026, 11, 9, 12, 0, 0, 0, time.UTC)
		run := &dockwellv1.TaskRun{
			Id: "run_upgrade", TaskId: "tsk_upgrade", ServerId: "srv_web",
			State:     dockwellv1.TaskRunState_TASK_RUN_STATE_SUCCEEDED,
			StartTime: timestamppb.New(at), EndTime: timestamppb.New(at.Add(time.Second)),
			Output: []*dockwellv1.TaskOutputLine{{Time: timestamppb.New(at), Text: "kept before builds"}},
		}
		encoded, err := proto.Marshal(run)
		if err != nil {
			t.Fatal(err)
		}
		task := store.TaskRun{
			ID: run.Id, TaskID: run.TaskId, ServerID: run.ServerId, State: int32(run.State),
			StartedAt: at, EndedAt: at.Add(time.Second), Run: encoded,
		}
		if err := old.Update(ctx, func(tx *store.Tx) error { return tx.PutTaskRun(ctx, task) }); err != nil {
			t.Fatal(err)
		}
		if err := old.Close(); err != nil {
			t.Fatal(err)
		}
		db, err := store.Open(ctx, location)
		if err != nil {
			t.Fatal(err)
		}
		defer db.Close()
		build := store.Build{
			ID: "bld_upgrade", ChangeID: "chg_upgrade", ResourceID: "svc_web",
			EnvironmentID: "env_web", Service: "web", ServerID: run.ServerId,
			State: store.BuildDone, CreatedAt: at, Detection: `{"builder":"BUILDER_RAILPACK"}`,
		}
		line := store.BuildLine{Number: 1, Step: 1, At: at, Text: "kept after upgrade"}
		if err := db.Update(ctx, func(tx *store.Tx) error {
			if err := tx.CreateBuild(ctx, build); err != nil {
				return err
			}
			if err := tx.UpdateBuild(ctx, build); err != nil {
				return err
			}
			_, err := tx.AddBuildLines(ctx, build.ID, []store.BuildLine{line})
			return err
		}); err != nil {
			t.Fatal(err)
		}
		if err := db.Close(); err != nil {
			t.Fatal(err)
		}
		again, err := store.OpenExisting(ctx, location)
		if err != nil {
			t.Fatal(err)
		}
		defer again.Close()
		build.Lines = 1
		if err := again.View(ctx, func(tx *store.Tx) error {
			gotTask, err := tx.GetTaskRun(ctx, task.ID)
			if err != nil {
				return err
			}
			if !reflect.DeepEqual(gotTask, task) {
				t.Errorf("task run after upgrade = %+v, want %+v", gotTask, task)
			}
			gotBuild, err := tx.GetBuild(ctx, build.ID)
			if err != nil {
				return err
			}
			if !reflect.DeepEqual(gotBuild, build) {
				t.Errorf("build after reopening = %+v, want %+v", gotBuild, build)
			}
			lines, err := tx.BuildLines(ctx, build.ID, 0, 10)
			if err != nil {
				return err
			}
			if !reflect.DeepEqual(lines, []store.BuildLine{line}) {
				t.Errorf("build log after reopening = %+v, want %+v", lines, []store.BuildLine{line})
			}
			return nil
		}); err != nil {
			t.Fatal(err)
		}
	})
}
