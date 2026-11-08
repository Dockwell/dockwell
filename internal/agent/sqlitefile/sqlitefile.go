// SPDX-License-Identifier: AGPL-3.0-only

// Package sqlitefile reads the tables of a SQLite database, read-only, from its
// file and the committed frames of its write-ahead log, without SQLite. The agent
// build links no SQLite (ADR 0023), and what it reads this way is another program's
// records, such as 1Panel's (ADR 0056), which it must never change or lock. So it
// opens both files read-only, writes nothing, and takes no lock: it reads the
// pages as they stand, and Changed says whether they moved underneath it, for its
// caller to read again.
//
// It reads a table whole, by its rowid B-tree, and knows nothing of indexes,
// queries or types beyond what a record holds. A page it can't read is an error,
// never a guess.
package sqlitefile

import (
	"bytes"
	"encoding/binary"
	"errors"
	"fmt"
	"io"
	"math"
	"os"
	"strconv"
	"strings"
	"unicode"

	"golang.org/x/sys/unix"
)

// maxDepth is how deep a table's B-tree may go. A real one is a few levels; a
// loop in a damaged file would otherwise never end.
const maxDepth = 32

// maxPayload is the most one row may hold, 64 MiB, so a damaged length can't
// make it allocate without end.
const maxPayload = 64 << 20

// ErrWriting is a database in the rollback journal's mode that's part way through
// a write, which its journal holds. Read it again once the write ends.
var ErrWriting = errors.New("it's part way through a write, which its journal holds")

// MaxRows is the most rows a scan reads of one table, so a damaged or crafted file
// can't have it read without end.
const MaxRows = 1_000_000

// journalMagic starts a rollback journal that holds a write under way. A journal
// kept between writes has it zeroed.
var journalMagic = []byte{0xd9, 0xd5, 0x05, 0xf9, 0x20, 0xa1, 0x63, 0xd7}

// DB is a SQLite database, as its files stood when it was opened.
type DB struct {
	file     *os.File
	wal      *os.File
	walPath  string
	pageSize int
	usable   int
	// frames maps a page to where its newest committed copy starts in the WAL.
	frames map[uint32]int64
	// stamp is what Changed compares: the header's change counter, and the
	// files' sizes and the WAL's salts.
	stamp stamp
	// rollback is a database in the rollback journal's mode, not the WAL's, and
	// journalPath where its journal is.
	rollback    bool
	journalPath string
	tables      map[string]*Table
}

type stamp struct {
	counter       uint32
	size, walSize int64
	salt          [8]byte
}

// Table is a table: where its B-tree starts, and its columns in order.
type Table struct {
	Name    string
	root    uint32
	Columns []string
	// rowid is the column that's the rowid itself (INTEGER PRIMARY KEY), or -1.
	rowid int
}

// Open opens the database at path, with path+"-wal" if there is one, read-only.
func Open(path string) (*DB, error) {
	file, err := openRead(path)
	if err != nil {
		return nil, err
	}
	db := &DB{file: file}
	if err := db.load(path); err != nil {
		_ = db.Close()
		return nil, err
	}
	return db, nil
}

func openRead(path string) (*os.File, error) {
	fd, err := unix.Open(path, unix.O_RDONLY|unix.O_NOFOLLOW|unix.O_CLOEXEC|unix.O_NONBLOCK, 0)
	if err != nil {
		return nil, &os.PathError{Op: "open", Path: path, Err: err}
	}
	f := os.NewFile(uintptr(fd), path)
	st, err := f.Stat()
	if err != nil {
		_ = f.Close()
		return nil, err
	}
	if !st.Mode().IsRegular() {
		_ = f.Close()
		return nil, fmt.Errorf("%s isn't a file", path)
	}
	return f, nil
}

