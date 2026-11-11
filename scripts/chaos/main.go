// SPDX-License-Identifier: AGPL-3.0-only

// Command chaos runs the chaos tests: it breaks a stand of Dockwell at bad moments,
// puts it right, and checks that what ADR 0054 promises holds (docs/testing/chaos.md).
//
//	go run ./scripts/chaos bin/dockwell bin/dockwell-agent
//
// It runs from the repository's root, on this machine's Docker. CHAOS_FAKE_DOCKER
// names the fake Engine's binary to run on instead, for the scenarios that need no
// real one; CHAOS_ONLY runs the scenarios whose names start with it; CHAOS_ROUNDS
// runs each that many times; CHAOS_KEEP keeps the stand's folder. The scenarios that
// kill Docker, fill the disk or jump the clock need CHAOS_HOST_FAULTS=1 and root
// through sudo -n. CHAOS_REQUIRE_ALL=1 fails if a selected scenario is skipped.
package main

import (
	"context"
	"fmt"
	"os"
	"os/signal"
	"strconv"
	"strings"
	"syscall"
	"time"
)

func main() {
	if len(os.Args) != 3 {
		fmt.Fprintln(os.Stderr, "usage: go run ./scripts/chaos path/to/dockwell path/to/dockwell-agent")
		os.Exit(2)
	}
	ctx, stopSignals := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	code := run(ctx, os.Args[1], os.Args[2])
	stopSignals()
	os.Exit(code)
}

func run(ctx context.Context, bin, agentBin string) int {
	rounds := 1
	if v := os.Getenv("CHAOS_ROUNDS"); v != "" {
		n, err := strconv.Atoi(v)
		if err != nil || n < 1 {
			say("CHAOS_ROUNDS=%q isn't a number of rounds. Set it to 1 or more", v)
			return 2
		}
		rounds = n
	}
	only := os.Getenv("CHAOS_ONLY")
	start := time.Now()
	s, err := newStand(ctx, bin, agentBin, os.Getenv("CHAOS_FAKE_DOCKER"))
	if s != nil {
		defer s.close()
	}
	if err != nil {
		if s == nil {
			say("can't set up the stand: %v", err)
		} else {
			s.keep = true
			say("can't set up the stand: %v. Its logs are in %s", err, s.dir)
		}
		return 1
	}
	say("the stand is up in %s", since(start))
	var failed, skipped []string
	ran := 0
	for round := 1; round <= rounds && ctx.Err() == nil; round++ {
		for _, sc := range scenarios {
			if !strings.HasPrefix(sc.name, only) || ctx.Err() != nil {
				continue
			}
			if why := s.cant(sc); why != "" {
				if round == 1 {
					say("SKIP %s: %s", sc.name, why)
					skipped = append(skipped, sc.name)
				}
				continue
			}
			ran++
			began := time.Now()
			say("--- %s (round %d)", sc.name, round)
			if err := s.play(ctx, sc); err != nil {
				s.keep = true
				say("FAIL %s after %s: %v", sc.name, since(began), err)
				s.dump(sc.name, round)
				failed = append(failed, fmt.Sprintf("%s (round %d)", sc.name, round))
				if err := s.recover(ctx); err != nil {
					say("the stand didn't come back after %s, so the rest can't run: %v", sc.name, err)
					return 1
				}
				continue
			}
			say("PASS %s in %s", sc.name, since(began))
		}
	}
	say("ran %d scenarios in %s; %d failed, %d skipped", ran, since(start), len(failed), len(skipped))
	switch {
	case ctx.Err() != nil:
		say("the run was interrupted: %v", ctx.Err())
		return 1
	case ran == 0:
		say("no scenario ran: CHAOS_ONLY=%q names none this machine can run", only)
		return 1
	case len(failed) > 0:
		say("failed: %s. The stand's logs are in %s", strings.Join(failed, ", "), s.dir)
		return 1
	case len(skipped) > 0 && os.Getenv("CHAOS_REQUIRE_ALL") == "1":
		say("required scenarios were skipped: %s", strings.Join(skipped, ", "))
		return 1
	}
	return 0
}
