// SPDX-License-Identifier: AGPL-3.0-only

// Package desired keeps what an agent's server should run: for each resource,
// what the last step applied and verified there, sealed on the agent's disk, so
// the server can be put back while the control plane is away (ADR 0054).
package desired

import (
	"bytes"
	"crypto/aes"
	"crypto/cipher"
	"crypto/rand"
	"crypto/sha256"
	"encoding/binary"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"log/slog"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"sync"

	agentv1 "github.com/dockwell/dockwell/gen/dockwell/agent/v1"
	"github.com/dockwell/dockwell/internal/resource"
	"github.com/dockwell/dockwell/internal/resource/resourcepb"
)

// The cache's layout in the agent's data folder.
const (
	// Folder holds a record for each resource.
	Folder = "desired"
	// KeyFile is the key the records are sealed with.
	KeyFile = "desired.key"
	// Damaged is where a record that doesn't open goes, in Folder.
	Damaged = "damaged"
	// suffix ends a record's name.
	suffix = ".rec"
)

// magic starts a record: the format's name and its version, which a later
// format changes, so an agent that goes back reads none of its records.
var magic = []byte("DWD\x01")

// info binds a record's sealing to the format, before the server and the resource.
const info = "dockwell.desired.v1"

// keySize is the key's length: AES-256.
const keySize = 32

// Record is one resource as its server should hold it.
type Record struct {
	Ref resource.Ref
	// ScopeName is the name of the resource's scope, which its Docker
	// objects are named after (ADR 0035), where its kind has one.
	ScopeName string
	// Revision is the revision the step ran toward, and Change the change
	// that made it.
	Revision int64
	Change   string
	// Spec is the spec, encoded as its kind's message. Nil means the resource
	// was deleted, which removes its record.
	Spec []byte
	// Values are the values of the variables and secrets the spec uses, at
	// the revisions it pins.
	Values map[string]string
	// Image is the image it runs, for a kind that runs one.
	Image resource.Image
}

// sealed is what a record keeps inside its sealing.
type sealed struct {
	ScopeName string            `json:"scope_name,omitempty"`
	Revision  int64             `json:"revision"`
	Change    string            `json:"change,omitempty"`
	Spec      []byte            `json:"spec"`
	Values    map[string]string `json:"values,omitempty"`
	Image     string            `json:"image,omitempty"`
	Digest    string            `json:"digest,omitempty"`
}

// Store is an agent's cache of desired state.
type Store struct {
	dir    string
	server string
	aead   cipher.AEAD
	logger *slog.Logger

	mu sync.Mutex
	// held is the revision of each record, which the store knows without
	// opening it.
	held    map[resource.Ref]int64
	damaged int
}

// ErrOlder says a record is for a revision older than the one kept.
var ErrOlder = errors.New("the agent keeps a later revision")

// Open opens the cache in dir, the agent's data folder, for server: it makes
// the key on first use, opens every record, and moves aside any that doesn't
// open, which it then acts on no more.
func Open(dir, server string, logger *slog.Logger) (*Store, error) {
	if logger == nil {
		logger = slog.New(slog.DiscardHandler)
	}
	if server == "" {
		return nil, errors.New("can't keep this server's desired state without its ID")
	}
	folder := filepath.Join(dir, Folder)
	if err := os.MkdirAll(folder, 0o700); err != nil {
		return nil, fmt.Errorf("can't make %s for this server's desired state: %w", folder, err)
	}
	if err := os.Chmod(folder, 0o700); err != nil { //nolint:gosec // a folder, for the agent alone
		return nil, fmt.Errorf("can't keep %s to the agent alone: %w", folder, err)
	}
	key, err := loadKey(filepath.Join(dir, KeyFile))
	if err != nil {
		return nil, err
	}
	block, err := aes.NewCipher(key)
	clear(key)
	if err != nil {
		return nil, err
	}
	aead, err := cipher.NewGCM(block)
	if err != nil {
		return nil, err
	}
	s := &Store{dir: folder, server: server, aead: aead, logger: logger, held: map[resource.Ref]int64{}}
	if err := s.load(); err != nil {
		return nil, err
	}
	return s, nil
}

// loadKey reads the key, or makes it if there's none. A key of the wrong length
// is put aside and a new one made, which leaves every record unreadable: they
// go aside too, and the control plane fills the cache again.
func loadKey(path string) ([]byte, error) {
	b, err := os.ReadFile(path) //nolint:gosec // the key's place in the agent's own folder
	switch {
	case err == nil && len(b) == keySize:
		return b, nil
	case err == nil:
		if err := os.Rename(path, path+".damaged"); err != nil {
			return nil, fmt.Errorf("can't put aside %s, which isn't a key: %w", path, err)
		}
	case !errors.Is(err, fs.ErrNotExist):
		return nil, fmt.Errorf("can't read the key to this server's desired state, %s: %w", path, err)
	}
	key := make([]byte, keySize)
	if _, err := rand.Read(key); err != nil {
		return nil, err
	}
	if err := writeDurably(path, key); err != nil {
		return nil, fmt.Errorf("can't keep the key to this server's desired state in %s: %w", path, err)
	}
	return key, nil
}

