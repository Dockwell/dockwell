// SPDX-License-Identifier: AGPL-3.0-only

import { timestampDate, timestampMs } from "@bufbuild/protobuf/wkt";
import { useQuery } from "@connectrpc/connect-query";
import {
  ComposeService,
  type DockerObject,
  DockerObjectKind,
  DockerOwnership,
  DockerService,
  FileService,
} from "@dockwell/api";
import {
  Button,
  Callout,
  Icon,
  Loading,
  Panel,
  SkeletonRows,
  StatusBadge,
  Table,
  type TableColumn,
  Tag,
  TextField,
  useMediaQuery,
} from "@dockwell/ui";
import { Trans, useLingui } from "@lingui/react/macro";
import { keepPreviousData } from "@tanstack/react-query";
import { Link, useNavigate } from "@tanstack/react-router";
import { lazy, Suspense, useRef, useState } from "react";
import { sentence } from "../../lib/errors";
import { formatDateTime } from "../../lib/format";
import { useHeldRows } from "../../lib/loading";
import { useTickingNow, useTimeWords } from "../../lib/time";
import { useMay } from "../auth/may";
import { ActionMenu } from "../shell/ActionMenu";
import { EmptyState } from "../shell/EmptyState";
import { useListSearchShortcut } from "../shell/ListTools";
import { ComposeOperations } from "./ComposeOperations";
import { type ComposeSource, ComposeSourceSheet } from "./ComposeSourceSheet";
import { useDockerWatch } from "./live";
import { LiveUpdates } from "./parts";

import "./Compose.css";

const NewCompose = lazy(async () => ({ default: (await import("../apps/NewAppSheet")).NewAppSheet }));

export const stackPath = "/servers/$serverId/docker/composes/$composeId";

export function composeOf(row: DockerObject | undefined) {
  return row?.detail.case === "compose" ? row.detail.value : undefined;
}

export function ComposeStatus({ row }: { row: DockerObject }) {
  const { t } = useLingui();
  const compose = composeOf(row);
  const status =
    compose?.observationUnavailable || compose?.checking
      ? "unknown"
      : compose?.servicesNeedingAttention
        ? "degraded"
        : !compose?.runningServices
          ? "stopped"
          : compose.runningServices === compose.totalServices
            ? "healthy"
            : "degraded";
  const label = compose?.observationUnavailable
    ? t`Unavailable`
    : compose?.checking
      ? t`Checking`
      : compose?.servicesNeedingAttention
        ? t`Needs attention`
        : !compose?.runningServices
          ? t`Stopped`
          : compose.runningServices === compose.totalServices
            ? t`Running`
            : t`Partly running`;
  return <StatusBadge status={status} label={label} labelIsState />;
}

export function ComposeOwnership({ row }: { row: DockerObject }) {
  return row.ownership === DockerOwnership.MANAGED ? (
    <Tag words tone="cobalt">
      <Trans>Managed by Dockwell</Trans>
    </Tag>
  ) : (
    <Tag words>
      <Trans>Not managed</Trans>
    </Tag>
  );
}

