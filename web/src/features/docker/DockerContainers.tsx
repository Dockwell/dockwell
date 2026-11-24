// SPDX-License-Identifier: AGPL-3.0-only

import { timestampMs } from "@bufbuild/protobuf/wkt";
import { useQuery } from "@connectrpc/connect-query";
import {
  ChangeService,
  ComposeService,
  type ContainerPort,
  ContainerService,
  ContainerState,
  type DockerContainer,
  DockerHealth,
  type DockerObject,
  DockerObjectKind,
  DockerOwnership,
  type DockerSample,
  DockerService,
  ImportService,
  LogService,
  type Server,
  TerminalService,
} from "@dockwell/api";
import {
  Button,
  Callout,
  Loading,
  Panel,
  SegmentedControl,
  SelectField,
  SkeletonRows,
  StatusBadge,
  Table,
  type TableColumn,
  Tag,
  TextField,
  useMediaQuery,
} from "@dockwell/ui";
import { Trans, useLingui } from "@lingui/react/macro";
import { Link, useNavigate } from "@tanstack/react-router";
import { useEffect, useState } from "react";
import { reasonOf, sentence } from "../../lib/errors";
import { formatBytes, formatPercent } from "../../lib/format";
import { RouterButtonLink, RouterTagLink } from "../../lib/links";
import { useHeldRows } from "../../lib/loading";
import { useTimeWords } from "../../lib/time";
import { useWorkloadControls } from "../apps/workload";
import { useMay } from "../auth/may";
import { statusOf, useContainerStateWord } from "../servers/containers";
import { useControlPlaneNow } from "../servers/useFleet";
import { ActionMenu, type MenuAction } from "../shell/ActionMenu";
import { EmptyState } from "../shell/EmptyState";
import { useListSearchShortcut, useRowMenu } from "../shell/ListTools";
import { DockerTakeOn } from "./DockerTakeOn";
import { dockerKey, rowWithHealth, useDockerWatch } from "./live";
import { containerOf, LiveUpdates, Unread } from "./parts";

type State = "all" | "running" | "stopped" | "attention";
const shellTarget = "/servers/$serverId/terminal";
const detailTarget = "/servers/$serverId/docker/containers/$containerId";

export function ContainerStatus({
  row,
  state = row?.state ?? ContainerState.UNSPECIFIED,
}: {
  row?: DockerObject;
  state?: ContainerState;
}) {
  const { t } = useLingui();
  const stateWord = useContainerStateWord();
  if (state === ContainerState.RUNNING) {
    switch (containerOf(row)?.health) {
      case DockerHealth.HEALTHY:
        return <StatusBadge status="healthy" label={t`Healthy`} title={t`Healthy`} labelIsState />;
      case DockerHealth.UNHEALTHY:
        return <StatusBadge status="degraded" label={t`Unhealthy`} title={t`Unhealthy`} labelIsState />;
      case DockerHealth.STARTING:
        return <StatusBadge status="deploying" label={t`Starting`} title={t`Starting`} labelIsState />;
      default:
        return <StatusBadge status="healthy" label={t`Running`} title={t`Running`} labelIsState />;
    }
  }
  const label = stateWord(state);
  return <StatusBadge status={statusOf(state)} label={label} title={label} labelIsState />;
}

export function Ownership({ row }: { row: DockerObject }) {
  const { t } = useLingui();
  if (row.ownership === DockerOwnership.MANAGED) {
    const label = t`Managed by Dockwell`;
    if (row.managedResourceId && row.managedResource?.kind === "service") {
      return (
        <RouterTagLink
          words
          tone="cobalt"
          title={label}
          to="/apps/$serviceId"
          params={{ serviceId: row.managedResourceId }}
        >
          {label}
        </RouterTagLink>
      );
    }
    return (
      <Tag words tone="cobalt">
        <span className="docker-cell-text" title={label}>
          {label}
        </span>
      </Tag>
    );
  }
  const label = row.ownership === DockerOwnership.SYSTEM ? t`System` : t`Not managed`;
  return (
    <Tag words>
      <span className="docker-cell-text" title={label}>
        {label}
      </span>
    </Tag>
  );
}

