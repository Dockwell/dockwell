// SPDX-License-Identifier: AGPL-3.0-only

package agent

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"syscall"

	"golang.org/x/sys/unix"

	v1 "github.com/dockwell/dockwell/gen/dockwell/v1"
)

const (
	toolboxSwapPath = "/var/lib/dockwell-toolbox.swap"
	swapMib         = 1 << 20
	swapMark        = " # Dockwell toolbox"
	fstabPath       = "/etc/fstab"
	swappinessPath  = "/etc/sysctl.d/zz-dockwell-toolbox.conf"
	swapIntentName  = "host-toolbox-swap-intent"
)

func (t *hostToolbox) owns(info os.FileInfo) bool {
	st, ok := info.Sys().(*syscall.Stat_t)
	uid := 0
	if t.tools.Root != "" {
		uid = os.Geteuid()
	}
	return ok && int(st.Uid) == uid
}

func toolboxRegular(info os.FileInfo) bool {
	st, ok := info.Sys().(*syscall.Stat_t)
	return ok && info.Mode().IsRegular() && st.Nlink == 1
}

func (t *hostToolbox) swaps() (active []*v1.ToolboxActiveSwap, managed, recommended, free uint64, why string) {
	raw, _ := t.read("/proc/swaps")
	for line := range strings.SplitSeq(string(raw), "\n") {
		f := strings.Fields(line)
		if len(f) != 5 || f[0] == "Filename" {
			continue
		}
		size, _ := strconv.ParseUint(f[2], 10, 64)
		used, _ := strconv.ParseUint(f[3], 10, 64)
		active = append(active, &v1.ToolboxActiveSwap{Path: f[0], SizeMib: size / 1024, UsedMib: (used + 1023) / 1024})
	}
	raw, _ = t.read(fstabPath)
	if fi, err := os.Lstat(t.path(toolboxSwapPath)); err == nil && toolboxRegular(fi) && t.owns(fi) &&
		fi.Mode().Perm() == 0o600 && (slices.Contains(strings.Split(string(raw), "\n"), toolboxSwapLine()) || t.interruptedSwap(fi)) {
		if n := fi.Size(); n > 0 {
			managed = uint64(n) / swapMib
		}
	}

	recommended = 1024
	if t.memMiB("MemTotal:") >= 1536 {
		recommended = 2048
	}
	if free, why = t.swapSpace(); free < recommended {
		recommended = free / 256 * 256
	}
	return active, managed, recommended, free, why
}

func (t *hostToolbox) memMiB(key string) uint64 {
	raw, _ := t.read("/proc/meminfo")
	for line := range strings.SplitSeq(string(raw), "\n") {
		f := strings.Fields(line)
		if len(f) >= 2 && f[0] == key {
			kib, _ := strconv.ParseUint(f[1], 10, 64)
			return kib / 1024
		}
	}
	return 0
}

func (t *hostToolbox) swapSpace() (uint64, string) {
	var fs unix.Statfs_t
	if err := unix.Statfs(t.path(filepath.Dir(toolboxSwapPath)), &fs); err != nil {
		return 0, "Dockwell can't check the swap file's disk."
	}
	if fs.Type != unix.EXT4_SUPER_MAGIC && fs.Type != unix.XFS_SUPER_MAGIC {
		return 0, "This disk needs filesystem-specific swap setup. Dockwell supports ext4 and XFS."
	}
	if fs.Type == unix.XFS_SUPER_MAGIC && !t.xfsSwapSupported() {
		return 0, "XFS swap files require Linux 4.18 or later."
	}
	if fs.Bsize <= 0 {
		return 0, "Dockwell can't read the swap file's disk size."
	}
	blockSize := uint64(fs.Bsize)
	available := fs.Bavail * blockSize / swapMib
	reserve := max(uint64(256), fs.Blocks*blockSize/swapMib/20)
	if available <= reserve {
		return 0, "The disk has no room for swap with enough free space left."
	}
	return available - reserve, ""
}

func (t *hostToolbox) xfsSwapSupported() bool {
	raw, err := t.read("/proc/sys/kernel/osrelease")
	parts := strings.SplitN(string(raw), ".", 3)
	if err != nil || len(parts) < 2 {
		return false
	}
	major, majorErr := strconv.Atoi(parts[0])
	minor, minorErr := strconv.Atoi(parts[1])
	return majorErr == nil && minorErr == nil && (major > 4 || major == 4 && minor >= 18)
}

