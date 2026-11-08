// SPDX-License-Identifier: AGPL-3.0-only

package resource

import (
	"context"
	"time"
)

// BuildState is where a build stands, as its server says.
type BuildState int

// Where a build can stand.
const (
	BuildQueued BuildState = iota + 1
	BuildBuilding
	BuildDone
	BuildFailed
	BuildCancelled
)

// BuildLine is one line of a build's log.
type BuildLine struct {
	Number int64
	Time   time.Time
	// Step is the step it came from, from 1, or 0 before the first.
	Step int
	Text string
}

// BuildUpdate is how a build under way is going: where it stands, what it has
// found out, and the lines its log gained since the last update.
type BuildUpdate struct {
	State   BuildState
	Lines   []BuildLine
	Dropped int64
	// Subject and Author are the commit's.
	Subject, Author string
	// Steps is how many it has, and Cached how many came from the cache.
	Steps, Cached int
	// Step is the one it is on, or that failed, and StepName its words.
	Step     int
	StepName string
	// Error is why it failed, in plain words, and FailedLine what its failing
	// step said last.
	Error, FailedLine string
	Start, End        time.Time
	Image             string
}

type buildsKey struct{}

// WithBuilds returns a context whose step reports how a build goes to fn, as the
// agent sends it over its stream.
func WithBuilds(ctx context.Context, fn func(BuildUpdate)) context.Context {
	return context.WithValue(ctx, buildsKey{}, fn)
}

// ReportBuild tells whoever follows the step how its build is going. Nothing hears
// it when nobody follows.
func ReportBuild(ctx context.Context, u BuildUpdate) {
	if fn, _ := ctx.Value(buildsKey{}).(func(BuildUpdate)); fn != nil {
		fn(u)
	}
}
