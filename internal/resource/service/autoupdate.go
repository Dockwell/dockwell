// SPDX-License-Identifier: AGPL-3.0-only

package service

import (
	"errors"
	"fmt"
	"slices"
	"strconv"
	"time"

	"github.com/dockwell/dockwell/internal/lazyre"

	resourcev1 "github.com/dockwell/dockwell/gen/dockwell/resource/v1alpha1"
)

// The window an automatic update takes unless its spec says (ADR 0080).
const (
	DefaultUpdateAt      = "03:00"
	DefaultUpdateMinutes = 120
	minUpdateMinutes     = 15
	maxUpdateMinutes     = 24 * 60
)

var clock = lazyre.Regexp(`^([01]\d|2[0-3]):[0-5]\d$`)

// checkAutoUpdate fills in a service's automatic updates' defaults, and refuses
// a window that can't open, or an app that takes none. One that's off is left
// out, so off has one spelling.
func checkAutoUpdate(svc *resourcev1.Service) error {
	a := svc.GetAutoUpdate()
	if a == nil {
		return nil
	}
	if a.GetLevel() == resourcev1.AutoUpdate_LEVEL_UNSPECIFIED {
		svc.AutoUpdate = nil
		return nil
	}
	if svc.GetBuild() != nil {
		return errors.New("an app that builds from Git takes no automatic updates, as each deploy builds its branch's newest commit. Leave out its automatic updates")
	}
	if svc.GetRuntime() != nil {
		return errors.New("an app from a folder takes no automatic updates, as its runtime's version decides its image. Leave out its automatic updates")
	}
	return CheckAutoUpdate(a)
}

// CheckAutoUpdate fills in an automatic update's defaults, and refuses a window
// that can't open: of a service, or the install's default for new apps.
func CheckAutoUpdate(a *resourcev1.AutoUpdate) error {
	if _, ok := resourcev1.AutoUpdate_Level_name[int32(a.GetLevel())]; !ok {
		return fmt.Errorf("automatic update level %d isn't one. Use patch, minor or any", a.GetLevel())
	}
	if a.GetAt() == "" {
		a.At = DefaultUpdateAt
	}
	if !clock().MatchString(a.GetAt()) {
		return fmt.Errorf("%q isn't a time of day for automatic updates. Use 24-hour HH:MM, such as 03:00", a.GetAt())
	}
	if a.GetMinutes() == 0 {
		a.Minutes = DefaultUpdateMinutes
	}
	if a.GetMinutes() < minUpdateMinutes || a.GetMinutes() > maxUpdateMinutes {
		return fmt.Errorf("an automatic update's window lasts from %d to %d minutes, and this one asks for %d", minUpdateMinutes, maxUpdateMinutes, a.GetMinutes())
	}
	for _, d := range a.GetWeekdays() {
		if d < 0 || d > 6 {
			return fmt.Errorf("weekday %d isn't a day. Use 0 for Sunday to 6 for Saturday", d)
		}
	}
	slices.Sort(a.Weekdays)
	a.Weekdays = slices.Compact(a.Weekdays)
	if len(a.GetWeekdays()) == 7 {
		a.Weekdays = nil
	}
	if a.GetTimeZone() == "" {
		a.TimeZone = "UTC"
	}
	if _, err := time.LoadLocation(a.GetTimeZone()); err != nil {
		return fmt.Errorf("%q isn't a time zone. Use a name such as Europe/Amsterdam", a.GetTimeZone())
	}
	return nil
}

// UpdateWindow is the window an automatic update may start in that holds at, and
// whether there's one: its opening, which tells one window from the next, and its
// end. A window that opens late on one day and runs past midnight holds the small
// hours of the next, on the day it opened.
func UpdateWindow(a *resourcev1.AutoUpdate, at time.Time) (opens, ends time.Time, ok bool) {
	if a.GetLevel() == resourcev1.AutoUpdate_LEVEL_UNSPECIFIED || !clock().MatchString(a.GetAt()) {
		return time.Time{}, time.Time{}, false
	}
	zone, err := time.LoadLocation(a.GetTimeZone())
	if err != nil {
		zone = time.UTC
	}
	hour, _ := strconv.Atoi(a.GetAt()[:2])
	minute, _ := strconv.Atoi(a.GetAt()[3:])
	length := time.Duration(max(a.GetMinutes(), minUpdateMinutes)) * time.Minute
	t := at.In(zone)
	// Today's window, or yesterday's still open past midnight.
	for _, back := range []int{0, -1} {
		day := time.Date(t.Year(), t.Month(), t.Day()+back, hour, minute, 0, 0, zone)
		if len(a.GetWeekdays()) > 0 && !slices.Contains(a.GetWeekdays(), int32(day.Weekday())) { //nolint:gosec // a weekday is 0 to 6
			continue
		}
		if !at.Before(day) && at.Before(day.Add(length)) {
			return day, day.Add(length), true
		}
	}
	return time.Time{}, time.Time{}, false
}

// Takes says an automatic update at level takes an update of kind, by how far it
// moves: "patch", "minor", "major", or "release" for a catalog release of the
// same version, as ADR 0079's checks class it. A new image of a moving tag is
// classed by what its versions say before it's asked here. Any other kind, such
// as one nothing could class, counts as the largest step: only "any" takes it.
func Takes(level resourcev1.AutoUpdate_Level, kind string) bool {
	switch kind {
	case "patch", "release":
		return level >= resourcev1.AutoUpdate_LEVEL_PATCH
	case "minor":
		return level >= resourcev1.AutoUpdate_LEVEL_MINOR
	}
	return level >= resourcev1.AutoUpdate_LEVEL_ANY
}
