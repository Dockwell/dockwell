// SPDX-License-Identifier: AGPL-3.0-only

// Package buildinfo reports which build of the dockwell binary is running.
//
// Everything comes from the build information the Go toolchain embeds: the
// module version stamped from the VCS tag, the commit and its time, and
// whether the tree was modified. Release builds may also set the version at
// link time:
//
//	go build -ldflags "-X github.com/dockwell/dockwell/internal/buildinfo.version=v1.2.3"
//
// Which build it is, the full build or the agent build, comes from the build
// tags: the agent build is built with -tags agent
// (docs/adr/0023-a-full-build-and-an-agent-build.md).
package buildinfo

import (
	"regexp"
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

// Edition is which of the two builds of dockwell a binary is.
type Edition string

// The two builds of dockwell, from the same source.
const (
	// Full is the full build: the control plane with its own agent, the
	// panel, the API and every command. The control server runs it.
	Full Edition = "full"
	// Agent is the agent build, built with -tags agent: only the agent, for
	// a server that joins a control plane.
	Agent Edition = "agent"
)

// Info identifies one build of the dockwell binary.
type Info struct {
	// Version is a semantic version such as "v0.4.0", a Go pseudo-version for
	// builds between releases, or Devel.
	Version string
	// Edition is which build this is. It is empty only for a binary that
	// stands in for one made before there were two builds, as the install
	// tests link.
	Edition Edition
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
	switch i.Edition {
	case Full:
		pb.Edition = dockwellv1.BuildEdition_BUILD_EDITION_FULL
	case Agent:
		pb.Edition = dockwellv1.BuildEdition_BUILD_EDITION_AGENT
	}
	if !i.CommitTime.IsZero() {
		pb.CommitTime = timestamppb.New(i.CommitTime)
	}
	return pb
}

// release is a release's version, with or without its "v", and pseudo the tail of
// a Go pseudo-version, which a build between releases carries. Each is made on
// first use, as the agent build doesn't read them as it starts.
var (
	release = sync.OnceValue(func() *regexp.Regexp { return regexp.MustCompile(`^v?\d+\.\d+\.\d+(-[0-9A-Za-z.-]+)?$`) })
	pseudo  = sync.OnceValue(func() *regexp.Regexp { return regexp.MustCompile(`\d{14}-[0-9a-f]{12}$`) })
)

// Release reports whether the build is a release: a release's version, built from
// the tree as it was tagged.
func (i Info) Release() bool {
	return !i.Modified && release().MatchString(i.Version) && !pseudo().MatchString(i.Version)
}

// EditionOf is which build a BuildInfo from the API names. A build that
// names none was made before there were two builds, so it is the full
// build.
func EditionOf(b *dockwellv1.BuildInfo) Edition {
	if b.GetEdition() == dockwellv1.BuildEdition_BUILD_EDITION_AGENT {
		return Agent
	}
	return Full
}

var read = sync.OnceValue(func() Info {
	bi, _ := debug.ReadBuildInfo()
	info := fromBuildInfo(bi, version)
	info.Edition = Edition(edition)
	return info
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
