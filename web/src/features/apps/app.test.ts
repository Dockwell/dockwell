// SPDX-License-Identifier: AGPL-3.0-only

import { timestampNow } from "@bufbuild/protobuf/wkt";
import { Connection, DeploymentState } from "@dockwell/api";
import { describe, expect, it } from "vitest";
import { anApp, aRevision, aStack, SERVER } from "../../test/apps";
import type { ServerSummary } from "../servers/server";
import {
  appState,
  canRollBackTo,
  companions,
  envNames,
  fieldName,
  groupApps,
  groupState,
  matches,
  nameFromImage,
  shortImages,
  sortGroups,
  tidyMessage,
} from "./app";

const online: ServerSummary = { id: SERVER, name: "hz-1", connection: Connection.CONNECTED, address: "49.12.33.7" };
const offline: ServerSummary = { ...online, connection: Connection.UNREACHABLE };

describe("an app's state", () => {
  const withState = (state: DeploymentState, error = "") =>
    anApp("web", "nginx", {}, { deployments: [{ serverId: SERVER, serverName: "hz-1", state, error }] });

  it("is live once its server has it, and says nothing more than that", () => {
    expect(appState(withState(DeploymentState.LIVE), [online])).toMatchObject({ kind: "live", status: "healthy" });
  });

  it("is deploying while a connected server works on it, and waits for one that isn't", () => {
    expect(appState(withState(DeploymentState.PENDING), [online]).kind).toBe("deploying");
    expect(appState(withState(DeploymentState.PENDING), [offline])).toMatchObject({
      kind: "waiting",
      status: "degraded",
      servers: ["hz-1"],
    });
  });

  it("is failed with why, whatever its server is doing", () => {
    expect(appState(withState(DeploymentState.FAILED, "it exits"), [offline])).toMatchObject({
      kind: "failed",
      status: "down",
      error: "it exits",
    });
  });

  it("doesn't vouch for an app on a server that isn't connected", () => {
    expect(appState(withState(DeploymentState.LIVE), [offline])).toMatchObject({ kind: "offline", servers: ["hz-1"] });
  });

  it("takes a server the fleet doesn't list yet as saying nothing", () => {
    expect(appState(withState(DeploymentState.LIVE), undefined).kind).toBe("live");
    expect(appState(withState(DeploymentState.LIVE), []).kind).toBe("live");
  });

  it("says when Dockwell put back what ran before", () => {
    const app = anApp("web", "nginx", {}, { rolledBackChangeId: "chg_failed" });
    expect(appState(app, [online])).toMatchObject({ kind: "rolled-back", status: "degraded" });
  });

  it("has nowhere to run once every server has left", () => {
    expect(appState(anApp("web", "nginx", {}, { deployments: [] }), [online]).kind).toBe("unplaced");
  });
});

