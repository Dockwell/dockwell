// SPDX-License-Identifier: AGPL-3.0-only

import { createClient } from "@connectrpc/connect";
import { useTransport } from "@connectrpc/connect-query";
import {
  type BackupDestinationInfo,
  BackupService,
  ChangeService,
  type ConnectionInfo,
  ConnectionReach,
  LinkKind,
} from "@dockwell/api";
import { Button, Callout, Icon, PHONE_QUERY, StatusBadge, Table, type TableColumn, useMediaQuery } from "@dockwell/ui";
import { plural } from "@lingui/core/macro";
import { Trans, useLingui } from "@lingui/react/macro";
import { Link, useNavigate } from "@tanstack/react-router";
import { useMemo, useState } from "react";

import { sentence } from "../../lib/errors";
import { usePlaces } from "../apps/useServices";
import { useMay } from "../auth/may";
import { useDestinations } from "../backups/DestinationsPanel";
import { destinationsOn } from "../backups/storage";
import { useChangeSheet } from "../changes/ChangeSheet";
import { EmptyState } from "../shell/EmptyState";
import { Page } from "../shell/Page";
import { ConnectionSheet } from "./ConnectionSheet";
import { LastCheckCell, useLastCheck } from "./LastCheck";
import { type MailProviderId, mailPlaceOf, mailProvider } from "./mail";
import { hostOf, type ProviderId, placeOf, provider } from "./providers";
import { ServiceTiles } from "./ServiceTiles";
import { appsOf } from "./storage";
import { useConnections, useProjectLabel } from "./useConnections";
import "./Connections.css";

type ListRow =
  | { kind: "connection"; id: string; connection: ConnectionInfo }
  | { kind: "destination"; id: string; destination: BackupDestinationInfo };

/**
 * Every connection: the buckets apps keep files in, each with the apps that read
 * it, and the way to add one (Connections-List, ADR 0099). Standalone legacy
 * backup destinations are listed alongside connections as backup destinations
 * with an action to use them as a connection so totals match Integrations.
 * A connection is install-wide, so adding one is an admin's; below an admin,
 * the list holds only what the caller's projects may use, and only their own apps.
 */
