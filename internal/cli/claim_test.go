// SPDX-License-Identifier: AGPL-3.0-only

//go:build !agent

package cli

import (
	"context"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"os/user"
	"path/filepath"
	"regexp"
	"strings"
	"testing"
	"time"

	"connectrpc.com/connect"

	dockwellv1 "github.com/dockwell/dockwell/gen/dockwell/v1"
	"github.com/dockwell/dockwell/gen/dockwell/v1/dockwellv1connect"
	"github.com/dockwell/dockwell/internal/auth"
	"github.com/dockwell/dockwell/internal/auth/authtest"
	"github.com/dockwell/dockwell/internal/datadir"
	"github.com/dockwell/dockwell/internal/seal"
	"github.com/dockwell/dockwell/internal/store"
)

var linkRE = regexp.MustCompile(`(https?://\S+)/(claim|recover)#(\S+)`)

// startRunWithOutput runs dockwell run on dir until the test ends, or until
// stop is called, and returns its standard output once it contains want.
func startRunWithOutput(t *testing.T, dir, want string, args ...string) (out *syncBuffer, stop func()) {
	t.Helper()
	out, _, stop = startRunWithLogs(t, dir, want, args...)
	return out, stop
}

// startRunWithLogs is startRunWithOutput that also returns what dockwell
// run logged. Its standard output stands for a terminal, where a person who
// started it reads it, as the claim link goes nowhere else.
func startRunWithLogs(t *testing.T, dir, want string, args ...string) (out, logs *syncBuffer, stop func()) {
	t.Helper()
	return startRunOn(t, true, dir, want, args...)
}

// startRunAsService is startRunWithLogs with standard output that isn't a
// terminal, as a service's goes to the journal, and returns once it has said
// how to get a claim link.
func startRunAsService(t *testing.T, dir string) (out, logs *syncBuffer, stop func()) {
	t.Helper()
	return startRunOn(t, false, dir, "To claim it, run this on this server")
}

func startRunOn(t *testing.T, terminal bool, dir, want string, args ...string) (out, logs *syncBuffer, stop func()) {
	t.Helper()
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan int, 1)
	out = &syncBuffer{}
	errOut := &syncBuffer{}
	all := append([]string{"run", "--listen", "127.0.0.1:0", "--agent-listen", "127.0.0.1:0", "--data-dir", dir, "--log-format", "json"}, args...)
	go func() {
		done <- Execute(ctx, all, IO{Out: out, Err: errOut, Getenv: func(k string) string {
			return map[string]string{"USER": "mira", "SSH_CONNECTION": "198.51.100.23 52000 203.0.113.7 22"}[k]
		}, OutIsTerminal: func() bool { return terminal }})
	}()
	stopped := false
	stop = func() {
		if stopped {
			return
		}
		stopped = true
		cancel()
		if code := <-done; code != exitOK {
			t.Errorf("run exited %d:\n%s", code, errOut.String())
		}
	}
	t.Cleanup(stop)
	deadline := time.Now().Add(10 * time.Second)
	for !strings.Contains(out.String(), want) {
		if time.Now().After(deadline) {
			t.Fatalf("dockwell run never printed %q:\n%s\n%s", want, out.String(), errOut.String())
		}
		time.Sleep(20 * time.Millisecond)
	}
	return out, errOut, stop
}

// claimOverHTTP claims the install from link with a software passkey, as
// the panel would.
func claimOverHTTP(t *testing.T, link string) {
	t.Helper()
	m := linkRE.FindStringSubmatch(link)
	base, token := m[1], m[3]
	var d net.Dialer
	addr := strings.TrimPrefix(base, "http://localhost")
	client := &http.Client{Transport: &http.Transport{DialContext: func(ctx context.Context, network, _ string) (net.Conn, error) {
		return d.DialContext(ctx, network, "127.0.0.1"+addr)
	}}}
	c := dockwellv1connect.NewAuthServiceClient(client, base, connect.WithInterceptors(originHeader(base)))
	deadline := time.Now().Add(5 * time.Second)
	var begin *connect.Response[dockwellv1.BeginClaimResponse]
	var err error
	for {
		begin, err = c.BeginClaim(t.Context(), connect.NewRequest(&dockwellv1.BeginClaimRequest{Token: token, Name: "Mira Chen", Email: "mira@acme.dev"}))
		if err == nil || time.Now().After(deadline) {
			break
		}
		time.Sleep(20 * time.Millisecond)
	}
	if err != nil {
		t.Fatalf("BeginClaim: %v", err)
	}
	a := &authtest.Authenticator{}
	cred, _, err := a.Create(begin.Msg.GetOptionsJson(), base)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := c.FinishClaim(t.Context(), connect.NewRequest(&dockwellv1.FinishClaimRequest{
		CeremonyId: begin.Msg.GetCeremonyId(), Token: token, CredentialJson: cred,
	})); err != nil {
		t.Fatalf("FinishClaim: %v", err)
	}
}

