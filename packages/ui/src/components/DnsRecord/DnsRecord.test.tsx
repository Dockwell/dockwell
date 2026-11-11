// SPDX-License-Identifier: AGPL-3.0-only

import { render, screen } from "@testing-library/react";
import userEvent from "@testing-library/user-event";
import { describe, expect, it, vi } from "vitest";

import { expectAccessible } from "../../test/a11y";
import { DnsRecord } from "./DnsRecord";

describe("DnsRecord", () => {
  it("shows the record to make and says what DNS has, as a status", async () => {
    const { container } = render(
      <DnsRecord
        type="A"
        name="blog.example.com"
        value="203.0.113.7"
        state={{ status: "deploying", word: "Not set yet" }}
        onCheck={() => {}}
      />,
    );
    expect(container.querySelector(".dw-dns-row")).toHaveTextContent("Ablog.example.com203.0.113.7");
    expect(screen.getByRole("status")).toHaveTextContent("Not set yet");
    await expectAccessible(container);
  });

  it("copies the value alone, and says it did", async () => {
    const user = userEvent.setup();
    const writeText = vi.fn(async () => {});
    Object.defineProperty(navigator, "clipboard", { configurable: true, value: { writeText } });
    render(<DnsRecord type="A" name="blog.example.com" value="203.0.113.7" />);
    await user.click(screen.getByRole("button", { name: "Copy 203.0.113.7" }));
    expect(writeText).toHaveBeenCalledWith("203.0.113.7");
    expect(await screen.findByRole("button", { name: "Copied 203.0.113.7" })).toBeVisible();
  });

  it("checks again on request, and shows that it's checking", async () => {
    const user = userEvent.setup();
    const onCheck = vi.fn();
    const { rerender } = render(
      <DnsRecord
        type="A"
        name="a.example.com"
        value="203.0.113.7"
        state={{ status: "degraded", word: "Points elsewhere" }}
        onCheck={onCheck}
      />,
    );
    await user.click(screen.getByRole("button", { name: "Check again" }));
    expect(onCheck).toHaveBeenCalledOnce();
    rerender(
      <DnsRecord
        type="A"
        name="a.example.com"
        value="203.0.113.7"
        state={{ status: "degraded", word: "Points elsewhere" }}
        onCheck={onCheck}
        checking
      />,
    );
    expect(screen.getByRole("button", { name: "Check again" })).toHaveAttribute("aria-disabled", "true");
  });

  it("offers no check and says nothing where nothing was checked", () => {
    render(<DnsRecord type="AAAA" name="a.example.com" value="2001:db8::7" />);
    expect(screen.queryByRole("status")).toBeNull();
    expect(screen.queryByRole("button", { name: "Check again" })).toBeNull();
  });
});
