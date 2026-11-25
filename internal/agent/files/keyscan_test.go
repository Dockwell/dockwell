// SPDX-License-Identifier: AGPL-3.0-only

package files

import (
	"archive/zip"
	"bytes"
	"encoding/base64"
	"fmt"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"sync/atomic"
	"testing"
	"unicode/utf8"

	"golang.org/x/sys/unix"
	"google.golang.org/protobuf/proto"

	agentv1 "github.com/dockwell/dockwell/gen/dockwell/agent/v1"
	dockwellv1 "github.com/dockwell/dockwell/gen/dockwell/v1"
)

type keyFormCase struct {
	name   string
	body   string
	secret string
}

func allPrivateKeyForms() []keyFormCase {
	rsaPub := strings.Repeat("AAAAB3NzaC1yc2EAAAABJQAAAQEAr1234567890abcdefghijklmnopqrstuvwx\n", 18) // > 1 KiB RSA public key
	ecPEM := "-----BEGIN EC PRIVATE KEY-----\nMHcCAQEEIKubeConfigClientKeySecretPayload0123456789abcdefghijk\n-----END EC PRIVATE KEY-----\n"
	kubeCertPEM := "-----BEGIN CERTIFICATE-----\nMIIBIjANBgkqhkiG9w0BAQEFAAOCAQ8AMIIBCgKCAQEAExampleCertData==\n-----END CERTIFICATE-----\n"
	kubeCertB64 := base64.StdEncoding.EncodeToString([]byte(kubeCertPEM))
	kubeKeyB64 := base64.StdEncoding.EncodeToString([]byte(ecPEM))
	wrappedB64 := func(raw string) string {
		s := base64.StdEncoding.EncodeToString([]byte(raw))
		var b strings.Builder
		for i := 0; i < len(s); i += 64 {
			if i > 0 {
				b.WriteByte('\n')
			}
			b.WriteString(s[i:min(len(s), i+64)])
		}
		return b.String()
	}
	return []keyFormCase{
		{
			name:   "gcp-service-account-json",
			body:   "{\n  \"type\": \"service_account\",\n  \"project_id\": \"prod-123\",\n  \"private_key_id\": \"abc123\",\n  \"private_key\": \"-----BEGIN PRIVATE KEY-----\\nMIIEvQIBADANBgkqhkiG9w0BAQEFAASCBKcwggSjAgEAAoIB\\n-----END PRIVATE KEY-----\\n\",\n  \"client_email\": \"svc@prod-123.iam.gserviceaccount.com\"\n}\n",
			secret: "MIIEvQIBADANBgkq",
		},
		{
			name:   "gcp-service-account-json-minified-past-512",
			body:   `{"pad":"` + strings.Repeat("p", 700) + `","private_key":"-----BEGIN PRIVATE KEY-----\r\nMIIEvQIBADANBgkqhkiG9w0BAQEFAASCBKcwggSjAgEAAoIB\r\n-----END PRIVATE KEY-----\r\n"}`,
			secret: "MIIEvQIBADANBgkq",
		},
		{
			name:   "gcp-service-account-json-double-quoted",
			body:   "services:\n  api:\n    environment:\n      GCP_KEY: \"{\\\"type\\\":\\\"service_account\\\",\\\"private_key\\\":\\\"-----BEGIN PRIVATE KEY-----\\\\nMIIEvQIBADANBgkqhkiG9w0DoubleQuotedComposeSecret\\\\n-----END PRIVATE KEY-----\\\\n\\\"}\"\n",
			secret: "DoubleQuotedComposeSecret",
		},
		{
			name:   "gcp-service-account-json-double-quoted-crlf",
			body:   `{"nested":"{\"private_key\":\"-----BEGIN PRIVATE KEY-----\\r\\nMIIEvQIBADANBgkqhkiG9w0DoubleQuotedCrlfSecret\\r\\n-----END PRIVATE KEY-----\\r\\n\"}"}` + "\n",
			secret: "DoubleQuotedCrlfSecret",
		},
		{
			name:   "php-json-escaped-slash",
			body:   `{"private_key":"-----BEGIN EC PRIVATE KEY-----\nMHcCAQEE\/ab\/cdPhpJsonEscapedSlashSecret\n-----END EC PRIVATE KEY-----\n"}` + "\n",
			secret: "PhpJsonEscapedSlashSecret",
		},
		{
			name:   "dotnet-json-escaped-plus-unicode",
			body:   `{"private_key":"-----BEGIN EC PRIVATE KEY-----\nMHcCAQEE\u002Bab\u002BcdDotnetJsonEscapedPlusSecret\n-----END EC PRIVATE KEY-----\n"}` + "\n",
			secret: "DotnetJsonEscapedPlusSecret",
		},
		{
			name:   "json-escaped-literal-plus",
			body:   `{"private_key":"-----BEGIN EC PRIVATE KEY-----\nMHcCAQEE\+ab\+cdLiteralEscapedPlusSecret\n-----END EC PRIVATE KEY-----\n"}` + "\n",
			secret: "LiteralEscapedPlusSecret",
		},
		{
			name:   "kubeconfig-client-key-data",
			body:   "apiVersion: v1\nkind: Config\nclusters:\n- cluster:\n    certificate-authority-data: " + kubeCertB64 + "\nusers:\n- name: default\n  user:\n    client-certificate-data: " + kubeCertB64 + "\n    client-key-data: " + kubeKeyB64 + "\n",
			secret: kubeKeyB64[len(kubeKeyB64)-24:],
		},
		{
			name:   "k8s-secret-tls-key-wrapped-base64",
			body:   "apiVersion: v1\nkind: Secret\ndata:\n  tls.crt: " + kubeCertB64 + "\n  tls.key: |\n    " + strings.ReplaceAll(wrappedB64(ecPEM), "\n", "\n    ") + "\n",
			secret: kubeKeyB64[len(kubeKeyB64)-24:],
		},
		{
			name:   "base64-pem-offset-1",
			body:   "key_b64=" + base64.StdEncoding.EncodeToString([]byte("\n"+ecPEM)) + "\n",
			secret: base64.StdEncoding.EncodeToString([]byte("\n" + ecPEM))[len(base64.StdEncoding.EncodeToString([]byte("\n"+ecPEM)))-24:],
		},
		{
			name:   "base64-pem-offset-2",
			body:   "key_b64=" + base64.StdEncoding.EncodeToString([]byte("  "+ecPEM)) + "\n",
			secret: base64.StdEncoding.EncodeToString([]byte("  " + ecPEM))[len(base64.StdEncoding.EncodeToString([]byte("  "+ecPEM)))-24:],
		},
		{
			name:   "pgp-private-key-with-headers",
			body:   "-----BEGIN PGP PRIVATE KEY BLOCK-----\nVersion: GnuPG v2\nComment: signing key\n\nlQOYBGP1234567890abcdefSECRETpgpKEYpayload==\n=abcd\n-----END PGP PRIVATE KEY BLOCK-----\n",
			secret: "SECRETpgpKEYpayload",
		},
		{
			name:   "pgp-private-key-modern-blank-line",
			body:   "-----BEGIN PGP PRIVATE KEY BLOCK-----\n\nlQOYBGP1234567890abcdefMODERNpgpKEYpayload==\n=ef01\n-----END PGP PRIVATE KEY BLOCK-----\n",
			secret: "MODERNpgpKEYpayload",
		},
		{
			name:   "pgp-private-key-json-escaped",
			body:   `{"pgp":"-----BEGIN PGP PRIVATE KEY BLOCK-----\n\nlQOYBGP1234567890abcdefESCAPEDpgpKEYpayload==\n-----END PGP PRIVATE KEY BLOCK-----\n"}`,
			secret: "ESCAPEDpgpKEYpayload",
		},
		{
			name:   "putty-ppk-v2",
			body:   "PuTTY-User-Key-File-2: ssh-rsa\nEncryption: none\nComment: prod-rsa\nPublic-Lines: 18\n" + rsaPub + "Private-Lines: 14\nAAABAQCPuttyV2SecretPrivateKeyMaterialHere1234567890==\nPrivate-MAC: 0123456789abcdef\n",
			secret: "PuttyV2SecretPrivateKey",
		},
		{
			name:   "putty-ppk-v3-argon2",
			body:   "PuTTY-User-Key-File-3: ssh-ed25519\nEncryption: aes256-cbc\nComment: ops-ed25519\nPublic-Lines: 2\nAAAAC3NzaC1lZDI1NTE5AAAAIExamplePublicKeyDataHere\nKey-Derivation: Argon2id\nArgon2-Memory: 8192\nArgon2-Passes: 13\nArgon2-Parallelism: 1\nArgon2-Salt: 0123456789abcdef\nPrivate-Lines: 4\nPuttyV3SecretPrivateKeyMaterialHere1234567890==\nPrivate-MAC: fedcba9876543210\n",
			secret: "PuttyV3SecretPrivateKey",
		},
		{
			name:   "putty-ppk-json-escaped",
			body:   `{"ppk":"PuTTY-User-Key-File-3: ssh-ed25519\nEncryption: none\nPublic-Lines: 1\nAAAAC3NzaC1lZDI1NTE5AAAA\nPrivate-Lines: 2\nPuttyEscapedSecretKeyData==\n"}`,
			secret: "PuttyEscapedSecretKeyData",
		},
		{
			name:   "ssh2-secsh-encrypted-with-headers",
			body:   "---- BEGIN SSH2 ENCRYPTED PRIVATE KEY ----\nSubject: deploy\nComment: \"2048-bit RSA, converted from OpenSSH\"\nP2/56wAAASecshEncryptedSecretKeyPayload==\n---- END SSH2 ENCRYPTED PRIVATE KEY ----\n",
			secret: "SecshEncryptedSecretKey",
		},
		{
			name:   "ssh2-secsh-encrypted-direct-base64",
			body:   "---- BEGIN SSH2 ENCRYPTED PRIVATE KEY ----\nP2/56wAAASecshDirectSecretKeyPayloadHere==\n---- END SSH2 ENCRYPTED PRIVATE KEY ----\n",
			secret: "SecshDirectSecretKey",
		},
		{
			name:   "ssh2-secsh-json-escaped",
			body:   `{"ssh2":"---- BEGIN SSH2 ENCRYPTED PRIVATE KEY ----\nComment: \"rsa\"\nP2/56wAAASecshJsonSecretKeyPayload==\n---- END SSH2 ENCRYPTED PRIVATE KEY ----\n"}`,
			secret: "SecshJsonSecretKey",
		},
		{
			name:   "env-quoted-multiline-double",
			body:   "APP_ENV=production\nTLS_KEY=\"-----BEGIN RSA PRIVATE KEY-----\nMIIEowIBAAKCAQEAwJh2EnvMultilineSecret\n-----END RSA PRIVATE KEY-----\"\n",
			secret: "EnvMultilineSecret",
		},
		{
			name:   "env-quoted-multiline-single",
			body:   "APP_ENV=production\nTLS_KEY='-----BEGIN EC PRIVATE KEY-----\nMHcCAQEEIPEnvSingleQuotedSecretKey\n-----END EC PRIVATE KEY-----'\n",
			secret: "EnvSingleQuotedSecretKey",
		},
		{
			name:   "env-quoted-escaped-newlines",
			body:   "APP_ENV=production\nTLS_KEY=\"-----BEGIN PRIVATE KEY-----\\nMIIEvgIBADANBgkqhkiG9w0EnvEscapedSecret\\n-----END PRIVATE KEY-----\\n\"\n",
			secret: "EnvEscapedSecret",
		},
		{
			name:   "env-quoted-short-with-end",
			body:   "TLS_KEY=\"-----BEGIN PRIVATE KEY-----\nMIIE\n-----END PRIVATE KEY-----\"\n",
			secret: "MIIE",
		},
		{
			name:   "short-base64-lines-8-chars",
			body:   "-----BEGIN PRIVATE KEY-----\nMIIEvgIB\nADANBgkq\nhkiG9w0B\n-----END PRIVATE KEY-----\n",
			secret: "ADANBgkq",
		},
		{
			name:   "short-base64-lines-4-chars-indented",
			body:   "KEY=\"-----BEGIN RSA PRIVATE KEY-----\n  MIIE\n  vgIB\n  ADAN\n  Bgkq\n  hkiG\n\"\n",
			secret: "Bgkq",
		},
		{
			name:   "short-base64-lines-json-escaped",
			body:   `{"k":"-----BEGIN PRIVATE KEY-----\nMIIEvgIB\nADANBgkq\nhkiG9w0B\n-----END PRIVATE KEY-----\n"}`,
			secret: "ADANBgkq",
		},
	}
}