func (db *DB) load(path string) error {
	var header [100]byte
	if _, err := db.file.ReadAt(header[:], 0); err != nil {
		return fmt.Errorf("%s is too short to be a SQLite database: %w", path, err)
	}
	if string(header[:16]) != "SQLite format 3\x00" {
		return fmt.Errorf("%s isn't a SQLite database", path)
	}
	size := int(binary.BigEndian.Uint16(header[16:18]))
	if size == 1 {
		size = 65536
	}
	if size < 512 || size > 65536 || size&(size-1) != 0 {
		return fmt.Errorf("%s says its pages are %d bytes, which SQLite never writes", path, size)
	}
	if enc := binary.BigEndian.Uint32(header[56:60]); enc != 1 && enc != 0 {
		return fmt.Errorf("%s keeps its text in UTF-16, which this reads only as UTF-8", path)
	}
	db.pageSize = size
	db.usable = size - int(header[20])
	if db.usable < 480 {
		return fmt.Errorf("%s keeps too much of each page for itself to be read", path)
	}
	db.stamp.counter = binary.BigEndian.Uint32(header[24:28])
	// 1 is the rollback journal, which writes pages in place, and 2 the WAL.
	db.rollback = header[18] == 1
	db.journalPath = path + "-journal"
	if db.rollback && db.journalHot() {
		return fmt.Errorf("%s: %w", path, ErrWriting)
	}
	if st, err := db.file.Stat(); err == nil {
		db.stamp.size = st.Size()
	}
	if err := db.loadWAL(path + "-wal"); err != nil {
		return err
	}
	return db.loadSchema()
}

// loadWAL finds, for each page, its newest copy in a frame the WAL has committed:
// frames that carry the WAL's salts, with a checksum that follows from every frame
// before them, up to the last that ends a transaction.
func (db *DB) loadWAL(path string) error {
	db.walPath = path
	wal, err := openRead(path)
	if errors.Is(err, unix.ENOENT) {
		return nil
	}
	if err != nil {
		return err
	}
	db.wal = wal
	var header [32]byte
	if _, err := wal.ReadAt(header[:], 0); err != nil {
		return nil //nolint:nilerr // an empty or partly written WAL commits nothing yet
	}
	var order binary.ByteOrder
	switch binary.BigEndian.Uint32(header[0:4]) {
	case 0x377f0682:
		order = binary.LittleEndian
	case 0x377f0683:
		order = binary.BigEndian
	default:
		return nil
	}
	if int(binary.BigEndian.Uint32(header[8:12])) != db.pageSize {
		return nil
	}
	s1, s2 := checksum(order, header[:24], 0, 0)
	if s1 != binary.BigEndian.Uint32(header[24:28]) || s2 != binary.BigEndian.Uint32(header[28:32]) {
		return nil
	}
	copy(db.stamp.salt[:], header[16:24])
	if st, err := wal.Stat(); err == nil {
		db.stamp.walSize = st.Size()
	}
	committed := map[uint32]int64{}
	pending := map[uint32]int64{}
	frame := make([]byte, 24+db.pageSize)
	for at := int64(32); ; at += int64(len(frame)) {
		// A frame cut short, as one being written, ends what's committed.
		if n, _ := wal.ReadAt(frame, at); n < len(frame) {
			break
		}
		if [8]byte(frame[8:16]) != db.stamp.salt {
			break
		}
		s1, s2 = checksum(order, frame[:8], s1, s2)
		s1, s2 = checksum(order, frame[24:], s1, s2)
		if s1 != binary.BigEndian.Uint32(frame[16:20]) || s2 != binary.BigEndian.Uint32(frame[20:24]) {
			break
		}
		pending[binary.BigEndian.Uint32(frame[0:4])] = at + 24
		if binary.BigEndian.Uint32(frame[4:8]) != 0 {
			for page, off := range pending {
				committed[page] = off
			}
			clear(pending)
		}
	}
	db.frames = committed
	return nil
}

// checksum is the WAL's running checksum over data, in the byte order the WAL's
// magic says, from s1 and s2.
func checksum(order binary.ByteOrder, data []byte, s1, s2 uint32) (uint32, uint32) {
	for i := 0; i+8 <= len(data); i += 8 {
		s1 += order.Uint32(data[i:]) + s2
		s2 += order.Uint32(data[i+4:]) + s1
	}
	return s1, s2
}

