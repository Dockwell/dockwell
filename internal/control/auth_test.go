// SPDX-License-Identifier: AGPL-3.0-only

package control

import (
	"context"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/tls"
	"crypto/x509"
	"crypto/x509/pkix"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"math/big"
	"net"
	"net/http"
	"net/http/cookiejar"
	"net/netip"
	"net/url"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"connectrpc.com/connect"
	"connectrpc.com/grpchealth"
	"google.golang.org/protobuf/reflect/protoreflect"
	"google.golang.org/protobuf/reflect/protoregistry"
	"google.golang.org/protobuf/types/dynamicpb"

	dockwellv1 "github.com/dockwell/dockwell/gen/dockwell/v1"
	"github.com/dockwell/dockwell/gen/dockwell/v1/dockwellv1connect"
	"github.com/dockwell/dockwell/internal/auth"
	"github.com/dockwell/dockwell/internal/auth/authtest"
	"github.com/dockwell/dockwell/internal/control/agentapi"
	"github.com/dockwell/dockwell/internal/control/authapi"
	"github.com/dockwell/dockwell/internal/control/fleet"
	"github.com/dockwell/dockwell/internal/control/identity"
	logapi "github.com/dockwell/dockwell/internal/control/logs"
	"github.com/dockwell/dockwell/internal/health"
	"github.com/dockwell/dockwell/internal/https"
	"github.com/dockwell/dockwell/internal/resource"
	"github.com/dockwell/dockwell/internal/resource/backuppolicy"
	"github.com/dockwell/dockwell/internal/resource/container"
	"github.com/dockwell/dockwell/internal/resource/firewall"
	"github.com/dockwell/dockwell/internal/resource/network"
	"github.com/dockwell/dockwell/internal/resource/route"
	"github.com/dockwell/dockwell/internal/resource/service"
	"github.com/dockwell/dockwell/internal/resource/variable"
	"github.com/dockwell/dockwell/internal/resource/volume"
	"github.com/dockwell/dockwell/internal/revision"
	"github.com/dockwell/dockwell/internal/seal"
	"github.com/dockwell/dockwell/internal/secrets"
	"github.com/dockwell/dockwell/internal/secrets/secretstest"
	"github.com/dockwell/dockwell/internal/store"
	"github.com/dockwell/dockwell/internal/store/storetest"
	"github.com/dockwell/dockwell/internal/task"
)

// fullServer is a control plane with sign-in, as dockwell run starts it.
// fakeNames is what DNS says of each name, for a test, as one never asks the world.
type fakeNames struct {
	mu sync.Mutex
	m  map[string][]netip.Addr
}

func (n *fakeNames) set(name string, addresses ...string) {
	n.mu.Lock()
	defer n.mu.Unlock()
	n.m[name] = nil
	for _, s := range addresses {
		n.m[name] = append(n.m[name], netip.MustParseAddr(s))
	}
}

func (n *fakeNames) lookup(_ context.Context, name string) ([]netip.Addr, error) {
	n.mu.Lock()
	defer n.mu.Unlock()
	if found, ok := n.m[name]; ok {
		return found, nil
	}
	return nil, &net.DNSError{Err: "no such host", Name: name, IsNotFound: true}
}

type fullServer struct {
	// names is what DNS says of each name.
	names *fakeNames
	// config is what it was started with, and kinds what it runs, for a restart.
	config Config
	kinds  *resource.Registry
	auth   *auth.Service
	engine *revision.Engine
	agents *agentapi.Agents
	keeper *secrets.Keeper
	ident  *identity.Authority
	store  *store.DB
	// agentAddr is the agent listener's address.
	agentAddr string
	db        string // where the database is, for store.Open
	api       string // the loopback listener's address
	https     string // the HTTPS listener's address, with a domain
	http      string // the plain-HTTP listener's address, with a domain
	// stop stops the server, as a signal does, and returns what Run did.
	stop func() error
	// server is the control plane itself.
	server *Server
	// lastDocker is the server addDockerServer last added.
	lastDocker *testServer
	// onePanelRoot is where the next Docker server added has 1Panel installed, if anywhere.
	onePanelRoot string
	// volumes wraps the volume driver of the next server added, to see what its
	// agent hears. Nil leaves it as it is.
	volumes func(*volume.Driver) resource.Type
	// probe makes the checks an agent makes of a container on the next server added,
	// as a fake engine's containers listen nowhere. Nil dials them.
	probe func(ctx context.Context, c *health.Check, address string) (health.Result, error)
}

// execSQL runs a statement on the server's database from outside the store,
// to make its reads or writes fail as a damaged disk or a bug elsewhere would.
func (f *fullServer) execSQL(t *testing.T, query string) {
	t.Helper()
	storetest.Exec(t, f.db, query)
}

// askWait is how long these tests give a step-up that was asked for.
const askWait = 200 * time.Millisecond

// testRecheck is how often the next control plane started rechecks a session for
// what's open on it. Zero keeps auth's own. A test that sets it puts it back.
var testRecheck time.Duration

func startFull(t *testing.T, backend store.Backend, public https.Source) *fullServer {
	t.Helper()
	return startFullLogging(t, backend, public, slog.New(slog.DiscardHandler))
}

// startFullLogging is startFull with the control plane logging to logger.
func startFullLogging(t *testing.T, backend store.Backend, public https.Source, logger *slog.Logger) *fullServer {
	t.Helper()
	return startFullWith(t, backend, public, logger, nil, logapi.Options{})
}

