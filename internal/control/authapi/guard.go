// SPDX-License-Identifier: AGPL-3.0-only

package authapi

import (
	"context"
	"crypto/tls"
	"encoding/json"
	"errors"
	"log/slog"
	"net"
	"net/http"
	"strings"

	"connectrpc.com/connect"
	"connectrpc.com/grpchealth"
	"google.golang.org/protobuf/proto"
	"google.golang.org/protobuf/reflect/protoreflect"
	"google.golang.org/protobuf/reflect/protoregistry"

	dockwellv1 "github.com/dockwell/dockwell/gen/dockwell/v1"
	"github.com/dockwell/dockwell/gen/dockwell/v1/dockwellv1connect"
	"github.com/dockwell/dockwell/internal/auth"
)

// publicProcedures work without a session: claiming, signing in, and what
// the sign-in screen and health checks need. Every other procedure on the
// API listener needs one. The agent listener is separate and uses mTLS
// (internal/control/agentapi).
var publicProcedures = map[string]bool{
	dockwellv1connect.AuthServiceGetAuthStateProcedure:        true,
	dockwellv1connect.AuthServiceCheckClaimProcedure:          true,
	dockwellv1connect.AuthServiceBeginClaimProcedure:          true,
	dockwellv1connect.AuthServiceFinishClaimProcedure:         true,
	dockwellv1connect.AuthServiceBeginPasskeySignInProcedure:  true,
	dockwellv1connect.AuthServiceFinishPasskeySignInProcedure: true,
	dockwellv1connect.AuthServiceSignInWithCodeProcedure:      true,
	dockwellv1connect.AuthServiceSignInWithLinkProcedure:      true,
	// The sign-in screen shows the version.
	dockwellv1connect.SystemServiceGetVersionProcedure: true,
	// The gRPC equivalent of /healthz, for supervisors that speak gRPC.
	"/" + grpchealth.HealthV1ServiceName + "/Check": true,
	"/" + grpchealth.HealthV1ServiceName + "/Watch": true,
}

// PublicProcedures lists the procedures that work without a session.
func PublicProcedures() []string {
	out := make([]string, 0, len(publicProcedures))
	for p := range publicProcedures {
		out = append(out, p)
	}
	return out
}

type (
	requestKey   struct{}
	principalKey struct{}
	// uncheckedKey marks a request whose session couldn't be checked, as
	// when the database failed: it may still work.
	uncheckedKey struct{}
)

// request is what Guard learned about an HTTP request.
type request struct {
	client auth.Client
	// token is the session cookie's value. Empty when there is none, or the
	// request came with an API token, which takes its place.
	token string
	// bearer is the API token from the Authorization header, when hasBearer.
	bearer    string
	hasBearer bool
	// sameOrigin is true when the browser said the request came from the
	// panel's own origin, by its Origin or Sec-Fetch-Site header.
	sameOrigin bool
	get        bool
}

// ClientFrom returns who is asking, as Guard resolved it.
func ClientFrom(ctx context.Context) (auth.Client, bool) {
	r, ok := ctx.Value(requestKey{}).(request)
	return r.client, ok
}

// PrincipalFrom returns the signed-in user behind a request, if any.
func PrincipalFrom(ctx context.Context) (auth.Principal, bool) {
	p, ok := ctx.Value(principalKey{}).(auth.Principal)
	return p, ok
}

// sessionUnchecked reports a request with a session cookie that couldn't be
// checked, which went ahead without a principal because its procedure
// needs none.
func sessionUnchecked(ctx context.Context) bool {
	unchecked, _ := ctx.Value(uncheckedKey{}).(bool)
	return unchecked
}