export function ConnectionsPage() {
  const { t } = useLingui();
  const phone = useMediaQuery(PHONE_QUERY);
  const navigate = useNavigate();
  const may = useMay();
  const mayAdd = may(ChangeService.method.proposeChange, { install: true });
  const mayUseAsConnection = may(BackupService.method.useDestinationAsConnection);
  const sheet = useChangeSheet();
  const transport = useTransport();
  const backupClient = useMemo(() => createClient(BackupService, transport), [transport]);
  const query = useConnections();
  const destinations = useDestinations().data?.destinations ?? [];
  const connections = query.data?.connections ?? [];
  const standaloneDests = destinations.filter((d) => d.connection === "");
  const totalRows = connections.length + standaloneDests.length;
  const [adding, setAdding] = useState<ProviderId | MailProviderId | "">();
  const apps = new Set(connections.flatMap((c) => appsOf(c).map((a) => a.id))).size;
  const linkedBackupDests = destinations.filter(
    (d) => d.connection !== "" && connections.some((c) => c.name === d.connection),
  ).length;
  // One shape whatever is there: the connections, the backup destinations that hold
  // keys of their own, then what uses the connections.
  const countParts = [
    connections.length > 0 ? plural(connections.length, { one: "# connection", other: "# connections" }) : "",
    standaloneDests.length > 0
      ? plural(standaloneDests.length, {
          one: "# backup destination with its own keys",
          other: "# backup destinations with their own keys",
        })
      : "",
  ].filter(Boolean);
  const appsText = plural(apps, { one: "# app", other: "# apps" });
  const destsText = plural(linkedBackupDests, { one: "# backup destination", other: "# backup destinations" });
  const usage =
    apps > 0 && linkedBackupDests > 0
      ? t`used by ${appsText} and ${destsText}`
      : apps > 0
        ? plural(apps, { one: "used by # app", other: "used by # apps" })
        : linkedBackupDests > 0
          ? plural(linkedBackupDests, {
              one: "used by # backup destination",
              other: "used by # backup destinations",
            })
          : connections.length > 0
            ? t`no app uses them yet`
            : "";
  const lead = query.isPending || totalRows === 0 ? undefined : [...countParts, usage].filter(Boolean).join(" · ");
  const rows: ListRow[] = [
    ...connections.map((connection): ListRow => ({ kind: "connection", id: connection.id, connection })),
    ...standaloneDests.map(
      (destination): ListRow => ({ kind: "destination", id: `dest:${destination.id}`, destination }),
    ),
  ];
  const openRow = (row: ListRow) => {
    if (row.kind === "connection") {
      void navigate({ to: "/connections/$name", params: { name: row.connection.name } });
    } else {
      void navigate({ to: "/backups" });
    }
  };
  const convertToConnection = (d: BackupDestinationInfo) =>
    sheet.open({
      kind: "plan",
      // biome-ignore lint/correctness/useHookAtTopLevel: Connect RPC method name begins with "use", not a React hook
      make: () => backupClient.useDestinationAsConnection({ destination: d.name }),
    });
  const add = mayAdd.allowed ? (
    <Button variant="primary" icon="plus" onClick={() => setAdding("")} {...mayAdd.off}>
      <Trans>Add connection</Trans>
    </Button>
  ) : null;

  return (
    <Page
      title={t`Connections`}
      lead={lead}
      phoneSub={lead}
      back="/more"
      actions={totalRows > 0 ? add : null}
      phoneAction={
        // A header icon has no room for a reason: it's there only for whoever may add one.
        mayAdd.allowed && totalRows > 0 ? (
          <button
            type="button"
            className="mobile-header-button"
            aria-label={t`Add connection`}
            onClick={() => setAdding("")}
          >
            <Icon name="plus" size={20} />
          </button>
        ) : undefined
      }
    >
      {query.error ? (
        <Callout tone="critical" title={t`Dockwell can't read its connections`}>
          {sentence(query.error)} <Trans>The page tries again by itself.</Trans>
        </Callout>
      ) : null}
      {query.isPending ? null : totalRows === 0 && !query.error ? (
        <EmptyState
          icon="network"
          title={t`No connections yet`}
          className="connections-empty"
          choices={
            mayAdd.allowed ? (
              <div className="connections-tiles">
                <ServiceTiles onPick={(id) => setAdding(id)} pressed={false} />
              </div>
            ) : undefined
          }
        >
          {mayAdd.allowed ? (
            <Trans>A bucket's keys or a mail server's sign-in, pasted once, for any app to use</Trans>
          ) : (
            <Trans>An admin adds the buckets and mail servers your apps may use.</Trans>
          )}
        </EmptyState>
      ) : phone ? (
        <ul className="connections-phone">
          {rows.map((row) =>
            row.kind === "connection" ? (
              <li key={row.id}>
                <Link to="/connections/$name" params={{ name: row.connection.name }} className="connections-phone-row">
                  <Mark connection={row.connection} />
                  <span className="connections-phone-text">
                    <span className="connections-mono connections-name">{row.connection.name}</span>
                    <span className="connections-meta">
                      <UsedBy connection={row.connection} destinations={destinations} />
                    </span>
                    <PhoneCheck connection={row.connection} />
                  </span>
                </Link>
              </li>
            ) : (
              <li key={row.id} className="connections-phone-row connections-phone-dest">
                <Link to="/backups" className="connections-phone-main">
                  <DestinationMark destination={row.destination} />
                  <span className="connections-phone-text">
                    <span className="connections-mono connections-name">{row.destination.name}</span>
                    <span className="connections-meta">
                      <Trans>Backup destination</Trans>
                    </span>
                  </span>
                </Link>
                <Button
                  size="sm"
                  variant="secondary"
                  onClick={() => convertToConnection(row.destination)}
                  {...mayUseAsConnection.off}
                >
                  <Trans>Use as a connection</Trans>
                </Button>
              </li>
            ),
          )}
        </ul>
      ) : (
        <ConnectionsTable
          rows={rows}
          destinations={destinations}
          onOpen={openRow}
          onUseAsConnection={convertToConnection}
          useAsConnectionOff={mayUseAsConnection.off}
        />
      )}
      {adding !== undefined ? (
        <ConnectionSheet
          onClose={() => setAdding(undefined)}
          onSaved={(name) => void navigate({ to: "/connections/$name", params: { name } })}
          existing={connections}
          start={adding || undefined}
        />
      ) : null}
    </Page>
  );
}

