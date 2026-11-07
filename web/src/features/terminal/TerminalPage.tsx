// SPDX-License-Identifier: AGPL-3.0-only

import { createClient } from "@connectrpc/connect";
import { createConnectQueryKey, useQuery, useTransport } from "@connectrpc/connect-query";
import { type Server, StepUpAction, type Terminal, TerminalService } from "@dockwell/api";
import {
  Button,
  Callout,
  Icon,
  PHONE_QUERY,
  StatusBadge,
  TextField,
  useMediaQuery,
  useResolvedTheme,
} from "@dockwell/ui";
import { Trans, useLingui } from "@lingui/react/macro";
import { useQueryClient } from "@tanstack/react-query";
import { Link } from "@tanstack/react-router";
import { type KeyboardEvent, type ReactNode, useCallback, useEffect, useMemo, useRef, useState } from "react";

import { reasonOf, sentence } from "../../lib/errors";
import { formatBytes } from "../../lib/format";
import { PasskeyError } from "../../lib/webauthn";
import { useAuthError } from "../auth/messages";
import { withStepUpIfAsked } from "../auth/stepUp";
import { ServerFrame } from "../servers/ServerFrame";
import { EmptyState } from "../shell/EmptyState";
import type { TerminalStatus } from "./session";
import { type ScreenHandle, TerminalScreen } from "./TerminalScreen";
import { useEndWords } from "./words";
import "./TerminalPage.css";

/** The type's size, as the mocks set it. */
const DESKTOP_FONT = 12.5;
const PHONE_FONT = 13;

/** A server's terminal, as its own tab of the server's page. */
export function TerminalPage({ serverId }: { serverId: string }) {
  const phone = useMediaQuery(PHONE_QUERY);
  const { t } = useLingui();
  const [title, setTitle] = useState<string>();
  return (
    <ServerFrame
      id={serverId}
      noTabs={phone}
      phoneHead={
        phone
          ? {
              phoneTitle: t`Terminal`,
              phoneSub: title ? t`${title} · through the agent` : t`Through the agent`,
              backLink: (
                <Link
                  to="/servers/$serverId"
                  params={{ serverId }}
                  className="mobile-header-button"
                  aria-label={t`Back to the server`}
                >
                  <Icon name="arrow-left" size={20} />
                </Link>
              ),
            }
          : undefined
      }
    >
      {(server) => <Terminals server={server} phone={phone} onTitle={setTitle} />}
    </ServerFrame>
  );
}

/** Each tab's name: its title, and a number where two share one. */
function tabNames(tabs: readonly Terminal[]): Map<string, string> {
  const seen = new Map<string, number>();
  const names = new Map<string, string>();
  for (const tab of tabs) {
    const n = (seen.get(tab.title) ?? 0) + 1;
    seen.set(tab.title, n);
    names.set(tab.id, n === 1 ? tab.title : `${tab.title} (${n})`);
  }
  return names;
}

interface TerminalsProps {
  server: Server;
  phone: boolean;
  onTitle: (title: string | undefined) => void;
}

