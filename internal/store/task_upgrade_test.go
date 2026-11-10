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

// Main already stores local tokens at version 24. Adding task runs must upgrade
// that database and preserve the tokens, including which shell account made one.
func TestUpgradeFromLocalTokensKeepsTokensAndTaskRuns(t *testing.T) {
	storetest.Each(t, func(t *testing.T, b store.Backend) {
		ctx := t.Context()
		location := storetest.New(t, b)
		old, err := store.OpenAt(ctx, location, 24)
		if err != nil {
			t.Fatal(err)
		}
		defer old.Close()
		at := time.Date(2026, 11, 8, 12, 0, 0, 0, time.UTC)
		token := store.APIToken{
			ID: "tok_local", TokenHash: []byte("local token hash"), UserID: "usr_owner",
			Name: "Local CLI", Hint: "dwt_test", Scope: store.ScopeReadWrite,
			Grants: []string{store.GrantLogs}, CreatedAt: at, LocalAccount: "mira@web-1",
		}
		if err := old.Update(ctx, func(tx *store.Tx) error {
			if err := tx.CreateUser(ctx, store.User{
				ID: token.UserID, Email: "mira@example.com", Name: "Mira Chen",
				Role: store.RoleOwner, WebAuthnHandle: []byte("owner handle"), CreatedAt: at,
			}); err != nil {
				return err
			}
			return tx.CreateAPIToken(ctx, token)
		}); err != nil {
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
		checkToken := func(db *store.DB) {
			t.Helper()
			if err := db.View(ctx, func(tx *store.Tx) error {
				got, err := tx.GetAPITokenByHash(ctx, token.TokenHash)
				if err != nil {
					return err
				}
				if !reflect.DeepEqual(got, token) {
					t.Errorf("local token after upgrade = %+v, want %+v", got, token)
				}
				return nil
			}); err != nil {
				t.Fatal(err)
			}
		}
		checkToken(db)
		run := &dockwellv1.TaskRun{
			Id: "run_upgrade", TaskId: "tsk_upgrade", ServerId: "srv_web",
			State:     dockwellv1.TaskRunState_TASK_RUN_STATE_SUCCEEDED,
			Reason:    dockwellv1.TaskRunReason_TASK_RUN_REASON_BY_HAND,
			StartTime: timestamppb.New(at), EndTime: timestamppb.New(at.Add(time.Second)),
			Output: []*dockwellv1.TaskOutputLine{{Time: timestamppb.New(at), Text: "done"}},
		}
		encoded, err := proto.Marshal(run)
		if err != nil {
			t.Fatal(err)
		}
		stored := store.TaskRun{
			ID: run.Id, TaskID: run.TaskId, ServerID: run.ServerId, State: int32(run.State),
			StartedAt: at, EndedAt: at.Add(time.Second), Run: encoded,
		}
		if err := db.Update(ctx, func(tx *store.Tx) error { return tx.PutTaskRun(ctx, stored) }); err != nil {
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
		checkToken(again)
		if err := again.View(ctx, func(tx *store.Tx) error {
			got, err := tx.GetTaskRun(ctx, stored.ID)
			if err != nil {
				return err
			}
			if !reflect.DeepEqual(got, stored) {
				t.Errorf("task run after reopening = %+v, want %+v", got, stored)
			}
			return nil
		}); err != nil {
			t.Fatal(err)
		}
	})
}
