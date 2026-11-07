// SPDX-License-Identifier: AGPL-3.0-only

import { describe, expect, it } from "vitest";

import { settleRouters, track } from "./routers";

function loading(forMs: number) {
  const router = { state: { status: "pending" as "pending" | "idle", isLoading: true } };
  setTimeout(() => {
    router.state = { status: "idle", isLoading: false };
  }, forMs);
  return router;
}

describe("settling a test's routers", () => {
  it("waits for a router to finish what it's loading", async () => {
    const router = track(loading(60));
    await settleRouters();
    expect(router.state.status).toBe("idle");
  });

  it("gives up on one that never finishes, rather than hang the test", async () => {
    const started = Date.now();
    const router = track({ state: { status: "pending" as const, isLoading: true } });
    await settleRouters();
    expect(router.state.status).toBe("pending");
    expect(Date.now() - started).toBeLessThan(3_000);
  });
});
