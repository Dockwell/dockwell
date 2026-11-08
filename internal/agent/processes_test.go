// SPDX-License-Identifier: AGPL-3.0-only

package agent

import (
	"bufio"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strconv"
	"strings"
	"syscall"
	"testing"
	"time"

	"golang.org/x/sys/unix"

	"github.com/dockwell/dockwell/internal/agent/collect"
	"github.com/dockwell/dockwell/internal/docker"
	"github.com/dockwell/dockwell/internal/docker/dockertest"
	"github.com/dockwell/dockwell/internal/resource"
)

// A process for these tests to end: this test binary again, asleep. It prints
// the ID of a thread of its own that lives as long as it does, which isn't its
// main one: Go's runtime starts and stops its other threads as it likes, so one
// read from /proc/<pid>/task may be gone by the time a test opens it.
func TestHelperProcess(t *testing.T) {
	if os.Getenv("DOCKWELL_PROCESS_HELPER") != "1" {
		t.Skip("only run as a helper")
	}
	// A goroutine locked to its thread keeps it; one that landed on the main
	// thread keeps that, so the next lands on another.
	var keep func()
	keep = func() {
		runtime.LockOSThread()
		if tid := syscall.Gettid(); tid != syscall.Getpid() {
			fmt.Println(tid)
		} else {
			go keep()
		}
		select {}
	}
	go keep()
	time.Sleep(time.Minute)
	os.Exit(0)
}

// startHelper starts TestHelperProcess, and returns it, what its Wait says when
// it ends, and the thread it keeps.
func startHelper(t *testing.T) (*exec.Cmd, <-chan error, int) {
	t.Helper()
	r, w, err := os.Pipe()
	if err != nil {
		t.Fatal(err)
	}
	cmd := exec.Command(os.Args[0], "-test.run=^TestHelperProcess$")
	cmd.Env = append(os.Environ(), "DOCKWELL_PROCESS_HELPER=1")
	cmd.Stdout = w
	if err := cmd.Start(); err != nil {
		t.Fatal(err)
	}
	_ = w.Close()
	t.Cleanup(func() { _ = cmd.Process.Kill() })
	line, err := bufio.NewReader(r).ReadString('\n')
	_ = r.Close()
	tid, convErr := strconv.Atoi(strings.TrimSpace(line))
	if err != nil || convErr != nil {
		t.Fatalf("the helper said no thread: %q, %v", line, err)
	}
	done := make(chan error, 1)
	go func() { done <- cmd.Wait() }()
	return cmd, done, tid
}

func startOf(t *testing.T, c *collect.Collector, pid int) uint64 {
	t.Helper()
	for range 50 {
		if start, ok := c.ProcessStart(uint32(pid)); ok {
			return start
		}
		time.Sleep(20 * time.Millisecond)
	}
	t.Fatalf("PID %d never showed in /proc", pid)
	return 0
}

func running(done <-chan error) bool {
	select {
	case err := <-done:
		_ = err // it ended, however it did
		return false
	case <-time.After(100 * time.Millisecond):
		return true
	}
}

func reasonOf(err error) string { return resource.ReasonOf(err) }

// The process named is ended, through a pidfd.
func TestEndEndsTheProcessNamed(t *testing.T) {
	c := collect.New()
	p := newProcesses(c, nil)
	cmd, done, _ := startHelper(t)
	_, killed, err := p.end(t.Context(), uint32(cmd.Process.Pid), startOf(t, c, cmd.Process.Pid))
	if err != nil || killed {
		t.Fatalf("end = %v, killed %v", err, killed)
	}
	if running(done) {
		t.Error("the process is still running")
	}
}

// A thread has a PID, and no pidfd of its own: it's refused, saying the process
// it's of, rather than signalled as the whole process (#218's review, M1).
func TestEndingAThreadIsRefused(t *testing.T) {
	c := collect.New()
	p := newProcesses(c, nil)
	cmd, done, tid := startHelper(t)
	startOf(t, c, cmd.Process.Pid)
	_, _, err := p.end(t.Context(), uint32(tid), startOf(t, c, tid))
	if reasonOf(err) != collect.ReasonProcessProtected || !strings.Contains(err.Error(), fmt.Sprintf("thread of process %d", cmd.Process.Pid)) {
		t.Errorf("ending a thread: %v, want it refused as a thread of %d", err, cmd.Process.Pid)
	}
	if !running(done) {
		t.Error("ending a thread ended its process")
	}
}

