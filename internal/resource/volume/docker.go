// SPDX-License-Identifier: AGPL-3.0-only

package volume

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"log/slog"
	"maps"
	"os"
	"path/filepath"
	"slices"
	"sync"
	"time"

	resourcev1 "github.com/dockwell/dockwell/gen/dockwell/resource/v1alpha1"
	"github.com/dockwell/dockwell/internal/docker"
	"github.com/dockwell/dockwell/internal/human"
	"github.com/dockwell/dockwell/internal/resource"
)

// Kept lists the volumes deleted through plans that are still on the
// server, each with when it was deleted. Docker can't rename a volume or
// change its labels, so the list is the agent's own, in a file in its data
// directory. Without a Path it is kept in memory.
type Kept struct {
	Path string

	mu  sync.Mutex
	mem map[string]time.Time
}

// load reads the list. Call with mu held.
func (k *Kept) load() (map[string]time.Time, error) {
	if k.Path == "" {
		if k.mem == nil {
			k.mem = map[string]time.Time{}
		}
		return k.mem, nil
	}
	b, err := os.ReadFile(k.Path)
	if errors.Is(err, fs.ErrNotExist) {
		return map[string]time.Time{}, nil
	}
	if err != nil {
		return nil, fmt.Errorf("can't read the list of deleted volumes: %w", err)
	}
	out := map[string]time.Time{}
	if err := json.Unmarshal(b, &out); err != nil {
		return nil, fmt.Errorf("can't read the list of deleted volumes in %s: %w. Remove the file to forget them; they stay on the server until a docker volume prune", k.Path, err)
	}
	return out, nil
}

// save writes the list. Call with mu held.
func (k *Kept) save(m map[string]time.Time) error {
	if k.Path == "" {
		k.mem = m
		return nil
	}
	b, err := json.MarshalIndent(m, "", "  ")
	if err != nil {
		return err
	}
	tmp := k.Path + ".new"
	if err := os.MkdirAll(filepath.Dir(k.Path), 0o700); err != nil {
		return err
	}
	if err := os.WriteFile(tmp, b, 0o600); err != nil {
		return fmt.Errorf("can't keep the list of deleted volumes: %w", err)
	}
	return os.Rename(tmp, k.Path)
}

// update changes the list under its lock.
func (k *Kept) update(fn func(map[string]time.Time) bool) error {
	k.mu.Lock()
	defer k.mu.Unlock()
	m, err := k.load()
	if err != nil {
		return err
	}
	if fn(m) {
		return k.save(m)
	}
	return nil
}

// Since returns when a kept volume was deleted.
func (k *Kept) Since(name string) (time.Time, bool, error) {
	k.mu.Lock()
	defer k.mu.Unlock()
	m, err := k.load()
	if err != nil {
		return time.Time{}, false, err
	}
	at, ok := m[name]
	return at, ok, nil
}

// Docker is a Runtime on the Docker Engine.
type Docker struct {
	Client *docker.Client
	Kept   *Kept
	// Now is the clock; nil means time.Now.
	Now func() time.Time
}

var _ Runtime = (*Docker)(nil)

func (d *Docker) now() time.Time {
	if d.Now != nil {
		return d.Now()
	}
	return time.Now()
}

// labels are what a volume at t carries for spec.
func labels(t Target, spec *resourcev1.Volume) map[string]string {
	l := maps.Clone(spec.GetLabels())
	if l == nil {
		l = map[string]string{}
	}
	l[docker.LabelProject] = t.Project
	l[docker.LabelVolume] = t.Key
	return l
}

// Project reads a volume as Docker reports it back into a spec, without
// Dockwell's and Compose's labels.
func Project(v *docker.Volume) *resourcev1.Volume {
	out := &resourcev1.Volume{Driver: v.Driver}
	for k, val := range v.Options {
		if out.Options == nil {
			out.Options = map[string]string{}
		}
		out.Options[k] = val
	}
	for k, val := range v.Labels {
		if docker.IsOwnLabel(k) {
			continue
		}
		if out.Labels == nil {
			out.Labels = map[string]string{}
		}
		out.Labels[k] = val
	}
	return out
}

// Inspect reads the volume; one that is only kept doesn't exist.
func (d *Docker) Inspect(ctx context.Context, t Target) (*resourcev1.Volume, error) {
	if _, kept, err := d.Kept.Since(t.Name); err != nil || kept {
		return nil, err
	}
	v, err := d.Client.InspectVolume(ctx, t.Name)
	switch {
	case docker.IsNotFound(err):
		return nil, nil
	case err != nil:
		return nil, err
	}
	return Project(v), nil
}

