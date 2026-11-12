// SPDX-License-Identifier: AGPL-3.0-only

package resource

import "errors"

// Failure is a step's failure said twice: in plain words, for whoever hit it,
// and as the chain of errors that led there, for whoever digs in. Plain is
// whole, in the shape design/system/README.md asks for: the fact, then the
// consequence, then the fix, in sentences, with no IDs. It names the server
// only as "this server", as a driver doesn't know its name; the engine says
// which, once there is a choice.
//
// Error gives the chain, so a log keeps all of it. Explain gives the two
// apart, which is what a plan's step and a change's error show.
//
// Reason says what it comes down to, when that is known, so whoever shows it
// can say what to do about it where they are: a person on a form that has only
// an image can't check a command. It's one of the Reason constants.
type Failure struct {
	Plain  string
	Err    error
	Reason string
}

// What a failure can come down to.
const (
	ReasonImage     = "image"     // the image: its name, its tag, or its platform
	ReasonCommand   = "command"   // what the image runs isn't there, or can't run
	ReasonExits     = "exits"     // it started, and stopped or keeps restarting
	ReasonUnhealthy = "unhealthy" // it runs, and fails its health check
	ReasonPort      = "port"      // a port it publishes is in use
	ReasonMount     = "mount"     // a volume or a path it mounts
	ReasonBuild     = "build"     // building its image from source failed
	ReasonCancelled = "cancelled" // someone cancelled its build
	// ReasonRegistry is the registry's own trouble: it didn't answer a pull
	// properly, so the same pull may work in a while, and nothing about the image
	// needs changing.
	ReasonRegistry = "registry"
	// ReasonDisk is a server without room for the image a step would pull (ADR
	// 0080).
	ReasonDisk = "disk"
	// ReasonDatabaseRelease is a database whose new image runs a release its data
	// can't move to in place, as PostgreSQL 16's to 17 (ADR 0080).
	ReasonDatabaseRelease = "database_release"
)

func (f *Failure) Error() string { return f.Err.Error() }

func (f *Failure) Unwrap() error { return f.Err }

// Plainly says err in plain words. A failure already said so keeps its words,
// which were written nearer to what happened.
func Plainly(plain string, err error) error {
	var f *Failure
	if err == nil || errors.As(err, &f) {
		return err
	}
	return &Failure{Plain: plain, Err: err}
}

// PlainlyBecause is Plainly for a failure that comes down to something known.
func PlainlyBecause(reason, plain string, err error) error {
	var f *Failure
	if err == nil || errors.As(err, &f) {
		return err
	}
	return &Failure{Plain: plain, Err: err, Reason: reason}
}

// ReasonOf says what err comes down to, or nothing when that isn't known.
func ReasonOf(err error) string {
	var f *Failure
	if errors.As(err, &f) {
		return f.Reason
	}
	return ""
}

// Explain gives what a person reads about err, and the chain behind it when
// that says more. An error nobody put in plain words is its own message,
// which is written whole already, with nothing behind it.
func Explain(err error) (plain, detail string) {
	var f *Failure
	if errors.As(err, &f) {
		return f.Plain, err.Error()
	}
	return err.Error(), ""
}
