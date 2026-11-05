// SPDX-License-Identifier: AGPL-3.0-only

import { create } from "@bufbuild/protobuf";
import { timestampNow } from "@bufbuild/protobuf/wkt";
import { ConnectError } from "@connectrpc/connect";
import {
  CertificateState,
  ChangeSchema,
  ChangeState,
  Connection,
  DeploymentState,
  DomainSchema,
  FieldOp,
  ServiceInfoSchema,
} from "@dockwell/api";
import { describe, expect, it } from "vitest";

import { sentence } from "../../lib/errors";
import { anApp, aProject, aRevision, SERVER } from "../../test/apps";
import type { ServerSummary } from "../servers/server";
import {
  type AppGroup,
  appState,
  bareImage,
  canRollBackTo,
  companions,
  fieldName,
  groupApps,
  groupState,
  imageChange,
  imageOf,
  keepOrder,
  matches,
  nameFromImage,
  type PlaceOf,
  portRefusal,
  shortDigest,
  shortImage,
  shortImages,
  sortGroups,
  tidyMessage,
  toLabel,
  unreached,
  valuesOf,
} from "./app";
import { lookOf, proxyList, routeName } from "./domains";

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

  it("doesn't say live before it knows, while the fleet hasn't answered", () => {
    expect(appState(withState(DeploymentState.LIVE), undefined)).toMatchObject({ kind: "unknown", status: "stopped" });
  });

  it("isn't placed once each server it ran on has left the fleet", () => {
    expect(appState(withState(DeploymentState.LIVE), [])).toMatchObject({ kind: "unplaced", status: "stopped" });
    const on = (id: string, name: string) => ({ serverId: id, serverName: name, state: DeploymentState.LIVE });
    const both = anApp("web", "nginx", {}, { deployments: [on(SERVER, "hz-1"), on("srv_gone", "hz-2")] });
    expect(appState(both, [online]).kind).toBe("live");
  });

  it("is failed, not deploying forever, once its change has failed", () => {
    const stuck = anApp(
      "web",
      "nginx",
      {},
      {
        changeState: ChangeState.FAILED,
        deployments: [{ serverId: SERVER, serverName: "hz-1", state: DeploymentState.PENDING }],
      },
    );
    expect(appState(stuck, [online]).kind).toBe("failed");
  });

  it("says when Dockwell put back what ran before", () => {
    const app = anApp("web", "nginx", {}, { rolledBackChangeId: "chg_failed" });
    expect(appState(app, [online])).toMatchObject({ kind: "rolled-back", status: "degraded" });
  });

  it("has nowhere to run when no revision placed it", () => {
    expect(appState(anApp("web", "nginx", {}, { deployments: [] }), [online]).kind).toBe("unplaced");
  });
});

