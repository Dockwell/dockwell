// SPDX-License-Identifier: AGPL-3.0-only

package artifact

import (
	"archive/tar"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"io"
	"os"
	"path"
	"path/filepath"
	"strings"

	"google.golang.org/protobuf/proto"

	v1 "github.com/dockwell/dockwell/gen/dockwell/v1"
)

// ContextSize limits the upload. Its expanded tree may use up to ContextExpanded.
const (
	BuildContext    = v1.DockerArtifactKind_DOCKER_ARTIFACT_KIND_BUILD_CONTEXT
	Dockerfile      = v1.DockerArtifactKind_DOCKER_ARTIFACT_KIND_DOCKERFILE
	ContextSize     = 1 << 30
	ContextExpanded = 4 << 30
	DockerfileSize  = 1 << 20
)

// UploadKind reports the kinds that accept bytes through the shared lane.
func UploadKind(kind v1.DockerArtifactKind) bool {
	return kind == Import || kind == BuildContext || kind == Dockerfile
}

func uploadLimit(kind v1.DockerArtifactKind) uint64 {
	switch kind {
	case BuildContext:
		return ContextSize
	case Dockerfile:
		return DockerfileSize
	default:
		return MaxBytes
	}
}

func expansionLimit(kind v1.DockerArtifactKind) uint64 {
	if kind == BuildContext {
		return ContextExpanded
	}
	return MaxExpanded
}

// ContextPath accepts only portable relative names. The root is handled
// separately when scanning a tar produced with "tar ... .".
func ContextPath(name string) bool {
	return len(name) <= 240 && !strings.ContainsAny(name, "\\:\x00\r\n") &&
		filepath.IsLocal(name) && name != "." && path.Clean(name) == name
}

func walkBuildTar(ctx context.Context, src io.Reader, each func(*tar.Header, io.Reader) error) error {
	r := checkedReader{ctx, src}
	tr := tar.NewReader(r)
	seen := map[[32]byte]struct{}{}
	root := false
	for count := 0; ; count++ {
		h, err := tr.Next()
		if errors.Is(err, io.EOF) {
			var end [maxPadding + 1]byte
			n, err := io.ReadFull(r, end[:])
			if n == len(end) || err != io.EOF && err != io.ErrUnexpectedEOF || !allZero(end[:n]) {
				return ErrArchive
			}
			return nil
		}
		if err != nil || count >= 50000 || h.Typeflag != tar.TypeDir && h.Typeflag != tar.TypeReg {
			return ErrArchive
		}
		for key := range h.PAXRecords {
			if strings.HasPrefix(key, "GNU.sparse") || strings.HasPrefix(key, "SCHILY.") {
				return ErrArchive
			}
		}
		h.Name = strings.TrimSuffix(strings.TrimPrefix(h.Name, "./"), "/")
		if h.Typeflag == tar.TypeDir && (h.Name == "" || h.Name == ".") && !root {
			root = true
			continue
		}
		if !ContextPath(h.Name) || h.Size < 0 || h.Typeflag == tar.TypeDir && h.Size != 0 {
			return ErrArchive
		}
		key := sha256.Sum256([]byte(h.Name))
		if _, exists := seen[key]; exists {
			return ErrArchive
		}
		seen[key] = struct{}{}
		if err := each(h, tr); err != nil {
			return err
		}
	}
}

