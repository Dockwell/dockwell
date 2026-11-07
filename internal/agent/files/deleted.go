// SPDX-License-Identifier: AGPL-3.0-only

package files

import (
	"cmp"
	"context"
	"crypto/rand"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"os"
	"path"
	"path/filepath"
	"regexp"
	"slices"
	"strings"
	"sync/atomic"
	"time"

	"golang.org/x/sys/unix"
	"google.golang.org/protobuf/types/known/timestamppb"

	agentv1 "github.com/dockwell/dockwell/gen/dockwell/agent/v1"
	dockwellv1 "github.com/dockwell/dockwell/gen/dockwell/v1"
)

// Recently deleted is a folder in Dockwell's data, where only root reaches and Files
// never walks, as the data directory is shut. A delete is a rename into it, so it's
// instant whatever the size, and a container that holds a file open keeps it. Each
// entry is a folder of its own, named by its ID: item is what was deleted, and
// about.json what it was. What's deleted for good waits here too, or beside where it
// was when it's on another filesystem, until it's removed in the background. See
// docs/adr/0045-a-file-manager-on-a-server.md.

const (
	// KeepDeleted is how long Recently deleted keeps what's deleted.
	KeepDeleted = 7 * 24 * time.Hour
	// maxDeleted is how many it keeps. Past that the oldest go first.
	maxDeleted = 1000
	// pruneEvery is how often what's kept long enough goes.
	pruneEvery = time.Hour
	// maxDepth is how many folders deep a removal goes.
	maxDepth = 256
	// asidePrefix names what's deleted for good beside where it was, while it's removed.
	asidePrefix = ".dockwell-deleting-"
	// removingPrefix names an entry of Recently deleted that's being removed.
	removingPrefix = ".removing-"
	itemName       = "item"
	aboutName      = "about.json"
)

// deletedID is what an ID in Recently deleted looks like: rand.Text's, so no ID can
// name a path of its own.
var deletedID = regexp.MustCompile(`^[A-Z2-7]{26}$`)

// about is what's said of an entry in Recently deleted.
type about struct {
	Path    string              `json:"path"`
	Type    dockwellv1.FileType `json:"type"`
	Size    uint64              `json:"size,omitempty"`
	Deleted time.Time           `json:"deleted"`
	// ForGood is what's to be removed now. It isn't listed.
	ForGood bool `json:"for_good,omitempty"`
	// Aside is where what's deleted for good waits when it can't come here, as it's on
	// another filesystem, with Dev and Ino to say what it is.
	Aside string `json:"aside,omitempty"`
	Dev   uint64 `json:"dev,omitempty"`
	Ino   uint64 `json:"ino,omitempty"`
}

func (a *about) file(id string) *dockwellv1.DeletedFile {
	return &dockwellv1.DeletedFile{
		Id: id, Path: a.Path, Type: a.Type, SizeBytes: a.Size,
		DeleteTime: timestamppb.New(a.Deleted), PurgeTime: timestamppb.New(a.Deleted.Add(KeepDeleted)),
	}
}

// deletedDir is where Recently deleted is, or nothing on a server with no data
// directory.
func (s *Server) deletedDir() string {
	if s.dataDir == "" {
		return ""
	}
	return filepath.Join(s.dataDir, "agent", "deleted-files")
}

// openDeleted opens Recently deleted, making it where it isn't.
func (s *Server) openDeleted() (int, error) {
	dir := s.deletedDir()
	if dir == "" {
		return -1, errors.New("this agent has no data directory")
	}
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return -1, err
	}
	return unix.Open(dir, unix.O_PATH|unix.O_DIRECTORY|unix.O_NOFOLLOW|unix.O_CLOEXEC, 0)
}

// claim keeps an entry of Recently deleted for one call at a time, as putting it
// back and removing it can't both happen. It reports whether it got it.
func (s *Server) claim(id string) bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.claimed[id] {
		return false
	}
	s.claimed[id] = true
	return true
}

func (s *Server) unclaim(id string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	delete(s.claimed, id)
}

// kick has what's waiting removed now, rather than within the hour.
func (s *Server) kick() {
	select {
	case s.pruneNow <- struct{}{}:
	default:
	}
}

