// SPDX-License-Identifier: AGPL-3.0-only

// Package dockertest is a fake Docker Engine for tests: an HTTP server on
// a unix socket that speaks the endpoints the docker package uses, and
// behaves as the engine does where Dockwell can tell. Containers take what
// they leave out from their image, as the engine fills them in; volumes a
// container mounts are made when missing; a volume or network in use can't
// be removed.
package dockertest

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"maps"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"regexp"
	"slices"
	"strings"
	"sync"
	"testing"

	"github.com/dockwell/dockwell/internal/docker"
)

// Image is an image the fake can pull.
type Image struct {
	Config docker.Config
	// Volumes are paths the image declares, which get anonymous volumes.
	Volumes []string
	// Exits makes a container of it stop right after it starts, with
	// exit code 1.
	Exits bool
	// Unhealthy makes its health check fail.
	Unhealthy bool
	// Size is what pulling it downloads, in bytes.
	Size int64
}

// Engine is a fake Docker Engine.
type Engine struct {
	// Socket is where it listens.
	Socket string
	// APIVersion and MinAPIVersion are what /version reports.
	APIVersion, MinAPIVersion string

	srv *httptest.Server

	mu sync.Mutex
	// Registry has the images a pull can find, by reference.
	Registry   map[string]Image
	images     map[string]*image // pulled, by reference and by ID
	containers map[string]*container
	volumes    map[string]*docker.Volume
	networks   map[string]*docker.Network
	// Pulls counts pulls, and Creates containers created.
	Pulls, Creates int
	// Requests logs each request, as "METHOD /path".
	Requests []string
}

type image struct {
	id string
	Image
}

type container struct {
	docker.Container
	img *image
}

// New starts a fake engine on a socket in a test's temporary directory,
// with Docker's default bridge network, and stops it when the test ends.
func New(t testing.TB) *Engine {
	t.Helper()
	// A socket's path must stay short, under 108 bytes on Linux.
	dir, err := filepath.Abs(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	if len(dir) > 80 {
		dir = shortDir(t)
	}
	e := &Engine{
		Socket:     filepath.Join(dir, "docker.sock"),
		APIVersion: "1.47", MinAPIVersion: "1.24",
		Registry:   map[string]Image{},
		images:     map[string]*image{},
		containers: map[string]*container{},
		volumes:    map[string]*docker.Volume{},
		networks: map[string]*docker.Network{
			"bridge": {ID: newID(), Name: "bridge", Driver: "bridge", Options: map[string]string{}, Labels: map[string]string{}},
		},
	}
	l, err := (&net.ListenConfig{}).Listen(context.Background(), "unix", e.Socket)
	if err != nil {
		t.Fatal(err)
	}
	e.srv = httptest.NewUnstartedServer(http.HandlerFunc(e.serve))
	_ = e.srv.Listener.Close()
	e.srv.Listener = l
	e.srv.Start()
	t.Cleanup(e.srv.Close)
	return e
}

func shortDir(t testing.TB) string {
	t.Helper()
	dir, err := os.MkdirTemp("", "dw")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.RemoveAll(dir) })
	return dir
}

// Client returns a client for the engine.
func (e *Engine) Client() *docker.Client { return docker.New(e.Socket) }

func newID() string {
	b := make([]byte, 32)
	_, _ = rand.Read(b)
	return hex.EncodeToString(b)
}

func writeJSON(w http.ResponseWriter, status int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(v)
}

func fail(w http.ResponseWriter, status int, format string, args ...any) {
	writeJSON(w, status, map[string]string{"message": fmt.Sprintf(format, args...)})
}

var versioned = regexp.MustCompile(`^/v\d+\.\d+(/.*)$`)

