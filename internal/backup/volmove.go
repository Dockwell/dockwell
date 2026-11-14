// SPDX-License-Identifier: AGPL-3.0-only

package backup

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"maps"
	"os"
	"path/filepath"
	"syscall"

	resourcev1 "github.com/dockwell/dockwell/gen/dockwell/resource/v1alpha1"
	"github.com/dockwell/dockwell/internal/docker"
	"github.com/dockwell/dockwell/internal/human"
	"github.com/dockwell/dockwell/internal/resource"
	"github.com/dockwell/dockwell/internal/resource/volume"
)

// Moving one volume's data into another, as moving an app to another project does
// (ADR 0084, #572). Docker can't rename a volume, and Dockwell finds one by its name,
// so the data goes into a volume of the new name: for a local volume with no
// options, by renaming the old one's folder into the new one's place, which takes no
// time and no room; for one with options, by making the new one with the same, so
// it reaches the same data; and otherwise by copying the files and emptying the old
// one. Nothing may mount either while it moves.

var _ volume.DataMover = (*Runner)(nil)

// movesDir is where a copy keeps how far it got, under the agent's backups.
const movesDir = "moves"

// moveRecord is how far a copy into a volume got, so a step run again carries on
// rather than copying over what it finished, or losing what it emptied.
type moveRecord struct {
	Move resource.VolumeMove
	// Copied says the copy finished, and only emptying the old volume is left.
	Copied bool `json:",omitempty"`
}

func (r *Runner) movePath(to string) string {
	return filepath.Join(r.Root, movesDir, to+".json")
}

func (r *Runner) readMove(to string) (*moveRecord, error) {
	b, err := os.ReadFile(r.movePath(to))
	if errors.Is(err, fs.ErrNotExist) {
		return nil, nil
	}
	if err != nil {
		return nil, fmt.Errorf("can't read how far the move into %s got: %w", to, err)
	}
	var rec moveRecord
	if err := json.Unmarshal(b, &rec); err != nil {
		return nil, fmt.Errorf("can't read how far the move into %s got, in %s: %w", to, r.movePath(to), err)
	}
	return &rec, nil
}

func (r *Runner) saveMove(to string, rec *moveRecord) error {
	path := r.movePath(to)
	if rec == nil {
		if err := os.Remove(path); err != nil && !errors.Is(err, fs.ErrNotExist) {
			return fmt.Errorf("can't finish the move into %s: %w", to, err)
		}
		return nil
	}
	b, err := json.Marshal(rec)
	if err != nil {
		return err
	}
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		return fmt.Errorf("can't keep how far the move into %s got: %w", to, err)
	}
	if err := os.WriteFile(path+".new", b, 0o600); err != nil {
		return fmt.Errorf("can't keep how far the move into %s got: %w", to, err)
	}
	if err := os.Rename(path+".new", path); err != nil {
		return fmt.Errorf("can't keep how far the move into %s got: %w", to, err)
	}
	return nil
}

