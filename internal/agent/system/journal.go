// SPDX-License-Identifier: AGPL-3.0-only

package system

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"sync"
	"time"
	"unicode/utf8"

	"google.golang.org/protobuf/types/known/timestamppb"

	dockwellv1 "github.com/dockwell/dockwell/gen/dockwell/v1"
	"github.com/dockwell/dockwell/internal/agent/hosttool"
	"github.com/dockwell/dockwell/internal/agent/logs"
)

// Reading the system log back (ADR 0067, ADR 0082): the journal through journalctl,
// newest first, a page at a time, with only the fields a line shows; or the end of
// /var/log/messages on a server that keeps no journal.

// MaxLines is the most lines a page holds.
const MaxLines = 200

// maxMessage is where a line is cut, as a container's are (ADR 0025).
const maxMessage = 16 << 10

// fileTail is how much of /var/log/messages a page reads, at most.
const fileTail = 1 << 20

var journalctl = hosttool.Tool{Name: "journalctl", Paths: []string{"/usr/bin/journalctl", "/bin/journalctl"}, Env: []string{"TZ=UTC", "SYSTEMD_COLORS=0"}}

// journalFields are all a line reads of the journal. Never _CMDLINE, which can
// hold a password typed with it. journalctl adds the cursor and the time itself.
var journalFields = "MESSAGE,PRIORITY,_SYSTEMD_UNIT,SYSLOG_IDENTIFIER,_PID"

// Page is a page of the system log, newest first.
type Page struct {
	Lines  []*dockwellv1.SystemLogLine
	More   bool
	Source dockwellv1.SystemLogSource
}

// Journal reads a page of the server's system log, newest first: what unit wrote
// (empty for everything), at level or worse (unset for every level), older than the
// line at before (empty for the newest), at most limit lines.
func (r *Reader) Journal(ctx context.Context, unit string, level dockwellv1.LogLevel, before string, limit int) (*Page, error) {
	if r.Unavailable != "" {
		return nil, errors.New(r.Unavailable)
	}
	if limit <= 0 || limit > MaxLines {
		limit = MaxLines
	}
	if unit != "" && !unitName().MatchString(unit) && !rcName().MatchString(unit) {
		return nil, fmt.Errorf("%q isn't a system service's name", unit)
	}
	switch {
	case r.exists("run/systemd/journal"):
		return r.journal(ctx, unit, level, before, limit)
	case r.exists(filepath.Join("var", "log", "messages")):
		return r.messages(unit, level, before, limit)
	}
	return &Page{Source: dockwellv1.SystemLogSource_SYSTEM_LOG_SOURCE_NONE}, nil
}

func (r *Reader) journal(ctx context.Context, unit string, level dockwellv1.LogLevel, before string, limit int) (*Page, error) {
	if strings.ContainsAny(before, "\n\x00") {
		return nil, errors.New("that isn't a cursor of this server's journal")
	}
	n := limit + 1
	args := []string{"-o", "json", "--system", "--no-pager", "--reverse", "--output-fields=" + journalFields}
	if unit != "" {
		args = append(args, "--unit="+unit)
	}
	switch level {
	case dockwellv1.LogLevel_LOG_LEVEL_ERROR:
		args = append(args, "--priority=err")
	case dockwellv1.LogLevel_LOG_LEVEL_WARN:
		args = append(args, "--priority=warning")
	case dockwellv1.LogLevel_LOG_LEVEL_UNSPECIFIED, dockwellv1.LogLevel_LOG_LEVEL_DEBUG, dockwellv1.LogLevel_LOG_LEVEL_INFO:
	}
	if before != "" {
		// Reading from the cursor, newest first, starts with its own line, which the
		// page before already had.
		args = append(args, "--cursor="+before)
		n++
	}
	args = append(args, "--lines="+strconv.Itoa(n))
	res, err := r.Tools.Run(ctx, journalctl, Deadline, nil, args...)
	if err == nil && res.Code != 0 && strings.Contains(res.Errs, "output-fields") {
		// systemd before 236 has no --output-fields: the fields are read and dropped.
		args = dropArg(args, "--output-fields="+journalFields)
		res, err = r.Tools.Run(ctx, journalctl, Deadline, nil, args...)
	}
	if err != nil {
		return nil, fmt.Errorf("can't read the server's journal: %w", err)
	}
	if res.Code != 0 {
		return nil, fmt.Errorf("can't read the server's journal: %w", hosttool.Fail("journalctl", res))
	}
	page := &Page{Source: dockwellv1.SystemLogSource_SYSTEM_LOG_SOURCE_JOURNAL}
	sc := bufio.NewScanner(bytes.NewReader(res.Out))
	sc.Buffer(make([]byte, 64<<10), hosttool.MaxOutput)
	for sc.Scan() {
		line, ok := journalLine(sc.Bytes())
		if !ok || before != "" && line.GetCursor() == before {
			continue
		}
		if len(page.Lines) == limit {
			page.More = true
			break
		}
		page.Lines = append(page.Lines, line)
	}
	return page, nil
}

