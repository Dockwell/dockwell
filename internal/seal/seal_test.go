// SPDX-License-Identifier: AGPL-3.0-only

package seal

import (
	"bytes"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestRoundTrip(t *testing.T) {
	path := filepath.Join(t.TempDir(), "control", "key")
	k, err := LoadOrCreate(path)
	if err != nil {
		t.Fatal(err)
	}
	info, err := os.Stat(path)
	if err != nil {
		t.Fatal(err)
	}
	if info.Mode().Perm() != 0o600 {
		t.Errorf("key file mode = %v, want 0600", info.Mode().Perm())
	}

	sealed := k.Seal("ca-key", []byte("secret"))
	if bytes.Contains(sealed, []byte("secret")) {
		t.Fatal("sealed value contains the plaintext")
	}
	got, err := k.Open("ca-key", sealed)
	if err != nil || string(got) != "secret" {
		t.Fatalf("Open = %q, %v", got, err)
	}

	// The same key loads back.
	again, err := LoadOrCreate(path)
	if err != nil {
		t.Fatal(err)
	}
	if got, err := again.Open("ca-key", sealed); err != nil || string(got) != "secret" {
		t.Fatalf("reloaded key: Open = %q, %v", got, err)
	}
}

func TestOpenRejects(t *testing.T) {
	k, err := LoadOrCreate(filepath.Join(t.TempDir(), "key"))
	if err != nil {
		t.Fatal(err)
	}
	other, err := LoadOrCreate(filepath.Join(t.TempDir(), "key"))
	if err != nil {
		t.Fatal(err)
	}
	sealed := k.Seal("ca-key", []byte("secret"))

	tampered := bytes.Clone(sealed)
	tampered[len(tampered)-1] ^= 1

	for name, open := range map[string]func() ([]byte, error){
		"another purpose": func() ([]byte, error) { return k.Open("session-key", sealed) },
		"another key":     func() ([]byte, error) { return other.Open("ca-key", sealed) },
		"tampered":        func() ([]byte, error) { return k.Open("ca-key", tampered) },
		"truncated":       func() ([]byte, error) { return k.Open("ca-key", sealed[:10]) },
		"empty":           func() ([]byte, error) { return k.Open("ca-key", nil) },
	} {
		if _, err := open(); !errors.Is(err, ErrOpen) {
			t.Errorf("%s: err = %v, want ErrOpen", name, err)
		}
	}
}

func TestRefusesReadableKey(t *testing.T) {
	path := filepath.Join(t.TempDir(), "key")
	if _, err := LoadOrCreate(path); err != nil {
		t.Fatal(err)
	}
	if err := os.Chmod(path, 0o644); err != nil {
		t.Fatal(err)
	}
	_, err := LoadOrCreate(path)
	if err == nil || !strings.Contains(err.Error(), "chmod 600") {
		t.Fatalf("LoadOrCreate on a world-readable key = %v", err)
	}
}

func TestRefusesWrongSize(t *testing.T) {
	path := filepath.Join(t.TempDir(), "key")
	if err := os.WriteFile(path, []byte("short"), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := LoadOrCreate(path); err == nil || !strings.Contains(err.Error(), "restore it from backup") {
		t.Fatalf("LoadOrCreate on a short key = %v", err)
	}
}

func TestLoadNeverCreates(t *testing.T) {
	path := filepath.Join(t.TempDir(), "key")
	if _, err := Load(path); err == nil {
		t.Fatal("Load created a key")
	}
	if _, err := os.Stat(path); err == nil {
		t.Fatal("Load left a key file behind")
	}
	if _, err := LoadOrCreate(path); err != nil {
		t.Fatal(err)
	}
	if _, err := Load(path); err != nil {
		t.Fatalf("Load of an existing key = %v", err)
	}
}
