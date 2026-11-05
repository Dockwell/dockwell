// SPDX-License-Identifier: AGPL-3.0-only

import { Code, ConnectError } from "@connectrpc/connect";
import {
  ActorKind,
  type AddServicePortRequest,
  ChangeState,
  Connection,
  DeploymentState,
  FieldOp,
  Protocol,
  type RollBackServiceRequest,
  Surface,
  type WatchChangeResponse,
  type WatchServersResponse,
} from "@dockwell/api";
import { screen, waitFor, within } from "@testing-library/react";
import userEvent from "@testing-library/user-event";
import { afterEach, describe, expect, it, vi } from "vitest";

import { activateLocale } from "../../app/i18n";
import { expectAccessible } from "../../test/a11y";
import { anApp, aProject, aRevision, places, projectPlaces, SERVER } from "../../test/apps";
import { webPlanned } from "../../test/changes";
import { type Api, feed, onPhone, renderAt } from "../../test/render";
import { aServer, changed, snapshotComplete } from "../../test/servers";

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

describe("an app's frame", () => {
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

  it("gives every tab a place, including the logs task's", async () => {
    const user = userEvent.setup();
    const { router } = renderAt("/apps/svc_web", api());
    const tabs = await screen.findByRole("navigation", { name: "App sections" });
    await user.click(within(tabs).getByRole("link", { name: "Logs" }));
    await waitFor(() => expect(router.state.location.pathname).toBe("/apps/svc_web/logs"));
    expect(screen.getByRole("heading", { level: 1, name: "web" })).toBeVisible();
    // Until they are here, the command that reads them, with the app's own name, to copy.
    expect(await screen.findByText("docker logs env_prod-web-1")).toBeVisible();
    expect(screen.getByRole("button", { name: "Copy the command" })).toBeVisible();
    await user.click(within(tabs).getByRole("link", { name: "Environment" }));
    expect(await screen.findByRole("cell", { name: "API_KEY" })).toBeVisible();
    expect(screen.getAllByRole("cell", { name: "Hidden" })).toHaveLength(2);
  });

  it("keeps Deploy in reach on a phone, and tells the app's state in the header", async () => {
    onPhone();
    const { container } = renderAt("/apps/svc_web", api());
    expect(await screen.findByRole("heading", { level: 1, name: "web" })).toBeVisible();
    expect(screen.getByText("r3 · Live · web:1.5")).toBeVisible();
    expect(container.querySelector(".app-actionbar")).toHaveTextContent(/^Deploy$/);
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
  it("says what runs, where it is reached, and how it is set up", async () => {
    const { container } = renderAt("/apps/svc_web", api());
    const running = await screen.findByRole("region", { name: "Running" });
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
    const running = await screen.findByRole("region", { name: "Running" });
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
    const running = await screen.findByRole("region", { name: "Running" });
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
    const running = await screen.findByRole("region", { name: "Running" });
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

  it("shows no metric or restart it has no API for", async () => {
    renderAt("/apps/svc_web", api());
    await screen.findByRole("region", { name: "Running" });
    for (const name of ["Metrics", "Replicas", "Health check"]) {
      expect(screen.queryByRole("region", { name })).not.toBeInTheDocument();
    }
    expect(screen.queryByRole("button", { name: "Restart" })).not.toBeInTheDocument();
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
    const running = await screen.findByRole("region", { name: "Running" });
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

    await user.clear(within(sheet).getByLabelText("On the server"));
    await user.type(within(sheet).getByLabelText("On the server"), "8080");
    await user.click(within(sheet).getByRole("button", { name: "Review plan" }));
    expect(await screen.findByRole("dialog", { name: "Review plan" })).toBeVisible();
    expect(asked.at(-1)).toMatchObject({ id: "svc_alone", hostPort: 8080, containerPort: 80, protocol: Protocol.TCP });
  });

  it("offers another port once it has one", async () => {
    renderAt("/apps/svc_web", api());
    const running = await screen.findByRole("region", { name: "Running" });
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
    const running = await screen.findByRole("region", { name: "Running" });
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
    const { container } = renderAt("/apps/svc_web", api([both], {}, hz1, hz2));
    const table = await screen.findByRole("table", { name: "Where it runs" });
    expect(within(table).getByRole("row", { name: /hz-1/ })).toHaveTextContent("Live");
    expect(await within(table).findByRole("row", { name: /hz-2 .*Offline/ })).toBeVisible();
    expect(await screen.findByRole("status")).toHaveTextContent("hz-2 isn't connected");
    await expectAccessible(container);
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
    renderAt("/apps/svc_web", api([back]));
    const said = await screen.findByRole("status");
    expect(said).toHaveTextContent("The last deploy failed, so Dockwell rolled it back");
    expect(said).toHaveTextContent("web runs nginx:1.4 again");
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
    expect(r3).toHaveTextContent("Live");
    expect(r3).toHaveTextContent("Deployed web:1.5");
    expect(r3).not.toHaveTextContent("to web");
    expect(r3).toHaveTextContent("Mira Chen");
    expect(r3).toHaveTextContent("From the panel");
    expect(r3).toHaveAttribute("aria-current", "true");
    // Only that a secret changed.
    const r2 = within(table).getByRole("row", { name: /r2/ });
    expect(r2).toHaveTextContent("env[API_KEY]");
    expect(r2).toHaveTextContent("changed");
    expect(r2).toHaveTextContent("Replaced");
    // The way back from what is live is the one before.
    expect(screen.getByRole("button", { name: "Roll back to r2" })).toBeVisible();
    // The words the plan was made with are its Plan fact.
    expect(screen.getByText("Deploy ghcr.io/acme/web:1.5 to web")).toBeVisible();
    await expectAccessible(container);
  });

  it("says a revision that matches what runs already matches, with no way back to it", async () => {
    const back = anApp("web", "nginx:1.5", {}, { revision: 5n, rolledBackChangeId: "chg_4" });
    const { container } = renderAt(
      "/apps/svc_web/deployments?revision=3",
      api([back], {
        services: {
          listServices: () => ({ services: [back] }),
          listServiceRevisions: () => ({ revisions: afterAutoRollback }),
        },
      }),
    );
    expect(await screen.findByText("r3 already matches what runs")).toBeVisible();
    expect(screen.queryByRole("button", { name: /Roll back to/ })).not.toBeInTheDocument();
    await expectAccessible(container);
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
        proposedVia: Surface.PANEL,
      },
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
    expect(alert).toHaveTextContent("Why it failed");
    expect(alert).toHaveTextContent("hz-1: it exits");
    expect(alert).toHaveTextContent("Dockwell rolled back the steps already done.");
    expect(screen.queryByRole("button", { name: /Roll back to/ })).not.toBeInTheDocument();
    expect(screen.getByRole("row", { name: /r5/ })).toHaveTextContent("Rolled back after a failed deploy");
    await expectAccessible(container);
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