// startFullWith is startFull with the control plane logging to logger, serving
// the panel's proxied listener on ingress if it isn't nil, and with the limits
// of reading logs changed.
func startFullWith(t *testing.T, backend store.Backend, public https.Source, logger *slog.Logger, ingress net.Listener, logs logapi.Options) *fullServer {
	t.Helper()
	dir := t.TempDir()
	key, err := seal.LoadOrCreate(filepath.Join(dir, "key"))
	if err != nil {
		t.Fatal(err)
	}
	db, path := storetest.Open(t, backend)
	ident, err := identity.Bootstrap(t.Context(), db, key, logger)
	if err != nil {
		t.Fatal(err)
	}
	hub, err := fleet.New(t.Context(), db, logger)
	if err != nil {
		t.Fatal(err)
	}
	var sites auth.Sites
	if public != nil {
		sites.Domain = public.Domain()
	}
	people, err := auth.New(auth.Config{DB: db, Key: key, Logger: logger, Sites: sites, AskWait: askWait, Recheck: testRecheck})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := revision.Setup(t.Context(), db, logger); err != nil {
		t.Fatal(err)
	}
	kinds := resource.NewRegistry(&container.Driver{}, &firewall.Driver{}, &service.Driver{}, &volume.Driver{}, &network.Driver{}, &route.Driver{}, variable.Type{}, variable.SecretType{}, backuppolicy.Type{}, backuppolicy.Destination{}, &task.Driver{})
	keeper := secretstest.Keeper(db, kinds)
	agents := agentapi.NewAgents(kinds, logger)
	engine := revision.New(db, kinds, agents, keeper, logger)
	engine.RequireRoles()
	agents.OnConnect(func(string) { _ = engine.Resume(context.Background()) })
	t.Cleanup(engine.Wait)
	// No test resolves a real name: what each says is a name's is set on f.names.
	names := &fakeNames{m: map[string][]netip.Addr{}}
	listen := func() net.Listener {
		var lc net.ListenConfig
		ln, err := lc.Listen(t.Context(), "tcp", "127.0.0.1:0")
		if err != nil {
			t.Fatal(err)
		}
		return ln
	}
	ls := Listeners{API: listen(), Agents: listen(), Ingress: ingress}
	config := Config{
		DNSLookup: names.lookup,
		Logger:    logger, Build: testBuild, Panel: testPanel, Identity: ident, Fleet: hub, Auth: people, Public: public,
		Changes: engine, Agents: agents, Store: db, Secrets: keeper, AgentHosts: []string{"127.0.0.1"}, StepGrace: 200 * time.Millisecond,
		EventPoll: 200 * time.Millisecond, Logs: logs,
		DataDir: dir, KeyFile: filepath.Join(dir, "key"), AgentListen: ls.Agents.Addr().String(),
	}
	if public != nil {
		// As dockwell run says it of a domain with plain HTTP on port 80 and HTTPS on 8443: the ingress takes the
		// public ports while there are routes, and serves the domain on 443.
		config.PanelURL, config.PanelCA, config.PanelHandsOver = "https://"+public.Domain()+":8443", "Let's Encrypt", true
	}
	srv, err := New(config)
	if err != nil {
		t.Fatal(err)
	}
	f := &fullServer{
		names: names,
		auth:  people, engine: engine, agents: agents, keeper: keeper, ident: ident, store: db, db: path,
		api: ls.API.Addr().String(), agentAddr: ls.Agents.Addr().String(), server: srv, config: config, kinds: kinds,
	}
	if public != nil {
		ls.HTTPS, ls.HTTP = listen(), listen()
		f.https, f.http = ls.HTTPS.Addr().String(), ls.HTTP.Addr().String()
	}
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() { done <- srv.Run(ctx, ls) }()
	var once sync.Once
	var runErr error
	f.stop = func() error {
		once.Do(func() {
			cancel()
			runErr = <-done
		})
		return runErr
	}
	t.Cleanup(func() {
		if err := f.stop(); err != nil {
			t.Errorf("Run returned %v after cancel", err)
		}
	})
	return f
}

// browser is an HTTP client that behaves like one: it keeps cookies, dials
// the listener whatever the URL's host, and says which origin it is on.
type browser struct {
	origin    string
	client    *http.Client
	auth      *authtest.Authenticator
	userAgent string
}

func newBrowser(t *testing.T, origin, dialAddr string, roots *x509.CertPool) *browser {
	t.Helper()
	jar, _ := cookiejar.New(nil)
	var d net.Dialer
	tr := &http.Transport{
		DialContext: func(ctx context.Context, network, _ string) (net.Conn, error) {
			return d.DialContext(ctx, network, dialAddr)
		},
		TLSClientConfig: &tls.Config{RootCAs: roots, MinVersion: tls.VersionTLS12},
	}
	return &browser{origin: origin, client: &http.Client{Jar: jar, Transport: tr}, auth: &authtest.Authenticator{}}
}

// Do sets Origin and Sec-Fetch-Site the way a browser does for the panel's
// own requests.
func (b *browser) Do(r *http.Request) (*http.Response, error) {
	r.Header.Set("Origin", b.origin)
	r.Header.Set("Sec-Fetch-Site", "same-origin")
	if b.userAgent != "" {
		r.Header.Set("User-Agent", b.userAgent)
	}
	return b.client.Do(r)
}

func (b *browser) authClient() dockwellv1connect.AuthServiceClient {
	return dockwellv1connect.NewAuthServiceClient(b, b.origin, connect.WithProtoJSON())
}

