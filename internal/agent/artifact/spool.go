// SPDX-License-Identifier: AGPL-3.0-only

package artifact

import (
	"archive/tar"
	"bufio"
	"compress/gzip"
	"context"
	"crypto/rand"
	"crypto/sha256"
	"encoding"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"time"
	"unicode"

	"golang.org/x/sys/unix"
	"google.golang.org/protobuf/proto"
	"google.golang.org/protobuf/types/known/timestamppb"

	agentv1 "github.com/dockwell/dockwell/gen/dockwell/agent/v1"
	v1 "github.com/dockwell/dockwell/gen/dockwell/v1"
	"github.com/dockwell/dockwell/internal/human"
)

// Artifact operations and states used by the spool and its callers.
const (
	Import     = v1.DockerArtifactKind_DOCKER_ARTIFACT_KIND_IMAGE_IMPORT
	Export     = v1.DockerArtifactKind_DOCKER_ARTIFACT_KIND_IMAGE_EXPORT
	PushSource = v1.DockerArtifactKind_DOCKER_ARTIFACT_KIND_PUSH_SOURCE
	Receiving  = v1.DockerArtifactState_DOCKER_ARTIFACT_STATE_RECEIVING
	Checking   = v1.DockerArtifactState_DOCKER_ARTIFACT_STATE_CHECKING
	Ready      = v1.DockerArtifactState_DOCKER_ARTIFACT_STATE_READY
	Failed     = v1.DockerArtifactState_DOCKER_ARTIFACT_STATE_FAILED
	Aborted    = v1.DockerArtifactState_DOCKER_ARTIFACT_STATE_ABORTED
)

// Refusals that the agent can send without including paths or archive contents.
var (
	ErrUnavailable   = errors.New("artifact is unavailable or has expired")
	ErrOffset        = errors.New("artifact changed since the last transfer. Read its committed offset")
	ErrLeaseLimit    = errors.New("this server has too many active artifact leases")
	ErrQuota         = errors.New("there is not enough space for this artifact")
	ErrPlanned       = errors.New("this archive belongs to a reviewed change. Discard the change before using the archive again")
	ErrTransferSlots = errors.New("waiting for a transfer slot")
	ErrBusy          = errors.New("an artifact transfer is already in progress")
)

// SpaceError gives the required and usable bytes at a storage refusal.
type SpaceError struct{ Needed, Available uint64 }

func (e *SpaceError) Error() string {
	return fmt.Sprintf("the archive needs %s, with %s available",
		human.Size(int64(e.Needed)), human.Size(int64(e.Available))) //nolint:gosec // counts are bounded by the spool limits
}

func (*SpaceError) Unwrap() error { return ErrQuota }

// gzipEstimate is the initial expansion reservation before staging begins.
func gzipEstimate(size uint64) uint64 { return min(4*size, 256<<20) }

type journal struct {
	Owner          string
	Expected       string
	HashState      []byte
	Expanded       bool
	Gzip           bool
	Pin            *agentv1.DockerArtifactPin
	PinOrphanAfter time.Time
	Info           *v1.DockerArtifactInfo
}

type lease struct {
	mu      sync.RWMutex
	dir     string
	expires time.Time
	journal
	// reserve is the work left to write, in addition to bytes statfs already counts.
	reserve uint64
	// The spool mutex protects the published snapshot and idle deadline.
	snapshot    *v1.DockerArtifactInfo
	idleUntil   time.Time
	pinned      *agentv1.DockerArtifactPin
	orphanAfter time.Time
}

// Spool keeps private artifacts and their disk reservations on one server.
type Spool struct {
	mu         sync.Mutex
	dir        string
	leases     map[string]*lease
	reserved   uint64
	validation chan struct{}
	pushes     chan struct{}
	wake       chan struct{}
	sealingID  string
	sealCancel context.CancelFunc
	sealDone   chan struct{}
	now        func() time.Time
}

