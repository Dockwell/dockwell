// SPDX-License-Identifier: AGPL-3.0-only

package files

import (
	"bytes"
	"context"
	"crypto/rand"
	"errors"
	"fmt"
	"log/slog"
	"os"
	"slices"
	"sync/atomic"

	"golang.org/x/sys/unix"

	agentv1 "github.com/dockwell/dockwell/gen/dockwell/agent/v1"
	dockwellv1 "github.com/dockwell/dockwell/gen/dockwell/v1"
)

// Save replaces the text of the file at p, if it is still the version the person
// read: the same text, in the same file, where their read ended. A file that
// changed since is left as it is, and they hear so. record is what the control plane
// calls this save, for what's said of it if it ends after nobody is waiting. confirmed
// is the path a passkey confirmed, if any: a way back onto the server takes one, for
// where the walk ends here, which a link swapped in since doesn't pass.
func (s *Server) Save(ctx context.Context, p, content, wanted, record, confirmed string) (*agentv1.SaveFileResult, error) {
	if err := CheckPath(p); err != nil {
		return nil, err
	}
	if len(content) > MaxFileBytes {
		return nil, fmt.Errorf("that text is %d bytes, and a file saves at %d at most. Connect to the server over SSH to write more", len(content), MaxFileBytes)
	}
	var began atomic.Bool
	return attempt(ctx, s, p, func(ctx context.Context, t *trail) (*agentv1.SaveFileResult, error) {
		out := s.save(ctx, t, p, content, wanted, confirmed, &began)
		t.back = func() {
			if began.Load() {
				s.saveEnded(record, p, out)
			}
		}
		return out, nil
	}, func(r *agentv1.FileRefusal) *agentv1.SaveFileResult {
		if began.Load() {
			r.Kind = agentv1.FileRefusal_KIND_MAY_LAND
			r.Message = fmt.Sprintf("%s stopped answering as the agent began to write it, so this save may still land when it answers. "+
				"Open it then, to see what's there", r.GetPath())
		} else {
			r.Message += ". Nothing was saved"
		}
		return &agentv1.SaveFileResult{Refusal: r}
	})
}

// SavesEnded says how each save that was left behind as it wrote came out, for the
// control plane to put on record. It waits for a stream, and holds as many as can be
// left behind.
func (s *Server) SavesEnded() <-chan *agentv1.SaveEnded { return s.savesEnded }

func (s *Server) saveEnded(record, p string, out *agentv1.SaveFileResult) {
	ended := &agentv1.SaveEnded{Record: record, Path: p}
	if saved := out.GetSaved(); saved != nil {
		ended.Path, ended.Landed, ended.WayBackIn = saved.GetPath(), true, saved.GetWayBackIn()
	} else if r := out.GetRefusal(); r != nil {
		ended.Path, ended.Reason = r.GetPath(), r.GetMessage()
	}
	select {
	case s.savesEnded <- ended:
	default:
		s.logger.Warn("a save that was left behind ended, and too many are waiting to be put on record, so this one won't be", slog.String("path", ended.GetPath()))
	}
}

func (s *Server) save(ctx context.Context, t *trail, p, content, wanted, confirmed string, began *atomic.Bool) *agentv1.SaveFileResult {
	w, refusal := s.guard(ctx).walk(t, p)
	if refusal != nil {
		return &agentv1.SaveFileResult{Refusal: refusal}
	}
	defer w.close()
	where := w.path()
	if w.st.Mode&unix.S_IFMT != unix.S_IFREG {
		return &agentv1.SaveFileResult{Refusal: &agentv1.FileRefusal{
			Kind: agentv1.FileRefusal_KIND_NOT_A_FILE, Path: where,
			Message: fmt.Sprintf("%s is %s, not a file, so there's nothing to save over", where, kindOf(&w.st)),
		}}
	}
	if why := credentials(where); why != "" {
		return &agentv1.SaveFileResult{Refusal: refused(where, why)}
	}
	now, truncated, refusal := readText(t, w)
	if refusal != nil {
		return &agentv1.SaveFileResult{Refusal: refusal}
	}
	// What can't be read here can't be saved over, version or no version: anyone
	// can work out the digest of a well-known binary, such as /usr/bin/sudo.
	if !truncated && isBinary(now) {
		return &agentv1.SaveFileResult{Refusal: &agentv1.FileRefusal{
			Kind: agentv1.FileRefusal_KIND_NOT_A_FILE, Path: where,
			Message: where + " isn't a text file, so it can't be saved here. Connect to the server over SSH to work with it",
		}}
	}
	if why := whatItHolds(now); why != "" {
		return &agentv1.SaveFileResult{Refusal: refused(where, why)}
	}
	if truncated || version(where, &w.st, now) != wanted {
		return &agentv1.SaveFileResult{Refusal: changed(where)}
	}
	// Asked last, so a passkey isn't asked for over what would be refused anyway.
	wayIn := s.wayIn(w)
	if wayIn && confirmed != where {
		return &agentv1.SaveFileResult{Refusal: &agentv1.FileRefusal{
			Kind: agentv1.FileRefusal_KIND_NEEDS_STEP_UP, Path: where,
			Message: where + " can let someone back onto this server, so saving it takes your passkey, and nothing was saved. Confirm with it, then try again",
		}}
	}
	text := keepMark(now, []byte(content))
	if refusal := s.putText(ctx, t, w, wanted, now, text, began); refusal != nil {
		return &agentv1.SaveFileResult{Refusal: refusal}
	}
	return &agentv1.SaveFileResult{Saved: &dockwellv1.SaveFileResponse{
		Path: where, File: entry(w.last().name, &w.st, owners{}), Version: version(where, &w.st, text), WayBackIn: wayIn,
	}}
}

