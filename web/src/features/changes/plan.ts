// SPDX-License-Identifier: AGPL-3.0-only

import { timestampMs } from "@bufbuild/protobuf/wkt";
import {
  type Change,
  ChangeState,
  FieldOp,
  type HostAccess,
  HostAccessKind,
  IngressChange,
  type Intent,
  IntentKind,
  type Plan,
  PlanAction,
  type PlannedResource,
  type PlanStep,
  type ServerPlan,
  type StepProgress,
  StepState,
} from "@dockwell/api";
import type { PlanAction as DiffAction, PlanKind, PlanResource } from "@dockwell/ui";

import type { FieldRows } from "./fields";

/** The limits a container can change in place, for as long as it goes on having them. */
const IN_PLACE_LIMITS: ReadonlySet<string> = new Set([
  "resources.cpus",
  "resources.memory",
  "resources.memory_reservation",
]);

/** How PlanDiff draws each of the plan's actions that change something. */
const ACTIONS: Readonly<Record<Exclude<PlanAction, PlanAction.NONE>, DiffAction>> = {
  [PlanAction.UNSPECIFIED]: "update",
  [PlanAction.CREATE]: "create",
  [PlanAction.UPDATE]: "update",
  [PlanAction.REPLACE]: "replace",
  [PlanAction.DELETE]: "destroy",
};

/** A plan that restarts what it's about, which changes no spec, so it has nothing to undo or go back to. */
export function restarts(plan: Plan | undefined): boolean {
  return (plan?.servers ?? []).some((s) => s.steps.some((step) => step.restart !== ""));
}

/** A step that starts what's there and isn't running, so nothing serves while it does. */
export function startsStopped(step: PlanStep): boolean {
  return step.changes.some((c) => c.path === "state" && c.to === "running");
}

/** A step that changes a route on a proxy that stays up, which reloads it. A rollout doesn't. */
export function reloadsProxy(step: PlanStep): boolean {
  if (step.resource?.kind !== "route" || step.action === PlanAction.NONE) return false;
  const change = step.ingress?.change;
  return change !== IngressChange.STARTS && change !== IngressChange.STOPS && change !== IngressChange.RESTARTS;
}

/**
 * The kind PlanDiff draws a resource of the API's kind as; one it doesn't know
 * takes its word from kindLabel. A service is an app, the one word people see
 * for it everywhere.
 */
export function diffKind(kind: string): PlanKind {
  switch (kind) {
    case "service":
      return "app";
    case "project":
    case "environment":
    case "container":
    case "volume":
    case "network":
    case "route":
    case "secret":
    case "variable":
      return kind;
    case "firewall.rule":
      return "firewall";
  }
  return "resource";
}

/** A resource's kind as a word, for a kind that isn't known yet: "ssl.certificate" reads "ssl certificate". */
export function kindWord(kind: string): string {
  return kind.replace(/[._]+/g, " ");
}

/** The resources a plan changes: one it leaves as it is isn't one, as the plan's summary doesn't count it. */
/** What a plan says it sets a field of what it plans to. */
export function changeTo(planned: PlannedResource, path: string): string | undefined {
  return planned.changes.find((c) => c.path === path)?.to;
}

/** The domain a planned route sends, from what the plan sets, or takes away. */
export function routeDomain(planned: PlannedResource): string | undefined {
  const domain = planned.changes.find((c) => c.path === "domain");
  return domain?.to || domain?.from || undefined;
}

/** A planned route's domain and path, as people read them. */
export function routeWhereOf(planned: PlannedResource): string | undefined {
  const domain = routeDomain(planned);
  if (!domain) return undefined;
  const path = planned.changes.find((c) => c.path === "path");
  return `${domain}${path?.to || path?.from || ""}`;
}

/** What a person calls what a plan changes: a route's domain, which they typed, not the name made from it. */
export function shownName(planned: PlannedResource | undefined): string {
  if (!planned) return "";
  if (planned.resource?.kind === "route") return routeWhereOf(planned) ?? planned.resource.name;
  return planned.resource?.name ?? "";
}

export function changing(plan: Plan | undefined): PlannedResource[] {
  return (plan?.resources ?? []).filter((r) => r.action !== PlanAction.NONE);
}

/** True for what is as good as root on the server, as the plan says, and always for Docker's socket and the server's network. */
export function asGoodAsRoot(access: HostAccess): boolean {
  return (
    access.sameAsRoot || access.kind === HostAccessKind.DOCKER_SOCKET || access.kind === HostAccessKind.HOST_NETWORK
  );
}

