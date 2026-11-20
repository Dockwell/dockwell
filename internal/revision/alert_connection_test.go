// SPDX-License-Identifier: AGPL-3.0-only

package revision

import (
	"errors"
	"log/slog"
	"strings"
	"testing"

	resourcev1 "github.com/dockwell/dockwell/gen/dockwell/resource/v1alpha1"
	"github.com/dockwell/dockwell/internal/control/place"
	"github.com/dockwell/dockwell/internal/resource"
	"github.com/dockwell/dockwell/internal/resource/alert"
	"github.com/dockwell/dockwell/internal/resource/connection"
	"github.com/dockwell/dockwell/internal/resource/link"
	"github.com/dockwell/dockwell/internal/resource/variable"
	"github.com/dockwell/dockwell/internal/scope"
	"github.com/dockwell/dockwell/internal/secrets/secretstest"
	"github.com/dockwell/dockwell/internal/store"
	"github.com/dockwell/dockwell/internal/store/storetest"
)

func TestAlertChannelConnectionRules(t *testing.T) {
	storetest.Each(t, testAlertChannelConnectionRules)
}

func testAlertChannelConnectionRules(t *testing.T, backend store.Backend) {
	db, _ := storetest.Open(t, backend)
	logger := slog.New(slog.DiscardHandler)
	if _, err := Setup(t.Context(), db, logger); err != nil {
		t.Fatal(err)
	}
	kinds := resource.NewRegistry(
		scope.ProjectType{}, scope.EnvironmentType{},
		connection.Type{}, link.Type{}, variable.Type{}, variable.SecretType{},
		alert.Rule{}, alert.Channel{},
	)
	e := New(db, kinds, fleet{}, secretstest.Keeper(db, kinds), logger)
	tr := tree(t, db)
	org := tr.Organization.ID
	proj := tr.Projects[0].ID
	env := tr.Projects[0].Environments[0].ID

	smtpConn := func(host, user, pass string, shared bool, reach resourcev1.ConnectionReach) *resourcev1.ConnectionSpec {
		return &resourcev1.ConnectionSpec{
			DisplayName: "Mail",
			Kind:        resourcev1.LinkKind_LINK_KIND_SMTP,
			Shared:      shared,
			Reach:       reach,
			Smtp: &resourcev1.ConnectionSmtp{
				Host: host, Port: 587, Security: resourcev1.SmtpSecurity_SMTP_SECURITY_STARTTLS, User: user, Password: pass,
			},
		}
	}
	chanSpec := func(conn string) *resourcev1.AlertChannel {
		return &resourcev1.AlertChannel{To: &resourcev1.AlertChannel_Email{Email: &resourcev1.AlertEmail{
			Connection: conn, From: "alerts@example.com", To: []string{"ops@example.com"},
		}}}
	}
	connRef := func(name string) resource.Ref { return resource.Ref{Kind: connection.Kind, Scope: org, Name: name} }
	chanRef := func(name string) resource.Ref { return resource.Ref{Kind: alert.ChannelKind, Scope: org, Name: name} }
	linkRef := func(name string) resource.Ref { return resource.Ref{Kind: link.Kind, Scope: env, Name: name} }
	secRef := func(name string) resource.Ref { return resource.Ref{Kind: variable.SecretKind, Scope: env, Name: name} }
	mailSets := []*resourcev1.LinkSet{{Name: "SMTP_HOST", Part: resourcev1.LinkPart_LINK_PART_HOST}}

	// 1. Missing connection is refused with install wording.
	if _, err := e.Propose(t.Context(), Proposal{
		Summary: "Missing connection",
		Edits:   []Edit{{Ref: chanRef("missing"), Spec: chanSpec("nope")}},
	}); err == nil || !strings.Contains(err.Error(), "isn't on this install") {
		t.Fatalf("missing connection = %v", err)
	}

	// 2. Non-SMTP (S3) connection is refused.
	apply(t, e, propose(t, e, "Add S3", Edit{
		Ref: connRef("bucket"),
		Spec: &resourcev1.ConnectionSpec{
			Kind: resourcev1.LinkKind_LINK_KIND_S3,
			S3:   &resourcev1.ConnectionS3{Endpoint: "https://s3.example.com", Region: "auto", Bucket: "bkt", AccessKeyId: "AKIA", SecretKey: "secret"},
		},
	}))
	if _, err := e.Propose(t.Context(), Proposal{
		Summary: "S3 alert channel",
		Edits:   []Edit{{Ref: chanRef("s3-mail"), Spec: chanSpec("bucket")}},
	}); err == nil || !strings.Contains(err.Error(), "isn't a mail server") {
		t.Fatalf("S3 connection = %v", err)
	}

	// 3. TLS rules: a mail connection that passes ConnectionSmtp.Validate (plain SMTP
	// with auth on a private IP, or plain SMTP without auth on a public host) must
	// still be refused for alert email, naming the connection.
	apply(t, e, propose(t, e, "Add private plain+auth SMTP", Edit{
		Ref: connRef("plain-auth"),
		Spec: &resourcev1.ConnectionSpec{
			Kind: resourcev1.LinkKind_LINK_KIND_SMTP,
			Smtp: &resourcev1.ConnectionSmtp{
				Host: "10.0.0.5", Port: 25, Security: resourcev1.SmtpSecurity_SMTP_SECURITY_NONE,
				User: "alerts", Password: "pw1",
			},
		},
	}))
	if _, err := e.Propose(t.Context(), Proposal{
		Summary: "Alert on private plain+auth SMTP",
		Edits:   []Edit{{Ref: chanRef("ch-plain-auth"), Spec: chanSpec("plain-auth")}},
	}); err == nil || !strings.Contains(err.Error(), "plain-auth") || !strings.Contains(err.Error(), "signs in only over TLS") {
		t.Fatalf("plain+auth SMTP = %v", err)
	}
	apply(t, e, propose(t, e, "Add public plain no-auth SMTP", Edit{
		Ref: connRef("public-plain"),
		Spec: &resourcev1.ConnectionSpec{
			Kind: resourcev1.LinkKind_LINK_KIND_SMTP,
			Smtp: &resourcev1.ConnectionSmtp{
				Host: "smtp.example.com", Port: 25, Security: resourcev1.SmtpSecurity_SMTP_SECURITY_NONE,
			},
		},
	}))
	if _, err := e.Propose(t.Context(), Proposal{
		Summary: "Alert on public plain SMTP",
		Edits:   []Edit{{Ref: chanRef("ch-public-plain"), Spec: chanSpec("public-plain")}},
	}); err == nil || !strings.Contains(err.Error(), "public-plain") || !strings.Contains(err.Error(), "has to use TLS") {
		t.Fatalf("public plain SMTP = %v", err)
	}

	// 4. Shared connection or connection with an app link is refused.
	apply(t, e, propose(t, e, "Add shared SMTP", Edit{
		Ref:  connRef("shared-mail"),
		Spec: smtpConn("smtp.example.com", "alerts", "pw-shared", true, resourcev1.ConnectionReach_CONNECTION_REACH_EVERY),
	}))
	if _, err := e.Propose(t.Context(), Proposal{
		Summary: "Alert on shared SMTP",
		Edits:   []Edit{{Ref: chanRef("ch-shared"), Spec: chanSpec("shared-mail")}},
	}); err == nil || !strings.Contains(err.Error(), "shared connection") {
		t.Fatalf("shared SMTP = %v", err)
	}

	apply(t, e, propose(t, e, "Add app SMTP and link",
		Edit{Ref: connRef("app-mail"), Spec: smtpConn("smtp.example.com", "app", "pw-app", false, resourcev1.ConnectionReach_CONNECTION_REACH_EVERY)},
		Edit{Ref: linkRef("mail-link"), Spec: &resourcev1.Link{Label: "Mail", Connection: "app-mail", Kind: resourcev1.LinkKind_LINK_KIND_SMTP, Sets: mailSets}},
	))
	if _, err := e.Propose(t.Context(), Proposal{
		Summary: "Alert on linked SMTP",
		Edits:   []Edit{{Ref: chanRef("ch-linked"), Spec: chanSpec("app-mail")}},
	}); err == nil || !strings.Contains(err.Error(), "send mail as the alert sender") {
		t.Fatalf("linked SMTP = %v", err)
	}

	// Same-change link deletion without password rotation is refused; with rotation it succeeds.
	if _, err := e.Propose(t.Context(), Proposal{
		Summary: "Disconnect link and adopt without rotation",
		Edits: []Edit{
			{Ref: linkRef("mail-link"), Spec: nil},
			{Ref: chanRef("ch-adopt"), Spec: chanSpec("app-mail")},
		},
	}); err == nil || !strings.Contains(err.Error(), "an app holds the password of connection app-mail") {
		t.Fatalf("same-change unlink without rotation = %v", err)
	}
	apply(t, e, propose(t, e, "Disconnect link, rotate password, and adopt",
		Edit{Ref: linkRef("mail-link"), Spec: nil},
		Edit{Ref: connRef("app-mail"), Spec: smtpConn("smtp.example.com", "alerts", "pw-fresh-alerts", false, resourcev1.ConnectionReach_CONNECTION_REACH_UNSPECIFIED)},
		Edit{Ref: chanRef("ch-adopt"), Spec: chanSpec("app-mail")},
	))

	// 5. Creating an install-only connection + alert channel in ONE change (channel
	// listed first) succeeds via Referrer ordering.
	apply(t, e, propose(t, e, "Add alert mail connection and channel",
		Edit{Ref: chanRef("ops"), Spec: chanSpec("postmark")},
		Edit{Ref: connRef("postmark"), Spec: smtpConn("smtp.postmarkapp.com", "pm-user", "pm-pass-1", false, resourcev1.ConnectionReach_CONNECTION_REACH_UNSPECIFIED)},
	))

	// 6. Linking an app to an alert connection is refused (no leak when unreached;
	// explicit refusal when reached).
	if _, err := e.Propose(t.Context(), Proposal{
		Summary: "Link unreached alert conn",
		Edits:   []Edit{{Ref: linkRef("try-link"), Spec: &resourcev1.Link{Label: "Mail", Connection: "postmark", Kind: resourcev1.LinkKind_LINK_KIND_SMTP, Sets: mailSets}}},
	}); err == nil || !strings.Contains(err.Error(), "isn't one this project can use") {
		t.Fatalf("link to unreached alert conn = %v", err)
	}
	widened := smtpConn("smtp.postmarkapp.com", "", "", false, resourcev1.ConnectionReach_CONNECTION_REACH_PROJECTS)
	widened.Projects = []string{proj}
	apply(t, e, propose(t, e, "Widen postmark reach", Edit{Ref: connRef("postmark"), Spec: widened}))
	if _, err := e.Propose(t.Context(), Proposal{
		Summary: "Link reached alert conn",
		Edits:   []Edit{{Ref: linkRef("try-link"), Spec: &resourcev1.Link{Label: "Mail", Connection: "postmark", Kind: resourcev1.LinkKind_LINK_KIND_SMTP, Sets: mailSets}}},
	}); err == nil || !strings.Contains(err.Error(), "used for alert email") {
		t.Fatalf("link to reached alert conn = %v", err)
	}

	// 7. Marking shared, deleting, retyping, or breaking TLS on postmark while ops uses it is refused.
	shareEdit := smtpConn("smtp.postmarkapp.com", "", "", true, resourcev1.ConnectionReach_CONNECTION_REACH_UNSPECIFIED)
	if _, err := e.Propose(t.Context(), Proposal{
		Summary: "Share alert conn",
		Edits:   []Edit{{Ref: connRef("postmark"), Spec: shareEdit}},
	}); err == nil || !strings.Contains(err.Error(), "can't be marked shared") {
		t.Fatalf("share alert conn = %v", err)
	}
	if _, err := e.Propose(t.Context(), Proposal{
		Summary: "Delete alert conn",
		Edits:   []Edit{{Ref: connRef("postmark"), Spec: nil}},
	}); err == nil || !strings.Contains(err.Error(), "alert channel ops still uses the connection postmark") {
		t.Fatalf("delete alert conn = %v", err)
	}
	if _, err := e.Propose(t.Context(), Proposal{
		Summary: "Retype alert conn",
		Edits: []Edit{{Ref: connRef("postmark"), Spec: &resourcev1.ConnectionSpec{
			Kind: resourcev1.LinkKind_LINK_KIND_S3,
			S3:   &resourcev1.ConnectionS3{Endpoint: "https://s3.example.com", Region: "auto", Bucket: "bkt", AccessKeyId: "A", SecretKey: "B"},
		}}},
	}); err == nil || !strings.Contains(err.Error(), "another kind of connection") {
		t.Fatalf("retype alert conn = %v", err)
	}
	if _, err := e.Propose(t.Context(), Proposal{
		Summary: "Downgrade alert conn to plain+auth on private IP",
		Edits: []Edit{{Ref: connRef("postmark"), Spec: &resourcev1.ConnectionSpec{
			Kind: resourcev1.LinkKind_LINK_KIND_SMTP,
			Smtp: &resourcev1.ConnectionSmtp{
				Host: "10.0.0.5", Port: 25, Security: resourcev1.SmtpSecurity_SMTP_SECURITY_NONE,
				User: "pm-user", Password: "pm-pass-1",
			},
		}}},
	}); err == nil || !strings.Contains(err.Error(), "postmark") || !strings.Contains(err.Error(), "signs in only over TLS") {
		t.Fatalf("downgrade alert conn TLS = %v", err)
	}

	// 8. Live plain secret holding the password or full SMTP URL blocks adoption,
	// blocks rotating back to that password, and blocks Undo reverting to it.
	apply(t, e, propose(t, e, "Add app secret with copied SMTP password",
		Edit{Ref: secRef("APP_SMTP_PASS"), Spec: &resourcev1.Secret{Value: "copied-pass"}},
		Edit{Ref: connRef("held-mail"), Spec: smtpConn("smtp.example.com", "u", "copied-pass", false, resourcev1.ConnectionReach_CONNECTION_REACH_UNSPECIFIED)},
	))
	if _, err := e.Propose(t.Context(), Proposal{
		Summary: "Adopt held mail connection",
		Edits:   []Edit{{Ref: chanRef("ch-held"), Spec: chanSpec("held-mail")}},
	}); err == nil || !strings.Contains(err.Error(), "an app holds the password of connection held-mail") {
		t.Fatalf("adopt held-password conn = %v", err)
	}
	// Also caught when a plain secret holds the full smtp:// URL.
	urlConn := smtpConn("smtp.example.com", "u-url", "url-pass", false, resourcev1.ConnectionReach_CONNECTION_REACH_UNSPECIFIED)
	apply(t, e, propose(t, e, "Add app secret with full SMTP URL",
		Edit{Ref: secRef("APP_SMTP_URL"), Spec: &resourcev1.Secret{Value: connection.URL(urlConn.GetSmtp())}},
		Edit{Ref: connRef("held-url-mail"), Spec: urlConn},
	))
	if _, err := e.Propose(t.Context(), Proposal{
		Summary: "Adopt held-URL mail connection",
		Edits:   []Edit{{Ref: chanRef("ch-held-url"), Spec: chanSpec("held-url-mail")}},
	}); err == nil || !strings.Contains(err.Error(), "an app holds the password of connection held-url-mail") {
		t.Fatalf("adopt held-URL conn = %v", err)
	}

	// Rotating postmark to a fresh password works; rotating onto copied-pass is refused.
	rotPlan := propose(t, e, "Rotate postmark password", Edit{
		Ref:  connRef("postmark"),
		Spec: smtpConn("smtp.postmarkapp.com", "pm-user", "pm-pass-2", false, resourcev1.ConnectionReach_CONNECTION_REACH_UNSPECIFIED),
	})
	apply(t, e, rotPlan)
	if _, err := e.Propose(t.Context(), Proposal{
		Summary: "Rotate postmark to app-held password",
		Edits:   []Edit{{Ref: connRef("postmark"), Spec: smtpConn("smtp.postmarkapp.com", "pm-user", "copied-pass", false, resourcev1.ConnectionReach_CONNECTION_REACH_UNSPECIFIED)}},
	}); err == nil || !strings.Contains(err.Error(), "an app holds the password of connection postmark") {
		t.Fatalf("rotate to app-held password = %v", err)
	}
	// Undo that would restore pm-pass-1 after an app secret holds pm-pass-1 is refused.
	apply(t, e, propose(t, e, "App adopts old postmark password", Edit{
		Ref:  secRef("OLD_PM_PASS"),
		Spec: &resourcev1.Secret{Value: "pm-pass-1"},
	}))
	if _, err := e.Undo(t.Context(), rotPlan.Change, By{Actor: SystemActor}); err == nil || !strings.Contains(err.Error(), "an app holds the password of connection postmark") {
		t.Fatalf("Undo reverting postmark to app-held password = %v", err)
	}

	// 9. Deleting channel + connection together in one change succeeds.
	apply(t, e, propose(t, e, "Delete ops and postmark together",
		Edit{Ref: connRef("postmark"), Spec: nil},
		Edit{Ref: chanRef("ops"), Spec: nil},
	))
}

