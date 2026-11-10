// SPDX-License-Identifier: AGPL-3.0-only

// Package enroll gives a server's agent its identity: it joins the control
// plane with a ticket, keeps its key and certificates on disk, and renews
// its certificate before it expires.
//
// The private key is made on the server and never leaves it; the control
// plane only ever sees a certificate request. The first connection is
// verified against the CA pin in the ticket, and every later one against
// the CA certificate the control plane returned.
//
// Every renewal brings a new key. The next key is written to disk before
// the request goes out, so a crash or a lost answer retries with the same
// key, which the control plane recognises. A copy of an old key is useless
// once the server has renewed.
package enroll

import (
	"context"
	"crypto/ecdsa"
	"crypto/tls"
	"crypto/x509"
	"encoding/json"
	"encoding/pem"
	"errors"
	"fmt"
	"io/fs"
	"net"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"sync"
	"time"

	"connectrpc.com/connect"

	agentv1 "github.com/dockwell/dockwell/gen/dockwell/agent/v1"
	"github.com/dockwell/dockwell/gen/dockwell/agent/v1/agentv1connect"
	"github.com/dockwell/dockwell/internal/buildinfo"
	"github.com/dockwell/dockwell/internal/pki"
	"github.com/dockwell/dockwell/internal/ticket"
)

// Files in the agent's directory.
const (
	configFile  = "agent.json"
	keyFile     = "server.key"
	nextKeyFile = "server.key.next"
	certFile    = "server.crt"
	caFile      = "ca.crt"
)

// ErrNotJoined means the agent's directory holds no identity yet.
var ErrNotJoined = errors.New("this server hasn't joined a control plane. Run dockwell join with a ticket from the control server")

// Identity is who this server is, and how it reaches its control plane.
// It is safe for concurrent use once loaded.
type Identity struct {
	ServerID   string
	ServerName string
	ControlURL string
	JoinedAt   time.Time
	CA         *x509.Certificate

	mu      sync.Mutex
	key     *ecdsa.PrivateKey
	cert    *x509.Certificate
	next    *ecdsa.PrivateKey // a renewal's key, kept until the renewal lands
	dir     string
	renewed chan struct{} // closed when the certificate is next replaced
}

// New makes an identity from its parts, for the control server's own agent,
// which enrolls without a ticket.
func New(serverID, serverName, controlURL string, key *ecdsa.PrivateKey, cert, ca *x509.Certificate) *Identity {
	return &Identity{
		ServerID: serverID, ServerName: serverName, ControlURL: controlURL, JoinedAt: time.Now().UTC(),
		CA: ca, key: key, cert: cert,
	}
}

// Cert is the current certificate.
func (i *Identity) Cert() *x509.Certificate {
	i.mu.Lock()
	defer i.mu.Unlock()
	return i.cert
}

// Key is the current private key.
func (i *Identity) Key() *ecdsa.PrivateKey {
	i.mu.Lock()
	defer i.mu.Unlock()
	return i.key
}

// Replace installs a renewed key and certificate, and tells whoever waits
// on Renewed.
func (i *Identity) Replace(key *ecdsa.PrivateKey, cert *x509.Certificate) {
	i.mu.Lock()
	defer i.mu.Unlock()
	i.key, i.cert, i.next = key, cert, nil
	if i.renewed != nil {
		close(i.renewed)
		i.renewed = nil
	}
}

// Renewed is closed the next time the certificate is replaced. A connection
// keeps the certificate it was made with, so a long-lived stream watches
// this to move to the new one.
func (i *Identity) Renewed() <-chan struct{} {
	i.mu.Lock()
	defer i.mu.Unlock()
	if i.renewed == nil {
		i.renewed = make(chan struct{})
	}
	return i.renewed
}

type config struct {
	ServerID   string    `json:"server_id"`
	ServerName string    `json:"server_name"`
	ControlURL string    `json:"control_url"`
	JoinedAt   time.Time `json:"joined_at"`
}

