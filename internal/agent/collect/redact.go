// SPDX-License-Identifier: AGPL-3.0-only

package collect

import (
	"path"
	"slices"
	"strings"

	"github.com/dockwell/dockwell/internal/lazyre"
)

// Hidden stands in for a secret a command line held, as the process list and
// process detail show it.
const Hidden = "…"

// secretFlags are explicit flags whose value is always a credential, such as
// cloudflared's connector token ("cloudflared tunnel run --token <token>",
// ADR 0104) or Redis's --requirepass.
var secretFlags = map[string]bool{
	"token":                true,
	"credentials-contents": true,
	"credentials":          true,
	"credential":           true,
	"password":             true,
	"passwd":               true,
	"pass":                 true,
	"pw":                   true,
	"pwd":                  true,
	"passphrase":           true,
	"secret":               true,
	"apikey":               true,
	"api-key":              true,
	"api-secret":           true,
	"access-key":           true,
	"secret-key":           true,
	"secret-key-base":      true,
	"secret_key_base":      true,
	"private-key":          true,
	"signing-key":          true,
	"encryption-key":       true,
	"master-key":           true,
	"client-secret":        true,
	"auth":                 true,
	"authkey":              true,
	"auth-key":             true,
	"auth_key":             true,
	"authorization":        true,
	"bearer":               true,
	"cookie":               true,
	"pat":                  true,
	"webhook":              true,
	"webhook-url":          true,
	"webhook_url":          true,
	"requirepass":          true,
	"masterauth":           true,
	"dsn":                  true,
	"connection-string":    true,
	"database-url":         true,
}

// secretFlagSuffixes catch compound credential flags such as --smtp-password,
// --admin-pw, --db-pass, --access-token, --webhook-secret, --secret_key_base,
// and dotted long flags such as --spring.datasource.password.
var secretFlagSuffixes = []string{
	"-token", "_token", ".token",
	"-password", "_password", ".password",
	"-passwd", "_passwd", ".passwd",
	"-pass", "_pass", ".pass",
	"-pw", "_pw", ".pw",
	"-pwd", "_pwd", ".pwd",
	"-passphrase", "_passphrase", ".passphrase",
	"-secret", "_secret", ".secret",
	"-apikey", "_apikey", ".apikey",
	"-api-key", "_api_key", ".api_key", ".api-key",
	"-secret-key", "_secret_key", ".secret_key", ".secret-key",
	"-key-base", "_key_base", ".key_base",
	"-access-key", "_access_key", ".access_key", ".access-key",
	"-private-key", "_private_key", ".private_key", ".private-key",
	"-signing-key", "_signing_key", ".signing_key", ".signing-key",
	"-encryption-key", "_encryption_key", ".encryption_key", ".encryption-key",
	"-master-key", "_master_key", ".master_key", ".master-key",
	"-credential", "_credential", ".credential",
	"-credentials", "_credentials", ".credentials",
	"-credentials-contents", "_credentials_contents",
	"-auth-token", "_auth_token", ".auth_token", ".auth-token",
	"-auth-key", "_auth_key", ".auth_key", ".auth-key",
	"-authkey", "_authkey", ".authkey",
	"-pat", "_pat", ".pat",
	"-webhook", "_webhook", ".webhook",
	"-dsn", "_dsn", ".dsn",
}

// nonSecretWords identify trailing word segments of flags/keys that name a
// location, size, user, or setting rather than the secret value itself
// (e.g. --token-file, PASSWORD_FILE, --db.password.path, AUTH_MODE).
var nonSecretWords = map[string]bool{
	"file":      true,
	"path":      true,
	"dir":       true,
	"len":       true,
	"length":    true,
	"ttl":       true,
	"timeout":   true,
	"expiry":    true,
	"expire":    true,
	"count":     true,
	"mode":      true,
	"type":      true,
	"name":      true,
	"id":        true,
	"port":      true,
	"host":      true,
	"env":       true,
	"enabled":   true,
	"enable":    true,
	"disabled":  true,
	"disable":   true,
	"header":    true,
	"alg":       true,
	"algorithm": true,
	"method":    true,
	"scheme":    true,
	"issuer":    true,
	"user":      true,
	"username":  true,
}

// secretEnvWords are word segments of KEY=value arguments, flags, headers, or
// form keys that hold credentials.
var secretEnvWords = map[string]bool{
	"password":      true,
	"passwd":        true,
	"pass":          true,
	"pw":            true,
	"pwd":           true,
	"passphrase":    true,
	"secret":        true,
	"token":         true,
	"apikey":        true,
	"authkey":       true,
	"auth":          true,
	"pat":           true,
	"webhook":       true,
	"signing":       true,
	"credential":    true,
	"credentials":   true,
	"authorization": true,
	"bearer":        true,
	"cookie":        true,
	"dsn":           true,
	"requirepass":   true,
	"masterauth":    true,
}

