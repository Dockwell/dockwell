// SPDX-License-Identifier: AGPL-3.0-only

//go:build !agent

package cli

import (
	"context"
	"net"
	"net/http"
	"net/http/httptest"
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
// run logged.
func startRunWithLogs(t *testing.T, dir, want string, args ...string) (out, logs *syncBuffer, stop func()) {
	t.Helper()
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan int, 1)
	out = &syncBuffer{}
	errOut := &syncBuffer{}
	all := append([]string{"run", "--listen", "127.0.0.1:0", "--agent-listen", "127.0.0.1:0", "--data-dir", dir, "--log-format", "json"}, args...)
	go func() {
		done <- Execute(ctx, all, IO{Out: out, Err: errOut, Getenv: func(k string) string {
			return map[string]string{"USER": "mira", "SSH_CONNECTION": "198.51.100.23 52000 203.0.113.7 22"}[k]
		}})
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
		"Run dockwell claim for a new link.",
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
	addr := panelAddress{Port: "7600", PublicHost: "203.0.113.7"}
	for _, tt := range []struct {
		env  map[string]string
		want string
	}{
		{nil, "root@203.0.113.7"},
		{map[string]string{"USER": "mira"}, "mira@203.0.113.7"},
		{map[string]string{"USER": "root", "SUDO_USER": "mira"}, "mira@203.0.113.7"},
		{map[string]string{"USER": "mira", "SSH_CONNECTION": "198.51.100.23 52000 10.0.0.5 22"}, "mira@10.0.0.5"},
		{map[string]string{"SSH_CONNECTION": "2001:db8::9 52000 2001:db8::1 22"}, "root@[2001:db8::1]"},
	} {
		if got := sshTarget(addr, env(tt.env)); got != tt.want {
			t.Errorf("sshTarget(%v) = %q, want %q", tt.env, got, tt.want)
		}
	}
	if got := sshTarget(panelAddress{}, env(nil)); got != "root@your-server" {
		t.Errorf("with no address known: %q", got)
	}
	if got := sshTarget(panelAddress{PublicHost: "127.0.0.1"}, env(nil)); got != "root@your-server" {
		t.Errorf("with only a loopback address: %q", got)
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
		printClaim(&b, addr, auth.Link{Token: "x", ExpiresAt: time.Now().Add(time.Hour)}, func(string) string { return "" })
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
	out, _ = startRunWithOutput(t, dir, "still works", "--listen", "127.0.0.1:"+port)
	if strings.Contains(out.String(), "/claim#") || !strings.Contains(out.String(), "run dockwell claim for a new one") {
		t.Errorf("after a restart:\n%s", out.String())
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
		"":          "ssh -L 7600:127.0.0.1:7600 root@203.0.113.7",
		"127.0.0.1": "ssh -L 7600:127.0.0.1:7600 root@203.0.113.7",
		"127.0.0.2": "ssh -L 7600:127.0.0.2:7600 root@203.0.113.7",
		"::1":       "ssh -L 7600:[::1]:7600 root@203.0.113.7",
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
