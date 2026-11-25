// SPDX-License-Identifier: AGPL-3.0-only

package caddy

import (
	"bytes"
	"cmp"
	"context"
	"crypto/x509"
	"encoding/json"
	"encoding/pem"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/netip"
	"net/url"
	"os"
	"path"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"syscall"
	"time"

	"github.com/dockwell/dockwell/internal/ingress"
	"github.com/dockwell/dockwell/internal/lazyre"
)

const (
	rateLimitPathPrefix = "/ratelimit/"
	maxKeyFile          = 128 << 10
)

// rateLimitURI is what a rate-limited route asks its agent on signin.sock.
func rateLimitURI(id string, rl *ingress.RateLimit) string {
	return fmt.Sprintf("%s%s?rpm=%d&burst=%d", rateLimitPathPrefix, id, rl.RequestsPerMinute, rl.Burst)
}

// rateLimitAsk asks the agent on signin.sock whether the believed visitor address is
// within the route's rate limit. 2xx continues to the route; 429 + Retry-After goes
// straight back to the visitor.
func rateLimitAsk(r ingress.Route) handler {
	client := peerAddress
	if believes(r) {
		client = "{http.vars." + clientVar + "}"
	}
	return handler{
		Handler:   "reverse_proxy",
		Upstreams: []upstream{{Dial: r.RateLimit.Dial}},
		Rewrite:   &rewrite{Method: http.MethodGet, URI: rateLimitURI(r.ID, r.RateLimit)},
		Headers: &headers{Request: &headerOps{Set: map[string][]string{
			signInClientHeader: {client},
		}}},
		HandleResponse: []responseHandler{{Match: &responseMatch{StatusCode: []int{2}}, Routes: []beliefRoute{
			{Handle: []map[string]any{{"handler": "vars"}}},
		}}},
	}
}

// readRateLimit parses rateLimitAsk's handler back into an ingress.RateLimit.
func readRateLimit(h handler) (*ingress.RateLimit, bool) {
	if h.Handler != "reverse_proxy" || h.Rewrite == nil || len(h.Upstreams) != 1 || !strings.HasPrefix(h.Rewrite.URI, rateLimitPathPrefix) {
		return nil, false
	}
	u, err := url.Parse(h.Rewrite.URI)
	if err != nil {
		return nil, false
	}
	rpm, err := strconv.ParseUint(u.Query().Get("rpm"), 10, 32)
	if err != nil || rpm == 0 {
		return nil, false
	}
	burst, err := strconv.ParseUint(u.Query().Get("burst"), 10, 32)
	if err != nil {
		return nil, false
	}
	return &ingress.RateLimit{
		RequestsPerMinute: uint32(rpm),
		Burst:             uint32(burst),
		Dial:              h.Upstreams[0].Dial,
	}, true
}

// certMeta holds X.509 details read from an issued certificate in Caddy's storage.
type certMeta struct {
	NotBefore time.Time
	NotAfter  time.Time
	Issuer    string
	Subject   string
	SANs      []string
	CertPEM   string
	KeyPEM    string
}

func certMetaFromX509(leaf *x509.Certificate) certMeta {
	issuer := cmp.Or(leaf.Issuer.CommonName, strings.Join(leaf.Issuer.Organization, ", "))
	subject := cmp.Or(leaf.Subject.CommonName, func() string {
		if len(leaf.DNSNames) > 0 {
			return leaf.DNSNames[0]
		}
		return ""
	}())
	return certMeta{
		NotBefore: leaf.NotBefore,
		NotAfter:  leaf.NotAfter,
		Issuer:    issuer,
		Subject:   subject,
		SANs:      slices.Clone(leaf.DNSNames),
	}
}

// readIssuedCert finds the latest issued certificate for domain in Caddy's storage,
// optionally reading its private key PEM as well.
func (c *Caddy) readIssuedCert(domain string, includeKey bool) (certMeta, bool) {
	var best certMeta
	var bestFound bool
	root, err := os.OpenRoot(c.dataDir())
	if err != nil {
		return best, false
	}
	defer root.Close()
	const certificates = "caddy/certificates"
	dir, err := root.OpenFile(certificates, os.O_RDONLY|syscall.O_NONBLOCK|syscall.O_DIRECTORY, 0)
	if err != nil {
		return best, false
	}
	defer dir.Close()
	issuers, err := dir.ReadDir(issuerDirs)
	if err != nil {
		return best, false
	}
	for _, issuer := range issuers {
		certPath := filepath.Join(certificates, issuer.Name(), domain, domain+".crt")
		f, err := openRegular(root, certPath)
		if err != nil {
			continue
		}
		b, err := io.ReadAll(io.LimitReader(f, maxCertFile))
		_ = f.Close()
		if err != nil {
			continue
		}
		block, _ := pem.Decode(b)
		if block == nil {
			continue
		}
		leaf, err := x509.ParseCertificate(block.Bytes)
		if err != nil {
			continue
		}
		if !bestFound || leaf.NotAfter.After(best.NotAfter) {
			meta := certMetaFromX509(leaf)
			meta.CertPEM = string(b)
			if includeKey {
				keyPath := filepath.Join(certificates, issuer.Name(), domain, domain+".key")
				if kf, kerr := openRegular(root, keyPath); kerr == nil {
					if kb, rerr := io.ReadAll(io.LimitReader(kf, maxKeyFile)); rerr == nil {
						meta.KeyPEM = string(kb)
					}
					_ = kf.Close()
				}
			}
			best = meta
			bestFound = true
		}
	}
	return best, bestFound
}

