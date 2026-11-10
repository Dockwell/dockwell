// SPDX-License-Identifier: AGPL-3.0-only

// Package desired keeps what an agent's server should run: for each resource,
// what the last step applied and verified there, sealed on the agent's disk, so
// the server can be put back while the control plane is away (ADR 0054).
package desired

import (
	"bytes"
	"crypto/aes"
	"crypto/cipher"
	"crypto/hmac"
	"crypto/rand"
	"crypto/sha256"
	"encoding/binary"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"log/slog"
	"maps"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"time"

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

// TombstoneLife is how long a deletion holds back what the control plane sends
// of the resource: far longer than any fill, whose calls each end within a
// minute, so a fill that read what ran before the deletion can't bring it back.
const TombstoneLife = 7 * 24 * time.Hour

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
	// was deleted, which leaves a tombstone, so an empty spec is an empty slice.
	Spec []byte
	// Values are the values of the variables and secrets the spec uses, at
	// the revisions it pins.
	Values map[string]string
	// Image is the image it runs, for a kind that runs one.
	Image resource.Image
	// Stopped says the agent last saw it stopped by someone, or exited by itself,
	// where its restart policy left it, so it isn't made again while the control
	// plane is away: that would start it (ADR 0054). Keep and Remember carry it
	// over, unless Started.
	Stopped bool
	// Volumes are the volumes Docker made for the paths its image declares, by
	// path, as the agent last saw them mounted: a database's data, which no spec
	// names. One made again mounts them by name. Keep and Remember carry them over
	// when they're nil.
	Volumes map[string]string
	// Started says the step that kept it started the container, so it runs:
	// Keep doesn't carry over what was seen of it stopped. It isn't kept.
	Started bool
}

// Standing is how the agent saw a resource that runs something stand.
type Standing struct {
	// Stopped says it was stopped by someone, or exited by itself.
	Stopped bool
	// Volumes are the volumes Docker made for its image's paths, by path.
	Volumes map[string]string
}

// sealed is what a record keeps inside its sealing.
type sealed struct {
	ScopeName string            `json:"scope_name,omitempty"`
	Revision  int64             `json:"revision"`
	Change    string            `json:"change,omitempty"`
	Spec      []byte            `json:"spec,omitempty"`
	Values    map[string]string `json:"values,omitempty"`
	Image     string            `json:"image,omitempty"`
	Digest    string            `json:"digest,omitempty"`
	Stopped   bool              `json:"stopped,omitempty"`
	Volumes   map[string]string `json:"volumes,omitempty"`
	// Gone marks a tombstone: the resource was deleted at Revision, at At, in
	// Unix seconds. It holds nothing else.
	Gone bool  `json:"gone,omitempty"`
	At   int64 `json:"at,omitempty"`
}

// entry is what the store knows of a resource without opening its record.
type entry struct {
	revision int64
	// gone is a tombstone's, written at.
	gone bool
	at   time.Time
	// unusable is a record that no longer says what runs, as a step passed and
	// it couldn't be replaced, and couldn't be removed either. Nothing reads it,
	// and no hello lists it, so the control plane sends it again.
	unusable bool
	// kept is when a step or the control plane last had it kept, so a look at
	// how it stands from before then is old news (Seen).
	kept time.Time
	// standing is how the agent last saw it stand, as its record says, so a look
	// that sees nothing new costs no read of the record.
	standing Standing
}

// Store is an agent's cache of desired state.
type Store struct {
	dir    string
	server string
	aead   cipher.AEAD
	cache  string
	logger *slog.Logger
	now    func() time.Time

	mu      sync.Mutex
	held    map[resource.Ref]entry
	damaged int
}

// errDamaged says a record doesn't open: it was changed, cut short, sealed with
// another key or for another server, or is of another format. Anything else that
// stops a read, such as a disk error, isn't damage.
var errDamaged = errors.New("damaged")

// Open opens the cache in dir, the agent's data folder, for server: it makes the
// key on first use, opens every record, and moves aside any that doesn't open,
// which it then acts on no more. A record it can't read now, as for a disk error,
// fails Open instead, as it may open later.
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
	clearTemps(dir, "."+KeyFile+".", time.Now())
	clearTemps(folder, ".", time.Now())
	key, err := loadKey(dir, logger)
	if err != nil {
		return nil, err
	}
	defer clear(key)
	block, err := aes.NewCipher(key)
	if err != nil {
		return nil, err
	}
	aead, err := cipher.NewGCM(block)
	if err != nil {
		return nil, err
	}
	s := &Store{dir: folder, server: server, aead: aead, cache: cacheID(key), logger: logger, now: time.Now, held: map[resource.Ref]entry{}}
	if err := s.load(); err != nil {
		return nil, err
	}
	return s, nil
}

