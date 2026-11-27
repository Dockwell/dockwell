// SPDX-License-Identifier: AGPL-3.0-only

package gitssh

import (
	"context"
	"crypto/subtle"
	"encoding/base64"
	"errors"
	"fmt"
	"net"
	"strings"
	"sync"
	"time"

	"golang.org/x/crypto/ssh"

	"github.com/dockwell/dockwell/internal/netguard"
)

const (
	dialTimeout      = 10 * time.Second
	handshakeTimeout = 10 * time.Second
	listTimeout      = 10 * time.Second
	fetchTimeout     = 2 * time.Minute
	idleReadTimeout  = 30 * time.Second
)

// Transport errors use plain product language and never echo untrusted remote text.
var (
	ErrHostKeyUnverified     = errors.New("the Git host's SSH key hasn't been accepted yet. Compare its fingerprint with the host's, accept it, then test again")
	ErrHostKeyChanged        = errors.New("the Git host presented a different SSH key than the one pinned, so Dockwell refused to connect. Ask an admin to verify the new fingerprint and accept it")
	ErrUnreachable           = errors.New("couldn't reach the Git host over SSH. Check the repository address and network, then try again")
	ErrAuth                  = errors.New("the Git host refused this deploy key, so Dockwell can't sign in. Add the public key to the repository's deploy keys, then test again")
	ErrRepositoryUnavailable = errors.New("the repository isn't there or this deploy key has no access to it. Check the address and that the key is added to this repository, then try again")
	ErrTimeout               = errors.New("the Git host didn't answer in time over SSH. Check the host and network, then try again")
	ErrTooLarge              = errors.New("the repository sent more data than Dockwell allows for a fetch")
)

// preferredKeys follows SFTP's host-key negotiation order (ADR 0112):
// Ed25519, ECDSA, then RSA with SHA-2.
var preferredKeys = []string{
	ssh.KeyAlgoED25519,
	ssh.KeyAlgoECDSA256,
	ssh.KeyAlgoECDSA384,
	ssh.KeyAlgoECDSA521,
	ssh.KeyAlgoRSASHA512,
	ssh.KeyAlgoRSASHA256,
}

// AlgorithmsFor returns the SSH HostKeyAlgorithms list for a pinned key type.
func AlgorithmsFor(keyType string) []string {
	switch keyType {
	case ssh.KeyAlgoED25519, ssh.KeyAlgoECDSA256, ssh.KeyAlgoECDSA384, ssh.KeyAlgoECDSA521:
		return []string{keyType}
	case ssh.KeyAlgoRSA:
		return []string{ssh.KeyAlgoRSASHA512, ssh.KeyAlgoRSASHA256}
	}
	return nil
}

// ValidFingerprint reports whether fp is a valid OpenSSH SHA256:... fingerprint.
func ValidFingerprint(fp string) bool {
	rest, ok := strings.CutPrefix(fp, "SHA256:")
	if !ok {
		return false
	}
	decoded, err := base64.RawStdEncoding.Strict().DecodeString(rest)
	return err == nil && len(decoded) == 32
}

// ValidAlgorithm reports whether algo is a supported SSH host key type.
func ValidAlgorithm(algo string) bool {
	return len(AlgorithmsFor(algo)) > 0
}

// Handshake reports the host key observed and verified before client auth.
type Handshake struct {
	Reached          bool
	Verified         bool
	SignedIn         bool
	Fingerprint      string
	HostKeyAlgorithm string
}

// HostKeyChange carries the newly observed host key when a pinned key changed.
type HostKeyChange struct {
	Fingerprint string
	Algorithm   string
}

func (e *HostKeyChange) Error() string {
	if e.Fingerprint == "" {
		return ErrHostKeyChanged.Error()
	}
	return fmt.Sprintf("%s (presented %s %s)", ErrHostKeyChanged, e.Algorithm, e.Fingerprint)
}

func (e *HostKeyChange) Unwrap() error { return ErrHostKeyChanged }

// Config carries the endpoint, pinned host key, and in-memory private key for
// one SSH Git operation.
type Config struct {
	Endpoint         Endpoint
	HostKeyAlgorithm string
	HostKeyPin       string
	PrivateKeyPEM    string
	// Local permits loopback addresses (tests only).
	Local bool
	// HandshakeTimeout overrides the default handshake timeout when > 0 (tests).
	HandshakeTimeout time.Duration
	// ListTimeout overrides the default ref-list timeout when > 0 (tests).
	ListTimeout time.Duration
	// FetchTimeout overrides the default pack-fetch timeout when > 0 (tests).
	FetchTimeout time.Duration
	// IdleTimeout overrides the default idle read timeout when > 0 (tests).
	IdleTimeout time.Duration
}

