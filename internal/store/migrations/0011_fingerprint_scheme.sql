-- SPDX-License-Identifier: AGPL-3.0-only
-- How each change's fingerprint was made. Before ADR 0022 it hashed every
-- spec as it is; since, it digests a spec that may hold a secret with a key
-- derived from the install's key. A change planned before the upgrade is
-- checked as it was fingerprinted, so it applies while its plan holds. See
-- docs/adr/0022-secrets-encrypted-at-rest.md.
--
--   1  every spec hashed with SHA-256, as planned before the upgrade
--   2  specs of kinds that may hold secrets digested with HMAC-SHA256

ALTER TABLE changes ADD COLUMN fingerprint_scheme INTEGER NOT NULL DEFAULT 1;
