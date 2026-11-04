// SPDX-License-Identifier: AGPL-3.0-only

import { create } from "@bufbuild/protobuf";
import { Code, ConnectError } from "@connectrpc/connect";
import {
  type ApplyChangeRequest,
  ChangeState,
  HostAccessKind,
  HostAccessSchema,
  PlanAction,
  StepProgressSchema,
  StepState,
  type WatchChangeResponse,
  WatchChangeResponseSchema,
} from "@dockwell/api";
import { act, screen, waitFor, within } from "@testing-library/react";
import userEvent from "@testing-library/user-event";
import { StrictMode, useEffect } from "react";
import { afterEach, describe, expect, it, vi } from "vitest";

import { activateLocale } from "../../app/i18n";
import { expectAccessible } from "../../test/a11y";
import { applied, moved, shopPlanned, watched, webPlanned } from "../../test/changes";
import { type Api, feed, onPhone, renderWithApi } from "../../test/render";
import { ChangeSheetProvider, type Opening, useChangeSheet } from "./ChangeSheet";

function Opener({ opening }: { opening: Opening }) {
  const { open } = useChangeSheet();
  useEffect(() => open(opening), [open, opening]);
  return null;
}

function renderSheet(opening: Opening, api: Api) {
  return renderWithApi(
    <ChangeSheetProvider>
      <Opener opening={opening} />
    </ChangeSheetProvider>,
    api,
  );
}

const proposeWeb: Opening = {
  kind: "propose",
  summary: "Update web",
  edits: [{ resource: { name: "web" }, spec: { kind: { case: "container", value: { image: "nginx:1.28" } } } }],
};

afterEach(() => {
  vi.unstubAllGlobals();
});

