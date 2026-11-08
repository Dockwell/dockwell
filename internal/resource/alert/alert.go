// SPDX-License-Identifier: AGPL-3.0-only

// Package alert is the "alert.rule" and "alert.channel" kinds: what Dockwell
// watches for, and where it tells you. Both live in the organization and run on
// no server: the control plane evaluates the rules and sends to the channels. See
// docs/adr/0044-alerts.md.
package alert

import (
	"errors"
	"fmt"
	"net/netip"
	"net/url"
	"slices"
	"strconv"
	"strings"
	"time"

	"google.golang.org/protobuf/proto"

	resourcev1 "github.com/dockwell/dockwell/gen/dockwell/resource/v1alpha1"
	"github.com/dockwell/dockwell/internal/id"
	"github.com/dockwell/dockwell/internal/netguard"
	"github.com/dockwell/dockwell/internal/resource"
)

// The kinds.
const (
	RuleKind    resource.Kind = "alert.rule"
	ChannelKind resource.Kind = "alert.channel"
)

// Bounds on what a rule takes.
const (
	// MaxDuration is the longest a condition may have to hold.
	MaxDuration = 24 * time.Hour
	// MinRepeat and MaxRepeat bound how often a firing alert is sent again, so
	// one outage can't become a storm, and a repeat still means something.
	MinRepeat = 10 * time.Minute
	MaxRepeat = 7 * 24 * time.Hour
	// DefaultLate is how late a scheduled backup may start, unless a rule says.
	DefaultLate = time.Hour
)

// Rule is the alert rule kind.
type Rule struct{}

var (
	_ resource.Type     = Rule{}
	_ resource.Unplaced = Rule{}
	_ resource.Referrer = Rule{}
)

// Kind returns "alert.rule".
func (Rule) Kind() resource.Kind { return RuleKind }

// Level is the organization: a rule watches every server and app.
func (Rule) Level() resource.Level { return resource.LevelOrganization }

// IDPrefix is "alr".
func (Rule) IDPrefix() string { return id.AlertRule }

// New returns an empty AlertRule.
func (Rule) New() proto.Message { return &resourcev1.AlertRule{} }

// Unplaced says the control plane evaluates it.
func (Rule) Unplaced() {}

// CheckName takes what fits in a URL and on a command line.
func (Rule) CheckName(name string) error { return resource.CheckLabel("alert rule", name, "disk-full") }

// References are the channels it names.
func (Rule) References(spec proto.Message) []resource.Reference {
	r, _ := spec.(*resourcev1.AlertRule)
	refs := make([]resource.Reference, 0, len(r.GetChannels()))
	for _, name := range r.GetChannels() {
		refs = append(refs, resource.Reference{Kind: ChannelKind, Name: name, Verb: "send to", Apart: true})
	}
	return refs
}

// ReferenceKinds is the channel kind.
func (Rule) ReferenceKinds() []resource.Kind { return []resource.Kind{ChannelKind} }

