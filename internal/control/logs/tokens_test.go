// SPDX-License-Identifier: AGPL-3.0-only

package logs

import (
	"encoding/base64"
	"encoding/json"
	"fmt"
	"strings"
	"testing"
	"time"

	"connectrpc.com/connect"

	agentv1 "github.com/dockwell/dockwell/gen/dockwell/agent/v1"
	dockwellv1 "github.com/dockwell/dockwell/gen/dockwell/v1"
	agentlogs "github.com/dockwell/dockwell/internal/agent/logs"
)

func tokenOf(t *testing.T, v any) string {
	t.Helper()
	b, err := json.Marshal(v)
	if err != nil {
		t.Fatal(err)
	}
	return base64.RawURLEncoding.EncodeToString(b)
}

func placesOf(n int) map[string][2]int64 {
	out := make(map[string][2]int64, n)
	for i := range n {
		out[fmt.Sprintf("%012x", i)] = [2]int64{1700000000000000000, 1}
	}
	return out
}

var onOne = []server{{id: "srv_a", name: "a"}}

// A token comes back from whoever holds it, so what it says is checked, and a refusal
// says what is wrong and what to do.
func TestATokenIsCheckedBeforeItIsUsed(t *testing.T) {
	resume := func(p *serverPlace) string {
		return tokenOf(t, resumeToken{Servers: map[string]*serverPlace{"srv_a": p}})
	}
	page := func(places map[string][2]int64) string { return tokenOf(t, pageToken{"srv_a": places}) }
	for name, c := range map[string]struct{ resume, page, why string }{
		"too long": {
			resume: strings.Repeat("A", maxTokenBytes+1), page: strings.Repeat("A", maxTokenBytes+1),
			why: fmt.Sprintf("and one is at most %d", maxTokenBytes),
		},
		"too many sources on a server": {
			resume: resume(&serverPlace{Last: 1, Sources: placesOf(maxPlaces + 1)}), page: page(placesOf(maxPlaces + 1)),
			why: fmt.Sprintf("names %d sources on a server, and one names at most %d", maxPlaces+1, maxPlaces),
		},
		"a source that isn't one": {
			resume: resume(&serverPlace{Last: 1, Sources: map[string][2]int64{"../../etc": {1, 1}}}),
			page:   page(map[string][2]int64{"../../etc": {1, 1}}),
			why:    "names a source that isn't one",
		},
		"a time before there were any": {
			resume: resume(&serverPlace{Last: 1, Sources: map[string][2]int64{"aaaaaaaaaaaa": {-1, 1}}}),
			page:   page(map[string][2]int64{"aaaaaaaaaaaa": {-1, 1}}),
			why:    "can't be one",
		},
		"a count no cursor holds": {
			resume: resume(&serverPlace{Last: 1, Sources: map[string][2]int64{"aaaaaaaaaaaa": {1, 1 << 32}}}),
			page:   page(map[string][2]int64{"aaaaaaaaaaaa": {1, 1 << 32}}),
			why:    "can't be one",
		},
		"not one at all": {resume: "not a token!", page: "not a token!", why: "isn't one"},
	} {
		t.Run(name, func(t *testing.T) {
			_, err := readResumeToken(c.resume, onOne)
			checkRefused(t, err, "resume_token", c.why)
			_, err = readPageToken(c.page, onOne)
			checkRefused(t, err, "page_token", c.why)
		})
	}
	for name, p := range map[string]*serverPlace{
		"no place":            nil,
		"a last line in 1969": {Last: -1},
		"a beginning in 1969": {Last: 1, From: -1},
	} {
		t.Run(name, func(t *testing.T) {
			_, err := readResumeToken(resume(p), onOne)
			checkRefused(t, err, "resume_token", "")
		})
	}
}

func checkRefused(t *testing.T, err error, field, why string) {
	t.Helper()
	if connect.CodeOf(err) != connect.CodeInvalidArgument {
		t.Fatalf("%s: %v", field, err)
	}
	for _, want := range []string{"this " + field, why, "so the logs can't carry on from it", "Leave it out"} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("%s said %q, without %q", field, err.Error(), want)
		}
	}
}

// As much as a token may name is taken, and what a stream writes reads back, so a
// reader never gets a token it can't use.
func TestATokenAtItsLimitsIsTakenAndTokensReadBack(t *testing.T) {
	full := &serverPlace{Last: 5, From: 2, Sources: placesOf(maxPlaces)}
	got, err := readResumeToken((&resumeToken{Servers: map[string]*serverPlace{"srv_a": full}}).encode(), onOne)
	if err != nil {
		t.Fatal(err)
	}
	if p := got.Servers["srv_a"]; p == nil || p.Last != 5 || p.From != 2 || len(p.Sources) != maxPlaces {
		t.Errorf("it read back %+v", p)
	}
	pages, err := readPageToken(pageToken{"srv_a": placesOf(maxPlaces)}.encode(), onOne)
	if err != nil {
		t.Fatal(err)
	}
	if len(pages["srv_a"]) != maxPlaces {
		t.Errorf("a page's token read back %d sources", len(pages["srv_a"]))
	}
	// However many sources come, what is written stays within what is read.
	p := &serverPlace{}
	for i := range maxPlaces * 3 {
		p.note(fmt.Sprintf("%012x", i), time.Unix(0, int64(i+1)))
	}
	if _, err := readResumeToken((&resumeToken{Servers: map[string]*serverPlace{"srv_a": p}}).encode(), onOne); err != nil {
		t.Errorf("a token a stream wrote was refused: %v", err)
	}
	if maxPlaces != agentlogs.MaxNamedSources {
		t.Errorf("a token holds %d sources, and an agent takes %d", maxPlaces, agentlogs.MaxNamedSources)
	}
}

