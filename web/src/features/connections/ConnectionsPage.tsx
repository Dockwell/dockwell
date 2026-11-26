// SPDX-License-Identifier: AGPL-3.0-only

import { createClient } from "@connectrpc/connect";
import { useTransport } from "@connectrpc/connect-query";
import {
  type AlertChannelInfo,
  AlertService,
  type BackupDestinationInfo,
  BackupService,
  ChangeService,
  type ConnectionInfo,
  ConnectionReach,
  LinkKind,
} from "@dockwell/api";
import {
  Button,
  Callout,
  Icon,
  Loading,
  PHONE_QUERY,
  SelectField,
  SkeletonRows,
  StatusBadge,
  Table,
  type TableColumn,
  useMediaQuery,
} from "@dockwell/ui";
import { plural } from "@lingui/core/macro";
import { Trans, useLingui } from "@lingui/react/macro";
import { Link, useNavigate } from "@tanstack/react-router";
import { useMemo, useState } from "react";

import { sentence } from "../../lib/errors";
import { channelConnection, channelsOn } from "../alerts/mail";
import { useChannels } from "../alerts/useAlerts";
import { usePlaces } from "../apps/useServices";
import { useMay } from "../auth/may";
import { useDestinations } from "../backups/DestinationsPanel";
import { destinationsOn } from "../backups/storage";
import { useChangeSheet } from "../changes/ChangeSheet";
import { ActionMenu, type MenuAction } from "../shell/ActionMenu";
import { EmptyState } from "../shell/EmptyState";
import {
  ListPager,
  type ListPlace,
  ListSearchField,
  ListSortField,
  SortHeader,
  useListPlace,
  useRowMenu,
} from "../shell/ListTools";
import { type ListSearch, pagedRows, sortedRows } from "../shell/list";
import { Page } from "../shell/Page";
import { ConnectionSheet } from "./ConnectionSheet";
import { LastCheckCell, useLastCheck } from "./LastCheck";
import { type MailProviderId, mailPlaceOf, mailProvider } from "./mail";
import { ConnectionMark, DestinationMark } from "./ProviderMark";
import { hostOf, type ProviderId, placeOf, provider } from "./providers";
import { ServiceTiles } from "./ServiceTiles";
import { appsOf } from "./storage";
import { useConnections, useProjectLabel } from "./useConnections";
import "./Connections.css";

type ListRow =
  | { kind: "connection"; id: string; connection: ConnectionInfo }
  | { kind: "destination"; id: string; destination: BackupDestinationInfo };

/** The list's place, in its URL: its filter, its sort and its page. */
export interface ConnectionsSearch extends ListSearch {
  kind?: "mail" | "storage" | "destination";
}

function rowKind(row: ListRow): NonNullable<ConnectionsSearch["kind"]> {
  return row.kind === "destination" ? "destination" : row.connection.kind === LinkKind.SMTP ? "mail" : "storage";
}

/** The columns the list sorts by, as its address names them. */
const SORT_KEYS = ["name"] as const;

/** Maximum consumer names shown inline before collapsing into "and N more". */
const USED_BY_PREVIEW = 3;

/** A row's name, and where it is, as the list filters and sorts by them. */
function rowWords(row: ListRow): string {
  return row.kind === "connection"
    ? `${row.connection.name} ${row.connection.displayName} ${where(row.connection)}`
    : `${row.destination.name} ${whereDestination(row.destination)}`;
}

function rowName(row: ListRow): string {
  return row.kind === "connection" ? row.connection.name : row.destination.name;
}

/**
 * Every connection: the buckets apps keep files in, each with the apps that read
 * it, and the way to add one (Connections-List, ADR 0099). Standalone legacy
 * backup destinations are listed alongside connections as backup destinations
 * with an action to use them as a connection so totals match Integrations.
 * A connection is install-wide, so adding one is an admin's; below an admin,
 * the list holds only what the caller's projects may use, and only their own apps.
 */
