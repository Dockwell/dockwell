// SPDX-License-Identifier: AGPL-3.0-only

// Package human formats values the way Dockwell writes them for people:
// a number, a space and a unit ("41 s", "30 min", "1 h"), and lists of
// names ("hz-1, hz-2 and hz-3").
package human

import (
	"strconv"
	"strings"
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

// List writes names the way messages do: "a", "a and b", "a, b and c",
// and "none" for none.
func List(names []string) string {
	switch len(names) {
	case 0:
		return "none"
	case 1:
		return names[0]
	}
	return strings.Join(names[:len(names)-1], ", ") + " and " + names[len(names)-1]
}

// Either is List for a choice: "a, b or c".
func Either(names []string) string {
	if len(names) < 2 {
		return List(names)
	}
	return strings.Join(names[:len(names)-1], ", ") + " or " + names[len(names)-1]
}

// Size says how many bytes, in the largest unit that keeps it above 1, 1024-based,
// with one decimal under 10, such as "12 MB" or "1.2 GB".
func Size(bytes int64) string {
	units := []string{"B", "KB", "MB", "GB", "TB"}
	f, i := float64(bytes), 0
	for f >= 1024 && i < len(units)-1 {
		f /= 1024
		i++
	}
	if i > 0 && f < 10 {
		return strconv.FormatFloat(f, 'f', 1, 64) + " " + units[i]
	}
	return strconv.FormatFloat(f, 'f', 0, 64) + " " + units[i]
}
