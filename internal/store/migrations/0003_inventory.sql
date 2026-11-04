-- SPDX-License-Identifier: AGPL-3.0-only
-- What each server is, as its agent last reported: kept so the fleet shows
-- an offline server's OS and hardware, not a blank row. Both are protobuf
-- (dockwell.v1.ServerInventory and dockwell.v1.BuildInfo).
ALTER TABLE servers ADD COLUMN inventory BLOB;
ALTER TABLE servers ADD COLUMN agent_build BLOB;