// Every private key form in #936 (M2 + L1) is refused across reads/editor,
// saves, search, binary previews, single downloads, single-file selection zips,
// single-file archive jobs, and permission widening.
func TestAllPrivateKeyFormsRefusedAcrossSurfaces(t *testing.T) {
	d := newDisk(t)
	d.noAccounts()
	site := filepath.Join(d.root, "site")
	if err := os.MkdirAll(site, 0o755); err != nil {
		t.Fatal(err)
	}
	pngHead := []byte{0x89, 'P', 'N', 'G', '\r', '\n', 0x1a, '\n', 0x00, 0x00, 0x00, 0x0d}

	for _, tc := range allPrivateKeyForms() {
		t.Run(tc.name, func(t *testing.T) {
			p := filepath.Join(site, tc.name+".txt")
			write(t, p, tc.body)

			// 1. Read / editor
			if got := d.read(t, p); got.GetRefusal().GetKind() != agentv1.FileRefusal_KIND_REFUSED {
				t.Errorf("Read = %v, want KIND_REFUSED", got.GetRefusal())
			}

			// 2. Save (cannot overwrite a file holding a private key)
			if saveRes, err := d.server.Save(t.Context(), p, "harmless\n", "any", "", ""); err != nil || saveRes.GetRefusal().GetKind() != agentv1.FileRefusal_KIND_REFUSED {
				t.Errorf("Save = %v, %v, want KIND_REFUSED", saveRes.GetRefusal(), err)
			}

			// 3. Search omits the file
			found, err := d.server.SearchFiles(t.Context(), site, tc.secret, true)
			if err != nil {
				t.Fatalf("SearchFiles: %v", err)
			}
			for _, m := range found.GetResponse().GetMatches() {
				if m.GetPath() == p {
					t.Errorf("SearchFiles matched refused key file %s", p)
				}
			}

			// 4. Single download refuses before sending any frame
			if sent, data, out := d.download(t, p); out.GetRefusal().GetKind() != agentv1.FileRefusal_KIND_REFUSED || len(sent) != 0 || bytes.Contains(data, []byte(tc.secret)) {
				t.Errorf("Download = refusal=%v sent=%d leaked=%v", out.GetRefusal(), len(sent), bytes.Contains(data, []byte(tc.secret)))
			}

			// 5. Binary preview (ReadWithOptions allowBinary=true) refuses embedded key
			imgPath := filepath.Join(site, tc.name+".png")
			if err := os.WriteFile(imgPath, append(bytes.Clone(pngHead), tc.body...), 0o644); err != nil {
				t.Fatal(err)
			}
			if prev, err := d.server.ReadWithOptions(t.Context(), imgPath, true); err != nil || prev.GetRefusal().GetKind() != agentv1.FileRefusal_KIND_REFUSED {
				t.Errorf("ReadWithOptions(allowBinary) = %v, %v, want KIND_REFUSED", prev.GetRefusal(), err)
			}

			// 6. Single-file selection zip (DownloadMany) refuses cleanly before sending
			sentAny := false
			dm, err := d.server.DownloadMany(t.Context(), []string{p}, func(*agentv1.SendFileRequest) error {
				sentAny = true
				return nil
			})
			if err != nil || dm.GetRefusal().GetKind() != agentv1.FileRefusal_KIND_REFUSED || sentAny {
				t.Errorf("DownloadMany single = %v, %v, sent=%v", dm.GetRefusal(), err, sentAny)
			}

			// 7. Single-file Compress archive job refuses cleanly with no zip created
			arcPath := filepath.Join(site, tc.name+".zip")
			comp, err := d.server.Compress(t.Context(), []string{p}, arcPath, "", testArchiveOwner)
			if err != nil || comp.GetRefusal().GetKind() != agentv1.FileRefusal_KIND_REFUSED || there(arcPath) {
				t.Errorf("Compress single = %v, %v, exists=%v", comp.GetRefusal(), err, there(arcPath))
			}

			// 8. Tar archive containing the key is refused on single download
			tarPath := filepath.Join(site, tc.name+".tar")
			if err := os.WriteFile(tarPath, tarOf(t, []struct{ name, body string }{
				{name: "payload.dat", body: tc.body},
			}), 0o600); err != nil {
				t.Fatal(err)
			}
			if sent, data, out := d.download(t, tarPath); out.GetRefusal().GetKind() != agentv1.FileRefusal_KIND_REFUSED || len(sent) != 0 || bytes.Contains(data, []byte(tc.secret)) {
				t.Errorf("Download tar = refusal=%v sent=%d leaked=%v", out.GetRefusal(), len(sent), bytes.Contains(data, []byte(tc.secret)))
			}

			// 9. Widening permissions requires passkey (protectedHead)
			if err := os.Chmod(p, 0o600); err != nil {
				t.Fatal(err)
			}
			widened, err := d.server.ChangeFileAccess(t.Context(), &agentv1.ChangeFileAccessCall{
				Path: p, Change: &dockwellv1.FileAccessChange{Mode: proto.Uint32(0o644)},
			})
			if err != nil || widened.GetRefusal().GetKind() != agentv1.FileRefusal_KIND_NEEDS_STEP_UP {
				t.Errorf("ChangeFileAccess widen = %v, %v, want KIND_NEEDS_STEP_UP", widened.GetRefusal(), err)
			}
		})
	}
}