// ValidID recognizes the random, hex-encoded IDs used for lease directories.
func ValidID(id string) bool {
	raw, err := hex.DecodeString(id)
	return len(id) == 32 && len(raw) == 16 && err == nil
}

// Open reconciles the journal before admitting any streams. Work that might
// have outlived a process is truncated to the last committed segment.
func Open(dir string) (*Spool, error) {
	return OpenWithClock(dir, time.Now)
}

// OpenWithClock reconciles a spool using the supplied lease clock.
func OpenWithClock(dir string, now func() time.Time) (*Spool, error) {
	if now == nil {
		now = time.Now
	}
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return nil, err
	}
	s := &Spool{dir: dir, leases: make(map[string]*lease), validation: make(chan struct{}, 1), pushes: make(chan struct{}, 4), wake: make(chan struct{}, 1), now: now}
	dirs, err := os.ReadDir(dir)
	if err != nil {
		return nil, err
	}
	for _, item := range dirs {
		name := item.Name()
		at := filepath.Join(dir, name)
		l := &lease{dir: at}
		b, e := readJournal(item, at)
		if !item.IsDir() || !ValidID(name) || e != nil || len(b) > maxJSON || json.Unmarshal(b, &l.journal) != nil || !l.recover(name, now()) {
			if err := os.RemoveAll(at); err != nil {
				return nil, err
			}
			continue
		}
		s.publish(l)
		s.leases[name] = l
		s.reserved += l.reserve
	}
	return s, nil
}

func readJournal(item os.DirEntry, dir string) ([]byte, error) {
	if !item.IsDir() || !ValidID(item.Name()) {
		return nil, ErrUnavailable
	}
	f, err := os.Open(filepath.Join(dir, "lease.json")) //nolint:gosec // a validated ID inside the agent spool
	if err != nil {
		return nil, err
	}
	defer f.Close()
	return io.ReadAll(io.LimitReader(f, maxJSON+1))
}

func (l *lease) recover(id string, now time.Time) bool {
	info := l.Info
	l.expires = info.GetExpiresAt().AsTime()
	if info.GetArtifactId() != id || l.Owner == "" || !retained(info, l.Pin) && !now.Before(l.expires) || info.GetSizeBytes() > MaxBytes || info.GetReceivedBytes() > info.GetSizeBytes() {
		return false
	}
	size := int64(info.SizeBytes)         //nolint:gosec // journal sizes above MaxBytes are discarded
	received := int64(info.ReceivedBytes) //nolint:gosec // journal rejects offsets beyond the size
	_ = os.Remove(filepath.Join(l.dir, "lease.tmp"))
	_ = os.Remove(filepath.Join(l.dir, "work.tar"))
	_ = os.RemoveAll(filepath.Join(l.dir, "build-tree"))
	if UploadKind(info.GetKind()) && (info.GetState() == Receiving || info.GetState() == Checking) {
		_ = os.Remove(filepath.Join(l.dir, "expanded.tar"))
		fi, err := os.Stat(l.source())
		if err != nil || fi.Size() < received || os.Truncate(l.source(), received) != nil {
			return false
		}
		l.reserve = info.SizeBytes - info.ReceivedBytes
		if l.Gzip {
			l.reserve += gzipEstimate(info.GetSizeBytes())
		}
		return true
	}
	if UploadKind(info.GetKind()) && info.GetState() == Failed {
		_ = os.Remove(l.source())
		return true
	}
	if UploadKind(info.GetKind()) && info.GetState() == Ready {
		fi, err := os.Stat(l.source())
		var limit int64 = MaxExpanded
		switch info.Kind { //nolint:exhaustive // imports use the larger default; only upload kinds reach here
		case BuildContext:
			limit = ContextExpanded
		case Dockerfile:
			limit = DockerfileSize
		}
		return err == nil && fi.Size() > 0 && fi.Size() <= limit && (l.Expanded || fi.Size() == size)
	}
	if info.GetKind() == Export && info.GetState() == Checking {
		_ = os.Remove(l.source())
		return true
	}
	if (info.GetKind() == Export || info.GetKind() == PushSource) && info.GetState() == Ready {
		fi, err := os.Stat(l.source())
		return err == nil && fi.Size() == size
	}
	return false
}

