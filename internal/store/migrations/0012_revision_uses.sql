-- SPDX-License-Identifier: AGPL-3.0-only
-- What each revision of a resource uses of others in its scope, such as the
-- variables and secrets a service uses: under which name, which kind and
-- name, and the revision it is pinned to. It holds references, never a
-- value, so it is kept in the clear beside a sealed spec, and finding what
-- uses a value, or listing an environment's values with their services,
-- opens no spec. The revision store writes it with each revision of a kind
-- that uses others, and like revisions it never changes. See
-- docs/adr/0022-secrets-encrypted-at-rest.md.
CREATE TABLE revision_uses (
  resource_id   TEXT NOT NULL,
  revision      INTEGER NOT NULL,
  scope         TEXT NOT NULL,    -- where the resource, and what it uses, live
  used_as       TEXT NOT NULL,    -- the name the resource sees it as, such as DATABASE_URL
  used_kind     TEXT NOT NULL,    -- such as variable or secret
  used_name     TEXT NOT NULL,
  used_revision INTEGER NOT NULL, -- the revision it is pinned to; 0 when it isn't
  PRIMARY KEY (resource_id, revision, used_as),
  FOREIGN KEY (resource_id, revision) REFERENCES revisions (resource_id, number)
);

CREATE INDEX revision_uses_used ON revision_uses (scope, used_kind, used_name);
