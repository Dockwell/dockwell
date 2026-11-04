// SPDX-License-Identifier: AGPL-3.0-only

// Package human formats values the way Dockwell writes them for people:
// a number, a space and a unit ("41 s", "30 min", "1 h").
package human

import (
	"strconv"
	"time"
)

// Duration writes d in the largest unit that keeps it exact: "1 h",
// "90 min", "45 s", "1.5 s".
func Duration(d time.Duration) string {
	switch {
	case d >= time.Hour && d%time.Hour == 0:
		return strconv.FormatInt(int64(d/time.Hour), 10) + " h"
	case d >= time.Minute && d%time.Minute == 0:
		return strconv.FormatInt(int64(d/time.Minute), 10) + " min"
	default:
		return strconv.FormatFloat(d.Seconds(), 'f', -1, 64) + " s"
	}
}
