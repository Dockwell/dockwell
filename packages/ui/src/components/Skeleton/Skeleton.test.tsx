// SPDX-License-Identifier: AGPL-3.0-only

import { act, render, screen } from "@testing-library/react";
import { afterEach, beforeEach, describe, expect, it, vi } from "vitest";

import { expectAccessible } from "../../test/a11y";
import { Table, type TableColumn } from "../Table/Table";
import { Loading, Skeleton, SkeletonCard, SkeletonFacts, SkeletonRows, SkeletonTabs } from "./Skeleton";

describe("Skeleton", () => {
  it("draws a shape that assistive technology passes over", () => {
    const { container } = render(<Skeleton text="title" width={240} />);
    const shape = container.querySelector(".dw-skeleton");
    expect(shape).toHaveAttribute("aria-hidden", "true");
    expect(shape).toHaveClass("is-line", "is-text-title");
    expect(shape).toHaveStyle({ width: "240px" });
  });

  it("draws a circle at its size and a pill at a badge's", () => {
    const { container } = render(
      <>
        <Skeleton shape="circle" size={12} />
        <Skeleton shape="pill" />
      </>,
    );
    expect(container.querySelector(".is-circle")).toHaveStyle({ width: "12px", height: "12px" });
    expect(container.querySelector(".is-pill")).not.toHaveClass("is-text-body");
  });
});

describe("Loading", () => {
  beforeEach(() => {
    vi.useFakeTimers();
  });
  afterEach(() => {
    vi.useRealTimers();
  });

  it("is busy, says its label once after the shapes' delay, and never shows it", () => {
    render(
      <Loading label="Loading images">
        <SkeletonRows rows={2} />
      </Loading>,
    );
    const status = document.querySelector(".dw-loading-said");
    expect(status).toHaveAttribute("aria-live", "polite");
    expect(status).toBeEmptyDOMElement();
    act(() => vi.advanceTimersByTime(150));
    expect(status).toHaveTextContent("Loading images");
    expect(status).toHaveClass("dw-sr");
    // One announcement, never a status of the page's own.
    expect(screen.queryByRole("status")).toBeNull();
    expect(document.querySelector(".dw-loading")).toHaveAttribute("aria-busy", "true");
  });

  it("says what a long wait is, in place, only once it runs long", () => {
    render(
      <Loading label="Loading images" slow="Docker is measuring disk use on this server">
        <SkeletonRows />
      </Loading>,
    );
    expect(screen.queryByText("Docker is measuring disk use on this server")).toBeNull();
    act(() => vi.advanceTimersByTime(2900));
    expect(screen.queryByText("Docker is measuring disk use on this server")).toBeNull();
    act(() => vi.advanceTimersByTime(200));
    expect(screen.getByText("Docker is measuring disk use on this server")).toHaveClass("dw-loading-slow");
  });

  it("holds every shape and passes axe", async () => {
    vi.useRealTimers();
    const { container } = render(
      <Loading label="Loading the app">
        <SkeletonTabs />
        <SkeletonCard title="Facts">
          <SkeletonFacts count={3} />
        </SkeletonCard>
        <SkeletonCard lines={2} />
        <SkeletonRows lead="icon" end="pill" />
      </Loading>,
    );
    expect(container.querySelectorAll(".dw-skeleton-fact")).toHaveLength(3);
    expect(container.querySelectorAll(".dw-skeleton-row")).toHaveLength(3);
    expect(container.querySelectorAll(".dw-skeleton-tab")).toHaveLength(4);
    await expectAccessible(container);
  });
});

interface Image {
  id: string;
  tag: string;
  status: string;
  size: string;
}

const columns: TableColumn<Image>[] = [
  { key: "tag", header: "Image", mono: true },
  { key: "status", header: "Status", skeleton: "pill" },
  { key: "size", header: "Size", align: "right" },
  { key: "menu", header: <span className="dw-sr">Actions</span>, align: "right", render: () => null },
];

describe("Table while loading", () => {
  beforeEach(() => {
    vi.useFakeTimers();
  });
  afterEach(() => {
    vi.useRealTimers();
  });

  it("draws its rows' shapes under its real header, never a sentence", () => {
    const { container } = render(<Table columns={columns} rows={[]} loading loadingRows={4} caption="Images" />);
    // The real header, drawn for the eye; assistive technology hears the label until the rows land.
    expect(Array.from(container.querySelectorAll("th")).map((th) => th.textContent)).toEqual([
      "Image",
      "Status",
      "Size",
      "Actions",
    ]);
    expect(container.querySelector("table")).toHaveAttribute("aria-hidden", "true");
    const rows = container.querySelectorAll("tbody tr.dw-table-skeleton-row");
    expect(rows).toHaveLength(4);
    expect(container.querySelector(".dw-table-wrap")).toHaveAttribute("aria-busy", "true");
    expect(screen.queryByText("Nothing here yet.")).toBeNull();
    const first = rows[0]?.querySelectorAll("td") ?? [];
    // The first column is a name, a status a pill, a number at the right, a header-less column an action.
    expect(first[0]?.querySelector(".dw-skeleton.is-line")).not.toBeNull();
    expect(first[1]?.querySelector(".dw-skeleton.is-pill")).not.toBeNull();
    expect(first[2]).toHaveClass("is-num");
    expect(first[3]?.querySelector(".dw-table-skeleton-action")).not.toBeNull();
    act(() => vi.advanceTimersByTime(150));
    expect(document.querySelector(".dw-loading-said")).toHaveTextContent("Loading");
  });

  it("keeps the rows it has while they load again", () => {
    const { container } = render(
      <Table columns={columns} rows={[{ id: "1", tag: "redis:7", status: "Used", size: "40 MB" }]} loading />,
    );
    expect(screen.getByText("redis:7")).toBeInTheDocument();
    expect(container.querySelector(".dw-table-skeleton-row")).toBeNull();
    expect(container.querySelector(".dw-table-wrap")).not.toHaveAttribute("aria-busy");
  });

  it("says what a long wait is under its shapes", () => {
    render(
      <Table
        columns={columns}
        rows={[]}
        loading
        loadingLabel="Loading images"
        slow="Docker is measuring disk use on this server"
      />,
    );
    expect(screen.queryByText("Docker is measuring disk use on this server")).toBeNull();
    act(() => vi.advanceTimersByTime(3000));
    expect(screen.getByText("Docker is measuring disk use on this server")).toBeInTheDocument();
    expect(document.querySelector(".dw-loading-said")).toHaveTextContent("Loading images");
  });

  it("passes axe while loading", async () => {
    vi.useRealTimers();
    const { container } = render(<Table columns={columns} rows={[]} loading caption="Images" />);
    await expectAccessible(container);
  });
});