// keep moves what's at tg into Recently deleted, saying it's to be removed now when
// forGood is set. It returns nothing, and no refusal, where it can't, as it's on
// another filesystem or there's no data directory: nothing changed then.
func (s *Server) keep(tg *target, st *unix.Stat_t, forGood bool) (*dockwellv1.DeletedFile, *agentv1.FileRefusal) {
	root, err := s.openDeleted()
	if err != nil {
		s.logger.Warn("Recently deleted can't be opened, so what's deleted can't be put back", slog.String("error", err.Error()))
		return nil, nil
	}
	defer unix.Close(root)
	id := rand.Text()
	a := about{Path: tg.where, Type: typeOf(st.Mode), Deleted: s.now().UTC(), ForGood: forGood}
	if a.Type == dockwellv1.FileType_FILE_TYPE_FILE {
		a.Size = uint64(max(st.Size, 0))
	}
	entryDir, refusal := newEntry(root, id, &a, tg.where)
	if refusal != nil {
		return nil, refusal
	}
	defer unix.Close(entryDir)
	dropEntry := func() { _ = removeAll(root, id, 0, 0) }
	if err := unix.Renameat2(tg.dir(), tg.name, entryDir, itemName, unix.RENAME_NOREPLACE); err != nil {
		dropEntry()
		if errors.Is(err, unix.EXDEV) {
			return nil, nil
		}
		return nil, undeleted(tg.where, err)
	}
	var moved unix.Stat_t
	if err := unix.Fstatat(entryDir, itemName, &moved, unix.AT_SYMLINK_NOFOLLOW); err != nil || identify(&moved) != identify(st) {
		if unix.Renameat2(entryDir, itemName, tg.dir(), tg.name, unix.RENAME_NOREPLACE) == nil {
			dropEntry()
		}
		return nil, gone(tg.where)
	}
	if forGood {
		s.kick()
	} else if n := s.countDeleted(root); n > maxDeleted {
		s.kick()
	}
	return a.file(id), nil
}

// newEntry makes an entry in Recently deleted that says what a, and returns it open.
func newEntry(root int, id string, a *about, where string) (int, *agentv1.FileRefusal) {
	cant := func(err error) *agentv1.FileRefusal {
		return &agentv1.FileRefusal{
			Kind: agentv1.FileRefusal_KIND_DENIED, Path: where,
			Message: fmt.Sprintf("the agent can't keep %s in Recently deleted: %v, so nothing was deleted. Check the free space on Dockwell's data directory", where, err),
		}
	}
	if err := unix.Mkdirat(root, id, 0o700); err != nil {
		return -1, cant(err)
	}
	dir, err := unix.Openat(root, id, unix.O_PATH|unix.O_DIRECTORY|unix.O_NOFOLLOW|unix.O_CLOEXEC, 0)
	if err == nil {
		err = writeAbout(dir, a)
	}
	if err != nil {
		if dir >= 0 {
			_ = unix.Close(dir)
		}
		_ = removeAll(root, id, 0, 0)
		return -1, cant(err)
	}
	return dir, nil
}

// writeAbout puts a in an entry, in place of what it said.
func writeAbout(dir int, a *about) error {
	text, err := json.Marshal(a)
	if err != nil {
		return err
	}
	const next = aboutName + ".next"
	fd, err := unix.Openat(dir, next, unix.O_WRONLY|unix.O_CREAT|unix.O_TRUNC|unix.O_NOFOLLOW|unix.O_CLOEXEC, 0o600)
	if err != nil {
		return err
	}
	f := os.NewFile(uintptr(fd), next)
	_, werr := f.Write(text)
	if werr == nil {
		werr = f.Sync()
	}
	if err := errors.Join(werr, f.Close()); err != nil {
		return err
	}
	return unix.Renameat(dir, next, dir, aboutName)
}