func toolboxSwapIdentity(fi os.FileInfo) string {
	st, _ := fi.Sys().(*syscall.Stat_t)
	return fmt.Sprintf("%d/%d/%d", st.Dev, st.Ino, st.Size)
}

func (t *hostToolbox) interruptedSwap(fi os.FileInfo) bool {
	raw, err := os.ReadFile(filepath.Join(t.dir, swapIntentName))
	return err == nil && slices.Contains(strings.Fields(string(raw)), toolboxSwapIdentity(fi))
}

var toolboxSwapWorkspace = toolboxPattern(`^\.dockwell-swap-[0-9]+$`)

func (t *hostToolbox) removeSwapWorkspaces() error {
	parent := t.path(filepath.Dir(toolboxSwapPath))
	entries, err := os.ReadDir(parent)
	if err != nil {
		return err
	}
	for _, entry := range entries {
		if !entry.IsDir() || !toolboxSwapWorkspace().MatchString(entry.Name()) {
			continue
		}
		info, err := entry.Info()
		if err != nil {
			return err
		}
		if !t.owns(info) || info.Mode().Perm() != 0o700 {
			continue
		}
		dir := filepath.Join(parent, entry.Name())
		if !toolboxSameMount(dir, parent) {
			return fmt.Errorf("%s is a separate mount. Unmount it before planning swap changes", dir)
		}
		files, err := os.ReadDir(dir)
		if err != nil {
			return err
		}
		for _, file := range files {
			name := filepath.Join(dir, file.Name())
			info, err := file.Info()
			if err != nil {
				return err
			}
			if !toolboxRegular(info) || !t.owns(info) || !toolboxSameMount(name, dir) ||
				(file.Name() != "new" && file.Name() != "fstab" && !strings.HasPrefix(file.Name(), ".dockwell-toolbox-")) {
				return fmt.Errorf("%s isn't a Dockwell swap workspace file. Check the workspace before planning", name)
			}
			if err := os.Remove(name); err != nil {
				return err
			}
		}
		if err := os.Remove(dir); err != nil {
			return err
		}
	}
	return nil
}

func toolboxSwapLine() string {
	return toolboxSwapPath + " none swap sw 0 0" + swapMark
}

func (t *hostToolbox) planSwap(edit *toolboxEdit, size uint64) error {
	if size != 0 && (size < 128 || size > 64*1024) {
		return errors.New("choose a swap size from 128 MiB to 64 GiB")
	}
	if err := t.trustedParent(toolboxSwapPath); err != nil {
		return err
	}
	fstab, err := t.file(fstabPath, 0o644)
	if err != nil {
		return err
	}
	var out strings.Builder
	owned := false
	for line := range strings.SplitAfterSeq(string(fstab.old), "\n") {
		f := strings.Fields(line)
		if len(f) > 0 && f[0] == toolboxSwapPath {
			if owned || strings.TrimSuffix(line, "\n") != toolboxSwapLine() {
				return errors.New("the swap file has an fstab entry Dockwell doesn't own")
			}
			owned = true
			continue
		}
		out.WriteString(line)
	}
	old := uint64(0)
	hasFile := false
	if fi, err := os.Lstat(t.path(toolboxSwapPath)); err == nil {
		if !toolboxRegular(fi) || !t.owns(fi) || fi.Mode().Perm() != 0o600 || (!owned && !t.interruptedSwap(fi)) {
			return errors.New("a file already exists at Dockwell's swap path")
		}
		if n := fi.Size(); n > 0 {
			old = uint64(n) / swapMib
		}
		hasFile = true
		edit.extra = []byte(toolboxCandidateInfo(fi))
	} else if !errors.Is(err, os.ErrNotExist) {
		return err
	} else if owned {
		return errors.New("the fstab entry Dockwell owns points to a missing swap file")
	}
	active, _, _, space, why := t.swaps()
	var isActive bool
	for _, s := range active {
		if s.GetPath() != toolboxSwapPath {
			continue
		}
		if !hasFile {
			return errors.New("the active swap file isn't owned by Dockwell")
		}
		isActive = true
		if s.GetUsedMib() > 0 && t.memMiB("MemAvailable:") < s.GetUsedMib()+128 {
			return errors.New("there isn't enough available RAM to turn this swap file off safely")
		}
	}
	if isActive {
		edit.extra = append(edit.extra, "+active"...)
	}
	if err := t.removeSwapWorkspaces(); err != nil {
		return err
	}
	if old == size && (owned || !hasFile) {
		return errors.New("the swap file Dockwell owns already has that size")
	}
	if size > 0 {
		if why != "" {
			return errors.New(why)
		}
		if size > space {
			return fmt.Errorf("there's room for a %d MiB swap file. Choose a smaller size", space)
		}
		if out.Len() > 0 && !strings.HasSuffix(out.String(), "\n") {
			out.WriteByte('\n')
		}
		out.WriteString(toolboxSwapLine() + "\n")
	}
	for _, tool := range []string{"mkswap", "swapon", "swapoff", "findmnt"} {
		if _, ok := t.tools.Find(toolboxTool(tool)); !ok {
			return fmt.Errorf("%s is required to manage this server's swap", tool)
		}
	}
	fstab.next = []byte(out.String())
	toolboxDifference(edit.plan, toolboxSwapPath, fmt.Sprintf("%d MiB", old), fmt.Sprintf("%d MiB", size))
	toolboxDifference(edit.plan, fstabPath, "", "Back up and verify fstab before writing")
	edit.files = []toolboxWrite{fstab}
	edit.apply = func(ctx context.Context) error { return t.changeSwap(ctx, fstab, size, hasFile, isActive) }
	return nil
}

