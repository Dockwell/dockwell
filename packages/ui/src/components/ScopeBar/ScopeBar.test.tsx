// SPDX-License-Identifier: AGPL-3.0-only

import { render, screen } from "@testing-library/react";
import userEvent from "@testing-library/user-event";
import { describe, expect, it, vi } from "vitest";

import { englishStrings, UIStringsProvider } from "../../lib/strings";
import { expectAccessible } from "../../test/a11y";
import { ScopeBar } from "./ScopeBar";

describe("ScopeBar", () => {
  it("is navigation through each level, the last one current", async () => {
    const user = userEvent.setup();
    const onSelect = vi.fn();
    const { container } = render(
      <ScopeBar org="Acme" project="storefront" environment="production" onSelect={onSelect} />,
    );
    expect(screen.getByRole("navigation", { name: "Scope" })).toBeVisible();
    const buttons = screen.getAllByRole("button");
    expect(buttons.map((b) => b.textContent)).toEqual(["Acme", "storefront", "production"]);
    expect(screen.getByRole("button", { name: "production" })).toHaveAttribute("aria-current", "page");
    expect(screen.getByRole("button", { name: "Acme" })).not.toHaveAttribute("aria-current");
    await user.click(screen.getByRole("button", { name: "storefront" }));
    expect(onSelect).toHaveBeenCalledWith("project");
    expect(container.querySelectorAll(".dw-scope-sep")).toHaveLength(2);
    await expectAccessible(container);
  });

  it("shows a named lock on a protected environment", () => {
    render(<ScopeBar org="Acme" project="storefront" environment="production" protected onSelect={() => {}} />);
    expect(screen.getByRole("img", { name: "Protected environment" })).toBeVisible();
    expect(screen.getByRole("button", { name: "production Protected environment" })).toBeVisible();
  });

  it("is plain text without onSelect, and skips missing levels", async () => {
    const { container } = render(<ScopeBar org="Acme" project="storefront" />);
    expect(screen.queryByRole("button")).toBeNull();
    expect(screen.getByText("storefront").closest(".dw-scope-item")).toHaveAttribute("aria-current", "page");
    expect(container.querySelector(".dw-scope-caret")).toBeNull();
    await expectAccessible(container);
  });

  it("uses translated words", () => {
    render(
      <UIStringsProvider
        strings={{ ...englishStrings, scopeBar: { label: "范围", protectedEnvironment: "受保护的环境" } }}
      >
        <ScopeBar org="Acme" environment="production" protected />
      </UIStringsProvider>,
    );
    expect(screen.getByRole("navigation", { name: "范围" })).toBeVisible();
    expect(screen.getByRole("img", { name: "受保护的环境" })).toBeVisible();
  });
});
