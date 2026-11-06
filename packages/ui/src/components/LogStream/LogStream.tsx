// SPDX-License-Identifier: AGPL-3.0-only

import {
  Fragment,
  memo,
  type ReactNode,
  type Ref,
  useCallback,
  useEffect,
  useId,
  useImperativeHandle,
  useLayoutEffect,
  useMemo,
  useReducer,
  useRef,
  useState,
} from "react";

import { cx, hasContent } from "../../lib/cx";
import { TONE } from "../../lib/status";
import { type UIStrings, useUIStrings } from "../../lib/strings";
import { useControlled } from "../../lib/useControlled";
import { Button } from "../Button/Button";
import { Glyph } from "../Glyph/Glyph";
import { Icon } from "../Icon/Icon";
import { type LogSpan, parseAnsi, plainText } from "./ansi";
import { HeightIndex } from "./heights";
import "./LogStream.css";

export type LogLevel = "info" | "warn" | "error";

export interface LogLine {
  id?: string | number;
  /** Wall-clock time with milliseconds, in the viewer's time zone. */
  time: string;
  /** The time in full, for a tooltip. */
  timeTitle?: string;
  /** The replica or server the line came from. */
  source: string;
  /** The source in full, for a tooltip: its deploy and its container. */
  sourceTitle?: string;
  level?: LogLevel;
  /** May hold terminal colour codes, which draw as colours and never as text. */
  text: string;
  /** How much more the line had past its text, in words such as "12 KB". */
  cut?: string;
  /** A note in the stream rather than a line of it, such as lines left out. */
  notice?: "info" | "warning";
  /** When it was written, in milliseconds, so lines close together can read as one. */
  at?: number;
  /** The day it was written, in words. It shows where the day changes. */
  day?: string;
}

/** What a page can do with a stream that is showing. */
export interface LogStreamHandle {
  /** Brings a position into view, stopping following. */
  show(position: number): void;
}

/**
 * A long list of log lines, read a few at a time: a plain array, or anything that
 * can say what is at a position. Lines leave from the start and arrive at the end.
 */
export interface LogLines {
  /** How many lines there are. */
  readonly length: number;
  /** The position of the first, which grows as old lines go. Zero if none ever do. */
  readonly start: number;
  /** Changes whenever the lines do. */
  readonly version: number;
  /** Changes when positions no longer mean what they did, as with a new filter. */
  readonly epoch: number;
  /** The line at a position from start up to start + length. */
  at(position: number): LogLine | undefined;
  /** The position of a line by its ID, if it shows, to keep a reader's place. */
  positionOf?(id: string | number): number | undefined;
  /** The position of what shows nearest after a line that is here, shown or not. */
  positionNear?(id: string | number): number | undefined;
  /** How many have come that don't show yet, while what shows holds still for a reader. */
  readonly arrived?: number;
}

export interface LogStreamProps {
  lines: readonly LogLine[] | LogLines;
  title?: ReactNode;
  /** While streaming: shows Live, or Paused once following stops. */
  live?: boolean;
  /** Whether new lines scroll into view. Following stops on its own when the reader scrolls up. */
  follow?: boolean;
  onFollowChange?: (follow: boolean) => void;
  /** Pixels; "fill" takes the room its parent gives it. */
  height?: number | "fill";
  /** Filters and other controls, before the follow button. */
  toolbar?: ReactNode;
  /** Shows the source column. Leave it out when there is only one source. */
  showSource?: boolean;
  /** Marks this text wherever a line has it, in any case. */
  highlight?: string;
  /** Takes the place of the lines while there are none. */
  empty?: ReactNode;
  /** Above the lines, such as a way to load older ones. */
  top?: ReactNode;
  /** A line under the log, which holds its place whether or not it has words in it. */
  foot?: ReactNode;
  /** Beside the title, such as the zone times are in. */
  meta?: ReactNode;
  /** Wraps long lines, or cuts them at the edge. */
  wrap?: boolean;
  /** The line a reader is on: a search's current match, or the current error. */
  current?: string | number | undefined;
  /** What can be done with one line, shown as it is pointed at. */
  actions?: (line: LogLine, position: number) => ReactNode;
  /** A line as text, when a selection of whole lines is copied. */
  copyLine?: (line: LogLine) => string;
  /** Holds a place for Pause while not live, so what sits beside it stays put. */
  holdFollow?: boolean;
  ref?: Ref<LogStreamHandle>;
  className?: string;
}

const LEVEL_SHAPE = { warn: TONE.warning.shape, error: TONE.critical.shape } as const;

