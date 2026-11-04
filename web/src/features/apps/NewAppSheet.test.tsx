// SPDX-License-Identifier: AGPL-3.0-only

import { create } from "@bufbuild/protobuf";
import { Code, ConnectError } from "@connectrpc/connect";
import {
  type ApplyChangeRequest,
  type Change,
  ChangeSchema,
  ChangeState,
  CheckComposeResponseSchema,
  HostAccessKind,
  type ImportComposeRequest,
  PlanAction,
  type ProposeChangeRequest,
  type WatchChangeResponse,
  type WatchServersResponse,
} from "@dockwell/api";
import { screen, waitFor, within } from "@testing-library/react";
import userEvent from "@testing-library/user-event";
import { afterEach, describe, expect, it, vi } from "vitest";

import { activateLocale } from "../../app/i18n";
import { expectAccessible } from "../../test/a11y";
import { anApp, places, SERVER } from "../../test/apps";
import { moved, watched } from "../../test/changes";
import { type Api, feed, onPhone, renderAt } from "../../test/render";
import { aServer, changed, snapshotComplete } from "../../test/servers";

const hz1 = aServer({ id: SERVER, name: "hz-1" });

function planned(fields: Partial<Change> = {}): Change {
  return Object.assign(
    create(ChangeSchema, {
      id: "chg_new",
      summary: "Run nginx:1.27 as nginx",
      state: ChangeState.PLANNED,
      plan: {
        fingerprint: "feedfacefeedfacefeedfacefeedface",
        summary: { create: 1 },
        resources: [
          {
            id: "svc_nginx",
            resource: { kind: "service", scope: "env_prod", name: "nginx" },
            action: PlanAction.CREATE,
          },
        ],
      },
    }),
    fields,
  );
}

function api(more: Api = {}, ...servers: ReturnType<typeof aServer>[]): Api {
  const watch = feed<WatchServersResponse>();
  watch.push(...(servers.length ? servers : [hz1]).map(changed), snapshotComplete);
  return {
    resources: places(),
    services: { listServices: () => ({}), listServiceRevisions: () => ({}) },
    servers: { watchServers: watch.stream },
    ...more,
  };
}

async function openImage(apiImpl: Api) {
  const rendered = renderAt("/apps/new", apiImpl);
  const sheet = await screen.findByRole("dialog", { name: "New app" });
  return { ...rendered, sheet };
}

afterEach(() => {
  vi.unstubAllGlobals();
});

