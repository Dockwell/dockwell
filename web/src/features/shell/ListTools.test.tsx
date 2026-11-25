// SPDX-License-Identifier: AGPL-3.0-only

import { Table, type TableColumn } from "@dockwell/ui";
import { I18nProvider } from "@lingui/react";
import { fireEvent, render as plainRender, screen, within } from "@testing-library/react";
import userEvent from "@testing-library/user-event";
import { useState } from "react";
import { describe, expect, it, vi } from "vitest";

import { i18n } from "../../app/i18n";
import { expectAccessible } from "../../test/a11y";
import { ListPager, ListSearchField, SortHeader, useListPlace, useRowMenu, useRowSelection } from "./ListTools";
import { type ListSearch, pagedRows, sortedRows } from "./list";

interface Row {
  id: string;
  name: string;
}

const render = (ui: React.ReactNode) => plainRender(<I18nProvider i18n={i18n}>{ui}</I18nProvider>);

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
        <button type="button">Menu item</button>
      </>
    );
  }

  it("gives focus back to the list, unless something took it first", async () => {
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
    screen.getByRole("button", { name: "Menu item" }).focus();
    for (const cb of frames.splice(0)) cb(0);
    expect(document.activeElement).toBe(screen.getByRole("button", { name: "Menu item" }));
    raf.mockRestore();
  });
});
