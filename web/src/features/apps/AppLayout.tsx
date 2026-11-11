// SPDX-License-Identifier: AGPL-3.0-only

import { timestampDate } from "@bufbuild/protobuf/wkt";
import { createClient } from "@connectrpc/connect";
import { useQuery, useTransport } from "@connectrpc/connect-query";
import { BuildState, ChangeService, LogService, Protocol, ServiceService } from "@dockwell/api";
import { Button, Callout, Icon, PHONE_QUERY, StatusBadge, TabNav, Tag, useMediaQuery } from "@dockwell/ui";
import { Trans, useLingui } from "@lingui/react/macro";
import { Link, useMatch } from "@tanstack/react-router";
import { type ReactNode, type RefObject, useMemo, useRef, useState } from "react";
import { shownRef } from "../../lib/built";
import { sentence } from "../../lib/errors";
import { RouterButtonLink, RouterTabLink } from "../../lib/links";
import { useTimeWords } from "../../lib/time";
import { useChangeSheet } from "../changes/ChangeSheet";
import { LogsSearchButton } from "../logs/phoneSearch";
import { Reconnecting } from "../servers/FleetNotes";
import { useAgoNow, useFleetView } from "../servers/useFleet";
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
  nameOf,
  recordSaysMore,
  revisionName,
  shortImage,
} from "./app";
import { shortCommit, shownRepository } from "./Build";
import { DeploySheet } from "./DeploySheet";
import { HealthBadge, useHealthTitle, useHealthWords } from "./Health";
import { useHealth } from "./health";
import { WholeImage } from "./ImageName";
import { RuntimeSheet } from "./RuntimeSheet";
import { useWhoWords } from "./revision";
import { runtimeOf, runtimeWords } from "./runtime";
import { usePlaces, useServices } from "./useServices";
import { useAppWords } from "./words";
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
  if (!value && logs && !query.isPending && !query.error) {
    return <GoneAppFrame serviceId={serviceId}>{children}</GoneAppFrame>;
  }
  if (!value) {
    return (
      <Page title={t`App`} fullBody={logs !== undefined} back="/apps" breadcrumb={<Crumbs />}>
        {query.isPending ? (
          <div className="app-loading" aria-busy="true">
            <span className="dw-sr">
              <Trans>Loading the app</Trans>
            </span>
            <div className="app-skeleton is-tabs" />
            <div className="app-skeleton is-panel" />
          </div>
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

function Crumbs({ name, project }: { name?: string; project?: string | undefined }) {
  const { t } = useLingui();
  return (
    <nav className="app-crumbs" aria-label={t`Breadcrumb`}>
      <Link to="/apps">
        <Trans>Apps</Trans>
      </Link>
      {project ? (
        <>
          <span aria-hidden="true">/</span>
          <span>{project}</span>
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
  const name = nameOf(service);
  const image = imageOf(service);
  // An app from a folder says what it runs on, and changes its version where another deploys an image.
  const runtime = runtimeOf(service);
  const runtimes = useRuntimes().data;
  const runsOn = runtime ? runtimeWords(runtime, runtimes) : "";
  const revision = revisionName(service.revision);
  const revisions = useRevisions(serviceId).revisions;
  const latest = revisions?.[0];
  const ago = service.updateTime ? time.ago(timestampDate(service.updateTime), now) : "";
  const runsWord = healthWords.word(health);
  // A build someone cancelled stops a deploy on purpose, and isn't news the way a rollback is (ADR 0046).
  const cancelled =
    state.kind === "rolled-back" &&
    revisions?.find((r) => r.change?.id === service.rolledBackChangeId)?.build?.state === BuildState.CANCELLED;
  const recordWord = cancelled ? t`Build cancelled` : recordSaysMore(state) ? words.state(state) : "";
  const moving = isMoving(state);
  // Nothing changes what can't be read, which the overview says.
  const changeable = state.kind !== "unreadable";
  // Nothing restarts while it is moving, or where it has no server to do it on.
  const restartable = !moving && state.kind !== "unplaced" && changeable;
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
  const restart = () =>
    sheet.open({
      kind: "plan",
      make: () => createClient(ChangeService, transport).restartService({ id: service.id }),
    });
  const place = places.place(environmentOf(service));
  // An action on what you can see stays where it is, off, with why (ADR 0059).
  const may = useMay();
  const here = { environment: environmentOf(service) };
  const mayRestart = may(ChangeService.method.restartService, here);
  const mayDeploy = may(ServiceService.method.deployService, here);
  const mayChangeVersion = may(ServiceService.method.changeServiceRuntime, here);
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
      breadcrumb={<Crumbs name={name} project={place && !place.isDefault ? place.label : undefined} />}
      badge={
        <>
          <HealthBadge health={health} title={healthTitle} />
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
            <Button variant="primary" icon="deploy" onClick={deploy} {...(runtime ? mayChangeVersion : mayDeploy).off}>
              {runtime ? <Trans>Change version</Trans> : <Trans>Deploy</Trans>}
            </Button>
          ) : null}
        </>
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
      {phone && onLogs ? null : (
        <TabNav label={t`App sections`}>
          <RouterTabLink to="/apps/$serviceId" params={{ serviceId }} activeOptions={{ exact: true }}>
            <Trans>Overview</Trans>
          </RouterTabLink>
          <RouterTabLink to="/apps/$serviceId/deployments" params={{ serviceId }} count={Number(service.revision)}>
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
      )}
      <LogsSearchButton value={searchButton}>{children}</LogsSearchButton>
      {/* The logs tab has a bar of its own on a phone: Download and Copy. */}
      {phone && !onLogs && changeable ? (
        <div className="app-actionbar">
          <RouterButtonLink to="/apps/$serviceId/logs" params={{ serviceId }} icon="terminal" size="lg">
            <Trans>Logs</Trans>
          </RouterButtonLink>
          {restartable ? (
            <Button icon="refresh" size="lg" onClick={restart} {...mayRestart.off}>
              <Trans>Restart</Trans>
            </Button>
          ) : null}
          <Button
            variant="primary"
            icon="deploy"
            size="lg"
            onClick={deploy}
            {...(runtime ? mayChangeVersion : mayDeploy).off}
          >
            {runtime ? <Trans>Change version</Trans> : <Trans>Deploy</Trans>}
          </Button>
        </div>
      ) : null}
      {deploying && runtime ? <RuntimeSheet service={service} onClose={() => setDeploying(false)} /> : null}
      {deploying && !runtime ? <DeploySheet service={service} onClose={() => setDeploying(false)} /> : null}
    </Page>
  );
}
