// SPDX-License-Identifier: AGPL-3.0-only

package files

import (
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"path"
	"slices"
	"strings"
	"sync/atomic"

	"golang.org/x/sys/unix"

	agentv1 "github.com/dockwell/dockwell/gen/dockwell/agent/v1"
	dockwellv1 "github.com/dockwell/dockwell/gen/dockwell/v1"
	"github.com/dockwell/dockwell/internal/mounts"
)

// What a person makes, moves and deletes on a server, for FileService (ADR 0045).
// Each walks to the folder a path is in as a read does, following every link on the
// way, and acts on the path's last part through that folder's descriptor, with the
// calls that don't follow it. So a link is moved or deleted as a link, and a link
// swapped in behind the walk changes nothing: the folder checked is the one written.

const (
	// MaxName is the longest name a part of a path can have, as the kernel's limit is.
	MaxName = 255
	// A new file and a new folder have these, whatever the agent's umask.
	madeFileMode = 0o644
	madeDirMode  = 0o755
)

// CheckName refuses a path whose last part isn't a name something can have: one
// that's empty, as / is, or longer than a server takes. CheckPath goes first.
func CheckName(p string) error {
	name := path.Base(strings.TrimRight(p, "/"))
	switch {
	case name == "/" || name == ".":
		return errors.New("that's the server's whole file system, which Dockwell doesn't make, move or delete. Name a file or a folder in it")
	case len(name) > MaxName:
		return fmt.Errorf("that name is %d bytes, and a server takes %d at most. Give it a shorter one", len(name), MaxName)
	}
	return nil
}

// target is the last part of a path, in the folder a walk opened, which stays open.
type target struct {
	w     *walked
	name  string
	where string // the folder's path, every link followed, and name, fit to show
}

func (tg *target) dir() int { return tg.w.last().fd }

// stat reads what's at tg, without following it.
func (tg *target) stat() (unix.Stat_t, error) {
	var st unix.Stat_t
	err := unix.Fstatat(tg.dir(), tg.name, &st, unix.AT_SYMLINK_NOFOLLOW)
	return st, err
}

// targetOf walks to the folder p is in, and checks what's in it can be changed:
// nothing in Docker's data but a volume's files. The caller closes what it returns.
func (s *Server) targetOf(t *trail, g *guard, p string) (*target, *agentv1.FileRefusal) {
	p = strings.TrimRight(p, "/")
	dir, name := path.Split(p)
	w, refusal := g.walk(t, dir)
	if refusal != nil {
		return nil, refusal
	}
	tg := &target{w: w, name: name, where: joined(w.path(), name)}
	if w.st.Mode&unix.S_IFMT != unix.S_IFDIR {
		folder := w.path()
		w.close()
		return nil, &agentv1.FileRefusal{
			Kind: agentv1.FileRefusal_KIND_NOT_A_DIRECTORY, Path: folder,
			Message: fmt.Sprintf("%s is %s, not a folder, so nothing can go in it", folder, kindOf(&w.st)),
		}
	}
	if at := w.last().place; at != free {
		w.close()
		why := whyVolumeOpts
		if at == dockerRoot {
			why = whyDocker
		}
		return nil, &agentv1.FileRefusal{
			Kind: agentv1.FileRefusal_KIND_REFUSED, Path: tg.where,
			Message: fmt.Sprintf("%s is in %s, so Dockwell doesn't change it. Change Docker's volumes from Dockwell's own screens", tg.where, why),
		}
	}
	return tg, nil
}

