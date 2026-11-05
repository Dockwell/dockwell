// SPDX-License-Identifier: AGPL-3.0-only

package control

import (
	"bufio"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"net"
	"net/http"
	"runtime"
	"slices"
	"strconv"
	"strings"
	"testing"
	"time"

	"connectrpc.com/connect"

	dockwellv1 "github.com/dockwell/dockwell/gen/dockwell/v1"
	"github.com/dockwell/dockwell/gen/dockwell/v1/dockwellv1connect"
	logapi "github.com/dockwell/dockwell/internal/control/logs"
	"github.com/dockwell/dockwell/internal/docker"
	"github.com/dockwell/dockwell/internal/docker/dockertest"
	"github.com/dockwell/dockwell/internal/store"
)

func logToken(t *testing.T, b *browser, grants ...dockwellv1.TokenGrant) (string, string) {
	t.Helper()
	res, err := dockwellv1connect.NewTokenServiceClient(b, b.origin, connect.WithProtoJSON()).CreateToken(t.Context(), connect.NewRequest(&dockwellv1.CreateTokenRequest{
		Name: "logs " + strconv.FormatInt(time.Now().UnixNano(), 10), Scope: dockwellv1.TokenScope_TOKEN_SCOPE_READ_ONLY, Grants: grants,
		Proof: stepUpFor(t, b, dockwellv1.StepUpAction_STEP_UP_ACTION_CREATE_TOKEN, ""),
	}))
	if err != nil {
		t.Fatal(err)
	}
	return res.Msg.GetSecret(), res.Msg.GetToken().GetId()
}

// A revoked token stops reading at once, whether it reads or is stuck behind flow
// control, with its handler blocked in a send.
func TestARevokedTokenStopsReading(t *testing.T) {
	f := startFull(t, store.SQLite, nil)
	b := signedIn(t, f)
	e := f.addDockerServer(t)
	c := changeClient(b, b.origin)
	done := apply(t, c, propose(t, c, "Run web", runImage("ghcr.io/acme/shop:1.4")))
	name := docker.ContainerName(docker.Project(done.GetPlan().GetResources()[0].GetResource().GetScope()), "web")
	secret, id := logToken(t, b, dockwellv1.TokenGrant_TOKEN_GRANT_LOGS)
	lc := logClient(bearer{token: secret, rt: h2cTransport(t, f.api)}, "http://"+f.api)

	reading := follow(t, lc, &dockwellv1.WatchLogsRequest{Id: "web", Tail: -1})
	reading.waitCaughtUp()

	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()
	stuck, err := lc.WatchLogs(ctx, connect.NewRequest(&dockwellv1.WatchLogsRequest{Id: "web", Tail: -1}))
	if err != nil {
		t.Fatal(err)
	}
	defer stuck.Close()
	if !stuck.Receive() {
		t.Fatalf("no first message: %v", stuck.Err())
	}
	flood := e.Flood(name, 512)
	defer flood.Stop()
	eventually(t, "the flood", func() bool { return flood.Lines() > 50_000 })

	if _, err := dockwellv1connect.NewTokenServiceClient(b, b.origin).RevokeToken(t.Context(), connect.NewRequest(&dockwellv1.RevokeTokenRequest{Id: id})); err != nil {
		t.Fatal(err)
	}
	select {
	case err := <-reading.done:
		if reasonOf(err) != "token_rejected" {
			t.Errorf("the reader's stream ended with %v, want token_rejected", err)
		}
	case <-time.After(5 * time.Second):
		t.Error("a reader's stream stayed open after its token was revoked")
	}
	eventually(t, "Docker's follows to end, the stuck reader's too", func() bool { return e.Followers() == 0 })
}

// deleteWeb deletes a service through a change, and returns the plan's step.
func deleteWeb(t *testing.T, c dockwellv1connect.ChangeServiceClient, scope string) *dockwellv1.Change {
	t.Helper()
	gone := propose(t, c, "Delete web", &dockwellv1.Edit{Resource: &dockwellv1.ResourceRef{Kind: "service", Scope: scope, Name: "web"}, Delete: true})
	w := watch(t, c, gone.GetId())
	if _, err := c.ApplyChange(t.Context(), connect.NewRequest(&dockwellv1.ApplyChangeRequest{Id: gone.GetId(), Fingerprint: gone.GetPlan().GetFingerprint()})); err != nil {
		t.Fatalf("deleting: %v", err)
	}
	if st := waitWatch(t, w).last().GetState(); st != dockwellv1.ChangeState_CHANGE_STATE_APPLIED {
		t.Fatalf("deleting ended %v", st)
	}
	return gone
}

