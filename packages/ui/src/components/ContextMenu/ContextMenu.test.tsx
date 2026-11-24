// SPDX-License-Identifier: AGPL-3.0-only

import { render, screen } from "@testing-library/react";
import userEvent from "@testing-library/user-event";
import { describe, expect, it, vi } from "vitest";

import { expectAccessible } from "../../test/a11y";
import { ContextMenu, type ContextMenuItem } from "./ContextMenu";

describe("ContextMenu", () => {
  it("renders items and separators, cycles with arrow keys, and closes on Escape", async () => {
    const onOpen = vi.fn();
    const onDelete = vi.fn();
    const onClose = vi.fn();
    const items: ContextMenuItem[] = [
      { id: "open", label: "Open", icon: "file", onSelect: onOpen },
      { id: "disabled", label: "Extract", disabled: true },
      { kind: "separator", id: "sep" },
      { id: "delete", label: "Delete", icon: "trash", danger: true, onSelect: onDelete },
    ];
    render(<ContextMenu open label="File actions" position={{ x: 100, y: 120 }} items={items} onClose={onClose} />);
    const menu = screen.getByRole("menu", { name: "File actions" });
    expect(menu).toBeVisible();
    await expectAccessible(menu);

    const openItem = screen.getByRole("menuitem", { name: "Open" });
    const deleteItem = screen.getByRole("menuitem", { name: "Delete" });
    expect(openItem).toHaveFocus();
    await userEvent.keyboard("{ArrowDown}");
    expect(deleteItem).toHaveFocus();
    await userEvent.keyboard("{ArrowUp}");
    expect(openItem).toHaveFocus();
    await userEvent.keyboard("{Escape}");
    expect(onClose).toHaveBeenCalledTimes(1);
  });

  it("calls onSelect and closes when an item is clicked", async () => {
    const onRename = vi.fn();
    const onClose = vi.fn();
    render(
      <ContextMenu
        open
        label="Row actions"
        position={{ x: 20, y: 30 }}
        items={[{ id: "rename", label: "Rename", shortcut: "F2", onSelect: onRename }]}
        onClose={onClose}
      />,
    );
    await userEvent.click(screen.getByRole("menuitem", { name: /Rename/ }));
    expect(onClose).toHaveBeenCalledTimes(1);
    expect(onRename).toHaveBeenCalledTimes(1);
  });

  it("supports download links", () => {
    render(
      <ContextMenu
        open
        label="Download menu"
        position={{ x: 20, y: 30 }}
        items={[{ id: "dl", label: "Download", icon: "download", href: "/dl", download: true }]}
        onClose={() => {}}
      />,
    );
    const link = screen.getByRole("menuitem", { name: "Download" });
    expect(link).toHaveAttribute("href", "/dl");
    expect(link).toHaveAttribute("download");
  });
});