// Create makes the volume. One deleted and kept comes back with its data,
// as undoing its deletion asks, if it was made the same way.
func (d *Docker) Create(ctx context.Context, t Target, spec *resourcev1.Volume) error {
	deleted, kept, err := d.Kept.Since(t.Name)
	if err != nil {
		return err
	}
	if kept {
		v, err := d.Client.InspectVolume(ctx, t.Name)
		switch {
		case docker.IsNotFound(err):
			// Removed by hand since: make it anew.
		case err != nil:
			return err
		default:
			if !resource.Equal(normal(Project(v)), normal(spec)) {
				return fmt.Errorf("the volume %s was deleted %s, and is kept with its data until %s, but was made another way, so it can't come back as asked for. "+
					"Ask for it as it was, or remove the old one first with docker volume rm %s, which destroys its data",
					t.Name, deleted.Format(time.DateOnly), deleted.Add(KeepFor).Format(time.DateOnly), t.Name)
			}
			return d.Kept.update(func(m map[string]time.Time) bool { delete(m, t.Name); return true })
		}
		if err := d.Kept.update(func(m map[string]time.Time) bool { delete(m, t.Name); return true }); err != nil {
			return err
		}
	}
	_, err = d.Client.CreateVolume(ctx, &docker.VolumeCreate{
		Name: t.Name, Driver: spec.GetDriver(), DriverOpts: spec.GetOptions(), Labels: labels(t, spec),
	})
	if err != nil {
		return fmt.Errorf("docker can't create the volume %s: %w", t.Name, err)
	}
	return nil
}

func normal(v *resourcev1.Volume) *resourcev1.Volume {
	if v.Driver == "" {
		v.Driver = "local"
	}
	return v
}

// Delete keeps the volume, with its data, for KeepFor; Prune removes it
// after.
func (d *Docker) Delete(ctx context.Context, t Target) error {
	if _, err := d.Client.InspectVolume(ctx, t.Name); docker.IsNotFound(err) {
		return nil
	} else if err != nil {
		return err
	}
	at := d.now()
	return d.Kept.update(func(m map[string]time.Time) bool {
		if _, ok := m[t.Name]; ok {
			return false
		}
		m[t.Name] = at
		return true
	})
}

// Prune removes the volumes kept for KeepFor or longer. One a container
// still uses stays, and is tried again on the next prune.
func (d *Docker) Prune(ctx context.Context, logger *slog.Logger) error {
	d.Kept.mu.Lock()
	m, err := d.Kept.load()
	d.Kept.mu.Unlock()
	if err != nil {
		return err
	}
	now := d.now()
	for _, name := range slices.Sorted(maps.Keys(m)) {
		deleted := m[name]
		if now.Sub(deleted) < KeepFor {
			continue
		}
		err := d.Client.RemoveVolume(ctx, name)
		switch {
		case err == nil, docker.IsNotFound(err):
			logger.Info("removed a deleted volume and its data, kept for long enough",
				slog.String("volume", name), slog.Time("deleted", deleted), slog.String("kept_for", human.Duration(KeepFor)))
		case docker.IsConflict(err):
			logger.Warn("a deleted volume is still in use, so it stays until no container uses it",
				slog.String("volume", name), slog.String("error", err.Error()))
			continue
		default:
			return fmt.Errorf("can't remove the deleted volume %s: %w", name, err)
		}
		if err := d.Kept.update(func(m map[string]time.Time) bool { delete(m, name); return true }); err != nil {
			return err
		}
	}
	return nil
}

// PruneEvery is how often KeepPruned prunes.
const PruneEvery = time.Hour

// KeepPruned prunes now and every PruneEvery until ctx is done.
func (d *Docker) KeepPruned(ctx context.Context, logger *slog.Logger) {
	t := time.NewTicker(PruneEvery)
	defer t.Stop()
	for {
		if err := d.Prune(ctx, logger); err != nil && !docker.IsUnavailable(err) && ctx.Err() == nil {
			logger.Warn("can't remove deleted volumes yet; the agent tries again in an hour", slog.String("error", err.Error()))
		}
		select {
		case <-ctx.Done():
			return
		case <-t.C:
		}
	}
}

// Ready checks Docker answers.
func (d *Docker) Ready(ctx context.Context) error { return d.Client.Ping(ctx) }
