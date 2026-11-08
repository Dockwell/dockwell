// SPDX-License-Identifier: AGPL-3.0-only

package convert

import (
	"slices"
	"strings"
	"testing"
	"time"

	catalogv1 "github.com/dockwell/dockwell/gen/dockwell/catalog/v1alpha1"
)

// The follow-ups the narrow check of #302 asked for (#322).

// A name holds a secret when one of its words is a secret's, and no word after it
// says the name is about the secret instead.
func TestASecretIsNamedByItsWords(t *testing.T) {
	for _, name := range []string{
		"ADMIN_PASSWORD", "RABBITMQ_DEFAULT_PASS", "AES_PWD", "NPS_VKEY", "SSH_PASSPHRASE", "OPENAI_APIKEY",
		"SECRET_KEY_BASE", "JWT_SECRET", "HASH_SALT", "REGISTRATION_TOKEN", "API_KEY", "PRIVATE_KEY", "db_password",
	} {
		if !secretNamed(name) {
			t.Errorf("%s reads as no secret", name)
		}
	}
	for _, name := range []string{
		"TOKEN_URL", "MAX_TOKENS", "SSH_KEY_PATH", "PASSWORD_MIN_LENGTH", "SALT_ROUNDS", "ACCESS_KEY_ID",
		"PUBLIC_KEY", "KEYCLOAK_ADMIN", "PASSENGER", "MONKEY", "NACOS_AUTH_IDENTITY_VALUE", "TOKEN_EXPIRY",
	} {
		if secretNamed(name) {
			t.Errorf("%s reads as a secret", name)
		}
	}
	// 1Panel's text field named as one is a secret, generated where 1Panel makes one up.
	f := onePanelFieldOf(&onePanelField{EnvKey: "RABBITMQ_DEFAULT_PASS", Type: "text", Default: "rabbitmq", Random: true}, false)
	if f.Kind != Generated {
		t.Errorf("RABBITMQ_DEFAULT_PASS is %v", f.Kind)
	}
	if f := onePanelFieldOf(&onePanelField{EnvKey: "TOKEN_URL", Type: "text", Default: "https://auth.example.com/token"}, false); f.Kind != Text || f.Default == "" {
		t.Errorf("TOKEN_URL is %v, %q", f.Kind, f.Default)
	}
}

// Versions read as people mean them: a date or a calendar version is one line, a
// suffix that names another product's version is part of the major, a variant is
// a line of its own, and a pre-release comes before its release.
func TestVersionsReadAsTheirMajors(t *testing.T) {
	for _, same := range [][2]string{
		{"20250803", "20251101"},
		{"2026.10.4", "2027.1.0"},
		{"2024.10.22-7ca5933", "2025.1.3-1a2b3c4"},
		{"13.22-alpine", "13.23-alpine"},
		{"1.6.22-alpine3.18", "1.6.23-alpine3.19"},
		{"v2.1", "2.9"},
		{"6.2.0-standalone", "6.3.0-standalone"},
	} {
		if majorOf(same[0]) != majorOf(same[1]) {
			t.Errorf("%s and %s read as different majors: %q, %q", same[0], same[1], majorOf(same[0]), majorOf(same[1]))
		}
	}
	for _, other := range [][2]string{
		{"0.8.7-pg13", "0.8.7-pg18"},
		{"1.6.0", "1.6.0-postgres"},
		{"13.23", "18.6"},
		{"0.30.4", "0.31.0"},
	} {
		if majorOf(other[0]) == majorOf(other[1]) {
			t.Errorf("%s and %s read as one major, %q", other[0], other[1], majorOf(other[0]))
		}
	}
	versions := []string{"2.0.0", "2.0.0-beta.2", "1.10.0", "2.0.0-rc.1", "1.9.0", "v2.0.1"}
	slices.SortFunc(versions, compareVersions)
	if want := []string{"1.9.0", "1.10.0", "2.0.0-beta.2", "2.0.0-rc.1", "2.0.0", "v2.0.1"}; !slices.Equal(versions, want) {
		t.Errorf("versions sort as %q, want %q", versions, want)
	}
	// An entry at a date moves to a later one, as it's one line.
	c := converter()
	c.Overlays["a"] = written("Stands in", "ai")
	first := c.Convert(t.Context(), youngApp("20250803"))
	c.Previous = map[string]*catalogv1.Entry{"a": first.Entry}
	if r := c.Convert(t.Context(), youngApp("20251101")); r.Entry.GetVersion() != "20251101" || r.Held != "" {
		t.Errorf("a date didn't move on: %s, %q", r.Entry.GetVersion(), r.Held)
	}
}

// A value a person types into what decides a service's reach, or a volume's or a
// network's, reaches the server, whatever its default.
func TestATypedValueReachesTheServer(t *testing.T) {
	for name, tc := range map[string]struct{ file, reaches string }{
		"privileged":         {"services:\n  a:\n    image: example/a:1.0\n    privileged: ${PRIV:-false}\n", "a: its privileged, as a person sets it, ${PRIV:-false}"},
		"network_mode":       {"services:\n  a:\n    image: example/a:1.0\n    network_mode: ${NET:-bridge}\n", "a: its network_mode, as a person sets it, ${NET:-bridge}"},
		"a volume's driver":  {"services:\n  a:\n    image: example/a:1.0\n    volumes:\n      - data:/data\nvolumes:\n  data:\n    driver: ${DRIVER:-local}\n", "volume data: its driver, as a person sets it, ${DRIVER:-local}"},
		"a volume's device":  {"services:\n  a:\n    image: example/a:1.0\n    volumes:\n      - data:/data\nvolumes:\n  data:\n    driver_opts:\n      type: none\n      o: bind\n      device: ${DATA_DIR:-/srv/data}\n", "volume data: a server path a person names, ${DATA_DIR:-/srv/data}"},
		"a network's driver": {"services:\n  a:\n    image: example/a:1.0\n    networks: [n]\nnetworks:\n  n:\n    driver: ${NETDRIVER:-bridge}\n", "network n: its driver, as a person sets it, ${NETDRIVER:-bridge}"},
	} {
		t.Run(name, func(t *testing.T) {
			r := convertPlain(t, converter(), tc.file)
			if r.Entry != nil || r.Class != ClassReaches || !strings.Contains(r.LeftOut, tc.reaches) {
				t.Errorf("%s: %q, want it to name %q", r.Class, r.LeftOut, tc.reaches)
			}
			// Not what a stand-in reads as.
			if strings.Contains(r.LeftOut, "/srv/data\"") || strings.Contains(r.LeftOut, "stand-in") {
				t.Errorf("it names a stand-in's reading: %q", r.LeftOut)
			}
		})
	}
}

// The lows: a day is a day, a source's comments go, and an example in another
// language is no default.
func TestTheFollowUpsLows(t *testing.T) {
	if days(36*time.Hour) != "1 day" || days(72*time.Hour) != "3 days" {
		t.Errorf("days: %q, %q", days(36*time.Hour), days(72*time.Hour))
	}
	r := convertPlain(t, converter(), "# the app\nservices:\n  a:\n    # its image\n    image: example/a:1.0 # pinned later\n")
	if r.Entry == nil || strings.Contains(r.Entry.GetCompose(), "#") {
		t.Errorf("its file keeps a comment:\n%s", r.Entry.GetCompose())
	}
	if f := onePanelFieldOf(&onePanelField{EnvKey: "OLLAMA_URL", Type: "text", Default: "例如: http://127.0.0.1:11434"}, false); f.Default != "" {
		t.Errorf("an example is a default: %q", f.Default)
	}
}
