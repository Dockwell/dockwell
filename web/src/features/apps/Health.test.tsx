// SPDX-License-Identifier: AGPL-3.0-only

import { create, type MessageInitShape } from "@bufbuild/protobuf";
import { timestampFromDate } from "@bufbuild/protobuf/wkt";
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
  RestartPolicy,
  type RestartServiceRequest,
  type WatchChangeResponse,
  type WatchEventsResponse,
  type WatchServersResponse,
  WatchServersResponseSchema,
} from "@dockwell/api";
import { focusManager, onlineManager } from "@tanstack/react-query";
import { screen, waitFor, within } from "@testing-library/react";
import userEvent from "@testing-library/user-event";
import { afterEach, describe, expect, it, vi } from "vitest";

import { activateLocale } from "../../app/i18n";
import { expectAccessible } from "../../test/a11y";
import { aHealth, anApp, aRevision, type HealthOn, places, running, SERVER } from "../../test/apps";
import { changeEvent, moved, since, snapshot, webPlanned } from "../../test/changes";
import { type Api, feed, onPhone, renderAt, untilAborted } from "../../test/render";
import { aServer, changed, snapshotComplete } from "../../test/servers";

const hz1 = aServer({ id: SERVER, name: "hz-1", inventory: { addresses: ["49.12.33.7"] } });
const hz2 = aServer({ id: "srv_2", name: "hz-2", inventory: { addresses: ["49.12.33.8"] } });
const web = anApp("web", "ghcr.io/acme/web:1.5");

function api(
  services = [web],
  health: Api["health"] = running(web),
  more: Api = {},
  ...servers: ReturnType<typeof aServer>[]
): Api {
  const watch = feed<WatchServersResponse>();
  watch.push(...(servers.length ? servers : [hz1]).map(changed), snapshotComplete);
  return {
    resources: places(),
    services: {
      listServices: () => ({ services }),
      listServiceRevisions: () => ({ revisions: [aRevision(3n, "web:1.5")] }),
    },
    servers: { watchServers: watch.stream },
    changes: { watchChange: feed<WatchChangeResponse>().stream, discardChange: () => ({}) },
    health,
    ...more,
  };
}

const reading = (...on: HealthOn[]): Api["health"] => ({
  listHealth: () => ({ services: [aHealth("svc_web", ...on)] }),
});

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
    tone: "dw-tone-running",
    says: /Nothing checks it, so this only means it started/,
  },
  {
    on: { state: HealthState.STARTING },
    word: "Starting",
    tone: "dw-tone-cobalt",
    says: /Its check hasn't finished yet/,
  },
  {
    on: {
      state: HealthState.UNHEALTHY,
      said: "GET /healthz on port 8080 answered 503 Service Unavailable",
      failing: 3,
    },
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
    tone: "dw-tone-unknown",
    says: /The agent on hz-1 hasn't said how this runs since .* ago, when it read Healthy/,
    notice: /Nobody can say how web runs/,
  },
];

afterEach(() => {
  vi.unstubAllGlobals();
  vi.useRealTimers();
  onlineManager.setOnline(true);
  localStorage.clear();
});

/** The names of the list's rows, top to bottom. */
const rowNames = () =>
  screen
    .queryAllByRole("row")
    .slice(1)
    .map((row) => row.querySelector(".apps-name, .dw-cell-title")?.textContent ?? "");

