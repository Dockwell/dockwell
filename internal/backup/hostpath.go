// SPDX-License-Identifier: AGPL-3.0-only

package backup

import (
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"path"
	"slices"
	"strings"

	resourcev1 "github.com/dockwell/dockwell/gen/dockwell/resource/v1alpha1"
	"github.com/dockwell/dockwell/internal/docker"
	"github.com/dockwell/dockwell/internal/human"
	"github.com/dockwell/dockwell/internal/resource"
	"github.com/dockwell/dockwell/internal/resource/container"
	"github.com/dockwell/dockwell/internal/volumetool"
)

// validateHostPath verifies that hostPath is a clean, allowed host directory path
// and resolves symlinks through r.Paths when present.
func (r *Runner) validateHostPath(ctx context.Context, hostPath string) (string, error) {
	if !path.IsAbs(hostPath) {
		return "", fmt.Errorf("website folder %q must be an absolute path", hostPath)
	}
	if err := container.CheckSpelling(hostPath, "website folder "+hostPath); err != nil {
		return "", err
	}
	clean := path.Clean(hostPath)
	if err := container.DefaultHost.CheckPath(clean, "website folder "+clean); err != nil {
		return "", err
	}
	if r.Paths != nil {
		leads, err := r.Paths.Follow(ctx, clean)
		if err != nil {
			return "", fmt.Errorf("can't check website folder %s: %w", clean, err)
		}
		if resolved := leads[clean]; resolved != "" && resolved != clean {
			if err := container.DefaultHost.CheckPath(resolved, "website folder "+clean+" (leads to "+resolved+")"); err != nil {
				return "", err
			}
		}
	}
	return clean, nil
}

func (r *Runner) holdHostPath(hostPath string) (release func(), err error) {
	key := "host:" + hostPath
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.working[key] {
		return nil, fmt.Errorf("a backup or restore of %s is under way. Wait for it to end, then try again", hostPath)
	}
	if r.working == nil {
		r.working = map[string]bool{}
	}
	r.working[key] = true
	return func() {
		r.mu.Lock()
		defer r.mu.Unlock()
		delete(r.working, key)
	}, nil
}

// usersForHostPath returns containers in ref's Compose project that mount hostPath.
func (r *Runner) usersForHostPath(ctx context.Context, ref resource.Ref, hostPath string) (*users, error) {
	proj := docker.Project(ref.Scope)
	found, err := r.Client.ListContainers(ctx, map[string][]string{"label": {docker.LabelProject + "=" + proj}})
	if err != nil {
		return nil, err
	}
	out := &users{}
	for i := range found {
		s := &found[i]
		if _, helper := s.Labels[labelHelper]; helper {
			continue
		}
		c, err := r.Client.InspectContainer(ctx, s.ID)
		if docker.IsNotFound(err) {
			continue
		}
		if err != nil {
			return nil, err
		}
		mountsPath := false
		for _, m := range c.Mounts {
			if m.Type != "bind" && m.Source == "" {
				continue
			}
			src := path.Clean(m.Source)
			if src == hostPath || strings.HasPrefix(src, hostPath+"/") || strings.HasPrefix(hostPath, src+"/") {
				mountsPath = true
				break
			}
		}
		if !mountsPath {
			continue
		}
		out.list = append(out.list, userOf(c))
	}
	slices.SortFunc(out.list, func(a, b *user) int { return strings.Compare(a.name, b.name) })
	return out, nil
}

func hostBindMount(hostPath, verb string) docker.Mount {
	return docker.Mount{
		Type:     "bind",
		Source:   hostPath,
		Target:   helperMount,
		ReadOnly: verb == "pack" || verb == "measure",
	}
}

// PreviewHostPath says what backing up a website folder would do.
func (r *Runner) PreviewHostPath(ctx context.Context, ref resource.Ref, hostPath string) (Preview, error) {
	clean, err := r.validateHostPath(ctx, hostPath)
	if err != nil {
		return Preview{}, err
	}
	if fi, err := os.Stat(clean); err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return Preview{}, fmt.Errorf("the website folder %s doesn't exist on this server yet", clean)
		}
		return Preview{}, fmt.Errorf("can't read website folder %s: %w", clean, err)
	} else if !fi.IsDir() {
		return Preview{}, fmt.Errorf("%s isn't a directory", clean)
	}
	u, err := r.usersForHostPath(ctx, ref, clean)
	if err != nil {
		return Preview{}, err
	}
	p := Preview{Method: resource.BackupCopy}
	running := u.writing()
	p.Running = namesOf(running)
	if len(running) > 0 {
		p.Method = resource.BackupCopyRunning
	}
	disk, err := r.disk()
	if err != nil {
		return Preview{}, err
	}
	p.Free, p.Reserve = disk.free, disk.reserve
	var known bool
	p.Estimate, known = r.estimateHostPath(ctx, ref, clean)
	p.Unmeasured = !known
	return p, nil
}

