// SPDX-License-Identifier: AGPL-3.0-only

import { create } from "@bufbuild/protobuf";
import { createClient } from "@connectrpc/connect";
import { createConnectQueryKey, useQuery, useTransport } from "@connectrpc/connect-query";
import {
  FileService,
  GetTerminalPreferencesResponseSchema,
  type ListTerminalsResponse,
  type Server,
  StepUpAction,
  type Terminal,
  type TerminalPreferences,
  TerminalPreferencesSchema,
  TerminalService,
} from "@dockwell/api";
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
import { Link, useNavigate } from "@tanstack/react-router";
import {
  type CSSProperties,
  type KeyboardEvent,
  type ReactNode,
  useCallback,
  useEffect,
  useLayoutEffect,
  useMemo,
  useRef,
  useState,
} from "react";

import { reasonOf, sentence } from "../../lib/errors";
import { formatBytes } from "../../lib/format";
import { PasskeyError } from "../../lib/webauthn";
import { AddPasskeyHere } from "../auth/AddPasskeyHere";
import { useMay } from "../auth/may";
import { useAuthError } from "../auth/messages";
import { StepUpDeclined, withStepUpIfAsked } from "../auth/stepUp";
import { ServerFrame } from "../servers/ServerFrame";
import { EmptyState } from "../shell/EmptyState";
import { KeyBar } from "./KeyBar";
import { NewSessionMenu } from "./NewSessionMenu";
import { PHONE_FONT, PHONE_SIZES, resolveDisplaySettings } from "./preferences";
import { SavedCommandsPanel } from "./SavedCommandsPanel";
import { TerminalSettingsControl } from "./SettingsPopover";
import { commandLineCount, hasDisallowedControlChars, normalizeCommandBody } from "./secrets";
import type { TerminalStatus } from "./session";
import { type MirroredInput, type ScreenHandle, TerminalScreen } from "./TerminalScreen";
import { useEndWords } from "./words";
import "./TerminalPage.css";

const TEXT_SIZE_KEY = "dockwell.terminal.textSize";
const NARROW_ASIDE_QUERY = "(max-width: 1099.98px)";

/**
 * How tall the screen a page has is, less a phone's keyboard where it shows: what the
 * terminal and its keys fit in, so the keys sit right above the keyboard.
 */
function useVisibleHeight(): number | undefined {
  const [height, setHeight] = useState(() => window.visualViewport?.height);
  useEffect(() => {
    const view = window.visualViewport;
    if (!view) return;
    const measure = () => setHeight(view.height);
    view.addEventListener("resize", measure);
    return () => view.removeEventListener("resize", measure);
  }, []);
  return height;
}

/** Which shell the page is for, from its URL. */
export interface TerminalSearch {
  /** A container on the server, by name, for a shell in it. */
  container?: string;
  containerId?: string;
  containerTitle?: string;
  /** Open the container's database client (psql, mysql, mariadb, redis-cli, valkey-cli, mongosh) directly. */
  console?: boolean;
  /** The logical database to connect to when console is true. */
  database?: string;
}

interface ConsoleOptions {
  databaseConsole: true;
  database?: string | undefined;
}

interface ExactTerminal {
  id: string;
  name: string;
  argv?: string[];
}

interface TabDisplay {
  /** What the tab is: "Host" (or "Host (2)") or the container name (with count if duplicate). */
  what: string;
  /** The server name. */
  server: string;
  /** Full accessible label combining what and server. */
  full: string;
  /** Tooltip with prompt title and server. */
  hint: string;
  isContainer: boolean;
}

/** A server's terminal, as its own tab of the server's page, and the shells in its containers. */
export function TerminalPage({ serverId, search = {} }: { serverId: string; search?: TerminalSearch }) {
  const phone = useMediaQuery(PHONE_QUERY);
  const { t } = useLingui();
  const [title, setTitle] = useState<string>();
  const key = search.containerId
    ? `${search.containerId}:${search.console ? "console" : "shell"}:${search.database ?? ""}`
    : search.console
      ? `console:${search.container ?? ""}:${search.database ?? ""}`
      : "terminal";
  return (
    <ServerFrame
      id={serverId}
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
      {(server) => (
        <Terminals
          key={key}
          server={server}
          phone={phone}
          container={search.container}
          consoleMode={Boolean(search.console)}
          database={search.database}
          exact={
            search.containerId
              ? { id: search.containerId, name: search.containerTitle || search.containerId.slice(0, 12) }
              : undefined
          }
          onTitle={setTitle}
        />
      )}
    </ServerFrame>
  );
}

/** Builds each tab's display labels: host or container name, numbered if duplicate, plus its server. */
function buildTabDisplays(tabs: readonly Terminal[], serverName: string, hostWord: string): Map<string, TabDisplay> {
  const seen = new Map<string, number>();
  const out = new Map<string, TabDisplay>();
  for (const tab of tabs) {
    const isContainer = Boolean(tab.container);
    const base = isContainer ? tab.container : hostWord;
    const n = (seen.get(base) ?? 0) + 1;
    seen.set(base, n);
    const what = n === 1 ? base : `${base} (${n})`;
    const full = `${what} · ${serverName}`;
    const hint = tab.title ? `${tab.title} · ${serverName}` : full;
    out.set(tab.id, { what, server: serverName, full, hint, isContainer });
  }
  return out;
}

interface TerminalsProps {
  server: Server;
  phone: boolean;
  /** A container a shell is wanted in, from the page's URL. */
  container: string | undefined;
  /** True when the URL asked to open a database console straight away. */
  consoleMode?: boolean;
  /** The logical database inside container when consoleMode is true. */
  database?: string | undefined;
  exact?: ExactTerminal | undefined;
  onTitle: (title: string | undefined) => void;
}

