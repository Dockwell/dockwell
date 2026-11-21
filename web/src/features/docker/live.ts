// SPDX-License-Identifier: AGPL-3.0-only

import { timestampMs } from "@bufbuild/protobuf/wkt";
import { Code, ConnectError, createClient } from "@connectrpc/connect";
import { useTransport } from "@connectrpc/connect-query";
import {
  type DockerHealthUpdate,
  type DockerObject,
  type DockerObjectKey,
  DockerObjectKind,
  type DockerSample,
  DockerService,
} from "@dockwell/api";
import { useEffect, useRef, useState } from "react";

import { useAwayFor } from "../../lib/away";

const refreshInterval = 10_000;
const firstRetry = 5_000;

export function dockerKey(server: string, id: string): string {
  return `${server}/${id}`;
}

export function rowWithHealth(row: DockerObject, updates: ReadonlyMap<string, DockerHealthUpdate>): DockerObject {
  const key = row.key;
  const update = key && updates.get(dockerKey(key.serverId, key.engineId));
  if (
    row.detail.case !== "container" ||
    !update?.observedAt ||
    (row.observedAt && timestampMs(row.observedAt) > timestampMs(update.observedAt))
  ) {
    return row;
  }
  return { ...row, detail: { case: "container", value: { ...row.detail.value, health: update.health } } };
}

export function useDockerWatch(
  servers: readonly string[],
  keys: readonly DockerObjectKey[],
  refresh: () => void,
  kind = DockerObjectKind.CONTAINER,
) {
  const transport = useTransport();
  const [samples, setSamples] = useState<ReadonlyMap<string, DockerSample>>(new Map());
  const [health, setHealth] = useState<ReadonlyMap<string, DockerHealthUpdate>>(new Map());
  const [live, setLive] = useState<ReadonlyMap<string, boolean>>(new Map());
  const [attempt, retry] = useState(0);
  const refreshRef = useRef(refresh);
  const refreshed = useRef(0);
  refreshRef.current = refresh;

  // Over plain HTTP these streams share the browser's connection limit with
  // the shell and other tabs. Compose can catch up when its watch reopens.
  const away = useAwayFor(0);
  const serverKey = kind === DockerObjectKind.COMPOSE && away ? "" : servers.join(",");
  const visibleKey = keys
    .slice(0, 16)
    .map((key) => dockerKey(key.serverId, key.engineId))
    .sort()
    .join(",");
  // biome-ignore lint/correctness/useExhaustiveDependencies: Retry deliberately reopens the same subscription.
  useEffect(() => {
    if (!serverKey) return;
    const serverIds = serverKey.split(",");
    const visibleKeys = visibleKey
      ? visibleKey.split(",").map((key) => {
          const [serverId = "", engineId = ""] = key.split("/");
          return { serverId, engineId, kind: DockerObjectKind.CONTAINER };
        })
      : [];
    const alive = new Set(visibleKeys.map((key) => dockerKey(key.serverId, key.engineId)));
    const client = createClient(DockerService, transport);
    const abort = new AbortController();
    let reconnect: ReturnType<typeof setTimeout> | undefined;
    let pending: ReturnType<typeof setTimeout> | undefined;
    let backoff = firstRetry;

    const refreshInventory = () => {
      if (pending !== undefined) return;
      const apply = () => {
        pending = undefined;
        refreshed.current = Date.now();
        refreshRef.current();
      };
      const wait = refreshed.current + refreshInterval - Date.now();
      if (wait <= 0) apply();
      else pending = setTimeout(apply, wait);
    };

    const watch = async () => {
      let refused = false;
      try {
        for await (const update of client.watchDockerObjects(
          { selection: { serverIds, kind }, visibleKeys },
          { signal: abort.signal, timeoutMs: 0 },
        )) {
          if (abort.signal.aborted) return;
          if (update.watchStatus) {
            const { serverId, live: up } = update.watchStatus;
            setLive((current) => new Map(current).set(serverId, up));
            if (up) backoff = firstRetry;
            else
              setSamples((current) => new Map([...current].filter(([, sample]) => sample.key?.serverId !== serverId)));
          }
          if (
            update.resync ||
            update.changedKeys.length ||
            (kind === DockerObjectKind.COMPOSE && update.healthUpdates.length)
          ) {
            refreshInventory();
          }
          if (update.samples.length) {
            setSamples((current) => {
              const next = new Map([...current].filter(([key]) => alive.has(key)));
              for (const sample of update.samples) {
                if (sample.key) next.set(dockerKey(sample.key.serverId, sample.key.engineId), sample);
              }
              return next;
            });
          }
          if (update.healthUpdates.length && alive.size) {
            setHealth((current) => {
              const next = new Map([...current].filter(([key]) => alive.has(key)));
              for (const item of update.healthUpdates) {
                if (item.key) next.set(dockerKey(item.key.serverId, item.key.engineId), item);
              }
              return next;
            });
          }
        }
      } catch (err) {
        const code = ConnectError.from(err).code;
        refused = code === Code.PermissionDenied || code === Code.Unauthenticated;
      }
      if (abort.signal.aborted) return;
      setLive(new Map(serverIds.map((id) => [id, false])));
      setSamples(new Map());
      if (!refused) {
        reconnect = setTimeout(() => void watch(), backoff);
        backoff = Math.min(backoff * 2, 60_000);
      }
    };
    void watch();
    return () => {
      clearTimeout(reconnect);
      clearTimeout(pending);
      abort.abort();
    };
  }, [attempt, kind, serverKey, transport, visibleKey]);

  return {
    connected: servers.some((id) => live.get(id) === true),
    paused: servers.some((id) => live.get(id) === false),
    retry: () => retry((previous) => previous + 1),
    health,
    samples,
  };
}
