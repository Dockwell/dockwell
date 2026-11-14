// SPDX-License-Identifier: AGPL-3.0-only

package convert

import (
	"fmt"
	"maps"
	"path"
	"regexp"
	"slices"
	"strings"

	"github.com/dockwell/dockwell/internal/resource"
)

// What 1Panel's scripts do, read as text and never run (ADR 0085). An app's
// scripts/init.sh runs on the server as it installs, upgrade.sh as it upgrades, and
// uninstall.sh as it goes. Most of them make a folder the app binds and give it to
// the user its image runs as, which Dockwell does itself as it makes the app's
// volume (Mount.make_owner); some add a variable to .env that an upgrade fills in
// as any new input, run init.sh again, or remove the app's volumes, which Dockwell's
// own removal asks about. A script reads as those steps only when every line of it
// is one of them, or says nothing, such as an echo; anything else leaves the app
// out, naming the command.

// scriptSteps is what an app's scripts come to.
type scriptSteps struct {
	// owners is whom each folder the app binds is given to, by its path in the
	// app's folder, such as data.
	owners map[string]string
}

var (
	// A line that does nothing Dockwell need do: an echo to the terminal, the shell's
	// own settings, and the frame of an if whose branches are read too.
	scriptNoise = []*regexp.Regexp{
		// To the terminal, naming at most a variable's value, never a command's.
		regexp.MustCompile(`^echo (?:"(?:[^"\\$` + "`" + `]|\$\{?[A-Za-z_][A-Za-z0-9_]*\}?)*"|'[^']*')(?: >&2)?$`),
		regexp.MustCompile(`^set -[euxo]+(?: pipefail)?$`),
		regexp.MustCompile(`^(?:else|fi)$`),
		regexp.MustCompile(`^if \[ -d "?\$\{?[A-Z_]+\}?"? \]; then$`),
		regexp.MustCompile(`^if \[ \$\? -eq 0 \]; then$`),
		// upgrade.sh running init.sh again, whose own lines are read as its own.
		regexp.MustCompile(`^if \[ -f \./scripts/init\.sh \]; then$`),
		regexp.MustCompile(`^(?:(?:bash|sh) )?\./scripts/init\.sh$`),
		// A variable added to .env with its value, which an upgrade does for a new
		// input with its default, and a Compose file's own default stands for.
		regexp.MustCompile(`^if \[\[? -f \./\.env \]?\]; then$`),
		regexp.MustCompile(`^if grep -q ['"]?[A-Z_][A-Z0-9_]*['"]? \./\.env; then$`),
		regexp.MustCompile(`^echo (?:'[A-Z_][A-Z0-9_]*=[^'$` + "`" + `\\]*'|"[A-Z_][A-Z0-9_]*=[^"$` + "`" + `\\]*") >> \./\.env$`),
		// uninstall.sh removing the app with its volumes, which Dockwell's own
		// removal of a project asks about.
		regexp.MustCompile(`^(?:if|elif) docker[ -]compose version >/dev/null 2>&1; then$`),
		regexp.MustCompile(`^docker[ -]compose down --volumes$`),
		regexp.MustCompile(`^exit 127$`),
	}
	scriptChown   = regexp.MustCompile(`^chown (?:-R )?(\d+):(\d+) ([^;&|<>` + "`" + `]+?)(?: >/dev/null 2>&1 \|\| true)?$`)
	scriptMkdir   = regexp.MustCompile(`^mkdir (?:-p )?([^;&|<>` + "`" + `]+)$`)
	scriptAssign  = regexp.MustCompile(`^([A-Z_][A-Z0-9_]*)=("?)([A-Za-z0-9_./-]+)("?)$`)
	scriptCommand = regexp.MustCompile(`^[a-z][a-z0-9_.-]{0,20}$`)
)

// readScripts reads what an app's scripts do, by their names, such as init.sh, or
// says why they can't be read as steps.
func readScripts(scripts map[string]string) (scriptSteps, error) {
	steps := scriptSteps{owners: map[string]string{}}
	for _, name := range slices.Sorted(maps.Keys(scripts)) {
		paths := map[string]string{}
		for raw := range strings.SplitSeq(scripts[name], "\n") {
			line := strings.TrimSpace(raw)
			if line == "" || strings.HasPrefix(line, "#") || slices.ContainsFunc(scriptNoise, func(re *regexp.Regexp) bool { return re.MatchString(line) }) {
				continue
			}
			if m := scriptAssign.FindStringSubmatch(line); len(m) == 5 && m[2] == m[4] {
				paths[m[1]] = m[3]
				continue
			}
			if m := scriptMkdir.FindStringSubmatch(line); m != nil {
				// A folder it binds Dockwell makes as a volume; one it doesn't bind
				// nothing reads.
				if _, err := scriptPaths(name, m[1], paths); err != nil {
					return scriptSteps{}, err
				}
				continue
			}
			if m := scriptChown.FindStringSubmatch(line); m != nil {
				uid, gid, err := resource.ParseOwner(m[1] + ":" + m[2])
				if err != nil {
					return scriptSteps{}, leave(ClassScripts, "its %s gives a folder to %s:%s, which isn't a user Dockwell can give one to", name, m[1], m[2])
				}
				dirs, err := scriptPaths(name, m[3], paths)
				if err != nil {
					return scriptSteps{}, err
				}
				for _, dir := range dirs {
					steps.owners[dir] = resource.FormatOwner(uid, gid)
				}
				continue
			}
			command, _, _ := strings.Cut(line, " ")
			if !scriptCommand.MatchString(command) {
				command = "a command"
			}
			return scriptSteps{}, leave(ClassScripts, "its %s runs %s on the server, which Dockwell can't do as a step of its own, and it runs nothing a store holds", name, command)
		}
	}
	return steps, nil
}

// scriptPaths reads the folders a line of a script names, each in the app's own
// folder, with any variable the script set to a path in its place.
func scriptPaths(script, text string, vars map[string]string) ([]string, error) {
	var out []string
	for word := range strings.FieldsSeq(text) {
		word = strings.Trim(word, `"`)
		if name, ok := strings.CutPrefix(word, "$"); ok {
			name = strings.TrimSuffix(strings.TrimPrefix(name, "{"), "}")
			if word, ok = vars[name]; !ok {
				return nil, leave(ClassScripts, "its %s names a folder by a variable it doesn't set, %s", script, name)
			}
		}
		clean := path.Clean(word)
		if path.IsAbs(clean) || clean == "." || strings.HasPrefix(clean, "..") || strings.ContainsAny(clean, `*?[]{}$\'"`) {
			return nil, leave(ClassScripts, "its %s changes %s, outside the app's own folder", script, fmt.Sprintf("%.60q", word))
		}
		out = append(out, clean)
	}
	return out, nil
}

// ownerOf is whom a folder the app binds is given to: its own owner, or that of the
// folder it's in, as chown -R gives what's inside too.
func (s scriptSteps) ownerOf(rel string) string {
	for p := path.Clean(rel); p != "." && p != "/"; p = path.Dir(p) {
		if o, ok := s.owners[p]; ok {
			return o
		}
	}
	return ""
}
