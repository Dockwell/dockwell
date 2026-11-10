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
	// homes are those of root and of who can become it, as the accounts write
	// them and with every link followed, as a walk follows each.
	homes []string
	// adminHomes are those homes by what they are, so a start file in one counts
	// whatever path reaches it.
	adminHomes map[identity]bool
	// everyone's are every account's home, both ways, which a folder made, moved or
	// deleted counts for as a whole, as a home holds its owner's way in, and
	// everyoneIDs are those homes by what they are.
	everyone    []string
	everyoneIDs map[identity]bool
	// adminDirs are the folders in an admin's home that let someone in, .ssh and
	// each of wayInDirs, both ways, whether they're there yet or not, which a folder
	// counts for when it is or holds one.
	adminDirs []string
	// homeRoot is where the home root leads, when that's somewhere else, as /home
	// does on an ostree system or where it moved to a data disk.
	homeRoot string
	// written are /home and every account's home by what they are, each with the
	// path the rule goes by, so a walk through another name for one, as a bind
	// mount gives it, is spelled as the rule knows it too (asWritten).
	written map[identity]string
}

// bothWays is p as written, cleaned, and with every link followed as far as it's
// there, unless it's on a mount that can stop answering, as following it there
// would hold every call while it's down.
func bothWays(info, p string) []string {
	p = path.Clean(p)
	if probe := path.Join(p, "x"); mounts.PassOver(info, probe) != probe {
		return []string{p}
	}
	if to := resolveLoose(p); to != p {
		return []string{p, to}
	}
	return []string{p}
}

// resolveLoose follows every link in p as far as it's there, and keeps the rest as
// written: a folder of an admin's home counts before it's made.
func resolveLoose(p string) string {
	if to, err := filepath.EvalSymlinks(p); err == nil {
		return to
	}
	dir, base := path.Split(p)
	dir = path.Clean(dir)
	if dir == p || base == "" {
		return p
	}
	return path.Join(resolveLoose(dir), base)
}

// waysIn reads what's listed, by device and inode, as the refused list does, so a
// bind mount of one under another name is still it. A home on a network mount is
// passed over, as a stat there would hold every call while that mount is down.
func (s *Server) waysIn() *waysIn {
	ways := &waysIn{
		dirs: map[identity]bool{}, files: map[identity]bool{}, adminHomes: map[identity]bool{}, everyoneIDs: map[identity]bool{},
		written: map[identity]string{},
	}
	mark := func(into map[identity]bool, p string) {
		if id, ok := s.identityOf(p); ok {
			into[id] = true
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
	if root := resolveLoose(s.homeRoot); root != path.Clean(s.homeRoot) {
		ways.homeRoot = root
	}
	if id, ok := s.identityOf(s.homeRoot); ok {
		ways.written[id] = "/home"
	}
	for _, home := range s.everyonesHomes() {
		ways.everyone = append(ways.everyone, bothWays(info, home)...)
		if probe := filepath.Join(home, "x"); mounts.PassOver(info, probe) == probe {
			mark(ways.everyoneIDs, home)
			if id, ok := s.identityOf(home); ok {
				ways.written[id] = path.Clean(home)
			}
		}
	}
	for _, home := range s.adminHomes() {
		ways.homes = append(ways.homes, bothWays(info, home)...)
		for _, d := range append([]string{".ssh"}, wayInDirs...) {
			ways.adminDirs = append(ways.adminDirs, bothWays(info, filepath.Join(home, d))...)
		}
		if probe := filepath.Join(home, "x"); mounts.PassOver(info, probe) != probe {
			continue
		}
		mark(ways.adminHomes, home)
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
		home := path.Clean(f[5])
		if uid, err := strconv.Atoi(f[2]); (err == nil && uid == 0 || gids[f[3]] || members[f[0]]) && home != "/" && !slices.Contains(homes, home) {
			homes = append(homes, home)
		}
	}
	return homes
}

// everyonesHomes are the homes of every account on the server, but those that are
// no home at all.
func (s *Server) everyonesHomes() []string {
	passwd, _ := s.accounts()
	var homes []string
	for line := range strings.Lines(passwd) {
		f := strings.Split(strings.TrimSpace(line), ":")
		if len(f) < 6 || !strings.HasPrefix(f[5], "/") {
			continue
		}
		switch home := path.Clean(f[5]); home {
		case "/", "/nonexistent", "/dev/null":
		default:
			if !slices.Contains(homes, home) {
				homes = append(homes, home)
			}
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
	if ways.listed(w.path(), s.dataDir, inVolume) {
		return true
	}
	for _, as := range ways.asWritten(w.steps[:len(w.steps)-1], "/"+w.last().name) {
		if ways.listed(as, s.dataDir, inVolume) {
			return true
		}
	}
	// What's above a volume is Docker's own, and what's in one is an app's, unless
	// its data is really somewhere listed.
	for i := from; i < len(w.steps)-1; i++ {
		if ways.dirs[w.steps[i].id] {
			return true
		}
	}
	if len(w.steps) > 1 && slices.Contains(startFiles, w.last().name) && ways.adminHomes[w.steps[len(w.steps)-2].id] {
		return true
	}
	return ways.files[identify(&w.st)]
}

// asWritten is where steps lead, and then tail, spelled from each place a step is
// by what it is that the rule knows by another path: /home, or an account's home,
// reached by a bind mount's other name, as /mnt/data/home is when it's bound onto
// /home. A link is followed to where it leads, which the rule's paths take in too
// (bothWays); a bind mount isn't a link, so it's known by what it is.
func (ways *waysIn) asWritten(steps []step, tail string) []string {
	var out []string
	for i := range steps {
		place, ok := ways.written[steps[i].id]
		if !ok {
			continue
		}
		var b strings.Builder
		b.WriteString(place)
		for _, s := range steps[i+1:] {
			b.WriteString("/")
			b.WriteString(s.name)
		}
		b.WriteString(tail)
		out = append(out, path.Clean(b.String()))
	}
	return out
}

// listed reports what's a way in by its path, as written or as the home root
// reads where it leads somewhere else: the rule goes by /home.
func (ways *waysIn) listed(where, dataDir string, inVolume bool) bool {
	if listedPath(where, dataDir, ways.homes, inVolume) {
		return true
	}
	if root := ways.homeRoot; root != "" && mounts.Holds(root, where) {
		return listedPath(path.Join("/home", strings.TrimPrefix(where, root)), dataDir, ways.homes, inVolume)
	}
	return false
}

// listedPath reports what's a way in by its path alone.
func listedPath(where, dataDir string, homes []string, inVolume bool) bool {
	base := path.Base(where)
	switch {
	case slices.Contains(wayInNames, base), strings.HasPrefix(base, "sudoers"):
		return true
	case slices.ContainsFunc(wayInDirs, func(d string) bool { return strings.Contains(where+"/", "/"+d+"/") }):
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

// statIdentity says what's at p, following links, by device and inode.
func statIdentity(p string) (identity, bool) {
	var st unix.Stat_t
	if unix.Stat(p, &st) != nil {
		return identity{}, false
	}
	return identify(&st), true
}
