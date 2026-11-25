// SPDX-License-Identifier: AGPL-3.0-only

package website

import (
	"bytes"
	"context"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"

	"golang.org/x/crypto/bcrypt"

	resourcev1 "github.com/dockwell/dockwell/gen/dockwell/resource/v1alpha1"
	"github.com/dockwell/dockwell/internal/testport"
)

func TestPauseAndResumeResources(t *testing.T) {
	// ProxyToPort gets a web service when paused, and drops it on resume.
	portSite := &resourcev1.Website{
		Serves:    &resourcev1.Website_Proxy{Proxy: &resourcev1.WebsiteProxy{Port: 3000}},
		Paused:    true,
		PauseMode: resourcev1.WebsitePauseMode_WEBSITE_PAUSE_MODE_MAINTENANCE,
	}
	if err := Check(portSite); err != nil {
		t.Fatal(err)
	}
	res := Resources(portSite, "app.example.com", Front{})
	if res.Route.GetHostPort() != 0 || res.Route.GetService() != WebService || res.Route.GetPort() != Port {
		t.Errorf("paused port site route = %v", res.Route)
	}
	if res.Services[WebService] == nil || res.Networks[SiteNetwork] == nil {
		t.Fatalf("paused port site should run web service on site network")
	}
	if cfg := Config(portSite); !strings.Contains(cfg, "Temporarily unavailable") || !strings.Contains(cfg, "503") {
		t.Errorf("paused maintenance config:\n%s", cfg)
	}

	// Plain 503 mode.
	portSite.PauseMode = resourcev1.WebsitePauseMode_WEBSITE_PAUSE_MODE_PLAIN_503
	if cfg := Config(portSite); !strings.Contains(cfg, `respond "Service Unavailable" 503`) || strings.Contains(cfg, "<html") {
		t.Errorf("plain 503 config:\n%s", cfg)
	}

	// PHP site keeps its folder mount on web when paused and stops php service.
	phpSite := &resourcev1.Website{
		Serves: &resourcev1.Website_Php{Php: &resourcev1.WebsitePhp{Root: "/var/www/shop", Version: "8.4"}},
		Paused: true,
	}
	if err := Check(phpSite); err != nil {
		t.Fatal(err)
	}
	phpRes := Resources(phpSite, "shop.example.com", Front{})
	if phpRes.Services[PHPService] != nil {
		t.Errorf("paused PHP site should not run PHP service")
	}
	if m := phpRes.Services[WebService].GetApp().GetMounts(); len(m) != 1 || m[0].GetHostPath() != "/var/www/shop" {
		t.Errorf("paused PHP site should keep folder mount, got %v", m)
	}
}

