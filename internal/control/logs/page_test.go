// SPDX-License-Identifier: AGPL-3.0-only

package logs

import (
	"strings"
	"testing"
	"time"

	"google.golang.org/protobuf/proto"
	"google.golang.org/protobuf/types/known/timestamppb"

	agentv1 "github.com/dockwell/dockwell/gen/dockwell/agent/v1"
	dockwellv1 "github.com/dockwell/dockwell/gen/dockwell/v1"
)

// A read a move cut short starts again, so a page that forgets what a server sent
// comes out as if that server had only ever sent it the once: nothing of the first
// try is left in its lines, its search's counts, or where the next page starts, and
// nothing of another server's goes with it.
func TestAPageThatForgetsAServerIsAsIfItNeverHeardFromIt(t *testing.T) {
	at := time.Date(2026, 11, 5, 12, 0, 0, 0, time.UTC)
	line := func(i int) *dockwellv1.LogLine {
		return &dockwellv1.LogLine{Time: timestamppb.New(at.Add(time.Duration(i) * time.Second)), Text: strings.Repeat("x", 10+i)}
	}
	// Sources sort by when they began, so each has its own time.
	source := func(id string, i int) *dockwellv1.LogSource {
		return &dockwellv1.LogSource{Id: id, CreateTime: timestamppb.New(at.Add(time.Duration(i) * time.Minute))}
	}
	other := &agentv1.SendLogsRequest{
		Sources: []*dockwellv1.LogSource{source("other", 0)},
		Chunks:  []*dockwellv1.LogChunk{{SourceId: "other", Lines: []*dockwellv1.LogLine{line(0)}}},
		Scans:   []*agentv1.LogScan{{SourceId: "other", Lines: 5}},
	}
	// The first try, with a gap and a scan that say more than the second try does.
	cut := &agentv1.SendLogsRequest{
		Sources: []*dockwellv1.LogSource{source("web", 1), source("old", 2)},
		Chunks: []*dockwellv1.LogChunk{
			{SourceId: "web", Lines: []*dockwellv1.LogLine{line(1)}},
			{SourceId: "old", Lines: []*dockwellv1.LogLine{line(2)}, Gap: &dockwellv1.LogGap{Reason: dockwellv1.LogGapReason_LOG_GAP_REASON_GONE, EndTime: timestamppb.New(at.Add(time.Hour))}},
		},
		Scans: []*agentv1.LogScan{{SourceId: "web", Lines: 100, CutShort: true, TimedOut: true, OldestTime: timestamppb.New(at.Add(-time.Hour)), Older: true}, {SourceId: "scanned", Older: true}},
	}
	// The second, of the same sources, none of them missing lines or with more.
	again := &agentv1.SendLogsRequest{
		Sources: []*dockwellv1.LogSource{source("web", 1), source("old", 2)},
		Chunks: []*dockwellv1.LogChunk{
			{SourceId: "web", Lines: []*dockwellv1.LogLine{line(1), line(3)}},
			{SourceId: "old", Lines: []*dockwellv1.LogLine{line(4)}},
			{SourceId: "scanned", Lines: []*dockwellv1.LogLine{line(5)}},
		},
		Scans: []*agentv1.LogScan{{SourceId: "web", Lines: 7}},
	}
	finish := func(p *page) *dockwellv1.ReadLogsResponse {
		res := &dockwellv1.ReadLogsResponse{}
		p.finish(res, true)
		return res
	}
	never := newPage(100, pageToken{})
	never.add("srv_a", other)
	never.add("srv_b", again)

	retried := newPage(100, pageToken{})
	retried.add("srv_a", other)
	retried.add("srv_b", cut)
	retried.forget("srv_b")
	retried.add("srv_b", again)

	if retried.bytes != never.bytes {
		t.Errorf("a page that forgot counts %d bytes, want %d", retried.bytes, never.bytes)
	}
	if !proto.Equal(finish(retried), finish(never)) {
		t.Errorf("a page that forgot a server came to\n%v\nwant\n%v", finish(retried), finish(never))
	}
	alone := newPage(100, pageToken{})
	alone.add("srv_b", cut)
	alone.forget("srv_b")
	if !proto.Equal(finish(alone), finish(newPage(100, pageToken{}))) {
		t.Errorf("a page that forgot its only server = %v, want nothing", finish(alone))
	}
}
