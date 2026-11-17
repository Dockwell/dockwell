// SPDX-License-Identifier: AGPL-3.0-only

// Package ownerstep runs the steps that make a catalog app's owner, and reads
// whether it's made, against the app's container, and hides what they sent from
// what the app says back. The agent runs them; the control plane and the catalog
// check what an entry's steps may be. See
// docs/adr/0105-a-catalog-apps-owner-is-made-at-install.md.
package ownerstep

import (
	"bytes"
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"html"
	"io"
	"maps"
	"net"
	"net/http"
	"net/url"
	"regexp"
	"slices"
	"strconv"
	"strings"
	"time"
	"unicode/utf8"

	resourcev1 "github.com/dockwell/dockwell/gen/dockwell/resource/v1alpha1"
)

// MaxSaid is how much of what an app says is kept, after it's scrubbed.
const MaxSaid = 512

// maxAnswer is how much of an answer or a command's output is read at all.
const maxAnswer = 64 << 10

// RequestWait bounds one request, and CommandWait one command.
const (
	RequestWait = 30 * time.Second
	CommandWait = 2 * time.Minute
)

// Target is the app's running container, as a step reaches it.
type Target interface {
	// Exec runs words in the container as user, with env added to its environment,
	// and returns what it printed, both streams, and how it exited.
	Exec(ctx context.Context, words []string, user string, env []string) ([]byte, int, error)
	// Address is where the container's port answers, as host:port, from the agent.
	Address(ctx context.Context, port uint32) (string, error)
}

// Result is what came of running an owner's steps.
type Result struct {
	// Made: the steps succeeded, and done read it where the owner says made_when_done.
	Made bool
	// Waiting: the steps succeeded, but done doesn't read it made yet.
	Waiting bool
	// Step is the one that failed, from 1, the sign-up steps' after the owner's.
	Step int
	// Said is what the app said, scrubbed, then cut to MaxSaid.
	Said string
}

// names finds ${NAME} in a template.
var names = regexp.MustCompile(`\$\{([A-Za-z_][A-Za-z0-9_]*)\}`)

// Run makes the owner: it runs o's steps, then its sign-up steps, with values for
// what they name, against t. With checkFirst, it reads done first and runs no
// owner step where it reads made, as a second try after one that stopped part-way
// does. Values a request captures live in its memory alone, for the steps after it.
func Run(ctx context.Context, t Target, o *resourcev1.OwnerRun, values map[string]string, checkFirst bool) Result {
	vals := prepared(o, values)
	captured := map[string]string{}
	defer clear(vals)
	defer clear(captured)
	fail := func(step int, said []byte, err error) Result {
		return Result{Step: step, Said: Scrub(saying(err, said), vals, captured, o)}
	}
	made := false
	if checkFirst && o.GetDone() != nil {
		made, _, _ = check(ctx, t, o, o.GetDone(), vals)
	}
	if !made {
		for i, step := range o.GetSteps() {
			if said, err := runStep(ctx, t, o, step, vals, captured); err != nil {
				return fail(i+1, said, err)
			}
		}
	}
	steps := len(o.GetSteps())
	if signups := o.GetSignupsSteps(); len(signups) > 0 {
		closed := false
		if checkFirst && o.GetSignupsCheck() != nil {
			closed, _, _ = check(ctx, t, o, o.GetSignupsCheck(), vals)
		}
		if !closed {
			for i, step := range signups {
				if said, err := runStep(ctx, t, o, step, vals, captured); err != nil {
					return fail(steps+i+1, said, err)
				}
			}
		}
		if c := o.GetSignupsCheck(); c != nil {
			yes, said, err := check(ctx, t, o, c, vals)
			if !yes {
				if err == nil {
					err = errors.New("its sign-ups still read open")
				}
				return fail(steps+len(signups), said, err)
			}
		}
	}
	if o.GetMadeWhenDone() {
		if yes, _, _ := check(ctx, t, o, o.GetDone(), vals); !yes {
			return Result{Waiting: true}
		}
	}
	return Result{Made: true}
}

// Read reads c, as an owner's done or its sign-ups' check, and says yes or, where
// it can't read, why, scrubbed.
func Read(ctx context.Context, t Target, o *resourcev1.OwnerRun, c *resourcev1.OwnerCheck, values map[string]string) (bool, string) {
	if c == nil {
		return false, "nothing says how to read it"
	}
	values = prepared(o, values)
	defer clear(values)
	yes, said, err := check(ctx, t, o, c, values)
	if yes {
		return true, ""
	}
	return false, Scrub(saying(err, said), values, nil, o)
}