// Validate refuses what can't be evaluated, and fills in the defaults, so rules
// that mean the same compare equal.
func (Rule) Validate(spec proto.Message) error {
	r := spec.(*resourcev1.AlertRule) //nolint:forcetypeassert // the registry gives this kind's message
	c := r.GetCondition()
	if _, ok := resourcev1.AlertRule_Condition_name[int32(c)]; !ok || c == resourcev1.AlertRule_CONDITION_UNSPECIFIED {
		return errors.New("a rule needs a condition, such as CONDITION_DISK_FULL")
	}
	switch c {
	case resourcev1.AlertRule_CONDITION_DISK_FULL, resourcev1.AlertRule_CONDITION_MEMORY_HIGH:
		if r.Threshold == 0 {
			r.Threshold = 90
		}
		if r.GetThreshold() > 100 {
			return fmt.Errorf("a rule's threshold for %s is a percent, from 1 to 100", Word(c))
		}
	case resourcev1.AlertRule_CONDITION_CERTIFICATE_EXPIRING:
		if r.Threshold == 0 {
			r.Threshold = 14
		}
		if r.GetThreshold() > 90 {
			return errors.New("a rule's threshold for a certificate is in days, from 1 to 90, as a certificate from Let's Encrypt lasts 90")
		}
	default:
		if r.GetThreshold() != 0 {
			return fmt.Errorf("a rule for %s takes no threshold. Leave it 0", Word(c))
		}
	}
	held, err := ParseDuration("duration", r.GetDuration())
	if err != nil {
		return err
	}
	if c == resourcev1.AlertRule_CONDITION_BACKUP_LATE && held == 0 {
		held = DefaultLate
	}
	switch {
	case held > MaxDuration:
		return fmt.Errorf("a rule's duration is at most %s, as one that has to hold longer would tell you a day late", FormatDuration(MaxDuration))
	case held != 0 && c == resourcev1.AlertRule_CONDITION_CHANGE_FAILED:
		return errors.New("a rule for a failed change fires as the change fails, so it takes no duration. Leave it empty")
	}
	r.Duration = FormatDuration(held)
	repeat, err := ParseDuration("repeat", r.GetRepeat())
	if err != nil {
		return err
	}
	switch {
	case repeat != 0 && c == resourcev1.AlertRule_CONDITION_CHANGE_FAILED:
		return errors.New("a failed change is told once, as it's over, so its rule takes no repeat. Leave it empty")
	case repeat != 0 && repeat < MinRepeat:
		return fmt.Errorf("a rule repeats at most every %s, so an outage doesn't become a storm", FormatDuration(MinRepeat))
	case repeat > MaxRepeat:
		return fmt.Errorf("a rule repeats at least every %s, or not at all: leave repeat empty", FormatDuration(MaxRepeat))
	}
	r.Repeat = FormatDuration(repeat)
	switch r.GetSeverity() {
	case resourcev1.AlertRule_SEVERITY_UNSPECIFIED:
		r.Severity = resourcev1.AlertRule_SEVERITY_WARNING
	case resourcev1.AlertRule_SEVERITY_WARNING, resourcev1.AlertRule_SEVERITY_CRITICAL:
	default:
		return errors.New("a rule's severity is SEVERITY_WARNING or SEVERITY_CRITICAL")
	}
	seen := map[string]bool{}
	channels := r.GetChannels()[:0]
	for _, name := range r.GetChannels() {
		name = strings.TrimSpace(name)
		if err := (Channel{}).CheckName(name); err != nil {
			return fmt.Errorf("a rule names channels by name: %w", err)
		}
		if !seen[name] {
			seen[name] = true
			channels = append(channels, name)
		}
	}
	r.Channels = channels
	return nil
}

// ParseDuration reads a duration a rule gives, such as "5m" or "1h30m". Empty is 0.
func ParseDuration(field, s string) (time.Duration, error) {
	s = strings.TrimSpace(s)
	if s == "" || s == "0" {
		return 0, nil
	}
	if days, ok := strings.CutSuffix(s, "d"); ok {
		// Up to ten years: what holds longer is each field's own limit to say.
		if n, err := strconv.Atoi(days); err == nil && n > 0 && n <= 3650 {
			return time.Duration(n) * 24 * time.Hour, nil
		}
	}
	d, err := time.ParseDuration(s)
	if err != nil || d < 0 {
		return 0, fmt.Errorf("a rule's %s is a duration, such as 90s, 5m or 4h", field)
	}
	if d%time.Second != 0 {
		return 0, fmt.Errorf("a rule's %s counts whole seconds, such as 90s", field)
	}
	return d, nil
}

// FormatDuration writes a duration as a rule keeps it: "5m", "1h30m", "90s"; empty
// for 0.
func FormatDuration(d time.Duration) string {
	if d <= 0 {
		return ""
	}
	var b strings.Builder
	if h := d / time.Hour; h > 0 {
		fmt.Fprintf(&b, "%dh", h)
		d -= h * time.Hour
	}
	if m := d / time.Minute; m > 0 {
		fmt.Fprintf(&b, "%dm", m)
		d -= m * time.Minute
	}
	if s := d / time.Second; s > 0 {
		fmt.Fprintf(&b, "%ds", s)
	}
	return b.String()
}

// Duration is how long a validated rule's condition has to hold.
func Duration(r *resourcev1.AlertRule) time.Duration {
	d, _ := ParseDuration("duration", r.GetDuration())
	return d
}

// Repeat is how often a validated rule sends a firing alert again; 0 for never.
func Repeat(r *resourcev1.AlertRule) time.Duration {
	d, _ := ParseDuration("repeat", r.GetRepeat())
	return d
}

