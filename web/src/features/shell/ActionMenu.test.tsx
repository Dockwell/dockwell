// SPDX-License-Identifier: AGPL-3.0-only

import { render, screen, within } from "@testing-library/react";
import userEvent from "@testing-library/user-event";
import { describe, expect, it, vi } from "vitest";

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
});
