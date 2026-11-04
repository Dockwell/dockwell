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
	color   bool
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
		if m.GetSearchCutShort() {
			reach = "where the search stopped, at its limit"
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
			at := g.GetEndTime().AsTime()
			if len(c.GetLines()) > 0 {
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
			fmt.Fprintln(p.out, gapWords(s.gap))
			continue
		}
		p.line(s.source, s.line)
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
	if same > 1 && s.GetRevision() > 0 {
		label += " r" + strconv.FormatInt(s.GetRevision(), 10)
	}
	return label
}

// gapWords says what a gap is missing, as the panel's marker does.
func gapWords(g *dockwellv1.LogGap) string {
	switch g.GetReason() {
	case dockwellv1.LogGapReason_LOG_GAP_REASON_TOO_FAST:
		return fmt.Sprintf("-- %s lines not shown: this service wrote faster than a live stream carries. Read them with dockwell logs read --", count(g.GetLines()))
	case dockwellv1.LogGapReason_LOG_GAP_REASON_READER_BEHIND:
		return fmt.Sprintf("-- %s lines not shown: this reader fell behind. Read them with dockwell logs read --", count(g.GetLines()))
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
// otherwise move the cursor, set the title, or rewrite what is on the screen.
// Only colour codes survive, and only on a terminal. Other control characters
// go, a tab stays.
func terminalSafe(text string, color bool) string {
	var b strings.Builder
	b.Grow(len(text))
	for i := 0; i < len(text); {
		c := text[i]
		switch {
		case c == 0x1b:
			end, keep := escapeEnd(text, i)
			if keep && color {
				b.WriteString(text[i:end])
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
			// The C1 controls, which some terminals read as escapes too.
			if r < 0x80 || r > 0x9f {
				b.WriteString(text[i : i+size])
			}
			i += size
		}
	}
	return b.String()
}

// escapeEnd finds the end of the escape sequence at i, and whether it only
// sets colours: CSI parameters ending in m.
func escapeEnd(text string, i int) (int, bool) {
	j := i + 1
	if j >= len(text) {
		return j, false
	}
	switch text[j] {
	case '[':
		j++
		for j < len(text) && text[j] >= 0x20 && text[j] < 0x40 {
			j++
		}
		if j >= len(text) {
			return j, false
		}
		return j + 1, text[j] == 'm'
	case ']', 'P', 'X', '^', '_':
		// A string, up to BEL or ST. Without one, the rest of the line is it.
		for j++; j < len(text); j++ {
			if text[j] == 0x07 {
				return j + 1, false
			}
			if text[j] == 0x1b && j+1 < len(text) && text[j+1] == '\\' {
				return j + 2, false
			}
		}
		return j, false
	}
	return j + 1, false
}

// sourcesTable lists where a service's logs come from.
func sourcesTable(w io.Writer, m *dockwellv1.ListLogSourcesResponse, now time.Time) {
	if len(m.GetSources()) == 0 {
		fmt.Fprintln(w, "No logs yet: this service has no containers, and its servers kept none.")
	} else {
		tw := newTable(w)
		row(tw, "ID", "Server", "Replica", "Revision", "State", "Started", "Kept")
		for _, s := range m.GetSources() {
			revision := "-"
			if s.GetRevision() > 0 {
				revision = "r" + strconv.FormatInt(s.GetRevision(), 10)
			}
			row(tw, s.GetId(), s.GetServerName(), strconv.Itoa(int(s.GetReplica())), revision, word(s.GetState()), when(s.GetStartTime(), now), orDash(word(s.GetKept())))
		}
		_ = tw.Flush()
	}
	for _, s := range m.GetServers() {
		fmt.Fprintln(w, sentence(s.GetMessage()))
	}
}
