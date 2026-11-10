// SPDX-License-Identifier: AGPL-3.0-only

package alerts

import (
	"bytes"
	"context"
	"crypto/hmac"
	"crypto/rand"
	"crypto/sha256"
	"crypto/tls"
	"encoding/hex"
	"encoding/json"
	"errors"
	"io"
	"mime"
	"mime/quotedprintable"
	"net"
	"net/http"
	"net/mail"
	"net/smtp"
	"net/textproto"
	"net/url"
	"slices"
	"strconv"
	"strings"
	"time"
	"unicode/utf8"

	"google.golang.org/protobuf/reflect/protoreflect"

	resourcev1 "github.com/dockwell/dockwell/gen/dockwell/resource/v1alpha1"
	"github.com/dockwell/dockwell/internal/buildinfo"
	"github.com/dockwell/dockwell/internal/netguard"
	"github.com/dockwell/dockwell/internal/resource"
	"github.com/dockwell/dockwell/internal/resource/alert"
)

// TelegramAPI is where a Telegram channel's bot sends.
const TelegramAPI = "https://api.telegram.org"

// What a far end takes in one message, past which Dockwell leaves lines out:
// Discord's in characters, ntfy's in bytes (past it, ntfy.sh makes the message an
// attachment), Telegram's in UTF-16 units, and Slack's header and section blocks'.
const (
	discordMax      = 2000
	ntfyMax         = 4096
	telegramMax     = 4096
	slackHeaderMax  = 150
	slackSectionMax = 3000
	maxRetryAfter   = time.Hour
)

// ownHosts are where each kind's own service answers: only what those say of why
// they refused a message is read back. Anything else answers with a status alone,
// as a webhook does, so a test can't read an internal service's errors.
var ownHosts = map[string][]string{
	"slack":   {"hooks.slack.com"},
	"discord": {"discord.com", "discordapp.com", "ptb.discord.com", "canary.discord.com"},
	"ntfy":    {"ntfy.sh"},
}

// Sender sends messages to channels, one try at a time: the outbox tries again
// what doesn't land (ADR 0044). Every request it makes to an address a person
// gave goes through one guard: no link-local or metadata address, whatever a
// name resolves to, checked as it connects; no proxy; no redirect followed; a
// timeout; and no more than 64 KiB read back (docs/security/alerts.md).
type Sender struct {
	// Client sends over HTTP. Nil is one that dials no link-local address, and
	// goes through no proxy.
	Client *http.Client
	// TLS is how email channels check their server's certificate. Nil is the
	// system's roots.
	TLS *tls.Config
	// Telegram is where Telegram's API answers. Empty is TelegramAPI.
	Telegram string
}

// Result is how one message to a channel went, in the words a delivery keeps.
type Result struct {
	OK   bool
	Said string
	// Limited says the far end asked Dockwell to slow down (429), which isn't a
	// failure: the outbox waits RetryAfter, when it said how long, and goes again.
	Limited    bool
	RetryAfter time.Duration
}

// Once sends m to a channel once, as the outbox does each try, and a test. What
// it says holds nothing of what signs in to the channel, whatever the far end
// echoed.
func (s *Sender) Once(ctx context.Context, c *resourcev1.AlertChannel, m Message) Result {
	r := s.once(ctx, c, m)
	r.Said = scrub(r.Said, c)
	return r
}

func (s *Sender) once(ctx context.Context, c *resourcev1.AlertChannel, m Message) Result {
	ctx, cancel := context.WithTimeout(ctx, SendTimeout)
	defer cancel()
	switch to := c.GetTo().(type) {
	case *resourcev1.AlertChannel_Webhook:
		return s.webhook(ctx, to.Webhook, m)
	case *resourcev1.AlertChannel_Slack:
		return s.slack(ctx, to.Slack, m)
	case *resourcev1.AlertChannel_Discord:
		return s.discord(ctx, to.Discord, m)
	case *resourcev1.AlertChannel_Ntfy:
		return s.ntfy(ctx, to.Ntfy, m)
	case *resourcev1.AlertChannel_Telegram:
		return s.telegram(ctx, to.Telegram, m)
	case *resourcev1.AlertChannel_Email:
		return s.email(ctx, to.Email, m)
	default:
		return Result{Said: "this channel sends nowhere Dockwell knows of"}
	}
}

