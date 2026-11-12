// SPDX-License-Identifier: AGPL-3.0-only

package registry

import (
	"cmp"
	"slices"
	"strconv"
	"strings"
)

// Kind is the size of a step from one version to a newer one (ADR 0079).
type Kind int

// The kinds of step, smallest first.
const (
	// KindNone is no step: the same version, or one that isn't newer.
	KindNone Kind = iota
	// KindPatch moved the version's last number: 16.3 to 16.4 where versions
	// have two numbers, 1.27.3 to 1.27.4 where they have three.
	KindPatch
	// KindMinor moved the second of three numbers or more: 1.27.3 to 1.28.0.
	KindMinor
	// KindMajor moved the first number: 16.4 to 17.0.
	KindMajor
)

func (k Kind) String() string {
	switch k {
	case KindPatch:
		return "patch"
	case KindMinor:
		return "minor"
	case KindMajor:
		return "major"
	case KindNone:
	}
	return "none"
}

// Newer is a tag newer than a version tag, in its track.
type Newer struct {
	Tag  string
	Kind Kind
}

// track is a version tag read for what's newer than it: its numbers, whether it
// has a v before them, its variant (its suffix but for a pre-release's word),
// and whether it's a pre-release.
type track struct {
	nums    []int
	v       bool
	variant string
	pre     bool
	// preNum is a pre-release's numbers, such as 2 for rc2, to order two
	// pre-releases of one version.
	preNum []int
}

// readTrack reads a tag as a version, and says whether it is one.
func readTrack(tag string) (track, bool) {
	p := parseVersion(tag)
	if !p.ok {
		return track{}, false
	}
	t := track{nums: p.nums, v: strings.HasPrefix(tag, "v") || strings.HasPrefix(tag, "V"), variant: strings.ToLower(p.suffix), pre: p.pre}
	if t.pre {
		// A pre-release's word and its number go, so 1.0-rc1-alpine keeps to
		// -alpine, and 1.0-rc1 to the plain track.
		low := t.variant
		for _, w := range preRelease {
			if strings.HasPrefix(low, w) {
				low = low[len(w):]
				break
			}
		}
		i := 0
		for i < len(low) && (low[i] >= '0' && low[i] <= '9' || low[i] == '.') {
			i++
		}
		t.preNum = digitRuns(low[:i])
		t.variant = strings.TrimLeft(low[i:], "-_.+")
	}
	return t, true
}

// key is a track's shape: two tags are in one track when their keys match. A
// run of digits in a variant matches any other, so 16.3-alpine3.20 moves to
// 16.4-alpine3.21, and how many numbers there are must match, so 1.27, which
// follows 1.27.x itself, moves to 1.28 and never to 1.27.4.
func (t track) key() string {
	var b strings.Builder
	if t.v {
		b.WriteByte('v')
	}
	b.WriteString(strconv.Itoa(len(t.nums)))
	b.WriteByte('|')
	b.WriteString(variantShape(t.variant))
	return b.String()
}

// variantShape is a variant with each run of digits as #.
func variantShape(s string) string {
	var b strings.Builder
	digits := false
	for i := range len(s) {
		c := s[i]
		if c >= '0' && c <= '9' {
			if !digits {
				b.WriteByte('#')
			}
			digits = true
			continue
		}
		digits = false
		b.WriteByte(c)
	}
	return b.String()
}

// digitRuns are the numbers in a string, in order, such as [3 21] for alpine3.21.
func digitRuns(s string) []int {
	var out []int
	n, in := 0, false
	for i := range len(s) {
		c := s[i]
		if c >= '0' && c <= '9' {
			n = n*10 + int(c-'0')
			in = true
			if n > 1<<30 {
				n = 1 << 30
			}
			continue
		}
		if in {
			out = append(out, n)
		}
		n, in = 0, false
	}
	if in {
		out = append(out, n)
	}
	return out
}

func compareInts(a, b []int) int {
	for i := range max(len(a), len(b)) {
		x, y := 0, 0
		if i < len(a) {
			x = a[i]
		}
		if i < len(b) {
			y = b[i]
		}
		if x != y {
			return cmp.Compare(x, y)
		}
	}
	return 0
}

// IsVersion says whether a tag reads as a version, which a check reads newer
// tags for. Any other tag, such as latest or stable, gets only the digest check.
func IsVersion(tag string) bool {
	_, ok := readTrack(tag)
	return ok
}

// step classes the step from one version to another of as many numbers, with
// places numbers in a version of their track: the first number that differs is
// the major when it's the first, the minor when it's the second of three or
// more, and the patch otherwise. It's KindNone when to isn't newer.
func step(from, to track, places int) Kind {
	for i := range min(len(from.nums), len(to.nums)) {
		switch {
		case to.nums[i] < from.nums[i]:
			return KindNone
		case to.nums[i] == from.nums[i]:
			continue
		case i == 0:
			return KindMajor
		case i == 1 && places >= 3:
			return KindMinor
		}
		return KindPatch
	}
	// The same numbers: newer only for a pre-release that runs, as its release,
	// or a later pre-release of it.
	switch {
	case !from.pre:
		return KindNone
	case !to.pre:
		return KindPatch
	case compareInts(to.preNum, from.preNum) > 0:
		return KindPatch
	}
	return KindNone
}

