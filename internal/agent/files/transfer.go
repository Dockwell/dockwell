// SPDX-License-Identifier: AGPL-3.0-only

package files

import (
	"context"
	"crypto/rand"
	"crypto/subtle"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"math"
	"os"
	"path"
	"path/filepath"
	"strconv"
	"strings"
	"sync/atomic"
	"time"

	"golang.org/x/sys/unix"

	agentv1 "github.com/dockwell/dockwell/gen/dockwell/agent/v1"
	dockwellv1 "github.com/dockwell/dockwell/gen/dockwell/v1"
)

// Uploads and downloads (ADR 0045). An upload goes to a file of its own beside where
// it's going, named .dockwell-upload-<id>, which only root reads, as it arrives, a
// part at a time, from wherever the last part ended, so a dropped connection picks
// up where it stopped. Once all of it is there, it's put in place by a rename that
// never replaces, as a make is. A note in the data directory says what each upload
// is, and whose: the user and session that began it, as the control plane names
// them, and no one else's call reaches it. One not finished within a day goes. A download sends a file as it reads
// it, or a folder as a zip archive made as it goes, a part at a time, so the agent
// holds a part, never the file.

const (
	// MaxUploadBytes is the most a file uploaded can be: 16 GB, as the panel
	// counts it, in steps of 1024.
	MaxUploadBytes = 16 << 30
	// FrameBytes is the most a part of a file sent either way holds.
	FrameBytes = 128 << 10
	// keepUploads is how long an upload has to finish.
	keepUploads = 24 * time.Hour
	// uploadPrefix names an upload's file while it arrives.
	uploadPrefix = ".dockwell-upload-"
	// maxUnpacked is the most entries an archive unpacks to, and maxUnpackDepth how
	// deep its folders go.
	maxUnpacked    = 100_000
	maxUnpackDepth = 64
	// maxArchived is the most entries a folder's download holds.
	maxArchived = 200_000
)

// upload is what the agent keeps of an upload, in the data directory.
type upload struct {
	ID string `json:"id"`
	// Path is where it goes, with every link in its folder's path followed.
	Path   string            `json:"path"`
	Size   uint64            `json:"size"`
	Unpack dockwellv1.Unpack `json:"unpack,omitempty"`
	Began  time.Time         `json:"began"`
	// Confirmed is the path a passkey confirmed, where it's a way back in.
	Confirmed string `json:"confirmed,omitempty"`
	WayBackIn bool   `json:"way_back_in,omitempty"`
	// Dev and Ino are what its file is, beside Path.
	Dev uint64 `json:"dev"`
	Ino uint64 `json:"ino"`
	// Owner is who began it, which every later call has to be.
	Owner string `json:"owner"`
}

func (u *upload) part() string { return uploadPrefix + u.ID }

// arriving reports a name an upload's file has as it arrives. It's left out of a
// listing and a folder's download: it's the uploader's until it's in place, and its
// name holds the upload's ID.
func arriving(name string) bool {
	id, ok := strings.CutPrefix(name, uploadPrefix)
	return ok && deletedID().MatchString(id)
}

func (u *upload) proto(received uint64) *dockwellv1.Upload {
	return &dockwellv1.Upload{
		UploadId: u.ID, Path: u.Path, SizeBytes: u.Size, ReceivedBytes: received, MaxBytes: MaxUploadBytes, Unpack: u.Unpack, WayBackIn: u.WayBackIn,
	}
}

func (s *Server) uploadsDir() string {
	if s.dataDir == "" {
		return ""
	}
	return filepath.Join(s.dataDir, "agent", "uploads")
}

// loadUpload is the upload id, where owner began it. Anyone else's is as if it
// weren't there, so an ID seen somewhere is no way into another person's upload.
func (s *Server) loadUpload(id, owner string) (*upload, error) {
	u, err := s.readUpload(id)
	if err != nil {
		return nil, err
	}
	if owner == "" || subtle.ConstantTimeCompare([]byte(u.Owner), []byte(owner)) != 1 {
		return nil, os.ErrNotExist
	}
	return u, nil
}

// readUpload is the note on the upload id, whoever began it.
func (s *Server) readUpload(id string) (*upload, error) {
	if !deletedID().MatchString(id) || s.uploadsDir() == "" {
		return nil, os.ErrNotExist
	}
	text, err := os.ReadFile(filepath.Join(s.uploadsDir(), id+".json")) //nolint:gosec // an ID deletedID matched, in the agent's own folder
	if err != nil {
		return nil, err
	}
	var u upload
	if err := json.Unmarshal(text, &u); err != nil {
		return nil, err
	}
	if u.ID != id {
		return nil, os.ErrNotExist
	}
	return &u, nil
}

