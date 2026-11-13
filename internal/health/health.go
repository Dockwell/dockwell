// SPDX-License-Identifier: AGPL-3.0-only

// Package health checks containers: what a check says, where the agent makes
// it, and watching every service's containers once a deploy is over. It runs
// on the agent, so it stays small: a check holds no goroutine and no connection
// between runs, and what it keeps of a container is a few short strings. See
// docs/adr/0027-health-checks-rollouts-and-rollback.md.
package health

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net"
	"net/http"
	"net/url"
	"slices"
	"strconv"
	"strings"
	"syscall"
	"time"
	"unicode"

	dockwellv1 "github.com/dockwell/dockwell/gen/dockwell/v1"
	"github.com/dockwell/dockwell/internal/docker"
)

// Defaults for a check the agent makes, where its spec leaves them out.
const (
	DefaultInterval = 10 * time.Second
	DefaultTimeout  = 5 * time.Second
	DefaultRetries  = 3
)

// Limits on a check the agent makes: at most this often, and for this long
// each, as many of them share the agent's few workers.
const (
	MinInterval = time.Second
	MaxTimeout  = time.Minute
)

// Label is the label a container carries for a check the agent makes, as
// Docker has no field for one. It holds a Check as JSON, so a check survives
// the agent, and docker compose up from an export keeps it.
const Label = docker.OwnPrefix + "check"

// Check is a check the agent makes of a container. It can name a port and a
// path, and no address: that is Docker's to say. So nothing that may write a
// spec can send the agent anywhere but to the container it runs.
type Check struct {
	HTTP        *HTTP         `json:"http,omitempty"`
	TCP         *TCP          `json:"tcp,omitempty"`
	Interval    time.Duration `json:"interval,omitempty"`
	Timeout     time.Duration `json:"timeout,omitempty"`
	StartPeriod time.Duration `json:"start_period,omitempty"`
	Retries     int           `json:"retries,omitempty"`
}

// HTTP is a GET to the container.
type HTTP struct {
	Port int    `json:"port"`
	Path string `json:"path,omitempty"`
	Host string `json:"host,omitempty"`
}

// TCP is a connection to the container.
type TCP struct {
	Port int `json:"port"`
}

// Parse reads a Check from its label.
func Parse(label string) (*Check, error) {
	var c Check
	if err := json.Unmarshal([]byte(label), &c); err != nil {
		return nil, fmt.Errorf("can't read the %s label: %w", Label, err)
	}
	if c.HTTP == nil && c.TCP == nil {
		return nil, fmt.Errorf("the %s label names no check", Label)
	}
	return &c, nil
}

// Encode writes a Check as its label holds it.
func (c *Check) Encode() string {
	b, _ := json.Marshal(c)
	return string(b)
}

// IntervalOr is how often a check runs.
func (c *Check) IntervalOr() time.Duration {
	if c.Interval > 0 {
		return c.Interval
	}
	return DefaultInterval
}

// TimeoutOr is how long one try of a check may take.
func (c *Check) TimeoutOr() time.Duration {
	if c.Timeout > 0 {
		return c.Timeout
	}
	return DefaultTimeout
}

// RetriesOr is how many failures in a row make a check fail.
func (c *Check) RetriesOr() int {
	if c.Retries > 0 {
		return c.Retries
	}
	return DefaultRetries
}

// Bound is how long a container may take to turn healthy or unhealthy, by its
// check: its start period, then as many failures as its retries, each taking
// up to its timeout with an interval after it.
func (c *Check) Bound() time.Duration {
	return c.StartPeriod + time.Duration(c.RetriesOr())*(c.IntervalOr()+c.TimeoutOr())
}

// Result is one check's result.
type Result struct {
	Passed bool
	// Output says what happened, in a line for people.
	Output string
	At     time.Time
	Took   time.Duration
}

// client makes the agent's checks. Each one is a connection of its own, closed
// straight after, so a check holds nothing between runs. A redirect isn't
// followed: a container saying where to go next could otherwise send the agent
// anywhere, as to a cloud's metadata address, and a 3xx passes as it stands.
// Nothing goes through a proxy the agent's environment names.
var client = &http.Client{
	Transport: &http.Transport{
		Proxy:                  nil,
		DisableKeepAlives:      true,
		MaxResponseHeaderBytes: 16 << 10,
		TLSHandshakeTimeout:    0,
	},
	CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse },
}

// Run makes the check of the container at address, and says how it went. It
// waits up to the check's timeout. A status or a refusal is a result: only ctx
// ending isn't one.
func Run(ctx context.Context, c *Check, address string) (Result, error) {
	start := time.Now()
	ctx, cancel := context.WithTimeout(ctx, c.TimeoutOr())
	defer cancel()
	var r Result
	switch {
	case c.HTTP != nil:
		r = runHTTP(ctx, c, address)
	case c.TCP != nil:
		r = runTCP(ctx, c, address)
	default:
		return Result{}, errors.New("the check has neither http nor tcp")
	}
	r.At, r.Took = time.Now(), time.Since(start)
	return r, nil
}