/** How close to the end, in pixels, still counts as reading the newest lines. */
const AT_END = 8;
/** One line of text in the log's type. */
const ROW = 19;
/** The padding the phone's layout adds to an entry, around its meta row. */
const PHONE_EXTRA = ROW + 16;
/** How many rows are drawn past each edge of what shows, so scrolling doesn't show a gap. */
const OVERSCAN = 16;
/** How many rows are drawn where there is no layout to say how many fit. */
const NO_LAYOUT_ROWS = 200;
/** How long the browser's own scrolling goes on after what shows changes. */
const SETTLE_MS = 300;
/** How much of a line is drawn until a reader asks for the rest. */
const LONG_LINE = 1000;
/** How far apart lines of one level can be, in milliseconds, and still read as one thing, as a stack trace does. */
const GROUP_MS = 100;
/** Past this, how many lines came is only "a lot", as the exact number moves too fast to read. */
const MANY_LINES = 10_000;

/** A line as text, as it shows. */
function lineText(line: LogLine): string {
  return [line.time, line.source, plainText(line.text)].filter(Boolean).join(" ");
}

/** Whether this line carries on the one before it: of one level, from one source, and close in time. */
function continues(before: LogLine | undefined, line: LogLine): boolean {
  if (!before || before.notice || line.notice) return false;
  if (!line.level || line.level === "info" || before.level !== line.level || before.source !== line.source)
    return false;
  if (before.at !== undefined && line.at !== undefined) return Math.abs(line.at - before.at) <= GROUP_MS;
  return before.time === line.time;
}

/** A JSON line set out over several, what it says first, or undefined if it isn't one. */
export function prettyJson(text: string): string | undefined {
  let value: unknown;
  try {
    value = JSON.parse(text);
  } catch {
    return undefined;
  }
  if (typeof value !== "object" || value === null || Array.isArray(value)) return undefined;
  const fields = value as Record<string, unknown>;
  const first = ["msg", "message"].filter((key) => key in fields);
  const keys = [...first, ...Object.keys(fields).filter((key) => !first.includes(key))];
  return keys
    .map((key) => {
      const field = fields[key];
      return `${key}: ${typeof field === "string" ? field : JSON.stringify(field, null, 2)}`;
    })
    .join("\n");
}

/** Whether a line looks like JSON that prettyJson may set out, without reading it. */
function looksLikeJson(text: string): boolean {
  return text.startsWith("{") && text.endsWith("}");
}

function fromArray(lines: readonly LogLine[], version: number, epoch: number): LogLines {
  return { length: lines.length, start: 0, version, epoch, at: (position) => lines[position] };
}

function isArray(lines: readonly LogLine[] | LogLines): lines is readonly LogLine[] {
  return Array.isArray(lines);
}

/**
 * Log lines that follow new output and pause the moment someone scrolls up
 * to read. Warnings and errors carry a glyph in their own column, and errors
 * a faint critical band, so the level reads without colour.
 *
 * Only the lines that show are drawn, so a hundred thousand of them cost what a
 * screenful does. Screen readers hear when the stream starts or stops, and not
 * each line, since a log can write thousands a second.
 */
