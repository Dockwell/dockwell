// SPDX-License-Identifier: AGPL-3.0-only

package files

import (
	"os"
	"path"
	"path/filepath"
	"slices"
	"strconv"
	"strings"

	"golang.org/x/sys/unix"

	"github.com/dockwell/dockwell/internal/mounts"
	"github.com/dockwell/dockwell/internal/resource"
)

// A save takes a person's passkey, just now, where a plan's mount would be as good
// as root: resource.SystemPath decides both, so what closes a gap closes it for
// both, and mounting /etc and saving in /etc get the same answer. What's here is
// what that rule can't see: names that let someone in wherever they are, what root
// and those who can become it run as they log in, wherever their home is, and what
// reaches a listed place under another name. See ADR 0032.
var (
	// wayInNames let someone in, or say who may, wherever they are.
	wayInNames = []string{"authorized_keys", "authorized_keys2", ".rhosts", ".shosts", ".k5login", ".k5users"}
	// wayInDirs run as a person, wherever their home is.
	wayInDirs = []string{".config/systemd/user", ".local/share/systemd/user"}
	// startFiles run as a person logs in, or as a shell starts.
	startFiles = []string{
		".bashrc", ".bash_profile", ".bash_login", ".bash_logout", ".profile", ".zshrc", ".zshenv", ".zprofile", ".zlogin", ".cshrc", ".tcshrc", ".kshrc",
	}
	// adminGroups are those who can become root, or as good as.
	adminGroups = []string{"sudo", "wheel", "admin", "docker"}
	// listedFiles are worth finding under another name, as a hard link has one.
	listedFiles = []string{
		"/etc/passwd", "/etc/shadow", "/etc/group", "/etc/gshadow", "/etc/sudoers", "/etc/crontab", "/etc/anacrontab",
		"/etc/ld.so.preload", "/etc/hosts.equiv", "/etc/ssh/sshd_config", "/etc/ssh/ssh_config",
	}
)

// waysIn is what's listed on this server, as of one call: the paths can change
// between calls, and what matters is what's there.
type waysIn struct {
	dirs, files map[identity]bool
	homes       []string // of root and those who can become it
}

// waysIn reads what's listed, by device and inode, as the refused list does, so a
// bind mount of one under another name is still it. A home on a network mount is
// passed over, as a stat there would hold every call while that mount is down.
func (s *Server) waysIn() *waysIn {
	ways := &waysIn{dirs: map[identity]bool{}, files: map[identity]bool{}}
	mark := func(into map[identity]bool, p string) {
		var st unix.Stat_t
		if unix.Stat(p, &st) == nil {
			into[identify(&st)] = true
		}
	}
	for _, p := range s.systemPlaces() {
		mark(ways.dirs, p)
	}
	if s.dataDir != "" {
		mark(ways.dirs, s.dataDir)
	}
	for _, p := range s.listedFiles {
		mark(ways.files, p)
	}
	info := s.mounts()
	for _, home := range s.adminHomes() {
		ways.homes = append(ways.homes, home)
		if probe := filepath.Join(home, "x"); mounts.PassOver(info, probe) != probe {
			continue
		}
		mark(ways.dirs, filepath.Join(home, ".ssh"))
		for _, d := range wayInDirs {
			mark(ways.dirs, filepath.Join(home, d))
		}
		for _, f := range slices.Concat(startFiles, wayInNames) {
			mark(ways.files, filepath.Join(home, f))
		}
	}
	return ways
}

// adminHomes are the homes of root and of who can become it, by what the server's
// accounts say.
func (s *Server) adminHomes() []string {
	passwd, group := s.accounts()
	gids, members := map[string]bool{}, map[string]bool{}
	for line := range strings.Lines(group) {
		f := strings.Split(strings.TrimSpace(line), ":")
		if len(f) < 4 || !slices.Contains(adminGroups, f[0]) {
			continue
		}
		gids[f[2]] = true
		for _, m := range strings.Split(f[3], ",") {
			members[m] = true
		}
	}
	var homes []string
	for line := range strings.Lines(passwd) {
		f := strings.Split(strings.TrimSpace(line), ":")
		if len(f) < 6 || f[5] == "" || f[5] == "/" {
			continue
		}
		if uid, err := strconv.Atoi(f[2]); (err == nil && uid == 0 || gids[f[3]] || members[f[0]]) && !slices.Contains(homes, f[5]) {
			homes = append(homes, f[5])
		}
	}
	return homes
}

// readAccounts reads who the server says there is.
func readAccounts() (passwd, group string) {
	p, _ := os.ReadFile("/etc/passwd")
	g, _ := os.ReadFile("/etc/group")
	return string(p), string(g)
}

// wayIn reports a walk that ended somewhere saving takes a passkey. It reads what
// the walk went through, not how it was spelled.
func (s *Server) wayIn(w *walked) bool {
	from, inVolume := 0, false
	for i := range w.steps {
		if w.steps[i].place == oneVolume {
			from, inVolume = i+1, true
		}
	}
	ways := s.waysIn()
	if listedPath(w.path(), s.dataDir, ways.homes, inVolume) {
		return true
	}
	// What's above a volume is Docker's own, and what's in one is an app's, unless
	// its data is really somewhere listed.
	for i := from; i < len(w.steps)-1; i++ {
		if ways.dirs[w.steps[i].id] {
			return true
		}
	}
	return ways.files[identify(&w.st)]
}

// listedPath reports what's a way in by its path alone.
func listedPath(where, dataDir string, homes []string, inVolume bool) bool {
	base := path.Base(where)
	switch {
	case slices.Contains(wayInNames, base), strings.HasPrefix(base, "sudoers"):
		return true
	case slices.ContainsFunc(wayInDirs, func(d string) bool { return strings.Contains(where, "/"+d+"/") }):
		return true
	case slices.Contains(startFiles, base) && slices.Contains(homes, path.Dir(where)):
		return true
	case dataDir != "" && mounts.Holds(dataDir, where):
		return true
	case inVolume:
		return false // Docker's volumes are on the rule's list, and they hold apps' files
	}
	return resource.SystemPath(where, false)
}
