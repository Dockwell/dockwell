// SPDX-License-Identifier: AGPL-3.0-only

//go:build !agent

package cli

import (
	"errors"
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

// errNotCleared says a try is still on its way, which is what a directory that keeps
// filling at the socket's name looks like.
var errNotCleared = errors.New("clearing what stands at the socket hasn't finished")

// listenForIngressWhenItCan is a listener that waits for listen, which runs behind it
// and never ahead of it. The panel's socket is in a directory Caddy owns, so a
// compromised Caddy can keep putting something at that name, and win the moment
// between clearing it and listening on it, or fill a directory there for as long as
// it likes, and clearing that doesn't end while it writes. Neither may stop dockwell
// run, as the SSH tunnel to the panel goes with it. So the panel goes without its
// domain for now, and this goes on trying until it has it.
func listenForIngressWhenItCan(logger *slog.Logger, path string, every time.Duration, listen func() (net.Listener, error)) net.Listener {
	warned := false
	warn := func(err error) {
		warned = true
		logger.Warn("can't listen for what the ingress sends the panel, so the panel doesn't answer on its domain until the agent can. The SSH tunnel to the panel works, and this is tried again. If it goes on, something keeps taking the socket: look at what's running in the ingress's container",
			slog.String("socket", path), slog.String("error", err.Error()), slog.Duration("every", every))
	}
	l := &lateListener{addr: &net.UnixAddr{Name: path, Net: "unix"}, bound: make(chan struct{}), done: make(chan struct{}), stopped: make(chan struct{})}
	go func() {
		defer close(l.stopped)
		ticker := time.NewTicker(every)
		defer ticker.Stop()
		for tries := 0; ; tries++ {
			got, ok := l.try(listen, ticker.C, warn)
			if !ok {
				return
			}
			if got.err == nil {
				l.ln = got.ln
				close(l.bound)
				if warned {
					logger.Info("listening for what the ingress sends the panel, so the panel answers on its domain again", slog.String("socket", path))
				}
				return
			}
			if tries%ingressListenWarnEvery == 0 {
				warn(got.err)
			}
			select {
			case <-l.done:
				return
			case <-ticker.C:
			}
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

// tried is what one try at listening came to.
type tried struct {
	ln  net.Listener
	err error
}

// try runs listen, saying now and then that it hasn't finished, and reports false if
// the listener closes first. Closing doesn't wait for it, as it may not end, so what
// it gets by then is closed as it comes.
func (l *lateListener) try(listen func() (net.Listener, error), ticks <-chan time.Time, warn func(error)) (tried, bool) {
	result := make(chan tried, 1)
	go func() {
		ln, err := listen()
		result <- tried{ln, err}
	}()
	for ticked := 0; ; ticked++ {
		select {
		case got := <-result:
			return got, true
		case <-l.done:
			go func() {
				if got := <-result; got.err == nil {
					_ = got.ln.Close()
				}
			}()
			return tried{}, false
		case <-ticks:
			if ticked%ingressListenWarnEvery == 0 {
				warn(errNotCleared)
			}
		}
	}
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
