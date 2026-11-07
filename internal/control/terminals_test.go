// SPDX-License-Identifier: AGPL-3.0-only

package control

import (
	"bytes"
	"context"
	"fmt"
	"net/http"
	"slices"
	"strings"
	"testing"
	"time"

	"connectrpc.com/connect"

	agentv1 "github.com/dockwell/dockwell/gen/dockwell/agent/v1"
	dockwellv1 "github.com/dockwell/dockwell/gen/dockwell/v1"
	"github.com/dockwell/dockwell/gen/dockwell/v1/dockwellv1connect"
	"github.com/dockwell/dockwell/internal/agent/terminal"
	"github.com/dockwell/dockwell/internal/control/authapi"
	"github.com/dockwell/dockwell/internal/control/terminals"
	"github.com/dockwell/dockwell/internal/store"
	"github.com/dockwell/dockwell/internal/store/storetest"
)

func terminalClient(c connect.HTTPClient, origin string) dockwellv1connect.TerminalServiceClient {
	return dockwellv1connect.NewTerminalServiceClient(c, origin, connect.WithProtoJSON())
}

// openTerminal opens a terminal on server, confirming it with b's passkey.
func openTerminal(t *testing.T, b *browser, server string) *dockwellv1.Terminal {
	t.Helper()
	c := terminalClient(b, b.origin)
	proof := stepUpFor(t, b, dockwellv1.StepUpAction_STEP_UP_ACTION_OPEN_TERMINAL, server)
	res, err := c.OpenTerminal(t.Context(), connect.NewRequest(&dockwellv1.OpenTerminalRequest{ServerId: server, Columns: 80, Rows: 24, Proof: proof}))
	if err != nil {
		t.Fatalf("OpenTerminal: %v", err)
	}
	return res.Msg.GetTerminal()
}

// attachment is one AttachTerminal stream, and what it read.
type attachment struct {
	t      *testing.T
	c      dockwellv1connect.TerminalServiceClient
	id     string
	cancel context.CancelFunc
	msgs   chan *dockwellv1.AttachTerminalResponse
	errs   chan error
	out    bytes.Buffer
	first  *dockwellv1.AttachTerminalResponse
	end    *dockwellv1.AttachTerminalResponse
	typed  uint64
}

func attachTo(t *testing.T, b *browser, terminalID string, offset uint64) *attachment {
	t.Helper()
	c := terminalClient(b, b.origin)
	ctx, cancel := context.WithCancel(t.Context())
	t.Cleanup(cancel)
	a := &attachment{t: t, c: c, id: terminalID, cancel: cancel, msgs: make(chan *dockwellv1.AttachTerminalResponse, 256), errs: make(chan error, 1)}
	stream, err := c.AttachTerminal(ctx, connect.NewRequest(&dockwellv1.AttachTerminalRequest{TerminalId: terminalID, Offset: offset, Columns: 80, Rows: 24}))
	if err != nil {
		t.Fatalf("AttachTerminal: %v", err)
	}
	go func() {
		for stream.Receive() {
			a.msgs <- stream.Msg()
		}
		a.errs <- stream.Err()
		close(a.msgs)
	}()
	a.first = a.next()
	for a.first.GetHeartbeat() {
		a.first = a.next()
	}
	if a.first == nil || !a.first.GetAttached() {
		t.Fatalf("the attachment began with %v: %v", a.first, a.err())
	}
	a.typed = a.first.GetInputOffset()
	return a
}

func (a *attachment) err() error {
	select {
	case err := <-a.errs:
		return err
	case <-time.After(5 * time.Second):
		return nil
	}
}

func (a *attachment) next() *dockwellv1.AttachTerminalResponse {
	a.t.Helper()
	select {
	case m, ok := <-a.msgs:
		if !ok {
			return nil
		}
		if len(m.GetData()) > 0 {
			a.out.Write(m.GetData())
		}
		if m.GetEnd() != nil || m.GetMoved() {
			a.end = m
		}
		return m
	case <-time.After(10 * time.Second):
		a.t.Fatalf("the attachment sent nothing more; it read %q", a.out.String())
		return nil
	}
}

