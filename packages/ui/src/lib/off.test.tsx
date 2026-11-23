// SPDX-License-Identifier: AGPL-3.0-only

import { cleanup, fireEvent, render, screen } from "@testing-library/react";
import userEvent from "@testing-library/user-event";
import { describe, expect, it, vi } from "vitest";

import { Button } from "../components/Button/Button";

describe("a control that's off", () => {
  it("keeps its accessible description and puts its reason in the top layer on hover and focus", async () => {
    const showPopover = vi.fn();
    const hidePopover = vi.fn();
    // jsdom doesn't implement the Popover API or compute layout.
    Object.assign(HTMLElement.prototype, { showPopover, hidePopover });
    try {
      vi.spyOn(document.documentElement, "clientWidth", "get").mockReturnValue(390);
      vi.spyOn(document.documentElement, "clientHeight", "get").mockReturnValue(844);
      const onClick = vi.fn();
      const onSubmit = vi.fn();
      const user = userEvent.setup();
      render(
        <form onSubmit={onSubmit} style={{ overflow: "auto" }}>
          <Button type="submit" off="Dockwell needs this service" onClick={onClick}>
            Stop
          </Button>
          <Button>Close</Button>
        </form>,
      );
      const stop = screen.getByRole("button", { name: "Stop" });
      const reason = screen.getByText("Dockwell needs this service");
      expect(stop).toHaveAttribute("aria-disabled", "true");
      expect(stop).toHaveAccessibleDescription(reason.textContent);
      expect(stop).toHaveAttribute("aria-describedby", reason.id);
      expect(reason).toHaveAttribute("popover", "manual");

      const anchor = vi.spyOn(stop, "getBoundingClientRect").mockReturnValue(new DOMRect(16, 785, 50, 32));
      vi.spyOn(reason, "getBoundingClientRect").mockReturnValue(new DOMRect(0, 0, 240, 48));
      fireEvent.pointerEnter(stop, { pointerType: "mouse" });
      expect(showPopover).toHaveBeenCalledOnce();
      expect(reason).toHaveStyle({ left: "16px", top: "733px" });

      anchor.mockReturnValue(new DOMRect(324, 36, 50, 32));
      fireEvent.scroll(stop.parentElement as HTMLElement);
      expect(reason).toHaveStyle({ left: "134px", top: "72px" });

      await user.tab();
      expect(stop).toHaveFocus();
      fireEvent.pointerLeave(stop);
      expect(hidePopover).not.toHaveBeenCalled();
      await user.keyboard("{Enter}");
      expect(onClick).not.toHaveBeenCalled();
      expect(onSubmit).not.toHaveBeenCalled();
      await user.tab();
      expect(screen.getByRole("button", { name: "Close" })).toHaveFocus();
      expect(hidePopover).toHaveBeenCalledOnce();
      await user.tab({ shift: true });
      expect(showPopover).toHaveBeenCalledTimes(2);
    } finally {
      cleanup();
      Reflect.deleteProperty(HTMLElement.prototype, "showPopover");
      Reflect.deleteProperty(HTMLElement.prototype, "hidePopover");
    }
  });
});
