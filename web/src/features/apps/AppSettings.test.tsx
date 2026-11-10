// SPDX-License-Identifier: AGPL-3.0-only

import { create } from "@bufbuild/protobuf";
import { Code, ConnectError } from "@connectrpc/connect";
import {
  ChangeState,
  type Container,
  HealthcheckSchema,
  HostAccessKind,
  HostAccessSchema,
  ImageAnswer,
  MountSchema,
  PortMappingSchema,
  type ProposeChangeRequest,
  RestartPolicy,
  type ServiceInfo,
  type WatchChangeResponse,
  type WatchEventsResponse,
  type WatchServersResponse,
} from "@dockwell/api";
import { act, screen, waitFor, within } from "@testing-library/react";
import userEvent from "@testing-library/user-event";
import { describe, expect, it, vi } from "vitest";

import { expectAccessible } from "../../test/a11y";
import { anApp, aRevision, places } from "../../test/apps";
import { changeEvent, moved, since, snapshot, watched, webPlanned } from "../../test/changes";
import { type Api, feed, onPhone, renderAt } from "../../test/render";
import { aServer, changed, snapshotComplete } from "../../test/servers";

/** An app with its own values set, a port, a volume, and a command. */
function anEditedApp(): ServiceInfo {
  const service = anApp("web", "nginx:1.5", {
    env: { API_KEY: "", OWN: "" },
    command: ["serve", "--port", "80"],
    ports: [{ hostPort: 8080, containerPort: 80 }],
    mounts: [{ source: { case: "volume", value: "data" }, target: "/data" }],
    restart: RestartPolicy.UNLESS_STOPPED,
  });
  service.revision = 4n;
  return service;
}

function api(service: ServiceInfo | (() => ServiceInfo), proposals: ProposeChangeRequest[] = []): Api {
  const current = typeof service === "function" ? service : () => service;
  const watch = feed<WatchServersResponse>();
  watch.push(changed(aServer({ id: "srv_1", name: "hz-1" })), snapshotComplete);
  return {
    resources: {
      ...places(),
      listResources: (req) =>
        req.kind === "volume"
          ? { resources: [{ id: "vol_1", resource: { kind: "volume", scope: "env_prod", name: "data" } }] }
          : places().listResources(req),
    },
    services: {
      listServices: () => ({ services: [current()] }),
      listServiceRevisions: () => ({ revisions: [aRevision(4n, "nginx:1.5")] }),
    },
    servers: { watchServers: watch.stream },
    domains: { listDomains: () => ({}) },
    changes: {
      proposeChange: (req: ProposeChangeRequest) => {
        proposals.push(req);
        return { change: webPlanned() };
      },
      watchChange: feed<WatchChangeResponse>().stream,
      discardChange: () => ({}),
    },
  };
}

const section = (name: string) => screen.getByRole("region", { name });
const ready = () => screen.findByRole("region", { name: "General" }, { timeout: 5000 });
const appOf = (request: ProposeChangeRequest | undefined): Container | undefined => {
  const edit = request?.edits.find((e) => e.resource?.kind === "service");
  const spec = edit?.spec?.kind.case === "service" ? edit.spec.kind.value : undefined;
  return spec?.runs.case === "app" ? spec.runs.value : undefined;
};

