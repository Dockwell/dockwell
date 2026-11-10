// SPDX-License-Identifier: AGPL-3.0-only

package alerts

import (
	"bufio"
	"context"
	"crypto/tls"
	"encoding/base64"
	"encoding/json"
	"io"
	"mime"
	"mime/quotedprintable"
	"net"
	"net/http"
	"net/http/httptest"
	"net/mail"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"
	"unicode/utf8"

	resourcev1 "github.com/dockwell/dockwell/gen/dockwell/resource/v1alpha1"
	"github.com/dockwell/dockwell/internal/store"
)

// sent is what one of this test's far ends got.
type sent struct {
	path, auth string
	body       map[string]any
}

// farEnd answers every request with code and answer, and keeps what it got.
func farEnd(t *testing.T, code int, answer string) (*httptest.Server, *[]sent) {
	t.Helper()
	var mu sync.Mutex
	var got []sent
	srv := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		raw, _ := io.ReadAll(r.Body)
		var body map[string]any
		_ = json.Unmarshal(raw, &body)
		mu.Lock()
		got = append(got, sent{path: r.URL.Path, auth: r.Header.Get("Authorization"), body: body})
		mu.Unlock()
		w.WriteHeader(code)
		_, _ = io.WriteString(w, answer)
	}))
	t.Cleanup(srv.Close)
	return srv, &got
}

// asOwnHost is a client that reaches srv whatever host a URL names, as though srv
// were that service's own, for what Dockwell reads back only from one.
func asOwnHost(srv *httptest.Server) *http.Client {
	tr := srv.Client().Transport.(*http.Transport).Clone()
	tr.TLSClientConfig.InsecureSkipVerify = true
	addr := srv.Listener.Addr().String()
	tr.DialContext = func(ctx context.Context, network, _ string) (net.Conn, error) {
		return (&net.Dialer{}).DialContext(ctx, network, addr)
	}
	return &http.Client{Transport: tr}
}

func firing(severity resourcev1.AlertRule_Severity) Message {
	r := &resourcev1.AlertRule{Condition: resourcev1.AlertRule_CONDITION_SERVICE_DOWN, Severity: severity}
	a := store.Alert{ID: "alt_1", Title: "shop <beta> & co is down on web-1", Detail: "@everyone Its visitors get an error there.", Path: "/apps/svc_1", OpenedAt: t0}
	return Message{Status: "firing", Rule: r, RuleName: "app-down", Alerts: []store.Alert{a}, Panel: "https://panel.example.com", At: t0}
}

