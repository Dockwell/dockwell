// SPDX-License-Identifier: AGPL-3.0-only

package control

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net"
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/dockwell/dockwell/internal/store"
	"github.com/dockwell/dockwell/internal/store/storetest"

	"connectrpc.com/connect"
	"google.golang.org/genproto/googleapis/api/annotations"
	"google.golang.org/protobuf/proto"
	"google.golang.org/protobuf/reflect/protoreflect"
	"google.golang.org/protobuf/reflect/protoregistry"
	"google.golang.org/protobuf/types/descriptorpb"

	dockwellv1 "github.com/dockwell/dockwell/gen/dockwell/v1"
	"github.com/dockwell/dockwell/gen/dockwell/v1/dockwellv1connect"
	"github.com/dockwell/dockwell/internal/control/authapi"
)

// restMethod is one method's REST path.
type restMethod struct {
	procedure string
	verb      string
	path      string // with {id} and other variables as they are in the rule
	body      string
	md        protoreflect.MethodDescriptor
}

// restMethods lists every REST path in dockwell.v1, from the schema.
func restMethods(t *testing.T) []restMethod {
	t.Helper()
	var out []restMethod
	protoregistry.GlobalFiles.RangeFilesByPackage("dockwell.v1", func(fd protoreflect.FileDescriptor) bool {
		for i := range fd.Services().Len() {
			sd := fd.Services().Get(i)
			for j := range sd.Methods().Len() {
				md := sd.Methods().Get(j)
				opts, _ := md.Options().(*descriptorpb.MethodOptions)
				if opts == nil || !proto.HasExtension(opts, annotations.E_Http) {
					continue
				}
				rule, _ := proto.GetExtension(opts, annotations.E_Http).(*annotations.HttpRule)
				verb, path := httpRule(rule)
				out = append(out, restMethod{
					procedure: "/" + string(sd.FullName()) + "/" + string(md.Name()),
					verb:      verb, path: path, body: rule.GetBody(), md: md,
				})
			}
		}
		return true
	})
	if len(out) == 0 {
		t.Fatal("no REST paths in dockwell.v1; is the generated package linked in?")
	}
	return out
}

func httpRule(rule *annotations.HttpRule) (verb, path string) {
	switch p := rule.GetPattern().(type) {
	case *annotations.HttpRule_Get:
		return http.MethodGet, p.Get
	case *annotations.HttpRule_Post:
		return http.MethodPost, p.Post
	case *annotations.HttpRule_Put:
		return http.MethodPut, p.Put
	case *annotations.HttpRule_Patch:
		return http.MethodPatch, p.Patch
	case *annotations.HttpRule_Delete:
		return http.MethodDelete, p.Delete
	case *annotations.HttpRule_Custom:
		return p.Custom.GetKind(), p.Custom.GetPath()
	}
	return "", ""
}

// fillPath puts a value in every variable of a path template.
func fillPath(path string) string {
	var b strings.Builder
	for {
		open := strings.IndexByte(path, '{')
		if open < 0 {
			b.WriteString(path)
			return b.String()
		}
		end := strings.IndexByte(path[open:], '}')
		b.WriteString(path[:open] + "x")
		path = path[open+end+1:]
	}
}

// callREST calls a REST path as b, with b's cookies and origin, and the
// headers in extra on top (an empty value deletes one). It returns the
// status, the Dockwell-Error reason and the body.
func callREST(t *testing.T, b *browser, verb, path string, extra map[string]string, body string) (int, string, string) {
	t.Helper()
	ctx, cancel := context.WithTimeout(t.Context(), 5*time.Second)
	defer cancel()
	var reader io.Reader = http.NoBody
	if verb == http.MethodPost || verb == http.MethodPut || verb == http.MethodPatch {
		if body == "" {
			body = "{}"
		}
		reader = strings.NewReader(body)
	}
	req, err := http.NewRequestWithContext(ctx, verb, b.origin+fillPath(path), reader)
	if err != nil {
		t.Fatal(err)
	}
	if reader != http.NoBody {
		req.Header.Set("Content-Type", "application/json")
	}
	req.Header.Set("Origin", b.origin)
	req.Header.Set("Sec-Fetch-Site", "same-origin")
	for k, v := range extra {
		switch {
		case k == "Host":
			req.Host = v
		case v == "":
			req.Header.Del(k)
		default:
			req.Header.Set(k, v)
		}
	}
	res, err := b.client.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer res.Body.Close()
	out, _ := io.ReadAll(res.Body)
	return res.StatusCode, res.Header.Get(authapi.ReasonKey), string(out)
}