func TestRulesValidation(t *testing.T) {
	// Port proxy allows rate limit, refuses web-server rules and custom Caddy.
	okPort := &resourcev1.Website{
		Serves: &resourcev1.Website_Proxy{Proxy: &resourcev1.WebsiteProxy{Port: 3000}},
		Rules: &resourcev1.WebsiteRules{
			RateLimit: &resourcev1.WebsiteRateLimit{Enabled: true, RequestsPerMinute: 120, Burst: 20},
		},
	}
	if err := Check(okPort); err != nil {
		t.Fatalf("port site with rate limit failed: %v", err)
	}
	rl := Resources(okPort, "app.example.com", Front{}).Route.GetRateLimit()
	if rl.GetRequestsPerMinute() != 120 || rl.GetBurst() != 20 {
		t.Errorf("route rate limit = %v", rl)
	}

	badPort := &resourcev1.Website{
		Serves: &resourcev1.Website_Proxy{Proxy: &resourcev1.WebsiteProxy{Port: 3000}},
		Rules: &resourcev1.WebsiteRules{
			Headers: []*resourcev1.WebsiteHeader{{Name: "X-Test", Value: "1"}},
		},
	}
	if err := Check(badPort); err == nil || !strings.Contains(err.Error(), "without a web server") {
		t.Errorf("port site with headers = %v, want refusal", err)
	}

	// CORS wildcard with credentials refused.
	badCors := &resourcev1.Website{
		Serves: &resourcev1.Website_Folder{Folder: &resourcev1.WebsiteFolder{Root: "/var/www/site"}},
		Rules: &resourcev1.WebsiteRules{
			Cors: &resourcev1.WebsiteCors{Enabled: true, AllowedOrigins: []string{"*"}, AllowCredentials: true},
		},
	}
	if err := Check(badCors); err == nil || !strings.Contains(err.Error(), "credentials") {
		t.Errorf("wildcard CORS with credentials = %v, want refusal", err)
	}

	// Sub-path proxy SSRF refused.
	badSubpath := &resourcev1.Website{
		Serves: &resourcev1.Website_Folder{Folder: &resourcev1.WebsiteFolder{Root: "/var/www/site"}},
		Rules: &resourcev1.WebsiteRules{
			SubpathProxies: []*resourcev1.WebsiteSubpathProxy{{Path: "/meta", Url: "http://169.254.169.254/latest"}},
		},
	}
	if err := Check(badSubpath); err == nil || !strings.Contains(err.Error(), "metadata") {
		t.Errorf("sub-path proxy to metadata = %v, want refusal", err)
	}

	// Header placeholder injection refused.
	badHeader := &resourcev1.Website{
		Serves: &resourcev1.Website_Folder{Folder: &resourcev1.WebsiteFolder{Root: "/var/www/site"}},
		Rules: &resourcev1.WebsiteRules{
			Headers: []*resourcev1.WebsiteHeader{{Name: "X-Leak", Value: "{$DW_BASIC_AUTH_HASH_0}"}},
		},
	}
	if err := Check(badHeader); err == nil {
		t.Errorf("header with placeholder should be refused")
	}
}

func TestPHPTuningAndExtensions(t *testing.T) {
	w := php(&resourcev1.WebsitePhp{
		Root:               "/var/www/shop",
		Version:            "8.4",
		UploadLimitMb:      32,
		PostMaxSizeMb:      128,
		MemoryLimitMb:      512,
		MaxExecutionTimeS:  120,
		FpmMaxChildren:     20,
		DisabledExtensions: []string{"imagick", "opcache"},
	})
	if err := Check(w); err != nil {
		t.Fatal(err)
	}
	got := Resources(w, "shop.example.com", Front{})
	fpm := got.Services[PHPService].GetApp()
	if !slices.Equal(fpm.GetEntrypoint(), []string{"/bin/sh", "-c", phpBootScript, "php"}) {
		t.Errorf("entrypoint = %v", fpm.GetEntrypoint())
	}
	cmd := strings.Join(fpm.GetCommand(), " ")
	for _, want := range []string{
		"pm.max_children = 20",
		"imagick opcache",
		"upload_max_filesize=32M",
		"post_max_size=128M",
		"memory_limit=512M",
		"max_execution_time=120",
		"opcache.enable=0",
	} {
		if !strings.Contains(cmd, want) {
			t.Errorf("PHP command missing %q:\n%s", want, cmd)
		}
	}
	if tmp := fpm.GetTmpfs(); len(tmp) != 1 || tmp[0].GetSize() != "144M" {
		t.Errorf("PHP tmpfs = %v, want 144M (max(32,128)+16)", tmp)
	}
	if cfg := Config(w); !strings.Contains(cfg, "max_size 128MiB") {
		t.Errorf("Caddy max_size should match larger of upload and post:\n%s", cfg)
	}

	// Out-of-bounds checks.
	for _, tc := range []struct {
		p    *resourcev1.WebsitePhp
		says string
	}{
		{&resourcev1.WebsitePhp{Root: "/var/www/shop", MemoryLimitMb: 16}, "memory_limit"},
		{&resourcev1.WebsitePhp{Root: "/var/www/shop", MaxExecutionTimeS: 2}, "max_execution_time"},
		{&resourcev1.WebsitePhp{Root: "/var/www/shop", PostMaxSizeMb: 5000}, "post_max_size"},
		{&resourcev1.WebsitePhp{Root: "/var/www/shop", FpmMaxChildren: 500}, "pm.max_children"},
		{&resourcev1.WebsitePhp{Root: "/var/www/shop", DisabledExtensions: []string{"pdo_fake"}}, "optional extensions"},
	} {
		if err := Check(php(tc.p)); err == nil || !strings.Contains(err.Error(), tc.says) {
			t.Errorf("Check(%v) = %v, want %q", tc.p, err, tc.says)
		}
	}
}

