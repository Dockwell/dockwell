// SPDX-License-Identifier: AGPL-3.0-only

import { timestampDate } from "@bufbuild/protobuf/wkt";
import { createClient } from "@connectrpc/connect";
import { useQuery, useTransport } from "@connectrpc/connect-query";
import {
  ChangeService,
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
  UpdateSource,
} from "@dockwell/api";
import {
  Button,
  Callout,
  cx,
  Icon,
  Panel,
  PHONE_QUERY,
  StatusBadge,
  Tag,
  Timeline,
  type TimelineEvent,
  useMediaQuery,
  useOff,
} from "@dockwell/ui";
import { plural } from "@lingui/core/macro";
import { Trans, useLingui } from "@lingui/react/macro";
import { Link, useNavigate, useSearch } from "@tanstack/react-router";
import { type CSSProperties, type ReactNode, useEffect, useState } from "react";

import { RouterButtonLink } from "../../lib/links";
import { useTimeWords } from "../../lib/time";
import { happenedAt, useDescribe, useHealthEventWords } from "../activity/describe";
import { WatchAddress } from "../alerts/WatchAddress";
import { type Offer, useMay } from "../auth/may";
import { AppBackups } from "../backups/AppBackups";
import { InstalledPanel } from "../catalog/InstalledPanel";
import { useChangeSheet } from "../changes/ChangeSheet";
import { MachineWords } from "../changes/FailureNotice";
import { withoutIds } from "../changes/plan";
import { useWaitingFor } from "../changes/waiting";
import { CopyButton } from "../servers/CopyButton";
import { PrivateTag } from "../servers/ServerAddress";
import { useAgoNow, useControlPlaneNow, useFleetView } from "../servers/useFleet";
import { ReadFailed } from "../shell/ReadFailed";
import { useRuntimes } from "../websites/website";
import { AddDomainSheet, type MoveBehind, moveBehindOf } from "./AddDomainSheet";
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
import { type DomainRead, lookOf, mayBeBehind, routeWhere, useAppDomains, visitorUrl } from "./domains";
import { ExportSheet } from "./ExportSheet";
import { HealthBadge, RunsOnServers, useHealthWords, useRunsNow } from "./Health";
import { type AppHealth, type RunsOnServer, runsAsFarAsAnyoneKnows, useHealth } from "./health";
import { ImageName, WholeImage } from "./ImageName";
import { RuntimeSheet } from "./RuntimeSheet";
import { useRevisionLook, useRevisionTitle, useWhoWords } from "./revision";
import { knownVersion, newerBuild, runtimeEnv, runtimeOf, runtimeWords } from "./runtime";
import { offered, UpdateCard } from "./updates";
import { UpgradeProgress } from "./upgrade";
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
  // A catalog install's update is its catalog panel's, with its plan, as it has been (ADR 0079).
  const update = service.update?.source === UpdateSource.CATALOG ? undefined : service.update;
  return (
    <div className="app-overview">
      <div className="app-main">
        <WaitingNotice />
        <AppNotice />
        <RunningPanel />
        {/* What's newer takes room in the main column when there's something to take, and an
            update under way, or just landed, takes its place (ADR 0079, ADR 0080). */}
        <UpgradeProgress
          service={service}
          fallback={update && offered(update) ? <UpdateCard service={service} /> : null}
        />
        <InstalledPanel service={service} />
        <RecentHealth />
        <RecentDeployments />
      </div>
      <div className="app-aside">
        <ConfigurationPanel />
        {update && !offered(update) ? <UpdateCard service={service} /> : null}
        <HealthCheckPanel />
        <VolumesPanel app={appOf(service)} />
        <AppBackups service={service} app={appOf(service)} />
        <RunsWithPanel />
      </div>
    </div>
  );
}

/**
 * A change to this app, or to one of its routes, that a token proposed and only
 * a person can apply, such as one the CLI stopped at for a person: whoever
 * didn't run it learns of it here, and on Activity.
 */
