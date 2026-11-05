// SPDX-License-Identifier: AGPL-3.0-only

import { timestampDate } from "@bufbuild/protobuf/wkt";
import { createClient } from "@connectrpc/connect";
import { useQuery, useTransport } from "@connectrpc/connect-query";
import { ChangeService, LogService } from "@dockwell/api";
import { Button, Callout, Icon, PHONE_QUERY, StatusBadge, TabNav, useMediaQuery } from "@dockwell/ui";
import { Trans, useLingui } from "@lingui/react/macro";
import { Link, useMatch } from "@tanstack/react-router";
import { type ReactNode, type RefObject, useMemo, useRef, useState } from "react";

import { sentence } from "../../lib/errors";
import { RouterButtonLink, RouterTabLink } from "../../lib/links";
import { useTimeWords } from "../../lib/time";
import { useChangeSheet } from "../changes/ChangeSheet";
import { LogsSearchButton } from "../logs/phoneSearch";
import { Reconnecting } from "../servers/FleetNotes";
import { useAgoNow, useFleetView } from "../servers/useFleet";
import { EmptyState } from "../shell/EmptyState";
import { Page } from "../shell/Page";
import { AppContext, useApp, useRevisions } from "./AppContext";
import {
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
import { DeploySheet } from "./DeploySheet";
import { HealthBadge, useHealthTitle, useHealthWords } from "./Health";
import { useHealth } from "./health";
import { WholeImage } from "./ImageName";
import { useWhoWords } from "./revision";
import { usePlaces, useServices, useServicesLive } from "./useServices";
import { useAppWords } from "./words";
import "./Apps.css";

/**
 * One app's frame, whichever tab is open: where it sits, its name and state,
 * what to do with it, and its tabs, each of them a route of its own under
 * /apps/<id>. The tabs read the app from here (useApp), so moving between
 * them reads nothing again. Live, as the screens under it are.
 */
export function AppLayout({ serviceId, children }: { serviceId: string; children: ReactNode }) {
  const { t } = useLingui();
  const query = useServices();
  const { reconnecting } = useServicesLive();
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
      <Page title={t`App`} back="/apps" breadcrumb={<Crumbs />}>
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
  const revision = revisionName(service.revision);
  const latest = useRevisions(serviceId).revisions?.[0];
  const ago = service.updateTime ? time.ago(timestampDate(service.updateTime), now) : "";
  const runsWord = healthWords.word(health);
  const recordWord = recordSaysMore(state) ? words.state(state) : "";
  const moving = isMoving(state);
  // Nothing restarts while it is moving, or where it has no server to do it on.
  const restartable = !moving && state.kind !== "unplaced";
  const restart = () =>
    sheet.open({
      kind: "plan",
      make: () => createClient(ChangeService, transport).restartService({ id: service.id }),
    });
  const place = places.place(environmentOf(service));
  return (
    <Page
      title={name}
      back="/apps"
      breadcrumb={<Crumbs name={name} project={place && !place.isDefault ? place.label : undefined} />}
      badge={
        <>
          <HealthBadge health={health} title={healthTitle} />
          <code className="app-revision">{revision}</code>
          {recordWord ? <StatusBadge status={state.status} label={recordWord} labelIsState /> : null}
        </>
      }
      lead={
        state.kind === "rollback-failed" ? (
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
        ) : moving ? (
          <Trans>
            Deploying <code>{image}</code>, started {ago}
          </Trans>
        ) : (
          <>
            <WholeImage image={image} />
            {" · "}
            {state.kind === "rolled-back" ? (
              <Trans>rolled back by Dockwell {ago}</Trans>
            ) : (
              who.deployed(latest?.change, ago)
            )}
          </>
        )
      }
      actions={
        <>
          {restartable ? (
            <Button icon="refresh" onClick={restart}>
              <Trans>Restart</Trans>
            </Button>
          ) : null}
          <Button variant="primary" icon="deploy" onClick={() => setDeploying(true)}>
            <Trans>Deploy</Trans>
          </Button>
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
      phoneSub={[revision, runsWord, recordWord, phone && !onLogs ? shortImage(image) : ""].filter(Boolean).join(" · ")}
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
        </TabNav>
      )}
      <LogsSearchButton value={searchButton}>{children}</LogsSearchButton>
      {/* The logs tab has a bar of its own on a phone: Download and Copy. */}
      {phone && !onLogs ? (
        <div className="app-actionbar">
          {restartable ? (
            <Button icon="refresh" size="lg" onClick={restart}>
              <Trans>Restart</Trans>
            </Button>
          ) : null}
          <Button variant="primary" icon="deploy" size="lg" onClick={() => setDeploying(true)}>
            <Trans>Deploy</Trans>
          </Button>
        </div>
      ) : null}
      {deploying ? <DeploySheet service={service} onClose={() => setDeploying(false)} /> : null}
    </Page>
  );
}
