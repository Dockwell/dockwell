// SPDX-License-Identifier: AGPL-3.0-only

// Package gitsshtest provides an in-process SSH Git server for unit and
// integration tests. It serves real git-upload-pack over golang.org/x/crypto/ssh
// with configurable host keys, authorized public keys, protocol version and
// fault injection.
package gitsshtest

import (
	"bytes"
	"context"
	"crypto/ecdsa"
	"crypto/ed25519"
	"crypto/elliptic"
	"crypto/rand"
	"encoding/binary"
	"errors"
	"fmt"
	"io"
	"net"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"time"

	"golang.org/x/crypto/ssh"
)

// Server is an in-process SSH server serving git-upload-pack for a local
// repository directory.
type Server struct {
	Addr     string
	RepoName string
	RepoDir  string

	ln net.Listener

	mu           sync.Mutex
	hostSigner   ssh.Signer
	authorized   map[string]bool
	denyRepo     bool
	v1Only       bool
	delay        time.Duration
	extraBytes   int
	closed       bool
	seenChannels []string
	seenReqs     []string
	wg           sync.WaitGroup
}

// NewServer starts an in-process SSH Git server listening on listenAddr (for
// example "127.0.0.1:0") serving repoDir under repoName (for example
// "acme/web.git").
func NewServer(listenAddr, repoName, repoDir string) (*Server, error) {
	signer, err := newEd25519Signer()
	if err != nil {
		return nil, err
	}
	var lc net.ListenConfig
	ln, err := lc.Listen(context.Background(), "tcp", listenAddr)
	if err != nil {
		return nil, err
	}
	s := &Server{
		Addr:       ln.Addr().String(),
		RepoName:   strings.TrimPrefix(repoName, "/"),
		RepoDir:    repoDir,
		ln:         ln,
		hostSigner: signer,
		authorized: map[string]bool{},
	}
	s.wg.Add(1)
	go s.serve()
	return s, nil
}

// HostKey returns the current host key algorithm and SHA-256 fingerprint.
func (s *Server) HostKey() (algorithm, fingerprint string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	pub := s.hostSigner.PublicKey()
	return pub.Type(), ssh.FingerprintSHA256(pub)
}

// RotateHostKey replaces the server's Ed25519 host key with a fresh one.
func (s *Server) RotateHostKey() (algorithm, fingerprint string, err error) {
	signer, err := newEd25519Signer()
	if err != nil {
		return "", "", err
	}
	s.mu.Lock()
	s.hostSigner = signer
	s.mu.Unlock()
	pub := signer.PublicKey()
	return pub.Type(), ssh.FingerprintSHA256(pub), nil
}

// RotateHostKeyECDSA replaces the server's host key with an ECDSA P-256 key to
// test host-key algorithm change detection.
func (s *Server) RotateHostKeyECDSA() (algorithm, fingerprint string, err error) {
	priv, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		return "", "", err
	}
	signer, err := ssh.NewSignerFromKey(priv)
	if err != nil {
		return "", "", err
	}
	s.mu.Lock()
	s.hostSigner = signer
	s.mu.Unlock()
	pub := signer.PublicKey()
	return pub.Type(), ssh.FingerprintSHA256(pub), nil
}

// SetAuthorizedKeys replaces the set of authorized OpenSSH public keys.
func (s *Server) SetAuthorizedKeys(keys ...string) error {
	next := make(map[string]bool, len(keys))
	for _, k := range keys {
		if strings.TrimSpace(k) == "" {
			continue
		}
		pub, _, _, _, err := ssh.ParseAuthorizedKey([]byte(k))
		if err != nil {
			return err
		}
		next[string(pub.Marshal())] = true
	}
	s.mu.Lock()
	s.authorized = next
	s.mu.Unlock()
	return nil
}

// SetDenyRepo toggles simulating a missing or inaccessible repository after SSH auth.
func (s *Server) SetDenyRepo(deny bool) {
	s.mu.Lock()
	s.denyRepo = deny
	s.mu.Unlock()
}

// SetV1Only ignores GIT_PROTOCOL=version=2 so the client exercises v1 fallback.
func (s *Server) SetV1Only(v1 bool) {
	s.mu.Lock()
	s.v1Only = v1
	s.mu.Unlock()
}

// SetDelay adds an artificial delay before responding to git-upload-pack.
func (s *Server) SetDelay(d time.Duration) {
	s.mu.Lock()
	s.delay = d
	s.mu.Unlock()
}

// SetExtraBytes injects extra side-band pack bytes to test size bounds.
func (s *Server) SetExtraBytes(n int) {
	s.mu.Lock()
	s.extraBytes = n
	s.mu.Unlock()
}

// SeenChannels returns every SSH channel type requested by clients.
func (s *Server) SeenChannels() []string {
	s.mu.Lock()
	defer s.mu.Unlock()
	out := make([]string, len(s.seenChannels))
	copy(out, s.seenChannels)
	return out
}

// SeenRequests returns every SSH session request type received from clients.
func (s *Server) SeenRequests() []string {
	s.mu.Lock()
	defer s.mu.Unlock()
	out := make([]string, len(s.seenReqs))
	copy(out, s.seenReqs)
	return out
}

// URL returns the canonical ssh:// URL for this test server's repository.
func (s *Server) URL() string {
	return "ssh://git@" + s.Addr + "/" + s.RepoName
}