func (r *Runner) estimateHostPath(ctx context.Context, ref resource.Ref, hostPath string) (int64, bool) {
	if size := r.lastSize(ref); size > 0 {
		return size, true
	}
	ctx, cancel := context.WithTimeout(ctx, measureWait)
	defer cancel()
	stats, err := r.toolMount(ctx, hostBindMount(hostPath, "measure"), hostPath, "measure", docker.Streams{})
	if err != nil {
		if !errors.Is(err, context.DeadlineExceeded) && ctx.Err() == nil {
			r.logger().Warn("can't measure website folder", "path", hostPath, "error", err.Error())
		}
		return 0, false
	}
	return stats.Bytes, true
}

func (r *Runner) roomForHostPath(ctx context.Context, ref resource.Ref, hostPath string) error {
	d, err := r.disk()
	if err != nil {
		return err
	}
	size, _ := r.estimateHostPath(ctx, ref, hostPath)
	needs := size + size/margin
	if held := r.drillHeld(d.filesystem); held > 0 && d.free-needs-held < d.reserve {
		r.yieldDrill(d.filesystem)
		if d, err = r.disk(); err != nil {
			return err
		}
	}
	if d.free-needs >= d.reserve {
		return nil
	}
	return tooFull(d, needs)
}

func (r *Runner) takeHostPath(ctx context.Context, take Take) (_ *Manifest, err error) {
	clean, err := r.validateHostPath(ctx, take.HostPath)
	if err != nil {
		return nil, err
	}
	release, err := r.holdHostPath(clean)
	if err != nil {
		return nil, err
	}
	defer release()
	if fi, err := os.Stat(clean); err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return nil, fmt.Errorf("the website folder %s doesn't exist on this server yet", clean)
		}
		return nil, fmt.Errorf("can't read website folder %s: %w", clean, err)
	} else if !fi.IsDir() {
		return nil, fmt.Errorf("%s isn't a directory", clean)
	}
	u, err := r.usersForHostPath(ctx, take.Ref, clean)
	if err != nil {
		return nil, err
	}
	if err := r.roomForHostPath(ctx, take.Ref, clean); err != nil {
		return nil, err
	}
	if err := os.MkdirAll(r.dir(take.Ref), 0o700); err != nil {
		return nil, fmt.Errorf("can't make a place for the backup: %w", err)
	}
	m := &Manifest{
		ID:          take.ID,
		Environment: take.Ref.Scope,
		Volume:      take.Ref.Name,
		Format:      formatArchive,
		Method:      resource.BackupCopy,
		Taken:       r.now().UTC(),
	}
	running := u.writing()
	if len(running) > 0 && !take.Stop {
		m.Method = resource.BackupCopyRunning
	}
	var stopped []*user
	startAgain := func() error {
		if len(stopped) == 0 {
			return nil
		}
		toStart := stopped
		stopped = nil
		return r.startAgain(ctx, take.ID, toStart)
	}
	defer func() { err = errors.Join(err, startAgain()) }()
	if take.Stop {
		for _, c := range running {
			if err := r.withRunState(c.id, func(state resourcev1.DesiredRunState) error {
				actual, err := r.Client.InspectContainer(ctx, c.id)
				if docker.IsNotFound(err) {
					return nil
				}
				if err != nil {
					return err
				}
				if !actual.State.Running || actual.State.Paused {
					return nil
				}
				if state == resourcev1.DesiredRunState_DESIRED_RUN_STATE_PAUSED {
					return fmt.Errorf("%s is running while its revision says paused. Back up again once its pause is restored", c.name)
				}
				stopped = append(stopped, c)
				return r.stop(ctx, take.ID, []*user{c}, false)
			}); err != nil {
				return nil, err
			}
		}
	}
	err = r.write(take.Ref, m, func(w io.Writer) (int64, error) {
		stats, err := r.toolMount(ctx, hostBindMount(clean, "pack"), clean, "pack", docker.Streams{Stdout: w})
		m.Unpacked = stats.Bytes
		return stats.Changed, errors.Join(err, startAgain())
	}, func(rd io.Reader) error {
		read, err := volumetool.Verify(rd)
		m.Files = read.Files
		return err
	})
	if err != nil {
		return nil, err
	}
	return m, nil
}

