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
	"net/netip"
	"runtime/debug"
	"strconv"
	"sync"
	"time"
)

// maxServeConns caps concurrent keep-alive connections handled on signin.sock so a
// request storm cannot multiply per-connection buffers past the agent's 40 MB budget (M4).
const maxServeConns = 32

// Serve answers what the proxy asks on ln with h, over HTTP/1.1, as Caddy's
// reverse_proxy asks: one request at a time on each connection, which it keeps
// between them. It leaves net/http's server out, which with its HTTP/2 the agent
// build would otherwise link for this alone, at about half a megabyte of every
// server's memory (ADR 0093). Only root and the proxy reach ln. It returns what
// stopped it, net.ErrClosed once ln is closed, and closes the connections it kept.
func Serve(ln net.Listener, h http.Handler, wait time.Duration, logger *slog.Logger) error {
	var mu sync.Mutex
	live := map[net.Conn]struct{}{}
	sem := make(chan struct{}, maxServeConns)
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
		sem <- struct{}{}
		mu.Lock()
		live[conn] = struct{}{}
		mu.Unlock()
		go func() {
			defer func() { <-sem }()
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
	r := bufio.NewReaderSize(head, 2048)
	bw := bufio.NewWriterSize(conn, 512)
	rlSrv, _ := h.(*Server)
	// Reuse answer across keep-alive requests to minimize per-request heap churn under floods (M4).
	w := answer{header: make(http.Header, 4)}
	for {
		head.N = maxHeader
		_ = conn.SetReadDeadline(time.Now().Add(wait))
		if rlSrv != nil {
			handled, keepOpen := tryServeRateLimitFast(conn, r, bw, head, rlSrv, wait)
			if handled {
				if !keepOpen {
					return
				}
				continue
			}
		}
		req, err := http.ReadRequest(r)
		if err != nil {
			return
		}
		// What's left is the body's, which the drain bounds.
		head.N = maxRequest + 1
		w.reset()
		h.ServeHTTP(&w, req)
		// No ask of the agent's has a body; one past the bound ends the connection.
		// The drain waits no longer than reading a request does (L2).
		_ = conn.SetReadDeadline(time.Now().Add(wait))
		left, _ := io.Copy(io.Discard, io.LimitReader(req.Body, maxRequest+1))
		_ = req.Body.Close()
		if err := w.send(conn, bw, req, wait); err != nil || req.Close || left > maxRequest {
			return
		}
	}
}

var (
	rateLimitPrefixBytes = []byte(RateLimitPrefix)
	clientHeaderBytes    = []byte(ClientHeader)
	connectionBytes      = []byte("Connection")
	contentLengthBytes   = []byte("Content-Length")
	transferEncBytes     = []byte("Transfer-Encoding")
	okEmptyCloseResp     = []byte("HTTP/1.1 200 OK\r\nContent-Length: 0\r\nConnection: close\r\n\r\n")
)

// tryServeRateLimitFast handles HTTP/1.1 /ratelimit/<routeID>?rpm=<N>&burst=<B> asks
// directly from the connection's bufio.Reader without allocating http.Request, url.URL,
// or header maps, keeping agent RSS under 40 MB even during concurrent file transfers
// and a multi-route rate-limit flood (M4).
func tryServeRateLimitFast(conn net.Conn, r *bufio.Reader, bw *bufio.Writer, head *io.LimitedReader, s *Server, wait time.Duration) (handled, keepOpen bool) {
	if _, err := r.Peek(1); err != nil {
		return true, false
	}
	peek, _ := r.Peek(r.Buffered())
	nl := bytes.IndexByte(peek, '\n')
	if nl < 0 {
		return false, true
	}
	firstLine := bytes.TrimRight(peek[:nl], "\r")
	sp1 := bytes.IndexByte(firstLine, ' ')
	if sp1 <= 0 {
		return false, true
	}
	rest := firstLine[sp1+1:]
	if !bytes.HasPrefix(rest, rateLimitPrefixBytes) {
		return false, true
	}

	// Consume request line (already buffered).
	line, err := r.ReadSlice('\n')
	if err != nil {
		return true, false
	}
	line = bytes.TrimRight(line, "\r\n")
	sp1 = bytes.IndexByte(line, ' ')
	if sp1 <= 0 {
		return true, false
	}
	isHead := len(line[:sp1]) == 4 && string(line[:sp1]) == http.MethodHead
	rest = line[sp1+1:]
	sp2 := bytes.IndexByte(rest, ' ')
	if sp2 <= 0 {
		return true, false
	}
	target := rest[:sp2]
	proto := rest[sp2+1:]
	closeConn := bytes.Equal(proto, []byte("HTTP/1.0"))

	afterPrefix := target[len(rateLimitPrefixBytes):]
	var idBytes, queryBytes []byte
	if q := bytes.IndexByte(afterPrefix, '?'); q >= 0 {
		idBytes = afterPrefix[:q]
		queryBytes = afterPrefix[q+1:]
	} else {
		idBytes = afterPrefix
	}
	var idBuf [128]byte
	var idSlice []byte
	if len(idBytes) <= len(idBuf) {
		copy(idBuf[:], idBytes)
		idSlice = idBuf[:len(idBytes)]
	} else {
		idSlice = append([]byte(nil), idBytes...)
	}
	rpm, burst, hasRate := parseRateQueryBytes(queryBytes)

	// Read headers with zero heap allocations.
	var clientAddr netip.Addr
	var contentLen int64
	var hasBodyTransfer bool
	for {
		hline, herr := r.ReadSlice('\n')
		if herr != nil {
			if !errors.Is(herr, bufio.ErrBufferFull) {
				return true, false
			}
			// Drain oversized single header line cleanly.
			for errors.Is(herr, bufio.ErrBufferFull) {
				_, herr = r.ReadSlice('\n')
			}
			if herr != nil {
				return true, false
			}
			continue
		}
		hline = bytes.TrimRight(hline, "\r\n")
		if len(hline) == 0 {
			break
		}
		colon := bytes.IndexByte(hline, ':')
		if colon <= 0 {
			continue
		}
		name := hline[:colon]
		val := trimASCIISpace(hline[colon+1:])
		switch {
		case equalFoldASCII(name, clientHeaderBytes):
			clientAddr = parseRateClientBytes(val)
		case equalFoldASCII(name, connectionBytes):
			if containsFoldASCII(val, "close") {
				closeConn = true
			}
		case equalFoldASCII(name, contentLengthBytes):
			if n, perr := parseUintBytes(val); perr == nil && n <= maxRequest {
				contentLen = int64(n)
			} else {
				closeConn = true
			}
		case equalFoldASCII(name, transferEncBytes):
			if len(val) > 0 {
				hasBodyTransfer = true
				closeConn = true
			}
		}
	}

	if hasBodyTransfer || contentLen > maxRequest {
		closeConn = true
	} else if contentLen > 0 {
		head.N = maxRequest + 1
		_ = conn.SetReadDeadline(time.Now().Add(wait))
		if _, derr := io.CopyN(io.Discard, r, contentLen); derr != nil {
			return true, false
		}
	}

	allowed := true
	retryAfter := 0
	if hasRate {
		allowed, retryAfter = s.rateLimiter().AllowBytes(idSlice, clientAddr, rpm, burst, s.now())
	}

	_ = conn.SetWriteDeadline(time.Now().Add(wait))
	defer func() { _ = conn.SetWriteDeadline(time.Time{}) }()
	bw.Reset(conn)
	if allowed {
		if closeConn {
			if _, werr := bw.Write(okEmptyCloseResp); werr != nil {
				return true, false
			}
		} else {
			if _, werr := bw.Write(okEmptyResp); werr != nil {
				return true, false
			}
		}
		return true, bw.Flush() == nil && !closeConn
	}

	var respBuf [192]byte
	b := respBuf[:0]
	b = append(b, "HTTP/1.1 429 Too Many Requests\r\nContent-Length: "...)
	b = strconv.AppendInt(b, int64(len(tooManyRequestsBody)), 10)
	b = append(b, "\r\nContent-Type: text/plain; charset=utf-8\r\nCache-Control: no-store\r\nRetry-After: "...)
	b = strconv.AppendInt(b, int64(max(1, retryAfter)), 10)
	if closeConn {
		b = append(b, "\r\nConnection: close\r\n\r\n"...)
	} else {
		b = append(b, "\r\n\r\n"...)
	}
	if !isHead {
		b = append(b, tooManyRequestsBody...)
	}
	if _, werr := bw.Write(b); werr != nil {
		return true, false
	}
	return true, bw.Flush() == nil && !closeConn
}

func parseRateQueryBytes(raw []byte) (rpm, burst uint32, ok bool) {
	for len(raw) > 0 {
		part := raw
		if i := bytes.IndexByte(raw, '&'); i >= 0 {
			part, raw = raw[:i], raw[i+1:]
		} else {
			raw = nil
		}
		eq := bytes.IndexByte(part, '=')
		if eq <= 0 {
			continue
		}
		k, v := part[:eq], part[eq+1:]
		switch string(k) {
		case "rpm":
			n, err := parseUintBytes(v)
			if err != nil || n == 0 || n > 0xffffffff {
				return 0, 0, false
			}
			rpm = uint32(n)
			ok = true
		case "burst":
			if n, err := parseUintBytes(v); err == nil && n <= 0xffffffff {
				burst = uint32(n)
			}
		}
	}
	return rpm, burst, ok
}

func parseUintBytes(b []byte) (uint64, error) {
	if len(b) == 0 || len(b) > 20 {
		return 0, strconv.ErrSyntax
	}
	var n uint64
	for _, c := range b {
		if c < '0' || c > '9' {
			return 0, strconv.ErrSyntax
		}
		n = n*10 + uint64(c-'0')
	}
	return n, nil
}

func parseRateClientBytes(b []byte) netip.Addr {
	b = trimASCIISpace(b)
	if len(b) == 0 || len(b) > 64 {
		return netip.Addr{}
	}
	a, err := netip.ParseAddr(string(b))
	if err != nil {
		return netip.Addr{}
	}
	a = a.Unmap()
	if a.Is6() {
		p, _ := a.WithZone("").Prefix(64)
		return p.Addr()
	}
	return a
}

func trimASCIISpace(b []byte) []byte {
	for len(b) > 0 && (b[0] == ' ' || b[0] == '\t') {
		b = b[1:]
	}
	for len(b) > 0 && (b[len(b)-1] == ' ' || b[len(b)-1] == '\t') {
		b = b[:len(b)-1]
	}
	return b
}

func equalFoldASCII(a, b []byte) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		ca, cb := a[i], b[i]
		if ca >= 'A' && ca <= 'Z' {
			ca += 'a' - 'A'
		}
		if cb >= 'A' && cb <= 'Z' {
			cb += 'a' - 'A'
		}
		if ca != cb {
			return false
		}
	}
	return true
}