// Every alert-channel/connection isolation rule must also run at Apply time when
// a valid plan stays pending while the other side changes before Apply runs.
func TestAlertChannelConnectionRulesAtApply(t *testing.T) {
	storetest.Each(t, testAlertChannelConnectionRulesAtApply)
}

func testAlertChannelConnectionRulesAtApply(t *testing.T, backend store.Backend) {
	db, _ := storetest.Open(t, backend)
	logger := slog.New(slog.DiscardHandler)
	if _, err := Setup(t.Context(), db, logger); err != nil {
		t.Fatal(err)
	}
	kinds := resource.NewRegistry(
		scope.ProjectType{}, scope.EnvironmentType{},
		connection.Type{}, link.Type{}, variable.Type{}, variable.SecretType{},
		alert.Rule{}, alert.Channel{},
	)
	e := New(db, kinds, fleet{}, secretstest.Keeper(db, kinds), logger)
	tr := tree(t, db)
	org := tr.Organization.ID
	env := tr.Projects[0].Environments[0].ID

	connRef := func(name string) resource.Ref { return resource.Ref{Kind: connection.Kind, Scope: org, Name: name} }
	chanRef := func(name string) resource.Ref { return resource.Ref{Kind: alert.ChannelKind, Scope: org, Name: name} }
	linkRef := func(name string) resource.Ref { return resource.Ref{Kind: link.Kind, Scope: env, Name: name} }
	secRef := func(name string) resource.Ref { return resource.Ref{Kind: variable.SecretKind, Scope: env, Name: name} }
	smtpConn := func(pass string) *resourcev1.ConnectionSpec {
		return &resourcev1.ConnectionSpec{
			Kind:  resourcev1.LinkKind_LINK_KIND_SMTP,
			Reach: resourcev1.ConnectionReach_CONNECTION_REACH_EVERY,
			Smtp: &resourcev1.ConnectionSmtp{
				Host: "smtp.example.com", Port: 587, Security: resourcev1.SmtpSecurity_SMTP_SECURITY_STARTTLS,
				User: "alerts", Password: pass,
			},
		}
	}
	chanOn := func(conn string) *resourcev1.AlertChannel {
		return &resourcev1.AlertChannel{To: &resourcev1.AlertChannel_Email{Email: &resourcev1.AlertEmail{
			Connection: conn, From: "alerts@example.com", To: []string{"ops@example.com"},
		}}}
	}
	mailSets := []*resourcev1.LinkSet{{Name: "SMTP_HOST", Part: resourcev1.LinkPart_LINK_PART_HOST}}
	mustFailApply := func(label string, p *Plan, wantSub string) {
		t.Helper()
		err := e.Apply(t.Context(), p.Change)
		if err == nil || !strings.Contains(err.Error(), wantSub) {
			t.Fatalf("%s at apply time = %v, want %q", label, err, wantSub)
		}
	}
	loadSpec := func(ref resource.Ref) any {
		t.Helper()
		var out any
		if err := db.View(t.Context(), func(tx *store.Tx) error {
			it, err := e.load(t.Context(), tx, ref)
			out = it.current
			return err
		}); err != nil {
			t.Fatal(err)
		}
		return out
	}

	apply(t, e, propose(t, e, "Seed SMTP connections",
		Edit{Ref: connRef("m0"), Spec: smtpConn("p0")},
		Edit{Ref: connRef("m1"), Spec: smtpConn("p1")},
		Edit{Ref: connRef("m2"), Spec: smtpConn("p2")},
		Edit{Ref: connRef("m3"), Spec: smtpConn("p3")},
		Edit{Ref: connRef("m4"), Spec: smtpConn("p4")},
		Edit{Ref: connRef("m5"), Spec: smtpConn("p5")},
		Edit{Ref: connRef("m6"), Spec: smtpConn("p6")},
		Edit{Ref: connRef("m7"), Spec: smtpConn("p7")},
		Edit{Ref: connRef("m8"), Spec: smtpConn("p8")},
		Edit{Ref: connRef("m9"), Spec: smtpConn("p9-old")},
		Edit{Ref: linkRef("l0"), Spec: &resourcev1.Link{Label: "Mail", Connection: "m0", Kind: resourcev1.LinkKind_LINK_KIND_SMTP, Sets: mailSets}},
		Edit{Ref: chanRef("c7"), Spec: chanOn("m7")},
		Edit{Ref: chanRef("c9"), Spec: chanOn("m9")},
	))

	// 1. Link create proposed while m1 is free, then c1 claims m1 before apply.
	linkCreatePlan := propose(t, e, "Create link to m1", Edit{
		Ref:  linkRef("l1"),
		Spec: &resourcev1.Link{Label: "Mail", Connection: "m1", Kind: resourcev1.LinkKind_LINK_KIND_SMTP, Sets: mailSets},
	})
	apply(t, e, propose(t, e, "Claim m1 for alerts", Edit{Ref: chanRef("c1"), Spec: chanOn("m1")}))
	mustFailApply("link create", linkCreatePlan, "used for alert email")
	if loadSpec(linkRef("l1")) != nil {
		t.Error("conflicting link l1 landed after apply refusal")
	}

	// 2. Link retarget proposed while m2 is free, then c2 claims m2 before apply.
	retargetPlan := propose(t, e, "Retarget l0 to m2", Edit{
		Ref:  linkRef("l0"),
		Spec: &resourcev1.Link{Label: "Mail", Connection: "m2", Kind: resourcev1.LinkKind_LINK_KIND_SMTP, Sets: mailSets},
	})
	apply(t, e, propose(t, e, "Claim m2 for alerts", Edit{Ref: chanRef("c2"), Spec: chanOn("m2")}))
	mustFailApply("link retarget", retargetPlan, "used for alert email")
	if got := loadSpec(linkRef("l0")).(*resourcev1.Link); got.GetConnection() != "m0" {
		t.Errorf("l0 moved to %q after apply refusal", got.GetConnection())
	}

	// 3. Sharing proposed while m3 is free, then c3 claims m3 before apply.
	shareSpec := smtpConn("")
	shareSpec.Shared = true
	shareSpec.Smtp.User = ""
	sharePlan := propose(t, e, "Share m3", Edit{Ref: connRef("m3"), Spec: shareSpec})
	apply(t, e, propose(t, e, "Claim m3 for alerts", Edit{Ref: chanRef("c3"), Spec: chanOn("m3")}))
	mustFailApply("sharing", sharePlan, "can't be marked shared")
	if got := loadSpec(connRef("m3")).(*resourcev1.ConnectionSpec); got.GetShared() {
		t.Error("m3 became shared after apply refusal")
	}

	// 4. Delete proposed while m4 is free, then c4 claims m4 before apply.
	delPlan := propose(t, e, "Delete m4", Edit{Ref: connRef("m4"), Spec: nil})
	apply(t, e, propose(t, e, "Claim m4 for alerts", Edit{Ref: chanRef("c4"), Spec: chanOn("m4")}))
	mustFailApply("delete", delPlan, "still uses the connection")
	if loadSpec(connRef("m4")) == nil {
		t.Error("m4 was deleted after apply refusal")
	}

	// 5. Retype to S3 proposed while m5 is free, then c5 claims m5 before apply.
	retypePlan := propose(t, e, "Retype m5 to S3", Edit{
		Ref: connRef("m5"),
		Spec: &resourcev1.ConnectionSpec{
			Kind: resourcev1.LinkKind_LINK_KIND_S3,
			S3:   &resourcev1.ConnectionS3{Endpoint: "https://s3.example.com", Region: "auto", Bucket: "bkt", AccessKeyId: "A", SecretKey: "B"},
		},
	})
	apply(t, e, propose(t, e, "Claim m5 for alerts", Edit{Ref: chanRef("c5"), Spec: chanOn("m5")}))
	mustFailApply("kind change", retypePlan, "another kind of connection")
	if got := loadSpec(connRef("m5")).(*resourcev1.ConnectionSpec); got.GetKind() != resourcev1.LinkKind_LINK_KIND_SMTP {
		t.Errorf("m5 kind changed to %v after apply refusal", got.GetKind())
	}

	// 6. TLS downgrade proposed while m6 is free, then c6 claims m6 before apply.
	tlsDowngradePlan := propose(t, e, "Downgrade m6 to private plain+auth", Edit{
		Ref: connRef("m6"),
		Spec: &resourcev1.ConnectionSpec{
			Kind: resourcev1.LinkKind_LINK_KIND_SMTP,
			Smtp: &resourcev1.ConnectionSmtp{
				Host: "10.0.0.5", Port: 25, Security: resourcev1.SmtpSecurity_SMTP_SECURITY_NONE,
				User: "alerts", Password: "p6",
			},
		},
	})
	apply(t, e, propose(t, e, "Claim m6 for alerts", Edit{Ref: chanRef("c6"), Spec: chanOn("m6")}))
	mustFailApply("TLS downgrade", tlsDowngradePlan, "signs in only over TLS")
	if got := loadSpec(connRef("m6")).(*resourcev1.ConnectionSpec); got.GetSmtp().GetSecurity() != resourcev1.SmtpSecurity_SMTP_SECURITY_STARTTLS {
		t.Errorf("m6 security changed to %v after apply refusal", got.GetSmtp().GetSecurity())
	}

	// 7. Password rotation on alert connection m7 proposed while target password is
	// unheld, then an app secret adopts that password before apply.
	rotToAppPass := propose(t, e, "Rotate m7 password", Edit{
		Ref:  connRef("m7"),
		Spec: smtpConn("p7-taken"),
	})
	apply(t, e, propose(t, e, "App takes p7-taken", Edit{Ref: secRef("TAKEN_PASS"), Spec: &resourcev1.Secret{Value: "p7-taken"}}))
	mustFailApply("rotate to app-held password", rotToAppPass, "an app holds the password of connection m7")
	if got := loadSpec(connRef("m7")).(*resourcev1.ConnectionSpec); got.GetSmtp().GetPassword() != "p7" {
		t.Errorf("m7 password changed to %q after apply refusal", got.GetSmtp().GetPassword())
	}

	// 8. Alert channel create proposed while m8 is free, then an app links m8 before apply.
	chanCreatePlan := propose(t, e, "Create c8 on m8", Edit{Ref: chanRef("c8"), Spec: chanOn("m8")})
	apply(t, e, propose(t, e, "Link m8 to app", Edit{
		Ref:  linkRef("l8"),
		Spec: &resourcev1.Link{Label: "Mail", Connection: "m8", Kind: resourcev1.LinkKind_LINK_KIND_SMTP, Sets: mailSets},
	}))
	mustFailApply("channel create", chanCreatePlan, "send mail as the alert sender")
	if loadSpec(chanRef("c8")) != nil {
		t.Error("conflicting channel c8 landed after apply refusal")
	}

	// 9. Undo of a password rotation proposed while old password is unheld, then an
	// app secret adopts the old password before Undo applies.
	rotM9 := propose(t, e, "Rotate m9 to p9-new", Edit{Ref: connRef("m9"), Spec: smtpConn("p9-new")})
	apply(t, e, rotM9)
	undoPlan, err := e.Undo(t.Context(), rotM9.Change, By{Actor: SystemActor})
	if err != nil {
		t.Fatalf("Undo propose = %v", err)
	}
	apply(t, e, propose(t, e, "App adopts p9-old", Edit{Ref: secRef("OLD_M9_PASS"), Spec: &resourcev1.Secret{Value: "p9-old"}}))
	mustFailApply("undo to app-held password", undoPlan, "an app holds the password of connection m9")
	if got := loadSpec(connRef("m9")).(*resourcev1.ConnectionSpec); got.GetSmtp().GetPassword() != "p9-new" {
		t.Errorf("m9 password reverted to %q after apply refusal", got.GetSmtp().GetPassword())
	}
}

