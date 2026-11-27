// SPDX-License-Identifier: AGPL-3.0-only

import { act, fireEvent, render, screen, within } from "@testing-library/react";
import userEvent from "@testing-library/user-event";
import { afterEach, describe, expect, it, vi } from "vitest";

import { expectAccessible } from "../../test/a11y";
import { ActionMenu } from "./ActionMenu";

describe("a row's actions in one menu (#795)", () => {
  function renderMenu() {
    const change = vi.fn();
    const remove = vi.fn();
    const rendered = render(
      <ActionMenu
        label="Actions for LOG_LEVEL"
        actions={[
          { id: "change", icon: "edit", label: "Change", onSelect: change },
          { id: "share", icon: "layers", label: "Make it shared", off: "REGION is shared already", onSelect: vi.fn() },
          { id: "open", icon: "external", label: "Open app.example.com", href: "https://app.example.com" },
          { id: "remove", icon: "trash", label: "Remove", danger: true, onSelect: remove },
        ]}
      />,
    );
    return { ...rendered, change, remove };
  }

  it("opens on its button, starts at the first thing it can do, and moves with the arrows", async () => {
    const user = userEvent.setup();
    const { container } = renderMenu();
    const button = screen.getByRole("button", { name: "Actions for LOG_LEVEL" });
    expect(button).toHaveAttribute("aria-expanded", "false");
    await user.click(button);
    const menu = screen.getByRole("menu", { name: "Actions for LOG_LEVEL" });
    expect(button).toHaveAttribute("aria-expanded", "true");
    expect(within(menu).getByRole("menuitem", { name: "Change" })).toHaveFocus();
    await user.keyboard("{ArrowDown}");
    expect(within(menu).getByRole("menuitem", { name: /Make it shared/ })).toHaveFocus();
    await user.keyboard("{End}");
    expect(within(menu).getByRole("menuitem", { name: "Remove" })).toHaveFocus();
    await user.keyboard("{ArrowDown}");
    expect(within(menu).getByRole("menuitem", { name: "Change" })).toHaveFocus();
    await expectAccessible(container);
    await user.keyboard("{Escape}");
    expect(screen.queryByRole("menu")).toBeNull();
    expect(button).toHaveFocus();
  });

  it("says why one can't be done under its name, as a phone has no hover, and does nothing on it", async () => {
    const user = userEvent.setup();
    renderMenu();
    await user.click(screen.getByRole("button", { name: "Actions for LOG_LEVEL" }));
    const off = screen.getByRole("menuitem", { name: /Make it shared/ });
    expect(off).toHaveAttribute("aria-disabled", "true");
    expect(off).toHaveTextContent("Make it sharedREGION is shared already");
    await user.click(off);
    expect(screen.getByRole("menu")).toBeVisible();
  });

  it("does what's chosen and closes, and opens a link out in a new tab", async () => {
    const user = userEvent.setup();
    const { remove } = renderMenu();
    await user.click(screen.getByRole("button", { name: "Actions for LOG_LEVEL" }));
    const out = screen.getByRole("menuitem", { name: "Open app.example.com" });
    expect(out).toHaveAttribute("href", "https://app.example.com");
    expect(out).toHaveAttribute("target", "_blank");
    await user.click(screen.getByRole("menuitem", { name: "Remove" }));
    expect(remove).toHaveBeenCalledOnce();
    expect(screen.queryByRole("menu")).toBeNull();
  });

  it("closes on a tap elsewhere", async () => {
    const user = userEvent.setup();
    renderMenu();
    await user.click(screen.getByRole("button", { name: "Actions for LOG_LEVEL" }));
    await user.click(document.body);
    expect(screen.queryByRole("menu")).toBeNull();
  });

  it("keeps a menu opened at the left edge of a narrow screen inside the viewport", async () => {
    vi.stubGlobal("innerWidth", 390);
    vi.spyOn(HTMLElement.prototype, "getBoundingClientRect").mockImplementation(function (this: HTMLElement) {
      return this.getAttribute("role") === "menu" ? new DOMRect(0, 0, 220, 120) : new DOMRect(20, 200, 44, 44);
    });
    const user = userEvent.setup();
    renderMenu();
    await user.click(screen.getByRole("button", { name: "Actions for LOG_LEVEL" }));
    expect(screen.getByRole("menu")).toHaveStyle({ right: "154px" });
  });

  it("is nothing when there's nothing to do", () => {
    const { container } = render(<ActionMenu label="Actions for nothing" actions={[]} />);
    expect(container).toBeEmptyDOMElement();
  });

  afterEach(() => {
    vi.restoreAllMocks();
    vi.unstubAllGlobals();
  });

  it("returns focus to its trigger button before onSelect runs so a sheet returns there on close (#956)", async () => {
    const user = userEvent.setup();
    let activeDuringSelect: Element | null = null;
    render(
      <ActionMenu
        label="Actions for api"
        actions={[
          {
            id: "stop",
            icon: "stop",
            label: "Stop",
            onSelect: () => {
              activeDuringSelect = document.activeElement;
            },
          },
        ]}
      />,
    );
    const trigger = screen.getByRole("button", { name: "Actions for api" });
    await user.click(trigger);
    await user.click(screen.getByRole("menuitem", { name: "Stop" }));
    expect(activeDuringSelect).toBe(trigger);
  });

  it("closes when its trigger scrolls out of view or under the desktop top bar, and never draws over the top bar (#956)", async () => {
    const user = userEvent.setup();
    vi.stubGlobal("innerWidth", 1280);
    vi.stubGlobal("innerHeight", 800);
    let buttonTop = 80;
    vi.spyOn(HTMLElement.prototype, "getBoundingClientRect").mockImplementation(function (this: HTMLElement) {
      if (this.classList.contains("topbar")) return new DOMRect(0, 0, 1280, 52);
      if (this.getAttribute("role") === "menu") return new DOMRect(100, buttonTop + 32, 200, 120);
      if (this.tagName === "BUTTON") return new DOMRect(300, buttonTop, 28, 28);
      return new DOMRect(0, 0, 1280, 800);
    });
    render(
      <div>
        <header className="topbar" />
        <ActionMenu label="Actions for api" actions={[{ id: "stop", label: "Stop", onSelect: vi.fn() }]} />
      </div>,
    );
    await user.click(screen.getByRole("button", { name: "Actions for api" }));
    const menu = screen.getByRole("menu", { name: "Actions for api" });
    expect(Number.parseFloat(menu.style.top)).toBeGreaterThanOrEqual(56);

    // Row slides under the sticky top bar (button top < 52): menu closes instead of floating over the bar.
    buttonTop = 30;
    fireEvent.scroll(window);
    expect(screen.queryByRole("menu")).toBeNull();

    // Re-open in view, then scroll fully off the bottom of the viewport: menu closes.
    buttonTop = 200;
    await user.click(screen.getByRole("button", { name: "Actions for api" }));
    expect(screen.getByRole("menu", { name: "Actions for api" })).toBeVisible();
    buttonTop = 900;
    fireEvent.scroll(window);
    expect(screen.queryByRole("menu")).toBeNull();
  });

  it("closes under the phone header or tab bar at 390 px and never draws over them (#956)", async () => {
    const user = userEvent.setup();
    vi.stubGlobal("innerWidth", 390);
    vi.stubGlobal("innerHeight", 844);
    let buttonTop = 700;
    vi.spyOn(HTMLElement.prototype, "getBoundingClientRect").mockImplementation(function (this: HTMLElement) {
      if (this.classList.contains("mobile-header")) return new DOMRect(0, 0, 390, 56);
      if (this.classList.contains("tabbar")) return new DOMRect(0, 782, 390, 62);
      if (this.getAttribute("role") === "menu") return new DOMRect(100, buttonTop - 124, 200, 120);
      if (this.tagName === "BUTTON") return new DOMRect(320, buttonTop, 44, 44);
      return new DOMRect(0, 0, 390, 844);
    });
    render(
      <div>
        <header className="mobile-header" />
        <ActionMenu label="Actions for api" actions={[{ id: "stop", label: "Stop", onSelect: vi.fn() }]} />
        <nav className="tabbar" />
      </div>,
    );
    await user.click(screen.getByRole("button", { name: "Actions for api" }));
    const menu = screen.getByRole("menu", { name: "Actions for api" });
    // Near the bottom tab bar, the menu opens above the button rather than drawing over .tabbar.
    expect(menu.style.bottom).toBe("148px");

    // Scrolling the button's center into .tabbar closes the menu without dropping focus to body.
    buttonTop = 765;
    fireEvent.scroll(window);
    expect(screen.queryByRole("menu")).toBeNull();
    expect(document.activeElement).not.toBe(document.body);

    // Scrolling the button's center under .mobile-header also closes the menu.
    buttonTop = 120;
    await user.click(screen.getByRole("button", { name: "Actions for api" }));
    expect(screen.getByRole("menu", { name: "Actions for api" })).toBeVisible();
    buttonTop = 20;
    fireEvent.scroll(window);
    expect(screen.queryByRole("menu")).toBeNull();
  });

  it("stays open when opened over an anchor partly under a sticky bar, and closes once the anchor moves behind it (#956)", async () => {
    const user = userEvent.setup();
    let onResize: (() => void) | undefined;
    class FakeResizeObserver {
      constructor(cb: ResizeObserverCallback) {
        onResize = () => cb([], this as unknown as ResizeObserver);
      }
      observe() {}
      unobserve() {}
      disconnect() {}
    }
    vi.stubGlobal("ResizeObserver", FakeResizeObserver);
    vi.stubGlobal("innerWidth", 1280);
    vi.stubGlobal("innerHeight", 800);
    // Anchor top is 4 px under the 52 px topbar (center is 62 px, still visible).
    let buttonTop = 48;
    vi.spyOn(HTMLElement.prototype, "getBoundingClientRect").mockImplementation(function (this: HTMLElement) {
      if (this.classList.contains("topbar")) return new DOMRect(0, 0, 1280, 52);
      if (this.getAttribute("role") === "menu") return new DOMRect(100, buttonTop + 32, 200, 100);
      if (this.tagName === "BUTTON") return new DOMRect(300, buttonTop, 28, 28);
      return new DOMRect(0, 0, 1280, 800);
    });
    render(
      <section tabIndex={-1} aria-label="Apps list">
        <header className="topbar" />
        <ActionMenu label="Actions for api" actions={[{ id: "stop", label: "Stop", onSelect: vi.fn() }]} />
      </section>,
    );
    await user.click(screen.getByRole("button", { name: "Actions for api" }));
    expect(screen.getByRole("menu", { name: "Actions for api" })).toBeVisible();

    // Initial ResizeObserver callback with no anchor movement must not close the menu.
    act(() => {
      onResize?.();
    });
    expect(screen.getByRole("menu", { name: "Actions for api" })).toBeVisible();

    // Once scrolled so the anchor's center passes behind the top bar, it closes and returns focus to the list.
    buttonTop = 30;
    fireEvent.scroll(window);
    expect(screen.queryByRole("menu")).toBeNull();
    expect(screen.getByLabelText("Apps list")).toHaveFocus();
  });

  it("detects sticky filter bars, stuck table heads, phone batch bars, and update-picks bars (#956)", async () => {
    const user = userEvent.setup();
    vi.stubGlobal("innerWidth", 390);
    vi.stubGlobal("innerHeight", 844);
    let buttonTop = 300;
    vi.spyOn(HTMLElement.prototype, "getBoundingClientRect").mockImplementation(function (this: HTMLElement) {
      if (this.classList.contains("topbar")) return new DOMRect(0, 0, 390, 52);
      if (this.classList.contains("sticky-filters")) return new DOMRect(0, 52, 390, 48);
      if (this.tagName === "THEAD") return new DOMRect(0, 100, 390, 36);
      if (this.classList.contains("sticky-bottom-bar")) return new DOMRect(0, 710, 390, 48);
      if (this.classList.contains("tabbar")) return new DOMRect(0, 782, 390, 62);
      if (this.getAttribute("role") === "menu") return new DOMRect(100, buttonTop + 32, 200, 100);
      if (this.tagName === "BUTTON") return new DOMRect(300, buttonTop, 28, 28);
      return new DOMRect(0, 0, 390, 844);
    });
    render(
      <section tabIndex={-1} aria-label="Apps list">
        <header className="topbar" style={{ position: "sticky", top: "0px" }} />
        <div className="sticky-filters" style={{ position: "sticky", top: "52px" }} />
        <table>
          <thead>
            <tr>
              <th>App</th>
            </tr>
          </thead>
          <tbody>
            <tr>
              <td>
                <ActionMenu label="Actions for api" actions={[{ id: "stop", label: "Stop", onSelect: vi.fn() }]} />
              </td>
            </tr>
          </tbody>
        </table>
        <div className="sticky-bottom-bar" style={{ position: "sticky", bottom: "70px" }} />
        <nav className="tabbar" style={{ position: "sticky", bottom: "0px" }} />
      </section>,
    );
    await user.click(screen.getByRole("button", { name: "Actions for api" }));
    expect(screen.getByRole("menu", { name: "Actions for api" })).toBeVisible();

    // Sliding the button under the stuck thead (bottom 136, below the sticky filter bar) closes the menu.
    buttonTop = 115;
    fireEvent.scroll(window);
    expect(screen.queryByRole("menu")).toBeNull();
    expect(screen.getByLabelText("Apps list")).toHaveFocus();

    // Re-open near the phone batch bar (top 710, 134 px above viewport bottom): menu opens above it,
    // and scrolling the button's center behind that bar closes the menu.
    buttonTop = 640;
    await user.click(screen.getByRole("button", { name: "Actions for api" }));
    const menu = screen.getByRole("menu", { name: "Actions for api" });
    expect(menu).toBeVisible();
    expect(menu.style.bottom).toBe("208px");
    buttonTop = 700;
    fireEvent.scroll(window);
    expect(screen.queryByRole("menu")).toBeNull();
  });

  it("shows live reasons for verbs disabled during a refresh and disables opening while loading (#956)", async () => {
    const user = userEvent.setup();
    const { rerender } = render(
      <ActionMenu label="Actions for api" loading actions={[{ id: "stop", label: "Stop", onSelect: vi.fn() }]} />,
    );
    const trigger = screen.getByRole("button", { name: "Actions for api" });
    expect(trigger).toBeDisabled();
    expect(trigger).toHaveAttribute("aria-busy", "true");

    rerender(
      <ActionMenu
        label="Actions for api"
        actions={[
          { id: "open", label: "Open", onSelect: vi.fn() },
          { id: "stop", label: "Stop", onSelect: vi.fn() },
        ]}
        reasons={{ stop: "Already stopped" }}
      />,
    );
    expect(trigger).toBeEnabled();
    await user.click(trigger);

    // A deploy starts while the menu is open: live reasons say "Busy: deploying", not "Already stopped".
    rerender(
      <ActionMenu
        label="Actions for api"
        actions={[{ id: "open", label: "Open", onSelect: vi.fn() }]}
        reasons={{ stop: "Busy: deploying" }}
      />,
    );
    const stopItem = screen.getByRole("menuitem", { name: /Stop/ });
    expect(stopItem).toHaveAttribute("aria-disabled", "true");
    expect(stopItem).toHaveTextContent("Busy: deploying");
    expect(stopItem).not.toHaveTextContent("Already stopped");
  });

  it("repositions on a layout shift observed by ResizeObserver without scroll or re-render (#956)", async () => {
    const user = userEvent.setup();
    let onResize: (() => void) | undefined;
    class FakeResizeObserver {
      constructor(cb: ResizeObserverCallback) {
        onResize = () => cb([], this as unknown as ResizeObserver);
      }
      observe() {}
      unobserve() {}
      disconnect() {}
    }
    vi.stubGlobal("ResizeObserver", FakeResizeObserver);
    vi.stubGlobal("innerHeight", 800);
    let buttonTop = 300;
    vi.spyOn(HTMLElement.prototype, "getBoundingClientRect").mockImplementation(function (this: HTMLElement) {
      if (this.getAttribute("role") === "menu") return new DOMRect(100, buttonTop + 32, 200, 100);
      if (this.tagName === "BUTTON") return new DOMRect(300, buttonTop, 28, 28);
      return new DOMRect(0, 0, 1280, 800);
    });
    render(<ActionMenu label="Actions for api" actions={[{ id: "stop", label: "Stop", onSelect: vi.fn() }]} />);
    await user.click(screen.getByRole("button", { name: "Actions for api" }));
    const menu = screen.getByRole("menu", { name: "Actions for api" });
    expect(menu).toHaveStyle({ top: "332px" });

    // A callout above grows without scrolling or re-rendering this component.
    buttonTop = 380;
    act(() => {
      onResize?.();
    });
    expect(menu).toHaveStyle({ top: "412px" });
  });
});