func (a *attachment) waitFor(text string) {
	a.t.Helper()
	for !strings.Contains(a.out.String(), text) {
		if a.next() == nil {
			a.t.Fatalf("the attachment ended before %q: %q, %v", text, a.out.String(), a.err())
		}
	}
}

// ended reads on until the attachment ends, and returns its last message.
func (a *attachment) ended() *dockwellv1.AttachTerminalResponse {
	a.t.Helper()
	for a.end == nil {
		if a.next() == nil {
			a.t.Fatalf("the attachment ended with no end: %v", a.err())
		}
	}
	return a.end
}

func (a *attachment) write(text string) uint64 {
	a.t.Helper()
	res, err := a.c.WriteTerminal(a.t.Context(), connect.NewRequest(&dockwellv1.WriteTerminalRequest{TerminalId: a.id, Offset: a.typed, Data: []byte(text)}))
	if err != nil {
		a.t.Fatalf("WriteTerminal %q: %v", text, err)
	}
	a.typed = res.Msg.GetOffset()
	return a.typed
}

// terminalEvents are the audit log's events about terminals, oldest first.
func terminalEvents(t *testing.T, f *fullServer) []store.Event {
	t.Helper()
	var events []store.Event
	err := f.store.View(t.Context(), func(tx *store.Tx) error {
		var err error
		events, err = tx.ListEvents(t.Context(), 1000)
		return err
	})
	if err != nil {
		t.Fatal(err)
	}
	events = slices.DeleteFunc(events, func(e store.Event) bool { return !strings.HasPrefix(e.Action, "terminal.") })
	slices.Reverse(events)
	return events
}

func waitForTerminalEvent(t *testing.T, b *browser, server, action string) *dockwellv1.Event {
	t.Helper()
	deadline := time.Now().Add(10 * time.Second)
	for {
		list, err := dockwellv1connect.NewEventServiceClient(b, b.origin, connect.WithProtoJSON()).ListEvents(t.Context(),
			connect.NewRequest(&dockwellv1.ListEventsRequest{ServerId: server}))
		if err != nil {
			t.Fatal(err)
		}
		for _, e := range list.Msg.GetEvents() {
			if e.GetAction() == action {
				return e
			}
		}
		if time.Now().After(deadline) {
			t.Fatalf("the server's timeline never had %s: %v", action, list.Msg.GetEvents())
		}
		time.Sleep(10 * time.Millisecond)
	}
}

// A person opens a root shell on a server with a passkey, types into it, sees what
// it prints, and closes it. Its opening and closing are on Activity, and nothing
// typed or printed is on record anywhere.
func TestAPersonOpensATerminal(t *testing.T) { storetest.Each(t, testAPersonOpensATerminal) }