func originHeader(origin string) connect.UnaryInterceptorFunc {
	return func(next connect.UnaryFunc) connect.UnaryFunc {
		return func(ctx context.Context, req connect.AnyRequest) (connect.AnyResponse, error) {
			req.Header().Set("Origin", origin)
			return next(ctx, req)
		}
	}
}

func TestClaimAndRecoverThroughTheCLI(t *testing.T) {
	dir := t.TempDir()
	out, _ := startRunWithOutput(t, dir, "/claim#")
	for _, want := range []string{
		"This install has no owner yet. Claim it within 24 h",
		"ssh -L ", ":127.0.0.1:", " mira@203.0.113.7",
		"http://localhost:",
		"Run dockwell claim --data-dir " + dir + " for a new link.",
	} {
		if !strings.Contains(out.String(), want) {
			t.Errorf("dockwell run didn't print %q:\n%s", want, out.String())
		}
	}
	first := linkRE.FindString(out.String())

	none := map[string]string{envLogFormat: "json"}
	r := run(t, none, "claim", "--data-dir", dir)
	if r.code != exitOK || !strings.Contains(r.out, "/claim#") {
		t.Fatalf("dockwell claim = %d:\n%s%s", r.code, r.out, r.err)
	}
	second := linkRE.FindString(r.out)
	if second == first {
		t.Fatal("dockwell claim printed the same link")
	}

	claimOverHTTP(t, second)

	// Its own exit status, so install.sh can tell it from a failure.
	r = run(t, none, "claim", "--data-dir", dir)
	if r.code != exitClaimed || !strings.Contains(r.err, "already has an owner, mira@acme.dev") || !strings.Contains(r.err, "dockwell recover") {
		t.Errorf("dockwell claim after claiming = %d %q, want %d", r.code, r.err, exitClaimed)
	}
	r = run(t, none, "recover", "--data-dir", dir)
	if r.code != exitOK || !strings.Contains(r.out, "signs you in as mira@acme.dev once, within 15 min") || !strings.Contains(r.out, "/recover#") {
		t.Errorf("dockwell recover = %d:\n%s%s", r.code, r.out, r.err)
	}
	r = run(t, none, "recover", "--data-dir", dir, "--user", "eve@acme.dev")
	if r.code != exitUsage || !strings.Contains(r.err, "nobody signs in as eve@acme.dev") {
		t.Errorf("dockwell recover for nobody = %d %q", r.code, r.err)
	}
	r = run(t, none, "claim", "--data-dir", t.TempDir())
	if r.code != exitError || !strings.Contains(r.err, "there's no control plane") {
		t.Errorf("dockwell claim away from the control server = %d %q", r.code, r.err)
	}
}

func TestRunSaysWhenThePanelGainsADomain(t *testing.T) {
	dir := t.TempDir()
	out, stop := startRunWithOutput(t, dir, "/claim#")
	claimOverHTTP(t, linkRE.FindString(out.String()))
	stop()

	domain := []string{
		"--domain", "panel.example.com", "--https-listen", "127.0.0.1:0", "--http-listen", "127.0.0.1:0",
		"--acme-directory-url", "https://acme.invalid/directory",
	}
	out, stop = startRunWithOutput(t, dir, "The panel now answers at https://panel.example.com", domain...)
	if !strings.Contains(out.String(), "passkeys made on localhost don't work at panel.example.com") ||
		!strings.Contains(out.String(), "dockwell recover") {
		t.Errorf("the domain notice doesn't explain passkeys:\n%s", out.String())
	}
	stop()

	// A proxy down takes the domain with it, so the link works through a
	// tunnel too (ADR 0024).
	r := run(t, map[string]string{}, "recover", "--data-dir", dir)
	if r.code != exitOK || !strings.Contains(r.out, "https://panel.example.com:") ||
		!strings.Contains(r.out, "doesn't answer") || !strings.Contains(r.out, "ssh -L") || !strings.Contains(r.out, "http://localhost:") {
		t.Errorf("dockwell recover with a domain = %d:\n%s%s", r.code, r.out, r.err)
	}
}

