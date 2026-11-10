// SPDX-License-Identifier: AGPL-3.0-only

package agent

import (
	"context"
	"errors"
	"fmt"
	"net"
	"slices"
	"strconv"
	"sync"
	"sync/atomic"
	"time"

	agentv1 "github.com/dockwell/dockwell/gen/dockwell/agent/v1"
	"github.com/dockwell/dockwell/gen/dockwell/agent/v1/agentv1connect"
	"github.com/dockwell/dockwell/internal/agent/enroll"
	"github.com/dockwell/dockwell/internal/docker"
)

// forwardDial is how long connecting to a service's port may take.
const forwardDial = 10 * time.Second

// forwardChunk is the most a message of Forward carries.
const forwardChunk = 32 << 10

const (
	// forwardsPerConn is how many forwards share a connection to the control plane
	// (enroll.ForwardClient): each takes in at most its window of what the browser
	// sends before the agent writes it to the app, and the connection theirs
	// together, so a forward to an app that reads slowly holds up no other.
	forwardsPerConn = enroll.ForwardsPerConn
	// maxForwards is how many connections to apps opened through the panel this
	// agent carries at once: four apps with four busy connections each. Each holds
	// up to its window and a message or two each way, about 200 KiB when busy, and
	// an agent's memory has a budget of 40 MB, so past them it refuses, with room
	// to spare as the agent grows. make forwards-load holds it to that with every
	// one busy.
	maxForwards = 16
)

// errTooManyForwards is a forward past maxForwards.
var errTooManyForwards = fmt.Errorf("this server carries %d connections to apps opened through the panel already, the most it carries at once. Close tabs of apps you opened through the panel, then try again", maxForwards)

// forwardPool is the connections forwards go over, a few forwards to each, and
// how many forwards there are.
type forwardPool struct {
	mu    sync.Mutex
	open  int
	conns []*forwardConn
}

type forwardConn struct {
	client  agentv1connect.AgentServiceClient
	release func()
	carries int
}

// take gives a forward a connection: one with room, or a new one, which goes once
// it carries nothing, so a renewed certificate reaches the next. It says how to
// give it back.
func (p *forwardPool) take(id *enroll.Identity) (agentv1connect.AgentServiceClient, func(), error) {
	p.mu.Lock()
	defer p.mu.Unlock()
	if p.open >= maxForwards {
		return nil, nil, errTooManyForwards
	}
	var fc *forwardConn
	for _, c := range p.conns {
		if c.carries < forwardsPerConn {
			fc = c
			break
		}
	}
	if fc == nil {
		client, release, err := id.ForwardClient()
		if err != nil {
			return nil, nil, err
		}
		fc = &forwardConn{client: client, release: release}
		p.conns = append(p.conns, fc)
	}
	fc.carries++
	p.open++
	var once sync.Once
	return fc.client, func() {
		once.Do(func() {
			p.mu.Lock()
			defer p.mu.Unlock()
			fc.carries--
			p.open--
			if fc.carries == 0 {
				p.conns = slices.DeleteFunc(p.conns, func(c *forwardConn) bool { return c == fc })
				fc.release()
			}
		})
	}, nil
}

