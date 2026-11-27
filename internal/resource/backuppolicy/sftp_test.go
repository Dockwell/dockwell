// SPDX-License-Identifier: AGPL-3.0-only

package backuppolicy

import (
	"testing"

	"google.golang.org/protobuf/proto"

	resourcev1 "github.com/dockwell/dockwell/gen/dockwell/resource/v1alpha1"
)

func TestSFTPCredentialReuse(t *testing.T) {
	saved := &resourcev1.BackupDestinationSftp{
		Host: "storage.example.com", Port: 22, User: "backups",
		HostKeyFingerprint: "old fingerprint", HostKeyAlgorithm: "ssh-ed25519", Password: "example password",
	}
	edit := &resourcev1.BackupDestinationSftp{
		Host: " STORAGE.EXAMPLE.COM ", User: "backups", Path: "/new-folder",
		HostKeyFingerprint: saved.HostKeyFingerprint,
	}
	for _, tc := range []struct {
		name         string
		change       func(*resourcev1.BackupDestinationSftp)
		wantPassword string
	}{
		{"same account and pin", func(_ *resourcev1.BackupDestinationSftp) {}, saved.Password},
		{"new host", func(s *resourcev1.BackupDestinationSftp) { s.Host = "other.example.com" }, ""},
		{"new port", func(s *resourcev1.BackupDestinationSftp) { s.Port = 2222 }, ""},
		{"new user", func(s *resourcev1.BackupDestinationSftp) { s.User = "other" }, ""},
		{"new pin", func(s *resourcev1.BackupDestinationSftp) { s.HostKeyFingerprint = "new fingerprint" }, ""},
		{"replacement", func(s *resourcev1.BackupDestinationSftp) { s.Password = "replacement" }, "replacement"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			next := proto.CloneOf(edit)
			tc.change(next)
			KeepSFTPCredentials(saved, next)
			if next.Password != tc.wantPassword {
				t.Fatal("unexpected password reuse")
			}
		})
	}
	saved.Password, saved.PrivateKey, saved.KeyPassphrase = "", "example key", "example key passphrase"
	edit.HostKeyFingerprint = "new fingerprint"
	KeepSFTPCredentials(saved, edit)
	if edit.PrivateKey != saved.PrivateKey || edit.KeyPassphrase != saved.KeyPassphrase || edit.HostKeyFingerprint != "new fingerprint" {
		t.Fatal("the same SSH account could not keep its key when changing its pin")
	}
}
