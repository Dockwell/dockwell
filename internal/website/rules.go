// SPDX-License-Identifier: AGPL-3.0-only

package website

import (
	"cmp"
	"errors"
	"fmt"
	"net/url"
	"regexp"
	"slices"
	"strconv"
	"strings"

	resourcev1 "github.com/dockwell/dockwell/gen/dockwell/resource/v1alpha1"
	"github.com/dockwell/dockwell/internal/netguard"
)

const (
	// MaxBasicAuthUsers is the most Basic Auth users a website may keep.
	MaxBasicAuthUsers = 20
	// MaxCorsOrigins is the most CORS origins a website may allow.
	MaxCorsOrigins = 50
	// MaxPathRules is the most path redirect/rewrite rules a website may have.
	MaxPathRules = 50
	// MaxSubpathProxies is the most sub-path proxies a website may have.
	MaxSubpathProxies = 20
	// MaxHeaders is the most extra response headers a website may set or remove.
	MaxHeaders = 50

	// DefaultRateLimitRPM is the default per-visitor-address rate limit.
	DefaultRateLimitRPM = 60
	// MaxRateLimitRPM is the highest per-visitor-address rate limit.
	MaxRateLimitRPM = 60000
	// MaxRateLimitBurst is the largest burst above the steady rate.
	MaxRateLimitBurst = 1000

	// DefaultCorsMaxAge is the default preflight cache duration in seconds.
	DefaultCorsMaxAge = 3600
	// MaxCorsMaxAge is the longest preflight cache duration in seconds.
	MaxCorsMaxAge = 86400

	// DefaultStaticCacheMaxAge is 7 days in seconds.
	DefaultStaticCacheMaxAge = 604800
	// MinStaticCacheMaxAge is 1 minute in seconds.
	MinStaticCacheMaxAge = 60
	// MaxStaticCacheMaxAge is 1 year in seconds.
	MaxStaticCacheMaxAge = 31536000
)

// DefaultCorsMethods are allowed when CORS is enabled without explicit methods.
var DefaultCorsMethods = []string{"GET", "POST", "PUT", "PATCH", "DELETE", "OPTIONS"}

// DefaultCorsHeaders are allowed when CORS is enabled without explicit headers.
var DefaultCorsHeaders = []string{"Content-Type", "Authorization"}

// DefaultStaticCacheExtensions are cached when static caching is enabled without explicit extensions.
var DefaultStaticCacheExtensions = []string{
	"css", "js", "mjs", "png", "jpg", "jpeg", "gif", "webp", "avif", "svg", "ico", "woff", "woff2", "ttf", "otf",
}

var (
	basicUserRE    = regexp.MustCompile(`^[A-Za-z0-9._-]{1,64}$`)
	headerNameRE   = regexp.MustCompile(`^[A-Za-z0-9-]{1,128}$`)
	extNameRE      = regexp.MustCompile(`^[a-z0-9]{1,16}$`)
	serviceIDRE    = regexp.MustCompile(`^[A-Za-z0-9_-]{1,64}$`)
	rulePathRE     = regexp.MustCompile(`^(/[A-Za-z0-9._~%+-]*)*(/\*?)?$`)
	validMethods   = []string{"GET", "HEAD", "POST", "PUT", "PATCH", "DELETE", "OPTIONS"}
	blockedHeaders = []string{"host", "content-length", "transfer-encoding", "connection", "upgrade", "keep-alive", "te", "trailer"}
)

// BasicAuthPasswordSecret returns the Dockwell secret name holding a Basic Auth user's password.
func BasicAuthPasswordSecret(username string) string {
	return "WEBSITE_AUTH_" + sanitizeSecretPart(username)
}

// BasicAuthHashSecret returns the Dockwell secret name holding a Basic Auth user's bcrypt hash.
func BasicAuthHashSecret(username string) string {
	return BasicAuthPasswordSecret(username) + "_HASH"
}

// BasicAuthEnvVar returns the container environment variable name carrying the i-th user's bcrypt hash.
func BasicAuthEnvVar(idx int) string {
	return "DW_BASIC_AUTH_HASH_" + strconv.Itoa(idx)
}

// SubpathAlias returns the Docker network alias on SiteNetwork for the i-th sub-path proxy service target.
func SubpathAlias(idx int) string {
	return "subpath-" + strconv.Itoa(idx)
}