function Terminals({ server, phone, onTitle }: TerminalsProps) {
  const { t, i18n } = useLingui();
  const transport = useTransport();
  const queryClient = useQueryClient();
  const theme = useResolvedTheme();
  const explain = useAuthError();
  const client = useMemo(() => createClient(TerminalService, transport), [transport]);
  const serverId = server.id;
  // What's open is this page's to know once it has looked: it opens and closes them itself.
  const listed = useQuery(
    TerminalService.method.listTerminals,
    { serverId },
    { staleTime: Number.POSITIVE_INFINITY, refetchOnWindowFocus: false },
  );
  const [tabs, setTabs] = useState<Terminal[]>([]);
  const [active, setActive] = useState<string>();
  const [statuses, setStatuses] = useState<Record<string, TerminalStatus>>({});
  const [size, setSize] = useState<{ columns: number; rows: number }>();
  const [selection, setSelection] = useState(false);
  const [opening, setOpening] = useState(false);
  const [problem, setProblem] = useState<string>();
  const [lost, setLost] = useState<bigint>();
  const [finding, setFinding] = useState(false);
  const [menu, setMenu] = useState(false);
  const handles = useRef(new Map<string, ScreenHandle>());
  const merged = useRef(false);

  useEffect(() => {
    if (merged.current || !listed.data) return;
    merged.current = true;
    const open = listed.data.terminals;
    setTabs((now) => [...open.filter((o) => !now.some((n) => n.id === o.id)), ...now]);
    setActive((now) => now ?? open.at(-1)?.id);
  }, [listed.data]);

  const names = tabNames(tabs);
  const current = tabs.find((tab) => tab.id === active);
  const status = current ? statuses[current.id] : undefined;
  useEffect(() => onTitle(current?.title), [current?.title, onTitle]);

  const handle = () => (active ? handles.current.get(active) : undefined);

  const open = useCallback(async () => {
    setOpening(true);
    setProblem(undefined);
    const want = size ?? { columns: 80, rows: 24 };
    try {
      const res = await withStepUpIfAsked(transport, StepUpAction.OPEN_TERMINAL, serverId, (proof) =>
        client.openTerminal({ serverId, columns: want.columns, rows: want.rows, proof }),
      );
      const opened = res.terminal;
      if (opened) {
        setTabs((now) => [...now, opened]);
        setActive(opened.id);
      }
      void queryClient.invalidateQueries({
        queryKey: createConnectQueryKey({ schema: TerminalService.method.listTerminals, cardinality: "finite" }),
      });
    } catch (err) {
      const reason = reasonOf(err);
      const name = server.name;
      if (reason === "no_passkey_here") {
        setProblem(
          t`Opening a terminal takes a passkey, and you have none for this address. Add one under Profile and security, or connect to ${name} over SSH.`,
        );
      } else if (err instanceof PasskeyError && err.problem === "cancelled") {
        setProblem(t`The passkey prompt closed, so no terminal opened. Open one when you're ready.`);
      } else {
        setProblem(err instanceof PasskeyError ? explain(err) : sentence(err));
      }
    } finally {
      setOpening(false);
    }
  }, [client, explain, queryClient, server.name, serverId, size, t, transport]);

  const forget = (id: string) => {
    handles.current.delete(id);
    setTabs((now) => {
      const at = now.findIndex((tab) => tab.id === id);
      const rest = now.filter((tab) => tab.id !== id);
      if (id === active) setActive(rest[Math.min(at, rest.length - 1)]?.id);
      return rest;
    });
    setStatuses(({ [id]: _, ...rest }) => rest);
  };

  const close = async (id: string) => {
    setMenu(false);
    if (statuses[id]?.kind !== "ended") {
      try {
        await client.closeTerminal({ terminalId: id });
      } catch (err) {
        // One that's gone already is what closing asked for.
        if (reasonOf(err) !== "not_found") {
          setProblem(sentence(err));
          return;
        }
      }
    }
    forget(id);
  };

  const onStatus = useCallback((id: string, next: TerminalStatus) => {
    setStatuses((now) => ({ ...now, [id]: next }));
    if (next.kind === "attached") setLost(undefined);
  }, []);

  const fontSize = phone ? PHONE_FONT : DESKTOP_FONT;
  const screens = tabs.map((tab) => (
    <TerminalScreen
      key={tab.id}
      ref={(h) => {
        if (h) handles.current.set(tab.id, h);
      }}
      terminal={tab}
      transport={transport}
      active={tab.id === active}
      theme={theme}
      fontSize={fontSize}
      label={t`${names.get(tab.id) ?? tab.title} output`}
      onStatus={(s) => onStatus(tab.id, s)}
      onSize={(columns, rows) => tab.id === active && setSize({ columns, rows })}
      onSelection={(has) => tab.id === active && setSelection(has)}
      onLost={(bytes) => tab.id === active && setLost(bytes)}
    />
  ));

  const notice = current ? (
    <Notice
      status={status}
      onBringBack={() => handle()?.bringBack()}
      onOpen={open}
      onClose={() => void close(current.id)}
      opening={opening}
    />
  ) : null;

  const empty = (
    <EmptyState
      icon="terminal"
      title={<Trans>No terminal open</Trans>}
      action={
        <Button variant="primary" size="lg" icon="terminal" loading={opening} onClick={open}>
          <Trans>Open a terminal</Trans>
        </Button>
      }
    >
      <p>
        <Trans>
          A terminal here is a root shell on <code>{server.name}</code>, through its agent, so it needs no SSH port.
          Opening one asks for your passkey.
        </Trans>
      </p>
    </EmptyState>
  );

  const problemCallout = problem ? (
    <Callout tone="critical" title={<Trans>No terminal opened</Trans>}>
      {problem}
    </Callout>
  ) : null;

  const find = (text: string, backwards = false) => handle()?.find(text, backwards);
  // One tree for both layouts, so turning a phone or narrowing a window keeps each
  // terminal where it is, with its scrollback and what's typed that hasn't gone.
  return (
    <div className={phone ? "terminal-layout terminal-phone" : "terminal-layout"}>
      {problemCallout}
      <section className={phone ? "terminal-panel" : "dw-panel terminal-panel"} aria-label={t`Terminal sessions`}>
        {phone ? null : (
          <div className="terminal-head">
            {tabs.length > 0 ? (
              <div role="tablist" aria-label={t`Sessions`} className="terminal-tabs">
                {tabs.map((tab) => (
                  <TerminalTab
                    key={tab.id}
                    name={names.get(tab.id) ?? tab.title}
                    selected={tab.id === active}
                    ended={statuses[tab.id]?.kind === "ended"}
                    onSelect={() => setActive(tab.id)}
                    onClose={() => void close(tab.id)}
                  />
                ))}
              </div>
            ) : null}
            <span className="terminal-head-tool">
              <Button
                variant="ghost"
                size="sm"
                icon="plus"
                aria-label={t`New session`}
                loading={opening}
                onClick={open}
              />
            </span>
            <span className="terminal-head-space" />
            {current ? (
              <span className="terminal-head-tool terminal-tools">
                <Button
                  variant="ghost"
                  size="sm"
                  icon="search"
                  aria-label={t`Find in scrollback`}
                  aria-pressed={finding}
                  onClick={() => {
                    setFinding((was) => {
                      if (was) handle()?.clearFind();
                      return !was;
                    });
                  }}
                />
                <Button
                  variant="ghost"
                  size="sm"
                  icon="copy"
                  aria-label={t`Copy selection`}
                  disabled={!selection}
                  title={selection ? undefined : t`Select text in the terminal to copy it`}
                  onClick={() => void handle()?.copy()}
                />
                <SessionMenu
                  open={menu}
                  onOpenChange={setMenu}
                  onPaste={() => {
                    setMenu(false);
                    void handle()?.paste();
                  }}
                  onClear={() => {
                    setMenu(false);
                    handle()?.clear();
                    handle()?.focus();
                  }}
                  onClose={() => void close(current.id)}
                />
              </span>
            ) : null}
          </div>
        )}
        <div className="terminal-body">
          {finding && current && !phone ? (
            <FindBar
              onFind={find}
              onDone={() => {
                setFinding(false);
                handle()?.clearFind();
                handle()?.focus();
              }}
            />
          ) : null}
          {screens}
          {tabs.length === 0 && (listed.isFetched || listed.isError) ? empty : null}
          {notice}
        </div>
        {phone ? null : (
          <div className="terminal-foot">
            <Connection status={status} none={!current} />
            <span>
              <Trans>
                · no SSH port needed · opening it is on <Link to="/activity">Activity</Link>, what you type isn't
              </Trans>
            </span>
            {lost !== undefined ? (
              <span>
                <Trans>· {formatBytes(lost, i18n.locale)} it printed while you were away is gone</Trans>
              </span>
            ) : null}
            <span className="terminal-head-space" />
            {current && size ? (
              <span className="terminal-size">
                {size.columns} × {size.rows}
              </span>
            ) : null}
          </div>
        )}
      </section>
    </div>
  );
}

