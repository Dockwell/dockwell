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
	"strings"
	"sync"
	"time"

	resourcev1 "github.com/dockwell/dockwell/gen/dockwell/resource/v1alpha1"
	"github.com/dockwell/dockwell/internal/docker"
	"github.com/dockwell/dockwell/internal/human"
	"github.com/dockwell/dockwell/internal/resource"
	"github.com/dockwell/dockwell/internal/resource/container"
)

// Kept lists the volumes deleted through plans that are still on the
// server, each with when it was deleted: an environment's volumes, and the
// volumes Docker made for a container's image, which deleting the
// container, or covering their path with a mount, took out of use. Docker
// can't rename a volume or change its labels, so the list is the agent's
// own. It is read from a file in the agent's data directory once, kept in
// memory, and each change is written through to the file before it
// counts. Without a Path it is kept in memory alone.
type Kept struct {
	Path string
	// Now is the clock for the image's volumes it keeps; nil means
	// time.Now.
	Now func() time.Time

	mu   sync.Mutex
	list map[string]kept // nil until read
}

// kept is one volume on the list.
type kept struct {
	Deleted time.Time `json:"deleted"`
	// Container and Path, for a volume Docker made for an image, are the
	// container it was mounted in, by name, and where.
	Container string `json:"container,omitempty"`
	Path      string `json:"path,omitempty"`
}

func (k *Kept) now() time.Time {
	if k.Now != nil {
		return k.Now()
	}
	return time.Now()
}

// load returns the list, reading it from its file the first time. Call
// with mu held.
func (k *Kept) load() (map[string]kept, error) {
	if k.list != nil {
		return k.list, nil
	}
	out := map[string]kept{}
	if k.Path != "" {
		b, err := os.ReadFile(k.Path)
		switch {
		case errors.Is(err, fs.ErrNotExist):
		case err != nil:
			return nil, fmt.Errorf("can't read the list of deleted volumes: %w", err)
		default:
			if err := json.Unmarshal(b, &out); err != nil {
				return nil, fmt.Errorf("can't read the list of deleted volumes in %s: %w. Remove the file to forget them; they stay on the server until a docker volume prune", k.Path, err)
			}
		}
	}
	k.list = out
	return out, nil
}

// save writes m to the file, then keeps it as the list. If it can't be
// written, the list stays as it was. Call with mu held.
func (k *Kept) save(m map[string]kept) error {
	if k.Path != "" {
		b, err := json.MarshalIndent(m, "", "  ")
		if err != nil {
			return err
		}
		tmp := k.Path + ".new"
		if err := os.MkdirAll(filepath.Dir(k.Path), 0o700); err != nil {
			return fmt.Errorf("can't keep the list of deleted volumes: %w", err)
		}
		if err := os.WriteFile(tmp, b, 0o600); err != nil {
			return fmt.Errorf("can't keep the list of deleted volumes: %w", err)
		}
		if err := os.Rename(tmp, k.Path); err != nil {
			return fmt.Errorf("can't keep the list of deleted volumes: %w", err)
		}
	}
	k.list = m
	return nil
}

// change changes a copy of the list, and keeps it if fn says it changed.
// Call with mu held.
func (k *Kept) change(fn func(map[string]kept) bool) error {
	m, err := k.load()
	if err != nil {
		return err
	}
	m = maps.Clone(m)
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
	e, ok := m[name]
	return e.Deleted, ok, nil
}

// Has reports whether a volume is kept: deleted, though still on the
// server.
func (k *Kept) Has(name string) (bool, error) {
	_, ok, err := k.Since(name)
	return ok, err
}

// KeepImageVolumes keeps the volumes Docker made for a container's image,
// by the path each was mounted at, as deleted now: Prune removes them
// once they have been kept for KeepFor, unless the container mounts them
// again first (TakeImageVolumes).
func (k *Kept) KeepImageVolumes(owner string, byPath map[string]string) error {
	if len(byPath) == 0 {
		return nil
	}
	at := k.now()
	k.mu.Lock()
	defer k.mu.Unlock()
	return k.change(func(m map[string]kept) bool {
		for path, name := range byPath {
			m[name] = kept{Deleted: at, Container: owner, Path: path}
		}
		return true
	})
}

