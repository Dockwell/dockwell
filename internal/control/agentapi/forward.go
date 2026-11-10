// SPDX-License-Identifier: AGPL-3.0-only

package agentapi

import (
	"context"
	"crypto/rand"
	"encoding/binary"
	"errors"
	"fmt"
	"io"
	"net"
	"sync"
	"time"

	"connectrpc.com/connect"

	agentv1 "github.com/dockwell/dockwell/gen/dockwell/agent/v1"
	"github.com/dockwell/dockwell/internal/resource"
)

// ForwardOpen is how long an agent has to open a forward's stream.
const ForwardOpen = 15 * time.Second

// forwardLonger is how long a forward may last at most, as a call needs a bound: a
// connection a browser keeps open for an app's own stream lasts no longer.
const forwardLonger = 12 * time.Hour

// forwardChunk is the most a message of Forward carries.
const forwardChunk = 32 << 10

// Forwards reports an agent that carries a connection to a port a service
// publishes (ADR 0061).
func (s *Session) Forwards() bool { return s.forwards }

// forwardLink is one forwarded connection, waiting for the agent's Forward stream,
// then carrying it: the control plane's end of a pipe, whose other end is what
// Session.Forward returns.
type forwardLink struct {
	server  string
	token   uint64
	claimed bool          // by Agents.mu, once its stream has come
	end     net.Conn      // the pipe's end the stream pumps
	ready   chan struct{} // closed once the stream has come
}

func (a *Agents) openForward(server string, end net.Conn) *forwardLink {
	a.mu.Lock()
	defer a.mu.Unlock()
	for {
		var b [8]byte
		_, _ = rand.Read(b[:])
		token := binary.BigEndian.Uint64(b[:])
		if _, taken := a.forwards[token]; token == 0 || taken {
			continue
		}
		l := &forwardLink{server: server, token: token, end: end, ready: make(chan struct{})}
		a.forwards[token] = l
		return l
	}
}

func (a *Agents) closeForward(l *forwardLink) {
	a.mu.Lock()
	defer a.mu.Unlock()
	delete(a.forwards, l.token)
}

// forwardFor finds a forward of a server waiting for its stream, and takes it, so a
// token works once.
func (a *Agents) forwardFor(server string, token uint64) *forwardLink {
	a.mu.Lock()
	defer a.mu.Unlock()
	l := a.forwards[token]
	if l == nil || l.server != server || l.claimed {
		return nil
	}
	l.claimed = true
	return l
}

// Forward asks the agent to connect to the port a service publishes for port in its
// container, and returns the connection, carried over a stream of the agent's own.
// Closing it ends the forward on the agent. The agent finds the container and where
// the port is published itself (ADR 0061).
func (s *Session) Forward(ctx context.Context, project, service string, port uint32) (net.Conn, error) {
	if !s.forwards {
		return nil, fmt.Errorf("%s's agent is too old to open an app through the panel. Update the agent on %s, then try again", s.name, s.name)
	}
	mine, theirs := net.Pipe()
	link := s.agents.openForward(s.server, theirs)
	callCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), forwardLonger)
	ended := make(chan error, 1)
	go func() {
		defer cancel()
		defer s.agents.closeForward(link)
		defer theirs.Close()
		r, err := s.call(callCtx, &agentv1.Call{Call: &agentv1.Call_Forward{Forward: &agentv1.ForwardCall{
			Project: project, Service: service, Port: port, Stream: link.token,
		}}}, forwardLonger, nil)
		if err == nil && r.GetError() != nil {
			err = &resource.ServerError{Server: s.name, Err: errors.New(r.GetError().GetMessage())}
		}
		ended <- err
	}()
	wait := time.NewTimer(ForwardOpen)
	defer wait.Stop()
	select {
	case <-link.ready:
		return &forwardConn{Conn: mine, stop: cancel}, nil
	case err := <-ended:
		_ = mine.Close()
		if err == nil {
			err = fmt.Errorf("%s's agent ended the forward before it began", s.name)
		}
		return nil, err
	case <-wait.C:
		cancel()
		_ = mine.Close()
		return nil, fmt.Errorf("%s's agent didn't open the forward within %s", s.name, ForwardOpen)
	case <-ctx.Done():
		cancel()
		_ = mine.Close()
		return nil, ctx.Err()
	}
}

// forwardConn is a forwarded connection: closing it ends the agent's call too.
type forwardConn struct {
	net.Conn
	stop context.CancelFunc
}

func (c *forwardConn) Close() error {
	err := c.Conn.Close()
	c.stop()
	return err
}

// Forward carries one forwarded connection: what the service sends comes up into
// the pipe, and what's written to the pipe goes down to it.
func (h *Handler) Forward(ctx context.Context, stream *connect.BidiStream[agentv1.ForwardRequest, agentv1.ForwardResponse]) error {
	peer, err := requireServer(ctx)
	if err != nil {
		return err
	}
	if _, err := h.auth.CheckConnection(ctx, peer.ServerID, peer.Serial); err != nil {
		return h.refuse(ctx, peer, err)
	}
	if h.agents == nil {
		return connect.NewError(connect.CodeFailedPrecondition, errors.New("this control plane forwards nothing"))
	}
	first, err := stream.Receive()
	if err != nil {
		return err
	}
	l := h.agents.forwardFor(peer.ServerID, first.GetStream())
	if l == nil {
		return connect.NewError(connect.CodeNotFound, errors.New("nobody is waiting for this forward"))
	}
	close(l.ready)
	// Down: what the client writes goes to the service, until the pipe closes. Nothing
	// sends once this handler returns, as the stream is closing then.
	var sendMu sync.Mutex
	closed := false
	send := func(m *agentv1.ForwardResponse) bool {
		sendMu.Lock()
		defer sendMu.Unlock()
		return !closed && stream.Send(m) == nil
	}
	down := make(chan struct{})
	go func() {
		defer close(down)
		buf := make([]byte, forwardChunk)
		for {
			n, err := l.end.Read(buf)
			if n > 0 && !send(&agentv1.ForwardResponse{Data: buf[:n]}) {
				return
			}
			if err != nil {
				send(&agentv1.ForwardResponse{Closed: true})
				return
			}
		}
	}()
	// Up: what the service says goes into the pipe, until it closes or the stream ends.
	var ended error
	for m := first; ; {
		if data := m.GetData(); len(data) > 0 {
			if _, err := l.end.Write(data); err != nil {
				break // the client closed: nothing more goes to it
			}
		}
		if m.GetClosed() {
			break
		}
		if m, ended = stream.Receive(); ended != nil {
			break
		}
	}
	_ = l.end.Close()
	sendMu.Lock()
	closed = true
	sendMu.Unlock()
	<-down
	if ended != nil && !errors.Is(ended, io.EOF) && ctx.Err() == nil {
		return ended
	}
	return nil
}