// Slack, Discord, ntfy and Telegram each get the message as they take it, and say
// why when they refuse it, from their own hosts alone, with nothing that signs in.
func TestTheChatChannels(t *testing.T) {
	m := firing(resourcev1.AlertRule_SEVERITY_CRITICAL)

	slack, gotSlack := farEnd(t, http.StatusOK, "ok")
	s := &Sender{Client: slack.Client()}
	if r := s.Once(t.Context(), &resourcev1.AlertChannel{To: &resourcev1.AlertChannel_Slack{Slack: &resourcev1.AlertSlack{Url: slack.URL + "/services/T/B/X"}}}, m); !r.OK || r.Said != "200 OK" {
		t.Errorf("Slack says %+v", r)
	}
	// The title and lines as plain text, which nothing in them formats, then the link.
	b := (*gotSlack)[0].body
	blocks, _ := json.Marshal(b["blocks"])
	if want := `[{"text":{"emoji":false,"text":"shop \u003cbeta\u003e \u0026 co is down on web-1","type":"plain_text"},"type":"header"},{"text":{"emoji":false,"text":"@everyone Its visitors get an error there.","type":"plain_text"},"type":"section"},{"elements":[{"text":"\u003chttps://panel.example.com/apps/svc_1|Open in Dockwell\u003e","type":"mrkdwn"}],"type":"context"}]`; string(blocks) != want {
		t.Errorf("Slack got the blocks %s", blocks)
	}
	if b["text"] != "shop &lt;beta&gt; &amp; co is down on web-1" {
		t.Errorf("Slack's notification text is %q", b["text"])
	}
	// Why, from Slack's own host; the status alone from anything else.
	refused, _ := farEnd(t, http.StatusForbidden, "invalid_token")
	s.Client = refused.Client()
	if r := s.Once(t.Context(), &resourcev1.AlertChannel{To: &resourcev1.AlertChannel_Slack{Slack: &resourcev1.AlertSlack{Url: refused.URL + "/services/T/B/s3cret"}}}, m); r.OK || r.Said != "403 Forbidden" {
		t.Errorf("something that isn't Slack refusing says %+v", r)
	}
	s.Client = asOwnHost(refused)
	if r := s.Once(t.Context(), &resourcev1.AlertChannel{To: &resourcev1.AlertChannel_Slack{Slack: &resourcev1.AlertSlack{Url: "https://hooks.slack.com/services/T/B/s3cret"}}}, m); r.OK || r.Said != "403 Forbidden: invalid_token" {
		t.Errorf("Slack refusing says %+v", r)
	}

	discord, gotDiscord := farEnd(t, http.StatusNoContent, "")
	s.Client = discord.Client()
	if r := s.Once(t.Context(), &resourcev1.AlertChannel{To: &resourcev1.AlertChannel_Discord{Discord: &resourcev1.AlertDiscord{Url: discord.URL + "/api/webhooks/1/x"}}}, m); !r.OK {
		t.Errorf("Discord says %+v", r)
	}
	if b := (*gotDiscord)[0].body; !strings.HasPrefix(b["content"].(string), `**shop \<beta\> & co is down on web-1**`+"\n") || len(b["allowed_mentions"].(map[string]any)["parse"].([]any)) != 0 {
		t.Errorf("Discord got %v, want no one mentioned", b)
	}

	push, gotPush := farEnd(t, http.StatusOK, "{}")
	s.Client = push.Client()
	n := &resourcev1.AlertNtfy{Server: push.URL, Topic: "dockwell-alerts-x7f2", Token: "tk_example"}
	if r := s.Once(t.Context(), &resourcev1.AlertChannel{To: &resourcev1.AlertChannel_Ntfy{Ntfy: n}}, m); !r.OK {
		t.Errorf("ntfy says %+v", r)
	}
	got := (*gotPush)[0]
	if got.path != "/" || got.auth != "Bearer tk_example" || got.body["topic"] != "dockwell-alerts-x7f2" || got.body["priority"] != 5.0 || got.body["click"] != "https://panel.example.com/apps/svc_1" || got.body["title"] != "shop <beta> & co is down on web-1" {
		t.Errorf("ntfy got %+v; want the topic in the body, never the URL", got)
	}
	warn := firing(resourcev1.AlertRule_SEVERITY_WARNING)
	_ = s.Once(t.Context(), &resourcev1.AlertChannel{To: &resourcev1.AlertChannel_Ntfy{Ntfy: n}}, warn)
	if (*gotPush)[1].body["priority"] != 4.0 {
		t.Errorf("a warning goes to ntfy at %v, want 4", (*gotPush)[1].body["priority"])
	}
	denied, _ := farEnd(t, http.StatusBadRequest, `{"code":40008,"http":400,"error":"invalid request: message too large"}`)
	s.Client = denied.Client()
	if r := s.Once(t.Context(), &resourcev1.AlertChannel{To: &resourcev1.AlertChannel_Ntfy{Ntfy: &resourcev1.AlertNtfy{Server: denied.URL, Topic: "x"}}}, m); r.Said != "400 Bad Request" {
		t.Errorf("an ntfy server of someone's refusing says %+v, want the status alone", r)
	}
	s.Client = asOwnHost(denied)
	if r := s.Once(t.Context(), &resourcev1.AlertChannel{To: &resourcev1.AlertChannel_Ntfy{Ntfy: &resourcev1.AlertNtfy{Topic: "x"}}}, m); r.Said != "400 Bad Request: invalid request: message too large" {
		t.Errorf("ntfy.sh refusing says %+v", r)
	}
	// What only repeats the status isn't said twice.
	forbidden, _ := farEnd(t, http.StatusForbidden, `{"code":40301,"error":"forbidden"}`)
	s.Client = asOwnHost(forbidden)
	if r := s.Once(t.Context(), &resourcev1.AlertChannel{To: &resourcev1.AlertChannel_Ntfy{Ntfy: &resourcev1.AlertNtfy{Topic: "x"}}}, m); r.Said != "403 Forbidden" {
		t.Errorf("ntfy.sh forbidding says %+v", r)
	}

	bot, gotBot := farEnd(t, http.StatusOK, `{"ok":true}`)
	s.Client, s.Telegram = bot.Client(), bot.URL
	tg := &resourcev1.AlertTelegram{BotToken: "123456789:AAEsecretsecretsecretsecretsecret", ChatId: "-1001234567890"}
	if r := s.Once(t.Context(), &resourcev1.AlertChannel{To: &resourcev1.AlertChannel_Telegram{Telegram: tg}}, m); !r.OK {
		t.Errorf("Telegram says %+v", r)
	}
	if b := (*gotBot)[0]; b.path != "/bot123456789:AAEsecretsecretsecretsecretsecret/sendMessage" || b.body["chat_id"] != "-1001234567890" || !strings.HasPrefix(b.body["text"].(string), "shop <beta> & co is down on web-1\n") {
		t.Errorf("Telegram got %+v", b)
	}
	lost, _ := farEnd(t, http.StatusBadRequest, `{"ok":false,"error_code":400,"description":"Bad Request: chat not found"}`)
	s.Client, s.Telegram = lost.Client(), lost.URL
	r := s.Once(t.Context(), &resourcev1.AlertChannel{To: &resourcev1.AlertChannel_Telegram{Telegram: tg}}, m)
	if r.Said != "400 Bad Request: chat not found" || strings.Contains(r.Said, "AAE") {
		t.Errorf("Telegram refusing says %+v", r)
	}
	s.Client, s.Telegram = nil, "http://127.0.0.1:1"
	if r := s.Once(t.Context(), &resourcev1.AlertChannel{To: &resourcev1.AlertChannel_Telegram{Telegram: tg}}, m); r.OK || strings.Contains(r.Said, "AAE") || r.Said != "connection refused" {
		t.Errorf("Telegram out of reach says %+v, and nothing of the token", r)
	}
}

