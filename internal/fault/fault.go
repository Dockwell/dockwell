// SPDX-License-Identifier: AGPL-3.0-only

// Package fault gives an error a kind apart from its message, and tests a way
// to stop a sequence partway. A package names the kinds of error its callers
// act on, such as revision.ErrStale, and writes each message whole, for
// people: the fact, then the consequence, then the fix
// (design/system/README.md). The kind's own text is never part of the message,
// so an API can pass the message on as it is, and a caller still tells the kind
// with errors.Is.
package fault

import "fmt"

// New returns an error of kind whose message is format's, as fmt.Errorf
// would make it, %w included.
func New(kind error, format string, args ...any) error {
	return &kinded{kind: kind, err: fmt.Errorf(format, args...)}
}

type kinded struct{ kind, err error }

func (k *kinded) Error() string { return k.err.Error() }

// Unwrap gives the kind, and what the message wraps.
func (k *kinded) Unwrap() []error { return []error{k.kind, k.err} }

// Points are the named steps of a sequence, where a test can stop it, as by
// killing the process at each one in turn to see what is left. Nil does
// nothing, as it is everywhere but in a test.
type Points func(name string)

// Reach says the sequence got to a step.
func (p Points) Reach(name string) {
	if p != nil {
		p(name)
	}
}
