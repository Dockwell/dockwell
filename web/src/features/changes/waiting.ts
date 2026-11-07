// SPDX-License-Identifier: AGPL-3.0-only

import { createQueryOptions, useQuery, useTransport } from "@connectrpc/connect-query";
import { type Change, ChangeService, ChangeState, type ResourceRef, Surface } from "@dockwell/api";
import { useQueries } from "@tanstack/react-query";
import { useMemo } from "react";

/** How often the newest changes are read again for what waits for a person, as a token can propose one whenever. */
const READ_EVERY_MS = 60_000;

/**
 * Whether a change waits for a person: a token proposed it, from the CLI or
 * the API, and its plan is one only a signed-in person may apply, as it
 * destroys data, may cut off access, or changes what a container can reach on
 * its server. The CLI stops at such a plan and prints a link to it, which only
 * the person who ran it saw. One proposed in the panel is someone's to review
 * there already.
 */
export function waitsForAPerson(change: Change): boolean {
  return (
    change.state === ChangeState.PLANNED &&
    change.proposedVia !== Surface.PANEL &&
    Boolean(change.plan?.destructive || change.plan?.hostAccess)
  );
}

/** The changes that wait for a person, newest first, of the newest page of changes. */
export function useWaitingForAPerson(): Change[] {
  const list = useQuery(
    ChangeService.method.listChanges,
    { pageSize: 50 },
    { refetchInterval: READ_EVERY_MS, refetchIntervalInBackground: false },
  );
  return useMemo(() => (list.data?.changes ?? []).filter(waitsForAPerson), [list.data]);
}

/**
 * The changes that wait for a person and touch any of refs, with their plans
 * in full, as a list has a plan only in brief.
 */
export function useWaitingFor(touches: (ref: ResourceRef) => boolean): Change[] {
  const transport = useTransport();
  const waiting = useWaitingForAPerson();
  const full = useQueries({
    queries: waiting.map((change) =>
      createQueryOptions(ChangeService.method.getChange, { id: change.id }, { transport }),
    ),
  });
  return waiting.filter((_, i) => {
    const plan = full[i]?.data?.change?.plan;
    return plan?.resources.some((r) => r.resource !== undefined && touches(r.resource)) ?? false;
  });
}
