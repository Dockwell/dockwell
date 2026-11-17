// SPDX-License-Identifier: AGPL-3.0-only

import {
  type ConnectionRead,
  type FieldChange,
  FieldOp,
  type HostAccess,
  HostAccessKind,
  LinkPart,
  type LinkSet,
  type LinkStorage,
  type NeedStorage,
} from "@dockwell/api";
import type { PlanChange, PlanValue } from "@dockwell/ui";
import { plural } from "@lingui/core/macro";
import { Trans, useLingui } from "@lingui/react/macro";
import { type ReactNode, useMemo } from "react";

import { shownImage, shownRef } from "../../lib/built";
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
  /** The change deletes data the server keeps for 7 days, so undoing it brings the data back. */
  keepsData?: boolean;
  /** The change replaces data with a backup, and undoing it puts back what was there. */
  restores?: boolean;
  /** Where a link to a connection keeps the app's files once applied, and where it did before (ADR 0099). */
  storage?: LinkStorage | undefined;
  storageBefore?: LinkStorage | undefined;
  /** What a variable or a secret reads from a connection once applied, and what it read before (ADR 0099). */
  reads?: ConnectionRead | undefined;
  readsBefore?: ConnectionRead | undefined;
  /** A link's sets whole, after the change and before it, where they change (the slice 3 review, M1). */
  sets?: readonly LinkSet[] | undefined;
  setsBefore?: readonly LinkSet[] | undefined;
  /** Where a catalog install's needs come from, for its project's row (the slice 3 review, L2). */
  needs?: readonly NeedStorage[] | undefined;
  needsBefore?: readonly NeedStorage[] | undefined;
}

/** A catalog install's pinned need's connection, prefix or own field. */
const PINNED = /^catalog\.needs\[[^\]]+\]\.(connection|prefix|own)$/;

/** A link's sets[n] field: its name, part, template or words for TLS. */
/** A field of a link's sets[n], or one whole. */
const SET = /^sets\[\d+\](\.|$)/;

/** Whether two of a link's sets are the same in every field a plan says. */
const sameSet = (a: LinkSet, b: LinkSet): boolean =>
  a.name === b.name && a.part === b.part && a.template === b.template && a.tlsOn === b.tlsOn && a.tlsOff === b.tlsOff;

/** A link's set before and after a change: undefined on the side it isn't on. */
type SetPair = { was?: LinkSet; now?: LinkSet };

/**
 * A link's sets paired by name, in the order they now have, with those it drops
 * after: a name on one side alone comes or goes, and a name on both is the same set.
 * One dropped and one added, alone and reading the same part, is a rename (the slice
 * 3 re-check, N1). Sets the same on both sides are left out.
 */
function pairSets(before: readonly LinkSet[], after: readonly LinkSet[]): SetPair[] {
  const was = new Map(before.map((s) => [s.name, s]));
  const now = new Set(after.map((s) => s.name));
  const added = after.filter((s) => !was.has(s.name));
  const dropped = before.filter((s) => !now.has(s.name));
  const renamed = added.length === 1 && dropped.length === 1 && added[0]?.part === dropped[0]?.part;
  const pairs: SetPair[] = [];
  for (const s of after) {
    const b = was.get(s.name);
    if (b) {
      if (!sameSet(b, s)) pairs.push({ was: b, now: s });
    } else pairs.push({ was: renamed ? dropped[0] : undefined, now: s });
  }
  if (!renamed) for (const s of dropped) pairs.push({ was: s });
  return pairs;
}

const SECRET: PlanValue = { secret: true };

/** One port or one mount of a container: a list item's fields, by name, from "ports[0].host_port". */
const ITEM = /^(ports|mounts|tmpfs)\[(\d+)\]\.(.+)$/;
const KEYED = /^(env|labels|options)\[(.+)\]$/;
/** The revision a service pins a variable or secret it uses to, which Dockwell sets. */
const PIN = /^revisions\[(.+)\]$/;
/** A shared variable or secret a service uses, by the name it has in the app: "secrets[DB_PASSWORD]" is MARIADB_PASSWORD. */
const USES = /^(variables|secrets)\[(.+)\]$/;

