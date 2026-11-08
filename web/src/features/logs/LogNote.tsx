// SPDX-License-Identifier: AGPL-3.0-only

import { ConnectError } from "@connectrpc/connect";
import { type LogSource, ServerLogState, type ServerLogStatus } from "@dockwell/api";
import { Button, Callout, PHONE_QUERY, useMediaQuery } from "@dockwell/ui";
import { Trans, useLingui } from "@lingui/react/macro";
import { type ReactNode, useState } from "react";

import { sentence } from "../../lib/errors";
import { formatClock, formatDay } from "../../lib/format";
import type { StreamState } from "./useLogStream";
import { clockMinutes, type Deploy, deployRevision, instanceName } from "./words";

/**
 * A note on how the logs stand. A phone has little room for it, and what matters
 * is the log, so there it says its title, and the rest on asking.
 */
function LogCallout({
  tone,
  title,
  children,
}: {
  tone: "info" | "warning" | "critical";
  title: ReactNode;
  children?: ReactNode;
}) {
  const { t } = useLingui();
  const phone = useMediaQuery(PHONE_QUERY);
  const [open, setOpen] = useState(false);
  return (
    <Callout
      tone={tone}
      title={title}
      className="logs-note"
      actions={
        phone && children ? (
          <Button
            size="sm"
            variant="ghost"
            icon={open ? "chevron-up" : "chevron-down"}
            aria-label={open ? t`Show less` : t`Show more`}
            aria-expanded={open}
            onClick={() => setOpen(!open)}
          />
        ) : undefined
      }
    >
      {phone && !open ? undefined : children}
    </Callout>
  );
}

/** A time as a person acts on it: the time alone today, and the day before that. */
function when(ms: number, locale: string, now = Date.now()): string {
  const clock = clockMinutes(ms);
  if (new Date(ms).toDateString() === new Date(now).toDateString()) return clock;
  return `${formatDay(new Date(ms), locale, now)}, ${clock}`;
}

/** A server's message for a reader. It leaves a machine's name as written, whatever comes first in a sentence. */
function statusWords(s: ServerLogStatus): string {
  const lead = `${s.serverName} can't read logs now: `;
  const message = s.message.startsWith(lead) ? s.message.slice(lead.length) : s.message;
  if (message.startsWith(s.serverName)) return /[.!?]$/.test(message) ? message : `${message}.`;
  return sentence(new ConnectError(message));
}

