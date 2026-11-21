// SPDX-License-Identifier: AGPL-3.0-only

// Package artifact stages reviewed image transfers in a private agent spool.
package artifact

import (
	"archive/tar"
	"bufio"
	"bytes"
	"compress/gzip"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"io"
	"strconv"
	"strings"
	"time"

	v1 "github.com/dockwell/dockwell/gen/dockwell/v1"
	"github.com/dockwell/dockwell/internal/registry"
)

// The on-disk, decompression, frame and lease limits shared by the byte lane.
const (
	MaxBytes    = 8 << 30
	MaxExpanded = 16 << 30
	MaxChunk    = 64 << 20
	Frame       = 128 << 10
	Lifetime    = 24 * time.Hour
	maxEntries  = 50002
	maxJSON     = 1 << 20
)

// ErrArchive is a file the strict Docker save reader refused.
var ErrArchive = errors.New("the Docker image archive is invalid or exceeds its limits")

type manifest struct {
	Config   string
	RepoTags boundedStrings
	Layers   boundedStrings
}

// boundedStrings stops decoding once an array exceeds the archive limits. The
// outer JSON can still contain unrelated fields, which the decoder skips.
type boundedStrings []string

func (s *boundedStrings) UnmarshalJSON(data []byte) error {
	if bytes.Equal(data, []byte("null")) {
		*s = nil
		return nil
	}
	d := json.NewDecoder(bytes.NewReader(data))
	if start, err := d.Token(); err != nil || start != json.Delim('[') {
		return ErrArchive
	}
	*s = (*s)[:0]
	for d.More() {
		var entry string
		if len(*s) == 128 || d.Decode(&entry) != nil || len(entry) > 512 || entry == "" {
			return ErrArchive
		}
		*s = append(*s, entry)
	}
	if end, err := d.Token(); err != nil || end != json.Delim(']') {
		return ErrArchive
	}
	return nil
}

type extent struct{ start, size int64 }

type archive struct {
	file         io.ReaderAt
	entries      map[string]extent
	manifest     []manifest
	images       []*v1.DockerArchiveImage
	expanded     uint64
	exportConfig string
}

type checkedReader struct {
	ctx context.Context
	r   io.Reader
}

func (r checkedReader) Read(p []byte) (int, error) {
	if err := r.ctx.Err(); err != nil {
		return 0, err
	}
	return r.r.Read(p)
}

func digest(h []byte) string { return "sha256:" + hex.EncodeToString(h) }

