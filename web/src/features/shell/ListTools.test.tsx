// SPDX-License-Identifier: AGPL-3.0-only

import { Table, type TableColumn } from "@dockwell/ui";
import { I18nProvider } from "@lingui/react";
import { fireEvent, render as plainRender, screen, within } from "@testing-library/react";
import userEvent from "@testing-library/user-event";
import { useState } from "react";
import { describe, expect, it, vi } from "vitest";

import { i18n } from "../../app/i18n";
import { expectAccessible } from "../../test/a11y";
import { ActionMenu } from "./ActionMenu";
import { ListPager, ListSearchField, SortHeader, useListPlace, useRowMenu, useRowSelection } from "./ListTools";
import { type ListSearch, pagedRows, sortedRows } from "./list";

interface Row {
  id: string;
  name: string;
}

const render = (ui: React.ReactNode) =>
  plainRender(ui, { wrapper: ({ children }) => <I18nProvider i18n={i18n}>{children}</I18nProvider> });

const ALL: Row[] = Array.from({ length: 120 }, (_, i) => ({
  id: `r${i + 1}`,
  name: `item ${String(120 - i).padStart(3, "0")}`,
}));

/** A list on the toolkit, as a page holds it: its place in state where a route would hold it. */
function List({ onOpen }: { onOpen: (row: Row) => void }) {
  const [search, setSearch] = useState<ListSearch>({});
  const place = useListPlace(search, setSearch, ["name"]);
  const menu = useRowMenu<Row>();
  const filtered = ALL.filter((r) => r.name.includes(place.query));
  const sorted = sortedRows(filtered, place.sort, (r) => r.name);
  const page = pagedRows(sorted, place.page);
  const columns: TableColumn<Row>[] = [
    {
      key: "name",
      header: <SortHeader label="Name" sortKey="name" place={place} />,
      sort: place.sortOf("name"),
    },
  ];
  return (
    <>
      <ListSearchField
        label="Filter items"
        placeholder="Filter by name"
        value={place.typed}
        onChange={place.setTyped}
      />
      <Table<Row>
        columns={columns}
        rows={page.rows}
        caption="Items"
        onRowClick={onOpen}
        onRowContextMenu={(row, at, el) =>
          menu.open(row, at, el, {
            label: `Actions for ${row.name}`,
            actions: [{ id: "open", icon: "external", label: "Open", onSelect: () => onOpen(row) }],
          })
        }
      />
      <ListPager page={page} onPage={place.setPage} />
      {menu.element}
    </>
  );
}

describe("the shared list toolkit", () => {
  it("pages over the whole list with its count, and walks it", async () => {
    const user = userEvent.setup();
    const { container } = render(<List onOpen={vi.fn()} />);
    const table = screen.getByRole("table", { name: "Items" });
    expect(within(table).getAllByRole("row")).toHaveLength(51);
    expect(screen.getByText("1-50 of 120")).toBeVisible();
    await user.click(screen.getByRole("button", { name: /Next/ }));
    expect(screen.getByText("51-100 of 120")).toBeVisible();
    await user.click(screen.getByRole("button", { name: /Previous/ }));
    expect(screen.getByText("1-50 of 120")).toBeVisible();
    await expectAccessible(container);
  });

  it("sorts by its head, the other way on a second click, and says so", async () => {
    const user = userEvent.setup();
    render(<List onOpen={vi.fn()} />);
    const table = screen.getByRole("table", { name: "Items" });
    const first = () => within(table).getAllByRole("row")[1]?.textContent;
    expect(first()).toContain("item 120");
    await user.click(within(table).getByRole("button", { name: /Name/ }));
    expect(first()).toContain("item 001");
    expect(within(table).getByRole("columnheader")).toHaveAttribute("aria-sort", "ascending");
    await user.click(within(table).getByRole("button", { name: /Name/ }));
    expect(first()).toContain("item 120");
    expect(within(table).getByRole("columnheader")).toHaveAttribute("aria-sort", "descending");
  });

  it("filters as you type, from / anywhere, and starts again at the first page", async () => {
    const user = userEvent.setup();
    render(<List onOpen={vi.fn()} />);
    await user.click(screen.getByRole("button", { name: /Next/ }));
    expect(screen.getByText("51-100 of 120")).toBeVisible();
    await user.keyboard("/");
    const field = screen.getByRole("searchbox", { name: "Filter items" });
    expect(field).toHaveFocus();
    await user.type(field, "item 11");
    const table = screen.getByRole("table", { name: "Items" });
    expect(await within(table).findByText("item 110")).toBeVisible();
    // item 110 to item 119: ten rows and the head, on one page again, so no pager.
    expect(within(table).getAllByRole("row")).toHaveLength(11);
    expect(screen.queryByText(/of 120/)).toBeNull();
  });

  it("opens a row's menu where a right-click or Shift+F10 asked for it, and gives focus back", async () => {
    const user = userEvent.setup();
    const onOpen = vi.fn();
    const { container } = render(<List onOpen={onOpen} />);
    const table = screen.getByRole("table", { name: "Items" });
    const row = within(table).getByRole("row", { name: /item 120/ });
    await user.pointer({ keys: "[MouseRight]", target: row });
    const menu = screen.getByRole("menu", { name: "Actions for item 120" });
    expect(within(menu).getByRole("menuitem", { name: "Open" })).toHaveFocus();
    await expectAccessible(container);
    await user.keyboard("{Escape}");
    expect(screen.queryByRole("menu")).toBeNull();
    expect(row).toHaveFocus();
    await user.keyboard("{Shift>}{F10}{/Shift}");
    fireEvent.scroll(window);
    expect(screen.getByRole("menu", { name: "Actions for item 120" })).toBeVisible();
    await user.click(screen.getByRole("menuitem", { name: "Open" }));
    expect(onOpen).toHaveBeenCalledWith(expect.objectContaining({ name: "item 120" }));
    expect(screen.queryByRole("menu")).toBeNull();
  });
});