export function LogStream({
  lines: linesProp,
  title,
  live = false,
  follow: followProp,
  onFollowChange,
  height = 240,
  toolbar,
  showSource = true,
  highlight = "",
  empty,
  top,
  foot,
  meta,
  wrap = true,
  current,
  actions,
  copyLine = lineText,
  holdFollow = false,
  ref,
  className,
}: LogStreamProps) {
  const strings = useUIStrings();
  const [follow, setFollow] = useControlled(followProp, true, onFollowChange);
  const body = useRef<HTMLDivElement>(null);
  const titleId = useId();
  const bodyId = useId();
  const hasTitle = hasContent(title);

  // A plain array has no version of its own; a new array is a new version.
  const arrayVersion = useRef(0);
  const lastArray = useRef<readonly LogLine[] | undefined>(undefined);
  const lines = useMemo(() => {
    if (!isArray(linesProp)) return linesProp;
    if (lastArray.current !== linesProp) {
      arrayVersion.current++;
      lastArray.current = linesProp;
    }
    return fromArray(linesProp, arrayVersion.current, 0);
  }, [linesProp]);

  const [, redraw] = useReducer((n: number) => n + 1, 0);
  const index = useRef(new HeightIndex());
  const synced = useRef({ epoch: -1, end: 0 });
  // How the text wraps at the current width.
  const columns = useRef(0);
  const phone = useRef(false);
  const adjust = useRef(0);
  const pendingTop = useRef<number | undefined>(undefined);
  const settledAt = useRef(0);
  const anchor = useRef<{ id: string | number; into: number } | undefined>(undefined);
  // A filter hid the line a reader is on. It stays their place, for when it shows
  // again, until they go somewhere else: looking something up shouldn't lose it.
  const placeHeld = useRef(false);
  const expected = useRef<number | undefined>(undefined);
  const lastTop = useRef(0);
  const [view, setView] = useState({ top: 0, height: 0 });
  const rows = useRef(new Map<number, HTMLDivElement>());
  const [expanded, setExpanded] = useState<ReadonlySet<string | number>>(new Set());
  // The line tapped or clicked, for a finger, which has nothing to hover with, and
  // the one pointed at. A line's actions are drawn only for these, the current
  // line and the one holding the keyboard's focus, as a flood draws every row anew
  // many times a second.
  const [activeLine, setActiveLine] = useState<string | number | undefined>(undefined);
  const [pointedLine, setPointedLine] = useState<string | number | undefined>(undefined);
  const [focusedLine, setFocusedLine] = useState<string | number | undefined>(undefined);
  const rowsBox = useRef<HTMLDivElement>(null);
  // A position to bring into view, which holds until its row has been measured.
  const wanted = useRef<number | undefined>(undefined);
  // Which rows a selection starts and ends in, kept drawn so it survives scrolling.
  const chosenRows = useRef<readonly [number, number] | undefined>(undefined);
  const wraps = useRef(wrap);
  wraps.current = wrap;

  const estimate = useCallback((line: LogLine | undefined) => {
    if (!line) return ROW;
    const lines =
      wraps.current && columns.current > 0
        ? Math.max(1, Math.ceil(Math.min(line.text.length, LONG_LINE) / columns.current))
        : 1;
    return lines * ROW + (phone.current && !line.notice ? PHONE_EXTRA : 0);
  }, []);

  // Keeps the heights in step with the lines. It only moves forward with their
  // version, so running it twice for one render changes nothing.
  const end = lines.start + lines.length;
  if (synced.current.epoch !== lines.epoch) {
    // Positions mean something else now. A reader who isn't following keeps
    // their place, by the line at the top of what shows.
    const place = synced.current.epoch >= 0 ? anchor.current : undefined;
    if (synced.current.epoch >= 0) settledAt.current = performance.now() + SETTLE_MS;
    index.current.reset(lines.start);
    for (let p = lines.start; p < end; p++) index.current.push(estimate(lines.at(p)));
    synced.current = { epoch: lines.epoch, end };
    if (place) {
      const position = lines.positionOf?.(place.id);
      const near = position === undefined ? lines.positionNear?.(place.id) : undefined;
      if (position !== undefined) pendingTop.current = index.current.offset(position) + place.into;
      else if (near !== undefined) pendingTop.current = index.current.offset(near);
      // With nothing showing, whether it is still here can't be told, and when it
      // has gone, the next that shows is a place like any other.
      placeHeld.current = position === undefined && (near !== undefined || lines.length === 0);
    }
  } else {
    if (lines.start > index.current.first) {
      const gone = index.current.dropTo(lines.start);
      adjust.current -= gone;
    }
    for (let p = synced.current.end; p < end; p++) index.current.push(estimate(lines.at(p)));
    synced.current.end = end;
  }

  const noteAnchor = (top: number) => {
    if (placeHeld.current) return;
    const atTop = index.current.at(top);
    const line = lines.at(atTop);
    if (line?.id !== undefined) anchor.current = { id: line.id, into: top - index.current.offset(atTop) };
  };

  const fits = view.height > 0;
  const first = fits ? Math.max(lines.start, index.current.at(view.top) - OVERSCAN) : lines.start;
  const last = fits
    ? Math.min(end, index.current.at(view.top + view.height) + OVERSCAN + 1)
    : Math.min(end, lines.start + NO_LAYOUT_ROWS);

  useImperativeHandle(
    ref,
    () => ({
      show(position: number) {
        wanted.current = position;
        setFollow(false);
        redraw();
      },
    }),
    [setFollow],
  );

  const onHeights = useCallback(() => {
    const el = body.current;
    if (!el) return;
    setView((was) =>
      was.top === el.scrollTop && was.height === el.clientHeight ? was : { top: el.scrollTop, height: el.clientHeight },
    );
  }, []);

  // Whatever changed what is drawn: measures what is new, keeps the reader's
  // place, and follows the end.
  useLayoutEffect(() => {
    const el = body.current;
    if (!el) return;
    let moved = 0;
    let changed = false;
    const topRow = index.current.at(el.scrollTop);
    // Reading each row's height costs one layout, of the few rows there are,
    // and it catches whatever changed how a row wraps: its width, its source
    // column, its font.
    for (const [position, row] of rows.current) {
      const h = row.offsetHeight;
      if (h <= 0) continue;
      const was = index.current.get(position);
      if (h === was) continue;
      index.current.set(position, h);
      changed = true;
      if (position < topRow) moved += h - was;
    }
    if (changed) redraw();
    const position = wanted.current;
    if (position !== undefined) {
      const room = rowsBox.current?.offsetTop ?? 0;
      el.scrollTop = Math.max(0, room + index.current.offset(position) - el.clientHeight / 3);
      expected.current = el.scrollTop;
      pendingTop.current = undefined;
      // That line is the reader's place now, wherever it sits in what shows, and not
      // whichever line happens to be above it.
      const line = lines.at(position);
      if (line?.id !== undefined) {
        anchor.current = { id: line.id, into: el.scrollTop - index.current.offset(position) };
        placeHeld.current = true;
      }
      // Once it is drawn and measured, this was its place. Until then it was a guess.
      if (rows.current.has(position)) wanted.current = undefined;
      else redraw();
    } else if (follow && lines.length > 0) {
      el.scrollTop = el.scrollHeight;
      expected.current = el.scrollTop;
      pendingTop.current = undefined;
      placeHeld.current = false;
    } else {
      const pending = pendingTop.current;
      const by = adjust.current + moved;
      if (pending !== undefined) {
        el.scrollTop = pending;
        expected.current = el.scrollTop;
        pendingTop.current = undefined;
      } else if (by !== 0) {
        el.scrollTop = el.scrollTop + by;
        expected.current = el.scrollTop;
      }
    }
    adjust.current = 0;
    lastTop.current = el.scrollTop;
    onHeights();
    // Note which line is at the top, in case the positions change under it.
    noteAnchor(el.scrollTop);
  });

  // A new width wraps lines differently. Every height goes back to an estimate
  // from how many characters fit, and the rows that show measure themselves.
  useEffect(() => {
    const el = body.current;
    if (!el || typeof ResizeObserver === "undefined") return undefined;
    let width = -1;
    const resized = () => {
      if (el.clientWidth !== width) {
        width = el.clientWidth;
        const text = el.querySelector<HTMLElement>(".dw-log-text");
        const unit = charWidth(el);
        const narrow = el.closest<HTMLElement>(".dw-log")?.classList.contains("dw-log") && width < 560;
        phone.current = Boolean(narrow);
        const room = text?.clientWidth || width - (narrow ? 24 : 96 + 120 + 10 + 3 * 12 + 32);
        columns.current = unit > 0 ? Math.max(8, Math.floor(room / unit)) : 0;
        const at = index.current;
        const { first: from, length } = at;
        for (let p = from; p < from + length; p++) at.set(p, estimate(lineAt.current(p)));
      }
      onHeights();
      redraw();
    };
    const observer = new ResizeObserver(resized);
    observer.observe(el);
    resized();
    // The log's font may arrive after it is drawn, which changes what wraps.
    void document.fonts?.ready.then(() => {
      width = -1;
      resized();
    });
    return () => observer.disconnect();
  }, [estimate, onHeights]);

  const lineAt = useRef(lines.at.bind(lines));
  lineAt.current = (position) => lines.at(position);

  // The reader's own scrolling: drawing what shows, and stopping following if it
  // went up. What this component scrolled itself says so first.
  const scrolling = useRef(0);
  const onScroll = () => {
    const el = body.current;
    if (!el) return;
    const now = el.scrollTop;
    const ours = expected.current !== undefined && Math.abs(now - expected.current) <= 1;
    expected.current = undefined;
    // Just after what shows changed, as with a new filter, the browser moves the
    // scroll itself. Only what a reader does counts, and their wheel, keys and
    // fingers say so on their own.
    const settling = performance.now() < settledAt.current;
    const atEnd = el.scrollHeight - now - el.clientHeight < AT_END;
    if (!ours && follow && now < lastTop.current && !atEnd) {
      if (settling) el.scrollTop = el.scrollHeight;
      else setFollow(false);
    }
    // Going back down to the end is asking to follow again.
    if (!ours && !settling && !follow && live && atEnd && now > lastTop.current) setFollow(true);
    lastTop.current = now;
    if (scrolling.current === 0) {
      scrolling.current = requestAnimationFrame(() => {
        scrolling.current = 0;
        onHeights();
      });
    }
    if (!ours && !settling) placeHeld.current = false;
    noteAnchor(now);
  };
  useEffect(() => () => cancelAnimationFrame(scrolling.current), []);
  // A line the keyboard is on can leave the view or the store, and its buttons with
  // it. The reader's place then stays with the log, not at the top of the page.
  useLayoutEffect(() => {
    if (focusedLine !== undefined && document.activeElement === document.body) {
      setFocusedLine(undefined);
      body.current?.focus({ preventScroll: true });
    }
  });

  // Going up by wheel, key or touch stops following before the next line can pull
  // the reader back down.
  const touchY = useRef(0);
  const reading = () => {
    if (follow) setFollow(false);
  };
  const onKeyDown = (e: React.KeyboardEvent) => {
    if (e.key === "ArrowUp" || e.key === "PageUp" || e.key === "Home") reading();
    if (e.key === "End" && live && !follow) {
      e.preventDefault();
      setFollow(true);
    }
  };

  // Text being chosen with the mouse holds still. A click that chose nothing goes
  // back to following, as nothing needed holding.
  const pressPaused = useRef(false);
  const chosenInLog = useCallback(() => {
    const chosen = document.getSelection();
    return Boolean(chosen && !chosen.isCollapsed && body.current?.contains(chosen.anchorNode));
  }, []);
  const onPress = (e: React.PointerEvent) => {
    if (e.button !== 0) return;
    // The scrollbar itself, whose scroll events come too late to hold it.
    if (e.target === e.currentTarget) {
      reading();
      return;
    }
    if (e.pointerType !== "mouse" || !follow || !(e.target as Element).closest(".dw-log-row")) return;
    pressPaused.current = true;
    reading();
    window.addEventListener(
      "pointerup",
      () => {
        if (pressPaused.current && !chosenInLog()) setFollow(true);
        pressPaused.current = false;
      },
      { once: true },
    );
  };

  // A selection made any other way holds still as well, and the rows it starts
  // and ends in stay drawn, so it outlives scrolling away from it.
  useEffect(() => {
    const changed = () => {
      const chosen = document.getSelection();
      if (!chosen || chosen.isCollapsed || !body.current?.contains(chosen.anchorNode)) {
        chosenRows.current = undefined;
        return;
      }
      const from = rowPosition(chosen.anchorNode);
      const to = rowPosition(chosen.focusNode);
      if (from !== undefined && to !== undefined) chosenRows.current = from <= to ? [from, to] : [to, from];
      if (followRef.current) setFollow(false);
    };
    document.addEventListener("selectionchange", changed);
    return () => document.removeEventListener("selectionchange", changed);
  }, [setFollow]);
  const followRef = useRef(follow);
  followRef.current = follow;

  // Whole lines copy as what they are, a line of text each, rather than as cells
  // on lines of their own. Part of one line copies as the browser does.
  const onCopy = (e: React.ClipboardEvent) => {
    const chosen = document.getSelection();
    if (!chosen || chosen.isCollapsed) return;
    const from = rowPosition(chosen.anchorNode);
    const to = rowPosition(chosen.focusNode);
    if (from === undefined || to === undefined) return;
    const inOneText = from === to && closestText(chosen.anchorNode) && closestText(chosen.focusNode);
    if (inOneText) return;
    const text: string[] = [];
    for (let p = Math.min(from, to); p <= Math.max(from, to); p++) {
      const line = lines.at(p);
      if (line) text.push(line.notice ? line.text : copyLine(line));
    }
    e.clipboardData.setData("text/plain", text.join("\n"));
    e.preventDefault();
  };

  // How many have come since following stopped.
  const pausedAt = useRef(end);
  if (follow) pausedAt.current = end;
  const newLines = lines.arrived ?? Math.max(0, end - pausedAt.current);

  const drawn: ReactNode[] = [];
  const nextRows = new Map<number, HTMLDivElement>();
  rows.current = nextRows;
  const drawRow = (p: number) => {
    const line = lines.at(p);
    if (!line) return;
    const key = line.id ?? p;
    const before = p > lines.start ? lines.at(p - 1) : undefined;
    const carries = continues(before, line);
    drawn.push(
      <div
        key={key}
        data-position={p}
        className="dw-log-row"
        style={{ transform: `translateY(${index.current.offset(p)}px)` }}
        ref={(el) => {
          if (el) nextRows.set(p, el);
        }}
      >
        <Row
          line={line}
          position={p}
          showSource={showSource}
          highlight={highlight}
          strings={strings}
          open={expanded.has(key)}
          onOpen={() =>
            setExpanded((was) => {
              const next = new Set(was);
              if (!next.delete(key)) next.add(key);
              return next;
            })
          }
          carries={carries}
          sameTime={Boolean(before && !before.notice && before.time === line.time && before.source === line.source)}
          day={line.day && before?.day !== line.day ? line.day : undefined}
          current={current !== undefined && line.id === current}
          active={activeLine !== undefined && line.id === activeLine}
          actions={
            actions &&
            line.id !== undefined &&
            (line.id === current || line.id === activeLine || line.id === pointedLine || line.id === focusedLine)
              ? actions
              : undefined
          }
        />
      </div>,
    );
  };
  // In order, so the rows a selection holds keep their places in the document.
  const held = chosenRows.current;
  if (held && held[0] < first) drawRow(held[0]);
  if (held && held[1] < first && held[1] !== held[0]) drawRow(held[1]);
  for (let p = first; p < last; p++) drawRow(p);
  if (held && held[0] >= last) drawRow(held[0]);
  if (held && held[1] >= last && held[1] !== held[0]) drawRow(held[1]);

  return (
    <div
      className={cx(
        "dw-log",
        height === "fill" && "is-fill",
        !showSource && "no-source",
        !wrap && "no-wrap",
        className,
      )}
    >
      <div className="dw-log-head">
        <div className="dw-log-title">
          {live ? (
            <span className={cx("dw-live", !follow && "is-paused")} role="status">
              <span className="dw-live-dot" />
              <Held
                now={follow ? strings.logStream.live : strings.logStream.paused}
                other={follow ? strings.logStream.paused : strings.logStream.live}
              />
            </span>
          ) : null}
          {hasTitle ? (
            <span id={titleId} className="dw-log-name">
              {title}
            </span>
          ) : null}
          {hasContent(meta) ? <span className="dw-log-meta">{meta}</span> : null}
        </div>
        <div className="dw-log-tools">
          {toolbar}
          {live || holdFollow ? (
            <Button
              size="sm"
              variant="ghost"
              icon={follow ? "pause" : "play"}
              aria-controls={bodyId}
              onClick={() => setFollow(!follow)}
              className={cx(!live && "is-held")}
              disabled={!live}
            >
              <Held
                now={follow ? strings.logStream.pause : strings.logStream.follow}
                other={follow ? strings.logStream.follow : strings.logStream.pause}
              />
            </Button>
          ) : null}
        </div>
      </div>
      <div className="dw-log-frame">
        <div
          ref={body}
          id={bodyId}
          className="dw-log-body"
          style={height === "fill" ? undefined : { height }}
          onScroll={onScroll}
          onKeyDown={onKeyDown}
          onWheel={(e) => {
            if (e.deltaY < 0) reading();
          }}
          onTouchStart={(e) => {
            touchY.current = e.touches[0]?.clientY ?? 0;
          }}
          onTouchMove={(e) => {
            // A finger moving down drags the lines down: scrolling up.
            if ((e.touches[0]?.clientY ?? 0) > touchY.current + 4) reading();
          }}
          onPointerDown={onPress}
          onClick={(e) => {
            if (chosenInLog()) return;
            const row = (e.target as Element).closest<HTMLElement>(".dw-log-row");
            const line = row ? lines.at(Number(row.dataset["position"])) : undefined;
            setActiveLine(line?.id);
          }}
          onCopy={onCopy}
          onFocus={(e) => {
            const row = (e.target as Element).closest<HTMLElement>(".dw-log-row");
            setFocusedLine(row ? lines.at(Number(row.dataset["position"]))?.id : undefined);
          }}
          onBlur={(e) => {
            if (!e.currentTarget.contains(e.relatedTarget)) setFocusedLine(undefined);
          }}
          onPointerOver={(e) => {
            if (!actions || e.pointerType === "touch") return;
            const row = (e.target as Element).closest<HTMLElement>(".dw-log-row");
            const line = row ? lines.at(Number(row.dataset["position"])) : undefined;
            if (line?.id !== pointedLine) setPointedLine(line?.id);
          }}
          onPointerLeave={() => setPointedLine(undefined)}
          role="log"
          // Each line isn't news: a log can write thousands a second. The status
          // above says when the stream starts and stops.
          aria-live="off"
          aria-labelledby={hasTitle ? titleId : undefined}
          // The reader scrolls the log with the keyboard too.
          // biome-ignore lint/a11y/noNoninteractiveTabindex: a scrolling region must be focusable
          tabIndex={0}
        >
          {top ? <div className="dw-log-top">{top}</div> : null}
          <div ref={rowsBox} className="dw-log-rows" style={{ height: index.current.total }}>
            {drawn}
          </div>
          {lines.length === 0 && hasContent(empty) ? <div className="dw-log-empty">{empty}</div> : null}
        </div>
        {live && !follow && newLines > 0 ? (
          <Button
            size="sm"
            variant="secondary"
            icon="chevron-down"
            className="dw-log-jump"
            onClick={() => setFollow(true)}
          >
            {newLines > MANY_LINES ? strings.logStream.manyNewLines : strings.logStream.newLines(newLines)}
          </Button>
        ) : null}
      </div>
      {foot === undefined ? null : <div className="dw-log-foot">{foot}</div>}
    </div>
  );
}

