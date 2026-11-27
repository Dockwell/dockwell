// SPDX-License-Identifier: AGPL-3.0-only

package backuppolicy

import (
	"errors"
	"path"
	"strconv"
	"strings"

	resourcev1 "github.com/dockwell/dockwell/gen/dockwell/resource/v1alpha1"
	"github.com/dockwell/dockwell/internal/backup/sftpstore"
)

// SFTPConfig reads the endpoint and credentials of an SFTP destination.
func SFTPConfig(s *resourcev1.BackupDestinationSftp) sftpstore.Config {
	return sftpstore.Config{
		Host: s.GetHost(), Port: s.GetPort(), User: s.GetUser(), Path: s.GetPath(),
		Fingerprint: s.GetHostKeyFingerprint(), HostKeyAlgorithm: s.GetHostKeyAlgorithm(), PrivateKey: s.GetPrivateKey(),
		Password: s.GetPassword(), KeyPassphrase: s.GetKeyPassphrase(),
	}
}

// SFTPValues are the settings sealed to an agent for a transfer or probe.
func SFTPValues(s *resourcev1.BackupDestinationSftp) map[string]string {
	return map[string]string{
		"transport": "sftp", "host": s.GetHost(), "port": strconv.FormatUint(uint64(s.GetPort()), 10),
		"user": s.GetUser(), "path": s.GetPath(), "host_key_fingerprint": s.GetHostKeyFingerprint(),
		"host_key_algorithm": s.GetHostKeyAlgorithm(), "private_key": s.GetPrivateKey(),
		"password": s.GetPassword(), "key_passphrase": s.GetKeyPassphrase(),
	}
}

// KeepSFTPCredentials fills an omitted sign-in only for the saved SSH account.
// A key proves identity without disclosing itself. A password is sent to the
// server, so reuse it only with the same pinned host key.
func KeepSFTPCredentials(was, next *resourcev1.BackupDestinationSftp) {
	if was == nil || next == nil || next.GetPrivateKey() != "" || next.GetPassword() != "" || next.GetKeyPassphrase() != "" {
		return
	}
	normalizeSFTP(next)
	if next.Host != was.GetHost() || SFTPConfig(next).Address() != SFTPConfig(was).Address() || next.User != was.GetUser() {
		return
	}
	next.PrivateKey, next.KeyPassphrase = was.GetPrivateKey(), was.GetKeyPassphrase()
	if next.HostKeyFingerprint == was.GetHostKeyFingerprint() &&
		(next.HostKeyAlgorithm == was.GetHostKeyAlgorithm() || next.HostKeyAlgorithm == "" || was.GetHostKeyAlgorithm() == "") {
		next.Password = was.GetPassword()
	}
	if next.HostKeyAlgorithm == "" && next.HostKeyFingerprint == was.GetHostKeyFingerprint() {
		next.HostKeyAlgorithm = was.GetHostKeyAlgorithm()
	}
}

func normalizeSFTP(s *resourcev1.BackupDestinationSftp) {
	s.Host = strings.ToLower(strings.TrimSpace(s.Host))
	s.User = strings.TrimSpace(s.User)
	s.Path = strings.TrimSpace(s.Path)
	s.HostKeyFingerprint = strings.TrimSpace(s.HostKeyFingerprint)
	s.HostKeyAlgorithm = strings.TrimSpace(s.HostKeyAlgorithm)
	if s.Port == 0 {
		s.Port = 22
	}
}

// ValidateSFTP validates and normalizes an SFTP destination. Only the connectivity
// test accepts an empty fingerprint, to learn it before offering credentials.
func ValidateSFTP(d *resourcev1.BackupDestination, requireHostKey bool) error {
	if d.GetEndpoint() != "" || d.GetRegion() != "" || d.GetBucket() != "" || d.GetPrefix() != "" ||
		d.GetAccessKey() != "" || d.GetSecretKey() != "" || d.GetConnection() != "" {
		return errors.New("an SFTP destination takes SSH and folder settings. Leave out the S3 settings and connection")
	}
	s := d.GetSftp()
	normalizeSFTP(s)
	cfg := SFTPConfig(s)
	if err := cfg.Validate(requireHostKey); err != nil {
		return err
	}
	publicKey, err := cfg.PublicKey()
	if err != nil {
		return err
	}
	s.PublicKey = publicKey
	s.Path = path.Clean(s.Path)
	return nil
}