// TestEveryMethodHasARESTPath enforces the schema's REST rules (ADR 0015):
// every unary method of dockwell.v1 has one REST path under /v1/, and no
// two share one; reads are GETs and only reads are, with no body; changes
// are POSTs or DELETEs; and streams have none, since REST can't carry them
// and Vanguard would answer every call with 415.
func TestEveryMethodHasARESTPath(t *testing.T) {
	rules := map[string]restMethod{}
	for _, m := range restMethods(t) {
		rules[m.procedure] = m
	}
	seen := map[string]string{}
	var methods int
	protoregistry.GlobalFiles.RangeFilesByPackage("dockwell.v1", func(fd protoreflect.FileDescriptor) bool {
		for i := range fd.Services().Len() {
			sd := fd.Services().Get(i)
			for j := range sd.Methods().Len() {
				md := sd.Methods().Get(j)
				methods++
				procedure := "/" + string(sd.FullName()) + "/" + string(md.Name())
				m, ok := rules[procedure]
				if md.IsStreamingClient() || md.IsStreamingServer() {
					if ok {
						t.Errorf("%s streams, so it can't have a REST path, but has %s %s", md.FullName(), m.verb, m.path)
					}
					continue
				}
				if !ok {
					t.Errorf("%s has no REST path: add a google.api.http option", md.FullName())
					continue
				}
				opts, _ := md.Options().(*descriptorpb.MethodOptions)
				if len(proto.GetExtension(opts, annotations.E_Http).(*annotations.HttpRule).GetAdditionalBindings()) > 0 {
					t.Errorf("%s has more than one REST path", md.FullName())
				}
				if !strings.HasPrefix(m.path, authapi.RESTPrefix) {
					t.Errorf("%s's REST path %s isn't under %s", md.FullName(), m.path, authapi.RESTPrefix)
				}
				key := m.verb + " " + m.path
				if other, dup := seen[key]; dup {
					t.Errorf("%s and %s share %s", other, md.FullName(), key)
				}
				seen[key] = string(md.FullName())
				effect, _ := proto.GetExtension(opts, dockwellv1.E_Effect).(dockwellv1.Effect)
				read := effect == dockwellv1.Effect_EFFECT_READ && opts.GetIdempotencyLevel() == descriptorpb.MethodOptions_NO_SIDE_EFFECTS
				switch m.verb {
				case http.MethodGet:
					if !read {
						t.Errorf("%s is a GET, but it isn't a read with no side effects", md.FullName())
					}
				case http.MethodPost:
					if m.body != "*" {
						t.Errorf("%s is a POST with body %q, want \"*\"", md.FullName(), m.body)
					}
				case http.MethodDelete:
					if effect != dockwellv1.Effect_EFFECT_DESTRUCTIVE {
						t.Errorf("%s is a DELETE, but its effect is %v", md.FullName(), effect)
					}
				default:
					t.Errorf("%s uses %s; use GET, POST or DELETE", md.FullName(), m.verb)
				}
				if read && m.verb != http.MethodGet {
					t.Errorf("%s reads with no side effects, so make it a GET, not %s", md.FullName(), m.verb)
				}
				if m.verb != http.MethodPost && m.body != "" {
					t.Errorf("%s is a %s with a body", md.FullName(), m.verb)
				}
			}
		}
		return true
	})
	if methods < 25 {
		t.Errorf("checked only %d methods", methods)
	}
}

func TestRESTPathsWithASession(t *testing.T) { storetest.Each(t, testRESTPathsWithASession) }

func testRESTPathsWithASession(t *testing.T, backend store.Backend) {
	srv := startFull(t, backend, nil)
	_, port, _ := net.SplitHostPort(srv.api)
	b := newBrowser(t, "http://localhost:"+port, srv.api, nil)
	link, _ := srv.auth.CreateClaimLink(t.Context(), "test")
	b.claim(t, link.Token)

	status, _, body := callREST(t, b, http.MethodGet, "/v1/authState", nil, "")
	var state struct {
		Claimed bool `json:"claimed"`
		User    struct {
			Email string `json:"email"`
		} `json:"user"`
		SignInMethod string `json:"signInMethod"`
	}
	if err := json.Unmarshal([]byte(body), &state); status != http.StatusOK || err != nil || !state.Claimed ||
		state.User.Email != "mira@acme.dev" || state.SignInMethod != "SIGN_IN_METHOD_CLAIM" {
		t.Errorf("GET /v1/authState = %d %s", status, body)
	}
	if status, _, body := callREST(t, b, http.MethodGet, "/v1/servers", nil, ""); status != http.StatusOK || !strings.HasPrefix(body, "{") {
		t.Errorf("GET /v1/servers = %d %s", status, body)
	}
	// A POST with no body is a request with no fields, however it is sent.
	for _, tt := range []struct {
		name        string
		contentType string
		chunked     bool
	}{
		{"no body at all, as curl -X POST sends it", "", false},
		{"an empty form, as curl -X POST -d '' sends it", "application/x-www-form-urlencoded", false},
		{"an empty chunked body", "", true},
		{"an empty chunked JSON body", "application/json", true},
	} {
		req, _ := http.NewRequestWithContext(t.Context(), http.MethodPost, b.origin+"/v1/sessions:revokeOthers", http.NoBody)
		if tt.contentType != "" {
			req.Header.Set("Content-Type", tt.contentType)
		}
		if tt.chunked {
			req.Body, req.ContentLength, req.TransferEncoding = io.NopCloser(strings.NewReader("")), -1, []string{"chunked"}
		}
		res, err := b.Do(req)
		if err != nil {
			t.Fatal(err)
		}
		out, _ := io.ReadAll(res.Body)
		_ = res.Body.Close()
		if res.StatusCode != http.StatusOK {
			t.Errorf("POST /v1/sessions:revokeOthers with %s = %d %s, want 200", tt.name, res.StatusCode, out)
		}
	}
	if status, why, body := callREST(t, b, http.MethodPost, "/v1/passkeys/{id}:remove", nil, "{}"); status != http.StatusNotFound || why != "not_found" {
		t.Errorf("POST /v1/passkeys/x:remove = %d (reason %q) %s, want 404 not_found", status, why, body)
	}
	status, _, body = callREST(t, b, http.MethodGet, "/v1/nothing/here", nil, "")
	var notFound rpcStatus
	if err := json.Unmarshal([]byte(body), &notFound); status != http.StatusNotFound || err != nil || notFound.Code != 5 ||
		!strings.Contains(notFound.Message, "/v1/openapi.yaml") {
		t.Errorf("an unknown REST path = %d %s, want 404 with a google.rpc.Status", status, body)
	}

	// The OpenAPI document needs no session.
	anon := newBrowser(t, b.origin, srv.api, nil)
	status, _, body = callREST(t, anon, http.MethodGet, "/v1/openapi.yaml", nil, "")
	if status != http.StatusOK || !strings.Contains(body, "\nopenapi: 3.") || !strings.Contains(body, "/v1/servers/{id}:") {
		t.Errorf("GET /v1/openapi.yaml = %d %.200s", status, body)
	}
}

