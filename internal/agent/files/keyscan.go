// SPDX-License-Identifier: AGPL-3.0-only

package files

import (
	"bytes"
	"encoding/base64"
	"unicode/utf8"

	agentv1 "github.com/dockwell/dockwell/gen/dockwell/agent/v1"
)

const (
	// beforeKey is what can come before a key on its line: an indent, as in YAML,
	// and the byte order mark at the start of a file.
	beforeKey = " \t\ufeff"
	// keyTail is how much of what went before a part is kept, to find a key that
	// starts across two parts, including keys wrapped onto short base64 lines.
	keyTail = 512
	// lineHead is how much of a line's start keyScan keeps, which is all the line
	// rules look at.
	lineHead = 512
	// puttyWindow is how far past a PuTTY-User-Key-File header a Private-Lines
	// header can sit (covers large RSA public keys and PPK v3 KDF parameters).
	puttyWindow = 16 << 10
)

var (
	beginPEMMark    = []byte("-----BEGIN ")
	endPEMMark      = []byte("-----END ")
	beginSSH2Mark   = []byte("---- BEGIN SSH2 ")
	endSSH2Mark     = []byte("---- END SSH2 ")
	puttyHeaderMark = []byte("PuTTY-User-Key-File-")
	puttyPrivMark   = []byte("Private-Lines:")
	fiveDashes      = []byte("-----")
	// Base64 encodings of "-----BEGIN " at byte offsets 0, 1, and 2 mod 3, as in
	// kubeconfig client-key-data, k3s.yaml, and Kubernetes Secret manifests.
	b64PEMMark0 = []byte("LS0tLS1CRUdJTi")
	b64PEMMark1 = []byte("0tLS0tQkVHSU4g")
	b64PEMMark2 = []byte("tLS0tLUJFR0lOI")
)

// keyScan finds where a private key or a password hash starts in what's sent,
// across parts, with no per-part heap allocations. It catches:
//   - PEM private keys and PGP private key blocks (standalone on a text line,
//     quoted in .env or JSON with \n / \\n escapes and JSON-escaped \/ or \+,
//     base64-wrapped as in kubeconfig client-key-data, or mid-line in binary/tar
//     data followed by encryption/armor headers or base64, even when wrapped
//     onto short lines),
//   - SSH2 "SECSH" private keys (---- BEGIN SSH2 ENCRYPTED PRIVATE KEY ----),
//   - PuTTY .ppk files (PuTTY-User-Key-File-N: followed by Private-Lines: N),
//   - lines of a password file (shadow / .htpasswd), including right after a
//     tar header's NUL padding when the line reads as text.
type keyScan struct {
	tail        [keyTail]byte
	tailLen     int
	look        [keyTail * 2]byte
	line        [lineHead]byte
	lineLen     int
	lineOver    bool
	afterNul    nulLine
	puttyRemain int
}

// reset clears k so a caller (such as archiver) can reuse one keyScan across files.
func (k *keyScan) reset() {
	clear(k.tail[:k.tailLen])
	clear(k.line[:k.lineLen])
	clear(k.afterNul.buf[:k.afterNul.n])
	k.tailLen = 0
	k.lineLen = 0
	k.lineOver = false
	k.afterNul.n = 0
	k.afterNul.after = false
	k.puttyRemain = 0
}

// holdsPrivateKey reports whether text holds any refused private key form,
// without allocating a keyScan on the heap.
func holdsPrivateKey(text []byte) bool {
	if len(text) == 0 {
		return false
	}
	if matchKeyBlock(text, true) {
		return true
	}
	if matchPuTTYInBuf(text, true) {
		return true
	}
	if !bytes.Contains(text, beginPEMMark) && !bytes.Contains(text, beginSSH2Mark) {
		return false
	}
	for rest := text; len(rest) > 0; {
		seg, more, _ := bytes.Cut(rest, []byte{'\n'})
		rest = more
		if holdsStandalonePrivateKeyLine(seg) {
			return true
		}
	}
	return false
}

