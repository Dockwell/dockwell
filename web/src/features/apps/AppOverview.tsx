// SPDX-License-Identifier: AGPL-3.0-only

import { timestampDate } from "@bufbuild/protobuf/wkt";
import { createClient } from "@connectrpc/connect";
import { useTransport } from "@connectrpc/connect-query";
import {
  ChangeState,
  Connection,
  type Container,
  type Domain,
  type Mount,
  Protocol,
  type ServiceInfo,
  ServiceService,
} from "@dockwell/api";
import {
  Button,
  Callout,
  cx,
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
import { type CSSProperties, useState } from "react";

import { sentence } from "../../lib/errors";
import { useTimeWords } from "../../lib/time";
import { useChangeSheet } from "../changes/ChangeSheet";
import { MachineWords } from "../changes/FailureNotice";
import { withoutIds } from "../changes/plan";
import { CopyButton } from "../servers/CopyButton";
import { useAgoNow, useFleetView } from "../servers/useFleet";
import { AddDomainSheet } from "./AddDomainSheet";
import { AddPortSheet } from "./AddPortSheet";
import { useApp, useRevisions } from "./AppContext";
import {
  appOf,
  appState,
  bareImage,
  canRollBackTo,
  companions,
  deployedAt,
  environmentOf,
  imageOf,
  isDockerSocket,
  isMoving,
  nameOf,
  revisionName,
  valuesOf,
} from "./app";
import { asksNoCertificate, lookOf, useAppDomains } from "./domains";
import { ExportSheet } from "./ExportSheet";
import { ImageName, WholeImage } from "./ImageName";
import { useRevisionLook, useRevisionTitle, useWhoWords } from "./revision";
import { usePlaces } from "./useServices";
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
          shape="spinner"
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
            service.rolledBackChangeId !== "" ? (
              <Button onClick={() => openChange(service.rolledBackChangeId)}>
                <Trans>See why</Trans>
              </Button>
            ) : (
              <Button onClick={() => openChange(service.changeId)}>
                <Trans>Open the plan</Trans>
              </Button>
            )
          }
        >
          {service.rolledBackChangeId !== "" ? (
            <Trans>
              Once it reconnects, Dockwell puts <code>{image}</code> back, after a deploy that failed. Until then it
              can't tell what is running there.
            </Trans>
          ) : (
            <Trans>
              The deploy waits for its agent, and carries on once it reconnects. Check that <code>dockwell run</code> is
              still running on the server.
            </Trans>
          )}
        </Callout>
      );
    case "rollback-failed":
      return (
        <Callout
          tone="critical"
          title={t`Putting ${name} back failed too`}
          actions={
            <Button onClick={() => openChange(service.rolledBackChangeId)}>
              <Trans>See why</Trans>
            </Button>
          }
        >
          {state.error ? (
            <span>
              <MachineWords>{withoutIds(state.error)}</MachineWords>{" "}
            </span>
          ) : null}
          <Trans>
            {name} may not be running, or may be running the new version. Check it, then deploy what should run, or fix
            it on the server.
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
          {state.error ? (
            <span>
              <MachineWords>{withoutIds(state.error)}</MachineWords>{" "}
            </span>
          ) : null}
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
            {name} is on <code>{image}</code>, as it was before that deploy. Fix what stopped the deploy, then deploy
            again.
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
        <Callout tone="warning" title={t`${name} has no server in the fleet`}>
          <Trans>
            Each server it ran on has left the fleet, so Dockwell can't see it or change it there, and its containers
            may still be running on them. Deploy it to run it on a server in the fleet.
          </Trans>
        </Callout>
      );
  }
  return null;
}

/**
 * What runs now, and where it is reached. Who deployed it and when is the
 * page's lead and the timeline's first line, so wide screens lead here with
 * the address; a phone has no lead, so it says it here. During a deploy the
 * image that runs is the one before it, until it is replaced.
 */