// NewerTags are the newest tag of each kind of step that's newer than current,
// in its track, among tags: the newest patch, minor and major, in that order.
// A track is a tag's v, how many numbers it has, and its suffix with any digits:
// 16.3-alpine moves to 16.4-alpine, never to 16.4 or 16.4-bookworm. Pre-releases
// are left out unless current is one. How many numbers its versions have, which
// says whether the second is a minor or a patch, is read from the tags of its
// major in its track: PostgreSQL's 16.x have two, so 16.3 to 16.4 is a patch,
// and nginx's 1.27.3 makes 1.27 to 1.28 a minor. Nil when current isn't a
// version.
func NewerTags(current string, tags []string) []Newer {
	cur, ok := readTrack(current)
	if !ok {
		return nil
	}
	want := cur.key()
	places := len(cur.nums)
	shape := variantShape(cur.variant)
	type candidate struct {
		tag  string
		t    track
		kind Kind
	}
	var found []candidate
	for _, tag := range tags {
		t, ok := readTrack(tag)
		if !ok || t.v != cur.v || variantShape(t.variant) != shape {
			continue
		}
		// Any tag of its major in its track, however many numbers, says how many its
		// versions have.
		if t.nums[0] == cur.nums[0] && len(t.nums) > places {
			places = len(t.nums)
		}
		if t.key() != want || t.pre && !cur.pre || tag == current {
			continue
		}
		found = append(found, candidate{tag: tag, t: t})
	}
	best := map[Kind]candidate{}
	for _, c := range found {
		c.kind = step(cur, c.t, places)
		if c.kind == KindNone {
			continue
		}
		if have, ok := best[c.kind]; !ok || better(c.t, c.tag, have.t, have.tag, cur.variant) {
			best[c.kind] = c
		}
	}
	var out []Newer
	for _, k := range []Kind{KindPatch, KindMinor, KindMajor} {
		if c, ok := best[k]; ok {
			out = append(out, Newer{Tag: c.tag, Kind: k})
		}
	}
	return out
}

// better says whether a is a better tag to move to than b: a newer version, a
// release before a pre-release, then the variant that runs exactly, then the
// newest variant, such as alpine3.21 before alpine3.20, then by name.
func better(a track, aTag string, b track, bTag, variant string) bool {
	if c := compareInts(a.nums, b.nums); c != 0 {
		return c > 0
	}
	if a.pre != b.pre {
		return !a.pre
	}
	if c := compareInts(a.preNum, b.preNum); c != 0 {
		return c > 0
	}
	if (a.variant == variant) != (b.variant == variant) {
		return a.variant == variant
	}
	if c := compareInts(digitRuns(a.variant), digitRuns(b.variant)); c != 0 {
		return c > 0
	}
	return aTag < bTag
}

// Step classes a step from one version to another, read alone, as a catalog
// entry's versions are: KindNone when they're the same, and KindMajor when they
// differ and either doesn't read as a version, which says too much rather than
// too little.
func Step(from, to string) Kind {
	if strings.EqualFold(strings.TrimSpace(from), strings.TrimSpace(to)) {
		return KindNone
	}
	a, okA := readTrack(from)
	b, okB := readTrack(to)
	if !okA || !okB {
		return KindMajor
	}
	places := max(len(a.nums), len(b.nums))
	// Compare as many numbers as both have, a missing one as 0.
	n := places
	pad := func(t track) track {
		nums := slices.Clone(t.nums)
		for len(nums) < n {
			nums = append(nums, 0)
		}
		t.nums = nums
		return t
	}
	k := step(pad(a), pad(b), places)
	if k == KindNone {
		// Older, or the same version written another way: a catalog release
		// moved it, so it's no smaller a step than a patch.
		if compareInts(b.nums, a.nums) < 0 {
			return KindMajor
		}
		return KindPatch
	}
	return k
}

// TrackWord is the longest run of letters in a version tag's variant, such as
// "alpine" for 16.3-alpine3.20, for asking a registry only for tags that hold
// it. Empty for a tag with no variant, or one that isn't a version.
func TrackWord(tag string) string {
	t, ok := readTrack(tag)
	if !ok {
		return ""
	}
	best, run := "", 0
	for i := 0; i <= len(t.variant); i++ {
		if i < len(t.variant) && (t.variant[i] >= 'a' && t.variant[i] <= 'z') {
			run++
			continue
		}
		if run > len(best) {
			best = t.variant[i-run : i]
		}
		run = 0
	}
	return best
}