// check scans part and returns a download-stopped refusal if a private key or
// password hash is found.
func (k *keyScan) check(where string, part []byte) *agentv1.FileRefusal {
	if why := k.scan(part, true); why != "" {
		return keyStopRefusal(where, why)
	}
	return nil
}

// finish flushes any pending end-of-file line or key state after the final part.
func (k *keyScan) finish(where string) *agentv1.FileRefusal {
	if why := k.finishWhy(true); why != "" {
		return keyStopRefusal(where, why)
	}
	return nil
}

// checkAll scans a complete buffer (including EOF handling) in one call.
func (k *keyScan) checkAll(where string, data []byte) *agentv1.FileRefusal {
	if r := k.check(where, data); r != nil {
		return r
	}
	return k.finish(where)
}

func keyStopRefusal(where, why string) *agentv1.FileRefusal {
	return &agentv1.FileRefusal{
		Kind: agentv1.FileRefusal_KIND_REFUSED, Path: where,
		Message: where + " holds " + why + ", so the download stopped before it, and is cut short. Connect to the server over SSH to work with it",
	}
}

func (k *keyScan) scan(part []byte, checkHashes bool) string {
	if len(part) == 0 {
		return ""
	}
	if k.tailLen > 0 {
		n := copy(k.look[:], k.tail[:k.tailLen])
		n += copy(k.look[n:], part[:min(len(part), keyTail)])
		join := k.look[:n]
		if matchKeyBlock(join, false) {
			return whyPrivateKey
		}
		if k.puttyRemain > 0 && hasPuTTYPrivateLines(join, false) {
			return whyPrivateKey
		}
		if k.scanPuTTYHeaders(join, false) {
			return whyPrivateKey
		}
	}
	if matchKeyBlock(part, false) {
		return whyPrivateKey
	}
	if k.puttyRemain > 0 {
		if hasPuTTYPrivateLines(part[:min(len(part), k.puttyRemain)], false) {
			return whyPrivateKey
		}
		k.puttyRemain = max(0, k.puttyRemain-len(part))
	}
	if k.scanPuTTYHeaders(part, false) {
		return whyPrivateKey
	}
	k.keepTail(part)
	for rest := part; len(rest) > 0; {
		seg, more, ended := bytes.Cut(rest, []byte{'\n'})
		rest = more
		if checkHashes && k.afterNul.feed(seg, ended) {
			return whyHashes
		}
		if checkHashes {
			if nul := bytes.IndexByte(seg, 0); nul >= 0 {
				// A password file in a tar without a trailing newline ends at the
				// tar block's NUL padding; check the text before the first NUL.
				if room := lineHead - k.lineLen; room > 0 && nul > 0 {
					take := min(nul, room)
					copy(k.line[k.lineLen:], seg[:take])
					k.lineLen += take
				}
				if k.lineLen > 0 && holdsHashLine(k.line[:k.lineLen], false) {
					return whyHashes
				}
				seg = seg[nul:]
			}
		}
		if room := lineHead - k.lineLen; room > 0 {
			take := min(len(seg), room)
			copy(k.line[k.lineLen:], seg[:take])
			k.lineLen += take
			seg = seg[take:]
		}
		if len(seg) > 0 && !k.lineOver {
			if len(bytes.TrimRight(seg, " \t\r")) > 0 {
				k.lineOver = true
			}
		}
		if ended {
			if why := k.finishLine(checkHashes); why != "" {
				return why
			}
		}
	}
	if checkHashes && k.lineLen > 0 && holdsHashLine(k.line[:k.lineLen], true) {
		return whyHashes
	}
	return ""
}

func (k *keyScan) finishWhy(checkHashes bool) string {
	if k.tailLen > 0 {
		tail := k.tail[:k.tailLen]
		if matchKeyBlock(tail, true) {
			return whyPrivateKey
		}
		if k.puttyRemain > 0 && hasPuTTYPrivateLines(tail, true) {
			return whyPrivateKey
		}
	}
	if checkHashes && k.afterNul.finish() {
		return whyHashes
	}
	return k.finishLine(checkHashes)
}

