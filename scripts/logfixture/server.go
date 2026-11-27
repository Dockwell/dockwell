// SPDX-License-Identifier: AGPL-3.0-only

// Package logfixture feeds logs to the fake Docker Engine used in load tests.
package logfixture

import (
	"bufio"
	"context"
	"errors"
	"fmt"
	"log"
	"net"
	"net/http"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/dockwell/dockwell/internal/docker"
	"github.com/dockwell/dockwell/internal/docker/dockertest"
)

// Listen exposes controls for a fake engine, returning the listening address.
func Listen(e *dockertest.Engine, addr string) (net.Listener, error) {
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
	server := &http.Server{Handler: mux, ReadHeaderTimeout: 5 * time.Second}
	listener, err := (&net.ListenConfig{}).Listen(context.Background(), "tcp", addr)
	if err != nil {
		return nil, err
	}
	go func() {
		if err := server.Serve(listener); err != nil && !errors.Is(err, net.ErrClosed) {
			log.Printf("log fixture: %v", err)
		}
	}()
	return listener, nil
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
