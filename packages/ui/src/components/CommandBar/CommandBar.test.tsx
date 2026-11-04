// SPDX-License-Identifier: AGPL-3.0-only

import { fireEvent, render, screen, within } from "@testing-library/react";
import userEvent from "@testing-library/user-event";
import { useState } from "react";
import { describe, expect, it, vi } from "vitest";

import { englishStrings, UIStringsProvider } from "../../lib/strings";
import { expectAccessible } from "../../test/a11y";
import { CommandBar, type CommandItem } from "./CommandBar";

const items: CommandItem[] = [
  { id: "deploy", group: "Actions", icon: "deploy", label: "Deploy api-gateway to production", hint: "main · 8be41d2" },
  { id: "restart", group: "Actions", icon: "refresh", label: "Restart api-gateway in staging" },
  { id: "rollback", group: "Actions", icon: "undo", label: "Roll back web to r63" },
  { id: "fleet", group: "Go to", icon: "server", label: "Fleet", kbd: "G F" },
  { id: "apps", group: "Go to", label: "Apps" },
  { id: "fra", label: "fra-1-07", hint: "Degraded · 10.210.4.17" },
];

const combobox = () => screen.getByRole("combobox");
const activeOption = () => {
  const id = combobox().getAttribute("aria-activedescendant");
  return id ? document.getElementById(id) : null;
};

