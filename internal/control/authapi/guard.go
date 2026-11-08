// SPDX-License-Identifier: AGPL-3.0-only

package authapi

import (
	"context"
	"crypto/tls"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"net"
	"net/http"
	"strings"
	"sync"

	"connectrpc.com/connect"
	"connectrpc.com/grpchealth"
	"google.golang.org/protobuf/proto"
	"google.golang.org/protobuf/reflect/protoreflect"
	"google.golang.org/protobuf/reflect/protoregistry"

	dockwellv1 "github.com/dockwell/dockwell/gen/dockwell/v1"
	"github.com/dockwell/dockwell/gen/dockwell/v1/dockwellv1connect"
	"github.com/dockwell/dockwell/internal/auth"
	"github.com/dockwell/dockwell/internal/control/place"
	"github.com/dockwell/dockwell/internal/store"
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
	// Someone invited accepts before they sign in.
	dockwellv1connect.AuthServiceCheckInviteProcedure:        true,
	dockwellv1connect.AuthServiceBeginAcceptInviteProcedure:  true,
	dockwellv1connect.AuthServiceFinishAcceptInviteProcedure: true,
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

// Authorizer is the check Interceptor makes of a call to procedure, for what
// reads a call before Interceptor sees it, such as the REST paths, which read
// path variables first: it refuses what Interceptor would, with the same error.
func Authorizer(svc *auth.Service, logger *slog.Logger) func(ctx context.Context, procedure string) error {
	i := &interceptor{svc: svc, logger: logger}
	return func(ctx context.Context, procedure string) error {
		_, err := i.authorize(ctx, procedure)
		return err
	}
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
					if proto.GetExtension(md.Options(), dockwellv1.E_NeedsGrant) == dockwellv1.TokenGrant_TOKEN_GRANT_LOGS {
						m[procedure] = auth.AccessLogs
					}
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

// rule is what a procedure needs of a role (ADR 0049): the lowest role it
// needs anywhere, which the interceptor checks first, and what its objects are.
type rule struct {
	needs, needsProtected dockwellv1.Needs
	reach                 dockwellv1.Reach
	effect                dockwellv1.Effect
}

// roleRules maps each procedure of dockwell.v1 to its rule, from its
// (dockwell.v1.min_role) and (dockwell.v1.reach). A procedure without one needs
// the owner, so a new method is closed until it says.
var roleRules = func() map[string]rule {
	m := map[string]rule{}
	protoregistry.GlobalFiles.RangeFilesByPackage("dockwell.v1", func(fd protoreflect.FileDescriptor) bool {
		for i := range fd.Services().Len() {
			sd := fd.Services().Get(i)
			for j := range sd.Methods().Len() {
				md := sd.Methods().Get(j)
				needs, _ := proto.GetExtension(md.Options(), dockwellv1.E_MinRole).(dockwellv1.Needs)
				protected, _ := proto.GetExtension(md.Options(), dockwellv1.E_MinRoleProtected).(dockwellv1.Needs)
				reach, _ := proto.GetExtension(md.Options(), dockwellv1.E_Reach).(dockwellv1.Reach)
				effect, _ := proto.GetExtension(md.Options(), dockwellv1.E_Effect).(dockwellv1.Effect)
				m["/"+string(sd.FullName())+"/"+string(md.Name())] = rule{needs: needs, needsProtected: protected, reach: reach, effect: effect}
			}
		}
		return true
	})
	return m
}()

// roleOf is the store's role a Needs names, or "" for none.
func roleOf(n dockwellv1.Needs) string {
	switch n {
	case dockwellv1.Needs_NEEDS_VIEWER:
		return store.RoleViewer
	case dockwellv1.Needs_NEEDS_DEVELOPER:
		return store.RoleDeveloper
	case dockwellv1.Needs_NEEDS_ADMIN:
		return store.RoleAdmin
	case dockwellv1.Needs_NEEDS_NOTHING:
		return ""
	case dockwellv1.Needs_NEEDS_OWNER, dockwellv1.Needs_NEEDS_UNSPECIFIED:
		// Unset is the owner's, so a method that doesn't say is closed.
	}
	return store.RoleOwner
}

// gate checks the lowest role a procedure needs anywhere against the highest
// the caller holds, a person's own or a token's maker's, read now. It's the
// first of the checks roles make: where the call's objects are is checked
// after (ADR 0048, ADR 0049).
// It returns ctx with the caller's grants, for what's checked after.
func (i *interceptor) gate(ctx context.Context, p auth.Principal, procedure string) (context.Context, error) {
	ctx, _, _, err := i.check(ctx, p, procedure)
	return ctx, err
}

// gateCall is gate for a call as it opens, which audits a refusal for want of
// a role. A stream's rechecks don't: what ended it was a change to a grant,
// audited as it was made.
func (i *interceptor) gateCall(ctx context.Context, client auth.Client, p auth.Principal, procedure string) (context.Context, error) {
	ctx, held, needs, err := i.check(ctx, p, procedure)
	if err != nil && needs != "" {
		i.svc.RefuseRole(ctx, client, p, procedure, held, needs)
	}
	return ctx, err
}

// check is the gate, saying when it refuses what the caller holds and what the
// call needs.
func (i *interceptor) check(ctx context.Context, p auth.Principal, procedure string) (_ context.Context, held, needs string, _ error) {
	r, known := roleRules[procedure]
	if !known && unroled[procedure] {
		return ctx, "", "", nil // gRPC's health and reflection, which read nothing of anyone's
	}
	needs = roleOf(r.needs)
	if needs == "" {
		return ctx, "", "", nil
	}
	grants, err := i.svc.Grants(ctx, p)
	if err != nil {
		i.logger.ErrorContext(ctx, "can't read what someone may do", slog.Any("error", err))
		return ctx, "", "", ReasonError(connect.CodeUnavailable, errors.New("can't check your role right now, so this didn't happen. Try again in a moment"), ReasonUnavailable)
	}
	held = auth.Highest(grants)
	if store.RoleRank(held) >= store.RoleRank(needs) {
		return place.WithCaller(ctx, place.Caller{UserID: p.User.ID, Grants: grants}), "", "", nil
	}
	if why, ok := adminsBecause[procedure]; ok && held != "" && needs == store.RoleAdmin {
		return ctx, held, needs, ReasonError(connect.CodePermissionDenied,
			fmt.Errorf("%s, which only an admin or the owner can do, and %s, so it didn't happen. Ask one of them", why, youAre(p, held)), ReasonNeedsRole)
	}
	return ctx, held, needs, ReasonError(connect.CodePermissionDenied, roleRefusal(p, held, needs), ReasonNeedsRole)
}

// unroled are the calls outside dockwell.v1 that need no role: gRPC's health,
// for supervisors, and reflection, which says what the API is. Any other call
// without a rule is the owner's.
var unroled = map[string]bool{
	"/" + grpchealth.HealthV1ServiceName + "/Check":                  true,
	"/" + grpchealth.HealthV1ServiceName + "/Watch":                  true,
	"/grpc.reflection.v1.ServerReflection/ServerReflectionInfo":      true,
	"/grpc.reflection.v1alpha.ServerReflection/ServerReflectionInfo": true,
}

// grantsFrom are the caller's grants, as the gate read them for this call.
func grantsFrom(ctx context.Context) ([]store.Grant, bool) {
	c, ok := place.CallerFrom(ctx)
	return c.Grants, ok
}

// roleRefusal says what the call needs, what the caller holds, that it didn't
// happen, and who can change that.
func roleRefusal(p auth.Principal, held, needs string) error {
	who, whose := "you're "+aRole(held), "your"
	if p.Token != nil {
		who, whose = "this API token's maker is "+aRole(held), "its maker's"
	}
	switch {
	case held == "" && p.Token != nil:
		return errors.New("this API token's maker holds no role on this install any more, so it can't make calls. Ask an admin, or use another token")
	case held == "":
		return errors.New("you hold no role on this install any more, so you can only sign out. Ask an admin to add you again")
	case needs == store.RoleOwner:
		return fmt.Errorf("only the owner can do this, and %s, so it didn't happen. Ask the owner", who)
	}
	return fmt.Errorf("only %s can do this, and %s, so it didn't happen. Ask an admin to change %s role", atLeast(needs), who, whose)
}

// adminsBecause says why a call is an admin's, where its name alone doesn't.
var adminsBecause = map[string]string{
	dockwellv1connect.CatalogServiceInstallCatalogEntryProcedure: "installing from the catalog makes a project of its own",
	// A website is its project, and the engine holds a project to an admin.
	dockwellv1connect.WebsiteServiceUpdateWebsiteProcedure: "changing a website changes its project",
	dockwellv1connect.WebsiteServiceDeleteWebsiteProcedure: "deleting a website deletes its project",
}

// youAre says what the caller holds: "you're a developer", or for an API
// token, its maker's.
func youAre(p auth.Principal, held string) string {
	if p.Token != nil {
		return "this API token's maker is " + aRole(held)
	}
	return "you're " + aRole(held)
}

// aRole names a role with its article: "a viewer", "an admin".
func aRole(role string) string {
	if role == store.RoleAdmin || role == store.RoleOwner {
		return "an " + role
	}
	return "a " + role
}

// atLeast names who may do what a role may: "a developer or more", "an admin
// or the owner".
func atLeast(role string) string {
	switch role {
	case store.RoleAdmin:
		return "an admin or the owner"
	case store.RoleViewer:
		return "someone with a role"
	}
	return aRole(role) + " or more"
}

func (i *interceptor) WrapUnary(next connect.UnaryFunc) connect.UnaryFunc {
	return func(ctx context.Context, req connect.AnyRequest) (connect.AnyResponse, error) {
		procedure := req.Spec().Procedure
		ctx, err := i.authorize(ctx, procedure)
		if err != nil {
			return nil, err
		}
		if err := i.checkScope(ctx, procedure, req.Any()); err != nil {
			return nil, err
		}
		if err := i.openPage(ctx, procedure, req.Any()); err != nil {
			return nil, err
		}
		res, err := next(ctx, req)
		// Each question of which project, such as one an edit of a change asks,
		// answered for someone who holds one alone.
		for range 16 {
			again, cerr := i.theirProject(ctx, procedure, req.Any(), err)
			if cerr != nil {
				return nil, cerr
			}
			if !again {
				break
			}
			res, err = next(ctx, req)
		}
		if err != nil {
			return res, i.narrowChoice(ctx, procedure, err)
		}
		if _, err := i.filter(ctx, procedure, res.Any()); err != nil {
			i.logger.ErrorContext(ctx, "can't narrow a response to what its caller may see", slog.Any("error", err))
			return nil, ReasonError(connect.CodeUnavailable, errors.New("can't check what you may see right now. Try again in a moment"), ReasonUnavailable)
		}
		i.sealPage(ctx, procedure, res.Any())
		if trims(ctx, procedure) {
			trim(res.Any())
		}
		return res, nil
	}
}

// scopedConn checks what a stream's request names, narrows what it sends to the
// caller's places, and trims it to names and states for someone below an admin
// reading what projects name. Each recheck reads the caller's grants again and
// checks the request against them.
type scopedConn struct {
	connect.StreamingHandlerConn
	i    *interceptor
	trim bool

	mu     sync.Mutex
	ctx    context.Context //nolint:containedctx // the stream's own, with the grants last read
	req    any             // the request, once received
	ended  error           // why a recheck ended the stream
	failed int             // rechecks in a row that couldn't read the grants
}

// failedRechecks is how many rechecks in a row may fail to read a stream's
// grants before it ends: a failed read isn't a reason to close, as the
// session's own check rides one out, but a stream can't go on for long without
// knowing its caller may still make it.
const failedRechecks = 3

func (c *scopedConn) Receive(m any) error {
	if err := c.StreamingHandlerConn.Receive(m); err != nil {
		return err
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.req != nil {
		return nil
	}
	c.req = m
	if err := c.i.checkScope(c.ctx, c.Spec().Procedure, m); err != nil {
		return err
	}
	return c.i.openPage(c.ctx, c.Spec().Procedure, m)
}

// recheck reads the caller's grants again, as the gate does, and checks the
// request against them. What it read narrows what the stream sends next.
func (c *scopedConn) recheck(ctx context.Context, p auth.Principal) error {
	procedure := c.Spec().Procedure
	gated, err := c.i.gate(ctx, p, procedure)
	c.mu.Lock()
	defer c.mu.Unlock()
	if err == nil && c.req != nil {
		err = c.i.checkScope(gated, procedure, c.req)
	}
	if connect.CodeOf(err) == connect.CodeUnavailable {
		// The grants couldn't be read: what was read before still narrows what
		// the stream sends, until the next recheck decides.
		c.failed++
		if c.failed < failedRechecks {
			return nil
		}
	}
	if err != nil {
		c.ended = err
		return err
	}
	c.failed = 0
	// The stream's own context, with the grants just read.
	if caller, ok := place.CallerFrom(gated); ok {
		c.ctx = place.WithCaller(c.ctx, caller)
	}
	return nil
}

// endedBy is why a recheck ended the stream, or nil when it didn't.
func (c *scopedConn) endedBy() error {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.ended
}

func (c *scopedConn) Send(m any) error {
	c.mu.Lock()
	ctx := c.ctx
	c.mu.Unlock()
	skip, err := c.i.filter(ctx, c.Spec().Procedure, m)
	if err != nil {
		return ReasonError(connect.CodeUnavailable, errors.New("can't check what you may see right now. Try again in a moment"), ReasonUnavailable)
	}
	if skip {
		return nil
	}
	c.i.sealPage(ctx, c.Spec().Procedure, m)
	if c.trim {
		trim(m)
	}
	return c.StreamingHandlerConn.Send(m)
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
		procedure := conn.Spec().Procedure
		scoped := &scopedConn{StreamingHandlerConn: conn, i: i, trim: trims(ctx, procedure), ctx: ctx}
		// Each recheck asks the gate and the request's places again, so a stream
		// ends once its caller's role, or a token's maker's, no longer lets them
		// make it, or reach what it names (ADR 0048).
		still := func(recheck context.Context) error { return scoped.recheck(recheck, p) }
		tracked, stop := i.svc.Track(ctx, client, p, still)
		defer stop()
		err = i.narrowChoice(ctx, procedure, next(tracked, scoped))
		if ctx.Err() == nil && tracked.Err() != nil {
			if ended := scoped.endedBy(); ended != nil {
				return ended
			}
			if p.Token != nil {
				return ReasonError(connect.CodeUnauthenticated, auth.ErrTokenRejected, ReasonTokenRejected)
			}
			return ReasonError(connect.CodeUnauthenticated, errSessionEnded, ReasonSessionEnded)
		}
		return err
	}
}

// Admit checks a call to procedure as the interceptor does before its handler
// runs: who makes it, and that their role lets them. The REST paths ask it
// before Vanguard reads the request, so someone who can't make the call learns
// that, and not what's wrong with their query or body. The interceptor checks
// the call again after, so a path Admit isn't asked about loses nothing.
func Admit(ctx context.Context, svc *auth.Service, logger *slog.Logger, procedure string) error {
	_, err := (&interceptor{svc: svc, logger: logger}).authorize(ctx, procedure)
	return err
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
	p, ok := PrincipalFrom(ctx)
	if !ok {
		if req.token != "" {
			return ctx, ReasonError(connect.CodeUnauthenticated, errSessionEnded, ReasonSessionEnded)
		}
		return ctx, ReasonError(connect.CodeUnauthenticated, auth.ErrUnauthenticated, ReasonUnauthenticated)
	}
	ctx, err := i.gateCall(ctx, req.client, p, procedure)
	if err != nil {
		return ctx, err
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
		ctx, err := i.gateCall(ctx, req.client, p, procedure)
		if err != nil {
			return ctx, err
		}
		return context.WithValue(ctx, principalKey{}, p), nil
	case errors.Is(err, auth.ErrTokenRejected):
		return ctx, ReasonError(connect.CodeUnauthenticated, auth.ErrTokenRejected, ReasonTokenRejected)
	case errors.Is(err, auth.ErrTokenScope):
		return ctx, ReasonError(connect.CodePermissionDenied, auth.ErrTokenScope, ReasonTokenScope)
	case errors.Is(err, auth.ErrTokenLogs):
		return ctx, ReasonError(connect.CodePermissionDenied, auth.ErrTokenLogs, ReasonTokenLogs)
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
//
// The proxy has HTTP/3's early data turned off. If a config ever turned it on, as
// a hand edit could, a request in it says so with Early-Data, per RFC 8470, and is
// asked to wait for the handshake: it can be replayed, and the panel acts on what
// it is sent, which the marking below says has had a whole handshake.
func ViaIngress(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("Early-Data") != "" {
			w.WriteHeader(http.StatusTooEarly)
			return
		}
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
