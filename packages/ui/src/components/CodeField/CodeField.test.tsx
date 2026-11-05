// SPDX-License-Identifier: AGPL-3.0-only

import { fireEvent, render, screen } from "@testing-library/react";
import { useState } from "react";
import { describe, expect, it } from "vitest";

import { expectAccessible } from "../../test/a11y";
import { CodeField } from "./CodeField";

function Field({
  start = "one\ntwo",
  ...props
}: {
  start?: string;
  error?: string;
  markedLine?: number;
  hint?: string;
}) {
  const [value, setValue] = useState(start);
  return <CodeField label="File" value={value} onChange={(e) => setValue(e.target.value)} {...props} />;
}

describe("CodeField", () => {
  it("numbers its lines as the value grows", () => {
    const { container } = render(<Field />);
    expect(container.querySelector(".dw-code-numbers")?.textContent).toBe("1\n2");
    fireEvent.change(screen.getByLabelText("File"), { target: { value: "a\nb\nc" } });
    expect(container.querySelector(".dw-code-numbers")?.textContent).toBe("1\n2\n3");
  });

  it("keeps lines whole, so a number always means one line", () => {
    render(<Field />);
    expect(screen.getByLabelText("File")).toHaveAttribute("wrap", "off");
  });

  it("says what is wrong in place of its hint, and marks the line", async () => {
    const { container } = render(<Field hint="Paste it" error="Line two is wrong" markedLine={2} />);
    const field = screen.getByLabelText("File");
    expect(field).toHaveAttribute("aria-invalid", "true");
    expect(field).toHaveAccessibleDescription("Line two is wrong");
    expect(screen.queryByText("Paste it")).not.toBeInTheDocument();
    expect(container.querySelector(".dw-code-mark")).toHaveStyle({ "--dw-code-line": "1" });
    await expectAccessible(container);
  });

  it("marks nothing without a line", () => {
    const { container } = render(<Field hint="Paste it" />);
    expect(container.querySelector(".dw-code-mark")).not.toBeInTheDocument();
    expect(screen.getByLabelText("File")).toHaveAccessibleDescription("Paste it");
  });

  it("scrolls its numbers and its mark with the text", () => {
    const { container } = render(<Field markedLine={1} />);
    const field = screen.getByLabelText("File");
    Object.defineProperty(field, "scrollTop", { value: 40, configurable: true });
    fireEvent.scroll(field);
    expect(container.querySelector(".dw-code-numbers")).toHaveStyle({ transform: "translateY(-40px)" });
    expect(container.querySelector(".dw-code-marks")).toHaveStyle({ transform: "translateY(-40px)" });
  });

  it("brings the marked line into view when the mark moves, and leaves it alone otherwise", () => {
    const long = Array.from({ length: 200 }, (_, i) => `line ${i + 1}`).join("\n");
    const { rerender } = render(<CodeField label="File" value={long} onChange={() => {}} />);
    const field = screen.getByLabelText<HTMLTextAreaElement>("File");
    Object.defineProperty(field, "clientHeight", { configurable: true, value: 200 });
    field.style.lineHeight = "20px";
    field.scrollTop = 3000;

    rerender(<CodeField label="File" value={long} markedLine={6} onChange={() => {}} />);
    expect(field.scrollTop).toBe(10);

    // Scrolled away, the same mark stays where the reader went.
    field.scrollTop = 1500;
    rerender(<CodeField label="File" value={`${long}\n`} markedLine={6} onChange={() => {}} />);
    expect(field.scrollTop).toBe(1500);

    // Already in view, nothing jumps.
    field.scrollTop = 100;
    rerender(<CodeField label="File" value={long} markedLine={10} onChange={() => {}} />);
    expect(field.scrollTop).toBe(100);
  });

  it("still hands its ref to whoever passes one", () => {
    const seen: (HTMLTextAreaElement | null)[] = [];
    render(<CodeField label="File" value="a" onChange={() => {}} ref={(el) => void seen.push(el)} />);
    expect(seen[0]).toBe(screen.getByLabelText("File"));
  });
});
