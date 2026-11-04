-- SPDX-License-Identifier: AGPL-3.0-only
-- A change's servers are written once, with its plan. The same rules are
-- in 0009_timeline.postgres.sql.

CREATE TRIGGER change_servers_never_change BEFORE UPDATE ON change_servers
BEGIN SELECT RAISE(ABORT, 'the servers of a change are fixed with its plan: plan a new change instead'); END;

CREATE TRIGGER change_servers_are_kept BEFORE DELETE ON change_servers
BEGIN SELECT RAISE(ABORT, 'the servers of a change are kept'); END;