// cacheID names this cache, for the control plane to tell a new one: it is
// derived from the key, so it changes when the key does and says nothing of it.
func cacheID(key []byte) string {
	mac := hmac.New(sha256.New, key)
	mac.Write([]byte("dockwell.desired.cache.v1"))
	return hex.EncodeToString(mac.Sum(nil)[:8])
}

// leftoverAge is how old a temporary file is before it's taken for what a power
// cut left. A younger one may be another start's, writing its key now: clearing
// it made that start's link fail, and three in a row left it with no key.
const leftoverAge = time.Minute

// clearTemps removes what a power cut left of a write in dir: the temporary
// files, which start with prefix, that never took the place of what they wrote,
// and are at least leftoverAge old at now. A younger one goes at a later start.
func clearTemps(dir, prefix string, now time.Time) {
	entries, err := os.ReadDir(dir)
	if err != nil {
		return
	}
	for _, e := range entries {
		if e.IsDir() || !strings.HasPrefix(e.Name(), prefix) {
			continue
		}
		if info, err := e.Info(); err == nil && now.Sub(info.ModTime()) >= leftoverAge {
			_ = os.Remove(filepath.Join(dir, e.Name()))
		}
	}
}

// loadKey reads the key, or makes it if there's none. A key of the wrong length
// is put aside and a new one made, which leaves every record unreadable: they
// go aside too, and the control plane fills the cache again. Two agents that
// start on one folder at once agree on one key: the second to make one reads
// the first's.
func loadKey(dir string, logger *slog.Logger) ([]byte, error) {
	path := filepath.Join(dir, KeyFile)
	for range 3 {
		b, err := os.ReadFile(path) //nolint:gosec // the key's place in the agent's own folder
		switch {
		case err == nil && len(b) == keySize:
			if info, err := os.Stat(path); err == nil && info.Mode().Perm() != 0o600 {
				if err := os.Chmod(path, 0o600); err != nil {
					return nil, fmt.Errorf("the key to this server's desired state, %s, is %v, and can't be made the agent's alone: %w", path, info.Mode().Perm(), err)
				}
				logger.Warn("the key to this server's desired state could be read by others, so the agent made it its own alone again",
					slog.String("key", path), slog.String("was", info.Mode().Perm().String()))
			}
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
		// Written beside it, then linked into place, which fails if another
		// start made one first. That one is the key then.
		tmp, err := writeTemp(dir, "."+KeyFile+".", key)
		if err != nil {
			return nil, fmt.Errorf("can't keep the key to this server's desired state in %s: %w", dir, err)
		}
		err = os.Link(tmp, path)
		_ = os.Remove(tmp)
		switch {
		case err == nil:
			return key, syncDir(dir)
		case !errors.Is(err, fs.ErrExist) && !errors.Is(err, fs.ErrNotExist):
			// Another start made the key first, or something took this one's
			// temporary file away: either way, read again.
			return nil, fmt.Errorf("can't keep the key to this server's desired state in %s: %w", path, err)
		}
	}
	return nil, fmt.Errorf("can't settle on a key to this server's desired state in %s, as something keeps changing it", path)
}

// load opens every record, keeping its resource and revision, moves aside those
// that don't open, and removes tombstones past their life.
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
		r, in, err := s.read(path)
		if err == nil && fileName(r.Ref) != name {
			err = fmt.Errorf("%w: its name isn't its resource's", errDamaged)
		}
		switch {
		case errors.Is(err, errDamaged):
			s.putAside(path, err)
			continue
		case err != nil:
			return fmt.Errorf("can't read %s, a record of this server's desired state: %w. Check the disk and the agent's data folder", path, err)
		}
		if in.Gone {
			at := time.Unix(in.At, 0)
			if s.now().Sub(at) > TombstoneLife {
				_ = os.Remove(path)
				continue
			}
			s.held[r.Ref] = entry{revision: r.Revision, gone: true, at: at}
			continue
		}
		s.held[r.Ref] = entry{revision: r.Revision, standing: Standing{Stopped: r.Stopped, Volumes: r.Volumes}}
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

// Keep keeps what a step that passed on this server left a resource as. A step
// says what runs, so it's kept whatever revision the cache held, which a control
// plane's store that went back can number lower. A nil spec leaves a tombstone,
// which holds back what the control plane sends of the resource (Remember). A
// revision of zero, as a step that acts on a resource as it is has, keeps the
// revision there. If it can't be kept, what was kept of the resource goes too,
// as it no longer says what runs, and the error says so.
func (s *Store) Keep(r Record) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if r.Revision == 0 {
		r.Revision = s.held[r.Ref].revision
	}
	s.carry(&r, !r.Started)
	if err := s.write(r, true); err != nil {
		return s.dropLocked(r.Ref, fmt.Errorf("can't keep %s as this server runs it now: %w", r.Ref, err))
	}
	return nil
}

