// SPDX-License-Identifier: AGPL-3.0-only

import { fireEvent, render, screen } from "@testing-library/react";
import userEvent from "@testing-library/user-event";
import { describe, expect, it, vi } from "vitest";

import { englishStrings, UIStringsProvider } from "../../lib/strings";
import { expectAccessible } from "../../test/a11y";
import { type LogLine, LogStream } from "./LogStream";

const lines: LogLine[] = [
  { id: 1, time: "14:16:02.118", source: "api-7f9c-1", text: "GET /healthz 200 2 ms" },
  { id: 2, time: "14:16:02.541", source: "api-7f9c-2", level: "warn", text: "slow query: 812 ms" },
  { id: 3, time: "14:16:03.007", source: "api-7f9c-1", level: "error", text: "connection reset by peer" },
];

/** jsdom has no layout, so give the log a size and a scroll position it can report. */
function fakeScroll(el: HTMLElement, { scrollHeight, clientHeight }: { scrollHeight: number; clientHeight: number }) {
  let top = 0;
  Object.defineProperty(el, "scrollHeight", { configurable: true, get: () => scrollHeight });
  Object.defineProperty(el, "clientHeight", { configurable: true, get: () => clientHeight });
  Object.defineProperty(el, "scrollTop", {
    configurable: true,
    get: () => top,
    set: (v: number) => {
      top = v;
    },
  });
  return {
    get top() {
      return top;
    },
    scrollTo(v: number) {
      top = v;
      fireEvent.scroll(el);
    },
  };
}