describe("what the list shows as one thing", () => {
  it("is one stack for the services that share a network", () => {
    const groups = groupApps(aStack("web", "api", "worker", "db", "cache"));
    expect(groups).toHaveLength(1);
    expect(groups[0]?.stack).toBe(true);
    expect(groups[0]?.services.map((s) => s.resource?.name)).toEqual(["api", "cache", "db", "web", "worker"]);
  });

  it("follows what connects them, through a service on two networks, and a volume they share", () => {
    const groups = groupApps([
      anApp("web", "a", { networks: ["front"] }),
      anApp("api", "b", { networks: ["front", "back"] }),
      anApp("db", "c", { networks: ["back"] }),
      anApp("sidecar", "d", { mounts: [{ source: { case: "volume", value: "data" }, target: "/d" }] }),
      anApp("backup", "e", { mounts: [{ source: { case: "volume", value: "data" }, target: "/d" }] }),
    ]);
    expect(groups.map((g) => g.services.length).sort()).toEqual([2, 3]);
  });

  it("keeps an app alone when nothing connects it, even on the same server path", () => {
    const groups = groupApps([
      anApp("nginx", "a"),
      anApp("redis", "b"),
      anApp("x", "c", { mounts: [{ source: { case: "hostPath", value: "/srv" }, target: "/d" }] }),
      anApp("y", "d", { mounts: [{ source: { case: "hostPath", value: "/srv" }, target: "/d" }] }),
    ]);
    expect(groups).toHaveLength(4);
    expect(groups.every((g) => !g.stack)).toBe(true);
  });

  it("doesn't reach across environments: their networks are their own", () => {
    const staging = anApp(
      "web",
      "a",
      { networks: ["default"] },
      { id: "svc_s", resource: { kind: "service", scope: "env_staging", name: "web" } },
    );
    const prod = anApp("web", "a", { networks: ["default"] });
    expect(groupApps([staging, prod])).toHaveLength(2);
  });

  it("names what a service runs with", () => {
    const stack = aStack("web", "api", "db");
    const alone = anApp("nginx", "a");
    expect(companions(stack[0] ?? alone, [...stack, alone]).map((s) => s.resource?.name)).toEqual(["api", "db"]);
    expect(companions(alone, [...stack, alone])).toEqual([]);
  });

  it("takes its state from its most concerning service, and comes first when that needs you", () => {
    const stack = aStack("web", "api");
    const failing = stack[1]?.deployments[0];
    if (failing) failing.state = DeploymentState.FAILED;
    const quiet = anApp("nginx", "a", {}, { updateTime: timestampNow() });
    const groups = groupApps([quiet, ...stack]);
    const stackGroup = groups.find((g) => g.stack);
    expect(stackGroup && groupState(stackGroup, [online]).kind).toBe("failed");
    expect(sortGroups(groups, [online])[0]?.stack).toBe(true);
  });
});

describe("the words an app goes by", () => {
  it("names an app after its image, as a DNS label", () => {
    expect(nameFromImage("nginx:1.27")).toBe("nginx");
    expect(nameFromImage("ghcr.io/acme/storefront_web:2c71f09")).toBe("storefront-web");
    expect(nameFromImage("registry:5000/team/api@sha256:abc")).toBe("api");
    expect(nameFromImage("  ")).toBe("");
    expect(nameFromImage("123")).toBe("");
    expect(nameFromImage(`${"a".repeat(80)}:1`)).toHaveLength(63);
  });

  it("filters on a name or an image", () => {
    const app = anApp("web", "ghcr.io/acme/storefront:1");
    expect(matches(app, "WEB")).toBe(true);
    expect(matches(app, "storefront")).toBe(true);
    expect(matches(app, " ")).toBe(true);
    expect(matches(app, "redis")).toBe(false);
  });

  it("drops what a page about one app doesn't need from the API's messages", () => {
    expect(
      tidyMessage("Service web in env_01M6M9 isn't valid: the server path / mounted at /host is the whole file system"),
    ).toBe("The server path / mounted at /host is the whole file system");
    expect(tidyMessage("hz-1: applied service web in env_01M6M9, but it isn't healthy")).toBe(
      "Hz-1: applied web, but it isn't healthy",
    );
  });

  it("shortens an image change to the tag, where only the tag moves", () => {
    expect(shortImages("nginx:1.27", "nginx:1.28")).toBe("1.27 → 1.28");
    expect(shortImages("registry:5000/a:1", "registry:5000/a:2")).toBe("1 → 2");
    expect(shortImages("nginx:1.27", "caddy:2")).toBe("nginx:1.27 → caddy:2");
    expect(shortImages("nginx", "nginx:2")).toBe("nginx → nginx:2");
  });

  it("reads a plan's path as the service's own field", () => {
    expect(fieldName("app.image")).toBe("image");
    expect(fieldName("app.env[KEY]")).toBe("env[KEY]");
  });

  it("lists variables by name", () => {
    expect(envNames(anApp("web", "a", { env: { Z: "", A: "" } }).spec?.runs.value)).toEqual(["A", "Z"]);
    expect(envNames(undefined)).toEqual([]);
  });

  it("won't roll back to a revision that failed or deleted the service", () => {
    expect(canRollBackTo(aRevision(2n, "a"))).toBe(true);
    expect(canRollBackTo(aRevision(2n, "a", { deleted: true }))).toBe(false);
    expect(
      canRollBackTo(aRevision(2n, "a", { deployments: [{ serverId: SERVER, state: DeploymentState.FAILED }] })),
    ).toBe(false);
  });
});
