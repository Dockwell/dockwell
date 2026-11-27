// SPDX-License-Identifier: AGPL-3.0-only

package backup

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"path"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"time"

	"github.com/dockwell/dockwell/internal/backup/s3"
	"github.com/dockwell/dockwell/internal/backup/seal"
	"github.com/dockwell/dockwell/internal/backup/sftpstore"
	"github.com/dockwell/dockwell/internal/human"
	ids "github.com/dockwell/dockwell/internal/id"
	"github.com/dockwell/dockwell/internal/resource"
	"github.com/dockwell/dockwell/internal/resource/volume"
)

// Destination is off-site storage a backup goes to. It lives in memory for one
// call, sealed to the agent's stream on the way here (ADR 0022), and never reaches a
// log, an error or the disk.
type Destination struct {
	Endpoint, Region, Bucket, Prefix, AccessKey, SecretKey, Passphrase string
	VirtualHost                                                        bool
	SFTP                                                               *sftpstore.Config
	sweepFolder, sweepFailedIDs                                        string
	sweepBefore                                                        time.Time
}

// DestinationFrom reads a destination from the values a call carries. Nil values are
// no destination.
func DestinationFrom(values map[string]string) *Destination {
	if len(values) == 0 {
		return nil
	}
	if values["transport"] == "sftp" {
		port, _ := strconv.ParseUint(values["port"], 10, 32)
		before, _ := strconv.ParseInt(values["sweep_before"], 10, 64)
		return &Destination{
			Passphrase:  values["passphrase"],
			sweepFolder: values["sweep_failed_folder"], sweepFailedIDs: values["sweep_failed_ids"], sweepBefore: time.Unix(before, 0),
			SFTP: &sftpstore.Config{
				Host: values["host"], Port: uint32(port), User: values["user"], Path: values["path"],
				Fingerprint: values["host_key_fingerprint"], HostKeyAlgorithm: values["host_key_algorithm"], PrivateKey: values["private_key"],
				Password: values["password"], KeyPassphrase: values["key_passphrase"],
			},
		}
	}
	return &Destination{
		Endpoint: values["endpoint"], Region: values["region"], Bucket: values["bucket"], Prefix: values["prefix"],
		AccessKey: values["access_key"], SecretKey: values["secret_key"], Passphrase: values["passphrase"],
		VirtualHost: values["virtual_host"] == "true",
	}
}

var sameOriginClient = &http.Client{
	CheckRedirect: checkBackupRedirect,
}

func checkBackupRedirect(req *http.Request, via []*http.Request) error {
	if len(via) >= 10 {
		return errors.New("stopped after 10 redirects")
	}
	if len(via) == 0 {
		return nil
	}
	orig := via[0].URL
	if !sameOrigin(orig, req.URL) {
		return fmt.Errorf("storage redirected from %s://%s to %s://%s, which isn't allowed", orig.Scheme, orig.Host, req.URL.Scheme, req.URL.Host)
	}
	return nil
}

func sameOrigin(a, b *url.URL) bool {
	if a == nil || b == nil {
		return false
	}
	if !strings.EqualFold(a.Scheme, b.Scheme) {
		return false
	}
	if !strings.EqualFold(a.Hostname(), b.Hostname()) {
		return false
	}
	return effectivePort(a) == effectivePort(b)
}

func effectivePort(u *url.URL) string {
	if p := u.Port(); p != "" {
		return p
	}
	switch strings.ToLower(u.Scheme) {
	case "https":
		return "443"
	case "http":
		return "80"
	default:
		return ""
	}
}

func (d *Destination) bucket(r *Runner) *s3.Bucket {
	client := r.HTTP
	if client == nil {
		client = sameOriginClient
	}
	return &s3.Bucket{Endpoint: d.Endpoint, Region: d.Region, Name: d.Bucket, AccessKey: d.AccessKey, SecretKey: d.SecretKey, VirtualHost: d.VirtualHost, Client: client}
}

// key is what a backup is called there.
func (d *Destination) key(m *Manifest) string {
	return path.Join(d.Prefix, m.Environment, m.Volume, m.ID+"."+m.Format+".sealed")
}

