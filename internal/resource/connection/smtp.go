// SPDX-License-Identifier: AGPL-3.0-only

package connection

import (
	"errors"
	"fmt"
	"net/mail"
	"net/netip"
	"net/url"
	"regexp"
	"strconv"
	"strings"

	resourcev1 "github.com/dockwell/dockwell/gen/dockwell/resource/v1alpha1"
	"github.com/dockwell/dockwell/internal/resource"
)

// The ports a mail server's connection takes unless it says, by its security.
const (
	PortStartTLS = 587
	PortTLS      = 465
	PortNone     = 25
)

// hostName is a mail server's name: letters, digits, dots and dashes.
var hostName = regexp.MustCompile(`^[A-Za-z0-9]([A-Za-z0-9.-]{0,251}[A-Za-z0-9])?$`)

// cgnat is shared address space, which netguard takes for private too.
var cgnat = netip.MustParsePrefix("100.64.0.0/10")

// maxLine bounds a user, a password and a sender's name: one line, of what a mail
// server takes.
const maxLine = 256

// Security is a mail server's security, STARTTLS unless it says.
func Security(m *resourcev1.ConnectionSmtp) resourcev1.SmtpSecurity {
	if m.GetSecurity() == resourcev1.SmtpSecurity_SMTP_SECURITY_UNSPECIFIED {
		return resourcev1.SmtpSecurity_SMTP_SECURITY_STARTTLS
	}
	return m.GetSecurity()
}

// SecurityWord is how a need's words name a security: starttls, tls or none.
func SecurityWord(s resourcev1.SmtpSecurity) string {
	switch s {
	case resourcev1.SmtpSecurity_SMTP_SECURITY_TLS:
		return "tls"
	case resourcev1.SmtpSecurity_SMTP_SECURITY_NONE:
		return "none"
	default:
		return "starttls"
	}
}

// Port is the port a mail server's connection connects to: its own, or its
// security's.
func Port(m *resourcev1.ConnectionSmtp) int {
	if m.GetPort() != 0 {
		return int(m.GetPort())
	}
	switch Security(m) {
	case resourcev1.SmtpSecurity_SMTP_SECURITY_TLS:
		return PortTLS
	case resourcev1.SmtpSecurity_SMTP_SECURITY_NONE:
		return PortNone
	default:
		return PortStartTLS
	}
}

// PrivateHost says a host is an address on a private network, loopback or shared address
// space, judged by its range alone. A name says nothing of where it leads: Docker's
// DNS may hand even a name of one label to the server's own resolvers, so a name is
// never taken for near (the SMTP review's H1 and L4). Only the check, which reads the
// address it reached, takes the app's own networks too.
func PrivateHost(host string) bool {
	addr, err := netip.ParseAddr(strings.Trim(host, "[]"))
	if err != nil {
		return false
	}
	addr = addr.Unmap()
	return addr.IsLoopback() || addr.IsPrivate() || cgnat.Contains(addr)
}

// checkSMTP refuses a host that isn't a name or an address, a port out of range, a
// user or a sender's name over more than one line, a sender that isn't an address,
// and a password sent without TLS to a server that isn't near. It sets the security
// and the port where they're left to their defaults.
func checkSMTP(m *resourcev1.ConnectionSmtp) error {
	m.Host = strings.TrimSuffix(strings.TrimSpace(m.GetHost()), ".")
	switch {
	case m.GetHost() == "":
		return errors.New("a connection to a mail server needs its host, such as smtp.example.com")
	case !hostName.MatchString(m.GetHost()) && !isAddr(m.GetHost()):
		return errors.New("a mail server's host is its name or address alone, such as smtp.example.com, with its port apart")
	case resourcev1.SmtpSecurity_name[int32(m.GetSecurity())] == "":
		return errors.New("a mail server's security is SMTP_SECURITY_STARTTLS, SMTP_SECURITY_TLS or SMTP_SECURITY_NONE")
	case m.GetPort() > 65535:
		return errors.New("a mail server's port is from 1 to 65535, such as 587")
	case strings.ContainsAny(m.GetUser(), "\r\n") || len(m.GetUser()) > maxLine:
		return fmt.Errorf("a mail server's user is one line, at most %d characters", maxLine)
	case strings.ContainsAny(m.GetPassword(), "\r\n") || len(m.GetPassword()) > maxLine:
		return fmt.Errorf("a mail server's password is one line, at most %d characters", maxLine)
	case m.GetUser() == "" && m.GetPassword() != "":
		return errors.New("a mail server's password needs the user it signs in as")
	}
	if err := CheckSenderName(m.GetSenderName()); err != nil {
		return err
	}
	m.Security = Security(m)
	m.Port = uint32(Port(m)) //nolint:gosec // from 1 to 65535
	m.Sender = strings.TrimSpace(m.GetSender())
	m.SenderName = strings.TrimSpace(m.GetSenderName())
	if err := CheckSender(m.GetSender()); err != nil {
		return err
	}
	if m.GetSecurity() == resourcev1.SmtpSecurity_SMTP_SECURITY_NONE && m.GetUser() != "" && !PrivateHost(m.GetHost()) {
		return fmt.Errorf("a connection to %s signs in only over STARTTLS or TLS, as the password could cross the internet where anyone between could read it. Without TLS, give the server's private address, such as 10.0.0.5, or use STARTTLS or TLS", m.GetHost())
	}
	return nil
}