function RunningPanel() {
  const { t } = useLingui();
  const { service, state } = useApp();
  const phone = useMediaQuery(PHONE_QUERY);
  const { fleet, servers } = useFleetView();
  const words = useAppWords();
  const who = useWhoWords();
  const time = useTimeWords();
  const now = useAgoNow(deployedAt(service));
  const revisions = useRevisions(service.id).revisions;
  const app = appOf(service);
  const image = imageOf(service);
  const revision = revisionName(service.revision);
  const when = service.updateTime ? time.ago(timestampDate(service.updateTime), now) : "";
  const stateWord = words.state(state);
  const moving = isMoving(state);
  const [addingPort, setAddingPort] = useState(false);
  const [addingDomain, setAddingDomain] = useState(false);
  // Once something reaches it, the ways to add another sit up here.
  const { domains } = useAppDomains(service);
  const reached = (app?.ports.length ?? 0) > 0 || domains.length > 0;
  // While it deploys, what runs is the revision before it, if there is one.
  const before = moving
    ? revisions?.find((r) => r.number < service.revision && !r.deleted && r.image !== "")
    : undefined;
  // While a rollback runs or waits, what is there is what nobody can say yet.
  const rollingBack = (moving && service.rolledBackChangeId !== "") || state.kind === "rollback-failed";
  const running = rollingBack ? "" : moving ? bareImage(before?.image ?? "") : image;
  const away = state.servers.join(", ");
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
        const one = appState({ deployments: [row], rolledBackChangeId: "", changeState: service.changeState }, servers);
        return (
          <StatusBadge
            status={online === Connection.CONNECTED ? one.status : "degraded"}
            label={online === Connection.CONNECTED ? words.state(one) : t`Offline`}
            labelIsState
          />
        );
      },
    },
  ];
  return (
    <Panel
      title={rollingBack ? t`What runs` : t`Running`}
      actions={
        // The title carries the state on wide screens; the phone's header has only the word.
        <span className="app-running-actions">
          {phone ? <StatusBadge status={state.status} label={stateWord} labelIsState variant="pill" /> : null}
          {reached ? (
            <>
              <button type="button" className="app-panel-link" onClick={() => setAddingDomain(true)}>
                <Trans>Add a domain</Trans>
              </button>
              <button type="button" className="app-panel-link" onClick={() => setAddingPort(true)}>
                <Trans>Add a port</Trans>
              </button>
            </>
          ) : null}
        </span>
      }
      flush
      className="app-running"
    >
      {phone ? (
        <p className="app-running-now">
          {state.kind === "rollback-failed" ? (
            <Trans>Rolling back failed {when}</Trans>
          ) : state.kind === "rolled-back" ? (
            <Trans>
              {revision} rolled back by Dockwell {when}
            </Trans>
          ) : moving ? (
            <Trans>
              {revision} started deploying {when}
            </Trans>
          ) : (
            who.revisionDeployed(revisions?.[0]?.change, revision, when)
          )}
        </p>
      ) : null}
      <ReachedAt
        app={app}
        service={service}
        onAddPort={() => setAddingPort(true)}
        onAddDomain={() => setAddingDomain(true)}
      />
      <div className="app-running-image">
        {running ? (
          <>
            <span className="app-running-label">
              <Trans>Image</Trans>
            </span>
            <WholeImage image={running} />
            <CopyButton text={running} label={t`Copy the image`} />
          </>
        ) : rollingBack ? (
          <>
            <span className="app-running-label">
              <Trans>Image</Trans>
            </span>
            <span className="app-muted">
              {state.kind === "rollback-failed" ? (
                <Trans>Unknown, as putting it back failed</Trans>
              ) : state.kind === "waiting" ? (
                <Trans>Unknown until {away} reconnects</Trans>
              ) : (
                <Trans>Unknown until this is put back</Trans>
              )}
            </span>
          </>
        ) : (
          <span className="app-muted">
            <Trans>Nothing yet. It runs once the deploy finishes.</Trans>
          </span>
        )}
      </div>
      {fleet ? (
        <Table
          className="app-running-servers"
          density="compact"
          columns={columns}
          rows={service.deployments.map((d) => ({ ...d, id: d.serverId }))}
          caption={t`Where it runs`}
        />
      ) : null}
      {addingPort ? <AddPortSheet service={service} onClose={() => setAddingPort(false)} /> : null}
      {addingDomain ? <AddDomainSheet service={service} onClose={() => setAddingDomain(false)} /> : null}
    </Panel>
  );
}

/**
 * Where an app is reached from outside its server: its published ports today.
 * Domains belong here too, first, when routes land (#27), and the way to add
 * one sits beside the way to add a port.
 */
/**
 * A domain the app is reached at: open once it serves; until then, what the server
 * says of its certificate, which it keeps trying by itself.
 */
function DomainRow({ domain }: { domain: Domain }) {
  const { t } = useLingui();
  const look = lookOf(domain);
  const name = domain.name;
  return (
    <li className={cx("app-reach-row", "app-reach-domain", look.state === "failed" && "is-failed")}>
      <Icon name="globe" size={16} />
      <code className="app-reach-at">{name}</code>
      <span className="app-reach-to">
        {look.noCertificate ? (
          <Trans>TLS ends before this server</Trans>
        ) : look.state === "serving" ? (
          <Trans>HTTPS</Trans>
        ) : (
          look.message ||
          (asksNoCertificate(domain) ? (
            <Trans>Waiting to hear from the server</Trans>
          ) : (
            <Trans>Getting its certificate</Trans>
          ))
        )}
      </span>
      {look.state === "serving" ? (
        <a className="app-reach-open" href={domain.url} target="_blank" rel="noreferrer" aria-label={t`Open ${name}`}>
          <Icon name="external" size={16} />
        </a>
      ) : null}
    </li>
  );
}

