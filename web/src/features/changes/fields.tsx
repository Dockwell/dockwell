// SPDX-License-Identifier: AGPL-3.0-only

import { type FieldChange, FieldOp, type HostAccess, HostAccessKind } from "@dockwell/api";
import type { PlanChange, PlanValue } from "@dockwell/ui";
import { Trans, useLingui } from "@lingui/react/macro";
import { type ReactNode, useMemo } from "react";

import { asGoodAsRoot } from "./plan";

/** What the rows of a plan are read from: a resource's kind and what changes in it. */
export interface FieldSource {
  kind: string;
  changes: readonly FieldChange[];
  /** What the change lets it reach on its servers, which reads as a risk. */
  hostAccess?: readonly HostAccess[];
  /** Where the data is that the change takes out of use, which the server keeps for 7 days. */
  keptPaths?: readonly string[];
  /** The change removes data its spec can't bring back. */
  destroysData?: boolean;
  /** The change replaces data with a backup, and undoing it puts back what was there. */
  restores?: boolean;
}

const SECRET: PlanValue = { secret: true };

/** One port or one mount of a container: a list item's fields, by name, from "ports[0].host_port". */
const ITEM = /^(ports|mounts|tmpfs)\[(\d+)\]\.(.+)$/;
const KEYED = /^(env|labels|options)\[(.+)\]$/;
/** The revision a service pins a variable or secret it uses to, which Dockwell sets. */
const PIN = /^revisions\[(.+)\]$/;

/**
 * Reads a plan's fields as what they mean, for every kind: one port takes one
 * row, "8080 → 3000, TCP", and one volume one row, "web-data at /data", where
 * the diff has a field each. A field no one has words for yet reads as words
 * all the same, from its name. Whatever puts a server at risk comes first in
 * its resource, marked critical, however green the rest is.
 */
