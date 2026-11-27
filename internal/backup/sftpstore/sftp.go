// SPDX-License-Identifier: AGPL-3.0-only

// Package sftpstore transfers sealed backup files over a pinned SSH connection.
package sftpstore

import (
	"cmp"
	"context"
	"crypto/rand"
	"crypto/subtle"
	"encoding/base64"
	"errors"
	"fmt"
	"io"
	"math"
	"net"
	"os"
	"path"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"time"
	"unicode"

	"github.com/pkg/sftp"
	"golang.org/x/crypto/ssh"

	"github.com/dockwell/dockwell/internal/netguard"
)

const (
	handshakeTimeout  = 10 * time.Second
	idleTimeout       = 2 * time.Minute
	keepaliveInterval = 30 * time.Second
	uploadRequests    = 8
	downloadRequests  = 4
	partialPrefix     = ".dockwell-partial-"
)

// ErrHostKeyUnverified and the other transport errors give callers safe words
// about failures without passing through any server-supplied text.
var (
	ErrHostKeyUnverified = errors.New("the server's host key needs verification before signing in. Compare the fingerprint with the server's, then trust it and test again")
	ErrHostKeyChanged    = errors.New("the SFTP server presented a different host key, so Dockwell refused to sign in. Verify the new fingerprint with the server owner, then update the destination")
	ErrUnreachable       = errors.New("the SFTP server didn't complete an SSH connection. Check the host, port and network, then try again")
	ErrAuth              = errors.New("the SFTP server didn't accept the sign-in. Check the user and its SSH key or password, then test again")
	ErrSubsystem         = errors.New("the SSH server didn't open SFTP. Enable SFTP for this user, then test again")
	ErrFolder            = errors.New("the SFTP folder couldn't be opened or created. Check the folder and the user's permissions, then try again")
	ErrWrite             = errors.New("the SFTP write didn't finish, so Dockwell couldn't confirm the copy. Check the connection and storage permissions")
	ErrRead              = errors.New("the SFTP read didn't finish")
	ErrDelete            = errors.New("the SFTP file couldn't be deleted. Check the connection and the user's delete permission, then try again")
	ErrLocalIdle         = errors.New("the transfer stopped because this server's disk stopped moving backup data. Check the disk on this server, then try again")
	ErrIdle              = errors.New("the transfer stopped because the SFTP server stopped answering. Check the server, its free space and the network, then try again")
)

// Config is the endpoint and its credentials, held for a single call.
type Config struct {
	Host, User, Path, Fingerprint, HostKeyAlgorithm, PrivateKey, Password, KeyPassphrase string
	Port                                                                                 uint32
	// KeyTypesDir holds learned types for legacy pins, under the agent data directory.
	KeyTypesDir string
}

// Address joins the host and effective SSH port.
func (c Config) Address() string {
	port := c.Port
	if port == 0 {
		port = 22
	}
	return net.JoinHostPort(c.Host, strconv.FormatUint(uint64(port), 10))
}

// Validate checks the connection settings without contacting the server.
func (c Config) Validate(requireHostKey bool) error {
	if !validHost(c.Host) || c.Port > 65535 {
		return errors.New("enter an SFTP host such as example.com and a port from 1 to 65535")
	}
	if c.User == "" || len(c.User) > 255 || strings.IndexFunc(c.User, unicode.IsControl) >= 0 {
		return errors.New("enter the SFTP user's name")
	}
	if !path.IsAbs(c.Path) || len(c.Path) > 1024 || strings.IndexFunc(c.Path, unicode.IsControl) >= 0 {
		return errors.New("enter an absolute SFTP folder, such as /backups")
	}
	if requireHostKey || c.Fingerprint != "" {
		fingerprint, ok := strings.CutPrefix(c.Fingerprint, "SHA256:")
		decoded, err := base64.RawStdEncoding.Strict().DecodeString(fingerprint)
		if !ok || err != nil || len(decoded) != 32 {
			return errors.New("enter the verified SHA256 fingerprint of the SFTP server's host key")
		}
	}
	if c.HostKeyAlgorithm != "" && len(algorithmsFor(c.HostKeyAlgorithm)) == 0 {
		return errors.New("enter a supported SFTP host key type")
	}
	_, err := c.auth()
	return err
}

func validHost(host string) bool {
	if net.ParseIP(host) != nil {
		return true
	}
	host = strings.TrimSuffix(host, ".")
	if host == "" || len(host) > 253 {
		return false
	}
	for label := range strings.SplitSeq(host, ".") {
		if label == "" || len(label) > 63 || label[0] == '-' || label[len(label)-1] == '-' {
			return false
		}
		for _, ch := range label {
			if (ch < 'a' || ch > 'z') && (ch < 'A' || ch > 'Z') && (ch < '0' || ch > '9') && ch != '-' {
				return false
			}
		}
	}
	return true
}