func (b *browser) cookies() []*http.Cookie {
	u, _ := url.Parse(b.origin)
	return b.client.Jar.Cookies(u)
}

// claim claims the install through the API, as the panel does.
func (b *browser) claim(t *testing.T, token string) *connect.Response[dockwellv1.FinishClaimResponse] {
	t.Helper()
	c := b.authClient()
	if _, err := c.CheckClaim(t.Context(), connect.NewRequest(&dockwellv1.CheckClaimRequest{Token: token})); err != nil {
		t.Fatalf("CheckClaim: %v", err)
	}
	begin, err := c.BeginClaim(t.Context(), connect.NewRequest(&dockwellv1.BeginClaimRequest{Token: token, Name: "Mira Chen", Email: "mira@acme.dev"}))
	if err != nil {
		t.Fatalf("BeginClaim: %v", err)
	}
	cred, _, err := b.auth.Create(begin.Msg.GetOptionsJson(), b.origin)
	if err != nil {
		t.Fatal(err)
	}
	res, err := c.FinishClaim(t.Context(), connect.NewRequest(&dockwellv1.FinishClaimRequest{
		CeremonyId: begin.Msg.GetCeremonyId(), Token: token, CredentialJson: cred, PasskeyName: "Chrome on Linux",
	}))
	if err != nil {
		t.Fatalf("FinishClaim: %v", err)
	}
	return res
}

func (b *browser) signIn(t *testing.T) {
	t.Helper()
	c := b.authClient()
	begin, err := c.BeginPasskeySignIn(t.Context(), connect.NewRequest(&dockwellv1.BeginPasskeySignInRequest{}))
	if err != nil {
		t.Fatal(err)
	}
	resp, err := b.auth.Get(begin.Msg.GetOptionsJson(), b.origin, nil)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := c.FinishPasskeySignIn(t.Context(), connect.NewRequest(&dockwellv1.FinishPasskeySignInRequest{
		CeremonyId: begin.Msg.GetCeremonyId(), CredentialJson: resp,
	})); err != nil {
		t.Fatalf("FinishPasskeySignIn: %v", err)
	}
}

func TestClaimSignOutAndSignInOverLocalhost(t *testing.T) {
	storetest.Each(t, testClaimSignOutAndSignInOverLocalhost)
}

func testClaimSignOutAndSignInOverLocalhost(t *testing.T, backend store.Backend) {
	srv := startFull(t, backend, nil)
	_, port, _ := net.SplitHostPort(srv.api)
	b := newBrowser(t, "http://localhost:"+port, srv.api, nil)
	c := b.authClient()

	state, err := c.GetAuthState(t.Context(), connect.NewRequest(&dockwellv1.GetAuthStateRequest{}))
	if err != nil {
		t.Fatal(err)
	}
	if state.Msg.GetClaimed() || state.Msg.GetUser() != nil || state.Msg.GetRpId() != "localhost" {
		t.Errorf("before the claim: %v", state.Msg)
	}
	link, err := srv.auth.CreateClaimLink(t.Context(), "test")
	if err != nil {
		t.Fatal(err)
	}
	res := b.claim(t, link.Token)
	cookie := res.Header().Get("Set-Cookie")
	for _, want := range []string{"dockwell_session=", "HttpOnly", "SameSite=Strict", "Path=/"} {
		if !strings.Contains(cookie, want) {
			t.Errorf("session cookie %q lacks %q", cookie, want)
		}
	}
	if strings.Contains(cookie, "Secure") || strings.Contains(cookie, "__Host-") {
		t.Errorf("a localhost cookie %q is marked for HTTPS", cookie)
	}

	state, _ = c.GetAuthState(t.Context(), connect.NewRequest(&dockwellv1.GetAuthStateRequest{}))
	if !state.Msg.GetClaimed() || state.Msg.GetUser().GetEmail() != "mira@acme.dev" ||
		state.Msg.GetSignInMethod() != dockwellv1.SignInMethod_SIGN_IN_METHOD_CLAIM {
		t.Errorf("after the claim: %v", state.Msg)
	}
	// A second claim with the same link fails.
	_, err = c.CheckClaim(t.Context(), connect.NewRequest(&dockwellv1.CheckClaimRequest{Token: link.Token}))
	var cerr *connect.Error
	if !errors.As(err, &cerr) || cerr.Code() != connect.CodeFailedPrecondition || cerr.Meta().Get(authapi.ReasonKey) != "claimed" {
		t.Errorf("CheckClaim after claiming = %v, want failed_precondition with reason claimed", err)
	}

	sessions, err := c.ListSessions(t.Context(), connect.NewRequest(&dockwellv1.ListSessionsRequest{}))
	if err != nil || len(sessions.Msg.GetSessions()) != 1 || !sessions.Msg.GetSessions()[0].GetCurrent() {
		t.Fatalf("ListSessions = %v, %v", sessions, err)
	}

	out, err := c.SignOut(t.Context(), connect.NewRequest(&dockwellv1.SignOutRequest{}))
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(out.Header().Get("Set-Cookie"), "Max-Age=0") {
		t.Errorf("SignOut cookie = %q, want it cleared", out.Header().Get("Set-Cookie"))
	}
	if _, err := c.ListSessions(t.Context(), connect.NewRequest(&dockwellv1.ListSessionsRequest{})); connect.CodeOf(err) != connect.CodeUnauthenticated {
		t.Errorf("ListSessions after signing out = %v, want unauthenticated", err)
	}

	b.signIn(t)
	state, _ = c.GetAuthState(t.Context(), connect.NewRequest(&dockwellv1.GetAuthStateRequest{}))
	if state.Msg.GetSignInMethod() != dockwellv1.SignInMethod_SIGN_IN_METHOD_PASSKEY {
		t.Errorf("after signing in again: %v", state.Msg)
	}
	servers := dockwellv1connect.NewServerServiceClient(b, b.origin)
	if _, err := servers.ListServers(t.Context(), connect.NewRequest(&dockwellv1.ListServersRequest{})); err != nil {
		t.Errorf("ListServers when signed in: %v", err)
	}
}

