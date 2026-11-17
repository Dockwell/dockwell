// SPDX-License-Identifier: AGPL-3.0-only

import { create } from "@bufbuild/protobuf";
import { PlanAction, PlanSchema, PlanStepSchema, RouteReach } from "@dockwell/api";
import { describe, expect, it } from "vitest";

import { type ChangeWords, reachLines, tunnelledRoutes } from "./words";

/** The two lines reachLines reads, in words a test can see. */
const words = {
  unencrypted: (server: string) => `unencrypted to ${server}`,
  answers: (answers: { reach: RouteReach } | undefined, port: unknown, server: string) => {
    if (!answers) return [];
    const out = answers.reach === RouteReach.ANYONE ? [] : [`answers only ${server}`];
    return port ? [...out, `reaches a port on ${server}`] : out;
  },
} as unknown as ChangeWords;

function route(way: string) {
  return create(PlanSchema, {
    resources: [
      {
        id: "rte_1",
        resource: { kind: "route", scope: "env_1", name: "wiki" },
        action: PlanAction.CREATE,
        changes: [{ path: "publish.way", to: way }],
      },
    ],
  });
}

describe("how a route is reached, through a tunnel Dockwell publishes (ADR 0104)", () => {
  const step = create(PlanStepSchema, {
    resourceId: "rte_1",
    unencrypted: true,
    answers: { reach: RouteReach.THIS_SERVER },
  });

  it("says nothing of the happy path: TLS ends at Cloudflare, and only the server's tunnel reaches it", () => {
    expect(tunnelledRoutes(route("server-tunnel")).has("rte_1")).toBe(true);
    expect(reachLines(words, step, "web-1", true)).toEqual([]);
  });

  it("still says what it lets reach a port", () => {
    const port = create(PlanStepSchema, {
      resourceId: "rte_1",
      unencrypted: true,
      answers: { reach: RouteReach.THIS_SERVER },
      serverPort: { port: 8080 },
    });
    expect(reachLines(words, port, "web-1", true)).toEqual(["reaches a port on web-1"]);
  });

  it("says nothing of answering the tunnel's loopback alone, and still says a list with more in it", () => {
    const only = (proxies: string[]) =>
      create(PlanStepSchema, { resourceId: "rte_1", answers: { reach: RouteReach.PROXIES, proxies } });
    expect(reachLines(words, only(["127.0.0.1/32"]), "web-1", true)).toEqual([]);
    expect(reachLines(words, only(["127.0.0.1/32", "10.0.0.0/8"]), "web-1", true)).toEqual(["answers only web-1"]);
  });

  it("says both for a route that isn't published through a tunnel", () => {
    expect(tunnelledRoutes(route("front")).size).toBe(0);
    expect(reachLines(words, step, "web-1", false)).toEqual(["unencrypted to web-1", "answers only web-1"]);
  });
});
