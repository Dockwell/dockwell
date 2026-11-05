// SPDX-License-Identifier: AGPL-3.0-only

// Command logload serves the fake Docker Engine the tests use on a unix socket,
// with a small HTTP interface to make its containers write, for measuring logs
// end to end on a machine without Docker: point dockwell at the socket with
// DOCKER_HOST=unix://<socket>. web/e2e/logs.ts drives it. It is never part of a
// release. CI's Docker job reads real containers' logs.
//
//	go run ./scripts/logload /tmp/load 127.0.0.1:7699
//
// The HTTP interface:
//
//	POST /print?stream=stdout   each line of the body, written now, by every running container
//	POST /rate?lines=5000&size=120   this many lines a second until told otherwise; 0 stops
//	POST /flood?size=200   as fast as whoever follows can read, until POST /flood?size=0
//	POST /replica   a second replica of the first container
//	POST /stop?service=web   stops that service's containers, as a crash would
//	GET  /written   how many lines have been written, as text
//	GET  /followers   how many readers follow a log now
package main

import (
	"bufio"
	"context"
	"fmt"
	"log"
	"net"
	"net/http"
	"os"
	"os/signal"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"syscall"
	"testing"
	"time"

	"github.com/dockwell/dockwell/internal/docker"
	"github.com/dockwell/dockwell/internal/docker/dockertest"
)

// tb is what dockertest asks of a test.
type tb struct {
	testing.TB
	dir     string
	cleanup []func()
}

func (t *tb) Helper()                         {}
func (t *tb) TempDir() string                 { return t.dir }
func (t *tb) Cleanup(f func())                { t.cleanup = append(t.cleanup, f) }
func (t *tb) Fatal(args ...any)               { log.Fatal(args...) }
func (t *tb) Fatalf(format string, a ...any)  { log.Fatalf(format, a...) }
func (t *tb) Errorf(format string, a ...any)  { log.Printf(format, a...) }
func (t *tb) Logf(format string, args ...any) { log.Printf(format, args...) }

func main() {
	if len(os.Args) != 3 {
		log.Fatal("logload takes the directory to put its socket in, and the address for its HTTP interface, such as /tmp/load 127.0.0.1:7699")
	}
	dir, err := filepath.Abs(os.Args[1])
	if err != nil {
		log.Fatalf("can't read that directory: %v", err)
	}
	if err := os.MkdirAll(dir, 0o700); err != nil { //nolint:gosec // a test tool's own argument
		log.Fatalf("can't make that directory: %v", err)
	}
	t := &tb{dir: dir}
	e := dockertest.New(t)
	// Any image can be pulled. One tagged broken prints why, and exits at once.
	e.Find = func(ref string) (dockertest.Image, bool) {
		img := dockertest.Image{Size: 8 << 20, Config: docker.Config{Cmd: []string{"serve"}, Env: []string{"PATH=/usr/bin:/bin"}}}
		if strings.HasSuffix(ref, ":broken") {
			img.Exits = true
			img.Prints = []string{
				"INFO starting up",
				"\x1b[31mERROR\x1b[0m can't reach the database at db:5432: connection refused",
				"    at connect (db.go:42)",
				"    at main (main.go:12)",
				"FATAL giving up after 3 tries",
			}
		}
		return img, true
	}
	s := &server{e: e}
	mux := http.NewServeMux()
	mux.HandleFunc("POST /print", s.print)
	mux.HandleFunc("POST /rate", s.rate)
	mux.HandleFunc("POST /flood", s.flood)
	mux.HandleFunc("POST /replica", func(w http.ResponseWriter, _ *http.Request) {
		// Another replica of the first container there is, on this one server.
		names := s.running()
		if len(names) == 0 {
			http.Error(w, "no container is running", http.StatusConflict)
			return
		}
		first := e.Container(names[0])
		labels := map[string]string{}
		for k, v := range first.Config.Labels {
			labels[k] = v
		}
		labels[docker.LabelNumber] = "2"
		name := strings.TrimSuffix(strings.TrimPrefix(first.Name, "/"), "-1") + "-2"
		e.AddContainer(docker.Container{
			Name:    "/" + name,
			Created: time.Now().UTC(),
			Config:  docker.Config{Image: first.Config.Image, Labels: labels},
			State:   docker.State{Status: "running", Running: true, StartedAt: time.Now().UTC()},
		})
		w.WriteHeader(http.StatusNoContent)
	})
	mux.HandleFunc("POST /stop", func(w http.ResponseWriter, r *http.Request) {
		service := r.URL.Query().Get("service")
		for _, name := range s.running() {
			if c := e.Container(name); c != nil && c.Config.Labels[docker.LabelService] == service {
				e.Stop(name)
			}
		}
		w.WriteHeader(http.StatusNoContent)
	})
	mux.HandleFunc("GET /written", func(w http.ResponseWriter, _ *http.Request) {
		fmt.Fprintln(w, s.written.Load()+s.floodLines())
	})
	mux.HandleFunc("GET /followers", func(w http.ResponseWriter, _ *http.Request) {
		fmt.Fprintln(w, e.Followers())
	})
	go func() {
		srv := &http.Server{Addr: os.Args[2], Handler: mux, ReadHeaderTimeout: 5 * time.Second}
		// With port 0 it takes one that is free, and says which.
		l, err := (&net.ListenConfig{}).Listen(context.Background(), "tcp", srv.Addr)
		if err != nil {
			log.Fatal(err)
		}
		fmt.Fprintln(os.Stderr, "logload: listening on", l.Addr())
		log.Fatal(srv.Serve(l))
	}()
	fmt.Println(e.Socket)
	stop := make(chan os.Signal, 1)
	signal.Notify(stop, os.Interrupt, syscall.SIGTERM)
	<-stop
	for _, f := range t.cleanup {
		f()
	}
}