func (k *keyScan) finishLine(checkHashes bool) string {
	if k.lineLen == 0 {
		return ""
	}
	line := k.line[:k.lineLen]
	over := k.lineOver
	k.lineLen = 0
	k.lineOver = false
	if !over && holdsStandalonePrivateKeyLine(line) {
		return whyPrivateKey
	}
	if checkHashes && holdsHashLine(line, false) {
		return whyHashes
	}
	return ""
}

func (k *keyScan) keepTail(part []byte) {
	if len(part) >= keyTail {
		k.tailLen = copy(k.tail[:], part[len(part)-keyTail:])
		return
	}
	keep := min(k.tailLen, keyTail-len(part))
	if keep < k.tailLen {
		copy(k.tail[:], k.tail[k.tailLen-keep:k.tailLen])
	}
	copy(k.tail[keep:], part)
	k.tailLen = keep + len(part)
}

func (k *keyScan) scanPuTTYHeaders(buf []byte, atEOF bool) bool {
	if !bytes.Contains(buf, puttyHeaderMark) {
		return false
	}
	for off := 0; off < len(buf); {
		i := bytes.Index(buf[off:], puttyHeaderMark)
		if i < 0 {
			return false
		}
		pos := off + i
		if end, ok := matchPuTTYHeaderAt(buf, pos); ok {
			limit := min(len(buf), end+puttyWindow)
			if hasPuTTYPrivateLines(buf[end:limit], atEOF) {
				return true
			}
			if rem := puttyWindow - (len(buf) - end); rem > k.puttyRemain {
				k.puttyRemain = rem
			}
		}
		off = pos + len(puttyHeaderMark)
	}
	return false
}

func matchPuTTYInBuf(buf []byte, atEOF bool) bool {
	if !bytes.Contains(buf, puttyHeaderMark) {
		return false
	}
	for off := 0; off < len(buf); {
		i := bytes.Index(buf[off:], puttyHeaderMark)
		if i < 0 {
			return false
		}
		pos := off + i
		if end, ok := matchPuTTYHeaderAt(buf, pos); ok {
			limit := min(len(buf), end+puttyWindow)
			if hasPuTTYPrivateLines(buf[end:limit], atEOF) {
				return true
			}
		}
		off = pos + len(puttyHeaderMark)
	}
	return false
}

// holdsStandalonePrivateKeyLine matches a PEM, PGP, or SSH2 SECSH private key
// BEGIN header standing alone on a line (indented or after a BOM).
func holdsStandalonePrivateKeyLine(line []byte) bool {
	line = bytes.TrimLeft(line, beforeKey)
	if bytes.HasPrefix(line, beginPEMMark) {
		line = bytes.TrimRight(line, " \t\r\n")
		if bytes.HasSuffix(line, fiveDashes) && len(line) > len(beginPEMMark)+len(fiveDashes) {
			return isPrivatePEMLabel(line[len(beginPEMMark) : len(line)-len(fiveDashes)])
		}
		return false
	}
	if bytes.HasPrefix(line, beginSSH2Mark) {
		line = bytes.TrimRight(line, " \t\r\n")
		rest := line[len(beginSSH2Mark):]
		n, ok := matchSSH2PrivateLabel(rest)
		return ok && n == len(rest)
	}
	return false
}

// matchKeyBlock finds a PEM, PGP, SSH2 SECSH, or base64-wrapped PEM private key
// block anywhere in buf (raw or escaped) when followed by encryption/armor/RFC-4716
// headers or a base64 key body.
func matchKeyBlock(buf []byte, atEOF bool) bool {
	if bytes.Contains(buf, beginPEMMark) {
		for off := 0; off < len(buf); {
			i := bytes.Index(buf[off:], beginPEMMark)
			if i < 0 {
				break
			}
			pos := off + i
			if matchPEMAt(buf[pos:], atEOF) {
				return true
			}
			off = pos + len(beginPEMMark)
		}
	}
	if bytes.Contains(buf, beginSSH2Mark) {
		for off := 0; off < len(buf); {
			i := bytes.Index(buf[off:], beginSSH2Mark)
			if i < 0 {
				break
			}
			pos := off + i
			if matchSSH2At(buf[pos:], atEOF) {
				return true
			}
			off = pos + len(beginSSH2Mark)
		}
	}
	if matchBase64PEM(buf, atEOF) {
		return true
	}
	return false
}

