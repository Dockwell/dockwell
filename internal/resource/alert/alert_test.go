// SPDX-License-Identifier: AGPL-3.0-only

package alert

import (
	"strings"
	"testing"
	"time"

	"google.golang.org/protobuf/proto"

	resourcev1 "github.com/dockwell/dockwell/gen/dockwell/resource/v1alpha1"
)

// A rule takes what its condition needs, fills in its defaults, and refuses what
// would tell nobody anything, in words that say what to give.
func TestARule(t *testing.T) {
	disk := &resourcev1.AlertRule{Condition: resourcev1.AlertRule_CONDITION_DISK_FULL, Duration: "300s", Repeat: "1d", Channels: []string{"ops", " ops "}}
	if err := (Rule{}).Validate(disk); err != nil {
		t.Fatal(err)
	}
	if disk.GetThreshold() != 90 || disk.GetDuration() != "5m" || disk.GetRepeat() != "24h" || disk.GetSeverity() != resourcev1.AlertRule_SEVERITY_WARNING || len(disk.GetChannels()) != 1 {
		t.Errorf("Validate wrote %v", disk)
	}
	if got := Title(disk); got != "Disk above 90%" {
		t.Errorf("Title = %q", got)
	}
	if got := Expression(disk); got != "disk.percent > 90" {
		t.Errorf("Expression = %q", got)
	}
	late := &resourcev1.AlertRule{Condition: resourcev1.AlertRule_CONDITION_BACKUP_LATE}
	if err := (Rule{}).Validate(late); err != nil || late.GetDuration() != "1h" {
		t.Errorf("a late backup's rule = %v, %v; want an hour unless it says", late, err)
	}
	for _, tc := range []struct {
		r    *resourcev1.AlertRule
		says string
	}{
		{&resourcev1.AlertRule{}, "needs a condition"},
		{&resourcev1.AlertRule{Condition: 99}, "needs a condition"},
		{&resourcev1.AlertRule{Condition: resourcev1.AlertRule_CONDITION_DISK_FULL, Threshold: 101}, "from 1 to 100"},
		{&resourcev1.AlertRule{Condition: resourcev1.AlertRule_CONDITION_CERTIFICATE_EXPIRING, Threshold: 91}, "from 1 to 90"},
		{&resourcev1.AlertRule{Condition: resourcev1.AlertRule_CONDITION_SERVICE_DOWN, Threshold: 5}, "takes no threshold"},
		{&resourcev1.AlertRule{Condition: resourcev1.AlertRule_CONDITION_SERVICE_DOWN, Duration: "soon"}, "is a duration"},
		{&resourcev1.AlertRule{Condition: resourcev1.AlertRule_CONDITION_SERVICE_DOWN, Duration: "25h"}, "at most 24h"},
		{&resourcev1.AlertRule{Condition: resourcev1.AlertRule_CONDITION_CHANGE_FAILED, Duration: "5m"}, "takes no duration"},
		{&resourcev1.AlertRule{Condition: resourcev1.AlertRule_CONDITION_CHANGE_FAILED, Repeat: "1h"}, "told once"},
		{&resourcev1.AlertRule{Condition: resourcev1.AlertRule_CONDITION_SERVICE_DOWN, Repeat: "1m"}, "doesn't become a storm"},
		{&resourcev1.AlertRule{Condition: resourcev1.AlertRule_CONDITION_SERVICE_DOWN, Repeat: "30d"}, "or not at all"},
		{&resourcev1.AlertRule{Condition: resourcev1.AlertRule_CONDITION_SERVICE_DOWN, Severity: 7}, "SEVERITY_WARNING or SEVERITY_CRITICAL"},
		{&resourcev1.AlertRule{Condition: resourcev1.AlertRule_CONDITION_SERVICE_DOWN, Channels: []string{"Ops Team"}}, "names channels by name"},
	} {
		if err := (Rule{}).Validate(tc.r); err == nil || !strings.Contains(err.Error(), tc.says) {
			t.Errorf("Validate(%v) = %v, want it to say %q", tc.r, err, tc.says)
		}
	}
	refs := (Rule{}).References(disk)
	if len(refs) != 1 || refs[0].Kind != ChannelKind || refs[0].Name != "ops" {
		t.Errorf("a rule's references are %v, want the channel it names", refs)
	}
}

