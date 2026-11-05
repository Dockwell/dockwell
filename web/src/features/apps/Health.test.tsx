// SPDX-License-Identifier: AGPL-3.0-only

import { create, type MessageInitShape } from "@bufbuild/protobuf";
import { Code, ConnectError } from "@connectrpc/connect";
import {
  ChangeSchema,
  ChangeState,
  DeploymentState,
  EventKind,
  EventSchema,
  HealthCheckKind,
  HealthState,
  type ListEventsRequest,
  PlanAction,
  type RestartServiceRequest,
  type WatchChangeResponse,
  type WatchServersResponse,
} from "@dockwell/api";
import { screen, waitFor, within } from "@testing-library/react";
import userEvent from "@testing-library/user-event";
import { afterEach, describe, expect, it, vi } from "vitest";

import { activateLocale } from "../../app/i18n";
import { expectAccessible } from "../../test/a11y";
import { aHealth, anApp, aRevision, type HealthOn, places, running, SERVER } from "../../test/apps";
import { type Api, feed, onPhone, renderAt } from "../../test/render";
import { aServer, changed, snapshotComplete } from "../../test/servers";

const hz1 = aServer({ id: SERVER, name: "hz-1", inventory: { addresses: ["49.12.33.7"] } });
const hz2 = aServer({ id: "srv_2", name: "hz-2", inventory: { addresses: ["49.12.33.8"] } });
const web = anApp("web", "ghcr.io/acme/web:1.5");

function api(services = [web], health: Api["health"] = running(web), more: Api = {}, ...servers: ReturnType<typeof aServer>[]): Api {
  const watch = feed<WatchServersResponse>();
  watch.push(...(servers.length ? servers : [hz1]).map(changed), snapshotComplete);
  return {
    resources: places(),
    services: { listServices: () => ({ services }), listServiceRevisions: () => ({ revisions: [aRevision(3n, "web:1.5")] }) },
    servers: { watchServers: watch.stream },
    changes: { watchChange: feed<WatchChangeResponse>().stream, discardChange: () => ({}) },
    health,
    ...more,
  };
}

const reading = (...on: HealthOn[]): Api["health"] => ({ listHealth: () => ({ services: [aHealth("svc_web", ...on)] }) });

/** Each state, with its word, its tone, what it says, and whether it asks for a look. */
const STATES: readonly {
  on: HealthOn;
  word: string;
  tone: string;
  says: RegExp;
  notice?: RegExp;
}[] = [
  {
    on: { state: HealthState.HEALTHY, said: "GET /healthz on port 8080 answered 200 OK", passed: true },
    word: "Healthy",
    tone: "dw-tone-healthy",
    says: /GET \/healthz on port 8080 answered 200 OK/,
  },
  {
    on: { state: HealthState.NO_CHECK },
    word: "Running",
    tone: "dw-tone-ink",
    says: /Nothing checks it, so this only means it started/,
  },
  {
    on: { state: HealthState.STARTING },
    word: "Starting",
    tone: "dw-tone-cobalt",
    says: /Its check hasn't finished yet/,
  },
  {
    on: { state: HealthState.UNHEALTHY, said: "GET /healthz on port 8080 answered 503 Service Unavailable", failing: 3 },
    word: "Unhealthy",
    tone: "dw-tone-warning",
    says: /answered 503 Service Unavailable/,
    notice: /web fails its health check/,
  },
  {
    on: { state: HealthState.NOT_RUNNING, said: "the container isn't running: it is exited, with exit code 137" },
    word: "Not running",
    tone: "dw-tone-critical",
    says: /The container isn't running: it is exited, with exit code 137/,
    notice: /web isn't running/,
  },
  {
    on: { state: HealthState.PAUSED, said: "the container is paused" },
    word: "Paused",
    tone: "dw-tone-idle",
    says: /Someone paused it, so it serves nothing and no check runs/,
    notice: /web is paused/,
  },
  {
    on: { state: HealthState.UNKNOWN, lastSaid: HealthState.HEALTHY },
    word: "Unknown",
    tone: "dw-tone-idle",
    says: /hz-1 hasn't said how it is lately. It last read Healthy/,
    notice: /Nobody can say how web runs/,
  },
];

