// SPDX-License-Identifier: AGPL-3.0-only

// Package testport gives tests ports that stay theirs: below the kernel's
// ephemeral range, so no outgoing connection and no listener on port 0 takes one,
// and never the same one twice in a process. A test that frees a port and binds it
// again, or hands one to something that binds it later, or needs one nothing
// answers on, takes it here: a port the kernel picked can be taken by anything else
// on a busy machine in between (#707).
package testport

import (
	"context"
	"math/rand/v2"
	"net"
	"os"
	"strconv"
	"strings"
	"sync"
	"testing"
)

// below is the lowest port the range starts at, so a port isn't a well-known
// service's.
const below = 10000

// given is what Free has given out in this process, as nothing may hold a port
// between Free and what binds it, so asking again would find it free.
var (
	mu    sync.Mutex
	given = map[int]bool{}
)

// ephemeralStart is where the kernel starts handing out ports on its own, from
// /proc, or Linux's default.
func ephemeralStart() int {
	if b, err := os.ReadFile("/proc/sys/net/ipv4/ip_local_port_range"); err == nil {
		if f := strings.Fields(string(b)); len(f) == 2 {
			if n, err := strconv.Atoi(f[0]); err == nil {
				return n
			}
		}
	}
	return 32768
}

// Free returns a port nothing holds, on TCP and UDP, on every address, that the
// kernel doesn't hand out on its own and that Free hasn't given out before.
func Free(t testing.TB) int {
	t.Helper()
	top := ephemeralStart()
	if top <= below+1000 {
		t.Fatalf("the kernel hands out ports from %d, which leaves tests none of their own; raise net.ipv4.ip_local_port_range's start above %d", top, below+1000)
	}
	mu.Lock()
	defer mu.Unlock()
	var lc net.ListenConfig
	for range 500 {
		port := below + rand.IntN(top-below) //nolint:gosec // a port to try, not a secret
		if given[port] {
			continue
		}
		l, err := lc.Listen(context.Background(), "tcp", ":"+strconv.Itoa(port))
		if err != nil {
			continue
		}
		_ = l.Close()
		u, err := lc.ListenPacket(context.Background(), "udp", ":"+strconv.Itoa(port))
		if err != nil {
			continue
		}
		_ = u.Close()
		given[port] = true
		return port
	}
	t.Fatal("no free port below the kernel's ephemeral range")
	return 0
}

// Addr is host and a Free port, as one address.
func Addr(t testing.TB, host string) string {
	t.Helper()
	return net.JoinHostPort(host, strconv.Itoa(Free(t)))
}

// Listen listens on host at a Free port, for a listener a test may close and bind
// again at the same address.
func Listen(t testing.TB, host string) net.Listener {
	t.Helper()
	var lc net.ListenConfig
	for range 20 {
		l, err := lc.Listen(context.Background(), "tcp", Addr(t, host))
		if err == nil {
			return l
		}
	}
	t.Fatalf("can't listen on %s at a port of a test's own", host)
	return nil
}
