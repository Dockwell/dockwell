// SPDX-License-Identifier: AGPL-3.0-only

package build

import (
	"bytes"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/dockwell/dockwell/internal/resource"
	"github.com/dockwell/dockwell/internal/resource/binding"
)

// Steps that run at once and print lines with no end yet hold no more than
// maxPartials between them.
func TestUnfinishedLinesAreBoundedAcrossSteps(t *testing.T) {
	r := newReport(t.Context(), nil, time.Now)
	r.mu.Lock()
	defer r.mu.Unlock()
	for i := range 40 {
		var s solveStatus
		s.Logs = append(s.Logs, struct {
			Vertex    string
			Stream    int
			Data      []byte
			Timestamp time.Time
		}{Vertex: fmt.Sprintf("sha256:%d", i), Stream: 1, Data: bytes.Repeat([]byte("x"), 60<<10)})
		r.status(&s)
	}
	total := 0
	for _, x := range r.vertices {
		total += len(x.partial)
	}
	if total > maxPartials {
		t.Errorf("40 steps hold %d bytes of unfinished lines, over %d", total, maxPartials)
	}
	if r.number == 0 {
		t.Error("the lines it ended aren't in the log")
	}
}

// statusOf is one of BuildKit's statuses, as buildctl's rawjson prints it: a step
// that started, and what it printed.
func statusOf(t *testing.T, name string, logs ...[]byte) string {
	t.Helper()
	const digest = "sha256:aa"
	started := time.Date(2026, 11, 8, 12, 0, 1, 0, time.UTC)
	at := time.Date(2026, 11, 8, 12, 34, 56, 123000000, time.UTC)
	s := map[string]any{}
	if name != "" {
		s["Vertexes"] = []map[string]any{{"Digest": digest, "Name": name, "Started": started}}
	}
	var ls []map[string]any
	for _, d := range logs {
		ls = append(ls, map[string]any{"Vertex": digest, "Stream": 1, "Data": d, "Timestamp": at})
	}
	if len(ls) > 0 {
		s["Logs"] = ls
	}
	b, err := json.Marshal(s)
	if err != nil {
		t.Fatal(err)
	}
	return string(b) + "\n"
}

// fed is what a report keeps of what a builder wrote, as writes: its lines, and
// what it would say of a failure.
func fed(t *testing.T, secrets map[string]string, writes ...string) ([]string, facts, int64) {
	t.Helper()
	var lines []string
	var dropped int64
	ctx := resource.WithBuilds(t.Context(), func(u resource.BuildUpdate) {
		for _, l := range u.Lines {
			lines = append(lines, l.Text)
		}
		dropped += u.Dropped
	})
	r := newReport(ctx, secrets, func() time.Time { return time.Date(2026, 11, 8, 12, 0, 0, 0, time.UTC) })
	stop := r.follow()
	_, _ = r.Write([]byte(marker + "phase build\n"))
	for _, w := range writes {
		_, _ = r.Write([]byte(w))
	}
	stop()
	return lines, r.facts(), dropped
}

// holds says where a secret is in what a report keeps, as it is or in base64.
func holds(lines []string, f facts, secret string) []string {
	all := append(append(slices.Clone(lines), f.failedLine, f.stepError, f.stepName), f.said...)
	all = append(all, f.tail...)
	var out []string
	encoded := base64.StdEncoding.EncodeToString([]byte(secret))
	for _, l := range all {
		if strings.Contains(l, secret) || strings.Contains(l, encoded[:len(encoded)-4]) || strings.Contains(l, `"Data"`) {
			out = append(out, fmt.Sprintf("%.100q", l))
		}
	}
	return out
}

// A status longer than a line usually is, as a step that prints fast makes, is
// read whole, and its lines masked: nothing of it is kept raw, in base64.
func TestALongStatusIsReadWholeAndMasked(t *testing.T) {
	step := statusOf(t, "[2/2] RUN cat /run/secrets/X")
	var logs [][]byte
	for i := range 40 {
		logs = append(logs, fmt.Appendf(nil, "%04d %s npm_s3cr3t-value\n", i, strings.Repeat("x", 2000)))
	}
	big := statusOf(t, "", logs...)
	if len(big) <= maxPending {
		t.Fatalf("the status is %d bytes, not more than a line usually holds", len(big))
	}
	writes := []string{step}
	for i := 0; i < len(big); i += 32 << 10 {
		writes = append(writes, big[i:min(i+(32<<10), len(big))])
	}
	writes = append(writes, `{"Vertexes":[{"Digest":"sha256:aa","Name":"[2/2] RUN cat /run/secrets/X","Error":"exit code: 1"}]}`+"\n")
	lines, f, _ := fed(t, map[string]string{"X": "npm_s3cr3t-value"}, writes...)
	if leaked := holds(lines, f, "npm_s3cr3t-value"); len(leaked) > 0 {
		t.Errorf("what's kept holds the secret, or a status raw: %v", leaked)
	}
	masked := 0
	for _, l := range lines {
		if strings.HasSuffix(l, " "+binding.Masked) {
			masked++
		}
	}
	if masked != 40 {
		t.Errorf("%d of the step's 40 lines are in the log, masked", masked)
	}
}

// A status too long to keep, past maxStatus, is dropped and counted, with the rest
// of it, never kept raw.
func TestAStatusTooLongToKeepIsDroppedAndCounted(t *testing.T) {
	big := statusOf(t, "", bytes.Repeat([]byte("npm_s3cr3t-value "), (maxStatus/17)+1024))
	var writes []string
	for i := 0; i < len(big); i += 32 << 10 {
		writes = append(writes, big[i:min(i+(32<<10), len(big))])
	}
	writes = append(writes, "after\n")
	lines, f, dropped := fed(t, map[string]string{"X": "npm_s3cr3t-value"}, writes...)
	if leaked := holds(lines, f, "npm_s3cr3t-value"); len(leaked) > 0 {
		t.Errorf("what's kept holds the secret, or a status raw: %v", leaked)
	}
	if dropped != 1 || !slices.Contains(lines, "after") {
		t.Errorf("dropped %d, and kept %q", dropped, lines)
	}
}

// A short secret that's also in a status's JSON, as 34 is in 12:34:56, neither stops
// it parsing nor lets a longer one through in base64.
func TestAShortSecretInAStatusDoesntUnmaskAnother(t *testing.T) {
	step := statusOf(t, "[2/2] RUN cat /run/secrets/X")
	lines, f, _ := fed(t, map[string]string{"PIN": "34", "X": "npm_s3cr3t-value"}, step, statusOf(t, "", []byte("npm_s3cr3t-value\n")))
	if leaked := holds(lines, f, "npm_s3cr3t-value"); len(leaked) > 0 {
		t.Errorf("what's kept holds the secret, or a status raw: %v", leaked)
	}
	if !slices.Contains(lines, binding.Masked) {
		t.Errorf("the step's line isn't in the log, masked: %q", lines)
	}
}

// A line that starts as JSON and isn't a status is dropped and counted, not kept.
func TestAStatusThatDoesntParseIsDropped(t *testing.T) {
	lines, _, dropped := fed(t, nil, `{"Logs":[{"Data":"bnBtX3MzY3IzdC12YWx1ZQ==",`+"\n")
	if len(lines) != 0 || dropped != 1 {
		t.Errorf("kept %q, dropped %d", lines, dropped)
	}
}
