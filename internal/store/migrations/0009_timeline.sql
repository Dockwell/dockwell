-- SPDX-License-Identifier: AGPL-3.0-only
-- What the event timeline needs. See docs/adr/0020-the-event-timeline.md.
--
-- Each change names the servers its plan touches, removed ones included,
-- so the timeline can show what happened on one server. Like the plan,
-- they are written once, when the change is planned; the dialect files
-- keep them that way. The timeline also finds changes by the resources
-- they touch and by when they started applying, or were planned until
-- then, and lists the audit log in time order.

CREATE TABLE change_servers (
  change_id TEXT NOT NULL REFERENCES changes (id),
  server_id TEXT NOT NULL,
  PRIMARY KEY (change_id, server_id)
);
CREATE INDEX change_servers_server ON change_servers (server_id, change_id);

-- The changes planned before this kept their servers only in their plans.
-- Those that applied placed their revisions on them, which is enough to
-- find them.
INSERT INTO change_servers (change_id, server_id)
  SELECT DISTINCT v.change_id, d.server_id
  FROM revisions v JOIN deployments d ON d.resource_id = v.resource_id AND d.revision = v.number;

CREATE INDEX change_items_resource ON change_items (resource_id);
CREATE INDEX change_items_scope ON change_items (scope);
CREATE INDEX changes_timeline ON changes ((COALESCE(applied_at, created_at)), id);
CREATE INDEX audit_events_timeline ON audit_events (at, id);

-- Each audit event names who or what it acted on, other than its actor,
-- in target: the server that joined, the API token or passkey made or
-- removed, the person a sign-in link is for. Its writer records that, and
-- where the event came from in surface, which until now only changes
-- recorded. Events recorded before this kept a token's or passkey's ID in
-- their free text, as their writers put it, so they are filled in from it
-- here, once, and their surface from their actor; nothing reads the text
-- again.
ALTER TABLE audit_events ADD COLUMN target TEXT NOT NULL DEFAULT '';
UPDATE audit_events SET target = subject WHERE action LIKE 'server.%';
UPDATE audit_events SET target = substr(detail, 7, 30)
  WHERE action IN ('token.create', 'token.revoke') AND detail LIKE 'token tok%';
UPDATE audit_events SET target = substr(detail, 1, 30)
  WHERE action IN ('passkey.add', 'passkey.remove') AND detail LIKE 'pky%';
UPDATE audit_events SET target = subject
  WHERE target = '' AND subject LIKE 'usr%' AND actor <> ('user:' || subject);
UPDATE audit_events SET surface = CASE
    WHEN actor LIKE 'user:%' OR actor LIKE 'anonymous:%' THEN 'panel'
    WHEN actor LIKE 'token:%' THEN 'api'
    WHEN actor LIKE 'cli:%' OR actor LIKE 'server:%' AND action = 'server.join' THEN 'cli'
    ELSE 'automatic'
  END
  WHERE surface = '';

-- Resources list oldest first, by when their IDs were made, which is the
-- ULID that ends each ID, whatever its kind's prefix.
CREATE INDEX resources_oldest ON resources ((substr(id, length(id) - 25)), id);
