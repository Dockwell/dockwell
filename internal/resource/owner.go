// SPDX-License-Identifier: AGPL-3.0-only

package resource

import (
	"fmt"
	"strconv"
	"strings"
)

// MaxID is the highest user or group number Dockwell takes: Linux's, less the one
// that means none.
const MaxID = 1<<32 - 2

// ParseOwner reads a user and group by number, as "uid:gid", such as "82:82".
func ParseOwner(s string) (uid, gid uint32, err error) {
	u, g, ok := strings.Cut(strings.TrimSpace(s), ":")
	if !ok {
		return 0, 0, fmt.Errorf("%q isn't a user and group: give both by number, as uid:gid, such as 82:82", s)
	}
	if uid, err = ParseID(u); err != nil {
		return 0, 0, err
	}
	if gid, err = ParseID(g); err != nil {
		return 0, 0, err
	}
	return uid, gid, nil
}

// ParseID reads a user's or a group's number, however it's written, such as 82 or
// 0082, and refuses what isn't one.
func ParseID(s string) (uint32, error) {
	n, err := strconv.ParseUint(strings.TrimSpace(s), 10, 64)
	if err != nil || n > MaxID || strings.TrimSpace(s) == "" || strings.ContainsAny(s, "+-") {
		return 0, fmt.Errorf("%q isn't a user's or a group's number. Give one from 0 to %d, such as 82", s, uint64(MaxID))
	}
	return uint32(n), nil
}

// FormatOwner writes a user and group as Dockwell keeps them: "82:82".
func FormatOwner(uid, gid uint32) string {
	return strconv.FormatUint(uint64(uid), 10) + ":" + strconv.FormatUint(uint64(gid), 10)
}
