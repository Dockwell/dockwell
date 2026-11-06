// SPDX-License-Identifier: AGPL-3.0-only

package docker

import (
	"bufio"
	"bytes"
	"context"
	"encoding/binary"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strconv"
	"time"
	"unsafe"
)

// LogConfig is how Docker keeps a container's log.
type LogConfig struct {
	Type   string            `json:"Type"`
	Config map[string]string `json:"Config,omitempty"`
}

// Docker's log drivers that Dockwell treats by name.
const (
	LogDriverJSONFile = "json-file"
	LogDriverLocal    = "local"
)

// Rotation is what Dockwell asks of json-file, Docker's default driver, which
// keeps a log without a limit unless told otherwise: 5 files of 10 MiB, the
// four older ones compressed, so a container's log never takes more than
// 50 MiB of disk. See docs/adr/0025-logs-per-deploy.md.
func Rotation() *LogConfig {
	return &LogConfig{Type: LogDriverJSONFile, Config: map[string]string{
		"max-size": "10m",
		"max-file": "5",
		"compress": "true",
	}}
}

// Unbounded reports a log Docker keeps without a limit: json-file with no
// max-size. Any other driver is the operator's choice, and local rotates by
// itself.
func (c LogConfig) Unbounded() bool {
	return c.Type == LogDriverJSONFile && c.Config["max-size"] == ""
}

// LogOptions says what to read of a container's log.
type LogOptions struct {
	// Follow goes on with what is written next, until the container stops.
	Follow bool
	// Tail reads only this many of the newest lines. Zero reads all of them,
	// and less than zero none, for a follow of what comes next alone.
	Tail int
	// Since and Until bound the lines by when they were written, each
	// inclusive. Docker reads either side down to the nanosecond.
	Since, Until time.Time
}

// LogStream says which of a container's outputs a line came from.
type LogStream byte

// A container's outputs, as Docker's multiplexed stream numbers them.
const (
	Stdout LogStream = 1
	Stderr LogStream = 2
)

// MaxLogLine is how much of a line a LogReader keeps, in bytes. Docker splits
// a longer line into parts of 16 KiB, and the rest of it is counted, not
// kept.
const MaxLogLine = 16 << 10

// LogEntry is one line of a log. Its slices are valid until the next call to
// Next.
type LogEntry struct {
	Stream LogStream
	// Time is the time as Docker wrote it, in RFC 3339 with nanoseconds, or
	// empty when the line came without one. ParseLogTime reads it.
	Time []byte
	// Text is the line, without its line ending, at most MaxLogLine bytes.
	Text []byte
	// Cut counts the bytes of the line past Text.
	Cut int
	// Size is how much the line took on the wire: all of it, what was cut too.
	Size int
}

// ParseLogTime reads a time as a LogEntry has it.
func ParseLogTime(b []byte) (time.Time, bool) {
	if len(b) == 0 {
		return time.Time{}, false
	}
	// time.Parse keeps nothing of its argument, so this copies nothing per
	// line, on a path that sees every line of a noisy container.
	t, err := time.Parse(time.RFC3339Nano, unsafe.String(&b[0], len(b))) //nolint:gosec // time.Parse keeps nothing of it
	return t, err == nil
}

// LogReader reads a container's log, one line at a time, without allocating
// per line: a noisy container's lines may be dropped as soon as they are
// seen, and seeing them should cost almost nothing.
type LogReader struct {
	body io.ReadCloser
	br   *bufio.Reader
	tty  bool

	hdr   [8]byte
	frame []byte
	// A line longer than 16 KiB comes in parts, each without a newline, and
	// the two outputs can take turns. So each has its own line under way.
	parts [3]partial
	entry LogEntry
}

type partial struct {
	buf  []byte
	time []byte
	cut  int
	size int
}

// ContainerLogs reads a container's log, stdout and stderr both, with each
// line's time. tty is whether the container has one, as Config says, since
// such a container's log isn't multiplexed. Close what it returns, or cancel
// ctx.
func (c *Client) ContainerLogs(ctx context.Context, id string, tty bool, o LogOptions) (*LogReader, error) {
	path, err := c.versioned(ctx, "/containers/"+esc(id)+"/logs")
	if err != nil {
		return nil, err
	}
	q := url.Values{"stdout": {"1"}, "stderr": {"1"}, "timestamps": {"1"}, "tail": {"all"}}
	if o.Follow {
		q.Set("follow", "1")
	}
	if o.Tail != 0 {
		q.Set("tail", strconv.Itoa(max(o.Tail, 0)))
	}
	if !o.Since.IsZero() {
		q.Set("since", logTime(o.Since))
	}
	if !o.Until.IsZero() {
		q.Set("until", logTime(o.Until))
	}
	res, err := c.send(ctx, http.MethodGet, path, q, nil) //nolint:bodyclose // the reader closes it
	if err != nil {
		return nil, err
	}
	return newLogReader(res.Body, tty), nil
}

