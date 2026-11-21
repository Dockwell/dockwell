// SPDX-License-Identifier: AGPL-3.0-only

import { createConnectQueryKey, useQuery } from "@connectrpc/connect-query";
import { type DockerServerSummary, DockerService, type Server, ServerService } from "@dockwell/api";
import {
  Button,
  Callout,
  Icon,
  Loading,
  Metric,
  Panel,
  SelectField,
  Skeleton,
  SkeletonCard,
  SkeletonFacts,
  SkeletonRows,
  TabNav,
} from "@dockwell/ui";
import { Trans, useLingui } from "@lingui/react/macro";
import { useQueryClient } from "@tanstack/react-query";
import { Link } from "@tanstack/react-router";
import { lazy, Suspense, useState } from "react";

import { sentence } from "../../lib/errors";
import { RouterButtonLink, RouterTabLink } from "../../lib/links";
import { useMay } from "../auth/may";
import { DiskUsePanel } from "../servers/DiskUse";
import { ServerFrame } from "../servers/ServerFrame";
import { useFleetView } from "../servers/useFleet";
import { EmptyState } from "../shell/EmptyState";
import { Page } from "../shell/Page";
import { DockerCleanup } from "./DockerCleanup";
import { ContainerStatus, DockerContainers } from "./DockerContainers";
import { DockerImages } from "./DockerImages";
import { DockerStorage } from "./DockerStorage";
import { rowWithHealth, useDockerWatch } from "./live";
import { Fact, LiveUpdates, Unread } from "./parts";

import "./Docker.css";

const Composes = lazy(async () => ({ default: (await import("./DockerComposes")).DockerComposes }));

export type DockerTab = "overview" | "containers" | "composes" | "images" | "networks" | "volumes" | "cleanup";

export function DockerWorkspace({
  serverId,
  tab,
  attention,
  group,
}: {
  serverId?: string;
  tab: DockerTab;
  attention?: boolean | undefined;
  group?: string | undefined;
}) {
  const { t } = useLingui();
  const allowed = useMay()(DockerService.method.getDockerOverview).allowed;
  const fleet = useFleetView();
  const servers = fleet.servers ?? [];
  if (!allowed) {
    return (
      <Page title={t`Docker`} back="/servers">
        <EmptyState icon="lock" title={<Trans>Docker inventory needs an admin</Trans>} />
      </Page>
    );
  }
  if (serverId) {
    return (
      <ServerFrame id={serverId} phoneHead={{ phoneTitle: t`Docker` }}>
        {(server) => (
          <Workspace
            key={serverId}
            tab={tab}
            attention={attention}
            group={group}
            servers={[{ id: server.id, name: server.name }]}
            server={server}
            multiple={servers.length > 1}
          />
        )}
      </ServerFrame>
    );
  }
  return (
    <Page title={t`Docker`} width="wide" back="/servers">
      {servers.length ? (
        <FleetDocker servers={servers} tab={tab} attention={attention} />
      ) : (
        <EmptyState
          icon="server"
          title={<Trans>No servers to inspect</Trans>}
          action={
            <RouterButtonLink to="/servers">
              <Trans>Open Servers</Trans>
            </RouterButtonLink>
          }
        />
      )}
    </Page>
  );
}

function FleetDocker({
  servers,
  tab,
  attention,
}: {
  servers: WorkspaceProps["servers"];
  tab: DockerTab;
  attention?: boolean | undefined;
}) {
  const { t } = useLingui();
  const [page, setPage] = useState(0);
  const pages = Math.ceil(servers.length / 32);
  const start = Math.min(page, pages - 1) * 32;
  const selected = servers.slice(start, start + 32);
  return (
    <>
      {pages > 1 ? (
        <div className="docker-scope">
          <SelectField
            label={t`Server range`}
            value={String(start / 32)}
            onChange={(event) => setPage(Number(event.target.value))}
            options={Array.from({ length: pages }, (_, i) => {
              const first = i * 32 + 1;
              const last = Math.min(first + 31, servers.length);
              return { value: String(i), label: t`Servers ${first} to ${last}` };
            })}
          />
        </div>
      ) : null}
      <Workspace key={start} servers={selected} tab={tab} attention={attention} multiple />
    </>
  );
}

interface WorkspaceProps {
  tab: DockerTab;
  servers: readonly Pick<Server, "id" | "name">[];
  server?: Server | undefined;
  multiple: boolean;
  attention?: boolean | undefined;
  group?: string | undefined;
}