func matchBase64PEM(buf []byte, atEOF bool) bool {
	if bytes.Contains(buf, b64PEMMark0) {
		for off := 0; off < len(buf); {
			i := bytes.Index(buf[off:], b64PEMMark0)
			if i < 0 {
				break
			}
			pos := off + i
			if matchBase64PEMAt(buf[pos:], "", 0, atEOF) {
				return true
			}
			off = pos + len(b64PEMMark0)
		}
	}
	if bytes.Contains(buf, b64PEMMark1) {
		for off := 0; off < len(buf); {
			i := bytes.Index(buf[off:], b64PEMMark1)
			if i < 0 {
				break
			}
			pos := off + i
			if pos > 0 {
				if v := b64Val(buf[pos-1]); v >= 0 && v&0x0f == 0x02 && matchBase64PEMAt(buf[pos:], "AC", 1, atEOF) {
					return true
				}
			}
			off = pos + len(b64PEMMark1)
		}
	}
	if bytes.Contains(buf, b64PEMMark2) {
		for off := 0; off < len(buf); {
			i := bytes.Index(buf[off:], b64PEMMark2)
			if i < 0 {
				break
			}
			pos := off + i
			if pos > 0 {
				if v := b64Val(buf[pos-1]); v >= 0 && v&0x03 == 0x00 && matchBase64PEMAt(buf[pos:], "AAA", 2, atEOF) {
					return true
				}
			}
			off = pos + len(b64PEMMark2)
		}
	}
	return false
}

// matchBase64PEMAt decodes at most 160 base64 characters (~120 bytes) into
// fixed stack buffers and checks whether they start a PEM private key block.
func matchBase64PEMAt(src []byte, prefix string, skip int, atEOF bool) bool {
	var encoded [160]byte
	nb := copy(encoded[:], prefix)
	for i := 0; i < len(src) && nb < len(encoded); i++ {
		c := src[i]
		switch {
		case isASCIILetter(c) || isASCIIDigit(c) || c == '+' || c == '/':
			encoded[nb] = c
			nb++
		case c == '-':
			encoded[nb] = '+'
			nb++
		case c == '_':
			encoded[nb] = '/'
			nb++
		case c == '\r' || c == '\n' || c == ' ' || c == '\t':
		case c == '\\':
			bs := countBackslashes(src[i:])
			if bs > 0 && i+bs < len(src) {
				rem := src[i+bs:]
				switch rem[0] {
				case '/', '+':
					encoded[nb] = rem[0]
					nb++
					i += bs
					continue
				case 'n', 'r':
					i += bs
					continue
				case 'u', 'U':
					if len(rem) >= 5 && rem[1] == '0' && rem[2] == '0' {
						switch {
						case eqHex2(rem[3:5], "2b"):
							encoded[nb] = '+'
							nb++
							i += bs + 4
							continue
						case eqHex2(rem[3:5], "2f"):
							encoded[nb] = '/'
							nb++
							i += bs + 4
							continue
						}
					}
				}
			}
			i = len(src)
		default:
			i = len(src)
		}
	}
	if nb%4 == 1 {
		nb--
	}
	if nb < 16 {
		return false
	}
	var decoded [120]byte
	dn, err := base64.RawStdEncoding.Decode(decoded[:], encoded[:nb])
	if err != nil || dn-skip <= len(beginPEMMark) || !bytes.HasPrefix(decoded[skip:dn], beginPEMMark) {
		return false
	}
	return matchPEMAt(decoded[skip:dn], atEOF)
}