export function dockerPort(port: ContainerPort): string {
  const bind = port.address.includes(":") ? `[${port.address}]` : port.address;
  const host = bind ? `${bind}:${port.serverPort}` : String(port.serverPort);
  return `${host} → ${port.containerPort}${port.protocol === "tcp" ? "" : `/${port.protocol}`}`;
}

function ValuePreview({ values }: { values: readonly string[] }) {
  const { t } = useLingui();
  return values.length ? (
    <span className="docker-port-preview" title={values.join(", ")}>
      {values.slice(0, 2).map((value) => (
        <code className="docker-cell-text" key={value}>
          {value}
        </code>
      ))}
      {values.length > 2 ? <span className="docker-cell-text">{t`+${values.length - 2} more`}</span> : null}
    </span>
  ) : (
    <span className="docker-muted docker-cell-text" title={t`None`}>
      <Trans>None</Trans>
    </span>
  );
}

function ImageReference({ image }: { image: string | undefined }) {
  return image ? (
    <code className="docker-image-ref docker-cell-text" title={image}>
      {image}
    </code>
  ) : null;
}

function Usage({ sample, now }: { sample?: DockerSample | undefined; now: number }) {
  const { i18n, t } = useLingui();
  if (!sample?.observedAt || Math.max(0, now - timestampMs(sample.observedAt)) > 25_000) {
    return (
      <span className="docker-muted docker-cell-text" title={t`No sample`}>
        <Trans>No sample</Trans>
      </span>
    );
  }
  const cpu = sample.cpuPercent === undefined ? undefined : formatPercent(sample.cpuPercent, i18n.locale);
  const memory = sample.memoryBytes === undefined ? undefined : formatBytes(sample.memoryBytes, i18n.locale);
  const usage = [cpu === undefined ? undefined : `${cpu} ${t`CPU`}`, memory].filter(Boolean).join(" · ");
  return (
    <span className="docker-usage" title={usage}>
      {cpu !== undefined ? (
        <span>
          {cpu} <Trans>CPU</Trans>
        </span>
      ) : null}
      {cpu !== undefined && memory !== undefined ? " " : null}
      {memory !== undefined ? (
        <span>
          {memory}{" "}
          <span className="dw-sr">
            <Trans>memory</Trans>
          </span>
        </span>
      ) : null}
    </span>
  );
}

interface ContainerActionProps {
  row: DockerObject;
  stack?: (project: string) => void;
  takeOnMode?: "menu" | "primary" | "choose";
  shortcuts?: boolean;
  workloadServers?: number | undefined;
  onApplied?: () => void;
  onTakeOn?: (row: DockerObject) => void;
}

export function ContainerActions(props: ContainerActionProps) {
  const actionsFor = useContainerActions();
  return <ContainerActionButtons {...actionsFor(props)} />;
}