// A browser can send any bytes as its User-Agent. The session list still
// answers.
func TestSessionsListWithAnyUserAgent(t *testing.T) {
	storetest.Each(t, testSessionsListWithAnyUserAgent)
}

func testSessionsListWithAnyUserAgent(t *testing.T, backend store.Backend) {
	srv := startFull(t, backend, nil)
	_, port, _ := net.SplitHostPort(srv.api)
	b := newBrowser(t, "http://localhost:"+port, srv.api, nil)
	b.userAgent = "Mozilla/5.0 \xff\xfe " + strings.Repeat("é", 400)
	link, _ := srv.auth.CreateClaimLink(t.Context(), "test")
	b.claim(t, link.Token)
	list, err := b.authClient().ListSessions(t.Context(), connect.NewRequest(&dockwellv1.ListSessionsRequest{}))
	if err != nil || len(list.Msg.GetSessions()) != 1 || !strings.HasPrefix(list.Msg.GetSessions()[0].GetUserAgent(), "Mozilla/5.0 \uFFFD é") {
		t.Errorf("ListSessions = %v, %v", list, err)
	}
}

// TestEveryProcedureNeedsASession calls every procedure the API listener
// serves without a session. Only the public ones may answer; a new RPC is
// refused until someone adds it to the allowlist on purpose.
func TestEveryProcedureNeedsASession(t *testing.T) {
	storetest.Each(t, testEveryProcedureNeedsASession)
}

func testEveryProcedureNeedsASession(t *testing.T, backend store.Backend) {
	srv := startFull(t, backend, nil)
	_, port, _ := net.SplitHostPort(srv.api)
	b := newBrowser(t, "http://localhost:"+port, srv.api, nil)
	// Bidi streams need HTTP/2, unencrypted on loopback.
	h2c := newBrowser(t, b.origin, srv.api, nil)
	var p http.Protocols
	p.SetUnencryptedHTTP2(true)
	h2c.client.Transport.(*http.Transport).Protocols = &p
	public := map[string]bool{}
	for _, p := range authapi.PublicProcedures() {
		public[p] = true
	}

	var checked int
	check := func(served string, sd protoreflect.ServiceDescriptor) {
		for i := range sd.Methods().Len() {
			md := sd.Methods().Get(i)
			procedure := "/" + served + "/" + string(md.Name())
			via := b
			if md.IsStreamingClient() {
				via = h2c
			}
			err := callEmptyWith(t, via, via.origin, procedure, md)
			checked++
			unauthenticated := err != nil && connect.CodeOf(err) == connect.CodeUnauthenticated
			switch {
			case public[procedure] && unauthenticated:
				t.Errorf("%s is public but answered unauthenticated", procedure)
			case !public[procedure] && !unauthenticated:
				t.Errorf("%s %s without a session, want unauthenticated", procedure, outcome(err))
			}
		}
	}
	protoregistry.GlobalFiles.RangeFiles(func(fd protoreflect.FileDescriptor) bool {
		if fd.Package() == "dockwell.v1" {
			for i := range fd.Services().Len() {
				sd := fd.Services().Get(i)
				check(string(sd.FullName()), sd)
			}
		}
		return true
	})
	// connect's health and reflection handlers register their schemas under
	// a private package name, and serve them under the standard one.
	for served, registered := range map[string]string{
		grpchealth.HealthV1ServiceName:             "connectext.grpc.health.v1.Health",
		"grpc.reflection.v1.ServerReflection":      "connectext.grpc.reflection.v1.ServerReflection",
		"grpc.reflection.v1alpha.ServerReflection": "connectext.grpc.reflection.v1.ServerReflection",
	} {
		d, err := protoregistry.GlobalFiles.FindDescriptorByName(protoreflect.FullName(registered))
		if err != nil {
			t.Fatalf("%s isn't registered: %v", registered, err)
		}
		check(served, d.(protoreflect.ServiceDescriptor))
	}
	if checked < 25 {
		t.Errorf("checked only %d procedures", checked)
	}

	// The REST paths reach the same procedures, through Vanguard.
	var paths int
	for _, m := range restMethods(t) {
		status, why, body := callREST(t, b, m.verb, m.path, nil, "")
		paths++
		switch {
		case public[m.procedure] && status == http.StatusUnauthorized:
			t.Errorf("%s %s (%s) is public but answered 401: %s", m.verb, m.path, m.procedure, body)
		case !public[m.procedure] && (status != http.StatusUnauthorized || why != authapi.ReasonUnauthenticated):
			t.Errorf("%s %s (%s) answered %d (reason %q) without a session, want 401 unauthenticated: %s", m.verb, m.path, m.procedure, status, why, body)
		}
	}
	if paths < 25 {
		t.Errorf("checked only %d REST paths", paths)
	}
}

