// SPDX-License-Identifier: AGPL-3.0-only

package link_test

import (
	"slices"
	"strings"
	"testing"

	resourcev1 "github.com/dockwell/dockwell/gen/dockwell/resource/v1alpha1"
	"github.com/dockwell/dockwell/internal/resource"
	"github.com/dockwell/dockwell/internal/resource/link"
)

// A link to a mail server sets a mail server's parts, says a sender of its app's own
// where it likes, and takes nothing of a database's or storage's; its user is a
// secret too, as a provider's key ID often is (ADR 0099).
func TestALinkToAMailServer(t *testing.T) {
	t.Parallel()
	ok := func() *resourcev1.Link {
		return &resourcev1.Link{Label: "Mail", Connection: "relay", Kind: resourcev1.LinkKind_LINK_KIND_SMTP, Sender: "blog@example.com", SenderName: "Blog", Sets: []*resourcev1.LinkSet{
			{Name: "SMTP_HOST", Part: resourcev1.LinkPart_LINK_PART_HOST},
			{Name: "SMTP_USER", Part: resourcev1.LinkPart_LINK_PART_USER},
			{Name: "SMTP_SECURE", Part: resourcev1.LinkPart_LINK_PART_SECURITY},
			{Name: "MAIL_FROM", Part: resourcev1.LinkPart_LINK_PART_SENDER},
		}}
	}
	if err := (link.Type{}).Validate(ok()); err != nil {
		t.Fatal(err)
	}
	for want, change := range map[string]func(*resourcev1.Link){
		"takes no prefix or features":      func(l *resourcev1.Link) { l.Prefix = "blog/" },
		"which a mail server doesn't have": func(l *resourcev1.Link) { l.Sets[0].Part = resourcev1.LinkPart_LINK_PART_BUCKET },
		"an email address alone":           func(l *resourcev1.Link) { l.Sender = "Blog <blog@example.com>" },
		"names no service, environment":    func(l *resourcev1.Link) { l.Service = "db" },
		"with no template or TLS words":    func(l *resourcev1.Link) { l.Sets[0].TlsOn = "true" },
	} {
		l := ok()
		change(l)
		if err := (link.Type{}).Validate(l); err == nil || !strings.Contains(err.Error(), want) {
			t.Errorf("%s: %v", want, err)
		}
	}
	for want, l := range map[string]*resourcev1.Link{
		"which a link to a mail server does": {Label: "Database", Service: "db", Engine: resourcev1.LinkEngine_LINK_ENGINE_MYSQL, Sender: "a@example.com"},
		"takes no sender":                    {Label: "Storage", Connection: "uploads", Kind: resourcev1.LinkKind_LINK_KIND_S3, Sender: "a@example.com", Sets: []*resourcev1.LinkSet{{Name: "B", Part: resourcev1.LinkPart_LINK_PART_BUCKET}}},
	} {
		if err := (link.Type{}).Validate(l); err == nil || !strings.Contains(err.Error(), want) {
			t.Errorf("%s: %v", want, err)
		}
	}
	ties := (link.Type{}).Ties("mail", ok())
	if !slices.Contains(ties, resource.Ref{Kind: "secret", Name: "SMTP_USER"}) || !slices.Contains(ties, resource.Ref{Kind: "variable", Name: "SMTP_HOST"}) {
		t.Errorf("the link ties %v", ties)
	}
	was, is := ok(), ok()
	is.Sender = "news@example.com"
	if !(link.Type{}).ChangesWhatsRead(was, is) {
		t.Error("a new sender doesn't reach the app")
	}
}
