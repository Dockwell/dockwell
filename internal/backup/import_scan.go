// SPDX-License-Identifier: AGPL-3.0-only

package backup

import (
	"errors"
	"fmt"
	"io"
	"strings"

	dockwellv1 "github.com/dockwell/dockwell/gen/dockwell/v1"
	"github.com/dockwell/dockwell/internal/agent/collect"
	"github.com/dockwell/dockwell/internal/lazyre"
)

// scanSQLDump validates a plain SQL stream for client meta-commands, shell
// escapes, cross-database switches, cluster/multi-database dump headers, role
// changes, and server-side file/program access before it is loaded.
func scanSQLDump(engine dockwellv1.DatabaseEngine, r io.Reader) error {
	sc := newSQLScanner(engine)
	if _, err := io.Copy(sc, r); err != nil {
		return err
	}
	return sc.Close()
}

// sqlScanReader scans SQL bytes as they stream from r, returning an error
// before handing any chunk containing a refused command to the caller.
type sqlScanReader struct {
	r   io.Reader
	sc  *sqlScanner
	err error
}

func newSQLScanReader(engine dockwellv1.DatabaseEngine, r io.Reader) *sqlScanReader {
	return &sqlScanReader{r: r, sc: newSQLScanner(engine)}
}

func (s *sqlScanReader) Read(p []byte) (int, error) {
	if s.err != nil {
		return 0, s.err
	}
	n, err := s.r.Read(p)
	if n > 0 {
		if serr := s.sc.feed(p[:n]); serr != nil {
			s.err = serr
			return 0, serr
		}
	}
	if errors.Is(err, io.EOF) {
		if serr := s.sc.Close(); serr != nil {
			s.err = serr
			return 0, serr
		}
	}
	return n, err
}

// sqlScanner is a constant-memory streaming SQL pre-scanner. It processes
// arbitrarily large dumps and single-line INSERT statements in O(1) memory.
type sqlScanner struct {
	engine dockwellv1.DatabaseEngine

	lineNo      int
	atLineStart bool
	lineTrimmed bool // saw first non-whitespace on current line

	// Lexer states.
	inLineComment     bool
	lineCommentBuf    []byte
	blockCommentDepth int
	inCommentSniff    bool // sniffing /* to detect MySQL /*! or MariaDB /*M! executable comments
	commentSniffExec  bool // saw /*! or /*M!, now reading optional version digits
	commentSniffBuf   []byte
	inExecComment     bool // currently inside /*! ... */ or /*M! ... */
	inSingleQuote     bool
	singleQuoteEscape bool // backslash escape active inside '...'
	inDoubleQuote     bool
	inBacktick        bool
	inDollarQuote     bool
	dollarTag         string
	dollarMatch       []byte

	// Backslash client command buffering (psql \..., mysql/mariadb \!, \., \u, etc.).
	inBackslash          bool
	backslashAtLineStart bool
	backslashBuf         []byte

	// Pending 2-char lookahead.
	prevByte byte
	hasPrev  bool

	// PostgreSQL COPY ... FROM stdin state.
	pendingCopyData bool
	inCopyData      bool
	copyLineBuf     []byte
	restrictKey     string

	// MySQL custom DELIMITER support.
	readingDelimiter bool
	delimArgBuf      []byte
	delim            string
	delimMatchBuf    []byte

	// Current SQL statement tracking (bounded to O(1) memory).
	stmtLine      int
	stmtFirst     []string // first up to 8 tokens of current statement
	stmtPrev2     string
	stmtPrev1     string
	curWord       []byte
	curWordQuoted bool

	err error
}

func newSQLScanner(engine dockwellv1.DatabaseEngine) *sqlScanner {
	return &sqlScanner{
		engine:      engine,
		lineNo:      1,
		atLineStart: true,
		delim:       ";",
	}
}

func (s *sqlScanner) Write(p []byte) (int, error) {
	if err := s.feed(p); err != nil {
		return 0, err
	}
	return len(p), nil
}

