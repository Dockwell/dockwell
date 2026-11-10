// SPDX-License-Identifier: AGPL-3.0-only

package backup

import (
	"bytes"
	"strings"
)

// maxTables is how many of a dump's tables a drill keeps in mind, which bounds what
// it holds of a dump with a great many. Past them, it checks those it has and says so.
const maxTables = 100_000

// lineHead is how much of a line the reader keeps: enough to tell what a statement
// makes, and never a row's data.
const lineHead = 1 << 10

// dumpTable is a table a dump makes, in the database it's in.
type dumpTable struct {
	database string
	// schema is PostgreSQL's, such as public; MySQL has none.
	schema, name string
	filled       bool
}

// heldDump is what a dump holds, as a drill reads it on its way into the load (ADR
// 0068): the databases and tables it makes, and which tables it fills. A drill
// checks those against what loaded, so what today's image brings of its own counts
// for nothing, and what a dump never made isn't asked for.
type heldDump struct {
	mysql bool
	// databases in the order the dump makes or names them.
	databases []string
	tables    []*dumpTable
	byName    map[string]*dumpTable
	// over says it held more tables than maxTables.
	over bool

	current string
	copying *dumpTable // a COPY under way, which a row fills
	line    []byte
	long    bool // the line under way is past lineHead
}

func newHeldDump(mysql bool) *heldDump {
	return &heldDump{mysql: mysql, byName: map[string]*dumpTable{}}
}

// Write reads the dump a line at a time, keeping only the start of each.
func (h *heldDump) Write(b []byte) (int, error) {
	n := len(b)
	for len(b) > 0 {
		i := bytes.IndexByte(b, '\n')
		if i < 0 {
			h.keep(b)
			break
		}
		h.keep(b[:i])
		h.end()
		b = b[i+1:]
	}
	return n, nil
}

func (h *heldDump) keep(b []byte) {
	if room := lineHead - len(h.line); room > 0 {
		h.line = append(h.line, b[:min(len(b), room)]...)
	}
	if len(h.line) >= lineHead {
		h.long = true
	}
}

// end reads a whole line.
func (h *heldDump) end() {
	line := strings.TrimRight(string(h.line), "\r")
	h.line, h.long = h.line[:0], false
	if h.copying != nil {
		if line == `\.` {
			h.copying = nil
		} else {
			h.copying.filled = true
		}
		return
	}
	if h.mysql {
		h.mysqlLine(line)
	} else {
		h.postgresLine(line)
	}
}

// Close reads a last line that had no newline.
func (h *heldDump) Close() error {
	if len(h.line) > 0 {
		h.end()
	}
	return nil
}

func (h *heldDump) postgresLine(line string) {
	switch {
	case strings.HasPrefix(line, `\connect `):
		h.use(postgresConnect(strings.TrimPrefix(line, `\connect `)))
	case strings.HasPrefix(line, "CREATE DATABASE "):
		name, _ := postgresName(strings.TrimPrefix(line, "CREATE DATABASE "))
		h.database(name)
	case strings.HasPrefix(line, "CREATE TABLE "), strings.HasPrefix(line, "CREATE UNLOGGED TABLE "):
		rest := strings.TrimPrefix(strings.TrimPrefix(line, "CREATE UNLOGGED TABLE "), "CREATE TABLE ")
		if schema, name, ok := postgresQualified(rest); ok {
			h.table(schema, name)
		}
	case strings.HasPrefix(line, "COPY ") && strings.HasSuffix(line, "FROM stdin;"):
		if schema, name, ok := postgresQualified(strings.TrimPrefix(line, "COPY ")); ok {
			h.copying = h.table(schema, name)
		}
	case strings.HasPrefix(line, "INSERT INTO "):
		if schema, name, ok := postgresQualified(strings.TrimPrefix(line, "INSERT INTO ")); ok {
			if t := h.table(schema, name); t != nil {
				t.filled = true
			}
		}
	}
}

