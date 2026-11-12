// SPDX-License-Identifier: AGPL-3.0-only

// Package alert is the "alert.rule" and "alert.channel" kinds: what Dockwell
// watches for, and where it tells you. Both live in the organization and run on
// no server: the control plane evaluates the rules and sends to the channels. See
// docs/adr/0044-alerts.md.
package alert

import (
	"errors"
	"fmt"
	"slices"
	"strconv"
	"strings"
	"time"

	"google.golang.org/protobuf/proto"

	resourcev1 "github.com/dockwell/dockwell/gen/dockwell/resource/v1alpha1"
	"github.com/dockwell/dockwell/internal/id"
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
	// DefaultCheckHold is how long an address has to hold down before its alert
	// opens, from its first failed run, and up before it resolves, unless a rule
	// says.
	DefaultCheckHold = 2 * time.Minute
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
	case resourcev1.AlertRule_CONDITION_UPDATE_AVAILABLE:
		if r.GetThreshold() != 0 && r.GetThreshold() != MajorsOnly {
			return fmt.Errorf("a rule's threshold for an update is %d, for majors alone, or 0, for any", MajorsOnly)
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
	// A check that comes back resolves only once it has been up this long, so one
	// that fails twice and passes once, round and round, stays one alert and
	// doesn't get around repeat (ADR 0044). Its check says how many failed runs
	// make it down; this is how long it has to hold, from the first of them.
	if c == resourcev1.AlertRule_CONDITION_CHECK_DOWN && held == 0 {
		held = DefaultCheckHold
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
	case resourcev1.AlertRule_CONDITION_CHECK_DOWN:
		return "an address that doesn't answer"
	case resourcev1.AlertRule_CONDITION_RESTORE_DRILL_FAILED:
		return "a backup that doesn't restore"
	case resourcev1.AlertRule_CONDITION_RESTORE_DRILL_LATE:
		return "a restore check that's late"
	case resourcev1.AlertRule_CONDITION_UPDATE_AVAILABLE:
		return "an app's update"
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
	case resourcev1.AlertRule_CONDITION_CHECK_DOWN:
		return "Address down"
	case resourcev1.AlertRule_CONDITION_RESTORE_DRILL_FAILED:
		return "Restore check failed"
	case resourcev1.AlertRule_CONDITION_RESTORE_DRILL_LATE:
		return "Restore check late"
	case resourcev1.AlertRule_CONDITION_UPDATE_AVAILABLE:
		if r.GetThreshold() == MajorsOnly {
			return "Major update available"
		}
		return "Update available"
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
	case resourcev1.AlertRule_CONDITION_CHECK_DOWN:
		return "check.state == down"
	case resourcev1.AlertRule_CONDITION_RESTORE_DRILL_FAILED:
		return "backup.drill == failed"
	case resourcev1.AlertRule_CONDITION_RESTORE_DRILL_LATE:
		return "backup.drill.passed > 2 periods ago"
	case resourcev1.AlertRule_CONDITION_UPDATE_AVAILABLE:
		if r.GetThreshold() == MajorsOnly {
			return "service.update == major"
		}
		return "service.update != none"
	default:
		return ""
	}
}

// MajorsOnly is the threshold of a rule for an update that fires for a major
// alone, the first number of the version moving.
const MajorsOnly = 3

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
		{"check-down", &resourcev1.AlertRule{Condition: resourcev1.AlertRule_CONDITION_CHECK_DOWN, Duration: "2m", Severity: critical, Repeat: "4h"}},
		{"restore-drill-failed", &resourcev1.AlertRule{Condition: resourcev1.AlertRule_CONDITION_RESTORE_DRILL_FAILED, Severity: critical}},
		{"restore-drill-late", &resourcev1.AlertRule{Condition: resourcev1.AlertRule_CONDITION_RESTORE_DRILL_LATE, Severity: warning}},
		// Off until a person turns it on: an update is news, not trouble (ADR 0079).
		{"update-available", &resourcev1.AlertRule{Condition: resourcev1.AlertRule_CONDITION_UPDATE_AVAILABLE, Severity: warning, Disabled: true}},
	}
	return slices.Clip(rules)
}
