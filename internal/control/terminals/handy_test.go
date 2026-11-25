// SPDX-License-Identifier: AGPL-3.0-only

package terminals

import "testing"

func TestLooksLikeSecret(t *testing.T) {
	t.Parallel()

	secrets := []string{
		"export OPENAI_API_KEY=sk-1234567890abcdefghij1234",
		"curl -H 'Authorization: Bearer eyJhbGciOiJIUzI1NiJ9.eyJzdWIiOiIxMjM0NTY3ODkwIn0.abc1234567890' https://api.example.com",
		"DATABASE_URL=postgres://admin:s3cr3tpass@db.internal:5432/app psql",
		"curl -u alice:hunter2 https://api.example.com/status",
		"docker login -u bot --password supersecretvalue ghcr.io",
		"GH_TOKEN=ghp_1234567890abcdefghij1234567890abcd gh pr list",
		"redis-cli -a myredispass ping",
		"cat <<'EOF'\n-----BEGIN OPENSSH PRIVATE KEY-----\nabc\n-----END OPENSSH PRIVATE KEY-----\nEOF",
	}
	for _, cmd := range secrets {
		if ok, _ := LooksLikeSecret(cmd); !ok {
			t.Errorf("LooksLikeSecret(%q) = false, want true", cmd)
		}
	}

	safe := []string{
		"docker ps --format 'table {{.Names}}\\t{{.Image}}\\t{{.Status}}'",
		"df -h",
		"journalctl -u dockwell -n 100 --no-pager",
		"export API_TOKEN=$API_TOKEN",
		"export DB_PASSWORD=${DB_PASSWORD}",
		"curl -H 'Authorization: Bearer <token>' https://api.example.com",
		"DATABASE_URL=postgres://user:changeme@localhost/db psql",
		"mysql -u root -p",
		"docker logs -f --tail 200 api",
		"systemctl restart caddy",
	}
	for _, cmd := range safe {
		if ok, why := LooksLikeSecret(cmd); ok {
			t.Errorf("LooksLikeSecret(%q) = true (%s), want false", cmd, why)
		}
	}
}

func TestValidateCommandFieldsControlChars(t *testing.T) {
	t.Parallel()

	// CRLF folds to LF, trailing newlines are stripped, and tabs + embedded LF are allowed.
	name, body, group, err := validateCommandFields(" Multi ", "cd /srv\r\n\tdocker ps\r\n\n", " Ops ")
	if err != nil {
		t.Fatalf("valid multiline command failed: %v", err)
	}
	if name != "Multi" || body != "cd /srv\n\tdocker ps" || group != "Ops" {
		t.Fatalf("got (%q, %q, %q), want (%q, %q, %q)", name, body, group, "Multi", "cd /srv\n\tdocker ps", "Ops")
	}

	// Control characters below 0x20 (except LF and TAB) and DEL (0x7f) must be rejected.
	badBodies := []string{
		"echo ok\x00evil",
		"echo ok\rrm -rf /",
		"echo \x1b[31mred",
		"echo \x03break",
		"echo \x15erase",
		"echo \x04eof",
		"echo \x07bell",
		"echo \x7fdel",
		"echo \u009b31mred",              // C1 CSI
		"echo safe \u202e# fr- mr\u202c", // reordered drawing
		"rm\u200b -rf /tmp/x",            // zero width
		"echo \u2066hidden\u2069",
	}
	if _, _, _, err := validateCommandFields("Fine", "echo café 日本", ""); err != nil {
		t.Errorf("ordinary Unicode refused: %v", err)
	}
	for _, bad := range badBodies {
		if _, _, _, err := validateCommandFields("Bad", bad, ""); err == nil {
			t.Errorf("validateCommandFields accepted control characters in %q", bad)
		}
	}
}
