// SPDX-License-Identifier: AGPL-3.0-only

import { timestampDate } from "@bufbuild/protobuf/wkt";
import { createClient } from "@connectrpc/connect";
import { useQuery, useTransport } from "@connectrpc/connect-query";
import { BuildState, ChangeService, DesiredRunState, LogService, Protocol, ServiceService } from "@dockwell/api";
import {
  Button,
  ButtonLink,
  Callout,
  Icon,
  Loading,
  PHONE_QUERY,
  Skeleton,
  SkeletonCard,
  SkeletonFacts,
  StatusBadge,
  TabNav,
  Tag,
  useMediaQuery,
} from "@dockwell/ui";
import { Trans, useLingui } from "@lingui/react/macro";
import { Link, useMatch, useNavigate } from "@tanstack/react-router";
import { type ReactNode, type RefObject, useMemo, useRef, useState } from "react";
import { shownRef } from "../../lib/built";
import { sentence } from "../../lib/errors";
import { RouterButtonLink, RouterTabLink } from "../../lib/links";
import { useTimeWords } from "../../lib/time";
import { useChangeSheet } from "../changes/ChangeSheet";
import { LogsSearchButton } from "../logs/phoneSearch";
import { Reconnecting } from "../servers/FleetNotes";
import { useAgoNow, useFleetView } from "../servers/useFleet";
import { ActionMenu } from "../shell/ActionMenu";
import { usePageCommands } from "../shell/CommandMenu";
import { EmptyState } from "../shell/EmptyState";
import { Page } from "../shell/Page";
import { useRuntimes } from "../websites/website";
import { AppContext, useApp, useRevisions } from "./AppContext";
import {
  appOf,
  appState,
  deployedAt,
  environmentOf,
  imageOf,
  isMoving,
  recordSaysMore,
  revisionName,
  shortImage,
  stoppedByRevision,
} from "./app";
import { shortCommit, shownRepository } from "./Build";
import { DeploySheet } from "./DeploySheet";
import { AppDeployAction } from "./deployAction";
import { lookOf, useAppDomains, visitorUrl } from "./domains";
import { HealthBadge, useHealthTitle, useHealthWords } from "./Health";
import { useHealth } from "./health";
import { WholeImage } from "./ImageName";
import { useShownName } from "./names";
import { RuntimeSheet } from "./RuntimeSheet";
import { useWhoWords } from "./revision";
import { runtimeOf, runtimeWords } from "./runtime";
import { usePlaces, useServices } from "./useServices";
import { useAppWords } from "./words";
import { useWorkloadControls } from "./workload";
import "./Apps.css";
import { useMay } from "../auth/may";
import { useServicesWatch } from "./ServicesLive";

/**
 * One app's frame, whichever tab is open: where it sits, its name and state,
 * what to do with it, and its tabs, each of them a route of its own under
 * /apps/<id>. The tabs read the app from here (useApp), so moving between
 * them reads nothing again. Live, as the screens under it are.
 */