// SnapshotBuild writes a private server-folder copy using the same disk guard
// as uploaded contexts. The caller supplies a descriptor-checked walker.
func (s *Spool) SnapshotBuild(ctx context.Context, owner string, write func(io.Writer) error) (*v1.DockerArtifactInfo, error) {
	info, err := s.Begin(owner, Export, 0, "", false, "")
	if err != nil {
		return nil, err
	}
	id := info.GetArtifactId()
	l, err := s.locked(id, owner, false)
	if err != nil {
		return nil, err
	}
	defer l.mu.Unlock()
	f, err := os.OpenFile(l.source(), os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0o600)
	if err != nil {
		_ = s.remove(l)
		return nil, err
	}
	hash := sha256.New()
	w := &quotaWriter{spool: s, lease: l, dst: io.MultiWriter(f, hash), limit: ContextExpanded}
	err = write(w)
	w.close()
	err = errors.Join(err, f.Sync(), f.Close(), ctx.Err())
	if err != nil {
		_ = s.remove(l)
		return nil, err
	}
	l.Info.Kind, l.Info.State, l.Info.MaxBytes = BuildContext, Ready, ContextSize
	l.Info.SizeBytes, l.Info.ReceivedBytes = w.written, w.written
	l.Info.Sha256 = hex.EncodeToString(hash.Sum(nil))
	if err := l.save(); err != nil {
		_ = s.remove(l)
		return nil, err
	}
	s.publish(l)
	return proto.CloneOf(l.Info), nil
}

// BuildText reads one bounded context member or a sealed Dockerfile override.
func (s *Spool) BuildText(ctx context.Context, id, owner, name string) ([]byte, error) {
	l, err := s.locked(id, owner, true)
	if err != nil {
		return nil, err
	}
	defer l.mu.RUnlock()
	if l.Info.GetState() != Ready || l.Info.GetKind() != BuildContext && l.Info.GetKind() != Dockerfile {
		return nil, ErrUnavailable
	}
	f, err := os.Open(l.source())
	if err != nil {
		return nil, err
	}
	defer f.Close()
	read := func(src io.Reader) ([]byte, error) {
		b, err := io.ReadAll(io.LimitReader(src, DockerfileSize+1))
		if len(b) > DockerfileSize {
			return nil, ErrArchive
		}
		return b, err
	}
	if l.Info.GetKind() == Dockerfile {
		return read(f)
	}
	var text []byte
	err = walkBuildTar(ctx, f, func(h *tar.Header, r io.Reader) error {
		if h.Name == name && h.Typeflag == tar.TypeReg {
			var err error
			text, err = read(r)
			return err
		}
		return nil
	})
	if err != nil {
		return nil, err
	}
	if text == nil {
		return nil, os.ErrNotExist
	}
	return text, nil
}

// WithBuildContext extracts only regular files and directories into a private
// tree. The sparse-free tar and quota writer keep extraction bounded.
func (s *Spool) WithBuildContext(ctx context.Context, id, digest string, each func(string) error) error {
	l, err := s.locked(id, "", false)
	if err != nil {
		return err
	}
	defer l.mu.Unlock()
	if l.Info.GetSha256() != digest || l.Info.GetState() != Ready || l.Info.GetKind() != BuildContext {
		return ErrUnavailable
	}
	f, err := os.Open(l.source())
	if err != nil {
		return err
	}
	defer f.Close()
	dir := filepath.Join(l.dir, "build-tree")
	if err := os.RemoveAll(dir); err != nil {
		return err
	}
	if err := os.Mkdir(dir, 0o700); err != nil {
		return err
	}
	defer os.RemoveAll(dir)
	root, err := os.OpenRoot(dir)
	if err != nil {
		return err
	}
	defer root.Close()
	buf := make([]byte, Frame)
	qw := &quotaWriter{spool: s, lease: l, limit: ContextExpanded}
	defer qw.close()
	if err := walkBuildTar(ctx, f, func(h *tar.Header, r io.Reader) error {
		if h.Typeflag == tar.TypeDir {
			return root.MkdirAll(h.Name, 0o750)
		}
		if err := root.MkdirAll(path.Dir(h.Name), 0o750); err != nil {
			return err
		}
		out, err := root.OpenFile(h.Name, os.O_WRONLY|os.O_EXCL|os.O_CREATE, 0o600)
		if err != nil {
			return err
		}
		qw.dst = out
		_, err = io.CopyBuffer(qw, checkedReader{ctx, r}, buf)
		if err == nil {
			err = out.Chmod(os.FileMode(h.Mode & 0o777))
		}
		return errors.Join(err, out.Close())
	}); err != nil {
		return err
	}
	return each(dir)
}
