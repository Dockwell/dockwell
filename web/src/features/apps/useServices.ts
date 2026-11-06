// SPDX-License-Identifier: AGPL-3.0-only

import { create } from "@bufbuild/protobuf";
import { createClient } from "@connectrpc/connect";
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
import { type QueryClient, type QueryKey, useQuery, useQueryClient } from "@tanstack/react-query";
import { useEffect, useMemo, useState } from "react";

import { isSessionEnded } from "../../lib/errors";
import { keepTrying } from "../../lib/retry";
import { readHealthSoon } from "./health";

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

/** How soon one screen's watch takes over from the last one's, moving between two that show services, so nothing went unseen. */
const HANDOVER_MS = 1_500;

/** When a screen last stopped watching, as what it would have seen goes unseen until the next one starts. */
const lastWatched = new WeakMap<QueryClient, number>();

/**
 * Whether the cached list may be older than something it should show: nothing
 * was watching since it was read, as when a deploy finished while another
 * screen was open. The list stays "fresh" for 30 s whatever happened, and a
 * first snapshot can't say what moved, so the list is read again. Moving
 * straight between two screens that show services reads nothing.
 */
function wentUnwatched(queryClient: QueryClient, list: QueryKey): boolean {
  // Not read yet: the read on its way is as new as the snapshot.
  const read = queryClient.getQueryState(list)?.dataUpdatedAt ?? 0;
  if (read === 0) return false;
  return Date.now() - Math.max(read, lastWatched.get(queryClient) ?? 0) > HANDOVER_MS;
}

/**
 * Keeps the services up to date while a screen shows them: a change that
 * applies, or a step that ends, reads them again. It watches the timeline for
 * changes alone and reads only when one moved on, so an idle screen reads
 * nothing, and a pull's progress, which only rewrites a step's message,
 * doesn't read either. It says when the watch is reconnecting, as what is
 * shown may be a little old until it is back, and catches up when it is.
 */
export function useServicesLive(): { reconnecting: boolean } {
  const transport = useTransport();
  const queryClient = useQueryClient();
  const [reconnecting, setReconnecting] = useState(false);
  useEffect(() => {
    const abort = new AbortController();
    const client = createClient(EventService, transport);
    const key = createConnectQueryKey({ schema: ServiceService, cardinality: undefined });
    const list = createConnectQueryKey({
      schema: ServiceService.method.listServices,
      input: {},
      transport,
      cardinality: "finite",
    });
    let timer: ReturnType<typeof setTimeout> | undefined;
    const read = () => {
      clearTimeout(timer);
      timer = setTimeout(() => {
        void queryClient.invalidateQueries({ queryKey: key });
        // What runs is what a change changes.
        readHealthSoon(queryClient);
      }, SETTLE_MS);
    };
    // Where each change stood when last seen: its state, and each step's.
    const seen = new Map<string, string>();
    let dropped = false;
    void keepTrying(
      abort.signal,
      BACKOFF,
      async (connected, attempt) => {
        for await (const msg of client.watchEvents({ kinds: [EventKind.CHANGE], pageSize: 1 }, { signal: attempt })) {
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
          // A first snapshot says nothing the first read didn't, unless that
          // read is older than the watch. After a drop, anything may have
          // happened.
          if (msg.snapshot ? dropped || wentUnwatched(queryClient, list) : moved) read();
          dropped = false;
        }
        // A stream that ends with no error, as when the control plane stops,
        // is as much a drop as one that fails.
        if (!abort.signal.aborted) {
          dropped = true;
          setReconnecting(true);
        }
        return undefined;
      },
      (err) => {
        if (isSessionEnded(err)) return "done";
        dropped = true;
        setReconnecting(true);
        return undefined;
      },
    );
    return () => {
      abort.abort();
      clearTimeout(timer);
      lastWatched.set(queryClient, Date.now());
    };
  }, [transport, queryClient]);
  return { reconnecting };
}

/** An environment by ID: its name and what to call it. */
export interface Place {
  id: string;
  /** Where it lives: its project's ID. */
  scope: string;
  name: string;
  /** How the panel shows it: its project's name, and its own once its project has a choice. */
  label: string;
  /** Its project's name, as a request names it. */
  project: string;
  /** In the hidden default project: the one named default, or the only one there is. */
  isDefault: boolean;
}

/** Where apps can live, and what to call each once there is a choice. */
export interface Places {
  environments: readonly Place[];
  /** More than one project (scope.Tree.Shown): an environment goes by its project. */
  severalProjects: boolean;
  /** A project has more than one environment (scope.Tree.Shown), so environments show. */
  showsEnvironment: boolean;
  /** Where an app goes unless someone says otherwise: the default project's first environment. */
  defaultEnvironment: Place | undefined;
  /** What to call an environment, by ID; empty until they load. */
  label(id: string): string;
  /** An environment by ID, or undefined until they load. */
  place(id: string): Place | undefined;
  /** The projects' names, as a request names them. */
  projects: readonly string[];
  /** The projects and environments are still on their way. */
  isPending: boolean;
}

/** The name of the project an install starts with (ADR 0014). */
const DEFAULT_PROJECT = "default";

/** The environments and projects, read once and shared with the command bar. */
export function usePlaces(): Places {
  const projectsQuery = useConnectQuery(ResourceService.method.listResources, { kind: "project" });
  const environmentsQuery = useConnectQuery(ResourceService.method.listResources, { kind: "environment" });
  const projects = projectsQuery.data?.resources;
  const environments = environmentsQuery.data?.resources;
  const isPending = projectsQuery.isPending || environmentsQuery.isPending;
  return useMemo(() => {
    const several = (projects?.length ?? 0) > 1;
    const inProject = new Map<string, number>();
    for (const env of environments ?? []) {
      const scope = env.resource?.scope ?? "";
      inProject.set(scope, (inProject.get(scope) ?? 0) + 1);
    }
    const shows = [...inProject.values()].some((count) => count > 1);
    const list: Place[] = (environments ?? []).map((env) => {
      const scope = env.resource?.scope ?? "";
      const name = env.resource?.name ?? env.id;
      const shown = env.displayName || name;
      const project = projects?.find((p) => p.id === scope);
      const projectName = project?.resource?.name ?? "";
      const projectLabel = project?.displayName || projectName;
      const own = (inProject.get(scope) ?? 0) > 1;
      const label = !several || !projectLabel ? shown : own ? `${projectLabel} / ${shown}` : projectLabel;
      return {
        id: env.id,
        scope,
        name,
        label,
        project: projectName,
        isDefault: !several || projectName === DEFAULT_PROJECT,
      };
    });
    const byId = new Map(list.map((e) => [e.id, e]));
    return {
      environments: list,
      severalProjects: several,
      showsEnvironment: shows,
      defaultEnvironment: list.find((e) => e.isDefault) ?? list[0],
      label: (id) => byId.get(id)?.label ?? "",
      place: (id) => byId.get(id),
      projects: (projects ?? []).map((p) => p.resource?.name ?? "").filter(Boolean),
      isPending,
    };
  }, [projects, environments, isPending]);
}