func logTexts(t *testing.T, lc dockwellv1connect.LogServiceClient, id string) []string {
	t.Helper()
	read, err := lc.ReadLogs(t.Context(), connect.NewRequest(&dockwellv1.ReadLogsRequest{Id: id}))
	if err != nil {
		t.Fatal(err)
	}
	var lines []string
	for _, ch := range read.Msg.GetChunks() {
		for _, l := range ch.GetLines() {
			lines = append(lines, l.GetText())
		}
	}
	return lines
}

// Deleting a service takes its kept logs out of reach, by any name, and its plan
// says so. A new service of the name starts clean. Undoing the deletion brings them
// back.
func TestADeletedServicesLogsAreOutOfReachUnlessUndone(t *testing.T) {
	f := startFull(t, store.SQLite, nil)
	b := signedIn(t, f)
	e := f.addDockerServer(t)
	c := changeClient(b, b.origin)
	lc := logClient(b, b.origin)
	first := apply(t, c, propose(t, c, "Run web", runImage("ghcr.io/acme/shop:1.4")))
	scope := first.GetPlan().GetResources()[0].GetResource().GetScope()
	name := docker.ContainerName(docker.Project(scope), "web")
	e.Print(name, docker.Stdout, "password=hunter2 from the service you deleted")
	sources, err := lc.ListLogSources(t.Context(), connect.NewRequest(&dockwellv1.ListLogSourcesRequest{Id: "web"}))
	if err != nil {
		t.Fatal(err)
	}
	id := sources.Msg.GetService().GetId()

	gone := deleteWeb(t, c, scope)
	said := false
	for _, s := range gone.GetPlan().GetServers() {
		for _, step := range s.GetSteps() {
			said = said || step.GetHidesLogs()
		}
	}
	if !said {
		t.Errorf("the plan to delete it doesn't say what happens to its logs: %v", gone.GetPlan().GetServers())
	}
	if lines := logTexts(t, lc, id); len(lines) != 0 {
		t.Errorf("a deleted service read by its ID: %q", lines)
	}
	list, err := lc.ListLogSources(t.Context(), connect.NewRequest(&dockwellv1.ListLogSourcesRequest{Id: id}))
	if err != nil {
		t.Fatal(err)
	}
	if !list.Msg.GetService().GetDeleted() || len(list.Msg.GetSources()) != 0 {
		t.Errorf("a deleted service lists %v", list.Msg)
	}

	// Undoing it brings them back, as it does its data.
	undo, err := c.UndoChange(t.Context(), connect.NewRequest(&dockwellv1.UndoChangeRequest{Id: gone.GetId()}))
	if err != nil {
		t.Fatalf("undoing: %v", err)
	}
	if back := apply(t, c, undo.Msg.GetChange()); back.GetState() != dockwellv1.ChangeState_CHANGE_STATE_APPLIED {
		t.Fatalf("undoing ended %v: %s", back.GetState(), back.GetError())
	}
	lines := logTexts(t, lc, "web")
	if !slices.ContainsFunc(lines, func(s string) bool { return strings.Contains(s, "hunter2") }) {
		t.Errorf("after undoing the deletion its logs read %q", lines)
	}

	// Deleted again, and made anew, it starts clean.
	deleteWeb(t, c, scope)
	again := apply(t, c, propose(t, c, "Run a new web", runImage("ghcr.io/acme/shop:1.4")))
	if again.GetState() != dockwellv1.ChangeState_CHANGE_STATE_APPLIED {
		t.Fatalf("the new one ended %v", again.GetState())
	}
	if lines := logTexts(t, lc, "web"); slices.ContainsFunc(lines, func(s string) bool { return strings.Contains(s, "hunter2") }) {
		t.Errorf("the new service called web reads the deleted one's lines: %q", lines)
	}
}

