// SPDX-License-Identifier: AGPL-3.0-only

import { useQuery } from "@connectrpc/connect-query";
import {
  ChangeService,
  DockerCleanupKind,
  type DockerObject,
  DockerObjectKind,
  DockerOwnership,
  DockerService,
  type Server,
} from "@dockwell/api";
import {
  Button,
  Callout,
  Icon,
  Loading,
  Panel,
  SegmentedControl,
  SelectField,
  SkeletonRows,
  SlowLine,
  Table,
  type TableColumn,
  TextField,
  useMediaQuery,
} from "@dockwell/ui";
import { Trans, useLingui } from "@lingui/react/macro";
import { keepPreviousData } from "@tanstack/react-query";
import { Link } from "@tanstack/react-router";
import { useEffect, useState } from "react";
import { sentence } from "../../lib/errors";
import { formatBytes } from "../../lib/format";
import { useHeldRows } from "../../lib/loading";
import { useMay } from "../auth/may";
import { CopyButton } from "../servers/CopyButton";
import { EmptyState } from "../shell/EmptyState";
import { isHex64VolumeName, shortAnonymousVolumeName } from "./cleanupHelpers";
import { DockerPruneSheet } from "./DockerPruneSheet";
import { DockerStorageCreate } from "./DockerStorageCreate";
import { useDockerWatch } from "./live";
import { LiveUpdates, MeasuringSize, Unread } from "./parts";
import { StorageOwnership } from "./storageParts";
import { useVolumeMeasurements } from "./volumeMeasurements";
import "./DockerStorage.css";

type ServerName = Pick<Server, "name" | "id">;

export function NetworkPools({ row, summary = false }: { row: DockerObject; summary?: boolean }) {
  const { t, i18n } = useLingui();
  const net = row.detail.case === "network" ? row.detail.value : undefined;
  if (!net?.pools.length) return <span className="docker-storage-muted">{t`No pools reported`}</span>;
  const pools = summary ? net.pools.slice(0, 1) : net.pools;
  const remaining = net.pools.length - pools.length;
  return (
    <ul className="docker-storage-pools">
      {pools.map((pool) => (
        <li key={`${pool.subnet}/${pool.gateway}`}>
          <code>{pool.subnet}</code>
          {pool.gateway ? (
            <span className="docker-storage-muted">
              <Trans>
                Gateway <code>{pool.gateway}</code>
              </Trans>
            </span>
          ) : null}
        </li>
      ))}
      {remaining > 0 ? (
        <li className="docker-storage-muted">{t`+${remaining.toLocaleString(i18n.locale)} more`}</li>
      ) : null}
    </ul>
  );
}

function StorageMountpoint({ path }: { path: string }) {
  const { t } = useLingui();
  if (!path) return <span className="docker-muted">{t`Unavailable`}</span>;
  const keep = Math.min(22, Math.floor(path.length / 2));
  return (
    <code className="docker-storage-mountpoint" title={path}>
      <span>{path.slice(0, path.length - keep)}</span>
      <span>{path.slice(path.length - keep)}</span>
    </code>
  );
}

type StorageRow = DockerObject & { id: string };

