// SPDX-License-Identifier: AGPL-3.0-only

package agent

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"syscall"
	"time"
	"unicode"
	"unicode/utf8"

	"golang.org/x/sys/unix"

	v1 "github.com/dockwell/dockwell/gen/dockwell/v1"
)

const (
	toolboxCleanupLimit = 256
	toolboxLogStage     = ".dockwell-log-"
)

func toolboxMount(path string) (uint64, error) {
	fd, err := unix.Open(path, unix.O_PATH|unix.O_NOFOLLOW|unix.O_CLOEXEC, 0)
	if err != nil {
		return 0, err
	}
	defer unix.Close(fd)
	return toolboxMountFD(fd)
}

func toolboxMountFD(fd int) (uint64, error) {
	var out unix.Statx_t
	err := unix.Statx(fd, "", unix.AT_EMPTY_PATH, unix.STATX_MNT_ID, &out)
	if err == nil && out.Mask&unix.STATX_MNT_ID != 0 {
		return out.Mnt_id, nil
	}
	// Older kernels expose the same mount ID on a file handle.
	raw, err := os.ReadFile("/proc/self/fdinfo/" + strconv.Itoa(fd))
	if err != nil {
		return 0, err
	}
	for line := range strings.SplitSeq(string(raw), "\n") {
		if id, ok := strings.CutPrefix(line, "mnt_id:"); ok {
			return strconv.ParseUint(strings.TrimSpace(id), 10, 64)
		}
	}
	return 0, errors.New("the kernel didn't identify the mount")
}

func toolboxSameMount(path, parent string) bool {
	file, err := toolboxMount(path)
	dir, dirErr := toolboxMount(parent)
	return err == nil && dirErr == nil && file == dir
}

type toolboxCandidate struct {
	path       string
	info       os.FileInfo
	mount      uint64
	tempCutoff time.Time
	rotated    bool
}

var (
	toolboxRotated = toolboxPattern(`(?:\.[1-9][0-9]*(?:\.(?:gz|xz|zst|bz2))?|[-.]20[0-9]{6}\.(?:gz|xz|zst|bz2))$`)
	toolboxJournal = toolboxPattern(`^(?:system|user-[0-9]+)@[a-f0-9]+-[a-f0-9]+-[a-f0-9]+\.journal$`)
	toolboxMachine = toolboxPattern(`^[a-f0-9]{32}$`)
)

func toolboxCandidateInfo(info os.FileInfo) string {
	st, _ := info.Sys().(*syscall.Stat_t)
	return fmt.Sprintf("%d/%d/%d/%d/%d/%d", st.Dev, st.Ino, st.Size, st.Mtim.Nano(), st.Ctim.Nano(), st.Mode)
}

func toolboxJournalSize(info os.FileInfo) uint64 {
	st, _ := info.Sys().(*syscall.Stat_t)
	if st.Blocks <= 0 {
		return 0
	}
	return uint64(st.Blocks) * 512
}

func (t *hostToolbox) planCleanup(ctx context.Context, edit *toolboxEdit, ask *v1.ToolboxCleanup) error {
	kind, days, keep := ask.GetKind(), ask.GetDays(), ask.GetKeepMib()
	if days > 3650 || keep > 1<<30 || (keep > 0 && (kind != v1.ToolboxCleanupKind_TOOLBOX_CLEANUP_KIND_JOURNAL || days > 0)) {
		return errors.New("choose an age or a journal size limit")
	}
	if kind == v1.ToolboxCleanupKind_TOOLBOX_CLEANUP_KIND_APT {
		days = 0
	} else if days == 0 && keep == 0 {
		days = 7
	}
	if kind == v1.ToolboxCleanupKind_TOOLBOX_CLEANUP_KIND_TEMPORARY && days < 3 {
		return errors.New("temporary files must be at least three days old")
	}
	var cutoff time.Time
	if days != 0 {
		cutoff = time.Now().Add(-time.Duration(days) * 24 * time.Hour)
	}
	candidates, err := t.scanCleanup(ctx, kind, cutoff, keep)
	if err != nil {
		return err
	}
	if len(candidates) > toolboxCleanupLimit {
		return errors.New("this scan found more than 256 files. Choose an older age or a larger size limit and scan again")
	}
	var total uint64
	hash := sha256.New()
	for _, c := range candidates {
		bytes := toolboxJournalSize(c.info)
		total += bytes
		edit.plan.Files = append(edit.plan.Files, &v1.ToolboxFile{Path: c.path, Bytes: bytes})
		fmt.Fprintf(hash, "%q %s %d\n", c.path, toolboxCandidateInfo(c.info), c.mount)
	}
	edit.extra = hash.Sum(nil)
	toolboxDifference(edit.plan, "Reclaimable files", fmt.Sprintf("%d files (%d bytes)", len(candidates), total), "")
	edit.apply = func(ctx context.Context) error {
		return t.cleanFiles(ctx, candidates)
	}
	return nil
}

