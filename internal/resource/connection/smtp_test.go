// SPDX-License-Identifier: AGPL-3.0-only

package connection

import (
	"strings"
	"testing"

	"google.golang.org/protobuf/proto"

	resourcev1 "github.com/dockwell/dockwell/gen/dockwell/resource/v1alpha1"
)

func relay() *resourcev1.ConnectionSpec {
	return &resourcev1.ConnectionSpec{
		Kind: resourcev1.LinkKind_LINK_KIND_SMTP, Reach: resourcev1.ConnectionReach_CONNECTION_REACH_EVERY,
		Smtp: &resourcev1.ConnectionSmtp{Host: "smtp.example.com", User: "notes@example.com", Password: "the-password", Sender: "notes@example.com", SenderName: "Notes"},
	}
}

// A mail server's connection takes STARTTLS on 587 unless it says, and its port by
// its security; it refuses what can't reach a server, a sender that isn't an address
// alone, and a password without TLS to a server that isn't near.
func TestAMailServersConnectionIsChecked(t *testing.T) {
	t.Parallel()
	c := relay()
	if err := (Type{}).Validate(c); err != nil {
		t.Fatal(err)
	}
	if c.GetSmtp().GetSecurity() != resourcev1.SmtpSecurity_SMTP_SECURITY_STARTTLS || c.GetSmtp().GetPort() != PortStartTLS {
		t.Errorf("unset, it's %v on %d", c.GetSmtp().GetSecurity(), c.GetSmtp().GetPort())
	}
	tls := relay()
	tls.Smtp.Security = resourcev1.SmtpSecurity_SMTP_SECURITY_TLS
	if err := (Type{}).Validate(tls); err != nil || tls.GetSmtp().GetPort() != PortTLS {
		t.Errorf("TLS is on %d: %v", tls.GetSmtp().GetPort(), err)
	}
	for want, change := range map[string]func(*resourcev1.ConnectionSmtp){
		"needs its host":              func(m *resourcev1.ConnectionSmtp) { m.Host = "" },
		"its name or address alone":   func(m *resourcev1.ConnectionSmtp) { m.Host = "smtp://smtp.example.com" },
		"from 1 to 65535":             func(m *resourcev1.ConnectionSmtp) { m.Port = 70000 },
		"user is one line":            func(m *resourcev1.ConnectionSmtp) { m.User = "a\nb" },
		"needs the user it signs in":  func(m *resourcev1.ConnectionSmtp) { m.User = "" },
		"an email address alone":      func(m *resourcev1.ConnectionSmtp) { m.Sender = "Notes <notes@example.com>" },
		"no quotes or angle brackets": func(m *resourcev1.ConnectionSmtp) { m.SenderName = "Notes <x>" },
		"SMTP_SECURITY_STARTTLS":      func(m *resourcev1.ConnectionSmtp) { m.Security = 9 },
		"signs in only over STARTTLS": func(m *resourcev1.ConnectionSmtp) { m.Security = resourcev1.SmtpSecurity_SMTP_SECURITY_NONE },
	} {
		c := relay()
		change(c.Smtp)
		if err := (Type{}).Validate(c); err == nil || !strings.Contains(err.Error(), want) {
			t.Errorf("%s: %v", want, err)
		} else if strings.Contains(err.Error(), "the-password") {
			t.Errorf("%s says the password: %v", want, err)
		}
	}
	// Without TLS, only a private address signs in, judged by its range: no name,
	// though of one label, as Docker's DNS may hand it on (the review's H1 and L4).
	for _, host := range []string{"mail", "mail.internal", "2001:db8::25", "[2001:db8::25]", "203.0.113.7", "fe80::1"} {
		c := relay()
		c.Smtp.Host, c.Smtp.Security = host, resourcev1.SmtpSecurity_SMTP_SECURITY_NONE
		if err := (Type{}).Validate(c); err == nil {
			t.Errorf("%s without TLS takes a password", host)
		}
	}
	for _, host := range []string{"10.0.0.5", "192.168.1.2", "127.0.0.1", "100.64.0.9", "fd00::25", "::1"} {
		c := relay()
		c.Smtp.Host, c.Smtp.Security = host, resourcev1.SmtpSecurity_SMTP_SECURITY_NONE
		if err := (Type{}).Validate(c); err != nil || c.GetSmtp().GetPort() != PortNone {
			t.Errorf("%s without TLS: %v, on %d", host, err, c.GetSmtp().GetPort())
		}
	}
	both := relay()
	both.S3 = &resourcev1.ConnectionS3{Endpoint: "https://s3.example.com", Bucket: "uploads"}
	if err := (Type{}).Validate(both); err == nil {
		t.Error("a mail server's connection holds a bucket")
	}
	if err := (Type{}).CheckNew(&resourcev1.ConnectionSpec{Smtp: &resourcev1.ConnectionSmtp{Host: "smtp.example.com", User: "a"}}); err == nil {
		t.Error("a new mail server's connection signs in with no password")
	}
}

