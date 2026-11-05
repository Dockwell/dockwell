// SPDX-License-Identifier: AGPL-3.0-only

package compose

import (
	"fmt"
	"maps"
	"strings"
	"unicode"
)

// MaxEnv is the largest .env file Dockwell reads, far past any real one: as much
// as the CLI reads of a file that may hold secrets.
const MaxEnv = 64 << 10

// readEnv reads the text of an .env file as docker compose does, and returns its
// variables with given over them, as a shell's win over a file's. A value may
// name a variable set earlier in the file, or given. compose-go leaves one that
// has no value blank, so a password with a $ in it would quietly be another
// password: this refuses it, as interpolate refuses one in a Compose file. A refusal
// names a line and never what is on it: an .env holds secrets, and what comes after
// a $ in a password reads as a variable's name, so a name is no safer than a value.
// A refusal goes back to the caller and into the control plane's log.
func readEnv(text string, given map[string]string) (map[string]string, error) {
	if text == "" {
		return given, nil
	}
	in := &interpolator{variables: maps.Clone(given), missing: map[string]bool{}}
	if in.variables == nil {
		in.variables = map[string]string{}
	}
	found := map[string]string{}
	r := &envReader{rest: strings.TrimPrefix(text, "\xef\xbb\xbf"), line: 1}
	for {
		key, inherited, err := r.key()
		if err != nil {
			return nil, err
		}
		if key == "" {
			break
		}
		if inherited {
			// A name alone takes its value from what is given, or has none.
			continue
		}
		value, err := r.value(in)
		if err != nil {
			return nil, err
		}
		found[key], in.variables[key] = value, value
	}
	maps.Copy(found, given)
	if err := checkVariables(found); err != nil {
		return nil, err
	}
	return found, nil
}

// envReader reads an .env file, from the front.
type envReader struct {
	rest string
	line int
}

// isSpace is the white space that doesn't end a line.
func isSpace(r rune) bool { return r != '\n' && unicode.IsSpace(r) }

func (r *envReader) advance(n int) {
	r.line += strings.Count(r.rest[:n], "\n")
	r.rest = r.rest[n:]
}

// key reads up to the next value's name, past comments and white space, and
// reports a name with no value after it. It is empty at the end of the file.
func (r *envReader) key() (string, bool, error) {
	for {
		r.advance(len(r.rest) - len(strings.TrimLeftFunc(r.rest, unicode.IsSpace)))
		if !strings.HasPrefix(r.rest, "#") {
			break
		}
		end := strings.IndexByte(r.rest, '\n')
		if end < 0 {
			return "", false, nil
		}
		r.advance(end)
	}
	if r.rest == "" {
		return "", false, nil
	}
	if after, ok := strings.CutPrefix(r.rest, "export"); ok && after != "" && isSpace(rune(after[0])) {
		r.advance(len(r.rest) - len(strings.TrimLeftFunc(after, isSpace)))
	}
	end := strings.IndexAny(r.rest, "=:\n")
	if end < 0 {
		end = len(r.rest)
	}
	key := strings.TrimRightFunc(r.rest[:end], unicode.IsSpace)
	for _, c := range key {
		if !unicode.IsLetter(c) && !unicode.IsNumber(c) && !strings.ContainsRune("_.-[]", c) {
			return "", false, invalid("line %d of the .env file has something other than a name before its =. Write it as NAME=value", r.line)
		}
	}
	if key == "" {
		return "", false, invalid("line %d of the .env file has a value with no name. Write it as NAME=value", r.line)
	}
	if end == len(r.rest) || r.rest[end] == '\n' {
		r.advance(end)
		return key, true, nil
	}
	r.advance(end + 1)
	r.advance(len(r.rest) - len(strings.TrimLeftFunc(r.rest, isSpace)))
	return key, false, nil
}

// value reads the value that comes next, and resolves what it names.
func (r *envReader) value(in *interpolator) (string, error) {
	line := r.line
	switch {
	case strings.HasPrefix(r.rest, `"`), strings.HasPrefix(r.rest, "'"):
		quote := r.rest[0]
		raw, n, ok := quotedValue(r.rest, quote)
		if !ok {
			return "", invalid("line %d of the .env file has a quote that doesn't close. Close it", line)
		}
		r.advance(n)
		if quote == '\'' {
			return raw, nil
		}
		return expandEnv(in, escapes(raw), line)
	}
	end := strings.IndexByte(r.rest, '\n')
	if end < 0 {
		end = len(r.rest)
	}
	raw, _, _ := strings.Cut(r.rest[:end], " #")
	r.advance(end)
	return expandEnv(in, strings.TrimRightFunc(raw, unicode.IsSpace), line)
}