func untar(ctx context.Context, file io.ReaderAt, size int64, manifestDigest ...string) (*archive, error) {
	if size < 2*tarBlock || size > MaxExpanded {
		return nil, ErrArchive
	}
	idx, err := indexTar(ctx, file, size, tarIndexDir(file))
	if err != nil {
		return nil, err
	}
	defer idx.file.Close()
	a := &archive{file: file, entries: make(map[string]extent)}
	if len(manifestDigest) != 0 {
		sum := manifestDigest[0]
		if !registry.IsDigest(sum) {
			return nil, ErrArchive
		}
		name := "blobs/sha256/" + strings.TrimPrefix(sum, "sha256:")
		if err := a.keep(idx, name); err != nil {
			return nil, err
		}
		data, err := a.json(name)
		var manifest struct {
			Config struct {
				Digest string `json:"digest"`
			} `json:"config"`
			SchemaVersion int `json:"schemaVersion"`
		}
		got := sha256.Sum256(data)
		if err != nil || digest(got[:]) != sum || json.Unmarshal(data, &manifest) != nil ||
			manifest.SchemaVersion != 2 || !registry.IsDigest(manifest.Config.Digest) {
			return nil, ErrArchive
		}
		a.exportConfig = manifest.Config.Digest
	}
	if err := a.keep(idx, "manifest.json"); err != nil {
		return nil, err
	}
	data, err := a.json("manifest.json")
	if err != nil || !readManifest(data, &a.manifest) {
		return nil, ErrArchive
	}
	for _, m := range a.manifest {
		if err := a.keep(idx, m.Config); err != nil {
			return nil, err
		}
		for _, layer := range m.Layers {
			if err := a.keep(idx, layer); err != nil {
				return nil, err
			}
		}
	}
	var aliases int
	byID, byTag, verifiedLayers := map[string]bool{}, map[string]bool{}, map[string]string{}
	var expanded int64
	var layers layerReader
	var configBuffer []byte
	for _, m := range a.manifest {
		var cfg struct {
			OS           string
			Architecture string
			Variant      string
			RootFS       struct {
				DiffIDs boundedStrings `json:"diff_ids"`
			}
		}
		config, err := a.readJSON(m.Config, configBuffer)
		configBuffer = config
		if err != nil || json.Unmarshal(config, &cfg) != nil || cfg.OS == "" || cfg.Architecture == "" ||
			len(cfg.RootFS.DiffIDs) != len(m.Layers) || len(m.Layers) > 128 {
			return nil, ErrArchive
		}
		sha := sha256.Sum256(config)
		id := digest(sha[:])
		hexID := strings.TrimPrefix(id, "sha256:")
		if byID[id] || (m.Config != hexID+".json" && m.Config != "blobs/sha256/"+hexID) {
			return nil, ErrArchive
		}
		byID[id] = true
		img := &v1.DockerArchiveImage{ImageId: id, Platform: strings.TrimSuffix(cfg.OS+"/"+cfg.Architecture+"/"+cfg.Variant, "/")}
		if len(img.Platform) > 80 || strings.ContainsAny(img.Platform, " \t\r\n") {
			return nil, ErrArchive
		}
		for _, name := range m.RepoTags {
			r, err := registry.Parse(name)
			if err != nil || r.Tag == "" || r.Digest != "" || byTag[r.String()] || aliases >= 128 {
				return nil, ErrArchive
			}
			byTag[r.String()] = true
			aliases++
			img.ProposedTags = append(img.ProposedTags, r.String())
		}
		for i, name := range m.Layers {
			entry, ok := a.entries[name]
			if !ok || !registry.IsDigest(cfg.RootFS.DiffIDs[i]) {
				return nil, ErrArchive
			}
			if known := verifiedLayers[name]; known != "" {
				if known != cfg.RootFS.DiffIDs[i] {
					return nil, ErrArchive
				}
			} else {
				sum, n, err := layers.digest(ctx, a.file, name, entry, MaxExpanded-expanded)
				if err != nil || sum != cfg.RootFS.DiffIDs[i] {
					return nil, ErrArchive
				}
				expanded += n
				verifiedLayers[name] = sum
			}
			img.SizeBytes += uint64(entry.size) //nolint:gosec // only nonnegative tar entries reach this loop
		}
		a.images = append(a.images, img)
	}
	a.expanded = uint64(expanded) // The validator caps the total at MaxExpanded.
	return a, nil
}

func (a *archive) keep(idx *tarIndex, name string) error {
	if _, ok := a.entries[name]; ok {
		return nil
	}
	if !validTarName([]byte(name)) {
		return ErrArchive
	}
	e, err := idx.fileExtent(a.file, name)
	if err != nil {
		return err
	}
	a.entries[name] = e
	return nil
}

func readManifest(data []byte, out *[]manifest) bool {
	d := json.NewDecoder(bytes.NewReader(data))
	if start, err := d.Token(); err != nil || start != json.Delim('[') {
		return false
	}
	var raw json.RawMessage
	for d.More() {
		if len(*out) == 64 || d.Decode(&raw) != nil || len(raw) > 100<<10 {
			return false
		}
		var m manifest
		if json.Unmarshal(raw, &m) != nil || len(m.Layers) > 128 || len(m.RepoTags) > 128 {
			return false
		}
		*out = append(*out, m)
	}
	end, err := d.Token()
	return err == nil && end == json.Delim(']') && len(*out) > 0 && d.Decode(&raw) == io.EOF
}

func (a *archive) json(name string) ([]byte, error) {
	return a.readJSON(name, nil)
}