// JudgeMoveData says how the data of the volume from moves into t, made as spec,
// and reports false when there's nothing left to move. A copy is refused here, before
// anything stops, when the disk has no room for it.
func (r *Runner) JudgeMoveData(ctx context.Context, from string, t volume.Target, spec *resourcev1.Volume) (resource.VolumeMove, bool, error) {
	if rec, err := r.readMove(t.Name); err != nil {
		return resource.VolumeMove{}, false, err
	} else if rec != nil && rec.Move.From == from {
		return rec.Move, true, nil
	}
	// Moving back what a copy into from didn't finish: from has part of it, and
	// t all, as the copy empties t only once it's done.
	if back, err := r.readMove(from); err != nil {
		return resource.VolumeMove{}, false, err
	} else if back != nil && back.Move.From == t.Name && !back.Copied {
		return resource.VolumeMove{}, false, nil
	}
	src, err := r.source4Move(ctx, from)
	if err != nil || src == nil {
		return resource.VolumeMove{}, false, err
	}
	move := resource.VolumeMove{From: from}
	if len(src.Options) > 0 {
		if !sameVolume(src, spec) {
			return move, false, fmt.Errorf("%s reaches its data through its options, so %s takes it by reaching the same, and needs the same driver and options", from, t.Name)
		}
		move.How = resource.MoveSameOptions
		there, err := r.Client.InspectVolume(ctx, t.Name)
		if err != nil && !docker.IsNotFound(err) {
			return move, false, err
		}
		return move, there == nil, nil
	}
	if spec.GetDriver() != src.Driver && (spec.GetDriver() != "" || src.Driver != "local") || len(spec.GetOptions()) > 0 {
		return move, false, fmt.Errorf("%s takes the data of %s, so it's made the same way: the %s driver, with no options", t.Name, from, src.Driver)
	}
	if src.Driver == "local" && src.Mountpoint != "" {
		switch here, err := inPlace(src.Mountpoint); {
		case err != nil:
			return move, false, err
		case here == placeEmpty:
			return move, false, nil
		case here != placeElsewhere:
			move.How = resource.MoveInPlace
			return move, true, nil
		}
	}
	// Copied, as a restore copies, after a check there's room for it.
	stats, err := r.tool(ctx, volume.Target{Name: from, Key: from}, "measure", docker.Streams{})
	if err != nil {
		return move, false, fmt.Errorf("can't measure %s to copy it: %w", from, err)
	}
	if stats.Files <= 1 {
		return move, false, nil
	}
	move.How, move.Bytes = resource.MoveCopy, stats.Bytes
	return move, true, roomToCopy(stats.Free, stats.Size, stats.Bytes, from)
}

// source4Move reads the volume a move takes its data from, or nil when it's gone. It
// must be a project's volume, as Dockwell makes them.
func (r *Runner) source4Move(ctx context.Context, from string) (*docker.Volume, error) {
	src, err := r.Client.InspectVolume(ctx, from)
	switch {
	case docker.IsNotFound(err):
		return nil, nil
	case err != nil:
		return nil, fmt.Errorf("can't read the volume %s: %w", from, err)
	}
	project, key := src.Labels[docker.LabelProject], src.Labels[docker.LabelVolume]
	if project == "" || key == "" || docker.ScopedName(project, key) != from {
		return nil, fmt.Errorf("%s isn't a volume of a project, so Dockwell doesn't move its data", from)
	}
	return src, nil
}

// sameVolume reports whether spec makes a volume as src was made.
func sameVolume(src *docker.Volume, spec *resourcev1.Volume) bool {
	driver := spec.GetDriver()
	if driver == "" {
		driver = "local"
	}
	return driver == src.Driver && maps.Equal(src.Options, spec.GetOptions())
}

// Where a local volume's folder is, for a move in place.
const (
	placeFull      = iota // it holds something, on the filesystem beside it
	placeEmpty            // it holds nothing, so there's nothing to move
	placeGone             // an earlier run renamed it, and stopped before making it again
	placeElsewhere        // this agent can't see it, or it's on a filesystem of its own
)

// inPlace says where the folder at data, a local volume's, is.
func inPlace(data string) (int, error) {
	parent, err := os.Lstat(filepath.Dir(data))
	if errors.Is(err, fs.ErrNotExist) {
		return placeElsewhere, nil // as with Docker in a virtual machine
	}
	if err != nil {
		return 0, fmt.Errorf("can't read the folder of %s: %w", data, err)
	}
	fi, err := os.Lstat(data)
	switch {
	case errors.Is(err, fs.ErrNotExist):
		return placeGone, nil
	case err != nil:
		return 0, fmt.Errorf("can't read %s: %w", data, err)
	case !fi.IsDir() || !parent.IsDir():
		return placeElsewhere, nil
	}
	// The volumes' own folder, above it, is where the new one goes.
	root, err := os.Stat(filepath.Dir(filepath.Dir(data)))
	if err != nil {
		return placeElsewhere, nil //nolint:nilerr // can't see where the new one goes, so it's copied
	}
	if deviceOf(fi) != deviceOf(root) || deviceOf(parent) != deviceOf(root) {
		return placeElsewhere, nil
	}
	empty, err := emptyDir(data)
	if err != nil {
		return 0, err
	}
	if empty {
		return placeEmpty, nil
	}
	return placeFull, nil
}

func deviceOf(fi fs.FileInfo) uint64 {
	if st, ok := fi.Sys().(*syscall.Stat_t); ok {
		return uint64(st.Dev) //nolint:unconvert // Dev's type differs by architecture
	}
	return 0
}