func (c Config) auth() ([]ssh.AuthMethod, error) {
	switch {
	case c.Password != "" && c.PrivateKey == "" && c.KeyPassphrase == "" && len(c.Password) <= 4096:
		answered := false
		return []ssh.AuthMethod{ssh.Password(c.Password), ssh.KeyboardInteractive(func(_, _ string, questions []string, echo []bool) ([]string, error) {
			if len(questions) == 0 && len(echo) == 0 {
				return []string{}, nil
			}
			if answered || len(questions) != 1 || len(echo) != 1 || echo[0] {
				return nil, ErrAuth
			}
			answered = true
			return []string{c.Password}, nil
		})}, nil
	case c.PrivateKey == "" || c.Password != "" || len(c.PrivateKey) > 64<<10 || len(c.KeyPassphrase) > 4096:
		return nil, errors.New("give one SSH private key or a password for this SFTP user")
	}
	signer, err := c.signer()
	if err != nil {
		return nil, err
	}
	return []ssh.AuthMethod{ssh.PublicKeys(signer)}, nil
}

// PublicKey derives the OpenSSH authorized_keys entry from the submitted private key.
func (c Config) PublicKey() (string, error) {
	if c.PrivateKey == "" {
		return "", nil
	}
	signer, err := c.signer()
	if err != nil {
		return "", err
	}
	return strings.TrimSpace(string(ssh.MarshalAuthorizedKey(signer.PublicKey()))), nil
}

func (c Config) signer() (ssh.Signer, error) {
	var signer ssh.Signer
	var err error
	if c.KeyPassphrase == "" {
		signer, err = ssh.ParsePrivateKey([]byte(c.PrivateKey))
	} else {
		signer, err = ssh.ParsePrivateKeyWithPassphrase([]byte(c.PrivateKey), []byte(c.KeyPassphrase))
	}
	if err != nil {
		return nil, errors.New("the SSH private key couldn't be opened. Check the key and its passphrase")
	}
	return signer, nil
}

var preferredKeys = []string{ssh.KeyAlgoED25519, ssh.KeyAlgoECDSA256, ssh.KeyAlgoECDSA384, ssh.KeyAlgoECDSA521, ssh.KeyAlgoRSASHA512, ssh.KeyAlgoRSASHA256}

func algorithmsFor(key string) []string {
	switch key {
	case ssh.KeyAlgoED25519, ssh.KeyAlgoECDSA256, ssh.KeyAlgoECDSA384, ssh.KeyAlgoECDSA521:
		return []string{key}
	case ssh.KeyAlgoRSA:
		return []string{ssh.KeyAlgoRSASHA512, ssh.KeyAlgoRSASHA256}
	}
	return nil
}

// Handshake reports what happened before the file transfer subsystem opened.
type Handshake struct {
	Reached, Verified, SignedIn   bool
	Fingerprint, HostKeyAlgorithm string
}

// HostKeyChange carries the new identity to the agent's Activity report.
type HostKeyChange struct {
	Fingerprint, Algorithm string
}

func (e *HostKeyChange) Error() string {
	if e.Fingerprint == "" {
		return ErrHostKeyChanged.Error()
	}
	return fmt.Sprintf("%s (presented %s %s)", ErrHostKeyChanged, e.Algorithm, e.Fingerprint)
}

func (e *HostKeyChange) Unwrap() error { return ErrHostKeyChanged }

// Client keeps one SSH connection for the operations of a backup transfer.
type Client struct {
	workers sync.WaitGroup
	files   *sftp.Client
	ssh     *ssh.Client
	stop    func() bool
	cancel  context.CancelFunc
	raw     *idleConn
	root    string
}

// Open binds an SFTP read or probe to ctx. An empty or changed pin refuses
// authentication before the client offers its credentials.
func Open(ctx context.Context, c Config, dial netguard.DialFunc) (*Client, Handshake, error) {
	return openPinned(ctx, c, dial, downloadRequests)
}

// OpenUpload uses the write request window, where the transport reuses one
// packet buffer and callers do not retain decrypted chunks.
func OpenUpload(ctx context.Context, c Config, dial netguard.DialFunc) (*Client, Handshake, error) {
	return openPinned(ctx, c, dial, uploadRequests)
}

