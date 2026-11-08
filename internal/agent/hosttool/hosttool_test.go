// SPDX-License-Identifier: AGPL-3.0-only

package hosttool

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"syscall"
	"testing"
	"time"
)

// tool writes a stand-in at /usr/sbin/<name> under a new root, and returns a
// runner on that root and the tool.
func tool(t *testing.T, name, script string) (*Runner, Tool) {
	t.Helper()
	root := t.TempDir()
	dir := filepath.Join(root, "usr", "sbin")
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, name), []byte("#!/bin/sh\n"+script), 0o755); err != nil {
		t.Fatal(err)
	}
	return &Runner{Root: root}, Tool{Name: name, Paths: []string{"/usr/bin/" + name, "/usr/sbin/" + name}}
}

// A tool gets the clean environment and what it asks for, nothing of the
// agent's own, runs from /, and reads /dev/null unless it's given input.
func TestAToolGetsACleanEnvironment(t *testing.T) {
	t.Setenv("DOCKWELL_SECRET", "leaked")
	r, tl := tool(t, "envy", `env | sort; pwd; cat`)
	tl.Env = []string{"DEBIAN_FRONTEND=noninteractive"}
	res, err := r.Run(t.Context(), tl, 5*time.Second, nil)
	if err != nil || res.Code != 0 {
		t.Fatalf("%v, %+v", err, res)
	}
	got := strings.Fields(string(res.Out))
	want := []string{"DEBIAN_FRONTEND=noninteractive", "HOME=/root", "LANG=C", "LC_ALL=C", "PATH=/usr/sbin:/usr/bin:/sbin:/bin", "/"}
	// The shell may add PWD, SHLVL and _ of its own.
	var kept []string
	for _, line := range got {
		if !strings.HasPrefix(line, "PWD=") && !strings.HasPrefix(line, "SHLVL=") && !strings.HasPrefix(line, "_=") && !strings.HasPrefix(line, "OLDPWD=") {
			kept = append(kept, line)
		}
	}
	if strings.Join(kept, " ") != strings.Join(want, " ") {
		t.Errorf("environment %q, want %q", kept, want)
	}
	res, err = r.Run(t.Context(), tl, 5*time.Second, []byte("table inet dockwell {}\n"))
	if err != nil || !strings.HasSuffix(string(res.Out), "table inet dockwell {}\n") {
		t.Errorf("with input: %q, %v", res.Out, err)
	}
}

// A tool past its deadline is stopped with everything it started, and says so:
// what it started gets SIGTERM with it, to end as it would, and goes for good
// a grace later.
func TestAToolPastItsDeadlineGoesWithWhatItStarted(t *testing.T) {
	dir := t.TempDir()
	pidFile, termFile := filepath.Join(dir, "child"), filepath.Join(dir, "term")
	child := "trap 'echo term > " + termFile + "; exit 0' TERM; sleep 60 & wait"
	r, tl := tool(t, "hang", "sh -c \""+child+"\" & echo $! > "+pidFile+"\necho waiting >&2\nsleep 60\n")
	r.Grace = 200 * time.Millisecond
	start := time.Now()
	_, err := r.Run(t.Context(), tl, 300*time.Millisecond, nil)
	if !errors.Is(err, context.DeadlineExceeded) || !strings.Contains(err.Error(), "after saying: waiting") {
		t.Fatalf("a hung tool says %v", err)
	}
	if took := time.Since(start); took > 5*time.Second {
		t.Errorf("stopping it took %s", took)
	}
	b, err := os.ReadFile(pidFile)
	if err != nil {
		t.Fatal(err)
	}
	pid, _ := strconv.Atoi(strings.TrimSpace(string(b)))
	deadline := time.Now().Add(5 * time.Second)
	for syscall.Kill(pid, 0) == nil {
		if time.Now().After(deadline) {
			t.Fatalf("what the tool started, pid %d, outlived it", pid)
		}
		time.Sleep(20 * time.Millisecond)
	}
	if said, err := os.ReadFile(termFile); err != nil || strings.TrimSpace(string(said)) != "term" {
		t.Errorf("what the tool started got no SIGTERM: %q, %v", said, err)
	}
}