// Kernels can refuse a thread's pidfd with different errors. Its /proc entry
// still identifies the process it belongs to, and no signal may be sent.
func TestAThreadIsRefusedWhenItsPidfdCannotOpen(t *testing.T) {
	c := collect.New()
	cmd, done, tid := startHelper(t)
	start := startOf(t, c, tid)
	for _, openErr := range []error{unix.EINVAL, unix.ENOENT, unix.ESRCH} {
		t.Run(openErr.Error(), func(t *testing.T) {
			p := newProcesses(c, nil)
			p.pidfdOpen = func(pid, flags int) (int, error) {
				if pid != tid || flags != 0 {
					t.Fatalf("pidfd_open(%d, %d), want (%d, 0)", pid, flags, tid)
				}
				return -1, openErr
			}
			p.opened = func(uint32) { t.Fatal("a failed pidfd was treated as open") }
			p.signal = func(int, syscall.Signal) error { t.Fatal("it signalled a thread"); return nil }
			_, killed, err := p.end(t.Context(), uint32(tid), start)
			if reasonOf(err) != collect.ReasonProcessProtected || !strings.Contains(err.Error(), fmt.Sprintf("thread of process %d", cmd.Process.Pid)) {
				t.Fatalf("ending a thread: %v, want it refused as a thread of %d", err, cmd.Process.Pid)
			}
			if killed {
				t.Fatal("it reports killing a thread")
			}
		})
	}
	if !running(done) {
		t.Error("ending a thread ended its process")
	}
}

// A failed pidfd for a process that has actually gone remains a gone result.
// Inspecting /proc after the failure is only to explain a refusal, never to signal.
func TestAProcessThatGoesBeforeItsPidfdOpens(t *testing.T) {
	for _, openErr := range []error{unix.ENOENT, unix.ESRCH} {
		t.Run(openErr.Error(), func(t *testing.T) {
			c := collect.New()
			p := newProcesses(c, nil)
			cmd, done, _ := startHelper(t)
			start := startOf(t, c, cmd.Process.Pid)
			p.pidfdOpen = func(int, int) (int, error) {
				if err := cmd.Process.Kill(); err != nil {
					t.Fatal(err)
				}
				if err := <-done; err == nil {
					t.Fatal("the helper ended on its own")
				}
				return -1, openErr
			}
			p.opened = func(uint32) { t.Fatal("a failed pidfd was treated as open") }
			p.signal = func(int, syscall.Signal) error { t.Fatal("it signalled after a failed open"); return nil }
			_, killed, err := p.end(t.Context(), uint32(cmd.Process.Pid), start)
			if reasonOf(err) != collect.ReasonProcessGone || killed {
				t.Fatalf("a process that went away: %v, killed %v, want gone", err, killed)
			}
		})
	}
}

// A kernel with no pidfds can't end a process safely, as a signal by PID alone
// could reach whatever took the PID: it's refused, saying how to over SSH, and
// nothing is signalled (#218's review, M1).
func TestWithoutPidfdsNothingIsSignalled(t *testing.T) {
	c := collect.New()
	p := newProcesses(c, nil)
	p.pidfdOpen = func(int, int) (int, error) { return -1, unix.ENOSYS }
	cmd, done, _ := startHelper(t)
	_, _, err := p.end(t.Context(), uint32(cmd.Process.Pid), startOf(t, c, cmd.Process.Pid))
	if reasonOf(err) != collect.ReasonProcessProtected || !strings.Contains(err.Error(), fmt.Sprintf("sudo kill %d", cmd.Process.Pid)) {
		t.Errorf("without pidfds: %v, want it refused with how to end it over SSH", err)
	}
	if !running(done) {
		t.Error("without pidfds, the process was ended anyway")
	}
}

// What the pidfd names is what's checked: a process that goes once the pidfd is
// open, as one whose PID then went to another would, is gone, and nothing is
// signalled (#218's review, L6).
func TestAProcessThatGoesOnceItsOpenIsntSignalled(t *testing.T) {
	c := collect.New()
	p := newProcesses(c, nil)
	cmd, done, _ := startHelper(t)
	start := startOf(t, c, cmd.Process.Pid)
	p.opened = func(uint32) {
		_ = cmd.Process.Kill()
		if err := <-done; err == nil {
			t.Error("the helper ended on its own")
		}
	}
	var signalled []syscall.Signal
	p.signal = func(_ int, sig syscall.Signal) error {
		signalled = append(signalled, sig)
		return nil
	}
	_, _, err := p.end(t.Context(), uint32(cmd.Process.Pid), start)
	if reasonOf(err) != collect.ReasonProcessGone {
		t.Errorf("a process that went once its pidfd was open: %v, want it gone", err)
	}
	if len(signalled) > 0 {
		t.Errorf("it signalled %v", signalled)
	}
	// And a process that started at another time isn't the one named.
	cmd, done, _ = startHelper(t)
	if _, _, err := newProcesses(c, nil).end(t.Context(), uint32(cmd.Process.Pid), startOf(t, c, cmd.Process.Pid)+1); reasonOf(err) != collect.ReasonProcessGone {
		t.Errorf("a process that started at another time: %v, want it gone", err)
	}
	if !running(done) {
		t.Error("a process that started at another time was ended")
	}
}

// A thread the kernel runs is refused, not only left off the list (#218's review, M5).
func TestAKernelThreadIsRefused(t *testing.T) {
	stat, err := os.ReadFile("/proc/2/stat")
	if err != nil || !strings.Contains(string(stat), "(kthreadd)") {
		t.Skip("this machine shows no kernel threads, as in a container")
	}
	c := collect.New()
	_, _, err = newProcesses(c, nil).end(t.Context(), 2, startOf(t, c, 2))
	if reasonOf(err) != collect.ReasonProcessProtected || !strings.Contains(err.Error(), "kernel") {
		t.Errorf("ending kthreadd: %v, want it refused", err)
	}
}

