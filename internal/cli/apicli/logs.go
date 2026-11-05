// SPDX-License-Identifier: AGPL-3.0-only

package apicli

import (
	"fmt"
	"io"
	"os"
	"slices"
	"strconv"
	"strings"
	"time"
	"unicode/utf8"

	"golang.org/x/term"

	dockwellv1 "github.com/dockwell/dockwell/gen/dockwell/v1"
)

// logPrinter prints log lines for people: one line of output for each, with
// its time, and its source when there is more than one. What it says about the
// stream, as when a server goes offline, goes to standard error, so standard
// output stays the lines themselves.
type logPrinter struct {
	out, err io.Writer
	// color keeps a line's own colours, on a terminal.
	color bool
	// printed says a line went out, which finish resets after.
	printed bool
	sources map[string]*dockwellv1.LogSource
	servers map[string]dockwellv1.ServerLogState
}

func newLogPrinter(out, err io.Writer) *logPrinter {
	color := false
	if f, ok := out.(*os.File); ok {
		color = term.IsTerminal(int(f.Fd()))
	}
	return &logPrinter{out: out, err: err, color: color, sources: map[string]*dockwellv1.LogSource{}, servers: map[string]dockwellv1.ServerLogState{}}
}

// shownLine is a line or a gap on its way out, in time order across sources.
type shownLine struct {
	at     time.Time
	source string
	line   *dockwellv1.LogLine
	gap    *dockwellv1.LogGap
}

func (p *logPrinter) watch(m *dockwellv1.WatchLogsResponse) {
	p.sourcesChanged(m.GetSources())
	p.statuses(m.GetServers())
	p.chunks(m.GetChunks())
}

func (p *logPrinter) read(m *dockwellv1.ReadLogsResponse) {
	p.sourcesChanged(m.GetSources())
	p.statuses(m.GetServers())
	p.chunks(m.GetChunks())
	if t := m.GetSearchedBackTo(); t != nil {
		reach := "as far back as the server has"
		switch {
		case m.GetSearchTimedOut():
			reach = "where the search stopped, after 30 s"
		case m.GetSearchCutShort():
			reach = "where the search stopped, at 1,000,000 lines of a container"
		}
		fmt.Fprintf(p.err, "Searched %s lines, back to %s, %s.\n", count(m.GetSearchedLines()), t.AsTime().Local().Format("2 Jan 15:04:05"), reach)
	}
	if t := m.GetNextPageToken(); t != "" {
		fmt.Fprintf(p.err, "There are older lines: pass --page-token %s.\n", t)
	}
}

func (p *logPrinter) sourcesChanged(sources []*dockwellv1.LogSource) {
	for _, s := range sources {
		p.sources[s.GetId()] = s
	}
}

// statuses says what changed of each server, once it is something to say: a
// server that reads as it did says nothing, and one that comes back says so.
func (p *logPrinter) statuses(list []*dockwellv1.ServerLogStatus) {
	for _, s := range list {
		was := p.servers[s.GetServerId()]
		p.servers[s.GetServerId()] = s.GetState()
		switch s.GetState() {
		case dockwellv1.ServerLogState_SERVER_LOG_STATE_READING:
			if was == dockwellv1.ServerLogState_SERVER_LOG_STATE_OFFLINE || was == dockwellv1.ServerLogState_SERVER_LOG_STATE_UNAVAILABLE {
				fmt.Fprintf(p.err, "%s is back, and its logs carry on.\n", s.GetServerName())
			}
		case dockwellv1.ServerLogState_SERVER_LOG_STATE_OFFLINE, dockwellv1.ServerLogState_SERVER_LOG_STATE_UNAVAILABLE:
			fmt.Fprintln(p.err, sentence(s.GetMessage()))
		case dockwellv1.ServerLogState_SERVER_LOG_STATE_UNSPECIFIED, dockwellv1.ServerLogState_SERVER_LOG_STATE_DONE:
		}
	}
}

// chunks prints chunks in time order, as a message may hold several sources'.
func (p *logPrinter) chunks(chunks []*dockwellv1.LogChunk) {
	var all []shownLine
	for _, c := range chunks {
		if g := c.GetGap(); g != nil {
			// Where the lines it stands for were, which a chunk's first line
			// may be well after.
			at := g.GetEndTime().AsTime()
			if g.GetEndTime() == nil && len(c.GetLines()) > 0 {
				at = c.GetLines()[0].GetTime().AsTime()
			}
			all = append(all, shownLine{at: at, source: c.GetSourceId(), gap: g})
		}
		for _, l := range c.GetLines() {
			all = append(all, shownLine{at: l.GetTime().AsTime(), source: c.GetSourceId(), line: l})
		}
	}
	slices.SortStableFunc(all, func(a, b shownLine) int { return a.at.Compare(b.at) })
	for _, s := range all {
		if s.gap != nil {
			// Standard output stays what the container wrote.
			fmt.Fprintln(p.err, gapWords(s.gap))
			continue
		}
		p.line(s.source, s.line)
	}
}