func TestRunRefusesToServeThePanelBeyondLoopback(t *testing.T) {
	// The fix names the setting as it was given, and never suggests that a
	// domain would let the panel's own listener leave loopback.
	for _, tt := range []struct {
		env  map[string]string
		args []string
		want []string
	}{
		{nil, []string{"--listen", "0.0.0.0:0"}, []string{
			"--listen 0.0.0.0:0 would serve the panel over plain HTTP beyond this server, so Dockwell refuses it.",
			"Remove --listen to keep the panel on 127.0.0.1:7600",
			"keep --listen on loopback and add --domain, which serves it over HTTPS",
		}},
		{map[string]string{envListen: "0.0.0.0:0"}, nil, []string{
			"DOCKWELL_LISTEN=0.0.0.0:0 would serve the panel over plain HTTP beyond this server",
			"Remove DOCKWELL_LISTEN to keep the panel on 127.0.0.1:7600",
			"keep DOCKWELL_LISTEN on loopback and add DOCKWELL_DOMAIN",
		}},
		{nil, []string{"--listen", "0.0.0.0:0", "--domain", "panel.example.com"}, []string{
			"--listen 0.0.0.0:0 would serve the panel over plain HTTP",
			"keep --listen on loopback and add --domain",
		}},
	} {
		env := map[string]string{envLogFormat: "json", envDataDir: t.TempDir()}
		for k, v := range tt.env {
			env[k] = v
		}
		r := run(t, env, append([]string{"run", "--agent-listen", "127.0.0.1:0"}, tt.args...)...)
		if r.code != exitUsage {
			t.Errorf("run %v with %v = %d %q, want a usage error", tt.args, tt.env, r.code, r.err)
		}
		for _, want := range tt.want {
			if !strings.Contains(r.err, want) {
				t.Errorf("run %v with %v said %q, want %q", tt.args, tt.env, r.err, want)
			}
		}
	}
	r := run(t, map[string]string{envLogFormat: "json", envDataDir: t.TempDir()},
		"run", "--domain", "203.0.113.7")
	if r.code != exitUsage || !strings.Contains(r.err, "is an IP address. Passkeys need a hostname") {
		t.Errorf("run --domain <IP> = %d %q", r.code, r.err)
	}
}

func TestSSHTarget(t *testing.T) {
	env := func(m map[string]string) func(string) string { return func(k string) string { return m[k] } }
	public := panelAddress{Port: "7600", PublicHost: "203.0.113.7"}
	// A cloud server behind 1:1 NAT, which you sign in to as ubuntu: its own
	// address is private, and its SSH sessions arrive there too.
	cloud := panelAddress{Port: "7600", PublicHost: "10.0.0.5"}
	for _, tt := range []struct {
		addr panelAddress
		env  map[string]string
		want string
	}{
		// The system service: nothing says who signs in, or as whom.
		{public, nil, "<user>@203.0.113.7"},
		{cloud, nil, "<user>@<your server>"},
		// A user away from an SSH session isn't who signs in from elsewhere.
		{public, map[string]string{"USER": "mira"}, "<user>@203.0.113.7"},
		{public, map[string]string{"USER": "root", "SUDO_USER": "mira"}, "mira@203.0.113.7"},
		{public, map[string]string{"USER": "mira", "SSH_CONNECTION": "198.51.100.23 52000 203.0.113.9 22"}, "mira@203.0.113.9"},
		{public, map[string]string{"USER": "mira", "SSH_CONNECTION": "198.51.100.23 52000 10.0.0.5 22"}, "mira@203.0.113.7"},
		{public, map[string]string{"SSH_CONNECTION": "2001:db8::9 52000 2001:db8::1 22"}, "<user>@[2001:db8::1]"},
		{cloud, map[string]string{"USER": "root", "SUDO_USER": "ubuntu", "SSH_CONNECTION": "198.51.100.23 52000 10.0.0.5 22"}, "ubuntu@<your server>"},
		{cloud, map[string]string{"USER": "ubuntu", "SSH_CONNECTION": "198.51.100.23 52000 10.0.0.5 22"}, "ubuntu@<your server>"},
		{panelAddress{PublicHost: "100.64.3.2"}, nil, "<user>@<your server>"},
		{panelAddress{}, nil, "<user>@<your server>"},
		{panelAddress{PublicHost: "127.0.0.1"}, nil, "<user>@<your server>"},
	} {
		if got := sshTarget(tt.addr, env(tt.env)); got != tt.want {
			t.Errorf("sshTarget(%s, %v) = %q, want %q", tt.addr.PublicHost, tt.env, got, tt.want)
		}
	}
	for addr, want := range map[panelAddress]string{
		{Port: "7600"}: "http://localhost:7600",
		{Domain: "panel.acme.dev", HTTPSPort: "443"}:             "https://panel.acme.dev",
		{Domain: "panel.acme.dev"}:                               "https://panel.acme.dev",
		{Domain: "panel.acme.dev", HTTPSPort: "8443"}:            "https://panel.acme.dev:8443",
		{Domain: "panel.acme.dev", Port: "7600"}:                 "https://panel.acme.dev",
		{Domain: "panel.acme.dev", HTTPSPort: "5001", Port: "1"}: "https://panel.acme.dev:5001",
	} {
		if got := addr.url(); got != want {
			t.Errorf("%+v.url() = %q, want %q", addr, got, want)
		}
	}
}

