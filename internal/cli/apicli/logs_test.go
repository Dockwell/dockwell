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
	"google.golang.org/protobuf/proto"
	"google.golang.org/protobuf/reflect/protoreflect"
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

// What a container prints is untrusted, and these are the ways it could reach a
// terminal: changing what the keyboard sends, making the terminal answer on
// standard input, hiding what comes next, or reading backwards.
func TestOnlyPlainColourReachesATerminal(t *testing.T) {
	const r = "\x1b[0m"
	for _, tc := range []struct{ name, in, color string }{
		{"a plain colour, closed at the line's end", "\x1b[31mred", "\x1b[31mred" + r},
		{"a colour closed by the line", "\x1b[1;32mok\x1b[0m", "\x1b[1;32mok\x1b[0m"},
		{"a reset with no parameters", "\x1b[mtext", "\x1b[0mtext"},
		{"modifyOtherKeys, which changes what keys send and outlasts the CLI", "\x1b[>4;2mtext", "text"},
		{"the key-modifier query, which answers on standard input", "\x1b[?1mtext", "text"},
		{"an intermediate byte", "\x1b[ 1mtext", "text"},
		{"a sign, which isn't a plain number", "\x1b[+1mtext", "text"},
		{"conceal, which hides what follows", "\x1b[8mhidden", "hidden"},
		{"conceal among colours, which go on", "\x1b[8;31mhidden", "\x1b[31mhidden" + r},
		{"reverse, whose text can match the ground", "\x1b[7mtext", "text"},
		{"blink", "\x1b[5mtext", "text"},
		{"a background, on which text can vanish", "\x1b[41;30mtext", "text"},
		{"black, on a dark terminal", "\x1b[30mtext", "text"},
		{"white, on a light one", "\x1b[97mtext", "text"},
		{"a palette colour, whose arguments aren't codes", "\x1b[38;5;8mtext", "text"},
		{"a palette colour beside a safe one", "\x1b[1;38;5;196;31mtext", "\x1b[1;31mtext" + r},
		{"a 24-bit colour", "\x1b[38;2;0;0;0mtext", "text"},
		{"a background palette colour", "\x1b[48;5;0mtext", "text"},
		{"colons, which aren't the plain form", "\x1b[38:5:1mtext", "text"},
		{"a number too long to mean anything", "\x1b[00000000031mtext", "text"},
		{"ESC before a C1 control, whose second byte must not be left", "\x1b\u009b2Jcleared", "2Jcleared"},
		{"a C1 CSI by itself", "\u009b8mtext", "8mtext"},
		{"a C1 string terminator", "\u009ctext", "text"},
		{"a lone C1 byte", "a\x9b2Jb", "a2Jb"},
		{"direction overrides", "safe\u202egnp.exe", "safe\ufffdgnp.exe"},
		{"direction isolates", "a\u2066b\u2069c", "a\ufffdb\ufffdc"},
	} {
		if got := terminalSafe(tc.in, true); got != tc.color {
			t.Errorf("%s: terminalSafe(%q) = %q, want %q", tc.name, tc.in, got, tc.color)
		}
		if plain := terminalSafe(tc.in, false); strings.IndexByte(plain, 0x1b) >= 0 || strings.IndexByte(plain, 0x9b) >= 0 {
			t.Errorf("%s: off a terminal, %q came through as %q", tc.name, tc.in, plain)
		}
	}
}

// One line can't hide what is printed after it, Dockwell's own markers included,
// or leave the terminal changed once the CLI has gone.
func TestALineCantHideWhatFollows(t *testing.T) {
	var out, errs bytes.Buffer
	p := &logPrinter{out: &out, err: &errs, color: true, sources: map[string]*dockwellv1.LogSource{}, servers: map[string]dockwellv1.ServerLogState{}}
	p.watch(&dockwellv1.WatchLogsResponse{Chunks: []*dockwellv1.LogChunk{{
		SourceId: "a",
		Lines: []*dockwellv1.LogLine{
			{Time: timestamppb.Now(), Text: "\x1b[8mconcealed\x1b[31m red with no end"},
			{Time: timestamppb.Now(), Text: "the next line"},
		},
	}}})
	first, rest, _ := strings.Cut(out.String(), "\n")
	if strings.Contains(first, "\x1b[8m") || !strings.HasSuffix(first, "\x1b[0m") {
		t.Errorf("a line left its colour on: %q", first)
	}
	if strings.Contains(rest, "\x1b[") {
		t.Errorf("the next line carries codes: %q", rest)
	}
	out.Reset()
	p.finish()
	if out.String() != "\x1b[0m" {
		t.Errorf("leaving wrote %q, want a reset", out.String())
	}
}

