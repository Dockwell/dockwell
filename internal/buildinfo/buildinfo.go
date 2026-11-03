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

	"google.golang.org/protobuf/types/known/timestamppb"

	dockwellv1 "github.com/dockwell/dockwell/gen/dockwell/v1"
)

// version overrides the module version when set at link time.
var version string

// Devel is the version a build reports when nothing better is known, such as
// `go run` outside a VCS checkout.
const Devel = "devel"

// Info identifies one build of the dockwell binary.
type Info struct {
	// Version is a semantic version such as "v0.4.0", a Go pseudo-version for
	// builds between releases, or Devel.
	Version string
	// Commit is the full commit hash, or empty when unknown.
	Commit string
	// CommitTime is the time of Commit, or the zero time when unknown.
	CommitTime time.Time
	// Modified reports uncommitted changes in the tree at build time.
	Modified bool
	// GoVersion is the toolchain that built the binary, such as "go1.27.3".
	GoVersion string
	// Platform is GOOS/GOARCH, such as "linux/amd64".
	Platform string
}

// ShortCommit returns the first 12 characters of the commit hash.
func (i Info) ShortCommit() string {
	const n = 12
	if len(i.Commit) > n {
		return i.Commit[:n]
	}
	return i.Commit
}

// Proto returns the build in its API form, dockwell.v1.BuildInfo. The CLI's
// JSON output uses it too, so a build has one shape everywhere.
func (i Info) Proto() *dockwellv1.BuildInfo {
	pb := &dockwellv1.BuildInfo{
		Version:   i.Version,
		Commit:    i.Commit,
		Modified:  i.Modified,
		GoVersion: i.GoVersion,
		Platform:  i.Platform,
	}
	if !i.CommitTime.IsZero() {
		pb.CommitTime = timestamppb.New(i.CommitTime)
	}
	return pb
}

var read = sync.OnceValue(func() Info {
	bi, _ := debug.ReadBuildInfo()
	return fromBuildInfo(bi, version)
})

// Read returns the running binary's build information. It is computed once.
func Read() Info { return read() }

func fromBuildInfo(bi *debug.BuildInfo, override string) Info {
	info := Info{
		Version:   Devel,
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