afterEach(() => {
  vi.unstubAllGlobals();
  localStorage.clear();
});

describe("what runs, from what the server says", () => {
  it.each(STATES)("reads $word on the list, its overview and a phone, kept apart from the record", async ({ on, word, tone, says, notice }) => {
    const first = renderAt("/apps", api([web], reading(on)));
    const row = await screen.findByRole("row", { name: /web/ });
    await waitFor(() => expect(row).toHaveTextContent(word));
    expect(row.querySelector(".apps-status")).toHaveClass(tone);
    // The record isn't what runs, and says nothing while it has nothing to add.
    expect(row).not.toHaveTextContent("Live");
    expect(row).toHaveTextContent("r3");
    // Only a check that passed is green.
    if (word !== "Healthy") expect(row.querySelector(".dw-tone-healthy")).toBeNull();
    await expectAccessible(first.container);
    first.unmount();

    const page = renderAt("/apps/svc_web", api([web], reading(on)));
    const panel = await screen.findByRole("region", { name: "What runs" });
    await waitFor(() => expect(panel).toHaveTextContent(word));
    expect(panel).toHaveTextContent(says);
    const lead = page.container.querySelector(".page-head, .page") as HTMLElement;
    expect(lead).toHaveTextContent(word);
    if (notice) expect(page.container.querySelector(".dw-callout")).toHaveTextContent(notice);
    else expect(page.container.querySelector(".dw-callout")).toBeNull();
    await expectAccessible(page.container);
    page.unmount();

    onPhone();
    renderAt("/apps", api([web], reading(on)));
    const link = await screen.findByRole("link", { name: /web/ });
    await waitFor(() => expect(link).toHaveTextContent(word));
    expect(link.querySelector(".apps-phone-glyph")).toHaveClass(tone);
  });

  it("says how long it has been so, and when its check last said so", async () => {
    renderAt(
      "/apps/svc_web",
      api([web], reading({ state: HealthState.HEALTHY, said: "GET / on port 80 answered 200 OK", passed: true, forMs: 180_000 })),
    );
    const panel = await screen.findByRole("region", { name: "What runs" });
    await waitFor(() => expect(panel).toHaveTextContent("for 3 min"));
    expect(panel).toHaveTextContent(/answered 200 OK\. +\d+ s ago/);
  });

  it("gives each server its own word and words, and says where something's wrong, from the second on", async () => {
    const both = anApp(
      "web",
      "nginx",
      {},
      {
        deployments: [
          { serverId: SERVER, serverName: "hz-1", state: DeploymentState.LIVE },
          { serverId: "srv_2", serverName: "hz-2", state: DeploymentState.LIVE },
        ],
      },
    );
    const { container } = renderAt(
      "/apps/svc_web",
      api(
        [both],
        reading(
          { state: HealthState.HEALTHY, said: "GET / on port 80 answered 200 OK", passed: true },
          { state: HealthState.NOT_RUNNING, server: "srv_2", serverName: "hz-2", said: "the container isn't running: it is exited" },
        ),
        {},
        hz1,
        hz2,
      ),
    );
    const panel = await screen.findByRole("region", { name: "What runs" });
    const rows = await within(panel).findAllByRole("listitem");
    const [first, second] = rows;
    expect(first).toHaveTextContent("hz-1");
    expect(first).toHaveTextContent("Healthy");
    expect(second).toHaveTextContent("hz-2");
    expect(second).toHaveTextContent("Not running");
    // As a whole it's as well as its worst.
    expect(container.querySelector(".dw-callout")).toHaveTextContent("web isn't running on hz-2");
    await expectAccessible(container);
  });

  it("never reads a failed read as not running, and never as green", async () => {
    renderAt("/apps", api([web], {
      listHealth: () => {
        throw new ConnectError("the control plane is restarting", Code.Unavailable);
      },
    }));
    const row = await screen.findByRole("row", { name: /web/ });
    await waitFor(() => expect(row).toHaveTextContent("Unknown"));
    expect(row).not.toHaveTextContent("Not running");
    expect(row.querySelector(".dw-tone-healthy")).toBeNull();
    expect(row.querySelector(".dw-tone-critical")).toBeNull();
    // It isn't the app that needs a look.
    expect(screen.queryByText(/needs? a look/)).not.toBeInTheDocument();
  });

  it("says on an app's page that it can't read, with no notice of what's wrong with the app", async () => {
    const { container } = renderAt("/apps/svc_web", api([web], {
      listHealth: () => {
        throw new ConnectError("no", Code.Unavailable);
      },
    }));
    const panel = await screen.findByRole("region", { name: "What runs" });
    await waitFor(() => expect(panel).toHaveTextContent("Dockwell can't read how this runs right now"));
    expect(panel).toHaveTextContent("That says nothing about the app.");
    expect(container.querySelector(".dw-callout")).toBeNull();
  });

  it("doesn't flash not running for what a deploy has just made, before its agent has looked", async () => {
    const fresh = anApp(
      "web",
      "nginx",
      {},
      { deployments: [{ serverId: SERVER, serverName: "hz-1", state: DeploymentState.LIVE, updateTime: { seconds: BigInt(Math.floor(Date.now() / 1000)), nanos: 0 } }] },
    );
    renderAt("/apps", api([fresh], reading({ state: HealthState.NOT_RUNNING, scannedMsAgo: 5_000 })));
    const row = await screen.findByRole("row", { name: /web/ });
    await waitFor(() => expect(row).toHaveTextContent("Checking"));
    expect(row).not.toHaveTextContent("Not running");
  });

  it("reads once for a list of many apps, and shares it with what's open", async () => {
    let reads = 0;
    const many = Array.from({ length: 40 }, (_, i) => anApp(`app-${i}`, "nginx"));
    renderAt("/apps", api(many, {
      listHealth: () => {
        reads += 1;
        return { services: many.map((s) => aHealth(s.id, { state: HealthState.NO_CHECK })) };
      },
    }));
    await waitFor(() => expect(screen.getAllByText("Running").length).toBeGreaterThan(30));
    expect(reads).toBe(1);
  });

  it("comes first when it needs a look, whatever its record says, and counts in the line under the title", async () => {
    const quiet = anApp("quiet", "a");
    const broken = anApp("broken", "b", {}, { updateTime: { seconds: 1n, nanos: 0 } });
    renderAt("/apps", api([quiet, broken], {
      listHealth: () => ({
        services: [aHealth("svc_quiet", { state: HealthState.NO_CHECK }), aHealth("svc_broken", { state: HealthState.NOT_RUNNING })],
      }),
    }));
    expect(await screen.findByText(/1 needs a look/)).toBeVisible();
    const rows = screen.getAllByRole("row");
    expect(rows[1]).toHaveTextContent("broken");
  });

  it("says a rollback stands where it runs, and what it can't say where it doesn't", async () => {
    const back = anApp("web", "nginx:1.4", {}, { rolledBackChangeId: "chg_failed" });
    const page = renderAt("/apps/svc_web", api([back]));
    const said = await screen.findByRole("status");
    await waitFor(() => expect(said).toHaveTextContent("web is on nginx:1.4, as it was before that deploy."));
    page.unmount();

    renderAt("/apps/svc_web", api([back], reading({ state: HealthState.NOT_RUNNING })));
    const stopped = await screen.findByRole("status");
    await waitFor(() => expect(stopped).toHaveTextContent("Dockwell put nginx:1.4 back, as it was before that deploy."));
    expect(stopped).toHaveTextContent("web isn't running now.");
    expect(stopped).not.toHaveTextContent("web is on nginx:1.4");
  });

  it("says what runs after a rollback that failed, as far as the server says", async () => {
    const stuck = anApp(
      "web",
      "nginx:slow",
      {},
      {
        rolledBackChangeId: "chg_failed",
        changeState: ChangeState.FAILED,
        deployments: [{ serverId: SERVER, serverName: "hz-1", state: DeploymentState.FAILED, error: "hz-1: no daemon" }],
      },
    );
    renderAt("/apps/svc_web", api([stuck], reading({ state: HealthState.NOT_RUNNING })));
    const alert = await screen.findByRole("alert");
    await waitFor(() => expect(alert).toHaveTextContent("web isn't running now."));
    expect(alert).not.toHaveTextContent("may not be running");
  });

  it("makes no claim that an offline server's app keeps running", async () => {
    const away = aServer({ id: SERVER, name: "hz-1", inventory: { addresses: [] }, connection: 2 });
    renderAt("/apps/svc_web", api([web], reading({ state: HealthState.UNKNOWN }), {}, away));
    const notice = await screen.findByRole("status");
    await waitFor(() => expect(notice).toHaveTextContent("It may well be running"));
    expect(notice).not.toHaveTextContent("keeps running");
  });

  it("speaks Chinese, and the agent's words stay as it wrote them", async () => {
    await activateLocale("zh-CN");
    renderAt("/apps/svc_web", api([web], reading({ state: HealthState.PAUSED })));
    const panel = await screen.findByRole("region", { name: "运行内容" });
    await waitFor(() => expect(panel).toHaveTextContent("已暂停"));
    await activateLocale("en");
  });
});

