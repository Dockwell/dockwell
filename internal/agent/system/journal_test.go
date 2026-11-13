// SPDX-License-Identifier: AGPL-3.0-only

package system_test

import (
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	dockwellv1 "github.com/dockwell/dockwell/gen/dockwell/v1"
	"github.com/dockwell/dockwell/internal/agent/hosttool"
	"github.com/dockwell/dockwell/internal/agent/system"
	"github.com/dockwell/dockwell/internal/agent/system/systemtest"
)

// A page of the journal is read newest first with only the fields a line shows,
// narrowed to a unit and a level, and the next page starts after the cursor's own
// line; a message journald keeps as bytes is text where it's UTF-8.
func TestTheJournalIsReadAPageAtATime(t *testing.T) {
	s := systemtest.New(t, systemtest.Unit{
		Props: systemtest.Running("nginx.service"),
		Journal: []string{
			`{"__CURSOR":"c3","__REALTIME_TIMESTAMP":"1762963203000000","_SYSTEMD_UNIT":"nginx.service","_PID":"40","PRIORITY":"3","MESSAGE":[27,91,51,49,109,98,97,100,27,91,48,109]}`,
			`{"__CURSOR":"c2","__REALTIME_TIMESTAMP":"1762963202000000","_SYSTEMD_UNIT":"nginx.service","_PID":"40","PRIORITY":"4","MESSAGE":[255,254,0]}`,
			`{"__CURSOR":"c1","__REALTIME_TIMESTAMP":"1762963201000000","_SYSTEMD_UNIT":"init.scope","SYSLOG_IDENTIFIER":"systemd","_PID":"1","PRIORITY":"6","MESSAGE":"Started nginx.service."}`,
		},
	})
	r := reader(s)
	page, err := r.Journal(t.Context(), "nginx.service", dockwellv1.LogLevel_LOG_LEVEL_WARN, "", 2)
	if err != nil {
		t.Fatal(err)
	}
	if len(page.Lines) != 2 || !page.More || page.Source != dockwellv1.SystemLogSource_SYSTEM_LOG_SOURCE_JOURNAL {
		t.Fatalf("the page reads %d lines, more %t, from %v", len(page.Lines), page.More, page.Source)
	}
	first, second := page.Lines[0], page.Lines[1]
	if first.GetMessage() != "\x1b[31mbad\x1b[0m" || first.GetLevel() != dockwellv1.LogLevel_LOG_LEVEL_ERROR || first.GetPid() != 40 || first.GetUnit() != "nginx.service" {
		t.Errorf("the newest line reads %v", first)
	}
	if second.GetMessage() != "a binary message of 3 bytes" || second.GetLevel() != dockwellv1.LogLevel_LOG_LEVEL_WARN {
		t.Errorf("the binary line reads %v", second)
	}
	calls := s.Calls()
	want := "journalctl -o json --system --no-pager --reverse --output-fields=MESSAGE,PRIORITY,_SYSTEMD_UNIT,SYSLOG_IDENTIFIER,_PID --unit=nginx.service --priority=warning --lines=3"
	if !slices.Contains(calls, want) {
		t.Errorf("journalctl ran %q, want %q", calls, want)
	}
	next, err := r.Journal(t.Context(), "nginx.service", dockwellv1.LogLevel_LOG_LEVEL_UNSPECIFIED, "c2", 2)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(s.Calls()[len(s.Calls())-1], "--cursor=c2 --lines=4") {
		t.Errorf("the page before ran %q", s.Calls()[len(s.Calls())-1])
	}
	// The stand prints the whole journal: the cursor's own line is left out.
	if slices.ContainsFunc(next.Lines, func(l *dockwellv1.SystemLogLine) bool { return l.GetCursor() == "c2" }) {
		t.Error("the page before holds the cursor's own line")
	}
	if got := system.Why(page.Lines); len(got) != 2 {
		t.Errorf("why reads %q", got)
	}
	if got := system.Why(next.Lines); !slices.Equal(got, []string{"\x1b[31mbad\x1b[0m"}) {
		t.Errorf("why leaves systemd's own line in: %q", got)
	}
	if _, err := r.Journal(t.Context(), "nginx.service; rm", dockwellv1.LogLevel_LOG_LEVEL_UNSPECIFIED, "", 2); err == nil {
		t.Error("a name that isn't a unit's was read")
	}
}

// Without a journal, the end of /var/log/messages is read, newest first, narrowed
// to the service's tag, with busybox's levels.
func TestWithoutAJournalTheMessagesFileIsRead(t *testing.T) {
	root := t.TempDir()
	log := strings.Join([]string{
		"Nov 12 08:00:01 web-1 daemon.info nginx[812]: started",
		"Nov 12 08:00:02 web-1 authpriv.notice sudo: alice : COMMAND=/bin/ls",
		"Nov 12 08:00:03 web-1 daemon.err nginx[812]: bind() to 0.0.0.0:80 failed",
		"2026-11-12T08:00:04.123456+00:00 web-1 nginx[812]: shutting down",
		"",
	}, "\n")
	if err := os.MkdirAll(filepath.Join(root, "var", "log"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(root, "var", "log", "messages"), []byte(log), 0o644); err != nil {
		t.Fatal(err)
	}
	r := &system.Reader{Tools: &hosttool.Runner{Root: root}, Root: root}
	page, err := r.Journal(t.Context(), "nginx", dockwellv1.LogLevel_LOG_LEVEL_UNSPECIFIED, "", 2)
	if err != nil {
		t.Fatal(err)
	}
	if page.Source != dockwellv1.SystemLogSource_SYSTEM_LOG_SOURCE_FILE || len(page.Lines) != 2 || !page.More {
		t.Fatalf("the page reads %d lines, more %t, from %v", len(page.Lines), page.More, page.Source)
	}
	if page.Lines[0].GetMessage() != "shutting down" || page.Lines[1].GetLevel() != dockwellv1.LogLevel_LOG_LEVEL_ERROR || page.Lines[1].GetPid() != 812 {
		t.Errorf("the page reads %v", page.Lines)
	}
	rest, err := r.Journal(t.Context(), "nginx", dockwellv1.LogLevel_LOG_LEVEL_UNSPECIFIED, page.Lines[1].GetCursor(), 10)
	if err != nil {
		t.Fatal(err)
	}
	if len(rest.Lines) != 1 || rest.Lines[0].GetMessage() != "started" || rest.More {
		t.Errorf("the page before reads %v, more %t", rest.Lines, rest.More)
	}
	errs, err := r.Journal(t.Context(), "", dockwellv1.LogLevel_LOG_LEVEL_ERROR, "", 10)
	if err != nil {
		t.Fatal(err)
	}
	if len(errs.Lines) != 1 {
		t.Errorf("errors only reads %v", errs.Lines)
	}
}
