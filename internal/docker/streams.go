// SPDX-License-Identifier: AGPL-3.0-only

package docker

import (
	"bufio"
	"bytes"
	"context"
	"encoding/binary"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/url"
	"strings"
	"time"
)

// Streams are what a process in a container reads and writes. Nil Stdin gives it
// none, and nil Stdout or Stderr throws that away.
type Streams struct {
	Stdin          io.Reader
	Stdout, Stderr io.Writer
}

// hijacked is a connection the engine turned into a process's raw streams.
type hijacked struct {
	conn *net.UnixConn
	r    *bufio.Reader
	stop func() bool
}

// close ends the streams, which stops what is reading or writing them.
func (h *hijacked) close() {
	h.stop()
	_ = h.conn.Close()
}

// hijack sends a request whose answer is a process's streams, on a connection of
// its own. Go's HTTP client can't half-close a connection, and a process only
// hears the end of its stdin from that. Cancelling ctx ends the streams.
func (c *Client) hijack(ctx context.Context, path string, query url.Values, body any) (*hijacked, error) {
	path, err := c.versioned(ctx, path)
	if err != nil {
		return nil, err
	}
	var d net.Dialer
	conn, err := d.DialContext(ctx, "unix", c.socket)
	if err != nil {
		return nil, c.unavailable(err)
	}
	h := &hijacked{conn: conn.(*net.UnixConn), r: bufio.NewReader(conn)}
	h.stop = context.AfterFunc(ctx, func() { _ = conn.Close() })
	var r io.Reader = http.NoBody
	if body != nil {
		b, err := json.Marshal(body)
		if err != nil {
			h.close()
			return nil, err
		}
		r = bytes.NewReader(b)
	}
	u := "http://docker" + path
	if len(query) > 0 {
		u += "?" + query.Encode()
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, u, r)
	if err != nil {
		h.close()
		return nil, err
	}
	req.Header.Set("Connection", "Upgrade")
	req.Header.Set("Upgrade", "tcp")
	if body != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	if err := req.Write(conn); err != nil {
		h.close()
		return nil, fmt.Errorf("can't reach Docker at %s: %w", c.socket, err)
	}
	res, err := http.ReadResponse(h.r, req)
	if err != nil {
		h.close()
		return nil, fmt.Errorf("can't read Docker's answer to %s: %w", path, err)
	}
	if res.StatusCode >= http.StatusBadRequest {
		defer h.close()
		defer res.Body.Close()
		b, _ := io.ReadAll(io.LimitReader(res.Body, 64<<10))
		var e struct{ Message string }
		if json.Unmarshal(b, &e) != nil || e.Message == "" {
			e.Message = strings.TrimSpace(string(b))
		}
		return nil, &Error{Status: res.StatusCode, Message: scrubMount(e.Message)}
	}
	return h, nil
}

// frameStream names the third stream of a multiplexed connection: the engine's
// own error about the process.
const frameSystem = 3

// pump feeds the process its stdin and takes its output until it ends, or until
// something here can't take any more, which ends the streams.
func (h *hijacked) pump(s Streams) error {
	fed := make(chan error, 1)
	if s.Stdin != nil {
		go func() {
			_, err := io.Copy(h.conn, s.Stdin)
			if cerr := h.conn.CloseWrite(); err == nil {
				err = cerr
			}
			fed <- err
		}()
	}
	err := Demux(h.r, s.Stdout, s.Stderr)
	h.close() // what still feeds it has nowhere to go
	if s.Stdin != nil {
		// A process can leave before it reads all it was given, which is its
		// exit code's to say, not a failure to feed it. A reader here that
		// failed is what matters.
		if ferr := <-fed; err == nil && ferr != nil && !isClosed(ferr) {
			err = ferr
		}
	}
	return err
}

// isClosed says an error came of writing to a process that had gone.
func isClosed(err error) bool {
	return errors.Is(err, net.ErrClosed) || errors.Is(err, io.ErrClosedPipe) || strings.Contains(err.Error(), "broken pipe") ||
		strings.Contains(err.Error(), "connection reset")
}

// Demux splits what the engine sends of a process with no terminal into its
// stdout and stderr, as its frames say.
func Demux(r io.Reader, stdout, stderr io.Writer) error {
	if stdout == nil {
		stdout = io.Discard
	}
	if stderr == nil {
		stderr = io.Discard
	}
	var head [8]byte
	var system []byte
	for {
		if _, err := io.ReadFull(r, head[:]); err != nil {
			if errors.Is(err, io.EOF) {
				break
			}
			if errors.Is(err, net.ErrClosed) {
				return err
			}
			return fmt.Errorf("docker's stream ended partway through a frame: %w", err)
		}
		size := int64(binary.BigEndian.Uint32(head[4:]))
		var to io.Writer
		var buf bytes.Buffer
		switch head[0] {
		case 0, 1:
			to = stdout
		case 2:
			to = stderr
		case frameSystem:
			to = &buf
		default:
			return fmt.Errorf("docker's stream has a frame of an unknown kind (%d)", head[0])
		}
		if n, err := io.CopyN(to, r, size); err != nil {
			if n < size && !errors.Is(err, io.EOF) {
				return err
			}
			return fmt.Errorf("docker's stream ended partway through a frame: %w", err)
		}
		system = append(system, buf.Bytes()...)
	}
	if len(system) > 0 {
		return errors.New(strings.TrimSpace(string(system)))
	}
	return nil
}

