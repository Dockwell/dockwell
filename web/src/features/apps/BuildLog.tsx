// SPDX-License-Identifier: AGPL-3.0-only

import type { BuildLine } from "@dockwell/api";
import { Callout, cx } from "@dockwell/ui";
import { plural } from "@lingui/core/macro";
import { useLingui } from "@lingui/react/macro";
import { useLayoutEffect, useRef } from "react";

import "./BuildLog.css";

/**
 * A build's log: each line, with the step it came from, the failing step's lines
 * marked, and where the log left lines out. It follows the end while it runs and
 * whoever reads it hasn't scrolled up.
 */
export function BuildLog({
  lines,
  leftOut,
  live,
  queued = false,
  failedStep,
  focusFailure = false,
  error,
  label,
  dropped = 0n,
}: {
  lines: readonly BuildLine[];
  leftOut: bigint;
  live: boolean;
  queued?: boolean;
  failedStep: number;
  focusFailure?: boolean;
  error?: string | undefined;
  label: string;
  /** How many lines its server dropped, as the build printed faster than its log could take them. */
  dropped?: bigint;
}) {
  const box = useRef<HTMLDivElement>(null);
  const following = useRef(true);
  const shownFailure = useRef<Element>(null);
  useLayoutEffect(() => {
    const el = box.current;
    if (!el) return;
    if (focusFailure) {
      const failed =
        el.querySelector(".is-failed.is-error") ?? el.querySelector(".is-error") ?? el.querySelector(".is-failed");
      if (failed && failed !== shownFailure.current) {
        shownFailure.current = failed;
        el.scrollTop += failed.getBoundingClientRect().top - el.getBoundingClientRect().top;
      }
    } else if (following.current) el.scrollTop = el.scrollHeight;
  });
  const { t, i18n } = useLingui();
  if (error)
    return (
      <Callout tone="critical" title={t`Can't read the build's log`}>
        {error}
      </Callout>
    );
  let last = 0n;
  return (
    <div
      ref={box}
      className="build-log"
      role="log"
      aria-label={label}
      aria-live={live ? "polite" : "off"}
      // biome-ignore lint/a11y/noNoninteractiveTabindex: a scrolling region has to take the keyboard
      tabIndex={0}
      onScroll={(event) => {
        const el = event.currentTarget;
        following.current = el.scrollHeight - el.scrollTop - el.clientHeight < 24;
      }}
    >
      {lines.length === 0 ? (
        <p className="build-log-empty">
          {queued
            ? t`Waiting its turn: a server runs one build at a time.`
            : live
              ? t`Waiting for the build's first line`
              : t`The build printed nothing.`}
        </p>
      ) : null}
      {lines.map((line) => {
        const gap = last > 0n && line.number > last + 1n ? line.number - last - 1n : 0n;
        last = line.number;
        const failed = failedStep > 0 && line.step === failedStep;
        const count = i18n.number(Number(gap || leftOut));
        return (
          <div key={line.number.toString()}>
            {gap > 0n ? <div className="build-log-gap">{t`${count} lines left out`}</div> : null}
            <div className={cx("build-log-line", failed && "is-failed", line.text.startsWith("ERROR ") && "is-error")}>
              <span className="build-log-step" aria-hidden="true">
                {line.step > 0 ? `#${line.step}` : ""}
              </span>
              <span className="build-log-text">{line.text}</span>
            </div>
          </div>
        );
      })}
      {dropped > 0n ? (
        <div className="build-log-gap">
          {plural(Number(dropped), {
            one: "# line dropped, as the build printed faster than its log could take it",
            other: "# lines dropped, as the build printed faster than its log could take them",
          })}
        </div>
      ) : null}
      {live ? <div className="build-log-cursor" aria-hidden="true" /> : null}
    </div>
  );
}