// changeable checks what's at tg is something these may move or delete: nothing
// that's refused, nothing that is or holds a mount, and nothing that holds what's
// refused.
func (s *Server) changeable(g *guard, tg *target, st *unix.Stat_t) *agentv1.FileRefusal {
	if _, why := g.enter(tg.w.last().place, tg.name, st); why != "" {
		return refused(tg.where, why)
	}
	if why := credentials(tg.where); why != "" {
		return refused(tg.where, why)
	}
	if st.Mode&unix.S_IFMT != unix.S_IFDIR {
		return nil
	}
	if at := s.mountIn(tg.where); at != "" {
		what := tg.where + " is where something is mounted"
		if at != tg.where {
			what = fmt.Sprintf("%s holds %s, where something is mounted", tg.where, at)
		}
		return &agentv1.FileRefusal{
			Kind: agentv1.FileRefusal_KIND_REFUSED, Path: tg.where,
			Message: what + ", so Dockwell doesn't move or delete it, as that would reach into what's mounted. Unmount it over SSH first",
		}
	}
	for _, shut := range g.held {
		if mounts.Holds(tg.where, shut.path) {
			return &agentv1.FileRefusal{
				Kind: agentv1.FileRefusal_KIND_REFUSED, Path: tg.where,
				Message: fmt.Sprintf("%s holds %s, which is %s, so Dockwell doesn't move or delete it", tg.where, shut.path, shut.why),
			}
		}
	}
	return nil
}

// mountIn is the first mount at where or under it, by what the kernel lists, or
// nothing.
func (s *Server) mountIn(where string) string {
	for line := range strings.Lines(s.mounts()) {
		fields := strings.Fields(line)
		if len(fields) < 5 {
			continue
		}
		if point := mounts.Unescape(fields[4]); mounts.Holds(where, point) {
			return point
		}
	}
	return ""
}

// wayInAt reports a write at tg that can let someone back onto the server, by the
// rule a save goes by: where it is, and each folder the walk went through, with a
// start file in an admin's home by what that home is. For what's there, st says
// what it is. A folder, dir, counts for every account's home it is or holds, as a
// home holds its owner's way in, and for each of an admin's way-in folders it is or
// holds, such as .ssh or their systemd units, wherever their home is.
func (s *Server) wayInAt(tg *target, st *unix.Stat_t, dir bool) bool {
	from, inVolume := 0, false
	for i := range tg.w.steps {
		if tg.w.steps[i].place == oneVolume {
			from, inVolume = i+1, true
		}
	}
	ways := s.waysIn()
	if ways.listed(tg.where, s.dataDir, inVolume) {
		return true
	}
	// Spelled as the rule knows it too, where the walk went through another name
	// for a home, as a bind mount gives it.
	written := ways.asWritten(tg.w.steps, "/"+tg.name)
	for _, as := range written {
		if ways.listed(as, s.dataDir, inVolume) {
			return true
		}
	}
	for i := from; i < len(tg.w.steps); i++ {
		if ways.dirs[tg.w.steps[i].id] {
			return true
		}
	}
	if slices.Contains(startFiles, tg.name) && ways.adminHomes[tg.w.last().id] {
		return true
	}
	if st != nil && ways.files[identify(st)] {
		return true
	}
	if !dir {
		return false
	}
	if st != nil && (ways.dirs[identify(st)] || ways.everyoneIDs[identify(st)]) {
		return true
	}
	holds := func(p string) bool {
		return mounts.Holds(tg.where, p) || slices.ContainsFunc(written, func(as string) bool { return mounts.Holds(as, p) })
	}
	return slices.ContainsFunc(ways.everyone, holds) || slices.ContainsFunc(ways.adminDirs, holds)
}

// isDir reports what st says is a folder.
func isDir(st *unix.Stat_t) bool { return st.Mode&unix.S_IFMT == unix.S_IFDIR }

// needsPasskey is a write that can let someone back onto the server, with no passkey
// for where it is.
func needsPasskey(where, doing string) *agentv1.FileRefusal {
	return &agentv1.FileRefusal{
		Kind: agentv1.FileRefusal_KIND_NEEDS_STEP_UP, Path: where,
		Message: where + " can let someone back onto this server, so " + doing + " takes your passkey, and nothing changed. Confirm with it, then try again",
	}
}

// exists is something at a path that a make, move or put back would take.
func exists(where, then string) *agentv1.FileRefusal {
	return &agentv1.FileRefusal{
		Kind: agentv1.FileRefusal_KIND_EXISTS, Path: where,
		Message: where + " is there already, so " + then + ". Choose another name, or move that first",
	}
}

