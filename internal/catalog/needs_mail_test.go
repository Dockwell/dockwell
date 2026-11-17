// SPDX-License-Identifier: AGPL-3.0-only

package catalog_test

import (
	"slices"
	"strings"
	"testing"

	"google.golang.org/protobuf/proto"

	catalogv1 "github.com/dockwell/dockwell/gen/dockwell/catalog/v1alpha1"
	resourcev1 "github.com/dockwell/dockwell/gen/dockwell/resource/v1alpha1"
	"github.com/dockwell/dockwell/internal/catalog"
)

// securityWords are Nodemailer's: SMTP_SECURE for TLS from the start, and
// SMTP_REQUIRE_TLS for STARTTLS.
func securityWords() []*catalogv1.NeedSet {
	return []*catalogv1.NeedSet{
		{Name: "SMTP_SECURE", Part: catalogv1.Part_PART_SECURITY, Words: []*catalogv1.NeedWord{{Value: "tls", Word: "true"}, {Value: "starttls", Word: "false"}, {Value: "none", Word: "false"}}},
		{Name: "SMTP_REQUIRE_TLS", Part: catalogv1.Part_PART_SECURITY, Words: []*catalogv1.NeedWord{{Value: "tls", Word: "false"}, {Value: "starttls", Word: "true"}, {Value: "none", Word: "false"}}},
	}
}

// withMail is WordPress given an optional mail server, as ADR 0099's example writes
// one, its parts each a whole value of its environment.
func withMail(t *testing.T) *catalogv1.Entry {
	t.Helper()
	e := proto.CloneOf(wordpressNeeds(t))
	e.Compose = strings.Replace(e.GetCompose(), "WORDPRESS_DB_HOST: ${DB_ADDRESS}", "WORDPRESS_DB_HOST: ${DB_ADDRESS}\n"+
		"      SMTP_HOST: ${SMTP_HOST}\n"+
		"      SMTP_PORT: ${SMTP_PORT}\n"+
		"      SMTP_SECURE: ${SMTP_SECURE}\n"+
		"      SMTP_REQUIRE_TLS: ${SMTP_REQUIRE_TLS}\n"+
		"      SMTP_USER: ${SMTP_USER}\n"+
		"      SMTP_PASSWORD: ${SMTP_PASSWORD}\n"+
		"      MAIL_FROM: ${MAIL_FROM}", 1)
	e.Needs = append(e.Needs, &catalogv1.Need{
		Name: "mail", Label: "Mail", Kind: catalogv1.Kind_KIND_SMTP, Optional: true,
		Sets: append([]*catalogv1.NeedSet{
			{Name: "SMTP_HOST", Part: catalogv1.Part_PART_HOST},
			{Name: "SMTP_PORT", Part: catalogv1.Part_PART_PORT},
			{Name: "SMTP_USER", Part: catalogv1.Part_PART_USER},
			{Name: "SMTP_PASSWORD", Part: catalogv1.Part_PART_PASSWORD},
			{Name: "MAIL_FROM", Part: catalogv1.Part_PART_SENDER},
		}, securityWords()...),
	})
	return e
}

// A mail server's need as ADR 0099 writes one passes the check; one that can't reach
// a server, or sets what only another kind has, is refused.
func TestCheckTakesAMailNeed(t *testing.T) {
	e := withMail(t)
	if err := catalog.Check(e); err != nil {
		t.Fatal(err)
	}
	mail := func(e *catalogv1.Entry) *catalogv1.Need { return e.Needs[len(e.Needs)-1] }
	drop := func(name string) func(e *catalogv1.Entry) {
		return func(e *catalogv1.Entry) {
			mail(e).Sets = slices.DeleteFunc(mail(e).Sets, func(s *catalogv1.NeedSet) bool { return s.GetName() == name })
		}
	}
	for want, change := range map[string]func(e *catalogv1.Entry){
		"sets too little to reach one": drop("SMTP_PORT"),
		"not how the app secures it":   func(e *catalogv1.Entry) { drop("SMTP_SECURE")(e); drop("SMTP_REQUIRE_TLS")(e) },
		"Set both, or neither":         drop("SMTP_PASSWORD"),
		"only S3 storage takes":        func(e *catalogv1.Entry) { mail(e).Bucket = "mail" },
		"KIND_SMTP alone": func(e *catalogv1.Entry) {
			mail(e).Accepts = []*catalogv1.NeedAccept{{Engine: catalogv1.Engine_ENGINE_MYSQL}}
		},
		"has no word for none": func(e *catalogv1.Entry) { mail(e).Sets[5].Words = mail(e).Sets[5].Words[:2] },
		"which only a mail server has": func(e *catalogv1.Entry) {
			e.Needs[0].Sets = append(e.Needs[0].Sets, &catalogv1.NeedSet{Name: "MAIL_FROM", Part: catalogv1.Part_PART_SENDER})
		},
		"its own mail server, which Dock": func(e *catalogv1.Entry) { mail(e).Service = "db" },
	} {
		e := withMail(t)
		change(e)
		if err := catalog.Check(e); err == nil || !strings.Contains(err.Error(), want) {
			t.Errorf("%s: %v", want, err)
		}
	}
}

