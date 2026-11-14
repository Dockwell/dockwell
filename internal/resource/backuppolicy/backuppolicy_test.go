// SPDX-License-Identifier: AGPL-3.0-only

package backuppolicy

import (
	"strings"
	"testing"
	"time"

	resourcev1 "github.com/dockwell/dockwell/gen/dockwell/resource/v1alpha1"
	"github.com/dockwell/dockwell/internal/resource"
)

func TestValidateFillsInAndRefuses(t *testing.T) {
	p := &resourcev1.BackupPolicy{Volume: "data"}
	if err := (Type{}).Validate(p); err != nil {
		t.Fatal(err)
	}
	if p.GetEvery() != resourcev1.BackupPolicy_EVERY_DAY || p.GetAt() != "03:00" || p.GetTimeZone() != "UTC" || p.GetKeep() != DefaultKeep {
		t.Errorf("defaults = %v", p)
	}
	for want, bad := range map[string]*resourcev1.BackupPolicy{
		"needs a volume":    {},
		"time of day":       {Volume: "v", At: "25:00"},
		"isn't a day":       {Volume: "v", Every: resourcev1.BackupPolicy_EVERY_WEEK, Weekday: 9},
		"isn't a time zone": {Volume: "v", TimeZone: "Mars/Olympus"},
		"at most":           {Volume: "v", Keep: MaxKeep + 1},
	} {
		if err := (Type{}).Validate(bad); err == nil || !strings.Contains(err.Error(), want) {
			t.Errorf("%v: %v", bad, err)
		}
	}
}

func TestNext(t *testing.T) {
	amsterdam, err := time.LoadLocation("Europe/Amsterdam")
	if err != nil {
		t.Fatal(err)
	}
	at := func(day, hour, minute int) time.Time { return time.Date(2026, 11, day, hour, minute, 0, 0, time.UTC) }
	for name, c := range map[string]struct {
		p     *resourcev1.BackupPolicy
		after time.Time
		want  time.Time
	}{
		"later today": {&resourcev1.BackupPolicy{Every: resourcev1.BackupPolicy_EVERY_DAY, At: "03:00", TimeZone: "UTC"}, at(6, 1, 0), at(6, 3, 0)},
		"tomorrow":    {&resourcev1.BackupPolicy{Every: resourcev1.BackupPolicy_EVERY_DAY, At: "03:00", TimeZone: "UTC"}, at(6, 3, 0), at(7, 3, 0)},
		"in its zone": {&resourcev1.BackupPolicy{Every: resourcev1.BackupPolicy_EVERY_DAY, At: "03:00", TimeZone: "Europe/Amsterdam"}, at(6, 1, 0), time.Date(2026, 11, 6, 3, 0, 0, 0, amsterdam)},
		"each hour":   {&resourcev1.BackupPolicy{Every: resourcev1.BackupPolicy_EVERY_HOUR, At: "03:15", TimeZone: "UTC"}, at(6, 7, 20), at(6, 8, 15)},
		"this hour":   {&resourcev1.BackupPolicy{Every: resourcev1.BackupPolicy_EVERY_HOUR, At: "03:15", TimeZone: "UTC"}, at(6, 7, 10), at(6, 7, 15)},
		// 6 Nov 2026 is a Friday.
		"this week": {&resourcev1.BackupPolicy{Every: resourcev1.BackupPolicy_EVERY_WEEK, Weekday: 0, At: "04:00", TimeZone: "UTC"}, at(6, 12, 0), at(8, 4, 0)},
		"next week": {&resourcev1.BackupPolicy{Every: resourcev1.BackupPolicy_EVERY_WEEK, Weekday: 5, At: "04:00", TimeZone: "UTC"}, at(6, 12, 0), at(13, 4, 0)},
	} {
		if got := Next(c.p, c.after); !got.Equal(c.want) {
			t.Errorf("%s: %v, want %v", name, got, c.want)
		}
	}
}