describe("the list's order", () => {
  const quiet = anApp("quiet", "nginx:1.27", {}, { updateTime: { seconds: 2_000_000_000n, nanos: 0 } });
  const broken = anApp("broken", "nginx:1.27", {}, { updateTime: { seconds: 1n, nanos: 0 } });

  it("is what needs a look first once how each runs has come, and doesn't move as it does", async () => {
    renderAt(
      "/apps",
      api([quiet, broken], {
        listHealth: async () => {
          await new Promise((r) => setTimeout(r, 300));
          return {
            services: [
              aHealth("svc_quiet", { state: HealthState.NO_CHECK }),
              aHealth("svc_broken", { state: HealthState.NOT_RUNNING }),
            ],
          };
        },
      }),
    );
    const seen = new Set<string>();
    await waitFor(() => {
      // The rows that hold the list's place have no names.
      const names = rowNames().filter(Boolean);
      if (names.length > 0) seen.add(names.join(","));
      expect(screen.getByText(/1 needs a look/)).toBeVisible();
    });
    expect(rowNames()).toEqual(["broken", "quiet"]);
    expect([...seen]).toEqual(["broken,quiet"]);
  });

  it("stays as it is when something goes wrong later, and offers to bring it up", async () => {
    vi.useFakeTimers({ shouldAdvanceTime: true });
    let down = false;
    renderAt(
      "/apps",
      api([quiet, broken], {
        listHealth: () => ({
          services: [
            aHealth("svc_quiet", { state: HealthState.NO_CHECK }),
            aHealth("svc_broken", { state: down ? HealthState.NOT_RUNNING : HealthState.NO_CHECK }),
          ],
        }),
      }),
    );
    await waitFor(() => expect(rowNames()).toEqual(["quiet", "broken"]));
    expect(screen.queryByRole("button", { name: "Show them first" })).not.toBeInTheDocument();
    down = true;
    await vi.advanceTimersByTimeAsync(11_000);
    await waitFor(() => expect(screen.getByText(/1 needs a look/)).toBeVisible());
    // Nothing moves under the pointer: the lead says so, and offers.
    expect(rowNames()).toEqual(["quiet", "broken"]);
    await userEvent
      .setup({ advanceTimers: vi.advanceTimersByTime })
      .click(screen.getByRole("button", { name: "Show them first" }));
    await waitFor(() => expect(rowNames()).toEqual(["broken", "quiet"]));
    expect(screen.queryByRole("button", { name: "Show them first" })).not.toBeInTheDocument();
  });

  it("keeps the revision's column as it is whatever a row's record says, and narrow", async () => {
    const deploying = anApp(
      "web",
      "nginx:1.27",
      {},
      {
        changeId: "chg_1",
        changeState: ChangeState.APPLYING,
        deployments: [{ serverId: SERVER, serverName: "hz-1", state: DeploymentState.PENDING }],
      },
    );
    const first = renderAt("/apps", api([quiet], running(quiet)));
    await waitFor(() => expect(rowNames()).toEqual(["quiet"]));
    expect(screen.getByRole("columnheader", { name: "Revision" })).toHaveStyle({ width: "152px" });
    first.unmount();
    renderAt("/apps", api([deploying], running(deploying)));
    await waitFor(() => expect(screen.getByRole("row", { name: /web/ })).toHaveTextContent("Deploying"));
    expect(screen.getByRole("columnheader", { name: "Revision" })).toHaveStyle({ width: "152px" });
  });

  it("counts as people do among the calm", async () => {
    const at = { updateTime: { seconds: 1_000n, nanos: 0 } };
    const apps = ["svc-10", "svc-2", "svc-1"].map((n) => anApp(n, "nginx:1.27", {}, at));
    renderAt("/apps", api(apps, running(...apps)));
    await waitFor(() => expect(rowNames()).toEqual(["svc-1", "svc-2", "svc-10"]));
  });
});

describe("an app that isn't running", () => {
  it.each([
    [RestartPolicy.UNLESS_STOPPED, "Docker restarts it only when it stops on its own, so someone stopped it."],
    [RestartPolicy.UNSPECIFIED, "Docker restarts it only when it stops on its own, so someone stopped it."],
    [RestartPolicy.ALWAYS, "Docker restarts it only when it stops on its own, so someone stopped it."],
    [RestartPolicy.ON_FAILURE, "Docker restarts it only when it exits with an error, and doesn't now."],
    [RestartPolicy.NO, "Nothing starts it again on its own."],
  ])("says what follows from its restart policy, %s, beside what its Restarts row says", async (restart, says) => {
    const app = anApp("web", "ghcr.io/acme/web:1.5", { restart });
    const { container } = renderAt("/apps/svc_web", api([app], reading({ state: HealthState.NOT_RUNNING })));
    await waitFor(() => expect(container.querySelector(".dw-callout")).toHaveTextContent("web isn't running"));
    expect(container.querySelector(".dw-callout")).toHaveTextContent(says);
    expect(container.querySelector(".dw-callout")).toHaveTextContent("Restart it, or deploy.");
  });

  it("doesn't say there's no check, as nothing runs to say", async () => {
    renderAt(
      "/apps/svc_web",
      api([web], reading({ state: HealthState.NOT_RUNNING, check: HealthCheckKind.UNSPECIFIED })),
    );
    const config = await screen.findByRole("region", { name: "Configuration" });
    await waitFor(() => expect(config).toHaveTextContent("None set here. The image may have one."));
    expect(config).not.toHaveTextContent("Running only means it started");
  });
});

describe("an agent that can't read Docker", () => {
  it("says so, and how to check, and the server doesn't read as healthy", async () => {
    const blind = aServer({ ...hz1, dockerUnreadableSince: timestampFromDate(new Date(Date.now() - 150_000)) });
    const { container } = renderAt(
      "/apps/svc_web",
      api(
        [web],
        reading({
          state: HealthState.UNKNOWN,
          stale: false,
          lastSaid: HealthState.NO_CHECK,
          dockerUnreadableMsAgo: 150_000,
        }),
        {},
        blind,
      ),
    );
    const panel = await screen.findByRole("region", { name: "What runs" });
    await waitFor(() => expect(panel).toHaveTextContent("Unknown"));
    expect(panel).toHaveTextContent(
      /The agent on hz-1 can't read Docker, since 2 min ago, so it can't say how this runs. Check that Docker is running there, with systemctl status docker./,
    );
    const notice = container.querySelector(".dw-callout");
    expect(notice).toHaveTextContent("Nobody can say how web runs");
    expect(notice).toHaveTextContent("The agent on hz-1 can't read Docker");
    expect(notice).toHaveTextContent("systemctl status docker");
    expect(notice).not.toHaveTextContent("dockwell run");
    // Its server isn't healthy either, wherever the panel shows it.
    const top = container.querySelector(".topbar") as HTMLElement;
    await waitFor(() => expect(top).toHaveTextContent("Can't read Docker"));
    expect(top.querySelector(".dw-tone-healthy")).toBeNull();
  });

  it("is said apart from an agent that has gone, and one that has just not said", async () => {
    const gone = aServer({ ...hz1, connection: 2 });
    const first = renderAt(
      "/apps/svc_web",
      api([web], reading({ state: HealthState.UNKNOWN, lastSaid: HealthState.NO_CHECK }), {}, gone),
    );
    let panel = await screen.findByRole("region", { name: "What runs" });
    await waitFor(() =>
      expect(panel).toHaveTextContent(
        /The agent on hz-1 isn't connected, so it can't say how this runs now. It last read Running, .* ago./,
      ),
    );
    first.unmount();
    renderAt("/apps/svc_web", api([web], reading({ state: HealthState.UNKNOWN, stale: false })));
    panel = await screen.findByRole("region", { name: "What runs" });
    await waitFor(() => expect(panel).toHaveTextContent("The agent on hz-1 hasn't read how this runs yet."));
    expect(document.querySelector(".dw-callout")).toHaveTextContent("journalctl -u dockwell");
  });
});

