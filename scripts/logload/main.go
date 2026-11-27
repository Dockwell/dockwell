// SPDX-License-Identifier: AGPL-3.0-only

// Command logload serves the fake Docker Engine the tests use on a unix socket,
// with a small HTTP interface to make its containers write, for measuring logs
// end to end on a machine without Docker: point dockwell at the socket with
// DOCKER_HOST=unix://<socket>. web/e2e/logs.ts drives it. It is never part of a
// release. CI's jobs on Docker read real containers' logs.
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
	"fmt"
	"log"
	"os"
	"os/signal"
	"path/filepath"
	"strings"
	"syscall"
	"testing"

	"github.com/dockwell/dockwell/internal/docker"
	"github.com/dockwell/dockwell/internal/docker/dockertest"
	"github.com/dockwell/dockwell/scripts/logfixture"
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
	listener, err := logfixture.Listen(e, os.Args[2])
	if err != nil {
		log.Fatal(err)
	}
	fmt.Fprintln(os.Stderr, "logload: listening on", listener.Addr())
	defer listener.Close()
	fmt.Println(e.Socket)
	stop := make(chan os.Signal, 1)
	signal.Notify(stop, os.Interrupt, syscall.SIGTERM)
	<-stop
	for _, f := range t.cleanup {
		f()
	}
}
