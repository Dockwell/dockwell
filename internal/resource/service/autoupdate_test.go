// SPDX-License-Identifier: AGPL-3.0-only

package service

import (
	"strings"
	"testing"
	"time"

	resourcev1 "github.com/dockwell/dockwell/gen/dockwell/resource/v1alpha1"
)

// An automatic update's window fills in its defaults, keeps its days in order,
// and refuses what can't open; off has one spelling, which is none.
func TestAnAutomaticUpdatesWindowIsChecked(t *testing.T) {
	d := &Driver{}
	svc := func(a *resourcev1.AutoUpdate) *resourcev1.Service {
		return &resourcev1.Service{Runs: &resourcev1.Service_App{App: &resourcev1.Container{Image: "nginx:1.27"}}, AutoUpdate: a}
	}
	s := svc(&resourcev1.AutoUpdate{Level: resourcev1.AutoUpdate_LEVEL_PATCH, Weekdays: []int32{6, 0, 6}})
	if err := d.Validate(s); err != nil {
		t.Fatal(err)
	}
	a := s.GetAutoUpdate()
	if a.GetAt() != "03:00" || a.GetMinutes() != 120 || a.GetTimeZone() != "UTC" || len(a.GetWeekdays()) != 2 || a.GetWeekdays()[0] != 0 {
		t.Errorf("filled in as %v", a)
	}
	off := svc(&resourcev1.AutoUpdate{At: "04:00"})
	if err := d.Validate(off); err != nil || off.GetAutoUpdate() != nil {
		t.Errorf("off is %v, %v", off.GetAutoUpdate(), err)
	}
	for _, c := range []struct {
		a    *resourcev1.AutoUpdate
		want string
	}{
		{&resourcev1.AutoUpdate{Level: resourcev1.AutoUpdate_LEVEL_ANY, At: "3am"}, "isn't a time of day"},
		{&resourcev1.AutoUpdate{Level: resourcev1.AutoUpdate_LEVEL_ANY, Minutes: 5}, "from 15 to 1440 minutes"},
		{&resourcev1.AutoUpdate{Level: resourcev1.AutoUpdate_LEVEL_ANY, Weekdays: []int32{7}}, "weekday 7 isn't a day"},
		{&resourcev1.AutoUpdate{Level: resourcev1.AutoUpdate_LEVEL_ANY, TimeZone: "Mars/Olympus"}, "isn't a time zone"},
	} {
		if err := d.Validate(svc(c.a)); err == nil || !strings.Contains(err.Error(), c.want) {
			t.Errorf("%v = %v, want %q", c.a, err, c.want)
		}
	}
}

// A window holds from its opening for its length, on its days, in its zone, and
// one that runs past midnight holds the small hours of the next day for the day it
// opened.
func TestAnUpdateWindowHoldsItsHours(t *testing.T) {
	a := &resourcev1.AutoUpdate{Level: resourcev1.AutoUpdate_LEVEL_PATCH, At: "23:30", Minutes: 120, TimeZone: "America/Los_Angeles", Weekdays: []int32{6}}
	la, _ := time.LoadLocation("America/Los_Angeles")
	saturday := time.Date(2026, 11, 14, 23, 45, 0, 0, la)
	opens, ends, ok := UpdateWindow(a, saturday)
	if !ok || !opens.Equal(time.Date(2026, 11, 14, 23, 30, 0, 0, la)) || !ends.Equal(opens.Add(2*time.Hour)) {
		t.Errorf("Saturday 23:45 is in %v to %v, %t", opens, ends, ok)
	}
	if o, _, ok := UpdateWindow(a, saturday.Add(time.Hour)); !ok || !o.Equal(opens) {
		t.Errorf("Sunday 00:45 is in %v, %t, want Saturday's window", o, ok)
	}
	if _, _, ok := UpdateWindow(a, saturday.Add(2*time.Hour)); ok {
		t.Error("the window holds after it ends")
	}
	if _, _, ok := UpdateWindow(a, saturday.Add(-24*time.Hour)); ok {
		t.Error("the window opens on a day it doesn't")
	}
	if _, _, ok := UpdateWindow(&resourcev1.AutoUpdate{At: "23:30", Minutes: 120}, saturday); ok {
		t.Error("an update that's off has a window")
	}
}

// A level takes what moves as far as it says, and no further.
func TestALevelTakesWhatItSays(t *testing.T) {
	for _, c := range []struct {
		level resourcev1.AutoUpdate_Level
		kind  string
		takes bool
	}{
		// A moving tag's new image that nothing classed counts as major.
		{resourcev1.AutoUpdate_LEVEL_PATCH, "digest", false},
		{resourcev1.AutoUpdate_LEVEL_MINOR, "digest", false},
		{resourcev1.AutoUpdate_LEVEL_ANY, "digest", true},
		{resourcev1.AutoUpdate_LEVEL_PATCH, "release", true},
		{resourcev1.AutoUpdate_LEVEL_PATCH, "patch", true},
		{resourcev1.AutoUpdate_LEVEL_PATCH, "minor", false},
		{resourcev1.AutoUpdate_LEVEL_MINOR, "minor", true},
		{resourcev1.AutoUpdate_LEVEL_MINOR, "major", false},
		{resourcev1.AutoUpdate_LEVEL_ANY, "major", true},
		{resourcev1.AutoUpdate_LEVEL_UNSPECIFIED, "patch", false},
		{resourcev1.AutoUpdate_LEVEL_MINOR, "commit", false},
	} {
		if got := Takes(c.level, c.kind); got != c.takes {
			t.Errorf("%v takes %s: %t", c.level, c.kind, got)
		}
	}
}