describe("what the fleet last said stands by its age too", () => {
  /** A watch of the fleet that says how hz-1 is, then, while answering is false, nothing at all, as a frozen control plane. */
  const quietFleet = (heartbeats: boolean) => {
    const state = { answering: true, watches: 0 };
    async function* watchServers(_req: unknown, ctx: { signal: AbortSignal }) {
      state.watches += 1;
      if (!state.answering) {
        await untilAborted(ctx.signal);
        return;
      }
      yield changed(hz1);
      yield snapshotComplete;
      while (!ctx.signal.aborted) {
        await Promise.race([new Promise((r) => setTimeout(r, 10_000)), untilAborted(ctx.signal)]);
        if (heartbeats && state.answering && !ctx.signal.aborted)
          yield create(WatchServersResponseSchema, { heartbeat: true });
        if (!state.answering) await untilAborted(ctx.signal);
      }
    }
    return { state, watchServers };
  };

  it("reads Unknown in the top bar, on the server's page and in its list, once the watch has heard nothing for 30 s", async () => {
    vi.useFakeTimers({ shouldAdvanceTime: true });
    const fleet = quietFleet(true);
    const page = renderAt("/servers/srv_1", {
      ...api([web], running(web)),
      servers: { watchServers: fleet.watchServers },
    });
    const top = await waitFor(() => {
      const bar = page.container.querySelector(".topbar") as HTMLElement;
      expect(bar).toHaveTextContent("Healthy");
      return bar;
    });
    // Heartbeats alone keep it standing, with nothing else to say.
    await vi.advanceTimersByTimeAsync(60_000);
    expect(top).toHaveTextContent("Healthy");
    // The control plane freezes: nothing comes, not even a heartbeat.
    fleet.state.answering = false;
    await vi.advanceTimersByTimeAsync(25_000);
    expect(top).toHaveTextContent("Healthy");
    await vi.advanceTimersByTimeAsync(10_000);
    await waitFor(() => expect(top).toHaveTextContent("Unknown"));
    expect(top.querySelector(".dw-tone-healthy")).toBeNull();
    expect(top.querySelector(".dw-status")).toHaveAttribute(
      "title",
      expect.stringMatching(
        /^The panel can't reach the control plane, so it can't say how your servers are\. It last heard .* ago, and keeps trying\.$/,
      ),
    );
    const agent = screen.getByRole("region", { name: "Agent" });
    expect(agent).toHaveTextContent("Unknown");
    expect(agent.querySelector(".dw-tone-healthy")).toBeNull();
    expect(page.container.querySelector(".fleet-unread")).toHaveTextContent(
      /The panel can't reach the control plane, so it can't say how this server is\. It last heard .* ago, and keeps trying\./,
    );
    // The watch that heard nothing made way for others, which wait as it did, and it says so all the while.
    await vi.advanceTimersByTimeAsync(5_000);
    expect(fleet.state.watches).toBeGreaterThan(1);
    expect(top).toHaveTextContent("Unknown");
    // Back: the next watch says how it is, and it stands again.
    fleet.state.answering = true;
    await vi.advanceTimersByTimeAsync(50_000);
    await waitFor(() => expect(top).toHaveTextContent("Healthy"));
    expect(page.container.querySelector(".fleet-unread")).toBeNull();
  });

  it("says so in the browser's words when it's the browser that went offline", async () => {
    vi.useFakeTimers({ shouldAdvanceTime: true });
    const fleet = quietFleet(true);
    const page = renderAt("/", { ...api([web], running(web)), servers: { watchServers: fleet.watchServers } });
    await screen.findByRole("heading", { level: 1, name: "Overview" });
    await waitFor(() => expect(page.container.querySelector(".topbar")).toHaveTextContent("Healthy"));
    fleet.state.answering = false;
    onlineManager.setOnline(false);
    await vi.advanceTimersByTimeAsync(35_000);
    const top = page.container.querySelector(".topbar") as HTMLElement;
    await waitFor(() => expect(top).toHaveTextContent("Unknown"));
    expect(top.querySelector(".dw-status")).toHaveAttribute(
      "title",
      expect.stringMatching(/^This browser is offline, so the panel can't say how your servers are\./),
    );
    // The overview of the one server says so, in place of what it last said of it.
    expect(page.container.querySelector(".fleet-unread")).toHaveTextContent(
      /This browser is offline, so the panel can't say how this server is\./,
    );
  });

  it("reads Unknown when the clock steps back an hour as the watch goes quiet", async () => {
    vi.useFakeTimers({ shouldAdvanceTime: true });
    const fleet = quietFleet(true);
    const page = renderAt("/apps", { ...api([web], running(web)), servers: { watchServers: fleet.watchServers } });
    const top = await waitFor(() => {
      const bar = page.container.querySelector(".topbar") as HTMLElement;
      expect(bar).toHaveTextContent("Healthy");
      return bar;
    });
    fleet.state.answering = false;
    vi.setSystemTime(Date.now() - 3_600_000);
    await vi.advanceTimersByTimeAsync(40_000);
    await waitFor(() => expect(top).toHaveTextContent("Unknown"));
    expect(top.querySelector(".dw-tone-healthy")).toBeNull();
  });

  it("reads Unknown in a list of servers too", async () => {
    vi.useFakeTimers({ shouldAdvanceTime: true });
    let answering = true;
    renderAt("/servers", {
      ...api([web], running(web)),
      servers: {
        async *watchServers(_req: unknown, ctx: { signal: AbortSignal }) {
          if (answering) {
            yield changed(hz1);
            yield changed(hz2);
            yield snapshotComplete;
          }
          await untilAborted(ctx.signal);
        },
      },
    });
    const list = await screen.findByRole("region", { name: "Servers" });
    await waitFor(() => expect(list.querySelector(".servers-row .dw-tone-healthy")).not.toBeNull());
    answering = false;
    await vi.advanceTimersByTimeAsync(35_000);
    await waitFor(() => expect(list.querySelector(".servers-row .dw-tone-unknown")).not.toBeNull());
    expect(list.querySelector(".dw-tone-healthy")).toBeNull();
  });
});

describe("what was read stands by its age, however reads go", () => {
  const healthy = (): Api["health"] => ({
    listHealth: () => ({
      services: [aHealth("svc_web", { state: HealthState.HEALTHY, passed: true, said: "200 OK" })],
    }),
  });

  it("doesn't leave Healthy standing on a read that never answers, and the next read goes out", async () => {
    vi.useFakeTimers({ shouldAdvanceTime: true });
    let hang = false;
    let reads = 0;
    renderAt(
      "/apps",
      api([web], {
        listHealth: () => {
          reads += 1;
          if (hang) return new Promise(() => {});
          return { services: [aHealth("svc_web", { state: HealthState.HEALTHY, passed: true, said: "200 OK" })] };
        },
      }),
    );
    const row = await screen.findByRole("row", { name: /web/ });
    await waitFor(() => expect(row).toHaveTextContent("Healthy"));
    hang = true;
    const at = reads;
    await vi.advanceTimersByTimeAsync(20_000);
    expect(row).toHaveTextContent("Healthy");
    await vi.advanceTimersByTimeAsync(15_000);
    await waitFor(() => expect(row).toHaveTextContent("Unknown"));
    expect(row.querySelector(".dw-tone-healthy")).toBeNull();
    // Said once, above the list, with when it last heard.
    expect(document.querySelector(".apps-unread")).toHaveTextContent(
      /The panel can't reach the control plane, so it can't say how your apps run, and they read Unknown. It last heard .* ago, and keeps trying./,
    );
    // A read that hangs gives way, so reads go on going out.
    await vi.advanceTimersByTimeAsync(60_000);
    expect(reads - at).toBeGreaterThan(3);
  });

  it("doesn't leave Healthy standing in a browser gone offline, and says so", async () => {
    vi.useFakeTimers({ shouldAdvanceTime: true });
    renderAt("/apps", api([web], healthy()));
    const row = await screen.findByRole("row", { name: /web/ });
    await waitFor(() => expect(row).toHaveTextContent("Healthy"));
    onlineManager.setOnline(false);
    await vi.advanceTimersByTimeAsync(31_000);
    await waitFor(() => expect(row).toHaveTextContent("Unknown"));
    expect(document.querySelector(".apps-unread")).toHaveTextContent(
      /This browser is offline, so the panel can't say how your apps run, and they read Unknown. It last heard .* ago. Check your connection./,
    );
    // It isn't the apps that need a look.
    expect(screen.queryByText(/needs? a look/)).not.toBeInTheDocument();
    onlineManager.setOnline(true);
    await waitFor(() => expect(row).toHaveTextContent("Healthy"));
    expect(document.querySelector(".apps-unread")).toBeNull();
  });

  it("says the browser is offline at the top, and lets the fleet's watch go", async () => {
    renderAt("/apps", api([web], healthy()));
    await waitFor(() => expect(document.querySelector(".topbar")).toHaveTextContent("hz-1"));
    const top = document.querySelector(".topbar") as HTMLElement;
    onlineManager.setOnline(false);
    window.dispatchEvent(new Event("offline"));
    await waitFor(() => expect(top).toHaveTextContent("You're offline"));
    expect(top.querySelector(".dw-tone-healthy")).toBeNull();
  });

  it("doesn't say the control plane is gone as a screen comes back, while its first read is under way", async () => {
    vi.useFakeTimers({ shouldAdvanceTime: true });
    let slow = false;
    renderAt(
      "/apps",
      api([web], {
        listHealth: async () => {
          if (slow) await new Promise((r) => setTimeout(r, 3_000));
          return { services: [aHealth("svc_web", { state: HealthState.HEALTHY, passed: true, said: "200 OK" })] };
        },
      }),
    );
    const row = await screen.findByRole("row", { name: /web/ });
    await waitFor(() => expect(row).toHaveTextContent("Healthy"));
    // In the background nothing reads, so what was read gets old.
    focusManager.setFocused(false);
    await vi.advanceTimersByTimeAsync(40_000);
    slow = true;
    focusManager.setFocused(true);
    await vi.advanceTimersByTimeAsync(1_000);
    // Its read back is under way: nothing says the control plane is gone, as it isn't known to be.
    expect(document.querySelector(".apps-unread")).toBeNull();
    expect(row).not.toHaveTextContent("Unknown");
    await vi.advanceTimersByTimeAsync(3_000);
    await waitFor(() => expect(row).toHaveTextContent("Healthy"));
    focusManager.setFocused(undefined);
  });

  const fine = () => ({ services: [aHealth("svc_web", { state: HealthState.HEALTHY, passed: true, said: "200 OK" })] });

  // What a row reads each second, and whether it's green.
  const watch = async (row: HTMLElement, seconds: number) => {
    const seen: string[] = [];
    for (let i = 0; i < seconds; i++) {
      await vi.advanceTimersByTimeAsync(1_000);
      const word = row.textContent?.match(/Healthy|Unknown|Not running|Checking/)?.[0];
      seen.push(`${word}${row.querySelector(".dw-tone-healthy") ? ":green" : ""}`);
    }
    return seen;
  };

  it("never shows an hour-old reading as a tab comes back, while its read back is under way", async () => {
    vi.useFakeTimers({ shouldAdvanceTime: true });
    let slow = false;
    renderAt(
      "/apps",
      api([web], {
        listHealth: async () => {
          if (!slow)
            return { services: [aHealth("svc_web", { state: HealthState.HEALTHY, passed: true, said: "200 OK" })] };
          await new Promise((r) => setTimeout(r, 7_000));
          return { services: [aHealth("svc_web", { state: HealthState.NOT_RUNNING, said: "exited" })] };
        },
      }),
    );
    const row = await screen.findByRole("row", { name: /web/ });
    await waitFor(() => expect(row).toHaveTextContent("Healthy"));
    focusManager.setFocused(false);
    await vi.advanceTimersByTimeAsync(3_600_000);
    slow = true;
    focusManager.setFocused(true);
    const seen = await watch(row, 8);
    expect(seen.filter((s) => s.includes("Healthy"))).toEqual([]);
    expect(seen.slice(0, 6)).toEqual(Array(6).fill("Checking"));
    await waitFor(() => expect(row).toHaveTextContent("Not running"));
    focusManager.setFocused(undefined);
  });

  it("reads Checking once, then stays Unknown with its note, where health never arrives", async () => {
    vi.useFakeTimers({ shouldAdvanceTime: true });
    renderAt("/apps", api([web], { listHealth: () => new Promise(() => {}) }));
    const row = await screen.findByRole("row", { name: /web/ });
    const seen = await watch(row, 60);
    const noted: boolean[] = [];
    for (let i = 0; i < 30; i++) {
      await vi.advanceTimersByTimeAsync(1_000);
      noted.push(document.querySelector(".apps-unread") !== null);
    }
    // One read's time, and then it holds, through every read that goes out and hangs.
    const from = seen.indexOf("Unknown");
    expect(from).toBeGreaterThan(0);
    expect(seen.slice(0, from).every((word) => word === "Checking")).toBe(true);
    expect(seen.slice(from).every((word) => word === "Unknown")).toBe(true);
    expect(noted.every(Boolean)).toBe(true);
  });

  it("keeps no reading trusted for the length of a clock's step back", async () => {
    vi.useFakeTimers({ shouldAdvanceTime: true });
    let hang = false;
    renderAt("/apps", api([web], { listHealth: () => (hang ? new Promise(() => {}) : fine()) }));
    const row = await screen.findByRole("row", { name: /web/ });
    await waitFor(() => expect(row).toHaveTextContent("Healthy"));
    await vi.advanceTimersByTimeAsync(10_500);
    vi.setSystemTime(Date.now() - 3_600_000);
    hang = true;
    await vi.advanceTimersByTimeAsync(60_000);
    expect(row).toHaveTextContent("Unknown");
    expect(row.querySelector(".dw-tone-healthy")).toBeNull();
  });

  it("never shows a reading from before a sleep as the laptop wakes", async () => {
    vi.useFakeTimers({ shouldAdvanceTime: true });
    let hang = false;
    renderAt("/apps", api([web], { listHealth: () => (hang ? new Promise(() => {}) : fine()) }));
    const row = await screen.findByRole("row", { name: /web/ });
    await waitFor(() => expect(row).toHaveTextContent("Healthy"));
    // Asleep for an hour: the wall clock moves, and no timer runs.
    hang = true;
    vi.setSystemTime(Date.now() + 3_600_000);
    const seen = await watch(row, 12);
    expect(seen.filter((s) => s.includes("Healthy"))).toEqual([]);
    // Its read back on waking is Checking for its time, then nobody can say.
    expect(seen.at(-1)).toBe("Unknown");
  });

  it("never shows a reading from before a sleep as the laptop wakes, with a read under way across it", async () => {
    vi.useFakeTimers({ shouldAdvanceTime: true });
    let hang = false;
    renderAt("/apps", api([web], { listHealth: () => (hang ? new Promise(() => {}) : fine()) }));
    const row = await screen.findByRole("row", { name: /web/ });
    await waitFor(() => expect(row).toHaveTextContent("Healthy"));
    // The next read goes out and hangs, and the laptop sleeps with it under way.
    hang = true;
    await vi.advanceTimersByTimeAsync(10_500);
    expect(row).toHaveTextContent("Healthy");
    vi.setSystemTime(Date.now() + 3_600_000);
    // Woken: the look that notices finds the reading an hour old, whatever the read under way does.
    await vi.advanceTimersByTimeAsync(1_500);
    expect(row).not.toHaveTextContent("Healthy");
    expect(row.querySelector(".dw-tone-healthy")).toBeNull();
  });

  it("gives a hung read back one time, not one for each screen moved to", async () => {
    vi.useFakeTimers({ shouldAdvanceTime: true });
    let hang = false;
    const page = renderAt("/apps", api([web], { listHealth: () => (hang ? new Promise(() => {}) : fine()) }));
    const row = await screen.findByRole("row", { name: /web/ });
    await waitFor(() => expect(row).toHaveTextContent("Healthy"));
    focusManager.setFocused(false);
    await vi.advanceTimersByTimeAsync(3_600_000);
    hang = true;
    focusManager.setFocused(true);
    const green: string[] = [];
    const look = (at: string) => {
      if (page.container.querySelector("main .dw-tone-healthy")) green.push(at);
    };
    for (let i = 0; i < 4; i++) {
      await vi.advanceTimersByTimeAsync(1_500);
      look(`list ${i}`);
    }
    page.router.history.push("/apps/svc_web");
    await screen.findByRole("heading", { level: 1, name: "web" });
    for (let i = 0; i < 4; i++) {
      await vi.advanceTimersByTimeAsync(1_500);
      look(`app ${i}`);
    }
    // 12 s on, past the read's own 8 s, whichever screen opened since.
    const head = page.container.querySelector(".page-title-row") as HTMLElement;
    expect(head).toHaveTextContent("Unknown");
    page.router.history.push("/apps");
    await vi.advanceTimersByTimeAsync(1_000);
    expect(screen.getByRole("row", { name: /web/ })).toHaveTextContent("Unknown");
    expect(green).toEqual([]);
    focusManager.setFocused(undefined);
  });

  it("says on an app's page and in its head that it can't say, once what it read is too old", async () => {
    vi.useFakeTimers({ shouldAdvanceTime: true });
    let hang = false;
    const page = renderAt(
      "/apps/svc_web",
      api([web], {
        listHealth: () =>
          hang
            ? new Promise(() => {})
            : { services: [aHealth("svc_web", { state: HealthState.HEALTHY, passed: true, said: "200 OK" })] },
      }),
    );
    const panel = await screen.findByRole("region", { name: "What runs" });
    await waitFor(() => expect(panel).toHaveTextContent("Healthy"));
    hang = true;
    await vi.advanceTimersByTimeAsync(35_000);
    await waitFor(() =>
      expect(panel).toHaveTextContent("The panel can't reach the control plane, so it can't say how this runs."),
    );
    const head = page.container.querySelector(".page-title-row") as HTMLElement;
    expect(head).toHaveTextContent("Unknown");
    expect(head.querySelector(".dw-tone-healthy")).toBeNull();
  });
});

describe("what runs, from what the server says", () => {
  it.each(STATES)(
    "reads $word on the list, its overview and a phone, kept apart from the record",
    async ({ on, word, tone, says, notice }) => {
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
      // The app's head says so too, and only that it's healthy where a check passed.
      const head = page.container.querySelector(".page-title-row") as HTMLElement;
      expect(head).toHaveTextContent(word);
      if (word !== "Healthy") expect(head.querySelector(".dw-tone-healthy")).toBeNull();
      if (notice) expect(page.container.querySelector(".dw-callout")).toHaveTextContent(notice);
      else expect(page.container.querySelector(".dw-callout")).toBeNull();
      await expectAccessible(page.container);
      page.unmount();

      onPhone();
      renderAt("/apps", api([web], reading(on)));
      const link = await screen.findByRole("link", { name: /web/ });
      await waitFor(() => expect(link).toHaveTextContent(word));
      expect(link.querySelector(".apps-phone-glyph")).toHaveClass(tone);
    },
  );

  it("says how long it has been so, and when its check last said so", async () => {
    renderAt(
      "/apps/svc_web",
      api(
        [web],
        reading({ state: HealthState.HEALTHY, said: "GET / on port 80 answered 200 OK", passed: true, forMs: 180_000 }),
      ),
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
          {
            state: HealthState.NOT_RUNNING,
            server: "srv_2",
            serverName: "hz-2",
            said: "the container isn't running: it is exited",
          },
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
    renderAt(
      "/apps",
      api([web], {
        listHealth: () => {
          throw new ConnectError("the control plane is restarting", Code.Unavailable);
        },
      }),
    );
    const row = await screen.findByRole("row", { name: /web/ });
    await waitFor(() => expect(row).toHaveTextContent("Unknown"));
    expect(row).not.toHaveTextContent("Not running");
    expect(row.querySelector(".dw-tone-healthy")).toBeNull();
    expect(row.querySelector(".dw-tone-critical")).toBeNull();
    // It isn't the app that needs a look.
    expect(screen.queryByText(/needs? a look/)).not.toBeInTheDocument();
  });

  it("lets what it last read stand through a failed read or two, and then says it can't read", async () => {
    vi.useFakeTimers({ shouldAdvanceTime: true });
    try {
      let failing = false;
      renderAt(
        "/apps",
        api([web], {
          listHealth: () => {
            if (failing) throw new ConnectError("the control plane is restarting", Code.Unavailable);
            return { services: [aHealth("svc_web", { state: HealthState.NO_CHECK })] };
          },
        }),
      );
      const row = await screen.findByRole("row", { name: /web/ });
      await waitFor(() => expect(row).toHaveTextContent("Running"));
      failing = true;
      await vi.advanceTimersByTimeAsync(11_000);
      expect(row).toHaveTextContent("Running");
      await vi.advanceTimersByTimeAsync(30_000);
      await waitFor(() => expect(row).toHaveTextContent("Unknown"));
      expect(row).not.toHaveTextContent("Not running");
    } finally {
      vi.useRealTimers();
    }
  });

  it("says on an app's page that it can't read, with no notice of what's wrong with the app", async () => {
    const { container } = renderAt(
      "/apps/svc_web",
      api([web], {
        listHealth: () => {
          throw new ConnectError("no", Code.Unavailable);
        },
      }),
    );
    const panel = await screen.findByRole("region", { name: "What runs" });
    await waitFor(() =>
      expect(panel).toHaveTextContent(
        "The panel can't reach the control plane, so it can't say how this runs. It keeps trying.",
      ),
    );
    expect(container.querySelector(".dw-callout")).toBeNull();
  });

  it("doesn't flash not running for what a deploy has just made, before its agent has looked", async () => {
    const fresh = anApp(
      "web",
      "nginx",
      {},
      {
        deployments: [
          {
            serverId: SERVER,
            serverName: "hz-1",
            state: DeploymentState.LIVE,
            updateTime: { seconds: BigInt(Math.floor(Date.now() / 1000)), nanos: 0 },
          },
        ],
      },
    );
    renderAt("/apps", api([fresh], reading({ state: HealthState.NOT_RUNNING, scannedMsAgo: 5_000 })));
    const row = await screen.findByRole("row", { name: /web/ });
    await waitFor(() => expect(row).toHaveTextContent("Checking"));
    expect(row).not.toHaveTextContent("Not running");
  });

  it("reads once for a list of many apps, and shares it with what's open", async () => {
    let reads = 0;
    const many = Array.from({ length: 40 }, (_, i) => anApp(`app-${i}`, "nginx"));
    renderAt(
      "/apps",
      api(many, {
        listHealth: () => {
          reads += 1;
          return { services: many.map((s) => aHealth(s.id, { state: HealthState.NO_CHECK })) };
        },
      }),
    );
    await waitFor(() => expect(screen.getAllByText("Running").length).toBeGreaterThan(30));
    expect(reads).toBe(1);
  });

  it("reads again at once when a change moves on, as what runs is what it changes", async () => {
    let reads = 0;
    const watch = feed<WatchEventsResponse>();
    watch.push(snapshot([]));
    renderAt("/apps", {
      ...api([web], {
        listHealth: () => {
          reads += 1;
          return {
            services: [aHealth("svc_web", { state: reads > 1 ? HealthState.HEALTHY : HealthState.NOT_RUNNING })],
          };
        },
      }),
      events: { watchEvents: watch.stream, listEvents: () => ({}) },
    });
    const row = await screen.findByRole("row", { name: /web/ });
    await waitFor(() => expect(row).toHaveTextContent("Not running"));
    watch.push(since([changeEvent(moved(webPlanned(), { state: ChangeState.APPLIED }))]));
    // Well before its own pace would read again.
    await waitFor(() => expect(row).toHaveTextContent("Healthy"), { timeout: 3_000 });
  });

  it("comes first when it needs a look, whatever its record says, and counts in the line under the title", async () => {
    const quiet = anApp("quiet", "a");
    const broken = anApp("broken", "b", {}, { updateTime: { seconds: 1n, nanos: 0 } });
    renderAt(
      "/apps",
      api([quiet, broken], {
        listHealth: () => ({
          services: [
            aHealth("svc_quiet", { state: HealthState.NO_CHECK }),
            aHealth("svc_broken", { state: HealthState.NOT_RUNNING }),
          ],
        }),
      }),
    );
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
    await waitFor(() =>
      expect(stopped).toHaveTextContent("Dockwell put nginx:1.4 back, as it was before that deploy."),
    );
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
        deployments: [
          { serverId: SERVER, serverName: "hz-1", state: DeploymentState.FAILED, error: "hz-1: no daemon" },
        ],
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
    await waitFor(() => expect(said).toHaveTextContent("None. Without one, Running only means it started."));
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
  const plan = (rolling: boolean, unchecked: { stopped?: boolean; noCheck?: boolean } = {}) =>
    create(ChangeSchema, {
      id: "chg_restart",
      summary: "Restart web",
      state: ChangeState.PLANNED,
      restart: true,
      plan: {
        fingerprint: "feedfacefeedfacefeedfacefeedface",
        summary: { replace: 1 },
        undoWindow: { seconds: 604_800n, nanos: 0 },
        resources: [
          {
            id: "svc_web",
            resource: { kind: "service", scope: "env_prod", name: "web" },
            action: PlanAction.REPLACE,
            restart: true,
          },
        ],
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
                ...(unchecked.stopped ? { changes: [{ path: "state", from: "exited", to: "running" }] } : {}),
                rollout: {
                  rolling,
                  healthyWithin: { seconds: unchecked.noCheck ? 2n : 60n, nanos: 0 },
                  ...(unchecked.noCheck ? { noCheck: true, soak: { seconds: 5n, nanos: 0 } } : {}),
                  ...(rolling ? {} : { ports: ["8080/tcp"] }),
                },
              },
            ],
          },
        ],
      },
    } as unknown as MessageInitShape<typeof ChangeSchema>);

  it.each([
    { rolling: true, reads: /A new container takes over/ },
    { rolling: false, reads: /about 13 s/ },
  ])(
    "asks for a plan, says what it does for this app, and has no undo (rolling: $rolling)",
    async ({ rolling, reads }) => {
      const user = userEvent.setup();
      const asked: RestartServiceRequest[] = [];
      renderAt(
        "/apps/svc_web",
        api([web], running(web), {
          changes: {
            watchChange: feed<WatchChangeResponse>().stream,
            discardChange: () => ({}),
            restartService: (req) => {
              asked.push(req);
              return { change: plan(rolling) };
            },
          },
        }),
      );
      await user.click(await screen.findByRole("button", { name: "Restart" }));
      const sheet = await screen.findByRole("dialog", { name: "Review plan" });
      expect(asked[0]?.id).toBe("svc_web");
      expect(sheet).toHaveTextContent(reads);
      expect(sheet).toHaveTextContent("A restart has no undo, as it changes nothing to go back to.");
      expect(sheet.querySelector(".dw-sheet-note")).toHaveTextContent("No undo");
      expect(sheet).not.toHaveTextContent(/Undo for \d+ days/);
      // It counts what it restarts as that, not as a replace.
      expect(sheet.querySelector(".dw-plan-counts")).toHaveTextContent("1 to restart");
      expect(sheet).not.toHaveTextContent("to replace");
      expect(sheet.querySelector(".dw-plan-act")).toHaveTextContent("Restart");
      await expectAccessible(document.body);
    },
  );

  it("says what nothing checking it comes to, for one that's stopped, with one clock", async () => {
    const user = userEvent.setup();
    renderAt(
      "/apps/svc_web",
      api([web], reading({ state: HealthState.NOT_RUNNING }), {
        changes: {
          watchChange: feed<WatchChangeResponse>().stream,
          discardChange: () => ({}),
          restartService: () => ({ change: plan(true, { stopped: true, noCheck: true }) }),
        },
      }),
    );
    await user.click(await screen.findByRole("button", { name: "Restart" }));
    const sheet = await screen.findByRole("dialog", { name: "Review plan" });
    expect(sheet).toHaveTextContent(
      "Nothing checks it, so it has to keep running for 5 s. If it doesn't, the new one goes, and what was stopped stays stopped",
    );
    expect(sheet).not.toHaveTextContent("healthy within");
    expect(sheet).not.toHaveTextContent("stay healthy");
  });

  it("isn't offered while it's moving, or where it has no server", async () => {
    const moving = anApp(
      "web",
      "nginx",
      {},
      {
        changeId: "chg_1",
        changeState: ChangeState.APPLYING,
        deployments: [{ serverId: SERVER, serverName: "hz-1", state: DeploymentState.PENDING }],
      },
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
    const { container } = renderAt(
      "/apps/svc_web",
      api([web], running(web), {
        events: {
          listEvents: (req) => {
            asked.push(req);
            return { events: [event("evt_1", HealthState.UNHEALTHY, 7, "GET / answered 503")] };
          },
        },
      }),
    );
    const panel = await screen.findByRole("region", { name: "Recent health" });
    const health = asked.find((r) => r.kinds.includes(EventKind.HEALTH));
    expect(health?.resourceId).toBe("svc_web");
    expect(health?.pageSize).toBeLessThanOrEqual(5);
    expect(await within(panel).findAllByRole("listitem")).toHaveLength(1);
    expect(panel).toHaveTextContent("web turned unhealthy on hz-1");
    expect(panel).toHaveTextContent("GET / answered 503");
    expect(panel).toHaveTextContent("It changed 7 more times since the last of these.");
    expect(panel.querySelector(".dw-tone-warning")).not.toBeNull();
    await expectAccessible(container);
  });

  it("says nothing while there's nothing to say", async () => {
    renderAt("/apps/svc_web", api());
    await screen.findByRole("region", { name: "What runs" });
    expect(screen.queryByRole("region", { name: "Recent health" })).not.toBeInTheDocument();
  });
});
