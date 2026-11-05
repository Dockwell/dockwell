// SPDX-License-Identifier: AGPL-3.0-only

import { render, screen } from "@testing-library/react";
import userEvent from "@testing-library/user-event";
import { describe, expect, it, vi } from "vitest";

import { expectAccessible } from "../../test/a11y";
import { SelectField } from "./SelectField";

const options = [
  { value: "a", label: "Production" },
  { value: "b", label: "Staging" },
];

describe("SelectField", () => {
  it("is a labelled native select", async () => {
    const user = userEvent.setup();
    const onChange = vi.fn();
    const { container } = render(
      <SelectField label="Environment" hint="Where it runs" options={options} onChange={onChange} />,
    );
    const select = screen.getByLabelText("Environment");
    expect(select).toHaveAccessibleDescription("Where it runs");
    await user.selectOptions(select, "b");
    expect(onChange).toHaveBeenCalled();
    await expectAccessible(container);
  });

  it("says what is wrong in place of its hint", () => {
    render(<SelectField label="Environment" hint="Where it runs" error="Choose one" options={options} />);
    const select = screen.getByLabelText("Environment");
    expect(select).toHaveAttribute("aria-invalid", "true");
    expect(select).toHaveAccessibleDescription("Choose one");
  });
});