// saying is what a failure says: its error, then what the app said, which Scrub
// then hides values in and cuts.
func saying(err error, said []byte) []byte {
	if err == nil {
		return said
	}
	if len(bytes.TrimSpace(said)) == 0 {
		return []byte(err.Error())
	}
	return slices.Concat([]byte(err.Error()+": "), said)
}

// Basic is what a step names the owner's Basic credential by: their user, or email,
// and password, as base64 writes "user:password", for a check that signs in as
// them, as Gitea's API takes it.
const Basic = "DOCKWELL_OWNER_BASIC"

// BasicOf is what a step names a captured value's Basic credential by, with the
// owner's user, or email: DOCKWELL_BASIC_ and the captured name, such as a password
// the app made up, which a request signs in with before it sets the owner's own.
const BasicOf = "DOCKWELL_BASIC_"

// basics adds, for each value a step captured, its Basic credential with the
// owner's user, or email.
func basics[V any](o *resourcev1.OwnerRun, names map[string]V, captured map[string]string) {
	who := o.GetUser()
	if who == "" {
		who = o.GetEmail()
	}
	for name := range names {
		if v := captured[name]; v != "" && who != "" {
			captured[BasicOf+name] = base64.StdEncoding.EncodeToString([]byte(who + ":" + v))
		}
	}
}

// Host and Scheme are what a command reads where the app is reached by: its host,
// such as "docs.example.com", and "https" or "http".
const (
	Host   = "DOCKWELL_HOST"
	Scheme = "DOCKWELL_SCHEME"
)

// prepared is a copy of values with the owner's Basic credential, where there's a
// password to make it from, and where the app is reached.
func prepared(o *resourcev1.OwnerRun, values map[string]string) map[string]string {
	out := maps.Clone(values)
	if out == nil {
		out = map[string]string{}
	}
	who := o.GetUser()
	if who == "" {
		who = o.GetEmail()
	}
	if pw := values[o.GetPassword()]; pw != "" && who != "" {
		out[Basic] = base64.StdEncoding.EncodeToString([]byte(who + ":" + pw))
	}
	out[Host], out[Scheme] = o.GetHost(), scheme(o)
	return out
}

func scheme(o *resourcev1.OwnerRun) string {
	if o.GetHttps() {
		return "https"
	}
	return "http"
}

func runStep(ctx context.Context, t Target, o *resourcev1.OwnerRun, step *resourcev1.OwnerStep, values, captured map[string]string) ([]byte, error) {
	// A step whose work a try that stopped after it did is skipped, so a second try
	// goes on to the steps after it (the re-check's N6).
	if u := step.GetUnless(); u != nil {
		all := maps.Clone(values)
		maps.Copy(all, captured)
		yes, _, _ := check(ctx, t, o, u, all)
		clear(all)
		if yes {
			return nil, nil
		}
	}
	switch {
	case step.GetCommand() != nil:
		out, err := runCommand(ctx, t, step.GetCommand(), values, captured)
		if err != nil {
			return out, err
		}
		if err := captureOutput(out, step.GetCommand().GetCapture(), captured); err != nil {
			return out, err
		}
		basics(o, step.GetCommand().GetCapture(), captured)
		return out, nil
	case step.GetRequest() != nil:
		status, answer, err := send(ctx, t, o, step.GetRequest(), values, captured)
		if err != nil {
			return answer, err
		}
		if !wanted(status, step.GetRequest().GetExpectStatus()) {
			return answer, fmt.Errorf("it answered %d", status)
		}
		if err := capture(answer, step.GetRequest().GetCapture(), captured); err != nil {
			return answer, err
		}
		basics(o, step.GetRequest().GetCapture(), captured)
		return answer, nil
	default:
		return nil, errors.New("a step is neither a command nor a request")
	}
}

// runCommand runs a command with every value in its environment, never its words.
func runCommand(ctx context.Context, t Target, c *resourcev1.OwnerCommand, values, captured map[string]string) ([]byte, error) {
	if c.GetOneOff() {
		return nil, errors.New("this agent can't run a step as a one-off container yet")
	}
	env := make([]string, 0, len(values)+len(captured))
	for _, m := range []map[string]string{values, captured} {
		for _, name := range slices.Sorted(maps.Keys(m)) {
			env = append(env, name+"="+m[name])
		}
	}
	defer clear(env)
	ctx, cancel := context.WithTimeout(ctx, CommandWait)
	defer cancel()
	out, code, err := t.Exec(ctx, c.GetWords(), c.GetUser(), env)
	if err != nil {
		return out, err
	}
	if code != 0 {
		return out, fmt.Errorf("it exited with %d", code)
	}
	return out, nil
}

