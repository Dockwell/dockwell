// SPDX-License-Identifier: AGPL-3.0-only

package alerts

import (
	"bytes"
	"cmp"
	"context"
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strconv"
	"strings"
	"time"

	resourcev1 "github.com/dockwell/dockwell/gen/dockwell/resource/v1alpha1"
	"github.com/dockwell/dockwell/internal/buildinfo"
	"github.com/dockwell/dockwell/internal/netguard"
	"github.com/dockwell/dockwell/internal/resource/alert"
	"github.com/dockwell/dockwell/internal/store"
)

// How a message goes out.
const (
	// SendTimeout is how long one try may take, connecting and all.
	SendTimeout = 10 * time.Second
	// saidMax bounds what's kept of what the far end answered: a line, not a page.
	saidMax = 200
)

// Message is what goes to a channel: alerts of one rule that fired, still fire, or
// resolved, or a test.
type Message struct {
	// Status is "firing", "resolved" or "test".
	Status string
	Rule   *resourcev1.AlertRule
	// RuleName is the rule's name, such as "disk-full".
	RuleName string
	Alerts   []store.Alert
	// Panel is where the panel answers, such as "https://panel.example.com". Empty
	// when the control plane has no domain, and messages carry no link.
	Panel string
	// At is when it's sent.
	At time.Time
}

// Title is what a message says first, in a line: the alert's own, or how many of a
// rule's there are.
func (m Message) Title() string {
	switch {
	case m.Status == "test":
		return "Dockwell test message"
	case len(m.Alerts) == 1 && m.Status == "resolved":
		return "Resolved: " + m.Alerts[0].Title
	case len(m.Alerts) == 1:
		return m.Alerts[0].Title
	}
	title := many(m.Rule, m.Alerts)
	if m.Status == "resolved" {
		return "Resolved: " + title
	}
	return title
}

// many says, in a line, that a rule holds for several things at once.
func many(r *resourcev1.AlertRule, alerts []store.Alert) string {
	n := len(alerts)
	condition := resourcev1.AlertRule_Condition(alerts[0].Condition)
	switch condition {
	case resourcev1.AlertRule_CONDITION_SERVICE_DOWN:
		return fmt.Sprintf("%d apps are down", n)
	case resourcev1.AlertRule_CONDITION_SERVER_UNREACHABLE:
		return fmt.Sprintf("%d servers can't be reached", n)
	case resourcev1.AlertRule_CONDITION_DISK_FULL:
		return fmt.Sprintf("%d disks are over %d%% full", n, r.GetThreshold())
	case resourcev1.AlertRule_CONDITION_MEMORY_HIGH:
		return fmt.Sprintf("%d servers use over %d%% of their memory", n, r.GetThreshold())
	case resourcev1.AlertRule_CONDITION_CERTIFICATE_EXPIRING:
		return fmt.Sprintf("%d certificates end within %d days", n, r.GetThreshold())
	case resourcev1.AlertRule_CONDITION_CERTIFICATE_FAILED:
		return fmt.Sprintf("%d domains have no certificate", n)
	case resourcev1.AlertRule_CONDITION_BACKUP_FAILED:
		return fmt.Sprintf("%d backups failed", n)
	case resourcev1.AlertRule_CONDITION_BACKUP_LATE:
		return fmt.Sprintf("%d backups are late", n)
	case resourcev1.AlertRule_CONDITION_CHANGE_FAILED:
		return fmt.Sprintf("%d changes failed", n)
	case resourcev1.AlertRule_CONDITION_UNSPECIFIED:
	}
	return fmt.Sprintf("%s holds for %d", cmp.Or(alert.Title(r), alerts[0].RuleName), n)
}

// ended says why an alert closed without resolving, in a line, or "".
func ended(a store.Alert) string {
	switch a.Ended {
	case "removed":
		return "Its rule was removed, so Dockwell no longer watches for this, and closed it."
	case "disabled":
		return "Its rule was turned off, so Dockwell no longer watches for this, and closed it."
	}
	return ""
}

