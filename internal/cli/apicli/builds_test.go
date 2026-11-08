// SPDX-License-Identifier: AGPL-3.0-only

package apicli

import (
	"bytes"
	"strings"
	"testing"
	"time"

	"google.golang.org/protobuf/types/known/timestamppb"

	dockwellv1 "github.com/dockwell/dockwell/gen/dockwell/v1"
)

func TestABuildReadsAsItsLinesThenHowItEnded(t *testing.T) {
	now := time.Date(2026, 11, 7, 12, 0, 0, 0, time.UTC)
	var out bytes.Buffer
	Render(&out, &dockwellv1.ReadBuildLogResponse{Lines: []*dockwellv1.BuildLine{
		{Number: 1, Text: "[1/2] FROM alpine:3.23"}, {Number: 1000, Text: "a thousand"}, {Number: 1501, Text: "\x1b[31mred\x1b[0m"},
	}}, now)
	if got := out.String(); got != "[1/2] FROM alpine:3.23\n… 998 lines left out …\na thousand\n… 500 lines left out …\nred\n" {
		t.Errorf("a build's log reads %q", got)
	}
	out.Reset()
	Render(&out, &dockwellv1.ReadBuildLogResponse{Lines: []*dockwellv1.BuildLine{{Number: 1, Text: "one"}}, Dropped: 3}, now)
	if got := out.String(); got != "one\n… 3 lines dropped, as the build printed faster than its log could take them …\n" {
		t.Errorf("a log with lines dropped reads %q", got)
	}
	out.Reset()
	Render(&out, &dockwellv1.WatchBuildResponse{Build: &dockwellv1.Build{
		State: dockwellv1.BuildState_BUILD_STATE_DONE, Commit: strings.Repeat("8be41d2", 6)[:40], Steps: 6, CachedSteps: 4,
		StartTime: timestamppb.New(now.Add(-52 * time.Second)), EndTime: timestamppb.New(now),
	}}, now)
	if got := out.String(); got != "Built 8be41d2 in 52 s, 4 of 6 steps from the cache.\n" {
		t.Errorf("a build that ended reads %q", got)
	}
	out.Reset()
	Render(&out, &dockwellv1.WatchBuildResponse{Build: &dockwellv1.Build{
		State: dockwellv1.BuildState_BUILD_STATE_FAILED, Error: "The build failed at step 4 of 9, RUN npm ci: npm ERR! missing script: build. Fix it in the repository, then deploy again",
	}}, now)
	if got := out.String(); !strings.HasPrefix(got, "The build failed at step 4 of 9") {
		t.Errorf("a build that failed reads %q", got)
	}
	out.Reset()
	Render(&out, &dockwellv1.WatchBuildResponse{Build: &dockwellv1.Build{State: dockwellv1.BuildState_BUILD_STATE_BUILDING}, Lines: []*dockwellv1.BuildLine{{Number: 3, Text: "hello"}}}, now)
	if got := out.String(); got != "hello\n" {
		t.Errorf("a build under way reads %q", got)
	}
}
