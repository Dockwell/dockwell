// SPDX-License-Identifier: AGPL-3.0-only

import type { ServiceInfo } from "@dockwell/api";
import { cx, Metric, Panel } from "@dockwell/ui";
import { plural } from "@lingui/core/macro";
import { useLingui } from "@lingui/react/macro";

import { formatBytes } from "../../lib/format";
import { appOf } from "./app";
import { type AppUsageSeries, useAppUsage } from "./usage";

/** A share as a person reads it: whole from 10 up, to one place below, so 0.4 isn't 0. */
export function shareWords(percent: number): string {
  return percent >= 10 ? String(Math.round(percent)) : String(Math.round(percent * 10) / 10);
}

/**
 * Its CPU against what it may use: where it has a limit, the share of every
 * replica's limit together, the sum against the sum, so 3 replicas at half a CPU
 * each under a limit of 1 read 50%, not 150% (#848's review, M2); with none, of
 * one CPU.
 */
export function cpuOfLimit(service: ServiceInfo, usage: AppUsageSeries): { value: number; ofLimit: boolean } {
  const cpus = Number(appOf(service)?.resources?.cpus ?? "");
  return cpus > 0
    ? { value: usage.cpuNow / (cpus * usage.replicasNow), ofLimit: true }
    : { value: usage.cpuNow, ofLimit: false };
}

/**
 * What the app uses, as the App-Overview mock's Metrics card has it, first in the
 * aside (#797): its CPU and memory over the last 5 minutes, all replicas together,
 * each against its limit where it has one. Nothing until its servers' samples
 * carry it, and nothing once it stops running.
 */
export function AppMetrics({ service }: { service: ServiceInfo }) {
  const { t, i18n } = useLingui();
  const usage = useAppUsage(service);
  if (!usage) return null;
  const cpu = cpuOfLimit(service, usage);
  const cpus = Number(appOf(service)?.resources?.cpus ?? "");
  const limit = usage.memoryLimit > 0 ? formatBytes(usage.memoryLimit, i18n.locale) : "";
  const cpuData =
    cpus > 0 ? usage.cpu.map((c, i) => (c === null ? null : c / (cpus * (usage.replicas[i] ?? 1)))) : usage.cpu;
  const missing = usage.missing;
  // With a server not reporting, its replicas aren't in the figures, so they aren't all (#852's review, L3).
  const span =
    missing > 0
      ? plural(missing, { one: "# server not reporting", other: "# servers not reporting" })
      : t`Last 5 minutes, all replicas`;
  return (
    <Panel title={t`Metrics`} actions={<span className="app-muted app-metrics-span">{span}</span>}>
      {/* While the panel can't hear the fleet, its last figures read as not current, as the overview's do (L4). */}
      <div className={cx("app-metrics", !usage.current && "is-stale")} aria-disabled={!usage.current || undefined}>
        <Metric
          label={t`CPU`}
          value={shareWords(cpu.value)}
          unit={cpu.ofLimit ? t`% of limit` : t`% of a CPU`}
          data={cpuData}
          min={0}
          {...(cpu.ofLimit ? { max: 100 } : {})}
        />
        <Metric
          label={t`Memory`}
          value={formatBytes(usage.memoryNow, i18n.locale)}
          unit={limit ? t`of ${limit}` : undefined}
          data={usage.memory}
          min={0}
          {...(usage.memoryLimit > 0 ? { max: usage.memoryLimit } : {})}
        />
      </div>
    </Panel>
  );
}
