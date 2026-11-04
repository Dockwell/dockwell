// SPDX-License-Identifier: AGPL-3.0-only

import { create } from "@bufbuild/protobuf";
import { ConnectError, createClient } from "@connectrpc/connect";
import { createConnectQueryKey, useQuery as useConnectQuery, useTransport } from "@connectrpc/connect-query";
import {
  ChangeState,
  EventKind,
  EventService,
  ListServicesResponseSchema,
  ResourceService,
  type ServiceInfo,
  ServiceService,
} from "@dockwell/api";
import { useQuery, useQueryClient } from "@tanstack/react-query";
import { useEffect, useMemo, useState } from "react";

import { isSessionEnded } from "../../lib/errors";
import { keepTrying } from "../../lib/retry";

/** The most ListServices returns at once. */
const PAGE_SIZE = 1000;

/**
 * Every service, read a page at a time and kept as one list, under
 * ListServices' own query key, so a change applied in the plan sheet reads it
 * again. Each screen and the command bar share it: moving between them reads
 * nothing.
 */
export function useServices() {
  const transport = useTransport();
  return useQuery({
    queryKey: createConnectQueryKey({
      schema: ServiceService.method.listServices,
      input: {},
      transport,
      cardinality: "finite",
    }),
    queryFn: async ({ signal }) => {
      const client = createClient(ServiceService, transport);
      const all = create(ListServicesResponseSchema);
      let pageToken = "";
      do {
        const page = await client.listServices({ pageSize: PAGE_SIZE, pageToken }, { signal });
        all.services.push(...page.services);
        pageToken = page.nextPageToken;
      } while (pageToken !== "");
      return all;
    },
    select: selectServices,
  });
}

function selectServices(res: { services: ServiceInfo[] }): ServiceInfo[] {
  return res.services;
}

const BACKOFF = { minMs: 1_000, maxMs: 15_000 };

/** How long a burst of moves waits before the list is read again, so it is read once for them. */
const SETTLE_MS = 150;

/**
 * Keeps the services up to date while a screen shows them: a change that
 * applies, or a step that ends, reads them again. It watches the timeline for
 * changes alone and reads only when one moved on, so an idle screen reads
 * nothing, and a pull's progress, which only rewrites a step's message,
 * doesn't read either. It says when the watch is reconnecting, as what is
 * shown may be a little old until it is back.
 */
export function useServicesLive(): { reconnecting: boolean } {
  const transport = useTransport();
  const queryClient = useQueryClient();
  const [reconnecting, setReconnecting] = useState(false);
  useEffect(() => {
    const abort = new AbortController();
    const client = createClient(EventService, transport);
    const key = createConnectQueryKey({ schema: ServiceService, cardinality: undefined });
    let timer: ReturnType<typeof setTimeout> | undefined;
    const read = () => {
      clearTimeout(timer);
      timer = setTimeout(() => void queryClient.invalidateQueries({ queryKey: key }), SETTLE_MS);
    };
    // Where each change stood when last seen: its state, and each step's.
    const seen = new Map<string, string>();
    let dropped = false;
    void keepTrying(
      abort.signal,
      BACKOFF,
      async (connected) => {
        for await (const msg of client.watchEvents(
          { kinds: [EventKind.CHANGE], pageSize: 1 },
          { signal: abort.signal },
        )) {
          connected();
          setReconnecting(false);
          let moved = false;
          for (const event of msg.events) {
            const change = event.change;
            // A plan someone is still reviewing changes nothing that runs.
            if (!change || change.state === ChangeState.PLANNED) continue;
            const mark = `${change.state}/${change.steps.map((s) => s.state).join(",")}`;
            if (seen.get(change.id) !== mark) moved = true;
            seen.set(change.id, mark);
          }
          // The first snapshot says nothing the first read didn't. After a
          // drop, anything may have happened.
          if (msg.snapshot ? dropped : moved) read();
          dropped = false;
        }
        return undefined;
      },
      (err) => {
        if (isSessionEnded(err)) return "done";
        dropped = true;
        ConnectError.from(err);
        setReconnecting(true);
        return undefined;
      },
    );
    return () => {
      abort.abort();
      clearTimeout(timer);
    };
  }, [transport, queryClient]);
  return { reconnecting };
}

/** One environment or project by ID: its name and what to call it. */
export interface Place {
  id: string;
  /** Where it lives: its project's ID, for an environment. */
  scope: string;
  name: string;
  /** How the panel shows it. */
  label: string;
}

/** Where apps can live, and what to call each once there is a choice. */
export interface Places {
  environments: readonly Place[];
  /** More than one project: an environment goes by project / environment. */
  severalProjects: boolean;
  /** What to call an environment, by ID; empty until they load. */
  label(id: string): string;
}

/** The environments and projects, read once and shared with the command bar. */
export function usePlaces(): Places {
  const projects = useConnectQuery(ResourceService.method.listResources, { kind: "project" }).data?.resources;
  const environments = useConnectQuery(ResourceService.method.listResources, { kind: "environment" }).data?.resources;
  return useMemo(() => {
    const several = (projects?.length ?? 0) > 1;
    const list = (environments ?? []).map((env) => {
      const scope = env.resource?.scope ?? "";
      const name = env.resource?.name ?? env.id;
      const shown = env.displayName || name;
      const project = projects?.find((p) => p.id === scope);
      const projectName = project?.displayName || project?.resource?.name || "";
      return { id: env.id, scope, name, label: several && projectName ? `${projectName} / ${shown}` : shown };
    });
    const labels = new Map(list.map((e) => [e.id, e.label]));
    return { environments: list, severalProjects: several, label: (id) => labels.get(id) ?? "" };
  }, [projects, environments]);
}
