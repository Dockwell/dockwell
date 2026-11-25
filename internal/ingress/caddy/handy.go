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
	"math/big"
	"net"
	"net/http"
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
	"github.com/dockwell/dockwell/internal/netguard"
)

const (
	rateLimitPathPrefix = "/ratelimit/"
	maxKeyFile          = 128 << 10
	// forceRenewTimeout bounds an in-place forced ACME HTTP-01 renewal.
	forceRenewTimeout = 30 * time.Second
	// renewedCertTag marks temporary bridge certificates swapped in place by RenewCertificate(force=true).
	renewedCertTag = "dockwell.renewed"
	// renewedPinDropBefore drops a temporary forced-renewal bridge pin once it enters the
	// renewal window so Caddy serves and renews its own managed certificate from storage (N1).
	renewedPinDropBefore = 30 * 24 * time.Hour
	// certMagicLockStale matches CertMagic's stale lock file age.
	certMagicLockStale = 2 * time.Hour
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
	Serial    *big.Int
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
	var serial *big.Int
	if leaf.SerialNumber != nil {
		serial = new(big.Int).Set(leaf.SerialNumber)
	}
	return certMeta{
		Serial:    serial,
		NotBefore: leaf.NotBefore,
		NotAfter:  leaf.NotAfter,
		Issuer:    issuer,
		Subject:   subject,
		SANs:      slices.Clone(leaf.DNSNames),
	}
}