// TestRESTRefusesCrossSiteRequests: REST paths take simple requests, which a
// browser sends from any page without asking. A cookie's request must come
// from the panel; one with an API token needs no origin, since no browser
// adds the token for a page.
func TestRESTRefusesCrossSiteRequests(t *testing.T) {
	storetest.Each(t, testRESTRefusesCrossSiteRequests)
}

func testRESTRefusesCrossSiteRequests(t *testing.T, backend store.Backend) {
	srv := startFull(t, backend, nil)
	_, port, _ := net.SplitHostPort(srv.api)
	origin := "http://localhost:" + port
	laptop := newBrowser(t, origin, srv.api, nil)
	link, _ := srv.auth.CreateClaimLink(t.Context(), "test")
	laptop.claim(t, link.Token)
	phone := newBrowser(t, origin, srv.api, nil)
	phone.auth = laptop.auth
	phone.signIn(t)
	token := createToken(t, laptop, dockwellv1.TokenScope_TOKEN_SCOPE_READ_ONLY)
	phoneSignedIn := func() bool {
		status, _, _ := callREST(t, phone, http.MethodGet, "/v1/sessions", nil, "")
		return status == http.StatusOK
	}

	for _, tt := range []struct {
		name    string
		verb    string
		path    string
		headers map[string]string
		body    string
		status  int
		reason  string
	}{
		// With the session cookie.
		{"a read from the panel", http.MethodGet, "/v1/sessions", nil, "", http.StatusOK, ""},
		{
			"a GET with no origin, as a typed URL sends it", http.MethodGet, "/v1/sessions",
			map[string]string{"Origin": "", "Sec-Fetch-Site": ""},
			"", http.StatusOK, "",
		},
		{
			"a cross-site GET", http.MethodGet, "/v1/sessions",
			map[string]string{"Origin": "", "Sec-Fetch-Site": "cross-site"},
			"", http.StatusForbidden, authapi.ReasonOrigin,
		},
		{
			"a GET from another local port", http.MethodGet, "/v1/sessions",
			map[string]string{"Origin": "", "Sec-Fetch-Site": "same-site"},
			"", http.StatusForbidden, authapi.ReasonOrigin,
		},
		{
			"a text/plain form post from another site", http.MethodPost, "/v1/sessions:revokeOthers",
			map[string]string{"Origin": "https://evil.test", "Sec-Fetch-Site": "cross-site", "Content-Type": "text/plain"},
			"{}",
			http.StatusForbidden, authapi.ReasonOrigin,
		},
		{
			"a urlencoded form post from another local port", http.MethodPost, "/v1/sessions:revokeOthers",
			map[string]string{"Origin": "http://localhost:3000", "Sec-Fetch-Site": "same-site", "Content-Type": "application/x-www-form-urlencoded"},
			"a=b", http.StatusForbidden, authapi.ReasonOrigin,
		},
		{
			"a JSON post from a null origin", http.MethodPost, "/v1/sessions:revokeOthers",
			map[string]string{"Origin": "null", "Sec-Fetch-Site": ""},
			"", http.StatusForbidden, authapi.ReasonOrigin,
		},
		{
			"a JSON post with the cookie and no origin", http.MethodPost, "/v1/sessions:revokeOthers",
			map[string]string{"Origin": "", "Sec-Fetch-Site": ""},
			"", http.StatusForbidden, authapi.ReasonOrigin,
		},
		{
			// Who's calling is checked before the body is read, so a cookie with
			// no origin is refused for that, whatever the body's type.
			"a text/plain post with the cookie and no origin", http.MethodPost, "/v1/sessions:revokeOthers",
			map[string]string{"Origin": "", "Sec-Fetch-Site": "", "Content-Type": "text/plain"},
			"{}", http.StatusForbidden, authapi.ReasonOrigin,
		},
		{
			"a proxy's Basic credentials don't stand in for a token", http.MethodPost, "/v1/sessions:revokeOthers",
			map[string]string{"Origin": "https://evil.test", "Authorization": "Basic bWlyYTpodW50ZXIy"},
			"", http.StatusForbidden, authapi.ReasonOrigin,
		},
		{
			"DNS rebinding", http.MethodGet, "/v1/sessions",
			map[string]string{"Host": "evil.test:" + port, "Origin": "http://evil.test:" + port},
			"", http.StatusMisdirectedRequest, authapi.ReasonMisdirected,
		},

		// With a bearer an auth proxy in front adds, such as oauth2-proxy's
		// or Pomerium's, which isn't a Dockwell token: the cookie works, and
		// the origin checks apply, as if it weren't there.
		{
			"a proxy's bearer leaves the cookie to sign in", http.MethodGet, "/v1/sessions",
			map[string]string{"Authorization": "Bearer " + proxyJWT},
			"", http.StatusOK, "",
		},
		{
			"a proxy's bearer and a write from the panel", http.MethodPost, "/v1/sessions/{id}:revoke",
			map[string]string{"Authorization": "Bearer " + proxyJWT},
			"", http.StatusNotFound, "not_found",
		},
		{
			"a proxy's bearer on a cross-site write", http.MethodPost, "/v1/sessions:revokeOthers",
			map[string]string{"Origin": "https://evil.test", "Sec-Fetch-Site": "cross-site", "Authorization": "Bearer " + proxyJWT},
			"", http.StatusForbidden, authapi.ReasonOrigin,
		},
		{
			"a proxy's bearer on a write with no origin", http.MethodPost, "/v1/sessions:revokeOthers",
			map[string]string{"Origin": "", "Sec-Fetch-Site": "", "Authorization": "Bearer " + proxyJWT},
			"", http.StatusForbidden, authapi.ReasonOrigin,
		},

		// With an API token, which takes the cookie's place.
		{
			"a token with no origin", http.MethodGet, "/v1/servers",
			map[string]string{"Origin": "", "Sec-Fetch-Site": "", "Authorization": "Bearer " + token},
			"", http.StatusOK, "",
		},
		{
			"a token whatever the origin says", http.MethodGet, "/v1/servers",
			map[string]string{"Origin": "https://evil.test", "Sec-Fetch-Site": "cross-site", "Authorization": "Bearer " + token},
			"", http.StatusOK, "",
		},
		{
			"a token, never the cookie beside it", http.MethodGet, "/v1/sessions",
			map[string]string{"Authorization": "Bearer " + token},
			"", http.StatusForbidden, authapi.ReasonNeedsSession,
		},
		{
			"a wrong token, never the cookie beside it", http.MethodGet, "/v1/sessions",
			map[string]string{"Authorization": "Bearer dwt_wrong"},
			"", http.StatusUnauthorized, authapi.ReasonTokenRejected,
		},
		{
			"an empty token, as an unset variable sends it", http.MethodGet, "/v1/servers",
			map[string]string{"Origin": "", "Sec-Fetch-Site": "", "Authorization": "Bearer "},
			"", http.StatusUnauthorized, authapi.ReasonTokenRejected,
		},
		{
			"a token and DNS rebinding", http.MethodGet, "/v1/servers",
			map[string]string{"Host": "evil.test:" + port, "Authorization": "Bearer " + token},
			"", http.StatusMisdirectedRequest, authapi.ReasonMisdirected,
		},
	} {
		t.Run(tt.name, func(t *testing.T) {
			status, why, body := callREST(t, laptop, tt.verb, tt.path, tt.headers, tt.body)
			if status != tt.status || why != tt.reason {
				t.Errorf("%s %s = %d (reason %q), want %d (reason %q): %s", tt.verb, tt.path, status, why, tt.status, tt.reason, body)
			}
			// Refusals from the guard and the interceptor alike are a
			// google.rpc.Status, as every REST error is.
			if code, ok := restStatusCodes[status]; ok && status != http.StatusUnsupportedMediaType {
				var st rpcStatus
				if err := json.Unmarshal([]byte(body), &st); err != nil || st.Code != code || st.Message == "" || st.Details == nil {
					t.Errorf("%s %s = %d %q, want a google.rpc.Status with code %d", tt.verb, tt.path, status, body, code)
				}
			}
			if !phoneSignedIn() {
				t.Fatal("a refused request signed the phone out")
			}
		})
	}

	// An empty form, which REST takes as an empty JSON object, still needs
	// the panel's origin for a write with the cookie.
	req, _ := http.NewRequestWithContext(t.Context(), http.MethodPost, origin+"/v1/sessions:revokeOthers", http.NoBody)
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	res, err := laptop.client.Do(req) // the cookie, and no Origin or Sec-Fetch-Site
	if err != nil {
		t.Fatal(err)
	}
	_ = res.Body.Close()
	if res.StatusCode != http.StatusForbidden || res.Header.Get(authapi.ReasonKey) != authapi.ReasonOrigin || !phoneSignedIn() {
		t.Errorf("an empty form post with the cookie and no origin = %d (reason %q), want 403 origin",
			res.StatusCode, res.Header.Get(authapi.ReasonKey))
	}
	// A proxy's bearer is no token: without a cookie, the call is signed out.
	anon := newBrowser(t, origin, srv.api, nil)
	if status, why, body := callREST(t, anon, http.MethodGet, "/v1/servers", map[string]string{"Authorization": "Bearer " + proxyJWT}, ""); status != http.StatusUnauthorized ||
		why != authapi.ReasonUnauthenticated {
		t.Errorf("GET /v1/servers with a proxy's bearer and no cookie = %d (reason %q) %s, want 401 unauthenticated", status, why, body)
	}
}