func testAPersonOpensATerminal(t *testing.T, backend store.Backend) {
	f := startFull(t, backend, nil)
	b := signedIn(t, f)
	id := f.addServer(t, false).id.ServerID
	c := terminalClient(b, b.origin)

	_, err := c.OpenTerminal(t.Context(), connect.NewRequest(&dockwellv1.OpenTerminalRequest{ServerId: id, Columns: 80, Rows: 24}))
	if connect.CodeOf(err) != connect.CodePermissionDenied || reasonOf(err) != authapi.ReasonNeedsStepUp || !strings.Contains(err.Error(), "root shell") {
		t.Fatalf("opening with no passkey = %v", err)
	}
	if got := terminalEvents(t, f); len(got) != 0 {
		t.Errorf("being asked for a passkey went on record as %v", got)
	}
	_, err = c.OpenTerminal(t.Context(), connect.NewRequest(&dockwellv1.OpenTerminalRequest{ServerId: id, Columns: 900, Rows: 24}))
	if connect.CodeOf(err) != connect.CodeInvalidArgument {
		t.Errorf("a terminal 900 wide = %v", err)
	}

	term := openTerminal(t, b, id)
	if !strings.HasPrefix(term.GetId(), "trm_") || term.GetServerId() != id || !strings.Contains(term.GetTitle(), "@") || term.GetColumns() != 80 {
		t.Fatalf("opened %v", term)
	}
	list, err := c.ListTerminals(t.Context(), connect.NewRequest(&dockwellv1.ListTerminalsRequest{ServerId: id}))
	if err != nil || len(list.Msg.GetTerminals()) != 1 || list.Msg.GetTerminals()[0].GetId() != term.GetId() {
		t.Fatalf("listed %v, %v", list, err)
	}

	a := attachTo(t, b, term.GetId(), 0)
	a.write("echo s3cret-$((6*7))\n")
	a.waitFor("s3cret-42")
	// A resize alone says only where what's typed stands.
	res, err := c.WriteTerminal(t.Context(), connect.NewRequest(&dockwellv1.WriteTerminalRequest{TerminalId: term.GetId(), Offset: a.typed, Columns: 100, Rows: 30}))
	if err != nil || res.Msg.GetOffset() != a.typed {
		t.Fatalf("resizing = %v, %v", res, err)
	}
	a.write("stty size\n")
	a.waitFor("30 100")

	closed, err := c.CloseTerminal(t.Context(), connect.NewRequest(&dockwellv1.CloseTerminalRequest{TerminalId: term.GetId()}))
	if err != nil || closed.Msg.GetEnd().GetReason() != dockwellv1.TerminalEndReason_TERMINAL_END_REASON_CLOSED {
		t.Fatalf("closing = %v, %v", closed, err)
	}
	if end := a.ended(); end.GetEnd().GetReason() != dockwellv1.TerminalEndReason_TERMINAL_END_REASON_CLOSED {
		t.Errorf("the attachment ended with %v", end)
	}

	closing := waitForTerminalEvent(t, b, id, "terminal.close")
	if closing.GetKind() != dockwellv1.EventKind_EVENT_KIND_SERVER || closing.GetSubject().GetId() != id || closing.GetActor().GetName() != "Mira Chen" ||
		closing.GetTerminal().GetId() != term.GetId() || closing.GetTerminal().GetEnd().GetReason() != dockwellv1.TerminalEndReason_TERMINAL_END_REASON_CLOSED ||
		closing.GetTerminal().GetOpenTime() == nil || closing.GetTerminal().GetEnd().GetTime() == nil {
		t.Errorf("its closing reads %v", closing)
	}
	opening := waitForTerminalEvent(t, b, id, "terminal.open")
	if opening.GetTerminal().GetTitle() != term.GetTitle() || opening.GetSurface() != dockwellv1.Surface_SURFACE_PANEL {
		t.Errorf("its opening reads %v", opening)
	}
	var actions []string
	events := terminalEvents(t, f)
	for i := range events {
		e := &events[i]
		actions = append(actions, e.Action)
		if strings.Contains(e.Detail, "s3cret") || strings.Contains(e.Detail, "stty") {
			t.Errorf("the audit log holds what was typed or printed: %s", e.Detail)
		}
		if e.Target != id || e.Subject != term.GetId() || !strings.HasPrefix(e.Actor, "user:") {
			t.Errorf("went on record as %+v", e)
		}
	}
	if !slices.Equal(actions, []string{"terminal.opening", "terminal.open", "terminal.close"}) {
		t.Errorf("on record: %v", actions)
	}
	list, err = c.ListTerminals(t.Context(), connect.NewRequest(&dockwellv1.ListTerminalsRequest{ServerId: id}))
	if err != nil || len(list.Msg.GetTerminals()) != 0 {
		t.Errorf("once closed, listed %v, %v", list, err)
	}
	if _, err := c.WriteTerminal(t.Context(), connect.NewRequest(&dockwellv1.WriteTerminalRequest{TerminalId: term.GetId(), Data: []byte("x")})); connect.CodeOf(err) != connect.CodeNotFound {
		t.Errorf("writing to a closed terminal = %v", err)
	}
}

// No token reaches a terminal, whatever its scope, and refusing it asks nothing of
// the server.
func TestNoTokenReachesATerminal(t *testing.T) { storetest.Each(t, testNoTokenReachesATerminal) }

