// SPDX-License-Identifier: AGPL-3.0-only

package testport

import (
	"bufio"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
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
