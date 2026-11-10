// SPDX-License-Identifier: AGPL-3.0-only

import { createClient } from "@connectrpc/connect";
import { useTransport } from "@connectrpc/connect-query";
import { StepUpAction, VariableService } from "@dockwell/api";
import { createContext, useCallback, useContext, useEffect, useMemo, useRef, useState } from "react";

import { useAuthError } from "../auth/messages";
import { withStepUpIfAsked } from "../auth/stepUp";

/** How long a revealed value stays on screen before it hides again. */
export const REVEALED_FOR = 60_000;

interface Shown {
  value?: string;
  busy?: boolean;
  problem?: string;
}

/** The secrets revealed on a page, by ID, and how to reveal and hide them. */
export interface Reveals {
  get(id: string): Shown;
  reveal(id: string): void;
  hide(id: string): void;
  /** The environment the page's secrets are in, by ID, where it's known: revealing is asked for there (ADR 0059). */
  environment: string | undefined;
}

const RevealsContext = createContext<Reveals | undefined>(undefined);

/** Gives the secrets on a page one place to keep what is revealed, whichever layout shows them. */
export const RevealsProvider = RevealsContext.Provider;

/**
 * Reveals secrets' values: for each, the browser asks for a passkey, which
 * confirms revealing that secret (a step-up), and the control plane answers
 * with the value, once, and records it in the audit log (docs/adr/0022).
 * Values live in this state alone, never in a cache, and each hides again
 * after a minute, or when the page goes.
 */
export function useReveals(environment?: string): Reveals {
  const transport = useTransport();
  const explain = useAuthError();
  const [shown, setShown] = useState<Readonly<Record<string, Shown>>>({});
  const timers = useRef(new Map<string, ReturnType<typeof setTimeout>>());
  // Ends the reveals in flight when the page goes: a value that arrives
  // after is dropped, rather than kept until its timer would hide it.
  const leaving = useRef(new AbortController());

  useEffect(() => {
    const pending = timers.current;
    const left = new AbortController();
    leaving.current = left;
    return () => {
      left.abort();
      for (const timer of pending.values()) clearTimeout(timer);
      pending.clear();
    };
  }, []);

  const set = useCallback((id: string, next: Shown) => setShown((was) => ({ ...was, [id]: next })), []);

  const hide = useCallback(
    (id: string) => {
      clearTimeout(timers.current.get(id));
      timers.current.delete(id);
      set(id, {});
    },
    [set],
  );

  const reveal = useCallback(
    (id: string) => {
      set(id, { busy: true });
      const signal = leaving.current.signal;
      void (async () => {
        try {
          const revealed = await withStepUpIfAsked(transport, StepUpAction.REVEAL_SECRET, id, (proof) =>
            createClient(VariableService, transport).revealSecret({ id, proof }, { signal }),
          );
          if (signal.aborted) return; // the page is gone, and the value with this call
          set(id, { value: revealed.value });
          clearTimeout(timers.current.get(id));
          timers.current.set(
            id,
            setTimeout(() => hide(id), REVEALED_FOR),
          );
        } catch (err) {
          if (!signal.aborted) set(id, { problem: explain(err) });
        }
      })();
    },
    [transport, explain, set, hide],
  );

  return useMemo(
    () => ({ get: (id: string) => shown[id] ?? {}, reveal, hide, environment }),
    [shown, reveal, hide, environment],
  );
}

/** One secret's reveal, from the page's RevealsProvider. */
export function useReveal(id: string) {
  const reveals = useContext(RevealsContext);
  if (!reveals) throw new Error("useReveal needs a RevealsProvider around it");
  const shown = reveals.get(id);
  return {
    value: shown.value,
    busy: shown.busy ?? false,
    problem: shown.problem,
    reveal: () => reveals.reveal(id),
    hide: () => reveals.hide(id),
    environment: reveals.environment,
  };
}