function WaitingNotice() {
  const { service } = useApp();
  const sheet = useChangeSheet();
  const name = nameOf(service);
  const scope = service.resource?.scope ?? "";
  const routes = new Set(
    useAppDomains(service).domains.flatMap((domain) =>
      domain.routes.filter((route) => route.service === name).map((route) => route.name),
    ),
  );
  const waiting = useWaitingFor(
    (ref) =>
      ref.scope === scope &&
      ((ref.kind === "service" && ref.name === name) || (ref.kind === "route" && routes.has(ref.name))),
  );
  const first = waiting[0];
  if (!first) return null;
  return (
    <Callout
      tone="warning"
      title={plural(waiting.length, {
        one: "# change to this app needs your approval",
        other: "# changes to this app need your approval",
      })}
      actions={
        <Button variant="primary" icon="commit" onClick={() => sheet.open({ kind: "change", id: first.id })}>
          <Trans>Review plan</Trans>
        </Button>
      }
    >
      {waiting.length === 1 ? (
        <Trans>
          {first.summary}. It was proposed with an API token, and its plan is one only a signed-in person may apply.
          Nothing changes until someone does.
        </Trans>
      ) : (
        <Trans>
          The newest is {first.summary}. They were proposed with API tokens, and their plans are ones only a signed-in
          person may apply. Nothing changes until someone does; Activity lists them all.
        </Trans>
      )}
    </Callout>
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
      const which = blind ?? gone ?? unknown[0];
      const server = which?.serverName ?? "";
      // What puts it right is on the server's page, with the command to run there when only a shell can.
      return (
        <Callout
          tone="warning"
          title={t`Nobody can say how ${name} runs`}
          actions={which ? <ServerLink id={which.serverId} name={server} /> : undefined}
        >
          {blind ? (
            <Trans>Docker isn't answering the agent on {server}. It may well be running.</Trans>
          ) : gone ? (
            <Trans>The agent on {server} isn't connected. It may well be running.</Trans>
          ) : (
            <Trans>The agent on {server} hasn't said how it runs lately. It may well be running.</Trans>
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
      // An update says how it goes as its phases, in the place of what's newer, where any other deploy has
      // its steps in the plan (ADR 0080).
      return (
        <UpgradeProgress
          service={service}
          elsewhere
          fallback={
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
          }
        />
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
            <>
              <ServerLinks names={state.servers} />
              {service.rolledBackChangeId !== "" ? (
                <Button onClick={() => openChange(service.rolledBackChangeId)}>
                  <Trans>See why</Trans>
                </Button>
              ) : (
                <Button onClick={() => openChange(service.changeId)}>
                  <Trans>Open the plan</Trans>
                </Button>
              )}
            </>
          }
        >
          {service.rolledBackChangeId !== "" ? (
            <Trans>
              Once it reconnects, Dockwell puts <code>{image}</code> back, after a deploy that failed.
            </Trans>
          ) : (
            <Trans>The deploy carries on once its agent reconnects.</Trans>
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
          actions={<ServerLinks names={state.servers} />}
        >
          <Trans>Dockwell can't see or change {name} there until its agent reconnects. It may well be running.</Trans>
        </Callout>
      );
    case "unreadable":
      return (
        <Callout tone="critical" title={t`Dockwell can't read what ${name} runs`}>
          <Trans>
            What the control plane stores for it was changed, or sealed with keys this install hasn't got, so Dockwell
            won't use it, and can't change {name}. Its containers keep running as they are. Restore the database and the
            key file from the same backup.
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
  // An update that just landed says so, with Undo, in the place of what's newer (ADR 0080), so here
  // it's what its health says.
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
  // An action on what you can see stays where it is, off, with why (ADR 0059).
  const mayAddPort = useMay()(ServiceService.method.addServicePort, { environment: environmentOf(service) });
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
              <PanelLink offer={mayAddPort} onClick={() => setAddingDomain(true)}>
                <Trans>Add a domain</Trans>
              </PanelLink>
              <PanelLink offer={mayAddPort} onClick={() => setAddingPort(true)}>
                <Trans>Add a port</Trans>
              </PanelLink>
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
  read,
  offer,
  onRemove,
  onMoveBehind,
}: {
  domain: Domain;
  route: DomainRoute;
  read: DomainRead;
  /** Whether its reader may plan here: removing it is off, with why, when not (ADR 0059). */
  offer: Offer;
  onRemove: () => void;
  onMoveBehind: () => void;
}) {
  const { t } = useLingui();
  const look = lookOf(domain, useControlPlaneNow(30_000));
  const silent = useSilent(domain, look);
  const where = routeWhere(domain, route);
  // Past how long a reading stands, its row stays, and says nothing of how it was.
  const said = read === "read";
  return (
    <li
      className={cx(
        "app-reach-row",
        "app-reach-domain",
        said && look.state === "failed" && "is-failed",
        said && look.warning && "is-warning",
      )}
    >
      <Icon name="globe" size={16} />
      <div className="app-reach-main">
        <code className="app-reach-at">{where}</code>
        <DomainFacts domain={domain} route={route} look={look} silent={silent} read={read} />
      </div>
      <div className="app-reach-acts">
        {said ? (
          <WatchAddress url={visitorUrl(domain, route)} where={where} serving={look.state === "serving"} />
        ) : null}
        {said && look.state === "serving" ? (
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
        <Button
          variant="ghost"
          size="sm"
          icon="trash"
          aria-label={t`Remove ${where}`}
          onClick={onRemove}
          {...offer.off}
        />
      </div>
      {said ? (
        <p className="app-domain-note">
          <DomainNote domain={domain} look={look} silent={silent} />
          {mayBeBehind(look) ? (
            <>
              {" "}
              <button type="button" className="app-panel-link" onClick={onMoveBehind}>
                <Trans>It's behind a tunnel or a proxy</Trans>
              </button>
            </>
          ) : null}
        </p>
      ) : null}
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
  const may = useMay();
  const mayAdd = may(ServiceService.method.addServicePort, { environment: environmentOf(service) });
  const mayPlan = may(ChangeService.method.proposeChange, { environment: environmentOf(service) });
  const { domains, read, isPending, failed, retrying, retry } = useAppDomains(service);
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
            <Button icon="plus" onClick={onAddDomain} {...mayAdd.off}>
              <Trans>Add a domain</Trans>
            </Button>
            <Button icon="plus" onClick={onAddPort} {...mayAdd.off}>
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
      return { key: `${at}/${udp ? "udp" : "tcp"}`, at, host, local, target: port.containerPort, udp };
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
            read={read}
            offer={mayPlan}
            onRemove={() => remove(routeWhere(domain, route), route.name)}
            onMoveBehind={() => onMoveBehind(moveBehindOf(domain, route))}
          />
        ))}
        {rows.map((row) => {
          const { target, at } = row;
          return (
            <li key={row.key} className="app-reach-row">
              <Icon name="globe" size={16} />
              <span className="app-reach-where">
                <code className="app-reach-at">{row.at}</code>
                {row.local ? null : <PrivateTag address={row.host} />}
              </span>
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
  // A timeline's undo has no off state, so it isn't drawn for someone who may not roll back (ADR 0059).
  const mayRollBack = useMay()(ServiceService.method.rollBackService, { environment: environmentOf(service) }).allowed;
  const back = !mayRollBack
    ? undefined
    : (revisions ?? [])
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
      title: revisionTitle(rev, revisions ?? []).node,
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
        <ReadFailed
          error={error}
          title={t`Can't load the deployments`}
          actions={
            <Button onClick={() => void refetch()} loading={isFetching}>
              <Trans>Try again</Trans>
            </Button>
          }
        />
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

/**
 * What checks the app and what its check last said, in a panel of its own as
 * App-Overview has it, with how many of its servers pass. Healthy only ever
 * means a check passed, so with no check there's no badge.
 */
function HealthCheckPanel() {
  const { t } = useLingui();
  const { service, health } = useApp();
  const words = useHealthWords();
  const time = useTimeWords();
  const now = useControlPlaneNow(15_000);
  const app = appOf(service);
  const check = app?.healthcheck;
  const test = check?.test ?? [];
  const read = health.read === "read";
  // Whether anything checks it: what its spec sets, or what the servers say the image has.
  const hasCheck =
    test[0] !== "NONE" &&
    (check?.http !== undefined ||
      check?.tcp !== undefined ||
      test.length > 0 ||
      health.check === HealthCheckKind.COMMAND ||
      health.check === HealthCheckKind.HTTP ||
      health.check === HealthCheckKind.TCP);
  // Every server it's placed on counts, whether or not it has answered.
  const placed = [
    ...new Set([...service.deployments.map((d) => d.serverId), ...health.servers.map((s) => s.serverId)]),
  ];
  const byId = new Map(health.servers.map((s) => [s.serverId, s] as const));
  let passing = 0;
  let failing = 0;
  let unread = 0;
  for (const id of placed) {
    const server = byId.get(id);
    // What can't be heard from passes nothing, however it last read.
    if (!read || !server || server.stale || server.kind === "unknown") unread += 1;
    else if (server.kind === "healthy") passing += 1;
    else if (server.kind === "unhealthy") failing += 1;
  }
  const total = placed.length;
  const several = total > 1;
  // The badge goes by how each server reads, as the head does, never by a single
  // check's result: a check can fail below its retries and the app still read healthy.
  let badge: ReactNode = null;
  if (hasCheck && unread > 0) {
    badge = (
      <StatusBadge status="unknown" label={several ? t`${passing} of ${total} passing` : t`Unknown`} labelIsState />
    );
  } else if (hasCheck && failing > 0) {
    badge = (
      <StatusBadge
        status="degraded"
        label={several ? t`${failing} of ${total} failing` : t({ message: "Failing", context: "health check" })}
        labelIsState
      />
    );
  } else if (hasCheck && passing > 0) {
    badge = (
      <StatusBadge
        status={passing === total ? "healthy" : health.status}
        label={several ? t`${passing} of ${total} passing` : t`Passing`}
        labelIsState
      />
    );
  }
  // What to say it said: the newest failing server's, or else the newest of any,
  // and during a rollout what the latest revision's container said.
  const heard = read ? health.servers : [];
  const at = (s: RunsOnServer) => (s.newest ?? s).last?.at?.getTime() ?? 0;
  const failingFirst = heard.filter((s) => s.kind === "unhealthy").sort((a, b) => at(b) - at(a));
  const chosen = failingFirst[0] ?? [...heard].sort((a, b) => at(b) - at(a))[0];
  const about = chosen?.newest ? { ...chosen, kind: chosen.newest.kind, last: chosen.newest.last } : chosen;
  const waiting =
    hasCheck &&
    heard.length > 0 &&
    heard.every((s) => !(s.newest ?? s).last && (s.kind === "healthy" || s.kind === "starting"));
  const said = !about || !hasCheck ? "" : waiting ? t`Waiting for its first result.` : words.says(about, now);
  // Where a check is set: Settings, at its section.
  const setCheck = (label: string) => (
    <Link to="/apps/$serviceId/settings" params={{ serviceId: service.id }} hash="health">
      {label}
    </Link>
  );
  return (
    <Panel title={t`Health check`} actions={badge}>
      <dl className="app-facts">
        <dt>{t`Checks`}</dt>
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
            <span>
              <span className="app-muted">{t`Turned off.`}</span> {setCheck(t`Turn it on`)}
            </span>
          ) : test.length > 0 ? (
            <code>{test.filter((part) => part !== "CMD" && part !== "CMD-SHELL").join(" ")}</code>
          ) : health.check === HealthCheckKind.COMMAND ? (
            <span>
              <Trans>The image's own</Trans>
            </span>
          ) : health.check === HealthCheckKind.NONE ? (
            <span>
              <span className="app-muted">{t`None.`}</span> {setCheck(t`Add one`)}
            </span>
          ) : (
            <span>
              <span className="app-muted">{t`The image's own, if it has one.`}</span> {setCheck(t`Set one`)}
            </span>
          )}
        </dd>
        {said ? (
          <>
            <dt>{t`It said`}</dt>
            <dd className="app-check-said">
              <span>{said}</span>
              {about?.last?.at && !waiting ? (
                <span className="app-muted">
                  {several && about ? `${about.serverName} · ` : ""}
                  {time.ago(about.last.at, now)}
                </span>
              ) : null}
            </dd>
          </>
        ) : null}
      </dl>
    </Panel>
  );
}

/**
 * What an app from a folder runs on (ADR 0058): its language's version and build, with
 * a newer build to update to, the folder, and how it installs, starts and listens.
 */
function RuntimeFacts() {
  const { t } = useLingui();
  const { service } = useApp();
  const runtimes = useRuntimes().data;
  const [changing, setChanging] = useState(false);
  const runtime = runtimeOf(service);
  if (!runtime) return null;
  const offered = knownVersion(runtime.language, runtime.version, runtimes);
  const retired = offered?.retired === true;
  const newer = newerBuild(service, runtimes);
  const server = service.deployments[0]?.serverId ?? "";
  const release = newer ? "" : (offered?.release ?? "");
  return (
    <>
      <dt>{t`Runtime`}</dt>
      <dd className="app-runtime">
        <span>
          {runtimeWords(runtime, runtimes)}
          {release ? <span className="app-muted"> · {release}</span> : null}
        </span>
        {retired ? (
          <span className="app-runtime-older">
            <span className="app-muted">
              <Trans>Dockwell no longer offers this version, so no new app runs on it</Trans>
            </span>
            <Button size="sm" onClick={() => setChanging(true)}>
              <Trans>Change version</Trans>
            </Button>
          </span>
        ) : null}
        {newer ? (
          <span className="app-runtime-older">
            <span className="app-muted">
              <Trans>An older build of {runtime.version}</Trans>
            </span>
            <Button size="sm" onClick={() => setChanging(true)}>
              <Trans>Update to {newer.release}</Trans>
            </Button>
          </span>
        ) : null}
        {changing ? <RuntimeSheet service={service} onClose={() => setChanging(false)} /> : null}
      </dd>
      <dt>{t`Folder`}</dt>
      <dd className="app-runtime-folder">
        <code>{runtime.folder}</code>
        {server ? (
          <Link to="/servers/$serverId/files" params={{ serverId: server }} search={{ path: runtime.folder }}>
            <Trans>Open in Files</Trans>
          </Link>
        ) : null}
      </dd>
      <dt>{t`Installs with`}</dt>
      <dd>
        {runtime.install.trim() === "-" ? (
          <span className="app-muted">{t`Nothing`}</span>
        ) : (
          <code>{runtime.install || offered?.install}</code>
        )}
      </dd>
      <dt>{t`Starts with`}</dt>
      <dd>
        <code>{runtime.start || offered?.start}</code>
      </dd>
      <dt>{t`Port`}</dt>
      <dd>
        <code>{runtime.port}</code>
      </dd>
      <dd className="app-runtime-note">
        <Trans>Each start installs again, so what you change in its folder runs once you restart it.</Trans>
      </dd>
    </>
  );
}

/** How it is set up, as far as that isn't the image's own. */
function ConfigurationPanel() {
  const { t } = useLingui();
  const { service } = useApp();
  const words = useAppWords();
  const [exporting, setExporting] = useState(false);
  const app = appOf(service);
  const runtime = runtimeOf(service);
  // What its runtime gives it is said with the runtime, not counted as the app's.
  const values = valuesOf(service, new Set(runtimeEnv(service).keys())).filter((v) => v.kind !== "runtime");
  const secrets = values.filter((v) => v.kind === "secret").length;
  const plain = values.length - secrets;
  const networks = app?.networks ?? [];
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
        {runtime ? (
          <RuntimeFacts />
        ) : (
          <>
            <dt>{t`Command`}</dt>
            <dd>
              {app?.command.length ? (
                <code>{app.command.join(" ")}</code>
              ) : (
                <span className="app-muted">{t`The image's own`}</span>
              )}
            </dd>
          </>
        )}
        {!runtime && app?.entrypoint.length ? (
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
        {!runtime && app?.workingDir ? (
          <>
            <dt>{t`Directory`}</dt>
            <dd>
              <code>{app.workingDir}</code>
            </dd>
          </>
        ) : null}
        <dt>{t`Restarts`}</dt>
        <dd>{words.restart(app?.restart ?? 0)}</dd>
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

/** A panel's link that acts, which stays where it is, off, with why, for someone who may not (ADR 0059). */
function PanelLink({ offer, onClick, children }: { offer: Offer; onClick: () => void; children: ReactNode }) {
  const off = useOff(offer.reason || undefined);
  return (
    <button type="button" className="app-panel-link" onClick={onClick} disabled={offer.off.disabled} {...off.props}>
      <span>{children}</span>
      {off.why}
    </button>
  );
}

/**
 * A server a state is about, as a link to its page, which says what puts it
 * right and, where only a shell can, the command to run there (#451).
 */
function ServerLink({ id, name }: { id: string; name: string }) {
  const { t } = useLingui();
  return (
    <RouterButtonLink to="/servers/$serverId" params={{ serverId: id }} icon="server">
      {t`Open ${name}`}
    </RouterButtonLink>
  );
}

/** The servers a state names, by name, as links: two at most, as the rest are a click from the first. */
function ServerLinks({ names }: { names: readonly string[] }) {
  const { service } = useApp();
  const found = names.flatMap((name) => {
    const id = service.deployments.find((d) => d.serverName === name)?.serverId;
    return id ? [{ id, name }] : [];
  });
  return (
    <>
      {found.slice(0, 2).map(({ id, name }) => (
        <ServerLink key={id} id={id} name={name} />
      ))}
    </>
  );
}
