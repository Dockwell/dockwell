// SPDX-License-Identifier: AGPL-3.0-only

import { render, screen } from "@testing-library/react";
import userEvent from "@testing-library/user-event";
import { expect, it } from "vitest";

import { TagLink } from "./Tag";

it("makes the whole word tag a keyboard-accessible link", async () => {
  render(
    <TagLink href="/apps/example" words tone="cobalt">
      Managed by Dockwell
    </TagLink>,
  );
  const link = screen.getByRole("link", { name: "Managed by Dockwell" });
  expect(link).toHaveClass("dw-tag", "dw-tag-words", "dw-tag-cobalt", "dw-tag-link");
  expect(link).toHaveAttribute("href", "/apps/example");
  await userEvent.setup().tab();
  expect(link).toHaveFocus();
});
