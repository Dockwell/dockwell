// SPDX-License-Identifier: AGPL-3.0-only

import { create, type MessageInitShape } from "@bufbuild/protobuf";
import { durationFromMs, timestampFromDate } from "@bufbuild/protobuf/wkt";
import {
  type ApplyChangeRequest,
  BackupMethod,
  type Change,
  ChangeSchema,
  ChangeState,
  IntentKind,
  IntentSchema,
  PlanAction,
  ServiceInfoSchema,
  StepPhase,
  StepProgressSchema,
  StepState,
  Surface,
  type WatchChangeResponse,
  WatchChangeResponseSchema,
} from "@dockwell/api";
import { act, screen, waitFor, within } from "@testing-library/react";
import userEvent from "@testing-library/user-event";
import { useEffect } from "react";
import { describe, expect, it } from "vitest";

import { expectAccessible } from "../../test/a11y";
import { applied, mira, moved, watched } from "../../test/changes";
import { type Api, feed, renderWithApi } from "../../test/render";
import { UpgradeProgress } from "../apps/upgrade";
import { ChangeSheetProvider, type Opening, useChangeSheet } from "./ChangeSheet";
import { tagOf, upgradePhases } from "./upgrade";

/** A planned update of web from 1.4 to 1.5, with its volume data backed up first. */
function updatePlanned(): Change {
  return create(ChangeSchema, {
    id: "chg_up",
    summary: "Update web to ghcr.io/acme/shop:1.5",
    state: ChangeState.PLANNED,
    proposedBy: mira,
    proposedVia: Surface.PANEL,
    createTime: timestampFromDate(new Date()),
    plan: {
      fingerprint: "a1b2c3d4e5f60718293a4b5c6d7e8f90",
      summary: { replace: 1 },
      undoWindow: durationFromMs(7 * 86_400_000),
      intent: {
        kind: IntentKind.UPGRADE,
        resourceId: "svc_web",
        image: "ghcr.io/acme/shop:1.5",
        fromImage: "ghcr.io/acme/shop:1.4",
        fromDigest: "sha256:aaaaaaaaaaaabbbbbbbbbbbbccccccccccccddddddddddddeeeeeeeeeeeeffff",
      },
      resources: [
        { id: "vol_data", resource: { kind: "volume", scope: "env_1", name: "data" }, action: PlanAction.UPDATE },
        {
          id: "svc_web",
          resource: { kind: "service", scope: "env_1", name: "web" },
          action: PlanAction.REPLACE,
          servers: ["srv_1"],
          previousServers: ["srv_1"],
        },
      ],
      servers: [
        {
          serverId: "srv_1",
          serverName: "hz-1",
          downtime: durationFromMs(2_000),
          steps: [
            {
              resourceId: "vol_data",
              resource: { kind: "volume", scope: "env_1", name: "data" },
              action: PlanAction.UPDATE,
              backupFirst: { method: BackupMethod.COPY, stops: ["web"] },
            },
            {
              resourceId: "svc_web",
              resource: { kind: "service", scope: "env_1", name: "web" },
              action: PlanAction.REPLACE,
              downtime: durationFromMs(2_000),
            },
          ],
        },
      ],
    },
  });
}

function step(
  resourceId: string,
  state: StepState,
  more: Omit<MessageInitShape<typeof StepProgressSchema>, "$typeName"> = {},
) {
  return create(StepProgressSchema, { ...more, serverId: "srv_1", resourceId, state });
}

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

describe("an update's phases", () => {
  it("come from its steps: a backup first, then the pull, the start and the health check", () => {
    const planned = updatePlanned();
    expect(upgradePhases(planned.plan, undefined).map((p) => `${p.key}:${p.state}`)).toEqual([
      "backup:queued",
      "pull:queued",
      "start:queued",
      "check:queued",
    ]);
    const pulling = moved(planned, {
      state: ChangeState.APPLYING,
      steps: [
        step("vol_data", StepState.DONE),
        step("svc_web", StepState.RUNNING, {
          phase: StepPhase.PULLING,
          activity: "Pulling ghcr.io/acme/shop:1.5: 12 of 48 MB",
          doneBytes: 12n << 20n,
          totalBytes: 48n << 20n,
        }),
      ],
    });
    const phases = upgradePhases(pulling.plan, pulling);
    expect(phases.map((p) => `${p.key}:${p.state}`)).toEqual([
      "backup:done",
      "pull:current",
      "start:queued",
      "check:queued",
    ]);
    expect(phases[1]?.total).toBe(48n << 20n);
    const checking = moved(pulling, {
      steps: [step("vol_data", StepState.DONE), step("svc_web", StepState.RUNNING, { phase: StepPhase.CHECKING })],
    });
    expect(upgradePhases(checking.plan, checking).map((p) => p.state)).toEqual(["done", "done", "done", "current"]);
    // A failure says where by what it comes down to: a health check that failed is the check's.
    const failed = moved(pulling, {
      state: ChangeState.FAILED,
      steps: [step("vol_data", StepState.DONE), step("svc_web", StepState.FAILED, { reason: "unhealthy" })],
    });
    expect(upgradePhases(failed.plan, failed).map((p) => p.state)).toEqual(["done", "done", "done", "failed"]);
    const noRoom = moved(failed, {
      steps: [step("vol_data", StepState.DONE), step("svc_web", StepState.FAILED, { reason: "disk" })],
    });
    expect(upgradePhases(noRoom.plan, noRoom).map((p) => p.state)).toEqual(["done", "failed", "queued", "queued"]);
  });

  it("name a version by its image's tag", () => {
    expect(tagOf("postgres:16.4")).toBe("16.4");
    expect(tagOf("registry.example.com:5000/acme/shop")).toBe("latest");
    expect(tagOf("ghcr.io/acme/shop:1.5@sha256:abc")).toBe("1.5");
  });
});