func testNoTokenReachesATerminal(t *testing.T, backend store.Backend) {
	f := startFull(t, backend, nil)
	b := signedIn(t, f)
	id := f.addServer(t, false).id.ServerID
	term := openTerminal(t, b, id)
	for _, scope := range []dockwellv1.TokenScope{dockwellv1.TokenScope_TOKEN_SCOPE_READ_ONLY, dockwellv1.TokenScope_TOKEN_SCOPE_READ_WRITE} {
		token := createToken(t, b, scope)
		script := terminalClient(bearer{token: token, rt: b.client.Transport}, b.origin)
		needsSession := func(what string, err error) {
			t.Helper()
			if connect.CodeOf(err) != connect.CodePermissionDenied || reasonOf(err) != authapi.ReasonNeedsSession {
				t.Errorf("%v %s = %v, want needs_session", scope, what, err)
			}
		}
		_, err := script.OpenTerminal(t.Context(), connect.NewRequest(&dockwellv1.OpenTerminalRequest{ServerId: id, Columns: 80, Rows: 24}))
		needsSession("opening a terminal", err)
		_, err = script.ListTerminals(t.Context(), connect.NewRequest(&dockwellv1.ListTerminalsRequest{ServerId: id}))
		needsSession("listing terminals", err)
		_, err = script.WriteTerminal(t.Context(), connect.NewRequest(&dockwellv1.WriteTerminalRequest{TerminalId: term.GetId(), Data: []byte("id\n")}))
		needsSession("typing", err)
		_, err = script.CloseTerminal(t.Context(), connect.NewRequest(&dockwellv1.CloseTerminalRequest{TerminalId: term.GetId()}))
		needsSession("closing", err)
		stream, err := script.AttachTerminal(t.Context(), connect.NewRequest(&dockwellv1.AttachTerminalRequest{TerminalId: term.GetId()}))
		if err == nil {
			for stream.Receive() {
				t.Errorf("%v attached and read %v", scope, stream.Msg())
			}
			err = stream.Err()
		}
		needsSession("attaching", err)
		_, err = dockwellv1connect.NewAuthServiceClient(bearer{token: token, rt: b.client.Transport}, b.origin, connect.WithProtoJSON()).BeginStepUp(t.Context(),
			connect.NewRequest(&dockwellv1.BeginStepUpRequest{Action: dockwellv1.StepUpAction_STEP_UP_ACTION_OPEN_TERMINAL, Target: id}))
		if err == nil {
			t.Errorf("%v began a step-up for a terminal", scope)
		}
		// REST is the same handler, and refuses the same way.
		code, _, body := callREST(t, b, http.MethodPost, "/v1/servers/"+id+"/terminals", map[string]string{"Authorization": "Bearer " + token}, `{"columns":80,"rows":24}`)
		if code != http.StatusForbidden || !strings.Contains(body, "terminal") {
			t.Errorf("%v POST terminals: %d %s", scope, code, body)
		}
	}
}

// A terminal is its session's alone: another session of the same person can't
// reach it, and signing out ends it.
func TestATerminalIsItsSessionsAlone(t *testing.T) {
	storetest.Each(t, testATerminalIsItsSessionsAlone)
}

