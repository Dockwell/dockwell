// SPDX-License-Identifier: AGPL-3.0-only

// Command fakedocker serves the fake Docker Engine the tests use on a unix
// socket, for the panel's end-to-end test on a machine without Docker: point
// dockwell at it with DOCKER_HOST=unix://<socket>. It pulls any image. An
// image whose tag is "broken" exits as soon as it starts, for a deploy that
// fails, and one with a name it can't find, "doesnotexist", can't be pulled.
// It is never part of a release. CI's Docker job runs the same flows on real
// Docker.
//
//	go run ./scripts/fakedocker /tmp/fake/docker.sock
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
)

// tb is what dockertest asks of a test: a place for the socket, and what to
// do when the process ends.
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
	if len(os.Args) != 2 {
		log.Fatal("fakedocker takes the directory to put its socket in, such as /tmp/fake")
	}
	dir, err := filepath.Abs(os.Args[1])
	if err != nil {
		log.Fatalf("can't read that directory: %v", err)
	}
	if err := os.MkdirAll(dir, 0o700); err != nil { //nolint:gosec // the directory the person running it names
		log.Fatalf("can't make that directory: %v", err)
	}
	t := &tb{dir: dir}
	e := dockertest.New(t)
	e.Find = func(ref string) (dockertest.Image, bool) {
		name, _, _ := strings.Cut(ref, ":")
		if strings.HasSuffix(name, "doesnotexist") {
			return dockertest.Image{}, false
		}
		return dockertest.Image{
			Exits: strings.HasSuffix(ref, ":broken"),
			Size:  48 << 20,
			Config: docker.Config{
				Cmd: []string{"serve"},
				Env: []string{"PATH=/usr/local/bin:/usr/bin:/bin"},
			},
		}, true
	}
	fmt.Println(e.Socket)
	stop := make(chan os.Signal, 1)
	signal.Notify(stop, os.Interrupt, syscall.SIGTERM)
	<-stop
	for _, f := range t.cleanup {
		f()
	}
}
