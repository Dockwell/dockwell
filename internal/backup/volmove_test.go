// SPDX-License-Identifier: AGPL-3.0-only

package backup

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	resourcev1 "github.com/dockwell/dockwell/gen/dockwell/resource/v1alpha1"
	"github.com/dockwell/dockwell/internal/docker"
	"github.com/dockwell/dockwell/internal/resource"
	"github.com/dockwell/dockwell/internal/resource/volume"
)

// moving makes a volume of the environment env_shop, as Dockwell makes one, holding a
// file, and says where a volume of the same name in env_store goes.
func moving(t *testing.T, b *bench, options map[string]string) (string, volume.Target) {
	t.Helper()
	src := volume.TargetOf(resource.Ref{Kind: volume.Kind, Scope: "env_shop", Name: "data"})
	if _, err := b.client.CreateVolume(t.Context(), &docker.VolumeCreate{
		Name: src.Name, DriverOpts: options, Labels: map[string]string{docker.LabelProject: src.Project, docker.LabelVolume: src.Key},
	}); err != nil {
		t.Fatal(err)
	}
	if len(options) == 0 {
		if err := os.WriteFile(filepath.Join(b.engine.VolumeDir(src.Name), "orders.db"), []byte("the orders"), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	return src.Name, volume.TargetOf(resource.Ref{Kind: volume.Kind, Scope: "env_store", Name: "data"})
}

func (b *bench) make(t *testing.T, to volume.Target, options map[string]string) {
	t.Helper()
	if _, err := b.client.CreateVolume(t.Context(), &docker.VolumeCreate{
		Name: to.Name, DriverOpts: options, Labels: map[string]string{docker.LabelProject: to.Project, docker.LabelVolume: to.Key},
	}); err != nil {
		t.Fatal(err)
	}
}

func (b *bench) holds(name string) string {
	got, err := os.ReadFile(filepath.Join(b.engine.VolumeDir(name), "orders.db"))
	if err != nil {
		return ""
	}
	return string(got)
}

// Where the agent sees Docker's own folders, a local volume's data moves by a rename
// of its folder into the new volume's place: the old one is left empty, and a move
// run again finds nothing left. Moving it back works the same way. Nothing moves
// while a container mounts either, nor over files the new one holds (ADR 0084, #572).
func TestAMoveRenamesTheVolumesFolderInPlace(t *testing.T) {
	b := newBench(t)
	b.engine.Mountpoints = true
	from, to := moving(t, b, nil)
	move, left, err := b.runner.JudgeMoveData(t.Context(), from, to, &resourcev1.Volume{})
	if err != nil || !left || move.How != resource.MoveInPlace || move.From != from || move.Bytes != 0 {
		t.Fatalf("judging the move = %+v, %v, %v", move, left, err)
	}
	b.make(t, to, nil)

	// A container that mounts the old volume stops the move, with nothing changed.
	inUse := b.engine.AddContainer(docker.Container{ID: "user", Name: "/user", Mounts: []docker.MountPoint{{Type: "volume", Name: from, Destination: "/data"}}})
	if err := b.runner.MoveData(t.Context(), move, to); err == nil || !strings.Contains(err.Error(), "a container mounts "+from) {
		t.Errorf("moving while a container mounts it = %v", err)
	}
	if err := b.client.RemoveContainer(t.Context(), inUse.ID, true); err != nil {
		t.Fatal(err)
	}
	if b.holds(from) != "the orders" || b.holds(to.Name) != "" {
		t.Fatal("a refused move changed something")
	}

	if err := b.runner.MoveData(t.Context(), move, to); err != nil {
		t.Fatal(err)
	}
	if b.holds(to.Name) != "the orders" {
		t.Errorf("the new volume holds %q", b.holds(to.Name))
	}
	if entries, err := os.ReadDir(b.engine.VolumeDir(from)); err != nil || len(entries) != 0 {
		t.Errorf("the old volume holds %v, %v", entries, err)
	}
	if _, left, err := b.runner.JudgeMoveData(t.Context(), from, to, &resourcev1.Volume{}); left || err != nil {
		t.Errorf("judging a move that's done = %v, %v", left, err)
	}
	if err := b.runner.MoveData(t.Context(), move, to); err != nil || b.holds(to.Name) != "the orders" {
		t.Errorf("a move run again = %v, and the new volume holds %q", err, b.holds(to.Name))
	}

	// And back, as an undo moves it.
	back, left, err := b.runner.JudgeMoveData(t.Context(), to.Name, volume.TargetOf(resource.Ref{Kind: volume.Kind, Scope: "env_shop", Name: "data"}), &resourcev1.Volume{})
	if err != nil || !left || back.How != resource.MoveInPlace {
		t.Fatalf("judging the move back = %+v, %v, %v", back, left, err)
	}
	if err := b.runner.MoveData(t.Context(), back, volume.TargetOf(resource.Ref{Kind: volume.Kind, Scope: "env_shop", Name: "data"})); err != nil {
		t.Fatal(err)
	}
	if b.holds(from) != "the orders" || b.holds(to.Name) != "" {
		t.Errorf("moved back, the old volume holds %q and the new %q", b.holds(from), b.holds(to.Name))
	}

	// Files in the new volume stay, and nothing moves over them.
	if err := os.WriteFile(filepath.Join(b.engine.VolumeDir(to.Name), "theirs"), []byte("x"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := b.runner.MoveData(t.Context(), move, to); err == nil || !strings.Contains(err.Error(), "already holds files") {
		t.Errorf("moving over files = %v", err)
	}
	if b.holds(from) != "the orders" {
		t.Error("a refused move took the data")
	}
}

// A run cut short after the rename, before the old folder was made again, finishes
// by making it.
func TestAMoveCutShortAfterItsRenameFinishes(t *testing.T) {
	b := newBench(t)
	b.engine.Mountpoints = true
	from, to := moving(t, b, nil)
	b.make(t, to, nil)
	dst := b.engine.VolumeDir(to.Name)
	if err := os.Remove(dst); err != nil {
		t.Fatal(err)
	}
	if err := os.Rename(b.engine.VolumeDir(from), dst); err != nil {
		t.Fatal(err)
	}
	move := resource.VolumeMove{From: from, How: resource.MoveInPlace}
	if err := b.runner.MoveData(t.Context(), move, to); err != nil {
		t.Fatal(err)
	}
	if fi, err := os.Stat(filepath.Join(filepath.Dir(dst), from)); err != nil || !fi.IsDir() {
		t.Errorf("the old volume's folder = %v", err)
	}
	if b.holds(to.Name) != "the orders" {
		t.Errorf("the new volume holds %q", b.holds(to.Name))
	}
}

// Where the agent can't see Docker's folders, the files are copied, in helpers, and
// the old volume emptied; the plan says how much. A copy a rollback cuts short is
// left where the data still is whole (ADR 0084, #572).
func TestAMoveCopiesWhatItCantRename(t *testing.T) {
	b := newBench(t)
	from, to := moving(t, b, nil)
	move, left, err := b.runner.JudgeMoveData(t.Context(), from, to, &resourcev1.Volume{})
	if err != nil || !left || move.How != resource.MoveCopy || move.Bytes != int64(len("the orders")) {
		t.Fatalf("judging the copy = %+v, %v, %v", move, left, err)
	}
	b.make(t, to, nil)
	if err := b.runner.MoveData(t.Context(), move, to); err != nil {
		t.Fatal(err)
	}
	if b.holds(to.Name) != "the orders" || b.holds(from) != "" {
		t.Errorf("copied, the new volume holds %q and the old %q", b.holds(to.Name), b.holds(from))
	}
	if rec, err := b.runner.readMove(to.Name); rec != nil || err != nil {
		t.Errorf("a finished copy left %+v, %v", rec, err)
	}
	if _, left, err := b.runner.JudgeMoveData(t.Context(), from, to, &resourcev1.Volume{}); left || err != nil {
		t.Errorf("judging a copy that's done = %v, %v", left, err)
	}

	// A copy into to that began and didn't finish: the old volume is whole, so a
	// rollback's move back finds nothing to move.
	back := volume.TargetOf(resource.Ref{Kind: volume.Kind, Scope: "env_shop", Name: "data"})
	if err := os.WriteFile(filepath.Join(b.engine.VolumeDir(from), "orders.db"), []byte("the orders"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := b.runner.saveMove(to.Name, &moveRecord{Move: move}); err != nil {
		t.Fatal(err)
	}
	if _, left, err := b.runner.JudgeMoveData(t.Context(), to.Name, back, &resourcev1.Volume{}); left || err != nil {
		t.Errorf("moving back a copy cut short = %v, %v", left, err)
	}

	// One that finished copying, and hadn't emptied the old volume: the new one has it
	// all, and moving back empties the old one first.
	if err := b.runner.saveMove(to.Name, &moveRecord{Move: move, Copied: true}); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(b.engine.VolumeDir(from), "orders.db"), []byte("half"), 0o600); err != nil {
		t.Fatal(err)
	}
	moveBack, left, err := b.runner.JudgeMoveData(t.Context(), to.Name, back, &resourcev1.Volume{})
	if err != nil || !left || moveBack.How != resource.MoveCopy {
		t.Fatalf("judging the move back = %+v, %v, %v", moveBack, left, err)
	}
	if err := b.runner.MoveData(t.Context(), moveBack, back); err != nil {
		t.Fatal(err)
	}
	if b.holds(from) != "the orders" || b.holds(to.Name) != "" {
		t.Errorf("moved back, the old volume holds %q and the new %q", b.holds(from), b.holds(to.Name))
	}
}

// A volume with options reaches its data through them, so the new one is made with
// the same, and nothing moves. One made otherwise is refused, as is a volume no
// project made.
func TestAMoveKeepsAVolumesOptions(t *testing.T) {
	b := newBench(t)
	options := map[string]string{"type": "none", "o": "bind", "device": "/srv/orders"}
	from, to := moving(t, b, options)
	move, left, err := b.runner.JudgeMoveData(t.Context(), from, to, &resourcev1.Volume{Options: options})
	if err != nil || !left || move.How != resource.MoveSameOptions {
		t.Fatalf("judging the move = %+v, %v, %v", move, left, err)
	}
	if _, _, err := b.runner.JudgeMoveData(t.Context(), from, to, &resourcev1.Volume{}); err == nil || !strings.Contains(err.Error(), "the same driver and options") {
		t.Errorf("moving into a volume without its options = %v", err)
	}
	b.make(t, to, options)
	if err := b.runner.MoveData(t.Context(), move, to); err != nil {
		t.Fatal(err)
	}
	if _, left, err := b.runner.JudgeMoveData(t.Context(), from, to, &resourcev1.Volume{Options: options}); left || err != nil {
		t.Errorf("judging it once made = %v, %v", left, err)
	}

	if _, err := b.client.CreateVolume(t.Context(), &docker.VolumeCreate{Name: "someones"}); err != nil {
		t.Fatal(err)
	}
	if _, _, err := b.runner.JudgeMoveData(t.Context(), "someones", to, &resourcev1.Volume{}); err == nil || !strings.Contains(err.Error(), "isn't a volume of a project") {
		t.Errorf("moving a volume no project made = %v", err)
	}
}

// A copy leaves what a backup leaves alone on the disk.
func TestACopyNeedsRoom(t *testing.T) {
	if err := roomToCopy(10<<30, 100<<30, 1<<30, "shop_data"); err != nil {
		t.Errorf("a gigabyte onto 10 free = %v", err)
	}
	err := roomToCopy(2<<30, 100<<30, 1<<30, "shop_data")
	if err == nil || !strings.Contains(err.Error(), "nothing stopped and nothing moved") {
		t.Errorf("a gigabyte onto 2 free, leaving 2 alone = %v", err)
	}
}