func (t *hostToolbox) scanCleanup(ctx context.Context, kind v1.ToolboxCleanupKind, cutoff time.Time, keep uint64) ([]toolboxCandidate, error) {
	var dirs []string
	switch kind {
	case v1.ToolboxCleanupKind_TOOLBOX_CLEANUP_KIND_APT:
		dirs = []string{"/var/cache/apt/archives"}
	case v1.ToolboxCleanupKind_TOOLBOX_CLEANUP_KIND_JOURNAL:
		idRaw, _ := t.read("/etc/machine-id")
		id := strings.TrimSpace(string(idRaw))
		if !toolboxMachine().MatchString(id) {
			return nil, errors.New("this server has no usable journal machine ID")
		}
		dirs = []string{"/var/log/journal/" + id, "/run/log/journal/" + id}
	case v1.ToolboxCleanupKind_TOOLBOX_CLEANUP_KIND_ROTATED_LOGS:
		dirs = []string{"/var/log"}
	case v1.ToolboxCleanupKind_TOOLBOX_CLEANUP_KIND_TEMPORARY:
		dirs = []string{"/tmp"}
	default:
		return nil, errors.New("choose files to scan")
	}
	var chosen []toolboxCandidate
	var total uint64
	checked := 0
	var walk func(*os.File, string, uint64) error
	walk = func(parent *os.File, dir string, mount uint64) error {
		entries, err := parent.ReadDir(-1)
		if err != nil {
			return err
		}
		slices.SortFunc(entries, func(a, b os.DirEntry) int { return strings.Compare(a.Name(), b.Name()) })
		for _, entry := range entries {
			if err := ctx.Err(); err != nil {
				return err
			}
			checked++
			if checked > 20000 {
				return errors.New("the cleanup scan reached its file limit")
			}
			base := entry.Name()
			name := filepath.Join(dir, base)
			if entry.IsDir() {
				if kind != v1.ToolboxCleanupKind_TOOLBOX_CLEANUP_KIND_ROTATED_LOGS || name == "/var/log/journal" || strings.HasPrefix(base, toolboxLogStage) {
					continue
				}
				child, err := t.openCleanupDirAt(parent, base, name)
				if err != nil {
					continue
				}
				id, err := toolboxMountFD(int(child.Fd()))
				if err == nil && id == mount {
					err = walk(child, name, mount)
				}
				_ = child.Close()
				if err != nil {
					return err
				}
				continue
			}
			if !utf8.ValidString(name) || strings.ContainsFunc(name, unicode.IsControl) {
				continue
			}
			handle, err := toolboxOpenAt(parent, base, unix.O_PATH)
			if err != nil {
				continue
			}
			info, err := handle.Stat()
			id, mountErr := toolboxMountFD(int(handle.Fd()))
			if err == nil && mountErr == nil && id == mount && toolboxRegular(info) &&
				(kind == v1.ToolboxCleanupKind_TOOLBOX_CLEANUP_KIND_ROTATED_LOGS || t.owns(info)) {
				if kind == v1.ToolboxCleanupKind_TOOLBOX_CLEANUP_KIND_JOURNAL && strings.HasSuffix(base, ".journal") {
					total += toolboxJournalSize(info)
				}
				if toolboxCleanupName(kind, base, info) && toolboxOldEnough(kind, info, cutoff) {
					if lease, err := toolboxLease(handle); err == nil {
						_ = lease.Close()
						candidate := toolboxCandidate{path: name, info: info, mount: mount, rotated: kind == v1.ToolboxCleanupKind_TOOLBOX_CLEANUP_KIND_ROTATED_LOGS}
						if kind == v1.ToolboxCleanupKind_TOOLBOX_CLEANUP_KIND_TEMPORARY {
							candidate.tempCutoff = cutoff
						}
						chosen = append(chosen, candidate)
					}
				}
			}
			_ = handle.Close()
		}
		return nil
	}
	for _, dir := range dirs {
		parent, err := t.openCleanupDir(dir)
		if errors.Is(err, os.ErrNotExist) {
			continue
		}
		if err != nil {
			return nil, err
		}
		mount, err := toolboxMountFD(int(parent.Fd()))
		if err == nil {
			err = walk(parent, dir, mount)
		}
		_ = parent.Close()
		if err != nil {
			return nil, err
		}
	}

	if keep > 0 {
		slices.SortFunc(chosen, func(a, b toolboxCandidate) int {
			if cmp := a.info.ModTime().Compare(b.info.ModTime()); cmp != 0 {
				return cmp
			}
			return strings.Compare(a.path, b.path)
		})
		count := 0
		for count < len(chosen) && total > keep*swapMib {
			total -= toolboxJournalSize(chosen[count].info)
			count++
		}
		chosen = chosen[:count]
	}
	return chosen, nil
}

