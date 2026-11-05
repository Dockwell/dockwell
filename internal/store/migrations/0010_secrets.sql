-- SPDX-License-Identifier: AGPL-3.0-only
-- Envelope encryption for specs that may hold secrets, such as a secret's
-- value or a container's environment. See
-- docs/adr/0022-secrets-encrypted-at-rest.md.
--
-- Each such spec is stored sealed under a data key of its own. Data keys
-- are kept here wrapped under a key-encryption key, which is kept sealed
-- with the install's key, in a file beside the database and never in it.
-- Revisions and change items never change, so rotating the
-- key-encryption key wraps every data key again, here, and leaves them be.

-- Key-encryption keys. The newest that isn't retired wraps new data keys.
CREATE TABLE key_encryption_keys (
  id         TEXT PRIMARY KEY,
  sealed     BLOB NOT NULL,   -- the key, sealed with the install's key; empty once retired
  created_at INTEGER NOT NULL,
  retired_at INTEGER          -- set once a newer one wraps every data key
);

-- Data keys, each sealing one stored spec.
CREATE TABLE data_keys (
  id         TEXT PRIMARY KEY,
  kek_id     TEXT NOT NULL REFERENCES key_encryption_keys (id),
  wrapped    BLOB NOT NULL,   -- the data key, sealed under the key-encryption key
  created_at INTEGER NOT NULL
);

CREATE INDEX data_keys_kek ON data_keys (kek_id, id);
