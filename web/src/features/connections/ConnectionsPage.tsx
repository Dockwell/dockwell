// SPDX-License-Identifier: AGPL-3.0-only

import { ChangeService, type ConnectionInfo, ConnectionReach, LinkKind } from "@dockwell/api";
import { Button, Callout, Icon, PHONE_QUERY, Table, type TableColumn, useMediaQuery } from "@dockwell/ui";
import { plural } from "@lingui/core/macro";
import { Trans, useLingui } from "@lingui/react/macro";
import { Link, useNavigate } from "@tanstack/react-router";
import { useState } from "react";

import { sentence } from "../../lib/errors";
import { usePlaces } from "../apps/useServices";
import { useMay } from "../auth/may";
import { EmptyState } from "../shell/EmptyState";
import { Page } from "../shell/Page";
import { ConnectionSheet } from "./ConnectionSheet";
import { type MailProviderId, mailPlaceOf, mailProvider } from "./mail";
import { hostOf, type ProviderId, placeOf, provider } from "./providers";
import { ServiceTiles } from "./ServiceTiles";
import { appsOf } from "./storage";
import { useConnections, useProjectLabel } from "./useConnections";
import "./Connections.css";

/**
 * Every connection: the buckets apps keep files in, each with the apps that read
 * it, and the way to add one (Connections-List, ADR 0099). A connection is
 * install-wide, so adding one is an admin's; below an admin, the list holds only
 * what the caller's projects may use, and only their own apps.
 */
export function ConnectionsPage() {
  const { t } = useLingui();
  const phone = useMediaQuery(PHONE_QUERY);
  const navigate = useNavigate();
  const may = useMay();
  const mayAdd = may(ChangeService.method.proposeChange, { install: true });
  const query = useConnections();
  const connections = query.data?.connections ?? [];
  const [adding, setAdding] = useState<ProviderId | MailProviderId | "">();
  const apps = new Set(connections.flatMap((c) => appsOf(c).map((a) => a.id))).size;
  const lead = query.isPending
    ? undefined
    : connections.length === 0
      ? undefined
      : [
          plural(connections.length, { one: "# connection", other: "# connections" }),
          apps === 0 ? t`no app uses them yet` : plural(apps, { one: "used by # app", other: "used by # apps" }),
        ].join(" · ");
  const open = (c: ConnectionInfo) => void navigate({ to: "/connections/$name", params: { name: c.name } });
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
      actions={connections.length > 0 ? add : null}
      phoneAction={
        // A header icon has no room for a reason: it's there only for whoever may add one.
        mayAdd.allowed && connections.length > 0 ? (
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
      {query.isPending ? null : connections.length === 0 && !query.error ? (
        <EmptyState
          icon="key"
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
          {connections.map((c) => (
            <li key={c.id}>
              <Link to="/connections/$name" params={{ name: c.name }} className="connections-phone-row">
                <Mark connection={c} />
                <span className="connections-phone-text">
                  <span className="connections-mono connections-name">{c.name}</span>
                  <span className="connections-meta">
                    <UsedBy connection={c} />
                  </span>
                </span>
              </Link>
            </li>
          ))}
        </ul>
      ) : (
        <ConnectionsTable connections={connections} onOpen={open} />
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
  connections,
  onOpen,
}: {
  connections: readonly ConnectionInfo[];
  onOpen: (c: ConnectionInfo) => void;
}) {
  const { t } = useLingui();
  const projectName = useProjectLabel();
  const admin = connections.some(seesWhere);
  const columns: TableColumn<ConnectionInfo>[] = [
    {
      key: "name",
      header: t`Connection`,
      render: (c) => (
        <div className="connections-cell-name">
          <Mark connection={c} />
          <div className="dw-cell">
            <Link to="/connections/$name" params={{ name: c.name }} className="connections-mono connections-name">
              {c.name}
            </Link>
            <span className="connections-meta">{where(c) || c.displayName}</span>
          </div>
        </div>
      ),
    },
    { key: "uses", header: t`Used by`, width: 320, render: (c) => <UsedBy connection={c} /> },
  ];
  if (admin) {
    columns.push({
      key: "reach",
      header: t`Projects`,
      width: 220,
      render: (c) =>
        c.reach === ConnectionReach.EVERY ? (
          <Trans>Every project</Trans>
        ) : c.projects.length === 0 ? (
          <span className="connections-meta">
            <Trans>None yet</Trans>
          </span>
        ) : (
          c.projects.map(projectName).join(", ")
        ),
    });
  }
  return <Table columns={columns} rows={connections} onRowClick={onOpen} caption={t`Connections`} />;
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

/** How many apps read it, with their names. */
function UsedBy({ connection }: { connection: ConnectionInfo }) {
  const { t } = useLingui();
  const places = usePlaces();
  const reading = appsOf(connection);
  // A name two environments share says which each is.
  const apps = reading.map((a) =>
    reading.some((o) => o.id !== a.id && o.name === a.name) ? `${a.name} (${places.label(a.environment)})` : a.name,
  );
  // Shared, more than one app may use it.
  const shared = connection.shared
    ? ` · ${t({ message: "shared", context: "a connection more than one app may use" })}`
    : "";
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
