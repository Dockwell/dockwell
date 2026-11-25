// SPDX-License-Identifier: AGPL-3.0-only

package files

import (
	"bytes"
	"context"
	"crypto/sha256"
	"errors"
	"fmt"
	"io"
	"math"
	"os"
	posixpath "path"
	"slices"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"

	"golang.org/x/sys/unix"
	"google.golang.org/protobuf/proto"

	agentv1 "github.com/dockwell/dockwell/gen/dockwell/agent/v1"
	dockwellv1 "github.com/dockwell/dockwell/gen/dockwell/v1"
	"github.com/dockwell/dockwell/internal/resource"
)

const (
	maxFileAccounts = 4096
	// Leave room in the response and Undo request for each path's before and after values.
	maxAccessPathsBytes = 512 << 10
	accessHeadBytes     = 4 << 10
)

// ListFileAccounts reads the agent's local account and group lists.
func (s *Server) ListFileAccounts(ctx context.Context) (*agentv1.ListFileAccountsResult, error) {
	return attempt(ctx, s, "/etc/passwd", func(_ context.Context, t *trail) (*agentv1.ListFileAccountsResult, error) {
		if r := t.touch("/etc/passwd"); r != nil {
			return &agentv1.ListFileAccountsResult{Refusal: r}, nil
		}
		passwd, groups := s.accounts()
		return &agentv1.ListFileAccountsResult{Response: &dockwellv1.ListFileAccountsResponse{
			Users: fileAccounts(passwd), Groups: fileAccounts(groups),
		}}, nil
	}, func(r *agentv1.FileRefusal) *agentv1.ListFileAccountsResult {
		return &agentv1.ListFileAccountsResult{Refusal: r}
	})
}

func fileAccounts(text string) []*dockwellv1.FileAccount {
	var accounts []*dockwellv1.FileAccount
	for line := range strings.Lines(text) {
		fields := strings.SplitN(strings.TrimSpace(line), ":", 4)
		if len(fields) < 3 || fields[0] == "" || len(fields[0]) > MaxName || Printable(fields[0]) != fields[0] {
			continue
		}
		id, err := strconv.ParseUint(fields[2], 10, 32)
		if err != nil || id == math.MaxUint32 {
			continue
		}
		accounts = append(accounts, &dockwellv1.FileAccount{Name: fields[0], Id: uint32(id)})
		if len(accounts) >= maxFileAccounts {
			break
		}
	}
	return accounts
}

// CheckAccessChange validates the paths and values accepted by ChangeFileAccess.
func CheckAccessChange(path string, c *dockwellv1.FileAccessChange) error {
	if err := CheckPath(path); err != nil {
		return err
	}
	if err := CheckName(path); err != nil {
		return err
	}
	if c == nil || c.Mode == nil && c.FileMode == nil && c.Uid == nil && c.Gid == nil && len(c.GetRestore()) == 0 {
		return errors.New("choose permissions, a user or a group to change")
	}
	if c.GetMode() > 0o7777 || c.GetFileMode() > 0o7777 || (c.GetMode()|c.GetFileMode())&0o6000 != 0 && !c.GetAllowSetId() {
		return errors.New("confirm set-user-ID or set-group-ID explicitly before enabling either")
	}
	if c.GetUid() == math.MaxUint32 || c.GetGid() == math.MaxUint32 {
		return errors.New("choose a valid user and group")
	}
	if !c.GetRecursive() && (c.FileMode != nil || c.GetKeepFileExecute()) {
		return errors.New("choose a folder and its contents to set a separate file mode")
	}
	if len(c.GetRestore()) > 0 {
		return checkAccessRestore(c)
	}
	return nil
}

func checkAccessRestore(c *dockwellv1.FileAccessChange) error {
	if c.Mode != nil || c.FileMode != nil || c.Uid != nil || c.Gid != nil || c.GetKeepFileExecute() ||
		len(c.GetRestore()) > scanEntries+1 || !c.GetRecursive() && len(c.GetRestore()) > 1 {
		return errors.New("undo an earlier permission change by itself")
	}
	seen := make(map[string]bool, len(c.GetRestore()))
	total := 0
	for _, item := range c.GetRestore() {
		p, before, after := item.GetPath(), item.GetBefore(), item.GetAfter()
		total += len(p)
		if CheckPath(p) != nil || seen[p] || total > maxAccessPathsBytes ||
			before == nil || after == nil || before.GetMode() > 0o7777 || after.GetMode() > 0o7777 ||
			before.GetMode()&0o6000 != 0 && !c.GetAllowSetId() ||
			before.GetUid() == math.MaxUint32 || before.GetGid() == math.MaxUint32 {
			return errors.New("send the original undo entries from the permission change")
		}
		seen[p] = true
	}
	return nil
}

