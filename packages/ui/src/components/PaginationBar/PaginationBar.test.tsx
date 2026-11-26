// SPDX-License-Identifier: AGPL-3.0-only

import { render, screen } from "@testing-library/react";
import userEvent from "@testing-library/user-event";
import { describe, expect, it, vi } from "vitest";

import { PaginationBar } from "./PaginationBar";

describe("PaginationBar", () => {
  it("renders summary, disables boundaries, and calls previous and next handlers", async () => {
    const user = userEvent.setup();
    const onPrevious = vi.fn();
    const onNext = vi.fn();
    const { rerender } = render(
      <PaginationBar
        label="App pages"
        summary="1-50 of 67"
        announcement="Page 1"
        hasPrevious={false}
        hasNext={true}
        onPrevious={onPrevious}
        onNext={onNext}
      />,
    );

    expect(screen.getByRole("navigation", { name: "App pages" })).toBeVisible();
    expect(screen.getByText("1-50 of 67")).toBeVisible();
    expect(screen.getByRole("button", { name: "Previous" })).toBeDisabled();
    const next = screen.getByRole("button", { name: "Next" });
    expect(next).toBeEnabled();
    await user.click(next);
    expect(onNext).toHaveBeenCalledTimes(1);

    rerender(
      <PaginationBar
        label="App pages"
        summary="51-67 of 67"
        announcement="Page 2"
        hasPrevious={true}
        hasNext={false}
        onPrevious={onPrevious}
        onNext={onNext}
      />,
    );
    expect(screen.getByRole("button", { name: "Next" })).toBeDisabled();
    await user.click(screen.getByRole("button", { name: "Previous" }));
    expect(onPrevious).toHaveBeenCalledTimes(1);
  });
});