// Fail-closed: if a stored alert channel cannot be decoded, linking to a mail
// connection fails closed for both install admins and non-admin callers, and
// callers below install admin receive an error naming no other channel or connection.
func TestAlertChannelDecodeFailureFailsClosed(t *testing.T) {
	storetest.Each(t, testAlertChannelDecodeFailureFailsClosed)
}

func testAlertChannelDecodeFailureFailsClosed(t *testing.T, backend store.Backend) {
	db, loc := storetest.Open(t, backend)
	logger := slog.New(slog.DiscardHandler)
	if _, err := Setup(t.Context(), db, logger); err != nil {
		t.Fatal(err)
	}
	kinds := resource.NewRegistry(
		scope.ProjectType{}, scope.EnvironmentType{},
		connection.Type{}, link.Type{}, variable.Type{}, variable.SecretType{},
		alert.Rule{}, alert.Channel{},
	)
	e := New(db, kinds, fleet{}, secretstest.Keeper(db, kinds), logger)
	tr := tree(t, db)
	org := tr.Organization.ID
	proj := tr.Projects[0].ID
	env := tr.Projects[0].Environments[0].ID

	connRef := func(name string) resource.Ref { return resource.Ref{Kind: connection.Kind, Scope: org, Name: name} }
	chanRef := func(name string) resource.Ref { return resource.Ref{Kind: alert.ChannelKind, Scope: org, Name: name} }
	linkRef := func(name string) resource.Ref { return resource.Ref{Kind: link.Kind, Scope: env, Name: name} }

	apply(t, e, propose(t, e, "Seed SMTP and alert channel",
		Edit{Ref: connRef("secret-conn"), Spec: &resourcev1.ConnectionSpec{
			Kind:  resourcev1.LinkKind_LINK_KIND_SMTP,
			Reach: resourcev1.ConnectionReach_CONNECTION_REACH_EVERY,
			Smtp:  &resourcev1.ConnectionSmtp{Host: "smtp.example.com", Port: 587, Security: resourcev1.SmtpSecurity_SMTP_SECURITY_STARTTLS, User: "u", Password: "p"},
		}},
		Edit{Ref: chanRef("secret-channel"), Spec: &resourcev1.AlertChannel{To: &resourcev1.AlertChannel_Email{Email: &resourcev1.AlertEmail{
			Connection: "secret-conn", From: "alerts@example.com", To: []string{"ops@example.com"},
		}}}},
	))

	// Corrupt the stored alert channel spec so decoding fails.
	if backend == store.Postgres {
		storetest.Exec(t, loc, `DROP TRIGGER revisions_never_change ON revisions`)
		storetest.Exec(t, loc, `UPDATE revisions SET spec = decode('0a0a', 'hex') WHERE resource_id IN (SELECT id FROM resources WHERE kind = 'alert.channel' AND name = 'secret-channel')`)
	} else {
		storetest.Exec(t, loc, `DROP TRIGGER revisions_never_change`)
		storetest.Exec(t, loc, `UPDATE revisions SET spec = x'0a0a' WHERE resource_id IN (SELECT id FROM resources WHERE kind = 'alert.channel' AND name = 'secret-channel')`)
	}

	linkProp := Proposal{
		Summary: "Link to secret-conn while channel unreadable",
		Edits: []Edit{{
			Ref: linkRef("mail"),
			Spec: &resourcev1.Link{
				Label: "Mail", Connection: "secret-conn", Kind: resourcev1.LinkKind_LINK_KIND_SMTP,
				Sets: []*resourcev1.LinkSet{{Name: "SMTP_HOST", Part: resourcev1.LinkPart_LINK_PART_HOST}},
			},
		}},
	}

	// Admin caller fails closed.
	if _, err := e.Propose(t.Context(), linkProp); err == nil {
		t.Fatal("admin Propose with undecodable channel = nil, want fail-closed error")
	}

	// Caller below install admin fails closed with ErrUnavailable and no channel/connection names.
	devCtx := place.WithCaller(t.Context(), place.Caller{
		Grants: []store.Grant{{ProjectID: proj, Role: store.RoleDeveloper}},
	})
	_, err := e.Propose(devCtx, linkProp)
	if !errors.Is(err, ErrUnavailable) {
		t.Fatalf("non-admin Propose with undecodable channel = %v, want ErrUnavailable", err)
	}
	if strings.Contains(err.Error(), "secret-channel") || strings.Contains(err.Error(), "secret-conn") {
		t.Errorf("non-admin error leaked names: %q", err.Error())
	}

	// M1: S3-only workflows (creating, editing, linking, deleting S3 connections)
	// are completely unaffected by an unreadable alert channel.
	s3Spec := &resourcev1.ConnectionSpec{
		Kind:  resourcev1.LinkKind_LINK_KIND_S3,
		Reach: resourcev1.ConnectionReach_CONNECTION_REACH_EVERY,
		S3:    &resourcev1.ConnectionS3{Endpoint: "https://s3.example.com", Region: "auto", Bucket: "bkt-m1", AccessKeyId: "AKIA", SecretKey: "secret"},
	}
	apply(t, e, propose(t, e, "Create S3 connection and link while alert channel unreadable",
		Edit{Ref: connRef("s3-ok"), Spec: s3Spec},
		Edit{Ref: linkRef("s3-link"), Spec: &resourcev1.Link{
			Label: "Storage", Connection: "s3-ok", Kind: resourcev1.LinkKind_LINK_KIND_S3,
			Sets: []*resourcev1.LinkSet{{Name: "S3_BUCKET", Part: resourcev1.LinkPart_LINK_PART_BUCKET}},
		}},
	))
}

