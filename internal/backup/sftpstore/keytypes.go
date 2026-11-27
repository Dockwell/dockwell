// SPDX-License-Identifier: AGPL-3.0-only

package sftpstore

import (
	"encoding/base64"
	"encoding/hex"
	"io"
	"os"
	"path/filepath"
	"strings"
)

// For a legacy fingerprint-only pin, remember the verified type on this
// agent. The cached value only chooses the algorithms to offer. Every
// handshake still compares the presented key with the stored fingerprint.
func (c Config) keyTypeFile() string {
	if c.KeyTypesDir == "" || c.HostKeyAlgorithm != "" {
		return ""
	}
	encoded, ok := strings.CutPrefix(c.Fingerprint, "SHA256:")
	if !ok {
		return ""
	}
	fingerprint, err := base64.RawStdEncoding.Strict().DecodeString(encoded)
	if err != nil || len(fingerprint) != 32 {
		return ""
	}
	return filepath.Join(c.KeyTypesDir, hex.EncodeToString(fingerprint))
}

func readKeyType(file string) string {
	if file == "" {
		return ""
	}
	f, err := os.Open(file) //nolint:gosec // file is derived from a decoded fingerprint under the agent's data directory
	if err != nil {
		return ""
	}
	defer f.Close()
	data, err := io.ReadAll(io.LimitReader(f, 64))
	if err != nil || len(algorithmsFor(string(data))) == 0 {
		return ""
	}
	return string(data)
}

func rememberKeyType(file string, seen Handshake) {
	if file == "" || !seen.Verified || len(algorithmsFor(seen.HostKeyAlgorithm)) == 0 {
		return
	}
	dir := filepath.Dir(file)
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return
	}
	f, err := os.CreateTemp(dir, ".key-type-")
	if err != nil {
		return
	}
	defer os.Remove(f.Name())
	_, written := f.WriteString(seen.HostKeyAlgorithm)
	if closed := f.Close(); written == nil && closed == nil {
		_ = os.Rename(f.Name(), file)
	}
}