// CheckSender refuses a sender that isn't an address alone, such as
// notes@example.com. Empty is none.
func CheckSender(sender string) error {
	if sender == "" {
		return nil
	}
	a, err := mail.ParseAddress(sender)
	if err != nil || a.Name != "" || a.Address != sender {
		return fmt.Errorf("%q isn't an email address alone, such as notes@example.com. Its name goes apart", sender)
	}
	return nil
}

// CheckSenderName refuses a sender's name over more than one line.
func CheckSenderName(name string) error {
	if strings.ContainsAny(name, "\r\n\"<>") || len([]rune(name)) > maxLine {
		return errors.New("who the mail comes from is a name on one line, with no quotes or angle brackets, such as Notes")
	}
	return nil
}

func isAddr(host string) bool {
	_, err := netip.ParseAddr(host)
	return err == nil
}

// keepSMTP holds on to who a mail server's connection signs in as when an update
// leaves the user and the password out, while where the password goes stays: the
// same host, port and security. A new password comes with its user, and a user with
// its password, whether it's the saved one or not, so no edit tells a right guess at
// the user from a wrong one.
func keepSMTP(from, to *resourcev1.ConnectionSmtp) (resource.Kept, error) {
	switch {
	case to.GetPassword() != "" && to.GetUser() == "":
		return resource.Kept{}, errors.New("a new password needs the user it signs in as too. Give both, even when the user stays the same")
	case to.GetPassword() != "":
		return resource.Kept{}, nil
	case to.GetUser() != "":
		return resource.Kept{}, errors.New("give a new user and its password together, or leave both out to keep the saved sign-in")
	case from.GetUser() == "":
		return resource.Kept{}, nil
	case movesServer(from, to):
		return resource.Kept{}, errors.New("the connection moves to another host, port or security, so it needs its password again: Dockwell won't send what it kept to where it hasn't sent it before")
	}
	to.User, to.Password = from.GetUser(), from.GetPassword()
	return resource.Kept{Reason: "kept_key", Words: "keeps the sign-in it was saved with, as the update left that out"}, nil
}

// movesServer says whether an edit sends a mail server's connection somewhere else,
// or less safely: another host or port, or another security.
func movesServer(from, to *resourcev1.ConnectionSmtp) bool {
	if from == nil || to == nil {
		return from != to
	}
	return to.GetHost() != from.GetHost() || Port(to) != Port(from) || Security(to) != Security(from)
}

// smtpPart is one of a mail server's values, by the part a link sets.
func smtpPart(m *resourcev1.ConnectionSmtp, part resourcev1.LinkPart) (value string, secret, ok bool) {
	switch part {
	case resourcev1.LinkPart_LINK_PART_HOST:
		return m.GetHost(), false, true
	case resourcev1.LinkPart_LINK_PART_PORT:
		return strconv.Itoa(Port(m)), false, true
	case resourcev1.LinkPart_LINK_PART_USER:
		return m.GetUser(), true, true
	case resourcev1.LinkPart_LINK_PART_PASSWORD:
		return m.GetPassword(), true, true
	case resourcev1.LinkPart_LINK_PART_SENDER:
		return m.GetSender(), false, true
	case resourcev1.LinkPart_LINK_PART_SENDER_NAME:
		return m.GetSenderName(), false, true
	case resourcev1.LinkPart_LINK_PART_URL:
		return URL(m), true, true
	default:
		return "", false, false
	}
}

// URL is a mail server's connection as one URL, smtp://user:password@host:port, or
// smtps:// for TLS from the start, as Nodemailer and most others read it.
func URL(m *resourcev1.ConnectionSmtp) string {
	u := url.URL{Scheme: "smtp", Host: hostPort(m.GetHost(), Port(m))}
	if Security(m) == resourcev1.SmtpSecurity_SMTP_SECURITY_TLS {
		u.Scheme = "smtps"
	}
	if m.GetUser() != "" {
		u.User = url.UserPassword(m.GetUser(), m.GetPassword())
	}
	return u.String()
}

func hostPort(host string, port int) string {
	if strings.Contains(host, ":") {
		return "[" + host + "]:" + strconv.Itoa(port)
	}
	return host + ":" + strconv.Itoa(port)
}
