// SPDX-License-Identifier: AGPL-3.0-only

package catalogapi

import (
	"path/filepath"
	"strings"
	"testing"
	"time"

	"google.golang.org/protobuf/proto"

	catalogv1 "github.com/dockwell/dockwell/gen/dockwell/catalog/v1alpha1"
	dockwellv1 "github.com/dockwell/dockwell/gen/dockwell/v1"
	"github.com/dockwell/dockwell/internal/store"
)

// An app whose source says it runs on amd64 alone goes on an amd64 server, and is
// refused on an arm64 one, naming the server and where to put it; a server that
// hasn't said its architecture yet, and an app that runs on both, go anywhere
// (ADR 0085).
func TestAnAppGoesOnAServerOfItsArchitecture(t *testing.T) {
	db, err := store.Open(t.Context(), filepath.Join(t.TempDir(), "state.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = db.Close() })
	now := time.Now()
	servers := map[string]string{"srv_x86": "amd64", "srv_arm": "arm64", "srv_new": ""}
	names := map[string]string{"srv_x86": "hz-1", "srv_arm": "hz-2", "srv_new": "hz-3"}
	if err := db.Update(t.Context(), func(tx *store.Tx) error {
		for id, arch := range servers {
			if err := tx.CreateServer(t.Context(), store.Server{ID: id, Name: names[id], CreatedAt: now, CertSerial: id, CertNotAfter: now.Add(time.Hour)}); err != nil {
				return err
			}
			inv, err := proto.Marshal(&dockwellv1.ServerInventory{Hostname: names[id], Architecture: arch})
			if err != nil {
				return err
			}
			if err := tx.SaveServerReport(t.Context(), id, store.ServerReport{Inventory: inv}, now); err != nil {
				return err
			}
		}
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	h := &Handler{db: db}
	amd64 := &catalogv1.Entry{Name: "Board", Architectures: []string{"amd64"}}
	if err := h.checkArchitectures(t.Context(), amd64, []string{"srv_x86"}); err != nil {
		t.Errorf("on an amd64 server: %v", err)
	}
	err = h.checkArchitectures(t.Context(), amd64, []string{"srv_arm"})
	if err == nil || !strings.Contains(err.Error(), "Board runs on amd64 servers alone, as its source says, and hz-2 is arm64. Install it on an amd64 server") {
		t.Errorf("on an arm64 server: %v", err)
	}
	if err := h.checkArchitectures(t.Context(), amd64, []string{"srv_new"}); err != nil {
		t.Errorf("on a server that hasn't said: %v", err)
	}
	if err := h.checkArchitectures(t.Context(), &catalogv1.Entry{Name: "Both"}, []string{"srv_arm"}); err != nil {
		t.Errorf("an app that runs on both: %v", err)
	}
}
