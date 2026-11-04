// SPDX-License-Identifier: AGPL-3.0-only

import { render, screen } from "@testing-library/react";
import userEvent from "@testing-library/user-event";
import { describe, expect, it, vi } from "vitest";

import { expectAccessible } from "../../test/a11y";
import { type TabItem, Tabs } from "./Tabs";

const tabs: TabItem[] = [
  { id: "overview", label: "Overview", controls: "panel" },
  { id: "deployments", label: "Deployments", count: 128 },
  { id: "logs", label: "Logs", icon: "terminal" },
  { id: "alerts", label: "Alerts", count: 2, countTone: "warning" },
];

const tab = (name: string) => screen.getByRole("tab", { name: new RegExp(`^${name}`) });

describe("Tabs", () => {
  it("is a named tab list that selects the first tab by default", async () => {
    const { container } = render(
      <>
        <Tabs label="App sections" tabs={tabs} />
        <div role="tabpanel" id="panel" aria-label="Overview" />
      </>,
    );
    expect(screen.getByRole("tablist", { name: "App sections" })).toBeVisible();
    expect(tab("Overview")).toHaveAttribute("aria-selected", "true");
    expect(tab("Overview")).toHaveAttribute("aria-controls", "panel");
    expect(tab("Overview")).toHaveAttribute("tabindex", "0");
    expect(tab("Logs")).toHaveAttribute("tabindex", "-1");
    await expectAccessible(container);
  });

  it("shows counts, toned only when given a tone", () => {
    render(<Tabs label="App sections" tabs={tabs} />);
    expect(screen.getByText("128")).toHaveClass("dw-tab-count");
    expect(screen.getByText("128").className).not.toContain("dw-tone-");
    expect(screen.getByText("2")).toHaveClass("dw-tab-count", "dw-tone-warning");
  });

  it("moves with arrow keys, Home and End, selecting as it goes", async () => {
    const user = userEvent.setup();
    const onChange = vi.fn();
    render(<Tabs label="App sections" tabs={tabs} defaultValue="deployments" onChange={onChange} />);
    tab("Deployments").focus();
    await user.keyboard("{ArrowRight}");
    expect(tab("Logs")).toHaveFocus();
    expect(tab("Logs")).toHaveAttribute("aria-selected", "true");
    await user.keyboard("{End}");
    expect(tab("Alerts")).toHaveFocus();
    await user.keyboard("{ArrowRight}");
    expect(tab("Overview")).toHaveFocus();
    await user.keyboard("{ArrowLeft}");
    expect(tab("Alerts")).toHaveFocus();
    await user.keyboard("{Home}");
    expect(tab("Overview")).toHaveAttribute("aria-selected", "true");
    expect(onChange.mock.calls.map(([id]) => id)).toEqual(["logs", "alerts", "overview", "alerts", "overview"]);
  });

  it("ignores arrow keys with a modifier held", async () => {
    const user = userEvent.setup();
    const onChange = vi.fn();
    render(<Tabs label="App sections" tabs={tabs} onChange={onChange} />);
    tab("Overview").focus();
    await user.keyboard("{Alt>}{ArrowRight}{/Alt}");
    expect(onChange).not.toHaveBeenCalled();
    expect(tab("Overview")).toHaveFocus();
  });

  it("selects the first tab when the tabs arrive after mount, or the chosen one goes away", async () => {
    const user = userEvent.setup();
    const { rerender } = render(<Tabs label="App sections" tabs={[]} />);
    expect(screen.queryAllByRole("tab")).toHaveLength(0);
    rerender(<Tabs label="App sections" tabs={tabs} />);
    expect(tab("Overview")).toHaveAttribute("aria-selected", "true");
    await user.click(tab("Logs"));
    rerender(<Tabs label="App sections" tabs={tabs.filter((t) => t.id !== "logs")} />);
    expect(tab("Overview")).toHaveAttribute("aria-selected", "true");
    expect(tab("Overview")).toHaveAttribute("tabindex", "0");
  });

  it("selects the default tab once it arrives", () => {
    const { rerender } = render(<Tabs label="App sections" tabs={tabs.slice(0, 2)} defaultValue="logs" />);
    expect(tab("Overview")).toHaveAttribute("aria-selected", "true");
    rerender(<Tabs label="App sections" tabs={tabs} defaultValue="logs" />);
    expect(tab("Logs")).toHaveAttribute("aria-selected", "true");
  });

  it("follows the parent when controlled", async () => {
    const user = userEvent.setup();
    const onChange = vi.fn();
    const { rerender } = render(<Tabs label="App sections" tabs={tabs} value="logs" onChange={onChange} />);
    await user.click(tab("Alerts"));
    expect(onChange).toHaveBeenCalledWith("alerts");
    expect(tab("Logs")).toHaveAttribute("aria-selected", "true");
    rerender(<Tabs label="App sections" tabs={tabs} value="alerts" onChange={onChange} />);
    expect(tab("Alerts")).toHaveAttribute("aria-selected", "true");
    expect(tab("Alerts")).toHaveAttribute("tabindex", "0");
  });
});
