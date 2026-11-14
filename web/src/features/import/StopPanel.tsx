// SPDX-License-Identifier: AGPL-3.0-only

import { createClient } from "@connectrpc/connect";
import { useQuery, useTransport } from "@connectrpc/connect-query";
import {
  Connection,
  ContainerService,
  ContainerState,
  ImportService,
  PanelKind,
  type Server,
  type ServerContainer,
} from "@dockwell/api";
import { Button, Callout } from "@dockwell/ui";
import { Trans, useLingui } from "@lingui/react/macro";
import { useMemo, useState } from "react";

import { useMay } from "../auth/may";
import { useChangeSheet } from "../changes/ChangeSheet";

/*
 * Stopping the panel an import came from (docs/adr/0089-importing-from-coolify-and-dokploy.md):
 * Coolify's own containers, or Dokploy's own services, as a change with a plan the
 * sheet makes. What the panel ran keeps running, and undoing it starts the panel
 * again as it was.
 */

/**
 * The panel whose own app runs on a server, as its list of containers says:
 * Coolify's, or a task of Dokploy's Swarm service, which Swarm names
 * dokploy.<slot>.<task>.
 */
export function panelRunning(containers: ServerContainer[]): PanelKind | undefined {
  const found = panelOn(containers);
  return found?.running ? found.kind : undefined;
}

/** A panel on a server: which, whether its own app runs, and whether its proxy is there and stopped. */
export interface PanelOn {
  kind: PanelKind;
  running: boolean;
  proxyStopped: boolean;
}

/** Each panel's proxy, which holds ports 80 and 443 until its domains come over. */
export const PANEL_PROXIES: Record<PanelKind, string> = {
  [PanelKind.UNSPECIFIED]: "",
  [PanelKind.COOLIFY]: "coolify-proxy",
  [PanelKind.DOKPLOY]: "dokploy-traefik",
};

/** The panel whose own app is on a server, running or stopped, as its list of containers says. */
export function panelOn(containers: ServerContainer[]): PanelOn | undefined {
  let found: PanelOn | undefined;
  for (const c of containers) {
    const running = c.state === ContainerState.RUNNING;
    const kind =
      c.name === "coolify" && c.image.includes("coollabsio/coolify")
        ? PanelKind.COOLIFY
        : c.name.startsWith("dokploy.") && c.image.includes("dokploy/dokploy")
          ? PanelKind.DOKPLOY
          : undefined;
    if (kind !== undefined && (found === undefined || running)) found = { kind, running, proxyStopped: false };
  }
  if (found) {
    const proxy = PANEL_PROXIES[found.kind];
    found.proxyStopped = containers.some((c) => c.name === proxy && c.state !== ContainerState.RUNNING);
  }
  return found;
}

/** The panel that runs on a server, where someone here may stop it. */
function usePanelHere(server: Server): PanelKind | undefined {
  return usePanelRunning(server, useMay()(ImportService.method.planStopPanel).allowed);
}

/**
 * The panel whose own app runs on a server, as its list of containers says, where
 * allowed too and someone here may read that list.
 */
export function usePanelRunning(server: Server, allowedToo = true): PanelKind | undefined {
  const found = usePanelOn(server, allowedToo);
  return found?.running ? found.kind : undefined;
}

/** The panel whose own app is on a server, running or stopped, where someone here may read its list. */
export function usePanelOn(server: Server, allowedToo = true): PanelOn | undefined {
  const may = useMay();
  const readable = server.connection === Connection.CONNECTED && server.dockerUnreadableSince === undefined;
  const allowed = may(ContainerService.method.listContainers).allowed && allowedToo;
  // The same read as the containers panel's, which the query cache shares.
  const list = useQuery(
    ContainerService.method.listContainers,
    { server: server.id },
    { retry: false, enabled: readable && allowed },
  );
  return readable && allowed ? panelOn(list.data?.containers ?? []) : undefined;
}

function notNowKey(serverId: string): string {
  return `dockwell.stopPanel.notNow.${serverId}`;
}

function saidNotNow(serverId: string): boolean {
  try {
    return localStorage.getItem(notNowKey(serverId)) !== null;
  } catch {
    return false;
  }
}

/**
 * On a server's Overview, where Coolify or Dokploy runs: a way to stop it, as a plan
 * that says what stops and what keeps running, until someone says not now.
 */
export function StopPanelCallout({ server }: { server: Server }) {
  const { t } = useLingui();
  const kind = usePanelHere(server);
  const transport = useTransport();
  const imports = useMemo(() => createClient(ImportService, transport), [transport]);
  const sheet = useChangeSheet();
  const [notNow, setNotNow] = useState(() => saidNotNow(server.id));
  if (kind === undefined || notNow) return null;
  const name = kind === PanelKind.COOLIFY ? "Coolify" : "Dokploy";
  const dismiss = () => {
    try {
      localStorage.setItem(notNowKey(server.id), "1");
    } catch {
      // A private window forgets, and it shows again next time.
    }
    setNotNow(true);
  };
  // The sheet makes the plan, so it's the sheet's to discard, and it says a refusal.
  const stop = () => {
    sheet.open({
      kind: "plan",
      make: async () => {
        const res = await imports.planStopPanel({ serverId: server.id, panel: kind });
        return { change: res.change };
      },
    });
  };
  return (
    <Callout
      tone="info"
      title={t`${name} runs on this server`}
      actions={
        <>
          <Button size="sm" icon="stop" onClick={stop}>
            <Trans>Stop {name}</Trans>
          </Button>
          <Button size="sm" variant="ghost" onClick={dismiss}>
            <Trans>Not now</Trans>
          </Button>
        </>
      }
    >
      <Trans>Stopping it ends its deploys, and what it ran keeps running.</Trans>
    </Callout>
  );
}