// secretKeyQualifiers pair with "key" (e.g. API_KEY, STRIPE_KEY, MASTER_KEY)
// so mundane keys like SORT_KEY or CACHE_KEY aren't over-redacted.
var secretKeyQualifiers = map[string]bool{
	"api":        true,
	"secret":     true,
	"access":     true,
	"private":    true,
	"signing":    true,
	"encryption": true,
	"encrypt":    true,
	"master":     true,
	"auth":       true,
	"token":      true,
	"client":     true,
	"webhook":    true,
	"ssh":        true,
	"jwt":        true,
	"license":    true,
	"hmac":       true,
	"session":    true,
	"cookie":     true,
	"app":        true,
	"stripe":     true,
	"github":     true,
	"gitlab":     true,
	"slack":      true,
	"openai":     true,
	"anthropic":  true,
	"aws":        true,
	"gcp":        true,
	"azure":      true,
	"cloudflare": true,
	"resend":     true,
	"sendgrid":   true,
	"twilio":     true,
	"tailscale":  true,
	"oauth":      true,
	"smtp":       true,
	"s3":         true,
	"db":         true,
	"database":   true,
	"redis":      true,
	"vault":      true,
	"kms":        true,
	"service":    true,
	"admin":      true,
}

// secretURLKeys are KEY=value names whose values are connection URLs that may
// contain credentials or query-string secrets.
var secretURLKeys = map[string]bool{
	"database_url":      true,
	"db_url":            true,
	"redis_url":         true,
	"mongo_url":         true,
	"mongodb_url":       true,
	"postgres_url":      true,
	"postgresql_url":    true,
	"mysql_url":         true,
	"amqp_url":          true,
	"broker_url":        true,
	"connection_string": true,
}

// wrapperProgs precede the real executable in a command line (e.g. sudo -u root
// mysql -pX, timeout 5 redis-cli -a X, if true; then mysql -pX; fi).
var wrapperProgs = map[string]bool{
	"sudo": true, "doas": true, "env": true, "exec": true,
	"nohup": true, "nice": true, "ionice": true, "chrt": true,
	"stdbuf": true, "setsid": true, "timeout": true, "time": true,
	"strace": true, "ltrace": true, "chroot": true, "su": true,
	"runuser": true, "taskset": true, "watch": true, "xargs": true,
	// Shell compound-command keywords that precede a command.
	"if": true, "then": true, "do": true, "else": true, "elif": true,
	"while": true, "until": true, "!": true, "{": true,
}

// wrapperArgFlags are short/long flags on wrappers that take a separate next
// token as their own argument (so e.g. "sudo -u mysql ssh -p 22" doesn't treat
// the username "mysql" as the program).
var wrapperArgFlags = map[string]bool{
	"-u": true, "-g": true, "-n": true, "-k": true, "-s": true,
	"-C": true, "-D": true, "-h": true, "-r": true, "-t": true,
	"-T": true, "-U": true, "-e": true, "-w": true, "-i": true,
	"-o": true, "-p": true, "-c": true,
	"--user": true, "--group": true, "--env": true, "--workdir": true,
	"--signal": true, "--kill-after": true, "--adjustment": true,
	"--unset": true, "--chdir": true, "--name": true, "--entrypoint": true,
}

// mysqlLikeProgs take -p<password> or -p <password>.
var mysqlLikeProgs = map[string]bool{
	"mysql": true, "mysqladmin": true, "mariadb": true,
	"mariadb-admin": true, "mysqldump": true, "mariadb-dump": true,
	"sshpass": true, "mongosh": true, "mongo": true,
	"docker-login": true,
}

// ldapProgs take -w<password> or -w <password>.
var ldapProgs = map[string]bool{
	"ldapsearch": true, "ldapmodify": true, "ldapadd": true,
	"ldapdelete": true, "ldappasswd": true, "ldapwhoami": true,
}

// containerCLIProgs can either run "login -p <pass>" or wrap an inner command
// via "exec [opts] <container> <cmd>" / "run [opts] <image> <cmd>".
var containerCLIProgs = map[string]bool{
	"docker": true, "podman": true, "nerdctl": true, "kubectl": true,
}

func isKnownProg(base string) bool {
	if isShellProg(base) || mysqlLikeProgs[base] || ldapProgs[base] || containerCLIProgs[base] {
		return true
	}
	switch base {
	case "redis-cli", "curl", "openssl", "smbclient", "htpasswd", "vault", "cloudflared":
		return true
	default:
		return false
	}
}

// installInArg finds the token "cloudflared service install" takes as an
// argument of its own, inside a shell's -c string.
var installInArg = lazyre.Regexp(`(cloudflared\S*\s+service\s+install\s+)(?:"[^"]*"|'[^']*'|\S+)`)

// urlPattern finds URLs inside an argument so embedded credentials (user:pass@,
// http(s)://token@), webhook URL paths, and secret query parameters can be hidden.
var urlPattern = lazyre.Regexp(`[A-Za-z][A-Za-z0-9+.-]*://[^\s"'` + "`" + `<>]+`)

// bearerBasicPattern hides a standalone Bearer or Basic credential token anywhere
// in an argument (such as `Bearer S3cret` or `Basic dXNlcjpwYXNz`).
var bearerBasicPattern = lazyre.Regexp(`(?i)(\b(?:bearer|basic)\s+)([^\s"';,]+)`)

// jsonSecretKeyPattern matches JSON object/array keys whose names suggest secrets.
var jsonSecretKeyPattern = lazyre.Regexp(`(?i)"[^"]*(?:pass|secret|token|key|auth|credential|pw)[^"]*"\s*:`)

