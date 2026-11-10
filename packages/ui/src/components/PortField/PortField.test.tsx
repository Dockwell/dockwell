// SPDX-License-Identifier: AGPL-3.0-only

import { render, screen } from "@testing-library/react";
import userEvent from "@testing-library/user-event";
import { useState } from "react";
import { describe, expect, it, vi } from "vitest";

import { expectAccessible } from "../../test/a11y";
import { type PortCheck, PortField, validPortNumber } from "./PortField";

function Field({ check, suggestion, start = "" }: { check?: PortCheck; suggestion?: number; start?: string }) {
  const [value, setValue] = useState(start);
  return <PortField label="Port" value={value} onChange={setValue} check={check} suggestion={suggestion} />;
}

describe("PortField", () => {
  it("says a free port is free, beside it and to a screen reader", async () => {
    const { container } = render(<Field start="8083" check={{ state: "free" }} />);
    const field = screen.getByRole("textbox", { name: "Port" });
    expect(field).toHaveValue("8083");
    expect(field).toHaveAttribute("inputmode", "numeric");
    expect(field).toHaveAccessibleDescription("Free");
    await expectAccessible(container);
  });

  it("says what holds a taken port, and takes the free one offered", async () => {
    const user = userEvent.setup();
    const { container } = render(<Field start="8080" check={{ state: "taken", by: "redis" }} suggestion={8084} />);
    const field = screen.getByRole("textbox", { name: "Port" });
    expect(field).toHaveAttribute("aria-invalid", "true");
    expect(screen.getByRole("alert")).toHaveTextContent("redis uses it · Use 8084");
    await expectAccessible(container);
    await user.click(screen.getByRole("button", { name: "Use 8084" }));
    expect(field).toHaveValue("8084");
  });

  it("says a port is taken when nobody can say by what", () => {
    render(<Field start="8080" check={{ state: "taken" }} />);
    expect(screen.getByRole("alert")).toHaveTextContent(/^Taken$/);
  });

  it("offers a free port while it's empty, and refuses what isn't a port", async () => {
    const user = userEvent.setup();
    render(<Field suggestion={8083} check={{ state: "free" }} />);
    const field = screen.getByRole("textbox", { name: "Port" });
    // Nothing typed, so nothing is said to be free yet.
    expect(screen.queryByText("Free")).toBeNull();
    await user.click(screen.getByRole("button", { name: "Use 8083" }));
    expect(field).toHaveValue("8083");
    expect(screen.queryByRole("button", { name: "Use 8083" })).toBeNull();
    await user.clear(field);
    await user.type(field, "70000");
    expect(screen.getByRole("alert")).toHaveTextContent("Use a port from 1 to 65535.");
    expect(screen.queryByText("Free")).toBeNull();
  });

  it("takes digits only", async () => {
    const user = userEvent.setup();
    const onChange = vi.fn();
    render(<PortField label="Port" value="" onChange={onChange} />);
    await user.type(screen.getByRole("textbox", { name: "Port" }), "8a");
    expect(onChange).toHaveBeenNthCalledWith(1, "8");
    expect(onChange).toHaveBeenNthCalledWith(2, "");
  });

  it("says nothing of a port nobody can check, and shows that it's checking", () => {
    const { rerender } = render(
      <PortField label="Port" value="8080" onChange={() => {}} check={{ state: "unknown" }} />,
    );
    expect(screen.getByRole("textbox", { name: "Port" })).not.toHaveAccessibleDescription();
    rerender(<PortField label="Port" value="8080" onChange={() => {}} check={{ state: "checking" }} />);
    expect(screen.getByRole("textbox", { name: "Port" })).toHaveAccessibleDescription("Checking");
  });

  it("knows a port from what isn't one", () => {
    expect(validPortNumber("1")).toBe(true);
    expect(validPortNumber("65535")).toBe(true);
    expect(validPortNumber("0")).toBe(false);
    expect(validPortNumber("65536")).toBe(false);
    expect(validPortNumber("80a")).toBe(false);
    expect(validPortNumber("")).toBe(false);
  });
});