type server struct {
	e       *dockertest.Engine
	written atomic.Uint64
	mu      sync.Mutex
	stop    chan struct{}
	floods  []*dockertest.Flood
}

func (s *server) floodLines() uint64 {
	s.mu.Lock()
	defer s.mu.Unlock()
	n := uint64(0)
	for _, f := range s.floods {
		n += f.Lines()
	}
	return n
}

// running lists the containers that run now.
func (s *server) running() []string {
	var out []string
	for _, name := range s.e.Containers() {
		if c := s.e.Container(name); c != nil && c.State.Running {
			out = append(out, name)
		}
	}
	return out
}

func (s *server) print(w http.ResponseWriter, r *http.Request) {
	stream := docker.Stdout
	if r.URL.Query().Get("stream") == "stderr" {
		stream = docker.Stderr
	}
	var lines []string
	sc := bufio.NewScanner(r.Body)
	sc.Buffer(make([]byte, 0, 1<<20), 16<<20)
	for sc.Scan() {
		lines = append(lines, sc.Text())
	}
	for _, name := range s.running() {
		s.e.Print(name, stream, lines...)
		s.written.Add(uint64(len(lines)))
	}
	w.WriteHeader(http.StatusNoContent)
}

func (s *server) rate(w http.ResponseWriter, r *http.Request) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.stop != nil {
		close(s.stop)
		s.stop = nil
	}
	lines, _ := strconv.Atoi(r.URL.Query().Get("lines"))
	size, _ := strconv.Atoi(r.URL.Query().Get("size"))
	if lines > 0 {
		s.stop = make(chan struct{})
		go s.write(s.stop, lines, max(size, 40))
	}
	w.WriteHeader(http.StatusNoContent)
}

// write writes lines a second, a batch every 10 ms, each saying when it was
// written, so a reader can tell how long it took.
func (s *server) write(stop <-chan struct{}, rate, size int) {
	const every = 10 * time.Millisecond
	tick := time.NewTicker(every)
	defer tick.Stop()
	start := time.Now()
	sent := 0
	levels := []string{"INFO", "INFO", "INFO", "INFO", "INFO", "INFO", "INFO", "INFO", "WARN", "ERROR"}
	for {
		select {
		case <-stop:
			return
		case now := <-tick.C:
			due := int(now.Sub(start).Seconds()*float64(rate)) - sent
			if due <= 0 {
				continue
			}
			batch := make([]string, due)
			for i := range batch {
				sent++
				body := fmt.Sprintf("%s GET /v1/products/%d 200 %d ms at=%d ", levels[sent%len(levels)], sent, sent%97, now.UnixMilli())
				batch[i] = body + strings.Repeat("x", max(size-len(body), 0))
			}
			for _, name := range s.running() {
				s.e.Print(name, docker.Stdout, batch...)
				s.written.Add(uint64(len(batch)))
			}
		}
	}
}

func (s *server) flood(w http.ResponseWriter, r *http.Request) {
	s.mu.Lock()
	defer s.mu.Unlock()
	size, _ := strconv.Atoi(r.URL.Query().Get("size"))
	for _, f := range s.floods {
		f.Stop()
	}
	if size > 0 {
		for _, name := range s.running() {
			s.floods = append(s.floods, s.e.Flood(name, size))
		}
	}
	w.WriteHeader(http.StatusNoContent)
}
