// SPDX-License-Identifier: AGPL-3.0-only

package control

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"connectrpc.com/connect"

	dockwellv1 "github.com/dockwell/dockwell/gen/dockwell/v1"
	"github.com/dockwell/dockwell/internal/agent/files"
	"github.com/dockwell/dockwell/internal/controlbackup"
	"github.com/dockwell/dockwell/internal/seal"
	"github.com/dockwell/dockwell/internal/store"
)

func TestControlBackupGoesToAnotherServerDuringAFetch(t *testing.T) {
	f := startFull(t, store.SQLite, nil)
	control := f.addNamedServer(t, "control", false)
	busy := f.addNamedServer(t, "fetching", false)
	spare := f.addNamedServer(t, "available", false)
	busy.stop()
	busy.standalone = true
	busy.connect(t, f)

	// Hold the first server's exclusive restore lane through the entire delivery.
	release, err := busy.disk.WaitBackupFetch(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	defer release()

	key, err := seal.LoadOrCreate(f.config.KeyFile)
	if err != nil {
		t.Fatal(err)
	}
	manager := controlbackup.New(controlbackup.Config{
		DB: f.store, Key: key, KeyFile: f.config.KeyFile, Install: f.ident.InstallID(),
		ControlID: control.id.ServerID, Agents: f.agents, Logger: f.config.Logger, Build: f.config.Build,
	})
	ctx := t.Context()
	if err := manager.Init(ctx); err != nil {
		t.Fatal(err)
	}
	if err := f.store.Update(ctx, func(tx *store.Tx) error {
		return tx.SetControlBackupServers(ctx, []string{busy.id.ServerID}, nil, time.Now())
	}); err != nil {
		t.Fatal(err)
	}
	if _, err := manager.TakeControlBackup(ctx, connect.NewRequest(&dockwellv1.TakeControlBackupRequest{})); err != nil {
		t.Fatal(err)
	}
	run, cancel := context.WithCancel(ctx)
	done := make(chan struct{})
	go func() {
		defer close(done)
		manager.Run(run)
	}()
	defer func() {
		cancel()
		<-done
	}()

	var kept store.ControlBackupPlace
	var last []store.ControlBackupPlace
	t.Cleanup(func() {
		if t.Failed() {
			t.Logf("placements: %+v", last)
		}
	})
	eventually(t, "a refusal from the fetching server and a copy on the available server", func() bool {
		var refused bool
		if err := f.store.View(ctx, func(tx *store.Tx) error {
			s, err := tx.ControlBackup(ctx)
			if err != nil {
				return err
			}
			places, err := tx.ControlBackupPlaces(ctx, s.Generation)
			last = places
			for _, p := range places {
				switch p.Place {
				case busy.id.ServerID:
					refused = p.SentAt.IsZero() && strings.Contains(p.Problem, files.ErrRestoring.Error())
				case spare.id.ServerID:
					kept = p
				}
			}
			return err
		}); err != nil {
			t.Fatal(err)
		}
		return refused && !kept.SentAt.IsZero()
	})
	if info, err := os.Stat(filepath.Join(kept.Path, kept.File)); err != nil || info.Size() == 0 {
		t.Fatalf("the available server didn't keep the bundle: %v, %v", info, err)
	}
}