func (s *sqlScanner) Close() error {
	if s.err != nil {
		return s.err
	}
	if s.hasPrev {
		b := s.prevByte
		s.hasPrev = false
		if err := s.step(b, 0, false); err != nil {
			return err
		}
	}
	if err := s.flushWord(); err != nil {
		return err
	}
	if err := s.finishLine(); err != nil {
		return err
	}
	return s.finishStmt()
}

func (s *sqlScanner) feed(p []byte) error {
	if s.err != nil {
		return s.err
	}
	for _, b := range p {
		if !s.hasPrev {
			s.prevByte = b
			s.hasPrev = true
			continue
		}
		cur := s.prevByte
		s.prevByte = b
		if err := s.step(cur, b, true); err != nil {
			s.err = err
			return err
		}
	}
	return nil
}

func (s *sqlScanner) isPG() bool {
	return s.engine == dockwellv1.DatabaseEngine_DATABASE_ENGINE_POSTGRESQL
}

func (s *sqlScanner) step(b, next byte, hasNext bool) error {
	// Inside PostgreSQL COPY ... FROM stdin data block: only "\." on its own line ends it.
	if s.inCopyData {
		if b == '\n' {
			line := strings.TrimRight(string(s.copyLineBuf), "\r")
			s.copyLineBuf = s.copyLineBuf[:0]
			s.lineNo++
			if line == `\.` {
				s.inCopyData = false
				s.atLineStart = true
				s.lineTrimmed = false
			}
			return nil
		}
		if len(s.copyLineBuf) < 8 {
			s.copyLineBuf = append(s.copyLineBuf, b)
		}
		return nil
	}

	// Reading MySQL DELIMITER argument until newline.
	if s.readingDelimiter {
		if b == '\n' {
			s.finishDelimiter()
			s.onNewline()
			return nil
		}
		if len(s.delimArgBuf) < 64 {
			s.delimArgBuf = append(s.delimArgBuf, b)
		}
		return nil
	}

	// Buffering a backslash command until end of line so we can inspect it whole.
	if s.inBackslash {
		if b == '\n' {
			line := strings.TrimRight(string(s.backslashBuf), "\r")
			atStart := s.backslashAtLineStart
			s.inBackslash = false
			s.backslashAtLineStart = false
			s.backslashBuf = s.backslashBuf[:0]
			err := s.checkBackslashLine(line, atStart)
			s.onNewline()
			return err
		}
		if len(s.backslashBuf) < 256 {
			s.backslashBuf = append(s.backslashBuf, b)
		}
		return nil
	}

	// Line comment (-- or MySQL #): check for cluster/multi-database dump headers.
	if s.inLineComment {
		if b == '\n' {
			err := s.checkCommentHeader(string(s.lineCommentBuf))
			s.inLineComment = false
			s.lineCommentBuf = s.lineCommentBuf[:0]
			s.onNewline()
			return err
		}
		if len(s.lineCommentBuf) < 128 {
			s.lineCommentBuf = append(s.lineCommentBuf, b)
		}
		return nil
	}

	// Sniffing MySQL/MariaDB block comment opening: /*! or /*M! executable comments
	// are unwrapped as SQL, while MariaDB's sandbox pragma (/*M!999999\- ... */)
	// and normal /* ... */ comments stay comments.
	if s.inCommentSniff {
		if !s.commentSniffExec {
			switch {
			case len(s.commentSniffBuf) == 0 && b == '!':
				s.commentSniffExec = true
				return nil
			case len(s.commentSniffBuf) == 0 && b == 'M':
				s.commentSniffBuf = append(s.commentSniffBuf, 'M')
				return nil
			case len(s.commentSniffBuf) == 1 && b == '!':
				s.commentSniffExec = true
				s.commentSniffBuf = s.commentSniffBuf[:0]
				return nil
			default:
				// Plain /* ... */ block comment; process current byte in block comment state.
				s.inCommentSniff = false
				s.commentSniffBuf = s.commentSniffBuf[:0]
				s.blockCommentDepth = 1
			}
		} else {
			if b >= '0' && b <= '9' && len(s.commentSniffBuf) < 10 {
				s.commentSniffBuf = append(s.commentSniffBuf, b)
				return nil
			}
			// MariaDB 11 dumps emit /*M!999999\- enable the sandbox mode */ or /*!999999\- ... */.
			if len(s.commentSniffBuf) > 0 && b == '\\' && hasNext && next == '-' {
				s.inCommentSniff = false
				s.commentSniffExec = false
				s.commentSniffBuf = s.commentSniffBuf[:0]
				s.blockCommentDepth = 1
				s.hasPrev = false
				return nil
			}
			// Executable comment body begins at b: unwrap and continue normal scanning.
			s.inCommentSniff = false
			s.commentSniffExec = false
			s.commentSniffBuf = s.commentSniffBuf[:0]
			s.inExecComment = true
		}
	}

	// Block comment /* ... */.
	if s.blockCommentDepth > 0 {
		if b == '\n' {
			s.lineNo++
		}
		if s.isPG() && b == '/' && hasNext && next == '*' {
			s.blockCommentDepth++
			s.hasPrev = false
			return nil
		}
		if b == '*' && hasNext && next == '/' {
			s.blockCommentDepth--
			s.hasPrev = false
			return nil
		}
		return nil
	}

	// Single-quoted string literal '...'.
	if s.inSingleQuote {
		if b == '\n' {
			s.lineNo++
		}
		if s.singleQuoteEscape && b == '\\' && hasNext {
			// Skip escaped next byte.
			if next == '\n' {
				s.lineNo++
			}
			s.hasPrev = false
			return nil
		}
		if b == '\'' {
			if hasNext && next == '\'' {
				s.hasPrev = false
				return nil
			}
			s.inSingleQuote = false
			s.singleQuoteEscape = false
		}
		return nil
	}

	// PostgreSQL dollar-quoted string $tag$...$tag$ (tagged, untagged, or containing nested tags).
	if s.inDollarQuote {
		if b == '\n' {
			s.lineNo++
		}
		if len(s.dollarMatch) > 0 {
			if b == '$' {
				if len(s.dollarMatch)+1 == len(s.dollarTag) && string(s.dollarMatch)+"$" == s.dollarTag {
					s.inDollarQuote = false
					s.dollarTag = ""
					s.dollarMatch = s.dollarMatch[:0]
					return nil
				}
				// Did not close s.dollarTag, but this '$' could itself start the closing tag (e.g. $$tag$).
				s.dollarMatch = append(s.dollarMatch[:0], '$')
				return nil
			}
			idx := len(s.dollarMatch)
			if idx < len(s.dollarTag)-1 && b == s.dollarTag[idx] {
				s.dollarMatch = append(s.dollarMatch, b)
			} else {
				s.dollarMatch = s.dollarMatch[:0]
			}
			return nil
		}
		if b == '$' {
			s.dollarMatch = append(s.dollarMatch[:0], '$')
		}
		return nil
	}

	// Double-quoted identifier ("..." in PG, or string/ident in MySQL).
	if s.inDoubleQuote {
		if b == '\n' {
			s.lineNo++
		}
		if !s.isPG() && b == '\\' && hasNext {
			if next == '\n' {
				s.lineNo++
			}
			s.hasPrev = false
			return nil
		}
		if b == '"' {
			if hasNext && next == '"' {
				s.hasPrev = false
				return nil
			}
			s.inDoubleQuote = false
			if s.isPG() {
				return s.flushWord()
			}
			s.curWord = s.curWord[:0]
			s.curWordQuoted = false
			return nil
		}
		if s.isPG() && len(s.curWord) < 64 {
			s.curWord = append(s.curWord, b)
		}
		return nil
	}

	// MySQL backtick-quoted identifier `...`.
	if s.inBacktick {
		if b == '\n' {
			s.lineNo++
		}
		if b == '`' {
			if hasNext && next == '`' {
				s.hasPrev = false
				return nil
			}
			s.inBacktick = false
			return s.flushWord()
		}
		if len(s.curWord) < 64 {
			s.curWord = append(s.curWord, b)
		}
		return nil
	}

	// End of an unwrapped MySQL/MariaDB executable comment (/*! ... */ or /*M! ... */).
	if s.inExecComment && b == '*' && hasNext && next == '/' {
		if err := s.flushWord(); err != nil {
			return err
		}
		s.inExecComment = false
		s.delimMatchBuf = s.delimMatchBuf[:0]
		s.hasPrev = false
		return nil
	}

	// Backslash client commands anywhere on a line outside quotes, comments, and COPY data.
	// PostgreSQL treats any '\' as a psql meta-command; MySQL/MariaDB treat '\' plus '!', '.', '?', or a letter.
	if b == '\\' && (s.isPG() || (hasNext && (next == '!' || next == '.' || next == '?' || isASCIIAlpha(next)))) {
		atStart := s.atLineStart && !s.lineTrimmed && len(s.stmtFirst) == 0 && len(s.curWord) == 0
		if err := s.flushWord(); err != nil {
			return err
		}
		s.delimMatchBuf = s.delimMatchBuf[:0]
		s.inBackslash = true
		s.backslashAtLineStart = atStart
		s.backslashBuf = append(s.backslashBuf[:0], '\\')
		s.atLineStart = false
		s.lineTrimmed = true
		return nil
	}

	// Track whether we have seen any non-whitespace on this line.
	if s.atLineStart && !s.lineTrimmed {
		if b == ' ' || b == '\t' || b == '\r' {
			return nil
		}
		if b == '\n' {
			s.onNewline()
			return nil
		}
		s.lineTrimmed = true
		s.atLineStart = false
	}

	// Start line comment (-- or MySQL #).
	if b == '-' && hasNext && next == '-' {
		if err := s.flushWord(); err != nil {
			return err
		}
		s.delimMatchBuf = s.delimMatchBuf[:0]
		s.inLineComment = true
		s.lineCommentBuf = s.lineCommentBuf[:0]
		s.hasPrev = false
		return nil
	}
	if !s.isPG() && b == '#' {
		if err := s.flushWord(); err != nil {
			return err
		}
		s.delimMatchBuf = s.delimMatchBuf[:0]
		s.inLineComment = true
		s.lineCommentBuf = s.lineCommentBuf[:0]
		return nil
	}

	// Start block comment /* ... */ (or MySQL/MariaDB /*! / /*M! executable comment).
	if b == '/' && hasNext && next == '*' {
		if err := s.flushWord(); err != nil {
			return err
		}
		s.delimMatchBuf = s.delimMatchBuf[:0]
		s.hasPrev = false
		if !s.isPG() {
			s.inCommentSniff = true
			s.commentSniffExec = false
			s.commentSniffBuf = s.commentSniffBuf[:0]
			return nil
		}
		s.blockCommentDepth = 1
		return nil
	}

	// Start single-quoted string.
	if b == '\'' {
		ePrefix := s.isPG() && len(s.curWord) == 1 && (s.curWord[0] == 'E' || s.curWord[0] == 'e')
		if !ePrefix {
			if err := s.flushWord(); err != nil {
				return err
			}
		} else {
			s.curWord = s.curWord[:0]
		}
		s.delimMatchBuf = s.delimMatchBuf[:0]
		s.inSingleQuote = true
		s.singleQuoteEscape = !s.isPG() || ePrefix
		if s.stmtLine == 0 {
			s.stmtLine = s.lineNo
		}
		return nil
	}

	// Start double-quoted identifier/string.
	if b == '"' {
		if err := s.flushWord(); err != nil {
			return err
		}
		s.delimMatchBuf = s.delimMatchBuf[:0]
		s.inDoubleQuote = true
		s.curWord = s.curWord[:0]
		s.curWordQuoted = true
		if s.stmtLine == 0 {
			s.stmtLine = s.lineNo
		}
		return nil
	}

	// Start MySQL backtick identifier.
	if !s.isPG() && b == '`' {
		if err := s.flushWord(); err != nil {
			return err
		}
		s.delimMatchBuf = s.delimMatchBuf[:0]
		s.inBacktick = true
		s.curWord = s.curWord[:0]
		s.curWordQuoted = true
		if s.stmtLine == 0 {
			s.stmtLine = s.lineNo
		}
		return nil
	}

	// PostgreSQL dollar-quoted string $tag$...$tag$.
	if s.isPG() && b == '$' && len(s.curWord) == 0 && hasNext && (next == '$' || isIdentStart(next)) {
		s.curWord = append(s.curWord[:0], '$')
		return nil
	}
	if s.isPG() && len(s.curWord) > 0 && s.curWord[0] == '$' {
		if b == '$' {
			s.dollarTag = string(append(s.curWord, '$'))
			s.curWord = s.curWord[:0]
			s.inDollarQuote = true
			s.dollarMatch = s.dollarMatch[:0]
			return nil
		}
		if isIdentChar(b) && len(s.curWord) < 64 {
			s.curWord = append(s.curWord, b)
			return nil
		}
		// Not a valid dollar tag (e.g. $1 parameter); reset.
		s.curWord = s.curWord[:0]
	}

	// Custom or standard statement delimiter.
	if s.matchDelimiter(b) {
		if err := s.flushWord(); err != nil {
			return err
		}
		return s.finishStmt()
	}

	// Newline.
	if b == '\n' {
		if err := s.flushWord(); err != nil {
			return err
		}
		if s.readingDelimiter {
			s.finishDelimiter()
		}
		s.onNewline()
		return nil
	}

	// Punctuation tokens we care about: '.', '*', '('.
	if b == '.' || b == '*' || b == '(' {
		if err := s.flushWord(); err != nil {
			return err
		}
		return s.recordToken(string(b))
	}

	// Identifier / keyword characters.
	if isIdentChar(b) {
		if len(s.curWord) == 0 && s.stmtLine == 0 {
			s.stmtLine = s.lineNo
		}
		if len(s.curWord) < 64 {
			s.curWord = append(s.curWord, b)
		}
		return nil
	}

	// Any other whitespace or punctuation finishes the current word.
	return s.flushWord()
}

