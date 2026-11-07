// SPDX-License-Identifier: AGPL-3.0-only

package docker

import (
	"math"
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
	// /etc/passwd says the number of, and it can say 0. A number past what a user
	// can be counts as one, as the image can list it, and Docker takes what it
	// lists as 32 bits: 4294967296 is 0 there.
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
	id, err := strconv.Atoi(name)
	switch {
	case err == nil && id == 0:
		return RunsAsRoot, ""
	case err == nil && id > 0 && id <= math.MaxInt32:
		return RunsAsOther, ""
	}
	return RunsAsNamed, name
}