interface TabProps {
  name: string;
  selected: boolean;
  ended: boolean;
  onSelect: () => void;
  onClose: () => void;
}

/** One session's tab: its name, and a way to close it, as the mock has it. */
function TerminalTab({ name, selected, ended, onSelect, onClose }: TabProps) {
  const { t } = useLingui();
  const keys = (e: KeyboardEvent<HTMLButtonElement>) => {
    if (e.key === "Delete") {
      e.preventDefault();
      onClose();
    }
  };
  return (
    <button
      type="button"
      role="tab"
      aria-selected={selected}
      className="terminal-tab"
      onClick={onSelect}
      onKeyDown={keys}
      aria-keyshortcuts="Delete"
      aria-description={ended ? t`Ended` : undefined}
    >
      <span className="terminal-tab-icon">
        <Icon name="terminal" size={14} />
      </span>
      <span className="terminal-tab-name">{name}</span>
      <span
        className="terminal-tab-close"
        aria-hidden="true"
        title={t`Close ${name}`}
        onClick={(e) => {
          e.stopPropagation();
          onClose();
        }}
      >
        <Icon name="x" size={12} />
      </span>
    </button>
  );
}

/** How the attachment stands, as the foot says it. */
function Connection({ status, none }: { status: TerminalStatus | undefined; none: boolean }) {
  const { t } = useLingui();
  if (none) return <StatusBadge status="stopped" label={t`No terminal open`} labelIsState />;
  switch (status?.kind) {
    case "attached":
      return <StatusBadge status="healthy" label={t`Connected through the agent`} labelIsState />;
    case "reconnecting":
      return <StatusBadge status="deploying" label={t`Reconnecting`} labelIsState meta={t`what you type waits`} />;
    case "moved":
      return <StatusBadge status="stopped" label={t`Open somewhere else`} labelIsState />;
    case "ended":
      return <StatusBadge status="stopped" label={t`Ended`} labelIsState />;
    case "failed":
      return <StatusBadge status="down" label={t`Can't reach the terminal`} labelIsState />;
    default:
      return <StatusBadge status="deploying" label={t`Connecting through the agent`} labelIsState />;
  }
}