function useContainerActions() {
  const { t } = useLingui();
  const may = useMay();
  const navigate = useNavigate();
  const { changeRun, restartInstance } = useWorkloadControls();
  return ({
    row,
    stack,
    takeOnMode = "menu",
    shortcuts = true,
    workloadServers = 1,
    onApplied,
    onTakeOn,
  }: ContainerActionProps) => {
    const key = row.key;
    const label = t`Actions for ${row.name}`;
    if (!key) return { label, actions: [], row };
    const managed = row.managedResource;
    const canTake =
      onTakeOn && row.ownership === DockerOwnership.EXTERNAL && may(ContainerService.method.adoptContainer).allowed;
    const showTakeOn = canTake && takeOnMode !== "menu";
    const showShortcuts = shortcuts && (takeOnMode !== "choose" || !canTake);
    const params = { serverId: key.serverId, containerId: key.engineId };
    const log =
      may(LogService.method.watchLogs).allowed &&
      row.ownership !== DockerOwnership.SYSTEM &&
      (row.ownership !== DockerOwnership.MANAGED ||
        (!!row.managedResourceId && row.managedResource?.kind === "service"));
    const terminal = may(TerminalService.method.openTerminal).allowed && row.state === ContainerState.RUNNING;
    const actions: MenuAction[] = [
      {
        id: "inspect",
        label: t`Inspect`,
        icon: "eye",
        onSelect: () => void navigate({ to: detailTarget, params, search: { view: "inspect", name: row.name } }),
      },
    ];
    if (row.managedResourceId && row.managedResource?.kind === "service") {
      const serviceId = row.managedResourceId;
      actions.push({
        id: "app",
        label: t`Open app`,
        icon: "service",
        onSelect: () => void navigate({ to: "/apps/$serviceId", params: { serviceId } }),
      });
    }
    if (canTake && !showTakeOn) {
      actions.push({
        id: "take-on",
        label: t`Take on`,
        icon: "plus",
        onSelect: () => onTakeOn?.(row),
      });
    }
    if (managed && row.managedRevision > 0n && row.managedResourceId) {
      const where = managed.kind === "container" ? { install: true as const } : { environment: managed.scope };
      const active = [ContainerState.RUNNING, ContainerState.PAUSED, ContainerState.RESTARTING].includes(row.state);
      const mayPlan = may(ChangeService.method.proposeChange, where);
      if (mayPlan.allowed) {
        actions.unshift({
          id: active ? "stop" : "start",
          label:
            workloadServers > 1
              ? active
                ? t`Stop workload on ${workloadServers} servers`
                : t`Start workload on ${workloadServers} servers`
              : active
                ? t`Stop workload`
                : t`Start workload`,
          icon: active ? "stop" : "play",
          onSelect: () => changeRun(managed, row.managedRevision, !active, onApplied),
        });
      }
      if (
        (row.state === ContainerState.RUNNING || row.state === ContainerState.RESTARTING) &&
        may(ChangeService.method.restartService, where).allowed
      ) {
        actions.splice(mayPlan.allowed ? 1 : 0, 0, {
          id: "restart",
          label: t`Restart this container`,
          icon: "refresh",
          onSelect: () => restartInstance(row.managedResourceId, key.serverId, key.engineId, onApplied),
        });
      }
    }
    const project = containerOf(row)?.composeProject;
    if (stack && project) {
      actions.push({
        id: "compose",
        label: t`Filter by this Compose project`,
        icon: "filter",
        onSelect: () => stack(project),
      });
    }
    return { label, actions, row, params, objectKey: key, log, terminal, showTakeOn, showShortcuts, onTakeOn };
  };
}

function ContainerActionButtons({
  label,
  actions,
  row,
  params,
  objectKey,
  log,
  terminal,
  showTakeOn,
  showShortcuts,
  onTakeOn,
}: ReturnType<ReturnType<typeof useContainerActions>>) {
  const { t } = useLingui();
  if (!objectKey) return null;
  return (
    <div className="docker-row-actions">
      {showTakeOn ? (
        <Button icon="plus" variant="secondary" size="sm" onClick={() => onTakeOn?.(row)}>
          <Trans>Take on</Trans>
        </Button>
      ) : null}
      {log && showShortcuts ? (
        <RouterButtonLink
          to={detailTarget}
          params={params}
          search={{ view: "logs", name: row.name }}
          icon="file"
          variant="ghost"
          size="sm"
          aria-label={t`Logs for ${row.name}`}
          title={t`Logs`}
        />
      ) : null}
      {terminal && showShortcuts ? (
        <RouterButtonLink
          to={shellTarget}
          params={{ serverId: objectKey.serverId }}
          search={{ containerId: objectKey.engineId, containerTitle: row.name }}
          icon="terminal"
          variant="ghost"
          size="sm"
          aria-label={t`Terminal for ${row.name}`}
          title={t`Terminal`}
        />
      ) : null}
      <ActionMenu actions={actions} label={label} />
    </div>
  );
}