func b64Val(c byte) int {
	switch {
	case c >= 'A' && c <= 'Z':
		return int(c - 'A')
	case c >= 'a' && c <= 'z':
		return int(c-'a') + 26
	case c >= '0' && c <= '9':
		return int(c-'0') + 52
	case c == '+' || c == '-':
		return 62
	case c == '/' || c == '_':
		return 63
	default:
		return -1
	}
}

func matchPEMAt(b []byte, atEOF bool) bool {
	rest := b[len(beginPEMMark):]
	end := bytes.Index(rest[:min(len(rest), 80)], fiveDashes)
	if end <= 0 {
		return false
	}
	label := rest[:end]
	if !isPrivatePEMLabel(label) {
		return false
	}
	after := skipHSpace(rest[end+len(fiveDashes):])
	lb, escaped, ok := detectLineBreak(after)
	if !ok {
		return false
	}
	body := after[lb:]
	// Skip at most one optional blank line (e.g. PGP armor with no headers).
	if s := skipHSpace(body); len(s) > 0 {
		if lb2, ok := consumeLineBreak(s, escaped); ok {
			body = s[lb2:]
		}
	}
	isPGP := bytes.Equal(label, []byte("PGP PRIVATE KEY BLOCK"))
	if matchKeyHeaderLine(skipHSpace(body), escaped, isPGP, false) {
		return true
	}
	return matchBase64Body(body, escaped, false, atEOF)
}

func matchSSH2At(b []byte, atEOF bool) bool {
	rest := b[len(beginSSH2Mark):]
	n, ok := matchSSH2PrivateLabel(rest)
	if !ok {
		return false
	}
	after := skipHSpace(rest[n:])
	lb, escaped, ok := detectLineBreak(after)
	if !ok {
		return false
	}
	body := after[lb:]
	if s := skipHSpace(body); len(s) > 0 {
		if lb2, ok := consumeLineBreak(s, escaped); ok {
			body = s[lb2:]
		}
	}
	if matchKeyHeaderLine(skipHSpace(body), escaped, false, true) {
		return true
	}
	return matchBase64Body(body, escaped, true, atEOF)
}

func isPrivatePEMLabel(label []byte) bool {
	for _, b := range label {
		if !isPEMLabelChar(b) {
			return false
		}
	}
	if hasWordSuffix(label, []byte("PRIVATE KEY")) {
		return true
	}
	return hasWordSuffix(label, []byte("PRIVATE KEY BLOCK"))
}

func hasWordSuffix(b, suffix []byte) bool {
	if !bytes.HasSuffix(b, suffix) {
		return false
	}
	if len(b) == len(suffix) {
		return true
	}
	prev := b[len(b)-len(suffix)-1]
	return prev == ' ' || prev == '-'
}

func matchSSH2PrivateLabel(rest []byte) (int, bool) {
	if bytes.HasPrefix(rest, []byte("ENCRYPTED PRIVATE KEY ----")) {
		return len("ENCRYPTED PRIVATE KEY ----"), true
	}
	if bytes.HasPrefix(rest, []byte("PRIVATE KEY ----")) {
		return len("PRIVATE KEY ----"), true
	}
	return 0, false
}

func matchKeyHeaderLine(s []byte, escaped, isPGP, isSSH2 bool) bool {
	switch {
	case isSSH2:
		// RFC 4716 header: Header-Tag: value (e.g. Subject:, Comment:).
		if len(s) == 0 || !isASCIILetter(s[0]) {
			return false
		}
		i := 1
		for i < len(s) && i < 64 && (isASCIILetter(s[i]) || isASCIIDigit(s[i]) || s[i] == '-') {
			i++
		}
		if i >= len(s) || s[i] != ':' {
			return false
		}
		val := skipHSpace(s[i+1:])
		return hasHeaderValue(val, escaped)
	case isPGP:
		for _, hdr := range [][]byte{
			[]byte("Version:"), []byte("Comment:"), []byte("MessageID:"), []byte("Hash:"), []byte("Charset:"),
		} {
			if bytes.HasPrefix(s, hdr) {
				return hasHeaderValue(skipHSpace(s[len(hdr):]), escaped)
			}
		}
		return false
	default:
		// RFC 1421 encrypted PEM headers (Proc-Type:, DEK-Info:), case-insensitive.
		for _, hdr := range [][]byte{[]byte("Proc-Type:"), []byte("DEK-Info:")} {
			if len(s) >= len(hdr) && bytes.EqualFold(s[:len(hdr)], hdr) {
				return hasHeaderValue(skipHSpace(s[len(hdr):]), escaped)
			}
		}
		return false
	}
}