func (s *Server) saveUpload(u *upload) error {
	dir := s.uploadsDir()
	if dir == "" {
		return errors.New("this agent has no data directory to keep an upload's place in")
	}
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return err
	}
	text, err := json.Marshal(u)
	if err != nil {
		return err
	}
	next := filepath.Join(dir, u.ID+".json.next")
	if err := os.WriteFile(next, text, 0o600); err != nil {
		return err
	}
	return os.Rename(next, filepath.Join(dir, u.ID+".json"))
}

func (s *Server) dropUpload(id string) {
	if dir := s.uploadsDir(); dir != "" {
		_ = os.Remove(filepath.Join(dir, id+".json"))
	}
}

// errNoOwner is a call about an upload that doesn't say whose it is.
var errNoOwner = errors.New("the control plane didn't say who's uploading, so the agent can't keep it theirs. Update the control plane to this agent's version")

// uploadGone is an upload the agent doesn't have, or has for someone else.
func uploadGone(id string) *agentv1.FileRefusal {
	return &agentv1.FileRefusal{
		Kind:    agentv1.FileRefusal_KIND_UPLOAD_GONE,
		Message: fmt.Sprintf("there's no upload %s on this server. It may have finished, or gone after a day unfinished. Start it again", id),
	}
}

// BeginUpload starts an upload of size bytes to p for owner, or says how far owner's
// upload id got. confirmed is the path a passkey confirmed, if any, where p is a way
// back in.
func (s *Server) BeginUpload(ctx context.Context, p string, size uint64, unpack dockwellv1.Unpack, id, confirmed, owner string) (*agentv1.UploadResult, error) {
	if owner == "" {
		return nil, errNoOwner
	}
	if id != "" {
		return s.uploadAsItStands(ctx, id, owner), nil
	}
	if err := CheckPath(p); err != nil {
		return nil, err
	}
	if err := CheckName(p); err != nil {
		return nil, err
	}
	if size > MaxUploadBytes {
		return &agentv1.UploadResult{Refusal: &agentv1.FileRefusal{
			Kind: agentv1.FileRefusal_KIND_TOO_LARGE, Path: p,
			Message: fmt.Sprintf("that file is %s, and an upload can be %s at most. Copy it over SSH", sizeOf(size), sizeOf(MaxUploadBytes)),
		}}, nil
	}
	return attempt(ctx, s, p, func(ctx context.Context, t *trail) (*agentv1.UploadResult, error) {
		return s.beginUpload(ctx, t, p, size, unpack, confirmed, owner), nil
	}, func(r *agentv1.FileRefusal) *agentv1.UploadResult {
		r.Message += ". Nothing was uploaded"
		return &agentv1.UploadResult{Refusal: r}
	})
}