func TestCustomCaddyGuards(t *testing.T) {
	for _, tc := range []struct {
		raw  string
		root string
		says string
	}{
		{"}\nother.example.com {\n\trespond hi\n", FolderTarget, "unexpected '}'"},
		{"import /etc/passwd", FolderTarget, "import isn't allowed"},
		{"tls internal", FolderTarget, "tls isn't allowed"},
		{"bind 0.0.0.0", FolderTarget, "bind isn't allowed"},
		{"admin :2019", FolderTarget, "admin isn't allowed"},
		{"root * /etc", FolderTarget, "must stay inside /srv"},
		{"root * /srv/../etc", FolderTarget, "must stay inside /srv"},
		{"@other host evil.example.com", FolderTarget, "host matchers"},
		{"header X-Secret {$DW_BASIC_AUTH_HASH_0}", FolderTarget, "environment"},
		{"respond \"ok\" {\n", FolderTarget, "unclosed '{'"},
	} {
		_, err := CheckCustomCaddy(tc.raw, tc.root)
		if err == nil || !strings.Contains(err.Error(), tc.says) {
			t.Errorf("CheckCustomCaddy(%q) = %v, want %q", tc.raw, err, tc.says)
		}
	}
	// Valid custom Caddy passes static check.
	clean, err := CheckCustomCaddy("header X-Custom \"hello\"\n@health path /healthz\nrespond @health \"ok\" 200", FolderTarget)
	if err != nil || !strings.Contains(clean, "header X-Custom") {
		t.Fatalf("valid custom Caddy failed: %q %v", clean, err)
	}
}