// A need is offered a mail server's connection only where the app's words tell its
// security apart from each other, under one name or another: an app told only to
// use TLS from the start can't require STARTTLS, nor go without TLS.
func TestAMailNeedTakesOnlyTheSecurityItCanBeTold(t *testing.T) {
	n := &catalogv1.Need{Kind: catalogv1.Kind_KIND_SMTP, Sets: securityWords()}
	for _, s := range []string{"starttls", "tls", "none"} {
		if why := catalog.SecurityWhy(n, s); why != "" {
			t.Errorf("both names, %s: %s", s, why)
		}
	}
	n.Sets = n.Sets[:1]
	for s, want := range map[string]string{"tls": "", "starttls": "require STARTTLS", "none": "go without TLS"} {
		if why := catalog.SecurityWhy(n, s); want == "" && why != "" || want != "" && !strings.Contains(why, want) {
			t.Errorf("SMTP_SECURE alone, %s: %q", s, why)
		}
	}
}

// A mail server's connection meets the need through a link named for it: each name
// read from the connection, the user and password as secrets, and its security
// written in the app's words. A storage connection is no way to meet it, nor a mail
// server's one that serves another app unshared.
func TestAConnectionMeetsAMailNeed(t *testing.T) {
	e := withMail(t)
	relay := catalog.ConnectionWay{Name: "relay", Label: "Relay", Kind: catalogv1.Kind_KIND_SMTP, Security: "starttls", Sender: "notes@example.com"}
	busy := catalog.ConnectionWay{Name: "busy", Kind: catalogv1.Kind_KIND_SMTP, Security: "starttls", Serves: true}
	bucket := catalog.ConnectionWay{Name: "uploads", Prefix: "site/"}
	ways := catalog.WithConnections(e, catalog.Ways(e, lookupBuiltIn(t)), []catalog.ConnectionWay{relay, busy, bucket})
	var choices []string
	for _, w := range ways["mail"] {
		choices = append(choices, w.Choice()+"="+w.Why)
	}
	if !slices.Equal(choices, []string{"unset=", "connection:relay=", "connection:busy=it serves another app already, and isn't shared"}) {
		t.Errorf("mail's ways are %v", choices)
	}
	met, err := catalog.Meet(e, ways, map[string]string{"mail": "connection:relay"}, nil)
	if err != nil {
		t.Fatal(err)
	}
	needed, err := catalog.Need(e, met)
	if err != nil {
		t.Fatal(err)
	}
	l := needed.Links["mail"]
	if l.GetConnection() != "relay" || l.GetKind() != resourcev1.LinkKind_LINK_KIND_SMTP || l.GetLabel() != "Mail" || l.GetSender() != "" || l.GetPrefix() != "" {
		t.Errorf("the link is %v", l)
	}
	for name, secret := range map[string]bool{"SMTP_HOST": false, "SMTP_PORT": false, "MAIL_FROM": false, "SMTP_USER": true, "SMTP_PASSWORD": true} {
		if r, ok := needed.Read[name]; !ok || r.Connection != "relay" || r.Link != "mail" || r.Secret != secret {
			t.Errorf("%s is read as %+v", name, r)
		}
	}
	inputs := map[string]*catalogv1.Input{}
	for _, in := range needed.Entry.GetInputs() {
		inputs[in.GetName()] = in
	}
	for name, word := range map[string]string{"SMTP_SECURE": "false", "SMTP_REQUIRE_TLS": "true"} {
		if in := inputs[name]; in.GetDefaultValue() != word {
			t.Errorf("%s is %v, want %q", name, in, word)
		}
	}
	if pin := needed.Pinned["mail"]; pin.GetConnection() != "relay" || pin.GetPrefix() != "" {
		t.Errorf("the pin is %v", pin)
	}
}
