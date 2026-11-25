// SPDX-License-Identifier: AGPL-3.0-only

package website

import (
	"cmp"
	"encoding/json"
	"errors"
	"fmt"
	"net"
	"net/url"
	"path"
	"regexp"
	"slices"
	"strconv"
	"strings"

	resourcev1 "github.com/dockwell/dockwell/gen/dockwell/resource/v1alpha1"
)

// MaxCustomCaddyBytes caps the size of a website's Advanced Caddy block.
const MaxCustomCaddyBytes = 16384

// forbiddenDirectives cannot appear inside a website's scoped custom Caddy block.
var forbiddenDirectives = map[string]string{
	"import":     "import isn't allowed in custom site config: Dockwell manages included files itself",
	"tls":        "tls isn't allowed here: HTTPS certificates are managed by Dockwell's ingress",
	"bind":       "bind isn't allowed here: the website container listens only on its internal port",
	"admin":      "admin isn't allowed here: Caddy's admin API stays disabled inside website containers",
	"auto_https": "auto_https isn't allowed here: HTTPS is managed by Dockwell's ingress",
	"servers":    "global server options aren't allowed here: write directives for this website's block alone",
	"http_port":  "global options aren't allowed here: write directives for this website's block alone",
	"https_port": "global options aren't allowed here: write directives for this website's block alone",
	"storage":    "global options aren't allowed here: write directives for this website's block alone",
	"pki":        "global options aren't allowed here: write directives for this website's block alone",
	"acme_ca":    "global options aren't allowed here: write directives for this website's block alone",
	"email":      "global options aren't allowed here: write directives for this website's block alone",
	"log_append": "",
}

// AllowedRoot returns the container directory a website is allowed to serve files from,
// or empty for a proxy website that serves no local folder.
func AllowedRoot(w *resourcev1.Website) string {
	switch KindOf(w) {
	case Folder:
		return FolderTarget
	case PHP:
		return PHPTarget
	default:
		return ""
	}
}

// CheckCustomCaddy validates and normalizes a website's scoped custom Caddy directives
// before they are wrapped inside the site's :8080 { ... } block.
func CheckCustomCaddy(raw, allowedRoot string) (string, error) {
	s := strings.ReplaceAll(raw, "\r\n", "\n")
	s = strings.Trim(s, "\n")
	if strings.TrimSpace(s) == "" {
		return "", nil
	}
	if len(s) > MaxCustomCaddyBytes {
		return "", fmt.Errorf("custom Caddy config is %d bytes, over the %d-byte limit", len(s), MaxCustomCaddyBytes)
	}
	for i := range len(s) {
		c := s[i]
		if c == 0 || (c < 0x20 && c != '\n' && c != '\t') {
			return "", errors.New("custom Caddy config contains control characters that aren't allowed")
		}
	}
	if strings.Contains(s, "{$") || strings.Contains(s, "{env.") || strings.Contains(s, "{system.") {
		return "", errors.New("custom Caddy config can't read container environment or system placeholders")
	}

	depth := 0
	lines := strings.Split(s, "\n")
	for lineIdx, line := range lines {
		tokens, err := tokenizeCaddyLine(line)
		if err != nil {
			return "", fmt.Errorf("custom Caddy line %d: %w", lineIdx+1, err)
		}
		if len(tokens) == 0 {
			continue
		}
		// Reject closing a block we didn't open (escaping the site block).
		for _, tok := range tokens {
			switch tok {
			case "{":
				depth++
			case "}":
				depth--
				if depth < 0 {
					return "", fmt.Errorf("custom Caddy line %d: unexpected '}' closes the website's own block", lineIdx+1)
				}
			}
		}
		first := strings.ToLower(tokens[0])
		if first == "}" || first == "{" {
			continue
		}
		// Reject global options block or snippet definitions at site scope.
		if strings.HasPrefix(first, "(") {
			return "", fmt.Errorf("custom Caddy line %d: snippets aren't allowed inside a site block", lineIdx+1)
		}
		if reason := forbiddenDirectives[first]; reason != "" {
			return "", fmt.Errorf("custom Caddy line %d: %s", lineIdx+1, reason)
		}
		// Prevent root pointing outside the website's mounted folder.
		if first == "root" {
			if err := checkCustomRootDirective(tokens, allowedRoot, lineIdx+1); err != nil {
				return "", err
			}
		}
		// Prevent host matchers scoping to another website's name.
		if first == "host" || (strings.HasPrefix(first, "@") && len(tokens) >= 2 && strings.EqualFold(tokens[1], "host")) {
			return "", fmt.Errorf("custom Caddy line %d: host matchers aren't allowed: this config is scoped to this website alone", lineIdx+1)
		}
	}
	if depth != 0 {
		return "", errors.New("custom Caddy config has an unclosed '{' block")
	}
	return s, nil
}

