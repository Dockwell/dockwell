// SPDX-License-Identifier: AGPL-3.0-only

import { durationMs } from "@bufbuild/protobuf/wkt";
import { useQuery } from "@connectrpc/connect-query";
import {
  type AlertChannelInfo,
  type AlertCheck,
  type AlertCheckInfo,
  type AlertCheckRun,
  AlertCheckState,
  type AlertRuleInfo,
  AlertService,
} from "@dockwell/api";
import type { Status } from "@dockwell/ui";
import type { MessageDescriptor } from "@lingui/core";
import { msg } from "@lingui/core/macro";

import { useMay } from "../auth/may";

/** How often a check runs, as the control plane takes it (internal/resource/alert, Everies). */
export const EVERIES = ["30s", "1m", "5m", "15m"] as const;
/** How many failed runs in a row make a check down: the control plane takes 1 to 10. */
export const DOWN_AFTER = [1, 2, 3, 5, 10] as const;
/** What a check starts at, as the control plane fills them in, so a check with them compares equal. */
export const CHECK_DEFAULTS = { every: "1m", timeout: "10s", downAfter: 2, expectFrom: 200, expectTo: 399 } as const;
/** The rule that says when a check goes down (internal/resource/alert, Defaults). */
export const CHECK_DOWN_RULE = "check-down";

/** The install's checks, by name, with how each stands; only for whoever may list them. */
export function useChecks(options: { poll?: number } = {}) {
  const may = useMay()(AlertService.method.listAlertChecks, { install: true });
  return {
    ...useQuery(
      AlertService.method.listAlertChecks,
      {},
      { enabled: may.allowed, refetchInterval: options.poll ?? 30_000 },
    ),
    may,
  };
}

/** How a check stands, as a badge: its state, in a word. */
export function checkStatus(state: AlertCheckState | undefined): { status: Status; word: MessageDescriptor } {
  switch (state) {
    case AlertCheckState.UP:
      return { status: "healthy", word: msg({ context: "check state", message: "Up" }) };
    case AlertCheckState.FAILING:
      return { status: "degraded", word: msg({ context: "check state", message: "Failing" }) };
    case AlertCheckState.DOWN:
      return { status: "down", word: msg({ context: "check state", message: "Down" }) };
    default:
      return { status: "unknown", word: msg`Waiting for its first run` };
  }
}

/** How long a run took, in milliseconds, or undefined for one that got no answer. */
export function tookMs(run: AlertCheckRun): number | undefined {
  return run.took ? Math.round(durationMs(run.took)) : undefined;
}

/** The host a check asks, as a person names it, or the URL as typed when it doesn't read as one. */
export function hostOf(url: string): string {
  try {
    return new URL(url).host;
  } catch {
    return url;
  }
}

/** A URL as a check compares it: what a person means by the same address. */
function sameAddress(a: string, b: string): boolean {
  try {
    return new URL(a).href === new URL(b).href;
  } catch {
    return a === b;
  }
}

/** The check that already watches an address, if one does. */
export function checkOf(checks: readonly AlertCheckInfo[], url: string): AlertCheckInfo | undefined {
  return checks.find((c) => sameAddress(c.spec?.address ?? "", url));
}

/**
 * A name for a check of url, from its host, that no check has: what fits in a URL
 * and on a command line, as the control plane takes it, such as shop-example-com.
 */
export function nameFor(url: string, taken: readonly string[]): string {
  let base = hostOf(url)
    .toLowerCase()
    .replace(/[^a-z0-9]+/g, "-")
    .replace(/^-+|-+$/g, "");
  if (!/^[a-z]/.test(base)) base = `check-${base}`.replace(/-+$/, "");
  base = base.slice(0, 56).replace(/-+$/, "");
  if (!taken.includes(base)) return base;
  for (let n = 2; ; n++) {
    const name = `${base}-${n}`;
    if (!taken.includes(name)) return name;
  }
}

/** A check's spec with the control plane's defaults filled in where it has none. */
export function withDefaults(spec: Partial<AlertCheck> | undefined): {
  every: string;
  downAfter: number;
  expectFrom: number;
  expectTo: number;
} {
  return {
    every: spec?.every || CHECK_DEFAULTS.every,
    downAfter: spec?.downAfter || CHECK_DEFAULTS.downAfter,
    expectFrom: spec?.expectFrom || CHECK_DEFAULTS.expectFrom,
    expectTo: spec?.expectTo || CHECK_DEFAULTS.expectTo,
  };
}

/** Where a check's alert goes, as the check-down rule sends it. */
export function alertsTo(
  rules: readonly AlertRuleInfo[] | undefined,
  channels: readonly AlertChannelInfo[] | undefined,
): { to: "pending" } | { to: "off" } | { to: "none" } | { to: "every" } | { to: "some"; names: string[] } {
  if (!rules || !channels) return { to: "pending" };
  const rule = rules.find((r) => r.name === CHECK_DOWN_RULE);
  if (!rule || rule.spec?.disabled) return { to: "off" };
  if (channels.length === 0) return { to: "none" };
  const named = rule.spec?.channels ?? [];
  return named.length === 0 ? { to: "every" } : { to: "some", names: named };
}