// direct sends a request to the container alone: through no proxy the agent's
// environment names, as one would see a body with a password in it, on a connection
// of its own, and following no redirect.
var direct = &http.Client{
	Timeout: RequestWait,
	Transport: &http.Transport{
		Proxy:             nil,
		DialContext:       (&net.Dialer{Timeout: 10 * time.Second}).DialContext,
		DisableKeepAlives: true,
	},
	CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse },
}

// send makes a request to the container, and says its status and its answer.
func send(ctx context.Context, t Target, o *resourcev1.OwnerRun, r *resourcev1.OwnerRequest, values, captured map[string]string) (int, []byte, error) {
	addr, err := t.Address(ctx, r.GetPort())
	if err != nil {
		return 0, nil, err
	}
	all := maps.Clone(values)
	if all == nil {
		all = map[string]string{}
	}
	maps.Copy(all, captured)
	defer clear(all)
	path := Fill(r.GetPath(), all, url.PathEscape)
	if !strings.HasPrefix(path, "/") {
		path = "/" + path
	}
	var body []byte
	if r.GetBody() != "" {
		body = []byte(Fill(r.GetBody(), all, encoderFor(r.GetContentType())))
	}
	defer clear(body)
	req, err := http.NewRequestWithContext(ctx, cmpOr(r.GetMethod(), http.MethodGet), "http://"+addr+path, bytes.NewReader(body))
	if err != nil {
		return 0, nil, err
	}
	if r.GetContentType() != "" {
		req.Header.Set("Content-Type", r.GetContentType())
	}
	for _, name := range slices.Sorted(maps.Keys(r.GetHeaders())) {
		value := Fill(r.GetHeaders()[name], all, func(s string) string { return s })
		if strings.EqualFold(name, "Host") {
			req.Host = value
			continue
		}
		req.Header.Set(name, value)
	}
	// Where the app is reached, as a proxy in front of it would say, whatever the
	// entry says (ADR 0103, 0105).
	if o.GetHost() != "" {
		req.Host = o.GetHost()
	}
	req.Header.Set("X-Forwarded-Proto", scheme(o))
	res, err := direct.Do(req)
	if err != nil {
		return 0, nil, err
	}
	defer res.Body.Close()
	answer, err := io.ReadAll(io.LimitReader(res.Body, maxAnswer))
	if err != nil {
		return res.StatusCode, answer, err
	}
	if res.StatusCode >= 300 && res.StatusCode < 400 {
		return res.StatusCode, answer, fmt.Errorf("it answered %d, a redirect, which a step never follows", res.StatusCode)
	}
	return res.StatusCode, answer, nil
}

// check reads c: its request's status is the one it wants, and its answer holds
// what it says.
func check(ctx context.Context, t Target, o *resourcev1.OwnerRun, c *resourcev1.OwnerCheck, values map[string]string) (bool, []byte, error) {
	if cmd := c.GetCommand(); cmd != nil {
		out, err := runCommand(ctx, t, cmd, values, nil)
		if err != nil {
			return false, out, err
		}
		if c.GetContains() != "" && !bytes.Contains(out, []byte(c.GetContains())) {
			return false, out, errors.New("what it printed doesn't say so")
		}
		return true, out, nil
	}
	status, answer, err := send(ctx, t, o, c.GetRequest(), values, nil)
	if err != nil {
		return false, answer, err
	}
	if !wanted(status, c.GetStatus()) {
		return false, answer, fmt.Errorf("it answered %d", status)
	}
	if c.GetContains() != "" && !bytes.Contains(answer, []byte(c.GetContains())) {
		return false, answer, errors.New("its answer doesn't say so")
	}
	return true, answer, nil
}

func wanted(status int, want uint32) bool {
	if want != 0 {
		return status == int(want)
	}
	return status >= 200 && status < 300
}

func cmpOr(a, b string) string {
	if a != "" {
		return a
	}
	return b
}

// encoderFor writes a value as a body of the type writes one: JSON escapes it inside
// the string the template quotes, and a form escapes it as a query does.
func encoderFor(contentType string) func(string) string {
	switch {
	case strings.HasPrefix(contentType, "application/json"):
		return jsonInside
	case strings.HasPrefix(contentType, "application/x-www-form-urlencoded"):
		return url.QueryEscape
	default:
		return func(s string) string { return s }
	}
}

// jsonInside is s as JSON writes it inside a string, without the quotes.
func jsonInside(s string) string {
	b, _ := json.Marshal(s)
	return string(b[1 : len(b)-1])
}

