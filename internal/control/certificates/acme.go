// SPDX-License-Identifier: AGPL-3.0-only

package certificates

import (
	"context"
	"crypto"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/base64"
	"encoding/json"
	"encoding/pem"
	"errors"
	"fmt"
	"io"
	"net/http"
	"slices"
	"strconv"
	"strings"
	"time"

	"golang.org/x/crypto/acme"

	"github.com/dockwell/dockwell/internal/control/dnsproviders"
	"github.com/dockwell/dockwell/internal/store"
)

// LetsEncrypt is the CA certificates by DNS come from unless set.
const LetsEncrypt = "https://acme-v02.api.letsencrypt.org/directory"

// clientTimeout bounds reaching the CA for its directory and the account, as
// x/crypto/acme retries a CA that answers 5xx with no end of its own.
var clientTimeout = time.Minute

// client is an ACME client on the install's own account, made and registered on
// its first use, its key sealed in the store (ADR 0088). It's apart from the panel's
// autocert account and from Caddy's.
func (m *Manager) client(ctx context.Context) (*acme.Client, error) {
	var acct account
	var key crypto.Signer
	err := m.DB.Update(ctx, func(tx *store.Tx) error {
		sealed, err := tx.GetSetting(ctx, accountSetting)
		switch {
		case errors.Is(err, store.ErrNotFound):
			k, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
			if err != nil {
				return err
			}
			der, err := x509.MarshalECPrivateKey(k)
			if err != nil {
				return err
			}
			acct = account{Key: pem.EncodeToMemory(&pem.Block{Type: "EC PRIVATE KEY", Bytes: der}), Gen: 1}
			clear(der)
			b, err := m.sealTo(ctx, tx, accountHolder, acct.Gen, acct)
			if err != nil {
				return err
			}
			key = k
			return tx.PutSetting(ctx, accountSetting, b)
		case err != nil:
			return err
		}
		if err := m.openFrom(ctx, tx, accountHolder, 1, sealed, &acct); err != nil {
			return fmt.Errorf("can't open the ACME account: %w", err)
		}
		key, err = parseKey(acct.Key)
		clear(acct.Key)
		return err
	})
	if err != nil {
		return nil, err
	}
	c := &acme.Client{Key: key, DirectoryURL: m.directory(), HTTPClient: m.HTTP, UserAgent: "dockwell"}
	reach, cancel := context.WithTimeout(ctx, clientTimeout)
	defer cancel()
	if _, err := c.Discover(reach); err != nil {
		return nil, fmt.Errorf("can't reach the certificate authority at %s: %w", m.directory(), caError(err))
	}
	if acct.URL != "" {
		c.KID = acme.KeyID(acct.URL)
		return c, nil
	}
	a, err := c.Register(reach, &acme.Account{}, acme.AcceptTOS)
	if errors.Is(err, acme.ErrAccountAlreadyExists) {
		a, err = c.GetReg(reach, "")
	}
	if err != nil {
		return nil, fmt.Errorf("can't make the ACME account at %s: %w", m.directory(), err)
	}
	acct.URL = a.URI
	err = m.DB.Update(ctx, func(tx *store.Tx) error {
		b, err := m.sealTo(ctx, tx, accountHolder, acct.Gen, acct)
		if err != nil {
			return err
		}
		return tx.PutSetting(ctx, accountSetting, b)
	})
	if err != nil {
		return nil, err
	}
	c.KID = acme.KeyID(acct.URL)
	return c, nil
}

func (m *Manager) directory() string {
	if m.Directory != "" {
		return m.Directory
	}
	return LetsEncrypt
}

func parseKey(p []byte) (crypto.Signer, error) {
	b, _ := pem.Decode(p)
	if b == nil {
		return nil, errors.New("the key isn't PEM")
	}
	return x509.ParseECPrivateKey(b.Bytes)
}

// solver writes a name's challenge record through the provider whose zone holds it.
type solver struct {
	m         *Manager
	providers map[string]dnsproviders.API // by provider name
	zones     map[string][]dnsproviders.Zone
	// written are the records written, to delete whatever happens.
	written []noted
}

// write writes value at _acme-challenge.<host>, through the provider that proves
// host, noting it first so a crash leaves nothing behind.
func (s *solver) write(ctx context.Context, provider, host, value string) error {
	api := s.providers[provider]
	if api == nil {
		return fmt.Errorf("the DNS provider %s isn't there", provider)
	}
	zone, ok := zoneFor(s.zones[provider], host)
	if !ok {
		return fmt.Errorf("the DNS provider %s's token doesn't reach a zone that holds %s. Give it DNS:Edit there", provider, host)
	}
	n := noted{Provider: provider, ZoneID: zone.ID, Zone: zone.Name, Name: "_acme-challenge." + host, Value: value, At: s.m.now()}
	if err := s.m.note(ctx, n, ""); err != nil {
		return err
	}
	id, err := api.AppendTXT(ctx, zone, n.Name, value)
	if err != nil {
		return err
	}
	n.ID = id
	s.written = append(s.written, n)
	return s.m.note(ctx, n, id)
}

