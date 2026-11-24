// SPDX-License-Identifier: AGPL-3.0-only

package agent

import (
	"context"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"time"

	"github.com/dockwell/dockwell/internal/agent/access"
)

// The service also runs before sshd on a later boot. The lock and wait ID
// are shared with the agent, so a stale timer cannot undo a kept change.
const sshRevertScript = `#!/bin/sh
set -eu
dir=$1
mode=$2
system=$8
if [ "$mode" = boot ]; then
  cmp -s /proc/sys/kernel/random/boot_id "$dir/ssh-boot-id" && exit 0
fi
exec 9>"$dir/ssh.lock"
if [ "$mode" = boot ]; then
  flock -n 9 || exit 0
  wait=$(cat "$dir/ssh-wait-id" 2>/dev/null) || exit 0
else
  flock 9
  wait=$3
  [ "$(cat "$dir/ssh-wait-id" 2>/dev/null)" = "$wait" ] || exit 0
fi
cleanup() {
  rm -f "$dir/ssh-wait-id" "$dir/ssh-revert.sh" "$system/dockwell-ssh-restore.service"
  for unit in ssh.service sshd.service fail2ban.service; do
    rm -f "$system/$unit.d/dockwell-access-restore.conf"
  done
  systemctl daemon-reload
}
needle='"wait": "'"$wait"'"'
if ! grep -Fq -- "$needle" "$dir/waiting.json" 2>/dev/null ||
   grep -Fq -- "$needle" "$dir/confirmed.json" 2>/dev/null; then
  cleanup
  exit 0
fi
restore() {
  if [ -s "$1" ]; then
    tmp=$(mktemp "${2%/*}/.dockwell-restore-XXXXXX")
    trap 'rm -f "$tmp"' EXIT
    cat "$1" > "$tmp"
    mv -fT "$tmp" "$2"
  else
    rm -f "$2"
  fi
}
restore "$dir/ssh-previous" "$4"
if [ -f "$dir/ssh-jail-previous" ]; then
  restore "$dir/ssh-jail-previous" "$6"
fi
if [ "$mode" = timer ]; then
  if [ -n "$7" ] && [ -f "$dir/ssh-jail-previous" ] && "$7" ping >/dev/null 2>&1; then
    "$7" reload --restart sshd || :
  fi
  systemctl reload-or-restart "$5"
fi
cleanup
`

func (s *sshSettings) Arm(ctx context.Context, dir, wait string, delay time.Duration, trial *access.SSHChange) error {
	dir, err := filepath.Abs(dir)
	if err != nil {
		return err
	}
	if err := s.checkSSHRestoreDir(dir); err != nil {
		return err
	}
	bootID, err := fs.ReadFile(s.reader.proc(), "sys/kernel/random/boot_id")
	if err != nil {
		return fmt.Errorf("can't identify this boot for SSH rollback: %w", err)
	}
	for name, body := range map[string]string{
		"ssh-revert.sh": sshRevertScript, "ssh-boot-id": string(bootID), "ssh-wait-id": wait,
	} {
		if err := writeSSHOwnedFile(filepath.Join(dir, name), body); err != nil {
			return err
		}
	}
	if err := writeSSHRollbackCopy(filepath.Join(dir, "ssh-previous"), trial.OldFile); err != nil {
		return err
	}
	jailBackup := filepath.Join(dir, "ssh-jail-previous")
	if trial.Jail == nil {
		if err := writeSSHOwnedFile(jailBackup, ""); err != nil {
			return err
		}
	} else if err := writeSSHRollbackCopy(jailBackup, trial.Jail.Old); err != nil {
		return err
	}
	if err := s.armSSHBootRestore(ctx, dir, trial); err != nil {
		return fmt.Errorf("can't install the SSH boot rollback: %w", err)
	}
	return s.Rearm(ctx, dir, wait, delay, trial)
}

// systemd runs the helper as root. Every parent of the helper has to prevent
// other users from replacing either it or a directory on its path.
func (s *sshSettings) checkSSHRestoreDir(dir string) error {
	if strings.ContainsAny(dir, "\r\n") {
		return errors.New("the agent data directory for SSH rollback must fit on one line")
	}
	root := s.reader.rootPath("/")
	for at := filepath.Clean(dir); ; at = filepath.Dir(at) {
		info, err := os.Lstat(at)
		if err != nil || !info.IsDir() || !sshOwnership(info, s.reader.tools.Root) || info.Mode().Perm()&0o022 != 0 {
			return fmt.Errorf("SSH rollback needs an agent data directory whose parents are writable only by root: %s", at)
		}
		if at == root || at == filepath.Dir(at) {
			return nil
		}
	}
}

