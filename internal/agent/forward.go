// SPDX-License-Identifier: AGPL-3.0-only

package agent

import (
	"context"
	"errors"
	"fmt"
	"net"
	"strconv"
	"sync"
	"sync/atomic"
	"time"

	agentv1 "github.com/dockwell/dockwell/gen/dockwell/agent/v1"
	"github.com/dockwell/dockwell/internal/docker"
)

// forwardDial is how long connecting to a service's port may take.
const forwardDial = 10 * time.Second

// forwardChunk is the most a message of Forward carries.
const forwardChunk = 32 << 10

// forward carries one connection to the port a service publishes, over a connection
// of the agent's own to the control plane, as a terminal's bytes go (ADR 0061). It
// finds the service's running container, and where Docker publishes the port the
// call names, itself, and dials that: never an address the call names.
func (c *calls) forward(ctx context.Context, call *agentv1.ForwardCall) *agentv1.CallResult {
	fail := func(err error) *agentv1.CallResult {
		return &agentv1.CallResult{Result: &agentv1.CallResult_Error{Error: &agentv1.CallError{Message: err.Error(), Draining: c.isClosing() && c.ctx.Err() == nil}}}
	}
	if c.cfg.Docker == nil {
		return fail(errors.New("this agent can't reach Docker, so it opens no app"))
	}
	addr, err := published(ctx, c.cfg.Docker, call.GetProject(), call.GetService(), call.GetPort())
	if err != nil {
		return fail(err)
	}
	conn, err := (&net.Dialer{Timeout: forwardDial}).DialContext(ctx, "tcp", addr)
	if err != nil {
		return fail(fmt.Errorf("%s doesn't answer on %s: %w", call.GetService(), addr, err))
	}
	defer conn.Close()
	client, release, err := c.cfg.Identity.Client()
	if err != nil {
		return fail(err)
	}
	defer release()
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