func (s *sqlScanner) onNewline() {
	s.lineNo++
	s.atLineStart = true
	s.lineTrimmed = false
	s.delimMatchBuf = s.delimMatchBuf[:0]
	if s.pendingCopyData {
		s.pendingCopyData = false
		s.inCopyData = true
		s.copyLineBuf = s.copyLineBuf[:0]
	}
}

func (s *sqlScanner) finishDelimiter() {
	fields := strings.Fields(string(s.delimArgBuf))
	if len(fields) > 0 {
		s.delim = fields[0]
	}
	s.readingDelimiter = false
	s.delimArgBuf = s.delimArgBuf[:0]
	s.delimMatchBuf = s.delimMatchBuf[:0]
	_ = s.finishStmt()
}

func (s *sqlScanner) matchDelimiter(b byte) bool {
	if s.delim == ";" {
		return b == ';'
	}
	s.delimMatchBuf = append(s.delimMatchBuf, b)
	if len(s.delimMatchBuf) > len(s.delim) {
		s.delimMatchBuf = s.delimMatchBuf[len(s.delimMatchBuf)-len(s.delim):]
	}
	if string(s.delimMatchBuf) == s.delim {
		s.delimMatchBuf = s.delimMatchBuf[:0]
		return true
	}
	return false
}

