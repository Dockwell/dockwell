// SPDX-License-Identifier: AGPL-3.0-only

package logs

import (
	"context"
	"net/http"
	"strings"
	"sync"
	"time"
)

const (
	// writeStall is how long a write to a reader can make no progress before the
	// reader counts as gone. A stream can stay quiet for as long as its log does,
	// so this isn't a limit on a request, only on a stalled write. A reader that
	// was only paused carries on from where it was.
	writeStall = 30 * time.Second
	// writeStep is how much goes out at a time, each with that long to go, so a
	// slow reader of a large page is fine as long as it reads at all: at least
	// 64 KiB each 30 s.
	writeStep = 64 << 10
)

// Guard bounds what a reader that stops reading can hold of the control plane:
// each write has to make progress, and a page's place is held until it has been
// written. It goes around every path that reads logs.
func (h *Handler) Guard(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		held := &pageHold{}
		defer held.release()
		out := &stallWriter{ResponseWriter: w, control: http.NewResponseController(w), stall: h.options.WriteStall}
		next.ServeHTTP(out, r.WithContext(context.WithValue(r.Context(), pageHoldKey{}, held)))
	})
}

// GuardREST is Guard for the REST paths that read logs, among all the others.
func (h *Handler) GuardREST(next http.Handler) http.Handler {
	guarded := h.Guard(next)
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if p := r.URL.Path; strings.HasPrefix(p, "/v1/services/") && (strings.HasSuffix(p, "/logs") || strings.HasSuffix(p, "/logSources")) {
			guarded.ServeHTTP(w, r)
			return
		}
		next.ServeHTTP(w, r)
	})
}

type pageHoldKey struct{}

// pageHold lets go of a page's place once its response has been written.
type pageHold struct {
	mu   sync.Mutex
	done []func()
}

func (p *pageHold) release() {
	p.mu.Lock()
	defer p.mu.Unlock()
	for _, f := range p.done {
		f()
	}
	p.done = nil
}

// holdUntilWritten has release run once the response has been written, and says
// whether anything around this request will.
func holdUntilWritten(ctx context.Context, release func()) bool {
	held, ok := ctx.Value(pageHoldKey{}).(*pageHold)
	if !ok {
		return false
	}
	held.mu.Lock()
	defer held.mu.Unlock()
	held.done = append(held.done, release)
	return true
}

// stallWriter gives up on a reader that has stopped reading. It keeps no deadline
// between writes, so a quiet stream can stay open for as long as it likes.
type stallWriter struct {
	http.ResponseWriter
	control *http.ResponseController
	stall   time.Duration
}

func (w *stallWriter) Write(p []byte) (int, error) {
	written := 0
	for len(p) > 0 {
		n := min(len(p), writeStep)
		_ = w.control.SetWriteDeadline(time.Now().Add(w.stall)) // a writer that takes none has no reader to stall
		m, err := w.ResponseWriter.Write(p[:n])
		_ = w.control.SetWriteDeadline(time.Time{})
		written += m
		if err != nil {
			return written, err
		}
		p = p[n:]
	}
	return written, nil
}

// FlushError sends what is buffered, within the same limit.
func (w *stallWriter) FlushError() error {
	_ = w.control.SetWriteDeadline(time.Now().Add(w.stall))
	defer func() { _ = w.control.SetWriteDeadline(time.Time{}) }()
	return w.control.Flush()
}

// Flush is FlushError for what can't take an error.
func (w *stallWriter) Flush() { _ = w.FlushError() }

// Unwrap lets a ResponseController reach what is underneath.
func (w *stallWriter) Unwrap() http.ResponseWriter { return w.ResponseWriter }
