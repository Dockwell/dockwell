// SPDX-License-Identifier: AGPL-3.0-only

package bump

import (
	"bytes"
	"errors"
	"fmt"
	"regexp"
	"strconv"
	"strings"
	"sync"
)

// Edit is what a bump changes in an entry's file: each image it moves, by the
// whole reference as the file writes it, and the entry's version, where the image
// that gives it moves, and its release.
type Edit struct {
	Images  map[string]string
	Version string
	Release int
}

var (
	// The version's scalar alone, quoted or plain, so a comment after it stays.
	versionLine = sync.OnceValue(func() *regexp.Regexp {
		return regexp.MustCompile(`(?m)^version:[ \t]*("[^"\n]*"|'[^'\n]*'|[^\s#'"][^\s#]*)[ \t]*(?:#.*)?$`)
	})
	releaseLine = sync.OnceValue(func() *regexp.Regexp { return regexp.MustCompile(`(?m)^release:[ \t]*(\d+)[ \t]*$`) })
)

// Apply makes an edit to an entry's file as text, so its comments and its layout
// stay: each image's line, the version's and the release's, each of which must be
// in it exactly once, or nothing is edited.
func Apply(text []byte, e Edit) ([]byte, error) {
	if len(text) == 0 || text[len(text)-1] != '\n' {
		return nil, errors.New("it doesn't end with a new line, which an edit keeps. Add one")
	}
	lines := strings.SplitAfter(string(text), "\n")
	lines = lines[:len(lines)-1]
	for from, to := range e.Images {
		at := -1
		for i, l := range lines {
			key, value, ok := strings.Cut(strings.TrimSpace(l), ":")
			if !ok || strings.TrimSpace(key) != "image" || strings.Trim(strings.TrimSpace(value), `"'`) != from {
				continue
			}
			if at >= 0 {
				return nil, fmt.Errorf("its image %s is written on more than one line, so the bump can't tell which to move. Write it once", from)
			}
			at = i
		}
		if at < 0 {
			return nil, fmt.Errorf("its image %s isn't on a line of its own as image: %s", from, from)
		}
		lines[at] = strings.Replace(lines[at], from, to, 1)
	}
	out := strings.Join(lines, "")
	if e.Version != "" {
		m := versionLine().FindAllStringSubmatchIndex(out, -1)
		if len(m) != 1 {
			return nil, fmt.Errorf("its version is on %d top-level lines, not one", len(m))
		}
		value := out[m[0][2]:m[0][3]]
		quoted := e.Version
		switch {
		case strings.HasPrefix(value, `"`):
			quoted = strconv.Quote(e.Version)
		case strings.HasPrefix(value, "'"):
			quoted = "'" + e.Version + "'"
		}
		out = out[:m[0][2]] + quoted + out[m[0][3]:]
	}
	m := releaseLine().FindAllStringSubmatchIndex(out, -1)
	if len(m) != 1 {
		return nil, fmt.Errorf("its release is on %d top-level lines, not one", len(m))
	}
	out = out[:m[0][2]] + strconv.Itoa(e.Release) + out[m[0][3]:]
	return []byte(out), nil
}

// Release is the release an entry's file says.
func Release(text []byte) (int, error) {
	m := releaseLine().FindAllSubmatch(text, -1)
	if len(m) != 1 {
		return 0, fmt.Errorf("its release is on %d top-level lines, not one", len(m))
	}
	return strconv.Atoi(string(m[0][1]))
}

// contextLines is how many lines a patch's hunk shows around what it changes.
const contextLines = 3

// Diff is a patch that turns before into after, a file whose lines an Apply only
// replaced, one for one, as git apply takes it, for the file at path in the
// repository.
func Diff(path string, before, after []byte) ([]byte, error) {
	a := strings.SplitAfter(string(before), "\n")
	b := strings.SplitAfter(string(after), "\n")
	if len(a) != len(b) {
		return nil, fmt.Errorf("the edit of %s changed how many lines it has", path)
	}
	a, b = a[:len(a)-1], b[:len(b)-1]
	var changed []int
	for i := range a {
		if a[i] != b[i] {
			changed = append(changed, i)
		}
	}
	if len(changed) == 0 {
		return nil, nil
	}
	var out bytes.Buffer
	fmt.Fprintf(&out, "--- a/%s\n+++ b/%s\n", path, path)
	for i := 0; i < len(changed); {
		start := max(changed[i]-contextLines, 0)
		end := min(changed[i]+contextLines+1, len(a))
		j := i + 1
		for j < len(changed) && changed[j]-contextLines <= end {
			end = min(changed[j]+contextLines+1, len(a))
			j++
		}
		fmt.Fprintf(&out, "@@ -%d,%d +%d,%d @@\n", start+1, end-start, start+1, end-start)
		for k := start; k < end; k++ {
			if a[k] == b[k] {
				out.WriteString(" " + a[k])
				continue
			}
			out.WriteString("-" + a[k])
			out.WriteString("+" + b[k])
		}
		i = j
	}
	return out.Bytes(), nil
}