function Workspace({ tab, servers, server, multiple, attention, group }: WorkspaceProps) {
  const { t } = useLingui();
  const ids = servers.map((s) => s.id);
  return (
    <section className="docker-workspace" aria-label={t`Docker`}>
      <div className="docker-navigation">
        <TabNav label={t`Docker views`}>
          {server ? (
            <>
              <RouterTabLink
                to="/servers/$serverId/docker"
                params={{ serverId: server.id }}
                activeOptions={{ exact: true, includeSearch: false }}
              >
                <Trans>Overview</Trans>
              </RouterTabLink>
              <RouterTabLink
                to="/servers/$serverId/docker/containers"
                params={{ serverId: server.id }}
                activeOptions={{ exact: true, includeSearch: false }}
              >
                <Trans>Containers</Trans>
              </RouterTabLink>
              <RouterTabLink
                to="/servers/$serverId/docker/composes"
                params={{ serverId: server.id }}
                activeOptions={{ exact: true, includeSearch: false }}
              >
                <Trans>Composes</Trans>
              </RouterTabLink>
              <RouterTabLink to="/servers/$serverId/docker/images" params={{ serverId: server.id }}>
                <Trans>Images</Trans>
              </RouterTabLink>
              <RouterTabLink to="/servers/$serverId/docker/networks" params={{ serverId: server.id }}>
                <Trans>Networks</Trans>
              </RouterTabLink>
              <RouterTabLink to="/servers/$serverId/docker/volumes" params={{ serverId: server.id }}>
                <Trans>Volumes</Trans>
              </RouterTabLink>
            </>
          ) : (
            <>
              <RouterTabLink to="/servers/docker" search={{}} activeOptions={{ exact: true, includeSearch: true }}>
                <Trans>Overview</Trans>
              </RouterTabLink>
              <RouterTabLink
                to="/servers/docker"
                search={{ view: "containers", ...(tab === "containers" && attention ? { attention: true } : {}) }}
                activeOptions={{ exact: true }}
              >
                <Trans>Containers</Trans>
              </RouterTabLink>
              <RouterTabLink
                to="/servers/docker"
                search={{ view: "composes" }}
                activeOptions={{ exact: true, includeSearch: true }}
              >
                <Trans>Composes</Trans>
              </RouterTabLink>
              <RouterTabLink
                to="/servers/docker"
                search={{ view: "images" }}
                activeOptions={{ exact: true, includeSearch: true }}
              >
                <Trans>Images</Trans>
              </RouterTabLink>
              <RouterTabLink
                to="/servers/docker"
                search={{ view: "networks" }}
                activeOptions={{ exact: true, includeSearch: true }}
              >
                <Trans>Networks</Trans>
              </RouterTabLink>
              <RouterTabLink
                to="/servers/docker"
                search={{ view: "volumes" }}
                activeOptions={{ exact: true, includeSearch: true }}
              >
                <Trans>Volumes</Trans>
              </RouterTabLink>
            </>
          )}
        </TabNav>
        {server && multiple ? (
          <RouterButtonLink
            to="/servers/docker"
            search={tab === "overview" ? {} : { view: tab }}
            variant="ghost"
            size="sm"
            icon="grid"
          >
            <Trans>All servers</Trans>
          </RouterButtonLink>
        ) : null}
      </div>
      {tab === "overview" ? (
        <Overview servers={servers} server={server} ids={ids} />
      ) : tab === "composes" ? (
        <Suspense
          fallback={
            <Loading label={t`Loading Compose projects`}>
              <SkeletonCard title={t`Compose projects`} flush>
                <SkeletonRows rows={3} end="pill" />
              </SkeletonCard>
            </Loading>
          }
        >
          <Composes servers={servers} serverId={server?.id} />
        </Suspense>
      ) : tab === "images" ? (
        <DockerImages servers={servers} fleet={!server} />
      ) : tab === "networks" ? (
        <DockerStorage servers={servers} fleet={!server} key="networks" />
      ) : tab === "volumes" ? (
        <DockerStorage servers={servers} fleet={!server} volumes key="volumes" />
      ) : tab === "cleanup" ? (
        <DockerCleanup servers={servers} group={group} />
      ) : (
        <DockerContainers servers={servers} fleet={server === undefined} attention={attention} />
      )}
    </section>
  );
}

