// SPDX-License-Identifier: AGPL-3.0-only

// Package cron reads a five-field cron schedule and says when it next runs, in a
// time zone, as ADR 0053 decides: lists, ranges, steps and names, the @ words but
// @reboot, both day fields either-or as in Vixie cron, nothing more often than a
// minute. Both builds link it, so the control plane and the agent agree on every
// next run.
package cron

import (
	"errors"
	"fmt"
	"math/bits"
	"strconv"
	"strings"
	"time"
)

// Schedule is a parsed cron schedule.
type Schedule struct {
	minute, hour, dom, month, dow uint64
	// domStar and dowStar say a day field starts with *. As in Vixie cron, a day
	// then has to match both fields; with neither, either one is enough.
	domStar, dowStar bool
	// hourStar says the hour field starts with *, which runs by the clock's real
	// time through a change of daylight saving, as Vixie cron runs such a job.
	hourStar bool
}

// field is one of the five, with its range and names.
type field struct {
	name     string
	min, max int
	names    []string // names[i] is min+i
}

var fields = [5]field{
	{name: "minute", min: 0, max: 59},
	{name: "hour", min: 0, max: 23},
	{name: "day of the month", min: 1, max: 31},
	{name: "month", min: 1, max: 12, names: []string{"JAN", "FEB", "MAR", "APR", "MAY", "JUN", "JUL", "AUG", "SEP", "OCT", "NOV", "DEC"}},
	{name: "day of the week", min: 0, max: 7, names: []string{"SUN", "MON", "TUE", "WED", "THU", "FRI", "SAT"}},
}

// words are the @ schedules.
var words = map[string]string{
	"@yearly":   "0 0 1 1 *",
	"@annually": "0 0 1 1 *",
	"@monthly":  "0 0 1 * *",
	"@weekly":   "0 0 * * 0",
	"@daily":    "0 0 * * *",
	"@midnight": "0 0 * * *",
	"@hourly":   "0 * * * *",
}

// Parse reads a schedule, such as "30 2 * * *" or "@daily".
func Parse(expr string) (Schedule, error) {
	s := strings.TrimSpace(expr)
	if strings.HasPrefix(s, "@") {
		w, ok := words[strings.ToLower(s)]
		if !ok {
			return Schedule{}, fmt.Errorf("%q isn't a schedule Dockwell knows. Use five fields, such as 0 4 * * *, or @hourly, @daily, @weekly, @monthly or @yearly", s)
		}
		s = w
	}
	parts := strings.Fields(s)
	if len(parts) != 5 {
		return Schedule{}, fmt.Errorf("%q has %d fields, and a schedule has five: minute, hour, day of the month, month and day of the week, such as 0 4 * * *", expr, len(parts))
	}
	var out Schedule
	sets := [5]*uint64{&out.minute, &out.hour, &out.dom, &out.month, &out.dow}
	for i, p := range parts {
		set, err := parseField(fields[i], p)
		if err != nil {
			return Schedule{}, err
		}
		*sets[i] = set
	}
	// Sunday is 0 and 7.
	if out.dow&(1<<7) != 0 {
		out.dow = out.dow&^(1<<7) | 1
	}
	out.domStar = strings.HasPrefix(parts[2], "*")
	out.dowStar = strings.HasPrefix(parts[4], "*")
	out.hourStar = strings.HasPrefix(parts[1], "*")
	return out, nil
}