// inlineKVPattern finds KEY=VALUE assignments embedded inside a larger word or
// string (such as --env=POSTGRES_PASSWORD=x, --set=a.password=x,
// user=x&password=y, username=u,password=y, jdbc:...;password=y,
// connect(password='y')).
var inlineKVPattern = lazyre.Regexp(`([A-Za-z_][A-Za-z0-9_.-]*)=("[^"]*"?|'[^']*'?|[^#&;,\s"'` + "`" + `)\]}]+)`)

// Redact hides secret flag values, secret KEY=value arguments, per-program short
// password flags, credential headers, form bodies, URL credentials/webhooks/query
// tokens, Java -D secret properties, secret JSON objects/arrays, and cloudflared
// tokens before a command line leaves the server.
func Redact(args []string) []string { return redact(args) }

type nextMode int

const (
	nextNone nextMode = iota
	nextHideAll
	nextHideAfterColon
	nextHideAfterPercent
	nextOpenSSLPass
	nextHeader
	nextFormBody
)

// progTracker follows the active executable across wrappers (sudo -u, timeout,
// nice -n, env, docker exec, if/then/do) and subcommands (docker login,
// vault login, htpasswd -b).
type progTracker struct {
	prog           string
	inWrapper      bool
	skipNext       bool
	htpasswdBatch  bool
	htpasswdNoFile bool
	htpasswdSkip   bool
	htpasswdPos    int
	vaultLogin     bool
}

func (pt *progTracker) reset() {
	*pt = progTracker{}
}

// observe advances program tracking for tok and reports true if tok itself is a
// positional secret (such as `vault login <token>` or `htpasswd -b f u <pass>`).
func (pt *progTracker) observe(tok string) bool {
	if tok == "" {
		return false
	}
	if pt.skipNext {
		pt.skipNext = false
		return false
	}
	if pt.prog == "" || pt.inWrapper {
		if strings.HasPrefix(tok, "-") {
			if pt.inWrapper && !strings.Contains(tok, "=") && wrapperArgFlags[tok] {
				pt.skipNext = true
			}
			return false
		}
		if k, _, ok := strings.Cut(tok, "="); ok && !strings.ContainsAny(k, "/:") {
			return false
		}
		base := strings.ToLower(path.Base(strings.Trim(tok, `"'`)))
		if wrapperProgs[base] {
			pt.inWrapper = true
			return false
		}
		if !pt.inWrapper {
			pt.prog = base
			return false
		}
		if isKnownProg(base) {
			pt.prog = base
			pt.inWrapper = false
		}
		return false
	}

	// Subcommand handling for container CLIs (docker/podman/nerdctl/kubectl).
	if containerCLIProgs[pt.prog] {
		if strings.HasPrefix(tok, "-") {
			return false
		}
		switch strings.ToLower(tok) {
		case "login":
			pt.prog = "docker-login"
		case "exec", "run":
			pt.prog = ""
			pt.inWrapper = true
		}
		return false
	}

	// sshpass wraps another command (e.g. sshpass -p S3cret ssh -p 22 host).
	if pt.prog == "sshpass" && !strings.HasPrefix(tok, "-") {
		base := strings.ToLower(path.Base(strings.Trim(tok, `"'`)))
		if base == "ssh" || base == "scp" || base == "sftp" || base == "rsync" || isKnownProg(base) {
			pt.prog = base
		}
		return false
	}

	// vault login <token>
	if pt.prog == "vault" {
		if !pt.vaultLogin {
			if tok == "login" {
				pt.vaultLogin = true
			}
			return false
		}
		if strings.HasPrefix(tok, "-") || strings.Contains(tok, "=") {
			return false
		}
		pt.vaultLogin = false
		return true
	}

	// htpasswd -b <file> <user> <password> (or -nb <user> <password>)
	if pt.prog == "htpasswd" {
		if pt.htpasswdSkip {
			pt.htpasswdSkip = false
			return false
		}
		if strings.HasPrefix(tok, "-") && !strings.HasPrefix(tok, "--") {
			if tok == "-C" {
				pt.htpasswdSkip = true
				return false
			}
			if strings.ContainsRune(tok[1:], 'b') {
				pt.htpasswdBatch = true
			}
			if strings.ContainsRune(tok[1:], 'n') {
				pt.htpasswdNoFile = true
			}
			return false
		}
		if strings.HasPrefix(tok, "--") {
			return false
		}
		pt.htpasswdPos++
		if pt.htpasswdBatch {
			want := 3
			if pt.htpasswdNoFile {
				want = 2
			}
			if pt.htpasswdPos == want {
				return true
			}
		}
	}
	return false
}

func redact(args []string) []string {
	out := make([]string, len(args))
	mode := nextNone
	var tracker progTracker
	for i, arg := range args {
		if mode != nextNone {
			out[i] = applyNextMode(arg, mode)
			mode = nextNone
			continue
		}
		if i > 0 && isShellProg(tracker.prog) && isShellCFlag(args[i-1]) {
			out[i] = redactShell(arg)
			continue
		}
		if tracker.observe(arg) {
			out[i] = preserveQuotes(arg, func(_ string) string { return Hidden })
			continue
		}
		redacted, next := redactToken(arg, tracker.prog, false)
		out[i] = redacted
		mode = next
	}
	// "cloudflared service install <token>" takes the token with no flag.
	for i := 2; i+1 < len(out); i++ {
		if out[i] == "install" && out[i-1] == "service" && slices.ContainsFunc(out[:i-1], isCloudflared) {
			out[i+1] = Hidden
		}
	}
	return out
}

