// SPDX-License-Identifier: AGPL-3.0-only

package health

import (
	"context"
	"fmt"
	"net"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/dockwell/dockwell/internal/docker"
)

// serve answers on loopback, as a container would on its address.
func serve(t *testing.T, h http.HandlerFunc) (address string, port int) {
	t.Helper()
	s := httptest.NewServer(h)
	t.Cleanup(s.Close)
	host, p, err := net.SplitHostPort(s.Listener.Addr().String())
	if err != nil {
		t.Fatal(err)
	}
	port, _ = strconv.Atoi(p)
	return host, port
}

func TestAnHTTPCheckGoesByTheStatus(t *testing.T) {
	var host atomic.Value
	var status atomic.Int64
	address, port := serve(t, func(w http.ResponseWriter, r *http.Request) {
		host.Store(r.Host)
		w.Header().Set("Location", "http://169.254.169.254/latest/meta-data/")
		w.WriteHeader(int(status.Load()))
		_, _ = fmt.Fprint(w, "a body the agent never reads: secret=hunter2")
	})
	c := &Check{HTTP: &HTTP{Port: port, Path: "/healthz", Host: "app.example.com"}}
	for code, passes := range map[int]bool{200: true, 204: true, 301: true, 399: true, 400: false, 404: false, 500: false, 503: false} {
		status.Store(int64(code))
		r, err := Run(t.Context(), c, address)
		if err != nil {
			t.Fatal(err)
		}
		if r.Passed != passes {
			t.Errorf("a status of %d passed = %t", code, r.Passed)
		}
		if !strings.Contains(r.Output, fmt.Sprintf("GET /healthz on port %d answered %d", port, code)) {
			t.Errorf("a status of %d says %q", code, r.Output)
		}
		if strings.Contains(r.Output, "hunter2") {
			t.Errorf("a check passed on the body it was sent: %q", r.Output)
		}
		if r.At.IsZero() || r.Took <= 0 {
			t.Errorf("a result has no time: %+v", r)
		}
	}
	if host.Load() != "app.example.com" {
		t.Errorf("the check sent the host %v", host.Load())
	}
}

// A container saying where to go next could otherwise send the agent anywhere,
// as to a cloud's metadata address, with the answer on the timeline.
func TestAnHTTPCheckFollowsNoRedirect(t *testing.T) {
	var elsewhere atomic.Int64
	_, otherPort := serve(t, func(http.ResponseWriter, *http.Request) { elsewhere.Add(1) })
	address, port := serve(t, func(w http.ResponseWriter, r *http.Request) {
		http.Redirect(w, r, fmt.Sprintf("http://127.0.0.1:%d/secret", otherPort), http.StatusFound)
	})
	r, err := Run(t.Context(), &Check{HTTP: &HTTP{Port: port}}, address)
	if err != nil || !r.Passed {
		t.Fatalf("a 302 = %+v, %v, want it to pass as it stands", r, err)
	}
	if elsewhere.Load() != 0 {
		t.Error("the check followed the redirect")
	}
}

func TestACheckGoesThroughNoProxy(t *testing.T) {
	t.Setenv("HTTP_PROXY", "http://127.0.0.1:1")
	t.Setenv("http_proxy", "http://127.0.0.1:1")
	address, port := serve(t, func(w http.ResponseWriter, _ *http.Request) { w.WriteHeader(http.StatusOK) })
	if r, err := Run(t.Context(), &Check{HTTP: &HTTP{Port: port}}, address); err != nil || !r.Passed {
		t.Errorf("with a proxy in the environment, the check = %+v, %v", r, err)
	}
}