// ReadCertificate reads an issued ACME certificate (and optionally its private key)
// for domain from Caddy's storage.
func (c *Caddy) ReadCertificate(_ context.Context, domain string, includeKey bool) (*ingress.CertificateRead, error) {
	meta, ok := c.readIssuedCert(domain, includeKey)
	if !ok {
		return nil, fmt.Errorf("no issued certificate is stored on this server for %s", domain)
	}
	if includeKey && meta.KeyPEM == "" {
		return nil, fmt.Errorf("the private key for %s couldn't be read from Caddy storage", domain)
	}
	return &ingress.CertificateRead{
		CertPEM:   meta.CertPEM,
		KeyPEM:    meta.KeyPEM,
		Issuer:    meta.Issuer,
		Subject:   meta.Subject,
		SANs:      meta.SANs,
		NotBefore: meta.NotBefore,
		NotAfter:  meta.NotAfter,
	}, nil
}

// RenewCertificate triggers immediate renewal for domain. When force is true, it
// clears the cached certificate files for domain and cycles Caddy's TLS config so
// CertMagic drops the in-memory cached certificate for domain and obtains a fresh
// certificate immediately (other domains reload their existing certificates from disk).
func (c *Caddy) RenewCertificate(ctx context.Context, domain string, force bool) (string, error) {
	if !force {
		if err := c.Retry(ctx); err != nil {
			return "", err
		}
		return "Triggered certificate check on Caddy.", nil
	}
	// Snapshot current Caddy config before clearing the domain's stored files.
	fullCfg, _, err := c.call(ctx, http.MethodGet, "/config/", nil, nil)
	if err != nil {
		return "", err
	}
	if err := c.clearIssuedFiles(domain); err != nil {
		return "", err
	}
	// Load a minimal config without apps.tls so Caddy's UsagePool releases the
	// in-memory certmagic.Cache, then immediately restore the full config.
	paths := c.Runtime.Paths()
	minimal, err := json.Marshal(map[string]any{
		"admin": map[string]any{
			"listen": adminListen(paths),
			"config": map[string]any{"persist": false},
		},
	})
	if err != nil {
		return "", err
	}
	if _, _, err := c.call(ctx, http.MethodPost, "/load", minimal, http.Header{"Cache-Control": {"must-revalidate"}}); err != nil {
		return "", err
	}
	if _, _, err := c.call(ctx, http.MethodPost, "/load", fullCfg, http.Header{"Cache-Control": {"must-revalidate"}}); err != nil {
		return "", err
	}
	return "Started immediate certificate renewal on Caddy.", nil
}

func (c *Caddy) clearIssuedFiles(domain string) error {
	root, err := os.OpenRoot(c.dataDir())
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return nil
		}
		return err
	}
	defer root.Close()
	const certificates = "caddy/certificates"
	dir, err := root.OpenFile(certificates, os.O_RDONLY|syscall.O_NONBLOCK|syscall.O_DIRECTORY, 0)
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return nil
		}
		return err
	}
	defer dir.Close()
	issuers, err := dir.ReadDir(issuerDirs)
	if err != nil {
		return err
	}
	for _, issuer := range issuers {
		target := filepath.Join(certificates, issuer.Name(), domain)
		if err := root.RemoveAll(target); err != nil && !errors.Is(err, os.ErrNotExist) {
			return fmt.Errorf("can't clear cached certificate files for %s: %w", domain, err)
		}
	}
	return nil
}

// CaddyfileAdapter is an optional Runtime capability to adapt a Caddyfile when the
// ingress container isn't running yet (e.g. creating a server's first website).
type CaddyfileAdapter interface {
	AdaptCaddyfile(ctx context.Context, caddyfile []byte) ([]byte, error)
}

var (
	basicAuthPlaceholderRE = lazyre.Regexp(`\{\$DW_BASIC_AUTH_HASH_[0-9]+\}`)
	dummyValidationHash    = "$2a$10$00000000000000000000000000000000000000000000000000000"
	metadataNames          = []string{"metadata", "metadata.google.internal", "metadata.goog", "instance-data", "instance-data.ec2.internal"}
	metadataAddrs          = []netip.Addr{netip.MustParseAddr("100.100.100.200"), netip.MustParseAddr("fd00:ec2::254")}
	dockerAPIPorts         = []int{2375, 2376}
)

