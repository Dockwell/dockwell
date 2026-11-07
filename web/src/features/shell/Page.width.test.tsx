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
    ["/apps", "Apps", "is-wide"],
    ["/catalog", "Catalog", "is-wide"],
    ["/activity", "Activity", "is-wide"],
    ["/websites", "Websites", "is-wide"],
    ["/settings", "Settings", "is-narrow"],
  ])("%s is %s", async (path, heading, kind) => {
    const page = await pageAt(path, heading);
    expect(page).toHaveClass("page", kind);
  });

  it("takes the standard width, with no class of its own, when it names none", async () => {
    const page = await pageAt("/more", "More");
    expect(page.className).not.toMatch(/\bis-(wide|narrow|full)\b/);
  });
});
