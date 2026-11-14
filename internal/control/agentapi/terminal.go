// SPDX-License-Identifier: AGPL-3.0-only

package agentapi

import (
	"context"
	"crypto/rand"
	"encoding/binary"
	"errors"
	"fmt"
	"io"
	"slices"
	"sync"
	"time"

	"connectrpc.com/connect"

	agentv1 "github.com/dockwell/dockwell/gen/dockwell/agent/v1"
	dockwellv1 "github.com/dockwell/dockwell/gen/dockwell/v1"
	"github.com/dockwell/dockwell/internal/fault"
	"github.com/dockwell/dockwell/internal/resource"
)

// TerminalTimeout is how long a server has to open or close a terminal.
const TerminalTimeout = 15 * time.Second

// ErrNotAttached is a write to a terminal with no attachment to carry it.
var ErrNotAttached = errors.New("the terminal isn't attached")

// Terminals reports an agent that opens terminals.
func (s *Session) Terminals() bool { return s.terminals }

// ContainerTerminals reports an agent that opens shells in containers.
func (s *Session) ContainerTerminals() bool { return s.containerTerminals }

// OpenTerminals is what the agent said in its hello of the terminals it has.
func (s *Session) OpenTerminals() []string { return slices.Clone(s.openTerminals) }

// Opened is when this stream said hello.
func (s *Session) Opened() time.Time { return s.opened }

// OnTerminalEnded calls fn with what an agent says of a terminal that ended. An
// agent speaks for its own server and nothing more, so what it names has to be
// checked.
func (a *Agents) OnTerminalEnded(fn func(server string, ended *agentv1.TerminalEnded)) {
	a.mu.Lock()
	defer a.mu.Unlock()
	a.terminalHearer = fn
}

func (a *Agents) reportTerminalEnded(server string, ended *agentv1.TerminalEnded) {
	a.mu.Lock()
	hear := a.terminalHearer
	a.mu.Unlock()
	if hear != nil {
		hear(server, ended)
	}
}

// OpenTerminal asks the agent to start a shell.
func (s *Session) OpenTerminal(ctx context.Context, call *agentv1.OpenTerminalCall) (*agentv1.OpenTerminalResult, error) {
	r, err := s.call(ctx, &agentv1.Call{Call: &agentv1.Call_OpenTerminal{OpenTerminal: call}}, TerminalTimeout, nil)
	if err != nil {
		return nil, err
	}
	if out := r.GetOpenTerminal(); out != nil {
		return out, nil
	}
	return nil, fmt.Errorf("%s's agent answered opening a terminal with nothing", s.name)
}

// CloseTerminal asks the agent to hang up a terminal's shell.
func (s *Session) CloseTerminal(ctx context.Context, id string, reason dockwellv1.TerminalEndReason) (*dockwellv1.TerminalEnd, error) {
	call := &agentv1.CloseTerminalCall{Id: id, Reason: reason}
	r, err := s.call(ctx, &agentv1.Call{Call: &agentv1.Call_CloseTerminal{CloseTerminal: call}}, TerminalTimeout, nil)
	if err != nil {
		return nil, err
	}
	return r.GetCloseTerminal().GetEnd(), nil
}

// TerminalLink is one attachment of a terminal, between the agent's Terminal
// stream and whoever reads it: what the terminal prints comes up it, and what's
// typed goes down.
type TerminalLink struct {
	server  string
	token   uint64
	claimed bool // by Agents.mu, once its stream has come
	up      chan *agentv1.TerminalRequest
	done    chan struct{}
	// ready is closed once the agent's stream has come, which down is then.
	ready chan struct{}

	sendMu sync.Mutex // one write at a time, each waiting for its answer
	down   func(*agentv1.TerminalResponse) error
	taken  chan uint64
	// gone is closed once the agent's stream has ended.
	gone     chan struct{}
	goneOnce sync.Once
}