func (a *archive) readJSON(name string, scratch []byte) ([]byte, error) {
	e, ok := a.entries[name]
	if !ok || e.size > maxJSON {
		return nil, ErrArchive
	}
	if int64(cap(scratch)) < e.size {
		scratch = make([]byte, e.size)
	}
	scratch = scratch[:e.size]
	_, err := a.file.ReadAt(scratch, e.start)
	return scratch, err
}

type layerReader struct {
	reader *bufio.Reader
	gzip   *gzip.Reader
	buffer []byte
}

func (l *layerReader) digest(ctx context.Context, file io.ReaderAt, name string, e extent, budget int64) (string, int64, error) {
	raw := sha256.New()
	input := io.TeeReader(checkedReader{ctx, io.NewSectionReader(file, e.start, e.size)}, raw)
	if l.reader == nil {
		l.reader = bufio.NewReader(input)
		l.buffer = make([]byte, 32<<10)
	} else {
		l.reader.Reset(input)
	}
	var source io.Reader = l.reader
	if magic, _ := l.reader.Peek(2); len(magic) == 2 && magic[0] == 0x1f && magic[1] == 0x8b {
		var err error
		if l.gzip == nil {
			l.gzip, err = gzip.NewReader(l.reader)
		} else {
			err = l.gzip.Reset(l.reader)
		}
		if err != nil {
			return "", 0, err
		}
		defer l.gzip.Close()
		source = l.gzip
	}
	h := sha256.New()
	n, err := io.CopyBuffer(h, io.LimitReader(source, budget+1), l.buffer)
	if err != nil || n > budget {
		return "", 0, ErrArchive
	}
	if expected, ok := strings.CutPrefix(name, "blobs/sha256/"); ok && digest(raw.Sum(nil)) != "sha256:"+expected {
		return "", 0, ErrArchive
	}
	return digest(h.Sum(nil)), n, nil
}

// write produces Docker save format with only the selected config, its verified
// layers and the aliases explicitly given by the caller.
func (a *archive) write(ctx context.Context, dst io.Writer, id string, tags []string) error {
	for i, image := range a.images {
		if image.ImageId != id {
			continue
		}
		m := a.manifest[i]
		out := manifest{Config: strings.TrimPrefix(id, "sha256:") + ".json", RepoTags: tags}
		tw := tar.NewWriter(dst)
		buf := make([]byte, Frame)
		copyEntry := func(name, old string) error {
			e := a.entries[old]
			if err := tw.WriteHeader(&tar.Header{Name: name, Mode: 0o600, Size: e.size}); err != nil {
				return err
			}
			_, err := io.CopyBuffer(tw, checkedReader{ctx, io.NewSectionReader(a.file, e.start, e.size)}, buf)
			return err
		}
		if err := copyEntry(out.Config, m.Config); err != nil {
			return err
		}
		for j, layer := range m.Layers {
			name := "layers/" + strconv.Itoa(j) + ".tar"
			if err := copyEntry(name, layer); err != nil {
				return err
			}
			out.Layers = append(out.Layers, name)
		}
		data, err := json.Marshal([]manifest{out})
		if err != nil {
			return err
		}
		if err := tw.WriteHeader(&tar.Header{Name: "manifest.json", Mode: 0o600, Size: int64(len(data))}); err != nil {
			return err
		}
		if _, err := tw.Write(data); err != nil {
			return err
		}
		return tw.Close()
	}
	return ErrArchive
}

// Identity returns the signed-by-content config identity and its ordered layers.
func (a *Archive) Identity(id string) (platform string, layers []string, ok bool) {
	for i, image := range a.a.images {
		if image.ImageId != id {
			continue
		}
		var cfg struct {
			RootFS struct {
				DiffIDs []string `json:"diff_ids"`
			}
		}
		data, err := a.a.json(a.a.manifest[i].Config)
		if err != nil || json.Unmarshal(data, &cfg) != nil {
			return "", nil, false
		}
		return image.Platform, cfg.RootFS.DiffIDs, true
	}
	return "", nil, false
}