func (l *lease) source() string {
	if l.Expanded {
		return filepath.Join(l.dir, "expanded.tar")
	}
	return filepath.Join(l.dir, "bytes")
}

func (l *lease) save() error {
	tmp := filepath.Join(l.dir, "lease.tmp")
	f, err := os.OpenFile(tmp, os.O_WRONLY|os.O_CREATE|os.O_TRUNC, 0o600) //nolint:gosec // fixed name inside a private lease directory
	if err != nil {
		return err
	}
	err = json.NewEncoder(f).Encode(&l.journal)
	if err == nil {
		err = f.Sync()
	}
	if cerr := f.Close(); err == nil {
		err = cerr
	}
	if err != nil {
		return err
	}
	if err := os.Rename(tmp, filepath.Join(l.dir, "lease.json")); err != nil {
		return err
	}
	dir, err := os.Open(l.dir)
	if err != nil {
		return err
	}
	defer dir.Close()
	return dir.Sync()
}

// headroom is read while the reservation table is locked. Count files already
// written separately from the promises still waiting to be written.
func (s *Spool) headroom() uint64 {
	var st unix.Statfs_t
	if unix.Statfs(s.dir, &st) != nil {
		return 0
	}
	blockSize := uint64(st.Bsize) //nolint:gosec // filesystem block sizes are positive
	free := st.Bavail * blockSize
	floor := diskFloor(st.Blocks * blockSize)
	used := s.reserved
	err := filepath.WalkDir(s.dir, func(_ string, d os.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if d.Type().IsRegular() {
			fi, err := d.Info()
			if err != nil {
				if errors.Is(err, os.ErrNotExist) {
					return nil
				}
				return err
			}
			used += uint64(fi.Size()) //nolint:gosec // a regular file has a nonnegative size
		}
		return nil
	})
	if err != nil || used >= 24<<30 || free <= floor+s.reserved {
		return 0
	}
	return min(24<<30-used, free-floor-s.reserved)
}

// Begin starts a private lease and reserves space for its declared upload.
func (s *Spool) Begin(owner string, kind v1.DockerArtifactKind, size uint64, sha string, compressed bool, name string) (*v1.DockerArtifactInfo, error) {
	upload := UploadKind(kind)
	if owner == "" || len(owner) > 128 || !upload && kind != Export && kind != PushSource ||
		upload && (size == 0 || size > uploadLimit(kind)) || kind == Dockerfile && compressed ||
		!upload && (size != 0 || compressed || name != "") {
		return nil, ErrArchive
	}
	if sha != "" {
		decoded, err := hex.DecodeString(sha)
		if err != nil || len(decoded) != 32 || len(sha) != 64 {
			return nil, ErrArchive
		}
	}
	if len(name) > 255 || strings.ContainsAny(name, "/\\") || strings.ContainsFunc(name, unicode.IsControl) {
		return nil, ErrArchive
	}
	reservation := size
	if compressed {
		reservation += gzipEstimate(size)
	}
	s.Prune(s.now())
	s.mu.Lock()
	defer s.mu.Unlock()
	if len(s.leases) >= 64 {
		return nil, ErrLeaseLimit
	}
	if free := s.headroom(); reservation > free {
		return nil, &SpaceError{Needed: reservation, Available: free}
	}
	var nonce [16]byte
	if _, err := rand.Read(nonce[:]); err != nil {
		return nil, err
	}
	id := hex.EncodeToString(nonce[:])
	at := filepath.Join(s.dir, id)
	if err := os.Mkdir(at, 0o700); err != nil {
		return nil, err
	}
	now := s.now()
	l := &lease{dir: at, expires: now.Add(Lifetime), idleUntil: now.Add(idleLifetime), reserve: reservation}
	l.Owner, l.Expected, l.Gzip = owner, sha, compressed
	l.Info = &v1.DockerArtifactInfo{ArtifactId: id, Kind: kind, State: Receiving, SizeBytes: size, MaxBytes: uploadLimit(kind), FileName: name, ExpiresAt: timestamppb.New(l.expires)}
	if kind == Export || kind == PushSource {
		l.Info.State = Checking
	}
	if UploadKind(kind) {
		f, err := os.OpenFile(l.source(), os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0o600)
		if err == nil {
			err = f.Close()
		}
		if err != nil {
			_ = os.RemoveAll(at)
			return nil, err
		}
	}
	l.Info.CommittedPrefixSha256 = hex.EncodeToString(sha256.New().Sum(nil))
	if err := l.save(); err != nil {
		_ = os.RemoveAll(at)
		return nil, err
	}
	l.snapshot = proto.CloneOf(l.Info)
	s.leases[id] = l
	s.reserved += reservation
	return proto.CloneOf(l.Info), nil
}