// A check holds nothing between runs: each connection closes.
func TestACheckLeavesNoConnection(t *testing.T) {
	opened := make(chan net.Conn, 16)
	s := httptest.NewUnstartedServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) { w.WriteHeader(http.StatusOK) }))
	var closed atomic.Int64
	s.Config.ConnState = func(c net.Conn, st http.ConnState) {
		switch st {
		case http.StateNew:
			opened <- c
		case http.StateClosed:
			closed.Add(1)
		case http.StateActive, http.StateIdle, http.StateHijacked:
		}
	}
	s.Start()
	defer s.Close()
	host, p, _ := net.SplitHostPort(s.Listener.Addr().String())
	port, _ := strconv.Atoi(p)
	for range 3 {
		if _, err := Run(t.Context(), &Check{HTTP: &HTTP{Port: port}}, host); err != nil {
			t.Fatal(err)
		}
	}
	deadline := time.Now().Add(5 * time.Second)
	for closed.Load() < 3 && time.Now().Before(deadline) {
		time.Sleep(5 * time.Millisecond)
	}
	if closed.Load() != 3 {
		t.Errorf("after 3 checks, %d of %d connections closed", closed.Load(), len(opened))
	}
}

func TestATCPCheck(t *testing.T) {
	l, err := (&net.ListenConfig{}).Listen(t.Context(), "tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	port := l.Addr().(*net.TCPAddr).Port
	c := &Check{TCP: &TCP{Port: port}}
	if r, err := Run(t.Context(), c, "127.0.0.1"); err != nil || !r.Passed {
		t.Errorf("a port that takes the connection = %+v, %v", r, err)
	}
	_ = l.Close()
	r, err := Run(t.Context(), c, "127.0.0.1")
	if err != nil || r.Passed || !strings.Contains(r.Output, "was refused") || !strings.Contains(r.Output, "every address") {
		t.Errorf("a port nothing listens on = %+v, %v, want it to say so, and to look at what the app listens on", r, err)
	}
	if strings.Contains(r.Output, "127.0.0.1") {
		t.Errorf("a refusal gives the address: %q", r.Output)
	}
}

// A check that gets no answer gives up at its timeout, and says it did.
func TestACheckTimesOut(t *testing.T) {
	release := make(chan struct{})
	defer close(release)
	address, port := serve(t, func(w http.ResponseWriter, _ *http.Request) { <-release })
	c := &Check{HTTP: &HTTP{Port: port}, Timeout: 50 * time.Millisecond}
	start := time.Now()
	r, err := Run(t.Context(), c, address)
	if err != nil || r.Passed || !strings.Contains(r.Output, "got no answer within 50ms") {
		t.Errorf("a check nothing answers = %+v, %v", r, err)
	}
	if took := time.Since(start); took > 5*time.Second {
		t.Errorf("it took %s", took)
	}
}

func TestACheckEndsWithItsContext(t *testing.T) {
	release := make(chan struct{})
	defer close(release)
	address, port := serve(t, func(http.ResponseWriter, *http.Request) { <-release })
	ctx, cancel := context.WithCancel(t.Context())
	done := make(chan Result, 1)
	go func() {
		r, _ := Run(ctx, &Check{HTTP: &HTTP{Port: port}, Timeout: time.Minute}, address)
		done <- r
	}()
	time.Sleep(20 * time.Millisecond)
	cancel()
	select {
	case r := <-done:
		if r.Passed {
			t.Errorf("a check cut off passed: %+v", r)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("the check went on after its context ended")
	}
}

func TestBoundIsWhatChecksTakeToGiveUp(t *testing.T) {
	c := &Check{TCP: &TCP{Port: 1}, StartPeriod: 20 * time.Second, Interval: 2 * time.Second, Timeout: time.Second, Retries: 4}
	if got, want := c.Bound(), 20*time.Second+4*3*time.Second; got != want {
		t.Errorf("Bound = %s, want %s", got, want)
	}
	if got, want := (&Check{TCP: &TCP{Port: 1}}).Bound(), 3*(DefaultInterval+DefaultTimeout); got != want {
		t.Errorf("Bound with the defaults = %s, want %s", got, want)
	}
}

func TestALabelReadsBack(t *testing.T) {
	c := &Check{HTTP: &HTTP{Port: 80, Path: "/up", Host: "a.example"}, Interval: 2 * time.Second, Retries: 5}
	got, err := Parse(c.Encode())
	if err != nil {
		t.Fatal(err)
	}
	if got.HTTP == nil || *got.HTTP != *c.HTTP || got.Interval != c.Interval || got.Retries != 5 {
		t.Errorf("read back as %+v", got)
	}
	for _, bad := range []string{"", "not json", "{}", `{"interval":5}`} {
		if _, err := Parse(bad); err == nil {
			t.Errorf("Parse(%q) took it", bad)
		}
	}
}

// What a check prints reaches a timeline anyone with the read scope sees, so it
// is cut short, and what the container's environment holds is masked.
func TestCleanKeepsOutWhatItCan(t *testing.T) {
	env := []string{"DATABASE_PASSWORD=hunter2hunter2", "DEBUG=1", "MODE=on", "API_KEY=abcdef"}
	for in, want := range map[string]string{
		"could not connect with hunter2hunter2":                 "could not connect with •••",
		"key abcdef refused":                                    "key ••• refused",
		"mode on, debug 1":                                      "mode on, debug 1",
		"dial postgres://app:s3cret@db:5432/app: refused":       "dial postgres://app:•••@db:5432/app: refused",
		"curl: (7) Failed to connect to localhost\nport 80\r\n": "curl: (7) Failed to connect to localhost port 80",
		"\x1b[31mred\x1b[0m":                                    "[31mred[0m",
		"\u202eoverride":                                        "override",
		"   leading   space  ":                                  "leading space",
		"":                                                      "",
	} {
		if got := Clean(in, env); got != want {
			t.Errorf("Clean(%q) = %q, want %q", in, got, want)
		}
	}
	long := Clean(strings.Repeat("x", 10_000), nil)
	if len(long) > MaxOutput+len("…") || !strings.HasSuffix(long, "…") {
		t.Errorf("a long output is %d bytes", len(long))
	}
}

func TestWhatDockersCheckSaid(t *testing.T) {
	if _, ok := FromDocker(nil, nil); ok {
		t.Error("no health check said something")
	}
	if _, ok := FromDocker(&docker.Health{Status: "starting"}, nil); ok {
		t.Error("a check that hasn't run said something")
	}
	at := time.Unix(1_700_000_000, 0)
	r, ok := FromDocker(&docker.Health{Log: []docker.HealthRun{
		{End: at.Add(-time.Minute), ExitCode: 0, Output: "old\n"},
		{End: at, ExitCode: 22, Output: "curl: (22) The requested URL returned error: 503\ntoken=topsecret\n"},
	}}, []string{"TOKEN=topsecret"})
	if !ok || r.Passed || !r.At.Equal(at) {
		t.Fatalf("the last run = %+v, %t", r, ok)
	}
	if want := "the command exited with 22: curl: (22) The requested URL returned error: 503 token=•••"; r.Output != want {
		t.Errorf("it says %q, want %q", r.Output, want)
	}
	r, _ = FromDocker(&docker.Health{Log: []docker.HealthRun{{ExitCode: 1}}}, nil)
	if r.Output != "the command exited with 1: the command printed nothing" {
		t.Errorf("a silent command says %q", r.Output)
	}
}

func TestWhereAContainerIsReached(t *testing.T) {
	n := map[string]*docker.EndpointSettings{
		"env_x_back":          {IPAddress: "172.20.0.2"},
		"env_x_a":             {IPAddress: "172.21.0.2"},
		docker.IngressNetwork: {IPAddress: "172.30.0.2"},
	}
	if got := Address(n); got != "172.30.0.2" {
		t.Errorf("on the ingress network too, it is reached at %s", got)
	}
	delete(n, docker.IngressNetwork)
	if got := Address(n); got != "172.21.0.2" {
		t.Errorf("it is reached at %s, want the same address each time", got)
	}
	if got := Address(map[string]*docker.EndpointSettings{"bridge": {}}); got != "" {
		t.Errorf("a stopped container is at %q", got)
	}
}

// A check goes to the container, whatever the agent's environment says. This reads
// the client itself, as Go's proxy settings are read once for a process, and pass
// over loopback, where a test has its servers.
func TestAChecksClientTakesNoProxy(t *testing.T) {
	if client.Transport.(*http.Transport).Proxy != nil {
		t.Error("a check would go through HTTP_PROXY")
	}
}
