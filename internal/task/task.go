// SPDX-License-Identifier: AGPL-3.0-only

// Package task is the "task" kind: work that runs on a schedule on each server it's
// placed on, as ADR 0053 decides. Type checks a spec, which both builds do; Driver
// is the agent's, which keeps each task's spec on the server's disk, keeps its time,
// runs it, and keeps its runs until the control plane reads them.
package task

import (
	"errors"
	"fmt"
	"net/url"
	"path"
	"strings"
	"time"
	_ "time/tzdata" // a server's own zone files aren't always there, and a zone means the same everywhere
	"unicode/utf8"

	"google.golang.org/protobuf/proto"

	resourcev1 "github.com/dockwell/dockwell/gen/dockwell/resource/v1alpha1"
	dockwellv1 "github.com/dockwell/dockwell/gen/dockwell/v1"
	"github.com/dockwell/dockwell/internal/cron"
	"github.com/dockwell/dockwell/internal/resource"
)

// Kind is the task kind.
const Kind resource.Kind = "task"

const (
	// DefaultTimeout is how long a run may take unless its task says.
	DefaultTimeout = 10 * time.Minute
	// MaxTimeout is the longest a task may let a run take.
	MaxTimeout = 24 * time.Hour
	// MaxRetries is how many more times a run may try.
	MaxRetries = 3
	// maxCommand is the longest command a task holds, in bytes.
	maxCommand = 8 << 10
	// maxDisplayName is the longest name people call a task by, in characters.
	maxDisplayName = 80
)

// Type is what both builds know of tasks: their spec, and how to check one.
type Type struct{}

var (
	_ resource.Type         = Type{}
	_ resource.IDPrefixer   = Type{}
	_ resource.HostAccesser = Type{}
	_ resource.WhatRunner   = Type{}
	_ resource.Referrer     = Type{}
)

// Kind returns "task".
func (Type) Kind() resource.Kind { return Kind }

// Level is the environment, as a service's: a task is the same model as one.
func (Type) Level() resource.Level { return resource.LevelEnvironment }

// IDPrefix is "tsk".
func (Type) IDPrefix() string { return "tsk" }

// New returns an empty Task.
func (Type) New() proto.Message { return &resourcev1.Task{} }

// KindOf says what a task runs.
func KindOf(t *resourcev1.Task) dockwellv1.TaskKind {
	switch t.GetRuns().(type) {
	case *resourcev1.Task_Shell:
		return dockwellv1.TaskKind_TASK_KIND_SHELL
	case *resourcev1.Task_App:
		return dockwellv1.TaskKind_TASK_KIND_APP
	case *resourcev1.Task_Request:
		return dockwellv1.TaskKind_TASK_KIND_REQUEST
	}
	return dockwellv1.TaskKind_TASK_KIND_UNSPECIFIED
}

// Validate checks what a task runs and when, and fills in its defaults, so specs
// that mean the same compare equal.
func (Type) Validate(spec proto.Message) error {
	t := spec.(*resourcev1.Task)
	if err := checkRuns(t); err != nil {
		return err
	}
	s, err := cron.Parse(t.GetSchedule())
	if err != nil {
		return err
	}
	if err := s.Valid(); err != nil {
		return fmt.Errorf("the schedule %q never runs: %w. Choose days that come, such as 1 to 28", t.GetSchedule(), err)
	}
	t.Schedule = strings.Join(strings.Fields(t.GetSchedule()), " ")
	if t.GetTimeZone() == "" {
		t.TimeZone = "UTC"
	}
	if _, err := time.LoadLocation(t.GetTimeZone()); err != nil || t.GetTimeZone() == "Local" || strings.HasPrefix(t.GetTimeZone(), "/") {
		return fmt.Errorf("%q isn't a time zone. Use a name such as Europe/Amsterdam", t.GetTimeZone())
	}
	if t.GetTimeoutSeconds() == 0 {
		t.TimeoutSeconds = uint32(DefaultTimeout / time.Second)
	}
	if time.Duration(t.GetTimeoutSeconds())*time.Second > MaxTimeout {
		return fmt.Errorf("a run may take %s at most, and %s is longer. Split the work, or run it more often", MaxTimeout, time.Duration(t.GetTimeoutSeconds())*time.Second)
	}
	if t.GetRetries() > MaxRetries {
		return fmt.Errorf("a run tries again %d times at most, not %d", MaxRetries, t.GetRetries())
	}
	if utf8.RuneCountInString(t.GetDisplayName()) > maxDisplayName || strings.ContainsAny(t.GetDisplayName(), "\r\n") {
		return fmt.Errorf("a task's name for people is one line of %d characters at most", maxDisplayName)
	}
	t.DisplayName = strings.TrimSpace(t.GetDisplayName())
	return nil
}

// checkRuns checks what a task runs.
func checkRuns(t *resourcev1.Task) error {
	switch r := t.GetRuns().(type) {
	case *resourcev1.Task_Shell:
		c := strings.TrimSpace(r.Shell.GetCommand())
		switch {
		case c == "":
			return errors.New("a shell task needs a command, such as find /var/log/app -mtime +7 -delete")
		case len(c) > maxCommand:
			return fmt.Errorf("a task's command is %d KB at most. Put a longer one in a script on the server, and run that", maxCommand>>10)
		case strings.ContainsRune(c, 0):
			return errors.New("a task's command can't hold a NUL byte")
		}
		r.Shell.Command = c
		if d := r.Shell.GetWorkingDir(); d != "" && (!path.IsAbs(d) || path.Clean(d) != d) {
			return fmt.Errorf("%q isn't a folder on the server. Use a full path, such as /srv/app", d)
		}
	case *resourcev1.Task_App:
		if err := resource.CheckLabel("app", r.App.GetService(), "web"); err != nil {
			return fmt.Errorf("an app's task needs the app it runs in: %w", err)
		}
		words, err := Words(r.App.GetCommand())
		switch {
		case err != nil:
			return err
		case len(words) == 0:
			return errors.New("an app's task needs a command, such as /app/api report sales")
		case len(r.App.GetCommand()) > maxCommand:
			return fmt.Errorf("a task's command is %d KB at most", maxCommand>>10)
		}
		r.App.Command = strings.TrimSpace(r.App.GetCommand())
	case *resourcev1.Task_Request:
		return checkRequest(r.Request)
	default:
		return errors.New("a task runs a shell command on the server, a command in an app, or an HTTP request. Give it one")
	}
	return nil
}

