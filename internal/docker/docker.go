// SPDX-License-Identifier: AGPL-3.0-only

// Package docker is a small client for the Docker Engine API over its
// unix socket: the endpoints Dockwell's drivers use, and no more. The
// official Go SDK would put megabytes of code into every agent, which
// stays under 40 MB at idle (ADR 0018), so this speaks the API directly
// with net/http. See docs/adr/0021-the-docker-driver.md.
package docker

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/url"
	"os"
	"slices"
	"strconv"
	"strings"
	"sync"
	"syscall"
	"time"
)

// DefaultSocket is where the Docker Engine listens on Linux.
const DefaultSocket = "/var/run/docker.sock"

// The API versions the client speaks. It uses the newest the engine and
// it share, and the fields it uses mean the same in all of them.
const (
	// MinAPIVersion is Docker 20.10's.
	MinAPIVersion = "1.41"
	// MaxAPIVersion is Docker 27.3's, the newest the client was written
	// against. Docker 29 takes nothing older than 1.44, and still takes it.
	MaxAPIVersion = "1.47"
)

// Client talks to one Docker Engine. It is safe for concurrent use.
type Client struct {
	// Adopted is the containers this server took on, which it reads as what they
	// are now. Nil has none, and takes none on. Set it before the first call.
	Adopted *Adopted

	socket string
	http   *http.Client

	mu      sync.Mutex
	version string // negotiated by the first successful Ping
	engine  string // the engine's version, such as "27.3.1"
	// unusable says why the client can't reach any engine, such as
	// DOCKER_HOST naming one it can't speak to.
	unusable error
}

// Unusable returns a client that reaches no engine, whose every call says
// why, so the drivers built on it say so too.
func Unusable(why error) *Client {
	c := New("")
	c.unusable = why
	return c
}

// New returns a client for the engine listening on socket.
func New(socket string) *Client {
	if socket == "" {
		socket = DefaultSocket
	}
	return &Client{
		socket: socket,
		http: &http.Client{Transport: &http.Transport{
			DialContext: func(ctx context.Context, _, _ string) (net.Conn, error) {
				var d net.Dialer
				return d.DialContext(ctx, "unix", socket)
			},
			MaxIdleConns:    2,
			IdleConnTimeout: 30 * time.Second,
		}},
	}
}

// Socket is where the client reaches the engine.
func (c *Client) Socket() string { return c.socket }

// Close lets go of idle connections.
func (c *Client) Close() { c.http.CloseIdleConnections() }

// Error is the engine refusing a request, with its own reason.
type Error struct {
	Status  int
	Message string
}

func (e *Error) Error() string { return strings.TrimSpace(e.Message) }

// mountFailure is how Docker starts to spell out a local volume's mount
// that failed. The reason comes after the last ": ".
const mountFailure = "failed to mount local volume"

// scrubMount leaves out of Docker's message what a failed mount tried: its
// device and options, which can hold a password. Docker 20.10 prints them
// all, and later versions mask only password=, while the kernel takes pass=
// too. This message goes into changes and logs that anyone who can read
// them sees, and the reason at its end is what a person needs.
func scrubMount(message string) string {
	_, rest, found := strings.Cut(message, mountFailure)
	if !found {
		return message
	}
	if i := strings.LastIndex(rest, ": "); i >= 0 {
		return message[:len(message)-len(rest)] + rest[i:]
	}
	return message
}

// IsNotFound reports the engine saying what was asked for doesn't exist.
func IsNotFound(err error) bool { return status(err) == http.StatusNotFound }

// IsConflict reports the engine refusing because of what exists, such as
// a name in use or a volume a container uses.
func IsConflict(err error) bool { return status(err) == http.StatusConflict }

// IsInUse reports the engine refusing to remove what a container uses: a
// conflict, or for a network, forbidden, as most engines answer.
func IsInUse(err error) bool {
	s := status(err)
	return s == http.StatusConflict || s == http.StatusForbidden
}

func status(err error) int {
	var e *Error
	if errors.As(err, &e) {
		return e.Status
	}
	return 0
}

// UnavailableError means the engine can't be reached, or can't be used.
// Its message says why and how to fix it, for people.
type UnavailableError struct {
	msg string
	Err error
}

func (e *UnavailableError) Error() string { return e.msg }

func (e *UnavailableError) Unwrap() error { return e.Err }

// IsUnavailable reports an engine that can't be reached or used.
func IsUnavailable(err error) bool {
	var u *UnavailableError
	return errors.As(err, &u)
}

