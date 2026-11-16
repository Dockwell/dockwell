// SPDX-License-Identifier: AGPL-3.0-only

package registry

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestNewerTags(t *testing.T) {
	postgres := []string{"9.6.24", "15.8", "16", "16.3", "16.4", "16.4-alpine", "16.4-alpine3.20", "16.4-alpine3.21", "16.4-bookworm", "17", "17.0", "17.2", "17.2-alpine", "18beta1", "18.0-beta1", "latest", "alpine"}
	nginx := []string{"1", "1.26", "1.26.2", "1.27", "1.27.3", "1.27.4", "1.28", "1.28.0", "1.28.1", "1.28.1-alpine", "mainline", "stable"}
	for _, c := range []struct {
		current string
		tags    []string
		want    string
	}{
		// PostgreSQL numbers its versions with two numbers since 10, so 16.3 to 16.4 is a patch.
		{"16.3", postgres, "16.4 patch, 17.2 major"},
		{"16.4", postgres, "17.2 major"},
		{"17.2", postgres, ""},
		// A suffix keeps its track, and the variant that runs exactly comes first.
		{"16.3-alpine", postgres, "16.4-alpine patch, 17.2-alpine major"},
		{"16.3-alpine3.20", postgres, "16.4-alpine3.20 patch"},
		{"16.3-alpine3.19", postgres, "16.4-alpine3.21 patch"},
		// A tag of one number moves by majors alone, and never to a longer tag.
		{"16", postgres, "17 major"},
		// nginx has three numbers, so its two-number tags move by minors.
		{"1.27", nginx, "1.28 minor"},
		{"1.27.3", nginx, "1.27.4 patch, 1.28.1 minor"},
		{"1.26.2", nginx, "1.28.1 minor"},
		// Pre-releases are left out unless one runs.
		{"17.2", []string{"17.2", "17.3-rc1", "18.0-beta1"}, ""},
		// With nothing beyond major 18 to read, its second number can't be told, so 18.1 is a minor.
		{"18.0-beta1", []string{"18.0-beta1", "18.0-beta2", "18.0", "18.1-rc1"}, "18.0 patch, 18.1-rc1 minor"},
		{"18.0-beta1", []string{"18.0-beta1", "18.0-beta2"}, "18.0-beta2 patch"},
		{"18.0-beta2", []string{"18.0-beta1", "18.0-beta2"}, ""},
		// A v before the numbers keeps its track.
		{"v1.2.3", []string{"v1.2.4", "1.2.9", "v2.0.0", "v1.3.0"}, "v1.2.4 patch, v1.3.0 minor, v2.0.0 major"},
		// A build's number in the suffix is its own track, and the same version isn't newer.
		{"1.2.3-ls45", []string{"1.2.3-ls47", "1.2.4-ls48", "1.2.4"}, "1.2.4-ls48 patch"},
		// What isn't a version gets nothing here, only the digest check.
		{"latest", postgres, ""},
		{"stable-alpine", nginx, ""},
	} {
		var said []string
		for _, n := range NewerTags(c.current, c.tags) {
			said = append(said, fmt.Sprintf("%s %s", n.Tag, n.Kind))
		}
		got := strings.Join(said, ", ")
		if got != c.want {
			t.Errorf("NewerTags(%q) = %q, want %q", c.current, got, c.want)
		}
	}
}

func TestStep(t *testing.T) {
	for _, c := range []struct {
		from, to string
		want     Kind
	}{
		// Two versions alone can't say whether the second number is a patch, so it's a minor.
		{"17.11", "17.12", KindMinor},
		{"17.12", "18.1", KindMajor},
		{"1.27.3", "1.28.0", KindMinor},
		{"1.27.3", "1.27.4", KindPatch},
		{"1.27", "1.27.4", KindPatch},
		{"11.4.4", "11.4.4", KindNone},
		{"2024.10", "2024.11", KindMinor},
		{"stable", "edge", KindMajor},
		{"17.11", "17.11.0", KindPatch},
		{"v2.1", "2.2", KindMinor},
	} {
		if got := Step(c.from, c.to); got != c.want {
			t.Errorf("Step(%q, %q) = %s, want %s", c.from, c.to, got, c.want)
		}
	}
}