func runHTTP(ctx context.Context, c *Check, address string) Result {
	path := c.HTTP.Path
	if path == "" {
		path = "/"
	}
	what := "GET " + path + " on port " + strconv.Itoa(c.HTTP.Port)
	u := url.URL{Scheme: "http", Host: net.JoinHostPort(address, strconv.Itoa(c.HTTP.Port))}
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, u.String()+path, http.NoBody)
	if err != nil {
		return Result{Output: what + " can't be sent: " + err.Error()}
	}
	req.Header.Set("User-Agent", "dockwell-health/1")
	if c.HTTP.Host != "" {
		req.Host = c.HTTP.Host
	}
	res, err := client.Do(req)
	if err != nil {
		return Result{Output: what + " " + refusal(err, c.TimeoutOr(), c.HTTP.Port)}
	}
	// The body is the container's to fill, so it stays unread, and so does the
	// reason it sends with its status: only the number means anything here.
	_ = res.Body.Close()
	said := strconv.Itoa(res.StatusCode)
	if t := http.StatusText(res.StatusCode); t != "" {
		said += " " + t
	}
	return Result{Passed: res.StatusCode >= 200 && res.StatusCode < 400, Output: what + " answered " + said}
}

func runTCP(ctx context.Context, c *Check, address string) Result {
	what := "a connection to port " + strconv.Itoa(c.TCP.Port)
	var d net.Dialer
	conn, err := d.DialContext(ctx, "tcp", net.JoinHostPort(address, strconv.Itoa(c.TCP.Port)))
	if err != nil {
		return Result{Output: what + " " + refusal(err, c.TimeoutOr(), c.TCP.Port)}
	}
	_ = conn.Close()
	return Result{Passed: true, Output: what + " was taken"}
}

// refusal says why a connection failed, in words that say what to look at. It
// never passes on the error's own text, which would hold the address.
func refusal(err error, timeout time.Duration, port int) string {
	switch {
	case errors.Is(err, syscall.ECONNREFUSED):
		return "was refused: nothing listens on that port on the container's address. Check the port, and that the app listens on every address in its container, not only on localhost"
	case errors.Is(err, context.DeadlineExceeded), errors.Is(err, syscall.ETIMEDOUT), isTimeout(err):
		return fmt.Sprintf("got no answer within %s. Check that port %d is the app's, and that the app isn't stuck", timeout, port)
	case errors.Is(err, syscall.EHOSTUNREACH), errors.Is(err, syscall.ENETUNREACH):
		return "can't reach the container from the server, so the agent can't check it. Use a command check, which Docker runs inside the container"
	case errors.Is(err, syscall.ECONNRESET), errors.Is(err, syscall.EPIPE):
		return "was closed before it got an answer. Check that the app speaks HTTP on that port"
	}
	var perr *net.OpError
	if errors.As(err, &perr) {
		return "failed at " + perr.Op + ". Check that the app listens on that port"
	}
	return "got no usable answer. Check that the app speaks HTTP on that port"
}

func isTimeout(err error) bool {
	var t interface{ Timeout() bool }
	return errors.As(err, &t) && t.Timeout()
}

// Address is where the agent reaches a container on the server: its address on
// the ingress network if it has one, as that is where its traffic comes from,
// and otherwise on the first of its networks, by name, that has one. A
// container with none, as one that isn't running, gives "".
func Address(networks map[string]*docker.EndpointSettings) string {
	if ep := networks[docker.IngressNetwork]; ep != nil && ep.IPAddress != "" {
		return ep.IPAddress
	}
	best, bestName := "", ""
	for name, ep := range networks {
		// A database copy's network is internal, and the server can't reach it.
		if ep == nil || ep.IPAddress == "" || strings.HasPrefix(name, docker.CopyPrefix) {
			continue
		}
		if best == "" || name < bestName {
			best, bestName = ep.IPAddress, name
		}
	}
	return best
}

// MaxOutput is the most of a check's output that leaves the agent.
const MaxOutput = 240

// shortest is the shortest environment value that gets masked: shorter ones,
// such as "1" and "true", would mask ordinary words and hold no secret.
const shortest = 6

