// SPDX-License-Identifier: AGPL-3.0-only

import { timestampDate } from "@bufbuild/protobuf/wkt";
import { createClient } from "@connectrpc/connect";
import { useQuery, useTransport } from "@connectrpc/connect-query";
import {
  ChangeState,
  Connection,
  type Container,
  type Domain,
  type DomainRoute,
  EventKind,
  EventService,
  HealthCheckKind,
  type Mount,
  Protocol,
  RestartPolicy,
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
  Tag,
  Timeline,
  type TimelineEvent,
  useMediaQuery,
} from "@dockwell/ui";
import { plural } from "@lingui/core/macro";
import { Trans, useLingui } from "@lingui/react/macro";
import { Link, useNavigate, useSearch } from "@tanstack/react-router";
import { type CSSProperties, useEffect, useState } from "react";

import { sentence } from "../../lib/errors";
import { RouterButtonLink } from "../../lib/links";
import { useTimeWords } from "../../lib/time";
import { happenedAt, useDescribe, useHealthEventWords } from "../activity/describe";
import { useChangeSheet } from "../changes/ChangeSheet";
import { MachineWords } from "../changes/FailureNotice";
import { withoutIds } from "../changes/plan";
import { CopyButton } from "../servers/CopyButton";
import { useAgoNow, useFleetView } from "../servers/useFleet";
import { AddDomainSheet, type MoveBehind } from "./AddDomainSheet";
import { AddPortSheet } from "./AddPortSheet";
import { useApp, useRevisions } from "./AppContext";
import {
  appOf,
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
import { DomainFacts, DomainNote, useSilent } from "./DomainNote";
import { lookOf, routeWhere, useAppDomains, visitorUrl } from "./domains";
import { ExportSheet } from "./ExportSheet";
import { HealthBadge, RunsOnServers, useRunsNow } from "./Health";
import { type AppHealth, runsAsFarAsAnyoneKnows, useHealth } from "./health";
import { ImageName, WholeImage } from "./ImageName";
import { useRevisionLook, useRevisionTitle, useWhoWords } from "./revision";
import { usePlaces } from "./useServices";
import { useAppWords } from "./words";

const RECENT = 5;

/**
 * One app at a glance (App-Overview, M-App): how it runs now and where it is reached,
 * how that has gone, its recent deployments, how it is configured, and what it runs
 * with. It says what is wrong first, with the way out. There are no metrics or replica
 * counts until the API has them.
 */
export function AppOverview() {
  const { service } = useApp();
  return (
    <div className="app-overview">
      <div className="app-main">
        <AppNotice />
        <RunningPanel />
        <RecentHealth />
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

/**
 * What a health state comes to, as a notice, where the record has nothing worse to say:
 * the fact, what follows and what to do. What its check said is in the panel below it.
 */
function HealthNotice({
  health,
  name,
  serviceId,
  restart,
}: {
  health: AppHealth;
  name: string;
  serviceId: string;
  restart: RestartPolicy;
}) {
  const { t } = useLingui();
  const { fleet, servers } = useFleetView();
  if (health.read !== "read") return null;
  const where = health.servers
    .filter((s) => s.kind === health.kind)
    .map((s) => s.serverName)
    .join(", ");
  const logs = (
    <RouterButtonLink to="/apps/$serviceId/logs" params={{ serviceId }}>
      <Trans>See its logs</Trans>
    </RouterButtonLink>
  );
  switch (health.kind) {
    case "not-running":
      return (
        <Callout
          tone="critical"
          title={fleet ? t`${name} isn't running on ${where}` : t`${name} isn't running`}
          actions={logs}
        >
          {/* What says none gets unless-stopped, as its Restarts row says. */}
          {restart === RestartPolicy.ALWAYS ||
          restart === RestartPolicy.UNLESS_STOPPED ||
          restart === RestartPolicy.UNSPECIFIED ? (
            <Trans>
              Docker restarts it only when it stops on its own, so someone stopped it. Restart it, or deploy.
            </Trans>
          ) : restart === RestartPolicy.ON_FAILURE ? (
            <Trans>Docker restarts it only when it exits with an error, and doesn't now. Restart it, or deploy.</Trans>
          ) : (
            <Trans>Nothing starts it again on its own. Restart it, or deploy.</Trans>
          )}
        </Callout>
      );
    case "unhealthy":
      return (
        <Callout
          tone="warning"
          title={fleet ? t`${name} fails its health check on ${where}` : t`${name} fails its health check`}
          actions={logs}
        >
          <Trans>
            It stays up until someone changes something. Fix what its check reports, or the check, then deploy.
          </Trans>
        </Callout>
      );
    case "paused":
      return (
        <Callout tone="warning" title={fleet ? t`${name} is paused on ${where}` : t`${name} is paused`}>
          <Trans>
            It serves nothing until someone unpauses it, and restarting or deploying it also puts that right.
          </Trans>
        </Callout>
      );
    case "unknown": {
      const unknown = health.servers.filter((s) => s.kind === "unknown");
      const blind = unknown.find((s) => s.dockerUnreadableSince);
      const gone = unknown.find((s) =>
        servers?.some((f) => f.id === s.serverId && f.connection !== Connection.CONNECTED),
      );
      const server = (blind ?? gone ?? unknown[0])?.serverName ?? "";
      return (
        <Callout tone="warning" title={t`Nobody can say how ${name} runs`}>
          {blind ? (
            <Trans>
              The agent on {server} can't read Docker, so it can't say what runs there. Check that Docker is running on
              the server, with <code>systemctl status docker</code>.
            </Trans>
          ) : gone ? (
            <Trans>
              It may well be running. Check that <code>dockwell run</code> is still running on {server}.
            </Trans>
          ) : (
            <Trans>
              The agent on {server} is connected, and hasn't said how it runs lately. It may well be running. If this
              goes on, read the agent's log on the server, with <code>journalctl -u dockwell</code>.
            </Trans>
          )}
        </Callout>
      );
    }
    case "healthy":
    case "running":
    case "starting":
      return null;
  }
}

/** The fact, then what it means, then what to do, when anything needs saying. */
function AppNotice() {
  const { t } = useLingui();
  const { service, state, health } = useApp();
  const sheet = useChangeSheet();
  const name = nameOf(service);
  const servers = state.servers.join(", ");
  const image = imageOf(service);
  const revision = revisionName(service.revision);
  const runsNow = useRunsNow(health, name);
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
          {health.read !== "read" ? (
            <Trans>
              {name} may not be running, or may be running the new version. Check it, then deploy what should run, or
              fix it on the server.
            </Trans>
          ) : runsNow ? (
            <>
              {runsNow} <Trans>Deploy what should run, or fix it on the server.</Trans>
            </>
          ) : (
            <Trans>
              Something of {name} runs, and Dockwell can't say which version. Check it, then deploy what should run, or
              fix it on the server.
            </Trans>
          )}
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
          {runsAsFarAsAnyoneKnows(health) ? (
            <Trans>
              {name} is on <code>{image}</code>, as it was before that deploy. Fix what stopped the deploy, then deploy
              again.
            </Trans>
          ) : (
            <>
              <Trans>
                Dockwell put <code>{image}</code> back, as it was before that deploy.
              </Trans>{" "}
              {runsNow} <Trans>Fix what stopped the deploy, then deploy again.</Trans>
            </>
          )}
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
            Dockwell can't see {name} there or change it until its agent reconnects. It may well be running. Check that{" "}
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
  return (
    <HealthNotice
      health={health}
      name={name}
      serviceId={service.id}
      restart={appOf(service)?.restart ?? RestartPolicy.UNSPECIFIED}
    />
  );
}

/**
 * How it runs now, by what its servers say, and where it is reached. Who deployed it
 * and when is the page's lead and the timeline's first line, so wide screens lead here
 * with the address; a phone has no lead, so it says it here. During a deploy the image
 * that runs is the one before it, until it is replaced.
 */
function RunningPanel() {
  const { t } = useLingui();
  const { service, state, health } = useApp();
  const phone = useMediaQuery(PHONE_QUERY);
  const { fleet } = useFleetView();
  const who = useWhoWords();
  const time = useTimeWords();
  const now = useAgoNow(deployedAt(service));
  const revisions = useRevisions(service.id).revisions;
  const app = appOf(service);
  const image = imageOf(service);
  const revision = revisionName(service.revision);
  const when = service.updateTime ? time.ago(timestampDate(service.updateTime), now) : "";
  const moving = isMoving(state);
  const [addingPort, setAddingPort] = useState(false);
  const [addingDomain, setAddingDomain] = useState(false);
  // An arrival sends you here to add one. Going back or reading the page again shouldn't open it again.
  const navigate = useNavigate();
  const { add }: { add?: "domain" | "port" } = useSearch({ strict: false });
  useEffect(() => {
    if (!add) return;
    if (add === "domain") setAddingDomain(true);
    if (add === "port") setAddingPort(true);
    void navigate({ to: "/apps/$serviceId", params: { serviceId: service.id }, search: {}, replace: true });
  }, [add, navigate, service.id]);
  // Once something reaches it, the ways to add another sit up here.
  const { domains, failed: domainsUnread } = useAppDomains(service);
  const [movingBehind, setMovingBehind] = useState<MoveBehind>();
  // What it couldn't read may well be there, so the ways to add another stay in reach.
  const reached = (app?.ports.length ?? 0) > 0 || domains.length > 0 || domainsUnread;
  // While it deploys, what runs is the revision before it, if there is one.
  const before = moving
    ? revisions?.find((r) => r.number < service.revision && !r.deleted && r.image !== "")
    : undefined;
  // While a rollback runs or waits, what is there is what nobody can say yet.
  const rollingBack = (moving && service.rolledBackChangeId !== "") || state.kind === "rollback-failed";
  const running = rollingBack ? "" : moving ? bareImage(before?.image ?? "") : image;
  const away = state.servers.join(", ");
  return (
    <Panel
      title={t`What runs`}
      actions={
        // The title carries the state on wide screens; the phone's header has only the word.
        <span className="app-running-actions">
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
      <RunsOnServers health={health} fleet={fleet} />
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
        onMoveBehind={setMovingBehind}
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
      {addingPort ? <AddPortSheet service={service} onClose={() => setAddingPort(false)} /> : null}
      {addingDomain ? <AddDomainSheet service={service} onClose={() => setAddingDomain(false)} /> : null}
      {movingBehind ? (
        <AddDomainSheet service={service} start={movingBehind} onClose={() => setMovingBehind(undefined)} />
      ) : null}
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
function DomainRow({
  domain,
  route,
  onRemove,
  onMoveBehind,
}: {
  domain: Domain;
  route: DomainRoute;
  onRemove: () => void;
  onMoveBehind: () => void;
}) {
  const { t } = useLingui();
  const look = lookOf(domain);
  const silent = useSilent(look);
  const where = routeWhere(domain, route);
  // Its certificate can't come, and a tunnel or a proxy in front may be why.
  const mayBeBehind = /set tls to off/.test(look.message);
  return (
    <li
      className={cx(
        "app-reach-row",
        "app-reach-domain",
        look.state === "failed" && "is-failed",
        look.warning && "is-warning",
      )}
    >
      <Icon name="globe" size={16} />
      <div className="app-reach-main">
        <code className="app-reach-at">{where}</code>
        <DomainFacts domain={domain} route={route} look={look} silent={silent} />
      </div>
      <div className="app-reach-acts">
        {look.state === "serving" ? (
          <a
            className="app-reach-open"
            href={visitorUrl(domain, route)}
            target="_blank"
            rel="noreferrer"
            aria-label={t`Open ${where}`}
          >
            <Icon name="external" size={16} />
          </a>
        ) : null}
        <Button variant="ghost" size="sm" icon="trash" aria-label={t`Remove ${where}`} onClick={onRemove} />
      </div>
      <p className="app-domain-note">
        <DomainNote domain={domain} look={look} silent={silent} />
        {mayBeBehind ? (
          <>
            {" "}
            <button type="button" className="app-panel-link" onClick={onMoveBehind}>
              <Trans>It's behind a tunnel or a proxy</Trans>
            </button>
          </>
        ) : null}
      </p>
    </li>
  );
}

function ReachedAt({
  app,
  service,
  onAddPort,
  onAddDomain,
  onMoveBehind,
}: {
  app: Container | undefined;
  service: ServiceInfo;
  onAddPort: () => void;
  onAddDomain: () => void;
  onMoveBehind: (start: MoveBehind) => void;
}) {
  const { t } = useLingui();
  const { servers } = useFleetView();
  const { domains, isPending, failed, retrying, retry } = useAppDomains(service);
  const sheet = useChangeSheet();
  const name = nameOf(service);
  const ports = app?.ports ?? [];
  // Couldn't read isn't none: it may well be reached at something this can't say.
  const unread = failed ? (
    <div className="app-reach-unread">
      <Callout
        tone="warning"
        title={t`Couldn't read this app's domains`}
        actions={
          <Button onClick={retry} loading={retrying}>
            <Trans>Try again</Trans>
          </Button>
        }
      >
        <Trans>It may be reached at more than this says. Nothing has changed.</Trans>
      </Callout>
    </div>
  ) : null;
  if (ports.length === 0 && domains.length === 0 && failed) return unread;
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
  const routes = domains.flatMap((domain) =>
    domain.routes.filter((route) => route.service === name).map((route) => ({ domain, route })),
  );
  const remove = (where: string, route: string) =>
    sheet.open({
      kind: "propose",
      summary: t`Remove ${where} from ${name}`,
      edits: [{ resource: { kind: "route", scope: environmentOf(service), name: route }, delete: true }],
    });
  return (
    <>
      {unread}
      <ul className="app-reach">
        {routes.map(({ domain, route }) => (
          <DomainRow
            key={route.id}
            domain={domain}
            route={route}
            onRemove={() => remove(routeWhere(domain, route), route.name)}
            onMoveBehind={() =>
              onMoveBehind({
                domain: domain.name,
                port: String(route.hostPort || route.port),
                route: route.name,
                routes: domain.routes,
              })
            }
          />
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
    </>
  );
}

/** How many of a service's health events its page shows. The server has told of each at most once in 5 minutes. */
const RECENT_HEALTH = 5;

/**
 * How it has run lately, as the timeline tells of it. A flapping app is one line every
 * few minutes that says how many times it changed. It says nothing while nothing has
 * gone wrong, and a page reads the same in that case as in one whose read failed, as
 * what runs now is said above.
 */
function RecentHealth() {
  const { t } = useLingui();
  const { service } = useApp();
  const words = useHealthEventWords();
  const describe = useDescribe();
  const time = useTimeWords();
  const events = useQuery(
    EventService.method.listEvents,
    { kinds: [EventKind.HEALTH], resourceId: service.id, pageSize: RECENT_HEALTH },
    { refetchInterval: 30_000 },
  );
  const list = events.data?.events ?? [];
  const newest = list[0]?.time;
  const now = useAgoNow(newest ? timestampDate(newest) : undefined);
  if (list.length === 0) return null;
  const items: TimelineEvent[] = list.map((event) => {
    const when = happenedAt(event);
    const look = words.look(event);
    return {
      id: event.id,
      kind: "config",
      icon: "activity",
      ...(look ? { status: look.status, statusLabel: look.word } : {}),
      title: describe(event, new Map()).title,
      meta: words.detail(event),
      time: time.ago(when, now),
      datetime: when.toISOString(),
    };
  });
  return (
    <Panel
      title={t`Recent health`}
      actions={
        <Link to="/activity" search={{ kind: "health", resource: service.id }} className="app-panel-link">
          <Trans>All of it</Trans>
        </Link>
      }
    >
      <Timeline className="app-timeline" events={items} />
    </Panel>
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
      ...((i === 0 || failed) && look.status ? { status: look.status, statusLabel: look.word } : {}),
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
  const { service, health } = useApp();
  const words = useAppWords();
  const [exporting, setExporting] = useState(false);
  const app = appOf(service);
  const values = valuesOf(service);
  const secrets = values.filter((v) => v.kind === "secret").length;
  const plain = values.length - secrets;
  const networks = app?.networks ?? [];
  const check = app?.healthcheck;
  const test = check?.test ?? [];
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
        <dt>{t`Health check`}</dt>
        <dd className="app-health-check">
          {check?.http ? (
            <span>
              <code>GET {check.http.path || "/"}</code> <Trans>on port {check.http.port}</Trans>
            </span>
          ) : check?.tcp ? (
            <span>
              <Trans>A connection to port {check.tcp.port}</Trans>
            </span>
          ) : test[0] === "NONE" ? (
            <span className="app-muted">{t`Turned off. Without one, Running only means it started.`}</span>
          ) : test.length > 0 ? (
            <code>{test.filter((part) => part !== "CMD" && part !== "CMD-SHELL").join(" ")}</code>
          ) : health.check === HealthCheckKind.COMMAND ? (
            <span>
              <Trans>The image's own</Trans>
            </span>
          ) : health.check === HealthCheckKind.NONE ? (
            <span className="app-muted">{t`None. Without one, Running only means it started.`}</span>
          ) : (
            <span className="app-muted">{t`None set here. The image may have one.`}</span>
          )}
        </dd>
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
  const places = usePlaces();
  const health = useHealth();
  const others = companions(service, services, places.place);
  if (others.length === 0) return null;
  return (
    <Panel title={t`Runs with`} flush>
      <ul className="app-companions">
        {others.map((other) => {
          return (
            <li key={other.id}>
              <Link to="/apps/$serviceId" params={{ serviceId: other.id }} className="app-companion">
                <span className="app-companion-text">
                  <span className="app-companion-name">{nameOf(other)}</span>
                  <ImageName image={imageOf(other)} className="app-companion-image" />
                </span>
                <HealthBadge health={health.of(other)} />
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
  // A route can't outlive what it serves, so its plan takes them with it, and lists them.
  const routes = useAppDomains(service).domains.flatMap((domain) =>
    domain.routes.filter((route) => route.service === name),
  );
  return (
    <Panel className="app-delete">
      <p className="app-delete-text">
        {routes.length > 0 ? (
          <Trans>
            Deleting {name} stops it, removes its container, and takes its domains with it. Its volumes stay, and you
            review a plan first.
          </Trans>
        ) : (
          <Trans>
            Deleting {name} stops it and removes its container. Its volumes stay, and you review a plan first.
          </Trans>
        )}
      </p>
      <Button
        icon="trash"
        onClick={() =>
          sheet.open({
            kind: "propose",
            summary: t`Delete ${name}`,
            edits: [
              ...routes.map((route) => ({
                resource: { kind: "route", scope: resource?.scope ?? "", name: route.name },
                delete: true,
              })),
              { resource: { kind: "service", scope: resource?.scope ?? "", name }, delete: true },
            ],
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
