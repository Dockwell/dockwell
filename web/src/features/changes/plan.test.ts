// SPDX-License-Identifier: AGPL-3.0-only

import { describe, expect, it } from "vitest";

import { factAndRest } from "./plan";

describe("factAndRest", () => {
  it("leads with what a check answered, and follows with what to do", () => {
    expect(
      factAndRest(
        "app runs, but fails its health check: GET /healthz on port 8080 answered 503 Service Unavailable. Fix what it answers, or the check, then try again.",
      ),
    ).toEqual([
      "app runs, but fails its health check: GET /healthz on port 8080 answered 503 Service Unavailable",
      "Fix what it answers, or the check, then try again.",
    ]);
  });

  it("doesn't end a sentence at a full stop in what a command printed", () => {
    expect(
      factAndRest(
        'app runs, but fails its health check: the command exited with 1: "no. Not \\"yet\\". Really: •••". Fix what it reports, or the check, then try again.',
      ),
    ).toEqual([
      'app runs, but fails its health check: the command exited with 1: "no. Not \\"yet\\". Really: •••"',
      "Fix what it reports, or the check, then try again.",
    ]);
  });

  it("keeps a machine value whole, and a message with one sentence", () => {
    expect(factAndRest("nginx:1.27 can't be pulled. Check the name.")).toEqual([
      "nginx:1.27 can't be pulled",
      "Check the name.",
    ]);
    expect(factAndRest("it failed")).toEqual(["it failed", ""]);
  });
});