// finish leaves the terminal as it found it, whatever was printed last.
func (p *logPrinter) finish() {
	if p.color && p.printed {
		_, _ = io.WriteString(p.out, resetLine)
	}
}

func (p *logPrinter) line(source string, l *dockwellv1.LogLine) {
	var b strings.Builder
	b.WriteString(l.GetTime().AsTime().Local().Format("15:04:05.000"))
	b.WriteString("  ")
	if len(p.sources) > 1 {
		b.WriteString(p.label(source))
		b.WriteString("  ")
	}
	b.WriteString(terminalSafe(l.GetText(), p.color))
	if n := l.GetCutBytes(); n > 0 {
		fmt.Fprintf(&b, " … (%s more)", humanBytes(uint64(n)))
	}
	b.WriteByte('\n')
	_, _ = io.WriteString(p.out, b.String())
	p.printed = true
}

// label names a source: its server, and which of the server's it is when
// that alone wouldn't say.
func (p *logPrinter) label(id string) string {
	s := p.sources[id]
	if s == nil {
		return id
	}
	same := 0
	for _, o := range p.sources {
		if o.GetServerId() == s.GetServerId() {
			same++
		}
	}
	label := s.GetServerName()
	if s.GetReplica() > 1 {
		label += "-" + strconv.Itoa(int(s.GetReplica()))
	}
	if same > 1 {
		// By its own name, as a restart's has the same revision as what it replaces.
		if _, name, ok := strings.Cut(s.GetContainerName(), "-"); ok {
			return label + " " + name
		}
		if s.GetRevision() > 0 {
			label += " r" + strconv.FormatInt(s.GetRevision(), 10)
		}
	}
	return label
}

// gapWords says what a gap is missing, as the panel's marker does.
func gapWords(g *dockwellv1.LogGap) string {
	switch g.GetReason() {
	case dockwellv1.LogGapReason_LOG_GAP_REASON_TOO_FAST:
		return fmt.Sprintf("-- %s lines not shown: this service wrote faster than a live stream carries. Read them with dockwell logs read --", count(g.GetLines()))
	case dockwellv1.LogGapReason_LOG_GAP_REASON_SHARED:
		return fmt.Sprintf("-- %s lines not shown: this server shares what it carries live between everyone reading, and this service wrote faster than this reader's share. Read them with dockwell logs read --", count(g.GetLines()))
	case dockwellv1.LogGapReason_LOG_GAP_REASON_READER_BEHIND:
		return fmt.Sprintf("-- %s lines not shown: this reader fell behind. Read them with dockwell logs read --", count(g.GetLines()))
	case dockwellv1.LogGapReason_LOG_GAP_REASON_MATCH_LIMIT:
		return fmt.Sprintf("-- %s older matches not shown: a read to the end holds the newest 2 MB of each container. Read them with dockwell logs read --", count(g.GetLines()))
	case dockwellv1.LogGapReason_LOG_GAP_REASON_GONE:
		if g.GetEndTime() != nil {
			return fmt.Sprintf("-- Lines from %s to %s are gone: the server no longer has them --",
				g.GetStartTime().AsTime().Local().Format("15:04:05"), g.GetEndTime().AsTime().Local().Format("15:04:05"))
		}
		return fmt.Sprintf("-- Lines after %s are gone: the server no longer has this container's log --", g.GetStartTime().AsTime().Local().Format("15:04:05"))
	case dockwellv1.LogGapReason_LOG_GAP_REASON_UNSPECIFIED:
	}
	return "-- Some lines are missing here --"
}

// count writes a number with commas, as the panel does.
func count(n uint64) string {
	s := strconv.FormatUint(n, 10)
	for i := len(s) - 3; i > 0; i -= 3 {
		s = s[:i] + "," + s[i:]
	}
	return s
}

func humanBytes(n uint64) string {
	switch {
	case n >= 1<<20:
		return strconv.FormatFloat(float64(n)/(1<<20), 'f', 1, 64) + " MB"
	case n >= 1<<10:
		return strconv.FormatUint(n>>10, 10) + " KB"
	}
	return strconv.FormatUint(n, 10) + " bytes"
}

// terminalSafe makes a line safe to print: what a container prints could
// otherwise move the cursor, set the title, change what the keyboard sends,
// hide what is printed next, or rewrite what is on the screen. Only plain colour
// and weight survive, and only on a terminal, each from a short list, and closed
// at the line's end. Other control characters go, a tab stays, and so do those
// that read text backwards, which would make it say something else.
func terminalSafe(text string, color bool) string {
	var b strings.Builder
	b.Grow(len(text))
	styled := false
	for i := 0; i < len(text); {
		c := text[i]
		switch {
		case c == 0x1b:
			end, params := escapeEnd(text, i)
			if params != "" && color {
				b.WriteString("\x1b[")
				b.WriteString(params)
				b.WriteByte('m')
				styled = params != resetSGR
			}
			i = end
		case c == '\t':
			b.WriteByte(c)
			i++
		case c < 0x20 || c == 0x7f:
			i++
		case c < utf8.RuneSelf:
			b.WriteByte(c)
			i++
		default:
			r, size := utf8.DecodeRuneInString(text[i:])
			switch {
			case r == utf8.RuneError && size == 1:
				// Not UTF-8, so not text: a lone byte could read as a control.
			case r >= 0x80 && r <= 0x9f:
				// The C1 controls, which some terminals read as escapes too.
			case isDirectional(r):
				b.WriteRune(utf8.RuneError)
			default:
				b.WriteString(text[i : i+size])
			}
			i += size
		}
	}
	if styled {
		b.WriteString(resetLine)
	}
	return b.String()
}