func (s *Server) beginUpload(ctx context.Context, t *trail, p string, size uint64, unpack dockwellv1.Unpack, confirmed, owner string) *agentv1.UploadResult {
	refuse := func(r *agentv1.FileRefusal) *agentv1.UploadResult { return &agentv1.UploadResult{Refusal: r} }
	tg, refusal := s.targetOf(t, s.guard(ctx), p)
	if refusal != nil {
		return refuse(refusal)
	}
	defer tg.w.close()
	if refusal := t.touch(tg.where); refusal != nil {
		return refuse(refusal)
	}
	switch _, err := tg.stat(); {
	case err == nil:
		return refuse(exists(tg.where, "nothing was uploaded"))
	case !errors.Is(err, unix.ENOENT):
		return refuse(failed(tg.where, err))
	}
	if why := credentials(tg.where); why != "" {
		return refuse(refused(tg.where, why))
	}
	wayIn := s.wayInAt(tg, nil, false)
	if wayIn && confirmed != tg.where {
		return refuse(needsPasskey(tg.where, "uploading it"))
	}
	var fs unix.Statfs_t
	if err := unix.Fstatfs(tg.dir(), &fs); err == nil {
		if free := fs.Bavail * uint64(fs.Bsize); free < size { //nolint:gosec // a block size is positive
			return refuse(&agentv1.FileRefusal{
				Kind: agentv1.FileRefusal_KIND_TOO_LARGE, Path: tg.where,
				Message: fmt.Sprintf("that file is %s, and the disk %s is on has %s free, so nothing was uploaded. Free some space first", sizeOf(size), path.Dir(tg.where), sizeOf(free)),
			})
		}
	}
	u := &upload{ID: rand.Text(), Path: tg.where, Size: size, Unpack: unpack, Began: s.now().UTC(), WayBackIn: wayIn, Owner: owner}
	if wayIn {
		u.Confirmed = confirmed
	}
	fd, err := unix.Openat(tg.dir(), u.part(), unix.O_WRONLY|unix.O_CREAT|unix.O_EXCL|unix.O_NOFOLLOW|unix.O_CLOEXEC, 0o600)
	if err != nil {
		return refuse(&agentv1.FileRefusal{
			Kind: agentv1.FileRefusal_KIND_DENIED, Path: tg.where,
			Message: fmt.Sprintf("the agent can't start an upload to %s: %v, so nothing was uploaded. %s", tg.where, err, fixFor(err)),
		})
	}
	defer unix.Close(fd)
	var st unix.Stat_t
	if err := unix.Fstat(fd, &st); err != nil {
		_ = unix.Unlinkat(tg.dir(), u.part(), 0)
		return refuse(denied(tg.where, err))
	}
	u.Dev, u.Ino = st.Dev, st.Ino
	if err := s.saveUpload(u); err != nil {
		_ = unix.Unlinkat(tg.dir(), u.part(), 0)
		return refuse(&agentv1.FileRefusal{
			Kind: agentv1.FileRefusal_KIND_DENIED, Path: tg.where,
			Message: fmt.Sprintf("the agent can't note an upload: %v, so nothing was uploaded. Check the free space on Dockwell's data directory", err),
		})
	}
	return &agentv1.UploadResult{Upload: u.proto(0)}
}

// openPart opens an upload's file, by walking to its folder again, and checks it's
// still what the upload made. The caller closes what it returns.
func (s *Server) openPart(ctx context.Context, t *trail, u *upload, flags int) (*target, int, *unix.Stat_t, *agentv1.FileRefusal) {
	tg, refusal := s.targetOf(t, s.guard(ctx), u.Path)
	if refusal != nil {
		return nil, -1, nil, refusal
	}
	if refusal := t.touch(tg.where); refusal != nil {
		tg.w.close()
		return nil, -1, nil, refusal
	}
	fd, err := unix.Openat(tg.dir(), u.part(), flags|unix.O_NOFOLLOW|unix.O_CLOEXEC, 0)
	if err != nil {
		tg.w.close()
		return nil, -1, nil, uploadGone(u.ID)
	}
	var st unix.Stat_t
	if err := unix.Fstat(fd, &st); err != nil || st.Dev != u.Dev || st.Ino != u.Ino {
		_ = unix.Close(fd)
		tg.w.close()
		return nil, -1, nil, &agentv1.FileRefusal{
			Kind: agentv1.FileRefusal_KIND_CHANGED, Path: u.Path,
			Message: "the upload to " + u.Path + " isn't where it was, or something else took its place. Start it again",
		}
	}
	return tg, fd, &st, nil
}

// uploadAsItStands says how far an upload got.
func (s *Server) uploadAsItStands(ctx context.Context, id, owner string) *agentv1.UploadResult {
	u, err := s.loadUpload(id, owner)
	if err != nil {
		return &agentv1.UploadResult{Refusal: uploadGone(id)}
	}
	out, err := attempt(ctx, s, u.Path, func(ctx context.Context, t *trail) (*agentv1.UploadResult, error) {
		tg, fd, st, refusal := s.openPart(ctx, t, u, unix.O_PATH)
		if refusal != nil {
			return &agentv1.UploadResult{Refusal: refusal}, nil
		}
		_ = unix.Close(fd)
		tg.w.close()
		return &agentv1.UploadResult{Upload: u.proto(uint64(max(st.Size, 0)))}, nil
	}, func(r *agentv1.FileRefusal) *agentv1.UploadResult { return &agentv1.UploadResult{Refusal: r} })
	if err != nil {
		return &agentv1.UploadResult{Refusal: denied(u.Path, err)}
	}
	return out
}