export function ConnectionsPage({
  search = {},
  onSearch,
}: {
  search?: ConnectionsSearch;
  onSearch?: (next: ConnectionsSearch) => void;
}) {
  const { t, i18n } = useLingui();
  const phone = useMediaQuery(PHONE_QUERY);
  const navigate = useNavigate();
  const may = useMay();
  const mayAdd = may(ChangeService.method.proposeChange, { install: true });
  const mayUseAsConnection = may(BackupService.method.useDestinationAsConnection);
  const mayListChannels = may(AlertService.method.listAlertChannels).allowed;
  const sheet = useChangeSheet();
  const transport = useTransport();
  const backupClient = useMemo(() => createClient(BackupService, transport), [transport]);
  const query = useConnections();
  const destinations = useDestinations().data?.destinations ?? [];
  const channels = useChannels(mayListChannels).data?.channels ?? [];
  const connections = query.data?.connections ?? [];
  const standaloneDests = destinations.filter((d) => d.connection === "");
  const totalRows = connections.length + standaloneDests.length;
  const [editing, setEditing] = useState<ConnectionInfo>();
  const [adding, setAdding] = useState<ProviderId | MailProviderId | "">();
  const apps = new Set(connections.flatMap((c) => appsOf(c).map((a) => a.id))).size;
  const linkedBackupDests = destinations.filter(
    (d) => d.connection !== "" && connections.some((c) => c.name === d.connection),
  ).length;
  const linkedAlertChannels = channels.filter((ch) => {
    const conn = channelConnection(ch);
    return conn !== "" && connections.some((c) => c.name === conn);
  }).length;
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
  const userParts = [
    apps > 0 ? plural(apps, { one: "# app", other: "# apps" }) : "",
    linkedBackupDests > 0
      ? plural(linkedBackupDests, { one: "# backup destination", other: "# backup destinations" })
      : "",
    linkedAlertChannels > 0 ? plural(linkedAlertChannels, { one: "# alert channel", other: "# alert channels" }) : "",
  ].filter(Boolean);
  const usageList =
    userParts.length > 1
      ? new Intl.ListFormat(i18n.locale, { style: "long", type: "conjunction" }).format(userParts)
      : "";
  const usage =
    userParts.length > 1
      ? t`used by ${usageList}`
      : apps > 0
        ? plural(apps, { one: "used by # app", other: "used by # apps" })
        : linkedBackupDests > 0
          ? plural(linkedBackupDests, {
              one: "used by # backup destination",
              other: "used by # backup destinations",
            })
          : linkedAlertChannels > 0
            ? plural(linkedAlertChannels, {
                one: "used by # alert channel",
                other: "used by # alert channels",
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
  const place = useListPlace(search, onSearch, SORT_KEYS);
  const shown = sortedRows(
    rows.filter(
      (row) => (!search.kind || rowKind(row) === search.kind) && rowWords(row).toLowerCase().includes(place.query),
    ),
    place.sort,
    rowName,
  );
  const page = pagedRows(shown, place.page);
  const kinds = new Set(rows.map(rowKind));
  const kindLabel = (kind: NonNullable<ConnectionsSearch["kind"]>) =>
    kind === "mail" ? t`Mail` : kind === "storage" ? t`Object storage` : t`Backup destination`;
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
  const removeOff = (connection: ConnectionInfo) => {
    if (!mayAdd.allowed) return mayAdd.off;
    const apps = connection.uses.flatMap((use) => (use.apps.length ? use.apps : [use.link]));
    const names = [
      ...apps,
      ...destinationsOn(connection.name, destinations).map((d) => d.name),
      ...channelsOn(connection.name, channels).map((c) => c.name),
    ].filter(Boolean);
    const unique = [...new Set(names)];
    if (unique.length === 0) return {};
    const shown = unique.slice(0, USED_BY_PREVIEW).join(", ");
    const extra = unique.length - USED_BY_PREVIEW;
    const usedBy = extra > 0 ? t`${shown} and ${extra} more` : shown;
    return { off: t`Used by ${usedBy}. Disconnect it there first.` };
  };
  /** A row's menu: open it, and for a destination with keys of its own, use it as a connection. */
  const actionsFor = (row: ListRow): MenuAction[] => [
    { id: "open", icon: "network", label: t`Open`, onSelect: () => openRow(row) },
    ...(row.kind === "destination"
      ? [
          {
            id: "use",
            icon: "link" as const,
            label: t`Use as a connection`,
            ...(mayUseAsConnection.allowed
              ? {}
              : { off: mayUseAsConnection.reason || undefined, disabled: !mayUseAsConnection.reason }),
            onSelect: () => convertToConnection(row.destination),
          },
        ]
      : !mayAdd.allowed || seesWhere(row.connection)
        ? [
            {
              id: "edit",
              label: t`Edit`,
              icon: "edit" as const,
              ...mayAdd.off,
              onSelect: () => setEditing(row.connection),
            },
            {
              id: "remove",
              label: t`Remove`,
              icon: "trash" as const,
              danger: true,
              ...removeOff(row.connection),
              onSelect: () => {
                const name = row.connection.name;
                sheet.open({
                  kind: "propose",
                  summary: t`Remove the connection ${name}`,
                  edits: [{ resource: { kind: "connection", name }, delete: true }],
                });
              },
            },
          ]
        : []),
  ];
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
      {totalRows > 0 ? (
        <div className="list-bar">
          <ListSearchField
            label={t`Filter connections`}
            placeholder={t`Filter by name`}
            size={phone ? "lg" : "sm"}
            value={place.typed}
            onChange={place.setTyped}
          />
          {phone ? <ListSortField place={place} columns={{ name: t`Connection` }} /> : null}
          {kinds.size > 1 || search.kind ? (
            <SelectField
              size="sm"
              aria-label={t`Type`}
              value={search.kind ?? ""}
              options={[
                { value: "", label: t`Every type` },
                ...[...kinds].map((kind) => ({ value: kind, label: kindLabel(kind) })),
              ]}
              onChange={(event) => {
                const value = event.target.value;
                onSearch?.({
                  ...search,
                  page: undefined,
                  kind: value === "mail" || value === "storage" || value === "destination" ? value : undefined,
                });
              }}
            />
          ) : null}
        </div>
      ) : null}
      {query.isPending ? (
        phone ? (
          <Loading label={t`Loading connections`}>
            <SkeletonRows rows={3} lead="mark" className="dw-panel" />
          </Loading>
        ) : (
          <ConnectionsTable
            rows={[]}
            destinations={destinations}
            channels={channels}
            onOpen={openRow}
            onUseAsConnection={convertToConnection}
            useAsConnectionOff={mayUseAsConnection.off}
            place={place}
            actionsFor={actionsFor}
            loading
          />
        )
      ) : totalRows === 0 && !query.error ? (
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
      ) : shown.length === 0 && !query.error ? (
        <EmptyState
          icon="search"
          title={t`Nothing matches`}
          action={
            <Button
              size="lg"
              onClick={() => {
                place.clearQuery();
                onSearch?.({ ...search, q: undefined, page: undefined, kind: undefined });
              }}
            >
              <Trans>Clear the filter</Trans>
            </Button>
          }
        >
          <p>
            {place.query ? (
              <Trans>
                No connection here has <code>{place.typed.trim()}</code> in its name.
              </Trans>
            ) : null}
          </p>
        </EmptyState>
      ) : phone ? (
        <>
          <ul className="connections-phone">
            {page.rows.map((row) =>
              row.kind === "connection" ? (
                <li key={row.id} className="connections-phone-item">
                  <Link
                    to="/connections/$name"
                    params={{ name: row.connection.name }}
                    className="connections-phone-row"
                  >
                    <ConnectionMark connection={row.connection} />
                    <span className="connections-phone-text">
                      <span className="connections-mono connections-name">{row.connection.name}</span>
                      <span className="connections-meta">
                        <UsedBy connection={row.connection} destinations={destinations} channels={channels} plain />
                      </span>
                      <PhoneCheck connection={row.connection} />
                    </span>
                  </Link>
                  <ActionMenu label={t`Actions for ${rowName(row)}`} actions={actionsFor(row)} />
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
          <ListPager page={page} onPage={place.setPage} />
        </>
      ) : (
        <>
          <ConnectionsTable
            rows={page.rows}
            destinations={destinations}
            channels={channels}
            onOpen={openRow}
            onUseAsConnection={convertToConnection}
            useAsConnectionOff={mayUseAsConnection.off}
            place={place}
            actionsFor={actionsFor}
          />
          <ListPager page={page} onPage={place.setPage} />
        </>
      )}
      {editing ? (
        <ConnectionSheet onClose={() => setEditing(undefined)} existing={connections} editing={editing} />
      ) : null}
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
  channels,
  onOpen,
  onUseAsConnection,
  useAsConnectionOff,
  place,
  actionsFor,
  loading = false,
}: {
  /** The connections are on their way: the table draws their rows' shapes under its header. */
  loading?: boolean;
  rows: readonly ListRow[];
  destinations: readonly BackupDestinationInfo[];
  channels: readonly AlertChannelInfo[];
  onOpen: (row: ListRow) => void;
  onUseAsConnection: (d: BackupDestinationInfo) => void;
  useAsConnectionOff: { off?: string; disabled?: true };
  place: ListPlace;
  actionsFor: (row: ListRow) => MenuAction[];
}) {
  const { t } = useLingui();
  const projectName = useProjectLabel();
  const menu = useRowMenu<ListRow>();
  const hasStandalone = rows.some((r) => r.kind === "destination");
  const admin = rows.some((r) => (r.kind === "connection" ? seesWhere(r.connection) : r.destination.endpoint !== ""));
  const columns: TableColumn<ListRow>[] = [
    {
      key: "name",
      header: <SortHeader label={t`Connection`} sortKey="name" place={place} />,
      sort: place.sortOf("name"),
      render: (row) =>
        row.kind === "connection" ? (
          <div className="connections-cell-name">
            <ConnectionMark connection={row.connection} />
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
          <UsedBy connection={row.connection} destinations={destinations} channels={channels} />
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
  columns.push({
    key: "actions",
    header: (
      <span className="dw-sr">
        <Trans>Actions</Trans>
      </span>
    ),
    width: hasStandalone ? 238 : 48,
    align: "right",
    render: (row) => (
      <span className="connections-row-actions">
        {row.kind === "destination" ? (
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
        ) : null}
        <ActionMenu label={t`Actions for ${rowName(row)}`} actions={actionsFor(row)} />
      </span>
    ),
  });
  return (
    <>
      <Table
        columns={columns}
        rows={rows}
        onRowClick={onOpen}
        onRowContextMenu={(row, at, el) =>
          menu.open(row, at, el, { label: t`Actions for ${rowName(row)}`, actions: actionsFor(row) })
        }
        caption={t`Connections`}
        loading={loading}
        loadingLabel={t`Loading connections`}
      />
      {menu.element}
    </>
  );
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

/** A phone row's last check, in a line: the state, and when. */
function PhoneCheck({ connection }: { connection: ConnectionInfo }) {
  const last = useLastCheck(connection);
  if (!last) return null;
  return <StatusBadge status={last.status} label={last.label} meta={last.meta} labelIsState />;
}

function UsedByNames({ names, plain = false }: { names: readonly string[]; plain?: boolean }) {
  const { t } = useLingui();
  const [expanded, setExpanded] = useState(false);
  const full = names.join(", ");
  if (names.length <= USED_BY_PREVIEW) {
    return (
      <span className="connections-mono connections-meta connections-apps" title={full}>
        {full || t`none`}
      </span>
    );
  }
  const extra = names.length - USED_BY_PREVIEW;
  const shown = names.slice(0, USED_BY_PREVIEW).join(", ");
  if (plain) {
    return (
      <span className="connections-mono connections-meta connections-apps" title={full}>
        {t`${shown} and ${extra} more`}
      </span>
    );
  }
  return (
    <span
      className={
        expanded
          ? "connections-mono connections-meta connections-apps is-expanded"
          : "connections-mono connections-meta connections-apps"
      }
      title={full}
    >
      {expanded ? full : shown}{" "}
      <button
        type="button"
        className="connections-more-btn"
        aria-expanded={expanded}
        onClick={(event) => {
          event.stopPropagation();
          event.preventDefault();
          setExpanded((was) => !was);
        }}
      >
        {expanded ? t`Show fewer` : t`and ${extra} more`}
      </button>
    </span>
  );
}

/** How many apps, backup destinations, or alert channels use it, with their names. */
function UsedBy({
  connection,
  destinations,
  channels,
  plain = false,
}: {
  connection: ConnectionInfo;
  destinations: readonly BackupDestinationInfo[];
  channels: readonly AlertChannelInfo[];
  plain?: boolean;
}) {
  const { t } = useLingui();
  const places = usePlaces();
  const reading = appsOf(connection);
  const backups = destinationsOn(connection.name, destinations);
  const alertChannels = channelsOn(connection.name, channels);
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
    const names = backups.map((d) => d.name);
    return (
      <div className="dw-cell">
        <span>
          {plural(backups.length, {
            one: "Backups",
            other: "# backup destinations",
          })}
        </span>
        <UsedByNames names={names} plain={plain} />
      </div>
    );
  }
  if (alertChannels.length > 0 && apps.length === 0) {
    // When the single alert channel shares the connection's name, say Alerts once instead of repeating the name.
    if (alertChannels.length === 1 && alertChannels[0]?.name === connection.name) {
      return (
        <span>
          <Trans>Alerts</Trans>
        </span>
      );
    }
    const names = alertChannels.map((ch) => ch.name);
    return (
      <div className="dw-cell">
        <span>
          {plural(alertChannels.length, {
            one: "Alerts",
            other: "# alert channels",
          })}
        </span>
        <UsedByNames names={names} plain={plain} />
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
      <UsedByNames names={apps} plain={plain} />
    </div>
  );
}
