// SPDX-License-Identifier: AGPL-3.0-only

// Package sftptest serves an in-memory SFTP filesystem for local tests.
package sftptest

import (
	"context"
	"crypto/ed25519"
	"crypto/rand"
	"errors"
	"io"
	"net"
	"sync"
	"sync/atomic"
	"time"

	"github.com/pkg/sftp"
	"golang.org/x/crypto/ssh"
)

// Login controls how the fixture authenticates the user.
type Login struct {
	User, Password       string
	AcceptKey            func(ssh.PublicKey) bool
	KeyboardInteractive  bool
	EchoPasswordPrompt   bool
	SecondPasswordPrompt bool
	IgnoreKeepalives     bool
	HostKeys             []ssh.Signer
	ReadGate             <-chan struct{}
}

// Server supports dropping a connection partway through a read or write.
type Server struct {
	address           string
	signer            ssh.Signer
	listener          net.Listener
	files             sftp.Handlers
	disk              bool
	attempts          atomic.Int64
	keepaliveRequests atomic.Int64
	passwordPrompts   atomic.Int64
	ignoreKeepalives  bool
	readGate          <-chan struct{}
	cutWriteAt        atomic.Int64
	cutReadAt         atomic.Int64
	done              chan struct{}

	mu     sync.Mutex
	conns  map[net.Conn]struct{}
	closed bool
}

// Listen starts the fixture on a local address.
func Listen(ctx context.Context, address string, login Login) (*Server, error) {
	return listen(ctx, address, login, false)
}

// ListenDisk starts the fixture with disk-backed storage for transfer loads.
func ListenDisk(ctx context.Context, address string, login Login) (*Server, error) {
	return listen(ctx, address, login, true)
}

func listen(ctx context.Context, address string, login Login, disk bool) (*Server, error) {
	_, private, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		return nil, err
	}
	signer, err := ssh.NewSignerFromKey(private)
	if err != nil {
		return nil, err
	}
	var config net.ListenConfig
	listener, err := config.Listen(ctx, "tcp", address)
	if err != nil {
		return nil, err
	}
	s := &Server{
		address: listener.Addr().String(), signer: signer, listener: listener, files: sftp.InMemHandler(), disk: disk,
		conns: make(map[net.Conn]struct{}), done: make(chan struct{}), readGate: login.ReadGate, ignoreKeepalives: login.IgnoreKeepalives,
	}
	cfg := &ssh.ServerConfig{}
	if login.KeyboardInteractive {
		cfg.KeyboardInteractiveCallback = func(meta ssh.ConnMetadata, prompt ssh.KeyboardInteractiveChallenge) (*ssh.Permissions, error) {
			s.attempts.Add(1)
			if _, err := prompt("", "before the prompt", nil, nil); err != nil {
				return nil, err
			}
			s.passwordPrompts.Add(1)
			reply, err := prompt("", "", []string{"Password: "}, []bool{login.EchoPasswordPrompt})
			if err != nil || meta.User() != login.User || len(reply) != 1 || reply[0] != login.Password {
				return nil, errors.New("password rejected")
			}
			if login.SecondPasswordPrompt {
				s.passwordPrompts.Add(1)
				_, err := prompt("", "", []string{"Password again: "}, []bool{false})
				return nil, err
			}
			_, err = prompt("", "after the prompt", nil, nil)
			return nil, err
		}
	}
	if login.Password != "" && !login.KeyboardInteractive {
		cfg.PasswordCallback = func(meta ssh.ConnMetadata, password []byte) (*ssh.Permissions, error) {
			s.attempts.Add(1)
			if meta.User() == login.User && string(password) == login.Password {
				return nil, nil
			}
			return nil, errors.New("password rejected")
		}
	}
	if login.AcceptKey != nil {
		cfg.PublicKeyCallback = func(meta ssh.ConnMetadata, key ssh.PublicKey) (*ssh.Permissions, error) {
			s.attempts.Add(1)
			if meta.User() == login.User && login.AcceptKey(key) {
				return nil, nil
			}
			return nil, errors.New("key rejected")
		}
	}
	cfg.AddHostKey(signer)
	for _, key := range login.HostKeys {
		cfg.AddHostKey(key)
	}
	go s.serve(cfg)
	return s, nil
}

// Address is the TCP address the fixture listens on.
func (s *Server) Address() string { return s.address }

// Fingerprint is the fixture's SSH host key fingerprint.
func (s *Server) Fingerprint() string { return ssh.FingerprintSHA256(s.signer.PublicKey()) }

// AuthAttempts counts requests that reached the authentication callbacks.
func (s *Server) AuthAttempts() int64 { return s.attempts.Load() }

// Keepalives counts the SSH keepalive requests.
func (s *Server) Keepalives() int64 { return s.keepaliveRequests.Load() }

// PasswordPrompts counts nonempty keyboard-interactive rounds sent by the fixture.
func (s *Server) PasswordPrompts() int64 { return s.passwordPrompts.Load() }

// CutWriteAt closes the first connection that writes at or past offset.
func (s *Server) CutWriteAt(offset int64) { s.cutWriteAt.Store(offset) }

