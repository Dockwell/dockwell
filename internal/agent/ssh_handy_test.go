// SPDX-License-Identifier: AGPL-3.0-only

package agent

import (
	"bytes"
	"crypto/ed25519"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"golang.org/x/crypto/ssh"

	resourcev1 "github.com/dockwell/dockwell/gen/dockwell/resource/v1alpha1"
	"github.com/dockwell/dockwell/internal/agent/hosttool"
)

func TestSSHKeyKeepsMetadataAndRejectsPrivateMaterial(t *testing.T) {
	priv := ed25519.NewKeyFromSeed(bytes.Repeat([]byte{1}, ed25519.SeedSize))
	public, err := ssh.NewPublicKey(priv.Public())
	if err != nil {
		t.Fatal(err)
	}
	line := strings.TrimSpace(string(ssh.MarshalAuthorizedKey(public))) + " workstation"
	content, key, err := parsePastedSSHKey(line)
	if err != nil {
		t.Fatal(err)
	}
	stamped := fmt.Appendf(nil, "# dockwell-added 2026-01-02T03:04:05Z %s\n%s\n", key.Fingerprint, content)
	rows := parseSSHKeys(stamped)
	if got := rows[1].key; got.GetComment() != "workstation" || got.GetFingerprint() != ssh.FingerprintSHA256(public) || got.GetAdded() == nil {
		t.Fatalf("bad key metadata: %v", got)
	}
	if _, _, err := parsePastedSSHKey("-----BEGIN OPENSSH PRIVATE KEY-----"); err == nil || !strings.Contains(err.Error(), "private key") {
		t.Fatalf("private material: %v", err)
	}
	if _, _, err := parsePastedSSHKey("command=\"false\" " + line); err == nil {
		t.Fatal("accepted key with an option")
	}
}

func TestSSHKeysCannotFollowHomeOrAuthorizedKeyLinks(t *testing.T) {
	root := t.TempDir()
	if err := os.MkdirAll(filepath.Join(root, "real", ".ssh"), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink("real", filepath.Join(root, "link")); err != nil {
		t.Fatal(err)
	}
	r := newSSHReader(Config{Tools: &hosttool.Runner{Root: root}})
	a := sshAccount{name: "fixture", home: "/link", uid: os.Getuid(), gid: os.Getgid()}
	if dir, err := r.sshKeyDir(a, false); err == nil {
		_ = dir.Close()
		t.Fatal("linked home was read")
	}
	a.home = "/real"
	dir, err := r.sshKeyDir(a, false)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = dir.Close() }()
	if err := os.Symlink("/etc/passwd", filepath.Join(root, "real", ".ssh", "authorized_keys")); err != nil {
		t.Fatal(err)
	}
	if _, err := readAuthorized(dir, a); err == nil {
		t.Fatal("linked authorized_keys was read")
	}
}

func TestSSHChangeSettingsRenderBothPasswordMethodsAndTheTemporaryPort(t *testing.T) {
	off, on := false, true
	got := sshFile(&resourcev1.SshConfig{
		PasswordLogin: &off, KeyLogin: &on, RootLogin: "prohibit-password", Port: 2222,
		ListenAddresses: []string{"192.0.2.12", "2001:db8::12"},
	}, 22)
	for _, directive := range []string{"PasswordAuthentication no", "ChallengeResponseAuthentication no", "PubkeyAuthentication yes", "PermitRootLogin prohibit-password", "Port 2222", "Port 22", "ListenAddress 192.0.2.12", "ListenAddress 2001:db8::12"} {
		if !strings.Contains(got, directive+"\n") {
			t.Errorf("missing %q", directive)
		}
	}
}

func TestSSHLoginParserKeepsOnlyMetadata(t *testing.T) {
	now := time.Now()
	ok := parseSSHLogin(now, "Accepted publickey for deploy from 2001:db8::2 port 49152 ssh2: ED25519 SHA256:example")
	bad := parseSSHLogin(now, "Failed password for invalid user guest from 192.0.2.12 port 60234 ssh2")
	if !ok.GetAccepted() || ok.GetMethod() != "publickey" || ok.GetAddress() != "2001:db8::2" || bad.GetAccepted() || bad.GetUser() != "guest" {
		t.Fatalf("parsed %v and %v", ok, bad)
	}
	if parseSSHLogin(now, "Failed password for deploy from hostname port 49152") != nil {
		t.Fatal("accepted an unverified address")
	}
}
