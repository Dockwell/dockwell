// SPDX-License-Identifier: AGPL-3.0-only

package probe

import (
	"bufio"
	"context"
	"encoding/base64"
	"net"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/dockwell/dockwell/internal/netguard"
)

// hostile is a mail server on loopback that answers each command as answer says,
// and records every command it's sent.
type hostile struct {
	addr     *net.TCPAddr
	mu       sync.Mutex
	commands []string
}

func newHostile(t *testing.T, greet string, answer func(line string) string) *hostile {
	t.Helper()
	ln, err := (&net.ListenConfig{}).Listen(t.Context(), "tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = ln.Close() })
	h := &hostile{addr: ln.Addr().(*net.TCPAddr)}
	go func() {
		for {
			conn, err := ln.Accept()
			if err != nil {
				return
			}
			go func() {
				defer conn.Close()
				if greet == "" {
					time.Sleep(time.Hour)
					return
				}
				_, _ = conn.Write([]byte(greet + "\r\n"))
				r := bufio.NewReader(conn)
				for {
					line, err := r.ReadString('\n')
					if err != nil {
						return
					}
					line = strings.TrimRight(line, "\r\n")
					h.mu.Lock()
					h.commands = append(h.commands, line)
					h.mu.Unlock()
					out := answer(line)
					if out == "" {
						return
					}
					_, _ = conn.Write([]byte(out + "\r\n"))
				}
			}()
		}
	}()
	return h
}

func (h *hostile) sent() []string {
	h.mu.Lock()
	defer h.mu.Unlock()
	return append([]string(nil), h.commands...)
}

const reviewPassword = "review-pass-8d1f"

// A server that takes AUTH and then says 250 to anything is never asked to send.
func TestReviewSMTPA250ForAnythingSendsNothing(t *testing.T) {
	t.Parallel()
	h := newHostile(t, "220 hi", func(line string) string {
		switch {
		case strings.HasPrefix(line, "EHLO"):
			return "250-hi\r\n250 AUTH PLAIN LOGIN"
		case strings.HasPrefix(line, "AUTH"):
			return "235 2.7.0 ok"
		case line == "QUIT":
			return "221 bye"
		}
		return "250 ok"
	})
	ctx, cancel := context.WithTimeout(t.Context(), CheckWait)
	defer cancel()
	res := checkSMTP(ctx, &SMTP{Host: "127.0.0.1", Port: h.addr.Port, Security: SecurityNone, User: "u", Password: reviewPassword}, netguard.Options{Allow: here})
	if !res.Passed {
		t.Fatalf("it didn't pass: %+v", res)
	}
	for _, c := range h.sent() {
		up := strings.ToUpper(c)
		if strings.HasPrefix(up, "MAIL") || strings.HasPrefix(up, "RCPT") || strings.HasPrefix(up, "DATA") {
			t.Errorf("the check sent %q", c)
		}
	}
}

// A malformed reply that says the sign-in back never reaches the check's answer.
func TestReviewSMTPAMalformedReplyQuotesNothing(t *testing.T) {
	t.Parallel()
	h := newHostile(t, "220 hi", func(line string) string {
		switch {
		case strings.HasPrefix(line, "EHLO"):
			return "250-hi\r\n250 AUTH PLAIN"
		case strings.HasPrefix(line, "AUTH PLAIN "):
			raw, _ := base64.StdEncoding.DecodeString(strings.TrimPrefix(line, "AUTH PLAIN "))
			return "XYZ you said " + strings.ReplaceAll(string(raw), "\x00", " ")
		}
		return "250 ok"
	})
	ctx, cancel := context.WithTimeout(t.Context(), CheckWait)
	defer cancel()
	res := checkSMTP(ctx, &SMTP{Host: "127.0.0.1", Port: h.addr.Port, Security: SecurityNone, User: "the-user", Password: reviewPassword}, netguard.Options{Allow: here})
	if s := res.Step + res.Code + res.Words; strings.Contains(s, reviewPassword) || strings.Contains(s, "the-user") {
		t.Errorf("the answer quotes the sign-in: %+v", res)
	}
}

// A server that drops STARTTLS from its offer is refused, and signs nothing in.
func TestReviewSMTPStrippedStartTLSIsRefused(t *testing.T) {
	t.Parallel()
	h := newHostile(t, "220 hi", func(line string) string {
		if strings.HasPrefix(line, "EHLO") {
			return "250-hi\r\n250 AUTH PLAIN LOGIN"
		}
		return "250 ok"
	})
	ctx, cancel := context.WithTimeout(t.Context(), CheckWait)
	defer cancel()
	res := checkSMTP(ctx, &SMTP{Host: "127.0.0.1", Port: h.addr.Port, Security: SecurityStartTLS, User: "u", Password: reviewPassword}, netguard.Options{Allow: here})
	if res.Code != CodeNoStartTLS {
		t.Errorf("it's %+v", res)
	}
	for _, c := range h.sent() {
		if strings.HasPrefix(strings.ToUpper(c), "AUTH") {
			t.Errorf("it signed in in the clear: %q", c)
		}
	}
}

// A server that accepts and never greets holds the check no longer than its bound.
func TestReviewSMTPASilentServerIsBounded(t *testing.T) {
	t.Parallel()
	h := newHostile(t, "", nil)
	ctx, cancel := context.WithTimeout(t.Context(), 2*time.Second)
	defer cancel()
	start := time.Now()
	res := checkSMTP(ctx, &SMTP{Host: "127.0.0.1", Port: h.addr.Port, Security: SecurityTLS}, netguard.Options{Allow: here})
	if took := time.Since(start); took > 4*time.Second {
		t.Errorf("it took %v", took)
	}
	if res.Code != CodeTimeout {
		t.Errorf("it's %+v", res)
	}
}