/** Words that change in a place as wide as the widest, so what is beside them stays put. */
function Held({ now, other }: { now: string; other: string }) {
  return (
    <span className="dw-held">
      <span>{now}</span>
      <span aria-hidden="true" className="dw-held-other">
        {other}
      </span>
    </span>
  );
}

function rowOf(node: Node | null): HTMLElement | null {
  const el = node instanceof Element ? node : (node?.parentElement ?? null);
  return el?.closest<HTMLElement>(".dw-log-row") ?? null;
}

function rowPosition(node: Node | null): number | undefined {
  const row = rowOf(node);
  return row ? Number(row.dataset["position"]) : undefined;
}

function closestText(node: Node | null): boolean {
  const el = node instanceof Element ? node : (node?.parentElement ?? null);
  return Boolean(el?.closest(".dw-log-text"));
}

/** How wide one character of the log's type is, in pixels. */
function charWidth(el: HTMLElement): number {
  const probe = document.createElement("span");
  probe.className = "dw-log-probe";
  probe.textContent = "0".repeat(32);
  el.appendChild(probe);
  const width = probe.getBoundingClientRect().width / 32;
  probe.remove();
  return width;
}

interface RowProps {
  line: LogLine;
  position: number;
  showSource: boolean;
  highlight: string;
  strings: UIStrings;
  open: boolean;
  onOpen: () => void;
  /** It carries on the line before, as a stack trace's frames do. */
  carries: boolean;
  /** The line before has the same time, from the same source. */
  sameTime: boolean;
  /** Its day, where that changes. */
  day: string | undefined;
  current: boolean;
  active: boolean;
  actions: ((line: LogLine, position: number) => ReactNode) | undefined;
}

