// SPDX-License-Identifier: AGPL-3.0-only

import {
  CatalogFormat,
  type CatalogSourceGuess,
  type CatalogSourceInfo,
  type CatalogSourceRefusal,
} from "@dockwell/api";
import { Button, SegmentedControl } from "@dockwell/ui";
import type { MessageDescriptor } from "@lingui/core";
import { msg, plural } from "@lingui/core/macro";
import { Trans, useLingui } from "@lingui/react/macro";
import { useMemo } from "react";

/**
 * Other stores' catalogs, read on the server (ADR 0078): a source may be 1Panel's
 * app store, Dokploy's templates or Coolify's, which Dockwell tells from the
 * repository and converts as it reads it. None of that is a step a person takes:
 * the link is the form, a preset is one click, and the format shows only as a fact,
 * or as a choice where Dockwell can't tell.
 */

/** A store a person adds in one click, with its upstream filled in. */
export interface Preset {
  id: string;
  name: MessageDescriptor;
  /** What a person calls the store, as a source of it reads. */
  label: string;
  repository: string;
  format: CatalogFormat;
}

/** The stores Dockwell reads directly, as presets. */
export const PRESETS: readonly Preset[] = [
  {
    id: "onepanel",
    name: msg`1Panel's app store`,
    label: "1Panel",
    repository: "https://github.com/1Panel-dev/appstore",
    format: CatalogFormat.ONEPANEL,
  },
  {
    id: "dokploy",
    name: msg`Dokploy's templates`,
    label: "Dokploy",
    repository: "https://github.com/Dokploy/templates",
    format: CatalogFormat.DOKPLOY,
  },
  {
    id: "coolify",
    name: msg`Coolify's templates`,
    label: "Coolify",
    repository: "https://github.com/coollabsio/coolify",
    format: CatalogFormat.COOLIFY,
  },
];

/** A repository's address as it compares: lowercase, with no trailing slash or .git. */
function repositoryKey(url: string): string {
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
  const format = source.source?.format ?? CatalogFormat.UNSPECIFIED;
  const preset = PRESETS.find((p) => p.format === format && repositoryKey(p.repository) === repositoryKey(repository));
  if (preset) return preset.label;
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
 * The stores a person can add in one click: those no source reads yet. Each opens
 * the source form with its link filled in, and the plan follows from there.
 */
export function SourcePresets({
  sources,
  off,
  onPick,
}: {
  sources: readonly CatalogSourceInfo[];
  /** Spread onto each button: off, with why, for whoever may not add a source. */
  off: { off?: string; disabled?: true };
  onPick: (preset: Preset) => void;
}) {
  const { t, i18n } = useLingui();
  const left = PRESETS.filter((p) => !sources.some((s) => s.source?.repository === p.repository));
  if (left.length === 0) return null;
  return (
    <fieldset className="sources-presets" aria-label={t`Add another store's apps`}>
      {left.map((preset) => (
        <Button key={preset.id} size="sm" icon="plus" {...off} onClick={() => onPick(preset)}>
          {i18n._(preset.name)}
        </Button>
      ))}
    </fieldset>
  );
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
 * What a source leaves out, one step away: by file for Dockwell's own entries, and
 * for another store's, counted by why, each kind opening to its apps and their
 * reasons, the most first.
 */
export function LeftOut({ refused }: { refused: readonly CatalogSourceRefusal[] }) {
  const { i18n } = useLingui();
  const groups = useMemo(() => {
    const out = new Map<string, CatalogSourceRefusal[]>();
    for (const r of refused) out.set(r.kind, [...(out.get(r.kind) ?? []), r]);
    return [...out].sort((a, b) => b[1].length - a[1].length || a[0].localeCompare(b[0]));
  }, [refused]);
  const left = refused.length;
  if (left === 0) return null;
  if (groups.every(([kind]) => kind === "")) {
    return (
      <details className="sources-left">
        <summary>{plural(left, { one: "# file left out, and why", other: "# files left out, and why" })}</summary>
        <ul>
          {refused.map((r) => (
            <li key={r.file}>
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
      <summary>{plural(left, { one: "# app left out, and why", other: "# apps left out, and why" })}</summary>
      {groups.map(([kind, apps]) => (
        <details key={kind} className="sources-left-kind">
          <summary>
            <span>{KINDS[kind] ? i18n._(KINDS[kind]) : kind || <Trans>Other reasons</Trans>}</span>
            <span className="sources-left-count">{apps.length}</span>
          </summary>
          <ul>
            {apps.map((r) => (
              <li key={`${r.id}:${r.file}`}>
                <code>{r.id || r.file}</code>
                <span>{r.reason}</span>
              </li>
            ))}
          </ul>
        </details>
      ))}
    </details>
  );
}
