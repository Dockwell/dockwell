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
        ? { id: "pause", label: "Pause", onSelect: () => onAction("pause", row) }
        : { id: "start", label: "Start", onSelect: () => onAction("start", row) },
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

  it("keeps an open ActionMenu open with focus through a refresh and layout shift, and closes cleanly with focus back to the list when its row goes", async () => {
    const user = userEvent.setup();
    const onAction = vi.fn();
    let buttonTop = 400;
    const rectSpy = vi.spyOn(HTMLElement.prototype, "getBoundingClientRect").mockImplementation(function (
      this: HTMLElement,
    ) {
      if (this.getAttribute("role") === "menu") return new DOMRect(100, buttonTop + 32, 220, 100);
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
      await user.keyboard("{ArrowDown}");
      expect(within(menu).getByRole("menuitem", { name: "Pause" })).toHaveFocus();

      // Refresh removes a row above (shifting the button upward) and updates beta's state,
      // and a scroll event fires as the shorter page clamps its scroll offset.
      buttonTop = 200;
      const updated: LiveRow[] = [{ id: "b", name: "beta", running: false }];
      rerender(<RefreshableList rows={updated} onAction={onAction} />);
      fireEvent.scroll(window);

      const stillOpen = screen.getByRole("menu", { name: "Actions for beta" });
      expect(stillOpen).toBeVisible();
      expect(stillOpen).toHaveStyle({ top: "232px" });
      // Pause was replaced by Start, so focus stays inside the menu instead of dropping to body.
      expect(stillOpen.contains(document.activeElement)).toBe(true);
      expect(within(stillOpen).getByRole("menuitem", { name: "Start" })).toBeVisible();

      // Refresh removes beta completely: the menu closes and focus returns cleanly to the list.
      rerender(<RefreshableList rows={[]} onAction={onAction} />);
      await Promise.resolve();
      expect(screen.queryByRole("menu")).toBeNull();
      expect(screen.getByLabelText("Apps list")).toHaveFocus();
    } finally {
      rectSpy.mockRestore();
    }
  });

  it("keeps an open row context menu open with focus and live actions through a refresh, and closes cleanly when its row goes", async () => {
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
    expect(within(menu).getByRole("menuitem", { name: "Pause" })).toHaveFocus();

    // Refresh changes beta's data while keeping its identity.
    const refreshed: LiveRow[] = [{ id: "b", name: "beta-v2", running: true }];
    rerender(<RefreshableList rows={refreshed} onAction={onAction} />);
    const updatedMenu = screen.getByRole("menu", { name: "Actions for beta-v2" });
    expect(updatedMenu).toBeVisible();
    expect(within(updatedMenu).getByRole("menuitem", { name: "Pause" })).toHaveFocus();

    // Refresh removes beta: context menu closes cleanly and focus returns to the list.
    rerender(<RefreshableList rows={[{ id: "a", name: "alpha", running: true }]} onAction={onAction} />);
    expect(screen.queryByRole("menu")).toBeNull();
    expect(screen.getByLabelText("Apps list")).toHaveFocus();
  });
});
