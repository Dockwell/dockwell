// SPDX-License-Identifier: AGPL-3.0-only

package apicli

import (
	"bytes"
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	"connectrpc.com/connect"
	"google.golang.org/protobuf/types/known/timestamppb"

	dockwellv1 "github.com/dockwell/dockwell/gen/dockwell/v1"
	"github.com/dockwell/dockwell/internal/control/authapi"
)

func TestTerminalSafe(t *testing.T) {
	for _, tc := range []struct{ in, plain, color string }{
		{"GET / 200", "GET / 200", "GET / 200"},
		{"\x1b[31mred\x1b[0m", "red", "\x1b[31mred\x1b[0m"},
		{"tab\tstays", "tab\tstays", "tab\tstays"},
		// What moves the cursor, clears the screen or sets the title goes,
		// on a terminal too.
		{"\x1b[2J\x1b[Hhidden", "hidden", "hidden"},
		{"\x1b]0;evil title\x07text", "text", "text"},
		{"\x1b]0;evil title\x1b\\text", "text", "text"},
		{"\x1bPdevice string\x1b\\text", "text", "text"},
		{"\x1b[?1049h", "", ""},
		{"bell\x07 and nul\x00 and bs\x08", "bell and nul and bs", "bell and nul and bs"},
		{"cr\r\noverwrite", "croverwrite", "croverwrite"},
		// The 8-bit form of an escape.
		{"\u009b2Jtext", "2Jtext", "2Jtext"},
		{"naïve 日本語", "naïve 日本語", "naïve 日本語"},
		{"\x1b", "", ""},
		{"\x1b[", "", ""},
	} {
		if got := terminalSafe(tc.in, false); got != tc.plain {
			t.Errorf("terminalSafe(%q, plain) = %q, want %q", tc.in, got, tc.plain)
		}
		if got := terminalSafe(tc.in, true); got != tc.color {
			t.Errorf("terminalSafe(%q, colour) = %q, want %q", tc.in, got, tc.color)
		}
	}
}

func TestPrintsLogsForPeople(t *testing.T) {
	var out, errs bytes.Buffer
	p := newLogPrinter(&out, &errs)
	at := time.Date(2026, 11, 4, 14, 19, 58, 12_000_000, time.UTC)
	clock := at.Local().Format("15:04:05.000")
	src := &dockwellv1.LogSource{Id: "aaaaaaaaaaaa", ServerId: "srv_1", ServerName: "hz-1", Replica: 1, Revision: 3}
	line := func(s string, cut uint32) *dockwellv1.LogLine {
		return &dockwellv1.LogLine{Time: timestamppb.New(at), Text: s, CutBytes: cut}
	}
	p.watch(&dockwellv1.WatchLogsResponse{
		Sources: []*dockwellv1.LogSource{src},
		Servers: []*dockwellv1.ServerLogStatus{{ServerId: "srv_1", ServerName: "hz-1", State: dockwellv1.ServerLogState_SERVER_LOG_STATE_READING}},
		Chunks:  []*dockwellv1.LogChunk{{SourceId: src.GetId(), Lines: []*dockwellv1.LogLine{line("GET / 200", 0)}}},
	})
	// One source: no source column.
	if got, want := out.String(), clock+"  GET / 200\n"; got != want {
		t.Errorf("one source printed %q, want %q", got, want)
	}
	if errs.Len() != 0 {
		t.Errorf("said %q as it started reading", errs.String())
	}
	out.Reset()

	// Two: a source column, and each of a server's says which it is.
	p.watch(&dockwellv1.WatchLogsResponse{
		Sources: []*dockwellv1.LogSource{{Id: "bbbbbbbbbbbb", ServerId: "srv_1", ServerName: "hz-1", Replica: 1, Revision: 4}},
		Chunks: []*dockwellv1.LogChunk{{
			SourceId: "bbbbbbbbbbbb",
			Gap:      &dockwellv1.LogGap{Reason: dockwellv1.LogGapReason_LOG_GAP_REASON_TOO_FAST, Lines: 1234567},
			Lines:    []*dockwellv1.LogLine{line("\x1b[2Jlong line", 2048)},
		}},
	})
	if got := out.String(); !strings.Contains(got, "-- 1,234,567 lines not shown: this service wrote faster") ||
		!strings.Contains(got, clock+"  hz-1 r4  long line … (2 KB more)\n") {
		t.Errorf("printed %q", got)
	}
	out.Reset()

	// A server going away and coming back is said apart from the lines.
	p.watch(&dockwellv1.WatchLogsResponse{Servers: []*dockwellv1.ServerLogStatus{{
		ServerId: "srv_1", ServerName: "hz-1", State: dockwellv1.ServerLogState_SERVER_LOG_STATE_OFFLINE, Message: "hz-1 isn't connected",
	}}})
	p.watch(&dockwellv1.WatchLogsResponse{Servers: []*dockwellv1.ServerLogStatus{{
		ServerId: "srv_1", ServerName: "hz-1", State: dockwellv1.ServerLogState_SERVER_LOG_STATE_READING,
	}}})
	if out.Len() != 0 {
		t.Errorf("printed %q with the lines", out.String())
	}
	if got := errs.String(); !strings.Contains(got, "hz-1 isn't connected.") || !strings.Contains(got, "hz-1 is back") {
		t.Errorf("said %q", got)
	}
	errs.Reset()

	// A read says how far a search reached, and how to go on.
	p.read(&dockwellv1.ReadLogsResponse{SearchedBackTo: timestamppb.New(at), SearchedLines: 1500, NextPageToken: "abc"})
	if got := errs.String(); !strings.Contains(got, "Searched 1,500 lines") || !strings.Contains(got, "--page-token abc") {
		t.Errorf("said %q", got)
	}
}

// Ctrl-C ends a reader of logs, which is what it is for: no error, and nothing
// about whether something went through, which other commands say.
func TestAnInterruptEndsReadingLogsQuietly(t *testing.T) {
	canceled := connect.NewError(connect.CodeCanceled, context.Canceled)
	if err := explain(canceled, &Client{URL: "http://x"}, "dockwell logs follow", nil); err != nil {
		t.Errorf("an interrupt while following logs = %v, want nothing", err)
	}
	if err := explain(canceled, &Client{URL: "http://x"}, "dockwell changes apply", nil); err == nil {
		t.Error("an interrupt while applying said nothing")
	}
}

func TestATokenWithoutTheGrantIsToldWhatToDo(t *testing.T) {
	err := connect.NewError(connect.CodePermissionDenied, errors.New("no"))
	err.Meta().Set(authapi.ReasonKey, authapi.ReasonTokenLogs)
	got := explain(err, &Client{URL: "https://panel.test"}, "dockwell logs read", nil)
	if got == nil || !strings.Contains(got.Error(), "can't read logs") || !strings.Contains(got.Error(), "Profile and security") {
		t.Errorf("refused for want of the grant = %v", got)
	}
}
