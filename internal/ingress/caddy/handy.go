// SPDX-License-Identifier: AGPL-3.0-only

package caddy

import (
	"cmp"
	"context"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/tls"
	"crypto/x509"
	"crypto/x509/pkix"
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

	"golang.org/x/crypto/acme"

	"github.com/dockwell/dockwell/internal/ingress"
	"github.com/dockwell/dockwell/internal/lazyre"
)

const (
	rateLimitPathPrefix = "/ratelimit/"
	maxKeyFile          = 128 << 10
	// forceRenewTimeout bounds an in-place forced ACME HTTP-01 renewal.
	forceRenewTimeout = 30 * time.Second
	// renewedCertTag marks certificates swapped in place by RenewCertificate(force=true).
	renewedCertTag = "dockwell.renewed"
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
	if err := ingress.CheckDomainName(domain); err != nil {
		return best, false
	}
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
	if err := ingress.CheckDomainName(domain); err != nil {
		return nil, err
	}
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

// RenewCertificate triggers renewal for domain without deleting existing valid
// certificate files or dropping any routes on the server (M3). When force is true,
// it obtains a fresh certificate from the configured ACME CA via HTTP-01 (using a
// single temporary challenge route on Caddy's HTTP server while every other route
// and domain's current HTTPS certificate stay live), verifies the new certificate,
// and only then swaps it atomically into Caddy's storage and live TLS config.
func (c *Caddy) RenewCertificate(ctx context.Context, domain string, force bool) (string, bool, error) {
	if err := ingress.CheckDomainName(domain); err != nil {
		return "", false, err
	}
	prev, hadPrev := c.readIssuedCert(domain, false)
	if !force {
		if err := c.Retry(ctx); err != nil {
			return "", false, err
		}
		due := !hadPrev || time.Until(prev.NotAfter) <= 30*24*time.Hour
		return "Triggered certificate check on Caddy.", due, nil
	}

	renewCtx, cancel := context.WithTimeout(ctx, forceRenewTimeout)
	defer cancel()

	certPEM, keyPEM, err := c.obtainForcedACMECert(renewCtx, domain)
	if err != nil {
		if hadPrev {
			return "", false, fmt.Errorf("forced renewal for %s failed (%w); the existing valid certificate remains in place", domain, err)
		}
		return "", false, fmt.Errorf("forced renewal for %s failed: %w", domain, err)
	}

	// Only after the new certificate is obtained and verified do we swap it into
	// Caddy's storage and live TLS config.
	if err := c.writeRenewedCertToStorage(domain, certPEM, keyPEM); err != nil {
		return "", false, fmt.Errorf("obtained new certificate for %s, but couldn't persist it: %w", domain, err)
	}
	domainTag := renewedCertTag + "." + domain
	if _, err := c.editLoaded(renewCtx, func(loaded []map[string]any) []map[string]any {
		loaded = slices.DeleteFunc(loaded, func(cert map[string]any) bool { return tagged(cert, domainTag) })
		return append(loaded, map[string]any{
			"certificate": string(certPEM),
			"key":         string(keyPEM),
			"tags":        []any{renewedCertTag, domainTag},
		})
	}); err != nil {
		return "", false, fmt.Errorf("persisted renewed certificate for %s, but couldn't load it into Caddy: %w", domain, err)
	}
	if err := c.selectRenewedCert(renewCtx, domain, domainTag); err != nil {
		return "", false, fmt.Errorf("loaded renewed certificate for %s, but couldn't activate it on TLS: %w", domain, err)
	}
	c.keepRunning(renewCtx)
	return fmt.Sprintf("Renewed certificate for %s in place.", domain), true, nil
}

// selectRenewedCert adds or updates a domain-scoped TLS connection policy on Dockwell's
// HTTPS server so Caddy immediately selects the newly renewed certificate over any
// previously cached managed certificate, without affecting any other domain.
func (c *Caddy) selectRenewedCert(ctx context.Context, domain, tag string) error {
	servers, _, up, err := c.readServers(ctx)
	if err != nil || !up || servers == nil {
		return err
	}
	raw, ok := servers[serverName]
	if !ok {
		return nil
	}
	var srv map[string]any
	if err := json.Unmarshal(raw, &srv); err != nil {
		return err
	}
	var existing []any
	if p, ok := srv["tls_connection_policies"].([]any); ok {
		existing = p
	}
	var kept []any
	hasCatchAll := false
	for _, entry := range existing {
		m, _ := entry.(map[string]any)
		if m == nil {
			continue
		}
		if match, _ := m["match"].(map[string]any); match != nil {
			if snis, _ := match["sni"].([]any); len(snis) == 1 && snis[0] == domain {
				continue
			}
		} else if len(m) == 0 {
			hasCatchAll = true
			continue
		}
		kept = append(kept, m)
	}
	policy := map[string]any{
		"match": map[string]any{
			"sni": []string{domain},
		},
		"certificate_selection": map[string]any{
			"any_tag": []string{tag},
		},
	}
	policies := append([]any{policy}, kept...)
	if hasCatchAll || len(policies) > 0 {
		policies = append(policies, map[string]any{})
	}
	srv["tls_connection_policies"] = policies
	updated, err := json.Marshal(srv)
	if err != nil {
		return err
	}
	servers[serverName] = updated
	body, err := json.Marshal(servers)
	if err != nil {
		return err
	}
	_, _, err = c.call(ctx, http.MethodPatch, serversPath, body, nil)
	return err
}

func (c *Caddy) acmeHTTPClient() (*http.Client, error) {
	tr := http.DefaultTransport.(*http.Transport).Clone()
	if c.ACMERoots != "" {
		rootsPEM, err := os.ReadFile(c.ACMERoots)
		if err != nil {
			return nil, fmt.Errorf("read ACME roots: %w", err)
		}
		pool := x509.NewCertPool()
		if !pool.AppendCertsFromPEM(rootsPEM) {
			return nil, errors.New("no valid PEM certificates in ACME roots")
		}
		tr.TLSClientConfig = &tls.Config{RootCAs: pool, MinVersion: tls.VersionTLS12}
	}
	return &http.Client{Transport: tr, Timeout: 20 * time.Second}, nil
}

func (c *Caddy) obtainForcedACMECert(ctx context.Context, domain string) (certPEM, keyPEM []byte, err error) {
	httpClient, err := c.acmeHTTPClient()
	if err != nil {
		return nil, nil, err
	}
	acctKey, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		return nil, nil, fmt.Errorf("generate ACME account key: %w", err)
	}
	client := &acme.Client{
		Key:          acctKey,
		DirectoryURL: cmp.Or(c.ACMEDirectory, acme.LetsEncryptURL),
		HTTPClient:   httpClient,
	}
	if _, err := client.Register(ctx, &acme.Account{}, acme.AcceptTOS); err != nil && !errors.Is(err, acme.ErrAccountAlreadyExists) {
		return nil, nil, fmt.Errorf("register ACME account: %w", err)
	}
	order, err := client.AuthorizeOrder(ctx, acme.DomainIDs(domain))
	if err != nil {
		return nil, nil, fmt.Errorf("authorize order: %w", err)
	}
	for _, authzURL := range order.AuthzURLs {
		authz, err := client.GetAuthorization(ctx, authzURL)
		if err != nil {
			return nil, nil, fmt.Errorf("read authorization: %w", err)
		}
		if authz.Status == acme.StatusValid {
			continue
		}
		var chal *acme.Challenge
		for _, ch := range authz.Challenges {
			if ch.Type == "http-01" {
				chal = ch
				break
			}
		}
		if chal == nil {
			return nil, nil, errors.New("CA offered no HTTP-01 challenge")
		}
		keyAuth, err := client.HTTP01ChallengeResponse(chal.Token)
		if err != nil {
			return nil, nil, fmt.Errorf("build HTTP-01 response: %w", err)
		}
		cleanup, err := c.installHTTP01ChallengeRoute(ctx, domain, chal.Token, keyAuth)
		if err != nil {
			return nil, nil, fmt.Errorf("install HTTP-01 challenge route: %w", err)
		}
		_, acceptErr := client.Accept(ctx, chal)
		var waitErr error
		if acceptErr == nil {
			_, waitErr = client.WaitAuthorization(ctx, authzURL)
		}
		cleanup()
		if acceptErr != nil {
			return nil, nil, fmt.Errorf("accept HTTP-01 challenge: %w", acceptErr)
		}
		if waitErr != nil {
			return nil, nil, fmt.Errorf("HTTP-01 validation failed: %w", waitErr)
		}
	}
	order, err = client.WaitOrder(ctx, order.URI)
	if err != nil {
		return nil, nil, fmt.Errorf("wait for ACME order: %w", err)
	}

	leafKey, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		return nil, nil, fmt.Errorf("generate certificate key: %w", err)
	}
	csr, err := x509.CreateCertificateRequest(rand.Reader, &x509.CertificateRequest{
		Subject:  pkix.Name{CommonName: domain},
		DNSNames: []string{domain},
	}, leafKey)
	if err != nil {
		return nil, nil, fmt.Errorf("create CSR: %w", err)
	}
	ders, _, err := client.CreateCertFromOrder(ctx, order, csr, true)
	if err != nil {
		return nil, nil, fmt.Errorf("finalize certificate order: %w", err)
	}
	if len(ders) == 0 {
		return nil, nil, errors.New("CA returned an empty certificate chain")
	}
	leaf, err := x509.ParseCertificate(ders[0])
	if err != nil {
		return nil, nil, fmt.Errorf("parse issued certificate: %w", err)
	}
	if err := leaf.VerifyHostname(domain); err != nil {
		return nil, nil, fmt.Errorf("issued certificate does not match %s: %w", domain, err)
	}
	var certBuf strings.Builder
	for _, der := range ders {
		certBuf.Write(pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: der}))
	}
	keyDER, err := x509.MarshalECPrivateKey(leafKey)
	if err != nil {
		return nil, nil, fmt.Errorf("marshal certificate key: %w", err)
	}
	keyPEM = pem.EncodeToMemory(&pem.Block{Type: "EC PRIVATE KEY", Bytes: keyDER})
	return []byte(certBuf.String()), keyPEM, nil
}

