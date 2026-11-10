// SPDX-License-Identifier: AGPL-3.0-only

package docker_test

import (
	"io"
	"strings"
	"testing"

	"github.com/dockwell/dockwell/internal/docker"
	"github.com/dockwell/dockwell/internal/docker/dockertest"
)

// sent says when all of a reader has been read, so by a process's stdin: by then
// its last write is on the connection.
type sent struct {
	r    io.Reader
	done chan struct{}
}

func (s *sent) Read(p []byte) (int, error) {
	n, err := s.r.Read(p)
	if err == io.EOF {
		select {
		case <-s.done:
		default:
			close(s.done)
		}
	}
	return n, err
}

// A process can end without reading all it was given, as psql does at a
// restore's first error. The engine then closes the connection with some of it
// still unread there, and the kernel says that to the next read here as a reset,
// in place of the end, once all the engine sent has been read. How the process
// went is its exit code's to say, not a broken stream's.
func TestAProcessThatLeavesItsStdinUnreadSaysOnlyHowItExited(t *testing.T) {
	stdin := &sent{r: strings.NewReader("CREATE TABLE orders ();\n"), done: make(chan struct{})}
	said := "psql:<stdin>:1: ERROR:  relation \"orders\" already exists\n"
	e := dockertest.New(t)
	e.AddLocalImage("postgres:17", dockertest.Image{Config: docker.Config{Cmd: []string{"postgres"}}, Exec: func(x dockertest.Exec) int {
		<-stdin.done // all of it is on the connection, and none of it read
		_, _ = io.WriteString(x.Stderr, said)
		return 3
	}})
	client := e.Client()
	id, err := client.CreateContainer(t.Context(), "c-postgres", &docker.CreateRequest{Config: docker.Config{Image: "postgres:17"}})
	if err != nil {
		t.Fatal(err)
	}
	if err := client.StartContainer(t.Context(), id); err != nil {
		t.Fatal(err)
	}
	var stderr strings.Builder
	code, err := client.Exec(t.Context(), id, []string{"psql"}, docker.Streams{Stdin: stdin, Stderr: &stderr})
	if err != nil {
		t.Fatalf("a process that left its stdin unread says %v, want only its exit code", err)
	}
	if code != 3 || stderr.String() != said {
		t.Errorf("got exit code %d and %q, want 3 and %q", code, stderr.String(), said)
	}
}