func openPinned(ctx context.Context, c Config, dial netguard.DialFunc, requestsPerFile int) (*Client, Handshake, error) {
	if dial == nil {
		dial = netguard.Dialer(netguard.Options{Private: true, Loopback: true})
	}
	cache := c.keyTypeFile()
	c.HostKeyAlgorithm = cmp.Or(c.HostKeyAlgorithm, readKeyType(cache))
	algorithms := algorithmsFor(c.HostKeyAlgorithm)
	if c.HostKeyAlgorithm == "" {
		algorithms = preferredKeys
	} else if len(algorithms) == 0 {
		return nil, Handshake{}, errors.New("the SFTP destination uses an unsupported host key type. Test the destination to verify a supported key")
	}
	client, result, err := open(ctx, c, dial, algorithms, requestsPerFile)
	if c.HostKeyAlgorithm == "" {
		rememberKeyType(cache, result)
	}
	// Existing pins without a type may predate this preference order. Try the
	// other supported types, with the original pin enforced on every connection.
	if c.HostKeyAlgorithm == "" && errors.Is(err, ErrHostKeyChanged) {
		for _, key := range preferredKeys {
			if key == result.HostKeyAlgorithm || (key == ssh.KeyAlgoRSASHA512 || key == ssh.KeyAlgoRSASHA256) && result.HostKeyAlgorithm == ssh.KeyAlgoRSA {
				continue
			}
			if candidate, seen, failure := open(ctx, c, dial, []string{key}, requestsPerFile); seen.Verified {
				rememberKeyType(cache, seen)
				return candidate, seen, failure
			}
		}
	}
	// If the server has stopped offering the pinned type, discover its new key
	// on a separate connection whose empty pin prevents any authentication.
	var negotiation *ssh.AlgorithmNegotiationError
	if c.Fingerprint != "" && c.HostKeyAlgorithm != "" && errors.As(err, &negotiation) && negotiation.What == "host key" {
		c.Fingerprint, c.HostKeyAlgorithm = "", ""
		_, seen, _ := open(ctx, c, dial, preferredKeys, requestsPerFile)
		return nil, seen, &HostKeyChange{Fingerprint: seen.Fingerprint, Algorithm: seen.HostKeyAlgorithm}
	}
	if errors.As(err, &negotiation) {
		err = ErrUnreachable
	}
	return client, result, err
}

func open(ctx context.Context, c Config, dial netguard.DialFunc, algorithms []string, requestsPerFile int) (*Client, Handshake, error) {
	return openTimed(ctx, c, dial, algorithms, requestsPerFile, idleTimeout, keepaliveInterval)
}

func openTimed(ctx context.Context, c Config, dial netguard.DialFunc, algorithms []string, requestsPerFile int, idle, ping time.Duration) (*Client, Handshake, error) {
	var result Handshake
	auth, err := c.auth()
	if err != nil {
		return nil, result, err
	}
	addr := c.Address()
	conn, err := dial(ctx, "tcp", addr)
	if err != nil {
		return nil, result, ErrUnreachable
	}
	raw := &idleConn{Conn: conn, idle: idle}
	result.Reached = true
	stop := context.AfterFunc(ctx, func() { _ = raw.Close() })
	connected := false
	defer func() {
		if !connected {
			stop()
			_ = raw.Close()
		}
	}()
	if err := raw.SetDeadline(time.Now().Add(handshakeTimeout)); err != nil {
		return nil, result, ErrUnreachable
	}
	// The callback is also invoked during rekey. Only the initial handshake
	// writes result, and every callback enforces the same pin.
	var observed atomic.Bool
	cfg := &ssh.ClientConfig{
		User: c.User, Auth: auth, HostKeyAlgorithms: algorithms,
		HostKeyCallback: func(_ string, _ net.Addr, key ssh.PublicKey) error {
			fingerprint := ssh.FingerprintSHA256(key)
			match := subtle.ConstantTimeCompare([]byte(fingerprint), []byte(c.Fingerprint)) == 1 &&
				(c.HostKeyAlgorithm == "" || key.Type() == c.HostKeyAlgorithm)
			if !observed.Swap(true) {
				result.Fingerprint, result.HostKeyAlgorithm, result.Verified = fingerprint, key.Type(), match
			}
			switch {
			case c.Fingerprint == "":
				return ErrHostKeyUnverified
			case !match:
				return &HostKeyChange{Fingerprint: fingerprint, Algorithm: key.Type()}
			default:
				return nil
			}
		},
	}
	sshConn, channels, requests, err := ssh.NewClientConn(raw, addr, cfg)
	if err != nil {
		var negotiation *ssh.AlgorithmNegotiationError
		switch {
		case errors.Is(err, ErrHostKeyUnverified), errors.Is(err, ErrHostKeyChanged), errors.As(err, &negotiation):
			return nil, result, err
		case result.Verified && (errors.Is(err, ErrAuth) ||
			strings.HasPrefix(strings.TrimPrefix(err.Error(), "ssh: handshake failed: "), "ssh: unable to authenticate, attempted methods ")):
			return nil, result, ErrAuth
		default:
			return nil, result, ErrUnreachable
		}
	}
	result.SignedIn = true
	out := &Client{ssh: ssh.NewClient(sshConn, channels, requests), stop: stop, raw: raw, root: path.Clean(c.Path)}
	out.files, err = out.openSubsystem(requestsPerFile)
	if err != nil {
		_ = out.ssh.Close()
		out.workers.Wait()
		return nil, result, ErrSubsystem
	}
	raw.progress()
	raw.active.Store(true)
	raw.refresh()
	lifetime, cancel := context.WithCancel(ctx)
	out.cancel = cancel
	out.workers.Go(func() { raw.watch(lifetime) })
	out.workers.Go(func() { raw.keepalive(lifetime, ping, out.ssh) })
	connected = true
	return out, result, nil
}