// installHTTP01ChallengeRoute temporarily prepends one HTTP-01 challenge route to
// Caddy's HTTP server without dropping any existing routes or servers. The returned
// cleanup function removes only that challenge route.
func (c *Caddy) installHTTP01ChallengeRoute(ctx context.Context, domain, token, keyAuth string) (func(), error) {
	httpPort, httpsPort := c.ports()
	servers, _, up, err := c.readServers(ctx)
	if err != nil {
		return nil, err
	}
	if !up || servers == nil {
		return nil, errors.New("Caddy isn't serving routes right now")
	}
	const challengeID = "dockwell.acme.http01"
	challengeRoute := map[string]any{
		"@id":      challengeID,
		"terminal": true,
		"match": []any{
			map[string]any{
				"host": []string{domain},
				"path": []string{"/.well-known/acme-challenge/" + token},
			},
		},
		"handle": []any{
			map[string]any{
				"handler":     "static_response",
				"status_code": 200,
				"body":        keyAuth,
			},
		},
	}

	hadHTTPServer := false
	var prevHTTPRaw json.RawMessage
	if raw, ok := servers[httpServerName]; ok {
		hadHTTPServer = true
		prevHTTPRaw = append(json.RawMessage(nil), raw...)
		var srv map[string]any
		if err := json.Unmarshal(raw, &srv); err != nil {
			return nil, fmt.Errorf("parse HTTP server config: %w", err)
		}
		var routes []any
		if existing, ok := srv["routes"].([]any); ok {
			for _, r := range existing {
				if m, ok := r.(map[string]any); ok && m["@id"] == challengeID {
					continue
				}
				routes = append(routes, r)
			}
		}
		srv["routes"] = append([]any{challengeRoute}, routes...)
		updated, err := json.Marshal(srv)
		if err != nil {
			return nil, err
		}
		servers[httpServerName] = updated
	} else {
		// No explicit plain HTTP server yet: add one for the challenge plus the standard
		// HTTP->HTTPS redirect so other HTTP requests keep redirecting smoothly.
		redirLoc := "https://{http.request.host}{http.request.uri}"
		if httpsPort != 443 {
			redirLoc = fmt.Sprintf("https://{http.request.host}:%d{http.request.uri}", httpsPort)
		}
		fallbackRedirect := map[string]any{
			"handle": []any{
				map[string]any{
					"handler":     "static_response",
					"status_code": 308,
					"headers":     map[string][]string{"Location": {redirLoc}},
				},
			},
		}
		srv := map[string]any{
			"listen": []string{":" + strconv.Itoa(httpPort)},
			"routes": []any{challengeRoute, fallbackRedirect},
		}
		updated, err := json.Marshal(srv)
		if err != nil {
			return nil, err
		}
		servers[httpServerName] = updated
	}

	body, err := json.Marshal(servers)
	if err != nil {
		return nil, err
	}
	if _, _, err := c.call(ctx, http.MethodPatch, serversPath, body, nil); err != nil {
		return nil, err
	}
	cleanup := func() {
		cleanCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), 10*time.Second)
		defer cancel()
		cur, _, ok, err := c.readServers(cleanCtx)
		if err != nil || !ok || cur == nil {
			return
		}
		if hadHTTPServer {
			var srv map[string]any
			if raw, exists := cur[httpServerName]; exists && json.Unmarshal(raw, &srv) == nil {
				var routes []any
				if existing, ok := srv["routes"].([]any); ok {
					for _, r := range existing {
						if m, ok := r.(map[string]any); ok && m["@id"] == challengeID {
							continue
						}
						routes = append(routes, r)
					}
				}
				srv["routes"] = routes
				if updated, err := json.Marshal(srv); err == nil {
					cur[httpServerName] = updated
				}
			} else if len(prevHTTPRaw) > 0 {
				cur[httpServerName] = prevHTTPRaw
			}
		} else {
			delete(cur, httpServerName)
		}
		if patched, err := json.Marshal(cur); err == nil {
			_, _, _ = c.call(cleanCtx, http.MethodPatch, serversPath, patched, nil)
		}
	}
	return cleanup, nil
}

