// SPDX-License-Identifier: AGPL-3.0-only

import { createClient } from "@connectrpc/connect";
import { useTransport } from "@connectrpc/connect-query";
import {
  ChangeService,
  DockerService,
  FileService,
  FirewallService,
  ImportService,
  ProcessService,
  type Server,
  ServerService,
  TerminalService,
  UpdateService,
} from "@dockwell/api";
import { type IconName, TabNav, Tag } from "@dockwell/ui";
import { Trans, useLingui } from "@lingui/react/macro";
import { Link, useNavigate, useRouterState } from "@tanstack/react-router";
import { Fragment, type ReactNode, useMemo } from "react";

import { RouterTabLink } from "../../lib/links";
import { useMay } from "../auth/may";
import { useChangeSheet } from "../changes/ChangeSheet";
import { useOnePanelHere } from "../import/onePanelHere";
import { usePanelOn } from "../import/StopPanel";
import { ActionMenu, type MenuAction } from "../shell/ActionMenu";
import { usePageCommands } from "../shell/CommandMenu";
import { EmptyState } from "../shell/EmptyState";
import { Page, type PageProps } from "../shell/Page";
import { summarize, useServerUpdates } from "../updates/updates";
import { Connecting, usePageReconnecting } from "./FleetNotes";
import { publicAddressOf } from "./network";
import { useRemoveServer } from "./RemoveServer";
import { ServerAddress } from "./ServerAddress";
import { ServerLabels } from "./ServerLabels";
import { primaryAddress, useServerWords } from "./server";
import { useFleet, useFleetUnread } from "./useFleet";
import "./ServerFrame.css";

/** What a tab changes of the phone's header, for a screen of its own within it. */
export type PhoneHead = Pick<PageProps, "phoneTitle" | "phoneMachineTitle" | "phoneSub" | "backLink">;

interface ServerFrameProps {
  id: string;
  /** What the phone's header says instead of the server's name. */
  phoneHead?: PhoneHead | undefined;
  /** Leaves the tabs out on a phone, for a screen that takes all of it. */
  noTabs?: boolean;
  /** The tab's own screen, for the server. */
  children: (server: Server) => ReactNode;
}

/**
 * The page a server's frame stands in, whatever state it's in: a wide page, as its rows of panels and its tables, such
 * as Processes, use the width. Every state takes it from here, so the page keeps its width as the fleet's state
 * arrives.
 */
function ServerFramePage(props: Omit<PageProps, "width">) {
  return <Page {...props} width="wide" />;
}

/**
 * One server's own page, which exists from the first server on, whichever tab
 * is open: its name, what it is, and its tabs, each a route of its own.
 */
export function ServerFrame({ id, phoneHead, noTabs = false, children }: ServerFrameProps) {
  const { t } = useLingui();
  const state = useFleet();
  const status = usePageReconnecting(state.status);
  if (state.status === "connecting") {
    return (
      <ServerFramePage title={t`Server`}>
        <Connecting error={state.error} />
      </ServerFramePage>
    );
  }
  const server = state.servers.find((s) => s.id === id);
  if (!server) {
    return (
      <ServerFramePage title={t`Server`} status={status}>
        <EmptyState
          icon="server"
          title={<Trans>No server with this ID</Trans>}
          action={
            <Link to="/" className="dw-btn dw-btn-secondary dw-btn-lg">
              <Trans>Go to the overview</Trans>
            </Link>
          }
        >
          <p>
            <Trans>
              <code>{id}</code> may have been removed, or the link may be wrong.
            </Trans>
          </p>
        </EmptyState>
      </ServerFramePage>
    );
  }
  return (
    <ServerPage server={server} fleet={state.servers.length > 1} status={status} phoneHead={phoneHead} noTabs={noTabs}>
      {children(server)}
    </ServerPage>
  );
}

interface ServerPageProps {
  server: Server;
  fleet: boolean;
  status: ReactNode;
  phoneHead: PhoneHead | undefined;
  noTabs: boolean;
  children: ReactNode;
}

