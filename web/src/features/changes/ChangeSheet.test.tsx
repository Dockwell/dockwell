// SPDX-License-Identifier: AGPL-3.0-only

import { create } from "@bufbuild/protobuf";
import { Code, ConnectError } from "@connectrpc/connect";
import {
  type ApplyChangeRequest,
  ChangeState,
  StepProgressSchema,
  StepState,
  type WatchChangeResponse,
  WatchChangeResponseSchema,
} from "@dockwell/api";
import { act, screen, waitFor, within } from "@testing-library/react";
import userEvent from "@testing-library/user-event";
import { useEffect } from "react";
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
    const planned = webPlanned();
    renderSheet(proposeWeb, {
      changes: {
        proposeChange: () => ({ change: planned }),
        watchChange: watch.stream,
        applyChange: (req) => {
          applies.push(req);
          return { change: moved(planned, { state: ChangeState.APPLYING }) };
        },
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