// proxyJWT is the kind of bearer an auth proxy in front adds: not Dockwell's.
const proxyJWT = "eyJhbGciOiJSUzI1NiJ9.eyJzdWIiOiJtaXJhIn0.c2lnbmF0dXJl"

// rpcStatus is a google.rpc.Status as JSON.
type rpcStatus struct {
	Code    int    `json:"code"`
	Message string `json:"message"`
	Details []any  `json:"details"`
}

// restStatusCodes are the google.rpc.Status codes REST answers with, by HTTP
// status. 421 has no code of its own; it is the client's to fix.
var restStatusCodes = map[int]int{
	http.StatusUnauthorized:         int(connect.CodeUnauthenticated),
	http.StatusForbidden:            int(connect.CodePermissionDenied),
	http.StatusNotFound:             int(connect.CodeNotFound),
	http.StatusMisdirectedRequest:   int(connect.CodeInvalidArgument),
	http.StatusUnsupportedMediaType: 0,
}

// createToken makes an API token through the API, as the panel does, and
// returns its secret.
func createToken(t *testing.T, b *browser, scope dockwellv1.TokenScope) string {
	t.Helper()
	res, err := dockwellv1connect.NewTokenServiceClient(b, b.origin, connect.WithProtoJSON()).CreateToken(t.Context(),
		connect.NewRequest(&dockwellv1.CreateTokenRequest{
			Name: "CI " + scope.String(), Scope: scope, Proof: stepUpFor(t, b, dockwellv1.StepUpAction_STEP_UP_ACTION_CREATE_TOKEN, ""),
		}))
	if err != nil {
		t.Fatalf("CreateToken: %v", err)
	}
	return res.Msg.GetSecret()
}