func (t *hostToolbox) changeSwap(ctx context.Context, fstab toolboxWrite, size uint64, owned, active bool) (err error) {
	if size > 64*1024 {
		return errors.New("swap files can be up to 64 GiB")
	}
	target := t.path(toolboxSwapPath)
	intent := filepath.Join(t.dir, swapIntentName)
	workspace, err := os.MkdirTemp(filepath.Dir(target), ".dockwell-swap-")
	if err != nil {
		return err
	}
	defer func() {
		if workspace != "" {
			_ = os.RemoveAll(workspace)
		}
	}()
	newSwap := filepath.Join(workspace, "new")
	if size > 0 {
		f, err := os.OpenFile(newSwap, os.O_CREATE|os.O_EXCL|os.O_RDWR, 0o600) //nolint:gosec // newly created file inside the swap workspace
		if err != nil {
			return err
		}
		allocErr := unix.Fallocate(int(f.Fd()), 0, 0, int64(size)*swapMib)
		closeErr := f.Close()
		if err := errors.Join(allocErr, closeErr); err != nil {
			return err
		}
		if _, err := t.run(ctx, toolboxTool("mkswap"), "--", newSwap); err != nil {
			return err
		}
	}
	toTrack := target
	if size > 0 {
		toTrack = newSwap
	}
	info, err := os.Lstat(toTrack)
	if err != nil {
		return err
	}
	stamps := toolboxSwapIdentity(info)
	if size > 0 && owned {
		previous, err := os.Lstat(target)
		if err != nil {
			return err
		}
		stamps += "\n" + toolboxSwapIdentity(previous)
	}
	if err := toolboxAtomic(intent, []byte(stamps), 0o600); err != nil {
		return err
	}
	backup := fstab.path + ".dockwell-toolbox.bak"
	if _, err := t.file(backup, 0o600); err != nil {
		return err
	}
	if err := toolboxAtomic(t.path(backup), fstab.old, 0o600); err != nil {
		return err
	}
	if active {
		if _, err := t.run(ctx, toolboxTool("swapoff"), "--", target); err != nil {
			return err
		}
	}
	var swapped, installed, newActive, fstabWritten bool
	defer func() {
		if err == nil {
			return
		}
		revert := context.WithoutCancel(ctx)
		if newActive {
			_, stopErr := t.run(revert, toolboxTool("swapoff"), "--", target)
			err = errors.Join(err, stopErr)
			if stopErr != nil {
				// Keep the active swap and retain the old file for recovery.
				workspace = ""
				return
			}
		}
		if swapped {
			back := unix.Renameat2(unix.AT_FDCWD, target, unix.AT_FDCWD, newSwap, unix.RENAME_EXCHANGE)
			err = errors.Join(err, back)
			if back != nil {
				workspace = ""
			}
		} else if installed {
			err = errors.Join(err, os.Remove(target))
		}
		if fstabWritten {
			err = errors.Join(err, t.undo(fstab), t.reloadSwapUnits(revert))
		}
		if active {
			_, backErr := t.run(revert, toolboxTool("swapon"), "--", target)
			err = errors.Join(err, backErr)
		}
	}()
	if size > 0 {
		if owned {
			// Keep a valid file at the fstab path even across a power loss.
			if err := unix.Renameat2(unix.AT_FDCWD, newSwap, unix.AT_FDCWD, target, unix.RENAME_EXCHANGE); err != nil {
				return err
			}
			swapped = true
		} else {
			if err := unix.Renameat2(unix.AT_FDCWD, newSwap, unix.AT_FDCWD, target, unix.RENAME_NOREPLACE); err != nil {
				return err
			}
			installed = true
		}
	}
	staged := filepath.Join(workspace, "fstab")
	if err := toolboxAtomic(staged, fstab.next, 0o600); err != nil {
		return err
	}
	if _, err := t.run(ctx, toolboxTool("findmnt"), "--verify", "--tab-file", staged); err != nil {
		return fmt.Errorf("the proposed fstab didn't pass verification: %w", err)
	}
	if size > 0 {
		if _, err := t.run(ctx, toolboxTool("swapon"), "--", target); err != nil {
			return err
		}
		newActive = true
	}
	fstabWritten = true
	if err := t.write(fstab); err != nil {
		return err
	}
	if err := t.reloadSwapUnits(ctx); err != nil {
		return err
	}
	if size == 0 {
		if err := os.Remove(target); err != nil {
			return err
		}
	}
	_ = os.Remove(intent)
	return nil
}