describe("an app's settings", () => {
  it("says what its image runs where the app sets nothing, and offers limits that fit its server", async () => {
    const user = userEvent.setup();
    const service = anApp("web", "nginx:1.27", { ports: [{ hostPort: 8080, containerPort: 80 }] });
    service.revision = 4n;
    const watch = feed<WatchServersResponse>();
    watch.push(
      changed(
        aServer({
          id: "srv_1",
          name: "hz-1",
          inventory: { osName: "Ubuntu 24.04.1 LTS", cpuCount: 2, memoryBytes: 2n << 30n },
        }),
      ),
      snapshotComplete,
    );
    renderAt("/apps/svc_web/settings", {
      ...api(service),
      servers: { watchServers: watch.stream },
      images: {
        getImage: () => ({
          answer: ImageAnswer.FOUND,
          config: {
            command: ["nginx", "-g", "daemon off;"],
            workingDir: "/usr/share/nginx",
            healthcheck: ["CMD-SHELL", "curl -f http://localhost/"],
            ports: [{ port: 80 }],
          },
        }),
      },
    });
    await ready();
    const general = await screen.findByRole("region", { name: "General" });
    await waitFor(() => expect(general).toHaveTextContent("nginx -g 'daemon off;' the image's"));
    expect(general).toHaveTextContent("root the image's");
    expect(general).toHaveTextContent("/usr/share/nginx the image's");
    expect(screen.getByRole("region", { name: "Health check" })).toHaveTextContent(
      "curl -f http://localhost/ the image's",
    );
    await user.click(screen.getByRole("button", { name: "Edit Resources" }));
    const form = screen.getByRole("form", { name: "Edit Resources" });
    const memory = within(form).getByRole("combobox", { name: "Memory limit" });
    expect(
      within(memory)
        .getAllByRole("option")
        .map((o) => o.textContent),
    ).toEqual(["No limit", "256M", "512M", "1G", "Other"]);
    expect(memory).toHaveAccessibleDescription(/^hz-1 has 2/);
    expect(within(form).getByRole("combobox", { name: "CPU limit" })).toHaveAccessibleDescription("hz-1 has 2 vCPU.");
  });

  it("shows how it runs, section by section, each with its own Edit", async () => {
    const { container } = renderAt("/apps/svc_web/settings", api(anEditedApp()));
    await ready();
    expect(within(section("General")).getByText("serve --port 80")).toBeVisible();
    expect(within(section("Ports")).getByText("8080")).toBeVisible();
    expect(within(section("Volumes and mounts")).getByText("/data")).toBeVisible();
    expect(within(section("Resources")).getAllByText("No limit")).toHaveLength(2);
    expect(within(section("General")).getByText("nginx:1.5")).toBeVisible();
    expect(within(section("Danger zone")).getByText("Delete web", { selector: ".app-danger-title" })).toBeVisible();
    expect(within(section("Danger zone")).getByRole("button", { name: "Delete web" })).toHaveClass("dw-btn-danger");
    expect(screen.getByRole("navigation", { name: "Settings sections" })).toBeVisible();
    await expectAccessible(container);
  });

  it("plans a new command from the app as it was read, keeping its own values unread", async () => {
    const user = userEvent.setup();
    const proposals: ProposeChangeRequest[] = [];
    renderAt("/apps/svc_web/settings", api(anEditedApp(), proposals));
    await ready();
    await user.click(screen.getByRole("button", { name: "Edit General" }));
    const form = screen.getByRole("form", { name: "Edit General" });
    expect(within(form).getByRole("button", { name: /Review plan/ })).toBeDisabled();
    expect(within(form).getByText("Nothing has changed yet.")).toBeVisible();
    const command = within(form).getByRole("textbox", { name: "Command" });
    await user.clear(command);
    await user.type(command, `serve --name "my app`);
    await user.click(within(form).getByRole("button", { name: /Review plan/ }));
    expect(within(form).getByText("A quote isn't closed. Close it, or leave it out.")).toBeVisible();
    await user.type(command, `"`);
    await user.selectOptions(
      within(form).getByRole("combobox", { name: "Restarts" }),
      String(RestartPolicy.ON_FAILURE),
    );
    await user.click(within(form).getByRole("button", { name: /Review plan/ }));
    await waitFor(() => expect(proposals).toHaveLength(1));
    expect(proposals[0]?.summary).toBe("Change how web runs");
    const edit = proposals[0]?.edits[0];
    expect(edit?.baseRevision).toBe(4n);
    expect(edit?.keepEnv).toEqual(["API_KEY", "OWN"]);
    const app = appOf(proposals[0]);
    expect(app?.command).toEqual(["serve", "--name", "my app"]);
    expect(app?.env).toEqual({});
    expect(app?.restart).toBe(RestartPolicy.ON_FAILURE);
    // The rest stays as it was read.
    expect(app?.ports.map((p) => p.hostPort)).toEqual([8080]);
  });

  it("sets an HTTP health check, refusing a duration Docker can't read", async () => {
    const user = userEvent.setup();
    const proposals: ProposeChangeRequest[] = [];
    renderAt("/apps/svc_web/settings", api(anEditedApp(), proposals));
    await ready();
    await user.click(screen.getByRole("button", { name: "Edit Health check" }));
    const form = screen.getByRole("form", { name: "Edit Health check" });
    await user.click(within(form).getByRole("radio", { name: "HTTP" }));
    // The port it listens on is offered, and the paths an app's health is often at.
    const port = within(form).getByRole("combobox", { name: "Port in the app" });
    await user.clear(port);
    await user.type(port, "80");
    await user.type(within(form).getByRole("combobox", { name: "Path" }), "/healthz");
    // Its timing is one step away, said in a line while closed.
    const timing = within(form).getByRole("button", { name: "Timing" });
    expect(timing).toHaveAccessibleDescription("Every 10s, 5s to answer, unhealthy after 3 failures");
    await user.click(timing);
    await user.type(within(form).getByRole("textbox", { name: "Every" }), "10");
    await user.click(within(form).getByRole("button", { name: /Review plan/ }));
    expect(within(form).getByText("Use a duration, such as 10s or 1m.")).toBeVisible();
    await user.type(within(form).getByRole("textbox", { name: "Every" }), "s");
    await user.click(within(form).getByRole("button", { name: /Review plan/ }));
    await waitFor(() => expect(proposals).toHaveLength(1));
    const check = appOf(proposals[0])?.healthcheck;
    expect(check?.http).toEqual(expect.objectContaining({ port: 80, path: "/healthz" }));
    expect(check?.interval).toBe("10s");
    expect(check?.test).toEqual([]);
  });

  it("changes a limit, and says a limit never asks for the passkey on an app that's as good as root", async () => {
    const user = userEvent.setup();
    const service = anEditedApp();
    service.hostAccess = [create(HostAccessSchema, { kind: HostAccessKind.PRIVILEGED, sameAsRoot: true })];
    const proposals: ProposeChangeRequest[] = [];
    renderAt("/apps/svc_web/settings", api(service, proposals));
    await ready();
    await user.click(screen.getByRole("button", { name: "Edit General" }));
    await user.type(
      within(screen.getByRole("form", { name: "Edit General" })).getByRole("textbox", { name: "User" }),
      "1000",
    );
    expect(
      screen.getByText("web is as good as root on its server, so applying this asks for your passkey."),
    ).toBeVisible();
    await user.click(
      within(screen.getByRole("form", { name: "Edit General" })).getByRole("button", { name: "Cancel" }),
    );

    await user.click(screen.getByRole("button", { name: "Edit Resources" }));
    const form = screen.getByRole("form", { name: "Edit Resources" });
    await user.selectOptions(within(form).getByRole("combobox", { name: "Memory limit" }), "256M");
    expect(within(form).getByText("A limit never asks for your passkey, whatever web reaches.")).toBeVisible();
    await user.click(within(form).getByRole("button", { name: /Review plan/ }));
    await waitFor(() => expect(proposals).toHaveLength(1));
    expect(proposals[0]?.summary).toBe("Change the limits of web");
    expect(appOf(proposals[0])?.resources).toEqual(expect.objectContaining({ memory: "256M", cpus: "" }));
  });

  it("adds a port and takes one away", async () => {
    const user = userEvent.setup();
    const proposals: ProposeChangeRequest[] = [];
    renderAt("/apps/svc_web/settings", api(anEditedApp(), proposals));
    await ready();
    await user.click(screen.getByRole("button", { name: "Edit Ports" }));
    const form = screen.getByRole("form", { name: "Edit Ports" });
    await user.click(within(form).getByRole("button", { name: "Add a port" }));
    // A port added takes a free one on the server: the app's own first port holds 8080.
    const second = within(form).getByRole("textbox", { name: "Port 2 on the server" });
    await waitFor(() => expect(second).toHaveValue("8081"));
    expect(second).toHaveAccessibleDescription(/Free/);
    await user.clear(second);
    await user.type(second, "8080");
    // The other row holds it, which is said as it's typed.
    expect(second).toHaveAccessibleDescription(/Taken/);
    await user.type(within(form).getByRole("textbox", { name: "Port 2 in the app" }), "81");
    await user.click(within(form).getByRole("button", { name: /Review plan/ }));
    expect(within(form).getByText("It publishes this port already.")).toBeVisible();
    await user.click(within(form).getByRole("button", { name: "Remove port 1" }));
    await user.click(within(form).getByRole("button", { name: /Review plan/ }));
    await waitFor(() => expect(proposals).toHaveLength(1));
    expect(appOf(proposals[0])?.ports.map((p) => `${p.hostPort}:${p.containerPort}`)).toEqual(["8080:81"]);
  });

  it("mounts a server path, saying what it reaches, and makes a volume it names that isn't there", async () => {
    const user = userEvent.setup();
    const proposals: ProposeChangeRequest[] = [];
    renderAt("/apps/svc_web/settings", api(anEditedApp(), proposals));
    await ready();
    await user.click(screen.getByRole("button", { name: "Edit Volumes and mounts" }));
    const form = screen.getByRole("form", { name: "Edit Volumes and mounts" });
    await user.click(within(form).getByRole("button", { name: "Add a volume or mount" }));
    await user.type(within(form).getByLabelText("Mount 2: volume or server path"), "/var/run/docker.sock");
    await user.type(within(form).getByLabelText("Mount 2: path in the app"), "/var/run/docker.sock");
    expect(within(form).getByText(/gives this app control of Docker on the server, the same as root/)).toBeVisible();
    await user.click(within(form).getByRole("button", { name: "Add a volume or mount" }));
    await user.type(within(form).getByLabelText("Mount 3: volume or server path"), "cache");
    await user.type(within(form).getByLabelText("Mount 3: path in the app"), "cache");
    await user.click(within(form).getByRole("button", { name: /Review plan/ }));
    expect(within(form).getByText("Use an absolute path in the app, such as /data.")).toBeVisible();
    await user.clear(within(form).getByLabelText("Mount 3: path in the app"));
    await user.type(within(form).getByLabelText("Mount 3: path in the app"), "/cache");
    await user.click(within(form).getByRole("button", { name: /Review plan/ }));
    await waitFor(() => expect(proposals).toHaveLength(1));
    // data is there already; cache comes with it, on the server the app runs on.
    expect(proposals[0]?.edits.map((e) => `${e.resource?.kind} ${e.resource?.name}`)).toEqual([
      "volume cache",
      "service web",
    ]);
    expect(proposals[0]?.edits[0]?.servers).toEqual(["srv_1"]);
    expect(appOf(proposals[0])?.mounts.map((m) => `${m.source.value}:${m.target}`)).toEqual([
      "data:/data",
      "/var/run/docker.sock:/var/run/docker.sock",
      "cache:/cache",
    ]);
  });

  it("chooses how it's checked from a list on a phone, and has no section nav there", async () => {
    onPhone();
    const user = userEvent.setup();
    const { container } = renderAt("/apps/svc_web/settings", api(anEditedApp()));
    await ready();
    expect(screen.queryByRole("navigation", { name: "Settings sections" })).toBeNull();
    await user.click(screen.getByRole("button", { name: "Edit Health check" }));
    expect(screen.getByRole("combobox", { name: "How it's checked" })).toBeVisible();
    await expectAccessible(container);
  });
});

