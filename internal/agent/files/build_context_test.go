// SPDX-License-Identifier: AGPL-3.0-only

package files

import (
	"errors"
	"os"
	"path/filepath"
	"testing"

	"github.com/moby/patternmatcher"

	"github.com/dockwell/dockwell/internal/agent/artifact"
)

func TestBuildFolderSnapshotUsesIgnoreRulesAndRejectsIncludedLinks(t *testing.T) {
	folder := t.TempDir()
	if err := os.WriteFile(filepath.Join(folder, "Dockerfile"), []byte("FROM scratch\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(folder, ".dockerignore"), []byte("excluded\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	link := filepath.Join(folder, "excluded")
	if err := os.Symlink("/proc/self/environ", link); err != nil {
		t.Fatal(err)
	}
	spool, err := artifact.Open(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	s := New(t.TempDir(), nil, nil)
	const owner = "build-fixture/session"
	ctx := t.Context()
	info, err := s.SnapshotContext(ctx, spool, owner, folder, "Dockerfile")
	if err != nil {
		t.Fatal(err)
	}
	err = spool.WithBuildContext(ctx, info.GetArtifactId(), info.GetSha256(), func(snapshot string) error {
		if _, err := os.Lstat(filepath.Join(snapshot, "excluded")); !errors.Is(err, os.ErrNotExist) {
			t.Fatalf("ignored entry was staged: %v", err)
		}
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(folder, "Dockerfile.dockerignore"), []byte("!excluded\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := s.SnapshotContext(ctx, spool, owner, folder, "Dockerfile"); !errors.Is(err, errContextUnsafe) {
		t.Fatalf("specific ignore rule included a link outside the context: %v", err)
	}
}

func TestBuildFolderRefusesHardLinks(t *testing.T) {
	outside := filepath.Join(t.TempDir(), ".pgpass")
	if err := os.WriteFile(outside, []byte("fixture"), 0o600); err != nil {
		t.Fatal(err)
	}
	folder := t.TempDir()
	if err := os.Link(outside, filepath.Join(folder, "data")); err != nil {
		t.Fatal(err)
	}
	spool, err := artifact.Open(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	s := New(t.TempDir(), nil, nil)
	if _, err := s.SnapshotContext(t.Context(), spool, "fixture/session", folder, "Dockerfile"); !errors.Is(err, ErrContextHardLink) {
		t.Fatalf("snapshot accepted a hard link to a protected file: %v", err)
	}
}

func TestIgnoredBuildEntriesNeverUseTheIncludedEntryLimit(t *testing.T) {
	ignore, err := patternmatcher.New([]string{"ignored*"})
	if err != nil {
		t.Fatal(err)
	}
	w := &contextWalk{ctx: t.Context(), entries: 50000, ignore: ignore}
	// Invalid archive names are never validated and don't use an entry.
	for range 50001 {
		if err := w.add(-1, free, `ignored:\name`, `ignored:\name`, 0); err != nil {
			t.Fatal(err)
		}
	}
	if w.entries != 50000 {
		t.Fatalf("ignored entries used the count: %d", w.entries)
	}
	if err := w.add(-1, free, "included", "included", 0); !errors.Is(err, ErrContextEntry) {
		t.Fatalf("an included file passed the limit: %v", err)
	}
}