// readAbout reads what an entry says.
func readAbout(root int, id string) (*about, error) {
	fd, err := unix.Openat(root, path.Join(id, aboutName), unix.O_RDONLY|unix.O_NOFOLLOW|unix.O_CLOEXEC, 0)
	if err != nil {
		return nil, err
	}
	f := os.NewFile(uintptr(fd), aboutName)
	defer f.Close()
	text, err := io.ReadAll(io.LimitReader(f, 64<<10))
	if err != nil {
		return nil, err
	}
	var a about
	if err := json.Unmarshal(text, &a); err != nil {
		return nil, err
	}
	return &a, nil
}

// forGood deletes what's at tg for good. A folder goes out of the way at once, into
// Recently deleted or beside where it was, and is removed in the background, so its
// path is free however large it is, and nothing reads it half removed.
func (s *Server) forGood(tg *target, st *unix.Stat_t) *agentv1.FileRefusal {
	if st.Mode&unix.S_IFMT != unix.S_IFDIR {
		if err := unix.Unlinkat(tg.dir(), tg.name, 0); err != nil {
			return undeleted(tg.where, err)
		}
		return nil
	}
	kept, refusal := s.keep(tg, st, true)
	if refusal != nil || kept != nil {
		return refusal
	}
	// On another filesystem: beside where it was, with a note to finish the job if the
	// agent stops first.
	id := rand.Text()
	aside := asidePrefix + id
	if err := unix.Renameat2(tg.dir(), tg.name, tg.dir(), aside, unix.RENAME_NOREPLACE); err != nil {
		return undeleted(tg.where, err)
	}
	var moved unix.Stat_t
	if err := unix.Fstatat(tg.dir(), aside, &moved, unix.AT_SYMLINK_NOFOLLOW); err != nil || identify(&moved) != identify(st) {
		_ = unix.Renameat2(tg.dir(), aside, tg.dir(), tg.name, unix.RENAME_NOREPLACE)
		return gone(tg.where)
	}
	if root, err := s.openDeleted(); err == nil {
		a := about{Path: tg.where, Type: typeOf(st.Mode), Deleted: s.now().UTC(), ForGood: true, Aside: joined(tg.w.path(), aside), Dev: st.Dev, Ino: st.Ino}
		entryDir, refusal := newEntry(root, id, &a, tg.where)
		_ = unix.Close(root)
		if refusal == nil {
			_ = unix.Close(entryDir)
			s.kick()
			return nil
		}
	}
	// With nowhere to note it, it goes now.
	if err := removeAll(tg.dir(), aside, st.Dev, 0); err != nil {
		s.logger.Warn("a folder deleted for good was left in part", slog.String("path", joined(tg.w.path(), aside)), slog.String("error", err.Error()))
	}
	return nil
}

// removeAll removes name in dir, and all it holds, through descriptors and never by
// path, so a link in it is removed, not followed. It stays on the filesystem dev,
// leaving what's mounted in it, and goes maxDepth folders deep at most. A dev of 0
// is the filesystem it's on. What it can't remove stays, and it says what.
func removeAll(dir int, name string, dev uint64, depth int) error {
	err := unix.Unlinkat(dir, name, 0)
	if err == nil || errors.Is(err, unix.ENOENT) {
		return nil
	}
	if !errors.Is(err, unix.EISDIR) && !errors.Is(err, unix.EPERM) {
		return fmt.Errorf("%s: %w", name, err)
	}
	if depth >= maxDepth {
		return fmt.Errorf("%s is more than %d folders deep, so what's under it was left", name, maxDepth)
	}
	fd, err := unix.Openat(dir, name, unix.O_RDONLY|unix.O_DIRECTORY|unix.O_NOFOLLOW|unix.O_CLOEXEC, 0)
	if err != nil {
		return fmt.Errorf("%s: %w", name, err)
	}
	var st unix.Stat_t
	if err := unix.Fstat(fd, &st); err != nil {
		_ = unix.Close(fd)
		return fmt.Errorf("%s: %w", name, err)
	}
	if dev == 0 {
		dev = st.Dev
	}
	if st.Dev != dev {
		_ = unix.Close(fd)
		return fmt.Errorf("%s is another filesystem mounted there, so it was left", name)
	}
	f := os.NewFile(uintptr(fd), name)
	var errs []error
	// A pass can miss what moves as the folder is read, so a few go over it again.
	for range 3 {
		if _, err := f.Seek(0, io.SeekStart); err != nil {
			errs = append(errs, err)
			break
		}
		for {
			names, err := f.Readdirnames(256)
			for _, n := range names {
				if err := removeAll(fd, n, dev, depth+1); err != nil && len(errs) < 8 {
					errs = append(errs, fmt.Errorf("%s/%w", name, err))
				}
			}
			if err != nil {
				break
			}
		}
		if err := unix.Unlinkat(dir, name, unix.AT_REMOVEDIR); err == nil || errors.Is(err, unix.ENOENT) {
			_ = f.Close()
			return nil
		} else if !errors.Is(err, unix.ENOTEMPTY) {
			errs = append(errs, fmt.Errorf("%s: %w", name, err))
			break
		}
	}
	_ = f.Close()
	if len(errs) == 0 {
		errs = append(errs, fmt.Errorf("%s isn't empty", name))
	}
	return errors.Join(errs...)
}