// Guard wraps every handler on the API listeners. It:
//   - answers only for the hosts the panel lives on (Host check), which
//     also defeats DNS rebinding;
//   - refuses API requests a browser made from another origin, by Origin
//     and Sec-Fetch-Site (CSRF, on top of SameSite=Strict cookies and the
//     non-simple content types Connect uses), whether they use the
//     procedure paths or the REST paths, which accept simple requests;
//   - sends a browser's page loads on a loopback IP to localhost, where
//     passkeys work;
//   - marks HTTPS responses with HSTS, and API responses as uncacheable;
//   - and records the client, and the session cookie or the API token, for
//     Interceptor.
//
// A request with an API token ("Authorization: Bearer dwt_…") is
// authenticated by the token alone: its cookie is ignored, and it skips the
// origin checks. Those guard the cookie a browser adds to every request on
// its own; no browser adds an Authorization header for a page, nor lets a
// page from another origin send one without a CORS preflight, which the API
// never allows. A bearer that isn't a Dockwell token, as an auth proxy in
// front adds, is ignored like any other scheme: the cookie and the origin
// checks apply as if it weren't there.
//
// Its own refusals on REST paths are a google.rpc.Status in JSON, as every
// other error there is; elsewhere they are text, with the reason under
// ReasonKey either way.
func Guard(sites auth.Sites, logger *slog.Logger, next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		site, err := sites.Resolve(r)
		if err != nil {
			logger.DebugContext(r.Context(), "refused a request for another host", slog.String("host", r.Host), slog.String("from", r.RemoteAddr))
			// No code means another host, and the request is the client's to
			// fix, so REST gets invalid_argument with the 421.
			refuse(w, r, http.StatusMisdirectedRequest, connect.CodeInvalidArgument, ReasonMisdirected,
				sentence(strings.TrimPrefix(err.Error(), auth.ErrMisdirected.Error()+": ")))
			return
		}
		if site.Secure {
			w.Header().Set("Strict-Transport-Security", "max-age=63072000")
		}
		if !site.CanSignIn() && isNavigation(r) {
			_, port, _ := net.SplitHostPort(r.Host)
			target := "http://localhost"
			if port != "" {
				target += ":" + port
			}
			// The host is fixed: only the port and path come from the request.
			http.Redirect(w, r, target+r.URL.RequestURI(), http.StatusTemporaryRedirect) //nolint:gosec // see above
			return
		}
		req := request{
			client: auth.Client{Site: site, Addr: r.RemoteAddr, UserAgent: r.UserAgent()},
			get:    r.Method == http.MethodGet || r.Method == http.MethodHead,
		}
		req.bearer, req.hasBearer = bearerToken(r)
		if !req.hasBearer && strings.HasPrefix(strings.ToLower(strings.TrimSpace(r.Header.Get("Authorization"))), "bearer ") {
			logger.DebugContext(r.Context(), "ignored a bearer that isn't a Dockwell API token, as a proxy in front may add",
				slog.String("path", r.URL.Path), slog.String("from", r.RemoteAddr))
		}
		// Page loads and assets may come from anywhere, such as a link in an
		// email; SameSite=Strict keeps the cookie off a cross-site navigation.
		// The API answers only the panel's own origin, unless the request
		// came with an API token instead of the cookie.
		if isAPI(r.URL.Path) {
			if !req.hasBearer {
				var err error
				if req.sameOrigin, err = checkOrigin(r, site); err != nil {
					logger.InfoContext(r.Context(), "refused a cross-origin request", slog.String("origin", r.Header.Get("Origin")),
						slog.String("path", r.URL.Path), slog.String("from", r.RemoteAddr))
					refuse(w, r, http.StatusForbidden, connect.CodePermissionDenied, ReasonOrigin, err.Error())
					return
				}
			}
			w.Header().Set("Cache-Control", "no-store")
		}
		if c, err := r.Cookie(site.CookieName()); err == nil && !req.hasBearer {
			req.token = c.Value
		}
		next.ServeHTTP(w, r.WithContext(context.WithValue(r.Context(), requestKey{}, req)))
	})
}

// checkOrigin refuses a request a browser sent from another origin. A
// present Origin header must be the site's own origin, and Sec-Fetch-Site,
// when present, must say same-origin, or none for a navigation the person
// started. Clients that aren't browsers send neither; they can't be tricked
// into sending a cookie, and Interceptor makes cookie-authenticated writes
// prove they came from the panel. It reports whether the browser said the
// request came from the panel's origin.
func checkOrigin(r *http.Request, site auth.Site) (bool, error) {
	origin := r.Header.Get("Origin")
	fetch := r.Header.Get("Sec-Fetch-Site")
	if origin != "" && !strings.EqualFold(origin, site.Origin) {
		return false, errors.New("Dockwell refused a request from " + origin + ", which isn't the panel's origin " + site.Origin + ".")
	}
	switch fetch {
	case "", "same-origin", "none":
	default:
		return false, errors.New("Dockwell refused a request another site made. Open the panel at " + site.Origin + " and try there.")
	}
	return origin != "" || fetch == "same-origin", nil
}

