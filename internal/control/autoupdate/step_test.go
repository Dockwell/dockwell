// SPDX-License-Identifier: AGPL-3.0-only

package autoupdate

import (
	"testing"

	dockwellv1 "github.com/dockwell/dockwell/gen/dockwell/v1"
)

func labelled(version string) *dockwellv1.ImageConfig {
	return &dockwellv1.ImageConfig{Version: version}
}

func withEnv(name, value string) *dockwellv1.ImageConfig {
	return &dockwellv1.ImageConfig{Env: []*dockwellv1.ImageVariable{{Name: "PATH", Value: "/bin"}, {Name: name, Value: value}}}
}

// A moving tag's new image moves as far as the versions its images say, by their
// label or their official image's variable; what says none can't be told, and a
// two-place move in its second place counts as minor (ADR 0080).
func TestAMovingTagsStepIsWhatItsImagesSay(t *testing.T) {
	for _, c := range []struct {
		name     string
		image    string
		was, now *dockwellv1.ImageConfig
		want     string
	}{
		{"the same release rebuilt", "ghcr.io/acme/shop:16.4", labelled("16.4"), labelled("16.4"), "patch"},
		{"a patch", "ghost:latest", labelled("5.2.1"), labelled("5.2.3"), "patch"},
		{"two places, the second", "ghost:latest", labelled("5.2"), labelled("5.3"), "minor"},
		{"a minor", "ghost:latest", labelled("5.2.1"), labelled("5.3.0"), "minor"},
		{"a major", "ghost:latest", labelled("5.130.0"), labelled("6.0.0"), "major"},
		{"by the official image's variable", "ghost:5", withEnv("GHOST_VERSION", "5.2.1"), withEnv("GHOST_VERSION", "5.4.0"), "minor"},
		{"by postgres's own", "postgres:16", withEnv("PG_VERSION", "16.3-1.pgdg120+2"), withEnv("PG_VERSION", "16.3-1.pgdg120+3"), "patch"},
		{"a v and a suffix", "acme/web:stable", labelled("v1.4.2-alpine"), labelled("v2.0.0-alpine"), "major"},
		{"no labels", "ghost:latest", &dockwellv1.ImageConfig{}, &dockwellv1.ImageConfig{}, ""},
		{"one side says nothing", "ghost:latest", labelled("5.2.1"), &dockwellv1.ImageConfig{}, ""},
		{"not numbers", "acme/web:latest", labelled("nightly"), labelled("nightly"), ""},
		{"nothing read", "ghost:latest", nil, labelled("6.0.0"), ""},
	} {
		if got := Step(c.image, c.was, c.now); got != c.want {
			t.Errorf("%s: Step = %q, want %q", c.name, got, c.want)
		}
	}
}

// Only a tag of three places or more names one release whole.
func TestATagNamesAReleaseWithThreePlaces(t *testing.T) {
	for image, want := range map[string]bool{
		"nginx:1.27.3":                  true,
		"nginx:1.27.3-alpine":           true,
		"acme/web:v2.0.1":               true,
		"nginx:1.27":                    false,
		"postgres:16.4":                 false,
		"postgres:16":                   false,
		"ghost":                         false,
		"ghost:latest":                  false,
		"nginx:alpine":                  false,
		"registry.example.com:5000/web": false,
	} {
		if got := NamesARelease(image); got != want {
			t.Errorf("NamesARelease(%s) = %t, want %t", image, got, want)
		}
	}
}
