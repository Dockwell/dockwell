// SPDX-License-Identifier: AGPL-3.0-only

import { timestampDate } from "@bufbuild/protobuf/wkt";
import { Button, Callout, PHONE_QUERY, StatusBadge, TabNav, useMediaQuery } from "@dockwell/ui";
import { Trans, useLingui } from "@lingui/react/macro";
import { Link } from "@tanstack/react-router";
import { type ReactNode, useMemo, useState } from "react";

import { sentence } from "../../lib/errors";
import { RouterButtonLink, RouterTabLink } from "../../lib/links";
import { useTimeWords } from "../../lib/time";
import { Reconnecting } from "../servers/FleetNotes";
import { useControlPlaneNow, useFleetView } from "../servers/useFleet";
import { EmptyState } from "../shell/EmptyState";
import { Page } from "../shell/Page";
import { AppContext, useApp, useRevisions } from "./AppContext";
import { appState, imageOf, nameOf, revisionName } from "./app";
import { DeploySheet } from "./DeploySheet";
import { useServices, useServicesLive } from "./useServices";
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
  const service = query.data?.find((s) => s.id === serviceId);
  const state = useMemo(() => (service ? appState(service, servers) : undefined), [service, servers]);
  const value = useMemo(
    () => (service && state ? { service, state, services: query.data ?? [] } : undefined),
    [service, state, query.data],
  );

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

function Crumbs({ name }: { name?: string }) {
  const { t } = useLingui();
  return (
    <nav className="app-crumbs" aria-label={t`Breadcrumb`}>
      <Link to="/apps">
        <Trans>Apps</Trans>
      </Link>
      {name ? (
        <>
          <span aria-hidden="true">/</span>
          <span aria-current="page">{name}</span>
        </>
      ) : null}
    </nav>
  );
}

function AppFrame({ reconnecting, children }: { reconnecting: boolean; children: ReactNode }) {
  const { t } = useLingui();
  const { service, state } = useApp();
  const words = useAppWords();
  const time = useTimeWords();
  const phone = useMediaQuery(PHONE_QUERY);
  const now = useControlPlaneNow(30_000);
  const [deploying, setDeploying] = useState(false);
  const serviceId = service.id;
  const name = nameOf(service);
  const image = imageOf(service);
  const revision = revisionName(service.revision);
  const latest = useRevisions(serviceId).revisions?.[0];
  const actor = latest?.change?.appliedBy?.name || latest?.change?.proposedBy?.name;
  const ago = service.updateTime ? time.ago(timestampDate(service.updateTime), now) : "";
  const stateWord = words.state(state);
  const moving = state.kind === "deploying" || state.kind === "waiting";
  return (
    <Page
      title={name}
      back="/apps"
      breadcrumb={<Crumbs name={name} />}
      badge={
        <>
          <StatusBadge status={state.status} label={stateWord} labelIsState />
          <code className="app-revision">{revision}</code>
        </>
      }
      lead={
        <>
          <code>{image}</code>
          {" · "}
          {moving ? (
            <Trans>started {ago}</Trans>
          ) : actor ? (
            <Trans>
              deployed {ago} by {actor}
            </Trans>
          ) : (
            <Trans>deployed {ago}</Trans>
          )}
        </>
      }
      actions={
        <>
          <RouterButtonLink to="/apps/$serviceId/logs" params={{ serviceId }} icon="terminal">
            <Trans>Logs</Trans>
          </RouterButtonLink>
          <Button variant="primary" icon="deploy" onClick={() => setDeploying(true)}>
            <Trans>Deploy</Trans>
          </Button>
        </>
      }
      phoneSub={`${revision} · ${stateWord}`}
      status={reconnecting ? <Reconnecting /> : undefined}
      className="app-page"
    >
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
      {children}
      {phone ? (
        <div className="app-actionbar">
          <RouterButtonLink to="/apps/$serviceId/logs" params={{ serviceId }} icon="terminal" size="lg">
            <Trans>Logs</Trans>
          </RouterButtonLink>
          <Button variant="primary" icon="deploy" size="lg" onClick={() => setDeploying(true)}>
            <Trans>Deploy</Trans>
          </Button>
        </div>
      ) : null}
      {deploying ? <DeploySheet service={service} onClose={() => setDeploying(false)} /> : null}
    </Page>
  );
}