func applyNextMode(arg string, mode nextMode) string {
	switch mode {
	case nextHideAll:
		return preserveQuotes(arg, func(_ string) string { return Hidden })
	case nextHideAfterColon:
		return preserveQuotes(arg, hideAfterColon)
	case nextHideAfterPercent:
		return preserveQuotes(arg, hideAfterPercent)
	case nextOpenSSLPass:
		return preserveQuotes(arg, redactOpenSSLPass)
	case nextHeader:
		return preserveQuotes(arg, redactHeaderValue)
	case nextFormBody:
		return preserveQuotes(arg, redactFormBody)
	default:
		return arg
	}
}

func hideAfterColon(s string) string {
	if user, pass, ok := strings.Cut(s, ":"); ok && pass != "" {
		return user + ":" + Hidden
	}
	return s
}

func hideAfterPercent(s string) string {
	if user, pass, ok := strings.Cut(s, "%"); ok && pass != "" {
		return user + "%" + Hidden
	}
	return s
}

func redactOpenSSLPass(s string) string {
	lower := strings.ToLower(s)
	if strings.HasPrefix(lower, "pass:") {
		return s[:5] + Hidden
	}
	if strings.HasPrefix(lower, "file:") || strings.HasPrefix(lower, "env:") || strings.HasPrefix(lower, "fd:") || lower == "stdin" {
		return s
	}
	return Hidden
}

// redactHeaderValue hides the value of any HTTP header whose name looks like a
// credential (Authorization, Proxy-Authorization, X-Api-Key, X-Auth-Token,
// PRIVATE-TOKEN, Cookie, apikey, etc.), keeping Bearer/Basic scheme labels.
func redactHeaderValue(s string) string {
	if red, ok := redactSecretHeaderLine(s); ok {
		return red
	}
	return redactInlineValues(s)
}

func redactSecretHeaderLine(s string) (string, bool) {
	colon := strings.IndexByte(s, ':')
	if colon <= 0 {
		return "", false
	}
	name := strings.TrimSpace(s[:colon])
	rest := s[colon+1:]
	if name == "" || strings.HasPrefix(rest, "//") {
		return "", false
	}
	if !isSecretHeaderName(name) {
		return "", false
	}
	valTrim := strings.TrimLeft(rest, " \t")
	if valTrim == "" {
		return "", false
	}
	sep := rest[:len(rest)-len(valTrim)]
	if strings.EqualFold(name, "authorization") || strings.EqualFold(name, "proxy-authorization") {
		lower := strings.ToLower(valTrim)
		for _, scheme := range []string{"bearer ", "basic "} {
			if strings.HasPrefix(lower, scheme) {
				after := strings.TrimLeft(valTrim[len(scheme):], " \t")
				if after == "" {
					return "", false
				}
				prefix := valTrim[:len(valTrim)-len(after)]
				return s[:colon+1] + sep + prefix + Hidden, true
			}
		}
	}
	return s[:colon+1] + sep + Hidden, true
}

func isSecretHeaderName(name string) bool {
	if strings.EqualFold(name, "cookie") || strings.EqualFold(name, "set-cookie") {
		return true
	}
	return isSecretKey(name)
}

// redactFormBody redacts secret keys in a curl -d / --data body, handling JSON
// objects/arrays as well as &-separated form key=value pairs (even when values
// contain spaces).
func redactFormBody(s string) string {
	if looksLikeSecretJSON(s) {
		return Hidden
	}
	if strings.Contains(s, "=") {
		parts := strings.Split(s, "&")
		changed := false
		for i, p := range parts {
			if k, v, ok := strings.Cut(p, "="); ok && v != "" && isSecretKey(k) {
				parts[i] = k + "=" + Hidden
				changed = true
			}
		}
		if changed {
			return strings.Join(parts, "&")
		}
	}
	return redactInlineValues(s)
}

// preserveQuotes runs fn on the inner text of a quoted shell token, keeping the
// outer quotes intact when present.
func preserveQuotes(tok string, fn func(string) string) string {
	if len(tok) >= 2 {
		q := tok[0]
		if (q == '"' || q == '\'') && tok[len(tok)-1] == q {
			return string(q) + fn(tok[1:len(tok)-1]) + string(q)
		}
	}
	return fn(tok)
}

