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
// start, klog's E0102 prefix, and how runtimes say they crashed. A line that
// says none has none: its stream says nothing, as many apps log everything to
// standard error. It can be fooled by what an app prints, so it only ever sorts
// lines for reading.
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
	head := text[:min(len(text), wordReach)]
	if l := wordLevel(head); l != dockwellv1.LogLevel_LOG_LEVEL_UNSPECIFIED {
		return l
	}
	return crashLevel(text, head)
}

// isContinuation reports a line that carries on the one before it, as the
// frames of a stack trace do, so it takes that line's level. Most are indented.
// Go's and PHP's frames aren't, so they are read by their form.
func isContinuation(line []byte) bool {
	if len(line) == 0 {
		return false
	}
	if line[0] == ' ' || line[0] == '\t' {
		return len(bytes.TrimSpace(line)) > 0
	}
	for _, prefix := range continuationPrefixes {
		if bytes.HasPrefix(line, prefix) {
			return true
		}
	}
	return isNumberedFrame(line) || isCallFrame(line) || isCloser(line)
}

// isCloser reads a line of nothing but closing brackets, as what a runtime prints
// of an object ends with, at the start of the line, after its indented fields.
func isCloser(line []byte) bool {
	for _, c := range line {
		switch c {
		case '}', ']', ')', ',', ';', ' ':
		default:
			return false
		}
	}
	return true
}

var continuationPrefixes = [][]byte{
	[]byte("Caused by:"), []byte("goroutine "), []byte("created by "), []byte("[signal "), []byte("Stack trace:"),
	[]byte("stack backtrace:"), []byte("note: run with"), []byte("note: Some details"), []byte("at "), []byte("from "),
	[]byte("Suppressed:"), []byte("Node.js v"),
}

// isNumberedFrame reads "#3 /app/x.php(3): foo()", as PHP and gdb number frames.
func isNumberedFrame(line []byte) bool {
	if line[0] != '#' {
		return false
	}
	i := 1
	for i < len(line) && line[i] >= '0' && line[i] <= '9' {
		i++
	}
	return i > 1 && i < len(line) && line[i] == ' '
}

// isCallFrame reads "main.handle(0x1, 0x2)", as Go writes a frame: a dotted name
// with no space in it, and its arguments to the end of the line.
func isCallFrame(line []byte) bool {
	open := bytes.IndexByte(line, '(')
	if open < 3 || line[len(line)-1] != ')' {
		return false
	}
	name := line[:open]
	return bytes.IndexByte(name, '.') > 0 && bytes.IndexByte(name, ' ') < 0
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
// first on the line and followed by a colon or a code, as "error: no such file"
// and "error[E0425]: ...", since lowercase words are mostly what a line is about:
// "no error", "error rate".
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
		leading := start == 0 && i < len(text) && (text[i] == ':' || text[i] == '[')
		if isUpper(word) || bracketed || leading {
			return level
		}
	}
	return dockwellv1.LogLevel_LOG_LEVEL_UNSPECIFIED
}