// Every private key form split across FrameBytes boundaries and across tiny
// chunks is caught before the key bytes go out.
func TestAllPrivateKeyFormsSplitAcrossParts(t *testing.T) {
	d := newDisk(t)
	d.noAccounts()
	site := filepath.Join(d.root, "site")
	if err := os.MkdirAll(site, 0o755); err != nil {
		t.Fatal(err)
	}

	for _, tc := range allPrivateKeyForms() {
		t.Run(tc.name, func(t *testing.T) {
			// Tiny-chunk streaming (7 bytes per chunk) through keyScan directly.
			raw := append([]byte{0x00, 0x01, 0x02}, tc.body...)
			var scan keyScan
			var tripped *agentv1.FileRefusal
			for off := 0; off < len(raw); off += 7 {
				end := min(len(raw), off+7)
				if r := scan.check("/x", raw[off:end]); r != nil {
					tripped = r
					break
				}
			}
			if tripped == nil {
				tripped = scan.finish("/x")
			}
			if tripped == nil || tripped.GetKind() != agentv1.FileRefusal_KIND_REFUSED {
				t.Errorf("tiny-chunk keyScan missed %s", tc.name)
			}

			// Split across the FrameBytes (128 KiB) boundary in a binary file download.
			for _, offset := range []int{10, 28, len(tc.body) / 2} {
				if offset >= len(tc.body) || offset <= 0 {
					continue
				}
				prefixLen := FrameBytes - offset
				blob := make([]byte, prefixLen)
				for i := range blob {
					blob[i] = byte(i%251 + 1)
				}
				blob[0] = 0 // binary, no newline before key
				blob = append(blob, tc.body...)
				p := filepath.Join(site, tc.name+".bin")
				if err := os.WriteFile(p, blob, 0o600); err != nil {
					t.Fatal(err)
				}
				_, data, out := d.download(t, p)
				if out.GetRefusal().GetKind() != agentv1.FileRefusal_KIND_REFUSED {
					t.Errorf("FrameBytes split offset=%d: refusal=%v, want KIND_REFUSED", offset, out.GetRefusal())
				}
				if bytes.Contains(data, []byte(tc.secret)) {
					t.Errorf("FrameBytes split offset=%d: leaked secret %q", offset, tc.secret)
				}
			}

			// Also verify text file > 128 KiB with key past 128 KiB (within first MiB)
			// is refused BEFORE any byte goes out.
			bigText := strings.Repeat("config=ok\n", 15_000) + tc.body
			tp := filepath.Join(site, tc.name+"-big.txt")
			write(t, tp, bigText)
			if sent, data, out := d.download(t, tp); out.GetRefusal().GetKind() != agentv1.FileRefusal_KIND_REFUSED || len(sent) != 0 || len(data) != 0 {
				t.Errorf("text >128KiB download: refusal=%v sent=%d bytes=%d, want KIND_REFUSED and 0 sent", out.GetRefusal(), len(sent), len(data))
			}
		})
	}
}

