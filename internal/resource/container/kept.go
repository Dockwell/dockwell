// SPDX-License-Identifier: AGPL-3.0-only

package container

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"log/slog"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"sync"
	"time"

	"github.com/dockwell/dockwell/internal/docker"
)

// KeptRepo is where the images a moved tag leaves are kept, under a name of
// Dockwell's own that no registry has (ADR 0080).
const KeptRepo = "dockwell.local/kept"

// KeepImagesFor is how long a kept image stays: the undo window, as the
// rollback or undo that would run it again goes after that.
const KeepImagesFor = 7 * 24 * time.Hour

// KeptImages keeps the image a pull of a moving tag leaves, which Docker leaves
// with no tag and docker image prune removes, so the rollback or the undo that
// puts it back finds it on the server, whether or not its registry still has it.
// After KeepImagesFor it lets go of its tag, and the image goes with it unless a
// container still uses it.
type KeptImages struct {
	// Path is the file it keeps its list in.
	Path   string
	Client *docker.Client
	// Now is its clock; nil means time.Now.
	Now func() time.Time

	mu sync.Mutex
}

func (k *KeptImages) now() time.Time {
	if k.Now != nil {
		return k.Now()
	}
	return time.Now()
}

var unsafeName = regexp.MustCompile(`[^a-z0-9._-]+`)

// KeptName is the tag an image ref ran as is kept under: its repository's last
// part and the image's ID, such as dockwell.local/kept/web:3f2a9c1b7d00.
func KeptName(ref, imageID string) string {
	repo := docker.Repository(ref)
	if i := strings.LastIndex(repo, "/"); i >= 0 {
		repo = repo[i+1:]
	}
	repo = strings.Trim(unsafeName.ReplaceAllString(strings.ToLower(repo), "-"), "-._")
	if repo == "" {
		repo = "image"
	}
	id := strings.TrimPrefix(imageID, "sha256:")
	if len(id) > 12 {
		id = id[:12]
	}
	return KeptRepo + "/" + repo + ":" + id
}

// Keep tags the image a moved tag left, and notes when.
func (k *KeptImages) Keep(ctx context.Context, ref, imageID string) error {
	if k == nil || imageID == "" {
		return nil
	}
	name := KeptName(ref, imageID)
	if err := k.Client.TagImage(ctx, imageID, name); err != nil {
		return fmt.Errorf("can't keep %s's old image as %s: %w", ref, name, err)
	}
	k.mu.Lock()
	defer k.mu.Unlock()
	list, err := k.read()
	if err != nil {
		return err
	}
	list[name] = k.now()
	return k.write(list)
}

// Prune lets go of what was kept KeepImagesFor ago. An image a container still
// uses keeps its tag, and is tried again on the next prune.
func (k *KeptImages) Prune(ctx context.Context, logger *slog.Logger) error {
	k.mu.Lock()
	defer k.mu.Unlock()
	list, err := k.read()
	if err != nil {
		return err
	}
	changed := false
	for name, at := range list {
		if k.now().Sub(at) < KeepImagesFor {
			continue
		}
		if err := k.Client.UntagImage(ctx, name); err != nil {
			logger.InfoContext(ctx, "an image kept for a rollback is still in use, so it stays a while", slog.String("image", name), slog.String("error", err.Error()))
			continue
		}
		delete(list, name)
		changed = true
	}
	if !changed {
		return nil
	}
	return k.write(list)
}

// PruneImagesEvery is how often KeepPruned prunes.
const PruneImagesEvery = time.Hour

// KeepPruned prunes now and every PruneImagesEvery until ctx is done.
func (k *KeptImages) KeepPruned(ctx context.Context, logger *slog.Logger) {
	t := time.NewTicker(PruneImagesEvery)
	defer t.Stop()
	for {
		if err := k.Prune(ctx, logger); err != nil && ctx.Err() == nil {
			logger.WarnContext(ctx, "can't let go of the images kept for rollbacks; they stay until the next try", slog.String("error", err.Error()))
		}
		select {
		case <-ctx.Done():
			return
		case <-t.C:
		}
	}
}

func (k *KeptImages) read() (map[string]time.Time, error) {
	out := map[string]time.Time{}
	b, err := os.ReadFile(k.Path)
	switch {
	case errors.Is(err, fs.ErrNotExist):
		return out, nil
	case err != nil:
		return nil, fmt.Errorf("can't read the images kept for rollbacks in %s: %w", k.Path, err)
	}
	if err := json.Unmarshal(b, &out); err != nil {
		return nil, fmt.Errorf("can't read the images kept for rollbacks in %s: %w. Remove the file to forget them; they stay on the server until docker image prune -a", k.Path, err)
	}
	return out, nil
}

func (k *KeptImages) write(list map[string]time.Time) error {
	b, err := json.Marshal(list)
	if err != nil {
		return err
	}
	if err := os.MkdirAll(filepath.Dir(k.Path), 0o700); err != nil {
		return err
	}
	tmp := k.Path + ".new"
	if err := os.WriteFile(tmp, b, 0o600); err != nil {
		return err
	}
	return os.Rename(tmp, k.Path)
}