// WriteUpload writes what next gives to owner's upload id, from offset, which has to
// be how far it got, until next says there's no more. A part at a time is all it
// holds.
func (s *Server) WriteUpload(ctx context.Context, id, owner string, offset uint64, next func() ([]byte, error)) (*agentv1.UploadResult, error) {
	u, err := s.loadUpload(id, owner)
	if err != nil {
		return &agentv1.UploadResult{Refusal: uploadGone(id)}, nil
	}
	if !s.claim("upload:" + id) {
		return &agentv1.UploadResult{Refusal: &agentv1.FileRefusal{
			Kind: agentv1.FileRefusal_KIND_OFFSET, Path: u.Path,
			Message: "another write to the upload to " + u.Path + " is under way, so nothing was written. Wait for it, or start that one again",
		}}, nil
	}
	defer s.unclaim("upload:" + id)
	// Opening it can wait on a path that stopped answering; what's written after
	// goes as fast as the parts come.
	var tg *target
	var fd int
	opened, err := attempt(ctx, s, u.Path, func(ctx context.Context, t *trail) (*agentv1.UploadResult, error) {
		var st *unix.Stat_t
		var refusal *agentv1.FileRefusal
		tg, fd, st, refusal = s.openPart(ctx, t, u, unix.O_WRONLY)
		if refusal != nil {
			return &agentv1.UploadResult{Refusal: refusal}, nil
		}
		if got := uint64(max(st.Size, 0)); got != offset {
			_ = unix.Close(fd)
			tg.w.close()
			return &agentv1.UploadResult{Upload: u.proto(got), Refusal: &agentv1.FileRefusal{
				Kind: agentv1.FileRefusal_KIND_OFFSET, Path: u.Path,
				Message: fmt.Sprintf("the upload to %s got to %d bytes, and this write starts at %d, so nothing was written. Send from %d", u.Path, got, offset, got),
			}}, nil
		}
		return nil, nil
	}, func(r *agentv1.FileRefusal) *agentv1.UploadResult { return &agentv1.UploadResult{Refusal: r} })
	switch {
	case err != nil:
		return nil, err
	case opened != nil:
		return opened, nil
	}
	defer tg.w.close()
	defer unix.Close(fd)
	at := offset
	for {
		data, err := next()
		if errors.Is(err, io.EOF) {
			break
		}
		if err != nil {
			// The connection dropped: what's there is kept, to pick up from.
			return &agentv1.UploadResult{Upload: u.proto(at)}, nil
		}
		if at+uint64(len(data)) > u.Size {
			return &agentv1.UploadResult{Upload: u.proto(at), Refusal: &agentv1.FileRefusal{
				Kind: agentv1.FileRefusal_KIND_TOO_LARGE, Path: u.Path,
				Message: fmt.Sprintf("more came than the %s the upload to %s said it would be, so the rest wasn't written. Start it again with its size", sizeOf(u.Size), u.Path),
			}}, nil
		}
		for len(data) > 0 {
			n, err := unix.Pwrite(fd, data, int64(at)) //nolint:gosec // an upload is far below 2^63
			if errors.Is(err, unix.ENOSPC) || errors.Is(err, unix.EDQUOT) {
				return &agentv1.UploadResult{Upload: u.proto(at), Refusal: &agentv1.FileRefusal{
					Kind: agentv1.FileRefusal_KIND_NO_SPACE, Path: u.Path,
					Message: fmt.Sprintf("the disk %s is on is full, or its quota is, so the upload stopped at %s, which is kept. Free some space, then pick it up again", path.Dir(u.Path), sizeOf(at)),
				}}, nil
			}
			if err != nil {
				return &agentv1.UploadResult{Upload: u.proto(at), Refusal: &agentv1.FileRefusal{
					Kind: agentv1.FileRefusal_KIND_DENIED, Path: u.Path,
					Message: fmt.Sprintf("the agent can't write the upload to %s: %v, and kept the %d bytes it has. %s, then pick it up again", u.Path, err, at, fixFor(err)),
				}}, nil
			}
			data, at = data[n:], at+uint64(n) //nolint:gosec // n is never negative
		}
		if ctx.Err() != nil {
			break
		}
	}
	return &agentv1.UploadResult{Upload: u.proto(at)}, nil
}