func sanitizeSecretPart(s string) string {
	var b strings.Builder
	for _, r := range strings.ToUpper(s) {
		if (r >= 'A' && r <= 'Z') || (r >= '0' && r <= '9') {
			b.WriteRune(r)
		} else {
			b.WriteByte('_')
		}
	}
	return b.String()
}

// HasWebServerRules reports whether r contains any rule executed by the site's Caddy container
// (anything other than ingress-enforced rate limiting).
func HasWebServerRules(r *resourcev1.WebsiteRules) bool {
	if r == nil {
		return false
	}
	if ba := r.GetBasicAuth(); ba != nil && len(ba.GetUsers()) > 0 {
		return true
	}
	if r.GetCors().GetEnabled() {
		return true
	}
	if len(r.GetPathRules()) > 0 || len(r.GetSubpathProxies()) > 0 || len(r.GetHeaders()) > 0 {
		return true
	}
	if r.GetStaticCache().GetEnabled() {
		return true
	}
	return false
}

// RouteRateLimit converts a website's rate limit rule into the ingress RouteRateLimit spec,
// or nil when disabled.
func RouteRateLimit(rl *resourcev1.WebsiteRateLimit) *resourcev1.RouteRateLimit {
	if rl == nil || !rl.GetEnabled() {
		return nil
	}
	return &resourcev1.RouteRateLimit{
		RequestsPerMinute: cmp.Or(rl.GetRequestsPerMinute(), uint32(DefaultRateLimitRPM)),
		Burst:             rl.GetBurst(),
	}
}

func checkRules(w *resourcev1.Website) error {
	r := w.GetRules()
	if r == nil {
		return nil
	}
	if KindOf(w) == ProxyToPort && HasWebServerRules(r) {
		return errors.New("a port on the server is served straight from the ingress without a web server of its own, so HTTP rules other than rate limiting apply to URL, folder and PHP websites")
	}
	if err := checkBasicAuth(r.GetBasicAuth()); err != nil {
		return err
	}
	if err := checkCors(r.GetCors()); err != nil {
		return err
	}
	if err := checkRateLimit(r.GetRateLimit()); err != nil {
		return err
	}
	if err := checkPathRules(r.GetPathRules()); err != nil {
		return err
	}
	if err := checkSubpathProxies(r.GetSubpathProxies()); err != nil {
		return err
	}
	if err := checkHeaders(r.GetHeaders()); err != nil {
		return err
	}
	return checkStaticCache(r.GetStaticCache())
}

func checkBasicAuth(ba *resourcev1.WebsiteBasicAuth) error {
	if ba == nil {
		return nil
	}
	p := strings.TrimSpace(ba.GetPath())
	if p == "/" {
		p = ""
	}
	if p != "" {
		p = strings.TrimRight(p, "/")
		if !strings.HasPrefix(p, "/") || strings.ContainsAny(p, " \t\r\n\"'`{}\\"+"*") || !pathRE.MatchString(p) {
			return fmt.Errorf("basic auth path %q needs to be a path prefix starting with /, such as /admin", ba.GetPath())
		}
		for _, seg := range strings.Split(p[1:], "/") {
			if seg == "." || seg == ".." {
				return fmt.Errorf("basic auth path %q has %s in it. Give the path it leads to", ba.GetPath(), seg)
			}
		}
	}
	ba.Path = p

	realm := strings.TrimSpace(ba.GetRealm())
	if len(realm) > 128 || !isSafeHeaderValue(realm) {
		return errors.New("basic auth realm may use up to 128 plain ASCII characters without quotes, backslashes or braces")
	}
	ba.Realm = realm

	if len(ba.GetUsers()) > MaxBasicAuthUsers {
		return fmt.Errorf("basic auth takes up to %d users, and this names %d", MaxBasicAuthUsers, len(ba.GetUsers()))
	}
	seenUser := map[string]bool{}
	seenSecret := map[string]bool{}
	for _, u := range ba.GetUsers() {
		name := strings.TrimSpace(u.GetUsername())
		if !basicUserRE.MatchString(name) {
			return fmt.Errorf("basic auth username %q may use 1 to 64 letters, digits, dots, dashes and underscores", u.GetUsername())
		}
		lower := strings.ToLower(name)
		if seenUser[lower] {
			return fmt.Errorf("basic auth user %q is listed twice. Name each user once", name)
		}
		seenUser[lower] = true
		secretName := BasicAuthPasswordSecret(name)
		if seenSecret[secretName] {
			return fmt.Errorf("basic auth user %q maps to the same secret name %s as another user. Choose a distinct username", name, secretName)
		}
		seenSecret[secretName] = true
		u.Username = name
		u.Secret = strings.TrimSpace(u.GetSecret())
		if u.Secret == "" {
			u.Secret = secretName
		}
	}
	return nil
}

