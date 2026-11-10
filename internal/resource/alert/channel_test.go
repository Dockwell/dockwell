// SPDX-License-Identifier: AGPL-3.0-only

package alert

import (
	"slices"
	"strings"
	"testing"

	"google.golang.org/protobuf/proto"

	resourcev1 "github.com/dockwell/dockwell/gen/dockwell/resource/v1alpha1"
)

func email(e *resourcev1.AlertEmail) *resourcev1.AlertChannel {
	return &resourcev1.AlertChannel{To: &resourcev1.AlertChannel_Email{Email: e}}
}

func ntfy(n *resourcev1.AlertNtfy) *resourcev1.AlertChannel {
	return &resourcev1.AlertChannel{To: &resourcev1.AlertChannel_Ntfy{Ntfy: n}}
}

const botToken = "123456789:AAEexampleexampleexampleexample01"

// Each kind of channel takes what it sends with, fills in what Dockwell can, and
// refuses what can't be sent to, in words that hold nothing that signs in.
func TestEachKindOfChannel(t *testing.T) {
	mail := email(&resourcev1.AlertEmail{Host: " smtp.example.com ", From: "Dockwell <alerts@example.com>", To: []string{"Ops <ops@example.com>"}, Username: "alerts", Password: "s3cret"})
	if err := (Channel{}).Validate(mail); err != nil {
		t.Fatal(err)
	}
	if e := mail.GetEmail(); e.GetHost() != "smtp.example.com" || e.GetPort() != 587 || e.GetSecurity() != resourcev1.AlertEmail_SECURITY_STARTTLS || e.GetTo()[0] != "ops@example.com" {
		t.Errorf("Validate wrote %v", e)
	}
	push := ntfy(&resourcev1.AlertNtfy{Topic: "dockwell-alerts-x7f2"})
	if err := (Channel{}).Validate(push); err != nil || push.GetNtfy().GetServer() != DefaultNtfyServer {
		t.Errorf("an ntfy channel = %v, %v; want ntfy.sh unless it says", push, err)
	}
	for _, ok := range []*resourcev1.AlertChannel{
		{To: &resourcev1.AlertChannel_Slack{Slack: &resourcev1.AlertSlack{Url: "https://hooks.slack.com/services/T000/B000/XXXX"}}},
		{To: &resourcev1.AlertChannel_Discord{Discord: &resourcev1.AlertDiscord{Url: "https://discord.com/api/webhooks/0000/XXXX"}}},
		{To: &resourcev1.AlertChannel_Telegram{Telegram: &resourcev1.AlertTelegram{BotToken: botToken, ChatId: "-1001234567890"}}},
		{To: &resourcev1.AlertChannel_Telegram{Telegram: &resourcev1.AlertTelegram{BotToken: botToken, ChatId: "@example_alerts"}}},
		email(&resourcev1.AlertEmail{Host: "10.0.0.5", Security: resourcev1.AlertEmail_SECURITY_NONE, From: "alerts@example.com", To: []string{"ops@example.com"}}),
		ntfy(&resourcev1.AlertNtfy{Server: "http://ntfy.home.arpa/", Topic: "alerts"}),
	} {
		if err := (Channel{}).Validate(ok); err != nil {
			t.Errorf("Validate(%v) = %v", ok, err)
		}
	}
	for _, tc := range []struct {
		c    *resourcev1.AlertChannel
		says string
	}{
		{&resourcev1.AlertChannel{}, "a webhook, email, ntfy, Slack, Discord or Telegram"},
		{&resourcev1.AlertChannel{To: &resourcev1.AlertChannel_Slack{Slack: &resourcev1.AlertSlack{Url: "http://hooks.slack.com/services/T/B/X"}}}, "a Slack webhook's URL to hooks.slack.com has to use HTTPS"},
		{&resourcev1.AlertChannel{To: &resourcev1.AlertChannel_Discord{Discord: &resourcev1.AlertDiscord{Url: "https://169.254.169.254/latest"}}}, "metadata service"},
		{ntfy(&resourcev1.AlertNtfy{Server: "https://[fe80::1]", Topic: "alerts"}), "as that's a link-local address"},
		{email(&resourcev1.AlertEmail{Host: "metadata.google.internal", From: "a@example.com", To: []string{"b@example.com"}}), "metadata service"},
		{ntfy(&resourcev1.AlertNtfy{Topic: "two words"}), "1 to 64 letters"},
		{ntfy(&resourcev1.AlertNtfy{Server: "https://ntfy.example.com/?topic=x", Topic: "alerts"}), "with the topic apart"},
		{&resourcev1.AlertChannel{To: &resourcev1.AlertChannel_Telegram{Telegram: &resourcev1.AlertTelegram{BotToken: "not-a-token", ChatId: "1"}}}, "its number, a colon and its secret"},
		{&resourcev1.AlertChannel{To: &resourcev1.AlertChannel_Telegram{Telegram: &resourcev1.AlertTelegram{BotToken: botToken}}}, "a Telegram chat is its ID"},
		{email(&resourcev1.AlertEmail{From: "a@example.com", To: []string{"b@example.com"}}), "needs its SMTP server"},
		{email(&resourcev1.AlertEmail{Host: "smtp.example.com:587", From: "a@example.com", To: []string{"b@example.com"}}), "with its port apart"},
		{email(&resourcev1.AlertEmail{Host: "169.254.169.254", From: "a@example.com", To: []string{"b@example.com"}}), "metadata service"},
		{email(&resourcev1.AlertEmail{Host: "smtp.example.com", Security: resourcev1.AlertEmail_SECURITY_NONE, From: "a@example.com", To: []string{"b@example.com"}}), "has to use TLS"},
		{email(&resourcev1.AlertEmail{Host: "10.0.0.5", Security: resourcev1.AlertEmail_SECURITY_NONE, Username: "alerts", From: "a@example.com", To: []string{"b@example.com"}}), "signs in only over TLS"},
		{email(&resourcev1.AlertEmail{Host: "smtp.example.com", To: []string{"b@example.com"}}), "who its emails come from"},
		{email(&resourcev1.AlertEmail{Host: "smtp.example.com", From: "a@example.com"}), "who its emails go to"},
		{email(&resourcev1.AlertEmail{Host: "smtp.example.com", From: "a@example.com", To: []string{"b@example.com\r\nBcc: c@example.com"}}), "isn't an email address"},
		{email(&resourcev1.AlertEmail{Host: "smtp.example.com", From: "a@example.com", To: slices.Repeat([]string{"b@example.com"}, 11)}), "at most 10"},
	} {
		err := (Channel{}).Validate(tc.c)
		if err == nil || !strings.Contains(err.Error(), tc.says) {
			t.Errorf("Validate(%v) = %v, want it to say %q", tc.c, err, tc.says)
		}
		if err != nil && strings.Contains(err.Error(), "AAE") {
			t.Errorf("Validate said what signs in: %v", err)
		}
	}
	for _, tc := range []struct {
		c    *resourcev1.AlertChannel
		says string
	}{
		{&resourcev1.AlertChannel{To: &resourcev1.AlertChannel_Slack{Slack: &resourcev1.AlertSlack{}}}, "incoming webhook's URL"},
		{&resourcev1.AlertChannel{To: &resourcev1.AlertChannel_Discord{Discord: &resourcev1.AlertDiscord{}}}, "under Integrations"},
		{&resourcev1.AlertChannel{To: &resourcev1.AlertChannel_Telegram{Telegram: &resourcev1.AlertTelegram{ChatId: "1"}}}, "@BotFather"},
		{ntfy(&resourcev1.AlertNtfy{}), "needs its topic"},
		{email(&resourcev1.AlertEmail{Host: "smtp.example.com", Username: "alerts"}), "signs in as alerts needs its password"},
	} {
		if err := (Channel{}).CheckNew(tc.c); err == nil || !strings.Contains(err.Error(), tc.says) {
			t.Errorf("CheckNew(%v) = %v, want it to say %q", tc.c, err, tc.says)
		}
	}
	for c, want := range map[*resourcev1.AlertChannel]string{
		mail: "ops@example.com",
		email(&resourcev1.AlertEmail{To: []string{"a@example.com", "b@example.com", "c@example.com"}}): "a@example.com +2",
		push: "ntfy.sh",
		{To: &resourcev1.AlertChannel_Telegram{Telegram: &resourcev1.AlertTelegram{BotToken: botToken, ChatId: "@example_alerts"}}}: "@example_alerts",
		{To: &resourcev1.AlertChannel_Slack{Slack: &resourcev1.AlertSlack{Url: "https://hooks.slack.com/services/T/B/X"}}}:          "hooks.slack.com",
	} {
		if got := Target(c); got != want {
			t.Errorf("Target(%v) = %q, want %q", c, got, want)
		}
	}
}

