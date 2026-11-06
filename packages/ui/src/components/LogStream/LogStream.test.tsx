// SPDX-License-Identifier: AGPL-3.0-only

import { act, fireEvent, render, screen } from "@testing-library/react";
import userEvent from "@testing-library/user-event";
import { createRef } from "react";
import { describe, expect, it, vi } from "vitest";

import { englishStrings, UIStringsProvider } from "../../lib/strings";
import { expectAccessible } from "../../test/a11y";
import { type LogLine, type LogLines, LogStream, type LogStreamHandle, prettyJson } from "./LogStream";

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
    expect(screen.getByRole("status")).toHaveTextContent("Following");
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
    expect(screen.getByRole("status")).toHaveTextContent("Following");
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
            manyNewLines: "超过 1 万行新日志",
            showAll: "显示整行",
            expand: "展开",
            collapse: "收起",
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

  it("reads a stack trace as one thing: one glyph, and each time once", () => {
    const { container } = render(
      <LogStream
        lines={[
          { id: 1, time: "01:19:18.888", at: 1000, source: "hz-1", level: "error", text: "ERROR migration failed" },
          { id: 2, time: "01:19:18.888", at: 1000, source: "hz-1", level: "error", text: "    at a (x.js:1)" },
          { id: 3, time: "01:19:18.900", at: 1012, source: "hz-1", level: "error", text: "    at b (x.js:2)" },
          { id: 4, time: "01:19:20.000", at: 2100, source: "hz-1", level: "error", text: "ERROR another" },
          { id: 5, time: "01:19:20.000", at: 2100, source: "hz-2", level: "error", text: "ERROR elsewhere" },
        ]}
      />,
    );
    const rows = Array.from(container.querySelectorAll(".dw-log-line"));
    expect(rows.map((r) => r.querySelectorAll(".dw-log-lvl svg").length)).toEqual([1, 0, 0, 1, 1]);
    expect(rows.map((r) => r.classList.contains("is-carried"))).toEqual([false, true, true, false, false]);
    // A time the line above has is there for a reader who points at it, and for copying.
    const times = rows.map((r) => r.querySelector(".dw-log-time"));
    expect(times[0]).not.toHaveClass("is-repeat");
    expect(times[1]).toHaveClass("is-repeat");
    expect(times[1]).toHaveTextContent("01:19:18.888");
    expect(times[2]).not.toHaveClass("is-repeat");
    expect(times[4]).not.toHaveClass("is-repeat");
  });

  it("says the day where it changes", () => {
    const { container } = render(
      <LogStream
        lines={[
          { id: 1, time: "23:59:59.000", source: "a", text: "late", day: "Wed 4 Nov" },
          { id: 2, time: "23:59:59.500", source: "a", text: "later", day: "Wed 4 Nov" },
          { id: 3, time: "00:00:01.000", source: "a", text: "early", day: "Thu 5 Nov" },
        ]}
      />,
    );
    expect(Array.from(container.querySelectorAll(".dw-log-day")).map((d) => d.textContent)).toEqual([
      "Wed 4 Nov",
      "Thu 5 Nov",
    ]);
  });

  /** Chooses text from one node to another, as a reader does. */
  function choose(from: Node, to: Node) {
    const selection = document.getSelection() as Selection;
    selection.removeAllRanges();
    const range = document.createRange();
    range.setStartBefore(from);
    range.setEndAfter(to);
    selection.addRange(range);
    document.dispatchEvent(new Event("selectionchange"));
  }

  it("copies chosen lines as lines, with their time and source", () => {
    const { container } = render(<LogStream lines={lines} />);
    const rows = container.querySelectorAll(".dw-log-line");
    choose(rows[0] as Node, rows[1] as Node);
    const setData = vi.fn();
    fireEvent.copy(screen.getByRole("log"), { clipboardData: { setData } });
    expect(setData).toHaveBeenCalledWith(
      "text/plain",
      "14:16:02.118 api-7f9c-1 GET /healthz 200 2 ms\n14:16:02.541 api-7f9c-2 slow query: 812 ms",
    );
    document.getSelection()?.removeAllRanges();
  });

  it("leaves copying part of one line to the browser", () => {
    const { container } = render(<LogStream lines={lines} />);
    const text = container.querySelector(".dw-log-text") as Element;
    choose(text.firstChild as Node, text.firstChild as Node);
    const setData = vi.fn();
    fireEvent.copy(screen.getByRole("log"), { clipboardData: { setData } });
    expect(setData).not.toHaveBeenCalled();
    document.getSelection()?.removeAllRanges();
  });

  it("holds still while text is being chosen, and goes on if none was", () => {
    const onFollowChange = vi.fn();
    const { container, rerender } = render(<LogStream lines={lines} live follow onFollowChange={onFollowChange} />);
    const row = container.querySelector(".dw-log-text") as Element;
    fireEvent.pointerDown(row, { button: 0, pointerType: "mouse" });
    expect(onFollowChange).toHaveBeenLastCalledWith(false);
    rerender(<LogStream lines={lines} live follow={false} onFollowChange={onFollowChange} />);
    // A click that chose nothing asked for nothing.
    fireEvent.pointerUp(window);
    expect(onFollowChange).toHaveBeenLastCalledWith(true);

    onFollowChange.mockClear();
    rerender(<LogStream lines={lines} live follow onFollowChange={onFollowChange} />);
    fireEvent.pointerDown(row, { button: 0, pointerType: "mouse" });
    rerender(<LogStream lines={lines} live follow={false} onFollowChange={onFollowChange} />);
    choose(row, row);
    fireEvent.pointerUp(window);
    expect(onFollowChange).not.toHaveBeenCalledWith(true);
    document.getSelection()?.removeAllRanges();
  });

  it("follows again on scrolling back to the end", () => {
    const onFollowChange = vi.fn();
    render(<LogStream lines={lines} live follow={false} onFollowChange={onFollowChange} height={100} />);
    const log = screen.getByRole("log");
    const scroll = fakeScroll(log, { scrollHeight: 1000, clientHeight: 100 });
    scroll.scrollTo(400);
    expect(onFollowChange).not.toHaveBeenCalled();
    scroll.scrollTo(900);
    expect(onFollowChange).toHaveBeenCalledWith(true);
  });

  describe("keeps a paused reader's place through a filter", () => {
    const ROW = 19;
    const all: LogLine[] = Array.from({ length: 1000 }, (_, i) => ({
      id: i,
      time: "t",
      source: "a",
      text: `line ${i}`,
    }));
    let epoch = 0;
    /** What a store offers: the lines a filter keeps, each by its ID. */
    const showing = (keeps: (id: number) => boolean): LogLines => {
      const kept = all.filter((l) => keeps(l.id as number));
      epoch++;
      return {
        length: kept.length,
        start: 0,
        version: epoch,
        epoch,
        at: (p) => kept[p],
        positionOf: (id) => {
          const p = kept.findIndex((l) => l.id === id);
          return p < 0 ? undefined : p;
        },
        positionNear: (id) => {
          if (kept.length === 0) return undefined;
          const p = kept.findIndex((l) => (l.id as number) >= (id as number));
          return p < 0 ? kept.length - 1 : p;
        },
      };
    };
    const atLine500 = (log: HTMLElement) => {
      const scroll = fakeScroll(log, { scrollHeight: 19_000, clientHeight: 240 });
      scroll.scrollTo(500 * ROW + 5);
      return scroll;
    };
    const paused = (lines: LogLines) => <LogStream lines={lines} follow={false} live />;

    it("through a search that found nothing", () => {
      const { rerender } = render(paused(showing(() => true)));
      const scroll = atLine500(screen.getByRole("log"));
      rerender(paused(showing(() => false)));
      scroll.scrollTo(0); // the browser's, as what there is to scroll has gone
      rerender(paused(showing(() => true)));
      expect(scroll.top).toBe(500 * ROW + 5);
    });

    it("through a search that found nothing, typed a key at a time", () => {
      const { rerender } = render(paused(showing(() => true)));
      const scroll = atLine500(screen.getByRole("log"));
      rerender(paused(showing((id) => id % 100 === 7)));
      rerender(paused(showing(() => false)));
      scroll.scrollTo(0);
      rerender(paused(showing(() => true)));
      expect(scroll.top).toBe(500 * ROW + 5);
    });

    it("through a search that found other lines, going to the nearest", () => {
      const { rerender } = render(paused(showing(() => true)));
      const scroll = atLine500(screen.getByRole("log"));
      rerender(paused(showing((id) => id % 100 === 7)));
      expect(scroll.top).toBe(5 * ROW); // line 507, the sixth match
      rerender(paused(showing((id) => id % 50 === 7)));
      expect(scroll.top).toBe(10 * ROW); // still by the line a reader is on
      rerender(paused(showing(() => true)));
      expect(scroll.top).toBe(500 * ROW + 5);
    });

    it("going to the last that shows when nothing comes after", () => {
      const { rerender } = render(paused(showing(() => true)));
      const scroll = atLine500(screen.getByRole("log"));
      rerender(paused(showing((id) => id < 10)));
      expect(scroll.top).toBe(9 * ROW);
      rerender(paused(showing(() => true)));
      expect(scroll.top).toBe(500 * ROW + 5);
    });

    it("staying where a reader went within it", () => {
      const { rerender } = render(paused(showing(() => true)));
      const scroll = atLine500(screen.getByRole("log"));
      rerender(paused(showing((id) => id % 100 === 7)));
      vi.spyOn(performance, "now").mockReturnValue(performance.now() + 1000); // past the browser's own scrolling
      scroll.scrollTo(2 * ROW); // line 207
      rerender(paused(showing(() => true)));
      expect(scroll.top).toBe(207 * ROW);
      vi.restoreAllMocks();
    });

    it("staying at what a step went to", () => {
      const ref = createRef<LogStreamHandle>();
      const page = (lines: LogLines) => <LogStream ref={ref} lines={lines} follow={false} live />;
      const { rerender } = render(page(showing(() => true)));
      const scroll = atLine500(screen.getByRole("log"));
      rerender(page(showing((id) => id % 100 === 7)));
      act(() => ref.current?.show(3)); // line 307, with the matches before it above it
      expect(scroll.top).toBe(0);
      rerender(page(showing(() => true)));
      expect(scroll.top).toBe(304 * ROW); // as far down what shows as it was
    });
  });

  it("says a lot of new lines without a number that can't be read", () => {
    const many = (arrived: number) => ({
      length: 3,
      start: 0,
      version: arrived,
      epoch: 0,
      arrived,
      at: (p: number) => lines[p],
    });
    const { rerender } = render(<LogStream lines={many(12)} live follow={false} />);
    expect(screen.getByRole("button", { name: "12 new lines" })).toBeVisible();
    rerender(<LogStream lines={many(265_653)} live follow={false} />);
    expect(screen.getByRole("button", { name: "10k+ new lines" })).toBeVisible();
  });

  it("cuts a long line with an ellipsis, and shows the rest on asking", async () => {
    const long = "x".repeat(1500);
    const { container } = render(<LogStream lines={[{ id: 1, time: "t", source: "a", text: long }]} />);
    expect(container.querySelector(".dw-log-text")?.textContent).toContain("…");
    await userEvent.setup().click(screen.getByRole("button", { name: "Show all of this line" }));
    expect(container.querySelector(".dw-log-text")?.textContent).toContain(long);
  });

  it("sets out a JSON line over several, what it says first", async () => {
    const json = JSON.stringify({ level: "error", time: "now", msg: "startup failed", err: { code: "2BP01" } });
    const { container } = render(<LogStream lines={[{ id: 1, time: "t", source: "a", text: json }]} />);
    const user = userEvent.setup();
    await user.click(screen.getByRole("button", { name: "Set out" }));
    const pretty = container.querySelector(".dw-log-pretty");
    expect(pretty?.textContent?.startsWith("msg: startup failed\nlevel: error")).toBe(true);
    expect(pretty?.textContent).toContain('"code": "2BP01"');
    await user.click(screen.getByRole("button", { name: "Put back" }));
    expect(container.querySelector(".dw-log-pretty")).toBeNull();
  });

  it("offers nothing for what only looks like JSON", () => {
    expect(prettyJson("{not json}")).toBeUndefined();
    expect(prettyJson("[1]")).toBeUndefined();
  });

  it("marks the line a reader is on, and its match", () => {
    const { container } = render(<LogStream lines={lines} current={2} highlight="slow" />);
    expect(container.querySelector(".dw-log-line.is-current")).toHaveTextContent("slow query");
    expect(container.querySelector("mark.is-current")).toHaveTextContent("slow");
  });

  it("keeps a line's actions under the keyboard, and the focus in the log when the line goes", () => {
    const actions = () => <button type="button">Copy this line</button>;
    const log = (shown: LogLine[], current?: number) => (
      <LogStream title="Logs" lines={shown} current={current} actions={actions} />
    );
    const { rerender } = render(log(lines, 3));
    const button = screen.getByRole("button", { name: "Copy this line" });
    act(() => button.focus());

    // It is no longer the line being read, but the keyboard is still on it.
    rerender(log(lines));
    expect(screen.getByRole("button", { name: "Copy this line" })).toHaveFocus();

    rerender(log(lines.slice(0, 2)));
    expect(screen.queryByRole("button", { name: "Copy this line" })).toBeNull();
    expect(screen.getByRole("log")).toHaveFocus();
  });

  it("brings a line into view, and stops following", () => {
    const onFollowChange = vi.fn();
    const handle = createRef<LogStreamHandle>();
    render(<LogStream ref={handle} lines={lines} live follow onFollowChange={onFollowChange} height={100} />);
    act(() => handle.current?.show(1));
    expect(onFollowChange).toHaveBeenLastCalledWith(false);
  });

  it("holds a place for Follow, and for what is under the log, whatever they say", () => {
    const { container, rerender } = render(<LogStream lines={lines} holdFollow foot="" />);
    const button = container.querySelector(".dw-log-tools .is-held");
    expect(button).not.toBeNull();
    expect(container.querySelector(".dw-log-foot")).not.toBeNull();
    rerender(<LogStream lines={lines} live holdFollow foot="" />);
    expect(screen.getByRole("button", { name: "Pause" })).toBeVisible();
    // The word that isn't showing holds the wider of the two.
    expect(container.querySelector(".dw-live .dw-held-other")).toHaveTextContent("Paused");
    expect(screen.getByRole("status")).toHaveTextContent("Following");
  });
});