// leftBehind says what a refusal comes to for a write left behind: once it began to
// act, it can still land when the path answers.
func leftBehind(r *agentv1.FileRefusal, began *atomic.Bool, doing string) *agentv1.FileRefusal {
	if began.Load() {
		r.Kind = agentv1.FileRefusal_KIND_MAY_LAND
		r.Message = fmt.Sprintf("%s stopped answering as the agent began %s, so it may still happen when it answers. Look again then", r.GetPath(), doing)
	} else {
		r.Message += ". Nothing changed"
	}
	return r
}

// Make makes an empty file or a folder at p, never over what's there. confirmed is
// the path a passkey confirmed, if any, where p is a way back in.
func (s *Server) Make(ctx context.Context, p string, kind dockwellv1.FileType, confirmed string) (*agentv1.MakeFileResult, error) {
	if err := CheckPath(p); err != nil {
		return nil, err
	}
	if err := CheckName(p); err != nil {
		return nil, err
	}
	if kind != dockwellv1.FileType_FILE_TYPE_FILE && kind != dockwellv1.FileType_FILE_TYPE_DIRECTORY {
		return nil, errors.New("say whether to make a file or a folder")
	}
	var began atomic.Bool
	return attempt(ctx, s, p, func(ctx context.Context, t *trail) (*agentv1.MakeFileResult, error) {
		return s.make(ctx, t, p, kind, confirmed, &began), nil
	}, func(r *agentv1.FileRefusal) *agentv1.MakeFileResult {
		return &agentv1.MakeFileResult{Refusal: leftBehind(r, &began, "making it")}
	})
}

func (s *Server) make(ctx context.Context, t *trail, p string, kind dockwellv1.FileType, confirmed string, began *atomic.Bool) *agentv1.MakeFileResult {
	refuse := func(r *agentv1.FileRefusal) *agentv1.MakeFileResult { return &agentv1.MakeFileResult{Refusal: r} }
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
		return refuse(exists(tg.where, "nothing was made"))
	case !errors.Is(err, unix.ENOENT):
		return refuse(failed(tg.where, err))
	}
	if why := credentials(tg.where); why != "" {
		return refuse(refused(tg.where, why))
	}
	wayIn := s.wayInAt(tg, nil, kind == dockwellv1.FileType_FILE_TYPE_DIRECTORY)
	if wayIn && confirmed != tg.where {
		return refuse(needsPasskey(tg.where, "making it"))
	}
	began.Store(true)
	if ctx.Err() != nil {
		return refuse(notAnswering(tg.where))
	}
	fd, err := makeAt(tg.dir(), tg.name, kind)
	if errors.Is(err, unix.EEXIST) {
		return refuse(exists(tg.where, "nothing was made"))
	}
	if err != nil {
		return refuse(&agentv1.FileRefusal{
			Kind: agentv1.FileRefusal_KIND_DENIED, Path: tg.where,
			Message: fmt.Sprintf("the agent can't make %s: %v. %s", tg.where, err, fixFor(err)),
		})
	}
	defer unix.Close(fd)
	var st unix.Stat_t
	if err := unix.Fstat(fd, &st); err != nil {
		return refuse(denied(tg.where, err))
	}
	// A folder is opened by its name after it's made, so another can be in its place
	// by then: only what's the agent's own, and empty, takes the folder's owner.
	if kind == dockwellv1.FileType_FILE_TYPE_DIRECTORY && (st.Uid != uint32(unix.Geteuid()) || !emptyDir(fd)) { //nolint:gosec // a user ID fits
		return refuse(&agentv1.FileRefusal{
			Kind: agentv1.FileRefusal_KIND_CHANGED, Path: tg.where,
			Message: tg.where + " changed as the agent made it, so its owner and mode were left as they are. Look at it again",
		})
	}
	ownLike(fd, &tg.w.st, kind)
	if err := unix.Fstat(fd, &st); err != nil {
		return refuse(denied(tg.where, err))
	}
	return &agentv1.MakeFileResult{Made: &dockwellv1.MakeFileResponse{Path: tg.where, File: entry(tg.name, &st, owners{}), WayBackIn: wayIn}}
}

