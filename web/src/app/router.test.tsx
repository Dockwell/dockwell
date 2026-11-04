// SPDX-License-Identifier: AGPL-3.0-only

import { screen, within } from "@testing-library/react";
import { describe, expect, it } from "vitest";

import { renderAt } from "../test/render";

describe("routes", () => {
  it("opens on the overview, inside the shell", async () => {
    renderAt("/");
    expect(await screen.findByRole("heading", { level: 1, name: "Overview" })).toBeVisible();
    expect(screen.getByRole("navigation", { name: "Main" })).toBeVisible();
    expect(screen.getByRole("main")).toContainElement(screen.getByRole("heading", { level: 1 }));
    expect(document.title).toBe("Overview · Dockwell");
  });

  it("serves the status page on its own, outside the shell", async () => {
    renderAt("/status");
    expect(await screen.findByRole("heading", { level: 1, name: "The control plane is running" })).toBeVisible();
    expect(screen.queryByRole("navigation", { name: "Main" })).toBeNull();
  });

  it("lists the servers", async () => {
    renderAt("/servers");
    expect(await screen.findByRole("heading", { level: 1, name: "Servers" })).toBeVisible();
    expect(screen.getByRole("region", { name: "Servers" })).toBeVisible();
  });

  it("says plainly that a section from the design isn't built yet", async () => {
    renderAt("/backups");
    expect(await screen.findByRole("heading", { level: 1, name: "Backups" })).toBeVisible();
    expect(screen.getByRole("heading", { level: 2, name: "Not in this release yet" })).toBeVisible();
    expect(within(screen.getByRole("main")).getByRole("link", { name: "Go to the overview" })).toHaveAttribute(
      "href",
      "/",
    );
  });

  it.each(["/servers/hz-9/files", "/nowhere"])("explains a missing page at %s and links home", async (path) => {
    renderAt(path);
    expect(await screen.findByRole("heading", { level: 1, name: "No page here" })).toBeVisible();
    expect(screen.getByText(path).tagName).toBe("CODE");
    expect(screen.getByRole("link", { name: "Go to the overview" })).toHaveAttribute("href", "/");
  });
});