// Remember keeps what the control plane says a resource on this server runs, and
// says whether it did. It doesn't over a later revision, as of a step that ended
// after its stream did, nor at or below a deletion's revision, so a fill that read
// what ran before a deletion can't bring it back.
func (s *Store) Remember(r Record) (bool, error) {
	if r.Revision <= 0 {
		return false, fmt.Errorf("there's no revision to keep %s at", r.Ref)
	}
	if r.Spec == nil {
		return false, fmt.Errorf("there's no spec to keep for %s", r.Ref)
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if was, ok := s.held[r.Ref]; ok && !was.unusable && (r.Revision < was.revision || was.gone && r.Revision <= was.revision) {
		return false, nil
	}
	s.carry(&r, true)
	if err := s.write(r, true); err != nil {
		return false, s.dropLocked(r.Ref, fmt.Errorf("can't keep %s as this server runs it: %w", r.Ref, err))
	}
	return true, nil
}

// carry has r take over what the agent saw of the resource as it last stood: its
// image's volumes, unless r says them, and, if stopped, whether it was stopped. A
// step or a fill says what should run, not how it stands, which only a look can.
func (s *Store) carry(r *Record, stopped bool) {
	if r.Spec == nil {
		return
	}
	if e, ok := s.held[r.Ref]; !ok || e.gone || e.unusable {
		return
	}
	was, _, err := s.read(filepath.Join(s.dir, fileName(r.Ref)))
	if err != nil {
		return
	}
	if stopped {
		r.Stopped = was.Stopped
	}
	if r.Volumes == nil {
		r.Volumes = was.Volumes
	}
}

// Forget removes the record of a resource the control plane says this server no
// longer runs, if it still holds it at revision, the one the control plane saw,
// and says whether it did. A step since then has the say.
func (s *Store) Forget(ref resource.Ref, revision int64) (bool, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	was, ok := s.held[ref]
	if !ok || was.gone || was.unusable || was.revision != revision {
		return false, nil
	}
	if err := os.Remove(filepath.Join(s.dir, fileName(ref))); err != nil && !errors.Is(err, fs.ErrNotExist) {
		return false, fmt.Errorf("can't forget %s, which this server no longer runs: %w", ref, err)
	}
	delete(s.held, ref)
	return true, syncDir(s.dir)
}

// Drop forgets what the cache holds of ref, as it no longer says what runs: a
// step passed, and what it left couldn't be kept.
func (s *Store) Drop(ref resource.Ref, why error) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.dropLocked(ref, why)
}

// dropLocked removes the record of ref, or, if it can't, marks it unusable, so
// nothing reads it and no hello lists it. It returns why, with what came of it.
func (s *Store) dropLocked(ref resource.Ref, why error) error {
	was, ok := s.held[ref]
	if !ok {
		return why
	}
	path := filepath.Join(s.dir, fileName(ref))
	err := os.Remove(path)
	if err == nil || errors.Is(err, fs.ErrNotExist) {
		delete(s.held, ref)
		_ = syncDir(s.dir)
		return fmt.Errorf("%w, so the agent forgot what it kept of it. Check that the agent's data folder is writable", why)
	}
	// A folder that can't take a removal may still let the record itself be
	// written: cut short, it doesn't open after a restart either, and goes aside.
	_ = os.Truncate(path, 0)
	was.unusable = true
	s.held[ref] = was
	return fmt.Errorf("%w, nor can what it kept of it be removed (%w), so the agent reads it no more. Check that the agent's data folder is writable", why, err)
}

// write seals r into its record, or a tombstone for a nil spec, and indexes it.
// kept says a step or the control plane had it kept, rather than a look.
func (s *Store) write(r Record, kept bool) error {
	in := sealed{
		ScopeName: r.ScopeName, Revision: r.Revision, Change: r.Change, Spec: r.Spec, Values: r.Values, Image: r.Image.ID, Digest: r.Image.Digest,
		Stopped: r.Stopped, Volumes: r.Volumes,
	}
	at := s.now()
	if r.Spec == nil {
		in = sealed{Revision: r.Revision, Gone: true, At: at.Unix()}
	} else if in.Spec == nil {
		in.Spec = []byte{}
	}
	b, err := s.seal(r.Ref, in)
	if err != nil {
		return err
	}
	if err := writeDurably(filepath.Join(s.dir, fileName(r.Ref)), b); err != nil {
		return err
	}
	e := entry{revision: r.Revision, kept: s.held[r.Ref].kept, standing: Standing{Stopped: r.Stopped, Volumes: r.Volumes}}
	if kept {
		e.kept = at
	}
	if r.Spec == nil {
		e.gone, e.at = true, time.Unix(at.Unix(), 0)
	}
	s.held[r.Ref] = e
	return nil
}

