// SPDX-License-Identifier: AGPL-3.0-only

package sqlitefile

import (
	"bytes"
	"database/sql"
	"encoding/binary"
	"errors"
	"fmt"
	"math/rand/v2"
	"os"
	"path/filepath"
	"reflect"
	"strconv"
	"strings"
	"testing"

	_ "modernc.org/sqlite" // SQLite itself writes what these tests read
)

// sqliteAt opens a database at path with SQLite, with what pragmas say, on one
// connection, so a WAL stays as it is until the test closes it.
func sqliteAt(t *testing.T, path string, pragmas ...string) *sql.DB {
	t.Helper()
	var dsn strings.Builder
	dsn.WriteString("file:" + path)
	for i, p := range pragmas {
		sep := "&"
		if i == 0 {
			sep = "?"
		}
		dsn.WriteString(sep + "_pragma=" + p)
	}
	db, err := sql.Open("sqlite", dsn.String())
	if err != nil {
		t.Fatal(err)
	}
	db.SetMaxOpenConns(1)
	t.Cleanup(func() { _ = db.Close() })
	return db
}

func exec(t *testing.T, db *sql.DB, query string, args ...any) {
	t.Helper()
	if _, err := db.Exec(query, args...); err != nil {
		t.Fatalf("%s: %v", query, err)
	}
}

type row struct {
	ID   int64
	Name string
	N    any
	F    any
	B    []byte
	Big  string
}

// want reads the table with SQLite, as rows.
func want(t *testing.T, db *sql.DB) []row {
	t.Helper()
	rows, err := db.Query(`SELECT id, name, n, f, b, big FROM things ORDER BY id`)
	if err != nil {
		t.Fatal(err)
	}
	defer rows.Close()
	var out []row
	for rows.Next() {
		var r row
		var big sql.NullString
		if err := rows.Scan(&r.ID, &r.Name, &r.N, &r.F, &r.B, &big); err != nil {
			t.Fatal(err)
		}
		r.Big = big.String
		out = append(out, r)
	}
	return out
}