// An hourly schedule runs once an hour by the world's clock, as the local one goes back
// and forward: a time of day names two moments on one of those days, and none on the other.
func TestAnHourlyScheduleKeepsToTheHourAsTheClocksChange(t *testing.T) {
	p := &resourcev1.BackupPolicy{Volume: "data", Every: resourcev1.BackupPolicy_EVERY_HOUR, At: "03:30", TimeZone: "Europe/Amsterdam"}
	for name, start := range map[string]time.Time{
		"back, on 25 Oct 2026":    time.Date(2026, 10, 24, 21, 30, 0, 0, time.UTC),
		"forward, on 29 Mar 2026": time.Date(2026, 3, 28, 21, 30, 0, 0, time.UTC),
	} {
		at := start
		for hour := 1; hour <= 8; hour++ {
			want := start.Add(time.Duration(hour) * time.Hour)
			next := Next(p, at)
			if !next.Equal(want) {
				t.Fatalf("%s: after %s comes %s, want %s", name, at.UTC(), next.UTC(), want)
			}
			at = next
		}
	}
	// From early in the hour that happens twice: 02:15:12 summer time comes before
	// 02:30 summer time, not 02:30 winter time an hour later.
	between := time.Date(2026, 10, 25, 0, 15, 12, 5, time.UTC)
	if got, want := Next(p, between), time.Date(2026, 10, 25, 0, 30, 0, 0, time.UTC); !got.Equal(want) {
		t.Errorf("after %s comes %s, want %s", between, got, want)
	}
}

// A key in the endpoint would show in the plan, which shows the endpoint as it
// is, so it goes where the plan hides it: the access key and the secret.
func TestADestinationsEndpointHoldsNoKey(t *testing.T) {
	good := func() *resourcev1.BackupDestination {
		return &resourcev1.BackupDestination{
			Endpoint: "https://example.r2.cloudflarestorage.com", Bucket: "acme-backups", AccessKey: "AKIAEXAMPLE", SecretKey: "the-secret",
		}
	}
	if err := (Destination{}).Validate(good()); err != nil {
		t.Fatal(err)
	}
	for _, endpoint := range []string{
		"https://AKIAEXAMPLE:the-secret@example.r2.cloudflarestorage.com",
		"https://AKIAEXAMPLE@example.r2.cloudflarestorage.com",
		"https://:the-secret@example.r2.cloudflarestorage.com",
	} {
		d := good()
		d.Endpoint = endpoint
		err := (Destination{}).Validate(d)
		if err == nil || !strings.Contains(err.Error(), "access key and secret") {
			t.Errorf("%s: %v, want a refusal that says where a key goes", endpoint, err)
		}
		if err != nil && (strings.Contains(err.Error(), "the-secret") || strings.Contains(err.Error(), "AKIAEXAMPLE")) {
			t.Errorf("the refusal says what signs in: %v", err)
		}
	}
}