// parseField reads one field as a set of the values it takes.
func parseField(f field, text string) (uint64, error) {
	var set uint64
	for _, item := range strings.Split(text, ",") {
		if item == "" {
			return 0, fmt.Errorf("the %s %q has an empty item between commas", f.name, text)
		}
		rng, stepText, hasStep := strings.Cut(item, "/")
		step := 1
		if hasStep {
			n, err := strconv.Atoi(stepText)
			if err != nil || n < 1 {
				return 0, fmt.Errorf("the %s %q steps by %q, which isn't a whole number from 1", f.name, text, stepText)
			}
			step = n
		}
		lo, hi := f.min, f.max
		if f.name == "day of the week" {
			hi = 6 // * means Sunday to Saturday once
		}
		if rng != "*" {
			a, b, isRange := strings.Cut(rng, "-")
			var err error
			if lo, err = f.value(a); err != nil {
				return 0, fmt.Errorf("the %s %q: %w", f.name, text, err)
			}
			hi = lo
			if isRange {
				if hi, err = f.value(b); err != nil {
					return 0, fmt.Errorf("the %s %q: %w", f.name, text, err)
				}
				if hi < lo {
					return 0, fmt.Errorf("the %s %q runs backwards, from %d to %d", f.name, text, lo, hi)
				}
			} else if hasStep {
				// "5/15" is from 5 to the end, every 15.
				hi = f.max
			}
		}
		for v := lo; v <= hi; v += step {
			set |= 1 << uint(v)
		}
	}
	return set, nil
}

// value reads a number or a name in a field's range.
func (f field) value(text string) (int, error) {
	for i, n := range f.names {
		if strings.EqualFold(text, n) {
			return f.min + i, nil
		}
	}
	n, err := strconv.Atoi(text)
	if err != nil {
		return 0, fmt.Errorf("%q isn't a number", text)
	}
	if n < f.min || n > f.max {
		return 0, fmt.Errorf("%d is outside %d to %d", n, f.min, f.max)
	}
	return n, nil
}

// errNever is a schedule no date matches, such as 30 February.
var errNever = errors.New("no date matches it")

// Next is the first time after after when the schedule runs, in loc, to the minute.
// A schedule at set hours runs at a wall-clock time that a change of daylight saving
// skips once, at the first minute after the change, and at one that comes twice the
// first time only. One whose hour field starts with *, such as every 15 minutes,
// runs by the real time, as Vixie cron has it: through both of an hour that comes
// twice, and not in one that's skipped. It's the zero time for a schedule no date in
// the next eight years matches.
func (s Schedule) Next(after time.Time, loc *time.Location) time.Time {
	t, err := s.next(after, loc)
	if err != nil {
		return time.Time{}
	}
	return t
}

// Valid says whether any date matches the schedule, so it ever runs.
func (s Schedule) Valid() error {
	if _, err := s.next(time.Date(2000, 1, 1, 0, 0, 0, 0, time.UTC), time.UTC); err != nil {
		return err
	}
	return nil
}

func (s Schedule) next(after time.Time, loc *time.Location) (time.Time, error) {
	if loc == nil {
		loc = time.UTC
	}
	if s.hourStar {
		return s.nextReal(after, loc)
	}
	local := after.In(loc)
	// Walk wall-clock minutes from the one after, as cron does, skipping whole
	// months, days and hours that can't match.
	wall := time.Date(local.Year(), local.Month(), local.Day(), local.Hour(), local.Minute(), 0, 0, time.UTC).Add(time.Minute)
	end := wall.AddDate(8, 0, 0)
	for wall.Before(end) {
		switch {
		case s.month&(1<<uint(wall.Month())) == 0:
			wall = time.Date(wall.Year(), wall.Month()+1, 1, 0, 0, 0, 0, time.UTC)
			continue
		case !s.dayMatches(wall):
			wall = time.Date(wall.Year(), wall.Month(), wall.Day()+1, 0, 0, 0, 0, time.UTC)
			continue
		case s.hour&(1<<uint(wall.Hour())) == 0:
			wall = time.Date(wall.Year(), wall.Month(), wall.Day(), wall.Hour()+1, 0, 0, 0, time.UTC)
			continue
		case s.minute&(1<<uint(wall.Minute())) == 0:
			wall = wall.Add(time.Minute)
			continue
		}
		if at := instant(wall, loc); at.After(after) {
			return at, nil
		}
		wall = wall.Add(time.Minute)
	}
	return time.Time{}, errNever
}