describe("a health check on the overview", () => {
  const facts = async (app: Parameters<typeof anApp>[2], health: Api["health"] = running(web)) => {
    const { unmount } = renderAt("/apps/svc_web", api([anApp("web", "nginx", app)], health));
    const panel = await screen.findByRole("region", { name: "Configuration" });
    const term = within(panel).getByText("Health check");
    return { said: term.nextElementSibling as HTMLElement, unmount };
  };

  it("says what an HTTP check asks", async () => {
    const { said } = await facts({ healthcheck: { http: { port: 8080, path: "/healthz" } } });
    expect(said).toHaveTextContent("GET /healthz on port 8080");
  });

  it("says what a connection check asks", async () => {
    const { said } = await facts({ healthcheck: { tcp: { port: 5432 } } });
    expect(said).toHaveTextContent("A connection to port 5432");
  });

  it("says what a command asks", async () => {
    const { said } = await facts({ healthcheck: { test: ["CMD", "curl", "-f", "http://localhost"] } });
    expect(said).toHaveTextContent("curl -f http://localhost");
  });

  it("says the image has one, where the server says so and the spec doesn't", async () => {
    const { said } = await facts({}, reading({ state: HealthState.HEALTHY, check: HealthCheckKind.COMMAND }));
    expect(said).toHaveTextContent("The image's own");
  });

  it("is honest where there's none: Running only means it started", async () => {
    const { said } = await facts({});
    await waitFor(() => expect(said).toHaveTextContent("None. Nothing checks it, so this only means it started."));
  });

  it("says one turned off is off", async () => {
    const { said } = await facts({ healthcheck: { test: ["NONE"] } });
    expect(said).toHaveTextContent("Turned off.");
  });

  it("doesn't say there's none until it knows", async () => {
    const { said } = await facts({}, { listHealth: () => new Promise(() => {}) });
    expect(said).toHaveTextContent("None set here. The image may have one.");
  });
});

