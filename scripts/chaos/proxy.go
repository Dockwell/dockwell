// SPDX-License-Identifier: AGPL-3.0-only

package main

import (
	"context"
	"errors"
	"io"
	"net"
	"sync"
)

// What the proxy does with the connection between chaos-1's agent and the
// control plane.
const (
	// pass carries every byte both ways.
	pass = iota
	// cut carries nothing and closes nothing, as a network that loses every
	// packet: what is sent waits in the sockets' buffers, and a side that closes
	// is heard only once the cut heals, as its close is a packet too.
	cut
	// refuse closes every connection and each new one at once, as a server that
	// isn't listening: the agent sees its stream end straight away.
	refuse
)

// proxy stands between chaos-1's agent and the control plane's agent listener,
// so a scenario can cut the connection without touching either side.
type proxy struct {
	ctx    context.Context
	cancel context.CancelFunc
	ln     net.Listener
	target string

	mu    sync.Mutex
	mode  int
	moved *sync.Cond // broadcast as the mode changes
	conns map[net.Conn]struct{}
}

func newProxy(ctx context.Context, target string) (*proxy, error) {
	ln, err := (&net.ListenConfig{}).Listen(ctx, "tcp", "127.0.0.1:0")
	if err != nil {
		return nil, err
	}
	ctx, cancel := context.WithCancel(ctx)
	p := &proxy{ctx: ctx, cancel: cancel, ln: ln, target: target, conns: map[net.Conn]struct{}{}}
	p.moved = sync.NewCond(&p.mu)
	go p.serve()
	return p, nil
}

// addr is where the agent reaches the control plane through the proxy.
func (p *proxy) addr() string { return p.ln.Addr().String() }

func (p *proxy) close() {
	p.cancel()
	p.set(refuse)
	_ = p.ln.Close()
}

// set changes what the proxy does. Refusing closes what's open now.
func (p *proxy) set(mode int) {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.mode = mode
	if mode == refuse {
		for c := range p.conns {
			_ = c.Close()
		}
	}
	p.moved.Broadcast()
}

func (p *proxy) serve() {
	for {
		down, err := p.ln.Accept()
		if err != nil {
			return
		}
		go p.carry(down)
	}
}

// carry joins one connection from the agent to the control plane.
func (p *proxy) carry(down net.Conn) {
	p.mu.Lock()
	refused := p.mode == refuse
	p.mu.Unlock()
	if refused {
		_ = down.Close()
		return
	}
	up, err := (&net.Dialer{}).DialContext(p.ctx, "tcp", p.target)
	if err != nil {
		_ = down.Close()
		return
	}
	if !p.track(down, up) {
		return
	}
	var wg sync.WaitGroup
	wg.Add(2)
	go func() { defer wg.Done(); p.copy(up, down) }()
	go func() { defer wg.Done(); p.copy(down, up) }()
	wg.Wait()
	p.mu.Lock()
	delete(p.conns, down)
	delete(p.conns, up)
	p.mu.Unlock()
}

// track keeps both ends, so refusing can close them; it closes them and says
// so if the proxy refuses already.
func (p *proxy) track(down, up net.Conn) bool {
	p.mu.Lock()
	defer p.mu.Unlock()
	if p.mode == refuse {
		_ = down.Close()
		_ = up.Close()
		return false
	}
	p.conns[down], p.conns[up] = struct{}{}, struct{}{}
	return true
}

// copy carries one direction. During a cut it reads nothing, so the sender's
// buffers fill as they would, and it holds what it read and a close it saw
// until the cut heals.
func (p *proxy) copy(dst, src net.Conn) {
	buf := make([]byte, 32<<10)
	for p.through() {
		n, err := src.Read(buf)
		if n > 0 {
			if !p.through() {
				break
			}
			if _, werr := dst.Write(buf[:n]); werr != nil {
				break
			}
		}
		if err != nil {
			if !errors.Is(err, io.EOF) {
				_ = src.Close()
			}
			break
		}
	}
	p.through()
	_ = dst.Close()
	_ = src.Close()
}

// through waits out a cut, and reports whether bytes may pass: not once the
// proxy refuses, which has closed the connection.
func (p *proxy) through() bool {
	p.mu.Lock()
	defer p.mu.Unlock()
	for p.mode == cut {
		p.moved.Wait()
	}
	return p.mode == pass
}
