// SPDX-License-Identifier: AGPL-3.0-only

// Package systemtest stands in for a systemd server's systemctl, journalctl and
// sshd in tests, with units whose state the actions move (ADR 0082). Nothing here
// runs the machine's own tools.
package systemtest

import (
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
)

// Unit is a unit the stand keeps, by its properties as systemctl show says them.
type Unit struct {
	// Props are its properties, such as Id, ActiveState and UnitFileState.
	Props map[string]string
	// Fails says starting it fails, as a unit with a bad config does.
	Fails bool
	// Journal are its journal entries, newest first, as journalctl -o json prints
	// them.
	Journal []string
}

// Stand is a server's files with stand-ins for its init's tools.
type Stand struct {
	// Root is the server's files, for a Reader's Root and its runner's.
	Root string
}

// Calls are the stand-ins' runs so far, each its tool and arguments.
func (s *Stand) Calls() []string {
	b, _ := os.ReadFile(filepath.Join(s.Root, "calls"))
	return slices.DeleteFunc(strings.Split(strings.TrimSpace(string(b)), "\n"), func(l string) bool { return l == "" })
}

// Prop reads a unit's property as the stand keeps it now.
func (s *Stand) Prop(t *testing.T, id, key string) string {
	t.Helper()
	b, err := os.ReadFile(filepath.Join(s.Root, "state", id)) //nolint:gosec // G304: the stand's own file, by a unit the test named
	if err != nil {
		t.Fatal(err)
	}
	for _, line := range strings.Split(string(b), "\n") {
		if k, v, ok := strings.Cut(line, "="); ok && k == key {
			return v
		}
	}
	return ""
}

// Own says the agent runs in unit, as /proc/self/cgroup says it.
func (s *Stand) Own(t *testing.T, unit string) {
	t.Helper()
	write(t, filepath.Join(s.Root, "proc", "self", "cgroup"), "0::/system.slice/"+unit+"\n", 0o644)
}

// NoSSHD takes the stand's sshd away, as on a server where it's somewhere else.
func (s *Stand) NoSSHD(t *testing.T) {
	t.Helper()
	if err := os.Remove(filepath.Join(s.Root, "usr", "sbin", "sshd")); err != nil {
		t.Fatal(err)
	}
}

// IfUp puts a stand-in ifup on the stand, whose --no-act fails with line unless it's empty.
func (s *Stand) IfUp(t *testing.T, line string) {
	t.Helper()
	script := "#!/bin/sh\necho \"ifup $*\" >> '" + filepath.Join(s.Root, "calls") + "'\n"
	if line != "" {
		script += "echo '" + line + "' >&2\nexit 1\n"
	}
	write(t, filepath.Join(s.Root, "sbin", "ifup"), script, 0o755)
}

// SSHDFails has the stand's sshd -t fail with line.
func (s *Stand) SSHDFails(t *testing.T, line string) {
	t.Helper()
	write(t, filepath.Join(s.Root, "usr", "sbin", "sshd"), "#!/bin/sh\necho \"sshd $*\" >> '"+filepath.Join(s.Root, "calls")+"'\necho '"+line+"' >&2\nexit 255\n", 0o755)
}

func write(t *testing.T, p, content string, mode os.FileMode) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(p), 0o750); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(p, []byte(content), mode); err != nil {
		t.Fatal(err)
	}
}

