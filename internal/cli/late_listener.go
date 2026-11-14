// SPDX-License-Identifier: AGPL-3.0-only

package cli

import (
	"errors"
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

// listenWords say what listenWhenItCan finds: that it can't listen yet, and what
// that keeps from working, and that it listens again.
type listenWords struct {
	cant  func(path string, err error, every time.Duration)
	again func(path string)
}

// listenWhenItCan is a listener that waits for listen, which runs behind it and never
// ahead of it. A socket in a directory Caddy owns can be taken by a compromised Caddy,
// which can keep putting something at that name, and win the moment between clearing
// it and listening on it, or fill a directory there for as long as it likes, and
// clearing that doesn't end while it writes. Neither may stop dockwell run. So what
// it serves goes without for now, and this goes on trying until it has it.
func listenWhenItCan(path string, every time.Duration, words listenWords, listen func() (net.Listener, error)) net.Listener {
	warned := false
	warn := func(err error) {
		warned = true
		words.cant(path, err, every)
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
				// It says so before Accept can go on, so the log never has what it
				// serves answering ahead of the line that says it does.
				if warned {
					words.again(path)
				}
				l.ln = got.ln
				close(l.bound)
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
