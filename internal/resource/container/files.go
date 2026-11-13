// SPDX-License-Identifier: AGPL-3.0-only

package container

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"io/fs"
	"log/slog"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"time"

	resourcev1 "github.com/dockwell/dockwell/gen/dockwell/resource/v1alpha1"
	"github.com/dockwell/dockwell/internal/docker"
)

// What a container's file mounts may hold (ADR 0085): a config file is a few KiB,
// and the largest of 1Panel's, Dokploy's and Coolify's was under 100 KiB on 12 Nov
// 2026. A file holds text, as a plan shows it.
const (
	maxFileBytes      = 256 << 10
	maxContainerFiles = 64
	maxFilesBytes     = 1 << 20
	// defaultFileMode is a file's when its mount says none: readable by any user,
	// as Compose's configs are.
	defaultFileMode = 0o444
)

// fileMode is the mode a file mount's file has: its own, or 0444.
func fileMode(f *resourcev1.MountFile) uint32 {
	if m := f.GetMode(); m != 0 {
		return m
	}
	return defaultFileMode
}

// validateFile checks one file mount, and writes its mode as Docker would read it
// back, so what runs reads back as the spec says.
func validateFile(m *resourcev1.Mount, f *resourcev1.MountFile) error {
	switch {
	case len(f.GetContent()) > maxFileBytes:
		return fmt.Errorf("the file mounted at %s holds %d KiB, and Dockwell writes up to %d KiB. Mount a volume for something larger", m.GetTarget(), len(f.GetContent())>>10, maxFileBytes>>10)
	case f.GetMode()&^0o555 != 0:
		return fmt.Errorf("the file mounted at %s has the mode %04o, and a file Dockwell writes is read-only: give it read and execute bits alone, such as 0444 or 0555", m.GetTarget(), f.GetMode())
	case m.GetExternal() || m.GetMakeOwner() != "" || m.GetRelabel() != resourcev1.Relabel_RELABEL_UNSPECIFIED:
		return fmt.Errorf("the file mounted at %s is one Dockwell writes, so it takes no external, make_owner or relabel. Leave them out", m.GetTarget())
	}
	f.Mode = fileMode(f)
	m.ReadOnly = true
	return nil
}

// validateFiles checks what a container's file mounts come to together.
func validateFiles(c *resourcev1.Container) error {
	n, total := 0, 0
	for _, m := range c.GetMounts() {
		if f := m.GetFile(); f != nil {
			n++
			total += len(f.GetContent())
		}
	}
	switch {
	case n > maxContainerFiles:
		return fmt.Errorf("the container mounts %d files Dockwell writes, and it takes up to %d. Put them in a volume", n, maxContainerFiles)
	case total > maxFilesBytes:
		return fmt.Errorf("the files the container mounts come to %d KiB, and Dockwell writes up to %d KiB for one container. Put them in a volume", total>>10, maxFilesBytes>>10)
	}
	return nil
}

// Files keeps the files containers mount on this server, each once, in a folder of
// the agent's own, by the hash of its mode and content, so two containers with one
// file share it, and a new version is a new file that no running container sees
// change under it.
type Files struct {
	// Dir is the folder, such as /var/lib/dockwell/agent/files, which only root
	// reads and writes: Docker binds each file from it as root.
	Dir string
	// Client lists the containers on the server, whose mounts say which files are
	// in use, for KeepPruned. Nil prunes nothing.
	Client *docker.Client
}

// fileName is the name a file is kept by: its mode and the hash of mode and content,
// so the mode Docker reads it with is in its name.
func fileName(content string, mode uint32) string {
	sum := sha256.Sum256([]byte(strconv.FormatUint(uint64(mode), 8) + "\n" + content))
	return fmt.Sprintf("%04o-%s", mode, hex.EncodeToString(sum[:]))
}

// Path is where a file is kept, written or not.
func (f *Files) Path(content string, mode uint32) string {
	return filepath.Join(f.Dir, fileName(content, mode))
}

