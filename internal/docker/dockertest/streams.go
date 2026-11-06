// SPDX-License-Identifier: AGPL-3.0-only

package dockertest

import (
	"bufio"
	"encoding/binary"
	"encoding/json"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"sync"
	"time"

	"github.com/dockwell/dockwell/internal/docker"
	"github.com/dockwell/dockwell/internal/volumetool"
)

// Exec is a command run in a container of an image with an Exec of its own.
type Exec struct {
	Cmd []string
	// Env is the container's environment.
	Env []string
	// Volumes says where each mount of the container is on this machine, by its
	// path in the container.
	Volumes        map[string]string
	Stdin          io.Reader
	Stdout, Stderr io.Writer
}

// exec is a command the engine has been asked to run.
type execRun struct {
	container *container
	cmd       []string
	running   bool
	exitCode  int
}

// streamPath matches what answers with a process's streams, or waits on one,
// and so holds no lock: a helper runs for as long as it runs.
var streamPath = regexp.MustCompile(`^(?:/v[0-9.]+)?/(containers|exec)/([^/]+)/(attach|start|wait)$`)

// VolumeDir is where the fake engine keeps what's in a volume, as a directory on
// this machine. A helper container reads and writes it, as a test may.
func (e *Engine) VolumeDir(name string) string {
	e.mu.Lock()
	defer e.mu.Unlock()
	return e.volumeDir(name)
}

func (e *Engine) volumeDir(name string) string {
	if e.volumeRoot == "" {
		dir, err := os.MkdirTemp("", "dockertest-volumes")
		if err != nil {
			panic(err)
		}
		e.volumeRoot = dir
	}
	dir := filepath.Join(e.volumeRoot, name)
	if err := os.MkdirAll(dir, 0o755); err != nil {
		panic(err)
	}
	return dir
}

// mountsOf says where each volume a container mounts is on this machine.
func (e *Engine) mountsOf(c *container) map[string]string {
	out := map[string]string{}
	for _, m := range c.HostConfig.Mounts {
		if m.Type == "volume" {
			out[m.Target] = e.volumeDir(m.Source)
		}
	}
	for _, m := range c.Mounts {
		if m.Type == "volume" {
			out[m.Destination] = e.volumeDir(m.Name)
		}
	}
	return out
}

// serveStream answers what holds a connection for as long as a process runs. It
// reports false for any other request.
func (e *Engine) serveStream(w http.ResponseWriter, r *http.Request) bool {
	m := streamPath.FindStringSubmatch(r.URL.Path)
	if m == nil || r.Method != http.MethodPost {
		return false
	}
	switch kind, id, action := m[1], m[2], m[3]; {
	case kind == "containers" && action == "attach":
		e.attachTo(w, r, id)
	case kind == "containers" && action == "wait":
		e.waitFor(w, id)
	case kind == "exec" && action == "start":
		e.startExec(w, r, id)
	default:
		return false
	}
	return true
}

// frames writes a stream of a process as the engine does for one with no
// terminal, each frame with its stream and size.
type frames struct {
	mu     *sync.Mutex
	w      io.Writer
	stream byte
}

func (f frames) Write(b []byte) (int, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	written := 0
	for len(b) > 0 {
		chunk := b[:min(len(b), 32<<10)]
		var head [8]byte
		head[0] = f.stream
		binary.BigEndian.PutUint32(head[4:], uint32(len(chunk))) //nolint:gosec // at most 32 KiB
		if _, err := f.w.Write(head[:]); err != nil {
			return written, err
		}
		if _, err := f.w.Write(chunk); err != nil {
			return written, err
		}
		written += len(chunk)
		b = b[len(chunk):]
	}
	return written, nil
}