func (c *Caddy) now() time.Time {
	if c.nowForTest != nil {
		return c.nowForTest()
	}
	return time.Now()
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
// it obtains a fresh certificate from the configured ACME CA via HTTP-01 using a
// reused per-CA ACME account (kept sealed as a secret), writes key, certificate,
// and CertMagic metadata together under CertMagic's storage lock without following
// symlinks (N4), and installs a temporary domain-scoped TLS bridge pin so Caddy
// serves the new certificate immediately without a restart. The pin is dropped
// automatically on the watch tick once Caddy renews the certificate or it enters
// its renewal window, and immediately if the route is deleted or switched to a
// custom or DNS-01 certificate (N1).
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

	issued, err := c.obtainForcedACMECert(renewCtx, domain)
	if err != nil {
		if hadPrev {
			return "", false, fmt.Errorf("forced renewal for %s failed (%w); the existing valid certificate remains in place", domain, err)
		}
		return "", false, fmt.Errorf("forced renewal for %s failed: %w", domain, err)
	}

	// Write key, cert, and CertMagic JSON metadata together under CertMagic's storage
	// lock using os.OpenRoot so Caddy owns and renews it as its own managed cert (N4).
	if err := c.writeRenewedCertToStorage(renewCtx, domain, issued); err != nil {
		return "", false, fmt.Errorf("obtained new certificate for %s, but couldn't persist it: %w", domain, err)
	}

	// Caddy's in-memory CertMagic cache survives admin reloads without restarting, so
	// install a temporary bridge pin for immediate serving; PruneRenewedCerts,
	// SetCertificates and DeleteRoute drop it automatically (N1).
	domainTag := renewedCertTag + "." + domain
	if _, err := c.editLoaded(renewCtx, func(loaded []map[string]any) []map[string]any {
		loaded = slices.DeleteFunc(loaded, func(cert map[string]any) bool { return tagged(cert, domainTag) })
		return append(loaded, map[string]any{
			"certificate": string(issued.certPEM),
			"key":         string(issued.keyPEM),
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

// renewedTagDomain returns the domain name encoded in a renewedCertTag.<domain> tag,
// or "" if cert is not a temporary forced-renewal bridge certificate.
func renewedTagDomain(cert map[string]any) string {
	tags, _ := cert["tags"].([]any)
	prefix := renewedCertTag + "."
	for _, raw := range tags {
		if s, ok := raw.(string); ok {
			if dom, found := strings.CutPrefix(s, prefix); found && dom != "" {
				return strings.ToLower(dom)
			}
		}
	}
	return ""
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
	for _, entry := range existing {
		m, _ := entry.(map[string]any)
		if len(m) == 0 {
			continue
		}
		if match, _ := m["match"].(map[string]any); match != nil {
			if snis, _ := match["sni"].([]any); len(snis) == 1 && snis[0] == domain {
				continue
			}
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
	policies = append(policies, map[string]any{})
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

// dropRenewedPolicies removes domain-scoped forced-renewal TLS connection policies
// for any domain in domains, deleting tls_connection_policies entirely when none remain.
func (c *Caddy) dropRenewedPolicies(ctx context.Context, domains map[string]bool) (bool, error) {
	if len(domains) == 0 {
		return false, nil
	}
	servers, _, up, err := c.readServers(ctx)
	if err != nil || !up || servers == nil {
		return false, err
	}
	raw, ok := servers[serverName]
	if !ok {
		return false, nil
	}
	var srv map[string]any
	if err := json.Unmarshal(raw, &srv); err != nil {
		return false, err
	}
	existing, ok := srv["tls_connection_policies"].([]any)
	if !ok || len(existing) == 0 {
		return false, nil
	}
	var kept []any
	removed := false
	for _, entry := range existing {
		m, _ := entry.(map[string]any)
		if len(m) == 0 {
			continue
		}
		if dom := policyRenewedDomain(m); dom != "" && domains[dom] {
			removed = true
			continue
		}
		kept = append(kept, m)
	}
	if !removed {
		return false, nil
	}
	if len(kept) == 0 {
		delete(srv, "tls_connection_policies")
	} else {
		srv["tls_connection_policies"] = append(kept, map[string]any{})
	}
	updated, err := json.Marshal(srv)
	if err != nil {
		return false, err
	}
	servers[serverName] = updated
	body, err := json.Marshal(servers)
	if err != nil {
		return false, err
	}
	_, _, err = c.call(ctx, http.MethodPatch, serversPath, body, nil)
	return err == nil, err
}

func policyRenewedDomain(policy map[string]any) string {
	sel, _ := policy["certificate_selection"].(map[string]any)
	if sel == nil {
		return ""
	}
	tags, _ := sel["any_tag"].([]any)
	prefix := renewedCertTag + "."
	for _, raw := range tags {
		if s, ok := raw.(string); ok {
			if dom, found := strings.CutPrefix(s, prefix); found && dom != "" {
				return strings.ToLower(dom)
			}
		}
	}
	return ""
}

// PruneRenewedCerts removes temporary forced-renewal bridge pins (both load_pem and
// tls_connection_policies) whenever the route is gone, a custom or DNS-01 certificate
// skips automatic HTTPS for that domain, Caddy's own storage holds a newer renewed
// certificate, or the pinned certificate has entered its renewal window (N1).
func (c *Caddy) PruneRenewedCerts(ctx context.Context, activeHTTPS []string) error {
	servers, _, up, err := c.readServers(ctx)
	if err != nil || !up || servers == nil {
		return err
	}
	activeSet := make(map[string]bool, len(activeHTTPS))
	for _, d := range activeHTTPS {
		activeSet[strings.ToLower(strings.TrimSpace(d))] = true
	}
	skipSet := map[string]bool{}
	var policyDomains []string
	if raw, ok := servers[serverName]; ok {
		var srv struct {
			Auto struct {
				Skip []string `json:"skip_certificates"`
			} `json:"automatic_https"`
			Policies []map[string]any `json:"tls_connection_policies"`
		}
		if json.Unmarshal(raw, &srv) == nil {
			for _, s := range srv.Auto.Skip {
				skipSet[strings.ToLower(strings.TrimSpace(s))] = true
			}
			for _, p := range srv.Policies {
				if dom := policyRenewedDomain(p); dom != "" {
					policyDomains = append(policyDomains, dom)
				}
			}
		}
	}

	now := c.now()
	dropDomains := map[string]bool{}
	keptDomains := map[string]bool{}
	loadedChanged, err := c.editLoaded(ctx, func(loaded []map[string]any) []map[string]any {
		return slices.DeleteFunc(loaded, func(cert map[string]any) bool {
			if !tagged(cert, renewedCertTag) {
				return false
			}
			dom := renewedTagDomain(cert)
			if dom == "" {
				return true
			}
			if !activeSet[dom] || skipSet[dom] {
				dropDomains[dom] = true
				return true
			}
			pemStr, _ := cert["certificate"].(string)
			leaf := parseFirstCert([]byte(pemStr))
			if leaf == nil {
				dropDomains[dom] = true
				return true
			}
			if shouldDropRenewedPin(leaf, now, c.readIssuedCert) {
				dropDomains[dom] = true
				return true
			}
			keptDomains[dom] = true
			return false
		})
	})
	if err != nil {
		return err
	}
	// Also clean up any orphaned renewed policy whose load_pem entry is already gone.
	for _, dom := range policyDomains {
		if !keptDomains[dom] {
			dropDomains[dom] = true
		}
	}
	policyChanged := false
	if len(dropDomains) > 0 {
		if changed, perr := c.dropRenewedPolicies(ctx, dropDomains); perr != nil {
			return perr
		} else {
			policyChanged = changed
		}
	}
	if loadedChanged || policyChanged {
		c.keepRunning(ctx)
	}
	return nil
}

func shouldDropRenewedPin(pinned *x509.Certificate, now time.Time, readStored func(string, bool) (certMeta, bool)) bool {
	if !pinned.NotAfter.After(now) {
		return true
	}
	remaining := pinned.NotAfter.Sub(now)
	if remaining <= renewedPinDropBefore {
		return true
	}
	if lifetime := pinned.NotAfter.Sub(pinned.NotBefore); lifetime > 0 && remaining <= lifetime/3 {
		return true
	}
	// Drop once Caddy's own ACME issuer has written a newer certificate to storage.
	var domain string
	if len(pinned.DNSNames) > 0 {
		domain = strings.ToLower(pinned.DNSNames[0])
	} else {
		domain = strings.ToLower(pinned.Subject.CommonName)
	}
	if domain != "" && readStored != nil {
		if stored, ok := readStored(domain, false); ok && stored.NotAfter.After(now) {
			if stored.Serial != nil && pinned.SerialNumber != nil && stored.Serial.Cmp(pinned.SerialNumber) != 0 && !stored.NotAfter.Before(pinned.NotAfter) {
				return true
			}
		}
	}
	return false
}

func parseFirstCert(pemBytes []byte) *x509.Certificate {
	block, _ := pem.Decode(pemBytes)
	if block == nil {
		return nil
	}
	leaf, err := x509.ParseCertificate(block.Bytes)
	if err != nil {
		return nil
	}
	return leaf
}

type forcedACMECert struct {
	certPEM    []byte
	keyPEM     []byte
	certURL    string
	caURL      string
	accountURL string
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

// acmeAccountKey returns a reusable P-256 ACME account key for caURL, loading and
// saving it sealed via ACMEAccounts when configured, so forced renewals reuse one
// ACME account per CA rather than registering a fresh account on every call (M3, N5).
func (c *Caddy) acmeAccountKey(caURL string) (*ecdsa.PrivateKey, error) {
	c.acmeMu.Lock()
	defer c.acmeMu.Unlock()
	if c.acmeAccounts == nil {
		c.acmeAccounts = map[string]*ecdsa.PrivateKey{}
	}
	if !c.acmeLoaded && c.ACMEAccounts != nil {
		c.acmeLoaded = true
		if plain, err := c.ACMEAccounts.KeptACMEAccounts(); err == nil && len(plain) > 0 {
			defer clear(plain)
			var saved map[string]string
			if json.Unmarshal(plain, &saved) == nil {
				for u, pemStr := range saved {
					if blk, _ := pem.Decode([]byte(pemStr)); blk != nil {
						if k, kerr := x509.ParseECPrivateKey(blk.Bytes); kerr == nil {
							c.acmeAccounts[u] = k
						}
					}
				}
			}
		}
	}
	if existing, ok := c.acmeAccounts[caURL]; ok && existing != nil {
		return existing, nil
	}
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		return nil, fmt.Errorf("generate ACME account key: %w", err)
	}
	c.acmeAccounts[caURL] = key
	if c.ACMEAccounts != nil {
		out := make(map[string]string, len(c.acmeAccounts))
		for u, k := range c.acmeAccounts {
			der, merr := x509.MarshalECPrivateKey(k)
			if merr != nil {
				return nil, fmt.Errorf("marshal ACME account key: %w", merr)
			}
			out[u] = string(pem.EncodeToMemory(&pem.Block{Type: "EC PRIVATE KEY", Bytes: der}))
			clear(der)
		}
		raw, merr := json.Marshal(out)
		if merr != nil {
			return nil, fmt.Errorf("encode ACME accounts: %w", merr)
		}
		defer clear(raw)
		if serr := c.ACMEAccounts.KeepACMEAccounts(raw); serr != nil {
			return nil, fmt.Errorf("seal ACME account key: %w", serr)
		}
	}
	return key, nil
}

func (c *Caddy) obtainForcedACMECert(ctx context.Context, domain string) (forcedACMECert, error) {
	httpClient, err := c.acmeHTTPClient()
	if err != nil {
		return forcedACMECert{}, err
	}
	caURL := cmp.Or(c.ACMEDirectory, acme.LetsEncryptURL)
	acctKey, err := c.acmeAccountKey(caURL)
	if err != nil {
		return forcedACMECert{}, err
	}
	client := &acme.Client{
		Key:          acctKey,
		DirectoryURL: caURL,
		HTTPClient:   httpClient,
	}
	acct, err := client.Register(ctx, &acme.Account{}, acme.AcceptTOS)
	if errors.Is(err, acme.ErrAccountAlreadyExists) {
		acct, err = client.GetReg(ctx, "")
	}
	if err != nil {
		return forcedACMECert{}, fmt.Errorf("register ACME account: %w", err)
	}
	order, err := client.AuthorizeOrder(ctx, acme.DomainIDs(domain))
	if err != nil {
		return forcedACMECert{}, fmt.Errorf("authorize order: %w", err)
	}
	for _, authzURL := range order.AuthzURLs {
		authz, err := client.GetAuthorization(ctx, authzURL)
		if err != nil {
			return forcedACMECert{}, fmt.Errorf("read authorization: %w", err)
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
			return forcedACMECert{}, errors.New("CA offered no HTTP-01 challenge")
		}
		keyAuth, err := client.HTTP01ChallengeResponse(chal.Token)
		if err != nil {
			return forcedACMECert{}, fmt.Errorf("build HTTP-01 response: %w", err)
		}
		cleanup, err := c.installHTTP01ChallengeRoute(ctx, domain, chal.Token, keyAuth)
		if err != nil {
			return forcedACMECert{}, fmt.Errorf("install HTTP-01 challenge route: %w", err)
		}
		_, acceptErr := client.Accept(ctx, chal)
		var waitErr error
		if acceptErr == nil {
			_, waitErr = client.WaitAuthorization(ctx, authzURL)
		}
		cleanup()
		if acceptErr != nil {
			return forcedACMECert{}, fmt.Errorf("accept HTTP-01 challenge: %w", acceptErr)
		}
		if waitErr != nil {
			return forcedACMECert{}, fmt.Errorf("HTTP-01 validation failed: %w", waitErr)
		}
	}
	order, err = client.WaitOrder(ctx, order.URI)
	if err != nil {
		return forcedACMECert{}, fmt.Errorf("wait for ACME order: %w", err)
	}

	leafKey, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		return forcedACMECert{}, fmt.Errorf("generate certificate key: %w", err)
	}
	csr, err := x509.CreateCertificateRequest(rand.Reader, &x509.CertificateRequest{
		Subject:  pkix.Name{CommonName: domain},
		DNSNames: []string{domain},
	}, leafKey)
	if err != nil {
		return forcedACMECert{}, fmt.Errorf("create CSR: %w", err)
	}
	ders, certURL, err := client.CreateCertFromOrder(ctx, order, csr, true)
	if err != nil {
		return forcedACMECert{}, fmt.Errorf("finalize certificate order: %w", err)
	}
	if len(ders) == 0 {
		return forcedACMECert{}, errors.New("CA returned an empty certificate chain")
	}
	leaf, err := x509.ParseCertificate(ders[0])
	if err != nil {
		return forcedACMECert{}, fmt.Errorf("parse issued certificate: %w", err)
	}
	if err := leaf.VerifyHostname(domain); err != nil {
		return forcedACMECert{}, fmt.Errorf("issued certificate does not match %s: %w", domain, err)
	}
	var certBuf strings.Builder
	for _, der := range ders {
		certBuf.Write(pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: der}))
	}
	keyDER, err := x509.MarshalECPrivateKey(leafKey)
	if err != nil {
		return forcedACMECert{}, fmt.Errorf("marshal certificate key: %w", err)
	}
	keyPEM := pem.EncodeToMemory(&pem.Block{Type: "EC PRIVATE KEY", Bytes: keyDER})
	clear(keyDER)
	var accountURL string
	if acct != nil {
		accountURL = acct.URI
	}
	return forcedACMECert{
		certPEM:    []byte(certBuf.String()),
		keyPEM:     keyPEM,
		certURL:    certURL,
		caURL:      caURL,
		accountURL: accountURL,
	}, nil
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

// acquireCertMagicLock takes CertMagic's FileStorage lock for domain inside root
// without following symlinks, preventing concurrent writes with Caddy's own ACME
// renewal loop (N4).
func acquireCertMagicLock(ctx context.Context, root *os.Root, domain string) (func(), error) {
	const locksDir = "caddy/locks"
	if err := mkdirAllOwned(root, locksDir, 0o700, proxyOwns()); err != nil {
		return nil, fmt.Errorf("create CertMagic lock dir: %w", err)
	}
	lockRel := filepath.Join(locksDir, "issue_cert_"+domain+".lock")
	who := proxyOwns()
	for {
		f, err := root.OpenFile(lockRel, os.O_WRONLY|os.O_CREATE|os.O_EXCL|syscall.O_NOFOLLOW, 0o600)
		if err == nil {
			if who.uid >= 0 {
				_ = f.Chown(who.uid, who.gid)
			}
			_, _ = f.WriteString(time.Now().UTC().Format(time.RFC3339))
			_ = f.Close()
			return func() { _ = root.Remove(lockRel) }, nil
		}
		if !errors.Is(err, os.ErrExist) {
			return nil, fmt.Errorf("acquire CertMagic lock %s: %w", lockRel, err)
		}
		// Break a stale lock older than CertMagic's stale threshold if it's a regular file.
		if st, serr := root.Lstat(lockRel); serr == nil {
			if !st.Mode().IsRegular() {
				return nil, fmt.Errorf("CertMagic lock %s %w", lockRel, errNotRegular)
			}
			if time.Since(st.ModTime()) > certMagicLockStale {
				_ = root.Remove(lockRel)
				continue
			}
		}
		select {
		case <-ctx.Done():
			return nil, fmt.Errorf("timed out waiting for CertMagic storage lock on %s: %w", domain, ctx.Err())
		case <-time.After(150 * time.Millisecond):
		}
	}
}

// writeRenewedCertToStorage writes key, certificate, and CertMagic JSON metadata
// together under CertMagic's storage lock inside os.OpenRoot(c.dataDir()), never
// following symlinks (N4). Key is written first, then cert, then metadata JSON.
func (c *Caddy) writeRenewedCertToStorage(ctx context.Context, domain string, issued forcedACMECert) error {
	if err := ingress.CheckDomainName(domain); err != nil {
		return err
	}
	root, err := os.OpenRoot(c.dataDir())
	if err != nil {
		return err
	}
	defer root.Close()

	unlock, err := acquireCertMagicLock(ctx, root, domain)
	if err != nil {
		return err
	}
	defer unlock()

	const certificates = "caddy/certificates"
	targetRelDir := ""
	if dir, err := root.OpenFile(certificates, os.O_RDONLY|syscall.O_NONBLOCK|syscall.O_DIRECTORY, 0); err == nil {
		entries, rerr := dir.ReadDir(issuerDirs)
		_ = dir.Close()
		if rerr == nil {
			for _, e := range entries {
				if !e.IsDir() {
					continue
				}
				candRel := filepath.Join(certificates, e.Name(), domain)
				if f, ferr := openRegular(root, filepath.Join(candRel, domain+".crt")); ferr == nil {
					_ = f.Close()
					targetRelDir = candRel
					break
				}
			}
		}
	}
	if targetRelDir == "" {
		issuerName := "acme-v02.api.letsencrypt.org-directory"
		if u, err := url.Parse(issued.caURL); err == nil && u.Host != "" {
			clean := strings.Trim(u.Host+u.Path, "/")
			clean = strings.NewReplacer("/", "-", ":", "-").Replace(clean)
			if clean != "" {
				issuerName = clean
			}
		}
		targetRelDir = filepath.Join(certificates, issuerName, domain)
	}

	// Refuse any symlink along the target directory inside root before writing.
	if err := mkdirAllOwned(root, targetRelDir, 0o700, proxyOwns()); err != nil {
		return err
	}
	metaJSON, err := json.MarshalIndent(map[string]any{
		"sans": []string{domain},
		"issuer_data": map[string]any{
			"url":     issued.certURL,
			"ca":      issued.caURL,
			"account": issued.accountURL,
		},
	}, "", "\t")
	if err != nil {
		return err
	}
	who := proxyOwns()
	// Write key first, then certificate, then CertMagic's metadata JSON.
	if err := writeIn(c.dataDir(), filepath.Join(targetRelDir, domain+".key"), issued.keyPEM, 0o600, who); err != nil {
		return err
	}
	if err := writeIn(c.dataDir(), filepath.Join(targetRelDir, domain+".crt"), issued.certPEM, 0o644, who); err != nil {
		return err
	}
	if err := writeIn(c.dataDir(), filepath.Join(targetRelDir, domain+".json"), metaJSON, 0o644, who); err != nil {
		return err
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
	dockerAPIPorts         = []int{2375, 2376}
	validHostRE            = lazyre.Regexp(`^([a-z0-9]([a-z0-9-]{0,61}[a-z0-9])?\.)*[a-z0-9]([a-z0-9-]{0,61}[a-z0-9])?$`)
	subpathAliasRE         = lazyre.Regexp(`^subpath-[0-9]+$`)
	regexQuantifierRE      = lazyre.Regexp(`^[0-9]+(,[0-9]*)?$`)
	headerPlaceholderRE    = lazyre.Regexp(`^http\.request\.header\.[A-Za-z0-9-]+$`)
)

// ForValidation substitutes Dockwell's Basic Auth env-var placeholders with a syntactically
// valid dummy bcrypt hash for dry-run adaptation on Caddy's /adapt endpoint.
func ForValidation(config string) string {
	return basicAuthPlaceholderRE().ReplaceAllStringFunc(config, func(string) string {
		return dummyValidationHash
	})
}

// ValidateCaddyfile adapts caddyfile with Caddy's own Caddyfile adapter and verifies
// the resulting JSON against the site-block allowlist and outbound upstream guard.
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
	if err := ValidateAdaptedCaddyJSONContext(ctx, adapted, allowedRoot, c.lookupForTest); err != nil {
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
	allowedHandlerKeys = map[string]map[string]bool{
		"subroute":              {"handler": true, "routes": true, "errors": true},
		"vars":                  {"handler": true, "root": true},
		"headers":               {"handler": true, "request": true, "response": true},
		"static_response":       {"handler": true, "status_code": true, "headers": true, "body": true, "close": true, "abort": true},
		"rewrite":               {"handler": true, "method": true, "uri": true, "strip_path_prefix": true, "strip_path_suffix": true, "uri_substring": true, "path_regexp": true},
		"encode":                {"handler": true, "encodings": true, "prefer": true, "minimum_length": true, "match": true},
		"request_body":          {"handler": true, "max_size": true},
		"authentication":        {"handler": true, "providers": true},
		"file_server":           {"handler": true, "root": true, "hide": true, "index_names": true, "canonical_uris": true, "status_code": true, "pass_thru": true, "precompressed": true, "precompressed_order": true},
		"reverse_proxy":         {"handler": true, "upstreams": true, "headers": true, "transport": true, "load_balancing": true, "flush_interval": true, "rewrite": true, "handle_response": true, "stream_timeout": true, "stream_close_delay": true, "trusted_proxies": true},
		"error":                 {"handler": true, "error": true, "status_code": true},
		"copy_response":         {"handler": true, "status_code": true},
		"copy_response_headers": {"handler": true, "include": true, "exclude": true},
		"intercept":             {"handler": true, "handle_response": true},
	}
	allowedHeaderOpKeys = map[string]bool{
		"set":      true,
		"add":      true,
		"delete":   true,
		"replace":  true,
		"deferred": true,
		"require":  true,
	}
	allowedFileMatcherKeys = map[string]bool{
		"root":       true,
		"try_files":  true,
		"try_policy": true,
		"split_path": true,
	}
	allowedEncodings = map[string]bool{
		"gzip": true,
		"zstd": true,
		"br":   true,
	}
	allowedHTTPTransportKeys = map[string]bool{
		"protocol":                true,
		"versions":                true,
		"dial_timeout":            true,
		"read_timeout":            true,
		"write_timeout":           true,
		"response_header_timeout": true,
		"expect_continue_timeout": true,
		"read_buffer_size":        true,
		"write_buffer_size":       true,
		"keep_alive":              true,
		"compression":             true,
		"tls":                     true,
	}
	allowedHTTPTransportTLSKeys = map[string]bool{
		"insecure_skip_verify": true,
		"server_name":          true,
		"renegotiation":        true,
		"except_ports":         true,
		"handshake_timeout":    true,
	}
	allowedFastCGITransportKeys = map[string]bool{
		"protocol":       true,
		"root":           true,
		"split_path":     true,
		"env":            true,
		"dial_timeout":   true,
		"read_timeout":   true,
		"write_timeout":  true,
		"capture_stderr": true,
	}
	// allowedGeneralPlaceholders is the short named list of Caddy placeholders allowed
	// in general site directives (rewrite, file matcher try_files, vars_regexp keys,
	// headers, respond bodies). None can reach files, environment variables, arbitrary
	// vars, or query parameter values.
	allowedGeneralPlaceholders = map[string]bool{
		"http.request.uri":                     true,
		"http.request.uri.path":                true,
		"http.request.uri.prefixed_query":      true,
		"http.request.orig_uri":                true,
		"http.request.orig_uri.path":           true,
		"http.request.orig_uri.prefixed_query": true,
		"http.request.host":                    true,
		"http.request.hostport":                true,
		"http.request.scheme":                  true,
		"http.request.method":                  true,
		"http.matchers.file.relative":          true,
		"http.error.status_code":               true,
		"http.error.status_text":               true,
	}
	// allowedHeaderAndBodyOnlyPlaceholders are permitted ONLY in header values and
	// static_response bodies, never in paths, rewrites, matchers, variables or dials.
	allowedHeaderAndBodyOnlyPlaceholders = map[string]bool{
		"http.request.remote.host":             true,
		"http.request.remote.port":             true,
		"http.vars.client_ip":                  true,
		"http.reverse_proxy.upstream.hostport": true,
	}
)

type placeholderContext int

const (
	phNone placeholderContext = iota
	phRegex
	phGeneral
	phHeaderOrBody
)

type adaptCheck struct {
	ctx         context.Context
	allowedRoot string
	lookup      netguard.LookupFunc
}

// ValidateAdaptedCaddyJSON checks the canonical JSON produced by Caddy's Caddyfile adapter
// against a strict allowlist of top-level sections, server options, matchers, handlers,
// handler keys, placeholders and resolved upstream addresses.
func ValidateAdaptedCaddyJSON(adapted []byte, allowedRoot string) error {
	return ValidateAdaptedCaddyJSONContext(context.Background(), adapted, allowedRoot, nil)
}

// ValidateAdaptedCaddyJSONContext is ValidateAdaptedCaddyJSON with a context and optional
// DNS resolver override for upstream validation.
func ValidateAdaptedCaddyJSONContext(ctx context.Context, adapted []byte, allowedRoot string, lookup netguard.LookupFunc) error {
	var top map[string]any
	if err := json.Unmarshal(adapted, &top); err != nil {
		return fmt.Errorf("adapted Caddy config isn't valid JSON: %w", err)
	}
	for k := range top {
		if !allowedTopKeys[k] {
			return fmt.Errorf("custom Caddy config can't set top-level %q; only HTTP directives for this website are allowed", k)
		}
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
	if err := checkPlaceholdersInTree(admin, phNone); err != nil {
		return err
	}
	if rawLog, hasLog := top["logging"]; hasLog {
		if err := checkAdaptedLogging(rawLog); err != nil {
			return err
		}
		if err := checkPlaceholdersInTree(rawLog, phNone); err != nil {
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
	ac := &adaptCheck{ctx: ctx, allowedRoot: allowedRoot, lookup: lookup}
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
			if err := checkPlaceholdersInTree(rawTP, phNone); err != nil {
				return err
			}
		}
		if rawLogs, ok := srv["logs"]; ok {
			if err := checkAdaptedServerLogs(rawLogs); err != nil {
				return err
			}
			if err := checkPlaceholdersInTree(rawLogs, phNone); err != nil {
				return err
			}
		}
		if rawErrs, ok := srv["errors"]; ok {
			if err := ac.checkAdaptedErrors(rawErrs); err != nil {
				return err
			}
		}
		if routes, ok := srv["routes"].([]any); ok {
			if err := ac.checkAdaptedRoutes(routes); err != nil {
				return err
			}
		}
	}
	return nil
}

func (ac *adaptCheck) checkAdaptedErrors(rawErrs any) error {
	errsMap, ok := rawErrs.(map[string]any)
	if !ok {
		return errors.New("custom Caddy errors config must be an object")
	}
	for k := range errsMap {
		if k != "routes" {
			return fmt.Errorf("custom Caddy config can't set errors.%s", k)
		}
	}
	if routes, ok := errsMap["routes"].([]any); ok {
		return ac.checkAdaptedRoutes(routes)
	}
	return nil
}

// checkPlaceholdersInString enforces the strict placeholder allowlist (M1, N2) on s
// according to the field's context.
func checkPlaceholdersInString(s string, pctx placeholderContext) error {
	if !strings.ContainsAny(s, "{}") {
		return nil
	}
	if pctx == phNone {
		return fmt.Errorf("custom Caddy config can't use placeholders or braces in %q", s)
	}
	i := 0
	for i < len(s) {
		openIdx := strings.IndexByte(s[i:], '{')
		closeBefore := strings.IndexByte(s[i:], '}')
		if closeBefore >= 0 && (openIdx < 0 || closeBefore < openIdx) {
			return fmt.Errorf("custom Caddy config contains unmatched '}' in %q", s)
		}
		if openIdx < 0 {
			break
		}
		start := i + openIdx
		rest := s[start+1:]
		closeRel := strings.IndexByte(rest, '}')
		if closeRel < 0 {
			return fmt.Errorf("custom Caddy config contains unclosed '{' in %q", s)
		}
		name := rest[:closeRel]
		if strings.ContainsRune(name, '{') {
			return fmt.Errorf("custom Caddy config can't nest placeholders in %q", s)
		}
		if err := checkOnePlaceholder(name, pctx); err != nil {
			return err
		}
		i = start + 1 + closeRel + 1
	}
	return nil
}

func checkOnePlaceholder(name string, pctx placeholderContext) error {
	if pctx == phRegex {
		if regexQuantifierRE().MatchString(name) {
			return nil
		}
		return fmt.Errorf("custom Caddy regex can't use placeholder {%s}", name)
	}
	if allowedGeneralPlaceholders[name] {
		return nil
	}
	if pctx == phHeaderOrBody {
		if allowedHeaderAndBodyOnlyPlaceholders[name] || headerPlaceholderRE().MatchString(name) {
			return nil
		}
	}
	if allowedHeaderAndBodyOnlyPlaceholders[name] || headerPlaceholderRE().MatchString(name) {
		return fmt.Errorf("placeholder {%s} is allowed only in header values and respond bodies", name)
	}
	return fmt.Errorf("placeholder {%s} isn't allowed in custom Caddy config", name)
}

func checkPlaceholdersInTree(v any, pctx placeholderContext) error {
	switch x := v.(type) {
	case string:
		return checkPlaceholdersInString(x, pctx)
	case map[string]any:
		for k, val := range x {
			if err := checkPlaceholdersInString(k, phNone); err != nil {
				return err
			}
			if err := checkPlaceholdersInTree(val, pctx); err != nil {
				return err
			}
		}
	case []any:
		for _, item := range x {
			if err := checkPlaceholdersInTree(item, pctx); err != nil {
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

func (ac *adaptCheck) checkAdaptedRoutes(routes []any) error {
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
		if g, ok := rt["group"].(string); ok {
			if err := checkPlaceholdersInString(g, phNone); err != nil {
				return err
			}
		}
		if matchers, ok := rt["match"].([]any); ok {
			if err := ac.checkAdaptedMatchers(matchers); err != nil {
				return err
			}
		}
		if handlers, ok := rt["handle"].([]any); ok {
			for _, hItem := range handlers {
				h, ok := hItem.(map[string]any)
				if !ok {
					return errors.New("adapted Caddy handler isn't an object")
				}
				if err := ac.checkAdaptedHandler(h); err != nil {
					return err
				}
			}
		}
	}
	return nil
}

func (ac *adaptCheck) checkAdaptedMatchers(matchers []any) error {
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
				if err := ac.checkAdaptedMatchers(sub); err != nil {
					return err
				}
			case "file":
				if ac.allowedRoot == "" {
					return errors.New("file matcher isn't allowed on a proxy website that mounts no folder")
				}
				fm, ok := v.(map[string]any)
				if !ok {
					return errors.New("invalid 'file' matcher in adapted Caddy config")
				}
				for fk, fv := range fm {
					if !allowedFileMatcherKeys[fk] {
						return fmt.Errorf("file matcher option %q isn't allowed in custom site config", fk)
					}
					switch fk {
					case "root":
						s, ok := fv.(string)
						if !ok || s == "" {
							return errors.New("file matcher root must be a non-empty static path inside the website's folder")
						}
						if err := checkAdaptedRoot("file matcher", s, ac.allowedRoot); err != nil {
							return err
						}
					case "try_files":
						if err := checkPlaceholdersInTree(fv, phGeneral); err != nil {
							return err
						}
					default:
						if err := checkPlaceholdersInTree(fv, phNone); err != nil {
							return err
						}
					}
				}
			case "path_regexp":
				if err := checkRegexpObj(v); err != nil {
					return err
				}
			case "header_regexp":
				if err := checkNamedRegexpMap(v, phNone); err != nil {
					return err
				}
			case "vars_regexp":
				if err := checkNamedRegexpMap(v, phGeneral); err != nil {
					return err
				}
			case "vars":
				vm, ok := v.(map[string]any)
				if !ok {
					return errors.New("invalid 'vars' matcher in adapted Caddy config")
				}
				for vk, vv := range vm {
					if err := checkPlaceholdersInString(vk, phGeneral); err != nil {
						return err
					}
					if err := checkPlaceholdersInTree(vv, phGeneral); err != nil {
						return err
					}
				}
			default:
				if err := checkPlaceholdersInTree(v, phNone); err != nil {
					return err
				}
			}
		}
	}
	return nil
}

func checkRegexpObj(v any) error {
	m, ok := v.(map[string]any)
	if !ok {
		return errors.New("invalid regexp matcher in adapted Caddy config")
	}
	for k, val := range m {
		switch k {
		case "pattern":
			s, ok := val.(string)
			if !ok {
				return errors.New("regexp pattern must be a string")
			}
			if err := checkPlaceholdersInString(s, phRegex); err != nil {
				return err
			}
		case "name":
			if err := checkPlaceholdersInTree(val, phNone); err != nil {
				return err
			}
		default:
			return fmt.Errorf("regexp matcher option %q isn't allowed", k)
		}
	}
	return nil
}

func checkNamedRegexpMap(v any, keyCtx placeholderContext) error {
	m, ok := v.(map[string]any)
	if !ok {
		return errors.New("invalid regexp matcher map in adapted Caddy config")
	}
	for k, val := range m {
		if err := checkPlaceholdersInString(k, keyCtx); err != nil {
			return err
		}
		if err := checkRegexpObj(val); err != nil {
			return err
		}
	}
	return nil
}

func checkHeaderOps(raw any) error {
	m, ok := raw.(map[string]any)
	if !ok {
		return errors.New("invalid header operations in adapted Caddy config")
	}
	for k, v := range m {
		if !allowedHeaderOpKeys[k] {
			return fmt.Errorf("header operation %q isn't allowed in custom site config", k)
		}
		switch k {
		case "set", "add":
			if err := checkPlaceholdersInTree(v, phHeaderOrBody); err != nil {
				return err
			}
		case "delete":
			if err := checkPlaceholdersInTree(v, phNone); err != nil {
				return err
			}
		case "replace":
			rm, ok := v.(map[string]any)
			if !ok {
				return errors.New("invalid header replace map in adapted Caddy config")
			}
			for hk, rules := range rm {
				if err := checkPlaceholdersInString(hk, phNone); err != nil {
					return err
				}
				arr, ok := rules.([]any)
				if !ok {
					return errors.New("invalid header replace list in adapted Caddy config")
				}
				for _, item := range arr {
					entry, ok := item.(map[string]any)
					if !ok {
						return errors.New("invalid header replace entry in adapted Caddy config")
					}
					for ek, ev := range entry {
						switch ek {
						case "search_regexp":
							s, _ := ev.(string)
							if err := checkPlaceholdersInString(s, phRegex); err != nil {
								return err
							}
						case "search":
							if err := checkPlaceholdersInTree(ev, phNone); err != nil {
								return err
							}
						case "replace":
							if err := checkPlaceholdersInTree(ev, phHeaderOrBody); err != nil {
								return err
							}
						default:
							return fmt.Errorf("header replace option %q isn't allowed", ek)
						}
					}
				}
			}
		case "require":
			if err := checkPlaceholdersInTree(v, phNone); err != nil {
				return err
			}
		}
	}
	return nil
}

func (ac *adaptCheck) checkAdaptedHandler(h map[string]any) error {
	name, _ := h["handler"].(string)
	if !allowedHandlers[name] {
		return fmt.Errorf("custom Caddy handler %q isn't allowed in custom site config", name)
	}
	allowedKeys := allowedHandlerKeys[name]
	for k := range h {
		if !allowedKeys[k] {
			if name == "file_server" && k == "browse" {
				return errors.New("file_server browse isn't allowed in custom site config")
			}
			if name == "vars" {
				return fmt.Errorf("custom Caddy vars can set only a static root inside the website's folder, not %q", k)
			}
			if name == "reverse_proxy" && k == "dynamic_upstreams" {
				return errors.New("dynamic_upstreams isn't allowed in custom Caddy config")
			}
			return fmt.Errorf("custom Caddy handler %q option %q isn't allowed", name, k)
		}
	}
	switch name {
	case "subroute":
		if rawErrs, hasErrs := h["errors"]; hasErrs {
			if err := ac.checkAdaptedErrors(rawErrs); err != nil {
				return err
			}
		}
		if routes, ok := h["routes"].([]any); ok {
			if err := ac.checkAdaptedRoutes(routes); err != nil {
				return err
			}
		}
	case "vars":
		rootVal, exists := h["root"]
		if !exists {
			return errors.New("custom Caddy vars handler must set only a static root path")
		}
		s, ok := rootVal.(string)
		if !ok || s == "" {
			return errors.New("custom Caddy root must be a non-empty static path string")
		}
		if err := checkAdaptedRoot("custom Caddy", s, ac.allowedRoot); err != nil {
			return err
		}
	case "headers":
		if reqOps, ok := h["request"]; ok {
			if err := checkHeaderOps(reqOps); err != nil {
				return err
			}
		}
		if respOps, ok := h["response"]; ok {
			if err := checkHeaderOps(respOps); err != nil {
				return err
			}
		}
	case "static_response":
		if hdrs, ok := h["headers"]; ok {
			if err := checkPlaceholdersInTree(hdrs, phHeaderOrBody); err != nil {
				return err
			}
		}
		if body, ok := h["body"]; ok {
			if err := checkPlaceholdersInTree(body, phHeaderOrBody); err != nil {
				return err
			}
		}
		if sc, ok := h["status_code"]; ok {
			if err := checkPlaceholdersInTree(sc, phNone); err != nil {
				return err
			}
		}
	case "rewrite":
		for k, v := range h {
			switch k {
			case "handler":
			case "uri":
				if err := checkPlaceholdersInTree(v, phGeneral); err != nil {
					return err
				}
			case "path_regexp":
				arr, ok := v.([]any)
				if !ok {
					return errors.New("invalid rewrite path_regexp in adapted Caddy config")
				}
				for _, item := range arr {
					entry, ok := item.(map[string]any)
					if !ok {
						return errors.New("invalid rewrite path_regexp entry in adapted Caddy config")
					}
					for ek, ev := range entry {
						switch ek {
						case "find":
							s, _ := ev.(string)
							if err := checkPlaceholdersInString(s, phRegex); err != nil {
								return err
							}
						case "replace":
							if err := checkPlaceholdersInTree(ev, phGeneral); err != nil {
								return err
							}
						default:
							return fmt.Errorf("rewrite path_regexp option %q isn't allowed", ek)
						}
					}
				}
			default:
				if err := checkPlaceholdersInTree(v, phNone); err != nil {
					return err
				}
			}
		}
	case "encode":
		if encs, ok := h["encodings"].(map[string]any); ok {
			for ek, ev := range encs {
				if !allowedEncodings[ek] {
					return fmt.Errorf("encode algorithm %q isn't allowed in custom site config", ek)
				}
				if err := checkPlaceholdersInTree(ev, phNone); err != nil {
					return err
				}
			}
		}
		for k, v := range h {
			if k != "handler" && k != "encodings" {
				if err := checkPlaceholdersInTree(v, phNone); err != nil {
					return err
				}
			}
		}
	case "request_body", "copy_response", "copy_response_headers":
		if err := checkPlaceholdersInTree(h, phNone); err != nil {
			return err
		}
	case "error":
		if errMsg, ok := h["error"]; ok {
			if err := checkPlaceholdersInTree(errMsg, phGeneral); err != nil {
				return err
			}
		}
		if sc, ok := h["status_code"]; ok {
			if err := checkPlaceholdersInTree(sc, phNone); err != nil {
				return err
			}
		}
	case "file_server":
		if ac.allowedRoot == "" {
			return errors.New("file_server isn't allowed on a proxy website that mounts no folder")
		}
		if rootVal, exists := h["root"]; exists {
			s, ok := rootVal.(string)
			if !ok || s == "" {
				return errors.New("file_server root must be a non-empty static path string")
			}
			if err := checkAdaptedRoot("file_server", s, ac.allowedRoot); err != nil {
				return err
			}
		}
		for k, v := range h {
			if k != "handler" && k != "root" {
				if err := checkPlaceholdersInTree(v, phNone); err != nil {
					return err
				}
			}
		}
	case "authentication":
		if providers, ok := h["providers"].(map[string]any); ok {
			for p, pv := range providers {
				if p != "http_basic" {
					return fmt.Errorf("authentication provider %q isn't allowed in custom site config", p)
				}
				if err := checkPlaceholdersInTree(pv, phNone); err != nil {
					return err
				}
			}
		}
	case "reverse_proxy":
		if ups, ok := h["upstreams"].([]any); ok {
			for _, uItem := range ups {
				u, ok := uItem.(map[string]any)
				if !ok {
					return errors.New("invalid reverse_proxy upstream in adapted Caddy config")
				}
				for uk := range u {
					if uk != "dial" && uk != "max_requests" {
						return fmt.Errorf("reverse_proxy upstream option %q isn't allowed", uk)
					}
				}
				dial, _ := u["dial"].(string)
				if err := ac.checkAdaptedDial(dial); err != nil {
					return err
				}
			}
		}
		if rawHdrs, ok := h["headers"]; ok {
			hm, ok := rawHdrs.(map[string]any)
			if !ok {
				return errors.New("invalid reverse_proxy headers in adapted Caddy config")
			}
			for hk, hv := range hm {
				if hk != "request" && hk != "response" {
					return fmt.Errorf("reverse_proxy headers option %q isn't allowed", hk)
				}
				if err := checkHeaderOps(hv); err != nil {
					return err
				}
			}
		}
		if rawRW, ok := h["rewrite"]; ok {
			rwm, ok := rawRW.(map[string]any)
			if !ok {
				return errors.New("invalid reverse_proxy rewrite in adapted Caddy config")
			}
			for rk, rv := range rwm {
				switch rk {
				case "uri":
					if err := checkPlaceholdersInTree(rv, phGeneral); err != nil {
						return err
					}
				case "method", "strip_path_prefix", "strip_path_suffix":
					if err := checkPlaceholdersInTree(rv, phNone); err != nil {
						return err
					}
				default:
					return fmt.Errorf("reverse_proxy rewrite option %q isn't allowed", rk)
				}
			}
		}
		if rawTr, hasTr := h["transport"]; hasTr {
			if err := ac.checkAdaptedTransport(rawTr); err != nil {
				return err
			}
		}
		if rawHR, hasHR := h["handle_response"]; hasHR {
			if err := ac.checkHandleResponses(rawHR); err != nil {
				return err
			}
		}
		for _, k := range []string{"load_balancing", "flush_interval", "stream_timeout", "stream_close_delay", "trusted_proxies"} {
			if v, ok := h[k]; ok {
				if err := checkPlaceholdersInTree(v, phNone); err != nil {
					return err
				}
			}
		}
	case "intercept":
		if rawHR, hasHR := h["handle_response"]; hasHR {
			if err := ac.checkHandleResponses(rawHR); err != nil {
				return err
			}
		}
	}
	return nil
}

func (ac *adaptCheck) checkHandleResponses(raw any) error {
	list, ok := raw.([]any)
	if !ok {
		return errors.New("invalid handle_response in adapted Caddy config")
	}
	for _, item := range list {
		entry, ok := item.(map[string]any)
		if !ok {
			return errors.New("invalid handle_response entry in adapted Caddy config")
		}
		for k, v := range entry {
			switch k {
			case "routes":
				routes, ok := v.([]any)
				if !ok {
					return errors.New("invalid handle_response routes in adapted Caddy config")
				}
				if err := ac.checkAdaptedRoutes(routes); err != nil {
					return err
				}
			case "match", "status_code":
				if err := checkPlaceholdersInTree(v, phNone); err != nil {
					return err
				}
			default:
				return fmt.Errorf("handle_response option %q isn't allowed in custom site config", k)
			}
		}
	}
	return nil
}

func (ac *adaptCheck) checkAdaptedTransport(rawTr any) error {
	tr, ok := rawTr.(map[string]any)
	if !ok {
		return errors.New("invalid reverse_proxy transport in adapted Caddy config")
	}
	proto, _ := tr["protocol"].(string)
	switch proto {
	case "", "http":
		for k, v := range tr {
			if !allowedHTTPTransportKeys[k] {
				return fmt.Errorf("reverse_proxy http transport option %q isn't allowed in custom site config", k)
			}
			if k == "tls" {
				tlsMap, ok := v.(map[string]any)
				if !ok {
					return errors.New("invalid reverse_proxy transport.tls in adapted Caddy config")
				}
				for tk, tv := range tlsMap {
					if !allowedHTTPTransportTLSKeys[tk] {
						return fmt.Errorf("reverse_proxy transport.tls option %q isn't allowed in custom site config", tk)
					}
					if err := checkPlaceholdersInTree(tv, phNone); err != nil {
						return err
					}
				}
				continue
			}
			if err := checkPlaceholdersInTree(v, phNone); err != nil {
				return err
			}
		}
	case "fastcgi":
		for k, v := range tr {
			if !allowedFastCGITransportKeys[k] {
				return fmt.Errorf("fastcgi transport option %q isn't allowed in custom site config", k)
			}
			switch k {
			case "root":
				s, ok := v.(string)
				if !ok || s == "" {
					return errors.New("fastcgi transport root must be a non-empty static path string")
				}
				if err := checkAdaptedRoot("fastcgi transport", s, ac.allowedRoot); err != nil {
					return err
				}
			case "env":
				if err := checkPlaceholdersInTree(v, phGeneral); err != nil {
					return err
				}
			default:
				if err := checkPlaceholdersInTree(v, phNone); err != nil {
					return err
				}
			}
		}
	default:
		return fmt.Errorf("reverse_proxy transport %q isn't allowed in custom site config", proto)
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

func (ac *adaptCheck) checkAdaptedDial(dial string) error {
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
	if portStr != "" {
		if n, err := strconv.Atoi(portStr); err == nil && slices.Contains(dockerAPIPorts, n) {
			return fmt.Errorf("custom reverse_proxy can't dial Docker's API port %d", n)
		}
	}
	// Dockwell's own container aliases on the site's private network.
	if host == "php" || subpathAliasRE().MatchString(host) {
		return nil
	}
	if err := netguard.CheckResolved(ac.ctx, host, netguard.Options{}, ac.lookup); err != nil {
		return fmt.Errorf("custom reverse_proxy upstream %q refused: %w", dial, err)
	}
	if net.ParseIP(host) == nil {
		trimmed := strings.TrimSuffix(host, ".")
		if len(trimmed) > 253 || !validHostRE().MatchString(trimmed) {
			return fmt.Errorf("custom reverse_proxy upstream %q isn't a valid hostname", dial)
		}
	}
	return nil
}

// Verify interfaces on *Caddy.
var (
	_ ingress.CaddyfileValidator = (*Caddy)(nil)
	_ ingress.CertificateRenewer = (*Caddy)(nil)
	_ ingress.CertificateReader  = (*Caddy)(nil)
	_ ingress.RenewedCertPruner  = (*Caddy)(nil)
)
