// SPDX-License-Identifier: AGPL-3.0-only

package catalog

import (
	"crypto/subtle"
	"encoding/base64"
	"encoding/hex"
	"fmt"
	"strings"
	"testing"

	"golang.org/x/crypto/argon2"

	catalogv1 "github.com/dockwell/dockwell/gen/dockwell/catalog/v1alpha1"
)

// verifyPHC verifies a secret against an Argon2id PHC string as Vaultwarden's own
// check does (argon2::PasswordVerifier): with the parameters, salt and key the
// string says, never Dockwell's own.
func verifyPHC(t *testing.T, phc, secret string) bool {
	t.Helper()
	parts := strings.Split(phc, "$")
	if len(parts) != 6 || parts[0] != "" || parts[1] != "argon2id" || parts[2] != fmt.Sprintf("v=%d", argon2.Version) {
		t.Fatalf("%q isn't an Argon2id PHC string", phc)
	}
	var m, tm uint32
	var p uint8
	if _, err := fmt.Sscanf(parts[3], "m=%d,t=%d,p=%d", &m, &tm, &p); err != nil {
		t.Fatalf("its parameters %q: %v", parts[3], err)
	}
	salt, err1 := base64.RawStdEncoding.DecodeString(parts[4])
	key, err2 := base64.RawStdEncoding.DecodeString(parts[5])
	if err1 != nil || err2 != nil {
		t.Fatalf("its salt or key isn't base64 without padding: %v %v", err1, err2)
	}
	got := argon2.IDKey([]byte(secret), salt, tm, m, p, uint32(len(key)))
	return subtle.ConstantTimeCompare(got, key) == 1
}

// An input's hash_of is Argon2id with the parameters OWASP recommends, as a PHC
// string an app's own check verifies the secret against, and nothing else; a salt of
// its own each time (ADR 0105, Vaultwarden's ADMIN_TOKEN).
func TestHashOfIsAnArgon2idPHCOfTheSecret(t *testing.T) {
	t.Parallel()
	a, err := HashOf("Ow9Kx7Vq2mRt9Lw4zBpN8cT3")
	if err != nil {
		t.Fatal(err)
	}
	b, _ := HashOf("Ow9Kx7Vq2mRt9Lw4zBpN8cT3")
	if !strings.HasPrefix(a, "$argon2id$v=19$m=19456,t=2,p=1$") || a == b {
		t.Errorf("HashOf = %q and %q, want OWASP's parameters and a salt each", a, b)
	}
	if !verifyPHC(t, a, "Ow9Kx7Vq2mRt9Lw4zBpN8cT3") || verifyPHC(t, a, "Ow9Kx7Vq2mRt9Lw4zBpN8cT4") {
		t.Error("the PHC doesn't verify the secret alone")
	}
	if strings.Contains(a, "Ow9Kx7") || strings.Contains(a, hex.EncodeToString([]byte("Ow9Kx7"))) {
		t.Errorf("the PHC holds the secret: %q", a)
	}
}

// An entry's hash input is written as it installs, from the secret it hashes, and
// never asked of a person, given by one, or written anew by an upgrade (ADR 0105).
func TestFillWritesAHashOfASecret(t *testing.T) {
	t.Parallel()
	e := &catalogv1.Entry{Name: "Vault", Inputs: []*catalogv1.Input{
		{Name: "ADMIN_TOKEN_PLAIN", Label: "Admin token", Type: catalogv1.InputType_INPUT_TYPE_SECRET, Generate: 48},
		{Name: "ADMIN_TOKEN", Label: "Admin token's hash", Type: catalogv1.InputType_INPUT_TYPE_SECRET, HashOf: "ADMIN_TOKEN_PLAIN"},
	}}
	if Asks(e.GetInputs()[1]) {
		t.Error("a hash is asked of a person")
	}
	f, err := Fill(e, nil, nil)
	if err != nil {
		t.Fatal(err)
	}
	plain, hash := f.Secrets["ADMIN_TOKEN_PLAIN"], f.Secrets["ADMIN_TOKEN"]
	if len(plain) != 48 || !verifyPHC(t, hash, plain) || f.Variables["ADMIN_TOKEN"] == hash || f.Plain["ADMIN_TOKEN"] != "" {
		t.Errorf("filled %d characters, a hash that verifies %v, stands in %v", len(plain), verifyPHC(t, hash, plain), f.Variables["ADMIN_TOKEN"] != hash)
	}
	if _, err := Fill(e, map[string]string{"ADMIN_TOKEN": "$argon2id$mine"}, nil); err == nil || !strings.Contains(err.Error(), "Leave ADMIN_TOKEN out") {
		t.Errorf("a hash given: %v", err)
	}
	// An upgrade keeps the hash the install has.
	kept, err := fill(e, nil, nil, map[string]bool{"ADMIN_TOKEN_PLAIN": true, "ADMIN_TOKEN": true})
	if err != nil || kept.Held["ADMIN_TOKEN"] == "" || kept.Secrets["ADMIN_TOKEN"] != "" {
		t.Errorf("an upgrade's fill: %v, %v", kept, err)
	}
}

// The check refuses a hash that isn't a secret's of the entry's own.
func TestCheckRefusesAHashThatIsntASecrets(t *testing.T) {
	t.Parallel()
	secret := func(name, of string) *catalogv1.Input {
		return &catalogv1.Input{Name: name, Type: catalogv1.InputType_INPUT_TYPE_SECRET, HashOf: of}
	}
	for name, c := range map[string][]*catalogv1.Input{
		"of nothing":    {secret("H", "NONE")},
		"of itself":     {secret("H", "H")},
		"of a hash":     {secret("A", "B"), secret("B", "A")},
		"not a secret":  {{Name: "P", Type: catalogv1.InputType_INPUT_TYPE_SECRET, Generate: 32}, {Name: "H", Type: catalogv1.InputType_INPUT_TYPE_TEXT, HashOf: "P"}},
		"of plain text": {{Name: "P", Type: catalogv1.InputType_INPUT_TYPE_TEXT}, secret("H", "P")},
		"generating":    {{Name: "P", Type: catalogv1.InputType_INPUT_TYPE_SECRET, Generate: 32}, {Name: "H", Type: catalogv1.InputType_INPUT_TYPE_SECRET, HashOf: "P", Generate: 32}},
	} {
		e := &catalogv1.Entry{Inputs: c}
		if err := checkHashOf(e, c[len(c)-1]); err == nil {
			t.Errorf("%s: took it", name)
		}
	}
	ok := &catalogv1.Entry{Inputs: []*catalogv1.Input{{Name: "P", Type: catalogv1.InputType_INPUT_TYPE_SECRET, Generate: 32}, secret("H", "P")}}
	if err := checkHashOf(ok, ok.GetInputs()[1]); err != nil {
		t.Error(err)
	}
}
