// SPDX-License-Identifier: AGPL-3.0-only

package backuppolicy

import (
	"strings"
	"testing"
	"time"

	resourcev1 "github.com/dockwell/dockwell/gen/dockwell/resource/v1alpha1"
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