describe("an app's settings, where it could go wrong", () => {
  it("plans from the app as it was at Edit, so a change made meanwhile makes it stale, and starts again from that", async () => {
    const user = userEvent.setup();
    let app = anEditedApp();
    const proposals: ProposeChangeRequest[] = [];
    const events = feed<WatchEventsResponse>();
    events.push(snapshot([]));
    renderAt("/apps/svc_web/settings", {
      ...api(() => app, proposals),
      events: { watchEvents: events.stream, listEvents: () => ({}) },
    });
    await ready();
    await user.click(screen.getByRole("button", { name: "Edit Ports" }));
    const form = screen.getByRole("form", { name: "Edit Ports" });
    await user.click(within(form).getByRole("button", { name: "Add a port" }));
    await user.clear(within(form).getByRole("textbox", { name: "Port 2 on the server" }));
    await user.type(within(form).getByRole("textbox", { name: "Port 2 on the server" }), "9090");
    await user.type(within(form).getByRole("textbox", { name: "Port 2 in the app" }), "90");

    // Someone else adds a port meanwhile.
    app = anEditedApp();
    app.revision = 5n;
    if (app.spec?.runs.case === "app")
      app.spec.runs.value.ports.push(create(PortMappingSchema, { hostPort: 7070, containerPort: 70 }));
    events.push(since([changeEvent(moved(webPlanned(), { state: ChangeState.APPLIED }))]));
    await screen.findByText("r5", undefined, { timeout: 5000 });

    await user.click(within(form).getByRole("button", { name: /Review plan/ }));
    await waitFor(() => expect(proposals).toHaveLength(1));
    // From what it read at Edit: the server finds r5 there, and refuses it as stale.
    expect(proposals[0]?.edits[0]?.baseRevision).toBe(4n);
    const sheet = await screen.findByRole("dialog", { name: "Review plan" });
    await user.click(within(sheet).getByRole("button", { name: "Cancel" }));
    expect(
      await screen.findByText(
        "web changed after you started, so the form shows it as it is now. Make your change again.",
      ),
    ).toBeVisible();
    expect(within(screen.getByRole("form", { name: "Edit Ports" })).getByDisplayValue("7070")).toBeVisible();
  });

  it("keeps what you typed when its own apply fails, with no word of anyone else, and plans again from the rollback", async () => {
    const user = userEvent.setup();
    let app = anEditedApp();
    const proposals: ProposeChangeRequest[] = [];
    const events = feed<WatchEventsResponse>();
    events.push(snapshot([]));
    const watch = feed<WatchChangeResponse>();
    const base = api(() => app, proposals);
    renderAt("/apps/svc_web/settings", {
      ...base,
      changes: {
        ...base.changes,
        watchChange: watch.stream,
        applyChange: () => ({ change: moved(webPlanned(), { state: ChangeState.APPLYING }) }),
      },
      events: { watchEvents: events.stream, listEvents: () => ({}) },
    });
    await ready();
    await user.click(screen.getByRole("button", { name: "Edit Ports" }));
    const form = screen.getByRole("form", { name: "Edit Ports" });
    await user.click(within(form).getByRole("button", { name: "Add a port" }));
    await user.clear(within(form).getByRole("textbox", { name: "Port 2 on the server" }));
    await user.type(within(form).getByRole("textbox", { name: "Port 2 on the server" }), "9090");
    await user.type(within(form).getByRole("textbox", { name: "Port 2 in the app" }), "90");
    await user.click(within(form).getByRole("button", { name: /Review plan/ }));
    await user.click(await screen.findByRole("button", { name: /Apply plan/ }));

    // The apply makes r5 and fails there.
    app = anEditedApp();
    app.revision = 5n;
    const failed = moved(webPlanned(), {
      state: ChangeState.FAILED,
      error: "port 9090 is taken",
      rolledBackBy: "chg_back",
    });
    events.push(since([changeEvent(failed)]));
    watch.push(watched(failed));
    const sheet = await screen.findByRole("dialog", { name: "Couldn't replace web" }, { timeout: 5000 });
    expect(sheet).toHaveTextContent("port 9090 is taken");
    await user.click(within(sheet).getByRole("button", { name: /Done/ }));
    await waitFor(() => expect(screen.queryByRole("dialog")).toBeNull());

    // Dockwell's rollback lands after, as r6: the app as it was.
    app = anEditedApp();
    app.revision = 6n;
    events.push(
      since([changeEvent(moved(webPlanned(), { id: "chg_back", state: ChangeState.APPLIED, rollback: true }))]),
    );
    await screen.findByText("r6", undefined, { timeout: 5000 });

    const kept = screen.getByRole("form", { name: "Edit Ports" });
    expect(within(kept).getByRole("textbox", { name: "Port 2 on the server" })).toHaveValue("9090");
    expect(within(kept).getByRole("textbox", { name: "Port 2 in the app" })).toHaveValue("90");
    expect(screen.queryByText(/changed after you started/)).toBeNull();

    // The retry plans from the rollback, so it isn't refused as stale.
    await user.click(within(kept).getByRole("button", { name: /Review plan/ }));
    await waitFor(() => expect(proposals).toHaveLength(2));
    expect(proposals[1]?.edits[0]?.baseRevision).toBe(6n);
    expect(appOf(proposals[1])?.ports.map((p) => p.hostPort)).toEqual([8080, 9090]);
  });

  it("says in its own words that a plan came back stale", async () => {
    const user = userEvent.setup();
    const stale = api(anEditedApp());
    stale.changes = {
      ...stale.changes,
      proposeChange: () => {
        throw new ConnectError("web changed", Code.FailedPrecondition, { "Dockwell-Error": "plan_stale" });
      },
    };
    renderAt("/apps/svc_web/settings", stale);
    await ready();
    await user.click(screen.getByRole("button", { name: "Edit Resources" }));
    await user.selectOptions(screen.getByRole("combobox", { name: "Memory limit" }), "256M");
    await user.click(screen.getByRole("button", { name: /Review plan/ }));
    expect(
      await screen.findByText(
        "web changed after you started, so this would undo that change. Close this, and the form shows it as it is now.",
      ),
    ).toBeVisible();
  });

  it("goes into the form at its first field, and back to Edit on Cancel", async () => {
    const user = userEvent.setup();
    renderAt("/apps/svc_web/settings", api(anEditedApp()));
    await ready();
    await user.click(screen.getByRole("button", { name: "Edit General" }));
    expect(screen.getByRole("textbox", { name: "Command" })).toHaveFocus();
    await user.click(screen.getByRole("button", { name: "Cancel" }));
    await waitFor(() => expect(screen.getByRole("button", { name: "Edit General" })).toHaveFocus());
  });

  it("keeps an exec-form check as Docker runs it while its words don't change", async () => {
    const user = userEvent.setup();
    const service = anEditedApp();
    if (service.spec?.runs.case === "app")
      service.spec.runs.value.healthcheck = create(HealthcheckSchema, { test: ["CMD", "pg_isready", "-U", "app"] });
    const proposals: ProposeChangeRequest[] = [];
    renderAt("/apps/svc_web/settings", api(service, proposals));
    await ready();
    await user.click(screen.getByRole("button", { name: "Edit Health check" }));
    await user.click(screen.getByRole("button", { name: "Timing" }));
    await user.type(screen.getByRole("textbox", { name: "Every" }), "5s");
    await user.click(screen.getByRole("button", { name: /Review plan/ }));
    await waitFor(() => expect(proposals).toHaveLength(1));
    expect(appOf(proposals[0])?.healthcheck?.test).toEqual(["CMD", "pg_isready", "-U", "app"]);
    expect(appOf(proposals[0])?.healthcheck?.interval).toBe("5s");
  });

  it("makes no volume for a mount of an external one, as Docker's own", async () => {
    const user = userEvent.setup();
    const service = anEditedApp();
    if (service.spec?.runs.case === "app")
      service.spec.runs.value.mounts.push(
        create(MountSchema, { source: { case: "volume", value: "1panel_data" }, target: "/opt", external: true }),
      );
    const proposals: ProposeChangeRequest[] = [];
    renderAt("/apps/svc_web/settings", api(service, proposals));
    await ready();
    await user.click(screen.getByRole("button", { name: "Edit Volumes and mounts" }));
    const form = screen.getByRole("form", { name: "Edit Volumes and mounts" });
    await user.click(within(form).getAllByRole("checkbox")[0] as HTMLElement);
    await user.click(within(form).getByRole("button", { name: /Review plan/ }));
    await waitFor(() => expect(proposals).toHaveLength(1));
    expect(proposals[0]?.edits.map((e) => e.resource?.kind)).toEqual(["service"]);
    expect(appOf(proposals[0])?.mounts.find((m) => m.target === "/opt")?.external).toBe(true);
  });

  it("says a mount that's as good as root asks for the passkey, even on an app that isn't yet", async () => {
    const user = userEvent.setup();
    renderAt("/apps/svc_web/settings", api(anEditedApp()));
    await ready();
    await user.click(screen.getByRole("button", { name: "Edit Volumes and mounts" }));
    const form = screen.getByRole("form", { name: "Edit Volumes and mounts" });
    await user.click(within(form).getByRole("button", { name: "Add a volume or mount" }));
    await user.type(within(form).getByLabelText("Mount 2: volume or server path"), "/etc/nginx");
    expect(within(form).getByText(/is as good as root there, so applying this asks for your passkey/)).toBeVisible();
    expect(
      within(form).getByText("web is as good as root on its server, so applying this asks for your passkey."),
    ).toBeVisible();
    await user.clear(within(form).getByLabelText("Mount 2: volume or server path"));
    await user.type(within(form).getByLabelText("Mount 2: volume or server path"), "/srv/web");
    expect(within(form).getByText(/You approve this in the plan; API tokens can't./)).toBeVisible();
    expect(within(form).queryByText(/asks for your passkey/)).toBeNull();
  });

  it("keeps no more memory for it than its limit, and reads amounts as the server does", async () => {
    const user = userEvent.setup();
    const proposals: ProposeChangeRequest[] = [];
    renderAt("/apps/svc_web/settings", api(anEditedApp(), proposals));
    await ready();
    await user.click(screen.getByRole("button", { name: "Edit Resources" }));
    // An amount that isn't one of the picks is typed, under Other.
    await user.selectOptions(screen.getByRole("combobox", { name: "Memory limit" }), "other");
    await user.type(screen.getByRole("textbox", { name: "Memory limit, typed" }), "1.5G");
    await user.click(screen.getByRole("button", { name: "Memory kept for it" }));
    await user.type(screen.getByRole("textbox", { name: "Memory kept for it" }), "2Gi");
    await user.click(screen.getByRole("button", { name: /Review plan/ }));
    expect(screen.getByText("Keep no more for it than its limit, 1.5G.")).toBeVisible();
    await user.clear(screen.getByRole("textbox", { name: "Memory kept for it" }));
    await user.type(screen.getByRole("textbox", { name: "Memory kept for it" }), "1Gi");
    await user.click(screen.getByRole("button", { name: /Review plan/ }));
    await waitFor(() => expect(proposals).toHaveLength(1));
    expect(appOf(proposals[0])?.resources).toEqual(
      expect.objectContaining({ memory: "1.5G", memoryReservation: "1Gi" }),
    );
  });

  it("marks the section in view in its nav", async () => {
    const seen: ((entries: { target: Element; isIntersecting: boolean }[]) => void)[] = [];
    vi.stubGlobal(
      "IntersectionObserver",
      class {
        constructor(callback: (entries: { target: Element; isIntersecting: boolean }[]) => void) {
          seen.push(callback);
        }
        observe() {}
        disconnect() {}
      },
    );
    renderAt("/apps/svc_web/settings", api(anEditedApp()));
    await ready();
    const nav = screen.getByRole("navigation", { name: "Settings sections" });
    await waitFor(() => expect(seen.length).toBeGreaterThan(0));
    act(() => seen.at(-1)?.([{ target: document.getElementById("ports") as Element, isIntersecting: true }]));
    expect(within(nav).getByRole("link", { name: "Ports" })).toHaveAttribute("aria-current", "true");
    expect(within(nav).getByRole("link", { name: "Ports" })).toHaveAttribute("href", "#ports");
    vi.unstubAllGlobals();
  });
});