// redactToken redacts one argument (or shell word) given the active program
// basename, returning the redacted token and what to do with the next token.
func redactToken(tok, prog string, inShell bool) (string, nextMode) {
	// Per-program short flags (mysql/mongosh/docker-login -p, redis-cli -a,
	// curl -u/-H/-d, openssl -k/-pass*, ldapsearch -w, smbclient -U).
	if red, next, handled := redactProgramFlag(tok, prog); handled {
		return red, next
	}
	// Generic -H/--header and -d/--data* values on HTTP/API clients.
	if red, next, handled := redactCommonFlag(tok); handled {
		return red, next
	}
	// Long / explicit secret flag taking the next argument.
	if isSecretFlag(tok) {
		return tok, nextHideAll
	}
	// Java system properties: -Dspring.datasource.password=S3cret
	if strings.HasPrefix(tok, "-D") {
		if key, val, ok := strings.Cut(tok[2:], "="); ok && val != "" && isSecretKey(key) {
			return "-D" + key + "=" + Hidden, nextNone
		}
	}
	// Flag=value (including --flag="a b", --env=KEY=VAL, --set=a.password=v,
	// --config='{"password":"x"}', --header="X-Auth-Token: x", --data="password=x").
	if flag, val, ok := strings.Cut(tok, "="); ok && strings.HasPrefix(flag, "-") {
		if isSecretFlag(flag) {
			return flag + "=" + Hidden, nextNone
		}
		if prog == "curl" && strings.EqualFold(flag, "--user") {
			return flag + "=" + preserveQuotes(val, hideAfterColon), nextNone
		}
		if prog == "smbclient" && strings.EqualFold(flag, "--user") {
			return flag + "=" + preserveQuotes(val, hideAfterPercent), nextNone
		}
		if prog == "openssl" && isOpenSSLPassFlag(flag) {
			return flag + "=" + preserveQuotes(val, redactOpenSSLPass), nextNone
		}
		if strings.EqualFold(flag, "--header") {
			return flag + "=" + preserveQuotes(val, redactHeaderValue), nextNone
		}
		if isDataFlag(flag) {
			return flag + "=" + preserveQuotes(val, redactFormBody), nextNone
		}
		if val != "" {
			if looksLikeSecretJSON(stripQuotes(val)) {
				return flag + "=" + Hidden, nextNone
			}
			redVal := redactInlineValues(val)
			if redVal != val {
				return flag + "=" + redVal, nextNone
			}
		}
	}
	// Flag:value for long secret flags (--password:S3cret).
	if flag, val, ok := strings.Cut(tok, ":"); ok && strings.HasPrefix(flag, "-") && isSecretFlag(flag) && val != "" {
		return flag + ":" + Hidden, nextNone
	}
	// KEY=value assignment (e.g. PGPASSWORD=x, MYSQL_PWD=x, GITHUB_PAT=x, TOKEN="a b").
	if key, val, ok := strings.Cut(tok, "="); ok && !strings.HasPrefix(key, "-") && isSecretKey(key) {
		if val != "" {
			return key + "=" + Hidden, nextNone
		}
		return tok, nextNone
	}
	// Secret JSON object or array argument: '{"password":"S3cret"}' / '[{"token":"S3cret"}]'
	if looksLikeSecretJSON(stripQuotes(tok)) {
		return preserveQuotes(tok, func(_ string) string { return Hidden }), nextNone
	}
	// YAML / heredoc key line inside a shell script: "password: S3cret"
	if inShell && strings.HasSuffix(tok, ":") && len(tok) > 1 && !strings.HasPrefix(tok, "-") && !strings.Contains(tok, "://") {
		key := strings.TrimSuffix(tok, ":")
		if isSecretHeaderName(key) {
			return tok, nextHideAll
		}
	}
	// Standalone header argument (with quotes in sh -c or as one argv element).
	if red, ok := redactSecretHeaderLine(stripQuotes(tok)); ok {
		return preserveQuotes(tok, func(_ string) string { return red }), nextNone
	}
	// Inline URLs, auth headers, key=value pairs, or nested shell snippets.
	if !inShell && strings.ContainsAny(tok, " \t\n") {
		return redactShell(tok), nextNone
	}
	return redactInlineValues(tok), nextNone
}

func redactCommonFlag(tok string) (string, nextMode, bool) {
	if strings.EqualFold(tok, "--header") {
		return tok, nextHeader, true
	}
	if isDataFlag(tok) {
		return tok, nextFormBody, true
	}
	return "", nextNone, false
}

func isDataFlag(tok string) bool {
	switch strings.ToLower(tok) {
	case "--data", "--data-raw", "--data-urlencode", "--data-binary", "--data-ascii":
		return true
	default:
		return false
	}
}

