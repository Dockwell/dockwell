// SPDX-License-Identifier: AGPL-3.0-only

import { describe, expect, it } from "vitest";

import { mergeComposeDraft } from "./mergeComposeDraft";

describe("reloading a Compose draft", () => {
  const base = "services:\n  cache:\n    image: redis:7\n  web:\n    image: nginx:1.27\n";
  const latest = base.replace("redis:7", "redis:8");

  it("keeps edits to a different service and retains the new revision", () => {
    const draft = base.replace("nginx:1.27", "nginx:1.28");
    expect(mergeComposeDraft(base, draft, latest)).toBe(latest.replace("nginx:1.27", "nginx:1.28"));
  });

  it("leaves overlapping edits for side-by-side review", () => {
    expect(mergeComposeDraft(base, base.replace("redis:7", "redis:6"), latest)).toBeUndefined();
  });

  it("keeps a change made in both versions once", () => {
    const draft = latest.replace("nginx:1.27", "nginx:1.28");
    expect(mergeComposeDraft(base, draft, latest)).toBe(draft);
  });
});
