// SPDX-License-Identifier: AGPL-3.0-only

import { type ConnectError, createClient } from "@connectrpc/connect";
import { useTransport } from "@connectrpc/connect-query";
import { ServerService, type SystemServiceInfo, type SystemServiceList, SystemServiceState } from "@dockwell/api";
import type { Status } from "@dockwell/ui";
import type { MessageDescriptor } from "@lingui/core";
import { msg } from "@lingui/core/macro";
import { useEffect, useState } from "react";

import { isSessionEnded, reasonOf } from "../../lib/errors";
import { keepTrying } from "../../lib/retry";

/** How often the control plane reads the list again, as internal/control/servers has it. */
export const SYSTEM_EVERY_MS = 10_000;

const BACKOFF = { minMs: 1_000, maxMs: 30_000 };

export interface SystemServicesState {
  /** The latest list, while there is one. */
  list: SystemServiceList | undefined;
  /** Why the watch failed last, until it hears again. */
  error: ConnectError | undefined;
}

/**
 * A server's system services, as its agent reads them every 10 s while this is
 * open (ADR 0067). The watch reconnects after a failure, and stops for good on one
 * trying again won't fix: an agent too old to list them, or a session that ended.
 */
export function useSystemServices(serverId: string, enabled = true): SystemServicesState {
  const transport = useTransport();
  const [state, setState] = useState<SystemServicesState>({ list: undefined, error: undefined });
  useEffect(() => {
    if (!enabled) return;
    const abort = new AbortController();
    const client = createClient(ServerService, transport);
    void keepTrying(
      abort.signal,
      BACKOFF,
      async (connected, signal) => {
        for await (const res of client.watchSystemServices({ serverId }, { signal })) {
          connected();
          setState({ list: res.list, error: undefined });
        }
        return undefined;
      },
      (err) => {
        const error = err as ConnectError;
        setState((s) => ({ ...s, error }));
        return isSessionEnded(err) || reasonOf(err) === "agent_outdated" || reasonOf(err) === "not_found"
          ? "done"
          : undefined;
      },
    );
    return () => abort.abort();
  }, [transport, serverId, enabled]);
  return state;
}

/** Which of a server's system services show. */
export type SystemView = "failed" | "running" | "all";

/** Whether a service shows in a view. */
export function inView(s: SystemServiceInfo, view: SystemView): boolean {
  switch (view) {
    case "failed":
      return s.state === SystemServiceState.FAILED;
    case "running":
      return s.state === SystemServiceState.RUNNING;
    case "all":
      return true;
  }
}

/** Whether a service matches what's typed: its name or what it says it is. */
export function matchesService(s: SystemServiceInfo, query: string): boolean {
  const q = query.trim().toLowerCase();
  return q === "" || s.name.toLowerCase().includes(q) || s.description.toLowerCase().includes(q);
}

/** How a service's state reads: the badge's status and its word. */
export const STATE: Record<SystemServiceState, { status: Status; word: MessageDescriptor }> = {
  [SystemServiceState.UNSPECIFIED]: {
    status: "unknown",
    word: msg({ message: "Unknown", context: "system service state" }),
  },
  [SystemServiceState.RUNNING]: {
    status: "running",
    word: msg({ message: "Running", context: "system service state" }),
  },
  [SystemServiceState.EXITED]: { status: "stopped", word: msg({ message: "Exited", context: "system service state" }) },
  [SystemServiceState.FAILED]: { status: "down", word: msg({ message: "Failed", context: "system service state" }) },
  [SystemServiceState.INACTIVE]: {
    status: "stopped",
    word: msg({ message: "Inactive", context: "system service state" }),
  },
  [SystemServiceState.STARTING]: {
    status: "deploying",
    word: msg({ message: "Starting", context: "system service state" }),
  },
  [SystemServiceState.STOPPING]: {
    status: "deploying",
    word: msg({ message: "Stopping", context: "system service state" }),
  },
};