func hasHeaderValue(val []byte, escaped bool) bool {
	if len(val) == 0 {
		return false
	}
	if _, ok := consumeLineBreak(val, escaped); ok {
		return false
	}
	b := val[0]
	if b < 0x21 || b > 0x7e {
		return false
	}
	if escaped && b == '"' {
		return false
	}
	return true
}

// matchBase64Body checks whether b starts with base64 key lines: either a line
// of 16+ base64 chars, multiple short base64-only lines totaling 16+ chars (L1),
// or 4+ base64 chars followed directly by the matching END header.
func matchBase64Body(b []byte, escaped, isSSH2, _ bool) bool {
	total := 0
	for len(b) > 0 {
		s := skipHSpace(b)
		if total >= 4 && matchEndHeader(s, isSSH2) {
			return true
		}
		run, pos := 0, 0
		for pos < len(s) {
			step := nextBase64Token(s[pos:], escaped)
			if step == 0 {
				break
			}
			pos += step
			run++
		}
		if run == 0 {
			return false
		}
		if run >= 16 {
			return true
		}
		rest := skipHSpace(s[pos:])
		if lb, ok := consumeLineBreak(rest, escaped); ok {
			total += run
			if total >= 16 {
				return true
			}
			b = rest[lb:]
			continue
		}
		if total+run >= 16 && (len(rest) == 0 || rest[0] == '"' || rest[0] == '\'' || (escaped && rest[0] == '\\')) {
			return true
		}
		return false
	}
	return false
}

// nextBase64Token returns the byte length of one base64 character at the start
// of s, including JSON-escaped \/, \+, \u002b, \u002f, and \u003d in escaped mode.
func nextBase64Token(s []byte, escaped bool) int {
	if len(s) == 0 {
		return 0
	}
	if isBase64Char(s[0]) {
		return 1
	}
	if !escaped || s[0] != '\\' {
		return 0
	}
	bs := countBackslashes(s)
	if bs == 0 || bs >= len(s) {
		return 0
	}
	rem := s[bs:]
	if rem[0] == '/' || rem[0] == '+' {
		return bs + 1
	}
	if len(rem) >= 5 && (rem[0] == 'u' || rem[0] == 'U') && rem[1] == '0' && rem[2] == '0' {
		if eqHex2(rem[3:5], "2b") || eqHex2(rem[3:5], "2f") || eqHex2(rem[3:5], "3d") {
			return bs + 5
		}
	}
	return 0
}

func matchEndHeader(s []byte, isSSH2 bool) bool {
	if isSSH2 {
		if !bytes.HasPrefix(s, endSSH2Mark) {
			return false
		}
		_, ok := matchSSH2PrivateLabel(s[len(endSSH2Mark):])
		return ok
	}
	if !bytes.HasPrefix(s, endPEMMark) {
		return false
	}
	rest := s[len(endPEMMark):]
	end := bytes.Index(rest[:min(len(rest), 80)], fiveDashes)
	return end > 0 && isPrivatePEMLabel(rest[:end])
}