// Close closes the files.
func (db *DB) Close() error {
	var errs []error
	if db.wal != nil {
		errs = append(errs, db.wal.Close())
	}
	if db.file != nil {
		errs = append(errs, db.file.Close())
	}
	return errors.Join(errs...)
}

// Changed reports whether the files changed since the database was opened, so
// what was read may mix two moments: the change counter, the sizes, or the WAL's
// salts moved.
func (db *DB) Changed() bool {
	// In rollback mode a write changes pages in place, with its journal open.
	if db.rollback && db.journalHot() {
		return true
	}
	var counter [4]byte
	if _, err := db.file.ReadAt(counter[:], 24); err != nil || binary.BigEndian.Uint32(counter[:]) != db.stamp.counter {
		return true
	}
	if st, err := db.file.Stat(); err != nil || st.Size() != db.stamp.size {
		return true
	}
	if db.wal == nil {
		// One that appeared since may hold what was written since.
		st, err := os.Lstat(db.walPath)
		return err == nil && st.Size() > 0
	}
	var salt [8]byte
	if _, err := db.wal.ReadAt(salt[:], 16); err != nil && db.stamp.walSize > 0 {
		return true
	}
	st, err := db.wal.Stat()
	return err != nil || st.Size() != db.stamp.walSize || salt != db.stamp.salt
}

// journalHot reports a rollback journal that holds a write under way.
func (db *DB) journalHot() bool {
	f, err := openRead(db.journalPath)
	if err != nil {
		return false
	}
	defer f.Close()
	head := make([]byte, len(journalMagic))
	n, _ := f.ReadAt(head, 0)
	return n == len(head) && bytes.Equal(head, journalMagic)
}

// page reads page n, from the WAL where it has a newer copy.
func (db *DB) page(n uint32, buf []byte) ([]byte, error) {
	if n == 0 {
		return nil, errors.New("a page points at page 0, which doesn't exist")
	}
	buf = buf[:db.pageSize]
	if off, ok := db.frames[n]; ok {
		if _, err := db.wal.ReadAt(buf, off); err != nil {
			return nil, fmt.Errorf("can't read page %d from the WAL: %w", n, err)
		}
		return buf, nil
	}
	if _, err := db.file.ReadAt(buf, int64(n-1)*int64(db.pageSize)); err != nil {
		if errors.Is(err, io.EOF) {
			return nil, fmt.Errorf("page %d is past the end of the file", n)
		}
		return nil, fmt.Errorf("can't read page %d: %w", n, err)
	}
	return buf, nil
}

// Tables are the database's tables, by name.
func (db *DB) Tables() map[string]*Table { return db.tables }

// Table is the table called name, if there is one.
func (db *DB) Table(name string) (*Table, bool) {
	t, ok := db.tables[name]
	return t, ok
}

func (db *DB) loadSchema() error {
	schema := &Table{Name: "sqlite_schema", root: 1, Columns: []string{"type", "name", "tbl_name", "rootpage", "sql"}, rowid: -1}
	db.tables = map[string]*Table{}
	return db.Scan(schema, func(r Row) error {
		if r.Text("type") != "table" {
			return nil
		}
		root := r.Int("rootpage")
		if root <= 0 || root > math.MaxUint32 {
			return nil // a virtual table, which has no B-tree of its own
		}
		cols, rowid, ok := columns(r.Text("sql"))
		if !ok {
			return nil // WITHOUT ROWID, or what this can't read: left out
		}
		name := r.Text("name")
		db.tables[name] = &Table{Name: name, root: uint32(root), Columns: cols, rowid: rowid}
		return nil
	})
}

// Row is a row of a table, by column.
type Row struct {
	table  *Table
	rowid  int64
	values []any
}

