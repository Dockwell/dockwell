// SPDX-License-Identifier: AGPL-3.0-only

//go:build !agent

package cli

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net"
	"os"
	"path/filepath"
	"regexp"
	"slices"
	"strings"
	"sync"
	"syscall"
	"testing"
	"time"

	agentv1 "github.com/dockwell/dockwell/gen/dockwell/agent/v1"
	dockwellv1 "github.com/dockwell/dockwell/gen/dockwell/v1"
	"github.com/dockwell/dockwell/internal/control/fleet"
	"github.com/dockwell/dockwell/internal/control/identity"
	"github.com/dockwell/dockwell/internal/seal"
	"github.com/dockwell/dockwell/internal/store"
)

func TestHelp(t *testing.T) {
	r := run(t, nil)
	if r.code != exitOK {
		t.Fatalf("exit %d, stderr %q", r.code, r.err)
	}
	for _, want := range []string{"Commands:", "run", "version", "--log-level", `Run "dockwell <command> --help"`} {
		if !strings.Contains(r.out, want) {
			t.Errorf("help missing %q:\n%s", want, r.out)
		}
	}
	// Content rule: sentence case everywhere, including cobra's headings.
	for _, banned := range []string{"Available Commands", "Global Flags", "Use \"dockwell [command]"} {
		if strings.Contains(r.out, banned) {
			t.Errorf("help still contains %q:\n%s", banned, r.out)
		}
	}

	sub := run(t, nil, "run", "--help")
	if !strings.Contains(sub.out, "Global flags:") || !strings.Contains(sub.out, "--listen") {
		t.Errorf("run --help:\n%s", sub.out)
	}
}

func TestRunExplainsListenFailures(t *testing.T) {
	var lc net.ListenConfig
	busy, err := lc.Listen(t.Context(), "tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer busy.Close()

	tests := []struct {
		name, listen, want string
	}{
		{"address in use", busy.Addr().String(), "Stop the process using that address, or pass a different --listen address"},
		{"malformed address", "no-port-here", "Pass --listen as host:port, such as 127.0.0.1:7600"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			r := run(t, map[string]string{envLogFormat: "json", envDataDir: t.TempDir()},
				"run", "--listen", tt.listen, "--agent-listen", "127.0.0.1:0")
			if r.code != exitError {
				t.Fatalf("exit %d, want %d; stderr %q", r.code, exitError, r.err)
			}
			if !strings.Contains(r.err, "can't listen on "+tt.listen) || !strings.Contains(r.err, tt.want) {
				t.Errorf("stderr = %q, want it to contain %q", r.err, tt.want)
			}
		})
	}
}

func TestWithListenHint(t *testing.T) {
	if withListenHint(nil, "--listen") != nil {
		t.Error("withListenHint(nil) should stay nil")
	}
	eacces := fmt.Errorf("can't listen on 0.0.0.0:80: %w", &net.OpError{Op: "listen", Err: syscall.EACCES})
	if got := withListenHint(eacces, "--agent-listen").Error(); !strings.Contains(got, "CAP_NET_BIND_SERVICE") || !strings.Contains(got, "--agent-listen") {
		t.Errorf("EACCES hint = %q", got)
	}
	malformed := fmt.Errorf("can't listen on foo: %w", &net.AddrError{Err: "missing port in address", Addr: "foo"})
	if got := withListenHint(malformed, "--agent-listen").Error(); !strings.Contains(got, "such as :7443") {
		t.Errorf("the agent listener's example should be its own default, got %q", got)
	}
	if got := withListenHint(eacces, "--listen").Error(); !strings.Contains(got, "CAP_NET_BIND_SERVICE") {
		t.Errorf("EACCES hint = %q", got)
	}
	other := errors.New("something else")
	if got := withListenHint(other, "--listen"); !errors.Is(got, other) || got.Error() != "something else" {
		t.Errorf("unrelated errors should pass through untouched, got %q", got)
	}
}

func TestRunStopsOnCancel(t *testing.T) {
	ctx, cancel := context.WithCancel(t.Context())
	done := make(chan int, 1)
	var out, errOut bytes.Buffer
	go func() {
		done <- Execute(ctx, []string{"run", "--listen", "127.0.0.1:0", "--agent-listen", "127.0.0.1:0", "--data-dir", t.TempDir(), "--log-format", "json"}, IO{
			Out: &out, Err: &errOut, Getenv: noEnv,
		})
	}()
	time.Sleep(100 * time.Millisecond)
	cancel()
	select {
	case code := <-done:
		if code != exitOK {
			t.Fatalf("exit %d after cancel, stderr:\n%s", code, errOut.String())
		}
	case <-time.After(5 * time.Second):
		t.Fatal("run did not stop within 5 s of cancellation")
	}
}