// CutReadAt closes the first connection that reads at or past offset.
func (s *Server) CutReadAt(offset int64) { s.cutReadAt.Store(offset) }

// Close stops accepting connections and closes any open sessions.
func (s *Server) Close() {
	s.mu.Lock()
	s.closed = true
	_ = s.listener.Close()
	for conn := range s.conns {
		_ = conn.Close()
	}
	s.mu.Unlock()
	<-s.done
}

func (s *Server) serve(cfg *ssh.ServerConfig) {
	defer close(s.done)
	var connections sync.WaitGroup
	defer connections.Wait()
	for {
		raw, err := s.listener.Accept()
		if err != nil {
			return
		}
		s.mu.Lock()
		if s.closed {
			s.mu.Unlock()
			_ = raw.Close()
			return
		}
		s.conns[raw] = struct{}{}
		s.mu.Unlock()
		connections.Go(func() {
			defer func() {
				_ = raw.Close()
				s.mu.Lock()
				delete(s.conns, raw)
				s.mu.Unlock()
			}()
			s.session(raw, cfg)
		})
	}
}

func (s *Server) session(raw net.Conn, cfg *ssh.ServerConfig) {
	_ = raw.SetDeadline(time.Now().Add(10 * time.Second))
	_, channels, requests, err := ssh.NewServerConn(raw, cfg)
	if err != nil {
		return
	}
	_ = raw.SetDeadline(time.Time{})
	go func() {
		for req := range requests {
			if req.Type == "keepalive@openssh.com" {
				s.keepaliveRequests.Add(1)
				if s.ignoreKeepalives {
					continue
				}
			}
			_ = req.Reply(false, nil)
		}
	}()
	for incoming := range channels {
		if incoming.ChannelType() != "session" {
			_ = incoming.Reject(ssh.UnknownChannelType, "only sessions are supported")
			continue
		}
		channel, requests, err := incoming.Accept()
		if err != nil {
			return
		}
		s.subsystem(raw, channel, requests)
	}
}

func (s *Server) subsystem(raw net.Conn, channel ssh.Channel, requests <-chan *ssh.Request) {
	defer channel.Close()
	for req := range requests {
		var payload struct{ Name string }
		ok := req.Type == "subsystem" && ssh.Unmarshal(req.Payload, &payload) == nil && payload.Name == "sftp"
		_ = req.Reply(ok, nil)
		if !ok {
			continue
		}
		if s.disk {
			server, err := sftp.NewServer(channel)
			if err == nil {
				_ = server.Serve()
				_ = server.Close()
			}
			return
		}
		files := s.files
		cut := func() { _ = raw.Close() }
		files.FileGet = cutReads{files.FileGet, &s.cutReadAt, cut, s.readGate}
		files.FilePut = cutWrites{files.FilePut, &s.cutWriteAt, cut}
		files.FileCmd = folderCommands{files.FileCmd}
		server := sftp.NewRequestServer(channel, files)
		_ = server.Serve()
		_ = server.Close()
		return
	}
}

type folderCommands struct{ sftp.FileCmder }

func (c folderCommands) PosixRename(r *sftp.Request) error {
	return c.FileCmder.(sftp.PosixRenameFileCmder).PosixRename(r) //nolint:forcetypeassert // InMemHandler supplies PosixRename.
}

func (folderCommands) StatVFS(*sftp.Request) (*sftp.StatVFS, error) {
	return &sftp.StatVFS{Frsize: 4096, Bavail: 32768}, nil
}

type cutReads struct {
	sftp.FileReader
	at   *atomic.Int64
	cut  func()
	gate <-chan struct{}
}

func (r cutReads) Fileread(req *sftp.Request) (io.ReaderAt, error) {
	f, err := r.FileReader.Fileread(req)
	if err != nil {
		return nil, err
	}
	return reading{f, r.at, r.cut, r.gate}, nil
}

type reading struct {
	io.ReaderAt
	at   *atomic.Int64
	cut  func()
	gate <-chan struct{}
}

func (r reading) ReadAt(p []byte, off int64) (int, error) {
	if r.gate != nil {
		<-r.gate
	}
	if n := r.at.Load(); n > 0 && off >= n && r.at.CompareAndSwap(n, 0) {
		r.cut()
		return 0, io.ErrUnexpectedEOF
	}
	return r.ReaderAt.ReadAt(p, off)
}

type cutWrites struct {
	sftp.FileWriter
	at  *atomic.Int64
	cut func()
}

func (w cutWrites) Filewrite(req *sftp.Request) (io.WriterAt, error) {
	f, err := w.FileWriter.Filewrite(req)
	if err != nil {
		return nil, err
	}
	return writing{f, w.at, w.cut}, nil
}

type writing struct {
	io.WriterAt
	at  *atomic.Int64
	cut func()
}

func (w writing) WriteAt(p []byte, off int64) (int, error) {
	if n := w.at.Load(); n > 0 && off >= n && w.at.CompareAndSwap(n, 0) {
		w.cut()
		return 0, io.ErrUnexpectedEOF
	}
	return w.WriterAt.WriteAt(p, off)
}