// An update that leaves the user and password out keeps them while the host, port
// and security stay; one user or password alone is refused whether it's the saved
// one or not, so no edit tells a guess at it.
func TestAMailServersSignInIsKept(t *testing.T) {
	t.Parallel()
	was := relay()
	_ = (Type{}).Validate(was)
	keep := func(change func(*resourcev1.ConnectionSmtp)) (*resourcev1.ConnectionSpec, error) {
		is := proto.CloneOf(was)
		is.Smtp.User, is.Smtp.Password = "", ""
		change(is.Smtp)
		_ = (Type{}).Validate(is)
		_, err := (Type{}).Keep(was, is)
		return is, err
	}
	is, err := keep(func(m *resourcev1.ConnectionSmtp) { m.SenderName = "Blog" })
	if err != nil || is.GetSmtp().GetUser() != "notes@example.com" || is.GetSmtp().GetPassword() != "the-password" {
		t.Errorf("a rename keeps %v: %v", is.GetSmtp(), err)
	}
	for want, change := range map[string]func(*resourcev1.ConnectionSmtp){
		"needs its password again": func(m *resourcev1.ConnectionSmtp) { m.Host = "smtp.example.net" },
		"another host, port":       func(m *resourcev1.ConnectionSmtp) { m.Port = 2525 },
		"or security":              func(m *resourcev1.ConnectionSmtp) { m.Security = resourcev1.SmtpSecurity_SMTP_SECURITY_TLS },
		"leave both out":           func(m *resourcev1.ConnectionSmtp) { m.User = "notes@example.com" },
		"the user it signs in as":  func(m *resourcev1.ConnectionSmtp) { m.Password = "new" },
	} {
		if _, err := keep(change); err == nil || !strings.Contains(err.Error(), want) {
			t.Errorf("%s: %v", want, err)
		}
	}
	if !(Type{}).ChangesWhatsRead(was, func() *resourcev1.ConnectionSpec { c := proto.CloneOf(was); c.Smtp.Sender = "a@example.com"; return c }()) {
		t.Error("a new sender doesn't reach the apps that read it")
	}
}

// Each part a link reads of a mail server's connection, the user, the password and
// the URL as secrets, and its security in no part, as each app has its own words.
func TestAMailServersParts(t *testing.T) {
	t.Parallel()
	c := relay()
	c.Smtp.Password = "p@ss:word"
	_ = (Type{}).Validate(c)
	for part, want := range map[resourcev1.LinkPart][2]string{
		resourcev1.LinkPart_LINK_PART_HOST:        {"smtp.example.com", ""},
		resourcev1.LinkPart_LINK_PART_PORT:        {"587", ""},
		resourcev1.LinkPart_LINK_PART_USER:        {"notes@example.com", "secret"},
		resourcev1.LinkPart_LINK_PART_PASSWORD:    {"p@ss:word", "secret"},
		resourcev1.LinkPart_LINK_PART_SENDER:      {"notes@example.com", ""},
		resourcev1.LinkPart_LINK_PART_SENDER_NAME: {"Notes", ""},
		resourcev1.LinkPart_LINK_PART_URL:         {"smtp://notes%40example.com:p%40ss%3Aword@smtp.example.com:587", "secret"},
	} {
		value, secret, ok := Part(c, part)
		if !ok || value != want[0] || secret != (want[1] == "secret") {
			t.Errorf("%s is %q, secret %v, %v", part, value, secret, ok)
		}
	}
	if _, _, ok := Part(c, resourcev1.LinkPart_LINK_PART_SECURITY); ok {
		t.Error("security is a part a link reads")
	}
	c.Smtp.Security, c.Smtp.Port = resourcev1.SmtpSecurity_SMTP_SECURITY_TLS, 0
	if u := URL(c.GetSmtp()); !strings.HasPrefix(u, "smtps://") || !strings.HasSuffix(u, ":465") {
		t.Errorf("TLS's URL is %s", u)
	}
}