func checkCustomRootDirective(tokens []string, allowedRoot string, lineNo int) error {
	if allowedRoot == "" {
		return fmt.Errorf("custom Caddy line %d: root isn't allowed on a proxy website that mounts no folder", lineNo)
	}
	// root [<matcher>] <path>
	var target string
	switch len(tokens) {
	case 2:
		target = tokens[1]
	case 3:
		target = tokens[2]
	default:
		return fmt.Errorf("custom Caddy line %d: root expects a folder inside %s", lineNo, allowedRoot)
	}
	if !pathWithin(target, allowedRoot) {
		return fmt.Errorf("custom Caddy line %d: root %q must stay inside %s", lineNo, target, allowedRoot)
	}
	return nil
}

func pathWithin(target, allowedRoot string) bool {
	if allowedRoot == "" || !strings.HasPrefix(target, "/") {
		return false
	}
	clean := path.Clean(target)
	return clean == allowedRoot || strings.HasPrefix(clean, allowedRoot+"/")
}

// tokenizeCaddyLine splits one Caddyfile line into tokens, stripping # comments and
// respecting double-quoted and backtick-quoted strings.
func tokenizeCaddyLine(line string) ([]string, error) {
	var tokens []string
	var cur strings.Builder
	inToken := false
	i := 0
	for i < len(line) {
		c := line[i]
		switch {
		case c == ' ' || c == '\t':
			if inToken {
				tokens = append(tokens, cur.String())
				cur.Reset()
				inToken = false
			}
			i++
		case c == '#' && !inToken:
			return tokens, nil
		case c == '"':
			if inToken {
				tokens = append(tokens, cur.String())
				cur.Reset()
				inToken = false
			}
			i++
			var q strings.Builder
			closed := false
			for i < len(line) {
				if line[i] == '\\' && i+1 < len(line) {
					q.WriteByte(line[i+1])
					i += 2
					continue
				}
				if line[i] == '"' {
					closed = true
					i++
					break
				}
				q.WriteByte(line[i])
				i++
			}
			if !closed {
				return nil, errors.New("unclosed double quote")
			}
			tokens = append(tokens, q.String())
		case c == '`':
			if inToken {
				tokens = append(tokens, cur.String())
				cur.Reset()
				inToken = false
			}
			i++
			var q strings.Builder
			closed := false
			for i < len(line) {
				if line[i] == '`' {
					closed = true
					i++
					break
				}
				q.WriteByte(line[i])
				i++
			}
			if !closed {
				return nil, errors.New("unclosed backtick string")
			}
			tokens = append(tokens, q.String())
		default:
			cur.WriteByte(c)
			inToken = true
			i++
		}
	}
	if inToken {
		tokens = append(tokens, cur.String())
	}
	return tokens, nil
}

// writeCustomCaddy writes the validated custom directives indented by indent inside a block.
func writeCustomCaddy(b *strings.Builder, custom, indent string) {
	if strings.TrimSpace(custom) == "" {
		return
	}
	for _, line := range strings.Split(custom, "\n") {
		if strings.TrimSpace(line) == "" {
			b.WriteByte('\n')
			continue
		}
		b.WriteString(indent)
		b.WriteString(line)
		b.WriteByte('\n')
	}
}

var basicAuthPlaceholderRE = regexp.MustCompile(`\{\$DW_BASIC_AUTH_HASH_\d+\}`)

// dummyValidationHash is a syntactically valid 60-byte bcrypt hash used ONLY during
// dry-run `caddy adapt` validation when site environment variables are absent.
const dummyValidationHash = "$2a$10$00000000000000000000000000000000000000000000000000000"

// ForValidation prepares a Dockwell-generated Caddyfile for dry-run adaptation on Caddy's
// /adapt endpoint by substituting Dockwell's Basic Auth env-var placeholders with a
// syntactically valid dummy hash. Never used to run a container.
func ForValidation(config string) string {
	return basicAuthPlaceholderRE.ReplaceAllStringFunc(config, func(string) string {
		return dummyValidationHash
	})
}

// ValidateAdaptedCaddyJSON inspects the canonical JSON returned by Caddy's own Caddyfile
// adapter and verifies that custom directives never escaped the site's :8080 server block,
// never enabled admin or TLS, never claimed another site's hostname, never escaped the
// allowed root folder, and never targeted loopback, link-local, metadata, or Docker API upstreams.
func ValidateAdaptedCaddyJSON(adapted []byte, allowedRoot string) error {
	var top map[string]any
	if err := json.Unmarshal(adapted, &top); err != nil {
		return fmt.Errorf("adapted Caddy config isn't valid JSON: %w", err)
	}
	admin, _ := top["admin"].(map[string]any)
	if disabled, _ := admin["disabled"].(bool); !disabled {
		return errors.New("adapted config must keep Caddy's admin API disabled")
	}
	if listen, _ := admin["listen"].(string); listen != "" {
		return errors.New("adapted config can't open a Caddy admin listener")
	}
	apps, _ := top["apps"].(map[string]any)
	for appName := range apps {
		if appName != "http" {
			return fmt.Errorf("custom Caddy config can't configure Caddy's %q app; only HTTP directives for this website are allowed", appName)
		}
	}
	httpApp, _ := apps["http"].(map[string]any)
	servers, _ := httpApp["servers"].(map[string]any)
	if len(servers) != 1 {
		return fmt.Errorf("custom Caddy config must stay inside this website's single server block (got %d servers)", len(servers))
	}
	wantListen := fmt.Sprintf(":%d", Port)
	for _, rawSrv := range servers {
		srv, _ := rawSrv.(map[string]any)
		listen, _ := srv["listen"].([]any)
		if len(listen) != 1 || listen[0] != wantListen {
			return fmt.Errorf("custom Caddy config must listen only on %s (got %v)", wantListen, listen)
		}
		if routes, ok := srv["routes"].([]any); ok {
			if err := checkAdaptedRoutes(routes, allowedRoot); err != nil {
				return err
			}
		}
	}
	return nil
}

