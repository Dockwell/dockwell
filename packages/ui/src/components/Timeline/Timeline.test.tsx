// SPDX-License-Identifier: AGPL-3.0-only

import { render, screen, within } from "@testing-library/react";
import userEvent from "@testing-library/user-event";
import { describe, expect, it, vi } from "vitest";

import { englishStrings, UIStringsProvider } from "../../lib/strings";
import { expectAccessible } from "../../test/a11y";
import { Timeline, type TimelineEvent } from "./Timeline";

const events: TimelineEvent[] = [
  {
    id: 1,
    kind: "alert",
    tone: "warning",
    title: "worker memory above 90%",
    meta: "4 servers",
    time: "14:16",
    datetime: "2026-11-03T14:16:00Z",
  },
  {
    id: 2,
    kind: "deploy",
    title: "Deployed worker",
    status: "degraded",
    revision: "r57",
    actor: "Sam Okafor",
    time: "13:56",
    undoable: true,
    undoLabel: "Roll back",
  },
  { id: 3, kind: "config", title: "Changed MAX_CONNECTIONS on api", time: "12:02", undoable: true },
];

describe("Timeline", () => {
  it("takes another mark for events its kinds don't name", () => {
    const { container } = render(
      <Timeline events={[{ id: "sign-in", kind: "config", icon: "user", title: "Signed in", time: "14:02" }]} />,
    );
    const mark = container.querySelector(".dw-tl-mark svg");
    const plain = render(<Timeline events={[{ id: "config", kind: "config", title: "Changed", time: "14:02" }]} />);
    expect(mark?.innerHTML).not.toEqual(plain.container.querySelector(".dw-tl-mark svg")?.innerHTML);
  });

  it("is an ordered list of events with their times", async () => {
    const { container } = render(<Timeline events={events} onUndo={() => {}} />);
    const items = within(screen.getByRole("list")).getAllByRole("listitem");
    expect(items).toHaveLength(3);
    expect(container.querySelector("ol")).toHaveClass("dw-tl");
    expect(screen.getByText("14:16").tagName).toBe("TIME");
    expect(screen.getByText("14:16")).toHaveAttribute("datetime", "2026-11-03T14:16:00Z");
    expect(items[1]?.querySelector(".dw-tl-meta")?.textContent).toBe("Degradedr57Sam Okafor");
    expect(screen.getByText("r57").tagName).toBe("CODE");
    expect(items[0]?.querySelector(".dw-tl-mark")).toHaveClass("dw-tone-warning");
    expect(items[2]?.querySelector(".dw-tl-mark")?.className).toBe("dw-tl-mark");
    await expectAccessible(container);
  });

  it("offers undo on undoable events, described by the event", async () => {
    const user = userEvent.setup();
    const onUndo = vi.fn();
    render(<Timeline events={events} onUndo={onUndo} />);
    const rollBack = screen.getByRole("button", { name: "Roll back" });
    expect(rollBack).toHaveAccessibleDescription("Deployed worker");
    const undo = screen.getByRole("button", { name: "Undo" });
    expect(undo).toHaveAccessibleDescription("Changed MAX_CONNECTIONS on api");
    await user.click(rollBack);
    expect(onUndo).toHaveBeenCalledWith(events[1]);
    expect(screen.getAllByRole("button")).toHaveLength(2);
  });

  it("shows no undo without onUndo, and translates the default verb", () => {
    const { unmount } = render(<Timeline events={events} />);
    expect(screen.queryByRole("button")).toBeNull();
    unmount();
    render(
      <UIStringsProvider strings={{ ...englishStrings, undo: "撤销" }}>
        <Timeline events={events} onUndo={() => {}} />
      </UIStringsProvider>,
    );
    expect(screen.getByRole("button", { name: "撤销" })).toBeVisible();
  });

  it("takes a word for its status where the status's own says too much", () => {
    render(
      <Timeline
        events={[{ id: 1, kind: "deploy", title: "Deploy web", time: "now", status: "healthy", statusLabel: "Live" }]}
      />,
    );
    expect(screen.getByText("Live")).toBeVisible();
    expect(screen.queryByText("Healthy")).not.toBeInTheDocument();
  });
});