type accessProgress struct {
	mu               sync.Mutex
	path             string
	changed, skipped uint64
	wayIn            bool
	undo             []*dockwellv1.FileAccessUndoEntry
}

func (p *accessProgress) snapshot(stopped string) *dockwellv1.ChangeFileAccessResponse {
	p.mu.Lock()
	defer p.mu.Unlock()
	return &dockwellv1.ChangeFileAccessResponse{
		Path: p.path, Changed: p.changed, Skipped: p.skipped, WayBackIn: p.wayIn,
		Stopped: stopped, Undo: slices.Clone(p.undo),
	}
}

type accessChange struct {
	s         *Server
	t         *trail
	ctx       context.Context
	guard     *guard
	ways      *waysIn
	change    *dockwellv1.FileAccessChange
	progress  *accessProgress
	approved  bool
	acting    *atomic.Bool
	rootDepth int
	planned   map[string]identity
	head      []byte
	restore   map[string]*dockwellv1.FileAccessUndoEntry
}

func (a *accessChange) requested(w *walked, where string) (mode, uid, gid uint32) {
	st := &w.st
	mode, uid, gid = st.Mode&0o7777, st.Uid, st.Gid
	if item := a.restore[where]; item != nil {
		before := item.GetBefore()
		return before.GetMode(), before.GetUid(), before.GetGid()
	}
	c := a.change
	switch {
	case c.GetRecursive() && !isDir(st):
		if c.FileMode != nil {
			mode = c.GetFileMode()
			if c.GetKeepFileExecute() {
				keep := uint32(0o111)
				if c.Mode != nil {
					keep &= c.GetMode()
				}
				mode |= st.Mode & keep
			}
		} else if c.Mode != nil {
			// A legacy recursive mode describes a folder. Its search bits do not
			// make regular files executable.
			mode = c.GetMode() &^ 0o111
			mode |= st.Mode & 0o111 & folderSearch(c.GetMode())
		}
	case c.Mode != nil:
		mode = c.GetMode()
		if c.GetRecursive() && c.FileMode == nil {
			mode |= folderSearch(mode)
		}
	}
	if c.Uid != nil {
		uid = c.GetUid()
	}
	if c.Gid != nil {
		gid = c.GetGid()
	}
	return mode, uid, gid
}

func folderSearch(mode uint32) uint32 { return (mode & 0o444) >> 2 }

func (a *accessChange) sensitive(w *walked, where string, mode, uid, gid uint32) bool {
	if w.st.Uid == 0 || w.st.Gid == 0 || uid == 0 || gid == 0 ||
		mode&0o6000 != 0 || resource.SystemPath(where, false) {
		return true
	}
	dir := isDir(&w.st)
	widens := mode&^w.st.Mode != 0 || uid != w.st.Uid || gid != w.st.Gid
	if !dir && credentialName(where) {
		// Removing permission bits is a normal repair for a key or password file.
		return widens
	}
	tg := &target{w: &walked{steps: w.steps[:len(w.steps)-1]}, name: w.last().name, where: where}
	return a.s.wayInWith(a.ways, tg, &w.st, dir) || !dir && widens && a.protectedHead(w.last().fd)
}

// protectedHead checks the same inode as the permission change. If its contents
// can't be checked, widening it still takes a passkey. apply checks its ctime
// before writing so it can't act on a file changed since the check.
func (a *accessChange) protectedHead(fd int) bool {
	readFD, err := unix.Open("/proc/self/fd/"+strconv.Itoa(fd), unix.O_RDONLY|unix.O_NONBLOCK|unix.O_CLOEXEC, 0)
	if err != nil {
		return true
	}
	f := os.NewFile(uintptr(readFD), "file header")
	defer f.Close()
	if a.head == nil {
		a.head = make([]byte, accessHeadBytes)
	}
	n, err := io.ReadFull(f, a.head)
	head := a.head[:n]
	protected := err != nil && !errors.Is(err, io.EOF) && !errors.Is(err, io.ErrUnexpectedEOF) ||
		holdsPrivateKey(head) || bytes.Contains(head, beginKeyMark) && keyStarts().Match(head) ||
		!showsBinary(head) && hashLine().Match(head)
	clear(head)
	return protected
}

