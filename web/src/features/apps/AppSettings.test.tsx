// SPDX-License-Identifier: AGPL-3.0-only

import { create } from "@bufbuild/protobuf";
import {
  type Container,
  HostAccessKind,
  HostAccessSchema,
  type ProposeChangeRequest,
  RestartPolicy,
  type ServiceInfo,
  type WatchChangeResponse,
  type WatchServersResponse,
} from "@dockwell/api";
import { screen, waitFor, within } from "@testing-library/react";
import userEvent from "@testing-library/user-event";
import { describe, expect, it } from "vitest";

import { expectAccessible } from "../../test/a11y";
import { anApp, aRevision, places } from "../../test/apps";
import { webPlanned } from "../../test/changes";
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

function api(service: ServiceInfo, proposals: ProposeChangeRequest[] = []): Api {
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
      listServices: () => ({ services: [service] }),
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
  it("shows how it runs, section by section, each with its own Edit", async () => {
    const { container } = renderAt("/apps/svc_web/settings", api(anEditedApp()));
    await ready();
    expect(within(section("General")).getByText("serve --port 80")).toBeVisible();
    expect(within(section("Ports")).getByText("8080")).toBeVisible();
    expect(within(section("Volumes and mounts")).getByText("/data")).toBeVisible();
    expect(within(section("Resources")).getAllByText("No limit")).toHaveLength(2);
    expect(within(section("Danger zone")).getByRole("button", { name: "Delete this app" })).toBeVisible();
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
    await user.type(within(form).getByRole("textbox", { name: "Port in the app" }), "80");
    await user.type(within(form).getByRole("textbox", { name: "Path" }), "/healthz");
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
    await user.type(within(form).getByRole("textbox", { name: "Memory limit" }), "256M");
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
    await user.type(within(form).getByRole("textbox", { name: "Port 2 on the server" }), "8080");
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
    await user.type(
      within(form).getByRole("textbox", { name: "Mount 2: volume or server path" }),
      "/var/run/docker.sock",
    );
    await user.type(within(form).getByRole("textbox", { name: "Mount 2: path in the app" }), "/var/run/docker.sock");
    expect(within(form).getByText(/gives this app control of Docker on the server, the same as root/)).toBeVisible();
    await user.click(within(form).getByRole("button", { name: "Add a volume or mount" }));
    await user.type(within(form).getByRole("textbox", { name: "Mount 3: volume or server path" }), "cache");
    await user.type(within(form).getByRole("textbox", { name: "Mount 3: path in the app" }), "cache");
    await user.click(within(form).getByRole("button", { name: /Review plan/ }));
    expect(within(form).getByText("Use an absolute path in the app, such as /data.")).toBeVisible();
    await user.clear(within(form).getByRole("textbox", { name: "Mount 3: path in the app" }));
    await user.type(within(form).getByRole("textbox", { name: "Mount 3: path in the app" }), "/cache");
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