func (t *hostToolbox) reloadSwapUnits(ctx context.Context) error {
	if !t.systemd() {
		return nil
	}
	_, err := t.run(ctx, toolboxSystem, "daemon-reload")
	return err
}

func toolboxCheckSwappiness(path string, raw []byte, wanted string) error {
	for line := range strings.SplitSeq(string(raw), "\n") {
		line, _, _ = strings.Cut(line, "#")
		key, val, ok := strings.Cut(line, "=")
		key = strings.ReplaceAll(strings.TrimPrefix(strings.TrimSpace(key), "-"), "/", ".")
		if ok && key == "vm.swappiness" && strings.TrimSpace(val) != wanted {
			return fmt.Errorf("%s also sets vm.swappiness. Remove that setting or set it to %s before planning", path, wanted)
		}
	}
	return nil
}

func (t *hostToolbox) planSwappiness(edit *toolboxEdit, value uint32) error {
	if value > 200 {
		return errors.New("choose a swappiness from 0 to 200")
	}
	old, err := t.read("/proc/sys/vm/swappiness")
	if err != nil {
		return err
	}
	wanted := strconv.FormatUint(uint64(value), 10)
	if raw, err := t.read("/etc/sysctl.conf"); err == nil {
		if err := toolboxCheckSwappiness("/etc/sysctl.conf", raw, wanted); err != nil {
			return err
		}
	} else if !errors.Is(err, os.ErrNotExist) {
		return err
	}
	for _, dir := range []string{"/etc/sysctl.d", "/run/sysctl.d", "/usr/local/lib/sysctl.d", "/usr/lib/sysctl.d", "/lib/sysctl.d"} {
		entries, err := os.ReadDir(t.path(dir))
		if err != nil && !errors.Is(err, os.ErrNotExist) {
			return err
		}
		for _, entry := range entries {
			if entry.IsDir() || !strings.HasSuffix(entry.Name(), ".conf") || entry.Name() <= filepath.Base(swappinessPath) {
				continue
			}
			name := filepath.Join(dir, entry.Name())
			raw, err := t.read(name)
			if err != nil {
				return err
			}
			if err := toolboxCheckSwappiness(name, raw, wanted); err != nil {
				return err
			}
		}
	}
	w, err := t.file(swappinessPath, 0o644)
	if err != nil {
		return err
	}
	const marker = "# Managed by Dockwell toolbox\n"
	if !w.wasMissing && !strings.HasPrefix(string(w.old), marker) {
		return errors.New("the sysctl file for Dockwell is already in use")
	}
	w.next = fmt.Appendf(nil, "%svm.swappiness = %d\n", marker, value)
	edit.files = []toolboxWrite{w}
	toolboxDifference(edit.plan, "vm.swappiness", strings.TrimSpace(string(old)), strconv.FormatUint(uint64(value), 10))
	edit.apply = func(ctx context.Context) error {
		_, err := t.run(ctx, toolboxTool("sysctl"), "-w", fmt.Sprintf("vm.swappiness=%d", value))
		return err
	}
	return nil
}
