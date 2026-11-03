// SPDX-License-Identifier: AGPL-3.0-only

// Package enroll gives a server's agent its identity: it joins the control
// plane with a ticket, keeps its key and certificates on disk, and renews
// its certificate before it expires.
//
// The private key is made on the server and never leaves it; the control
// plane only ever sees a certificate request. The first connection is
// verified against the CA pin in the ticket, and every later one against
// the CA certificate the control plane returned.
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
	"net/http"
	"net/url"
	"os"
	"path/filepath"
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
	configFile = "agent.json"
	keyFile    = "server.key"
	certFile   = "server.crt"
	caFile     = "ca.crt"
)

// ErrNotJoined means the agent's directory holds no identity yet.
var ErrNotJoined = errors.New("this server hasn't joined a control plane. Run dockwell join with a ticket from the control server")

// Identity is who this server is, and how it reaches its control plane.
type Identity struct {
	ServerID   string
	ServerName string
	ControlURL string
	Key        *ecdsa.PrivateKey
	Cert       *x509.Certificate
	CA         *x509.Certificate
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
	u, err := url.Parse(tk.GetControlUrl())
	if err != nil {
		return nil, err
	}
	client := agentv1connect.NewAgentServiceClient(httpClient(&tls.Config{
		MinVersion:         tls.VersionTLS13,
		ServerName:         u.Hostname(),
		InsecureSkipVerify: true, //nolint:gosec // VerifyConnection checks the chain against the ticket's CA pin
		VerifyConnection:   pki.VerifyPinned(tk.GetCaPin()),
	}), tk.GetControlUrl(), connect.WithGRPC())

	res, err := client.Join(ctx, connect.NewRequest(&agentv1.JoinRequest{
		TokenId:  tk.GetTokenId(),
		Secret:   tk.GetSecret(),
		Csr:      csr,
		Hostname: hostname,
		Build:    build.Proto(),
	}))
	if err != nil {
		return nil, joinError(tk.GetControlUrl(), err)
	}
	cert, err := x509.ParseCertificate(res.Msg.GetCertificate())
	if err != nil {
		return nil, fmt.Errorf("the control plane sent an unreadable certificate: %w", err)
	}
	ca, err := x509.ParseCertificate(res.Msg.GetCaCertificate())
	if err != nil {
		return nil, fmt.Errorf("the control plane sent an unreadable CA certificate: %w", err)
	}
	if pki.Pin(ca) != tk.GetCaPin() {
		return nil, errors.New("the control plane returned a CA that doesn't match the ticket; not joining")
	}
	if !cert.PublicKey.(*ecdsa.PublicKey).Equal(&key.PublicKey) { //nolint:forcetypeassert // the CA only signs ECDSA keys
		return nil, errors.New("the control plane signed a different key than this server sent; not joining")
	}
	return &Identity{
		ServerID:   res.Msg.GetServerId(),
		ServerName: res.Msg.GetServerName(),
		ControlURL: tk.GetControlUrl(),
		Key:        key,
		Cert:       cert,
		CA:         ca,
	}, nil
}

func joinError(controlURL string, err error) error {
	var cerr *connect.Error
	if errors.As(err, &cerr) {
		switch cerr.Code() {
		case connect.CodePermissionDenied, connect.CodeInvalidArgument:
			return errors.New(cerr.Message())
		case connect.CodeUnavailable:
			return fmt.Errorf("can't reach the control plane at %s: %s. Check that port is open from this server", controlURL, cerr.Message())
		default:
		}
	}
	return fmt.Errorf("joining the control plane at %s failed: %w", controlURL, err)
}

// Save writes the identity to dir. The key is only readable by its owner.
func (i *Identity) Save(dir string) error {
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return fmt.Errorf("can't create %s: %w", dir, err)
	}
	keyDER, err := x509.MarshalPKCS8PrivateKey(i.Key)
	if err != nil {
		return err
	}
	cfg, err := json.MarshalIndent(config{ServerID: i.ServerID, ServerName: i.ServerName, ControlURL: i.ControlURL, JoinedAt: time.Now().UTC()}, "", "  ")
	if err != nil {
		return err
	}
	// The config goes last: a crash midway leaves no half-joined identity.
	for _, f := range []struct {
		name string
		data []byte
		mode os.FileMode
	}{
		{keyFile, pem.EncodeToMemory(&pem.Block{Type: "PRIVATE KEY", Bytes: keyDER}), 0o600},
		{certFile, pki.EncodeCertPEM(i.Cert), 0o644},
		{caFile, pki.EncodeCertPEM(i.CA), 0o644},
		{configFile, append(cfg, '\n'), 0o600},
	} {
		if err := writeFileAtomic(filepath.Join(dir, f.name), f.data, f.mode); err != nil {
			return err
		}
	}
	return nil
}

// Load reads an identity saved with Save. It returns ErrNotJoined if dir
// has none.
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
	keyPEM, err := root.ReadFile(keyFile)
	if err != nil {
		return nil, err
	}
	block, _ := pem.Decode(keyPEM)
	if block == nil {
		return nil, fmt.Errorf("%s isn't a PEM key", filepath.Join(dir, keyFile))
	}
	parsed, err := x509.ParsePKCS8PrivateKey(block.Bytes)
	if err != nil {
		return nil, err
	}
	key, ok := parsed.(*ecdsa.PrivateKey)
	if !ok {
		return nil, errors.New("the server key isn't an ECDSA key")
	}
	certPEM, err := root.ReadFile(certFile)
	if err != nil {
		return nil, err
	}
	cert, err := pki.ParseCertPEM(certPEM)
	if err != nil {
		return nil, err
	}
	caPEM, err := root.ReadFile(caFile)
	if err != nil {
		return nil, err
	}
	ca, err := pki.ParseCertPEM(caPEM)
	if err != nil {
		return nil, err
	}
	return &Identity{ServerID: cfg.ServerID, ServerName: cfg.ServerName, ControlURL: cfg.ControlURL, Key: key, Cert: cert, CA: ca}, nil
}

// ClientTLS is the TLS configuration for talking to the control plane:
// this server's certificate, and only the install's CA trusted.
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
		Certificates: []tls.Certificate{{
			Certificate: [][]byte{i.Cert.Raw},
			PrivateKey:  i.Key,
			Leaf:        i.Cert,
		}},
	}, nil
}

// NeedsRenewal reports whether the certificate is two thirds through its life.
func (i *Identity) NeedsRenewal(now time.Time) bool { return !now.Before(pki.RenewAt(i.Cert)) }

// Renew asks the control plane for a fresh certificate for the same key,
// proving who this server is with the current one.
func (i *Identity) Renew(ctx context.Context) error {
	cfg, err := i.ClientTLS()
	if err != nil {
		return err
	}
	csr, err := pki.CSR(i.Key)
	if err != nil {
		return err
	}
	client := agentv1connect.NewAgentServiceClient(httpClient(cfg), i.ControlURL, connect.WithGRPC())
	res, err := client.RenewCertificate(ctx, connect.NewRequest(&agentv1.RenewCertificateRequest{Csr: csr}))
	if err != nil {
		return fmt.Errorf("renewing the certificate failed: %w", err)
	}
	cert, err := x509.ParseCertificate(res.Msg.GetCertificate())
	if err != nil {
		return err
	}
	i.Cert = cert
	return nil
}

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
	}}
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