describe("the plan sheet, for an update", () => {
  it("says what it goes from and to, backs up first unless you switch it off, and follows each phase", async () => {
    const user = userEvent.setup();
    const watch = feed<WatchChangeResponse>();
    const applies: ApplyChangeRequest[] = [];
    const planned = updatePlanned();
    let skipped: boolean | undefined;
    const opening = (backup: boolean): Opening => ({
      kind: "plan",
      make: () => {
        skipped = !backup;
        return Promise.resolve({ change: planned });
      },
      choice: {
        label: "Back up first",
        on: backup,
        description: backup ? "Backs up data before anything changes" : "Undo won't bring back what it changes in data",
        flip: () => opening(!backup),
      },
    });
    renderSheet(opening(true), {
      changes: {
        watchChange: watch.stream,
        discardChange: () => ({}),
        applyChange: (req) => {
          applies.push(req);
          return { change: moved(planned, { state: ChangeState.APPLYING }) };
        },
      },
    });
    const sheet = await screen.findByRole("dialog", { name: "Review plan" });
    await within(sheet).findByText("ghcr.io/acme/shop:1.4", { selector: "code" });
    const line = sheet.querySelector<HTMLElement>(".upgrade-line");
    if (!line) throw new Error("no line says what the update goes from and to");
    expect(line).toHaveTextContent("ghcr.io/acme/shop:1.4aaaaaaaaaaaa→toghcr.io/acme/shop:1.5");
    expect(skipped).toBe(false);
    await user.click(within(sheet).getByRole("switch", { name: /Back up first/ }));
    await waitFor(() => expect(skipped).toBe(true));
    await user.click(
      within(await screen.findByRole("dialog", { name: "Review plan" })).getByRole("switch", { name: /Back up first/ }),
    );
    await waitFor(() => expect(skipped).toBe(false));

    const review = await screen.findByRole("dialog", { name: "Review plan" });
    act(() => watch.push(watched(planned)));
    await user.click(within(review).getByRole("button", { name: /Apply plan/ }));
    await waitFor(() => expect(applies).toHaveLength(1));
    const running = moved(planned, {
      state: ChangeState.APPLYING,
      steps: [step("vol_data", StepState.DONE), step("svc_web", StepState.QUEUED)],
    });
    act(() => watch.push(watched(running)));
    const applying = await screen.findByRole("dialog", { name: "Updating to 1.5" });
    act(() =>
      watch.push(
        create(WatchChangeResponseSchema, {
          event: {
            case: "step",
            value: {
              index: 1,
              step: step("svc_web", StepState.RUNNING, {
                phase: StepPhase.PULLING,
                activity: "Pulling ghcr.io/acme/shop:1.5: 12 of 48 MB",
                doneBytes: 12n << 20n,
                totalBytes: 48n << 20n,
              }),
            },
          },
        }),
      ),
    );
    const phases = await within(applying).findByRole("list", { name: "How the update is going" });
    expect(within(phases).getByText("12 of 48 MB")).toBeVisible();
    expect(within(phases).getByRole("progressbar", { name: "Pulled of the image" })).toHaveAttribute(
      "value",
      String(12 << 20),
    );
    expect(phases).toHaveTextContent("Back upDone");
    expect(phases).toHaveTextContent("Pull the imageUnder way");
    // One server's update says how it goes as its phases, not its steps too.
    expect(within(applying).queryByRole("region", { name: "Steps" })).toBeNull();
    await expectAccessible(document.body);

    act(() => watch.push(watched(applied(running))));
    expect(await screen.findByRole("dialog", { name: "Updated to 1.5" })).toBeVisible();
  });
});

describe("an update on the app's page", () => {
  it("shows its phases while it applies, then that it's updated, with Undo, and nothing for another change", async () => {
    const watch = feed<WatchChangeResponse>();
    const planned = updatePlanned();
    const service = create(ServiceInfoSchema, { id: "svc_web", changeId: "chg_up", changeState: ChangeState.APPLYING });
    const { rerender } = renderWithApi(<UpgradeProgress service={service} fallback={<p>deploying</p>} />, {
      changes: { watchChange: watch.stream },
    });
    expect(screen.getByText("deploying")).toBeVisible();
    const running = moved(planned, {
      state: ChangeState.APPLYING,
      steps: [
        step("vol_data", StepState.RUNNING, { phase: StepPhase.BACKING_UP, activity: "Copying data: 1.2 MB so far" }),
      ],
    });
    act(() => watch.push(watched(running)));
    const card = await screen.findByRole("region", { name: "Updating to 1.5" });
    expect(within(card).getByRole("list", { name: "How the update is going" })).toHaveTextContent("Back upUnder way");
    expect(within(card).getByText("Copying data: 1.2 MB so far")).toBeVisible();
    act(() => watch.push(watched(applied(running))));
    const done = await screen.findByRole("region", { name: "Updated to 1.5" });
    expect(within(done).getByRole("button", { name: "Undo" })).toBeVisible();
    await expectAccessible(document.body);
    // A deploy that isn't an update is the page's own to say.
    const deploy = moved(planned, { id: "chg_deploy" });
    if (deploy.plan) deploy.plan.intent = create(IntentSchema, { kind: IntentKind.DEPLOY });
    rerender(
      <UpgradeProgress
        service={create(ServiceInfoSchema, {
          id: "svc_web",
          changeId: "chg_deploy",
          changeState: ChangeState.APPLYING,
        })}
        fallback={<p>deploying</p>}
      />,
    );
    act(() => watch.push(watched(moved(deploy, { state: ChangeState.APPLYING }))));
    expect(await screen.findByText("deploying")).toBeVisible();
  });
});