// outcome says how a call ended, for a test's message: a success says so,
// where connect.CodeOf would call it unknown.
func outcome(err error) string {
	if err == nil {
		return "succeeded"
	}
	return fmt.Sprintf("answered %v", connect.CodeOf(err))
}

// callEmptyWith calls a procedure through client with an empty request,
// whatever its shape, and returns the error it ends with: nil when it
// answered at all.
func callEmptyWith(t *testing.T, b connect.HTTPClient, origin, procedure string, md protoreflect.MethodDescriptor) error {
	t.Helper()
	ctx, cancel := context.WithTimeout(t.Context(), 5*time.Second)
	defer cancel()
	target := origin + procedure
	newOut := connect.WithResponseInitializer(func(_ connect.Spec, msg any) error {
		*msg.(*dynamicpb.Message) = *dynamicpb.NewMessage(md.Output())
		return nil
	})
	client := connect.NewClient[dynamicpb.Message, dynamicpb.Message](b, target, connect.WithSchema(md), newOut, connect.WithProtoJSON())
	in := dynamicpb.NewMessage(md.Input())
	var err error
	switch {
	case md.IsStreamingClient():
		stream := client.CallBidiStream(ctx)
		// When the server has already answered, Send reports io.EOF, and
		// the answer comes from Receive.
		if err = stream.Send(in); err == nil || errors.Is(err, io.EOF) {
			_ = stream.CloseRequest()
			_, err = stream.Receive()
		}
		_ = stream.CloseResponse()
	case md.IsStreamingServer():
		stream, serr := client.CallServerStream(ctx, connect.NewRequest(in))
		if err = serr; err == nil {
			// A watch that answers at all has passed the guard.
			if stream.Receive() {
				_ = stream.Close()
				return nil
			}
			err = stream.Err()
			_ = stream.Close()
		}
	default:
		_, err = client.CallUnary(ctx, connect.NewRequest(in))
	}
	if err == nil || errors.Is(err, io.EOF) {
		return nil
	}
	return err
}

func TestOriginAndHostChecks(t *testing.T) { storetest.Each(t, testOriginAndHostChecks) }

func testOriginAndHostChecks(t *testing.T, backend store.Backend) {
	srv := startFull(t, backend, nil)
	_, port, _ := net.SplitHostPort(srv.api)
	origin := "http://localhost:" + port
	b := newBrowser(t, origin, srv.api, nil)
	link, _ := srv.auth.CreateClaimLink(t.Context(), "test")
	b.claim(t, link.Token)
	cookie := b.cookies()[0]

	post := func(headers map[string]string) (int, string, string) {
		req, _ := http.NewRequestWithContext(t.Context(), http.MethodPost, origin+dockwellv1connect.AuthServiceListSessionsProcedure, strings.NewReader("{}"))
		req.Header.Set("Content-Type", "application/json")
		req.AddCookie(cookie)
		for k, v := range headers {
			if k == "Host" {
				req.Host = v
				continue
			}
			req.Header.Set(k, v)
		}
		res, err := b.client.Transport.RoundTrip(req)
		if err != nil {
			t.Fatal(err)
		}
		defer res.Body.Close()
		body, _ := io.ReadAll(res.Body)
		return res.StatusCode, string(body), res.Header.Get(authapi.ReasonKey)
	}
	for _, tt := range []struct {
		name    string
		headers map[string]string
		status  int
	}{
		{"the panel's own origin", map[string]string{"Origin": origin}, http.StatusOK},
		{"same-origin fetch metadata only", map[string]string{"Sec-Fetch-Site": "same-origin"}, http.StatusOK},
		{"another site", map[string]string{"Origin": "https://evil.test"}, http.StatusForbidden},
		{"another local port", map[string]string{"Origin": "http://localhost:3000", "Sec-Fetch-Site": "same-site"}, http.StatusForbidden},
		{"cross-site fetch metadata", map[string]string{"Sec-Fetch-Site": "cross-site"}, http.StatusForbidden},
		{"a null origin", map[string]string{"Origin": "null"}, http.StatusForbidden},
		{"a cookie with no origin at all", nil, http.StatusForbidden},
		{"DNS rebinding", map[string]string{"Host": "evil.test:" + port, "Origin": "http://evil.test:" + port}, http.StatusMisdirectedRequest},
	} {
		t.Run(tt.name, func(t *testing.T) {
			status, body, why := post(tt.headers)
			if status != tt.status {
				t.Errorf("status %d, want %d: %s", status, tt.status, body)
			}
			want := map[int]string{http.StatusForbidden: authapi.ReasonOrigin, http.StatusMisdirectedRequest: authapi.ReasonMisdirected}[tt.status]
			if why != want {
				t.Errorf("reason %q, want %q", why, want)
			}
		})
	}

	// Following a link to the panel from another site loads the page.
	nav, _ := http.NewRequestWithContext(t.Context(), http.MethodGet, origin+"/servers", http.NoBody)
	nav.Header.Set("Accept", "text/html")
	nav.Header.Set("Sec-Fetch-Site", "cross-site")
	navRes, err := b.client.Transport.RoundTrip(nav)
	if err != nil {
		t.Fatal(err)
	}
	_ = navRes.Body.Close()
	if navRes.StatusCode != http.StatusOK {
		t.Errorf("a cross-site navigation to the panel = %d, want 200", navRes.StatusCode)
	}

	// Reads may go as GET, which browsers send without Origin.
	req, _ := http.NewRequestWithContext(t.Context(), http.MethodGet,
		origin+dockwellv1connect.AuthServiceListSessionsProcedure+"?encoding=json&message=%7B%7D", http.NoBody)
	req.AddCookie(cookie)
	res, err := b.client.Transport.RoundTrip(req)
	if err != nil {
		t.Fatal(err)
	}
	_ = res.Body.Close()
	if res.StatusCode != http.StatusOK || res.Header.Get("Cache-Control") != "no-store" {
		t.Errorf("a GET read = %d, Cache-Control %q", res.StatusCode, res.Header.Get("Cache-Control"))
	}
}