// Every channel's far end is held to the same rules: nothing goes to a
// link-local address, whatever the channel.
func TestNoChannelReachesLinkLocal(t *testing.T) {
	s := &Sender{}
	m := firing(resourcev1.AlertRule_SEVERITY_WARNING)
	for _, c := range []*resourcev1.AlertChannel{
		{To: &resourcev1.AlertChannel_Webhook{Webhook: &resourcev1.AlertWebhook{Url: "http://169.254.169.254/latest"}}},
		{To: &resourcev1.AlertChannel_Slack{Slack: &resourcev1.AlertSlack{Url: "https://169.254.169.254/services/T/B/X"}}},
		{To: &resourcev1.AlertChannel_Discord{Discord: &resourcev1.AlertDiscord{Url: "https://[fe80::1]/api/webhooks/1/x"}}},
		{To: &resourcev1.AlertChannel_Ntfy{Ntfy: &resourcev1.AlertNtfy{Server: "http://169.254.169.254", Topic: "x"}}},
		{To: &resourcev1.AlertChannel_Email{Email: &resourcev1.AlertEmail{Host: "169.254.169.254", Port: 25, Security: resourcev1.AlertEmail_SECURITY_NONE, From: "a@example.com", To: []string{"b@example.com"}}}},
	} {
		if r := s.Once(t.Context(), c, m); r.OK || !strings.Contains(r.Said, "link-local") {
			t.Errorf("%v says %+v", c, r)
		}
	}
}

// smtpServer is this test's SMTP server: STARTTLS or TLS from the start, AUTH
// PLAIN once it's TLS, and every message it takes.
type smtpServer struct {
	addr     string
	tls      *tls.Config
	starttls bool
	mu       sync.Mutex
	auth     []string
	mail     []string
	rcpt     [][]string
	// mechs is what it signs in with, "PLAIN" unless a test says; refuse answers
	// RCPT for an address; and noTLS answers STARTTLS, when set.
	mechs  string
	refuse map[string]string
	noTLS  string
}

// set changes how the server answers, under its lock.
func (s *smtpServer) set(f func(*smtpServer)) {
	s.mu.Lock()
	defer s.mu.Unlock()
	f(s)
}

func newSMTP(t *testing.T, implicit, starttls bool) (*smtpServer, *tls.Config) {
	t.Helper()
	// httptest's certificate is for 127.0.0.1, and its client trusts it.
	certs := httptest.NewTLSServer(http.NotFoundHandler())
	server := &tls.Config{Certificates: certs.TLS.Certificates, MinVersion: tls.VersionTLS12}
	client := certs.Client().Transport.(*http.Transport).TLSClientConfig.Clone()
	certs.Close()
	var ln net.Listener
	var err error
	if implicit {
		ln, err = tls.Listen("tcp", "127.0.0.1:0", server)
	} else {
		ln, err = net.Listen("tcp", "127.0.0.1:0")
	}
	if err != nil {
		t.Fatal(err)
	}
	s := &smtpServer{addr: ln.Addr().String(), tls: server, starttls: starttls}
	t.Cleanup(func() { _ = ln.Close() })
	go func() {
		for {
			conn, err := ln.Accept()
			if err != nil {
				return
			}
			go s.serve(conn, implicit)
		}
	}()
	return s, client
}