// What a tool started that ignores SIGTERM goes too, a grace later.
func TestWhatIgnoresSIGTERMGoesAGraceLater(t *testing.T) {
	pidFile := filepath.Join(t.TempDir(), "child")
	r, tl := tool(t, "stubborn", "sh -c \"trap '' TERM; sleep 60\" & echo $! > "+pidFile+"\ntrap '' TERM\nsleep 60\n")
	r.Grace = 200 * time.Millisecond
	if _, err := r.Run(t.Context(), tl, 300*time.Millisecond, nil); !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("a stubborn tool says %v", err)
	}
	b, err := os.ReadFile(pidFile)
	if err != nil {
		t.Fatal(err)
	}
	pid, _ := strconv.Atoi(strings.TrimSpace(string(b)))
	deadline := time.Now().Add(5 * time.Second)
	for syscall.Kill(pid, 0) == nil {
		if time.Now().After(deadline) {
			t.Fatalf("what the tool started, pid %d, ignored SIGTERM and outlived it", pid)
		}
		time.Sleep(20 * time.Millisecond)
	}
}

// A tool that prints more than a run reads fails, rather than filling memory.
func TestAToolThatPrintsTooMuchFails(t *testing.T) {
	r, tl := tool(t, "chatty", "head -c 5000000 /dev/zero\n")
	if _, err := r.Run(t.Context(), tl, 10*time.Second, nil); !errors.Is(err, ErrTooMuch) {
		t.Errorf("5 MB of output: %v", err)
	}
}

// Tools run one at a time, and a caller that leaves while it waits its turn
// gives up without running.
func TestToolsRunOneAtATime(t *testing.T) {
	log := filepath.Join(t.TempDir(), "log")
	r, tl := tool(t, "slow", "echo start >> "+log+"\nsleep 0.2\necho end >> "+log+"\n")
	var wg sync.WaitGroup
	for range 3 {
		wg.Go(func() {
			if _, err := r.Run(context.Background(), tl, 5*time.Second, nil); err != nil {
				t.Error(err)
			}
		})
	}
	wg.Wait()
	b, _ := os.ReadFile(log)
	if got := strings.Fields(string(b)); strings.Join(got, " ") != "start end start end start end" {
		t.Errorf("runs overlapped: %v", got)
	}
	release := make(chan struct{})
	held := make(chan struct{})
	go func() {
		done, _ := r.wait(context.Background())
		close(held)
		<-release
		done()
	}()
	<-held
	ctx, cancel := context.WithTimeout(context.Background(), 50*time.Millisecond)
	defer cancel()
	if _, err := r.Run(ctx, tl, 5*time.Second, nil); !errors.Is(err, context.DeadlineExceeded) {
		t.Errorf("a caller that left while waiting: %v", err)
	}
	close(release)
}

// A tool runs from one of its fixed paths, the first that's there, and never
// from PATH; one at none of them, or not executable, isn't installed.
func TestAToolRunsOnlyFromItsPaths(t *testing.T) {
	r, tl := tool(t, "nft", "echo sbin\n")
	if p, ok := r.Find(tl); !ok || p != "/usr/sbin/nft" {
		t.Errorf("found %q, %v", p, ok)
	}
	if err := os.Chmod(filepath.Join(r.Root, "usr", "sbin", "nft"), 0o644); err != nil {
		t.Fatal(err)
	}
	if _, err := r.Run(t.Context(), tl, time.Second, nil); !errors.Is(err, ErrMissing) {
		t.Errorf("a tool that can't run: %v", err)
	}
	t.Setenv("PATH", filepath.Join(r.Root, "usr", "sbin"))
	if _, err := r.Run(t.Context(), Tool{Name: "nft", Paths: []string{"/nowhere/nft"}}, time.Second, nil); !errors.Is(err, ErrMissing) {
		t.Errorf("a tool only on PATH: %v", err)
	}
	if _, err := (&Runner{Off: true}).Run(t.Context(), tl, time.Second, nil); !errors.Is(err, ErrOff) {
		t.Errorf("a runner in a test: %v", err)
	}
}

// A tool that fails says its status and the last of what it said.
func TestFailSaysWhatTheToolSaid(t *testing.T) {
	r, tl := tool(t, "sad", "echo one >&2\necho two >&2\nexit 3\n")
	res, err := r.Run(t.Context(), tl, time.Second, nil)
	if err != nil {
		t.Fatal(err)
	}
	if got := Fail("sad", res).Error(); got != "sad ended with status 3: one; two" {
		t.Errorf("fail says %q", got)
	}
}