func testATerminalIsItsSessionsAlone(t *testing.T, backend store.Backend) {
	f := startFull(t, backend, nil)
	b := signedIn(t, f)
	id := f.addServer(t, false).id.ServerID
	term := openTerminal(t, b, id)
	a := attachTo(t, b, term.GetId(), 0)

	other := newBrowser(t, b.origin, f.api, nil)
	other.auth = b.auth
	other.signIn(t)
	oc := terminalClient(other, other.origin)
	list, err := oc.ListTerminals(t.Context(), connect.NewRequest(&dockwellv1.ListTerminalsRequest{ServerId: id}))
	if err != nil || len(list.Msg.GetTerminals()) != 0 {
		t.Errorf("another session listed %v, %v", list, err)
	}
	if _, err := oc.WriteTerminal(t.Context(), connect.NewRequest(&dockwellv1.WriteTerminalRequest{TerminalId: term.GetId(), Data: []byte("id\n")})); connect.CodeOf(err) != connect.CodeNotFound {
		t.Errorf("another session typing = %v", err)
	}
	if _, err := oc.CloseTerminal(t.Context(), connect.NewRequest(&dockwellv1.CloseTerminalRequest{TerminalId: term.GetId()})); connect.CodeOf(err) != connect.CodeNotFound {
		t.Errorf("another session closing = %v", err)
	}
	stream, err := oc.AttachTerminal(t.Context(), connect.NewRequest(&dockwellv1.AttachTerminalRequest{TerminalId: term.GetId()}))
	if err == nil {
		for stream.Receive() {
			t.Errorf("another session attached and read %v", stream.Msg())
		}
		err = stream.Err()
	}
	if connect.CodeOf(err) != connect.CodeNotFound {
		t.Errorf("another session attaching = %v", err)
	}
	// A proof is for one session, and works once.
	proof := stepUpFor(t, b, dockwellv1.StepUpAction_STEP_UP_ACTION_OPEN_TERMINAL, id)
	if _, err := oc.OpenTerminal(t.Context(), connect.NewRequest(&dockwellv1.OpenTerminalRequest{ServerId: id, Columns: 80, Rows: 24, Proof: proof})); reasonOf(err) != authapi.ReasonNeedsStepUp {
		t.Errorf("another session opening with b's proof = %v", err)
	}

	a.write("echo still-$((1+1))\n")
	a.waitFor("still-2")
	if _, err := b.authClient().SignOut(t.Context(), connect.NewRequest(&dockwellv1.SignOutRequest{})); err != nil {
		t.Fatal(err)
	}
	closing := waitForTerminalEvent(t, other, id, "terminal.close")
	if closing.GetTerminal().GetEnd().GetReason() != dockwellv1.TerminalEndReason_TERMINAL_END_REASON_SIGNED_OUT {
		t.Errorf("signing out closed it as %v", closing.GetTerminal())
	}
}

// A proof opens one terminal on the server it's for.
func TestAProofOpensOneTerminal(t *testing.T) { storetest.Each(t, testAProofOpensOneTerminal) }

func testAProofOpensOneTerminal(t *testing.T, backend store.Backend) {
	f := startFull(t, backend, nil)
	b := signedIn(t, f)
	id := f.addServer(t, false).id.ServerID
	other := f.addNamedServer(t, "hz-2", false).id.ServerID
	c := terminalClient(b, b.origin)
	open := func(server, proof string) error {
		_, err := c.OpenTerminal(t.Context(), connect.NewRequest(&dockwellv1.OpenTerminalRequest{ServerId: server, Columns: 80, Rows: 24, Proof: proof}))
		return err
	}
	if err := open(id, stepUpFor(t, b, dockwellv1.StepUpAction_STEP_UP_ACTION_OPEN_TERMINAL, other)); reasonOf(err) != authapi.ReasonNeedsStepUp {
		t.Errorf("opening on hz-1 with hz-2's proof = %v", err)
	}
	if err := open(id, stepUpFor(t, b, dockwellv1.StepUpAction_STEP_UP_ACTION_SAVE_WAY_BACK_IN, id+":/root/.ssh/authorized_keys")); reasonOf(err) != authapi.ReasonNeedsStepUp {
		t.Errorf("opening with a proof for a save = %v", err)
	}
	proof := stepUpFor(t, b, dockwellv1.StepUpAction_STEP_UP_ACTION_OPEN_TERMINAL, id)
	if err := open(id, proof); err != nil {
		t.Fatalf("opening with its proof = %v", err)
	}
	if err := open(id, proof); reasonOf(err) != authapi.ReasonNeedsStepUp {
		t.Errorf("opening again with the same proof = %v", err)
	}
}

// A passkey asked for that never comes is on Activity, as a stolen session's try.
func TestAnUnconfirmedTerminalIsOnActivity(t *testing.T) {
	storetest.Each(t, testAnUnconfirmedTerminalIsOnActivity)
}