func (a *Agents) openLink(server string) *TerminalLink {
	a.mu.Lock()
	defer a.mu.Unlock()
	for {
		var b [8]byte
		_, _ = rand.Read(b[:])
		token := binary.BigEndian.Uint64(b[:])
		if _, taken := a.links[token]; token == 0 || taken {
			continue
		}
		l := &TerminalLink{
			server: server, token: token, up: make(chan *agentv1.TerminalRequest), done: make(chan struct{}), ready: make(chan struct{}),
			taken: make(chan uint64, 1), gone: make(chan struct{}),
		}
		a.links[token] = l
		return l
	}
}

func (a *Agents) closeLink(l *TerminalLink) {
	a.mu.Lock()
	defer a.mu.Unlock()
	delete(a.links, l.token)
	close(l.done)
}

// linkFor finds an attachment of a server waiting for its stream, and takes it, so
// a token works once.
func (a *Agents) linkFor(server string, token uint64) *TerminalLink {
	a.mu.Lock()
	defer a.mu.Unlock()
	l := a.links[token]
	if l == nil || l.server != server || l.claimed {
		return nil
	}
	l.claimed = true
	return l
}

// Write sends what's typed from offset, with the terminal's size if cols and rows
// are set, and waits for how much the terminal has taken in all. A resize alone
// has no answer, so it returns offset.
func (l *TerminalLink) Write(ctx context.Context, offset uint64, data []byte, cols, rows uint32) (uint64, error) {
	select {
	case <-l.ready:
	case <-l.done:
		return 0, ErrNotAttached
	case <-ctx.Done():
		return 0, ctx.Err()
	}
	l.sendMu.Lock()
	defer l.sendMu.Unlock()
	select {
	case <-l.taken: // an answer for a write that gave up waiting
	default:
	}
	if err := l.down(&agentv1.TerminalResponse{Offset: offset, Data: data, Columns: cols, Rows: rows}); err != nil {
		return 0, ErrNotAttached
	}
	if len(data) == 0 {
		return offset, nil
	}
	select {
	case n := <-l.taken:
		return n, nil
	case <-l.gone:
		return 0, ErrNotAttached
	case <-l.done:
		return 0, ErrNotAttached
	case <-ctx.Done():
		return 0, ctx.Err()
	}
}

// AttachTerminal asks the agent to carry a terminal's bytes over a stream of its
// own, and passes each message of what it prints to each, until the attachment
// ends, each fails, or ctx ends, which stops it on the agent at once. attached
// hears the link once the agent's stream has come, for what's typed to go down.
// each may block: flow control holds up the terminal's shell meanwhile.
//
// It returns ErrLost when the agent's stream ends first, and ErrNotConnected when
// the call never reached the agent. When the agent stopped it to move to a new
// stream, or moved before it got there, that is ErrMoving too, and the next stream
// can carry it on.
func (s *Session) AttachTerminal(ctx context.Context, call *agentv1.AttachTerminalCall, attached func(*TerminalLink),
	each func(*agentv1.TerminalRequest) error,
) (*agentv1.AttachTerminalResult, error) {
	link := s.agents.openLink(s.server)
	defer s.agents.closeLink(link)
	call.Stream = link.token
	c := &agentv1.Call{Call: &agentv1.Call_AttachTerminal{AttachTerminal: call}}
	s.mu.Lock()
	if s.closed {
		s.mu.Unlock()
		if s.moving() {
			return nil, s.stoppedToMove()
		}
		return nil, s.notConnected()
	}
	s.next++
	c.Id = s.next
	result := make(chan *agentv1.CallResult, 1)
	s.pending[c.Id] = result
	s.mu.Unlock()
	cancel := func() {
		s.mu.Lock()
		delete(s.pending, c.GetId())
		s.mu.Unlock()
		_ = s.write(&agentv1.ConnectResponse{Message: &agentv1.ConnectResponse_Cancel{Cancel: &agentv1.CancelCall{Id: c.GetId()}}})
	}
	if err := s.writeCall(&agentv1.ConnectResponse{Message: &agentv1.ConnectResponse_Call{Call: c}}); err != nil {
		s.mu.Lock()
		delete(s.pending, c.GetId())
		s.mu.Unlock()
		if errors.Is(err, ErrMoving) {
			return nil, s.stoppedToMove()
		}
		return nil, s.notConnected()
	}
	ready := link.ready
	var r *agentv1.CallResult
wait:
	for {
		select {
		case <-ready:
			ready = nil
			if attached != nil {
				attached(link)
			}
		case m := <-link.up:
			// The stream came before its first message, so whoever waits hears of the
			// link first: what's typed as soon as the client hears it's attached has
			// somewhere to go.
			if ready != nil {
				ready = nil
				if attached != nil {
					attached(link)
				}
			}
			if err := each(m); err != nil {
				cancel()
				return nil, err
			}
		case r = <-result:
			break wait
		case <-s.done:
			select {
			case r = <-result:
				break wait
			default:
			}
			if s.moving() {
				return nil, s.stoppedToMove()
			}
			return nil, fault.New(ErrLost, "%s's agent disconnected while it carried a terminal", s.name)
		case <-ctx.Done():
			cancel()
			return nil, ctx.Err()
		}
	}
	// What the agent sent before its result is all on its way up: take it first.
	for {
		select {
		case m := <-link.up:
			if err := each(m); err != nil {
				return nil, err
			}
			continue
		case <-link.gone:
		case <-time.After(time.Second):
		}
		break
	}
	switch e := r.GetError(); {
	case e == nil:
		return r.GetAttachTerminal(), nil
	case e.GetDraining():
		return nil, s.stoppedToMove()
	default:
		return nil, &resource.ServerError{Server: s.name, Err: errors.New(e.GetMessage())}
	}
}