func (r *Runner) judgeRestoreHostPath(ctx context.Context, ref resource.Ref, src resource.RestoreSource) (resource.Decision, bool, error) {
	if s, err := r.stageOf(ref, src.Change); err != nil {
		return resource.Decision{}, false, err
	} else if s.Stage == stageFinished {
		return resource.Decision{}, false, nil
	}
	m, _, err := r.source(ref, src)
	if err != nil {
		return resource.Decision{}, false, err
	}
	clean, err := r.validateHostPath(ctx, src.HostPath)
	if err != nil {
		return resource.Decision{}, false, err
	}
	u, err := r.usersForHostPath(ctx, ref, clean)
	if err != nil {
		return resource.Decision{}, false, err
	}
	stops := u.running()
	dec := resource.Decision{
		Action:   resource.ActionReplace,
		Effect:   resource.EffectRestart,
		Destroys: true,
		Restore: &resource.RestorePlan{
			Backup: src.Backup,
			Undoes: src.Undoes,
			Method: m.Method,
			Taken:  m.Taken,
			Size:   m.Size,
			Stops:  namesOf(stops),
		},
	}
	if len(stops) > 0 {
		dec.Downtime = estimate(m)
	}
	return dec, true, nil
}

func (r *Runner) applyRestoreHostPath(ctx context.Context, step resource.Step, src resource.RestoreSource) error {
	ref := step.Ref
	clean, err := r.validateHostPath(ctx, src.HostPath)
	if err != nil {
		return err
	}
	release, err := r.holdHostPath(clean)
	if err != nil {
		return err
	}
	defer release()
	st, err := r.stageOf(ref, src.Change)
	if err != nil {
		return err
	}
	if st.Stage == stageFinished {
		return nil
	}
	if st.Stage == stageUndone {
		st.Stage = stageCopied
	}
	run := "restore-" + src.Change
	began, err := r.began(st, run)
	if err != nil {
		return err
	}
	u := &users{}
	stopping := func(err error) error {
		if !began {
			return err
		}
		return r.giveBackHostPath(ctx, ref, clean, st, run, u, err)
	}
	m, file, err := r.source(ref, src)
	if err != nil {
		return stopping(err)
	}
	resource.Progress(ctx, "Checking the backup")
	if err := check(file, m, verifyArchive); err != nil {
		if began {
			return stopping(fmt.Errorf("the backup is damaged: %w. Choose another", err))
		}
		return fmt.Errorf("the backup is damaged, so nothing was changed or stopped: %w. Choose another", err)
	}
	if err := os.MkdirAll(clean, 0o755); err != nil { //nolint:gosec // website document root must be readable by non-root web server processes
		return stopping(fmt.Errorf("can't ensure website folder %s exists: %w", clean, err))
	}
	if u, err = r.usersForHostPath(ctx, ref, clean); err != nil {
		u = &users{}
		return stopping(err)
	}
	if st.Stage == "" {
		if err := r.roomToRestoreHostPath(ctx, clean, m, began); err != nil {
			return stopping(err)
		}
	}
	if err := r.adopt(run, src.Undoes); err != nil {
		return stopping(err)
	}
	if err := r.stop(ctx, run, u.running(), true); err != nil {
		return r.giveBackHostPath(ctx, ref, clean, st, run, u, err)
	}
	if err := r.replaceHostPath(ctx, ref, clean, st, file); err != nil {
		return r.giveBackHostPath(ctx, ref, clean, st, run, u, err)
	}
	if err := r.startAll(ctx, run, u, nil); err != nil {
		var down downList
		if errors.As(err, &down) {
			err = errors.New(down.why())
		}
		return r.giveBackHostPath(ctx, ref, clean, st, run, u, err)
	}
	return r.saveState(ref, st, stageFinished)
}

func (r *Runner) roomToRestoreHostPath(ctx context.Context, hostPath string, m *Manifest, began bool) error {
	r.yieldDrill("")
	refused := func(fact, fix string) error {
		if began {
			return fmt.Errorf("%s. %s", fact, fix)
		}
		return fmt.Errorf("%s, so nothing was stopped or changed. %s", fact, fix)
	}
	d, err := r.disk()
	if err != nil {
		return err
	}
	ctx, cancel := context.WithTimeout(ctx, measureWait)
	defer cancel()
	v, err := r.toolMount(ctx, hostBindMount(hostPath, "measure"), hostPath, "measure", docker.Streams{})
	if err != nil {
		return nil
	}
	copying := v.Bytes + v.Bytes/margin
	if copying > 0 && d.free-copying < d.reserve {
		return refused(fmt.Sprintf("the disk that holds backups has %s free, and a copy of what's in %s takes up to %s, leaving %s alone",
			human.Size(d.free), hostPath, human.Size(copying), human.Size(d.reserve)), "Delete old backups or free some space, then try again")
	}
	bringing := m.Unpacked
	free, keep := v.Free, int64(0)
	if v.Filesystem == d.filesystem {
		free, keep = free-copying, d.reserve
	}
	if bringing > 0 && free-bringing < keep {
		return refused(fmt.Sprintf("the disk that holds %s has %s free once a copy of what's there is taken, and this backup needs %s more there",
			hostPath, human.Size(max(free, 0)), human.Size(bringing)), "Free some space on that disk, then try again")
	}
	return nil
}