func testAnUnconfirmedTerminalIsOnActivity(t *testing.T, backend store.Backend) {
	f := startFull(t, backend, nil)
	b := signedIn(t, f)
	id := f.addServer(t, false).id.ServerID
	c := terminalClient(b, b.origin)
	if _, err := c.OpenTerminal(t.Context(), connect.NewRequest(&dockwellv1.OpenTerminalRequest{ServerId: id, Columns: 80, Rows: 24})); reasonOf(err) != authapi.ReasonNeedsStepUp {
		t.Fatal(err)
	}
	e := waitForTerminalEvent(t, b, id, "terminal.open_unconfirmed")
	if e.GetSubject().GetId() != id || e.GetTerminal().GetError() == "" {
		t.Errorf("reads %v", e)
	}
}

// An attachment's heartbeat stops before the attachment does, so none goes on a
// stream that's closing, and a client that hears nothing else hears them.
func TestAnAttachmentBeats(t *testing.T) { storetest.Each(t, testAnAttachmentBeats) }

func testAnAttachmentBeats(t *testing.T, backend store.Backend) {
	was := terminals.HeartbeatEvery
	terminals.HeartbeatEvery = time.Millisecond
	t.Cleanup(func() { terminals.HeartbeatEvery = was })
	f := startFull(t, backend, nil)
	b := signedIn(t, f)
	id := f.addServer(t, false).id.ServerID
	for range 20 {
		term := openTerminal(t, b, id)
		a := attachTo(t, b, term.GetId(), 0)
		beats := 0
		for beats < 3 {
			if a.next().GetHeartbeat() {
				beats++
			}
		}
		a.write("exit\n")
		if end := a.ended(); end.GetEnd().GetReason() != dockwellv1.TerminalEndReason_TERMINAL_END_REASON_EXITED {
			t.Fatalf("ended with %v", end)
		}
	}
}

// A terminal outlives its attachment: the next one carries on from the byte the
// last had, and one from elsewhere moves it there.
func TestATerminalOutlivesItsAttachment(t *testing.T) {
	storetest.Each(t, testATerminalOutlivesItsAttachment)
}

func testATerminalOutlivesItsAttachment(t *testing.T, backend store.Backend) {
	f := startFull(t, backend, nil)
	b := signedIn(t, f)
	id := f.addServer(t, false).id.ServerID
	term := openTerminal(t, b, id)
	a := attachTo(t, b, term.GetId(), 0)
	a.write("echo one-$((1))\n")
	a.waitFor("one-1\r\n")
	had := a.first.GetOffset() + uint64(a.out.Len())
	a.cancel()
	// It goes on meanwhile.
	time.Sleep(50 * time.Millisecond)

	again := attachTo(t, b, term.GetId(), had)
	if again.first.GetOffset() != had || again.first.GetInputOffset() != a.typed {
		t.Errorf("attached again at %d with %d typed, want %d and %d", again.first.GetOffset(), again.first.GetInputOffset(), had, a.typed)
	}
	again.write("echo two-$((2))\n")
	again.waitFor("two-2\r\n")
	if strings.Contains(again.out.String(), "one-1") {
		t.Errorf("sent again what it had: %q", again.out.String())
	}
	// Writing what was typed already types nothing.
	again.typed = 0
	if n := again.write("echo one-$((1))\n"); n != a.typed+uint64(len("echo two-$((2))\n")) {
		t.Errorf("a write sent again took the terminal to %d", n)
	}

	third := attachTo(t, b, term.GetId(), 0)
	third.waitFor("two-2\r\n")
	if !strings.Contains(third.out.String(), "one-1") {
		t.Errorf("from 0 it read %q", third.out.String())
	}
	if end := again.ended(); !end.GetMoved() {
		t.Errorf("the attachment it moved from ended with %v", end)
	}
	if _, err := again.c.WriteTerminal(t.Context(), connect.NewRequest(&dockwellv1.WriteTerminalRequest{TerminalId: term.GetId(), Offset: third.typed, Data: []byte(":\n")})); err != nil {
		t.Errorf("typing through the new attachment = %v", err)
	}
}

// What's typed the moment the client hears it's attached has somewhere to go,
// however the attachment's first message and its stream race in the control plane.
func TestTypingAsSoonAsAttached(t *testing.T) { storetest.Each(t, testTypingAsSoonAsAttached) }