func (s *sqlScanner) finishLine() error {
	if s.inLineComment {
		err := s.checkCommentHeader(string(s.lineCommentBuf))
		s.inLineComment = false
		s.lineCommentBuf = s.lineCommentBuf[:0]
		if err != nil {
			return err
		}
	}
	if s.readingDelimiter {
		s.finishDelimiter()
	}
	if s.inBackslash {
		line := strings.TrimRight(string(s.backslashBuf), "\r")
		atStart := s.backslashAtLineStart
		s.inBackslash = false
		s.backslashAtLineStart = false
		s.backslashBuf = s.backslashBuf[:0]
		return s.checkBackslashLine(line, atStart)
	}
	return nil
}

func (s *sqlScanner) checkCommentHeader(comment string) error {
	if s.isPG() {
		if strings.Contains(comment, "PostgreSQL database cluster dump") {
			return fmt.Errorf("line %d is a full cluster dump (pg_dumpall), which can't be imported into a single database. Dump only the target database with pg_dump", s.lineNo)
		}
		return nil
	}
	if strings.Contains(comment, "Current Database:") {
		return fmt.Errorf("line %d is a multi-database dump (-- Current Database), which can't be imported into a single database. Dump only the target database without --all-databases or --databases", s.lineNo)
	}
	return nil
}

