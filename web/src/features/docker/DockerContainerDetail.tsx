// SPDX-License-Identifier: AGPL-3.0-only

import { toJson } from "@bufbuild/protobuf";
import { timestampDate, timestampMs } from "@bufbuild/protobuf/wkt";
import { useQuery } from "@connectrpc/connect-query";
import {
  type DockerContainer,
  type DockerObject,
  DockerObjectKind,
  DockerObjectSchema,
  DockerOwnership,
  type DockerSample,
  DockerService,
  LogService,
} from "@dockwell/api";
import { Button, Callout, Icon, Metric, Panel, TabNav } from "@dockwell/ui";
import { Trans, useLingui } from "@lingui/react/macro";
import { Link } from "@tanstack/react-router";
import { useRef } from "react";

import { sentence } from "../../lib/errors";
import { formatBytes, formatPercent } from "../../lib/format";
import { RouterButtonLink, RouterTabLink } from "../../lib/links";
import { useTimeWords } from "../../lib/time";
import { useMay } from "../auth/may";
import { LogsPage, type LogsSearch, readLogsSearch } from "../logs/LogsPage";
import { LogsSearchButton } from "../logs/phoneSearch";
import { CopyButton } from "../servers/CopyButton";
import { mountSource } from "../servers/containers";
import { useControlPlaneNow, useFleetView } from "../servers/useFleet";
import { EmptyState } from "../shell/EmptyState";
import { Page } from "../shell/Page";
import { ContainerActions, ContainerStatus, dockerPort, Ownership } from "./DockerContainers";
import { dockerKey, rowWithHealth, useDockerWatch } from "./live";
import { containerOf, Fact, LiveUpdates } from "./parts";

import "./Docker.css";

const detail = "/servers/$serverId/docker/containers/$containerId";
const inventory = "/servers/$serverId/docker/containers";

export interface ContainerDetailSearch extends LogsSearch {
  name?: string;
  view?: "inspect" | "logs";
}

export function readContainerDetailSearch(search: Record<string, unknown>): ContainerDetailSearch {
  const result: ContainerDetailSearch = readLogsSearch(search);
  if (typeof search["name"] === "string" && /^[A-Za-z0-9][A-Za-z0-9_.-]{0,254}$/.test(search["name"])) {
    result.name = search["name"];
  }
  if (search["view"] === "inspect" || search["view"] === "logs") result.view = search["view"];
  return result;
}