// byteOrderMark is what some editors start a UTF-8 file with. A browser's decoder
// takes it off the text it reads, so what comes back to be saved doesn't have it.
var byteOrderMark = []byte{0xef, 0xbb, 0xbf}

// keepMark gives text the mark old starts with, which nobody could have seen to
// take out. The version a save returns has to be of these bytes.
func keepMark(old, text []byte) []byte {
	if bytes.HasPrefix(old, byteOrderMark) && !bytes.HasPrefix(text, byteOrderMark) {
		return append(slices.Clone(byteOrderMark), text...)
	}
	return text
}

// changed is a save that found the file other than as it was read.
func changed(where string) *agentv1.FileRefusal {
	return &agentv1.FileRefusal{
		Kind: agentv1.FileRefusal_KIND_CHANGED, Path: where,
		Message: where + " changed since you opened it, so nothing was saved. Open it again to see what's there",
	}
}

// Stand-ins for what's hard to come by in a test: a disk that fills up halfway, and
// one that stalls in a flush.
var (
	putIn     = overwrite
	syncSpare = (*os.File).Sync
)

// putText puts text in the walk's last part, in place: a rename would leave a
// container that mounts this one file reading the old one, as a mount holds the
// inode, and would break hard links. A reader can see the file half written for a
// moment, as with any editor that writes in place.
//
// A copy of the old text goes beside it first, as it's what can't be had again if
// the write fails partway and it can't go back. Flushing that copy is where a
// struggling disk stalls, so only after it does this read the file again and compare
// it: what took that long may have changed the file, and whoever asked may have gone.
// A save that outlived its caller writes nothing. Past began it can land, and
// whoever hears of it is told that (Save).
func (s *Server) putText(ctx context.Context, t *trail, w *walked, wanted string, old, text []byte, began *atomic.Bool) *agentv1.FileRefusal {
	where, parent, name := w.path(), w.steps[len(w.steps)-2].fd, w.last().name
	if refusal := t.touch(where); refusal != nil {
		return refusal
	}
	spare, refusal := writeSpare(parent, where, old)
	if refusal != nil {
		return refusal
	}
	keep := false
	defer func() {
		if !keep {
			_ = unix.Unlinkat(parent, spare, 0)
		}
	}()
	now, truncated, refusal := readText(t, w)
	switch {
	case refusal.GetKind() == agentv1.FileRefusal_KIND_NOT_FOUND:
		return changed(where) // it went, or something else took its place
	case refusal != nil:
		return refusal
	case truncated || version(where, &w.st, now) != wanted:
		return changed(where)
	}
	// In this order, so whatever gave up on it first either sees this has begun or
	// has ended ctx, and this stops.
	began.Store(true)
	if ctx.Err() != nil {
		return notAnswering(where)
	}
	if s.Writing != nil {
		s.Writing(where)
	}
	fd, err := unix.Openat(parent, name, unix.O_WRONLY|unix.O_NOFOLLOW|unix.O_NONBLOCK|unix.O_CLOEXEC, 0)
	if err != nil {
		return unwritten(where, err)
	}
	f := os.NewFile(uintptr(fd), where)
	defer f.Close()
	var st unix.Stat_t
	if err := unix.Fstat(fd, &st); err != nil {
		return unwritten(where, err)
	}
	if identify(&st) != identify(&w.st) {
		return changed(where) // it went between the check and here: it isn't what was read
	}
	if err := putIn(f, text); err != nil {
		if again := putIn(f, now); again != nil {
			keep = true
			return &agentv1.FileRefusal{
				Kind: agentv1.FileRefusal_KIND_DENIED, Path: where,
				Message: fmt.Sprintf("the agent couldn't finish writing %s: %v, and couldn't put it back: %v. "+
					"The file may be cut short. The text it had is beside it, as %s: put it back over SSH", where, err, again, spare),
			}
		}
		return unwritten(where, err)
	}
	if err := unix.Fstat(fd, &st); err != nil {
		return unwritten(where, err)
	}
	w.st = st
	return nil
}

// writeSpare writes text to a new file in the directory parent, for no one to
// read but root, and says what it called it. Its name holds none of the file's,
// which may be as long as a name can be, and starts with a dot, which what reads
// a directory of configs, such as cron and nginx's include, passes over.
func writeSpare(parent int, where string, text []byte) (string, *agentv1.FileRefusal) {
	name := ".dockwell-save-" + rand.Text()
	fd, err := unix.Openat(parent, name, unix.O_WRONLY|unix.O_CREAT|unix.O_EXCL|unix.O_NOFOLLOW|unix.O_CLOEXEC, 0o600)
	if err != nil {
		return "", unwritten(where, err)
	}
	f := os.NewFile(uintptr(fd), name)
	_, err = f.Write(text)
	if err == nil {
		err = syncSpare(f)
	}
	err = errors.Join(err, f.Close())
	if err != nil {
		_ = unix.Unlinkat(parent, name, 0)
		return "", unwritten(where, err)
	}
	return name, nil
}

// overwrite makes f hold text, and nothing after it.
func overwrite(f *os.File, text []byte) error {
	if _, err := f.WriteAt(text, 0); err != nil {
		return err
	}
	if err := f.Truncate(int64(len(text))); err != nil {
		return err
	}
	return f.Sync()
}

// unwritten is a save the server kept out, which left the file as it was.
func unwritten(where string, err error) *agentv1.FileRefusal {
	return &agentv1.FileRefusal{
		Kind: agentv1.FileRefusal_KIND_DENIED, Path: where,
		Message: fmt.Sprintf("the agent can't write %s: %v, so nothing was saved. Check the disk's free space, and how the file is mounted", where, err),
	}
}