export function AppLayout({ serviceId, children }: { serviceId: string; children: ReactNode }) {
  const { t } = useLingui();
  const query = useServices();
  const { reconnecting } = useServicesWatch();
  const { servers } = useFleetView();
  const healthView = useHealth();
  const service = query.data?.find((s) => s.id === serviceId);
  const state = useMemo(() => (service ? appState(service, servers) : undefined), [service, servers]);
  const health = healthView.of(service ?? { id: serviceId });
  const value = useMemo(
    () => (service && state ? { service, state, health, services: query.data ?? [] } : undefined),
    [service, state, health, query.data],
  );

  // A service that is gone can still have logs its servers kept, as when its
  // first deploy failed and was rolled back, and Activity links to them.
  const logs = useMatch({ from: "/_shell/apps/$serviceId/logs", shouldThrow: false });
  const overview = useMatch({ from: "/_shell/apps/$serviceId/", shouldThrow: false });
  if (!value && logs && !query.isPending && !query.error) {
    return <GoneAppFrame serviceId={serviceId}>{children}</GoneAppFrame>;
  }
  if (!value) {
    return (
      <Page
        title={t`App`}
        pending={query.isPending}
        fullBody={logs !== undefined}
        back="/apps"
        breadcrumb={<Crumbs />}
        className={query.isPending ? "app-page" : undefined}
      >
        {query.isPending ? (
          <>
            <AppTabs serviceId={serviceId} />
            <Loading label={t`Loading the app`}>
              {overview ? (
                <div className="app-overview">
                  <div className="app-main">
                    <SkeletonCard lines={2} />
                    <SkeletonCard height={160} />
                  </div>
                  <div className="app-aside">
                    <SkeletonCard>
                      <SkeletonFacts count={4} layout="rows" />
                    </SkeletonCard>
                    <SkeletonCard lines={2} />
                  </div>
                </div>
              ) : logs ? (
                <Skeleton shape="block" height={420} />
              ) : (
                <SkeletonCard lines={4} />
              )}
            </Loading>
          </>
        ) : query.error ? (
          <Callout tone="critical" title={t`Can't load this app`}>
            {sentence(query.error)} <Trans>Nothing changed on the server. Reload the page to try again.</Trans>
          </Callout>
        ) : (
          <EmptyState
            icon="service"
            title={<Trans>No app with this ID</Trans>}
            action={
              <RouterButtonLink to="/apps" size="lg">
                <Trans>Go to the apps</Trans>
              </RouterButtonLink>
            }
          >
            <p>
              <Trans>
                <code>{serviceId}</code> may have been deleted, or the link may be wrong.
              </Trans>
            </p>
          </EmptyState>
        )}
      </Page>
    );
  }
  return (
    <AppContext value={value}>
      <AppFrame reconnecting={reconnecting}>{children}</AppFrame>
    </AppContext>
  );
}

/** Its project, as the breadcrumb links to it, once there are two (ADR 0074). */
interface CrumbProject {
  id: string;
  label: string;
}

function Crumbs({ name, project }: { name?: string; project?: CrumbProject | undefined }) {
  const { t } = useLingui();
  return (
    <nav className="app-crumbs" aria-label={t`Breadcrumb`}>
      <Link to="/apps">
        <Trans>Apps</Trans>
      </Link>
      {project ? (
        <>
          <span aria-hidden="true">/</span>
          <Link to="/projects/$projectId" params={{ projectId: project.id }}>
            {project.label}
          </Link>
        </>
      ) : null}
      {name ? (
        <>
          <span aria-hidden="true">/</span>
          <span aria-current="page">{name}</span>
        </>
      ) : null}
    </nav>
  );
}

/** The frame of an app that isn't there now, named as its logs name it, for reading what its servers kept. */
function GoneAppFrame({ serviceId, children }: { serviceId: string; children: ReactNode }) {
  const { t } = useLingui();
  const sources = useQuery(LogService.method.listLogSources, { id: serviceId }, { refetchOnWindowFocus: false });
  const name = sources.data?.service?.name;
  const searchButton = useRef<(() => void) | null>(null);
  return (
    <LogsSearchButton value={searchButton}>
      <Page
        title={name ?? t`Logs`}
        fullBody
        back="/apps"
        breadcrumb={<Crumbs {...(name ? { name } : {})} />}
        phoneAction={<LogsSearchAction opens={searchButton} />}
      >
        {children}
      </Page>
    </LogsSearchButton>
  );
}

/** The phone header's button on an app's logs, which opens their search. */
function LogsSearchAction({ opens }: { opens: RefObject<(() => void) | null> }) {
  const { t } = useLingui();
  return (
    <button
      type="button"
      className="mobile-header-button"
      aria-label={t`Search logs`}
      onClick={() => opens.current?.()}
    >
      <Icon name="search" size={20} />
    </button>
  );
}