// False-positive guard: ssh/puttygen/gpg binary strings, prose mentioning key
// formats, public keys, certificates, and random data must not trip the scan.
func TestKeyScanFalsePositiveGuards(t *testing.T) {
	d := newDisk(t)
	d.noAccounts()
	site := filepath.Join(d.root, "site")
	if err := os.MkdirAll(site, 0o755); err != nil {
		t.Fatal(err)
	}

	kubeCertB64 := base64.StdEncoding.EncodeToString([]byte("-----BEGIN CERTIFICATE-----\nMIIBIjANBgkqhkiG9w0BAQEFAAOCAQ8AMIIBCgKCAQEAExampleCertData==\n-----END CERTIFICATE-----\n"))
	pubKeyB64 := base64.StdEncoding.EncodeToString([]byte("-----BEGIN PUBLIC KEY-----\nMIIBIjANBgkqhkiG9w0BAQEFAAOCAQ8AMIIBCgKCAQEAExamplePubKey==\n-----END PUBLIC KEY-----\n"))
	hdrOnlyB64 := base64.StdEncoding.EncodeToString([]byte("-----BEGIN PRIVATE KEY-----\n"))

	textAllowed := []struct {
		name string
		body string
	}{
		{"prose-pem", "Paste -----BEGIN PRIVATE KEY----- here\n"},
		{"prose-gcp", `Google Cloud JSON has "private_key": "-----BEGIN PRIVATE KEY-----\n..." with escapes.` + "\n"},
		{"prose-gcp-placeholder", `{"private_key": "-----BEGIN PRIVATE KEY-----\n<paste your key>\n-----END PRIVATE KEY-----\n"}` + "\n"},
		{"prose-pgp", "PGP keys start with -----BEGIN PGP PRIVATE KEY BLOCK----- in RFC 4880.\n"},
		{"prose-ssh2", "SSH2 SECSH keys use ---- BEGIN SSH2 ENCRYPTED PRIVATE KEY ---- as a header.\n"},
		{"prose-putty", "PuTTY .ppk uses PuTTY-User-Key-File- with Private-Lines: for keys.\n"},
		{"putty-header-only", "PuTTY-User-Key-File-3: ssh-ed25519\nEncryption: none\nComment: no private lines here\n"},
		{"putty-zero-private-lines", "PuTTY-User-Key-File-2: ssh-rsa\nEncryption: none\nPrivate-Lines: 0\n"},
		{"putty-prose-after-count", "PuTTY-User-Key-File-2: ssh-rsa\nPrivate-Lines: 4 is the line count\n"},
		{"pem-public-key", "-----BEGIN PUBLIC KEY-----\nMIIBIjANBgkqhkiG9w0BAQEFAAOCAQ8AMIIBCgKCAQEA\n-----END PUBLIC KEY-----\n"},
		{"pgp-public-key", "-----BEGIN PGP PUBLIC KEY BLOCK-----\nVersion: GnuPG v2\n\nmQINBF1234567890abcdef1234567890abcdef==\n-----END PGP PUBLIC KEY BLOCK-----\n"},
		{"ssh2-public-key", "---- BEGIN SSH2 PUBLIC KEY ----\nComment: \"rsa-key\"\nAAAAB3NzaC1yc2EAAAADAQABAAABAQ==\n---- END SSH2 PUBLIC KEY ----\n"},
		{"certificate", "-----BEGIN CERTIFICATE-----\nMIIBIjANBgkqhkiG9w0BAQEFAAOCAQ8AMIIBCgKCAQEA\n-----END CERTIFICATE-----\n"},
		{"kubeconfig-certs-only", "apiVersion: v1\nkind: Config\nclusters:\n- cluster:\n    certificate-authority-data: " + kubeCertB64 + "\nusers:\n- name: viewer\n  user:\n    client-certificate-data: " + kubeCertB64 + "\n"},
		{"base64-pem-public-key", "pub=" + pubKeyB64 + "\n"},
		{"base64-pem-header-only", "example=" + hdrOnlyB64 + "\n"},
		{"go-code-header-const", "const h = \"-----BEGIN PRIVATE KEY-----\\n\"\nconst e = \"-----END PRIVATE KEY-----\\n\"\n"},
		{"short-word-then-prose", "prefix \"-----BEGIN PRIVATE KEY-----\nshort\nhere is prose\n"},
	}

	for _, tc := range textAllowed {
		t.Run("text/"+tc.name, func(t *testing.T) {
			p := filepath.Join(site, tc.name+".txt")
			write(t, p, tc.body)
			if got := d.read(t, p); got.GetFile() == nil {
				t.Errorf("Read refused benign text %s: %v", tc.name, got.GetRefusal())
			}
			if _, data, out := d.download(t, p); out.GetRefusal() != nil || string(data) != tc.body {
				t.Errorf("Download refused benign text %s: %v", tc.name, out.GetRefusal())
			}
		})
	}

	binAllowed := []struct {
		name string
		body []byte
	}{
		{"ssh-keygen-fmt", []byte("\x7fELF\x00-----BEGIN %s PRIVATE KEY-----\x00usage")},
		{"ssh-keygen-rodata", []byte("\x7fELF\x00-----BEGIN OPENSSH PRIVATE KEY-----\n\x00\x00\x00\x00-----END OPENSSH PRIVATE KEY-----\n\x00")},
		{"puttygen-rodata", []byte("\x7fELF\x00PuTTY-User-Key-File-%d: %s\n\x00Private-Lines: %d\n\x00")},
		{"gpg-rodata", []byte("\x7fELF\x00-----BEGIN PGP %s-----\n\x00")},
		{"ssh2-rodata", []byte("\x7fELF\x00---- BEGIN SSH2 ENCRYPTED PRIVATE KEY ----\n\x00\x00\x00")},
	}

	for _, tc := range binAllowed {
		t.Run("binary/"+tc.name, func(t *testing.T) {
			p := filepath.Join(site, tc.name+".bin")
			if err := os.WriteFile(p, tc.body, 0o600); err != nil {
				t.Fatal(err)
			}
			if _, data, out := d.download(t, p); out.GetRefusal() != nil || !bytes.Equal(data, tc.body) {
				t.Errorf("Download refused benign binary %s: %v", tc.name, out.GetRefusal())
			}
		})
	}
}

// keyScan.check allocates zero bytes per part (even when parts contain marker
// prefixes), and holdsPrivateKey allocates zero bytes per call (N2).
func TestKeyScanZeroAllocsPerPart(t *testing.T) {
	kubeCertB64 := base64.StdEncoding.EncodeToString([]byte("-----BEGIN CERTIFICATE-----\nMIIBIjANBgkqhkiG9w0BAQEFAAOCAQ8AMIIBCgKCAQEA\n-----END CERTIFICATE-----\n"))
	line := []byte("-----BEGIN CERTIFICATE----- PuTTY-User-Key-File-3: ssh-rsa cert=" + kubeCertB64 + "\n")
	part := bytes.Repeat(line, FrameBytes/len(line))
	var scan keyScan
	allocs := testing.AllocsPerRun(50, func() {
		if r := scan.check("/var/log/app.log", part); r != nil {
			t.Fatalf("unexpected refusal: %v", r)
		}
	})
	if allocs != 0 {
		t.Errorf("keyScan.check allocated %.1f times per part, want 0", allocs)
	}
	hpAllocs := testing.AllocsPerRun(50, func() {
		if holdsPrivateKey(part) {
			t.Fatal("holdsPrivateKey unexpectedly tripped on benign part")
		}
	})
	if hpAllocs != 0 {
		t.Errorf("holdsPrivateKey allocated %.1f times per call, want 0", hpAllocs)
	}
}