// cleanup deletes every record written, on a context of its own, and forgets them.
func (s *solver) cleanup(ctx context.Context) {
	ctx, cancel := context.WithTimeout(context.WithoutCancel(ctx), time.Minute)
	defer cancel()
	for _, n := range s.written {
		if err := s.providers[n.Provider].Delete(ctx, dnsproviders.Zone{ID: n.ZoneID, Name: n.Zone}, n.ID); err != nil {
			s.m.logger().Warn("can't delete a challenge record; it's noted, and goes as the control plane next starts", "name", n.Name, "error", err.Error())
			continue
		}
		_ = s.m.forget(ctx, n)
	}
}

// zoneFor is the zone among zones that holds host: the longest.
func zoneFor(zones []dnsproviders.Zone, host string) (dnsproviders.Zone, bool) {
	var best dnsproviders.Zone
	for _, z := range zones {
		if (host == z.Name || strings.HasSuffix(host, "."+z.Name)) && len(z.Name) > len(best.Name) {
			best = z
		}
	}
	return best, best.Name != ""
}

// authzFailed is an order that failed for some of its names alone: a record that
// couldn't be written, or an authorization the CA refused. The rest can be ordered
// without them.
type authzFailed struct {
	Names []string
	Err   error
}

func (e *authzFailed) Error() string {
	return strings.Join(e.Names, ", ") + ": " + e.Err.Error()
}

func (e *authzFailed) Unwrap() error { return e.Err }

// order gets a certificate for names, each proven by DNS-01 through the provider
// providerOf names for it, with a key of its own.
func (m *Manager) order(ctx context.Context, c *acme.Client, names []string, providerOf func(name string) string, s *solver) (*issued, error) {
	defer s.cleanup(ctx)
	o, err := c.AuthorizeOrder(ctx, acme.DomainIDs(names...))
	if err != nil {
		return nil, fmt.Errorf("ordering: %w", caError(err))
	}
	var accept []*acme.Challenge
	var waits []string
	nameOf := map[string]string{}
	for _, u := range o.AuthzURLs {
		z, err := c.GetAuthorization(ctx, u)
		if err != nil {
			return nil, caError(err)
		}
		if z.Status == acme.StatusValid {
			continue
		}
		var chal *acme.Challenge
		for _, ch := range z.Challenges {
			if ch.Type == "dns-01" {
				chal = ch
			}
		}
		if chal == nil {
			return nil, fmt.Errorf("the certificate authority offers no DNS-01 challenge for %s", z.Identifier.Value)
		}
		value, err := c.DNS01ChallengeRecord(chal.Token)
		if err != nil {
			return nil, err
		}
		name := z.Identifier.Value
		if z.Wildcard {
			name = "*." + name
		}
		host := strings.TrimPrefix(name, "*.")
		if err := s.write(ctx, providerOf(name), host, value); err != nil {
			return nil, &authzFailed{Names: []string{name}, Err: err}
		}
		accept = append(accept, chal)
		waits = append(waits, u)
		nameOf[u] = name
		if m.Propagation != nil {
			if err := m.Propagation.Wait(ctx, "_acme-challenge."+host, value); err != nil {
				return nil, &authzFailed{Names: []string{name}, Err: err}
			}
		}
	}
	for _, ch := range accept {
		if _, err := c.Accept(ctx, ch); err != nil {
			return nil, fmt.Errorf("asking for %s's check: %w", ch.URI, caError(err))
		}
	}
	for _, u := range waits {
		if _, err := c.WaitAuthorization(ctx, u); err != nil {
			var ae *acme.AuthorizationError
			if errors.As(err, &ae) {
				return nil, &authzFailed{Names: []string{nameOf[u]}, Err: caError(err)}
			}
			return nil, fmt.Errorf("waiting for its check: %w", caError(err))
		}
	}
	if o, err = c.WaitOrder(ctx, o.URI); err != nil {
		return nil, fmt.Errorf("waiting for the order: %w", caError(err))
	}
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		return nil, err
	}
	csr, err := x509.CreateCertificateRequest(rand.Reader, &x509.CertificateRequest{Subject: pkix.Name{CommonName: names[0]}, DNSNames: names}, key)
	if err != nil {
		return nil, err
	}
	// From the order, as an RFC 8555 CA needn't say where the order is as it
	// finalizes it (x/crypto/acme's CreateCertFromOrder).
	der, _, err := c.CreateCertFromOrder(ctx, o, csr, true)
	if err != nil {
		return nil, fmt.Errorf("finalizing the order: %w", caError(err))
	}
	leaf, err := x509.ParseCertificate(der[0])
	if err != nil {
		return nil, err
	}
	var chain []byte
	for _, d := range der {
		chain = append(chain, pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: d})...)
	}
	kd, err := x509.MarshalECPrivateKey(key)
	if err != nil {
		return nil, err
	}
	defer clear(kd)
	sorted := slices.Clone(names)
	slices.Sort(sorted)
	return &issued{
		Chain: string(chain), Key: pem.EncodeToMemory(&pem.Block{Type: "EC PRIVATE KEY", Bytes: kd}),
		Names: sorted, Serial: leaf.SerialNumber.Text(16), NotAfter: leaf.NotAfter, IssuedAt: m.now(),
	}, nil
}

