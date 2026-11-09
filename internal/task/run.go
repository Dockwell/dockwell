// SPDX-License-Identifier: AGPL-3.0-only

package task

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/netip"
	"os"
	"os/exec"
	"slices"
	"strconv"
	"strings"
	"sync"
	"syscall"
	"time"

	resourcev1 "github.com/dockwell/dockwell/gen/dockwell/resource/v1alpha1"
	dockwellv1 "github.com/dockwell/dockwell/gen/dockwell/v1"
)

// stopGrace is how long a command has between SIGTERM and SIGKILL.
const stopGrace = 10 * time.Second

// outputGraceAtMost is how long what a shell command started has to finish
// printing, once the shell has ended, for that to be in the run's output.
const outputGraceAtMost = 5 * time.Second

// outputGrace is outputGraceAtMost, a variable for tests.
var outputGrace = outputGraceAtMost

// attempt is how one try of a run went.
type attempt struct {
	exitCode   int32 // a command's, -1 when it was stopped
	httpStatus int32
	bytes      int64
	timedOut   bool
	// held says something the command started held its output past its end.
	held bool
	// err is why it failed, for people, or why it couldn't start, with started
	// false. Nil when it succeeded.
	err     error
	started bool
}

// shellEnv is all the environment a shell task gets (ADR 0053).
var shellEnv = []string{"PATH=/usr/local/sbin:/usr/local/bin:/usr/sbin:/usr/bin:/sbin:/bin", "HOME=/root", "LANG=C.UTF-8"}

// runShell runs a shell command on the server, as the agent's user, which is root,
// in a process group of its own so a timeout stops all of it. The run is the
// shell's: something it starts that leaves the group, as setsid does, runs on
// after it, as under cron. What that prints once the shell has ended is kept for
// outputGrace at most, and read and let go of after, for as long as it prints, so
// it never writes to a pipe nobody reads, which would end it.
func runShell(ctx context.Context, s *resourcev1.TaskShell, timeout time.Duration, out *output) attempt {
	dir := s.GetWorkingDir()
	if dir == "" {
		dir = "/"
	}
	// The output goes through a pipe of the agent's own, so the shell's end is
	// known when it comes, whoever still holds the pipe.
	pr, pw, err := os.Pipe()
	if err != nil {
		return attempt{exitCode: -1, err: fmt.Errorf("the agent can't make a pipe for the command's output: %w", err)}
	}
	// The timeout below stops the whole group; a context's cancel would stop the
	// shell alone.
	cmd := exec.CommandContext(context.WithoutCancel(ctx), "/bin/sh", "-c", s.GetCommand()) //nolint:gosec // what a person approved with a passkey, to run as root (ADR 0053)
	cmd.Dir, cmd.Env, cmd.Stdout, cmd.Stderr = dir, shellEnv, pw, pw
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
	err = cmd.Start()
	_ = pw.Close() // the shell has its own; this one would keep the pipe open
	if err != nil {
		_ = pr.Close()
		return attempt{exitCode: -1, err: fmt.Errorf("the server can't start /bin/sh in %s: %w", dir, err)}
	}
	// Read to the end, whoever holds the pipe, which is past the run for what runs
	// on after it: kept until then, and let go of after.
	sink := &afterRun{out: out}
	read := make(chan struct{})
	go func() {
		_, _ = io.Copy(sink, pr)
		_ = pr.Close()
		close(read)
	}()
	ended := func(a attempt) attempt {
		select {
		case <-read:
		case <-time.After(outputGrace):
			sink.letGo()
			a.held = true
			out.Line("Dockwell: something the command started still holds its output, and runs on after it.")
		}
		return a
	}
	done := make(chan error, 1)
	go func() { done <- cmd.Wait() }()
	timer := time.NewTimer(timeout)
	defer timer.Stop()
	var why error
	timedOut := false
	select {
	case err := <-done:
		return ended(shellEnded(err))
	case <-timer.C:
		timedOut, why = true, fmt.Errorf("it took longer than its timeout, %s, and was stopped", timeout)
	case <-ctx.Done():
		why = errors.New("the agent stopped, and stopped it")
	}
	// Stop the whole group: what the shell started too.
	_ = syscall.Kill(-cmd.Process.Pid, syscall.SIGTERM)
	select {
	case err := <-done:
		_ = err // it was stopped, which is what's said
	case <-time.After(stopGrace):
		_ = syscall.Kill(-cmd.Process.Pid, syscall.SIGKILL)
		err := <-done
		_ = err // as above
		why = fmt.Errorf("%w, and didn't stop until it was killed", why)
	}
	a := ended(attempt{exitCode: -1, timedOut: timedOut, err: why, started: true})
	out.Line("Dockwell: " + why.Error() + ".")
	return a
}