// got reads the table with this package.
func got(t *testing.T, path string) []row {
	t.Helper()
	db, err := Open(path)
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	table, ok := db.Table("things")
	if !ok {
		t.Fatalf("no table things in %v", db.Tables())
	}
	var out []row
	if err := db.Scan(table, func(r Row) error {
		b, _ := r.Value("b").([]byte)
		out = append(out, row{ID: r.Int("id"), Name: r.Text("name"), N: r.Value("n"), F: r.Value("f"), B: b, Big: r.Text("big")})
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	return out
}

func fill(t *testing.T, db *sql.DB, rows int) {
	t.Helper()
	exec(t, db, "CREATE TABLE `things` (`id` integer PRIMARY KEY AUTOINCREMENT,`created_at` datetime,`name` text NOT NULL,`n` integer,`f` real,`b` blob,`big` text)")
	tx, err := db.Begin()
	if err != nil {
		t.Fatal(err)
	}
	r := rand.New(rand.NewPCG(1, 2))
	for i := range rows {
		var n, f any
		if i%7 != 0 {
			n = r.Int64N(1<<40) - 1<<39
			f = r.Float64() * 1000
		}
		big := ""
		switch i % 97 {
		case 3:
			big = strings.Repeat("compose: ", 2000) // past a page: overflow
		case 5:
			big = strings.Repeat("x", 200_000) // many overflow pages
		}
		if _, err := tx.Exec("INSERT INTO things (created_at, name, n, f, b, big) VALUES (CURRENT_TIMESTAMP, ?, ?, ?, ?, ?)",
			fmt.Sprintf("thing %d, 名字", i), n, f, []byte{byte(i), 0, 255}, big); err != nil {
			t.Fatal(err)
		}
	}
	if err := tx.Commit(); err != nil {
		t.Fatal(err)
	}
}

func same(t *testing.T, want, got []row) {
	t.Helper()
	if len(want) != len(got) {
		t.Fatalf("%d rows, want %d", len(got), len(want))
	}
	for i := range want {
		if !reflect.DeepEqual(want[i], got[i]) {
			t.Fatalf("row %d = %+v,\nwant %+v", i, short(got[i]), short(want[i]))
		}
	}
}

func short(r row) row {
	if len(r.Big) > 40 {
		r.Big = r.Big[:40] + fmt.Sprintf("… (%d)", len(r.Big))
	}
	return r
}

// What SQLite writes reads the same here, at every page size, with rows deep in
// the tree, long ones over many pages, and values of every kind.
func TestReadsWhatSQLiteWrote(t *testing.T) {
	for _, size := range []int{512, 1024, 4096, 65536} {
		t.Run(strconv.Itoa(size), func(t *testing.T) {
			path := filepath.Join(t.TempDir(), "agent.db")
			db := sqliteAt(t, path, fmt.Sprintf("page_size(%d)", size))
			fill(t, db, 3000)
			wanted := want(t, db)
			same(t, wanted, got(t, path))
		})
	}
}

// A column added since rows were written reads as nothing in those rows.
func TestAColumnAddedLaterIsNothingInOlderRows(t *testing.T) {
	path := filepath.Join(t.TempDir(), "agent.db")
	db := sqliteAt(t, path)
	fill(t, db, 10)
	exec(t, db, "ALTER TABLE `things` ADD `status` text")
	exec(t, db, "INSERT INTO things (name, status) VALUES ('new', 'Running')")
	r, err := Open(path)
	if err != nil {
		t.Fatal(err)
	}
	defer r.Close()
	table, _ := r.Table("things")
	if c := table.Columns; c[len(c)-1] != "status" {
		t.Fatalf("columns %v", c)
	}
	var statuses []any
	_ = r.Scan(table, func(row Row) error {
		statuses = append(statuses, row.Value("status"))
		return nil
	})
	if len(statuses) != 11 || statuses[0] != nil || statuses[10] != "Running" {
		t.Errorf("statuses %v", statuses)
	}
	if v := (Row{table: table}).Value("no_such_column"); v != nil {
		t.Errorf("a column that isn't there = %v", v)
	}
}

// What's committed in the WAL and not yet in the file reads as the file has it
// after a checkpoint: added, changed and deleted rows. What isn't committed yet
// doesn't read at all.
func TestReadsWhatTheWALCommitted(t *testing.T) {
	path := filepath.Join(t.TempDir(), "agent.db")
	db := sqliteAt(t, path, "journal_mode(WAL)", "wal_autocheckpoint(0)", "cache_size(8)")
	fill(t, db, 500)
	exec(t, db, "PRAGMA wal_checkpoint(TRUNCATE)")
	exec(t, db, "UPDATE things SET name = 'changed' WHERE id % 3 = 0")
	exec(t, db, "DELETE FROM things WHERE id % 5 = 0")
	exec(t, db, "INSERT INTO things (name, big) VALUES ('after', ?)", strings.Repeat("y", 30_000))
	if st, err := os.Stat(path + "-wal"); err != nil || st.Size() == 0 {
		t.Fatalf("the test needs a WAL: %v", err)
	}
	wanted := want(t, db)
	same(t, wanted, got(t, path))

	// A transaction under way spills to the WAL, uncommitted, past its small cache.
	before, _ := os.Stat(path + "-wal")
	tx, err := db.Begin()
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = tx.Rollback() }()
	for range 400 {
		if _, err := tx.Exec("INSERT INTO things (name, big) VALUES ('uncommitted', ?)", strings.Repeat("z", 3000)); err != nil {
			t.Fatal(err)
		}
	}
	if after, _ := os.Stat(path + "-wal"); after.Size() <= before.Size() {
		t.Fatal("the transaction wrote nothing to the WAL, so this tests nothing")
	}
	for _, r := range got(t, path) {
		if r.Name == "uncommitted" {
			t.Fatal("a row of a transaction under way read as there")
		}
	}
}

// A change after opening says so, so a caller reads again.
func TestAChangeAfterOpeningSaysSo(t *testing.T) {
	path := filepath.Join(t.TempDir(), "agent.db")
	db := sqliteAt(t, path, "journal_mode(WAL)", "wal_autocheckpoint(0)")
	fill(t, db, 20)
	r, err := Open(path)
	if err != nil {
		t.Fatal(err)
	}
	defer r.Close()
	if r.Changed() {
		t.Fatal("changed with nothing written")
	}
	exec(t, db, "INSERT INTO things (name) VALUES ('later')")
	if !r.Changed() {
		t.Error("a write since reads as no change")
	}
}

// What isn't a database, a link, or a damaged one is an error, never a panic.
func TestWhatIsntADatabaseIsRefused(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "agent.db")
	db := sqliteAt(t, path)
	fill(t, db, 300)
	_ = db.Close()

	text := filepath.Join(dir, "notes.txt")
	if err := os.WriteFile(text, bytes.Repeat([]byte("not a database "), 100), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := Open(text); err == nil || !strings.Contains(err.Error(), "isn't a SQLite database") {
		t.Errorf("a text file: %v", err)
	}
	link := filepath.Join(dir, "link.db")
	if err := os.Symlink(path, link); err != nil {
		t.Fatal(err)
	}
	if _, err := Open(link); err == nil {
		t.Error("a link was followed")
	}

	whole, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	r := rand.New(rand.NewPCG(3, 4))
	for i := range 300 {
		damaged := bytes.Clone(whole)
		switch i % 3 {
		case 0:
			damaged = damaged[:r.IntN(len(damaged))]
		default:
			for range 1 + r.IntN(20) {
				damaged[100+r.IntN(len(damaged)-100)] = byte(r.Uint32())
			}
		}
		bad := filepath.Join(dir, fmt.Sprintf("bad-%d.db", i))
		if err := os.WriteFile(bad, damaged, 0o600); err != nil {
			t.Fatal(err)
		}
		func() {
			defer func() {
				if p := recover(); p != nil {
					t.Fatalf("damage %d panicked: %v", i, p)
				}
			}()
			d, err := Open(bad)
			if err != nil {
				return
			}
			defer d.Close()
			for _, table := range d.Tables() {
				_ = d.Scan(table, func(Row) error { return nil })
			}
		}()
	}
}

func TestColumnsOfATable(t *testing.T) {
	for _, c := range []struct {
		sql   string
		cols  []string
		rowid int
		ok    bool
	}{
		{"CREATE TABLE `app_installs` (`id` integer PRIMARY KEY AUTOINCREMENT,`name` text NOT NULL UNIQUE,`param` text,CONSTRAINT `fk` FOREIGN KEY (`app_id`) REFERENCES `apps`(`id`))", []string{"id", "name", "param"}, 0, true},
		{`CREATE TABLE "a b" ("x y" TEXT, [z] INTEGER, w DEFAULT ('1,2'), PRIMARY KEY(z))`, []string{"x y", "z", "w"}, 1, true},
		{"CREATE TABLE t (k TEXT PRIMARY KEY, v) WITHOUT ROWID", nil, -1, false},
		{"CREATE TABLE t (id INTEGER PRIMARY KEY DESC, v)", []string{"id", "v"}, -1, true},
		{"CREATE TABLE t (a, b, UNIQUE (a, b))", []string{"a", "b"}, -1, true},
	} {
		cols, rowid, ok := columns(c.sql)
		if !reflect.DeepEqual(cols, c.cols) || rowid != c.rowid || ok != c.ok {
			t.Errorf("columns(%s) = %v, %d, %v, want %v, %d, %v", c.sql, cols, rowid, ok, c.cols, c.rowid, c.ok)
		}
	}
}

// In the rollback journal's mode, a write under way is in its journal: opening
// then says so, and one that starts after opening reads as a change.
func TestAWriteInRollbackModeSaysSo(t *testing.T) {
	path := filepath.Join(t.TempDir(), "rollback.db")
	db := sqliteAt(t, path, "journal_mode(PERSIST)")
	fill(t, db, 10)
	r, err := Open(path)
	if err != nil {
		t.Fatal(err)
	}
	defer r.Close()
	if r.Changed() {
		t.Fatal("changed with nothing written")
	}
	// A journal kept between writes, as PERSIST keeps it, holds nothing under way.
	if err := os.WriteFile(path+"-journal", make([]byte, 512), 0o600); err != nil {
		t.Fatal(err)
	}
	if r.Changed() {
		t.Error("a zeroed journal reads as a write")
	}
	if err := os.WriteFile(path+"-journal", append(bytes.Clone(journalMagic), make([]byte, 504)...), 0o600); err != nil {
		t.Fatal(err)
	}
	if !r.Changed() {
		t.Error("a write under way reads as no change")
	}
	if _, err := Open(path); !errors.Is(err, ErrWriting) {
		t.Errorf("opening during a write = %v", err)
	}
}

// A table whose pages loop is damaged, and says so, rather than read for ever.
func TestAPageReachedTwiceIsDamage(t *testing.T) {
	path := filepath.Join(t.TempDir(), "loop.db")
	db := sqliteAt(t, path, "journal_mode(DELETE)")
	fill(t, db, 2000)
	if err := db.Close(); err != nil {
		t.Fatal(err)
	}
	r, err := Open(path)
	if err != nil {
		t.Fatal(err)
	}
	table, ok := r.Table("things")
	if !ok {
		t.Fatal("no table things")
	}
	page, err := r.page(table.root, make([]byte, r.pageSize))
	if err != nil || page[0] != 0x05 {
		t.Fatalf("the table's root isn't an interior page: %v", err)
	}
	r.Close()
	// Its last child is itself.
	f, err := os.OpenFile(path, os.O_RDWR, 0)
	if err != nil {
		t.Fatal(err)
	}
	var self [4]byte
	binary.BigEndian.PutUint32(self[:], table.root)
	if _, err := f.WriteAt(self[:], int64(table.root-1)*int64(r.pageSize)+8); err != nil {
		t.Fatal(err)
	}
	_ = f.Close()
	r, err = Open(path)
	if err != nil {
		t.Fatal(err)
	}
	defer r.Close()
	if err := r.Scan(table, func(Row) error { return nil }); err == nil || !strings.Contains(err.Error(), "twice") {
		t.Errorf("a loop = %v", err)
	}
}