func testTypingAsSoonAsAttached(t *testing.T, backend store.Backend) {
	f := startFull(t, backend, nil)
	b := signedIn(t, f)
	id := f.addServer(t, false).id.ServerID
	term := openTerminal(t, b, id)
	typed := uint64(0)
	for i := range 30 {
		a := attachTo(t, b, term.GetId(), 0)
		a.typed = typed
		typed = a.write(":\n")
		if typed != uint64(2*(i+1)) {
			t.Fatalf("after %d writes the terminal took %d bytes", i+1, typed)
		}
		a.cancel()
	}
}

// When a server's agent restarts its shells go with it, and the terminals go on
// record as ended that way. One the control plane doesn't know, as it restarted, is
// closed.
func TestATerminalEndsWithItsAgent(t *testing.T) { storetest.Each(t, testATerminalEndsWithItsAgent) }

func testATerminalEndsWithItsAgent(t *testing.T, backend store.Backend) {
	f := startFull(t, backend, nil)
	b := signedIn(t, f)
	s := f.addServer(t, false)
	id := s.id.ServerID
	openTerminal(t, b, id)
	s.stop()
	waitConnected(t, f, id, false)
	s.terms = terminal.New(terminal.Options{Shell: []string{"/bin/sh"}, Home: t.TempDir()})
	if _, err := s.terms.Open(&agentv1.OpenTerminalCall{Id: "trm_stranger", Columns: 80, Rows: 24}); err != nil {
		t.Fatal(err)
	}
	s.connect(t, f)
	closing := waitForTerminalEvent(t, b, id, "terminal.close")
	if closing.GetTerminal().GetEnd().GetReason() != dockwellv1.TerminalEndReason_TERMINAL_END_REASON_AGENT_RESTARTED {
		t.Errorf("closed as %v", closing.GetTerminal())
	}
	deadline := time.Now().Add(10 * time.Second)
	for slices.Contains(s.terms.Known(), "trm_stranger") {
		if time.Now().After(deadline) {
			t.Fatalf("the agent still has a terminal the control plane doesn't know: %v", s.terms.Known())
		}
		time.Sleep(10 * time.Millisecond)
	}
}

// A server that's removed from the fleet takes its terminals off the control plane,
// on record as ended that way.
func TestATerminalEndsWithItsServer(t *testing.T) { storetest.Each(t, testATerminalEndsWithItsServer) }

func testATerminalEndsWithItsServer(t *testing.T, backend store.Backend) {
	was := terminals.RemovedEvery
	terminals.RemovedEvery = 20 * time.Millisecond
	t.Cleanup(func() { terminals.RemovedEvery = was })
	f := startFull(t, backend, nil)
	b := signedIn(t, f)
	keep := f.addServer(t, false).id.ServerID
	gone := f.addNamedServer(t, "hz-2", false)
	openTerminal(t, b, keep)
	openTerminal(t, b, gone.id.ServerID)
	// Its agent comes back with a certificate that's revoked, which is refused, and
	// that takes it off the fleet.
	if err := f.ident.Revoke(t.Context(), gone.id.ServerID, "cli:test"); err != nil {
		t.Fatal(err)
	}
	gone.stop()
	gone.run(t)
	closing := waitForTerminalEvent(t, b, gone.id.ServerID, "terminal.close")
	if closing.GetTerminal().GetEnd().GetReason() != dockwellv1.TerminalEndReason_TERMINAL_END_REASON_SERVER_REMOVED {
		t.Errorf("closed as %v", closing.GetTerminal())
	}
	c := terminalClient(b, b.origin)
	list, err := c.ListTerminals(t.Context(), connect.NewRequest(&dockwellv1.ListTerminalsRequest{ServerId: keep}))
	if err != nil || len(list.Msg.GetTerminals()) != 1 {
		t.Errorf("the server that stayed has %v, %v", list, err)
	}
}

// A server keeps 8 terminals, and the ninth says so.
func TestAServerKeepsEightTerminals(t *testing.T) { storetest.Each(t, testAServerKeepsEightTerminals) }