// ImageVolumes returns the volumes kept for a container's image, by the
// path each was mounted at, and when each was deleted.
func (k *Kept) ImageVolumes(owner string) (map[string]string, map[string]time.Time, error) {
	k.mu.Lock()
	defer k.mu.Unlock()
	m, err := k.load()
	if err != nil {
		return nil, nil, err
	}
	byPath, since := map[string]string{}, map[string]time.Time{}
	for name, e := range m {
		if e.Container == owner && e.Path != "" {
			byPath[e.Path], since[e.Path] = name, e.Deleted
		}
	}
	return byPath, since, nil
}

// TakeImageVolumes takes the volumes at paths off the list, as the
// container mounts them again, so Prune leaves them. undo puts them back
// as they were, for a container that couldn't be made after all.
func (k *Kept) TakeImageVolumes(owner string, paths []string) (undo func(), err error) {
	k.mu.Lock()
	defer k.mu.Unlock()
	taken := map[string]kept{}
	err = k.change(func(m map[string]kept) bool {
		for name, e := range m {
			if e.Container == owner && slices.Contains(paths, e.Path) {
				taken[name] = e
				delete(m, name)
			}
		}
		return len(taken) > 0
	})
	undo = func() {
		k.mu.Lock()
		defer k.mu.Unlock()
		_ = k.change(func(m map[string]kept) bool {
			maps.Copy(m, taken)
			return len(taken) > 0
		})
	}
	return undo, err
}

