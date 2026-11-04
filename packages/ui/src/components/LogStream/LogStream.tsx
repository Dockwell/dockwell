// SPDX-License-Identifier: AGPL-3.0-only

import {
  Fragment,
  memo,
  type ReactNode,
  useCallback,
  useEffect,
  useId,
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
import { type LogSpan, parseAnsi } from "./ansi";
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
  /** The position of a line by its ID, if it is here, to keep a reader's place. */
  positionOf?(id: string | number): number | undefined;
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
  const expected = useRef<number | undefined>(undefined);
  const lastTop = useRef(0);
  const [view, setView] = useState({ top: 0, height: 0 });
  const rows = useRef(new Map<number, HTMLDivElement>());
  const [expanded, setExpanded] = useState<ReadonlySet<string | number>>(new Set());

  const estimate = useCallback((line: LogLine | undefined) => {
    if (!line) return ROW;
    const wrap =
      columns.current > 0 ? Math.max(1, Math.ceil(Math.min(line.text.length, LONG_LINE) / columns.current)) : 1;
    return wrap * ROW + (phone.current && !line.notice ? PHONE_EXTRA : 0);
  }, []);

  // Keeps the heights in step with the lines. It only moves forward with their
  // version, so running it twice for one render changes nothing.
  const end = lines.start + lines.length;
  if (synced.current.epoch !== lines.epoch) {
    // Positions mean something else now. A reader who isn't following keeps
    // their place, by the line at the top of what shows.
    const firstShown = lines.length > 0 && synced.current.epoch >= 0 ? anchor.current : undefined;
    if (synced.current.epoch >= 0) settledAt.current = performance.now() + SETTLE_MS;
    index.current.reset(lines.start);
    for (let p = lines.start; p < end; p++) index.current.push(estimate(lines.at(p)));
    synced.current = { epoch: lines.epoch, end };
    if (firstShown) {
      const position = lines.positionOf?.(firstShown.id);
      if (position !== undefined) pendingTop.current = index.current.offset(position) + firstShown.into;
    }
  } else {
    if (lines.start > index.current.first) {
      const gone = index.current.dropTo(lines.start);
      adjust.current -= gone;
    }
    for (let p = synced.current.end; p < end; p++) index.current.push(estimate(lines.at(p)));
    synced.current.end = end;
  }

  const fits = view.height > 0;
  const first = fits ? Math.max(lines.start, index.current.at(view.top) - OVERSCAN) : lines.start;
  const last = fits
    ? Math.min(end, index.current.at(view.top + view.height) + OVERSCAN + 1)
    : Math.min(end, lines.start + NO_LAYOUT_ROWS);

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
    if (follow && lines.length > 0) {
      el.scrollTop = el.scrollHeight;
      expected.current = el.scrollTop;
      pendingTop.current = undefined;
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
    const atTop = index.current.at(el.scrollTop);
    const line = lines.at(atTop);
    if (line?.id !== undefined) anchor.current = { id: line.id, into: el.scrollTop - index.current.offset(atTop) };
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
    if (!ours && follow && now < lastTop.current && el.scrollHeight - now - el.clientHeight >= AT_END) {
      if (settling) el.scrollTop = el.scrollHeight;
      else setFollow(false);
    }
    lastTop.current = now;
    if (scrolling.current === 0) {
      scrolling.current = requestAnimationFrame(() => {
        scrolling.current = 0;
        onHeights();
      });
    }
    const atTop = index.current.at(now);
    const line = lines.at(atTop);
    if (line?.id !== undefined) anchor.current = { id: line.id, into: now - index.current.offset(atTop) };
  };
  useEffect(() => () => cancelAnimationFrame(scrolling.current), []);

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

  // How many have come since following stopped.
  const pausedAt = useRef(end);
  if (follow) pausedAt.current = end;
  const newLines = Math.max(0, end - pausedAt.current);

  const drawn: ReactNode[] = [];
  const nextRows = new Map<number, HTMLDivElement>();
  rows.current = nextRows;
  for (let p = first; p < last; p++) {
    const line = lines.at(p);
    if (!line) continue;
    const key = line.id ?? p;
    drawn.push(
      <div
        key={key}
        className="dw-log-row"
        style={{ transform: `translateY(${index.current.offset(p)}px)` }}
        ref={(el) => {
          if (el) nextRows.set(p, el);
        }}
      >
        <Row
          line={line}
          showSource={showSource}
          highlight={highlight}
          strings={strings}
          open={expanded.has(key)}
          onOpen={() => setExpanded((was) => new Set(was).add(key))}
        />
      </div>,
    );
  }

  return (
    <div className={cx("dw-log", height === "fill" && "is-fill", !showSource && "no-source", className)}>
      <div className="dw-log-head">
        <div className="dw-log-title">
          {live ? (
            <span className={cx("dw-live", !follow && "is-paused")} role="status">
              <span className="dw-live-dot" />
              {follow ? strings.logStream.live : strings.logStream.paused}
            </span>
          ) : null}
          {hasTitle ? (
            <span id={titleId} className="dw-log-name">
              {title}
            </span>
          ) : null}
        </div>
        <div className="dw-log-tools">
          {toolbar}
          {live ? (
            <Button
              size="sm"
              variant="ghost"
              icon={follow ? "pause" : "play"}
              aria-controls={bodyId}
              onClick={() => setFollow(!follow)}
            >
              {follow ? strings.logStream.pause : strings.logStream.follow}
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
          onPointerDown={(e) => {
            // The scrollbar itself, whose scroll events come too late to hold it.
            if (e.target === e.currentTarget) reading();
          }}
          onMouseUp={() => {
            // Text chosen to copy would go as new lines push it out.
            const chosen = document.getSelection();
            if (chosen && !chosen.isCollapsed && body.current?.contains(chosen.anchorNode)) reading();
          }}
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
          <div className="dw-log-rows" style={{ height: index.current.total }}>
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
            {strings.logStream.newLines(newLines)}
          </Button>
        ) : null}
      </div>
    </div>
  );
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
  showSource: boolean;
  highlight: string;
  strings: UIStrings;
  open: boolean;
  onOpen: () => void;
}