// Join spends a ticket: it makes a key, asks the control plane in the
// ticket to sign it, and returns the new identity. Nothing is written to
// disk; call Save.
func Join(ctx context.Context, ticketString, hostname string, build buildinfo.Info) (*Identity, error) {
	tk, err := ticket.Decode(ticketString)
	if err != nil {
		return nil, err
	}
	key, csr, err := pki.NewServerKey()
	if err != nil {
		return nil, err
	}
	// A ticket spent stays spent, so only what never reached the control plane goes
	// on to the next address. That's how a server on the control server's own network
	// joins when its public address can't be come back in on.
	req := &agentv1.JoinRequest{TokenId: tk.GetTokenId(), Secret: tk.GetSecret(), Csr: csr, Hostname: hostname, Build: build.Proto()}
	addresses := append([]string{tk.GetControlUrl()}, tk.GetOtherUrls()...)
	var res *connect.Response[agentv1.JoinResponse]
	var controlURL string
	var unreached []error
	for i, at := range addresses {
		controlURL = at
		if res, err = joinAt(ctx, tk.GetCaPin(), at, req, i < len(addresses)-1); !neverArrived(err) {
			break
		}
		unreached = append(unreached, rpcError(at, "joining", err))
	}
	if neverArrived(err) {
		return nil, errors.Join(unreached...)
	}
	if err != nil {
		return nil, rpcError(controlURL, "joining", err)
	}
	ca, err := x509.ParseCertificate(res.Msg.GetCaCertificate())
	if err != nil {
		return nil, fmt.Errorf("the control plane sent an unreadable CA certificate: %w", err)
	}
	if pki.Pin(ca) != tk.GetCaPin() {
		return nil, errors.New("the control plane returned a CA that doesn't match the ticket; not joining")
	}
	cert, err := checkIssued(res.Msg.GetCertificate(), key, ca)
	if err != nil {
		return nil, err
	}
	return New(res.Msg.GetServerId(), res.Msg.GetServerName(), controlURL, key, cert, ca), nil
}

// joinDial and joinHandshake bound reaching one of a ticket's addresses when there's
// another to go on to, as a firewall that drops packets otherwise holds the join for
// minutes. Only reaching it is bounded: once the control plane has the ticket, it may
// take its time, as a server that went on to the next address would find it spent.
var (
	joinDial      = 10 * time.Second
	joinHandshake = 10 * time.Second
)

// joinAt asks the control plane at controlURL to sign a joining server's key.
func joinAt(ctx context.Context, pin, controlURL string, req *agentv1.JoinRequest, more bool) (*connect.Response[agentv1.JoinResponse], error) {
	u, err := url.Parse(controlURL)
	if err != nil {
		return nil, err
	}
	hc := httpClient(&tls.Config{
		MinVersion:         tls.VersionTLS13,
		ServerName:         u.Hostname(),
		InsecureSkipVerify: true, //nolint:gosec // VerifyConnection checks the chain against the ticket's CA pin
		VerifyConnection:   pki.VerifyPinned(pin),
	})
	defer hc.CloseIdleConnections()
	if more {
		if t, ok := hc.Transport.(*http.Transport); ok {
			t.DialContext = (&net.Dialer{Timeout: joinDial}).DialContext
			t.TLSHandshakeTimeout = joinHandshake
		}
	}
	return agentv1connect.NewAgentServiceClient(hc, controlURL, connect.WithGRPC()).Join(ctx, connect.NewRequest(req))
}

// neverArrived reports a join that didn't reach the control plane at that address.
func neverArrived(err error) bool {
	code := connect.CodeOf(err)
	return err != nil && (code == connect.CodeUnavailable || code == connect.CodeDeadlineExceeded)
}

// checkIssued verifies a certificate the control plane issued: it must be
// for key, and a client certificate from ca.
func checkIssued(der []byte, key *ecdsa.PrivateKey, ca *x509.Certificate) (*x509.Certificate, error) {
	cert, err := x509.ParseCertificate(der)
	if err != nil {
		return nil, fmt.Errorf("the control plane sent an unreadable certificate: %w", err)
	}
	pub, ok := cert.PublicKey.(*ecdsa.PublicKey)
	if !ok || !pub.Equal(&key.PublicKey) {
		return nil, errors.New("the control plane signed a different key than this server sent; keeping the current certificate")
	}
	pool := x509.NewCertPool()
	pool.AddCert(ca)
	if _, err := cert.Verify(x509.VerifyOptions{Roots: pool, KeyUsages: []x509.ExtKeyUsage{x509.ExtKeyUsageClientAuth}}); err != nil {
		return nil, fmt.Errorf("the control plane sent a certificate that doesn't verify: %w; keeping the current certificate", err)
	}
	return cert, nil
}

func rpcError(controlURL, doing string, err error) error {
	var cerr *connect.Error
	if errors.As(err, &cerr) {
		switch cerr.Code() {
		case connect.CodePermissionDenied, connect.CodeInvalidArgument:
			return fmt.Errorf("%s failed: %w", doing, errors.New(cerr.Message()))
		case connect.CodeUnavailable:
			return fmt.Errorf("can't reach the control plane at %s: %s. Check that its port is open from this server", controlURL, cerr.Message())
		default:
		}
	}
	return fmt.Errorf("%s with the control plane at %s failed: %w", doing, controlURL, err)
}