describe("what the list shows as one thing", () => {
  const placeOf = (id: string): PlaceOf | undefined =>
    id === "env_prod"
      ? { label: "default", isDefault: true }
      : id.startsWith("env_")
        ? { label: id.slice(4), isDefault: false }
        : undefined;

  it("is one row for a project's services, by its name", () => {
    const groups = groupApps(aProject("shop", "web", "api", "worker", "db", "cache"), placeOf);
    expect(groups).toHaveLength(1);
    expect(groups[0]).toMatchObject({ stack: true, name: "shop", key: "env_shop" });
    expect(groups[0]?.services.map((s) => s.resource?.name)).toEqual(["api", "cache", "db", "web", "worker"]);
  });

  it("keeps two projects apart, whatever their services are called and however they connect", () => {
    const groups = groupApps([...aProject("blog", "db", "web"), ...aProject("git", "db", "web")], placeOf);
    expect(groups.map((g) => g.name)).toEqual(["blog", "git"]);
    expect(groups.map((g) => g.services.length)).toEqual([2, 2]);
  });

  it("keeps an app of the default project a row of its own, connected or not", () => {
    const groups = groupApps(
      [anApp("nginx", "a", { networks: ["default"] }), anApp("redis", "b", { networks: ["default"] })],
      placeOf,
    );
    expect(groups).toHaveLength(2);
    expect(groups.every((g) => !g.stack && g.name === "")).toBe(true);
  });

  it("calls a project of one service by its name, with nothing to fold", () => {
    const [group] = groupApps(aProject("blog", "web"), placeOf);
    expect(group).toMatchObject({ stack: false, name: "blog" });
  });

  it("leaves an app alone while where it lives isn't known", () => {
    expect(groupApps(aProject("blog", "web", "db"), () => undefined)).toHaveLength(2);
  });

  it("runs with the rest of its project, and in the default project with what it shares a network or volume with", () => {
    const [web, api, db] = aProject("shop", "web", "api", "db");
    if (!web || !api || !db) throw new Error("no project");
    expect(companions(web, [web, api, db], placeOf).map((s) => s.resource?.name)).toEqual(["api", "db"]);
    const a = anApp("a", "x", { networks: ["front"] });
    const b = anApp("b", "x", { networks: ["front"] });
    const shared = (name: string) =>
      anApp(name, "x", { mounts: [{ source: { case: "volume", value: "data" }, target: "/d" }] });
    const alone = anApp("alone", "x");
    const all = [a, b, shared("c"), shared("d"), alone];
    expect(companions(a, all, placeOf).map((s) => s.resource?.name)).toEqual(["b"]);
    expect(companions(shared("c"), all, placeOf).map((s) => s.resource?.name)).toEqual(["d"]);
    expect(companions(alone, all, placeOf)).toEqual([]);
  });

  it("takes its state from its most concerning service, and comes first when that needs you", () => {
    const stack = aProject("shop", "web", "api");
    const failing = stack[1]?.deployments[0];
    if (failing) failing.state = DeploymentState.FAILED;
    const quiet = anApp("nginx", "a", {}, { updateTime: timestampNow() });
    const groups = groupApps([quiet, ...stack], placeOf);
    const stackGroup = groups.find((g) => g.stack);
    expect(stackGroup && groupState(stackGroup, [online]).kind).toBe("failed");
    expect(sortGroups(groups, [online])[0]?.stack).toBe(true);
  });
});