func (s *Spool) locked(id, owner string, read bool) (*lease, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	l := s.leases[id]
	if l == nil || l.expired(s.now()) || owner != "" && owner != l.Owner {
		return nil, ErrUnavailable
	}
	take := l.mu.TryLock
	if read {
		take = l.mu.TryRLock
	}
	if !take() {
		return nil, ErrBusy
	}
	return l, nil
}

// TransferSlotsReason labels a retryable transfer admission refusal.
const TransferSlotsReason = "artifact_transfer_slots"

const idleLifetime = 15 * time.Minute

func sealedExport(info *v1.DockerArtifactInfo) bool {
	return info.GetKind() == Export && info.GetState() == Ready
}

func retained(info *v1.DockerArtifactInfo, pin *agentv1.DockerArtifactPin) bool {
	return pin != nil && !sealedExport(info) || UploadKind(info.GetKind()) && info.GetState() == Checking
}

func (l *lease) expired(now time.Time) bool {
	if retained(l.snapshot, l.pinned) {
		return false
	}
	return !now.Before(l.expires) || !sealedExport(l.snapshot) && !now.Before(l.idleUntil)
}

func (s *Spool) publish(l *lease) {
	s.mu.Lock()
	defer s.mu.Unlock()
	l.snapshot = proto.CloneOf(l.Info)
	l.pinned = proto.CloneOf(l.Pin)
	l.orphanAfter = l.PinOrphanAfter
	l.expires = l.Info.GetExpiresAt().AsTime()
	if l.pinned != nil && !sealedExport(l.Info) {
		l.snapshot.ExpiresAt = nil
	}
	l.idleUntil = s.now().Add(idleLifetime)
}

// Status returns the committed metadata without waiting for a byte writer.
func (s *Spool) Status(id, owner string) (*v1.DockerArtifactInfo, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	l := s.leases[id]
	now := s.now()
	if l == nil || l.expired(now) || owner != "" && owner != l.Owner {
		return nil, ErrUnavailable
	}
	if owner != "" && l.snapshot.GetState() != Failed {
		l.idleUntil = now.Add(idleLifetime)
	}
	return proto.CloneOf(l.snapshot), nil
}

// List returns lightweight lease metadata for one actor and session. It does not
// renew a lease or copy its archive preview into the response.
func (s *Spool) List(owner string) []*v1.DockerArtifactInfo {
	s.mu.Lock()
	defer s.mu.Unlock()
	now := s.now()
	var items []*v1.DockerArtifactInfo
	for _, l := range s.leases {
		if l.Owner == owner && !l.expired(now) {
			item := l.snapshot
			if item.GetKind() == PushSource {
				continue
			}
			items = append(items, &v1.DockerArtifactInfo{
				ArtifactId: item.GetArtifactId(), Kind: item.GetKind(), State: item.GetState(),
				SizeBytes: item.GetSizeBytes(), ReceivedBytes: item.GetReceivedBytes(),
				FileName: item.GetFileName(), ExpiresAt: proto.CloneOf(item.GetExpiresAt()),
			})
		}
	}
	slices.SortFunc(items, func(a, b *v1.DockerArtifactInfo) int {
		return b.GetExpiresAt().AsTime().Compare(a.GetExpiresAt().AsTime())
	})
	return items
}