// New makes a systemd stand with the units given. An alias in a unit's Names
// reads as the unit.
func New(t *testing.T, units ...Unit) *Stand {
	t.Helper()
	root := t.TempDir()
	s := &Stand{Root: root}
	state := filepath.Join(root, "state")
	write(t, filepath.Join(root, "run", "systemd", "system", ".keep"), "", 0o644)
	write(t, filepath.Join(root, "run", "systemd", "journal", ".keep"), "", 0o644)
	for _, u := range units {
		id := u.Props["Id"]
		var b strings.Builder
		for _, k := range sortedKeys(u.Props) {
			b.WriteString(k + "=" + u.Props[k] + "\n")
		}
		write(t, filepath.Join(state, id), b.String(), 0o644)
		for _, alias := range strings.Fields(u.Props["Names"]) {
			if alias != id {
				write(t, filepath.Join(state, "alias", alias), id, 0o644)
			}
		}
		if u.Fails {
			write(t, filepath.Join(state, "fails", id), "", 0o644)
		}
		write(t, filepath.Join(state, "journal", id), strings.Join(u.Journal, "\n")+"\n", 0o644)
	}
	calls := filepath.Join(root, "calls")
	write(t, filepath.Join(root, "usr", "bin", "systemctl"), `#!/bin/sh
echo "systemctl $*" >> '`+calls+`'
state='`+state+`'
verb="$1"
shift
names=""
props=""
seen=""
for a in "$@"; do
  case "$a" in
    --property=*) props="${a#--property=}" ;;
    --*) ;;
    *) if [ -n "$seen" ]; then names="$names $a"; fi ;;
  esac
  [ "$a" = "--" ] && seen=1
done
resolve() {
  if [ -f "$state/alias/$1" ]; then cat "$state/alias/$1"; else echo "$1"; fi
}
set_prop() {
  f="$state/$1"
  if grep -q "^$2=" "$f"; then sed -i "s|^$2=.*|$2=$3|" "$f"; else echo "$2=$3" >> "$f"; fi
}
case "$verb" in
  list-units|list-unit-files)
    for f in "$state"/*; do [ -f "$f" ] && echo "$(basename "$f") x"; done ;;
  show)
    first=1
    for n in $names; do
      id=$(resolve "$n")
      [ "$first" = 1 ] || echo
      first=0
      if [ -f "$state/$id" ]; then cat "$state/$id"; else printf 'Id=%s\nLoadState=not-found\nActiveState=inactive\n' "$n"; fi
    done ;;
  start|restart|reload)
    for n in $names; do
      id=$(resolve "$n")
      if [ -f "$state/fails/$id" ]; then
        set_prop "$id" ActiveState failed; set_prop "$id" SubState failed; set_prop "$id" Result exit-code
        echo "Job for $id failed because the control process exited with error code." >&2
        exit 1
      fi
      set_prop "$id" ActiveState active; set_prop "$id" SubState running
    done ;;
  stop)
    for n in $names; do id=$(resolve "$n"); set_prop "$id" ActiveState inactive; set_prop "$id" SubState dead; done ;;
  enable)
    for n in $names; do id=$(resolve "$n"); set_prop "$id" UnitFileState enabled; echo "Created symlink /etc/systemd/system/multi-user.target.wants/$id." >&2; done ;;
  disable)
    for n in $names; do id=$(resolve "$n"); set_prop "$id" UnitFileState disabled; echo "Removed /etc/systemd/system/multi-user.target.wants/$id." >&2; done ;;
  mask)
    for n in $names; do id=$(resolve "$n"); set_prop "$id" UnitFileState masked; set_prop "$id" LoadState masked; done ;;
  unmask)
    for n in $names; do id=$(resolve "$n"); set_prop "$id" UnitFileState disabled; set_prop "$id" LoadState loaded; done ;;
esac
`, 0o755)
	write(t, filepath.Join(root, "usr", "bin", "journalctl"), `#!/bin/sh
echo "journalctl $*" >> '`+calls+`'
state='`+state+`'
unit=""
for a in "$@"; do
  case "$a" in
    --unit=*) unit="${a#--unit=}" ;;
  esac
done
if [ -n "$unit" ]; then
  [ -f "$state/alias/$unit" ] && unit=$(cat "$state/alias/$unit")
  cat "$state/journal/$unit" 2>/dev/null
else
  cat "$state"/journal/* 2>/dev/null
fi
`, 0o755)
	write(t, filepath.Join(root, "usr", "sbin", "sshd"), "#!/bin/sh\necho \"sshd $*\" >> '"+calls+"'\n", 0o755)
	return s
}

func sortedKeys(m map[string]string) []string {
	keys := make([]string, 0, len(m))
	for k := range m {
		keys = append(keys, k)
	}
	slices.Sort(keys)
	// Id first, as systemctl prints it.
	if i := slices.Index(keys, "Id"); i > 0 {
		keys = append([]string{"Id"}, slices.Delete(keys, i, i+1)...)
	}
	return keys
}

// Running is a running, enabled service's properties.
func Running(id string, more ...string) map[string]string {
	p := map[string]string{
		"Id": id, "Names": id, "Description": id, "LoadState": "loaded", "ActiveState": "active", "SubState": "running",
		"UnitFileState": "enabled", "CanStart": "yes", "CanStop": "yes", "CanReload": "no", "Result": "success",
	}
	for i := 0; i+1 < len(more); i += 2 {
		p[more[i]] = more[i+1]
	}
	return p
}

// Stopped is a stopped, disabled service's properties.
func Stopped(id string, more ...string) map[string]string {
	p := Running(id, "ActiveState", "inactive", "SubState", "dead", "UnitFileState", "disabled")
	for i := 0; i+1 < len(more); i += 2 {
		p[more[i]] = more[i+1]
	}
	return p
}

// Entry is one journal entry as journalctl -o json prints it.
func Entry(cursor, unit, pid, message string) string {
	return `{"__CURSOR":"` + cursor + `","__REALTIME_TIMESTAMP":"1762963200000000","_SYSTEMD_UNIT":"` + unit + `","_PID":"` + pid + `","PRIORITY":"6","MESSAGE":"` + message + `"}`
}