// FinishUpload puts owner's upload id in place, once all of it is there, and unpacks
// it into a new folder beside it, if it was to be.
func (s *Server) FinishUpload(ctx context.Context, id, owner string) (*agentv1.FinishUploadResult, error) {
	u, err := s.loadUpload(id, owner)
	if err != nil {
		return &agentv1.FinishUploadResult{Refusal: uploadGone(id)}, nil
	}
	if !s.claim("upload:" + id) {
		return &agentv1.FinishUploadResult{Refusal: &agentv1.FileRefusal{
			Kind: agentv1.FileRefusal_KIND_OFFSET, Path: u.Path,
			Message: "a write to the upload to " + u.Path + " is under way. Finish it once that's done",
		}}, nil
	}
	defer s.unclaim("upload:" + id)
	var began atomic.Bool
	out, err := attempt(ctx, s, u.Path, func(ctx context.Context, t *trail) (*agentv1.FinishUploadResult, error) {
		return s.finishUpload(ctx, t, u, &began), nil
	}, func(r *agentv1.FileRefusal) *agentv1.FinishUploadResult {
		return &agentv1.FinishUploadResult{Refusal: leftBehind(r, &began, "putting it in place")}
	})
	if err != nil || out.GetRefusal() != nil || u.Unpack == dockwellv1.Unpack_UNPACK_UNSPECIFIED {
		return out, err
	}
	// Unpacking takes as long as the archive does, so it's on its own, and what it
	// used goes back once it's done, as a transfer's does.
	to, entries, refusal := s.unpack(ctx, out.GetFinished().GetPath(), u.Unpack)
	s.settleSoon()
	if refusal != nil {
		return &agentv1.FinishUploadResult{Finished: out.GetFinished(), Refusal: refusal}, nil
	}
	out.Finished.UnpackedTo, out.Finished.UnpackedEntries = to, entries
	return out, nil
}

func (s *Server) finishUpload(ctx context.Context, t *trail, u *upload, began *atomic.Bool) *agentv1.FinishUploadResult {
	refuse := func(r *agentv1.FileRefusal) *agentv1.FinishUploadResult {
		return &agentv1.FinishUploadResult{Refusal: r}
	}
	tg, fd, st, refusal := s.openPart(ctx, t, u, unix.O_RDONLY)
	if refusal != nil {
		return refuse(refusal)
	}
	defer tg.w.close()
	defer unix.Close(fd)
	if got := uint64(max(st.Size, 0)); got != u.Size {
		return refuse(&agentv1.FileRefusal{
			Kind: agentv1.FileRefusal_KIND_OFFSET, Path: u.Path,
			Message: fmt.Sprintf("%d of the %d bytes of %s are there, so it wasn't put in place. Send the rest from %d", got, u.Size, u.Path, got),
		})
	}
	if tg.where != u.Path {
		return refuse(&agentv1.FileRefusal{
			Kind: agentv1.FileRefusal_KIND_CHANGED, Path: u.Path,
			Message: fmt.Sprintf("%s leads to %s now, so the upload wasn't put in place. Start it again", u.Path, tg.where),
		})
	}
	switch _, err := tg.stat(); {
	case err == nil:
		return refuse(exists(tg.where, "the upload wasn't put in place, and is kept for a day to finish once that's moved"))
	case !errors.Is(err, unix.ENOENT):
		return refuse(failed(tg.where, err))
	}
	// What was a way back in when it began took a passkey for this path. What
	// became one since didn't, and a finish takes none, so it starts again.
	wayIn := s.wayInAt(tg, nil, false)
	if wayIn && u.Confirmed != tg.where {
		return refuse(&agentv1.FileRefusal{
			Kind: agentv1.FileRefusal_KIND_CHANGED, Path: tg.where,
			Message: tg.where + " can let someone back onto this server now, and didn't when the upload began, so it wasn't put in place. Start the upload again",
		})
	}
	began.Store(true)
	if ctx.Err() != nil {
		return refuse(notAnswering(tg.where))
	}
	if err := unix.Fsync(fd); err != nil {
		return refuse(&agentv1.FileRefusal{
			Kind: agentv1.FileRefusal_KIND_DENIED, Path: tg.where,
			Message: fmt.Sprintf("the agent can't flush the upload to %s: %v. %s", tg.where, err, fixFor(err)),
		})
	}
	ownLike(fd, &tg.w.st, dockwellv1.FileType_FILE_TYPE_FILE)
	if err := unix.Renameat2(tg.dir(), u.part(), tg.dir(), tg.name, unix.RENAME_NOREPLACE); err != nil {
		if errors.Is(err, unix.EEXIST) {
			return refuse(exists(tg.where, "the upload wasn't put in place"))
		}
		return refuse(&agentv1.FileRefusal{
			Kind: agentv1.FileRefusal_KIND_DENIED, Path: tg.where,
			Message: fmt.Sprintf("the agent can't put the upload in place at %s: %v. %s", tg.where, err, fixFor(err)),
		})
	}
	var placed unix.Stat_t
	if err := unix.Fstatat(tg.dir(), tg.name, &placed, unix.AT_SYMLINK_NOFOLLOW); err != nil || placed.Ino != u.Ino || placed.Dev != u.Dev {
		_ = unix.Renameat2(tg.dir(), tg.name, tg.dir(), u.part(), unix.RENAME_NOREPLACE)
		return refuse(gone(tg.where))
	}
	s.dropUpload(u.ID)
	return &agentv1.FinishUploadResult{Finished: &dockwellv1.FinishUploadResponse{
		Path: tg.where, File: entry(tg.name, &placed, owners{}), WayBackIn: wayIn,
	}}
}