// Value is the value of column, which is nil, int64, float64, string or []byte,
// or nil where the table has no such column or the row was written before it.
func (r Row) Value(column string) any {
	for i, c := range r.table.Columns {
		if !strings.EqualFold(c, column) {
			continue
		}
		if i == r.table.rowid {
			return r.rowid
		}
		if i < len(r.values) {
			return r.values[i]
		}
		return nil
	}
	return nil
}

// Text is column as text: what's there as text or bytes, or a number in digits.
func (r Row) Text(column string) string {
	switch v := r.Value(column).(type) {
	case string:
		return v
	case []byte:
		return string(v)
	case int64:
		return strconv.FormatInt(v, 10)
	case float64:
		return strconv.FormatFloat(v, 'g', -1, 64)
	}
	return ""
}

// Int is column as a whole number, or 0.
func (r Row) Int(column string) int64 {
	switch v := r.Value(column).(type) {
	case int64:
		return v
	case float64:
		return int64(v)
	case string:
		var n int64
		_, _ = fmt.Sscan(v, &n)
		return n
	}
	return 0
}

// Bool is column as SQLite keeps a boolean: a number other than 0.
func (r Row) Bool(column string) bool {
	switch v := r.Value(column).(type) {
	case int64:
		return v != 0
	case string:
		return v == "1" || strings.EqualFold(v, "true")
	}
	return false
}

// Scan calls each with every row of table, in rowid order, until each returns an
// error, which Scan returns.
func (db *DB) Scan(table *Table, each func(Row) error) error {
	s := &scan{seen: map[uint32]bool{}}
	return db.walk(table, s, table.root, 0, func(row Row) error {
		s.rows++
		if s.rows > MaxRows {
			return fmt.Errorf("%s holds more than %d rows, more than this reads", table.Name, MaxRows)
		}
		return each(row)
	})
}

// scan is what one scan has read: each page once, and how many rows.
type scan struct {
	seen map[uint32]bool
	rows int
}

func (db *DB) walk(table *Table, s *scan, n uint32, depth int, each func(Row) error) error {
	if depth > maxDepth {
		return fmt.Errorf("%s's pages go deeper than a table can, so the file is damaged", table.Name)
	}
	// A page reached twice is a loop, which SQLite never writes.
	if s.seen[n] {
		return fmt.Errorf("%s reaches page %d twice, so the file is damaged", table.Name, n)
	}
	s.seen[n] = true
	buf := make([]byte, db.pageSize)
	page, err := db.page(n, buf)
	if err != nil {
		return err
	}
	head := 0
	if n == 1 {
		head = 100
	}
	if len(page) < head+8 {
		return fmt.Errorf("page %d is too short", n)
	}
	kind := page[head]
	cells := int(binary.BigEndian.Uint16(page[head+3:]))
	switch kind {
	case 0x0d: // a leaf of a table: the rows
		ptrs := head + 8
		for i := range cells {
			at := ptrs + 2*i
			if at+2 > len(page) {
				return fmt.Errorf("page %d's cells run past its end", n)
			}
			row, err := db.leafCell(table, page, int(binary.BigEndian.Uint16(page[at:])))
			if err != nil {
				return fmt.Errorf("%s, page %d: %w", table.Name, n, err)
			}
			if err := each(row); err != nil {
				return err
			}
		}
		return nil
	case 0x05: // inside a table: where each range of rowids is
		ptrs := head + 12
		children := make([]uint32, 0, cells+1)
		for i := range cells {
			at := ptrs + 2*i
			if at+2 > len(page) {
				return fmt.Errorf("page %d's cells run past its end", n)
			}
			cell := int(binary.BigEndian.Uint16(page[at:]))
			if cell+4 > len(page) {
				return fmt.Errorf("page %d points past its end", n)
			}
			children = append(children, binary.BigEndian.Uint32(page[cell:]))
		}
		children = append(children, binary.BigEndian.Uint32(page[head+8:]))
		for _, child := range children {
			if err := db.walk(table, s, child, depth+1, each); err != nil {
				return err
			}
		}
		return nil
	}
	return fmt.Errorf("%s's page %d is of kind %#x, not a table's", table.Name, n, kind)
}