func containsFoldASCII(haystack []byte, needle string) bool {
	if len(haystack) < len(needle) {
		return false
	}
	for i := 0; i <= len(haystack)-len(needle); i++ {
		match := true
		for j := range len(needle) {
			c := haystack[i+j]
			if c >= 'A' && c <= 'Z' {
				c += 'a' - 'A'
			}
			if c != needle[j] {
				match = false
				break
			}
		}
		if match {
			return true
		}
	}
	return false
}

// answer is what a handler writes, kept whole until it's sent, as the agent's
// answers are a few hundred bytes.
type answer struct {
	header http.Header
	status int
	body   bytes.Buffer
}

func (a *answer) reset() {
	clear(a.header)
	a.status = 0
	a.body.Reset()
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

var okEmptyResp = []byte("HTTP/1.1 200 OK\r\nContent-Length: 0\r\n\r\n")

// send writes the answer as HTTP/1.1, with its length, so the connection can carry
// the next request, within wait (L2).
func (a *answer) send(conn net.Conn, w *bufio.Writer, req *http.Request, wait time.Duration) error {
	a.WriteHeader(http.StatusOK)
	_ = conn.SetWriteDeadline(time.Now().Add(wait))
	defer func() { _ = conn.SetWriteDeadline(time.Time{}) }()
	w.Reset(conn)
	// Fast path for empty 200 OK with no extra headers (the common rate-limit allow response):
	// avoids allocating an http.Response and header formatting buffers on every request (M4).
	if a.status == http.StatusOK && a.body.Len() == 0 && len(a.header) == 0 && !req.Close {
		if _, err := w.Write(okEmptyResp); err != nil {
			return err
		}
		return w.Flush()
	}
	var scratch [32]byte
	_, _ = w.WriteString("HTTP/1.1 ")
	_, _ = w.Write(strconv.AppendInt(scratch[:0], int64(a.status), 10))
	_ = w.WriteByte(' ')
	if text := http.StatusText(a.status); text != "" {
		_, _ = w.WriteString(text)
	} else {
		_, _ = w.WriteString("status")
	}
	_, _ = w.WriteString("\r\nContent-Length: ")
	_, _ = w.Write(strconv.AppendInt(scratch[:0], int64(a.body.Len()), 10))
	_, _ = w.WriteString("\r\n")
	if req.Close {
		_, _ = w.WriteString("Connection: close\r\n")
	}
	for k, vals := range a.header {
		if k == "Content-Length" || (req.Close && k == "Connection") {
			continue
		}
		for _, v := range vals {
			_, _ = w.WriteString(k)
			_, _ = w.WriteString(": ")
			_, _ = w.WriteString(v)
			_, _ = w.WriteString("\r\n")
		}
	}
	_, _ = w.WriteString("\r\n")
	if req.Method != http.MethodHead && a.body.Len() > 0 {
		if _, err := w.Write(a.body.Bytes()); err != nil {
			return err
		}
	}
	return w.Flush()
}
