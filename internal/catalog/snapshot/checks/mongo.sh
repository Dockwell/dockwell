# SPDX-License-Identifier: AGPL-3.0-only
# shellcheck shell=bash
# shellcheck disable=SC2034,SC2154 # Dockwell's scripts/catalog-e2e.sh sets what this reads and reads what this sets
#
# mongo's check, which Dockwell's scripts/catalog-e2e.sh sources to prove an install
# and an upgrade of it (Dockwell's ADR 0100): check_mongo first, once it's installed,
# and check_mongo upgraded, once it's upgraded from an earlier release. It answers,
# signs in with what Dockwell generated, and after an upgrade finds what it wrote.

# Its one release before, 8.0.34, doesn't start on CI's runners (earliest below).
upgrade_waived[mongo]="no MongoDB 8.0 before 8.0.38 starts on Linux 6.19 or newer, as CI's runners run (SERVER-121912); give it a before line once it pins 8.0.39 or later"

# No MongoDB 8.0 before 8.0.38 starts on Linux 6.19 or newer, as CI's runners run
# (SERVER-121912), so no release of the catalog's before it is proven from.
earliest[mongo]=8.0.38

check_mongo() {
  local db bound said
  db=$(container db)
  generated db MONGO_INITDB_ROOT_PASSWORD >/dev/null
  bound=$(docker inspect --format '{{json .HostConfig.PortBindings}}' "$db") || fail "docker inspect $db failed"
  [[ "$bound" == "{}" || "$bound" == null ]] || fail "MongoDB is published: $bound"
  # mongosh as the root user, with the password the container was given, which only
  # its own shell reads.
  # shellcheck disable=SC2016 # the container's shell reads the password.
  mongo() { docker exec "$db" sh -c 'exec mongosh --quiet --norc -u "$MONGO_INITDB_ROOT_USERNAME" -p "$MONGO_INITDB_ROOT_PASSWORD" --authenticationDatabase admin --eval "$1"' sh "$1"; }
  said=$(mongo 'print(db.adminCommand({ ping: 1 }).ok)') || fail "MongoDB doesn't take the root password it was given: $said"
  [[ "$said" == *1 ]] || fail "MongoDB answered '$said'"
  said=$(docker exec "$db" mongosh --quiet --norc -u root -p not-the-password --authenticationDatabase admin --eval 'print(1)' 2>&1) && fail "MongoDB took a wrong password: $said"
  # Someone who never signs in is answered nothing, as access control is on.
  said=$(docker exec "$db" mongosh --quiet --norc --eval 'print(JSON.stringify(db.adminCommand({ listDatabases: 1 })))' 2>&1) || true
  [[ "$said" != *'"databases"'* ]] || fail "MongoDB lists its databases to someone who never signed in: ${said:0:200}"
  if [[ "$1" == first ]]; then
    said=$(mongo 'print(db.getSiblingDB("e2e").kept.insertOne({ kept: true }).acknowledged)') || fail "MongoDB can't keep a document: $said"
  else
    said=$(mongo 'print(db.getSiblingDB("e2e").kept.countDocuments({ kept: true }))') || fail "MongoDB can't count what it kept: $said"
    [[ "$said" == *1 ]] || fail "what MongoDB kept didn't stay through the upgrade: $said"
  fi
}
