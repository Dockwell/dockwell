// SPDX-License-Identifier: AGPL-3.0-only

package docker

import (
	"context"
	"errors"
	"net/http"
	"net/url"
	"strconv"
)

// ExecTerminal is a process in a container with a terminal of its own, as
// docker exec -it runs one: what it prints comes up the connection as it is, with
// no frames, and what's written goes to it as typed.
type ExecTerminal struct {
	c  *Client
	id string
	h  *hijacked
}

// ID is the exec instance's ID.
func (e *ExecTerminal) ID() string { return e.id }

// Read reads what the process prints.
func (e *ExecTerminal) Read(p []byte) (int, error) { return e.h.r.Read(p) }

// Write types into it.
func (e *ExecTerminal) Write(p []byte) (int, error) { return e.h.conn.Write(p) }

// Close ends the connection. The process may go on: see State for its PID.
func (e *ExecTerminal) Close() error {
	e.h.close()
	return nil
}

// Resize sets the terminal's size.
func (e *ExecTerminal) Resize(ctx context.Context, cols, rows uint16) error {
	q := url.Values{"h": {strconv.Itoa(int(rows))}, "w": {strconv.Itoa(int(cols))}}
	return e.c.do(ctx, http.MethodPost, "/exec/"+esc(e.id)+"/resize", q, nil, nil)
}

// ExecState is how an exec instance stands.
type ExecState struct {
	Running     bool   `json:"Running"`
	ExitCode    int    `json:"ExitCode"`
	ContainerID string `json:"ContainerID"`
	// Pid is the process's ID on the server, outside the container.
	Pid int `json:"Pid"`
}

// State says how the process stands.
func (e *ExecTerminal) State(ctx context.Context) (ExecState, error) {
	var s ExecState
	err := e.c.do(ctx, http.MethodGet, "/exec/"+esc(e.id)+"/json", nil, nil, &s)
	return s, err
}

// ErrExecContainerChanged means Docker resolved a different container when it
// created the exec. The command did not start.
var ErrExecContainerChanged = errors.New("the container changed before the terminal opened. Open the current container from Docker")

// ExecWithTerminal runs cmd in a running container on a terminal of cols by rows,
// as the container's own user, with its environment and env besides, and returns
// its streams. The id must be the full container ID. Cancelling ctx ends them.
func (c *Client) ExecWithTerminal(ctx context.Context, id string, cmd, env []string, cols, rows uint16) (*ExecTerminal, error) {
	var made struct {
		ID string `json:"Id"`
	}
	req := struct {
		AttachStdin, AttachStdout, AttachStderr, Tty bool
		Cmd, Env                                     []string
		ConsoleSize                                  [2]uint16 `json:",omitempty"`
	}{true, true, true, true, cmd, env, [2]uint16{rows, cols}}
	if err := c.do(ctx, http.MethodPost, "/containers/"+esc(id)+"/exec", nil, req, &made); err != nil {
		return nil, err
	}
	terminal := &ExecTerminal{c: c, id: made.ID}
	// Docker can resolve an ID-shaped name after the original container goes
	// away. Inspect the created exec before it can run in the wrong container.
	state, err := terminal.State(ctx)
	if err != nil {
		return nil, err
	}
	if state.ContainerID != id {
		// Docker has no exec delete endpoint. Its daemon keeps the unstarted
		// descriptor until the replacement container stops or is removed.
		return nil, ErrExecContainerChanged
	}
	h, err := c.hijack(ctx, "/exec/"+esc(made.ID)+"/start", nil, struct {
		Detach, Tty bool
		ConsoleSize [2]uint16 `json:",omitempty"`
	}{false, true, [2]uint16{rows, cols}})
	if err != nil {
		return nil, err
	}
	terminal.h = h
	return terminal, nil
}

// HasPath reports whether a container has something at path, from its own
// filesystem, with nothing run in it: what docker cp would see.
func (c *Client) HasPath(ctx context.Context, id, path string) (bool, error) {
	res, err := c.versionedSend(ctx, http.MethodHead, "/containers/"+esc(id)+"/archive", url.Values{"path": {path}})
	if IsNotFound(err) {
		return false, nil
	}
	if err != nil {
		return false, err
	}
	defer res.Body.Close()
	return res.StatusCode == http.StatusOK, nil
}

// versionedSend is send to a versioned endpoint, with no body.
func (c *Client) versionedSend(ctx context.Context, method, path string, query url.Values) (*http.Response, error) {
	path, err := c.versioned(ctx, path)
	if err != nil {
		return nil, err
	}
	res, err := c.send(ctx, method, path, query, nil)
	if err != nil {
		return nil, err
	}
	if res == nil {
		return nil, errors.New("docker gave no answer")
	}
	return res, nil
}
