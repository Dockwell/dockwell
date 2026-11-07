// SPDX-License-Identifier: AGPL-3.0-only

import { readFileSync } from "node:fs";
import { resolve } from "node:path";
import { create } from "@bufbuild/protobuf";
import { timestampFromDate } from "@bufbuild/protobuf/wkt";
import { Code, ConnectError } from "@connectrpc/connect";
import {
  ActorKind,
  type AddServicePortRequest,
  CertificateState,
  type Change,
  ChangeSchema,
  ChangeState,
  CloudProvider,
  Connection,
  DeploymentState,
  DomainCheckState,
  FieldOp,
  HealthState,
  IntentKind,
  NetworkShape,
  Protocol,
  PublicAddressSource,
  type RollBackServiceRequest,
  RollbackState,
  StepProgressSchema,
  StepState,
  Surface,
  type WatchChangeResponse,
  type WatchServersResponse,
} from "@dockwell/api";
import { act, screen, waitFor, within } from "@testing-library/react";
import userEvent from "@testing-library/user-event";
import { afterEach, describe, expect, it, vi } from "vitest";

import { activateLocale } from "../../app/i18n";
import { expectAccessible } from "../../test/a11y";
import { aHealth, anApp, aProject, aRevision, ENV, places, projectPlaces, running, SERVER } from "../../test/apps";
import { applied, moved, watched, webPlanned } from "../../test/changes";
import { type Api, feed, onPhone, renderAt } from "../../test/render";
import { aServer, changed, snapshotComplete } from "../../test/servers";
import { formRefusal, routeName } from "./domains";

const hz1 = aServer({ id: SERVER, name: "hz-1", inventory: { addresses: ["49.12.33.7"] } });

const web = anApp("web", "ghcr.io/acme/web:1.5", {
  ports: [{ hostPort: 8080, containerPort: 80 }],
  env: { API_KEY: "", LOG_LEVEL: "" },
  command: ["serve", "--port", "80"],
  mounts: [
    { source: { case: "volume", value: "data" }, target: "/data" },
    { source: { case: "hostPath", value: "/srv/conf" }, target: "/conf", readOnly: true },
  ],
});

const revisions = [
  aRevision(3n, "ghcr.io/acme/web:1.5"),
  aRevision(2n, "ghcr.io/acme/web:1.4", {
    deployments: [{ serverId: SERVER, serverName: "hz-1", state: DeploymentState.SUPERSEDED }],
    changes: [{ path: "app.env[API_KEY]", op: FieldOp.MODIFIED, sensitive: true }],
  }),
  aRevision(1n, "ghcr.io/acme/web:1.3", {
    deployments: [{ serverId: SERVER, serverName: "hz-1", state: DeploymentState.SUPERSEDED }],
  }),
];

/** r5 put back what r3 had, after r4 failed: what the app runs now is r3's. */
const afterAutoRollback = [
  aRevision(5n, "nginx:1.5", {
    rolledBackTo: 0n,
    matchesLatest: true,
    change: { id: "chg_5", summary: "Roll back", rollback: true, reverts: "chg_4", state: ChangeState.APPLIED },
  }),
  aRevision(4n, "nginx:broken", {
    change: { id: "chg_4", summary: "Deploy nginx:broken to web", state: ChangeState.FAILED, error: "it exits" },
    deployments: [{ serverId: SERVER, serverName: "hz-1", state: DeploymentState.FAILED, error: "it exits" }],
  }),
  aRevision(3n, "nginx:1.5", {
    matchesLatest: true,
    deployments: [{ serverId: SERVER, serverName: "hz-1", state: DeploymentState.SUPERSEDED }],
  }),
  aRevision(2n, "nginx:1.4", {
    deployments: [{ serverId: SERVER, serverName: "hz-1", state: DeploymentState.SUPERSEDED }],
  }),
];

function api(services = [web], more: Api = {}, ...servers: ReturnType<typeof aServer>[]): Api {
  const watch = feed<WatchServersResponse>();
  watch.push(...(servers.length ? servers : [hz1]).map(changed), snapshotComplete);
  return {
    resources: places(),
    services: { listServices: () => ({ services }), listServiceRevisions: () => ({ revisions }) },
    servers: { watchServers: watch.stream },
    changes: { watchChange: feed<WatchChangeResponse>().stream, discardChange: () => ({}) },
    ...more,
  };
}

afterEach(() => {
  vi.unstubAllGlobals();
});

describe("an app's history", () => {
  it("says a revision placed it, where that's all it did, and doesn't call it a pull", async () => {
    const list = [
      aRevision(4n, "nginx:1.5", { changes: [], serversLeft: ["hz-2"] }),
      aRevision(3n, "nginx:1.5", { changes: [], serversAdded: ["hz-2"] }),
      aRevision(2n, "nginx:1.5", {
        changes: [],
        serversAdded: ["hz-3"],
        change: { id: "chg_2", summary: "Place it", state: ChangeState.FAILED, error: "it exits" },
      }),
    ];
    renderAt(
      "/apps/svc_web/deployments",
      api([web], {
        services: { listServices: () => ({ services: [web] }), listServiceRevisions: () => ({ revisions: list }) },
      }),
    );
    expect(await screen.findByRole("row", { name: /r4/ })).toHaveTextContent("Took it off hz-2");
    expect(screen.getByRole("row", { name: /r3/ })).toHaveTextContent("Placed it on hz-2");
    expect(screen.getByRole("row", { name: /r2/ })).toHaveTextContent("Couldn't place it on hz-3");
    expect(screen.queryByText(/Pulled/)).not.toBeInTheDocument();
  });
});

describe("an app's frame", () => {
  it("says what to do about an app it can't read, and offers nothing it would refuse", async () => {
    const web = anApp("web", "", {}, { spec: undefined, specUnreadable: "service svc_web: can't open it" });
    renderAt("/apps/svc_web", api([web]));
    expect(await screen.findByText("Dockwell can't read what web runs")).toBeVisible();
    expect(screen.getByText(/Restore the database and the key file from the same backup/)).toBeVisible();
    expect(screen.queryByRole("button", { name: "Deploy" })).not.toBeInTheDocument();
    expect(screen.queryByRole("button", { name: "Restart" })).not.toBeInTheDocument();
  });

  it("shows its name, state, revision, and tabs that are places", async () => {
    const { container } = renderAt("/apps/svc_web", api());
    expect(await screen.findByRole("heading", { level: 1, name: "web" })).toBeVisible();
    const tabs = screen.getByRole("navigation", { name: "App sections" });
    expect(
      within(tabs)
        .getAllByRole("link")
        .map((l) => l.textContent),
    ).toEqual(["Overview", "Deployments 3", "Logs", "Environment"]);
    expect(within(tabs).getByRole("link", { name: "Overview" })).toHaveAttribute("aria-current", "page");
    expect(screen.getByRole("navigation", { name: "Breadcrumb" })).toHaveTextContent("Apps/web");
    expect(document.title).toBe("web · Dockwell");
    await expectAccessible(container);
  });

  // A change the CLI proposed that only a person can apply shows on the app it
  // touches, or only whoever ran the CLI learns it waits (#112).
  it("says a change to it waits for a person, and opens its plan", async () => {
    // A list has each plan in brief, and GetChange in full.
    const planned = (id: string, summary: string, name: string, brief: boolean) =>
      create(ChangeSchema, {
        id,
        summary,
        state: ChangeState.PLANNED,
        proposedVia: Surface.CLI,
        plan: { hostAccess: true, resources: brief ? [] : [{ resource: { kind: "service", scope: ENV, name } }] },
      });
    const trust = planned("chg_trust", "Trust 127.0.0.1 on www", "web", true);
    const elsewhere = planned("chg_other", "Delete api", "api", true);
    const full: Record<string, Change> = {
      chg_trust: planned("chg_trust", "Trust 127.0.0.1 on www", "web", false),
      chg_other: planned("chg_other", "Delete api", "api", false),
    };
    const getChange = vi.fn(({ id }: { id: string }) => ({ change: full[id] }));
    renderAt(
      "/apps/svc_web",
      api([web], {
        changes: {
          watchChange: feed<WatchChangeResponse>().stream,
          discardChange: () => ({}),
          listChanges: () => ({ changes: [trust, elsewhere] }),
          getChange,
        },
      }),
    );
    const notice = await screen.findByText("1 change to this app needs your approval");
    const callout = notice.closest(".dw-callout") as HTMLElement;
    expect(callout).toHaveTextContent("Trust 127.0.0.1 on www. It was proposed with an API token");
    await userEvent.click(within(callout).getByRole("button", { name: "Review plan" }));
    await waitFor(() =>
      expect(getChange).toHaveBeenCalledWith(expect.objectContaining({ id: "chg_trust" }), expect.anything()),
    );
  });

  it("says when there's no such app, and the way back", async () => {
    const { container } = renderAt("/apps/svc_gone", api());
    expect(await screen.findByRole("heading", { name: "No app with this ID" })).toBeVisible();
    expect(screen.getByRole("link", { name: "Go to the apps" })).toHaveAttribute("href", "/apps");
    await expectAccessible(container);
  });

  it("says what is wrong when it can't load", async () => {
    renderAt("/apps/svc_web", {
      ...api(),
      services: {
        listServices: () => {
          throw new ConnectError("your role can't read services. Ask an owner", Code.PermissionDenied);
        },
      },
    });
    const alert = await screen.findByRole("alert");
    expect(alert).toHaveTextContent("Can't load this app");
    expect(alert).toHaveTextContent("Your role can't read services. Ask an owner.");
  });

  it("gives every tab a place", async () => {
    const user = userEvent.setup();
    const { router } = renderAt("/apps/svc_web", api());
    const tabs = await screen.findByRole("navigation", { name: "App sections" });
    await user.click(within(tabs).getByRole("link", { name: "Logs" }));
    await waitFor(() => expect(router.state.location.pathname).toBe("/apps/svc_web/logs"));
    expect(screen.getByRole("heading", { level: 1, name: "web" })).toBeVisible();
    expect(await screen.findByRole("region", { name: "Logs" })).toBeVisible();
    await user.click(within(tabs).getByRole("link", { name: "Environment" }));
    expect(await screen.findByRole("cell", { name: "API_KEY" })).toBeVisible();
    expect(screen.getAllByRole("cell", { name: "Hidden" })).toHaveLength(2);
    expect(screen.getAllByRole("cell", { name: "Set on this app" })).toHaveLength(2);
  });

  it("lists what an app really runs with, and shows only a variable's value", async () => {
    const service = anApp("web", "nginx:1.5", { env: { OWN: "" } });
    if (service.spec) {
      service.spec.variables = { LOG_LEVEL: "LOG_LEVEL", TZ_NAME: "TZ" };
      service.spec.secrets = { API_KEY: "API_KEY", DB_PASS: "DB_PASSWORD" };
    }
    const { container } = renderAt(
      "/apps/svc_web/environment",
      api([service], {
        variables: {
          listVariables: () => ({
            environmentId: "env_prod",
            variables: [
              { id: "var_1", name: "LOG_LEVEL", value: "info" },
              { id: "var_2", name: "TZ", value: "UTC" },
              { id: "sec_1", name: "API_KEY", secret: true },
              { id: "sec_2", name: "DB_PASSWORD", secret: true },
            ],
          }),
        },
      }),
    );
    const row = async (name: string) => {
      const cell = await screen.findByRole("cell", { name });
      return within(cell.closest("tr") as HTMLElement);
    };
    expect(await (await row("LOG_LEVEL")).findByText("info")).toBeVisible();
    expect((await row("LOG_LEVEL")).getByRole("cell", { name: "Variable" })).toBeVisible();
    // It sees TZ under a name of its own, so the row says which it is.
    expect(await (await row("TZ_NAME")).findByText("UTC")).toBeVisible();
    expect((await row("TZ_NAME")).getByRole("cell", { name: "Variable TZ" })).toBeVisible();
    // A secret is hidden until a passkey reveals it, as on Variables.
    expect((await row("API_KEY")).getByRole("img", { name: "Hidden" })).toBeVisible();
    expect((await row("API_KEY")).getByRole("button", { name: "Reveal API_KEY" })).toBeVisible();
    expect((await row("API_KEY")).getByRole("cell", { name: "Secret" })).toBeVisible();
    expect((await row("DB_PASS")).getByRole("cell", { name: "Secret DB_PASSWORD" })).toBeVisible();
    expect((await row("OWN")).getByRole("cell", { name: "Set on this app" })).toBeVisible();
    expect(screen.getByRole("link", { name: "Variables and secrets" })).toHaveAttribute(
      "href",
      expect.stringContaining("/variables?environment=env_prod"),
    );
    await expectAccessible(container);
  });

  it("doesn't say an app has no variables when it uses only secrets", async () => {
    const service = anApp("web", "nginx:1.5");
    if (service.spec) service.spec.secrets = { API_KEY: "API_KEY" };
    const reads = vi.fn(() => ({ environmentId: "env_prod", variables: [] }));
    const { router } = renderAt("/apps/svc_web", api([service], { variables: { listVariables: reads } }));
    expect(await screen.findByRole("link", { name: "1 secret" })).toBeVisible();
    expect(screen.queryByText("None set")).not.toBeInTheDocument();
    await router.navigate({ to: "/apps/$serviceId/environment", params: { serviceId: service.id } });
    expect(await screen.findByRole("cell", { name: "API_KEY" })).toBeVisible();
    expect(screen.queryByText(/This app runs with only what its image sets/)).not.toBeInTheDocument();
    // It reads the environment's variables once, to offer them to the app.
    await waitFor(() => expect(reads).toHaveBeenCalledOnce());
  });

  it("says when a variable's value couldn't be read, not that it's empty", async () => {
    const service = anApp("web", "nginx:1.5");
    if (service.spec) service.spec.variables = { LEVEL: "LEVEL" };
    renderAt(
      "/apps/svc_web/environment",
      api([service], {
        variables: {
          listVariables: () => {
            throw new ConnectError("your role can't read these", Code.PermissionDenied);
          },
        },
      }),
    );
    expect(await screen.findByRole("cell", { name: "Not loaded" })).toBeVisible();
  });

  it("says what's known of an app whose servers have all left", async () => {
    const service = anApp("web", "nginx:1.5", {}, { deployments: [] });
    renderAt("/apps/svc_web", api([service]));
    const alert = await screen.findByText("web has no server in the fleet");
    expect(alert.closest(".dw-callout")).toHaveTextContent("its containers may still be running on them");
    expect(screen.queryByText(/No server runs/)).not.toBeInTheDocument();
  });

  it("offers a failed app the way back, to a revision that matches the record too", async () => {
    const failed = anApp(
      "web",
      "nginx:1.5",
      {},
      {
        revision: 3n,
        changeId: "chg_3",
        changeState: ChangeState.FAILED,
        deployments: [{ serverId: SERVER, serverName: "hz-1", state: DeploymentState.FAILED, error: "hz-1: it exits" }],
      },
    );
    const asked: bigint[] = [];
    const user = userEvent.setup();
    renderAt(
      "/apps/svc_web",
      api([failed], {
        services: {
          listServices: () => ({ services: [failed] }),
          listServiceRevisions: () => ({
            revisions: [
              aRevision(3n, "nginx:1.5", {
                change: { id: "chg_3", summary: "Roll back", state: ChangeState.FAILED, error: "it exits" },
                deployments: [{ serverId: SERVER, state: DeploymentState.FAILED, error: "it exits" }],
              }),
              aRevision(2n, "nginx:broken", {
                deployments: [{ serverId: SERVER, state: DeploymentState.FAILED, error: "it exits" }],
              }),
              aRevision(1n, "nginx:1.5", { matchesLatest: true }),
            ],
          }),
          rollBackService: (req) => {
            asked.push(req.revision);
            return { change: webPlanned() };
          },
        },
      }),
    );
    await user.click(await screen.findByRole("button", { name: "Roll back to r1" }));
    await waitFor(() => expect(asked).toEqual([1n]));
  });

  it("reaches the logs of a revision that has them, and offers none of one that hasn't", async () => {
    const logs = {
      listLogSources: () => ({
        service: { id: "svc_web", name: "web" },
        sources: [{ id: "aaaaaaaaaaaa", revision: 3n, serverName: "hz-1" }],
      }),
      watchLogs: async function* () {
        yield* [];
      },
    };
    const first = renderAt("/apps/svc_web/deployments?revision=3", { ...api(), logs });
    const link = await screen.findByRole("link", { name: "Read this deploy's logs" });
    expect(link.getAttribute("href")).toContain("/apps/svc_web/logs");
    first.unmount();
    renderAt("/apps/svc_web/deployments?revision=2", { ...api(), logs });
    await screen.findByRole("table", { name: "Revisions of web" });
    await act(() => new Promise((resolve) => setTimeout(resolve, 100)));
    expect(screen.queryByRole("link", { name: "Read this deploy's logs" })).toBeNull();
  });

  it("offers no logs of a revision whose server went away", async () => {
    const lost = aRevision(3n, "ghcr.io/acme/web:1.5", {
      change: create(ChangeSchema, {
        id: "chg_lost",
        state: ChangeState.FAILED,
        steps: [create(StepProgressSchema, { state: StepState.FAILED, lost: true })],
      }),
    });
    renderAt("/apps/svc_web/deployments?revision=3", {
      ...api(),
      services: { listServices: () => ({ services: [web] }), listServiceRevisions: () => ({ revisions: [lost] }) },
      logs: {
        listLogSources: () => ({
          service: { id: "svc_web", name: "web" },
          sources: [{ id: "aaaaaaaaaaaa", revision: 3n }],
        }),
        watchLogs: async function* () {
          yield* [];
        },
      },
    });
    await screen.findByRole("table", { name: "Revisions of web" });
    await act(() => new Promise((resolve) => setTimeout(resolve, 100)));
    expect(screen.queryByRole("link", { name: "Read this deploy's logs" })).toBeNull();
  });

  it("keeps Deploy in reach on a phone, and tells the app's state in the header", async () => {
    onPhone();
    const { container } = renderAt("/apps/svc_web", api());
    expect(await screen.findByRole("heading", { level: 1, name: "web" })).toBeVisible();
    expect(screen.getByText("r3 · Checking · web:1.5")).toBeVisible();
    expect(container.querySelector(".app-actionbar")).toHaveTextContent(/^RestartDeploy$/);
    expect(screen.getByRole("link", { name: "Back" })).toHaveAttribute("href", "/apps");
    await expectAccessible(container);
  });

  it("has one way to the logs, its tab", async () => {
    renderAt("/apps/svc_web", api());
    await screen.findByRole("heading", { level: 1, name: "web" });
    expect(screen.getAllByRole("link", { name: "Logs" })).toHaveLength(1);
    expect(
      within(screen.getByRole("navigation", { name: "App sections" })).getByRole("link", { name: "Logs" }),
    ).toBeVisible();
  });

  it("shows the project it's in, once there is more than one", async () => {
    const stack = aProject("shop", "web", "db");
    renderAt("/apps/svc_shop_web", { ...api(stack), resources: projectPlaces("shop") });
    await screen.findByRole("heading", { level: 1, name: "web" });
    await waitFor(() =>
      expect(screen.getByRole("navigation", { name: "Breadcrumb" })).toHaveTextContent("Apps/shop/web"),
    );
  });

  it("says who deployed it in words that read: a person by name, the CLI by where it ran", async () => {
    const cli = aRevision(3n, "ghcr.io/acme/web:1.5", {
      change: {
        id: "chg_3",
        summary: "Deploy",
        state: ChangeState.APPLIED,
        appliedBy: { kind: ActorKind.TOKEN, name: "Local access on home-t3code, read-write" },
        appliedVia: Surface.CLI,
      },
    });
    const { container } = renderAt(
      "/apps/svc_web",
      api([web], {
        services: { listServices: () => ({ services: [web] }), listServiceRevisions: () => ({ revisions: [cli] }) },
      }),
    );
    await waitFor(() => expect(container.querySelector(".page-lead")).toHaveTextContent("from the CLI on home-t3code"));
    expect(container.querySelector(".page-lead")).not.toHaveTextContent("Local access");
    expect(container.querySelector(".page-lead")).not.toHaveTextContent("read-write");
  });

  it("speaks Chinese", async () => {
    await activateLocale("zh-CN");
    renderAt("/apps/svc_web", api());
    expect(await screen.findByRole("heading", { level: 2, name: "配置" })).toBeVisible();
    expect(screen.getByRole("button", { name: "部署" })).toBeVisible();
  });
});