/** One entry. It redraws only when what it shows changes, not with each batch of lines. */
const Row = memo(
  function Row({
    line,
    position,
    showSource,
    highlight,
    strings,
    open,
    onOpen,
    carries,
    sameTime,
    day,
    current,
    active,
    actions,
  }: RowProps) {
    const dayRow = day ? <div className="dw-log-day">{day}</div> : null;
    if (line.notice) {
      return (
        <>
          {dayRow}
          <div className={cx("dw-log-notice", `is-${line.notice}`)}>
            {line.notice === "warning" ? <Glyph shape={TONE.warning.shape} size={8} /> : null}
            <span>{line.text}</span>
          </div>
        </>
      );
    }
    const level = line.level ?? "info";
    const long = !open && line.text.length > LONG_LINE;
    const json = looksLikeJson(line.text);
    const pretty = open && json ? prettyJson(line.text) : undefined;
    const tools = actions?.(line, position);
    return (
      <>
        {dayRow}
        <div
          className={cx(
            "dw-log-line",
            `is-${level}`,
            carries && "is-carried",
            current && "is-current",
            active && "is-active",
          )}
        >
          <span className={cx("dw-log-time", sameTime && "is-repeat")} title={line.timeTitle}>
            {line.time}
          </span>
          {showSource ? (
            <span className={cx("dw-log-src", sameTime && "is-repeat")} title={line.sourceTitle ?? line.source}>
              {line.source}
            </span>
          ) : null}
          <span className="dw-log-lvl">
            {level === "info" || carries ? null : (
              <>
                <Glyph shape={LEVEL_SHAPE[level]} size={8} />
                <span className="dw-log-lvl-word">{strings.logStream.level[level]}</span>
              </>
            )}
          </span>
          <span className="dw-log-text">
            {pretty ? (
              <span className="dw-log-pretty">{pretty}</span>
            ) : (
              <LineText
                text={long ? line.text.slice(0, LONG_LINE) : line.text}
                highlight={highlight}
                current={current}
              />
            )}
            {long ? (
              <>
                <span aria-hidden="true">…</span>
                <button type="button" className="dw-log-more" onClick={onOpen}>
                  <Icon name="chevron-down" size={14} />
                  {strings.logStream.showAll}
                </button>
              </>
            ) : null}
            {json && !long ? (
              <button type="button" className="dw-log-more" aria-expanded={open} onClick={onOpen}>
                <Icon name={open ? "chevron-up" : "chevron-down"} size={14} />
                {open ? strings.logStream.collapse : strings.logStream.expand}
              </button>
            ) : null}
            {line.cut ? <span className="dw-log-cut"> {strings.logStream.cut(line.cut)}</span> : null}
          </span>
          {tools ? <span className="dw-log-actions">{tools}</span> : null}
        </div>
      </>
    );
  },
  (a, b) =>
    a.line === b.line &&
    a.position === b.position &&
    a.showSource === b.showSource &&
    a.highlight === b.highlight &&
    a.open === b.open &&
    a.strings === b.strings &&
    a.carries === b.carries &&
    a.sameTime === b.sameTime &&
    a.day === b.day &&
    a.current === b.current &&
    a.active === b.active &&
    a.actions === b.actions,
);