// load opens every record, keeping its resource and revision, and moves aside
// those that don't open.
func (s *Store) load() error {
	entries, err := os.ReadDir(s.dir)
	if err != nil {
		return fmt.Errorf("can't read this server's desired state in %s: %w", s.dir, err)
	}
	for _, e := range entries {
		name := e.Name()
		if e.IsDir() || !strings.HasSuffix(name, suffix) {
			continue
		}
		path := filepath.Join(s.dir, name)
		r, err := s.read(path)
		if err == nil && fileName(r.Ref) != name {
			err = errors.New("its name isn't its resource's")
		}
		if err != nil {
			s.putAside(path, err)
			continue
		}
		s.held[r.Ref] = r.Revision
	}
	return nil
}

// putAside moves a record that doesn't open to Damaged, where nothing reads it.
func (s *Store) putAside(path string, why error) {
	s.damaged++
	aside := filepath.Join(s.dir, Damaged)
	if err := os.MkdirAll(aside, 0o700); err == nil {
		err = os.Rename(path, filepath.Join(aside, filepath.Base(path)))
		if err == nil {
			s.logger.Warn("a record of this server's desired state doesn't open, as a power cut, an edit or another server's copy leaves one, so the agent put it aside and acts on it no more. "+
				"The control plane sends what it lacks when it connects", slog.String("record", filepath.Base(path)), slog.String("why", why.Error()))
			return
		}
	}
	// It can't be moved, so it's removed: a record that doesn't open is never read.
	_ = os.Remove(path)
	s.logger.Warn("a record of this server's desired state doesn't open, and can't be put aside, so the agent removed it. "+
		"The control plane sends what it lacks when it connects", slog.String("record", filepath.Base(path)), slog.String("why", why.Error()))
}

// Keep keeps r, or removes its record when its spec is nil, so an empty spec is
// an empty slice. A record for a revision older than the one kept is refused
// with ErrOlder, so a late or repeated message can't put the past back. A
// revision of zero, as a step that acts on a resource as it is has, keeps the
// revision there.
func (s *Store) Keep(r Record) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	was, kept := s.held[r.Ref]
	if kept && r.Revision != 0 && r.Revision < was {
		return fmt.Errorf("%w: it keeps %s at revision %d, and this is revision %d", ErrOlder, r.Ref, was, r.Revision)
	}
	if r.Revision == 0 {
		r.Revision = was
	}
	path := filepath.Join(s.dir, fileName(r.Ref))
	if r.Spec == nil {
		if err := os.Remove(path); err != nil && !errors.Is(err, fs.ErrNotExist) {
			return fmt.Errorf("can't forget %s, which was deleted: %w", r.Ref, err)
		}
		delete(s.held, r.Ref)
		return syncDir(s.dir)
	}
	b, err := s.seal(r)
	if err != nil {
		return err
	}
	if err := writeDurably(path, b); err != nil {
		return fmt.Errorf("can't keep %s as this server should run it: %w", r.Ref, err)
	}
	s.held[r.Ref] = r.Revision
	return nil
}

// Read opens the record of ref, or says there's none.
func (s *Store) Read(ref resource.Ref) (Record, bool, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if _, ok := s.held[ref]; !ok {
		return Record{}, false, nil
	}
	r, err := s.read(filepath.Join(s.dir, fileName(ref)))
	if err != nil {
		return Record{}, false, err
	}
	if r.Ref != ref {
		return Record{}, false, fmt.Errorf("the record of %s holds %s", ref, r.Ref)
	}
	return r, true, nil
}

// Holds says what the cache keeps, for the agent's hello.
func (s *Store) Holds() *agentv1.DesiredHeld {
	s.mu.Lock()
	defer s.mu.Unlock()
	out := &agentv1.DesiredHeld{Damaged: uint32(min(s.damaged, 1<<31))} //nolint:gosec // at most 1<<31
	refs := make([]resource.Ref, 0, len(s.held))
	for ref := range s.held {
		refs = append(refs, ref)
	}
	slices.SortFunc(refs, func(a, b resource.Ref) int { return strings.Compare(a.String(), b.String()) })
	for _, ref := range refs {
		out.Held = append(out.Held, &agentv1.HeldResource{Resource: resourcepb.Ref(ref), Revision: s.held[ref]})
	}
	return out
}

// fileName is a record's name: a hash of its resource, so no name a resource
// can have reaches the file system.
func fileName(ref resource.Ref) string {
	sum := sha256.Sum256([]byte(string(ref.Kind) + "\x00" + ref.Scope + "\x00" + ref.Name))
	return hex.EncodeToString(sum[:16]) + suffix
}

