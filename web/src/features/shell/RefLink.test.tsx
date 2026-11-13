// SPDX-License-Identifier: AGPL-3.0-only

import { readdirSync, readFileSync } from "node:fs";
import { join, relative, resolve } from "node:path";
import { createMemoryHistory, createRootRoute, createRouter, RouterProvider } from "@tanstack/react-router";
import { render, screen } from "@testing-library/react";
import userEvent from "@testing-library/user-event";
import { describe, expect, it } from "vitest";

import { REF_KINDS, RefLink, refKindOf } from "./RefLink";

/** Where each kind's page is, for an ID of "x". */
const PAGES: Record<(typeof REF_KINDS)[number], string> = {
  app: "/apps/x",
  database: "/databases/x",
  server: "/servers/x",
  change: "/changes/x",
  project: "/projects/x",
  website: "/websites/x",
  entry: "/catalog/x",
  alert: "/alerts/x",
};

function renderLinks() {
  const root = createRootRoute({
    component: () => (
      <ul>
        {REF_KINDS.map((kind) => (
          <li key={kind}>
            <RefLink kind={kind} id="x">{`a ${kind}`}</RefLink>
            <RefLink kind={kind} id="x" look="plain">{`the ${kind}`}</RefLink>
          </li>
        ))}
      </ul>
    ),
  });
  const router = createRouter({ routeTree: root, history: createMemoryHistory({ initialEntries: ["/"] }) });
  return render(<RouterProvider router={router} />);
}

describe("a reference to a thing", () => {
  it("opens its page, for every kind, as a chip or in the words around it", async () => {
    renderLinks();
    for (const kind of REF_KINDS) {
      expect(await screen.findByRole("link", { name: `a ${kind}` })).toHaveAttribute("href", PAGES[kind]);
      expect(screen.getByRole("link", { name: `the ${kind}` })).toHaveAttribute("href", PAGES[kind]);
    }
  });

  it("takes the keyboard as any link does", async () => {
    const user = userEvent.setup();
    renderLinks();
    await screen.findByRole("link", { name: "a app" });
    await user.tab();
    expect(screen.getByRole("link", { name: "a app" })).toHaveFocus();
    await user.tab();
    expect(screen.getByRole("link", { name: "the app" })).toHaveFocus();
  });

  it("knows a kind by the prefix its ID was made with, and nothing else", () => {
    expect(refKindOf("srv_01")).toBe("server");
    expect(refKindOf("svc_01")).toBe("app");
    expect(refKindOf("chg_01")).toBe("change");
    expect(refKindOf("prj_01")).toBe("project");
    expect(refKindOf("alt_01")).toBe("alert");
    expect(refKindOf("bkp_01")).toBeUndefined();
    expect(refKindOf("hz-1")).toBeUndefined();
  });
});

// Vitest runs in the panel's package, web/.
const features = resolve("src/features");

function sources(dir: string): string[] {
  const out: string[] = [];
  for (const entry of readdirSync(dir, { withFileTypes: true })) {
    const path = join(dir, entry.name);
    if (entry.isDirectory()) out.push(...sources(path));
    else if (entry.name.endsWith(".tsx") && !entry.name.includes(".test.")) out.push(path);
  }
  return out;
}

/**
 * Where a known reference may stay words, each with why. A plan under review is a
 * decision in progress, which a link would leave half read.
 */
const STAYS_WORDS: Record<string, string> = {
  "changes/PlanSteps.tsx": "a plan under review",
  "changes/ChangeSheet.tsx": "a plan under review",
};

/** A known reference's name set on its own as a chip's, a code's or a cell's text. */
const BARE = /<(code|Tag|strong|span|dd)\b[^>]*>\s*\{[\w?.!]*\.(serverName|subjectName|serviceName)\}\s*<\//g;

describe("every name a link", () => {
  it("leaves no server, app or subject's name as bare words outside a plan under review", () => {
    const bare: string[] = [];
    for (const path of sources(features)) {
      const name = relative(features, path);
      if (name in STAYS_WORDS) continue;
      const text = readFileSync(path, "utf8");
      for (const match of text.matchAll(BARE)) {
        const line = text.slice(0, match.index).split("\n").length;
        bare.push(`${name}:${line} ${match[0]}`);
      }
    }
    expect(bare, "make each a RefLink, which opens its page").toEqual([]);
  });
});