// writeRenewedCertToStorage atomically writes the renewed certificate and private key
// into Caddy's FileStorage layout under <data>/caddy/certificates/<issuer>/<domain>/.
func (c *Caddy) writeRenewedCertToStorage(domain string, certPEM, keyPEM []byte) error {
	certsRoot := filepath.Join(c.dataDir(), "caddy", "certificates")
	targetDir := ""
	if entries, err := os.ReadDir(certsRoot); err == nil {
		for _, e := range entries {
			if !e.IsDir() {
				continue
			}
			cand := filepath.Join(certsRoot, e.Name(), domain)
			if st, err := os.Stat(filepath.Join(cand, domain+".crt")); err == nil && !st.IsDir() {
				targetDir = cand
				break
			}
		}
	}
	if targetDir == "" {
		issuerName := "acme-v02.api.letsencrypt.org-directory"
		if u, err := url.Parse(c.ACMEDirectory); err == nil && u.Host != "" {
			clean := strings.Trim(u.Host+u.Path, "/")
			clean = strings.NewReplacer("/", "-", ":", "-").Replace(clean)
			if clean != "" {
				issuerName = clean
			}
		}
		targetDir = filepath.Join(certsRoot, issuerName, domain)
	}
	if err := os.MkdirAll(targetDir, 0o700); err != nil {
		return err
	}
	if err := writeFileAtomic(filepath.Join(targetDir, domain+".crt"), certPEM, 0o644); err != nil {
		return err
	}
	if err := writeFileAtomic(filepath.Join(targetDir, domain+".key"), keyPEM, 0o600); err != nil {
		return err
	}
	return nil
}