func (s *smtpServer) serve(conn net.Conn, secure bool) {
	defer conn.Close()
	r, w := bufio.NewReader(conn), conn
	say := func(line string) { _, _ = io.WriteString(w, line+"\r\n") }
	say("220 smtp.test ready")
	var to []string
	for {
		line, err := r.ReadString('\n')
		if err != nil {
			return
		}
		cmd := strings.TrimRight(line, "\r\n")
		verb := strings.ToUpper(strings.SplitN(cmd, " ", 2)[0])
		switch verb {
		case "EHLO", "HELO":
			switch {
			case !secure && s.starttls:
				say("250-smtp.test\r\n250 STARTTLS")
			case secure:
				s.mu.Lock()
				mechs := cmpOr(s.mechs, "PLAIN")
				s.mu.Unlock()
				say("250-smtp.test\r\n250 AUTH " + mechs)
			default:
				say("250 smtp.test")
			}
		case "STARTTLS":
			s.mu.Lock()
			no := s.noTLS
			s.mu.Unlock()
			if no != "" {
				say(no)
				continue
			}
			say("220 go ahead")
			tc := tls.Server(conn, s.tls)
			if tc.Handshake() != nil {
				return
			}
			conn, r, w, secure = tc, bufio.NewReader(tc), tc, true
		case "AUTH":
			s.mu.Lock()
			mechs := cmpOr(s.mechs, "PLAIN")
			s.mu.Unlock()
			var got string
			switch {
			case strings.HasPrefix(cmd, "AUTH PLAIN ") && strings.Contains(mechs, "PLAIN"):
				raw, _ := base64.StdEncoding.DecodeString(strings.TrimPrefix(cmd, "AUTH PLAIN "))
				got = string(raw)
			case cmd == "AUTH LOGIN" && strings.Contains(mechs, "LOGIN"):
				var parts []string
				for _, ask := range []string{"VXNlcm5hbWU6", "UGFzc3dvcmQ6"} {
					say("334 " + ask)
					l, err := r.ReadString('\n')
					if err != nil {
						return
					}
					raw, _ := base64.StdEncoding.DecodeString(strings.TrimRight(l, "\r\n"))
					parts = append(parts, string(raw))
				}
				got = "LOGIN " + strings.Join(parts, ":")
			default:
				say("504 5.7.4 Unrecognized authentication type")
				continue
			}
			s.mu.Lock()
			s.auth = append(s.auth, got)
			s.mu.Unlock()
			say("235 ok")
		case "MAIL":
			say("250 ok")
		case "RCPT":
			addr := strings.TrimSuffix(strings.TrimPrefix(cmd, "RCPT TO:<"), ">")
			s.mu.Lock()
			no := s.refuse[addr]
			s.mu.Unlock()
			if no != "" {
				say(no)
				continue
			}
			to = append(to, addr)
			say("250 ok")
		case "DATA":
			say("354 go on")
			var b strings.Builder
			for {
				l, err := r.ReadString('\n')
				if err != nil || l == ".\r\n" {
					break
				}
				b.WriteString(l)
			}
			s.mu.Lock()
			s.mail, s.rcpt = append(s.mail, b.String()), append(s.rcpt, to)
			s.mu.Unlock()
			say("250 queued")
		case "QUIT":
			say("221 bye")
			return
		default:
			say("502 not here")
		}
	}
}