// pruneUploads removes what was uploaded and not finished within a day, by what it
// is, so what took its place stays.
func (s *Server) pruneUploads(ctx context.Context) {
	dir := s.uploadsDir()
	if dir == "" {
		return
	}
	entries, err := os.ReadDir(dir)
	if err != nil {
		return
	}
	cutoff := s.now().Add(-keepUploads)
	for _, e := range entries {
		id, ok := strings.CutSuffix(e.Name(), ".json")
		if !ok || ctx.Err() != nil {
			continue
		}
		u, err := s.readUpload(id)
		if err != nil || !u.Began.Before(cutoff) || !s.claim("upload:"+id) {
			continue
		}
		// Its folder not answering now, the next pass tries again.
		if err := s.removeUpload(ctx, &trail{s: s}, u); err != nil {
			s.logger.Warn("can't remove an upload that didn't finish", slog.String("path", u.Path), slog.String("error", err.Error()))
		}
		s.unclaim("upload:" + id)
	}
}

// removeUpload removes what's there of u, by what it is, so what took its place
// stays, and its note. Where its folder doesn't answer, both stay, and it says why.
func (s *Server) removeUpload(ctx context.Context, t *trail, u *upload) error {
	w, refusal := s.guard(ctx).walk(t, path.Dir(u.Path))
	switch {
	case refusal.GetKind() == agentv1.FileRefusal_KIND_NOT_ANSWERING:
		return errors.New(refusal.GetMessage())
	case refusal == nil:
		err := removeAll(w.last().fd, u.part(), u.Dev, u.Ino, 0)
		w.close()
		s.dropUpload(u.ID)
		return err
	}
	// Its folder is gone, or isn't one: what was in it went with it.
	s.dropUpload(u.ID)
	return nil
}

// CancelUpload removes what's there of owner's upload id, and its note.
func (s *Server) CancelUpload(ctx context.Context, id, owner string) (*agentv1.FinishUploadResult, error) {
	u, err := s.loadUpload(id, owner)
	if err != nil {
		return &agentv1.FinishUploadResult{Refusal: uploadGone(id)}, nil
	}
	if !s.claim("upload:" + id) {
		return &agentv1.FinishUploadResult{Refusal: &agentv1.FileRefusal{
			Kind: agentv1.FileRefusal_KIND_OFFSET, Path: u.Path,
			Message: "a write to the upload to " + u.Path + " is under way, so it wasn't stopped. Stop it there, then try again",
		}}, nil
	}
	defer s.unclaim("upload:" + id)
	return attempt(ctx, s, u.Path, func(ctx context.Context, t *trail) (*agentv1.FinishUploadResult, error) {
		if err := s.removeUpload(ctx, t, u); err != nil {
			return &agentv1.FinishUploadResult{Refusal: &agentv1.FileRefusal{
				Kind: agentv1.FileRefusal_KIND_DENIED, Path: u.Path,
				Message: fmt.Sprintf("the agent can't remove the upload to %s: %v. It goes on its own a day after it began", u.Path, err),
			}}, nil
		}
		return &agentv1.FinishUploadResult{}, nil
	}, func(r *agentv1.FileRefusal) *agentv1.FinishUploadResult {
		return &agentv1.FinishUploadResult{Refusal: r}
	})
}

// sizeOf is a number of bytes as the panel and the CLI write it, in steps of 1024:
// "512 B", "2.1 GB", "16 GB".
func sizeOf(n uint64) string {
	units := []string{"B", "KB", "MB", "GB", "TB", "PB"}
	v, i := float64(n), 0
	for math.Round(v) >= 1024 && i < len(units)-1 {
		v /= 1024
		i++
	}
	digits := 0
	if i > 0 && v < 10 {
		digits = 1
	}
	return strings.TrimSuffix(strconv.FormatFloat(v, 'f', digits, 64), ".0") + " " + units[i]
}