describe("an app's overview", () => {
  it("tags a port at a private address, as the internet can't reach it there", async () => {
    const behindNAT = aServer({ id: SERVER, name: "hz-1", inventory: { addresses: ["10.0.0.25"] } });
    renderAt("/apps/svc_web", api([web], {}, behindNAT));
    const running = await screen.findByRole("region", { name: "What runs" });
    const open = await within(running).findByRole("link", { name: "Open 10.0.0.25:8080" });
    expect(within(running).getByText("Private")).toBeVisible();
    // The row's grid has four columns, so the tag goes in with the address, and doesn't push the rest along.
    const row = open.closest(".app-reach-row");
    expect(row?.children).toHaveLength(4);
    expect(row?.querySelector(".app-reach-where")).toHaveTextContent(/^10\.0\.0\.25:8080Private$/);
  });

  it("says what runs, where it is reached, and how it is set up", async () => {
    const { container } = renderAt("/apps/svc_web", api());
    const running = await screen.findByRole("region", { name: "What runs" });
    // A long name wraps after a slash, rather than in the middle of a word.
    expect(within(running).getByText("ghcr.io/acme/web:1.5").querySelectorAll("wbr")).toHaveLength(2);
    expect(await within(running).findByRole("link", { name: "Open 49.12.33.7:8080" })).toHaveAttribute(
      "href",
      "http://49.12.33.7:8080",
    );
    expect(running).not.toHaveTextContent("hz-1");
    // Who deployed it and when is the page's lead, and the timeline's first line, and not here too.
    expect(running).not.toHaveTextContent("deployed");
    await waitFor(() =>
      expect(container.querySelector(".page-lead")).toHaveTextContent("deployed 1 h ago by Mira Chen"),
    );
    const config = screen.getByRole("region", { name: "Configuration" });
    expect(config).toHaveTextContent("serve --port 80");
    expect(config).toHaveTextContent("2 variables");
    const volumes = screen.getByRole("region", { name: "Volumes" });
    expect(volumes).toHaveTextContent("/srv/conf");
    expect(volumes).toHaveTextContent("Server path");
    expect(volumes).toHaveTextContent("Read-only");
    // Words a person reads are in sans.
    expect(within(volumes).getByText("Read-only").closest(".dw-tag")).toHaveClass("dw-tag-words");
    await expectAccessible(container);
  });

  it("says who deployed it inside the panel on a phone, which has no lead", async () => {
    onPhone();
    renderAt("/apps/svc_web", api());
    const running = await screen.findByRole("region", { name: "What runs" });
    await waitFor(() => expect(running).toHaveTextContent("r3 deployed 1 h ago by Mira Chen"));
  });

  it("names what runs during a deploy, which is the image before it until it's replaced", async () => {
    const moving = anApp(
      "web",
      "nginx:1.6",
      {},
      { revision: 4n, deployments: [{ serverId: SERVER, serverName: "hz-1", state: DeploymentState.PENDING }] },
    );
    const list = [
      aRevision(4n, "nginx:1.6", {
        change: { id: "chg_4", summary: "Deploy", state: ChangeState.APPLYING },
        deployments: [{ serverId: SERVER, serverName: "hz-1", state: DeploymentState.PENDING }],
      }),
      aRevision(3n, "nginx:1.5"),
    ];
    const { container } = renderAt(
      "/apps/svc_web",
      api([moving], {
        services: { listServices: () => ({ services: [moving] }), listServiceRevisions: () => ({ revisions: list }) },
      }),
    );
    const running = await screen.findByRole("region", { name: "What runs" });
    expect(await within(running).findByText("nginx:1.5")).toBeVisible();
    expect(running).not.toHaveTextContent("nginx:1.6");
    expect(container.querySelector(".page-lead")).toHaveTextContent("Deploying nginx:1.6");
    // The arc of work in flight, not the dot of what is healthy.
    const note = await screen.findByRole("status");
    expect(note.querySelector(".dw-glyph")).toBeInTheDocument();
    expect(note.querySelector("circle:not([stroke])")).not.toBeInTheDocument();
  });

  it("says when a first deploy has nothing running yet", async () => {
    const moving = anApp(
      "web",
      "nginx:1.6",
      {},
      { revision: 1n, deployments: [{ serverId: SERVER, serverName: "hz-1", state: DeploymentState.PENDING }] },
    );
    renderAt(
      "/apps/svc_web",
      api([moving], {
        services: { listServices: () => ({ services: [moving] }), listServiceRevisions: () => ({ revisions: [] }) },
      }),
    );
    const running = await screen.findByRole("region", { name: "What runs" });
    expect(running).toHaveTextContent("Nothing yet");
    expect(running).not.toHaveTextContent("nginx:1.6");
  });

  it("says when it can't read the deployments, rather than showing none", async () => {
    const user = userEvent.setup();
    let fail = true;
    renderAt(
      "/apps/svc_web",
      api([web], {
        services: {
          listServices: () => ({ services: [web] }),
          listServiceRevisions: () => {
            if (fail) throw new ConnectError("the control plane couldn't read its services", Code.Internal);
            return { revisions };
          },
        },
      }),
    );
    const panel = await screen.findByRole("region", { name: "Recent deployments" });
    const alert = await within(panel).findByRole("alert");
    expect(alert).toHaveTextContent("Can't load the deployments");
    fail = false;
    await user.click(within(alert).getByRole("button", { name: "Try again" }));
    expect(await within(panel).findByText("r2")).toBeVisible();
  });

  it("shows no metric it has no API for", async () => {
    renderAt("/apps/svc_web", api());
    await screen.findByRole("region", { name: "What runs" });
    for (const name of ["Metrics", "Replicas"]) {
      expect(screen.queryByRole("region", { name })).not.toBeInTheDocument();
    }
  });

  it("says when nothing outside reaches it, and how to change that, as a plan", async () => {
    const user = userEvent.setup();
    const alone = anApp("alone", "nginx", { env: { API_KEY: "" } });
    const asked: AddServicePortRequest[] = [];
    const { container } = renderAt(
      "/apps/svc_alone",
      api([alone], {
        services: {
          listServices: () => ({ services: [alone] }),
          listServiceRevisions: () => ({ revisions }),
          addServicePort: (req) => {
            asked.push(req);
            if (req.hostPort === 9000) {
              const err = new ConnectError(
                "hz-1: can't plan service alone in env_prod: holder, in another environment, publishes port 9000, so alone can't. Choose another port on the server, or free that one first",
                Code.FailedPrecondition,
              );
              err.metadata.set("Dockwell-Error", "agent_failed");
              throw err;
            }
            if (req.hostPort === 80) {
              const err = new ConnectError(
                "alone already publishes port 80. Choose another port",
                Code.InvalidArgument,
              );
              err.metadata.set("Dockwell-Error", "invalid");
              throw err;
            }
            return { change: webPlanned() };
          },
        },
      }),
    );
    const running = await screen.findByRole("region", { name: "What runs" });
    expect(await within(running).findByText(/Nothing outside the server reaches it yet/)).toBeVisible();
    await expectAccessible(container);
    await user.click(within(running).getByRole("button", { name: "Add a port" }));
    const sheet = await screen.findByRole("dialog", { name: "Add a port to alone" });
    await expectAccessible(document.body);

    // A port is a number; that's said at the field, before anything is asked.
    await user.click(within(sheet).getByRole("button", { name: "Review plan" }));
    expect(within(sheet).getByLabelText("On the server")).toHaveAccessibleDescription("Use a port from 1 to 65535.");
    expect(asked).toEqual([]);

    await user.type(within(sheet).getByLabelText("On the server"), "80");
    await user.type(within(sheet).getByLabelText("In the app"), "80");
    await user.click(within(sheet).getByRole("button", { name: "Review plan" }));
    await waitFor(() =>
      expect(within(sheet).getByLabelText("On the server")).toHaveAccessibleDescription(/already publishes port 80/),
    );

    // Something else on the server holds this one, which only the server knows.
    await user.clear(within(sheet).getByLabelText("On the server"));
    await user.type(within(sheet).getByLabelText("On the server"), "9000");
    await user.click(within(sheet).getByRole("button", { name: "Review plan" }));
    await waitFor(() =>
      expect(within(sheet).getByLabelText("On the server")).toHaveAccessibleDescription(
        /^Holder, in another environment, publishes port 9000, so alone can't/,
      ),
    );
    expect(sheet).not.toHaveTextContent("env_prod");

    await user.clear(within(sheet).getByLabelText("On the server"));
    await user.type(within(sheet).getByLabelText("On the server"), "8080");
    await user.click(within(sheet).getByRole("button", { name: "Review plan" }));
    expect(await screen.findByRole("dialog", { name: "Review plan" })).toBeVisible();
    expect(asked.at(-1)).toMatchObject({ id: "svc_alone", hostPort: 8080, containerPort: 80, protocol: Protocol.TCP });
  });

  it("says what a port added did, not which fields moved", async () => {
    const service = anApp("web", "nginx:1.5", {}, { revision: 2n });
    const port = (field: string, to: string) => ({ path: `app.ports[0].${field}`, op: FieldOp.ADDED, to });
    renderAt(
      "/apps/svc_web",
      api([service], {
        services: {
          listServices: () => ({ services: [service] }),
          listServiceRevisions: () => ({
            revisions: [
              aRevision(2n, "nginx:1.5", {
                changes: [port("host_port", "8080"), port("container_port", "80"), port("protocol", "tcp")],
              }),
              aRevision(1n, "nginx:1.5"),
            ],
          }),
        },
      }),
    );
    expect(await screen.findByText("Published port 8080")).toBeVisible();
    expect(screen.queryByText(/and 2 more/)).not.toBeInTheDocument();
  });

  it("says a newer image a pull found, and what a tag really was", async () => {
    const service = anApp("web", "nginx:latest", {}, { revision: 2n });
    const digest = `sha256:${"ab".repeat(32)}`;
    renderAt(
      "/apps/svc_web/deployments?revision=2",
      api([service], {
        services: {
          listServices: () => ({ services: [service] }),
          listServiceRevisions: () => ({
            revisions: [
              aRevision(2n, "nginx:latest", {
                changes: [],
                deployments: [{ serverId: SERVER, serverName: "hz-1", state: DeploymentState.LIVE, digest }],
              }),
              aRevision(1n, "nginx:latest"),
            ],
          }),
        },
      }),
    );
    expect((await screen.findAllByText("Pulled nginx:latest again")).length).toBeGreaterThan(0);
    expect(screen.getByTitle(digest)).toHaveTextContent("sha256:abababababab");
  });

  it("shows an image a rollback pinned without its digest, and deploys its tag", async () => {
    const service = anApp("web", `nginx:latest@sha256:${"ab".repeat(32)}`);
    const asked: string[] = [];
    const user = userEvent.setup();
    renderAt(
      "/apps/svc_web",
      api([service], {
        services: {
          listServices: () => ({ services: [service] }),
          listServiceRevisions: () => ({ revisions }),
          deployService: (req) => {
            asked.push(req.image);
            return { change: webPlanned() };
          },
        },
      }),
    );
    expect((await screen.findAllByText("nginx:latest")).length).toBeGreaterThan(0);
    expect(document.body).not.toHaveTextContent("sha256:abab");
    await user.click(screen.getByRole("button", { name: "Deploy" }));
    const sheet = await screen.findByRole("dialog", { name: "Deploy web" });
    expect(within(sheet).getByLabelText("Image")).toHaveValue("nginx:latest");
    expect(within(sheet).getByLabelText("Image")).toHaveAccessibleDescription(
      "Pulls nginx:latest again, and replaces web only if that's a newer image.",
    );
    await user.click(within(sheet).getByRole("button", { name: /Review plan/ }));
    await waitFor(() => expect(asked).toEqual(["nginx:latest"]));
  });

  it("comes back to a form that failed saying why at its field, ready to change", async () => {
    const user = userEvent.setup();
    const watch = feed<WatchChangeResponse>();
    const service = anApp("web", "nginx:1.5", { ports: [{ hostPort: 80, containerPort: 80 }] });
    renderAt(
      "/apps/svc_web",
      api([service], {
        services: {
          listServices: () => ({ services: [service] }),
          listServiceRevisions: () => ({ revisions }),
          deployService: () => ({ change: webPlanned() }),
          addServicePort: () => ({ change: webPlanned() }),
        },
        changes: {
          watchChange: watch.stream,
          applyChange: () => ({ change: webPlanned() }),
          discardChange: () => ({}),
        },
      }),
    );
    const fail = async (message: string) => {
      await user.click(await screen.findByRole("button", { name: /Apply plan/ }));
      watch.push(watched(moved(webPlanned(), { state: ChangeState.FAILED, error: message })));
      const failed = await screen.findByRole("dialog", { name: "Couldn't replace web" });
      await user.click(within(failed).getByRole("button", { name: /Edit and try again/ }));
    };
    await user.click(await screen.findByRole("button", { name: "Deploy" }));
    const deploy = await screen.findByRole("dialog", { name: "Deploy web" });
    await user.clear(within(deploy).getByLabelText("Image"));
    await user.type(within(deploy).getByLabelText("Image"), "nginx:nope");
    await user.click(within(deploy).getByRole("button", { name: /Review plan/ }));
    await fail("hz-1: pull access denied for nginx");
    const back = await screen.findByRole("dialog", { name: "Deploy web" });
    const image = within(back).getByLabelText<HTMLInputElement>("Image");
    expect(image).toHaveAccessibleDescription(/pull access denied for nginx/);
    await waitFor(() => expect(image).toHaveFocus());
    expect(image.value.slice(image.selectionStart ?? 0, image.selectionEnd ?? 0)).toBe("nginx:nope");
    await user.click(within(back).getByRole("button", { name: "Cancel" }));

    await user.click(await screen.findByRole("button", { name: "Add a port" }));
    const port = await screen.findByRole("dialog", { name: "Add a port to web" });
    await user.type(within(port).getByLabelText("On the server"), "81");
    await user.type(within(port).getByLabelText("In the app"), "80");
    await user.click(within(port).getByRole("button", { name: /Review plan/ }));
    await fail("hz-1: address already in use");
    const portBack = await screen.findByRole("dialog", { name: "Add a port to web" });
    const host = within(portBack).getByLabelText<HTMLInputElement>("On the server");
    expect(host).toHaveAccessibleDescription(/address already in use/);
    await waitFor(() => expect(host).toHaveFocus());
  });

  it("brings the plan's own fix back to the field, and leaves an app's name as it's written", async () => {
    const user = userEvent.setup();
    const watch = feed<WatchChangeResponse>();
    const service = anApp("web", "nginx:1.5");
    renderAt(
      "/apps/svc_web",
      api([service], {
        services: {
          listServices: () => ({ services: [service] }),
          listServiceRevisions: () => ({ revisions }),
          deployService: () => ({ change: webPlanned() }),
        },
        changes: {
          watchChange: watch.stream,
          applyChange: () => ({ change: webPlanned() }),
          discardChange: () => ({}),
        },
      }),
    );
    await user.click(await screen.findByRole("button", { name: "Deploy" }));
    const deploy = await screen.findByRole("dialog", { name: "Deploy web" });
    await user.clear(within(deploy).getByLabelText("Image"));
    await user.type(within(deploy).getByLabelText("Image"), "nginx:nope");
    await user.click(within(deploy).getByRole("button", { name: /Review plan/ }));
    await user.click(await screen.findByRole("button", { name: /Apply plan/ }));
    watch.push(
      watched(
        moved(webPlanned(), {
          state: ChangeState.FAILED,
          error:
            "web stopped right after it started, with exit code 1. Check its command and variables, then try again.",
          steps: [create(StepProgressSchema, { resourceId: "svc_web", state: StepState.FAILED, reason: "exits" })],
        }),
      ),
    );
    const failed = await screen.findByRole("dialog", { name: "Couldn't replace web" });
    await user.click(within(failed).getByRole("button", { name: /Edit and try again/ }));
    const back = await screen.findByRole("dialog", { name: "Deploy web" });
    expect(within(back).getByLabelText("Image")).toHaveAccessibleDescription(
      "web stopped right after it started, with exit code 1. Check the image and tag, then deploy again.",
    );
  });

  it("says Dockwell is putting it back, not that it's deploying, while a rollback runs", async () => {
    const moving = anApp(
      "web",
      "nginx:1.32",
      {},
      {
        deployments: [{ serverId: SERVER, serverName: "hz-1", state: DeploymentState.PENDING }],
        changeState: ChangeState.APPLYING,
        rolledBackChangeId: "chg_failed",
      },
    );
    const user = userEvent.setup();
    renderAt("/apps/svc_web", api([moving]));
    await user.click(await screen.findByRole("button", { name: "Deploy" }));
    const sheet = await screen.findByRole("dialog", { name: "Deploy web" });
    expect(within(sheet).getByLabelText("Image")).toHaveAccessibleDescription(
      "Dockwell is putting nginx:1.32 back. You can review this plan once that ends.",
    );
    expect(sheet).not.toHaveTextContent("still deploying");
  });

  it("names a field in the language it reads in", async () => {
    await activateLocale("zh-CN");
    try {
      const service = anApp("web", "nginx:1.5", {}, { revision: 2n });
      renderAt(
        "/apps/svc_web",
        api([service], {
          services: {
            listServices: () => ({ services: [service] }),
            listServiceRevisions: () => ({
              revisions: [
                aRevision(2n, "nginx:1.5", {
                  changes: [{ path: "app.ports[0].host_port", op: FieldOp.MODIFIED, from: "80", to: "81" }],
                }),
                aRevision(1n, "nginx:1.5"),
              ],
            }),
          },
        }),
      );
      expect(await screen.findByText("已更改 端口")).toBeVisible();
      expect(document.body).not.toHaveTextContent(/\bports\b/);
    } finally {
      await activateLocale("en");
    }
  });

  it("says so when a deploy is under way, and takes another once it ends", async () => {
    const moving = anApp(
      "web",
      "nginx:1.32",
      {},
      {
        deployments: [{ serverId: SERVER, serverName: "hz-1", state: DeploymentState.PENDING }],
        changeState: ChangeState.APPLYING,
      },
    );
    const user = userEvent.setup();
    renderAt("/apps/svc_web", api([moving]));
    await user.click(await screen.findByRole("button", { name: "Deploy" }));
    const sheet = await screen.findByRole("dialog", { name: "Deploy web" });
    expect(within(sheet).getByLabelText("Image")).toHaveAccessibleDescription(
      "nginx:1.32 is still deploying. You can review this plan once that ends.",
    );
    expect(within(sheet).getByRole("button", { name: /Review plan/ })).toBeDisabled();
    expect(sheet).not.toHaveTextContent("It runs nginx:1.32 now");
  });

  it("says an app's address is its domain, and what its certificate is doing until it's there", async () => {
    const domain = (state: CertificateState, message = "") => ({
      name: "app.example.com",
      url: "https://app.example.com",
      routes: [{ id: "res_1", name: "app-example-com", service: "web", port: 80 }],
      servers: [
        {
          serverId: SERVER,
          serverName: "hz-1",
          status: { domain: "app.example.com", certificate: { state, message } },
        },
      ],
    });
    let domains = [domain(CertificateState.PENDING, "Waiting for the certificate authority to answer")];
    const asked: { environment: string; service: string }[] = [];
    const { container } = renderAt(
      "/apps/svc_web",
      api([web], {
        domains: {
          listDomains: (req) => {
            asked.push({ environment: req.environment, service: req.service });
            return { domains };
          },
        },
      }),
    );
    const running = await screen.findByRole("region", { name: "What runs" });
    expect(await within(running).findByText("app.example.com")).toBeVisible();
    expect(within(running).getByText("Waiting for the certificate authority to answer")).toBeVisible();
    // Nothing to open until it answers.
    expect(within(running).queryByRole("link", { name: "Open app.example.com" })).not.toBeInTheDocument();
    expect(asked[0]).toEqual({ environment: web.resource?.scope, service: "web" });
    await expectAccessible(container);

    // It follows the certificate, and stops once there's nothing to wait for.
    domains = [domain(CertificateState.ISSUED)];
    expect(
      await within(running).findByRole("link", { name: "Open app.example.com" }, { timeout: 7000 }),
    ).toHaveAttribute("href", "https://app.example.com");
    expect(within(running).getByText("Valid")).toBeVisible();
  }, 10_000);

  it("says what's wrong with a certificate or the proxy in the server's own words", async () => {
    const failed = {
      name: "app.example.com",
      url: "https://app.example.com",
      routes: [{ id: "rte_1", name: "app-example-com", service: "web", port: 80 }],
      servers: [
        {
          serverId: SERVER,
          serverName: "hz-1",
          status: {
            domain: "app.example.com",
            certificate: { state: CertificateState.FAILED, message: "DNS has no record for app.example.com. Add one" },
          },
        },
      ],
    };
    const noPorts = anApp("web", "nginx:1.5");
    renderAt("/apps/svc_web", api([noPorts], { domains: { listDomains: () => ({ domains: [failed] }) } }));
    const words = await screen.findByText("DNS has no record for app.example.com. Add one");
    expect(words.closest("li")).toHaveClass("is-failed");
    expect(screen.queryByRole("link", { name: "Open app.example.com" })).not.toBeInTheDocument();
    // A domain with no port still has the way to add either.
    const running = screen.getByRole("region", { name: "What runs" });
    expect(within(running).getByRole("button", { name: "Add a domain" })).toBeVisible();
    expect(within(running).getByRole("button", { name: "Add a port" })).toBeVisible();
  });

  it("adds a domain as a plan, with a route named for it, and says what's in its way", async () => {
    const user = userEvent.setup();
    const alone = anApp("alone", "nginx:1.5", {}, {});
    const edits: unknown[] = [];
    renderAt(
      "/apps/svc_alone",
      api([alone], {
        services: { listServices: () => ({ services: [alone] }), listServiceRevisions: () => ({ revisions }) },
        domains: {
          // Another app's domain, in its environment, has the name this one would.
          listDomains: (req) => ({
            domains:
              req.service === ""
                ? [
                    {
                      name: "app-example.com",
                      routes: [{ id: "res_1", name: "app-example-com", service: "other", port: 80 }],
                    },
                  ]
                : [],
          }),
        },
        changes: {
          watchChange: feed<WatchChangeResponse>().stream,
          discardChange: () => ({}),
          proposeChange: (req) => {
            const route = req.edits[0]?.spec?.kind.value as { domain: string } | undefined;
            if (route?.domain === "bad.example") {
              const err = new ConnectError(
                "bad.example is under .example, which is reserved, so no public certificate authority issues for it. Use a public domain, such as app.example.com",
                Code.InvalidArgument,
              );
              err.metadata.set("Dockwell-Error", "invalid");
              throw err;
            }
            if (route?.domain === "taken.example.com") {
              const err = new ConnectError(
                "hz-1: 1Panel-openresty holds port 80, so Caddy can't start. Stop it first",
                Code.FailedPrecondition,
              );
              err.metadata.set("Dockwell-Error", "agent_failed");
              throw err;
            }
            edits.push(req.edits[0]);
            return { change: webPlanned() };
          },
        },
      }),
    );
    await user.click(await screen.findByRole("button", { name: "Add a domain" }));
    const sheet = await screen.findByRole("dialog", { name: "Add a domain to alone" });
    await expectAccessible(document.body);
    const field = within(sheet).getByLabelText("Domain");

    await user.click(within(sheet).getByRole("button", { name: /Review plan/ }));
    expect(field).toHaveAccessibleDescription("Name the domain, such as app.example.com.");

    await user.type(field, "bad.example");
    await user.click(within(sheet).getByRole("button", { name: /Review plan/ }));
    await waitFor(() => expect(field).toHaveAccessibleDescription(/no public certificate authority/));

    // What holds the proxy's ports is the server's to say, by name.
    await user.clear(field);
    await user.type(field, "taken.example.com");
    await user.click(within(sheet).getByRole("button", { name: /Review plan/ }));
    expect(await within(sheet).findByText(/1Panel-openresty holds port 80, so Caddy can't start/)).toBeVisible();

    await user.clear(field);
    await user.type(field, "App.Example.COM");
    await user.click(within(sheet).getByRole("button", { name: /Review plan/ }));
    expect(await screen.findByRole("dialog", { name: "Review plan" })).toBeVisible();
    expect(edits[0]).toMatchObject({
      resource: { kind: "route", scope: alone.resource?.scope, name: "app-example-com-2" },
      spec: { kind: { case: "route", value: { domain: "app.example.com", service: "alone", port: 80 } } },
    });
  });

  it("offers a route with no certificate, for a tunnel or a proxy in front", async () => {
    const user = userEvent.setup();
    const alone = anApp("alone", "nginx:1.5", {}, {});
    const edits: { spec?: { kind: { value: unknown } } }[] = [];
    renderAt(
      "/apps/svc_alone",
      api([alone], {
        services: { listServices: () => ({ services: [alone] }), listServiceRevisions: () => ({ revisions }) },
        domains: { listDomains: () => ({ domains: [] }) },
        changes: {
          watchChange: feed<WatchChangeResponse>().stream,
          discardChange: () => ({}),
          proposeChange: (req) => {
            const route = req.edits[0]?.spec?.kind.value as { trustedProxies?: string[] } | undefined;
            if (route?.trustedProxies?.includes("0.0.0.0/0")) {
              const err = new ConnectError(
                "the trusted proxy 0.0.0.0/0 covers too much to be someone's proxy",
                Code.InvalidArgument,
              );
              err.metadata.set("Dockwell-Error", "invalid");
              throw err;
            }
            edits.push(req.edits[0] as never);
            return { change: webPlanned() };
          },
        },
      }),
    );
    await user.click(await screen.findByRole("button", { name: "Add a domain" }));
    const sheet = await screen.findByRole("dialog", { name: "Add a domain to alone" });
    const field = within(sheet).getByLabelText("Domain");
    expect(within(sheet).queryByRole("group", { name: "What's in front" })).toBeNull();
    await user.type(field, "app.example.com");
    await user.click(within(sheet).getByRole("switch", { name: /A tunnel or a proxy in front handles HTTPS/ }));
    expect(field).toHaveAccessibleDescription("Point its DNS at your tunnel or proxy.");
    const front = within(sheet).getByRole("group", { name: "What's in front" });
    await expectAccessible(sheet);
    const review = async () => {
      await user.click(within(sheet).getByRole("button", { name: /Review plan/ }));
      const plan = await screen.findByRole("dialog", { name: "Review plan" });
      await user.click(within(plan).getByRole("button", { name: "Cancel" }));
      await screen.findByRole("dialog", { name: "Add a domain to alone" });
      return edits.at(-1)?.spec?.kind.value;
    };

    // A tunnel on this server is the default, and names it, so the app reads the right scheme and client.
    expect(within(front).getByRole("radio", { name: /A tunnel on this server/ })).toBeChecked();
    expect(await review()).toMatchObject({
      domain: "app.example.com",
      service: "alone",
      port: 80,
      tls: "off",
      trustedProxies: ["127.0.0.1"],
    });

    // Cloudflare's proxy names the preset, and says what that path is.
    await user.click(within(front).getByRole("radio", { name: /Cloudflare's proxy/ }));
    expect(front).toHaveTextContent(/crosses the open internet unencrypted.*Flexible mode.*a tunnel is the better way/);
    expect(await review()).toMatchObject({ tls: "off", trustedProxies: ["cloudflare"] });

    // Another proxy takes what connects, and the example isn't the range the guide warns against.
    await user.click(within(front).getByRole("radio", { name: /Another proxy/ }));
    const proxies = within(front).getByLabelText("What connects to this server");
    expect(proxies).toHaveAccessibleDescription(/10\.77\.0\.0\/24.*a network of its own/);
    expect(sheet).not.toHaveTextContent("172.17");
    await user.click(within(sheet).getByRole("button", { name: /Review plan/ }));
    expect(proxies).toHaveAccessibleDescription("Name what connects to this server, as an address or a range.");
    await waitFor(() => expect(proxies).toHaveFocus());
    await user.type(proxies, "0.0.0.0/0");
    await user.click(within(sheet).getByRole("button", { name: /Review plan/ }));
    await waitFor(() => expect(proxies).toHaveAccessibleDescription(/covers too much to be someone's proxy/));
    await user.clear(proxies);
    await user.type(proxies, "10.0.0.5, 172.20.0.0/16");
    expect(await review()).toMatchObject({ tls: "off", trustedProxies: ["10.0.0.5", "172.20.0.0/16"] });

    // Answering anyone is explicit, in amber, and is what a private network needs.
    const anyone = within(front).getByRole("radio", { name: /Anyone who can reach this server/ });
    expect(anyone.closest("label")).toHaveTextContent(/private network.*gets a 404/);
    await user.click(anyone);
    expect(front).toHaveTextContent(/around whatever is in front\. You approve this in the plan/);
    expect(await review()).toMatchObject({ tls: "off", plainForAnyone: true });
    expect(((edits.at(-1)?.spec?.kind.value ?? {}) as { trustedProxies?: string[] }).trustedProxies ?? []).toEqual([]);
    await expectAccessible(sheet);

    // Turned off again, what was typed for a proxy goes nowhere.
    await user.click(within(front).getByRole("radio", { name: /Another proxy/ }));
    await user.click(within(sheet).getByRole("switch", { name: /A tunnel or a proxy in front handles HTTPS/ }));
    const plain = (await review()) as { tls?: string; trustedProxies?: string[] };
    expect(plain.tls ?? "").toBe("");
    expect(plain.trustedProxies ?? []).toEqual([]);
  });

  it("hands a domain over to its plan, and comes back to what was typed", async () => {
    const user = userEvent.setup();
    const alone = anApp("alone", "nginx:1.5", {}, {});
    const watch = feed<WatchChangeResponse>();
    renderAt(
      "/apps/svc_alone",
      api([alone], {
        services: { listServices: () => ({ services: [alone] }), listServiceRevisions: () => ({ revisions }) },
        domains: { listDomains: () => ({ domains: [] }) },
        changes: {
          watchChange: watch.stream,
          discardChange: () => ({}),
          proposeChange: () => ({ change: webPlanned() }),
          applyChange: () => ({ change: moved(webPlanned(), { state: ChangeState.APPLYING }) }),
        },
      }),
    );
    await user.click(await screen.findByRole("button", { name: "Add a domain" }));
    const sheet = await screen.findByRole("dialog", { name: "Add a domain to alone" });
    await user.type(within(sheet).getByLabelText("Domain"), "app.example.com");
    await user.click(within(sheet).getByRole("button", { name: /Review plan/ }));

    // Cancelling comes back here, with what was typed.
    const review = await screen.findByRole("dialog", { name: "Review plan" });
    await user.click(within(review).getByRole("button", { name: "Cancel" }));
    const back = await screen.findByRole("dialog", { name: "Add a domain to alone" });
    expect(within(back).getByLabelText("Domain")).toHaveValue("app.example.com");

    // A plan that fails comes back too, saying why at the field, ready to change.
    await user.click(within(back).getByRole("button", { name: /Review plan/ }));
    const again = await screen.findByRole("dialog", { name: "Review plan" });
    await user.click(within(again).getByRole("button", { name: /Apply plan/ }));
    await screen.findByRole("dialog", { name: "Applying" });
    watch.push(
      watched(
        moved(webPlanned(), { state: ChangeState.FAILED, error: "Caddy refused app.example.com: it has a bad domain" }),
      ),
    );
    const failed = await screen.findByRole("dialog", { name: /^Couldn't / });
    await user.click(within(failed).getByRole("button", { name: /Edit and try again/ }));
    const fix = await screen.findByRole("dialog", { name: "Add a domain to alone" });
    const field = within(fix).getByLabelText("Domain");
    expect(field).toHaveAccessibleDescription("Caddy refused app.example.com: it has a bad domain");
    await waitFor(() => expect(field).toHaveFocus());

    // What's about none of the fields is said above them, not pinned on the domain.
    await user.click(within(fix).getByRole("button", { name: /Review plan/ }));
    const third = await screen.findByRole("dialog", { name: "Review plan" });
    await user.click(within(third).getByRole("button", { name: /Apply plan/ }));
    await screen.findByRole("dialog", { name: "Applying" });
    watch.push(watched(moved(webPlanned(), { state: ChangeState.FAILED, error: "the agent went away" })));
    const failedAgain = await screen.findByRole("dialog", { name: /^Couldn't / });
    await user.click(within(failedAgain).getByRole("button", { name: /Edit and try again/ }));
    const form = await screen.findByRole("dialog", { name: "Add a domain to alone" });
    expect(within(form).getByRole("alert")).toHaveTextContent("The agent went away");
    expect(within(form).getByLabelText("Domain")).not.toHaveAttribute("aria-invalid", "true");
  });

  it("puts a domain on the app's page as soon as it applies, though it was read moments before", async () => {
    const user = userEvent.setup();
    const alone = anApp("alone", "nginx:1.5", {}, {});
    const watch = feed<WatchChangeResponse>();
    const domain = {
      name: "app.example.com",
      url: "https://app.example.com",
      routes: [{ id: "res_route", name: "app-example-com", service: "alone", port: 80 }],
      servers: [{ serverName: "hz-1", status: { certificate: { state: CertificateState.ISSUED } } }],
    };
    let domains: (typeof domain)[] = [];
    renderAt(
      "/apps/svc_alone",
      api([alone], {
        services: { listServices: () => ({ services: [alone] }), listServiceRevisions: () => ({ revisions }) },
        domains: { listDomains: () => ({ domains }) },
        changes: {
          watchChange: watch.stream,
          proposeChange: () => ({ change: webPlanned() }),
          applyChange: () => ({ change: moved(webPlanned(), { state: ChangeState.APPLYING }) }),
        },
      }),
      { staleTime: 30_000 },
    );
    await user.click(await screen.findByRole("button", { name: "Add a domain" }));
    const sheet = await screen.findByRole("dialog", { name: "Add a domain to alone" });
    await user.type(within(sheet).getByLabelText("Domain"), "app.example.com");
    await user.click(within(sheet).getByRole("button", { name: /Review plan/ }));
    const review = await screen.findByRole("dialog", { name: "Review plan" });
    await user.click(within(review).getByRole("button", { name: /Apply plan/ }));
    await screen.findByRole("dialog", { name: "Applying" });
    domains = [domain];
    watch.push(watched(moved(webPlanned(), { state: ChangeState.APPLIED })));
    await waitFor(() => expect(document.querySelector(".app-reach-domain")).toHaveTextContent("app.example.com"));
  });

  it("gets a certificate unless it's asked not to, and sends an app on the server's network to its port", async () => {
    const user = userEvent.setup();
    const edits: { spec?: { kind: { value: unknown } } }[] = [];
    const onServer = anApp("onhost", "nginx:1.5", { hostNetwork: true }, {});
    renderAt(
      "/apps/svc_onhost",
      api([onServer], {
        services: { listServices: () => ({ services: [onServer] }), listServiceRevisions: () => ({ revisions }) },
        domains: { listDomains: () => ({ domains: [] }) },
        changes: {
          watchChange: feed<WatchChangeResponse>().stream,
          discardChange: () => ({}),
          proposeChange: (req) => {
            edits.push(req.edits[0] as never);
            return { change: webPlanned() };
          },
        },
      }),
    );
    await user.click(await screen.findByRole("button", { name: "Add a domain" }));
    const sheet = await screen.findByRole("dialog", { name: "Add a domain to onhost" });
    await user.type(within(sheet).getByLabelText("Domain"), "app.example.com");
    // Nothing's likely to work here: 80 is the proxy's, so there's no default to be refused.
    const port = within(sheet).getByLabelText("Port on the server");
    expect(port).toHaveValue("");
    await user.type(port, "8080");
    await user.click(within(sheet).getByRole("button", { name: /Review plan/ }));
    await screen.findByRole("dialog", { name: "Review plan" });
    const route = edits[0]?.spec?.kind.value as {
      service?: string;
      port?: number;
      tls?: string;
      trustedProxies?: string[];
    };
    // service says whose it is, and host_port says where it dials, rather than its container.
    expect(route).toMatchObject({ domain: "app.example.com", service: "onhost", hostPort: 8080 });
    expect(route.port ?? 0).toBe(0);
    expect(route.tls ?? "").toBe("");
    expect(route.trustedProxies ?? []).toEqual([]);
  });

  describe("a domain's form, by what's refused and where", () => {
    const refusing = (message: string, reason = "invalid") => {
      const err = new ConnectError(message, reason === "invalid" ? Code.InvalidArgument : Code.Unavailable);
      if (reason) err.metadata.set("Dockwell-Error", reason);
      throw err;
    };
    const form = async (service: ReturnType<typeof anApp>, propose: () => { change: Change }) => {
      const user = userEvent.setup();
      renderAt(
        `/apps/${service.id}`,
        api([service], {
          services: { listServices: () => ({ services: [service] }), listServiceRevisions: () => ({ revisions }) },
          domains: { listDomains: () => ({ domains: [] }) },
          changes: {
            watchChange: feed<WatchChangeResponse>().stream,
            discardChange: () => ({}),
            proposeChange: propose,
          },
        }),
      );
      await user.click(await screen.findByRole("button", { name: "Add a domain" }));
      const sheet = await screen.findByRole("dialog", { name: /^Add a domain to / });
      return { user, sheet };
    };
    const onHost = anApp("onhost", "nginx:1.5", { hostNetwork: true }, {});
    const bare = () => anApp("web", "nginx:1.5", {}, {});

    it.each([
      [
        "Docker's API",
        "port 2375 is Docker's API, which is root on the server to anyone who reaches it, so no route may send to it",
        "2375",
      ],
      [
        "the proxy's own",
        "Can't plan on hz-1: can't plan route app-example-com: port 80 on this server is the one Caddy listens on, so a route to it would send requests round in a circle",
        "80",
      ],
      [
        "Dockwell's own",
        "Can't plan on hz-1: can't plan route app-example-com: port 7600 on this server is one Dockwell listens on itself",
        "7600",
      ],
    ])("puts a refused port at the port field, and takes you there: %s", async (_, message, typed) => {
      const { user, sheet } = await form(onHost, () => refusing(message));
      await user.type(within(sheet).getByLabelText("Domain"), "app.example.com");
      const port = within(sheet).getByLabelText("Port on the server");
      await user.type(port, typed);
      await user.click(within(sheet).getByRole("button", { name: /Review plan/ }));
      await waitFor(() => expect(port).toHaveAttribute("aria-invalid", "true"));
      expect(within(sheet).getByLabelText("Domain")).not.toHaveAttribute("aria-invalid", "true");
      // Which server's agent couldn't plan which route says nothing, with one route in front of you.
      expect(port).not.toHaveAccessibleDescription(/can't plan/i);
      expect(within(sheet).getByRole("alert")).toHaveTextContent(new RegExp(`port ${typed}`, "i"));
      await waitFor(() => expect(port).toHaveFocus());
    });

    it("says what isn't about a field above them all, not at the domain", async () => {
      const { user, sheet } = await form(bare(), () =>
        refusing("the domain's server couldn't answer about app.example.com", "agent_failed"),
      );
      const field = within(sheet).getByLabelText("Domain");
      await user.type(field, "app.example.com");
      await user.click(within(sheet).getByRole("button", { name: /Review plan/ }));
      expect(await within(sheet).findByRole("alert")).toHaveTextContent(/couldn't answer/);
      expect(field).not.toHaveAttribute("aria-invalid", "true");
    });

    it("takes you to a refused field, says so to a screen reader, and opens on the domain", async () => {
      const { user, sheet } = await form(bare(), () => ({ change: webPlanned() }));
      const field = within(sheet).getByLabelText("Domain");
      await waitFor(() => expect(field).toHaveFocus());
      const port = within(sheet).getByLabelText("Port in the app");
      await user.click(port);
      await user.click(within(sheet).getByRole("button", { name: /Review plan/ }));
      await waitFor(() => expect(field).toHaveFocus());
      expect(within(sheet).getByRole("alert")).toHaveTextContent("Name the domain, such as app.example.com.");
    });

    it("trims what's typed, and starts from the port an app publishes", async () => {
      const edits: { domain?: string; port?: number }[] = [];
      const published = anApp("published", "nginx:1.5", { ports: [{ hostPort: 8081, containerPort: 3000 }] }, {});
      const user = userEvent.setup();
      renderAt(
        "/apps/svc_published",
        api([published], {
          services: { listServices: () => ({ services: [published] }), listServiceRevisions: () => ({ revisions }) },
          domains: { listDomains: () => ({ domains: [] }) },
          changes: {
            watchChange: feed<WatchChangeResponse>().stream,
            discardChange: () => ({}),
            proposeChange: (req) => {
              edits.push(req.edits[0]?.spec?.kind.value as never);
              return { change: webPlanned() };
            },
          },
        }),
      );
      await user.click(await screen.findByRole("button", { name: "Add a domain" }));
      const sheet = await screen.findByRole("dialog", { name: "Add a domain to published" });
      expect(within(sheet).getByLabelText("Port in the app")).toHaveValue("3000");
      await user.type(within(sheet).getByLabelText("Domain"), "  App.Example.com ");
      await user.click(within(sheet).getByRole("button", { name: /Review plan/ }));
      await screen.findByRole("dialog", { name: "Review plan" });
      expect(edits[0]).toMatchObject({ domain: "app.example.com", port: 3000 });
    });

    it("says what an address is, in the server's words, rather than that a route needs a name", () => {
      for (const typed of ["192.168.1.10", "123", "1.2"]) expect(routeName(typed)).toMatch(/^route-[0-9-]+$/);
      expect(routeName("1a.example.com")).toBe("a-example-com");
    });

    it("says it's a port, and leaves a domain written as it is when the server leads with it", () => {
      expect(formRefusal("app.example.com. ends in a dot. Leave it off, as in app.example.com")).toMatch(
        /^app\.example\.com\. ends/,
      );
      expect(formRefusal("https://app.example.com isn't valid")).toMatch(/^https:/);
      expect(
        formRefusal("www is served by route www. Delete www first, or in this change, or choose another path"),
      ).toBe("Www is served by route www. Delete www first, or in this change, or choose another domain.");
      expect(formRefusal("Can't plan on hz-1: can't plan route r: port 80 is taken")).toBe("Port 80 is taken");
      expect(formRefusal("Nodot needs a dot, such as app.example.com.", "nodot")).toBe(
        "nodot needs a dot, such as app.example.com.",
      );
      expect(formRefusal("A route needs a domain", "nodot")).toBe("A route needs a domain");
      expect(formRefusal("Route app-example-com in env_01M6NKE3KQJX isn't valid: port 2375 is Docker's API")).toBe(
        "Port 2375 is Docker's API",
      );
    });
  });

  describe("with two servers", () => {
    const hz2 = aServer({ id: "srv_2", name: "hz-2", inventory: { addresses: ["49.12.33.8"] } });
    const send = async (service: ReturnType<typeof anApp>, choose?: string) => {
      const user = userEvent.setup();
      const edits: { servers: string[] }[] = [];
      renderAt(
        `/apps/${service.id}`,
        api(
          [service],
          {
            services: { listServices: () => ({ services: [service] }), listServiceRevisions: () => ({ revisions }) },
            domains: { listDomains: () => ({ domains: [] }) },
            changes: {
              watchChange: feed<WatchChangeResponse>().stream,
              discardChange: () => ({}),
              // The API's rule, as it refuses a route with no server once there are two.
              proposeChange: (req) => {
                const edit = req.edits[0];
                if (!edit?.servers.length) {
                  const err = new ConnectError(
                    "there are 2 servers, hz-1 and hz-2, so say which it runs on",
                    Code.InvalidArgument,
                  );
                  err.metadata.set("Dockwell-Error", "invalid");
                  throw err;
                }
                edits.push(edit as never);
                return { change: webPlanned() };
              },
            },
          },
          hz1,
          hz2,
        ),
      );
      await user.click(await screen.findByRole("button", { name: "Add a domain" }));
      const sheet = await screen.findByRole("dialog", { name: /^Add a domain to / });
      await user.type(within(sheet).getByLabelText("Domain"), "app.example.com");
      if (choose) await user.selectOptions(within(sheet).getByLabelText("Server"), choose);
      else expect(within(sheet).queryByLabelText("Server")).toBeNull();
      await user.click(within(sheet).getByRole("button", { name: /Review plan/ }));
      await screen.findByRole("dialog", { name: "Review plan" });
      return edits[0]?.servers;
    };

    it("sends a domain to the server its app runs on, with nothing to choose", async () => {
      expect(await send(anApp("web", "nginx:1.5", {}, {}))).toEqual([SERVER]);
    });

    it("asks which, when its app runs on several, as a route runs on one", async () => {
      const both = anApp(
        "web",
        "nginx:1.5",
        {},
        {
          deployments: [
            { serverId: SERVER, serverName: "hz-1" },
            { serverId: "srv_2", serverName: "hz-2" },
          ],
        },
      );
      expect(await send(both, "srv_2")).toEqual(["srv_2"]);
    });
  });

  describe("what a domain's row says", () => {
    const certificate = (state: CertificateState, message = "", expireTime?: Date) => ({
      state,
      message,
      ...(expireTime ? { expireTime: timestampFromDate(expireTime) } : {}),
    });
    const aDomain = (status: object, route: object = {}) => ({
      name: "app.example.com",
      url: "https://app.example.com",
      routes: [{ id: "rte_1", name: "app-example-com", service: "web", port: 8080, ...route }],
      servers: [{ serverId: SERVER, serverName: "hz-1", status: { domain: "app.example.com", ...status } }],
    });
    const atPage = (listDomains: () => { domains: unknown[] }, more: Api = {}) => {
      const app = anApp("web", "nginx:1.5", {}, {});
      renderAt(
        "/apps/svc_web",
        api([app], {
          services: { listServices: () => ({ services: [app] }), listServiceRevisions: () => ({ revisions }) },
          domains: { listDomains: listDomains as never },
          ...more,
        }),
      );
      return screen.findByRole("region", { name: "What runs" });
    };

    it("gives each state a shape and a word, the port, and how long a certificate has", async () => {
      const running = await atPage(() => ({
        domains: [
          aDomain({
            certificate: certificate(CertificateState.ISSUED, "", new Date(Date.now() + 61 * 86_400_000 - 60_000)),
          }),
        ],
      }));
      const row = (await within(running).findByText("app.example.com")).closest("li") as HTMLElement;
      expect(row).toHaveTextContent("Valid for 61 days");
      expect(row).toHaveTextContent("Port 8080");
      expect(row).not.toHaveTextContent("On hz-1");
      expect(row.querySelector(".dw-status.dw-tone-healthy svg, .dw-status.dw-tone-healthy .dw-glyph")).not.toBeNull();
    });

    it("keeps a long message beneath the name, in a row of its own, however long either is", async () => {
      const long =
        "pending.example.net doesn't answer over HTTPS yet: this server couldn't reach the certificate authority";
      const running = await atPage(() => ({
        domains: [aDomain({ certificate: certificate(CertificateState.PENDING, long) })],
      }));
      const name = await within(running).findByText("app.example.com");
      const note = within(running).getByText(long);
      expect(note).toHaveClass("app-domain-note");
      expect(note.closest("li")).toContainElement(name);
      expect(note).not.toContainElement(name);
      // Its name's column can't be squeezed by a message, which only the grid's CSS can promise.
      const css = readFileSync(resolve(__dirname, "Apps.css"), "utf8");
      expect(css).toMatch(/\.app-domain-note \{\s*grid-column: 2 \/ -1;/);
      expect(css).toMatch(/\.app-reach-domain \{\s*grid-template-columns: 16px minmax\(0, 1fr\) auto;/);
    });

    it("shows what a route under a path opens, as a domain's root may be another app's", async () => {
      const running = await atPage(() => ({
        domains: [
          {
            ...aDomain(
              { certificate: certificate(CertificateState.ISSUED) },
              { path: "/api", url: "https://app.example.com/api" },
            ),
            routes: [
              { id: "rte_0", name: "root", path: "", service: "nginx", port: 80, url: "https://app.example.com" },
              {
                id: "rte_1",
                name: "app-api",
                path: "/api",
                service: "web",
                port: 8080,
                url: "https://app.example.com/api",
              },
            ],
          },
        ],
      }));
      expect(await within(running).findByText("app.example.com/api")).toBeVisible();
      expect(within(running).getByRole("link", { name: "Open app.example.com/api" })).toHaveAttribute(
        "href",
        "https://app.example.com/api",
      );
      expect(within(running).queryByText("app.example.com")).toBeNull();
    });

    it("says a renewal is failing, in amber, and keeps reading", async () => {
      vi.useFakeTimers({ shouldAdvanceTime: true });
      try {
        const said =
          "Renewing the certificate for app.example.com failed, though the one it has still serves until 3 February 2027.";
        let reads = 0;
        const running = await atPage(() => {
          reads += 1;
          return { domains: [aDomain({ certificate: certificate(CertificateState.ISSUED, said) })] };
        });
        const note = await within(running).findByText(said);
        expect(note.closest("li")).toHaveClass("is-warning");
        expect(note.closest("li")).toHaveTextContent("Renewal failing");
        expect(within(running).getByRole("link", { name: "Open app.example.com" })).toBeVisible();
        const before = reads;
        await act(() => vi.advanceTimersByTimeAsync(16_000));
        expect(reads - before).toBeGreaterThanOrEqual(3);
      } finally {
        vi.useRealTimers();
      }
    });

    it("reads a certificate past its end as expired, in red, with what to do", async () => {
      const running = await atPage(() => ({
        domains: [
          aDomain({
            certificate: certificate(CertificateState.ISSUED, "", new Date(Date.now() - 2 * 86_400_000)),
          }),
        ],
      }));
      const row = (await within(running).findByText("app.example.com")).closest("li") as HTMLElement;
      expect(row).toHaveTextContent("Expired");
      expect(row).not.toHaveTextContent("Valid");
      expect(row).toHaveClass("is-failed");
      expect(row).toHaveTextContent(/ran out .* so browsers refuse this domain\. Dockwell renews it by itself\./);
      expect(within(row).getByRole("link", { name: "check its page" })).toHaveAttribute("href", "/servers/srv_1");
      expect(within(row).queryByRole("link", { name: "Open app.example.com" })).toBeNull();
    });

    it("stops saying where a domain stands once it can't be read, and says it again when it can", async () => {
      vi.useFakeTimers({ shouldAdvanceTime: true });
      try {
        let answers = true;
        const said = "Renewing it is failing.";
        const running = await atPage(() => {
          if (!answers) throw new ConnectError("the control plane is down", Code.Unavailable);
          return {
            domains: [
              aDomain({
                certificate: certificate(CertificateState.ISSUED, said, new Date(Date.now() + 47 * 86_400_000)),
              }),
            ],
          };
        });
        const row = (await within(running).findByText("app.example.com")).closest("li") as HTMLElement;
        expect(row).toHaveTextContent("Renewal failing");
        answers = false;
        await act(() => vi.advanceTimersByTimeAsync(61_000));
        await waitFor(() => expect(row).toHaveTextContent("Unknown"));
        expect(row).not.toHaveTextContent("Renewal failing");
        expect(row).not.toHaveTextContent(said);
        expect(row).not.toHaveClass("is-warning");
        expect(within(row).queryByRole("link", { name: "Open app.example.com" })).toBeNull();
        expect(within(running).getByText("Couldn't read this app's domains")).toBeVisible();

        answers = true;
        await act(() => vi.advanceTimersByTimeAsync(11_000));
        await waitFor(() => expect(row).toHaveTextContent("Renewal failing"));
        expect(within(running).queryByText("Couldn't read this app's domains")).toBeNull();
      } finally {
        vi.useRealTimers();
      }
    });

    it("goes on reading once each domain serves, on the pace of any reading, so what it says can age", async () => {
      vi.useFakeTimers({ shouldAdvanceTime: true });
      try {
        let reads = 0;
        const running = await atPage(() => {
          reads += 1;
          return { domains: [aDomain({ certificate: certificate(CertificateState.ISSUED) })] };
        });
        await within(running).findByText("Valid");
        const before = reads;
        await act(() => vi.advanceTimersByTimeAsync(21_000));
        expect(reads - before).toBe(2);
      } finally {
        vi.useRealTimers();
      }
    });

    it("says a domain no server runs any more has none, rather than getting a certificate for ever", async () => {
      vi.useFakeTimers({ shouldAdvanceTime: true });
      try {
        let reads = 0;
        const running = await atPage(() => {
          reads += 1;
          return { domains: [{ ...aDomain({}), servers: [] }] };
        });
        const row = (await within(running).findByText("app.example.com")).closest("li") as HTMLElement;
        expect(row).toHaveTextContent("No server");
        expect(row).toHaveTextContent(/nothing answers for it\. Remove it/);
        expect(row).not.toHaveTextContent("Issuing");
        await act(() => vi.advanceTimersByTimeAsync(61_000));
        const settled = reads;
        await act(() => vi.advanceTimersByTimeAsync(60_000));
        // As often as any reading, as one read less often would run out between reads.
        expect(reads - settled).toBeLessThanOrEqual(7);
      } finally {
        vi.useRealTimers();
      }
    });

    it("names the form's own switch, rather than a field only the API has, and lets the row act on it", async () => {
      const user = userEvent.setup();
      const said =
        "app.example.com points elsewhere. If this server sits behind a tunnel, or a proxy that ends TLS, it needs no certificate: set tls to off on its routes.";
      const running = await atPage(() => ({
        domains: [aDomain({ certificate: certificate(CertificateState.FAILED, said) })],
      }));
      const row = (await within(running).findByText("app.example.com")).closest("li") as HTMLElement;
      expect(row).not.toHaveTextContent("set tls to off");
      expect(row).toHaveTextContent('turn on "A tunnel or a proxy in front handles HTTPS" for this domain');
      await user.click(within(row).getByRole("button", { name: "It's behind a tunnel or a proxy" }));
      const sheet = await screen.findByRole("dialog", { name: "Move app.example.com behind what's in front" });
      expect(within(sheet).getByLabelText("Domain")).toHaveValue("app.example.com");
      expect(within(sheet).getByLabelText("Domain")).toHaveAttribute("readonly");
      expect(within(sheet).getByLabelText("Port in the app")).toHaveValue("8080");
      expect(within(sheet).getByRole("switch", { name: /A tunnel or a proxy in front/ })).toBeChecked();
    });

    it("offers that on a row that's still serving, as a failing renewal names the switch too", async () => {
      const user = userEvent.setup();
      const said =
        "Its next certificate can't come. If this server sits behind a tunnel, or a proxy that ends TLS, it needs no certificate: set tls to off on its routes.";
      const running = await atPage(() => ({
        domains: [
          aDomain({
            certificate: certificate(CertificateState.ISSUED, said, new Date(Date.now() + 20 * 86_400_000)),
          }),
        ],
      }));
      const row = (await within(running).findByText("app.example.com")).closest("li") as HTMLElement;
      expect(row).toHaveTextContent("Renewal failing");
      expect(row).toHaveTextContent('turn on "A tunnel or a proxy in front handles HTTPS" for this domain');
      await user.click(within(row).getByRole("button", { name: "It's behind a tunnel or a proxy" }));
      expect(await screen.findByRole("dialog", { name: "Move app.example.com behind what's in front" })).toBeVisible();
    });

    it("says nothing of which server with only one, and names it with two", async () => {
      const hz2 = aServer({ id: "srv_2", name: "hz-2" });
      const app = anApp("web", "nginx:1.5", {}, {});
      const issued = { certificate: certificate(CertificateState.ISSUED) };
      const domain = aDomain(issued);
      const seen = { ...domain, servers: [{ ...domain.servers[0], serverId: "srv_2", serverName: "hz-2" }] };
      renderAt(
        "/apps/svc_web",
        api(
          [app],
          {
            services: { listServices: () => ({ services: [app] }), listServiceRevisions: () => ({ revisions }) },
            domains: { listDomains: (() => ({ domains: [seen] })) as never },
          },
          hz1,
          hz2,
        ),
      );
      const running = await screen.findByRole("region", { name: "What runs" });
      const row = (await within(running).findByText("app.example.com")).closest("li") as HTMLElement;
      await waitFor(() => expect(row).toHaveTextContent("On hz-2"));
    });

    it("moves a domain behind a tunnel by editing the routes it has, whatever they're called", async () => {
      const user = userEvent.setup();
      const edits: { resource?: { name: string }; servers: string[]; spec?: { kind: { value: unknown } } }[] = [];
      const said =
        "If this server sits behind a tunnel, or a proxy that ends TLS, it needs no certificate: set tls to off on its routes.";
      const running = await atPage(
        () => ({
          domains: [
            {
              ...aDomain({ certificate: certificate(CertificateState.FAILED, said) }),
              // As the guide makes them, by hand, with names the form wouldn't give them.
              routes: [
                { id: "rte_1", name: "www", path: "", service: "web", port: 8080 },
                { id: "rte_2", name: "api", path: "/api", service: "api", port: 9000 },
              ],
            },
          ],
        }),
        {
          changes: {
            watchChange: feed<WatchChangeResponse>().stream,
            discardChange: () => ({}),
            proposeChange: (req) => {
              edits.push(...(req.edits as never[]));
              return { change: webPlanned() };
            },
          },
        },
      );
      await user.click(await within(running).findByRole("button", { name: "It's behind a tunnel or a proxy" }));
      const sheet = await screen.findByRole("dialog", { name: "Move app.example.com behind what's in front" });
      await user.clear(within(sheet).getByLabelText("Port in the app"));
      await user.type(within(sheet).getByLabelText("Port in the app"), "8081");
      await user.click(within(sheet).getByRole("button", { name: /Review plan/ }));
      await screen.findByRole("dialog", { name: "Review plan" });
      // A domain is served one way or the other, so all its routes go, and each keeps what it has.
      expect(edits.map((edit) => edit.resource?.name)).toEqual(["www", "api"]);
      expect(edits[0]?.spec?.kind.value).toMatchObject({
        domain: "app.example.com",
        service: "web",
        port: 8081,
        tls: "off",
        trustedProxies: ["127.0.0.1"],
      });
      expect(edits[1]?.spec?.kind.value).toMatchObject({
        domain: "app.example.com",
        path: "/api",
        service: "api",
        port: 9000,
        tls: "off",
      });
      // It stays on the server it runs on.
      expect(edits.map((edit) => edit.servers)).toEqual([[], []]);
    });

    it("removes a domain as a plan", async () => {
      const user = userEvent.setup();
      const edits: { resource?: { kind: string; name: string }; delete: boolean }[] = [];
      const running = await atPage(
        () => ({ domains: [aDomain({ certificate: certificate(CertificateState.ISSUED) })] }),
        {
          changes: {
            watchChange: feed<WatchChangeResponse>().stream,
            discardChange: () => ({}),
            proposeChange: (req) => {
              edits.push(...(req.edits as never[]));
              return { change: webPlanned() };
            },
          },
        },
      );
      await user.click(await within(running).findByRole("button", { name: "Remove app.example.com" }));
      await screen.findByRole("dialog", { name: "Review plan" });
      expect(edits).toEqual([
        expect.objectContaining({
          resource: expect.objectContaining({ kind: "route", name: "app-example-com" }),
          delete: true,
        }),
      ]);
    });

    it("has an app's delete plan take its routes with it, and only its own", async () => {
      const user = userEvent.setup();
      const edits: { resource?: { kind: string; name: string }; delete: boolean }[] = [];
      await atPage(
        () => ({
          domains: [
            {
              ...aDomain({ certificate: certificate(CertificateState.ISSUED) }),
              routes: [
                { id: "rte_1", name: "ours", service: "web", port: 80 },
                { id: "rte_2", name: "theirs", path: "/api", service: "api", port: 80 },
              ],
            },
          ],
        }),
        {
          changes: {
            watchChange: feed<WatchChangeResponse>().stream,
            discardChange: () => ({}),
            proposeChange: (req) => {
              edits.push(...(req.edits as never[]));
              return { change: webPlanned() };
            },
          },
        },
      );
      expect(await screen.findByText(/takes its domains with it/)).toBeVisible();
      await user.click(screen.getByRole("button", { name: "Delete this app" }));
      await screen.findByRole("dialog", { name: "Review plan" });
      expect(edits.map((e) => [e.resource?.kind, e.resource?.name, e.delete])).toEqual([
        ["route", "ours", true],
        ["service", "web", true],
      ]);
    });

    it("says it couldn't read them, and reads them again when you ask, rather than saying nothing reaches it", async () => {
      const user = userEvent.setup();
      let fails = true;
      const running = await atPage(() => {
        if (fails) throw new ConnectError("the control plane couldn't read its routes", Code.Internal);
        return { domains: [aDomain({ certificate: certificate(CertificateState.ISSUED) })] };
      });
      expect(await within(running).findByText("Couldn't read this app's domains")).toBeVisible();
      expect(running).not.toHaveTextContent("Nothing outside the server reaches it yet");
      // What it couldn't read may well be there, and the ways to add another stay in reach.
      expect(within(running).getByRole("button", { name: "Add a domain" })).toBeVisible();
      expect(within(running).getByRole("button", { name: "Add a port" })).toBeVisible();
      await expectAccessible(running);
      fails = false;
      await user.click(within(running).getByRole("button", { name: "Try again" }));
      expect(await within(running).findByText("app.example.com")).toBeVisible();
      expect(within(running).queryByText("Couldn't read this app's domains")).toBeNull();
    });

    it("says it couldn't read them beside the ports it knows, as there may be more than those", async () => {
      const published = anApp("web", "nginx:1.5", { ports: [{ hostPort: 8081, containerPort: 80 }] }, {});
      renderAt(
        "/apps/svc_web",
        api([published], {
          services: { listServices: () => ({ services: [published] }), listServiceRevisions: () => ({ revisions }) },
          domains: {
            listDomains: () => {
              throw new ConnectError("the control plane couldn't read its routes", Code.Internal);
            },
          },
        }),
      );
      const running = await screen.findByRole("region", { name: "What runs" });
      expect(await within(running).findByText("Couldn't read this app's domains")).toBeVisible();
      expect(within(running).getByRole("link", { name: /:8081/ })).toBeVisible();
    });

    it("says nothing of what reaches it until it has read them", async () => {
      const running = await atPage(() => new Promise(() => {}) as never);
      await new Promise((resolve) => setTimeout(resolve, 50));
      expect(running).not.toHaveTextContent("Nothing outside the server reaches it yet");
    });

    it("finds a route that dials a port on the server, for an app on the server's own network", async () => {
      const onHost = anApp("onhost", "nginx:1.5", { hostNetwork: true }, {});
      renderAt(
        "/apps/svc_onhost",
        api([onHost], {
          services: { listServices: () => ({ services: [onHost] }), listServiceRevisions: () => ({ revisions }) },
          domains: {
            // As ListDomains filters: by the route's service, which it now names.
            listDomains: (req) => ({
              domains:
                req.service === "onhost"
                  ? [
                      aDomain(
                        { certificate: certificate(CertificateState.ISSUED) },
                        { service: "onhost", port: 0, hostPort: 18080 },
                      ),
                    ]
                  : [],
            }),
          },
        }),
      );
      const running = await screen.findByRole("region", { name: "What runs" });
      const row = (await within(running).findByText("app.example.com")).closest("li");
      expect(row).toHaveTextContent("Port 18080");
      expect(running).not.toHaveTextContent("Nothing outside the server reaches it yet");
    });
  });

  describe("when an arrival sends you here to add what reaches it", () => {
    let router: ReturnType<typeof renderAt>["router"];
    const toArrival = async (user: ReturnType<typeof userEvent.setup>, domains: object[] = []) => {
      const alone = anApp("alone", "nginx:1.5", {}, {});
      const watch = feed<WatchChangeResponse>();
      const planned = webPlanned();
      const resource = planned.plan?.resources[0];
      if (resource) {
        resource.id = alone.id;
        if (resource.resource) {
          resource.resource.kind = "service";
          resource.resource.name = "alone";
        }
      }
      const step = planned.plan?.servers[0]?.steps[0];
      if (step?.resource) {
        step.resource.kind = "service";
        step.resource.name = "alone";
      }
      ({ router } = renderAt(
        "/apps/svc_alone",
        api([alone], {
          services: {
            listServices: () => ({ services: [alone] }),
            listServiceRevisions: () => ({ revisions }),
            deployService: () => ({ change: planned }),
          },
          domains: { listDomains: () => ({ domains: domains as never }) },
          changes: {
            watchChange: watch.stream,
            applyChange: () => ({ change: moved(planned, { state: ChangeState.APPLYING }) }),
          },
        }),
      ));
      await user.click(await screen.findByRole("button", { name: "Deploy" }));
      const form = await screen.findByRole("dialog", { name: "Deploy alone" });
      await user.clear(within(form).getByLabelText("Image"));
      await user.type(within(form).getByLabelText("Image"), "nginx:1.6");
      await user.click(within(form).getByRole("button", { name: /Review plan/ }));
      const review = await screen.findByRole("dialog", { name: "Review plan" });
      await user.click(within(review).getByRole("button", { name: /Apply plan/ }));
      await screen.findByRole("dialog", { name: "Applying" });
      watch.push(watched(applied(planned)));
      return await screen.findByRole("dialog", { name: "alone is deployed" });
    };

    it.each([
      ["Add a domain", "Add a domain to alone"],
      ["Add a port", "Add a port to alone"],
    ])("%s opens its form, for that app, and doesn't open it again", async (button, form) => {
      const user = userEvent.setup();
      const done = await toArrival(user);
      await user.click(await within(done).findByRole("button", { name: button }));
      const sheet = await screen.findByRole("dialog", { name: form });
      expect(screen.queryByRole("dialog", { name: "alone is deployed" })).toBeNull();
      await waitFor(() => expect(router.state.location.search).not.toHaveProperty("add"));
      await user.click(within(sheet).getByRole("button", { name: "Cancel" }));
      await waitFor(() => expect(screen.queryByRole("dialog", { name: form })).toBeNull());
      await new Promise((resolve) => setTimeout(resolve, 50));
      expect(screen.queryByRole("dialog", { name: form })).toBeNull();
    });
  });

  it("has nothing to open or copy in an arrival, for a domain whose server isn't connected, and goes to its page", async () => {
    const user = userEvent.setup();
    const alone = anApp("alone", "nginx:1.5", {}, {});
    const planned = webPlanned();
    const resource = planned.plan?.resources[0];
    if (resource) {
      resource.id = alone.id;
      if (resource.resource)
        Object.assign(resource.resource, { kind: "service", name: "alone", scope: alone.resource?.scope });
    }
    const step = planned.plan?.servers[0]?.steps[0];
    if (step?.resource) Object.assign(step.resource, { kind: "service", name: "alone" });
    const watch = feed<WatchChangeResponse>();
    const { router } = renderAt(
      "/apps/svc_alone",
      api([alone], {
        services: {
          listServices: () => ({ services: [alone] }),
          listServiceRevisions: () => ({ revisions }),
          deployService: () => ({ change: planned }),
        },
        domains: {
          listDomains: () => ({
            domains: [
              {
                name: "app.example.com",
                url: "https://app.example.com",
                routes: [{ id: "rte_1", name: "app", service: "alone", port: 80 }],
                servers: [
                  {
                    serverId: SERVER,
                    serverName: "hz-1",
                    stale: true,
                    status: { domain: "app.example.com", certificate: { state: CertificateState.ISSUED } },
                  },
                ],
              },
            ],
          }),
        },
        changes: {
          watchChange: watch.stream,
          applyChange: () => ({ change: moved(planned, { state: ChangeState.APPLYING }) }),
        },
      }),
    );
    await user.click(await screen.findByRole("button", { name: "Deploy" }));
    const form = await screen.findByRole("dialog", { name: "Deploy alone" });
    await user.clear(within(form).getByLabelText("Image"));
    await user.type(within(form).getByLabelText("Image"), "nginx:1.6");
    await user.click(within(form).getByRole("button", { name: /Review plan/ }));
    const review = await screen.findByRole("dialog", { name: "Review plan" });
    await user.click(within(review).getByRole("button", { name: /Apply plan/ }));
    await screen.findByRole("dialog", { name: "Applying" });
    watch.push(watched(applied(planned)));
    const done = await screen.findByRole("dialog", { name: "alone is deployed" });
    const row = (await within(done).findByText("app.example.com", { selector: ".plan-reach-at" })).closest(
      ".plan-reach-row",
    ) as HTMLElement;
    expect(row).toHaveTextContent("Not connected");
    expect(row).toHaveTextContent("hz-1 isn't connected, so this is what it last said: Valid.");
    expect(within(row).queryByRole("link", { name: "app.example.com" })).toBeNull();
    expect(within(row).queryByRole("button", { name: "Copy app.example.com" })).toBeNull();
    await user.click(within(row).getByRole("link", { name: "Check its page" }));
    await waitFor(() => expect(router.state.location.pathname).toBe(`/servers/${SERVER}`));
    expect(screen.queryByRole("dialog", { name: "alone is deployed" })).toBeNull();
  });

  it("says so in an open arrival too, as its server goes", async () => {
    const user = userEvent.setup();
    const alone = anApp("alone", "nginx:1.5", {}, {});
    const planned = webPlanned();
    const resource = planned.plan?.resources[0];
    if (resource) {
      resource.id = alone.id;
      if (resource.resource)
        Object.assign(resource.resource, { kind: "service", name: "alone", scope: alone.resource?.scope });
    }
    const step = planned.plan?.servers[0]?.steps[0];
    if (step?.resource) Object.assign(step.resource, { kind: "service", name: "alone" });
    const fleet = feed<WatchServersResponse>();
    fleet.push(changed(aServer({ id: SERVER, name: "hz-1" })), snapshotComplete);
    const watch = feed<WatchChangeResponse>();
    let stale = false;
    renderAt(
      "/apps/svc_alone",
      api([alone], {
        services: {
          listServices: () => ({ services: [alone] }),
          listServiceRevisions: () => ({ revisions }),
          deployService: () => ({ change: planned }),
        },
        servers: { watchServers: fleet.stream },
        domains: {
          // Asked for by service on the page, and by environment in the arrival, which is what this is about.
          listDomains: (req) => ({
            domains: req.service
              ? []
              : [
                  {
                    name: "app.example.com",
                    url: "https://app.example.com",
                    routes: [{ id: "rte_1", name: "app", service: "alone", port: 80 }],
                    servers: [
                      {
                        serverId: SERVER,
                        serverName: "hz-1",
                        stale,
                        status: { domain: "app.example.com", certificate: { state: CertificateState.ISSUED } },
                      },
                    ],
                  },
                ],
          }),
        },
        changes: {
          watchChange: watch.stream,
          applyChange: () => ({ change: moved(planned, { state: ChangeState.APPLYING }) }),
        },
      }),
    );
    await user.click(await screen.findByRole("button", { name: "Deploy" }));
    const form = await screen.findByRole("dialog", { name: "Deploy alone" });
    await user.clear(within(form).getByLabelText("Image"));
    await user.type(within(form).getByLabelText("Image"), "nginx:1.6");
    await user.click(within(form).getByRole("button", { name: /Review plan/ }));
    const review = await screen.findByRole("dialog", { name: "Review plan" });
    await user.click(within(review).getByRole("button", { name: /Apply plan/ }));
    await screen.findByRole("dialog", { name: "Applying" });
    watch.push(watched(applied(planned)));
    const done = await screen.findByRole("dialog", { name: "alone is deployed" });
    expect(await within(done).findByRole("link", { name: "app.example.com" })).toBeVisible();
    stale = true;
    act(() => fleet.push(changed(aServer({ id: SERVER, name: "hz-1", connection: Connection.UNREACHABLE }))));
    expect(await within(done).findByText("Not connected")).toBeVisible();
    expect(within(done).queryByRole("link", { name: "app.example.com" })).toBeNull();
  });

  it("opens nothing for an address that asks for what isn't a form", async () => {
    const alone = anApp("alone", "nginx:1.5", {}, {});
    renderAt(
      "/apps/svc_alone?add=everything",
      api([alone], {
        services: { listServices: () => ({ services: [alone] }), listServiceRevisions: () => ({ revisions }) },
        domains: { listDomains: () => ({ domains: [] }) },
      }),
    );
    await screen.findByRole("region", { name: "What runs" });
    await new Promise((resolve) => setTimeout(resolve, 50));
    expect(screen.queryByRole("dialog")).toBeNull();
  });

  describe("a domain whose server isn't connected", () => {
    const issued = (state: CertificateState, message = "") => ({
      domain: "app.example.com",
      certificate: {
        state,
        message,
        ...(state === CertificateState.ISSUED
          ? { expireTime: timestampFromDate(new Date(Date.now() + 61 * 86_400_000 - 60_000)) }
          : {}),
      },
    });
    const away = (status?: object) => ({
      name: "app.example.com",
      url: "https://app.example.com",
      routes: [{ id: "rte_1", name: "app-example-com", service: "web", port: 80 }],
      servers: [{ serverId: SERVER, serverName: "hz-1", stale: true, ...(status ? { status } : {}) }],
    });
    const seen = aServer({
      id: SERVER,
      name: "hz-1",
      connection: Connection.UNREACHABLE,
      lastSeenTime: timestampFromDate(new Date(Date.now() - 5 * 60_000)),
    });
    const atPage = (listDomains: () => { domains: ReturnType<typeof away>[] }) => {
      const app = anApp("web", "nginx:1.5", {}, {});
      renderAt(
        "/apps/svc_web",
        api(
          [app],
          {
            services: { listServices: () => ({ services: [app] }), listServiceRevisions: () => ({ revisions }) },
            domains: { listDomains },
          },
          seen,
        ),
      );
      return screen.findByRole("region", { name: "What runs" });
    };

    it("doesn't say it's serving: it names the server, and what it last said, and when", async () => {
      const running = await atPage(() => ({ domains: [away(issued(CertificateState.ISSUED))] }));
      const row = (await within(running).findByText("app.example.com")).closest("li") as HTMLElement;
      expect(row).toHaveTextContent("Not connected");
      expect(row).toHaveTextContent(/hz-1 isn't connected, so this is what it last said, .*ago: Valid for 61 days\./);
      expect(row.querySelector(".dw-tone-healthy")).toBeNull();
      expect(within(row).queryByRole("link", { name: "Open app.example.com" })).toBeNull();
      expect(within(row).getByRole("link", { name: "Check its page" })).toHaveAttribute("href", `/servers/${SERVER}`);
      await expectAccessible(running);
    });

    it("frames a certificate on its way as what it last said, not as what's happening", async () => {
      const said =
        "Getting a certificate for app.example.com. It answers over HTTPS once that's done, which usually takes under a minute.";
      const running = await atPage(() => ({ domains: [away(issued(CertificateState.PENDING, said))] }));
      const row = (await within(running).findByText("app.example.com")).closest("li") as HTMLElement;
      expect(row).toHaveTextContent(/hz-1 isn't connected, so this is what it last said, .*ago: Getting a certificate/);
      expect(row).toHaveTextContent("Not connected");
      expect(row).not.toHaveTextContent("Issuing");
    });

    it("says so of a failure too, without calling it one, as nothing is known of it now", async () => {
      const running = await atPage(() => ({
        domains: [away(issued(CertificateState.FAILED, "DNS points elsewhere."))],
      }));
      const row = (await within(running).findByText("app.example.com")).closest("li") as HTMLElement;
      expect(row).not.toHaveClass("is-failed");
      expect(row).toHaveTextContent(/last said, .*ago: DNS points elsewhere\./);
    });

    it("says it hasn't said anything, when it never has", async () => {
      const running = await atPage(() => ({ domains: [away()] }));
      const row = (await within(running).findByText("app.example.com")).closest("li") as HTMLElement;
      expect(row).toHaveTextContent("hz-1 isn't connected, and hasn't said anything of this yet.");
    });

    it("says so as soon as the server goes, though what serves isn't otherwise read again", async () => {
      const watch = feed<WatchServersResponse>();
      const connected = aServer({ id: SERVER, name: "hz-1", connection: Connection.CONNECTED });
      watch.push(changed(connected), snapshotComplete);
      let stale = false;
      const app = anApp("web", "nginx:1.5", {}, {});
      renderAt(
        "/apps/svc_web",
        api([app], {
          services: { listServices: () => ({ services: [app] }), listServiceRevisions: () => ({ revisions }) },
          servers: { watchServers: watch.stream },
          domains: {
            listDomains: () => {
              const server = { serverId: SERVER, serverName: "hz-1", stale, status: issued(CertificateState.ISSUED) };
              return { domains: [{ ...away(), servers: [server] }] };
            },
          },
        }),
      );
      const running = await screen.findByRole("region", { name: "What runs" });
      expect(await within(running).findByText("Valid for 61 days")).toBeVisible();
      stale = true;
      act(() => watch.push(changed(aServer({ id: SERVER, name: "hz-1", connection: Connection.UNREACHABLE }))));
      expect(await within(running).findByText("Not connected")).toBeVisible();
      expect(within(running).queryByText("Valid for 61 days", { exact: true })).toBeNull();
      // And back, the same way.
      stale = false;
      act(() => watch.push(changed(connected)));
      expect(await within(running).findByText("Valid for 61 days")).toBeVisible();
      expect(within(running).queryByText("Not connected")).toBeNull();
    });

    it("reads again less often after a while, and goes back to itself once the server's back", async () => {
      vi.useFakeTimers({ shouldAdvanceTime: true });
      try {
        let domains = [away(issued(CertificateState.ISSUED))];
        let reads = 0;
        const running = await atPage(() => {
          reads += 1;
          return { domains };
        });
        await within(running).findByText("Not connected");
        await act(() => vi.advanceTimersByTimeAsync(65_000));
        const settled = reads;
        await act(() => vi.advanceTimersByTimeAsync(60_000));
        expect(reads - settled).toBeLessThanOrEqual(7);
        domains = [
          {
            ...away(issued(CertificateState.ISSUED)),
            servers: [{ ...away(issued(CertificateState.ISSUED)).servers[0], stale: false }] as never,
          },
        ];
        await act(() => vi.advanceTimersByTimeAsync(31_000));
        expect(await within(running).findByText("Valid for 61 days")).toBeVisible();
        expect(within(running).queryByText("Not connected")).toBeNull();
      } finally {
        vi.useRealTimers();
      }
    });
  });

  describe("a domain another app has", () => {
    const held = (route: string, more: object = {}) => ({
      name: "app.example.com",
      url: "https://app.example.com",
      routes: [{ id: "rte_1", name: route, path: "", service: "api", port: 80, ...more }],
      servers: [{ serverId: SERVER, serverName: "hz-1" }],
    });
    describe("what the form says to do", () => {
      const open = async (network: Parameters<typeof aServer>[0]["network"], check: () => object, outbound = "") => {
        const user = userEvent.setup();
        const app = anApp("web", "nginx:1.5", {}, {});
        const server = aServer({
          id: SERVER,
          name: "hz-1",
          inventory: { addresses: ["10.0.0.25"], outboundAddress: outbound },
          network,
        });
        renderAt(
          "/apps/svc_web",
          api(
            [app],
            {
              services: { listServices: () => ({ services: [app] }), listServiceRevisions: () => ({ revisions }) },
              domains: { listDomains: () => ({ domains: [] }), checkDomain: () => ({ check: check() }) },
            },
            server,
          ),
        );
        await user.click(await screen.findByRole("button", { name: "Add a domain" }));
        const sheet = await screen.findByRole("dialog", { name: "Add a domain to web" });
        await user.type(within(sheet).getByLabelText("Domain"), "app.example.com");
        return { user, sheet };
      };
      const cloud = {
        ipv4: { address: "203.0.113.50", source: PublicAddressSource.SET_BY_PERSON },
        provider: CloudProvider.ORACLE,
        shape: NetworkShape.CLOUD_NAT,
      };

      it("shows the record to make, to copy, and follows DNS as it arrives", async () => {
        let state = DomainCheckState.NO_RECORD;
        vi.useFakeTimers({ shouldAdvanceTime: true });
        try {
          const { sheet } = await open(cloud, () => ({
            domain: "app.example.com",
            state,
            found: state === DomainCheckState.POINTS_HERE ? ["203.0.113.50"] : [],
          }));
          const row = sheet.querySelector(".domain-record-row");
          expect(row).toHaveTextContent("Aapp.example.com203.0.113.50");
          expect(within(sheet).getByRole("button", { name: "Copy 203.0.113.50" })).toBeVisible();
          expect(await within(sheet).findByText("Waiting for DNS")).toBeVisible();
          expect(sheet).toHaveTextContent("the VCN's security list, or a network security group in its place");
          expect(sheet).toHaveTextContent("insert the rule before the REJECT one with iptables -I");
          state = DomainCheckState.POINTS_HERE;
          await vi.advanceTimersByTimeAsync(5500);
          expect(await within(sheet).findByText("DNS has this record")).toBeVisible();
        } finally {
          vi.useRealTimers();
        }
      });

      it("says a record on Cloudflare's proxy blocks the certificate, and how to go on", async () => {
        const { sheet } = await open(cloud, () => ({
          domain: "app.example.com",
          state: DomainCheckState.ELSEWHERE,
          found: ["104.16.0.1"],
          proxied: true,
        }));
        expect(
          await within(sheet).findByText(/points at Cloudflare's proxy, which can block the certificate check/),
        ).toHaveTextContent('Turn on "A tunnel or a proxy in front handles HTTPS", or set the record to DNS only.');
      });

      it("leads with a tunnel on a home network, with the router's record second", async () => {
        const { sheet } = await open(
          {
            ipv4: { address: "203.0.113.7", source: PublicAddressSource.LOOKED_UP, unproven: true },
            shape: NetworkShape.LOCAL_NAT,
          },
          () => ({ domain: "app.example.com", state: DomainCheckState.NO_RECORD }),
        );
        const tunnel = within(sheet).getByRole("link", { name: "Install Cloudflare Tunnel from the Catalog" });
        expect(tunnel).toHaveAttribute("href", "/catalog/cloudflared");
        expect(sheet).toHaveTextContent("Recommended. It needs no public address and no forwarded port.");
        expect(sheet).toHaveTextContent("Or forward ports 80 and 443 on your router");
        expect(sheet.querySelector(".domain-record-row")).toHaveTextContent("203.0.113.7");
        expect(sheet).toHaveTextContent("That's your router's address");
      });

      it("shows a way out on a cloud as a record to check, and each family's record", async () => {
        const { sheet } = await open(
          {
            ipv4: { address: "203.0.113.7", source: PublicAddressSource.LOOKED_UP, unproven: true },
            ipv6: { address: "2001:db8::5", source: PublicAddressSource.ON_INTERFACE },
            provider: CloudProvider.AZURE,
            shape: NetworkShape.CLOUD_NAT,
          },
          () => ({ domain: "app.example.com", state: DomainCheckState.NO_RECORD }),
        );
        const rows = sheet.querySelectorAll(".domain-record-row");
        expect(rows[0]).toHaveTextContent("Aapp.example.com203.0.113.7");
        expect(rows[1]).toHaveTextContent("AAAAapp.example.com2001:db8::5");
        expect(sheet).toHaveTextContent("Make these records at your DNS provider.");
        expect(sheet).toHaveTextContent(
          "That's how this server reaches the internet, which may not be its way in. Check it against Azure's console.",
        );
        // Each row says whether DNS has its own record.
        expect(await within(sheet).findAllByText("Waiting for DNS")).toHaveLength(2);
      });

      it("says of each record whether DNS has it, as an A record says nothing of an AAAA one", async () => {
        const { sheet } = await open(
          {
            ipv4: { address: "203.0.113.50", source: PublicAddressSource.SET_BY_PERSON },
            ipv6: { address: "2001:db8::5", source: PublicAddressSource.ON_INTERFACE },
            provider: CloudProvider.AWS,
            shape: NetworkShape.CLOUD_NAT,
          },
          () => ({ domain: "app.example.com", state: DomainCheckState.POINTS_HERE, found: ["203.0.113.50"] }),
        );
        expect(await within(sheet).findByText("DNS has this record")).toBeVisible();
        expect(within(sheet).getAllByText("DNS has this record")).toHaveLength(1);
        expect(within(sheet).getByText("Waiting for DNS")).toBeVisible();
      });

      it("reads DNS again until it has every record, not just the first", async () => {
        let found = ["203.0.113.50"];
        vi.useFakeTimers({ shouldAdvanceTime: true });
        try {
          const { sheet } = await open(
            {
              ipv4: { address: "203.0.113.50", source: PublicAddressSource.SET_BY_PERSON },
              ipv6: { address: "2001:db8::5", source: PublicAddressSource.ON_INTERFACE },
              provider: CloudProvider.AWS,
              shape: NetworkShape.CLOUD_NAT,
            },
            () => ({ domain: "app.example.com", state: DomainCheckState.POINTS_HERE, found }),
          );
          expect(await within(sheet).findByText("Waiting for DNS")).toBeVisible();
          found = ["203.0.113.50", "2001:db8::5"];
          await vi.advanceTimersByTimeAsync(5500);
          await waitFor(() => expect(within(sheet).getAllByText("DNS has this record")).toHaveLength(2));
        } finally {
          vi.useRealTimers();
        }
      });

      it("at home with IPv6, leads with a tunnel, offers the AAAA record, and puts the router behind a forwarded port", async () => {
        const { sheet } = await open(
          {
            ipv4: { address: "203.0.113.7", source: PublicAddressSource.LOOKED_UP, unproven: true },
            ipv6: { address: "2001:db8::5", source: PublicAddressSource.ON_INTERFACE },
            shape: NetworkShape.LOCAL_NAT,
          },
          () => ({ domain: "app.example.com", state: DomainCheckState.NO_RECORD }),
          "192.168.1.20",
        );
        expect(within(sheet).getByRole("link", { name: "Install Cloudflare Tunnel from the Catalog" })).toBeVisible();
        expect(sheet).toHaveTextContent("Or make this record, as this server has a public IPv6 address of its own.");
        expect(sheet).toHaveTextContent(
          "Or forward ports 80 and 443 on your router to this server, at 192.168.1.20, and make this record.",
        );
        const rows = sheet.querySelectorAll(".domain-record-row");
        expect(rows[0]).toHaveTextContent("AAAAapp.example.com2001:db8::5");
        expect(rows[1]).toHaveTextContent("Aapp.example.com203.0.113.7");
        expect(sheet).not.toHaveTextContent("Make these records");
        expect(sheet).not.toHaveTextContent("may not be its way in");
        expect(sheet).toHaveTextContent("Its certificate comes by itself once the domain reaches this server");
      });

      it("offers only a tunnel behind carrier NAT", async () => {
        const { sheet } = await open({ shape: NetworkShape.CARRIER_NAT }, () => ({ domain: "app.example.com" }));
        expect(within(sheet).getByRole("link", { name: "Install Cloudflare Tunnel from the Catalog" })).toBeVisible();
        expect(sheet).toHaveTextContent("neither can a forwarded port");
        expect(sheet.querySelector(".domain-record-row")).toBeNull();
      });

      it("says where the address is with none known, and offers a tunnel once the form's switch says so", async () => {
        const { user, sheet } = await open({ provider: CloudProvider.ORACLE, shape: NetworkShape.CLOUD_NAT }, () => ({
          domain: "app.example.com",
        }));
        expect(sheet).toHaveTextContent("Oracle Cloud's console shows it");
        // The form's own switch is the one question, with no second control asking it again.
        expect(within(sheet).queryByRole("radio", { name: "Through a tunnel or a proxy" })).toBeNull();
        await user.click(within(sheet).getByRole("switch", { name: /A tunnel or a proxy in front handles HTTPS/ }));
        expect(within(sheet).getByRole("link", { name: "Install Cloudflare Tunnel from the Catalog" })).toBeVisible();
      });
    });

    const typed = async (domains: ReturnType<typeof held>[], domain = "app.example.com") => {
      const user = userEvent.setup();
      const edits: { resource?: { name: string } }[] = [];
      const app = anApp("web", "nginx:1.5", {}, {});
      renderAt(
        "/apps/svc_web",
        api([app], {
          services: { listServices: () => ({ services: [app] }), listServiceRevisions: () => ({ revisions }) },
          // What's sent to this app is asked for by name, and what the form checks isn't.
          domains: { listDomains: (req) => ({ domains: req.service ? [] : domains }) },
          changes: {
            watchChange: feed<WatchChangeResponse>().stream,
            discardChange: () => ({}),
            proposeChange: (req) => {
              edits.push(...(req.edits as never[]));
              return { change: webPlanned() };
            },
          },
        }),
      );
      await user.click(await screen.findByRole("button", { name: "Add a domain" }));
      const sheet = await screen.findByRole("dialog", { name: "Add a domain to web" });
      await user.type(within(sheet).getByLabelText("Domain"), domain);
      return { user, sheet, edits };
    };

    // The same whatever its route is called: one the form would make, or the guide's.
    it.each([["app-example-com"], ["www"]])("says it leaves that app, and moves route %s", async (route) => {
      const { user, sheet, edits } = await typed([held(route)]);
      const warning = await within(sheet).findByRole("status");
      expect(warning).toHaveTextContent("app.example.com goes to api now. Going on moves it here, and it leaves api.");
      await expectAccessible(sheet);
      await user.click(within(sheet).getByRole("button", { name: /Review plan/ }));
      await screen.findByRole("dialog", { name: "Review plan" });
      expect(edits[0]?.resource?.name).toBe(route);
    });

    it("says where it goes now, for what's no app's", async () => {
      const { sheet } = await typed([held("www", { service: "", port: 0, hostPort: 8080 })]);
      expect(await within(sheet).findByRole("status")).toHaveTextContent(
        "app.example.com goes to port 8080 on the server now.",
      );
    });

    it("says nothing of a domain that's this app's, or of a path under one that isn't", async () => {
      const { sheet } = await typed([held("www", { service: "web" })]);
      await new Promise((resolve) => setTimeout(resolve, 50));
      expect(sheet).not.toHaveTextContent("Going on moves it");
    });

    it("gives a domain's root a name of its own, beside a path that has the one it would take", async () => {
      expect(
        routeName("app.example.com", [
          { name: "app.example.com", routes: [{ name: "app-example-com", path: "/api" }] },
        ] as never),
      ).toBe("app-example-com-2");
    });
  });

  describe("a domain no server has said anything of", () => {
    const silent = (tls = "off") => ({
      name: "app.example.com",
      url: `${tls === "off" ? "http" : "https"}://app.example.com`,
      routes: [{ id: "res_1", name: "app-example-com", service: "alone", port: 80, tls }],
      servers: [{ serverId: "srv_1", serverName: "hz-1", stale: false }],
    });
    const atPage = (listDomains: () => { domains: ReturnType<typeof silent>[] }) => {
      const alone = anApp("alone", "nginx:1.5", {}, {});
      renderAt(
        "/apps/svc_alone",
        api([alone], {
          services: { listServices: () => ({ services: [alone] }), listServiceRevisions: () => ({ revisions }) },
          domains: { listDomains },
        }),
      );
    };
    const passes = (ms: number) => act(() => vi.advanceTimersByTimeAsync(ms));

    it("names the server after about a minute, points at its page, and reads again less often", async () => {
      vi.useFakeTimers({ shouldAdvanceTime: true });
      try {
        let reads = 0;
        atPage(() => {
          reads += 1;
          return { domains: [silent()] };
        });
        const running = await screen.findByRole("region", { name: "What runs" });
        expect(await within(running).findByText("Waiting for the server")).toBeVisible();
        await passes(50_000);
        expect(within(running).getByText("Waiting for the server")).toBeVisible();
        const early = reads;
        expect(early).toBeGreaterThanOrEqual(8);
        await passes(15_000);
        expect(within(running).queryByText("Waiting for the server")).toBeNull();
        expect(running).toHaveTextContent("hz-1 hasn't reported yet.");
        expect(within(running).getByRole("link", { name: "Check its page" })).toHaveAttribute("href", "/servers/srv_1");
        // It doesn't read as a failure, as nothing has failed that anyone can say.
        expect(running.querySelector(".app-reach-domain.is-failed")).toBeNull();
        // Still reading, and it has said what it can: every 10 s now, once it has noticed,
        // as one read less often would run out between reads.
        await passes(5_000);
        const settled = reads;
        await passes(60_000);
        expect(reads - settled).toBeGreaterThanOrEqual(5);
        expect(reads - settled).toBeLessThanOrEqual(7);
      } finally {
        vi.useRealTimers();
      }
    });

    it("says so of a domain waiting on a certificate as well", async () => {
      vi.useFakeTimers({ shouldAdvanceTime: true });
      try {
        atPage(() => ({ domains: [silent("")] }));
        const running = await screen.findByRole("region", { name: "What runs" });
        expect(await within(running).findByText("Issuing")).toBeVisible();
        await passes(61_000);
        expect(running).toHaveTextContent("hz-1 hasn't reported yet.");
      } finally {
        vi.useRealTimers();
      }
    });

    it("goes back to reading often, and to what it says, once the server speaks", async () => {
      vi.useFakeTimers({ shouldAdvanceTime: true });
      try {
        let domains = [silent("")];
        let reads = 0;
        atPage(() => {
          reads += 1;
          return { domains };
        });
        const running = await screen.findByRole("region", { name: "What runs" });
        await within(running).findByText("Issuing");
        await passes(95_000);
        expect(running).toHaveTextContent("hz-1 hasn't reported yet.");
        domains = [
          {
            ...silent(""),
            servers: [
              {
                serverId: "srv_1",
                serverName: "hz-1",
                stale: false,
                status: {
                  domain: "app.example.com",
                  certificate: { state: CertificateState.PENDING, message: "Asking for it" },
                },
              } as never,
            ],
          },
        ];
        await passes(30_000);
        expect(await within(running).findByText("Asking for it")).toBeVisible();
        const settled = reads;
        await passes(20_000);
        expect(reads - settled).toBeGreaterThanOrEqual(3);
      } finally {
        vi.useRealTimers();
      }
    });
  });

  it("doesn't say a tunnel's domain is getting a certificate while its server hasn't reported", async () => {
    const alone = anApp("alone", "nginx:1.5", {}, {});
    renderAt(
      "/apps/svc_alone",
      api([alone], {
        services: { listServices: () => ({ services: [alone] }), listServiceRevisions: () => ({ revisions }) },
        domains: {
          listDomains: () => ({
            domains: [
              {
                name: "app.example.com",
                url: "http://app.example.com",
                routes: [{ id: "res_1", name: "app-example-com", service: "alone", port: 80, tls: "off" }],
                servers: [{ serverName: "hz-1", stale: false }],
              },
            ],
          }),
        },
      }),
    );
    const running = await screen.findByRole("region", { name: "What runs" });
    expect(await within(running).findByText("Waiting for the server")).toBeVisible();
    expect(within(running).queryByText("Issuing")).toBeNull();
    expect(within(running).queryByRole("link", { name: "Open app.example.com" })).toBeNull();
  });

  it("says a domain behind a tunnel has no certificate here, and doesn't wait for one", async () => {
    const alone = anApp("alone", "nginx:1.5", {}, {});
    const listDomains = vi.fn(() => ({
      domains: [
        {
          name: "app.example.com",
          url: "http://app.example.com",
          routes: [{ id: "res_1", name: "app-example-com", service: "alone", port: 80, tls: "off" }],
          servers: [
            {
              serverName: "hz-1",
              status: { domain: "app.example.com", certificate: { state: CertificateState.NONE } },
            },
          ],
        },
      ],
    }));
    renderAt(
      "/apps/svc_alone",
      api([alone], {
        services: { listServices: () => ({ services: [alone] }), listServiceRevisions: () => ({ revisions }) },
        domains: { listDomains },
      }),
    );
    const running = await screen.findByRole("region", { name: "What runs" });
    expect(await within(running).findByText("TLS ends in front")).toBeVisible();
    expect(within(running).queryByText("Issuing")).toBeNull();
    expect(within(running).getByRole("link", { name: "Open app.example.com" })).toHaveAttribute(
      "href",
      "https://app.example.com",
    );
  });

  it("offers another port once it has one", async () => {
    renderAt("/apps/svc_web", api());
    const running = await screen.findByRole("region", { name: "What runs" });
    expect(within(running).getByRole("button", { name: "Add a port" })).toBeVisible();
  });

  it("gives the same port over TCP and UDP a row each", async () => {
    const spy = vi.spyOn(console, "error");
    const both = anApp("dns", "coredns", {
      ports: [
        { hostPort: 53, containerPort: 53, protocol: Protocol.TCP },
        { hostPort: 53, containerPort: 53, protocol: Protocol.UDP },
      ],
    });
    renderAt("/apps/svc_dns", api([both]));
    const running = await screen.findByRole("region", { name: "What runs" });
    await waitFor(() => expect(within(running).getAllByRole("listitem")).toHaveLength(2));
    expect(spy.mock.calls.flat().join(" ")).not.toContain("same key");
    spy.mockRestore();
  });

  it("lists what it runs with, in a project", async () => {
    const stack = aProject("shop", "web", "api", "db");
    renderAt("/apps/svc_shop_web", { ...api(stack), resources: projectPlaces("shop") });
    const panel = await screen.findByRole("region", { name: "Runs with" });
    expect(within(panel).getByRole("link", { name: /api/ })).toHaveAttribute("href", "/apps/svc_shop_api");
    expect(within(panel).getByRole("link", { name: /db/ })).toBeVisible();
  });

  it("names its servers from the second server on, and says when one is offline", async () => {
    const hz2 = aServer({ id: "srv_2", name: "hz-2", connection: Connection.UNREACHABLE });
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
        {
          health: {
            listHealth: () => ({
              services: [
                aHealth(
                  "svc_web",
                  { state: HealthState.NO_CHECK },
                  { state: HealthState.UNKNOWN, server: "srv_2", serverName: "hz-2" },
                ),
              ],
            }),
          },
        },
        hz1,
        hz2,
      ),
    );
    const panel = await screen.findByRole("region", { name: "What runs" });
    await waitFor(() => expect(panel).toHaveTextContent("hz-1"));
    expect(panel).toHaveTextContent("hz-2");
    expect(await screen.findByRole("status")).toHaveTextContent("hz-2 isn't connected");
    await expectAccessible(container);
  });

  it("says a server lost during a deploy one way: what waits is the rollback, and nobody can say what runs", async () => {
    const waiting = anApp(
      "web",
      "nginx:1.27",
      {},
      {
        revision: 3n,
        rolledBackChangeId: "chg_2",
        changeId: "chg_3",
        changeState: ChangeState.APPLYING,
        deployments: [{ serverId: SERVER, serverName: "hz-1", state: DeploymentState.PENDING }],
      },
    );
    const list = [
      aRevision(3n, "nginx:1.27", {
        change: { id: "chg_3", summary: "Roll back", rollback: true, reverts: "chg_2", state: ChangeState.APPLYING },
        deployments: [{ serverId: SERVER, serverName: "hz-1", state: DeploymentState.PENDING }],
      }),
      aRevision(2n, "nginx:1.28", { change: { id: "chg_2", summary: "Deploy", state: ChangeState.FAILED } }),
      aRevision(1n, "nginx:1.27"),
    ];
    const away = aServer({ id: SERVER, name: "hz-1", connection: Connection.UNREACHABLE });
    const { container } = renderAt(
      "/apps/svc_web",
      api(
        [waiting],
        {
          services: {
            listServices: () => ({ services: [waiting] }),
            listServiceRevisions: () => ({ revisions: list }),
          },
        },
        away,
      ),
    );
    const note = await screen.findByRole("status");
    await within(note).findByText(/hz-1 isn't connected/);
    expect(note).toHaveTextContent("Once it reconnects, Dockwell puts nginx:1.27 back");
    expect(note).toHaveTextContent("Until then it can't tell what is running there.");
    expect(note).not.toHaveTextContent("The deploy waits");
    expect(within(note).getByRole("button", { name: "See why" })).toBeVisible();
    expect(container.querySelector(".page-lead")).toHaveTextContent(
      "Dockwell puts nginx:1.27 back once hz-1 reconnects",
    );
    expect(container.querySelector(".page-lead")).not.toHaveTextContent("Deploying");
    const running = screen.getByRole("region", { name: "What runs" });
    expect(running).toHaveTextContent("Unknown until hz-1 reconnects");
    expect(running).not.toHaveTextContent("nginx:1.28");
    const recent = await screen.findByRole("region", { name: "Recent deployments" });
    expect(await within(recent).findByText("Rolling back after a failed deploy")).toBeVisible();
    expect(recent).not.toHaveTextContent("Rolled back after a failed deploy");
    expect(recent).not.toHaveTextContent("Deploying");
    await expectAccessible(container);
  });

  it("agrees with the sheet when a rollback failed, on a fresh read, saying nobody knows what runs", async () => {
    const user = userEvent.setup();
    const stuck = anApp(
      "web",
      "nginx:slow",
      {},
      {
        rolledBackChangeId: "chg_failed",
        changeState: ChangeState.FAILED,
        deployments: [
          {
            serverId: SERVER,
            serverName: "hz-1",
            state: DeploymentState.FAILED,
            error: "hz-1: Cannot connect to the Docker daemon. Is it running?",
          },
        ],
      },
    );
    const asked: string[] = [];
    const { container } = renderAt(
      "/apps/svc_web",
      api([stuck], {
        services: { listServices: () => ({ services: [stuck] }), listServiceRevisions: () => ({ revisions: [] }) },
        changes: {
          getChange: (req) => {
            asked.push(req.id);
            return { change: webPlanned() };
          },
          watchChange: feed<WatchChangeResponse>().stream,
        },
      }),
    );
    const alert = await screen.findByRole("alert");
    expect(alert).toHaveTextContent("Putting web back failed too");
    expect(alert).toHaveTextContent("hz-1: Cannot connect to the Docker daemon. Is it running?");
    expect(alert).toHaveTextContent(
      "web may not be running, or may be running the new version. Check it, then deploy what should run, or fix it on the server.",
    );
    // Not as a deploy that failed, and not as though it runs what its spec says.
    expect(alert).not.toHaveTextContent("The last deploy of web failed");
    const panel = await screen.findByRole("region", { name: "What runs" });
    expect(panel).toHaveTextContent("Unknown, as putting it back failed");
    expect(panel).not.toHaveTextContent("nginx:slow");
    const lead = container.querySelector(".page-lead");
    expect(lead).toHaveTextContent("Putting nginx:slow back failed");
    expect(lead).not.toHaveTextContent("deployed");
    expect(screen.getAllByText("Rollback failed").length).toBeGreaterThan(0);
    await expectAccessible(container);
    await user.click(within(alert).getByRole("button", { name: "See why" }));
    await waitFor(() => expect(asked).toContain("chg_failed"));
  });

  it("says a failed deploy as that, with no word of a rollback, until there is a rollback that failed", async () => {
    const failed = anApp(
      "web",
      "nginx:slow",
      {},
      {
        changeState: ChangeState.FAILED,
        deployments: [{ serverId: SERVER, serverName: "hz-1", state: DeploymentState.FAILED, error: "it exits" }],
      },
    );
    renderAt("/apps/svc_web", api([failed]));
    const alert = await screen.findByRole("alert");
    expect(alert).toHaveTextContent("The last deploy of web failed");
    expect(alert).not.toHaveTextContent("Putting web back");
    expect(await screen.findByRole("region", { name: "What runs" })).toBeVisible();
    expect(screen.queryByText("Rollback failed")).toBeNull();
  });

  it("says a rollback is what happened last in the lead, not that Dockwell deployed", async () => {
    const back = anApp("web", "nginx:1.4", {}, { rolledBackChangeId: "chg_failed" });
    const { container } = renderAt("/apps/svc_web", api([back]));
    await screen.findByRole("status");
    const lead = container.querySelector(".page-lead");
    expect(lead).toHaveTextContent("rolled back by Dockwell");
    expect(lead).not.toHaveTextContent("deployed");
  });

  it("says on a phone that a rollback is what happened last, or that it failed, not that Dockwell deployed", async () => {
    onPhone();
    const back = anApp("web", "nginx:1.4", {}, { rolledBackChangeId: "chg_failed" });
    const first = renderAt("/apps/svc_web", api([back]));
    await first.findByRole("status");
    expect(first.container.querySelector(".app-running-now")).toHaveTextContent("r3 rolled back by Dockwell");
    expect(first.container.querySelector(".app-running-now")).not.toHaveTextContent("deployed");
    first.unmount();

    const stuck = anApp("web", "nginx:slow", {}, { rolledBackChangeId: "chg_failed", changeState: ChangeState.FAILED });
    const second = renderAt("/apps/svc_web", api([stuck]));
    await second.findByRole("alert");
    expect(second.container.querySelector(".app-running-now")).toHaveTextContent("Rolling back failed");
    expect(second.container.querySelector(".app-running-now")).not.toHaveTextContent("deployed");
  });

  it("says Docker's socket for what it is on the page, as the plan did", async () => {
    const withSocket = anApp("agent", "portainer/agent:2", {
      mounts: [
        { source: { case: "hostPath", value: "/var/run/docker.sock" }, target: "/var/run/docker.sock" },
        { source: { case: "hostPath", value: "/srv/site" }, target: "/site", readOnly: true },
      ],
    });
    renderAt("/apps/svc_agent", api([withSocket]));
    const volumes = await screen.findByRole("region", { name: "Volumes" });
    const rows = within(volumes).getAllByRole("listitem");
    expect(rows[0]).toHaveTextContent("Docker's socket");
    expect(rows[0]).not.toHaveTextContent("Server path");
    expect(rows[1]).toHaveTextContent("Server path");
  });

  it("leaves what a failed check said, and what to do about it, as the server says it, in Deploy", async () => {
    const user = userEvent.setup();
    const said =
      "web runs, but fails its health check: GET /healthz on port 8080 answered 503 Service Unavailable. Fix what it answers, or the check, then try again.";
    const failed = moved(webPlanned(), {
      state: ChangeState.FAILED,
      error: said,
      steps: [
        create(StepProgressSchema, {
          serverId: "srv_1",
          resourceId: "res_web",
          state: StepState.FAILED,
          message: said,
          reason: "unhealthy",
        }),
      ],
    });
    const web = anApp("web", "nginx:1.27");
    renderAt(
      "/apps/svc_web",
      api([web], {
        services: {
          listServices: () => ({ services: [web] }),
          listServiceRevisions: () => ({ revisions }),
          deployService: () => ({ change: failed }),
        },
      }),
    );
    await user.click(await screen.findByRole("button", { name: "Deploy" }));
    const form = await screen.findByRole("dialog", { name: "Deploy web" });
    const field = within(form).getByLabelText("Image");
    await user.clear(field);
    await user.type(field, "ghcr.io/acme/web:sick");
    await user.click(within(form).getByRole("button", { name: "Review plan" }));
    const sheet = await screen.findByRole("dialog", { name: "Couldn't replace web" });
    const notice = within(sheet).getByRole("alert");
    expect(notice).toHaveTextContent("answered 503 Service Unavailable");
    expect(notice).toHaveTextContent("Fix what it answers, or the check, then try again.");
    expect(notice).not.toHaveTextContent("Check the image and tag");
  });

  it("says what to check in what the Deploy form has, and lets you leave from there", async () => {
    const user = userEvent.setup();
    const failed = moved(webPlanned(), {
      state: ChangeState.FAILED,
      error: "web stopped right after it started, with exit code 1. Check its command and variables, then try again.",
      steps: [
        create(StepProgressSchema, {
          serverId: "srv_1",
          resourceId: "res_web",
          state: StepState.FAILED,
          message: "x",
          reason: "exits",
        }),
      ],
    });
    renderAt(
      "/apps/svc_web",
      api([web], {
        services: {
          listServices: () => ({ services: [web] }),
          listServiceRevisions: () => ({ revisions }),
          deployService: () => ({ change: failed }),
        },
      }),
    );
    await user.click(await screen.findByRole("button", { name: "Deploy" }));
    const form = await screen.findByRole("dialog", { name: "Deploy web" });
    const field = within(form).getByLabelText("Image");
    await user.clear(field);
    await user.type(field, "ghcr.io/acme/web:broken");
    await user.click(within(form).getByRole("button", { name: "Review plan" }));
    const sheet = await screen.findByRole("dialog", { name: "Couldn't replace web" });
    const notice = within(sheet).getByRole("alert");
    expect(notice).toHaveTextContent("Check the image and tag, then deploy again.");
    expect(notice).not.toHaveTextContent("Check its command and variables");
    await user.click(within(sheet).getByRole("button", { name: /Edit and try again/ }));
    const back = await screen.findByRole("dialog", { name: "Deploy web" });
    expect(within(back).getByLabelText("Image")).toHaveValue("ghcr.io/acme/web:broken");
    await user.click(within(back).getByRole("button", { name: "Review plan" }));
    const again = await screen.findByRole("dialog", { name: "Couldn't replace web" });
    await user.click(within(again).getByRole("button", { name: /^Done/ }));
    await waitFor(() => expect(screen.queryByRole("dialog")).toBeNull());
  });

  it("leaves what a failed check said, and what to do about it, as the server says it, in Add a port", async () => {
    const user = userEvent.setup();
    const said =
      "web runs, but fails its health check: GET /healthz on port 8080 answered 503 Service Unavailable. Fix what it answers, or the check, then try again.";
    const failed = moved(webPlanned(), {
      state: ChangeState.FAILED,
      error: said,
      steps: [
        create(StepProgressSchema, {
          serverId: "srv_1",
          resourceId: "res_web",
          state: StepState.FAILED,
          message: said,
          reason: "unhealthy",
        }),
      ],
    });
    const alone = anApp("alone", "nginx:1.27", { ports: [] });
    renderAt(
      "/apps/svc_alone",
      api([alone], {
        services: {
          listServices: () => ({ services: [alone] }),
          listServiceRevisions: () => ({ revisions }),
          addServicePort: () => ({ change: failed }),
        },
      }),
    );
    await user.click(await screen.findByRole("button", { name: "Add a port" }));
    const form = await screen.findByRole("dialog", { name: "Add a port to alone" });
    await user.type(within(form).getByLabelText(/On the server/), "8080");
    await user.type(within(form).getByLabelText(/In the app/), "80");
    await user.click(within(form).getByRole("button", { name: "Review plan" }));
    const sheet = await screen.findByRole("dialog", { name: "Couldn't replace web" });
    const notice = within(sheet).getByRole("alert");
    expect(notice).toHaveTextContent("answered 503 Service Unavailable");
    expect(notice).toHaveTextContent("Fix what it answers, or the check, then try again.");
    expect(notice).not.toHaveTextContent("Check the port in the app");
  });

  it("says what to check in what the Add a port form has", async () => {
    const user = userEvent.setup();
    const failed = moved(webPlanned(), {
      state: ChangeState.FAILED,
      error: "web stopped right after it started, with exit code 1. Check its command and variables, then try again.",
      steps: [
        create(StepProgressSchema, {
          serverId: "srv_1",
          resourceId: "res_web",
          state: StepState.FAILED,
          message: "x",
          reason: "exits",
        }),
      ],
    });
    const alone = anApp("alone", "nginx:1.27", { ports: [] });
    renderAt(
      "/apps/svc_alone",
      api([alone], {
        services: {
          listServices: () => ({ services: [alone] }),
          listServiceRevisions: () => ({ revisions }),
          addServicePort: () => ({ change: failed }),
        },
      }),
    );
    await user.click(await screen.findByRole("button", { name: "Add a port" }));
    const form = await screen.findByRole("dialog", { name: "Add a port to alone" });
    await user.type(within(form).getByLabelText(/On the server/), "8080");
    await user.type(within(form).getByLabelText(/In the app/), "80");
    await user.click(within(form).getByRole("button", { name: "Review plan" }));
    const sheet = await screen.findByRole("dialog", { name: "Couldn't replace web" });
    expect(within(sheet).getByRole("alert")).toHaveTextContent("Check the port in the app, then try again.");
    await user.click(within(sheet).getByRole("button", { name: /^Done/ }));
    await waitFor(() => expect(screen.queryByRole("dialog")).toBeNull());
  });

  it("says what is going on first, with the way out", async () => {
    const user = userEvent.setup();
    const pending = anApp(
      "web",
      "nginx",
      {},
      { deployments: [{ serverId: SERVER, serverName: "hz-1", state: DeploymentState.PENDING }] },
    );
    const getChange = vi.fn(() => ({ change: webPlanned() }));
    const { unmount } = renderAt(
      "/apps/svc_web",
      api([pending], { changes: { getChange, watchChange: feed<WatchChangeResponse>().stream } }),
    );
    const note = await screen.findByRole("status");
    expect(note).toHaveTextContent("Deploying r3 of web");
    await user.click(within(note).getByRole("button", { name: "Follow it" }));
    await screen.findByRole("dialog");
    expect(getChange).toHaveBeenCalled();
    unmount();

    const failed = anApp(
      "web",
      "nginx",
      {},
      {
        deployments: [
          {
            serverId: SERVER,
            serverName: "hz-1",
            state: DeploymentState.FAILED,
            error: "hz-1: applied service web in env_1, but it isn't healthy",
          },
        ],
      },
    );
    const second = renderAt("/apps/svc_web", api([failed]));
    const alert = await second.findByRole("alert");
    expect(alert).toHaveTextContent("The last deploy of web failed");
    expect(alert).toHaveTextContent("hz-1: applied web, but it isn't healthy");
    expect(alert).not.toHaveTextContent("env_1");
    second.unmount();

    const back = anApp("web", "nginx:1.4", {}, { rolledBackChangeId: "chg_failed" });
    renderAt("/apps/svc_web", api([back], { health: running(back) }));
    const said = await screen.findByRole("status");
    expect(said).toHaveTextContent("The last deploy failed, so Dockwell rolled it back");
    await waitFor(() => expect(said).toHaveTextContent("web is on nginx:1.4, as it was before that deploy."));
    expect(said).not.toHaveTextContent("runs");
    expect(said).not.toHaveTextContent("again, as");
    expect(within(said).getByRole("button", { name: "See why" })).toBeVisible();
  });

  it("deploys another image, as a plan, keeping what it refuses at the field", async () => {
    const user = userEvent.setup();
    const asked: string[] = [];
    renderAt(
      "/apps/svc_web",
      api([web], {
        services: {
          listServices: () => ({ services: [web] }),
          listServiceRevisions: () => ({ revisions }),
          deployService: (req) => {
            asked.push(req.image);
            if (req.image === "bad image") {
              const err = new ConnectError(
                'the image "bad image" has a space. Write it as nginx:1.27',
                Code.InvalidArgument,
              );
              err.metadata.set("Dockwell-Error", "invalid");
              throw err;
            }
            return { change: webPlanned() };
          },
        },
      }),
    );
    await user.click(await screen.findByRole("button", { name: "Deploy" }));
    const sheet = await screen.findByRole("dialog", { name: "Deploy web" });
    const field = within(sheet).getByLabelText("Image");
    expect(field).toHaveValue("ghcr.io/acme/web:1.5");
    await expectAccessible(document.body);
    await user.clear(field);
    await user.type(field, "bad image");
    await user.click(within(sheet).getByRole("button", { name: "Review plan" }));
    await waitFor(() => expect(field).toHaveAccessibleDescription(/has a space/));
    await user.clear(field);
    await user.type(field, "ghcr.io/acme/web:1.6");
    await user.click(within(sheet).getByRole("button", { name: "Review plan" }));
    expect(await screen.findByRole("dialog", { name: "Review plan" })).toBeVisible();
    expect(asked).toEqual(["bad image", "ghcr.io/acme/web:1.6"]);
  });

  it("rolls back in one click from the live deployment, as a plan", async () => {
    const user = userEvent.setup();
    const asked: RollBackServiceRequest[] = [];
    renderAt(
      "/apps/svc_web",
      api([web], {
        services: {
          listServices: () => ({ services: [web] }),
          listServiceRevisions: () => ({ revisions }),
          rollBackService: (req) => {
            asked.push(req);
            return { change: webPlanned() };
          },
        },
      }),
    );
    await user.click(await screen.findByRole("button", { name: "Roll back to r2" }));
    expect(await screen.findByRole("dialog", { name: "Review plan" })).toBeVisible();
    expect(asked[0]).toMatchObject({ id: "svc_web", revision: 2n, summary: "Roll back web to r2" });
  });

  it("offers the way back to what differs, not to what it already runs, after an automatic rollback", async () => {
    const back = anApp("web", "nginx:1.5", {}, { revision: 5n, rolledBackChangeId: "chg_4" });
    renderAt(
      "/apps/svc_web",
      api([back], {
        services: {
          listServices: () => ({ services: [back] }),
          listServiceRevisions: () => ({ revisions: afterAutoRollback }),
        },
      }),
    );
    expect(await screen.findByRole("button", { name: "Roll back to r2" })).toBeVisible();
    expect(screen.queryByRole("button", { name: "Roll back to r3" })).not.toBeInTheDocument();
  });

  it("says each deployment in the past tense, without the app's name", async () => {
    const list = [
      aRevision(4n, "ghcr.io/acme/web:1.6", {
        rolledBackTo: 2n,
        change: { id: "chg_4", summary: "Roll back web to r2", reverts: "chg_3", state: ChangeState.APPLIED },
      }),
      aRevision(3n, "ghcr.io/acme/web:1.6", {
        changes: [
          { path: "app.image", op: FieldOp.MODIFIED, from: "ghcr.io/acme/web:1.5", to: "ghcr.io/acme/web:1.6" },
        ],
      }),
      aRevision(2n, "ghcr.io/acme/web:1.5", {
        changes: [{ path: "app.ports[0]", op: FieldOp.ADDED, to: "8080:80" }],
      }),
      aRevision(1n, "ghcr.io/acme/web:1.5"),
    ];
    renderAt(
      "/apps/svc_web",
      api([web], {
        services: { listServices: () => ({ services: [web] }), listServiceRevisions: () => ({ revisions: list }) },
      }),
    );
    const panel = await screen.findByRole("region", { name: "Recent deployments" });
    expect(await within(panel).findByText("Rolled back to r2")).toBeVisible();
    expect(panel).toHaveTextContent("Deployed web:1.6");
    expect(panel).toHaveTextContent("Changed ports");
    expect(panel).toHaveTextContent("Created from web:1.5");
    expect(panel).not.toHaveTextContent(/Deploy ghcr|Roll back web|Run /);
  });

  it("deletes through a plan, then goes to the list", async () => {
    const user = userEvent.setup();
    const proposals: { edits: { delete: boolean; resource?: { name: string } }[] }[] = [];
    const applied = webPlanned();
    renderAt(
      "/apps/svc_web",
      api([web], {
        changes: {
          proposeChange: (req) => {
            proposals.push(req);
            return { change: applied };
          },
          watchChange: feed<WatchChangeResponse>().stream,
          discardChange: () => ({}),
        },
      }),
    );
    await user.click(await screen.findByRole("button", { name: "Delete this app" }));
    await screen.findByRole("dialog", { name: "Review plan" });
    expect(proposals[0]?.edits[0]).toMatchObject({ delete: true, resource: { name: "web" } });
  });

  it("exports the app or its project as a file with no secrets, and says what it left out", async () => {
    const user = userEvent.setup();
    const asked: { service: string; environment: string }[] = [];
    renderAt(
      "/apps/svc_web",
      api([web], {
        compose: {
          exportCompose: (req) => {
            asked.push(req);
            return { compose: `name: x\nservices:\n  ${req.service || "all"}: {}\n`, omitted: ["web.API_KEY"] };
          },
        },
      }),
    );
    await user.click(await screen.findByRole("button", { name: "Export as Compose" }));
    const sheet = await screen.findByRole("dialog", { name: "Export as Compose" });
    const file = await within(sheet).findByRole("region", { name: "Compose file" });
    await waitFor(() => expect(file).toHaveTextContent("web: {}"));
    expect(sheet).toHaveTextContent("Left out as they may hold secrets");
    expect(sheet).toHaveTextContent("web.API_KEY");
    expect(asked[0]).toEqual(expect.objectContaining({ service: "web", environment: "env_prod" }));
    await expectAccessible(document.body);
    await user.click(within(sheet).getByRole("radio", { name: "Its whole project" }));
    await waitFor(() => expect(file).toHaveTextContent("all: {}"));
    await user.click(within(sheet).getByRole("button", { name: "Copy" }));
    expect(await within(sheet).findByRole("button", { name: "Copied" })).toBeVisible();
  });

  it("reads the file again each time it opens, as a deploy may have changed it", async () => {
    const user = userEvent.setup();
    let image = "nginx:1";
    const read = vi.fn(() => ({ compose: `services:\n  web:\n    image: ${image}\n`, omitted: [] }));
    renderAt("/apps/svc_web", api([web], { compose: { exportCompose: read } }));
    await user.click(await screen.findByRole("button", { name: "Export as Compose" }));
    const first = await screen.findByRole("dialog", { name: "Export as Compose" });
    await within(first).findByText(/nginx:1/);
    await user.keyboard("{Escape}");
    await waitFor(() => expect(screen.queryByRole("dialog", { name: "Export as Compose" })).not.toBeInTheDocument());

    image = "nginx:2";
    await user.click(screen.getByRole("button", { name: "Export as Compose" }));
    const again = await screen.findByRole("dialog", { name: "Export as Compose" });
    await within(again).findByText(/nginx:2/);
    expect(again).not.toHaveTextContent("nginx:1");
  });
});

describe("an app's deployments", () => {
  it("lists revisions with what changed, who did it and from where, the live one open beside them", async () => {
    const { container } = renderAt("/apps/svc_web/deployments", api());
    const table = await screen.findByRole("table", { name: "Revisions of web" });
    const r3 = await within(table).findByRole("row", { name: /r3/ });
    expect(r3).not.toHaveTextContent("Live");
    expect(r3).toHaveTextContent("Deployed web:1.5");
    expect(r3).not.toHaveTextContent("to web");
    expect(r3).toHaveTextContent("Mira Chen");
    expect(r3).toHaveTextContent("From the panel");
    expect(r3).toHaveAttribute("aria-current", "true");
    // Only that a secret changed.
    const r2 = within(table).getByRole("row", { name: /r2/ });
    expect(r2).toHaveTextContent("API_KEY");
    expect(r2).toHaveTextContent("changed");
    expect(r2).toHaveTextContent("Replaced");
    // The way back from what is live is the one before.
    expect(screen.getByRole("button", { name: "Roll back to r2" })).toBeVisible();
    // The words the plan was made with are its Plan fact.
    expect(screen.getByText("Deploy ghcr.io/acme/web:1.5 to web")).toBeVisible();
    await expectAccessible(container);
  });

  it("says a revision matches the latest, and still lets someone put it back", async () => {
    const back = anApp("web", "nginx:1.5", {}, { revision: 5n, rolledBackChangeId: "chg_4" });
    const asked: bigint[] = [];
    const user = userEvent.setup();
    const { container } = renderAt(
      "/apps/svc_web/deployments?revision=3",
      api([back], {
        services: {
          listServices: () => ({ services: [back] }),
          listServiceRevisions: () => ({ revisions: afterAutoRollback }),
          rollBackService: (req) => {
            asked.push(req.revision);
            throw new ConnectError("web already runs what revision 3 had");
          },
        },
      }),
    );
    expect(await screen.findByText(/^r3 matches the latest revision\./)).toBeVisible();
    // The row that is open says so to whoever can't see its colour.
    expect(screen.getByRole("row", { name: /^r3/ })).toHaveAttribute("aria-selected", "true");
    expect(screen.getByRole("row", { name: /^r2/ })).toHaveAttribute("aria-selected", "false");
    await expectAccessible(container);
    // The record can match while what runs doesn't, so only the plan can say.
    await user.click(screen.getByRole("button", { name: "Roll back to r3" }));
    await waitFor(() => expect(asked).toEqual([3n]));
  });

  it("offers the newest that differs from the one that runs", async () => {
    const back = anApp("web", "nginx:1.5", {}, { revision: 5n, rolledBackChangeId: "chg_4" });
    renderAt(
      "/apps/svc_web/deployments",
      api([back], {
        services: {
          listServices: () => ({ services: [back] }),
          listServiceRevisions: () => ({ revisions: afterAutoRollback }),
        },
      }),
    );
    expect(await screen.findByRole("button", { name: "Roll back to r2" })).toBeVisible();
  });

  it("agrees with the header when the server that runs it is offline", async () => {
    const hzOff = aServer({ id: SERVER, name: "hz-1", connection: Connection.UNREACHABLE });
    renderAt("/apps/svc_web/deployments", api([web], {}, hzOff));
    const table = await screen.findByRole("table", { name: "Revisions of web" });
    const r3 = await within(table).findByRole("row", { name: /r3/ });
    await waitFor(() => expect(r3).toHaveTextContent("hz-1 offline"));
    expect(r3).not.toHaveTextContent("Live");
  });

  it("says failed for what failed, whatever is still marked as on its way", async () => {
    const stuck = aRevision(3n, "nginx:bad", {
      change: { id: "chg_3", summary: "Deploy", state: ChangeState.FAILED, error: "the rollback couldn't commit" },
      deployments: [{ serverId: SERVER, serverName: "hz-1", state: DeploymentState.PENDING }],
    });
    const failing = anApp(
      "web",
      "nginx:bad",
      {},
      {
        changeState: ChangeState.FAILED,
        deployments: [{ serverId: SERVER, serverName: "hz-1", state: DeploymentState.PENDING }],
      },
    );
    renderAt(
      "/apps/svc_web/deployments",
      api([failing], {
        services: {
          listServices: () => ({ services: [failing] }),
          listServiceRevisions: () => ({ revisions: [stuck] }),
        },
      }),
    );
    const table = await screen.findByRole("table", { name: "Revisions of web" });
    expect(await within(table).findByRole("row", { name: /r3/ })).toHaveTextContent("Failed");
  });

  it("shows an image change by what moved, each value whole, with the full name in its fact", async () => {
    const list = [
      aRevision(4n, "getmeili/meilisearch:broken", {
        changes: [
          {
            path: "app.image",
            op: FieldOp.MODIFIED,
            from: "getmeili/meilisearch:v1.11",
            to: "getmeili/meilisearch:broken",
          },
        ],
      }),
    ];
    const { container } = renderAt(
      "/apps/svc_web/deployments",
      api([web], {
        services: { listServices: () => ({ services: [web] }), listServiceRevisions: () => ({ revisions: list }) },
      }),
    );
    await screen.findByRole("table", { name: "Revisions of web" });
    const diff = await waitFor(() => {
      const found = container.querySelector(".deploy-diff");
      if (!found) throw new Error("no diff yet");
      return found as HTMLElement;
    });
    expect(within(diff).getByText("v1.11").closest("del")).toBeInTheDocument();
    expect(within(diff).getByText("broken").closest("ins")).toBeInTheDocument();
    expect(diff).not.toHaveTextContent("meilisearch");
    expect(container.querySelector(".deploy-image")).toHaveTextContent("getmeili/meilisearch:broken");
  });

  it("reads what a revision changed in the plan's words, each value whole, the old then an arrow then the new", async () => {
    const list = [
      aRevision(2n, "nginx:1.5", {
        changes: [
          { path: "app.image", op: FieldOp.MODIFIED, from: "nginx:1.4", to: "nginx:1.5" },
          { path: "app.ports[0].host_port", op: FieldOp.ADDED, to: "8080" },
          { path: "app.ports[0].container_port", op: FieldOp.ADDED, to: "80" },
          { path: "app.ports[0].protocol", op: FieldOp.ADDED, to: "tcp" },
          { path: "app.mounts[0].volume", op: FieldOp.ADDED, to: "web-data" },
          { path: "app.mounts[0].target", op: FieldOp.ADDED, to: "/data" },
          { path: "app.env[API_KEY]", op: FieldOp.MODIFIED, sensitive: true },
        ],
      }),
      aRevision(1n, "nginx:1.4"),
    ];
    const { container } = renderAt(
      "/apps/svc_web/deployments",
      api([web], {
        services: { listServices: () => ({ services: [web] }), listServiceRevisions: () => ({ revisions: list }) },
      }),
    );
    await screen.findByRole("table", { name: "Revisions of web" });
    const diff = await waitFor(() => {
      const found = container.querySelector(".deploy-diff");
      if (!found) throw new Error("no diff yet");
      return found as HTMLElement;
    });
    const row = (label: string) => within(diff).getByText(label).closest(".deploy-diff-row") as HTMLElement;
    expect(diff).not.toHaveTextContent("ports[0]");
    expect(diff).not.toHaveTextContent("mounts[0]");
    expect(row("port").querySelector("ins")).toHaveTextContent("8080 → 80, TCP");
    expect(row("volume").querySelector("ins")).toHaveTextContent("web-data at /data");
    expect(row("volume").querySelector("ins")).toHaveClass("is-words");
    // The old value, an arrow, then the new, as whole values, and a field added has no arrow.
    const image = row("image");
    expect(image.querySelector("del")).toHaveTextContent("1.4");
    expect(image.querySelector("ins")).toHaveTextContent("1.5");
    const arrow = image.querySelector(".deploy-diff-arrow") as Element;
    expect(arrow).toBeInTheDocument();
    const follows = (a: Element | null, b: Element | null) =>
      ((a?.compareDocumentPosition(b as Element) ?? 0) & Node.DOCUMENT_POSITION_FOLLOWING) !== 0;
    expect(follows(image.querySelector("del"), arrow)).toBe(true);
    expect(follows(arrow, image.querySelector("ins"))).toBe(true);
    expect(row("port").querySelector(".deploy-diff-arrow")).toBeNull();
    // A variable says it changed, and never what it holds.
    expect(row("variable")).toHaveTextContent("API_KEY");
    expect(row("variable")).toHaveTextContent("Changed, and hidden");
    await expectAccessible(container);
  });

  it("opens the one you choose, in its address, with the way back to it", async () => {
    const user = userEvent.setup();
    const asked: bigint[] = [];
    const { router } = renderAt(
      "/apps/svc_web/deployments",
      api([web], {
        services: {
          listServices: () => ({ services: [web] }),
          listServiceRevisions: () => ({ revisions }),
          rollBackService: (req) => {
            asked.push(req.revision);
            return { change: webPlanned() };
          },
        },
      }),
    );
    await user.click(await screen.findByRole("row", { name: /r1/ }));
    await waitFor(() => expect(router.state.location.search).toEqual({ revision: "1" }));
    await user.click(screen.getByRole("button", { name: "Roll back to r1" }));
    expect(await screen.findByRole("dialog", { name: "Review plan" })).toBeVisible();
    expect(asked).toEqual([1n]);
  });

  it("says why one failed, and offers no way back to it", async () => {
    const failed = aRevision(4n, "nginx:broken", {
      change: {
        id: "chg_4",
        summary: "Deploy nginx:broken to web",
        state: ChangeState.FAILED,
        error: "it exits",
        rolledBackBy: "chg_5",
        rollbackStatus: { id: "chg_5", state: RollbackState.APPLIED, steps: 1, restored: ["web"] },
        proposedVia: Surface.PANEL,
      },
      changes: [{ path: "image", op: FieldOp.MODIFIED, from: "nginx:1.5", to: "nginx:broken" }],
      deployments: [{ serverId: SERVER, serverName: "hz-1", state: DeploymentState.FAILED, error: "hz-1: it exits" }],
    });
    const list = [
      aRevision(5n, "nginx:1.5", {
        change: {
          id: "chg_5",
          summary: "Roll back Deploy",
          rollback: true,
          reverts: "chg_4",
          state: ChangeState.APPLIED,
        },
      }),
      failed,
    ];
    const { container } = renderAt(
      "/apps/svc_web/deployments?revision=4",
      api([web], {
        services: { listServices: () => ({ services: [web] }), listServiceRevisions: () => ({ revisions: list }) },
      }),
    );
    const alert = await screen.findByRole("alert");
    // A machine value keeps its case, and what happened leads.
    expect(alert).toHaveTextContent("hz-1: it exits");
    // It says what runs now, from how the rollback went, in the app's own words.
    expect(screen.getByRole("status")).toHaveTextContent("Dockwell put nginx:1.5 back on web");
    expect(screen.getByRole("status")).toHaveTextContent("so it was down while that happened");
    expect(screen.queryByRole("button", { name: /Roll back to/ })).not.toBeInTheDocument();
    expect(screen.getByRole("row", { name: /r5/ })).toHaveTextContent("Rolled back after a failed deploy");
    await expectAccessible(container);
  });

  it("titles a revision from what its plan says it was for, when it says", async () => {
    const list = [
      aRevision(6n, "nginx:latest", {
        change: {
          id: "chg_6",
          summary: "Pull again",
          state: ChangeState.APPLIED,
          plan: { intent: { kind: IntentKind.REPULL, resourceId: "svc_web", image: "docker.io/library/nginx:latest" } },
        },
      }),
      aRevision(5n, "nginx:1.5", {
        change: {
          id: "chg_5",
          summary: "A port",
          state: ChangeState.APPLIED,
          plan: { intent: { kind: IntentKind.PUBLISH_PORT, resourceId: "svc_web", port: 9411 } },
        },
        // What it changed reads otherwise, and what the plan says wins.
        changes: [{ path: "app.restart_policy", op: FieldOp.MODIFIED, from: "no", to: "always" }],
      }),
      aRevision(4n, "nginx:broken", {
        change: {
          id: "chg_4",
          summary: "A deploy",
          state: ChangeState.FAILED,
          plan: { intent: { kind: IntentKind.DEPLOY, resourceId: "svc_web", image: "nginx:broken" } },
        },
      }),
    ];
    renderAt(
      "/apps/svc_web/deployments",
      api([web], {
        services: { listServices: () => ({ services: [web] }), listServiceRevisions: () => ({ revisions: list }) },
      }),
    );
    expect(await screen.findByRole("row", { name: /r6/ })).toHaveTextContent("Pulled nginx:latest again");
    expect(screen.getByRole("row", { name: /r5/ })).toHaveTextContent("Published port 9411");
    const failed = screen.getByRole("row", { name: /r4/ });
    expect(failed).toHaveTextContent("Couldn't deploy nginx:broken");
    expect(within(failed).getByText("nginx:broken").tagName).toBe("CODE");
  });

  it("doesn't say a rollback happened until it did", async () => {
    const rollingBack = (state: ChangeState) =>
      aRevision(5n, "nginx:1.5", {
        change: { id: "chg_5", summary: "Roll back Deploy", rollback: true, reverts: "chg_4", state },
      });
    for (const [state, words] of [
      [ChangeState.APPLYING, "Rolling back after a failed deploy"],
      [ChangeState.FAILED, "Rolling back after a failed deploy failed too"],
    ] as const) {
      const { unmount } = renderAt(
        "/apps/svc_web/deployments",
        api([web], {
          services: {
            listServices: () => ({ services: [web] }),
            listServiceRevisions: () => ({ revisions: [rollingBack(state)] }),
          },
        }),
      );
      const row = await screen.findByRole("row", { name: /r5/ });
      expect(row).toHaveTextContent(words);
      if (state === ChangeState.APPLYING) {
        expect(row).toHaveTextContent("Rolling back");
        expect(row).not.toHaveTextContent("Deploying");
      }
      unmount();
    }
  });

  it("reads older revisions a page at a time", async () => {
    const user = userEvent.setup();
    const list = vi.fn((req: { pageToken: string }) =>
      req.pageToken === ""
        ? { revisions: revisions.slice(0, 2), nextPageToken: "more" }
        : { revisions: revisions.slice(2) },
    );
    renderAt(
      "/apps/svc_web/deployments",
      api([web], { services: { listServices: () => ({ services: [web] }), listServiceRevisions: list } }),
    );
    await screen.findByRole("row", { name: /r2/ });
    expect(screen.getByText("Showing r2 to r3")).toBeVisible();
    await user.click(screen.getByRole("button", { name: "Show older revisions" }));
    expect(await screen.findByRole("row", { name: /r1/ })).toBeVisible();
    expect(screen.queryByRole("button", { name: "Show older revisions" })).not.toBeInTheDocument();
  });

  it("says what is wrong when it can't load", async () => {
    renderAt(
      "/apps/svc_web/deployments",
      api([web], {
        services: {
          listServices: () => ({ services: [web] }),
          listServiceRevisions: () => {
            throw new ConnectError("the control plane couldn't read its services", Code.Internal);
          },
        },
      }),
    );
    expect(await screen.findByRole("alert")).toHaveTextContent("Can't load the deployments");
  });

  it("opens a revision in a sheet on a phone", async () => {
    onPhone();
    const user = userEvent.setup();
    const { container } = renderAt("/apps/svc_web/deployments", api());
    const row = await screen.findByRole("button", { name: /r2/ });
    expect(screen.queryByRole("dialog")).not.toBeInTheDocument();
    await expectAccessible(container);
    await user.click(row);
    const sheet = await screen.findByRole("dialog", { name: "r2" });
    expect(within(sheet).getByRole("button", { name: "Roll back to r2" })).toBeVisible();
    await expectAccessible(document.body);
  });
});