// expandEnv resolves the variables a value names. What goes wrong says where, and
// how to write what the value meant, and nothing of what the line holds, the name
// after its $ included.
func expandEnv(in *interpolator, value string, line int) (string, error) {
	out, err := in.substitute(place{owner: "the .env file"}, value)
	if len(in.missing) > 0 {
		return "", invalid("line %d of the .env file has a $ before a name with no value. To write a $ of your own, use $$, or put the value in single quotes. To use a variable, set it earlier in the file", line)
	}
	if in.expanded > MaxExpansion {
		return "", invalid("line %d of the .env file takes the variables past %d MiB once they are expanded, which Dockwell doesn't read. Use each long value less often, or shorten it", line, MaxExpansion>>20)
	}
	if err != nil {
		return "", invalid("line %d of the .env file has a $ that Dockwell can't read as a variable. To write a $ of your own, use $$, or put the value in single quotes", line)
	}
	return out, nil
}

// quotedValue reads a value in quotes at the front of s, and says how many bytes that
// took. It keeps each escape but one of the quote itself for what comes after.
func quotedValue(s string, quote byte) (string, int, bool) {
	var b strings.Builder
	escaped := false
	for i := 1; i < len(s); i++ {
		c := s[i]
		switch {
		case c == quote && !escaped:
			return b.String(), i + 1, true
		case c == quote:
			escaped = false
			b.WriteByte(c)
		case c == '\\' && !escaped:
			escaped = true
		case escaped:
			escaped = false
			b.WriteByte('\\')
			b.WriteByte(c)
		default:
			b.WriteByte(c)
		}
	}
	return "", 0, false
}

// escapes reads the escapes double quotes take, as docker compose does: \n, \t
// and so on, and \$ for a $ that starts nothing, which is $$ to what follows.
func escapes(s string) string {
	if !strings.Contains(s, `\`) {
		return s
	}
	var b strings.Builder
	for i := 0; i < len(s); i++ {
		if s[i] != '\\' || i+1 == len(s) {
			b.WriteByte(s[i])
			continue
		}
		i++
		switch s[i] {
		case 'a':
			b.WriteByte('\a')
		case 'b':
			b.WriteByte('\b')
		case 'c':
			// docker compose reads \c as nothing it knows, and keeps it.
			b.WriteString(`\c`)
		case 'f':
			b.WriteByte('\f')
		case 'n':
			b.WriteByte('\n')
		case 'r':
			b.WriteByte('\r')
		case 't':
			b.WriteByte('\t')
		case 'v':
			b.WriteByte('\v')
		case '$':
			b.WriteString("$$")
		case '"':
			b.WriteByte('"')
		case '\\':
			b.WriteByte('\\')
		default:
			b.WriteByte('\\')
			b.WriteByte(s[i])
		}
	}
	return b.String()
}

// checkDirectory refuses a directory for a file's relative paths that isn't one
// a plan can show as it is: what comes of it is a server path, and goes through
// all a server path does. It must say what it means, as one must.
func checkDirectory(dir string) error {
	switch {
	case dir == "":
		return nil
	case len(dir) > 4<<10:
		return invalid("the directory is %d KiB long, which no path on a server is. Give the directory the Compose file is in", len(dir)>>10)
	case !strings.HasPrefix(dir, "/"):
		return invalid("the directory %q isn't an absolute path, and a server has nowhere else to start from. Give the one the Compose file is in, such as /srv/app", dir)
	case strings.ContainsAny(dir, ":") || strings.IndexFunc(dir, unicode.IsControl) >= 0:
		return invalid("the directory %q has a colon or a control character in it, which Docker can't take. Use a path without one", dir)
	}
	for _, part := range strings.Split(dir, "/") {
		if part == "." || part == ".." {
			return invalid("the directory %q has %s in it, which a server reads through symlinks, so Dockwell can't tell where it leads. Write it without", dir, part)
		}
	}
	return nil
}

// checkDeploy refuses what a service's deploy holds beyond its limits, by name.
func checkDeploy(what string, svc map[string]any) error {
	deploy, ok := svc["deploy"].(map[string]any)
	if !ok {
		return nil
	}
	if err := check(what+"'s deploy", deploy, deployKeys); err != nil {
		return err
	}
	resources, ok := deploy["resources"].(map[string]any)
	if !ok {
		return nil
	}
	if err := check(what+"'s deploy.resources", resources, limitsKeys); err != nil {
		return err
	}
	for _, section := range []string{"limits", "reservations"} {
		if m, ok := resources[section].(map[string]any); ok {
			if err := check(fmt.Sprintf("%s's deploy.resources.%s", what, section), m, amountKeys); err != nil {
				return err
			}
		}
	}
	return nil
}