// A first deploy that failed and was rolled back was never deleted: its logs are
// what its reader needs.
func TestAFailedFirstDeploysLogsStayReadable(t *testing.T) {
	f := startFull(t, store.SQLite, nil)
	b := signedIn(t, f)
	e := f.addDockerServer(t)
	e.Registry["ghcr.io/acme/shop:bad"] = dockertest.Image{Exits: true, Prints: []string{"ERROR can't reach the database"}, Config: docker.Config{Cmd: []string{"shop"}}}
	c := changeClient(b, b.origin)
	lc := logClient(b, b.origin)
	failed := propose(t, c, "Run web", runImage("ghcr.io/acme/shop:bad"))
	serviceID := failed.GetPlan().GetResources()[0].GetId()
	w := watch(t, c, failed.GetId())
	if _, err := c.ApplyChange(t.Context(), connect.NewRequest(&dockwellv1.ApplyChangeRequest{Id: failed.GetId(), Fingerprint: failed.GetPlan().GetFingerprint()})); err != nil {
		t.Fatal(err)
	}
	waitWatch(t, w)
	eventually(t, "the rollback to keep its log", func() bool {
		list, err := lc.ListLogSources(t.Context(), connect.NewRequest(&dockwellv1.ListLogSourcesRequest{Id: serviceID}))
		return err == nil && slices.ContainsFunc(list.Msg.GetSources(), func(s *dockwellv1.LogSource) bool {
			return s.GetState() == dockwellv1.LogSourceState_LOG_SOURCE_STATE_KEPT
		})
	})
}

// A plan and a check both say when a file's log rotation isn't what Dockwell takes.
func TestAComposeFilesRotationIsSaidNotDropped(t *testing.T) {
	f := startFull(t, store.SQLite, nil)
	b := signedIn(t, f)
	f.addDockerServer(t)
	cc := dockwellv1connect.NewComposeServiceClient(b, b.origin, connect.WithProtoJSON())
	file := "services: {web: {image: 'ghcr.io/acme/shop:1.4', logging: {options: {max-size: 1g}}}}\n"
	check, err := cc.CheckCompose(t.Context(), connect.NewRequest(&dockwellv1.CheckComposeRequest{Compose: file}))
	if err != nil {
		t.Fatal(err)
	}
	if len(check.Msg.GetNotes()) != 1 || !strings.Contains(check.Msg.GetNotes()[0], "max-size 1g") || !strings.Contains(check.Msg.GetNotes()[0], "5 files of 10 MiB") {
		t.Errorf("the check says %v", check.Msg.GetNotes())
	}
	imported, err := cc.ImportCompose(t.Context(), connect.NewRequest(&dockwellv1.ImportComposeRequest{Compose: file}))
	if err != nil {
		t.Fatal(err)
	}
	said := false
	for _, n := range imported.Msg.GetChange().GetPlan().GetNotes() {
		said = said || (n.GetReason() == "log_rotation" && strings.Contains(n.GetMessage(), "max-size 1g"))
	}
	if !said {
		t.Errorf("the plan says %v", imported.Msg.GetChange().GetPlan().GetNotes())
	}
}

// A query is a search, not a way to send megabytes down an agent's main stream.
func TestAQueryHasALimit(t *testing.T) {
	f := startFull(t, store.SQLite, nil)
	b := signedIn(t, f)
	f.addDockerServer(t)
	c := changeClient(b, b.origin)
	apply(t, c, propose(t, c, "Run web", runImage("ghcr.io/acme/shop:1.4")))
	lc := logClient(b, b.origin)
	long := strings.Repeat("a", 1025)
	if _, err := lc.ReadLogs(t.Context(), connect.NewRequest(&dockwellv1.ReadLogsRequest{Id: "web", Query: long})); connect.CodeOf(err) != connect.CodeInvalidArgument || !strings.Contains(err.Error(), "1024 bytes") {
		t.Errorf("reading with a query of 1,025 bytes = %v", err)
	}
	stream, err := lc.WatchLogs(t.Context(), connect.NewRequest(&dockwellv1.WatchLogsRequest{Id: "web", Query: long}))
	if err != nil {
		t.Fatal(err)
	}
	defer stream.Close()
	if stream.Receive() || connect.CodeOf(stream.Err()) != connect.CodeInvalidArgument {
		t.Errorf("following with a query of 1,025 bytes = %v", stream.Err())
	}
	if _, err := lc.ReadLogs(t.Context(), connect.NewRequest(&dockwellv1.ReadLogsRequest{Id: "web", Query: strings.Repeat("a", 1024)})); err != nil {
		t.Errorf("reading with a query of 1,024 bytes = %v", err)
	}
}

func heapNow() float64 {
	runtime.GC()
	var m runtime.MemStats
	runtime.ReadMemStats(&m)
	return float64(m.HeapAlloc) / (1 << 20)
}