func TestPageLoadsOnALoopbackIPGoToLocalhost(t *testing.T) {
	storetest.Each(t, testPageLoadsOnALoopbackIPGoToLocalhost)
}

func testPageLoadsOnALoopbackIPGoToLocalhost(t *testing.T, backend store.Backend) {
	srv := startFull(t, backend, nil)
	req, _ := http.NewRequestWithContext(t.Context(), http.MethodGet, "http://"+srv.api+"/claim", http.NoBody)
	req.Header.Set("Accept", "text/html")
	req.Header.Set("Sec-Fetch-Mode", "navigate")
	res, err := http.DefaultTransport.RoundTrip(req)
	if err != nil {
		t.Fatal(err)
	}
	_ = res.Body.Close()
	_, port, _ := net.SplitHostPort(srv.api)
	if res.StatusCode != http.StatusTemporaryRedirect || res.Header.Get("Location") != "http://localhost:"+port+"/claim" {
		t.Errorf("page load on 127.0.0.1 = %d to %q", res.StatusCode, res.Header.Get("Location"))
	}
	// Tools that aren't browsers get the page where they asked.
	req, _ = http.NewRequestWithContext(t.Context(), http.MethodGet, "http://"+srv.api+"/claim", http.NoBody)
	req.Header.Set("Accept", "text/html")
	res, err = http.DefaultTransport.RoundTrip(req)
	if err != nil {
		t.Fatal(err)
	}
	_ = res.Body.Close()
	if res.StatusCode != http.StatusOK {
		t.Errorf("curl's page load on 127.0.0.1 = %d, want 200", res.StatusCode)
	}
	// The API still answers on the IP, for the CLI and curl.
	req, _ = http.NewRequestWithContext(t.Context(), http.MethodGet, "http://"+srv.api+"/healthz", http.NoBody)
	res, err = http.DefaultTransport.RoundTrip(req)
	if err != nil {
		t.Fatal(err)
	}
	_ = res.Body.Close()
	if res.StatusCode != http.StatusOK {
		t.Errorf("/healthz on 127.0.0.1 = %d", res.StatusCode)
	}
}

func TestStreamsEndWhenTheirSessionIsRevoked(t *testing.T) {
	storetest.Each(t, testStreamsEndWhenTheirSessionIsRevoked)
}

func testStreamsEndWhenTheirSessionIsRevoked(t *testing.T, backend store.Backend) {
	srv := startFull(t, backend, nil)
	_, port, _ := net.SplitHostPort(srv.api)
	origin := "http://localhost:" + port
	laptop := newBrowser(t, origin, srv.api, nil)
	link, _ := srv.auth.CreateClaimLink(t.Context(), "test")
	laptop.claim(t, link.Token)
	phone := newBrowser(t, origin, srv.api, nil)
	phone.auth = laptop.auth // the same synced passkey
	phone.signIn(t)

	stream, err := dockwellv1connect.NewServerServiceClient(phone, origin).WatchServers(t.Context(), connect.NewRequest(&dockwellv1.WatchServersRequest{}))
	if err != nil {
		t.Fatal(err)
	}
	if !stream.Receive() || !stream.Msg().GetSnapshotComplete() {
		t.Fatalf("watch didn't start: %v", stream.Err())
	}
	sessions, _ := laptop.authClient().ListSessions(t.Context(), connect.NewRequest(&dockwellv1.ListSessionsRequest{}))
	var phoneID string
	for _, s := range sessions.Msg.GetSessions() {
		if !s.GetCurrent() {
			phoneID = s.GetId()
		}
	}
	if _, err := laptop.authClient().RevokeSession(t.Context(), connect.NewRequest(&dockwellv1.RevokeSessionRequest{Id: phoneID})); err != nil {
		t.Fatal(err)
	}
	done := make(chan error, 1)
	go func() {
		for stream.Receive() {
		}
		done <- stream.Err()
	}()
	select {
	case err := <-done:
		if connect.CodeOf(err) != connect.CodeUnauthenticated || reason(err) != authapi.ReasonSessionEnded {
			t.Errorf("the revoked session's stream ended with %v (reason %q), want unauthenticated with reason session_ended", err, reason(err))
		}
	case <-time.After(5 * time.Second):
		t.Error("the revoked session's stream stayed open")
	}
}

// selfSigned makes a certificate for name, and a pool that trusts it.
func selfSigned(t *testing.T, name string) (tls.Certificate, *x509.CertPool) {
	t.Helper()
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	tmpl := &x509.Certificate{
		SerialNumber: big.NewInt(1),
		Subject:      pkix.Name{CommonName: name},
		DNSNames:     []string{name},
		NotBefore:    time.Now().Add(-time.Hour),
		NotAfter:     time.Now().Add(time.Hour),
	}
	der, err := x509.CreateCertificate(rand.Reader, tmpl, tmpl, &key.PublicKey, key)
	if err != nil {
		t.Fatal(err)
	}
	leaf, _ := x509.ParseCertificate(der)
	pool := x509.NewCertPool()
	pool.AddCert(leaf)
	return tls.Certificate{Certificate: [][]byte{der}, PrivateKey: key, Leaf: leaf}, pool
}