// ListDeleted says what's in Recently deleted, newest first.
func (s *Server) ListDeleted() (*agentv1.ListDeletedResult, error) {
	out := &dockwellv1.ListDeletedFilesResponse{}
	root, err := s.openDeleted()
	if err != nil {
		return &agentv1.ListDeletedResult{Listing: out}, nil // with none, nothing is in it
	}
	defer unix.Close(root)
	for _, e := range s.entries(root) {
		if !e.about.ForGood {
			out.Deleted = append(out.Deleted, e.about.file(e.id))
		}
	}
	return &agentv1.ListDeletedResult{Listing: out}, nil
}

// kept is an entry of Recently deleted.
type kept struct {
	id    string
	about *about
}

// entries reads Recently deleted, newest first. What doesn't say what it is goes
// last, as the oldest.
func (s *Server) entries(root int) []kept {
	dir, err := os.Open(s.deletedDir())
	if err != nil {
		return nil
	}
	names, _ := dir.Readdirnames(-1)
	_ = dir.Close()
	var out []kept
	for _, name := range names {
		if !deletedID.MatchString(name) {
			continue
		}
		a, err := readAbout(root, name)
		if err != nil {
			a = &about{}
		}
		out = append(out, kept{name, a})
	}
	slices.SortFunc(out, func(a, b kept) int {
		return cmp.Or(b.about.Deleted.Compare(a.about.Deleted), strings.Compare(a.id, b.id))
	})
	return out
}

func (s *Server) countDeleted(root int) int { return len(s.entries(root)) }

// Restore puts back what was deleted, at the path it was deleted from, as that is
// now: never over what's there. confirmed is the path a passkey confirmed, if any,
// where that's a way back in.
func (s *Server) Restore(ctx context.Context, id, confirmed string) (*agentv1.RestoreFileResult, error) {
	if !deletedID.MatchString(id) {
		return nil, fmt.Errorf("%q isn't an ID in Recently deleted. List them with ListDeletedFiles", id)
	}
	var began atomic.Bool
	return attempt(ctx, s, "/", func(ctx context.Context, t *trail) (*agentv1.RestoreFileResult, error) {
		return s.restore(ctx, t, id, confirmed, &began), nil
	}, func(r *agentv1.FileRefusal) *agentv1.RestoreFileResult {
		return &agentv1.RestoreFileResult{Refusal: leftBehind(r, &began, "putting it back")}
	})
}

