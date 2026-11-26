// SPDX-License-Identifier: AGPL-3.0-only

// Package keep names sealed control backups and selects expired copies.
package keep

import (
	"fmt"
	"slices"
	"strconv"
	"strings"
	"time"

	"github.com/dockwell/dockwell/internal/id"
)

// Prefix is the directory reserved for one install and generation.
func Prefix(install string, generation int64) string {
	return fmt.Sprintf("dockwell-control/%s/%d/", install, generation)
}

// Name orders bundles by capture time and records which phrase sealed them.
func Name(at time.Time, bundle, phrase string) string {
	return fmt.Sprintf("%019d-%s-%s.dwcb", at.UnixMilli(), bundle, phrase)
}

// Valid reports whether the parts can name a bundle without escaping its directory.
func Valid(install string, generation int64, name string) bool {
	_, err := id.Parse("ins", install)
	_, _, ok := parse(name)
	return err == nil && generation > 0 && ok
}

func parse(name string) (time.Time, string, bool) {
	base, ok := strings.CutSuffix(name, ".dwcb")
	if !ok {
		return time.Time{}, "", false
	}
	parts := strings.Split(base, "-")
	if len(parts) != 3 || len(parts[0]) != 19 {
		return time.Time{}, "", false
	}
	ms, err := strconv.ParseInt(parts[0], 10, 64)
	_, bundleErr := id.Parse("cbk", parts[1])
	_, phraseErr := id.Parse("phrase", parts[2])
	if err != nil || ms <= 0 || bundleErr != nil || phraseErr != nil {
		return time.Time{}, "", false
	}
	at := time.UnixMilli(ms).UTC()
	return at, parts[2], name == Name(at, parts[1], parts[2])
}

// Taken reads the capture time of a valid bundle name.
func Taken(name string) (time.Time, bool) {
	at, _, ok := parse(name)
	return at, ok
}

// Expired chooses copies older than the newly completed one in the same place
// and generation. A newly completed bundle retires older passphrase copies.
func Expired(names []string, newest string) []string {
	at, phrase, ok := parse(newest)
	if !ok || !slices.Contains(names, newest) {
		return nil
	}
	slices.Sort(names)
	slices.Reverse(names)
	days := make(map[int64]bool, 7)
	var drop []string
	recent := 0
	oldestRecent := at.Unix() / 86400
	for _, name := range names {
		t, key, valid := parse(name)
		if !valid {
			continue
		}
		switch {
		case name >= newest:
			recent++
		case key != phrase:
			drop = append(drop, name)
		case recent < 24:
			recent++
			oldestRecent = t.Unix() / 86400
		default:
			day := t.Unix() / 86400
			age := oldestRecent - day
			if age <= 0 || age > 7 || days[day] {
				drop = append(drop, name)
			} else {
				days[day] = true
			}
		}
	}
	return drop
}
