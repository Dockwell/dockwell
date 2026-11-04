// SPDX-License-Identifier: AGPL-3.0-only

// Package identity is the control plane's authority over who is who: the
// install's ID, its certificate authority, the join tickets that let new
// servers in, and the certificates that identify them afterwards.
//
// The CA's private key never leaves the control plane and is stored sealed
// (internal/seal). Join tokens are single-use and short-lived, only their
// secret's hash is stored, and creating or spending one is audited.
package identity

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"crypto/subtle"
	"crypto/x509"
	"errors"
	"fmt"
	"log/slog"
	"strconv"
	"strings"
	"time"

	agentv1 "github.com/dockwell/dockwell/gen/dockwell/agent/v1"
	"github.com/dockwell/dockwell/internal/human"
	"github.com/dockwell/dockwell/internal/id"
	"github.com/dockwell/dockwell/internal/pki"
	"github.com/dockwell/dockwell/internal/seal"
	"github.com/dockwell/dockwell/internal/store"
	"github.com/dockwell/dockwell/internal/ticket"
)

// Ticket lifetimes.
const (
	DefaultTicketTTL = time.Hour
	MaxTicketTTL     = 24 * time.Hour
)

const (
	settingInstallID = "install.id"
	settingCACert    = "ca.cert"
	settingCAKey     = "ca.key"
	settingAgentURL  = "agent.url"
	purposeCAKey     = "ca-key"
)

// Errors for callers to map to API codes.
var (
	// ErrTicketRejected covers a wrong, spent or expired join token. One
	// error for all three gives a guesser nothing to learn.
	ErrTicketRejected = errors.New("this join ticket is invalid, already used or expired. Create a new one on the control server with dockwell token create")
	ErrRevoked        = errors.New("this server was removed from Dockwell. Join it again with a new ticket")
	ErrUnknownServer  = errors.New("this server isn't known to the control plane. Join it with a ticket")
	// ErrStaleCertificate means a renewal presented a certificate that was
	// already replaced: either a lost retry gone wrong, or another machine
	// holding a copy of the server's key.
	ErrStaleCertificate = errors.New("this server's certificate was already replaced by a newer one. " +
		"If this server didn't renew it, another machine has its key: remove the server and join it again")
	// ErrBadRequest wraps requests that are malformed, such as an invalid CSR.
	ErrBadRequest = errors.New("invalid request")
	// ErrNoInstall means the database has no install yet.
	ErrNoInstall = errors.New("this data directory has no Dockwell install yet. Start the control plane with dockwell run first")
)

// Authority issues and checks identities for one install.
type Authority struct {
	db        *store.DB
	ca        *pki.CA
	installID string
	logger    *slog.Logger
	now       func() time.Time
}

// Bootstrap loads the install's identity, creating the install ID and CA on
// the first start.
func Bootstrap(ctx context.Context, db *store.DB, key *seal.Key, logger *slog.Logger) (*Authority, error) {
	return load(ctx, db, key, logger, true)
}

// Open loads an existing install's identity and never creates one, for
// commands that run beside the control plane.
func Open(ctx context.Context, db *store.DB, key *seal.Key, logger *slog.Logger) (*Authority, error) {
	return load(ctx, db, key, logger, false)
}

func load(ctx context.Context, db *store.DB, key *seal.Key, logger *slog.Logger, create bool) (*Authority, error) {
	a := &Authority{db: db, logger: logger, now: time.Now}
	err := db.Update(ctx, func(tx *store.Tx) error {
		installID, err := tx.GetSetting(ctx, settingInstallID)
		if errors.Is(err, store.ErrNotFound) {
			if !create {
				return ErrNoInstall
			}
			return a.create(ctx, tx, key)
		}
		if err != nil {
			return err
		}
		certPEM, err := tx.GetSetting(ctx, settingCACert)
		if err != nil {
			return fmt.Errorf("the install has an ID but no CA certificate: %w", err)
		}
		sealed, err := tx.GetSetting(ctx, settingCAKey)
		if err != nil {
			return fmt.Errorf("the install has an ID but no CA key: %w", err)
		}
		keyDER, err := key.Open(purposeCAKey, sealed)
		if err != nil {
			return fmt.Errorf("can't unseal the CA key: %w. The key file doesn't match this database; restore the matching key or database", err)
		}
		ca, err := pki.LoadCA(certPEM, keyDER)
		if err != nil {
			return err
		}
		a.installID, a.ca = string(installID), ca
		return nil
	})
	if err != nil {
		return nil, err
	}
	return a, nil
}