// An email goes over TLS, verifying the server's certificate, signs in only over
// TLS, and has a subject no title can break out of.
func TestAnEmail(t *testing.T) {
	m := firing(resourcev1.AlertRule_SEVERITY_CRITICAL)
	m.Alerts[0].Title = "shop is down on wéb-1\r\nBcc: someone@example.com"
	for _, implicit := range []bool{false, true} {
		srv, roots := newSMTP(t, implicit, true)
		host, port, _ := net.SplitHostPort(srv.addr)
		security := resourcev1.AlertEmail_SECURITY_STARTTLS
		if implicit {
			security = resourcev1.AlertEmail_SECURITY_TLS
		}
		e := &resourcev1.AlertEmail{Host: host, Port: mustPort(t, port), Security: security, Username: "alerts", Password: "s3cret", From: "Dockwell <alerts@example.com>", To: []string{"ops@example.com", "oncall@example.com"}}
		s := &Sender{TLS: roots}
		if r := s.Once(t.Context(), &resourcev1.AlertChannel{To: &resourcev1.AlertChannel_Email{Email: e}}, m); !r.OK || r.Said != "accepted by 127.0.0.1" {
			t.Fatalf("an email over %v says %+v", security, r)
		}
		srv.mu.Lock()
		if len(srv.auth) != 1 || srv.auth[0] != "\x00alerts\x00s3cret" || strings.Join(srv.rcpt[0], ",") != "ops@example.com,oncall@example.com" {
			t.Errorf("the server signed in %q and sent to %v", srv.auth, srv.rcpt)
		}
		msg, err := mail.ReadMessage(strings.NewReader(srv.mail[0]))
		srv.mu.Unlock()
		if err != nil {
			t.Fatal(err)
		}
		subject, _ := new(mime.WordDecoder).DecodeHeader(msg.Header.Get("Subject"))
		if subject != "shop is down on wéb-1 Bcc: someone@example.com" || msg.Header.Get("Bcc") != "" || msg.Header.Get("Auto-Submitted") != "auto-generated" {
			t.Errorf("the email's headers are %v, its subject %q", msg.Header, subject)
		}
		body, _ := io.ReadAll(quotedprintable.NewReader(msg.Body))
		if !strings.Contains(string(body), "@everyone Its visitors get an error there.\r\nhttps://panel.example.com/apps/svc_1") {
			t.Errorf("the email says %q", body)
		}
	}

	// A server that doesn't offer STARTTLS gets nothing, its password least of all.
	plain, roots := newSMTP(t, false, false)
	host, port, _ := net.SplitHostPort(plain.addr)
	e := &resourcev1.AlertEmail{Host: host, Port: mustPort(t, port), Username: "alerts", Password: "s3cret", From: "alerts@example.com", To: []string{"ops@example.com"}}
	if r := (&Sender{TLS: roots}).Once(t.Context(), &resourcev1.AlertChannel{To: &resourcev1.AlertChannel_Email{Email: e}}, m); r.OK || !strings.Contains(r.Said, "doesn't offer STARTTLS") {
		t.Errorf("a server without STARTTLS says %+v", r)
	}
	// One whose certificate isn't trusted gets nothing either.
	untrusted, _ := newSMTP(t, false, true)
	host, port, _ = net.SplitHostPort(untrusted.addr)
	e = &resourcev1.AlertEmail{Host: host, Port: mustPort(t, port), Username: "alerts", Password: "s3cret", From: "alerts@example.com", To: []string{"ops@example.com"}}
	if r := (&Sender{}).Once(t.Context(), &resourcev1.AlertChannel{To: &resourcev1.AlertChannel_Email{Email: e}}, m); r.OK || !strings.Contains(r.Said, "certificate isn't valid") {
		t.Errorf("a server with a certificate no one trusts says %+v", r)
	}
	for _, srv := range []*smtpServer{plain, untrusted} {
		srv.mu.Lock()
		if len(srv.auth) != 0 || len(srv.mail) != 0 {
			t.Errorf("a server Dockwell shouldn't trust got %q and %d emails", srv.auth, len(srv.mail))
		}
		srv.mu.Unlock()
	}
}

func cmpOr(a, b string) string {
	if a != "" {
		return a
	}
	return b
}

// One address the server refuses, as someone's who left, keeps the message from no
// one else; it goes to the rest, and says which it refused. Only when it refuses
// every one is nothing sent.
func TestAnEmailGoesToWhomeverTheServerTakes(t *testing.T) {
	m := firing(resourcev1.AlertRule_SEVERITY_WARNING)
	srv, roots := newSMTP(t, true, false)
	srv.set(func(s *smtpServer) {
		s.refuse = map[string]string{"gone@example.com": "550 5.1.1 gone@example.com: mailbox unavailable"}
	})
	host, port, _ := net.SplitHostPort(srv.addr)
	e := &resourcev1.AlertEmail{Host: host, Port: mustPort(t, port), Security: resourcev1.AlertEmail_SECURITY_TLS, From: "alerts@example.com", To: []string{"gone@example.com", "ops@example.com", "oncall@example.com"}}
	c := &resourcev1.AlertChannel{To: &resourcev1.AlertChannel_Email{Email: e}}
	r := (&Sender{TLS: roots}).Once(t.Context(), c, m)
	if !r.OK || r.Said != "accepted by 127.0.0.1; it refused gone@example.com (550 5.1.1 gone@example.com: mailbox unavailable)" {
		t.Errorf("one address refused says %+v", r)
	}
	srv.mu.Lock()
	if len(srv.mail) != 1 || strings.Join(srv.rcpt[0], ",") != "ops@example.com,oncall@example.com" {
		t.Errorf("the server took %d messages, for %v", len(srv.mail), srv.rcpt)
	}
	srv.mu.Unlock()

	e.To = []string{"gone@example.com"}
	if r := (&Sender{TLS: roots}).Once(t.Context(), c, m); r.OK || !strings.HasPrefix(r.Said, "the server refused every address: gone@example.com (550") {
		t.Errorf("every address refused says %+v", r)
	}
}