describe("CommandBar inline", () => {
  it("tells its owner what is typed, for items that take it", async () => {
    const user = userEvent.setup();
    const typed: string[] = [];
    render(<CommandBar inline items={items} onQueryChange={(q) => typed.push(q)} />);
    await user.type(combobox(), "web");
    expect(typed).toEqual(["w", "we", "web"]);
  });

  it("lists every item under its group while the query is empty", async () => {
    const { container } = render(<CommandBar inline items={items} scope="Acme / storefront" />);
    expect(screen.getByRole("search", { name: "Command bar" })).toBeVisible();
    expect(combobox()).toHaveAccessibleName("Search or run a command");
    expect(combobox()).toHaveAttribute("aria-expanded", "true");
    const listbox = screen.getByRole("listbox", { name: "Results" });
    expect(combobox()).toHaveAttribute("aria-controls", listbox.id);
    expect(within(listbox).getAllByRole("option")).toHaveLength(6);
    expect(screen.getByRole("group", { name: "Actions" })).toBeVisible();
    expect(within(screen.getByRole("group", { name: "Go to" })).getAllByRole("option")).toHaveLength(2);
    expect(activeOption()).toHaveTextContent("Deploy api-gateway to production");
    expect(activeOption()).toHaveAttribute("aria-selected", "true");
    expect(screen.getByText("Acme / storefront").tagName).toBe("B");
    await expectAccessible(container);
  });

  it("filters fuzzily, highlights matched characters and drops the groups", async () => {
    const user = userEvent.setup();
    render(<CommandBar inline items={items} />);
    await user.type(combobox(), "rb web");
    const options = screen.getAllByRole("option");
    expect(options).toHaveLength(1);
    expect(options[0]).toHaveTextContent("Roll back web to r63");
    expect(Array.from(options[0]?.querySelectorAll("mark") ?? []).map((m) => m.textContent)).toEqual(["R", "b", "web"]);
    expect(screen.queryByRole("group")).toBeNull();
  });

  it("ranks better matches first", async () => {
    const user = userEvent.setup();
    render(<CommandBar inline items={items} />);
    await user.type(combobox(), "api");
    expect(screen.getAllByRole("option").map((o) => o.textContent?.slice(0, 6))).toEqual(["Deploy", "Restar"]);
  });

  it("moves the active option with arrow keys, stopping at the ends, and runs it with Enter", async () => {
    const user = userEvent.setup();
    const onSelect = vi.fn();
    render(<CommandBar inline items={items} onSelect={onSelect} />);
    await user.click(combobox());
    await user.keyboard("{ArrowUp}");
    expect(activeOption()).toHaveTextContent("Deploy api-gateway");
    await user.keyboard("{ArrowDown}{ArrowDown}");
    expect(activeOption()).toHaveTextContent("Roll back web");
    expect(screen.getAllByRole("option", { selected: true })).toHaveLength(1);
    await user.keyboard("{ArrowDown}{ArrowDown}{ArrowDown}{ArrowDown}{ArrowDown}");
    expect(activeOption()).toHaveTextContent("fra-1-07");
    await user.keyboard("{Enter}");
    expect(onSelect).toHaveBeenCalledWith(items[5]);
    expect(combobox()).toHaveFocus();
  });

  it("starts from the first result again when the query changes", async () => {
    const user = userEvent.setup();
    render(<CommandBar inline items={items} />);
    await user.click(combobox());
    await user.keyboard("{ArrowDown}{ArrowDown}");
    await user.type(combobox(), "a");
    expect(activeOption()).toBe(screen.getAllByRole("option")[0]);
  });

  it("runs an item on click and follows the pointer", async () => {
    const user = userEvent.setup();
    const onSelect = vi.fn();
    render(<CommandBar inline items={items} onSelect={onSelect} />);
    const fleet = screen.getByRole("option", { name: /Fleet/ });
    await user.hover(fleet);
    expect(activeOption()).toBe(fleet);
    await user.click(fleet);
    expect(onSelect).toHaveBeenCalledWith(items[3]);
  });

  it("names an option by its label, and gives its shortcut as aria-keyshortcuts", async () => {
    const { container } = render(
      <CommandBar
        inline
        items={[...items, { id: "palette", group: "Go to", icon: "search", label: "Search everything", kbd: "⇧⌘F" }]}
      />,
    );
    // The key caps stay on screen and out of the name.
    const fleet = screen.getByRole("option", { name: "Fleet" });
    expect(within(fleet).getByText("G F").tagName).toBe("KBD");
    expect(within(fleet).getByText("G F")).toHaveAttribute("aria-hidden", "true");
    // aria-keyshortcuts can't say a sequence of keys, so it's left out.
    expect(fleet).not.toHaveAttribute("aria-keyshortcuts");
    expect(screen.getByRole("option", { name: "Search everything" })).toHaveAttribute(
      "aria-keyshortcuts",
      "Shift+Meta+F",
    );
    expect(screen.getByRole("option", { name: "Apps" })).not.toHaveAttribute("aria-keyshortcuts");
    await expectAccessible(container);
  });

  it("says when nothing matches, in a live region", async () => {
    const user = userEvent.setup();
    const onSelect = vi.fn();
    const { container } = render(<CommandBar inline items={items} onSelect={onSelect} defaultQuery="zzz" />);
    expect(screen.getByRole("status")).toHaveTextContent("Nothing matches “zzz”. Try a server, app or action name.");
    expect(combobox()).toHaveAttribute("aria-expanded", "false");
    expect(combobox()).not.toHaveAttribute("aria-activedescendant");
    await user.type(combobox(), "{ArrowDown}{Enter}");
    expect(onSelect).not.toHaveBeenCalled();
    await user.clear(combobox());
    expect(activeOption()).toHaveTextContent("Deploy api-gateway to production");
    await expectAccessible(container);
  });

  it("closes on Escape and leaves Enter to an input method that is composing", () => {
    const onClose = vi.fn();
    const onSelect = vi.fn();
    render(<CommandBar inline items={items} onClose={onClose} onSelect={onSelect} />);
    fireEvent.keyDown(combobox(), { key: "Enter", isComposing: true });
    fireEvent.keyDown(combobox(), { key: "Enter", keyCode: 229 });
    expect(onSelect).not.toHaveBeenCalled();
    fireEvent.keyDown(combobox(), { key: "Escape" });
    expect(onClose).toHaveBeenCalledOnce();
  });

  it("focuses itself inline only when asked", () => {
    const { unmount } = render(<CommandBar inline items={items} />);
    expect(combobox()).not.toHaveFocus();
    unmount();
    render(<CommandBar inline autoFocus items={items} />);
    expect(combobox()).toHaveFocus();
  });

  it("speaks the app's language", async () => {
    const user = userEvent.setup();
    render(
      <UIStringsProvider
        strings={{
          ...englishStrings,
          commandBar: {
            ...englishStrings.commandBar,
            placeholder: "搜索或运行命令",
            empty: (query) => `没有与“${query}”匹配的结果。`,
            scope: (scope) => <>范围：{scope}</>,
          },
        }}
      >
        <CommandBar inline items={items} scope="Acme / storefront" />
      </UIStringsProvider>,
    );
    expect(screen.getByRole("combobox", { name: "搜索或运行命令" })).toHaveAttribute("placeholder", "搜索或运行命令");
    expect(screen.getByText("范围：Acme / storefront")).toBeVisible();
    await user.type(combobox(), "qq");
    expect(screen.getByRole("status")).toHaveTextContent("没有与“qq”匹配的结果。");
  });
});

function Harness({ onSelect }: { onSelect?: (item: CommandItem) => void }) {
  const [open, setOpen] = useState(false);
  return (
    <div>
      <button type="button" onClick={() => setOpen(true)}>
        Search
      </button>
      <CommandBar
        open={open}
        items={items}
        onClose={() => setOpen(false)}
        onSelect={(item) => {
          onSelect?.(item);
          setOpen(false);
        }}
      />
    </div>
  );
}