// An update keeps what signs in while it signs in to the same place, and asks
// for it again once it doesn't, so a password never goes to a server someone has
// just named.
func TestAChannelKeepsWhatSignsInOnlyWhereItWas(t *testing.T) {
	was := email(&resourcev1.AlertEmail{Host: "smtp.example.com", Port: 587, Security: resourcev1.AlertEmail_SECURITY_STARTTLS, Username: "alerts", Password: "s3cret", From: "a@example.com", To: []string{"b@example.com"}})
	is := email(&resourcev1.AlertEmail{Host: "smtp.example.com", Port: 587, Security: resourcev1.AlertEmail_SECURITY_STARTTLS, Username: "alerts", From: "a@example.com", To: []string{"c@example.com"}})
	kept, err := (Channel{}).Keep(was, is)
	if err != nil || kept.Reason != "kept_credentials" || is.GetEmail().GetPassword() != "s3cret" {
		t.Errorf("Keep = %v, %v, leaving %v", kept, err, is)
	}
	for _, moved := range []*resourcev1.AlertEmail{
		{Host: "smtp.example.net", Port: 587, Username: "alerts"},
		{Host: "smtp.example.com", Port: 2525, Username: "alerts"},
		{Host: "smtp.example.com", Port: 587, Username: "someone-else"},
		{Host: "smtp.example.com", Port: 587, Security: resourcev1.AlertEmail_SECURITY_TLS, Username: "alerts"},
	} {
		c := email(moved)
		_, err := (Channel{}).Keep(was, c)
		if err == nil || !strings.Contains(err.Error(), "needs its password again") || c.GetEmail().GetPassword() != "" {
			t.Errorf("Keep(%v) = %v, leaving %q; want it asked for again", moved, err, c.GetEmail().GetPassword())
		}
	}
	// Given again, it goes where it's told.
	again := email(&resourcev1.AlertEmail{Host: "smtp.example.net", Password: "n3w"})
	if _, err := (Channel{}).Keep(was, again); err != nil || again.GetEmail().GetPassword() != "n3w" {
		t.Errorf("a new server with its password = %v, %v", again, err)
	}

	pushWas := ntfy(&resourcev1.AlertNtfy{Server: "https://ntfy.sh", Topic: "dockwell-alerts-x7f2", Token: "tk_example"})
	if _, err := (Channel{}).Keep(pushWas, ntfy(&resourcev1.AlertNtfy{Server: "https://ntfy.example.com"})); err == nil || !strings.Contains(err.Error(), "needs its topic and token again") {
		t.Errorf("moving ntfy's server keeping its topic and token = %v", err)
	}
	same := ntfy(&resourcev1.AlertNtfy{})
	if _, err := (Channel{}).Keep(pushWas, same); err != nil || same.GetNtfy().GetTopic() != "dockwell-alerts-x7f2" || same.GetNtfy().GetToken() != "tk_example" {
		t.Errorf("ntfy on the same server = %v, %v", same, err)
	}

	hookWas := &resourcev1.AlertChannel{To: &resourcev1.AlertChannel_Webhook{Webhook: &resourcev1.AlertWebhook{Url: "https://hooks.example.com/t0ken", Secret: "s3cret"}}}
	elsewhere := &resourcev1.AlertChannel{To: &resourcev1.AlertChannel_Webhook{Webhook: &resourcev1.AlertWebhook{Url: "https://hooks.example.net/hook"}}}
	if _, err := (Channel{}).Keep(hookWas, elsewhere); err == nil || !strings.Contains(err.Error(), "needs its secret again") {
		t.Errorf("a webhook to another host keeping its secret = %v", err)
	}

	// A bot's token goes only to Telegram, whichever chat it's for.
	tgWas := &resourcev1.AlertChannel{To: &resourcev1.AlertChannel_Telegram{Telegram: &resourcev1.AlertTelegram{BotToken: botToken, ChatId: "1"}}}
	tg := &resourcev1.AlertChannel{To: &resourcev1.AlertChannel_Telegram{Telegram: &resourcev1.AlertTelegram{ChatId: "2"}}}
	if _, err := (Channel{}).Keep(tgWas, tg); err != nil || tg.GetTelegram().GetBotToken() != botToken {
		t.Errorf("a Telegram channel to another chat = %v, %v", tg, err)
	}

	// Another type keeps nothing.
	other := &resourcev1.AlertChannel{To: &resourcev1.AlertChannel_Slack{Slack: &resourcev1.AlertSlack{}}}
	if kept, err := (Channel{}).Keep(was, other); err != nil || kept.Reason != "" || other.GetSlack().GetUrl() != "" {
		t.Errorf("a channel of another type = %v, %v", kept, err)
	}
}

// What a list shows of a channel names the fields it keeps sealed, and holds none
// of them.
func TestWhatAListShowsOfAChannel(t *testing.T) {
	c := ntfy(&resourcev1.AlertNtfy{Server: "https://ntfy.sh", Topic: "dockwell-alerts-x7f2", Token: "tk_example"})
	if got := Sealed(c); !slices.Equal(got, []string{"ntfy.topic", "ntfy.token"}) {
		t.Errorf("Sealed = %v", got)
	}
	r := Redacted(c)
	if r.GetNtfy().GetTopic() != "" || r.GetNtfy().GetToken() != "" || r.GetNtfy().GetServer() != "https://ntfy.sh" {
		t.Errorf("Redacted = %v", r)
	}
	if c.GetNtfy().GetTopic() == "" {
		t.Errorf("Redacted changed the channel it was given")
	}
	if got := Sealed(&resourcev1.AlertChannel{}); got != nil {
		t.Errorf("an empty channel's sealed fields = %v", got)
	}
	if !proto.Equal(Redacted(&resourcev1.AlertChannel{}), &resourcev1.AlertChannel{}) {
		t.Errorf("an empty channel redacted isn't empty")
	}
}
