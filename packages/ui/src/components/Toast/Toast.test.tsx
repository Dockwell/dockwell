// SPDX-License-Identifier: AGPL-3.0-only

import { act, fireEvent, render, screen } from "@testing-library/react";
import userEvent from "@testing-library/user-event";
import { afterEach, beforeEach, describe, expect, it, vi } from "vitest";

import { englishStrings, UIStringsProvider } from "../../lib/strings";
import { expectAccessible } from "../../test/a11y";
import { Toast } from "./Toast";

describe("Toast", () => {
  it("is a polite live region with an undo action and a dismiss button", async () => {
    const user = userEvent.setup();
    const onUndo = vi.fn();
    const onClose = vi.fn();
    const { container } = render(
      <Toast
        title="Rolled back to r127"
        description="api serves r127 on 3 replicas."
        action={{ label: "Undo", icon: "undo", kbd: "⌘Z", onClick: onUndo }}
        onClose={onClose}
      />,
    );
    const toast = screen.getByRole("status");
    expect(toast).toHaveClass("dw-toast", "dw-tone-healthy");
    expect(toast).toHaveTextContent("Rolled back to r127api serves r127 on 3 replicas.");
    await user.click(screen.getByRole("button", { name: /^Undo/ }));
    expect(onUndo).toHaveBeenCalledOnce();
    await user.click(screen.getByRole("button", { name: "Dismiss" }));
    expect(onClose).toHaveBeenCalledOnce();
    expect(container.querySelector(".dw-toast-timer")).toBeNull();
    await expectAccessible(container);
  });

  it("alerts for critical, with the critical shape, and stays without a duration", async () => {
    const { container } = render(<Toast tone="critical" title="Deploy failed" />);
    expect(screen.getByRole("alert")).toHaveClass("dw-tone-critical");
    expect(container.querySelector(".dw-toast-glyph rect")).not.toBeNull();
    expect(screen.queryByRole("button")).toBeNull();
    await expectAccessible(container);
  });

  it("names the dismiss button in the app's language", () => {
    render(
      <UIStringsProvider strings={{ ...englishStrings, dismiss: "关闭通知" }}>
        <Toast title="已回滚" onClose={() => {}} />
      </UIStringsProvider>,
    );
    expect(screen.getByRole("button", { name: "关闭通知" })).toBeVisible();
  });
});

describe("Toast timer", () => {
  beforeEach(() => {
    vi.useFakeTimers();
  });
  afterEach(() => {
    vi.useRealTimers();
  });

  it("asks to close when the duration runs out, drawing the timer", () => {
    const onClose = vi.fn();
    const { container } = render(<Toast title="Saved" duration={6000} onClose={onClose} />);
    expect(container.querySelector(".dw-toast-timer")).toHaveAttribute("aria-hidden", "true");
    expect(screen.getByRole("status").getAttribute("style")).toContain("--dw-toast-d: 6000ms");
    act(() => vi.advanceTimersByTime(5999));
    expect(onClose).not.toHaveBeenCalled();
    act(() => vi.advanceTimersByTime(1));
    expect(onClose).toHaveBeenCalledOnce();
  });

  it("pauses while hovered or focused, and resumes with the time that was left", () => {
    const onClose = vi.fn();
    render(<Toast title="Saved" duration={6000} onClose={onClose} action={{ label: "Undo" }} />);
    const toast = screen.getByRole("status");
    act(() => vi.advanceTimersByTime(4000));
    fireEvent.mouseEnter(toast);
    act(() => vi.advanceTimersByTime(10_000));
    expect(onClose).not.toHaveBeenCalled();
    fireEvent.mouseLeave(toast);
    act(() => vi.advanceTimersByTime(1500));
    expect(onClose).not.toHaveBeenCalled();

    act(() => screen.getByRole("button", { name: "Undo" }).focus());
    act(() => vi.advanceTimersByTime(10_000));
    expect(onClose).not.toHaveBeenCalled();
    act(() => screen.getByRole("button", { name: "Undo" }).blur());
    act(() => vi.advanceTimersByTime(500));
    expect(onClose).toHaveBeenCalledOnce();
  });

  it("asks to close once, however often it's hovered or focused after", () => {
    const onClose = vi.fn();
    render(<Toast title="Saved" duration={6000} onClose={onClose} action={{ label: "Undo" }} />);
    const toast = screen.getByRole("status");
    act(() => vi.advanceTimersByTime(6000));
    expect(onClose).toHaveBeenCalledOnce();
    // The caller hasn't removed it yet.
    for (let i = 0; i < 3; i += 1) {
      fireEvent.mouseEnter(toast);
      fireEvent.mouseLeave(toast);
      act(() => vi.advanceTimersByTime(100));
    }
    act(() => screen.getByRole("button", { name: "Undo" }).focus());
    act(() => screen.getByRole("button", { name: "Undo" }).blur());
    act(() => vi.advanceTimersByTime(10_000));
    expect(onClose).toHaveBeenCalledOnce();
  });

  it("can't dismiss itself without onClose, so draws no timer", () => {
    const { container } = render(<Toast title="Saved" duration={6000} />);
    expect(container.querySelector(".dw-toast-timer")).toBeNull();
    act(() => vi.advanceTimersByTime(10_000));
    expect(screen.getByRole("status")).toBeVisible();
  });
});