// The claim screen shows where to point a domain only when this server
// knows a public address: never a loopback or private one, or a hostname.
func TestTheDNSAddressIsPublic(t *testing.T) {
	for host, want := range map[string]string{
		"203.0.113.7":        "203.0.113.7",
		"::ffff:203.0.113.7": "203.0.113.7",
		"2001:db8::1":        "2001:db8::1",
		"127.0.0.1":          "",
		"::1":                "",
		"10.0.0.5":           "",
		"172.31.4.9":         "",
		"192.168.1.20":       "",
		"100.64.3.2":         "",
		"fd00::5":            "",
		"fe80::1":            "",
		"0.0.0.0":            "",
		"agents.example.com": "",
		"":                   "",
	} {
		addr := panelAddress{Port: "7600", PublicHost: host}
		if got := addr.dnsAddress(); got != want {
			t.Errorf("dnsAddress for %q = %q, want %q", host, got, want)
		}
		var b strings.Builder
		printClaim(&b, addr, auth.Link{Token: "x", ExpiresAt: time.Now().Add(time.Hour)}, "dockwell claim", func(string) string { return "" })
		switch {
		case want == "" && !strings.Contains(b.String(), "DNS record at this server's public address,"):
			t.Errorf("with %q, the claim says where to point a domain:\n%s", host, b.String())
		case want != "" && !strings.Contains(b.String(), "record at "+want+","):
			t.Errorf("with %q, the claim doesn't say to point a domain at %s:\n%s", host, want, b.String())
		}
	}
	if got := (panelAddress{PublicHost: "2001:db8::1"}).dnsRecord(); got != "AAAA" {
		t.Errorf("the record for an IPv6 address is %s", got)
	}
}

// The claim screen gets the same public address, or none.
func TestCheckClaimShowsOnlyAPublicAddress(t *testing.T) {
	for advertise, want := range map[string]string{
		"":                          "", // the agent listener's loopback address
		"https://10.0.0.5:7443":     "",
		"https://203.0.113.7:7443":  "203.0.113.7",
		"https://agents.acme.dev:1": "",
	} {
		var args []string
		if advertise != "" {
			args = []string{"--advertise-url", advertise}
		}
		out, stop := startRunWithOutput(t, t.TempDir(), "/claim#", args...)
		m := linkRE.FindStringSubmatch(out.String())
		base, token := m[1], m[3]
		var d net.Dialer
		client := &http.Client{Transport: &http.Transport{DialContext: func(ctx context.Context, network, _ string) (net.Conn, error) {
			return d.DialContext(ctx, network, "127.0.0.1"+strings.TrimPrefix(base, "http://localhost"))
		}}}
		c := dockwellv1connect.NewAuthServiceClient(client, base, connect.WithInterceptors(originHeader(base)))
		res, err := c.CheckClaim(t.Context(), connect.NewRequest(&dockwellv1.CheckClaimRequest{Token: token}))
		if err != nil {
			t.Fatalf("CheckClaim with --advertise-url %q: %v", advertise, err)
		}
		if got := res.Msg.GetPublicAddress(); got != want {
			t.Errorf("with --advertise-url %q, the claim screen's public address is %q, want %q", advertise, got, want)
		}
		stop()
	}
}

// Restarting dockwell run keeps the claim link it printed before, which may
// be the one install.sh showed. Only dockwell claim replaces it.
func TestARestartKeepsTheClaimLink(t *testing.T) {
	dir := t.TempDir()
	out, stop := startRunWithOutput(t, dir, "/claim#")
	first := linkRE.FindString(out.String())
	stop()

	port := strings.TrimPrefix(linkRE.FindStringSubmatch(first)[1], "http://localhost:")
	out, logs, _ := startRunWithLogs(t, dir, "still works", "--listen", "127.0.0.1:"+port)
	if strings.Contains(out.String(), "/claim#") || !strings.Contains(out.String(), "run dockwell claim --data-dir "+dir+" for a new one") {
		t.Errorf("after a restart:\n%s", out.String())
	}
	if !strings.Contains(logs.String(), `the claim link printed earlier still works","expires":`) || !strings.Contains(logs.String(), `"command":"dockwell claim --data-dir `+dir+`"`) {
		t.Errorf("a restart doesn't log the command that prints a new link as command:\n%s", logs.String())
	}
	claimOverHTTP(t, first)
}

// A restart where the panel answers somewhere else, on another port or a
// domain, prints a new link: the earlier one doesn't work as printed.
func TestARestartOnAnotherAddressPrintsANewLink(t *testing.T) {
	dir := t.TempDir()
	out, stop := startRunWithOutput(t, dir, "/claim#")
	first := linkRE.FindString(out.String())
	stop()

	out, stop = startRunWithOutput(t, dir, "/claim#") // another free port
	second := linkRE.FindString(out.String())
	stop()
	if second == "" || second == first || !strings.Contains(out.String(), "The panel's address changed since the last claim link was printed") {
		t.Fatalf("after a restart on another port:\n%s", out.String())
	}

	// dockwell claim records where its link is printed too: a restart on that
	// address keeps it.
	r := run(t, map[string]string{envLogFormat: "json"}, "claim", "--data-dir", dir)
	third := linkRE.FindString(r.out)
	port := strings.TrimPrefix(linkRE.FindStringSubmatch(third)[1], "http://localhost:")
	out, stop = startRunWithOutput(t, dir, "still works", "--listen", "127.0.0.1:"+port)
	stop()
	if strings.Contains(out.String(), "/claim#") {
		t.Errorf("a restart on the address dockwell claim printed for replaced its link:\n%s", out.String())
	}

	domain := []string{
		"--listen", "127.0.0.1:" + port,
		"--domain", "panel.example.com", "--https-listen", "127.0.0.1:0", "--http-listen", "127.0.0.1:0",
		"--acme-directory-url", "https://acme.invalid/directory",
	}
	out, _ = startRunWithOutput(t, dir, "/claim#", domain...)
	if m := linkRE.FindStringSubmatch(out.String()); len(m) < 2 || !strings.HasPrefix(m[1], "https://panel.example.com:") {
		t.Errorf("after gaining a domain, the new link isn't for it:\n%s", out.String())
	}
}