// H1: A password-hash line split at every offset across a 128 KiB part boundary
// (past the first MiB in plain text, in a tar, and in a binary) must be refused
// with zero bytes of the salt or hash leaving.
func TestPasswordHashSplitAcrossPartBoundaryLeaksNothing(t *testing.T) {
	d := newDisk(t)
	d.noAccounts()
	site := filepath.Join(d.root, "site")
	if err := os.MkdirAll(site, 0o755); err != nil {
		t.Fatal(err)
	}

	const hashEntry = "backup:$6$saltsalt$HASHSECRET0123456789abcdefghijklmnopqrstuvwxyz:19000:0:99999:7:::\n"
	assertNoHashLeaked := func(t *testing.T, label string, data []byte, out *agentv1.DownloadFileResult) {
		t.Helper()
		if out.GetRefusal().GetKind() != agentv1.FileRefusal_KIND_REFUSED {
			t.Fatalf("%s: refusal=%v, want KIND_REFUSED", label, out.GetRefusal())
		}
		if bytes.Contains(data, []byte("$6$s")) || bytes.Contains(data, []byte("saltsalt")) || bytes.Contains(data, []byte("HASHSECRET")) {
			t.Fatalf("%s: leaked hash bytes at tail %q", label, data[max(0, len(data)-80):])
		}
		if idx := bytes.Index(data, []byte("$6$")); idx >= 0 && idx != len(data)-3 {
			t.Fatalf("%s: leaked %d bytes past $6$: %q", label, len(data)-3-idx, data[idx:])
		}
	}

	// 1. Plain text past the first MiB (9th 128 KiB boundary), every offset.
	// Build one reusable 9*FrameBytes+len(hashEntry) text buffer to keep it fast.
	textBoundary := 9 * FrameBytes
	textPad := bytes.Repeat([]byte("a"), textBoundary)
	for i := 63; i < len(textPad); i += 64 {
		textPad[i] = '\n'
	}
	textPath := filepath.Join(site, "big-shadow.txt")
	for offset := 1; offset <= len(hashEntry); offset++ {
		prefixLen := textBoundary - offset
		buf := make([]byte, 0, prefixLen+len(hashEntry)+16)
		buf = append(buf, textPad[:prefixLen-1]...)
		buf = append(buf, '\n')
		buf = append(buf, hashEntry...)
		buf = append(buf, "after=1\n"...)
		if err := os.WriteFile(textPath, buf, 0o600); err != nil {
			t.Fatal(err)
		}
		_, data, out := d.download(t, textPath)
		assertNoHashLeaked(t, fmt.Sprintf("text>1MiB offset=%d", offset), data, out)
	}

	// 2. Binary file (after NUL with no newline, and after newline), every offset across 128 KiB.
	binPath := filepath.Join(site, "blob.bin")
	binPad := bytes.Repeat([]byte{0x01, 0x02, 0x7f, 0x80}, FrameBytes/4)
	for _, sep := range []byte{0x00, '\n'} {
		for offset := 1; offset <= len(hashEntry); offset++ {
			prefixLen := FrameBytes - offset
			buf := make([]byte, 0, prefixLen+len(hashEntry)+16)
			buf = append(buf, 0x00) // mark binary
			buf = append(buf, binPad[:prefixLen-2]...)
			buf = append(buf, sep)
			buf = append(buf, hashEntry...)
			buf = append(buf, 0x00, 0x00)
			if err := os.WriteFile(binPath, buf, 0o600); err != nil {
				t.Fatal(err)
			}
			_, data, out := d.download(t, binPath)
			assertNoHashLeaked(t, fmt.Sprintf("binary sep=%#x offset=%d", sep, offset), data, out)
		}
	}

	// 3. Tar archive: first member starts at byte 512; test every offset across
	// a 128 KiB boundary (via a padding member before shadow, and via keyScan
	// splits at every byte around byte 512 for the first member).
	tarFirst := tarOf(t, []struct{ name, body string }{
		{name: "shadow", body: hashEntry},
	})
	for split := 512; split <= 512+len(hashEntry); split++ {
		var scan keyScan
		r1 := scan.check("/t.tar", tarFirst[:split])
		var sent []byte
		if r1 == nil {
			sent = tarFirst[:split]
			r2 := scan.check("/t.tar", tarFirst[split:])
			if r2 == nil {
				r2 = scan.finish("/t.tar")
			}
			if r2 == nil {
				t.Fatalf("tar-first split=%d: missed hash line", split)
			}
		}
		if bytes.Contains(sent, []byte("$6$s")) || bytes.Contains(sent, []byte("saltsalt")) || bytes.Contains(sent, []byte("HASHSECRET")) {
			t.Fatalf("tar-first split=%d: leaked hash bytes %q", split, sent[512:])
		}
		if idx := bytes.Index(sent, []byte("$6$")); idx >= 0 && idx != len(sent)-3 {
			t.Fatalf("tar-first split=%d: leaked bytes past $6$: %q", split, sent[idx:])
		}
	}
	// Also a tar crossing the real 128 KiB Download FrameBytes boundary (with
	// and without a trailing newline on the shadow member).
	tarPath := filepath.Join(site, "backup.tar")
	// Pad member header (512) + padded data + shadow header (512) puts shadow
	// data at 512 + padBlocks*512 + 512. Using padSize so shadow's start lands
	// inside the last 512 bytes before FrameBytes; then test a tar with shadow
	// starting at FrameBytes - offset via a leading comment/pad inside shadow.
	padMemberBody := strings.Repeat("p", FrameBytes-1024-512)
	for _, trailingNL := range []bool{true, false} {
		entry := hashEntry
		if !trailingNL {
			entry = strings.TrimSuffix(entry, "\n")
		}
		for offset := 1; offset <= len(entry); offset++ {
			// Member 1: 512 header + 126.5 KiB body -> ends at FrameBytes - 1024.
			// Member 2 ("shadow"): 512 header (ends at FrameBytes - 512), then
			// (512 - offset) bytes of prefix lines so hashEntry starts at FrameBytes - offset.
			var shadowPrefix string
			if 512-offset > 0 {
				shadowPrefix = strings.Repeat("x", 512-offset-1) + "\n"
			}
			rawTar := tarOf(t, []struct{ name, body string }{
				{name: "pad.dat", body: padMemberBody},
				{name: "shadow", body: shadowPrefix + entry},
			})
			if err := os.WriteFile(tarPath, rawTar, 0o600); err != nil {
				t.Fatal(err)
			}
			_, data, out := d.download(t, tarPath)
			assertNoHashLeaked(t, fmt.Sprintf("tar nl=%v offset=%d", trailingNL, offset), data, out)
		}
	}
}

// N1: Short wrapped base64 lines stop on the 16th base64 character even when
// the part ends right before the line break (at most 15 chars can leave).
func TestShortWrappedBase64StopsAt16WithoutNewline(t *testing.T) {
	part := []byte("-----BEGIN PRIVATE KEY-----\nMIIEvgIB\nADANBgkq")
	var scan keyScan
	if r := scan.check("/k.pem", part); r == nil || r.GetKind() != agentv1.FileRefusal_KIND_REFUSED {
		t.Fatalf("short wrapped 8+8 without trailing newline = %v, want KIND_REFUSED", r)
	}
}