// nextReal walks real minutes, as they read in loc, skipping whole months, days and
// hours that can't match.
func (s Schedule) nextReal(after time.Time, loc *time.Location) (time.Time, error) {
	at := after.Truncate(time.Minute).Add(time.Minute)
	end := at.AddDate(8, 0, 0)
	for at.Before(end) {
		wall := at.In(loc)
		var next time.Time
		switch {
		case s.month&(1<<uint(wall.Month())) == 0:
			next = time.Date(wall.Year(), wall.Month()+1, 1, 0, 0, 0, 0, loc)
		case !s.dayMatches(wall):
			next = time.Date(wall.Year(), wall.Month(), wall.Day()+1, 0, 0, 0, 0, loc)
		case s.hour&(1<<uint(wall.Hour())) == 0:
			next = at.Add(time.Duration(60-wall.Minute()) * time.Minute)
		case s.minute&(1<<uint(wall.Minute())) == 0:
			next = at.Add(time.Minute)
		default:
			return at, nil
		}
		// A midnight a change of daylight saving skips reads as a time before it.
		if !next.After(at) {
			next = at.Add(time.Minute)
		}
		at = next
	}
	return time.Time{}, errNever
}

// dayMatches applies both day fields, as Vixie cron does.
func (s Schedule) dayMatches(wall time.Time) bool {
	dom := s.dom&(1<<uint(wall.Day())) != 0
	dow := s.dow&(1<<uint(wall.Weekday())) != 0
	if s.domStar || s.dowStar {
		return dom && dow
	}
	return dom || dow
}

// instant is when a wall-clock minute happens in loc: the first time, where a change
// of daylight saving makes it come twice, and the minute the clocks jumped to, where
// one skipped it.
func instant(wall time.Time, loc *time.Location) time.Time {
	at := time.Date(wall.Year(), wall.Month(), wall.Day(), wall.Hour(), wall.Minute(), 0, 0, loc)
	var first time.Time
	for _, shift := range []time.Duration{-2 * time.Hour, -time.Hour, -30 * time.Minute, 0, 30 * time.Minute, time.Hour, 2 * time.Hour} {
		if c := at.Add(shift); sameWall(c.In(loc), wall) && (first.IsZero() || c.Before(first)) {
			first = c
		}
	}
	if first.IsZero() {
		return firstAfterGap(wall, loc)
	}
	return first
}

// sameWall says t reads as wall's date, hour and minute.
func sameWall(t, wall time.Time) bool {
	return t.Year() == wall.Year() && t.Month() == wall.Month() && t.Day() == wall.Day() && t.Hour() == wall.Hour() && t.Minute() == wall.Minute()
}

// firstAfterGap is the first instant after a gap a change of daylight saving
// made around wall: the minute the clocks jumped to.
func firstAfterGap(wall time.Time, loc *time.Location) time.Time {
	// The gap starts at the last minute before wall that exists; the instant one
	// minute after that is the first wall-clock time after the gap.
	for m := 1; m <= 24*60; m++ {
		before := wall.Add(-time.Duration(m) * time.Minute)
		at := time.Date(before.Year(), before.Month(), before.Day(), before.Hour(), before.Minute(), 0, 0, loc)
		if sameWall(at, before) {
			return at.Add(time.Minute)
		}
	}
	return time.Date(wall.Year(), wall.Month(), wall.Day(), wall.Hour(), wall.Minute(), 0, 0, loc)
}

// Every is how many minutes apart the schedule's first two runs after at are, a
// rough measure of how often it runs, for a timeline. 0 for one that never runs.
func (s Schedule) Every(at time.Time, loc *time.Location) time.Duration {
	a := s.Next(at, loc)
	if a.IsZero() {
		return 0
	}
	return s.Next(a, loc).Sub(a)
}

// Minutes is how many minutes of an hour the schedule runs at, for words such as
// "every 5 minutes".
func (s Schedule) Minutes() int { return bits.OnesCount64(s.minute) }