func TestClaimOverHTTPSWithADomain(t *testing.T) { storetest.Each(t, testClaimOverHTTPSWithADomain) }

func testClaimOverHTTPSWithADomain(t *testing.T, backend store.Backend) {
	cert, roots := selfSigned(t, "panel.acme.dev")
	public := &https.Static{Name: "panel.acme.dev", Cert: cert}
	srv := startFull(t, backend, public)
	_, port, _ := net.SplitHostPort(srv.https)
	origin := "https://panel.acme.dev:" + port
	b := newBrowser(t, origin, srv.https, roots)

	link, _ := srv.auth.CreateClaimLink(t.Context(), "test")
	res := b.claim(t, link.Token)
	cookie := res.Header().Get("Set-Cookie")
	for _, want := range []string{"__Host-dockwell_session=", "Secure", "HttpOnly", "SameSite=Strict", "Path=/"} {
		if !strings.Contains(cookie, want) {
			t.Errorf("session cookie %q lacks %q", cookie, want)
		}
	}
	if !strings.Contains(res.Header().Get("Strict-Transport-Security"), "max-age=") {
		t.Error("HTTPS responses don't set HSTS")
	}
	state, err := b.authClient().GetAuthState(t.Context(), connect.NewRequest(&dockwellv1.GetAuthStateRequest{}))
	if err != nil || state.Msg.GetRpId() != "panel.acme.dev" || state.Msg.GetDomain() != "panel.acme.dev" || state.Msg.GetUser() == nil {
		t.Errorf("GetAuthState on the domain = %v, %v", state, err)
	}

	// The session made on the domain doesn't work on localhost: cookies don't
	// cross, and the session is bound to its address anyway.
	_, apiPort, _ := net.SplitHostPort(srv.api)
	local := newBrowser(t, "http://localhost:"+apiPort, srv.api, nil)
	u, _ := url.Parse(local.origin)
	local.client.Jar.SetCookies(u, []*http.Cookie{{Name: auth.LoopbackCookieName, Value: strings.TrimPrefix(strings.Split(cookie, ";")[0], auth.SecureCookieName+"=")}})
	if _, err := local.authClient().ListSessions(t.Context(), connect.NewRequest(&dockwellv1.ListSessionsRequest{})); connect.CodeOf(err) != connect.CodeUnauthenticated {
		t.Errorf("the domain's session on localhost = %v, want unauthenticated", err)
	}

	// Plain HTTP on the domain only redirects, and never to another host.
	noRedirect := &http.Client{CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }}
	req, _ := http.NewRequestWithContext(t.Context(), http.MethodGet, "http://"+srv.http+"/claim?x=1", http.NoBody)
	req.Host = "evil.test"
	resp, err := noRedirect.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	_ = resp.Body.Close()
	if resp.StatusCode != http.StatusMovedPermanently || resp.Header.Get("Location") != "https://panel.acme.dev/claim?x=1" {
		t.Errorf("plain HTTP = %d to %q, want a redirect to https://panel.acme.dev/claim?x=1", resp.StatusCode, resp.Header.Get("Location"))
	}
	req, _ = http.NewRequestWithContext(t.Context(), http.MethodPost, "http://"+srv.http+dockwellv1connect.AuthServiceSignInWithCodeProcedure, strings.NewReader("{}"))
	resp, err = noRedirect.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	_ = resp.Body.Close()
	if resp.StatusCode != http.StatusBadRequest {
		t.Errorf("a POST over plain HTTP = %d, want 400", resp.StatusCode)
	}

	// Another name on the HTTPS listener is refused.
	other := newBrowser(t, "https://panel.acme.dev:"+port, srv.https, roots)
	req, _ = http.NewRequestWithContext(t.Context(), http.MethodGet, origin+"/healthz", http.NoBody)
	req.Host = "other.acme.dev"
	resp, err = other.client.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	_ = resp.Body.Close()
	if resp.StatusCode != http.StatusMisdirectedRequest {
		t.Errorf("another Host on HTTPS = %d, want 421", resp.StatusCode)
	}
}

// reason is the Dockwell-Error reason an error carries.
func reason(err error) string {
	var cerr *connect.Error
	if errors.As(err, &cerr) {
		return cerr.Meta().Get(authapi.ReasonKey)
	}
	return ""
}

// Every refusal for want of a session says why, so the panel can tell a
// session that ended from a wrong code.
func TestRefusalsSayWhy(t *testing.T) { storetest.Each(t, testRefusalsSayWhy) }