func dropArg(args []string, arg string) []string {
	out := args[:0:0]
	for _, a := range args {
		if a != arg {
			out = append(out, a)
		}
	}
	return out
}

// journalLine reads one entry of journalctl's JSON. A field journald keeps as bytes
// is an array of numbers: text when they're UTF-8, as a coloured line is, and
// otherwise said as bytes.
func journalLine(b []byte) (*dockwellv1.SystemLogLine, bool) {
	var e map[string]json.RawMessage
	if json.Unmarshal(b, &e) != nil {
		return nil, false
	}
	str := func(k string) string {
		raw, ok := e[k]
		if !ok {
			return ""
		}
		var s string
		if json.Unmarshal(raw, &s) == nil {
			return s
		}
		var bs []byte
		var nums []int
		if json.Unmarshal(raw, &nums) == nil {
			bs = make([]byte, 0, len(nums))
			for _, n := range nums {
				// journald writes each byte as a number from 0 to 255.
				bs = append(bs, byte(n&0xff))
			}
			if utf8.Valid(bs) {
				return string(bs)
			}
			return fmt.Sprintf("a binary message of %d bytes", len(bs))
		}
		return ""
	}
	line := &dockwellv1.SystemLogLine{Cursor: str("__CURSOR"), Message: cut(strings.TrimRight(str("MESSAGE"), "\n"))}
	if line.Cursor == "" {
		return nil, false
	}
	if us, err := strconv.ParseInt(str("__REALTIME_TIMESTAMP"), 10, 64); err == nil {
		line.Time = timestamppb.New(time.UnixMicro(us))
	}
	line.Unit = str("_SYSTEMD_UNIT")
	if id := str("SYSLOG_IDENTIFIER"); line.Unit == "" || line.Unit == "init.scope" && id != "" {
		line.Unit = id
	}
	if pid, err := strconv.ParseUint(str("_PID"), 10, 32); err == nil {
		line.Pid = uint32(pid)
	}
	if line.Unit == "init.scope" && line.Pid == 1 {
		// systemd's own lines, about a unit, as journald keeps them without an identifier.
		line.Unit = "systemd"
	}
	line.Level = priorityLevel(str("PRIORITY"))
	return line, true
}

func cut(s string) string {
	if len(s) <= maxMessage {
		return s
	}
	s = s[:maxMessage]
	for !utf8.ValidString(s) {
		s = s[:len(s)-1]
	}
	return s
}

// priorityLevel is a journal priority's level: emergency to error read as errors,
// warning as warnings, notice and info as info, debug as debug.
func priorityLevel(p string) dockwellv1.LogLevel {
	switch p {
	case "0", "1", "2", "3":
		return dockwellv1.LogLevel_LOG_LEVEL_ERROR
	case "4":
		return dockwellv1.LogLevel_LOG_LEVEL_WARN
	case "7":
		return dockwellv1.LogLevel_LOG_LEVEL_DEBUG
	}
	return dockwellv1.LogLevel_LOG_LEVEL_INFO
}

// messagesLine is a line of a syslog file: a traditional or an RFC 3339 time, the
// host, busybox's facility.level where it writes one, the program and its PID.
var messagesLine = sync.OnceValue(func() *regexp.Regexp {
	return regexp.MustCompile(`^(?:([A-Z][a-z]{2} [ \d]\d \d\d:\d\d:\d\d)|(\d{4}-\d\d-\d\dT\S+)) \S+ (?:[a-z0-9]+\.([a-z]+) )?([^\s:\[]+)(?:\[(\d+)\])?: ?(.*)$`)
})

var syslogLevels = map[string]dockwellv1.LogLevel{
	"emerg": dockwellv1.LogLevel_LOG_LEVEL_ERROR, "alert": dockwellv1.LogLevel_LOG_LEVEL_ERROR, "crit": dockwellv1.LogLevel_LOG_LEVEL_ERROR,
	"err": dockwellv1.LogLevel_LOG_LEVEL_ERROR, "warn": dockwellv1.LogLevel_LOG_LEVEL_WARN, "warning": dockwellv1.LogLevel_LOG_LEVEL_WARN,
	"notice": dockwellv1.LogLevel_LOG_LEVEL_INFO, "info": dockwellv1.LogLevel_LOG_LEVEL_INFO, "debug": dockwellv1.LogLevel_LOG_LEVEL_DEBUG,
}

