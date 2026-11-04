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
    expect(log).toHaveAttribute("aria-live", "polite");
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
    expect(warn?.querySelector(".dw-sr")).toHaveTextContent("Warning");
    expect(error).toHaveClass("is-error");
    expect(error?.querySelector(".dw-sr")).toHaveTextContent("Error");
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
    expect(log).toHaveAttribute("aria-live", "off");
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
          },
        }}
      >
        <LogStream lines={lines} live />
      </UIStringsProvider>,
    );
    expect(screen.getByRole("status")).toHaveTextContent("实时");
    expect(screen.getByRole("button", { name: "暂停" })).toBeVisible();
    expect(screen.getByText("警告")).toHaveClass("dw-sr");
  });
});