// Pages asked for and never read don't pile up in the control plane.
func TestUnreadPagesDontPinTheControlPlane(t *testing.T) {
	f := startFull(t, store.SQLite, nil)
	b := signedIn(t, f)
	e := f.addDockerServer(t)
	c := changeClient(b, b.origin)
	done := apply(t, c, propose(t, c, "Run web", runImage("ghcr.io/acme/shop:1.4")))
	name := docker.ContainerName(docker.Project(done.GetPlan().GetResources()[0].GetResource().GetScope()), "web")
	text := strings.Repeat("y", 1400)
	batch := make([]string, 5000)
	for i := range batch {
		batch[i] = text
	}
	e.Print(name, docker.Stdout, batch...)
	secret, _ := logToken(t, b, dockwellv1.TokenGrant_TOKEN_GRANT_LOGS)
	base := heapNow()
	const unread = 32
	refused := 0
	var conns []net.Conn
	defer func() {
		for _, conn := range conns {
			_ = conn.Close()
		}
	}()
	for range unread {
		conn, err := net.Dial("tcp", f.api)
		if err != nil {
			t.Fatal(err)
		}
		_ = conn.(*net.TCPConn).SetReadBuffer(4096)
		conns = append(conns, conn)
		w := bufio.NewWriter(conn)
		fmt.Fprintf(w, "GET /v1/services/web/logs?pageSize=5000 HTTP/1.1\r\nHost: %s\r\nAuthorization: Bearer %s\r\n\r\n", f.api, secret)
		_ = w.Flush()
		time.Sleep(50 * time.Millisecond)
	}
	time.Sleep(time.Second)
	held := heapNow() - base
	for _, conn := range conns {
		_ = conn.SetReadDeadline(time.Now().Add(100 * time.Millisecond))
		head := make([]byte, 40)
		if n, _ := conn.Read(head); strings.Contains(string(head[:n]), "429") {
			refused++
		}
	}
	t.Logf("%d pages asked for and not read hold %.0f MiB; %d were refused", unread, held, refused)
	// One reader's 4 places, of pages of about 7.4 MiB.
	if held > 48 {
		t.Errorf("%d unread pages pin %.0f MiB", unread, held)
	}
	if refused == 0 {
		t.Error("no page was refused")
	}
}

// A full page reads through REST, with or without compression: 5,000 lines of JSON,
// whose quotes take two bytes each in the answer, once came to more than the REST
// path holds of a message, and curl got a 429 in words that help nobody.
func TestAFullPageReadsThroughREST(t *testing.T) {
	f := startFull(t, store.SQLite, nil)
	b := signedIn(t, f)
	e := f.addDockerServer(t)
	c := changeClient(b, b.origin)
	done := apply(t, c, propose(t, c, "Run web", runImage("ghcr.io/acme/shop:1.4")))
	name := docker.ContainerName(docker.Project(done.GetPlan().GetResources()[0].GetResource().GetScope()), "web")
	for i, text := range []string{strings.Repeat(`{"a":"b"},`, 160), strings.Repeat("\x1b[0m\x01", 300)} {
		batch := make([]string, 1000)
		for n := range batch {
			batch[n] = fmt.Sprintf("%d-%d %s", i, n, text)
		}
		for range 5 {
			e.Print(name, docker.Stdout, batch...)
		}
	}
	secret, _ := logToken(t, b, dockwellv1.TokenGrant_TOKEN_GRANT_LOGS)
	for _, encoding := range []string{"", "gzip"} {
		token, pages, lines := "", 0, 0
		for {
			conn, err := net.Dial("tcp", f.api)
			if err != nil {
				t.Fatal(err)
			}
			accept := ""
			if encoding != "" {
				accept = "Accept-Encoding: " + encoding + "\r\n"
			}
			path := "/v1/services/web/logs?pageSize=5000"
			if token != "" {
				path += "&pageToken=" + token
			}
			fmt.Fprintf(conn, "GET %s HTTP/1.1\r\nHost: %s\r\nAuthorization: Bearer %s\r\n%sConnection: close\r\n\r\n", path, f.api, secret, accept)
			res, err := http.ReadResponse(bufio.NewReader(conn), nil)
			if err != nil {
				t.Fatal(err)
			}
			body, err := io.ReadAll(res.Body)
			_ = res.Body.Close()
			_ = conn.Close()
			if err != nil {
				t.Fatal(err)
			}
			if res.StatusCode != http.StatusOK {
				t.Fatalf("Accept-Encoding %q, page %d = %s: %.200s", encoding, pages, res.Status, body)
			}
			if encoding != "" {
				break // that it came at all is the point; it says nothing readable here
			}
			if len(body) > 8<<20 {
				t.Errorf("a page came to %d bytes", len(body))
			}
			var page struct {
				NextPageToken string `json:"nextPageToken"`
				Chunks        []struct {
					Lines []json.RawMessage `json:"lines"`
				} `json:"chunks"`
			}
			if err := json.Unmarshal(body, &page); err != nil {
				t.Fatal(err)
			}
			pages++
			for _, ch := range page.Chunks {
				lines += len(ch.Lines)
			}
			if token = page.NextPageToken; token == "" {
				break
			}
		}
		if encoding == "" && lines != 10_000 {
			t.Errorf("%d pages held %d of 10,000 lines", pages, lines)
		}
	}
}