// binding is what a sealed backup is bound to, so a file left under another's name
// doesn't open as that one.
func binding(ref resource.Ref, id string) string {
	return ref.Scope + "/" + ref.Name + "/" + id
}

// sealedAs is the volume a copy at key was sealed as: its environment and name when
// it was sent, which its key names (Destination.key) and moving the app since
// doesn't change, so it opens as that while it lands as ref's (the review of #572,
// its N1). A key laid out otherwise is ref's.
func sealedAs(ref resource.Ref, key string) resource.Ref {
	dir := path.Dir(key)
	env, name := path.Base(path.Dir(dir)), path.Base(dir)
	if !strings.HasPrefix(env, ids.Environment+"_") || name != ref.Name {
		return ref
	}
	return resource.Ref{Kind: ref.Kind, Scope: env, Name: name}
}

// Copied is what became of a backup's copy on its destination.
type Copied struct {
	Key   string
	Bytes int64
}

// Send seals a backup that's on this disk and sends it to its destination, with its
// manifest beside it, sealed too, so what's there can be read without this install.
// It's a step of its own, once the backup is whole and on record: an upload can take
// as long as the network does, and holds nothing up.
func (r *Runner) Send(ctx context.Context, ref resource.Ref, id string, d *Destination) (*Copied, error) {
	m, err := readManifest(filepath.Join(r.dir(ref), id+".json"))
	if err != nil {
		return nil, fmt.Errorf("can't read the backup to send it: %w", err)
	}
	f, err := os.Open(filepath.Join(r.dir(ref), m.ID+"."+m.Format))
	if err != nil {
		return nil, fmt.Errorf("can't read the backup to send it: %w", err)
	}
	defer f.Close()
	fi, err := f.Stat()
	if err != nil {
		return nil, err
	}
	bound := binding(ref, m.ID)
	sealer, err := seal.New(d.Passphrase, bound, r.Work)
	if err != nil {
		return nil, err
	}
	total := sealer.Size(fi.Size())
	progress := func(sent int64) {
		resource.Progress(ctx, "Sending "+m.Volume+": "+human.Size(sent)+" of "+human.Size(total))
	}
	if d.SFTP != nil {
		size := fi.Size()
		chunks := max((size+sealer.ChunkSize()-1)/sealer.ChunkSize(), 1)
		body := &sealedPart{f: f, sealer: sealer, end: chunks, chunks: chunks, size: size, out: sealer.Header(), progress: progress}
		return r.sendSFTP(ctx, d, m, d.key(m), body, total, bound)
	}
	up, _ := upload(d.key(m), f, fi.Size(), sealer)
	up.Progress = progress
	bucket := d.bucket(r)
	if err := bucket.Store(ctx, up); err != nil {
		return nil, refused(err, d)
	}
	stored, err := bucket.Size(ctx, up.Key)
	if err != nil {
		return nil, fmt.Errorf("can't read back what was sent: %w", refused(err, d))
	}
	if stored != total {
		return nil, fmt.Errorf("the storage holds %s of the %s sent. Take another backup, and check the storage if it happens again", human.Size(stored), human.Size(total))
	}
	sealed, err := r.sealManifest(m, d, bound)
	if err != nil {
		return nil, err
	}
	if err := bucket.Put(ctx, up.Key+".json", sealed); err != nil {
		return nil, refused(err, d)
	}
	return &Copied{Key: up.Key, Bytes: stored}, nil
}

func (r *Runner) sealManifest(m *Manifest, d *Destination, bound string) ([]byte, error) {
	said, err := json.MarshalIndent(m, "", "  ")
	if err != nil {
		return nil, err
	}
	return seal.Small(said, d.Passphrase, bound+manifestBinding, r.Work)
}

type remoteReader interface {
	Get(context.Context, string) (io.ReadCloser, error)
	Delete(context.Context, string) error
}

func (r *Runner) remote(ctx context.Context, d *Destination, key string) (remoteReader, func(), error) {
	if d.SFTP == nil {
		return d.bucket(r), func() {}, nil
	}
	client, _, err := sftpstore.Open(ctx, *d.SFTP, nil)
	if err != nil {
		return nil, nil, err
	}
	d.sweep(ctx, client, key)
	return client, client.Close, nil
}

