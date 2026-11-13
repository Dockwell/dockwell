// SPDX-License-Identifier: AGPL-3.0-only

import {
  CatalogFormat,
  type CatalogSourceGuess,
  type CatalogSourceInfo,
  type CatalogSourceRefusal,
} from "@dockwell/api";
import { SegmentedControl } from "@dockwell/ui";
import type { MessageDescriptor } from "@lingui/core";
import { msg, plural } from "@lingui/core/macro";
import { Trans, useLingui } from "@lingui/react/macro";
import { useMemo } from "react";

import { storeLabel } from "./storeMarks";

/**
 * Other stores' catalogs, read on the server (ADR 0078): a source may be 1Panel's
 * app store, Dokploy's templates or Coolify's, which Dockwell tells from the
 * repository and converts as it reads it. None of that is a step a person takes:
 * the link is the form, a store is one click (Stores.tsx), and the format shows only
 * as a fact, or as a choice where Dockwell can't tell.
 */

/** A repository's address as it compares: lowercase, with no trailing slash or .git. */
export function repositoryKey(url: string): string {
  return url
    .trim()
    .toLowerCase()
    .replace(/\/+$/, "")
    .replace(/\.git$/, "")
    .replace(/\/+$/, "");
}

/**
 * How a person says a source, never by its ID: the store's own name for a store read
 * at its own repository, as "1Panel", and for any other source its repository's
 * owner and name, as "acme/appstore". The ID is the CLI's, and a tooltip's.
 */
export function sourceWords(source: CatalogSourceInfo): string {
  const repository = source.source?.repository ?? "";
  // The control plane says which store a source reads, at the store's own repository alone (ADR 0083).
  const store = storeLabel(source.store);
  if (store) return store;
  try {
    const u = new URL(repositoryKey(repository));
    const parts = u.pathname.split("/").filter(Boolean);
    if (parts.length >= 2) return `${parts[0]}/${parts[1]}`;
    if (parts.length === 1) return `${u.host}/${parts[0]}`;
    return u.host || source.name;
  } catch {
    return source.name;
  }
}

/**
 * Each source's words, by its ID: two that would read alike say their folder too, or
 * their ID as a last resort, so no two read the same.
 */
export function sourcesWords(sources: readonly CatalogSourceInfo[]): ReadonlyMap<string, string> {
  const words = new Map(sources.map((s) => [s.name, sourceWords(s)]));
  const seen = new Map<string, number>();
  for (const w of words.values()) seen.set(w, (seen.get(w) ?? 0) + 1);
  for (const s of sources) {
    const w = words.get(s.name) ?? s.name;
    if ((seen.get(w) ?? 0) > 1) words.set(s.name, `${w} (${s.source?.path || s.name})`);
  }
  return words;
}

/** What each format reads as. */
const FORMATS: Readonly<Partial<Record<CatalogFormat, MessageDescriptor>>> = {
  [CatalogFormat.UNSPECIFIED]: msg`Dockwell's entries`,
  [CatalogFormat.DOCKWELL]: msg`Dockwell's entries`,
  [CatalogFormat.ONEPANEL]: msg`1Panel's app store`,
  [CatalogFormat.DOKPLOY]: msg`Dokploy's templates`,
  [CatalogFormat.COOLIFY]: msg`Coolify's templates`,
};

/** Says another store's format in the reader's words, or nothing for one the panel has no words for. */
export function useStoreFormatWords(): (format: CatalogFormat) => string | undefined {
  const { i18n } = useLingui();
  return (format) =>
    format === CatalogFormat.UNSPECIFIED || format === CatalogFormat.DOCKWELL || !FORMATS[format]
      ? undefined
      : i18n._(FORMATS[format]);
}

/** Says a format in the reader's words. */
export function useFormatWords(): (format: CatalogFormat) => string {
  const { i18n } = useLingui();
  return (format) => i18n._(FORMATS[format] ?? msg`Dockwell's entries`);
}

/** Whether a source is another store's, which Dockwell converts. */
export function converted(source: CatalogSourceInfo): boolean {
  const format = source.source?.format ?? CatalogFormat.UNSPECIFIED;
  return format !== CatalogFormat.UNSPECIFIED && format !== CatalogFormat.DOCKWELL;
}

/**
 * How a repository the form was given reads, by its folders: a line of fact where
 * Dockwell can tell, a choice where it could be more than one, and nothing where it
 * holds no catalog Dockwell knows, which the plan says.
 */
