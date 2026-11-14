// SPDX-License-Identifier: AGPL-3.0-only

import type { Server } from "@dockwell/api";

/**
 * Servers' labels as the panel writes them, by the rules internal/labels and
 * internal/resource/labels.go keep on the server (ADR 0091): a label is
 * key:value, or a key alone for an empty value, and a selector is such terms with
 * spaces between, where a key alone matches a server that has it and ! in front
 * matches one that doesn't. A key is a Kubernetes label's: a name of at most 63
 * characters, letters, digits, -, _ and ., starting and ending with a letter or
 * digit, with an optional prefix, a DNS subdomain of at most 253 characters, and /.
 * A value is empty or a name. The checks here say what's wrong as the server
 * would, so a person reads it as they type.
 */

/** How long a name may be: a key's after its prefix, or a value. */
export const LABEL_NAME = 63;
/** How long a key's prefix may be. */
export const LABEL_PREFIX = 253;
/** How many labels a server may have. */
export const MAX_LABELS = 64;

/** What's wrong with a label, by what a check found, for words of the panel's own. */
export type LabelProblem =
  | { kind: "no-key" }
  | { kind: "empty-prefix" }
  | { kind: "long-prefix"; length: number }
  | { kind: "prefix-not-domain" }
  | { kind: "reserved-prefix"; prefix: string }
  | { kind: "no-name" }
  | { kind: "long"; length: number }
  | { kind: "ends" }
  | { kind: "character"; character: string }
  | { kind: "too-many" };

/** A label, read. */
export interface Label {
  key: string;
  value: string;
}

/** One term of a selector. */
export interface Term {
  key: string;
  /** What the key must be, when hasValue; otherwise it need only be there. */
  value: string;
  hasValue: boolean;
  /** Matches a server the rest doesn't. */
  not: boolean;
}

function alnum(c: string): boolean {
  return /^[A-Za-z0-9]$/.test(c);
}

function nameProblem(name: string): LabelProblem | undefined {
  if (name === "") return { kind: "no-name" };
  if (name.length > LABEL_NAME) return { kind: "long", length: name.length };
  // What it can't hold at all comes before where it may not stand, as the server says it.
  for (const c of name) {
    if (!alnum(c) && c !== "-" && c !== "_" && c !== ".") return { kind: "character", character: c };
  }
  if (!alnum(name[0] ?? "") || !alnum(name[name.length - 1] ?? "")) return { kind: "ends" };
  return undefined;
}

/** The prefix Dockwell keeps for its own labels, with each of its subdomains. */
export const RESERVED_PREFIX = "dockwell.io";

/** Whether a key's prefix is one Dockwell keeps for its own labels, as internal/resource/labels.go says. */
export function reservedPrefix(prefix: string): boolean {
  return prefix === RESERVED_PREFIX || prefix.endsWith(`.${RESERVED_PREFIX}`);
}

function dnsSubdomain(s: string): boolean {
  return s.split(".").every((part) => part !== "" && part.length <= 63 && /^[a-z0-9]([a-z0-9-]*[a-z0-9])?$/.test(part));
}

/** What's wrong with key as a label's key, or undefined. */
export function keyProblem(key: string): LabelProblem | undefined {
  if (key === "") return { kind: "no-key" };
  const slash = key.indexOf("/");
  if (slash < 0) return nameProblem(key);
  const prefix = key.slice(0, slash);
  if (prefix === "") return { kind: "empty-prefix" };
  if (prefix.length > LABEL_PREFIX) return { kind: "long-prefix", length: prefix.length };
  if (!dnsSubdomain(prefix)) return { kind: "prefix-not-domain" };
  if (reservedPrefix(prefix)) return { kind: "reserved-prefix", prefix };
  return nameProblem(key.slice(slash + 1));
}

/** What's wrong with value as a label's value, or undefined: empty is a value. */
export function valueProblem(value: string): LabelProblem | undefined {
  return value === "" ? undefined : nameProblem(value);
}

/** Reads one label, key:value or a key alone, or says what's wrong with it. */
export function parseLabel(text: string): Label | LabelProblem {
  const s = text.trim();
  const colon = s.indexOf(":");
  const key = colon < 0 ? s : s.slice(0, colon);
  const value = colon < 0 ? "" : s.slice(colon + 1);
  return keyProblem(key) ?? valueProblem(value) ?? { key, value };
}

/** Whether what parseLabel or parseTerm gave is a problem. */
export function isProblem(x: Label | Term | LabelProblem): x is LabelProblem {
  return "kind" in x;
}

/** A label as key:value, or the key alone for an empty value. */
export function labelText(key: string, value: string): string {
  return value === "" ? key : `${key}:${value}`;
}

/** Labels as key:value, by key. */
export function labelTexts(labels: Readonly<Record<string, string>>): string[] {
  return Object.keys(labels)
    .sort()
    .map((k) => labelText(k, labels[k] ?? ""));
}

/** Reads one term of a selector, such as pool:web, region or !draining. */
export function parseTerm(text: string): Term | LabelProblem {
  let s = text.trim();
  const not = s.startsWith("!");
  if (not) s = s.slice(1);
  const colon = s.indexOf(":");
  const key = colon < 0 ? s : s.slice(0, colon);
  const value = colon < 0 ? "" : s.slice(colon + 1);
  return keyProblem(key) ?? valueProblem(value) ?? { key, value, hasValue: colon >= 0, not };
}