// Write keeps a file, if it isn't kept already, and says where.
func (f *Files) Write(content string, mode uint32) (string, error) {
	p := f.Path(content, mode)
	if got, ok := f.Read(p); ok && got.GetContent() == content && got.GetMode() == mode {
		return p, nil
	}
	if err := os.MkdirAll(f.Dir, 0o700); err != nil {
		return "", fmt.Errorf("can't make the folder the agent keeps files in: %w", err)
	}
	tmp, err := os.CreateTemp(f.Dir, ".writing-*")
	if err != nil {
		return "", fmt.Errorf("can't write a file a container mounts: %w", err)
	}
	defer os.Remove(tmp.Name()) // gone once renamed
	if _, err := tmp.WriteString(content); err != nil {
		tmp.Close() //nolint:gosec // it's removed
		return "", fmt.Errorf("can't write a file a container mounts: %w", err)
	}
	if err := tmp.Chmod(fs.FileMode(mode)); err != nil {
		tmp.Close() //nolint:gosec // it's removed
		return "", err
	}
	if err := tmp.Close(); err != nil {
		return "", err
	}
	if err := os.Rename(tmp.Name(), p); err != nil {
		return "", fmt.Errorf("can't write a file a container mounts: %w", err)
	}
	return p, nil
}

// Read reads back a file this keeps, by its path, as a mount says it, and false for
// a path that isn't one: elsewhere, gone, a link, or not what its name says.
func (f *Files) Read(p string) (*resourcev1.MountFile, bool) {
	if f == nil || f.Dir == "" || filepath.Dir(filepath.Clean(p)) != filepath.Clean(f.Dir) {
		return nil, false
	}
	name := filepath.Base(p)
	modeText, _, ok := strings.Cut(name, "-")
	mode, err := strconv.ParseUint(modeText, 8, 32)
	if !ok || err != nil {
		return nil, false
	}
	fi, err := os.Lstat(p)
	if err != nil || !fi.Mode().IsRegular() || fi.Size() > maxFileBytes {
		return nil, false
	}
	b, err := os.ReadFile(p) //nolint:gosec // a file in the agent's own folder, by a name it gave it
	if err != nil || fileName(string(b), uint32(mode)) != name {
		return nil, false
	}
	return &resourcev1.MountFile{Content: string(b), Mode: uint32(mode)}, true
}

// fileGrace is how long a file nothing mounts is kept: a container a change is
// making may not be there yet.
const fileGrace = time.Hour

// Prune removes the files no container on the server mounts, kept at least an hour.
func (f *Files) Prune(ctx context.Context) error {
	if f == nil || f.Client == nil || f.Dir == "" {
		return nil
	}
	entries, err := os.ReadDir(f.Dir)
	if errors.Is(err, fs.ErrNotExist) {
		return nil
	}
	if err != nil {
		return err
	}
	list, err := f.Client.ListContainers(ctx, nil)
	if err != nil {
		return err
	}
	used := map[string]bool{}
	for i := range list {
		c, err := f.Client.InspectContainer(ctx, list[i].ID)
		if err != nil {
			continue
		}
		for _, b := range c.HostConfig.Binds {
			if source, _, _ := strings.Cut(b, ":"); filepath.Dir(source) == filepath.Clean(f.Dir) {
				used[filepath.Base(source)] = true
			}
		}
	}
	for _, e := range entries {
		info, err := e.Info()
		if err != nil || used[e.Name()] || time.Since(info.ModTime()) < fileGrace {
			continue
		}
		_ = os.Remove(filepath.Join(f.Dir, e.Name()))
	}
	return nil
}

// KeepPruned prunes the files nothing mounts every hour, until ctx is done.
func (f *Files) KeepPruned(ctx context.Context, logger *slog.Logger) {
	t := time.NewTicker(fileGrace)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-t.C:
		}
		if err := f.Prune(ctx); err != nil && ctx.Err() == nil {
			logger.WarnContext(ctx, "can't prune the files containers no longer mount; it tries again in an hour", slog.String("error", err.Error()))
		}
	}
}
