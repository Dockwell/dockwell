// SPDX-License-Identifier: AGPL-3.0-only

package cron

import (
	"strings"
	"testing"
	"time"
)

func mustZone(t *testing.T, name string) *time.Location {
	t.Helper()
	loc, err := time.LoadLocation(name)
	if err != nil {
		t.Fatal(err)
	}
	return loc
}

func TestNextRunsWhenTheFieldsSay(t *testing.T) {
	utc := time.UTC
	at := func(s string) time.Time {
		t.Helper()
		v, err := time.Parse("2006-01-02 15:04 MST", s)
		if err != nil {
			t.Fatal(err)
		}
		return v
	}
	for _, c := range []struct {
		expr, after, want string
	}{
		{"0 4 * * *", "2026-11-04 14:20 UTC", "2026-11-05 04:00 UTC"},
		{"0 4 * * *", "2026-11-05 03:59 UTC", "2026-11-05 04:00 UTC"},
		{"0 4 * * *", "2026-11-05 04:00 UTC", "2026-11-06 04:00 UTC"},
		{"*/5 * * * *", "2026-11-04 14:20 UTC", "2026-11-04 14:25 UTC"},
		{"5/15 * * * *", "2026-11-04 14:21 UTC", "2026-11-04 14:35 UTC"},
		{"0 * * * *", "2026-11-04 23:30 UTC", "2026-11-05 00:00 UTC"},
		{"30 2 * * MON-FRI", "2026-11-06 03:00 UTC", "2026-11-09 02:30 UTC"}, // a Friday, then Monday
		{"0 0 * * 7", "2026-11-04 00:00 UTC", "2026-11-08 00:00 UTC"},        // 7 is Sunday
		{"0 0 1 jan *", "2026-11-04 00:00 UTC", "2027-01-01 00:00 UTC"},
		{"@hourly", "2026-11-04 14:20 UTC", "2026-11-04 15:00 UTC"},
		{"@weekly", "2026-11-04 14:20 UTC", "2026-11-08 00:00 UTC"},
		{"@monthly", "2026-11-04 14:20 UTC", "2026-12-01 00:00 UTC"},
		{"0 0 29 2 *", "2026-11-04 00:00 UTC", "2028-02-29 00:00 UTC"},
		// Both day fields restricted: either one.
		{"0 0 13 * FRI", "2026-11-01 00:00 UTC", "2026-11-06 00:00 UTC"},
		{"0 0 13 * FRI", "2026-11-10 00:00 UTC", "2026-11-13 00:00 UTC"},
		// A day field starting with *: both, as Vixie cron has it.
		{"0 0 */2 * MON", "2026-11-01 00:00 UTC", "2026-11-09 00:00 UTC"},
		{"1,2,3 0 * * *", "2026-11-04 00:01 UTC", "2026-11-04 00:02 UTC"},
	} {
		s, err := Parse(c.expr)
		if err != nil {
			t.Errorf("%q: %v", c.expr, err)
			continue
		}
		if got := s.Next(at(c.after), utc); !got.Equal(at(c.want)) {
			t.Errorf("%q after %s runs %s, want %s", c.expr, c.after, got.Format("2006-01-02 15:04 MST"), c.want)
		}
	}
}

func TestParseRefusesWhatIsntASchedule(t *testing.T) {
	for _, c := range []struct{ expr, says string }{
		{"0 4 * *", "has 4 fields"},
		{"0 4 * * * *", "has 6 fields"},
		{"60 * * * *", "60 is outside 0 to 59"},
		{"* 24 * * *", "24 is outside 0 to 23"},
		{"* * 0 * *", "0 is outside 1 to 31"},
		{"* * * 13 *", "13 is outside 1 to 12"},
		{"* * * * 8", "8 is outside 0 to 7"},
		{"*/0 * * * *", "isn't a whole number from 1"},
		{"5-1 * * * *", "runs backwards"},
		{"1,,2 * * * *", "empty item"},
		{"x * * * *", `"x" isn't a number`},
		{"@reboot", "isn't a schedule Dockwell knows"},
		{"", "has 0 fields"},
	} {
		_, err := Parse(c.expr)
		if err == nil || !strings.Contains(err.Error(), c.says) {
			t.Errorf("Parse(%q) = %v, want it to say %q", c.expr, err, c.says)
		}
	}
	never, err := Parse("0 0 30 2 *")
	if err != nil {
		t.Fatal(err)
	}
	if never.Valid() == nil || !never.Next(time.Now(), time.UTC).IsZero() {
		t.Error("30 February reads as a day that comes")
	}
}

