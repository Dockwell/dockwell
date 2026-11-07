// SPDX-License-Identifier: AGPL-3.0-only

package docker

import (
	"strconv"
	"strings"
)

// RunsAs is what a container's user comes to, by root.
type RunsAs int

const (
	// RunsAsOther is a user other than root, by number.
	RunsAsOther RunsAs = iota
	// RunsAsRoot is root, by number or by name.
	RunsAsRoot
	// RunsAsNamed is a user by a name other than root, which only the image's
	// /etc/passwd says the number of, and it can say 0.
	RunsAsNamed
	// RunsAsImage is whatever user the image runs as, which is root unless the
	// image says otherwise.
	RunsAsImage
)

// UserRunsAs reads a container's user as Docker's runtime does: a name or a number,
// and a group after a colon, which says nothing of the user. A number is always an
// ID, read as strconv.Atoi reads it, so 00, +0 and 0:0 are root as much as 0 and
// root are. A user left out before the colon is the runtime's default, which is
// root. It returns the name for a user by name, which goes by the image.
func UserRunsAs(user string) (RunsAs, string) {
	if user == "" {
		return RunsAsImage, ""
	}
	name, _, _ := strings.Cut(user, ":")
	if name == "" || name == "root" {
		return RunsAsRoot, ""
	}
	if id, err := strconv.Atoi(name); err == nil {
		if id == 0 {
			return RunsAsRoot, ""
		}
		return RunsAsOther, ""
	}
	return RunsAsNamed, name
}