// Terminal carries one attachment of a terminal: what it prints comes up, and is
// handed to whoever waits for it, who reads no more until they take it, so flow
// control holds up the agent, and the terminal's shell; what's typed goes down.
func (h *Handler) Terminal(ctx context.Context, stream *connect.BidiStream[agentv1.TerminalRequest, agentv1.TerminalResponse]) error {
	peer, err := requireServer(ctx)
	if err != nil {
		return err
	}
	if _, err := h.auth.CheckConnection(ctx, peer.ServerID, peer.Serial); err != nil {
		return h.refuse(ctx, peer, err)
	}
	if h.agents == nil {
		return connect.NewError(connect.CodeFailedPrecondition, errors.New("this control plane opens no terminals"))
	}
	first, err := stream.Receive()
	if err != nil {
		return err
	}
	l := h.agents.linkFor(peer.ServerID, first.GetStream())
	if l == nil {
		// Whoever attached has gone, or it was never asked for.
		return connect.NewError(connect.CodeNotFound, errors.New("nobody is waiting for this terminal"))
	}
	defer l.goneOnce.Do(func() { close(l.gone) })
	// What's typed goes down from other calls' goroutines, so none sends once this
	// returns, as the stream is closing then.
	var sendMu sync.Mutex
	closed := false
	defer func() {
		sendMu.Lock()
		closed = true
		sendMu.Unlock()
	}()
	l.down = func(m *agentv1.TerminalResponse) error {
		sendMu.Lock()
		defer sendMu.Unlock()
		if closed {
			return io.EOF
		}
		return stream.Send(m)
	}
	close(l.ready)
	deliver := func(m *agentv1.TerminalRequest) error {
		if m.GetTaken() {
			select {
			case l.taken <- m.GetInputOffset():
			default: // nobody waits for it
			}
			if len(m.GetData()) == 0 && m.GetStream() == 0 {
				return nil
			}
		}
		select {
		case l.up <- m:
			return nil
		case <-l.done:
			return io.EOF
		case <-ctx.Done():
			return ctx.Err()
		}
	}
	if err := deliver(first); err != nil {
		return nil
	}
	return h.whileIn(ctx, peer, func() error {
		for {
			m, err := stream.Receive()
			if errors.Is(err, io.EOF) {
				return nil
			}
			if err != nil {
				return err
			}
			if err := deliver(m); err != nil {
				return nil //nolint:nilerr // whoever attached has gone, which ends the terminal as it should
			}
		}
	})
}
