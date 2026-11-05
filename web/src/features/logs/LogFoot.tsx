// SPDX-License-Identifier: AGPL-3.0-only

import { LogGapReason } from "@dockwell/api";
import { Button } from "@dockwell/ui";
import { plural } from "@lingui/core/macro";
import { Trans, useLingui } from "@lingui/react/macro";
import { type ReactNode, useEffect, useRef, useState } from "react";

import { formatDay } from "../../lib/format";
import type { LogStore } from "./store";
import type { DeepSearch } from "./useDeepSearch";
import { clockMinutes, clockSeconds, roughly } from "./words";

/** Lines a second past which a rate is worth saying, and past which a reader can't read them as they go. */
const SAY_RATE = 20;
const TOO_FAST_TO_READ = 300;

/** Goes through what comes at most once a second, so counts that move fast stay readable. */
function useOncePerSecond(store: LogStore, on: boolean) {
  const [, tick] = useState(0);
  const [rate, setRate] = useState(0);
  const last = useRef({ received: store.received, at: Date.now() });
  useEffect(() => {
    if (!on) return undefined;
    last.current = { received: store.received, at: Date.now() };
    const timer = setInterval(() => {
      const now = Date.now();
      const { received, at } = last.current;
      const seconds = Math.max(0.001, (now - at) / 1000);
      setRate(Math.max(0, Math.round((store.received - received) / seconds)));
      last.current = { received: store.received, at: now };
      tick((n) => n + 1);
    }, 1000);
    return () => clearInterval(timer);
  }, [store, on]);
  return rate;
}

/** What goes between two sentences: a space, except in Chinese, whose full stop has room of its own. */
function sentenceGap(locale: string): string {
  return locale.startsWith("zh") ? "" : " ";
}

function moment(ms: number, locale: string): string {
  const clock = clockMinutes(ms);
  return new Date(ms).toDateString() === new Date().toDateString()
    ? clock
    : `${formatDay(new Date(ms), locale)}, ${clock}`;
}

/** How far a search of all the servers keep reached, and whether that was all. */
export function DeepWords({ store, deep }: { store: LogStore; deep: DeepSearch }) {
  const { i18n } = useLingui();
  const matches = store.matches;
  const count = matches.toLocaleString(i18n.locale);
  const lines = deep.lines.toLocaleString(i18n.locale);
  const at = deep.reached ? moment(deep.reached, i18n.locale) : "";
  const found = plural(matches, {
    one: `${count} match in ${lines} lines, back to ${at}.`,
    other: `${count} matches in ${lines} lines, back to ${at}.`,
  });
  const down = deep.servers.map((s) => s.serverName).join(", ");
  const gap = sentenceGap(i18n.locale);
  return (
    <>
      {found}
      {gap}
      {deep.timedOut ? (
        <Trans>
          The search stopped after 30 s, at {at}. Read older lines with <code>dockwell logs read</code>.
        </Trans>
      ) : deep.cutShort ? (
        <Trans>
          The search stopped at the newest 1,000,000 lines of a container, at {at}. Read older lines with{" "}
          <code>dockwell logs read</code>.
        </Trans>
      ) : down ? null : (
        <Trans>That is all the servers keep.</Trans>
      )}
      {gap}
      {down ? <Trans>{down} couldn't be searched, so its lines aren't in this.</Trans> : null}
    </>
  );
}

/** Why lines aren't shown, by the newest marker, as the one that is going on now. */
function SkipWords({ store, phone }: { store: LogStore; phone: boolean }) {
  const { i18n } = useLingui();
  const { value, rounded } = roughly(store.skipped);
  const count = value.toLocaleString(i18n.locale);
  const lead = rounded
    ? plural(value, { one: `About ${count} line not shown`, other: `About ${count} lines not shown` })
    : plural(value, { one: `${count} line not shown`, other: `${count} lines not shown` });
  if (phone) return <>{lead}.</>;
  const reason = store.lastSkip?.gap?.reason;
  return (
    <>
      {lead}
      {i18n.locale.startsWith("zh") ? "：" : ": "}
      {reason === LogGapReason.SHARED ? (
        <Trans>this server shares what it carries live between everyone reading it.</Trans>
      ) : reason === LogGapReason.READER_BEHIND ? (
        <Trans>this page or the network to it fell behind.</Trans>
      ) : (
        <Trans>this app wrote faster than a live view can carry.</Trans>
      )}
      {sentenceGap(i18n.locale)}
      <Trans>
        Read them with <code>dockwell logs read</code>.
      </Trans>
    </>
  );
}

/**
 * What is said under the log: how far a search reached, why lines are missing, how
 * fast they come. It goes through a fast log at most once a second, so it can be
 * read, and has a place of its own, so nothing moves when it says something.
 */
export function LogFoot({
  store,
  deep,
  query,
  stale,
  live,
  following,
  rangeLabel,
  rangeEnd,
  levelFiltered,
  phone,
  onPause,
  onLastSkip,
}: {
  store: LogStore;
  deep: DeepSearch | undefined;
  query: string;
  /** Typed in a search of all the servers keep, and not run yet. */
  stale: boolean;
  live: boolean;
  following: boolean;
  rangeLabel: string;
  rangeEnd: number;
  levelFiltered: boolean;
  phone: boolean;
  onPause: () => void;
  onLastSkip: () => void;
}) {
  const { t, i18n } = useLingui();
  const rate = useOncePerSecond(store, live);
  let words: ReactNode = null;
  if (stale) {
    words = <Trans>Press Enter to search all the servers keep.</Trans>;
  } else if (deep) {
    words = deep.loading || deep.error || deep.reached === 0 ? null : <DeepWords store={store} deep={deep} />;
  } else if (query) {
    const shown = store.matches.toLocaleString(i18n.locale);
    const total = store.total.toLocaleString(i18n.locale);
    words =
      store.matches === 0
        ? null
        : plural(store.matches, {
            one: `${shown} of the ${total} lines loaded matches.`,
            other: `${shown} of the ${total} lines loaded match.`,
          });
  } else if (live && !following) {
    const at = clockSeconds(store.heldSince);
    const count = store.arrived.toLocaleString(i18n.locale);
    words = plural(store.arrived, {
      one: `Paused at ${at}. ${count} line since.`,
      other: `Paused at ${at}. ${count} lines since.`,
    });
  } else if (rangeLabel) {
    const to = clockMinutes(rangeEnd);
    words = (
      <Trans>
        {rangeLabel}, to {to}. Refresh for newer lines.
      </Trans>
    );
  } else if (levelFiltered) {
    words = <Trans>Levels are read from each line's text. A line that names none counts as info.</Trans>;
  }
  const speed = roughly(rate).value.toLocaleString(i18n.locale);
  return (
    <>
      {words ? <span className="logs-foot-words">{words}</span> : null}
      {store.skipped > 0 ? (
        <span className="logs-foot-skip">
          <span className="logs-foot-words">
            <SkipWords store={store} phone={phone} />
          </span>
          <Button size="sm" variant="ghost" className="logs-foot-button" onClick={onLastSkip}>
            <Trans>Go to the last</Trans>
          </Button>
        </span>
      ) : null}
      {live && rate >= SAY_RATE ? (
        <span className="logs-foot-rate">
          <span>{t`${speed} lines/s`}</span>
          {following && rate >= TOO_FAST_TO_READ ? (
            <Button size="sm" variant="ghost" className="logs-foot-button" onClick={onPause}>
              <Trans>Pause to read</Trans>
            </Button>
          ) : null}
        </span>
      ) : null}
    </>
  );
}