func (c Config) handshakeTimeout() time.Duration {
	if c.HandshakeTimeout > 0 {
		return c.HandshakeTimeout
	}
	return handshakeTimeout
}

func (c Config) listTimeout() time.Duration {
	if c.ListTimeout > 0 {
		return c.ListTimeout
	}
	return listTimeout
}

func (c Config) fetchTimeout() time.Duration {
	if c.FetchTimeout > 0 {
		return c.FetchTimeout
	}
	return fetchTimeout
}

func (c Config) idleTimeout() time.Duration {
	if c.IdleTimeout > 0 {
		return c.IdleTimeout
	}
	return idleReadTimeout
}

// sshConn wraps an ssh.Client and its underlying net.Conn so deadlines and
// context cancellation close the socket cleanly.
type sshConn struct {
	client *ssh.Client
	raw    net.Conn
	stop   func() bool
}

func (s *sshConn) Close() {
	if s == nil {
		return
	}
	if s.stop != nil {
		s.stop()
	}
	if s.client != nil {
		_ = s.client.Close()
	}
	if s.raw != nil {
		_ = s.raw.Close()
	}
}

// dialPinned dials the SSH server through netguard, verifies the pinned host
// key before sending client credentials, and handles host-key algorithm changes
// using an unauthenticated probe (following ADR 0112).
func dialPinned(ctx context.Context, cfg Config, dial netguard.DialFunc) (*sshConn, Handshake, error) {
	if dial == nil {
		opts := netguard.Options{Private: true, Loopback: cfg.Local}
		dial = netguard.Dialer(opts)
	}
	algorithms := AlgorithmsFor(cfg.HostKeyAlgorithm)
	if cfg.HostKeyAlgorithm == "" {
		algorithms = preferredKeys
	} else if len(algorithms) == 0 {
		return nil, Handshake{}, errors.New("the deploy key uses an unsupported host key type. Test the key to verify a supported host key")
	}

	conn, hs, err := dialOnce(ctx, cfg, dial, algorithms)
	// If pinned without an explicit algorithm, try the remaining preferred key
	// types while enforcing the exact same SHA-256 fingerprint pin.
	if cfg.HostKeyAlgorithm == "" && cfg.HostKeyPin != "" && errors.Is(err, ErrHostKeyChanged) {
		for _, algo := range preferredKeys {
			if algo == hs.HostKeyAlgorithm || ((algo == ssh.KeyAlgoRSASHA512 || algo == ssh.KeyAlgoRSASHA256) && hs.HostKeyAlgorithm == ssh.KeyAlgoRSA) {
				continue
			}
			if cand, seen, ferr := dialOnce(ctx, cfg, dial, []string{algo}); seen.Verified {
				return cand, seen, ferr
			}
		}
	}
	// If the server stopped offering the pinned host key type, probe on a fresh
	// connection with an empty pin (which aborts before authentication) so the
	// caller can report the newly observed host key.
	var neg *ssh.AlgorithmNegotiationError
	if cfg.HostKeyPin != "" && cfg.HostKeyAlgorithm != "" && errors.As(err, &neg) && neg.What == "host key" {
		probe := cfg
		probe.HostKeyPin, probe.HostKeyAlgorithm = "", ""
		_, seen, _ := dialOnce(ctx, probe, dial, preferredKeys)
		return nil, seen, &HostKeyChange{Fingerprint: seen.Fingerprint, Algorithm: seen.HostKeyAlgorithm}
	}
	if errors.As(err, &neg) {
		return nil, hs, ErrUnreachable
	}
	return conn, hs, err
}