// startRun runs `dockwell run` on loopback ports with its own data
// directory, and waits until the control server's agent has its identity.
func startRun(t *testing.T) string {
	t.Helper()
	dir := t.TempDir()
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan int, 1)
	var errOut syncBuffer
	go func() {
		done <- Execute(ctx, []string{"run", "--listen", "127.0.0.1:0", "--agent-listen", "127.0.0.1:0", "--data-dir", dir, "--log-format", "json"}, IO{
			Out: io.Discard, Err: &errOut, Getenv: noEnv,
		})
	}()
	t.Cleanup(func() {
		cancel()
		if code := <-done; code != exitOK {
			t.Errorf("run exited %d:\n%s", code, errOut.String())
		}
	})
	deadline := time.Now().Add(startUp)
	for {
		if _, err := os.Stat(filepath.Join(dir, "agent", "agent.json")); err == nil {
			return dir
		}
		select {
		case code := <-done:
			done <- code // for the cleanup, which waits on it
			t.Fatalf("dockwell run exited %d before its agent got an identity:\n%s", code, errOut.String())
		default:
		}
		if time.Now().After(deadline) {
			t.Fatalf("the control server's agent never got an identity:\n%s", errOut.String())
		}
		time.Sleep(20 * time.Millisecond)
	}
}

func TestAddAServerThroughTheCLI(t *testing.T) {
	control := startRun(t)
	none := map[string]string{envLogFormat: "json"}

	r := run(t, none, "token", "create", "--data-dir", control, "--ttl", "30m")
	if r.code != exitOK {
		t.Fatalf("token create exit %d: %s", r.code, r.err)
	}
	ticket := regexp.MustCompile(`dockwell join (dwj1\.\S+)`).FindStringSubmatch(r.out)
	if ticket == nil || !strings.Contains(r.out, "expires in 30 min") {
		t.Fatalf("token create printed:\n%s", r.out)
	}

	added := t.TempDir()
	if r = run(t, none, "role", "--data-dir", added); r.code != exitOK || r.out != "control\n" {
		t.Errorf("role before joining = %d %q, want control: run would start a control plane", r.code, r.out)
	}
	r = run(t, none, "join", ticket[1], "--data-dir", added, "--name", "hz-2")
	if r.code != exitOK || !strings.HasPrefix(r.out, "Joined as hz-2 (srv_") {
		t.Fatalf("join exit %d:\n%s%s", r.code, r.out, r.err)
	}
	if r = run(t, none, "role", "--data-dir", added); r.code != exitOK || !regexp.MustCompile(`^agent https://\S+\n$`).MatchString(r.out) {
		t.Errorf("role after joining = %d %q, want agent and the control plane's URL", r.code, r.out)
	}
	if r = run(t, none, "role", "--data-dir", control); r.code != exitOK || r.out != "control\n" {
		t.Errorf("role on the control server = %d %q", r.code, r.out)
	}
	r = run(t, none, "join", ticket[1], "--data-dir", control)
	if r.code != exitError || !strings.Contains(r.err, "runs a control plane") {
		t.Errorf("joining from the control server = %d %q", r.code, r.err)
	}

	r = run(t, none, "join", ticket[1], "--data-dir", added)
	if r.code != exitError || !strings.Contains(r.err, "already joined") {
		t.Errorf("joining twice = %d %q", r.code, r.err)
	}
	r = run(t, none, "join", ticket[1], "--data-dir", t.TempDir())
	if r.code != exitError || !strings.Contains(r.err, "invalid, already used or expired") {
		t.Errorf("reusing a ticket = %d %q", r.code, r.err)
	}
	r = run(t, none, "token", "create", "--data-dir", t.TempDir())
	if r.code != exitError || !strings.Contains(r.err, "there's no control plane") {
		t.Errorf("token create away from the control server = %d %q", r.code, r.err)
	}
	r = run(t, none, "join")
	if r.code != exitUsage || !strings.Contains(r.err, "takes one join ticket") {
		t.Errorf("join without a ticket = %d %q", r.code, r.err)
	}
}

