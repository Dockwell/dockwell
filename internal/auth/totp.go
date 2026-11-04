// SPDX-License-Identifier: AGPL-3.0-only

package auth

import (
	"crypto/hmac"
	"crypto/rand"
	"crypto/sha1" //nolint:gosec // RFC 6238 specifies HMAC-SHA1, and every authenticator app uses it
	"crypto/sha256"
	"crypto/subtle"
	"encoding/base32"
	"encoding/binary"
	"fmt"
	"net/url"
	"strings"
	"time"
)

// TOTP parameters, per RFC 6238 and what every authenticator app expects.
const (
	totpPeriod     = 30 // seconds per step
	totpSkew       = 1  // steps accepted either side of now
	totpSecretSize = 20 // bytes, the HMAC-SHA1 block's natural key size
	totpModulus    = 1_000_000
)

// Recovery codes: ten per user, 80 bits each, shown as XXXX-XXXX-XXXX-XXXX in
// Crockford's base32.
const (
	recoveryCodeCount = 10
	recoveryCodeBytes = 10
	recoveryCodeLen   = 16
)

var totpEncoding = base32.StdEncoding.WithPadding(base32.NoPadding)

// crockford is Crockford's base32 alphabet, as in internal/id: no I, L, O or U.
const crockford = "0123456789ABCDEFGHJKMNPQRSTVWXYZ"

func totpStep(t time.Time) int64 { return t.Unix() / totpPeriod }

// totpCode is the 6-digit code for a time step (RFC 4226 section 5.3).
func totpCode(secret []byte, step int64) string {
	var msg [8]byte
	binary.BigEndian.PutUint64(msg[:], uint64(step)) //nolint:gosec // steps are positive
	mac := hmac.New(sha1.New, secret)
	mac.Write(msg[:])
	sum := mac.Sum(nil)
	off := sum[len(sum)-1] & 0x0f
	bin := binary.BigEndian.Uint32(sum[off:off+4]) & 0x7fffffff
	return fmt.Sprintf("%06d", bin%totpModulus)
}

// matchTOTP checks code against the steps around now and returns the step it
// matches. Every step is compared, in constant time, so timing reveals
// nothing about which one matched.
func matchTOTP(secret []byte, code string, now time.Time) (int64, bool) {
	step := totpStep(now)
	var matched int64 = -1
	for d := -totpSkew; d <= totpSkew; d++ {
		s := step + int64(d)
		if subtle.ConstantTimeCompare([]byte(totpCode(secret, s)), []byte(code)) == 1 {
			matched = s
		}
	}
	return matched, matched >= 0
}

func newTOTPSecret() []byte {
	b := make([]byte, totpSecretSize)
	_, _ = rand.Read(b)
	return b
}

// totpURI is the otpauth:// URI authenticator apps read from a QR code.
func totpURI(secret []byte, email, serverName string) string {
	account := email
	if serverName != "" {
		account += " (" + serverName + ")"
	}
	q := url.Values{}
	q.Set("secret", totpEncoding.EncodeToString(secret))
	q.Set("issuer", "Dockwell")
	q.Set("algorithm", "SHA1")
	q.Set("digits", "6")
	q.Set("period", "30")
	return "otpauth://totp/" + url.PathEscape("Dockwell:"+account) + "?" + q.Encode()
}

// codeKind says what a typed code is.
type codeKind int

const (
	codeInvalid codeKind = iota
	codeTOTP
	codeRecovery
)

// normalizeCode tidies what a person typed: spaces and dashes go, letters go
// up to capitals, and the letters Crockford's base32 reads as digits (I, L
// and O) become them.
func normalizeCode(s string) (string, codeKind) {
	var b strings.Builder
	for _, r := range strings.ToUpper(s) {
		switch r {
		case ' ', '-', '\t':
		case 'I', 'L':
			b.WriteByte('1')
		case 'O':
			b.WriteByte('0')
		default:
			b.WriteRune(r)
		}
	}
	code := b.String()
	switch {
	case len(code) == 6 && strings.Trim(code, "0123456789") == "":
		return code, codeTOTP
	case len(code) == recoveryCodeLen && strings.Trim(code, crockford) == "":
		return code, codeRecovery
	default:
		return "", codeInvalid
	}
}

// newRecoveryCodes returns ten codes to show once, and their hashes to keep.
func newRecoveryCodes() (codes []string, hashes [][]byte) {
	for range recoveryCodeCount {
		raw := make([]byte, recoveryCodeBytes)
		_, _ = rand.Read(raw)
		code := encodeCrockford(raw)
		codes = append(codes, code[0:4]+"-"+code[4:8]+"-"+code[8:12]+"-"+code[12:16])
		hashes = append(hashes, hashRecoveryCode(code))
	}
	return codes, hashes
}

// hashRecoveryCode hashes a normalized code. Codes carry 80 random bits, so
// a plain SHA-256 can't be reversed by search.
func hashRecoveryCode(normalized string) []byte {
	sum := sha256.Sum256([]byte("dockwell recovery code\x00" + normalized))
	return sum[:]
}

// encodeCrockford writes 10 bytes as 16 base32 characters.
func encodeCrockford(b []byte) string {
	var out strings.Builder
	var buf uint64
	bits := 0
	for _, c := range b {
		buf = buf<<8 | uint64(c)
		bits += 8
		for bits >= 5 {
			bits -= 5
			out.WriteByte(crockford[(buf>>bits)&31])
		}
	}
	if bits > 0 {
		out.WriteByte(crockford[(buf<<(5-bits))&31])
	}
	return out.String()
}
