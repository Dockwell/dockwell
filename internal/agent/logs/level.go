// SPDX-License-Identifier: AGPL-3.0-only

package logs

import (
	"bytes"

	dockwellv1 "github.com/dockwell/dockwell/gen/dockwell/v1"
)

// levelReach is how far into a line its level is looked for. Fields such as
// a logger's name or a time come first in most formats, and words further on
// are more likely to be what the line is about than how serious it is.
const levelReach = 256

// wordReach is how far into a line a bare word such as ERROR counts.
const wordReach = 96

// ReadLevel finds the level a line says it has, without allocating. It reads
// JSON and logfmt fields, then words such as ERROR, [warn] or error: near the
// start, and klog's E0102 prefix. A line that says none has none: its stream
// says nothing, as many apps log everything to standard error. It can be
// fooled by what an app prints, so it only ever sorts lines for reading.
func ReadLevel(line []byte) dockwellv1.LogLevel {
	var clean [levelReach]byte
	text := stripEscapes(clean[:0], line[:min(len(line), levelReach)])
	text = bytes.TrimLeft(text, " \t")
	if len(text) == 0 {
		return dockwellv1.LogLevel_LOG_LEVEL_UNSPECIFIED
	}
	if text[0] == '{' {
		if l := jsonLevel(text); l != dockwellv1.LogLevel_LOG_LEVEL_UNSPECIFIED {
			return l
		}
	}
	if l := fieldLevel(text); l != dockwellv1.LogLevel_LOG_LEVEL_UNSPECIFIED {
		return l
	}
	if l := klogLevel(text); l != dockwellv1.LogLevel_LOG_LEVEL_UNSPECIFIED {
		return l
	}
	return wordLevel(text[:min(len(text), wordReach)])
}

// isContinuation reports a line that carries on the one before it, as the
// frames of a stack trace do, so it takes that line's level.
func isContinuation(line []byte) bool {
	if len(line) == 0 {
		return false
	}
	if line[0] == ' ' || line[0] == '\t' {
		return len(bytes.TrimSpace(line)) > 0
	}
	return bytes.HasPrefix(line, []byte("Caused by:")) || bytes.HasPrefix(line, []byte("goroutine "))
}

// stripEscapes copies text to out without terminal escape codes, which a
// coloured logger puts around its level.
func stripEscapes(out, text []byte) []byte {
	if bytes.IndexByte(text, 0x1b) < 0 {
		return append(out, text...)
	}
	for i := 0; i < len(text); i++ {
		if text[i] != 0x1b {
			out = append(out, text[i])
			continue
		}
		if i+1 < len(text) && text[i+1] == '[' {
			i += 2
			for i < len(text) && (text[i] < 0x40 || text[i] > 0x7e) {
				i++
			}
		}
	}
	return out
}

// jsonLevel finds a level field in a JSON line, without decoding it, as most
// structured loggers put theirs near the start.
func jsonLevel(text []byte) dockwellv1.LogLevel {
	for _, key := range [...]string{`"level"`, `"lvl"`, `"severity"`, `"log.level"`, `"loglevel"`, `"levelname"`} {
		i := indexFold(text, key)
		if i < 0 {
			continue
		}
		rest := bytes.TrimLeft(text[i+len(key):], " \t")
		if len(rest) == 0 || rest[0] != ':' {
			continue
		}
		rest = bytes.TrimLeft(rest[1:], " \t")
		if len(rest) == 0 {
			continue
		}
		if rest[0] == '"' {
			rest = rest[1:]
			if end := bytes.IndexByte(rest, '"'); end >= 0 {
				return namedLevel(rest[:end])
			}
			continue
		}
		return numberLevel(rest)
	}
	return dockwellv1.LogLevel_LOG_LEVEL_UNSPECIFIED
}

// numberLevel reads a level as a number, as pino and bunyan write it: 10
// trace, 20 debug, 30 info, 40 warn, 50 error and 60 fatal.
func numberLevel(text []byte) dockwellv1.LogLevel {
	if len(text) < 2 || text[0] < '1' || text[0] > '6' || text[1] != '0' {
		return dockwellv1.LogLevel_LOG_LEVEL_UNSPECIFIED
	}
	switch text[0] {
	case '1', '2':
		return dockwellv1.LogLevel_LOG_LEVEL_DEBUG
	case '3':
		return dockwellv1.LogLevel_LOG_LEVEL_INFO
	case '4':
		return dockwellv1.LogLevel_LOG_LEVEL_WARN
	}
	return dockwellv1.LogLevel_LOG_LEVEL_ERROR
}

