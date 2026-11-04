// SPDX-License-Identifier: AGPL-3.0-only

package authapi

import (
	"context"
	"errors"
	"log/slog"
	"net"
	"net/http"
	"strings"

	"connectrpc.com/connect"
	"connectrpc.com/grpchealth"

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
)

// request is what Guard learned about an HTTP request.
type request struct {
	client auth.Client
	token  string
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

// Guard wraps every handler on the API listeners. It:
//   - answers only for the hosts the panel lives on (Host check), which
//     also defeats DNS rebinding;
//   - refuses API requests a browser made from another origin, by Origin
//     and Sec-Fetch-Site (CSRF, on top of SameSite=Strict cookies and the
//     non-simple content types Connect uses);
//   - sends a browser's page loads on a loopback IP to localhost, where
//     passkeys work;
//   - marks HTTPS responses with HSTS, and API responses as uncacheable;
//   - and records the client and session cookie for Interceptor.
func Guard(sites auth.Sites, logger *slog.Logger, next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		site, err := sites.Resolve(r)
		if err != nil {
			logger.DebugContext(r.Context(), "refused a request for another host", slog.String("host", r.Host), slog.String("from", r.RemoteAddr))
			plain(w, http.StatusMisdirectedRequest, ReasonMisdirected, sentence(strings.TrimPrefix(err.Error(), auth.ErrMisdirected.Error()+": ")))
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
		// Page loads and assets may come from anywhere, such as a link in an
		// email; SameSite=Strict keeps the cookie off a cross-site navigation.
		// The API answers only the panel's own origin.
		var sameOrigin bool
		if isRPC(r.URL.Path) {
			var err error
			if sameOrigin, err = checkOrigin(r, site); err != nil {
				logger.InfoContext(r.Context(), "refused a cross-origin request", slog.String("origin", r.Header.Get("Origin")),
					slog.String("path", r.URL.Path), slog.String("from", r.RemoteAddr))
				plain(w, http.StatusForbidden, ReasonOrigin, err.Error())
				return
			}
			w.Header().Set("Cache-Control", "no-store")
		}
		req := request{
			client:     auth.Client{Site: site, Addr: r.RemoteAddr, UserAgent: r.UserAgent()},
			sameOrigin: sameOrigin,
			get:        r.Method == http.MethodGet || r.Method == http.MethodHead,
		}
		if c, err := r.Cookie(site.CookieName()); err == nil {
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

// isNavigation reports a browser loading a page, by the fetch metadata every
// current browser sends. Tools such as curl send none, and get the page
// where they asked for it.
func isNavigation(r *http.Request) bool {
	return r.Method == http.MethodGet && r.Header.Get("Sec-Fetch-Mode") == "navigate" && !isRPC(r.URL.Path)
}

// isRPC reports a Connect or gRPC procedure path, which always has the form
// /package.Service/Method.
func isRPC(path string) bool {
	service, _, ok := strings.Cut(strings.TrimPrefix(path, "/"), "/")
	return ok && strings.Contains(service, ".")
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

// Interceptor enforces sign-in on every procedure it wraps: a procedure not
// in publicProcedures needs a live session, and a write authenticated by
// cookie must come from the panel's own origin. It fails closed: a request
// that didn't pass through Guard, or a nil service, gets no session.
func Interceptor(svc *auth.Service, logger *slog.Logger) connect.Interceptor {
	return &interceptor{svc: svc, logger: logger}
}

type interceptor struct {
	svc    *auth.Service
	logger *slog.Logger
}

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
		// A stream ends when its session does: revoked, signed out or expired.
		p, ok := PrincipalFrom(ctx)
		if !ok {
			return next(ctx, conn)
		}
		client, _ := ClientFrom(ctx)
		tracked, stop := i.svc.Track(ctx, client, p)
		defer stop()
		err = next(tracked, conn)
		if ctx.Err() == nil && tracked.Err() != nil {
			return reasonError(connect.CodeUnauthenticated, errSessionEnded, ReasonSessionEnded)
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
		return ctx, reasonError(connect.CodeUnauthenticated, auth.ErrUnauthenticated, ReasonUnauthenticated)
	}
	if req.token != "" && i.svc != nil {
		p, err := i.svc.Authenticate(ctx, req.client, req.token)
		switch {
		case err == nil:
			ctx = context.WithValue(ctx, principalKey{}, p)
		case errors.Is(err, auth.ErrUnauthenticated):
		default:
			i.logger.ErrorContext(ctx, "can't check a session", slog.Any("error", err))
			return ctx, reasonError(connect.CodeUnavailable, errors.New("can't check your session right now. Try again in a moment"), ReasonUnavailable)
		}
	}
	if publicProcedures[procedure] {
		return ctx, nil
	}
	if _, ok := PrincipalFrom(ctx); !ok {
		if req.token != "" {
			return ctx, reasonError(connect.CodeUnauthenticated, errSessionEnded, ReasonSessionEnded)
		}
		return ctx, reasonError(connect.CodeUnauthenticated, auth.ErrUnauthenticated, ReasonUnauthenticated)
	}
	if !req.get && !req.sameOrigin {
		return ctx, reasonError(connect.CodePermissionDenied,
			errors.New("refused a change made with a session cookie but no Origin header, so it may not come from the panel. Make the change in the panel"),
			ReasonOrigin)
	}
	return ctx, nil
}
