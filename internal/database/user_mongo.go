// SPDX-License-Identifier: AGPL-3.0-only

package database

import (
	"encoding/json"
)

// An app's own user on a MongoDB, which the image's root user makes, as it makes
// nothing else: a user of the app's database, which signs in there, and owns that
// database alone (dbOwner), so it reaches no other app's on a server they share
// (ADR 0084). Each script signs in as the root user the image made, from the
// container's own environment, as a reading does, and says what it found as one
// line of JSON. A password goes to the server in the command that sets it, which
// MongoDB keeps out of its log.

// mongoSession connects and signs in as the root user, and names the database and
// the user, quoted as JSON, which holds any name CheckSQLName takes.
func mongoSession(user, db string) string {
	return `const env = process.env;
const m = new Mongo("mongodb://127.0.0.1:" + (env.DW_PORT || "27017") + "/?directConnection=true&serverSelectionTimeoutMS=5000&connectTimeoutMS=5000");
const admin = m.getDB("admin");
if (env.MONGO_INITDB_ROOT_USERNAME) admin.auth(env.MONGO_INITDB_ROOT_USERNAME, env.MONGO_INITDB_ROOT_PASSWORD || "");
const name = ` + jsString(user) + `;
const target = m.getDB(` + jsString(db) + `);
const roles = [{ role: "dbOwner", db: target.getName() }];
`
}

// mongoMake makes the user, owning its database, or gives it its password anew,
// with that role alone.
func mongoMake(user, db, password string) string {
	return mongoSession(user, db) + `const pwd = ` + jsString(password) + `;
if (target.getUser(name)) target.updateUser(name, { pwd, roles });
else target.createUser({ user: name, pwd, roles });
print(JSON.stringify({ made: true }));
`
}

// mongoThere says whether the server has the user, and owns its database as it.
func mongoThere(user, db string) string {
	return mongoSession(user, db) + `const u = target.getUser(name);
const owns = !!u && (u.roles || []).some((r) => r.role === "dbOwner" && r.db === target.getName());
print(JSON.stringify({ user: !!u, owner: owns ? name : "" }));
`
}

// mongoDrop drops the user and its database, with everything in it.
func mongoDrop(user, db string) string {
	return mongoSession(user, db) + `if (target.getUser(name)) target.dropUser(name);
target.dropDatabase();
print(JSON.stringify({ dropped: true }));
`
}

// mongoOpen says whether the image made a root user, whether the server answers at
// all, and whether it answers one who never signs in, as one without access control
// does. One that refuses the connection, or isn't found, isn't answering yet, as
// while its image makes its root user; one that refuses the command answers, closed.
const mongoOpen = `const env = process.env;
let answering = true;
let open = false;
try {
  const m = new Mongo("mongodb://127.0.0.1:" + (env.DW_PORT || "27017") + "/?directConnection=true&serverSelectionTimeoutMS=5000&connectTimeoutMS=5000");
  open = m.getDB("admin").runCommand({ listDatabases: 1, nameOnly: true }).ok === 1;
} catch (e) {
  const name = String((e && e.name) || "");
  if (/Network|ServerSelection|Timeout/.test(name) || /ECONNREFUSED|ECONNRESET|connect/i.test(String((e && e.message) || ""))) answering = false;
}
print(JSON.stringify({ root: !!env.MONGO_INITDB_ROOT_USERNAME, answering, open }));
`

// jsString is s as a JavaScript string.
func jsString(s string) string {
	b, _ := json.Marshal(s)
	return string(b)
}