// What a token says of a server that isn't the service's goes unread, so it costs
// nothing to carry and isn't written back.
func TestATokenSaysNothingOfOtherServers(t *testing.T) {
	token := tokenOf(t, resumeToken{Servers: map[string]*serverPlace{
		"srv_a":     {Last: 1, Sources: placesOf(1)},
		"srv_other": {Last: 1, Sources: placesOf(maxPlaces + 1)},
	}})
	got, err := readResumeToken(token, onOne)
	if err != nil {
		t.Fatal(err)
	}
	if len(got.Servers) != 1 || got.Servers["srv_a"] == nil {
		t.Errorf("it kept %v", got.Servers)
	}
	pages, err := readPageToken(tokenOf(t, pageToken{"srv_a": placesOf(1), "srv_other": placesOf(maxPlaces + 1)}), onOne)
	if err != nil {
		t.Fatal(err)
	}
	if len(pages) != 1 || pages["srv_a"] == nil {
		t.Errorf("a page's token kept %v", pages)
	}
}

// A server is told only of its own sources, so what a call names stays within what an
// agent takes, however many servers a service is on.
func TestANextPageTellsEachServerOfItsOwn(t *testing.T) {
	token := pageToken{}
	for i := range 40 {
		server := fmt.Sprintf("srv_%d", i)
		for s := range 2 {
			token.set(server, fmt.Sprintf("%02x%02x", i, s), [2]int64{1, 1})
		}
	}
	if n := len(token.cursors("srv_7")); n != 2 {
		t.Errorf("a server is told of %d cursors", n)
	}
	if n := len(token.sourceIDs("srv_7")); n != 2 {
		t.Errorf("a server is told of %d sources", n)
	}
	if n := len(token.cursors("srv_gone")); n != 0 {
		t.Errorf("a server with none is told of %d", n)
	}
	if n := len(token.flat()); n != 80 {
		t.Errorf("the page knows %d places", n)
	}
}

// A source with no place yet is read from when the stream began, not from another
// source's last line, as it may have printed before that.
func TestASourceWithNoPlaceIsReadFromWhenTheStreamBegan(t *testing.T) {
	p := &serverPlace{From: 100}
	p.note("aaaaaaaaaaaa", time.Unix(0, 500))
	call := &agentv1.LogsCall{Tail: 50}
	p.apply(call)
	if got := call.GetResumeFrom().AsTime().UnixNano(); got != 100 {
		t.Errorf("it reads from %d, want when the stream began", got)
	}
	if call.GetTail() != 0 || len(call.GetAfter()) != 1 {
		t.Errorf("the call is %v", call)
	}
	// A token that doesn't say goes by the last line.
	old := &serverPlace{}
	old.note("aaaaaaaaaaaa", time.Unix(0, 500))
	call = &agentv1.LogsCall{}
	old.apply(call)
	if got := call.GetResumeFrom().AsTime().UnixNano(); got != 500 {
		t.Errorf("with nothing said it reads from %d", got)
	}
	// A source the token forgets is read again from where it was forgotten, not from
	// the start, which would repeat it.
	many := &serverPlace{From: 1}
	for i := range maxPlaces + 1 {
		many.note(fmt.Sprintf("%012x", i), time.Unix(0, int64(100+i)))
	}
	if many.From != 100 {
		t.Errorf("having forgotten the oldest, at 100, it reads from %d", many.From)
	}
}

// What a request names is held to what a read can take, before anything is asked.
func TestARequestNamesNoMoreThanAReadTakes(t *testing.T) {
	ids := make([]string, agentlogs.MaxNamedSources+1)
	for i := range ids {
		ids[i] = fmt.Sprintf("%012x", i)
	}
	for name, err := range map[string]error{
		"sources":   checkLists(ids, nil, ""),
		"a bad one": checkLists([]string{"../etc"}, nil, ""),
		"levels":    checkLists(nil, make([]dockwellv1.LogLevel, maxLevels+1), ""),
		"a change":  checkLists(nil, nil, strings.Repeat("c", maxChangeID+1)),
	} {
		if connect.CodeOf(err) != connect.CodeInvalidArgument {
			t.Errorf("%s: %v", name, err)
		}
	}
	if err := checkLists(ids[:agentlogs.MaxNamedSources], make([]dockwellv1.LogLevel, maxLevels), strings.Repeat("c", maxChangeID)); err != nil {
		t.Errorf("at its limits: %v", err)
	}
}
