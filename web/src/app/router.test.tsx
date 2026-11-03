// SPDX-License-Identifier: AGPL-3.0-only

import { create } from "@bufbuild/protobuf";
import { GetVersionResponseSchema } from "@dockwell/api";
import { createMemoryHistory, createRouter, RouterProvider } from "@tanstack/react-router";
import { screen } from "@testing-library/react";
import { describe, expect, it } from "vitest";

import { routeTree } from "../routeTree.gen";
import { renderWithApi } from "../test/render";

function renderAt(path: string) {
  const router = createRouter({ routeTree, history: createMemoryHistory({ initialEntries: [path] }) });
  return renderWithApi(<RouterProvider router={router} />, {
    getVersion: () => create(GetVersionResponseSchema, {}),
  });
}

describe("routes", () => {
  it("serves the status page at the root", async () => {
    renderAt("/");
    expect(await screen.findByRole("heading", { level: 1, name: "The control plane is running" })).toBeVisible();
  });

  it("explains a missing page and links home", async () => {
    renderAt("/servers/hz-9");
    expect(await screen.findByRole("heading", { level: 1, name: "No page here" })).toBeVisible();
    expect(screen.getByText("/servers/hz-9").tagName).toBe("CODE");
    expect(screen.getByRole("link", { name: "Go to the overview" })).toHaveAttribute("href", "/");
  });
});
