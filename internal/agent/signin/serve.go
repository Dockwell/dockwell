// SPDX-License-Identifier: AGPL-3.0-only

package signin

import (
	"bufio"
	"bytes"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net"
	"net/http"
	"runtime/debug"
	"strconv"
	"sync"
	"time"
)

// Serve answers what the proxy asks on ln with h, over HTTP/1.1, as Caddy's
// reverse_proxy asks: one request at a time on each connection, which it keeps
// between them. It leaves net/http's server out, which with its HTTP/2 the agent
// build would otherwise link for this alone, at about half a megabyte of every
// server's memory (ADR 0093). Only root and the proxy reach ln. It returns what
// stopped it, net.ErrClosed once ln is closed, and closes the connections it kept.
func Serve(ln net.Listener, h http.Handler, wait time.Duration, logger *slog.Logger) error {
	var mu sync.Mutex
	live := map[net.Conn]struct{}{}
	for {
		conn, err := ln.Accept()
		if err != nil {
			// Nothing answers on a connection Caddy kept once the agent stops
			// answering, so a route that signs in reads as down, never as open
			// (#598's final check, L4).
			mu.Lock()
			for c := range live {
				_ = c.Close()
			}
			mu.Unlock()
			return err
		}
		mu.Lock()
		live[conn] = struct{}{}
		mu.Unlock()
		go func() {
			serveConn(conn, h, wait, logger)
			mu.Lock()
			delete(live, conn)
			mu.Unlock()
		}()
	}
}

const (
	// maxRequest bounds a request's body, which no ask of the agent's has, and an
	// answer.
	maxRequest = 64 << 10
	// maxHeader bounds a request's line and headers, as net/http's server does
	// (#598's final check, L1).
	maxHeader = 1<<20 + 4096
)

// serveConn answers one connection's requests until it ends, or one goes wrong. A
// handler that panics is logged, and its connection closed with no answer, so the
// proxy answers 502: the agent goes on, and the route stays closed (#598's final
// check, M1).
func serveConn(conn net.Conn, h http.Handler, wait time.Duration, logger *slog.Logger) {
	defer func() {
		if p := recover(); p != nil {
			logger.Error("the agent's sign-in failed answering the proxy, so this request got 502. Report it with this line",
				slog.String("panic", fmt.Sprint(p)), slog.String("stack", string(debug.Stack())))
		}
		_ = conn.Close()
	}()
	head := &io.LimitedReader{R: conn}
	r := bufio.NewReaderSize(head, 4096)
	for {
		head.N = maxHeader
		_ = conn.SetReadDeadline(time.Now().Add(wait))
		req, err := http.ReadRequest(r)
		if err != nil {
			return
		}
		// What's left is the body's, which the drain bounds.
		head.N = maxRequest + 1
		w := &answer{header: http.Header{}}
		h.ServeHTTP(w, req)
		// No ask of the agent's has a body; one past the bound ends the connection.
		// The drain waits no longer than reading a request does (L2).
		_ = conn.SetReadDeadline(time.Now().Add(wait))
		left, _ := io.Copy(io.Discard, io.LimitReader(req.Body, maxRequest+1))
		_ = req.Body.Close()
		if err := w.send(conn, req, wait); err != nil || req.Close || left > maxRequest {
			return
		}
	}
}

// answer is what a handler writes, kept whole until it's sent, as the agent's
// answers are a few hundred bytes.
type answer struct {
	header http.Header
	status int
	body   bytes.Buffer
}

func (a *answer) Header() http.Header { return a.header }

func (a *answer) WriteHeader(status int) {
	if a.status == 0 {
		a.status = status
	}
}

func (a *answer) Write(b []byte) (int, error) {
	a.WriteHeader(http.StatusOK)
	if a.body.Len()+len(b) > maxRequest {
		return 0, errors.New("an answer to the proxy is larger than the agent sends")
	}
	return a.body.Write(b)
}

// send writes the answer as HTTP/1.1, with its length, so the connection can carry
// the next request, within wait (L2).
func (a *answer) send(conn net.Conn, req *http.Request, wait time.Duration) error {
	a.WriteHeader(http.StatusOK)
	a.header.Set("Content-Length", strconv.Itoa(a.body.Len()))
	res := &http.Response{
		StatusCode: a.status, ProtoMajor: 1, ProtoMinor: 1, Header: a.header,
		ContentLength: int64(a.body.Len()), Body: io.NopCloser(&a.body), Request: req,
	}
	_ = conn.SetWriteDeadline(time.Now().Add(wait))
	defer func() { _ = conn.SetWriteDeadline(time.Time{}) }()
	w := bufio.NewWriter(conn)
	if err := res.Write(w); err != nil {
		return err
	}
	return w.Flush()
}
