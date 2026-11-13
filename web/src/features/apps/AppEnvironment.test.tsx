// SPDX-License-Identifier: AGPL-3.0-only

import { create, type MessageInitShape } from "@bufbuild/protobuf";
import { Code, ConnectError } from "@connectrpc/connect";
import {
  EnvironmentVariableSchema,
  HostAccessKind,
  HostAccessSchema,
  type ProposeChangeRequest,
  type RevealServiceValueRequest,
  type ServiceInfo,
  StepUpAction,
  type WatchChangeResponse,
  type WatchServersResponse,
} from "@dockwell/api";
import { screen, waitFor, within } from "@testing-library/react";
import userEvent from "@testing-library/user-event";
import { afterEach, describe, expect, it, vi } from "vitest";

import { expectAccessible } from "../../test/a11y";
import { anApp, aRevision, places } from "../../test/apps";
import { authError, stubPasskeys } from "../../test/auth";
import { webPlanned } from "../../test/changes";
import { type Api, feed, onPhone, renderAt } from "../../test/render";
import { aServer, changed, snapshotComplete } from "../../test/servers";
import { forgetDrafts } from "./AppEnvironment";
import { envEdits } from "./envEdit";

const store = (...list: MessageInitShape<typeof EnvironmentVariableSchema>[]) =>
  list.map((v) => create(EnvironmentVariableSchema, v));

/** An app that uses LOG_LEVEL, the secret API_KEY, and has OWN and OTHER set on itself, hidden. */
function anEditedApp(): ServiceInfo {
  const service = anApp("web", "nginx:1.5", { env: { OWN: "", OTHER: "" } });
  if (service.spec) {
    service.spec.variables = { LOG_LEVEL: "" };
    service.spec.secrets = { API_KEY: "" };
  }
  service.revision = 7n;
  return service;
}

