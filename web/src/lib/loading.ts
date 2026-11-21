// SPDX-License-Identifier: AGPL-3.0-only

import { useEffect } from "react";

const PREFIX = "dockwell.rows.";

function read(key: string): number | undefined {
  try {
    const n = Number(localStorage.getItem(PREFIX + key));
    return Number.isInteger(n) && n >= 0 ? n : undefined;
  } catch {
    return undefined;
  }
}

function write(key: string, n: number): void {
  try {
    localStorage.setItem(PREFIX + key, String(n));
  } catch {
    // Private windows and full disks forget; the list then draws its usual few rows.
  }
}

/**
 * How many rows a list draws while it waits: as many as it showed last time in
 * this browser, so nothing moves when they land, and fallback before that. It
 * remembers count once there is one. Never fewer than one, nor more than max, a
 * screen's worth.
 */
export function useHeldRows(key: string, count: number | undefined, fallback = 3, max = 8): number {
  useEffect(() => {
    if (count !== undefined) write(key, count);
  }, [key, count]);
  const held = count ?? read(key) ?? fallback;
  return Math.min(Math.max(held, 1), max);
}
