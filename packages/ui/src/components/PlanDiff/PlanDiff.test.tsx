// SPDX-License-Identifier: AGPL-3.0-only

import { render, screen } from "@testing-library/react";
import userEvent from "@testing-library/user-event";
import { describe, expect, it } from "vitest";

import { englishStrings, UIStringsProvider } from "../../lib/strings";
import { expectAccessible } from "../../test/a11y";
import { PlanDiff, type PlanResource } from "./PlanDiff";

const resources: PlanResource[] = [
  {
    id: "web",
    action: "update",
    kind: "app",
    name: "storefront-web",
    note: "Rolling update",
    changes: [{ path: "image", from: "ghcr.io/acme/web:r63", to: "ghcr.io/acme/web:r64" }],
  },
  {
    id: "db-url",
    action: "replace",
    kind: "secret",
    name: "DATABASE_URL",
    changes: [{ path: "value", from: { secret: true }, to: { secret: true } }],
  },
  {
    id: "domain",
    action: "create",
    kind: "domain",
    name: "shop.acme.dev",
    changes: [{ path: "target", to: "storefront-web" }],
  },
  {
    id: "volume",
    action: "destroy",
    kind: "volume",
    kindLabel: "volume, 4 GB",
    name: "uploads-old",
    changes: [{ path: "size", from: "4 GB" }],
  },
  { id: "job", action: "create", kind: "job", name: "nightly-reports" },
];

describe("PlanDiff", () => {
  it("names projects, environments and containers by their kind", () => {
    render(
      <PlanDiff
        resources={[
          { id: "p", action: "create", kind: "project", name: "shop" },
          { id: "e", action: "create", kind: "environment", name: "production" },
          { id: "c", action: "destroy", kind: "container", name: "web" },
        ]}
      />,
    );
    expect(screen.getAllByRole("listitem").map((li) => li.textContent)).toEqual([
      "+shop project Create",
      "+production environment Create",
      "−web container Destroy",
    ]);
  });

  it("sums up the plan, counting replacements apart from changes", async () => {
    const { container, rerender } = render(
      <PlanDiff resources={resources} impact="One replica at a time · no downtime" />,
    );
    const counts = () => Array.from(container.querySelectorAll(".dw-plan-count")).map((c) => c.textContent);
    expect(counts()).toEqual(["+2 to create", "~1 to change", "±1 to replace", "−1 to destroy"]);
    expect(screen.getByText("One replica at a time · no downtime")).toHaveClass("dw-plan-impact");
    await expectAccessible(container);
    // A plan that only replaces a database says so; it never reads as a change with nothing destroyed.
    rerender(<PlanDiff resources={[{ id: "db", action: "replace", kind: "database", name: "postgres" }]} />);
    expect(counts()).toEqual(["+0 to create", "~0 to change", "±1 to replace", "−0 to destroy"]);
    rerender(<PlanDiff resources={resources.filter((resource) => resource.action !== "replace")} />);
    expect(counts()).toEqual(["+2 to create", "~1 to change", "−1 to destroy"]);
  });

  it("shows each resource with its kind and action, struck through when destroyed", () => {
    render(<PlanDiff resources={resources} />);
    expect(screen.getByRole("button", { name: /storefront-web app Rolling update/ })).toBeVisible();
    expect(screen.getByRole("button", { name: /DATABASE_URL secret Replace/ })).toBeVisible();
    expect(screen.getByRole("button", { name: /uploads-old volume, 4 GB Destroy/ })).toBeVisible();
    expect(screen.getByText("uploads-old").closest(".dw-plan-res")).toHaveClass("is-remove");
    // A resource with nothing to expand isn't a button.
    expect(screen.queryByRole("button", { name: /nightly-reports/ })).toBeNull();
    expect(screen.getByText("nightly-reports").closest(".dw-plan-row")?.tagName).toBe("DIV");
  });

  it("starts destroyed resources collapsed and the rest open, and toggles them", async () => {
    const user = userEvent.setup();
    render(<PlanDiff resources={resources} />);
    const web = screen.getByRole("button", { name: /storefront-web/ });
    const volume = screen.getByRole("button", { name: /uploads-old/ });
    expect(web).toHaveAttribute("aria-expanded", "true");
    expect(volume).toHaveAttribute("aria-expanded", "false");
    const volumeChanges = document.getElementById(volume.getAttribute("aria-controls") ?? "");
    expect(volumeChanges).not.toBeVisible();
    await user.click(volume);
    expect(volume).toHaveAttribute("aria-expanded", "true");
    expect(volumeChanges).toBeVisible();
    await user.click(web);
    expect(web).toHaveAttribute("aria-expanded", "false");
  });

  it("reads old and new values in words, strikes the old one, and never shows a secret", () => {
    const { container } = render(<PlanDiff resources={resources} />);
    const image = screen.getByText("image").nextElementSibling;
    expect(image?.textContent).toBe("from ghcr.io/acme/web:r63to ghcr.io/acme/web:r64");
    expect(screen.getByText("ghcr.io/acme/web:r63")).toHaveClass("dw-diff-from");
    expect(screen.getByText("ghcr.io/acme/web:r64")).toHaveClass("dw-diff-to");
    expect(container.querySelectorAll(".dw-diff-secret")).toHaveLength(2);
    expect(container.textContent).not.toContain("[object Object]");
    expect(screen.getAllByText("hidden")).toHaveLength(2);
  });

  it("respects expanded and speaks the app's language", () => {
    render(
      <UIStringsProvider
        strings={{
          ...englishStrings,
          planDiff: {
            ...englishStrings.planDiff,
            action: { ...englishStrings.planDiff.action, update: "原地更新" },
            kind: { ...englishStrings.planDiff.kind, app: "应用" },
            summary: { create: "待创建", change: "待更改", replace: "待替换", destroy: "待销毁" },
          },
        }}
      >
        <PlanDiff
          resources={[
            {
              id: "a",
              action: "update",
              kind: "app",
              name: "api",
              expanded: false,
              changes: [{ path: "replicas", from: 2, to: 3 }],
            },
          ]}
        />
      </UIStringsProvider>,
    );
    expect(screen.getByRole("button", { name: /api 应用 原地更新/ })).toHaveAttribute("aria-expanded", "false");
    expect(screen.getByText("待更改")).toBeTruthy();
  });
});