// Text is the whole message, as a chat or an email takes it: the fact, what it
// means and what fixes it, then a link to where the panel shows it.
func (m Message) Text() string {
	if m.Status == "test" {
		return "This is a test from Dockwell, to see that alerts reach you here. Nothing is wrong."
	}
	var b strings.Builder
	b.WriteString(m.Title())
	if len(m.Alerts) == 1 {
		a := m.Alerts[0]
		if m.Status == "resolved" {
			if why := ended(a); why != "" {
				b.WriteString("\n" + why)
			} else if a.ResolvedAt != nil {
				fmt.Fprintf(&b, "\nIt held for %s.", humanDuration(a.ResolvedAt.Sub(a.OpenedAt)))
			}
		} else if a.Detail != "" {
			b.WriteString("\n" + a.Detail)
		}
		if link := m.link(a.Path); link != "" {
			b.WriteString("\n" + link)
		}
		return b.String()
	}
	for i := range m.Alerts {
		a := &m.Alerts[i]
		b.WriteString("\n- " + a.Title)
	}
	if m.Status != "resolved" && m.Alerts[0].Detail != "" {
		b.WriteString("\n" + m.Alerts[0].Detail)
	}
	if why := ended(m.Alerts[0]); m.Status == "resolved" && why != "" {
		b.WriteString("\n" + why)
	}
	if link := m.link("/alerts"); link != "" {
		b.WriteString("\n" + link)
	}
	return b.String()
}

func (m Message) link(path string) string {
	if m.Panel == "" || path == "" {
		return ""
	}
	return strings.TrimSuffix(m.Panel, "/") + path
}

// About is what a delivery says it was, as the channels' list shows it.
func (m Message) About() string {
	if m.Status == "test" {
		return "Test message"
	}
	return m.Title()
}

// humanDuration says a span in a word or two: "12 minutes", "3 hours".
func humanDuration(d time.Duration) string {
	switch {
	case d < 2*time.Minute:
		return "a minute"
	case d < 2*time.Hour:
		return strconv.Itoa(int(d/time.Minute)) + " minutes"
	case d < 48*time.Hour:
		return strconv.Itoa(int(d/time.Hour)) + " hours"
	}
	return strconv.Itoa(int(d/(24*time.Hour))) + " days"
}

// payload is what a webhook gets, as JSON.
type payload struct {
	Version  int            `json:"version"`
	Status   string         `json:"status"`
	Rule     *payloadRule   `json:"rule,omitempty"`
	Title    string         `json:"title"`
	Text     string         `json:"text"`
	Alerts   []payloadAlert `json:"alerts"`
	SentAt   time.Time      `json:"sent_at"`
	Dockwell string         `json:"dockwell"`
}

type payloadRule struct {
	Name      string `json:"name"`
	Title     string `json:"title"`
	Severity  string `json:"severity"`
	Condition string `json:"condition"`
}

type payloadAlert struct {
	ID         string     `json:"id"`
	Status     string     `json:"status"`
	Title      string     `json:"title"`
	Detail     string     `json:"detail,omitempty"`
	Facts      *Facts     `json:"facts,omitempty"`
	Ended      string     `json:"ended,omitempty"`
	ServerID   string     `json:"server_id,omitempty"`
	Subject    string     `json:"subject_id,omitempty"`
	Name       string     `json:"subject_name,omitempty"`
	OpenedAt   time.Time  `json:"opened_at"`
	ResolvedAt *time.Time `json:"resolved_at,omitempty"`
	URL        string     `json:"url,omitempty"`
}

func (m Message) payload() payload {
	p := payload{Version: 1, Status: m.Status, Title: m.Title(), Text: m.Text(), SentAt: m.At.UTC(), Alerts: []payloadAlert{}, Dockwell: buildinfo.Read().Version}
	if m.Rule != nil {
		p.Rule = &payloadRule{
			Name: m.RuleName, Title: alert.Title(m.Rule), Condition: alert.Expression(m.Rule),
			Severity: strings.ToLower(strings.TrimPrefix(m.Rule.GetSeverity().String(), "SEVERITY_")),
		}
	}
	for i := range m.Alerts {
		a := &m.Alerts[i]
		status := "firing"
		if a.ResolvedAt != nil {
			status = "resolved"
		}
		var facts *Facts
		if f := ReadFacts(a.Facts); f != (Facts{}) {
			facts = &f
		}
		p.Alerts = append(p.Alerts, payloadAlert{
			ID: a.ID, Status: status, Title: a.Title, Detail: a.Detail, Facts: facts, Ended: a.Ended, ServerID: a.ServerID, Subject: a.SubjectID, Name: a.SubjectName,
			OpenedAt: a.OpenedAt.UTC(), ResolvedAt: a.ResolvedAt, URL: m.link(a.Path),
		})
	}
	return p
}

