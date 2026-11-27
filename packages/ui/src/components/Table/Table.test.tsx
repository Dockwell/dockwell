// SPDX-License-Identifier: AGPL-3.0-only

import { render, screen, within } from "@testing-library/react";
import userEvent from "@testing-library/user-event";
import { describe, expect, it, vi } from "vitest";

import { englishStrings, UIStringsProvider } from "../../lib/strings";
import { expectAccessible } from "../../test/a11y";
import { Button } from "../Button/Button";
import { StatusBadge } from "../StatusBadge/StatusBadge";
import { Table, type TableColumn } from "./Table";

interface App {
  id: string;
  name: string;
  image: string;
  status: "healthy" | "degraded";
  replicas: string;
  domain: string;
  cpu: readonly number[];
  placement: { region: string };
}

const rows: App[] = [
  {
    id: "api",
    name: "api-gateway",
    image: "ghcr.io/acme/api:8be41d2",
    status: "healthy",
    replicas: "3/3",
    domain: "api.acme.dev",
    cpu: [1],
    placement: { region: "fra-1" },
  },
  {
    id: "worker",
    name: "worker",
    image: "ghcr.io/acme/worker:9a03e1b",
    status: "degraded",
    replicas: "4/4",
    domain: "—",
    cpu: [2],
    placement: { region: "fra-1" },
  },
];

const columns: TableColumn<App>[] = [
  {
    key: "name",
    header: "App",
    render: (row) => (
      <div className="dw-cell">
        <span className="dw-cell-title">{row.name}</span>
        <span className="dw-cell-sub">{row.image}</span>
      </div>
    ),
  },
  { key: "status", header: "Status", width: 140, render: (row) => <StatusBadge status={row.status} /> },
  { key: "replicas", header: "Replicas", align: "right" },
  { key: "domain", header: "Domain", mono: true, muted: true },
];

// Type checks: a column without render must name a value React can show.
// @ts-expect-error there is no such field
const _missing: TableColumn<App> = { key: "missing", header: "Missing" };
// @ts-expect-error an object needs render
const _object: TableColumn<App> = { key: "placement", header: "Placement" };
const _rendered: TableColumn<App> = { key: "placement", header: "Placement", render: (row) => row.placement.region };
void [_missing, _object, _rendered];

