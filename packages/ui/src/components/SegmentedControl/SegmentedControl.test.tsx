// SPDX-License-Identifier: AGPL-3.0-only

import { render, screen } from "@testing-library/react";
import userEvent from "@testing-library/user-event";
import { describe, expect, it, vi } from "vitest";

import { expectAccessible } from "../../test/a11y";
import { SegmentedControl, type SegmentedOption } from "./SegmentedControl";

type Env = "production" | "staging" | "previews";
const options: SegmentedOption<Env>[] = [
  { value: "production", label: "Production", count: 14 },
  { value: "staging", label: "Staging", count: 12, icon: "layers" },
  { value: "previews", label: "Previews", count: 3 },
];

const radio = (name: string) => screen.getByRole("radio", { name: new RegExp(`^${name}`) });

describe("SegmentedControl", () => {
  it("is a named radio group that selects the first option by default", async () => {
    const { container } = render(<SegmentedControl label="Environment" options={options} />);
    expect(screen.getByRole("radiogroup", { name: "Environment" })).toBeVisible();
    expect(radio("Production")).toHaveAttribute("aria-checked", "true");
    expect(radio("Production")).toHaveClass("is-on");
    expect(radio("Production")).toHaveTextContent("Production14");
    expect(radio("Staging")).toHaveAttribute("aria-checked", "false");
    await expectAccessible(container);
  });

  it("keeps only the selected option in the tab order", async () => {
    const user = userEvent.setup();
    render(
      <>
        <button type="button">Before</button>
        <SegmentedControl label="Environment" options={options} defaultValue="staging" />
        <button type="button">After</button>
      </>,
    );
    expect(radio("Staging")).toHaveAttribute("tabindex", "0");
    expect(radio("Production")).toHaveAttribute("tabindex", "-1");
    screen.getByRole("button", { name: "Before" }).focus();
    await user.tab();
    expect(radio("Staging")).toHaveFocus();
    await user.tab();
    expect(screen.getByRole("button", { name: "After" })).toHaveFocus();
  });

  it("moves selection and focus with arrow keys, wrapping, and with Home and End", async () => {
    const user = userEvent.setup();
    const onChange = vi.fn();
    render(<SegmentedControl label="Environment" options={options} onChange={onChange} />);
    radio("Production").focus();
    await user.keyboard("{ArrowRight}");
    expect(radio("Staging")).toHaveFocus();
    expect(radio("Staging")).toHaveAttribute("aria-checked", "true");
    await user.keyboard("{ArrowDown}");
    expect(radio("Previews")).toHaveFocus();
    await user.keyboard("{ArrowRight}");
    expect(radio("Production")).toHaveFocus();
    await user.keyboard("{ArrowLeft}");
    expect(radio("Previews")).toHaveFocus();
    await user.keyboard("{Home}");
    expect(radio("Production")).toHaveFocus();
    await user.keyboard("{End}");
    expect(radio("Previews")).toHaveFocus();
    await user.keyboard("{ArrowUp}");
    expect(radio("Staging")).toHaveAttribute("aria-checked", "true");
    expect(onChange.mock.calls.map(([value]) => value)).toEqual([
      "staging",
      "previews",
      "production",
      "previews",
      "production",
      "previews",
      "staging",
    ]);
  });

  it("reports changes without moving when controlled, and ignores a click on the selected option", async () => {
    const user = userEvent.setup();
    const onChange = vi.fn();
    const { rerender } = render(
      <SegmentedControl label="Environment" options={options} value="production" onChange={onChange} size="sm" />,
    );
    await user.click(radio("Production"));
    expect(onChange).not.toHaveBeenCalled();
    await user.click(radio("Previews"));
    expect(onChange).toHaveBeenCalledWith("previews");
    expect(radio("Production")).toHaveAttribute("aria-checked", "true");
    rerender(<SegmentedControl label="Environment" options={options} value="previews" onChange={onChange} size="sm" />);
    expect(radio("Previews")).toHaveAttribute("aria-checked", "true");
    expect(screen.getByRole("radiogroup")).toHaveClass("dw-seg-sm");
  });

  it("keeps a tab stop when the value matches no option", () => {
    render(<SegmentedControl<string> label="Environment" options={options} value="gone" />);
    expect(radio("Production")).toHaveAttribute("tabindex", "0");
    expect(screen.queryAllByRole("radio", { checked: true })).toHaveLength(0);
  });
});
