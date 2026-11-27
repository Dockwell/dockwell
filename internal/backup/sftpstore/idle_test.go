// SPDX-License-Identifier: AGPL-3.0-only

package sftpstore

import (
	"context"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"errors"
	"io"
	"net"
	"strconv"
	"strings"
	"testing"
	"time"

	"golang.org/x/crypto/ssh"

	"github.com/dockwell/dockwell/internal/backup/sftpstore/sftptest"
)

func fixture(t *testing.T, login sftptest.Login) (*sftptest.Server, Config) {
	t.Helper()
	login.User, login.Password = "backup-user", "fixture-password"
	srv, err := sftptest.Listen(t.Context(), "127.0.0.1:0", login)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(srv.Close)
	host, port, err := net.SplitHostPort(srv.Address())
	if err != nil {
		t.Fatal(err)
	}
	number, err := strconv.ParseUint(port, 10, 32)
	if err != nil {
		t.Fatal(err)
	}
	return srv, Config{Host: host, Port: uint32(number), User: login.User, Password: login.Password, Path: "/backups", Fingerprint: srv.Fingerprint()}
}

func TestSSHKeyboardInteractiveAllowsEmptyRounds(t *testing.T) {
	_, cfg := fixture(t, sftptest.Login{KeyboardInteractive: true})
	c, seen, err := Open(t.Context(), cfg, nil)
	if err != nil {
		t.Fatal(err)
	}
	defer c.Close()
	if !seen.SignedIn {
		t.Fatal("the single hidden prompt did not authenticate")
	}
}

func TestSSHKeyboardInteractiveRefusesExtraOrEchoedPrompts(t *testing.T) {
	for _, tc := range []struct {
		name    string
		login   sftptest.Login
		prompts int64
	}{
		{"second hidden prompt", sftptest.Login{KeyboardInteractive: true, SecondPasswordPrompt: true}, 2},
		{"echoed prompt", sftptest.Login{KeyboardInteractive: true, EchoPasswordPrompt: true}, 1},
	} {
		t.Run(tc.name, func(t *testing.T) {
			srv, cfg := fixture(t, tc.login)
			client, seen, err := Open(t.Context(), cfg, nil)
			if client != nil {
				client.Close()
			}
			if !errors.Is(err, ErrAuth) || !seen.Verified || seen.SignedIn || srv.PasswordPrompts() != tc.prompts {
				t.Fatalf("authentication = %+v, err=%v, prompts=%d", seen, err, srv.PasswordPrompts())
			}
		})
	}
}

func TestPinRemembersVerifiedLegacyKeyType(t *testing.T) {
	private, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	signer, err := ssh.NewSignerFromKey(private)
	if err != nil {
		t.Fatal(err)
	}
	_, cfg := fixture(t, sftptest.Login{HostKeys: []ssh.Signer{signer}})
	cfg.Fingerprint = ssh.FingerprintSHA256(signer.PublicKey())
	cfg.KeyTypesDir = t.TempDir()
	var count int
	dial := func(ctx context.Context, network, addr string) (net.Conn, error) {
		count++
		var d net.Dialer
		return d.DialContext(ctx, network, addr)
	}
	for _, want := range []int{2, 1} {
		count = 0
		client, seen, err := Open(t.Context(), cfg, dial)
		if err != nil {
			t.Fatal(err)
		}
		client.Close()
		if count != want || !seen.Verified || seen.HostKeyAlgorithm != ssh.KeyAlgoECDSA256 {
			t.Fatalf("opened %d connections, want %d, saw %+v", count, want, seen)
		}
	}
}

func TestSFTPIdleEvenWhenSSHAnswersKeepalives(t *testing.T) {
	gate := make(chan struct{})
	srv, cfg := fixture(t, sftptest.Login{ReadGate: gate})
	t.Cleanup(func() { close(gate) })
	var dial net.Dialer
	client, _, err := openTimed(t.Context(), cfg, dial.DialContext, preferredKeys, downloadRequests, 300*time.Millisecond, 25*time.Millisecond)
	if err != nil {
		t.Fatal(err)
	}
	defer client.Close()
	staged, err := client.Stage("fixture", 4, strings.NewReader("data"))
	if err != nil {
		t.Fatal(err)
	}
	if err := staged.Commit(); err != nil {
		t.Fatal(err)
	}
	stream, err := client.Get(t.Context(), "fixture")
	if err != nil {
		t.Fatal(err)
	}
	defer stream.Close()
	start := time.Now()
	if _, err := io.Copy(io.Discard, stream); !errors.Is(err, ErrIdle) {
		t.Fatalf("stalled peer: %v", err)
	}
	if time.Since(start) > 2*time.Second || srv.Keepalives() == 0 {
		t.Fatalf("idle took %s and SSH saw %d keepalives", time.Since(start), srv.Keepalives())
	}
}

func TestSSHKeepaliveExpiresWhenThePeerDoesNotReply(t *testing.T) {
	srv, cfg := fixture(t, sftptest.Login{IgnoreKeepalives: true})
	ctx, cancel := context.WithTimeout(t.Context(), 3*time.Second)
	defer cancel()
	var dial net.Dialer
	client, _, err := openTimed(ctx, cfg, dial.DialContext, preferredKeys, downloadRequests, time.Minute, 25*time.Millisecond)
	if err != nil {
		t.Fatal(err)
	}
	defer client.Close()
	_ = client.ssh.Wait()
	if got := client.failure(nil); !errors.Is(got, ErrIdle) || srv.Keepalives() != 1 {
		t.Fatalf("keepalive expiry = %v, requests=%d", got, srv.Keepalives())
	}
}

func TestLocalSFTPStallNamesTheServerDisk(t *testing.T) {
	raw, peer := net.Pipe()
	defer peer.Close()
	c := &idleConn{Conn: raw, idle: 30 * time.Millisecond}
	c.progress()
	c.local(true)
	c.watch(t.Context())
	if got := c.failure(nil); !errors.Is(got, ErrLocalIdle) {
		t.Fatalf("local stall: %v", got)
	}
}