func redactProgramFlag(tok, prog string) (string, nextMode, bool) {
	switch {
	case mysqlLikeProgs[prog]:
		if tok == "-p" {
			return tok, nextHideAll, true
		}
		if strings.HasPrefix(tok, "-p") && !strings.HasPrefix(tok, "--") && len(tok) > 2 {
			// Distinguish -pS3cret from long single-dash flags like -port.
			if tok != "-port" {
				return "-p" + Hidden, nextNone, true
			}
		}
	case prog == "redis-cli":
		if tok == "-a" {
			return tok, nextHideAll, true
		}
		if strings.HasPrefix(tok, "-a") && !strings.HasPrefix(tok, "--") && len(tok) > 2 {
			return "-a" + Hidden, nextNone, true
		}
	case prog == "curl":
		if tok == "-u" || tok == "--user" {
			return tok, nextHideAfterColon, true
		}
		if strings.HasPrefix(tok, "-u") && !strings.HasPrefix(tok, "--") && len(tok) > 2 {
			return "-u" + hideAfterColon(tok[2:]), nextNone, true
		}
		if tok == "-H" {
			return tok, nextHeader, true
		}
		if strings.HasPrefix(tok, "-H") && !strings.HasPrefix(tok, "--") && len(tok) > 2 {
			return "-H" + preserveQuotes(tok[2:], redactHeaderValue), nextNone, true
		}
		if tok == "-d" {
			return tok, nextFormBody, true
		}
		if strings.HasPrefix(tok, "-d") && !strings.HasPrefix(tok, "--") && len(tok) > 2 {
			return "-d" + preserveQuotes(tok[2:], redactFormBody), nextNone, true
		}
	case prog == "openssl":
		if tok == "-k" {
			return tok, nextHideAll, true
		}
		if strings.HasPrefix(tok, "-k") && !strings.HasPrefix(tok, "--") && len(tok) > 2 {
			return "-k" + Hidden, nextNone, true
		}
		if isOpenSSLPassFlag(tok) {
			return tok, nextOpenSSLPass, true
		}
	case ldapProgs[prog]:
		if tok == "-w" {
			return tok, nextHideAll, true
		}
		if strings.HasPrefix(tok, "-w") && !strings.HasPrefix(tok, "--") && len(tok) > 2 {
			return "-w" + Hidden, nextNone, true
		}
	case prog == "smbclient":
		if tok == "-U" || tok == "--user" {
			return tok, nextHideAfterPercent, true
		}
		if strings.HasPrefix(tok, "-U") && !strings.HasPrefix(tok, "--") && len(tok) > 2 {
			return "-U" + preserveQuotes(tok[2:], hideAfterPercent), nextNone, true
		}
	}
	return "", nextNone, false
}

func isOpenSSLPassFlag(tok string) bool {
	switch strings.ToLower(tok) {
	case "-pass", "-passin", "-passout", "--pass", "--passin", "--passout":
		return true
	default:
		return false
	}
}

func stripQuotes(s string) string {
	if len(s) >= 2 && ((s[0] == '"' && s[len(s)-1] == '"') || (s[0] == '\'' && s[len(s)-1] == '\'')) {
		return s[1 : len(s)-1]
	}
	return s
}

func looksLikeSecretJSON(s string) bool {
	trimmed := strings.TrimSpace(s)
	if !strings.HasPrefix(trimmed, "{") && !strings.HasPrefix(trimmed, "[") {
		return false
	}
	return jsonSecretKeyPattern().MatchString(trimmed)
}

type shellPiece struct {
	text  string
	isSep bool // shell command separator or whitespace
	reset bool // resets active command tracker and flag mode
}

// splitShellPieces splits a shell -c string into words, whitespace, and shell
// command/subshell separators (;, &&, ||, |, (, $(, ), `, newline), keeping
// quoted segments (including unclosed quotes from truncated cmdlines) glued to
// their surrounding word.
func splitShellPieces(s string) []shellPiece {
	var out []shellPiece
	i := 0
	n := len(s)
	for i < n {
		c := s[i]
		if c == ' ' || c == '\t' || c == '\r' || c == '\n' {
			start := i
			hasNL := false
			for i < n && (s[i] == ' ' || s[i] == '\t' || s[i] == '\r' || s[i] == '\n') {
				if s[i] == '\n' {
					hasNL = true
				}
				i++
			}
			out = append(out, shellPiece{text: s[start:i], isSep: true, reset: hasNL})
			continue
		}
		if i+1 < n && ((c == '&' && s[i+1] == '&') || (c == '|' && s[i+1] == '|') || (c == '$' && s[i+1] == '(')) {
			out = append(out, shellPiece{text: s[i : i+2], isSep: true, reset: true})
			i += 2
			continue
		}
		if c == ';' || c == '|' || c == '(' || c == ')' || c == '`' {
			out = append(out, shellPiece{text: s[i : i+1], isSep: true, reset: true})
			i++
			continue
		}
		if c == '&' && (i+1 == n || s[i+1] == ' ' || s[i+1] == '\t' || s[i+1] == '\n') {
			out = append(out, shellPiece{text: "&", isSep: true, reset: true})
			i++
			continue
		}
		// Read one shell word, preserving quoted strings within it.
		start := i
		for i < n {
			ch := s[i]
			if ch == '"' || ch == '\'' {
				q := ch
				i++
				for i < n {
					if q == '"' && s[i] == '\\' && i+1 < n {
						i += 2
						continue
					}
					if s[i] == q {
						i++
						break
					}
					i++
				}
				continue
			}
			if ch == ' ' || ch == '\t' || ch == '\r' || ch == '\n' ||
				ch == ';' || ch == '|' || ch == '(' || ch == ')' || ch == '`' {
				break
			}
			if i+1 < n && ((ch == '&' && s[i+1] == '&') || (ch == '$' && s[i+1] == '(')) {
				break
			}
			i++
		}
		out = append(out, shellPiece{text: s[start:i]})
	}
	return out
}

