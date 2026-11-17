// SPDX-License-Identifier: AGPL-3.0-only

// Package smtpauth is how Dockwell signs in to a mail server: PLAIN, or LOGIN, as
// Exchange offers alone. Alert email sends through it from the control plane, and
// a connection's check signs in through it from where an app is (ADR 0099), so
// both pick the same way and keep a password to the server they named.
package smtpauth

import (
	"errors"
	"net/smtp"
	"slices"
	"strings"
)

// Pick is how to sign in to the server c talks to, as user with password: PLAIN
// where it's offered, then LOGIN. offered says whether the server signs anyone in
// at all, and mechs what it offers, for the caller to say why nothing fits. inClear
// lets the password go over a connection without TLS, which only a caller that
// knows the server is near may ask.
func Pick(c *smtp.Client, user, password, host string, inClear bool) (auth smtp.Auth, offered bool, mechs []string) {
	ok, params := c.Extension("AUTH")
	if !ok {
		return nil, false, nil
	}
	mechs = strings.Fields(strings.ToUpper(params))
	switch {
	case slices.Contains(mechs, "PLAIN"):
		if inClear {
			return &plain{user: user, password: password, host: host}, true, mechs
		}
		return smtp.PlainAuth("", user, password, host), true, mechs
	case slices.Contains(mechs, "LOGIN"):
		return &login{user: user, password: password, host: host, clear: inClear}, true, mechs
	}
	return nil, true, mechs
}

// ErrClear refuses to send a password over a connection without TLS.
var ErrClear = errors.New("dockwell sends a password only over TLS")

// errElsewhere refuses a server other than the one named.
var errElsewhere = errors.New("the server isn't the one named")

// plain signs in with AUTH PLAIN where TLS may be left out, to the server named
// alone. With TLS, smtp.PlainAuth does the same.
type plain struct {
	user, password, host string
}

func (a *plain) Start(server *smtp.ServerInfo) (string, []byte, error) {
	if server.Name != a.host {
		return "", nil, errElsewhere
	}
	return "PLAIN", []byte("\x00" + a.user + "\x00" + a.password), nil
}

func (a *plain) Next(_ []byte, more bool) ([]byte, error) {
	if more {
		return nil, errors.New("the server asked for more than a user and a password")
	}
	return nil, nil
}

// login signs in with AUTH LOGIN: the user, then the password, each as the server
// asks, only over TLS unless clear, to the server named, as PlainAuth does.
type login struct {
	user, password, host string
	clear                bool
	step                 int
}

func (a *login) Start(server *smtp.ServerInfo) (string, []byte, error) {
	if !server.TLS && !a.clear {
		return "", nil, ErrClear
	}
	if server.Name != a.host {
		return "", nil, errElsewhere
	}
	a.step = 0
	return "LOGIN", nil, nil
}

func (a *login) Next(_ []byte, more bool) ([]byte, error) {
	if !more {
		return nil, nil
	}
	a.step++
	switch a.step {
	case 1:
		return []byte(a.user), nil
	case 2:
		return []byte(a.password), nil
	}
	return nil, errors.New("the server asked for more than a user and a password")
}
