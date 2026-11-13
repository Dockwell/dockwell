// SPDX-License-Identifier: AGPL-3.0-only

import { clone, create, type MessageInitShape } from "@bufbuild/protobuf";
import {
  type EditSchema,
  type EnvironmentVariable,
  type ServiceInfo,
  ServiceSchema as SpecServiceSchema,
} from "@dockwell/api";

import { environmentOf, type ServiceValue, valuesOf } from "./app";
import { runtimeEnv } from "./runtime";

/**
 * One edit to an app's environment, not yet planned, by the name the app sees
 * it under:
 * - add sets a value on the app, shown as it is, or, as a secret, makes one in
 *   the app's environment and has the app use it;
 * - use has the app use one its environment has already;
 * - change gives a value the app has a new one: its environment's, for every app
 *   that uses it, or the app's own, which stays the app's, shown, or hidden where
 *   hidden says, or becomes a secret of its environment the app uses, as secret
 *   says;
 * - share makes one of the app's own, shown, a variable of its environment the
 *   app uses, which other apps can use too;
 * - secret makes one of the app's own a secret of its environment the app uses,
 *   with the value it has, which the control plane moves, so a hidden one needn't
 *   be typed again (ADR 0081);
 * - remove takes one out of the app. One of its environment's stays there.
 */
export type EnvEdit =
  | { op: "add"; name: string; value: string; secret: boolean }
  | { op: "use"; name: string; from: string; secret: boolean }
  | { op: "change"; name: string; value: string; secret?: boolean; hidden?: boolean }
  | { op: "share"; name: string; value: string }
  | { op: "secret"; name: string }
  | { op: "remove"; name: string };

/** A row of the editor: what the app has, with what's pending on it. */
export interface EnvRow {
  name: string;
  /** What the app has now; undefined for one only pending. */
  now?: ServiceValue;
  pending?: EnvEdit;
  /** Its environment's variable or secret, as listed, when there is one. */
  stored?: EnvironmentVariable;
  /** A nonsecret default supplied by its runtime, rather than a stored variable. */
  runtimeValue?: string;
}