export function FormatLine({
  guesses,
  chosen,
  onChoose,
}: {
  guesses: readonly CatalogSourceGuess[];
  chosen: CatalogFormat | undefined;
  onChoose: (guess: CatalogSourceGuess) => void;
}) {
  const { t } = useLingui();
  const words = useFormatWords();
  if (guesses.length === 0) return null;
  if (guesses.length === 1) {
    const [only] = guesses as [CatalogSourceGuess];
    return (
      <p className="sources-format">
        <span>{words(only.format)}</span>
        <span className="sources-format-count">{plural(only.apps, { one: "# app", other: "# apps" })}</span>
      </p>
    );
  }
  const value = String(chosen ?? guesses[0]?.format ?? CatalogFormat.UNSPECIFIED);
  return (
    <SegmentedControl
      label={t`Read it as`}
      value={value}
      options={guesses.map((g) => ({ value: String(g.format), label: words(g.format) }))}
      onChange={(next) => {
        const guess = guesses.find((g) => String(g.format) === next);
        if (guess) onChoose(guess);
      }}
    />
  );
}

/** Why apps are left out, by the kind the control plane counts them as. */
const KINDS: Readonly<Record<string, MessageDescriptor>> = {
  scripts: msg`Run scripts on the server`,
  shipped: msg`Ship files beside their Compose file`,
  "another-app": msg`Use another app's database`,
  "no-version": msg`Name no version that stays put`,
  arch: msg`Run on amd64 alone`,
  platform: msg`Use a value their store fills in`,
  "cant-say": msg`Ask for what an entry can't say yet`,
  "cant-run": msg`Ask for what Dockwell can't run yet`,
  check: msg`Fail the catalog's checks`,
  file: msg`Have files that can't be read`,
  inside: msg`Put a secret inside a longer value`,
  "made-up": msg`Ask for a value Dockwell doesn't make yet`,
  skipped: msg`Marked by their store to leave out`,
  terms: msg`Come under terms of their own`,
  taken: msg`Have an ID a catalog before it has`,
};

/**
 * What would make the apps of a kind install, in a line, where it isn't plain from
 * the kind itself: what Dockwell would have to do, never what a person should.
 */
const NEEDS: Readonly<Record<string, MessageDescriptor>> = {
  scripts: msg`Install once Dockwell runs their setup steps itself`,
  shipped: msg`Install once Dockwell carries their files along`,
  "another-app": msg`Install once Dockwell connects them to a database`,
  "no-version": msg`Install once Dockwell pins their moving tag by digest`,
  arch: msg`Install once Dockwell offers them for amd64 servers`,
  platform: msg`Install once Dockwell fills in what their store does`,
  skipped: msg`Their store keeps them out of its own list`,
};

/** A group of what's left out: its kind, and the apps or files in it. */
interface Group {
  kind: string;
  items: CatalogSourceRefusal[];
}

/**
 * What a source leaves out, one step away: for another store's, counted by why, the
 * most first, each kind with what would make it install, opening to its apps and the
 * reason for each; for Dockwell's own entries, by file.
 */
export function LeftOut({ refused }: { refused: readonly CatalogSourceRefusal[] }) {
  const { i18n } = useLingui();
  const groups = useMemo(() => {
    const out = new Map<string, CatalogSourceRefusal[]>();
    for (const r of refused) out.set(r.kind, [...(out.get(r.kind) ?? []), r]);
    return [...out]
      .map(([kind, items]): Group => ({ kind, items }))
      .sort((a, b) => b.items.length - a.items.length || a.kind.localeCompare(b.kind));
  }, [refused]);
  const left = refused.length;
  if (left === 0) return null;
  if (groups.every(({ kind }) => kind === "")) {
    return (
      <details className="sources-left">
        <summary>{plural(left, { one: "# file left out, and why", other: "# files left out, and why" })}</summary>
        <ul className="sources-left-list">
          {refused.map((r) => (
            // An image can be left out, and lost by the entry that names it: one file, two reasons.
            <li key={`${r.file}\n${r.reason}`}>
              <code>{r.file}</code>
              <span>{r.reason}</span>
            </li>
          ))}
        </ul>
      </details>
    );
  }
  return (
    <details className="sources-left">
      <summary>{plural(left, { one: "# app left out, by why", other: "# apps left out, by why" })}</summary>
      <div className="sources-left-kinds">
        {groups.map(({ kind, items }) => (
          <details key={kind} className="sources-left-kind">
            <summary>
              <span className="sources-left-why">
                <span>{KINDS[kind] ? i18n._(KINDS[kind]) : kind || <Trans>Other reasons</Trans>}</span>
                {NEEDS[kind] ? <span className="sources-left-needs">{i18n._(NEEDS[kind])}</span> : null}
              </span>
              <span className="sources-left-count">{items.length}</span>
            </summary>
            <ul className="sources-left-list">
              {items.map((r) => (
                <li key={`${r.id}:${r.file}`}>
                  <code title={r.file}>{r.id || r.file}</code>
                  <span>{r.reason}</span>
                </li>
              ))}
            </ul>
          </details>
        ))}
      </div>
    </details>
  );
}