describe("New app, from an image", () => {
  it("asks only for the image, with nothing about where it runs on one server", async () => {
    const { sheet, container } = await openImage(api());
    expect(within(sheet).getByLabelText("Image")).toHaveFocus();
    expect(sheet).toHaveAccessibleDescription("Runs on hz-1");
    expect(within(sheet).queryByRole("group", { name: "Runs on" })).not.toBeInTheDocument();
    expect(within(sheet).queryByLabelText("Environment")).not.toBeInTheDocument();
    // What it doesn't have yet isn't offered.
    for (const later of ["Git", "Catalog"])
      expect(within(sheet).queryByRole("radio", { name: later })).not.toBeInTheDocument();
    expect(within(sheet).getByRole("radio", { name: "Image" })).toBeChecked();
    expect(within(sheet).getByRole("radio", { name: "Compose" })).toBeVisible();
    // Ports, variables and volumes wait behind a button each.
    expect(within(sheet).queryByLabelText("On the server")).not.toBeInTheDocument();
    await expectAccessible(container);
    await expectAccessible(document.body);
  });

  it("plans it, reviews it, applies with live progress, and ends at the app", async () => {
    const user = userEvent.setup();
    const proposals: ProposeChangeRequest[] = [];
    const applies: ApplyChangeRequest[] = [];
    const watch = feed<WatchChangeResponse>();
    const { sheet, router } = await openImage(
      api({
        changes: {
          proposeChange: (req) => {
            proposals.push(req);
            return { change: planned() };
          },
          watchChange: watch.stream,
          applyChange: (req) => {
            applies.push(req);
            return { change: planned({ state: ChangeState.APPLYING }) };
          },
          discardChange: () => ({}),
        },
      }),
    );
    await user.type(within(sheet).getByLabelText("Image"), "nginx:1.27");
    // The name comes from the image unless you say otherwise.
    expect(within(sheet).getByPlaceholderText("nginx")).toHaveValue("");
    await user.click(within(sheet).getByRole("button", { name: "Review plan" }));

    const plan = await screen.findByRole("dialog", { name: "Review plan" });
    expect(screen.queryByRole("dialog", { name: "New app" })).not.toBeInTheDocument();
    expect(proposals[0]?.summary).toBe("Run nginx:1.27 as nginx");
    expect(proposals[0]?.edits[0]?.resource).toMatchObject({ kind: "service", name: "nginx", scope: "" });
    expect(proposals[0]?.edits[0]?.servers).toEqual([]);

    await user.click(within(plan).getByRole("button", { name: /Apply plan/ }));
    await screen.findByRole("dialog", { name: "Applying" });
    expect(applies[0]?.fingerprint).toBe("feedfacefeedfacefeedfacefeedface");
    watch.push(watched(moved(planned(), { state: ChangeState.APPLIED })));
    const done = await screen.findByRole("dialog", { name: "Applied" });
    await user.click(within(done).getByRole("button", { name: "Open nginx" }));
    await waitFor(() => expect(router.state.location.pathname).toBe("/apps/svc_nginx"));
  });

  it("comes back to the form when the first deploy fails, which Dockwell rolled back", async () => {
    const user = userEvent.setup();
    const watch = feed<WatchChangeResponse>();
    const { sheet } = await openImage(
      api({
        changes: {
          proposeChange: () => ({ change: planned() }),
          watchChange: watch.stream,
          applyChange: () => ({ change: planned({ state: ChangeState.APPLYING }) }),
          discardChange: () => ({}),
        },
      }),
    );
    await user.type(within(sheet).getByLabelText("Image"), "nginx:doesnotexist");
    await user.click(within(sheet).getByRole("button", { name: "Review plan" }));
    await user.click(await screen.findByRole("button", { name: /Apply plan/ }));
    await screen.findByRole("dialog", { name: "Applying" });
    watch.push(
      watched(moved(planned(), { state: ChangeState.FAILED, error: "pull access denied", rolledBackBy: "chg_back" })),
    );
    const failed = await screen.findByRole("dialog", { name: "The change failed" });
    expect(failed).toHaveTextContent("pull access denied");
    await user.click(within(failed).getByRole("button", { name: "Done" }));
    const back = await screen.findByRole("dialog", { name: "New app" });
    expect(within(back).getByLabelText("Image")).toHaveValue("nginx:doesnotexist");
  });

  it("comes back to the form as you left it when you cancel the plan, and discards the plan", async () => {
    const user = userEvent.setup();
    const discard = vi.fn(() => ({}));
    const { sheet } = await openImage(
      api({
        changes: {
          proposeChange: () => ({ change: planned() }),
          watchChange: feed<WatchChangeResponse>().stream,
          discardChange: discard,
        },
      }),
    );
    await user.type(within(sheet).getByLabelText("Image"), "nginx:1.27");
    await user.click(within(sheet).getByRole("button", { name: "Review plan" }));
    const plan = await screen.findByRole("dialog", { name: "Review plan" });
    await user.click(within(plan).getByRole("button", { name: "Cancel" }));
    const back = await screen.findByRole("dialog", { name: "New app" });
    expect(within(back).getByLabelText("Image")).toHaveValue("nginx:1.27");
    await waitFor(() => expect(discard).toHaveBeenCalled());
  });

  it("asks before losing what you typed, and leaves at once when you typed nothing", async () => {
    const user = userEvent.setup();
    const { sheet, router } = await openImage(api());
    await user.type(within(sheet).getByLabelText("Image"), "nginx");
    await user.keyboard("{Escape}");
    expect(within(sheet).getByRole("status")).toHaveTextContent("Close, and lose what you typed here?");
    await user.click(within(sheet).getByRole("button", { name: "Keep editing" }));
    expect(within(sheet).getByLabelText("Image")).toHaveValue("nginx");
    await user.click(within(sheet).getByRole("button", { name: "Cancel" }));
    await user.click(within(sheet).getByRole("button", { name: "Discard" }));
    await waitFor(() => expect(router.state.location.pathname).toBe("/apps"));
  });

  it("goes on with Enter in a field", async () => {
    const user = userEvent.setup();
    const propose = vi.fn(() => ({ change: planned() }));
    const { sheet } = await openImage(
      api({
        changes: { proposeChange: propose, watchChange: feed<WatchChangeResponse>().stream, discardChange: () => ({}) },
      }),
    );
    await user.type(within(sheet).getByLabelText("Image"), "nginx:1.27{Enter}");
    expect(await screen.findByRole("dialog", { name: "Review plan" })).toBeVisible();
    expect(propose).toHaveBeenCalledOnce();
  });

  it("says at the field what you can fix, before asking the API", async () => {
    const user = userEvent.setup();
    const propose = vi.fn();
    const { sheet } = await openImage(api({ changes: { proposeChange: propose } }));
    await user.click(within(sheet).getByRole("button", { name: "Review plan" }));
    expect(within(sheet).getByLabelText("Image")).toHaveAccessibleDescription(/Name the image to run/);
    await waitFor(() => expect(within(sheet).getByLabelText("Image")).toHaveFocus());

    await user.type(within(sheet).getByLabelText("Image"), "nginx");
    await user.type(within(sheet).getByPlaceholderText("nginx"), "Not A Name");
    await user.click(within(sheet).getByRole("button", { name: "Add port" }));
    await user.type(within(sheet).getByLabelText("On the server"), "99999");
    await user.click(within(sheet).getByRole("button", { name: "Add variable" }));
    await user.type(within(sheet).getByPlaceholderText("LOG_LEVEL"), "1BAD");
    await user.click(within(sheet).getByRole("button", { name: "Add volume" }));
    await user.click(within(sheet).getByRole("button", { name: "Review plan" }));
    expect(within(sheet).getByLabelText(/^Name/, { selector: "[placeholder=nginx]" })).toHaveAccessibleDescription(
      /lowercase letters/,
    );
    expect(within(sheet).getByLabelText("On the server")).toHaveAccessibleDescription("Use a port from 1 to 65535.");
    expect(within(sheet).getByPlaceholderText("LOG_LEVEL")).toHaveAccessibleDescription(
      /Use letters, digits and underscores/,
    );
    expect(within(sheet).getByLabelText("Volume or server path")).toHaveAccessibleDescription(/Name a volume/);
    expect(propose).not.toHaveBeenCalled();
  });

  it("won't take the name of an app that runs here, which would change it", async () => {
    const user = userEvent.setup();
    const propose = vi.fn();
    const { sheet } = await openImage(
      api({
        services: {
          listServices: () => ({ services: [anApp("nginx", "nginx:1.26")] }),
          listServiceRevisions: () => ({}),
        },
        changes: { proposeChange: propose },
      }),
    );
    await user.type(within(sheet).getByLabelText("Image"), "nginx:1.27");
    await waitFor(async () => {
      await user.click(within(sheet).getByRole("button", { name: "Review plan" }));
      expect(within(sheet).getByPlaceholderText("nginx")).toHaveAccessibleDescription(
        /An app named nginx already runs here/,
      );
    });
    expect(propose).not.toHaveBeenCalled();
  });

  it("puts the API's refusal at the field it names, in plain language", async () => {
    const user = userEvent.setup();
    const { sheet } = await openImage(
      api({
        changes: {
          proposeChange: () => {
            const err = new ConnectError(
              "service agent in env_01M6M9 isn't valid: the server path / mounted at /host is the server's whole file system, so no container may mount it. Mount only the directory the container needs, such as /srv/app/data, or a volume",
              Code.InvalidArgument,
            );
            err.metadata.set("Dockwell-Error", "invalid");
            throw err;
          },
        },
      }),
    );
    await user.type(within(sheet).getByLabelText("Image"), "nginx:1.27");
    await user.click(within(sheet).getByRole("button", { name: "Add volume" }));
    await user.type(within(sheet).getByLabelText("Volume or server path"), "/");
    await user.type(within(sheet).getByLabelText("Path in the app"), "/host");
    await user.click(within(sheet).getByRole("button", { name: "Review plan" }));
    const path = within(sheet).getByLabelText("Volume or server path");
    await waitFor(() => expect(path).toHaveAccessibleDescription(/The server path \/ mounted at \/host/));
    expect(path).not.toHaveAccessibleDescription(/env_01M6M9/);
    await waitFor(() => expect(path).toHaveFocus());
    expect(screen.queryByRole("dialog", { name: "Review plan" })).not.toBeInTheDocument();
  });

  it("says above the form what is about none of its fields", async () => {
    const user = userEvent.setup();
    const { sheet } = await openImage(
      api({
        changes: {
          proposeChange: () => {
            throw new ConnectError(
              "hz-1 can't run service web now: Docker is installed, but isn't running. Start it with systemctl start docker",
              Code.FailedPrecondition,
            );
          },
        },
      }),
    );
    await user.type(within(sheet).getByLabelText("Image"), "nginx:1.27");
    await user.click(within(sheet).getByRole("button", { name: "Review plan" }));
    expect(await within(sheet).findByRole("alert")).toHaveTextContent("Start it with systemctl start docker");
  });

  it("makes the plan with ports, variables, volumes, and an .env file pasted in", async () => {
    const user = userEvent.setup();
    const proposals: ProposeChangeRequest[] = [];
    const { sheet } = await openImage(
      api({
        changes: {
          proposeChange: (req) => {
            proposals.push(req);
            return { change: planned() };
          },
          watchChange: feed<WatchChangeResponse>().stream,
          discardChange: () => ({}),
        },
      }),
    );
    await user.type(within(sheet).getByLabelText("Image"), "nginx:1.27");
    await user.click(within(sheet).getByRole("button", { name: "Add port" }));
    await user.type(within(sheet).getByLabelText("On the server"), "8080");
    await user.type(within(sheet).getByLabelText("In the app"), "80");
    await user.click(within(sheet).getByRole("button", { name: "Add variable" }));
    within(sheet).getByPlaceholderText("LOG_LEVEL").focus();
    await user.paste("A=1\nB=2");
    expect(
      within(sheet)
        .getAllByLabelText("Value")
        .map((v) => (v as HTMLInputElement).value),
    ).toEqual(["1", "2"]);
    await user.click(within(sheet).getByRole("button", { name: "Add volume" }));
    await user.type(within(sheet).getByLabelText("Volume or server path"), "site");
    await user.type(within(sheet).getByLabelText("Path in the app"), "/data");
    expect(within(sheet).getByText(/makes the volume/)).toBeVisible();
    await user.click(within(sheet).getByRole("button", { name: "Review plan" }));
    await screen.findByRole("dialog", { name: "Review plan" });
    const edits = proposals[0]?.edits ?? [];
    expect(edits.map((e) => `${e.resource?.kind} ${e.resource?.name}`)).toEqual(["volume site", "service nginx"]);
  });

  it("says what a server path reaches at the field, and the plan asks a person for it", async () => {
    const user = userEvent.setup();
    const { sheet } = await openImage(
      api({
        changes: {
          proposeChange: () => ({
            change: planned({
              plan: create(ChangeSchema, {
                plan: {
                  fingerprint: "feedfacefeedfacefeedfacefeedface",
                  hostAccess: true,
                  summary: { create: 1 },
                  resources: [
                    {
                      id: "svc_agent",
                      resource: { kind: "service", name: "agent" },
                      action: PlanAction.CREATE,
                      hostAccess: [{ kind: HostAccessKind.DOCKER_SOCKET, path: "/var/run/docker.sock" }],
                    },
                  ],
                },
              }).plan,
            }),
          }),
          watchChange: feed<WatchChangeResponse>().stream,
          discardChange: () => ({}),
        },
      }),
    );
    await user.type(within(sheet).getByLabelText("Image"), "portainer/portainer-ce:2");
    await user.click(within(sheet).getByRole("button", { name: "Add volume" }));
    const source = within(sheet).getByLabelText("Volume or server path");
    await user.type(source, "/srv/data");
    expect(within(sheet).getByText(/on the server\. A signed-in person has to approve/)).toBeVisible();
    await user.clear(source);
    await user.type(source, "/var/run/docker.sock");
    expect(within(sheet).getByText(/control of Docker on the server, the same as root/)).toBeVisible();
    expect(within(sheet).getByText("You approve what it reaches in the plan")).toBeVisible();
    await user.type(within(sheet).getByLabelText("Path in the app"), "/var/run/docker.sock");
    await user.click(within(sheet).getByRole("button", { name: "Review plan" }));
    const plan = await screen.findByRole("dialog", { name: "Review plan" });
    expect(plan).toHaveTextContent("This plan gives more access to a server");
    expect(plan).toHaveTextContent("Docker's socket at /var/run/docker.sock");
    expect(within(plan).getByRole("button", { name: /Apply plan/ })).toBeDisabled();
    await user.type(within(plan).getByLabelText(/to apply it/), "feedface");
    expect(within(plan).getByRole("button", { name: /Apply plan/ })).toBeEnabled();
  });

  it("asks where it runs from the second server on, and in which environment once there is a choice", async () => {
    const user = userEvent.setup();
    const proposals: ProposeChangeRequest[] = [];
    const { sheet } = await openImage(
      api(
        {
          resources: places("staging"),
          changes: {
            proposeChange: (req) => {
              proposals.push(req);
              return { change: planned() };
            },
            watchChange: feed<WatchChangeResponse>().stream,
            discardChange: () => ({}),
          },
        },
        hz1,
        aServer({ id: "srv_2", name: "hz-2" }),
      ),
    );
    const runsOn = await within(sheet).findByRole("group", { name: "Runs on" });
    await user.selectOptions(await within(sheet).findByLabelText("Environment"), "env_staging");
    await user.type(within(sheet).getByLabelText("Image"), "nginx:1.27");
    await user.click(within(sheet).getByRole("button", { name: "Review plan" }));
    expect(within(sheet).getByText("Choose where it runs.")).toBeVisible();
    await user.click(within(runsOn).getByRole("checkbox", { name: "hz-2" }));
    await user.click(within(sheet).getByRole("button", { name: "Review plan" }));
    await screen.findByRole("dialog", { name: "Review plan" });
    expect(proposals[0]?.edits[0]?.servers).toEqual(["srv_2"]);
    expect(proposals[0]?.edits[0]?.resource?.scope).toBe("env_staging");
  });

  it("fills a phone, with room for a thumb", async () => {
    onPhone();
    const { sheet, container } = await openImage(api());
    expect(sheet).toHaveClass("is-phone");
    await expectAccessible(document.body);
    await expectAccessible(container);
  });

  it("speaks Chinese", async () => {
    await activateLocale("zh-CN");
    renderAt("/apps/new", api());
    const sheet = await screen.findByRole("dialog", { name: "新建应用" });
    expect(within(sheet).getByRole("button", { name: "添加端口" })).toBeVisible();
    expect(within(sheet).getByLabelText("镜像")).toBeVisible();
  });
});

