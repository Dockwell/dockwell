// SPDX-License-Identifier: AGPL-3.0-only

// Package smtptest is a mail server for tests: STARTTLS or TLS from the start,
// AUTH PLAIN and LOGIN, and every command it's sent, so a test can say a check
// signed in and sent nothing.
package smtptest

import (
	"bufio"
	"context"
	"crypto/tls"
	"crypto/x509"
	"encoding/base64"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
)

// Server is a mail server on this machine's loopback.
type Server struct {
	// Addr is where it listens, as host:port, and Host and Port the same apart.
	Addr string
	Host string
	Port int
	// Roots trust its certificate, which is for 127.0.0.1.
	Roots *x509.CertPool

	tls      *tls.Config
	implicit bool

	mu sync.Mutex
	// StartTLS offers STARTTLS on a plain connection. Mechs is what it signs in
	// with once secure, "PLAIN LOGIN" unless set, and Plain offers them on a plain
	// connection too. Refuse answers AUTH where set, as a wrong password's reply.
	StartTLS bool
	Mechs    string
	Plain    bool
	Refuse   string
	// Users and passwords it takes, where any is set; any is taken otherwise.
	Users map[string]string

	commands []string
	signedIn []string
}

// New is a server that speaks TLS from the start where implicit, and offers
// STARTTLS otherwise.
func New(t testing.TB, implicit bool) *Server {
	t.Helper()
	certs := httptest.NewTLSServer(http.NotFoundHandler())
	conf := &tls.Config{Certificates: certs.TLS.Certificates, MinVersion: tls.VersionTLS12}
	roots := certs.Client().Transport.(*http.Transport).TLSClientConfig.RootCAs
	certs.Close()
	var ln net.Listener
	var err error
	if implicit {
		ln, err = tls.Listen("tcp", "127.0.0.1:0", conf)
	} else {
		ln, err = (&net.ListenConfig{}).Listen(context.Background(), "tcp", "127.0.0.1:0")
	}
	if err != nil {
		t.Fatal(err)
	}
	a := ln.Addr().(*net.TCPAddr)
	s := &Server{Addr: a.String(), Host: "127.0.0.1", Port: a.Port, Roots: roots, tls: conf, implicit: implicit, StartTLS: !implicit}
	t.Cleanup(func() { _ = ln.Close() })
	go func() {
		for {
			conn, err := ln.Accept()
			if err != nil {
				return
			}
			go s.serve(conn)
		}
	}()
	return s
}

// Set changes how it answers, under its lock.
func (s *Server) Set(f func(*Server)) {
	s.mu.Lock()
	defer s.mu.Unlock()
	f(s)
}

// Commands are the verbs it was sent, in order, such as EHLO, STARTTLS and AUTH.
func (s *Server) Commands() []string {
	s.mu.Lock()
	defer s.mu.Unlock()
	return append([]string(nil), s.commands...)
}

// SignedIn are who signed in, as user:password.
func (s *Server) SignedIn() []string {
	s.mu.Lock()
	defer s.mu.Unlock()
	return append([]string(nil), s.signedIn...)
}

func (s *Server) serve(conn net.Conn) {
	defer func() { _ = conn.Close() }()
	secure := s.implicit
	r, w := bufio.NewReader(conn), io.Writer(conn)
	say := func(line string) { _, _ = io.WriteString(w, line+"\r\n") }
	say("220 smtp.test ready")
	for {
		line, err := r.ReadString('\n')
		if err != nil {
			return
		}
		cmd := strings.TrimRight(line, "\r\n")
		verb := strings.ToUpper(strings.SplitN(cmd, " ", 2)[0])
		s.mu.Lock()
		s.commands = append(s.commands, verb)
		starttls, mechs, plain, refuse := s.StartTLS, s.Mechs, s.Plain, s.Refuse
		s.mu.Unlock()
		if mechs == "" {
			mechs = "PLAIN LOGIN"
		}
		switch verb {
		case "EHLO", "HELO":
			lines := []string{"smtp.test"}
			if !secure && starttls {
				lines = append(lines, "STARTTLS")
			}
			if secure || plain {
				lines = append(lines, "AUTH "+mechs)
			}
			for i, l := range lines {
				sep := "-"
				if i == len(lines)-1 {
					sep = " "
				}
				say("250" + sep + l)
			}
		case "STARTTLS":
			if secure || !starttls {
				say("502 5.5.1 not here")
				continue
			}
			say("220 2.0.0 go ahead")
			tc := tls.Server(conn, s.tls)
			if tc.HandshakeContext(context.Background()) != nil {
				return
			}
			conn, r, w, secure = tc, bufio.NewReader(tc), tc, true
		case "AUTH":
			var user, password string
			switch {
			case strings.HasPrefix(cmd, "AUTH PLAIN ") && strings.Contains(mechs, "PLAIN"):
				raw, _ := base64.StdEncoding.DecodeString(strings.TrimPrefix(cmd, "AUTH PLAIN "))
				parts := strings.SplitN(string(raw), "\x00", 3)
				if len(parts) == 3 {
					user, password = parts[1], parts[2]
				}
			case cmd == "AUTH LOGIN" && strings.Contains(mechs, "LOGIN"):
				var got []string
				for _, ask := range []string{"VXNlcm5hbWU6", "UGFzc3dvcmQ6"} {
					say("334 " + ask)
					l, err := r.ReadString('\n')
					if err != nil {
						return
					}
					raw, _ := base64.StdEncoding.DecodeString(strings.TrimRight(l, "\r\n"))
					got = append(got, string(raw))
				}
				user, password = got[0], got[1]
			default:
				say("504 5.7.4 Unrecognized authentication type")
				continue
			}
			s.mu.Lock()
			want, known := s.Users[user]
			takes := refuse == "" && (len(s.Users) == 0 || known && want == password)
			if takes {
				s.signedIn = append(s.signedIn, user+":"+password)
			}
			s.mu.Unlock()
			switch {
			case takes:
				say("235 2.7.0 Authentication successful")
			case refuse != "":
				say(refuse)
			default:
				say("535 5.7.8 Username and Password not accepted for " + user)
			}
		case "QUIT":
			say("221 2.0.0 bye")
			return
		default:
			say("250 ok")
		}
	}
}