describe("a server's refusal, as a sentence", () => {
  it("leaves a domain or an address written as it is, where it leads", () => {
    expect(tidyMessage("app.example.com. ends in a dot")).toBe("app.example.com. ends in a dot");
    expect(tidyMessage("https://app.example.com isn't a domain")).toBe("https://app.example.com isn't a domain");
    expect(tidyMessage("a route needs a domain")).toBe("A route needs a domain");
    expect(sentence(new ConnectError("app.example.com. ends in a dot"))).toBe("app.example.com. ends in a dot.");
    expect(sentence(new ConnectError("a route needs a domain"))).toBe("A route needs a domain.");
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
      "hz-1: applied web, but it isn't healthy",
    );
  });

  it("shortens an image change to the tag, where only the tag moves", () => {
    expect(shortImages("nginx:1.27", "nginx:1.28")).toBe("1.27 → 1.28");
    expect(shortImages("registry:5000/a:1", "registry:5000/a:2")).toBe("1 → 2");
    expect(shortImages("nginx:1.27", "caddy:2")).toBe("nginx:1.27 → caddy:2");
    expect(shortImages("nginx", "nginx:2")).toBe("nginx → nginx:2");
    expect(imageChange("getmeili/meilisearch:v1.11", "getmeili/meilisearch:broken")).toEqual({
      from: "v1.11",
      to: "broken",
    });
    expect(imageChange("a.example/x/api:1", "b.example/y/web:2")).toEqual({ from: "api:1", to: "web:2" });
  });

  it("tells an image apart by the end of its name on a narrow screen", () => {
    expect(shortImage("registry.internal.acme.example.com/platform/backend:1.4.2")).toBe("backend:1.4.2");
    expect(shortImage("nginx:1.27")).toBe("nginx:1.27");
  });

  it("makes a name into one a project can have", () => {
    expect(toLabel("my_app")).toBe("my-app");
    expect(toLabel("1panel-wordpress")).toBe("panel-wordpress");
    expect(toLabel("Shop")).toBe("shop");
    expect(toLabel("___")).toBe("");
  });

  it("reads a plan's path as the service's own field", () => {
    expect(fieldName("app.image")).toBe("image");
    expect(fieldName("app.env[KEY]")).toBe("env[KEY]");
  });

  it("lists variables by name", () => {
    const service = anApp("web", "a", { env: { Z: "", A: "" } });
    expect(valuesOf(service).map((v) => v.name)).toEqual(["A", "Z"]);
    expect(valuesOf(create(ServiceInfoSchema))).toEqual([]);
    // What it uses, by the names it sees, beside its own.
    if (service.spec) {
      service.spec.variables = { LEVEL: "LOG_LEVEL", TZ: "TZ" };
      service.spec.secrets = { KEY: "API_KEY" };
    }
    expect(valuesOf(service)).toEqual([
      { name: "A", kind: "own", from: "A" },
      { name: "KEY", kind: "secret", from: "API_KEY" },
      { name: "LEVEL", kind: "variable", from: "LOG_LEVEL" },
      { name: "TZ", kind: "variable", from: "TZ" },
      { name: "Z", kind: "own", from: "Z" },
    ]);
  });

  it("says which new services a plan leaves out of reach", () => {
    const change = create(ChangeSchema, {
      plan: {
        resources: [
          { resource: { kind: "network", name: "default" } },
          { resource: { kind: "service", name: "db" }, changes: [{ path: "app.image", op: FieldOp.ADDED }] },
          { resource: { kind: "service", name: "web" }, changes: [{ path: "app.ports[0]", op: FieldOp.ADDED }] },
          { resource: { kind: "service", name: "old" }, baseRevision: 2n },
        ],
      },
    });
    expect(unreached(change)).toEqual(["db"]);
    expect(unreached(undefined)).toEqual([]);
  });

  it("won't roll back to a revision that failed, deleted the service, or matches what runs", () => {
    expect(canRollBackTo(aRevision(2n, "a"))).toBe(true);
    expect(canRollBackTo(aRevision(2n, "a", { deleted: true }))).toBe(false);
    expect(canRollBackTo(aRevision(2n, "a", { matchesLatest: true }))).toBe(false);
    // While recovering, the record's word isn't enough to rule one out.
    expect(canRollBackTo(aRevision(2n, "a", { matchesLatest: true }), true)).toBe(true);
    expect(canRollBackTo(aRevision(2n, "a", { deleted: true, matchesLatest: true }), true)).toBe(false);
    expect(
      canRollBackTo(
        aRevision(2n, "a", { matchesLatest: true, deployments: [{ serverId: SERVER, state: DeploymentState.FAILED }] }),
        true,
      ),
    ).toBe(false);
    expect(
      canRollBackTo(aRevision(2n, "a", { deployments: [{ serverId: SERVER, state: DeploymentState.FAILED }] })),
    ).toBe(false);
  });

  it("reads a port another holds out of the agent's words", () => {
    expect(
      portRefusal(
        "Hz-1: can't plan service web in env_01abc: holder, in another environment, publishes port 8080, so web can't. Choose another port on the server, or free that one first.",
      ),
    ).toBe(
      "Holder, in another environment, publishes port 8080, so web can't. Choose another port on the server, or free that one first.",
    );
    expect(portRefusal("can't plan c: b also publishes port 53/udp in this same change, and only one can")).toBe(
      "B also publishes port 53/udp in this same change, and only one can",
    );
    expect(portRefusal("can't plan c: nginx publishes port 80 on :: (every IPv6 address), so c can't")).toBe(
      "Nginx publishes port 80 on :: (every IPv6 address), so c can't",
    );
    expect(portRefusal("the image isn't on the registry")).toBeUndefined();
    expect(portRefusal("use a port from 1 to 65535")).toBeUndefined();
  });

  it("keeps a list's order until the page is next opened", () => {
    const group = (key: string): AppGroup => ({ key, environment: key, name: key, services: [], stack: false });
    const first = keepOrder([], [group("a"), group("b"), group("c")]);
    expect(first.groups.map((g) => g.key)).toEqual(["a", "b", "c"]);
    // A deploy would pull c to the top of what is sorted, and it stays where it is.
    const next = keepOrder(first.order, [group("c"), group("a"), group("b")]);
    expect(next.groups.map((g) => g.key)).toEqual(["a", "b", "c"]);
    // One that is new comes first, and one a filter hides keeps its place.
    const fresh = keepOrder(next.order, [group("d"), group("c"), group("a")]);
    expect(fresh.groups.map((g) => g.key)).toEqual(["d", "a", "c"]);
    expect(keepOrder(fresh.order, [group("b"), group("a")]).groups.map((g) => g.key)).toEqual(["a", "b"]);
  });

  it("reads an image without the digest a rollback pinned it to", () => {
    const digest = `sha256:${"ab".repeat(32)}`;
    expect(bareImage(`nginx:1.27@${digest}`)).toBe("nginx:1.27");
    expect(bareImage("nginx:1.27")).toBe("nginx:1.27");
    expect(imageOf(anApp("web", `nginx:1.27@${digest}`))).toBe("nginx:1.27");
    expect(shortDigest(digest)).toBe("sha256:abababababab");
  });
});

