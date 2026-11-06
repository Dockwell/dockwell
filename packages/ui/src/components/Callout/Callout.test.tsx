// SPDX-License-Identifier: AGPL-3.0-only

import { render, screen } from "@testing-library/react";
import { describe, expect, it } from "vitest";

import { expectAccessible } from "../../test/a11y";
import { Button } from "../Button/Button";
import { Callout } from "./Callout";

describe("Callout", () => {
  it("draws not knowing in its own grey, in whatever tone it's said", () => {
    const { container } = render(
      <Callout tone="info" shape="dashed-ring">
        Nobody can say.
      </Callout>,
    );
    expect(container.querySelector(".dw-callout")).toHaveClass("dw-tone-cobalt");
    expect(container.querySelector(".dw-callout-glyph")).toHaveClass("dw-tone-unknown");
    const { container: plain } = render(<Callout tone="info">Something to know.</Callout>);
    expect(plain.querySelector(".dw-callout-glyph")).not.toHaveClass("dw-tone-unknown");
  });

  it("is a polite status with the tone's shape, by default info", async () => {
    const { container } = render(
      <Callout title="Firewall changes can’t lock you out">Dockwell keeps port 22 open to your address.</Callout>,
    );
    const callout = screen.getByRole("status");
    expect(callout).toHaveClass("dw-callout", "dw-tone-cobalt");
    expect(callout).toHaveTextContent(
      "Firewall changes can’t lock you outDockwell keeps port 22 open to your address.",
    );
    expect(container.querySelector(".dw-callout-glyph circle")).not.toBeNull();
    await expectAccessible(container);
  });

  it("alerts for critical, with a square, and holds actions", async () => {
    const { container } = render(
      <Callout tone="critical" title="ams-1-03 is down" actions={<Button size="sm">Replace server</Button>}>
        Its 2 replicas moved to ams-1-04 and ams-1-05.
      </Callout>,
    );
    expect(screen.getByRole("alert")).toHaveClass("dw-tone-critical");
    expect(container.querySelector(".dw-callout-glyph rect")).not.toBeNull();
    expect(screen.getByRole("button", { name: "Replace server" }).closest(".dw-callout-actions")).not.toBeNull();
    await expectAccessible(container);
  });

  it("pairs warning with a triangle and healthy with a circle", () => {
    const { container: warning } = render(<Callout tone="warning" title="3 security updates" />);
    expect(warning.querySelector(".dw-tone-warning .dw-callout-glyph path")).not.toBeNull();
    expect(warning.querySelector(".dw-callout-text")).toBeNull();
    const { container: healthy } = render(<Callout tone="healthy">Copied.</Callout>);
    expect(healthy.querySelector(".dw-tone-healthy .dw-callout-glyph circle")).not.toBeNull();
    expect(healthy.querySelector(".dw-callout-title")).toBeNull();
  });
});
