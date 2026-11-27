// SPDX-License-Identifier: AGPL-3.0-only

import { timestampMs } from "@bufbuild/protobuf/wkt";
import { useQuery } from "@connectrpc/connect-query";
import {
  ChangeService,
  ChangeState,
  ComposeService,
  type ContainerPort,
  ContainerService,
  ContainerState,
  DeploymentState,
  DesiredRunState,
  type DockerContainer,
  DockerContainerAction,
  DockerHealth,
  type DockerObject,
  DockerObjectKind,
  DockerOwnership,
  type DockerSample,
  DockerService,
  FileService,
  ImportService,
  LogService,
  type Server,
  type ServiceInfo,
  TerminalService,
} from "@dockwell/api";
import {
  Button,
  Callout,
  cx,
  Loading,
  PaginationBar,
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
import { plural } from "@lingui/core/macro";
import { Trans, useLingui } from "@lingui/react/macro";
import { Link, useNavigate } from "@tanstack/react-router";
import { useEffect, useState } from "react";
import { reasonOf, sentence } from "../../lib/errors";
import { formatBytes, formatPercent } from "../../lib/format";
import { RouterButtonLink, RouterTagLink } from "../../lib/links";
import { useHeldRows } from "../../lib/loading";
import { useTimeWords } from "../../lib/time";
import { PrepareServiceUpdate } from "../apps/ServiceRowActions";
import { counts, offered, useMayUpdate } from "../apps/updates";
import { useServices } from "../apps/useServices";
import { useWorkloadControls } from "../apps/workload";
import { useMay } from "../auth/may";
import { askable } from "../files/lookup";
import { statusOf, useContainerStateWord } from "../servers/containers";
import { useControlPlaneNow, useFleetView } from "../servers/useFleet";
import { ActionMenu, type MenuAction } from "../shell/ActionMenu";
import { EmptyState } from "../shell/EmptyState";
import {
  type BatchAction,
  BatchBar,
  cardContextMenu,
  combineBatchActions,
  SelectBox,
  selectionColumn,
  useBatchAction,
  useListSearchShortcut,
  useRowMenu,
  useRowSelection,
} from "../shell/ListTools";
import { acceptsLifecycle, useDockerLifecycle } from "./DockerLifecycle";
import { DockerRunSheet } from "./DockerRunSheet";
import { DockerTakeOn } from "./DockerTakeOn";
import { dockerKey, rowWithHealth, sameServerIds, useDockerWatch } from "./live";
import { containerOf, LiveUpdates, Unread } from "./parts";
import { publishedPortUrl } from "./publishedPort";

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
        return <StatusBadge status="running" label={t`Running`} title={t`Running`} labelIsState />;
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

export function portUrl(port: ContainerPort, address: string | undefined) {
  return port.protocol.toLowerCase() === "tcp" ? publishedPortUrl(port.address, port.serverPort, address) : undefined;
}

export function PublishedPorts({ ports, address }: { ports: readonly ContainerPort[]; address?: string | undefined }) {
  const { t } = useLingui();
  if (ports.length === 0) return <ValuePreview values={[]} />;
  return (
    <span className="docker-port-preview" title={ports.map(dockerPort).join(", ")}>
      {ports.slice(0, 2).map((port) => {
        const label = dockerPort(port);
        const href = portUrl(port, address);
        return href ? (
          <a className="docker-fact-link docker-cell-text" key={label} href={href} target="_blank" rel="noreferrer">
            <code>{label}</code>
          </a>
        ) : (
          <code className="docker-cell-text" key={label}>
            {label}
          </code>
        );
      })}
      {ports.length > 2 ? <span className="docker-cell-text">{t`+${ports.length - 2} more`}</span> : null}
    </span>
  );
}

function runTarget(row: DockerObject) {
  const resource = row.managedResource;
  if (
    !resource ||
    !["container", "service"].includes(resource.kind) ||
    !row.managedResourceId ||
    row.managedRevision <= 0n
  )
    return;
  const start = row.state === ContainerState.CREATED || row.state === ContainerState.EXITED;
  if (!start && ![ContainerState.RUNNING, ContainerState.PAUSED, ContainerState.RESTARTING].includes(row.state)) return;
  const where = resource.kind === "container" ? { install: true as const } : { environment: resource.scope };
  return { resource, baseRevision: row.managedRevision, start, where };
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
  address?: string | undefined;
  update?: MenuAction | undefined;
  workloadServers?: number | undefined;
  requireServicePlacement?: boolean;
  onApplied?: () => void;
  onTakeOn?: (row: DockerObject) => void;
}

export function ContainerActions(props: ContainerActionProps & { lifecycle?: ReturnType<typeof useDockerLifecycle> }) {
  const local = useDockerLifecycle();
  const actionsFor = useContainerActions(props.lifecycle ?? local);
  return (
    <>
      {props.lifecycle ? null : local.editor}
      <ContainerActionButtons {...actionsFor(props)} />
    </>
  );
}

function useContainerActions(lifecycle: ReturnType<typeof useDockerLifecycle>) {
  const { t } = useLingui();
  const may = useMay();
  const navigate = useNavigate();
  const { changeRun, changeRunMany, changePause, restartInstance } = useWorkloadControls();
  return ({
    row,
    stack,
    takeOnMode = "menu",
    shortcuts = true,
    address,
    update,
    workloadServers,
    requireServicePlacement = false,
    onApplied,
    onTakeOn,
  }: ContainerActionProps) => {
    const key = row.key;
    const label = t`Actions for ${row.name}`;
    if (!key) return { label, actions: [], reasons: undefined, row };
    const canTake =
      onTakeOn && row.ownership === DockerOwnership.EXTERNAL && may(ContainerService.method.adoptContainer).allowed;
    const showTakeOn = canTake && takeOnMode !== "menu";
    const showShortcuts = shortcuts && (takeOnMode !== "choose" || !canTake);
    const params = { serverId: key.serverId, containerId: key.engineId };
    const logScope =
      row.managedResource?.kind === "service" ? { environment: row.managedResource.scope } : { install: true as const };
    const log =
      may(LogService.method.watchLogs, logScope).allowed &&
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
    actions.unshift(...lifecycle.actionsFor(row, onApplied));
    const target = runTarget(row);
    if (target) {
      const mayPlan = may(ChangeService.method.proposeChange, target.where);
      const { start, resource, baseRevision } = target;
      const service = resource.kind === "service";
      const name = resource.name;
      const fleetAction = service && workloadServers !== undefined && workloadServers > 1;
      const label = fleetAction
        ? start
          ? t`Start ${name} on all ${workloadServers} servers`
          : t`Stop ${name} on all ${workloadServers} servers`
        : start
          ? t`Start`
          : t`Stop`;
      actions.unshift({
        id: start ? "start" : "stop",
        label,
        icon: start ? "play" : "stop",
        ...mayPlan.off,
        ...(mayPlan.allowed && service && requireServicePlacement && !workloadServers
          ? { off: t`Dockwell couldn't read where this service runs. Refresh to try again.` }
          : {}),
        onSelect: () =>
          fleetAction
            ? changeRunMany([{ resource, baseRevision }], start, label, onApplied)
            : changeRun(resource, baseRevision, start, onApplied),
      });
      if (row.state === ContainerState.PAUSED || row.state === ContainerState.RUNNING) {
        const pause = row.state === ContainerState.RUNNING;
        actions.splice(1, 0, {
          id: pause ? "pause" : "unpause",
          label: pause ? t`Pause workload` : t`Unpause workload`,
          icon: pause ? "pause" : "play",
          ...mayPlan.off,
          onSelect: () => changePause(resource, baseRevision, pause, onApplied),
        });
      }
      if (row.state === ContainerState.RUNNING || row.state === ContainerState.RESTARTING) {
        actions.splice(1, 0, {
          id: "restart",
          label: t`Restart this container`,
          icon: "refresh",
          ...may(ChangeService.method.restartService, target.where).off,
          onSelect: () => restartInstance(row.managedResourceId, key.serverId, key.engineId, onApplied),
        });
      }
    }
    if (log)
      actions.push({
        id: "logs",
        label: t`Logs`,
        icon: "file",
        onSelect: () => void navigate({ to: detailTarget, params, search: { view: "logs", name: row.name } }),
      });
    if (terminal)
      actions.push({
        id: "terminal",
        label: t`Terminal`,
        icon: "terminal",
        onSelect: () =>
          void navigate({
            to: shellTarget,
            params: { serverId: key.serverId },
            search: { containerId: key.engineId, containerTitle: row.name },
          }),
      });
    const container = containerOf(row);
    const path = [container?.composeDirectory, ...(container?.mounts.map((mount) => mount.hostPath) ?? [])].find(
      (path) => !!path && askable(key.serverId, path),
    );
    if (path && may(FileService.method.listFiles).allowed)
      actions.push({
        id: "files",
        label: t`Open files`,
        icon: "folder",
        onSelect: () =>
          void navigate({ to: "/servers/$serverId/files", params: { serverId: key.serverId }, search: { path } }),
      });
    const visit = container?.ports.map((port) => portUrl(port, address)).find((url) => url !== undefined);
    if (visit && row.state === ContainerState.RUNNING)
      actions.push({
        id: "visit",
        label: t`Open in browser`,
        icon: "external",
        href: visit,
      });
    if (update) actions.push(update);
    const project = containerOf(row)?.composeProject;
    if (stack && project) {
      actions.push({
        id: "compose",
        label: t`Filter by this Compose project`,
        icon: "filter",
        onSelect: () => stack(project),
      });
    }
    const externalOnly = row.ownership !== DockerOwnership.EXTERNAL ? t`Managed or system container` : undefined;
    const removing = row.state === ContainerState.REMOVING ? t`Being removed` : undefined;
    const running = row.state === ContainerState.RUNNING || row.state === ContainerState.RESTARTING;
    const paused = row.state === ContainerState.PAUSED;
    const stopped =
      row.state === ContainerState.CREATED || row.state === ContainerState.EXITED || row.state === ContainerState.DEAD;
    const reasons: Record<string, string | undefined> = {
      start: removing ?? (running || paused ? t`Already running` : undefined),
      stop: removing ?? (stopped ? t`Already stopped` : undefined),
      pause:
        removing ??
        (paused
          ? t`Already paused`
          : row.state === ContainerState.RESTARTING
            ? t`Restarting`
            : stopped
              ? t`Not running`
              : undefined),
      unpause: removing ?? (!paused ? t`Not paused` : undefined),
      restart: removing ?? (paused ? t`Paused` : stopped ? t`Not running` : undefined),
      [`external-${DockerContainerAction.START}`]:
        externalOnly ?? removing ?? (running || paused ? t`Already running` : undefined),
      [`external-${DockerContainerAction.STOP}`]:
        externalOnly ?? removing ?? (stopped ? t`Already stopped` : undefined),
      [`external-${DockerContainerAction.RESTART}`]:
        externalOnly ?? removing ?? (paused ? t`Paused` : stopped ? t`Not running` : undefined),
      [`external-${DockerContainerAction.PAUSE}`]:
        externalOnly ??
        removing ??
        (paused
          ? t`Already paused`
          : row.state === ContainerState.RESTARTING
            ? t`Restarting`
            : stopped
              ? t`Not running`
              : undefined),
      [`external-${DockerContainerAction.UNPAUSE}`]: externalOnly ?? removing ?? (!paused ? t`Not paused` : undefined),
      [`external-${DockerContainerAction.KILL}`]: externalOnly ?? removing ?? (stopped ? t`Not running` : undefined),
      [`external-${DockerContainerAction.REMOVE}`]:
        externalOnly ?? removing ?? (running || paused ? t`Stop the container first` : undefined),
      [`external-${DockerContainerAction.RENAME}`]: externalOnly ?? removing,
    };
    return { label, actions, reasons, row, params, objectKey: key, log, terminal, showTakeOn, showShortcuts, onTakeOn };
  };
}

function ContainerActionButtons({
  label,
  actions,
  reasons,
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
      <ActionMenu actions={actions} reasons={reasons} label={label} />
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
  const { servers: fleetServers } = useFleetView();
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
      placeholderData: (previous, previousQuery) =>
        !cursor && previous && sameServerIds(previousQuery, ids)
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
  const services = useServices().data;
  const placements = new Map(
    services?.map((service) => [
      service.id,
      {
        revision: service.revision,
        servers: new Set(
          service.deployments
            .filter(
              (deployment) => deployment.state === DeploymentState.LIVE || deployment.state === DeploymentState.PENDING,
            )
            .map((deployment) => deployment.serverId),
        ),
      },
    ]),
  );
  const placementFor = (row: Row) => {
    const placement = placements.get(row.object.managedResourceId);
    if (placement?.revision === row.object.managedRevision && placement.servers.has(row.serverId))
      return placement.servers;
    return undefined;
  };
  const mayUpdate = useMayUpdate();
  const [updatingApp, setUpdatingApp] = useState<ServiceInfo>();
  const { changeRunMany } = useWorkloadControls();
  const address = (row: Row) => fleetServers?.find((server) => server.id === row.serverId)?.address;
  const updateFor = (row: Row): MenuAction | undefined => {
    const service = services?.find((item) => item.id === row.object.managedResourceId);
    if (!service || !counts(service.update, now)) return;
    const offer = offered(service.update);
    if (!offer) return;
    const version = offer.toVersion;
    return {
      id: "update",
      icon: "arrow-up",
      label: version ? t`Update to ${version}` : t`Update`,
      ...mayUpdate(service).off,
      onSelect: () => setUpdatingApp(service),
    };
  };
  const lifecycle = useDockerLifecycle();
  const [pasting, setPasting] = useState(false);
  const actionsFor = useContainerActions(lifecycle);
  const rowActions = (row: Row) =>
    actionsFor({
      row: row.object,
      address: address(row),
      update: updateFor(row),
      workloadServers: placementFor(row)?.size,
      requireServicePlacement: true,
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
    { key: "name", header: t`Name`, width: fleet ? "18%" : "19%", render: name },
    { key: "state", header: t`State`, width: 120, render: (row) => <ContainerStatus row={row.object} /> },
    ...(fleet
      ? [
          {
            key: "serverName",
            header: t`Server`,
            width: 100,
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
      width: fleet ? "14%" : "16%",
      render: (row) => <ImageReference image={row.container?.image} />,
      skeleton: "mono",
    },
    {
      key: "ports",
      header: t`Published ports`,
      width: fleet ? 180 : 204,
      render: (row) => <PublishedPorts ports={row.container?.ports ?? []} address={address(row)} />,
    },
    { key: "addresses", header: t`Addresses`, width: 104, render: addresses, skeleton: "mono" },
    { key: "usage", header: t`Usage`, width: 112, render: measured, skeleton: "short" },
    { key: "uptime", header: t`Uptime`, width: 92, render: uptime, skeleton: "short" },
    {
      key: "actions",
      header: (
        <span className="dw-sr">
          <Trans>Actions</Trans>
        </span>
      ),
      align: "right",
      width: 112,
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
  const menu = useRowMenu<Row>((row) => rowActions(row), { rows: list, keyOf: (row) => row.id });
  const canAct =
    may(ChangeService.method.proposeChange).allowed || may(DockerService.method.planDockerMaintenance).allowed;
  const selection = useRowSelection(
    list.map((row) => row.id),
    canAct,
    searchInput,
  );
  const selected = list.filter((row) => selection.has(row.id));
  const selectedIdsKey = selected.map((row) => row.id).join(",");
  const [followUpNote, setFollowUpNote] = useState<{ ids: string; text: string }>();
  const candidates = selected.flatMap((row) => {
    const target = runTarget(row.object);
    return target ? [{ row, ...target, permission: may(ChangeService.method.proposeChange, target.where) }] : [];
  });
  const byId = new Map(candidates.map((target) => [target.row.id, target]));
  const batchAction = useBatchAction<Row>();
  const skippedServices = new Map<string, string>();
  const multiServerReasons = new Set<string>();
  const finishBatch = () => {
    setFollowUpNote(undefined);
    selection.clear();
    refresh();
  };
  const sharedActionOf = {
    start: DockerContainerAction.START,
    stop: DockerContainerAction.STOP,
    pause: DockerContainerAction.PAUSE,
    unpause: DockerContainerAction.UNPAUSE,
  } as const;
  const withButtonHint = (
    item: { actions: BatchAction[]; notes: string[] },
    verbLabel: string,
  ): { actions: BatchAction[]; notes: string[] } => {
    if (item.notes.length === 0) return item;
    const hint = `${verbLabel}: ${item.notes.join("; ")}`;
    return {
      actions: item.actions.map((action) => ({ ...action, title: action.title ? `${action.title}; ${hint}` : hint })),
      notes: [],
    };
  };
  const runBatch = (mode: "start" | "stop" | "pause" | "unpause") => {
    const start = mode === "start";
    const pause = mode === "pause";
    const togglePause = pause || mode === "unpause";
    const lifecycleAction = sharedActionOf[mode];
    const matches = (row: Row, starts: boolean) =>
      togglePause ? row.object.state === (pause ? ContainerState.RUNNING : ContainerState.PAUSED) : starts === start;
    const label = start ? t`Start` : pause ? t`Pause` : togglePause ? t`Unpause` : t`Stop`;
    const wrongState = start
      ? t`Already running`
      : pause
        ? t`Not running or already paused`
        : togglePause
          ? t`Not paused`
          : t`Already stopped`;
    const item = batchAction(
      selected,
      (row) => {
        if (row.object.ownership === DockerOwnership.SYSTEM) return { reason: t`System container` };
        if (row.object.ownership === DockerOwnership.EXTERNAL) {
          if (!acceptsLifecycle(row.object, lifecycleAction)) return { reason: wrongState };
          return lifecycle.permission.allowed ? undefined : { permission: lifecycle.permission };
        }
        const target = byId.get(row.id);
        if (!target) return { reason: t`Dockwell doesn't manage this container` };
        if (!matches(row, target.start)) return { reason: wrongState };
        if (!target.permission.allowed) return { permission: target.permission };
        if (target.resource.kind === "service") {
          const placement = placementFor(row);
          if (!placement) return { reason: t`Dockwell couldn't read where this service runs. Refresh to try again.` };
          const selectedServers = new Set(
            candidates
              .filter(
                (candidate) =>
                  candidate.row.object.managedResourceId === row.object.managedResourceId &&
                  candidate.baseRevision === target.baseRevision &&
                  matches(candidate.row, candidate.start) &&
                  candidate.permission.allowed,
              )
              .map((candidate) => candidate.row.serverId),
          );
          const missing = [...placement].filter((server) => !selectedServers.has(server));
          if (missing.length > 0) {
            if (row.object.managedResourceId) skippedServices.set(row.object.managedResourceId, target.resource.name);
            const selectedAnyState = new Set(
              selected
                .filter((item) => item.object.managedResourceId === row.object.managedResourceId)
                .map((item) => item.serverId),
            );
            const extra = [...placement].filter((server) => !selectedAnyState.has(server)).length;
            if (extra > 0) {
              const reason = plural(extra, { one: "Runs on # more server", other: "Runs on # more servers" });
              multiServerReasons.add(reason);
              return { reason };
            }
            const reason = t`Replicas are in different states`;
            multiServerReasons.add(reason);
            return { reason };
          }
        }
        return undefined;
      },
      {
        id: mode,
        label: (count) => (count ? t`${label} ${count}` : label),
        icon: pause ? "pause" : start || togglePause ? "play" : "stop",
        onSelect: (rows) => {
          setFollowUpNote(undefined);
          const managedRows = rows.filter((row) => row.object.ownership !== DockerOwnership.EXTERNAL);
          const externalRows = rows.filter((row) => row.object.ownership === DockerOwnership.EXTERNAL);
          const runExternal = () =>
            lifecycle.select({
              action: lifecycleAction,
              label,
              rows: externalRows.map((row) => row.object),
              onApplied: finishBatch,
            });
          if (managedRows.length > 0) {
            const unique = new Map(
              managedRows.flatMap((row) => {
                const target = byId.get(row.id);
                return target ? [[row.object.managedResourceId, target] as const] : [];
              }),
            );
            const n = unique.size;
            const summary = start
              ? plural(n, { one: "Start # workload", other: "Start # workloads" })
              : plural(n, { one: "Stop # workload", other: "Stop # workloads" });
            const state = togglePause ? (pause ? DesiredRunState.PAUSED : DesiredRunState.RUNNING) : start;
            const pauseSummary = pause
              ? plural(n, { one: "Pause # workload", other: "Pause # workloads" })
              : plural(n, { one: "Unpause # workload", other: "Unpause # workloads" });
            const extCount = externalRows.length;
            const followUpPlanNote =
              extCount > 0
                ? start
                  ? plural(extCount, {
                      one: "Then starts # container Dockwell didn't make, in its own plan.",
                      other: "Then starts # containers Dockwell didn't make, in their own plan.",
                    })
                  : pause
                    ? plural(extCount, {
                        one: "Then pauses # container Dockwell didn't make, in its own plan.",
                        other: "Then pauses # containers Dockwell didn't make, in their own plan.",
                      })
                    : togglePause
                      ? plural(extCount, {
                          one: "Then unpauses # container Dockwell didn't make, in its own plan.",
                          other: "Then unpauses # containers Dockwell didn't make, in their own plan.",
                        })
                      : plural(extCount, {
                          one: "Then stops # container Dockwell didn't make, in its own plan.",
                          other: "Then stops # containers Dockwell didn't make, in their own plan.",
                        })
                : undefined;
            const notAppliedText =
              extCount > 0
                ? start
                  ? plural(extCount, {
                      one: "# container Dockwell didn't make wasn't started because the managed plan didn't apply.",
                      other: "# containers Dockwell didn't make weren't started because the managed plan didn't apply.",
                    })
                  : pause
                    ? plural(extCount, {
                        one: "# container Dockwell didn't make wasn't paused because the managed plan didn't apply.",
                        other:
                          "# containers Dockwell didn't make weren't paused because the managed plan didn't apply.",
                      })
                    : togglePause
                      ? plural(extCount, {
                          one: "# container Dockwell didn't make wasn't unpaused because the managed plan didn't apply.",
                          other:
                            "# containers Dockwell didn't make weren't unpaused because the managed plan didn't apply.",
                        })
                      : plural(extCount, {
                          one: "# container Dockwell didn't make wasn't stopped because the managed plan didn't apply.",
                          other:
                            "# containers Dockwell didn't make weren't stopped because the managed plan didn't apply.",
                        })
                : undefined;
            changeRunMany(
              [...unique.values()],
              state,
              togglePause ? pauseSummary : summary,
              extCount > 0
                ? () => {
                    // Clear the managed rows that applied and keep only the external rows until their plan finishes.
                    selection.setAll(
                      selected.filter((row) => !externalRows.some((ext) => ext.id === row.id)).map((row) => row.id),
                      false,
                    );
                    runExternal();
                  }
                : finishBatch,
              extCount > 0 && followUpPlanNote && notAppliedText
                ? {
                    notes: [followUpPlanNote],
                    onNotApplied: (change) => {
                      const extIds = new Set(externalRows.map((row) => row.id));
                      const drop = selected.filter((row) => !extIds.has(row.id)).map((row) => row.id);
                      if (drop.length > 0) selection.setAll(drop, false);
                      // A plan still applying hasn't failed: say only what waits. A planned one closed here is
                      // discarded, or left for an admin, and hasn't applied either way.
                      const pending = change?.state === ChangeState.APPLYING;
                      setFollowUpNote({
                        ids: externalRows.map((row) => row.id).join(","),
                        text: pending
                          ? plural(extCount, {
                              one: "# container Dockwell didn't make waits for its own plan.",
                              other: "# containers Dockwell didn't make wait for their own plan.",
                            })
                          : notAppliedText,
                      });
                    },
                  }
                : undefined,
            );
            return;
          }
          if (externalRows.length > 0) runExternal();
        },
      },
    );
    return { rawNotes: item.notes, batch: withButtonHint(item, label) };
  };
  const sharedResults = [runBatch("start"), runBatch("stop"), runBatch("pause"), runBatch("unpause")];
  const sharedBatches = sharedResults.map((item) => item.batch);
  const externalSelected = selected.filter((row) => row.object.ownership === DockerOwnership.EXTERNAL);
  const systemSelected = selected.filter((row) => row.object.ownership === DockerOwnership.SYSTEM);
  const externalOnlyVerbs = lifecycle.verbs.filter(
    (verb) =>
      verb.action === DockerContainerAction.RESTART ||
      verb.action === DockerContainerAction.KILL ||
      verb.action === DockerContainerAction.REMOVE ||
      (verb.action === DockerContainerAction.RENAME && selected.length === 1),
  );
  // External-only verbs attach their skip reasons to their own buttons rather than flooding the bar.
  const externalOnlyBatches =
    externalSelected.length > 0
      ? externalOnlyVerbs.map((verb) =>
          withButtonHint(
            batchAction(
              externalSelected,
              (row) => {
                if (!acceptsLifecycle(row.object, verb.action)) return { reason: verb.unavailable };
                return lifecycle.permission.allowed ? undefined : { permission: lifecycle.permission };
              },
              {
                id: `external-${verb.action}`,
                label: (count) => (count ? t`${verb.label} ${count}` : verb.label),
                icon: verb.icon,
                ...("danger" in verb ? { danger: true } : {}),
                onSelect: (targets) => {
                  setFollowUpNote(undefined);
                  lifecycle.select({
                    action: verb.action,
                    label: verb.label,
                    rows: targets.map((row) => row.object),
                    onApplied: finishBatch,
                  });
                },
              },
            ),
            verb.label,
          ),
        )
      : [];
  const multiServerNote = [...multiServerReasons][0];
  const openSkipped = (() => {
    if (skippedServices.size === 0) return { actions: [], notes: [] };
    const entries = [...skippedServices.entries()];
    if (entries.length === 1 && entries[0]) {
      const [serviceId, serviceName] = entries[0];
      return {
        actions: [
          {
            id: "open-app",
            icon: "service" as const,
            label: t`Open ${serviceName}`,
            onSelect: () => void navigate({ to: "/apps/$serviceId", params: { serviceId } }),
          },
        ],
        notes: [],
      };
    }
    return {
      actions: [
        {
          id: "open-apps",
          icon: "service" as const,
          label: t`Open Apps`,
          onSelect: () => void navigate({ to: "/apps" }),
        },
      ],
      notes: [],
    };
  })();
  const activeFollowUp = followUpNote && followUpNote.ids === selectedIdsKey ? followUpNote.text : undefined;
  const barNotes: string[] = activeFollowUp
    ? [activeFollowUp]
    : multiServerNote
      ? [multiServerNote]
      : systemSelected.length > 0
        ? [
            plural(systemSelected.length, {
              one: "# system container skipped",
              other: "# system containers skipped",
            }),
          ]
        : [];
  const batch = {
    actions: combineBatchActions(...sharedBatches, ...externalOnlyBatches, openSkipped).actions,
    notes: barNotes,
  };
  const filtered = !!(search || project || state !== "all" || ownership);
  const outOfDate = changed || reasonOf(query.error) === "docker_resync";
  const next = query.data?.nextPageToken;
  const waiting = query.isPending || query.isPlaceholderData;
  const held = useHeldRows("docker.containers", waiting ? undefined : list.length);
  const slow = fleet ? t`Docker is slow to answer on these servers` : t`Docker is slow to answer on this server`;
  const imports: MenuAction[] = [
    ...(may(ChangeService.method.proposeChange).allowed
      ? [
          {
            id: "docker-run",
            label: t`Paste docker run`,
            icon: "terminal" as const,
            onSelect: () => setPasting(true),
          },
        ]
      : []),
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
      {lifecycle.editor}
      {pasting ? <DockerRunSheet servers={servers} onClose={() => setPasting(false)} /> : null}
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
        <section
          aria-label={t`Container inventory`}
          className={cx("list-select-region", selection.any && "has-selection")}
          ref={selection.list}
          tabIndex={-1}
          onKeyDown={selection.onKeyDown}
        >
          <Panel flush>
            {compact ? (
              <ul className="docker-card-list">
                {list.map((row) => (
                  <li
                    key={row.id}
                    className={cx("docker-card list-select-card", selection.has(row.id) && "is-checked")}
                    onContextMenu={(event) =>
                      cardContextMenu(event, (at, el) => menu.open(row, at, el, rowActions(row)))
                    }
                  >
                    <div className="docker-card-head">
                      {selection.enabled ? (
                        <SelectBox
                          label={t`Select ${row.object.name}`}
                          checked={selection.has(row.id)}
                          onPick={(on, range) => selection.toggle(row.id, on, range)}
                        />
                      ) : null}
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
                          <PublishedPorts ports={row.container.ports} address={address(row)} />
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
                rowClassName={(row) => (selection.has(row.id) ? "is-checked" : "")}
                onRowClick={open}
                rowHasContextMenu={(row) => !!row.object.key}
                onRowContextMenu={(row, at, element) => menu.open(row, at, element, rowActions(row))}
                columns={[
                  ...(selection.enabled
                    ? [
                        selectionColumn({
                          selection,
                          rows: list,
                          idOf: (row) => row.id,
                          labelOf: (row) => t`Select ${row.object.name}`,
                          allLabel: t`Select all rows on this page`,
                        }),
                      ]
                    : []),
                  ...columns,
                ]}
              />
            )}
          </Panel>
          {menu.element}
          <BatchBar count={selected.length} onClear={selection.clear} {...batch} />
        </section>
      ) : null}
      {updatingApp ? <PrepareServiceUpdate service={updatingApp} onClose={() => setUpdatingApp(undefined)} /> : null}
      {pages.length > 1 || next ? (
        <PaginationBar
          label={t`Container pages`}
          summary={<Trans>Page {pages.length}</Trans>}
          hasPrevious={pages.length > 1}
          hasNext={Boolean(next && !outOfDate)}
          onPrevious={() => {
            setChanged(false);
            setPages((all) => all.slice(0, -1));
          }}
          onNext={() => {
            if (next) setPages((all) => [...all, next]);
          }}
          previousLabel={<Trans>Previous</Trans>}
          nextLabel={<Trans>Next</Trans>}
        />
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
