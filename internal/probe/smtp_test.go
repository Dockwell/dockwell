// SPDX-License-Identifier: AGPL-3.0-only

package probe

import (
	"encoding/base64"
	"net"
	"net/netip"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/dockwell/dockwell/internal/netguard"
	"github.com/dockwell/dockwell/internal/probe/smtptest"
)

// trusting lets the check trust the test server's certificate, for this test.
func trusting(t *testing.T, srv *smtptest.Server) {
	t.Helper()
	was := roots
	roots = srv.Roots
	t.Cleanup(func() { roots = was })
}

func smtpRequest(srv *smtptest.Server, security string) *Request {
	return &Request{
		Kind:  KindSMTP,
		Allow: here,
		SMTP:  &SMTP{Host: srv.Host, Port: srv.Port, Security: security, User: "notes@example.com", Password: "the-password"},
	}
}

// sent says whether the server was asked to send mail.
func sent(srv *smtptest.Server) bool {
	return slices.ContainsFunc(srv.Commands(), func(c string) bool { return c == "MAIL" || c == "RCPT" || c == "DATA" })
}

// A server that takes the user signs it in, over STARTTLS or TLS from the start,
// and is never asked to send.
func TestAMailServerThatSignsInPasses(t *testing.T) { //nolint:paralleltest // it sets the roots the check trusts
	for _, implicit := range []bool{false, true} {
		srv := smtptest.New(t, implicit)
		trusting(t, srv)
		security := SecurityStartTLS
		if implicit {
			security = SecurityTLS
		}
		got, err := Check(t.Context(), smtpRequest(srv, security))
		if err != nil {
			t.Fatal(err)
		}
		if !got.Passed || got.Step != "" || got.Code != "" {
			t.Errorf("%s checks as %+v", security, got)
		}
		if who := srv.SignedIn(); !slices.Equal(who, []string{"notes@example.com:the-password"}) {
			t.Errorf("%s signed in as %v", security, who)
		}
		if sent(srv) {
			t.Errorf("%s's check asked to send: %v", security, srv.Commands())
		}
		if c := srv.Commands(); c[len(c)-1] != "QUIT" {
			t.Errorf("%s's check ended with %v, want QUIT", security, c)
		}
	}
}

// A wrong password is its reply code and enhanced code, never the reply's words,
// which say the user back.
func TestAWrongPasswordSaysItsCodeAlone(t *testing.T) { //nolint:paralleltest // it sets the roots the check trusts
	srv := smtptest.New(t, false)
	srv.Set(func(s *smtptest.Server) { s.Users = map[string]string{"notes@example.com": "another"} })
	trusting(t, srv)
	got, err := Check(t.Context(), smtpRequest(srv, SecurityStartTLS))
	if err != nil {
		t.Fatal(err)
	}
	if got.Passed || got.Step != StepSignIn || got.Code != "535 5.7.8" || got.Message != "" || got.Words != "" {
		t.Errorf("a wrong password checks as %+v", got)
	}
}

// A server that offers no STARTTLS where it's asked for is said so, and nothing
// signs in over the plain connection.
func TestNoStartTLSSignsNothingIn(t *testing.T) {
	t.Parallel()
	srv := smtptest.New(t, false)
	srv.Set(func(s *smtptest.Server) { s.StartTLS, s.Plain = false, true })
	got, err := Check(t.Context(), smtpRequest(srv, SecurityStartTLS))
	if err != nil {
		t.Fatal(err)
	}
	if got.Passed || got.Step != StepTLS || got.Code != CodeNoStartTLS {
		t.Errorf("a server without STARTTLS checks as %+v", got)
	}
	if slices.Contains(srv.Commands(), "AUTH") {
		t.Errorf("the check signed in without TLS: %v", srv.Commands())
	}
}

// Without TLS, a password goes to a private address alone, as one on the app's
// own network: this test's server, on loopback, takes it.
func TestNoTLSSignsInOnlyNearby(t *testing.T) {
	t.Parallel()
	srv := smtptest.New(t, false)
	srv.Set(func(s *smtptest.Server) { s.StartTLS, s.Plain = false, true })
	got, err := Check(t.Context(), smtpRequest(srv, SecurityNone))
	if err != nil {
		t.Fatal(err)
	}
	if !got.Passed || len(srv.SignedIn()) != 1 {
		t.Errorf("a server on the app's network without TLS checks as %+v, signed in %v", got, srv.SignedIn())
	}
	for addr, want := range map[string]bool{
		"127.0.0.1:25": true, "10.0.0.5:25": true, "192.168.1.2:587": true, "100.64.1.1:25": true, "[fd00::1]:25": true,
		"203.0.113.5:25": false, "[2001:db8::1]:25": false, "198.51.100.7:587": false,
	} {
		remote, err := net.ResolveTCPAddr("tcp", addr)
		if err != nil {
			t.Fatal(err)
		}
		if got := nearby(remote, netguard.Options{}); got != want {
			t.Errorf("%s is nearby: %v, want %v", addr, got, want)
		}
	}
	// One of the app's own networks is near, though public.
	remote, _ := net.ResolveTCPAddr("tcp", "203.0.113.5:25")
	if !nearby(remote, netguard.Options{Allow: []netip.Prefix{netip.MustParsePrefix("203.0.113.0/24")}}) {
		t.Error("an address on the app's own network isn't nearby")
	}
}