export function DockerContainerDetail({
  serverId,
  containerId,
  search,
  onSearch,
}: {
  serverId: string;
  containerId: string;
  search: ContainerDetailSearch;
  onSearch: (search: ContainerDetailSearch) => void;
}) {
  const { t } = useLingui();
  const may = useMay();
  const allowed = may(DockerService.method.getDockerObject).allowed;
  const valid = /^[0-9a-f]{64}$/.test(containerId);
  const server = useFleetView().servers?.find((s) => s.id === serverId);
  const query = useQuery(
    DockerService.method.getDockerObject,
    { key: { serverId, engineId: containerId, kind: DockerObjectKind.CONTAINER }, lastKnownName: search.name ?? "" },
    { enabled: allowed && valid, retry: false, refetchOnWindowFocus: false },
  );
  const raw = query.error ? undefined : query.data?.object;
  const { samples, connected, health, paused, retry } = useDockerWatch(
    allowed && valid ? [serverId] : [],
    raw?.key ? [raw.key] : [],
    () => void query.refetch(),
  );
  const object = raw ? rowWithHealth(raw, health) : undefined;
  const status = object ? (
    <span className="docker-header-status">
      <ContainerStatus row={object} />
      <Ownership row={object} />
    </span>
  ) : undefined;
  const sample = connected ? samples.get(dockerKey(serverId, containerId)) : undefined;
  const searchButton = useRef<(() => void) | null>(null);
  const params = { serverId, containerId };
  const label = object?.name ?? search.name ?? t`Container`;
  const supportedLogs =
    object &&
    may(LogService.method.watchLogs).allowed &&
    object.ownership !== DockerOwnership.SYSTEM &&
    (object.ownership !== DockerOwnership.MANAGED ||
      (object.managedResource?.kind === "service" && !!object.managedResourceId));
  const view = search.view;
  const container = containerOf(object);
  const replacement = query.data?.replacementKey;
  const hint = object ? { name: object.name } : search.name ? { name: search.name } : {};
  return (
    <LogsSearchButton value={searchButton}>
      <Page
        title={label}
        machineTitle={!!(object || search.name)}
        badge={status}
        lead={container?.image ? <code>{container.image}</code> : undefined}
        phoneSub={status}
        backLink={
          <Link
            to={inventory}
            params={{ serverId }}
            className="mobile-header-button"
            aria-label={t`Back to containers`}
          >
            <Icon name="arrow-left" size={20} />
          </Link>
        }
        breadcrumb={
          <nav className="page-breadcrumbs" aria-label={t`Breadcrumb`}>
            <Link to="/servers/$serverId/docker" params={{ serverId }}>
              {server?.name ?? t`Docker`}
            </Link>
            <span aria-hidden="true">/</span>
            <Link to={inventory} params={{ serverId }}>
              <Trans>Containers</Trans>
            </Link>
          </nav>
        }
        actions={object ? <ContainerActions row={object} /> : undefined}
        phoneAction={
          view === "logs" && supportedLogs ? (
            <button
              type="button"
              className="mobile-header-button"
              aria-label={t`Search logs`}
              onClick={() => searchButton.current?.()}
            >
              <Icon name="search" size={20} />
            </button>
          ) : undefined
        }
        fullBody={view === "logs" && !!supportedLogs}
      >
        <LiveUpdates paused={paused} retry={retry} />
        {!allowed || !valid ? (
          <EmptyState
            icon={allowed ? "service" : "lock"}
            title={allowed ? t`Open a container from Docker` : t`Docker inventory needs an admin`}
            action={
              <RouterButtonLink to={inventory} params={{ serverId }}>
                <Trans>Open containers</Trans>
              </RouterButtonLink>
            }
          />
        ) : query.error ? (
          <Callout
            tone="warning"
            title={<Trans>Container couldn't be inspected</Trans>}
            actions={
              <Button size="sm" onClick={() => void query.refetch()}>
                <Trans>Try again</Trans>
              </Button>
            }
          >
            {sentence(query.error)}
          </Callout>
        ) : query.isPending ? (
          <p role="status">
            <Trans>Reading container</Trans>
          </p>
        ) : object && container ? (
          <>
            <div className="docker-detail-nav">
              <TabNav label={t`Container views`}>
                <RouterTabLink
                  to={detail}
                  params={params}
                  search={hint}
                  activeOptions={{ exact: true, includeSearch: true }}
                  activeProps={{ className: view === undefined ? "is-active" : "" }}
                >
                  <Trans>Summary</Trans>
                </RouterTabLink>
                <RouterTabLink
                  to={detail}
                  params={params}
                  search={{ ...hint, view: "inspect" }}
                  activeOptions={{ exact: true, includeSearch: true }}
                >
                  <Trans>Inspect</Trans>
                </RouterTabLink>
                {supportedLogs ? (
                  <RouterTabLink
                    to={detail}
                    params={params}
                    search={{ ...hint, view: "logs" }}
                    activeOptions={{ exact: true, includeSearch: true }}
                  >
                    <Trans>Logs</Trans>
                  </RouterTabLink>
                ) : null}
              </TabNav>
            </div>
            {view === "inspect" ? (
              <Inspect object={object} container={container} />
            ) : view === "logs" && supportedLogs ? (
              <LogsPage
                key={containerId}
                id={object.managedResourceId || containerId}
                server={object.managedResourceId ? "" : serverId}
                sourceId={containerId.slice(0, 12)}
                onInspectLogging={() => onSearch({ name: object.name, view: "inspect" })}
                search={search}
                onSearch={(next) => onSearch({ ...next, name: object.name, view: "logs" })}
              />
            ) : (
              <Summary object={object} container={container} sample={sample} />
            )}
          </>
        ) : (
          <EmptyState
            icon="service"
            title={<Trans>This container is gone</Trans>}
            action={
              replacement ? (
                <RouterButtonLink to={detail} params={{ serverId, containerId: replacement.engineId }} search={hint}>
                  <Trans>Open replacement</Trans>
                </RouterButtonLink>
              ) : (
                <RouterButtonLink to={inventory} params={{ serverId }}>
                  <Trans>Open containers</Trans>
                </RouterButtonLink>
              )
            }
          />
        )}
      </Page>
    </LogsSearchButton>
  );
}

function Summary({
  object,
  container,
  sample,
}: {
  object: DockerObject;
  container: DockerContainer;
  sample?: DockerSample | undefined;
}) {
  const { t, i18n } = useLingui();
  const now = useControlPlaneNow(5_000);
  const times = useTimeWords();
  const at = sample?.observedAt;
  const live = at && now - timestampMs(at) < 25_000 ? sample : undefined;
  const bytes = (n?: bigint) => (n === undefined ? t`Unavailable` : formatBytes(n, i18n.locale));
  const unknown = t`Unavailable`;
  const sampled = live?.observedAt ? t`Sampled ${times.ago(timestampDate(live.observedAt), now)}` : undefined;
  return (
    <div className="docker-detail-body">
      <div className="docker-metrics">
        <Metric
          label={t`CPU`}
          value={live?.cpuPercent === undefined ? unknown : formatPercent(live.cpuPercent, i18n.locale)}
          icon="cpu"
          caption={live?.cpuPercent === undefined ? undefined : t`Share of one CPU`}
        />
        <Metric label={t`Memory`} value={bytes(live?.memoryBytes)} icon="gauge" caption={sampled} />
        <Metric label={t`Restarts`} value={String(container.restartCount)} icon="refresh" />
      </div>
      <div className="docker-overview-grid">
        <Panel title={t`Lifecycle`}>
          <dl className="docker-facts">
            <Fact label={t`Container ID`} value={object.key?.engineId ?? ""} mono />
            <Fact
              label={t`Created`}
              value={container.createdAt ? times.ago(timestampDate(container.createdAt), now) : unknown}
            />
            <Fact
              label={t`Started`}
              value={container.startedAt ? times.ago(timestampDate(container.startedAt), now) : t`Never started`}
            />
            {container.finishedAt ? (
              <Fact label={t`Last exit`} value={times.ago(timestampDate(container.finishedAt), now)} />
            ) : null}
            {container.exitCode !== undefined ? <Fact label={t`Exit code`} value={container.exitCode} mono /> : null}
            {container.oomKilled ? <Fact label={t`Exit reason`} value={t`Out of memory`} /> : null}
          </dl>
        </Panel>
        <Panel title={t`I/O`}>
          <dl className="docker-facts">
            <Fact label={t`Received`} value={bytes(live?.networkRxBytes)} />
            <Fact label={t`Sent`} value={bytes(live?.networkTxBytes)} />
            <Fact label={t`Read from disk`} value={bytes(live?.blockReadBytes)} />
            <Fact label={t`Written to disk`} value={bytes(live?.blockWriteBytes)} />
          </dl>
        </Panel>
      </div>
      <Attachments container={container} />
    </div>
  );
}

function Attachments({ container }: { container: DockerContainer }) {
  const { t } = useLingui();
  return (
    <div className="docker-overview-grid">
      <Panel title={t`Published ports`}>
        {container.ports.length ? (
          <ul className="docker-detail-list">
            {container.ports.map((port) => (
              <li key={dockerPort(port)}>
                <code>{dockerPort(port)}</code>
              </li>
            ))}
          </ul>
        ) : (
          <span className="docker-muted">
            <Trans>No ports published</Trans>
          </span>
        )}
      </Panel>
      <Panel title={t`Networks`}>
        {container.endpoints.length ? (
          <dl className="docker-facts">
            {container.endpoints.map((n) => (
              <Fact
                key={n.network}
                label={n.network}
                value={[n.ipv4, n.ipv6].filter(Boolean).join(" · ") || t`No address`}
                mono
              />
            ))}
          </dl>
        ) : (
          <span className="docker-muted">
            <Trans>No network attachments</Trans>
          </span>
        )}
      </Panel>
      <Panel title={t`Mounts`}>
        {container.mounts.length ? (
          <dl className="docker-facts">
            {container.mounts.map((m) => (
              <Fact
                key={m.target}
                label={m.target}
                value={
                  <>
                    {mountSource(m)}
                    {m.readOnly ? (
                      <>
                        {" "}
                        · <Trans>Read only</Trans>
                      </>
                    ) : null}
                  </>
                }
                mono
              />
            ))}
          </dl>
        ) : (
          <span className="docker-muted">
            <Trans>No mounts</Trans>
          </span>
        )}
      </Panel>
    </div>
  );
}

function Inspect({ object, container }: { object: DockerObject; container: DockerContainer }) {
  const { t, i18n } = useLingui();
  const json = JSON.stringify(toJson(DockerObjectSchema, object), null, 2);
  return (
    <div className="docker-detail-body">
      <div className="docker-overview-grid">
        <Panel title={t`Runtime`}>
          <dl className="docker-facts">
            <Fact label={t`Image ID`} value={container.imageId || t`Unavailable`} mono />
            <Fact label={t`Compose project`} value={container.composeProject || t`None`} mono />
            <Fact label={t`Compose service`} value={container.composeService || t`None`} mono />
            <Fact label={t`Restart policy`} value={container.restartPolicy || "no"} mono />
            {container.restartPolicy === "on-failure" ? (
              <Fact label={t`Restart retries`} value={container.restartRetries || t`Unlimited`} />
            ) : null}
            <Fact
              label={t`CPU limit`}
              value={container.cpuLimitNanos ? t`${Number(container.cpuLimitNanos) / 1e9} CPUs` : t`Unlimited`}
            />
            <Fact
              label={t`Memory limit`}
              value={container.memoryLimitBytes ? formatBytes(container.memoryLimitBytes, i18n.locale) : t`Unlimited`}
            />
          </dl>
        </Panel>
        <Panel title={t`Logging`}>
          <dl className="docker-facts">
            <Fact label={t`Driver`} value={container.logDriver || t`Engine default`} mono />
            <Fact
              label={t`Maximum file size`}
              value={container.logMaxSize || (container.logDriver === "json-file" ? t`Unlimited` : t`Driver default`)}
              mono
            />
            <Fact label={t`Maximum files`} value={container.logMaxFiles || t`Driver default`} />
          </dl>
        </Panel>
      </div>
      {container.configuredPorts.length ? (
        <Panel title={t`Configured port bindings`}>
          <ul className="docker-detail-list">
            {container.configuredPorts.map((port) => (
              <li key={dockerPort(port)}>
                <code>{dockerPort(port)}</code>
              </li>
            ))}
          </ul>
        </Panel>
      ) : null}
      {container.detailsTruncated ? (
        <Callout tone="warning" title={<Trans>Docker returned more attachments than fit in this view</Trans>} />
      ) : null}
      <Panel title={t`Inspect data`} actions={<CopyButton text={json} label={t`Copy redacted inspection data`} />}>
        <details className="docker-json">
          <summary>
            <Trans>Show redacted JSON</Trans>
          </summary>
          <section className="docker-json-scroll" aria-label={t`Redacted inspection data`}>
            <pre>{json}</pre>
          </section>
        </details>
      </Panel>
    </div>
  );
}