function ConnectionsTable({
  rows,
  destinations,
  onOpen,
  onUseAsConnection,
  useAsConnectionOff,
}: {
  rows: readonly ListRow[];
  destinations: readonly BackupDestinationInfo[];
  onOpen: (row: ListRow) => void;
  onUseAsConnection: (d: BackupDestinationInfo) => void;
  useAsConnectionOff: { off?: string; disabled?: true };
}) {
  const { t } = useLingui();
  const projectName = useProjectLabel();
  const hasStandalone = rows.some((r) => r.kind === "destination");
  const admin = rows.some((r) => (r.kind === "connection" ? seesWhere(r.connection) : r.destination.endpoint !== ""));
  const columns: TableColumn<ListRow>[] = [
    {
      key: "name",
      header: t`Connection`,
      render: (row) =>
        row.kind === "connection" ? (
          <div className="connections-cell-name">
            <Mark connection={row.connection} />
            <div className="dw-cell">
              <Link
                to="/connections/$name"
                params={{ name: row.connection.name }}
                className="connections-mono connections-name"
              >
                {row.connection.name}
              </Link>
              <span className="connections-meta">{where(row.connection) || row.connection.displayName}</span>
            </div>
          </div>
        ) : (
          <div className="connections-cell-name">
            <DestinationMark destination={row.destination} />
            <div className="dw-cell">
              <Link to="/backups" className="connections-mono connections-name">
                {row.destination.name}
              </Link>
              <span className="connections-meta">{whereDestination(row.destination)}</span>
            </div>
          </div>
        ),
    },
    {
      key: "uses",
      header: t`Used by`,
      width: hasStandalone ? 200 : 260,
      render: (row) =>
        row.kind === "connection" ? (
          <UsedBy connection={row.connection} destinations={destinations} />
        ) : (
          <span>
            <Trans>Backups</Trans>
          </span>
        ),
    },
    // What the control plane's last checks found, from where each app reaches it (#799).
    {
      key: "check",
      header: t`Last check`,
      width: hasStandalone ? 220 : 260,
      render: (row) =>
        row.kind === "connection" ? (
          <LastCheckCell connection={row.connection} />
        ) : (
          <span className="connections-meta">
            <Trans>Backup destination</Trans>
          </span>
        ),
    },
  ];
  if (admin) {
    columns.push({
      key: "reach",
      header: t`Projects`,
      width: hasStandalone ? 150 : 180,
      render: (row) =>
        row.kind === "destination" ? (
          <span className="connections-meta">
            <Trans>Backups only</Trans>
          </span>
        ) : row.connection.reach === ConnectionReach.EVERY ? (
          <Trans>Every project</Trans>
        ) : row.connection.projects.length === 0 ? (
          <span className="connections-meta">
            <Trans>None yet</Trans>
          </span>
        ) : (
          row.connection.projects.map(projectName).join(", ")
        ),
    });
  }
  if (hasStandalone) {
    columns.push({
      key: "actions",
      header: (
        <span className="dw-sr">
          <Trans>Actions</Trans>
        </span>
      ),
      width: 190,
      align: "right",
      render: (row) =>
        row.kind === "destination" ? (
          <Button
            size="sm"
            variant="secondary"
            onClick={(e) => {
              e.stopPropagation();
              onUseAsConnection(row.destination);
            }}
            {...useAsConnectionOff}
          >
            <Trans>Use as a connection</Trans>
          </Button>
        ) : null,
    });
  }
  return <Table columns={columns} rows={rows} onRowClick={onOpen} caption={t`Connections`} />;
}

/** Whether the caller sees where a connection is, as an admin does. */
export function seesWhere(c: ConnectionInfo): boolean {
  return c.endpoint !== "" || c.host !== "";
}

