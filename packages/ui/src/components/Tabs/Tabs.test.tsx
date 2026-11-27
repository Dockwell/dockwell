// SPDX-License-Identifier: AGPL-3.0-only

import { fireEvent, render, screen, waitFor } from "@testing-library/react";
import userEvent from "@testing-library/user-event";
import { describe, expect, it, vi } from "vitest";

import { expectAccessible } from "../../test/a11y";
import { type TabItem, TabLink, TabNav, Tabs } from "./Tabs";

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

describe("TabNav", () => {
  it("is links in a named navigation, the current one marked by its link", async () => {
    const { container } = render(
      <TabNav label="App sections">
        <TabLink href="/a" aria-current="page">
          Overview
        </TabLink>
        <TabLink href="/b" count={128}>
          Deployments
        </TabLink>
      </TabNav>,
    );
    const nav = screen.getByRole("navigation", { name: "App sections" });
    expect(nav).toHaveClass("dw-tabs");
    expect(screen.getByRole("link", { name: "Overview" })).toHaveAttribute("aria-current", "page");
    expect(screen.getByRole("link", { name: "Deployments 128" })).toHaveAttribute("href", "/b");
    await expectAccessible(container);
  });
});

describe("a tab list that doesn't fit", () => {
  function sized(el: HTMLElement, sizes: { clientWidth: number; scrollWidth: number; scrollLeft?: number }) {
    for (const [key, value] of Object.entries(sizes)) {
      Object.defineProperty(el, key, { configurable: true, value });
    }
  }

  it("fades the edge with more past it, and only then", () => {
    const { rerender } = render(
      <TabNav label="App sections">
        <TabLink href="/a">Overview</TabLink>
      </TabNav>,
    );
    const nav = screen.getByRole("navigation", { name: "App sections" });
    expect(nav).not.toHaveAttribute("data-more");

    sized(nav, { clientWidth: 300, scrollWidth: 400, scrollLeft: 0 });
    fireEvent.scroll(nav);
    expect(nav).toHaveAttribute("data-more", "end");

    sized(nav, { clientWidth: 300, scrollWidth: 400, scrollLeft: 50 });
    fireEvent.scroll(nav);
    expect(nav).toHaveAttribute("data-more", "both");

    sized(nav, { clientWidth: 300, scrollWidth: 400, scrollLeft: 100 });
    fireEvent.scroll(nav);
    expect(nav).toHaveAttribute("data-more", "start");
    rerender(
      <TabNav label="App sections">
        <TabLink href="/a">Overview</TabLink>
      </TabNav>,
    );
  });
});