func (e *Engine) serve(w http.ResponseWriter, r *http.Request) {
	e.mu.Lock()
	defer e.mu.Unlock()
	path := r.URL.Path
	e.Requests = append(e.Requests, r.Method+" "+path)
	if m := versioned.FindStringSubmatch(path); m != nil {
		path = m[1]
	}
	parts := strings.Split(strings.Trim(path, "/"), "/")
	switch {
	case path == "/_ping":
		_, _ = w.Write([]byte("OK"))
	case path == "/version":
		writeJSON(w, http.StatusOK, map[string]string{"Version": "27.3.1", "ApiVersion": e.APIVersion, "MinAPIVersion": e.MinAPIVersion})
	case parts[0] == "containers":
		e.serveContainers(w, r, parts[1:])
	case parts[0] == "images":
		e.serveImages(w, r, path)
	case parts[0] == "volumes":
		e.serveVolumes(w, r, parts[1:])
	case parts[0] == "networks":
		e.serveNetworks(w, r, parts[1:])
	default:
		fail(w, http.StatusNotFound, "page not found")
	}
}

// image finds a pulled image by reference or ID.
func (e *Engine) image(ref string) *image {
	if img, ok := e.images[docker.WithTag(ref)]; ok {
		return img
	}
	return e.images[ref]
}

func (e *Engine) serveImages(w http.ResponseWriter, r *http.Request, path string) {
	switch {
	case r.Method == http.MethodPost && path == "/images/create":
		ref := r.URL.Query().Get("fromImage")
		if tag := r.URL.Query().Get("tag"); tag != "" {
			ref += ":" + tag
		}
		e.Pulls++
		w.Header().Set("Content-Type", "application/json")
		enc := json.NewEncoder(w)
		src, ok := e.Registry[ref]
		if !ok {
			_ = enc.Encode(map[string]any{
				"error":       "pull access denied for " + ref + ", repository does not exist or may require 'docker login'",
				"errorDetail": map[string]string{"message": "pull access denied for " + ref + ", repository does not exist or may require 'docker login'"},
			})
			return
		}
		_ = enc.Encode(map[string]string{"status": "Pulling from " + ref})
		if src.Size > 0 {
			for _, part := range []int64{src.Size / 4, src.Size / 2, src.Size} {
				_ = enc.Encode(map[string]any{"status": "Downloading", "id": "layer1", "progressDetail": map[string]int64{"current": part, "total": src.Size}})
			}
			_ = enc.Encode(map[string]any{"status": "Pull complete", "id": "layer1"})
		}
		img := &image{id: "sha256:" + newID(), Image: src}
		e.images[ref], e.images[img.id] = img, img
		_ = enc.Encode(map[string]string{"status": "Status: Downloaded newer image for " + ref})
	case r.Method == http.MethodGet && strings.HasSuffix(path, "/json"):
		ref := strings.TrimSuffix(strings.TrimPrefix(path, "/images/"), "/json")
		img := e.image(ref)
		if img == nil {
			fail(w, http.StatusNotFound, "No such image: %s", ref)
			return
		}
		cfg := img.Config
		writeJSON(w, http.StatusOK, docker.Image{ID: img.id, Config: &cfg})
	default:
		fail(w, http.StatusNotFound, "page not found")
	}
}

// findContainer finds a container by name or ID.
func (e *Engine) findContainer(key string) *container {
	if c, ok := e.containers[key]; ok {
		return c
	}
	for _, c := range e.containers {
		if c.Name == "/"+key {
			return c
		}
	}
	return nil
}

