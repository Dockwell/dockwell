// SPDX-License-Identifier: AGPL-3.0-only

import { useQuery } from "@connectrpc/connect-query";
import { ResourceService } from "@dockwell/api";
import { TabNav } from "@dockwell/ui";
import { Trans, useLingui } from "@lingui/react/macro";
import { Link } from "@tanstack/react-router";
import type { ReactNode } from "react";

import { RouterTabLink } from "../../lib/links";
import { Page } from "../shell/Page";
import "./Apps.css";

/**
 * One app's frame, whichever tab is open: where it sits, its name, and its
 * tabs, each of them a route of its own under /apps/<id>.
 */
export function AppLayout({ serviceId, children }: { serviceId: string; children: ReactNode }) {
  const { t } = useLingui();
  const services = useQuery(ResourceService.method.listResources, { kind: "service" }).data?.resources;
  const name = services?.find((s) => s.id === serviceId)?.resource?.name ?? t`App`;
  return (
    <Page
      title={name}
      back="/apps"
      breadcrumb={
        <nav className="app-crumbs" aria-label={t`Breadcrumb`}>
          <Link to="/apps">
            <Trans>Apps</Trans>
          </Link>
          <span aria-hidden="true">/</span>
          <span aria-current="page">{name}</span>
        </nav>
      }
    >
      <TabNav label={t`App sections`}>
        <RouterTabLink to="/apps/$serviceId" params={{ serviceId }} activeOptions={{ exact: true }}>
          <Trans>Overview</Trans>
        </RouterTabLink>
        <RouterTabLink to="/apps/$serviceId/deployments" params={{ serviceId }}>
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
    </Page>
  );
}