// Read opens the record of ref, or says there's none, as for a resource deleted
// or never kept, or one whose record no longer says what runs.
func (s *Store) Read(ref resource.Ref) (Record, bool, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if e, ok := s.held[ref]; !ok || e.gone || e.unusable {
		return Record{}, false, nil
	}
	r, _, err := s.read(filepath.Join(s.dir, fileName(ref)))
	if err != nil {
		return Record{}, false, err
	}
	if r.Ref != ref {
		return Record{}, false, fmt.Errorf("the record of %s holds %s", ref, r.Ref)
	}
	return r, true, nil
}

// Refs lists the resources the cache holds a record of, by kind, scope and name.
func (s *Store) Refs() []resource.Ref {
	s.mu.Lock()
	defer s.mu.Unlock()
	refs := make([]resource.Ref, 0, len(s.held))
	for ref, e := range s.held {
		if !e.gone && !e.unusable {
			refs = append(refs, ref)
		}
	}
	slices.SortFunc(refs, func(a, b resource.Ref) int { return strings.Compare(a.String(), b.String()) })
	return refs
}

// Seen notes how the agent saw ref stand, in a look at at, rewriting its record
// only when that changes. What isn't held, or no longer, is left alone, and so is
// what a step or the control plane had kept since the look, which is old news.
func (s *Store) Seen(ref resource.Ref, st Standing, at time.Time) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if e, ok := s.held[ref]; !ok || e.gone || e.unusable || e.kept.After(at) || e.standing.equal(st) {
		return nil
	}
	r, _, err := s.read(filepath.Join(s.dir, fileName(ref)))
	if err != nil || r.Stopped == st.Stopped && maps.Equal(r.Volumes, st.Volumes) {
		return err
	}
	r.Stopped, r.Volumes = st.Stopped, st.Volumes
	if err := s.write(r, false); err != nil {
		return s.dropLocked(ref, fmt.Errorf("can't note how %s stands: %w", ref, err))
	}
	return nil
}

// Stands says how the agent last saw ref stand, as its record says, and whether
// it holds a record of ref to say it.
func (s *Store) Stands(ref resource.Ref) (Standing, bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	e, ok := s.held[ref]
	if !ok || e.gone || e.unusable {
		return Standing{}, false
	}
	return e.standing, true
}

func (a Standing) equal(b Standing) bool {
	return a.Stopped == b.Stopped && maps.Equal(a.Volumes, b.Volumes)
}

// Wipe removes every record and the key, as for a server removed from the fleet,
// whose agent then keeps nothing of what it ran.
func (s *Store) Wipe() error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if err := os.RemoveAll(s.dir); err != nil {
		return fmt.Errorf("can't remove %s: %w", s.dir, err)
	}
	if err := os.Remove(filepath.Join(filepath.Dir(s.dir), KeyFile)); err != nil && !errors.Is(err, fs.ErrNotExist) {
		return fmt.Errorf("can't remove the key to this server's desired state: %w", err)
	}
	clear(s.held)
	return nil
}

