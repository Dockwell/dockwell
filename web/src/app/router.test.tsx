// SPDX-License-Identifier: AGPL-3.0-only

import { act, screen, waitFor, within } from "@testing-library/react";
import userEvent from "@testing-library/user-event";
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
    expect(document.title).toBe("Control plane status · Dockwell");
  });

  it("names the tab for each page, in and out of the shell", async () => {
    const user = userEvent.setup();
    const { router } = renderAt("/apps");
    await screen.findByRole("heading", { level: 1, name: "Apps" });
    expect(document.title).toBe("Apps · Dockwell");

    await act(() => router.navigate({ to: "/status" }));
    await screen.findByRole("heading", { level: 1, name: "The control plane is running" });
    expect(document.title).toBe("Control plane status · Dockwell");

    await act(() => router.navigate({ to: "/nowhere" as "/" }));
    await screen.findByRole("heading", { level: 1, name: "No page here" });
    expect(document.title).toBe("No page here · Dockwell");

    await user.click(screen.getByRole("link", { name: "Go to the overview" }));
    await screen.findByRole("heading", { level: 1, name: "Overview" });
    expect(document.title).toBe("Overview · Dockwell");
  });

  it("goes home from the status page's logo without reloading the panel", async () => {
    const user = userEvent.setup();
    const { router } = renderAt("/status");
    await screen.findByRole("heading", { level: 1, name: "The control plane is running" });

    await user.click(screen.getByRole("link", { name: "Dockwell" }));

    // A plain link would leave the router where it was and load the document again.
    await waitFor(() => expect(router.state.location.pathname).toBe("/"));
    expect(await screen.findByRole("heading", { level: 1, name: "Overview" })).toBeVisible();
  });

  it("lists the servers", async () => {
    renderAt("/servers");
    expect(await screen.findByRole("heading", { level: 1, name: "Servers" })).toBeVisible();
    expect(screen.getByRole("region", { name: "Servers" })).toBeVisible();
  });

  it("says plainly that a section from the design isn't built yet", async () => {
    renderAt("/websites");
    expect(await screen.findByRole("heading", { level: 1, name: "Websites" })).toBeVisible();
    expect(screen.getByRole("heading", { level: 2, name: "Not in this release yet" })).toBeVisible();
    expect(within(screen.getByRole("main")).getByRole("link", { name: "Go to the overview" })).toHaveAttribute(
      "href",
      "/",
    );
  });

  it.each(["/servers/hz-9/nowhere", "/nowhere"])("explains a missing page at %s and links home", async (path) => {
    renderAt(path);
    expect(await screen.findByRole("heading", { level: 1, name: "No page here" })).toBeVisible();
    expect(screen.getByText(path).tagName).toBe("CODE");
    expect(screen.getByRole("link", { name: "Go to the overview" })).toHaveAttribute("href", "/");
  });
});