func writeFileAtomic(dst string, data []byte, mode os.FileMode) error {
	tmp, err := os.CreateTemp(filepath.Dir(dst), ".dockwell-cert-*")
	if err != nil {
		return err
	}
	tmpName := tmp.Name()
	cleanup := true
	defer func() {
		if cleanup {
			_ = os.Remove(tmpName)
		}
	}()
	if _, err := tmp.Write(data); err != nil {
		_ = tmp.Close()
		return err
	}
	if err := tmp.Chmod(mode); err != nil {
		_ = tmp.Close()
		return err
	}
	if err := tmp.Sync(); err != nil {
		_ = tmp.Close()
		return err
	}
	if err := tmp.Close(); err != nil {
		return err
	}
	if err := os.Rename(tmpName, dst); err != nil {
		return err
	}
	cleanup = false
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
	validHostRE            = lazyre.Regexp(`^([a-z0-9]([a-z0-9-]{0,61}[a-z0-9])?\.)*[a-z0-9]([a-z0-9-]{0,61}[a-z0-9])?$`)
)

// ForValidation substitutes Dockwell's Basic Auth env-var placeholders with a syntactically
// valid dummy bcrypt hash for dry-run adaptation on Caddy's /adapt endpoint.
func ForValidation(config string) string {
	return basicAuthPlaceholderRE().ReplaceAllStringFunc(config, func(string) string {
		return dummyValidationHash
	})
}