// bearer is an HTTP client that sends an API token and nothing else: no
// cookies and no origin, as a script does.
type bearer struct {
	token string
	rt    http.RoundTripper
}

func (b bearer) Do(r *http.Request) (*http.Response, error) {
	r.Header.Set("Authorization", "Bearer "+b.token)
	return b.rt.RoundTrip(r)
}

func h2cTransport(t *testing.T, addr string) *http.Transport {
	t.Helper()
	var d net.Dialer
	var p http.Protocols
	p.SetUnencryptedHTTP2(true)
	return &http.Transport{
		DialContext: func(ctx context.Context, network, _ string) (net.Conn, error) {
			return d.DialContext(ctx, network, addr)
		},
		Protocols: &p,
	}
}

// One token works with every protocol the API speaks, and stops working,
// streams included, the moment it is revoked.
func TestATokenWorksWithEveryProtocol(t *testing.T) {
	storetest.Each(t, testATokenWorksWithEveryProtocol)
}

func testATokenWorksWithEveryProtocol(t *testing.T, backend store.Backend) {
	srv := startFull(t, backend, nil)
	_, port, _ := net.SplitHostPort(srv.api)
	origin := "http://localhost:" + port
	b := newBrowser(t, origin, srv.api, nil)
	link, _ := srv.auth.CreateClaimLink(t.Context(), "test")
	b.claim(t, link.Token)
	secret := createToken(t, b, dockwellv1.TokenScope_TOKEN_SCOPE_READ_ONLY)
	if !strings.HasPrefix(secret, "dwt_") || len(secret) != 53 {
		t.Fatalf("secret %q", secret)
	}
	h1 := bearer{token: secret, rt: b.client.Transport}
	h2 := bearer{token: secret, rt: h2cTransport(t, srv.api)}
	// The CLI on the server itself may use the loopback IP, where no one
	// can sign in, but a token works.
	onIP := "http://" + srv.api

	clients := map[string]dockwellv1connect.ServerServiceClient{
		"Connect":         dockwellv1connect.NewServerServiceClient(h1, origin),
		"Connect on IP":   dockwellv1connect.NewServerServiceClient(h1, onIP),
		"Connect JSON":    dockwellv1connect.NewServerServiceClient(h1, origin, connect.WithProtoJSON()),
		"gRPC":            dockwellv1connect.NewServerServiceClient(h2, origin, connect.WithGRPC()),
		"gRPC-Web":        dockwellv1connect.NewServerServiceClient(h1, origin, connect.WithGRPCWeb()),
		"Connect over h2": dockwellv1connect.NewServerServiceClient(h2, origin),
	}
	for name, c := range clients {
		if _, err := c.ListServers(t.Context(), connect.NewRequest(&dockwellv1.ListServersRequest{})); err != nil {
			t.Errorf("ListServers over %s with a token: %v", name, err)
		}
	}
	rest := func() (int, string) {
		req, _ := http.NewRequestWithContext(t.Context(), http.MethodGet, onIP+"/v1/servers", http.NoBody)
		res, err := h1.Do(req)
		if err != nil {
			t.Fatal(err)
		}
		defer res.Body.Close()
		return res.StatusCode, res.Header.Get(authapi.ReasonKey)
	}
	if status, why := rest(); status != http.StatusOK {
		t.Errorf("GET /v1/servers with a token = %d (reason %q)", status, why)
	}

	streams := map[string]*connect.ServerStreamForClient[dockwellv1.WatchServersResponse]{}
	for _, name := range []string{"Connect", "gRPC", "gRPC-Web"} {
		stream, err := clients[name].WatchServers(t.Context(), connect.NewRequest(&dockwellv1.WatchServersRequest{}))
		if err != nil {
			t.Fatal(err)
		}
		if !stream.Receive() || !stream.Msg().GetSnapshotComplete() {
			t.Fatalf("watch over %s didn't start: %v", name, stream.Err())
		}
		streams[name] = stream
	}

	tokens, err := dockwellv1connect.NewTokenServiceClient(b, origin).ListTokens(t.Context(), connect.NewRequest(&dockwellv1.ListTokensRequest{}))
	if err != nil || len(tokens.Msg.GetTokens()) != 1 {
		t.Fatalf("ListTokens = %v, %v", tokens, err)
	}
	tok := tokens.Msg.GetTokens()[0]
	if tok.GetLastAddress() != "127.0.0.1" || tok.GetLastUsedTime() == nil || tok.GetScope() != dockwellv1.TokenScope_TOKEN_SCOPE_READ_ONLY ||
		!strings.HasPrefix(tok.GetHint(), secret[:8]) || tok.GetExpireTime() != nil {
		t.Errorf("the token as listed = %v", tok)
	}
	if _, err := dockwellv1connect.NewTokenServiceClient(b, origin).RevokeToken(t.Context(),
		connect.NewRequest(&dockwellv1.RevokeTokenRequest{Id: tok.GetId()})); err != nil {
		t.Fatal(err)
	}
	for name, stream := range streams {
		done := make(chan error, 1)
		go func() {
			for stream.Receive() {
			}
			done <- stream.Err()
		}()
		select {
		case err := <-done:
			if connect.CodeOf(err) != connect.CodeUnauthenticated || reason(err) != authapi.ReasonTokenRejected {
				t.Errorf("the revoked token's stream over %s ended with %v (reason %q), want token_rejected", name, err, reason(err))
			}
		case <-time.After(5 * time.Second):
			t.Errorf("the revoked token's stream over %s stayed open", name)
		}
		_ = stream.Close()
	}
	for name, c := range clients {
		_, err := c.ListServers(t.Context(), connect.NewRequest(&dockwellv1.ListServersRequest{}))
		if connect.CodeOf(err) != connect.CodeUnauthenticated || reason(err) != authapi.ReasonTokenRejected {
			t.Errorf("ListServers over %s with a revoked token = %v (reason %q), want token_rejected", name, err, reason(err))
		}
	}
	if status, why := rest(); status != http.StatusUnauthorized || why != authapi.ReasonTokenRejected {
		t.Errorf("GET /v1/servers with a revoked token = %d (reason %q), want 401 token_rejected", status, why)
	}
}