func TestADuration(t *testing.T) {
	for in, want := range map[string]time.Duration{"": 0, "0": 0, "90s": 90 * time.Second, "1h30m": 90 * time.Minute, "2d": 48 * time.Hour, "31d": 31 * 24 * time.Hour} {
		if got, err := ParseDuration("duration", in); err != nil || got != want {
			t.Errorf("ParseDuration(%q) = %v, %v; want %v", in, got, err, want)
		}
	}
	for _, in := range []string{"-5m", "1.5s", "forever", "3651d"} {
		if _, err := ParseDuration("duration", in); err == nil {
			t.Errorf("ParseDuration(%q) took it", in)
		}
	}
	for d, want := range map[time.Duration]string{0: "", 90 * time.Second: "1m30s", 2 * time.Hour: "2h", 25 * time.Hour: "25h"} {
		if got := FormatDuration(d); got != want {
			t.Errorf("FormatDuration(%v) = %q, want %q", d, got, want)
		}
	}
}

// A channel's URL goes nowhere it shouldn't, and what signs in to it stays when an
// update leaves it out.
func TestAWebhookChannel(t *testing.T) {
	for _, ok := range []string{
		"https://hooks.example.com/dockwell?token=abc",
		"http://10.0.0.5:8080/hook",
		"http://ntfy.home.arpa/alerts",
		"http://localhost:9000/hook",
		// A tailnet's address is on a private network, as netguard counts it.
		"http://100.64.0.7:8080/hook",
	} {
		if err := CheckURL(ok); err != nil {
			t.Errorf("CheckURL(%q) = %v", ok, err)
		}
	}
	for _, tc := range []struct{ url, says string }{
		{"http://hooks.example.com/dockwell", "has to use HTTPS"},
		{"https://user:pass@hooks.example.com/", "can't hold a user and password"},
		{"https://169.254.169.254/latest", "metadata service"},
		{"http://169.254.169.254/latest", "metadata service"},
		{"http://100.100.100.200/latest/meta-data/", "metadata service"},
		{"https://100.100.100.200/latest/meta-data/", "metadata service"},
		{"http://[fd00:ec2::254]/latest/meta-data/", "metadata service"},
		{"http://[fe80::1]/", "as that's a link-local address"},
		{"http://[::ffff:169.254.169.254]/", "metadata service"},
		{"http://0.0.0.0:7600/", "as that's an unspecified address"},
		{"http://metadata.google.internal/computeMetadata/v1/", "metadata service"},
		{"http://[ff02::1]/", "as that's a multicast address"},
		{"ftp://hooks.example.com/", "starts with https://"},
		{"hooks.example.com", "an address such as"},
	} {
		if err := CheckURL(tc.url); err == nil || !strings.Contains(err.Error(), tc.says) {
			t.Errorf("CheckURL(%q) = %v, want it to say %q", tc.url, err, tc.says)
		}
		if err := CheckURL(tc.url); err != nil && strings.Contains(err.Error(), "pass") && strings.Contains(err.Error(), "user:pass") {
			t.Errorf("CheckURL said what signs in: %v", err)
		}
	}
	was := &resourcev1.AlertChannel{To: &resourcev1.AlertChannel_Webhook{Webhook: &resourcev1.AlertWebhook{Url: "https://hooks.example.com/t0ken", Secret: "s3cret"}}}
	is := &resourcev1.AlertChannel{To: &resourcev1.AlertChannel_Webhook{Webhook: &resourcev1.AlertWebhook{}}}
	if err := (Channel{}).Validate(is); err != nil {
		t.Fatal(err)
	}
	kept, err := (Channel{}).Keep(was, is)
	if err != nil || kept.Reason != "kept_credentials" || !proto.Equal(is, was) {
		t.Errorf("Keep = %v, %v, leaving %v", kept, err, is)
	}
	if err := (Channel{}).CheckNew(&resourcev1.AlertChannel{To: &resourcev1.AlertChannel_Webhook{Webhook: &resourcev1.AlertWebhook{}}}); err == nil {
		t.Errorf("a new webhook with no URL was taken")
	}
	if got := Target(was); got != "hooks.example.com" {
		t.Errorf("Target = %q, want the host alone", got)
	}
}

// An install starts with a rule for each condition, each valid as it is.
func TestTheDefaults(t *testing.T) {
	seen := map[resourcev1.AlertRule_Condition]bool{}
	for _, d := range Defaults() {
		if err := (Rule{}).CheckName(d.Name); err != nil {
			t.Errorf("%s: %v", d.Name, err)
		}
		r := proto.CloneOf(d.Rule)
		if err := (Rule{}).Validate(r); err != nil {
			t.Errorf("%s: %v", d.Name, err)
		}
		seen[r.GetCondition()] = true
	}
	for c := range resourcev1.AlertRule_Condition_name {
		if c != 0 && !seen[resourcev1.AlertRule_Condition(c)] {
			t.Errorf("no default rule watches for %v", resourcev1.AlertRule_Condition(c))
		}
	}
}
