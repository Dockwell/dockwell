// SPDX-License-Identifier: AGPL-3.0-only

package backup

import (
	"bytes"
	"compress/gzip"
	"encoding/binary"
	"io"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"testing"

	"github.com/dockwell/dockwell/internal/docker"
	"github.com/dockwell/dockwell/internal/docker/dockertest"
	"github.com/dockwell/dockwell/internal/resource"
)

// bsonDoc builds a document of strings, booleans and int32s, in the order given.
func bsonDoc(fields ...any) []byte {
	var body bytes.Buffer
	for i := 0; i+1 < len(fields); i += 2 {
		name := fields[i].(string) //nolint:forcetypeassert // the test's own pairs
		switch v := fields[i+1].(type) {
		case string:
			body.WriteByte(0x02)
			body.WriteString(name + "\x00")
			_ = binary.Write(&body, binary.LittleEndian, int32(len(v)+1))
			body.WriteString(v + "\x00")
		case bool:
			body.WriteByte(0x08)
			body.WriteString(name + "\x00")
			if v {
				body.WriteByte(1)
			} else {
				body.WriteByte(0)
			}
		case int32:
			body.WriteByte(0x10)
			body.WriteString(name + "\x00")
			_ = binary.Write(&body, binary.LittleEndian, v)
		case int64:
			body.WriteByte(0x12)
			body.WriteString(name + "\x00")
			_ = binary.Write(&body, binary.LittleEndian, v)
		}
	}
	out := make([]byte, 4, body.Len()+5)
	binary.LittleEndian.PutUint32(out, uint32(body.Len()+5))
	return append(append(out, body.Bytes()...), 0)
}

// archiveOf is an archive as mongodump writes one of these collections, each with
// a document, leaving out the end of any named in unended.
func archiveOf(collections []namespace, unended ...namespace) []byte {
	var b bytes.Buffer
	_ = binary.Write(&b, binary.LittleEndian, uint32(mongoMagic))
	term := func() { _ = binary.Write(&b, binary.LittleEndian, int32(mongoTerminator)) }
	b.Write(bsonDoc("concurrent_collections", int32(4), "version", "0.1", "server_version", "8.0.34", "tool_version", "100.20.0"))
	for _, ns := range collections {
		b.Write(bsonDoc("db", ns.db, "collection", ns.collection, "metadata", "{}", "size", int32(0), "type", "collection"))
	}
	term()
	for _, ns := range collections {
		b.Write(bsonDoc("db", ns.db, "collection", ns.collection, "EOF", false, "CRC", int64(0)))
		b.Write(bsonDoc("n", int32(1)))
		term()
		if !slices.Contains(unended, ns) {
			b.Write(bsonDoc("db", ns.db, "collection", ns.collection, "EOF", true, "CRC", int64(0)))
			term()
		}
	}
	return b.Bytes()
}

func gzipped(t *testing.T, b []byte) []byte {
	t.Helper()
	var out bytes.Buffer
	w := gzip.NewWriter(&out)
	if _, err := w.Write(b); err != nil {
		t.Fatal(err)
	}
	if err := w.Close(); err != nil {
		t.Fatal(err)
	}
	return out.Bytes()
}

var shop = []namespace{{"shop", "orders"}, {"admin", "system.users"}}

// A dump is whole when each collection its prelude names ends, and nothing is cut.
func TestAMongoArchiveIsWholeWhenEachCollectionEnds(t *testing.T) {
	whole := archiveOf(shop)
	if err := verifyMongoArchive(bytes.NewReader(gzipped(t, whole))); err != nil {
		t.Errorf("a whole archive: %v", err)
	}
	for what, b := range map[string][]byte{
		"cut short":         whole[:len(whole)-10],
		"a collection open": archiveOf(shop, shop[0]),
		"not an archive":    []byte("-- PostgreSQL database cluster dump complete\n"),
		"empty":             nil,
	} {
		if err := verifyMongoArchive(bytes.NewReader(gzipped(t, b))); err == nil {
			t.Errorf("an archive %s reads as whole", what)
		}
	}
	if err := verifyMongoArchive(bytes.NewReader(gzipped(t, archiveOf(shop, shop[0])))); err == nil || !strings.Contains(err.Error(), "shop.orders stops short") {
		t.Errorf("an open collection says %v", err)
	}
}

// A restore reads what a dump holds from its start, and goes on with all of it.
func TestAMongoPreludeSaysWhetherThereIsAnOplog(t *testing.T) {
	for _, withOplog := range []bool{false, true} {
		collections := shop
		if withOplog {
			collections = append(slices.Clone(shop), oplog)
		}
		archive := archiveOf(collections)
		ns, whole, err := mongoPrelude(bytes.NewReader(archive))
		if err != nil {
			t.Fatal(err)
		}
		if slices.Contains(ns, oplog) != withOplog || len(ns) != len(collections) {
			t.Errorf("with an oplog %v, the prelude names %v", withOplog, ns)
		}
		if all, err := io.ReadAll(whole); err != nil || !bytes.Equal(all, archive) {
			t.Errorf("the archive read on after its prelude isn't the archive: %v", err)
		}
	}
}

// mongo stands in for MongoDB's image: its data is one archive, which mongodump
// writes and mongorestore reads, and it says whether a restore replayed an oplog.
// set is the replica set its configuration file makes it a member of.
type mongo struct {
	mu       sync.Mutex
	replayed []bool
	set      string
}

