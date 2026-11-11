// SPDX-License-Identifier: AGPL-3.0-only

import { create, type MessageInitShape } from "@bufbuild/protobuf";
import { Code, ConnectError } from "@connectrpc/connect";
import {
  EnvironmentVariableSchema,
  HostAccessKind,
  HostAccessSchema,
  type ProposeChangeRequest,
  type ServiceInfo,
  type WatchChangeResponse,
  type WatchServersResponse,
} from "@dockwell/api";
import { screen, waitFor, within } from "@testing-library/react";
import userEvent from "@testing-library/user-event";
import { afterEach, describe, expect, it } from "vitest";

import { expectAccessible } from "../../test/a11y";
import { anApp, aRevision, places } from "../../test/apps";
import { webPlanned } from "../../test/changes";
import { type Api, feed, onPhone, renderAt } from "../../test/render";
import { aServer, changed, snapshotComplete } from "../../test/servers";
import { forgetDrafts } from "./AppEnvironment";
import { envEdits } from "./envEdit";

const store = (...list: MessageInitShape<typeof EnvironmentVariableSchema>[]) =>
  list.map((v) => create(EnvironmentVariableSchema, v));

/** An app that uses LOG_LEVEL, the secret API_KEY, and has OWN set on itself. */
function anEditedApp(): ServiceInfo {
  const service = anApp("web", "nginx:1.5", { env: { OWN: "", OTHER: "" } });
  if (service.spec) {
    service.spec.variables = { LOG_LEVEL: "" };
    service.spec.secrets = { API_KEY: "" };
  }
  service.revision = 7n;
  return service;
}

const variables = store(
  {
    id: "var_1",
    name: "LOG_LEVEL",
    value: "info",
    usedBy: [{ serviceId: "svc_web", service: "web", name: "LOG_LEVEL" }],
  },
  { id: "sec_1", name: "API_KEY", secret: true, usedBy: [{ serviceId: "svc_web", service: "web", name: "API_KEY" }] },
  { id: "var_2", name: "REGION", value: "eu", usedBy: [{ serviceId: "svc_api", service: "api", name: "REGION" }] },
);