func TestLoopbackURL(t *testing.T) {
	for _, tt := range []struct {
		addr *net.TCPAddr
		want string
	}{
		{&net.TCPAddr{IP: net.IPv4zero, Port: 7443}, "https://127.0.0.1:7443"},
		{&net.TCPAddr{IP: net.IPv6unspecified, Port: 7443}, "https://127.0.0.1:7443"},
		{&net.TCPAddr{IP: net.ParseIP("10.0.0.5"), Port: 9443}, "https://10.0.0.5:9443"},
		{&net.TCPAddr{IP: net.IPv6loopback, Port: 7443}, "https://[::1]:7443"},
	} {
		if got := loopbackURL(tt.addr); got != tt.want {
			t.Errorf("loopbackURL(%v) = %q, want %q", tt.addr, got, tt.want)
		}
	}
}

// What a ticket leads with decides what it takes to use it from elsewhere, and only
// what works is said.
func TestJoinWordsSayWhatEachCaseNeeds(t *testing.T) {
	type u = identity.JoinURL
	for _, tc := range []struct {
		name  string
		urls  []u
		says  []string
		never []string
	}{
		{
			"a way in",
			[]u{{URL: "https://203.0.113.50:7443", WayIn: true}},
			[]string{"reaches this one at 203.0.113.50 on port 7443, so open that port at your provider and on this machine"},
			[]string{"forwarded", "older Dockwell"},
		},
		{
			"a way out",
			[]u{{URL: "https://203.0.113.7:7443"}, {URL: "https://10.0.0.25:7443"}},
			[]string{"leads with 203.0.113.7, which is how this server reaches the internet, and may not be a way in", "port 7443 there leads to this server", "set it under Network on its page"},
			[]string{"open that port at your provider"},
		},
		{
			"a private address alone",
			[]u{{URL: "https://10.0.0.25:7443"}},
			[]string{"10.0.0.25 is a private address, so only a server on the same network can use this ticket", "set this server's public address under Network on its page, or restart dockwell run with --advertise-url"},
			[]string{"open port"},
		},
		{"a shared address", []u{{URL: "https://100.72.1.2:7443"}}, []string{"100.72.1.2 is a private address"}, nil},
		{"loopback", []u{{URL: "https://127.0.0.1:7443"}}, []string{"127.0.0.1 is a private address"}, nil},
		{"a private IPv6 address", []u{{URL: "https://[fd00::5]:7443"}}, []string{"fd00::5 is a private address"}, nil},
		{
			"more than one",
			[]u{{URL: "https://203.0.113.50:7443", WayIn: true}, {URL: "https://10.0.0.25:7443"}},
			[]string{"A server running an older Dockwell tries https://203.0.113.50:7443 alone"},
			nil,
		},
	} {
		got := joinWords(tc.urls)
		for _, want := range tc.says {
			if !strings.Contains(got, want) {
				t.Errorf("%s says %q, not %q", tc.name, got, want)
			}
		}
		for _, not := range tc.never {
			if strings.Contains(got, not) {
				t.Errorf("%s says %q, with %q", tc.name, got, not)
			}
		}
	}
	if got := joinWords([]identity.JoinURL{{URL: "https://agents.example.com:443", WayIn: true}}); got != "" {
		t.Errorf("a name says %q", got)
	}
}

func TestACertificateCoversEachAddressATicketCanHold(t *testing.T) {
	hosts := agentHosts("https://10.0.0.25:7443", "https://203.0.113.50:7443")
	for _, want := range []string{"10.0.0.25", "203.0.113.50", "127.0.0.1", "localhost"} {
		if !slices.Contains(hosts, want) {
			t.Errorf("%v doesn't have %s", hosts, want)
		}
	}
	if got := agentHosts("https://10.0.0.25:7443", ""); slices.Contains(got, "") {
		t.Errorf("an unknown address went in as %v", got)
	}
}

