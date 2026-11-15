// SPDX-License-Identifier: AGPL-3.0-only

package testport

import (
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