// redactShell redacts secrets inside a shell -c string (or multi-word argument),
// tracking the active program across command separators, subshells, and shell
// keywords, and keeping quoted values with spaces intact as single tokens.
func redactShell(arg string) string {
	if looksLikeSecretJSON(arg) {
		return Hidden
	}
	// If the entire argument is a credential header or Bearer/Basic value
	// (e.g. curl -H "Authorization: Bearer S3cret" or "X-Auth-Token: S3cret"),
	// redact it directly before shell tokenization splits the scheme from its value.
	if !strings.ContainsAny(arg, "\n") && !strings.Contains(arg, " -") {
		if red, ok := redactSecretHeaderLine(arg); ok {
			return red
		}
		if bearerBasicPattern().MatchString(arg) {
			return redactInlineValues(arg)
		}
	}
	arg = installInArg().ReplaceAllString(arg, "${1}"+Hidden)
	pieces := splitShellPieces(arg)
	if len(pieces) == 0 {
		return redactInlineValues(arg)
	}
	var b strings.Builder
	b.Grow(len(arg))
	mode := nextNone
	var tracker progTracker
	for _, p := range pieces {
		if p.isSep {
			b.WriteString(p.text)
			if p.reset {
				tracker.reset()
				mode = nextNone
			}
			continue
		}
		tok := p.text
		if mode != nextNone {
			b.WriteString(applyNextMode(tok, mode))
			mode = nextNone
			continue
		}
		if tracker.observe(tok) {
			b.WriteString(preserveQuotes(tok, func(_ string) string { return Hidden }))
			continue
		}
		red, next := redactToken(tok, tracker.prog, true)
		b.WriteString(red)
		mode = next
	}
	return b.String()
}

// redactInlineValues redacts URLs (embedded credentials, webhook paths, and
// secret query parameters), credential headers, Bearer/Basic tokens, and any
// embedded secret KEY=VALUE assignments inside s.
func redactInlineValues(s string) string {
	if strings.Contains(s, "://") {
		s = urlPattern().ReplaceAllStringFunc(s, redactOneURL)
	}
	if strings.Contains(s, ":") {
		if red, ok := redactSecretHeaderLine(stripQuotes(s)); ok {
			return preserveQuotes(s, func(_ string) string { return red })
		}
	}
	if strings.ContainsAny(s, " \t") {
		s = bearerBasicPattern().ReplaceAllString(s, "${1}"+Hidden)
	}
	if strings.Contains(s, "=") {
		s = redactInlineKVs(s)
	}
	return s
}

func redactInlineKVs(s string) string {
	return inlineKVPattern().ReplaceAllStringFunc(s, func(match string) string {
		key, val, ok := strings.Cut(match, "=")
		if !ok || val == "" || !isSecretKey(key) {
			return match
		}
		if val == Hidden || val == `"`+Hidden+`"` || val == `'`+Hidden+`'` {
			return match
		}
		if len(val) >= 2 && ((val[0] == '"' && val[len(val)-1] == '"') || (val[0] == '\'' && val[len(val)-1] == '\'')) {
			q := val[:1]
			return key + "=" + q + Hidden + q
		}
		if val[0] == '"' || val[0] == '\'' {
			return key + "=" + Hidden
		}
		return key + "=" + Hidden
	})
}

func redactOneURL(raw string) string {
	schemeIdx := strings.Index(raw, "://")
	if schemeIdx < 0 {
		return raw
	}
	scheme := strings.ToLower(raw[:schemeIdx])
	rest := raw[schemeIdx+3:]

	// Split fragment (#...) and query (?...) from authority+path.
	frag := ""
	if fi := strings.IndexByte(rest, '#'); fi >= 0 {
		frag = rest[fi:]
		rest = rest[:fi]
	}
	query := ""
	if qi := strings.IndexByte(rest, '?'); qi >= 0 {
		query = rest[qi+1:]
		rest = rest[:qi]
	}

	// Redact authority credentials (user:pass@host or http(s)://token@host).
	slash := strings.IndexByte(rest, '/')
	authPart := rest
	pathPart := ""
	if slash >= 0 {
		authPart = rest[:slash]
		pathPart = rest[slash:]
	}
	host := authPart
	if at := strings.LastIndexByte(authPart, '@'); at >= 0 {
		userInfo := authPart[:at]
		host = authPart[at+1:]
		if user, pass, ok := strings.Cut(userInfo, ":"); ok && pass != "" {
			authPart = user + ":" + Hidden + "@" + host
		} else if userInfo != "" && (scheme == "http" || scheme == "https" || looksLikeBareToken(userInfo)) {
			authPart = Hidden + "@" + host
		}
	}

	// Redact path-embedded webhook/bot tokens (Slack, Discord, Telegram).
	pathPart = redactWebhookPath(host, pathPart)

	// Redact secret query-string parameters (?token=x&api_key=y&sig=z).
	if query != "" {
		parts := strings.Split(query, "&")
		changed := false
		for i, p := range parts {
			if k, v, ok := strings.Cut(p, "="); ok && v != "" && isSecretQueryKey(k) {
				parts[i] = k + "=" + Hidden
				changed = true
			}
		}
		if changed {
			query = strings.Join(parts, "&")
		}
	}

	var b strings.Builder
	b.Grow(len(raw))
	b.WriteString(raw[:schemeIdx+3])
	b.WriteString(authPart)
	b.WriteString(pathPart)
	if query != "" {
		b.WriteByte('?')
		b.WriteString(query)
	}
	b.WriteString(frag)
	return b.String()
}