// TestRulesAndPauseOnRealCaddy verifies every rule, pause mode, and custom Caddy
// validation against the real pinned Caddy binary.
func TestRulesAndPauseOnRealCaddy(t *testing.T) {
	bin := needCaddy(t)
	root := t.TempDir()
	for name, body := range map[string]string{
		"index.html":        "home",
		"style.css":         "body{}",
		"v2/page.html":      "v2 page",
		"new-about.html":    "new about",
		"admin/secret.html": "secret",
	} {
		if err := os.MkdirAll(filepath.Dir(filepath.Join(root, name)), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(root, name), []byte(body), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	apiHit := make(chan string, 8)
	apiSrv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		apiHit <- r.URL.Path
		_, _ = w.Write([]byte("api:" + r.URL.Path))
	}))
	t.Cleanup(apiSrv.Close)

	hash, err := bcrypt.GenerateFromPassword([]byte("s3cret-pass"), bcrypt.MinCost)
	if err != nil {
		t.Fatal(err)
	}

	w := &resourcev1.Website{
		Serves: &resourcev1.Website_Folder{Folder: &resourcev1.WebsiteFolder{Root: "/var/www/blog"}},
		Rules: &resourcev1.WebsiteRules{
			BasicAuth: &resourcev1.WebsiteBasicAuth{
				Path:  "/admin",
				Realm: "Admin Area",
				Users: []*resourcev1.WebsiteBasicAuthUser{{Username: "alice", Secret: BasicAuthPasswordSecret("alice")}},
			},
			Cors: &resourcev1.WebsiteCors{
				Enabled:          true,
				AllowedOrigins:   []string{"https://app.example.com", "https://admin.example.com"},
				AllowCredentials: true,
				MaxAgeSeconds:    600,
			},
			PathRules: []*resourcev1.WebsitePathRule{
				{From: "/old", To: "/new", Kind: resourcev1.WebsitePathRuleKind_WEBSITE_PATH_RULE_KIND_REDIRECT, Permanent: true},
				{From: "/blog/*", To: "/posts/*", Kind: resourcev1.WebsitePathRuleKind_WEBSITE_PATH_RULE_KIND_REDIRECT, Permanent: false},
				{From: "/about", To: "/new-about.html", Kind: resourcev1.WebsitePathRuleKind_WEBSITE_PATH_RULE_KIND_REWRITE},
				{From: "/v1/*", To: "/v2/*", Kind: resourcev1.WebsitePathRuleKind_WEBSITE_PATH_RULE_KIND_REWRITE},
			},
			SubpathProxies: []*resourcev1.WebsiteSubpathProxy{
				// Use 127.0.0.1 replaced below after Check (which blocks loopback on real specs).
			},
			Headers: []*resourcev1.WebsiteHeader{
				{Name: "X-Frame-Options", Value: "DENY"},
				{Name: "Server", Remove: true},
			},
			StaticCache: &resourcev1.WebsiteStaticCache{
				Enabled:       true,
				MaxAgeSeconds: 86400,
				Immutable:     true,
			},
		},
		CustomCaddy: "@health path /healthz\nrespond @health \"healthy\" 200",
	}
	if err := Check(w); err != nil {
		t.Fatal(err)
	}
	// Verify Caddy's own adapter accepts ForValidation(cfg) and ValidateAdaptedCaddyJSON accepts it
	// with a public upstream (before substituting the local httptest server).
	wForAdapt := w
	wForAdapt.Rules.SubpathProxies = []*resourcev1.WebsiteSubpathProxy{
		{Path: "/api", Url: "https://api.example.com", StripPrefix: true},
	}
	adaptCfg := strings.Replace(Config(wForAdapt), "root * "+FolderTarget, "root * "+root, 1)
	adaptCmd := exec.Command(bin, "adapt", "--config", "-", "--adapter", "caddyfile")
	adaptCmd.Stdin = strings.NewReader(ForValidation(adaptCfg))
	adapted, err := adaptCmd.CombinedOutput()
	if err != nil {
		t.Fatalf("caddy adapt failed: %v\n%s\nConfig:\n%s", err, adapted, adaptCfg)
	}
	if err := ValidateAdaptedCaddyJSON(adapted, root); err != nil {
		t.Fatalf("ValidateAdaptedCaddyJSON rejected valid config: %v", err)
	}

	// Verify ValidateAdaptedCaddyJSON rejects a custom reverse_proxy to metadata IP.
	badCfg := strings.Replace(adaptCfg, `respond @health "healthy" 200`, `reverse_proxy @health http://169.254.169.254`, 1)
	badAdapt := exec.Command(bin, "adapt", "--config", "-", "--adapter", "caddyfile")
	badAdapt.Stdin = strings.NewReader(ForValidation(badCfg))
	badJSON, err := badAdapt.CombinedOutput()
	if err != nil {
		t.Fatalf("caddy adapt badCfg failed: %v\n%s", err, badJSON)
	}
	if err := ValidateAdaptedCaddyJSON(badJSON, root); err == nil || !strings.Contains(err.Error(), "metadata") {
		t.Fatalf("ValidateAdaptedCaddyJSON accepted metadata upstream: %v", err)
	}

	// Attach subpath proxy directly to test httptest server on loopback for live traffic tests.
	w.Rules.SubpathProxies = []*resourcev1.WebsiteSubpathProxy{
		{Path: "/api", Url: apiSrv.URL, StripPrefix: true},
	}
	cfg := strings.Replace(Config(w), "root * "+FolderTarget, "root * "+root, 1)

	// Run real Caddy with the Basic Auth hash env var.
	addr := runsWithEnv(t, bin, cfg, []string{BasicAuthEnvVar(0) + "=" + string(hash)})
	client := &http.Client{
		Timeout: 5 * time.Second,
		CheckRedirect: func(*http.Request, []*http.Request) error {
			return http.ErrUseLastResponse
		},
	}

	doReq := func(method, path string, headers map[string]string, user, pass string) *http.Response {
		t.Helper()
		ctx, cancel := context.WithTimeout(t.Context(), 5*time.Second)
		defer cancel()
		req, err := http.NewRequestWithContext(ctx, method, "http://"+addr+path, http.NoBody)
		if err != nil {
			t.Fatal(err)
		}
		for k, v := range headers {
			req.Header.Set(k, v)
		}
		if user != "" {
			req.SetBasicAuth(user, pass)
		}
		res, err := client.Do(req)
		if err != nil {
			t.Fatal(err)
		}
		return res
	}

	// 1. Extra headers & Server removal on /.
	res := doReq("GET", "/", nil, "", "")
	res.Body.Close()
	if res.StatusCode != http.StatusOK || res.Header.Get("X-Frame-Options") != "DENY" || res.Header.Get("Server") != "" {
		t.Errorf("GET / status=%d X-Frame-Options=%q Server=%q", res.StatusCode, res.Header.Get("X-Frame-Options"), res.Header.Get("Server"))
	}

	// 2. Static cache on /style.css, NOT on /.
	res = doReq("GET", "/style.css", nil, "", "")
	res.Body.Close()
	if cc := res.Header.Get("Cache-Control"); cc != "public, max-age=86400, immutable" {
		t.Errorf("GET /style.css Cache-Control = %q", cc)
	}

	// 3. Basic Auth on /admin/secret.html.
	res = doReq("GET", "/admin/secret.html", nil, "", "")
	res.Body.Close()
	if res.StatusCode != http.StatusUnauthorized || !strings.Contains(res.Header.Get("Www-Authenticate"), "Admin Area") {
		t.Errorf("unauthenticated /admin = %d %q", res.StatusCode, res.Header.Get("Www-Authenticate"))
	}
	res = doReq("GET", "/admin/secret.html", nil, "alice", "s3cret-pass")
	var buf bytes.Buffer
	_, _ = buf.ReadFrom(res.Body)
	res.Body.Close()
	if res.StatusCode != http.StatusOK || buf.String() != "secret" {
		t.Errorf("authenticated /admin = %d %q", res.StatusCode, buf.String())
	}

	// 4. CORS allowed origin, second allowed origin, disallowed origin, and preflight.
	res = doReq("GET", "/", map[string]string{"Origin": "https://admin.example.com"}, "", "")
	res.Body.Close()
	if res.Header.Get("Access-Control-Allow-Origin") != "https://admin.example.com" || res.Header.Get("Access-Control-Allow-Credentials") != "true" || !strings.Contains(res.Header.Get("Vary"), "Origin") {
		t.Errorf("CORS allowed headers = %v", res.Header)
	}
	res = doReq("GET", "/", map[string]string{"Origin": "https://evil.example.com"}, "", "")
	res.Body.Close()
	if res.Header.Get("Access-Control-Allow-Origin") != "" {
		t.Errorf("CORS should reject evil origin, got %q", res.Header.Get("Access-Control-Allow-Origin"))
	}
	res = doReq("OPTIONS", "/", map[string]string{"Origin": "https://app.example.com"}, "", "")
	res.Body.Close()
	if res.StatusCode != http.StatusNoContent || res.Header.Get("Access-Control-Max-Age") != "600" {
		t.Errorf("CORS preflight = %d max-age=%q", res.StatusCode, res.Header.Get("Access-Control-Max-Age"))
	}

	// 5. Redirects (exact 301 + prefix 302) and rewrites (exact + prefix).
	res = doReq("GET", "/old", nil, "", "")
	res.Body.Close()
	if res.StatusCode != http.StatusMovedPermanently || res.Header.Get("Location") != "/new" {
		t.Errorf("GET /old = %d Location=%q", res.StatusCode, res.Header.Get("Location"))
	}
	res = doReq("GET", "/blog/hello?a=1", nil, "", "")
	res.Body.Close()
	if res.StatusCode != http.StatusFound || res.Header.Get("Location") != "/posts/hello?a=1" {
		t.Errorf("GET /blog/hello?a=1 = %d Location=%q", res.StatusCode, res.Header.Get("Location"))
	}
	res = doReq("GET", "/about", nil, "", "")
	buf.Reset()
	_, _ = buf.ReadFrom(res.Body)
	res.Body.Close()
	if res.StatusCode != http.StatusOK || buf.String() != "new about" {
		t.Errorf("rewrite /about = %d %q", res.StatusCode, buf.String())
	}
	res = doReq("GET", "/v1/page.html", nil, "", "")
	buf.Reset()
	_, _ = buf.ReadFrom(res.Body)
	res.Body.Close()
	if res.StatusCode != http.StatusOK || buf.String() != "v2 page" {
		t.Errorf("prefix rewrite /v1/page.html = %d %q", res.StatusCode, buf.String())
	}

	// 6. Sub-path proxy with strip_prefix.
	res = doReq("GET", "/api/users", nil, "", "")
	buf.Reset()
	_, _ = buf.ReadFrom(res.Body)
	res.Body.Close()
	if res.StatusCode != http.StatusOK || buf.String() != "api:/users" {
		t.Errorf("sub-path proxy /api/users = %d %q", res.StatusCode, buf.String())
	}

	// 7. Custom Caddy directive /healthz.
	res = doReq("GET", "/healthz", nil, "", "")
	buf.Reset()
	_, _ = buf.ReadFrom(res.Body)
	res.Body.Close()
	if res.StatusCode != http.StatusOK || buf.String() != "healthy" {
		t.Errorf("custom /healthz = %d %q", res.StatusCode, buf.String())
	}

	// 8. Paused maintenance page & plain 503 on real Caddy.
	pausedMaint := runs(t, bin, Config(&resourcev1.Website{
		Serves:    &resourcev1.Website_Folder{Folder: &resourcev1.WebsiteFolder{Root: "/var/www/blog"}},
		Paused:    true,
		PauseMode: resourcev1.WebsitePauseMode_WEBSITE_PAUSE_MODE_MAINTENANCE,
	}))
	status, body := get(t, pausedMaint, "/anything")
	if status != 503 || !strings.Contains(body, "paused for maintenance") {
		t.Errorf("paused maintenance = %d %q", status, body)
	}
	paused503 := runs(t, bin, Config(&resourcev1.Website{
		Serves:    &resourcev1.Website_Folder{Folder: &resourcev1.WebsiteFolder{Root: "/var/www/blog"}},
		Paused:    true,
		PauseMode: resourcev1.WebsitePauseMode_WEBSITE_PAUSE_MODE_PLAIN_503,
	}))
	status, body = get(t, paused503, "/anything")
	if status != 503 || body != "Service Unavailable" {
		t.Errorf("paused plain 503 = %d %q", status, body)
	}
}