func testRefusalsSayWhy(t *testing.T, backend store.Backend) {
	srv := startFull(t, backend, nil)
	_, port, _ := net.SplitHostPort(srv.api)
	origin := "http://localhost:" + port
	laptop := newBrowser(t, origin, srv.api, nil)
	servers := func(b *browser) error {
		_, err := dockwellv1connect.NewServerServiceClient(b, origin).ListServers(t.Context(), connect.NewRequest(&dockwellv1.ListServersRequest{}))
		return err
	}
	if err := servers(laptop); connect.CodeOf(err) != connect.CodeUnauthenticated || reason(err) != authapi.ReasonUnauthenticated {
		t.Errorf("no session = %v (reason %q), want reason unauthenticated", err, reason(err))
	}
	link, _ := srv.auth.CreateClaimLink(t.Context(), "test")
	laptop.claim(t, link.Token)
	phone := newBrowser(t, origin, srv.api, nil)
	phone.auth = laptop.auth
	phone.signIn(t)
	if _, err := laptop.authClient().RevokeOtherSessions(t.Context(), connect.NewRequest(&dockwellv1.RevokeOtherSessionsRequest{})); err != nil {
		t.Fatal(err)
	}
	if err := servers(phone); connect.CodeOf(err) != connect.CodeUnauthenticated || reason(err) != authapi.ReasonSessionEnded {
		t.Errorf("a revoked session = %v (reason %q), want reason session_ended", err, reason(err))
	}
	// A wrong code is unauthenticated too, for its own reason.
	_, err := phone.authClient().SignInWithCode(t.Context(), connect.NewRequest(&dockwellv1.SignInWithCodeRequest{Email: "mira@acme.dev", Code: "123456"}))
	if reason(err) != "code_rejected" {
		t.Errorf("a wrong code = %v (reason %q), want reason code_rejected", err, reason(err))
	}

	// A write with the cookie but no Origin.
	req, _ := http.NewRequestWithContext(t.Context(), http.MethodPost, origin+dockwellv1connect.AuthServiceRevokeOtherSessionsProcedure, strings.NewReader("{}"))
	req.Header.Set("Content-Type", "application/json")
	req.AddCookie(laptop.cookies()[0])
	res, err := laptop.client.Transport.RoundTrip(req)
	if err != nil {
		t.Fatal(err)
	}
	_ = res.Body.Close()
	if res.StatusCode != http.StatusForbidden || !strings.Contains(res.Header.Get("Content-Type"), "json") {
		t.Fatalf("a write without Origin = %d %s", res.StatusCode, res.Header.Get("Content-Type"))
	}
	noOrigin := dockwellv1connect.NewAuthServiceClient(connectFunc(func(r *http.Request) (*http.Response, error) {
		r.AddCookie(laptop.cookies()[0])
		return laptop.client.Transport.RoundTrip(r)
	}), origin, connect.WithProtoJSON())
	_, err = noOrigin.RevokeOtherSessions(t.Context(), connect.NewRequest(&dockwellv1.RevokeOtherSessionsRequest{}))
	if connect.CodeOf(err) != connect.CodePermissionDenied || reason(err) != authapi.ReasonOrigin {
		t.Errorf("a write without Origin = %v (reason %q), want reason origin", err, reason(err))
	}
}

// A session that can't be checked, as when the database fails, doesn't
// stop the calls that need none: the sign-in screen still loads, and people
// can still start signing in. Calls that need a session say to try again,
// and the cookie stays, as its session may still work.
func TestPublicCallsGoAheadWhenTheSessionCantBeChecked(t *testing.T) {
	storetest.Each(t, testPublicCallsGoAheadWhenTheSessionCantBeChecked)
}

func testPublicCallsGoAheadWhenTheSessionCantBeChecked(t *testing.T, backend store.Backend) {
	srv := startFull(t, backend, nil)
	_, port, _ := net.SplitHostPort(srv.api)
	b := newBrowser(t, "http://localhost:"+port, srv.api, nil)
	link, _ := srv.auth.CreateClaimLink(t.Context(), "test")
	b.claim(t, link.Token)
	servers := dockwellv1connect.NewServerServiceClient(b, b.origin)

	srv.execSQL(t, `ALTER TABLE sessions RENAME TO sessions_away`)
	if _, err := dockwellv1connect.NewSystemServiceClient(b, b.origin).GetVersion(t.Context(), connect.NewRequest(&dockwellv1.GetVersionRequest{})); err != nil {
		t.Errorf("GetVersion with a session that can't be checked: %v", err)
	}
	if _, err := b.authClient().BeginPasskeySignIn(t.Context(), connect.NewRequest(&dockwellv1.BeginPasskeySignInRequest{})); err != nil {
		t.Errorf("BeginPasskeySignIn with a session that can't be checked: %v", err)
	}
	state, err := b.authClient().GetAuthState(t.Context(), connect.NewRequest(&dockwellv1.GetAuthStateRequest{}))
	if err != nil || !state.Msg.GetClaimed() || state.Msg.GetUser() != nil {
		t.Errorf("GetAuthState with a session that can't be checked = %v, %v, want claimed and no user", state, err)
	} else if cookie := state.Header().Get("Set-Cookie"); cookie != "" {
		t.Errorf("GetAuthState cleared a cookie it couldn't check: %q", cookie)
	}
	if _, err := servers.ListServers(t.Context(), connect.NewRequest(&dockwellv1.ListServersRequest{})); connect.CodeOf(err) != connect.CodeUnavailable || reason(err) != authapi.ReasonUnavailable {
		t.Errorf("ListServers with a session that can't be checked = %v (reason %q), want unavailable", err, reason(err))
	}

	srv.execSQL(t, `ALTER TABLE sessions_away RENAME TO sessions`)
	if _, err := servers.ListServers(t.Context(), connect.NewRequest(&dockwellv1.ListServersRequest{})); err != nil {
		t.Errorf("ListServers once the session can be checked again: %v", err)
	}
}

// connectFunc adapts a function to connect.HTTPClient.
type connectFunc func(*http.Request) (*http.Response, error)

func (f connectFunc) Do(r *http.Request) (*http.Response, error) { return f(r) }