// unavailable explains why the socket can't be reached.
func (c *Client) unavailable(err error) error {
	switch {
	case errors.Is(err, syscall.ENOENT), errors.Is(err, os.ErrNotExist):
		return &UnavailableError{Err: err, msg: fmt.Sprintf("Docker isn't installed, or its socket isn't at %s. "+
			"Install Docker Engine (https://docs.docker.com/engine/install/); the agent finds it within a minute", c.socket)}
	case errors.Is(err, syscall.ECONNREFUSED):
		return &UnavailableError{Err: err, msg: "Docker is installed, but isn't running. " +
			"Start it with systemctl start docker; the agent finds it within a minute"}
	case errors.Is(err, syscall.EACCES), errors.Is(err, syscall.EPERM):
		return &UnavailableError{Err: err, msg: fmt.Sprintf("the agent isn't allowed to use Docker's socket, %s. "+
			"Run the agent as root, as the install script does, or add its user to the docker group", c.socket)}
	}
	return &UnavailableError{Err: err, msg: fmt.Sprintf("Docker doesn't answer on %s: %v. "+
		"Check it with docker info; the agent tries again within a minute", c.socket, err)}
}

// Ping checks the engine answers, and agrees an API version with it.
func (c *Client) Ping(ctx context.Context) error {
	if c.unusable != nil {
		return &UnavailableError{msg: c.unusable.Error(), Err: c.unusable}
	}
	ctx, cancel := context.WithTimeout(ctx, 5*time.Second)
	defer cancel()
	var v struct {
		Version       string
		APIVersion    string `json:"ApiVersion"`
		MinAPIVersion string `json:"MinAPIVersion"`
	}
	if err := c.raw(ctx, http.MethodGet, "/version", nil, nil, &v); err != nil {
		return err
	}
	use := MaxAPIVersion
	if older(v.APIVersion, use) {
		use = v.APIVersion
	}
	switch {
	case older(use, MinAPIVersion):
		return &UnavailableError{msg: fmt.Sprintf("Docker %s is too old: Dockwell needs Docker 20.10 or newer, with API %s. "+
			"Upgrade Docker Engine (https://docs.docker.com/engine/install/)", v.Version, MinAPIVersion)}
	case v.MinAPIVersion != "" && older(use, v.MinAPIVersion):
		return &UnavailableError{msg: fmt.Sprintf("Docker %s no longer takes API %s, the newest this agent speaks. "+
			"Update the agent to the control plane's version", v.Version, use)}
	}
	c.mu.Lock()
	c.version, c.engine = use, v.Version
	c.mu.Unlock()
	return nil
}

// Info is what the engine says of itself: the fields Dockwell uses.
type Info struct {
	// Name is the server's hostname.
	Name string
	// DockerRootDir is where it keeps its data, such as /var/lib/docker.
	DockerRootDir string
	// DefaultRuntime is what runs a container unless it asks for another, such as
	// runc, and CgroupVersion is the server's cgroup version, 1 or 2.
	DefaultRuntime string
	CgroupVersion  string
	// NCPU is how many CPUs the server has.
	NCPU int
	// LoggingDriver is what keeps a container's log unless it asks for
	// another, such as json-file.
	LoggingDriver string
	// SecurityOptions lists how it runs, such as name=rootless.
	SecurityOptions []string
	// LiveRestoreEnabled says containers keep running while the engine restarts.
	LiveRestoreEnabled bool
	// RegistryConfig is how it reaches registries: its mirrors of Docker Hub,
	// and which registries it reaches over plain HTTP.
	RegistryConfig *RegistryConfig
}

// RegistryConfig is how the engine reaches registries, as its daemon.json says.
type RegistryConfig struct {
	// Mirrors are where it pulls Docker Hub's images from first, such as
	// "https://mirror.example.com/".
	Mirrors []string
	// IndexConfigs says, by registry, whether it's reached over HTTPS.
	IndexConfigs map[string]struct {
		Secure bool
	}
}

// Insecure reports a registry the engine reaches over plain HTTP: one its
// daemon.json names as insecure, or one on the server itself, as Docker takes
// 127.0.0.0/8 to be.
func (r *RegistryConfig) Insecure(host string) bool {
	if r != nil {
		if c, ok := r.IndexConfigs[host]; ok {
			return !c.Secure
		}
	}
	name := host
	if h, _, err := net.SplitHostPort(host); err == nil {
		name = h
	}
	if name == "localhost" {
		return true
	}
	ip := net.ParseIP(name)
	return ip != nil && ip.IsLoopback()
}

// Rootless reports an engine that runs as an ordinary user, whose host
// network isn't the server's.
func (i *Info) Rootless() bool {
	return slices.Contains(i.SecurityOptions, "name=rootless")
}

// Info asks the engine about itself.
func (c *Client) Info(ctx context.Context) (*Info, error) {
	var out Info
	if err := c.do(ctx, http.MethodGet, "/info", nil, nil, &out); err != nil {
		return nil, err
	}
	return &out, nil
}

// Architecture is what the engine runs on, in the words an image says what it's
// built for, such as amd64.
func (c *Client) Architecture(ctx context.Context) (string, error) {
	var v struct {
		Arch string `json:"Arch"`
	}
	if err := c.raw(ctx, http.MethodGet, "/version", nil, nil, &v); err != nil {
		return "", err
	}
	return v.Arch, nil
}

// Version is the API version agreed by the last Ping, and the engine's
// own version; both are empty before one succeeds.
func (c *Client) Version() (api, engine string) {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.version, c.engine
}