// crashPhrases are how runtimes say they crashed or are about to, wherever they
// stand near the start of a line. They are long and particular enough that a line
// about something else rarely holds one.
var crashPhrases = [...]struct {
	text  []byte
	level dockwellv1.LogLevel
}{
	{[]byte("Traceback (most recent call last)"), dockwellv1.LogLevel_LOG_LEVEL_ERROR},
	{[]byte("During handling of the above exception"), dockwellv1.LogLevel_LOG_LEVEL_ERROR},
	{[]byte("The above exception was the direct cause"), dockwellv1.LogLevel_LOG_LEVEL_ERROR},
	{[]byte("Exception in thread"), dockwellv1.LogLevel_LOG_LEVEL_ERROR},
	{[]byte("Uncaught Exception"), dockwellv1.LogLevel_LOG_LEVEL_ERROR},
	{[]byte("Uncaught Error"), dockwellv1.LogLevel_LOG_LEVEL_ERROR},
	{[]byte("UnhandledPromiseRejection"), dockwellv1.LogLevel_LOG_LEVEL_ERROR},
	{[]byte("Unhandled Rejection"), dockwellv1.LogLevel_LOG_LEVEL_ERROR},
	{[]byte("Caused by:"), dockwellv1.LogLevel_LOG_LEVEL_ERROR},
	{[]byte("npm ERR!"), dockwellv1.LogLevel_LOG_LEVEL_ERROR},
	{[]byte("npm error"), dockwellv1.LogLevel_LOG_LEVEL_ERROR},
	{[]byte("npm WARN"), dockwellv1.LogLevel_LOG_LEVEL_WARN},
	{[]byte("npm warn"), dockwellv1.LogLevel_LOG_LEVEL_WARN},
	{[]byte("error Command failed"), dockwellv1.LogLevel_LOG_LEVEL_ERROR},
	{[]byte("PHP Fatal error"), dockwellv1.LogLevel_LOG_LEVEL_ERROR},
	{[]byte("PHP Parse error"), dockwellv1.LogLevel_LOG_LEVEL_ERROR},
	{[]byte("PHP Warning"), dockwellv1.LogLevel_LOG_LEVEL_WARN},
	{[]byte("PHP Deprecated"), dockwellv1.LogLevel_LOG_LEVEL_WARN},
	{[]byte("PHP Notice"), dockwellv1.LogLevel_LOG_LEVEL_INFO},
	{[]byte("Fatal error:"), dockwellv1.LogLevel_LOG_LEVEL_ERROR},
	{[]byte("fatal error:"), dockwellv1.LogLevel_LOG_LEVEL_ERROR},
	{[]byte("Parse error:"), dockwellv1.LogLevel_LOG_LEVEL_ERROR},
	{[]byte("panicked at"), dockwellv1.LogLevel_LOG_LEVEL_ERROR},
	{[]byte("Segmentation fault"), dockwellv1.LogLevel_LOG_LEVEL_ERROR},
	{[]byte("Bus error"), dockwellv1.LogLevel_LOG_LEVEL_ERROR},
	{[]byte("Illegal instruction"), dockwellv1.LogLevel_LOG_LEVEL_ERROR},
	{[]byte("Aborted (core dumped)"), dockwellv1.LogLevel_LOG_LEVEL_ERROR},
	{[]byte("core dumped"), dockwellv1.LogLevel_LOG_LEVEL_ERROR},
	{[]byte("terminate called"), dockwellv1.LogLevel_LOG_LEVEL_ERROR},
	{[]byte("double free or corruption"), dockwellv1.LogLevel_LOG_LEVEL_ERROR},
	{[]byte("stack smashing detected"), dockwellv1.LogLevel_LOG_LEVEL_ERROR},
	{[]byte("buffer overflow detected"), dockwellv1.LogLevel_LOG_LEVEL_ERROR},
	{[]byte("Assertion failed"), dockwellv1.LogLevel_LOG_LEVEL_ERROR},
	{[]byte("error while loading shared libraries"), dockwellv1.LogLevel_LOG_LEVEL_ERROR},
	{[]byte("exec format error"), dockwellv1.LogLevel_LOG_LEVEL_ERROR},
	{[]byte("exec user process caused"), dockwellv1.LogLevel_LOG_LEVEL_ERROR},
	{[]byte("JavaScript heap out of memory"), dockwellv1.LogLevel_LOG_LEVEL_ERROR},
	{[]byte("OutOfMemoryError"), dockwellv1.LogLevel_LOG_LEVEL_ERROR},
	{[]byte("Out of memory: Killed"), dockwellv1.LogLevel_LOG_LEVEL_ERROR},
}