func toolboxCleanupName(kind v1.ToolboxCleanupKind, name string, info os.FileInfo) bool {
	switch kind {
	case v1.ToolboxCleanupKind_TOOLBOX_CLEANUP_KIND_APT:
		return strings.HasSuffix(name, ".deb")
	case v1.ToolboxCleanupKind_TOOLBOX_CLEANUP_KIND_JOURNAL:
		return toolboxJournal().MatchString(name)
	case v1.ToolboxCleanupKind_TOOLBOX_CLEANUP_KIND_ROTATED_LOGS:
		return toolboxRotated().MatchString(name)
	case v1.ToolboxCleanupKind_TOOLBOX_CLEANUP_KIND_TEMPORARY:
		return !strings.HasPrefix(name, ".") && info.Mode().Perm()&0o077 == 0
	default:
		return false
	}
}

func toolboxOldEnough(kind v1.ToolboxCleanupKind, info os.FileInfo, cutoff time.Time) bool {
	if cutoff.IsZero() {
		return true
	}
	if !info.ModTime().Before(cutoff) {
		return false
	}
	if kind != v1.ToolboxCleanupKind_TOOLBOX_CLEANUP_KIND_TEMPORARY {
		return true
	}
	st, _ := info.Sys().(*syscall.Stat_t)
	return time.Unix(st.Ctim.Unix()).Before(cutoff) && time.Unix(st.Atim.Unix()).Before(cutoff)
}

func toolboxOpenAt(parent *os.File, name string, flags int) (*os.File, error) {
	fd, err := unix.Openat(int(parent.Fd()), name, flags|unix.O_NOFOLLOW|unix.O_CLOEXEC, 0)
	if err != nil {
		return nil, err
	}
	return os.NewFile(uintptr(fd), "/proc/self/fd/"+strconv.Itoa(fd)), nil
}

func (t *hostToolbox) openCleanupDirAt(parent *os.File, base, path string) (*os.File, error) {
	dir, err := toolboxOpenAt(parent, base, unix.O_RDONLY|unix.O_DIRECTORY)
	if err != nil {
		return nil, err
	}
	info, err := dir.Stat()
	if err == nil && (!t.owns(info) || (info.Mode().Perm()&0o002 != 0 || path == "/tmp") && info.Mode()&os.ModeSticky == 0) {
		err = fmt.Errorf("%s isn't a trusted cleanup directory", path)
	}
	if err != nil {
		_ = dir.Close()
		return nil, err
	}
	return dir, nil
}

func (t *hostToolbox) openCleanupDir(path string) (*os.File, error) {
	dir, err := os.Open(t.root())
	if err != nil {
		return nil, err
	}
	var name string
	for part := range strings.SplitSeq(strings.TrimPrefix(path, "/"), "/") {
		name += "/" + part
		child, err := t.openCleanupDirAt(dir, part, name)
		_ = dir.Close()
		if err != nil {
			return nil, err
		}
		dir = child
	}
	return dir, nil
}

