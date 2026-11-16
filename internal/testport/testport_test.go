// SPDX-License-Identifier: AGPL-3.0-only

package testport

import (
	"bufio"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"slices"
	"strconv"
	"strings"
	"testing"
)

// A port is given out once, below where the kernel hands ports out on its own, as
// nothing holds one between Free and what binds it. Without the memory, a repeat is
// all but certain well before this many.
func TestAPortIsGivenOutOnceAndIsntTheKernels(t *testing.T) {
	top := ephemeralStart()
	seen := map[int]bool{}
	for range 500 {
		port := Free(t)
		if seen[port] {
			t.Fatalf("port %d was given out twice", port)
		}
		if port < below || port >= top {
			t.Fatalf("port %d isn't between %d and the kernel's %d", port, below, top)
		}
		seen[port] = true
	}
}

// A listener's address can be bound again once it's closed.
func TestAListenersAddressCanBeBoundAgain(t *testing.T) {
	l := Listen(t, "127.0.0.1")
	addr := l.Addr().String()
	if err := l.Close(); err != nil {
		t.Fatal(err)
	}
	again := Listen(t, "127.0.0.1")
	defer again.Close()
	if again.Addr().String() == addr {
		t.Errorf("Listen gave %s out twice", addr)
	}
}

// holder is the environment variable that makes this test binary, run again as a
// child, claim the port it names, say so, and hold it until its input closes.
const holder = "DOCKWELL_TESTPORT_HOLD"

// A port another test process holds isn't given out here, as go test runs packages
// as processes at once: one process's port, given out to bind later, was taken by
// another's listener, then bound by the first as the second let it go (#731). It's
// free again once that process ends, however it ends.
func TestAPortAnotherProcessHoldsIsntGivenOut(t *testing.T) {
	if port := os.Getenv(holder); port != "" {
		n, err := strconv.Atoi(port)
		if err != nil {
			t.Fatal(err)
		}
		mu.Lock()
		ok := claim(n)
		mu.Unlock()
		fmt.Println("held", ok)
		_, _ = io.Copy(io.Discard, os.Stdin)
		return
	}
	port := Free(t)
	// Let this process's claim go, as a process that ended would, and have a child
	// take the port.
	mu.Lock()
	for i, f := range claims {
		if f.Name() == filepath.Join(claimDir(), strconv.Itoa(port)) {
			_ = f.Close()
			claims = slices.Delete(claims, i, i+1)
			break
		}
	}
	mu.Unlock()
	child := exec.Command(os.Args[0], "-test.run=^TestAPortAnotherProcessHoldsIsntGivenOut$")
	child.Env = append(os.Environ(), holder+"="+strconv.Itoa(port))
	in, err := child.StdinPipe()
	if err != nil {
		t.Fatal(err)
	}
	out, err := child.StdoutPipe()
	if err != nil {
		t.Fatal(err)
	}
	if err := child.Start(); err != nil {
		t.Fatal(err)
	}
	said := bufio.NewScanner(out)
	held := false
	for said.Scan() {
		if strings.HasPrefix(said.Text(), "held ") {
			held = said.Text() == "held true"
			break
		}
	}
	if !held {
		t.Fatalf("the child couldn't claim port %d", port)
	}
	mu.Lock()
	taken := !claim(port)
	mu.Unlock()
	if !taken {
		t.Errorf("port %d was claimed here while another process held it", port)
	}
	_ = in.Close()
	_, _ = io.Copy(io.Discard, out)
	if err := child.Wait(); err != nil {
		t.Fatalf("the child: %v", err)
	}
	mu.Lock()
	free := claim(port)
	mu.Unlock()
	if !free {
		t.Errorf("port %d stayed claimed once the process that held it ended", port)
	}
}

// bindsALiteral finds a bind of a literal port: Listen, ListenPacket or a Dialer's
// local address on "host:port" or ":port", with the port written out.
var bindsALiteral = regexp.MustCompile(`(Listen|ListenPacket|ListenTCP|ListenUDP)\([^)]*"[^"]*:([0-9]{4,5})"`)

// No test binds a port of testport's range directly, outside it: the claims keep
// test processes from one another only where each port goes through here. A test
// that wants a port of its own takes one from Free, Addr or Listen, and one that
// binds port 0 gets one from the kernel's range, which this one isn't (#731).
func TestNoTestBindsAPortOfTheRangeDirectly(t *testing.T) {
	root, err := filepath.Abs("../..")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(filepath.Join(root, "go.mod")); err != nil {
		t.Fatalf("the module isn't at %s: %v", root, err)
	}
	top := ephemeralStart()
	err = filepath.WalkDir(root, func(path string, d os.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if d.IsDir() && (d.Name() == "node_modules" || d.Name() == ".git" || d.Name() == "web") {
			return filepath.SkipDir
		}
		if d.IsDir() || !strings.HasSuffix(path, ".go") {
			return nil
		}
		b, err := os.ReadFile(path)
		if err != nil {
			return err
		}
		for _, m := range bindsALiteral.FindAllStringSubmatch(string(b), -1) {
			if port, err := strconv.Atoi(m[2]); err == nil && port >= below && port < top {
				rel, _ := filepath.Rel(root, path)
				t.Errorf("%s binds port %d, in the range testport gives out, so another test process may be given it: take one from testport", rel, port)
			}
		}
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
}