// Fill writes each ${NAME} in template as encode writes its value. A name with no
// value is left as it is, which a step's own answer then says.
func Fill(template string, values map[string]string, encode func(string) string) string {
	return names.ReplaceAllStringFunc(template, func(m string) string {
		name := m[2 : len(m)-1]
		if v, ok := values[name]; ok {
			return encode(v)
		}
		return m
	})
}

// capture keeps each field of a JSON answer a step names, by a path of keys apart by
// dots, as a value for the steps after it.
func capture(answer []byte, fields, into map[string]string) error {
	if len(fields) == 0 {
		return nil
	}
	var doc any
	if err := json.Unmarshal(answer, &doc); err != nil {
		return errors.New("its answer isn't JSON, so nothing of it can be kept for the next step")
	}
	for _, name := range slices.Sorted(maps.Keys(fields)) {
		v, ok := at(doc, fields[name])
		if !ok {
			return fmt.Errorf("its answer has no %s", fields[name])
		}
		into[name] = v
	}
	return nil
}

func at(doc any, path string) (string, bool) {
	for key := range strings.SplitSeq(path, ".") {
		switch d := doc.(type) {
		case map[string]any:
			doc = d[key]
		case []any:
			i, err := strconv.Atoi(key)
			if err != nil || i < 0 || i >= len(d) {
				return "", false
			}
			doc = d[i]
		default:
			return "", false
		}
	}
	switch v := doc.(type) {
	case string:
		return v, v != ""
	case float64:
		return strconv.FormatFloat(v, 'f', -1, 64), true
	case bool:
		return strconv.FormatBool(v), true
	default:
		return "", false
	}
}

// minHidden is the shortest value Scrub hides: a shorter one, such as a port, hides
// nothing a password has, and would hide every digit of the answer.
const minHidden = 4

// Scrub hides, in what an app said, every value the steps sent and every value they
// captured, in each form an answer may echo it: as it is, URL-encoded, JSON-escaped
// and HTML-escaped, and the owner's user, or email, and password as a Basic
// credential. It hides first, then cuts to MaxSaid on a character's edge, so no part
// of a value outlives the cut.
func Scrub(said []byte, values, captured map[string]string, o *resourcev1.OwnerRun) string {
	s := string(said)
	var hide []string
	for _, m := range []map[string]string{values, captured} {
		for name, v := range m {
			// Where the app is reached is no secret, and "http" is in every URL.
			if name != Host && name != Scheme {
				hide = append(hide, forms(v)...)
			}
		}
	}
	if pw := values[o.GetPassword()]; pw != "" {
		for _, who := range []string{o.GetUser(), o.GetEmail()} {
			if who != "" {
				pair := who + ":" + pw
				hide = append(hide, base64.StdEncoding.EncodeToString([]byte(pair)), base64.RawStdEncoding.EncodeToString([]byte(pair)))
			}
		}
	}
	// The longest first, so a value inside another goes with it.
	slices.SortFunc(hide, func(a, b string) int { return len(b) - len(a) })
	for _, v := range hide {
		if len(v) >= minHidden {
			s = strings.ReplaceAll(s, v, "[hidden]")
		}
	}
	s = strings.TrimSpace(s)
	if len(s) > MaxSaid {
		cut := MaxSaid
		for cut > 0 && !utf8.RuneStart(s[cut]) {
			cut--
		}
		s = s[:cut]
	}
	return s
}

// forms are the ways an answer may write v.
func forms(v string) []string {
	if v == "" {
		return nil
	}
	return slices.Compact(slices.Sorted(slices.Values([]string{v, url.QueryEscape(v), url.PathEscape(v), jsonInside(v), html.EscapeString(v)})))
}

// captureOutput keeps what a command printed that its capture names, each by a
// regular expression whose first group is the value, for the steps after it.
func captureOutput(out []byte, patterns, into map[string]string) error {
	for _, name := range slices.Sorted(maps.Keys(patterns)) {
		re, err := regexp.Compile(patterns[name])
		if err != nil {
			return fmt.Errorf("can't read what it printed for %s: %w", name, err)
		}
		m := re.FindSubmatch(out)
		if len(m) < 2 || len(m[1]) == 0 {
			return fmt.Errorf("what it printed has no %s", name)
		}
		into[name] = string(m[1])
	}
	return nil
}

// Check says whether steps are ones the agent can run as they say: a command's
// words name no ${NAME}, as values reach it in its environment, and only a "sh -c"
// line names one, as "$NAME"; a request names its port and a method it takes.
func Check(steps []*resourcev1.OwnerStep) error {
	for i, step := range steps {
		if err := checkStep(step); err != nil {
			return fmt.Errorf("step %d %w", i+1, err)
		}
	}
	return nil
}