// emptyDir reports a folder, open as fd, that holds nothing.
func emptyDir(fd int) bool {
	dup, err := unix.Dup(fd)
	if err != nil {
		return false
	}
	f := os.NewFile(uintptr(dup), "")
	defer f.Close()
	names, err := f.Readdirnames(1)
	return len(names) == 0 && errors.Is(err, io.EOF)
}

// fixFor is what to do about an error the kernel gave a write, by what it says.
func fixFor(err error) string {
	switch {
	case errors.Is(err, unix.EACCES), errors.Is(err, unix.EPERM):
		return "Check its permissions, and whether it's marked immutable with chattr"
	case errors.Is(err, unix.ENOSPC), errors.Is(err, unix.EDQUOT):
		return "Check the disk's free space, and the quota"
	case errors.Is(err, unix.EROFS):
		return "It's on a mount that's read-only. Mount it read-write to change it"
	}
	return "Check its permissions, and how it's mounted"
}

// makeAt makes name in dir, and returns it open.
func makeAt(dir int, name string, kind dockwellv1.FileType) (int, error) {
	if kind == dockwellv1.FileType_FILE_TYPE_FILE {
		return unix.Openat(dir, name, unix.O_WRONLY|unix.O_CREAT|unix.O_EXCL|unix.O_NOFOLLOW|unix.O_NONBLOCK|unix.O_CLOEXEC, madeFileMode)
	}
	if err := unix.Mkdirat(dir, name, madeDirMode); err != nil {
		return -1, err
	}
	return unix.Openat(dir, name, unix.O_RDONLY|unix.O_DIRECTORY|unix.O_NOFOLLOW|unix.O_CLOEXEC, 0)
}

// ownLike gives what fd is open on its folder's owner and group, as what's made in a
// site's folder is the site's, and its mode, whatever the umask took. A folder in one
// with the set-group-ID bit keeps it, as the kernel gave it.
func ownLike(fd int, folder *unix.Stat_t, kind dockwellv1.FileType) {
	if unix.Geteuid() == 0 {
		_ = unix.Fchown(fd, int(folder.Uid), int(folder.Gid))
	}
	mode := uint32(madeFileMode)
	if kind == dockwellv1.FileType_FILE_TYPE_DIRECTORY {
		mode = madeDirMode | folder.Mode&unix.S_ISGID
	}
	_ = unix.Fchmod(fd, mode)
}

// Move moves or renames what's at from to to, within one filesystem, never over
// what's there. confirmedFrom and confirmedTo are the paths a passkey confirmed, if
// any, where either end is a way back in.
func (s *Server) Move(ctx context.Context, from, to, confirmedFrom, confirmedTo string) (*agentv1.MoveFileResult, error) {
	for _, p := range []string{from, to} {
		if err := CheckPath(p); err != nil {
			return nil, err
		}
		if err := CheckName(p); err != nil {
			return nil, err
		}
	}
	var began atomic.Bool
	return attempt(ctx, s, from, func(ctx context.Context, t *trail) (*agentv1.MoveFileResult, error) {
		return s.move(ctx, t, from, to, confirmedFrom, confirmedTo, &began), nil
	}, func(r *agentv1.FileRefusal) *agentv1.MoveFileResult {
		return &agentv1.MoveFileResult{Refusal: leftBehind(r, &began, "moving it")}
	})
}