func (s *Spool) release(l *lease, n uint64) {
	s.mu.Lock()
	defer s.mu.Unlock()
	l.reserve -= n
	s.reserved -= n
}

func (s *Spool) reserveUpTo(l *lease, maxBytes uint64) uint64 {
	s.mu.Lock()
	defer s.mu.Unlock()
	n := min(maxBytes, s.headroom())
	l.reserve += n
	s.reserved += n
	return n
}

func (s *Spool) remove(l *lease) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if err := os.RemoveAll(l.dir); err != nil {
		return err
	}
	s.reserved -= l.reserve
	delete(s.leases, l.Info.ArtifactId)
	return nil
}

// Prune drops expired leases whose files are idle.
func (s *Spool) Prune(now time.Time) {
	s.mu.Lock()
	defer s.mu.Unlock()
	for id, l := range s.leases {
		if !l.expired(now) || !l.mu.TryLock() {
			continue
		}
		if os.RemoveAll(l.dir) == nil {
			s.reserved -= l.reserve
			delete(s.leases, id)
		}
		l.mu.Unlock()
	}
}

// AbortUpload cancels a running validation before discarding its files.
func (s *Spool) AbortUpload(ctx context.Context, id, owner string) (*v1.DockerArtifactInfo, error) {
	s.mu.Lock()
	l := s.leases[id]
	var done <-chan struct{}
	if l != nil && l.Owner == owner && s.sealingID == id {
		s.sealCancel()
		done = s.sealDone
	}
	s.mu.Unlock()
	if done != nil {
		select {
		case <-done:
		case <-ctx.Done():
			return nil, ctx.Err()
		}
	}
	return s.Abort(id, owner)
}

// Abort releases only the selected lease and its staged bytes.
func (s *Spool) Abort(id, owner string) (*v1.DockerArtifactInfo, error) {
	l, err := s.locked(id, owner, false)
	if err != nil {
		return nil, err
	}
	defer l.mu.Unlock()
	if owner != "" && l.Pin != nil && !sealedExport(l.Info) {
		return nil, ErrPlanned
	}
	if err := s.remove(l); err != nil {
		return nil, err
	}
	info := proto.CloneOf(l.Info)
	info.State = Aborted
	return info, nil
}

// DiscardExport releases an unpublished lease from a discarded control-plane plan.
func (s *Spool) DiscardExport(id string) error {
	l, err := s.locked(id, "", false)
	if err != nil {
		return err
	}
	defer l.mu.Unlock()
	if l.Pin != nil || l.Info.GetKind() != Export || l.Info.GetState() != Checking {
		return ErrUnavailable
	}
	return s.remove(l)
}

// Receive commits one exact segment after syncing its bytes and journal.
func (s *Spool) Receive(ctx context.Context, id, owner string, offset, length uint64, next func() ([]byte, error)) (*v1.DockerArtifactInfo, error) {
	l, err := s.locked(id, owner, false)
	if err != nil {
		return nil, err
	}
	defer l.mu.Unlock()
	if !UploadKind(l.Info.Kind) || l.Info.State != Receiving || offset != l.Info.ReceivedBytes || length == 0 || length > MaxChunk || length > l.Info.SizeBytes-offset {
		return nil, ErrOffset
	}
	f, err := os.OpenFile(l.source(), os.O_WRONLY, 0o600)
	if err != nil {
		return nil, err
	}
	defer f.Close()
	start := int64(offset) //nolint:gosec // offset is bounded by the 8 GiB lease
	if _, err = f.Seek(start, io.SeekStart); err != nil {
		return nil, err
	}
	hash := sha256.New()
	if offset > 0 {
		if err = hash.(encoding.BinaryUnmarshaler).UnmarshalBinary(l.HashState); err != nil {
			return nil, ErrOffset
		}
	}
	err = receiveFrames(ctx, length, next, io.MultiWriter(f, hash))
	if err == nil {
		err = f.Sync()
	}
	if err != nil {
		_ = f.Truncate(start)
		return nil, err
	}
	previous, previousState := proto.CloneOf(l.Info), l.HashState
	l.HashState, err = hash.(encoding.BinaryMarshaler).MarshalBinary()
	if err != nil {
		return nil, err
	}
	l.Info.ReceivedBytes += length
	l.Info.CommittedPrefixSha256 = hex.EncodeToString(hash.Sum(nil))
	if err := l.save(); err != nil {
		l.Info, l.HashState = previous, previousState
		_ = f.Truncate(start)
		return nil, err
	}
	s.release(l, length)
	s.publish(l)
	return proto.CloneOf(l.Info), nil
}