/** At most one note on how the logs stand, what matters most first. */
export function LogNote({
  name,
  stream,
  down,
  stopped,
  logErrors,
  unbounded,
  outside,
  chosen,
  rolledBackTo,
  noCurrent,
  locale,
  onReadMissed,
}: {
  name: string;
  stream: StreamState;
  down: readonly ServerLogStatus[];
  stopped: readonly LogSource[];
  logErrors: readonly LogSource[];
  unbounded: readonly LogSource[];
  /** The logs are of a container Dockwell didn't make, which it leaves as it is. */
  outside: boolean;
  chosen: Deploy | undefined;
  rolledBackTo: string | undefined;
  noCurrent: boolean;
  locale: string;
  /** Reads what came while this tab was away for long, as a time range of its own. */
  onReadMissed?: () => void;
}) {
  const { t } = useLingui();
  const first = down[0];
  if (first) {
    return first.state === ServerLogState.OFFLINE ? (
      <LogCallout tone="critical" title={t`${first.serverName} is offline`}>
        <Trans>
          Its agent isn't connected, so its logs can't be read now. Its containers keep running and logging. Check the
          server. Lines written meanwhile arrive when it reconnects, as far as Docker kept them.
        </Trans>
      </LogCallout>
    ) : (
      <LogCallout tone="warning" title={t`${first.serverName} can't read logs`}>
        {statusWords(first)}
      </LogCallout>
    );
  }
  if (stream.status === "reconnecting") {
    return (
      <LogCallout tone="warning" title={t`Reconnecting to the control server`}>
        <Trans>The lines written meanwhile arrive once it answers, with none twice.</Trans>
      </LogCallout>
    );
  }
  if (stream.missed) {
    const from = formatClock(new Date(stream.missed.from), locale);
    const to = formatClock(new Date(stream.missed.to), locale);
    return (
      <LogCallout tone="info" title={t`This tab was away from ${from} to ${to}`}>
        <Trans>
          Rather than read all that came then through the network before showing anything, it started again from the
          newest lines.
        </Trans>{" "}
        {onReadMissed ? (
          <button type="button" className="logs-note-link" onClick={onReadMissed}>
            <Trans>Read what came then</Trans>
          </button>
        ) : null}
      </LogCallout>
    );
  }
  if (stream.catchingUp) {
    return (
      <LogCallout tone="info" title={t`Catching up`}>
        <Trans>Reading what came while this tab was away. It follows live again once it has.</Trans>
      </LogCallout>
    );
  }
  const failing = logErrors[0];
  if (failing) {
    return (
      <LogCallout tone="warning" title={t`${failing.serverName} can't read a container's log`}>
        {failing.logError}
      </LogCallout>
    );
  }
  if (chosen && !chosen.current)
    return <KeptNote chosen={chosen} rolledBackTo={rolledBackTo} noCurrent={noCurrent} name={name} locale={locale} />;
  const off = stopped[0];
  if (off) {
    const at = off.finishTime ? formatClock(new Date(Number(off.finishTime.seconds) * 1000), locale) : "";
    const code = off.exitCode;
    return (
      <LogCallout tone={code === 0 ? "info" : "warning"} title={t`${name} isn't running on ${off.serverName}`}>
        <Trans>
          Its container stopped at {at} with exit code {code}. These are its last lines, and new lines show once it
          starts again.
        </Trans>
      </LogCallout>
    );
  }
  const wild = unbounded[0];
  if (wild) {
    return (
      <LogCallout tone="warning" title={t`This container's log has no limit on ${wild.serverName}`}>
        {outside ? (
          <Trans>
            It was made without rotation, so its log can fill the disk. Dockwell didn't make it, so it leaves that as it
            is. Recreate it with a log limit, as <code>docker run --log-opt max-size=10m</code> sets one.
          </Trans>
        ) : (
          <Trans>
            It was made without rotation, so its log can fill the disk. Dockwell sets rotation on the containers it
            makes, so the next deploy of this app fixes it.
          </Trans>
        )}
      </LogCallout>
    );
  }
  return null;
}

/** What became of a deploy the server kept the logs of, with when, and why as far as it is known. */
function KeptNote({
  chosen,
  rolledBackTo,
  noCurrent,
  name,
  locale,
}: {
  chosen: Deploy;
  rolledBackTo: string | undefined;
  noCurrent: boolean;
  name: string;
  locale: string;
}) {
  const revision = deployRevision(chosen);
  const source = chosen.sources[0];
  const finished = source?.finishTime ? when(Number(source.finishTime.seconds) * 1000, locale) : "";
  const image = source?.image ?? "";
  const code = source?.exitCode ?? 0;
  const cut = chosen.sources.some((s) => s.cutShort);
  const ranAs = source ? instanceName(source) : "";
  const body = (
    <>
      {image ? (
        code !== 0 ? (
          <Trans>
            <code>{image}</code> exited with code {code}.
          </Trans>
        ) : (
          <Trans>
            <code>{image}</code> exited with no error.
          </Trans>
        )
      ) : null}
      {ranAs ? (
        <>
          {" "}
          <Trans>
            It ran as <code>{ranAs}</code>.
          </Trans>
        </>
      ) : null}
      {noCurrent ? (
        <p>
          <Trans>{name} has no container now, so these are the logs of its last deploy.</Trans>
        </p>
      ) : null}
      {cut ? (
        <p>
          <Trans>It wrote more than the server keeps of one container, so not all of its lines are here.</Trans>
        </p>
      ) : null}
    </>
  );
  if (!chosen.failed) {
    return (
      <LogCallout
        tone="info"
        title={
          <Trans>
            <code>{revision}</code> was replaced at {finished}.
          </Trans>
        }
      >
        {body}
      </LogCallout>
    );
  }
  return (
    <LogCallout
      tone="critical"
      title={
        rolledBackTo ? (
          <Trans>
            <code>{revision}</code> failed at {finished} and was rolled back to <code>{rolledBackTo}</code>.
          </Trans>
        ) : (
          <Trans>
            <code>{revision}</code> failed at {finished} and was rolled back.
          </Trans>
        )
      }
    >
      {body}
    </LogCallout>
  );
}