// manifestBinding tells what's beside a sealed backup from the backup.
const manifestBinding = "/manifest"

// redactS3 replaces the destination's access key ID (and secret key, if echoed)
// in an s3.Error's message and code while keeping its HTTP status, code, region,
// and any outer agent operation context intact (#831).
func redactS3(err error, d *Destination) error {
	if err == nil || d == nil {
		return err
	}
	var said *s3.Error
	if !errors.As(err, &said) {
		return err
	}
	scrub := func(s string) string {
		if d.AccessKey != "" {
			s = strings.ReplaceAll(s, d.AccessKey, "[redacted]")
		}
		if d.SecretKey != "" {
			s = strings.ReplaceAll(s, d.SecretKey, "[redacted]")
		}
		return s
	}
	clean := &s3.Error{
		Status:  said.Status,
		Code:    scrub(said.Code),
		Message: scrub(said.Message),
		Region:  said.Region,
	}
	if err.Error() == said.Error() {
		return clean
	}
	if before, after, ok := strings.Cut(err.Error(), said.Error()); ok {
		return fmt.Errorf("%s%w%s", scrub(before), clean, scrub(after))
	}
	return clean
}

// refused says what to do about what storage said, after redacting credentials
// from provider error text.
func refused(err error, d *Destination) error {
	var said *s3.Error
	if errors.As(err, &said) {
		return fmt.Errorf("the storage refused it: %w. Check the destination's keys and bucket", redactS3(err, d))
	}
	return err
}

