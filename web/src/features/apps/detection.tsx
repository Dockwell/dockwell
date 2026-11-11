// SPDX-License-Identifier: AGPL-3.0-only

import { type BuildDetection, type DetectedPackage, DetectionProblem, ImageBuild_Builder } from "@dockwell/api";
import { Trans, useLingui } from "@lingui/react/macro";
import { Fragment, type ReactNode, useCallback } from "react";

/** What Railpack calls a source, as people name it: node is Node, golang is Go. */
const providerNames: Readonly<Record<string, string>> = {
  node: "Node",
  python: "Python",
  golang: "Go",
  go: "Go",
  php: "PHP",
  ruby: "Ruby",
  java: "Java",
  rust: "Rust",
  elixir: "Elixir",
  deno: "Deno",
  dotnet: ".NET",
  gleam: "Gleam",
  cpp: "C++",
  zig: "Zig",
  shell: "Shell",
};

/** The tool each provider installs, whose version says which, such as node for Node. */
const providerTools: Readonly<Record<string, string>> = { golang: "go", staticfile: "caddy" };

/** A version as people say it: 22 for Node's 22.23.4, 3.13 for Python's 3.13.9. */
export function shortVersion(version: string): string {
  const parts = version.split(".");
  const major = Number(parts[0]);
  if (parts.length === 1 || Number.isNaN(major)) return version;
  return major >= 10 ? (parts[0] ?? version) : parts.slice(0, 2).join(".");
}

/** The package a provider is named by, such as Node's node. */
function providerPackage(d: BuildDetection, provider: string): DetectedPackage | undefined {
  const tool = providerTools[provider] ?? provider;
  return d.packages.find((p) => p.name === tool);
}

/** Railpack's words, sentence-cased, as they end a sentence of their own. */
function asSentence(s: string): string {
  const trimmed = s.trim();
  return /[.!?]$/.test(trimmed) ? trimmed : `${trimmed}.`;
}

/** Each of a list's items, as code, between commas. */
function codes(items: readonly string[]): ReactNode {
  // A command may come twice, so each is keyed by how many came before it too.
  const seen = new Map<string, number>();
  return items.map((c) => {
    const n = seen.get(c) ?? 0;
    seen.set(c, n + 1);
    const first = seen.size === 1 && n === 0;
    return (
      <Fragment key={`${c}#${n}`}>
        {first ? null : ", "}
        <code>{c}</code>
      </Fragment>
    );
  });
}

/** Words for what reading a commit found (ADR 0046). */
export function useDetectionWords() {
  const { t } = useLingui();
  /** What the source is, as Railpack read it: "Node 22", or "a static site". */
  const what = useCallback(
    (d: BuildDetection): string => {
      const provider = d.providers[0] ?? "";
      if (provider === "staticfile") return t`Static site`;
      const name = providerNames[provider] ?? provider;
      const pkg = providerPackage(d, provider);
      const version = pkg ? shortVersion(pkg.requested || pkg.resolved) : "";
      return [name, version].filter(Boolean).join(" ");
    },
    [t],
  );
  /** The headline: "Dockerfile · node:22-alpine", or "Railpack · Node 22". */
  const headline = useCallback(
    (d: BuildDetection): string => {
      if (d.builder === ImageBuild_Builder.DOCKERFILE) {
        return [t`Dockerfile`, d.baseImage || `./${d.dockerfile}`].join(" · ");
      }
      const found = what(d);
      return found ? [t`Railpack`, found].join(" · ") : t`Railpack`;
    },
    [t, what],
  );
  /** What a Railpack build installs, runs and starts with, in a sentence or two. */
  const steps = useCallback((d: BuildDetection): ReactNode => {
    if (d.builder !== ImageBuild_Builder.RAILPACK || d.error) return null;
    const commands = d.steps.flatMap((s) => s.commands);
    const start = d.startCommand;
    if (commands.length > 0 && start) {
      return (
        <Trans>
          Runs {codes(commands)}, then starts with <code>{start}</code>.
        </Trans>
      );
    }
    if (start) {
      return (
        <Trans>
          Starts with <code>{start}</code>.
        </Trans>
      );
    }
    if (commands.length > 0) {
      return <Trans>Runs {codes(commands)}, and found nothing to start the app with: set a start command.</Trans>;
    }
    return <Trans>Found nothing to start the app with: set a start command.</Trans>;
  }, []);
  /** Where each package's version came from: "node 22.23.4, from package.json > engines > node". */
  const packages = useCallback(
    (d: BuildDetection): string[] =>
      d.packages.map((p) =>
        p.source ? t`${p.name} ${p.resolved}, from ${p.source}` : [p.name, p.resolved].filter(Boolean).join(" "),
      ),
    [t],
  );
  /** Railpack's warnings, in its words. */
  const warnings = useCallback((d: BuildDetection): string[] => d.warnings.map(asSentence), []);
  /** Why Railpack can't build it: the first line of what it said. */
  const cant = useCallback((d: BuildDetection): string => asSentence(d.error.trim().split("\n")[0] ?? ""), []);
  /** What stops Railpack, as a headline, and what to do about it, by what it said. */
  const refusal = useCallback(
    (d: BuildDetection): { headline: string; fix: string } => {
      switch (d.problem) {
        case DetectionProblem.NO_PROVIDER:
          return {
            headline: t`Railpack can't tell how to build it`,
            fix: t`Add a Dockerfile, or set both a build command and a start command under Change.`,
          };
        case DetectionProblem.VERSION: {
          const variable = `RAILPACK_${d.problemTool.toUpperCase().replace(/[-.]/g, "_")}_VERSION`;
          return {
            headline: t`Railpack can't find a version the repository asks for`,
            fix: d.problemTool
              ? t`Change the version the repository asks for, or set ${variable} as a build argument under Change.`
              : t`Change the version the repository asks for.`,
          };
        }
        case DetectionProblem.NO_START:
          return {
            headline: t`Railpack found nothing that starts the app`,
            fix: t`Set a start command under Change, or add a start script to the repository.`,
          };
        case DetectionProblem.NETWORK:
          return {
            headline: t`Railpack couldn't reach where versions are listed`,
            fix: t`Check the server reaches github.com. A plan goes ahead, and the build reads it again.`,
          };
        default:
          return {
            headline: t`Railpack can't build it`,
            fix: t`Fix what it says in the repository, or add a Dockerfile.`,
          };
      }
    },
    [t],
  );
  return { what, headline, steps, packages, warnings, cant, refusal };
}
