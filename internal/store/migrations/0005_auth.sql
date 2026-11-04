-- SPDX-License-Identifier: AGPL-3.0-only
-- People, how they sign in, and their sessions. See
-- docs/adr/0011-claim-and-sign-in.md and docs/security/sign-in.md.

-- People who sign in to the panel. The first is the owner, made only by
-- spending a claim link; the partial index below makes a second owner
-- impossible, whatever races.
CREATE TABLE users (
  id              TEXT PRIMARY KEY,
  email           TEXT NOT NULL,          -- lowercase
  name            TEXT NOT NULL,
  role            TEXT NOT NULL CHECK (role IN ('owner')),
  webauthn_handle BLOB NOT NULL UNIQUE,   -- random; passkeys name the user by it
  created_at      INTEGER NOT NULL,
  totp_secret     BLOB,                   -- sealed; set once a code confirmed it
  totp_pending    BLOB,                   -- sealed; awaiting a confirming code
  totp_last_step  INTEGER NOT NULL DEFAULT 0, -- newest time step accepted, so each code works once
  -- Codes refused without an audit event of their own (authenticator codes
  -- while code sign-in is locked, and wrong recovery codes), and when the
  -- last event that counted them was written: at most one an hour.
  codes_refused             INTEGER NOT NULL DEFAULT 0,
  codes_refused_reported_at INTEGER
);

CREATE UNIQUE INDEX users_email ON users (email);
CREATE UNIQUE INDEX users_one_owner ON users (role) WHERE role = 'owner';

-- Wrong authenticator codes, counted for every email address tried, whether
-- or not it has an account, so the answers don't say which do. Addresses
-- share 65,536 buckets by a hash of the email, which bounds the table however
-- many addresses are tried. See docs/security/sign-in.md.
CREATE TABLE code_guards (
  bucket    INTEGER PRIMARY KEY,
  failures  INTEGER NOT NULL DEFAULT 0, -- wrong codes in a row
  failed_at INTEGER,
  locked_at INTEGER                     -- set after too many wrong codes; only the owner lifts it
);

-- Passkeys. Each works only on the relying party ID (address) it was made on.
CREATE TABLE passkeys (
  id               TEXT PRIMARY KEY,
  user_id          TEXT NOT NULL REFERENCES users (id),
  credential_id    BLOB NOT NULL UNIQUE,
  rp_id            TEXT NOT NULL,
  name             TEXT NOT NULL,
  record           BLOB NOT NULL, -- the WebAuthn credential record (public key, sign count, flags), JSON
  created_at       INTEGER NOT NULL,
  last_used_at     INTEGER,
  clone_warning_at INTEGER        -- set when its sign count went backwards; it no longer signs in
);

CREATE INDEX passkeys_user ON passkeys (user_id);

-- One-time recovery codes for code sign-in. Only hashes are kept.
CREATE TABLE recovery_codes (
  user_id    TEXT NOT NULL REFERENCES users (id),
  code_hash  BLOB NOT NULL,
  created_at INTEGER NOT NULL,
  used_at    INTEGER,
  PRIMARY KEY (user_id, code_hash)
);

-- Signed-in browsers. Only a hash of the cookie's token is kept.
CREATE TABLE sessions (
  id           TEXT PRIMARY KEY,
  token_hash   BLOB NOT NULL UNIQUE,
  user_id      TEXT NOT NULL REFERENCES users (id),
  rp_id        TEXT NOT NULL, -- the address it was made on; it works only there
  method       TEXT NOT NULL, -- claim, passkey, authenticator_code, recovery_code, link
  user_agent   TEXT NOT NULL,
  created_ip   TEXT NOT NULL,
  last_ip      TEXT NOT NULL,
  created_at   INTEGER NOT NULL,
  last_seen_at INTEGER NOT NULL,
  expires_at   INTEGER NOT NULL, -- the absolute limit; the idle limit runs from last_seen_at
  revoked_at   INTEGER
);

CREATE INDEX sessions_user ON sessions (user_id);

-- One-time links: the claim link that creates the owner, and sign-in links
-- from dockwell recover. Only hashes are kept. A new link replaces older
-- unused ones for the same purpose and user.
CREATE TABLE link_tokens (
  id         TEXT PRIMARY KEY,
  purpose    TEXT NOT NULL CHECK (purpose IN ('claim', 'recover')),
  token_hash BLOB NOT NULL UNIQUE,
  user_id    TEXT REFERENCES users (id), -- whom a recover link signs in
  created_at INTEGER NOT NULL,
  created_by TEXT NOT NULL,
  expires_at INTEGER NOT NULL,
  used_at    INTEGER,
  revoked_at INTEGER
);
