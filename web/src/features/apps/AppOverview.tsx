// SPDX-License-Identifier: AGPL-3.0-only

import { timestampDate } from "@bufbuild/protobuf/wkt";
import { createClient } from "@connectrpc/connect";
import { useTransport } from "@connectrpc/connect-query";
import { ChangeState, Connection, type Container, type Mount, type ServiceInfo, ServiceService } from "@dockwell/api";
import {
  Button,
  Callout,
  Icon,
  Panel,
  PHONE_QUERY,
  StatusBadge,
  Table,
  type TableColumn,
  Tag,
  Timeline,
  type TimelineEvent,
  useMediaQuery,
} from "@dockwell/ui";
import { plural } from "@lingui/core/macro";
import { Trans, useLingui } from "@lingui/react/macro";
import { Link, useNavigate } from "@tanstack/react-router";
import { useState } from "react";

import { useTimeWords } from "../../lib/time";
import { useChangeSheet } from "../changes/ChangeSheet";
import { useChangeWords } from "../changes/words";
import { CopyButton } from "../servers/CopyButton";
import { useControlPlaneNow, useFleetView } from "../servers/useFleet";
import { useApp, useRevisions } from "./AppContext";
import {
  appOf,
  appState,
  canRollBackTo,
  companions,
  environmentOf,
  envNames,
  imageOf,
  nameOf,
  revisionName,
  tidyMessage,
} from "./app";
import { ExportSheet } from "./ExportSheet";
import { useRevisionLook, useRevisionTitle } from "./revision";
import { useAppWords } from "./words";

const RECENT = 5;

/**
 * One app at a glance (App-Overview, M-App): what runs and where it is
 * reached, its recent deployments, how it is configured, and what it runs
 * with. It says what is wrong first, with the way out. There are no metrics
 * or replica counts until the API has them (#28), and restarting isn't here
 * as nothing plans one yet.
 */
export function AppOverview() {
  const { service } = useApp();
  return (
    <div className="app-overview">
      <div className="app-main">
        <AppNotice />
        <RunningPanel />
        <RecentDeployments />
      </div>
      <div className="app-aside">
        <ConfigurationPanel />
        <VolumesPanel app={appOf(service)} />
        <RunsWithPanel />
        <DeletePanel />
      </div>
    </div>
  );
}

/** The fact, then what it means, then what to do, when anything needs saying. */
function AppNotice() {
  const { t } = useLingui();
  const { service, state } = useApp();
  const sheet = useChangeSheet();
  const name = nameOf(service);
  const servers = state.servers.join(", ");
  const image = imageOf(service);
  const revision = revisionName(service.revision);
  const openChange = (id: string) => sheet.open({ kind: "change", id });
  switch (state.kind) {
    case "deploying":
      return (
        <Callout
          tone="info"
          title={t`Deploying ${revision} of ${name}`}
          actions={
            <Button onClick={() => openChange(service.changeId)}>
              <Trans>Follow it</Trans>
            </Button>
          }
        >
          <Trans>It keeps going whether or not you stay here. Its steps show in the plan.</Trans>
        </Callout>
      );
    case "waiting":
      return (
        <Callout
          tone="warning"
          title={plural(state.servers.length, {
            one: `${servers} isn't connected`,
            other: `${servers} aren't connected`,
          })}
          actions={
            <Button onClick={() => openChange(service.changeId)}>
              <Trans>Open the plan</Trans>
            </Button>
          }
        >
          <Trans>
            The deploy waits for its agent, and carries on once it reconnects. Check that <code>dockwell run</code> is
            still running on the server.
          </Trans>
        </Callout>
      );
    case "failed":
      return (
        <Callout
          tone="critical"
          title={t`The last deploy of ${name} failed`}
          actions={
            <Button onClick={() => openChange(service.changeId)}>
              <Trans>Open the plan</Trans>
            </Button>
          }
        >
          {state.error ? <span>{tidyMessage(state.error)} </span> : null}
          <Trans>Fix what it says, then deploy again.</Trans>
        </Callout>
      );
    case "rolled-back":
      return (
        <Callout
          tone="warning"
          title={t`The last deploy failed, so Dockwell rolled it back`}
          actions={
            <Button onClick={() => openChange(service.rolledBackChangeId)}>
              <Trans>See why</Trans>
            </Button>
          }
        >
          <Trans>
            {name} runs <code>{image}</code> again, as it did before. Fix what stopped the deploy, then deploy again.
          </Trans>
        </Callout>
      );
    case "offline":
      return (
        <Callout
          tone="warning"
          title={plural(state.servers.length, {
            one: `${servers} isn't connected`,
            other: `${servers} aren't connected`,
          })}
        >
          <Trans>
            {name} keeps running there, but Dockwell can't see it or change it until its agent reconnects. Check that{" "}
            <code>dockwell run</code> is still running on the server.
          </Trans>
        </Callout>
      );
    case "unplaced":
      return (
        <Callout tone="warning" title={t`No server runs ${name}`}>
          <Trans>Each server it ran on has left the fleet. Deploy it to run it again.</Trans>
        </Callout>
      );
  }
  return null;
}

