// SPDX-License-Identifier: AGPL-3.0-only

import { type Timestamp, timestampDate } from "@bufbuild/protobuf/wkt";
import { Code, ConnectError, createClient } from "@connectrpc/connect";
import { createConnectQueryKey, useQuery, useTransport } from "@connectrpc/connect-query";
import { type JoinTicket, ServerService } from "@dockwell/api";
import { Button, Glyph, Icon, STATUS, Tag } from "@dockwell/ui";
import { plural } from "@lingui/core/macro";
import { useLingui } from "@lingui/react/macro";
import { useId, useState } from "react";

import { sentence } from "../../lib/errors";
import { useNow } from "../../lib/time";
import { useMay } from "../auth/may";
import "./WaitingJoinTickets.css";

/** How often the list is read while the page is open, so a ticket used or expired elsewhere goes. */
const READ_EVERY_MS = 30_000;

/** The list's query key, for the Add server sheet to read it again once it makes or stops a ticket. */
export const waitingJoinTicketsKey = createConnectQueryKey({
  schema: ServerService.method.listJoinTickets,
  cardinality: undefined,
});

/** When a ticket expires, by the server's own word: the list leaves out what it says expired. */
function expiryOf(ticket: { expireTime?: Timestamp | undefined }): number {
  return ticket.expireTime ? timestampDate(ticket.expireTime).getTime() : 0;
}

/**
 * Whether a revoke that failed says the ticket no longer waits: a server joined with
 * it, or it's gone. Either way it's stopped as far as Stop goes, and a server that has
 * just joined is the success it looks like (#800's review, M1).
 */
function noLongerWaiting(err: unknown): boolean {
  const code = ConnectError.from(err).code;
  return code === Code.FailedPrecondition || code === Code.NotFound;
}

/**
 * The join commands still waiting for a server, as one quiet line on the Servers page
 * with Stop (#800): a command copied and left unused keeps working for its hour, and
 * this is where a person sees it and stops it. With several, the line opens into a row
 * each, with when it expires, its labels and its own Stop, beside Stop all. Shown to an
 * admin alone, and only while the server lists one: what waits is the server's to say,
 * by its own clock, and the browser's clock only counts down to it.
 */
export function WaitingJoinTickets() {
  const { t } = useLingui();
  const transport = useTransport();
  const may = useMay();
  const mayList = may(ServerService.method.listJoinTickets).allowed;
  const mayStop = may(ServerService.method.revokeJoinTicket);
  const list = useQuery(
    ServerService.method.listJoinTickets,
    {},
    { enabled: mayList, refetchInterval: READ_EVERY_MS, retry: false },
  );
  const now = useNow(15_000);
  const rowsId = useId();
  const [open, setOpen] = useState(false);
  const [stopping, setStopping] = useState<string>();
  const [failure, setFailure] = useState<{ count: number; why: string }>();
  const waiting = list.data?.tickets ?? [];
  if (!mayList || waiting.length === 0) return null;
  // How long until a time, in the largest whole unit: "52 min", "23 h", and never less than a minute.
  const left = (at: number) => {
    const ms = at - now;
    const hours = Math.floor(ms / 3_600_000);
    return hours >= 1 ? t`${hours} h` : t`${Math.max(1, Math.floor(ms / 60_000))} min`;
  };
  const first = left(Math.min(...waiting.map(expiryOf)));
  const n = waiting.length;
  const words =
    n === 1
      ? t`1 join command waiting · expires in ${first}`
      : plural(n, { other: `# join commands waiting · the first expires in ${first}` });
  // Stops each one given; one that joined or went meanwhile counts as stopped, and only the rest can fail.
  const stop = async (tickets: readonly JoinTicket[], key: string) => {
    setStopping(key);
    setFailure(undefined);
    const client = createClient(ServerService, transport);
    const results = await Promise.allSettled(tickets.map((ticket) => client.revokeJoinTicket({ id: ticket.id })));
    const failed = results.filter(
      (r): r is PromiseRejectedResult => r.status === "rejected" && !noLongerWaiting(r.reason),
    );
    if (failed.length > 0) setFailure({ count: failed.length, why: sentence(failed[0]?.reason) });
    await list.refetch();
    setStopping(undefined);
  };
  const failedCount = failure?.count ?? 0;
  const failedWhy = failure?.why ?? "";
  return (
    <section className="servers-waiting" aria-label={t`Join commands waiting`}>
      <span className="servers-waiting-line">
        <span className={`servers-waiting-glyph dw-tone-${STATUS.deploying.tone}`} aria-hidden="true">
          <Glyph shape={STATUS.deploying.shape} size={8} />
        </span>
        {n > 1 ? (
          <button
            type="button"
            className="servers-waiting-toggle"
            aria-expanded={open}
            aria-controls={rowsId}
            onClick={() => setOpen(!open)}
          >
            <span>{words}</span>
            <Icon name={open ? "chevron-up" : "chevron-down"} size={14} />
          </button>
        ) : (
          <span>{words}</span>
        )}
        <Button
          size="sm"
          variant="ghost"
          loading={stopping === "all"}
          disabled={stopping !== undefined && stopping !== "all"}
          onClick={() => void stop(waiting, "all")}
          {...mayStop.off}
        >
          {n === 1 ? t`Stop` : t`Stop all`}
        </Button>
      </span>
      {n > 1 && open ? (
        <ul id={rowsId} className="servers-waiting-rows" aria-label={t`Join commands waiting, one by one`}>
          {waiting.map((ticket) => {
            const keys = Object.keys(ticket.labels).sort();
            const expires = left(expiryOf(ticket));
            return (
              <li key={ticket.id} className="servers-waiting-row">
                <span className="servers-waiting-when">{t`Expires in ${expires}`}</span>
                {keys.length > 0 ? (
                  <span className="servers-waiting-labels">
                    {keys.map((k) => (
                      <Tag key={k} label={k} value={ticket.labels[k] || undefined} />
                    ))}
                  </span>
                ) : null}
                <Button
                  size="sm"
                  variant="ghost"
                  loading={stopping === ticket.id}
                  disabled={stopping !== undefined && stopping !== ticket.id}
                  aria-label={t`Stop the join command that expires in ${expires}`}
                  onClick={() => void stop([ticket], ticket.id)}
                  {...mayStop.off}
                >
                  {t`Stop`}
                </Button>
              </li>
            );
          })}
        </ul>
      ) : null}
      {/* Fact, consequence, fix (#800). */}
      {failure ? (
        <p className="servers-waiting-failed" role="alert">
          {plural(failedCount, {
            one: `Couldn't stop # join command: ${failedWhy} It works until it expires. Try Stop again.`,
            other: `Couldn't stop # join commands: ${failedWhy} They work until they expire. Try Stop again.`,
          })}
        </p>
      ) : null}
    </section>
  );
}