func (m *mongo) exec(x dockertest.Exec) int {
	file := filepath.Join(x.Volumes["/data/db"], "data.archive")
	script := strings.Join(x.Cmd, " ")
	switch {
	case strings.Contains(script, "exec mongodump"):
		data, err := os.ReadFile(file)
		if err != nil {
			return 1
		}
		_, _ = x.Stdout.Write(data)
		return 0
	case strings.Contains(script, "exec mongorestore"):
		data, err := io.ReadAll(x.Stdin)
		if err != nil {
			return 1
		}
		m.mu.Lock()
		m.replayed = append(m.replayed, strings.Contains(script, "DW_OPLOG=1"))
		m.mu.Unlock()
		if err := os.WriteFile(file, data, 0o600); err != nil {
			return 1
		}
		return 0
	case strings.Contains(script, "isWritablePrimary"):
		return 0
	case strings.Contains(script, "print(set.replSet"):
		_, _ = io.WriteString(x.Stdout, m.set+"\n")
		return 0
	}
	return 127
}

// A MongoDB is backed up with mongodump, as an archive of its own kind, while it
// runs, and restored into a new one with mongorestore, its oplog replayed where it
// was dumped with one.
func TestAMongoDBIsBackedUpWithItsOwnTools(t *testing.T) {
	for _, withOplog := range []bool{false, true} {
		b := newBench(t)
		m := &mongo{}
		b.engine.AddLocalImage("mongo:8.0", dockertest.Image{Exec: m.exec, Config: docker.Config{Cmd: []string{"mongod"}}})
		collections := shop
		if withOplog {
			collections = append(slices.Clone(shop), oplog)
		}
		archive := archiveOf(collections)
		b.write("data.archive", string(archive))
		b.run("db", "mongo:8.0", "/data/db")

		got := b.take(false)
		if got.Method != resource.BackupDump || got.Format != formatMongoDump || got.Database != "MongoDB" {
			t.Fatalf("the backup is %+v", got)
		}
		if _, err := os.Stat(filepath.Join(b.runner.dir(ref), "bkp_1.archive.gz")); err != nil {
			t.Fatalf("the archive isn't kept as one: %v", err)
		}

		b.write("data.archive", "what changed since")
		if err := b.restore(resource.RestoreSource{Backup: "bkp_1", Change: "chg_1"}); err != nil {
			t.Fatalf("restoring: %v", err)
		}
		if got := b.read("data.archive"); got != string(archive) {
			t.Errorf("the restore loaded %d bytes, %q..., want the archive's %d", len(got), got[:min(len(got), 24)], len(archive))
		}
		m.mu.Lock()
		if !slices.Equal(m.replayed, []bool{withOplog}) {
			t.Errorf("with an oplog %v, the restore replayed %v", withOplog, m.replayed)
		}
		m.mu.Unlock()
	}
}

// What a MongoDB dump runs signs in from the container's environment, and holds no
// password: the tools get it on a descriptor the shell opens.
func TestMongoDBsToolsHoldNoPassword(t *testing.T) {
	for name, cmd := range map[string][]string{"dump": mongoDump, "load": mongoLoad, "ready": mongoReady} {
		script := strings.Join(cmd, " ")
		if strings.Contains(script, "--password") || !strings.Contains(script, "MONGO_INITDB_ROOT_PASSWORD") {
			t.Errorf("the %s runs %q", name, script)
		}
		if name != "ready" && !strings.Contains(script, "--config /dev/fd/3") {
			t.Errorf("the %s doesn't read its password from a descriptor: %q", name, script)
		}
	}
}

// A dump doesn't load into a member of a replica set, as the new server it starts
// knows no set: the restore is refused when it's planned, before anything stops,
// whether the member's command line says so or its configuration file (#373).
func TestADumpIsntRestoredIntoAReplicaSetMember(t *testing.T) {
	for _, c := range []struct {
		name  string
		cmd   []string
		asked string
	}{
		{"its command line", []string{"mongod", "--replSet", "rs0"}, ""},
		{"its command line, with =", []string{"mongod", "--bind_ip_all", "--replSet=rs0"}, ""},
		{"its configuration file", []string{"mongod", "--config", "/etc/mongod.conf"}, "rs0"},
	} {
		t.Run(c.name, func(t *testing.T) {
			b := newBench(t)
			m := &mongo{set: c.asked}
			b.engine.AddLocalImage("mongo:8.0", dockertest.Image{Exec: m.exec, Config: docker.Config{Cmd: []string{"mongod"}}})
			b.write("data.archive", string(archiveOf(shop)))
			id := b.run("db", "mongo:8.0", "/data/db", c.cmd...)
			b.take(false)
			b.write("data.archive", "what changed since")
			_, _, err := b.runner.JudgeRestore(t.Context(), ref, resource.RestoreSource{Backup: "bkp_1", Change: "chg_1"})
			if err == nil || !strings.Contains(err.Error(), "db is a member of the replica set rs0, and Dockwell can't restore a member in place yet") ||
				!strings.Contains(err.Error(), "take --replSet out of its command") || !strings.Contains(err.Error(), "#373") {
				t.Fatalf("restoring into a member: %v", err)
			}
			if !b.running(id) || b.read("data.archive") != "what changed since" {
				t.Error("a refused restore changed something")
			}
		})
	}
}

func TestAMembersSetIsOnItsCommandLine(t *testing.T) {
	for want, cmd := range map[string][]string{
		"rs0": {"mongod", "--replSet", "rs0"}, "rs1": {"--replSet=rs1", "--port", "27018"}, "": {"mongod", "--replSet"},
	} {
		if got := replicaSetOf(cmd); got != want {
			t.Errorf("%q: %q, want %q", cmd, got, want)
		}
	}
	if replicaSetOf([]string{"mongod"}) != "" {
		t.Error("one that runs alone is a member")
	}
}