// logTime is a time as the engine takes it in since and until: seconds and
// nanoseconds, as the docker CLI sends them.
func logTime(t time.Time) string {
	return fmt.Sprintf("%d.%09d", t.Unix(), t.Nanosecond())
}

func newLogReader(body io.ReadCloser, tty bool) *LogReader {
	return &LogReader{body: body, br: bufio.NewReaderSize(body, 64<<10), tty: tty, frame: make([]byte, 0, MaxLogLine+64)}
}

// Close stops reading.
func (r *LogReader) Close() error { return r.body.Close() }

// Next returns the next line of the log, or the error that ended it: io.EOF
// at its end, and anything else if it stopped before that.
func (r *LogReader) Next() (*LogEntry, error) {
	if r.tty {
		return r.nextRaw()
	}
	for {
		// In the reader, as a local would be allocated for each line.
		hdr := r.hdr[:]
		if _, err := io.ReadFull(r.br, hdr); err != nil {
			return r.ended(err)
		}
		stream := LogStream(hdr[0])
		if stream != Stdout && stream != Stderr {
			stream = Stdout
		}
		size := int(binary.BigEndian.Uint32(hdr[4:]))
		keep := min(size, cap(r.frame))
		r.frame = r.frame[:keep]
		if _, err := io.ReadFull(r.br, r.frame); err != nil {
			return r.ended(err)
		}
		// A frame past what is kept, which Docker doesn't send but a stream
		// could: all that matters of the rest is whether the line ends in it.
		over, ended := size-keep, bytes.HasSuffix(r.frame, newline)
		if over > 0 {
			if _, err := r.br.Discard(over - 1); err != nil {
				return r.ended(err)
			}
			last, err := r.br.ReadByte()
			if err != nil {
				return r.ended(err)
			}
			if ended = last == '\n'; ended {
				over--
			}
		}
		if e := r.take(stream, r.frame, over, size, ended); e != nil {
			return e, nil
		}
	}
}

// nextRaw reads a container with a tty, whose log is plain lines, with no
// frames around them, all as stdout.
func (r *LogReader) nextRaw() (*LogEntry, error) {
	for {
		line, err := r.br.ReadSlice('\n')
		// A very long line fills the buffer, and what there is so far is a
		// part of it.
		if err == nil || errors.Is(err, bufio.ErrBufferFull) {
			if e := r.take(Stdout, line, 0, len(line), err == nil); e != nil {
				return e, nil
			}
			continue
		}
		if len(line) > 0 {
			if e := r.take(Stdout, line, 0, len(line), false); e != nil {
				return e, nil
			}
		}
		return r.ended(err)
	}
}

// ended gives what a broken or finished stream left under way, then its error:
// io.EOF for a log that was read to its end, and io.ErrUnexpectedEOF for one
// cut off, as when Docker goes away partway.
func (r *LogReader) ended(err error) (*LogEntry, error) {
	for s := range r.parts {
		if p := &r.parts[s]; len(p.buf) > 0 || p.cut > 0 {
			e := r.finish(LogStream(s), p)
			return e, nil
		}
	}
	return nil, err
}

var newline = []byte{'\n'}

// take takes a frame's payload, which starts with its time, and returns the
// line it finishes, if any. over is how much of the frame was too big to read,
// and ended whether the line ends with this frame.
func (r *LogReader) take(s LogStream, payload []byte, over, size int, ended bool) *LogEntry {
	ts, text := payload, payload[:0]
	if i := bytes.IndexByte(payload, ' '); i >= 0 {
		ts, text = payload[:i], payload[i+1:]
	} else if len(bytes.TrimRight(payload, "\r\n")) == 0 {
		// A bare newline, as a line with no time.
		ts = nil
	}
	if ended {
		text = bytes.TrimRight(text, "\r\n")
	}
	p := &r.parts[s]
	if ended && over == 0 && len(p.buf) == 0 && p.cut == 0 {
		r.entry = LogEntry{Stream: s, Time: ts, Text: text[:min(len(text), MaxLogLine)], Cut: max(0, len(text)-MaxLogLine), Size: size}
		return &r.entry
	}
	if len(p.buf) == 0 && p.cut == 0 {
		p.time = append(p.time[:0], ts...)
	}
	room := MaxLogLine - len(p.buf)
	n := min(room, len(text))
	p.buf = append(p.buf, text[:n]...)
	p.cut += len(text) - n + over
	p.size += size
	if !ended {
		return nil
	}
	return r.finish(s, p)
}

// finish turns a line under way into an entry. Its bytes stay p's until the
// next part of that output starts over.
func (r *LogReader) finish(s LogStream, p *partial) *LogEntry {
	text := bytes.TrimRight(p.buf, "\r\n")
	r.entry = LogEntry{Stream: s, Time: p.time, Text: text, Cut: p.cut, Size: p.size}
	p.buf, p.cut, p.size = p.buf[:0], 0, 0
	return &r.entry
}