func credentialName(where string) bool {
	if credentials(where) != "" {
		return true
	}
	name := strings.TrimRight(strings.ToLower(posixpath.Base(where)), "~")
	if strings.HasSuffix(name, ".pub") {
		return false
	}
	for _, known := range []string{"id_", "ssh_host_", ".htpasswd", "htpasswd", "shadow", "gshadow", ".pgpass", ".my.cnf", ".netrc", "password", "passwd", "privkey", "private_key", "private-key"} {
		if strings.HasPrefix(name, known) {
			return true
		}
	}
	switch posixpath.Ext(name) {
	case ".key", ".pem", ".p12", ".pfx":
		return true
	}
	return false
}

func (a *accessChange) eligible(w *walked) *agentv1.FileRefusal {
	where := w.path()
	switch {
	case w.last().place != free:
		return refused(where, whyVolumeOpts)
	case isDir(&w.st):
		return nil
	case w.st.Mode&unix.S_IFMT != unix.S_IFREG:
		return refused(where, "neither a regular file nor a folder")
	case w.st.Nlink != 1:
		return refused(where, hardLinkedFile)
	}
	return nil
}

func changeLimit(where string) *agentv1.FileRefusal {
	return &agentv1.FileRefusal{
		Kind: agentv1.FileRefusal_KIND_TOO_LARGE, Path: where,
		Message: "the scan reached its limit. Choose a smaller folder",
	}
}

func accessState(st *unix.Stat_t) *dockwellv1.FileAccessState {
	return &dockwellv1.FileAccessState{Mode: st.Mode & 0o7777, Uid: st.Uid, Gid: st.Gid}
}

func (a *accessChange) apply(w *walked, where string, mode, uid, gid uint32) *agentv1.FileRefusal {
	fd := w.last().fd
	if r := a.t.touch(where); r != nil {
		return r
	}
	// The descriptor still refers to the inode we checked, even if it was renamed.
	procFD := "/proc/self/fd/" + strconv.Itoa(fd)
	if a.ctx.Err() != nil {
		return notAnswering(where)
	}
	var st unix.Stat_t
	if err := unix.Fstat(fd, &st); err != nil {
		return accessError(where, err)
	}
	if st.Uid != w.st.Uid || st.Gid != w.st.Gid || st.Mode != w.st.Mode || !isDir(&st) && (st.Nlink != 1 || st.Ctim != w.st.Ctim) {
		return gone(where)
	}
	if a.ctx.Err() != nil {
		return notAnswering(where)
	}
	before := accessState(&st)
	chown, chmod := uid != st.Uid || gid != st.Gid, mode != before.GetMode()
	// A chown can drop set-ID bits. Preserve an explicitly requested mode.
	explicitMode := a.change.Mode != nil || !isDir(&st) && a.change.FileMode != nil || a.restore != nil
	chmod = chmod || chown && explicitMode
	index := -1
	record := func(current *unix.Stat_t) {
		p := a.progress
		p.mu.Lock()
		defer p.mu.Unlock()
		entry := &dockwellv1.FileAccessUndoEntry{Path: where, Device: st.Dev, Inode: st.Ino, Before: before}
		if current != nil {
			entry.After, entry.ChangeTimeNs = accessState(current), current.Ctim.Nano()
		}
		if index == -1 {
			index = len(p.undo)
			p.changed++
			p.undo = append(p.undo, entry)
		} else {
			p.undo[index] = entry
		}
	}
	a.acting.Store(true)
	defer a.acting.Store(false)
	if a.ctx.Err() != nil {
		return notAnswering(where)
	}
	if chown {
		owner, group := -1, -1
		if uid != st.Uid {
			owner = int(uid)
		}
		if gid != st.Gid {
			group = int(gid)
		}
		if err := unix.Fchownat(fd, "", owner, group, unix.AT_EMPTY_PATH); err != nil {
			return accessError(where, err)
		}
		record(nil)
	}
	var failure *agentv1.FileRefusal
	if chmod {
		switch {
		case a.ctx.Err() != nil:
			return notAnswering(where)
		default:
			if err := unix.Chmod(procFD, mode); err != nil {
				failure = accessError(where, err)
			} else if index == -1 {
				record(nil)
			}
		}
	}
	if index != -1 {
		var current unix.Stat_t
		if err := unix.Fstat(fd, &current); err != nil {
			return accessError(where, err)
		}
		record(&current)
	}
	return failure
}

func accessError(where string, err error) *agentv1.FileRefusal {
	return &agentv1.FileRefusal{
		Kind: agentv1.FileRefusal_KIND_DENIED, Path: where,
		Message: fmt.Sprintf("the agent can't change permissions or ownership on %s: %v. %s", where, err, fixFor(err)),
	}
}