func (s *Server) move(ctx context.Context, t *trail, from, to, confirmedFrom, confirmedTo string, began *atomic.Bool) *agentv1.MoveFileResult {
	refuse := func(r *agentv1.FileRefusal) *agentv1.MoveFileResult { return &agentv1.MoveFileResult{Refusal: r} }
	g := s.guard(ctx)
	src, refusal := s.targetOf(t, g, from)
	if refusal != nil {
		return refuse(refusal)
	}
	defer src.w.close()
	dst, refusal := s.targetOf(t, g, to)
	if refusal != nil {
		return refuse(refusal)
	}
	defer dst.w.close()
	if refusal := t.touch(src.where); refusal != nil {
		return refuse(refusal)
	}
	st, err := src.stat()
	if err != nil {
		return refuse(failed(src.where, err))
	}
	if refusal := s.changeable(g, src, &st); refusal != nil {
		return refuse(refusal)
	}
	if why := credentials(dst.where); why != "" {
		return refuse(refused(dst.where, why))
	}
	if refusal := t.touch(dst.where); refusal != nil {
		return refuse(refusal)
	}
	switch _, err := dst.stat(); {
	case err == nil:
		return refuse(exists(dst.where, "nothing moved"))
	case !errors.Is(err, unix.ENOENT):
		return refuse(failed(dst.where, err))
	}
	wayIn := s.wayInAt(src, &st, isDir(&st)) || s.wayInAt(dst, &st, isDir(&st))
	if wayIn && (confirmedFrom != src.where || confirmedTo != dst.where) {
		r := needsPasskey(src.where, "moving it")
		r.Message = fmt.Sprintf("moving %s to %s can let someone back onto this server, so it takes your passkey, and nothing moved. Confirm with it, then try again", src.where, dst.where)
		return refuse(r)
	}
	began.Store(true)
	if ctx.Err() != nil {
		return refuse(notAnswering(src.where))
	}
	if err := unix.Renameat2(src.dir(), src.name, dst.dir(), dst.name, unix.RENAME_NOREPLACE); err != nil {
		return refuse(unmoved(src, dst, err))
	}
	var moved unix.Stat_t
	if err := unix.Fstatat(dst.dir(), dst.name, &moved, unix.AT_SYMLINK_NOFOLLOW); err != nil || identify(&moved) != identify(&st) {
		// Something took its place between the check and the move: put that back.
		_ = unix.Renameat2(dst.dir(), dst.name, src.dir(), src.name, unix.RENAME_NOREPLACE)
		return refuse(&agentv1.FileRefusal{
			Kind: agentv1.FileRefusal_KIND_CHANGED, Path: src.where,
			Message: src.where + " changed as the agent moved it, so what was there is back where it was. Look at it again",
		})
	}
	return &agentv1.MoveFileResult{Moved: &dockwellv1.MoveFileResponse{
		From: src.where, To: dst.where, File: entry(dst.name, &moved, owners{}), WayBackIn: wayIn,
	}}
}

// unmoved is a rename the kernel refused.
func unmoved(src, dst *target, err error) *agentv1.FileRefusal {
	switch {
	case errors.Is(err, unix.EEXIST), errors.Is(err, unix.ENOTEMPTY):
		return exists(dst.where, "nothing moved")
	case errors.Is(err, unix.EXDEV):
		return &agentv1.FileRefusal{
			Kind: agentv1.FileRefusal_KIND_OTHER_DISK, Path: src.where,
			Message: fmt.Sprintf("%s and %s are on different disks or mounts, so it can't move in one step, and nothing moved. Copy it over SSH for now", src.where, path.Dir(dst.where)),
		}
	case errors.Is(err, unix.EINVAL) && mounts.Holds(src.where, dst.where):
		return &agentv1.FileRefusal{
			Kind: agentv1.FileRefusal_KIND_DENIED, Path: src.where,
			Message: src.where + " can't go inside itself, so nothing moved. Choose a folder outside it",
		}
	case errors.Is(err, unix.EINVAL):
		return &agentv1.FileRefusal{
			Kind: agentv1.FileRefusal_KIND_DENIED, Path: src.where,
			Message: src.where + " is on a filesystem that can't move something without replacing what may be at its new path, so nothing moved. Move it over SSH",
		}
	case errors.Is(err, unix.EBUSY):
		return &agentv1.FileRefusal{
			Kind: agentv1.FileRefusal_KIND_REFUSED, Path: src.where,
			Message: src.where + " is in use as a mount, so nothing moved. Unmount it over SSH first",
		}
	case errors.Is(err, unix.ENOENT):
		return &agentv1.FileRefusal{
			Kind: agentv1.FileRefusal_KIND_CHANGED, Path: src.where,
			Message: src.where + " went as the agent moved it, so nothing moved. Look at the folder again",
		}
	}
	return &agentv1.FileRefusal{
		Kind: agentv1.FileRefusal_KIND_DENIED, Path: src.where,
		Message: fmt.Sprintf("the agent can't move %s to %s: %v, so nothing moved. %s", src.where, dst.where, err, fixFor(err)),
	}
}