// Docker is a Runtime on the Docker Engine.
type Docker struct {
	Client *docker.Client
	Kept   *Kept
	// Now is the clock; nil means time.Now.
	Now func() time.Time
	// Paths finds what no container may mount on this server, and follows
	// symlinks there, for a volume that binds a server path. Nil checks
	// only what Validate does.
	Paths *container.Paths
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
// as undoing its deletion asks, if it was made the same way. It holds the
// list while it does, so Prune can't remove the volume as it comes back.
// A plan that expected one back, when it is gone, fails rather than make
// the volume empty.
func (d *Docker) Create(ctx context.Context, t Target, spec *resourcev1.Volume, restore bool) error {
	if err := d.CheckHost(ctx, spec); err != nil {
		return err
	}
	back, err := d.bringBack(ctx, t, spec)
	if err != nil || back {
		return err
	}
	if restore {
		return fmt.Errorf("the volume %s was deleted, and the server kept it with its data for 7 days, but it's gone now, so this change can't bring the data back. "+
			"Plan the change again: it makes the volume anew, empty", t.Name)
	}
	_, err = d.Client.CreateVolume(ctx, &docker.VolumeCreate{
		Name: t.Name, Driver: spec.GetDriver(), DriverOpts: spec.GetOptions(), Labels: labels(t, spec),
	})
	if err != nil {
		return fmt.Errorf("docker can't create the volume %s: %w", t.Name, err)
	}
	return nil
}

// bringBack takes a kept volume off the list, and reports whether it is
// still on the server, so it comes back as it was.
func (d *Docker) bringBack(ctx context.Context, t Target, spec *resourcev1.Volume) (bool, error) {
	k := d.Kept
	k.mu.Lock()
	defer k.mu.Unlock()
	m, err := k.load()
	if err != nil {
		return false, err
	}
	e, isKept := m[t.Name]
	if !isKept {
		return false, nil
	}
	deleted := e.Deleted
	v, err := d.Client.InspectVolume(ctx, t.Name)
	switch {
	case docker.IsNotFound(err):
		// Removed by hand since: make it anew.
	case err != nil:
		return false, err
	case !resource.Equal(normal(Project(v)), normal(spec)):
		return false, fmt.Errorf("the volume %s was deleted %s, and is kept with its data until %s, but was made another way, so it can't come back as asked for. "+
			"Ask for it as it was, or remove the old one first with docker volume rm %s, which destroys its data",
			t.Name, deleted.Format(time.DateOnly), deleted.Add(KeepFor).Format(time.DateOnly), t.Name)
	}
	if err := k.change(func(m map[string]kept) bool { delete(m, t.Name); return true }); err != nil {
		return false, err
	}
	return v != nil, nil
}

// Restores reports whether creating the volume at t brings back one
// deleted and kept, which is still on the server.
func (d *Docker) Restores(ctx context.Context, t Target) (bool, error) {
	if _, kept, err := d.Kept.Since(t.Name); err != nil || !kept {
		return false, err
	}
	_, err := d.Client.InspectVolume(ctx, t.Name)
	switch {
	case docker.IsNotFound(err):
		return false, nil
	case err != nil:
		return false, err
	}
	return true, nil
}

// CheckHost refuses a volume that binds a server path no container may
// mount, as this server has it, symlinks followed.
func (d *Docker) CheckHost(ctx context.Context, spec *resourcev1.Volume) error {
	dev, ok := container.BindDevice(spec.GetDriver(), spec.GetOptions())
	if !ok || d.Paths == nil {
		return nil
	}
	h, err := d.Paths.Host(ctx)
	if err != nil {
		return err
	}
	return h.CheckPath(dev, "the server path "+dev+" that the volume binds")
}

func normal(v *resourcev1.Volume) *resourcev1.Volume {
	if v.Driver == "" {
		v.Driver = "local"
	}
	return v
}

// Delete keeps the volume, with its data, for KeepFor; Prune removes it
// after. One a container mounts can't go, as Docker refuses to remove it
// and the container would go on using it: delete what mounts it first.
func (d *Docker) Delete(ctx context.Context, t Target) error {
	if _, err := d.Client.InspectVolume(ctx, t.Name); docker.IsNotFound(err) {
		return nil
	} else if err != nil {
		return err
	}
	users, err := d.Client.ListContainers(ctx, map[string][]string{"volume": {t.Name}})
	if err != nil {
		return err
	}
	if len(users) > 0 {
		var names []string
		for _, u := range users {
			for _, n := range u.Names {
				names = append(names, strings.TrimPrefix(n, "/"))
			}
		}
		slices.Sort(names)
		return fmt.Errorf("containers still mount the volume %s (%s), so it can't go. Delete their services, or stop mounting it in them, first, in this change or an earlier one",
			t.Name, strings.Join(names, ", "))
	}
	at := d.now()
	d.Kept.mu.Lock()
	defer d.Kept.mu.Unlock()
	return d.Kept.change(func(m map[string]kept) bool {
		if _, ok := m[t.Name]; ok {
			return false
		}
		m[t.Name] = kept{Deleted: at}
		return true
	})
}

// removeWait bounds removing one volume, during which the list is held.
const removeWait = time.Minute

// Prune removes the volumes kept for KeepFor or longer. One a container
// still uses stays, and is tried again on the next prune.
//
// It holds the list while it removes each, and checks again first that it
// is still listed: an undo that brought it back since took it off the
// list, and its data stays.
func (d *Docker) Prune(ctx context.Context, logger *slog.Logger) error {
	k := d.Kept
	k.mu.Lock()
	m, err := k.load()
	names := slices.Sorted(maps.Keys(m))
	k.mu.Unlock()
	if err != nil {
		return err
	}
	for _, name := range names {
		if err := d.prune(ctx, logger, name); err != nil {
			return err
		}
	}
	return nil
}

// prune removes one volume if it is still listed, and was deleted KeepFor
// ago or more.
func (d *Docker) prune(ctx context.Context, logger *slog.Logger, name string) error {
	k := d.Kept
	k.mu.Lock()
	defer k.mu.Unlock()
	m, err := k.load()
	if err != nil {
		return err
	}
	e, ok := m[name]
	deleted := e.Deleted
	if !ok || d.now().Sub(deleted) < KeepFor {
		return nil
	}
	ctx, cancel := context.WithTimeout(ctx, removeWait)
	defer cancel()
	err = d.Client.RemoveVolume(ctx, name)
	switch {
	case err == nil, docker.IsNotFound(err):
		attrs := []any{slog.String("volume", name), slog.Time("deleted", deleted), slog.String("kept_for", human.Duration(KeepFor))}
		if e.Container != "" {
			attrs = append(attrs, slog.String("container", e.Container), slog.String("path", e.Path))
		}
		logger.Info("removed a deleted volume and its data, kept for long enough", attrs...)
	case docker.IsConflict(err):
		logger.Warn("a deleted volume is still in use, so it stays until no container uses it",
			slog.String("volume", name), slog.String("error", err.Error()))
		return nil
	default:
		return fmt.Errorf("can't remove the deleted volume %s: %w", name, err)
	}
	return k.change(func(m map[string]kept) bool { delete(m, name); return true })
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

// ReadyKey is the client: whether its Docker answers.
func (d *Docker) ReadyKey() any { return d.Client }