func checkCors(c *resourcev1.WebsiteCors) error {
	if c == nil || !c.GetEnabled() {
		return nil
	}
	if len(c.GetAllowedOrigins()) == 0 {
		return errors.New("CORS needs at least one allowed origin, such as https://app.example.com, or * for any origin")
	}
	if len(c.GetAllowedOrigins()) > MaxCorsOrigins {
		return fmt.Errorf("CORS takes up to %d allowed origins", MaxCorsOrigins)
	}
	seen := map[string]bool{}
	var origins []string
	for _, raw := range c.GetAllowedOrigins() {
		o := strings.TrimSpace(raw)
		if o == "" {
			continue
		}
		if o == "*" {
			if len(c.GetAllowedOrigins()) > 1 {
				return errors.New("CORS origin * allows every origin, so it can't be combined with other origins")
			}
			if c.GetAllowCredentials() {
				return errors.New("CORS can't allow credentials with the wildcard origin *. List the specific origins instead")
			}
			origins = []string{"*"}
			break
		}
		o = strings.TrimRight(o, "/")
		if strings.ContainsAny(o, " \t\r\n\"'`{}\\") {
			return fmt.Errorf("CORS origin %q has a character an origin can't hold", raw)
		}
		u, err := url.Parse(o)
		if err != nil || (u.Scheme != "http" && u.Scheme != "https") || u.Host == "" || u.User != nil || u.Path != "" || u.RawQuery != "" || u.Fragment != "" {
			return fmt.Errorf("CORS origin %q needs scheme and host alone, such as https://app.example.com", raw)
		}
		norm := strings.ToLower(u.Scheme) + "://" + strings.ToLower(u.Host)
		if !seen[norm] {
			seen[norm] = true
			origins = append(origins, norm)
		}
	}
	if len(origins) == 0 {
		return errors.New("CORS needs at least one allowed origin, such as https://app.example.com, or * for any origin")
	}
	c.AllowedOrigins = origins

	if len(c.GetAllowedMethods()) > 0 {
		seenM := map[string]bool{}
		var methods []string
		for _, m := range c.GetAllowedMethods() {
			m = strings.ToUpper(strings.TrimSpace(m))
			if m == "" || seenM[m] {
				continue
			}
			if !slices.Contains(validMethods, m) {
				return fmt.Errorf("CORS method %q isn't a supported HTTP method", m)
			}
			seenM[m] = true
			methods = append(methods, m)
		}
		c.AllowedMethods = methods
	}

	if len(c.GetAllowedHeaders()) > 0 {
		seenH := map[string]bool{}
		var hdrs []string
		for _, h := range c.GetAllowedHeaders() {
			h = strings.TrimSpace(h)
			if h == "" {
				continue
			}
			if !headerNameRE.MatchString(h) {
				return fmt.Errorf("CORS header %q isn't a valid HTTP header name", h)
			}
			key := strings.ToLower(h)
			if !seenH[key] {
				seenH[key] = true
				hdrs = append(hdrs, h)
			}
		}
		c.AllowedHeaders = hdrs
	}

	if c.GetMaxAgeSeconds() > MaxCorsMaxAge {
		return fmt.Errorf("CORS max age of %d s is more than %d s (24 hours)", c.GetMaxAgeSeconds(), MaxCorsMaxAge)
	}
	return nil
}

func checkRateLimit(rl *resourcev1.WebsiteRateLimit) error {
	if rl == nil || !rl.GetEnabled() {
		return nil
	}
	if rl.GetRequestsPerMinute() > MaxRateLimitRPM {
		return fmt.Errorf("rate limit of %d requests/min is more than %d", rl.GetRequestsPerMinute(), MaxRateLimitRPM)
	}
	if rl.GetBurst() > MaxRateLimitBurst {
		return fmt.Errorf("rate limit burst of %d is more than %d", rl.GetBurst(), MaxRateLimitBurst)
	}
	return nil
}