// forward carries one connection to the port a service publishes, over a stream of
// a connection to the control plane it shares with a few other forwards (ADR 0061).
// It finds the service's running container, and where Docker publishes the port the
// call names, itself, and dials that: never an address the call names.
func (c *calls) forward(ctx context.Context, call *agentv1.ForwardCall) *agentv1.CallResult {
	fail := func(err error) *agentv1.CallResult {
		return &agentv1.CallResult{Result: &agentv1.CallResult_Error{Error: &agentv1.CallError{Message: err.Error(), Draining: c.isClosing() && c.ctx.Err() == nil}}}
	}
	if c.cfg.Docker == nil || c.cfg.forwards == nil {
		return fail(errors.New("this agent can't reach Docker, so it opens no app"))
	}
	client, release, err := c.cfg.forwards.take(c.cfg.Identity)
	if err != nil {
		return fail(err)
	}
	defer release()
	addr, err := published(ctx, c.cfg.Docker, call.GetProject(), call.GetService(), call.GetPort())
	if err != nil {
		return fail(err)
	}
	conn, err := (&net.Dialer{Timeout: forwardDial}).DialContext(ctx, "tcp", addr)
	if err != nil {
		return fail(fmt.Errorf("%s doesn't answer on %s: %w", call.GetService(), addr, err))
	}
	defer conn.Close()
	streamCtx, cancel := context.WithCancel(ctx)
	defer cancel()
	stream := client.Forward(streamCtx)
	defer func() { _ = stream.CloseRequest(); _ = stream.CloseResponse() }()
	if err := stream.Send(&agentv1.ForwardRequest{Stream: call.GetStream()}); err != nil {
		return fail(fmt.Errorf("can't open the forward's stream: %w", err))
	}
	var sent, received atomic.Uint64
	var wg sync.WaitGroup
	wg.Add(2)
	// What the service says goes up, until it closes.
	go func() {
		defer wg.Done()
		buf := make([]byte, forwardChunk)
		for {
			n, err := conn.Read(buf)
			if n > 0 {
				if stream.Send(&agentv1.ForwardRequest{Data: buf[:n]}) != nil {
					cancel()
					return
				}
				sent.Add(uint64(n))
			}
			if err != nil {
				_ = stream.Send(&agentv1.ForwardRequest{Closed: true})
				return
			}
		}
	}()
	// What the client says comes down, until it closes, or the stream ends.
	go func() {
		defer wg.Done()
		defer closeWrite(conn)
		for {
			m, err := stream.Receive()
			if err != nil {
				cancel()
				_ = conn.Close()
				return
			}
			if data := m.GetData(); len(data) > 0 {
				if _, err := conn.Write(data); err != nil {
					cancel()
					return
				}
				received.Add(uint64(len(data)))
			}
			if m.GetClosed() {
				return
			}
		}
	}()
	go func() {
		<-streamCtx.Done()
		_ = conn.Close()
	}()
	wg.Wait()
	return &agentv1.CallResult{Result: &agentv1.CallResult_Forward{Forward: &agentv1.ForwardResult{Sent: sent.Load(), Received: received.Load()}}}
}

// closeWrite tells the service the client has said all it will, where the
// connection can say that alone.
func closeWrite(conn net.Conn) {
	if tcp, ok := conn.(interface{ CloseWrite() error }); ok {
		_ = tcp.CloseWrite()
	}
}

// published finds where Docker publishes a port of a service's running container on
// this server: its address, or loopback for one published on every address.
func published(ctx context.Context, client *docker.Client, project, service string, port uint32) (string, error) {
	if project == "" || service == "" || port == 0 || port > 65535 {
		return "", errors.New("a forward names a service and a port in its container")
	}
	list, err := client.ListContainers(ctx, map[string][]string{"label": {
		docker.LabelProject + "=" + project, docker.LabelService + "=" + service,
	}})
	if err != nil {
		return "", fmt.Errorf("can't list %s's containers: %w", service, err)
	}
	for i := range list {
		s := &list[i]
		if s.State != "running" || s.Labels[docker.LabelOneoff] == "True" {
			continue
		}
		for _, p := range s.Ports {
			if uint32(p.PrivatePort) != port || p.PublicPort == 0 || p.Type != "tcp" {
				continue
			}
			host := p.IP
			switch host {
			case "", "0.0.0.0":
				host = "127.0.0.1"
			case "::":
				host = "::1"
			}
			return net.JoinHostPort(host, strconv.Itoa(int(p.PublicPort))), nil
		}
	}
	return "", fmt.Errorf("%s publishes no port for %d in a container that runs on this server. Publish it on its page, then open it again", service, port)
}
