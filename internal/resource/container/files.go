// SPDX-License-Identifier: AGPL-3.0-only

package container

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"log/slog"
	"os"
	"path"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"time"
	"unicode/utf8"

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
	case m.GetExternal() || m.GetMakeOwner() != "" || m.GetRelabel() != resourcev1.Relabel_RELABEL_UNSPECIFIED || m.GetMakeMode() != 0 || len(m.GetSeed()) > 0:
		return fmt.Errorf("the file mounted at %s is one Dockwell writes, so it takes no external, make_owner, make_mode, seed or relabel. Leave them out", m.GetTarget())
	case kernelFolder(m.GetTarget()):
		return fmt.Errorf("the file mounted at %s would stand in the kernel's own folders, /proc, /sys and /dev, where a file Dockwell writes never goes. Mount it elsewhere", m.GetTarget())
	}
	f.Mode = fileMode(f)
	m.ReadOnly = true
	return nil
}

// kernelFolder reports a path in /proc, /sys or /dev, which the kernel fills in.
func kernelFolder(target string) bool {
	clean := filepath.Clean("/" + target)
	for _, k := range []string{"/proc", "/sys", "/dev"} {
		if clean == k || strings.HasPrefix(clean, k+"/") {
			return true
		}
	}
	return false
}

// validateFiles checks what a container's file mounts come to together.
func validateFiles(c *resourcev1.Container) error {
	n, total := 0, 0
	for _, m := range c.GetMounts() {
		if f := m.GetFile(); f != nil {
			n++
			total += len(f.GetContent())
		}
		// What a volume starts with counts with the files it mounts.
		for _, s := range m.GetSeed() {
			n++
			total += len(s.GetContent())
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
		// Kept again, as a container mounts it anew, so a prune counts its hour from
		// now.
		now := time.Now()
		if err := os.Chtimes(p, now, now); err != nil {
			return "", fmt.Errorf("can't keep a file a container mounts: %w", err)
		}
		return p, nil
	}
	// Docker makes a folder where a bind's source is missing, which a file can't be
	// renamed over: an empty one goes, so the file takes its name.
	if fi, err := os.Lstat(p); err == nil && fi.IsDir() {
		if err := os.Remove(p); err != nil {
			return "", fmt.Errorf("a folder holds the name of a file a container mounts, %s, and it isn't empty: %w. Remove it", p, err)
		}
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
	mine := func(source string) {
		if filepath.Dir(source) == filepath.Clean(f.Dir) {
			used[filepath.Base(source)] = true
		}
	}
	for i := range list {
		if err := ctx.Err(); err != nil {
			return err
		}
		c, err := f.Client.InspectContainer(ctx, list[i].ID)
		switch {
		case docker.IsNotFound(err):
			continue // gone since it was listed
		case err != nil:
			// What it mounts isn't known, so nothing is known to be unused.
			return fmt.Errorf("can't read what container %s mounts, so no file is removed: %w", list[i].ID, err)
		}
		for _, b := range c.HostConfig.Binds {
			source, _, _ := strings.Cut(b, ":")
			mine(source)
		}
		for _, m := range c.HostConfig.Mounts {
			if m.Type == "bind" {
				mine(m.Source)
			}
		}
		// What a container's volumes started with, which it reads back by.
		var prep map[string]volumePrep
		if json.Unmarshal([]byte(c.Config.Labels[LabelVolumePrep]), &prep) == nil {
			for _, p := range prep {
				for _, s := range p.Seed {
					used[s.File] = true
				}
			}
		}
	}
	for _, e := range entries {
		if err := ctx.Err(); err != nil {
			return err
		}
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

// maxSeedDepth is how deep under a volume a file it starts with may sit.
const maxSeedDepth = 8

// defaultSeedMode is a file's a volume starts with when its seed says none: the app
// may write it, as it may anything in its volume.
const defaultSeedMode = 0o644

// validateSeeds checks what a volume starts with (Mount.seed): each file's path under
// the volume is relative, clean and of plain names, at most 8 folders deep, given
// once, and no file stands where a folder of another does; what it holds is text
// within a file mount's bounds; and its mode has read, write and execute bits alone
// (ADR 0085). They're put in order, so specs that mean the same compare equal.
func validateSeeds(m *resourcev1.Mount) error {
	if len(m.GetSeed()) == 0 {
		return nil
	}
	if m.GetExternal() {
		return fmt.Errorf("the volume %s at %s is Docker's own, which Dockwell didn't make, so it starts with nothing of Dockwell's. Leave seed out", m.GetVolume(), m.GetTarget())
	}
	seen := map[string]bool{}
	for _, s := range m.GetSeed() {
		p := s.GetPath()
		clean := path.Clean(p)
		parts := strings.Split(clean, "/")
		switch {
		case p == "" || clean != p || path.IsAbs(p) || clean == "." || clean == ".." || strings.HasPrefix(clean, "../"):
			return fmt.Errorf("the volume at %s starts with %q, which isn't a path under it. Give one such as conf/app.conf", m.GetTarget(), p)
		case len(parts) > maxSeedDepth:
			return fmt.Errorf("the volume at %s starts with %s, over %d folders deep", m.GetTarget(), p, maxSeedDepth)
		case slices.ContainsFunc(parts, func(n string) bool { return n == "." || n == ".." || strings.ContainsAny(n, "\x00\\") }):
			return fmt.Errorf("the volume at %s starts with %q, which isn't a path of plain names", m.GetTarget(), p)
		case seen[clean]:
			return fmt.Errorf("the volume at %s starts with %s twice. Give it once", m.GetTarget(), p)
		case len(s.GetContent()) > maxFileBytes:
			return fmt.Errorf("the volume at %s starts with %s of %d KiB, and Dockwell writes up to %d KiB", m.GetTarget(), p, len(s.GetContent())>>10, maxFileBytes>>10)
		case !utf8.ValidString(s.GetContent()) || strings.IndexByte(s.GetContent(), 0) >= 0:
			return fmt.Errorf("the volume at %s starts with %s, which isn't text, and Dockwell writes text alone", m.GetTarget(), p)
		case s.GetMode()&^0o777 != 0:
			return fmt.Errorf("the volume at %s starts with %s of mode %04o: give read, write and execute bits alone, such as 0644", m.GetTarget(), p, s.GetMode())
		}
		if s.GetMode() == 0 {
			s.Mode = defaultSeedMode
		}
		seen[clean] = true
	}
	for p := range seen {
		for dir := path.Dir(p); dir != "."; dir = path.Dir(dir) {
			if seen[dir] {
				return fmt.Errorf("the volume at %s starts with %s inside %s, which is a file", m.GetTarget(), p, dir)
			}
		}
	}
	slices.SortFunc(m.Seed, func(a, b *resourcev1.MountSeed) int { return strings.Compare(a.GetPath(), b.GetPath()) })
	return nil
}