func runsWithEnv(t *testing.T, bin, config string, extraEnv []string) string {
	t.Helper()
	addr := testport.Addr(t, "127.0.0.1")
	_, port, _ := net.SplitHostPort(addr)
	dir := t.TempDir()
	config = strings.Replace(config, "\n:8080 {", "\n:"+port+" {", 1)
	moved := strings.ReplaceAll(script, "/tmp/Caddyfile", filepath.Join(dir, "Caddyfile"))
	moved = strings.Replace(moved, "exec caddy ", "exec "+bin+" ", 1)
	cmd := exec.Command("/bin/sh", "-c", moved, "web", config)
	cmd.Env = append(os.Environ(), "XDG_DATA_HOME="+filepath.Join(dir, "data"), "XDG_CONFIG_HOME="+filepath.Join(dir, "config"))
	cmd.Env = append(cmd.Env, extraEnv...)
	var out strings.Builder
	cmd.Stdout, cmd.Stderr = &out, &out
	if err := cmd.Start(); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		_ = cmd.Process.Kill()
		_ = cmd.Wait()
		if t.Failed() {
			t.Logf("Caddy said:\n%s", out.String())
		}
	})
	deadline := time.Now().Add(10 * time.Second)
	for {
		c, err := net.DialTimeout("tcp", addr, 200*time.Millisecond)
		if err == nil {
			c.Close()
			return addr
		}
		if time.Now().After(deadline) {
			t.Fatalf("Caddy didn't listen on %s: %v", addr, err)
		}
		time.Sleep(50 * time.Millisecond)
	}
}