func (s *Server) restore(ctx context.Context, t *trail, id, confirmed string, began *atomic.Bool) *agentv1.RestoreFileResult {
	refuse := func(r *agentv1.FileRefusal) *agentv1.RestoreFileResult { return &agentv1.RestoreFileResult{Refusal: r} }
	if !s.claim(id) {
		return refuse(busyDeleted(id))
	}
	defer s.unclaim(id)
	root, err := s.openDeleted()
	if err != nil {
		return refuse(notDeleted(id))
	}
	defer unix.Close(root)
	a, err := readAbout(root, id)
	if err != nil || a.ForGood {
		return refuse(notDeleted(id))
	}
	entryDir, err := unix.Openat(root, id, unix.O_PATH|unix.O_DIRECTORY|unix.O_NOFOLLOW|unix.O_CLOEXEC, 0)
	if err != nil {
		return refuse(notDeleted(id))
	}
	defer unix.Close(entryDir)
	var st unix.Stat_t
	if err := unix.Fstatat(entryDir, itemName, &st, unix.AT_SYMLINK_NOFOLLOW); err != nil {
		return refuse(notDeleted(id))
	}
	tg, refusal := s.targetOf(t, s.guard(ctx), a.Path)
	if refusal != nil {
		return refuse(refusal)
	}
	defer tg.w.close()
	if refusal := t.touch(tg.where); refusal != nil {
		return refuse(refusal)
	}
	switch _, err := tg.stat(); {
	case err == nil:
		return refuse(exists(tg.where, "nothing was put back"))
	case !errors.Is(err, unix.ENOENT):
		return refuse(failed(tg.where, err))
	}
	if why := credentials(tg.where); why != "" {
		return refuse(refused(tg.where, why))
	}
	wayIn := s.wayInAt(tg, &st)
	if wayIn && confirmed != tg.where {
		return refuse(needsPasskey(tg.where, "putting it back"))
	}
	began.Store(true)
	if ctx.Err() != nil {
		return refuse(notAnswering(tg.where))
	}
	if err := unix.Renameat2(entryDir, itemName, tg.dir(), tg.name, unix.RENAME_NOREPLACE); err != nil {
		switch {
		case errors.Is(err, unix.EEXIST), errors.Is(err, unix.ENOTEMPTY):
			return refuse(exists(tg.where, "nothing was put back"))
		case errors.Is(err, unix.EXDEV):
			return refuse(&agentv1.FileRefusal{
				Kind: agentv1.FileRefusal_KIND_OTHER_DISK, Path: tg.where,
				Message: path.Dir(tg.where) + " is on another disk or mount now than when this was deleted, so it can't go back in one step, and nothing was put back. Move what's there over SSH first",
			})
		}
		return refuse(&agentv1.FileRefusal{
			Kind: agentv1.FileRefusal_KIND_DENIED, Path: tg.where,
			Message: fmt.Sprintf("the agent can't put %s back: %v, so it's still in Recently deleted. Check the folder's permissions, and how it's mounted", tg.where, err),
		})
	}
	_ = unix.Unlinkat(entryDir, aboutName, 0)
	if err := unix.Unlinkat(root, id, unix.AT_REMOVEDIR); err != nil {
		s.logger.Warn("put back what was deleted, and its entry in Recently deleted is left", slog.String("id", id), slog.String("error", err.Error()))
	}
	var back unix.Stat_t
	if err := unix.Fstatat(tg.dir(), tg.name, &back, unix.AT_SYMLINK_NOFOLLOW); err != nil {
		back = st
	}
	return &agentv1.RestoreFileResult{Restored: &dockwellv1.RestoreFileResponse{Path: tg.where, File: entry(tg.name, &back, owners{}), WayBackIn: wayIn}}
}

// Purge deletes for good what's in Recently deleted. It goes out of the list at once,
// and is removed in the background.
func (s *Server) Purge(id string) (*agentv1.PurgeDeletedResult, error) {
	if !deletedID.MatchString(id) {
		return nil, fmt.Errorf("%q isn't an ID in Recently deleted. List them with ListDeletedFiles", id)
	}
	if !s.claim(id) {
		return &agentv1.PurgeDeletedResult{Refusal: busyDeleted(id)}, nil
	}
	defer s.unclaim(id)
	root, err := s.openDeleted()
	if err != nil {
		return &agentv1.PurgeDeletedResult{Refusal: notDeleted(id)}, nil // with none, nothing is in it
	}
	defer unix.Close(root)
	a, err := readAbout(root, id)
	if err != nil || a.ForGood {
		return &agentv1.PurgeDeletedResult{Refusal: notDeleted(id)}, nil // what doesn't say what it is goes with the next pass
	}
	if err := unix.Renameat(root, id, root, removingPrefix+id); err != nil {
		return &agentv1.PurgeDeletedResult{Refusal: &agentv1.FileRefusal{
			Kind: agentv1.FileRefusal_KIND_DENIED, Path: a.Path,
			Message: fmt.Sprintf("the agent can't delete %s from Recently deleted: %v. Check Dockwell's data directory", a.Path, err),
		}}, nil
	}
	s.kick()
	return &agentv1.PurgeDeletedResult{Purged: a.file(id)}, nil
}