// Lines that share a time, as a burst can, all come across pages.
func TestPagesKeepLinesOfOneTime(t *testing.T) {
	f := startFull(t, store.SQLite, nil)
	b := signedIn(t, f)
	e := f.addDockerServer(t)
	c := changeClient(b, b.origin)
	done := apply(t, c, propose(t, c, "Run web", runImage("ghcr.io/acme/shop:1.4")))
	name := docker.ContainerName(docker.Project(done.GetPlan().GetResources()[0].GetResource().GetScope()), "web")
	at := time.Now().Add(-time.Hour).UTC().Truncate(time.Second)
	const lines = 25
	for i := range lines {
		e.PrintAt(name, at, docker.Stdout, fmt.Sprintf("line %02d", i))
	}
	lc := logClient(b, b.origin)
	var got []string
	token := ""
	for range lines {
		res, err := lc.ReadLogs(t.Context(), connect.NewRequest(&dockwellv1.ReadLogsRequest{Id: "web", PageSize: 10, PageToken: token}))
		if err != nil {
			t.Fatal(err)
		}
		var page []string
		for _, ch := range res.Msg.GetChunks() {
			for _, l := range ch.GetLines() {
				page = append(page, l.GetText())
			}
		}
		got = append(page, got...)
		if token = res.Msg.GetNextPageToken(); token == "" {
			break
		}
	}
	if len(got) != lines {
		t.Fatalf("pages held %d of %d lines: %q", len(got), lines, got)
	}
	for i, l := range got {
		if l != fmt.Sprintf("line %02d", i) {
			t.Errorf("line %d is %q, in %q", i, l, got)
			break
		}
	}
}

// A long follow stays on record for as long as it lasts, and says when it ended, and
// a reader who comes back after the limit is on record again. A client that reconnects
// all day doesn't flood the log.
func TestAFollowStaysOnRecord(t *testing.T) {
	const every = 200 * time.Millisecond
	f := startFullWith(t, store.SQLite, nil, slog.New(slog.DiscardHandler), nil, logapi.Options{AuditEvery: every})
	b := signedIn(t, f)
	f.addDockerServer(t)
	c := changeClient(b, b.origin)
	apply(t, c, propose(t, c, "Run web", runImage("ghcr.io/acme/shop:1.4")))
	lc := logClient(b, b.origin)

	reading := func() int { return countLogEvents(t, f, "") }
	r := follow(t, lc, &dockwellv1.WatchLogsRequest{Id: "web", Tail: -1})
	r.waitCaughtUp()
	before := reading()
	eventually(t, "it to go on record again while it stays open", func() bool { return reading() > before })
	r.cancel()
	eventually(t, "it to say it ended", func() bool { return countLogEvents(t, f, "stopped following web") == 1 })
}

func TestReconnectingDoesntFloodTheAuditLog(t *testing.T) {
	f := startFull(t, store.SQLite, nil)
	b := signedIn(t, f)
	f.addDockerServer(t)
	c := changeClient(b, b.origin)
	apply(t, c, propose(t, c, "Run web", runImage("ghcr.io/acme/shop:1.4")))
	lc := logClient(b, b.origin)
	for range 5 {
		r := follow(t, lc, &dockwellv1.WatchLogsRequest{Id: "web", Tail: -1})
		r.waitCaughtUp()
		r.cancel()
		if err := <-r.done; connect.CodeOf(err) != connect.CodeCanceled {
			t.Errorf("a follow ended with %v", err)
		}
	}
	eventually(t, "it to say one ended", func() bool { return countLogEvents(t, f, "stopped following web") >= 1 })
	if n := countLogEvents(t, f, ""); n != 2 {
		t.Errorf("five short follows made %d events in the audit log, want one for the start and one for an end", n)
	}
}

// countLogEvents counts logs.read in the audit log, of those whose words hold what.
func countLogEvents(t *testing.T, f *fullServer, what string) int {
	t.Helper()
	n := 0
	err := f.store.View(context.Background(), func(tx *store.Tx) error {
		events, err := tx.ListEvents(t.Context(), 200)
		if err != nil {
			return err
		}
		for i := range events {
			if events[i].Action == "logs.read" && strings.Contains(events[i].Detail, what) {
				n++
			}
		}
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	return n
}