describe("the plan sheet", () => {
  it("reviews a plan, applies it, and follows its steps inline", async () => {
    const user = userEvent.setup();
    const watch = feed<WatchChangeResponse>();
    const applies: ApplyChangeRequest[] = [];
    const discard = vi.fn(() => ({}));
    const planned = webPlanned();
    renderSheet(proposeWeb, {
      changes: {
        proposeChange: () => ({ change: planned }),
        watchChange: watch.stream,
        applyChange: (req) => {
          applies.push(req);
          return { change: moved(planned, { state: ChangeState.APPLYING }) };
        },
        discardChange: discard,
      },
    });

    const sheet = await screen.findByRole("dialog", { name: "Review plan" });
    expect(sheet).toHaveAccessibleDescription("Update web");
    await within(sheet).findByText("to replace");
    expect(sheet).toHaveTextContent("1 to replace");
    // A replace counts on its own, and the secret says only that it changed.
    expect(sheet).toHaveTextContent("0 to change");
    expect(within(sheet).getAllByText("hidden")).toHaveLength(2);
    expect(sheet).toHaveTextContent("hz-1 · about 2 s down");
    const steps = within(sheet).getByRole("region", { name: "Steps by server" });
    expect(steps).toHaveTextContent("hz-1");
    expect(steps).toHaveTextContent("Replace web");
    expect(steps).toHaveTextContent("Container");
    expect(sheet).toHaveTextContent("Undo for 7 days");
    await expectAccessible(document.body);

    act(() => watch.push(watched(planned)));
    await user.click(within(sheet).getByRole("button", { name: /Apply plan/ }));
    await waitFor(() => expect(applies).toHaveLength(1));
    expect(applies[0]?.fingerprint).toBe("a1b2c3d4e5f60718293a4b5c6d7e8f90");

    const running = moved(planned, {
      state: ChangeState.APPLYING,
      steps: [
        create(StepProgressSchema, {
          serverId: "srv_1",
          resourceId: "res_web",
          state: StepState.QUEUED,
          message: "Waiting to replace web on hz-1",
        }),
      ],
    });
    act(() => watch.push(watched(running)));
    await screen.findByText("Waiting to replace web on hz-1");
    act(() =>
      watch.push(
        create(WatchChangeResponseSchema, {
          event: {
            case: "step",
            value: {
              index: 0,
              step: {
                serverId: "srv_1",
                resourceId: "res_web",
                state: StepState.RUNNING,
                message: "Replacing web on hz-1",
              },
            },
          },
        }),
      ),
    );
    const applying = await screen.findByRole("dialog", { name: "Applying" });
    expect(await within(applying).findByText("Replacing web on hz-1")).toBeVisible();
    expect(within(applying).getByText("Running")).toBeVisible();
    expect(applying).toHaveTextContent("It keeps applying if you close this.");

    act(() => watch.push(watched(applied(running))));
    const done = await screen.findByRole("dialog", { name: "Applied" });
    expect(done).toHaveTextContent("Every step ran");
    expect(done).toHaveTextContent(/You can undo it until/);
    expect(within(done).getByRole("button", { name: "Undo" })).toBeVisible();
    await expectAccessible(document.body);
    // An applied plan stays, sheet or no sheet.
    await user.click(within(done).getByRole("button", { name: "Done" }));
    await new Promise((resolve) => setTimeout(resolve, 20));
    expect(discard).not.toHaveBeenCalled();
  });

  it("asks for the start of the fingerprint before applying a destructive plan", async () => {
    const user = userEvent.setup();
    const applies: ApplyChangeRequest[] = [];
    const destructive = webPlanned();
    if (destructive.plan) destructive.plan.destructive = true;
    renderSheet(proposeWeb, {
      changes: {
        proposeChange: () => ({ change: destructive }),
        watchChange: feed<WatchChangeResponse>().stream,
        applyChange: (req) => {
          applies.push(req);
          return { change: destructive };
        },
      },
    });

    const sheet = await screen.findByRole("dialog", { name: "Review plan" });
    const apply = await within(sheet).findByRole("button", { name: /Apply plan/ });
    expect(sheet).toHaveTextContent("This plan destroys data or may cut off access");
    expect(apply).toBeDisabled();
    const field = within(sheet).getByLabelText("Type a1b2c3d4 to apply it");
    await user.type(field, "a1b2c3d5");
    expect(apply).toBeDisabled();
    await user.clear(field);
    await user.type(field, "A1B2C3D4");
    expect(apply).toBeEnabled();
    await expectAccessible(document.body);
    await user.click(apply);
    await waitFor(() => expect(applies[0]?.fingerprint).toBe("a1b2c3d4e5f60718293a4b5c6d7e8f90"));
  });

  it("asks for the fingerprint before giving a container more of its server, and says what it reaches", async () => {
    const user = userEvent.setup();
    const applies: ApplyChangeRequest[] = [];
    const reaching = webPlanned();
    const plan = reaching.plan;
    const step = plan?.servers[0]?.steps[0];
    if (!plan || !step) throw new Error("webPlanned has no step");
    plan.hostAccess = true;
    step.hostAccess = [
      create(HostAccessSchema, { kind: HostAccessKind.SERVER_PATH, path: "/srv/app/config", readOnly: true }),
      create(HostAccessSchema, { kind: HostAccessKind.DOCKER_SOCKET, path: "/var/run/docker.sock" }),
    ];
    step.untilHealthy = { $typeName: "google.protobuf.Duration", seconds: 110n, nanos: 0 };
    step.keptPaths = ["/var/lib/postgresql/data"];
    step.destroysData = true;
    renderSheet(proposeWeb, {
      changes: {
        proposeChange: () => ({ change: reaching }),
        watchChange: feed<WatchChangeResponse>().stream,
        applyChange: (req) => {
          applies.push(req);
          return { change: reaching };
        },
      },
    });

    const sheet = await screen.findByRole("dialog", { name: "Review plan" });
    expect(sheet).toHaveTextContent("This plan gives a container more access to its server");
    expect(sheet).toHaveTextContent(
      "Reaches the server path /srv/app/config (read-only); Docker's socket at /var/run/docker.sock, which gives control of Docker on the server, the same as root on hz-1",
    );
    expect(sheet).toHaveTextContent("Up to 1 min 50 s until healthy");
    expect(sheet).toHaveTextContent(
      "Takes out of use what its image keeps at /var/lib/postgresql/data, which the server keeps for 7 days",
    );
    const apply = await within(sheet).findByRole("button", { name: /Apply plan/ });
    expect(apply).toBeDisabled();
    await user.type(within(sheet).getByLabelText("Type a1b2c3d4 to apply it"), "a1b2c3d4");
    expect(apply).toBeEnabled();
    await expectAccessible(document.body);
    await user.click(apply);
    await waitFor(() => expect(applies[0]?.fingerprint).toBe("a1b2c3d4e5f60718293a4b5c6d7e8f90"));
  });

  it("discards the plan it made when you cancel", async () => {
    const user = userEvent.setup();
    const discarded: string[] = [];
    renderSheet(
      { kind: "propose", summary: "Create the project shop", edits: [] },
      {
        changes: {
          proposeChange: () => ({ change: shopPlanned() }),
          watchChange: feed<WatchChangeResponse>().stream,
          discardChange: (req) => {
            discarded.push(req.id);
            return {};
          },
        },
      },
    );
    const sheet = await screen.findByRole("dialog", { name: "Review plan" });
    await within(sheet).findByText("to create");
    expect(sheet).toHaveTextContent("2 to create");
    // Projects run on no server: there are no steps to show, and nothing goes down.
    expect(within(sheet).queryByRole("region", { name: "Steps by server" })).toBeNull();
    expect(sheet).toHaveTextContent("No server · nothing goes down");
    await user.click(within(sheet).getByRole("button", { name: "Cancel" }));
    await waitFor(() => expect(discarded).toEqual(["chg_shop"]));
    expect(screen.queryByRole("dialog")).toBeNull();
  });

  it("discards a plan that arrives after you close the sheet", async () => {
    const user = userEvent.setup();
    const discarded: string[] = [];
    let made: (value: { change: ReturnType<typeof shopPlanned> }) => void = () => {};
    renderSheet(
      { kind: "propose", summary: "Create the project shop", edits: [] },
      {
        changes: {
          proposeChange: () => new Promise((resolve) => (made = resolve)),
          watchChange: feed<WatchChangeResponse>().stream,
          discardChange: (req) => {
            discarded.push(req.id);
            return {};
          },
        },
      },
    );
    const sheet = await screen.findByRole("dialog", { name: "Planning" });
    await user.click(within(sheet).getAllByRole("button", { name: "Close" }).at(-1) as HTMLElement);
    expect(screen.queryByRole("dialog")).toBeNull();
    // The control plane finishes planning after all.
    act(() => made({ change: shopPlanned() }));
    await waitFor(() => expect(discarded).toEqual(["chg_shop"]));
  });

  it("plans once in StrictMode, which mounts the sheet twice, and discards that one plan", async () => {
    const user = userEvent.setup();
    const proposals = vi.fn(() => ({ change: shopPlanned() }));
    const discarded: string[] = [];
    renderWithApi(
      <StrictMode>
        <ChangeSheetProvider>
          <Opener opening={{ kind: "propose", summary: "Create the project shop", edits: [] }} />
        </ChangeSheetProvider>
      </StrictMode>,
      {
        changes: {
          proposeChange: proposals,
          watchChange: feed<WatchChangeResponse>().stream,
          discardChange: (req) => {
            discarded.push(req.id);
            return {};
          },
        },
      },
    );
    const sheet = await screen.findByRole("dialog", { name: "Review plan" });
    await within(sheet).findByText("to create");
    expect(proposals).toHaveBeenCalledTimes(1);
    expect(discarded).toEqual([]);
    await user.click(within(sheet).getByRole("button", { name: "Cancel" }));
    await waitFor(() => expect(discarded).toEqual(["chg_shop"]));
  });

  it("discards the plan it made when another sheet opens in its place", async () => {
    const discarded: string[] = [];
    let api: ReturnType<typeof useChangeSheet> | undefined;
    function Handle() {
      api = useChangeSheet();
      return null;
    }
    renderWithApi(
      <ChangeSheetProvider>
        <Handle />
      </ChangeSheetProvider>,
      {
        changes: {
          proposeChange: () => ({ change: shopPlanned() }),
          getChange: () => ({ change: applied(webPlanned()) }),
          watchChange: feed<WatchChangeResponse>().stream,
          discardChange: (req) => {
            discarded.push(req.id);
            return {};
          },
        },
      },
    );
    act(() => api?.open({ kind: "propose", summary: "Create the project shop", edits: [] }));
    await within(await screen.findByRole("dialog", { name: "Review plan" })).findByText("to create");
    // The command bar opens a recent change's plan over it.
    act(() => api?.open({ kind: "change", id: "chg_web" }));
    expect(await screen.findByRole("dialog", { name: "Applied" })).toBeVisible();
    await waitFor(() => expect(discarded).toEqual(["chg_shop"]));
  });

  it("counts only the resources that change, as the plan's summary does", async () => {
    const planned = shopPlanned();
    const [project] = planned.plan?.resources ?? [];
    if (project) project.action = PlanAction.NONE;
    if (planned.plan?.summary) planned.plan.summary.create = 1;
    renderSheet(
      { kind: "change", id: "chg_shop" },
      { changes: { getChange: () => ({ change: planned }), watchChange: feed<WatchChangeResponse>().stream } },
    );
    const sheet = await screen.findByRole("dialog", { name: "Review plan" });
    await within(sheet).findByText("to create");
    expect(sheet).toHaveTextContent("1 to create");
    expect(sheet).toHaveTextContent("0 to change");
    expect(within(sheet).queryByText("shop")).toBeNull();
    expect(within(sheet).getByText("production")).toBeVisible();
  });

  it("leaves a plan someone else made when you close it", async () => {
    const user = userEvent.setup();
    const discard = vi.fn();
    renderSheet(
      { kind: "change", id: "chg_shop" },
      {
        changes: {
          getChange: () => ({ change: shopPlanned() }),
          watchChange: feed<WatchChangeResponse>().stream,
          discardChange: discard,
        },
      },
    );
    const sheet = await screen.findByRole("dialog", { name: "Review plan" });
    await within(sheet).findByText("2");
    await user.click(within(sheet).getByRole("button", { name: "Close" }));
    expect(screen.queryByRole("dialog")).toBeNull();
    expect(discard).not.toHaveBeenCalled();
  });

  it("says why a plan couldn't be made", async () => {
    renderSheet(proposeWeb, {
      changes: {
        proposeChange: () => {
          throw new ConnectError(
            "there are two projects, default and shop. Name the one the environment is for",
            Code.InvalidArgument,
          );
        },
      },
    });
    const sheet = await screen.findByRole("dialog", { name: "The plan didn't work" });
    expect(await within(sheet).findByRole("alert")).toHaveTextContent(
      "There are two projects, default and shop. Name the one the environment is for.",
    );
  });

  it("fills a phone's screen, and reads in Chinese", async () => {
    onPhone();
    await activateLocale("zh-CN");
    renderSheet(proposeWeb, {
      changes: { proposeChange: () => ({ change: webPlanned() }), watchChange: feed<WatchChangeResponse>().stream },
    });
    const sheet = await screen.findByRole("dialog", { name: "审查计划" });
    expect(sheet).toHaveClass("is-phone");
    expect(within(sheet).getByRole("button", { name: /应用计划/ })).toBeVisible();
    await expectAccessible(document.body);
  });
});
