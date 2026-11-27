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
import { cx, Icon, type IconName, PHONE_QUERY, TabNav, Tag, useMediaQuery } from "@dockwell/ui";
import { plural } from "@lingui/core/macro";
import { Trans, useLingui } from "@lingui/react/macro";
import { Link, useNavigate, useRouterState } from "@tanstack/react-router";
import { Fragment, type ReactNode, useLayoutEffect, useMemo, useRef, useState } from "react";

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
  /** Lets the body span past the wide column, as System log does, while the head and tabs stay in place. */
  fullBody?: boolean;
  /** Caps the page column width; defaults to wide for tables and multi-column tools. */
  width?: PageProps["width"];
  /** The tab's own screen, for the server. */
  children: (server: Server) => ReactNode;
}

/**
 * The page a server's frame stands in, whatever state it's in: a wide page by default, as its tables,
 * such as Processes, use the width, or standard on Overview to keep cards balanced at 1920px.
 */
function ServerFramePage({ width = "wide", ...props }: PageProps) {
  return <Page {...props} width={width} />;
}

/**
 * One server's own page, which exists from the first server on, whichever tab
 * is open: its name, what it is, and its tabs, each a route of its own.
 */
export function ServerFrame({
  id,
  phoneHead,
  noTabs = false,
  fullBody = false,
  width = "wide",
  children,
}: ServerFrameProps) {
  const { t } = useLingui();
  const state = useFleet();
  const status = usePageReconnecting(state.status);
  if (state.status === "connecting") {
    return (
      <ServerFramePage title={t`Server`} width={width}>
        <Connecting error={state.error} shape="server" />
      </ServerFramePage>
    );
  }
  const server = state.servers.find((s) => s.id === id);
  if (!server) {
    return (
      <ServerFramePage title={t`Server`} status={status} width={width}>
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
    <ServerPage
      server={server}
      fleet={state.servers.length > 1}
      status={status}
      phoneHead={phoneHead}
      noTabs={noTabs}
      fullBody={fullBody}
      width={width}
    >
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
  fullBody: boolean;
  width: PageProps["width"];
  children: ReactNode;
}

function ServerPage({ server, fleet, status, phoneHead, noTabs, fullBody, width, children }: ServerPageProps) {
  const { t } = useLingui();
  const may = useMay();
  const phone = useMediaQuery(PHONE_QUERY);
  const viewportNarrow = useMediaQuery("(max-width: 1200px)");
  const tabsWrapRef = useRef<HTMLDivElement>(null);
  // Measured before the first paint, so a narrow column never draws all ten tabs first; kept as a yes or no, so a
  // resize re-renders the frame only when the answer changes.
  const [measuredNarrow, setMeasuredNarrow] = useState<boolean | undefined>(undefined);
  useLayoutEffect(() => {
    const el = tabsWrapRef.current;
    if (!el || typeof ResizeObserver === "undefined") return;
    const measure = () => {
      const width = el.getBoundingClientRect().width;
      setMeasuredNarrow(width > 0 ? width < 960 : undefined);
    };
    measure();
    const observer = new ResizeObserver(measure);
    observer.observe(el);
    return () => observer.disconnect();
  }, []);
  const narrow = measuredNarrow ?? viewportNarrow;
  const pathname = useRouterState({ select: (s) => s.location.pathname });
  const onDocker = pathname.startsWith(`/servers/${server.id}/docker`);
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
      width={width}
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
      fullBody={fullBody}
      className="server-page"
      {...phoneHead}
    >
      {noTabs ? null : (
        <div className="server-tabs-wrap" ref={tabsWrapRef}>
          <TabNav label={t`Server ${server.name}`}>
            <RouterTabLink to="/servers/$serverId" params={{ serverId: server.id }} activeOptions={{ exact: true }}>
              <Trans>Overview</Trans>
            </RouterTabLink>
            {/* A way to a place you can't read is left out (ADR 0059). */}
            {may(DockerService.method.getDockerOverview).allowed ? (
              phone && onDocker ? (
                <DockerTabsMenu server={server} multiple={fleet} />
              ) : (
                <RouterTabLink to="/servers/$serverId/docker" params={{ serverId: server.id }}>
                  <Trans>Docker</Trans>
                </RouterTabLink>
              )
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
            {narrow ? (
              <SystemTabsMenu server={server} />
            ) : (
              <>
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
                {may(ServerService.method.getHostToolbox).allowed ? (
                  <RouterTabLink to="/servers/$serverId/toolbox" params={{ serverId: server.id }}>
                    <Trans>Toolbox</Trans>
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
              </>
            )}
            {may(ImportService.method.importOnePanel).allowed ? <MoveInTab server={server} /> : null}
          </TabNav>
        </div>
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

type SystemTabTarget =
  | "/servers/$serverId/updates"
  | "/servers/$serverId/firewall"
  | "/servers/$serverId/processes"
  | "/servers/$serverId/toolbox"
  | "/servers/$serverId/services"
  | "/servers/$serverId/log";

/**
 * On phones, folds Docker sub-views into the Docker tab trigger so Docker pages keep one tab bar.
 */
function DockerTabsMenu({ server, multiple }: { server: Server; multiple: boolean }) {
  const { t } = useLingui();
  const may = useMay();
  const navigate = useNavigate();
  const pathname = useRouterState({ select: (s) => s.location.pathname });
  const base = `/servers/${server.id}/docker`;
  const rest = pathname.startsWith(base) ? pathname.slice(base.length) : "";
  const canReadSettings = may(DockerService.method.getDockerSettings).allowed;
  const views = [
    {
      id: "overview",
      label: t`Overview`,
      active: rest === "" || rest === "/",
      go: () => void navigate({ to: "/servers/$serverId/docker", params: { serverId: server.id } }),
    },
    {
      id: "containers",
      label: t`Containers`,
      active: rest.startsWith("/containers"),
      go: () => void navigate({ to: "/servers/$serverId/docker/containers", params: { serverId: server.id } }),
    },
    {
      id: "composes",
      label: t`Composes`,
      active: rest.startsWith("/composes"),
      go: () => void navigate({ to: "/servers/$serverId/docker/composes", params: { serverId: server.id } }),
    },
    {
      id: "images",
      label: t`Images`,
      active: rest.startsWith("/images"),
      go: () => void navigate({ to: "/servers/$serverId/docker/images", params: { serverId: server.id } }),
    },
    {
      id: "networks",
      label: t`Networks`,
      active: rest.startsWith("/networks"),
      go: () => void navigate({ to: "/servers/$serverId/docker/networks", params: { serverId: server.id } }),
    },
    {
      id: "volumes",
      label: t`Volumes`,
      active: rest.startsWith("/volumes"),
      go: () => void navigate({ to: "/servers/$serverId/docker/volumes", params: { serverId: server.id } }),
    },
    {
      id: "cleanup",
      label: t`Cleanup`,
      active: rest.startsWith("/cleanup"),
      go: () => void navigate({ to: "/servers/$serverId/docker/cleanup", params: { serverId: server.id } }),
    },
    ...(canReadSettings
      ? [
          {
            id: "settings",
            label: t`Settings`,
            active: rest.startsWith("/settings"),
            go: () => void navigate({ to: "/servers/$serverId/docker/settings", params: { serverId: server.id } }),
          },
        ]
      : []),
  ];
  const current = views.find((view) => view.active) ?? views[0];
  const actions: MenuAction[] = [
    ...views.map((view) => ({
      id: view.id,
      label: view.label,
      current: view.active,
      onSelect: view.go,
    })),
    ...(multiple
      ? [
          {
            id: "all-servers",
            label: t`All servers`,
            icon: "grid" as const,
            onSelect: () => {
              const tab = current?.id;
              void navigate({
                to: "/servers/docker",
                search:
                  tab === "containers" ||
                  tab === "composes" ||
                  tab === "images" ||
                  tab === "networks" ||
                  tab === "volumes" ||
                  tab === "cleanup"
                    ? { view: tab }
                    : {},
              });
            },
          },
        ]
      : []),
  ];
  const triggerLabel = current ? t`Docker: ${current.label}` : t`Docker`;
  return (
    <ActionMenu
      className="server-tabs-menu"
      label={triggerLabel}
      actions={actions}
      trigger={(props) => (
        <button type="button" className="dw-tab server-tabs-trigger" aria-current="page" {...props}>
          <span>{triggerLabel}</span>
          <Icon name="chevron-down" size={14} />
        </button>
      )}
    />
  );
}

/**
 * On narrow screens, groups the six secondary host tabs behind one System tab menu so the
 * tab strip fits without scrolling past half the server's tools.
 */
function SystemTabsMenu({ server }: { server: Server }) {
  const { t } = useLingui();
  const may = useMay();
  const navigate = useNavigate();
  const pathname = useRouterState({ select: (s) => s.location.pathname });
  const mayUpdates = may(UpdateService.method.getServerUpdates).allowed;
  const updates = useServerUpdates(server.id);
  const s = mayUpdates ? summarize(updates.data?.updates) : undefined;
  const security = s?.security.length ?? 0;
  const updateCount = security > 0 ? security : (s?.total ?? 0) > 0 ? s?.total : undefined;
  const items: {
    id: string;
    label: string;
    to: SystemTabTarget;
    suffix: string;
    count?: number | undefined;
    tone?: "warning" | undefined;
  }[] = [
    ...(mayUpdates
      ? [
          {
            id: "updates",
            label: t`Updates`,
            to: "/servers/$serverId/updates" as const,
            suffix: "/updates",
            count: updateCount,
            tone: security > 0 ? ("warning" as const) : undefined,
          },
        ]
      : []),
    ...(may(FirewallService.method.getServerFirewall).allowed
      ? [{ id: "firewall", label: t`Firewall`, to: "/servers/$serverId/firewall" as const, suffix: "/firewall" }]
      : []),
    ...(may(ProcessService.method.watchProcesses).allowed
      ? [{ id: "processes", label: t`Processes`, to: "/servers/$serverId/processes" as const, suffix: "/processes" }]
      : []),
    ...(may(ServerService.method.getHostToolbox).allowed
      ? [{ id: "toolbox", label: t`Toolbox`, to: "/servers/$serverId/toolbox" as const, suffix: "/toolbox" }]
      : []),
    ...(may(ServerService.method.watchSystemServices).allowed
      ? [
          {
            id: "services",
            label: t`System services`,
            to: "/servers/$serverId/services" as const,
            suffix: "/services",
          },
          { id: "log", label: t`System log`, to: "/servers/$serverId/log" as const, suffix: "/log" },
        ]
      : []),
  ];
  if (items.length === 0) return null;
  const active = items.find((item) => pathname.endsWith(item.suffix));
  const showCount = updateCount !== undefined && (!active || active.id === "updates");
  const showDot = updateCount !== undefined && active !== undefined && active.id !== "updates";
  const actions: MenuAction[] = items.map((item) => ({
    id: item.id,
    label: item.label,
    current: active?.id === item.id,
    badge:
      item.count !== undefined ? (
        <span className={cx("dw-tab-count", item.tone && `dw-tone-${item.tone}`)}>{item.count}</span>
      ) : undefined,
    onSelect: () => void navigate({ to: item.to, params: { serverId: server.id } }),
  }));
  const triggerText = active ? active.label : t`System`;
  const triggerLabel =
    updateCount !== undefined
      ? `${triggerText} (${plural(updateCount, { one: "# update", other: "# updates" })})`
      : triggerText;
  return (
    <ActionMenu
      className="server-tabs-menu"
      label={triggerLabel}
      actions={actions}
      trigger={(props) => (
        <button
          type="button"
          className="dw-tab server-tabs-trigger"
          aria-current={active ? "page" : undefined}
          {...props}
        >
          <span>{triggerText}</span>
          {showCount ? (
            <span className={cx("dw-tab-count", security > 0 && "dw-tone-warning")}>{updateCount}</span>
          ) : showDot ? (
            <span className={cx("server-tabs-dot", security > 0 && "dw-tone-warning")} aria-hidden="true" />
          ) : null}
          <Icon name="chevron-down" size={14} />
        </button>
      )}
    />
  );
}