func (r *Runner) replaceHostPath(ctx context.Context, ref resource.Ref, hostPath string, st *state, file string) error {
	if !reached(st.Stage, stageCopied) {
		resource.Progress(ctx, "Copying what's in "+hostPath+" now, which the server keeps for 7 days")
		st.Manifest = Manifest{
			ID: beforePrefix + st.Change, Environment: ref.Scope, Volume: ref.Name, Format: formatArchive, Method: resource.BackupCopy, Taken: r.now().UTC(),
		}
		if err := os.MkdirAll(r.dir(ref), 0o700); err != nil {
			return fmt.Errorf("can't make a place for what's there now: %w", err)
		}
		err := r.write(ref, &st.Manifest, func(w io.Writer) (int64, error) {
			stats, err := r.toolMount(ctx, hostBindMount(hostPath, "pack"), hostPath, "pack", docker.Streams{Stdout: w})
			st.Unpacked = stats.Bytes
			return stats.Changed, err
		}, verifyArchive)
		if err != nil {
			return fmt.Errorf("can't copy what's in %s now, so nothing was replaced: %w", hostPath, err)
		}
		if err := r.saveState(ref, st, stageCopied); err != nil {
			return err
		}
	}
	if reached(st.Stage, stageReplaced) {
		return nil
	}
	swapping, err := r.unpackHostPath(ctx, hostPath, file)
	if swapping && err != nil {
		err = errors.Join(err, r.saveState(ref, st, stageReplacing))
	}
	if err != nil {
		return err
	}
	return r.saveState(ref, st, stageReplaced)
}

func (r *Runner) unpackHostPath(ctx context.Context, hostPath, file string) (bool, error) {
	f, err := os.Open(file) //nolint:gosec // under the agent's own directory
	if err != nil {
		return false, err
	}
	defer f.Close()
	stats, err := r.toolMount(ctx, hostBindMount(hostPath, "unpack"), hostPath, "unpack", docker.Streams{Stdin: f})
	if err != nil {
		return stats.Swapping, fmt.Errorf("can't unpack the backup into %s: %w", hostPath, err)
	}
	return true, nil
}

func (r *Runner) giveBackHostPath(ctx context.Context, ref resource.Ref, hostPath string, st *state, run string, u *users, failed error) error {
	if st.Stage == stageReplacing || st.Stage == stageReplaced {
		if err := r.putBackHostPath(ctx, ref, hostPath, st, run); err != nil {
			return fmt.Errorf("%w. Putting back what was there failed too: %w. What stopped stays stopped, as what's in %s may not be whole. "+
				"A copy of what was there is at %s on this server. Run dockwell changes undo %s to try again",
				failed, err, hostPath, r.beforeFile(ref, st.Change), st.Change)
		}
	}
	if err := r.noteStopped(func(l stoppedFile) {
		if left, ok := l[run]; ok {
			left.Restore = false
			l[run] = left
		}
	}); err != nil {
		return errors.Join(failed, err)
	}
	if err := r.startAll(ctx, run, u, nil); err != nil {
		if st.Stage == stageUndone {
			return fmt.Errorf("%w. What was there is back in %s, and %w", failed, hostPath, err)
		}
		return fmt.Errorf("%w. %s still holds what it held, and %w", failed, hostPath, err)
	}
	if st.Stage == stageUndone {
		return fmt.Errorf("%w. What was there is back in %s, and what stopped started again", failed, hostPath)
	}
	return fmt.Errorf("%w. %s still holds what it held, and what stopped started again", failed, hostPath)
}

func (r *Runner) putBackHostPath(ctx context.Context, ref resource.Ref, hostPath string, st *state, run string) error {
	ctx, cancel := resource.Finishing(ctx, putBackWait)
	defer cancel()
	resource.Progress(ctx, "Putting back what was there")
	r.mu.Lock()
	list, err := r.readStopped()
	r.mu.Unlock()
	if err != nil {
		return err
	}
	for _, id := range list[run].Containers {
		if err := r.Client.StopContainer(ctx, id, stopWait); err != nil && !docker.IsNotFound(err) {
			return fmt.Errorf("can't stop %s: %w", shortID(id), err)
		}
	}
	if _, err := r.unpackHostPath(ctx, hostPath, r.beforeFile(ref, st.Change)); err != nil {
		return err
	}
	return r.saveState(ref, st, stageUndone)
}
