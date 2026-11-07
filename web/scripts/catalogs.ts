// SPDX-License-Identifier: AGPL-3.0-only

// Merges two versions of a message catalog from the one they started from, for
// merge-catalogs.ts, which has the tests in catalogs.test.ts.

import type { CatalogType, MessageType } from "@lingui/conf";
import { formatter } from "@lingui/format-po";

const po = formatter({ origins: false });

/** A catalog's three versions, as PO text: each missing where that side has no catalog. */
export interface Sides {
  base?: string;
  ours?: string;
  theirs?: string;
}

export interface Context {
  locale: string;
  sourceLocale: string;
  filename: string;
}

export interface Merged {
  /** The catalog with every message either side has, and its header as it was. */
  text: string;
  /** Each message's translation after the merge: "" where a side cleared it. */
  chosen: Map<string, string>;
  /** The messages each side translated differently, which only a person can choose between. */
  disagree: string[];
}

async function parse(text: string | undefined, ctx: Context): Promise<CatalogType> {
  return text === undefined ? {} : await po.parse(text, ctx);
}

/**
 * The translation each side ends with. A message missing on one side takes the
 * other's. One that a side changed from the base, cleared to "" included, takes
 * that side's. When both changed it differently, there's no answer.
 */
function translation(base?: MessageType, ours?: MessageType, theirs?: MessageType): string | undefined | null {
  if (!ours) return theirs?.translation;
  if (!theirs) return ours.translation;
  const [b, o, t] = [base?.translation, ours.translation ?? "", theirs.translation ?? ""];
  if (o === t) return o;
  if (base && o === (b ?? "")) return t;
  if (base && t === (b ?? "")) return o;
  return null;
}

/**
 * The messages a catalog has more than once with different translations, as a
 * clean git merge leaves one two branches each added: parsing it keeps only the
 * last copy, so they're found in its text, an entry at a time.
 */
async function twice(text: string | undefined, ctx: Context, side: string): Promise<string[]> {
  if (text === undefined) return [];
  const seen = new Map<string, { message: string; translations: Set<string> }>();
  for (const block of text.split(/\n[ \t]*\n/)) {
    if (!/^msgid /m.test(block) || /^msgid ""\nmsgstr ""\n"/m.test(block)) continue;
    for (const [id, message] of Object.entries(await po.parse(block, ctx))) {
      const entry = seen.get(id) ?? { message: message.message ?? id, translations: new Set<string>() };
      entry.translations.add(message.translation ?? "");
      seen.set(id, entry);
    }
  }
  return [...seen.values()]
    .filter(({ translations }) => translations.size > 1)
    .map(
      ({ message, translations }) =>
        `"${message}": ${[...translations].map((t) => `"${t}"`).join(" and ")}, both on ${side}`,
    );
}

/** A PO header's fields, in order: each "Name: value" line of the entry with the empty msgid. */
function headerFields(text: string | undefined): Map<string, string> {
  const fields = new Map<string, string>();
  const header = text?.split(/\n[ \t]*\n/).find((block) => /^msgid ""\nmsgstr ""/m.test(block)) ?? "";
  for (const [, name, value] of header.matchAll(/^"([^:"]+): ?(.*?)\\n"$/gm)) {
    if (name !== undefined && value !== undefined) fields.set(name, value);
  }
  return fields;
}

/**
 * The header, merged a field at a time: one a side changed takes that side's,
 * and POT-Creation-Date stays as the base had it, so a merge made later doesn't
 * move it.
 */
function mergeHeader(sides: Sides): string {
  const [base, ours, theirs] = [headerFields(sides.base), headerFields(sides.ours), headerFields(sides.theirs)];
  const lines: string[] = [];
  for (const name of new Set([...base.keys(), ...ours.keys(), ...theirs.keys()])) {
    const [b, o, t] = [base.get(name), ours.get(name), theirs.get(name)];
    const value = name === "POT-Creation-Date" ? (b ?? o ?? t) : o === b ? t : o;
    if (value !== undefined) lines.push(`"${name}: ${value}\\n"`);
  }
  return ['msgid ""', 'msgstr ""', ...lines].join("\n");
}

/** Merges ours and theirs from base, with the header merged a field at a time. */
export async function mergeCatalog(sides: Sides, ctx: Context): Promise<Merged> {
  const [base, ours, theirs] = await Promise.all([sides.base, sides.ours, sides.theirs].map((s) => parse(s, ctx)));
  const merged: CatalogType = {};
  const chosen = new Map<string, string>();
  const disagree = [...(await twice(sides.ours, ctx, "one side")), ...(await twice(sides.theirs, ctx, "the other"))];
  for (const id of new Set([...Object.keys(ours ?? {}), ...Object.keys(theirs ?? {})])) {
    const [b, o, t] = [base?.[id], ours?.[id], theirs?.[id]];
    const text = translation(b, o, t);
    if (text === null) {
      disagree.push(`"${o?.message ?? t?.message}": "${o?.translation}" on one side, "${t?.translation}" on the other`);
      continue;
    }
    merged[id] = { ...(t ?? o), translation: text };
    if (text !== undefined) chosen.set(id, text);
  }
  return { text: await po.serialize(merged, { ...ctx, existing: mergeHeader(sides) }), chosen, disagree };
}

/**
 * Fills each message lingui extract left untranslated with the translation the
 * merge chose for it, or any side had, unless the merge chose to clear it.
 * Returns the catalog, and how many it filled.
 */
export async function refill(
  extracted: string,
  merged: Merged,
  sides: Sides,
  ctx: Context,
): Promise<{ text: string; filled: number }> {
  const catalog = await po.parse(extracted, ctx);
  const known = new Map<string, string>();
  for (const side of [sides.base, sides.ours, sides.theirs]) {
    for (const [id, message] of Object.entries(await parse(side, ctx))) {
      if (message.translation) known.set(id, message.translation);
    }
  }
  let filled = 0;
  for (const [id, message] of Object.entries(catalog)) {
    if (message.translation) continue;
    if (merged.chosen.get(id) === "") continue;
    const text = merged.chosen.get(id) || known.get(id);
    if (!text) continue;
    message.translation = text;
    filled++;
  }
  if (filled === 0) return { text: extracted, filled };
  return { text: await po.serialize(catalog, { ...ctx, existing: extracted }), filled };
}