function ReachedAt({
  app,
  service,
  onAddPort,
  onAddDomain,
}: {
  app: Container | undefined;
  service: ServiceInfo;
  onAddPort: () => void;
  onAddDomain: () => void;
}) {
  const { t } = useLingui();
  const { servers } = useFleetView();
  const { domains, isPending } = useAppDomains(service);
  const ports = app?.ports ?? [];
  if (ports.length === 0 && domains.length === 0) {
    return (
      <div className="app-reach-none">
        <Icon name="globe" size={16} />
        <div className="app-reach-none-text">
          <p>
            {isPending ? null : (app?.networks.length ?? 0) > 0 ? (
              <Trans>Nothing outside the server reaches it yet. Other apps on its network do.</Trans>
            ) : (
              <Trans>Nothing outside the server reaches it yet.</Trans>
            )}
          </p>
          <div className="app-reach-actions">
            <Button icon="plus" onClick={onAddDomain}>
              <Trans>Add a domain</Trans>
            </Button>
            <Button icon="plus" onClick={onAddPort}>
              <Trans>Add a port</Trans>
            </Button>
          </div>
        </div>
      </div>
    );
  }
  const addresses = service.deployments.map((d) => servers?.find((s) => s.id === d.serverId)?.address ?? d.serverName);
  const rows = addresses.flatMap((address) =>
    ports.map((port) => {
      const local = port.hostIp === "127.0.0.1" || port.hostIp === "::1";
      const host = port.hostIp && port.hostIp !== "0.0.0.0" && port.hostIp !== "::" ? port.hostIp : address;
      const at = `${host.includes(":") ? `[${host}]` : host}:${port.hostPort}`;
      const udp = port.protocol === Protocol.UDP;
      // The same port over TCP and over UDP are two rows at one address.
      return { key: `${at}/${udp ? "udp" : "tcp"}`, at, local, target: port.containerPort, udp };
    }),
  );
  return (
    <ul className="app-reach">
      {domains.map((domain) => (
        <DomainRow key={domain.name} domain={domain} />
      ))}
      {rows.map((row) => {
        const { target, at } = row;
        return (
          <li key={row.key} className="app-reach-row">
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
  const now = useAgoNow(deployedAt(service));
  const sheet = useChangeSheet();
  const transport = useTransport();
  const lookOf = useRevisionLook();
  const revisionTitle = useRevisionTitle();
  const who = useWhoWords();
  const { revisions, isPending, error, refetch, isFetching } = useRevisions(service.id);
  const moving = isMoving(state);
  const shown = (revisions ?? []).slice(0, RECENT);
  // The newest revision that would run, and that differs from what runs now.
  const back = (revisions ?? [])
    .slice(1)
    .find((r) => canRollBackTo(r, state.kind === "failed" || state.kind === "rollback-failed"));
  const backTo = back ? revisionName(back.number) : "";
  const events: TimelineEvent[] = shown.map((rev, i) => {
    const change = rev.change;
    const when = rev.createTime ? timestampDate(rev.createTime) : new Date(0);
    const failed = change?.state === ChangeState.FAILED;
    const look = lookOf(rev, service.revision, state);
    return {
      id: rev.number.toString(),
      kind: change?.reverts || rev.rolledBackTo > 0n ? "rollback" : "deploy",
      ...(failed ? { tone: "critical" as const } : {}),
      // The state shows where it says something: on the one that is live, and on what didn't work.
      ...(i === 0 || failed ? { status: look.status, statusLabel: look.word } : {}),
      title: revisionTitle(rev).node,
      revision: revisionName(rev.number),
      actor: who.name(change),
      time: time.ago(when, now),
      datetime: when.toISOString(),
      // Nothing goes back while it is still moving. After a failure, going back
      // is how someone recovers.
      undoable: i === 0 && back !== undefined && !moving,
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
      {error && !revisions ? (
        <Callout
          tone="critical"
          title={t`Can't load the deployments`}
          actions={
            <Button onClick={() => void refetch()} loading={isFetching}>
              <Trans>Try again</Trans>
            </Button>
          }
        >
          {sentence(error)}
        </Callout>
      ) : isPending ? (
        // Holds the room what's coming will take, so nothing under it moves.
        <p
          className="app-note app-timeline-loading"
          aria-busy="true"
          style={{ "--app-timeline-rows": Math.min(Number(service.revision), RECENT) } as CSSProperties}
        >
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
  const values = valuesOf(service);
  const secrets = values.filter((v) => v.kind === "secret").length;
  const plain = values.length - secrets;
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
          {values.length > 0 ? (
            <Link to="/apps/$serviceId/environment" params={{ serviceId: service.id }}>
              {[
                plain > 0 ? plural(plain, { one: "# variable", other: "# variables" }) : "",
                secrets > 0 ? plural(secrets, { one: "# secret", other: "# secrets" }) : "",
              ]
                .filter(Boolean)
                .join(" · ")}
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
        {isPath ? (
          <Tag words>{isDockerSocket(mount.source.value ?? "") ? t`Docker's socket` : t`Server path`}</Tag>
        ) : null}
        {mount.readOnly ? <Tag words>{t`Read-only`}</Tag> : null}
      </span>
    </li>
  );
}

/** The others of its stack, each a link and a state. */
function RunsWithPanel() {
  const { t } = useLingui();
  const { service, services } = useApp();
  const { servers } = useFleetView();
  const places = usePlaces();
  const words = useAppWords();
  const others = companions(service, services, places.place);
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
                  <ImageName image={imageOf(other)} className="app-companion-image" />
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
    <Panel className="app-delete">
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
    </Panel>
  );
}