describe("the chevron at the end of a list that scrolls (#795)", () => {
  function sized(el: HTMLElement, sizes: { clientWidth: number; scrollWidth: number; scrollLeft?: number }) {
    for (const [key, value] of Object.entries(sizes)) {
      Object.defineProperty(el, key, { configurable: true, value });
    }
  }

  it("shows while more is past the end, and a tap scrolls on, out of the keyboard's and a screen reader's way", async () => {
    const { container } = render(
      <TabNav label="App sections">
        <TabLink href="/a">Overview</TabLink>
        <TabLink href="/b">Settings</TabLink>
      </TabNav>,
    );
    const nav = screen.getByRole("navigation", { name: "App sections" });
    expect(container.querySelector(".dw-tabs-more")).toBeNull();

    const scrollBy = vi.fn();
    nav.scrollBy = scrollBy;
    sized(nav, { clientWidth: 300, scrollWidth: 400, scrollLeft: 0 });
    fireEvent.scroll(nav);
    const more = container.querySelector<HTMLButtonElement>(".dw-tabs-more");
    expect(more).not.toBeNull();
    expect(more).toHaveAttribute("aria-hidden", "true");
    expect(more).toHaveAttribute("tabindex", "-1");
    expect(screen.getAllByRole("link")).toHaveLength(2);
    if (more) fireEvent.click(more);
    expect(scrollBy).toHaveBeenCalledWith({ left: 225, behavior: "smooth" });
    await expectAccessible(container);

    sized(nav, { clientWidth: 300, scrollWidth: 400, scrollLeft: 50 });
    fireEvent.scroll(nav);
    expect(container.querySelector(".dw-tabs-more")).not.toBeNull();

    sized(nav, { clientWidth: 300, scrollWidth: 400, scrollLeft: 100 });
    fireEvent.scroll(nav);
    expect(container.querySelector(".dw-tabs-more")).toBeNull();
  });

  it("drops the overflow chevron when tabs collapse to fit without the container resizing", async () => {
    const { container, rerender } = render(
      <TabNav label="App sections">
        <TabLink href="/a">Overview</TabLink>
        <TabLink href="/b">Settings</TabLink>
        <TabLink href="/c">Firewall</TabLink>
      </TabNav>,
    );
    const nav = screen.getByRole("navigation", { name: "App sections" });
    sized(nav, { clientWidth: 300, scrollWidth: 420, scrollLeft: 0 });
    fireEvent.scroll(nav);
    expect(container.querySelector(".dw-tabs-more")).not.toBeNull();

    sized(nav, { clientWidth: 300, scrollWidth: 280, scrollLeft: 0 });
    rerender(
      <TabNav label="App sections">
        <TabLink href="/a">Overview</TabLink>
        <TabLink href="/b">System</TabLink>
      </TabNav>,
    );
    await waitFor(() => expect(container.querySelector(".dw-tabs-more")).toBeNull());
  });

  it("is there for Tabs too, and leaves the tab list's tabs as they are", async () => {
    const { container } = render(
      <Tabs
        label="Views"
        tabs={[
          { id: "a", label: "One" },
          { id: "b", label: "Two" },
        ]}
      />,
    );
    const list = screen.getByRole("tablist", { name: "Views" });
    sized(list, { clientWidth: 100, scrollWidth: 400, scrollLeft: 0 });
    fireEvent.scroll(list);
    expect(container.querySelector(".dw-tabs-more")).not.toBeNull();
    expect(screen.getAllByRole("tab")).toHaveLength(2);
    await expectAccessible(container);
  });
});

describe("TabNav where its tabs don't fit", () => {
  /** A row 100 px wide, its tabs 60 px apart, and where it has scrolled to. */
  function laidOut(nav: HTMLElement) {
    let scrolled = 0;
    Object.defineProperty(nav, "scrollLeft", {
      configurable: true,
      get: () => scrolled,
      set: (v: number) => {
        scrolled = Math.max(0, v);
      },
    });
    nav.getBoundingClientRect = () => ({ left: 0, right: 100 }) as DOMRect;
    [...nav.querySelectorAll("a")].forEach((a, i) => {
      a.getBoundingClientRect = () => ({ left: i * 60 - scrolled, right: i * 60 + 50 - scrolled }) as DOMRect;
    });
    return () => scrolled;
  }

  it("brings a tab focused by keyboard clear of the fade, moving only the row", () => {
    const { container } = render(
      <TabNav label="App sections">
        <TabLink href="/a" aria-current="page">
          Overview
        </TabLink>
        <TabLink href="/b">Logs</TabLink>
        <TabLink href="/c">Settings</TabLink>
      </TabNav>,
    );
    const nav = container.querySelector("nav") as HTMLElement;
    const scrolled = laidOut(nav);
    // jsdom has none; one that's called would move the page.
    const page = vi.fn();
    Element.prototype.scrollIntoView = page;
    fireEvent.focusIn(screen.getByRole("link", { name: "Settings" }));
    // Its right edge at 170, past the row's 100, and 32 px for the fade.
    expect(scrolled()).toBe(102);
    fireEvent.focusIn(screen.getByRole("link", { name: "Overview" }));
    expect(scrolled()).toBe(0);
    expect(page).not.toHaveBeenCalled();
  });
});