// A PID past what Linux gives, which a signal would read as every process, is
// refused before anything is opened.
func TestAPIDPastPidMaxIsRefused(t *testing.T) {
	p := newProcesses(collect.New(), nil)
	p.pidfdOpen = func(int, int) (int, error) { t.Fatal("it opened a pidfd"); return -1, nil }
	for _, pid := range []uint32{0, maxPID + 1, 1<<32 - 1} {
		if _, _, err := p.end(t.Context(), pid, 1); err == nil {
			t.Errorf("ending PID %d went ahead", pid)
		}
	}
}

// fakeProcess makes what containerUsers reads through: a /proc directory whose
// root is a directory of the test's, with what passwd says in its /etc.
func fakeProcess(t *testing.T, passwd func(path string)) int {
	t.Helper()
	dir := t.TempDir()
	root := filepath.Join(dir, "rootfs")
	if err := os.MkdirAll(filepath.Join(root, "etc"), 0o755); err != nil {
		t.Fatal(err)
	}
	proc := filepath.Join(dir, "proc")
	if err := os.Mkdir(proc, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(root, filepath.Join(proc, "root")); err != nil {
		t.Fatal(err)
	}
	passwd(filepath.Join(root, "etc", "passwd"))
	fd, err := unix.Open(proc, unix.O_PATH|unix.O_DIRECTORY|unix.O_CLOEXEC, 0)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = unix.Close(fd) })
	return fd
}

func readUsersWithin(t *testing.T, dir int) map[uint32]string {
	t.Helper()
	got := make(chan map[uint32]string, 1)
	go func() { got <- containerUsers(dir) }()
	select {
	case users := <-got:
		return users
	case <-time.After(5 * time.Second):
		t.Fatal("reading a container's users held the agent up")
		return nil
	}
}

// A container's /etc/passwd is whatever its image holds. One that's a FIFO isn't
// opened, as that would hold the agent up; one that links out of the container
// reads inside it; and one that's huge is read only so far, its names cut short
// (#218's review, M2 and M3).
func TestAContainersUsersAreReadSafely(t *testing.T) {
	fifo := fakeProcess(t, func(path string) {
		if err := unix.Mkfifo(path, 0o644); err != nil {
			t.Fatal(err)
		}
	})
	if users := readUsersWithin(t, fifo); len(users) != 0 {
		t.Errorf("a FIFO named users %v", users)
	}

	out := fakeProcess(t, func(path string) {
		if err := os.Symlink("/etc/passwd", path); err != nil {
			t.Fatal(err)
		}
	})
	if users := readUsersWithin(t, out); len(users) != 0 {
		t.Errorf("a link to the server's /etc/passwd read it: %v", users)
	}

	huge := fakeProcess(t, func(path string) {
		body := "web:x:1000:1000::/:/bin/sh\n" + strings.Repeat("a", 64<<10) + ":x:1001:1001::/:/bin/sh\n"
		body += strings.Repeat("filler:x:9:9::/:/bin/sh\n", (2<<20)/24) + "late:x:1003:1003::/:/bin/sh\n"
		if err := os.WriteFile(path, []byte(body), 0o644); err != nil {
			t.Fatal(err)
		}
	})
	users := readUsersWithin(t, huge)
	if users[1000] != "web" || len(users[1001]) != 32 || users[1003] != "" {
		t.Errorf("a huge /etc/passwd: %q, a name of %d bytes, and %q from past its first 256 KB", users[1000], len(users[1001]), users[1003])
	}
}

// A container says its restart policy, as only some start their process again,
// and Docker is asked it once a container (#218's review, M4).
func TestContainersSayTheirRestartPolicy(t *testing.T) {
	e := dockertest.New(t)
	always := e.AddContainer(docker.Container{ID: "a1", Name: "/web", HostConfig: docker.HostConfig{RestartPolicy: docker.RestartPolicy{Name: "always"}}})
	none := e.AddContainer(docker.Container{ID: "b2", Name: "/job"})
	p := newProcesses(collect.New(), e.Client())
	now := time.Now()
	names := p.containerNames(t.Context(), now)
	if names[always.ID].GetRestartPolicy() != "always" || names[none.ID].GetRestartPolicy() != "no" {
		t.Errorf("restart policies: %q and %q, want always and no", names[always.ID].GetRestartPolicy(), names[none.ID].GetRestartPolicy())
	}
	inspected := func() int {
		n := 0
		for _, r := range e.Seen() {
			if strings.HasSuffix(r, "/json") && strings.Contains(r, "/containers/") && !strings.HasSuffix(r, "/containers/json") {
				n++
			}
		}
		return n
	}
	before := inspected()
	// Once the names are old, they're read again, and the policies aren't.
	names = p.containerNames(t.Context(), now.Add(containerNamesFor))
	if inspected() != before || names[always.ID].GetRestartPolicy() != "always" {
		t.Errorf("reading the names again inspected %d more, and says %q", inspected()-before, names[always.ID].GetRestartPolicy())
	}
}
