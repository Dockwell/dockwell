// SPDX-License-Identifier: AGPL-3.0-only

package apicli

import (
	"bytes"
	"strings"
	"testing"
	"time"

	"google.golang.org/protobuf/types/known/timestamppb"

	resourcev1 "github.com/dockwell/dockwell/gen/dockwell/resource/v1alpha1"
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

func TestWhatDetectFoundReadsAsAPlan(t *testing.T) {
	commit := strings.Repeat("8be41d2", 6)[:40]
	var out bytes.Buffer
	Render(&out, &dockwellv1.DetectBuildResponse{Commit: commit, Ref: "main", ServerName: "web-1", Detection: &dockwellv1.BuildDetection{
		Builder: resourcev1.ImageBuild_BUILDER_RAILPACK, RailpackVersion: "0.43.0", Providers: []string{"node"},
		Packages:     []*dockwellv1.DetectedPackage{{Name: "node", Requested: "22", Resolved: "22.23.4", Source: "package.json > engines > node"}},
		Steps:        []*dockwellv1.DetectedStep{{Name: "install", Commands: []string{"npm ci"}}, {Name: "build", Commands: []string{"npm run build"}}},
		StartCommand: "npm run start", Warnings: []string{"No node package manager detected, using npm"},
	}}, time.Now())
	want := "web-1 read 8be41d2 (main).\nBuilds with Railpack 0.43.0, as node.\n" +
		"  node     22.23.4        from package.json > engines > node\n" +
		"  install  npm ci         \n" +
		"  build    npm run build  \n" +
		"  starts   npm run start  \n" +
		"Railpack warns: No node package manager detected, using npm.\n"
	if got := out.String(); got != want {
		t.Errorf("what detect found reads\n%s\nnot\n%s", got, want)
	}
	out.Reset()
	Render(&out, &dockwellv1.DetectBuildResponse{Commit: commit, Ref: "main", ServerName: "web-1", Detection: &dockwellv1.BuildDetection{
		Builder: resourcev1.ImageBuild_BUILDER_DOCKERFILE, Dockerfile: "web/Dockerfile", BaseImage: "node:22-alpine",
	}}, time.Now())
	if got := out.String(); got != "web-1 read 8be41d2 (main).\nBuilds with its Dockerfile, web/Dockerfile, from node:22-alpine.\n" {
		t.Errorf("a Dockerfile reads %q", got)
	}
	out.Reset()
	Render(&out, &dockwellv1.DetectBuildResponse{Commit: commit, ServerName: "web-1", Detection: &dockwellv1.BuildDetection{
		Builder: resourcev1.ImageBuild_BUILDER_RAILPACK, Error: "Railpack could not determine how to build the app.\n\nThe following languages are supported:\n- Node",
	}}, time.Now())
	if got := out.String(); !strings.Contains(got, "Railpack can't build it. It says:\n  Railpack could not determine how to build the app.\n") || !strings.HasSuffix(got, "Add a Dockerfile, or set both a build command and a start command.\n") {
		t.Errorf("what Railpack can't build reads %q", got)
	}
}
