// SPDX-License-Identifier: AGPL-3.0-only

package control

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"connectrpc.com/connect"
	"google.golang.org/protobuf/proto"

	dockwellv1 "github.com/dockwell/dockwell/gen/dockwell/v1"
	controlfiles "github.com/dockwell/dockwell/internal/control/files"
	"github.com/dockwell/dockwell/internal/store"
	"github.com/dockwell/dockwell/internal/store/storetest"
)

func TestFileAccessProofAndPersistentPin(t *testing.T) {
	t.Parallel()
	storetest.Each(t, func(t *testing.T, backend store.Backend) {
		f := startFull(t, backend, nil)
		b := signedIn(t, f)
		server := f.addServer(t, false).id.ServerID
		path := filepath.Join(t.TempDir(), "site")
		key := filepath.Join(path, "authorized_keys")
		writeFile(t, key, "the host's login key")
		c := fileClient(b, b.origin)
		change := &dockwellv1.ChangeFileAccessRequest{
			ServerId: server, Path: key,
			Change: &dockwellv1.FileAccessChange{Mode: proto.Uint32(0o600)},
		}
		_, err := c.ChangeFileAccess(t.Context(), connect.NewRequest(change))
		needed := stepUpAsked(err)
		if reasonOf(err) != "needs_step_up" || needed.GetAction() != dockwellv1.StepUpAction_STEP_UP_ACTION_CHANGE_FILE_ACCESS || !strings.Contains(needed.GetTarget(), "mode 0600") {
			t.Fatalf("protected change = %v, %v", needed, err)
		}
		change.ConfirmationTarget = needed.GetTarget()
		change.Proof = stepUpFor(t, b, needed.GetAction(), needed.GetTarget())
		out, err := c.ChangeFileAccess(t.Context(), connect.NewRequest(change))
		if err != nil || !out.Msg.GetWayBackIn() || out.Msg.GetChanged() != 1 {
			t.Fatalf("confirmed change = %v, %v", out, err)
		}
		info, err := os.Stat(key)
		if err != nil || info.Mode().Perm() != 0o600 {
			t.Fatalf("file permissions = %v, %v", info, err)
		}
		event := waitForFileEvent(t, b, server, "file.access", key)
		if event.GetSubject().GetEntriesChanged() != 1 {
			t.Fatalf("access event = %v", event)
		}
		err = f.store.View(t.Context(), func(tx *store.Tx) error {
			stored, err := tx.GetEvent(t.Context(), event.GetId())
			if err != nil {
				return err
			}
			told, ok := controlfiles.ReadTold(stored.Detail)
			if !ok || told.Changed != 1 || strings.Contains(stored.Detail, "previous_access") {
				t.Fatalf("Activity detail = %s", stored.Detail)
			}
			data, err := tx.FileAccessUndo(t.Context(), event.GetId())
			if err != nil {
				return err
			}
			kept := &dockwellv1.ChangeFileAccessResponse{}
			if err := proto.Unmarshal(data, kept); err != nil {
				return err
			}
			if !proto.Equal(kept, &dockwellv1.ChangeFileAccessResponse{Undo: out.Msg.GetUndo()}) ||
				len(kept.GetUndo()) != 1 || kept.GetUndo()[0].GetBefore().GetMode() != 0o644 {
				t.Fatalf("stored Undo entries = %v", kept)
			}
			return nil
		})
		if err != nil {
			t.Fatal(err)
		}

		if _, err := c.PinFolder(t.Context(), connect.NewRequest(&dockwellv1.PinFolderRequest{ServerId: server, Path: path})); err != nil {
			t.Fatal(err)
		}
		view, err := c.ListPinnedFolders(t.Context(), connect.NewRequest(&dockwellv1.ListPinnedFoldersRequest{ServerId: server}))
		if err != nil || len(view.Msg.GetPaths()) != 1 || view.Msg.GetPaths()[0] != path {
			t.Fatalf("pinned folders = %v, %v", view, err)
		}
		if err := os.RemoveAll(path); err != nil {
			t.Fatal(err)
		}
		if _, err := c.UnpinFolder(t.Context(), connect.NewRequest(&dockwellv1.UnpinFolderRequest{ServerId: server, Path: path})); err != nil {
			t.Fatal(err)
		}
		view, err = c.ListPinnedFolders(t.Context(), connect.NewRequest(&dockwellv1.ListPinnedFoldersRequest{ServerId: server}))
		if err != nil || len(view.Msg.GetPaths()) != 0 {
			t.Fatalf("pinned folders after unpinning = %v, %v", view, err)
		}
	})
}