// notDeleted is an ID Recently deleted doesn't have.
func notDeleted(id string) *agentv1.FileRefusal {
	return &agentv1.FileRefusal{
		Kind:    agentv1.FileRefusal_KIND_NOT_FOUND,
		Message: fmt.Sprintf("there's nothing called %s in Recently deleted. It may have been put back, deleted for good, or kept its %s", id, "7 days"),
	}
}

// busyDeleted is an entry another call has.
func busyDeleted(id string) *agentv1.FileRefusal {
	return &agentv1.FileRefusal{
		Kind:    agentv1.FileRefusal_KIND_CHANGED,
		Message: id + " in Recently deleted is being put back or deleted just now. Look at the list again",
	}
}

// KeepDeleted keeps Recently deleted to its 7 days and its 1,000 entries, and removes
// what's deleted for good, now and as it's asked, until ctx ends.
func (s *Server) KeepDeleted(ctx context.Context) {
	every := time.NewTicker(pruneEvery)
	defer every.Stop()
	for {
		s.prune(ctx)
		select {
		case <-ctx.Done():
			return
		case <-every.C:
		case <-s.pruneNow:
		}
	}
}

// prune removes what's kept long enough, what's past the most kept, and what's
// deleted for good, with whatever a stop left half removed.
func (s *Server) prune(ctx context.Context) {
	if _, err := os.Stat(s.deletedDir()); s.deletedDir() == "" || err != nil {
		return
	}
	root, err := s.openDeleted()
	if err != nil {
		return
	}
	defer unix.Close(root)
	cutoff := s.now().Add(-KeepDeleted)
	for i, e := range s.entries(root) {
		if !e.about.ForGood && !e.about.Deleted.Before(cutoff) && i < maxDeleted {
			continue
		}
		if !s.claim(e.id) {
			continue
		}
		_ = unix.Renameat(root, e.id, root, removingPrefix+e.id)
		s.unclaim(e.id)
	}
	dir, err := os.Open(s.deletedDir())
	if err != nil {
		return
	}
	names, _ := dir.Readdirnames(-1)
	_ = dir.Close()
	for _, name := range names {
		if !strings.HasPrefix(name, removingPrefix) || ctx.Err() != nil {
			continue
		}
		if a, err := readAbout(root, name); err == nil && a.Aside != "" && !s.removeAside(ctx, a) {
			continue // its folder isn't there now: the next pass tries again
		}
		if err := removeAll(root, name, 0, 0); err != nil {
			s.logger.Warn("can't remove all of what was deleted from Recently deleted", slog.String("error", err.Error()))
		}
	}
}

// removeAside removes what's deleted for good where it waits beside where it was, if
// it's still what was deleted. It reports whether there's nothing more to do.
func (s *Server) removeAside(ctx context.Context, a *about) bool {
	dir, name := path.Split(a.Aside)
	if !strings.HasPrefix(name, asidePrefix) {
		return true
	}
	w, refusal := s.guard(ctx).walk(&trail{s: s}, dir)
	switch {
	case refusal.GetKind() == agentv1.FileRefusal_KIND_NOT_ANSWERING:
		return false
	case refusal != nil:
		return true
	}
	defer w.close()
	var st unix.Stat_t
	if err := unix.Fstatat(w.last().fd, name, &st, unix.AT_SYMLINK_NOFOLLOW); err != nil {
		return true
	}
	if st.Dev != a.Dev || st.Ino != a.Ino {
		s.logger.Warn("what was deleted for good isn't where it waited, so what's there now was left", slog.String("path", a.Aside))
		return true
	}
	if err := removeAll(w.last().fd, name, st.Dev, 0); err != nil {
		s.logger.Warn("can't remove all of a folder deleted for good", slog.String("path", a.Aside), slog.String("error", err.Error()))
	}
	return true
}
