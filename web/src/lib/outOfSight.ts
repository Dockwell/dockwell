// SPDX-License-Identifier: AGPL-3.0-only

import { useSyncExternalStore } from "react";

/**
 * How long a tab stays out of sight before its streams close. Over plain HTTP,
 * as through the SSH tunnel, a browser keeps six connections to the panel for
 * all its tabs, and each open stream holds one; a tab out of sight frees its
 * own for the tabs in view.
 */
export const HIDDEN_CLOSE_MS = 30_000;

let away = false;
let timer: ReturnType<typeof setTimeout> | undefined;
const listeners = new Set<() => void>();

function become(next: boolean): void {
  if (away === next) return;
  away = next;
  for (const listener of listeners) listener();
}

function onVisibility(): void {
  clearTimeout(timer);
  timer = undefined;
  if (document.visibilityState === "hidden") timer = setTimeout(() => become(true), HIDDEN_CLOSE_MS);
  else become(false);
}

function subscribe(listener: () => void): () => void {
  listeners.add(listener);
  if (listeners.size === 1) {
    document.addEventListener("visibilitychange", onVisibility);
    onVisibility();
  }
  return () => {
    listeners.delete(listener);
    if (listeners.size > 0) return;
    document.removeEventListener("visibilitychange", onVisibility);
    clearTimeout(timer);
    timer = undefined;
    away = false;
  };
}

/**
 * Whether the tab has been out of sight for HIDDEN_CLOSE_MS, when its streams
 * close; false again as it comes back. One look, shared by every stream.
 */
export function useAway(): boolean {
  return useSyncExternalStore(
    subscribe,
    () => away,
    () => false,
  );
}