// leafCell reads the row in a table leaf's cell at off.
func (db *DB) leafCell(table *Table, page []byte, off int) (Row, error) {
	if off >= len(page) {
		return Row{}, errors.New("a cell points past the page's end")
	}
	size, k := varint(page[off:])
	if k == 0 {
		return Row{}, errors.New("a cell's length can't be read")
	}
	off += k
	rowid, k := varint(page[off:])
	if k == 0 {
		return Row{}, errors.New("a cell's rowid can't be read")
	}
	off += k
	if size > maxPayload {
		return Row{}, fmt.Errorf("a row of %d bytes is more than this reads", size)
	}
	payload, err := db.payload(page, off, int(size))
	if err != nil {
		return Row{}, err
	}
	values, err := record(payload)
	if err != nil {
		return Row{}, err
	}
	return Row{table: table, rowid: int64(rowid), values: values}, nil //nolint:gosec // SQLite's rowids are signed 64-bit
}

// payload is a cell's whole payload of size bytes, from the page and the overflow
// pages after it.
func (db *DB) payload(page []byte, off, size int) ([]byte, error) {
	u := db.usable
	local := size
	if x := u - 35; size > x {
		m := (u-12)*32/255 - 23
		local = m + (size-m)%(u-4)
		if local > x {
			local = m
		}
	}
	if off+local > len(page) {
		return nil, errors.New("a row runs past its page")
	}
	out := make([]byte, 0, size)
	out = append(out, page[off:off+local]...)
	if local == size {
		return out, nil
	}
	if off+local+4 > len(page) {
		return nil, errors.New("a row's overflow can't be found")
	}
	next := binary.BigEndian.Uint32(page[off+local:])
	buf := make([]byte, db.pageSize)
	for hops := 0; len(out) < size; hops++ {
		if hops > maxPayload/(u-4)+1 {
			return nil, errors.New("a row's overflow pages loop")
		}
		p, err := db.page(next, buf)
		if err != nil {
			return nil, err
		}
		take := min(size-len(out), u-4)
		out = append(out, p[4:4+take]...)
		next = binary.BigEndian.Uint32(p[0:4])
	}
	return out, nil
}

// varint reads one of SQLite's variable-length integers, and how many bytes it
// took, or 0 where b is too short.
func varint(b []byte) (uint64, int) {
	var v uint64
	for i := range 9 {
		if i >= len(b) {
			return 0, 0
		}
		if i == 8 {
			return v<<8 | uint64(b[i]), 9
		}
		v = v<<7 | uint64(b[i]&0x7f)
		if b[i]&0x80 == 0 {
			return v, i + 1
		}
	}
	return v, 9
}

// record reads a record's values.
func record(b []byte) ([]any, error) {
	headerSize, k := varint(b)
	if k == 0 || headerSize > uint64(len(b)) {
		return nil, errors.New("a row's header runs past its end")
	}
	var types []uint64
	for at := k; at < int(headerSize); { //nolint:gosec // bounded above
		t, n := varint(b[at:int(headerSize)]) //nolint:gosec // bounded above
		if n == 0 {
			return nil, errors.New("a row's header can't be read")
		}
		types = append(types, t)
		at += n
	}
	body := b[headerSize:]
	values := make([]any, 0, len(types))
	for _, t := range types {
		n := serialSize(t)
		if n < 0 || n > len(body) {
			return nil, errors.New("a row's values run past its end")
		}
		v := body[:n]
		body = body[n:]
		switch {
		case t == 0:
			values = append(values, nil)
		case t >= 1 && t <= 6:
			values = append(values, signed(v))
		case t == 7:
			values = append(values, math.Float64frombits(binary.BigEndian.Uint64(v)))
		case t == 8:
			values = append(values, int64(0))
		case t == 9:
			values = append(values, int64(1))
		case t >= 12 && t%2 == 0:
			values = append(values, append([]byte(nil), v...))
		case t >= 13:
			values = append(values, string(v))
		default:
			return nil, fmt.Errorf("a row holds a value of type %d, which SQLite keeps for itself", t)
		}
	}
	return values, nil
}