/** What runs now, and where it is reached. */
function RunningPanel() {
  const { t } = useLingui();
  const { service, state } = useApp();
  const phone = useMediaQuery(PHONE_QUERY);
  const { fleet, servers } = useFleetView();
  const words = useAppWords();
  const change = useChangeWords();
  const time = useTimeWords();
  const now = useControlPlaneNow(30_000);
  const latest = useRevisions(service.id).revisions?.[0]?.change;
  const app = appOf(service);
  const image = imageOf(service);
  const revision = revisionName(service.revision);
  const when = service.updateTime ? time.ago(timestampDate(service.updateTime), now) : "";
  const actor = latest?.appliedBy?.name || latest?.proposedBy?.name;
  const via = latest ? change.via(latest.appliedVia || latest.proposedVia) : "";
  const stateWord = words.state(state);
  const moving = state.kind === "deploying" || state.kind === "waiting";
  const columns: TableColumn<ServiceInfo["deployments"][number] & { id: string }>[] = [
    {
      key: "server",
      header: t`Server`,
      render: (row) => <code className="app-server">{row.serverName}</code>,
    },
    {
      key: "status",
      header: t`Status`,
      width: 160,
      render: (row) => {
        const online = servers?.find((s) => s.id === row.serverId)?.connection ?? Connection.CONNECTED;
        const one = appState({ deployments: [row], rolledBackChangeId: "" }, servers);
        return (
          <StatusBadge
            status={online === Connection.CONNECTED ? one.status : "degraded"}
            label={online === Connection.CONNECTED ? words.deployment(row.state) : t`Offline`}
            labelIsState
          />
        );
      },
    },
  ];
  return (
    <Panel
      title={t`Running`}
      // The title carries it on wide screens; the phone's header has only the word.
      actions={phone ? <StatusBadge status={state.status} label={stateWord} labelIsState variant="pill" /> : null}
      flush
      className="app-running"
    >
      <div className="app-running-now">
        <span className="app-running-line">
          {moving ? (
            <Trans>
              {revision} started deploying {when}
            </Trans>
          ) : actor ? (
            <Trans>
              {revision} deployed {when} by {actor}
            </Trans>
          ) : (
            <Trans>
              {revision} deployed {when}
            </Trans>
          )}
          {via ? <span className="app-muted"> · {via}</span> : null}
        </span>
        <span className="app-running-image">
          <code>{image}</code>
          <CopyButton text={image} label={t`Copy the image`} />
        </span>
      </div>
      <ReachedAt app={app} service={service} />
      {fleet ? (
        <Table
          className="app-running-servers"
          density="compact"
          columns={columns}
          rows={service.deployments.map((d) => ({ ...d, id: d.serverId }))}
          caption={t`Where it runs`}
        />
      ) : null}
    </Panel>
  );
}

/**
 * Where an app is reached from outside its server: its published ports today.
 * Domains belong here too, first, when routes land (#27).
 */
function ReachedAt({ app, service }: { app: Container | undefined; service: ServiceInfo }) {
  const { t } = useLingui();
  const { servers } = useFleetView();
  const ports = app?.ports ?? [];
  if (ports.length === 0) {
    return (
      <p className="app-reach-none">
        <Icon name="globe" size={16} />
        <span>
          {(app?.networks.length ?? 0) > 0 ? (
            <Trans>Nothing outside the server reaches it, as it publishes no port. Other apps on its network do.</Trans>
          ) : (
            <Trans>Nothing outside the server reaches it, as it publishes no port.</Trans>
          )}
        </span>
      </p>
    );
  }
  const addresses = service.deployments.map((d) => servers?.find((s) => s.id === d.serverId)?.address ?? d.serverName);
  const rows = addresses.flatMap((address) =>
    ports.map((port) => {
      const local = port.hostIp === "127.0.0.1" || port.hostIp === "::1";
      const host = port.hostIp && port.hostIp !== "0.0.0.0" && port.hostIp !== "::" ? port.hostIp : address;
      const at = `${host.includes(":") ? `[${host}]` : host}:${port.hostPort}`;
      return { at, local, target: port.containerPort, udp: port.protocol === 2 };
    }),
  );
  return (
    <ul className="app-reach">
      {rows.map((row) => {
        const { target, at } = row;
        return (
          <li key={row.at} className="app-reach-row">
            <Icon name="globe" size={16} />
            <code className="app-reach-at">{row.at}</code>
            <span className="app-reach-to">
              {row.udp ? <Trans>port {target}, UDP</Trans> : <Trans>port {target}</Trans>}
              {row.local ? <span> · {t`only on the server`}</span> : null}
            </span>
            {row.local || row.udp ? null : (
              <a
                className="app-reach-open"
                href={`http://${row.at}`}
                target="_blank"
                rel="noreferrer"
                aria-label={t`Open ${at}`}
              >
                <Icon name="external" size={16} />
              </a>
            )}
          </li>
        );
      })}
    </ul>
  );
}