function ServerPage({ server, fleet, status, phoneHead, noTabs, children }: ServerPageProps) {
  const { t } = useLingui();
  const may = useMay();
  const unread = useFleetUnread();
  const { up, phoneSub } = useServerWords(server);
  const inv = server.inventory;
  const address = publicAddressOf(server) ?? primaryAddress(inv?.addresses ?? []);
  // The hostname only when it says something the name and address don't.
  const hostname = inv?.hostname && inv.hostname !== server.name && inv.hostname !== address ? inv.hostname : "";
  const parts: { key: string; node: ReactNode }[] = [
    { key: "hostname", node: hostname },
    { key: "address", node: address ? <ServerAddress address={address} className="server-address" /> : "" },
    { key: "up", node: up },
  ].filter((part) => part.node);
  const lead =
    parts.length > 0
      ? parts.map(({ key, node }, i) => (
          <Fragment key={key}>
            {i > 0 ? " · " : null}
            {node}
          </Fragment>
        ))
      : undefined;
  // The command bar offers this server's places first (#795), as its tabs do, where the reader may go.
  const navigate = useNavigate();
  const name = server.name;
  const serverId = server.id;
  const removal = useRemoveServer(server);
  const reboot = useRebootAction(server);
  // What acts on the server as a whole, in one menu at its header (#800), so no card ends with it.
  const actions = [reboot, removal.action].filter((a) => a !== undefined);
  usePageCommands([
    // The header menu's steps, which a phone reaches from its search, as it has no header menu (#795).
    ...actions
      .filter((a) => a.onSelect !== undefined && !a.off && !a.disabled)
      .map((a) => ({
        id: a.id,
        icon: (a.icon ?? "more") as IconName,
        label: a.id === "remove" ? t`Remove ${name} from the fleet` : t`Reboot ${name}`,
        run: () => a.onSelect?.(),
      })),
    ...(may(DockerService.method.getDockerOverview).allowed
      ? [
          {
            id: "docker",
            icon: "layers" as const,
            label: t`Open Docker on ${name}`,
            run: () => void navigate({ to: "/servers/$serverId/docker", params: { serverId } }),
          },
        ]
      : []),
    ...(may(TerminalService.method.listTerminals).allowed
      ? [
          {
            id: "terminal",
            icon: "terminal" as const,
            label: t`Open a terminal on ${name}`,
            run: () => void navigate({ to: "/servers/$serverId/terminal", params: { serverId } }),
          },
        ]
      : []),
    ...(may(FileService.method.listFiles).allowed
      ? [
          {
            id: "files",
            icon: "folder" as const,
            label: t`Browse the files on ${name}`,
            run: () => void navigate({ to: "/servers/$serverId/files", params: { serverId } }),
          },
        ]
      : []),
    ...(may(ProcessService.method.watchProcesses).allowed
      ? [
          {
            id: "processes",
            icon: "gauge" as const,
            label: t`Show the processes on ${name}`,
            run: () => void navigate({ to: "/servers/$serverId/processes", params: { serverId } }),
          },
        ]
      : []),
  ]);
  return (
    <ServerFramePage
      title={server.name}
      machineTitle
      actions={actions.length > 0 ? <ActionMenu label={t`Actions for ${name}`} actions={actions} /> : undefined}
      badge={
        <>
          {server.controlServer ? <Tag>{t`Runs the control plane`}</Tag> : null}
          <ServerLabels server={server} />
        </>
      }
      lead={lead}
      phoneSub={unread ? t`Unknown` : phoneSub}
      back={fleet ? "/servers" : undefined}
      status={status}
      className="server-page"
      {...phoneHead}
    >
      {noTabs ? null : (
        <TabNav label={t`Server ${server.name}`}>
          <RouterTabLink to="/servers/$serverId" params={{ serverId: server.id }} activeOptions={{ exact: true }}>
            <Trans>Overview</Trans>
          </RouterTabLink>
          {/* A way to a place you can't read is left out (ADR 0059). */}
          {may(DockerService.method.getDockerOverview).allowed ? (
            <RouterTabLink to="/servers/$serverId/docker" params={{ serverId: server.id }}>
              <Trans>Docker</Trans>
            </RouterTabLink>
          ) : null}
          {may(FileService.method.listFiles).allowed ? (
            <RouterTabLink to="/servers/$serverId/files" params={{ serverId: server.id }}>
              <Trans>Files</Trans>
            </RouterTabLink>
          ) : null}
          {may(TerminalService.method.listTerminals).allowed ? (
            <RouterTabLink to="/servers/$serverId/terminal" params={{ serverId: server.id }}>
              <Trans>Terminal</Trans>
            </RouterTabLink>
          ) : null}
          {may(UpdateService.method.getServerUpdates).allowed ? <UpdatesTab server={server} /> : null}
          {may(FirewallService.method.getServerFirewall).allowed ? (
            <RouterTabLink to="/servers/$serverId/firewall" params={{ serverId: server.id }}>
              <Trans>Firewall</Trans>
            </RouterTabLink>
          ) : null}
          {may(ProcessService.method.watchProcesses).allowed ? (
            <RouterTabLink to="/servers/$serverId/processes" params={{ serverId: server.id }}>
              <Trans>Processes</Trans>
            </RouterTabLink>
          ) : null}
          {may(ServerService.method.watchSystemServices).allowed ? (
            <RouterTabLink to="/servers/$serverId/services" params={{ serverId: server.id }}>
              <Trans>System services</Trans>
            </RouterTabLink>
          ) : null}
          {may(ServerService.method.watchSystemServices).allowed ? (
            <RouterTabLink to="/servers/$serverId/log" params={{ serverId: server.id }}>
              <Trans>System log</Trans>
            </RouterTabLink>
          ) : null}
          {may(ImportService.method.importOnePanel).allowed ? <MoveInTab server={server} /> : null}
        </TabNav>
      )}
      {children}
      {removal.sheet}
    </ServerFramePage>
  );
}