func (a *Authority) create(ctx context.Context, tx *store.Tx, key *seal.Key) error {
	installID := id.New("ins")
	ca, err := pki.NewCA(installID, a.now())
	if err != nil {
		return err
	}
	keyDER, err := ca.KeyDER()
	if err != nil {
		return err
	}
	for k, v := range map[string][]byte{
		settingInstallID: []byte(installID),
		settingCACert:    ca.CertPEM(),
		settingCAKey:     key.Seal(purposeCAKey, keyDER),
	} {
		if err := tx.PutSetting(ctx, k, v); err != nil {
			return err
		}
	}
	if err := a.audit(ctx, tx, "system", store.SurfaceAutomatic, "install.create", installID, "", "CA "+ca.Pin()); err != nil {
		return err
	}
	a.installID, a.ca = installID, ca
	a.logger.Info("created a new Dockwell install", slog.String("install", installID), slog.String("ca_pin", ca.Pin()))
	return nil
}

// SetAgentURL records where agents reach the control plane, so tickets made
// later (by dockwell token create, say) point there.
func (a *Authority) SetAgentURL(ctx context.Context, u string) error {
	return a.db.Update(ctx, func(tx *store.Tx) error { return tx.PutSetting(ctx, settingAgentURL, []byte(u)) })
}

// AgentURL returns where agents reach the control plane, as last recorded
// by the running control plane.
func (a *Authority) AgentURL(ctx context.Context) (string, error) {
	var u []byte
	err := a.db.View(ctx, func(tx *store.Tx) error {
		var err error
		u, err = tx.GetSetting(ctx, settingAgentURL)
		return err
	})
	if errors.Is(err, store.ErrNotFound) {
		return "", errors.New("the control plane hasn't recorded its address yet. Start it with dockwell run, or pass --control-url")
	}
	return string(u), err
}

// InstallID identifies this install.
func (a *Authority) InstallID() string { return a.installID }

// CA is the install's certificate authority.
func (a *Authority) CA() *pki.CA { return a.ca }

// Ticket is a join ticket ready to hand to the operator.
type Ticket struct {
	Value     string
	TokenID   string
	ExpiresAt time.Time
}

// CreateTicket makes a single-use join ticket for the control plane at
// controlURL, valid for ttl.
func (a *Authority) CreateTicket(ctx context.Context, controlURL, actor string, ttl time.Duration) (Ticket, error) {
	if ttl < time.Second || ttl > MaxTicketTTL {
		return Ticket{}, fmt.Errorf("a join ticket can last from 1 s to %s, not %s", human.Duration(MaxTicketTTL), human.Duration(ttl))
	}
	secret := make([]byte, ticket.SecretSize)
	_, _ = rand.Read(secret)
	sum := sha256.Sum256(secret)
	now := a.now()
	tok := store.JoinToken{
		ID:         id.New(id.JoinToken),
		SecretHash: sum[:],
		CreatedAt:  now,
		CreatedBy:  actor,
		ExpiresAt:  now.Add(ttl),
	}
	value := ticket.Encode(&agentv1.JoinTicket{
		ControlUrl: controlURL,
		CaPin:      a.ca.Pin(),
		TokenId:    tok.ID,
		Secret:     secret,
	})
	if _, err := ticket.Decode(value); err != nil {
		return Ticket{}, fmt.Errorf("can't make a ticket for %q: %w", controlURL, err)
	}
	err := a.db.Update(ctx, func(tx *store.Tx) error {
		if err := tx.CreateJoinToken(ctx, tok); err != nil {
			return err
		}
		return a.audit(ctx, tx, actor, store.SurfaceOf(actor), "join_token.create", tok.ID, "", "expires "+tok.ExpiresAt.UTC().Format(time.RFC3339))
	})
	if err != nil {
		return Ticket{}, err
	}
	return Ticket{Value: value, TokenID: tok.ID, ExpiresAt: tok.ExpiresAt}, nil
}

// Joined is a server that has just joined.
type Joined struct {
	Server store.Server
	Cert   *x509.Certificate
}