// A change of numbering, dates to semver or back, is neither newer nor lower, and a
// person takes it (ADR 0080).
func TestRenumbered(t *testing.T) {
	for _, c := range []struct {
		from, to string
		want     bool
	}{
		{"2024.10.1", "3.0.0", true},
		{"3.0.0", "2024.10.1", true},
		{"2024.10", "2024.11.2", false},
		{"1.2", "1.3", false},
		{"latest", "2024.10.1", false},
	} {
		if got := Renumbered(c.from, c.to); got != c.want {
			t.Errorf("Renumbered(%q, %q) = %v, want %v", c.from, c.to, got, c.want)
		}
	}
}

// A lower version is a move down, which a catalog install refuses (ADR 0080); a
// version with no numbers is never lower, as nothing says it is.
func TestOlder(t *testing.T) {
	for _, c := range []struct {
		from, to string
		want     bool
	}{
		{"2.0", "1.9", true},
		{"2.1", "2.0.1", true},
		{"17.11", "17.11.0", false},
		{"1.9", "2.0", false},
		{"2.0", "2.0", false},
		{"v2.1", "2.0", true},
		{"0.13.1-patch.1", "0.13.0", true},
		{"latest", "1.0", false},
		{"2.0", "latest", false},
		// A pre-release is below its release, and an earlier one below a later.
		{"2.0.0", "2.0.0-rc1", true},
		{"2.0.0-rc1", "2.0.0", false},
		{"2.0.0-rc2", "2.0.0-rc1", true},
		{"2.0.0-rc1", "2.0.0-beta3", true},
		{"2.0.0-beta3", "2.0.0-rc1", false},
		// A build numbered by a word, as linuxserver.io's images are.
		{"1.2.3-ls45", "1.2.3-ls44", true},
		{"1.2.3-ls44", "1.2.3-ls45", false},
		{"1.27-alpine", "1.26-alpine", true},
		{"8.4", "8.0-apache", true},
		{"1.10", "1.9", true},
		// Another numbering, dates to semver or back, moves on.
		{"2024.10.1", "3.0.0", false},
		{"3.0.0", "2024.10.1", false},
		{"2024.10", "2024.9.30", true},
	} {
		if got := Older(c.from, c.to); got != c.want {
			t.Errorf("Older(%q, %q) = %v, want %v", c.from, c.to, got, c.want)
		}
	}
}

func TestIsVersion(t *testing.T) {
	for tag, want := range map[string]bool{"16.3": true, "16": true, "v1.2": true, "1.27.3-alpine": true, "latest": false, "stable": false, "alpine": false, "18beta1": false} {
		if got := IsVersion(tag); got != want {
			t.Errorf("IsVersion(%q) = %v, want %v", tag, got, want)
		}
	}
}

func TestTrackWord(t *testing.T) {
	for tag, want := range map[string]string{"16.3-alpine3.20": "alpine", "16.3": "", "1.2.3-ls45": "ls", "18.0-beta1-bookworm": "bookworm", "latest": "", "17-trixie-slim": "trixie"} {
		if got := TrackWord(tag); got != want {
			t.Errorf("TrackWord(%q) = %q, want %q", tag, got, want)
		}
	}
}

// Tag lists shaped as Docker Hub has them: Redis numbers its versions with three
// numbers, so its two-number tags move by minors; PostgreSQL with two since 10,
// so its second number is a patch; nginx with three.
func TestNewerTagsOfRealShapedLists(t *testing.T) {
	redis := []string{"7", "7.2", "7.2.5", "7.2.6", "7.4", "7.4.1", "7.2-alpine", "7.2.5-alpine", "latest", "alpine"}
	postgres := []string{"16", "16.4", "16.6", "17", "17.0", "16.4-alpine"}
	nginx := []string{"1.27", "1.27.3", "1.28.0", "stable", "mainline"}
	for _, c := range []struct {
		current string
		tags    []string
		want    string
	}{
		{"7.2", redis, "7.4 minor"},
		{"7.2.5", redis, "7.2.6 patch, 7.4.1 minor"},
		{"7.2.5-alpine", redis, ""},
		{"7", redis, ""},
		{"16.4", postgres, "16.6 patch, 17.0 major"},
		{"1.27.3", nginx, "1.28.0 minor"},
		// Two-number tags of one major alone, as a registry that lists a few: it
		// can't be told, so the larger step.
		{"7.2", []string{"7.2", "7.4"}, "7.4 minor"},
		// A third number in a later major says the second isn't a patch any more.
		{"16.4", []string{"16.4", "16.6", "15.8", "17.0.1"}, "16.6 minor"},
		// An older major's third number says nothing of how it's numbered now, as
		// PostgreSQL's 9.6.24 before 10.
		{"16.4", []string{"9.6.24", "15.8", "16.4", "16.6"}, "16.6 patch"},
	} {
		var said []string
		for _, n := range NewerTags(c.current, c.tags) {
			said = append(said, fmt.Sprintf("%s %s", n.Tag, n.Kind))
		}
		if got := strings.Join(said, ", "); got != c.want {
			t.Errorf("NewerTags(%q) = %q, want %q", c.current, got, c.want)
		}
	}
}