describe("restarting an app", () => {
  const plan = (rolling: boolean) =>
    create(ChangeSchema, {
      id: "chg_restart",
      summary: "Restart web",
      state: ChangeState.PLANNED,
      restart: true,
      plan: {
        fingerprint: "feedfacefeedfacefeedfacefeedface",
        summary: { replace: 1 },
        undoWindow: { seconds: 604_800n, nanos: 0 },
        resources: [{ id: "svc_web", resource: { kind: "service", scope: "env_prod", name: "web" }, action: PlanAction.REPLACE, restart: true }],
        servers: [
          {
            serverId: SERVER,
            serverName: "hz-1",
            ...(rolling ? {} : { downtime: { seconds: 13n, nanos: 0 } }),
            steps: [
              {
                resourceId: "svc_web",
                resource: { kind: "service", scope: "env_prod", name: "web" },
                action: PlanAction.REPLACE,
                restart: "chg_restart",
                ...(rolling ? {} : { downtime: { seconds: 13n, nanos: 0 } }),
                rollout: { rolling, healthyWithin: { seconds: 60n, nanos: 0 }, ...(rolling ? {} : { ports: ["8080/tcp"] }) },
              },
            ],
          },
        ],
      },
    } as MessageInitShape<typeof ChangeSchema>);

  it.each([
    { rolling: true, reads: /A new container takes over/ },
    { rolling: false, reads: /about 13 s/ },
  ])("asks for a plan, says what it does for this app, and has no undo (rolling: $rolling)", async ({ rolling, reads }) => {
    const user = userEvent.setup();
    const asked: RestartServiceRequest[] = [];
    renderAt("/apps/svc_web", api([web], running(web), {
      changes: {
        watchChange: feed<WatchChangeResponse>().stream,
        discardChange: () => ({}),
        restartService: (req) => {
          asked.push(req);
          return { change: plan(rolling) };
        },
      },
    }));
    await user.click(await screen.findByRole("button", { name: "Restart" }));
    const sheet = await screen.findByRole("dialog", { name: "Review plan" });
    expect(asked[0]?.id).toBe("svc_web");
    expect(sheet).toHaveTextContent(reads);
    expect(sheet).toHaveTextContent("No undo, as a restart changes nothing to go back to");
    expect(sheet).not.toHaveTextContent(/Undo for \d+ days/);
    await expectAccessible(document.body);
  });

  it("isn't offered while it's moving, or where it has no server", async () => {
    const moving = anApp(
      "web",
      "nginx",
      {},
      { changeId: "chg_1", changeState: ChangeState.APPLYING, deployments: [{ serverId: SERVER, serverName: "hz-1", state: DeploymentState.PENDING }] },
    );
    const page = renderAt("/apps/svc_web", api([moving]));
    await screen.findByRole("heading", { level: 1, name: "web" });
    expect(screen.queryByRole("button", { name: "Restart" })).not.toBeInTheDocument();
    page.unmount();
    renderAt("/apps/svc_web", api([anApp("web", "nginx", {}, { deployments: [] })]));
    await screen.findByRole("heading", { level: 1, name: "web" });
    expect(screen.queryByRole("button", { name: "Restart" })).not.toBeInTheDocument();
  });
});