interface NoticeProps {
  status: TerminalStatus | undefined;
  onBringBack: () => void;
  onOpen: () => void;
  onClose: () => void;
  opening: boolean;
}

/** What the terminal on screen says over its output when it isn't there to type into. */
function Notice({ status, onBringBack, onOpen, onClose, opening }: NoticeProps) {
  const words = useEndWords();
  let body: ReactNode = null;
  switch (status?.kind) {
    case "moved":
      body = (
        <Callout
          tone="info"
          title={<Trans>This terminal is open somewhere else</Trans>}
          actions={
            <Button size="sm" onClick={onBringBack}>
              <Trans>Show it here</Trans>
            </Button>
          }
        >
          <Trans>Another tab or window attached to it, so what you type there goes to it. It goes on running.</Trans>
        </Callout>
      );
      break;
    case "ended": {
      const said = words(status.end);
      body = (
        <Callout
          tone={said.tone}
          title={said.title}
          actions={
            <>
              <Button size="sm" variant="primary" icon="terminal" loading={opening} onClick={onOpen}>
                <Trans>Open a new terminal</Trans>
              </Button>
              <Button size="sm" variant="ghost" onClick={onClose}>
                <Trans>Close this tab</Trans>
              </Button>
            </>
          }
        >
          {said.text}
        </Callout>
      );
      break;
    }
    case "failed":
      body = (
        <Callout
          tone="critical"
          title={<Trans>Can't reach this terminal</Trans>}
          actions={
            <Button size="sm" onClick={onBringBack}>
              <Trans>Try again</Trans>
            </Button>
          }
        >
          {sentence(status.error)}
        </Callout>
      );
      break;
  }
  return body ? <div className="terminal-notice">{body}</div> : null;
}

/** Finding text in what the terminal keeps, from the newest back. */
function FindBar({
  onFind,
  onDone,
}: {
  onFind: (text: string, backwards?: boolean) => boolean | undefined;
  onDone: () => void;
}) {
  const { t } = useLingui();
  const [text, setText] = useState("");
  const [missing, setMissing] = useState(false);
  const go = (backwards: boolean) => setMissing(text !== "" && !onFind(text, backwards));
  return (
    <search className="terminal-find" aria-label={t`Find in scrollback`}>
      <TextField
        aria-label={t`Find in scrollback`}
        size="sm"
        autoFocus
        value={text}
        placeholder={t`Find in scrollback`}
        invalid={missing}
        onChange={(e) => {
          setText(e.target.value);
          setMissing(e.target.value !== "" && !onFind(e.target.value));
        }}
        onKeyDown={(e) => {
          if (e.key === "Enter") {
            e.preventDefault();
            go(e.shiftKey);
          } else if (e.key === "Escape") {
            e.preventDefault();
            onDone();
          }
        }}
      />
      <Button
        variant="ghost"
        size="sm"
        icon="chevron-up"
        aria-label={t`Find the one before`}
        onClick={() => go(true)}
      />
      <Button
        variant="ghost"
        size="sm"
        icon="chevron-down"
        aria-label={t`Find the one after`}
        onClick={() => go(false)}
      />
      <Button variant="ghost" size="sm" icon="x" aria-label={t`Stop finding`} onClick={onDone} />
      {missing ? (
        <span className="terminal-find-none" role="status">
          <Trans>Not in what it keeps</Trans>
        </span>
      ) : null}
    </search>
  );
}

interface MenuProps {
  open: boolean;
  onOpenChange: (open: boolean) => void;
  onPaste: () => void;
  onClear: () => void;
  onClose: () => void;
}

/** The session's options, as the mock's more button has them. */
function SessionMenu({ open, onOpenChange, onPaste, onClear, onClose }: MenuProps) {
  const { t } = useLingui();
  const root = useRef<HTMLSpanElement>(null);
  useEffect(() => {
    if (!open) return;
    root.current?.querySelector<HTMLButtonElement>('[role="menuitem"]')?.focus();
    const away = (e: PointerEvent) => {
      if (!root.current?.contains(e.target as Node)) onOpenChange(false);
    };
    document.addEventListener("pointerdown", away);
    return () => document.removeEventListener("pointerdown", away);
  }, [open, onOpenChange]);
  const keys = (e: KeyboardEvent<HTMLDivElement>) => {
    const items = [...(root.current?.querySelectorAll<HTMLButtonElement>('[role="menuitem"]') ?? [])];
    const at = items.indexOf(document.activeElement as HTMLButtonElement);
    if (e.key === "Escape") {
      e.preventDefault();
      onOpenChange(false);
      root.current?.querySelector<HTMLButtonElement>("button")?.focus();
    } else if (e.key === "ArrowDown" || e.key === "ArrowUp") {
      e.preventDefault();
      const step = e.key === "ArrowDown" ? 1 : -1;
      items[(at + step + items.length) % items.length]?.focus();
    }
  };
  return (
    <span className="terminal-menu-anchor" ref={root}>
      <Button
        variant="ghost"
        size="sm"
        icon="more"
        aria-label={t`Session options`}
        aria-haspopup="menu"
        aria-expanded={open}
        onClick={() => onOpenChange(!open)}
      />
      {open ? (
        <div className="terminal-menu" role="menu" aria-label={t`Session options`} onKeyDown={keys}>
          <button type="button" role="menuitem" className="terminal-menu-item" onClick={onPaste}>
            <Trans>Paste</Trans>
          </button>
          <button type="button" role="menuitem" className="terminal-menu-item" onClick={onClear}>
            <Trans>Clear scrollback</Trans>
          </button>
          <button type="button" role="menuitem" className="terminal-menu-item terminal-menu-danger" onClick={onClose}>
            <Trans>Close terminal</Trans>
          </button>
        </div>
      ) : null}
    </span>
  );
}
