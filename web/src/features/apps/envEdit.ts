// SPDX-License-Identifier: AGPL-3.0-only

import { clone, create, type MessageInitShape } from "@bufbuild/protobuf";
import {
  type EditSchema,
  type EnvironmentVariable,
  type ServiceInfo,
  ServiceSchema as SpecServiceSchema,
} from "@dockwell/api";

import { environmentOf, type ServiceValue, valuesOf } from "./app";

/**
 * One edit to an app's environment, not yet planned, by the name the app sees
 * it under:
 * - add makes a variable, or a secret, in the app's environment, and has the app use it;
 * - use has the app use one its environment has already;
 * - change gives a value the app has a new one: its environment's, for every app
 *   that uses it, or the app's own;
 * - remove takes one out of the app. One of its environment's stays there.
 */
export type EnvEdit =
  | { op: "add"; name: string; value: string; secret: boolean }
  | { op: "use"; name: string; from: string; secret: boolean }
  | { op: "change"; name: string; value: string }
  | { op: "remove"; name: string };

/** A row of the editor: what the app has, with what's pending on it. */
export interface EnvRow {
  name: string;
  /** What the app has now; undefined for one only pending. */
  now?: ServiceValue;
  pending?: EnvEdit;
  /** Its environment's variable or secret, as listed, when there is one. */
  stored?: EnvironmentVariable;
}

/** What the editor shows: the app's values and what's pending, by name. */
export function envRows(
  service: ServiceInfo,
  pending: ReadonlyMap<string, EnvEdit>,
  store: readonly EnvironmentVariable[] | undefined,
): EnvRow[] {
  const find = (name: string, secret: boolean) => store?.find((v) => v.name === name && v.secret === secret);
  const rows = new Map<string, EnvRow>();
  for (const now of valuesOf(service)) {
    rows.set(now.name, {
      name: now.name,
      now,
      stored: now.kind === "own" ? undefined : find(now.from, now.kind === "secret"),
    });
  }
  for (const [name, edit] of pending) {
    const row = rows.get(name) ?? { name };
    row.pending = edit;
    if (edit.op === "use") row.stored = find(edit.from, edit.secret);
    rows.set(name, row);
  }
  return [...rows.values()].sort((a, b) => a.name.localeCompare(b.name));
}

/** How many of each kind of edit are pending, as the bar counts them. */
export function envCounts(pending: ReadonlyMap<string, EnvEdit>): { added: number; changed: number; removed: number } {
  let added = 0;
  let changed = 0;
  let removed = 0;
  for (const edit of pending.values()) {
    if (edit.op === "add" || edit.op === "use") added++;
    else if (edit.op === "change") changed++;
    else removed++;
  }
  return { added, changed, removed };
}

/**
 * The other apps a pending change of an environment's value restarts, as they
 * use it too, by name, without the app being edited.
 */
export function alsoRestarts(service: ServiceInfo, rows: readonly EnvRow[]): { id: string; name: string }[] {
  const others = new Map<string, string>();
  for (const row of rows) {
    if (row.pending?.op !== "change" || !row.stored) continue;
    for (const use of row.stored.usedBy)
      if (use.serviceId !== service.id) others.set(use.serviceId, use.service || use.serviceId);
  }
  return [...others].map(([id, name]) => ({ id, name })).sort((a, b) => a.name.localeCompare(b.name));
}

/** The plan sheet's edit for a variable or secret of an environment. */
export function valueEdit(
  kind: "variable" | "secret",
  scope: string,
  name: string,
  value: string,
): MessageInitShape<typeof EditSchema> {
  return {
    resource: { kind, scope, name },
    spec:
      kind === "secret"
        ? { kind: { case: "secret", value: { value } } }
        : { kind: { case: "variable", value: { value } } },
  };
}

/**
 * The edits that plan what's pending: one for each variable or secret of the
 * environment it makes or changes, then the app's own, from the spec as it was
 * read and at the revision it was read at, so a change that landed meanwhile
 * makes it stale rather than undone. The app's own values never come back from
 * the API, so the edit names those it keeps, and sets only the new ones.
 */
export function envEdits(
  service: ServiceInfo,
  pending: ReadonlyMap<string, EnvEdit>,
): MessageInitShape<typeof EditSchema>[] {
  const scope = environmentOf(service);
  const now = new Map(valuesOf(service).map((v) => [v.name, v] as const));
  const edits: MessageInitShape<typeof EditSchema>[] = [];
  const spec = service.spec ? clone(SpecServiceSchema, service.spec) : create(SpecServiceSchema);
  const app = spec.runs.case === "app" ? spec.runs.value : undefined;
  const own: Record<string, string> = {};
  const dropped = new Set<string>();
  let touched = false;
  for (const [name, edit] of pending) {
    const was = now.get(name);
    switch (edit.op) {
      case "add":
        edits.push(valueEdit(edit.secret ? "secret" : "variable", scope, name, edit.value));
        (edit.secret ? spec.secrets : spec.variables)[name] = "";
        touched = true;
        break;
      case "use":
        (edit.secret ? spec.secrets : spec.variables)[name] = edit.from === name ? "" : edit.from;
        touched = true;
        break;
      case "change":
        if (!was) break;
        if (was.kind === "own") {
          own[name] = edit.value;
          touched = true;
        } else edits.push(valueEdit(was.kind, scope, was.from, edit.value));
        break;
      case "remove":
        if (!was) break;
        if (was.kind === "own") dropped.add(name);
        else delete (was.kind === "secret" ? spec.secrets : spec.variables)[name];
        touched = true;
        break;
    }
  }
  if (touched && app) {
    const keepEnv = Object.keys(app.env).filter((name) => !(name in own) && !dropped.has(name));
    app.env = own;
    edits.push({
      resource: service.resource,
      spec: { kind: { case: "service", value: spec } },
      baseRevision: service.revision,
      keepEnv: keepEnv.sort(),
    });
  }
  return edits;
}