// afterRun is where a shell command's output goes: into its run's output until the
// run has ended, then nowhere, as what runs on after it may print for ever.
type afterRun struct {
	mu   sync.Mutex
	out  io.Writer
	gone bool
}

func (w *afterRun) Write(p []byte) (int, error) {
	w.mu.Lock()
	defer w.mu.Unlock()
	if w.gone {
		return len(p), nil
	}
	return w.out.Write(p)
}

// letGo ends what reaches the run's output.
func (w *afterRun) letGo() {
	w.mu.Lock()
	w.gone = true
	w.mu.Unlock()
}

// shellEnded is how a command that ended went.
func shellEnded(err error) attempt {
	var exit *exec.ExitError
	switch {
	case err == nil:
		return attempt{started: true}
	case errors.As(err, &exit):
		code := int32(exit.ExitCode()) //nolint:gosec // an exit status fits
		if code == -1 {
			return attempt{exitCode: -1, started: true, err: fmt.Errorf("it was ended by a signal: %s", exit.String())}
		}
		return attempt{exitCode: code, started: true, err: fmt.Errorf("it exited with %d", code)}
	}
	return attempt{exitCode: -1, started: true, err: err}
}

// maxRedirects is how many redirects a request follows.
const maxRedirects = 5

// requestClient makes requests that refuse where clouds keep their metadata
// services, and addresses that aren't one server's, however a name resolves or a
// redirect leads. This server and its network are what a task asks, so loopback
// and private addresses are its to ask (ADR 0053).
var requestClient = &http.Client{
	Transport: &http.Transport{
		Proxy: nil,
		DialContext: (&net.Dialer{
			Timeout: 30 * time.Second,
			Control: func(_, address string, _ syscall.RawConn) error {
				return refuseAddress(address)
			},
		}).DialContext,
		TLSHandshakeTimeout:   30 * time.Second,
		ResponseHeaderTimeout: 0,
		MaxIdleConns:          4,
		IdleConnTimeout:       30 * time.Second,
	},
	CheckRedirect: func(_ *http.Request, via []*http.Request) error {
		if len(via) >= maxRedirects {
			return fmt.Errorf("it was sent on more than %d times", maxRedirects)
		}
		return nil
	},
}

// errRefused is a request to an address a task doesn't ask.
type errRefused struct{ why string }

func (e errRefused) Error() string { return "it leads to " + e.why }

var (
	// metadataAddresses are where clouds keep their metadata services outside
	// link-local addresses: Alibaba Cloud's, and AWS's over IPv6.
	metadataAddresses = []netip.Addr{netip.MustParseAddr("100.100.100.200"), netip.MustParseAddr("fd00:ec2::254")}
	// nat64 is where IPv6 reaches IPv4 through a translator, which reaches a
	// metadata address as well as the address itself.
	nat64 = netip.MustParsePrefix("64:ff9b::/96")
)