// ValidateCaddyfile adapts caddyfile with Caddy's own Caddyfile adapter and verifies
// the resulting JSON against the site-block allowlist.
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

// Allowlists for website adapted JSON (single source of truth shared with internal/website).
var (
	allowedTopKeys = map[string]bool{
		"admin":   true,
		"logging": true,
		"apps":    true,
	}
	allowedServerKeys = map[string]bool{
		"listen":          true,
		"routes":          true,
		"automatic_https": true,
		"trusted_proxies": true,
		"logs":            true,
		"errors":          true,
	}
	allowedMatchers = map[string]bool{
		"path":          true,
		"path_regexp":   true,
		"method":        true,
		"header":        true,
		"header_regexp": true,
		"query":         true,
		"protocol":      true,
		"remote_ip":     true,
		"client_ip":     true,
		"vars":          true,
		"vars_regexp":   true,
		"file":          true,
		"not":           true,
	}
	allowedHandlers = map[string]bool{
		"subroute":              true,
		"vars":                  true,
		"headers":               true,
		"static_response":       true,
		"rewrite":               true,
		"encode":                true,
		"request_body":          true,
		"authentication":        true,
		"file_server":           true,
		"reverse_proxy":         true,
		"error":                 true,
		"copy_response":         true,
		"copy_response_headers": true,
		"intercept":             true,
	}
)