// Join spends a join token and gives the server a name and a certificate
// for the key in csr. peer names where the request came from, for the audit log.
func (a *Authority) Join(ctx context.Context, tokenID string, secret, csr []byte, hostname, peer string) (Joined, error) {
	var joined Joined
	err := a.db.Update(ctx, func(tx *store.Tx) error {
		tok, err := tx.GetJoinToken(ctx, tokenID)
		if errors.Is(err, store.ErrNotFound) {
			return ErrTicketRejected
		}
		if err != nil {
			return err
		}
		sum := sha256.Sum256(secret)
		now := a.now()
		if subtle.ConstantTimeCompare(sum[:], tok.SecretHash) != 1 || tok.UsedAt != nil || !now.Before(tok.ExpiresAt) {
			return ErrTicketRejected
		}
		joined, err = a.enroll(ctx, tx, csr, hostname, now)
		if err != nil {
			return err
		}
		if err := tx.MarkJoinTokenUsed(ctx, tok.ID, joined.Server.ID, now); err != nil {
			if errors.Is(err, store.ErrNotFound) {
				return ErrTicketRejected
			}
			return err
		}
		return a.audit(ctx, tx, "server:"+joined.Server.ID, store.SurfaceCLI, "server.join", joined.Server.ID, joined.Server.ID,
			fmt.Sprintf("%s joined with %s from %s", joined.Server.Name, tok.ID, peer))
	})
	if err != nil {
		return Joined{}, err
	}
	a.logger.Info("server joined", slog.String("server", joined.Server.ID), slog.String("name", joined.Server.Name))
	return joined, nil
}

// EnrollLocal gives the control server's own agent its identity, without a
// ticket: it shares the machine, and the data directory, with the CA.
func (a *Authority) EnrollLocal(ctx context.Context, csr []byte, hostname string) (Joined, error) {
	var joined Joined
	err := a.db.Update(ctx, func(tx *store.Tx) error {
		var err error
		joined, err = a.enroll(ctx, tx, csr, hostname, a.now())
		if err != nil {
			return err
		}
		return a.audit(ctx, tx, "system", store.SurfaceAutomatic, "server.join", joined.Server.ID, joined.Server.ID,
			joined.Server.Name+" is the control server")
	})
	return joined, err
}

func (a *Authority) enroll(ctx context.Context, tx *store.Tx, csr []byte, hostname string, now time.Time) (Joined, error) {
	serverID := id.New(id.Server)
	cert, err := a.ca.SignServer(csr, serverID, now)
	if err != nil {
		return Joined{}, fmt.Errorf("%w: %w", ErrBadRequest, err)
	}
	base := ServerName(hostname)
	for n := 1; n <= 100; n++ {
		name := base
		if n > 1 {
			name = base + "-" + strconv.Itoa(n)
		}
		s := store.Server{
			ID:           serverID,
			Name:         name,
			CreatedAt:    now,
			CertSerial:   pki.SerialString(cert),
			CertNotAfter: cert.NotAfter,
			CertKeyHash:  pki.KeyHash(cert),
			LastSeenAt:   &now,
		}
		err := tx.CreateServer(ctx, s)
		if errors.Is(err, store.ErrConflict) {
			continue
		}
		if err != nil {
			return Joined{}, err
		}
		return Joined{Server: s, Cert: cert}, nil
	}
	return Joined{}, fmt.Errorf("100 servers are already named like %s; join with a different hostname", base)
}

// Renew signs a fresh certificate for a known, active server that proved
// itself with presentedSerial, the serial of the certificate it connected
// with. Each renewal must bring a new key, and only the current certificate
// may renew. One exception lets an agent whose renewal answer was lost
// retry: its previous certificate, with the key it already sent.
func (a *Authority) Renew(ctx context.Context, serverID, presentedSerial string, csr []byte) (*x509.Certificate, error) {
	var cert *x509.Certificate
	err := a.db.Update(ctx, func(tx *store.Tx) error {
		s, err := a.checkServer(ctx, tx, serverID)
		if err != nil {
			return err
		}
		cert, err = a.ca.SignServer(csr, serverID, a.now())
		if err != nil {
			return fmt.Errorf("%w: %w", ErrBadRequest, err)
		}
		keyHash := pki.KeyHash(cert)
		var prev string
		switch {
		case presentedSerial == s.CertSerial:
			if subtle.ConstantTimeCompare(keyHash, s.CertKeyHash) == 1 {
				return fmt.Errorf("%w: a renewal must use a new key", ErrBadRequest)
			}
			prev = s.CertSerial
		case presentedSerial != "" && presentedSerial == s.PrevCertSerial && subtle.ConstantTimeCompare(keyHash, s.CertKeyHash) == 1:
			prev = s.PrevCertSerial // a retry of a renewal whose answer was lost
		default:
			return ErrStaleCertificate
		}
		if err := tx.UpdateServerCert(ctx, serverID, store.CertUpdate{
			Serial: pki.SerialString(cert), KeyHash: keyHash, NotAfter: cert.NotAfter, PrevSerial: prev,
		}); err != nil {
			return err
		}
		return a.audit(ctx, tx, "server:"+serverID, store.SurfaceAutomatic, "server.renew", serverID, serverID, "certificate "+pki.SerialString(cert))
	})
	if errors.Is(err, ErrStaleCertificate) {
		// Outside the failed transaction, so the warning survives.
		_ = a.db.Update(ctx, func(tx *store.Tx) error {
			return a.audit(ctx, tx, "server:"+serverID, store.SurfaceAutomatic, "server.renew_rejected", serverID, serverID, "presented replaced certificate "+presentedSerial)
		})
		a.logger.Warn("refused to renew a replaced certificate; another machine may hold this server's key",
			slog.String("server", serverID), slog.String("serial", presentedSerial))
	}
	if err != nil {
		return nil, err
	}
	return cert, nil
}

