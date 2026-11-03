// SPDX-License-Identifier: AGPL-3.0-only

package buildinfo

import (
	"runtime"
	"runtime/debug"
	"testing"
	"time"
)

func TestFromBuildInfo(t *testing.T) {
	platform := runtime.GOOS + "/" + runtime.GOARCH
	stamped := &debug.BuildInfo{
		GoVersion: "go1.27.3",
		Main:      debug.Module{Path: "github.com/dockwell/dockwell", Version: "v0.4.0"},
		Settings: []debug.BuildSetting{
			{Key: "vcs.revision", Value: "3f2a9c1d4e5b6a7f8091a2b3c4d5e6f708192a3b"},
			{Key: "vcs.time", Value: "2026-11-03T21:40:00+01:00"},
			{Key: "vcs.modified", Value: "true"},
		},
	}

	tests := []struct {
		name     string
		bi       *debug.BuildInfo
		override string
		want     Info
	}{
		{
			name: "no build info",
			want: Info{Version: "devel", GoVersion: runtime.Version(), Platform: platform},
		},
		{
			name: "devel module version",
			bi:   &debug.BuildInfo{GoVersion: "go1.27.3", Main: debug.Module{Version: "(devel)"}},
			want: Info{Version: "devel", GoVersion: "go1.27.3", Platform: platform},
		},
		{
			name: "stamped from VCS",
			bi:   stamped,
			want: Info{
				Version:    "v0.4.0",
				Commit:     "3f2a9c1d4e5b6a7f8091a2b3c4d5e6f708192a3b",
				CommitTime: time.Date(2026, 11, 3, 20, 40, 0, 0, time.UTC),
				Modified:   true,
				GoVersion:  "go1.27.3",
				Platform:   platform,
			},
		},
		{
			name:     "link-time version wins",
			bi:       stamped,
			override: "v0.5.0-rc.1",
			want: Info{
				Version:    "v0.5.0-rc.1",
				Commit:     "3f2a9c1d4e5b6a7f8091a2b3c4d5e6f708192a3b",
				CommitTime: time.Date(2026, 11, 3, 20, 40, 0, 0, time.UTC),
				Modified:   true,
				GoVersion:  "go1.27.3",
				Platform:   platform,
			},
		},
		{
			name: "unparseable commit time is dropped",
			bi: &debug.BuildInfo{Settings: []debug.BuildSetting{
				{Key: "vcs.time", Value: "yesterday"},
			}},
			want: Info{Version: "devel", GoVersion: runtime.Version(), Platform: platform},
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := fromBuildInfo(tt.bi, tt.override)
			if !got.CommitTime.Equal(tt.want.CommitTime) {
				t.Errorf("CommitTime = %v, want %v", got.CommitTime, tt.want.CommitTime)
			}
			got.CommitTime, tt.want.CommitTime = time.Time{}, time.Time{}
			if got != tt.want {
				t.Errorf("fromBuildInfo() = %+v, want %+v", got, tt.want)
			}
		})
	}
}

func TestShortCommit(t *testing.T) {
	for _, tt := range []struct{ commit, want string }{
		{"", ""},
		{"3f2a9c", "3f2a9c"},
		{"3f2a9c1d4e5b6a7f8091a2b3c4d5e6f708192a3b", "3f2a9c1d4e5b"},
	} {
		if got := (Info{Commit: tt.commit}).ShortCommit(); got != tt.want {
			t.Errorf("ShortCommit(%q) = %q, want %q", tt.commit, got, tt.want)
		}
	}
}

func TestReadIsStable(t *testing.T) {
	if a, b := Read(), Read(); a != b {
		t.Fatalf("Read() changed between calls: %+v then %+v", a, b)
	}
}