/** A term as parseTerm reads it. */
export function termText(term: Term): string {
  return `${term.not ? "!" : ""}${term.key}${term.hasValue ? `:${term.value}` : ""}`;
}

/** A term of a selector that can't be read, as it was written, and what's wrong with it. */
export interface BadTerm {
  text: string;
  problem: LabelProblem;
}

/**
 * A selector, read: the terms it could read, and those it couldn't. A term it can't
 * read matches no server, rather than be left out: leaving it out would widen the
 * set, and whatever acts on the set would act on more servers than were meant.
 */
export interface Selector {
  terms: Term[];
  bad: BadTerm[];
}

/** Reads a selector, terms with spaces between, keeping each term it can't read. */
export function parseSelector(text: string): Selector {
  const out: Selector = { terms: [], bad: [] };
  for (const part of text.split(/\s+/).filter(Boolean)) {
    const term = parseTerm(part);
    if (isProblem(term)) out.bad.push({ text: part, problem: term });
    else out.terms.push(term);
  }
  return out;
}

/** A selector as parseSelector reads it, with its terms, then those it can't read, as written. */
export function selectorText(terms: readonly Term[], bad: readonly BadTerm[] = []): string {
  return [...terms.map(termText), ...bad.map((b) => b.text)].join(" ");
}

/** Whether labels match a selector: none do while it has a term it can't read. */
export function matchesSelector(selector: Selector, labels: Readonly<Record<string, string>>): boolean {
  return selector.bad.length === 0 && matches(selector.terms, labels);
}

/** Whether labels match every term. */
export function matches(terms: readonly Term[], labels: Readonly<Record<string, string>>): boolean {
  return terms.every((t) => {
    const has = Object.hasOwn(labels, t.key);
    const match = has && (!t.hasValue || labels[t.key] === t.value);
    return match !== t.not;
  });
}

/** Every key the fleet's servers have, each with its values, by how many servers have it. */
export function fleetLabels(servers: readonly Server[]): Map<string, string[]> {
  const counts = new Map<string, Map<string, number>>();
  for (const s of servers) {
    for (const [k, v] of Object.entries(s.labels)) {
      const values = counts.get(k) ?? new Map<string, number>();
      values.set(v, (values.get(v) ?? 0) + 1);
      counts.set(k, values);
    }
  }
  const total = (values: Map<string, number>) => [...values.values()].reduce((a, b) => a + b, 0);
  const keys = [...counts.keys()].sort((a, b) => {
    const by = total(counts.get(b) ?? new Map()) - total(counts.get(a) ?? new Map());
    return by !== 0 ? by : a.localeCompare(b);
  });
  return new Map(
    keys.map((k) => [
      k,
      [...(counts.get(k) ?? new Map<string, number>()).keys()].sort((a, b) =>
        a.localeCompare(b, undefined, { numeric: true }),
      ),
    ]),
  );
}

/**
 * What to offer as someone types a label: each label the fleet uses, as key:value,
 * then each key alone with its colon, for a value of their own. Those a server has
 * already are left out.
 */
export function labelSuggestions(fleet: Map<string, string[]>, has: Readonly<Record<string, string>> = {}): string[] {
  const out: string[] = [];
  for (const [k, values] of fleet) {
    for (const v of values) if (has[k] !== v) out.push(labelText(k, v));
  }
  for (const k of fleet.keys()) out.push(`${k}:`);
  return out;
}

/** How many servers have no region label, for a lead that counts the regions servers are in. */
export function withoutRegion(servers: readonly Server[]): number {
  return servers.filter((s) => !Object.hasOwn(s.labels, "region")).length;
}

/** The key the fleet groups by first: region where a server has one, then the key most servers have. */
export function defaultGroupKey(fleet: Map<string, string[]>): string | undefined {
  if (fleet.has("region")) return "region";
  return fleet.keys().next().value;
}

/** A group of servers by one key's value, or of those without the key, last. */
export interface LabelGroup {
  /** The value, or undefined for servers without the key. */
  value: string | undefined;
  servers: Server[];
}

/** Servers by their value of key, by value, with those that have none last. */
export function groupBy(servers: readonly Server[], key: string): LabelGroup[] {
  const groups = new Map<string, Server[]>();
  const none: Server[] = [];
  for (const s of servers) {
    const v = s.labels[key];
    if (v === undefined) none.push(s);
    else groups.set(v, [...(groups.get(v) ?? []), s]);
  }
  const out: LabelGroup[] = [...groups.keys()]
    .sort((a, b) => a.localeCompare(b, undefined, { numeric: true }))
    .map((value) => ({ value, servers: groups.get(value) ?? [] }));
  if (none.length > 0) out.push({ value: undefined, servers: none });
  return out;
}

/**
 * What a change from labels to next sets and removes, as PlanServerLabels takes
 * them: each label that's new or has a new value, and each key that's gone.
 */
export function labelChanges(
  labels: Readonly<Record<string, string>>,
  next: Readonly<Record<string, string>>,
): { set: string[]; remove: string[] } {
  const set = Object.keys(next)
    .filter((k) => labels[k] !== next[k])
    .sort()
    .map((k) => labelText(k, next[k] ?? ""));
  const remove = Object.keys(labels)
    .filter((k) => !Object.hasOwn(next, k))
    .sort();
  return { set, remove };
}