/** What a change is for, when the method that proposed it said. */
export function intentOf(plan: Plan | undefined): Intent | undefined {
  const intent = plan?.intent;
  return intent && intent.kind !== IntentKind.UNSPECIFIED ? intent : undefined;
}

/** The order a person reads a plan in: what is at risk, then what runs, the app with a way in first, then the rest. */
function readingOrder(resource: PlanResource, planned: PlannedResource | undefined): number {
  if (resource.changes?.some((c) => c.tone === "critical" || c.tone === "warning")) return 0;
  if (!planned || !runs(planned)) return 3;
  return planned.changes.some((c) => /(^|\.)ports\[/.test(c.path)) ? 1 : 2;
}

/**
 * A plan's resources as PlanDiff shows them: each one's action, and its field
 * changes as rows that say what they mean, in the order a person reads them. A
 * sensitive field says it changed and never what it holds, which the API blanks
 * anyway. What the plan says it is for names the row: a deploy recreates a
 * container, which is how it is done and no news to whoever deployed, so it
 * counts as an update and the downtime says the rest.
 */
export function diffResources(
  plan: Plan | undefined,
  rows: FieldRows,
  options: { expanded?: boolean; tag?: (intent: Intent) => string } = {},
): PlanResource[] {
  const intent = intentOf(plan);
  const maybe = ifNewerIn(plan);
  const shown = changing(plan).map((resource) => {
    const kind = resource.resource?.kind ?? "";
    const steps = (plan?.servers ?? []).flatMap((s) => s.steps).filter((s) => s.resourceId === resource.id);
    const changes = rows({
      kind,
      changes: resource.changes,
      hostAccess: resource.hostAccess,
      keptPaths: [...new Set(steps.flatMap((s) => s.keptPaths))],
      destroysData: steps.some((s) => s.destroysData),
      restores: steps.some((s) => s.restore !== undefined),
    });
    const out: PlanResource = {
      id: resource.id,
      action: ACTIONS[resource.action as Exclude<PlanAction, PlanAction.NONE>],
      kind: diffKind(kind),
      name: shownName(resource) || resource.id,
      changes,
    };
    if (diffKind(kind) === "resource") out.kindLabel = rows.kindWord?.(kind) ?? kindWord(kind);
    if (maybe.has(resource.id)) out.ifNewer = true;
    if (steps.some((s) => s.restart !== "")) out.restart = true;
    if (intent?.resourceId === resource.id && resource.action !== PlanAction.DELETE) {
      out.action = "update";
      if (options.tag) out.note = options.tag(intent);
    }
    // What is at risk is never folded away, as what a plan deletes otherwise starts.
    if (changes.some((c) => c.tone === "critical" || c.tone === "warning")) out.expanded = true;
    if (options.expanded !== undefined) out.expanded = options.expanded;
    return { out, resource };
  });
  return shown
    .map(({ out, resource }, at) => ({ out, at, rank: readingOrder(out, resource) }))
    .sort((a, b) => a.rank - b.rank || a.at - b.at)
    .map(({ out }) => out);
}

/** What a plan makes that only matters to what runs in it. */
const PLUMBING: ReadonlySet<string> = new Set(["project", "environment", "network", "volume"]);

/** What comes along with a plan's apps, for one row that says so. */
export interface Fold {
  /** The rows left to show. */
  shown: PlanResource[];
  /** The rows the fold holds. */
  folded: PlanResource[];
  /** What it says of them. */
  project: string;
  environment: string;
  networks: number;
  volumes: number;
}

/**
 * A plan's rows with what it only makes for its apps folded away, when it has
 * apps to lead with. What is at risk stays on show, and so does anything changed
 * or deleted, which is news. An environment that comes with a new project isn't
 * shown at all: they stay out of sight until there is a second (ADR 0026).
 */
export function foldPlumbing(plan: Plan | undefined, resources: PlanResource[]): Fold {
  const fold: Fold = { shown: resources, folded: [], project: "", environment: "", networks: 0, volumes: 0 };
  const all = changing(plan);
  if (!all.some(runs)) return fold;
  const byId = new Map(all.map((r) => [r.id, r]));
  const makes = (r: PlanResource) => {
    const planned = byId.get(r.id);
    return (
      planned?.action === PlanAction.CREATE &&
      PLUMBING.has(planned.resource?.kind ?? "") &&
      !r.changes?.some((c) => c.tone === "critical" || c.tone === "warning")
    );
  };
  const madeProject = resources.some((r) => makes(r) && byId.get(r.id)?.resource?.kind === "project");
  const quiet = (r: PlanResource) => madeProject && makes(r) && byId.get(r.id)?.resource?.kind === "environment";
  fold.shown = resources.filter((r) => !makes(r));
  fold.folded = resources.filter((r) => makes(r) && !quiet(r));
  for (const r of fold.folded) {
    switch (byId.get(r.id)?.resource?.kind) {
      case "project":
        fold.project = r.name;
        break;
      case "environment":
        fold.environment = r.name;
        break;
      case "network":
        fold.networks += 1;
        break;
      case "volume":
        fold.volumes += 1;
        break;
    }
  }
  return fold;
}

/** The project a plan brings to life, which is what you called it, when its apps are many. */
export function madeProject(plan: Plan | undefined): string {
  const made = changing(plan).find((r) => r.resource?.kind === "project" && r.action === PlanAction.CREATE);
  return made?.resource?.name ?? "";
}

function durationMs(d: { seconds: bigint; nanos: number } | undefined): number {
  return d ? Number(d.seconds) * 1000 + d.nanos / 1e6 : 0;
}

/** The longest any one server is down for, in ms: each server's steps run one at a time, servers side by side. */
export function longestDowntime(servers: readonly ServerPlan[]): number {
  let longest = 0;
  for (const server of servers) longest = Math.max(longest, durationMs(server.downtime));
  return longest;
}

/** The longest any one server may take to be healthy again, in ms. */
export function longestUntilHealthy(servers: readonly ServerPlan[]): number {
  let longest = 0;
  for (const server of servers) longest = Math.max(longest, durationMs(server.untilHealthy));
  return longest;
}

/** The first characters of a plan's fingerprint, which a person types to confirm a destructive plan. */
export function shortFingerprint(plan: Plan | undefined): string {
  return (plan?.fingerprint ?? "").slice(0, 8);
}

/** True for a change that is over: applied, failed or discarded. */
export function isFinal(change: Change): boolean {
  return (
    change.state === ChangeState.APPLIED ||
    change.state === ChangeState.FAILED ||
    change.state === ChangeState.DISCARDED
  );
}

/**
 * True while a change can still be undone: applied, not undone already, and
 * within its undo window. A rollback is Dockwell's own repair, which undoing
 * would only break again, so it offers none.
 */
export function canUndo(change: Change | undefined, now: number): boolean {
  if (!change || change.state !== ChangeState.APPLIED || change.undoneBy !== "" || change.rollback) return false;
  return change.undoDeadline !== undefined && timestampMs(change.undoDeadline) > now;
}

/** What runs: a service or a container, which is live once applied, or stops if deleted. */
export function runs(resource: PlannedResource): boolean {
  const kind = resource.resource?.kind;
  return kind === "service" || kind === "container";
}

/** What a plan leaves running, as what is live once it has applied. */
export function goesLive(plan: Plan | undefined): PlannedResource[] {
  return changing(plan).filter((r) => runs(r) && r.action !== PlanAction.DELETE);
}

/** What a plan stops for good: what runs, and is deleted. */
export function goesAway(plan: Plan | undefined): PlannedResource[] {
  return changing(plan).filter((r) => runs(r) && r.action === PlanAction.DELETE);
}

/** True for a plan that does nothing but delete. */
export function onlyDeletes(plan: Plan | undefined): boolean {
  const all = changing(plan);
  return all.length > 0 && all.every((r) => r.action === PlanAction.DELETE);
}

/** True for a plan that only makes what wasn't there, as a first deploy does: nothing ran before it. */
export function onlyCreates(plan: Plan | undefined): boolean {
  const s = plan?.summary;
  return s !== undefined && s.create > 0 && s.change === 0 && s.replace === 0 && s.destroy === 0;
}

/** True for a plan with nothing to do: nothing changes, and no server has a step. */
export function isEmpty(plan: Plan | undefined): boolean {
  return plan !== undefined && changing(plan).length === 0 && plan.servers.every((s) => s.steps.length === 0);
}

/** True when the server went away during a change's step, so nothing can say whether it ran until the server is back. */
export function wentOffline(change: Change | undefined): boolean {
  return failedStep(change)?.lost === true;
}

/**
 * What a pull found, when every step on every server left what runs exactly as
 * it was: a fact the steps report once they know, so what a plan expected doesn't
 * come into it. One server that replaced it is enough for this to say nothing.
 */
export function foundNothingNewer(change: Change | undefined): string {
  const steps = change?.steps ?? [];
  if (steps.length === 0 || !steps.every((s) => s.did === PlanAction.NONE)) return "";
  return steps[0]?.message ?? "";
}

/**
 * Whether a step replaces what runs only if its pull finds a newer image: it has
 * nothing to change that a container can't change in place. Anything else it
 * changes, such as a container that has stopped or a limit it no longer has, it
 * changes whatever the pull finds. The CLI's replacesIfNewer is the same rule.
 */
export function replacesIfNewer(step: PlanStep): boolean {
  if (!step.pull || step.action !== PlanAction.REPLACE) return false;
  return step.changes.every((c) => {
    const path = c.path.replace(/^app\./, "");
    if (path === "restart" || path === "restart_retries") return true;
    return path.startsWith("resources.") && IN_PLACE_LIMITS.has(path) && c.op !== FieldOp.REMOVED;
  });
}

/** The resources a plan replaces only if a pull finds a newer image, on every server it replaces them. */
function ifNewerIn(plan: Plan | undefined): ReadonlySet<string> {
  const steps = (plan?.servers ?? []).flatMap((s) => s.steps);
  const certain = new Set(
    steps.filter((s) => s.action === PlanAction.REPLACE && !replacesIfNewer(s)).map((s) => s.resourceId),
  );
  return new Set(steps.filter((s) => replacesIfNewer(s) && !certain.has(s.resourceId)).map((s) => s.resourceId));
}

/** Whether all a plan changes depends on its pull finding a newer image, so it may change nothing. */
export function onlyIfNewer(plan: Plan | undefined): boolean {
  const maybe = ifNewerIn(plan).size;
  return maybe > 0 && changing(plan).length === maybe;
}

/** The step of a change that failed, if one did: what the server said is on it. */
export function failedStep(change: Change | undefined): StepProgress | undefined {
  return change?.steps.find((s) => s.state === StepState.FAILED);
}

/** How a step of a plan is going, once its change has started. */
export function progressOf(
  progress: readonly StepProgress[],
  serverId: string,
  resourceId: string,
): StepProgress | undefined {
  return progress.find((p) => p.serverId === serverId && p.resourceId === resourceId);
}

/**
 * A plan's steps in the order they run, in groups. It is one group today, as
 * every server runs side by side. Waves (#28) become more of them, one a wave,
 * each with its label, its servers within it as they are now.
 */
export interface StepGroup {
  key: string;
  servers: readonly ServerPlan[];
}

export function stepGroups(plan: Plan | undefined): StepGroup[] {
  const servers = plan?.servers ?? [];
  return servers.length > 0 ? [{ key: "all", servers }] : [];
}

/** True once a plan has more than one server, which is when which one starts to matter (PRD principle 4). */
export function isFleet(plan: Plan | undefined): boolean {
  return (plan?.servers ?? []).length > 1;
}

/**
 * A message from the API without the IDs that say nothing to whoever reads it,
 * as an older agent's may have: "service web in env_01M…" reads "web".
 */
export function withoutIds(message: string): string {
  return message
    .replace(/^.*?\bisn't valid: /, "")
    .replace(/\bservice (\S+) in env_[0-9A-Za-z]+/g, "$1")
    .replace(/ in env_[0-9A-Za-z]+/g, "")
    .replace(/ \((?:in )?chg_[0-9A-Za-z]+\)/g, "");
}

/**
 * Splits a message from the server into its fact, to lead with, and what
 * follows: "x can't be pulled. Check the name." The full stop of a machine
 * value, as in nginx:1.27, has no space after it, so it never ends a sentence.
 */
/** A server's words as people read them: a quote inside what it quoted, such as a Dockerfile's step, without the backslash Go quotes it with. */
export function shownQuotes(text: string): string {
  return text.replace(/\\(["\\])/g, "$1");
}

export function factAndRest(message: string): [string, string] {
  // Go's errors start lowercase. A name a message starts with keeps its case, so only the common words change.
  const text = message.trim().replace(/^(the|a|an|another|this|there|can't)\b/, (word) => {
    return word.charAt(0).toUpperCase() + word.slice(1);
  });
  // What a health check's command printed comes in quotes, and its full stops aren't the server's.
  let quoted = false;
  for (const mark of text.matchAll(/\\.|"|[.!?](\s+|$)|。/g)) {
    if (mark[0] === '"') quoted = !quoted;
    if (quoted || mark[0] === '"' || mark[0].startsWith("\\")) continue;
    return [text.slice(0, mark.index), text.slice(mark.index + mark[0].length).trim()];
  }
  return [text, ""];
}