export function useFieldRows(): (source: FieldSource) => PlanChange[] {
  const { t } = useLingui();
  return useMemo(() => {
    const labels: Readonly<Record<string, string>> = {
      image: t`image`,
      command: t`command`,
      entrypoint: t`entrypoint`,
      no_entrypoint: t`runs no entrypoint`,
      restart: t`restart`,
      user: t`user`,
      working_dir: t`working directory`,
      shm_size: t`shared memory`,
      networks: t`networks`,
      "healthcheck.test": t`health check`,
      references: t`names values in`,
      "healthcheck.interval": t`health check interval`,
      "healthcheck.timeout": t`health check timeout`,
      "healthcheck.start_period": t`health check start period`,
      "healthcheck.retries": t`health check retries`,
      display_name: t`display name`,
      driver: t`driver`,
      internal: t`internal`,
      domain: t`domain`,
      path: t`path`,
      service: t`app`,
      port: t`port`,
      action: t`action`,
      protocol: t`protocol`,
      sources: t`sources`,
      comment: t`comment`,
    };
    const yesNo = (value: string) => (value === "true" ? t`yes` : value === "false" ? t`no` : value);
    const protocol = (value: string) => value.toUpperCase();

    /** A field on its own: its value before and after, in words if it can be. */
    const plain = (path: string, change: FieldChange, label: ReactNode, read = yesNo): PlanChange => {
      const row: PlanChange = { path, label };
      if (change.sensitive) {
        if (change.op === FieldOp.MODIFIED) return { ...row, to: SECRET, tone: "change" };
        if (change.op === FieldOp.ADDED) row.to = SECRET;
        else row.from = SECRET;
        return row;
      }
      if (change.op !== FieldOp.ADDED) row.from = read(change.from);
      if (change.op !== FieldOp.REMOVED) row.to = read(change.to);
      return row;
    };

    /** A whole item that came or went, with what it reads as; one that moved is left to its fields. */
    const whole = (
      path: string,
      fields: readonly FieldChange[],
      label: ReactNode,
      read: (side: "from" | "to") => PlanValue,
    ) => {
      if (fields.every((f) => f.op === FieldOp.ADDED)) return { path, label, to: read("to") };
      if (fields.every((f) => f.op === FieldOp.REMOVED)) return { path, label, from: read("from") };
      return undefined;
    };

    const portRows = (path: string, fields: ReadonlyMap<string, FieldChange>): PlanChange[] => {
      const list = [...fields.values()];
      const side = (name: string, at: "from" | "to") => fields.get(name)?.[at] ?? "";
      const row = whole(path, list, t`port`, (at) => {
        const ip = side("host_ip", at);
        const host = side("host_port", at);
        const out = `${ip ? `${ip.includes(":") ? `[${ip}]` : ip}:` : ""}${host} → ${side("container_port", at)}`;
        return `${out}, ${protocol(side("protocol", at) || "tcp")}`;
      });
      if (row) return [row];
      const words: Readonly<Record<string, string>> = {
        host_port: t`port on the server`,
        container_port: t`port in the app`,
        protocol: t`protocol`,
        host_ip: t`port's address`,
      };
      return [...fields].map(([name, change]) =>
        plain(`${path}.${name}`, change, words[name] ?? humanize(name), name === "protocol" ? protocol : yesNo),
      );
    };

    const mountRows = (
      path: string,
      fields: ReadonlyMap<string, FieldChange>,
      access: readonly HostAccess[],
    ): PlanChange[] => {
      const list = [...fields.values()];
      const hostPath = fields.get("host_path");
      const reaches = hostPath !== undefined && hostPath.op !== FieldOp.REMOVED;
      const socket = reaches && access.some((a) => a.kind === HostAccessKind.DOCKER_SOCKET && a.path === hostPath.to);
      const label = hostPath ? (socket ? t`Docker's socket` : t`server path`) : t`volume`;
      // Red for what is as good as root, so it means something when it comes; amber for a folder of the app's own.
      const tone = access.some((a) => a.path === hostPath?.to && asGoodAsRoot(a)) ? "critical" : "warning";
      const read = (at: "from" | "to"): PlanValue => {
        const source = fields.get(hostPath ? "host_path" : "volume")?.[at] ?? "";
        const target = fields.get("target")?.[at] ?? "";
        const readOnly = fields.get("read_only")?.[at] === "true";
        if (target === source || target === "") {
          return readOnly ? (
            <Trans>
              <code>{source}</code>, read-only
            </Trans>
          ) : (
            source
          );
        }
        return readOnly ? (
          <Trans>
            <code>{source}</code> at <code>{target}</code>, read-only
          </Trans>
        ) : (
          <Trans>
            <code>{source}</code> at <code>{target}</code>
          </Trans>
        );
      };
      const row = whole(path, list, label, read);
      if (row) return [reaches ? { ...row, tone } : row];
      const words: Readonly<Record<string, string>> = {
        volume: t`volume`,
        host_path: t`server path`,
        target: t`mounted at`,
        read_only: t`read-only`,
        relabel: t`SELinux label`,
      };
      return [...fields].map(([name, change]) => {
        const one = plain(`${path}.${name}`, change, words[name] ?? humanize(name));
        return name === "host_path" && reaches ? { ...one, tone } : one;
      });
    };

    const tmpfsRows = (path: string, fields: ReadonlyMap<string, FieldChange>): PlanChange[] => {
      const read = (at: "from" | "to"): PlanValue => {
        const target = fields.get("target")?.[at] ?? "";
        const size = fields.get("size")?.[at];
        return size ? t`${target}, up to ${size}` : target;
      };
      const row = whole(path, [...fields.values()], t`tmpfs`, read);
      if (row) return [row];
      const words: Readonly<Record<string, string>> = { target: t`tmpfs at`, size: t`tmpfs size` };
      return [...fields].map(([name, change]) => plain(`${path}.${name}`, change, words[name] ?? humanize(name)));
    };

    /** A variable, a label or an option: named by its key, and hidden when it may hold a secret. */
    const keyed = (path: string, group: string, key: string, change: FieldChange): PlanChange => {
      const label = group === "env" ? t`variable` : group === "labels" ? t`label` : t`option`;
      const row: PlanChange = { path, label };
      if (change.sensitive) {
        row.hidden = true;
        if (change.op === FieldOp.REMOVED) row.from = key;
        else row.to = key;
        if (change.op === FieldOp.MODIFIED) row.tone = "change";
        return row;
      }
      if (change.op !== FieldOp.ADDED) row.from = `${key}=${change.from}`;
      if (change.op !== FieldOp.REMOVED) row.to = `${key}=${change.to}`;
      return row;
    };

    /** A route as a whole, on one row: where it is, and where it goes. */
    const routeRow = (fields: ReadonlyMap<string, FieldChange>): PlanChange | undefined =>
      whole("route", [...fields.values()], t`route`, (at) => {
        const side = (name: string) => fields.get(name)?.[at] ?? "";
        return `${side("domain")}${side("path")} → ${side("service")}:${side("port")}`;
      });

    /** A firewall rule as a whole, on one row: what it does, to what, from where. */
    const ruleRow = (fields: ReadonlyMap<string, FieldChange>): PlanChange | undefined =>
      whole("rule", [...fields.values()], t`rule`, (at) => {
        const side = (name: string) => fields.get(name)?.[at] ?? "";
        const port = side("port");
        const proto = protocol(side("protocol") || "tcp");
        const from = side("sources");
        return side("action") === "deny" || side("action") === "block" ? (
          from === "" ? (
            <Trans>
              block <code>{port}</code>, {proto}, from anywhere
            </Trans>
          ) : (
            <Trans>
              block <code>{port}</code>, {proto}, from <code>{from}</code>
            </Trans>
          )
        ) : from === "" ? (
          <Trans>
            allow <code>{port}</code>, {proto}, from anywhere
          </Trans>
        ) : (
          <Trans>
            allow <code>{port}</code>, {proto}, from <code>{from}</code>
          </Trans>
        );
      });

    /** How a field reads when it is how a resource gets past what Docker keeps for it. */
    const reaching = (path: string, access: readonly HostAccess[]): PlanChange["tone"] => {
      if (path === "networks" && access.some((a) => a.kind === HostAccessKind.HOST_NETWORK)) return "critical";
      if (path !== "driver" && !path.startsWith("options[")) return undefined;
      const reach = access.filter((a) => a.kind !== HostAccessKind.HOST_NETWORK);
      if (reach.length === 0) return undefined;
      return reach.some(asGoodAsRoot) ? "critical" : "warning";
    };

    return ({
      kind,
      changes,
      hostAccess = [],
      keptPaths = [],
      destroysData = false,
      restores = false,
    }: FieldSource): PlanChange[] => {
      const fields = changes.map((change) => ({ path: change.path.replace(/^app\./, ""), change }));
      const rows: PlanChange[] = [];
      // Data it takes away isn't a field of any spec, and leads whatever else changes.
      if (keptPaths.length > 0) {
        rows.push({ path: "data", label: t`data taken out of use`, to: keptPaths.join(", "), tone: "critical" });
      } else if (restores) {
        rows.push({
          path: "data",
          label: t`data`,
          to: <Trans>replaced, and undoing this puts it back</Trans>,
          tone: "critical",
        });
      } else if (destroysData) {
        rows.push({ path: "data", label: t`data`, to: <Trans>doesn't come back</Trans>, tone: "critical" });
      }

      // A kind that reads best as one row, when all of it comes or goes.
      if (kind === "route" || kind === "firewall.rule") {
        const byName = new Map(fields.map((f) => [f.path, f.change]));
        const row = kind === "route" ? routeRow(byName) : ruleRow(byName);
        if (row && fields.every((f) => f.path in labels)) return [...rows, row];
      }

      const items = new Map<string, Map<string, FieldChange>>();
      for (const { path, change } of fields) {
        const item = ITEM.exec(path);
        if (item) {
          const key = `${item[1]}[${item[2]}]`;
          if (!items.has(key)) items.set(key, new Map());
          items.get(key)?.set(item[3] ?? "", change);
        }
      }
      const done = new Set<string>();
      for (const { path, change } of fields) {
        const item = ITEM.exec(path);
        if (item) {
          const key = `${item[1]}[${item[2]}]`;
          if (done.has(key)) continue;
          done.add(key);
          const parts = items.get(key) ?? new Map<string, FieldChange>();
          const list = item[1];
          if (list === "ports") rows.push(...portRows(key, parts));
          else if (list === "tmpfs") rows.push(...tmpfsRows(key, parts));
          else rows.push(...mountRows(key, parts, hostAccess));
          continue;
        }
        // A pin moves as the value it pins does: what a person reads is that it takes the newer one.
        // One that comes or goes says nothing the row of what it uses doesn't.
        const pin = PIN.exec(path);
        if (pin) {
          if (change.op === FieldOp.MODIFIED) {
            rows.push({ path, label: pin[1] ?? "", to: t`uses its newer value`, tone: "change" });
          }
          continue;
        }
        const entry = KEYED.exec(path);
        const row = entry
          ? keyed(path, entry[1] ?? "", entry[2] ?? "", change)
          : plain(path, change, labels[path] ?? humanize(path));
        const tone = change.op === FieldOp.REMOVED ? undefined : reaching(path, hostAccess);
        rows.push(tone ? { ...row, tone } : row);
      }
      return rows
        .map((row, at) => ({ row, at }))
        .sort((a, b) => rank(a.row) - rank(b.row) || a.at - b.at)
        .map(({ row }) => row);
    };
  }, [t]);
}

/** The order a person reads a resource in: what puts it at risk, what it runs, how it is reached, what it keeps and knows. */
function rank(row: PlanChange): number {
  if (row.tone === "critical") return 0;
  if (row.tone === "warning") return 1;
  const name = row.path.replace(/\[.*$/, "").replace(/\..*$/, "");
  const at = ["image", "command", "entrypoint", "no_entrypoint", "ports", "networks", "mounts", "env"].indexOf(name);
  if (at !== -1) return at + 2;
  return name === "restart" ? 20 : 10;
}

/**
 * A field no one has words for yet, in words: "max_body_size" reads as "max
 * body size", "limits.cpu" as "limits cpu" and "backends[0].host" as "backends
 * 1 host".
 */
export function humanize(path: string): string {
  // The split keeps each bracket as its own part. A key is a name someone
  // wrote, such as FEATURE_FLAGS, and keeps its underscores.
  return path
    .split(/(\[[^\]]+\])/)
    .map((part) => {
      const bracket = /^\[(.+)\]$/.exec(part);
      if (!bracket) return part.replace(/[._]+/g, " ").trim();
      const key = bracket[1] ?? "";
      return /^\d+$/.test(key) ? String(Number(key) + 1) : key;
    })
    .filter((part) => part !== "")
    .join(" ");
}