// ValidateAdaptedCaddyJSON checks the canonical JSON produced by Caddy's Caddyfile adapter
// against a strict allowlist of top-level sections, server options, matchers and handlers.
func ValidateAdaptedCaddyJSON(adapted []byte, allowedRoot string) error {
	var top map[string]any
	if err := json.Unmarshal(adapted, &top); err != nil {
		return fmt.Errorf("adapted Caddy config isn't valid JSON: %w", err)
	}
	for k := range top {
		if !allowedTopKeys[k] {
			return fmt.Errorf("custom Caddy config can't set top-level %q; only HTTP directives for this website are allowed", k)
		}
	}
	if err := checkForbiddenPlaceholders(top); err != nil {
		return err
	}
	admin, _ := top["admin"].(map[string]any)
	if disabled, _ := admin["disabled"].(bool); !disabled {
		return errors.New("adapted config must keep Caddy's admin API disabled")
	}
	if listen, _ := admin["listen"].(string); listen != "" {
		return errors.New("adapted config can't open a Caddy admin listener")
	}
	for k := range admin {
		if k != "disabled" && k != "config" {
			return fmt.Errorf("custom Caddy config can't configure admin.%s", k)
		}
	}
	if rawLog, hasLog := top["logging"]; hasLog {
		if err := checkAdaptedLogging(rawLog); err != nil {
			return err
		}
	}
	apps, _ := top["apps"].(map[string]any)
	for appName := range apps {
		if appName != "http" {
			return fmt.Errorf("custom Caddy config can't configure Caddy's %q app; only HTTP directives for this website are allowed", appName)
		}
	}
	httpApp, _ := apps["http"].(map[string]any)
	for k := range httpApp {
		if k != "servers" {
			return fmt.Errorf("custom Caddy config can't set apps.http.%s", k)
		}
	}
	servers, _ := httpApp["servers"].(map[string]any)
	if len(servers) != 1 {
		return fmt.Errorf("custom Caddy config must stay inside this website's single server block (got %d servers)", len(servers))
	}
	for _, rawSrv := range servers {
		srv, _ := rawSrv.(map[string]any)
		for k := range srv {
			if !allowedServerKeys[k] {
				return fmt.Errorf("custom Caddy config can't set server option %q", k)
			}
		}
		listen, _ := srv["listen"].([]any)
		if len(listen) != 1 || listen[0] != ":8080" {
			return fmt.Errorf("custom Caddy config must listen only on :8080 (got %v)", listen)
		}
		if rawAuto, ok := srv["automatic_https"]; ok {
			auto, _ := rawAuto.(map[string]any)
			if disable, _ := auto["disable"].(bool); !disable || len(auto) != 1 {
				return errors.New("custom Caddy config must keep automatic_https disabled")
			}
		}
		if rawTP, ok := srv["trusted_proxies"]; ok {
			tp, _ := rawTP.(map[string]any)
			if src, _ := tp["source"].(string); src != "static" {
				return errors.New("custom Caddy config can't override trusted_proxies")
			}
		}
		if rawLogs, ok := srv["logs"]; ok {
			if err := checkAdaptedServerLogs(rawLogs); err != nil {
				return err
			}
		}
		if rawErrs, ok := srv["errors"]; ok {
			errsMap, _ := rawErrs.(map[string]any)
			for k := range errsMap {
				if k != "routes" {
					return fmt.Errorf("custom Caddy config can't set errors.%s", k)
				}
			}
			if routes, ok := errsMap["routes"].([]any); ok {
				if err := checkAdaptedRoutes(routes, allowedRoot); err != nil {
					return err
				}
			}
		}
		if routes, ok := srv["routes"].([]any); ok {
			if err := checkAdaptedRoutes(routes, allowedRoot); err != nil {
				return err
			}
		}
	}
	return nil
}

// checkForbiddenPlaceholders recursively checks every string in the adapted JSON
// so environment, system, and file placeholders cannot be introduced anywhere.
func checkForbiddenPlaceholders(v any) error {
	switch x := v.(type) {
	case string:
		lower := strings.ToLower(x)
		if strings.Contains(lower, "{$") || strings.Contains(lower, "{env.") || strings.Contains(lower, "{system.") || strings.Contains(lower, "{file.") {
			return errors.New("custom Caddy config can't use environment, system or file placeholders")
		}
	case map[string]any:
		for k, val := range x {
			if err := checkForbiddenPlaceholders(k); err != nil {
				return err
			}
			if err := checkForbiddenPlaceholders(val); err != nil {
				return err
			}
		}
	case []any:
		for _, item := range x {
			if err := checkForbiddenPlaceholders(item); err != nil {
				return err
			}
		}
	}
	return nil
}

