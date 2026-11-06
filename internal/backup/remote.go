// SPDX-License-Identifier: AGPL-3.0-only

package backup

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path"
	"path/filepath"
	"slices"

	"github.com/dockwell/dockwell/internal/backup/s3"
	"github.com/dockwell/dockwell/internal/backup/seal"
	"github.com/dockwell/dockwell/internal/human"
	"github.com/dockwell/dockwell/internal/resource"
)

// Destination is S3-compatible storage a backup goes to. It lives in memory for one
// call, sealed to the agent's stream on the way here (ADR 0022), and never reaches a
// log, an error or the disk.
type Destination struct {
	Endpoint, Region, Bucket, Prefix, AccessKey, SecretKey, Passphrase string
}

// DestinationFrom reads a destination from the values a call carries. Nil values are
// no destination.
func DestinationFrom(values map[string]string) *Destination {
	if len(values) == 0 {
		return nil
	}
	return &Destination{
		Endpoint: values["endpoint"], Region: values["region"], Bucket: values["bucket"], Prefix: values["prefix"],
		AccessKey: values["access_key"], SecretKey: values["secret_key"], Passphrase: values["passphrase"],
	}
}

func (d *Destination) bucket(r *Runner) *s3.Bucket {
	return &s3.Bucket{Endpoint: d.Endpoint, Region: d.Region, Name: d.Bucket, AccessKey: d.AccessKey, SecretKey: d.SecretKey, Client: r.HTTP}
}

// key is what a backup is called there.
func (d *Destination) key(m *Manifest) string {
	return path.Join(d.Prefix, m.Environment, m.Volume, m.ID+"."+m.Format+".sealed")
}

// Copied is what became of a backup's copy on its destination.
type Copied struct {
	Key   string
	Bytes int64
}

// send seals a backup and sends it to its destination, with its manifest beside it,
// so what's there can be read without this install.
func (r *Runner) send(ctx context.Context, m *Manifest, file string, d *Destination) (*Copied, error) {
	f, err := os.Open(file) //nolint:gosec // under the agent's own directory
	if err != nil {
		return nil, err
	}
	defer f.Close()
	fi, err := f.Stat()
	if err != nil {
		return nil, err
	}
	sealer, err := seal.New(d.Passphrase)
	if err != nil {
		return nil, err
	}
	bucket := d.bucket(r)
	up, total := upload(d.key(m), f, fi.Size(), sealer)
	up.Progress = func(sent int64) {
		resource.Progress(ctx, "Sending "+m.Volume+": "+human.Size(sent)+" of "+human.Size(total))
	}
	if err := bucket.Store(ctx, up); err != nil {
		return nil, err
	}
	stored, err := bucket.Size(ctx, up.Key)
	if err != nil {
		return nil, fmt.Errorf("can't read back what was sent: %w", err)
	}
	if stored != total {
		return nil, fmt.Errorf("the storage holds %s of the %s sent", human.Size(stored), human.Size(total))
	}
	said, err := json.MarshalIndent(m, "", "  ")
	if err != nil {
		return nil, err
	}
	if err := bucket.Put(ctx, up.Key+".json", said); err != nil {
		return nil, err
	}
	return &Copied{Key: up.Key, Bytes: stored}, nil
}

// upload is a sealed file in parts, each a whole number of chunks, so a part that
// has to go again can be made again from its place in the file. It says how long the
// sealed file is.
func upload(key string, f io.ReaderAt, size int64, sealer *seal.Sealer) (s3.Upload, int64) {
	total := seal.Size(size)
	chunks := max((size+seal.ChunkSize-1)/seal.ChunkSize, 1)
	perPart := s3.PartSize(total, seal.SealedChunk) / seal.SealedChunk
	count := (chunks + perPart - 1) / perPart
	chunkLength := func(n int64) int64 {
		if n == chunks-1 {
			return size - n*seal.ChunkSize + 16
		}
		return seal.SealedChunk
	}
	return s3.Upload{
		Key: key, Count: int(count),
		Length: func(i int) int64 {
			var length int64
			if i == 0 {
				length = int64(seal.HeaderSize)
			}
			first := int64(i) * perPart
			for n := first; n < min(first+perPart, chunks); n++ {
				length += chunkLength(n)
			}
			return length
		},
		Part: func(i int) s3.Part {
			return func() (io.Reader, error) {
				first := int64(i) * perPart
				p := &sealedPart{f: f, sealer: sealer, next: first, end: min(first+perPart, chunks), chunks: chunks, size: size}
				if i == 0 {
					p.out = sealer.Header()
				}
				return p, nil
			}
		},
	}, total
}