export function DockerStorage({
  servers,
  fleet,
  volumes = false,
}: {
  servers: readonly ServerName[];
  fleet: boolean;
  volumes?: boolean;
}) {
  const { t, i18n } = useLingui();
  const compact = useMediaQuery("(max-width: 1150px)");
  const kind = volumes ? DockerObjectKind.VOLUME : DockerObjectKind.NETWORK;
  const may = useMay();
  const mayCreate = may(ChangeService.method.proposeChange).allowed;
  const mayPrune = may(DockerService.method.planDockerMaintenance).allowed;
  const [pruning, setPruning] = useState(false);
  const [input, setInput] = useState("");
  const [search, setSearch] = useState("");
  useEffect(() => {
    const value = input.trim();
    if (value === search) return;
    const timer = setTimeout(() => {
      setSearch(value);
      setPages([""]);
    }, 200);
    return () => clearTimeout(timer);
  }, [input, search]);
  const [driver, setDriver] = useState("");
  const [ownership, setOwnership] = useState(DockerOwnership.UNSPECIFIED);
  const [pages, setPages] = useState([""]);
  const [creating, setCreating] = useState(false);
  const token = pages.at(-1) ?? "";
  const ids = servers.map((s) => s.id);
  const query = useQuery(
    DockerService.method.listDockerObjects,
    {
      serverIds: ids,
      kind,
      pageToken: token,
      filter: { search: search.trim(), driver, ownership },
    },
    { retry: false, refetchOnWindowFocus: false, placeholderData: keepPreviousData },
  );
  const refresh = () => {
    if (token) setPages([""]);
    else void query.refetch();
  };
  const watch = useDockerWatch(ids, [], refresh, kind);
  const rows = (query.data?.objects ?? []).flatMap((row) =>
    row.key && (row.detail.case === "network" || row.detail.case === "volume")
      ? [{ ...row, id: `${row.key.serverId}/${row.key.engineId}` }]
      : [],
  );
  const measurement = useVolumeMeasurements(rows);
  const measuring = volumes && rows.some((row) => measurement(row) === "pending");
  const next = !query.isPlaceholderData && query.data?.nextPageToken;
  const unavailable = query.data?.unavailable ?? [];
  const filtered = !!(search || driver || ownership);
  const clear = () => {
    setPages([""]);
    setInput("");
    setSearch("");
    setDriver("");
    setOwnership(DockerOwnership.UNSPECIFIED);
  };
  const title = volumes ? t`Docker volumes` : t`Docker networks`;
  const loading = volumes ? t`Loading Docker volumes` : t`Loading Docker networks`;
  const slow = fleet ? t`Docker is slow to answer on these servers` : t`Docker is slow to answer on this server`;
  const held = useHeldRows(
    volumes ? "docker.volumes" : "docker.networks",
    query.isSuccess && !query.isPlaceholderData && !filtered && !token ? rows.length : undefined,
  );
  const owners = [
    { value: String(DockerOwnership.UNSPECIFIED), label: t`All` },
    { value: String(DockerOwnership.MANAGED), label: t`Managed by Dockwell` },
    { value: String(DockerOwnership.EXTERNAL), label: t`Not managed` },
    { value: String(DockerOwnership.SYSTEM), label: t`System` },
  ];
  const changeOwner = (value: string) => {
    setOwnership(Number(value));
    setPages([""]);
  };
  const find = volumes ? t`Find a volume` : t`Find a network`;
  const mountOrPools = volumes ? t`Mountpoint` : t`Address pools`;
  const sizeOrIPv6 = volumes ? t`Size` : "IPv6";
  const add = mayCreate ? (
    <Button icon="plus" size="sm" variant="primary" onClick={() => setCreating(true)}>
      {volumes ? t`New volume` : t`New network`}
    </Button>
  ) : null;
  const nameOf = (row: StorageRow) => {
    const serverId = row.key?.serverId ?? "";
    const engineId = row.key?.engineId ?? "";
    const isAnonVol =
      volumes && row.detail.case === "volume" && (row.detail.value.anonymous || isHex64VolumeName(row.name));
    const displayName = isAnonVol ? shortAnonymousVolumeName(row.name) : row.name;
    const content = (
      <>
        <Icon name={volumes ? "archive" : "network"} size={16} />
        {isAnonVol ? <code title={row.name}>{displayName}</code> : <span>{displayName}</span>}
      </>
    );
    return (
      <div className="docker-storage-name-cell">
        <div className="docker-volume-anon-name">
          {volumes ? (
            <Link
              className="dw-link docker-row-link docker-storage-name"
              to="/servers/$serverId/docker/volumes/$volumeKey"
              params={{ serverId, volumeKey: engineId }}
              title={isAnonVol ? row.name : undefined}
            >
              {content}
            </Link>
          ) : (
            <Link
              className="dw-link docker-row-link docker-storage-name"
              to="/servers/$serverId/docker/networks/$networkId"
              params={{ serverId, networkId: engineId }}
            >
              {content}
            </Link>
          )}
          {isAnonVol ? <CopyButton text={row.name} label={t`Copy volume name`} /> : null}
        </div>
        {row.detail.case === "volume" && row.detail.value.anonymous ? (
          <span className="docker-storage-muted">{t`Anonymous`}</span>
        ) : null}
        {fleet ? (
          <span className="docker-storage-muted">{servers.find((server) => server.id === serverId)?.name}</span>
        ) : null}
      </div>
    );
  };
  const driverOf = (row: StorageRow) =>
    row.detail.case === "network" || row.detail.case === "volume" ? row.detail.value.driver : "";
  const locationOf = (row: StorageRow) =>
    row.detail.case === "volume" ? (
      <StorageMountpoint path={row.detail.value.mountpoint} />
    ) : (
      <NetworkPools row={row} summary />
    );
  const metricOf = (row: StorageRow) => {
    if (row.detail.case === "network") return row.detail.value.ipv6Enabled ? t`On` : t`Off`;
    const measured = measurement(row);
    if (measured === "pending") return <MeasuringSize />;
    if (!measured) return t`Size unknown`;
    const at = formatMeasurement(measured.time.seconds, i18n.locale);
    return <span title={t`Measured ${at}`}>{formatBytes(measured.bytes, i18n.locale)}</span>;
  };
  const countOf = (row: StorageRow) => {
    if (row.detail.case !== "network" && row.detail.case !== "volume") return null;
    const facts = row.detail.value;
    return facts.attachmentCount === undefined
      ? t`Unknown`
      : facts.attachmentsPartial
        ? `${facts.attachmentCount}+`
        : facts.attachmentCount;
  };
  const columns: TableColumn<StorageRow>[] = [
    { key: "name", header: volumes ? t`Volume` : t`Network`, width: "25%", skeleton: "titled", render: nameOf },
    { key: "driver", header: t`Driver`, width: 88, render: (row) => <code>{driverOf(row)}</code> },
    { key: "location", header: mountOrPools, render: locationOf },
    { key: "metric", header: sizeOrIPv6, width: volumes ? 112 : 72, render: metricOf },
    { key: "count", header: t`Containers`, width: 108, align: "right", render: countOf },
    { key: "owner", header: t`Ownership`, width: 168, render: (row) => <StorageOwnership row={row} /> },
  ];
  return (
    <>
      <div className="docker-section-heading">
        <h2>{volumes ? t`Volumes` : t`Networks`}</h2>
        <div className="docker-heading-actions">
          <Button
            variant="ghost"
            size="sm"
            icon="refresh"
            onClick={refresh}
            loading={query.isFetching && !query.isPending}
            aria-label={t`Refresh`}
            title={t`Refresh`}
          />
          {add}
        </div>
      </div>
      <div className="docker-filters docker-storage-filters">
        {compact ? (
          <SelectField
            aria-label={t`Ownership`}
            value={String(ownership)}
            options={[{ value: "0", label: t`All ownership` }, ...owners.slice(1)]}
            onChange={(event) => changeOwner(event.target.value)}
          />
        ) : (
          <SegmentedControl label={t`Ownership`} value={String(ownership)} options={owners} onChange={changeOwner} />
        )}
        <SelectField
          aria-label={t`Driver`}
          value={driver}
          options={[
            { value: "", label: t`All drivers` },
            ...[
              ...new Set([
                ...(volumes ? ["local"] : ["bridge", "host", "null", "overlay", "macvlan", "ipvlan"]),
                ...rows.map(driverOf),
                ...(driver ? [driver] : []),
              ]),
            ]
              .sort()
              .map((value) => ({ value, label: value })),
          ]}
          onChange={(event) => {
            setDriver(event.target.value);
            setPages([""]);
          }}
        />
        <TextField
          icon="search"
          aria-label={find}
          placeholder={find}
          className="docker-storage-search"
          value={input}
          maxLength={255}
          onChange={(event) => setInput(event.target.value)}
        />
        {mayPrune ? (
          <Button
            className="docker-filters-action"
            icon="trash"
            size="sm"
            variant="secondary"
            onClick={() => setPruning(true)}
          >
            <Trans>Remove unused</Trans>
          </Button>
        ) : null}
      </div>
      <LiveUpdates paused={watch.paused} retry={watch.retry} />
      <Unread values={unavailable} servers={servers} retry={refresh} />
      {query.error ? (
        <Callout
          tone="warning"
          title={volumes ? t`Volumes couldn't be read` : t`Networks couldn't be read`}
          actions={<Button size="sm" onClick={refresh}>{t`Retry`}</Button>}
        >
          {sentence(query.error)}
        </Callout>
      ) : null}
      {query.isPending ? (
        compact ? (
          <Loading label={loading} slow={slow}>
            <Panel flush>
              <SkeletonRows rows={held} lead="icon" end="pill" />
            </Panel>
          </Loading>
        ) : (
          <Panel flush>
            <Table
              className="docker-storage-list-table"
              caption={title}
              columns={columns}
              rows={[]}
              loading
              loadingLabel={loading}
              loadingRows={held}
              slow={slow}
            />
          </Panel>
        )
      ) : null}
      {!query.isPending &&
      !query.isPlaceholderData &&
      !query.error &&
      !rows.length &&
      !next &&
      unavailable.length < servers.length ? (
        <EmptyState
          icon={volumes ? "archive" : "network"}
          title={
            volumes
              ? filtered
                ? t`No matching volumes`
                : t`No volumes`
              : filtered
                ? t`No matching networks`
                : t`No networks`
          }
          action={filtered ? <Button variant="secondary" onClick={clear}>{t`Clear filters`}</Button> : undefined}
        />
      ) : null}
      {measuring ? (
        <SlowLine
          slow={
            fleet ? t`Docker is measuring disk use on these servers` : t`Docker is measuring disk use on this server`
          }
        />
      ) : null}
      {rows.length ? (
        <Panel className="docker-storage-list" flush>
          {compact ? (
            <ul className="docker-card-list" aria-label={title}>
              {rows.map((row) => (
                <li key={row.id} className="docker-card">
                  <div className="docker-card-head">
                    {nameOf(row)}
                    <StorageOwnership row={row} />
                  </div>
                  <div className="docker-storage-attachment-meta">
                    <code>{driverOf(row)}</code>
                    <span>
                      {sizeOrIPv6} · {metricOf(row)}
                    </span>
                    <span>
                      {t`Containers`} · {countOf(row)}
                    </span>
                  </div>
                  {locationOf(row)}
                </li>
              ))}
            </ul>
          ) : (
            <Table className="docker-storage-list-table" caption={title} rows={rows} columns={columns} />
          )}
        </Panel>
      ) : null}
      <div className="docker-storage-actions">
        {token ? <Button variant="secondary" onClick={() => setPages(pages.slice(0, -1))}>{t`Previous`}</Button> : null}
        {next ? <Button variant="secondary" onClick={() => setPages([...pages, next])}>{t`Next`}</Button> : null}
      </div>
      {creating ? (
        <DockerStorageCreate volumes={volumes} servers={servers} onClose={() => setCreating(false)} refresh={refresh} />
      ) : null}
      {pruning ? (
        <DockerPruneSheet
          kind={volumes ? DockerCleanupKind.VOLUME : DockerCleanupKind.NETWORK}
          servers={servers}
          onClose={() => setPruning(false)}
          onApplied={refresh}
          onReviewAgain={() => setPruning(true)}
        />
      ) : null}
    </>
  );
}

export function formatMeasurement(seconds: bigint, locale: string) {
  return new Intl.DateTimeFormat(locale, { dateStyle: "medium", timeStyle: "short" }).format(Number(seconds) * 1000);
}