// RunAttached attaches to a container that hasn't started, starts it, and pumps
// its streams until it ends, then says how it exited. What it prints goes only to
// s if its logging is off: otherwise the engine keeps a copy too. Make it with
// OpenStdin and StdinOnce to give it Stdin.
func (c *Client) RunAttached(ctx context.Context, id string, s Streams) (int, error) {
	q := url.Values{"stream": {"1"}, "stdout": {"1"}, "stderr": {"1"}}
	if s.Stdin != nil {
		q.Set("stdin", "1")
	}
	h, err := c.hijack(ctx, "/containers/"+esc(id)+"/attach", q, nil)
	if err != nil {
		return 0, err
	}
	if err := c.StartContainer(ctx, id); err != nil {
		h.close()
		return 0, err
	}
	if err := h.pump(s); err != nil {
		return 0, err
	}
	return c.WaitContainer(ctx, id)
}

// WaitContainer waits for a container to stop, and says how it exited.
func (c *Client) WaitContainer(ctx context.Context, id string) (int, error) {
	var out struct {
		StatusCode int `json:"StatusCode"`
		Error      *struct {
			Message string `json:"Message"`
		} `json:"Error"`
	}
	if err := c.do(ctx, http.MethodPost, "/containers/"+esc(id)+"/wait", nil, nil, &out); err != nil {
		return 0, err
	}
	if out.Error != nil && out.Error.Message != "" {
		return out.StatusCode, errors.New(out.Error.Message)
	}
	return out.StatusCode, nil
}

// Exec runs a command in a running container, pumps its streams until it ends,
// and says how it exited. It runs as the container's own user, with its
// environment.
func (c *Client) Exec(ctx context.Context, id string, cmd []string, s Streams) (int, error) {
	var made struct {
		ID string `json:"Id"`
	}
	req := struct {
		AttachStdin, AttachStdout, AttachStderr bool
		Cmd                                     []string
	}{s.Stdin != nil, true, true, cmd}
	if err := c.do(ctx, http.MethodPost, "/containers/"+esc(id)+"/exec", nil, req, &made); err != nil {
		return 0, err
	}
	h, err := c.hijack(ctx, "/exec/"+esc(made.ID)+"/start", nil, struct{ Detach, Tty bool }{})
	if err != nil {
		return 0, err
	}
	if err := h.pump(s); err != nil {
		return 0, err
	}
	// The stream ends a moment before the engine has the exit code.
	for range 100 {
		var out struct {
			Running  bool `json:"Running"`
			ExitCode int  `json:"ExitCode"`
		}
		if err := c.do(ctx, http.MethodGet, "/exec/"+esc(made.ID)+"/json", nil, nil, &out); err != nil {
			return 0, err
		}
		if !out.Running {
			return out.ExitCode, nil
		}
		select {
		case <-ctx.Done():
			return 0, ctx.Err()
		case <-time.After(50 * time.Millisecond):
		}
	}
	return 0, errors.New("docker still says the command is running, though its streams ended")
}

// ImportImage makes an image named ref from a tar of a filesystem, as docker
// import does. It pulls nothing.
func (c *Client) ImportImage(ctx context.Context, ref string, fs io.Reader) error {
	repo, tag, _ := strings.Cut(ref, ":")
	path, err := c.versioned(ctx, "/images/create")
	if err != nil {
		return err
	}
	u := "http://docker" + path + "?" + url.Values{"fromSrc": {"-"}, "repo": {repo}, "tag": {tag}}.Encode()
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, u, fs)
	if err != nil {
		return err
	}
	req.Header.Set("Content-Type", "application/x-tar")
	res, err := c.http.Do(req)
	if err != nil {
		if cerr := ctx.Err(); cerr != nil {
			return cerr
		}
		return fmt.Errorf("can't reach Docker at %s: %w", c.socket, err)
	}
	defer res.Body.Close()
	defer discard(res.Body)
	if res.StatusCode >= http.StatusBadRequest {
		b, _ := io.ReadAll(io.LimitReader(res.Body, 64<<10))
		return &Error{Status: res.StatusCode, Message: strings.TrimSpace(string(b))}
	}
	dec := json.NewDecoder(res.Body)
	for {
		var ev pullEvent
		if err := dec.Decode(&ev); err != nil {
			if errors.Is(err, io.EOF) {
				return nil
			}
			return fmt.Errorf("can't read the progress of importing %s: %w", ref, err)
		}
		if ev.Error != "" {
			return &Error{Status: http.StatusInternalServerError, Message: ev.Error}
		}
	}
}

// RemoveImage removes an image by reference, with its tag. One that's gone
// already is no error.
func (c *Client) RemoveImage(ctx context.Context, ref string) error {
	err := c.do(ctx, http.MethodDelete, "/images/"+escRef(ref), url.Values{"force": {"true"}}, nil, nil)
	if IsNotFound(err) {
		return nil
	}
	return err
}

// ImageTags lists the tags of images whose references match pattern, such as
// dockwell-volume-tool.
func (c *Client) ImageTags(ctx context.Context, pattern string) ([]string, error) {
	q, err := listQuery(map[string][]string{"reference": {pattern}})
	if err != nil {
		return nil, err
	}
	q.Del("all")
	var images []struct {
		RepoTags []string `json:"RepoTags"`
	}
	if err := c.do(ctx, http.MethodGet, "/images/json", q, nil, &images); err != nil {
		return nil, err
	}
	var out []string
	for i := range images {
		out = append(out, images[i].RepoTags...)
	}
	return out, nil
}