// checkRequest checks an HTTP request's URL, method and status.
func checkRequest(r *resourcev1.TaskRequest) error {
	u, err := url.Parse(strings.TrimSpace(r.GetUrl()))
	if err != nil || (u.Scheme != "http" && u.Scheme != "https") || u.Host == "" {
		return fmt.Errorf("%q isn't a URL a task can ask. Use http:// or https://, such as https://example.com/cron", r.GetUrl())
	}
	if u.User != nil {
		return errors.New("a task's URL can't hold a user or password, as everyone who reads the task sees it")
	}
	r.Url = u.String()
	switch m := strings.ToUpper(r.GetMethod()); m {
	case "":
		r.Method = "GET"
	case "GET", "HEAD", "POST":
		r.Method = m
	default:
		return fmt.Errorf("a task asks a URL with GET, HEAD or POST, not %s", r.GetMethod())
	}
	if s := r.GetExpectStatus(); s != 0 && (s < 100 || s > 599) {
		return fmt.Errorf("%d isn't an HTTP status. Use one from 100 to 599, such as 200, or leave it out for any 2xx", s)
	}
	return nil
}

// HostAccess is root on the server, for a shell command: the agent runs it as root.
// An app's task reaches what its app does, through References, and a request
// reaches nothing on the server.
func (Type) HostAccess(spec proto.Message) []resource.Access {
	if _, ok := spec.(*resourcev1.Task).GetRuns().(*resourcev1.Task_Shell); ok {
		return []resource.Access{{Kind: resource.AccessServerShell, SameAsRoot: true}}
	}
	return nil
}

// WhatRuns is what a task runs, without when, how long or what it's called, so a
// new schedule for a shell command asks nothing, and a new command asks again.
func (Type) WhatRuns(spec proto.Message) proto.Message {
	t := proto.Clone(spec).(*resourcev1.Task) //nolint:forcetypeassert // a clone has its original's type
	t.Schedule, t.TimeZone, t.TimeoutSeconds, t.Retries = "", "", 0, 0
	t.Paused, t.SkipMissed, t.DisplayName = false, false, ""
	return t
}

// References names an app's task's app, which has to run where the task does. The
// task reaches what the app reaches, as it runs with its mounts.
func (Type) References(spec proto.Message) []resource.Reference {
	if a, ok := spec.(*resourcev1.Task).GetRuns().(*resourcev1.Task_App); ok {
		return []resource.Reference{{Kind: "service", Name: a.App.GetService(), Verb: "run in"}}
	}
	return nil
}

// ReferenceKinds is services.
func (Type) ReferenceKinds() []resource.Kind { return []resource.Kind{"service"} }

// Timeout is how long a task's run may take.
func Timeout(t *resourcev1.Task) time.Duration {
	if t.GetTimeoutSeconds() == 0 {
		return DefaultTimeout
	}
	return time.Duration(t.GetTimeoutSeconds()) * time.Second
}

// Next is when a task next runs after after, or the zero time for one whose
// schedule or zone doesn't read, which Validate refuses.
func Next(t *resourcev1.Task, after time.Time) time.Time {
	s, err := cron.Parse(t.GetSchedule())
	if err != nil {
		return time.Time{}
	}
	loc, err := time.LoadLocation(t.GetTimeZone())
	if err != nil {
		return time.Time{}
	}
	return s.Next(after, loc)
}

// Words splits a command as a shell splits it into words: on spaces, with single
// quotes taking what's in them as it is, double quotes and backslashes as a shell
// has them, and nothing else a shell does.
func Words(command string) ([]string, error) {
	var (
		out     []string
		word    strings.Builder
		inWord  bool
		quote   rune
		escaped bool
	)
	for _, c := range command {
		switch {
		case escaped:
			if quote == '"' && c != '"' && c != '\\' && c != '$' && c != '`' {
				word.WriteRune('\\')
			}
			word.WriteRune(c)
			escaped = false
		case quote == '\'':
			if c == '\'' {
				quote = 0
			} else {
				word.WriteRune(c)
			}
		case c == '\\':
			escaped, inWord = true, true
		case quote == '"':
			if c == '"' {
				quote = 0
			} else {
				word.WriteRune(c)
			}
		case c == '\'' || c == '"':
			quote, inWord = c, true
		case c == ' ' || c == '\t' || c == '\n':
			if inWord {
				out = append(out, word.String())
				word.Reset()
				inWord = false
			}
		default:
			word.WriteRune(c)
			inWord = true
		}
	}
	switch {
	case quote != 0:
		return nil, fmt.Errorf("the command %q leaves a %c open. Close it", command, quote)
	case escaped:
		return nil, fmt.Errorf("the command %q ends with a backslash", command)
	}
	if inWord {
		out = append(out, word.String())
	}
	return out, nil
}