function Overview({
  servers,
  server,
  ids,
}: {
  servers: WorkspaceProps["servers"];
  server?: Server | undefined;
  ids: string[];
}) {
  const { t, i18n } = useLingui();
  const cache = useQueryClient();
  const read = useQuery(DockerService.method.getDockerOverview, { serverIds: ids }, { retry: false });
  const data = read.data;
  const rows = data?.serverSummaries ?? [];
  const attention = rows.flatMap((row) => row.attention).slice(0, 5);
  const { health, paused, retry } = useDockerWatch(
    ids,
    attention.flatMap((row) => (row.key ? [row.key] : [])),
    () => {
      void read.refetch();
      if (server) {
        void cache.invalidateQueries({
          queryKey: createConnectQueryKey({
            schema: ServerService.method.getServerDiskUse,
            input: { id: server.id },
            cardinality: "finite",
          }),
        });
      }
    },
  );
  const sum = (
    key: "containers" | "running" | "composeProjects" | "images" | "networks" | "volumes" | "needsAttention",
  ) => rows.reduce((total, row) => total + row[key], 0).toLocaleString(i18n.locale);
  const containers = (
    <Metric label={t`Containers`} value={sum("containers")} caption={t`${sum("running")} running`} icon="service" />
  );
  const needsAttention = <Metric label={t`Needs attention`} value={sum("needsAttention")} icon="alert" />;
  return (
    <>
      <div className="docker-section-heading">
        <div>
          <h2>
            <Trans>Docker overview</Trans>
          </h2>
          {data && !server ? (
            <p>
              <Trans>
                {rows.length} of {servers.length} selected servers answered
              </Trans>
            </p>
          ) : null}
        </div>
        <Button
          icon="refresh"
          variant="ghost"
          size="sm"
          onClick={() => void read.refetch()}
          loading={read.isFetching && !read.isPending}
          aria-label={t`Refresh Docker overview`}
        >
          <Trans>Refresh</Trans>
        </Button>
      </div>
      {read.error ? (
        <Callout tone="critical" title={<Trans>Docker inventory couldn't be read</Trans>}>
          {sentence(read.error)}
        </Callout>
      ) : null}
      <LiveUpdates paused={paused} retry={retry} />
      <Unread values={data?.unavailable ?? []} servers={servers} retry={() => void read.refetch()} />
      {read.isPending ? (
        <Loading label={t`Loading the Docker overview`} slow={t`Docker is slow to answer on this server`}>
          <div className="docker-metrics docker-overview-metrics" aria-hidden="true">
            {[
              { label: t`Containers`, icon: "service" as const },
              { label: t`Compose projects`, icon: "layers" as const },
              { label: t`Images`, icon: "archive" as const },
              { label: t`Needs attention`, icon: "alert" as const },
            ].map(({ label, icon }) => (
              <Metric key={label} label={label} icon={icon} value={<Skeleton text="metric" width={48} />} />
            ))}
          </div>
          <div className="docker-overview-grid">
            <SkeletonCard title={t`Docker disk usage`} height={120} />
            <SkeletonCard title={t`Engine inventory`}>
              <SkeletonFacts count={4} layout="rows" />
            </SkeletonCard>
          </div>
        </Loading>
      ) : null}
      {rows.length > 0 ? (
        <>
          <div className="docker-metrics docker-overview-metrics">
            {server ? (
              <Link
                className="docker-metric-link"
                to="/servers/$serverId/docker/containers"
                params={{ serverId: server.id }}
              >
                {containers}
              </Link>
            ) : (
              <Link className="docker-metric-link" to="/servers/docker" search={{ view: "containers" }}>
                {containers}
              </Link>
            )}
            {server ? (
              <Link
                className="docker-metric-link"
                to="/servers/$serverId/docker/composes"
                params={{ serverId: server.id }}
              >
                <Metric label={t`Compose projects`} value={sum("composeProjects")} icon="layers" />
              </Link>
            ) : (
              <Link className="docker-metric-link" to="/servers/docker" search={{ view: "composes" }}>
                <Metric label={t`Compose projects`} value={sum("composeProjects")} icon="layers" />
              </Link>
            )}
            {server ? (
              <Link
                className="docker-metric-link"
                to="/servers/$serverId/docker/images"
                params={{ serverId: server.id }}
              >
                <Metric label={t`Images`} value={sum("images")} icon="archive" />
              </Link>
            ) : (
              <Link className="docker-metric-link" to="/servers/docker" search={{ view: "images" }}>
                <Metric label={t`Images`} value={sum("images")} icon="archive" />
              </Link>
            )}
            {server ? (
              <Link
                className="docker-metric-link"
                to="/servers/$serverId/docker/containers"
                params={{ serverId: server.id }}
                search={{ attention: true }}
              >
                {needsAttention}
              </Link>
            ) : (
              <Link
                className="docker-metric-link"
                to="/servers/docker"
                search={{ view: "containers", attention: true }}
              >
                {needsAttention}
              </Link>
            )}
          </div>
          <div className="docker-overview-grid">
            {server ? (
              <DiskUsePanel server={server} dockerWorkspace />
            ) : (
              <Panel title={t`Docker disk usage`} actions={<Icon name="archive" />}>
                <ul className="docker-servers">
                  {rows.map((row) => {
                    const name = servers.find((s) => s.id === row.serverId)?.name ?? row.serverId;
                    return (
                      <li key={row.serverId}>
                        <Link className="dw-link" to="/servers/$serverId/docker" params={{ serverId: row.serverId }}>
                          {name}
                        </Link>
                      </li>
                    );
                  })}
                </ul>
                <RouterButtonLink
                  to="/servers/docker"
                  search={{ view: "cleanup" }}
                  icon="trash"
                  variant="secondary"
                  size="sm"
                >
                  <Trans>Review cleanup</Trans>
                </RouterButtonLink>
              </Panel>
            )}
            <Panel title={t`Engine inventory`} actions={<Icon name="layers" size={18} />}>
              <dl className="docker-facts">
                <Fact
                  label={t`Networks`}
                  value={
                    server ? (
                      <Link
                        className="dw-link"
                        to="/servers/$serverId/docker/networks"
                        params={{ serverId: server.id }}
                      >
                        {sum("networks")}
                      </Link>
                    ) : (
                      <Link className="dw-link" to="/servers/docker" search={{ view: "networks" }}>
                        {sum("networks")}
                      </Link>
                    )
                  }
                />
                <Fact
                  label={t`Volumes`}
                  value={
                    server ? (
                      <Link className="dw-link" to="/servers/$serverId/docker/volumes" params={{ serverId: server.id }}>
                        {sum("volumes")}
                      </Link>
                    ) : (
                      <Link className="dw-link" to="/servers/docker" search={{ view: "volumes" }}>
                        {sum("volumes")}
                      </Link>
                    )
                  }
                />
                {server && rows[0] ? <EngineFacts summary={rows[0]} /> : null}
              </dl>
            </Panel>
          </div>
          {attention.length ? (
            <Panel title={t`Needs attention`}>
              <ul className="docker-servers">
                {attention.map((raw) => {
                  const row = rowWithHealth(raw, health);
                  const key = row.key;
                  if (!key) return null;
                  return (
                    <li key={`${key.serverId}/${key.engineId}`}>
                      <Link
                        className="dw-link docker-row-link"
                        to="/servers/$serverId/docker/containers/$containerId"
                        params={{ serverId: key.serverId, containerId: key.engineId }}
                        search={{ name: row.name }}
                      >
                        {row.name}
                      </Link>
                      <ContainerStatus row={row} />
                      {server ? null : <span>{servers.find((s) => s.id === key.serverId)?.name ?? key.serverId}</span>}
                    </li>
                  );
                })}
              </ul>
            </Panel>
          ) : null}
          {!server ? (
            <Panel title={t`Servers`}>
              <ul className="docker-servers">
                {rows.map((row) => (
                  <li key={row.serverId}>
                    <Link className="dw-link" to="/servers/$serverId/docker" params={{ serverId: row.serverId }}>
                      {servers.find((s) => s.id === row.serverId)?.name ?? row.serverId}
                    </Link>
                    <span>
                      <Trans>
                        {row.running} of {row.containers} running
                      </Trans>
                    </span>
                    <code>{row.engineVersion}</code>
                  </li>
                ))}
              </ul>
            </Panel>
          ) : null}
        </>
      ) : null}
    </>
  );
}

function EngineFacts({ summary }: { summary: DockerServerSummary }) {
  const { t } = useLingui();
  return (
    <>
      {summary.engineVersion ? <Fact label={t`Engine version`} value={summary.engineVersion} mono /> : null}
      {summary.rootDirectory ? <Fact label={t`Docker root directory`} value={summary.rootDirectory} mono /> : null}
      {summary.loggingDriver ? <Fact label={t`Logging driver`} value={summary.loggingDriver} mono /> : null}
      {summary.liveRestore !== undefined ? (
        <Fact label={t`Live restore`} value={summary.liveRestore ? t`On` : t`Off`} />
      ) : null}
      {summary.registryMirrors.length ? (
        <Fact
          label={t`Registry mirrors`}
          value={
            <ul className="docker-detail-list">
              {summary.registryMirrors.map((mirror) => (
                <li key={mirror}>
                  <code>{mirror}</code>
                </li>
              ))}
            </ul>
          }
        />
      ) : null}
    </>
  );
}