func matchPuTTYHeaderAt(buf []byte, pos int) (int, bool) {
	rest := buf[pos+len(puttyHeaderMark):]
	i := 0
	for i < len(rest) && isASCIIDigit(rest[i]) {
		i++
	}
	if i == 0 || i >= len(rest) || rest[i] != ':' {
		return 0, false
	}
	after := skipHSpace(rest[i+1:])
	alg := 0
	for alg < len(after) && isPuTTYAlgChar(after[alg]) {
		alg++
	}
	if alg == 0 {
		return 0, false
	}
	after = skipHSpace(after[alg:])
	lb, _, ok := detectLineBreak(after)
	if !ok {
		return 0, false
	}
	consumed := len(buf) - len(after) + lb
	return consumed, true
}

func hasPuTTYPrivateLines(buf []byte, atEOF bool) bool {
	if !bytes.Contains(buf, puttyPrivMark) {
		return false
	}
	for off := 0; off < len(buf); {
		i := bytes.Index(buf[off:], puttyPrivMark)
		if i < 0 {
			return false
		}
		pos := off + i
		if lineStartsBefore(buf, pos) {
			after := skipHSpace(buf[pos+len(puttyPrivMark):])
			if len(after) > 0 && after[0] >= '1' && after[0] <= '9' {
				d := 1
				for d < len(after) && isASCIIDigit(after[d]) {
					d++
				}
				tail := skipHSpace(after[d:])
				if _, _, ok := detectLineBreak(tail); ok {
					return true
				}
				if len(tail) == 0 && atEOF {
					return true
				}
				if len(tail) > 0 && (tail[0] == '"' || tail[0] == '\'') {
					return true
				}
			}
		}
		off = pos + len(puttyPrivMark)
	}
	return false
}

// lineStartsBefore reports whether pos starts a line (after start-of-buffer,
// newline, NUL, or \n escape, skipping horizontal whitespace).
func lineStartsBefore(buf []byte, pos int) bool {
	i := pos
	for i > 0 && (buf[i-1] == ' ' || buf[i-1] == '\t') {
		i--
	}
	if i == 0 {
		return true
	}
	switch buf[i-1] {
	case '\n', '\r', 0:
		return true
	case 'n':
		return i >= 2 && buf[i-2] == '\\'
	}
	return false
}

func detectLineBreak(b []byte) (n int, escaped, ok bool) {
	if lb, matched := consumeLineBreak(b, false); matched {
		return lb, false, true
	}
	if lb, matched := consumeLineBreak(b, true); matched {
		return lb, true, true
	}
	return 0, false, false
}

func consumeLineBreak(b []byte, escaped bool) (int, bool) {
	if !escaped {
		if len(b) >= 2 && b[0] == '\r' && b[1] == '\n' {
			return 2, true
		}
		if len(b) >= 1 && b[0] == '\n' {
			return 1, true
		}
		return 0, false
	}
	if r, ok := consumeEscapedByte(b, 'r', "0d"); ok {
		if n, ok2 := consumeEscapedByte(b[r:], 'n', "0a"); ok2 {
			return r + n, true
		}
	}
	return consumeEscapedByte(b, 'n', "0a")
}

func consumeEscapedByte(b []byte, letter byte, hex2 string) (int, bool) {
	bs := countBackslashes(b)
	if bs == 0 || bs >= len(b) {
		return 0, false
	}
	rem := b[bs:]
	if rem[0] == letter {
		return bs + 1, true
	}
	if len(rem) >= 5 && (rem[0] == 'u' || rem[0] == 'U') && rem[1] == '0' && rem[2] == '0' && eqHex2(rem[3:5], hex2) {
		return bs + 5, true
	}
	return 0, false
}

func countBackslashes(b []byte) int {
	n := 0
	for n < len(b) && n < 8 && b[n] == '\\' {
		n++
	}
	return n
}

func eqHex2(b []byte, lower string) bool {
	return len(b) >= 2 && lowerASCII(b[0]) == lower[0] && lowerASCII(b[1]) == lower[1]
}

func lowerASCII(c byte) byte {
	if c >= 'A' && c <= 'Z' {
		return c + ('a' - 'A')
	}
	return c
}