func (c *Client) openSubsystem(requestsPerFile int) (*sftp.Client, error) {
	session, err := c.ssh.NewSession()
	if err != nil {
		return nil, err
	}
	w, err := session.StdinPipe()
	if err != nil {
		return nil, err
	}
	r, err := session.StdoutPipe()
	if err != nil {
		return nil, err
	}
	stderr, err := session.StderrPipe()
	if err != nil {
		return nil, err
	}
	if err := session.RequestSubsystem("sftp"); err != nil {
		return nil, err
	}
	c.workers.Go(func() { _, _ = io.Copy(io.Discard, stderr) })
	return sftp.NewClientPipe(progressReader{r, c.raw}, w, sftp.UseConcurrentReads(true),
		sftp.UseConcurrentWrites(true), sftp.MaxPacket((32<<10)-64), sftp.MaxConcurrentRequestsPerFile(requestsPerFile))
}

// Close ends the SSH connection and waits for its SFTP client to stop.
func (c *Client) Close() {
	c.stop()
	c.cancel()
	_ = c.ssh.Close()
	_ = c.files.Close()
	c.workers.Wait()
}

func (c *Client) failure(fallback error) error {
	return c.raw.failure(fallback)
}

// Folder creates the destination folder if necessary and verifies it is a directory.
func (c *Client) Folder() error {
	if err := c.files.MkdirAll(c.root); err != nil {
		return c.failure(ErrFolder)
	}
	info, err := c.files.Stat(c.root)
	if err != nil || !info.IsDir() {
		return c.failure(ErrFolder)
	}
	return nil
}

// FreeSpace returns the available bytes when the server supports OpenSSH statvfs.
func (c *Client) FreeSpace() (int64, bool) { return c.freeSpace(c.root) }

func (c *Client) freeSpace(dir string) (int64, bool) {
	if _, ok := c.files.HasExtension("statvfs@openssh.com"); !ok {
		return 0, false
	}
	info, err := c.files.StatVFS(dir)
	if err != nil || info.Frsize == 0 {
		return 0, false
	}
	if info.Bavail > math.MaxInt64/info.Frsize {
		return math.MaxInt64, true
	}
	return int64(info.Bavail * info.Frsize), true //nolint:gosec // bounded above
}

// Staged holds a fully written file under a temporary name in its final directory.
type Staged struct {
	client          *Client
	temporary, name string
}

// Stage writes the file under an exclusive temporary name and checks its size
// before anything is visible at key. The caller discards or commits it.
func (c *Client) Stage(key string, size int64, body io.Reader) (*Staged, error) {
	name, ok := c.object(key)
	if !ok || c.files.MkdirAll(path.Dir(name)) != nil {
		return nil, c.failure(ErrFolder)
	}
	dir := path.Dir(name)
	temporary := path.Join(dir, partialPrefix+rand.Text())
	f, err := c.files.OpenFile(temporary, os.O_WRONLY|os.O_CREATE|os.O_EXCL)
	if err != nil {
		return nil, c.failure(ErrWrite)
	}
	staged := &Staged{client: c, temporary: temporary, name: name}
	_, writeErr := f.ReadFrom(&io.LimitedReader{R: localReader{body, c.raw}, N: size})
	closeErr := f.Close()
	if writeErr != nil || closeErr != nil {
		staged.Discard()
		return nil, c.failure(ErrWrite)
	}
	info, err := c.files.Stat(temporary)
	if err != nil || info.Size() != size {
		staged.Discard()
		return nil, c.failure(ErrWrite)
	}
	return staged, nil
}