// In Europe/Amsterdam, 29 March 2026 goes from 02:00 to 03:00, and 25 October
// 2026 from 03:00 back to 02:00.
func TestDaylightSavingSkipsNoRunAndRunsNoneTwice(t *testing.T) {
	ams := mustZone(t, "Europe/Amsterdam")
	s, err := Parse("30 2 * * *")
	if err != nil {
		t.Fatal(err)
	}
	// 02:30 doesn't come on 29 March: it runs once, as the clocks jump, at 03:00.
	got := s.Next(time.Date(2026, 3, 28, 12, 0, 0, 0, ams), ams)
	if want := time.Date(2026, 3, 29, 3, 0, 0, 0, ams); !got.Equal(want) {
		t.Errorf("on the day 02:30 is skipped it runs %s, want %s", got, want)
	}
	if again := s.Next(got, ams); !again.Equal(time.Date(2026, 3, 30, 2, 30, 0, 0, ams)) {
		t.Errorf("after that it runs %s, want 30 March 02:30", again)
	}
	// 02:30 comes twice on 25 October: the first, in summer time, alone.
	got = s.Next(time.Date(2026, 10, 24, 12, 0, 0, 0, ams), ams)
	if want := time.Date(2026, 10, 25, 0, 30, 0, 0, time.UTC); !got.Equal(want) {
		t.Errorf("on the day 02:30 comes twice it runs at %s, want the first, %s", got.UTC(), want)
	}
	if again := s.Next(got, ams); !again.Equal(time.Date(2026, 10, 26, 2, 30, 0, 0, ams)) {
		t.Errorf("after the first 02:30 it runs %s, want the next day's, not the second 02:30", again.In(ams))
	}
	// Every 30 minutes through the jump: 01:30, then 03:00 once for the missing 02:00 and 02:30, then 03:30.
	half, _ := Parse("*/30 * * * *")
	var runs []string
	for at, i := time.Date(2026, 3, 29, 1, 0, 0, 0, ams), 0; i < 4; i++ {
		at = half.Next(at, ams)
		runs = append(runs, at.In(ams).Format("15:04"))
	}
	if strings.Join(runs, " ") != "01:30 03:00 03:30 04:00" {
		t.Errorf("every 30 minutes through the jump runs at %v", runs)
	}
}

// A schedule whose hour starts with * runs by the real time, as Vixie cron runs such
// a job: through both of the hour that comes twice on 25 October, and at 02:00 in
// each, where one at set hours runs the first alone.
func TestEveryFewMinutesRunsThroughAnHourThatComesTwice(t *testing.T) {
	ams := mustZone(t, "Europe/Amsterdam")
	for expr, want := range map[string]string{
		"*/15 * * * *": "01:45 CEST, 02:00 CEST, 02:15 CEST, 02:30 CEST, 02:45 CEST, 02:00 CET",
		"0 * * * *":    "02:00 CEST, 02:00 CET, 03:00 CET, 04:00 CET, 05:00 CET, 06:00 CET",
		"0 */2 * * *":  "02:00 CEST, 02:00 CET, 04:00 CET, 06:00 CET, 08:00 CET, 10:00 CET",
	} {
		s, err := Parse(expr)
		if err != nil {
			t.Fatal(err)
		}
		var runs []string
		// 01:30 in summer time.
		for at, i := time.Date(2026, 10, 24, 23, 30, 0, 0, time.UTC), 0; i < 6; i++ {
			at = s.Next(at, ams)
			runs = append(runs, at.In(ams).Format("15:04 MST"))
		}
		if got := strings.Join(runs, ", "); got != want {
			t.Errorf("%s from 01:30 on 25 October runs at %s, want %s", expr, got, want)
		}
	}
}

func TestEveryIsHowFarApartRunsAre(t *testing.T) {
	s, _ := Parse("*/5 * * * *")
	if got := s.Every(time.Date(2026, 11, 4, 14, 20, 0, 0, time.UTC), time.UTC); got != 5*time.Minute {
		t.Errorf("every 5 minutes reads as %s apart", got)
	}
	if s.Minutes() != 12 {
		t.Errorf("every 5 minutes runs at %d minutes of an hour", s.Minutes())
	}
}