// Word is what a condition watches, in a phrase for a sentence.
func Word(c resourcev1.AlertRule_Condition) string {
	switch c {
	case resourcev1.AlertRule_CONDITION_SERVICE_DOWN:
		return "an app that's down"
	case resourcev1.AlertRule_CONDITION_SERVER_UNREACHABLE:
		return "a server that can't be reached"
	case resourcev1.AlertRule_CONDITION_DISK_FULL:
		return "disk"
	case resourcev1.AlertRule_CONDITION_MEMORY_HIGH:
		return "memory"
	case resourcev1.AlertRule_CONDITION_CERTIFICATE_EXPIRING:
		return "a certificate that expires soon"
	case resourcev1.AlertRule_CONDITION_CERTIFICATE_FAILED:
		return "a certificate that couldn't be issued"
	case resourcev1.AlertRule_CONDITION_BACKUP_FAILED:
		return "a failed backup"
	case resourcev1.AlertRule_CONDITION_BACKUP_LATE:
		return "a late backup"
	case resourcev1.AlertRule_CONDITION_CHANGE_FAILED:
		return "a failed change"
	default:
		return "that condition"
	}
}

// Title is what a rule watches for, in words, such as "Disk above 90%".
func Title(r *resourcev1.AlertRule) string {
	switch r.GetCondition() {
	case resourcev1.AlertRule_CONDITION_SERVICE_DOWN:
		return "App down"
	case resourcev1.AlertRule_CONDITION_SERVER_UNREACHABLE:
		return "Server unreachable"
	case resourcev1.AlertRule_CONDITION_DISK_FULL:
		return fmt.Sprintf("Disk above %d%%", r.GetThreshold())
	case resourcev1.AlertRule_CONDITION_MEMORY_HIGH:
		return fmt.Sprintf("Memory above %d%%", r.GetThreshold())
	case resourcev1.AlertRule_CONDITION_CERTIFICATE_EXPIRING:
		return fmt.Sprintf("Certificate expires within %d days", r.GetThreshold())
	case resourcev1.AlertRule_CONDITION_CERTIFICATE_FAILED:
		return "Certificate failed"
	case resourcev1.AlertRule_CONDITION_BACKUP_FAILED:
		return "Backup failed"
	case resourcev1.AlertRule_CONDITION_BACKUP_LATE:
		return "Backup late"
	case resourcev1.AlertRule_CONDITION_CHANGE_FAILED:
		return "Change failed"
	default:
		return "Unknown condition"
	}
}

// Expression is a rule's condition as an expression, such as "disk.percent > 90",
// as Alerts lists it.
func Expression(r *resourcev1.AlertRule) string {
	switch r.GetCondition() {
	case resourcev1.AlertRule_CONDITION_SERVICE_DOWN:
		return "service.health != healthy"
	case resourcev1.AlertRule_CONDITION_SERVER_UNREACHABLE:
		return "server.connected == false"
	case resourcev1.AlertRule_CONDITION_DISK_FULL:
		return fmt.Sprintf("disk.percent > %d", r.GetThreshold())
	case resourcev1.AlertRule_CONDITION_MEMORY_HIGH:
		return fmt.Sprintf("memory.percent > %d", r.GetThreshold())
	case resourcev1.AlertRule_CONDITION_CERTIFICATE_EXPIRING:
		return fmt.Sprintf("certificate.days_left < %d", r.GetThreshold())
	case resourcev1.AlertRule_CONDITION_CERTIFICATE_FAILED:
		return "certificate.state == failed"
	case resourcev1.AlertRule_CONDITION_BACKUP_FAILED:
		return "backup.state == failed"
	case resourcev1.AlertRule_CONDITION_BACKUP_LATE:
		return "backup.late > " + r.GetDuration()
	case resourcev1.AlertRule_CONDITION_CHANGE_FAILED:
		return "change.state == failed"
	default:
		return ""
	}
}

// Channel is the alert channel kind.
type Channel struct{}

var (
	_ resource.Type       = Channel{}
	_ resource.Unplaced   = Channel{}
	_ resource.Rotates    = Channel{}
	_ resource.Keeper     = Channel{}
	_ resource.NewChecker = Channel{}
)