// A server that signs in with LOGIN alone, as Exchange's client front end does,
// gets the user and the password as it asks; one with neither PLAIN nor LOGIN
// gets nothing, and the words say what it offers.
func TestAnEmailSignsInWithLogin(t *testing.T) {
	m := firing(resourcev1.AlertRule_SEVERITY_WARNING)
	srv, roots := newSMTP(t, true, false)
	srv.set(func(s *smtpServer) { s.mechs = "GSSAPI NTLM LOGIN" })
	host, port, _ := net.SplitHostPort(srv.addr)
	e := &resourcev1.AlertEmail{Host: host, Port: mustPort(t, port), Security: resourcev1.AlertEmail_SECURITY_TLS, Username: "alerts", Password: "s3cret", From: "alerts@example.com", To: []string{"ops@example.com"}}
	c := &resourcev1.AlertChannel{To: &resourcev1.AlertChannel_Email{Email: e}}
	if r := (&Sender{TLS: roots}).Once(t.Context(), c, m); !r.OK {
		t.Errorf("signing in with LOGIN says %+v", r)
	}
	srv.mu.Lock()
	if len(srv.auth) != 1 || srv.auth[0] != "LOGIN alerts:s3cret" || len(srv.mail) != 1 {
		t.Errorf("the server heard %q and took %d messages", srv.auth, len(srv.mail))
	}
	srv.mu.Unlock()

	srv.set(func(s *smtpServer) { s.mechs = "GSSAPI NTLM" })
	if r := (&Sender{TLS: roots}).Once(t.Context(), c, m); r.OK || r.Said != "the server signs in with GSSAPI, NTLM, and Dockwell with PLAIN or LOGIN, so it sent nothing" {
		t.Errorf("a server with neither says %+v", r)
	}
}

// A server that offers STARTTLS and then refuses it says what it answered: it's
// there, and nothing was sent.
func TestAStartTLSRefusedSaysWhatTheServerSaid(t *testing.T) {
	srv, roots := newSMTP(t, false, true)
	srv.set(func(s *smtpServer) { s.noTLS = "454 4.7.0 TLS not available due to temporary reason" })
	host, port, _ := net.SplitHostPort(srv.addr)
	e := &resourcev1.AlertEmail{Host: host, Port: mustPort(t, port), Username: "alerts", Password: "s3cret", From: "alerts@example.com", To: []string{"ops@example.com"}}
	r := (&Sender{TLS: roots}).Once(t.Context(), &resourcev1.AlertChannel{To: &resourcev1.AlertChannel_Email{Email: e}}, firing(resourcev1.AlertRule_SEVERITY_WARNING))
	if r.OK || r.Said != "454 4.7.0 TLS not available due to temporary reason" {
		t.Errorf("STARTTLS refused says %+v", r)
	}
}

// Without TLS, a connection goes only to this server or its private networks,
// whatever a name led to: an address on the internet gets nothing in the clear.
func TestWithoutTLSNothingCrossesTheInternet(t *testing.T) {
	m := firing(resourcev1.AlertRule_SEVERITY_WARNING)
	for _, c := range []*resourcev1.AlertChannel{
		{To: &resourcev1.AlertChannel_Slack{Slack: &resourcev1.AlertSlack{Url: "http://203.0.113.10/services/T/B/X"}}},
		{To: &resourcev1.AlertChannel_Webhook{Webhook: &resourcev1.AlertWebhook{Url: "http://203.0.113.10/hook"}}},
		{To: &resourcev1.AlertChannel_Email{Email: &resourcev1.AlertEmail{Host: "203.0.113.10", Port: 25, Security: resourcev1.AlertEmail_SECURITY_NONE, From: "a@example.com", To: []string{"b@example.com"}}}},
	} {
		if r := (&Sender{}).Once(t.Context(), c, m); r.OK || r.Said != "203.0.113.10 is on the internet, where nothing goes without TLS, so Dockwell sends nothing there" {
			t.Errorf("%v without TLS says %+v", c, r)
		}
	}
}