func isAlnumKey(s string) bool {
	if s == "" {
		return false
	}
	for i := range len(s) {
		c := s[i]
		if !isASCIIAlpha(c) && (c < '0' || c > '9') {
			return false
		}
	}
	return true
}

func (s *sqlScanner) checkBackslashLine(line string, atLineStart bool) error {
	fields := strings.Fields(line)
	if len(fields) == 0 {
		return nil
	}
	cmd := fields[0]
	if s.isPG() {
		// Allow PostgreSQL 16.10+/17.6+ pg_dump's matching line-start \restrict / \unrestrict pair.
		if atLineStart && cmd == `\restrict` && len(fields) == 2 && isAlnumKey(fields[1]) && s.restrictKey == "" && s.lineNo <= 30 {
			s.restrictKey = fields[1]
			return nil
		}
		if atLineStart && cmd == `\unrestrict` && len(fields) == 2 && s.restrictKey != "" && fields[1] == s.restrictKey {
			s.restrictKey = ""
			return nil
		}
		if cmd == `\!` || strings.HasPrefix(cmd, `\!`) {
			return fmt.Errorf("line %d runs a shell command (\\!), which Dockwell refuses in dump imports. Remove it from the dump and upload again", s.lineNo)
		}
		if cmd == `\c` || cmd == `\connect` {
			return fmt.Errorf("line %d switches databases (%s), which Dockwell refuses in dump imports. Export only the target database and upload again", s.lineNo, cmd)
		}
		if strings.EqualFold(cmd, `\copy`) {
			return fmt.Errorf("line %d uses client-side \\copy, which Dockwell refuses in dump imports. Use SQL COPY FROM stdin or INSERT statements instead", s.lineNo)
		}
		if cmd == `\password` {
			return fmt.Errorf("line %d changes a role's password (\\password), which Dockwell refuses in dump imports. Remove it from the dump and upload again", s.lineNo)
		}
		return fmt.Errorf("line %d runs a psql meta-command (%s), which Dockwell refuses in dump imports. Remove client meta-commands and upload again", s.lineNo, cmd)
	}
	// MySQL / MariaDB backslash client commands.
	if cmd == `\!` || strings.HasPrefix(cmd, `\!`) {
		return fmt.Errorf("line %d runs a shell command (\\!), which Dockwell refuses in dump imports. Remove it from the dump and upload again", s.lineNo)
	}
	if cmd == `\.` || strings.HasPrefix(cmd, `\.`) {
		return fmt.Errorf("line %d includes an external file (\\.), which Dockwell refuses in dump imports. Inline the SQL or remove the directive and upload again", s.lineNo)
	}
	if cmd == `\u` || cmd == `\r` {
		return fmt.Errorf("line %d switches databases or connections (%s), which Dockwell refuses in dump imports. Export only the target database and upload again", s.lineNo, cmd)
	}
	return fmt.Errorf("line %d runs a client meta-command (%s), which Dockwell refuses in dump imports. Remove client meta-commands and upload again", s.lineNo, cmd)
}

