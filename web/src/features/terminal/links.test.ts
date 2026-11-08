// SPDX-License-Identifier: AGPL-3.0-only

import { afterEach, describe, expect, it, vi } from "vitest";

import { linkHandler, linkTarget } from "./links";

const range = { start: { x: 1, y: 1 }, end: { x: 4, y: 1 } };
const click = (keys: Partial<MouseEvent> = {}) => ({ ctrlKey: false, metaKey: false, ...keys }) as MouseEvent;

afterEach(() => {
  vi.restoreAllMocks();
});

describe("a link a program printed", () => {
  it("leads only to http or https", () => {
    expect(linkTarget("https://example.com/docs?a=1")).toBe("https://example.com/docs?a=1");
    expect(linkTarget("http://192.0.2.10:8080/")).toBe("http://192.0.2.10:8080/");
    for (const uri of [
      "javascript:alert(1)",
      "file:///etc/passwd",
      "data:text/html,<b>hi</b>",
      "vscode://x",
      "not a url",
    ]) {
      expect(linkTarget(uri)).toBeUndefined();
    }
  });

  it("opens with Ctrl held, in a tab that can't reach back", () => {
    const open = vi.spyOn(window, "open").mockReturnValue(null);
    const handler = linkHandler(() => null);
    expect(handler.allowNonHttpProtocols).toBe(false);
    handler.activate(click(), "https://example.com/", range);
    expect(open).not.toHaveBeenCalled();
    handler.activate(click({ ctrlKey: true }), "javascript:alert(1)", range);
    expect(open).not.toHaveBeenCalled();
    handler.activate(click({ ctrlKey: true }), "https://example.com/", range);
    expect(open).toHaveBeenCalledWith("https://example.com/", "_blank", "noopener,noreferrer");
  });

  it("says where it leads on hover", () => {
    const host = document.createElement("div");
    const handler = linkHandler(() => host);
    handler.hover?.(click(), "https://example.com/a", range);
    expect(host.title).toBe("https://example.com/a");
    handler.leave?.(click(), "https://example.com/a", range);
    expect(host.hasAttribute("title")).toBe(false);
    handler.hover?.(click(), "file:///etc/passwd", range);
    expect(host.hasAttribute("title")).toBe(false);
  });
});