// A server keeps eight terminals, and one session half of them, so a session, a
// stolen one too, can't take every place there is.
func testAServerKeepsEightTerminals(t *testing.T, backend store.Backend) {
	f := startFull(t, backend, nil)
	b := signedIn(t, f)
	id := f.addServer(t, false).id.ServerID
	for range terminals.MaxPerSession {
		openTerminal(t, b, id)
	}
	c := terminalClient(b, b.origin)
	_, err := c.OpenTerminal(t.Context(), connect.NewRequest(&dockwellv1.OpenTerminalRequest{ServerId: id, Columns: 80, Rows: 24}))
	if connect.CodeOf(err) != connect.CodeResourceExhausted || !strings.Contains(err.Error(), "from this session, which is as many as one session keeps there") {
		t.Errorf("a fifth from one session = %v", err)
	}
	other := newBrowser(t, b.origin, f.api, nil)
	other.auth = b.auth
	other.signIn(t)
	for range terminal.MaxOpen - terminals.MaxPerSession {
		openTerminal(t, other, id)
	}
	third := newBrowser(t, b.origin, f.api, nil)
	third.auth = b.auth
	third.signIn(t)
	_, err = terminalClient(third, third.origin).OpenTerminal(t.Context(), connect.NewRequest(&dockwellv1.OpenTerminalRequest{ServerId: id, Columns: 80, Rows: 24}))
	if connect.CodeOf(err) != connect.CodeResourceExhausted || !strings.Contains(err.Error(), "hz-1 has 8 terminals open, which is as many as it keeps") {
		t.Errorf("a ninth = %v", err)
	}
}

// One session keeps half the control plane's terminals, across its servers.
func TestASessionKeepsSixteenTerminals(t *testing.T) {
	f := startFull(t, store.SQLite, nil)
	b := signedIn(t, f)
	var servers []string
	for i := range terminals.MaxPerSessionInAll/terminals.MaxPerSession + 1 {
		servers = append(servers, f.addNamedServer(t, fmt.Sprintf("hz-%d", i+1), false).id.ServerID)
	}
	for _, id := range servers[:len(servers)-1] {
		for range terminals.MaxPerSession {
			openTerminal(t, b, id)
		}
	}
	_, err := terminalClient(b, b.origin).OpenTerminal(t.Context(),
		connect.NewRequest(&dockwellv1.OpenTerminalRequest{ServerId: servers[len(servers)-1], Columns: 80, Rows: 24}))
	if connect.CodeOf(err) != connect.CodeResourceExhausted || !strings.Contains(err.Error(), "you have 16 terminals open from this session") {
		t.Errorf("a seventeenth from one session = %v", err)
	}
}

// A terminal nobody is attached to ends with its session, and isn't use of it: its
// checks keep no session alive, and leave the address it was last used from as it
// was. Here the session was last used from elsewhere, which a check that counted
// as use would put back to the address the terminal was opened from.
func TestADetachedTerminalIsntUseOfItsSession(t *testing.T) {
	testRecheck = 20 * time.Millisecond
	t.Cleanup(func() { testRecheck = 0 })
	f := startFull(t, store.SQLite, nil)
	b := signedIn(t, f)
	id := f.addServer(t, false).id.ServerID
	openTerminal(t, b, id)
	list, err := b.authClient().ListSessions(t.Context(), connect.NewRequest(&dockwellv1.ListSessionsRequest{}))
	if err != nil || len(list.Msg.GetSessions()) != 1 {
		t.Fatalf("sessions = %v, %v", list, err)
	}
	session := list.Msg.GetSessions()[0].GetId()
	away := time.Now().Add(-2 * time.Minute).Truncate(time.Second)
	if err := f.store.Update(t.Context(), func(tx *store.Tx) error {
		return tx.TouchSession(t.Context(), session, away, "203.0.113.5")
	}); err != nil {
		t.Fatal(err)
	}
	time.Sleep(300 * time.Millisecond) // many rechecks
	var got store.Session
	if err := f.store.View(t.Context(), func(tx *store.Tx) (err error) {
		got, err = tx.GetSession(t.Context(), session)
		return err
	}); err != nil {
		t.Fatal(err)
	}
	if got.LastIP != "203.0.113.5" || !got.LastSeenAt.Equal(away) {
		t.Errorf("a detached terminal counted as use of its session: last seen %v from %s", got.LastSeenAt, got.LastIP)
	}
}