// ForValidation substitutes Dockwell's Basic Auth env-var placeholders with a syntactically
// valid dummy bcrypt hash for dry-run adaptation on Caddy's /adapt endpoint.
func ForValidation(config string) string {
	return basicAuthPlaceholderRE().ReplaceAllStringFunc(config, func(string) string {
		return dummyValidationHash
	})
}

// ValidateCaddyfile adapts caddyfile with Caddy's own Caddyfile adapter and verifies
// the resulting JSON stays strictly inside the website's :8080 server block.
func (c *Caddy) ValidateCaddyfile(ctx context.Context, caddyfile, allowedRoot string) (bool, string, error) {
	prepared := []byte(ForValidation(caddyfile))
	adapted, _, err := c.call(ctx, http.MethodPost, "/adapt", prepared, http.Header{"Content-Type": {"text/caddyfile"}})
	if errors.Is(err, errDown) {
		if adapter, ok := c.Runtime.(CaddyfileAdapter); ok {
			adapted, err = adapter.AdaptCaddyfile(ctx, prepared)
		}
	}
	if err != nil {
		var ae *adminError
		if errors.As(err, &ae) {
			return false, ae.Message, nil
		}
		if errors.Is(err, errDown) {
			return false, "", fmt.Errorf("caddy isn't running on this server to validate custom config: %w", err)
		}
		return false, err.Error(), nil
	}
	// Caddy's HTTP /adapt endpoint wraps the config JSON in {"result": {...}, "warnings": [...]}.
	var envelope struct {
		Result json.RawMessage `json:"result"`
	}
	if json.Unmarshal(adapted, &envelope) == nil && len(envelope.Result) > 0 && envelope.Result[0] == '{' {
		adapted = envelope.Result
	}
	if err := ValidateAdaptedCaddyJSON(adapted, allowedRoot); err != nil {
		return false, err.Error(), nil //nolint:nilerr // adapter/policy rejection is reported in valid=false, msg
	}
	return true, "", nil
}

// ValidateAdaptedCaddyJSON checks the canonical JSON produced by Caddy's Caddyfile adapter
// to ensure custom site directives never escape :8080, never enable admin or TLS, never
// match another site's hostname, never escape allowedRoot, and never dial unsafe upstreams.
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
	for _, rawSrv := range servers {
		srv, _ := rawSrv.(map[string]any)
		listen, _ := srv["listen"].([]any)
		if len(listen) != 1 || listen[0] != ":8080" {
			return fmt.Errorf("custom Caddy config must listen only on :8080 (got %v)", listen)
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
			return checkAdaptedRoutes(routes, allowedRoot)
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

func pathWithin(target, allowedRoot string) bool {
	if allowedRoot == "" || !strings.HasPrefix(target, "/") {
		return false
	}
	clean := path.Clean(target)
	return clean == allowedRoot || strings.HasPrefix(clean, allowedRoot+"/")
}

func checkAdaptedDial(dial string) error {
	if dial == "" {
		return nil
	}
	if strings.HasPrefix(dial, "unix/") || strings.HasPrefix(dial, "unixgram/") || strings.HasPrefix(dial, "unixpacket/") {
		return fmt.Errorf("custom reverse_proxy can't dial unix socket %q", dial)
	}
	host, portStr, err := net.SplitHostPort(dial)
	if err != nil {
		if u, uerr := url.Parse(dial); uerr == nil && u.Host != "" {
			host, portStr = u.Hostname(), u.Port()
		} else {
			host = dial
		}
	}
	host = strings.ToLower(strings.Trim(host, "[]"))
	if host == "php" || strings.HasPrefix(host, "subpath-") {
		return nil
	}
	if addr, err := netip.ParseAddr(host); err == nil {
		addr = addr.Unmap()
		switch {
		case addr.IsLoopback():
			return fmt.Errorf("custom reverse_proxy can't dial loopback address %s", host)
		case addr.IsUnspecified():
			return fmt.Errorf("custom reverse_proxy can't dial unspecified address %s", host)
		case addr.IsLinkLocalUnicast() || addr.IsLinkLocalMulticast() || slices.Contains(metadataAddrs, addr):
			return fmt.Errorf("custom reverse_proxy can't dial link-local or metadata address %s", host)
		}
	} else {
		if host == "localhost" || strings.HasSuffix(host, ".localhost") {
			return fmt.Errorf("custom reverse_proxy can't dial %s", host)
		}
		if slices.Contains(metadataNames, strings.TrimSuffix(host, ".")) {
			return fmt.Errorf("custom reverse_proxy can't dial metadata host %s", host)
		}
	}
	if portStr != "" {
		if n, err := strconv.Atoi(portStr); err == nil && slices.Contains(dockerAPIPorts, n) {
			return fmt.Errorf("custom reverse_proxy can't dial Docker's API port %d", n)
		}
	}
	return nil
}

// Verify interfaces on *Caddy.
var (
	_ ingress.CaddyfileValidator = (*Caddy)(nil)
	_ ingress.CertificateRenewer = (*Caddy)(nil)
	_ ingress.CertificateReader  = (*Caddy)(nil)
)

// unused import guard for bytes if needed
var _ = bytes.TrimSpace