func (s *sqlScanner) flushWord() error {
	if len(s.curWord) == 0 {
		return nil
	}
	w := strings.ToUpper(string(s.curWord))
	quoted := s.curWordQuoted
	s.curWord = s.curWord[:0]
	s.curWordQuoted = false

	// Check MySQL/MariaDB client commands at any statement start (line start or after a delimiter).
	if !s.isPG() && len(s.stmtFirst) == 0 && !quoted {
		switch w {
		case "SYSTEM":
			return fmt.Errorf("line %d runs a shell command (system), which Dockwell refuses in dump imports. Remove it from the dump and upload again", s.lineNo)
		case "SOURCE":
			return fmt.Errorf("line %d includes an external file (source), which Dockwell refuses in dump imports. Inline the SQL or remove the directive and upload again", s.lineNo)
		case "CONNECT":
			return fmt.Errorf("line %d switches databases or connections (connect), which Dockwell refuses in dump imports. Export only the target database and upload again", s.lineNo)
		case "TEE":
			return fmt.Errorf("line %d writes client output to a file (tee), which Dockwell refuses in dump imports. Remove client commands and upload again", s.lineNo)
		case "DELIMITER":
			s.readingDelimiter = true
			s.delimArgBuf = s.delimArgBuf[:0]
			s.delimMatchBuf = s.delimMatchBuf[:0]
			return nil
		}
	}
	return s.recordToken(w)
}