func (a *accessChange) walk(w *walked, apply bool) *agentv1.FileRefusal {
	var stopped *agentv1.FileRefusal
	var matched uint64
	var pathBytes, count int
	var needsProof bool
	visit := func(item *walked) bool {
		where := item.path()
		if a.ctx.Err() != nil {
			stopped = notAnswering(where)
			return false
		}
		if a.restore != nil {
			restore := a.restore[where]
			if restore == nil || restore.GetDevice() != item.st.Dev || restore.GetInode() != item.st.Ino || restore.GetChangeTimeNs() != item.st.Ctim.Nano() ||
				!proto.Equal(restore.GetAfter(), accessState(&item.st)) || a.eligible(item) != nil {
				return true
			}
			matched++
		} else if r := a.eligible(item); r != nil {
			if len(item.steps) == a.rootDepth {
				stopped = r
				return false
			}
			if apply {
				a.skip(1)
			}
			return true
		}
		mode, uid, gid := a.requested(item, where)
		if mode == item.st.Mode&0o7777 && uid == item.st.Uid && gid == item.st.Gid {
			return true
		}
		if !apply {
			count++
			pathBytes += len(where)
			if pathBytes > maxAccessPathsBytes {
				return true // Finish counting without reserving more paths or reading their contents.
			}
			a.planned[where] = item.last().id
		} else if a.planned != nil {
			if id, ok := a.planned[where]; !ok || id != item.last().id {
				a.skip(1)
				return true
			}
			// Only write entries that were counted, at most once per path.
			delete(a.planned, where)
		}
		if a.sensitive(item, where, mode, uid, gid) {
			a.progress.mu.Lock()
			a.progress.wayIn = true
			a.progress.mu.Unlock()
			if !a.approved {
				if !apply {
					needsProof = true
					return true
				}
				stopped = needsPasskey(a.progress.path, "changing permissions or ownership")
				return false
			}
		}
		if apply {
			stopped = a.apply(item, where, mode, uid, gid)
		}
		return stopped == nil
	}
	if a.change.GetRecursive() {
		scan := a.s.tree(a.ctx, a.t, a.guard)
		scan.access = true
		scan.below(w, 0, visit)
		if scan.limited && stopped == nil {
			stopped = changeLimit(a.progress.path)
		}
		if apply && a.restore == nil {
			a.skip(scan.skipped)
		}
	}
	if stopped == nil {
		visit(w)
	}
	if stopped == nil && pathBytes > maxAccessPathsBytes {
		stopped = &agentv1.FileRefusal{
			Kind: agentv1.FileRefusal_KIND_TOO_LARGE, Path: a.progress.path,
			Message: fmt.Sprintf("undoing changes to %d entries would need %d bytes of paths, over the %d-byte limit. Choose a smaller folder", count, pathBytes, maxAccessPathsBytes),
		}
	}
	if stopped == nil && needsProof {
		stopped = needsPasskey(a.progress.path, "changing permissions or ownership")
	}
	if apply && a.restore != nil {
		a.skip(uint64(len(a.restore)) - matched)
	}
	return stopped
}

func (a *accessChange) skip(n uint64) {
	a.progress.mu.Lock()
	a.progress.skipped += n
	a.progress.mu.Unlock()
}

