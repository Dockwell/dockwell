// SPDX-License-Identifier: AGPL-3.0-only

import { CommandBar, FleetGrid, PlanDiff, Sparkline } from "@dockwell/ui";
import { act, screen } from "@testing-library/react";
import { afterEach, describe, expect, it } from "vitest";

import { renderWithApi } from "../test/render";
import { activateLocale } from "./i18n";

function DesignSystemWords() {
  return (
    <>
      <FleetGrid
        servers={[
          { id: "a", name: "fra-1-01", status: "healthy", cpu: 31 },
          { id: "b", name: "fra-1-07", status: "degraded", cpu: 92 },
        ]}
      />
      <Sparkline data={[1, 2.345]} />
      <CommandBar inline items={[]} scope="Acme / storefront" />
      <PlanDiff resources={[{ id: "x", action: "update", kind: "app", name: "api" }]} />
    </>
  );
}

describe("Providers", () => {
  afterEach(async () => {
    await activateLocale("en");
  });

  it("gives the design system its words in English, with plurals", () => {
    renderWithApi(<DesignSystemWords />, {});
    expect(screen.getByText("2 servers")).toBeVisible();
    expect(screen.getByRole("button", { name: "fra-1-07, Degraded, CPU 92%" })).toBeVisible();
    expect(screen.getByRole("img", { name: "Trend over 2 samples, latest 2.35" })).toBeVisible();
    expect(screen.getByText("Acme / storefront").tagName).toBe("B");
    expect(screen.getByText("Update in place")).toBeVisible();
  });

  it("translates them when the locale changes", async () => {
    renderWithApi(<DesignSystemWords />, {});
    await act(() => activateLocale("zh-CN"));
    expect(screen.getByText("2 台服务器")).toBeVisible();
    expect(screen.getByText("1 台降级")).toBeVisible();
    expect(screen.getByRole("button", { name: "fra-1-07，降级，CPU 92%" })).toBeVisible();
    expect(screen.getByRole("img", { name: "2 个样本的趋势，最新值 2.35" })).toBeVisible();
    expect(screen.getByRole("combobox", { name: "搜索或运行命令" })).toBeVisible();
    expect(screen.getByText("Acme / storefront").closest(".dw-cmd-scope")).toHaveTextContent("范围：Acme / storefront");
    expect(screen.getByText("原地更新")).toBeVisible();
    expect(screen.getByText("应用")).toBeVisible();
  });
});