// older reports whether API version a comes before b, as "1.41" before
// "1.47". A version that can't be read is older than any.
func older(a, b string) bool {
	am, an, aok := apiVersion(a)
	bm, bn, bok := apiVersion(b)
	switch {
	case !aok:
		return bok
	case !bok:
		return false
	case am != bm:
		return am < bm
	}
	return an < bn
}

func apiVersion(v string) (int, int, bool) {
	major, minor, ok := strings.Cut(v, ".")
	if !ok {
		return 0, 0, false
	}
	m, err1 := strconv.Atoi(major)
	n, err2 := strconv.Atoi(minor)
	return m, n, err1 == nil && err2 == nil
}

// AtLeast reports whether the agreed API version is v or newer.
func (c *Client) AtLeast(v string) bool {
	api, _ := c.Version()
	return api != "" && !older(api, v)
}

// versioned returns path under the agreed API version, such as
// /v1.47/containers/json, agreeing it first if no Ping has yet.
func (c *Client) versioned(ctx context.Context, path string) (string, error) {
	api, _ := c.Version()
	if api == "" {
		if err := c.Ping(ctx); err != nil {
			return "", err
		}
		api, _ = c.Version()
	}
	return "/v" + api + path, nil
}

// do sends a request to a versioned endpoint.
func (c *Client) do(ctx context.Context, method, path string, query url.Values, body, out any) error {
	path, err := c.versioned(ctx, path)
	if err != nil {
		return err
	}
	return c.raw(ctx, method, path, query, body, out)
}

// raw sends a request and decodes the answer into out, unless out is
// nil.
func (c *Client) raw(ctx context.Context, method, path string, query url.Values, body, out any) error {
	res, err := c.send(ctx, method, path, query, body)
	if err != nil {
		return err
	}
	defer res.Body.Close()
	defer discard(res.Body)
	if out == nil || res.StatusCode == http.StatusNoContent || res.StatusCode == http.StatusNotModified {
		return nil
	}
	if err := json.NewDecoder(res.Body).Decode(out); err != nil {
		return fmt.Errorf("can't read Docker's answer to %s %s: %w", method, path, err)
	}
	return nil
}

// send sends a request, and turns an answer of 400 or more into an
// *Error. The caller closes the body of what it returns.
func (c *Client) send(ctx context.Context, method, path string, query url.Values, body any) (*http.Response, error) {
	var r io.Reader = http.NoBody
	if body != nil {
		b, err := json.Marshal(body)
		if err != nil {
			return nil, err
		}
		r = bytes.NewReader(b)
	}
	u := "http://docker" + path
	if len(query) > 0 {
		u += "?" + query.Encode()
	}
	req, err := http.NewRequestWithContext(ctx, method, u, r)
	if err != nil {
		return nil, err
	}
	if body != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	res, err := c.http.Do(req)
	if err != nil {
		switch cerr := ctx.Err(); {
		case errors.Is(cerr, context.DeadlineExceeded):
			// Docker took the request, or the connection, and sent
			// nothing back before the caller stopped waiting.
			return nil, &UnavailableError{Err: cerr, msg: fmt.Sprintf("Docker isn't answering on %s: it sent nothing back in time. "+
				"Check it with systemctl status docker; the agent tries again within a minute", c.socket)}
		case cerr != nil:
			return nil, cerr
		}
		var op *net.OpError
		if errors.As(err, &op) && op.Op == "dial" {
			return nil, c.unavailable(err)
		}
		return nil, fmt.Errorf("can't reach Docker at %s: %w", c.socket, err)
	}
	if res.StatusCode >= 400 {
		defer res.Body.Close()
		var e struct{ Message string }
		b, _ := io.ReadAll(io.LimitReader(res.Body, 64<<10))
		if json.Unmarshal(b, &e) != nil || e.Message == "" {
			e.Message = strings.TrimSpace(string(b))
		}
		if e.Message == "" {
			e.Message = http.StatusText(res.StatusCode)
		}
		return nil, &Error{Status: res.StatusCode, Message: scrubMount(e.Message)}
	}
	return res, nil
}

// discard reads what is left of a body, up to a limit, so its
// connection can be used again.
func discard(body io.Reader) {
	_, _ = io.Copy(io.Discard, io.LimitReader(body, 1<<20))
}

// esc escapes a name for a path.
func esc(name string) string { return url.PathEscape(name) }

// SocketFrom finds the engine's socket as the docker CLI does: from
// DOCKER_HOST when it names a unix socket, and DefaultSocket when it is
// unset. getenv reads the environment.
func SocketFrom(getenv func(string) string) (string, error) {
	host := getenv("DOCKER_HOST")
	switch {
	case host == "":
		return DefaultSocket, nil
	case strings.HasPrefix(host, "unix://"):
		return strings.TrimPrefix(host, "unix://"), nil
	}
	return "", fmt.Errorf("DOCKER_HOST is %s, but the agent reaches Docker over its unix socket only. "+
		"Unset DOCKER_HOST, or point it at the socket, such as unix:///var/run/docker.sock", host)
}
