// SPDX-License-Identifier: AGPL-3.0-only

import { render, screen } from "@testing-library/react";
import userEvent from "@testing-library/user-event";
import { createRef, useState } from "react";
import { describe, expect, it, vi } from "vitest";

import { englishStrings, UIStringsProvider } from "../../lib/strings";
import { expectAccessible } from "../../test/a11y";
import { Button } from "../Button/Button";
import { TextField } from "./TextField";

describe("TextField", () => {
  it("labels the input and describes it with the hint", async () => {
    const { container } = render(
      <TextField label="Health check path" hint="Dockwell waits for 200 before sending traffic." defaultValue="/" />,
    );
    const input = screen.getByRole("textbox", { name: "Health check path" });
    expect(input).toHaveAccessibleDescription("Dockwell waits for 200 before sending traffic.");
    expect(input).toHaveAttribute("type", "text");
    expect(input).not.toHaveAttribute("aria-invalid");
    await expectAccessible(container);
  });

  it("replaces the hint with the error, marks the input invalid and shows the critical shape", async () => {
    const { container } = render(
      <TextField
        label="Domain"
        hint="The domain to route."
        error="api.acme.dev already routes to another project. Remove it there, or use a different subdomain."
        aria-describedby="extra"
      />,
    );
    const input = screen.getByRole("textbox", { name: "Domain" });
    expect(input).toHaveAttribute("aria-invalid", "true");
    expect(input.getAttribute("aria-describedby")?.split(" ")).toHaveLength(2);
    expect(screen.queryByText("The domain to route.")).toBeNull();
    expect(container.querySelector(".dw-field")).toHaveClass("is-invalid");
    expect(container.querySelector(".dw-field-error svg")).toHaveAttribute("aria-hidden", "true");
    await expectAccessible(container);
  });

  it("marks optional fields with the translated word", () => {
    render(
      <UIStringsProvider strings={{ ...englishStrings, optional: "可选" }}>
        <TextField label="Description" optional />
      </UIStringsProvider>,
    );
    expect(screen.getByText("可选")).toHaveClass("dw-field-opt");
  });

  it("sets machine values in mono without spell checking, with affixes, an icon and an action", async () => {
    const { container } = render(
      <TextField
        label="Repository"
        mono
        prefix="github.com/"
        suffix=".git"
        icon="branch"
        size="lg"
        action={<Button size="sm" variant="ghost" icon="copy" aria-label="Copy" />}
      />,
    );
    const input = screen.getByRole("textbox", { name: "Repository" });
    expect(input).toHaveClass("is-mono");
    expect(input).toHaveAttribute("spellcheck", "false");
    expect(container.querySelector(".dw-input")).toHaveClass("dw-input-lg", "has-icon");
    expect(screen.getByText("github.com/")).toHaveClass("dw-input-prefix");
    expect(screen.getByText(".git")).toHaveClass("dw-input-suffix");
    expect(screen.getByRole("button", { name: "Copy" })).toBeVisible();
    await expectAccessible(container);
  });

  it("works controlled and uncontrolled, and passes the ref and id", async () => {
    const user = userEvent.setup();
    const ref = createRef<HTMLInputElement>();
    render(<TextField label="Name" id="name" ref={ref} />);
    const input = screen.getByRole("textbox", { name: "Name" });
    expect(ref.current).toBe(input);
    expect(input).toHaveAttribute("id", "name");
    await user.type(input, "web");
    expect(input).toHaveValue("web");

    const onChange = vi.fn();
    function Controlled() {
      const [value, setValue] = useState("api");
      return (
        <TextField
          label="Service"
          value={value}
          onChange={(event) => {
            onChange(event.target.value);
            setValue(event.target.value.toLowerCase());
          }}
        />
      );
    }
    render(<Controlled />);
    const controlled = screen.getByRole("textbox", { name: "Service" });
    await user.type(controlled, "X");
    expect(onChange).toHaveBeenCalledWith("apiX");
    expect(controlled).toHaveValue("apix");
  });
});