// Sender sends messages to channels.
type Sender struct {
	// Client sends webhooks. Nil is netguard's, for what a channel may reach
	// (alert.Guard), or for a test (alert.TestGuard).
	Client *http.Client
}

// doer sends a request: an *http.Client, or netguard's.
type doer interface {
	Do(*http.Request) (*http.Response, error)
}

// Result is how one message to a channel went, in the words a delivery keeps.
type Result struct {
	OK   bool
	Said string
}

// Once sends m to a channel once. The outbox tries again what doesn't land
// (ADR 0044).
func (s *Sender) Once(ctx context.Context, c *resourcev1.AlertChannel, m Message) Result {
	return s.send(ctx, c, m, s.client(defaultClient))
}

// Test sends a test to a channel once, as a person asks: never to this server's
// own addresses, whatever the channel may reach, so a test can't read what answers
// on the control server's loopback.
func (s *Sender) Test(ctx context.Context, c *resourcev1.AlertChannel, m Message) Result {
	return s.send(ctx, c, m, s.client(testClient))
}

func (s *Sender) send(ctx context.Context, c *resourcev1.AlertChannel, m Message, client doer) Result {
	ctx, cancel := context.WithTimeout(ctx, SendTimeout)
	defer cancel()
	switch to := c.GetTo().(type) {
	case *resourcev1.AlertChannel_Webhook:
		return s.webhook(ctx, to.Webhook, m, client)
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

func (s *Sender) webhook(ctx context.Context, w *resourcev1.AlertWebhook, m Message, client doer) Result {
	body, err := json.Marshal(m.payload())
	if err != nil {
		return Result{Said: "Dockwell couldn't write the message"}
	}
	ctx, cancel := context.WithTimeout(ctx, SendTimeout)
	defer cancel()
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, w.GetUrl(), bytes.NewReader(body))
	if err != nil {
		return Result{Said: "the webhook's URL isn't one to send to"}
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("User-Agent", "Dockwell/"+buildinfo.Read().Version)
	if w.GetSecret() != "" {
		req.Header.Set("Dockwell-Signature", Signature(w.GetSecret(), m.At, body))
	}
	resp, err := client.Do(req)
	if err != nil {
		return Result{Said: sendWords(err)}
	}
	defer resp.Body.Close()
	_, _ = io.CopyN(io.Discard, resp.Body, 64<<10)
	return Result{OK: resp.StatusCode >= 200 && resp.StatusCode < 300, Said: clip(resp.Status)}
}

func (s *Sender) client(guarded doer) doer {
	if s.Client != nil {
		return s.Client
	}
	return guarded
}

// defaultClient is netguard's for what a channel may reach (alert.Guard), and
// testClient for a test (alert.TestGuard): each dials no link-local or metadata
// address, whatever a name resolves to, goes through no proxy, follows no redirect,
// as a webhook that moves says so in its answer, and reads 64 KiB of an answer at
// most (ADR 0044).
var (
	defaultClient = netguard.Client(netguard.Options{Private: alert.Guard.Private, Loopback: alert.Guard.Loopback, MaxBody: 64 << 10, Timeout: SendTimeout})
	testClient    = netguard.Client(netguard.Options{Private: alert.TestGuard.Private, Loopback: alert.TestGuard.Loopback, MaxBody: 64 << 10, Timeout: SendTimeout})
)

// sendWords says why a message didn't arrive, in a few words, with nothing that
// signs in: never the URL, which may hold a token (netguard.Words).
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
