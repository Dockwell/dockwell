// SPDX-License-Identifier: AGPL-3.0-only

package convert

import (
	"testing"

	"github.com/dockwell/dockwell/internal/catalog"
)

// Every name the converter takes for a secret, a read hides too: what's shown of an
// install is decided by catalog.SecretNamed, which takes this as its floor (ADR 0081).
func TestEverySecretTheConverterSeesStaysHidden(t *testing.T) {
	names := []string{
		"ADMIN_PASSWORD", "RABBITMQ_DEFAULT_PASS", "SECRET_KEY_BASE", "PGPASSWORD", "MINIO_SECRETKEY", "JWTSECRET",
		"SERVICEACCOUNTTOKEN", "ACCESS_CODE", "PASS_CODE", "VAULT_TOKEN_ID", "API_KEY", "APP_VKEY", "DB_PWD",
		"PASSPHRASE", "PASSWORD_SALT", "ACCESSTOKEN", "PASSCODE", "PRIVATEKEY", "INIT_CLIENT_API_TOKENS",
	}
	for _, words := range [][]string{secretWords, joinedSecrets, joinedWords} {
		for _, w := range words {
			names = append(names, w, "APP_"+w, "APP"+w)
		}
	}
	for _, name := range names {
		if secretNamed(name) && !catalog.SecretNamed(name) {
			t.Errorf("the converter takes %s for a secret, and a read would show it", name)
		}
	}
}