describe("Table", () => {
  it("is a real table named by its caption, with column headers", async () => {
    const { container } = render(<Table caption="Apps in production" columns={columns} rows={rows} />);
    const table = screen.getByRole("table", { name: "Apps in production" });
    expect(
      within(table)
        .getAllByRole("columnheader")
        .map((th) => th.textContent),
    ).toEqual(["App", "Status", "Replicas", "Domain"]);
    expect(screen.getByRole("columnheader", { name: "Status" })).toHaveStyle({ width: "140px" });
    expect(screen.getByRole("columnheader", { name: "Replicas" })).toHaveClass("is-num");
    expect(within(table).getAllByRole("row")).toHaveLength(3);
    expect(screen.getByRole("cell", { name: "3/3" })).toHaveClass("is-num");
    expect(screen.getByRole("cell", { name: "api.acme.dev" })).toHaveClass("is-mono", "is-muted");
    expect(screen.getByText("ghcr.io/acme/api:8be41d2")).toHaveClass("dw-cell-sub");
    expect(table).toHaveClass("dw-table-comfortable");
    expect(screen.queryAllByRole("row").some((row) => row.hasAttribute("tabindex"))).toBe(false);
    await expectAccessible(container);
  });

  it("says what would appear when empty", async () => {
    const { container, rerender } = render(<Table caption="Apps" columns={columns} rows={[]} />);
    expect(screen.getByRole("cell", { name: "Nothing here yet." })).toHaveAttribute("colspan", "4");
    rerender(
      <Table
        caption="Apps"
        columns={columns}
        rows={[]}
        empty="No apps yet. Deploy one from an image or a repository."
      />,
    );
    expect(screen.getByText("No apps yet. Deploy one from an image or a repository.")).toBeVisible();
    await expectAccessible(container);
    rerender(
      <UIStringsProvider strings={{ ...englishStrings, tableEmpty: "这里还没有内容。" }}>
        <Table caption="Apps" columns={columns} rows={[]} />
      </UIStringsProvider>,
    );
    expect(screen.getByText("这里还没有内容。")).toBeVisible();
  });

  it("makes rows reachable by keyboard and opens them with click, Enter or Space", async () => {
    const user = userEvent.setup();
    const onRowClick = vi.fn();
    const { container } = render(
      <Table
        caption="Apps"
        columns={columns}
        rows={rows}
        onRowClick={onRowClick}
        selectedKey="worker"
        density="compact"
      />,
    );
    const [, apiRow, workerRow] = screen.getAllByRole("row");
    expect(workerRow).toHaveAttribute("aria-current", "true");
    expect(workerRow).toHaveClass("is-selected", "is-clickable");
    expect(apiRow).not.toHaveAttribute("aria-current");
    await user.tab();
    expect(apiRow).toHaveFocus();
    await user.keyboard("{Enter}");
    expect(onRowClick).toHaveBeenLastCalledWith(rows[0]);
    await user.tab();
    expect(workerRow).toHaveFocus();
    await user.keyboard(" ");
    expect(onRowClick).toHaveBeenLastCalledWith(rows[1]);
    if (apiRow) await user.click(apiRow);
    expect(onRowClick).toHaveBeenCalledTimes(3);
    await expectAccessible(container);
  });

  it("leaves clicks and keys on controls inside a row to the control", async () => {
    const user = userEvent.setup();
    const onRowClick = vi.fn();
    const onRestart = vi.fn();
    const withAction: TableColumn<App>[] = [
      ...columns,
      { key: "actions", header: "Actions", render: () => <Button onClick={onRestart}>Restart</Button> },
    ];
    render(<Table caption="Apps" columns={withAction} rows={rows.slice(0, 1)} onRowClick={onRowClick} />);
    const button = screen.getByRole("button", { name: "Restart" });
    await user.click(button);
    button.focus();
    await user.keyboard("{Enter}");
    expect(onRestart).toHaveBeenCalledTimes(2);
    expect(onRowClick).not.toHaveBeenCalled();
  });

  it("asks for a row's menu on right-click and on Shift+F10, but not from a control in the row", async () => {
    const user = userEvent.setup();
    const onRowContextMenu = vi.fn();
    const withAction: TableColumn<App>[] = [
      ...columns,
      { key: "actions", header: "Actions", render: () => <Button onClick={() => {}}>Restart</Button> },
    ];
    const { container } = render(
      <Table caption="Apps" columns={withAction} rows={rows} onRowContextMenu={onRowContextMenu} />,
    );
    const [, apiRow] = screen.getAllByRole("row");
    // A row with a menu is a keyboard stop even with nothing to open, so Shift+F10 can reach it.
    expect(apiRow).toHaveAttribute("tabindex", "0");
    await user.pointer({ keys: "[MouseRight]", target: apiRow as HTMLElement });
    expect(onRowContextMenu).toHaveBeenCalledTimes(1);
    expect(onRowContextMenu.mock.calls[0]?.[0]).toBe(rows[0]);
    expect(onRowContextMenu.mock.calls[0]?.[2]).toBe(apiRow);
    await user.pointer({
      keys: "[MouseRight]",
      target: screen.getAllByRole("button", { name: "Restart" })[0] as HTMLElement,
    });
    expect(onRowContextMenu).toHaveBeenCalledTimes(1);
    (apiRow as HTMLElement).focus();
    await user.keyboard("{Shift>}{F10}{/Shift}");
    expect(onRowContextMenu).toHaveBeenCalledTimes(2);
    await expectAccessible(container);
  });

  it("leaves a row with nothing to open out of clicks and the tab order", async () => {
    const user = userEvent.setup();
    const onRowClick = vi.fn();
    render(
      <Table
        caption="Apps"
        columns={columns}
        rows={rows}
        onRowClick={onRowClick}
        rowClickable={(row) => row !== rows[1]}
      />,
    );
    const [, opens, leadsNowhere] = screen.getAllByRole("row");
    expect(opens).toHaveClass("is-clickable");
    expect(opens).toHaveAttribute("tabindex", "0");
    expect(leadsNowhere).not.toHaveClass("is-clickable");
    expect(leadsNowhere).not.toHaveAttribute("tabindex");
    expect(leadsNowhere).not.toHaveAttribute("aria-selected");
    await user.click(leadsNowhere as HTMLElement);
    (leadsNowhere as HTMLElement).focus();
    await user.keyboard("{Enter}");
    expect(onRowClick).not.toHaveBeenCalled();
    await user.click(opens as HTMLElement);
    expect(onRowClick).toHaveBeenCalledTimes(1);
  });

  it("keys rows by another field, and scrolls past a maximum height in a focusable region", async () => {
    const { container } = render(
      <Table caption="Apps by name" columns={columns} rows={rows} rowKey="name" selectedKey="worker" maxHeight={200} />,
    );
    const region = screen.getByRole("region", { name: "Apps by name" });
    expect(region).toHaveAttribute("tabindex", "0");
    expect(region).toHaveStyle({ maxHeight: "200px" });
    expect(screen.getAllByRole("row")[2]).toHaveAttribute("aria-current", "true");
    await expectAccessible(container);
  });

  it("gives a row its own class, for rows that differ", () => {
    render(
      <Table
        columns={[{ key: "name", header: "Name" }]}
        rows={[
          { id: 1, name: "web" },
          { id: 2, name: "api" },
        ]}
        rowClassName={(row) => (row.id === 1 ? "is-head" : undefined)}
      />,
    );
    expect(screen.getByRole("row", { name: "web" })).toHaveClass("is-head");
    expect(screen.getByRole("row", { name: "api" })).not.toHaveClass("is-head");
  });

  it("keeps a focused row's DOM identity and focus through a refresh, and returns focus to the list when it leaves", async () => {
    const onRowClick = vi.fn();
    const { rerender } = render(
      <section tabIndex={-1} aria-label="Apps list">
        <Table caption="Apps" columns={columns} rows={rows} onRowClick={onRowClick} />
      </section>,
    );
    const workerRow = screen.getByRole("row", { name: /worker/ });
    workerRow.focus();
    expect(workerRow).toHaveFocus();

    // Refresh with updated data and changed order: same DOM element keeps focus.
    const [first, second] = rows;
    if (!first || !second) throw new Error("expected two fixture rows");
    const refreshed: App[] = [
      { ...second, replicas: "2/4", status: "healthy" },
      { ...first, replicas: "3/3" },
    ];
    rerender(
      <section tabIndex={-1} aria-label="Apps list">
        <Table caption="Apps" columns={columns} rows={refreshed} onRowClick={onRowClick} />
      </section>,
    );
    const updatedRow = screen.getByRole("row", { name: /worker/ });
    expect(updatedRow).toBe(workerRow);
    expect(updatedRow).toHaveTextContent("2/4");
    expect(updatedRow).toHaveFocus();

    // Refresh where the focused row disappears: focus returns cleanly to the list.
    rerender(
      <section tabIndex={-1} aria-label="Apps list">
        <Table caption="Apps" columns={columns} rows={refreshed.slice(1)} onRowClick={onRowClick} />
      </section>,
    );
    expect(screen.queryByRole("row", { name: /worker/ })).toBeNull();
    expect(screen.getByLabelText("Apps list")).toHaveFocus();

    // Blurring intentionally to the page background must not let a later refresh steal focus back.
    const apiRow = screen.getByRole("row", { name: /api/ });
    apiRow.focus();
    apiRow.blur();
    await Promise.resolve();
    rerender(
      <section tabIndex={-1} aria-label="Apps list">
        <Table caption="Apps" columns={columns} rows={[{ ...first, replicas: "4/4" }]} onRowClick={onRowClick} />
      </section>,
    );
    expect(apiRow).not.toHaveFocus();
  });

  it("does not make a non-scrolling wrapper click-focusable, cleans up fallback tabindex, and never steals focus from an outside input", async () => {
    const user = userEvent.setup();
    const onRowClick = vi.fn();
    const { container, rerender } = render(
      <>
        <input aria-label="Filter apps" />
        <Table caption="Apps" columns={columns} rows={rows} onRowClick={onRowClick} />
      </>,
    );
    const wrap = container.querySelector(".dw-table-wrap") as HTMLElement;
    expect(wrap).not.toHaveAttribute("tabindex");

    // Clicking a non-interactive table without onRowClick or a cell does not focus the wrapper.
    await user.click(screen.getByText("3/3"));
    expect(wrap).not.toHaveFocus();

    // Moving focus to an outside input before a refresh must keep focus in the input.
    const workerRow = screen.getByRole("row", { name: /worker/ });
    const filter = screen.getByRole("textbox", { name: "Filter apps" });
    workerRow.focus();
    filter.focus();
    rerender(
      <>
        <input aria-label="Filter apps" />
        <Table caption="Apps" columns={columns} rows={rows.slice(0, 1)} onRowClick={onRowClick} />
      </>,
    );
    expect(filter).toHaveFocus();

    // When a focused row disappears with no list section above the table, the wrapper takes focus
    // temporarily and drops its fallback tabindex once focus leaves it.
    const apiRow = screen.getByRole("row", { name: /api/ });
    apiRow.focus();
    rerender(
      <>
        <input aria-label="Filter apps" />
        <Table caption="Apps" columns={columns} rows={[]} onRowClick={onRowClick} />
      </>,
    );
    expect(wrap).toHaveFocus();
    expect(wrap).toHaveAttribute("tabindex", "-1");
    expect(wrap).toHaveAttribute("role", "group");
    expect(wrap).toHaveAttribute("aria-label", "Apps");
    filter.focus();
    expect(wrap).not.toHaveAttribute("tabindex");
    expect(wrap).not.toHaveAttribute("role");
    expect(wrap).not.toHaveAttribute("aria-label");
  });
});
