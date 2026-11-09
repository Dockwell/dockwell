// SPDX-License-Identifier: AGPL-3.0-only

package convert

import (
	"errors"
	"fmt"
	"strings"
)

// Class is why an app is left out, or kept at an earlier release, as the report
// counts it.
type Class string

// Every reason the converter gives belongs to one of these.
const (
	ClassScripts    Class = "Runs scripts on the server"
	ClassAnotherApp Class = "Uses a database or service from another app"
	ClassShipped    Class = "Ships config files beside its Compose file"
	ClassReaches    Class = "Reaches its server, waiting for a person to look"
	ClassWords      Class = "Converts once a person writes its summary or category"
	ClassOverlay    Class = "Its overlay no longer matches it"
	ClassYoung      Class = "Its version is under 14 days old"
	ClassAgeUnknown Class = "Its image's age isn't known yet"
	ClassNoVersion  Class = "Names no version that stays put"
	ClassArch       Class = "Runs on amd64 alone"
	ClassOwn        Class = "Dockwell has its own entry"
	ClassPlatform   Class = "Uses a value its platform fills in"
	ClassPin        Class = "Its image couldn't be pinned"
	ClassCantSay    Class = "Asks for what an entry can't say yet"
	ClassCantRun    Class = "Asks for what Dockwell can't run yet"
	ClassCheck      Class = "Fails the catalog's own checks"
	ClassFile       Class = "Its files can't be read"
	ClassMajor      Class = "Moves to a new major version, which waits for the breaking marker"
	ClassBack       Class = "Would go back to an older version"
	ClassInside     Class = "Puts a secret inside a longer value, such as a connection URL"
	ClassMadeUp     Class = "Asks for a value Dockwell doesn't make yet"
	ClassSkipped    Class = "Its source marks it as one to leave out"
	ClassTerms      Class = "Comes under terms of its own"
)

// leftOut is a reason an app is left out, with the class the report counts it in.
type leftOut struct {
	class Class
	msg   string
	// again says a later run may not have it, as when a registry didn't answer.
	again bool
}

func (e *leftOut) Error() string { return e.msg }

// leave says why an app is left out.
func leave(class Class, format string, args ...any) error {
	return &leftOut{class: class, msg: fmt.Sprintf(format, args...)}
}

// leaveForNow says why an app is left out on this run, for a reason a later run may
// not have.
func leaveForNow(class Class, format string, args ...any) error {
	return &leftOut{class: class, msg: fmt.Sprintf(format, args...), again: true}
}

// forNow says a reason is one a later run may not have.
func forNow(err error) bool {
	var l *leftOut
	return errors.As(err, &l) && l.again
}

// classOf is the class of a reason, or fallback for one that came from elsewhere,
// such as the catalog's own checks.
func classOf(err error, fallback Class) Class {
	var l *leftOut
	if errors.As(err, &l) {
		return l.class
	}
	return fallback
}

// forCatalog takes the fix an importer's error ends with, which speaks to someone
// importing a file of their own, off what a person who converts a catalog reads.
func forCatalog(msg string) string {
	if before, after, ok := strings.Cut(msg, ". Remove "); ok && strings.Contains(after, "import") {
		return before
	}
	return msg
}