// tokenRefusals are the reasons a call is refused for its token.
var tokenRefusals = map[string]bool{
	authapi.ReasonTokenRejected: true, authapi.ReasonTokenScope: true, authapi.ReasonNeedsSession: true,
	authapi.ReasonUnauthenticated: true, authapi.ReasonSessionEnded: true, authapi.ReasonOrigin: true,
}

// wantForToken is what a token of scope may do with a procedure, by the
// schema: the reason it is refused for, or "" when it may make the call.
func wantForToken(procedure string, md protoreflect.MethodDescriptor, scope dockwellv1.TokenScope, public map[string]bool) string {
	if public[procedure] {
		return ""
	}
	if strings.HasPrefix(procedure, "/"+dockwellv1connect.AuthServiceName+"/") || strings.HasPrefix(procedure, "/"+dockwellv1connect.TokenServiceName+"/") {
		return authapi.ReasonNeedsSession
	}
	if md.ParentFile().Package() != "dockwell.v1" {
		return "" // reflection, which shows the public schema
	}
	opts, _ := md.Options().(*descriptorpb.MethodOptions)
	if person, _ := proto.GetExtension(opts, dockwellv1.E_NeedsSession).(bool); person {
		return authapi.ReasonNeedsSession
	}
	switch effect, _ := proto.GetExtension(opts, dockwellv1.E_Effect).(dockwellv1.Effect); effect {
	case dockwellv1.Effect_EFFECT_READ:
		return ""
	case dockwellv1.Effect_EFFECT_PLAN, dockwellv1.Effect_EFFECT_APPLY:
		if scope == dockwellv1.TokenScope_TOKEN_SCOPE_READ_WRITE {
			return ""
		}
		return authapi.ReasonTokenScope
	default:
		return authapi.ReasonNeedsSession
	}
}

// TestWhatEachTokenMayCall calls every procedure, and every REST path,
// with a read-only and a read-write token. Each must be refused exactly
// when its effect, or its service, puts it out of the token's reach.
func TestWhatEachTokenMayCall(t *testing.T) { storetest.Each(t, testWhatEachTokenMayCall) }

