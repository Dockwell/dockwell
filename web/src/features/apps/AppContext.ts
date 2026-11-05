// SPDX-License-Identifier: AGPL-3.0-only

import { useInfiniteQuery } from "@connectrpc/connect-query";
import { type ServiceInfo, type ServiceRevision, ServiceService } from "@dockwell/api";
import { createContext, useContext, useMemo } from "react";

import type { AppState } from "./app";
import type { AppHealth } from "./health";

/** The app a tab of /apps/<id> shows, which its frame has read. */
export interface AppValue {
  service: ServiceInfo;
  /** What was deployed, and how that went. */
  state: AppState;
  /** How it runs now. */
  health: AppHealth;
  /** Every service, for what it runs with. */
  services: readonly ServiceInfo[];
}

export const AppContext = createContext<AppValue | undefined>(undefined);

/** The app the tab shows. Only the tabs under AppLayout may ask. */
export function useApp(): AppValue {
  const value = useContext(AppContext);
  if (!value) throw new Error("useApp is for the tabs under AppLayout");
  return value;
}

const PAGE_SIZE = 20;

/**
 * An app's revisions, newest first, a page at a time. The frame and its tabs
 * share them, and useServicesLive reads them again as changes move on, as
 * they are ServiceService's too.
 */
export function useRevisions(id: string) {
  const query = useInfiniteQuery(
    ServiceService.method.listServiceRevisions,
    { id, pageSize: PAGE_SIZE, pageToken: "" },
    { pageParamKey: "pageToken", getNextPageParam: (page) => page.nextPageToken || undefined },
  );
  const revisions = useMemo<ServiceRevision[] | undefined>(
    () => query.data?.pages.flatMap((page) => page.revisions),
    [query.data],
  );
  return { ...query, revisions };
}