// Signature is what signs a webhook's body: when it was sent, and the HMAC-SHA256,
// under the channel's secret, of that time, a dot, and the body.
func Signature(secret string, at time.Time, body []byte) string {
	t := strconv.FormatInt(at.Unix(), 10)
	mac := hmac.New(sha256.New, []byte(secret))
	mac.Write([]byte(t + "."))
	mac.Write(body)
	return "t=" + t + ",v1=" + hex.EncodeToString(mac.Sum(nil))
}

// explain says, from what the far end answered to a request it didn't take, why,
// or "" when its answer says nothing a person could act on.
type explain func(body []byte) string

// post sends JSON to a URL, and says how it went: the status, and why when the
// far end refused it and explain finds a reason.
func (s *Sender) post(ctx context.Context, rawURL string, v any, header http.Header, why explain) Result {
	body, err := json.Marshal(v)
	if err != nil {
		return Result{Said: "Dockwell couldn't write the message"}
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, rawURL, bytes.NewReader(body))
	if err != nil {
		return Result{Said: "its URL isn't one to send to"}
	}
	for k, vs := range header {
		req.Header[k] = vs
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("User-Agent", "Dockwell/"+buildinfo.Read().Version)
	resp, err := s.do(req)
	if err != nil {
		return Result{Said: sendWords(err)}
	}
	defer resp.Body.Close()
	answer, _ := io.ReadAll(io.LimitReader(resp.Body, 64<<10))
	if resp.StatusCode >= 200 && resp.StatusCode < 300 {
		return Result{OK: true, Said: clip(resp.Status)}
	}
	r := Result{Said: clip(resp.Status)}
	if resp.StatusCode == http.StatusTooManyRequests {
		r.Limited, r.RetryAfter = true, retryAfterOf(resp.Header, answer)
	}
	if why != nil {
		if reason := tidyReason(why(answer), resp.StatusCode); reason != "" {
			r.Said = clip(resp.Status + ": " + reason)
		}
	}
	return r
}

// tidyReason drops what repeats the status, as Telegram's "Bad Request: chat not
// found" does after "400 Bad Request".
func tidyReason(reason string, code int) string {
	reason = strings.TrimSpace(reason)
	status := http.StatusText(code)
	if strings.EqualFold(reason, status) {
		return ""
	}
	if rest, ok := strings.CutPrefix(reason, status+": "); ok {
		return rest
	}
	return reason
}

// retryAfterOf is how long a far end that's limiting asked Dockwell to wait: its
// Retry-After header, or Discord's or Telegram's retry_after in its answer.
func retryAfterOf(h http.Header, body []byte) time.Duration {
	var d time.Duration
	if v := strings.TrimSpace(h.Get("Retry-After")); v != "" {
		if n, err := strconv.Atoi(v); err == nil {
			d = time.Duration(n) * time.Second
		} else if at, err := http.ParseTime(v); err == nil {
			d = time.Until(at)
		}
	}
	var v struct {
		RetryAfter float64 `json:"retry_after"`
		Parameters struct {
			RetryAfter float64 `json:"retry_after"`
		} `json:"parameters"`
	}
	if d <= 0 && json.Unmarshal(body, &v) == nil {
		d = time.Duration(max(v.RetryAfter, v.Parameters.RetryAfter) * float64(time.Second))
	}
	return min(max(d, 0), maxRetryAfter)
}

// onOwnHost reports a URL to one of a kind's own services, over HTTPS.
func onOwnHost(raw, kind string) bool {
	u, err := url.Parse(raw)
	return err == nil && u.Scheme == "https" && slices.Contains(ownHosts[kind], strings.ToLower(u.Hostname()))
}

// ownReason is why, from a kind's own service alone.
func ownReason(raw, kind string, why explain) explain {
	if onOwnHost(raw, kind) {
		return why
	}
	return nil
}

// do sends a request through the guarded client: a test's or the outbox's, and
// one that reaches only this server and its private networks when the URL has no
// TLS.
func (s *Sender) do(req *http.Request) (*http.Response, error) {
	if s.Client != nil {
		return s.Client.Do(req)
	}
	return guardFor(req.Context(), req.URL.Scheme == "http").client.Do(req)
}

// Test sends a test to a channel once, as a person asks: never to this server's
// own addresses, whatever the channel may reach, so a test, which shows what
// answered, can't read what listens on the control server's loopback.
func (s *Sender) Test(ctx context.Context, c *resourcev1.AlertChannel, m Message) Result {
	return s.Once(context.WithValue(ctx, aTest{}, true), c, m)
}

// aTest marks a send that's a test (Sender.Test).
type aTest struct{}

func isTest(ctx context.Context) bool {
	v, _ := ctx.Value(aTest{}).(bool)
	return v
}

// scrub takes out of what a far end said each value of the channel's that signs
// in, and a URL's path and query, as one that echoes its URL would otherwise show
// them to whoever reads its deliveries.
func scrub(said string, c *resourcev1.AlertChannel) string {
	for _, v := range sealedValues(c) {
		said = strings.ReplaceAll(said, v, "…")
	}
	return said
}

// sealedValues are what of a channel signs in, longest first, with each URL's path,
// query and the longer parts of its path.
func sealedValues(c *resourcev1.AlertChannel) []string {
	var out []string
	var walk func(m protoreflect.Message)
	walk = func(m protoreflect.Message) {
		m.Range(func(fd protoreflect.FieldDescriptor, v protoreflect.Value) bool {
			switch {
			case fd.Message() != nil && !fd.IsList() && !fd.IsMap():
				walk(v.Message())
			case resource.IsSensitive(fd) && fd.Kind() == protoreflect.StringKind && !fd.IsList() && !fd.IsMap():
				out = append(out, v.String())
				if u, err := url.Parse(v.String()); err == nil && u.Host != "" {
					out = append(out, u.EscapedPath(), u.Path, u.RawQuery)
					for _, part := range strings.Split(u.Path, "/") {
						if len(part) >= 8 {
							out = append(out, part)
						}
					}
				}
			}
			return true
		})
	}
	walk(c.ProtoReflect())
	out = slices.DeleteFunc(out, func(v string) bool { return len(v) < 6 })
	slices.SortFunc(out, func(a, b string) int { return len(b) - len(a) })
	return slices.Compact(out)
}

// guarded is netguard's client and dialer for what a send may reach (ADR 0044):
// never a link-local or metadata address, checked as it connects, whatever a name
// resolved to; no proxy from the environment, as the dial check would see the
// proxy alone; no redirect, as a far end that moves says so in its answer; 10
// seconds; and 64 KiB of an answer at most.
type guarded struct {
	client *netguard.HTTP
	dial   netguard.DialFunc
}

// The outbox's guard reaches what a channel may (alert.Guard), and a test's what
// a test may (alert.TestGuard), never this server's own addresses; each has one
// without TLS too, which reaches only this server and its private networks,
// whatever a name led to.
var (
	outboxGuard      = guardedBy(alert.Guard, false)
	outboxPlainGuard = guardedBy(alert.Guard, true)
	testGuard        = guardedBy(alert.TestGuard, false)
	testPlainGuard   = guardedBy(alert.TestGuard, true)
)

func guardedBy(o netguard.Options, plain bool) guarded {
	o.Local, o.MaxBody, o.Timeout = plain, 64<<10, SendTimeout
	return guarded{client: netguard.Client(o), dial: netguard.Dialer(o)}
}

// guardFor is the guard a send goes through: a test's or the outbox's, without TLS
// or with it.
func guardFor(ctx context.Context, plain bool) guarded {
	switch {
	case isTest(ctx) && plain:
		return testPlainGuard
	case isTest(ctx):
		return testGuard
	case plain:
		return outboxPlainGuard
	}
	return outboxGuard
}

func (s *Sender) webhook(ctx context.Context, w *resourcev1.AlertWebhook, m Message) Result {
	p := m.payload()
	body, err := json.Marshal(p)
	if err != nil {
		return Result{Said: "Dockwell couldn't write the message"}
	}
	header := http.Header{}
	if w.GetSecret() != "" {
		header.Set("Dockwell-Signature", Signature(w.GetSecret(), m.At, body))
	}
	// What a webhook answers is its own, and may say what signs in to it, so only
	// its status is kept.
	return s.post(ctx, w.GetUrl(), json.RawMessage(body), header, nil)
}

// slackEscape is what Slack reads as markup in text: < > & always, and the
// characters that format, each made inert with a zero-width space after it, as
// Slack's markup has no escape for them.
var slackEscape = strings.NewReplacer("&", "&amp;", "<", "&lt;", ">", "&gt;", "*", "*\u200b", "_", "_\u200b", "~", "~\u200b", "`", "`\u200b")

// slack posts to an incoming webhook in blocks: the title as a header and what
// follows as a section, both plain text, so nothing a title holds formats or links,
// then the link. Its text, for notifications, is the title, escaped.
func (s *Sender) slack(ctx context.Context, sl *resourcev1.AlertSlack, m Message) Result {
	blocks := []any{map[string]any{"type": "header", "text": plainText(cutTo(m.Title(), slackHeaderMax, utf8.RuneCountInString))}}
	if lines := m.Lines(); len(lines) > 0 {
		blocks = append(blocks, map[string]any{"type": "section", "text": plainText(fit(lines, "", slackSectionMax, utf8.RuneCountInString))})
	}
	if link := m.Link(); link != "" {
		blocks = append(blocks, map[string]any{"type": "context", "elements": []any{map[string]any{"type": "mrkdwn", "text": "<" + link + "|Open in Dockwell>"}}})
	}
	return s.post(ctx, sl.GetUrl(), map[string]any{"text": slackEscape.Replace(m.Title()), "blocks": blocks}, nil, ownReason(sl.GetUrl(), "slack", plainReason))
}

func plainText(s string) map[string]any {
	return map[string]any{"type": "plain_text", "text": s, "emoji": false}
}

// discordEscape makes Discord's markup in what Dockwell didn't write inert: a link
// someone's words hold, such as a certificate authority's error, shows as words.
var discordEscape = strings.NewReplacer(
	`\`, `\\`, "*", `\*`, "_", `\_`, "~", `\~`, "`", "\\`", "|", `\|`, ">", `\>`, "<", `\<`,
	"[", `\[`, "]", `\]`, "(", `\(`, ")", `\)`,
)

// discordLine is a line of someone's words with its markup inert, a heading's or a
// list's at its start too.
func discordLine(l string) string {
	l = discordEscape.Replace(l)
	if strings.HasPrefix(l, "#") || strings.HasPrefix(l, "-") {
		l = `\` + l
	}
	return l
}

// discord posts to a webhook, mentioning no one whatever a title holds, with the
// title bold and every line's markup escaped. What doesn't fit is left out by the
// line, and the link always fits.
func (s *Sender) discord(ctx context.Context, d *resourcev1.AlertDiscord, m Message) Result {
	lines := []string{"**" + discordEscape.Replace(m.Title()) + "**"}
	for _, l := range m.Lines() {
		lines = append(lines, discordLine(l))
	}
	return s.post(ctx, d.GetUrl(), map[string]any{
		"content":          fit(lines, m.Link(), discordMax, utf8.RuneCountInString),
		"allowed_mentions": map[string]any{"parse": []string{}},
	}, nil, ownReason(d.GetUrl(), "discord", jsonReason("message")))
}

// ntfy publishes as JSON to the server's root, which takes a topic in the body,
// so the topic is never in a URL a log might keep.
func (s *Sender) ntfy(ctx context.Context, n *resourcev1.AlertNtfy, m Message) Result {
	priority := 3
	switch {
	case m.Critical():
		priority = 5
	case m.Status == "firing":
		priority = 4
	}
	msg := map[string]any{
		"topic":    n.GetTopic(),
		"title":    m.Title(),
		"message":  fit(m.Lines(), "", ntfyMax, func(s string) int { return len(s) }),
		"priority": priority,
	}
	if link := m.Link(); link != "" {
		msg["click"] = link
	}
	header := http.Header{}
	if n.GetToken() != "" {
		header.Set("Authorization", "Bearer "+n.GetToken())
	}
	server := alert.NtfyServer(n)
	return s.post(ctx, server+"/", msg, header, ownReason(server+"/", "ntfy", jsonReason("error")))
}

// telegram sends as the bot, in plain text, with no preview of the link.
func (s *Sender) telegram(ctx context.Context, tg *resourcev1.AlertTelegram, m Message) Result {
	api := s.Telegram
	if api == "" {
		api = TelegramAPI
	}
	return s.post(ctx, api+"/bot"+tg.GetBotToken()+"/sendMessage", map[string]any{
		"chat_id":                  tg.GetChatId(),
		"text":                     fit(append([]string{m.Title()}, m.Lines()...), m.Link(), telegramMax, utf16Len),
		"disable_web_page_preview": true,
	}, nil, jsonReason("description"))
}

// plainReason is a far end's answer that is a word or a line, as Slack's
// "invalid_token" is.
func plainReason(body []byte) string {
	line, _, _ := strings.Cut(strings.TrimSpace(string(body)), "\n")
	if !utf8.ValidString(line) || strings.HasPrefix(line, "<") {
		return ""
	}
	return line
}

// jsonReason reads why from a JSON answer's field.
func jsonReason(field string) explain {
	return func(body []byte) string {
		var v map[string]any
		if json.Unmarshal(body, &v) != nil {
			return ""
		}
		s, _ := v[field].(string)
		return s
	}
}

// email sends one message to every address, through the channel's SMTP server.
// It talks TLS, verifying the server's certificate, unless the channel says it's
// near enough to go without, and signs in only over TLS.
func (s *Sender) email(ctx context.Context, e *resourcev1.AlertEmail, m Message) Result {
	host := e.GetHost()
	addr := net.JoinHostPort(host, strconv.Itoa(alert.EmailPort(e)))
	security := alert.EmailSecurity(e)
	conn, err := guardFor(ctx, security == resourcev1.AlertEmail_SECURITY_NONE).dial(ctx, "tcp", addr)
	if err != nil {
		return Result{Said: sendWords(err)}
	}
	defer conn.Close()
	if deadline, ok := ctx.Deadline(); ok {
		_ = conn.SetDeadline(deadline)
	}
	config := s.tlsFor(host)
	if security == resourcev1.AlertEmail_SECURITY_TLS {
		tc := tls.Client(conn, config)
		if err := tc.HandshakeContext(ctx); err != nil {
			return Result{Said: sendWords(err)}
		}
		conn = tc
	}
	c, err := smtp.NewClient(conn, host)
	if err != nil {
		return Result{Said: smtpWords(err)}
	}
	defer c.Close()
	if security == resourcev1.AlertEmail_SECURITY_STARTTLS {
		if ok, _ := c.Extension("STARTTLS"); !ok {
			return Result{Said: "the server doesn't offer STARTTLS, so Dockwell sent nothing. Use TLS, or a server that offers it"}
		}
		if err := c.StartTLS(config); err != nil {
			return Result{Said: smtpWords(err)}
		}
	}
	if e.GetUsername() != "" {
		if _, tlsOn := c.TLSConnectionState(); !tlsOn {
			return Result{Said: "Dockwell sends a password only over TLS, so it sent nothing"}
		}
		auth, refusal := authFor(c, e, host)
		if auth == nil {
			return Result{Said: refusal}
		}
		if err := c.Auth(auth); err != nil {
			return Result{Said: smtpWords(err)}
		}
	}
	from, err := mail.ParseAddress(e.GetFrom())
	if err != nil {
		return Result{Said: "who it comes from isn't an email address"}
	}
	if err := c.Mail(from.Address); err != nil {
		return Result{Said: smtpWords(err)}
	}
	// Each recipient on its own: one the server refuses, such as someone who left,
	// keeps the message from no one else.
	var refused []string
	for _, to := range e.GetTo() {
		if err := c.Rcpt(to); err != nil {
			var tp *textproto.Error
			if !errors.As(err, &tp) {
				return Result{Said: smtpWords(err)}
			}
			refused = append(refused, to+" ("+smtpWords(err)+")")
		}
	}
	if len(refused) == len(e.GetTo()) {
		return Result{Said: clip("the server refused every address: " + strings.Join(refused, "; "))}
	}
	w, err := c.Data()
	if err != nil {
		return Result{Said: smtpWords(err)}
	}
	if _, err := w.Write(emailMessage(from, e.GetTo(), m)); err != nil {
		return Result{Said: smtpWords(err)}
	}
	if err := w.Close(); err != nil {
		return Result{Said: smtpWords(err)}
	}
	_ = c.Quit()
	said := "accepted by " + host
	if len(refused) > 0 {
		said += "; it refused " + strings.Join(refused, "; ")
	}
	return Result{OK: true, Said: clip(said)}
}

// authFor is how Dockwell signs in to an SMTP server that offers it: PLAIN, or
// LOGIN, as Exchange offers alone; or why it can't.
func authFor(c *smtp.Client, e *resourcev1.AlertEmail, host string) (smtp.Auth, string) {
	ok, params := c.Extension("AUTH")
	mechs := strings.Fields(strings.ToUpper(params))
	switch {
	case !ok:
		return nil, "the server doesn't offer to sign in, so Dockwell sent nothing. Clear the channel's user, or use a server that takes one"
	case slices.Contains(mechs, "PLAIN"):
		return smtp.PlainAuth("", e.GetUsername(), e.GetPassword(), host), ""
	case slices.Contains(mechs, "LOGIN"):
		return &loginAuth{user: e.GetUsername(), password: e.GetPassword(), host: host}, ""
	}
	return nil, clip("the server signs in with " + strings.Join(mechs, ", ") + ", and Dockwell with PLAIN or LOGIN, so it sent nothing")
}

// loginAuth signs in with AUTH LOGIN: the user, then the password, each as the
// server asks, only over TLS to the server the channel names, as PlainAuth does.
type loginAuth struct {
	user, password, host string
	step                 int
}

func (a *loginAuth) Start(server *smtp.ServerInfo) (string, []byte, error) {
	if !server.TLS {
		return "", nil, errors.New("dockwell sends a password only over TLS")
	}
	if server.Name != a.host {
		return "", nil, errors.New("the server isn't the one the channel names")
	}
	a.step = 0
	return "LOGIN", nil, nil
}

func (a *loginAuth) Next(_ []byte, more bool) ([]byte, error) {
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

func (s *Sender) tlsFor(host string) *tls.Config {
	if s.TLS != nil {
		c := s.TLS.Clone()
		c.ServerName = host
		return c
	}
	return &tls.Config{ServerName: host, MinVersion: tls.VersionTLS12}
}

// emailMessage is the email itself: plain text, quoted-printable, with a
// subject no title can break out of.
func emailMessage(from *mail.Address, to []string, m Message) []byte {
	subject := strings.Join(strings.Fields(m.Title()), " ")
	var b bytes.Buffer
	h := func(k, v string) { b.WriteString(k + ": " + v + "\r\n") }
	h("From", from.String())
	h("To", strings.Join(to, ", "))
	h("Subject", mime.QEncoding.Encode("utf-8", subject))
	h("Date", m.At.Format(time.RFC1123Z))
	h("Message-ID", "<"+rand.Text()+"@dockwell>")
	h("MIME-Version", "1.0")
	h("Content-Type", "text/plain; charset=utf-8")
	h("Content-Transfer-Encoding", "quoted-printable")
	h("Auto-Submitted", "auto-generated")
	h("X-Dockwell-Status", m.Status)
	b.WriteString("\r\n")
	qp := quotedprintable.NewWriter(&b)
	_, _ = qp.Write([]byte(strings.ReplaceAll(m.Text(), "\n", "\r\n") + "\r\n"))
	_ = qp.Close()
	return b.Bytes()
}

// smtpWords says what an SMTP server answered, as its code and its words.
func smtpWords(err error) string {
	var tp *textproto.Error
	if errors.As(err, &tp) {
		return clip(strconv.Itoa(tp.Code) + " " + tp.Msg)
	}
	return sendWords(err)
}

// sendWords says why a message didn't arrive, in a few words, with nothing that
// signs in: never a URL, which may hold a token (netguard.Words).
func sendWords(err error) string {
	return clip(netguard.Words(err))
}

func clip(s string) string {
	s = strings.TrimSpace(strings.ReplaceAll(s, "\n", " "))
	if len(s) > saidMax {
		return s[:saidMax] + "…"
	}
	return s
}

// fit joins lines into what a far end takes, by its count, leaving out whole lines
// from the end and saying so, and always keeping the link, so a long message still
// leads to the panel. A first line too long for it is cut short.
func fit(lines []string, link string, limit int, count func(string) int) string {
	tail := ""
	if link != "" {
		tail = "\n" + link
	}
	room := limit - count(tail)
	all := strings.Join(lines, "\n")
	if count(all) <= room {
		return all + tail
	}
	more := "\n…"
	out := ""
	for i, l := range lines {
		next := l
		if i > 0 {
			next = out + "\n" + l
		}
		if count(next)+count(more) > room {
			if i == 0 {
				return cutTo(l, room-count(more), count) + more + tail
			}
			break
		}
		out = next
	}
	return out + more + tail
}

// cutTo shortens s to limit by count, at a rune, ending it with "…".
func cutTo(s string, limit int, count func(string) int) string {
	if count(s) <= limit {
		return s
	}
	r := []rune(s)
	for len(r) > 0 && count(string(r))+count("…") > limit {
		r = r[:len(r)-1]
	}
	return string(r) + "…"
}

// utf16Len counts as Telegram does, in UTF-16 units.
func utf16Len(s string) int {
	n := 0
	for _, r := range s {
		if r > 0xFFFF {
			n += 2
		} else {
			n++
		}
	}
	return n
}