/**
 * Where a connection's bucket is: its store, or its endpoint's host, and the bucket.
 * A mail server's is its service, or its host, and who mail comes from.
 */
export function where(c: ConnectionInfo): string {
  if (c.kind === LinkKind.SMTP) {
    const p = c.host ? mailProvider(mailPlaceOf(c.host).provider) : undefined;
    return [p?.name || c.host, c.sender].filter(Boolean).join(" · ");
  }
  if (!c.endpoint) return "";
  const p = provider(placeOf(c.endpoint, c.region).provider);
  return [p.name || hostOf(c.endpoint), c.bucket].filter(Boolean).join(" · ");
}

/** Where a legacy backup destination's bucket is: its store or host, and the bucket. */
function whereDestination(d: BackupDestinationInfo): string {
  if (!d.endpoint) return d.bucket;
  const p = provider(placeOf(d.endpoint).provider);
  return [p.name || hostOf(d.endpoint), d.bucket].filter(Boolean).join(" · ");
}

/** A connection's tile: its store's two letters. */
export function Mark({ connection }: { connection: ConnectionInfo }) {
  if (connection.kind === LinkKind.SMTP) {
    const m = connection.host ? mailProvider(mailPlaceOf(connection.host).provider) : undefined;
    return (
      <span className="connection-mark" aria-hidden>
        {m?.mark ?? "@"}
      </span>
    );
  }
  const p = connection.endpoint ? provider(placeOf(connection.endpoint, connection.region).provider) : undefined;
  // Another store's tile is a plus where it's picked; saved, it's S3 storage.
  return (
    <span className="connection-mark" aria-hidden>
      {p && p.id !== "other" ? p.mark : "S3"}
    </span>
  );
}

/** A standalone legacy backup destination's store tile. */
function DestinationMark({ destination }: { destination: BackupDestinationInfo }) {
  const p = destination.endpoint ? provider(placeOf(destination.endpoint).provider) : undefined;
  return (
    <span className="connection-mark" aria-hidden>
      {p && p.id !== "other" ? p.mark : "S3"}
    </span>
  );
}

/** A phone row's last check, in a line: the state, and when. */
function PhoneCheck({ connection }: { connection: ConnectionInfo }) {
  const last = useLastCheck(connection);
  if (!last) return null;
  return <StatusBadge status={last.status} label={last.label} meta={last.meta} labelIsState />;
}

/** How many apps or backup destinations use it, with their names. */
function UsedBy({
  connection,
  destinations,
}: {
  connection: ConnectionInfo;
  destinations: readonly BackupDestinationInfo[];
}) {
  const { t } = useLingui();
  const places = usePlaces();
  const reading = appsOf(connection);
  const backups = destinationsOn(connection.name, destinations);
  // A name two environments share says which each is.
  const apps = reading.map((a) =>
    reading.some((o) => o.id !== a.id && o.name === a.name) ? `${a.name} (${places.label(a.environment)})` : a.name,
  );
  // Shared, more than one app may use it.
  const shared = connection.shared
    ? ` · ${t({ message: "shared", context: "a connection more than one app may use" })}`
    : "";
  if (backups.length > 0 && apps.length === 0) {
    // When the single backup destination shares the connection's name, say Backups once instead of repeating the name.
    if (backups.length === 1 && backups[0]?.name === connection.name) {
      return (
        <span>
          <Trans>Backups</Trans>
        </span>
      );
    }
    const names = backups.map((d) => d.name).join(", ");
    return (
      <div className="dw-cell">
        <span>
          {plural(backups.length, {
            one: "Backups",
            other: "# backup destinations",
          })}
        </span>
        <span className="connections-mono connections-meta connections-apps" title={names}>
          {names}
        </span>
      </div>
    );
  }
  if (apps.length === 0) {
    return (
      <span className="connections-meta">
        <Trans>No app yet</Trans>
        {shared}
      </span>
    );
  }
  return (
    <div className="dw-cell">
      <span>
        {plural(apps.length, { one: "# app", other: "# apps" })}
        {shared}
      </span>
      <span className="connections-mono connections-meta connections-apps" title={apps.join(", ")}>
        {apps.join(", ") || t`none`}
      </span>
    </div>
  );
}