// L2: text files up to 128 KiB scan without seeking on a non-seekable mount,
// binary files > 128 KiB download without seeking, and text files > 128 KiB
// get a refusal explaining that the mount can't seek (never blaming permissions),
// while folder/selection/compress zips leave them out cleanly.
func TestNonSeekableMountHandling(t *testing.T) {
	d := newDisk(t)
	d.noAccounts()
	site := filepath.Join(d.root, "site")
	if err := os.MkdirAll(site, 0o755); err != nil {
		t.Fatal(err)
	}

	smallText := strings.Repeat("a", FrameBytes-10) + "\n"
	exactFrameText := strings.Repeat("b", FrameBytes)
	bigText := strings.Repeat("line of text\n", 15_000) // ~195 KiB (> 128 KiB)
	bigBin := append([]byte{0x00, 0x01, 0x02}, bytes.Repeat([]byte("z"), 200<<10)...)

	write(t, filepath.Join(site, "small.txt"), smallText)
	write(t, filepath.Join(site, "exact.txt"), exactFrameText)
	write(t, filepath.Join(site, "big.txt"), bigText)
	if err := os.WriteFile(filepath.Join(site, "big.bin"), bigBin, 0o600); err != nil {
		t.Fatal(err)
	}

	var seekCalls atomic.Int32
	origSeek := seekFile
	seekFile = func(f *os.File) error {
		seekCalls.Add(1)
		return &os.PathError{Op: "seek", Path: f.Name(), Err: unix.ESPIPE}
	}
	t.Cleanup(func() { seekFile = origSeek })

	// 1. <= 128 KiB and exact 128 KiB text files succeed without calling seekFile.
	for name, want := range map[string]string{"small.txt": smallText, "exact.txt": exactFrameText} {
		_, data, out := d.download(t, filepath.Join(site, name))
		if out.GetRefusal() != nil || string(data) != want {
			t.Errorf("download %s on non-seekable mount: refusal=%v len=%d want=%d", name, out.GetRefusal(), len(data), len(want))
		}
	}
	// 2. Binary > 128 KiB succeeds on single download AND single-file selection
	// zip without seeking (L1 + M2).
	bigBinPath := filepath.Join(site, "big.bin")
	if _, data, out := d.download(t, bigBinPath); out.GetRefusal() != nil || !bytes.Equal(data, bigBin) {
		t.Errorf("download big.bin on non-seekable mount: refusal=%v len=%d", out.GetRefusal(), len(data))
	}
	var selBinZip bytes.Buffer
	selBinOut, err := d.server.DownloadMany(t.Context(), []string{bigBinPath}, func(m *agentv1.SendFileRequest) error {
		selBinZip.Write(m.GetData())
		return nil
	})
	if err != nil || selBinOut.GetRefusal() != nil {
		t.Fatalf("DownloadMany single big.bin on non-seekable mount = %v, %v", selBinOut, err)
	}
	if got := zipNames(t, selBinZip.Bytes()); got["big.bin"] != string(bigBin) {
		t.Errorf("DownloadMany single big.bin content mismatch: len=%d", len(got["big.bin"]))
	}
	if seekCalls.Load() != 0 {
		t.Errorf("seekFile called %d times for files that need no seek, want 0", seekCalls.Load())
	}

	// 3. Text > 128 KiB on single download fails cleanly before sending any frame,
	// with KIND_REFUSED, explaining that the mount can't seek (not blaming
	// permissions, and not repeating the path twice: N3).
	bigTxtPath := filepath.Join(site, "big.txt")
	sent, _, out := d.download(t, bigTxtPath)
	if out.GetRefusal() == nil || len(sent) != 0 {
		t.Fatalf("download big.txt on non-seekable mount = refusal=%v sent=%d, want refusal and 0 frames", out.GetRefusal(), len(sent))
	}
	if out.GetRefusal().GetKind() != agentv1.FileRefusal_KIND_REFUSED {
		t.Errorf("seek refusal kind = %v, want KIND_REFUSED", out.GetRefusal().GetKind())
	}
	rawMsg := out.GetRefusal().GetMessage()
	msg := strings.ToLower(rawMsg)
	if !strings.Contains(msg, "seek") {
		t.Errorf("seek refusal message = %q, want explanation about seeking", rawMsg)
	}
	if strings.Contains(msg, "permission") {
		t.Errorf("seek refusal message = %q, must not blame permissions", rawMsg)
	}
	if strings.Count(rawMsg, bigTxtPath) != 1 {
		t.Errorf("seek refusal message = %q, want path once, got %d", rawMsg, strings.Count(rawMsg, bigTxtPath))
	}

	// 4. Folder zip keeps small.txt, exact.txt, AND big.bin (L1: binaries need
	// no seek), leaving out only big.txt.
	_, zipData, zipOut := d.download(t, site)
	if zipOut.GetRefusal() != nil {
		t.Fatalf("folder zip on non-seekable mount refused: %v", zipOut.GetRefusal())
	}
	if zipOut.GetLeftOut() != 1 || !slices.Equal(zipOut.GetLeftOutPaths(), []string{"big.txt"}) {
		t.Errorf("folder zip left_out = %d (%v), want 1 ([big.txt])", zipOut.GetLeftOut(), zipOut.GetLeftOutPaths())
	}
	got := zipNames(t, zipData)
	if got["small.txt"] != smallText || got["exact.txt"] != exactFrameText || got["big.bin"] != string(bigBin) {
		t.Errorf("folder zip missing non-seekable files: keys=%d", len(got))
	}
	if _, ok := got["big.txt"]; ok {
		t.Error("folder zip unexpectedly included non-seekable big.txt")
	}
}

// M2: Verify files <= 128 KiB and binaries > 128 KiB need zero seeks in zips
// and single-file selections, and a text file > 128 KiB in a single-file
// selection (DownloadMany or Compress) rewinds only once (never three passes).
func TestZipPassCountsAndPreflightHandoff(t *testing.T) {
	d := newDisk(t)
	d.noAccounts()
	site := filepath.Join(d.root, "site")
	if err := os.MkdirAll(site, 0o755); err != nil {
		t.Fatal(err)
	}

	smallTxt := filepath.Join(site, "small.txt")
	bigTxt := filepath.Join(site, "big.txt")
	bigBin := filepath.Join(site, "big.bin")
	write(t, smallTxt, strings.Repeat("hello world\n", 100))
	write(t, bigTxt, strings.Repeat("config=ok\n", 20_000)) // ~200 KiB text
	if err := os.WriteFile(bigBin, append([]byte{0x00, 0x01, 0x02}, bytes.Repeat([]byte("z"), 300<<10)...), 0o600); err != nil {
		t.Fatal(err)
	}

	var seeks atomic.Int32
	origSeek := seekFile
	seekFile = func(f *os.File) error {
		seeks.Add(1)
		return origSeek(f)
	}
	t.Cleanup(func() { seekFile = origSeek })

	// 1. Single-file DownloadMany & Compress of small.txt and big.bin: 0 seeks.
	for i, p := range []string{smallTxt, bigBin} {
		seeks.Store(0)
		var buf bytes.Buffer
		out, err := d.server.DownloadMany(t.Context(), []string{p}, func(m *agentv1.SendFileRequest) error {
			buf.Write(m.GetData())
			return nil
		})
		if err != nil || out.GetRefusal() != nil {
			t.Fatalf("DownloadMany(%s) = %v, %v", p, out, err)
		}
		if seeks.Load() != 0 {
			t.Errorf("DownloadMany(%s) called seekFile %d times, want 0", p, seeks.Load())
		}
		seeks.Store(0)
		arc := filepath.Join(d.root, fmt.Sprintf("single-%d.zip", i))
		comp, err := d.server.Compress(t.Context(), []string{p}, arc, "", testArchiveOwner)
		if err != nil || comp.GetRefusal() != nil {
			t.Fatalf("Compress(%s) = %v, %v", p, comp, err)
		}
		if done := waitJob(t, d.server, comp.GetJob().GetJobId()); done.GetState() != dockwellv1.ArchiveJobState_ARCHIVE_JOB_STATE_DONE {
			t.Fatalf("Compress(%s) job = %v", p, done)
		}
		if seeks.Load() != 0 {
			t.Errorf("Compress(%s) called seekFile %d times, want 0", p, seeks.Load())
		}
	}

	// 2. Single-file DownloadMany & Compress of big.txt (> 128 KiB text):
	// exactly 1 seek (preflight scans first MiB + rewinds once, hands open file
	// directly to archiver; never 2 seeks / 3 passes).
	seeks.Store(0)
	var txtZip bytes.Buffer
	out, err := d.server.DownloadMany(t.Context(), []string{bigTxt}, func(m *agentv1.SendFileRequest) error {
		txtZip.Write(m.GetData())
		return nil
	})
	if err != nil || out.GetRefusal() != nil {
		t.Fatalf("DownloadMany(big.txt) = %v, %v", out, err)
	}
	if seeks.Load() != 1 {
		t.Errorf("DownloadMany(big.txt) called seekFile %d times, want 1", seeks.Load())
	}
	if got := zipNames(t, txtZip.Bytes()); got["big.txt"] != strings.Repeat("config=ok\n", 20_000) {
		t.Errorf("DownloadMany(big.txt) content len = %d", len(got["big.txt"]))
	}

	seeks.Store(0)
	txtArc := filepath.Join(d.root, "single-big-txt.zip")
	comp, err := d.server.Compress(t.Context(), []string{bigTxt}, txtArc, "", testArchiveOwner)
	if err != nil || comp.GetRefusal() != nil {
		t.Fatalf("Compress(big.txt) = %v, %v", comp, err)
	}
	if done := waitJob(t, d.server, comp.GetJob().GetJobId()); done.GetState() != dockwellv1.ArchiveJobState_ARCHIVE_JOB_STATE_DONE {
		t.Fatalf("Compress(big.txt) job = %v", done)
	}
	if seeks.Load() != 1 {
		t.Errorf("Compress(big.txt) called seekFile %d times, want 1", seeks.Load())
	}
}