func (e *Engine) serveContainers(w http.ResponseWriter, r *http.Request, parts []string) {
	if len(parts) == 1 && parts[0] == "create" && r.Method == http.MethodPost {
		e.create(w, r)
		return
	}
	if len(parts) == 0 {
		fail(w, http.StatusNotFound, "page not found")
		return
	}
	c := e.findContainer(parts[0])
	if c == nil {
		fail(w, http.StatusNotFound, "No such container: %s", parts[0])
		return
	}
	action := ""
	if len(parts) > 1 {
		action = parts[1]
	}
	switch {
	case r.Method == http.MethodGet && action == "json":
		writeJSON(w, http.StatusOK, c.Container)
	case r.Method == http.MethodPost && action == "start":
		if c.State.Running {
			w.WriteHeader(http.StatusNotModified)
			return
		}
		for name := range c.NetworkSettings.Networks {
			if e.networks[name] == nil {
				fail(w, http.StatusNotFound, "network %s not found", name)
				return
			}
		}
		if err := e.portsFree(c); err != nil {
			fail(w, http.StatusInternalServerError, "%v", err)
			return
		}
		c.State = docker.State{Status: "running", Running: true}
		if c.img.Exits {
			c.State = docker.State{Status: "exited", ExitCode: 1}
		}
		if hc := c.Config.Healthcheck; hc != nil && len(hc.Test) > 0 && hc.Test[0] != "NONE" && c.State.Running {
			c.State.Health = &docker.Health{Status: "healthy"}
			if c.img.Unhealthy {
				c.State.Health = &docker.Health{Status: "unhealthy", FailingStreak: 3}
			}
		}
		e.attach(c)
		w.WriteHeader(http.StatusNoContent)
	case r.Method == http.MethodPost && action == "stop":
		if !c.State.Running {
			w.WriteHeader(http.StatusNotModified)
			return
		}
		c.State = docker.State{Status: "exited"}
		e.detach(c)
		w.WriteHeader(http.StatusNoContent)
	case r.Method == http.MethodPost && action == "rename":
		name := r.URL.Query().Get("name")
		if e.findContainer(name) != nil {
			fail(w, http.StatusConflict, "Conflict. The container name \"/%s\" is already in use", name)
			return
		}
		c.Name = "/" + name
		w.WriteHeader(http.StatusNoContent)
	case r.Method == http.MethodPost && action == "update":
		var body struct{ RestartPolicy docker.RestartPolicy }
		if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
			fail(w, http.StatusBadRequest, "%v", err)
			return
		}
		c.HostConfig.RestartPolicy = body.RestartPolicy
		writeJSON(w, http.StatusOK, map[string]any{"Warnings": []string{}})
	case r.Method == http.MethodDelete && action == "":
		if c.State.Running && r.URL.Query().Get("force") != "true" {
			fail(w, http.StatusConflict, "cannot remove container %s: container is running: stop the container before removing or force remove", c.Name)
			return
		}
		e.detach(c)
		delete(e.containers, c.ID)
		w.WriteHeader(http.StatusNoContent)
	default:
		fail(w, http.StatusNotFound, "page not found")
	}
}

func (e *Engine) portsFree(c *container) error {
	for _, o := range e.containers {
		if o == c || !o.State.Running {
			continue
		}
		for port, bs := range c.HostConfig.PortBindings {
			for _, b := range bs {
				for oport, obs := range o.HostConfig.PortBindings {
					for _, ob := range obs {
						if b.HostPort != "" && b.HostPort == ob.HostPort && proto(port) == proto(oport) {
							return fmt.Errorf("driver failed programming external connectivity on endpoint %s: Bind for 0.0.0.0:%s failed: port is already allocated",
								strings.TrimPrefix(c.Name, "/"), b.HostPort)
						}
					}
				}
			}
		}
	}
	return nil
}

func proto(port string) string {
	_, p, _ := strings.Cut(port, "/")
	return p
}

// attach and detach keep each network's list of running containers.
func (e *Engine) attach(c *container) {
	for name := range c.NetworkSettings.Networks {
		if n := e.networks[name]; n != nil {
			if n.Containers == nil {
				n.Containers = map[string]struct {
					Name string `json:"Name"`
				}{}
			}
			n.Containers[c.ID] = struct {
				Name string `json:"Name"`
			}{strings.TrimPrefix(c.Name, "/")}
		}
	}
}

func (e *Engine) detach(c *container) {
	for _, n := range e.networks {
		delete(n.Containers, c.ID)
	}
}