func (s *sqlScanner) recordToken(tok string) error {
	if s.stmtLine == 0 {
		s.stmtLine = s.lineNo
	}
	if len(s.stmtFirst) < 8 {
		s.stmtFirst = append(s.stmtFirst, tok)
	}
	p2, p1 := s.stmtPrev2, s.stmtPrev1
	s.stmtPrev2, s.stmtPrev1 = p1, tok

	head := s.stmtFirst[0]
	if s.isPG() {
		if head == "COPY" && (p1 == "TO" || p1 == "FROM") && tok == "PROGRAM" {
			return fmt.Errorf("line %d uses COPY %s PROGRAM, which Dockwell refuses in dump imports. Remove server-side program commands and upload again", s.stmtLine, p1)
		}
		if head == "COPY" && p1 == "FROM" && tok == "STDIN" {
			s.pendingCopyData = true
		}
		if len(s.stmtFirst) == 2 {
			if (head == "CREATE" || head == "ALTER" || head == "DROP") && (tok == "ROLE" || tok == "USER" || tok == "GROUP") {
				return fmt.Errorf("line %d modifies database roles (%s %s), which Dockwell refuses in dump imports. Remove role statements and upload again", s.stmtLine, head, tok)
			}
			if (head == "CREATE" || head == "ALTER" || head == "DROP") && tok == "DATABASE" {
				return fmt.Errorf("line %d modifies a database (%s DATABASE), which Dockwell refuses in dump imports. Export only the target database and upload again", s.stmtLine, head)
			}
		}
		return nil
	}

	// MySQL / MariaDB statement checks.
	if len(s.stmtFirst) == 1 && head == "USE" {
		return fmt.Errorf("line %d switches databases (USE), which Dockwell refuses in dump imports. Export only the target database without --databases or USE statements", s.stmtLine)
	}
	if len(s.stmtFirst) == 2 {
		if (head == "CREATE" || head == "ALTER" || head == "DROP") && (tok == "DATABASE" || tok == "SCHEMA") {
			return fmt.Errorf("line %d modifies a database (%s %s), which Dockwell refuses in dump imports. Export only the target database and upload again", s.stmtLine, head, tok)
		}
		if (head == "CREATE" || head == "ALTER" || head == "DROP" || head == "RENAME") && (tok == "USER" || tok == "ROLE") {
			return fmt.Errorf("line %d modifies database users or roles (%s %s), which Dockwell refuses in dump imports. Remove user and role statements and upload again", s.stmtLine, head, tok)
		}
		if head == "LOAD" && tok == "DATA" {
			return fmt.Errorf("line %d uses LOAD DATA, which Dockwell refuses in dump imports. Export the dump with INSERT statements instead", s.stmtLine)
		}
	}
	if head == "GRANT" || head == "REVOKE" {
		return fmt.Errorf("line %d modifies database privileges (%s), which Dockwell refuses in dump imports. Remove privilege statements and upload again", s.stmtLine, head)
	}
	if p1 == "INTO" && (tok == "OUTFILE" || tok == "DUMPFILE") {
		return fmt.Errorf("line %d writes server files (INTO %s), which Dockwell refuses in dump imports. Remove file export clauses and upload again", s.stmtLine, tok)
	}
	if p1 == "LOAD_FILE" && tok == "(" {
		return fmt.Errorf("line %d reads server files (LOAD_FILE), which Dockwell refuses in dump imports. Remove LOAD_FILE calls and upload again", s.stmtLine)
	}
	// Refuse DML/DDL targeting MySQL's system schema (mysql.user, mysql.db, etc.).
	if p2 == "MYSQL" && p1 == "." && isIdentToken(tok) &&
		(head == "INSERT" || head == "UPDATE" || head == "DELETE" || head == "REPLACE" || head == "DROP" || head == "ALTER" || head == "TRUNCATE" || head == "CREATE") {
		return fmt.Errorf("line %d modifies the mysql system schema (mysql.%s), which Dockwell refuses in dump imports. Export only the target database and upload again", s.stmtLine, strings.ToLower(tok))
	}
	return nil
}