func TestTheTunnelGoesToTheListenersAddress(t *testing.T) {
	env := func(string) string { return "" }
	for host, want := range map[string]string{
		"":          "ssh -L 7600:127.0.0.1:7600 <user>@203.0.113.7",
		"127.0.0.1": "ssh -L 7600:127.0.0.1:7600 <user>@203.0.113.7",
		"127.0.0.2": "ssh -L 7600:127.0.0.2:7600 <user>@203.0.113.7",
		"::1":       "ssh -L 7600:[::1]:7600 <user>@203.0.113.7",
	} {
		var b strings.Builder
		printLink(&b, panelAddress{Host: host, Port: "7600", PublicHost: "203.0.113.7"}, "/claim#x", env)
		if !strings.Contains(b.String(), want) {
			t.Errorf("listening on %q:\n%s\nwant %q", host, b.String(), want)
		}
	}
	if got := ipOf(&net.TCPAddr{IP: net.IPv6loopback, Port: 7600}); got != "::1" {
		t.Errorf("ipOf([::1]:7600) = %q", got)
	}
}

// Plain HTTP redirects to the port HTTPS is bound to, however
// --https-listen spells it.
func TestTheRedirectGoesToTheBoundPort(t *testing.T) {
	var lc net.ListenConfig
	ln, err := lc.Listen(t.Context(), "tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer ln.Close()
	_, port, _ := net.SplitHostPort(ln.Addr().String())
	for _, listen := range []string{"127.0.0.1:0", ":https", ":" + port} {
		src, err := acmeSource(runFlags{domain: "panel.example.com", httpsListen: listen}, datadir.Dir{Root: t.TempDir()}, ln.Addr())
		if err != nil {
			t.Fatal(err)
		}
		rec := httptest.NewRecorder()
		src.HTTPHandler().ServeHTTP(rec, httptest.NewRequestWithContext(t.Context(), http.MethodGet, "http://panel.example.com/servers", http.NoBody))
		if want := "https://panel.example.com:" + port + "/servers"; rec.Header().Get("Location") != want {
			t.Errorf("--https-listen %s redirects to %q, want %q", listen, rec.Header().Get("Location"), want)
		}
	}
	src, _ := acmeSource(runFlags{domain: "panel.example.com"}, datadir.Dir{Root: t.TempDir()}, &net.TCPAddr{Port: 443})
	rec := httptest.NewRecorder()
	src.HTTPHandler().ServeHTTP(rec, httptest.NewRequestWithContext(t.Context(), http.MethodGet, "http://panel.example.com/x", http.NoBody))
	if rec.Header().Get("Location") != "https://panel.example.com/x" {
		t.Errorf("on 443: %q", rec.Header().Get("Location"))
	}
}

var listenerRE = regexp.MustCompile(`"listener":"control plane","addr":"([^"]+)"`)

// panelPort is the port the panel's listener logs it listens on, once it does.
func panelPort(t *testing.T, logs *syncBuffer) string {
	t.Helper()
	deadline := time.Now().Add(10 * time.Second)
	for {
		if m := listenerRE.FindAllStringSubmatch(logs.String(), -1); len(m) > 0 {
			_, port, _ := net.SplitHostPort(m[len(m)-1][1])
			return port
		}
		if time.Now().After(deadline) {
			t.Fatalf("dockwell run didn't log where it listens:\n%s", logs.String())
		}
		time.Sleep(20 * time.Millisecond)
	}
}

// authState reads GetAuthState from the panel that logs say listens.
func authState(t *testing.T, logs *syncBuffer) *dockwellv1.GetAuthStateResponse {
	t.Helper()
	port := panelPort(t, logs)
	base := "http://localhost:" + port
	var d net.Dialer
	client := &http.Client{Transport: &http.Transport{DialContext: func(ctx context.Context, network, _ string) (net.Conn, error) {
		return d.DialContext(ctx, network, "127.0.0.1:"+port)
	}}}
	c := dockwellv1connect.NewAuthServiceClient(client, base, connect.WithInterceptors(originHeader(base)))
	res, err := c.GetAuthState(t.Context(), connect.NewRequest(&dockwellv1.GetAuthStateRequest{}))
	if err != nil {
		t.Fatalf("GetAuthState: %v", err)
	}
	return res.Msg
}

// A service's standard output goes to the journal, which people other than
// root can read, so dockwell run prints no claim link there: it says to run
// dockwell claim, which prints one where the person running it reads it, and
// the panel shows the same command (#110).
func TestRunAsAServiceKeepsTheClaimLinkOutOfItsOutput(t *testing.T) {
	dir := t.TempDir()
	out, logs, stop := startRunAsService(t, dir)
	command := "dockwell claim --data-dir " + dir
	if strings.Contains(out.String(), "claim#") || strings.Contains(logs.String(), "claim#") {
		t.Fatalf("dockwell run printed a claim link where it isn't a terminal:\n%s\n%s", out.String(), logs.String())
	}
	if !strings.Contains(out.String(), "\n  "+command+"\n") || !strings.Contains(out.String(), "the system journal keeps it") {
		t.Errorf("dockwell run doesn't say how to get a link, and why it prints none:\n%s", out.String())
	}
	if !strings.Contains(logs.String(), `"command":"`+command+`"`) {
		t.Errorf("dockwell run doesn't log the command that prints a link:\n%s", logs.String())
	}
	if got := authState(t, logs).GetClaimCommand(); got != command {
		t.Errorf("the panel's claim command is %q, want %q", got, command)
	}

	r := run(t, map[string]string{envLogFormat: "json"}, "claim", "--data-dir", dir)
	first := linkRE.FindString(r.out)
	if r.code != exitOK || first == "" || !strings.Contains(r.out, "Run "+command+" for a new link.") {
		t.Fatalf("dockwell claim = %d:\n%s%s", r.code, r.out, r.err)
	}
	stop()

	// A restart elsewhere can't say the link works as printed, and doesn't
	// replace it with one nobody can read: the one dockwell claim printed still
	// claims the install, on the new port.
	out, logs, stop = startRunAsService(t, dir)
	if !strings.Contains(out.String(), "The panel's address changed since the last claim link was printed, so that link doesn't work as printed.\n\nThis install has no owner yet.") ||
		strings.Contains(out.String(), "claim#") {
		t.Errorf("a restart on another port as a service:\n%s", out.String())
	}
	moved := regexp.MustCompile(`localhost:\d+`).ReplaceAllString(first, "localhost:"+panelPort(t, logs))
	claimOverHTTP(t, moved)
	if got := authState(t, logs); !got.GetClaimed() || got.GetClaimCommand() != "" {
		t.Errorf("once claimed, GetAuthState says claimed %v and a claim command %q", got.GetClaimed(), got.GetClaimCommand())
	}
	stop()
}

// The command that prints a link is the one that works for how Dockwell runs:
// root has sudo in front, unless there's no sudo, as on Alpine, and keeps
// DOCKWELL_DATABASE through it for a control plane on Postgres.
func TestTheClaimCommandFitsHowDockwellRuns(t *testing.T) {
	defer func(was func() int) { geteuid = was }(geteuid)
	defer func(was func(string) (string, error)) { lookPath = was }(lookPath)
	state := t.TempDir()
	env := func(k string) string { return map[string]string{"XDG_STATE_HOME": state}[k] }
	const pg = "postgres://dockwell@db.internal/dockwell"
	for _, tt := range []struct {
		root, sudo bool
		dir        string
		location   string
		want       string
	}{
		{false, true, filepath.Join(state, "dockwell"), "", "dockwell claim"},
		{false, true, "/srv/dockwell", "", "dockwell claim --data-dir /srv/dockwell"},
		{false, true, "/srv/my dockwell", "", "dockwell claim --data-dir '/srv/my dockwell'"},
		{false, true, filepath.Join(state, "dockwell"), pg, "dockwell claim"},
		{true, true, systemDataDir, "", "sudo dockwell claim"},
		{true, true, systemDataDir + "/", "/var/lib/dockwell/control/state.db", "sudo dockwell claim"},
		{true, true, "/srv/dockwell", "", "sudo dockwell claim --data-dir /srv/dockwell"},
		{true, true, systemDataDir, pg, "sudo --preserve-env=DOCKWELL_DATABASE dockwell claim"},
		{true, false, systemDataDir, "", "dockwell claim"},
		{true, false, systemDataDir, pg, "dockwell claim"},
		{true, false, "/srv/dockwell", "", "dockwell claim --data-dir /srv/dockwell"},
	} {
		geteuid = func() int {
			if tt.root {
				return 0
			}
			return 1000
		}
		lookPath = func(string) (string, error) {
			if tt.sudo {
				return "/usr/bin/sudo", nil
			}
			return "", exec.ErrNotFound
		}
		if got := claimCommand(datadir.Dir{Root: tt.dir}, tt.location, env); got != tt.want {
			t.Errorf("as root %v, with sudo %v, in %s on %q: %q, want %q", tt.root, tt.sudo, tt.dir, tt.location, got, tt.want)
		}
	}
}

// A control plane on Postgres has no state in its directory, and sudo leaves
// DOCKWELL_DATABASE out, so the error says how to keep it.
func TestNoStateSaysSudoLeavesTheDatabaseOut(t *testing.T) {
	dir := layout(t, "control/key")
	r := run(t, map[string]string{envLogFormat: "json"}, "claim", "--data-dir", dir)
	if r.code != exitError || !strings.Contains(r.err, "has no state in") ||
		!strings.Contains(r.err, "sudo leaves DOCKWELL_DATABASE out, so with sudo, keep it: sudo --preserve-env=DOCKWELL_DATABASE") {
		t.Errorf("dockwell claim with no state = %d %q", r.code, r.err)
	}
}

// Words that say what dockwell run does, as it prints a link only to a terminal.
func TestHelpSaysTheLinkGoesOnlyToATerminal(t *testing.T) {
	r := run(t, nil, "run", "--help")
	if !strings.Contains(r.out, "a one-time link that claims it when its output is a terminal") || !strings.Contains(r.out, "sudo dockwell claim") {
		t.Errorf("dockwell run --help says:\n%s", r.out)
	}
}

// A user other than root looks in a directory of their own, so on a server
// where the system service runs Dockwell, dockwell claim looks where it keeps
// its state too, and when only root can, says to use sudo (#110).
func TestClaimAsAnotherUserLooksWhereTheServiceKeepsIt(t *testing.T) {
	if os.Geteuid() == 0 {
		t.Skip("root looks in the system's data directory already")
	}
	system := t.TempDir()
	defer func(was string) { systemDataDir = was }(systemDataDir)
	systemDataDir = system
	_, _, stop := startRunAsService(t, system)
	stop()
	env := map[string]string{"XDG_STATE_HOME": t.TempDir(), envLogFormat: "json"}

	r := run(t, env, "claim")
	if r.code != exitOK || linkRE.FindString(r.out) == "" {
		t.Fatalf("dockwell claim with the control plane in a system directory it can read = %d:\n%s%s", r.code, r.out, r.err)
	}
	// It opens the system's: that one has no owner yet, where the user's would
	// have had no control plane at all.
	r = run(t, env, "login", "--local")
	if r.code != exitError || !strings.Contains(r.err, "this install has no owner yet, so local access has nobody to act for") {
		t.Errorf("dockwell login --local with the control plane in a system directory it can read = %d:\n%s%s", r.code, r.out, r.err)
	}

	// Its directory open to all, and its key to root alone.
	sudo := func(what string) {
		t.Helper()
		for _, command := range [][]string{{"claim"}, {"recover"}, {"token", "create", "--ttl", "1h"}} {
			r = run(t, env, command...)
			name := "dockwell " + strings.Join(command[:min(2, len(command))], " ")
			want := "so " + name + " can't open the control plane as you. Run the same command again with sudo in front of it"
			if r.code != exitError || !strings.Contains(r.err, "keeps its state in "+system+" on this server, where only root can look") || !strings.Contains(r.err, want) {
				t.Errorf("dockwell %v where %s = %d %q, want %q", command, what, r.code, r.err, want)
			}
		}
		r = run(t, env, "login", "--local")
		if r.code != exitError || !strings.Contains(r.err, "so dockwell login --local can't open the control plane as you") {
			t.Errorf("dockwell login --local where %s = %d %q", what, r.code, r.err)
		}
	}
	control := datadir.Dir{Root: system}.Control()
	if err := os.Chmod(control, 0o600); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.Chmod(control, 0o700) })
	sudo("only root can read its key")
	if err := os.Chmod(control, 0o700); err != nil {
		t.Fatal(err)
	}

	if err := os.Chmod(system, 0o600); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.Chmod(system, 0o700) })
	sudo("only root can look")
	// With nothing in the system's directory either, it says what it always said.
	systemDataDir = t.TempDir()
	r = run(t, env, "claim")
	if r.code != exitError || !strings.Contains(r.err, "there's no control plane in") || strings.Contains(r.err, "sudo") {
		t.Errorf("dockwell claim on a server with no control plane = %d %q", r.code, r.err)
	}
}

