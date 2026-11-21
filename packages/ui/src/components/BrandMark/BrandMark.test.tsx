// SPDX-License-Identifier: AGPL-3.0-only

import { render, screen } from "@testing-library/react";
import { describe, expect, it } from "vitest";

import { expectAccessible } from "../../test/a11y";
import { BrandMark } from "./BrandMark";
import { BRANDS, brandIds } from "./brands";

describe("BrandMark", () => {
  it("draws every brand's own mark inline, on its ground, asking no other host", () => {
    for (const id of brandIds) {
      const { container, unmount } = render(<BrandMark brand={id} />);
      const tile = container.querySelector(".dw-brand-mark");
      expect(tile?.getAttribute("data-ground")).toBe(BRANDS[id].ground);
      expect(tile?.querySelector("svg path")).not.toBeNull();
      expect(container.querySelector("img, image, use")).toBeNull();
      unmount();
    }
  });

  it("is decorative beside a name, and says the label where it stands alone", async () => {
    const { container, rerender } = render(<BrandMark brand="cloudflare" />);
    expect(container.querySelector(".dw-brand-mark")?.getAttribute("aria-hidden")).toBe("true");
    rerender(<BrandMark brand="amazon-s3" label="Amazon S3" size={40} />);
    const tile = screen.getByRole("img", { name: "Amazon S3" });
    expect(tile.style.width).toBe("40px");
    await expectAccessible(container);
  });

  it("draws the product's own icon for a service with no mark", () => {
    const { container } = render(<BrandMark icon="mail" />);
    const tile = container.querySelector(".dw-brand-mark");
    expect(tile?.getAttribute("data-ground")).toBe("none");
    expect(tile?.querySelector(".dw-icon")).not.toBeNull();
  });
});