export function DockerComposes({
  servers,
  serverId,
}: {
  servers: readonly { id: string; name: string }[];
  serverId?: string | undefined;
}) {
  const { t, i18n } = useLingui();
  const searchInput = useListSearchShortcut();
  const may = useMay();
  const navigate = useNavigate();
  const times = useTimeWords();
  const compact = useMediaQuery(serverId ? "(max-width: 1000px)" : "(max-width: 1140px)");
  const [search, setSearch] = useState("");
  const [outside, setOutside] = useState(false);
  const [tokens, setTokens] = useState([""]);
  const [source, setSource] = useState<"folder" | "new">();
  const upload = useRef<HTMLInputElement>(null);
  const [file, setFile] = useState<ComposeSource & { server?: string | undefined }>();
  const query = useQuery(
    DockerService.method.listDockerObjects,
    {
      serverIds: servers.map((server) => server.id),
      kind: DockerObjectKind.COMPOSE,
      filter: { search, ownership: outside ? DockerOwnership.EXTERNAL : DockerOwnership.UNSPECIFIED },
      pageSize: 20,
      pageToken: tokens.at(-1) ?? "",
    },
    // A new search, filter or page keeps the last list in place until its answer lands.
    { retry: false, refetchOnWindowFocus: false, placeholderData: keepPreviousData },
  );
  const watch = useDockerWatch(
    servers.map((server) => server.id),
    [],
    () => void query.refetch(),
    DockerObjectKind.COMPOSE,
  );
  const rows = query.data?.objects.filter((row) => row.key !== undefined) ?? [];
  const held = useHeldRows("docker.composes", query.isSuccess && !query.isPlaceholderData ? rows.length : undefined);
  const now = useTickingNow(
    rows.reduce((last, row) => Math.max(last, row.observedAt ? timestampMs(row.observedAt) : 0), 0),
  );
  const reset = () => setTokens([""]);
  const refresh = () => {
    reset();
    if (tokens.length === 1) void query.refetch();
  };
  const nameOfServer = (id: string | undefined) => servers.find((item) => item.id === id)?.name ?? "";
  const link = (row: DockerObject) => (
    <Link
      className="dw-link compose-row-title"
      title={row.name}
      to={stackPath}
      params={{ serverId: row.key?.serverId ?? "", composeId: row.key?.engineId ?? "" }}
    >
      <Icon name="layers" size={17} />
      <span className="compose-clip">{row.name}</span>
    </Link>
  );
  const counts = (row: DockerObject) => {
    const body = composeOf(row);
    if (body?.observationUnavailable || body?.checking) return t`${body.totalServices} placed`;
    const running = body?.runningServices ?? 0;
    const total = body?.totalServices ?? 0;
    return t`${running} / ${total} running`;
  };
  const directory = (row: DockerObject) => {
    const path = row.ownership === DockerOwnership.EXTERNAL ? composeOf(row)?.sourceDirectory : undefined;
    return path ? (
      <code className="compose-clip" title={path}>
        {path}
      </code>
    ) : null;
  };
  const changed = (row: DockerObject) => {
    if (!row.observedAt) return <span className="compose-muted">{t`Unavailable`}</span>;
    const at = timestampDate(row.observedAt);
    const date = formatDateTime(at, i18n.locale);
    const title = row.ownership === DockerOwnership.EXTERNAL ? t`Newest container created ${date}` : date;
    return (
      <time dateTime={at.toISOString()} title={title}>
        {times.ago(at, now)}
      </time>
    );
  };
  const operations = (row: DockerObject) => {
    const params = { serverId: row.key?.serverId ?? "", composeId: row.key?.engineId ?? "" };
    const environment = composeOf(row)?.environmentId;
    return (
      <ComposeOperations
        row={row}
        open={() => void navigate({ to: stackPath, params })}
        takeOn={
          row.ownership === DockerOwnership.EXTERNAL && canTake
            ? () => void navigate({ to: stackPath, params, search: { action: "take-on" } })
            : undefined
        }
        remove={
          environment && may(ComposeService.method.planComposeWorkspace, { environment }).allowed
            ? () => void navigate({ to: stackPath, params, search: { action: "delete" } })
            : undefined
        }
        onWorkload={(action) => void navigate({ to: stackPath, params, search: { action } })}
      />
    );
  };
  const columns: TableColumn<DockerObject & { tableId: string }>[] = [
    { key: "name", header: t`Compose project`, width: "20%", render: link },
    ...(serverId
      ? []
      : [
          {
            key: "server",
            width: "12%",
            header: t`Server`,
            render: (row: DockerObject) => {
              const server = nameOfServer(row.key?.serverId);
              return (
                <span className="compose-clip" title={server}>
                  {server}
                </span>
              );
            },
          },
        ]),
    { key: "state", header: t`State`, width: "14%", render: (row) => <ComposeStatus row={row} /> },
    { key: "services", header: t`Services`, width: "11%", render: counts, skeleton: "short" },
    {
      key: "ownership",
      header: t`Ownership`,
      width: 152,
      render: (row) => <ComposeOwnership row={row} />,
      skeleton: "pill",
    },
    { key: "directory", header: t`Source directory`, render: directory, skeleton: "mono" },
    { key: "changed", header: t`Last change`, width: 102, render: changed },
    { key: "actions", header: <span className="dw-sr">{t`Actions`}</span>, width: 56, render: operations },
  ];
  const canImport = may(ComposeService.method.importCompose).allowed;
  const canTake = may(ComposeService.method.adoptCompose).allowed;
  const openNew = () => {
    setFile(undefined);
    setSource("new");
  };
  return (
    <section className="compose-list" aria-label={t`Composes`}>
      <div className="compose-list-head">
        <div className="compose-list-filter">
          <TextField
            ref={searchInput}
            icon="search"
            aria-label={t`Find a Compose project`}
            value={search}
            placeholder={t`Find a Compose project`}
            onChange={(event) => {
              setSearch(event.target.value);
              reset();
            }}
          />
          {outside ? (
            <Button
              size="sm"
              variant="ghost"
              icon="x"
              onClick={() => {
                setOutside(false);
                reset();
              }}
            >
              <Trans>Show all projects</Trans>
            </Button>
          ) : null}
        </div>
        <div className="compose-actions">
          {canImport ? (
            <input
              type="file"
              ref={upload}
              className="dw-sr"
              tabIndex={-1}
              aria-hidden="true"
              accept=".yml,.yaml,text/yaml,application/yaml"
              onChange={(event) => {
                const selected = event.target.files?.[0];
                event.target.value = "";
                if (selected)
                  void selected.text().then((yaml) => {
                    setFile({ file: yaml, directory: "", server: serverId });
                    setSource("new");
                  });
              }}
            />
          ) : null}
          <Button
            variant="ghost"
            size="sm"
            icon="refresh"
            aria-label={t`Refresh`}
            onClick={refresh}
            loading={query.isFetching && !query.isPending}
          />
          {canImport || canTake ? (
            <ActionMenu
              label={t`Import`}
              text={<Trans>Import</Trans>}
              icon="upload"
              actions={[
                ...(canImport
                  ? [
                      {
                        id: "upload",
                        label: t`Upload a file`,
                        icon: "upload" as const,
                        onSelect: () => upload.current?.click(),
                      },
                    ]
                  : []),
                ...(canImport && may(FileService.method.listFiles).allowed && may(FileService.method.readFile).allowed
                  ? [
                      {
                        id: "folder",
                        label: t`Server folder`,
                        icon: "folder" as const,
                        onSelect: () => setSource("folder"),
                      },
                    ]
                  : []),
                ...(canTake
                  ? [
                      {
                        id: "take",
                        label: t`Existing project`,
                        icon: "layers" as const,
                        onSelect: () => {
                          setOutside(true);
                          reset();
                        },
                      },
                    ]
                  : []),
              ]}
            />
          ) : null}
          {may(ComposeService.method.listComposeTemplates).allowed ? (
            <Link className="dw-link" to="/apps/templates">
              <Trans>Templates</Trans>
            </Link>
          ) : null}
          {canImport ? (
            <Button size="sm" variant="primary" icon="plus" onClick={openNew}>
              <Trans>New Compose</Trans>
            </Button>
          ) : null}
        </div>
      </div>
      <LiveUpdates paused={watch.paused} retry={watch.retry} />
      {query.error ? (
        <Callout tone="critical" title={t`Can't read Compose projects`}>
          {sentence(query.error)}
        </Callout>
      ) : null}
      {query.data?.unavailable.length ? (
        <Callout tone="warning" title={t`Some Engine observations are unavailable`}>
          {query.data.unavailable
            .map((item) => nameOfServer(item.serverId))
            .filter(Boolean)
            .join(", ")}
        </Callout>
      ) : null}
      {rows.length ? (
        compact ? (
          <ul className="compose-cards">
            {rows.map((row) => (
              <li key={`${row.key?.serverId}/${row.key?.engineId}`}>
                <div className="compose-card-title">
                  {link(row)}
                  {operations(row)}
                </div>
                <div className="compose-row-meta">
                  <ComposeStatus row={row} />
                  <span>{counts(row)}</span>
                </div>
                <div className="compose-row-meta">
                  <ComposeOwnership row={row} />
                  {!serverId ? <span>{nameOfServer(row.key?.serverId)}</span> : null}
                  {changed(row)}
                </div>
                {directory(row)}
              </li>
            ))}
          </ul>
        ) : (
          <Table
            className="compose-table"
            columns={columns}
            rows={rows.map((row) => ({ ...row, tableId: `${row.key?.serverId}/${row.key?.engineId}` }))}
            rowKey="tableId"
            caption={t`Compose projects`}
          />
        )
      ) : query.isPending ? (
        compact ? (
          <Loading label={t`Loading Compose projects`} slow={t`Docker is slow to answer on this server`}>
            <Panel flush>
              <SkeletonRows rows={held} end="pill" />
            </Panel>
          </Loading>
        ) : (
          <Table
            className="compose-table"
            columns={columns}
            rows={[]}
            caption={t`Compose projects`}
            loading
            loadingRows={held}
            loadingLabel={t`Loading Compose projects`}
            slow={t`Docker is slow to answer on this server`}
          />
        )
      ) : !query.error ? (
        <EmptyState icon="layers" title={outside ? t`No outside Compose projects` : t`No Compose projects found`} />
      ) : null}
      {tokens.length > 1 || query.data?.nextPageToken ? (
        <div className="compose-actions compose-paging">
          <Button size="sm" onClick={() => setTokens(tokens.slice(0, -1))} disabled={tokens.length === 1}>
            <Trans>Previous page</Trans>
          </Button>
          <Button
            size="sm"
            onClick={() => setTokens([...tokens, query.data?.nextPageToken ?? ""])}
            disabled={!query.data?.nextPageToken}
          >
            <Trans>Next page</Trans>
          </Button>
        </div>
      ) : null}
      {source === "folder" ? (
        <ComposeSourceSheet
          servers={servers}
          onClose={() => setSource(undefined)}
          onRead={(data, server) => {
            setFile({ ...data, server });
            setSource("new");
          }}
        />
      ) : null}
      {source === "new" ? (
        <Suspense fallback={null}>
          <NewCompose
            source="compose"
            fixedSource
            initialServer={file?.server ?? serverId}
            initialCompose={file}
            onSource={() => {}}
            onClose={() => {
              setSource(undefined);
              refresh();
            }}
          />
        </Suspense>
      ) : null}
    </section>
  );
}
