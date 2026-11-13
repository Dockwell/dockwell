// SPDX-License-Identifier: AGPL-3.0-only

package logs

import (
	"context"
	"fmt"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	agentv1 "github.com/dockwell/dockwell/gen/dockwell/agent/v1"
	dockwellv1 "github.com/dockwell/dockwell/gen/dockwell/v1"
)

// flood is a journal that gives a follow lines as fast as it takes them, until
// its context ends, as a server whose journal floods does.
type flood struct {
	lines int
	asked chan string
}

func (f *flood) FollowJournal(ctx context.Context, unit, after string, each func(JournalEntry)) error {
	if f.asked != nil {
		f.asked <- unit + "|" + after
	}
	level := []dockwellv1.LogLevel{dockwellv1.LogLevel_LOG_LEVEL_INFO, dockwellv1.LogLevel_LOG_LEVEL_ERROR}
	for i := 0; f.lines == 0 || i < f.lines; i++ {
		select {
		case <-ctx.Done():
			return nil
		default:
		}
		msg := fmt.Sprintf("line %d %s", i, strings.Repeat("x", 200))
		if i%10 == 0 {
			msg = fmt.Sprintf("line %d needle", i)
		}
		l := &dockwellv1.SystemLogLine{Cursor: strconv.Itoa(i), Message: msg, Level: level[i%2], Unit: "flood.service"}
		each(JournalEntry{Line: l, Size: len(msg) + 32})
	}
	<-ctx.Done()
	return nil
}

// collect gathers a stream's messages until it ends.
type collect struct {
	mu      sync.Mutex
	lines   int
	dropped uint64
	gap     bool
	texts   []string
	bytes   int
}

func (c *collect) send(m *agentv1.SendLogsRequest) error {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.lines += len(m.GetSystemLines())
	for _, l := range m.GetSystemLines() {
		c.bytes += len(l.GetMessage()) + 32
	}
	c.dropped += m.GetSystemDropped()
	c.gap = c.gap || m.GetSystemGap()
	for _, l := range m.GetSystemLines() {
		c.texts = append(c.texts, l.GetMessage())
	}
	return nil
}

// A follow of the system log takes a place like a service's and is held to its
// share of the server's live rate: past it, lines are dropped on the agent and
// counted, so a flooding journal can't take the server's network (ADR 0067).
func TestAFollowOfTheJournalIsHeldToItsShare(t *testing.T) {
	r := &Reader{Journal: &flood{}, LiveRate: 64 << 10, TotalRate: 64 << 10}
	// A reader that leaves cancels, as the control plane's does.
	ctx, cancel := context.WithCancel(t.Context())
	defer time.AfterFunc(time.Second, cancel).Stop()
	c := &collect{}
	began := time.Now()
	if err := r.Stream(ctx, &agentv1.LogsCall{Follow: true, System: &agentv1.SystemLogFollow{}}, c.send); err != nil {
		t.Fatal(err)
	}
	took := time.Since(began)
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.dropped == 0 || c.lines == 0 {
		t.Fatalf("a second of a flood sent %d lines and dropped %d", c.lines, c.dropped)
	}
	// At 64 KiB/s, with a quarter second's burst, and room for a slow machine.
	if most := int((took.Seconds() + 0.25) * float64(64<<10) * 1.2); c.bytes > most {
		t.Errorf("%s at 64 KiB/s sent %d bytes, more than %d", took, c.bytes, most)
	}
}

// A follow narrows what it's given by level and search, and asks the journal for
// its unit and its cursor.
func TestAFollowOfTheJournalNarrows(t *testing.T) {
	j := &flood{lines: 100, asked: make(chan string, 1)}
	r := &Reader{Journal: j}
	ctx, cancel := context.WithCancel(t.Context())
	defer time.AfterFunc(500*time.Millisecond, cancel).Stop()
	c := &collect{}
	call := &agentv1.LogsCall{Follow: true, System: &agentv1.SystemLogFollow{
		Unit: "flood.service", After: "c9", Level: dockwellv1.LogLevel_LOG_LEVEL_ERROR, Query: "NEEDLE",
	}}
	if err := r.Stream(ctx, call, c.send); err != nil {
		t.Fatal(err)
	}
	if got := <-j.asked; got != "flood.service|c9" {
		t.Errorf("the journal was asked for %q", got)
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	// Every tenth line holds the needle, and those are all info lines: none is an error.
	if c.lines != 0 {
		t.Errorf("errors holding needle: %q", c.texts)
	}
	if err := checkCall(&agentv1.LogsCall{System: &agentv1.SystemLogFollow{Query: strings.Repeat("q", maxQuery+1)}}); err == nil {
		t.Error("a search past its bound was taken")
	}
}