func dialOnce(ctx context.Context, cfg Config, dial netguard.DialFunc, algorithms []string) (*sshConn, Handshake, error) {
	var hs Handshake

	// Parse private key only in memory and clear the raw buffer immediately.
	var authMethods []ssh.AuthMethod
	if cfg.PrivateKeyPEM != "" {
		raw := []byte(cfg.PrivateKeyPEM)
		signer, err := ssh.ParsePrivateKey(raw)
		clear(raw)
		if err != nil {
			return nil, hs, errors.New("the deploy key's private half couldn't be opened")
		}
		authMethods = []ssh.AuthMethod{ssh.PublicKeys(signer)}
	}

	dialCtx, cancelDial := context.WithTimeout(ctx, min(dialTimeout, cfg.handshakeTimeout()))
	defer cancelDial()
	rawConn, err := dial(dialCtx, "tcp", cfg.Endpoint.Address())
	if err != nil {
		var refused *netguard.Refused
		if errors.As(err, &refused) || errors.Is(err, netguard.ErrForbidden) {
			return nil, hs, err
		}
		if errors.Is(err, context.DeadlineExceeded) || errors.Is(dialCtx.Err(), context.DeadlineExceeded) {
			return nil, hs, ErrTimeout
		}
		return nil, hs, fmt.Errorf("%w: %s", ErrUnreachable, netguard.Words(err))
	}
	hs.Reached = true

	// Bind raw socket close to ctx cancellation so blocked handshakes or reads
	// abort promptly.
	stop := context.AfterFunc(ctx, func() {
		_ = rawConn.SetDeadline(time.Now())
		_ = rawConn.Close()
	})
	cleanup := func() {
		stop()
		_ = rawConn.Close()
	}

	_ = rawConn.SetDeadline(time.Now().Add(cfg.handshakeTimeout()))

	var hostKeyErr error
	var mu sync.Mutex
	sshCfg := &ssh.ClientConfig{
		User:              cfg.Endpoint.User,
		Auth:              authMethods,
		HostKeyAlgorithms: algorithms,
		Timeout:           cfg.handshakeTimeout(),
		HostKeyCallback: func(_ string, _ net.Addr, key ssh.PublicKey) error {
			mu.Lock()
			defer mu.Unlock()
			fp := ssh.FingerprintSHA256(key)
			algo := key.Type()
			hs.Fingerprint = fp
			hs.HostKeyAlgorithm = algo
			if cfg.HostKeyPin == "" {
				hostKeyErr = ErrHostKeyUnverified
				return hostKeyErr
			}
			if subtle.ConstantTimeCompare([]byte(fp), []byte(cfg.HostKeyPin)) != 1 {
				hostKeyErr = &HostKeyChange{Fingerprint: fp, Algorithm: algo}
				return hostKeyErr
			}
			hs.Verified = true
			return nil
		},
	}

	cconn, chans, reqs, err := ssh.NewClientConn(rawConn, cfg.Endpoint.Address(), sshCfg)
	if err != nil {
		cleanup()
		mu.Lock()
		hErr := hostKeyErr
		mu.Unlock()
		if hErr != nil {
			return nil, hs, hErr
		}
		var neg *ssh.AlgorithmNegotiationError
		if errors.As(err, &neg) {
			return nil, hs, err
		}
		if errors.Is(ctx.Err(), context.DeadlineExceeded) || isNetTimeout(err) {
			return nil, hs, ErrTimeout
		}
		if hs.Verified && isAuthError(err) {
			return nil, hs, ErrAuth
		}
		return nil, hs, ErrUnreachable
	}
	hs.SignedIn = true
	// Clear handshake deadline; subsequent operations set their own deadlines.
	_ = rawConn.SetDeadline(time.Time{})

	// Reject every incoming server-initiated channel (no agent forwarding, no
	// port forwarding, no other channel types).
	rejectChans := make(chan ssh.NewChannel)
	go func() {
		for ch := range chans {
			_ = ch.Reject(ssh.Prohibited, "channel type not allowed")
		}
		close(rejectChans)
	}()
	// Reject global requests from the server.
	rejectReqs := make(chan *ssh.Request)
	go func() {
		for req := range reqs {
			if req.WantReply {
				_ = req.Reply(false, nil)
			}
		}
		close(rejectReqs)
	}()

	client := ssh.NewClient(cconn, rejectChans, rejectReqs)
	return &sshConn{client: client, raw: rawConn, stop: stop}, hs, nil
}

func isAuthError(err error) bool {
	if err == nil {
		return false
	}
	msg := err.Error()
	return strings.Contains(msg, "unable to authenticate") || strings.Contains(msg, "no supported methods remain")
}

func isNetTimeout(err error) bool {
	var ne net.Error
	return errors.As(err, &ne) && ne.Timeout()
}
