// SPDX-License-Identifier: AGPL-3.0-only

package apicli

import (
	"bufio"
	"context"
	"errors"
	"fmt"
	"io"
	"strconv"
	"strings"
	"time"

	"github.com/dockwell/dockwell/internal/resource"
)

// Env is what the API commands run with, so tests can substitute it.
type Env struct {
	Out, Err io.Writer
	In       io.Reader
	Getenv   func(string) string
	// Interactive reports whether a person is at a terminal to answer
	// questions: standard input and standard error are both terminals.
	Interactive func() bool
	// ReadSecret asks for a value on the terminal without showing it as it
	// is typed. Nil reads standard input as it is.
	ReadSecret func(prompt string) (string, error)
	// Connect finds the control plane and the API token to call it with,
	// from what the flags say. For local access, Renew asks for a new token,
	// after the control plane rejected the one it had, and Moved asks where
	// the control plane is now, after it couldn't be reached.
	Connect func(ctx context.Context, s Settings) (*Client, error)
	// Kinds knows each kind of resource, for where its resources live.
	Kinds *resource.Registry
	// Now is the time, for "3 min ago".
	Now func() time.Time

	lines *bufio.Reader
	// commands names each method's command in the API's messages.
	commands *strings.Replacer
}

// Settings are what the flags say about which control plane to call, and
// with which token. Empty fields come from the environment, the config
// file, or local access.
type Settings struct {
	URL   string
	Token string
	// Renew asks local access for a new token, and Moved for where the
	// control plane answers now.
	Renew, Moved bool
	// Locate asks only where the control plane is, for a message: the
	// client it gives has no token, and local access makes none.
	Locate bool
}

// UsageError marks a mistake in how a command was invoked.
type UsageError struct{ Err error }

func (e UsageError) Error() string { return e.Err.Error() }
func (e UsageError) Unwrap() error { return e.Err }

func usage(err error) error { return UsageError{err} }

// ExitError is an error with its own exit status, for an outcome scripts
// tell apart from a failure.
type ExitError struct {
	Code int
	Err  error
}

func (e ExitError) Error() string { return e.Err.Error() }
func (e ExitError) Unwrap() error { return e.Err }

// ExitNeedsPerson is the exit status of a command that stopped because
// what it would do needs a person to approve it in the panel.
const ExitNeedsPerson = 4

func (e *Env) now() time.Time {
	if e.Now != nil {
		return e.Now()
	}
	return time.Now()
}

func (e *Env) interactive() bool { return e.Interactive != nil && e.Interactive() }

// errNoAnswer says a question went unanswered.
var errNoAnswer = errors.New("standard input ended before an answer, so dockwell stopped, and nothing changed. " +
	"Answer at a terminal, or pass the answer as a flag")

// readLine reads one answer from standard input.
func (e *Env) readLine() (string, error) {
	if e.lines == nil {
		e.lines = bufio.NewReader(e.In)
	}
	line, err := e.lines.ReadString('\n')
	if err != nil && (line == "" || !errors.Is(err, io.EOF)) {
		if errors.Is(err, io.EOF) {
			return "", errNoAnswer
		}
		return "", fmt.Errorf("can't read your answer from standard input: %w", err)
	}
	return strings.TrimSpace(line), nil
}

// confirm asks a yes-or-no question on standard error. Only yes, or y,
// says yes.
func (e *Env) confirm(question string) (bool, error) {
	fmt.Fprintf(e.Err, "%s (y/N) ", question)
	answer, err := e.readLine()
	if errors.Is(err, errNoAnswer) {
		// No answer isn't yes.
		fmt.Fprintln(e.Err)
		return false, nil
	}
	if err != nil {
		return false, err
	}
	switch strings.ToLower(answer) {
	case "y", "yes":
		return true, nil
	}
	return false, nil
}

// choose asks which of options, by number or by name, until it gets one.
func (e *Env) choose(question string, options []string) (string, error) {
	for {
		fmt.Fprintf(e.Err, "%s\n", question)
		for i, o := range options {
			fmt.Fprintf(e.Err, "  %d  %s\n", i+1, o)
		}
		fmt.Fprintf(e.Err, "Enter a number or a name: ")
		answer, err := e.readLine()
		if err != nil {
			return "", err
		}
		for i, o := range options {
			if answer == o || answer == strconv.Itoa(i+1) {
				return o, nil
			}
		}
		fmt.Fprintf(e.Err, "%q isn't one of them.\n", answer)
	}
}