function api(service: ServiceInfo, proposals: ProposeChangeRequest[] = [], ...more: ServiceInfo[]): Api {
  const watch = feed<WatchServersResponse>();
  watch.push(changed(aServer({ id: "srv_1", name: "hz-1" })), snapshotComplete);
  return {
    resources: places(),
    services: {
      listServices: () => ({ services: [service, ...more] }),
      listServiceRevisions: () => ({ revisions: [aRevision(7n, "nginx:1.5")] }),
    },
    servers: { watchServers: watch.stream },
    variables: { listVariables: () => ({ environmentId: "env_prod", variables }) },
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

const form = () => screen.getByRole("form", { name: "Add a variable" });
const add = async (user: ReturnType<typeof userEvent.setup>, key: string, value: string, secret = false) => {
  await user.type(within(form()).getByRole("textbox", { name: "Key" }), key);
  await user.type(within(form()).getByRole("textbox", { name: "Value" }), value);
  if (secret) await user.click(within(form()).getByRole("switch", { name: "Secret" }));
  await user.click(within(form()).getByRole("button", { name: "Add" }));
};
const bar = () => screen.getByRole("region", { name: "Unapplied changes" });

afterEach(() => forgetDrafts());
/** The page with what the app uses read, which a busy machine takes longer to draw. */
const ready = () => screen.findByText("info", undefined, { timeout: 5000 });

describe("editing an app's environment", () => {
  it("waits with each edit in the list, then plans them together, keeping what it can't read", async () => {
    const user = userEvent.setup();
    const proposals: ProposeChangeRequest[] = [];
    const { container } = renderAt("/apps/svc_web/environment", api(anEditedApp(), proposals));
    expect(await ready()).toBeVisible();
    expect(screen.queryByRole("region", { name: "Unapplied changes" })).toBeNull();

    await add(user, "FEATURE", "beta");
    await add(user, "TOKEN", "t0k", true);
    await user.click(screen.getByRole("button", { name: "Remove OTHER" }));
    expect(screen.getByRole("cell", { name: "FEATURE, added, not applied" })).toBeVisible();
    expect(screen.getByRole("cell", { name: "OTHER, removed, not applied" })).toBeVisible();
    expect(bar()).toHaveTextContent("3 unapplied changes");
    expect(bar()).toHaveTextContent("2 added");
    expect(bar()).toHaveTextContent("1 removed");
    expect(bar()).toHaveTextContent("Applying it restarts web.");
    // Nothing is planned until you ask.
    expect(proposals).toHaveLength(0);
    await expectAccessible(container);

    await user.click(within(bar()).getByRole("button", { name: /Review plan/ }));
    await waitFor(() => expect(proposals).toHaveLength(1));
    const [request] = proposals;
    expect(request?.summary).toBe("Change the environment of web");
    const edits = request?.edits ?? [];
    expect(edits.map((e) => `${e.resource?.kind} ${e.resource?.name}`)).toEqual([
      "variable FEATURE",
      "secret TOKEN",
      "service web",
    ]);
    const app = edits[2];
    const spec = app?.spec?.kind.case === "service" ? app.spec.kind.value : undefined;
    expect(spec?.variables).toEqual({ LOG_LEVEL: "", FEATURE: "" });
    expect(spec?.secrets).toEqual({ API_KEY: "", TOKEN: "" });
    // Its own values never came back, so it keeps OWN without reading it, and OTHER goes.
    expect(spec?.runs.case === "app" ? spec.runs.value.env : undefined).toEqual({});
    expect(app?.keepEnv).toEqual(["OWN"]);
    expect(app?.baseRevision).toBe(7n);
  });

  it("says an app has no variables as an empty state, and draws the table once one is added", async () => {
    const user = userEvent.setup();
    renderAt("/apps/svc_web/environment", api(anApp("web", "nginx:1.5"), []));
    expect(await screen.findByRole("heading", { level: 2, name: "No variables yet" })).toBeVisible();
    expect(screen.getByText("It runs with only what its image sets.")).toBeVisible();
    // No column heads over a sentence.
    expect(screen.queryByRole("table")).toBeNull();
    // Adding waits for what the environment has, so a new name can't clash with one there.
    await waitFor(() => expect(within(form()).getByRole("button", { name: "Add" })).toBeEnabled());
    await add(user, "FEATURE", "beta");
    expect(await screen.findByRole("table", { name: "Environment variables of web" })).toBeVisible();
    expect(screen.queryByRole("heading", { level: 2, name: "No variables yet" })).toBeNull();
  });

  it("uses a variable the environment has as it is, rather than change it for every app", async () => {
    const user = userEvent.setup();
    const proposals: ProposeChangeRequest[] = [];
    renderAt("/apps/svc_web/environment", api(anEditedApp(), proposals));
    await ready();
    await add(user, "REGION", "us");
    expect(within(form()).getByRole("alert")).toHaveTextContent(
      "The environment has a variable REGION already. Use it as it is, or choose another name.",
    );
    await user.click(within(form()).getByRole("button", { name: "Use REGION" }));
    expect(screen.getByRole("cell", { name: "REGION, added, not applied" })).toBeVisible();
    expect(screen.getByText("eu")).toBeVisible();
    await user.click(within(bar()).getByRole("button", { name: /Review plan/ }));
    await waitFor(() => expect(proposals).toHaveLength(1));
    expect(proposals[0]?.summary).toBe("Add REGION to web");
    // Only the app changes: REGION stays as api has it.
    expect(proposals[0]?.edits.map((e) => e.resource?.kind)).toEqual(["service"]);
  });

  it("refuses a name the app has, or one no process takes", async () => {
    const user = userEvent.setup();
    renderAt("/apps/svc_web/environment", api(anEditedApp()));
    await ready();
    await add(user, "LOG_LEVEL", "debug");
    expect(within(form()).getByRole("alert")).toHaveTextContent(
      "web has LOG_LEVEL already. Change it in its row below.",
    );
    await user.clear(within(form()).getByRole("textbox", { name: "Key" }));
    await add(user, "1ST", "x");
    expect(within(form()).getByRole("alert")).toHaveTextContent("Use letters, digits and underscores");
    expect(screen.queryByRole("region", { name: "Unapplied changes" })).toBeNull();
  });

  it("changes a value without reading it back, and says which other apps it restarts", async () => {
    const user = userEvent.setup();
    const service = anEditedApp();
    const shared = [
      ...store({
        id: "var_1",
        name: "LOG_LEVEL",
        value: "info",
        usedBy: [
          { serviceId: "svc_web", service: "web", name: "LOG_LEVEL" },
          { serviceId: "svc_api", service: "api", name: "LOG_LEVEL" },
        ],
      }),
      ...variables.slice(1),
    ];
    const proposals: ProposeChangeRequest[] = [];
    const more = api(service, proposals);
    more.variables = { listVariables: () => ({ environmentId: "env_prod", variables: shared }) };
    renderAt("/apps/svc_web/environment", more);
    await ready();

    await user.click(screen.getByRole("button", { name: "Change LOG_LEVEL" }));
    const changing = screen.getByRole("form", { name: "Change LOG_LEVEL" });
    expect(within(changing).getByRole("textbox", { name: "New value of LOG_LEVEL" })).toHaveFocus();
    await user.keyboard("debug{Enter}");
    await user.click(screen.getByRole("button", { name: "Change OWN" }));
    expect(
      screen.getByText("What's set on this app isn't shown, so type the whole value it should have."),
    ).toBeVisible();
    await user.keyboard("s3cret{Enter}");
    expect(bar()).toHaveTextContent("Applying it restarts web, and api, which use what changes too.");

    // ⌘↵ or Ctrl ↵ reviews from anywhere on the page.
    await user.keyboard("{Control>}{Enter}{/Control}");
    await waitFor(() => expect(proposals).toHaveLength(1));
    const edits = proposals[0]?.edits ?? [];
    expect(edits.map((e) => `${e.resource?.kind} ${e.resource?.name}`)).toEqual(["variable LOG_LEVEL", "service web"]);
    const app = edits[1]?.spec?.kind.case === "service" ? edits[1].spec.kind.value : undefined;
    expect(app?.runs.case === "app" ? app.runs.value.env : undefined).toEqual({ OWN: "s3cret" });
    expect(edits[1]?.keepEnv).toEqual(["OTHER"]);
  });

  it("puts an edit back, or all of them", async () => {
    const user = userEvent.setup();
    renderAt("/apps/svc_web/environment", api(anEditedApp()));
    await ready();
    await add(user, "A", "1");
    await add(user, "B", "2");
    await user.click(screen.getByRole("button", { name: "Revert A" }));
    expect(screen.queryByRole("cell", { name: /^A,/ })).toBeNull();
    expect(bar()).toHaveTextContent("1 unapplied change");
    await user.click(within(bar()).getByRole("button", { name: "Discard" }));
    expect(screen.queryByRole("region", { name: "Unapplied changes" })).toBeNull();
    expect(screen.queryByRole("cell", { name: /^B,/ })).toBeNull();
  });

  it("says before you plan that an app as good as root asks for your passkey", async () => {
    const user = userEvent.setup();
    const service = anEditedApp();
    service.hostAccess = [create(HostAccessSchema, { kind: HostAccessKind.PRIVILEGED, sameAsRoot: true })];
    renderAt("/apps/svc_web/environment", api(service));
    await ready();
    await add(user, "A", "1");
    expect(bar()).toHaveTextContent("web is as good as root on its server, so applying this asks for your passkey.");
  });

  it("reads an .env file into edits: a name it has changes, the rest are added", async () => {
    const user = userEvent.setup();
    renderAt("/apps/svc_web/environment", api(anEditedApp()));
    await ready();
    await user.click(screen.getByRole("button", { name: "Import .env" }));
    const sheet = await screen.findByRole("dialog", { name: "Import .env" });
    await user.type(within(sheet).getByRole("textbox", { name: "Paste the file's text" }), "not a file");
    await user.click(within(sheet).getByRole("button", { name: "Import 0 values" }).closest("button") as HTMLElement);
    await user.clear(within(sheet).getByRole("textbox", { name: "Paste the file's text" }));
    await user.type(
      within(sheet).getByRole("textbox", { name: "Paste the file's text" }),
      "# comment{Enter}NEW=1{Enter}OWN=2",
    );
    await user.click(within(sheet).getByRole("button", { name: "Import 2 values" }));
    expect(screen.getByRole("cell", { name: "NEW, added, not applied" })).toBeVisible();
    expect(screen.getByRole("cell", { name: "OWN, changed, not applied" })).toBeVisible();
  });

  it("lays its rows out for a phone, and keeps the bar's buttons in reach", async () => {
    onPhone();
    const user = userEvent.setup();
    const { container } = renderAt("/apps/svc_web/environment", api(anEditedApp()));
    await ready();
    await add(user, "A", "1");
    expect(within(bar()).getByRole("button", { name: /Review plan/ })).toBeVisible();
    expect(screen.getAllByRole("columnheader").map((h) => h.textContent)).toEqual(["Key", "Actions"]);
    await expectAccessible(container);
  });
});

describe("editing an app's environment, where it could go wrong", () => {
  it("plans once: Ctrl+Enter in the open plan sheet is the sheet's, not a second plan", async () => {
    const user = userEvent.setup();
    const proposals: ProposeChangeRequest[] = [];
    renderAt("/apps/svc_web/environment", api(anEditedApp(), proposals));
    await ready();
    await add(user, "A", "1");
    await user.keyboard("{Control>}{Enter}{/Control}");
    await waitFor(() => expect(proposals).toHaveLength(1));
    const sheet = await screen.findByRole("dialog");
    within(sheet).getAllByRole("button")[0]?.focus();
    await user.keyboard("{Control>}{Enter}{/Control}");
    await new Promise((resolve) => setTimeout(resolve, 100));
    expect(proposals).toHaveLength(1);
  });

  it("says in its own words that the app changed meanwhile, when the plan comes back stale", async () => {
    const user = userEvent.setup();
    const stale = api(anEditedApp());
    stale.changes = {
      ...stale.changes,
      proposeChange: () => {
        throw new ConnectError("web changed while this was being planned", Code.FailedPrecondition, {
          "Dockwell-Error": "plan_stale",
        });
      },
    };
    renderAt("/apps/svc_web/environment", stale);
    await ready();
    await add(user, "A", "1");
    await user.click(within(bar()).getByRole("button", { name: /Review plan/ }));
    expect(
      await screen.findByText(
        "web changed after you started. Close this and review your edits again: they apply to what it is now.",
      ),
    ).toBeVisible();
  });

  it("uses an imported name the environment has as it is, and says so", async () => {
    const user = userEvent.setup();
    const proposals: ProposeChangeRequest[] = [];
    renderAt("/apps/svc_web/environment", api(anEditedApp(), proposals));
    await ready();
    await user.click(screen.getByRole("button", { name: "Import .env" }));
    const sheet = await screen.findByRole("dialog", { name: "Import .env" });
    await user.type(within(sheet).getByRole("textbox", { name: "Paste the file's text" }), "REGION=us{Enter}NEW=1");
    expect(within(sheet).getByRole("status")).toHaveTextContent("The environment has REGION already");
    await user.click(within(sheet).getByRole("button", { name: "Import 2 values" }));
    expect(screen.getByRole("cell", { name: "REGION, added, not applied" })).toBeVisible();
    await user.click(within(bar()).getByRole("button", { name: /Review plan/ }));
    await waitFor(() => expect(proposals).toHaveLength(1));
    // REGION stays as api has it: only NEW is made.
    expect(proposals[0]?.edits.map((e) => `${e.resource?.kind} ${e.resource?.name}`)).toEqual([
      "variable NEW",
      "service web",
    ]);
  });

  it("asks for the passkey when a shared value restarts another app that's as good as root", async () => {
    const user = userEvent.setup();
    const portainer = anApp("portainer", "portainer/portainer-ce:2");
    portainer.id = "svc_api";
    portainer.hostAccess = [create(HostAccessSchema, { kind: HostAccessKind.DOCKER_SOCKET, sameAsRoot: true })];
    const shared = store({
      id: "var_1",
      name: "LOG_LEVEL",
      value: "info",
      usedBy: [
        { serviceId: "svc_web", service: "web", name: "LOG_LEVEL" },
        { serviceId: "svc_api", service: "portainer", name: "LOG_LEVEL" },
      ],
    });
    const more = api(anEditedApp(), [], portainer);
    more.variables = {
      listVariables: () => ({ environmentId: "env_prod", variables: [...shared, ...variables.slice(1)] }),
    };
    renderAt("/apps/svc_web/environment", more);
    await ready();
    await user.click(screen.getByRole("button", { name: "Change LOG_LEVEL" }));
    await user.keyboard("debug{Enter}");
    expect(bar()).toHaveTextContent("Applying it restarts web, and portainer, which use what changes too.");
    expect(bar()).toHaveTextContent(
      "portainer is as good as root on its server, so applying this asks for your passkey.",
    );
  });

  it("keeps edits not yet planned while you look at another tab", async () => {
    const user = userEvent.setup();
    const { router } = renderAt("/apps/svc_web/environment", api(anEditedApp()));
    await ready();
    await add(user, "KEPT", "1");
    await router.navigate({ to: "/apps/$serviceId", params: { serviceId: "svc_web" } });
    await screen.findByRole("heading", { level: 1, name: "web" });
    await router.navigate({ to: "/apps/$serviceId/environment", params: { serviceId: "svc_web" } });
    expect(await screen.findByRole("cell", { name: "KEPT, added, not applied" })).toBeVisible();
    await user.click(within(bar()).getByRole("button", { name: "Discard" }));
  });

  it("waits to add until it can read what the environment has, and says why it can't", async () => {
    const user = userEvent.setup();
    const failing = api(anEditedApp());
    let fail = true;
    failing.variables = {
      listVariables: () => {
        if (fail) throw new ConnectError("your role can't read these", Code.PermissionDenied);
        return { environmentId: "env_prod", variables };
      },
    };
    renderAt("/apps/svc_web/environment", failing);
    const callout = await screen.findByText("Can't read the environment's variables and secrets", undefined, {
      timeout: 5000,
    });
    expect(callout.closest(".dw-callout")).toHaveTextContent(
      "Their values stay hidden, and Add waits until they load.",
    );
    expect(within(form()).getByRole("button", { name: "Add" })).toBeDisabled();
    expect(screen.getByRole("button", { name: "Import .env" })).toBeDisabled();
    fail = false;
    await user.click(screen.getByRole("button", { name: "Try again" }));
    await ready();
    expect(within(form()).getByRole("button", { name: "Add" })).toBeEnabled();
  });
});

describe("envEdits", () => {
  it("changes only the environment's value when that's all that changes", () => {
    const edits = envEdits(
      anEditedApp(),
      new Map([["LOG_LEVEL", { op: "change", name: "LOG_LEVEL", value: "debug" }]]),
    );
    expect(edits).toEqual([
      {
        resource: { kind: "variable", scope: "env_prod", name: "LOG_LEVEL" },
        spec: { kind: { case: "variable", value: { value: "debug" } } },
      },
    ]);
  });

  it("uses one of the environment's under another name", () => {
    const edits = envEdits(
      anEditedApp(),
      new Map([["DB_HOST", { op: "use", name: "DB_HOST", from: "HOST", secret: false }]]),
    );
    const spec = edits[0]?.spec?.kind?.case === "service" ? edits[0].spec.kind.value : undefined;
    expect(spec?.variables).toEqual({ LOG_LEVEL: "", DB_HOST: "HOST" });
    expect(edits[0]?.keepEnv).toEqual(["OTHER", "OWN"]);
  });

  it("stops using a secret without deleting it from the environment", () => {
    const edits = envEdits(anEditedApp(), new Map([["API_KEY", { op: "remove", name: "API_KEY" }]]));
    expect(edits).toHaveLength(1);
    const spec = edits[0]?.spec?.kind?.case === "service" ? edits[0].spec.kind.value : undefined;
    expect(spec?.secrets).toEqual({});
  });
});