// sudo dockwell claim for a control plane the user runs themselves says to run
// it as them, as root would leave files there they can't open.
func TestClaimWithSudoForAUsersOwnControlPlane(t *testing.T) {
	defer func(was func() int) { geteuid = was }(geteuid)
	defer func(was func(string) (*user.User, error)) { lookupUser = was }(lookupUser)
	defer func(was string) { systemDataDir = was }(systemDataDir)
	geteuid = func() int { return 0 }
	home := t.TempDir()
	lookupUser = func(name string) (*user.User, error) { return &user.User{Username: name, HomeDir: home}, nil }
	systemDataDir = t.TempDir()
	own := datadir.Dir{Root: filepath.Join(home, ".local", "state", "dockwell")}
	if err := datadir.Ensure(own.Control()); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(own.SealKey(), []byte("key"), 0o600); err != nil {
		t.Fatal(err)
	}
	r := run(t, map[string]string{"SUDO_USER": "mira", envLogFormat: "json"}, "claim")
	if r.code != exitError || !strings.Contains(r.err, "there's no control plane in "+systemDataDir+", but there's one in "+own.Root+", which mira runs") ||
		!strings.Contains(r.err, "Run dockwell claim as mira, without sudo") {
		t.Errorf("sudo dockwell claim for mira's own control plane = %d %q", r.code, r.err)
	}
	r = run(t, map[string]string{envLogFormat: "json"}, "claim")
	if r.code != exitError || strings.Contains(r.err, "mira") {
		t.Errorf("dockwell claim as root, not through sudo = %d %q", r.code, r.err)
	}
}