// L3: Deep left-out paths are capped to maxLeftOutPathBytes with a middle
// ellipsis on valid UTF-8 boundaries, keeping zip comments compact and valid.
func TestLeftOutPathsCappedAndValidUTF8(t *testing.T) {
	d := newDisk(t)
	d.noAccounts()
	site := filepath.Join(d.root, "site")
	deepDir := site
	for i := range 10 {
		deepDir = filepath.Join(deepDir, fmt.Sprintf("folder-%02d-%s", i, strings.Repeat("中", 12)))
	}
	if err := os.MkdirAll(deepDir, 0o755); err != nil {
		t.Fatal(err)
	}
	write(t, filepath.Join(site, "ok.txt"), "ok\n")
	keyBody := "-----BEGIN PRIVATE KEY-----\nMIIEvgIBADANBgkqhkiG9w0BAQEFAASCBK\n-----END PRIVATE KEY-----\n"
	for i := range 12 {
		write(t, filepath.Join(deepDir, fmt.Sprintf("secret-%02d.pem", i)), keyBody)
	}

	_, zipData, out := d.download(t, site)
	if out.GetRefusal() != nil {
		t.Fatalf("folder zip refused: %v", out.GetRefusal())
	}
	if out.GetLeftOut() != 12 {
		t.Fatalf("left_out = %d, want 12", out.GetLeftOut())
	}
	if len(out.GetLeftOutPaths()) != maxLeftOutPaths {
		t.Fatalf("len(left_out_paths) = %d, want %d", len(out.GetLeftOutPaths()), maxLeftOutPaths)
	}
	for _, p := range out.GetLeftOutPaths() {
		if len(p) > maxLeftOutPathBytes {
			t.Errorf("left-out path len = %d > %d: %q", len(p), maxLeftOutPathBytes, p)
		}
		if !utf8.ValidString(p) {
			t.Errorf("left-out path is invalid UTF-8: %q", p)
		}
		if !strings.Contains(p, "…") {
			t.Errorf("long left-out path missing middle ellipsis: %q", p)
		}
	}
	zr, err := zip.NewReader(bytes.NewReader(zipData), int64(len(zipData)))
	if err != nil {
		t.Fatalf("zip.NewReader: %v", err)
	}
	if !utf8.ValidString(zr.Comment) || len(zr.Comment) > 2500 {
		t.Errorf("zip comment len=%d valid=%v", len(zr.Comment), utf8.ValidString(zr.Comment))
	}
}

// L3 + M2: A folder zip, selection zip, and archive compress job leave out
// files whose start trips the key check (.env, a tar/binary with a key in its
// first 128 KiB, or a text file with a key in its first MiB) and list them; a
// key past 128 KiB in a binary stops the archive before any secret enters.
func TestZipsLeaveOutKeyFilesAndListThem(t *testing.T) {
	d := newDisk(t)
	d.noAccounts()
	site := filepath.Join(d.root, "site")
	if err := os.MkdirAll(site, 0o755); err != nil {
		t.Fatal(err)
	}

	write(t, filepath.Join(site, "readme.txt"), "safe content\n")
	write(t, filepath.Join(site, ".env"), "APP=1\nTLS_KEY=\"-----BEGIN RSA PRIVATE KEY-----\nMIIEowIBAAKCAQEAwJh2EnvSecretKey\n-----END RSA PRIVATE KEY-----\"\n")

	// A .tar with a key in its first 128 KiB (at ~10 KiB) plus trailing data > 128 KiB.
	headTar := tarOf(t, []struct{ name, body string }{
		{name: "id_rsa", body: "-----BEGIN RSA PRIVATE KEY-----\nMIIEowIBAAKCAQEAwJh2TarHeadSecret\n-----END RSA PRIVATE KEY-----\n"},
		{name: "pad.txt", body: strings.Repeat("p", 200<<10)},
	})
	if err := os.WriteFile(filepath.Join(site, "backup.tar"), headTar, 0o600); err != nil {
		t.Fatal(err)
	}

	// A text file > 128 KiB with a key at ~200 KiB (within the first MiB).
	write(t, filepath.Join(site, "config.txt"), strings.Repeat("ok=1\n", 40_000)+"-----BEGIN RSA PRIVATE KEY-----\nMIIEowIBAAKCAQEAwJh2BigTextSecret\n-----END RSA PRIVATE KEY-----\n")

	// Single downloads of each offending file STILL stop and leak no key bytes.
	for name, secret := range map[string]string{
		".env":       "EnvSecretKey",
		"backup.tar": "TarHeadSecret",
		"config.txt": "BigTextSecret",
	} {
		_, data, out := d.download(t, filepath.Join(site, name))
		if out.GetRefusal().GetKind() != agentv1.FileRefusal_KIND_REFUSED {
			t.Errorf("single download %s: refusal=%v, want KIND_REFUSED", name, out.GetRefusal())
		}
		if bytes.Contains(data, []byte(secret)) {
			t.Errorf("single download %s leaked %q", name, secret)
		}
	}

	wantLeftOut := []string{".env", "backup.tar", "config.txt"}
	checkZip := func(t *testing.T, label string, raw []byte, leftOut uint64, leftOutPaths []string) {
		t.Helper()
		if leftOut != 3 {
			t.Errorf("%s left_out = %d, want 3", label, leftOut)
		}
		sorted := slices.Clone(leftOutPaths)
		slices.Sort(sorted)
		if !slices.Equal(sorted, wantLeftOut) {
			t.Errorf("%s left_out_paths = %v, want %v", label, sorted, wantLeftOut)
		}
		zr, err := zip.NewReader(bytes.NewReader(raw), int64(len(raw)))
		if err != nil {
			t.Fatalf("%s zip.NewReader: %v", label, err)
		}
		for _, excluded := range wantLeftOut {
			if !strings.Contains(zr.Comment, excluded) {
				t.Errorf("%s zip comment %q does not list %s", label, zr.Comment, excluded)
			}
		}
		got := zipNames(t, raw)
		if got["readme.txt"] != "safe content\n" {
			t.Errorf("%s missing readme.txt: %v", label, got)
		}
		for _, excluded := range wantLeftOut {
			if _, ok := got[excluded]; ok {
				t.Errorf("%s leaked %s into archive", label, excluded)
			}
		}
	}

	// 1. Folder zip
	_, folderZip, folderOut := d.download(t, site)
	if folderOut.GetRefusal() != nil {
		t.Fatalf("folder zip refused: %v", folderOut.GetRefusal())
	}
	checkZip(t, "folder zip", folderZip, folderOut.GetLeftOut(), folderOut.GetLeftOutPaths())

	// 2. Selection zip (DownloadMany) with all four top-level files
	paths := []string{
		filepath.Join(site, "readme.txt"),
		filepath.Join(site, ".env"),
		filepath.Join(site, "backup.tar"),
		filepath.Join(site, "config.txt"),
	}
	var selBuf bytes.Buffer
	selOut, err := d.server.DownloadMany(t.Context(), paths, func(m *agentv1.SendFileRequest) error {
		selBuf.Write(m.GetData())
		return nil
	})
	if err != nil || selOut.GetRefusal() != nil {
		t.Fatalf("DownloadMany = %v, %v", selOut, err)
	}
	checkZip(t, "selection zip", selBuf.Bytes(), selOut.GetLeftOut(), selOut.GetLeftOutPaths())

	// 3. Archive compress job with all four top-level files
	arcPath := filepath.Join(d.root, "bundle.zip")
	comp, err := d.server.Compress(t.Context(), paths, arcPath, "", testArchiveOwner)
	if err != nil || comp.GetRefusal() != nil {
		t.Fatalf("Compress = %v, %v", comp, err)
	}
	done := waitJob(t, d.server, comp.GetJob().GetJobId())
	if done.GetState() != dockwellv1.ArchiveJobState_ARCHIVE_JOB_STATE_DONE {
		t.Fatalf("Compress job = %v", done)
	}
	rawArc, err := os.ReadFile(arcPath)
	if err != nil {
		t.Fatal(err)
	}
	checkZip(t, "compress job", rawArc, done.GetLeftOut(), done.GetLeftOutPaths())

	// 4. Single-file selection of config.txt (key at 200 KiB in text) or
	// backup.tar (key in first 128 KiB) is refused cleanly BEFORE sending.
	for _, p := range []string{filepath.Join(site, "config.txt"), filepath.Join(site, "backup.tar")} {
		sentAny := false
		singleSel, err := d.server.DownloadMany(t.Context(), []string{p}, func(*agentv1.SendFileRequest) error {
			sentAny = true
			return nil
		})
		if err != nil || singleSel.GetRefusal().GetKind() != agentv1.FileRefusal_KIND_REFUSED || sentAny {
			t.Errorf("DownloadMany single %s = %v, %v, sent=%v, want clean KIND_REFUSED", p, singleSel.GetRefusal(), err, sentAny)
		}
	}

	// 5. A binary with a key past 128 KiB (bounded prescan does not double-read
	// large binaries) stops the archive before any secret byte enters.
	deepDir := filepath.Join(d.root, "deep")
	if err := os.MkdirAll(deepDir, 0o755); err != nil {
		t.Fatal(err)
	}
	deepTar := tarOf(t, []struct{ name, body string }{
		{name: "pad.txt", body: strings.Repeat("p", 200<<10)},
		{name: "id_rsa", body: "-----BEGIN RSA PRIVATE KEY-----\nMIIEowIBAAKCAQEAwJh2TarDeepSecret\n-----END RSA PRIVATE KEY-----\n"},
	})
	if err := os.WriteFile(filepath.Join(deepDir, "deep.tar"), deepTar, 0o600); err != nil {
		t.Fatal(err)
	}
	_, deepZipData, deepZipOut := d.download(t, deepDir)
	if deepZipOut.GetRefusal().GetKind() != agentv1.FileRefusal_KIND_REFUSED {
		t.Errorf("folder zip with deep binary key = %v, want KIND_REFUSED", deepZipOut.GetRefusal())
	}
	if bytes.Contains(deepZipData, []byte("TarDeepSecret")) {
		t.Error("folder zip with deep binary key leaked secret")
	}
}