/**
 * Reads a plan's fields as what they mean, for every kind: one port takes one
 * row, "8080 → 3000, TCP", and one volume one row, "web-data at /data", where
 * the diff has a field each. A field no one has words for yet reads as words
 * all the same, from its name. Whatever puts a server at risk comes first in
 * its resource, marked critical, however green the rest is.
 */
/** Turns a resource's field changes into rows, and names a kind PlanDiff has no word of its own for. */
export type FieldRows = ((source: FieldSource) => PlanChange[]) & { kindWord?: (kind: string) => string | undefined };

export function useFieldRows(): FieldRows {
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
      "build.git.url": t`repository`,
      "build.git.ref": t`branch`,
      "build.git.commit": t`commit`,
      "build.context": t`build folder`,
      "build.dockerfile": t`Dockerfile`,
      "build.target": t`build stage`,
      database_name: t`database`,
      password: t`password in`,
      // Which of its own values an app shows, which Dockwell keeps beside them (ADR 0081).
      shown_env: t`shows the values of`,
      projects: t`serves`,
    };
    // How a route signs people in (ADR 0087), apart from labels, as a route with them
    // never reads as one row.
    const signInLabels: Readonly<Record<string, string>> = {
      "access.sign_in.provider": t`signs in with`,
      "access.sign_in.emails": t`lets sign in`,
      "access.sign_in.email_domains": t`lets sign in anyone at`,
      "access.sign_in.groups": t`lets sign in the groups`,
      "access.sign_in.session_hours": t`a sign-in lasts, in hours`,
      "access.sign_in.generation": t`sign-in generation`,
    };
    // What a person reads a value as, where the value itself is long: a commit in 7 digits, a branch by its own name, and an image Dockwell built as its commit.
    const readers: Readonly<Record<string, (value: string) => string>> = {
      "build.git.commit": (value) => value.slice(0, 7),
      "build.git.ref": shownRef,
      image: shownImage,
      // A DNS provider's projects by count, not their IDs: the plan's note names the one it gains (the review's B-M5).
      projects: (value) => {
        const count = value === "" ? 0 : value.split(",").length;
        return plural(count, { one: "# project", other: "# projects" });
      },
    };
    const kinds: Readonly<Record<string, string>> = {
      "database.user": t`database user`,
      // A server's own record, which holds its labels (ADR 0091).
      "server.host": t`server`,
      // How one of an app's needs is met, such as its database (ADR 0084).
      link: t({ message: "link", context: "an app's need, met" }),
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
    const keyed = (path: string, group: string, key: string, change: FieldChange, kind = ""): PlanChange => {
      const label = group === "env" ? t`variable` : group === "labels" ? t`label` : t`option`;
      const row: PlanChange = { path, label };
      if (change.sensitive) {
        row.hidden = true;
        if (change.op === FieldOp.REMOVED) row.from = key;
        else row.to = key;
        if (change.op === FieldOp.MODIFIED) row.tone = "change";
        return row;
      }
      // A server's label as the panel and the CLI write it, key:value (ADR 0091); a container's as Docker does.
      const pair = (value: string) =>
        kind === "server.host" ? (value === "" ? key : `${key}:${value}`) : `${key}=${value}`;
      if (change.op !== FieldOp.ADDED) row.from = pair(change.from);
      if (change.op !== FieldOp.REMOVED) row.to = pair(change.to);
      return row;
    };

    /**
     * A shared value an app uses: "uses secret", then its name, and the name the app
     * has it by where that differs, so a long name is a value that wraps rather than
     * a field that crowds out every value beside it.
     */
    const uses = (path: string, group: string, key: string, change: FieldChange): PlanChange => {
      const label = group === "secrets" ? t`uses secret` : t`uses variable`;
      const read = (name: string): PlanValue =>
        name === key ? (
          name
        ) : (
          <Trans>
            <code>{name}</code> as <code>{key}</code>
          </Trans>
        );
      const row: PlanChange = { path, label };
      if (change.op !== FieldOp.ADDED) row.from = read(change.from);
      if (change.op !== FieldOp.REMOVED) row.to = read(change.to);
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

    /**
     * A connection as a row names it: its label, and its name too where the other side
     * of the row is another connection with the same label (the slice 3 review, M2).
     */
    const nameOf = (
      c: { connection: string; connectionLabel: string },
      other?: { connection: string; connectionLabel: string },
    ) => {
      const label = c.connectionLabel || c.connection;
      if (!other || other.connection === c.connection) return label;
      return (other.connectionLabel || other.connection) === label ? `${label} (${c.connection})` : label;
    };
    /** Where a link keeps an app's files: "Uploads, under ghost/". */
    // A plain string, as the rows beside it are (the slice 3 review, L6).
    const keeps = (at: LinkStorage, other?: LinkStorage): PlanValue => {
      const label = nameOf(at, other);
      const prefix = at.prefix;
      return prefix === "" ? t`${label}, at the bucket's root` : t`${label}, under ${prefix}`;
    };
    // A part of what a link meets, in words: by its word in a field, such as
    // "access-key-id", or by the LinkPart a value reads.
    const partWords: Readonly<Record<string, string>> = {
      host: t({ message: "the host", context: "a part a link meets" }),
      port: t({ message: "the port", context: "a part a link meets" }),
      address: t({ message: "the address", context: "a part a link meets" }),
      database: t({ message: "the database", context: "a part a link meets" }),
      user: t({ message: "the user", context: "a part a link meets" }),
      password: t({ message: "the password", context: "a part a link meets" }),
      url: t({ message: "the URL", context: "a part a link meets" }),
      tls: t({ message: "whether it takes TLS", context: "a part a link meets" }),
      "key-prefix": t({ message: "the prefix", context: "a part a link meets" }),
      engine: t({ message: "the engine", context: "a part a link meets" }),
      endpoint: t({ message: "the endpoint", context: "a part a link meets" }),
      region: t({ message: "the region", context: "a part a link meets" }),
      bucket: t({ message: "the bucket", context: "a part a link meets" }),
      "access-key-id": t({ message: "the access key ID", context: "a part a link meets" }),
      "secret-key": t({ message: "the secret key", context: "a part a link meets" }),
      "path-style": t({ message: "whether the bucket goes in the path", context: "a part a link meets" }),
      "public-url": t({ message: "the public address", context: "a part a link meets" }),
      met: t({ message: "whether it's met", context: "a part a link meets" }),
    };
    const partWord = (word: string): string => partWords[word] ?? humanize(word);
    const partOf = (part: LinkPart): string => partWord((LinkPart[part] ?? "").toLowerCase().replaceAll("_", "-"));

    /**
     * Each set of a link that changes, as rows in place of its sets[n] fields: the name
     * and what it reads, "S3_BUCKET reads the bucket from Uploads", or, for a database,
     * "reads the host"; one reading another part, or renamed, says what it was in place
     * of; a URL's template and its words for TLS say whose they are. It reads the plan's
     * sets whole and pairs them by name, so one added or removed beside others is just
     * that (the slice 3 re-check, N1). A plan from before they were kept has none, and
     * its fields read as they did (N2).
     */
    const setsRows = (
      after?: LinkStorage,
      before?: LinkStorage,
      sets: readonly LinkSet[] = [],
      setsBefore: readonly LinkSet[] = [],
    ) => {
      const reads = (set: LinkSet, from?: LinkStorage): string => {
        const where = from ? from.connectionLabel || from.connection : "";
        if (set.part === LinkPart.UNSPECIFIED) {
          return where
            ? t`reads from ${where}`
            : t({ message: "reads", context: "a link's set read from a connection" });
        }
        const what = partOf(set.part);
        return where ? t`reads ${what} from ${where}` : t`reads ${what}`;
      };
      const out: PlanChange[] = [];
      for (const { was, now } of pairSets(setsBefore, sets)) {
        const path = `sets.${now?.name || was?.name}`;
        if (now && !was) out.push({ path, label: now.name, to: reads(now, after) });
        else if (was && !now) out.push({ path, label: was.name, from: reads(was, before) });
        else if (now && was && (now.name !== was.name || now.part !== was.part)) {
          const reading = reads(now, after);
          const wasPart = partOf(was.part);
          const wasName = was.name;
          // A part change reads as one (the slice 3 review, L1).
          const to =
            now.name === was.name ? t`${reading}, in place of ${wasPart}` : t`${reading}, in place of ${wasName}`;
          out.push({ path, label: now.name, to, tone: "change" });
        }
        // A URL's template, and its words for TLS, say whose they are.
        const name = now?.name || was?.name || "";
        for (const [key, label] of [
          ["template", t`${name}'s template`],
          ["tlsOn", t`${name} with TLS`],
          ["tlsOff", t`${name} without TLS`],
        ] as const) {
          const from = was?.[key] ?? "";
          const to = now?.[key] ?? "";
          if (from === to) continue;
          const row: PlanChange = { path: `${path}.${key}`, label };
          if (from !== "") row.from = from;
          if (to !== "") row.to = to;
          out.push(row);
        }
      }
      return out;
    };

    /** What a value reads from a connection, as one row in place of its from.connection fields. */
    const readsRow = (after?: ConnectionRead, before?: ConnectionRead): PlanChange | undefined => {
      const words = (r: ConnectionRead, other?: ConnectionRead) => {
        const what = partOf(r.part);
        const where = nameOf(r, other);
        return t`${what} from ${where}`;
      };
      if (!after && !before) return undefined;
      if (
        after &&
        before &&
        after.connection === before.connection &&
        after.part === before.part &&
        after.link === before.link
      ) {
        return undefined;
      }
      const row: PlanChange = {
        path: "reads",
        label: t({ message: "reads", context: "a value read from a connection" }),
      };
      if (after && before) {
        const now = words(after, before);
        // Say what differs: the part, the connection, or both (the slice 3 review, L1).
        const was =
          after.connection === before.connection
            ? partOf(before.part)
            : after.part === before.part
              ? nameOf(before, after)
              : words(before, after);
        return { ...row, to: t`${now}, in place of ${was}`, tone: "change" };
      }
      if (after) row.to = words(after);
      if (before) row.from = words(before);
      return row;
    };

    /** Where each need of a catalog install that changes comes from: "storage comes from Ghost files, under site/". */
    const needsRows = (after: readonly NeedStorage[] = [], before: readonly NeedStorage[] = []): PlanChange[] => {
      const words = (n: NeedStorage, other?: NeedStorage): PlanValue =>
        n.own || !n.storage ? t`its own values` : keeps(n.storage, other?.storage);
      const names = [...new Set([...before, ...after].map((n) => n.need))];
      const out: PlanChange[] = [];
      for (const need of names) {
        const now = after.find((n) => n.need === need);
        const was = before.find((n) => n.need === need);
        const row: PlanChange = { path: `needs[${need}]`, label: t`${need} comes from` };
        if (now && was) {
          const same =
            now.own === was.own &&
            now.storage?.connection === was.storage?.connection &&
            now.storage?.prefix === was.storage?.prefix;
          if (same) continue;
          out.push({ ...row, from: words(was, now), to: words(now, was), tone: "change" });
        } else if (now) out.push({ ...row, to: words(now) });
        else if (was) out.push({ ...row, from: words(was) });
      }
      return out;
    };

    /** A link to a connection's one row for where its files go, in place of its connection and prefix (ADR 0099). */
    const storageRow = (after?: LinkStorage, before?: LinkStorage): PlanChange | undefined => {
      // By name and prefix, never by label: a renamed label moves nothing (the slice 3 review, H1).
      const same = (a?: LinkStorage, b?: LinkStorage) => a?.connection === b?.connection && a?.prefix === b?.prefix;
      if ((!after && !before) || (after && before && same(after, before))) return undefined;
      const row: PlanChange = { path: "storage", label: t`keeps its files on` };
      if (before) row.from = keeps(before, after);
      if (after) row.to = keeps(after, before);
      if (after && before) row.tone = "change";
      return row;
    };

    const rows = ({
      kind,
      changes,
      hostAccess = [],
      keptPaths = [],
      destroysData = false,
      keepsData = false,
      restores = false,
      storage,
      storageBefore,
      reads,
      readsBefore,
      sets,
      setsBefore,
      needs,
      needsBefore,
    }: FieldSource): PlanChange[] => {
      // What a route that signs people in has of its provider is the provider's, as
      // saved, which its own plan says: one row says the route takes it (ADR 0087).
      const fromProvider = changes.some((c) => c.path.startsWith(FILLED));
      // Where a link to a connection keeps its files says what its connection and
      // prefix fields would, in words, by the connection's label.
      const stored = storage !== undefined || storageBefore !== undefined;
      const read = reads !== undefined || readsBefore !== undefined;
      const all = changes
        .filter((c) => !c.path.startsWith(FILLED))
        .filter((c) => !(stored && (c.path === "connection" || c.path === "prefix")))
        .filter((c) => !(read && c.path.startsWith("from.connection.")))
        .filter((c) => !((needs?.length || needsBefore?.length) && PINNED.test(c.path)))
        .map((change) => ({ path: change.path.replace(/^app\./, ""), change }));
      // A link's sets read as each name and what it reads, not as their fields, where
      // the plan has them whole; an older plan's read as they did (the slice 3 re-check, N2).
      const whole = kind === "link" && (sets?.length || setsBefore?.length);
      const fields = whole ? all.filter((f) => !SET.test(f.path)) : all;
      const rows: PlanChange[] = [];
      const kept = storageRow(storage, storageBefore);
      if (kept) rows.push(kept);
      const reading = readsRow(reads, readsBefore);
      if (reading) rows.push(reading);
      if (whole) rows.push(...setsRows(storage, storageBefore, sets, setsBefore));
      rows.push(...needsRows(needs, needsBefore));
      if (fromProvider) {
        rows.push({
          path: "access.sign_in.filled",
          label: t`its provider's settings`,
          to: t`as the provider has them now`,
          tone: "change",
        });
      }
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
      } else if (keepsData) {
        rows.push({ path: "data", label: t`data`, to: <Trans>kept 7 days, then deleted</Trans>, tone: "warning" });
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
        const used = USES.exec(path);
        const row = entry
          ? keyed(path, entry[1] ?? "", entry[2] ?? "", change, kind)
          : used
            ? uses(path, used[1] ?? "", used[2] ?? "", change)
            : plain(path, change, labels[path] ?? signInLabels[path] ?? humanize(path), readers[path] ?? yesNo);
        const tone = change.op === FieldOp.REMOVED ? undefined : reaching(path, hostAccess);
        rows.push(tone ? { ...row, tone } : row);
      }
      return rows
        .map((row, at) => ({ row, at }))
        .sort((a, b) => rank(a.row) - rank(b.row) || a.at - b.at)
        .map(({ row }) => row);
    };
    return Object.assign(rows, { kindWord: (kind: string) => kinds[kind] });
  }, [t]);
}

/** Where a route keeps what it has of its sign-in provider, which the control plane fills in. */
const FILLED = "access.sign_in.filled.";

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
