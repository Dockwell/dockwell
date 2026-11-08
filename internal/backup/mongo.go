// SPDX-License-Identifier: AGPL-3.0-only

package backup

import (
	"bufio"
	"bytes"
	"compress/gzip"
	"encoding/binary"
	"errors"
	"fmt"
	"io"
)

// MongoDB's tools, as its official image has them (ADR 0038). Each signs in as the
// root user the image made, from MONGO_INITDB_ROOT_USERNAME and
// MONGO_INITDB_ROOT_PASSWORD or their _FILE forms, with the password in a config
// file on a descriptor the shell opens: never an argument, which ps shows anyone
// on the server. With neither set, MongoDB checks no one, and they sign in as no one.
const mongoSignIn = `u="${MONGO_INITDB_ROOT_USERNAME:-}" p="${MONGO_INITDB_ROOT_PASSWORD:-}"
[ -n "${MONGO_INITDB_ROOT_USERNAME_FILE:-}" ] && u="$(cat "$MONGO_INITDB_ROOT_USERNAME_FILE")"
[ -n "${MONGO_INITDB_ROOT_PASSWORD_FILE:-}" ] && p="$(cat "$MONGO_INITDB_ROOT_PASSWORD_FILE")"
port="${DW_PORT:-27017}"
`

// mongoConfig gives a tool the password on descriptor 3, as YAML in single quotes.
const mongoConfig = `3<<END
password: '$(printf %s "$p" | sed "s/'/''/g")'
END
`

// mongoShell runs mongosh on a script with a home of its own, as the image's HOME is
// the data directory, where it would keep a log of each run.
const mongoShell = `d="$(mktemp -d)" || exit 1
said="$(HOME="$d" MONGO_INITDB_ROOT_USERNAME="$u" MONGO_INITDB_ROOT_PASSWORD="$p" mongosh --quiet --norc --nodb --eval "$script")"
c=$?; rm -rf "$d"
`

// mongoDump dumps the whole server, with its users and roles, as one archive on
// stdout. A member of a replica set is dumped with its oplog, so the dump is one
// moment; one that runs alone keeps none, so its dump isn't.
var mongoDump = []string{"sh", "-c", mongoSignIn + `script='print(new Mongo("mongodb://127.0.0.1:" + process.env.DW_PORT + "/?directConnection=true&serverSelectionTimeoutMS=5000").getDB("admin").runCommand({hello: 1}).setName || "")'
` + `export DW_PORT="$port"
` + mongoShell + `[ "$c" = 0 ] || { printf '%s\n' "$said" >&2; exit "$c"; }
set -- --host 127.0.0.1 --port "$port" --archive
[ -n "$said" ] && set -- "$@" --oplog
[ -n "$u" ] && set -- "$@" --username "$u" --authenticationDatabase admin --config /dev/fd/3
exec mongodump "$@" ` + mongoConfig}

// mongoLoad restores an archive from stdin, replacing each collection it holds,
// with its oplog replayed where it has one, which the agent says with DW_OPLOG.
var mongoLoad = []string{"sh", "-c", mongoSignIn + `set -- --host 127.0.0.1 --port "$port" --archive --drop
[ -n "${DW_OPLOG:-}" ] && set -- "$@" --oplogReplay
[ -n "$u" ] && set -- "$@" --username "$u" --authenticationDatabase admin --config /dev/fd/3
exec mongorestore "$@" ` + mongoConfig}

// mongoReady exits 0 once MongoDB takes connections as itself: not the temporary
// server its image runs to make its root user, which forks, nor before that user
// is there. It exits 2 when MongoDB refuses the sign-in for good.
var mongoReady = []string{"sh", "-c", mongoSignIn + `export DW_PORT="$port"
script='const env = process.env;
const admin = new Mongo("mongodb://127.0.0.1:" + env.DW_PORT + "/?directConnection=true&serverSelectionTimeoutMS=2000").getDB("admin");
const forked = (o) => !!((o.parsed || {}).processManagement || {}).fork;
try {
  if (env.MONGO_INITDB_ROOT_USERNAME) admin.auth(env.MONGO_INITDB_ROOT_USERNAME, env.MONGO_INITDB_ROOT_PASSWORD || "");
} catch (e) {
  // Not up yet, or not taking connections.
  if (!/Authentication failed/.test(String(e))) quit(1);
  // The temporary server checks no one, and has no root user yet.
  let o = {};
  try { o = admin.runCommand({ getCmdLineOpts: 1 }); } catch (e) {}
  quit(o.ok && forked(o) ? 1 : 2);
}
quit(forked(admin.runCommand({ getCmdLineOpts: 1 })) ? 1 : 0);'
` + mongoShell + `exit "$c"`}

