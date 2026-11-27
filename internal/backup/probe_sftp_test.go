// SPDX-License-Identifier: AGPL-3.0-only

package backup

import (
	"bytes"
	"crypto/ed25519"
	"crypto/rand"
	"crypto/x509"
	"encoding/pem"
	"net"
	"strconv"
	"strings"
	"testing"

	"golang.org/x/crypto/ssh"

	dockwellv1 "github.com/dockwell/dockwell/gen/dockwell/v1"
	"github.com/dockwell/dockwell/internal/backup/sftpstore"
	"github.com/dockwell/dockwell/internal/backup/sftpstore/sftptest"
)

func TestSFTPProbeVerifiesBeforeItOffersCredentials(t *testing.T) {
	const password = "example-fixture-password"
	_, private, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	signer, err := ssh.NewSignerFromKey(private)
	if err != nil {
		t.Fatal(err)
	}
	public := signer.PublicKey().Marshal()
	key, err := ssh.MarshalPrivateKeyWithPassphrase(private, "", []byte(password))
	if err != nil {
		t.Fatal(err)
	}
	login := sftptest.Login{User: "backups", Password: password, AcceptKey: func(key ssh.PublicKey) bool {
		return bytes.Equal(public, key.Marshal())
	}}
	server, err := sftptest.Listen(t.Context(), "127.0.0.1:0", login)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(server.Close)
	other, err := sftptest.Listen(t.Context(), "127.0.0.1:0", login)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(other.Close)

	host, port, err := net.SplitHostPort(server.Address())
	if err != nil {
		t.Fatal(err)
	}
	n, err := strconv.ParseUint(port, 10, 32)
	if err != nil {
		t.Fatal(err)
	}
	cfg := sftpstore.Config{Host: host, Port: uint32(n), User: login.User, Path: "/one/two", Password: password}
	first := TestSFTP(t.Context(), cfg, nil)
	if first.GetProblem() != dockwellv1.BackupDestinationProblem_BACKUP_DESTINATION_PROBLEM_HOST_KEY_UNVERIFIED ||
		first.GetSftp().GetHostKeyFingerprint() != server.Fingerprint() || first.GetSftp().GetHostKeyAlgorithm() != ssh.KeyAlgoED25519 || !first.GetSftp().GetReached() || first.GetSignedIn() {
		t.Fatalf("first contact = %v", first)
	}
	cfg.Fingerprint = other.Fingerprint()
	changed := TestSFTP(t.Context(), cfg, nil)
	if changed.GetProblem() != dockwellv1.BackupDestinationProblem_BACKUP_DESTINATION_PROBLEM_HOST_KEY_CHANGED ||
		changed.GetSftp().GetHostKeyFingerprint() != server.Fingerprint() || changed.GetSftp().GetHostKeyVerified() {
		t.Fatalf("changed key = %v", changed)
	}
	cfg.HostKeyAlgorithm = ssh.KeyAlgoECDSA256
	changedType := TestSFTP(t.Context(), cfg, nil)
	if changedType.GetProblem() != dockwellv1.BackupDestinationProblem_BACKUP_DESTINATION_PROBLEM_HOST_KEY_CHANGED ||
		changedType.GetSftp().GetHostKeyAlgorithm() != ssh.KeyAlgoED25519 ||
		changedType.GetSftp().GetHostKeyFingerprint() != server.Fingerprint() {
		t.Fatalf("changed key type = %v", changedType)
	}
	if attempts := server.AuthAttempts(); attempts != 0 {
		t.Fatalf("offered credentials %d times before verifying the host key", attempts)
	}
	cfg.HostKeyAlgorithm = ssh.KeyAlgoED25519
	cfg.Fingerprint, cfg.Password = server.Fingerprint(), "wrong password"
	refused := TestSFTP(t.Context(), cfg, nil)
	if refused.GetProblem() != dockwellv1.BackupDestinationProblem_BACKUP_DESTINATION_PROBLEM_KEYS_REFUSED ||
		!refused.GetSftp().GetHostKeyVerified() || refused.GetSignedIn() {
		t.Fatalf("wrong password = %v", refused)
	}
	cfg.Password = password
	check := func(out *dockwellv1.TestBackupDestinationResponse) {
		t.Helper()
		probe := out.GetSftp()
		if out.GetProblem() != 0 || !probe.GetHostKeyVerified() || !out.GetSignedIn() || !probe.GetFolderReady() ||
			!out.GetWrote() || !probe.GetReadBack() || !probe.GetDeleted() || probe.GetFreeBytes() != 128<<20 {
			t.Fatalf("probe = %v", out)
		}
	}
	check(TestSFTP(t.Context(), cfg, nil))
	cfg.Password, cfg.PrivateKey = "", string(pem.EncodeToMemory(key))
	if err := cfg.Validate(true); err == nil {
		t.Fatal("an encrypted key passed validation without its passphrase")
	}
	cfg.KeyPassphrase = password
	check(TestSFTP(t.Context(), cfg, nil))
	der, err := x509.MarshalPKCS8PrivateKey(private)
	if err != nil {
		t.Fatal(err)
	}
	cfg.PrivateKey = string(pem.EncodeToMemory(&pem.Block{Type: "PRIVATE KEY", Bytes: der}))
	cfg.KeyPassphrase = ""
	if got, err := cfg.PublicKey(); err != nil || got != strings.TrimSpace(string(ssh.MarshalAuthorizedKey(signer.PublicKey()))) {
		t.Fatalf("PKCS#8 public key = %q, %v", got, err)
	}
	check(TestSFTP(t.Context(), cfg, nil))
}