// Save writes the identity to dir. Keys are only readable by their owner.
// The config goes last, so a crash midway leaves no half-joined identity.
func (i *Identity) Save(dir string) error {
	i.mu.Lock()
	key, cert, next := i.key, i.cert, i.next
	i.dir = dir
	i.mu.Unlock()

	if err := os.MkdirAll(dir, 0o700); err != nil {
		return fmt.Errorf("can't create %s: %w", dir, err)
	}
	keyPEM, err := encodeKey(key)
	if err != nil {
		return err
	}
	cfg, err := json.MarshalIndent(config{ServerID: i.ServerID, ServerName: i.ServerName, ControlURL: i.ControlURL, JoinedAt: i.JoinedAt}, "", "  ")
	if err != nil {
		return err
	}
	for _, f := range []struct {
		name string
		data []byte
		mode os.FileMode
	}{
		{keyFile, keyPEM, 0o600},
		{certFile, pki.EncodeCertPEM(cert), 0o644},
		{caFile, pki.EncodeCertPEM(i.CA), 0o644},
		{configFile, append(cfg, '\n'), 0o600},
	} {
		if err := writeFileAtomic(filepath.Join(dir, f.name), f.data, f.mode); err != nil {
			return err
		}
	}
	if next == nil {
		if err := os.Remove(filepath.Join(dir, nextKeyFile)); err != nil && !errors.Is(err, fs.ErrNotExist) {
			return err
		}
	}
	return nil
}

// Load reads an identity saved with Save, including a renewal key that
// was made but not yet used. It returns ErrNotJoined if dir has none.
func Load(dir string) (*Identity, error) {
	root, err := os.OpenRoot(dir)
	if errors.Is(err, fs.ErrNotExist) {
		return nil, ErrNotJoined
	}
	if err != nil {
		return nil, err
	}
	defer root.Close()
	raw, err := root.ReadFile(configFile)
	if errors.Is(err, fs.ErrNotExist) {
		return nil, ErrNotJoined
	}
	if err != nil {
		return nil, err
	}
	var cfg config
	if err := json.Unmarshal(raw, &cfg); err != nil {
		return nil, fmt.Errorf("can't read %s: %w", filepath.Join(dir, configFile), err)
	}
	key, err := readKey(root, keyFile)
	if err != nil {
		return nil, err
	}
	var next *ecdsa.PrivateKey
	if _, err := root.Stat(nextKeyFile); err == nil {
		if next, err = readKey(root, nextKeyFile); err != nil {
			return nil, err
		}
	}
	cert, err := readCert(root, certFile)
	if err != nil {
		return nil, err
	}
	ca, err := readCert(root, caFile)
	if err != nil {
		return nil, err
	}
	return &Identity{
		ServerID: cfg.ServerID, ServerName: cfg.ServerName, ControlURL: cfg.ControlURL, JoinedAt: cfg.JoinedAt, CA: ca,
		key: key, cert: cert, next: next, dir: dir,
	}, nil
}

// NeedsRenewal reports whether the certificate is two thirds through its life.
func (i *Identity) NeedsRenewal(now time.Time) bool { return !now.Before(pki.RenewAt(i.Cert())) }

// NextKey returns the key for the next renewal, making and saving one if
// there isn't one yet, so a retry after a crash or a lost answer uses the
// same key.
func (i *Identity) NextKey() (*ecdsa.PrivateKey, error) {
	i.mu.Lock()
	defer i.mu.Unlock()
	if i.next != nil {
		return i.next, nil
	}
	key, _, err := pki.NewServerKey()
	if err != nil {
		return nil, err
	}
	if i.dir != "" {
		keyPEM, err := encodeKey(key)
		if err != nil {
			return nil, err
		}
		if err := writeFileAtomic(filepath.Join(i.dir, nextKeyFile), keyPEM, 0o600); err != nil {
			return nil, err
		}
	}
	i.next = key
	return key, nil
}

// Renew asks the control plane for a certificate for a new key, proving
// who this server is with the current certificate. Call Save afterwards.
func (i *Identity) Renew(ctx context.Context) error {
	next, err := i.NextKey()
	if err != nil {
		return err
	}
	csr, err := pki.CSR(next)
	if err != nil {
		return err
	}
	// A connection of its own, so the renewal presents the current
	// certificate even while the stream holds an older connection open.
	service, release, err := i.Client()
	if err != nil {
		return err
	}
	defer release()
	res, err := service.RenewCertificate(ctx, connect.NewRequest(&agentv1.RenewCertificateRequest{Csr: csr}))
	if err != nil {
		return rpcError(i.ControlURL, "renewing the certificate", err)
	}
	cert, err := checkIssued(res.Msg.GetCertificate(), next, i.CA)
	if err != nil {
		return err
	}
	i.Replace(next, cert)
	return nil
}