function AppFrame({ reconnecting, children }: { reconnecting: boolean; children: ReactNode }) {
  const { t } = useLingui();
  const { service, state, health } = useApp();
  const words = useAppWords();
  const healthWords = useHealthWords();
  const healthTitle = useHealthTitle(health);
  const sheet = useChangeSheet();
  const transport = useTransport();
  const time = useTimeWords();
  const who = useWhoWords();
  const places = usePlaces();
  const phone = useMediaQuery(PHONE_QUERY);
  const onLogs = useMatch({ from: "/_shell/apps/$serviceId/logs", shouldThrow: false }) !== undefined;
  const now = useAgoNow(deployedAt(service));
  const [deploying, setDeploying] = useState(false);
  const searchButton = useRef<(() => void) | null>(null);
  const serviceId = service.id;
  // A catalog install's app goes by the name the person gave it, with the app's beside it (#796).
  const shown = useShownName()(service, "app");
  const name = shown.title;
  const image = imageOf(service);
  // An app from a folder says what it runs on, and changes its version where another deploys an image.
  const runtime = runtimeOf(service);
  const runtimes = useRuntimes().data;
  const runsOn = runtime ? runtimeWords(runtime, runtimes) : "";
  const revision = revisionName(service.revision);
  const revisions = useRevisions(serviceId).revisions;
  const latest = revisions?.[0];
  const ago = service.updateTime ? time.ago(timestampDate(service.updateTime), now) : "";
  const stopped = stoppedByRevision(service, state);
  const runsWord = stopped ? t`Stopped` : healthWords.word(health);
  // A build someone cancelled stops a deploy on purpose, and isn't news the way a rollback is (ADR 0046).
  const cancelled =
    state.kind === "rolled-back" &&
    revisions?.find((r) => r.change?.id === service.rolledBackChangeId)?.build?.state === BuildState.CANCELLED;
  const recordWord = cancelled ? t`Build cancelled` : recordSaysMore(state) ? words.state(state) : "";
  const moving = isMoving(state);
  // Nothing changes what can't be read, which the overview says.
  const changeable = state.kind !== "unreadable";
  // Nothing restarts while it is moving, or where it has no server to do it on.
  const isStopped = appOf(service)?.desiredRunState === DesiredRunState.STOPPED;
  const controllable = !moving && state.kind !== "unplaced" && changeable && !isStopped;
  const pausable = controllable && !!appOf(service) && !!service.resource;
  const isPaused = appOf(service)?.desiredRunState === DesiredRunState.PAUSED;
  const restartable = controllable && !isPaused;
  const startable = !moving && state.kind !== "unplaced" && changeable && isStopped;
  const { changeRun, changePause, restart: restartWorkload } = useWorkloadControls();
  const start = () => {
    if (service.resource) changeRun(service.resource, service.revision, true);
  };
  const pause = () => {
    if (service.resource) changePause(service.resource, service.revision, !isPaused);
  };
  const pauseLabel = isPaused ? t`Unpause workload` : t`Pause workload`;
  const stop = () => {
    if (service.resource) changeRun(service.resource, service.revision, false);
  };
  const build = service.spec?.build;
  const source = build?.source.case === "git" ? build.source.value : undefined;
  // An app that builds from Git deploys the newest commit of its branch, with nothing to choose (ADR 0046).
  const deploy = () =>
    source
      ? sheet.open({
          kind: "plan",
          make: () => createClient(ServiceService, transport).deployService({ id: service.id }),
        })
      : setDeploying(true);
  const restart = () => restartWorkload(service.id);
  const place = places.place(environmentOf(service));
  // An action on what you can see stays where it is, off, with why (ADR 0059).
  const may = useMay();
  const here = { environment: environmentOf(service) };
  const mayRestart = may(ChangeService.method.restartService, here);
  const mayStart = may(ChangeService.method.proposeChange, here);
  const mayDeploy = may(ServiceService.method.deployService, here);
  const mayChangeVersion = may(ServiceService.method.changeServiceRuntime, here);
  // One Open, in the header, to the app's domain once it serves and anyone may reach it (#797):
  // a held one opens from its callout, through the panel.
  const reached = useAppDomains(service).domains.flatMap((domain) =>
    lookOf(domain).state === "serving"
      ? domain.routes.filter((route) => !route.held && route.path === "").map((route) => visitorUrl(domain, route))
      : [],
  );
  const opensAt = stopped ? undefined : reached[0];
  // The command bar offers what the header does, first (#795): only what the reader may do.
  const navigate = useNavigate();
  const mayChange = runtime ? mayChangeVersion : mayDeploy;
  usePageCommands([
    ...(opensAt
      ? [
          {
            id: "open",
            icon: "external" as const,
            label: t`Open ${name} at its domain`,
            run: () => void window.open(opensAt, "_blank", "noopener"),
          },
        ]
      : []),
    ...(startable && mayStart.allowed
      ? [{ id: "start", icon: "play" as const, label: t`Start ${name}`, hint: t`Opens a plan`, run: start }]
      : []),
    ...(changeable && mayChange.allowed
      ? [
          {
            id: "deploy",
            icon: "deploy" as const,
            label: runtime ? t`Change the version of ${name}` : t`Deploy ${name}`,
            run: deploy,
          },
        ]
      : []),
    ...(controllable && service.resource && mayStart.allowed
      ? [{ id: "stop", icon: "stop" as const, label: t`Stop ${name}`, hint: t`Opens a plan`, run: stop }]
      : []),
    ...(pausable && mayStart.allowed
      ? [
          {
            id: "pause",
            icon: isPaused ? ("play" as const) : ("pause" as const),
            label: isPaused ? t`Unpause ${name}` : t`Pause ${name}`,
            hint: t`Opens a plan`,
            run: pause,
          },
        ]
      : []),
    ...(restartable && mayRestart.allowed
      ? [{ id: "restart", icon: "refresh" as const, label: t`Restart ${name}`, hint: t`Opens a plan`, run: restart }]
      : []),
    {
      id: "logs",
      icon: "terminal",
      label: t`Show the logs of ${name}`,
      run: () => void navigate({ to: "/apps/$serviceId/logs", params: { serviceId } }),
    },
    {
      id: "environment",
      icon: "key",
      label: t`Open the environment of ${name}`,
      run: () => void navigate({ to: "/apps/$serviceId/environment", params: { serviceId } }),
    },
  ]);
  // Where it answers, on every tab, as soon as it publishes a port anyone outside the server reaches.
  const { servers: fleet } = useFleetView();
  const published = appOf(service)?.ports.find(
    (p) => p.protocol !== Protocol.UDP && p.hostIp !== "127.0.0.1" && p.hostIp !== "::1",
  );
  const address = fleet?.find((s) => s.id === service.deployments[0]?.serverId)?.address;
  const at =
    published && address ? `${address.includes(":") ? `[${address}]` : address}:${published.hostPort}` : undefined;
  const where = at ? (
    <>
      {" · "}
      {/* Named by where it answers: What runs has the link named Open, and two links of one name read as one. */}
      <a
        className="app-head-at"
        href={`http://${at}`}
        target="_blank"
        rel="noreferrer"
        aria-label={t`Answers at ${at}`}
      >
        <code>{at}</code>
        <Icon name="external" size={12} />
      </a>
    </>
  ) : null;
  return (
    <Page
      title={name}
      // A log view gains from every column it can have, under a head and tabs that stay where every tab has them.
      fullBody={onLogs}
      back="/apps"
      breadcrumb={
        // An install's app goes by its project's name, so the project's crumb would only say it twice (#796).
        <Crumbs
          name={name}
          project={
            place && !place.isDefault && place.label !== name ? { id: place.scope, label: place.label } : undefined
          }
        />
      }
      badge={
        <>
          {stopped ? (
            <StatusBadge status="stopped" label={t`Stopped`} labelIsState />
          ) : (
            <HealthBadge health={health} title={healthTitle} />
          )}
          {shown.app ? <Tag className="app-catalog-name">{shown.app}</Tag> : null}
          {source ? (
            <Tag icon="branch" className="app-branch">
              {shownRef(source.ref) || t`default branch`}
            </Tag>
          ) : null}
          <code className="app-revision">{revision}</code>
          {recordWord ? (
            <StatusBadge status={cancelled ? "stopped" : state.status} label={recordWord} labelIsState />
          ) : null}
        </>
      }
      lead={
        state.kind === "unreadable" ? (
          <Trans>Dockwell can't read what this runs</Trans>
        ) : state.kind === "rollback-failed" ? (
          <Trans>
            Putting <code>{image}</code> back failed, {ago}
          </Trans>
        ) : moving && service.rolledBackChangeId !== "" ? (
          state.kind === "waiting" ? (
            <Trans>
              Dockwell puts <code>{image}</code> back once {state.servers.join(", ")} reconnects
            </Trans>
          ) : (
            <Trans>
              Dockwell is putting <code>{image}</code> back, started {ago}
            </Trans>
          )
        ) : moving && source ? (
          <Trans>
            Building and deploying <code>{shownRepository(source.url)}</code>, started {ago}
          </Trans>
        ) : moving ? (
          <>
            <Trans>
              Deploying <code>{image}</code>, started {ago}
            </Trans>
            {where}
          </>
        ) : (
          <>
            {source ? (
              <span className="app-built-from">
                <Trans>
                  <code>{shownRepository(source.url)}</code> at <code>{shortCommit(source.commit)}</code>
                </Trans>
              </span>
            ) : runtime ? (
              <Trans>
                {runsOn} from <code>{runtime.folder}</code>
              </Trans>
            ) : (
              <WholeImage image={image} />
            )}
            {" · "}
            {cancelled ? (
              <Trans>build cancelled {ago}</Trans>
            ) : state.kind === "rolled-back" ? (
              <Trans>rolled back by Dockwell {ago}</Trans>
            ) : (
              who.deployed(latest?.change, ago)
            )}
            {where}
          </>
        )
      }
      actions={
        <>
          {opensAt ? (
            <ButtonLink href={opensAt} target="_blank" rel="noreferrer" icon="external">
              <Trans context="open an app at its domain">Open</Trans>
            </ButtonLink>
          ) : null}
          {controllable && service.resource ? (
            <Button icon="stop" onClick={stop} {...mayStart.off}>
              <Trans>Stop</Trans>
            </Button>
          ) : null}
          {pausable ? (
            <Button icon={isPaused ? "play" : "pause"} onClick={pause} {...mayStart.off}>
              {pauseLabel}
            </Button>
          ) : null}
          {restartable ? (
            <Button icon="refresh" onClick={restart} {...mayRestart.off}>
              <Trans>Restart</Trans>
            </Button>
          ) : null}
          {/*
            As App-Overview has it, between Restart and Deploy. It stays on the Logs tab too, as App-Logs has it,
            so the buttons don't move from tab to tab; there the router marks it as the current page.
          */}
          <RouterButtonLink to="/apps/$serviceId/logs" params={{ serviceId }} icon="terminal">
            <Trans>Logs</Trans>
          </RouterButtonLink>
          {changeable ? (
            <Button
              variant={startable ? "secondary" : "primary"}
              icon="deploy"
              onClick={deploy}
              {...(runtime ? mayChangeVersion : mayDeploy).off}
            >
              {runtime ? <Trans>Change version</Trans> : <Trans>Deploy</Trans>}
            </Button>
          ) : null}
          {startable ? (
            <Button variant="primary" icon="play" onClick={start} {...mayStart.off}>
              <Trans>Start</Trans>
            </Button>
          ) : null}
        </>
      }
      phoneAction={
        pausable ? (
          <ActionMenu
            label={t`Actions for ${name}`}
            actions={[
              {
                id: "pause",
                icon: isPaused ? "play" : "pause",
                label: pauseLabel,
                ...mayStart.off,
                onSelect: pause,
              },
            ]}
          />
        ) : undefined
      }
      {...(phone && onLogs
        ? {
            phoneTitle: t`${name} logs`,
            backLink: (
              <Link to="/apps/$serviceId" params={{ serviceId }} className="mobile-header-button" aria-label={t`Back`}>
                <Icon name="arrow-left" size={20} />
              </Link>
            ),
            phoneAction: <LogsSearchAction opens={searchButton} />,
          }
        : {})}
      phoneSub={[revision, runsWord, recordWord, phone && !onLogs ? runsOn || shortImage(image) : ""]
        .filter(Boolean)
        .join(" · ")}
      status={reconnecting ? <Reconnecting /> : undefined}
      className="app-page"
    >
      {/* An app's logs have all of a phone's screen, and the header goes back out of them. */}
      {phone && onLogs ? null : <AppTabs serviceId={serviceId} deployments={Number(service.revision)} />}
      <LogsSearchButton value={searchButton}>
        {/* A tab's empty state offers Deploy as the header does, where the reader may, and not while it moves. */}
        <AppDeployAction.Provider value={changeable && !runtime && mayDeploy.allowed ? deploy : undefined}>
          {children}
        </AppDeployAction.Provider>
      </LogsSearchButton>
      {/* The logs tab has a bar of its own on a phone: Download and Copy. */}
      {phone && !onLogs && changeable ? (
        <div className="app-actionbar">
          <RouterButtonLink to="/apps/$serviceId/logs" params={{ serviceId }} icon="terminal" size="lg">
            <Trans>Logs</Trans>
          </RouterButtonLink>
          {controllable && service.resource ? (
            <Button icon="stop" size="lg" onClick={stop} {...mayStart.off}>
              <Trans>Stop</Trans>
            </Button>
          ) : null}
          {restartable ? (
            <Button icon="refresh" size="lg" onClick={restart} {...mayRestart.off}>
              <Trans>Restart</Trans>
            </Button>
          ) : null}
          {startable ? (
            <Button variant="primary" icon="play" size="lg" onClick={start} {...mayStart.off}>
              <Trans>Start</Trans>
            </Button>
          ) : (
            <Button
              variant="primary"
              icon="deploy"
              size="lg"
              onClick={deploy}
              {...(runtime ? mayChangeVersion : mayDeploy).off}
            >
              {runtime ? <Trans>Change version</Trans> : <Trans>Deploy</Trans>}
            </Button>
          )}
        </div>
      ) : null}
      {deploying && runtime ? <RuntimeSheet service={service} onClose={() => setDeploying(false)} /> : null}
      {deploying && !runtime ? <DeploySheet service={service} onClose={() => setDeploying(false)} /> : null}
    </Page>
  );
}

/** An app's tabs, which draw at once, before the app they open has loaded. */
function AppTabs({ serviceId, deployments }: { serviceId: string; deployments?: number }) {
  const { t } = useLingui();
  return (
    <TabNav label={t`App sections`}>
      <RouterTabLink to="/apps/$serviceId" params={{ serviceId }} activeOptions={{ exact: true }}>
        <Trans>Overview</Trans>
      </RouterTabLink>
      <RouterTabLink to="/apps/$serviceId/deployments" params={{ serviceId }} count={deployments}>
        <Trans>Deployments</Trans>
      </RouterTabLink>
      <RouterTabLink to="/apps/$serviceId/logs" params={{ serviceId }}>
        <Trans>Logs</Trans>
      </RouterTabLink>
      <RouterTabLink to="/apps/$serviceId/environment" params={{ serviceId }}>
        <Trans>Environment</Trans>
      </RouterTabLink>
      <RouterTabLink to="/apps/$serviceId/settings" params={{ serviceId }}>
        <Trans>Settings</Trans>
      </RouterTabLink>
    </TabNav>
  );
}
