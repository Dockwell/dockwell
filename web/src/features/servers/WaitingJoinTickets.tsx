// SPDX-License-Identifier: AGPL-3.0-only

import { type Timestamp, timestampDate } from "@bufbuild/protobuf/wkt";
import { createClient } from "@connectrpc/connect";
import { createConnectQueryKey, useQuery, useTransport } from "@connectrpc/connect-query";
import { ServerService } from "@dockwell/api";
import { Button, Glyph, STATUS } from "@dockwell/ui";
import { plural } from "@lingui/core/macro";
import { useLingui } from "@lingui/react/macro";
import { useState } from "react";

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

/**
 * The join commands still waiting for a server, as one quiet line on the Servers page
 * with Stop (#800): a command copied and left unused keeps working for its hour, and
 * this is where a person sees it and stops it. Shown to an admin alone, and only while
 * one waits.
 */
export function WaitingJoinTickets() {
  const { t } = useLingui();
  const transport = useTransport();
  const may = useMay()(ServerService.method.listJoinTickets).allowed;
  const list = useQuery(
    ServerService.method.listJoinTickets,
    {},
    { enabled: may, refetchInterval: READ_EVERY_MS, retry: false },
  );
  const now = useNow(15_000);
  const [stopping, setStopping] = useState(false);
  const [failure, setFailure] = useState<{ count: number; why: string }>();
  // One that expired since the list was read is gone already.
  const expiry = (ticket: { expireTime?: Timestamp | undefined }) =>
    ticket.expireTime ? timestampDate(ticket.expireTime).getTime() : 0;
  const waiting = (list.data?.tickets ?? []).filter((ticket) => expiry(ticket) > now);
  if (!may || waiting.length === 0) return null;
  const soonest = Math.min(...waiting.map(expiry));
  // How long until it expires, in the largest whole unit: "52 min", "23 h".
  const ms = soonest - now;
  const hours = Math.floor(ms / 3_600_000);
  const left = hours >= 1 ? t`${hours} h` : t`${Math.max(1, Math.floor(ms / 60_000))} min`;
  const n = waiting.length;
  const words =
    n === 1
      ? t`1 join command waiting · expires in ${left}`
      : plural(n, { other: `# join commands waiting · the first expires in ${left}` });
  const stop = async () => {
    setStopping(true);
    setFailure(undefined);
    const client = createClient(ServerService, transport);
    const results = await Promise.allSettled(waiting.map((ticket) => client.revokeJoinTicket({ id: ticket.id })));
    const failed = results.filter((r): r is PromiseRejectedResult => r.status === "rejected");
    if (failed.length > 0) setFailure({ count: failed.length, why: sentence(failed[0]?.reason) });
    await list.refetch();
    setStopping(false);
  };
  const failedCount = failure?.count ?? 0;
  const failedWhy = failure?.why ?? "";
  return (
    <section className="servers-waiting" aria-label={t`Join commands waiting`}>
      <span className="servers-waiting-line">
        <span className={`servers-waiting-glyph dw-tone-${STATUS.deploying.tone}`} aria-hidden="true">
          <Glyph shape={STATUS.deploying.shape} size={8} />
        </span>
        <span>{words}</span>
        <Button size="sm" variant="ghost" loading={stopping} onClick={() => void stop()}>
          {n === 1 ? t`Stop` : t`Stop all`}
        </Button>
      </span>
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