// ClientTLS is the TLS configuration for talking to the control plane:
// this server's current certificate, and only the install's CA trusted.
func (i *Identity) ClientTLS() (*tls.Config, error) {
	u, err := url.Parse(i.ControlURL)
	if err != nil {
		return nil, err
	}
	pool := x509.NewCertPool()
	pool.AddCert(i.CA)
	return &tls.Config{
		MinVersion: tls.VersionTLS13,
		ServerName: u.Hostname(),
		RootCAs:    pool,
		GetClientCertificate: func(*tls.CertificateRequestInfo) (*tls.Certificate, error) {
			i.mu.Lock()
			defer i.mu.Unlock()
			return &tls.Certificate{Certificate: [][]byte{i.cert.Raw}, PrivateKey: i.key, Leaf: i.cert}, nil
		},
	}, nil
}

// Client is a client for the control plane on connections of its own,
// which authenticate with the certificate current when each is made. Call
// release when done with it: TLS fixes a connection's certificate at the
// handshake, so a renewed certificate only reaches the control plane on
// connections made after it.
func (i *Identity) Client() (client agentv1connect.AgentServiceClient, release func(), err error) {
	cfg, err := i.ClientTLS()
	if err != nil {
		return nil, nil, err
	}
	hc := httpClient(cfg)
	return agentv1connect.NewAgentServiceClient(hc, i.ControlURL, connect.WithGRPC()), hc.CloseIdleConnections, nil
}

// TransferClient is Client for uploads and downloads, whose connection takes in
// at most transferWindow of each stream, and transferConnWindow in all, before the
// agent reads it. Go's own is 4 MiB a stream, and three uploads at once, each
// waiting on a disk, took an agent past its 40 MB. A frame is 128 KiB, so this
// still has two on their way to each.
func (i *Identity) TransferClient() (client agentv1connect.AgentServiceClient, release func(), err error) {
	cfg, err := i.ClientTLS()
	if err != nil {
		return nil, nil, err
	}
	hc := httpClient(cfg)
	hc.Transport.(*http.Transport).HTTP2 = &http.HTTP2Config{ //nolint:forcetypeassert // httpClient's own
		MaxReceiveBufferPerStream:     transferWindow,
		MaxReceiveBufferPerConnection: transferConnWindow,
	}
	return agentv1connect.NewAgentServiceClient(hc, i.ControlURL, connect.WithGRPC()), hc.CloseIdleConnections, nil
}

const (
	transferWindow     = 256 << 10
	transferConnWindow = 1 << 20
)

// httpClient speaks HTTP/2 over TLS, through a proxy if the environment
// names one, as gRPC requires.
func httpClient(cfg *tls.Config) *http.Client {
	var p http.Protocols
	p.SetHTTP2(true)
	return &http.Client{Transport: &http.Transport{
		Proxy:               http.ProxyFromEnvironment,
		TLSClientConfig:     cfg,
		Protocols:           &p,
		TLSHandshakeTimeout: 10 * time.Second,
		IdleConnTimeout:     90 * time.Second,
	}}
}

func encodeKey(key *ecdsa.PrivateKey) ([]byte, error) {
	der, err := x509.MarshalPKCS8PrivateKey(key)
	if err != nil {
		return nil, err
	}
	return pem.EncodeToMemory(&pem.Block{Type: "PRIVATE KEY", Bytes: der}), nil
}

func readKey(root *os.Root, name string) (*ecdsa.PrivateKey, error) {
	b, err := root.ReadFile(name)
	if err != nil {
		return nil, err
	}
	block, _ := pem.Decode(b)
	if block == nil {
		return nil, fmt.Errorf("%s isn't a PEM key", name)
	}
	parsed, err := x509.ParsePKCS8PrivateKey(block.Bytes)
	if err != nil {
		return nil, fmt.Errorf("can't read %s: %w", name, err)
	}
	key, ok := parsed.(*ecdsa.PrivateKey)
	if !ok {
		return nil, fmt.Errorf("%s isn't an ECDSA key", name)
	}
	return key, nil
}

func readCert(root *os.Root, name string) (*x509.Certificate, error) {
	b, err := root.ReadFile(name)
	if err != nil {
		return nil, err
	}
	return pki.ParseCertPEM(b)
}

func writeFileAtomic(path string, data []byte, mode os.FileMode) error {
	tmp, err := os.CreateTemp(filepath.Dir(path), "."+filepath.Base(path)+".*")
	if err != nil {
		return err
	}
	defer func() { _ = os.Remove(tmp.Name()) }()
	if err := tmp.Chmod(mode); err != nil {
		_ = tmp.Close()
		return err
	}
	if _, err := tmp.Write(data); err != nil {
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
	return os.Rename(tmp.Name(), path)
}