// checkAdaptedLogging allows only Dockwell's own stderr/console access logger.
func checkAdaptedLogging(raw any) error {
	m, ok := raw.(map[string]any)
	if !ok {
		return errors.New("custom Caddy config can't configure logging")
	}
	for k := range m {
		if k != "logs" {
			return fmt.Errorf("custom Caddy config can't configure logging.%s", k)
		}
	}
	logs, _ := m["logs"].(map[string]any)
	for name, rawEntry := range logs {
		entry, _ := rawEntry.(map[string]any)
		switch name {
		case "default":
			for ek := range entry {
				if ek != "exclude" {
					return fmt.Errorf("custom Caddy config can't configure logging.logs.default.%s", ek)
				}
			}
		case "log0":
			for ek := range entry {
				if ek != "writer" && ek != "encoder" && ek != "include" {
					return fmt.Errorf("custom Caddy config can't configure logging.logs.log0.%s", ek)
				}
			}
			writer, _ := entry["writer"].(map[string]any)
			if out, _ := writer["output"].(string); out != "stderr" || len(writer) != 1 {
				return errors.New("custom Caddy config can't change log output from stderr")
			}
			enc, _ := entry["encoder"].(map[string]any)
			if enc != nil {
				if f, _ := enc["format"].(string); f != "console" || len(enc) != 1 {
					return errors.New("custom Caddy config can't change log encoder")
				}
			}
		default:
			return fmt.Errorf("custom Caddy config can't define custom logger %q", name)
		}
	}
	return nil
}

func checkAdaptedServerLogs(raw any) error {
	m, ok := raw.(map[string]any)
	if !ok {
		return errors.New("custom Caddy config can't configure server logs")
	}
	for k, v := range m {
		if k != "default_logger_name" || v != "log0" {
			return fmt.Errorf("custom Caddy config can't configure server logs.%s", k)
		}
	}
	return nil
}

func checkAdaptedRoutes(routes []any, allowedRoot string) error {
	for _, item := range routes {
		rt, ok := item.(map[string]any)
		if !ok {
			return errors.New("adapted Caddy route isn't an object")
		}
		for k := range rt {
			switch k {
			case "match", "handle", "terminal", "group":
			default:
				return fmt.Errorf("custom Caddy route option %q isn't allowed", k)
			}
		}
		if matchers, ok := rt["match"].([]any); ok {
			if err := checkAdaptedMatchers(matchers, allowedRoot); err != nil {
				return err
			}
		}
		if handlers, ok := rt["handle"].([]any); ok {
			for _, hItem := range handlers {
				h, ok := hItem.(map[string]any)
				if !ok {
					return errors.New("adapted Caddy handler isn't an object")
				}
				if err := checkAdaptedHandler(h, allowedRoot); err != nil {
					return err
				}
			}
		}
	}
	return nil
}

func checkAdaptedMatchers(matchers []any, allowedRoot string) error {
	for _, mItem := range matchers {
		m, ok := mItem.(map[string]any)
		if !ok {
			return errors.New("adapted Caddy matcher isn't an object")
		}
		for k, v := range m {
			if !allowedMatchers[k] {
				if k == "host" || k == "host_regexp" {
					return errors.New("custom Caddy config can't match another site's hostname")
				}
				return fmt.Errorf("custom Caddy matcher %q isn't allowed; config is scoped to this website alone", k)
			}
			switch k {
			case "not":
				sub, ok := v.([]any)
				if !ok {
					return errors.New("invalid 'not' matcher in adapted Caddy config")
				}
				if err := checkAdaptedMatchers(sub, allowedRoot); err != nil {
					return err
				}
			case "file":
				if allowedRoot == "" {
					return errors.New("file matcher isn't allowed on a proxy website that mounts no folder")
				}
				if fm, ok := v.(map[string]any); ok {
					if rootVal, ok := fm["root"].(string); ok && rootVal != "" {
						if err := checkAdaptedRoot("file matcher", rootVal, allowedRoot); err != nil {
							return err
						}
					}
				}
			}
		}
	}
	return nil
}