// H3: the same SMTP account (host, user, password) saved under two connection
// names (even with different port, TLS mode, or host case) cannot serve both an
// app and an alert channel, in either order, on rotation, or at Apply time; and
// recognized smtp:// / smtps:// URL secrets block adoption even when the
// connection's port or TLS mode changed since Disconnect.
func TestAlertChannelCrossConnectionAndURLSecrets(t *testing.T) {
	storetest.Each(t, testAlertChannelCrossConnectionAndURLSecrets)
}

func testAlertChannelCrossConnectionAndURLSecrets(t *testing.T, backend store.Backend) {
	db, _ := storetest.Open(t, backend)
	logger := slog.New(slog.DiscardHandler)
	if _, err := Setup(t.Context(), db, logger); err != nil {
		t.Fatal(err)
	}
	kinds := resource.NewRegistry(
		scope.ProjectType{}, scope.EnvironmentType{},
		connection.Type{}, link.Type{}, variable.Type{}, variable.SecretType{},
		alert.Rule{}, alert.Channel{},
	)
	e := New(db, kinds, fleet{}, secretstest.Keeper(db, kinds), logger)
	tr := tree(t, db)
	org := tr.Organization.ID
	proj := tr.Projects[0].ID
	env := tr.Projects[0].Environments[0].ID

	connRef := func(name string) resource.Ref { return resource.Ref{Kind: connection.Kind, Scope: org, Name: name} }
	chanRef := func(name string) resource.Ref { return resource.Ref{Kind: alert.ChannelKind, Scope: org, Name: name} }
	linkRef := func(name string) resource.Ref { return resource.Ref{Kind: link.Kind, Scope: env, Name: name} }
	secRef := func(name string) resource.Ref { return resource.Ref{Kind: variable.SecretKind, Scope: env, Name: name} }
	mailSets := []*resourcev1.LinkSet{{Name: "SMTP_HOST", Part: resourcev1.LinkPart_LINK_PART_HOST}}
	chanOn := func(conn string) *resourcev1.AlertChannel {
		return &resourcev1.AlertChannel{To: &resourcev1.AlertChannel_Email{Email: &resourcev1.AlertEmail{
			Connection: conn, From: "alerts@example.com", To: []string{"ops@example.com"},
		}}}
	}

	// 1. App linked to app-dup; alert-dup holds the same (host, user, password)
	// under different host case, port 465, and TLS. Channel adopting alert-dup is refused.
	apply(t, e, propose(t, e, "Seed app-dup, link, and alert-dup",
		Edit{Ref: connRef("app-dup"), Spec: &resourcev1.ConnectionSpec{
			Kind: resourcev1.LinkKind_LINK_KIND_SMTP, Reach: resourcev1.ConnectionReach_CONNECTION_REACH_EVERY,
			Smtp: &resourcev1.ConnectionSmtp{Host: "smtp.example.com", Port: 587, Security: resourcev1.SmtpSecurity_SMTP_SECURITY_STARTTLS, User: "acct", Password: "shared-acct-pass"},
		}},
		Edit{Ref: linkRef("l-dup"), Spec: &resourcev1.Link{Label: "Mail", Connection: "app-dup", Kind: resourcev1.LinkKind_LINK_KIND_SMTP, Sets: mailSets}},
		Edit{Ref: connRef("alert-dup"), Spec: &resourcev1.ConnectionSpec{
			Kind: resourcev1.LinkKind_LINK_KIND_SMTP,
			Smtp: &resourcev1.ConnectionSmtp{Host: "SMTP.Example.com.", Port: 465, Security: resourcev1.SmtpSecurity_SMTP_SECURITY_TLS, User: "acct", Password: "shared-acct-pass"},
		}},
	))
	if _, err := e.Propose(t.Context(), Proposal{
		Summary: "Adopt duplicate SMTP account",
		Edits:   []Edit{{Ref: chanRef("ch-dup"), Spec: chanOn("alert-dup")}},
	}); err == nil || !strings.Contains(err.Error(), "an app holds the password of connection alert-dup") {
		t.Fatalf("adopt duplicate SMTP account = %v", err)
	}

	// 2. Reverse order: alert channel uses alert-orig; app linking app-alias
	// (same account) is refused, and below admin leaks neither channel nor alert-orig.
	apply(t, e, propose(t, e, "Seed alert-orig, channel, and app-alias",
		Edit{Ref: connRef("alert-orig"), Spec: &resourcev1.ConnectionSpec{
			Kind: resourcev1.LinkKind_LINK_KIND_SMTP,
			Smtp: &resourcev1.ConnectionSmtp{Host: "smtp.example.net", Port: 587, Security: resourcev1.SmtpSecurity_SMTP_SECURITY_STARTTLS, User: "ops", Password: "alert-only-pass"},
		}},
		Edit{Ref: chanRef("ops-orig"), Spec: chanOn("alert-orig")},
		Edit{Ref: connRef("app-alias"), Spec: &resourcev1.ConnectionSpec{
			Kind: resourcev1.LinkKind_LINK_KIND_SMTP, Reach: resourcev1.ConnectionReach_CONNECTION_REACH_EVERY,
			Smtp: &resourcev1.ConnectionSmtp{Host: "smtp.example.net", Port: 465, Security: resourcev1.SmtpSecurity_SMTP_SECURITY_TLS, User: "ops", Password: "alert-only-pass"},
		}},
	))
	devCtx := place.WithCaller(t.Context(), place.Caller{
		Grants: []store.Grant{{ProjectID: proj, Role: store.RoleDeveloper}},
	})
	_, err := e.Propose(devCtx, Proposal{
		Summary: "Link app to alias of alert connection",
		Edits:   []Edit{{Ref: linkRef("l-alias"), Spec: &resourcev1.Link{Label: "Mail", Connection: "app-alias", Kind: resourcev1.LinkKind_LINK_KIND_SMTP, Sets: mailSets}}},
	})
	if err == nil || !strings.Contains(err.Error(), "same mail account credentials") {
		t.Fatalf("link to alias of alert connection = %v", err)
	}
	if strings.Contains(err.Error(), "ops-orig") || strings.Contains(err.Error(), "alert-orig") {
		t.Errorf("non-admin alias refusal leaked names: %q", err.Error())
	}

	// 3. Rotating an already-linked app connection onto alert-orig's credential is
	// refused at Propose AND at Apply time.
	if _, err := e.Propose(t.Context(), Proposal{
		Summary: "Rotate linked app-dup onto alert-orig credentials",
		Edits: []Edit{{Ref: connRef("app-dup"), Spec: &resourcev1.ConnectionSpec{
			Kind: resourcev1.LinkKind_LINK_KIND_SMTP, Reach: resourcev1.ConnectionReach_CONNECTION_REACH_EVERY,
			Smtp: &resourcev1.ConnectionSmtp{Host: "smtp.example.net", Port: 587, Security: resourcev1.SmtpSecurity_SMTP_SECURITY_STARTTLS, User: "ops", Password: "alert-only-pass"},
		}}},
	}); err == nil || !strings.Contains(err.Error(), "same mail account credentials") {
		t.Fatalf("rotate app-dup onto alert-orig credentials = %v", err)
	}

	// Apply-time race: propose rotating app-dup to fresh creds, then alert-orig rotates
	// to those creds before app-dup's plan applies.
	appRotPlan := propose(t, e, "Rotate app-dup to race-pass", Edit{
		Ref: connRef("app-dup"),
		Spec: &resourcev1.ConnectionSpec{
			Kind: resourcev1.LinkKind_LINK_KIND_SMTP, Reach: resourcev1.ConnectionReach_CONNECTION_REACH_EVERY,
			Smtp: &resourcev1.ConnectionSmtp{Host: "smtp.example.net", Port: 587, Security: resourcev1.SmtpSecurity_SMTP_SECURITY_STARTTLS, User: "ops", Password: "race-pass"},
		},
	})
	apply(t, e, propose(t, e, "Rotate alert-orig to race-pass first", Edit{
		Ref: connRef("alert-orig"),
		Spec: &resourcev1.ConnectionSpec{
			Kind: resourcev1.LinkKind_LINK_KIND_SMTP,
			Smtp: &resourcev1.ConnectionSmtp{Host: "smtp.example.net", Port: 587, Security: resourcev1.SmtpSecurity_SMTP_SECURITY_STARTTLS, User: "ops", Password: "race-pass"},
		},
	}))
	if err := e.Apply(t.Context(), appRotPlan.Change); err == nil || !strings.Contains(err.Error(), "same mail account credentials") {
		t.Fatalf("apply-time race rotating app-dup onto alert-orig creds = %v", err)
	}

	// 4. Recognized SMTP URL secret with different port/TLS mode still blocks adoption.
	apply(t, e, propose(t, e, "Seed URL secret with smtps:465 and connection with starttls:587",
		Edit{Ref: secRef("OLD_SMTP_URL"), Spec: &resourcev1.Secret{Value: "smtps://mailer:url-secret-pw@smtp.example.org:465"}},
		Edit{Ref: connRef("url-changed"), Spec: &resourcev1.ConnectionSpec{
			Kind: resourcev1.LinkKind_LINK_KIND_SMTP,
			Smtp: &resourcev1.ConnectionSmtp{Host: "smtp.example.org", Port: 587, Security: resourcev1.SmtpSecurity_SMTP_SECURITY_STARTTLS, User: "mailer", Password: "url-secret-pw"},
		}},
	))
	if _, err := e.Propose(t.Context(), Proposal{
		Summary: "Adopt connection whose password is in changed-port SMTP URL secret",
		Edits:   []Edit{{Ref: chanRef("ch-url"), Spec: chanOn("url-changed")}},
	}); err == nil || !strings.Contains(err.Error(), "an app holds the password of connection url-changed") {
		t.Fatalf("adopt connection with changed-port SMTP URL secret = %v", err)
	}

	// 5. H3 residual: compressed vs expanded IPv6 spellings of the same SMTP host
	// are the same account and cannot serve both an app and an alert channel.
	apply(t, e, propose(t, e, "Seed compressed IPv6 app connection + link and expanded IPv6 connection",
		Edit{Ref: connRef("v6-app"), Spec: &resourcev1.ConnectionSpec{
			Kind: resourcev1.LinkKind_LINK_KIND_SMTP, Reach: resourcev1.ConnectionReach_CONNECTION_REACH_EVERY,
			Smtp: &resourcev1.ConnectionSmtp{Host: "2001:db8::1", Port: 587, Security: resourcev1.SmtpSecurity_SMTP_SECURITY_STARTTLS, User: "v6u", Password: "v6-acct-pass"},
		}},
		Edit{Ref: linkRef("l-v6"), Spec: &resourcev1.Link{Label: "Mail", Connection: "v6-app", Kind: resourcev1.LinkKind_LINK_KIND_SMTP, Sets: mailSets}},
		Edit{Ref: connRef("v6-alert"), Spec: &resourcev1.ConnectionSpec{
			Kind: resourcev1.LinkKind_LINK_KIND_SMTP,
			Smtp: &resourcev1.ConnectionSmtp{Host: "2001:0db8:0000:0000:0000:0000:0000:0001", Port: 465, Security: resourcev1.SmtpSecurity_SMTP_SECURITY_TLS, User: "v6u", Password: "v6-acct-pass"},
		}},
	))
	if _, err := e.Propose(t.Context(), Proposal{
		Summary: "Adopt expanded IPv6 alias of linked app connection",
		Edits:   []Edit{{Ref: chanRef("ch-v6"), Spec: chanOn("v6-alert")}},
	}); err == nil || !strings.Contains(err.Error(), "an app holds the password of connection v6-alert") {
		t.Fatalf("adopt expanded IPv6 alias = %v", err)
	}
}