func checkPathRules(rules []*resourcev1.WebsitePathRule) error {
	if len(rules) > MaxPathRules {
		return fmt.Errorf("a website takes up to %d redirect and rewrite rules", MaxPathRules)
	}
	seen := map[string]bool{}
	for _, r := range rules {
		switch r.GetKind() {
		case resourcev1.WebsitePathRuleKind_WEBSITE_PATH_RULE_KIND_REDIRECT, resourcev1.WebsitePathRuleKind_WEBSITE_PATH_RULE_KIND_REWRITE:
		default:
			return errors.New("each path rule needs to say whether it redirects visitors or rewrites internally")
		}
		from := strings.TrimSpace(r.GetFrom())
		if !strings.HasPrefix(from, "/") || strings.ContainsAny(from, " \t\r\n\"'`{}\\") || !rulePathRE.MatchString(from) {
			return fmt.Errorf("rule source path %q needs to start with /, such as /old or /blog/*", r.GetFrom())
		}
		if strings.Contains(from, "..") || strings.Contains(from, "//") {
			return fmt.Errorf("rule source path %q has an invalid segment. Give the clean path", from)
		}
		if seen[from] {
			return fmt.Errorf("path rule for %s is listed twice. Keep one rule per source path", from)
		}
		seen[from] = true
		r.From = from

		to := strings.TrimSpace(r.GetTo())
		if to == "" || strings.ContainsAny(to, " \t\r\n\"'`{}\\") {
			return fmt.Errorf("rule target %q is empty or has a character a path or URL can't hold", r.GetTo())
		}
		if r.GetKind() == resourcev1.WebsitePathRuleKind_WEBSITE_PATH_RULE_KIND_REWRITE {
			if !strings.HasPrefix(to, "/") || strings.Contains(to, "..") || !rulePathRE.MatchString(to) {
				return fmt.Errorf("rewrite target %q needs to be a path on this website starting with /, such as /new or /v2/*", r.GetTo())
			}
			if strings.HasSuffix(from, "/*") != strings.HasSuffix(to, "/*") {
				return fmt.Errorf("prefix rewrite from %s to %s needs both paths to end with /*, or neither", from, to)
			}
			r.Permanent = false
		} else {
			if strings.HasPrefix(to, "/") {
				if strings.Contains(to, "..") || !rulePathRE.MatchString(to) {
					return fmt.Errorf("redirect target %q isn't a valid path", r.GetTo())
				}
			} else {
				u, err := url.Parse(strings.TrimSuffix(to, "/*"))
				if err != nil || (u.Scheme != "http" && u.Scheme != "https") || u.Host == "" || u.User != nil {
					return fmt.Errorf("redirect target %q needs to be a path starting with / or an http:// or https:// URL", r.GetTo())
				}
			}
			if strings.HasSuffix(to, "/*") && !strings.HasSuffix(from, "/*") {
				return fmt.Errorf("redirect target %s ends with /*, so its source %s needs to end with /* too", to, from)
			}
		}
		r.To = to
	}
	return nil
}

func checkSubpathProxies(proxies []*resourcev1.WebsiteSubpathProxy) error {
	if len(proxies) > MaxSubpathProxies {
		return fmt.Errorf("a website takes up to %d sub-path proxies", MaxSubpathProxies)
	}
	seen := map[string]bool{}
	for _, p := range proxies {
		rawPath := strings.TrimSpace(p.GetPath())
		rawPath = strings.TrimSuffix(rawPath, "/*")
		rawPath = strings.TrimRight(rawPath, "/")
		if rawPath == "" || !strings.HasPrefix(rawPath, "/") || !pathRE.MatchString(rawPath) {
			return fmt.Errorf("sub-path proxy %q needs a path prefix starting with /, such as /api", p.GetPath())
		}
		for _, seg := range strings.Split(rawPath[1:], "/") {
			if seg == "." || seg == ".." {
				return fmt.Errorf("sub-path proxy %q has %s in it. Give the path it leads to", p.GetPath(), seg)
			}
		}
		if seen[rawPath] {
			return fmt.Errorf("sub-path proxy for %s is listed twice. Keep one proxy per path", rawPath)
		}
		seen[rawPath] = true
		p.Path = rawPath

		rawURL := strings.TrimSpace(p.GetUrl())
		svcID := strings.TrimSpace(p.GetServiceId())
		switch {
		case rawURL != "" && svcID != "":
			return fmt.Errorf("sub-path proxy %s names both a URL and an app service. Give one of them", rawPath)
		case rawURL == "" && svcID == "":
			return fmt.Errorf("sub-path proxy %s needs a target URL or an app service", rawPath)
		case rawURL != "":
			if p.GetServicePort() != 0 {
				return fmt.Errorf("sub-path proxy %s targets a URL, so leave service_port unset", rawPath)
			}
			clean, err := checkURL(rawURL)
			if err != nil {
				return fmt.Errorf("sub-path proxy %s: %w", rawPath, err)
			}
			if u, uerr := url.Parse(clean); uerr == nil {
				if why := netguard.Forbidden(u.Hostname(), netguard.Options{}); why != "" {
					return fmt.Errorf("sub-path proxy %s: %s is %s, so a sub-path proxy can't send there", rawPath, u.Hostname(), why)
				}
			}
			p.Url = clean
		default:
			if !serviceIDRE.MatchString(svcID) {
				return fmt.Errorf("sub-path proxy %s names an invalid service ID %q", rawPath, svcID)
			}
			port := p.GetServicePort()
			if port == 0 || port > 65535 {
				return fmt.Errorf("sub-path proxy %s needs the app's container port, from 1 to 65535", rawPath)
			}
			if slices.Contains(dockerPorts, int(port)) {
				return fmt.Errorf("sub-path proxy %s can't send to Docker API port %d", rawPath, port)
			}
			p.ServiceId = svcID
		}
	}
	return nil
}

