// SPDX-License-Identifier: AGPL-3.0-only

package system

import (
	"encoding/json"
	"fmt"
	"strconv"
	"strings"
	"testing"
	"time"
	"unicode/utf8"

	"google.golang.org/protobuf/proto"
	"google.golang.org/protobuf/types/known/timestamppb"

	dockwellv1 "github.com/dockwell/dockwell/gen/dockwell/v1"
)

// parseJournalAsAMap is how parseJournal read an entry before ADR 0109: into a
// map of every field, then each field again.
func parseJournalAsAMap(b []byte) (*dockwellv1.SystemLogLine, string, bool) {
	var e map[string]json.RawMessage
	if json.Unmarshal(b, &e) != nil {
		return nil, "", false
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
		var nums []int
		if json.Unmarshal(raw, &nums) == nil {
			bs := make([]byte, 0, len(nums))
			for _, n := range nums {
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
		return nil, "", false
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
		line.Unit = "systemd"
	}
	line.Level = priorityLevel(str("PRIORITY"))
	return line, str("UNIT"), true
}

var journalSamples = []string{
	`{"__CURSOR":"c1","__REALTIME_TIMESTAMP":"1762963201000000","_SYSTEMD_UNIT":"init.scope","SYSLOG_IDENTIFIER":"systemd","_PID":"1","PRIORITY":"6","MESSAGE":"Started nginx.service.","UNIT":"nginx.service"}`,
	`{"__CURSOR":"c2","__REALTIME_TIMESTAMP":"1762963202000000","_SYSTEMD_UNIT":"nginx.service","_PID":"40","PRIORITY":"4","MESSAGE":[255,254,0]}`,
	`{"__CURSOR":"c3","_SYSTEMD_UNIT":"nginx.service","_PID":"40","PRIORITY":"3","MESSAGE":[27,91,51,49,109,98,97,100,27,91,48,109]}`,
	`{"__CURSOR":"c4","MESSAGE":"tab\tand \"quotes\" and é and \\ and a line\n","PRIORITY":"7"}`,
	`{"__CURSOR":"c5","MESSAGE":null,"_PID":null,"PRIORITY":5,"UNIT":["a"]}`,
	`{"__CURSOR":"c6","MESSAGE":"é日本😀","_PID":"4294967296","__REALTIME_TIMESTAMP":"x"}`,
	`{"__CURSOR":"c7","MESSAGE":"first","MESSAGE":"second","_SYSTEMD_UNIT":"init.scope","_PID":"1"}`,
	`{"__CURSOR":"c8","MESSAGE":[300,65,66],"SYSLOG_IDENTIFIER":"cron","_SYSTEMD_UNIT":""}`,
	`{"__CURSOR":"","MESSAGE":"no cursor"}`,
	`{"MESSAGE":"no cursor at all"}`,
	`{"__CURSOR":"c9","MESSAGE":"` + strings.Repeat("long ", 4000) + `"}`,
	`{"__CURSOR":"c10","MESSAGE":{"nested":true},"PRIORITY":"2"}`,
	`not json`,
	`{"__CURSOR":"c11"`,
	`[]`,
	``,
}

// An entry reads as it did when each field was read from a map, whatever it
// holds, as text, bytes, escapes, a null or something else.
func TestAJournalEntryReadsAsItDid(t *testing.T) {
	for _, s := range journalSamples {
		want, wantUnit, wantOK := parseJournalAsAMap([]byte(s))
		got, gotUnit, gotOK := parseJournal([]byte(s))
		if gotOK != wantOK || gotUnit != wantUnit || !proto.Equal(got, want) {
			t.Errorf("%.60s: got %v %q %v, want %v %q %v", s, got, gotUnit, gotOK, want, wantUnit, wantOK)
		}
	}
}

func BenchmarkParseJournal(b *testing.B) {
	line := []byte(journalSamples[0])
	b.Run("struct", func(b *testing.B) {
		b.ReportAllocs()
		for b.Loop() {
			parseJournal(line)
		}
	})
	b.Run("map", func(b *testing.B) {
		b.ReportAllocs()
		for b.Loop() {
			parseJournalAsAMap(line)
		}
	})
}