// Kind returns "alert.channel".
func (Channel) Kind() resource.Kind { return ChannelKind }

// Level is the organization: any rule can send to it.
func (Channel) Level() resource.Level { return resource.LevelOrganization }

// IDPrefix is "ach".
func (Channel) IDPrefix() string { return id.AlertChannel }

// New returns an empty AlertChannel.
func (Channel) New() proto.Message { return &resourcev1.AlertChannel{} }

// Unplaced says the control plane sends to it.
func (Channel) Unplaced() {}

// RotatesSensitive says setting what signs in always plans as a change, never
// compared with what's there, so a plan can't confirm a guess at it (ADR 0022).
func (Channel) RotatesSensitive() {}

// CheckName takes what fits in a URL and on a command line.
func (Channel) CheckName(name string) error {
	return resource.CheckLabel("alert channel", name, "ops-webhook")
}

// Keep holds on to what signs in to a channel when an update leaves it out, so a
// form can change the rest without asking for a token again. It keeps only from a
// channel of the same type.
func (Channel) Keep(current, desired proto.Message) (resource.Kept, error) {
	was, ok := current.(*resourcev1.AlertChannel)
	is, same := desired.(*resourcev1.AlertChannel)
	if !ok || !same {
		return resource.Kept{}, nil
	}
	kept := false
	if w, ok := is.GetTo().(*resourcev1.AlertChannel_Webhook); ok && was.GetWebhook() != nil {
		if w.Webhook.GetUrl() == "" {
			w.Webhook.Url, kept = was.GetWebhook().GetUrl(), true
		}
		if w.Webhook.GetSecret() == "" && was.GetWebhook().GetSecret() != "" {
			w.Webhook.Secret, kept = was.GetWebhook().GetSecret(), true
		}
	}
	if !kept {
		return resource.Kept{}, nil
	}
	return resource.Kept{Reason: "kept_credentials", Words: "keeps what signs in to it, as the update left that out"}, nil
}

// CheckNew asks a new channel for everything it sends with. An update can leave
// what signs in out, which Keep fills in, so Validate can't.
func (Channel) CheckNew(spec proto.Message) error {
	c := spec.(*resourcev1.AlertChannel) //nolint:forcetypeassert // Validate saw it
	if w := c.GetWebhook(); w != nil && w.GetUrl() == "" {
		return errors.New("a webhook channel needs its URL, such as https://hooks.example.com/dockwell")
	}
	return nil
}

// Validate refuses what can't be sent to, in words that hold nothing that signs in.
func (Channel) Validate(spec proto.Message) error {
	c := spec.(*resourcev1.AlertChannel) //nolint:forcetypeassert // the registry gives this kind's message
	switch to := c.GetTo().(type) {
	case *resourcev1.AlertChannel_Webhook:
		w := to.Webhook
		if w == nil {
			return errors.New("a webhook channel needs its URL, such as https://hooks.example.com/dockwell")
		}
		w.Url = strings.TrimSpace(w.GetUrl())
		if w.GetUrl() == "" {
			return nil // CheckNew asks a new one for it, and Keep fills in an update's
		}
		return CheckURL(w.GetUrl())
	default:
		return errors.New("a channel needs somewhere to send, such as a webhook")
	}
}

// CheckURL refuses a URL a channel can't send to, saying nothing of what it holds
// beyond its scheme and host.
func CheckURL(raw string) error {
	u, err := url.Parse(raw)
	if err != nil || u.Host == "" || u.Hostname() == "" {
		return errors.New("a webhook's URL is an address such as https://hooks.example.com/dockwell")
	}
	if u.User != nil {
		return errors.New("a webhook's URL can't hold a user and password. Use a token in its path or query, or a secret to sign with")
	}
	if why := netguard.Forbidden(u.Hostname(), Guard); why != "" {
		return fmt.Errorf("a webhook can't send to %s, as that's %s", u.Hostname(), why)
	}
	switch u.Scheme {
	case "https":
	case "http":
		if !Nearby(u.Hostname()) {
			return fmt.Errorf("a webhook's URL to %s has to use HTTPS, as an alert says what's wrong where anyone between could read it. Only an address on this server or its private network may use plain HTTP", u.Hostname())
		}
	default:
		return errors.New("a webhook's URL starts with https://")
	}
	return nil
}