// bearerToken returns the Dockwell API token in a request's Authorization
// header, and whether it has one: a bearer that starts with "dwt_". "Bearer"
// with nothing after it, as a script with an empty variable sends, is a
// token that fails; no proxy sends that. Any other bearer, such as the one
// oauth2-proxy or Pomerium add, and any other scheme, such as Basic, isn't
// Dockwell's and is ignored.
func bearerToken(r *http.Request) (string, bool) {
	scheme, token, _ := strings.Cut(strings.TrimSpace(r.Header.Get("Authorization")), " ")
	token = strings.TrimSpace(token)
	if !strings.EqualFold(scheme, "Bearer") || token != "" && !strings.HasPrefix(token, auth.TokenPrefix) {
		return "", false
	}
	return token, true
}

// isNavigation reports a browser loading a page, by the fetch metadata every
// current browser sends. Tools such as curl send none, and get the page
// where they asked for it.
func isNavigation(r *http.Request) bool {
	return r.Method == http.MethodGet && r.Header.Get("Sec-Fetch-Mode") == "navigate" && !isAPI(r.URL.Path)
}

// RESTPrefix starts every REST path, and nothing else: the panel has no
// pages under it (docs/adr/0015-rest-and-openapi-from-the-schema.md).
const RESTPrefix = "/v1/"

// isAPI reports a path the API serves: a Connect or gRPC procedure, or a
// REST path.
func isAPI(path string) bool {
	return isRPC(path) || strings.HasPrefix(path, RESTPrefix)
}

// isRPC reports a Connect or gRPC procedure path, which always has the form
// /package.Service/Method.
func isRPC(path string) bool {
	service, _, ok := strings.Cut(strings.TrimPrefix(path, "/"), "/")
	return ok && strings.Contains(service, ".")
}

// refuse answers a request Guard refuses: on a REST path, as REST answers
// errors, and elsewhere in text.
func refuse(w http.ResponseWriter, r *http.Request, status int, code connect.Code, reason, msg string) {
	if strings.HasPrefix(r.URL.Path, RESTPrefix) {
		RESTError(w, status, code, reason, msg)
		return
	}
	plain(w, status, reason, msg)
}

// plain answers with a message for people, and its reason under ReasonKey
// for clients, which Connect clients read as the error's metadata.
func plain(w http.ResponseWriter, code int, reason, msg string) {
	w.Header().Set(ReasonKey, reason)
	w.Header().Set("Content-Type", "text/plain; charset=utf-8")
	w.Header().Set("Cache-Control", "no-store")
	w.Header().Set("X-Content-Type-Options", "nosniff")
	w.WriteHeader(code)
	_, _ = w.Write([]byte(msg + "\n")) //nolint:gosec // text/plain with nosniff, never HTML
}

// RESTError answers a REST path's error the way Vanguard answers the rest:
// a google.rpc.Status in JSON, with code, and with reason, when there is
// one, under ReasonKey. status is the HTTP status, which is usually the one
// for code.
func RESTError(w http.ResponseWriter, status int, code connect.Code, reason, msg string) {
	if reason != "" {
		w.Header().Set(ReasonKey, reason)
	}
	w.Header().Set("Content-Type", "application/json")
	w.Header().Set("Cache-Control", "no-store")
	w.Header().Set("X-Content-Type-Options", "nosniff")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(struct {
		Code    int    `json:"code"`
		Message string `json:"message"`
		Details []any  `json:"details"`
	}{int(code), msg, []any{}})
}

// sentence capitalizes the first letter of an error message for people.
func sentence(s string) string {
	if s == "" {
		return s
	}
	s = strings.ToUpper(s[:1]) + s[1:]
	if !strings.HasSuffix(s, ".") {
		s += "."
	}
	return s
}

// errSessionEnded answers a session cookie that no longer works.
var errSessionEnded = errors.New("your session ended. Sign in again")

// Interceptor enforces sign-in on every procedure it wraps, whatever the
// protocol: REST paths reach the same handlers through Vanguard. A
// procedure not in publicProcedures needs a live session or an API token
// whose scope covers it, and a write authenticated by cookie must come from
// the panel's own origin. It fails closed: a request that didn't pass
// through Guard, or a nil service, gets no session.
func Interceptor(svc *auth.Service, logger *slog.Logger) connect.Interceptor {
	return &interceptor{svc: svc, logger: logger}
}

type interceptor struct {
	svc    *auth.Service
	logger *slog.Logger
}