// Report is what a server last said of its services, and when, by the clock of the
// server it came from, so its age needs no two clocks to agree.
type Report struct {
	// Said is whether it has said anything: it hasn't until it has read Docker.
	Said      bool
	Instances []*dockwellv1.InstanceHealth
	// ScannedAt and ReportedAt are on the server's clock, and ReceivedAt on this
	// one's.
	ScannedAt, ReportedAt, ReceivedAt time.Time
	// DockerUnreadableSince is since when the agent hasn't been able to read
	// Docker, on the server's clock: the zero time while it can.
	DockerUnreadableSince time.Time
}

// SpecEnv is the part of a container's environment worth masking in what a check
// prints: what its spec set, the values of what its service uses among them, and
// not what its image sets. Anyone with the image has that, and masking it would take
// words such as "production" out of a check's output. A container that doesn't say,
// as one not made by Dockwell, has it all masked.
func SpecEnv(env []string, labels map[string]string) []string {
	var set []string
	if json.Unmarshal([]byte(labels[labelSet]), &set) != nil || labels[labelSet] == "" {
		return env
	}
	var out []string
	for _, kv := range env {
		if k, _, ok := strings.Cut(kv, "="); ok && slices.Contains(set, "env:"+k) {
			out = append(out, kv)
		}
	}
	return out
}

// labelSet is the label on which a container lists what its spec sets, as the
// container driver writes it.
const labelSet = docker.OwnPrefix + "set"

// Clean makes what a check printed fit to pass on, into a change's error and a
// timeline that anyone with the read scope sees, where a log needs a grant of
// its own: one line, of printable characters, at most MaxOutput bytes, with the
// value of each of the container's environment variables masked, and so is a
// password in a URL. What a check prints can hold anything, so this isn't a
// promise it holds no secret. It is why a check's output is cut so short, and
// why the fix always points at docker inspect and docker logs.
func Clean(output string, env []string) string {
	for _, kv := range env {
		if _, v, ok := strings.Cut(kv, "="); ok && len(v) >= shortest {
			output = strings.ReplaceAll(output, v, "•••")
		}
	}
	output = maskURLPasswords(output)
	var b strings.Builder
	space := false
	for _, r := range output {
		switch {
		case unicode.IsSpace(r):
			space = b.Len() > 0
			continue
		case !unicode.IsPrint(r):
			continue // what could move a terminal's cursor, or turn text around, as IsPrint has neither
		}
		if space {
			b.WriteByte(' ')
			space = false
		}
		if b.Len()+len(string(r)) > MaxOutput {
			b.WriteString("…")
			break
		}
		b.WriteRune(r)
	}
	return b.String()
}

// maskURLPasswords masks what comes between a colon and an @ in anything with a
// scheme, as in postgres://app:secret@db/app.
func maskURLPasswords(s string) string {
	var out strings.Builder
	for {
		i := strings.Index(s, "://")
		if i < 0 {
			out.WriteString(s)
			return out.String()
		}
		out.WriteString(s[:i+3])
		s = s[i+3:]
		end := strings.IndexAny(s, "/ \t\r\n\"'")
		if end < 0 {
			end = len(s)
		}
		auth := s[:end]
		if at := strings.LastIndexByte(auth, '@'); at >= 0 {
			if colon := strings.IndexByte(auth[:at], ':'); colon >= 0 {
				out.WriteString(auth[:colon+1] + "•••" + auth[at:])
				s = s[end:]
				continue
			}
		}
		out.WriteString(auth)
		s = s[end:]
	}
}

// FromDocker reads the last run of a check Docker makes, cleaned, or false when
// none has finished.
func FromDocker(h *docker.Health, env []string) (Result, bool) {
	if h == nil || len(h.Log) == 0 {
		return Result{}, false
	}
	last := h.Log[len(h.Log)-1]
	out := Clean(last.Output, env)
	switch {
	case last.ExitCode != 0 && out == "":
		out = commandWords + "exited with " + strconv.Itoa(last.ExitCode) + " and printed nothing"
	case last.ExitCode != 0:
		// In quotes, as these are the container's words, and not Dockwell's.
		out = commandWords + "exited with " + strconv.Itoa(last.ExitCode) + ": " + strconv.Quote(out)
	case out == "":
		out = commandWords + "printed nothing"
	}
	return Result{Passed: last.ExitCode == 0, Output: out, At: last.End}, true
}

// commandWords starts what a command check's result says of the command.
const commandWords = "the command "

// WithFix ends what a check said with what to do about it, for what a person reads.
// A failed check's words are the one fact they need: short, cleaned and masked, and
// not a container's logs. What the agent's own check says may say what to look at
// already, and what a command printed is quoted, so its full stops aren't Dockwell's.
func WithFix(said string) string {
	switch {
	case strings.HasPrefix(said, commandWords):
		return said + ". Fix what it reports, or the check, then try again."
	case strings.Contains(said, ". "):
		return said + ", then try again."
	}
	return said + ". Fix what it answers, or the check, then try again."
}