// M2: converting a connection-backed (or webhook) alert channel to inline SMTP
// with a username requires a password after Keep; supplying a password succeeds,
// and ordinary inline edits that leave the server/user unchanged keep the password.
func TestAlertChannelInlineConversionRequiresPassword(t *testing.T) {
	storetest.Each(t, testAlertChannelInlineConversionRequiresPassword)
}

func testAlertChannelInlineConversionRequiresPassword(t *testing.T, backend store.Backend) {
	db, _ := storetest.Open(t, backend)
	logger := slog.New(slog.DiscardHandler)
	if _, err := Setup(t.Context(), db, logger); err != nil {
		t.Fatal(err)
	}
	kinds := resource.NewRegistry(
		scope.ProjectType{}, scope.EnvironmentType{},
		connection.Type{}, link.Type{}, variable.Type{}, variable.SecretType{},
		alert.Rule{}, alert.Channel{},
	)
	e := New(db, kinds, fleet{}, secretstest.Keeper(db, kinds), logger)
	tr := tree(t, db)
	org := tr.Organization.ID

	postmarkRef := resource.Ref{Kind: connection.Kind, Scope: org, Name: "postmark"}
	opsRef := resource.Ref{Kind: alert.ChannelKind, Scope: org, Name: "ops"}

	apply(t, e, propose(t, e, "Seed mail connection and channel",
		Edit{Ref: postmarkRef, Spec: &resourcev1.ConnectionSpec{
			Kind: resourcev1.LinkKind_LINK_KIND_SMTP,
			Smtp: &resourcev1.ConnectionSmtp{Host: "smtp.postmarkapp.com", Port: 587, Security: resourcev1.SmtpSecurity_SMTP_SECURITY_STARTTLS, User: "u", Password: "p"},
		}},
		Edit{Ref: opsRef, Spec: &resourcev1.AlertChannel{To: &resourcev1.AlertChannel_Email{Email: &resourcev1.AlertEmail{
			Connection: "postmark", From: "alerts@example.com", To: []string{"ops@example.com"},
		}}}},
	))

	// Converting connection-backed -> inline SMTP with username and empty password is refused.
	if _, err := e.Propose(t.Context(), Proposal{
		Summary: "Convert ops to inline without password",
		Edits: []Edit{{Ref: opsRef, Spec: &resourcev1.AlertChannel{To: &resourcev1.AlertChannel_Email{Email: &resourcev1.AlertEmail{
			Host: "smtp.example.com", Username: "alerts", From: "alerts@example.com", To: []string{"ops@example.com"},
		}}}}},
	}); err == nil || !strings.Contains(err.Error(), "signs in as alerts needs its password") {
		t.Fatalf("convert connection->inline without password = %v", err)
	}

	// Supplying the password succeeds, and a follow-up recipient edit keeps it.
	apply(t, e, propose(t, e, "Convert ops to inline with password",
		Edit{Ref: opsRef, Spec: &resourcev1.AlertChannel{To: &resourcev1.AlertChannel_Email{Email: &resourcev1.AlertEmail{
			Host: "smtp.example.com", Username: "alerts", Password: "inline-password", From: "alerts@example.com", To: []string{"ops@example.com"},
		}}}},
	))
	apply(t, e, propose(t, e, "Update inline recipients keeping password",
		Edit{Ref: opsRef, Spec: &resourcev1.AlertChannel{To: &resourcev1.AlertChannel_Email{Email: &resourcev1.AlertEmail{
			Host: "smtp.example.com", Username: "alerts", From: "alerts@example.com", To: []string{"oncall@example.com"},
		}}}},
	))
}