// installSummary runs the install script's summary for the control plane at
// panel, as root would with its output going to a log rather than a terminal,
// and returns what it printed. BIN is no dockwell at all, so a claim link made
// there would fail it.
func installSummary(t *testing.T, data, panel string) string {
	t.Helper()
	if _, err := exec.LookPath("curl"); err != nil {
		t.Skip("there's no curl here for the install script to ask the panel with")
	}
	// A server with sudo, as most have, so what the script would say of its
	// own differs from what the control plane says, as it runs here.
	bin := t.TempDir()
	if err := os.WriteFile(filepath.Join(bin, "sudo"), []byte("#!/bin/sh\nexit 1\n"), 0o700); err != nil {
		t.Fatal(err)
	}
	cmd := exec.CommandContext(t.Context(), "sh", "-c", `. "$1"; BIN=/nonexistent/dockwell; DATA=$2; version=9.9.9; downloader=curl; tmp=$(mktemp -d); control_summary "$3"`,
		"sh", installSteps(t), data, panel)
	cmd.Env = append(os.Environ(), "PATH="+bin+string(os.PathListSeparator)+os.Getenv("PATH"))
	out, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("the install script's summary failed: %v\n%s", err, out)
	}
	return string(out)
}

// cloud-init, a tee or Ansible keep the install script's output, so it prints a
// claim link only to a terminal. Elsewhere it says the install has no owner yet,
// and the command the control plane gives that prints a link, and makes none
// (#110). Once someone has claimed it, it says where the panel is.
func TestTheInstallScriptKeepsTheClaimLinkOutOfALog(t *testing.T) {
	dir := t.TempDir()
	_, logs, _ := startRunAsService(t, dir)
	panel := "http://127.0.0.1:" + panelPort(t, logs)
	out := installSummary(t, dir, panel)
	for _, want := range []string{
		"Dockwell v9.9.9 is running, and this is your control server. This install has no owner yet. To claim it, run this on this server, which prints a one-time link:",
		"\n  dockwell claim --data-dir " + dir + "\n",
		"The script doesn't print the link here, as its output isn't a terminal",
		"To add a server, run dockwell token create here",
	} {
		if !strings.Contains(out, want) {
			t.Errorf("the summary doesn't say %q:\n%s", want, out)
		}
	}
	if strings.Contains(out, "claim#") {
		t.Errorf("the summary printed a claim link where it isn't a terminal:\n%s", out)
	}

	r := run(t, map[string]string{envLogFormat: "json"}, "claim", "--data-dir", dir)
	claimOverHTTP(t, regexp.MustCompile(`localhost:\d+`).ReplaceAllString(linkRE.FindString(r.out), "localhost:"+panelPort(t, logs)))
	if out := installSummary(t, dir, panel); !strings.Contains(out, "Its panel\nlistens on "+panel) || strings.Contains(out, "no owner") {
		t.Errorf("once claimed, the summary says:\n%s", out)
	}

	// A panel that doesn't answer says nothing of an owner, so neither does this.
	out = installSummary(t, dir, "http://127.0.0.1:1")
	if !strings.Contains(out, "didn't say whether this install has an owner. If it has none yet, run this on this server") ||
		!strings.Contains(out, "\n  sudo dockwell claim --data-dir "+dir+"\n") {
		t.Errorf("with no panel answering, the summary says:\n%s", out)
	}
}

// A control plane that has never started has no address to print a link for.
func TestAControlPlaneThatHasntStartedSaysSo(t *testing.T) {
	dir := datadir.Dir{Root: t.TempDir()}
	if err := datadir.Ensure(dir.Control()); err != nil {
		t.Fatal(err)
	}
	if _, err := seal.LoadOrCreate(dir.SealKey()); err != nil {
		t.Fatal(err)
	}
	db, err := store.Open(t.Context(), dir.StateDB())
	if err != nil {
		t.Fatal(err)
	}
	_ = db.Close()
	r := run(t, map[string]string{envLogFormat: "json"}, "claim", "--data-dir", dir.Root)
	if r.code != exitError || !strings.Contains(r.err, "the control plane here hasn't started yet, so there's nothing to open. Start it, with dockwell run or its service, then run this again") {
		t.Errorf("dockwell claim before the control plane started = %d %q", r.code, r.err)
	}
}