// receiveFrames uses the byte lane's reusable buffer before asking for the next frame.
func receiveFrames(ctx context.Context, remaining uint64, next func() ([]byte, error), dst io.Writer) error {
	for {
		if err := ctx.Err(); err != nil {
			return err
		}
		frame, err := next()
		if errors.Is(err, io.EOF) && remaining == 0 {
			return nil
		}
		if err != nil {
			return err
		}
		n := uint64(len(frame))
		if n == 0 || n > Frame || n > remaining {
			return ErrOffset
		}
		if _, err := dst.Write(frame); err != nil {
			return err
		}
		remaining -= n
	}
}

// Seal verifies the committed upload and keeps its immutable preview.
func (s *Spool) Seal(ctx context.Context, id, owner string) (*v1.DockerArtifactInfo, error) {
	l, err := s.locked(id, owner, false)
	if err != nil {
		return nil, err
	}
	defer l.mu.Unlock()
	if !UploadKind(l.Info.Kind) {
		return nil, ErrUnavailable
	}
	if l.Info.State == Ready {
		return proto.CloneOf(l.Info), nil
	}
	if l.Info.ReceivedBytes != l.Info.SizeBytes || l.Expected != "" && l.Expected != l.Info.CommittedPrefixSha256 {
		return nil, ErrOffset
	}
	src, err := os.Open(l.source())
	if err != nil {
		return nil, err
	}
	defer src.Close()
	r := bufio.NewReader(src)
	magic, _ := r.Peek(2)
	f, n := src, int64(l.Info.SizeBytes) //nolint:gosec // Begin and recovery cap sizes at MaxBytes
	expanded := len(magic) == 2 && magic[0] == 0x1f && magic[1] == 0x8b
	if expanded != l.Gzip {
		return nil, ErrArchive
	}
	if expanded {
		f, err = os.OpenFile(filepath.Join(l.dir, "expanded.tar"), os.O_RDWR|os.O_CREATE|os.O_TRUNC, 0o600)
		if err != nil {
			return nil, err
		}
		defer f.Close()
		defer func() {
			if !l.Expanded {
				_ = os.Remove(f.Name())
			}
		}()
		gz, err := gzip.NewReader(r)
		if err != nil {
			return nil, ErrArchive
		}
		defer gz.Close()
		w := &quotaWriter{spool: s, lease: l, dst: f, limit: expansionLimit(l.Info.Kind), reserve: l.reserve}
		n, err = io.CopyBuffer(w, checkedReader{ctx, gz}, make([]byte, Frame))
		w.close()
		if err != nil {
			if errors.Is(err, ErrQuota) || errors.Is(err, context.Canceled) || errors.Is(err, context.DeadlineExceeded) {
				return nil, err
			}
			return nil, ErrArchive
		}
		if err := f.Sync(); err != nil {
			return nil, err
		}
	}
	var images []*v1.DockerArchiveImage
	switch l.Info.Kind {
	case Import:
		a, err := untar(ctx, f, n)
		if err != nil {
			return nil, err
		}
		images = a.images
	case BuildContext:
		if n < 2*tarBlock {
			return nil, ErrArchive
		}
		if _, err := f.Seek(0, io.SeekStart); err != nil {
			return nil, err
		}
		if err := walkBuildTar(ctx, f, func(*tar.Header, io.Reader) error { return nil }); err != nil {
			return nil, err
		}
	case Dockerfile:
		// Length and digest are already checked. Only the bounded text reader uses it.
	case Export, PushSource, v1.DockerArtifactKind_DOCKER_ARTIFACT_KIND_UNSPECIFIED:
		return nil, ErrUnavailable
	}
	previous := proto.CloneOf(l.Info)
	l.Expanded = expanded
	l.Info.Images = images
	l.Info.State = Ready
	l.Info.Sha256 = l.Info.CommittedPrefixSha256
	if end := s.now().Add(idleLifetime); end.After(l.expires) {
		l.Info.ExpiresAt = timestamppb.New(end)
	}
	if err := l.save(); err != nil {
		l.Info, l.Expanded = previous, false
		return nil, err
	}
	if expanded {
		_ = os.Remove(src.Name())
	}
	s.publish(l)
	return proto.CloneOf(l.Info), nil
}