const (
	resetSGR  = "0"
	resetLine = "\x1b[0m"
)

// isDirectional reports a character that reorders the text around it.
func isDirectional(r rune) bool {
	return (r >= 0x202a && r <= 0x202e) || (r >= 0x2066 && r <= 0x2069) || r == 0x200e || r == 0x200f || r == 0x061c
}

// escapeEnd finds the end of the escape sequence at i, and what of it may reach a
// terminal, as parameters for a colour code. It says nothing for what isn't one.
func escapeEnd(text string, i int) (int, string) {
	j := i + 1
	if j >= len(text) {
		return j, ""
	}
	switch text[j] {
	case '[':
		start := j + 1
		j = start
		for j < len(text) && text[j] >= 0x20 && text[j] < 0x40 {
			j++
		}
		if j >= len(text) {
			return j, ""
		}
		if text[j] != 'm' {
			return j + 1, ""
		}
		return j + 1, safeSGR(text[start:j])
	case ']', 'P', 'X', '^', '_':
		// A string, up to BEL or ST. Without one, the rest of the line is it.
		for j++; j < len(text); j++ {
			if text[j] == 0x07 {
				return j + 1, ""
			}
			if text[j] == 0x1b && j+1 < len(text) && text[j+1] == '\\' {
				return j + 2, ""
			}
		}
		return j, ""
	}
	// Two characters in all, whatever the second takes up: half of one would be
	// left behind as a raw byte.
	_, size := utf8.DecodeRuneInString(text[j:])
	return j + size, ""
}

// safeSGR keeps what a terminal can only show as colour or weight, from a colour
// code's parameters. Anything but digits and semicolons isn't a plain colour code:
// ESC[>4;2m changes what the keyboard sends, and ESC[?1m asks the terminal to
// answer on standard input. What can hide text goes: concealed, and backgrounds,
// reverse and black, white and the wider palettes, which can match the ground.
func safeSGR(params string) string {
	for _, c := range params {
		if c != ';' && (c < '0' || c > '9') {
			return ""
		}
	}
	if params == "" {
		return resetSGR
	}
	var kept []string
	fields := strings.Split(params, ";")
	for n := 0; n < len(fields); n++ {
		field := fields[n]
		if field == "" {
			field = resetSGR
		}
		code, err := strconv.Atoi(field)
		if err != nil || len(field) > 3 {
			return ""
		}
		switch {
		case code == 38 || code == 48 || code == 58:
			// These take arguments, which aren't codes of their own.
			if n+1 < len(fields) {
				switch fields[n+1] {
				case "5":
					n += 2
				case "2":
					n += 4
				}
			}
		case safeCodes[code]:
			kept = append(kept, strconv.Itoa(code))
		}
	}
	return strings.Join(kept, ";")
}

// safeCodes are the colour code parameters that reach a terminal: reset, bold,
// faint, italic, underline and their ends, the default colour, and the six hues
// and grey, plain and bright.
var safeCodes = map[int]bool{
	0: true, 1: true, 2: true, 3: true, 4: true, 22: true, 23: true, 24: true, 39: true,
	31: true, 32: true, 33: true, 34: true, 35: true, 36: true,
	90: true, 91: true, 92: true, 93: true, 94: true, 95: true, 96: true,
}

// sourcesTable lists where a service's logs come from.
func sourcesTable(w io.Writer, m *dockwellv1.ListLogSourcesResponse, now time.Time) {
	if len(m.GetSources()) == 0 {
		fmt.Fprintln(w, "No logs yet: this service has no containers, and its servers kept none.")
	} else {
		tw := newTable(w)
		row(tw, "ID", "Server", "Container", "Revision", "State", "Started", "Kept")
		for _, s := range m.GetSources() {
			revision := "-"
			if s.GetRevision() > 0 {
				revision = "r" + strconv.FormatInt(s.GetRevision(), 10)
			}
			row(tw, s.GetId(), s.GetServerName(), orDash(s.GetContainerName()), revision, word(s.GetState()), when(s.GetStartTime(), now), orDash(word(s.GetKept())))
		}
		_ = tw.Flush()
	}
	for _, s := range m.GetServers() {
		fmt.Fprintln(w, sentence(s.GetMessage()))
	}
}
