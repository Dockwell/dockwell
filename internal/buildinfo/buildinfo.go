// SPDX-License-Identifier: AGPL-3.0-only

// Package buildinfo reports which build of the dockwell binary is running.
//
// Everything comes from the build information the Go toolchain embeds: the
// module version stamped from the VCS tag, the commit and its time, and
// whether the tree was modified. Release builds may also set the version at
// link time:
//
//	go build -ldflags "-X github.com/dockwell/dockwell/internal/buildinfo.version=v1.2.3"
package buildinfo

import (
	"runtime"
	"runtime/debug"
	"sync"
	"time"
)

// version overrides the module version when set at link time.
var version string

// develVersion is what a build reports when nothing better is known, such as
// `go run` outside a VCS checkout.
const develVersion = "devel"

// Info identifies one build of the dockwell binary.
type Info struct {
	// Version is a semantic version such as "v0.4.0", a Go pseudo-version for
	// builds between releases, or "devel".
	Version string `json:"version"`
	// Commit is the full commit hash, or empty when unknown.
	Commit string `json:"commit,omitempty"`
	// CommitTime is the time of Commit, or the zero time when unknown.
	CommitTime time.Time `json:"commit_time,omitzero"`
	// Modified reports uncommitted changes in the tree at build time.
	Modified bool `json:"modified,omitempty"`
	// GoVersion is the toolchain that built the binary, such as "go1.27.3".
	GoVersion string `json:"go_version"`
	// Platform is GOOS/GOARCH, such as "linux/amd64".
	Platform string `json:"platform"`
}

// ShortCommit returns the first 12 characters of the commit hash.
func (i Info) ShortCommit() string {
	const n = 12
	if len(i.Commit) > n {
		return i.Commit[:n]
	}
	return i.Commit
}

var read = sync.OnceValue(func() Info {
	bi, ok := debug.ReadBuildInfo()
	if !ok {
		bi = nil
	}
	return fromBuildInfo(bi, version)
})

// Read returns the running binary's build information. It is computed once.
func Read() Info { return read() }

func fromBuildInfo(bi *debug.BuildInfo, override string) Info {
	info := Info{
		Version:   develVersion,
		GoVersion: runtime.Version(),
		Platform:  runtime.GOOS + "/" + runtime.GOARCH,
	}
	if bi != nil {
		if v := bi.Main.Version; v != "" && v != "(devel)" {
			info.Version = v
		}
		if bi.GoVersion != "" {
			info.GoVersion = bi.GoVersion
		}
		for _, s := range bi.Settings {
			switch s.Key {
			case "vcs.revision":
				info.Commit = s.Value
			case "vcs.time":
				if t, err := time.Parse(time.RFC3339, s.Value); err == nil {
					info.CommitTime = t.UTC()
				}
			case "vcs.modified":
				info.Modified = s.Value == "true"
			}
		}
	}
	if override != "" {
		info.Version = override
	}
	return info
}