// What a far end says holds nothing that signs in to its channel, whatever it
// echoes, as a read-only token reads deliveries.
func TestWhatAFarEndSaysHoldsNothingThatSignsIn(t *testing.T) {
	echo := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusNotFound)
		_, _ = io.WriteString(w, `{"message":"Unknown Webhook `+r.URL.Path+`"}`)
	}))
	t.Cleanup(echo.Close)
	c := &resourcev1.AlertChannel{To: &resourcev1.AlertChannel_Discord{Discord: &resourcev1.AlertDiscord{Url: "https://discord.com/api/webhooks/1/SEKRIT-stored-token"}}}
	r := (&Sender{Client: asOwnHost(echo)}).Once(t.Context(), c, firing(resourcev1.AlertRule_SEVERITY_WARNING))
	if r.OK || strings.Contains(r.Said, "SEKRIT") || r.Said != "404 Not Found: Unknown Webhook …" {
		t.Errorf("an echo of the URL says %q", r.Said)
	}
}

// A far end that asks Dockwell to slow down says how long, in its header or its
// answer, which isn't a failure.
func TestAFarEndThatLimitsSaysHowLong(t *testing.T) {
	m := firing(resourcev1.AlertRule_SEVERITY_WARNING)
	header := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Retry-After", "120")
		w.WriteHeader(http.StatusTooManyRequests)
	}))
	t.Cleanup(header.Close)
	c := &resourcev1.AlertChannel{To: &resourcev1.AlertChannel_Slack{Slack: &resourcev1.AlertSlack{Url: header.URL + "/services/T/B/X"}}}
	if r := (&Sender{Client: header.Client()}).Once(t.Context(), c, m); r.OK || !r.Limited || r.RetryAfter != 2*time.Minute {
		t.Errorf("a Retry-After of 120 reads %+v", r)
	}
	bot, _ := farEnd(t, http.StatusTooManyRequests, `{"ok":false,"error_code":429,"description":"Too Many Requests: retry after 35","parameters":{"retry_after":35}}`)
	tg := &resourcev1.AlertChannel{To: &resourcev1.AlertChannel_Telegram{Telegram: &resourcev1.AlertTelegram{BotToken: "123456789:AAEsecretsecretsecretsecretsecret", ChatId: "-1001234567890"}}}
	r := (&Sender{Client: bot.Client(), Telegram: bot.URL}).Once(t.Context(), tg, m)
	if !r.Limited || r.RetryAfter != 35*time.Second || r.Said != "429 Too Many Requests: retry after 35" {
		t.Errorf("Telegram's retry_after reads %+v", r)
	}
}

// Markup in what someone else wrote, such as a certificate authority's error, is
// inert: Discord shows a link's words, and Slack's blocks are plain text.
func TestMarkupInAnAlertIsInert(t *testing.T) {
	m := firing(resourcev1.AlertRule_SEVERITY_WARNING)
	m.Alerts[0].Title = "web_1's *disk* ~is~ `full`"
	m.Alerts[0].Detail = "[Reset your Dockwell password](https://evil.example/reset)\n# Heading"
	discord, got := farEnd(t, http.StatusNoContent, "")
	_ = (&Sender{Client: discord.Client()}).Once(t.Context(), &resourcev1.AlertChannel{To: &resourcev1.AlertChannel_Discord{Discord: &resourcev1.AlertDiscord{Url: discord.URL + "/x"}}}, m)
	content, _ := (*got)[0].body["content"].(string)
	if !strings.HasPrefix(content, "**web\\_1's \\*disk\\* \\~is\\~ \\`full\\`**\n\\[Reset your Dockwell password\\]\\(https://evil.example/reset\\)") {
		t.Errorf("Discord got %q", content)
	}
	slack, gotSlack := farEnd(t, http.StatusOK, "ok")
	_ = (&Sender{Client: slack.Client()}).Once(t.Context(), &resourcev1.AlertChannel{To: &resourcev1.AlertChannel_Slack{Slack: &resourcev1.AlertSlack{Url: slack.URL + "/x"}}}, m)
	if text, _ := (*gotSlack)[0].body["text"].(string); text != "web_\u200b1's *\u200bdisk*\u200b ~\u200bis~\u200b `\u200bfull`\u200b" {
		t.Errorf("Slack's notification text is %q", text)
	}
}

