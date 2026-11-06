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
      "−web container Delete",
    ]);
  });

  it("sums up the plan, counting replacements apart from changes", async () => {
    const { container, rerender } = render(
      <PlanDiff resources={resources} impact="One replica at a time · no downtime" />,
    );
    const counts = () => Array.from(container.querySelectorAll(".dw-plan-count")).map((c) => c.textContent);
    expect(counts()).toEqual(["+2 to create", "~1 to update", "±1 to replace", "−1 to delete"]);
    expect(screen.getByText("One replica at a time · no downtime").closest(".dw-plan-impact")).not.toBeNull();
    await expectAccessible(container);
    // A plan that only replaces a database says so; it never reads as a change with nothing destroyed.
    rerender(<PlanDiff resources={[{ id: "db", action: "replace", kind: "database", name: "postgres" }]} />);
    // Only what the plan does: a zero says nothing, and takes a phone's line to say it.
    expect(counts()).toEqual(["±1 to replace"]);
    rerender(<PlanDiff resources={resources.filter((resource) => resource.action !== "replace")} />);
    expect(counts()).toEqual(["+2 to create", "~1 to update", "−1 to delete"]);
    expect(container.querySelector(".is-zero")).toBeNull();
  });

  it("shows each resource with its kind and action, struck through when destroyed", () => {
    render(<PlanDiff resources={resources} />);
    expect(screen.getByRole("button", { name: /storefront-web app Rolling update/ })).toBeVisible();
    expect(screen.getByRole("button", { name: /DATABASE_URL secret Replace/ })).toBeVisible();
    expect(screen.getByRole("button", { name: /uploads-old volume, 4 GB Delete/ })).toBeVisible();
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

  it("gives the impact a row of its own, under the counts", () => {
    const { container } = render(<PlanDiff resources={resources} impact="hz-1 · no downtime expected" />);
    const impact = screen.getByText("hz-1 · no downtime expected").closest(".dw-plan-impact");
    expect(impact?.parentElement).toHaveClass("dw-plan-summary");
    expect(container.querySelector(".dw-plan-counts")?.contains(impact as Node)).toBe(false);
  });

  it("shows a field in words, and a machine value in mono among them", () => {
    render(
      <PlanDiff
        resources={[
          {
            id: "web",
            action: "create",
            kind: "service",
            name: "web",
            changes: [
              { path: "ports[0]", label: "port", to: "8080 → 3000, TCP" },
              {
                path: "mounts[0]",
                label: "volume",
                to: (
                  <>
                    <code>web-data</code> at <code>/data</code>
                  </>
                ),
              },
              { path: "env[API]", label: "variable", to: "API", hidden: true },
            ],
          },
        ]}
      />,
    );
    expect(screen.getByText("port")).toHaveClass("dw-plan-label");
    expect(screen.getByText("8080 → 3000, TCP")).toHaveClass("dw-diff-to");
    const volume = screen.getByText("web-data").closest(".dw-diff-to");
    expect(volume).toHaveClass("is-words");
    expect(volume).toHaveTextContent("web-data at /data");
    expect(screen.getByText("variable").nextElementSibling).toHaveTextContent("API");
    expect(screen.getByText("variable").nextElementSibling?.querySelector(".dw-diff-secret")).toHaveTextContent(
      "hidden",
    );
  });

  it("marks what puts a server at risk as critical, never as a green addition", async () => {
    const { container } = render(
      <PlanDiff
        resources={[
          {
            id: "web",
            action: "create",
            kind: "service",
            name: "web",
            changes: [{ path: "mounts[0]", label: "Docker's socket", to: "/var/run/docker.sock", tone: "critical" }],
          },
        ]}
      />,
    );
    expect(screen.getByText("Docker's socket")).toHaveClass("is-critical");
    expect(screen.getByText("/var/run/docker.sock")).toHaveClass("dw-diff-critical");
    expect(screen.getByText("/var/run/docker.sock")).not.toHaveClass("dw-diff-to");
    await expectAccessible(container);
  });

  it("says what it did in the past tense once it is done, with no zeros", () => {
    const { container } = render(<PlanDiff resources={resources} done />);
    expect(Array.from(container.querySelectorAll(".dw-plan-count")).map((c) => c.textContent)).toEqual([
      "+2 created",
      "~1 updated",
      "±1 replaced",
      "−1 deleted",
    ]);
    expect(screen.getByRole("button", { name: /uploads-old volume, 4 GB Deleted/ })).toBeVisible();
    const { container: one } = render(
      <PlanDiff resources={[{ id: "a", action: "create", kind: "job", name: "a" }]} done />,
    );
    expect(Array.from(one.querySelectorAll(".dw-plan-count")).map((c) => c.textContent)).toEqual(["+1 created"]);
  });

  it("folds what comes along with what matters on one row, which counts like the rest", async () => {
    const user = userEvent.setup();
    const { container } = render(
      <PlanDiff
        resources={[{ id: "web", action: "create", kind: "app", name: "web" }]}
        also={{
          label: "Also creates a network and a volume",
          action: "create",
          resources: [
            { id: "net", action: "create", kind: "network", name: "default" },
            {
              id: "vol",
              action: "create",
              kind: "volume",
              name: "data",
              changes: [{ path: "driver", label: "driver", to: "local" }],
            },
          ],
        }}
      />,
    );
    expect(Array.from(container.querySelectorAll(".dw-plan-count")).map((c) => c.textContent)).toEqual([
      "+3 to create",
    ]);
    const fold = screen.getByRole("button", { name: /Also creates a network and a volume/ });
    expect(fold).toHaveAttribute("aria-expanded", "false");
    expect(screen.queryByText("default")).not.toBeVisible();
    await expectAccessible(container);
    await user.click(fold);
    expect(fold).toHaveAttribute("aria-expanded", "true");
    expect(screen.getByText("default")).toBeVisible();
    // What it holds opens and closes as any row does, with the fold's own state apart.
    await user.click(screen.getByRole("button", { name: /data volume/ }));
    expect(fold).toHaveAttribute("aria-expanded", "true");
    await expectAccessible(container);
  });

  it("says what reaches the server and stops short of the worst in amber, and no green", () => {
    render(
      <PlanDiff
        resources={[
          {
            id: "site",
            action: "create",
            kind: "app",
            name: "site",
            changes: [{ path: "m", label: "server path", to: "/srv/site", tone: "warning" }],
          },
        ]}
      />,
    );
    expect(screen.getByText("server path")).toHaveClass("is-warning");
    expect(screen.getByText("/srv/site")).toHaveClass("dw-diff-warning");
    expect(screen.getByText("/srv/site")).not.toHaveClass("dw-diff-to");
  });

  it("can leave out the counts, for a plan that came to nothing", () => {
    const { container } = render(<PlanDiff resources={resources} noSummary />);
    expect(container.querySelector(".dw-plan-summary")).toBeNull();
  });

  it("counts a restart as one, apart from a replace", () => {
    const { container } = render(
      <PlanDiff
        resources={[
          { id: "a", action: "replace", kind: "service", name: "web", restart: true },
          { id: "b", action: "replace", kind: "service", name: "db" },
        ]}
      />,
    );
    const counts = container.querySelector(".dw-plan-counts");
    expect(counts).toHaveTextContent("1 to restart");
    expect(counts).toHaveTextContent("1 to replace");
    // Its row says so too, not that it replaces it.
    const acts = [...container.querySelectorAll(".dw-plan-act")].map((a) => a.textContent);
    expect(acts).toEqual(["Restart", "Replace"]);
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
            summary: {
              create: "待创建",
              change: "待更改",
              replace: "待替换",
              restart: "待重启",
              destroy: "待删除",
              ifNewer: "如有更新则更新",
            },
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

  it("counts what changes only if a pull finds a newer image apart, until it has happened", () => {
    const resources = [
      { id: "a", action: "update", kind: "app", name: "web", ifNewer: true },
      { id: "b", action: "update", kind: "app", name: "api" },
    ] as const;
    const { container, rerender } = render(<PlanDiff resources={resources} />);
    const counts = () => container.querySelector(".dw-plan-counts")?.textContent;
    expect(counts()).toContain("1 to update");
    expect(counts()).toContain("1 to update if newer");
    // Once it has happened it did or it didn't, and its counts say what it did.
    rerender(<PlanDiff resources={resources} done />);
    expect(counts()).toContain("2 updated");
    expect(counts()).not.toContain("if newer");
  });
});