// CheckServer returns a server if it may connect: it exists and isn't revoked.
func (a *Authority) CheckServer(ctx context.Context, serverID string) (store.Server, error) {
	var s store.Server
	err := a.db.View(ctx, func(tx *store.Tx) error {
		var err error
		s, err = a.checkServer(ctx, tx, serverID)
		return err
	})
	return s, err
}

// CheckConnection is CheckServer for a connection made with the
// certificate whose serial is given. Only the current certificate passes,
// or the one it replaced, which an agent still holds when a renewal's
// answer got lost. Anything older is ErrStaleCertificate: another machine
// may hold a copy of this server's old key.
func (a *Authority) CheckConnection(ctx context.Context, serverID, serial string) (store.Server, error) {
	s, err := a.CheckServer(ctx, serverID)
	if err != nil {
		return store.Server{}, err
	}
	if serial == "" || (serial != s.CertSerial && serial != s.PrevCertSerial) {
		return store.Server{}, ErrStaleCertificate
	}
	return s, nil
}

func (a *Authority) checkServer(ctx context.Context, tx *store.Tx, serverID string) (store.Server, error) {
	s, err := tx.GetServer(ctx, serverID)
	if errors.Is(err, store.ErrNotFound) {
		return store.Server{}, ErrUnknownServer
	}
	if err != nil {
		return store.Server{}, err
	}
	if s.RevokedAt != nil {
		return store.Server{}, ErrRevoked
	}
	return s, nil
}

// Revoke removes a server's right to connect.
func (a *Authority) Revoke(ctx context.Context, serverID, actor string) error {
	return a.db.Update(ctx, func(tx *store.Tx) error {
		if err := tx.RevokeServer(ctx, serverID, a.now()); err != nil {
			return err
		}
		return a.audit(ctx, tx, actor, store.SurfaceOf(actor), "server.revoke", serverID, serverID, "")
	})
}

// audit records an event in the audit log, from surface, that acted on
// target: the server, for a server's own events. A server joins with
// dockwell join, from the CLI, and renews its certificate on its own.
func (a *Authority) audit(ctx context.Context, tx *store.Tx, actor, surface, action, subject, target, detail string) error {
	return tx.RecordEvent(ctx, store.Event{
		ID: id.New(id.Event), At: a.now(), Actor: actor, Action: action, Subject: subject, Detail: detail, Surface: surface, Target: target,
	})
}

// ServerName turns a hostname into a server name: lowercase letters, digits
// and hyphens, at most 63 characters, the first DNS label only.
func ServerName(hostname string) string {
	label, _, _ := strings.Cut(strings.ToLower(strings.TrimSpace(hostname)), ".")
	var b strings.Builder
	for _, r := range label {
		switch {
		case r >= 'a' && r <= 'z', r >= '0' && r <= '9':
			b.WriteRune(r)
		case b.Len() > 0:
			b.WriteByte('-')
		}
	}
	name := strings.Trim(b.String(), "-")
	for strings.Contains(name, "--") {
		name = strings.ReplaceAll(name, "--", "-")
	}
	if len(name) > 56 { // room for a "-NN" suffix within 63
		name = strings.TrimRight(name[:56], "-")
	}
	if name == "" {
		return "server"
	}
	return name
}