// Holds says what the cache keeps, for the agent's hello: each resource it holds
// a record of, and not those deleted or unusable.
func (s *Store) Holds() *agentv1.DesiredHeld {
	s.mu.Lock()
	defer s.mu.Unlock()
	out := &agentv1.DesiredHeld{Damaged: uint32(min(s.damaged, 1<<31)), Cache: s.cache} //nolint:gosec // at most 1<<31
	refs := make([]resource.Ref, 0, len(s.held))
	for ref, e := range s.held {
		if !e.gone && !e.unusable {
			refs = append(refs, ref)
		}
	}
	slices.SortFunc(refs, func(a, b resource.Ref) int { return strings.Compare(a.String(), b.String()) })
	for _, ref := range refs {
		out.Held = append(out.Held, &agentv1.HeldResource{Resource: resourcepb.Ref(ref), Revision: s.held[ref].revision})
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

// seal writes a record: the magic, its resource in the clear, then its nonce and
// what it holds, sealed.
func (s *Store) seal(ref resource.Ref, in sealed) ([]byte, error) {
	payload, err := json.Marshal(in)
	if err != nil {
		return nil, err
	}
	defer clear(payload)
	var out bytes.Buffer
	out.Write(magic)
	for _, part := range []string{string(ref.Kind), ref.Scope, ref.Name} {
		out.Write(binary.AppendUvarint(nil, uint64(len(part))))
		out.WriteString(part)
	}
	nonce := make([]byte, s.aead.NonceSize())
	if _, err := rand.Read(nonce); err != nil {
		return nil, err
	}
	out.Write(nonce)
	out.Write(s.aead.Seal(nil, nonce, payload, s.additional(ref)))
	return out.Bytes(), nil
}

// maxRecord bounds what a record can be: a spec, and values of at most 64 KiB
// each, which no resource comes near.
const maxRecord = 16 << 20

// read opens the record at path. What doesn't open is errDamaged; anything else
// that stops it, such as a disk error, isn't.
func (s *Store) read(path string) (Record, sealed, error) {
	info, err := os.Stat(path)
	if err != nil {
		return Record{}, sealed{}, err
	}
	if info.Size() > maxRecord {
		return Record{}, sealed{}, fmt.Errorf("%w: it's %d bytes, more than a record can be", errDamaged, info.Size())
	}
	b, err := os.ReadFile(path) //nolint:gosec // a record in the agent's own folder, named by a hash
	if err != nil {
		return Record{}, sealed{}, err
	}
	rest, ok := bytes.CutPrefix(b, magic)
	if !ok {
		return Record{}, sealed{}, fmt.Errorf("%w: it isn't a record of this format", errDamaged)
	}
	var parts [3]string
	for i := range parts {
		n, size := binary.Uvarint(rest)
		if size <= 0 || n > uint64(len(rest)-size) { //nolint:gosec // size is at most len(rest), so this is never negative
			return Record{}, sealed{}, fmt.Errorf("%w: it ends before its resource does", errDamaged)
		}
		parts[i] = string(rest[size : size+int(n)]) //nolint:gosec // n fits in rest
		rest = rest[size+int(n):]                   //nolint:gosec // n fits in rest
	}
	ref := resource.Ref{Kind: resource.Kind(parts[0]), Scope: parts[1], Name: parts[2]}
	if len(rest) < s.aead.NonceSize() {
		return Record{}, sealed{}, fmt.Errorf("%w: it ends before its sealing does", errDamaged)
	}
	nonce, box := rest[:s.aead.NonceSize()], rest[s.aead.NonceSize():]
	payload, err := s.aead.Open(nil, nonce, box, s.additional(ref))
	if err != nil {
		return Record{}, sealed{}, fmt.Errorf("%w: its sealing doesn't open: it was changed, cut short, sealed with another key, or for another server", errDamaged)
	}
	defer clear(payload)
	var in sealed
	if err := json.Unmarshal(payload, &in); err != nil {
		return Record{}, sealed{}, fmt.Errorf("%w: what it holds doesn't read: %w", errDamaged, err)
	}
	r := Record{
		Ref: ref, ScopeName: in.ScopeName, Revision: in.Revision, Change: in.Change, Values: in.Values,
		Image: resource.Image{ID: in.Image, Digest: in.Digest}, Stopped: in.Stopped, Volumes: in.Volumes,
	}
	if !in.Gone {
		r.Spec = in.Spec
		if r.Spec == nil {
			r.Spec = []byte{}
		}
	}
	return r, in, nil
}

// writeTemp writes b to a new temporary file in dir whose name starts with
// prefix, synced and the agent's alone, and returns its path.
func writeTemp(dir, prefix string, b []byte) (string, error) {
	f, err := os.CreateTemp(dir, prefix+"*")
	if err != nil {
		return "", err
	}
	tmp := f.Name()
	if err := f.Chmod(0o600); err == nil {
		if _, err = f.Write(b); err == nil {
			err = f.Sync()
		}
		if cerr := f.Close(); err == nil {
			err = cerr
		}
		if err == nil {
			return tmp, nil
		}
	} else {
		_ = f.Close()
	}
	_ = os.Remove(tmp)
	return "", err
}

// writeDurably writes b to path, so a power cut leaves what was there or b: a
// temporary file beside it, synced, renamed over it, and the folder synced. The
// file is the agent's alone.
func writeDurably(path string, b []byte) error {
	dir := filepath.Dir(path)
	tmp, err := writeTemp(dir, "."+filepath.Base(path)+".", b)
	if err != nil {
		return err
	}
	if err := os.Rename(tmp, path); err != nil {
		_ = os.Remove(tmp)
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