func testWhatEachTokenMayCall(t *testing.T, backend store.Backend) {
	srv := startFull(t, backend, nil)
	_, port, _ := net.SplitHostPort(srv.api)
	origin := "http://localhost:" + port
	b := newBrowser(t, origin, srv.api, nil)
	link, _ := srv.auth.CreateClaimLink(t.Context(), "test")
	b.claim(t, link.Token)
	public := map[string]bool{}
	for _, p := range authapi.PublicProcedures() {
		public[p] = true
	}
	for _, scope := range []dockwellv1.TokenScope{dockwellv1.TokenScope_TOKEN_SCOPE_READ_ONLY, dockwellv1.TokenScope_TOKEN_SCOPE_READ_WRITE} {
		secret := createToken(t, b, scope)
		h1 := bearer{token: secret, rt: b.client.Transport}
		h2 := bearer{token: secret, rt: h2cTransport(t, srv.api)}
		var checked int
		check := func(served string, sd protoreflect.ServiceDescriptor) {
			for i := range sd.Methods().Len() {
				md := sd.Methods().Get(i)
				procedure := "/" + served + "/" + string(md.Name())
				var via connect.HTTPClient = h1
				if md.IsStreamingClient() {
					via = h2
				}
				err := callEmptyWith(t, via, origin, procedure, md)
				checked++
				got := reason(err)
				if !tokenRefusals[got] {
					got = ""
				}
				if want := wantForToken(procedure, md, scope, public); got != want {
					t.Errorf("%s with a %v token: %v (reason %q), want reason %q", procedure, scope, err, got, want)
				}
			}
		}
		protoregistry.GlobalFiles.RangeFilesByPackage("dockwell.v1", func(fd protoreflect.FileDescriptor) bool {
			for i := range fd.Services().Len() {
				sd := fd.Services().Get(i)
				check(string(sd.FullName()), sd)
			}
			return true
		})
		for served, registered := range map[string]string{
			"grpc.health.v1.Health":                    "connectext.grpc.health.v1.Health",
			"grpc.reflection.v1.ServerReflection":      "connectext.grpc.reflection.v1.ServerReflection",
			"grpc.reflection.v1alpha.ServerReflection": "connectext.grpc.reflection.v1.ServerReflection",
		} {
			d, err := protoregistry.GlobalFiles.FindDescriptorByName(protoreflect.FullName(registered))
			if err != nil {
				t.Fatal(err)
			}
			check(served, d.(protoreflect.ServiceDescriptor))
		}
		if checked < 30 {
			t.Errorf("checked only %d procedures", checked)
		}

		anon := newBrowser(t, origin, srv.api, nil) // no cookie
		for _, m := range restMethods(t) {
			status, why, body := callREST(t, anon, m.verb, m.path, map[string]string{"Origin": "", "Sec-Fetch-Site": "", "Authorization": "Bearer " + secret}, "")
			got := why
			if !tokenRefusals[got] {
				got = ""
			}
			if want := wantForToken(m.procedure, m.md, scope, public); got != want {
				t.Errorf("%s %s with a %v token = %d (reason %q), want reason %q: %s", m.verb, m.path, scope, status, why, want, body)
			}
		}
	}
}

// A token is refused the same way over every protocol, and says why.
func TestTokenRefusalsSayWhy(t *testing.T) { storetest.Each(t, testTokenRefusalsSayWhy) }

func testTokenRefusalsSayWhy(t *testing.T, backend store.Backend) {
	srv := startFull(t, backend, nil)
	_, port, _ := net.SplitHostPort(srv.api)
	origin := "http://localhost:" + port
	b := newBrowser(t, origin, srv.api, nil)
	link, _ := srv.auth.CreateClaimLink(t.Context(), "test")
	b.claim(t, link.Token)
	secret := createToken(t, b, dockwellv1.TokenScope_TOKEN_SCOPE_READ_WRITE)
	for name, opts := range map[string][]connect.ClientOption{"Connect": nil, "gRPC-Web": {connect.WithGRPCWeb()}} {
		c := dockwellv1connect.NewAuthServiceClient(bearer{token: secret, rt: b.client.Transport}, origin, opts...)
		_, err := c.RevokeOtherSessions(t.Context(), connect.NewRequest(&dockwellv1.RevokeOtherSessionsRequest{}))
		var cerr *connect.Error
		if !errors.As(err, &cerr) || cerr.Code() != connect.CodePermissionDenied || reason(err) != authapi.ReasonNeedsSession ||
			!strings.Contains(cerr.Message(), "Sign in and do it in the panel") {
			t.Errorf("RevokeOtherSessions over %s with a token = %v (reason %q), want permission_denied needs_session", name, err, reason(err))
		}
		// The last digit is random, so it may already be the one a typo uses.
		typo := "x"
		if secret[52:] == typo {
			typo = "y"
		}
		c = dockwellv1connect.NewAuthServiceClient(bearer{token: secret[:52] + typo, rt: b.client.Transport}, origin, opts...)
		if _, err := c.ListSessions(t.Context(), connect.NewRequest(&dockwellv1.ListSessionsRequest{})); reason(err) != authapi.ReasonTokenRejected {
			t.Errorf("a mistyped token over %s = %v (reason %q), want token_rejected", name, err, reason(err))
		}
		// Public calls go ahead whatever the token.
		c = dockwellv1connect.NewAuthServiceClient(bearer{token: "nonsense", rt: b.client.Transport}, origin, opts...)
		if state, err := c.GetAuthState(t.Context(), connect.NewRequest(&dockwellv1.GetAuthStateRequest{})); err != nil || state.Msg.GetUser() != nil {
			t.Errorf("GetAuthState over %s with a wrong token = %v, %v, want it signed out", name, state, err)
		}
	}
}