// hijack turns a request into a process's streams, as the engine does: the
// client's half-close is the end of stdin.
func hijack(w http.ResponseWriter) (io.Reader, io.Writer, func(), bool) {
	hj, ok := w.(http.Hijacker)
	if !ok {
		fail(w, http.StatusInternalServerError, "this connection can't be hijacked")
		return nil, nil, nil, false
	}
	conn, rw, err := hj.Hijack()
	if err != nil {
		fail(w, http.StatusInternalServerError, "%v", err)
		return nil, nil, nil, false
	}
	_, _ = conn.Write([]byte("HTTP/1.1 101 UPGRADED\r\nContent-Type: application/vnd.docker.raw-stream\r\nConnection: Upgrade\r\nUpgrade: tcp\r\n\r\n"))
	return bufio.NewReader(rw), conn, func() { _ = conn.Close() }, true
}

// attachTo runs a helper: the agent's binary, imported as an image, whose
// command is the volume tool. It runs the same code, in this process, on the
// directories that stand for its volumes.
func (e *Engine) attachTo(w http.ResponseWriter, r *http.Request, id string) {
	e.mu.Lock()
	c, err := e.findContainer(id)
	e.mu.Unlock()
	if err != nil || c == nil {
		fail(w, http.StatusNotFound, "No such container: %s", id)
		return
	}
	if !c.img.imported {
		fail(w, http.StatusNotImplemented, "this fake attaches only to helpers")
		return
	}
	stdin := truthy(r.URL.Query().Get("stdin"))
	if stdin && !(c.Config.OpenStdin && c.Config.StdinOnce) {
		fail(w, http.StatusBadRequest, "a container made without OpenStdin and StdinOnce never hears the end of its stdin")
		return
	}
	in, out, closeConn, ok := hijack(w)
	if !ok {
		return
	}
	defer closeConn()
	<-c.started
	mu := &sync.Mutex{}
	stdout, stderr := frames{mu, out, 1}, frames{mu, out, 2}
	if !stdin {
		in = strings.NewReader("")
	}
	e.mu.Lock()
	code := -1
	var args []string
	if len(c.Config.Cmd) > 2 && c.Config.Cmd[0] == volumetool.Command {
		args = c.Config.Cmd[1:]
		mounts := e.mountsOf(c)
		dir, mounted := mounts[args[1]]
		writes := args[0] != "pack"
		switch {
		case !mounted:
			code = 1
			_, _ = stderr.Write([]byte(args[1] + " isn't mounted\n"))
		case writes && readOnly(c, args[1]):
			code = 1
			_, _ = stderr.Write([]byte("read-only file system\n"))
		default:
			args = []string{args[0], dir}
		}
	} else {
		code = 127
		_, _ = stderr.Write([]byte("exec: not found\n"))
	}
	e.mu.Unlock()
	if code < 0 {
		if os.Getenv("DOCKERTEST_TOOL_DELAY") != "" {
			time.Sleep(time.Second)
		}
		code = volumetool.Main(args, in, stdout, stderr)
	}
	e.mu.Lock()
	c.State = docker.State{Status: "exited", ExitCode: code, StartedAt: c.State.StartedAt, FinishedAt: time.Now().UTC()}
	close(c.exited)
	e.mu.Unlock()
}

func readOnly(c *container, target string) bool {
	for _, m := range c.HostConfig.Mounts {
		if m.Target == target {
			return m.ReadOnly
		}
	}
	return false
}

// waitFor answers once a container has stopped.
func (e *Engine) waitFor(w http.ResponseWriter, id string) {
	e.mu.Lock()
	c, err := e.findContainer(id)
	var exited chan struct{}
	if c != nil {
		exited = c.exited
	}
	e.mu.Unlock()
	if err != nil || c == nil {
		fail(w, http.StatusNotFound, "No such container: %s", id)
		return
	}
	if exited != nil {
		<-exited
	}
	e.mu.Lock()
	code := c.State.ExitCode
	e.mu.Unlock()
	writeJSON(w, http.StatusOK, map[string]int{"StatusCode": code})
}

// createExec makes a command to run, which waits to be started.
func (e *Engine) createExec(w http.ResponseWriter, r *http.Request, c *container) {
	var req struct {
		Cmd []string
	}
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		fail(w, http.StatusBadRequest, "%v", err)
		return
	}
	if !c.State.Running {
		fail(w, http.StatusConflict, "container %s is not running", c.ID)
		return
	}
	if c.img.Exec == nil {
		fail(w, http.StatusBadRequest, "OCI runtime exec failed: %q: executable file not found in $PATH", strings.Join(req.Cmd, " "))
		return
	}
	id := newID()
	e.execs[id] = &execRun{container: c, cmd: req.Cmd}
	writeJSON(w, http.StatusCreated, map[string]string{"Id": id})
}