describe("a route's name", () => {
  const domain = (name: string, ...routes: string[]) =>
    create(DomainSchema, { name, routes: routes.map((route) => ({ name: route })) });

  it("is the domain as a DNS label", () => {
    expect(routeName("app.example.com")).toBe("app-example-com");
    expect(routeName("1.example.com")).toBe("example-com");
    const long = `${"a".repeat(60)}.example.com`;
    expect(routeName(long)).toBe(`${"a".repeat(60)}-ex`);
    expect(routeName(long).length).toBeLessThanOrEqual(63);
  });

  it("doesn't take another domain's, which would move that route", () => {
    expect(routeName("a.b.com", [domain("a-b.com", "a-b-com")])).toBe("a-b-com-2");
    expect(routeName("a.b.com", [domain("a-b.com", "a-b-com", "a-b-com-2")])).toBe("a-b-com-3");
    // Its own is its own: proposing it again changes where it sends.
    expect(routeName("a.b.com", [domain("a.b.com", "a-b-com")])).toBe("a-b-com");
  });
});

describe("where a domain stands", () => {
  const on = (...certificates: { state: CertificateState; message?: string }[]) =>
    create(DomainSchema, {
      name: "a.com",
      servers: certificates.map((certificate) => ({ status: { domain: "a.com", certificate } })),
    });

  it("is the worst of its servers", () => {
    expect(lookOf(on({ state: CertificateState.ISSUED })).state).toBe("serving");
    expect(
      lookOf(on({ state: CertificateState.ISSUED }, { state: CertificateState.PENDING, message: "soon" })),
    ).toEqual({
      state: "pending",
      message: "soon",
    });
    expect(
      lookOf(on({ state: CertificateState.PENDING }, { state: CertificateState.FAILED, message: "no DNS" })),
    ).toEqual({
      state: "failed",
      message: "no DNS",
    });
  });

  it("serves with no certificate behind a tunnel, as there's none to wait for", () => {
    expect(lookOf(on({ state: CertificateState.NONE }))).toEqual({
      state: "serving",
      message: "",
      noCertificate: true,
    });
    expect(lookOf(on({ state: CertificateState.ISSUED })).noCertificate).toBeUndefined();
    // One server still getting its certificate is still pending.
    expect(lookOf(on({ state: CertificateState.NONE }, { state: CertificateState.PENDING, message: "soon" }))).toEqual({
      state: "pending",
      message: "soon",
    });
  });

  it("reads the proxies a person wrote", () => {
    expect(proxyList("")).toEqual([]);
    expect(proxyList("  ")).toEqual([]);
    expect(proxyList("cloudflare")).toEqual(["cloudflare"]);
    expect(proxyList("203.0.113.0/24, 198.51.100.7\n  2001:db8::1 ,")).toEqual([
      "203.0.113.0/24",
      "198.51.100.7",
      "2001:db8::1",
    ]);
  });

  it("isn't serving before a server has said so", () => {
    expect(lookOf(create(DomainSchema, { name: "a.com" })).state).toBe("pending");
    expect(lookOf(create(DomainSchema, { name: "a.com", servers: [{ serverName: "hz-1" }] })).state).toBe("pending");
  });

  it("says what stops its proxy, even with a certificate", () => {
    const blocked = create(DomainSchema, {
      name: "a.com",
      servers: [
        {
          status: { domain: "a.com", certificate: { state: CertificateState.ISSUED } },
          ingress: { message: "1Panel-openresty holds port 80" },
        },
      ],
    });
    expect(lookOf(blocked)).toEqual({ state: "failed", message: "1Panel-openresty holds port 80" });
  });
});
