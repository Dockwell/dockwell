// SPDX-License-Identifier: AGPL-3.0-only

package website

import (
	"errors"
	"fmt"
	"path"
	"strings"

	resourcev1 "github.com/dockwell/dockwell/gen/dockwell/resource/v1alpha1"
	"github.com/dockwell/dockwell/internal/ingress/caddy"
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
	"log":        "log isn't allowed here: Dockwell configures this website's access logging itself",
	"log_append": "log_append isn't allowed in custom site config",
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
	lower := strings.ToLower(s)
	if strings.Contains(lower, "{$") || strings.Contains(lower, "{env.") || strings.Contains(lower, "{system.") || strings.Contains(lower, "{file.") {
		return "", errors.New("custom Caddy config can't read container environment, system or file placeholders")
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
		// Reject heredocs (<<) so multi-line strings cannot desynchronize brace counting
		// and escape the site's block.
		for _, tok := range tokens {
			if strings.HasPrefix(tok, "<<") {
				return "", fmt.Errorf("custom Caddy line %d: heredocs (<<) aren't allowed in custom site config", lineIdx+1)
			}
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
	if strings.ContainsAny(target, "{}") {
		return fmt.Errorf("custom Caddy line %d: root %q can't contain Caddy placeholders", lineNo, target)
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

// ForValidation prepares a Dockwell-generated Caddyfile for dry-run adaptation on Caddy's
// /adapt endpoint. Delegates to caddy.ForValidation so control plane and agent share one copy.
func ForValidation(config string) string {
	return caddy.ForValidation(config)
}

// ValidateAdaptedCaddyJSON inspects the canonical JSON returned by Caddy's own Caddyfile
// adapter against the site-block allowlist. Delegates to caddy.ValidateAdaptedCaddyJSON
// so control plane and agent share one single validator implementation.
func ValidateAdaptedCaddyJSON(adapted []byte, allowedRoot string) error {
	return caddy.ValidateAdaptedCaddyJSON(adapted, allowedRoot)
}