func emptyDir(dir string) (bool, error) {
	f, err := os.Open(dir) //nolint:gosec // a volume's folder, under Docker's own
	if err != nil {
		return false, fmt.Errorf("can't read %s: %w", dir, err)
	}
	defer f.Close()
	if _, err := f.Readdirnames(1); errors.Is(err, io.EOF) {
		return true, nil
	} else if err != nil {
		return false, fmt.Errorf("can't read %s: %w", dir, err)
	}
	return false, nil
}

// roomToCopy refuses a copy of size bytes onto a disk with free bytes of total, which
// leaves what a backup leaves alone.
func roomToCopy(free, total, size int64, from string) error {
	reserve := min(max(total/20, minReserve), maxReserve)
	needs := size + size/margin
	if free-needs >= reserve {
		return nil
	}
	return fmt.Errorf("the disk %s is on has %s free, and copying it takes about %s and leaves %s alone, so nothing stopped and nothing moved. Free some space, then plan the move again",
		from, human.Size(free), human.Size(needs), human.Size(reserve))
}

// MoveData moves the data into t, which is made by then, as move says. A step run
// again carries on from where the last stopped.
func (r *Runner) MoveData(ctx context.Context, move resource.VolumeMove, t volume.Target) error {
	for _, name := range []string{move.From, t.Name} {
		users, err := r.Client.ListContainers(ctx, map[string][]string{"volume": {name}})
		if err != nil {
			return fmt.Errorf("can't read what mounts %s: %w", name, err)
		}
		if len(users) > 0 {
			return fmt.Errorf("a container mounts %s, so its data can't move. The change deletes what mounted it before it moves the data; plan the move again", name)
		}
	}
	release, err := r.hold(volume.Target{Name: move.From, Key: move.From})
	if err != nil {
		return err
	}
	defer release()
	switch move.How {
	case resource.MoveSameOptions:
		return nil
	case resource.MoveInPlace:
		return r.moveInPlace(ctx, move.From, t)
	case resource.MoveCopy:
		return r.moveByCopy(ctx, move, t)
	}
	return fmt.Errorf("this agent doesn't know how to move a volume's data %s. Upgrade it, then plan the move again", move.How)
}

// moveInPlace renames the folder of the volume from into the place of t's, which
// must be empty, and makes from's again, empty. Each step leaves what a run again
// can tell and carry on from.
func (r *Runner) moveInPlace(ctx context.Context, from string, t volume.Target) error {
	src, err := r.Client.InspectVolume(ctx, from)
	if err != nil {
		return fmt.Errorf("can't read the volume %s: %w", from, err)
	}
	dst, err := r.Client.InspectVolume(ctx, t.Name)
	if err != nil {
		return fmt.Errorf("can't read the volume %s: %w", t.Name, err)
	}
	if src.Mountpoint == "" || dst.Mountpoint == "" {
		return fmt.Errorf("docker doesn't say where %s or %s keeps its files, so the data can't move in place. Plan the move again", from, t.Name)
	}
	here, err := inPlace(src.Mountpoint)
	switch {
	case err != nil:
		return err
	case here == placeEmpty:
		return nil
	case here == placeGone:
		return remake(src.Mountpoint)
	case here == placeElsewhere:
		return fmt.Errorf("the files of %s aren't where this agent can rename them any more, so they can't move in place. Plan the move again, which copies them", from)
	}
	if fi, err := os.Lstat(dst.Mountpoint); err == nil {
		if !fi.IsDir() {
			return fmt.Errorf("%s isn't a folder, so nothing moves into it", dst.Mountpoint)
		}
		// Docker made it empty; anything in it is someone's, and stays.
		if err := os.Remove(dst.Mountpoint); err != nil {
			return fmt.Errorf("%s already holds files, so the data of %s doesn't move over them: %w", t.Name, from, err)
		}
	} else if !errors.Is(err, fs.ErrNotExist) {
		return fmt.Errorf("can't read %s: %w", dst.Mountpoint, err)
	}
	if err := os.Rename(src.Mountpoint, dst.Mountpoint); err != nil {
		return fmt.Errorf("can't move the files of %s into %s: %w", from, t.Name, err)
	}
	syncDir(filepath.Dir(dst.Mountpoint))
	syncDir(filepath.Dir(src.Mountpoint))
	return remake(src.Mountpoint)
}