// additional is what a record's sealing is bound to: the format, this server and
// the resource.
func (s *Store) additional(ref resource.Ref) []byte {
	return []byte(info + "\x00" + s.server + "\x00" + string(ref.Kind) + "\x00" + ref.Scope + "\x00" + ref.Name)
}

// seal writes r as a record: the magic, its resource in the clear, then its
// nonce and what it holds, sealed.
func (s *Store) seal(r Record) ([]byte, error) {
	payload, err := json.Marshal(sealed{
		ScopeName: r.ScopeName, Revision: r.Revision, Change: r.Change, Spec: r.Spec, Values: r.Values,
		Image: r.Image.ID, Digest: r.Image.Digest,
	})
	if err != nil {
		return nil, err
	}
	defer clear(payload)
	var out bytes.Buffer
	out.Write(magic)
	for _, part := range []string{string(r.Ref.Kind), r.Ref.Scope, r.Ref.Name} {
		out.Write(binary.AppendUvarint(nil, uint64(len(part))))
		out.WriteString(part)
	}
	nonce := make([]byte, s.aead.NonceSize())
	if _, err := rand.Read(nonce); err != nil {
		return nil, err
	}
	out.Write(nonce)
	out.Write(s.aead.Seal(nil, nonce, payload, s.additional(r.Ref)))
	return out.Bytes(), nil
}

// maxRecord bounds what a record can be: a spec, and values of at most 64 KiB
// each, which no resource comes near.
const maxRecord = 16 << 20

// read opens the record at path.
func (s *Store) read(path string) (Record, error) {
	info, err := os.Stat(path)
	if err != nil {
		return Record{}, err
	}
	if info.Size() > maxRecord {
		return Record{}, fmt.Errorf("it's %d bytes, more than a record can be", info.Size())
	}
	b, err := os.ReadFile(path) //nolint:gosec // a record in the agent's own folder, named by a hash
	if err != nil {
		return Record{}, err
	}
	rest, ok := bytes.CutPrefix(b, magic)
	if !ok {
		return Record{}, errors.New("it isn't a record of this format")
	}
	var parts [3]string
	for i := range parts {
		n, size := binary.Uvarint(rest)
		if size <= 0 || n > uint64(len(rest)-size) { //nolint:gosec // size is at most len(rest), so this is never negative
			return Record{}, errors.New("it ends before its resource does")
		}
		parts[i] = string(rest[size : size+int(n)]) //nolint:gosec // n fits in rest
		rest = rest[size+int(n):]                   //nolint:gosec // n fits in rest
	}
	ref := resource.Ref{Kind: resource.Kind(parts[0]), Scope: parts[1], Name: parts[2]}
	if len(rest) < s.aead.NonceSize() {
		return Record{}, errors.New("it ends before its sealing does")
	}
	nonce, box := rest[:s.aead.NonceSize()], rest[s.aead.NonceSize():]
	payload, err := s.aead.Open(nil, nonce, box, s.additional(ref))
	if err != nil {
		return Record{}, errors.New("its sealing doesn't open: it was changed, cut short, sealed with another key, or for another server")
	}
	defer clear(payload)
	var in sealed
	if err := json.Unmarshal(payload, &in); err != nil {
		return Record{}, fmt.Errorf("what it holds doesn't read: %w", err)
	}
	if in.Spec == nil {
		in.Spec = []byte{}
	}
	return Record{
		Ref: ref, ScopeName: in.ScopeName, Revision: in.Revision, Change: in.Change, Spec: in.Spec, Values: in.Values,
		Image: resource.Image{ID: in.Image, Digest: in.Digest},
	}, nil
}

// writeDurably writes b to path, so a power cut leaves what was there or b: a
// temporary file beside it, synced, renamed over it, and the folder synced. The
// file is the agent's alone.
func writeDurably(path string, b []byte) error {
	dir := filepath.Dir(path)
	f, err := os.CreateTemp(dir, "."+filepath.Base(path)+".*")
	if err != nil {
		return err
	}
	tmp := f.Name()
	defer func() { _ = os.Remove(tmp) }()
	if err := f.Chmod(0o600); err != nil {
		_ = f.Close()
		return err
	}
	if _, err := f.Write(b); err != nil {
		_ = f.Close()
		return err
	}
	if err := f.Sync(); err != nil {
		_ = f.Close()
		return err
	}
	if err := f.Close(); err != nil {
		return err
	}
	if err := os.Rename(tmp, path); err != nil {
		return err
	}
	return syncDir(dir)
}

// syncDir syncs a folder, so a rename or a removal in it lasts a power cut.
func syncDir(dir string) error {
	d, err := os.Open(dir) //nolint:gosec // the agent's own folder
	if err != nil {
		return err
	}
	defer func() { _ = d.Close() }()
	return d.Sync()
}
