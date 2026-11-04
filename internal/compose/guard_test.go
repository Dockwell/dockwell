// SPDX-License-Identifier: AGPL-3.0-only

package compose

import (
	"context"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"
)

// child builds what the control plane runs as a child, once for all the tests.
var child = sync.OnceValues(func() (string, error) {
	dir, err := os.MkdirTemp("", "dockwell-guardchild")
	if err != nil {
		return "", err
	}
	path := filepath.Join(dir, "guardchild")
	cmd := exec.Command("go", "build", "-o", path, "./testdata/guardchild")
	cmd.Env = append(os.Environ(), "CGO_ENABLED=0")
	if out, err := cmd.CombinedOutput(); err != nil {
		return "", fmt.Errorf("can't build the child: %w: %s", err, out)
	}
	return path, nil
})

func TestMain(m *testing.M) {
	code := m.Run()
	if path, err := child(); err == nil {
		_ = os.RemoveAll(filepath.Dir(path))
	}
	os.Exit(code)
}

func guarded(t *testing.T) {
	t.Helper()
	path, err := child()
	if err != nil {
		t.Fatal(err)
	}
	cpu, memory, timeout := guardCPU, guardMemory, guardTimeout
	guardPath = path
	t.Cleanup(func() { guardPath, guardCPU, guardMemory, guardTimeout = "", cpu, memory, timeout })
}

// One long word goes through a quadratic splitter: 2 MiB of it took 394 s.
func costly() ([]byte, map[string]string) {
	return []byte("services:\n  web:\n    image: nginx:1\n    command: ${A}${A}\n"), map[string]string{"A": strings.Repeat("x", 1<<20-1)}
}

// A child that reads a file changes nothing about the answer.
func TestAGuardedFileReadsTheSame(t *testing.T) {
	guarded(t)
	s, err := Parse(t.Context(), []byte("services:\n  web:\n    image: nginx:1.27\n"), nil)
	if err != nil {
		t.Fatal(err)
	}
	if len(s.Services) != 1 || s.Services[0].Name != "web" {
		t.Errorf("services = %v, want web", s.Services)
	}
	_, err = Parse(t.Context(), []byte("services:\n  web:\n    build: .\n"), nil)
	if !errors.Is(err, ErrInvalid) || !strings.Contains(err.Error(), "build") {
		t.Errorf("a refused file = %v, want the file's own refusal", err)
	}
}

// What a file costs the child can't cost the control plane. Each limit has to
// be what stopped it, as a child that dies at once reads the same to Parse.
func TestAGuardStopsAFileThatCostsTooMuch(t *testing.T) {
	file, vars := costly()
	for name, c := range map[string]struct {
		limit         func()
		atLeast, most time.Duration
	}{
		"CPU": {func() { guardCPU = 1 }, 300 * time.Millisecond, 8 * time.Second},
		// With the other limits out of the way, so this is what stops it.
		"memory": {func() { guardMemory, guardCPU, guardTimeout = 128<<20, 600, 10*time.Minute }, 0, 60 * time.Second},
		"time":   {func() { guardTimeout = 500 * time.Millisecond }, 400 * time.Millisecond, 10 * time.Second},
	} {
		t.Run(name, func(t *testing.T) {
			guarded(t)
			c.limit()
			// A file that costs little passes under the same limit, so the
			// limit isn't just stopping the child from starting.
			if err := guard(t.Context(), []byte("services:\n  web:\n    image: nginx:1.27\n"), nil); err != nil {
				t.Fatalf("a small file under this limit: %v", err)
			}
			start := time.Now()
			err := guard(t.Context(), file, vars)
			if !errors.Is(err, ErrInvalid) || !strings.Contains(err.Error(), "more time or memory") {
				t.Errorf("guard = %.300v, want it refused for what it costs", err)
			}
			if took := time.Since(start); took < c.atLeast || took > c.most {
				t.Errorf("took %v, want %v to %v, so something else stopped it", took, c.atLeast, c.most)
			}
		})
	}
}

// The limits are for files made to cost more. The largest real shapes pass.
func TestAGuardTakesTheLargestRealFiles(t *testing.T) {
	guarded(t)
	build := func(head string, each func(i int) string) string {
		var b strings.Builder
		b.WriteString(head)
		for i := 0; b.Len() < MaxFile-64; i++ {
			b.WriteString(each(i))
		}
		return b.String()
	}
	for name, file := range map[string]string{
		"values": build("services:\n  web:\n    image: nginx:1\n    command: [a", func(int) string { return ",a" }) + "]\n",
		"names": build("services:\n  web:\n    image: nginx:1\n    environment:\n",
			func(i int) string { return "      K" + strconv.Itoa(i) + ": v\n" }),
		"services": build("services:\n",
			func(i int) string { return "  s" + strconv.Itoa(i) + ": {image: 'nginx:1'}\n" }),
	} {
		t.Run(name, func(t *testing.T) {
			if err := guard(t.Context(), []byte(file), nil); err != nil {
				t.Errorf("a file of %d KiB: %.300v", len(file)>>10, err)
			}
		})
	}
}

// Someone who gives up stops the work, and their turn is free for others.
func TestLeavingStopsTheChild(t *testing.T) {
	guarded(t)
	file, vars := costly()
	ctx, cancel := context.WithCancel(t.Context())
	time.AfterFunc(200*time.Millisecond, cancel)
	start := time.Now()
	_, err := Parse(ctx, file, vars)
	if !errors.Is(err, context.Canceled) || errors.Is(err, ErrInvalid) {
		t.Errorf("Parse = %v, want it stopped, not refused", err)
	}
	if took := time.Since(start); took > 5*time.Second {
		t.Errorf("took %v to stop", took)
	}
	if len(parsing) != 0 {
		t.Errorf("%d turns still held", len(parsing))
	}
}

// A child that can't do its job says nothing about the file, so it doesn't
// blame it, and nothing is read in this process either.
func TestAChildThatFailsStopsTheRead(t *testing.T) {
	for name, path := range map[string]string{"can't start": "/nonexistent/dockwell", "fails": "/bin/false"} {
		t.Run(name, func(t *testing.T) {
			guarded(t)
			guardPath = path
			_, err := Parse(t.Context(), []byte("services:\n  web:\n    image: nginx:1.27\n"), nil)
			if err == nil || errors.Is(err, ErrInvalid) {
				t.Errorf("Parse = %v, want an error that isn't the file's fault", err)
			}
		})
	}
}
