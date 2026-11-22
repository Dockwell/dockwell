// SPDX-License-Identifier: AGPL-3.0-only

package artifact

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"io"
	"os"

	"google.golang.org/protobuf/proto"

	v1 "github.com/dockwell/dockwell/gen/dockwell/v1"
	"github.com/dockwell/dockwell/internal/registry"
)

// Blob is the hashed extent of one config or layer in a private image archive.
type Blob struct {
	Digest string
	Size   int64
	Reader *io.SectionReader
}

// Distribution is the exact single-platform manifest and its local blobs. The
// payloads are valid only for the lifetime of the staging callback.
type Distribution struct {
	Manifest                 []byte
	Digest, Config, Platform string
	Blobs                    []Blob
}

func (a *archive) distribution(ctx context.Context) (*Distribution, error) {
	if len(a.images) != 1 {
		return nil, ErrArchive
	}
	img, m := a.images[0], a.manifest[0]
	out := &Distribution{Config: img.GetImageId(), Platform: img.GetPlatform()}
	type descriptor struct {
		MediaType string `json:"mediaType"`
		Digest    string `json:"digest"`
		Size      int64  `json:"size"`
	}
	buf := make([]byte, 32<<10)
	seen := map[string]bool{}
	blob := func(name, media string) (descriptor, error) {
		e := a.entries[name]
		r := io.NewSectionReader(a.file, e.start, e.size)
		h := sha256.New()
		n, err := io.CopyBuffer(h, checkedReader{ctx, r}, buf)
		if err != nil || n != e.size {
			return descriptor{}, ErrArchive
		}
		d := digest(h.Sum(nil))
		if !seen[d] {
			seen[d] = true
			out.Blobs = append(out.Blobs, Blob{Digest: d, Size: e.size, Reader: io.NewSectionReader(a.file, e.start, e.size)})
		}
		return descriptor{MediaType: media, Digest: d, Size: e.size}, nil
	}
	config, err := blob(m.Config, "application/vnd.oci.image.config.v1+json")
	if err != nil || config.Digest != out.Config {
		return nil, ErrArchive
	}
	layers := make([]descriptor, 0, len(m.Layers))
	for _, layer := range m.Layers {
		e := a.entries[layer]
		var magic [2]byte
		_, _ = a.file.ReadAt(magic[:], e.start)
		media := "application/vnd.oci.image.layer.v1.tar"
		if magic == [2]byte{0x1f, 0x8b} {
			media += "+gzip"
		}
		d, err := blob(layer, media)
		if err != nil {
			return nil, err
		}
		layers = append(layers, d)
	}
	out.Manifest, err = json.Marshal(struct {
		SchemaVersion int          `json:"schemaVersion"`
		MediaType     string       `json:"mediaType"`
		Config        descriptor   `json:"config"`
		Layers        []descriptor `json:"layers"`
	}{2, registry.OCIManifest, config, layers})
	if err != nil {
		return nil, ErrArchive
	}
	sum := sha256.Sum256(out.Manifest)
	out.Digest = digest(sum[:])
	return out, nil
}

// PreparePush verifies an Engine save stream, then seals its bytes to the
// artifact lane. This kind can never be served over the download endpoint.
func (s *Spool) PreparePush(ctx context.Context, owner string, src io.Reader, verify func(*Archive, string) bool, manifests ...string) (*v1.DockerArtifactInfo, *v1.DockerRegistryDescriptor, error) {
	info, err := s.Begin(owner, PushSource, 0, "", false, "")
	if err != nil {
		return nil, nil, err
	}
	l, err := s.locked(info.GetArtifactId(), owner, false)
	if err != nil {
		return nil, nil, err
	}
	defer l.mu.Unlock()
	ready := false
	defer func() {
		if !ready {
			_ = s.remove(l)
		}
	}()
	f, err := os.OpenFile(l.source(), os.O_RDWR|os.O_CREATE|os.O_EXCL, 0o600)
	if err != nil {
		return nil, nil, err
	}
	defer f.Close()
	h := sha256.New()
	w := &quotaWriter{spool: s, lease: l, dst: io.MultiWriter(f, h), limit: MaxBytes}
	n, err := io.CopyBuffer(w, checkedReader{ctx, src}, make([]byte, Frame))
	w.close()
	if err != nil {
		return nil, nil, err
	}
	a, err := untar(ctx, f, n, manifests...)
	if err != nil || len(a.images) != 1 || a.exportConfig != "" && a.exportConfig != a.images[0].GetImageId() || !verify(&Archive{a: a}, a.images[0].GetImageId()) {
		return nil, nil, ErrArchive
	}
	pub, err := a.distribution(ctx)
	if err != nil {
		return nil, nil, err
	}
	if err := f.Sync(); err != nil {
		return nil, nil, err
	}
	l.Info.State, l.Info.SizeBytes, l.Info.Sha256 = Ready, w.written, hex.EncodeToString(h.Sum(nil))
	if err := l.save(); err != nil {
		return nil, nil, err
	}
	s.publish(l)
	ready = true
	return proto.CloneOf(l.Info), &v1.DockerRegistryDescriptor{Platform: pub.Platform, ConfigDigest: pub.Config, ManifestDigest: pub.Digest}, nil
}

// TakePush admits a registry send without holding the archive or Files lanes.
func (s *Spool) TakePush(ctx context.Context, queued func()) (func(), error) {
	release := func() { <-s.pushes }
	select {
	case s.pushes <- struct{}{}:
		return release, nil
	default:
		queued()
	}
	select {
	case s.pushes <- struct{}{}:
		return release, nil
	case <-ctx.Done():
		return nil, ctx.Err()
	}
}

// WithPush checks the plan's lease, verified content and manifest before an apply.
func (s *Spool) WithPush(ctx context.Context, lease, change, sha string, each func(*Distribution) error) error {
	validation, err := s.TakeValidation(ctx)
	if err != nil {
		return err
	}
	validating := true
	defer func() {
		if validating {
			validation()
		}
	}()
	l, err := s.locked(lease, "", false)
	if err != nil {
		return err
	}
	defer l.mu.Unlock()
	if l.Pin.GetChangeId() != change || l.Info.GetKind() != PushSource || l.Info.GetState() != Ready || l.Info.GetSha256() != sha {
		return ErrUnavailable
	}
	f, err := os.Open(l.source())
	if err != nil {
		return err
	}
	defer f.Close()
	st, err := f.Stat()
	if err != nil || st.Size() != int64(l.Info.GetSizeBytes()) { //nolint:gosec // 8 GiB lease bound
		return ErrUnavailable
	}
	a, err := untar(ctx, f, st.Size())
	if err != nil {
		return err
	}
	pub, err := a.distribution(ctx)
	if err != nil {
		return err
	}
	validation()
	validating = false
	return each(pub)
}