// WithArchive reads the exact sealed import while keeping its lease pinned.
func (s *Spool) WithArchive(ctx context.Context, id, sha string, each func(*Archive) error) error {
	l, err := s.locked(id, "", false)
	if err != nil {
		return err
	}
	defer l.mu.Unlock()
	if !UploadKind(l.Info.Kind) || l.Info.State != Ready || l.Info.Sha256 != sha {
		return ErrUnavailable
	}
	f, err := os.Open(l.source())
	if err != nil {
		return err
	}
	defer f.Close()
	st, err := f.Stat()
	if err != nil {
		return err
	}
	a, err := untar(ctx, f, st.Size())
	if err != nil {
		return err
	}
	return each(&Archive{a: a})
}

// Archive is one checked Docker save archive and its selected image entries.
type Archive struct{ a *archive }

// RoomForLoad checks the local filesystem that receives the expanded layers.
// A daemon whose filesystem is outside this process leaves the check to Engine.
func (a *Archive) RoomForLoad(root string) error {
	var st unix.Statfs_t
	if root == "" || unix.Statfs(root, &st) != nil {
		return nil //nolint:nilerr // the remote Engine owns the filesystem check
	}
	blocks := uint64(st.Bsize) //nolint:gosec // filesystem block sizes are positive
	free, floor := st.Bavail*blocks, diskFloor(st.Blocks*blocks)
	usable := free - min(free, floor)
	if a.a.expanded > usable {
		return &SpaceError{Needed: a.a.expanded, Available: usable}
	}
	return nil
}

func diskFloor(size uint64) uint64 { return min(max(size/20, 1<<30), 10<<30) }

// WriteImage streams one verified image with exactly the supplied tags.
func (a *Archive) WriteImage(ctx context.Context, w io.Writer, id string, tags []string) error {
	return a.a.write(ctx, w, id, tags)
}