func toolboxLease(handle *os.File) (*os.File, error) {
	// The O_PATH handle was checked as a regular file before opening that
	// exact inode for an exclusive lease. A swapped name cannot open a device.
	file, err := os.OpenFile(fmt.Sprintf("/proc/self/fd/%d", handle.Fd()), os.O_RDWR|unix.O_NONBLOCK, 0)
	if err != nil {
		return nil, err
	}
	// SIGWINCH is ignored by default. An opener still waits for the lease.
	if _, err := unix.FcntlInt(file.Fd(), unix.F_SETSIG, int(unix.SIGWINCH)); err != nil {
		_ = file.Close()
		return nil, err
	}
	if _, err := unix.FcntlInt(file.Fd(), unix.F_SETLEASE, unix.F_WRLCK); err != nil {
		_ = file.Close()
		return nil, err
	}
	return file, nil
}

func (c toolboxCandidate) matches(parent *os.File) bool {
	return c.matchesAt(parent, filepath.Base(c.path), true)
}

func (c toolboxCandidate) matchesAt(parent *os.File, name string, checkChangeTime bool) bool {
	var now unix.Stat_t
	if err := unix.Fstatat(int(parent.Fd()), name, &now, unix.AT_SYMLINK_NOFOLLOW); err != nil {
		return false
	}
	was, _ := c.info.Sys().(*syscall.Stat_t)
	return now.Dev == was.Dev && now.Ino == was.Ino && now.Size == was.Size && now.Mode == was.Mode &&
		now.Nlink == 1 && now.Uid == was.Uid && now.Gid == was.Gid && now.Mtim.Nano() == was.Mtim.Nano() &&
		(!checkChangeTime || now.Ctim.Nano() == was.Ctim.Nano()) &&
		(c.tempCutoff.IsZero() || time.Unix(now.Atim.Unix()).Before(c.tempCutoff))
}

type toolboxCleanupLease struct {
	parent *os.File
	file   *os.File
}

func (f toolboxCleanupLease) exclusive() bool {
	lease, err := unix.FcntlInt(f.file.Fd(), unix.F_GETLEASE, 0)
	return err == nil && lease == unix.F_WRLCK
}

func (t *hostToolbox) leaseCleanup(c toolboxCandidate) (toolboxCleanupLease, error) {
	refused := fmt.Errorf("%s is in use or changed. Scan again before cleaning", c.path)
	parent, err := t.openCleanupDir(filepath.Dir(c.path))
	if err != nil {
		return toolboxCleanupLease{}, refused
	}
	lease, err := c.leaseIn(parent)
	if err != nil {
		_ = parent.Close()
		return toolboxCleanupLease{}, refused
	}
	return toolboxCleanupLease{parent: parent, file: lease}, nil
}

func (c toolboxCandidate) leaseIn(parent *os.File) (*os.File, error) {
	handle, err := toolboxOpenAt(parent, filepath.Base(c.path), unix.O_PATH)
	if err != nil {
		return nil, err
	}
	defer handle.Close()
	current, err := handle.Stat()
	mount, mountErr := toolboxMountFD(int(handle.Fd()))
	parentMount, parentErr := toolboxMountFD(int(parent.Fd()))
	if err != nil || mountErr != nil || parentErr != nil || mount != c.mount || mount != parentMount ||
		!toolboxRegular(current) || toolboxCandidateInfo(current) != toolboxCandidateInfo(c.info) || !c.matches(parent) {
		return nil, errors.New("the reviewed file changed")
	}
	return toolboxLease(handle)
}