// mongoMagic starts an archive mongodump writes.
const mongoMagic = 0x8199e26d

// mongoTerminator ends a prelude, and each piece of a collection's documents.
const mongoTerminator = -1

// maxMongoDocument bounds a document in an archive: MongoDB's own limit, with room.
const maxMongoDocument = 48 << 20

// mongoArchive reads an archive mongodump wrote: its prelude, which names each
// collection, then each collection's documents in pieces, each after a header that
// names it, the last header saying it's the end of that collection.
type mongoArchive struct {
	r *bufio.Reader
}

// namespace is a collection as an archive names it.
type namespace struct{ db, collection string }

// oplog is how an archive names the oplog it was dumped with.
var oplog = namespace{collection: "oplog"}

// next reads the next document, or says the next is a terminator.
func (a *mongoArchive) next(keep bool) (doc []byte, end bool, err error) {
	var size [4]byte
	if _, err := io.ReadFull(a.r, size[:]); err != nil {
		return nil, false, err
	}
	n := int32(binary.LittleEndian.Uint32(size[:])) //nolint:gosec // a length as the archive says it
	if n == mongoTerminator {
		return nil, true, nil
	}
	if n < 5 || n > maxMongoDocument {
		return nil, false, fmt.Errorf("a document says it's %d bytes", n)
	}
	if !keep {
		_, err := io.CopyN(io.Discard, a.r, int64(n)-4)
		if errors.Is(err, io.EOF) {
			err = io.ErrUnexpectedEOF
		}
		return nil, false, err
	}
	doc = make([]byte, n)
	copy(doc, size[:])
	if _, err := io.ReadFull(a.r, doc[4:]); err != nil {
		if errors.Is(err, io.EOF) {
			err = io.ErrUnexpectedEOF
		}
		return nil, false, err
	}
	return doc, false, nil
}

// prelude reads the archive's start: what made it, then each collection it holds.
func (a *mongoArchive) prelude() ([]namespace, error) {
	var magic [4]byte
	if _, err := io.ReadFull(a.r, magic[:]); err != nil {
		return nil, fmt.Errorf("it's empty or cut short: %w", err)
	}
	if binary.LittleEndian.Uint32(magic[:]) != mongoMagic {
		return nil, errors.New("it isn't an archive mongodump wrote")
	}
	if _, _, err := a.next(false); err != nil { // what made it
		return nil, err
	}
	var out []namespace
	for {
		doc, end, err := a.next(true)
		if err != nil {
			return nil, err
		}
		if end {
			return out, nil
		}
		f, err := bsonStrings(doc, "db", "collection")
		if err != nil {
			return nil, err
		}
		out = append(out, namespace{db: f["db"], collection: f["collection"]})
	}
}

// mongoPrelude reads what an archive holds from its start, and gives back a reader
// of the whole archive, what it read included, for a restore to go on with.
func mongoPrelude(r io.Reader) ([]namespace, io.Reader, error) {
	var read bytes.Buffer
	a := &mongoArchive{r: bufio.NewReader(io.TeeReader(r, &read))}
	ns, err := a.prelude()
	if err != nil {
		return nil, nil, fmt.Errorf("the dump doesn't read as mongodump's: %w", err)
	}
	// What the buffered reader read ahead is in read too, in order.
	return ns, io.MultiReader(&read, r), nil
}

