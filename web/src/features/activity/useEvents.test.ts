// SPDX-License-Identifier: AGPL-3.0-only

import { timestampFromDate } from "@bufbuild/protobuf/wkt";
import type { Event } from "@dockwell/api";
import { describe, expect, it } from "vitest";

import { auditEvent, snapshot } from "../../test/changes";
import { type Loaded, takeSnapshot } from "./useEvents";

const MIN = 60_000;

function at(id: string, minutesAgo: number): Event {
  return auditEvent({ id, action: "auth.sign_in", time: timestampFromDate(new Date(Date.now() - minutesAgo * MIN)) });
}

function loaded(events: Event[], older: string): Loaded {
  return { events: new Map(events.map((e) => [e.id, e])), older };
}

const ids = (l: Loaded) => [...l.events.keys()].sort();

describe("takeSnapshot", () => {
  const before = loaded([at("evt_5", 5), at("evt_6", 6), at("evt_9", 9), at("evt_30", 30)], "below-30");

  it("takes the first snapshot as all there is", () => {
    const { loaded: next, kept } = takeSnapshot(undefined, snapshot([at("evt_1", 1)], "below-1"));
    expect(kept).toBe(false);
    expect(ids(next)).toEqual(["evt_1"]);
    expect(next.older).toBe("below-1");
  });

  it("keeps what was loaded below a snapshot that meets it, and pages on where it was", () => {
    // Two new events while away; the snapshot reaches back to evt_6, which was loaded, and evt_5 left.
    const { loaded: next, kept } = takeSnapshot(
      before,
      snapshot([at("evt_0", 0), at("evt_2", 2), at("evt_6", 6)], "below-6"),
    );
    expect(kept).toBe(true);
    expect(ids(next)).toEqual(["evt_0", "evt_2", "evt_30", "evt_6", "evt_9"]);
    expect(next.older).toBe("below-30");
  });

  it("starts over below a snapshot that doesn't reach what was loaded, which would leave a gap", () => {
    const { loaded: next, kept } = takeSnapshot(
      before,
      snapshot([at("evt_0", 0), at("evt_1", 1), at("evt_2", 2)], "below-2"),
    );
    expect(kept).toBe(false);
    expect(ids(next)).toEqual(["evt_0", "evt_1", "evt_2"]);
    expect(next.older).toBe("below-2");
  });

  it("takes a snapshot that holds everything as everything", () => {
    const { loaded: next } = takeSnapshot(before, snapshot([at("evt_0", 0), at("evt_6", 6)], ""));
    expect(ids(next)).toEqual(["evt_0", "evt_6"]);
    expect(next.older).toBe("");
  });
});
