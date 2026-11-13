// SPDX-License-Identifier: AGPL-3.0-only

import { type ConnectError, createClient } from "@connectrpc/connect";
import { useTransport } from "@connectrpc/connect-query";
import {
  type Change,
  ChangeService,
  ChangeState,
  ServerService,
  StepState,
  SystemInit,
  SystemServiceAction,
  type SystemServiceInfo,
  type SystemServiceList,
  SystemServiceState,
} from "@dockwell/api";
import type { Status } from "@dockwell/ui";
import type { MessageDescriptor } from "@lingui/core";
import { msg } from "@lingui/core/macro";
import { useCallback, useEffect, useMemo, useRef, useState } from "react";

import { isSessionEnded, reasonOf } from "../../lib/errors";
import { keepTrying } from "../../lib/retry";
import { useChangeSheet } from "../changes/ChangeSheet";
import { followChange } from "../changes/follow";
import { isFinal } from "../changes/plan";

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

/** Words a line that says what went wrong has, in the many ways programs say it. */
const WRONG =
  /\b(emerg|alert|crit|error|fatal|panic|fail(ed|ure)?|denied|refused|unknown|invalid|cannot|can't|unable|no such|not found)\b/i;

/**
 * Which of the lines a failed unit's own process wrote last says why: the first that
 * reads as something going wrong, as what follows it is often its consequence ("test
 * failed"), or else the last.
 */
export function whyLine(why: readonly string[]): string {
  return why.find((l) => WRONG.test(l)) ?? why.at(-1) ?? "";
}

/** Whether a unit is masked: nothing starts it until it's unmasked. */
export function isMasked(s: SystemServiceInfo): boolean {
  return s.atBoot === "masked" || s.atBoot === "masked-runtime";
}

/** Whether a unit starts at boot, by its init's word: systemd's enabled, or an OpenRC runlevel. */
export function startsAtBoot(s: SystemServiceInfo, init: SystemInit): boolean {
  if (init === SystemInit.OPENRC) return s.atBoot !== "";
  return s.atBoot === "enabled" || s.atBoot === "enabled-runtime" || s.atBoot === "alias";
}

/** Whether a unit has no install section of its own, so another unit starts it and there's nothing to enable. */
export function startsWhenNeeded(s: SystemServiceInfo): boolean {
  return ["static", "indirect", "generated", "transient"].includes(s.atBoot);
}

/** Whether a unit runs, or is on its way to. */
export function isRunning(s: SystemServiceInfo): boolean {
  return s.state === SystemServiceState.RUNNING || s.state === SystemServiceState.STARTING;
}

/** The one action a unit's row offers: Unmask for a masked one, Restart for one that runs or failed, else Start. */
export function primaryAction(s: SystemServiceInfo): SystemServiceAction {
  if (isMasked(s)) return SystemServiceAction.UNMASK;
  if (isRunning(s) || s.state === SystemServiceState.FAILED) return SystemServiceAction.RESTART;
  return SystemServiceAction.START;
}

/** An action on a unit the page took, while it's planned, reviewed or applies. */
export interface UnitAct {
  name: string;
  action: SystemServiceAction;
  phase: "planning" | "reviewing" | "applying";
  changeId?: string;
}

/** An action on a unit that failed, and the line that says why. */
export interface UnitFailure {
  name: string;
  action: SystemServiceAction;
  why: string;
  changeId: string;
}

/**
 * Acting on a server's system services (ADR 0082): a plan for the action in the
 * plan sheet, then the change followed while it applies, so the unit's row says
 * where it is. One that fails stays on its row with why until another action.
 */
export function useUnitActions(serverId: string) {
  const transport = useTransport();
  const sheet = useChangeSheet();
  const client = useMemo(() => createClient(ChangeService, transport), [transport]);
  const [act, setAct] = useState<UnitAct>();
  const [failure, setFailure] = useState<UnitFailure>();

  const ended = useCallback((c: Change, asked: UnitAct) => {
    setAct(undefined);
    if (c.state === ChangeState.FAILED) {
      const step = c.steps.find((st) => st.state === StepState.FAILED);
      setFailure({
        name: asked.name,
        action: asked.action,
        why: step?.detail || step?.message || c.error,
        changeId: c.id,
      });
    }
  }, []);

  // Follows the change once it's planned, until it ends: by its ID, so the act's phase moving doesn't follow anew.
  const followId = act?.changeId ?? "";
  const following = useRef(act);
  following.current = act;
  useEffect(() => {
    const asked = following.current;
    if (!followId || !asked) return;
    const abort = new AbortController();
    void followChange(transport, followId, abort.signal, (c) => {
      if (abort.signal.aborted) return;
      if (isFinal(c)) {
        if (c.state !== ChangeState.DISCARDED) ended(c, asked);
        else setAct(undefined);
        return;
      }
      if (c.state === ChangeState.APPLYING)
        setAct((was) => (was?.changeId === c.id ? { ...was, phase: "applying" } : was));
    });
    return () => abort.abort();
  }, [followId, transport, ended]);

  const plan = useCallback(
    (name: string, action: SystemServiceAction) => {
      setFailure(undefined);
      setAct({ name, action, phase: "planning" });
      sheet.open({
        kind: "plan",
        make: async () => {
          try {
            const res = await client.planSystemServiceAction({ serverIds: [serverId], name, action });
            if (res.change) setAct({ name, action, phase: "reviewing", changeId: res.change.id });
            return res;
          } catch (err) {
            setAct(undefined);
            throw err;
          }
        },
        onClose: (c) => {
          // A plan closed unapplied is discarded; one applying goes on, followed.
          if (!c || c.state === ChangeState.PLANNED || c.state === ChangeState.DISCARDED) {
            setAct((was) => (was && (!c || was.changeId === c.id) ? undefined : was));
          }
        },
      });
    },
    [client, serverId, sheet],
  );

  const open = useCallback((id: string) => sheet.open({ kind: "change", id }), [sheet]);

  return {
    /** The action under way, if any. */
    act,
    /** The last action that failed, until another is taken. */
    failure,
    /** True while an action applies on the server: another waits for it. */
    busy: act?.phase === "applying",
    plan,
    open,
  };
}

export type UnitActions = ReturnType<typeof useUnitActions>;