// verifyMongoArchive reads a gzipped archive through, and says it's whole: each
// collection its prelude names ends, and nothing is cut short.
func verifyMongoArchive(r io.Reader) error {
	gz, err := gzip.NewReader(r)
	if err != nil {
		return fmt.Errorf("the dump doesn't open: %w", err)
	}
	defer gz.Close()
	a := &mongoArchive{r: bufio.NewReaderSize(gz, 1<<20)}
	named, err := a.prelude()
	if err != nil {
		return fmt.Errorf("the dump stops short or is damaged: %w", err)
	}
	ended := map[namespace]bool{}
	for {
		// A header, then its collection's documents to a terminator, or, at the
		// end of a collection, a header that says so and a terminator.
		doc, end, err := a.next(true)
		if errors.Is(err, io.EOF) {
			break
		}
		if err != nil {
			return fmt.Errorf("the dump stops short or is damaged: %w", err)
		}
		if end {
			return errors.New("the dump stops short or is damaged: a header is missing")
		}
		f, err := bsonStrings(doc, "db", "collection")
		if err != nil {
			return fmt.Errorf("the dump stops short or is damaged: %w", err)
		}
		eof, err := bsonBool(doc, "EOF")
		if err != nil {
			return fmt.Errorf("the dump stops short or is damaged: %w", err)
		}
		for {
			_, end, err := a.next(false)
			if err != nil {
				return fmt.Errorf("the dump stops short or is damaged: %w", err)
			}
			if end {
				break
			}
		}
		if eof {
			ended[namespace{db: f["db"], collection: f["collection"]}] = true
		}
	}
	for _, ns := range named {
		if !ended[ns] {
			return fmt.Errorf("the dump doesn't end as a whole one does: %s.%s stops short", ns.db, ns.collection)
		}
	}
	return nil
}

// bsonStrings reads a document's top-level strings by name.
func bsonStrings(doc []byte, names ...string) (map[string]string, error) {
	out := map[string]string{}
	err := bsonWalk(doc, func(kind byte, name string, value []byte) {
		for _, n := range names {
			if kind == 0x02 && name == n && len(value) > 4 {
				out[n] = string(value[4 : len(value)-1])
			}
		}
	})
	return out, err
}

// bsonBool reads a document's top-level boolean by name, false where it's absent.
func bsonBool(doc []byte, name string) (bool, error) {
	var out bool
	err := bsonWalk(doc, func(kind byte, n string, value []byte) {
		if kind == 0x08 && n == name && len(value) == 1 {
			out = value[0] == 1
		}
	})
	return out, err
}

// bsonWalk hands fn each of a document's top-level elements: its kind, its name and
// its value's bytes. It reads the kinds an archive's headers hold, and the sizes of
// the rest, which it passes over.
func bsonWalk(doc []byte, fn func(kind byte, name string, value []byte)) error {
	if len(doc) < 5 || int(binary.LittleEndian.Uint32(doc)) != len(doc) {
		return errors.New("a document's length doesn't match")
	}
	i := 4
	for i < len(doc)-1 {
		kind := doc[i]
		i++
		end := bytes.IndexByte(doc[i:], 0)
		if end < 0 {
			return errors.New("a name doesn't end")
		}
		name := string(doc[i : i+end])
		i += end + 1
		var n int
		switch kind {
		case 0x01, 0x09, 0x11, 0x12: // double, date, timestamp, int64
			n = 8
		case 0x02, 0x0d, 0x0e: // string, code, symbol: a length, then that much
			if i+4 > len(doc) {
				return errors.New("a string is cut short")
			}
			n = 4 + int(binary.LittleEndian.Uint32(doc[i:]))
		case 0x03, 0x04: // document, array
			if i+4 > len(doc) {
				return errors.New("a document is cut short")
			}
			n = int(binary.LittleEndian.Uint32(doc[i:]))
		case 0x05: // binary: a length, its subtype, then that much
			if i+4 > len(doc) {
				return errors.New("a binary is cut short")
			}
			n = 5 + int(binary.LittleEndian.Uint32(doc[i:]))
		case 0x07: // object ID
			n = 12
		case 0x08: // boolean
			n = 1
		case 0x0a, 0x06, 0xff, 0x7f: // null, undefined, min and max
			n = 0
		case 0x10: // int32
			n = 4
		case 0x13: // decimal128
			n = 16
		default:
			return fmt.Errorf("a header holds a value of a kind it doesn't (%#x)", kind)
		}
		if n < 0 || i+n > len(doc)-1 {
			return errors.New("a value runs past its document")
		}
		fn(kind, name, doc[i:i+n])
		i += n
	}
	return nil
}
