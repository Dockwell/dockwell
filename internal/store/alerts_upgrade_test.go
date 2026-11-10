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

// An install at main's version 25 keeps its local CLI token and task history
// when alert tables are added. New alerts and pending messages survive a restart.
func TestUpgradeFromTaskRunsKeepsTokensTasksAndAlerts(t *testing.T) {
	storetest.Each(t, func(t *testing.T, b store.Backend) {
		ctx := t.Context()
		location := storetest.New(t, b)
		old, err := store.OpenAt(ctx, location, 25)
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
		// The old schema keeps the owner's role on users; CreateUser now writes
		// grants, which this schema hasn't got, so the row is written as it was then.
		handle := `x'01'`
		if b == store.Postgres {
			handle = `'\x01'`
		}
		storetest.Exec(t, location, `INSERT INTO users (id, email, name, role, webauthn_handle, created_at) VALUES ('usr_owner', 'mira@example.com', 'Mira Chen', 'owner', `+handle+`, 5)`)
		if err := old.Update(ctx, func(tx *store.Tx) error {
			if err := tx.CreateAPIToken(ctx, token); err != nil {
				return err
			}
			return tx.PutTaskRun(ctx, stored)
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
		alert := store.Alert{
			ID: "alt_upgrade", RuleID: "alr_disk", RuleName: "disk", Thing: "server:web-1:/",
			Title: "web-1's disk is nearly full", Detail: "Free some space", OpenedAt: at,
		}
		send := store.AlertSend{
			ID: "snd_upgrade", AlertID: alert.ID, RuleID: alert.RuleID, ChannelID: "ach_ops",
			Status: "firing", QueuedAt: at, DueAt: at,
		}
		if err := db.Update(ctx, func(tx *store.Tx) error {
			if err := tx.OpenAlert(ctx, alert); err != nil {
				return err
			}
			return tx.QueueSend(ctx, send)
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
		if err := again.View(ctx, func(tx *store.Tx) error {
			got, err := tx.GetAPITokenByHash(ctx, token.TokenHash)
			if err != nil {
				return err
			}
			if !reflect.DeepEqual(got, token) {
				t.Errorf("local token after upgrading and reopening = %+v, want %+v", got, token)
			}
			kept, err := tx.GetTaskRun(ctx, stored.ID)
			if err != nil {
				return err
			}
			if !reflect.DeepEqual(kept, stored) {
				t.Errorf("task run after upgrading and reopening = %+v, want %+v", kept, stored)
			}
			opened, err := tx.GetAlert(ctx, alert.ID)
			if err != nil {
				return err
			}
			if !reflect.DeepEqual(opened, alert) {
				t.Errorf("alert after reopening = %+v, want %+v", opened, alert)
			}
			due, err := tx.DueSends(ctx, at, 10)
			if err != nil {
				return err
			}
			if !reflect.DeepEqual(due, []store.AlertSend{send}) {
				t.Errorf("messages owed after reopening = %+v, want %+v", due, send)
			}
			return nil
		}); err != nil {
			t.Fatal(err)
		}
	})
}
