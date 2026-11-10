// SPDX-License-Identifier: AGPL-3.0-only

package alerts

import (
	"cmp"
	"fmt"
	"strconv"
	"strings"
	"time"

	resourcev1 "github.com/dockwell/dockwell/gen/dockwell/resource/v1alpha1"
	"github.com/dockwell/dockwell/internal/buildinfo"
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
	case resourcev1.AlertRule_CONDITION_CHECK_DOWN:
		return fmt.Sprintf("%d addresses are down", n)
	case resourcev1.AlertRule_CONDITION_RESTORE_DRILL_FAILED:
		return fmt.Sprintf("%d backups don't restore", n)
	case resourcev1.AlertRule_CONDITION_RESTORE_DRILL_LATE:
		return fmt.Sprintf("%d schedules haven't passed a restore check lately", n)
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
	parts := append([]string{m.Title()}, m.Lines()...)
	if link := m.Link(); link != "" {
		parts = append(parts, link)
	}
	return strings.Join(parts, "\n")
}

// Lines are what a message says after its title, without its link: what it means
// and what fixes it, or each alert of a rule's that fired together.
func (m Message) Lines() []string {
	if m.Status == "test" {
		return []string{"This is a test from Dockwell, to see that alerts reach you here. Nothing is wrong."}
	}
	if len(m.Alerts) == 1 {
		a := m.Alerts[0]
		switch {
		case m.Status == "resolved" && ended(a) != "":
			return []string{ended(a)}
		case m.Status == "resolved" && a.ResolvedAt != nil:
			return []string{fmt.Sprintf("It held for %s.", humanDuration(a.ResolvedAt.Sub(a.OpenedAt)))}
		case m.Status != "resolved" && a.Detail != "":
			return []string{a.Detail}
		}
		return nil
	}
	var out []string
	for i := range m.Alerts {
		out = append(out, "- "+m.Alerts[i].Title)
	}
	if m.Status != "resolved" && len(m.Alerts) > 0 && m.Alerts[0].Detail != "" {
		out = append(out, m.Alerts[0].Detail)
	}
	if why := ended(m.Alerts[0]); m.Status == "resolved" && why != "" {
		out = append(out, why)
	}
	return out
}

// Link is where the panel shows what a message is about: the alert's own page, or
// Alerts for several. Empty when the control plane has no domain, or for a test.
func (m Message) Link() string {
	switch {
	case m.Status == "test" || len(m.Alerts) == 0:
		return ""
	case len(m.Alerts) == 1:
		return m.link(m.Alerts[0].Path)
	}
	return m.link("/alerts")
}

func (m Message) link(path string) string {
	if m.Panel == "" || path == "" {
		return ""
	}
	return strings.TrimSuffix(m.Panel, "/") + path
}

// Critical reports a message about a critical rule's alerts that fire.
func (m Message) Critical() bool {
	return m.Status == "firing" && m.Rule.GetSeverity() == resourcev1.AlertRule_SEVERITY_CRITICAL
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