// A rotator can replace a log name between fstatat and unlinkat. Move the
// entry atomically into a private directory and verify the leased inode at
// its new name before unlinking it. Rename itself changes ctime.
func (t *hostToolbox) unlinkRotatedLog(c toolboxCandidate, f toolboxCleanupLease) error {
	parent := int(f.parent.Fd())
	name, base := toolboxLogStage+rand.Text(), filepath.Base(c.path)
	if err := unix.Mkdirat(parent, name, 0o700); err != nil {
		return err
	}
	stage, err := toolboxOpenAt(f.parent, name, unix.O_RDONLY|unix.O_DIRECTORY)
	if err != nil {
		return err
	}
	defer stage.Close()
	info, err := stage.Stat()
	if err != nil || !t.owns(info) || info.Mode().Perm() != 0o700 {
		return errors.New("couldn't open a private directory for log cleanup")
	}
	defer func() { _ = unix.Unlinkat(parent, name, unix.AT_REMOVEDIR) }()
	private := int(stage.Fd())
	if err := unix.Renameat2(parent, base, private, base, unix.RENAME_NOREPLACE); err != nil {
		return err
	}
	if f.exclusive() && c.matchesAt(stage, base, false) {
		err = unix.Unlinkat(private, base, 0)
		if err == nil {
			return nil
		}
	} else {
		err = fmt.Errorf("%s changed during log rotation", c.path)
	}
	if restore := unix.Renameat2(private, base, parent, base, unix.RENAME_NOREPLACE); restore != nil {
		retained := filepath.Join(filepath.Dir(c.path), name, base)
		return errors.Join(err, fmt.Errorf("the log was kept at %s because its original name couldn't be restored: %w", retained, restore))
	}
	return err
}

func (t *hostToolbox) cleanFiles(ctx context.Context, candidates []toolboxCandidate) error {
	files := make([]toolboxCleanupLease, 0, len(candidates))
	defer func() {
		for _, f := range files {
			_ = f.file.Close()
			_ = f.parent.Close()
		}
	}()
	for _, c := range candidates {
		if err := ctx.Err(); err != nil {
			return err
		}
		f, err := t.leaseCleanup(c)
		if err != nil {
			return err
		}
		files = append(files, f)
	}
	for i, c := range candidates {
		if !c.matches(files[i].parent) {
			return fmt.Errorf("%s changed after the scan. Scan again before cleaning", c.path)
		}
	}
	for i, c := range candidates {
		f := files[i]
		if !f.exclusive() || !c.matches(f.parent) {
			return fmt.Errorf("%s is in use or changed. Scan again before cleaning", c.path)
		}
		var err error
		if c.rotated {
			err = t.unlinkRotatedLog(c, f)
		} else {
			err = unix.Unlinkat(int(f.parent.Fd()), filepath.Base(c.path), 0)
		}
		if err != nil {
			return fmt.Errorf("some files may already be cleaned. Scan again: %w", err)
		}
	}
	return nil
}

// apt uses POSIX record locks across the entire lists and archives lock
// files. Hold the same locks, including dpkg's frontend lock, until done.
func (t *hostToolbox) lockAPTCleanup() (func(), error) {
	var locks []*os.File
	release := func() {
		for _, lock := range locks {
			_ = lock.Close()
		}
	}
	for _, path := range []string{"/var/lib/dpkg/lock-frontend", "/var/lib/apt/lists/lock", "/var/cache/apt/archives/lock"} {
		if err := t.trustedParent(path); err != nil {
			release()
			return nil, err
		}
		lock, err := os.OpenFile(t.path(path), os.O_RDWR|os.O_CREATE|unix.O_NOFOLLOW|unix.O_NONBLOCK, 0o640) //nolint:gosec // apt opens its own locks with mode 0640
		if err != nil {
			release()
			return nil, err
		}
		locks = append(locks, lock)
		info, err := lock.Stat()
		if err != nil || !toolboxRegular(info) || !t.owns(info) || info.Mode().Perm()&0o022 != 0 {
			release()
			return nil, fmt.Errorf("%s isn't a trusted apt lock file", path)
		}
		flock := unix.Flock_t{Type: unix.F_WRLCK, Whence: unix.SEEK_SET}
		if err := unix.FcntlFlock(lock.Fd(), unix.F_SETLK, &flock); err != nil {
			release()
			if errors.Is(err, unix.EACCES) || errors.Is(err, unix.EAGAIN) {
				return nil, errors.New("apt is running on this server. Scan again when it finishes")
			}
			return nil, fmt.Errorf("can't take apt's lock %s: %w", path, err)
		}
	}
	return release, nil
}
