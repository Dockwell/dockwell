// SPDX-License-Identifier: AGPL-3.0-only

import { render, screen } from "@testing-library/react";
import userEvent from "@testing-library/user-event";
import { useState } from "react";
import { describe, expect, it, vi } from "vitest";

import { expectAccessible } from "../../test/a11y";
import { Switch } from "./Switch";

describe("Switch", () => {
  it("is a switch named by its label and described by its effect", async () => {
    const { container } = render(
      <Switch label="Deploy on push" description="Each push to main opens a plan for production." />,
    );
    const toggle = screen.getByRole("switch", { name: "Deploy on push" });
    expect(toggle).toHaveAccessibleDescription("Each push to main opens a plan for production.");
    expect(toggle).toHaveAttribute("aria-checked", "false");
    await expectAccessible(container);
  });

  it("toggles on its own when uncontrolled, by click, label and keyboard", async () => {
    const user = userEvent.setup();
    const onChange = vi.fn();
    render(<Switch label="Drain apps first" defaultChecked onChange={onChange} />);
    const toggle = screen.getByRole("switch", { name: "Drain apps first" });
    expect(toggle).toHaveAttribute("aria-checked", "true");
    await user.click(toggle);
    expect(toggle).toHaveAttribute("aria-checked", "false");
    expect(onChange).toHaveBeenLastCalledWith(false);
    await user.click(screen.getByText("Drain apps first"));
    expect(toggle).toHaveAttribute("aria-checked", "true");
    toggle.focus();
    await user.keyboard(" ");
    expect(toggle).toHaveAttribute("aria-checked", "false");
    await user.keyboard("{Enter}");
    expect(toggle).toHaveAttribute("aria-checked", "true");
    expect(onChange).toHaveBeenCalledTimes(4);
  });

  it("only reports changes when controlled", async () => {
    const user = userEvent.setup();
    const onChange = vi.fn();
    const { rerender } = render(<Switch label="Secret" checked={false} onChange={onChange} />);
    const toggle = screen.getByRole("switch", { name: "Secret" });
    await user.click(toggle);
    expect(onChange).toHaveBeenCalledWith(true);
    expect(toggle).toHaveAttribute("aria-checked", "false");
    rerender(<Switch label="Secret" checked onChange={onChange} />);
    expect(toggle).toHaveAttribute("aria-checked", "true");

    function Owner() {
      const [on, setOn] = useState(false);
      return <Switch label="External access" checked={on} onChange={setOn} />;
    }
    render(<Owner />);
    const owned = screen.getByRole("switch", { name: "External access" });
    await user.click(owned);
    expect(owned).toHaveAttribute("aria-checked", "true");
  });

  it("can't be changed while disabled, and takes an aria-label without a visible label", async () => {
    const user = userEvent.setup();
    const onChange = vi.fn();
    const { container } = render(
      <Switch aria-label="Protect production" disabled onChange={onChange} className="extra" />,
    );
    const toggle = screen.getByRole("switch", { name: "Protect production" });
    expect(toggle).toBeDisabled();
    await user.click(toggle);
    expect(onChange).not.toHaveBeenCalled();
    expect(container.firstChild).toHaveClass("dw-switch", "is-disabled", "extra");
    await expectAccessible(container);
  });
});
