// SPDX-License-Identifier: AGPL-3.0-only

package apicli

import (
	"strconv"
	"strings"
	"time"
)

// Times and sizes as the panel words them (design/system/README.md and
// web/src/lib): a number, a space and a unit, rounded to what a person
// acts on, and relative times under a day, then dates.

const day = 24 * time.Hour

// Ago says how long ago t was: "1 s ago" at the least, then "3 min ago",
// "2 h ago", and from a day on a date, "12 Oct", with the year when it
// isn't this one. A time a little ahead, from a clock that is off, reads
// "1 s ago".
func Ago(t, now time.Time) string {
	d := now.Sub(t)
	switch {
	case d < time.Minute:
		return strconv.Itoa(max(1, int(d/time.Second))) + " s ago"
	case d < time.Hour:
		return strconv.Itoa(int(d/time.Minute)) + " min ago"
	case d < day:
		return strconv.Itoa(int(d/time.Hour)) + " h ago"
	}
	return Date(t, now)
}

// In says when t comes: "in 40 s", "in 12 min", "in 5 hours", and from a
// day on, "on 12 Oct".
func In(t, now time.Time) string {
	d := t.Sub(now)
	if d >= day {
		return "on " + Date(t, now)
	}
	return "in " + Span(max(d, 0))
}

// Date writes a day the way the product does: "12 Oct", with the year when
// it isn't now's.
func Date(t, now time.Time) string {
	t = t.Local()
	if t.Year() != now.Local().Year() {
		return t.Format("2 Jan 2006")
	}
	return t.Format("2 Jan")
}

// Span writes a length of time rounded down to what a person acts on: "40
// s", "12 min", "5 hours", "41 days".
func Span(d time.Duration) string {
	switch {
	case d < time.Minute:
		return strconv.Itoa(int(d/time.Second)) + " s"
	case d < time.Hour:
		return strconv.Itoa(int(d/time.Minute)) + " min"
	case d < day:
		return plural(int(d/time.Hour), "hour", "hours")
	}
	return plural(int(d/day), "day", "days")
}

// Elapsed writes how long something took: "under 0.1 s", "0.4 s", "12
// s", then as Span.
func Elapsed(d time.Duration) string {
	if d < 100*time.Millisecond {
		return "under 0.1 s"
	}
	if d < 10*time.Second {
		return strconv.FormatFloat(d.Seconds(), 'f', 1, 64) + " s"
	}
	return Span(d)
}

func plural(n int, one, many string) string {
	if n == 1 {
		return "1 " + one
	}
	return strconv.Itoa(n) + " " + many
}

var byteUnits = []string{"B", "KB", "MB", "GB", "TB", "PB"}

// unitFor is the largest unit that keeps n at 1 or more once rounded, so
// 1,023.6 KB reads "1 MB".
func unitFor(n float64) int {
	unit := 0
	for unit < len(byteUnits)-1 && roundTo(n/pow1024(unit), 0) >= 1024 {
		unit++
	}
	return unit
}

func pow1024(unit int) float64 {
	v := 1.0
	for range unit {
		v *= 1024
	}
	return v
}

func roundTo(v float64, digits int) float64 {
	s := strconv.FormatFloat(v, 'f', digits, 64)
	r, _ := strconv.ParseFloat(s, 64)
	return r
}

// amountIn writes n in unit: one decimal below 10, none above, and no
// trailing zero.
func amountIn(n float64, unit int) string {
	v := n / pow1024(unit)
	digits := 0
	if unit > 0 && v < 10 {
		digits = 1
	}
	s := strconv.FormatFloat(v, 'f', digits, 64)
	if strings.Contains(s, ".") {
		s = strings.TrimSuffix(strings.TrimRight(s, "0"), ".")
	}
	return s
}

// Bytes writes a size in the largest unit that keeps it readable: "512
// MB", "2.1 GB", "18 GB".
func Bytes(n uint64) string {
	f := float64(n)
	unit := unitFor(f)
	return amountIn(f, unit) + " " + byteUnits[unit]
}

// BytesOf writes a part of a whole in the whole's unit: "7.4 of 8 GB".
func BytesOf(part, whole uint64) string {
	unit := unitFor(float64(whole))
	return amountIn(float64(part), unit) + " of " + amountIn(float64(whole), unit) + " " + byteUnits[unit]
}

// Percent writes a share of 100 as a whole percentage: "12%".
func Percent(v float64) string { return strconv.Itoa(int(v+0.5)) + "%" }

// List joins names as messages do: "a", "a and b", "a, b and c".
func List(items []string, conjunction string) string {
	switch len(items) {
	case 0:
		return ""
	case 1:
		return items[0]
	}
	return strings.Join(items[:len(items)-1], ", ") + " " + conjunction + " " + items[len(items)-1]
}