/** Whether a terminal tab's title is a database client opened by Dockwell. */
const CONSOLE_TITLE = /^(psql|mysql|mariadb|redis-cli|valkey-cli|mongosh)(?:\/([^@]+))?@/;

function matchesConsole(tab: Terminal, container: string, database: string | undefined): boolean {
  if (tab.container !== container) return false;
  const match = CONSOLE_TITLE.exec(tab.title);
  if (!match) return false;
  return database ? match[2] === database : !match[2];
}

function Terminals({ server, phone, container, consoleMode = false, database, exact, onTitle }: TerminalsProps) {
  const { t, i18n } = useLingui();
  const transport = useTransport();
  const queryClient = useQueryClient();
  const navigate = useNavigate();
  const theme = useResolvedTheme();
  const may = useMay();
  const explain = useAuthError();
  const narrowAside = useMediaQuery(NARROW_ASIDE_QUERY);
  const client = useMemo(() => createClient(TerminalService, transport), [transport]);
  const serverId = server.id;
  const canOpenFiles = may(FileService.method.listFiles).allowed;

  // What's open is this page's to know once it has looked: it opens and closes them itself, so
  // nothing reads it again while the page is up. Each time the page comes back it asks again,
  // as what it kept may have ended while it was away (#652).
  const listed = useQuery(
    TerminalService.method.listTerminals,
    { serverId },
    { staleTime: Number.POSITIVE_INFINITY, refetchOnWindowFocus: false, refetchOnMount: "always" },
  );
  const prefsQuery = useQuery(TerminalService.method.getTerminalPreferences, {});
  const [optimisticPrefs, setOptimisticPrefs] = useState<TerminalPreferences>();
  const [savingPrefs, setSavingPrefs] = useState(false);
  const [prefsError, setPrefsError] = useState<string>();
  const prefsSaveSeq = useRef(0);
  const prefsSaveChain = useRef<Promise<void>>(Promise.resolve());
  const activePrefs = optimisticPrefs ?? prefsQuery.data?.preferences;

  const [tabs, setTabs] = useState<Terminal[]>([]);
  const [active, setActive] = useState<string>();
  const [statuses, setStatuses] = useState<Record<string, TerminalStatus>>({});
  const [size, setSize] = useState<{ columns: number; rows: number }>();
  const [selection, setSelection] = useState(false);
  const [opening, setOpening] = useState(false);
  const [openingFiles, setOpeningFiles] = useState(false);
  const [problem, setProblem] = useState<string>();
  // A terminal took a passkey and there's none for this address: for which shell.
  const [noPasskey, setNoPasskey] = useState<{
    container?: string;
    exact?: ExactTerminal;
    consoleOpts?: ConsoleOptions;
  }>();
  // A container's shell that the agent found as good as root, waiting for the person
  // to go on to the passkey, or not, once they've read why.
  const [asking, setAsking] = useState<{ container: string; go: (yes: boolean) => void }>();
  const [lost, setLost] = useState<bigint>();
  const [finding, setFinding] = useState(false);
  const [menu, setMenu] = useState(false);
  const [newMenu, setNewMenu] = useState(false);
  const [settingsOpen, setSettingsOpen] = useState(false);
  const [commandsOpen, setCommandsOpen] = useState(false);
  const [fullscreen, setFullscreen] = useState(false);
  const [broadcast, setBroadcast] = useState(false);
  const [broadcastIds, setBroadcastIds] = useState<Set<string>>(() => new Set());

  const handles = useRef(new Map<string, ScreenHandle>());
  const oscCwds = useRef(new Map<string, string>());
  const [ctrl, setCtrl] = useState(false);
  const visible = useVisibleHeight();
  // On a phone the server's tabs stay above it, so the way to its other pages is a tap
  // away (#795): it fills what's below where it starts.
  const layout = useRef<HTMLDivElement>(null);
  const [top, setTop] = useState<number>();
  const measure = useCallback(() => {
    const el = layout.current;
    if (el) setTop(Math.round(el.getBoundingClientRect().top + window.scrollY));
  }, []);
  // After each render, as a note above it may come or go; the same number renders nothing again.
  useLayoutEffect(() => {
    if (phone && !fullscreen) measure();
  });
  useEffect(() => {
    if (!phone) return;
    window.addEventListener("resize", measure);
    return () => window.removeEventListener("resize", measure);
  }, [phone, measure]);
  const merged = useRef(false);
  // Each container tab's own exact target so reopening an ended container shell opens that container.
  const tabTargets = useRef(new Map<string, ExactTerminal>());
  // Tabs opened specifically for the URL's exact container (not other containers opened from +).
  const urlExactTabs = useRef(new Set<string>());
  const autoOpened = useRef(false);
  const consoleTabs = useRef(new Map<string, ConsoleOptions>());

  /** Opens the server's own terminal, a shell in a container, or a database console in it. */
  const open = useCallback(
    async (inContainer?: string, targetContainer?: ExactTerminal, consoleOpts?: ConsoleOptions) => {
      setOpening(true);
      setProblem(undefined);
      setNoPasskey(undefined);
      const want = size ?? { columns: 80, rows: 24 };
      // A container's shell asks for a passkey only where the agent finds it as good as root.
      const ref = targetContainer?.id ?? inContainer;
      const name = targetContainer?.name ?? inContainer;
      const target = ref ? `${serverId}:${ref}` : serverId;
      try {
        const res = await withStepUpIfAsked(
          transport,
          StepUpAction.OPEN_TERMINAL,
          target,
          (proof) =>
            client.openTerminal({
              serverId,
              columns: want.columns,
              rows: want.rows,
              proof,
              container: inContainer ?? "",
              ...(consoleOpts ? { databaseConsole: true, database: consoleOpts.database ?? "" } : {}),
              ...(targetContainer ? { containerId: targetContainer.id, argv: targetContainer.argv ?? [] } : {}),
            }),
          // Only the agent can tell a container reaches as far as root, so the page says
          // that before the browser asks. A server's terminal said so in its offer.
          name
            ? () =>
                new Promise<boolean>((resolve) =>
                  setAsking({
                    container: name,
                    go: (yes) => {
                      setAsking(undefined);
                      resolve(yes);
                    },
                  }),
                )
            : undefined,
        );
        const opened = res.terminal;
        if (opened) {
          if (targetContainer) {
            tabTargets.current.set(opened.id, targetContainer);
            if (exact && targetContainer.id === exact.id) {
              urlExactTabs.current.add(opened.id);
            }
          }
          if (consoleOpts) consoleTabs.current.set(opened.id, consoleOpts);
          setTabs((now) => [...now, opened]);
          setActive(opened.id);
        }
        void queryClient.invalidateQueries({
          queryKey: createConnectQueryKey({ schema: TerminalService.method.listTerminals, cardinality: "finite" }),
        });
      } catch (err) {
        const reason = reasonOf(err);
        if (err instanceof StepUpDeclined) {
          // They read why, and chose not to: nothing to say.
        } else if (reason === "no_passkey_here") {
          // Adding one here opens what was asked for once it's made.
          setNoPasskey(
            targetContainer
              ? { container: targetContainer.name, exact: targetContainer, ...(consoleOpts ? { consoleOpts } : {}) }
              : { container: inContainer, ...(consoleOpts ? { consoleOpts } : {}) },
          );
        } else if (err instanceof PasskeyError && err.problem === "cancelled") {
          setProblem(t`The passkey prompt closed, so no terminal opened. Open one when you're ready.`);
        } else {
          setProblem(err instanceof PasskeyError ? explain(err) : sentence(err));
        }
      } finally {
        setOpening(false);
      }
    },
    [client, exact, explain, queryClient, serverId, size, t, transport],
  );

  // The tabs come back from what the control plane lists once it has answered since the page
  // came back, not from what was kept before: a terminal that ended since isn't listed (#652).
  // Where it can't answer, what was kept stands, which closing and ending keep current.
  useEffect(() => {
    if (merged.current || !listed.data || !listed.isFetchedAfterMount) return;
    merged.current = true;
    const existing = listed.data.terminals;
    setTabs((now) => [...existing.filter((o) => !now.some((n) => n.id === o.id)), ...now]);
    if (consoleMode && (exact || container)) {
      const targetName = exact?.name ?? container ?? "";
      const existingConsole = targetName
        ? existing.findLast((o) => matchesConsole(o, targetName, database))
        : undefined;
      if (existingConsole) {
        consoleTabs.current.set(existingConsole.id, { databaseConsole: true, database });
        setActive((now) => now ?? existingConsole.id);
        autoOpened.current = true;
        return;
      }
      if (!autoOpened.current) {
        autoOpened.current = true;
        void open(exact ? undefined : container, exact, { databaseConsole: true, database });
        return;
      }
    }
    // A shell asked for in a container shows that container's, or the offer to open one.
    setActive(
      (now) =>
        now ??
        (exact ? undefined : container ? existing.findLast((o) => o.container === container)?.id : existing.at(-1)?.id),
    );
  }, [listed.data, listed.isFetchedAfterMount, container, exact, consoleMode, database, open]);

  // Lock page scrolling while fullscreen is active. Shift+Esc leaves fullscreen from anywhere;
  // bare Esc leaves only when focus is outside the terminal so Esc always reaches the shell.
  useEffect(() => {
    if (!fullscreen) return;
    const prevOverflow = document.body.style.overflow;
    document.body.style.overflow = "hidden";
    const onKeyDown = (e: globalThis.KeyboardEvent) => {
      if (e.key !== "Escape") return;
      if (finding || menu || newMenu || settingsOpen || commandsOpen) return;
      const isShiftEsc = e.shiftKey && !e.ctrlKey && !e.altKey && !e.metaKey;
      if (isShiftEsc) {
        e.preventDefault();
        setFullscreen(false);
        return;
      }
      if (e.ctrlKey || e.altKey || e.metaKey) return;
      const target = e.target instanceof Element ? e.target : null;
      if (target?.closest(".terminal-screen")) return;
      e.preventDefault();
      setFullscreen(false);
    };
    window.addEventListener("keydown", onKeyDown);
    return () => {
      document.body.style.overflow = prevOverflow;
      window.removeEventListener("keydown", onKeyDown);
    };
  }, [fullscreen, finding, menu, newMenu, settingsOpen, commandsOpen]);

  const liveTabs = useMemo(
    () => tabs.filter((tab) => statuses[tab.id]?.kind !== "ended" && statuses[tab.id]?.kind !== "moved"),
    [tabs, statuses],
  );
  const liveSignature = useMemo(() => liveTabs.map((tab) => tab.id).join(","), [liveTabs]);
  const prevLiveSignature = useRef(liveSignature);

  // Safety invariant: turn broadcast off whenever the set of live tabs changes (opened, closed, ended or moved).
  useEffect(() => {
    if (prevLiveSignature.current === liveSignature) return;
    prevLiveSignature.current = liveSignature;
    setBroadcast(false);
    setBroadcastIds(new Set());
  }, [liveSignature]);

  /**
   * Takes a terminal out of the list kept for this server, so the page doesn't bring it back
   * when it comes back, and asks again for what's open, as opening one does (#652).
   */
  const unlist = useCallback(
    (id: string) => {
      const key = createConnectQueryKey({
        schema: TerminalService.method.listTerminals,
        input: { serverId },
        cardinality: "finite",
      });
      queryClient.setQueriesData<ListTerminalsResponse>({ queryKey: key }, (kept) =>
        kept ? { ...kept, terminals: kept.terminals.filter((term) => term.id !== id) } : kept,
      );
      void queryClient.invalidateQueries({ queryKey: key });
    },
    [queryClient, serverId],
  );

  const displays = useMemo(() => buildTabDisplays(tabs, server.name, t`Host`), [tabs, server.name, t]);
  // A container in the page's address with no shell or console open in it yet.
  const wanted = exact
    ? tabs.some((tab) => urlExactTabs.current.has(tab.id))
      ? undefined
      : exact.name
    : container &&
        !tabs.some((tab) => (consoleMode ? matchesConsole(tab, container, database) : tab.container === container))
      ? container
      : undefined;
  const current = tabs.find((tab) => tab.id === active);
  const currentDisplay = current ? displays.get(current.id) : undefined;
  const status = current ? statuses[current.id] : undefined;
  useEffect(() => onTitle(currentDisplay?.full ?? current?.title), [currentDisplay?.full, current?.title, onTitle]);

  const handle = () => (active ? handles.current.get(active) : undefined);

  // Serialize and coalesce rapid preference changes so responses never arrive out of order.
  const savePreferences = useCallback(
    (next: TerminalPreferences) => {
      const seq = ++prefsSaveSeq.current;
      setOptimisticPrefs(next);
      setSavingPrefs(true);
      setPrefsError(undefined);
      try {
        localStorage.removeItem(TEXT_SIZE_KEY);
      } catch {
        // Ignore storage errors.
      }
      prefsSaveChain.current = prefsSaveChain.current.then(async () => {
        if (seq !== prefsSaveSeq.current) return;
        try {
          const res = await client.updateTerminalPreferences({ preferences: next });
          if (seq !== prefsSaveSeq.current) return;
          const prefsKey = createConnectQueryKey({
            schema: TerminalService.method.getTerminalPreferences,
            input: {},
            cardinality: "finite",
          });
          queryClient.setQueryData(
            prefsKey,
            create(GetTerminalPreferencesResponseSchema, { preferences: res.preferences }),
          );
          setOptimisticPrefs(res.preferences);
        } catch (err) {
          if (seq === prefsSaveSeq.current) {
            setPrefsError(sentence(err));
          }
        } finally {
          if (seq === prefsSaveSeq.current) {
            setSavingPrefs(false);
          }
        }
      });
      return prefsSaveChain.current;
    },
    [client, queryClient],
  );

  const resetPreferences = useCallback(() => {
    void savePreferences(create(TerminalPreferencesSchema, {}));
  }, [savePreferences]);

  const forget = (id: string) => {
    handles.current.delete(id);
    oscCwds.current.delete(id);
    tabTargets.current.delete(id);
    urlExactTabs.current.delete(id);
    consoleTabs.current.delete(id);
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
    unlist(id);
    forget(id);
  };

  const onStatus = useCallback(
    (id: string, next: TerminalStatus) => {
      setStatuses((now) => ({ ...now, [id]: next }));
      if (next.kind === "attached") setLost(undefined);
      // One that ended on its own stays as a tab until it's closed, but isn't open to come back to.
      if (next.kind === "ended") unlist(id);
    },
    [unlist],
  );

  const display = useMemo(() => resolveDisplaySettings(activePrefs, phone), [activePrefs, phone]);

  // Phone Aa cycles sizes through the same user preference as Settings so both always agree.
  const nextTextSize = () => {
    const idx = (PHONE_SIZES as readonly number[]).indexOf(display.fontSize);
    const next = PHONE_SIZES[(idx + 1) % PHONE_SIZES.length] ?? PHONE_FONT;
    const updated = create(TerminalPreferencesSchema, {
      fontSize: next === PHONE_FONT ? 0 : next,
      fontFamily: activePrefs?.fontFamily ?? "",
      lineHeight: activePrefs?.lineHeight ?? 0,
      cursorStyle: activePrefs?.cursorStyle ?? display.cursorStyleEnum,
      cursorBlink: activePrefs?.cursorBlink ?? display.cursorBlink,
      scrollback: activePrefs?.scrollback ?? 0,
    });
    void savePreferences(updated);
    handle()?.focus();
  };

  const selectTab = (tabId: string) => {
    // Switching to a tab that isn't opted into broadcast turns broadcast off so input never goes somewhere unexpected.
    if (broadcast && !broadcastIds.has(tabId)) {
      setBroadcast(false);
      setBroadcastIds(new Set());
    }
    setActive(tabId);
  };

  const toggleBroadcast = () => {
    if (broadcast) {
      setBroadcast(false);
      setBroadcastIds(new Set());
      return;
    }
    if (liveTabs.length < 2) return;
    setBroadcast(true);
    // Opt in only the active tab at first; every additional tab must be opted in explicitly.
    setBroadcastIds(new Set(active && liveTabs.some((tab) => tab.id === active) ? [active] : []));
  };

  const toggleBroadcastTab = (tabId: string) => {
    setBroadcastIds((prev) => {
      const next = new Set(prev);
      if (next.has(tabId)) {
        next.delete(tabId);
      } else {
        next.add(tabId);
      }
      return next;
    });
  };

  const canTypeCurrent = Boolean(current && (status?.kind === "attached" || status?.kind === "reconnecting"));
  const broadcastTargets = useMemo(() => liveTabs.filter((tab) => broadcastIds.has(tab.id)), [liveTabs, broadcastIds]);
  // One source of truth for both the Saved Commands target label and where Run/Insert sends.
  const commandTargets = useMemo(() => {
    if (broadcast) return broadcastTargets;
    return current && canTypeCurrent ? [current] : [];
  }, [broadcast, broadcastTargets, current, canTypeCurrent]);

  const commandTargetLabel = useMemo(() => {
    if (commandTargets.length === 0) return undefined;
    if (commandTargets.length === 1) {
      const only = commandTargets[0];
      return only ? (displays.get(only.id)?.full ?? only.title) : undefined;
    }
    const names = commandTargets.map((tab) => displays.get(tab.id)?.what ?? tab.title).join(", ");
    return t`${commandTargets.length} tabs: ${names}`;
  }, [commandTargets, displays, t]);

  /** Mirrors input from the active terminal to all other opted-in broadcast targets using each target's own modes. */
  const mirrorInput = useCallback(
    (sourceId: string, input: MirroredInput) => {
      if (!broadcast || !broadcastIds.has(sourceId) || broadcastTargets.length < 2) return;
      for (const target of broadcastTargets) {
        if (target.id === sourceId) continue;
        handles.current.get(target.id)?.receiveMirrored(input);
      }
    },
    [broadcast, broadcastIds, broadcastTargets],
  );

  /** Types a saved command into the exact target tabs named in the panel, with Enter only when run=true. */
  const sendCommand = useCallback(
    (rawCommand: string, run: boolean): string | undefined => {
      const cleaned = normalizeCommandBody(rawCommand);
      if (!cleaned) return undefined;
      if (hasDisallowedControlChars(cleaned, true)) {
        return t`This command contains disallowed control characters and wasn't sent.`;
      }
      if (commandTargets.length === 0) {
        return broadcast ? t`Select at least one tab to send this command to.` : t`Open a terminal session first.`;
      }
      const multiline = cleaned.includes("\n");
      if (!run && multiline) {
        const unsupported = commandTargets.filter((tab) => !handles.current.get(tab.id)?.hasBracketedPaste());
        if (unsupported.length > 0) {
          const lines = commandLineCount(cleaned);
          if (unsupported.length === 1 && commandTargets.length === 1) {
            return t`Can't safely insert this ${lines}-line command because this shell doesn't have bracketed paste enabled. Use Run to confirm and run all ${lines} lines.`;
          }
          const names = unsupported.map((tab) => displays.get(tab.id)?.what ?? tab.title).join(", ");
          return t`Can't safely insert this ${lines}-line command because bracketed paste isn't enabled in ${names}. Use Run to confirm and run all ${lines} lines.`;
        }
      }
      for (const tab of commandTargets) {
        const targetHandle = handles.current.get(tab.id);
        targetHandle?.pasteDirect(cleaned);
        if (run) targetHandle?.sendRaw("\r");
      }
      if (active && commandTargets.some((tab) => tab.id === active)) {
        handles.current.get(active)?.focus();
      }
      return undefined;
    },
    [active, broadcast, commandTargets, displays, t],
  );

  /** Opens Files at a host shell's working directory (from /proc, then OSC 7, then home, else Files' default). */
  const openFilesHere = useCallback(async () => {
    if (!current || current.container || openingFiles) return;
    setOpeningFiles(true);
    let target: string | undefined;
    try {
      const res = await client.getTerminalWorkingDirectory({ terminalId: current.id });
      if (res.known && res.path.startsWith("/")) {
        target = res.path;
      } else if (oscCwds.current.get(current.id)?.startsWith("/")) {
        target = oscCwds.current.get(current.id);
      } else if (res.home.startsWith("/")) {
        target = res.home;
      }
    } catch {
      const osc = oscCwds.current.get(current.id);
      if (osc?.startsWith("/")) target = osc;
    } finally {
      setOpeningFiles(false);
    }
    setFullscreen(false);
    void navigate({
      to: "/servers/$serverId/files",
      params: { serverId },
      search: target ? { path: target } : {},
    });
  }, [client, current, navigate, openingFiles, serverId]);

  const filesDisabledForContainer = Boolean(current?.container);
  const filesTitle = current?.container
    ? t`Files shows ${server.name}, not files inside ${current.container}`
    : t`Open Files at this shell's folder`;

  const screens = tabs.map((tab) => {
    const info = displays.get(tab.id);
    const isActive = tab.id === active;
    const isBroadcastTarget = broadcast && broadcastIds.has(tab.id);
    return (
      <TerminalScreen
        key={tab.id}
        ref={(h) => {
          if (h) handles.current.set(tab.id, h);
        }}
        terminal={tab}
        transport={transport}
        active={isActive}
        attached={isActive || isBroadcastTarget}
        theme={theme}
        fontSize={display.fontSize}
        fontFamily={display.fontFamily}
        lineHeight={display.lineHeightSetting}
        cursorStyle={display.cursorStyle}
        cursorBlink={display.cursorBlink}
        scrollback={display.scrollback}
        fullscreen={fullscreen}
        onExitFullscreen={() => setFullscreen(false)}
        onInput={(input) => mirrorInput(tab.id, input)}
        onOscCwd={(path) => {
          if (!tab.container) oscCwds.current.set(tab.id, path);
        }}
        label={t`${info?.full ?? tab.title} output`}
        onStatus={(s) => onStatus(tab.id, s)}
        onSize={(columns, rows) => isActive && setSize({ columns, rows })}
        onSelection={(has) => isActive && setSelection(has)}
        onLost={(bytes) => isActive && setLost(bytes)}
        onCtrlUsed={() => setCtrl(false)}
      />
    );
  });

  const notice = current ? (
    <Notice
      status={status}
      root={!current.container || current.title.startsWith("root@")}
      onBringBack={() => handle()?.bringBack()}
      onOpen={() => {
        const savedTarget = tabTargets.current.get(current.id);
        const consoleOpts = consoleTabs.current.get(current.id);
        void (savedTarget
          ? open(undefined, savedTarget, consoleOpts)
          : open(current.container || undefined, undefined, consoleOpts));
      }}
      onClose={() => void close(current.id)}
      opening={opening}
    />
  ) : null;

  const containerEmpty = wanted ? (
    <EmptyState
      icon="terminal"
      title={
        consoleMode ? (
          <Trans>
            No console open in <code>{wanted}</code>
          </Trans>
        ) : (
          <Trans>
            No shell open in <code>{wanted}</code>
          </Trans>
        )
      }
      action={
        <Button
          variant="primary"
          size="lg"
          icon="terminal"
          loading={opening}
          onClick={() =>
            void (exact
              ? open(undefined, exact, consoleMode ? { databaseConsole: true, database } : undefined)
              : open(wanted, undefined, consoleMode ? { databaseConsole: true, database } : undefined))
          }
        >
          {consoleMode ? <Trans>Open console</Trans> : <Trans>Open a shell</Trans>}
        </Button>
      }
    >
      {consoleMode ? (
        <p>
          <Trans>
            Opens the database client inside <code>{wanted}</code> as its admin user, signing in from the container's
            environment.
          </Trans>
        </p>
      ) : exact ? (
        <ExecForm opening={opening} run={(argv) => void open(undefined, { ...exact, argv })} />
      ) : (
        <p>
          <Trans>
            A shell in <code>{wanted}</code> runs in the container, as its own user, with the first of bash, ash and sh
            its image has. One in a container that reaches as far as root on <code>{server.name}</code> asks for your
            passkey.
          </Trans>
        </p>
      )}
    </EmptyState>
  ) : null;

  const empty = (
    <EmptyState
      icon="terminal"
      title={<Trans>No terminal open</Trans>}
      action={
        <Button variant="primary" size="lg" icon="terminal" loading={opening} onClick={() => void open()}>
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

  const askingCallout = asking ? (
    <Callout
      tone="warning"
      title={<Trans>This shell takes your passkey</Trans>}
      actions={
        <>
          <Button size="sm" variant="primary" icon="key" onClick={() => asking.go(true)}>
            <Trans>Confirm with your passkey</Trans>
          </Button>
          <Button size="sm" variant="ghost" onClick={() => asking.go(false)}>
            <Trans>Cancel</Trans>
          </Button>
        </>
      }
    >
      <Trans>
        <code>{asking.container}</code> reaches as far as root on <code>{server.name}</code>: it's privileged, holds
        Docker's socket, shares the server's network, or mounts a system path, through a volume or a link too. A shell
        in it is as good as root there, so it takes your passkey.
      </Trans>
    </Callout>
  ) : null;

  const shellIn = noPasskey?.container ?? "";
  const problemCallout = noPasskey ? (
    <AddPasskeyHere
      title={
        noPasskey.container
          ? t`A shell in ${shellIn} takes a passkey, and there's none for this address`
          : t`A terminal takes a passkey, and there's none for this address`
      }
      onAdded={() =>
        void (noPasskey.exact
          ? open(undefined, noPasskey.exact, noPasskey.consoleOpts)
          : open(noPasskey.container, undefined, noPasskey.consoleOpts))
      }
    />
  ) : problem ? (
    <Callout tone="critical" title={<Trans>No terminal opened</Trans>}>
      {problem}
    </Callout>
  ) : null;

  const find = (text: string, backwards = false) => handle()?.find(text, backwards);
  const commandsAsSheet = phone || narrowAside || fullscreen;
  const activeInBroadcast = Boolean(active && broadcastIds.has(active));

  const broadcastBar = broadcast ? (
    <section className="terminal-broadcast-bar" aria-label={t`Type into several tabs`}>
      <div className="terminal-broadcast-head">
        <span
          className={
            broadcastTargets.length > 1 && activeInBroadcast
              ? "terminal-broadcast-state is-live"
              : "terminal-broadcast-state"
          }
        >
          {broadcastTargets.length > 1 && activeInBroadcast ? (
            <Trans>Typing into {broadcastTargets.length} tabs</Trans>
          ) : (
            <Trans>Select tabs to type into at once</Trans>
          )}
        </span>
        <Button
          variant="ghost"
          size="sm"
          onClick={() => {
            setBroadcast(false);
            setBroadcastIds(new Set());
          }}
        >
          <Trans>Turn off</Trans>
        </Button>
      </div>
      <div className="terminal-broadcast-choices">
        {liveTabs.map((tab) => {
          const info = displays.get(tab.id);
          const checked = broadcastIds.has(tab.id);
          return (
            <label key={tab.id} className={checked ? "terminal-broadcast-chip is-selected" : "terminal-broadcast-chip"}>
              <input type="checkbox" checked={checked} onChange={() => toggleBroadcastTab(tab.id)} />
              <Icon name={info?.isContainer ? "service" : "server"} size={12} />
              <span>{info?.what ?? tab.title}</span>
            </label>
          );
        })}
      </div>
    </section>
  ) : null;

  // One tree for both layouts, so turning a phone or narrowing a window keeps each
  // terminal where it is, with its scrollback and what's typed that hasn't gone.
  return (
    <div
      ref={layout}
      className={["terminal-layout", phone ? "terminal-phone" : "", fullscreen ? "terminal-fullscreen" : ""]
        .filter(Boolean)
        .join(" ")}
      style={
        phone && !fullscreen
          ? ({
              ...(visible ? { "--visible-height": `${visible}px` } : {}),
              ...(top !== undefined ? { "--terminal-top": `${top}px` } : {}),
            } as CSSProperties)
          : undefined
      }
    >
      {askingCallout}
      {problemCallout}
      <div className="terminal-workspace">
        <section className={phone ? "terminal-panel" : "dw-panel terminal-panel"} aria-label={t`Terminal sessions`}>
          {phone ? (
            <div className="terminal-phone-bar">
              <NewSessionMenu
                server={server}
                open={newMenu}
                opening={opening}
                align="start"
                onOpenChange={setNewMenu}
                onOpenHost={() => void open()}
                onOpenContainer={(target) => void open(undefined, target)}
              />
              {tabs.length > 1 ? (
                <select
                  className="terminal-phone-select"
                  aria-label={t`Switch terminal session`}
                  value={active ?? ""}
                  onChange={(e) => selectTab(e.target.value)}
                >
                  {tabs.map((tab) => {
                    const info = displays.get(tab.id);
                    return (
                      <option key={tab.id} value={tab.id}>
                        {info?.full ?? tab.title}
                      </option>
                    );
                  })}
                </select>
              ) : (
                <span className="terminal-phone-current">{currentDisplay?.what ?? ""}</span>
              )}
              <span className="terminal-head-space" />
              {liveTabs.length >= 2 ? (
                <Button
                  variant={broadcast ? "secondary" : "ghost"}
                  size="sm"
                  icon="layers"
                  aria-label={t`Type into several tabs`}
                  aria-pressed={broadcast}
                  onClick={toggleBroadcast}
                />
              ) : null}
              <Button
                variant="ghost"
                size="sm"
                icon="terminal"
                aria-label={t`Saved commands`}
                aria-pressed={commandsOpen}
                onClick={() => setCommandsOpen((was) => !was)}
              />
              {current && canOpenFiles ? (
                <Button
                  variant="ghost"
                  size="sm"
                  icon="folder"
                  aria-label={t`Open Files here`}
                  title={filesTitle}
                  disabled={filesDisabledForContainer}
                  loading={openingFiles}
                  onClick={() => void openFilesHere()}
                />
              ) : null}
              <TerminalSettingsControl
                open={settingsOpen}
                phone
                resolved={display}
                raw={activePrefs}
                saving={savingPrefs}
                error={prefsError}
                onOpenChange={setSettingsOpen}
                onChange={(next) => void savePreferences(next)}
                onReset={resetPreferences}
              />
            </div>
          ) : (
            <div className="terminal-head">
              {tabs.length > 0 ? (
                <div role="tablist" aria-label={t`Sessions`} className="terminal-tabs">
                  {tabs.map((tab) => {
                    const info = displays.get(tab.id);
                    return (
                      <TerminalTab
                        key={tab.id}
                        what={info?.what ?? tab.title}
                        serverName={info?.server ?? server.name}
                        full={info?.full ?? tab.title}
                        hint={info?.hint ?? tab.title}
                        isContainer={info?.isContainer ?? Boolean(tab.container)}
                        selected={tab.id === active}
                        broadcasting={broadcast && broadcastIds.has(tab.id)}
                        ended={statuses[tab.id]?.kind === "ended"}
                        onSelect={() => selectTab(tab.id)}
                        onClose={() => void close(tab.id)}
                      />
                    );
                  })}
                </div>
              ) : null}
              <span className="terminal-head-tool">
                <NewSessionMenu
                  server={server}
                  open={newMenu}
                  opening={opening}
                  align="start"
                  onOpenChange={setNewMenu}
                  onOpenHost={() => void open()}
                  onOpenContainer={(target) => void open(undefined, target)}
                />
              </span>
              <span className="terminal-head-space" />
              <span className="terminal-head-tool terminal-tools">
                {liveTabs.length >= 2 ? (
                  <Button
                    variant={broadcast ? "secondary" : "ghost"}
                    size="sm"
                    icon="layers"
                    aria-label={t`Type into several tabs`}
                    aria-pressed={broadcast}
                    title={t`Send what you type to selected tabs`}
                    onClick={toggleBroadcast}
                  >
                    {broadcast && broadcastTargets.length > 0 ? (
                      <Trans>Type into several ({broadcastTargets.length})</Trans>
                    ) : (
                      <Trans>Type into several</Trans>
                    )}
                  </Button>
                ) : null}
                <Button
                  variant={commandsOpen ? "secondary" : "ghost"}
                  size="sm"
                  aria-pressed={commandsOpen}
                  onClick={() => setCommandsOpen((was) => !was)}
                >
                  <Trans>Commands</Trans>
                </Button>
                {current && canOpenFiles ? (
                  <Button
                    variant="ghost"
                    size="sm"
                    icon="folder"
                    aria-label={t`Open Files here`}
                    title={filesTitle}
                    disabled={filesDisabledForContainer}
                    loading={openingFiles}
                    onClick={() => void openFilesHere()}
                  />
                ) : null}
                {current ? (
                  <>
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
                  </>
                ) : null}
                <TerminalSettingsControl
                  open={settingsOpen}
                  phone={false}
                  resolved={display}
                  raw={activePrefs}
                  saving={savingPrefs}
                  error={prefsError}
                  onOpenChange={setSettingsOpen}
                  onChange={(next) => void savePreferences(next)}
                  onReset={resetPreferences}
                />
                <Button
                  variant="ghost"
                  size="sm"
                  icon={fullscreen ? "minimize" : "maximize"}
                  aria-label={fullscreen ? t`Exit fullscreen` : t`Fullscreen`}
                  aria-pressed={fullscreen}
                  title={fullscreen ? t`Exit fullscreen (Shift+Esc)` : t`Fullscreen`}
                  onClick={() => setFullscreen((was) => !was)}
                />
                {current ? (
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
                ) : null}
              </span>
            </div>
          )}
          {broadcastBar}
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
            {!current && (listed.isFetched || listed.isError)
              ? (containerEmpty ?? (tabs.length === 0 ? empty : null))
              : null}
            {notice}
          </div>
          <div className="terminal-foot">
            <div className="terminal-foot-copy">
              <Connection status={status} none={!current} />
              <span className="terminal-foot-note">
                <Trans>
                  · no SSH port needed · <Link to="/activity">Activity</Link> records that you opened it, not what you
                  type
                </Trans>
              </span>
              {lost !== undefined ? (
                <span className="terminal-foot-note">
                  <Trans>· {formatBytes(lost, i18n.locale)} it printed while you were away is gone</Trans>
                </span>
              ) : null}
            </div>
            {current && size ? (
              <span className="terminal-size">
                {size.columns} × {size.rows}
              </span>
            ) : null}
          </div>
          {phone && current && (status?.kind === "attached" || status?.kind === "reconnecting") ? (
            <KeyBar
              ctrl={ctrl}
              onCtrl={(on) => {
                setCtrl(on);
                handle()?.holdCtrl(on);
              }}
              onPress={(key) => handle()?.press(key)}
              onType={(text) => handle()?.type(text)}
              onTextSize={nextTextSize}
              onPaste={() => void handle()?.paste()}
            />
          ) : null}
        </section>

        <SavedCommandsPanel
          asSheet={commandsAsSheet}
          open={commandsOpen}
          onClose={() => setCommandsOpen(false)}
          canType={commandTargets.length > 0}
          broadcasting={broadcast}
          targetLabel={commandTargetLabel}
          onInsert={(cmd) => sendCommand(cmd, false)}
          onRun={(cmd) => sendCommand(cmd, true)}
        />
      </div>
    </div>
  );
}

interface TabProps {
  what: string;
  serverName: string;
  full: string;
  hint: string;
  isContainer: boolean;
  selected: boolean;
  broadcasting: boolean;
  ended: boolean;
  onSelect: () => void;
  onClose: () => void;
}

/** One session's tab: host or container name, its server, broadcast mark, and close button. */
function TerminalTab({
  what,
  serverName,
  full,
  hint,
  isContainer,
  selected,
  broadcasting,
  ended,
  onSelect,
  onClose,
}: TabProps) {
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
      aria-label={full}
      className={broadcasting ? "terminal-tab terminal-tab-broadcast" : "terminal-tab"}
      title={hint}
      onClick={onSelect}
      onKeyDown={keys}
      aria-keyshortcuts="Delete"
      aria-description={ended ? t`Ended` : broadcasting ? t`Selected to type into` : undefined}
    >
      <span className="terminal-tab-icon" aria-hidden="true">
        <Icon name={isContainer ? "service" : "server"} size={14} />
      </span>
      <span className="terminal-tab-name">{what}</span>
      <span className="terminal-tab-sep">{" · "}</span>
      <span className="terminal-tab-server">{serverName}</span>
      {broadcasting ? <span className="terminal-tab-broadcast-dot" aria-hidden="true" /> : null}
      <span
        className="terminal-tab-close"
        aria-hidden="true"
        title={t`Close ${full}`}
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
  /** Whether its shell ran as root. */
  root: boolean;
  onBringBack: () => void;
  onOpen: () => void;
  onClose: () => void;
  opening: boolean;
}

/** What the terminal on screen says over its output when it isn't there to type into. */
function Notice({ status, root, onBringBack, onOpen, onClose, opening }: NoticeProps) {
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
      const said = words(status.end, root);
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

function ExecForm({ opening, run }: { opening: boolean; run: (argv: string[]) => void }) {
  const { t } = useLingui();
  const [executable, setExecutable] = useState("");
  const [args, setArgs] = useState<{ key: number; value: string }[]>([]);
  const next = useRef(0);
  return (
    <details className="terminal-exec">
      <summary>
        <Trans>Run a command</Trans>
      </summary>
      <form
        onSubmit={(event) => {
          event.preventDefault();
          if (executable.trim() && !opening) run([executable.trim(), ...args.map((arg) => arg.value)]);
        }}
      >
        <TextField
          label={t`Executable`}
          placeholder="/bin/date"
          value={executable}
          onChange={(event) => setExecutable(event.target.value)}
        />
        {args.map((arg, index) => (
          <div key={arg.key} className="terminal-exec-argument">
            <TextField
              label={t`Argument ${index + 1}`}
              value={arg.value}
              onChange={(event) => {
                const value = event.target.value;
                setArgs((all) => all.map((item) => (item.key === arg.key ? { ...item, value } : item)));
              }}
            />
            <Button
              type="button"
              variant="ghost"
              icon="x"
              aria-label={t`Remove argument ${index + 1}`}
              onClick={() => setArgs((all) => all.filter((item) => item.key !== arg.key))}
            />
          </div>
        ))}
        <div className="terminal-exec-actions">
          <Button
            type="button"
            size="sm"
            variant="secondary"
            icon="plus"
            disabled={args.length >= 63}
            onClick={() => {
              const key = next.current++;
              setArgs((all) => [...all, { key, value: "" }]);
            }}
          >
            <Trans>Add argument</Trans>
          </Button>
          <Button type="submit" size="sm" icon="terminal" disabled={!executable.trim()} loading={opening}>
            <Trans>Run command</Trans>
          </Button>
        </div>
      </form>
    </details>
  );
}