describe("LogStream", () => {
  it("is a log named by its title, with a live indicator", async () => {
    const { container } = render(<LogStream title="api · production" lines={lines} live height={300} />);
    const log = screen.getByRole("log", { name: "api · production" });
    // Thousands of lines a second must not reach a screen reader one by one.
    expect(log).toHaveAttribute("aria-live", "off");
    expect(log).toHaveAttribute("tabindex", "0");
    expect(log).toHaveStyle({ height: "300px" });
    expect(screen.getByRole("status")).toHaveTextContent("Live");
    expect(screen.getByRole("button", { name: "Pause" })).toHaveAttribute("aria-controls", log.id);
    await expectAccessible(container);
  });

  it("marks warnings and errors with a shape and a word, never colour alone", () => {
    const { container } = render(<LogStream lines={lines} />);
    const [info, warn, error] = Array.from(container.querySelectorAll(".dw-log-line"));
    expect(info?.querySelector("svg")).toBeNull();
    expect(info?.textContent).toBe("14:16:02.118api-7f9c-1GET /healthz 200 2 ms");
    expect(warn).toHaveClass("is-warn");
    expect(warn?.querySelector("svg")).not.toBeNull();
    expect(warn?.querySelector(".dw-log-lvl-word")).toHaveTextContent("Warning");
    expect(error).toHaveClass("is-error");
    expect(error?.querySelector(".dw-log-lvl-word")).toHaveTextContent("Error");
    expect(screen.queryByRole("status")).toBeNull();
  });

  it("follows new lines, and pauses when the reader scrolls up", () => {
    const onFollowChange = vi.fn();
    const { rerender } = render(<LogStream title="Logs" lines={lines} live onFollowChange={onFollowChange} />);
    const log = screen.getByRole("log");
    const scroll = fakeScroll(log, { scrollHeight: 1000, clientHeight: 240 });
    const more = [...lines, { id: 4, time: "14:16:04.000", source: "api-7f9c-2", text: "GET / 200" }];
    rerender(<LogStream title="Logs" lines={more} live onFollowChange={onFollowChange} />);
    expect(scroll.top).toBe(1000);

    // Still at the end: keeps following.
    scroll.scrollTo(760);
    expect(onFollowChange).not.toHaveBeenCalled();

    scroll.scrollTo(400);
    expect(onFollowChange).toHaveBeenCalledWith(false);
    expect(screen.getByRole("status")).toHaveTextContent("Paused");
    expect(screen.getByRole("button", { name: "Follow" })).toBeVisible();

    rerender(<LogStream title="Logs" lines={[...more, { id: 5, time: "x", source: "y", text: "z" }]} live />);
    expect(scroll.top).toBe(400);
  });

  it("resumes following from the button, jumping to the newest line", async () => {
    const user = userEvent.setup();
    render(<LogStream lines={lines} live follow={false} />);
    const log = screen.getByRole("log");
    expect(screen.getByRole("status")).toHaveTextContent("Paused");
    const scroll = fakeScroll(log, { scrollHeight: 800, clientHeight: 240 });
    await user.click(screen.getByRole("button", { name: "Follow" }));
    // Controlled: the parent hasn't changed follow yet.
    expect(screen.getByRole("status")).toHaveTextContent("Paused");
    expect(scroll.top).toBe(0);
  });

  it("toggles following on its own when uncontrolled", async () => {
    const user = userEvent.setup();
    render(<LogStream lines={lines} live />);
    const log = screen.getByRole("log");
    const scroll = fakeScroll(log, { scrollHeight: 800, clientHeight: 240 });
    await user.click(screen.getByRole("button", { name: "Pause" }));
    expect(screen.getByRole("status")).toHaveTextContent("Paused");
    await user.click(screen.getByRole("button", { name: "Follow" }));
    expect(screen.getByRole("status")).toHaveTextContent("Live");
    expect(scroll.top).toBe(800);
  });

  it("draws only what shows, and places it by what comes before", () => {
    const many: LogLine[] = Array.from({ length: 100_000 }, (_, i) => ({
      id: i,
      time: "14:16:02.118",
      source: "a",
      text: `line ${i}`,
    }));
    const { container } = render(<LogStream lines={many} height={300} />);
    // No layout here says how many fit, so a bounded number is drawn.
    const drawn = container.querySelectorAll(".dw-log-line");
    expect(drawn.length).toBeLessThanOrEqual(200);
    expect(drawn.length).toBeGreaterThan(0);
  });

  it("leaves out the source column on request", () => {
    const { container } = render(<LogStream lines={lines} showSource={false} />);
    expect(container.querySelector(".dw-log-src")).toBeNull();
    expect(container.querySelector(".dw-log")).toHaveClass("no-source");
    expect(screen.getByText("GET /healthz 200 2 ms")).toBeVisible();
  });

  it("draws colours, never codes, and markup as text", () => {
    const { container } = render(
      <LogStream
        lines={[
          { id: 1, time: "t", source: "s", text: "\u001b[31mred\u001b[0m plain" },
          { id: 2, time: "t", source: "s", text: '<img src=x onerror="alert(1)">' },
          { id: 3, time: "t", source: "s", text: "\u001b[2Jwiped \u0000nul" },
        ]}
      />,
    );
    expect(container.querySelector(".dw-ansi-red")).toHaveTextContent("red");
    expect(container.textContent).not.toContain("\u001b");
    expect(container.querySelector("img")).toBeNull();
    expect(screen.getByText('<img src=x onerror="alert(1)">')).toBeVisible();
    expect(container.textContent).toContain("wiped \ufffdnul");
  });

  it("marks what a search found, across colours and in any case", () => {
    const { container } = render(
      <LogStream
        lines={[{ id: 1, time: "t", source: "s", text: "GET \u001b[31m/Car\u001b[32mt/items\u001b[0m and /CART" }]}
        highlight="cart"
      />,
    );
    const marks = [...container.querySelectorAll("mark")].map((m) => m.textContent);
    expect(marks).toEqual(["Car", "t", "CART"]);
    expect(container.querySelector(".dw-log-text")).toHaveTextContent("GET /Cart/items and /CART");
  });

  it("marks a line cut short, and draws a very long line once asked", async () => {
    const user = userEvent.setup();
    const { container } = render(
      <LogStream
        lines={[
          { id: 1, time: "t", source: "s", text: "x".repeat(10_000) },
          { id: 2, time: "t", source: "s", text: "short", cut: "12 KB" },
        ]}
      />,
    );
    expect(screen.getByText("12 KB more not kept")).toBeVisible();
    const first = container.querySelector(".dw-log-text");
    expect(first?.textContent?.length).toBeLessThan(2000);
    await user.click(screen.getByRole("button", { name: "Show all of this line" }));
    expect(container.querySelector(".dw-log-text")?.textContent?.length).toBeGreaterThanOrEqual(10_000);
  });

  it("draws a note in the stream apart from its lines", async () => {
    const { container } = render(
      <LogStream lines={[{ id: 1, time: "", source: "", text: "1,204 lines not shown", notice: "warning" }]} />,
    );
    const note = container.querySelector(".dw-log-notice");
    expect(note).toHaveTextContent("1,204 lines not shown");
    expect(note?.querySelector("svg")).not.toBeNull();
    await expectAccessible(container);
  });

  it("offers the new lines while paused, and End goes back to them", async () => {
    const user = userEvent.setup();
    const onFollowChange = vi.fn();
    const { rerender } = render(<LogStream lines={lines} live follow={false} onFollowChange={onFollowChange} />);
    expect(screen.queryByRole("button", { name: /new line/ })).toBeNull();
    rerender(
      <LogStream
        lines={[...lines, { id: 4, time: "t", source: "s", text: "next" }]}
        live
        follow={false}
        onFollowChange={onFollowChange}
      />,
    );
    const jump = screen.getByRole("button", { name: "1 new line" });
    await user.click(jump);
    expect(onFollowChange).toHaveBeenCalledWith(true);
    onFollowChange.mockClear();
    screen.getByRole("log").focus();
    await user.keyboard("{End}");
    expect(onFollowChange).toHaveBeenCalledWith(true);
  });

  it("stops following on the way up by key or wheel, before a new line can pull it back", async () => {
    const user = userEvent.setup();
    const onFollowChange = vi.fn();
    render(<LogStream lines={lines} live onFollowChange={onFollowChange} />);
    screen.getByRole("log").focus();
    await user.keyboard("{PageUp}");
    expect(onFollowChange).toHaveBeenCalledWith(false);
    onFollowChange.mockClear();

    render(<LogStream lines={lines} live onFollowChange={onFollowChange} />);
    fireEvent.wheel(screen.getAllByRole("log")[1] as HTMLElement, { deltaY: -40 });
    expect(onFollowChange).toHaveBeenCalledWith(false);
  });

  it("shows what stands in for lines while there are none", () => {
    render(<LogStream lines={[]} empty="No logs yet." />);
    expect(screen.getByText("No logs yet.")).toBeVisible();
  });

  it("speaks the app's language", () => {
    render(
      <UIStringsProvider
        strings={{
          ...englishStrings,
          logStream: {
            live: "实时",
            paused: "已暂停",
            follow: "跟随",
            pause: "暂停",
            level: { warn: "警告", error: "错误" },
            newLines: (count) => `${count} 行新日志`,
            showAll: "显示整行",
            cut: (size) => `另有 ${size} 未保留`,
          },
        }}
      >
        <LogStream lines={lines} live />
      </UIStringsProvider>,
    );
    expect(screen.getByRole("status")).toHaveTextContent("实时");
    expect(screen.getByRole("button", { name: "暂停" })).toBeVisible();
    expect(screen.getByText("警告")).toHaveClass("dw-log-lvl-word");
  });
});