// Nearby reports a host on this server or its private network, by its address or
// a name only such a network gives.
func Nearby(host string) bool {
	if host == "localhost" || !strings.Contains(host, ".") {
		return true
	}
	if addr, err := netip.ParseAddr(strings.Trim(host, "[]")); err == nil {
		addr = addr.Unmap()
		// Carrier-grade NAT's, as a tailnet's are, is a private network too, as
		// netguard counts it.
		return addr.IsLoopback() || addr.IsPrivate() || cgnat.Contains(addr)
	}
	for _, suffix := range []string{".internal", ".lan", ".local", ".home.arpa"} {
		if strings.HasSuffix(host, suffix) {
			return true
		}
	}
	return false
}

// Guard is what a channel may reach (netguard, #341): this server and its private
// networks as well as the internet, as a webhook or a chat server on the install's
// own network is what many want, and a channel is made and tested only by a person
// or a token that can make changes. Never a link-local or metadata address.
var Guard = netguard.Options{Private: true, Loopback: true}

// TestGuard is what a test of a channel may reach: what Guard does but this
// server's own addresses, so a test, which shows what answered, can't read what
// listens on the control server's loopback.
var TestGuard = netguard.Options{Private: true}

// cgnat is carrier-grade NAT's range.
var cgnat = netip.MustParsePrefix("100.64.0.0/10")

// Type is what sort of channel it is, such as "webhook".
func Type(c *resourcev1.AlertChannel) string {
	switch c.GetTo().(type) {
	case *resourcev1.AlertChannel_Webhook:
		return "webhook"
	default:
		return ""
	}
}

// Target is where a channel sends, with nothing that signs in: a webhook's host.
func Target(c *resourcev1.AlertChannel) string {
	if w := c.GetWebhook(); w != nil {
		if u, err := url.Parse(w.GetUrl()); err == nil {
			return u.Host
		}
	}
	return ""
}

// Default is one of the rules an install starts with.
type Default struct {
	Name string
	Rule *resourcev1.AlertRule
}

// Defaults are the rules an install starts with: one for each condition, each
// sending to every channel, so a new install tells you of what matters as soon as
// it has somewhere to send.
func Defaults() []Default {
	critical, warning := resourcev1.AlertRule_SEVERITY_CRITICAL, resourcev1.AlertRule_SEVERITY_WARNING
	rules := []Default{
		{"app-down", &resourcev1.AlertRule{Condition: resourcev1.AlertRule_CONDITION_SERVICE_DOWN, Duration: "2m", Severity: critical, Repeat: "4h"}},
		{"server-unreachable", &resourcev1.AlertRule{Condition: resourcev1.AlertRule_CONDITION_SERVER_UNREACHABLE, Duration: "2m", Severity: critical, Repeat: "4h"}},
		{"disk-full", &resourcev1.AlertRule{Condition: resourcev1.AlertRule_CONDITION_DISK_FULL, Threshold: 90, Duration: "5m", Severity: warning, Repeat: "24h"}},
		{"memory-high", &resourcev1.AlertRule{Condition: resourcev1.AlertRule_CONDITION_MEMORY_HIGH, Threshold: 90, Duration: "10m", Severity: warning, Repeat: "24h"}},
		{"certificate-expiring", &resourcev1.AlertRule{Condition: resourcev1.AlertRule_CONDITION_CERTIFICATE_EXPIRING, Threshold: 14, Severity: warning, Repeat: "24h"}},
		{"certificate-failed", &resourcev1.AlertRule{Condition: resourcev1.AlertRule_CONDITION_CERTIFICATE_FAILED, Duration: "10m", Severity: critical, Repeat: "24h"}},
		{"backup-failed", &resourcev1.AlertRule{Condition: resourcev1.AlertRule_CONDITION_BACKUP_FAILED, Severity: critical}},
		{"backup-late", &resourcev1.AlertRule{Condition: resourcev1.AlertRule_CONDITION_BACKUP_LATE, Duration: "1h", Severity: warning}},
		{"change-failed", &resourcev1.AlertRule{Condition: resourcev1.AlertRule_CONDITION_CHANGE_FAILED, Severity: warning}},
	}
	return slices.Clip(rules)
}