// refuseAddress refuses a dial to where a cloud keeps its metadata, at the
// address a name resolved to: link-local addresses and the others clouds use. It
// refuses an address that isn't one server's too: unspecified, or multicast.
func refuseAddress(address string) error {
	host, _, err := net.SplitHostPort(address)
	if err != nil {
		return err
	}
	ip, err := netip.ParseAddr(host)
	if err != nil {
		return nil //nolint:nilerr // a dial is to an address; a name here has nothing to check
	}
	ip = ip.Unmap()
	if nat64.Contains(ip) {
		b := ip.As16()
		ip = netip.AddrFrom4([4]byte(b[12:]))
	}
	switch {
	case ip.IsLinkLocalUnicast() || ip.IsLinkLocalMulticast() || slices.Contains(metadataAddresses, ip.WithZone("")):
		return errRefused{why: "a cloud's metadata service, which a task doesn't ask"}
	case ip.IsUnspecified():
		return errRefused{why: ip.String() + ", which is no one server. Ask 127.0.0.1 for this one"}
	case ip.IsMulticast():
		return errRefused{why: "a multicast address, which is no one server"}
	}
	return nil
}

// maxCounted is how much of an answer a request reads to count its size.
const maxCounted = 64 << 20

// runRequest asks a URL from the server, and keeps its status, how long it took
// and how much came back, never what.
func runRequest(ctx context.Context, r *resourcev1.TaskRequest, timeout time.Duration, out *output) attempt {
	ctx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()
	req, err := http.NewRequestWithContext(ctx, r.GetMethod(), r.GetUrl(), http.NoBody)
	if err != nil {
		return attempt{err: fmt.Errorf("its URL doesn't read: %w", err)}
	}
	req.Header.Set("User-Agent", "Dockwell scheduled task")
	start := time.Now()
	resp, err := requestClient.Do(req)
	if err != nil {
		timedOut := errors.Is(ctx.Err(), context.DeadlineExceeded)
		why := requestError(err, timedOut, timeout)
		out.Line(fmt.Sprintf("Dockwell: %s %s: %s.", r.GetMethod(), r.GetUrl(), why))
		return attempt{timedOut: timedOut, started: true, err: why}
	}
	defer resp.Body.Close()
	n, _ := io.Copy(io.Discard, io.LimitReader(resp.Body, maxCounted))
	took := time.Since(start).Round(time.Millisecond)
	out.Line(fmt.Sprintf("Dockwell: %s %s answered %s in %s, %d bytes.", r.GetMethod(), r.GetUrl(), resp.Status, took, n))
	a := attempt{httpStatus: int32(resp.StatusCode), bytes: n, started: true} //nolint:gosec // an HTTP status fits
	if want := r.GetExpectStatus(); want != 0 && resp.StatusCode != int(want) || want == 0 && (resp.StatusCode < 200 || resp.StatusCode > 299) {
		expected := "a 2xx status"
		if want != 0 {
			expected = strconv.FormatUint(uint64(want), 10)
		}
		a.err = fmt.Errorf("it answered %d, and the task expects %s", resp.StatusCode, expected)
	}
	return a
}

// requestError says why a request got no answer, for people.
func requestError(err error, timedOut bool, timeout time.Duration) error {
	if timedOut {
		return fmt.Errorf("it didn't answer within its timeout, %s", timeout)
	}
	if refused, ok := errors.AsType[errRefused](err); ok {
		return refused
	}
	msg := err.Error()
	if i := strings.LastIndex(msg, ": "); i >= 0 {
		msg = msg[i+2:]
	}
	return fmt.Errorf("it didn't answer: %s", msg)
}

// stateOf is how a run ended, from its last attempt.
func stateOf(a attempt) dockwellv1.TaskRunState {
	switch {
	case !a.started:
		return dockwellv1.TaskRunState_TASK_RUN_STATE_NOT_STARTED
	case a.timedOut:
		return dockwellv1.TaskRunState_TASK_RUN_STATE_TIMED_OUT
	case a.err != nil:
		return dockwellv1.TaskRunState_TASK_RUN_STATE_FAILED
	}
	return dockwellv1.TaskRunState_TASK_RUN_STATE_SUCCEEDED
}
