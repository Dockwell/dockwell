// SPDX-License-Identifier: AGPL-3.0-only

import { create } from "@bufbuild/protobuf";
import { ChangeSchema, IntentKind } from "@dockwell/api";
import { describe, expect, it } from "vitest";

import { changeIcon } from "./describe";

describe("a change's mark", () => {
  it("says what it does: an undo, a restart, a deploy, taking away, or a plan", () => {
    const change = (init: Parameters<typeof create<typeof ChangeSchema>>[1]) => changeIcon(create(ChangeSchema, init));
    expect(change({ reverts: "chg_1" })).toBe("undo");
    expect(change({ plan: { intent: { kind: IntentKind.ROLL_BACK } } })).toBe("undo");
    expect(change({ restart: true })).toBe("refresh");
    expect(change({ plan: { intent: { kind: IntentKind.DEPLOY } } })).toBe("deploy");
    expect(change({ plan: { intent: { kind: IntentKind.REPULL } } })).toBe("deploy");
    expect(change({ plan: { summary: { destroy: 2 } } })).toBe("trash");
    // Taking one thing away to make another is a plan like any other.
    expect(change({ plan: { summary: { destroy: 1, create: 1 } } })).toBe("commit");
    expect(change({ plan: { summary: { create: 3 } } })).toBe("commit");
  });
});