// A token's expiry can be a ttl, which the control plane adds to its own
// clock, so a client whose clock is off doesn't move it.
func TestCreateTokenWithATTL(t *testing.T) { storetest.Each(t, testCreateTokenWithATTL) }

func testCreateTokenWithATTL(t *testing.T, backend store.Backend) {
	srv := startFull(t, backend, nil)
	_, port, _ := net.SplitHostPort(srv.api)
	b := newBrowser(t, "http://localhost:"+port, srv.api, nil)
	link, _ := srv.auth.CreateClaimLink(t.Context(), "test")
	b.claim(t, link.Token)

	before := srv.auth.Now().Truncate(time.Millisecond)
	proof := stepUpFor(t, b, dockwellv1.StepUpAction_STEP_UP_ACTION_CREATE_TOKEN, "")
	status, why, body := callREST(t, b, http.MethodPost, "/v1/tokens", nil, `{"name": "CI", "scope": "TOKEN_SCOPE_READ_ONLY", "ttl": "3600s", "proof": "`+proof+`"}`)
	after := srv.auth.Now()
	var made struct {
		Token struct {
			ExpireTime time.Time `json:"expireTime"`
		} `json:"token"`
		Secret string `json:"secret"`
	}
	if err := json.Unmarshal([]byte(body), &made); status != http.StatusOK || err != nil || made.Secret == "" {
		t.Fatalf("POST /v1/tokens with a ttl = %d (reason %q) %s", status, why, body)
	}
	if exp := made.Token.ExpireTime; exp.Before(before.Add(time.Hour)) || exp.After(after.Add(time.Hour)) {
		t.Errorf("a token with a ttl of an hour expires at %v, want an hour after %v", exp, before)
	}
	for _, bad := range []string{
		`"ttl": "0s"`,
		`"ttl": "-60s"`,
		`"ttl": "60s", "expireTime": "2099-01-01T00:00:00Z"`,
	} {
		status, _, body := callREST(t, b, http.MethodPost, "/v1/tokens", nil, `{"name": "CI", "scope": "TOKEN_SCOPE_READ_ONLY", `+bad+`}`)
		if status != http.StatusBadRequest {
			t.Errorf("POST /v1/tokens with %s = %d %s, want 400", bad, status, body)
		}
	}
}

// Every REST path finds its own method before Vanguard reads it, so who's calling is
// checked for that method first, whatever its path variables hold.
func TestEveryRESTPathFindsItsMethod(t *testing.T) {
	routes := restRoutes()
	for _, m := range restMethods(t) {
		got, ok := routes.procedureOf(m.verb, fillPath(m.path))
		if !ok || got != m.procedure {
			t.Errorf("%s %s finds %q (%v), want %s", m.verb, m.path, got, ok, m.procedure)
		}
	}
	if _, ok := routes.procedureOf(http.MethodGet, "/v1/nothing/here"); ok {
		t.Error("a path no rule names found a method")
	}
}

// Who calls a REST path, and their role, are checked before its query and body
// are read: someone signed out learns they're signed out, and someone whose
// role doesn't cover the call learns that, however wrong the request.
func TestRESTChecksTheCallerBeforeTheRequest(t *testing.T) {
	storetest.Each(t, func(t *testing.T, backend store.Backend) {
		f := startFull(t, backend, nil)
		b := signedIn(t, f)
		anon := newBrowser(t, b.origin, f.api, nil)
		hold := aMember(t, f)
		hold(onInstall(store.RoleViewer))
		for _, tt := range []struct {
			who            *browser
			verb, path     string
			body           string
			status         int
			reason, saying string
		}{
			{anon, http.MethodGet, "/v1/auditEvents?pageSize=notanumber", "", http.StatusUnauthorized, "unauthenticated", ""},
			{anon, http.MethodPost, "/v1/settings:setTimeZone", "{not json", http.StatusUnauthorized, "unauthenticated", ""},
			{b, http.MethodGet, "/v1/auditEvents?pageSize=notanumber", "", http.StatusForbidden, "needs_role", "only an admin or the owner can do this, and you're a viewer"},
			{b, http.MethodPost, "/v1/settings:setTimeZone", "{not json", http.StatusForbidden, "needs_role", "only an admin or the owner"},
			{b, http.MethodPost, "/v1/servers/srv_nothere:setPublicAddress", `{"ipv4":{"address":"192.0.2.10"}}`, http.StatusForbidden, "needs_role", ""},
			// What the caller may make is still read as Vanguard reads it.
			{b, http.MethodGet, "/v1/events?pageSize=notanumber", "", http.StatusBadRequest, "", ""},
		} {
			status, reason, body := callREST(t, tt.who, tt.verb, tt.path, nil, tt.body)
			var said rpcStatus
			_ = json.Unmarshal([]byte(body), &said)
			if status != tt.status || reason != tt.reason || !strings.Contains(said.Message, tt.saying) {
				t.Errorf("%s %s: %d %q %s, want %d %q saying %q", tt.verb, tt.path, status, reason, body, tt.status, tt.reason, tt.saying)
			}
		}
	})
}