func (s *sqlScanner) finishStmt() error {
	s.stmtLine = 0
	s.stmtFirst = s.stmtFirst[:0]
	s.stmtPrev2 = ""
	s.stmtPrev1 = ""
	return nil
}

func isASCIIAlpha(b byte) bool {
	return (b >= 'a' && b <= 'z') || (b >= 'A' && b <= 'Z')
}

func isIdentStart(b byte) bool {
	return isASCIIAlpha(b) || b == '_'
}

func isIdentChar(b byte) bool {
	return isASCIIAlpha(b) || (b >= '0' && b <= '9') || b == '_'
}

func isIdentToken(s string) bool {
	return s != "" && isIdentStart(s[0])
}

// mysqlNearSQL matches MySQL/MariaDB's echoed SQL snippet in syntax errors,
// which can contain nested quotes from the dump itself.
var mysqlNearSQL = lazyre.Regexp(`near\s+'.*'(?:\s+(at\s+line\s+\d+))?`)

// importErrorLine matches tool error lines from psql, pg_restore, mysql, and mariadb.
var importErrorLine = lazyre.Regexp(`^(?:(?:ERROR|FATAL|PANIC):|ERROR\s+\d+\b|psql:.*(?:ERROR|FATAL|error):|pg_restore:\s*(?:error|fatal):|(?:mysql|mariadb):.*(?:ERROR|error))`)

// sanitizeImportError reduces a failed import's tool stderr to only the tool's
// own error messages, dropping echoed SQL input lines (LINE n:, DETAIL:,
// CONTEXT:, STATEMENT:, QUERY:, caret pointers), stripping MySQL's echoed
// "near '...'" SQL snippet, replacing quoted literals and key values, and
// applying collect.Redact as a final backstop so dump data and passwords never
// reach Activity or logs.
func sanitizeImportError(raw string) string {
	raw = strings.TrimSpace(raw)
	if raw == "" {
		return ""
	}
	var kept []string
	for line := range strings.SplitSeq(raw, "\n") {
		line = strings.TrimRight(line, "\r")
		trimmed := strings.TrimSpace(line)
		if trimmed == "" {
			continue
		}
		if strings.HasPrefix(trimmed, "LINE ") ||
			strings.HasPrefix(trimmed, "DETAIL:") ||
			strings.HasPrefix(trimmed, "CONTEXT:") ||
			strings.HasPrefix(trimmed, "STATEMENT:") ||
			strings.HasPrefix(trimmed, "QUERY:") ||
			strings.Trim(trimmed, " \t^") == "" {
			continue
		}
		if !importErrorLine().MatchString(trimmed) {
			continue
		}
		cleaned := mysqlNearSQL().ReplaceAllStringFunc(trimmed, func(m string) string {
			sub := mysqlNearSQL().FindStringSubmatch(m)
			if len(sub) > 1 && sub[1] != "" {
				return "near '...' " + sub[1]
			}
			return "near '...'"
		})
		cleaned = withoutValues(cleaned)
		cleaned = collect.Redact([]string{cleaned})[0]
		kept = append(kept, cleaned)
		if len(kept) >= importTailLines {
			break
		}
	}
	if len(kept) == 0 {
		return "the database tool stopped with an error (details withheld to keep dump data private)"
	}
	joined := strings.Join(kept, "\n")
	if len(joined) > importTailBytes {
		joined = joined[:importTailBytes]
	}
	return joined
}
