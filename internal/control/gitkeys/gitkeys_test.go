// SPDX-License-Identifier: AGPL-3.0-only

package gitkeys

import (
	"log/slog"
	"os"
	"path/filepath"
	"testing"

	"connectrpc.com/connect"
)

func TestSSHGateBoundsListsFetchesProjectsAndKeys(t *testing.T) {
	var g sshGate

	// Per-key listing cap is 1.
	rel1, err := g.acquire("prj_1", "gdk_1", false)
	if err != nil {
		t.Fatalf("first list acquire: %v", err)
	}
	if _, err := g.acquire("prj_1", "gdk_1", false); connect.CodeOf(err) != connect.CodeResourceExhausted {
		t.Fatalf("second concurrent list on same key = %v, want ResourceExhausted", err)
	}

	// Second listing on another key succeeds, hitting install-wide list cap (2).
	rel2, err := g.acquire("prj_2", "gdk_2", false)
	if err != nil {
		t.Fatalf("second list on other project: %v", err)
	}
	if _, err := g.acquire("prj_3", "gdk_3", false); connect.CodeOf(err) != connect.CodeResourceExhausted {
		t.Fatalf("third concurrent list = %v, want ResourceExhausted", err)
	}

	// Pack fetches have their own 2 slots (up to maxSSHTotal = 4).
	f1, err := g.acquire("prj_1", "gdk_1", true)
	if err != nil {
		t.Fatalf("first fetch acquire: %v", err)
	}
	// prj_1 now has 2 ops (1 list + 1 fetch), hitting per-project cap (2).
	if _, err := g.acquire("prj_1", "gdk_1", true); connect.CodeOf(err) != connect.CodeResourceExhausted {
		t.Fatalf("third op in prj_1 = %v, want ResourceExhausted", err)
	}
	f2, err := g.acquire("prj_3", "gdk_3", true)
	if err != nil {
		t.Fatalf("second fetch in prj_3: %v", err)
	}
	// Total is now 4 (2 lists + 2 fetches): any further op is refused.
	if _, err := g.acquire("prj_4", "gdk_4", true); connect.CodeOf(err) != connect.CodeResourceExhausted {
		t.Fatalf("fifth total op = %v, want ResourceExhausted", err)
	}

	// Releasing frees slots cleanly and is idempotent.
	rel1()
	rel1()
	rel2()
	f1()
	f2()
	rel3, err := g.acquire("prj_1", "gdk_1", false)
	if err != nil {
		t.Fatalf("re-acquire after release: %v", err)
	}
	rel3()
}

func TestSweepPackTempDirRemovesStalePacks(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "ssh-packs")
	if err := os.MkdirAll(dir, 0o700); err != nil {
		t.Fatal(err)
	}
	stale := filepath.Join(dir, "dockwell-ssh-pack-stale123")
	keep := filepath.Join(dir, "unrelated-file")
	if err := os.WriteFile(stale, []byte("old"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(keep, []byte("keep"), 0o600); err != nil {
		t.Fatal(err)
	}
	sweepPackTempDir(dir, slog.New(slog.DiscardHandler))
	if _, err := os.Stat(stale); !os.IsNotExist(err) {
		t.Fatalf("stale pack file was not swept: %v", err)
	}
	if _, err := os.Stat(keep); err != nil {
		t.Fatalf("unrelated file should remain: %v", err)
	}
}
