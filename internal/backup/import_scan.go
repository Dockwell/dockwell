// SPDX-License-Identifier: AGPL-3.0-only

package backup

import (
	"bytes"
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
	if engine == dockwellv1.DatabaseEngine_DATABASE_ENGINE_MYSQL || engine == dockwellv1.DatabaseEngine_DATABASE_ENGINE_MARIADB {
		r = newMySQLVersionUnwrapper(r)
	}
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
	lineBuf     []byte // buffered prefix of current line (for headers / client commands)
	lineTrimmed bool   // saw first non-whitespace on current line

	// Lexer states.
	inLineComment     bool
	lineCommentBuf    []byte
	blockCommentDepth int
	inSingleQuote     bool
	singleQuoteEscape bool // backslash escape active inside '...'
	inDoubleQuote     bool
	inBacktick        bool
	inDollarQuote     bool
	dollarTag         string
	dollarMatch       []byte

	// Pending 2-char lookahead / token buffers.
	prevByte byte
	hasPrev  bool

	// PostgreSQL COPY ... FROM stdin state.
	pendingCopyData bool
	inCopyData      bool
	copyLineBuf     []byte
	restrictKey     string

	// MySQL custom DELIMITER support.
	delim          string
	delimMatchBuf  []byte
	skipRestOfLine bool

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
	// Handle skipping rest of a DELIMITER or validated \restrict line.
	if s.skipRestOfLine {
		if b == '\n' {
			s.skipRestOfLine = false
			s.lineNo++
			s.atLineStart = true
			s.lineBuf = s.lineBuf[:0]
			s.lineTrimmed = false
		}
		return nil
	}

	// Inside PostgreSQL COPY ... FROM stdin data block: only "\." on its own line ends it.
	if s.inCopyData {
		if b == '\n' {
			line := strings.TrimRight(string(s.copyLineBuf), "\r")
			s.copyLineBuf = s.copyLineBuf[:0]
			s.lineNo++
			if line == `\.` {
				s.inCopyData = false
				s.atLineStart = true
				s.lineBuf = s.lineBuf[:0]
				s.lineTrimmed = false
			}
			return nil
		}
		if len(s.copyLineBuf) < 8 {
			s.copyLineBuf = append(s.copyLineBuf, b)
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

	// PostgreSQL dollar-quoted string $tag$...$tag$.
	if s.inDollarQuote {
		if b == '\n' {
			s.lineNo++
		}
		if b == '$' {
			s.dollarMatch = append(s.dollarMatch[:0], '$')
		} else if len(s.dollarMatch) > 0 {
			s.dollarMatch = append(s.dollarMatch, b)
		}
		if len(s.dollarMatch) > 0 && len(s.dollarMatch) == len(s.dollarTag) && string(s.dollarMatch) == s.dollarTag {
			s.inDollarQuote = false
			s.dollarTag = ""
			s.dollarMatch = s.dollarMatch[:0]
		} else if len(s.dollarMatch) >= len(s.dollarTag) {
			s.dollarMatch = s.dollarMatch[:0]
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

	// Track leading line text for client commands (\!, \connect, DELIMITER, etc.).
	if s.atLineStart {
		if !s.lineTrimmed {
			if b == ' ' || b == '\t' || b == '\r' {
				return nil
			}
			if b == '\n' {
				s.onNewline()
				return nil
			}
			s.lineTrimmed = true
		}
		// Buffer backslash commands until end of line so we can inspect them whole.
		if len(s.lineBuf) == 0 && b == '\\' {
			s.lineBuf = append(s.lineBuf, b)
			return nil
		}
		if len(s.lineBuf) > 0 && s.lineBuf[0] == '\\' {
			if b == '\n' {
				line := strings.TrimRight(string(s.lineBuf), "\r")
				err := s.checkBackslashLine(line)
				s.onNewline()
				return err
			}
			if len(s.lineBuf) < 256 {
				s.lineBuf = append(s.lineBuf, b)
			}
			return nil
		}
		// MySQL line-start client commands (system, source, connect, delimiter) at statement boundary.
		if !s.isPG() && len(s.stmtFirst) == 0 && (len(s.lineBuf) > 0 || len(s.curWord) == 0) {
			switch {
			case len(s.lineBuf) == 0 && isASCIIAlpha(b):
				s.lineBuf = append(s.lineBuf, b)
				// Also fall through to normal tokenization unless we detect a client command.
			case len(s.lineBuf) > 0:
				switch {
				case isASCIIAlpha(b) && len(s.lineBuf) < 16:
					s.lineBuf = append(s.lineBuf, b)
				case b == ' ' || b == '\t' || b == '\r' || b == '\n':
					word := strings.ToLower(string(s.lineBuf))
					switch word {
					case "system":
						return fmt.Errorf("line %d runs a shell command (system), which Dockwell refuses in dump imports. Remove it from the dump and upload again", s.lineNo)
					case "source":
						return fmt.Errorf("line %d includes an external file (source), which Dockwell refuses in dump imports. Inline the SQL or remove the directive and upload again", s.lineNo)
					case "connect":
						return fmt.Errorf("line %d switches databases or connections (connect), which Dockwell refuses in dump imports. Export only the target database and upload again", s.lineNo)
					case "delimiter":
						s.curWord = s.curWord[:0]
						s.lineBuf = s.lineBuf[:0]
						if b != '\n' {
							s.lineBuf = append(s.lineBuf, 'D') // sentinel: reading delimiter arg
						} else {
							s.onNewline()
						}
						return nil
					}
					s.atLineStart = false
					s.lineBuf = s.lineBuf[:0]
				default:
					s.atLineStart = false
					s.lineBuf = s.lineBuf[:0]
				}
			default:
				s.atLineStart = false
			}
		} else {
			s.atLineStart = false
		}
	}

	// Reading MySQL DELIMITER argument until newline.
	if !s.isPG() && len(s.lineBuf) > 0 && s.lineBuf[0] == 'D' && s.atLineStart {
		if b == '\n' {
			arg := strings.TrimSpace(string(s.lineBuf[1:]))
			if arg != "" {
				s.delim = arg
			}
			s.onNewline()
			return nil
		}
		if len(s.lineBuf) < 32 {
			s.lineBuf = append(s.lineBuf, b)
		}
		return nil
	}

	// Start line comment (-- or MySQL #).
	if b == '-' && hasNext && next == '-' {
		if err := s.flushWord(); err != nil {
			return err
		}
		s.inLineComment = true
		s.lineCommentBuf = s.lineCommentBuf[:0]
		s.hasPrev = false
		return nil
	}
	if !s.isPG() && b == '#' {
		if err := s.flushWord(); err != nil {
			return err
		}
		s.inLineComment = true
		s.lineCommentBuf = s.lineCommentBuf[:0]
		return nil
	}

	// Start block comment /* ... */.
	if b == '/' && hasNext && next == '*' {
		if err := s.flushWord(); err != nil {
			return err
		}
		s.hasPrev = false
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
		if isIdentChar(b) && len(s.curWord) < 32 {
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
	s.lineBuf = s.lineBuf[:0]
	s.lineTrimmed = false
	if s.pendingCopyData {
		s.pendingCopyData = false
		s.inCopyData = true
		s.copyLineBuf = s.copyLineBuf[:0]
	}
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
	if s.atLineStart && len(s.lineBuf) > 0 && s.lineBuf[0] == '\\' {
		line := strings.TrimRight(string(s.lineBuf), "\r")
		s.lineBuf = s.lineBuf[:0]
		return s.checkBackslashLine(line)
	}
	if !s.isPG() && s.atLineStart && len(s.lineBuf) > 0 && len(s.stmtFirst) == 0 {
		word := strings.ToLower(string(s.lineBuf))
		switch word {
		case "system":
			return fmt.Errorf("line %d runs a shell command (system), which Dockwell refuses in dump imports. Remove it from the dump and upload again", s.lineNo)
		case "source":
			return fmt.Errorf("line %d includes an external file (source), which Dockwell refuses in dump imports. Inline the SQL or remove the directive and upload again", s.lineNo)
		case "connect":
			return fmt.Errorf("line %d switches databases or connections (connect), which Dockwell refuses in dump imports. Export only the target database and upload again", s.lineNo)
		}
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

func (s *sqlScanner) checkBackslashLine(line string) error {
	fields := strings.Fields(line)
	if len(fields) == 0 {
		return nil
	}
	cmd := fields[0]
	if s.isPG() {
		// Allow PostgreSQL 16.10+/17.6+ pg_dump's matching \restrict / \unrestrict pair.
		if cmd == `\restrict` && len(fields) == 2 && s.restrictKey == "" && s.lineNo <= 30 {
			s.restrictKey = fields[1]
			return nil
		}
		if cmd == `\unrestrict` && len(fields) == 2 && s.restrictKey != "" && fields[1] == s.restrictKey {
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
	s.curWord = s.curWord[:0]
	s.curWordQuoted = false
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

// unwrapMySQLVersionedComments strips /*!NNNNN ... */ wrappers into plain SQL
// while ignoring MariaDB's own sandbox pragma /*!999999\- ... */.
func unwrapMySQLVersionedComments(in []byte) []byte {
	if !bytes.Contains(in, []byte("/*!")) {
		return in
	}
	var out bytes.Buffer
	out.Grow(len(in))
	i := 0
	for i < len(in) {
		idx := bytes.Index(in[i:], []byte("/*!"))
		if idx < 0 {
			out.Write(in[i:])
			break
		}
		out.Write(in[i : i+idx])
		i += idx + 3
		// Skip optional 5-6 digit version number.
		vStart := i
		for i < len(in) && in[i] >= '0' && in[i] <= '9' {
			i++
		}
		// MariaDB 11 dumps emit /*!999999\- enable the sandbox mode */.
		if i > vStart && i+1 < len(in) && in[i] == '\\' && in[i+1] == '-' {
			end := bytes.Index(in[i:], []byte("*/"))
			if end < 0 {
				break
			}
			i += end + 2
			continue
		}
		end := bytes.Index(in[i:], []byte("*/"))
		if end < 0 {
			out.Write(in[i:])
			break
		}
		out.WriteByte(' ')
		out.Write(in[i : i+end])
		out.WriteByte(' ')
		i += end + 2
	}
	return out.Bytes()
}
