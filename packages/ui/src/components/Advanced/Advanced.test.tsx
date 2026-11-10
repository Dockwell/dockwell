// SPDX-License-Identifier: AGPL-3.0-only

import { render, screen } from "@testing-library/react";
import userEvent from "@testing-library/user-event";
import { useState } from "react";
import { describe, expect, it, vi } from "vitest";

import { expectAccessible } from "../../test/a11y";
import { Advanced } from "./Advanced";

describe("Advanced", () => {
  it("is closed at first, and says what its defaults are in a line", async () => {
    const { container } = render(
      <Advanced summary="Restarts unless stopped · no limits">
        <label>
          Command <input />
        </label>
      </Advanced>,
    );
    const toggle = screen.getByRole("button", { name: "Advanced" });
    expect(toggle).toHaveAttribute("aria-expanded", "false");
    expect(toggle).toHaveAccessibleDescription("Restarts unless stopped · no limits");
    expect(screen.queryByLabelText("Command")).toBeNull();
    await expectAccessible(container);
  });

  it("opens to its fields, and leaves the summary out once they're in view", async () => {
    const user = userEvent.setup();
    const onOpenChange = vi.fn();
    const { container } = render(
      <Advanced label="More options" summary="Defaults" onOpenChange={onOpenChange}>
        <label>
          Command <input />
        </label>
      </Advanced>,
    );
    const toggle = screen.getByRole("button", { name: "More options" });
    await user.click(toggle);
    expect(toggle).toHaveAttribute("aria-expanded", "true");
    expect(onOpenChange).toHaveBeenLastCalledWith(true);
    expect(screen.getByLabelText("Command")).toBeInTheDocument();
    expect(screen.queryByText("Defaults")).toBeNull();
    expect(document.getElementById(toggle.getAttribute("aria-controls") ?? "")).toContainElement(
      screen.getByLabelText("Command"),
    );
    await expectAccessible(container);
    await user.click(toggle);
    expect(screen.queryByLabelText("Command")).toBeNull();
  });

  it("stays open where the form keeps it open, as when a field inside was refused", async () => {
    const user = userEvent.setup();
    function Form() {
      const [open, setOpen] = useState(true);
      return (
        <Advanced open={open} onOpenChange={setOpen} badge={<span>1 changed</span>}>
          <label>
            User <input />
          </label>
        </Advanced>
      );
    }
    render(<Form />);
    expect(screen.getByLabelText("User")).toBeInTheDocument();
    expect(screen.getByText("1 changed")).toBeInTheDocument();
    await user.click(screen.getByRole("button", { name: /Advanced/ }));
    expect(screen.queryByLabelText("User")).toBeNull();
    expect(screen.getByText("1 changed")).toBeInTheDocument();
  });
});