// startExec runs a command, with its streams on the connection.
func (e *Engine) startExec(w http.ResponseWriter, _ *http.Request, id string) {
	e.mu.Lock()
	x := e.execs[id]
	if x == nil {
		e.mu.Unlock()
		fail(w, http.StatusNotFound, "No such exec instance: %s", id)
		return
	}
	x.running = true
	c := x.container
	run, env, mounts := c.img.Exec, c.Config.Env, e.mountsOf(c)
	e.mu.Unlock()
	in, out, closeConn, ok := hijack(w)
	if !ok {
		return
	}
	defer closeConn()
	mu := &sync.Mutex{}
	code := run(Exec{Cmd: x.cmd, Env: env, Volumes: mounts, Stdin: in, Stdout: frames{mu, out, 1}, Stderr: frames{mu, out, 2}})
	e.mu.Lock()
	x.running, x.exitCode = false, code
	e.mu.Unlock()
}

// inspectExec says how a command went.
func (e *Engine) inspectExec(w http.ResponseWriter, id string) {
	x := e.execs[id]
	if x == nil {
		fail(w, http.StatusNotFound, "No such exec instance: %s", id)
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"Running": x.running, "ExitCode": x.exitCode})
}

// importImage makes an image from a tar, as docker import does.
func (e *Engine) importImage(w http.ResponseWriter, r *http.Request) {
	q := r.URL.Query()
	ref := q.Get("repo") + ":" + q.Get("tag")
	n, err := io.Copy(io.Discard, r.Body)
	if err != nil || n == 0 {
		fail(w, http.StatusBadRequest, "archive/tar: invalid tar header")
		return
	}
	id := "sha256:" + newID()
	img := &image{id: id, repo: q.Get("repo"), imported: true}
	e.images[ref], e.images[id] = img, img
	e.Imports++
	writeJSON(w, http.StatusOK, map[string]string{"status": id})
}

// listImages lists images whose repository matches a reference filter.
func (e *Engine) listImages(w http.ResponseWriter, r *http.Request) {
	f, err := filters(r)
	if err != nil {
		fail(w, http.StatusBadRequest, "%v", err)
		return
	}
	var out []map[string]any
	for ref, img := range e.images {
		if strings.HasPrefix(ref, "sha256:") {
			continue
		}
		for _, want := range f["reference"] {
			if strings.HasPrefix(ref, want+":") {
				out = append(out, map[string]any{"Id": img.id, "RepoTags": []string{ref}})
			}
		}
	}
	writeJSON(w, http.StatusOK, out)
}

// removeImage removes an image by reference.
func (e *Engine) removeImage(w http.ResponseWriter, ref string) {
	img := e.images[ref]
	if img == nil {
		fail(w, http.StatusNotFound, "No such image: %s", ref)
		return
	}
	for k, v := range e.images {
		if v == img {
			delete(e.images, k)
		}
	}
	writeJSON(w, http.StatusOK, []map[string]string{{"Deleted": img.id}})
}

// Images counts the images it holds, by reference, as a test of what's left
// behind reads them.
func (e *Engine) Images(repo string) []string {
	e.mu.Lock()
	defer e.mu.Unlock()
	var out []string
	for ref, img := range e.images {
		if img.repo == repo && !strings.HasPrefix(ref, "sha256:") {
			out = append(out, ref)
		}
	}
	return out
}

// Helpers are what was asked of each container made from an imported image.
func (e *Engine) Helpers() []docker.CreateRequest {
	e.mu.Lock()
	defer e.mu.Unlock()
	return append([]docker.CreateRequest(nil), e.helpers...)
}

// Started counts how many times a container has started, by name.
func (e *Engine) Started(name string) int {
	e.mu.Lock()
	defer e.mu.Unlock()
	return e.starts[strings.TrimPrefix(name, "/")]
}

