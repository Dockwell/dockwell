// SPDX-License-Identifier: AGPL-3.0-only

package task

import (
	"bytes"
	"sync"
	"time"

	"google.golang.org/protobuf/types/known/timestamppb"

	dockwellv1 "github.com/dockwell/dockwell/gen/dockwell/v1"
)

// MaxOutput is how much of what a run prints it keeps: the last 64 KB, a screen or
// two of a script's log (ADR 0053).
const MaxOutput = 64 << 10

// output keeps the last MaxOutput bytes of what a run prints, a line at a time,
// each with when it came. It's an io.Writer for both stdout and stderr.
type output struct {
	mu      sync.Mutex
	now     func() time.Time
	lines   []line
	size    int   // bytes in lines
	cut     int64 // bytes let go of from the start
	partial []byte
	partAt  time.Time
}

type line struct {
	at   time.Time
	text string
}

func newOutput(now func() time.Time) *output { return &output{now: now} }

// Write takes what a run printed, keeping each whole line as it ends.
func (o *output) Write(p []byte) (int, error) {
	o.mu.Lock()
	defer o.mu.Unlock()
	n := len(p)
	for len(p) > 0 {
		if len(o.partial) == 0 {
			o.partAt = o.now()
		}
		i := bytes.IndexByte(p, '\n')
		if i < 0 {
			o.partial = append(o.partial, p...)
			// A line with no end yet is kept to MaxOutput, as a whole one is.
			if len(o.partial) >= MaxOutput {
				o.keep(o.partial)
				o.partial = nil
			}
			break
		}
		o.partial = append(o.partial, p[:i]...)
		o.keep(o.partial)
		o.partial = o.partial[:0]
		p = p[i+1:]
	}
	return n, nil
}

// keep adds a line, letting go of the oldest past MaxOutput.
func (o *output) keep(text []byte) {
	text = bytes.TrimSuffix(text, []byte("\r"))
	if len(text) > MaxOutput {
		o.cut += int64(len(text) - MaxOutput)
		text = text[len(text)-MaxOutput:]
	}
	o.lines = append(o.lines, line{at: o.partAt, text: string(text)})
	o.size += len(text) + 1
	for o.size > MaxOutput && len(o.lines) > 1 {
		o.cut += int64(len(o.lines[0].text) + 1)
		o.size -= len(o.lines[0].text) + 1
		o.lines[0] = line{}
		o.lines = o.lines[1:]
	}
}

// Line adds a line of Dockwell's own, such as why an attempt stopped.
func (o *output) Line(text string) {
	o.mu.Lock()
	defer o.mu.Unlock()
	o.partAt = o.now()
	o.keep([]byte(text))
}

// done is what was kept, with what was cut, once the run has ended.
func (o *output) done() ([]*dockwellv1.TaskOutputLine, int64) {
	o.mu.Lock()
	defer o.mu.Unlock()
	if len(o.partial) > 0 {
		o.keep(o.partial)
		o.partial = nil
	}
	out := make([]*dockwellv1.TaskOutputLine, 0, len(o.lines))
	for _, l := range o.lines {
		out = append(out, &dockwellv1.TaskOutputLine{Time: timestamppb.New(l.at), Text: l.text})
	}
	return out, o.cut
}