func (e *Engine) create(w http.ResponseWriter, r *http.Request) {
	var req docker.CreateRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		fail(w, http.StatusBadRequest, "%v", err)
		return
	}
	name := r.URL.Query().Get("name")
	if e.findContainer(name) != nil {
		fail(w, http.StatusConflict, "Conflict. The container name \"/%s\" is already in use by another container", name)
		return
	}
	img := e.image(req.Image)
	if img == nil {
		fail(w, http.StatusNotFound, "No such image: %s", req.Image)
		return
	}
	c := &container{img: img}
	c.ID = newID()
	c.Name = "/" + name
	c.Image = img.id
	c.Config = merge(req.Config, img.Config)
	c.HostConfig = req.HostConfig
	c.State = docker.State{Status: "created"}
	mode := req.HostConfig.NetworkMode
	if mode == "" || mode == "default" {
		mode = "bridge"
		c.HostConfig.NetworkMode = "bridge"
	}
	if e.networks[mode] == nil {
		fail(w, http.StatusNotFound, "network %s not found", mode)
		return
	}
	c.NetworkSettings.Networks = map[string]*docker.EndpointSettings{mode: {}}
	for n, s := range req.NetworkingConfig.EndpointsConfig {
		if e.networks[n] == nil {
			fail(w, http.StatusNotFound, "network %s not found", n)
			return
		}
		c.NetworkSettings.Networks[n] = s
	}
	covered := map[string]bool{}
	for _, m := range req.HostConfig.Mounts {
		covered[m.Target] = true
		mp := docker.MountPoint{Type: m.Type, Source: m.Source, Destination: m.Target, RW: !m.ReadOnly}
		if m.Type == "volume" {
			e.ensureVolume(m.Source, nil)
			mp.Name = m.Source
		}
		c.Mounts = append(c.Mounts, mp)
	}
	for _, b := range req.HostConfig.Binds {
		parts := strings.Split(b, ":")
		if len(parts) < 2 {
			fail(w, http.StatusBadRequest, "invalid volume specification: '%s'", b)
			return
		}
		covered[parts[1]] = true
		mp := docker.MountPoint{Type: "bind", Source: parts[0], Destination: parts[1], RW: len(parts) < 3 || parts[2] != "ro"}
		if !strings.HasPrefix(parts[0], "/") {
			e.ensureVolume(parts[0], nil)
			mp.Type, mp.Name = "volume", parts[0]
		}
		c.Mounts = append(c.Mounts, mp)
	}
	for _, path := range img.Volumes {
		if !covered[path] {
			v := e.ensureVolume(newID(), map[string]string{"com.docker.volume.anonymous": ""})
			c.Mounts = append(c.Mounts, docker.MountPoint{Type: "volume", Name: v.Name, Destination: path, RW: true})
		}
	}
	e.containers[c.ID] = c
	e.Creates++
	writeJSON(w, http.StatusCreated, map[string]any{"Id": c.ID, "Warnings": []string{}})
}

// merge fills in what a container leaves out from its image, as the
// engine does.
func merge(c, img docker.Config) docker.Config {
	if c.User == "" {
		c.User = img.User
	}
	if c.WorkingDir == "" {
		c.WorkingDir = img.WorkingDir
	}
	if len(c.Entrypoint) == 0 {
		if len(c.Cmd) == 0 {
			c.Cmd = img.Cmd
		}
		if c.Entrypoint == nil {
			c.Entrypoint = img.Entrypoint
		}
	}
	have := map[string]bool{}
	for _, kv := range c.Env {
		k, _, _ := strings.Cut(kv, "=")
		have[k] = true
	}
	for _, kv := range img.Env {
		if k, _, _ := strings.Cut(kv, "="); !have[k] {
			c.Env = append(c.Env, kv)
		}
	}
	labels := maps.Clone(img.Labels)
	if labels == nil {
		labels = map[string]string{}
	}
	maps.Copy(labels, c.Labels)
	c.Labels = labels
	if ih := img.Healthcheck; ih != nil {
		if c.Healthcheck == nil {
			hc := *ih
			c.Healthcheck = &hc
		} else {
			hc := c.Healthcheck
			if len(hc.Test) == 0 {
				hc.Test = ih.Test
			}
			if hc.Interval == 0 {
				hc.Interval = ih.Interval
			}
			if hc.Timeout == 0 {
				hc.Timeout = ih.Timeout
			}
			if hc.StartPeriod == 0 {
				hc.StartPeriod = ih.StartPeriod
			}
			if hc.Retries == 0 {
				hc.Retries = ih.Retries
			}
		}
	}
	if c.ExposedPorts == nil {
		c.ExposedPorts = map[string]struct{}{}
	}
	maps.Copy(c.ExposedPorts, img.ExposedPorts)
	return c
}