// sealedPart reads a run of a file's chunks, sealed.
type sealedPart struct {
	f         io.ReaderAt
	sealer    *seal.Sealer
	next, end int64
	chunks    int64
	size      int64
	out       []byte
}

func (p *sealedPart) Read(b []byte) (int, error) {
	for len(p.out) == 0 {
		if p.next >= p.end {
			return 0, io.EOF
		}
		at := p.next * seal.ChunkSize
		chunk := make([]byte, min(seal.ChunkSize, p.size-at))
		if _, err := p.f.ReadAt(chunk, at); err != nil && !errors.Is(err, io.EOF) {
			return 0, err
		}
		p.out = p.sealer.Seal(uint64(p.next), p.next == p.chunks-1, chunk) //nolint:gosec // a count of chunks
		p.next++
	}
	n := copy(b, p.out)
	p.out = p.out[n:]
	return n, nil
}

// Fetch brings a backup back from where it was sent, onto this server's disk, and
// reads it back whole before it counts.
func (r *Runner) Fetch(ctx context.Context, ref resource.Ref, id, key string, d *Destination) (*Manifest, error) {
	bucket := d.bucket(r)
	said, err := bucket.Get(ctx, key+".json")
	if err != nil {
		return nil, fmt.Errorf("can't read what was kept beside the backup: %w", err)
	}
	var m Manifest
	err = json.NewDecoder(io.LimitReader(said, 1<<20)).Decode(&m)
	_ = said.Close()
	if err != nil {
		return nil, fmt.Errorf("can't read what was kept beside the backup: %w", err)
	}
	if m.ID != id || m.Volume != ref.Name || !slices.Contains([]string{formatArchive, formatDump}, m.Format) {
		return nil, fmt.Errorf("what's stored at %s isn't the backup %s", key, id)
	}
	m.Environment = ref.Scope
	if err := os.MkdirAll(r.dir(ref), 0o700); err != nil {
		return nil, fmt.Errorf("can't make a place for the backup: %w", err)
	}
	body, err := bucket.Get(ctx, key)
	if err != nil {
		return nil, fmt.Errorf("can't read the backup: %w", err)
	}
	defer body.Close()
	verify := verifyArchive
	if m.Format == formatDump {
		k := kindNamed(m.Database)
		if k == nil {
			return nil, fmt.Errorf("this backup is a dump of %s, which this agent can't read. Update it to the control plane's version", m.Database)
		}
		verify = verifyDump(k)
	}
	want := m
	resource.Progress(ctx, "Fetching "+m.Volume)
	err = r.write(ref, &m, func(w io.Writer) (int64, error) {
		return m.Changed, seal.Open(w, body, d.Passphrase)
	}, verify)
	if err != nil {
		return nil, err
	}
	if m.SHA256 != want.SHA256 {
		_ = os.Remove(filepath.Join(r.dir(ref), m.ID+"."+m.Format))
		_ = os.Remove(filepath.Join(r.dir(ref), m.ID+".json"))
		return nil, errors.New("what came back isn't what was sent, so it isn't kept")
	}
	return &m, nil
}

// deleteRemote removes a backup from where it was sent.
func (r *Runner) deleteRemote(ctx context.Context, key string, d *Destination) error {
	bucket := d.bucket(r)
	return errors.Join(bucket.Delete(ctx, key), bucket.Delete(ctx, key+".json"))
}