// caError is what the CA said, in its own words: its problem's type and detail.
func caError(err error) error {
	var ae *acme.Error
	if errors.As(err, &ae) {
		kind := strings.TrimPrefix(ae.ProblemType, "urn:ietf:params:acme:error:")
		if kind == "" {
			// No problem document, as from a proxy in front of it: its status, and
			// what it said.
			if detail := strings.TrimSpace(ae.Detail); detail != "" {
				return fmt.Errorf("the certificate authority answered %d: %s", ae.StatusCode, detail)
			}
			return fmt.Errorf("the certificate authority answered %d", ae.StatusCode)
		}
		if ae.Detail != "" {
			return fmt.Errorf("the certificate authority said %s: %s", kind, ae.Detail)
		}
		return fmt.Errorf("the certificate authority said %s", kind)
	}
	var ord *acme.OrderError
	if errors.As(err, &ord) {
		return fmt.Errorf("the certificate authority's order went %s", ord.Status)
	}
	return err
}

// renewalWindow asks the CA when to renew leaf, by ACME Renewal Information (RFC
// 9773), and says false where the CA says nothing of it. retry is how long the CA
// asks to be left before it's asked again, by its Retry-After.
func (m *Manager) renewalWindow(ctx context.Context, leaf *x509.Certificate) (start, end time.Time, retry time.Duration, ok bool) {
	ctx, cancel := context.WithTimeout(ctx, 15*time.Second)
	defer cancel()
	var dir struct {
		RenewalInfo string `json:"renewalInfo"`
	}
	if _, err := m.getJSON(ctx, m.directory(), &dir); err != nil || dir.RenewalInfo == "" || len(leaf.AuthorityKeyId) == 0 {
		return time.Time{}, time.Time{}, 0, false
	}
	var info struct {
		Window struct {
			Start time.Time `json:"start"`
			End   time.Time `json:"end"`
		} `json:"suggestedWindow"`
	}
	h, err := m.getJSON(ctx, strings.TrimSuffix(dir.RenewalInfo, "/")+"/"+ariID(leaf), &info)
	if h != nil {
		if secs, err := strconv.Atoi(h.Get("Retry-After")); err == nil && secs > 0 {
			retry = time.Duration(secs) * time.Second
		}
	}
	if err != nil || info.Window.Start.IsZero() || !info.Window.End.After(info.Window.Start) {
		return time.Time{}, time.Time{}, retry, false
	}
	return info.Window.Start, info.Window.End, retry, true
}

// ariID is a certificate's ID for ARI: its authority key ID and its serial number,
// as DER encodes the serial, each base64url with no padding (RFC 9773, section 4.1).
func ariID(leaf *x509.Certificate) string {
	serial := leaf.SerialNumber.Bytes()
	if len(serial) == 0 || serial[0]&0x80 != 0 {
		serial = append([]byte{0}, serial...)
	}
	return base64.RawURLEncoding.EncodeToString(leaf.AuthorityKeyId) + "." + base64.RawURLEncoding.EncodeToString(serial)
}

func (m *Manager) getJSON(ctx context.Context, url string, into any) (http.Header, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, http.NoBody)
	if err != nil {
		return nil, err
	}
	resp, err := m.httpClient().Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return resp.Header, fmt.Errorf("%s answered %d", url, resp.StatusCode)
	}
	return resp.Header, json.NewDecoder(io.LimitReader(resp.Body, 1<<20)).Decode(into)
}

func (m *Manager) httpClient() *http.Client {
	if m.HTTP != nil {
		return m.HTTP
	}
	return http.DefaultClient
}

// windowAt is when in the CA's window to renew: a third of the way in.
func windowAt(start, end time.Time) time.Time {
	return start.Add(end.Sub(start) / 3)
}

// renewAt is when a certificate renews: inside the CA's window where it gives one,
// and otherwise 30 days before its end, or at two thirds of a shorter life.
func renewAt(c *issued, windowStart, windowEnd time.Time, hasWindow bool) time.Time {
	if hasWindow {
		return windowAt(windowStart, windowEnd)
	}
	// The later of the two: 30 days before the end for a long life, two thirds of
	// the way for a shorter one, as 30 days before its end is before it began.
	at := c.NotAfter.Add(-30 * 24 * time.Hour)
	if life := c.NotAfter.Sub(c.IssuedAt); life > 0 {
		if two := c.IssuedAt.Add(life * 2 / 3); two.After(at) {
			at = two
		}
	}
	return at
}