func checkAdaptedRoutes(routes []any, allowedRoot string) error {
	for _, item := range routes {
		rt, ok := item.(map[string]any)
		if !ok {
			continue
		}
		if matchers, ok := rt["match"].([]any); ok {
			for _, mItem := range matchers {
				m, _ := mItem.(map[string]any)
				if _, hasHost := m["host"]; hasHost {
					return errors.New("custom Caddy config can't match another site's hostname")
				}
				if notList, ok := m["not"].([]any); ok {
					for _, nItem := range notList {
						if nm, _ := nItem.(map[string]any); nm != nil {
							if _, hasHost := nm["host"]; hasHost {
								return errors.New("custom Caddy config can't match another site's hostname")
							}
						}
					}
				}
			}
		}
		if handlers, ok := rt["handle"].([]any); ok {
			for _, hItem := range handlers {
				h, ok := hItem.(map[string]any)
				if !ok {
					continue
				}
				if err := checkAdaptedHandler(h, allowedRoot); err != nil {
					return err
				}
			}
		}
	}
	return nil
}

func checkAdaptedHandler(h map[string]any, allowedRoot string) error {
	name, _ := h["handler"].(string)
	switch name {
	case "subroute":
		if routes, ok := h["routes"].([]any); ok {
			if err := checkAdaptedRoutes(routes, allowedRoot); err != nil {
				return err
			}
		}
	case "vars":
		if rootVal, ok := h["root"].(string); ok && rootVal != "" {
			if !pathWithin(rootVal, allowedRoot) {
				return fmt.Errorf("custom Caddy root %q must stay inside %s", rootVal, cmp.Or(allowedRoot, "the website's folder"))
			}
		}
	case "file_server":
		if allowedRoot == "" {
			return errors.New("file_server isn't allowed on a proxy website that mounts no folder")
		}
		if rootVal, ok := h["root"].(string); ok && rootVal != "" {
			if !pathWithin(rootVal, allowedRoot) {
				return fmt.Errorf("file_server root %q must stay inside %s", rootVal, allowedRoot)
			}
		}
	case "reverse_proxy":
		if _, hasDyn := h["dynamic_upstreams"]; hasDyn {
			return errors.New("dynamic_upstreams isn't allowed in custom Caddy config")
		}
		if ups, ok := h["upstreams"].([]any); ok {
			for _, uItem := range ups {
				u, _ := uItem.(map[string]any)
				dial, _ := u["dial"].(string)
				if err := checkAdaptedDial(dial); err != nil {
					return err
				}
			}
		}
		if hr, ok := h["handle_response"].([]any); ok {
			for _, rItem := range hr {
				rm, _ := rItem.(map[string]any)
				if routes, ok := rm["routes"].([]any); ok {
					if err := checkAdaptedRoutes(routes, allowedRoot); err != nil {
						return err
					}
				}
			}
		}
	case "templates":
		return errors.New("caddy's templates handler isn't allowed in custom site config")
	}
	return nil
}

func checkAdaptedDial(dial string) error {
	if dial == "" {
		return nil
	}
	if strings.HasPrefix(dial, "unix/") || strings.HasPrefix(dial, "unixgram/") || strings.HasPrefix(dial, "unixpacket/") {
		return fmt.Errorf("custom reverse_proxy can't dial unix socket %q", dial)
	}
	// Caddy dials host:port (or may carry a scheme if unparsed).
	host, portStr, err := net.SplitHostPort(dial)
	if err != nil {
		if u, uerr := url.Parse(dial); uerr == nil && u.Host != "" {
			host = u.Hostname()
			portStr = u.Port()
		} else {
			host = dial
		}
	}
	host = strings.ToLower(strings.Trim(host, "[]"))
	// Allow internal website network aliases (php, subpath-N) generated by Dockwell.
	if host == PHPService || strings.HasPrefix(host, "subpath-") {
		return nil
	}
	if err := checkHost(host); err != nil {
		return fmt.Errorf("custom reverse_proxy upstream %q: %w", dial, err)
	}
	if portStr != "" {
		if n, err := strconv.Atoi(portStr); err == nil && slices.Contains(dockerPorts, n) {
			return fmt.Errorf("custom reverse_proxy can't dial Docker's API port %d", n)
		}
	}
	return nil
}