/** The latest deployments, each with who did it, and a way back from the one that is live. */
function RecentDeployments() {
  const { t } = useLingui();
  const { service, state } = useApp();
  const name = nameOf(service);
  const time = useTimeWords();
  const now = useControlPlaneNow(30_000);
  const sheet = useChangeSheet();
  const transport = useTransport();
  const lookOf = useRevisionLook();
  const revisionTitle = useRevisionTitle();
  const { revisions, isPending } = useRevisions(service.id);
  const moving = state.kind === "deploying" || state.kind === "waiting";
  const shown = (revisions ?? []).slice(0, RECENT);
  // The newest revision that isn't the live one and would run.
  const back = (revisions ?? []).slice(1).find(canRollBackTo);
  const backTo = back ? revisionName(back.number) : "";
  const events: TimelineEvent[] = shown.map((rev, i) => {
    const change = rev.change;
    const when = rev.createTime ? timestampDate(rev.createTime) : new Date(0);
    const failed = change?.state === ChangeState.FAILED;
    const look = lookOf(rev, service.revision);
    return {
      id: rev.number.toString(),
      kind: change?.reverts ? "rollback" : "deploy",
      ...(failed ? { tone: "critical" as const } : {}),
      // The state shows where it says something: on the one that is live, and on what didn't work.
      ...(i === 0 || failed ? { status: look.status, statusLabel: look.word } : {}),
      title: revisionTitle(rev),
      revision: revisionName(rev.number),
      actor: change?.appliedBy?.name || change?.proposedBy?.name || "",
      time: time.ago(when, now),
      datetime: when.toISOString(),
      // Nothing goes back while it is still moving.
      undoable: i === 0 && back !== undefined && !failed && !moving,
      undoLabel: t`Roll back to ${backTo}`,
    };
  });
  return (
    <Panel
      title={t`Recent deployments`}
      actions={
        <Link to="/apps/$serviceId/deployments" params={{ serviceId: service.id }} className="app-panel-link">
          <Trans>All deployments</Trans>
        </Link>
      }
    >
      {isPending ? (
        <p className="app-note" aria-busy="true">
          <Trans>Loading deployments</Trans>
        </p>
      ) : (
        <Timeline
          className="app-timeline"
          events={events}
          onUndo={() => {
            if (!back) return;
            const target = revisionName(back.number);
            sheet.open({
              kind: "plan",
              make: () =>
                createClient(ServiceService, transport).rollBackService({
                  id: service.id,
                  revision: back.number,
                  summary: t`Roll back ${name} to ${target}`,
                }),
            });
          }}
        />
      )}
    </Panel>
  );
}

