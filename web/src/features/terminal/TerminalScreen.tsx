// SPDX-License-Identifier: AGPL-3.0-only

import { createClient, type Transport } from "@connectrpc/connect";
import { type Terminal, TerminalService } from "@dockwell/api";
import type { ResolvedTheme } from "@dockwell/ui";
import { FitAddon } from "@xterm/addon-fit";
import { SearchAddon } from "@xterm/addon-search";
import { Terminal as XTerm } from "@xterm/xterm";
import "@xterm/xterm/css/xterm.css";
import { type Ref, useCallback, useEffect, useImperativeHandle, useRef } from "react";

import { type TerminalClient, TerminalSession, type TerminalStatus } from "./session";
import { adoptStyles } from "./styles";
import { lineHeightFor, TERMINAL_FONT, terminalTheme } from "./theme";

/** What the toolbar does to the terminal on screen. */
/** A key on the phone's bar that isn't a character. */
export type BarKey = "escape" | "tab" | "left" | "up" | "down" | "right";

/** What a key types, as a terminal sends it: cursor keys as the program in front asked for them. */
export function keySequence(key: BarKey, applicationCursor: boolean, ctrl: boolean): string {
  const arrows: Record<string, string> = { left: "D", up: "A", down: "B", right: "C" };
  const arrow = arrows[key];
  if (arrow) {
    if (ctrl) return `\x1b[1;5${arrow}`;
    return applicationCursor ? `\x1bO${arrow}` : `\x1b[${arrow}`;
  }
  return key === "escape" ? "\x1b" : "\t";
}

/** A character typed with Ctrl held, as a terminal sends it: Ctrl+C is ETX. What has no control form goes as it is. */
export function withCtrl(text: string): string {
  if (text.length !== 1) return text;
  if (text === " ") return "\x00";
  const code = text.toUpperCase().charCodeAt(0);
  return code >= 0x40 && code <= 0x7f ? String.fromCharCode(code & 0x1f) : text;
}

export interface ScreenHandle {
  find(text: string, backwards?: boolean): boolean;
  clearFind(): void;
  copy(): Promise<boolean>;
  paste(): Promise<void>;
  clear(): void;
  focus(): void;
  bringBack(): void;
  /** Types text as the person would, as a key on the phone's bar does. */
  type(text: string): void;
  /** Presses a key that isn't a character. */
  press(key: BarKey): void;
  /** Holds Ctrl for the next key, from the bar or the keyboard, or lets it go. */
  holdCtrl(on: boolean): void;
}

export interface ScreenProps {
  terminal: Terminal;
  transport: Transport;
  /** Only the terminal on screen is attached; the rest go on without the page. */
  active: boolean;
  theme: ResolvedTheme;
  fontSize: number;
  onStatus: (status: TerminalStatus) => void;
  onSize: (columns: number, rows: number) => void;
  onSelection: (has: boolean) => void;
  /** Some of what it printed while nobody was attached is gone. */
  onLost: (bytes: bigint) => void;
  /** Ctrl, held from the phone's bar, went with a key, and is let go. */
  onCtrlUsed?: () => void;
  label: string;
  ref?: Ref<ScreenHandle>;
}

/** Whether a key press is the platform's copy, with something chosen to copy. */
function isCopy(e: KeyboardEvent, mac: boolean): boolean {
  return (
    e.type === "keydown" && e.key.toLowerCase() === "c" && (mac ? e.metaKey && !e.ctrlKey : e.ctrlKey && e.shiftKey)
  );
}

/** Whether a key press is the platform's paste. Off a Mac, Ctrl+V is the terminal's own. */
function isPaste(e: KeyboardEvent, mac: boolean): boolean {
  return (
    e.type === "keydown" && e.key.toLowerCase() === "v" && (mac ? e.metaKey && !e.ctrlKey : e.ctrlKey && e.shiftKey)
  );
}

function binaryBytes(data: string): Uint8Array {
  const out = new Uint8Array(data.length);
  for (let i = 0; i < data.length; i++) out[i] = data.charCodeAt(i) & 0xff;
  return out;
}

const mac = typeof navigator !== "undefined" && /Mac|iPhone|iPad/.test(navigator.platform || navigator.userAgent);

/**
 * One terminal on screen: xterm.js, fed by its session, and typing into it. It
 * stays mounted while its tab is in the background, so its scrollback is there to
 * come back to, and attaches again from the last byte it had.
 */