func (h *heldDump) mysqlLine(line string) {
	switch {
	case strings.HasPrefix(line, "USE "):
		name, _ := mysqlName(strings.TrimPrefix(line, "USE "))
		h.use(name)
	case strings.HasPrefix(line, "CREATE DATABASE "):
		rest := strings.TrimPrefix(line, "CREATE DATABASE ")
		if i := strings.Index(rest, "*/"); strings.HasPrefix(rest, "/*!") && i >= 0 {
			rest = strings.TrimSpace(rest[i+2:])
		}
		name, _ := mysqlName(rest)
		h.database(name)
	case strings.HasPrefix(line, "CREATE TABLE "):
		if name, ok := mysqlName(strings.TrimPrefix(line, "CREATE TABLE ")); ok {
			h.table("", name)
		}
	case strings.HasPrefix(line, "INSERT INTO "):
		if name, ok := mysqlName(strings.TrimPrefix(line, "INSERT INTO ")); ok {
			if t := h.table("", name); t != nil {
				t.filled = true
			}
		}
	}
}

func (h *heldDump) use(name string) {
	if name == "" {
		return
	}
	h.current = name
	h.database(name)
}

func (h *heldDump) database(name string) {
	if name != "" && !contains(h.databases, name) {
		h.databases = append(h.databases, name)
	}
}

// table is a table of the database under way, made the first time it's named.
func (h *heldDump) table(schema, name string) *dumpTable {
	if h.current == "" || name == "" {
		return nil
	}
	key := h.current + "\x00" + schema + "\x00" + name
	if t := h.byName[key]; t != nil {
		return t
	}
	if len(h.tables) >= maxTables {
		h.over = true
		return nil
	}
	t := &dumpTable{database: h.current, schema: schema, name: name}
	h.byName[key] = t
	h.tables = append(h.tables, t)
	return t
}

func contains(list []string, s string) bool {
	for _, x := range list {
		if x == s {
			return true
		}
	}
	return false
}

// postgresConnect reads the database a \connect names: \connect orders,
// \connect "my db", or \connect -reuse-previous=on "dbname='orders'".
func postgresConnect(rest string) string {
	rest = strings.TrimSpace(strings.TrimPrefix(strings.TrimSpace(rest), "-reuse-previous=on"))
	if i := strings.Index(rest, "dbname='"); i >= 0 {
		s := rest[i+len("dbname='"):]
		var b strings.Builder
		for j := 0; j < len(s); j++ {
			switch {
			case s[j] == '\\' && j+1 < len(s):
				j++
				b.WriteByte(s[j])
			case s[j] == '\'':
				return b.String()
			default:
				b.WriteByte(s[j])
			}
		}
		return ""
	}
	name, _ := postgresName(rest)
	return name
}

// postgresName reads one identifier as PostgreSQL writes it: plain, or in double
// quotes with a doubled quote for one, and says what's after it.
func postgresName(s string) (name, rest string) {
	s = strings.TrimSpace(s)
	if !strings.HasPrefix(s, `"`) {
		end := strings.IndexAny(s, " .(;\t")
		if end < 0 {
			return s, ""
		}
		return s[:end], s[end:]
	}
	var b strings.Builder
	for i := 1; i < len(s); i++ {
		if s[i] != '"' {
			b.WriteByte(s[i])
			continue
		}
		if i+1 < len(s) && s[i+1] == '"' {
			b.WriteByte('"')
			i++
			continue
		}
		return b.String(), s[i+1:]
	}
	return "", ""
}

// postgresQualified reads schema.table, as pg_dump writes a table's name.
func postgresQualified(s string) (schema, name string, ok bool) {
	schema, rest := postgresName(s)
	if schema == "" || !strings.HasPrefix(rest, ".") {
		return "", "", false
	}
	name, _ = postgresName(rest[1:])
	return schema, name, name != ""
}

// mysqlName reads one identifier as mysqldump writes it, in backticks with a doubled
// backtick for one.
func mysqlName(s string) (string, bool) {
	s = strings.TrimSpace(s)
	if !strings.HasPrefix(s, "`") {
		return "", false
	}
	var b strings.Builder
	for i := 1; i < len(s); i++ {
		if s[i] != '`' {
			b.WriteByte(s[i])
			continue
		}
		if i+1 < len(s) && s[i+1] == '`' {
			b.WriteByte('`')
			i++
			continue
		}
		return b.String(), true
	}
	return "", false
}

// quotePostgres writes an identifier for a query.
func quotePostgres(name string) string {
	return `"` + strings.ReplaceAll(name, `"`, `""`) + `"`
}

// quoteMySQL writes an identifier for a query.
func quoteMySQL(name string) string {
	return "`" + strings.ReplaceAll(name, "`", "``") + "`"
}