/** How it is set up, as far as that isn't the image's own. */
function ConfigurationPanel() {
  const { t } = useLingui();
  const { service } = useApp();
  const words = useAppWords();
  const [exporting, setExporting] = useState(false);
  const app = appOf(service);
  const names = envNames(app);
  const count = names.length;
  const networks = app?.networks ?? [];
  const health = app?.healthcheck?.test ?? [];
  return (
    <Panel
      title={t`Configuration`}
      actions={
        <button type="button" className="app-panel-link" onClick={() => setExporting(true)}>
          <Trans>Export as Compose</Trans>
        </button>
      }
    >
      <dl className="app-facts">
        <dt>{t`Command`}</dt>
        <dd>
          {app?.command.length ? (
            <code>{app.command.join(" ")}</code>
          ) : (
            <span className="app-muted">{t`The image's own`}</span>
          )}
        </dd>
        {app?.entrypoint.length ? (
          <>
            <dt>{t`Entrypoint`}</dt>
            <dd>
              <code>{app.entrypoint.join(" ")}</code>
            </dd>
          </>
        ) : null}
        {app?.user ? (
          <>
            <dt>{t`User`}</dt>
            <dd>
              <code>{app.user}</code>
            </dd>
          </>
        ) : null}
        {app?.workingDir ? (
          <>
            <dt>{t`Directory`}</dt>
            <dd>
              <code>{app.workingDir}</code>
            </dd>
          </>
        ) : null}
        <dt>{t`Restarts`}</dt>
        <dd>{words.restart(app?.restart ?? 0)}</dd>
        {health.length > 0 ? (
          <>
            <dt>{t`Health check`}</dt>
            <dd>
              <code>{health.filter((part) => part !== "CMD" && part !== "CMD-SHELL").join(" ")}</code>
            </dd>
          </>
        ) : null}
        <dt>{t`Environment`}</dt>
        <dd>
          {count > 0 ? (
            <Link to="/apps/$serviceId/environment" params={{ serviceId: service.id }}>
              {plural(count, { one: "# variable", other: "# variables" })}
            </Link>
          ) : (
            <span className="app-muted">{t`None set`}</span>
          )}
        </dd>
        {networks.length > 0 ? (
          <>
            <dt>{t`Networks`}</dt>
            <dd className="app-tags">
              {networks.map((network) => (
                <code key={network}>{network}</code>
              ))}
            </dd>
          </>
        ) : null}
      </dl>
      {exporting ? (
        <ExportSheet
          environment={environmentOf(service)}
          service={nameOf(service)}
          onClose={() => setExporting(false)}
        />
      ) : null}
    </Panel>
  );
}

/** What it keeps, and anything of the server's it reaches. */
function VolumesPanel({ app }: { app: Container | undefined }) {
  const { t } = useLingui();
  const mounts = app?.mounts ?? [];
  if (mounts.length === 0) return null;
  return (
    <Panel title={t`Volumes`} flush>
      <ul className="app-mounts">
        {mounts.map((mount) => (
          <MountRow key={mount.target} mount={mount} />
        ))}
      </ul>
    </Panel>
  );
}

function MountRow({ mount }: { mount: Mount }) {
  const { t } = useLingui();
  const isPath = mount.source.case === "hostPath";
  return (
    <li className="app-mount">
      <span className="app-mount-source">
        <Icon name={isPath ? "server" : "archive"} size={14} />
        <code>{mount.source.value}</code>
      </span>
      <span className="app-mount-target">
        <Icon name="arrow-right" size={12} />
        <code>{mount.target}</code>
      </span>
      <span className="app-mount-tags">
        {isPath ? <Tag>{t`Server path`}</Tag> : null}
        {mount.readOnly ? <Tag>{t`Read-only`}</Tag> : null}
      </span>
    </li>
  );
}

/** The others of its stack, each a link and a state. */
function RunsWithPanel() {
  const { t } = useLingui();
  const { service, services } = useApp();
  const { servers } = useFleetView();
  const words = useAppWords();
  const others = companions(service, services);
  if (others.length === 0) return null;
  return (
    <Panel title={t`Runs with`} flush>
      <ul className="app-companions">
        {others.map((other) => {
          const state = appState(other, servers);
          return (
            <li key={other.id}>
              <Link to="/apps/$serviceId" params={{ serviceId: other.id }} className="app-companion">
                <span className="app-companion-text">
                  <span className="app-companion-name">{nameOf(other)}</span>
                  <code className="app-companion-image">{imageOf(other)}</code>
                </span>
                <StatusBadge status={state.status} label={words.state(state)} labelIsState />
              </Link>
            </li>
          );
        })}
      </ul>
    </Panel>
  );
}

/** Deleting is a plan too: this only opens it. */
function DeletePanel() {
  const { t } = useLingui();
  const { service } = useApp();
  const sheet = useChangeSheet();
  const navigate = useNavigate();
  const name = nameOf(service);
  const resource = service.resource;
  return (
    <div className="app-delete">
      <p className="app-delete-text">
        <Trans>
          Deleting {name} stops it and removes its container. Its volumes stay, and you review a plan first.
        </Trans>
      </p>
      <Button
        icon="trash"
        onClick={() =>
          sheet.open({
            kind: "propose",
            summary: t`Delete ${name}`,
            edits: [{ resource: { kind: "service", scope: resource?.scope ?? "", name }, delete: true }],
            onClose: (change) => {
              if (change && change.state !== ChangeState.PLANNED && change.state !== ChangeState.FAILED) {
                void navigate({ to: "/apps" });
              }
            },
          })
        }
      >
        <Trans>Delete this app</Trans>
      </Button>
    </div>
  );
}