// upload is a sealed file in parts, each a whole number of chunks, so a part that
// has to go again can be made again from its place in the file. It says how long the
// sealed file is.
func upload(key string, f io.ReaderAt, size int64, sealer *seal.Sealer) (s3.Upload, int64) {
	total := sealer.Size(size)
	chunkSize := sealer.ChunkSize()
	sealedChunk := chunkSize + seal.TagSize
	chunks := max((size+chunkSize-1)/chunkSize, 1)
	perPart := s3.PartSize(total, sealedChunk) / sealedChunk
	count := (chunks + perPart - 1) / perPart
	chunkLength := func(n int64) int64 {
		if n == chunks-1 {
			return size - n*chunkSize + seal.TagSize
		}
		return sealedChunk
	}
	return s3.Upload{
		Key: key, Count: int(count),
		Length: func(i int) int64 {
			var length int64
			if i == 0 {
				length = int64(len(sealer.Header()))
			}
			first := int64(i) * perPart
			end := min(first+perPart, chunks)
			length += (end - first) * sealedChunk
			if end == chunks {
				length += chunkLength(end-1) - sealedChunk
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
	buf       []byte
	progress  func(int64)
	read      int64
}

func (p *sealedPart) Read(b []byte) (int, error) {
	for len(p.out) == 0 {
		if p.next >= p.end {
			return 0, io.EOF
		}
		chunkSize := p.sealer.ChunkSize()
		at := p.next * chunkSize
		if p.buf == nil {
			p.buf = make([]byte, min(chunkSize, p.size-at)+seal.TagSize)
		}
		chunk := p.buf[:min(chunkSize, p.size-at)]
		if len(chunk) > 0 {
			if _, err := p.f.ReadAt(chunk, at); err != nil {
				return 0, err
			}
		}
		p.out = p.sealer.SealInPlace(uint64(p.next), p.next == p.chunks-1, chunk) //nolint:gosec // a count of chunks
		p.next++
	}
	n := copy(b, p.out)
	p.out = p.out[n:]
	if p.progress != nil {
		previous := p.read
		p.read += int64(n)
		if p.read>>20 != previous>>20 || (p.next == p.end && len(p.out) == 0) {
			p.progress(p.read)
		}
	}
	return n, nil
}

// Fetch brings a backup back from where it was sent, onto this server's disk, once
// what came back is what the control plane has on record as want: nothing that came
// from the storage decides that, and nothing here takes the place of a backup this
// server still has. It's kept under a name of its own until it's checked.
func (r *Runner) Fetch(ctx context.Context, ref resource.Ref, id, key, want string, d *Destination) (*Manifest, error) {
	t := volume.TargetOf(ref)
	release, err := r.hold(t)
	if err != nil {
		return nil, err
	}
	defer release()
	if want == "" {
		return nil, fmt.Errorf("nothing says what %s should hash to, so it can't be checked. Update the control plane", id)
	}
	for _, format := range formats {
		if _, err := os.Stat(filepath.Join(r.dir(ref), id+"."+format)); err == nil {
			return nil, fmt.Errorf("this server still has %s, so there's nothing to fetch. Restore it instead", id)
		}
	}
	bucket, closeStore, err := r.remote(ctx, d, key)
	if err != nil {
		return nil, err
	}
	defer closeStore()
	bound := binding(sealedAs(ref, key), id)
	said, err := bucket.Get(ctx, key+".json")
	if err != nil {
		return nil, fmt.Errorf("can't read what was kept beside the backup: %w", refused(err, d))
	}
	var plain bytes.Buffer
	manifestBound := bound + manifestBinding
	sealedManifest, err := io.ReadAll(io.LimitReader(said, 1<<20))
	_ = said.Close()
	if err != nil {
		return nil, fmt.Errorf("can't read what was kept beside the backup: %w", refused(err, d))
	}
	given, err := seal.Open(&plain, bytes.NewReader(sealedManifest), d.Passphrase, manifestBound)
	if err != nil {
		var newer *seal.NewerArchiveError
		if d.SFTP != nil && given != manifestBound && !errors.As(err, &newer) &&
			!errors.Is(err, sftpstore.ErrRead) && !errors.Is(err, sftpstore.ErrIdle) && !errors.Is(err, sftpstore.ErrLocalIdle) {
			err = errSFTPInvalidCopy
		}
		return nil, fmt.Errorf("can't read what was kept beside the backup: %w", err)
	}
	var m Manifest
	if err := json.Unmarshal(plain.Bytes(), &m); err != nil {
		return nil, fmt.Errorf("can't read what was kept beside the backup: %w", err)
	}
	if m.ID != id || m.Volume != ref.Name || m.SHA256 != want || !slices.Contains(formats, m.Format) {
		return nil, fmt.Errorf("what's stored at %s isn't the backup %s", key, id)
	}
	m.Environment = ref.Scope
	if err := os.MkdirAll(r.dir(ref), 0o700); err != nil {
		return nil, fmt.Errorf("can't make a place for the backup: %w", err)
	}
	body, err := bucket.Get(ctx, key)
	if err != nil {
		return nil, fmt.Errorf("can't read the backup: %w", refused(err, d))
	}
	defer body.Close()
	verify := verifyArchive
	if isDump(m.Format) {
		k := kindNamed(m.Database)
		if k == nil {
			return nil, fmt.Errorf("this backup is a dump of %s, which this agent can't read. Update it to the control plane's version", m.Database)
		}
		verify = verifyDump(k)
	}
	resource.Progress(ctx, "Fetching "+m.Volume)
	changed := m.Changed
	err = r.writeChecked(ref, &m, func(w io.Writer) (int64, error) {
		given, err := seal.Open(w, body, d.Passphrase, bound)
		if err != nil && d.SFTP != nil {
			var newer *seal.NewerArchiveError
			switch {
			case errors.As(err, &newer):
				return changed, err
			case errors.Is(err, sftpstore.ErrRead):
				err = fmt.Errorf("%w, so Dockwell kept no local copy. Check the connection and fetch it again", err)
			case given != bound && !errors.Is(err, sftpstore.ErrIdle) && !errors.Is(err, sftpstore.ErrLocalIdle):
				err = errSFTPInvalidCopy
			}
		}
		return changed, err
	}, verify, want)
	if err != nil {
		return nil, err
	}
	return &m, nil
}

// deleteRemote removes a backup from where it was sent.
func (r *Runner) deleteRemote(ctx context.Context, key string, d *Destination) error {
	bucket, closeStore, err := r.remote(ctx, d, key)
	if err != nil {
		return err
	}
	defer closeStore()
	return errors.Join(redactS3(bucket.Delete(ctx, key), d), redactS3(bucket.Delete(ctx, key+".json"), d))
}
