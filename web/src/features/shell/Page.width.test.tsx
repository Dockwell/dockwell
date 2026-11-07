// SPDX-License-Identifier: AGPL-3.0-only

import { screen } from "@testing-library/react";
import { describe, expect, it } from "vitest";

import { renderAt } from "../../test/render";

/** The page's own box under the shell, which carries its kind. */
async function pageAt(path: string, heading: string): Promise<Element> {
  renderAt(path);
  const title = await screen.findByRole("heading", { level: 1, name: heading });
  const page = title.closest(".page");
  if (!page) throw new Error(`${path} has no page around its heading`);
  return page;
}

// How wide each grows is the stylesheet's (Page.css), and web/e2e/layout-flow.ts measures it in a browser.
describe("a page's width by its kind", () => {
  it.each([
    ["/catalog", "Catalog", "is-wide"],
    ["/servers", "Servers", "is-wide"],
    ["/settings/general", "General", "is-narrow"],
    ["/profile", "Profile and security", "is-narrow"],
  ])("%s is %s", async (path, heading, kind) => {
    const page = await pageAt(path, heading);
    expect(page).toHaveClass("page", kind);
  });

  // A table of few columns reads at a detail page's width, its head and table sharing their edges.
  it.each([
    ["/apps", "Apps"],
    ["/domains", "Domains"],
    ["/backups", "Backups"],
    ["/activity", "Activity"],
    ["/more", "More"],
  ])("%s takes the standard width, with no class of its own", async (path, heading) => {
    const page = await pageAt(path, heading);
    expect(page.className).not.toMatch(/\bis-(wide|narrow)\b/);
  });
});
