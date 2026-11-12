// SPDX-License-Identifier: AGPL-3.0-only

package registry

import (
	"fmt"
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
		{"18.0-beta1", []string{"18.0-beta1", "18.0-beta2", "18.0", "18.1-rc1"}, "18.1-rc1 patch"},
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
		{"17.11", "17.12", KindPatch},
		{"17.12", "18.1", KindMajor},
		{"1.27.3", "1.28.0", KindMinor},
		{"1.27.3", "1.27.4", KindPatch},
		{"1.27", "1.27.4", KindPatch},
		{"11.4.4", "11.4.4", KindNone},
		{"2024.10", "2024.11", KindPatch},
		{"stable", "edge", KindMajor},
		{"17.11", "17.11.0", KindPatch},
		{"v2.1", "2.2", KindPatch},
	} {
		if got := Step(c.from, c.to); got != c.want {
			t.Errorf("Step(%q, %q) = %s, want %s", c.from, c.to, got, c.want)
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