/** What the editor shows: the app's values and what's pending, by name. */
export function envRows(
  service: ServiceInfo,
  pending: ReadonlyMap<string, EnvEdit>,
  store: readonly EnvironmentVariable[] | undefined,
): EnvRow[] {
  const find = (name: string, secret: boolean) => store?.find((v) => v.name === name && v.secret === secret);
  const rows = new Map<string, EnvRow>();
  const defaults = runtimeEnv(service);
  for (const now of valuesOf(service, new Set(defaults.keys()))) {
    rows.set(now.name, {
      name: now.name,
      now,
      stored: now.kind === "variable" || now.kind === "secret" ? find(now.from, now.kind === "secret") : undefined,
      runtimeValue: now.kind === "runtime" ? defaults.get(now.name) : undefined,
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
    else if (edit.op === "change" || edit.op === "share" || edit.op === "secret") changed++;
    else removed++;
  }
  return { added, changed, removed };
}

/**
 * Why one of the app's own values can't be made a secret of its environment as it
 * is, or undefined when it can: what the environment has isn't read yet, or it has
 * a secret or a variable of that name, which making one would change for every
 * app that uses it.
 */
export function cantMakeSecret(
  row: EnvRow,
  store: readonly EnvironmentVariable[] | undefined,
): "reading" | "secret" | "variable" | undefined {
  if (store === undefined) return "reading";
  const there = store.find((v) => v.name === row.name);
  if (!there) return undefined;
  return there.secret ? "secret" : "variable";
}

/**
 * Why one of the app's own values can't be made a variable of its environment
 * as it is, or undefined when it can: its value is hidden, what the environment
 * has isn't read yet, so a variable of that name might be there, or one of that
 * name there already holds something else, or is a secret.
 */
export function cantShare(
  row: EnvRow,
  store: readonly EnvironmentVariable[] | undefined,
): "hidden" | "reading" | "secret" | "other" | undefined {
  if (row.now?.value === undefined) return "hidden";
  if (store === undefined) return "reading";
  const there = store.find((v) => v.name === row.name);
  if (!there) return undefined;
  if (there.secret) return "secret";
  return there.value === row.now.value ? undefined : "other";
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
 * makes it stale rather than undone. An app's own value that may be a secret
 * never comes back from the API, so the edit names those it keeps, and sets
 * only the new ones. A value it sets on the app is one a person typed, as not a
 * secret, so it shows (ADR 0081). A secret moves to the environment instead, as
 * there it takes a passkey to reveal; store is what the environment has, so an
 * own value made a variable of the same value uses that one as it is.
 */
export function envEdits(
  service: ServiceInfo,
  pending: ReadonlyMap<string, EnvEdit>,
  store?: readonly EnvironmentVariable[],
): MessageInitShape<typeof EditSchema>[] {
  const scope = environmentOf(service);
  const now = new Map(valuesOf(service).map((v) => [v.name, v] as const));
  const edits: MessageInitShape<typeof EditSchema>[] = [];
  const spec = service.spec ? clone(SpecServiceSchema, service.spec) : create(SpecServiceSchema);
  const app = spec.runs.case === "app" ? spec.runs.value : undefined;
  const own: Record<string, string> = {};
  const dropped = new Set<string>();
  const moved: string[] = [];
  const shown = new Set(spec.shownEnv);
  let touched = false;
  const setOwn = (name: string, value: string) => {
    own[name] = value;
    shown.add(name);
    touched = true;
  };
  const toSecret = (name: string, value: string) => {
    edits.push(valueEdit("secret", scope, name, value));
    spec.secrets[name] = "";
    touched = true;
  };
  for (const [name, edit] of pending) {
    const was = now.get(name);
    switch (edit.op) {
      case "add":
        if (edit.secret) toSecret(name, edit.value);
        else setOwn(name, edit.value);
        break;
      case "use":
        (edit.secret ? spec.secrets : spec.variables)[name] = edit.from === name ? "" : edit.from;
        touched = true;
        break;
      case "change":
        if (!was) break;
        if (was.kind === "own" || was.kind === "runtime") {
          if (edit.secret) {
            dropped.add(name);
            toSecret(name, edit.value);
          } else if (edit.hidden) {
            // A new value for one that's hidden stays hidden: nothing here says it holds no secret.
            own[name] = edit.value;
            shown.delete(name);
            touched = true;
          } else setOwn(name, edit.value);
        } else edits.push(valueEdit(was.kind, scope, was.from, edit.value));
        break;
      case "share": {
        if (was?.kind !== "own") break;
        const there = store?.find((v) => v.name === name && !v.secret);
        if (there?.value !== edit.value) edits.push(valueEdit("variable", scope, name, edit.value));
        dropped.add(name);
        spec.variables[name] = "";
        touched = true;
        break;
      }
      case "secret":
        if (was?.kind !== "own") break;
        moved.push(name);
        dropped.add(name);
        touched = true;
        break;
      case "remove":
        if (!was) break;
        if (was.kind === "own" || was.kind === "runtime") dropped.add(name);
        else delete (was.kind === "secret" ? spec.secrets : spec.variables)[name];
        touched = true;
        break;
    }
  }
  if (touched && app) {
    const keepEnv = Object.keys(app.env).filter((name) => !(name in own) && !dropped.has(name));
    app.env = own;
    for (const name of dropped) shown.delete(name);
    spec.shownEnv = [...shown].sort();
    edits.push({
      resource: service.resource,
      spec: { kind: { case: "service", value: spec } },
      baseRevision: service.revision,
      keepEnv: keepEnv.sort(),
      ...(moved.length > 0 ? { envToSecrets: moved.sort() } : {}),
    });
  }
  return edits;
}