describe("New app, from a Compose file", () => {
  const web = anApp("web", "ghcr.io/acme/web:1", {
    ports: [{ hostPort: 8080, containerPort: 80 }],
    env: { API_KEY: "" },
    mounts: [{ source: { case: "hostPath", value: "/srv/conf" }, target: "/conf" }],
  });
  const compose = (more: Api["compose"]) => api({ compose: more ?? {} });

  it("says what a file makes as you type, then plans it and ends at the list", async () => {
    const user = userEvent.setup();
    const imports: ImportComposeRequest[] = [];
    const watch = feed<WatchChangeResponse>();
    const { router, container } = renderAt("/apps/new?source=compose", {
      ...compose({
        checkCompose: () =>
          create(CheckComposeResponseSchema, {
            services: [web, anApp("db", "postgres:17")],
            volumes: ["data"],
            networks: ["default"],
          }),
        importCompose: (req) => {
          imports.push(req);
          return { change: planned({ summary: "Run a Compose file of 2 services" }) };
        },
      }),
      changes: {
        watchChange: watch.stream,
        applyChange: () => ({ change: planned({ state: ChangeState.APPLYING }) }),
        discardChange: () => ({}),
      },
    });
    const sheet = await screen.findByRole("dialog", { name: "New app" });
    expect(within(sheet).getByRole("radio", { name: "Compose" })).toBeChecked();
    const review = within(sheet).getByRole("button", { name: "Review plan" });
    expect(review).toBeDisabled();
    await user.click(within(sheet).getByLabelText("Compose file"));
    await user.paste("services:\n  web:\n    image: ghcr.io/acme/web:1\n");
    const preview = await within(sheet).findByRole("region", { name: "What this file makes" });
    expect(preview).toHaveTextContent("This makes 2 services");
    expect(preview).toHaveTextContent("ghcr.io/acme/web:1");
    expect(preview).toHaveTextContent("8080:80");
    expect(preview).toHaveTextContent("Reaches /srv/conf");
    expect(preview).toHaveTextContent("data");
    expect(within(sheet).getByText("You approve what it reaches in the plan")).toBeVisible();
    await expectAccessible(document.body);
    await waitFor(() => expect(review).toBeEnabled());
    await user.click(review);
    await screen.findByRole("dialog", { name: "Review plan" });
    expect(imports[0]?.summary).toBe("Run a Compose file of 2 services");
    expect(imports[0]?.compose).toContain("ghcr.io/acme/web:1");
    expect(imports[0]?.environment).toBe("");
    expect(imports[0]?.servers).toEqual([]);
    await user.click(screen.getByRole("button", { name: /Apply plan/ }));
    await screen.findByRole("dialog", { name: "Applying" });
    watch.push(watched(moved(planned(), { state: ChangeState.APPLIED })));
    await user.click(await screen.findByRole("button", { name: "Show the apps" }));
    await waitFor(() => expect(router.state.location.pathname).toBe("/apps"));
    expect(container).toBeTruthy();
  });

  it("refuses at the line, by name, in plain language, and can't go on", async () => {
    const user = userEvent.setup();
    renderAt(
      "/apps/new?source=compose",
      compose({
        checkCompose: () =>
          create(CheckComposeResponseSchema, {
            refusal: {
              message:
                "the service web builds an image, which Dockwell can't do yet. Build and push the image, then name it with image",
              line: 3,
            },
          }),
      }),
    );
    const sheet = await screen.findByRole("dialog", { name: "New app" });
    const field = within(sheet).getByLabelText("Compose file");
    await user.click(field);
    await user.paste("services:\n  web:\n    build: .\n");
    await waitFor(() => expect(field).toHaveAccessibleDescription(/^The service web builds an image/));
    expect(field).toHaveAttribute("aria-invalid", "true");
    expect(document.querySelector(".dw-code-mark")).toHaveStyle({ "--dw-code-line": "2" });
    expect(within(sheet).getByRole("button", { name: "Review plan" })).toBeDisabled();
    await expectAccessible(document.body);
  });

  it("asks for what the file leaves out, rather than calling it wrong, and sends it", async () => {
    const user = userEvent.setup();
    const checks: Record<string, string>[] = [];
    renderAt(
      "/apps/new?source=compose",
      compose({
        checkCompose: (req) => {
          checks.push(req.variables);
          return req.variables["TAG"]
            ? create(CheckComposeResponseSchema, {
                variables: [{ name: "TAG", required: true }, { name: "HOST" }],
                services: [web],
              })
            : create(CheckComposeResponseSchema, {
                variables: [{ name: "TAG", required: true }, { name: "HOST" }],
                refusal: { message: "the Compose file uses TAG, with no value" },
              });
        },
      }),
    );
    const sheet = await screen.findByRole("dialog", { name: "New app" });
    const field = within(sheet).getByLabelText("Compose file");
    await user.click(field);
    await user.paste("services:\n  web:\n    image: a:$TAG\n");
    const tag = await within(sheet).findByLabelText("TAG");
    expect(field).not.toHaveAttribute("aria-invalid");
    // What has a default waits behind a button.
    expect(within(sheet).queryByLabelText(/HOST/)).not.toBeInTheDocument();
    await user.click(within(sheet).getByRole("button", { name: "1 with a default" }));
    expect(within(sheet).getByLabelText(/HOST/)).toBeVisible();
    await user.type(tag, "1.4");
    expect(await within(sheet).findByRole("region", { name: "What this file makes" })).toBeVisible();
    await waitFor(() => expect(checks.at(-1)).toEqual({ TAG: "1.4" }));
  });

  it("offers to remove what isn't in the file only when something isn't", async () => {
    const user = userEvent.setup();
    const imports: ImportComposeRequest[] = [];
    renderAt("/apps/new?source=compose", {
      ...compose({
        checkCompose: () => create(CheckComposeResponseSchema, { services: [web] }),
        importCompose: (req) => {
          imports.push(req);
          return { change: planned() };
        },
      }),
      services: {
        listServices: () => ({ services: [anApp("old", "old:1"), anApp("web", "x")] }),
        listServiceRevisions: () => ({}),
      },
      changes: { watchChange: feed<WatchChangeResponse>().stream, discardChange: () => ({}) },
    });
    const sheet = await screen.findByRole("dialog", { name: "New app" });
    await user.click(within(sheet).getByLabelText("Compose file"));
    await user.paste("services:\n  web:\n    image: a\n");
    const remove = await within(sheet).findByRole("switch", { name: /Remove what isn't in this file/ });
    expect(sheet).toHaveTextContent("old is here already");
    await user.click(remove);
    await waitFor(() => expect(within(sheet).getByRole("button", { name: "Review plan" })).toBeEnabled());
    await user.click(within(sheet).getByRole("button", { name: "Review plan" }));
    await screen.findByRole("dialog", { name: "Review plan" });
    expect(imports[0]?.removeOrphans).toBe(true);
  });
});