func checkHeaders(hdrs []*resourcev1.WebsiteHeader) error {
	if len(hdrs) > MaxHeaders {
		return fmt.Errorf("a website takes up to %d extra response headers", MaxHeaders)
	}
	for _, h := range hdrs {
		name := strings.TrimSpace(h.GetName())
		if !headerNameRE.MatchString(name) {
			return fmt.Errorf("header name %q isn't a valid HTTP header name", h.GetName())
		}
		if slices.Contains(blockedHeaders, strings.ToLower(name)) {
			return fmt.Errorf("header %s controls HTTP framing and can't be overridden here", name)
		}
		h.Name = name
		if h.GetRemove() {
			h.Value = ""
			continue
		}
		val := strings.TrimSpace(h.GetValue())
		if val == "" {
			return fmt.Errorf("header %s needs a value, or mark it to remove", name)
		}
		if len(val) > 1024 || !isSafeHeaderValue(val) {
			return fmt.Errorf("header %s value may use up to 1024 plain ASCII characters without quotes, backslashes or braces", name)
		}
		h.Value = val
	}
	return nil
}

func checkStaticCache(sc *resourcev1.WebsiteStaticCache) error {
	if sc == nil || !sc.GetEnabled() {
		return nil
	}
	if sc.GetMaxAgeSeconds() != 0 && (sc.GetMaxAgeSeconds() < MinStaticCacheMaxAge || sc.GetMaxAgeSeconds() > MaxStaticCacheMaxAge) {
		return fmt.Errorf("static cache max-age must be between %d s and %d s (1 year)", MinStaticCacheMaxAge, MaxStaticCacheMaxAge)
	}
	if len(sc.GetExtensions()) > 50 {
		return errors.New("static cache takes up to 50 file extensions")
	}
	seen := map[string]bool{}
	var exts []string
	for _, raw := range sc.GetExtensions() {
		e := strings.ToLower(strings.TrimPrefix(strings.TrimSpace(raw), "."))
		if e == "" || seen[e] {
			continue
		}
		if !extNameRE.MatchString(e) {
			return fmt.Errorf("static cache extension %q may use 1 to 16 lowercase letters and digits", raw)
		}
		seen[e] = true
		exts = append(exts, e)
	}
	sc.Extensions = exts
	return nil
}

func isSafeHeaderValue(s string) bool {
	for _, r := range s {
		if r < 0x20 || r > 0x7e || r == '"' || r == '\\' || r == '{' || r == '}' || r == '`' {
			return false
		}
	}
	return true
}

// basicAuthEnv returns the container environment variables mapping DW_BASIC_AUTH_HASH_<i>
// to the Dockwell secret reference for each Basic Auth user.
func basicAuthEnv(w *resourcev1.Website) map[string]string {
	users := w.GetRules().GetBasicAuth().GetUsers()
	if len(users) == 0 || w.GetPaused() {
		return nil
	}
	env := make(map[string]string, len(users))
	for i, u := range users {
		env[BasicAuthEnvVar(i)] = "${" + BasicAuthHashSecret(u.GetUsername()) + "}"
	}
	return env
}