// MySQL's official repository as Docker Hub listed it on 15 November 2026: its 8.0
// and 8.4 lines, 9.0 to 9.7, then year.month releases from 26.7. A database on 8.4.x
// was offered 26.10.0 as a major, which isn't a release of its own numbering; it moves
// within 8.4, and to 9.7 as its next major, and 26.x is never offered to it (#724).
// What's on year.month already moves within it.
func TestNewerTagsKeepToTheImagesOwnNumbering(t *testing.T) {
	raw, err := os.ReadFile(filepath.Join("testdata", "mysql-tags.txt"))
	if err != nil {
		t.Fatal(err)
	}
	mysql := strings.Fields(string(raw))
	node := []string{"16.20.2", "18.20.4", "20.18.0", "22.11.0", "23.3.0", "24.0.0"}
	postgres := []string{"9.6.24", "10.23", "11.22", "12.21", "13.17", "14.14", "15.9", "16.5", "17.2", "18.1"}
	for _, c := range []struct {
		current string
		tags    []string
		want    string
	}{
		{"8.4.7", mysql, "8.4.13 patch, 9.7.4 major"},
		{"8.4", mysql, "9.7 major"},
		{"8.4.7-oracle", mysql, "8.4.13-oracle patch, 9.7.4-oracle major"},
		{"8.0.44", mysql, "8.0.46 patch, 8.4.13 minor, 9.7.4 major"},
		{"9.4.0", mysql, "9.7.4 minor"},
		{"26.7.0", mysql, "26.10.0 minor"},
		// Majors a few apart are one numbering: Node's even releases, PostgreSQL's 9 to 10.
		{"18.20.4", node, "24.0.0 major"},
		{"9.6.24", postgres, ""},
		{"10.23", postgres, "18.1 major"},
	} {
		var said []string
		for _, n := range NewerTags(c.current, c.tags) {
			said = append(said, fmt.Sprintf("%s %s", n.Tag, n.Kind))
		}
		if got := strings.Join(said, ", "); got != c.want {
			t.Errorf("NewerTags(%q) = %q, want %q", c.current, got, c.want)
		}
	}
}

// Every newer tag of a track, the newest first, each with its step and major, for a
// catalog's bump, which takes the newest that's aged past its wait (ADR 0100). A
// pre-release is left out unless the current tag is one, and a variant keeps to its
// own track, one tag for each version.
func TestAllNewerAndTagVersion(t *testing.T) {
	tags := []string{"v1.4.2", "v1.4.3", "v1.5.0", "v2.0.0", "v2.1.0", "v3.0.0", "v1.6.0-rc1", "1.9.9", "v1.5.0-alpine"}
	var said []string
	for _, n := range AllNewer("v1.4.2", tags) {
		said = append(said, fmt.Sprintf("%s %s %d", n.Tag, n.Kind, n.Major))
	}
	if got := strings.Join(said, ", "); got != "v3.0.0 major 3, v2.1.0 major 2, v2.0.0 major 2, v1.5.0 minor 1, v1.4.3 patch 1" {
		t.Errorf("AllNewer = %s", got)
	}
	if got := AllNewer("latest", tags); got != nil {
		t.Errorf("AllNewer of a tag that isn't a version = %v", got)
	}
	for tag, want := range map[string]string{"7.1.3-apache": "7.1.3", "v3.2.1": "3.2.1", "17": "17", "latest": "", "2026.11.1": "2026.11.1"} {
		if got := TagVersion(tag); got != want {
			t.Errorf("TagVersion(%q) = %q, want %q", tag, got, want)
		}
	}
}
