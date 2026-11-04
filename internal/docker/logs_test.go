// SPDX-License-Identifier: AGPL-3.0-only

package docker

import (
	"bytes"
	"encoding/binary"
	"errors"
	"io"
	"strings"
	"testing"
	"time"
)

const stamp = "2026-11-04T14:19:58.012345678Z"

// frame is a line as the engine's multiplexed stream carries it.
func frame(s LogStream, payload string) []byte {
	var hdr [8]byte
	hdr[0] = byte(s)
	binary.BigEndian.PutUint32(hdr[4:], uint32(len(payload)))
	return append(hdr[:], payload...)
}

func reader(tty bool, chunks ...[]byte) *LogReader {
	return newLogReader(io.NopCloser(bytes.NewReader(bytes.Join(chunks, nil))), tty)
}

type got struct {
	stream LogStream
	time   string
	text   string
	cut    int
}

func readAll(t *testing.T, r *LogReader) []got {
	t.Helper()
	var out []got
	for {
		e, err := r.Next()
		if errors.Is(err, io.EOF) {
			return out
		}
		if err != nil {
			t.Fatal(err)
		}
		out = append(out, got{e.Stream, string(e.Time), string(e.Text), e.Cut})
	}
}

func TestReadsFrames(t *testing.T) {
	r := reader(false,
		frame(Stdout, stamp+" GET / 200\n"),
		frame(Stderr, stamp+" oops\r\n"),
		frame(Stdout, stamp+" \n"),
	)
	lines := readAll(t, r)
	want := []got{{Stdout, stamp, "GET / 200", 0}, {Stderr, stamp, "oops", 0}, {Stdout, stamp, "", 0}}
	for i := range want {
		if i >= len(lines) || lines[i] != want[i] {
			t.Fatalf("lines = %+v, want %+v", lines, want)
		}
	}
}

func TestJoinsALineDockerSplit(t *testing.T) {
	part := strings.Repeat("a", 16<<10)
	r := reader(false,
		frame(Stdout, stamp+" "+part),
		// The other output can take its turn between the parts.
		frame(Stderr, stamp+" between\n"),
		frame(Stdout, stamp+" "+part),
		frame(Stdout, stamp+" end\n"),
	)
	lines := readAll(t, r)
	if len(lines) != 2 {
		t.Fatalf("got %d lines, want 2", len(lines))
	}
	if lines[0].stream != Stderr || lines[0].text != "between" {
		t.Errorf("first line = %+v", lines[0])
	}
	if lines[1].stream != Stdout || len(lines[1].text) != MaxLogLine || lines[1].cut != (16<<10)+3 {
		t.Errorf("joined line has %d bytes and %d cut, want %d and %d", len(lines[1].text), lines[1].cut, MaxLogLine, (16<<10)+3)
	}
	if lines[1].time != stamp {
		t.Errorf("joined line's time = %q", lines[1].time)
	}
}

func TestAFrameTooBigIsCutNotBuffered(t *testing.T) {
	big := strings.Repeat("b", 1<<20)
	r := reader(false, frame(Stdout, stamp+" "+big+"\n"), frame(Stdout, stamp+" next\n"))
	lines := readAll(t, r)
	if len(lines) != 2 || lines[1].text != "next" {
		t.Fatalf("lines = %d, want the big one then next", len(lines))
	}
	if len(lines[0].text) != MaxLogLine {
		t.Errorf("kept %d bytes, want %d", len(lines[0].text), MaxLogLine)
	}
	if kept := len(lines[0].text) + lines[0].cut; kept != 1<<20 {
		t.Errorf("kept and cut add up to %d, want %d", kept, 1<<20)
	}
}

func TestReadsATerminalsPlainLines(t *testing.T) {
	r := reader(true, []byte(stamp+" one\r\n"+stamp+" two\r\n"+stamp+" partial"))
	lines := readAll(t, r)
	if len(lines) != 3 || lines[0].text != "one" || lines[1].text != "two" || lines[2].text != "partial" {
		t.Fatalf("lines = %+v", lines)
	}
}

func TestALineCutOffAtTheEndStillComes(t *testing.T) {
	r := reader(false, frame(Stdout, stamp+" whole\n"), frame(Stdout, stamp+" cut")[:12])
	lines := readAll(t, r)
	if len(lines) != 1 || lines[0].text != "whole" {
		t.Fatalf("lines = %+v", lines)
	}
}

func TestParseLogTime(t *testing.T) {
	at, ok := ParseLogTime([]byte(stamp))
	if !ok || !at.Equal(time.Date(2026, 11, 4, 14, 19, 58, 12_345_678, time.UTC)) {
		t.Errorf("ParseLogTime = %v, %v", at, ok)
	}
	if _, ok := ParseLogTime(nil); ok {
		t.Error("an empty time parsed")
	}
	if _, ok := ParseLogTime([]byte("not a time")); ok {
		t.Error("nonsense parsed")
	}
}

// A line that is dropped costs almost nothing, as a flood is all such lines.
func TestReadingALineDoesntAllocate(t *testing.T) {
	data := bytes.Repeat(frame(Stdout, stamp+" GET /v1/products/1 200 3 ms\n"), 1000)
	body := bytes.NewReader(data)
	r := newLogReader(io.NopCloser(body), false)
	n := testing.AllocsPerRun(100, func() {
		if _, err := r.Next(); err != nil {
			body.Reset(data)
			r.br.Reset(body)
		}
	})
	if n > 0 {
		t.Errorf("reading a line allocates %v times", n)
	}
}

func TestRotationIsBounded(t *testing.T) {
	r := Rotation()
	if r.Type != LogDriverJSONFile || r.Config["max-size"] == "" || r.Config["max-file"] == "" {
		t.Fatalf("Rotation = %+v", r)
	}
	if r.Unbounded() {
		t.Error("Rotation says it is unbounded")
	}
	if !(LogConfig{Type: LogDriverJSONFile}).Unbounded() {
		t.Error("json-file with no max-size isn't unbounded")
	}
	if (LogConfig{Type: LogDriverLocal}).Unbounded() {
		t.Error("local rotates by itself")
	}
}
