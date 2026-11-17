// SPDX-License-Identifier: AGPL-3.0-only

package probe

import (
	"context"
	"crypto/tls"
	"crypto/x509"
	"errors"
	"fmt"
	"net"
	"net/netip"
	"net/smtp"
	"net/textproto"
	"regexp"
	"strconv"
	"strings"
	"time"

	"github.com/dockwell/dockwell/internal/netguard"
	"github.com/dockwell/dockwell/internal/smtpauth"
)

// SMTP is a mail server to check, and who signs in to it. The check signs in and
// sends nothing (ADR 0099).
type SMTP struct {
	Host string `json:"host"`
	Port int    `json:"port"`
	// Security is starttls, tls or none.
	Security string `json:"security"`
	// User signs in with Password; empty signs in as no one.
	User     string `json:"user,omitempty"`
	Password string `json:"password,omitempty"`
}

// How a mail server's connection is secured, as SMTP.Security says.
const (
	SecurityStartTLS = "starttls"
	SecurityTLS      = "tls"
	SecurityNone     = "none"
)

// The steps an SMTP check takes, in order.
const (
	StepConnect = "connect"
	StepHello   = "hello"
	StepTLS     = "tls"
	StepSignIn  = "sign-in"
)

// What kept an SMTP check from going on, where the server gave no reply code.
const (
	// CodeNoStartTLS is a server that doesn't offer STARTTLS, where it's asked for.
	CodeNoStartTLS = "no-starttls"
	// CodeNoSignIn is a server that offers to sign no one in, where a user's set.
	CodeNoSignIn = "no-sign-in"
	// CodeMechanisms is a server that signs in only in ways Dockwell doesn't.
	CodeMechanisms = "mechanisms"
	// CodeClear is a password the check wouldn't send without TLS, to an address
	// that isn't private.
	CodeClear = "clear"
)

// handshakeWait bounds a TLS handshake, from the start or after STARTTLS, as a
// connection is bounded.
var handshakeWait = netguard.DialTimeout

// helloName is what the check says it is, as a client with no name of its own.
const helloName = "localhost"

// enhanced is an enhanced status code at the start of a reply, such as 5.7.8.
var enhanced = regexp.MustCompile(`^[245]\.\d{1,3}\.\d{1,3}\b`)

// roots are the CAs a check trusts, the system's unless a test sets them.
var roots *x509.CertPool

// checkSMTP connects, says EHLO, secures the connection as set, and signs in where
// a user is set, then quits: it never says MAIL, RCPT or DATA. A password goes only
// over TLS, or to a private address, as one on the app's own network.
func checkSMTP(ctx context.Context, c *SMTP, guard netguard.Options) Result {
	addr := net.JoinHostPort(c.Host, strconv.Itoa(c.Port))
	conn, err := netguard.Dialer(guard)(ctx, "tcp", addr)
	if err != nil {
		return Result{Step: StepConnect, Code: connectCode(err), Words: netguard.Words(err)}
	}
	defer conn.Close()
	raw := conn
	deadline, bounded := ctx.Deadline()
	if bounded {
		_ = raw.SetDeadline(deadline)
	}
	near := nearby(conn.RemoteAddr(), guard)
	config := &tls.Config{ServerName: c.Host, MinVersion: tls.VersionTLS12, RootCAs: roots}
	if c.Security == SecurityTLS {
		// A handshake is held to the bound a connection is, not the whole check's
		// (the SMTP review's L1).
		hctx, cancel := context.WithTimeout(ctx, handshakeWait)
		tc := tls.Client(conn, config)
		err := tc.HandshakeContext(hctx)
		cancel()
		if err != nil {
			return Result{Step: StepTLS, Code: connectCode(err), Words: netguard.Words(err)}
		}
		conn = tc
	}
	// The greeting and EHLO's answer are held to that bound too: a port that takes the
	// connection and says nothing, as a TLS-from-the-start one does to a plain start,
	// is said so in seconds, not the whole check's minute.
	restore := func() {
		if bounded {
			_ = raw.SetDeadline(deadline)
		} else {
			_ = raw.SetDeadline(time.Time{})
		}
	}
	_ = raw.SetDeadline(time.Now().Add(handshakeWait))
	client, err := smtp.NewClient(conn, c.Host)
	if err != nil {
		return replied(StepHello, err)
	}
	defer client.Close()
	if err := client.Hello(helloName); err != nil {
		return replied(StepHello, err)
	}
	restore()
	if c.Security == SecurityStartTLS {
		if ok, _ := client.Extension("STARTTLS"); !ok {
			return Result{Step: StepTLS, Code: CodeNoStartTLS}
		}
		_ = raw.SetDeadline(time.Now().Add(handshakeWait))
		err := client.StartTLS(config)
		restore()
		if err != nil {
			return replied(StepTLS, err)
		}
	}
	if c.User != "" {
		_, secured := client.TLSConnectionState()
		if !secured && !near {
			return Result{Step: StepSignIn, Code: CodeClear}
		}
		auth, offered, _ := smtpauth.Pick(client, c.User, c.Password, c.Host, !secured)
		switch {
		case !offered:
			return Result{Step: StepSignIn, Code: CodeNoSignIn}
		case auth == nil:
			return Result{Step: StepSignIn, Code: CodeMechanisms}
		}
		if err := client.Auth(auth); err != nil {
			return replied(StepSignIn, err)
		}
	}
	_ = client.Quit()
	return Result{Passed: true}
}

// valid refuses a mail server the check can't be asked of.
func (c *SMTP) valid() error {
	switch {
	case c == nil:
		return errors.New("the probe was asked of a mail server with none")
	case c.Host == "" || strings.ContainsAny(c.Host, " /:@\r\n") && !isAddr(c.Host):
		return errors.New("the probe was asked of a mail server with no host to connect to")
	case c.Port < 1 || c.Port > 65535:
		return errors.New("the probe was asked of a mail server on no port")
	case c.Security != SecurityStartTLS && c.Security != SecurityTLS && c.Security != SecurityNone:
		return fmt.Errorf("the probe doesn't secure a mail server's connection as %q", c.Security)
	case c.User == "" && c.Password != "":
		return errors.New("the probe was asked to sign in to a mail server with a password and no user")
	}
	return nil
}

func isAddr(host string) bool {
	_, err := netip.ParseAddr(host)
	return err == nil
}

// replied is a Result that stopped at step for err: the server's reply code and
// its enhanced code, never its words, which may say the user back; or what kept
// it from answering.
func replied(step string, err error) Result {
	var tp *textproto.Error
	if errors.As(err, &tp) {
		code := strconv.Itoa(tp.Code)
		if e := enhanced.FindString(strings.TrimSpace(tp.Msg)); e != "" {
			code += " " + e
		}
		return Result{Step: step, Code: code}
	}
	return Result{Step: step, Code: connectCode(err), Words: netguard.Words(err)}
}

// cgnat is shared address space, which netguard takes for private too.
var cgnat = netip.MustParsePrefix("100.64.0.0/10")

// nearby says the check reached a private address, or one on the app's own
// networks, where a password may go without TLS, as the app's would.
func nearby(remote net.Addr, guard netguard.Options) bool {
	ap, err := netip.ParseAddrPort(remote.String())
	if err != nil {
		return false
	}
	addr := ap.Addr().Unmap()
	if addr.IsPrivate() || addr.IsLoopback() || cgnat.Contains(addr) {
		return true
	}
	for _, p := range guard.Allow {
		if p.Contains(addr) {
			return true
		}
	}
	return false
}