/** One entry. It redraws only when what it shows changes, not with each batch of lines. */
const Row = memo(
  function Row({ line, showSource, highlight, strings, open, onOpen }: RowProps) {
    if (line.notice) {
      return (
        <div className={cx("dw-log-notice", `is-${line.notice}`)}>
          {line.notice === "warning" ? <Glyph shape={TONE.warning.shape} size={8} /> : null}
          <span>{line.text}</span>
        </div>
      );
    }
    const level = line.level ?? "info";
    const long = !open && line.text.length > LONG_LINE;
    return (
      <div className={cx("dw-log-line", `is-${level}`)}>
        <span className="dw-log-time" title={line.timeTitle}>
          {line.time}
        </span>
        {showSource ? (
          <span className="dw-log-src" title={line.sourceTitle ?? line.source}>
            {line.source}
          </span>
        ) : null}
        <span className="dw-log-lvl">
          {level === "info" ? null : (
            <>
              <Glyph shape={LEVEL_SHAPE[level]} size={8} />
              <span className="dw-log-lvl-word">{strings.logStream.level[level]}</span>
            </>
          )}
        </span>
        <span className="dw-log-text">
          <LineText text={long ? line.text.slice(0, LONG_LINE) : line.text} highlight={highlight} />
          {long ? (
            <button type="button" className="dw-log-more" onClick={onOpen}>
              <Icon name="chevron-down" size={14} />
              {strings.logStream.showAll}
            </button>
          ) : null}
          {line.cut ? <span className="dw-log-cut"> {strings.logStream.cut(line.cut)}</span> : null}
        </span>
      </div>
    );
  },
  (a, b) =>
    a.line === b.line &&
    a.showSource === b.showSource &&
    a.highlight === b.highlight &&
    a.open === b.open &&
    a.strings === b.strings,
);

/** A line's text, with its colours and what a search found in it. */
function LineText({ text, highlight }: { text: string; highlight: string }) {
  const spans = parseAnsi(text);
  if (highlight === "") return <>{spans.map(drawSpan)}</>;
  const parts = markHits(spans, highlight);
  return <>{parts.map(drawSpan)}</>;
}

interface Mark extends LogSpan {
  hit?: boolean;
}

function drawSpan(span: Mark, i: number): ReactNode {
  const classes = cx(
    span.color && `dw-ansi-${span.color}`,
    span.bold && "dw-ansi-bold",
    span.dim && "dw-ansi-dim",
    span.underline && "dw-ansi-underline",
  );
  const content = span.hit ? <mark className="dw-log-hit">{span.text}</mark> : span.text;
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