// What a ticket leads with follows the control server's public addresses as they're
// found, with its own address behind them, for a server on the same network, and
// stops once there are none. The certificate has each first, so no ticket names what
// it doesn't cover, and keeps them all through a restart.
func TestJoinTicketsFollowThePublicAddress(t *testing.T) {
	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()
	dir := t.TempDir()
	key, err := seal.LoadOrCreate(filepath.Join(dir, "key"))
	if err != nil {
		t.Fatal(err)
	}
	db, err := store.Open(ctx, filepath.Join(dir, "state.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	logger := slog.New(slog.DiscardHandler)
	authority, err := identity.Bootstrap(ctx, db, key, logger)
	if err != nil {
		t.Fatal(err)
	}
	const local = "https://10.0.0.25:7443"
	if err := authority.SetAgentURL(ctx, local); err != nil {
		t.Fatal(err)
	}
	now := time.Now()
	if err := db.Update(ctx, func(tx *store.Tx) error {
		return tx.CreateServer(ctx, store.Server{ID: "srv_A", Name: "oci-1", CreatedAt: now, CertSerial: "s", CertNotAfter: now})
	}); err != nil {
		t.Fatal(err)
	}
	hub, err := fleet.New(ctx, db, logger)
	if err != nil {
		t.Fatal(err)
	}
	hub.SetControlServer("srv_A")
	urls := func() []string {
		t.Helper()
		got, err := authority.JoinURLs(ctx)
		if err != nil {
			t.Fatal(err)
		}
		var out []string
		for _, u := range got {
			out = append(out, u.URL)
		}
		return out
	}
	waitFor := func(what string, want ...string) {
		t.Helper()
		deadline := time.Now().Add(5 * time.Second)
		for !slices.Equal(urls(), want) {
			if time.Now().After(deadline) {
				t.Fatalf("%s: tickets name %v, want %v", what, urls(), want)
			}
			time.Sleep(5 * time.Millisecond)
		}
	}
	if got := urls(); !slices.Equal(got, []string{local}) {
		t.Fatalf("before anything's found: %v", got)
	}
	var mu sync.Mutex
	var covered []string
	done := make(chan struct{})
	go func() {
		defer close(done)
		followPublicAddress(ctx, logger, hub, func(host string) {
			mu.Lock()
			defer mu.Unlock()
			covered = append(covered, host)
		}, authority, "7443")
	}()
	if _, err := hub.Attach(ctx, store.Server{ID: "srv_A"}, &dockwellv1.ServerInventory{Addresses: []string{"10.0.0.25", "2001:db8::5"}}, &dockwellv1.BuildInfo{}); err != nil {
		t.Fatal(err)
	}
	hub.RecordNetwork("srv_A", &agentv1.Network{Provider: dockwellv1.CloudProvider_CLOUD_PROVIDER_ORACLE, MachineProvider: dockwellv1.CloudProvider_CLOUD_PROVIDER_ORACLE, LookedUpAddress: "203.0.113.50"})
	waitFor("once found", "https://[2001:db8::5]:7443", "https://203.0.113.50:7443", local)
	mu.Lock()
	if !slices.Contains(covered, "2001:db8::5") || !slices.Contains(covered, "203.0.113.50") {
		t.Errorf("the certificate names %v", covered)
	}
	mu.Unlock()

	// Behind carrier NAT now, with no IPv6: there's nothing public to lead with.
	if _, err := hub.Attach(ctx, store.Server{ID: "srv_A"}, &dockwellv1.ServerInventory{Addresses: []string{"100.72.1.2"}, OutboundAddress: "100.72.1.2"}, &dockwellv1.BuildInfo{}); err != nil {
		t.Fatal(err)
	}
	hub.RecordNetwork("srv_A", &agentv1.Network{LookedUpAddress: "203.0.113.7"})
	waitFor("behind carrier NAT", local)
	cancel()
	<-done

	// A restart still names every address a ticket has led with.
	hosts, err := agentHostsAtStart(t.Context(), authority, local, true)
	if err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{"10.0.0.25", "2001:db8::5", "203.0.113.50", "127.0.0.1"} {
		if !slices.Contains(hosts, want) {
			t.Errorf("after a restart the certificate names %v, without %s", hosts, want)
		}
	}
}

// Until anything's heard of the control server's network, what the last run found
// stands, so a ticket made in the seconds after a restart still leads with it.
func TestAFollowerWaitsUntilSomethingsKnown(t *testing.T) {
	ctx, cancel := context.WithCancel(t.Context())
	dir := t.TempDir()
	key, err := seal.LoadOrCreate(filepath.Join(dir, "key"))
	if err != nil {
		t.Fatal(err)
	}
	db, err := store.Open(ctx, filepath.Join(dir, "state.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	logger := slog.New(slog.DiscardHandler)
	authority, err := identity.Bootstrap(ctx, db, key, logger)
	if err != nil {
		t.Fatal(err)
	}
	found := []identity.JoinURL{{URL: "https://203.0.113.50:7443", WayIn: true}}
	if err := authority.SetPublicAgentURLs(ctx, found); err != nil {
		t.Fatal(err)
	}
	now := time.Now()
	if err := db.Update(ctx, func(tx *store.Tx) error {
		return tx.CreateServer(ctx, store.Server{ID: "srv_A", Name: "oci-1", CreatedAt: now, CertSerial: "s", CertNotAfter: now})
	}); err != nil {
		t.Fatal(err)
	}
	hub, err := fleet.New(ctx, db, logger)
	if err != nil {
		t.Fatal(err)
	}
	hub.SetControlServer("srv_A")
	done := make(chan struct{})
	go func() {
		defer close(done)
		followPublicAddress(ctx, logger, hub, func(string) {}, authority, "7443")
	}()
	if _, err := hub.Attach(ctx, store.Server{ID: "srv_A"}, &dockwellv1.ServerInventory{Addresses: []string{"10.0.0.25"}}, &dockwellv1.BuildInfo{}); err != nil {
		t.Fatal(err)
	}
	time.Sleep(100 * time.Millisecond)
	cancel()
	<-done
	if got, err := authority.PublicAgentURLs(t.Context()); err != nil || !slices.Equal(got, found) {
		t.Errorf("with nothing heard, tickets lead with %v (%v)", got, err)
	}
}

// A listener bound to one address, or given its address by hand, isn't reached at a
// public address found for it, so it starts with tickets leading with its own.
func TestAListenerThatWontBeFollowedClearsWhatTicketsLeadWith(t *testing.T) {
	dir := t.TempDir()
	key, err := seal.LoadOrCreate(filepath.Join(dir, "key"))
	if err != nil {
		t.Fatal(err)
	}
	db, err := store.Open(t.Context(), filepath.Join(dir, "state.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	authority, err := identity.Bootstrap(t.Context(), db, key, slog.New(slog.DiscardHandler))
	if err != nil {
		t.Fatal(err)
	}
	if err := authority.SetAgentURL(t.Context(), "https://10.0.0.25:7443"); err != nil {
		t.Fatal(err)
	}
	if err := authority.SetPublicAgentURLs(t.Context(), []identity.JoinURL{{URL: "https://203.0.113.50:7443", WayIn: true}}); err != nil {
		t.Fatal(err)
	}
	if _, err := agentHostsAtStart(t.Context(), authority, "https://10.0.0.25:7443", true); err != nil {
		t.Fatal(err)
	}
	if got, _ := authority.PublicAgentURLs(t.Context()); len(got) != 1 {
		t.Fatalf("a listener that's followed lost what the last run found: %v", got)
	}
	hosts, err := agentHostsAtStart(t.Context(), authority, "https://10.0.0.25:7443", false)
	if err != nil {
		t.Fatal(err)
	}
	if got, _ := authority.PublicAgentURLs(t.Context()); len(got) != 0 {
		t.Errorf("tickets still lead with %v", got)
	}
	if !slices.Contains(hosts, "203.0.113.50") {
		t.Errorf("the certificate forgot a host a ticket named: %v", hosts)
	}
	if listensEverywhere(&net.TCPAddr{IP: net.ParseIP("10.0.0.25"), Port: 7443}) || !listensEverywhere(&net.TCPAddr{IP: net.IPv6unspecified, Port: 7443}) {
		t.Error("it can't tell a listener bound to one address")
	}
}

// Settings name the panel's address with its port, as a claim link does, and who
// issues its certificate, as dockwell run was told.
func TestThePanelsAddressAndItsCA(t *testing.T) {
	for _, tt := range []struct {
		addr panelAddress
		want string
	}{
		{panelAddress{Port: "7600"}, ""},
		{panelAddress{Domain: "panel.example.com", HTTPSPort: "443", Port: "7600"}, "https://panel.example.com"},
		{panelAddress{Domain: "panel.example.com", HTTPSPort: "8443", Port: "7600"}, "https://panel.example.com:8443"},
	} {
		if got := panelURL(tt.addr); got != tt.want {
			t.Errorf("panelURL(%+v) = %q, want %q", tt.addr, got, tt.want)
		}
	}
	for _, tt := range []struct {
		domain, directory, want string
	}{
		{"", "", ""},
		{"panel.example.com", "", "Let's Encrypt"},
		{"panel.example.com", "https://acme-v02.api.letsencrypt.org/directory", "Let's Encrypt"},
		{"panel.example.com", "https://acme-staging-v02.api.letsencrypt.org/directory", "Let's Encrypt's staging CA"},
		{"panel.example.com", "https://acme.example.com:14000/dir", "acme.example.com"},
	} {
		if got := certificateAuthority(runFlags{domain: tt.domain, acmeDirector: tt.directory}); got != tt.want {
			t.Errorf("certificateAuthority(%q, %q) = %q, want %q", tt.domain, tt.directory, got, tt.want)
		}
	}
}