func checkAdaptedHandler(h map[string]any, allowedRoot string) error {
	name, _ := h["handler"].(string)
	if !allowedHandlers[name] {
		return fmt.Errorf("custom Caddy handler %q isn't allowed in custom site config", name)
	}
	switch name {
	case "vars":
		if rootVal, exists := h["root"]; exists {
			s, ok := rootVal.(string)
			if !ok {
				return errors.New("custom Caddy root must be a static path string")
			}
			if s != "" {
				if err := checkAdaptedRoot("custom Caddy", s, allowedRoot); err != nil {
					return err
				}
			}
		}
	case "file_server":
		if allowedRoot == "" {
			return errors.New("file_server isn't allowed on a proxy website that mounts no folder")
		}
		if rootVal, exists := h["root"]; exists {
			s, ok := rootVal.(string)
			if !ok {
				return errors.New("file_server root must be a static path string")
			}
			if s != "" {
				if err := checkAdaptedRoot("file_server", s, allowedRoot); err != nil {
					return err
				}
			}
		}
	case "authentication":
		if providers, ok := h["providers"].(map[string]any); ok {
			for p := range providers {
				if p != "http_basic" {
					return fmt.Errorf("authentication provider %q isn't allowed in custom site config", p)
				}
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
		if rawTr, hasTr := h["transport"]; hasTr {
			tr, ok := rawTr.(map[string]any)
			if !ok {
				return errors.New("invalid reverse_proxy transport in adapted Caddy config")
			}
			proto, _ := tr["protocol"].(string)
			if proto != "" && proto != "http" && proto != "fastcgi" {
				return fmt.Errorf("reverse_proxy transport %q isn't allowed in custom site config", proto)
			}
			if rootVal, exists := tr["root"]; exists {
				s, ok := rootVal.(string)
				if !ok {
					return errors.New("fastcgi transport root must be a static path string")
				}
				if s != "" {
					if err := checkAdaptedRoot("fastcgi transport", s, allowedRoot); err != nil {
						return err
					}
				}
			}
		}
	}
	// Generically walk every nested routes / handle / match / errors subtree
	// (covers subroute.routes, reverse_proxy.handle_response[].routes,
	// intercept.handle_response[].routes, etc. without naming each handler).
	return walkNestedRoutes(h, allowedRoot, true)
}

func walkNestedRoutes(v any, allowedRoot string, isRootHandler bool) error {
	switch x := v.(type) {
	case map[string]any:
		for k, val := range x {
			switch k {
			case "routes":
				if routes, ok := val.([]any); ok {
					if err := checkAdaptedRoutes(routes, allowedRoot); err != nil {
						return err
					}
					continue
				}
			case "handle":
				if !isRootHandler {
					if handlers, ok := val.([]any); ok {
						for _, hItem := range handlers {
							hm, ok := hItem.(map[string]any)
							if !ok {
								return errors.New("adapted Caddy handler isn't an object")
							}
							if err := checkAdaptedHandler(hm, allowedRoot); err != nil {
								return err
							}
						}
						continue
					}
				}
			case "match":
				if !isRootHandler {
					if matchers, ok := val.([]any); ok {
						if err := checkAdaptedMatchers(matchers, allowedRoot); err != nil {
							return err
						}
						continue
					}
				}
			}
			if err := walkNestedRoutes(val, allowedRoot, false); err != nil {
				return err
			}
		}
	case []any:
		for _, item := range x {
			if err := walkNestedRoutes(item, allowedRoot, false); err != nil {
				return err
			}
		}
	}
	return nil
}

func checkAdaptedRoot(what, rootVal, allowedRoot string) error {
	if strings.ContainsAny(rootVal, "{}") {
		return fmt.Errorf("%s root %q can't contain Caddy placeholders", what, rootVal)
	}
	if !pathWithin(rootVal, allowedRoot) {
		return fmt.Errorf("%s root %q must stay inside %s", what, rootVal, cmp.Or(allowedRoot, "the website's folder"))
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
	if strings.ContainsAny(dial, "{}") {
		return fmt.Errorf("custom reverse_proxy upstream %q can't contain Caddy placeholders", dial)
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
		if len(host) > 253 || !validHostRE().MatchString(host) {
			return fmt.Errorf("custom reverse_proxy upstream %q isn't a valid hostname", dial)
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