// A server that signs no one in, or only in ways Dockwell doesn't, is said so.
func TestASignInTheServerDoesntOfferIsSaid(t *testing.T) { //nolint:paralleltest // it sets the roots the check trusts
	srv := smtptest.New(t, true)
	trusting(t, srv)
	srv.Set(func(s *smtptest.Server) { s.Mechs = "GSSAPI NTLM" })
	got, err := Check(t.Context(), smtpRequest(srv, SecurityTLS))
	if err != nil {
		t.Fatal(err)
	}
	if got.Passed || got.Step != StepSignIn || got.Code != CodeMechanisms {
		t.Errorf("a server signing in with GSSAPI alone checks as %+v", got)
	}
	// With no user, nothing signs in, and it passes.
	req := smtpRequest(srv, SecurityTLS)
	req.SMTP.User, req.SMTP.Password = "", ""
	if got, _ := Check(t.Context(), req); !got.Passed {
		t.Errorf("a server with no user checks as %+v", got)
	}
}

// What the app's networks don't take is refused before anything connects; a port
// nothing listens on is unreachable, at connect, which the panel offers another
// port for.
func TestAMailServerOutOfReachStopsAtConnect(t *testing.T) {
	t.Parallel()
	srv := smtptest.New(t, false)
	req := smtpRequest(srv, SecurityStartTLS)
	req.Allow = nil
	got, err := Check(t.Context(), req)
	if err != nil {
		t.Fatal(err)
	}
	if got.Step != StepConnect || got.Code != CodeRefused || len(srv.Commands()) != 0 {
		t.Errorf("an address the app can't reach checks as %+v, sent %v", got, srv.Commands())
	}
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	closed := ln.Addr().(*net.TCPAddr).Port
	_ = ln.Close()
	req = smtpRequest(srv, SecurityStartTLS)
	req.SMTP.Port = closed
	if got, _ := Check(t.Context(), req); got.Step != StepConnect || got.Code != CodeUnreachable {
		t.Errorf("a port nothing listens on checks as %+v", got)
	}
}

// A request the check can't be asked runs nothing.
func TestAMailServerRequestMustBeWhole(t *testing.T) {
	t.Parallel()
	for name, c := range map[string]*SMTP{
		"no server":         nil,
		"no host":           {Port: 587, Security: SecurityStartTLS},
		"a host with a URL": {Host: "smtp://smtp.example.com", Port: 587, Security: SecurityStartTLS},
		"no port":           {Host: "smtp.example.com", Security: SecurityStartTLS},
		"no security":       {Host: "smtp.example.com", Port: 587},
		"a password alone":  {Host: "smtp.example.com", Port: 587, Security: SecurityTLS, Password: "x"},
	} {
		if _, err := Check(t.Context(), &Request{Kind: KindSMTP, SMTP: c}); err == nil {
			t.Errorf("%s ran", name)
		}
	}
	if (&SMTP{Host: "2001:db8::25", Port: 25, Security: SecurityNone}).valid() != nil {
		t.Error("an IPv6 address isn't a host")
	}
}

// What the probe prints keeps no form of the user or the password, AUTH PLAIN's
// among them.
func TestTheLogHoldsNoFormOfAMailPassword(t *testing.T) {
	t.Parallel()
	req := Request{Kind: KindSMTP, SMTP: &SMTP{User: "notes@example.com", Password: "the-password"}}
	plain := base64.StdEncoding.EncodeToString([]byte("\x00notes@example.com\x00the-password"))
	said := withoutKeys("AUTH PLAIN "+plain+" then the-password and notes@example.com", req)
	for _, leak := range []string{"the-password", "notes@example.com", plain} {
		if strings.Contains(said, leak) {
			t.Errorf("the log keeps %q: %s", leak, said)
		}
	}
}

// A server that takes the connection and never finishes TLS is held to the
// handshake's bound, not the whole check's (the review's L1).
func TestAHandshakeIsBounded(t *testing.T) { //nolint:paralleltest // it sets the handshake's bound
	was := handshakeWait
	handshakeWait = 200 * time.Millisecond
	t.Cleanup(func() { handshakeWait = was })
	ln, err := (&net.ListenConfig{}).Listen(t.Context(), "tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = ln.Close() })
	held := make(chan net.Conn, 2)
	go func() {
		// It takes the connection and never answers.
		if conn, err := ln.Accept(); err == nil {
			held <- conn
		}
	}()
	t.Cleanup(func() {
		for {
			select {
			case conn := <-held:
				_ = conn.Close()
			default:
				return
			}
		}
	})
	port := ln.Addr().(*net.TCPAddr).Port
	start := time.Now()
	got, err := Check(t.Context(), &Request{Kind: KindSMTP, Allow: here, SMTP: &SMTP{Host: "127.0.0.1", Port: port, Security: SecurityTLS}})
	if err != nil {
		t.Fatal(err)
	}
	if got.Step != StepTLS || got.Code != CodeTimeout || time.Since(start) > 5*time.Second {
		t.Errorf("a silent TLS server checks as %+v after %s", got, time.Since(start))
	}
	// A plain start to it waits for a greeting no longer: it's said at hello.
	go func() {
		if conn, err := ln.Accept(); err == nil {
			held <- conn
		}
	}()
	start = time.Now()
	got, _ = Check(t.Context(), &Request{Kind: KindSMTP, Allow: here, SMTP: &SMTP{Host: "127.0.0.1", Port: port, Security: SecurityStartTLS}})
	if got.Step != StepHello || got.Code != CodeTimeout || time.Since(start) > 5*time.Second {
		t.Errorf("a silent server, started plainly, checks as %+v after %s", got, time.Since(start))
	}
}