// messages reads the end of /var/log/messages, newest first: before is the byte
// where the page before began.
func (r *Reader) messages(unit string, level dockwellv1.LogLevel, before string, limit int) (*Page, error) {
	root := r.Root
	if root == "" {
		root = "/"
	}
	f, err := os.Open(filepath.Join(root, "var", "log", "messages")) //nolint:gosec // G304: the server's own syslog file, below the stand's root in a test
	if err != nil {
		return nil, fmt.Errorf("can't read the server's system log: %w", err)
	}
	defer f.Close()
	fi, err := f.Stat()
	if err != nil || !fi.Mode().IsRegular() {
		return nil, errors.New("can't read the server's system log: /var/log/messages isn't a file")
	}
	end := fi.Size()
	if before != "" {
		at, err := strconv.ParseInt(strings.TrimPrefix(before, "file:"), 10, 64)
		if err != nil || !strings.HasPrefix(before, "file:") || at < 0 || at > end {
			return nil, errors.New("that isn't a place in this server's system log")
		}
		end = at
	}
	start := max(end-fileTail, 0)
	buf := make([]byte, end-start)
	if _, err := f.ReadAt(buf, start); err != nil && !errors.Is(err, io.EOF) {
		return nil, fmt.Errorf("can't read the server's system log: %w", err)
	}
	if start > 0 {
		// The first line is cut: it's the next page's.
		if i := bytes.IndexByte(buf, '\n'); i >= 0 {
			start += int64(i + 1)
			buf = buf[i+1:]
		}
	}
	page := &Page{Source: dockwellv1.SystemLogSource_SYSTEM_LOG_SOURCE_FILE}
	year := time.Now().Year()
	// Each line with where it begins, read from the end.
	var begins []int
	for i := 0; i < len(buf); {
		begins = append(begins, i)
		j := bytes.IndexByte(buf[i:], '\n')
		if j < 0 {
			break
		}
		i += j + 1
	}
	for k := len(begins) - 1; k >= 0; k-- {
		text := buf[begins[k]:]
		if k+1 < len(begins) {
			text = buf[begins[k]:begins[k+1]]
		}
		line, ok := messagesEntry(strings.TrimRight(string(text), "\n"), year)
		if !ok || unit != "" && line.GetUnit() != unit && line.GetUnit()+".service" != unit {
			continue
		}
		if level > dockwellv1.LogLevel_LOG_LEVEL_INFO && line.GetLevel() < level {
			continue
		}
		if len(page.Lines) == limit {
			page.More = true
			break
		}
		line.Cursor = "file:" + strconv.FormatInt(start+int64(begins[k]), 10)
		page.Lines = append(page.Lines, line)
	}
	if !page.More && start > 0 {
		page.More = true
	}
	return page, nil
}

func messagesEntry(text string, year int) (*dockwellv1.SystemLogLine, bool) {
	m := messagesLine().FindStringSubmatch(text)
	if m == nil {
		return nil, false
	}
	line := &dockwellv1.SystemLogLine{Unit: m[4], Message: cut(m[6])}
	switch {
	case m[1] != "":
		if t, err := time.Parse("Jan _2 15:04:05 2006", m[1]+" "+strconv.Itoa(year)); err == nil {
			// A line from December read in January is last year's.
			if t.After(time.Now().Add(24 * time.Hour)) {
				t = t.AddDate(-1, 0, 0)
			}
			line.Time = timestamppb.New(t)
		}
	case m[2] != "":
		if t, err := time.Parse(time.RFC3339Nano, m[2]); err == nil {
			line.Time = timestamppb.New(t)
		}
	}
	if pid, err := strconv.ParseUint(m[5], 10, 32); err == nil {
		line.Pid = uint32(pid)
	}
	if l, ok := syslogLevels[m[3]]; ok {
		line.Level = l
	} else {
		line.Level = logs.ReadLevel([]byte(m[6]))
		if line.Level == dockwellv1.LogLevel_LOG_LEVEL_UNSPECIFIED {
			line.Level = dockwellv1.LogLevel_LOG_LEVEL_INFO
		}
	}
	return line, true
}

// WhyLines is how many lines say why a unit failed.
const WhyLines = 3

// Why picks, from a unit's newest lines, the last few its own process wrote, oldest
// first: not systemd's, which only say that it failed and with what result.
func Why(lines []*dockwellv1.SystemLogLine) []string {
	var why []string
	for _, l := range lines {
		if l.GetPid() == 1 || l.GetUnit() == "systemd" || strings.TrimSpace(l.GetMessage()) == "" {
			continue
		}
		why = append(why, l.GetMessage())
		if len(why) == WhyLines {
			break
		}
	}
	for i, j := 0, len(why)-1; i < j; i, j = i+1, j-1 {
		why[i], why[j] = why[j], why[i]
	}
	return why
}

// Why reads why a unit failed: the last lines its own process wrote, from its 20
// newest. Nothing when none can be read.
func (r *Reader) Why(ctx context.Context, name string) []string {
	page, err := r.Journal(ctx, name, dockwellv1.LogLevel_LOG_LEVEL_UNSPECIFIED, "", 20)
	if err != nil {
		return nil
	}
	return Why(page.Lines)
}