// fieldLevel finds a logfmt field, level=warn, with or without quotes.
func fieldLevel(text []byte) dockwellv1.LogLevel {
	for _, key := range [...]string{"level=", "lvl=", "severity="} {
		for from := 0; from < len(text); {
			i := indexFold(text[from:], key)
			if i < 0 {
				break
			}
			at := from + i
			from = at + len(key)
			if at > 0 && !boundary(text[at-1]) {
				continue
			}
			value := bytes.TrimLeft(text[from:], `"'`)
			end := 0
			for end < len(value) && isLetter(value[end]) {
				end++
			}
			if l := namedLevel(value[:end]); l != dockwellv1.LogLevel_LOG_LEVEL_UNSPECIFIED {
				return l
			}
		}
	}
	return dockwellv1.LogLevel_LOG_LEVEL_UNSPECIFIED
}

// klogLevel reads klog's and glog's prefix: a letter, then the month and day,
// as "E0102 15:04:05".
func klogLevel(text []byte) dockwellv1.LogLevel {
	if len(text) < 6 || text[5] != ' ' {
		return dockwellv1.LogLevel_LOG_LEVEL_UNSPECIFIED
	}
	for _, d := range text[1:5] {
		if d < '0' || d > '9' {
			return dockwellv1.LogLevel_LOG_LEVEL_UNSPECIFIED
		}
	}
	switch text[0] {
	case 'I':
		return dockwellv1.LogLevel_LOG_LEVEL_INFO
	case 'W':
		return dockwellv1.LogLevel_LOG_LEVEL_WARN
	case 'E', 'F':
		return dockwellv1.LogLevel_LOG_LEVEL_ERROR
	}
	return dockwellv1.LogLevel_LOG_LEVEL_UNSPECIFIED
}

// wordLevel looks at each word. A word in capitals counts where it stands
// alone, and one in lowercase only in brackets, as nginx writes [error], or
// first on the line and followed by a colon, as "error: no such file", since
// lowercase words are mostly what a line is about: "no error", "error rate".
func wordLevel(text []byte) dockwellv1.LogLevel {
	for i := 0; i < len(text); {
		if !isLetter(text[i]) {
			i++
			continue
		}
		start := i
		for i < len(text) && isLetter(text[i]) {
			i++
		}
		word := text[start:i]
		if start > 0 && !boundary(text[start-1]) {
			continue
		}
		if i < len(text) && !boundary(text[i]) {
			continue
		}
		level := namedLevel(word)
		if level == dockwellv1.LogLevel_LOG_LEVEL_UNSPECIFIED {
			continue
		}
		bracketed := start > 0 && (text[start-1] == '[' || text[start-1] == '<') && i < len(text) && (text[i] == ']' || text[i] == '>')
		leading := start == 0 && i < len(text) && text[i] == ':'
		if isUpper(word) || bracketed || leading {
			return level
		}
	}
	return dockwellv1.LogLevel_LOG_LEVEL_UNSPECIFIED
}

// namedLevel reads a level's name in any case.
func namedLevel(name []byte) dockwellv1.LogLevel {
	if len(name) == 0 || len(name) > 8 {
		return dockwellv1.LogLevel_LOG_LEVEL_UNSPECIFIED
	}
	var low [8]byte
	for i, c := range name {
		if 'A' <= c && c <= 'Z' {
			c += 'a' - 'A'
		}
		low[i] = c
	}
	switch string(low[:len(name)]) {
	case "trace", "debug", "dbug", "verbose":
		return dockwellv1.LogLevel_LOG_LEVEL_DEBUG
	case "info", "notice", "inf":
		return dockwellv1.LogLevel_LOG_LEVEL_INFO
	case "warn", "warning", "wrn":
		return dockwellv1.LogLevel_LOG_LEVEL_WARN
	case "error", "err", "eror", "fatal", "ftl", "panic", "crit", "critical", "alert", "emerg", "severe":
		return dockwellv1.LogLevel_LOG_LEVEL_ERROR
	}
	return dockwellv1.LogLevel_LOG_LEVEL_UNSPECIFIED
}

func isLetter(c byte) bool { return ('a' <= c && c <= 'z') || ('A' <= c && c <= 'Z') }

func isUpper(word []byte) bool {
	for _, c := range word {
		if c < 'A' || c > 'Z' {
			return false
		}
	}
	return true
}

// boundary reports what may stand next to a word that names a level. A slash,
// a dot or a hyphen may not, so a path such as /ERROR or a name such as
// error-rate isn't one.
func boundary(c byte) bool {
	switch c {
	case ' ', '\t', '[', ']', '(', ')', '<', '>', '|', ':', ',', ';', '"', '\'':
		return true
	}
	return false
}

// indexFold finds key, in lowercase, in text in any case. A linear scan on a
// short prefix of each line: it stays allocation-free.
func indexFold(text []byte, key string) int {
	if key == "" || len(text) < len(key) {
		return -1
	}
	for i := 0; i+len(key) <= len(text); i++ {
		match := true
		for j := range len(key) {
			c := text[i+j]
			if 'A' <= c && c <= 'Z' {
				c += 'a' - 'A'
			}
			if c != key[j] {
				match = false
				break
			}
		}
		if match {
			return i
		}
	}
	return -1
}