/** A line's text, with its colours and what a search found in it. */
function LineText({ text, highlight, current }: { text: string; highlight: string; current: boolean }) {
  const spans = parseAnsi(text);
  if (highlight === "") return <>{spans.map((span, i) => drawSpan(span, i, current))}</>;
  const parts = markHits(spans, highlight);
  return <>{parts.map((span, i) => drawSpan(span, i, current))}</>;
}

interface Mark extends LogSpan {
  hit?: boolean;
}

function drawSpan(span: Mark, i: number, current: boolean): ReactNode {
  const classes = cx(
    span.color && `dw-ansi-${span.color}`,
    span.bold && "dw-ansi-bold",
    span.dim && "dw-ansi-dim",
    span.underline && "dw-ansi-underline",
  );
  const content = span.hit ? <mark className={cx("dw-log-hit", current && "is-current")}>{span.text}</mark> : span.text;
  return classes ? (
    <span key={i} className={classes}>
      {content}
    </span>
  ) : (
    <Fragment key={i}>{content}</Fragment>
  );
}

/** Cuts runs of text around what a search found, whichever runs it falls across. */
export function markHits(spans: readonly LogSpan[], query: string): Mark[] {
  const plain = spans.map((s) => s.text).join("");
  const lower = plain.toLowerCase();
  const needle = query.toLowerCase();
  if (needle === "" || lower.length !== plain.length) {
    return lower.length !== plain.length ? markFold(spans, query) : [...spans];
  }
  const hits: [number, number][] = [];
  for (let at = lower.indexOf(needle); at >= 0; at = lower.indexOf(needle, at + needle.length)) {
    hits.push([at, at + needle.length]);
  }
  return cutAt(spans, hits);
}