// Files that change between pass 1 (prescan/headScan) and pass 2 (stream) are
// still caught before sensitive bytes go out.
func TestFileChangedBetweenPrescanAndStreamIsCaught(t *testing.T) {
	d := newDisk(t)
	d.noAccounts()
	site := filepath.Join(d.root, "site")
	if err := os.MkdirAll(site, 0o755); err != nil {
		t.Fatal(err)
	}

	clean := strings.Repeat("clean text line\n", 15_000) // ~240 KiB
	p := filepath.Join(site, "mutable.txt")
	write(t, p, clean)

	// Case A: Single download — file is overwritten at seek time to hold Docker state.
	origSeek := seekFile
	swapped := false
	seekFile = func(f *os.File) error {
		if !swapped {
			swapped = true
			payload := strings.Repeat("a", 180<<10) + `{"MountOpts":{"o":"pass=toctou"}}` + "\n"
			if err := os.WriteFile(p, []byte(payload), 0o600); err != nil {
				return err
			}
		}
		return origSeek(f)
	}
	t.Cleanup(func() { seekFile = origSeek })

	_, data, out := d.download(t, p)
	if out.GetRefusal().GetKind() != agentv1.FileRefusal_KIND_REFUSED {
		t.Fatalf("single download TOCTOU Docker state: refusal=%v, want KIND_REFUSED", out.GetRefusal())
	}
	if bytes.Contains(data, []byte("pass=toctou")) {
		t.Error("single download TOCTOU leaked Docker MountOpts")
	}

	// Case B: Folder zip — file is overwritten at seek time with a key in the first 128 KiB:
	// pass 2 catches it before CreateHeader and leaves it out cleanly.
	write(t, p, clean)
	write(t, filepath.Join(site, "safe.txt"), "ok\n")
	swapped = false
	seekFile = func(f *os.File) error {
		if !swapped && strings.HasSuffix(f.Name(), "mutable.txt") {
			swapped = true
			payload := "-----BEGIN PRIVATE KEY-----\nMIIEvgIBADANBgkqhkiG9w0TOCTOUSecret\n-----END PRIVATE KEY-----\n" + strings.Repeat("x", 150<<10)
			if err := os.WriteFile(p, []byte(payload), 0o600); err != nil {
				return err
			}
		}
		return origSeek(f)
	}

	_, zipData, zipOut := d.download(t, site)
	if zipOut.GetRefusal() != nil {
		t.Fatalf("folder zip TOCTOU first-frame refused: %v", zipOut.GetRefusal())
	}
	if zipOut.GetLeftOut() != 1 || !slices.Contains(zipOut.GetLeftOutPaths(), "mutable.txt") {
		t.Errorf("folder zip TOCTOU left_out = %d (%v), want 1 with mutable.txt", zipOut.GetLeftOut(), zipOut.GetLeftOutPaths())
	}
	if bytes.Contains(zipData, []byte("TOCTOUSecret")) {
		t.Error("folder zip TOCTOU leaked key bytes")
	}

	// Case C: Folder zip — file is overwritten at seek time with a key past 128 KiB:
	// pass 2 stops before the key bytes enter the zip.
	write(t, p, clean)
	swapped = false
	seekFile = func(f *os.File) error {
		if !swapped && strings.HasSuffix(f.Name(), "mutable.txt") {
			swapped = true
			payload := strings.Repeat("x", 180<<10) + "\n-----BEGIN PRIVATE KEY-----\nMIIEvgIBADANBgkqhkiG9w0DeepTOCTOUSecret\n-----END PRIVATE KEY-----\n"
			if err := os.WriteFile(p, []byte(payload), 0o600); err != nil {
				return err
			}
		}
		return origSeek(f)
	}

	_, deepZipData, deepZipOut := d.download(t, site)
	if deepZipOut.GetRefusal().GetKind() != agentv1.FileRefusal_KIND_REFUSED {
		t.Fatalf("folder zip deep TOCTOU: refusal=%v, want KIND_REFUSED", deepZipOut.GetRefusal())
	}
	if bytes.Contains(deepZipData, []byte("DeepTOCTOUSecret")) {
		t.Error("folder zip deep TOCTOU leaked key bytes")
	}
}