describe("how it has run lately, on its page", () => {
  const event = (id: string, state: HealthState, flips: number, output: string) =>
    create(EventSchema, {
      id,
      kind: EventKind.HEALTH,
      action: "service.health",
      time: { seconds: BigInt(Math.floor(Date.now() / 1000) - 600), nanos: 0 },
      subject: { id: "svc_web", name: "web" },
      health: { serverId: SERVER, serverName: "hz-1", state, previous: HealthState.HEALTHY, flips, output },
    });

  it("asks for this app's health alone, a few at a time, and says how a flapping app has gone in one line", async () => {
    const asked: ListEventsRequest[] = [];
    const { container } = renderAt("/apps/svc_web", api([web], running(web), {
      events: {
        listEvents: (req) => {
          asked.push(req);
          return { events: [event("evt_1", HealthState.UNHEALTHY, 7, "GET / answered 503")] };
        },
      },
    }));
    const panel = await screen.findByRole("region", { name: "Recent health" });
    const health = asked.find((r) => r.kinds.includes(EventKind.HEALTH));
    expect(health?.resourceId).toBe("svc_web");
    expect(health?.pageSize).toBeLessThanOrEqual(5);
    expect(await within(panel).findAllByRole("listitem")).toHaveLength(1);
    expect(panel).toHaveTextContent("web turned unhealthy on hz-1");
    expect(panel).toHaveTextContent("GET / answered 503");
    expect(panel).toHaveTextContent("It changed 7 more times since the last time this said.");
    expect(panel.querySelector(".dw-tone-warning")).not.toBeNull();
    await expectAccessible(container);
  });

  it("says nothing while there's nothing to say", async () => {
    renderAt("/apps/svc_web", api());
    await screen.findByRole("region", { name: "What runs" });
    expect(screen.queryByRole("region", { name: "Recent health" })).not.toBeInTheDocument();
  });
});