// pausedConfig returns the Caddyfile served while a website is paused.
func pausedConfig(w *resourcev1.Website) string {
	var b strings.Builder
	b.WriteString(globals)
	fmt.Fprintf(&b, "\n:%d {\n", Port)
	b.WriteString("\tlog {\n\t\toutput stderr\n\t\tformat console\n\t}\n")
	b.WriteString("\theader {\n")
	b.WriteString("\t\tCache-Control \"no-store\"\n")
	b.WriteString("\t\tRetry-After \"300\"\n")
	if w.GetPauseMode() == resourcev1.WebsitePauseMode_WEBSITE_PAUSE_MODE_PLAIN_503 {
		b.WriteString("\t\tContent-Type \"text/plain; charset=utf-8\"\n")
		b.WriteString("\t}\n")
		b.WriteString("\trespond \"Service Unavailable\" 503\n")
	} else {
		b.WriteString("\t\tContent-Type \"text/html; charset=utf-8\"\n")
		b.WriteString("\t}\n")
		b.WriteString("\trespond \"" + maintenanceHTML + "\" 503\n")
	}
	b.WriteString("}\n")
	return b.String()
}

// maintenanceHTML is a calm, self-contained HTML maintenance page escaped for a Caddyfile double-quoted string.
const maintenanceHTML = `<!doctype html><html lang=\"en\"><head><meta charset=\"utf-8\"><meta name=\"viewport\" content=\"width=device-width,initial-scale=1\"><title>Maintenance</title><style>body{margin:0;min-height:100vh;display:grid;place-items:center;font:16px/1.5 system-ui,-apple-system,sans-serif;background:#f8fafc;color:#0f172a}main{max-width:28rem;padding:2rem;text-align:center}h1{font-size:1.25rem;margin:0 0 .5rem}p{margin:0;color:#475569}@media(prefers-color-scheme:dark){body{background:#0b1120;color:#f1f5f9}p{color:#94a3b8}}</style></head><body><main><h1>Temporarily unavailable</h1><p>This website is paused for maintenance. Please check back shortly.</p></main></body></html>`

// writeBasicAuthBlock writes the basic_auth directive with indent, using matcher.
func writeBasicAuthBlock(b *strings.Builder, ba *resourcev1.WebsiteBasicAuth, matcher, indent string) {
	switch {
	case ba.GetRealm() != "":
		fmt.Fprintf(b, "%sbasic_auth %s bcrypt \"%s\" {\n", indent, matcher, ba.GetRealm())
	case matcher != "*":
		fmt.Fprintf(b, "%sbasic_auth %s {\n", indent, matcher)
	default:
		fmt.Fprintf(b, "%sbasic_auth {\n", indent)
	}
	for i, u := range ba.GetUsers() {
		fmt.Fprintf(b, "%s\t%s {$%s}\n", indent, u.GetUsername(), BasicAuthEnvVar(i))
	}
	fmt.Fprintf(b, "%s}\n", indent)
}

// needsOrderedAuthRoute reports whether rules require an explicit outer route block
// so path cleaning and Basic Auth execute strictly before and after rewrites (L5).
func needsOrderedAuthRoute(r *resourcev1.WebsiteRules) bool {
	return r != nil && r.GetBasicAuth() != nil && len(r.GetBasicAuth().GetUsers()) > 0
}

