// SPDX-License-Identifier: AGPL-3.0-only

import { render } from "@testing-library/react";
import { describe, expect, it } from "vitest";

import { lockBodyScroll, tabbableIn } from "./modal";

describe("tabbableIn", () => {
  it("finds what Tab reaches, in document order", () => {
    const { container } = render(
      <div>
        <button type="button">One</button>
        <button type="button" disabled>
          Off
        </button>
        <a href="#x">Two</a>
        {/* biome-ignore lint/a11y/useValidAnchor: an anchor without href isn't tabbable, which is the point */}
        <a>Not a link</a>
        <input aria-label="Three" />
        <input type="hidden" />
        <span tabIndex={-1}>Programmatic</span>
        {/* biome-ignore lint/a11y/noNoninteractiveTabindex: any element with tabIndex 0 is tabbable */}
        <span tabIndex={0}>Four</span>
        <div hidden>
          <button type="button">Hidden</button>
        </div>
      </div>,
    );
    const root = container.firstElementChild as HTMLElement;
    expect(tabbableIn(root).map((el) => el.textContent || el.getAttribute("aria-label"))).toEqual([
      "One",
      "Two",
      "Three",
      "Four",
    ]);
  });
});

describe("lockBodyScroll", () => {
  it("counts nested locks and restores the body's own style", () => {
    document.body.style.overflow = "scroll";
    const outer = lockBodyScroll();
    const inner = lockBodyScroll();
    expect(document.body.style.overflow).toBe("hidden");
    inner();
    inner();
    expect(document.body.style.overflow).toBe("hidden");
    outer();
    expect(document.body.style.overflow).toBe("scroll");
    document.body.style.overflow = "";
  });
});