func skipHSpace(b []byte) []byte {
	i := 0
	for i < len(b) && (b[i] == ' ' || b[i] == '\t') {
		i++
	}
	return b[i:]
}

func isPEMLabelChar(b byte) bool {
	return (b >= 'A' && b <= 'Z') || isASCIIDigit(b) || b == ' ' || b == '-'
}

func isASCIILetter(b byte) bool {
	return (b >= 'A' && b <= 'Z') || (b >= 'a' && b <= 'z')
}

func isASCIIDigit(b byte) bool {
	return b >= '0' && b <= '9'
}

func isBase64Char(b byte) bool {
	return isASCIILetter(b) || isASCIIDigit(b) || b == '+' || b == '/' || b == '='
}

func isPuTTYAlgChar(b byte) bool {
	return isASCIILetter(b) || isASCIIDigit(b) || b == '-' || b == '@' || b == '.' || b == '_'
}

// textLine says whether a line's start reads as text: UTF-8 with no control
// bytes but tabs and a carriage return. A password file is text, and a run of
// random bytes in an image or an archive that happens to look like one of its
// lines isn't (CI on #921: random 2 MiB files tripped hashLine). The start is
// cut at lineHead (or at a part boundary when open), so a valid rune cut there
// still counts as text.
func textLine(line []byte, open bool) bool {
	for _, b := range line {
		if b < 0x20 && b != '\t' && b != '\r' || b == 0x7f {
			return false
		}
	}
	if utf8.Valid(line) {
		return true
	}
	if !open && len(line) < lineHead {
		return false
	}
	return utf8.Valid(trimRune(line))
}

// nulLine keeps the text of a line after its last NUL: a password file's first
// line, inside a tar, starts right after its member header's NUL padding, which
// the line before it never ends with a newline (the key-scan review's M1). It's
// read as a password file's line only when it reads as text, so binary data
// that holds a NUL isn't mistaken for one.
type nulLine struct {
	buf   [lineHead]byte
	n     int
	after bool
}

// feed takes a newline-free segment of a line, and whether the line ends with
// it, and says whether the text after a NUL is a hash line (including an open
// line at the end of a part, so nothing of a hash line leaves across parts).
func (l *nulLine) feed(seg []byte, ended bool) bool {
	// Fast path: when neither the buffered post-NUL text nor seg contains '$',
	// no hash line can match in seg, so only keep the tail after the last NUL.
	if bytes.IndexByte(seg, '$') < 0 && (!l.after || bytes.IndexByte(l.buf[:l.n], '$') < 0) {
		if i := bytes.LastIndexByte(seg, 0); i >= 0 {
			l.n, l.after, seg = 0, true, seg[i+1:]
		}
		if l.after {
			l.n += copy(l.buf[l.n:], seg)
		}
		if ended {
			l.n, l.after = 0, false
		}
		return false
	}
	for len(seg) > 0 {
		before, after, hasNul := bytes.Cut(seg, []byte{0})
		if l.after && len(before) > 0 {
			l.n += copy(l.buf[l.n:], before)
		}
		if hasNul {
			if l.after && l.n > 0 && holdsHashLine(l.buf[:l.n], false) {
				l.n, l.after = 0, false
				return true
			}
			l.n, l.after = 0, true
			seg = after
			continue
		}
		break
	}
	if !ended {
		return l.after && l.n > 0 && holdsHashLine(l.buf[:l.n], true)
	}
	found := l.after && l.n > 0 && holdsHashLine(l.buf[:l.n], false)
	l.n, l.after = 0, false
	return found
}

func (l *nulLine) finish() bool {
	if !l.after || l.n == 0 {
		return false
	}
	found := holdsHashLine(l.buf[:l.n], false)
	l.n, l.after = 0, false
	return found
}

// holdsHashLine says whether a line reads as text and is a password file's line.
func holdsHashLine(line []byte, open bool) bool {
	return bytes.IndexByte(line, '$') >= 0 && textLine(line, open) && hashLine().Match(line)
}