export function TerminalScreen({
  terminal,
  transport,
  active,
  theme,
  fontSize,
  onStatus,
  onSize,
  onSelection,
  onLost,
  onCtrlUsed,
  label,
  ref,
}: ScreenProps) {
  const host = useRef<HTMLElement>(null);
  const parts = useRef<{ term: XTerm; fit: FitAddon; search: SearchAddon; session: TerminalSession } | undefined>(
    undefined,
  );
  const handlers = useRef({ onStatus, onSize, onSelection, onLost, onCtrlUsed });
  handlers.current = { onStatus, onSize, onSelection, onLost, onCtrlUsed };
  // Ctrl held from the phone's bar, for the next key, which a phone's keyboard has no Ctrl for.
  const ctrl = useRef(false);
  const letCtrlGo = useCallback(() => {
    if (!ctrl.current) return;
    ctrl.current = false;
    handlers.current.onCtrlUsed?.();
  }, []);
  const first = useRef({ theme, fontSize });

  useEffect(() => {
    const el = host.current;
    if (!el) return;
    const term = new XTerm({
      fontFamily: TERMINAL_FONT,
      fontSize: first.current.fontSize,
      lineHeight: lineHeightFor(first.current.fontSize),
      // What a program prints in a colour too close to its ground still reads.
      minimumContrastRatio: 4.5,
      theme: terminalTheme(first.current.theme),
      cursorBlink: true,
      scrollback: 5000,
      allowProposedApi: true,
      macOptionIsMeta: true,
      rightClickSelectsWord: true,
    });
    const fit = new FitAddon();
    const search = new SearchAddon();
    term.loadAddon(fit);
    term.loadAddon(search);
    const unadopt = adoptStyles(el);
    term.open(el);
    const client = createClient(TerminalService, transport) as unknown as TerminalClient;
    const session = new TerminalSession(
      client,
      terminal.id,
      {
        onData: (data) => term.write(data),
        onLost: (bytes) => handlers.current.onLost(bytes),
        onStatus: (status) => handlers.current.onStatus(status),
      },
      { columns: terminal.columns || term.cols, rows: terminal.rows || term.rows },
    );
    parts.current = { term, fit, search, session };
    const fitNow = () => {
      if (!el.isConnected || el.clientWidth === 0 || el.clientHeight === 0) return;
      try {
        fit.fit();
      } catch {
        // Not laid out yet; the next resize fits it.
      }
    };
    term.onData((data) => {
      if (ctrl.current) {
        letCtrlGo();
        session.type(withCtrl(data));
        return;
      }
      session.type(data);
    });
    term.onBinary((data) => session.type(binaryBytes(data)));
    term.onResize(({ cols, rows }) => {
      session.resize(cols, rows);
      handlers.current.onSize(cols, rows);
    });
    term.onSelectionChange(() => handlers.current.onSelection(term.hasSelection()));
    term.attachCustomKeyEventHandler((e) => {
      if (isCopy(e, mac) && term.hasSelection()) {
        void navigator.clipboard?.writeText(term.getSelection());
        return false;
      }
      // The browser pastes into xterm's own field, which types it as a paste.
      if (isPaste(e, mac)) return false;
      return true;
    });
    let frame = 0;
    const seen =
      typeof ResizeObserver === "undefined"
        ? undefined
        : new ResizeObserver(() => {
            cancelAnimationFrame(frame);
            frame = requestAnimationFrame(fitNow);
          });
    seen?.observe(el);
    // The cells are measured in JetBrains Mono once it's there, or they'd be the fallback's.
    void document.fonts?.ready.then(() => {
      if (parts.current?.term !== term) return;
      term.options.fontFamily = TERMINAL_FONT;
      term.options.lineHeight = lineHeightFor(term.options.fontSize ?? first.current.fontSize);
      fitNow();
    });
    fitNow();
    handlers.current.onSize(term.cols, term.rows);
    return () => {
      cancelAnimationFrame(frame);
      seen?.disconnect();
      session.stop();
      term.dispose();
      unadopt();
      parts.current = undefined;
    };
  }, [terminal.id, terminal.columns, terminal.rows, transport, letCtrlGo]);

  useEffect(() => {
    const p = parts.current;
    if (!p) return;
    if (active) {
      p.session.start();
      p.term.focus();
    } else {
      p.session.stop();
    }
  }, [active]);

  useEffect(() => {
    const p = parts.current;
    if (!p) return;
    p.term.options.theme = terminalTheme(theme);
  }, [theme]);

  useEffect(() => {
    const p = parts.current;
    if (!p || p.term.options.fontSize === fontSize) return;
    p.term.options.fontSize = fontSize;
    p.term.options.lineHeight = lineHeightFor(fontSize);
    try {
      p.fit.fit();
    } catch {
      // Laid out later.
    }
  }, [fontSize]);

  useImperativeHandle(
    ref,
    () => ({
      find: (text, backwards = false) => {
        const p = parts.current;
        if (!p || !text) return false;
        const decorations = {
          matchOverviewRuler: "#00000000",
          activeMatchColorOverviewRuler: "#00000000",
          matchBackground: terminalTheme(theme).selectionBackground,
          activeMatchBackground: terminalTheme(theme).yellow,
        };
        const found = backwards
          ? p.search.findPrevious(text, { decorations })
          : p.search.findNext(text, { decorations, incremental: true });
        // Finding the one match again clears it and chooses it again, and xterm tells
        // only of the clearing, so what's chosen is read as it is.
        handlers.current.onSelection(p.term.hasSelection());
        return found;
      },
      clearFind: () => {
        parts.current?.search.clearDecorations();
        parts.current?.term.clearSelection();
      },
      copy: async () => {
        const p = parts.current;
        if (!p?.term.hasSelection() || !navigator.clipboard) return false;
        await navigator.clipboard.writeText(p.term.getSelection());
        return true;
      },
      paste: async () => {
        const p = parts.current;
        if (!p || !navigator.clipboard) return;
        const text = await navigator.clipboard.readText();
        if (text) p.term.paste(text);
        p.term.focus();
      },
      clear: () => parts.current?.term.clear(),
      focus: () => parts.current?.term.focus(),
      bringBack: () => parts.current?.session.bringBack(),
      type: (text) => {
        const held = ctrl.current;
        letCtrlGo();
        parts.current?.session.type(held ? withCtrl(text) : text);
        parts.current?.term.focus();
      },
      press: (key) => {
        const p = parts.current;
        if (!p) return;
        const held = ctrl.current;
        letCtrlGo();
        p.session.type(keySequence(key, p.term.modes.applicationCursorKeysMode, held));
        p.term.focus();
      },
      holdCtrl: (on) => {
        ctrl.current = on;
        parts.current?.term.focus();
      },
    }),
    [theme, letCtrlGo],
  );

  return <section ref={host} className="terminal-screen" aria-label={label} hidden={!active} />;
}