// sessionOnly are the services no API token may call: how you sign in, and
// your tokens, change only from a session.
var sessionOnly = []string{dockwellv1connect.AuthServiceName, dockwellv1connect.TokenServiceName}

// accessFor is what a procedure needs of an API token, by its effect.
// Anything unknown needs a session.
func accessFor(procedure string) auth.Access {
	for _, svc := range sessionOnly {
		if strings.HasPrefix(procedure, "/"+svc+"/") {
			return auth.AccessSession
		}
	}
	if a, ok := tokenAccess[procedure]; ok {
		return a
	}
	return auth.AccessSession
}

// tokenAccess maps each procedure of dockwell.v1 to what it needs of a
// token, by its (dockwell.v1.effect), and lets any token read the schema
// through reflection.
var tokenAccess = func() map[string]auth.Access {
	m := map[string]auth.Access{
		"/grpc.reflection.v1.ServerReflection/ServerReflectionInfo":      auth.AccessRead,
		"/grpc.reflection.v1alpha.ServerReflection/ServerReflectionInfo": auth.AccessRead,
	}
	protoregistry.GlobalFiles.RangeFilesByPackage("dockwell.v1", func(fd protoreflect.FileDescriptor) bool {
		for i := range fd.Services().Len() {
			sd := fd.Services().Get(i)
			for j := range sd.Methods().Len() {
				md := sd.Methods().Get(j)
				effect, _ := proto.GetExtension(md.Options(), dockwellv1.E_Effect).(dockwellv1.Effect)
				procedure := "/" + string(sd.FullName()) + "/" + string(md.Name())
				if person, _ := proto.GetExtension(md.Options(), dockwellv1.E_NeedsSession).(bool); person {
					// It needs a person, whatever its effect.
					m[procedure] = auth.AccessSession
					continue
				}
				switch effect {
				case dockwellv1.Effect_EFFECT_READ:
					m[procedure] = auth.AccessRead
				case dockwellv1.Effect_EFFECT_PLAN, dockwellv1.Effect_EFFECT_APPLY:
					m[procedure] = auth.AccessWrite
				default:
					m[procedure] = auth.AccessSession
				}
			}
		}
		return true
	})
	return m
}()

func (i *interceptor) WrapUnary(next connect.UnaryFunc) connect.UnaryFunc {
	return func(ctx context.Context, req connect.AnyRequest) (connect.AnyResponse, error) {
		ctx, err := i.authorize(ctx, req.Spec().Procedure)
		if err != nil {
			return nil, err
		}
		return next(ctx, req)
	}
}

func (*interceptor) WrapStreamingClient(next connect.StreamingClientFunc) connect.StreamingClientFunc {
	return next
}

func (i *interceptor) WrapStreamingHandler(next connect.StreamingHandlerFunc) connect.StreamingHandlerFunc {
	return func(ctx context.Context, conn connect.StreamingHandlerConn) error {
		ctx, err := i.authorize(ctx, conn.Spec().Procedure)
		if err != nil {
			return err
		}
		// A stream ends when its session or token does: revoked, signed out
		// or expired.
		p, ok := PrincipalFrom(ctx)
		if !ok {
			return next(ctx, conn)
		}
		client, _ := ClientFrom(ctx)
		tracked, stop := i.svc.Track(ctx, client, p)
		defer stop()
		err = next(tracked, conn)
		if ctx.Err() == nil && tracked.Err() != nil {
			if p.Token != nil {
				return ReasonError(connect.CodeUnauthenticated, auth.ErrTokenRejected, ReasonTokenRejected)
			}
			return ReasonError(connect.CodeUnauthenticated, errSessionEnded, ReasonSessionEnded)
		}
		return err
	}
}

