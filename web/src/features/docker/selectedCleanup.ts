// SPDX-License-Identifier: AGPL-3.0-only

import { createClient } from "@connectrpc/connect";
import { useTransport } from "@connectrpc/connect-query";
import {
  ChangeState,
  DockerCleanupKind,
  DockerCleanupReason,
  type DockerCleanupTarget,
  DockerService,
  type PreviewDockerCleanupResponse,
} from "@dockwell/api";
import { useLingui } from "@lingui/react/macro";
import { useQuery } from "@tanstack/react-query";
import { useState } from "react";

import { reasonOf } from "../../lib/errors";
import { useChangeSheet } from "../changes/ChangeSheet";
import { useDockerArtifactWords } from "./artifactWords";
import { dockerKey } from "./live";

type Target = { serverId: string; engineId: string };
const keyOf = (target: Target) => dockerKey(target.serverId, target.engineId);

/** Review exact inventory rows through the same evidence and limits as Remove unused. */
export function useSelectedCleanup(kind: DockerCleanupKind, selected: readonly Target[], allowed: boolean) {
  const { t } = useLingui();
  const transport = useTransport();
  const sheet = useChangeSheet();
  const words = useDockerArtifactWords();
  const [menuTarget, setMenuTarget] = useState<Target>();
  const client = createClient(DockerService, transport);
  const filters = {
    kinds: [kind],
    taggedImages: kind === DockerCleanupKind.IMAGE,
    volumes: kind === DockerCleanupKind.VOLUME,
    networks: kind === DockerCleanupKind.NETWORK,
  };
  const changed = () => new Error(t`Docker inventory changed. Nothing was deleted. Review again.`);
  const unread = t`Docker couldn't verify that this can be deleted`;
  const missing =
    kind === DockerCleanupKind.VOLUME
      ? t`This volume is gone or has been replaced`
      : t`This Docker item is no longer in the cleanup inventory`;
  const exclusion = (target: DockerCleanupTarget) => {
    if (target.reason === DockerCleanupReason.SWARM_MANAGER) return t`Docker Swarm manages volumes on this server`;
    if (target.reason === DockerCleanupReason.EXACT_REMOVAL_UNAVAILABLE) {
      if (kind === DockerCleanupKind.VOLUME) return t`Only local volumes with a verified identity can be deleted`;
      if (kind === DockerCleanupKind.NETWORK) return t`Only local networks can be deleted`;
    }
    if (target.reason === DockerCleanupReason.IN_USE) {
      if (kind === DockerCleanupKind.VOLUME) return t`Containers still use this volume`;
      if (kind === DockerCleanupKind.NETWORK) return t`Containers still use this network`;
      return t`Containers still use this image`;
    }
    return words.reason(target.reason, kind, target.composeProject);
  };
  const read = async (rows: readonly Target[], signal?: AbortSignal) => {
    const serverIds = [...new Set(rows.map((row) => row.serverId))].sort();
    const remaining = new Set(rows.map(keyOf));
    const candidates = new Map<string, DockerCleanupTarget>();
    const excluded = new Map<string, DockerCleanupTarget>();
    let first: PreviewDockerCleanupResponse | undefined;
    let pageToken = "";
    do {
      const page = await client.previewDockerCleanup({ serverIds, filters, pageToken, pageSize: 100 }, { signal });
      first ??= page;
      if (page.snapshot !== first.snapshot) throw changed();
      for (const target of page.candidates) {
        if (target.kind === kind && remaining.delete(keyOf(target))) candidates.set(keyOf(target), target);
      }
      for (const target of page.exclusions) {
        if (target.kind === kind && remaining.delete(keyOf(target))) excluded.set(keyOf(target), target);
      }
      pageToken = page.nextPageToken;
    } while (remaining.size > 0 && pageToken);
    return { ...first, serverIds, candidates, excluded };
  };
  type Preview = Awaited<ReturnType<typeof read>>;
  const whyMissing = (preview: Preview, row: Target) => {
    if (preview.unavailable.some((server) => server.serverId === row.serverId)) return unread;
    const limit = preview.serverLimits.find((server) => server.serverId === row.serverId);
    if (limit && !limit.supportedKinds.includes(kind)) return t`Update the agent to delete this Docker item`;
    return missing;
  };
  const select = (preview: Preview, rows: readonly Target[]) => {
    const targets: DockerCleanupTarget[] = [];
    const omitted = new Map<string, string>();
    const countByServer = new Map<string, number>();
    const limits = new Map(preview.serverLimits.map((server) => [server.serverId, server.maxTargets]));
    for (const row of rows) {
      // A volume key carries its generation. A matching name alone is never enough.
      const key = keyOf(row);
      const target = preview.candidates.get(key);
      const excluded = preview.excluded.get(key);
      const count = countByServer.get(row.serverId) ?? 0;
      const limit = limits.get(row.serverId);
      if (!target) omitted.set(key, excluded ? exclusion(excluded) : whyMissing(preview, row));
      else if ((limit && count >= limit) || (preview.maxTargetsTotal && targets.length >= preview.maxTargetsTotal))
        omitted.set(key, t`This plan reached the cleanup limit`);
      else {
        targets.push(target);
        countByServer.set(row.serverId, count + 1);
      }
    }
    return { targets, omitted };
  };
  const rows = menuTarget ? [...selected, menuTarget] : selected;
  const lookup = useQuery({
    queryKey: [
      "connect-query",
      {
        serviceName: DockerService.typeName,
        methodName: "RowCleanup",
        input: [kind, [...new Set(rows.map(keyOf))].sort()],
      },
    ],
    enabled: allowed && rows.length > 0,
    queryFn: ({ signal }) => read(rows, signal),
    retry: false,
    refetchInterval: (query) =>
      query.state.data?.unavailable.length ||
      [...(query.state.data?.excluded.values() ?? [])].some(
        ({ reason }) => reason === DockerCleanupReason.RECENTLY_CREATED,
      )
        ? 5_000
        : false,
  });
  const status = (row: Target): { reason: string } | { pending: true } | undefined => {
    const preview = lookup.data;
    if (lookup.isError) return { reason: unread };
    if (!preview) return { pending: true };
    if (preview.candidates.has(keyOf(row))) return undefined;
    const target = preview.excluded.get(keyOf(row));
    return { reason: target ? exclusion(target) : whyMissing(preview, row) };
  };
  const omitted = lookup.data ? select(lookup.data, selected).omitted : undefined;
  const batchStatus = (row: Target) => {
    const reason = omitted?.get(keyOf(row));
    return reason ? { reason } : status(row);
  };
  const rowOff = (row: Target) => {
    const no = status(row);
    return no && ("reason" in no ? { off: no.reason } : { disabled: true });
  };

  const remove = (rows: readonly Target[], onApplied: () => void, priorNotes: readonly string[] = []) => {
    if (rows.length === 0) return;
    sheet.open({
      kind: "plan",
      retryLabel: t`Review again`,
      make: async () => {
        try {
          const preview = await read(rows);
          const { targets, omitted } = select(preview, rows);
          const skipped = new Map<string, number>();
          for (const reason of omitted.values()) skipped.set(reason, (skipped.get(reason) ?? 0) + 1);
          const notes = [...priorNotes, ...[...skipped].map(([reason, count]) => t`${count} skipped (${reason})`)];
          if (targets.length === 0) {
            const detail = notes.join(" · ");
            throw new Error(t`${detail}. Nothing was deleted. Review again.`);
          }
          const result = await client.planDockerMaintenance({
            serverIds: preview.serverIds,
            expectedSnapshot: preview.snapshot,
            operation: { case: "cleanup", value: { filters, targets } },
          });
          return { ...result, notes: [...new Set(notes)] };
        } catch (error) {
          if (reasonOf(error) === "docker_cleanup_changed") throw changed();
          throw error;
        }
      },
      onClose: (change) => {
        if (change?.state === ChangeState.APPLIED) onApplied();
      },
    });
  };
  const refresh = () => {
    if (allowed && rows.length > 0) void lookup.refetch();
  };
  return {
    remove,
    rowOff,
    batchStatus,
    checkMenu: (target: Target) => {
      if (menuTarget && keyOf(menuTarget) === keyOf(target)) refresh();
      setMenuTarget(target);
    },
    refresh,
  };
}
