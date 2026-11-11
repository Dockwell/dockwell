// SPDX-License-Identifier: AGPL-3.0-only

package main

import (
	"context"
	"fmt"
	"os/exec"
	"strconv"
	"strings"
	"time"
)

// clock knows the real time however the server's clock is jumped, from the
// monotonic clock, which a jump doesn't move.
type clock struct {
	wall   time.Time // the wall clock at start, with no monotonic reading
	mono   time.Time // the same moment, with one
	jumped bool
	ntp    bool // whether time sync was on before the jump
}

func newClock() clock {
	now := time.Now()
	return clock{wall: now.Round(0), mono: now}
}

func (c *clock) real() time.Time { return c.wall.Add(time.Since(c.mono)) }

// ahead jumps the server's clock d ahead of the real time, with time sync off so
// nothing puts it back meanwhile.
func (c *clock) ahead(ctx context.Context, d time.Duration) error {
	b, err := exec.CommandContext(ctx, "timedatectl", "show", "--property=NTP", "--value").Output()
	if err != nil {
		return fmt.Errorf("can't read the time-sync setting before jumping the clock: %w", err)
	}
	c.ntp = strings.TrimSpace(string(b)) == "yes"
	if err := sudo(ctx, "timedatectl", "set-ntp", "false"); err != nil {
		return err
	}
	c.jumped = true
	return setClock(ctx, c.real().Add(d))
}

// back puts the server's clock and its time-sync setting back.
func (c *clock) back(ctx context.Context) error {
	if !c.jumped {
		return nil
	}
	if err := setClock(ctx, c.real()); err != nil {
		return fmt.Errorf("can't put the clock back: %w", err)
	}
	if err := sudo(ctx, "timedatectl", "set-ntp", strconv.FormatBool(c.ntp)); err != nil {
		return fmt.Errorf("can't restore the time-sync setting: %w", err)
	}
	c.jumped = false
	return nil
}

func setClock(ctx context.Context, t time.Time) error {
	return sudo(ctx, "date", "-u", "-s", fmt.Sprintf("@%d.%09d", t.Unix(), t.Nanosecond()))
}
