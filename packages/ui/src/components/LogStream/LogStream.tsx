// SPDX-License-Identifier: AGPL-3.0-only

import { type ReactNode, useEffect, useId, useRef } from "react";

import { cx } from "../../lib/cx";
import { TONE } from "../../lib/status";
import { useUIStrings } from "../../lib/strings";
import { useControlled } from "../../lib/useControlled";
import { Button } from "../Button/Button";
import { Glyph } from "../Glyph/Glyph";
import "./LogStream.css";

export type LogLevel = "info" | "warn" | "error";

export interface LogLine {
  id?: string | number;
  /** Wall-clock time with milliseconds, in the viewer's time zone. */
  time: string;
  /** The replica or server the line came from. */
  source: string;
  level?: LogLevel;
  text: string;
}

export interface LogStreamProps {
  lines: readonly LogLine[];
  title?: ReactNode;
  /** While streaming: shows Live, or Paused once following stops. */
  live?: boolean;
  /** Whether new lines scroll into view. Following stops on its own when the reader scrolls up. */
  follow?: boolean;
  onFollowChange?: (follow: boolean) => void;
  /** Pixels. */
  height?: number;
  /** Filters and other controls, before the follow button. */
  toolbar?: ReactNode;
  className?: string;
}

const LEVEL_SHAPE = { warn: TONE.warning.shape, error: TONE.critical.shape } as const;

/** How close to the end, in pixels, still counts as reading the newest lines. */
const AT_END = 8;

/**
 * Log lines that follow new output and pause the moment someone scrolls up
 * to read. Warnings and errors carry a glyph in their own column, and errors
 * a faint critical band, so the level reads without colour.
 */
export function LogStream({
  lines,
  title,
  live = false,
  follow: followProp,
  onFollowChange,
  height = 240,
  toolbar,
  className,
}: LogStreamProps) {
  const strings = useUIStrings();
  const [follow, setFollow] = useControlled(followProp, true, onFollowChange);
  const body = useRef<HTMLDivElement>(null);
  const titleId = useId();
  const bodyId = useId();
  const hasTitle = title != null && title !== false;

  useEffect(() => {
    const el = body.current;
    // lines is a dependency so each new batch scrolls into view.
    if (follow && el && lines) el.scrollTop = el.scrollHeight;
  }, [lines, follow]);

  const onScroll = () => {
    const el = body.current;
    if (!el || !follow) return;
    if (el.scrollHeight - el.scrollTop - el.clientHeight >= AT_END) setFollow(false);
  };

  return (
    <div className={cx("dw-log", className)}>
      <div className="dw-log-head">
        <div className="dw-log-title">
          {live ? (
            <span className={cx("dw-live", !follow && "is-paused")} role="status">
              <span className="dw-live-dot" />
              {follow ? strings.logStream.live : strings.logStream.paused}
            </span>
          ) : null}
          {hasTitle ? <span id={titleId}>{title}</span> : null}
        </div>
        <div className="dw-log-tools">
          {toolbar}
          <Button
            size="sm"
            variant="ghost"
            icon={follow ? "pause" : "play"}
            aria-controls={bodyId}
            onClick={() => setFollow(!follow)}
          >
            {follow ? strings.logStream.pause : strings.logStream.follow}
          </Button>
        </div>
      </div>
      <div
        ref={body}
        id={bodyId}
        className="dw-log-body"
        style={{ height }}
        onScroll={onScroll}
        role="log"
        aria-live={follow ? "polite" : "off"}
        aria-labelledby={hasTitle ? titleId : undefined}
        // The reader scrolls the log with the keyboard too.
        // biome-ignore lint/a11y/noNoninteractiveTabindex: a scrolling region must be focusable
        tabIndex={0}
      >
        {lines.map((line, i) => {
          const level = line.level ?? "info";
          return (
            <div key={line.id ?? i} className={cx("dw-log-line", `is-${level}`)}>
              <span className="dw-log-time">{line.time}</span>
              <span className="dw-log-src" title={line.source}>
                {line.source}
              </span>
              <span className="dw-log-lvl">
                {level === "info" ? null : (
                  <>
                    <Glyph shape={LEVEL_SHAPE[level]} size={8} />
                    <span className="dw-sr">{strings.logStream.level[level]}</span>
                  </>
                )}
              </span>
              <span className="dw-log-text">{line.text}</span>
            </div>
          );
        })}
      </div>
    </div>
  );
}