describe("CommandBar overlay", () => {
  it("renders nothing while closed", () => {
    render(<CommandBar items={items} />);
    expect(screen.queryByRole("dialog")).toBeNull();
  });

  it("opens in a portal as a modal dialog with focus in the input", async () => {
    const user = userEvent.setup();
    const { container } = render(<Harness />);
    await user.click(screen.getByRole("button", { name: "Search" }));
    const dialog = screen.getByRole("dialog", { name: "Command bar" });
    expect(dialog).toHaveAttribute("aria-modal", "true");
    expect(container.contains(dialog)).toBe(false);
    expect(combobox()).toHaveFocus();
    expect(document.body.style.overflow).toBe("hidden");
    await expectAccessible(document.body);
  });

  it("keeps focus inside while open", async () => {
    const user = userEvent.setup();
    render(<Harness />);
    await user.click(screen.getByRole("button", { name: "Search" }));
    await user.tab();
    expect(combobox()).toHaveFocus();
    await user.tab({ shift: true });
    expect(combobox()).toHaveFocus();
    screen.getByRole("button", { name: "Search", hidden: true }).focus();
    expect(combobox()).toHaveFocus();
  });

  it("closes on Escape and returns focus to the opener", async () => {
    const user = userEvent.setup();
    render(<Harness />);
    const opener = screen.getByRole("button", { name: "Search" });
    await user.click(opener);
    await user.keyboard("{Escape}");
    expect(screen.queryByRole("dialog")).toBeNull();
    expect(opener).toHaveFocus();
    expect(document.body.style.overflow).toBe("");
  });

  it("leaves Escape to an input method while it composes", async () => {
    const user = userEvent.setup();
    render(<Harness />);
    await user.click(screen.getByRole("button", { name: "Search" }));
    // Escape dismisses the pinyin candidates; the command bar stays.
    fireEvent.keyDown(combobox(), { key: "Escape", isComposing: true });
    fireEvent.keyDown(combobox(), { key: "Escape", keyCode: 229 });
    expect(screen.getByRole("dialog", { name: "Command bar" })).toBeVisible();
    fireEvent.keyDown(combobox(), { key: "Escape" });
    expect(screen.queryByRole("dialog")).toBeNull();
  });

  it("closes on a click on the scrim", async () => {
    const user = userEvent.setup();
    render(<Harness />);
    await user.click(screen.getByRole("button", { name: "Search" }));
    const scrim = document.querySelector(".dw-scrim");
    expect(scrim).not.toBeNull();
    if (scrim) await user.click(scrim);
    expect(screen.queryByRole("dialog")).toBeNull();
  });

  it("runs the chosen item, then hands focus back", async () => {
    const user = userEvent.setup();
    const onSelect = vi.fn();
    render(<Harness onSelect={onSelect} />);
    const opener = screen.getByRole("button", { name: "Search" });
    await user.click(opener);
    await user.keyboard("fleet{Enter}");
    expect(onSelect).toHaveBeenCalledWith(items[3]);
    expect(screen.queryByRole("dialog")).toBeNull();
    expect(opener).toHaveFocus();
  });

  it("fills a phone screen and closes with Cancel, as there's no Escape key", async () => {
    vi.stubGlobal("matchMedia", (query: string) => ({
      matches: query.includes("max-width"),
      addEventListener: () => {},
      removeEventListener: () => {},
    }));
    const user = userEvent.setup();
    render(<Harness />);
    const opener = screen.getByRole("button", { name: "Search" });
    await user.click(opener);
    const dialog = screen.getByRole("dialog", { name: "Command bar" });
    expect(within(dialog).queryByText("esc")).toBeNull();
    await user.click(within(dialog).getByRole("button", { name: "Cancel" }));
    expect(screen.queryByRole("dialog")).toBeNull();
    expect(opener).toHaveFocus();
  });

  it("shows Escape instead of Cancel on wider screens", async () => {
    const user = userEvent.setup();
    render(<Harness />);
    await user.click(screen.getByRole("button", { name: "Search" }));
    expect(screen.getByText("esc").tagName).toBe("KBD");
    expect(screen.queryByRole("button", { name: "Cancel" })).toBeNull();
  });

  it("starts with an empty query each time it opens", async () => {
    const user = userEvent.setup();
    render(<Harness />);
    await user.click(screen.getByRole("button", { name: "Search" }));
    await user.keyboard("web{Escape}");
    await user.click(screen.getByRole("button", { name: "Search" }));
    expect(combobox()).toHaveValue("");
  });
});