/** The same, where changing case changes a string's length, as a few letters do. */
function markFold(spans: readonly LogSpan[], query: string): Mark[] {
  const plain = spans.map((s) => s.text).join("");
  const pattern = new RegExp(query.replace(/[.*+?^${}()|[\]\\]/g, "\\$&"), "giu");
  const hits: [number, number][] = [];
  for (const m of plain.matchAll(pattern)) {
    if (m[0].length > 0) hits.push([m.index, m.index + m[0].length]);
  }
  return cutAt(spans, hits);
}

function cutAt(spans: readonly LogSpan[], hits: readonly [number, number][]): Mark[] {
  if (hits.length === 0) return [...spans];
  const out: Mark[] = [];
  let at = 0;
  let h = 0;
  for (const span of spans) {
    let from = 0;
    const length = span.text.length;
    while (from < length) {
      const here = at + from;
      const hit = hits[h];
      if (!hit) {
        out.push({ ...span, text: span.text.slice(from) });
        break;
      }
      if (here < hit[0]) {
        const to = Math.min(length, hit[0] - at);
        out.push({ ...span, text: span.text.slice(from, to) });
        from = to;
      } else {
        const to = Math.min(length, hit[1] - at);
        out.push({ ...span, text: span.text.slice(from, to), hit: true });
        from = to;
        if (at + to >= hit[1]) h++;
      }
    }
    at += length;
  }
  return out;
}