// remake makes a local volume's folder again, empty, as Docker makes one.
func remake(data string) error {
	if err := os.Mkdir(data, 0o755); err != nil && !errors.Is(err, fs.ErrExist) { //nolint:gosec // as Docker makes it
		return fmt.Errorf("can't make %s again, empty: %w", data, err)
	}
	syncDir(filepath.Dir(data))
	return nil
}

func syncDir(dir string) {
	if f, err := os.Open(dir); err == nil { //nolint:gosec // a volume's folder, under Docker's own
		_ = f.Sync()
		_ = f.Close()
	}
}

// moveByCopy copies the files of the volume from into t, then empties from, keeping
// how far it got, so a run again neither copies over what it finished nor loses what
// it emptied.
func (r *Runner) moveByCopy(ctx context.Context, move resource.VolumeMove, t volume.Target) error {
	rec, err := r.readMove(t.Name)
	if err != nil {
		return err
	}
	if rec != nil && rec.Move.From != move.From {
		rec = nil // a copy into t from elsewhere, which a rollback gave up on
	}
	src := volume.Target{Name: move.From, Key: move.From}
	// A copy into from, from t, that finished but hadn't emptied t when its change
	// was rolled back: from has all of it, and t what emptying left.
	if back, err := r.readMove(move.From); err != nil {
		return err
	} else if back != nil && back.Move.From == t.Name && back.Copied {
		if _, err := r.tool(ctx, t, "empty", docker.Streams{}); err != nil {
			return fmt.Errorf("can't empty %s to move its data back: %w", t.Name, err)
		}
		if err := r.saveMove(move.From, nil); err != nil {
			return err
		}
	}
	if rec == nil {
		have, err := r.tool(ctx, src, "measure", docker.Streams{})
		if err != nil {
			return fmt.Errorf("can't measure %s to copy it: %w", move.From, err)
		}
		if have.Files <= 1 {
			return nil
		}
		there, err := r.tool(ctx, t, "measure", docker.Streams{})
		if err != nil {
			return fmt.Errorf("can't measure %s to copy into it: %w", t.Name, err)
		}
		if there.Files > 1 {
			return fmt.Errorf("%s already holds files, so the data of %s doesn't move over them", t.Name, move.From)
		}
		if err := roomToCopy(there.Free, there.Size, have.Bytes, move.From); err != nil {
			return err
		}
		rec = &moveRecord{Move: move}
		if err := r.saveMove(t.Name, rec); err != nil {
			return err
		}
	}
	if !rec.Copied {
		resource.Progress(ctx, "Copying "+move.From+" into "+t.Key)
		if err := r.copyInto(ctx, src, t); err != nil {
			return err
		}
		rec.Copied = true
		if err := r.saveMove(t.Name, rec); err != nil {
			return err
		}
	}
	if _, err := r.tool(ctx, src, "empty", docker.Streams{}); err != nil {
		return fmt.Errorf("copied %s into %s, but can't empty it: %w", move.From, t.Name, err)
	}
	return r.saveMove(t.Name, nil)
}

// copyInto packs one volume into another, through a pipe, each in a helper. The
// unpack replaces whatever an earlier run left in to.
func (r *Runner) copyInto(ctx context.Context, from, to volume.Target) error {
	ctx, cancel := context.WithCancel(ctx)
	defer cancel()
	pr, pw := io.Pipe()
	packed := make(chan error, 1)
	go func() {
		_, err := r.tool(ctx, from, "pack", docker.Streams{Stdout: pw})
		_ = pw.CloseWithError(err)
		if err != nil {
			cancel()
		}
		packed <- err
	}()
	_, err := r.tool(ctx, to, "unpack", docker.Streams{Stdin: pr})
	_ = pr.CloseWithError(err)
	if perr := <-packed; perr != nil {
		return fmt.Errorf("can't read %s to copy it: %w", from.Name, perr)
	}
	if err != nil {
		return fmt.Errorf("can't copy %s into %s: %w", from.Name, to.Name, err)
	}
	return nil
}