// Close stops accepting connections and waits for active handlers.
func (s *Server) Close() error {
	s.mu.Lock()
	s.closed = true
	s.mu.Unlock()
	err := s.ln.Close()
	s.wg.Wait()
	return err
}

func (s *Server) serve() {
	defer s.wg.Done()
	for {
		c, err := s.ln.Accept()
		if err != nil {
			return
		}
		s.wg.Add(1)
		go func() {
			defer s.wg.Done()
			s.handleConn(c)
		}()
	}
}

func (s *Server) handleConn(raw net.Conn) {
	defer raw.Close()
	s.mu.Lock()
	signer := s.hostSigner
	s.mu.Unlock()

	cfg := &ssh.ServerConfig{
		PublicKeyCallback: func(_ ssh.ConnMetadata, key ssh.PublicKey) (*ssh.Permissions, error) {
			s.mu.Lock()
			ok := s.authorized[string(key.Marshal())]
			s.mu.Unlock()
			if !ok {
				return nil, errors.New("unknown public key")
			}
			return &ssh.Permissions{}, nil
		},
	}
	cfg.AddHostKey(signer)

	sc, chans, reqs, err := ssh.NewServerConn(raw, cfg)
	if err != nil {
		return
	}
	defer sc.Close()
	go ssh.DiscardRequests(reqs)

	for newChan := range chans {
		s.mu.Lock()
		s.seenChannels = append(s.seenChannels, newChan.ChannelType())
		s.mu.Unlock()
		if newChan.ChannelType() != "session" {
			_ = newChan.Reject(ssh.UnknownChannelType, "only session allowed")
			continue
		}
		ch, requests, err := newChan.Accept()
		if err != nil {
			return
		}
		go s.handleSession(ch, requests)
	}
}

func (s *Server) handleSession(ch ssh.Channel, requests <-chan *ssh.Request) {
	defer ch.Close()
	var gitProtocol string
	for req := range requests {
		s.mu.Lock()
		s.seenReqs = append(s.seenReqs, req.Type)
		s.mu.Unlock()
		switch req.Type {
		case "env":
			var kv struct{ Name, Value string }
			if ssh.Unmarshal(req.Payload, &kv) == nil && kv.Name == "GIT_PROTOCOL" {
				s.mu.Lock()
				v1Only := s.v1Only
				s.mu.Unlock()
				if !v1Only {
					gitProtocol = kv.Value
					_ = req.Reply(true, nil)
					continue
				}
			}
			_ = req.Reply(false, nil)
		case "exec":
			var cmd struct{ Command string }
			if err := ssh.Unmarshal(req.Payload, &cmd); err != nil {
				_ = req.Reply(false, nil)
				return
			}
			_ = req.Reply(true, nil)
			code := s.runExec(ch, cmd.Command, gitProtocol)
			var status [4]byte
			binary.BigEndian.PutUint32(status[:], uint32(code)) //nolint:gosec // exit code fits uint32
			_, _ = ch.SendRequest("exit-status", false, status[:])
			return
		default:
			_ = req.Reply(false, nil)
		}
	}
}

func (s *Server) runExec(ch ssh.Channel, command, gitProtocol string) int {
	s.mu.Lock()
	deny := s.denyRepo
	delay := s.delay
	extra := s.extraBytes
	s.mu.Unlock()

	if delay > 0 {
		time.Sleep(delay)
	}
	rest, ok := strings.CutPrefix(command, "git-upload-pack ")
	if !ok {
		_, _ = io.WriteString(ch.Stderr(), "unsupported command\n")
		return 1
	}
	repo := strings.Trim(strings.TrimSpace(rest), "'\"")
	repo = strings.TrimPrefix(repo, "/")
	want := strings.TrimSuffix(s.RepoName, ".git")
	got := strings.TrimSuffix(repo, ".git")
	if deny || got != want {
		_, _ = io.WriteString(ch.Stderr(), "ERROR: Repository not found.\n")
		return 128
	}
	if extra > 0 {
		// Emit valid v2 greeting, read ls-refs or fetch, and flood packfile bytes.
		_, _ = io.WriteString(ch, "000eversion 2\n0012fetch=shallow\n0000")
		buf := make([]byte, 4096)
		_, _ = ch.Read(buf)
		_, _ = io.WriteString(ch, "000dpackfile\n")
		chunk := make([]byte, 4096)
		chunk[0] = 1
		rem := extra
		for rem > 0 {
			n := min(rem, len(chunk)-1)
			var pkt bytes.Buffer
			fmt.Fprintf(&pkt, "%04x", n+5)
			pkt.Write(chunk[:n+1])
			if _, err := ch.Write(pkt.Bytes()); err != nil {
				return 1
			}
			rem -= n
		}
		_, _ = io.WriteString(ch, "0000")
		return 0
	}

	cmd := exec.CommandContext(context.Background(), "git-upload-pack", filepath.Clean(s.RepoDir)) //nolint:gosec // test fixture
	if gitProtocol != "" {
		cmd.Env = append(cmd.Environ(), "GIT_PROTOCOL="+gitProtocol)
	}
	cmd.Stdin = ch
	cmd.Stdout = ch
	cmd.Stderr = ch.Stderr()
	if err := cmd.Run(); err != nil {
		return 1
	}
	return 0
}

func newEd25519Signer() (ssh.Signer, error) {
	_, priv, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		return nil, err
	}
	return ssh.NewSignerFromKey(priv)
}