// crashLevel reads how a runtime says it crashed, which often names no level in
// so many words: a phrase near the start of the line, an exception's name leading
// it, or Ruby's form, which ends with the exception's name in brackets. text is
// all that was read of the line, and head what a word counts in.
func crashLevel(text, head []byte) dockwellv1.LogLevel {
	for i := range crashPhrases {
		if bytes.Contains(head, crashPhrases[i].text) {
			return crashPhrases[i].level
		}
	}
	if l := exceptionLevel(text); l != dockwellv1.LogLevel_LOG_LEVEL_UNSPECIFIED {
		return l
	}
	if failureLine(text) {
		return dockwellv1.LogLevel_LOG_LEVEL_ERROR
	}
	if bytes.Contains(text, []byte(":in ")) && text[len(text)-1] == ')' {
		if open := bytes.LastIndexByte(text, '('); open >= 0 && exceptionName(text[open+1:len(text)-1]) == dockwellv1.LogLevel_LOG_LEVEL_ERROR {
			return dockwellv1.LogLevel_LOG_LEVEL_ERROR
		}
	}
	return dockwellv1.LogLevel_LOG_LEVEL_UNSPECIFIED
}

// failureLine reads the plainest ways a process says it failed: a shell's Killed
// for a process the kernel ended, a non-zero exit status, and what a Go program
// passes to log.Fatalf, which by custom starts "failed to".
func failureLine(text []byte) bool {
	if string(text) == "Killed" {
		return true
	}
	if rest, ok := bytes.CutPrefix(text, []byte("exit status ")); ok {
		return len(rest) > 0 && (len(rest) != 1 || rest[0] != '0')
	}
	return bytes.HasPrefix(text, []byte("failed to ")) || bytes.HasPrefix(text, []byte("Failed to "))
}

// exceptionLevel reads a line that leads with an exception's name, as "ValueError:
// bad input", "java.lang.IllegalStateException: boom" or "RoutingError (No route):".
// One preceded by where it came from, as "/app/x.py:3: DeprecationWarning: soon"
// counts too.
func exceptionLevel(text []byte) dockwellv1.LogLevel {
	first, rest, _ := bytes.Cut(text, []byte{' '})
	if l := exceptionToken(first, rest); l != dockwellv1.LogLevel_LOG_LEVEL_UNSPECIFIED {
		return l
	}
	if len(first) > 1 && first[len(first)-1] == ':' && bytes.ContainsAny(first, "/.") {
		second, after, _ := bytes.Cut(rest, []byte{' '})
		return exceptionToken(second, after)
	}
	return dockwellv1.LogLevel_LOG_LEVEL_UNSPECIFIED
}

// exceptionToken reads one word as an exception's name, by how it ends, and by
// what stands after it: a colon, the end of the line, or a bracket.
func exceptionToken(word, rest []byte) dockwellv1.LogLevel {
	switch {
	case len(word) > 1 && word[len(word)-1] == ':':
		word = word[:len(word)-1]
	case len(rest) == 0:
		// On a line of its own, only a name that can't be a word of a sentence.
		if !bytes.ContainsAny(word, ".:$") {
			return dockwellv1.LogLevel_LOG_LEVEL_UNSPECIFIED
		}
	case rest[0] == '(':
	default:
		return dockwellv1.LogLevel_LOG_LEVEL_UNSPECIFIED
	}
	return exceptionName(word)
}

// exceptionName reads a whole word as the name of an exception or a warning: a
// dotted or scoped name, whose last part starts with a capital and ends so.
func exceptionName(word []byte) dockwellv1.LogLevel {
	last := word
	for i, c := range word {
		switch {
		case c == '.' || c == ':' || c == '\\':
			last = word[i+1:]
		case !isLetter(c) && (c < '0' || c > '9') && c != '_' && c != '$':
			return dockwellv1.LogLevel_LOG_LEVEL_UNSPECIFIED
		}
	}
	if len(last) == 0 || last[0] < 'A' || last[0] > 'Z' {
		return dockwellv1.LogLevel_LOG_LEVEL_UNSPECIFIED
	}
	switch {
	case bytes.HasSuffix(last, []byte("Warning")):
		return dockwellv1.LogLevel_LOG_LEVEL_WARN
	case bytes.HasSuffix(last, []byte("Error")), bytes.HasSuffix(last, []byte("Exception")), bytes.HasSuffix(last, []byte("Panic")):
		return dockwellv1.LogLevel_LOG_LEVEL_ERROR
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