describe("clearing a selection", () => {
  function Picked() {
    const selection = useRowSelection(["a", "b"]);
    return (
      <>
        <section ref={selection.list as React.RefObject<HTMLElement>} tabIndex={-1} aria-label="List" />
        <button type="button" onClick={selection.clear}>
          Clear
        </button>
        <div role="menu" aria-label="Row menu">
          <button type="button" role="menuitem">
            Menu item
          </button>
        </div>
        <button type="button">Elsewhere</button>
      </>
    );
  }

  it("gives focus back to the list, unless a menu took it first", async () => {
    const frames: FrameRequestCallback[] = [];
    const raf = vi.spyOn(window, "requestAnimationFrame").mockImplementation((cb) => frames.push(cb));
    plainRender(<Picked />);
    const clear = screen.getByRole("button", { name: "Clear" });
    clear.focus();
    fireEvent.click(clear);
    for (const cb of frames.splice(0)) cb(0);
    expect(document.activeElement).toBe(screen.getByLabelText("List"));

    clear.focus();
    fireEvent.click(clear);
    screen.getByRole("menuitem", { name: "Menu item" }).focus();
    for (const cb of frames.splice(0)) cb(0);
    expect(document.activeElement).toBe(screen.getByRole("menuitem", { name: "Menu item" }));

    // Focus that lands anywhere else, as a closing sheet gives it back, still goes to the list.
    clear.focus();
    fireEvent.click(clear);
    screen.getByRole("button", { name: "Elsewhere" }).focus();
    for (const cb of frames.splice(0)) cb(0);
    expect(document.activeElement).toBe(screen.getByLabelText("List"));
    raf.mockRestore();
  });

  it("keeps focus on a row inside the list that took focus right after Clear (#956)", () => {
    const frames: FrameRequestCallback[] = [];
    const raf = vi.spyOn(window, "requestAnimationFrame").mockImplementation((cb) => frames.push(cb));
    function PickedWithRow() {
      const selection = useRowSelection(["a", "b"]);
      return (
        <section ref={selection.list as React.RefObject<HTMLElement>} tabIndex={-1} aria-label="List">
          <button type="button">Row A</button>
          <div className="list-batch-bar">
            <button type="button" onClick={selection.clear}>
              Clear
            </button>
          </div>
        </section>
      );
    }
    plainRender(<PickedWithRow />);
    const clear = screen.getByRole("button", { name: "Clear" });
    const row = screen.getByRole("button", { name: "Row A" });
    clear.focus();
    fireEvent.click(clear);
    row.focus();
    for (const cb of frames.splice(0)) cb(0);
    expect(document.activeElement).toBe(row);
    raf.mockRestore();
  });
});

