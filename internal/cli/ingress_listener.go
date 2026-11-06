// SPDX-License-Identifier: AGPL-3.0-only

//go:build !agent

package cli

import (
	"log/slog"
	"net"
	"sync"
	"time"
)

// ingressListenRetry is how often the panel's socket is tried again, once it
// couldn't be listened on.
const ingressListenRetry = 5 * time.Second

// ingressListenWarnEvery is how many tries pass between saying it's still so.
const ingressListenWarnEvery = 12

// listenForIngressWhenItCan is listen, or a listener that waits for it. The panel's
// socket is in a directory Caddy owns, so a compromised Caddy that keeps putting
// something at that name can win the moment between clearing it and listening on it.
// That mustn't stop dockwell run, as the SSH tunnel to the panel goes with it. So the
// panel goes without its domain for now, and this tries again until it has it.
func listenForIngressWhenItCan(logger *slog.Logger, path string, every time.Duration, listen func() (net.Listener, error)) net.Listener {
	ln, err := listen()
	if err == nil {
		return ln
	}
	warn := func(err error) {
		logger.Warn("can't listen for what the ingress sends the panel, so the panel doesn't answer on its domain until the agent can. The SSH tunnel to the panel works, and this is tried again. If it goes on, something keeps taking the socket: look at what's running in the ingress's container",
			slog.String("socket", path), slog.String("error", err.Error()), slog.Duration("every", every))
	}
	warn(err)
	l := &lateListener{addr: &net.UnixAddr{Name: path, Net: "unix"}, bound: make(chan struct{}), done: make(chan struct{}), stopped: make(chan struct{})}
	go func() {
		defer close(l.stopped)
		ticker := time.NewTicker(every)
		defer ticker.Stop()
		for tries := 1; ; tries++ {
			select {
			case <-l.done:
				return
			case <-ticker.C:
			}
			ln, err := listen()
			if err != nil {
				if tries%ingressListenWarnEvery == 0 {
					warn(err)
				}
				continue
			}
			l.ln = ln
			close(l.bound)
			logger.Info("listening for what the ingress sends the panel, so the panel answers on its domain again", slog.String("socket", path))
			return
		}
	}()
	return l
}

// lateListener is a listener that isn't one yet. Accept waits for it, so what serves
// on it needs to know nothing of that.
type lateListener struct {
	addr net.Addr
	// ln is set before bound closes.
	ln            net.Listener
	bound         chan struct{}
	done, stopped chan struct{}
	stop          sync.Once
}

// Accept waits until it's listening, or closed.
func (l *lateListener) Accept() (net.Conn, error) {
	select {
	case <-l.done:
		return nil, net.ErrClosed
	case <-l.bound:
		return l.ln.Accept()
	}
}

// Close stops trying, and closes what it got.
func (l *lateListener) Close() error {
	l.stop.Do(func() { close(l.done) })
	<-l.stopped
	select {
	case <-l.bound:
		return l.ln.Close()
	default:
		return nil
	}
}

// Addr is where it listens, or will.
func (l *lateListener) Addr() net.Addr { return l.addr }