// ChangeFileAccess applies the requested mode or ownership through checked
// descriptors. A protected change's proof binds the resolved root, its identity
// and all requested values. A recursive change preflights before it writes.
func (s *Server) ChangeFileAccess(ctx context.Context, call *agentv1.ChangeFileAccessCall) (*agentv1.ChangeFileAccessResult, error) {
	p, change := call.GetPath(), call.GetChange()
	if err := CheckAccessChange(p, change); err != nil {
		return nil, err
	}
	var acting atomic.Bool
	progress := &accessProgress{path: p}
	refusal := func(r *agentv1.FileRefusal) *agentv1.ChangeFileAccessResult {
		return &agentv1.ChangeFileAccessResult{Refusal: r}
	}
	return attempt(ctx, s, p, func(ctx context.Context, t *trail) (*agentv1.ChangeFileAccessResult, error) {
		g := s.guard(ctx)
		tg, r := s.targetOf(t, g, p)
		if r != nil {
			return refusal(r), nil
		}
		w := tg.w
		defer w.close()
		if slices.ContainsFunc(w.steps, func(step step) bool { return Printable(step.name) != step.name }) ||
			arriving(tg.name) || strings.HasPrefix(tg.name, asidePrefix) || Printable(tg.name) != tg.name {
			return refusal(refused(tg.where, "a path Dockwell cannot change")), nil
		}
		if r := t.touch(tg.where); r != nil {
			return refusal(r), nil
		}
		fd, err := unix.Openat(tg.dir(), tg.name, unix.O_PATH|unix.O_NOFOLLOW|unix.O_CLOEXEC, 0)
		if err != nil {
			return refusal(failed(tg.where, err)), nil
		}
		st := unix.Stat_t{}
		if err := unix.Fstat(fd, &st); err != nil {
			_ = unix.Close(fd)
			return refusal(failed(tg.where, err)), nil
		}
		into, why := g.enter(w.last().place, tg.name, &st)
		if why != "" || into != free || isDir(&st) && dockerShaped(fd) {
			_ = unix.Close(fd)
			if why == "" {
				why = whyDocker
			}
			return refusal(refused(tg.where, why)), nil
		}
		w.steps = append(w.steps, step{fd: fd, name: tg.name, place: into, id: identify(&st)})
		w.st = st
		if change.GetRecursive() && !isDir(&st) {
			return refusal(refused(tg.where, "not a folder")), nil
		}
		bits, err := proto.Marshal(change)
		if err != nil {
			return nil, err
		}
		digest := sha256.Sum256(fmt.Appendf(bits, "\x00%d:%d", st.Dev, st.Ino))
		target := fmt.Sprintf("%s\n%x\n%s", tg.where, digest, accessSummary(change))
		if call.GetConfirmed() != "" && call.GetConfirmed() != target {
			return refusal(gone(tg.where)), nil
		}
		progress.mu.Lock()
		progress.path = tg.where
		progress.mu.Unlock()
		a := accessChange{
			s: s, t: t, ctx: ctx, guard: g, ways: s.waysIn(), change: change, progress: progress,
			approved: call.GetConfirmed() == target, acting: &acting, rootDepth: len(w.steps),
		}
		if len(change.GetRestore()) > 0 {
			a.restore = make(map[string]*dockwellv1.FileAccessUndoEntry, len(change.GetRestore()))
			for _, item := range change.GetRestore() {
				if item.GetPath() != tg.where && (!change.GetRecursive() || !strings.HasPrefix(item.GetPath(), tg.where+"/")) {
					return refusal(refused(item.GetPath(), "outside the folder being restored")), nil
				}
				a.restore[item.GetPath()] = item
			}
		}
		if change.GetRecursive() {
			a.planned = make(map[string]identity)
			if r := a.walk(w, false); r != nil {
				return &agentv1.ChangeFileAccessResult{Refusal: r, Confirmation: target}, nil
			}
		}
		if r := a.walk(w, true); r != nil {
			answer := progress.snapshot(r.GetMessage())
			if answer.GetChanged() == 0 {
				return &agentv1.ChangeFileAccessResult{Refusal: r, Confirmation: target}, nil
			}
			return &agentv1.ChangeFileAccessResult{Response: answer}, nil
		}
		return &agentv1.ChangeFileAccessResult{Response: progress.snapshot("")}, nil
	}, func(r *agentv1.FileRefusal) *agentv1.ChangeFileAccessResult {
		r = leftBehind(r, &acting, "changing permissions or ownership")
		if answer := progress.snapshot(r.GetMessage()); answer.GetChanged() > 0 {
			return &agentv1.ChangeFileAccessResult{Response: answer}
		}
		return refusal(r)
	})
}

func accessSummary(c *dockwellv1.FileAccessChange) string {
	if len(c.GetRestore()) > 0 {
		return fmt.Sprintf("restore previous access on %d entries", len(c.GetRestore()))
	}
	var edits []string
	if c.Mode != nil {
		if c.GetRecursive() {
			folder, file := c.GetMode(), c.GetFileMode()
			keep := c.GetKeepFileExecute()
			if c.FileMode == nil {
				folder |= folderSearch(folder)
				file = c.GetMode() &^ 0o111
				keep = true
			}
			edits = append(edits, fmt.Sprintf("folders %04o, files %04o", folder, file))
			if keep {
				edits = append(edits, "keep existing file execute bits")
			}
		} else {
			edits = append(edits, fmt.Sprintf("mode %04o", c.GetMode()))
		}
	} else if c.FileMode != nil {
		edits = append(edits, fmt.Sprintf("files %04o", c.GetFileMode()))
		if c.GetKeepFileExecute() {
			edits = append(edits, "keep existing file execute bits")
		}
	}
	if c.Uid != nil {
		edits = append(edits, fmt.Sprintf("owner ID %d", c.GetUid()))
	}
	if c.Gid != nil {
		edits = append(edits, fmt.Sprintf("group ID %d", c.GetGid()))
	}
	if c.GetRecursive() {
		edits = append(edits, "include everything inside")
	}
	return strings.Join(edits, ", ")
}
