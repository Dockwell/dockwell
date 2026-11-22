// SPDX-License-Identifier: AGPL-3.0-only

import { readdirSync, readFileSync } from "node:fs";
import { join, relative, resolve } from "node:path";
import { describe, expect, it } from "vitest";

// A screen that waits shows the shape of what is coming, in place, never a lone
// sentence (interaction principle 7 in design/system/README.md). These read the
// panel's source and the design system's for the shapes of a bare loading line,
// so one can't come back. Vitest runs in the panel's package, web/.

const roots = [resolve("src"), resolve("../packages/ui/src")];

function sources(dir: string, ending: string): string[] {
  const out: string[] = [];
  for (const entry of readdirSync(dir, { withFileTypes: true })) {
    const path = join(dir, entry.name);
    if (entry.isDirectory()) out.push(...sources(path, ending));
    else if (entry.name.endsWith(ending) && !entry.name.includes(".test.")) out.push(path);
  }
  return out;
}

/** Words that open a sentence about a wait. */
const VERB =
  "(?:Reading|Loading|Fetching|Finding|Looking|Waiting|Asking|Measuring|Listing|Getting|Opening|Searching|Counting|Checking|Connecting|Adding up|Working out)";
/** One message in the panel's i18n: <Trans>…</Trans>, or t`…` set as an element's content. */
const MESSAGE = `(?:<Trans>\\s*${VERB}[^<]*</Trans>|\\{\\s*t\`${VERB}[^\`]*\`\\s*\\})`;
const ELEMENT = "(p|span|div|li|dd|td|h[1-6]|strong|small|em|output)";

const SHAPES: { name: string; pattern: RegExp }[] = [
  {
    // <p role="status"><Trans>Reading local images</Trans></p>, with or without a spinner before it.
    name: "an element whose only content is a sentence about the wait",
    pattern: new RegExp(`<${ELEMENT}\\b[^>]*>\\s*(?:<(?:Icon|Glyph)\\b[^>]*/>\\s*)?${MESSAGE}\\s*</\\1>`, "g"),
  },
  {
    // <p>{pending ? <Trans>Reading what happened</Trans> : …}</p>
    name: "an element that says the wait on one side of a choice",
    pattern: new RegExp(`<${ELEMENT}\\b[^>]*>\\s*\\{[^{}<]*\\?\\s*(?:<Trans>\\s*${VERB}|t\`${VERB})`, "g"),
  },
  {
    // <EmptyState title={<Trans>Reading logs</Trans>} />: a wait that looks like there is nothing.
    name: "an empty state that stands for a wait",
    pattern: new RegExp(`<EmptyState\\b[^>]*?title=\\{\\s*(?:<Trans>\\s*|t\`)${VERB}`, "g"),
  },
];

/**
 * Where a message about work in flight may stand on its own, each with why. None of
 * these is a screen or a section waiting for its data: each is the state of work the
 * person started, said where that work shows.
 */
const IN_FLIGHT: { file: string; says: string; why: string }[] = [
  {
    file: "web/src/features/apps/AppDeployments.tsx",
    says: "Waiting for the server to begin the build.",
    why: "a deploy's build under its Queued badge: the state of a build, not a read",
  },
  {
    file: "web/src/features/apps/BuildLog.tsx",
    says: "Waiting its turn: a server runs one build at a time.",
    why: "a live build log before its first line, and why it waits: the stream's own state",
  },
];

describe("every wait a shape", () => {
  it("leaves no bare loading line in the panel or the design system", () => {
    const bare: string[] = [];
    for (const root of roots) {
      for (const path of sources(root, ".tsx")) {
        const name = relative(resolve(".."), path);
        const text = readFileSync(path, "utf8");
        for (const { name: shape, pattern } of SHAPES) {
          for (const match of text.matchAll(pattern)) {
            const line = text.slice(0, match.index).split("\n").length;
            if (IN_FLIGHT.some((allowed) => allowed.file === name && match[0].includes(allowed.says))) continue;
            bare.push(`${name}:${line} (${shape}) ${match[0].replace(/\s+/g, " ").slice(0, 100)}`);
          }
        }
      }
    }
    expect(bare, "draw the shape of what is coming with Loading, Skeleton or Table's loading").toEqual([]);
  });

  it("allows only what is still there", () => {
    const stale = IN_FLIGHT.filter(({ file, says }) => !readFileSync(resolve("..", file), "utf8").includes(says));
    expect(stale, "remove an allowance whose words are gone").toEqual([]);
  });

  it("leaves the shapes to the design system", () => {
    // A screen that drew its own bars (.apps-skeleton-bar, a placeholder tile and the like)
    // drifted from the others in colour, size and motion. Its layout may place the shared
    // shapes; only Skeleton.css paints them.
    const painted: string[] = [];
    for (const path of sources(resolve("src"), ".css")) {
      const text = readFileSync(path, "utf8");
      for (const match of text.matchAll(/([^{}]*)\{([^}]*)\}/g)) {
        const selector = (match[1] ?? "").trim();
        if (!/skeleton|is-placeholder|loading|ghost/i.test(selector.replace(/::placeholder/g, ""))) continue;
        if (/(^|[\s;])background(-color)?\s*:/.test(match[2] ?? "")) {
          painted.push(`${relative(resolve("src"), path)}: ${selector.split("\n").pop()}`);
        }
      }
    }
    expect(painted, "use Skeleton from @dockwell/ui, which paints every shape the same way").toEqual([]);
  });
});