// Commit publishes the complete file with a rename in its directory.
func (s *Staged) Commit() error {
	var err error
	if _, ok := s.client.files.HasExtension("posix-rename@openssh.com"); ok {
		err = s.client.files.PosixRename(s.temporary, s.name)
	} else {
		err = s.client.files.Rename(s.temporary, s.name)
	}
	if err != nil {
		return s.client.failure(ErrWrite)
	}
	s.temporary = ""
	return nil
}

// Discard attempts to delete an unpublished file. After a connection loss only
// the temporary name may remain, which is invisible to backup listings.
func (s *Staged) Discard() {
	if s.temporary != "" {
		_ = s.client.files.Remove(s.temporary)
	}
}

// PrepareUpload removes leftovers in the archive's directory and checks space.
func (c *Client) PrepareUpload(ctx context.Context, key string, total int64) error {
	name, ok := c.object(key)
	if !ok {
		return ErrFolder
	}
	dir := path.Dir(name)
	if err := c.files.MkdirAll(dir); err != nil {
		return c.failure(ErrFolder)
	}
	// Some accounts can write and rename but cannot list. A sweep is best effort.
	if files, err := c.files.ReadDirContext(ctx, dir); err == nil {
		cutoff := time.Now().Add(-24 * time.Hour)
		for _, f := range files {
			suffix, ok := strings.CutPrefix(f.Name(), partialPrefix)
			if ok && len(suffix) == 26 && strings.Trim(suffix, "ABCDEFGHIJKLMNOPQRSTUVWXYZ234567") == "" &&
				f.Mode().IsRegular() && f.ModTime().Before(cutoff) {
				_ = c.files.Remove(path.Join(dir, f.Name()))
			}
		}
	}
	if _, err := c.files.Stat(name); errors.Is(err, os.ErrNotExist) {
		_ = c.Delete(ctx, key+".json")
	}
	if free, ok := c.freeSpace(dir); ok && free < total {
		return errors.New("the SFTP server reports too little free space for this backup, so Dockwell skipped the upload. Free space in the destination, then try again")
	}
	return c.failure(ctx.Err())
}

// Sweep removes files expressly selected by the caller when their names and
// modification times belong to the directory and to the caller's snapshot.
func (c *Client) Sweep(ctx context.Context, folder string, before time.Time, orphan func(string) bool) {
	dir, ok := c.object(folder)
	if !ok {
		return
	}
	items, err := c.files.ReadDirContext(ctx, dir)
	if err != nil {
		return
	}
	for _, item := range items {
		if ctx.Err() != nil {
			return
		}
		name := item.Name()
		if path.Base(name) == name && item.Mode().IsRegular() && item.ModTime().Before(before) && orphan(name) {
			_ = c.files.Remove(path.Join(dir, name))
		}
	}
}

// Get streams a file below the destination folder. The pipe keeps the
// concurrent SFTP reads bounded while the seal reader consumes them in order.
func (c *Client) Get(ctx context.Context, key string) (io.ReadCloser, error) {
	name, ok := c.object(key)
	if !ok {
		return nil, ErrRead
	}
	f, err := c.files.Open(name)
	if err != nil {
		return nil, c.failure(ErrRead)
	}
	r, w := io.Pipe()
	done := make(chan struct{})
	stop := context.AfterFunc(ctx, func() { _ = r.CloseWithError(ctx.Err()) })
	go func() {
		defer close(done)
		_, err := f.WriteTo(localWriter{w, c.raw})
		_ = f.Close()
		if err != nil {
			_ = w.CloseWithError(c.failure(ErrRead))
		} else {
			_ = w.Close()
		}
	}()
	return &archiveReader{PipeReader: r, done: done, stop: stop}, nil
}

type archiveReader struct {
	*io.PipeReader
	done <-chan struct{}
	stop func() bool
}

func (r *archiveReader) Close() error {
	r.stop()
	_ = r.PipeReader.Close()
	<-r.done
	return nil
}

// Delete removes a file below the destination folder. A missing file counts as deleted.
func (c *Client) Delete(_ context.Context, key string) error {
	name, ok := c.object(key)
	if !ok {
		return c.failure(ErrDelete)
	}
	if err := c.files.Remove(name); err != nil && !errors.Is(err, os.ErrNotExist) {
		return c.failure(ErrDelete)
	}
	return nil
}

func (c *Client) object(key string) (string, bool) {
	if key == "" || key == "." || key == ".." || path.IsAbs(key) || path.Clean(key) != key || strings.HasPrefix(key, "../") || strings.ContainsRune(key, 0) {
		return "", false
	}
	return path.Join(c.root, key), true
}
