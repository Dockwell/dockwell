// SPDX-License-Identifier: AGPL-3.0-only

package databases

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"testing"

	"connectrpc.com/connect"

	dockwellv1 "github.com/dockwell/dockwell/gen/dockwell/v1"
	"github.com/dockwell/dockwell/internal/database"
)

// backupsOf lists what each volume has, newest first, as BackupService does, or
// fails for a volume it can't read.
type backupsOf map[string][]*dockwellv1.Backup

func (b backupsOf) ListBackups(_ context.Context, req *connect.Request[dockwellv1.ListBackupsRequest]) (*connect.Response[dockwellv1.ListBackupsResponse], error) {
	list, ok := b[req.Msg.GetVolumeId()]
	if !ok {
		return nil, connect.NewError(connect.CodeInternal, errors.New("the disk is gone"))
	}
	if n := int(req.Msg.GetPageSize()); n > 0 && n < len(list) {
		list = list[:n]
	}
	return connect.NewResponse(&dockwellv1.ListBackupsResponse{Backups: list}), nil
}

func TestABackupThatCantBeReadCostsOnlyItsDatabase(t *testing.T) {
	var runs []*dockwellv1.Backup
	for i := range 30 {
		runs = append(runs, &dockwellv1.Backup{Id: fmt.Sprintf("bkp_failed_%d", i), State: dockwellv1.BackupState_BACKUP_STATE_FAILED})
	}
	runs = append(runs, &dockwellv1.Backup{Id: "bkp_done", State: dockwellv1.BackupState_BACKUP_STATE_DONE})
	h := New(nil, backupsOf{"vol_shop": runs}, nil, nil, slog.New(slog.DiscardHandler))

	gone := &placed{db: &dockwellv1.Database{Id: "svc_gone", VolumeId: "vol_gone"}}
	shop := &placed{db: &dockwellv1.Database{Id: "svc_shop", VolumeId: "vol_shop"}}
	if err := h.complete(t.Context(), []*placed{gone, shop}); err != nil {
		t.Fatalf("one volume's backups that can't be read failed the list: %v", err)
	}
	if gone.db.GetNewestBackup() != nil || gone.db.GetLatestBackup() != nil {
		t.Errorf("the volume that can't be read has backups %v and %v", gone.db.GetNewestBackup(), gone.db.GetLatestBackup())
	}
	if got := shop.db.GetNewestBackup().GetId(); got != "bkp_done" {
		t.Errorf("the newest whole backup is %q, after 30 that failed, want bkp_done", got)
	}
	if got := shop.db.GetLatestBackup().GetId(); got != "bkp_failed_0" {
		t.Errorf("the latest run is %q, want bkp_failed_0", got)
	}
}

// A server's answer waits for its slowest database, so asking it waits longer than one
// database gets: one that hangs costs its own row, and the server's others still show.
func TestAServerGetsLongerThanItsSlowestDatabase(t *testing.T) {
	if askWait <= database.ReadWait() {
		t.Errorf("a server gets %s to answer, and each of its databases %s", askWait, database.ReadWait())
	}
}