// A message too long for a far end leaves whole lines out, says so, and keeps its
// link: Discord's 2000 characters, ntfy's 4096 bytes, and Telegram's 4096 UTF-16
// units.
func TestALongMessageKeepsItsLink(t *testing.T) {
	m := firing(resourcev1.AlertRule_SEVERITY_WARNING)
	m.Alerts = nil
	for i := range 300 {
		m.Alerts = append(m.Alerts, store.Alert{ID: "a", Title: "app-" + strconv.Itoa(i) + " 🚑 isn't running on web-1", Path: "/apps/x"})
	}
	discord, got := farEnd(t, http.StatusNoContent, "")
	_ = (&Sender{Client: discord.Client()}).Once(t.Context(), &resourcev1.AlertChannel{To: &resourcev1.AlertChannel_Discord{Discord: &resourcev1.AlertDiscord{Url: discord.URL + "/x"}}}, m)
	content, _ := (*got)[0].body["content"].(string)
	if utf8.RuneCountInString(content) > discordMax || !strings.HasSuffix(content, "\n…\nhttps://panel.example.com/alerts") {
		t.Errorf("Discord got %d characters, ending %q", utf8.RuneCountInString(content), content[max(len(content)-60, 0):])
	}
	push, gotPush := farEnd(t, http.StatusOK, "{}")
	_ = (&Sender{Client: push.Client()}).Once(t.Context(), &resourcev1.AlertChannel{To: &resourcev1.AlertChannel_Ntfy{Ntfy: &resourcev1.AlertNtfy{Server: push.URL, Topic: "x"}}}, m)
	if msg, _ := (*gotPush)[0].body["message"].(string); len(msg) > ntfyMax || !strings.HasSuffix(msg, "\n…") {
		t.Errorf("ntfy got %d bytes", len(msg))
	}
	bot, gotBot := farEnd(t, http.StatusOK, `{"ok":true}`)
	tg := &resourcev1.AlertChannel{To: &resourcev1.AlertChannel_Telegram{Telegram: &resourcev1.AlertTelegram{BotToken: "123456789:AAEsecretsecretsecretsecretsecret", ChatId: "-1001234567890"}}}
	_ = (&Sender{Client: bot.Client(), Telegram: bot.URL}).Once(t.Context(), tg, m)
	if text, _ := (*gotBot)[0].body["text"].(string); utf16Len(text) > telegramMax || !strings.HasSuffix(text, "\nhttps://panel.example.com/alerts") {
		t.Errorf("Telegram got %d UTF-16 units", utf16Len(text))
	}
}

func mustPort(t *testing.T, p string) uint32 {
	t.Helper()
	var n uint32
	for _, c := range p {
		n = n*10 + uint32(c-'0')
	}
	return n
}

// A test of a channel of any type never reaches this server's own addresses, with
// TLS or without, whatever the channel may; the outbox does, as a channel planned
// to one asks.
func TestATestOfAnyTypeNeverReachesLoopback(t *testing.T) {
	m := Message{Status: "test", At: t0}
	srv, roots := newSMTP(t, false, false)
	host, port, _ := net.SplitHostPort(srv.addr)
	email := &resourcev1.AlertChannel{To: &resourcev1.AlertChannel_Email{Email: &resourcev1.AlertEmail{Host: host, Port: mustPort(t, port), Security: resourcev1.AlertEmail_SECURITY_NONE, From: "a@example.com", To: []string{"b@example.com"}}}}
	slack, got := farEnd(t, http.StatusOK, "ok")
	plain := httptest.NewServer(http.NotFoundHandler())
	t.Cleanup(plain.Close)
	for _, c := range []*resourcev1.AlertChannel{
		email,
		{To: &resourcev1.AlertChannel_Slack{Slack: &resourcev1.AlertSlack{Url: slack.URL + "/services/T/B/X"}}},
		{To: &resourcev1.AlertChannel_Ntfy{Ntfy: &resourcev1.AlertNtfy{Server: plain.URL, Topic: "x"}}},
	} {
		if r := (&Sender{TLS: roots}).Test(t.Context(), c, m); r.OK || !strings.Contains(r.Said, "is this server's own address, so Dockwell sends nothing there") {
			t.Errorf("a test of %v says %+v", c, r)
		}
	}
	srv.mu.Lock()
	if len(srv.mail) != 0 || len(*got) != 0 {
		t.Errorf("a test reached this server: %d emails, %d posts", len(srv.mail), len(*got))
	}
	srv.mu.Unlock()
	if r := (&Sender{TLS: roots}).Once(t.Context(), email, m); !r.OK {
		t.Errorf("the outbox to this server's SMTP says %+v", r)
	}
}