/** An app installed from a catalog: what its entry set shows, and what may be a secret doesn't (ADR 0081). */
function anInstalledApp(): ServiceInfo {
  const service = anApp("web", "wordpress:7", { env: { DB_HOST: "db", DB_NAME: "wordpress", DB_EXTRA: "" } });
  if (service.spec) {
    service.spec.variables = { LOG_LEVEL: "" };
    service.spec.secrets = { API_KEY: "" };
    service.spec.shownEnv = ["DB_HOST", "DB_NAME"];
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

afterEach(() => {
  forgetDrafts();
  vi.unstubAllGlobals();
});
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
    // A plain value is the app's own, shown as it is; a secret is the environment's, which takes a passkey to read.
    expect(edits.map((e) => `${e.resource?.kind} ${e.resource?.name}`)).toEqual(["secret TOKEN", "service web"]);
    const app = edits[1];
    const spec = app?.spec?.kind.case === "service" ? app.spec.kind.value : undefined;
    expect(spec?.variables).toEqual({ LOG_LEVEL: "" });
    expect(spec?.secrets).toEqual({ API_KEY: "", TOKEN: "" });
    expect(spec?.runs.case === "app" ? spec.runs.value.env : undefined).toEqual({ FEATURE: "beta" });
    expect(spec?.shownEnv).toEqual(["FEATURE"]);
    // Its hidden values never came back, so it keeps OWN without reading it, and OTHER goes.
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
    // A hidden one of its own may be a secret, so it starts as one, which a passkey reveals.
    await user.click(screen.getByRole("button", { name: "Change OWN" }));
    expect(
      within(screen.getByRole("form", { name: "Change OWN" })).getByRole("switch", { name: "Secret" }),
    ).toBeChecked();
    expect(screen.getByText("It becomes a secret of the environment, which takes a passkey to reveal.")).toBeVisible();
    await user.keyboard("s3cret{Enter}");
    // One switched off stays its own, shown.
    await user.click(screen.getByRole("button", { name: "Change OTHER" }));
    await user.click(
      within(screen.getByRole("form", { name: "Change OTHER" })).getByRole("switch", { name: "Secret" }),
    );
    expect(
      screen.getByText("It's hidden, as it may be a secret, so type the whole value it should have."),
    ).toBeVisible();
    await user.type(screen.getByRole("textbox", { name: "New value of OTHER" }), "plain{Enter}");
    expect(bar()).toHaveTextContent("Applying it restarts web, and api, which use what changes too.");

    // ⌘↵ or Ctrl ↵ reviews from anywhere on the page.
    await user.keyboard("{Control>}{Enter}{/Control}");
    await waitFor(() => expect(proposals).toHaveLength(1));
    const edits = proposals[0]?.edits ?? [];
    expect(edits.map((e) => `${e.resource?.kind} ${e.resource?.name}`)).toEqual([
      "variable LOG_LEVEL",
      "secret OWN",
      "service web",
    ]);
    const app = edits[2]?.spec?.kind.case === "service" ? edits[2].spec.kind.value : undefined;
    expect(app?.runs.case === "app" ? app.runs.value.env : undefined).toEqual({ OTHER: "plain" });
    expect(app?.secrets).toEqual({ API_KEY: "", OWN: "" });
    expect(app?.shownEnv).toEqual(["OTHER"]);
    expect(edits[2]?.keepEnv).toEqual([]);
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

  it("keeps a hidden value hidden when a pasted file gives it a new one, Secret on or off", async () => {
    for (const secret of [false, true]) {
      forgetDrafts();
      const user = userEvent.setup();
      const proposals: ProposeChangeRequest[] = [];
      const { unmount } = renderAt("/apps/svc_web/environment", api(anInstalledApp(), proposals));
      await ready();
      await user.click(screen.getByRole("button", { name: "Import .env" }));
      const sheet = await screen.findByRole("dialog", { name: "Import .env" });
      await user.type(
        within(sheet).getByRole("textbox", { name: "Paste the file's text" }),
        "DB_EXTRA=hunter2{Enter}DB_HOST=db2{Enter}NEW=1",
      );
      if (secret) await user.click(within(sheet).getByRole("switch", { name: "Keep the new ones as secrets" }));
      await user.click(within(sheet).getByRole("button", { name: "Import 3 values" }));
      expect(within(screen.getByRole("row", { name: /^DB_EXTRA/ })).getByRole("img", { name: "Hidden" })).toBeVisible();
      await user.click(within(bar()).getByRole("button", { name: /Review plan/ }));
      await waitFor(() => expect(proposals).toHaveLength(1));
      const app = proposals[0]?.edits.find((e) => e.resource?.kind === "service");
      const spec = app?.spec?.kind.case === "service" ? app.spec.kind.value : undefined;
      // It reaches the app with its new value, and isn't shown: nothing said it holds no secret.
      expect(spec?.runs.case === "app" ? spec.runs.value.env["DB_EXTRA"] : undefined).toBe("hunter2");
      expect(spec?.shownEnv).not.toContain("DB_EXTRA");
      // One it showed stays shown with its new value.
      expect(spec?.shownEnv).toContain("DB_HOST");
      unmount();
    }
  });

  it("waits to make a value a secret, or share one, until it knows what the environment has", async () => {
    const user = userEvent.setup();
    const loading = api(anInstalledApp());
    loading.variables = { listVariables: () => new Promise(() => {}) };
    renderAt("/apps/svc_web/environment", loading);
    const share = await screen.findByRole("button", { name: "Make DB_HOST shared" }, { timeout: 5000 });
    expect(share).toHaveAttribute("aria-disabled", "true");
    expect(share).toHaveAccessibleDescription("Waits until the environment's variables and secrets load.");
    await user.click(screen.getByRole("button", { name: "Change DB_HOST" }));
    const changing = screen.getByRole("form", { name: "Change DB_HOST" });
    await user.click(within(changing).getByRole("switch", { name: "Secret" }));
    expect(within(changing).getByRole("button", { name: "Change" })).toBeDisabled();
    // As the app's own, shown, it needs nothing of the environment.
    await user.click(within(changing).getByRole("switch", { name: "Secret" }));
    expect(within(changing).getByRole("button", { name: "Change" })).toBeEnabled();
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

describe("an app's own values", () => {
  it("shows what holds no secret as it is, hides the rest, and changes a shown one from what it is", async () => {
    const user = userEvent.setup();
    const proposals: ProposeChangeRequest[] = [];
    const { container } = renderAt("/apps/svc_web/environment", api(anInstalledApp(), proposals));
    await ready();
    const row = (name: string) => screen.getByRole("row", { name: new RegExp(`^${name}`) });
    expect(within(row("DB_HOST")).getByText("db")).toBeVisible();
    expect(within(row("DB_HOST")).getByText("Set on this app")).toBeVisible();
    expect(within(row("DB_NAME")).getByText("wordpress")).toBeVisible();
    expect(within(row("DB_EXTRA")).getByRole("img", { name: "Hidden" })).toBeVisible();
    // Only one it shows can be made shared: the value of one it hides isn't here to share.
    expect(screen.queryByRole("button", { name: "Make DB_EXTRA shared" })).toBeNull();
    await expectAccessible(container);

    await user.click(screen.getByRole("button", { name: "Change DB_NAME" }));
    const changing = screen.getByRole("form", { name: "Change DB_NAME" });
    expect(within(changing).getByRole("textbox", { name: "New value of DB_NAME" })).toHaveValue("wordpress");
    expect(within(changing).getByRole("switch", { name: "Secret" })).not.toBeChecked();
    expect(within(changing).getByText("Only this app has it.")).toBeVisible();
    await user.clear(within(changing).getByRole("textbox", { name: "New value of DB_NAME" }));
    await user.keyboard("blog{Enter}");
    expect(within(row("DB_NAME")).getByText("wordpress")).toHaveClass("app-env-from");
    expect(within(row("DB_NAME")).getByText("blog")).toHaveClass("app-env-to");
    await user.click(within(bar()).getByRole("button", { name: /Review plan/ }));
    await waitFor(() => expect(proposals).toHaveLength(1));
    const edit = proposals[0]?.edits[0];
    const spec = edit?.spec?.kind.case === "service" ? edit.spec.kind.value : undefined;
    expect(spec?.runs.case === "app" ? spec.runs.value.env : undefined).toEqual({ DB_NAME: "blog" });
    expect(spec?.shownEnv).toEqual(["DB_HOST", "DB_NAME"]);
    expect(edit?.keepEnv).toEqual(["DB_EXTRA", "DB_HOST"]);
  });

  it("makes one shared in a step: a variable of its value, which the app uses", async () => {
    const user = userEvent.setup();
    const proposals: ProposeChangeRequest[] = [];
    renderAt("/apps/svc_web/environment", api(anInstalledApp(), proposals));
    await ready();
    await user.click(screen.getByRole("button", { name: "Make DB_HOST shared" }));
    expect(screen.getByRole("cell", { name: "DB_HOST, changed, not applied" })).toBeVisible();
    expect(within(screen.getByRole("row", { name: /^DB_HOST/ })).getByText("Shared variable")).toBeVisible();
    await user.click(within(bar()).getByRole("button", { name: /Review plan/ }));
    await waitFor(() => expect(proposals).toHaveLength(1));
    expect(proposals[0]?.summary).toBe("Make DB_HOST on web a shared variable");
    const edits = proposals[0]?.edits ?? [];
    expect(edits.map((e) => `${e.resource?.kind} ${e.resource?.name}`)).toEqual(["variable DB_HOST", "service web"]);
    const spec = edits[1]?.spec?.kind.case === "service" ? edits[1].spec.kind.value : undefined;
    expect(spec?.variables).toEqual({ LOG_LEVEL: "", DB_HOST: "" });
    expect(spec?.runs.case === "app" ? spec.runs.value.env : undefined).toEqual({});
    expect(spec?.shownEnv).toEqual(["DB_NAME"]);
    expect(edits[1]?.keepEnv).toEqual(["DB_EXTRA", "DB_NAME"]);
  });

  it("says why one can't be made shared, where the environment has that name for something else", async () => {
    const more = api(anInstalledApp());
    more.variables = {
      listVariables: () => ({
        environmentId: "env_prod",
        variables: [...variables, ...store({ id: "var_host", name: "DB_HOST", value: "elsewhere" })],
      }),
    };
    renderAt("/apps/svc_web/environment", more);
    await ready();
    const share = screen.getByRole("button", { name: "Make DB_HOST shared" });
    expect(share).toHaveAttribute("aria-disabled", "true");
    expect(share).toHaveAccessibleDescription(
      "The environment has a variable DB_HOST with another value. Rename one to share this.",
    );
  });

  it("opens a value for a change from a link that names it", async () => {
    renderAt("/apps/svc_web/environment?change=DB_HOST", api(anInstalledApp()));
    const changing = await screen.findByRole("form", { name: "Change DB_HOST" }, { timeout: 5000 });
    expect(within(changing).getByRole("textbox", { name: "New value of DB_HOST" })).toHaveValue("db");
  });

  it("keeps a secret out of the way of the environment's own of that name, and asks it for a value", async () => {
    const user = userEvent.setup();
    const more = api(anInstalledApp());
    more.variables = {
      listVariables: () => ({
        environmentId: "env_prod",
        variables: [...variables, ...store({ id: "var_name", name: "DB_NAME", value: "other" })],
      }),
    };
    renderAt("/apps/svc_web/environment", more);
    await ready();
    const form = (name: string) => screen.getByRole("form", { name: `Change ${name}` });
    // A secret of DB_NAME would change the environment's DB_NAME, which other apps may use.
    await user.click(screen.getByRole("button", { name: "Change DB_NAME" }));
    await user.click(within(form("DB_NAME")).getByRole("switch", { name: "Secret" }));
    await user.click(within(form("DB_NAME")).getByRole("button", { name: "Change" }));
    expect(within(form("DB_NAME")).getByRole("alert")).toHaveTextContent(
      "The environment has a variable DB_NAME already. Rename one to keep this as a secret.",
    );
    await user.click(within(form("DB_NAME")).getByRole("button", { name: "Cancel" }));
    await user.click(screen.getByRole("button", { name: "Change DB_HOST" }));
    await user.click(within(form("DB_HOST")).getByRole("switch", { name: "Secret" }));
    await user.clear(within(form("DB_HOST")).getByRole("textbox", { name: "New value of DB_HOST" }));
    await user.click(within(form("DB_HOST")).getByRole("button", { name: "Change" }));
    expect(within(form("DB_HOST")).getByRole("alert")).toHaveTextContent("A secret needs a value.");
    expect(screen.queryByRole("region", { name: "Unapplied changes" })).toBeNull();
  });
});

describe("an app's own value that may be a secret", () => {
  const withReveal = (proposals: ProposeChangeRequest[], asked: RevealServiceValueRequest[], steps: unknown[]) => {
    const more = api(anInstalledApp(), proposals);
    more.auth = {
      beginStepUp: (req) => {
        steps.push(req);
        return { ceremonyId: "cer_1", optionsJson: JSON.stringify({ challenge: "q83v", rpId: "localhost" }) };
      },
      finishStepUp: () => ({ proof: "proof_1" }),
    };
    more.variables = {
      listVariables: () => ({ environmentId: "env_prod", variables }),
      revealServiceValue: (req) => {
        asked.push(req);
        if (req.proof === "") throw authError(Code.PermissionDenied, "needs_step_up");
        return { value: "replica", revision: 7n };
      },
    };
    return more;
  };

  it("reveals with a passkey, for that value alone, and once seen can be said to be no secret", async () => {
    stubPasskeys();
    const user = userEvent.setup();
    const proposals: ProposeChangeRequest[] = [];
    const asked: RevealServiceValueRequest[] = [];
    const steps: unknown[] = [];
    renderAt("/apps/svc_web/environment", withReveal(proposals, asked, steps));
    await ready();
    await user.click(screen.getByRole("button", { name: "Reveal DB_EXTRA" }));
    expect(await screen.findByText("replica")).toBeVisible();
    expect(steps).toEqual([
      expect.objectContaining({ action: StepUpAction.REVEAL_SERVICE_VALUE, target: "svc_web:DB_EXTRA" }),
    ]);
    expect(asked.at(-1)).toEqual(expect.objectContaining({ serviceId: "svc_web", name: "DB_EXTRA", proof: "proof_1" }));
    await user.click(screen.getByRole("button", { name: "DB_EXTRA is no secret: show it" }));
    expect(screen.getByRole("cell", { name: "DB_EXTRA, changed, not applied" })).toBeVisible();
    await user.click(within(bar()).getByRole("button", { name: /Review plan/ }));
    await waitFor(() => expect(proposals).toHaveLength(1));
    const spec =
      proposals[0]?.edits[0]?.spec?.kind.case === "service" ? proposals[0].edits[0].spec.kind.value : undefined;
    expect(spec?.runs.case === "app" ? spec.runs.value.env : undefined).toEqual({ DB_EXTRA: "replica" });
    expect(spec?.shownEnv).toEqual(["DB_EXTRA", "DB_HOST", "DB_NAME"]);
  });

  it("becomes a secret with the value it has, which nobody types again", async () => {
    const user = userEvent.setup();
    const proposals: ProposeChangeRequest[] = [];
    renderAt("/apps/svc_web/environment", api(anInstalledApp(), proposals));
    await ready();
    // One it shows has nothing to keep secret, so it offers to share it instead.
    expect(screen.queryByRole("button", { name: "Make DB_HOST a secret" })).toBeNull();
    await user.click(screen.getByRole("button", { name: "Make DB_EXTRA a secret" }));
    expect(within(screen.getByRole("row", { name: /^DB_EXTRA/ })).getByText("New secret")).toBeVisible();
    await user.click(within(bar()).getByRole("button", { name: /Review plan/ }));
    await waitFor(() => expect(proposals).toHaveLength(1));
    expect(proposals[0]?.summary).toBe("Make DB_EXTRA on web a secret");
    const edit = proposals[0]?.edits[0];
    expect(edit?.envToSecrets).toEqual(["DB_EXTRA"]);
    expect(edit?.keepEnv).toEqual(["DB_HOST", "DB_NAME"]);
    expect(edit?.baseRevision).toBe(7n);
  });

  it("says why it can't become a secret where the environment has the name", async () => {
    const more = api(anInstalledApp());
    more.variables = {
      listVariables: () => ({
        environmentId: "env_prod",
        variables: [...variables, ...store({ id: "sec_extra", name: "DB_EXTRA", secret: true })],
      }),
    };
    renderAt("/apps/svc_web/environment", more);
    await ready();
    const make = screen.getByRole("button", { name: "Make DB_EXTRA a secret" });
    expect(make).toHaveAttribute("aria-disabled", "true");
    expect(make).toHaveAccessibleDescription(
      "The environment has a secret DB_EXTRA already. Rename one to keep this as a secret.",
    );
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
    // REGION stays as api has it, and NEW is the app's own.
    expect(proposals[0]?.edits.map((e) => `${e.resource?.kind} ${e.resource?.name}`)).toEqual(["service web"]);
    const app =
      proposals[0]?.edits[0]?.spec?.kind.case === "service" ? proposals[0].edits[0].spec.kind.value : undefined;
    expect(app?.variables).toEqual({ LOG_LEVEL: "", REGION: "" });
    expect(app?.runs.case === "app" ? app.runs.value.env : undefined).toEqual({ NEW: "1" });
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

  it("uses a variable of the same value as it is, making an own value shared", () => {
    const shared = store({ id: "var_host", name: "DB_HOST", value: "db" });
    const edits = envEdits(
      anInstalledApp(),
      new Map([["DB_HOST", { op: "share", name: "DB_HOST", value: "db" }]]),
      shared,
    );
    expect(edits.map((e) => e.resource?.kind)).toEqual(["service"]);
    const spec = edits[0]?.spec?.kind?.case === "service" ? edits[0].spec.kind.value : undefined;
    expect(spec?.variables).toEqual({ LOG_LEVEL: "", DB_HOST: "" });
  });

  it("stops using a secret without deleting it from the environment", () => {
    const edits = envEdits(anEditedApp(), new Map([["API_KEY", { op: "remove", name: "API_KEY" }]]));
    expect(edits).toHaveLength(1);
    const spec = edits[0]?.spec?.kind?.case === "service" ? edits[0].spec.kind.value : undefined;
    expect(spec?.secrets).toEqual({});
  });
});