func serialSize(t uint64) int {
	switch {
	case t <= 4:
		return int(t)
	case t == 5:
		return 6
	case t == 6, t == 7:
		return 8
	case t <= 11:
		return 0
	case t > 2*maxPayload:
		return -1
	}
	return int((t - 12) / 2)
}

// signed reads a big-endian two's-complement integer of 1 to 8 bytes.
func signed(b []byte) int64 {
	var v int64
	if b[0]&0x80 != 0 {
		v = -1
	}
	for _, c := range b {
		v = v<<8 | int64(c)
	}
	return v
}

// columns reads a CREATE TABLE statement's columns, in order, and which of them
// is the rowid, or -1. It reports false for one it can't read as a rowid table.
func columns(sql string) ([]string, int, bool) {
	open := strings.IndexByte(sql, '(')
	end := strings.LastIndexByte(sql, ')')
	if open < 0 || end <= open {
		return nil, -1, false
	}
	if tail := strings.ToUpper(sql[end+1:]); strings.Contains(tail, "WITHOUT") {
		return nil, -1, false
	}
	var cols []string
	var integer []bool
	rowid := -1
	keyedBy := "" // a table's own PRIMARY KEY of one column
	for _, def := range splitTop(sql[open+1 : end]) {
		def = strings.TrimSpace(def)
		words := strings.FieldsFunc(def, func(r rune) bool { return unicode.IsSpace(r) || r == '(' })
		if len(words) == 0 {
			continue
		}
		switch strings.ToUpper(words[0]) {
		case "PRIMARY":
			if in := strings.IndexByte(def, '('); in >= 0 {
				if key := splitTop(strings.TrimSuffix(strings.TrimSpace(def[in+1:]), ")")); len(key) == 1 {
					keyedBy, _ = identifier(strings.TrimSpace(key[0]))
				}
			}
			continue
		case "CONSTRAINT", "UNIQUE", "CHECK", "FOREIGN":
			continue
		}
		name, rest := identifier(def)
		if name == "" {
			return nil, -1, false
		}
		up := strings.ToUpper(strings.Join(strings.Fields(rest), " "))
		isInteger := strings.HasPrefix(up, "INTEGER")
		if isInteger && strings.Contains(up, "PRIMARY KEY") && !strings.Contains(up, "PRIMARY KEY DESC") {
			rowid = len(cols)
		}
		cols = append(cols, name)
		integer = append(integer, isInteger)
	}
	for i, c := range cols {
		if rowid < 0 && keyedBy != "" && strings.EqualFold(c, keyedBy) && integer[i] {
			rowid = i
		}
	}
	return cols, rowid, len(cols) > 0
}

// splitTop splits s at the commas outside parentheses and quotes.
func splitTop(s string) []string {
	var out []string
	depth, start := 0, 0
	var quote rune
	for i, r := range s {
		switch {
		case quote != 0:
			if r == quote {
				quote = 0
			}
		case r == '"' || r == '\'' || r == '`':
			quote = r
		case r == '[':
			quote = ']'
		case r == '(':
			depth++
		case r == ')':
			depth--
		case r == ',' && depth == 0:
			out = append(out, s[start:i])
			start = i + 1
		}
	}
	return append(out, s[start:])
}

// identifier reads a column's name from the start of its definition, quoted or
// not, and what follows it.
func identifier(def string) (string, string) {
	if def == "" {
		return "", ""
	}
	closing := map[byte]byte{'"': '"', '`': '`', '[': ']', '\'': '\''}[def[0]]
	if closing != 0 {
		end := strings.IndexByte(def[1:], closing)
		if end < 0 {
			return "", ""
		}
		return def[1 : 1+end], def[2+end:]
	}
	end := strings.IndexFunc(def, unicode.IsSpace)
	if end < 0 {
		return def, ""
	}
	return def[:end], def[end:]
}