// Publish takes one Engine save stream, rewrites the reviewed aliases and
// atomically publishes its immutable result. Engine output is never served directly.
func (s *Spool) Publish(ctx context.Context, id string, src io.Reader, tags []string, verify func(*Archive, string) bool, manifestDigest ...string) (*v1.DockerArtifactInfo, error) {
	l, err := s.locked(id, "", false)
	if err != nil {
		return nil, err
	}
	defer l.mu.Unlock()
	if l.Info.Kind != Export {
		return nil, ErrUnavailable
	}
	if l.Info.State == Ready {
		return proto.CloneOf(l.Info), nil
	}
	raw, err := os.OpenFile(filepath.Join(l.dir, "work.tar"), os.O_RDWR|os.O_CREATE|os.O_TRUNC, 0o600)
	if err != nil {
		return nil, err
	}
	defer raw.Close()
	defer os.Remove(raw.Name())
	copyBuffer := make([]byte, Frame)
	stage := &quotaWriter{spool: s, lease: l, dst: raw, limit: MaxBytes}
	n, err := io.CopyBuffer(stage, checkedReader{ctx, src}, copyBuffer)
	stage.close()
	if err != nil {
		return nil, err
	}
	a, err := untar(ctx, raw, n, manifestDigest...)
	if err != nil || len(a.images) != 1 || (a.exportConfig != "" && a.exportConfig != a.images[0].ImageId) || !verify(&Archive{a: a}, a.images[0].ImageId) {
		return nil, ErrArchive
	}
	out, err := os.OpenFile(l.source(), os.O_RDWR|os.O_CREATE|os.O_TRUNC, 0o600)
	if err != nil {
		return nil, err
	}
	defer out.Close()
	defer func() {
		if l.Info.State != Ready {
			_ = os.Remove(out.Name())
		}
	}()
	hash := sha256.New()
	measure := &quotaWriter{spool: s, lease: l, dst: io.MultiWriter(out, hash), limit: MaxBytes}
	defer measure.close()
	if err := a.write(ctx, measure, a.images[0].ImageId, tags); err != nil {
		return nil, err
	}
	if err := out.Sync(); err != nil {
		return nil, err
	}
	previous := proto.CloneOf(l.Info)
	l.Info.State = Ready
	l.Info.ExpiresAt = timestamppb.New(s.now().Add(Lifetime))
	l.Info.Sha256 = hex.EncodeToString(hash.Sum(nil))
	l.Info.SizeBytes = measure.written
	l.Info.FileName = "docker-image-" + a.images[0].ImageId[7:19] + ".tar"
	if err := l.save(); err != nil {
		l.Info = previous
		return nil, err
	}
	s.publish(l)
	return proto.CloneOf(l.Info), nil
}

// quotaWriter reserves a small window ahead of a disk write. Once written, disk
// accounting counts those bytes and the writer returns the matching reservation.
type quotaWriter struct {
	spool                   *Spool
	lease                   *lease
	dst                     io.Writer
	limit, reserve, written uint64
}

func (w *quotaWriter) Write(p []byte) (int, error) {
	size := uint64(len(p))
	if size > w.limit-w.written {
		return 0, ErrArchive
	}
	if size > w.reserve {
		w.reserve += w.spool.reserveUpTo(w.lease, min(4<<20, w.limit-w.written-w.reserve))
		if size > w.reserve {
			return 0, &SpaceError{Needed: size, Available: w.reserve}
		}
	}
	n, err := w.dst.Write(p)
	used := uint64(n) //nolint:gosec // io.Writer reports a nonnegative byte count
	w.written += used
	w.reserve -= used
	w.spool.release(w.lease, used)
	if errors.Is(err, unix.ENOSPC) || errors.Is(err, unix.EDQUOT) {
		return n, &SpaceError{Needed: size - used, Available: 0}
	}
	return n, err
}

func (w *quotaWriter) close() {
	w.spool.release(w.lease, w.reserve)
	w.reserve = 0
}

// Send writes a single byte range from a sealed export.
func (s *Spool) Send(ctx context.Context, id, owner string, offset, length uint64, dst io.Writer) error {
	// Open the sealed file under the lock. Concurrent readers have independent
	// offsets and a later deletion cannot change the already-open bytes.
	l, err := s.locked(id, owner, true)
	if err != nil {
		return err
	}
	if l.Info.Kind != Export || l.Info.State != Ready || offset > l.Info.SizeBytes || length > l.Info.SizeBytes-offset {
		l.mu.RUnlock()
		return ErrOffset
	}
	f, err := os.Open(l.source())
	l.mu.RUnlock()
	if err != nil {
		return err
	}
	defer f.Close()
	start, count := int64(offset), int64(length) //nolint:gosec // both are within the sealed export's 8 GiB limit
	n, err := io.CopyBuffer(dst, checkedReader{ctx, io.NewSectionReader(f, start, count)}, make([]byte, 31<<10))
	if err == nil && n != count {
		return io.ErrUnexpectedEOF
	}
	return err
}
