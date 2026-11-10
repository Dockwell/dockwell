// SPDX-License-Identifier: AGPL-3.0-only

import { create } from "@bufbuild/protobuf";
import { Code, ConnectError } from "@connectrpc/connect";
import {
  type ApplyChangeRequest,
  type Change,
  ChangeSchema,
  ChangeState,
  CheckComposeResponseSchema,
  FileType,
  HostAccessKind,
  ImageAnswer,
  type ImportComposeRequest,
  PlanAction,
  type ProposeChangeRequest,
  RestartPolicy,
  StepProgressSchema,
  StepState,
  type WatchChangeResponse,
  type WatchServersResponse,
} from "@dockwell/api";
import { screen, waitFor, within } from "@testing-library/react";
import userEvent from "@testing-library/user-event";
import { afterEach, describe, expect, it, vi } from "vitest";

import { activateLocale } from "../../app/i18n";
import { expectAccessible } from "../../test/a11y";
import { anApp, places, projectPlaces, SERVER } from "../../test/apps";
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
    const done = await screen.findByRole("dialog", { name: "nginx is deployed" });
    // Once it's up, the arrival's own button opens the app at its log.
    await user.click(within(done).getByRole("button", { name: "Open nginx" }));
    await waitFor(() => expect(router.state.location.pathname).toBe("/apps/svc_nginx/logs"));
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
    const failed = await screen.findByRole("dialog", { name: "Couldn't create nginx" });
    expect(failed).toHaveTextContent("pull access denied");
    await user.click(within(failed).getByRole("button", { name: /Edit and try again/ }));
    const back = await screen.findByRole("dialog", { name: "New app" });
    const field = within(back).getByLabelText<HTMLInputElement>("Image");
    expect(field).toHaveValue("nginx:doesnotexist");
    // What went wrong is at the field, ready to change, not on a button.
    expect(field).toHaveAccessibleDescription("Pull access denied");
    await waitFor(() => {
      expect(field).toHaveFocus();
      expect(field.value.slice(field.selectionStart ?? 0, field.selectionEnd ?? 0)).toBe("nginx:doesnotexist");
    });
  });

  it("leaves what a failed check said, and what to do about it, as the server says it", async () => {
    const user = userEvent.setup();
    const said =
      "nginx runs, but fails its health check: GET /healthz on port 8080 answered 503 Service Unavailable. Fix what it answers, or the check, then try again.";
    const failed = moved(planned(), {
      state: ChangeState.FAILED,
      error: said,
      steps: [
        create(StepProgressSchema, {
          serverId: "srv_1",
          resourceId: "svc_nginx",
          state: StepState.FAILED,
          message: said,
          reason: "unhealthy",
        }),
      ],
    });
    const { sheet } = await openImage(
      api({
        changes: {
          proposeChange: () => ({ change: failed }),
          watchChange: feed<WatchChangeResponse>().stream,
          discardChange: () => ({}),
        },
      }),
    );
    await user.type(within(sheet).getByLabelText("Image"), "nginx:sick");
    await user.click(within(sheet).getByRole("button", { name: "Review plan" }));
    const plan = await screen.findByRole("dialog", { name: "Couldn't create nginx" });
    const notice = within(plan).getByRole("alert");
    expect(notice).toHaveTextContent("answered 503 Service Unavailable");
    expect(notice).toHaveTextContent("Fix what it answers, or the check, then try again.");
    expect(notice).not.toHaveTextContent("Check the image and its variables");
  });

  it("says what to check in what this form has, and lets you leave from the failure", async () => {
    const user = userEvent.setup();
    const failed = moved(planned(), {
      state: ChangeState.FAILED,
      error: "nginx stopped right after it started, with exit code 1. Check its command and variables, then try again.",
      steps: [
        create(StepProgressSchema, {
          serverId: "srv_1",
          resourceId: "svc_nginx",
          state: StepState.FAILED,
          message: "x",
          reason: "exits",
        }),
      ],
    });
    const { sheet, router } = await openImage(
      api({
        changes: {
          proposeChange: () => ({ change: failed }),
          watchChange: feed<WatchChangeResponse>().stream,
          discardChange: () => ({}),
        },
      }),
    );
    await user.type(within(sheet).getByLabelText("Image"), "nginx:broken");
    await user.click(within(sheet).getByRole("button", { name: "Review plan" }));
    const plan = await screen.findByRole("dialog", { name: "Couldn't create nginx" });
    const notice = within(plan).getByRole("alert");
    // This form has no command to check.
    expect(notice).toHaveTextContent("Check the image and its variables, then try again.");
    expect(notice).not.toHaveTextContent("command");
    await user.click(within(plan).getByRole("button", { name: /^Done/ }));
    await waitFor(() => expect(router.state.location.pathname).toBe("/apps"));
  });

  it("keeps one shell and one scrim from the form to its plan and back, with your values and focus", async () => {
    const user = userEvent.setup();
    const { sheet } = await openImage(
      api({
        changes: {
          proposeChange: () => ({ change: planned() }),
          watchChange: feed<WatchChangeResponse>().stream,
          discardChange: () => ({}),
        },
      }),
    );
    const scrim = document.querySelector(".dw-scrim");
    const image = within(sheet).getByLabelText("Image");
    await user.type(image, "nginx:1.27");
    await user.keyboard("{Control>}{Enter}{/Control}");
    const plan = await screen.findByRole("dialog", { name: "Review plan" });
    expect(plan).toBe(sheet);
    expect(document.querySelector(".dw-scrim")).toBe(scrim);
    expect(document.querySelectorAll(".dw-sheet")).toHaveLength(1);
    await user.click(within(plan).getByRole("button", { name: "Cancel" }));
    const back = await screen.findByRole("dialog", { name: "New app" });
    expect(back).toBe(sheet);
    expect(document.querySelector(".dw-scrim")).toBe(scrim);
    expect(within(back).getByLabelText("Image")).toHaveValue("nginx:1.27");
    expect(within(back).getByLabelText("Image")).toHaveFocus();
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
    await user.type(within(sheet).getAllByLabelText("Name")[1] as HTMLElement, "1BAD");
    await user.click(within(sheet).getByRole("button", { name: "Add volume" }));
    await user.click(within(sheet).getByRole("button", { name: "Review plan" }));
    expect(within(sheet).getByLabelText(/^Name/, { selector: "[placeholder=nginx]" })).toHaveAccessibleDescription(
      /lowercase letters/,
    );
    expect(within(sheet).getByLabelText("On the server")).toHaveAccessibleDescription("Use a port from 1 to 65535.");
    expect(within(sheet).getAllByLabelText("Name")[1]).toHaveAccessibleDescription(
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

  it("picks a volume's server folder from the server's own folders", async () => {
    const user = userEvent.setup();
    const proposals: ProposeChangeRequest[] = [];
    const listed: string[] = [];
    const { sheet } = await openImage(
      api({
        files: {
          listFiles: (req) => {
            listed.push(`${req.serverId} ${req.path} ${req.lookup}`);
            const folder = (name: string) => ({ name, type: FileType.DIRECTORY });
            return req.path === "/"
              ? { path: "/", entries: [folder("srv"), folder("etc"), { name: "notes.txt", type: FileType.FILE }] }
              : { path: req.path, entries: [] };
          },
        },
        changes: {
          proposeChange: (req) => {
            proposals.push(req);
            throw new ConnectError("stop here", Code.FailedPrecondition);
          },
        },
      }),
    );
    await user.type(within(sheet).getByLabelText("Image"), "nginx:1.27");
    await user.click(within(sheet).getByRole("button", { name: "Add volume" }));
    await user.click(within(sheet).getByRole("button", { name: "Pick a folder on the server" }));
    // The picker opens on the server's folders, as a lookup, and a file isn't one.
    const folders = await within(sheet).findByRole("list", { name: "Folders in /" });
    expect(
      within(folders)
        .getAllByRole("button")
        .map((b) => b.textContent),
    ).toEqual(["srv", "etc"]);
    expect(listed[0]).toBe(`${SERVER} / true`);
    await user.click(within(folders).getByRole("button", { name: "srv" }));
    await user.type(within(sheet).getByLabelText("Path in the app"), "/data");
    await user.click(within(sheet).getByRole("button", { name: "Review plan" }));
    await waitFor(() => expect(proposals).toHaveLength(1));
    const edit = proposals[0]?.edits.find((e) => e.resource?.kind === "service");
    const service = edit?.spec?.kind.case === "service" ? edit.spec.kind.value : undefined;
    const mounts = service?.runs.case === "app" ? service.runs.value.mounts : [];
    expect(mounts[0]?.source).toEqual({ case: "hostPath", value: "/srv" });
    // And back to a volume, typed.
    await user.click(within(sheet).getByRole("button", { name: "Use a volume instead" }));
    expect(within(sheet).getByLabelText("Volume or server path")).toHaveValue("");
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
    // What the app listens on is the person's to say; the server's port starts at a free one.
    await user.type(within(sheet).getByLabelText("In the app"), "80");
    await waitFor(() => expect(within(sheet).getByLabelText<HTMLInputElement>("On the server").value).toBe("8080"));
    await user.click(within(sheet).getByRole("button", { name: "Add variable" }));
    within(sheet).getAllByLabelText("Name")[1]?.focus();
    await user.paste("A=1\nB=2");
    // Its fields are named once over the first row, and read out on every row.
    expect(sheet.querySelectorAll(".app-env label")).toHaveLength(2);
    expect(within(sheet).getAllByLabelText("Value")).toHaveLength(2);
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
    const app = edits[1]?.spec?.kind.case === "service" ? edits[1].spec.kind.value.runs.value : undefined;
    expect(app?.ports.map((p) => `${p.hostPort}:${p.containerPort}`)).toEqual(["8080:80"]);
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
    expect(within(sheet).getByText(/on the server\. You approve this in the plan; API tokens can't/)).toBeVisible();
    await user.clear(source);
    await user.type(source, "/var/run/docker.sock");
    expect(within(sheet).getByText(/control of Docker on the server, the same as root/)).toBeVisible();
    expect(within(sheet).getByText("You approve what it reaches in the plan")).toBeVisible();
    await user.type(within(sheet).getByLabelText("Path in the app"), "/var/run/docker.sock");
    await user.click(within(sheet).getByRole("button", { name: "Review plan" }));
    const plan = await screen.findByRole("dialog", { name: "Review plan" });
    const risk = within(plan).getByRole("alert");
    expect(risk).toHaveTextContent(/gets root on/);
    expect(risk).toHaveTextContent("/var/run/docker.sock gives it control of Docker on the server, the same as root.");
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
          return { change: planned({ summary: "Run a Compose file" }) };
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
    expect(preview).toHaveTextContent("This makes 2 apps");
    expect(preview).toHaveTextContent("ghcr.io/acme/web:1");
    expect(preview).toHaveTextContent("8080:80");
    expect(preview).toHaveTextContent("Reaches /srv/conf");
    expect(preview).toHaveTextContent("data");
    expect(within(sheet).getByText("You approve what it reaches in the plan")).toBeVisible();
    await expectAccessible(document.body);
    await waitFor(() => expect(review).toBeEnabled());
    await user.click(review);
    await screen.findByRole("dialog", { name: "Review plan" });
    expect(imports[0]?.summary).toBe("Run a Compose file");
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
                "the service web uses `build`, and Dockwell can't build an image yet. Build and push the image, then name it with `image`",
              line: 3,
            },
          }),
      }),
    );
    const sheet = await screen.findByRole("dialog", { name: "New app" });
    const field = within(sheet).getByLabelText("Compose file");
    await user.click(field);
    await user.paste("services:\n  web:\n    build: .\n");
    await waitFor(() => expect(field).toHaveAccessibleDescription(/^Line 3: the service web uses build/));
    expect(field).toHaveAttribute("aria-invalid", "true");
    expect(document.querySelector(".dw-code-mark")).toHaveStyle({ "--dw-code-line": "2" });
    // The file's own words are in mono.
    const words = [...sheet.querySelectorAll(".dw-field-error code")].map((c) => c.textContent);
    expect(words).toEqual(["build", "image"]);
    const review = within(sheet).getByRole("button", { name: "Review plan" });
    expect(review).toBeDisabled();
    // And the footer says why it can't go on.
    expect(within(sheet).getByText("Fix line 3 to review the plan")).toBeVisible();
    await expectAccessible(document.body);
  });

  it("says a file that isn't YAML at the line the parser gave up on, with no parser in it", async () => {
    const user = userEvent.setup();
    renderAt(
      "/apps/new?source=compose",
      compose({
        checkCompose: () =>
          create(CheckComposeResponseSchema, {
            refusal: { message: "line 6: check the indentation, and that quotes and brackets close", line: 6 },
          }),
      }),
    );
    const sheet = await screen.findByRole("dialog", { name: "New app" });
    const field = within(sheet).getByLabelText("Compose file");
    await user.click(field);
    await user.paste("services:\n  web:\n  image: [\n");
    await waitFor(() =>
      expect(field).toHaveAccessibleDescription("Line 6: check the indentation, and that quotes and brackets close."),
    );
    expect(document.querySelector(".dw-code-mark")).toHaveStyle({ "--dw-code-line": "5" });
  });

  it("says a file over the limit in plain words, without asking the API", async () => {
    const user = userEvent.setup();
    const check = vi.fn(() => create(CheckComposeResponseSchema, {}));
    renderAt("/apps/new?source=compose", compose({ checkCompose: check }));
    const sheet = await screen.findByRole("dialog", { name: "New app" });
    const field = within(sheet).getByLabelText("Compose file");
    await user.click(field);
    await user.paste(`# ${"x".repeat(300 << 10)}\nservices: {}\n`);
    await waitFor(() =>
      expect(field).toHaveAccessibleDescription(/^This file is 301 KiB, and Dockwell reads files of up to 256 KiB/),
    );
    expect(within(sheet).getByRole("button", { name: "Review plan" })).toBeDisabled();
    expect(within(sheet).getByText("This file is too large to read")).toBeVisible();
    await new Promise((r) => setTimeout(r, 400));
    expect(check).not.toHaveBeenCalled();
  });

  it("keeps the field to twelve lines, however long the file, so what comes next stays near", async () => {
    const user = userEvent.setup();
    const { container } = renderAt(
      "/apps/new?source=compose",
      compose({ checkCompose: () => create(CheckComposeResponseSchema, {}) }),
    );
    const sheet = await screen.findByRole("dialog", { name: "New app" });
    const input = () => container.ownerDocument.querySelector<HTMLElement>(".dw-code-input");
    expect(input()).toHaveStyle({ "--dw-code-lines": "12" });
    await user.click(within(sheet).getByLabelText("Compose file"));
    await user.paste("x\n".repeat(200));
    expect(input()).toHaveStyle({ "--dw-code-lines": "12" });
  });

  it("takes you to the variable it's waiting for, and hides what you type there until you ask", async () => {
    const user = userEvent.setup();
    const scrolled = vi.fn();
    Element.prototype.scrollIntoView = scrolled;
    renderAt(
      "/apps/new?source=compose",
      compose({
        checkCompose: () =>
          create(CheckComposeResponseSchema, {
            variables: [{ name: "DB_PASSWORD", required: true }],
            refusal: { message: "the Compose file uses DB_PASSWORD, with no value" },
          }),
      }),
    );
    const sheet = await screen.findByRole("dialog", { name: "New app" });
    await user.click(within(sheet).getByLabelText("Compose file"));
    // biome-ignore lint/suspicious/noTemplateCurlyInString: a Compose variable, as the file has it
    await user.paste("services:\n  a:\n    image: a:${DB_PASSWORD}\n");
    const go = await within(sheet).findByRole("button", { name: "Give DB_PASSWORD a value to review the plan" });
    await user.click(go);
    const field = within(sheet).getByLabelText(/DB_PASSWORD/);
    expect(scrolled).toHaveBeenCalled();
    expect(field).toHaveFocus();
    expect(field).toHaveAttribute("type", "password");
    const reveal = within(sheet).getByRole("button", { name: "Show the value" });
    expect(reveal).toHaveAttribute("aria-pressed", "false");
    await user.click(reveal);
    expect(field).toHaveAttribute("type", "text");
    expect(reveal).toHaveAttribute("aria-pressed", "true");
    await expectAccessible(document.body);
  });

  it("says what it would refuse before it asks for a secret", async () => {
    const user = userEvent.setup();
    renderAt(
      "/apps/new?source=compose",
      compose({
        checkCompose: () =>
          create(CheckComposeResponseSchema, {
            variables: [{ name: "DB_PASSWORD", required: true }],
            refusal: {
              message: "the service a has network_mode host, which Dockwell can't run yet",
              line: 4,
              endLine: 4,
            },
          }),
      }),
    );
    const sheet = await screen.findByRole("dialog", { name: "New app" });
    await user.click(within(sheet).getByLabelText("Compose file"));
    // biome-ignore lint/suspicious/noTemplateCurlyInString: a Compose variable, as the file has it
    await user.paste("services:\n  a:\n    image: a:${DB_PASSWORD}\n    network_mode: host\n");
    expect(await within(sheet).findByText(/Line 4: the service a has network_mode host/)).toBeVisible();
    expect(within(sheet).getByRole("button", { name: "Fix line 4 to review the plan" })).toBeVisible();
    expect(within(sheet).queryByRole("button", { name: /Give DB_PASSWORD/ })).not.toBeInTheDocument();
  });

  it("marks both lines when a parser can only say near which", async () => {
    const user = userEvent.setup();
    const { container } = renderAt(
      "/apps/new?source=compose",
      compose({
        checkCompose: () =>
          create(CheckComposeResponseSchema, {
            refusal: { message: "near lines 3\u20134: check for a missing colon on the first", line: 3, endLine: 4 },
          }),
      }),
    );
    const sheet = await screen.findByRole("dialog", { name: "New app" });
    await user.click(within(sheet).getByLabelText("Compose file"));
    await user.paste("services:\n  a:\n    image a\n    user: x\n");
    expect(await within(sheet).findByText(/Near lines 3\u20134: check for a missing colon on the first/)).toBeVisible();
    expect(container.ownerDocument.querySelector(".dw-code-mark")).toHaveStyle({
      "--dw-code-line": "2",
      "--dw-code-span": "2",
    });
  });

  it("names the project from the file, which you can change, and sends it", async () => {
    const user = userEvent.setup();
    const imports: ImportComposeRequest[] = [];
    renderAt("/apps/new?source=compose", {
      ...compose({
        checkCompose: () => create(CheckComposeResponseSchema, { name: "my_shop", services: [web] }),
        importCompose: (req) => {
          imports.push(req);
          return { change: planned() };
        },
      }),
      changes: { watchChange: feed<WatchChangeResponse>().stream, discardChange: () => ({}) },
    });
    const sheet = await screen.findByRole("dialog", { name: "New app" });
    await user.click(within(sheet).getByLabelText("Compose file"));
    await user.paste("name: my_shop\nservices:\n  web:\n    image: a\n");
    const name = within(sheet).getByLabelText(/^Project name/);
    // A name that wouldn't do is made into one that does.
    await waitFor(() => expect(name).toHaveValue("my-shop"));
    await waitFor(() => expect(within(sheet).getByRole("button", { name: "Review plan" })).toBeEnabled());
    await user.clear(name);
    await user.type(name, "storefront");
    await waitFor(() => expect(within(sheet).getByRole("button", { name: "Review plan" })).toBeEnabled());
    await user.click(within(sheet).getByRole("button", { name: "Review plan" }));
    await screen.findByRole("dialog", { name: "Review plan" });
    expect(imports[0]).toMatchObject({ project: "storefront", summary: "Run storefront from a Compose file" });
  });

  it("needs a name once other apps are here, as it could change what's there", async () => {
    const user = userEvent.setup();
    renderAt("/apps/new?source=compose", {
      ...compose({ checkCompose: () => create(CheckComposeResponseSchema, { services: [web] }) }),
      services: { listServices: () => ({ services: [anApp("old", "old:1")] }), listServiceRevisions: () => ({}) },
    });
    const sheet = await screen.findByRole("dialog", { name: "New app" });
    await user.click(within(sheet).getByLabelText("Compose file"));
    await user.paste("services:\n  web:\n    image: a\n");
    const review = within(sheet).getByRole("button", { name: "Review plan" });
    await within(sheet).findByRole("region", { name: "What this file makes" });
    await within(sheet).findByText("Name it to review the plan");
    expect(review).toBeDisabled();
    await user.type(within(sheet).getByLabelText(/^Project name/), "Not A Label");
    expect(within(sheet).getByLabelText(/^Project name/)).toHaveAccessibleDescription(/Use lowercase letters/);
    expect(review).toBeDisabled();
    await user.clear(within(sheet).getByLabelText(/^Project name/));
    await user.type(within(sheet).getByLabelText(/^Project name/), "shop");
    await waitFor(() => expect(review).toBeEnabled());
  });

  it("lets a file go in the default project with no name while that has no apps", async () => {
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
      changes: { watchChange: feed<WatchChangeResponse>().stream, discardChange: () => ({}) },
    });
    const sheet = await screen.findByRole("dialog", { name: "New app" });
    await user.click(within(sheet).getByLabelText("Compose file"));
    await user.paste("services:\n  web:\n    image: a\n");
    await waitFor(() => expect(within(sheet).getByRole("button", { name: "Review plan" })).toBeEnabled());
    await user.click(within(sheet).getByRole("button", { name: "Review plan" }));
    await screen.findByRole("dialog", { name: "Review plan" });
    expect(imports[0]).toMatchObject({ project: "", summary: "Run a Compose file" });
  });

  it("puts the API's word that a file needs a project at the name", async () => {
    const user = userEvent.setup();
    renderAt("/apps/new?source=compose", {
      ...compose({
        checkCompose: () => create(CheckComposeResponseSchema, { services: [web] }),
        importCompose: () => {
          const err = new ConnectError("the Compose file names no project", Code.InvalidArgument);
          err.metadata.set("Dockwell-Error", "needs_project");
          throw err;
        },
      }),
    });
    const sheet = await screen.findByRole("dialog", { name: "New app" });
    await user.click(within(sheet).getByLabelText("Compose file"));
    await user.paste("services:\n  web:\n    image: a\n");
    await waitFor(() => expect(within(sheet).getByRole("button", { name: "Review plan" })).toBeEnabled());
    await user.click(within(sheet).getByRole("button", { name: "Review plan" }));
    await waitFor(() =>
      expect(within(sheet).getByLabelText(/^Project name/)).toHaveAccessibleDescription(
        /The Compose file names no project/,
      ),
    );
  });

  it("says why a file was refused as it planned, without the ID of where it would go", async () => {
    const user = userEvent.setup();
    renderAt("/apps/new?source=compose", {
      ...compose({
        checkCompose: () => create(CheckComposeResponseSchema, { services: [web] }),
        importCompose: () => {
          throw new ConnectError(
            "can't plan on hz-1: can't plan service web in env_01M6RD1VW6ECB3G1B1RVTCD8JD: the image a:1 isn't on this server",
            Code.FailedPrecondition,
          );
        },
      }),
    });
    const sheet = await screen.findByRole("dialog", { name: "New app" });
    await user.click(within(sheet).getByLabelText("Compose file"));
    await user.paste("services:\n  web:\n    image: a:1\n");
    await waitFor(() => expect(within(sheet).getByRole("button", { name: "Review plan" })).toBeEnabled());
    await user.click(within(sheet).getByRole("button", { name: "Review plan" }));
    await within(sheet).findByText(/the image a:1 isn't on this server/);
    expect(sheet).not.toHaveTextContent("env_");
  });

  it("says when a name is here already, so the plan updates it", async () => {
    const user = userEvent.setup();
    renderAt("/apps/new?source=compose", {
      ...compose({ checkCompose: () => create(CheckComposeResponseSchema, { name: "shop", services: [web] }) }),
      resources: projectPlaces("shop"),
    });
    const sheet = await screen.findByRole("dialog", { name: "New app" });
    await user.click(within(sheet).getByLabelText("Compose file"));
    await user.paste("name: shop\nservices:\n  web:\n    image: a\n");
    await waitFor(() =>
      expect(within(sheet).getByLabelText(/^Project name/)).toHaveAccessibleDescription(/shop is here already/),
    );
  });

  it("says when a file writes a variable's value where anyone can read it, before there is a value", async () => {
    const user = userEvent.setup();
    renderAt("/apps/new?source=compose", {
      ...compose({
        checkCompose: () =>
          create(CheckComposeResponseSchema, {
            variables: [{ name: "REDIS_PASSWORD", required: true }],
            exposedVariables: [
              { name: "REDIS_PASSWORD", resource: { kind: "service", name: "shop" }, field: "command" },
              { name: "API_KEY", resource: { kind: "service", name: "shop" }, field: "command" },
              { name: "OWNER", resource: { kind: "service", name: "shop" }, field: "labels" },
              { name: "PREFIX", resource: { kind: "service", name: "shop" }, field: "environment" },
              { name: "MTU", resource: { kind: "network", name: "back" }, field: "driver_opts" },
            ],
            moreExposed: true,
            refusal: { message: "the Compose file uses REDIS_PASSWORD, with no value" },
          }),
      }),
    });
    const sheet = await screen.findByRole("dialog", { name: "New app" });
    await user.click(within(sheet).getByLabelText("Compose file"));
    // biome-ignore lint/suspicious/noTemplateCurlyInString: a Compose variable, as the file has it
    await user.paste('services:\n  shop:\n    image: a\n    command: ["${REDIS_PASSWORD}"]\n');
    // Two in one field say so once, and how to hide one depends on the field.
    const command = await within(sheet).findByText(/shop's command/);
    expect(command).toHaveTextContent(
      "Anyone who can read shop's command can see the value of REDIS_PASSWORD, API_KEY.",
    );
    expect(command).toHaveTextContent("Write the command as a list to keep it hidden.");
    const labels = within(sheet).getByText(/shop's labels/);
    expect(labels).toHaveTextContent("Don't pass a secret there.");
    const names = within(sheet).getByText(/shop's environment variable names/);
    expect(names).toHaveTextContent("Don't pass a secret there.");
    expect(within(sheet).getByText(/the network back's driver options/)).toHaveTextContent("MTU");
    expect(within(sheet).getByText(/more variables in plain sight/)).toBeVisible();
    // No false promise that a value isn't saved.
    expect(sheet).not.toHaveTextContent("aren't saved");
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
    const tag = await within(sheet).findByLabelText(/^TAG/);
    // It says which it needs.
    expect(within(sheet).getByText("Required")).toBeVisible();
    expect(within(sheet).getByText("Give TAG a value to review the plan")).toBeVisible();
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
    // Into the project it would change, on purpose.
    await user.type(within(sheet).getByLabelText(/^Project name/), "default");
    const remove = await within(sheet).findByRole("switch", { name: /Remove what isn't in this file/ });
    // How many, and the names only when asked: there may be seventy.
    expect(sheet).toHaveTextContent("1 other app is here.");
    expect(within(sheet).queryByText("old")).not.toBeInTheDocument();
    await user.click(within(sheet).getByRole("button", { name: "Show it" }));
    expect(within(sheet).getByText("old")).toBeVisible();
    await user.click(remove);
    await waitFor(() => expect(within(sheet).getByRole("button", { name: "Review plan" })).toBeEnabled());
    await user.click(within(sheet).getByRole("button", { name: "Review plan" }));
    await screen.findByRole("dialog", { name: "Review plan" });
    expect(imports[0]?.removeOrphans).toBe(true);
  });
});

describe("what a Compose file reads beside it", () => {
  const SECRET = "hunter2-not-for-the-page";
  const checking = (asked: { env: string; directory: string }[], refusal?: { message: string; line?: number }) => ({
    checkCompose: (req: { env: string; directory: string }) => {
      asked.push({ env: req.env, directory: req.directory });
      return create(CheckComposeResponseSchema, {
        ...(refusal && req.env !== "OK=1" && req.directory !== "/opt/shop" ? { refusal: { line: 0, ...refusal } } : {}),
        services: [anApp("db", "postgres:17")],
      });
    },
  });
  const typed = async (user: ReturnType<typeof userEvent.setup>, sheet: HTMLElement) => {
    await user.click(within(sheet).getByLabelText("Compose file"));
    await user.paste("services:\n  db:\n    image: postgres:17\n");
  };

  it("sends an .env and a directory to check and to plan, keeps neither, and hides the values", async () => {
    const user = userEvent.setup();
    const asked: { env: string; directory: string }[] = [];
    const imports: ImportComposeRequest[] = [];
    const set = vi.spyOn(Storage.prototype, "setItem");
    const { router } = renderAt("/apps/new?source=compose", {
      ...api({
        compose: {
          ...checking(asked),
          importCompose: (req) => {
            imports.push(req);
            return { change: planned({ summary: "Run a Compose file" }) };
          },
        },
      }),
      changes: { watchChange: feed<WatchChangeResponse>().stream, discardChange: () => ({}) },
    });
    const sheet = await screen.findByRole("dialog", { name: "New app" });
    await typed(user, sheet);
    await user.click(within(sheet).getByRole("button", { name: "Add an .env file" }));
    const env = within(sheet).getByLabelText(".env file");
    expect(env.closest(".app-env-hidden")).not.toBeNull();
    await user.click(env);
    await user.paste(`POSTGRES_PASSWORD=${SECRET}`);
    await user.click(within(sheet).getByRole("button", { name: "Say which directory it's in" }));
    await user.type(within(sheet).getByLabelText(/Directory on the server/), "/opt/shop");
    await waitFor(() => expect(asked.at(-1)).toEqual({ env: `POSTGRES_PASSWORD=${SECRET}`, directory: "/opt/shop" }));
    // A person can check what they pasted.
    await user.click(within(sheet).getByRole("button", { name: "Show" }));
    expect(env.closest(".app-env-hidden")).toBeNull();
    const review = within(sheet).getByRole("button", { name: "Review plan" });
    await waitFor(() => expect(review).toBeEnabled());
    await user.click(review);
    await screen.findByRole("dialog", { name: "Review plan" });
    expect(imports[0]?.env).toBe(`POSTGRES_PASSWORD=${SECRET}`);
    expect(imports[0]?.directory).toBe("/opt/shop");
    // Nowhere that outlives this sheet.
    expect(router.state.location.href).not.toContain(SECRET);
    expect(window.location.href).not.toContain(SECRET);
    for (const [, value] of set.mock.calls) expect(String(value)).not.toContain(SECRET);
    set.mockRestore();
  });

  it("puts what's wrong with an .env on the .env, at its line, and never its value", async () => {
    const user = userEvent.setup();
    const asked: { env: string; directory: string }[] = [];
    renderAt("/apps/new?source=compose", {
      ...api({
        compose: checking(asked, {
          message: "line 2 of the .env file has a $ before a name with no value. To write a $ of your own, use $$",
        }),
      }),
    });
    const sheet = await screen.findByRole("dialog", { name: "New app" });
    await typed(user, sheet);
    await user.click(await within(sheet).findByLabelText(".env file"));
    await user.paste(`A=1\nB=${SECRET}$oops`);
    const env = within(sheet).getByLabelText(".env file");
    await waitFor(() => expect(env).toHaveAttribute("aria-invalid", "true"));
    expect(sheet).toHaveTextContent("Line 2 of the .env file has a $ before a name with no value.");
    // Only what was pasted holds it, and that is the field's own.
    const beyond = sheet.cloneNode(true) as HTMLElement;
    for (const field of beyond.querySelectorAll("textarea")) field.remove();
    expect(beyond).not.toHaveTextContent(SECRET);
    // It isn't the Compose file that's wrong.
    expect(within(sheet).getByLabelText("Compose file")).not.toHaveAttribute("aria-invalid", "true");
    expect(within(sheet).getByRole("button", { name: "Review plan" })).toBeDisabled();
    await waitFor(() => expect(sheet).toHaveTextContent("Fix the .env file to review the plan"));
  });

  it("opens the directory where a relative path needs it, and puts what's wrong there", async () => {
    const user = userEvent.setup();
    const asked: { env: string; directory: string }[] = [];
    renderAt("/apps/new?source=compose", {
      ...api({
        compose: checking(asked, {
          message:
            "the service db mounts ./data, a path relative to the Compose file, and Dockwell doesn't know where the file is on the server. Pass the directory it is in, as directory (--directory on the CLI), or use an absolute path, such as /srv/db/data, or a named volume",
          line: 4,
        }),
      }),
    });
    const sheet = await screen.findByRole("dialog", { name: "New app" });
    await typed(user, sheet);
    const directory = await within(sheet).findByLabelText(/Directory on the server/);
    await waitFor(() => expect(directory).toHaveAttribute("aria-invalid", "true"));
    expect(within(sheet).getByLabelText("Compose file")).not.toHaveAttribute("aria-invalid", "true");
    await waitFor(() => expect(sheet).toHaveTextContent("Say where the file is to review the plan"));
    await user.type(directory, "/opt/shop");
    await waitFor(() => expect(within(sheet).getByRole("button", { name: "Review plan" })).toBeEnabled());
  });

  it("puts a directory that won't do on the directory", async () => {
    const user = userEvent.setup();
    renderAt("/apps/new?source=compose", {
      ...api({
        compose: {
          checkCompose: (req) =>
            create(CheckComposeResponseSchema, {
              ...(req.directory === "opt/shop"
                ? {
                    refusal: {
                      message:
                        'the directory "opt/shop" isn\'t an absolute path, and a server has nowhere else to start from. Give the one the Compose file is in, such as /opt/shop',
                    },
                  }
                : {}),
              services: [anApp("db", "postgres:17")],
            }),
        },
      }),
    });
    const sheet = await screen.findByRole("dialog", { name: "New app" });
    await typed(user, sheet);
    await user.click(await within(sheet).findByRole("button", { name: "Say which directory it's in" }));
    const directory = within(sheet).getByLabelText(/Directory on the server/);
    await user.type(directory, "opt/shop");
    await waitFor(() => expect(directory).toHaveAttribute("aria-invalid", "true"));
    expect(sheet).toHaveTextContent("isn't an absolute path");
  });
});

describe("New app, what it picks and what waits under Advanced", () => {
  const withHolder = (more: Api = {}) =>
    api({
      services: {
        listServices: () => ({
          services: [anApp("cache", "redis:7.4", { ports: [{ hostPort: 8080, containerPort: 6379 }] })],
        }),
        listServiceRevisions: () => ({}),
      },
      ...more,
    });

  it("takes a free port on the server, and says what holds one that's typed", async () => {
    const user = userEvent.setup();
    const { sheet } = await openImage(withHolder());
    await user.type(within(sheet).getByLabelText("Image"), "nginx:1.27");
    await user.click(within(sheet).getByRole("button", { name: "Add port" }));
    await user.type(within(sheet).getByLabelText("In the app"), "80");
    const host = within(sheet).getByLabelText<HTMLInputElement>("On the server");
    // cache publishes 8080, so the first free port is 8081.
    await waitFor(() => expect(host.value).toBe("8081"));
    expect(host).toHaveAccessibleDescription(/Free/);
    await user.clear(host);
    await user.type(host, "8080");
    expect(host).toHaveAttribute("aria-invalid", "true");
    expect(sheet).toHaveTextContent("cache uses it · Use 8081");
    await user.click(within(sheet).getByRole("button", { name: "Use 8081" }));
    expect(host.value).toBe("8081");

    // A second port doesn't take the first one's.
    await user.click(within(sheet).getByRole("button", { name: "Add port" }));
    const second = within(sheet).getAllByLabelText<HTMLInputElement>("On the server")[1];
    expect(second?.value).toBe("8082");
  });

  it("starts the rest at defaults it names, and plans what's changed, with the command that does the same", async () => {
    const user = userEvent.setup();
    const proposals: ProposeChangeRequest[] = [];
    const writeText = vi.spyOn(navigator.clipboard, "writeText").mockResolvedValue();
    const { sheet } = await openImage(
      withHolder({
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
    const advanced = within(sheet).getByRole("button", { name: "Advanced" });
    expect(advanced).toHaveAccessibleDescription(
      "The image's command · Restarts unless stopped · No limits · No health check",
    );
    await user.click(within(sheet).getByRole("button", { name: "Add port" }));
    await user.type(within(sheet).getByLabelText("In the app"), "80");
    await user.click(advanced);
    await user.type(within(sheet).getByLabelText("Memory limit"), "512");
    await user.type(within(sheet).getByLabelText("Health check path"), "/healthz");
    await user.selectOptions(within(sheet).getByLabelText("Restarts"), String(RestartPolicy.ON_FAILURE));

    // An amount that isn't one is said at its field, with Advanced open.
    await user.click(within(sheet).getByRole("button", { name: "Review plan" }));
    expect(within(sheet).getByLabelText("Memory limit")).toHaveAccessibleDescription(/Use an amount from 6M/);
    expect(proposals).toEqual([]);
    // The refused field has focus, its text selected to type over.
    await waitFor(() => expect(within(sheet).getByLabelText("Memory limit")).toHaveFocus());
    await user.clear(within(sheet).getByLabelText("Memory limit"));
    await user.type(within(sheet).getByLabelText("Memory limit"), "512M");

    await user.click(within(sheet).getByRole("button", { name: "Copy as CLI command" }));
    expect(writeText).toHaveBeenCalledWith(
      `dockwell services create nginx --app-image nginx:1.27 --app-ports '{"hostPort":8081,"containerPort":80,"protocol":"PROTOCOL_TCP"}' --app-restart on-failure --app-healthcheck-http-port 80 --app-healthcheck-http-path /healthz --app-resources-memory 512M`,
    );

    // Closed again, it says what was changed.
    await user.click(within(sheet).getByRole("button", { name: /^Advanced/ }));
    expect(within(sheet).getByText("3 changed")).toBeVisible();

    await user.click(within(sheet).getByRole("button", { name: "Review plan" }));
    await screen.findByRole("dialog", { name: "Review plan" });
    const edit = proposals[0]?.edits[0];
    const app = edit?.spec?.kind.case === "service" ? edit.spec.kind.value.runs.value : undefined;
    expect(app).toMatchObject({
      restart: RestartPolicy.ON_FAILURE,
      resources: { memory: "512M", cpus: "" },
      healthcheck: { http: { port: 80, path: "/healthz" } },
    });
    expect(app?.command).toEqual([]);
  });

  it("takes variables from an .env file, in place of any of the same name", async () => {
    const user = userEvent.setup();
    const { sheet } = await openImage(api());
    await user.click(within(sheet).getByRole("button", { name: "Add variable" }));
    await user.type(within(sheet).getByLabelText("Name"), "A");
    await user.upload(
      sheet.querySelector<HTMLInputElement>('.app-section input[type="file"]') as HTMLInputElement,
      new File(["# comment\nA=one\nexport B='two'\n"], ".env", { type: "text/plain" }),
    );
    await waitFor(() => expect(within(sheet).getAllByLabelText("Value")).toHaveLength(2));
    expect(
      within(sheet)
        .getAllByLabelText<HTMLInputElement>(/^Name$/)
        .map((n) => n.value),
    ).toEqual(["A", "B"]);
    expect(
      within(sheet)
        .getAllByLabelText<HTMLInputElement>("Value")
        .map((v) => v.value),
    ).toEqual(["one", "two"]);
  });
});

describe("New app, its image picked and read", () => {
  const images = {
    searchImages: (req: { term: string }) =>
      req.term.startsWith("ngi")
        ? {
            answer: ImageAnswer.FOUND,
            matches: [{ name: "nginx", description: "Official build of Nginx.", official: true }],
          }
        : { answer: ImageAnswer.FOUND },
    listImageTags: () => ({
      answer: ImageAnswer.FOUND,
      tags: [
        { name: "1.28", digest: "sha256:28", latest: true },
        { name: "latest", digest: "sha256:28", latest: true },
        { name: "1.27", digest: "sha256:27" },
      ],
    }),
    getImage: (req: { reference: string }) =>
      req.reference.startsWith("nginx:1.2")
        ? {
            answer: ImageAnswer.FOUND,
            digest: "sha256:28",
            config: {
              ports: [{ port: 80 }],
              env: [
                { name: "NGINX_VERSION", value: "1.28.0" },
                { name: "NGINX_ENTRYPOINT_QUIET_LOGS", value: "" },
              ],
              volumes: ["/usr/share/nginx/html"],
            },
          }
        : { answer: ImageAnswer.NOT_FOUND },
  };

  it("searches, lists a name's versions newest first, says it's there, and fills in what it declares", async () => {
    const user = userEvent.setup();
    const { sheet } = await openImage(api({ images }));
    const field = within(sheet).getByRole("combobox", { name: "Image" });
    await user.type(field, "ngi");
    const found = await within(sheet).findByRole("option", { name: /nginx/ });
    expect(found).toHaveTextContent("Official");
    await user.click(found);
    expect(field).toHaveValue("nginx:");
    const versions = await within(sheet).findAllByRole("option");
    expect(versions.map((o) => o.textContent)).toEqual(["1.28latest", "latest", "1.27"]);
    await user.click(versions[0] as HTMLElement);
    expect(field).toHaveValue("nginx:1.28");
    await waitFor(() => expect(field).toHaveAccessibleDescription(/Found/));
    await expectAccessible(document.body);

    // Nobody typed a port: the image said 80, and a free one on the server takes it.
    expect(within(sheet).getByLabelText<HTMLInputElement>("In the app").value).toBe("80");
    await waitFor(() => expect(within(sheet).getByLabelText<HTMLInputElement>("On the server").value).toBe("8080"));
    expect(within(sheet).getByLabelText<HTMLInputElement>("Path in the app").value).toBe("/usr/share/nginx/html");
    expect(within(sheet).getByLabelText<HTMLInputElement>("Volume or server path").value).toBe("nginx-html");
    // What it sets for itself, such as its version, isn't offered; what it lets you set is, as it gives it.
    expect(
      within(sheet)
        .getAllByLabelText<HTMLInputElement>(/^Name$/)
        .map((n) => n.value),
    ).toEqual(["NGINX_ENTRYPOINT_QUIET_LOGS"]);

    // Typing stays possible, and says what the registry says.
    await user.clear(field);
    await user.type(field, "acme/missing:1");
    await waitFor(() => expect(field).toHaveAccessibleDescription(/Not found, or private/));
  });
});