func (e *Engine) ensureVolume(name string, labels map[string]string) *docker.Volume {
	if v, ok := e.volumes[name]; ok {
		return v
	}
	if labels == nil {
		labels = map[string]string{}
	}
	v := &docker.Volume{Name: name, Driver: "local", Options: map[string]string{}, Labels: labels, CreatedAt: "2026-11-04T10:00:00Z"}
	e.volumes[name] = v
	return v
}

// volumeInUse reports a container that mounts the volume.
func (e *Engine) volumeInUse(name string) string {
	for _, c := range e.containers {
		for _, m := range c.Mounts {
			if m.Name == name {
				return c.ID
			}
		}
	}
	return ""
}

func (e *Engine) serveVolumes(w http.ResponseWriter, r *http.Request, parts []string) {
	switch {
	case r.Method == http.MethodPost && len(parts) == 1 && parts[0] == "create":
		var req docker.VolumeCreate
		if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
			fail(w, http.StatusBadRequest, "%v", err)
			return
		}
		if v, ok := e.volumes[req.Name]; ok {
			driver := req.Driver
			if driver == "" {
				driver = "local"
			}
			if driver != v.Driver {
				fail(w, http.StatusConflict, "create %s: volume name must be unique", req.Name)
				return
			}
			writeJSON(w, http.StatusCreated, v)
			return
		}
		v := e.ensureVolume(req.Name, maps.Clone(req.Labels))
		if req.Driver != "" {
			v.Driver = req.Driver
		}
		if req.DriverOpts != nil {
			v.Options = maps.Clone(req.DriverOpts)
		}
		writeJSON(w, http.StatusCreated, v)
	case len(parts) == 1 && r.Method == http.MethodGet:
		v, ok := e.volumes[parts[0]]
		if !ok {
			fail(w, http.StatusNotFound, "get %s: no such volume", parts[0])
			return
		}
		writeJSON(w, http.StatusOK, v)
	case len(parts) == 1 && r.Method == http.MethodDelete:
		if _, ok := e.volumes[parts[0]]; !ok {
			fail(w, http.StatusNotFound, "get %s: no such volume", parts[0])
			return
		}
		if id := e.volumeInUse(parts[0]); id != "" {
			fail(w, http.StatusConflict, "remove %s: volume is in use - [%s]", parts[0], id)
			return
		}
		delete(e.volumes, parts[0])
		w.WriteHeader(http.StatusNoContent)
	default:
		fail(w, http.StatusNotFound, "page not found")
	}
}

func (e *Engine) findNetwork(key string) *docker.Network {
	if n, ok := e.networks[key]; ok {
		return n
	}
	for _, n := range e.networks {
		if n.ID == key {
			return n
		}
	}
	return nil
}