// A drill falls due an hour after its policy's own time: on the policy's weekday, or
// Sunday for a daily or hourly policy, weekly; an hour after the month's first run,
// monthly. One that never drills never falls due (ADR 0068).
func TestNextDrill(t *testing.T) {
	at := func(month time.Month, day, hour, minute int) time.Time {
		return time.Date(2026, month, day, hour, minute, 0, 0, time.UTC)
	}
	daily := func(drill resourcev1.BackupPolicy_Drill) *resourcev1.BackupPolicy {
		return &resourcev1.BackupPolicy{Every: resourcev1.BackupPolicy_EVERY_DAY, At: "03:00", TimeZone: "UTC", Drill: drill}
	}
	weekly, monthly := resourcev1.BackupPolicy_DRILL_WEEKLY, resourcev1.BackupPolicy_DRILL_MONTHLY
	for name, c := range map[string]struct {
		p     *resourcev1.BackupPolicy
		after time.Time
		want  time.Time
	}{
		// 6 Nov 2026 is a Friday, and 8 Nov a Sunday.
		"a daily policy, on Sunday": {daily(weekly), at(11, 6, 12, 0), at(11, 8, 4, 0)},
		"later that Sunday":         {daily(weekly), at(11, 8, 3, 30), at(11, 8, 4, 0)},
		"the Sunday after":          {daily(weekly), at(11, 8, 4, 0), at(11, 15, 4, 0)},
		"a weekly policy, on its day": {
			&resourcev1.BackupPolicy{Every: resourcev1.BackupPolicy_EVERY_WEEK, Weekday: 3, At: "22:30", TimeZone: "UTC", Drill: weekly},
			at(11, 6, 12, 0), at(11, 11, 23, 30),
		},
		"an hourly policy, on Sunday": {
			&resourcev1.BackupPolicy{Every: resourcev1.BackupPolicy_EVERY_HOUR, At: "00:15", TimeZone: "UTC", Drill: weekly},
			at(11, 6, 12, 0), at(11, 8, 1, 15),
		},
		"the month's first run": {daily(monthly), at(11, 6, 12, 0), at(12, 1, 4, 0)},
		"this month's, not yet": {daily(monthly), at(12, 1, 2, 0), at(12, 1, 4, 0)},
		"a weekly policy's first of the month": {
			&resourcev1.BackupPolicy{Every: resourcev1.BackupPolicy_EVERY_WEEK, Weekday: 1, At: "03:00", TimeZone: "UTC", Drill: monthly},
			at(11, 6, 12, 0), at(12, 7, 4, 0),
		},
	} {
		if got := NextDrill(c.p, c.after); !got.Equal(c.want) {
			t.Errorf("%s: %v, want %v", name, got, c.want)
		}
	}
	if got := NextDrill(daily(resourcev1.BackupPolicy_DRILL_NEVER), at(11, 6, 12, 0)); !got.IsZero() {
		t.Errorf("a policy that never drills falls due %v", got)
	}
	if got := NextDrill(daily(resourcev1.BackupPolicy_DRILL_UNSPECIFIED), at(11, 6, 12, 0)); !got.IsZero() {
		t.Errorf("a policy from before drills falls due %v", got)
	}
}

// A policy that's new checks a restore weekly unless it says otherwise; one that says
// never, or monthly, keeps that.
func TestANewPolicyDrillsWeekly(t *testing.T) {
	for given, want := range map[resourcev1.BackupPolicy_Drill]resourcev1.BackupPolicy_Drill{
		resourcev1.BackupPolicy_DRILL_UNSPECIFIED: resourcev1.BackupPolicy_DRILL_WEEKLY,
		resourcev1.BackupPolicy_DRILL_NEVER:       resourcev1.BackupPolicy_DRILL_NEVER,
		resourcev1.BackupPolicy_DRILL_MONTHLY:     resourcev1.BackupPolicy_DRILL_MONTHLY,
	} {
		p := &resourcev1.BackupPolicy{Volume: "data", Drill: given}
		Type{}.DefaultNew(resource.Ref{}, p)
		if p.GetDrill() != want {
			t.Errorf("a new policy given %v drills %v, want %v", given, p.GetDrill(), want)
		}
	}
}

// A policy backs up a volume or an app's database alone, by its link: never both,
// never neither. One of a database alone stops nothing and isn't drilled, and names
// its link, so the link can't go while it does (ADR 0084).
func TestAPolicyOfAnAppsDatabase(t *testing.T) {
	for _, p := range []*resourcev1.BackupPolicy{{}, {Volume: "data", Link: "db"}} {
		if err := (Type{}).Validate(p); err == nil {
			t.Errorf("%v is valid", p)
		}
	}
	p := &resourcev1.BackupPolicy{Link: "db", Stop: true}
	(Type{}).DefaultNew(resource.Ref{}, p)
	if err := (Type{}).Validate(p); err != nil {
		t.Fatal(err)
	}
	if p.GetStop() || p.GetDrill() != resourcev1.BackupPolicy_DRILL_NEVER {
		t.Errorf("a policy of an app's database is %v", p)
	}
	if refs := (Type{}).References(p); len(refs) != 1 || refs[0].Kind != "link" || refs[0].Name != "db" {
		t.Errorf("it names %v", refs)
	}
}
