// SPDX-License-Identifier: AGPL-3.0-only

import { createClient, type Transport } from "@connectrpc/connect";
import { type Terminal, TerminalService } from "@dockwell/api";
import type { ResolvedTheme } from "@dockwell/ui";
import { FitAddon } from "@xterm/addon-fit";
import { SearchAddon } from "@xterm/addon-search";
import { Terminal as XTerm } from "@xterm/xterm";
import "@xterm/xterm/css/xterm.css";
import { type Ref, useCallback, useEffect, useImperativeHandle, useRef } from "react";

import { linkHandler } from "./links";
import { DEFAULT_SCROLLBACK, type XtermCursorStyle } from "./preferences";
import { type TerminalClient, TerminalSession, type TerminalStatus } from "./session";
import { adoptStyles } from "./styles";
import { lineHeightFor, TERMINAL_FONT, terminalTheme } from "./theme";

/** A key on the phone's bar that isn't a character. */
export type BarKey = "escape" | "tab" | "left" | "up" | "down" | "right";

/** Structured input mirrored to other opted-in broadcast tabs so each target encodes with its own modes. */
export type MirroredInput =
  | { kind: "paste"; text: string }
  | { kind: "key"; key: BarKey; ctrl: boolean }
  | { kind: "raw"; data: string | Uint8Array };

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

/** Recognizes arrow-key escape sequences so broadcast targets can re-encode for their own cursor mode. */
function cursorKeyOf(data: string): { key: BarKey; ctrl: boolean } | undefined {
  switch (data) {
    case "\x1b[A":
    case "\x1bOA":
      return { key: "up", ctrl: false };
    case "\x1b[B":
    case "\x1bOB":
      return { key: "down", ctrl: false };
    case "\x1b[C":
    case "\x1bOC":
      return { key: "right", ctrl: false };
    case "\x1b[D":
    case "\x1bOD":
      return { key: "left", ctrl: false };
    case "\x1b[1;5A":
      return { key: "up", ctrl: true };
    case "\x1b[1;5B":
      return { key: "down", ctrl: true };
    case "\x1b[1;5C":
      return { key: "right", ctrl: true };
    case "\x1b[1;5D":
      return { key: "left", ctrl: true };
    default:
      return undefined;
  }
}

/** A character typed with Ctrl held, as a terminal sends it: Ctrl+C is ETX. What has no control form goes as it is. */
export function withCtrl(text: string): string {
  if (text.length !== 1) return text;
  if (text === " ") return "\x00";
  const code = text.toUpperCase().charCodeAt(0);
  return code >= 0x40 && code <= 0x7f ? String.fromCharCode(code & 0x1f) : text;
}

/** Parses an OSC 7 payload (`file://host/path`) into an absolute path when valid. */
export function parseOsc7Path(payload: string): string | undefined {
  if (!payload.startsWith("file://")) return undefined;
  try {
    const u = new URL(payload);
    if (u.protocol !== "file:") return undefined;
    const decoded = decodeURIComponent(u.pathname);
    return decoded.startsWith("/") ? decoded : undefined;
  } catch {
    return undefined;
  }
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
  /** Sends raw bytes/text directly without re-triggering broadcast or stealing focus. */
  sendRaw(data: string | Uint8Array): void;
  /** Pastes text through xterm.paste() directly without re-triggering broadcast or applying Ctrl. */
  pasteDirect(text: string): void;
  /** Reports whether the program in this terminal currently has bracketed paste mode enabled. */
  hasBracketedPaste(): boolean;
  /** Receives mirrored input from another broadcast tab, encoded using this terminal's own modes. */
  receiveMirrored(input: MirroredInput): void;
  /** Presses a key that isn't a character. */
  press(key: BarKey): void;
  /** Holds Ctrl for the next key, from the bar or the keyboard, or lets it go. */
  holdCtrl(on: boolean): void;
}

