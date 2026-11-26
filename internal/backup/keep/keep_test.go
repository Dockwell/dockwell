// SPDX-License-Identifier: AGPL-3.0-only

package keep

import (
	"slices"
	"testing"
	"time"

	"github.com/dockwell/dockwell/internal/id"
)

func TestRetentionKeepsTwentyFourAndSevenDailyCopies(t *testing.T) {
	now := time.Date(2026, 11, 26, 19, 0, 0, 0, time.UTC)
	phrase := id.New("phrase")
	newest := Name(now, id.New("cbk"), phrase)
	names := []string{newest}
	for i := 1; i < 24; i++ {
		names = append(names, Name(now.Add(-time.Duration(i)*time.Minute), id.New("cbk"), phrase))
	}
	for day := 1; day <= 10; day++ {
		for j := range 3 {
			names = append(names, Name(now.AddDate(0, 0, -day).Add(time.Duration(j)*time.Minute), id.New("cbk"), phrase))
		}
	}
	rotated := Name(now.Add(-30*time.Minute), id.New("cbk"), id.New("phrase"))
	names = append(names, rotated)
	// A failed write didn't leave the new name in this place.
	if drop := Expired(names, Name(now, id.New("cbk"), phrase)); len(drop) != 0 {
		t.Fatalf("a failed write removed %v", drop)
	}
	drop := Expired(names, newest)
	if len(names)-len(drop) != 31 || !slices.Contains(drop, rotated) || slices.Contains(drop, newest) {
		t.Fatalf("kept %d bundles, drop = %v", len(names)-len(drop), drop)
	}
}
