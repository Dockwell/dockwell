// SPDX-License-Identifier: AGPL-3.0-only

import { timestampMs } from "@bufbuild/protobuf/wkt";
import { useQuery } from "@connectrpc/connect-query";
import {
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
  LogService,
  type Server,
  TerminalService,
} from "@dockwell/api";
import {
  Button,
  Callout,
  Panel,
  SegmentedControl,
  SelectField,
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
import { useTimeWords } from "../../lib/time";
import { useMay } from "../auth/may";
import { statusOf, useContainerStateWord } from "../servers/containers";
import { useTakeOn } from "../servers/takeOn";
import { useControlPlaneNow } from "../servers/useFleet";
import { ActionMenu, type MenuAction } from "../shell/ActionMenu";
import { EmptyState } from "../shell/EmptyState";
import { dockerKey, rowWithHealth, useDockerWatch } from "./live";
import { containerOf, LiveUpdates, Unread } from "./parts";

type State = "all" | "running" | "stopped" | "attention";
const shellTarget = "/servers/$serverId/terminal";
const detailTarget = "/servers/$serverId/docker/containers/$containerId";

export function ContainerStatus({ row }: { row: DockerObject }) {
  const { t } = useLingui();
  const stateWord = useContainerStateWord();
  if (row.state === ContainerState.RUNNING) {
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
  const label = stateWord(row.state);
  return <StatusBadge status={statusOf(row.state)} label={label} title={label} labelIsState />;
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

export function ContainerActions({ row, stack }: { row: DockerObject; stack?: (project: string) => void }) {
  const { t } = useLingui();
  const may = useMay();
  const navigate = useNavigate();
  const key = row.key;
  const { take } = useTakeOn(key?.serverId ?? "");
  if (!key) return null;
  const params = { serverId: key.serverId, containerId: key.engineId };
  const log =
    may(LogService.method.watchLogs).allowed &&
    row.ownership !== DockerOwnership.SYSTEM &&
    (row.ownership !== DockerOwnership.MANAGED || (!!row.managedResourceId && row.managedResource?.kind === "service"));
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
  if (row.ownership === DockerOwnership.EXTERNAL && may(ContainerService.method.adoptContainer).allowed) {
    const container = containerOf(row);
    actions.push({
      id: "take-on",
      label: t`Take on`,
      icon: "plus",
      onSelect: () =>
        take({
          engineId: key.engineId,
          name: row.name,
          composeService: container?.composeService ?? "",
          composeProject: container?.composeProject ?? "",
        }),
    });
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
  return (
    <div className="docker-row-actions">
      {log ? (
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
      {terminal ? (
        <RouterButtonLink
          to={shellTarget}
          params={{ serverId: key.serverId }}
          search={{ containerId: key.engineId, containerTitle: row.name }}
          icon="terminal"
          variant="ghost"
          size="sm"
          aria-label={t`Terminal for ${row.name}`}
          title={t`Terminal`}
        />
      ) : null}
      <ActionMenu actions={actions} label={t`Actions for ${row.name}`} />
    </div>
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
    { retry: false, refetchOnWindowFocus: false },
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
  const dots = (row: Row) => <ContainerActions row={row.object} stack={stack} />;
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
    { key: "image", header: t`Image`, width: "23%", render: (row) => <ImageReference image={row.container?.image} /> },
    {
      key: "ports",
      header: t`Published ports`,
      render: (row) => <ValuePreview values={row.container?.ports.map(dockerPort) ?? []} />,
    },
    { key: "addresses", header: t`Addresses`, render: addresses },
    { key: "usage", header: t`Usage`, render: measured },
    { key: "uptime", header: t`Uptime`, render: uptime },
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
  return (
    <>
      <div className="docker-section-heading">
        <h2>
          <Trans>Containers</Trans>
        </h2>
        <Button icon="refresh" size="sm" variant="ghost" onClick={refresh} aria-label={t`Refresh containers`}>
          <Trans>Refresh</Trans>
        </Button>
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
      {query.isPending ? (
        <p role="status">
          <Trans>Reading containers</Trans>
        </p>
      ) : null}
      {!query.isPending && !query.error && list.length === 0 && !query.data?.unavailable.length ? (
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
            <Table className="docker-table" caption={t`Container inventory`} rows={list} columns={columns} />
          )}
        </Panel>
      ) : null}
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