func redactWebhookPath(host, pathPart string) string {
	if pathPart == "" {
		return pathPart
	}
	hostLower := strings.ToLower(host)
	if h, _, ok := strings.Cut(hostLower, ":"); ok {
		hostLower = h
	}
	switch {
	case (hostLower == "hooks.slack.com" || hostLower == "hooks.slack-gov.com") && strings.HasPrefix(pathPart, "/services/"):
		return "/services/" + Hidden
	case (hostLower == "discord.com" || hostLower == "discordapp.com" || strings.HasSuffix(hostLower, ".discord.com")) &&
		strings.HasPrefix(pathPart, "/api/webhooks/"):
		return "/api/webhooks/" + Hidden
	case hostLower == "api.telegram.org":
		for _, prefix := range []string{"/bot", "/file/bot"} {
			if rest, ok := strings.CutPrefix(pathPart, prefix); ok && rest != "" {
				if slash := strings.IndexByte(rest, '/'); slash >= 0 {
					return prefix + Hidden + rest[slash:]
				}
				return prefix + Hidden
			}
		}
	}
	return pathPart
}

func isSecretQueryKey(k string) bool {
	if isSecretKey(k) {
		return true
	}
	return strings.EqualFold(k, "sig") || strings.EqualFold(k, "signature")
}

func looksLikeBareToken(s string) bool {
	for _, p := range []string{"ghp_", "gho_", "ghu_", "ghs_", "ghr_", "github_pat_", "glpat-", "sk_", "xox"} {
		if strings.HasPrefix(s, p) {
			return true
		}
	}
	return false
}

func isCloudflared(arg string) bool { return strings.Contains(arg, "cloudflared") }

func isShellProg(prog string) bool {
	switch prog {
	case "sh", "bash", "dash", "ash", "zsh", "ksh", "mksh", "busybox", "csh", "tcsh", "fish":
		return true
	default:
		return false
	}
}

func isShellCFlag(arg string) bool {
	if !strings.HasPrefix(arg, "-") || strings.HasPrefix(arg, "--") || len(arg) < 2 {
		return false
	}
	hasC := false
	for _, r := range arg[1:] {
		if (r < 'a' || r > 'z') && (r < 'A' || r > 'Z') {
			return false
		}
		if r == 'c' {
			hasC = true
		}
	}
	return hasC
}

func isSecretFlag(arg string) bool {
	if !strings.HasPrefix(arg, "-") {
		return false
	}
	name := strings.ToLower(strings.TrimLeft(arg, "-"))
	if name == "" {
		return false
	}
	// Single-letter flags are handled per program (e.g. mysql -p, redis-cli -a)
	// so benign flags such as ssh -p 22 or mkdir -p aren't redacted.
	if len(name) == 1 {
		return false
	}
	for _, p := range []string{"no-", "no_", "without-", "without_", "disable-", "disable_"} {
		if strings.HasPrefix(name, p) {
			return false
		}
	}
	parts := splitKeyWords(name)
	if len(parts) > 0 && nonSecretWords[parts[len(parts)-1]] {
		return false
	}
	if secretFlags[name] {
		return true
	}
	for _, s := range secretFlagSuffixes {
		if strings.HasSuffix(name, s) {
			return true
		}
	}
	if i := strings.LastIndexByte(name, '.'); i >= 0 && i+1 < len(name) {
		last := name[i+1:]
		if last != "key" && isSecretKey(last) {
			return true
		}
	}
	return false
}

// isSecretKey reports whether a KEY in a KEY=value argument, URL query param,
// header name, form body, or dotted property looks like a credential, password,
// token, or secret key.
func isSecretKey(key string) bool {
	if key == "" {
		return false
	}
	for i := range len(key) {
		c := key[i]
		isLetter := (c >= 'A' && c <= 'Z') || (c >= 'a' && c <= 'z')
		isDigit := c >= '0' && c <= '9'
		if !isLetter && !isDigit && c != '_' && c != '-' && c != '.' {
			return false
		}
	}
	parts := splitKeyWords(key)
	if len(parts) == 0 {
		return false
	}
	if nonSecretWords[parts[len(parts)-1]] {
		return false
	}
	lower := strings.ToLower(key)
	if secretURLKeys[lower] {
		return true
	}
	for _, s := range []string{"password", "passwd", "passphrase", "secret", "token", "apikey", "authkey", "requirepass", "masterauth", "_key_base", "-key-base"} {
		if strings.HasSuffix(lower, s) {
			return true
		}
	}
	hasKey := false
	hasQualifier := false
	for _, p := range parts {
		if secretEnvWords[p] {
			return true
		}
		if p == "key" {
			hasKey = true
		}
		if secretKeyQualifiers[p] {
			hasQualifier = true
		}
	}
	return hasKey && (len(parts) == 1 || hasQualifier)
}

// splitKeyWords splits snake_case, kebab-case, dotted, and camelCase keys into
// lowercase word segments (e.g. "keyStorePassword" -> ["key", "store", "password"]).
func splitKeyWords(key string) []string {
	var out []string
	start := 0
	for i := range len(key) {
		c := key[i]
		if c == '_' || c == '-' || c == '.' {
			if i > start {
				out = append(out, strings.ToLower(key[start:i]))
			}
			start = i + 1
			continue
		}
		if i > start && c >= 'A' && c <= 'Z' && key[i-1] >= 'a' && key[i-1] <= 'z' {
			out = append(out, strings.ToLower(key[start:i]))
			start = i
		}
	}
	if start < len(key) {
		out = append(out, strings.ToLower(key[start:]))
	}
	return out
}
