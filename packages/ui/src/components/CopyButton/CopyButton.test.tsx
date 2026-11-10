// SPDX-License-Identifier: AGPL-3.0-only

import { act, render, screen } from "@testing-library/react";
import userEvent from "@testing-library/user-event";
import { describe, expect, it, vi } from "vitest";

import { expectAccessible } from "../../test/a11y";
import { CopyButton } from "./CopyButton";

describe("CopyButton", () => {
  it("copies its text and says so, then is ready again", async () => {
    vi.useFakeTimers({ shouldAdvanceTime: true });
    const user = userEvent.setup({ advanceTimers: vi.advanceTimersByTime });
    const writeText = vi.spyOn(navigator.clipboard, "writeText").mockResolvedValue();
    const onCopied = vi.fn();
    const { container } = render(
      <CopyButton
        text="dockwell services create web"
        label="Copy as CLI command"
        copiedLabel="Copied"
        onCopied={onCopied}
      />,
    );
    await user.click(screen.getByRole("button", { name: "Copy as CLI command" }));
    expect(writeText).toHaveBeenCalledWith("dockwell services create web");
    expect(onCopied).toHaveBeenCalledOnce();
    expect(screen.getByRole("button", { name: "Copied" })).toBeInTheDocument();
    try {
      await expectAccessible(container);
      act(() => vi.advanceTimersByTime(2100));
      expect(screen.getByRole("button", { name: "Copy as CLI command" })).toBeInTheDocument();
    } finally {
      vi.useRealTimers();
    }
  });

  it("names an icon-only button by what it copies", async () => {
    const user = userEvent.setup();
    vi.spyOn(navigator.clipboard, "writeText").mockResolvedValue();
    render(<CopyButton text="x" label="Copy the token" copiedLabel="Token copied" iconOnly />);
    await user.click(screen.getByRole("button", { name: "Copy the token" }));
    expect(screen.getByRole("button", { name: "Token copied" })).not.toHaveTextContent(/./);
  });

  it("stays as it was when the browser refuses", async () => {
    const user = userEvent.setup();
    vi.spyOn(navigator.clipboard, "writeText").mockRejectedValue(new Error("denied"));
    render(<CopyButton text="x" label="Copy" copiedLabel="Copied" />);
    await user.click(screen.getByRole("button", { name: "Copy" }));
    expect(screen.getByRole("button", { name: "Copy" })).toBeInTheDocument();
  });
});