// writeRules emits the Caddyfile directives for WebsiteRules inside :8080 { ... }
// (or inside an outer route { ... } block when needsOrderedAuthRoute is true).
func writeRules(b *strings.Builder, r *resourcev1.WebsiteRules, indent string) {
	if r == nil {
		return
	}
	ba := r.GetBasicAuth()
	hasAuth := ba != nil && len(ba.GetUsers()) > 0
	hasRewrite := false
	for _, rule := range r.GetPathRules() {
		if rule.GetKind() != resourcev1.WebsitePathRuleKind_WEBSITE_PATH_RULE_KIND_REDIRECT {
			hasRewrite = true
			break
		}
	}
	if hasAuth {
		// Collapse repeated slashes before Basic Auth and rewrite matching; Caddy's path
		// matcher resolves dot segments, percent-escapes and case on the cleaned path (L5).
		fmt.Fprintf(b, "%suri path_regexp /{2,} /\n", indent)
		authMatcher := "*"
		if ba.GetPath() != "" {
			fmt.Fprintf(b, "%s@basic_auth path %s %s/*\n", indent, ba.GetPath(), ba.GetPath())
			authMatcher = "@basic_auth"
		}
		writeBasicAuthBlock(b, ba, authMatcher, indent)
	}

	var corsPreflightMaxAge uint32
	var corsPreflightOriginPattern string
	hasCorsPreflight := false
	if c := r.GetCors(); c != nil && c.GetEnabled() && len(c.GetAllowedOrigins()) > 0 {
		methods := c.GetAllowedMethods()
		if len(methods) == 0 {
			methods = DefaultCorsMethods
		}
		hdrs := c.GetAllowedHeaders()
		if len(hdrs) == 0 {
			hdrs = DefaultCorsHeaders
		}
		corsPreflightMaxAge = cmp.Or(c.GetMaxAgeSeconds(), uint32(DefaultCorsMaxAge))
		hasCorsPreflight = true
		if len(c.GetAllowedOrigins()) == 1 && c.GetAllowedOrigins()[0] == "*" {
			fmt.Fprintf(b, "%sheader {\n", indent)
			fmt.Fprintf(b, "%s\tAccess-Control-Allow-Origin \"*\"\n", indent)
			fmt.Fprintf(b, "%s\tAccess-Control-Allow-Methods \"%s\"\n", indent, strings.Join(methods, ", "))
			fmt.Fprintf(b, "%s\tAccess-Control-Allow-Headers \"%s\"\n", indent, strings.Join(hdrs, ", "))
			fmt.Fprintf(b, "%s}\n", indent)
		} else {
			quoted := make([]string, len(c.GetAllowedOrigins()))
			for i, o := range c.GetAllowedOrigins() {
				quoted[i] = regexp.QuoteMeta(o)
			}
			corsPreflightOriginPattern = "^(" + strings.Join(quoted, "|") + ")$"
			fmt.Fprintf(b, "%s@cors_origin header_regexp Origin %s\n", indent, corsPreflightOriginPattern)
			fmt.Fprintf(b, "%sheader @cors_origin {\n", indent)
			fmt.Fprintf(b, "%s\tAccess-Control-Allow-Origin {http.request.header.Origin}\n", indent)
			fmt.Fprintf(b, "%s\tAccess-Control-Allow-Methods \"%s\"\n", indent, strings.Join(methods, ", "))
			fmt.Fprintf(b, "%s\tAccess-Control-Allow-Headers \"%s\"\n", indent, strings.Join(hdrs, ", "))
			if c.GetAllowCredentials() {
				fmt.Fprintf(b, "%s\tAccess-Control-Allow-Credentials \"true\"\n", indent)
			}
			fmt.Fprintf(b, "%s\t+Vary Origin\n", indent)
			fmt.Fprintf(b, "%s}\n", indent)
		}
	}

	if len(r.GetHeaders()) > 0 {
		fmt.Fprintf(b, "%sheader {\n", indent)
		for _, h := range r.GetHeaders() {
			if h.GetRemove() {
				fmt.Fprintf(b, "%s\t-%s\n", indent, h.GetName())
			} else {
				fmt.Fprintf(b, "%s\t%s \"%s\"\n", indent, h.GetName(), h.GetValue())
			}
		}
		fmt.Fprintf(b, "%s}\n", indent)
	}

	if sc := r.GetStaticCache(); sc != nil && sc.GetEnabled() {
		exts := sc.GetExtensions()
		if len(exts) == 0 {
			exts = DefaultStaticCacheExtensions
		}
		globs := make([]string, len(exts))
		for i, e := range exts {
			globs[i] = "*." + e
		}
		maxAge := cmp.Or(sc.GetMaxAgeSeconds(), uint32(DefaultStaticCacheMaxAge))
		cc := fmt.Sprintf("public, max-age=%d", maxAge)
		if sc.GetImmutable() {
			cc += ", immutable"
		}
		fmt.Fprintf(b, "%s@static_cache path %s\n", indent, strings.Join(globs, " "))
		fmt.Fprintf(b, "%sheader @static_cache Cache-Control \"%s\"\n", indent, cc)
	}

	// Emit exact redirects and all internal rewrites before the post-rewrite Basic Auth
	// check and before handle/handle_path blocks.
	var prefixRedirects []*resourcev1.WebsitePathRule
	rwIdx := 0
	for _, rule := range r.GetPathRules() {
		if rule.GetKind() == resourcev1.WebsitePathRuleKind_WEBSITE_PATH_RULE_KIND_REDIRECT {
			if strings.HasSuffix(rule.GetFrom(), "/*") && strings.HasSuffix(rule.GetTo(), "/*") {
				prefixRedirects = append(prefixRedirects, rule)
				continue
			}
			code := "302"
			if rule.GetPermanent() {
				code = "301"
			}
			fmt.Fprintf(b, "%sredir %s %s %s\n", indent, rule.GetFrom(), rule.GetTo(), code)
			continue
		}
		// Internal rewrite.
		if strings.HasSuffix(rule.GetFrom(), "/*") && strings.HasSuffix(rule.GetTo(), "/*") {
			fromPrefix := strings.TrimSuffix(rule.GetFrom(), "*")
			toPrefix := strings.TrimSuffix(rule.GetTo(), "*")
			fmt.Fprintf(b, "%s@rw_%d path %s\n", indent, rwIdx, rule.GetFrom())
			fmt.Fprintf(b, "%suri @rw_%d replace %s %s 1\n", indent, rwIdx, fromPrefix, toPrefix)
			rwIdx++
		} else {
			fmt.Fprintf(b, "%srewrite %s %s\n", indent, rule.GetFrom(), rule.GetTo())
		}
	}

	// Also enforce Basic Auth after rewrites so a rewrite into a protected prefix
	// (e.g. /open -> /admin/secret.html) cannot bypass Basic Auth (L5).
	if hasAuth && hasRewrite && ba.GetPath() != "" {
		writeBasicAuthBlock(b, ba, "@basic_auth", indent)
	}

	if hasCorsPreflight {
		if corsPreflightOriginPattern == "" {
			fmt.Fprintf(b, "%s@cors_preflight method OPTIONS\n", indent)
		} else {
			fmt.Fprintf(b, "%s@cors_preflight {\n", indent)
			fmt.Fprintf(b, "%s\tmethod OPTIONS\n", indent)
			fmt.Fprintf(b, "%s\theader_regexp Origin %s\n", indent, corsPreflightOriginPattern)
			fmt.Fprintf(b, "%s}\n", indent)
		}
		fmt.Fprintf(b, "%shandle @cors_preflight {\n", indent)
		fmt.Fprintf(b, "%s\theader Access-Control-Max-Age \"%d\"\n", indent, corsPreflightMaxAge)
		fmt.Fprintf(b, "%s\trespond \"\" 204\n", indent)
		fmt.Fprintf(b, "%s}\n", indent)
	}

	for _, rule := range prefixRedirects {
		code := "302"
		if rule.GetPermanent() {
			code = "301"
		}
		prefix := strings.TrimSuffix(rule.GetFrom(), "/*")
		destPrefix := strings.TrimSuffix(rule.GetTo(), "/*")
		fmt.Fprintf(b, "%shandle_path %s/* {\n", indent, prefix)
		fmt.Fprintf(b, "%s\tredir * %s{uri} %s\n", indent, destPrefix, code)
		fmt.Fprintf(b, "%s}\n", indent)
	}

	for i, sp := range r.GetSubpathProxies() {
		target := ""
		var upstreamPrefix string
		if sp.GetUrl() != "" {
			target = sp.GetUrl()
			if u, err := url.Parse(target); err == nil && u.Path != "" {
				upstreamPrefix = u.EscapedPath()
				target = u.Scheme + "://" + u.Host
			}
		} else {
			target = fmt.Sprintf("http://%s:%d", SubpathAlias(i), sp.GetServicePort())
		}
		fmt.Fprintf(b, "%s@subpath_%d path %s %s/*\n", indent, i, sp.GetPath(), sp.GetPath())
		fmt.Fprintf(b, "%shandle @subpath_%d {\n", indent, i)
		if sp.GetStripPrefix() {
			fmt.Fprintf(b, "%s\turi strip_prefix %s\n", indent, sp.GetPath())
		}
		if upstreamPrefix != "" {
			fmt.Fprintf(b, "%s\trewrite * %s{uri}\n", indent, upstreamPrefix)
		}
		fmt.Fprintf(b, "%s\treverse_proxy %s", indent, target)
		if sp.GetSendTargetHost() {
			fmt.Fprintf(b, " {\n%s\t\theader_up Host {upstream_hostport}\n%s\t}\n", indent, indent)
		} else {
			b.WriteString("\n")
		}
		fmt.Fprintf(b, "%s}\n", indent)
	}
}