func (i *interceptor) authorize(ctx context.Context, procedure string) (context.Context, error) {
	req, ok := ctx.Value(requestKey{}).(request)
	if !ok {
		// Not through Guard: no site, so no session.
		if publicProcedures[procedure] && !strings.HasPrefix(procedure, "/"+dockwellv1connect.AuthServiceName+"/") {
			return ctx, nil
		}
		return ctx, ReasonError(connect.CodeUnauthenticated, auth.ErrUnauthenticated, ReasonUnauthenticated)
	}
	if req.hasBearer {
		if publicProcedures[procedure] {
			// A token adds nothing to a call that needs no session, so it
			// goes ahead as if signed out, whatever the token.
			return ctx, nil
		}
		return i.authorizeToken(ctx, req, procedure)
	}
	if req.token != "" && i.svc != nil {
		p, err := i.svc.Authenticate(ctx, req.client, req.token)
		switch {
		case err == nil:
			ctx = context.WithValue(ctx, principalKey{}, p)
		case errors.Is(err, auth.ErrUnauthenticated):
		case publicProcedures[procedure]:
			// The call needs no session, so one that can't be checked
			// doesn't stop it: signing in, the version and health still
			// answer. It goes ahead as if signed out.
			i.logger.WarnContext(ctx, "can't check a session; going on without it, as the call needs none",
				slog.String("procedure", procedure), slog.Any("error", err))
			ctx = context.WithValue(ctx, uncheckedKey{}, true)
		default:
			i.logger.ErrorContext(ctx, "can't check a session", slog.Any("error", err))
			return ctx, ReasonError(connect.CodeUnavailable, errors.New("can't check your session right now. Try again in a moment"), ReasonUnavailable)
		}
	}
	if publicProcedures[procedure] {
		return ctx, nil
	}
	if _, ok := PrincipalFrom(ctx); !ok {
		if req.token != "" {
			return ctx, ReasonError(connect.CodeUnauthenticated, errSessionEnded, ReasonSessionEnded)
		}
		return ctx, ReasonError(connect.CodeUnauthenticated, auth.ErrUnauthenticated, ReasonUnauthenticated)
	}
	if !req.get && !req.sameOrigin {
		return ctx, ReasonError(connect.CodePermissionDenied,
			errors.New("refused a change made with a session cookie but no Origin header, so it may not come from the panel. Make the change in the panel"),
			ReasonOrigin)
	}
	return ctx, nil
}

// authorizeToken checks the API token a call came with, and that its scope
// covers the procedure. A call with a token needs no origin: see Guard.
func (i *interceptor) authorizeToken(ctx context.Context, req request, procedure string) (context.Context, error) {
	if i.svc == nil {
		return ctx, ReasonError(connect.CodeUnauthenticated, auth.ErrTokenRejected, ReasonTokenRejected)
	}
	p, err := i.svc.AuthenticateToken(ctx, req.client, req.bearer, accessFor(procedure), procedure)
	switch {
	case err == nil:
		return context.WithValue(ctx, principalKey{}, p), nil
	case errors.Is(err, auth.ErrTokenRejected):
		return ctx, ReasonError(connect.CodeUnauthenticated, auth.ErrTokenRejected, ReasonTokenRejected)
	case errors.Is(err, auth.ErrTokenScope):
		return ctx, ReasonError(connect.CodePermissionDenied, auth.ErrTokenScope, ReasonTokenScope)
	case errors.Is(err, auth.ErrNeedsSession):
		return ctx, ReasonError(connect.CodePermissionDenied, auth.ErrNeedsSession, ReasonNeedsSession)
	case errors.Is(err, context.Canceled):
		return ctx, connect.NewError(connect.CodeCanceled, err)
	default:
		i.logger.ErrorContext(ctx, "can't check an API token", slog.Any("error", err))
		return ctx, ReasonError(connect.CodeUnavailable, errors.New("can't check your API token right now. Try again in a moment"), ReasonUnavailable)
	}
}

// ViaIngress marks what arrives on the panel's proxied listener as the HTTPS
// traffic it is, for that listener alone. Only this server's proxy and root
// reach that socket, so the listener is the proof: no header says anything
// about it, and the loopback listener reads none of them. RemoteAddr comes
// from what the proxy wrote in X-Forwarded-For, which it overwrites whatever a
// client sends. Host, Origin and Sec-Fetch-Site are as the browser sent them,
// and the guard checks them as it does any request (ADR 0024).
func ViaIngress(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		via := *r
		host := r.Host
		if h, _, err := net.SplitHostPort(host); err == nil {
			host = h
		}
		via.TLS = &tls.ConnectionState{Version: tls.VersionTLS13, HandshakeComplete: true, ServerName: host}
		via.RemoteAddr = net.JoinHostPort(forwardedClient(r), "0")
		next.ServeHTTP(w, &via)
	})
}

// forwardedClient is the address the proxy says a request came from, or none
// that could pass for loopback.
func forwardedClient(r *http.Request) string {
	list := strings.Split(r.Header.Get("X-Forwarded-For"), ",")
	if ip := net.ParseIP(strings.TrimSpace(list[len(list)-1])); ip != nil {
		return ip.String()
	}
	return "0.0.0.0"
}
