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
import { cx, Icon, type IconName, TabNav, Tag, useMediaQuery } from "@dockwell/ui";
import { Trans, useLingui } from "@lingui/react/macro";
import { Link, useNavigate, useRouterState } from "@tanstack/react-router";
import {
  type CSSProperties,
  type FocusEvent,
  Fragment,
  type KeyboardEvent,
  type ReactNode,
  useEffect,
  useId,
  useLayoutEffect,
  useMemo,
  useRef,
  useState,
} from "react";

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
export function ServerFrame({ id, phoneHead, noTabs = false, fullBody = false, children }: ServerFrameProps) {
  const { t } = useLingui();
  const state = useFleet();
  const status = usePageReconnecting(state.status);
  if (state.status === "connecting") {
    return (
      <ServerFramePage title={t`Server`}>
        <Connecting error={state.error} shape="server" />
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
    <ServerPage
      server={server}
      fleet={state.servers.length > 1}
      status={status}
      phoneHead={phoneHead}
      noTabs={noTabs}
      fullBody={fullBody}
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
  children: ReactNode;
}

function ServerPage({ server, fleet, status, phoneHead, noTabs, fullBody, children }: ServerPageProps) {
  const { t } = useLingui();
  const may = useMay();
  const narrow = useMediaQuery("(max-width: 960px)");
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
      fullBody={fullBody}
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
  const [open, setOpen] = useState(false);
  const [at, setAt] = useState<CSSProperties>();
  const root = useRef<HTMLSpanElement>(null);
  const menu = useRef<HTMLDivElement>(null);
  const id = useId();
  useLayoutEffect(() => {
    if (!open) {
      setAt(undefined);
      return;
    }
    const button = root.current?.querySelector("button")?.getBoundingClientRect();
    const box = menu.current?.getBoundingClientRect();
    if (!button || !box) return;
    const gutter = 16;
    const right = Math.max(gutter, Math.min(window.innerWidth - button.right, window.innerWidth - box.width - gutter));
    setAt({ top: button.bottom + 4, right });
  }, [open]);
  useEffect(() => {
    if (!open) return;
    root.current?.querySelector<HTMLElement>('[role="menuitem"]')?.focus({ preventScroll: true });
    const away = (e: PointerEvent) => {
      if (!root.current?.contains(e.target as Node)) setOpen(false);
    };
    const close = () => setOpen(false);
    document.addEventListener("pointerdown", away);
    window.addEventListener("resize", close);
    return () => {
      document.removeEventListener("pointerdown", away);
      window.removeEventListener("resize", close);
    };
  }, [open]);
  if (items.length === 0) return null;
  const active = items.find((item) => pathname.endsWith(item.suffix));
  const closeMenu = () => {
    setOpen(false);
    root.current?.querySelector<HTMLButtonElement>("button")?.focus();
  };
  const onKeys = (e: KeyboardEvent<HTMLDivElement>) => {
    const entries = [...(root.current?.querySelectorAll<HTMLElement>('[role="menuitem"]') ?? [])];
    const index = entries.indexOf(document.activeElement as HTMLElement);
    if (e.key === "Escape") {
      e.preventDefault();
      e.stopPropagation();
      closeMenu();
    } else if (e.key === "ArrowDown" || e.key === "ArrowUp") {
      e.preventDefault();
      const step = e.key === "ArrowDown" ? 1 : -1;
      entries[(index + step + entries.length) % entries.length]?.focus();
    }
  };
  const onBlur = (e: FocusEvent<HTMLDivElement>) => {
    if (!root.current?.contains(e.relatedTarget as Node | null)) setOpen(false);
  };
  return (
    <span className="server-tabs-menu" ref={root}>
      <button
        type="button"
        className="dw-tab server-tabs-trigger"
        aria-current={active ? "page" : undefined}
        aria-haspopup="menu"
        aria-expanded={open}
        aria-controls={open ? id : undefined}
        onClick={() => setOpen((v) => !v)}
      >
        <span>{active ? active.label : t`System`}</span>
        {updateCount !== undefined ? (
          <span className={cx("dw-tab-count", security > 0 && "dw-tone-warning")}>{updateCount}</span>
        ) : null}
        <Icon name="chevron-down" size={14} />
      </button>
      {open ? (
        <div
          id={id}
          ref={menu}
          className={cx("action-menu", at && "is-placed")}
          style={at}
          role="menu"
          aria-label={t`System tabs`}
          onKeyDown={onKeys}
          onBlur={onBlur}
        >
          {items.map((item) => (
            <button
              key={item.id}
              type="button"
              role="menuitem"
              className={cx("action-menu-item", active?.id === item.id && "is-current")}
              onClick={() => {
                setOpen(false);
                void navigate({ to: item.to, params: { serverId: server.id } });
              }}
            >
              <span className="action-menu-words">
                <span>{item.label}</span>
              </span>
              {item.count !== undefined ? (
                <span className={cx("dw-tab-count", item.tone && `dw-tone-${item.tone}`)}>{item.count}</span>
              ) : null}
            </button>
          ))}
        </div>
      ) : null}
    </span>
  );
}