func (e *Engine) serveNetworks(w http.ResponseWriter, r *http.Request, parts []string) {
	switch {
	case r.Method == http.MethodPost && len(parts) == 1 && parts[0] == "create":
		var req docker.NetworkCreate
		if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
			fail(w, http.StatusBadRequest, "%v", err)
			return
		}
		if e.networks[req.Name] != nil {
			fail(w, http.StatusConflict, "network with name %s already exists", req.Name)
			return
		}
		n := &docker.Network{ID: newID(), Name: req.Name, Driver: req.Driver, Internal: req.Internal, Options: maps.Clone(req.Options), Labels: maps.Clone(req.Labels)}
		if n.Driver == "" {
			n.Driver = "bridge"
		}
		if n.Options == nil {
			n.Options = map[string]string{}
		}
		if n.Labels == nil {
			n.Labels = map[string]string{}
		}
		e.networks[req.Name] = n
		writeJSON(w, http.StatusCreated, map[string]string{"Id": n.ID})
	case len(parts) == 1 && r.Method == http.MethodGet:
		n := e.findNetwork(parts[0])
		if n == nil {
			fail(w, http.StatusNotFound, "network %s not found", parts[0])
			return
		}
		writeJSON(w, http.StatusOK, n)
	case len(parts) == 1 && r.Method == http.MethodDelete:
		n := e.findNetwork(parts[0])
		if n == nil {
			fail(w, http.StatusNotFound, "network %s not found", parts[0])
			return
		}
		for _, c := range e.containers {
			if _, on := c.NetworkSettings.Networks[n.Name]; on {
				fail(w, http.StatusForbidden, "error while removing network: network %s id %s has active endpoints", n.Name, n.ID)
				return
			}
		}
		delete(e.networks, n.Name)
		w.WriteHeader(http.StatusNoContent)
	case len(parts) == 2 && parts[1] == "connect" && r.Method == http.MethodPost:
		n := e.findNetwork(parts[0])
		if n == nil {
			fail(w, http.StatusNotFound, "network %s not found", parts[0])
			return
		}
		var req struct {
			Container      string
			EndpointConfig docker.EndpointSettings
		}
		if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
			fail(w, http.StatusBadRequest, "%v", err)
			return
		}
		c := e.findContainer(req.Container)
		if c == nil {
			fail(w, http.StatusNotFound, "No such container: %s", req.Container)
			return
		}
		s := req.EndpointConfig
		c.NetworkSettings.Networks[n.Name] = &s
		if c.State.Running {
			e.attach(c)
		}
		w.WriteHeader(http.StatusOK)
	default:
		fail(w, http.StatusNotFound, "page not found")
	}
}

// Container returns a container by name, as the engine reports it, or
// nil.
func (e *Engine) Container(name string) *docker.Container {
	e.mu.Lock()
	defer e.mu.Unlock()
	c := e.findContainer(name)
	if c == nil {
		return nil
	}
	out := c.Container
	return &out
}

// Containers lists the containers' names, sorted.
func (e *Engine) Containers() []string {
	e.mu.Lock()
	defer e.mu.Unlock()
	var out []string
	for _, c := range e.containers {
		out = append(out, strings.TrimPrefix(c.Name, "/"))
	}
	slices.Sort(out)
	return out
}

// Edit changes a container behind Dockwell's back, as someone running
// docker by hand would.
func (e *Engine) Edit(name string, edit func(*docker.Container)) {
	e.mu.Lock()
	defer e.mu.Unlock()
	if c := e.findContainer(name); c != nil {
		edit(&c.Container)
	}
}

// Stop stops a container behind Dockwell's back.
func (e *Engine) Stop(name string) {
	e.mu.Lock()
	defer e.mu.Unlock()
	if c := e.findContainer(name); c != nil {
		c.State = docker.State{Status: "exited"}
		e.detach(c)
	}
}

// Volume returns a volume by name, or nil.
func (e *Engine) Volume(name string) *docker.Volume {
	e.mu.Lock()
	defer e.mu.Unlock()
	v, ok := e.volumes[name]
	if !ok {
		return nil
	}
	out := *v
	return &out
}

// Network returns a network by name, or nil.
func (e *Engine) Network(name string) *docker.Network {
	e.mu.Lock()
	defer e.mu.Unlock()
	n, ok := e.networks[name]
	if !ok {
		return nil
	}
	out := *n
	return &out
}

// AddVolume makes a volume behind Dockwell's back.
func (e *Engine) AddVolume(v docker.Volume) {
	e.mu.Lock()
	defer e.mu.Unlock()
	e.volumes[v.Name] = &v
}

// AddNetwork makes a network behind Dockwell's back.
func (e *Engine) AddNetwork(n docker.Network) {
	e.mu.Lock()
	defer e.mu.Unlock()
	if n.ID == "" {
		n.ID = newID()
	}
	e.networks[n.Name] = &n
}