/** Reboot, as a header menu's step: the plan says what goes offline, and asks for the passkey. */
function useRebootAction(server: Server): MenuAction | undefined {
  const { t } = useLingui();
  const sheet = useChangeSheet();
  const transport = useTransport();
  const client = useMemo(() => createClient(ChangeService, transport), [transport]);
  const may = useMay()(ChangeService.method.planReboot, { install: true });
  if (!may.allowed) return undefined;
  return {
    id: "reboot",
    icon: "refresh",
    label: t`Reboot`,
    onSelect: () => sheet.open({ kind: "plan", make: () => client.planReboot({ serverIds: [server.id] }) }),
  };
}

/**
 * The Move in tab, where 1Panel's containers are left to move or Coolify or Dokploy
 * runs, and on its own page:
 * a way in that doesn't rest on the Overview's callout, which someone can put away.
 */
function MoveInTab({ server }: { server: Server }) {
  const onePanel = useOnePanelHere(server);
  const panel = usePanelOn(server);
  const here = onePanel || panel !== undefined;
  const on = useRouterState({ select: (s) => s.location.pathname.endsWith("/import") });
  if (!here && !on) return null;
  return (
    <RouterTabLink to="/servers/$serverId/import" params={{ serverId: server.id }}>
      <Trans>Move in</Trans>
    </RouterTabLink>
  );
}

/**
 * The Updates tab, with how many security fixes wait, in a warning's tone, or how
 * many updates do when none is for security. Opening a server's page is looking,
 * so this is what has its agent read them (ADR 0043).
 */
function UpdatesTab({ server }: { server: Server }) {
  const query = useServerUpdates(server.id);
  const s = summarize(query.data?.updates);
  const security = s.security.length;
  return (
    <RouterTabLink
      to="/servers/$serverId/updates"
      params={{ serverId: server.id }}
      count={security > 0 ? security : s.total > 0 ? s.total : undefined}
      countTone={security > 0 ? "warning" : undefined}
    >
      <Trans>Updates</Trans>
    </RouterTabLink>
  );
}