// An empty backup records a previously absent file. No jail backup means this
// trial doesn't change fail2ban.
func writeSSHRollbackCopy(name, body string) error {
	if body != "" {
		return writeSSHOwnedFile(name, body)
	}
	f, err := os.OpenFile(name, os.O_CREATE|os.O_WRONLY|os.O_TRUNC, 0o600) //nolint:gosec // agent-owned rollback directory
	if err != nil {
		return err
	}
	if err := f.Sync(); err != nil {
		_ = f.Close()
		return err
	}
	return f.Close()
}

func (s *sshSettings) sshRestoreArgs(dir, mode, wait string, trial *access.SSHChange) []string {
	client, _ := s.reader.tools.Find(fail2banClient)
	return []string{
		"/bin/sh", filepath.Join(dir, "ssh-revert.sh"), dir, mode, wait, s.reader.rootPath(sshDropInPath), trial.Unit,
		s.reader.rootPath(sshJailPath), client,
		s.reader.rootPath("/etc/systemd/system"),
	}
}

func (s *sshSettings) Rearm(ctx context.Context, dir, wait string, delay time.Duration, trial *access.SSHChange) error {
	dir, err := filepath.Abs(dir)
	if err != nil {
		return err
	}
	unit := "dockwell-ssh-revert-" + strconv.FormatInt(time.Now().UnixNano(), 36)
	args := []string{
		"--quiet", "--collect", "--unit=" + unit, "--on-active=" + strconv.FormatInt(int64(delay/time.Second), 10),
		"--timer-property=AccuracySec=1s", "--property=ConditionPathExists=" + filepath.Join(dir, "ssh-revert.sh"),
		"--description=Restore OpenSSH access after an unconfirmed Dockwell change", "--",
	}
	for _, arg := range s.sshRestoreArgs(dir, "timer", wait, trial) {
		args = append(args, strings.ReplaceAll(arg, "$", "$$"))
	}
	if _, err := s.reader.sshTool(ctx, sshSystemdRun, args...); err != nil {
		return fmt.Errorf("can't arm the SSH rollback: %w", err)
	}
	return nil
}

const (
	sshRestoreUnit   = "dockwell-ssh-restore.service"
	sshRestoreDropIn = "dockwell-access-restore.conf"
)

func (s *sshSettings) armSSHBootRestore(ctx context.Context, dir string, trial *access.SSHChange) error {
	r := s.reader
	system := r.rootPath("/etc/systemd/system")
	quote := func(value string) string { return strconv.Quote(strings.ReplaceAll(value, "%", "%%")) }
	args := s.sshRestoreArgs(dir, "boot", "", trial)
	for i := 1; i < len(args); i++ {
		args[i] = quote(args[i])
	}
	// ExecStart's colon prefix disables systemd's environment substitution.
	service := "[Unit]\nDescription=Restore unconfirmed Dockwell SSH settings on boot\nRequiresMountsFor=" + quote(dir) +
		"\nConditionPathExists=" + strings.ReplaceAll(filepath.Join(dir, "ssh-wait-id"), "%", "%%") +
		"\n[Service]\nType=oneshot\nExecStart=:" + strings.Join(args, " ") + "\n"
	if err := writeSSHOwnedFile(filepath.Join(system, sshRestoreUnit), service); err != nil {
		return err
	}
	dependents := []string{trial.Unit}
	if trial.Jail != nil {
		dependents = append(dependents, "fail2ban.service")
	}
	for _, dependent := range dependents {
		folder := filepath.Join(system, dependent+".d")
		if err := os.MkdirAll(folder, 0o750); err != nil {
			return err
		}
		dropIn := "[Unit]\nWants=" + sshRestoreUnit + "\nAfter=" + sshRestoreUnit + "\n"
		if err := writeSSHOwnedFile(filepath.Join(folder, sshRestoreDropIn), dropIn); err != nil {
			return err
		}
	}
	_, err := r.sshTool(ctx, sshTools["systemctl"], "daemon-reload")
	return err
}

func (s *sshSettings) Disarm(ctx context.Context, dir string) error {
	system := s.reader.rootPath("/etc/systemd/system")
	paths := []string{filepath.Join(dir, "ssh-wait-id"), filepath.Join(dir, "ssh-revert.sh"), filepath.Join(system, sshRestoreUnit)}
	for _, unit := range []string{"ssh.service", "sshd.service", "fail2ban.service"} {
		paths = append(paths, filepath.Join(system, unit+".d", sshRestoreDropIn))
	}
	var result error
	var removedUnit bool
	for i, path := range paths {
		err := os.Remove(path)
		removedUnit = removedUnit || (i >= 2 && err == nil)
		if err != nil && !errors.Is(err, fs.ErrNotExist) {
			result = errors.Join(result, err)
		}
	}
	if removedUnit {
		_, err := s.reader.sshTool(ctx, sshTools["systemctl"], "daemon-reload")
		result = errors.Join(result, err)
	}
	return result
}