// Delete deletes what's at p: to Recently deleted, where it's on the same filesystem
// as Dockwell's data, or for good. confirmed is the path a passkey confirmed, if
// any, where p is a way back in.
func (s *Server) Delete(ctx context.Context, p string, forGood bool, confirmed string) (*agentv1.DeleteFileResult, error) {
	if err := CheckPath(p); err != nil {
		return nil, err
	}
	if err := CheckName(p); err != nil {
		return nil, err
	}
	var began atomic.Bool
	return attempt(ctx, s, p, func(ctx context.Context, t *trail) (*agentv1.DeleteFileResult, error) {
		return s.delete(ctx, t, p, forGood, confirmed, &began), nil
	}, func(r *agentv1.FileRefusal) *agentv1.DeleteFileResult {
		return &agentv1.DeleteFileResult{Refusal: leftBehind(r, &began, "deleting it")}
	})
}

func (s *Server) delete(ctx context.Context, t *trail, p string, forGood bool, confirmed string, began *atomic.Bool) *agentv1.DeleteFileResult {
	refuse := func(r *agentv1.FileRefusal) *agentv1.DeleteFileResult { return &agentv1.DeleteFileResult{Refusal: r} }
	g := s.guard(ctx)
	tg, refusal := s.targetOf(t, g, p)
	if refusal != nil {
		return refuse(refusal)
	}
	defer tg.w.close()
	if refusal := t.touch(tg.where); refusal != nil {
		return refuse(refusal)
	}
	st, err := tg.stat()
	if err != nil {
		return refuse(failed(tg.where, err))
	}
	if refusal := s.changeable(g, tg, &st); refusal != nil {
		return refuse(refusal)
	}
	// Said before a passkey is asked for, so a delete that can't be put back takes
	// one passkey, for the delete that goes ahead.
	if !forGood && !s.canKeep(&st) {
		return refuse(cantKeep(tg.where))
	}
	wayIn := s.wayInAt(tg, &st, isDir(&st))
	if wayIn && confirmed != tg.where {
		return refuse(needsPasskey(tg.where, "deleting it"))
	}
	began.Store(true)
	if ctx.Err() != nil {
		return refuse(notAnswering(tg.where))
	}
	out := &dockwellv1.DeleteFileResponse{Path: tg.where, WayBackIn: wayIn}
	if !forGood {
		kept, refusal := s.keep(tg, &st, false)
		switch {
		case refusal != nil:
			return refuse(refusal)
		case kept == nil:
			return refuse(cantKeep(tg.where))
		}
		out.Deleted = kept
		return &agentv1.DeleteFileResult{Deleted: out}
	}
	if refusal := s.forGood(tg, &st); refusal != nil {
		return refuse(refusal)
	}
	return &agentv1.DeleteFileResult{Deleted: out}
}

// cantKeep is a delete Recently deleted can't take.
func cantKeep(where string) *agentv1.FileRefusal {
	return &agentv1.FileRefusal{
		Kind: agentv1.FileRefusal_KIND_FOR_GOOD, Path: where,
		Message: where + " is on another disk or mount than Dockwell's data, so a delete can't be put back, and nothing was deleted. Delete it for good to go on",
	}
}

// gone is a delete that found what it checked replaced, and put it back.
func gone(where string) *agentv1.FileRefusal {
	return &agentv1.FileRefusal{
		Kind: agentv1.FileRefusal_KIND_CHANGED, Path: where,
		Message: where + " changed as the agent deleted it, so what was there is back where it was. Look at it again",
	}
}

// undeleted is a delete the kernel refused.
func undeleted(where string, err error) *agentv1.FileRefusal {
	if errors.Is(err, unix.EBUSY) {
		return &agentv1.FileRefusal{
			Kind: agentv1.FileRefusal_KIND_REFUSED, Path: where,
			Message: where + " is in use as a mount, so nothing was deleted. Unmount it over SSH first",
		}
	}
	return &agentv1.FileRefusal{
		Kind: agentv1.FileRefusal_KIND_DENIED, Path: where,
		Message: fmt.Sprintf("the agent can't delete %s: %v, so nothing was deleted. %s", where, err, fixFor(err)),
	}
}