/** While the next first page loads, keep only rows that still fit the selection. */
function matchesSelection(
  row: DockerObject,
  selection: {
    search: string;
    state: State;
    ownership: DockerOwnership;
    project: string;
  },
): boolean {
  const container = containerOf(row);
  const running = row.state === ContainerState.RUNNING;
  const attention =
    row.state === ContainerState.RESTARTING ||
    (row.state === ContainerState.EXITED && !!container?.exitCode) ||
    container?.health === DockerHealth.UNHEALTHY;
  return (
    (selection.state !== "running" || running) &&
    (selection.state !== "stopped" || !running) &&
    (selection.state !== "attention" || attention) &&
    (!selection.ownership || selection.ownership === row.ownership) &&
    (!selection.project || selection.project === container?.composeProject) &&
    [row.name, container?.image, row.key?.engineId, container?.composeProject]
      .join(" ")
      .toLowerCase()
      .includes(selection.search.toLowerCase())
  );
}

export function DockerContainers({
  servers,
  fleet,
  attention = false,
}: {
  servers: readonly Pick<Server, "id" | "name">[];
  fleet: boolean;
  attention?: boolean | undefined;
}) {
  const { t } = useLingui();
  const searchInput = useListSearchShortcut();
  const may = useMay();
  const navigate = useNavigate();
  const serverId = !fleet || servers.length === 1 ? servers[0]?.id : undefined;
  const [choosingContainer, setChoosingContainer] = useState(false);
  const [takeOn, setTakeOn] = useState<DockerObject>();

  const compact = useMediaQuery("(max-width: 1150px)");
  const [typed, setTyped] = useState("");
  const [search, setSearch] = useState("");
  const [state, setState] = useState<State>(attention ? "attention" : "all");
  const [ownership, setOwnership] = useState(DockerOwnership.UNSPECIFIED);
  const [project, setProject] = useState("");
  const [pages, setPages] = useState<string[]>([""]);
  const [changed, setChanged] = useState(false);
  const cursor = pages.at(-1) ?? "";
  const ids = servers.map((s) => s.id);
  const query = useQuery(
    DockerService.method.listDockerObjects,
    {
      serverIds: ids,
      kind: DockerObjectKind.CONTAINER,
      filter: {
        search,
        state: state === "running" ? ContainerState.RUNNING : ContainerState.UNSPECIFIED,
        stopped: state === "stopped",
        needsAttention: state === "attention",
        ownership,
        composeProject: project,
      },
      pageToken: cursor,
      pageSize: 16,
    },
    {
      retry: false,
      refetchOnWindowFocus: false,
      // The query key changes for filters, even when a visible container
      // still belongs to both selections. Keep its keyed row until the new
      // first page arrives so a click or keyboard focus can stay on it.
      placeholderData: (previous) =>
        !cursor && previous
          ? {
              ...previous,
              objects: previous.objects.filter((row) => matchesSelection(row, { search, state, ownership, project })),
              nextPageToken: "",
            }
          : undefined,
    },
  );
  const rows = query.data?.objects ?? [];
  const visible = rows.flatMap((row) => (row.key ? [row.key] : []));
  const { connected, samples, health, paused, retry } = useDockerWatch(ids, visible, () => {
    if (cursor) setChanged(true);
    else void query.refetch();
  });
  const now = useControlPlaneNow(5_000);
  const times = useTimeWords();
  const refresh = () => {
    setChanged(false);
    setPages([""]);
    if (!cursor) void query.refetch();
  };
  useEffect(() => {
    if (typed.trim() === search) return;
    const timer = setTimeout(() => {
      setSearch(typed.trim());
      setPages([""]);
      setChanged(false);
    }, 200);
    return () => clearTimeout(timer);
  }, [search, typed]);
  const stack = (name: string) => {
    setProject(name);
    setPages([""]);
    setChanged(false);
  };
  const clear = () => {
    setTyped("");
    setSearch("");
    setState("all");
    setOwnership(DockerOwnership.UNSPECIFIED);
    setChoosingContainer(false);
    stack("");
  };

  const name = (row: Row) => (
    <div className="docker-name">
      <Link
        className="dw-link docker-row-link docker-cell-text"
        title={row.object.name}
        to={detailTarget}
        params={{ serverId: row.serverId, containerId: row.engineId }}
        search={{ name: row.object.name }}
      >
        {row.object.name}
      </Link>
      <Ownership row={row.object} />
    </div>
  );
  const menu = useRowMenu<Row>();
  const actionsFor = useContainerActions();
  const rowActions = (row: Row) =>
    actionsFor({
      row: row.object,
      stack,
      takeOnMode: choosingContainer ? "choose" : "menu",
      onApplied: refresh,
      onTakeOn: setTakeOn,
    });
  const dots = (row: Row) => <ContainerActionButtons {...rowActions(row)} />;
  const open = (row: Row) =>
    void navigate({
      to: detailTarget,
      params: { serverId: row.serverId, containerId: row.engineId },
      search: { name: row.object.name },
    });
  const measured = (row: Row) => (
    <Usage
      sample={connected && row.object.state === ContainerState.RUNNING ? samples.get(row.id) : undefined}
      now={now}
    />
  );
  const uptime = (row: Row) => {
    const started = row.object.state === ContainerState.RUNNING ? row.container?.startedAt : undefined;
    const label = started ? times.span(Math.max(0, now - timestampMs(started))) : t`Unavailable`;
    return (
      <span className={started ? "docker-cell-text" : "docker-cell-text docker-muted"} title={label}>
        {label}
      </span>
    );
  };
  const addresses = (row: Row) => {
    const values = [...new Set(row.container?.endpoints.flatMap((n) => [n.ipv4, n.ipv6].filter(Boolean)) ?? [])];
    return <ValuePreview values={values} />;
  };
  const columns: TableColumn<Row>[] = [
    { key: "name", header: t`Name`, render: name },
    { key: "state", header: t`State`, render: (row) => <ContainerStatus row={row.object} /> },
    ...(fleet
      ? [
          {
            key: "serverName",
            header: t`Server`,
            render: (row: Row) => (
              <Link
                className="dw-link docker-cell-text"
                title={row.serverName}
                to="/servers/$serverId/docker"
                params={{ serverId: row.serverId }}
              >
                {row.serverName}
              </Link>
            ),
          },
        ]
      : []),
    {
      key: "image",
      header: t`Image`,
      width: "23%",
      render: (row) => <ImageReference image={row.container?.image} />,
      skeleton: "mono",
    },
    {
      key: "ports",
      header: t`Published ports`,
      render: (row) => <ValuePreview values={row.container?.ports.map(dockerPort) ?? []} />,
    },
    { key: "addresses", header: t`Addresses`, render: addresses, skeleton: "mono" },
    { key: "usage", header: t`Usage`, render: measured, skeleton: "short" },
    { key: "uptime", header: t`Uptime`, render: uptime, skeleton: "short" },
    {
      key: "actions",
      header: (
        <span className="dw-sr">
          <Trans>Actions</Trans>
        </span>
      ),
      align: "right",
      width: 128,
      render: dots,
    },
  ];
  const list: Row[] = rows.flatMap((raw) => {
    const object = rowWithHealth(raw, health);
    if (!object.key) return [];
    const { engineId, serverId } = object.key;
    return [
      {
        id: dockerKey(serverId, engineId),
        engineId,
        serverId,
        object,
        container: containerOf(object),
        serverName: servers.find((s) => s.id === serverId)?.name ?? serverId,
      },
    ];
  });
  const filtered = !!(search || project || state !== "all" || ownership);
  const outOfDate = changed || reasonOf(query.error) === "docker_resync";
  const next = query.data?.nextPageToken;
  const waiting = query.isPending || query.isPlaceholderData;
  const held = useHeldRows("docker.containers", waiting ? undefined : list.length);
  const slow = fleet ? t`Docker is slow to answer on these servers` : t`Docker is slow to answer on this server`;
  const imports: MenuAction[] = [
    ...(may(ContainerService.method.adoptContainer).allowed
      ? [
          {
            id: "take-on",
            label: t`Take on a container`,
            icon: "service" as const,
            onSelect: () => {
              clear();
              setOwnership(DockerOwnership.EXTERNAL);
              setChoosingContainer(true);
            },
          },
        ]
      : []),
    ...(may(ComposeService.method.importCompose).allowed
      ? [
          {
            id: "compose",
            label: t`Compose file`,
            icon: "file" as const,
            onSelect: () =>
              void navigate({
                to: "/apps/new",
                search: { source: "compose", ...(serverId ? { server: serverId } : {}) },
              }),
          },
        ]
      : []),
    ...(serverId && may(ImportService.method.importOnePanel).allowed
      ? [
          {
            id: "move-in",
            label: t`Move in`,
            icon: "download" as const,
            onSelect: () => void navigate({ to: "/servers/$serverId/import", params: { serverId } }),
          },
        ]
      : []),
  ];

  return (
    <>
      {takeOn ? <DockerTakeOn row={takeOn} onClose={() => setTakeOn(undefined)} /> : null}
      <div className="docker-section-heading">
        <h2>
          <Trans>Containers</Trans>
        </h2>
        <div className="docker-heading-actions">
          <Button
            icon="refresh"
            size="sm"
            variant="ghost"
            onClick={refresh}
            loading={query.isFetching && !waiting}
            aria-label={t`Refresh containers`}
            title={t`Refresh containers`}
          />
          <ActionMenu actions={imports} label={t`Import`} icon="download" text={t`Import`} />
          {may(ChangeService.method.proposeChange).allowed ? (
            <RouterButtonLink
              to="/apps/new"
              search={{ source: "image", advanced: true, ...(serverId ? { server: serverId } : {}) }}
              icon="plus"
              size="sm"
              variant="primary"
            >
              <Trans>New container</Trans>
            </RouterButtonLink>
          ) : null}
        </div>
      </div>
      <div className="docker-filters">
        <SegmentedControl
          label={t`Container state`}
          value={state}
          options={[
            { value: "all", label: t`All` },
            { value: "running", label: t`Running` },
            { value: "stopped", label: t`Stopped` },
            { value: "attention", label: t`Needs attention` },
          ]}
          onChange={(value: State) => {
            setState(value);
            setPages([""]);
            setChanged(false);
          }}
        />
        <TextField
          ref={searchInput}
          icon="search"
          value={typed}
          maxLength={255}
          onChange={(event) => setTyped(event.target.value)}
          placeholder={t`Name, image or project`}
          aria-label={t`Search containers`}
        />
        <SelectField
          aria-label={t`Container ownership`}
          value={String(ownership)}
          onChange={(event) => {
            setOwnership(Number(event.target.value));
            setChoosingContainer(false);
            setPages([""]);
            setChanged(false);
          }}
          options={[
            { value: String(DockerOwnership.UNSPECIFIED), label: t`All ownership` },
            { value: String(DockerOwnership.MANAGED), label: t`Managed by Dockwell` },
            { value: String(DockerOwnership.EXTERNAL), label: t`Not managed` },
            { value: String(DockerOwnership.SYSTEM), label: t`System` },
          ]}
        />
        {project ? (
          <Button
            aria-label={t`Clear Compose project filter`}
            icon="x"
            size="sm"
            variant="secondary"
            onClick={() => stack("")}
          >
            {project}
          </Button>
        ) : null}
      </div>
      <LiveUpdates paused={paused} retry={retry} />
      <Unread values={query.data?.unavailable ?? []} servers={servers} retry={refresh} />
      {outOfDate ? (
        <Callout
          tone="warning"
          title={<Trans>Container inventory changed</Trans>}
          actions={
            <Button onClick={refresh} size="sm">
              <Trans>Refresh list</Trans>
            </Button>
          }
        />
      ) : query.error ? (
        <Callout
          tone="critical"
          title={<Trans>Containers couldn't be read</Trans>}
          actions={
            <Button onClick={refresh} size="sm">
              <Trans>Try again</Trans>
            </Button>
          }
        >
          {sentence(query.error)}
        </Callout>
      ) : null}
      {waiting && list.length === 0 && !query.error ? (
        <Panel flush>
          {compact ? (
            <Loading label={t`Loading containers`} slow={slow}>
              <SkeletonRows rows={held} end="pill" />
            </Loading>
          ) : (
            <Table
              className="docker-table"
              caption={t`Container inventory`}
              rows={[]}
              columns={columns}
              loading
              loadingRows={held}
              loadingLabel={t`Loading containers`}
              slow={slow}
            />
          )}
        </Panel>
      ) : null}
      {!waiting && !query.error && list.length === 0 && !query.data?.unavailable.length ? (
        <EmptyState
          icon="service"
          title={filtered ? t`No containers match` : t`No containers here`}
          action={
            filtered ? (
              <Button onClick={clear} size="sm">
                <Trans>Clear filters</Trans>
              </Button>
            ) : undefined
          }
        />
      ) : null}
      {list.length > 0 ? (
        <Panel flush>
          {compact ? (
            <ul className="docker-card-list">
              {list.map((row) => (
                <li key={row.id} className="docker-card">
                  <div className="docker-card-head">
                    {name(row)}
                    <ContainerStatus row={row.object} />
                  </div>
                  {fleet ? (
                    <Link className="dw-link" to="/servers/$serverId/docker" params={{ serverId: row.serverId }}>
                      {row.serverName}
                    </Link>
                  ) : null}
                  <div className="docker-card-image">
                    <ImageReference image={row.container?.image} />
                  </div>
                  <div className="docker-card-foot">
                    <div>
                      {measured(row)}
                      {row.container?.ports.length ? (
                        <ValuePreview values={row.container.ports.map(dockerPort)} />
                      ) : null}
                    </div>
                    {dots(row)}
                  </div>
                </li>
              ))}
            </ul>
          ) : (
            <Table
              className="docker-table"
              caption={t`Container inventory`}
              rows={list}
              rowKey="id"
              onRowClick={open}
              rowHasContextMenu={(row) => !!row.object.key}
              onRowContextMenu={(row, at, element) => menu.open(row, at, element, rowActions(row))}
              columns={columns}
            />
          )}
        </Panel>
      ) : null}
      {menu.element}
      {pages.length > 1 || next ? (
        <nav className="docker-pager" aria-label={t`Container pages`}>
          <Button
            size="sm"
            variant="secondary"
            disabled={pages.length <= 1}
            onClick={() => {
              setChanged(false);
              setPages((all) => all.slice(0, -1));
            }}
          >
            <Trans>Previous</Trans>
          </Button>
          <span>
            <Trans>Page {pages.length}</Trans>
          </span>
          <Button
            size="sm"
            variant="secondary"
            disabled={!next || outOfDate}
            onClick={() => {
              if (next) setPages((all) => [...all, next]);
            }}
          >
            <Trans>Next</Trans>
          </Button>
        </nav>
      ) : null}
    </>
  );
}

interface Row {
  id: string;
  engineId: string;
  serverId: string;
  serverName: string;
  object: DockerObject;
  container: DockerContainer | undefined;
}
