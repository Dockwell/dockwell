-- SPDX-License-Identifier: AGPL-3.0-only
-- Renewal rotates the server's key and accepts only its current certificate,
-- so a copied key can't keep renewing alongside the real server. The
-- previous serial lets an agent retry a renewal whose answer it never got.
ALTER TABLE servers ADD COLUMN prev_cert_serial TEXT NOT NULL DEFAULT '';
ALTER TABLE servers ADD COLUMN cert_key_hash BLOB NOT NULL DEFAULT x'';
