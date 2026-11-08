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
	"os/exec"
	"strconv"
	"strings"
	"syscall"
	"time"

	resourcev1 "github.com/dockwell/dockwell/gen/dockwell/resource/v1alpha1"
	dockwellv1 "github.com/dockwell/dockwell/gen/dockwell/v1"
)

// stopGrace is how long a command has between SIGTERM and SIGKILL.
const stopGrace = 10 * time.Second

// attempt is how one try of a run went.
type attempt struct {
	exitCode   int32 // a command's, -1 when it was stopped
	httpStatus int32
	bytes      int64
	timedOut   bool
	// err is why it failed, for people, or why it couldn't start, with started
	// false. Nil when it succeeded.
	err     error
	started bool
}

// shellEnv is all the environment a shell task gets (ADR 0053).
var shellEnv = []string{"PATH=/usr/local/sbin:/usr/local/bin:/usr/sbin:/usr/bin:/sbin:/bin", "HOME=/root", "LANG=C.UTF-8"}

// runShell runs a shell command on the server, as the agent's user, which is root,
// in a process group of its own so a timeout stops all of it.
func runShell(ctx context.Context, s *resourcev1.TaskShell, timeout time.Duration, out *output) attempt {
	dir := s.GetWorkingDir()
	if dir == "" {
		dir = "/"
	}
	// The timeout below stops the whole group; a context's cancel would stop the
	// shell alone.
	cmd := exec.CommandContext(context.WithoutCancel(ctx), "/bin/sh", "-c", s.GetCommand()) //nolint:gosec // what a person approved with a passkey, to run as root (ADR 0053)
	cmd.Dir, cmd.Env, cmd.Stdout, cmd.Stderr = dir, shellEnv, out, out
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
	if err := cmd.Start(); err != nil {
		return attempt{exitCode: -1, err: fmt.Errorf("the server can't start /bin/sh in %s: %w", dir, err)}
	}
	done := make(chan error, 1)
	go func() { done <- cmd.Wait() }()
	timer := time.NewTimer(timeout)
	defer timer.Stop()
	var why error
	timedOut := false
	select {
	case err := <-done:
		return shellEnded(err)
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
		if err := <-done; err != nil {
			why = fmt.Errorf("%w, and didn't stop until it was killed", why)
		}
	}
	out.Line("Dockwell: " + why.Error() + ".")
	return attempt{exitCode: -1, timedOut: timedOut, err: why, started: true}
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

// requestClient makes requests that refuse link-local addresses, where clouds keep
// their metadata services, however a name resolves or a redirect leads.
var requestClient = &http.Client{
	Transport: &http.Transport{
		Proxy: nil,
		DialContext: (&net.Dialer{
			Timeout: 30 * time.Second,
			Control: func(_, address string, _ syscall.RawConn) error {
				return refuseLinkLocal(address)
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

// errLinkLocal is a request to a link-local address.
var errLinkLocal = errors.New("it leads to a link-local address, where a cloud keeps its metadata service, which a task doesn't ask")

func refuseLinkLocal(address string) error {
	host, _, err := net.SplitHostPort(address)
	if err != nil {
		return err
	}
	ip, err := netip.ParseAddr(host)
	if err != nil {
		return nil //nolint:nilerr // a dial is to an address; a name here has nothing to check
	}
	if ip = ip.Unmap(); ip.IsLinkLocalUnicast() || ip.IsLinkLocalMulticast() {
		return errLinkLocal
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
		out.Line(fmt.Sprintf("%s %s: %s.", r.GetMethod(), r.GetUrl(), why))
		return attempt{timedOut: timedOut, started: true, err: why}
	}
	defer resp.Body.Close()
	n, _ := io.Copy(io.Discard, io.LimitReader(resp.Body, maxCounted))
	took := time.Since(start).Round(time.Millisecond)
	out.Line(fmt.Sprintf("%s %s answered %s in %s, %d bytes.", r.GetMethod(), r.GetUrl(), resp.Status, took, n))
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
	switch {
	case timedOut:
		return fmt.Errorf("it didn't answer within its timeout, %s", timeout)
	case errors.Is(err, errLinkLocal):
		return errLinkLocal
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