export interface ScreenProps {
  terminal: Terminal;
  transport: Transport;
  /** Visible on screen and focused when active. */
  active: boolean;
  /** Attached to the backend stream (true when active or opted in to broadcast). */
  attached?: boolean;
  theme: ResolvedTheme;
  fontSize: number;
  fontFamily?: string;
  /** Explicit xterm lineHeight multiplier; 0 or omitted uses default 20 px row spacing. */
  lineHeight?: number;
  cursorStyle?: XtermCursorStyle;
  cursorBlink?: boolean;
  scrollback?: number;
  fullscreen?: boolean;
  onExitFullscreen?: () => void;
  /** Mirrors user input from this screen to broadcast targets when active. */
  onInput?: (input: MirroredInput) => void;
  /** Reports an OSC 7 working directory update when a shell emits one. */
  onOscCwd?: (path: string) => void;
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

/** Shift+Esc leaves fullscreen without stealing bare Esc from programs like vim. */
function isExitFullscreenChord(e: KeyboardEvent): boolean {
  return e.type === "keydown" && e.key === "Escape" && e.shiftKey && !e.ctrlKey && !e.altKey && !e.metaKey;
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
  attached = active,
  theme,
  fontSize,
  fontFamily = TERMINAL_FONT,
  lineHeight = 0,
  cursorStyle = "block",
  cursorBlink = true,
  scrollback = DEFAULT_SCROLLBACK,
  fullscreen = false,
  onExitFullscreen,
  onInput,
  onOscCwd,
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
  const effectiveLineHeight = lineHeight > 0 ? lineHeight : lineHeightFor(fontSize, fontFamily);
  const handlers = useRef({
    onStatus,
    onSize,
    onSelection,
    onLost,
    onCtrlUsed,
    onInput,
    onOscCwd,
    onExitFullscreen,
    fullscreen,
    fontFamily,
    fontSize,
    lineHeight,
  });
  handlers.current = {
    onStatus,
    onSize,
    onSelection,
    onLost,
    onCtrlUsed,
    onInput,
    onOscCwd,
    onExitFullscreen,
    fullscreen,
    fontFamily,
    fontSize,
    lineHeight,
  };
  // Ctrl held from the phone's bar, for the next key, which a phone's keyboard has no Ctrl for.
  const ctrl = useRef(false);
  // Suppresses broadcast mirroring and Ctrl rewriting during direct pastes / mirrored delivery.
  const suppressMirror = useRef(false);
  // Captures the plain text of an in-flight user paste so mirroring re-pastes per target.
  const pendingPaste = useRef<string | undefined>(undefined);
  const letCtrlGo = useCallback(() => {
    if (!ctrl.current) return;
    ctrl.current = false;
    handlers.current.onCtrlUsed?.();
  }, []);
  const first = useRef({ theme, fontSize, fontFamily, effectiveLineHeight, cursorStyle, cursorBlink, scrollback });

  useEffect(() => {
    const el = host.current;
    if (!el) return;
    const term = new XTerm({
      fontFamily: first.current.fontFamily,
      fontSize: first.current.fontSize,
      lineHeight: first.current.effectiveLineHeight,
      // What a program prints in a colour too close to its ground still reads.
      minimumContrastRatio: 4.5,
      theme: terminalTheme(first.current.theme),
      cursorStyle: first.current.cursorStyle,
      cursorBlink: first.current.cursorBlink,
      scrollback: first.current.scrollback,
      allowProposedApi: true,
      macOptionIsMeta: true,
      rightClickSelectsWord: true,
      // A link a program prints goes only to http(s), with Ctrl or Cmd held.
      linkHandler: linkHandler(() => host.current),
    });
    const fit = new FitAddon();
    const search = new SearchAddon();
    term.loadAddon(fit);
    term.loadAddon(search);
    const unadopt = adoptStyles(el);
    term.open(el);
    term.parser?.registerOscHandler?.(7, (payload) => {
      const parsed = parseOsc7Path(payload);
      if (parsed) handlers.current.onOscCwd?.(parsed);
      return false;
    });
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
    const onDomPaste = (e: ClipboardEvent) => {
      const text = e.clipboardData?.getData("text/plain");
      if (!text) return;
      pendingPaste.current = text;
      queueMicrotask(() => {
        if (pendingPaste.current === text) pendingPaste.current = undefined;
      });
    };
    el.addEventListener("paste", onDomPaste, true);

    term.onData((data) => {
      if (suppressMirror.current) {
        session.type(data);
        return;
      }
      const pasted = pendingPaste.current;
      if (pasted !== undefined) {
        pendingPaste.current = undefined;
        session.type(data);
        handlers.current.onInput?.({ kind: "paste", text: pasted });
        return;
      }
      if (data.startsWith("\x1b[200~") && data.endsWith("\x1b[201~")) {
        session.type(data);
        handlers.current.onInput?.({ kind: "paste", text: data.slice(6, -6) });
        return;
      }
      let payload = data;
      if (ctrl.current) {
        letCtrlGo();
        payload = withCtrl(data);
        session.type(payload);
        handlers.current.onInput?.({ kind: "raw", data: payload });
        return;
      }
      const cursorKey = cursorKeyOf(payload);
      session.type(payload);
      if (cursorKey) {
        handlers.current.onInput?.({ kind: "key", key: cursorKey.key, ctrl: cursorKey.ctrl });
      } else {
        handlers.current.onInput?.({ kind: "raw", data: payload });
      }
    });
    term.onBinary((data) => {
      const bytes = binaryBytes(data);
      session.type(bytes);
      handlers.current.onInput?.({ kind: "raw", data: bytes });
    });
    term.onResize(({ cols, rows }) => {
      session.resize(cols, rows);
      handlers.current.onSize(cols, rows);
    });
    term.onSelectionChange(() => handlers.current.onSelection(term.hasSelection()));
    term.attachCustomKeyEventHandler((e) => {
      // Bare Escape always reaches the shell (vim, less, htop, vi mode); only Shift+Esc leaves fullscreen here.
      if (handlers.current.fullscreen && isExitFullscreenChord(e)) {
        e.preventDefault();
        e.stopPropagation();
        handlers.current.onExitFullscreen?.();
        return false;
      }
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
    // The cells are measured in the chosen mono font once it's loaded.
    void document.fonts?.ready.then(() => {
      if (parts.current?.term !== term) return;
      const cur = handlers.current;
      term.options.fontFamily = cur.fontFamily;
      term.options.lineHeight = cur.lineHeight > 0 ? cur.lineHeight : lineHeightFor(cur.fontSize, cur.fontFamily);
      fitNow();
    });
    fitNow();
    handlers.current.onSize(term.cols, term.rows);
    return () => {
      el.removeEventListener("paste", onDomPaste, true);
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
    if (attached) {
      p.session.start();
    } else {
      p.session.stop();
    }
  }, [attached]);

  useEffect(() => {
    const p = parts.current;
    if (!p || !active) return;
    p.term.focus();
  }, [active]);

  useEffect(() => {
    const p = parts.current;
    if (!p) return;
    p.term.options.theme = terminalTheme(theme);
  }, [theme]);

  useEffect(() => {
    const p = parts.current;
    if (!p) return;
    const changed =
      p.term.options.fontSize !== fontSize ||
      p.term.options.fontFamily !== fontFamily ||
      p.term.options.lineHeight !== effectiveLineHeight;
    if (!changed) return;
    p.term.options.fontSize = fontSize;
    p.term.options.fontFamily = fontFamily;
    p.term.options.lineHeight = effectiveLineHeight;
    try {
      p.fit.fit();
    } catch {
      // Laid out later.
    }
  }, [fontSize, fontFamily, effectiveLineHeight]);

  useEffect(() => {
    const p = parts.current;
    if (!p) return;
    p.term.options.cursorStyle = cursorStyle;
    p.term.options.cursorBlink = cursorBlink;
    p.term.options.scrollback = scrollback;
  }, [cursorStyle, cursorBlink, scrollback]);

  useImperativeHandle(ref, () => {
    const pasteDirect = (text: string) => {
      const p = parts.current;
      if (!p) return;
      suppressMirror.current = true;
      try {
        p.term.paste(text);
      } finally {
        suppressMirror.current = false;
      }
    };
    return {
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
        if (text) {
          pendingPaste.current = text;
          try {
            p.term.paste(text);
          } finally {
            if (pendingPaste.current === text) pendingPaste.current = undefined;
          }
        }
        p.term.focus();
      },
      clear: () => parts.current?.term.clear(),
      focus: () => parts.current?.term.focus(),
      bringBack: () => parts.current?.session.bringBack(),
      type: (text) => {
        const held = ctrl.current;
        letCtrlGo();
        const payload = held ? withCtrl(text) : text;
        parts.current?.session.type(payload);
        handlers.current.onInput?.({ kind: "raw", data: payload });
        parts.current?.term.focus();
      },
      sendRaw: (data) => {
        parts.current?.session.type(data);
      },
      pasteDirect,
      hasBracketedPaste: () => Boolean(parts.current?.term.modes.bracketedPasteMode),
      receiveMirrored: (input) => {
        const p = parts.current;
        if (!p) return;
        switch (input.kind) {
          case "paste":
            pasteDirect(input.text);
            break;
          case "key":
            p.session.type(keySequence(input.key, p.term.modes.applicationCursorKeysMode, input.ctrl));
            break;
          case "raw":
            p.session.type(input.data);
            break;
        }
      },
      press: (key) => {
        const p = parts.current;
        if (!p) return;
        const held = ctrl.current;
        letCtrlGo();
        const seq = keySequence(key, p.term.modes.applicationCursorKeysMode, held);
        p.session.type(seq);
        handlers.current.onInput?.({ kind: "key", key, ctrl: held });
        p.term.focus();
      },
      holdCtrl: (on) => {
        ctrl.current = on;
        parts.current?.term.focus();
      },
    };
  }, [theme, letCtrlGo]);

  return <section ref={host} className="terminal-screen" aria-label={label} hidden={!active} />;
}