func checkStep(step *resourcev1.OwnerStep) error {
	if u := step.GetUnless(); u != nil {
		if err := CheckCheck(u); err != nil {
			return fmt.Errorf("is skipped by a check that %w", err)
		}
	}
	switch {
	case step.GetCommand() != nil:
		words := step.GetCommand().GetWords()
		if len(words) == 0 {
			return errors.New("runs no command")
		}
		for _, w := range words {
			if names.MatchString(w) {
				return fmt.Errorf("names %s in its words, where a password would show to anyone who lists the server's processes. Read it from the environment, as \"$NAME\" in a sh -c line", names.FindString(w))
			}
		}
		if step.GetCommand().GetOneOff() {
			return errors.New("runs as a one-off container, which no agent does yet")
		}
		for name, pattern := range step.GetCommand().GetCapture() {
			if re, err := regexp.Compile(pattern); err != nil || re.NumSubexp() < 1 {
				return fmt.Errorf("captures %s with %q, which isn't a regular expression with a group for the value", name, pattern)
			}
		}
	case step.GetRequest() != nil:
		return CheckRequest(step.GetRequest())
	default:
		return errors.New("is neither a command nor a request")
	}
	return nil
}

// CheckSecretWords refuses a command, a step's, or one of the checks that skip a
// step, that names one of secrets in its words, by "$NAME" in a sh -c line, or bare,
// as "printenv NAME" does, and one that names the owner's Basic credential or a
// captured value's: a shell writes the value into the command's own arguments, which
// anyone who lists the server's processes reads. A step that needs a password in a
// command has the app make one up, captures it, and sets the real one through the
// app's API (ADR 0105, the re-check's N3).
func CheckSecretWords(steps []*resourcev1.OwnerStep, secrets []string) error {
	for i, step := range steps {
		for _, c := range []*resourcev1.OwnerCommand{step.GetCommand(), step.GetUnless().GetCommand()} {
			if err := secretInWords(c, secrets); err != nil {
				return fmt.Errorf("step %d %w", i+1, err)
			}
		}
	}
	return nil
}

// CheckSecretCheck is CheckSecretWords for a check, as an owner's done.
func CheckSecretCheck(c *resourcev1.OwnerCheck, secrets []string) error {
	if err := secretInWords(c.GetCommand(), secrets); err != nil {
		return fmt.Errorf("a check %w", err)
	}
	return nil
}

// placeholder is {{NAME}}, which a script a step runs fills from its environment, as
// Ghost's does, so the value is never in its words.
var placeholder = regexp.MustCompile(`\{\{[A-Za-z_][A-Za-z0-9_]*\}\}`)

func secretInWords(c *resourcev1.OwnerCommand, secrets []string) error {
	for _, w := range c.GetWords() {
		w = placeholder.ReplaceAllString(w, "")
		for _, name := range append(slices.Clone(secrets), Basic) {
			if regexp.MustCompile(`\b` + regexp.QuoteMeta(name) + `\b`).MatchString(w) {
				return fmt.Errorf("names the secret %s in its command's words, where it would sit in the command's arguments for anyone listing the server's processes. Have the app make one up, capture it, and set %s through the app's API", name, name)
			}
		}
		if strings.Contains(w, BasicOf) {
			return fmt.Errorf("names a Basic credential, %s..., in its command's words, where it would sit in the command's arguments. Send it in a request's header", BasicOf)
		}
	}
	return nil
}

// CheckCheck says whether a check is one the agent can read: a request, or a
// command.
func CheckCheck(c *resourcev1.OwnerCheck) error {
	if c.GetCommand() != nil {
		if c.GetRequest() != nil {
			return errors.New("is a request and a command. Keep one")
		}
		return checkStep(&resourcev1.OwnerStep{Step: &resourcev1.OwnerStep_Command{Command: c.GetCommand()}})
	}
	return CheckRequest(c.GetRequest())
}

// CheckRequest says whether a request is one the agent can send.
func CheckRequest(r *resourcev1.OwnerRequest) error {
	if r.GetPort() == 0 || r.GetPort() > 65535 {
		return errors.New("names no port in the container to send to")
	}
	switch r.GetMethod() {
	case "", http.MethodGet, http.MethodPost, http.MethodPut, http.MethodPatch, http.MethodDelete:
	default:
		return fmt.Errorf("sends %s, where a step sends GET, POST, PUT, PATCH or DELETE", r.GetMethod())
	}
	if s := r.GetExpectStatus(); s >= 300 && s < 400 {
		return fmt.Errorf("expects %d, a redirect, which a step never follows", s)
	}
	return nil
}