// fakeWatch is a stream of logs a test scripts, one list of messages and how they
// end for each call.
type fakeWatch struct {
	calls []fakeCall
	asked []string
}

type fakeCall struct {
	messages []*dockwellv1.WatchLogsResponse
	err      error
}

func (f *fakeWatch) NewRequest() proto.Message { return &dockwellv1.WatchLogsRequest{} }

func (f *fakeWatch) Response() protoreflect.MessageDescriptor {
	return (&dockwellv1.WatchLogsResponse{}).ProtoReflect().Descriptor()
}

func (f *fakeWatch) Streams() bool { return true }

func (f *fakeWatch) Invoke(_ context.Context, _ *Client, req proto.Message, each func(proto.Message) error) error {
	f.asked = append(f.asked, req.(*dockwellv1.WatchLogsRequest).GetResumeToken())
	call := f.calls[0]
	f.calls = f.calls[1:]
	for _, m := range call.messages {
		if err := each(m); err != nil {
			return err
		}
	}
	return call.err
}

// A stream that drops, as when the control plane gives up on a reader that stopped
// reading, such as a pager left open, carries on from where it was.
func TestFollowingCarriesOnWhenAStreamDrops(t *testing.T) {
	var out, errs bytes.Buffer
	line := func(text, token string) *dockwellv1.WatchLogsResponse {
		return &dockwellv1.WatchLogsResponse{ResumeToken: token, Chunks: []*dockwellv1.LogChunk{{
			SourceId: "a", Lines: []*dockwellv1.LogLine{{Time: timestamppb.Now(), Text: text}},
		}}}
	}
	watch := &fakeWatch{calls: []fakeCall{
		{messages: []*dockwellv1.WatchLogsResponse{line("first", "place-1")}, err: connect.NewError(connect.CodeUnavailable, errors.New("reset"))},
		{messages: []*dockwellv1.WatchLogsResponse{line("second", "place-2")}},
	}}
	r := &runner{env: &Env{Out: &out, Err: &errs}, opts: &options{}, client: &Client{URL: "http://x"}}
	if err := r.followLogs(t.Context(), &Method{Call: watch}, &dockwellv1.WatchLogsRequest{}); err != nil {
		t.Fatal(err)
	}
	if got := strings.Join(watch.asked, ","); got != ",place-1" {
		t.Errorf("it asked for places %q", got)
	}
	if !strings.Contains(out.String(), "first") || !strings.Contains(out.String(), "second") {
		t.Errorf("printed %q", out.String())
	}
	if !strings.Contains(errs.String(), "carries on from where it was") {
		t.Errorf("said %q", errs.String())
	}
}

// What can't get better isn't tried again.
func TestFollowingStopsAtWhatCantGetBetter(t *testing.T) {
	var out, errs bytes.Buffer
	refused := connect.NewError(connect.CodeInvalidArgument, errors.New("a search takes up to 1024 bytes"))
	watch := &fakeWatch{calls: []fakeCall{{err: refused}}}
	r := &runner{env: &Env{Out: &out, Err: &errs}, opts: &options{}, client: &Client{URL: "http://x"}}
	if err := r.followLogs(t.Context(), &Method{Call: watch}, &dockwellv1.WatchLogsRequest{}); err == nil {
		t.Error("a refusal ended with no error")
	}
	if len(watch.asked) != 1 {
		t.Errorf("it asked %d times", len(watch.asked))
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
	if got := out.String(); got != clock+"  hz-1 r4  long line … (2 KB more)\n" {
		t.Errorf("printed %q", got)
	}
	// What is missing is said apart from the lines, so standard output stays what
	// the container wrote.
	if got := errs.String(); !strings.Contains(got, "-- 1,234,567 lines not shown: this service wrote faster") {
		t.Errorf("said %q", got)
	}
	out.Reset()
	errs.Reset()

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