describe("row menus across a list refresh (#956)", () => {
  interface LiveRow {
    id: string;
    name: string;
    running: boolean;
  }

  function RefreshableList({
    rows,
    onAction,
  }: {
    rows: readonly LiveRow[];
    onAction: (action: string, row: LiveRow) => void;
  }) {
    const actionsFor = (row: LiveRow) => [
      { id: "open", label: "Open", onSelect: () => onAction("open", row) },
      row.running
        ? { id: "stop", label: "Stop", unavailable: "Already stopped", onSelect: () => onAction("stop", row) }
        : { id: "start", label: "Start", unavailable: "Already running", onSelect: () => onAction("start", row) },
      ...(row.running
        ? [
            {
              id: "pause",
              label: "Pause",
              unavailable: "Not running or already paused",
              onSelect: () => onAction("pause", row),
            },
          ]
        : []),
    ];
    const menu = useRowMenu<LiveRow>((row) => ({ label: `Actions for ${row.name}`, actions: actionsFor(row) }), {
      rows,
      keyOf: (row) => row.id,
    });
    const columns: TableColumn<LiveRow>[] = [
      { key: "name", header: "Name" },
      {
        key: "actions",
        header: "Actions",
        render: (row) => <ActionMenu label={`Actions for ${row.name}`} actions={actionsFor(row)} />,
      },
    ];
    return (
      <section tabIndex={-1} aria-label="Apps list">
        <Table<LiveRow>
          columns={columns}
          rows={rows}
          caption="Apps"
          onRowContextMenu={(row, at, el) => menu.open(row, at, el)}
        />
        {menu.element}
      </section>
    );
  }

  it("keeps an open ActionMenu's verbs fixed across a refresh, keeps focus on a now-disabled item, and returns focus to the list when its row goes", async () => {
    const user = userEvent.setup();
    const onAction = vi.fn();
    let buttonTop = 400;
    const rectSpy = vi.spyOn(HTMLElement.prototype, "getBoundingClientRect").mockImplementation(function (
      this: HTMLElement,
    ) {
      if (this.getAttribute("role") === "menu") return new DOMRect(100, buttonTop + 32, 220, 100);
      if (this.tagName === "THEAD") return new DOMRect(0, 0, 800, 36);
      if (this.tagName === "BUTTON") return new DOMRect(300, buttonTop, 28, 28);
      return new DOMRect(0, 0, 800, 600);
    });
    try {
      const initial: LiveRow[] = [
        { id: "a", name: "alpha", running: true },
        { id: "b", name: "beta", running: true },
      ];
      const { rerender } = render(<RefreshableList rows={initial} onAction={onAction} />);
      await user.click(screen.getByRole("button", { name: "Actions for beta" }));
      const menu = screen.getByRole("menu", { name: "Actions for beta" });
      await user.keyboard("{ArrowDown}{ArrowDown}");
      expect(within(menu).getByRole("menuitem", { name: "Pause" })).toHaveFocus();

      // Refresh removes a row above (shifting the button upward) and stops beta,
      // and a scroll event fires as the shorter page clamps its scroll offset.
      buttonTop = 200;
      const updated: LiveRow[] = [{ id: "b", name: "beta", running: false }];
      rerender(<RefreshableList rows={updated} onAction={onAction} />);
      fireEvent.scroll(window);

      const stillOpen = screen.getByRole("menu", { name: "Actions for beta" });
      expect(stillOpen).toBeVisible();
      expect(stillOpen).toHaveStyle({ top: "232px" });
      // Stop and Pause turn disabled in place; Start is not swapped in while the menu is open.
      expect(within(stillOpen).queryByRole("menuitem", { name: "Start" })).toBeNull();
      const stopItem = within(stillOpen).getByRole("menuitem", { name: /Stop/ });
      expect(stopItem).toHaveAttribute("aria-disabled", "true");
      expect(stopItem).toHaveTextContent("Already stopped");
      // Focus stays on the exact Pause item (now disabled), not jumped back to Open.
      const pauseItem = within(stillOpen).getByRole("menuitem", { name: /Pause/ });
      expect(pauseItem).toHaveAttribute("aria-disabled", "true");
      expect(pauseItem).toHaveTextContent("Not running or already paused");
      expect(pauseItem).toHaveFocus();
      // Clicking the disabled Stop slot or pressing Enter on disabled Pause does not fire Start, Stop, or Open.
      await user.keyboard("{Enter}");
      await user.click(stopItem);
      expect(onAction).not.toHaveBeenCalled();

      // Re-opening the menu shows the new Start verb.
      await user.keyboard("{Escape}");
      await user.click(screen.getByRole("button", { name: "Actions for beta" }));
      expect(
        within(screen.getByRole("menu", { name: "Actions for beta" })).getByRole("menuitem", { name: "Start" }),
      ).toBeVisible();

      // Refresh removes beta completely: the menu closes and focus returns cleanly to the list.
      rerender(<RefreshableList rows={[]} onAction={onAction} />);
      await Promise.resolve();
      expect(screen.queryByRole("menu")).toBeNull();
      expect(screen.getByLabelText("Apps list")).toHaveFocus();
    } finally {
      rectSpy.mockRestore();
    }
  });

  it("keeps an open row context menu's verbs fixed while updating the row's label, keeps focus on a disabled item, and closes when its row goes", async () => {
    const user = userEvent.setup();
    const onAction = vi.fn();
    const initial: LiveRow[] = [
      { id: "a", name: "alpha", running: true },
      { id: "b", name: "beta", running: true },
    ];
    const { rerender } = render(<RefreshableList rows={initial} onAction={onAction} />);
    const betaRow = screen.getByRole("row", { name: /beta/ });
    await user.pointer({ keys: "[MouseRight]", target: betaRow });
    const menu = screen.getByRole("menu", { name: "Actions for beta" });
    await user.keyboard("{ArrowDown}");
    expect(within(menu).getByRole("menuitem", { name: "Stop" })).toHaveFocus();

    // Refresh renames beta and stops it: label updates live, Stop turns disabled in place (Start is not swapped in),
    // and focus stays on the exact Stop item.
    const refreshed: LiveRow[] = [{ id: "b", name: "beta-v2", running: false }];
    rerender(<RefreshableList rows={refreshed} onAction={onAction} />);
    const updatedMenu = screen.getByRole("menu", { name: "Actions for beta-v2" });
    expect(updatedMenu).toBeVisible();
    expect(within(updatedMenu).queryByRole("menuitem", { name: "Start" })).toBeNull();
    const stopItem = within(updatedMenu).getByRole("menuitem", { name: /Stop/ });
    expect(stopItem).toHaveAttribute("aria-disabled", "true");
    expect(stopItem).toHaveTextContent("Already stopped");
    expect(stopItem).toHaveFocus();
    await user.keyboard("{Enter}");
    await user.click(stopItem);
    expect(onAction).not.toHaveBeenCalled();

    // Refresh removes beta: context menu closes cleanly and focus returns to the list.
    rerender(<RefreshableList rows={[{ id: "a", name: "alpha", running: true }]} onAction={onAction} />);
    expect(screen.queryByRole("menu")).toBeNull();
    expect(screen.getByLabelText("Apps list")).toHaveFocus();
  });

  it("opens a row context menu at the pointer even on the last row of a short scrolling wrapper, and closes under the sticky top bar (#956)", () => {
    const onAction = vi.fn();
    let rowTop = 260;
    const rectSpy = vi.spyOn(HTMLElement.prototype, "getBoundingClientRect").mockImplementation(function (
      this: HTMLElement,
    ) {
      if (this.classList.contains("topbar")) return new DOMRect(0, 0, 1280, 52);
      if (this.classList.contains("dw-table-wrap")) return new DOMRect(0, 224, 800, 84);
      if (this.tagName === "THEAD") return new DOMRect(0, 224, 800, 36);
      if (this.getAttribute("role") === "menu") return new DOMRect(0, 0, 200, 140);
      if (this.tagName === "TR" && this.parentElement?.tagName === "TBODY") return new DOMRect(0, rowTop, 800, 48);
      return new DOMRect(0, 0, 1280, 800);
    });
    try {
      const initial: LiveRow[] = [{ id: "b", name: "beta", running: true }];
      render(
        <div>
          <header className="topbar" />
          <RefreshableList rows={initial} onAction={onAction} />
        </div>,
      );
      const wrap = document.querySelector(".dw-table-wrap") as HTMLElement;
      wrap.style.overflow = "auto";
      const row = screen.getByRole("row", { name: /beta/ });
      fireEvent.contextMenu(row, { clientX: 140, clientY: 284 });
      const menu = screen.getByRole("menu", { name: "Actions for beta" });
      expect(menu).toBeVisible();
      // Fixed menu extends below the 84px table wrap (bottom 308) and stays at the pointer (284px).
      expect(menu).toHaveStyle({ left: "140px", top: "284px" });

      rowTop = 36;
      fireEvent.scroll(window);
      expect(screen.queryByRole("menu")).toBeNull();
    } finally {
      rectSpy.mockRestore();
    }
  });
});
