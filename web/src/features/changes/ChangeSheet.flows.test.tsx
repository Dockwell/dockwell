// SPDX-License-Identifier: AGPL-3.0-only

import { readFileSync } from "node:fs";
import { resolve } from "node:path";

import { create } from "@bufbuild/protobuf";
import { Code, ConnectError } from "@connectrpc/connect";
import {
  ActorKind,
  ActorSchema,
  type ApplyChangeRequest,
  type Change,
  ChangeSchema,
  ChangeState,
  FieldChangeSchema,
  FieldOp,
  HostAccessKind,
  HostAccessSchema,
  IntentKind,
  IntentSchema,
  PlanAction,
  PlanNoteSchema,
  PlannedResourceSchema,
  PlanPlaceSchema,
  PlanSummarySchema,
  Role,
  RollbackState,
  RollbackStatusSchema,
  RouteAnswersSchema,
  RouteReach,
  ServerPlanSchema,
  StepProgressSchema,
  StepState,
  StepUpAction,
  StepUpNeededSchema,
  type WatchChangeResponse,
} from "@dockwell/api";
import { CommandBar } from "@dockwell/ui";
import { act, screen, waitFor, within } from "@testing-library/react";
import userEvent from "@testing-library/user-event";
import { useEffect, useState } from "react";
import { afterEach, describe, expect, it, vi } from "vitest";

import { activateLocale } from "../../app/i18n";
import { expectAccessible } from "../../test/a11y";
import { authError, authState, requestOptions, mira as signedIn, stubPasskeys } from "../../test/auth";
import { applied, mira, moved, openHowItWorks, watched, webPlanned } from "../../test/changes";
import { feed, renderWithApi } from "../../test/render";
import { ChangeSheetProvider, type Opening, useChangeSheet } from "./ChangeSheet";
import { shortFingerprint } from "./plan";

function Opener({ opening }: { opening: Opening }) {
  const sheet = useChangeSheet();
  useEffect(() => {
    sheet.open(opening);
  }, [sheet, opening]);
  return null;
}

// The link needs a router and its own data, and Activity's tests have those. Here it's where it goes.
vi.mock("../logs/DeployLogLinks", () => ({
  FailedDeployLogs: () => <a href="#logs">Read this deploy's logs</a>,
}));

function show(change: Change, more: Partial<Opening> = {}, watch = feed<WatchChangeResponse>()) {
  const applies: ApplyChangeRequest[] = [];
  const opening = { kind: "plan", make: () => Promise.resolve({ change }), ...more } as Opening;
  const { unmount } = renderWithApi(
    <ChangeSheetProvider>
      <Opener opening={opening} />
    </ChangeSheetProvider>,
    {
      changes: {
        watchChange: watch.stream,
        applyChange: (req) => {
          applies.push(req);
          return { change: moved(change, { state: ChangeState.APPLYING }) };
        },
      },
    },
  );
  return { applies, watch, unmount };
}

/** A replace of web, as an app, for what the method that proposed it said. */
function forIntent(kind: IntentKind, fields: { image?: string; port?: number; revision?: bigint } = {}): Change {
  const change = webPlanned();
  const plan = change.plan;
  const resource = plan?.resources[0];
  const step = plan?.servers[0]?.steps[0];
  if (!plan || !resource?.resource || !step?.resource) throw new Error("webPlanned has no resource");
  resource.resource.kind = "service";
  step.resource.kind = "service";
  plan.intent = create(IntentSchema, { kind, resourceId: "res_web", ...fields });
  return change;
}

const aDeploy = () => forIntent(IntentKind.DEPLOY, { image: "nginx:1.28" });

/** The plan of a first Compose file: a project, its environment, a network, two volumes, and two apps. */
function aCompose(): Change {
  const made = (kind: string, name: string, extra: object = {}) => ({
    id: `res_${name}`,
    resource: { kind, scope: "x", name },
    action: PlanAction.CREATE,
    ...extra,
  });
  return create(ChangeSchema, {
    id: "chg_blog",
    summary: "Run blog from a Compose file",
    state: ChangeState.PLANNED,
    proposedBy: mira,
    plan: {
      fingerprint: "b10gb10gb10gb10gb10gb10gb10gb10g",
      summary: { create: 7 },
      resources: [
        made("project", "blog"),
        made("environment", "production"),
        made("network", "default", { changes: [{ path: "driver", op: FieldOp.ADDED, to: "bridge" }] }),
        made("volume", "db-data", { changes: [{ path: "driver", op: FieldOp.ADDED, to: "local" }] }),
        made("volume", "wp-data", { changes: [{ path: "driver", op: FieldOp.ADDED, to: "local" }] }),
        made("service", "db", { changes: [{ path: "app.image", op: FieldOp.ADDED, to: "mariadb:11.4" }] }),
        made("service", "web", {
          changes: [
            { path: "app.image", op: FieldOp.ADDED, to: "wordpress:6.6-apache" },
            { path: "app.ports[0].host_port", op: FieldOp.ADDED, to: "8081" },
            { path: "app.ports[0].container_port", op: FieldOp.ADDED, to: "80" },
          ],
        }),
      ],
    },
  });
}

const rowNames = (sheet: HTMLElement) =>
  [...sheet.querySelectorAll(".dw-plan > .dw-plan-list > .dw-plan-res > .dw-plan-row .dw-plan-name")].map(
    (el) => el.textContent,
  );

afterEach(async () => {
  vi.unstubAllGlobals();
  await activateLocale("en");
});

describe("a plan reads as what it is for", () => {
  it("calls a deploy a deploy from the row to the arrival, and keeps the downtime, which is the honest part", async () => {
    const user = userEvent.setup();
    const planned = aDeploy();
    const watch = feed<WatchChangeResponse>();
    show(planned, {}, watch);
    const sheet = await screen.findByRole("dialog", { name: "Review plan" });
    await within(sheet).findByText("to update");
    expect(sheet).toHaveTextContent("1 to update");
    expect(sheet.querySelector(".dw-plan-act")).toHaveTextContent("Deploy");
    expect(sheet).not.toHaveTextContent(/replace/i);
    expect(sheet).toHaveTextContent("about 2 s down");
    await expectAccessible(document.body);

    act(() => watch.push(watched(planned)));
    await user.click(within(sheet).getByRole("button", { name: /Apply plan/ }));
    const running = moved(planned, {
      state: ChangeState.APPLYING,
      steps: [
        create(StepProgressSchema, {
          serverId: "srv_1",
          resourceId: "res_web",
          state: StepState.RUNNING,
          message: "x",
        }),
      ],
    });
    act(() => watch.push(watched(running)));
    const applying = await screen.findByRole("dialog", { name: "Deploying nginx:1.28" });
    expect(within(applying).getByRole("region", { name: "Steps" })).toHaveTextContent("Deploying nginx:1.28");
    expect(applying).not.toHaveTextContent(/replac/i);

    act(() => watch.push(watched(applied(running))));
    const done = await screen.findByRole("dialog", { name: "web is deployed" });
    expect(done).toHaveTextContent("1 updated");
    expect(done.querySelector(".dw-plan-act")).toHaveTextContent("Deployed");
    expect(done).not.toHaveTextContent(/replac/i);
  });

  it("calls a new port what it is, not a replaced app", async () => {
    const planned = forIntent(IntentKind.PUBLISH_PORT, { port: 9411 });
    show(planned);
    const sheet = await screen.findByRole("dialog", { name: "Review plan" });
    await within(sheet).findByText("to update");
    expect(sheet.querySelector(".dw-plan-act")).toHaveTextContent("Publish port");
    expect(sheet).not.toHaveTextContent(/replace/i);
  });

  it("says a port it published and a revision it went back to, rather than that it is deployed", async () => {
    show(applied(forIntent(IntentKind.PUBLISH_PORT, { port: 9411 })), { doneLabel: "Done" });
    const port = await screen.findByRole("dialog", { name: "Published port 9411" });
    expect(port.querySelector(".dw-plan-act")).toHaveTextContent("Published port");
  });

  it("arrives from a rollback as the brand book has it", async () => {
    show(applied(forIntent(IntentKind.ROLL_BACK, { revision: 127n })));
    const back = await screen.findByRole("dialog", { name: "Rolled back to r127" });
    expect(back.querySelector(".dw-plan-act")).toHaveTextContent("Rolled back");
    expect(back).not.toHaveTextContent(/is deployed|replaced/i);
  });

  it("reads what it was for on a change's record too, once it is over", async () => {
    show(applied(aDeploy()), {});
    await screen.findByRole("dialog", { name: "web is deployed" });
  });

  it("leaves a plain replace as a replace, as nothing says what it is for", async () => {
    const plain = webPlanned();
    show(plain);
    const sheet = await screen.findByRole("dialog", { name: "Review plan" });
    await within(sheet).findByText("to replace");
    expect(sheet).toHaveTextContent("1 to replace");
    expect(sheet.querySelector(".dw-plan-act")).toHaveTextContent("Replace");
  });

  it("says the safety net on a plan with a revision to go back to, and nothing on a first one", async () => {
    const planned = aDeploy();
    const resource = planned.plan?.resources[0];
    if (resource) resource.baseRevision = 63n;
    show(planned);
    const sheet = await screen.findByRole("dialog", { name: "Review plan" });
    const effects = await within(sheet).findByRole("region", { name: "What applying means" });
    expect(effects).toHaveTextContent("Rolls back to r63 if it fails");
    expect(within(effects).getByText("r63").tagName).toBe("CODE");
  });

  it("has nothing to roll back to on a first deploy, and doesn't say it has", async () => {
    show(aCompose());
    const sheet = await screen.findByRole("dialog", { name: "Review plan" });
    await within(sheet).findByText("to create");
    expect(sheet).not.toHaveTextContent("Rolls back");
  });

  it("sets what machines read in mono in its subtitle and title", async () => {
    show(moved(aDeploy(), { summary: "Deploy nginx:1.28 to web", state: ChangeState.FAILED, error: "no." }));
    const sheet = await screen.findByRole("dialog", { name: "Couldn't deploy nginx:1.28" });
    const codes = [...(sheet.querySelector(".dw-sheet-sub")?.querySelectorAll("code") ?? [])].map((c) => c.textContent);
    expect(codes).toEqual(["nginx:1.28", "web"]);
    expect(sheet.querySelector(".dw-sheet-title code")).toHaveTextContent("nginx:1.28");
  });

  it.each(["no_backup", "undo_no_backup"])("says what a failed plan with %s left in the data", async (reason) => {
    const failed = moved(aDeploy(), { state: ChangeState.FAILED, error: "no." });
    if (failed.plan) failed.plan.notes = [create(PlanNoteSchema, { reason, message: "The server's words" })];
    show(failed);
    const sheet = await screen.findByRole("dialog", { name: "Couldn't deploy nginx:1.28" });
    const left = within(sheet).getByText("No backup was taken, and the data stays as it is now");
    expect(left.closest(".dw-callout")).toHaveClass("dw-tone-warning");
    expect(sheet).toHaveTextContent("may refuse it or misread it");
  });

  it("says nothing of data when a plan that failed had nothing to say of it", async () => {
    show(moved(aDeploy(), { state: ChangeState.FAILED, error: "no." }));
    const sheet = await screen.findByRole("dialog", { name: "Couldn't deploy nginx:1.28" });
    expect(sheet).not.toHaveTextContent("No backup was taken");
  });

  it("says where it lands once there is a choice, and only then", async () => {
    const planned = aDeploy();
    if (planned.plan) {
      planned.plan.places = [create(PlanPlaceSchema, { project: "shop", environment: "staging" })];
    }
    show(planned);
    const sheet = await screen.findByRole("dialog", { name: "Review plan" });
    await within(sheet).findByText("to update");
    expect(sheet).toHaveTextContent("In project shop, environment staging");
  });
});

describe("a pull of the same tag", () => {
  function aRepull(): Change {
    const change = forIntent(IntentKind.REPULL, { image: "nginx:latest" });
    const plan = change.plan;
    const resource = plan?.resources[0];
    const step = plan?.servers[0]?.steps[0];
    if (!plan || !resource || !step) throw new Error("no step");
    resource.changes = [];
    step.pull = true;
    step.changes = [];
    plan.notes = [
      create(PlanNoteSchema, {
        reason: "pulls_again",
        message: "Pulls nginx:latest again, and replaces web only if that's a newer image",
        resource: { kind: "service", name: "web" },
      }),
    ];
    return change;
  }
  const found = (message: string, did: PlanAction, serverId = "srv_1") =>
    create(StepProgressSchema, {
      serverId,
      resourceId: "res_web",
      state: StepState.DONE,
      message,
      did,
    });

  it("doesn't read as a certain replace: its downtime and its row say it may come to nothing", async () => {
    show(aRepull());
    const sheet = await screen.findByRole("dialog", { name: "Review plan" });
    await within(sheet).findByText("to update if newer");
    expect(sheet.querySelector(".dw-plan-act")).toHaveTextContent("Pull again");
    expect(sheet).toHaveTextContent("about 2 s down, if there is a newer image");
    expect(sheet).toHaveTextContent("Pulls nginx:latest again, and replaces web only if that's a newer image.");
    // It's no warning: there is nothing to weigh.
    expect(sheet.querySelector(".plan-risk")).toBeNull();
    expect(within(sheet).getByRole("button", { name: /Apply plan/ })).toBeEnabled();
  });

  it("says a certain downtime as it is, for a deploy that has changes too", async () => {
    const both = aRepull();
    const step = both.plan?.servers[0]?.steps[0];
    if (step) step.changes = [create(FieldChangeSchema, { path: "image", op: FieldOp.MODIFIED, from: "a", to: "b" })];
    show(both);
    const sheet = await screen.findByRole("dialog", { name: "Review plan" });
    await within(sheet).findByText("to update");
    expect(sheet).toHaveTextContent("about 2 s down");
    expect(sheet).not.toHaveTextContent("if there is a newer image");
  });

  it("says nothing changed when it found nothing newer, and doesn't read as a deploy", async () => {
    const note = "nginx:latest is already the newest image, so web stays as it is";
    const stayed = applied(aRepull());
    stayed.steps = [found(note, PlanAction.NONE)];
    // It wrote no revision, so the API offers nothing to undo.
    stayed.undoDeadline = undefined;
    show(stayed);
    const sheet = await screen.findByRole("dialog", { name: "web stays as it is" });
    expect(sheet.querySelector(".plan-lead")).toHaveTextContent(note);
    expect(sheet).not.toHaveTextContent("is deployed");
    expect(sheet).not.toHaveTextContent("1 updated");
    // There is nothing to take back.
    expect(within(sheet).queryByRole("button", { name: /Undo until/ })).toBeNull();
    await expectAccessible(document.body);
  });

  it("says it stays only if it stayed on every server, not if it was replaced on one", async () => {
    const note = "nginx:latest is already the newest image, so web stays as it is";
    const fleet = applied(aRepull());
    fleet.steps = [found(note, PlanAction.NONE, "srv_1"), found("Replaced web on hz-2", PlanAction.REPLACE, "srv_2")];
    show(fleet);
    const sheet = await screen.findByRole("dialog", { name: "Pulled nginx:latest again" });
    expect(sheet).not.toHaveTextContent("stays as it is");
    expect(sheet).toHaveTextContent("1 updated");
  });

  it("says it stays when it stayed on every server of many", async () => {
    const note = "nginx:latest is already the newest image, so web stays as it is";
    const fleet = applied(aRepull());
    fleet.steps = [found(note, PlanAction.NONE, "srv_1"), found(note, PlanAction.NONE, "srv_2")];
    show(fleet);
    await screen.findByRole("dialog", { name: "web stays as it is" });
  });

  it("believes what a step did, not what its words or the plan expected", async () => {
    // A step that says the words, and was replaced all the same.
    const replaced = applied(aRepull());
    replaced.steps = [found("nginx:latest is already the newest image, so web stays as it is", PlanAction.REPLACE)];
    show(replaced);
    const sheet = await screen.findByRole("dialog", { name: "Pulled nginx:latest again" });
    expect(sheet.querySelector(".plan-lead")).not.toHaveTextContent("stays as it is");
  });

  it("doesn't say it stays while a step hasn't said what it did", async () => {
    const resumed = applied(aRepull());
    resumed.steps = [
      create(StepProgressSchema, { serverId: "srv_1", resourceId: "res_web", state: StepState.DONE, message: "Done" }),
    ];
    show(resumed);
    await screen.findByRole("dialog", { name: "Pulled nginx:latest again" });
  });

  it("says a pull that has more to change replaces whatever it finds, in its counts, its downtime and its words", async () => {
    const both = aRepull();
    const step = both.plan?.servers[0]?.steps[0];
    if (step) step.changes = [create(FieldChangeSchema, { path: "ports", op: FieldOp.MODIFIED, from: "a", to: "b" })];
    show(both);
    const sheet = await screen.findByRole("dialog", { name: "Review plan" });
    await within(sheet).findByText("to update");
    expect(sheet).toHaveTextContent("Pulls nginx:latest again, and replaces web whatever that finds");
    expect(sheet).not.toHaveTextContent("only if that's a newer image");
    expect(sheet).not.toHaveTextContent("if newer");
    expect(sheet).not.toHaveTextContent("if there is a newer image");
    expect(sheet).not.toHaveTextContent("if this replaces anything");
  });

  it("says a pull of what isn't running starts it, in the server's words, once, and promises nothing about newer", async () => {
    const stopped = aRepull();
    const plan = stopped.plan;
    const step = plan?.servers[0]?.steps[0];
    if (!plan || !step) throw new Error("no step");
    step.changes = [create(FieldChangeSchema, { path: "state", op: FieldOp.MODIFIED, from: "exited", to: "running" })];
    plan.notes = [
      create(PlanNoteSchema, {
        reason: "starts_stopped",
        message: "web is stopped, so this starts it",
        names: ["hz-1"],
        resource: { kind: "service", name: "web" },
      }),
    ];
    show(stopped);
    const sheet = await screen.findByRole("dialog", { name: "Review plan" });
    await within(sheet).findByText("to update");
    expect(within(sheet).getAllByText("web is stopped, so this starts it.")).toHaveLength(1);
    // It's no warning: there is nothing to weigh, and it is what was asked for.
    expect(sheet.querySelector(".plan-risk")).toBeNull();
    expect(sheet).not.toHaveTextContent("only if that's a newer image");
    expect(sheet).not.toHaveTextContent("if newer");
    expect(sheet).not.toHaveTextContent("if there is a newer image");
    expect(sheet).not.toHaveTextContent("if this replaces anything");
  });

  it("says each server's own, when a pull starts what is stopped on more than one", async () => {
    const stopped = aRepull();
    const plan = stopped.plan;
    const step = plan?.servers[0]?.steps[0];
    if (!plan || !step) throw new Error("no step");
    step.changes = [create(FieldChangeSchema, { path: "state", op: FieldOp.MODIFIED, from: "exited", to: "running" })];
    plan.notes = ["hz-1", "hz-2"].map((server) =>
      create(PlanNoteSchema, {
        reason: "starts_stopped",
        message: `web is stopped on ${server}, so this starts it`,
        names: [server],
        resource: { kind: "service", name: "web" },
      }),
    );
    // Two rows with one key may render both now, and lose one on the next read.
    const warned = vi.spyOn(console, "error");
    show(stopped);
    const sheet = await screen.findByRole("dialog", { name: "Review plan" });
    await within(sheet).findByText("web is stopped on hz-1, so this starts it.");
    expect(within(sheet).getByText("web is stopped on hz-2, so this starts it.")).toBeVisible();
    expect(warned.mock.calls.filter(([said]) => String(said).includes("same key"))).toHaveLength(0);
    warned.mockRestore();
  });

  it("treats a restart policy that drifted as no reason to replace", async () => {
    const drifted = aRepull();
    const step = drifted.plan?.servers[0]?.steps[0];
    if (step) {
      step.changes = [
        create(FieldChangeSchema, { path: "app.restart", op: FieldOp.MODIFIED, from: "no", to: "always" }),
      ];
    }
    show(drifted);
    const sheet = await screen.findByRole("dialog", { name: "Review plan" });
    await within(sheet).findByText("to update if newer");
    expect(sheet).toHaveTextContent("about 2 s down, if there is a newer image");
    expect(sheet).toHaveTextContent("only if that's a newer image");
  });

  it("treats a limit a container can change in place as no reason to replace, and one it loses as a reason", async () => {
    const withChange = (path: string, op: FieldOp) => {
      const change = aRepull();
      const step = change.plan?.servers[0]?.steps[0];
      if (step)
        step.changes = [
          create(FieldChangeSchema, { path, op, from: "1", ...(op === FieldOp.REMOVED ? {} : { to: "2" }) }),
        ];
      return change;
    };
    for (const path of [
      "app.restart_retries",
      "app.resources.cpus",
      "app.resources.memory",
      "app.resources.memory_reservation",
    ]) {
      const { unmount } = show(withChange(path, FieldOp.MODIFIED));
      const sheet = await screen.findByRole("dialog", { name: "Review plan" });
      await within(sheet).findByText("to update if newer");
      unmount();
    }
    const { unmount } = show(withChange("app.resources.memory", FieldOp.REMOVED));
    const sheet = await screen.findByRole("dialog", { name: "Review plan" });
    await within(sheet).findByText("to update");
    expect(sheet).not.toHaveTextContent("if there is a newer image");
    unmount();
    show(withChange("app.resources.pids", FieldOp.MODIFIED));
    const other = await screen.findByRole("dialog", { name: "Review plan" });
    await within(other).findByText("to update");
    expect(other).not.toHaveTextContent("if newer");
  });

  it("says a pull that may come to nothing as that in its counts and beside Undo", async () => {
    show(aRepull());
    const sheet = await screen.findByRole("dialog", { name: "Review plan" });
    await within(sheet).findByText("to update if newer");
    expect(sheet).toHaveTextContent("Undo for 7 days if this replaces anything");
  });

  it("says leaving a pin replaces it, quietly, and promises nothing about newer", async () => {
    const pin = aRepull();
    const plan = pin.plan;
    if (plan?.notes[0]) {
      plan.notes[0].reason = "leaves_pin";
      plan.notes[0].message = "Pulls nginx:latest again, and replaces web with what that names now";
    }
    const step = plan?.servers[0]?.steps[0];
    if (step) step.changes = [create(FieldChangeSchema, { path: "image", op: FieldOp.MODIFIED, from: "a@d", to: "a" })];
    show(pin);
    const sheet = await screen.findByRole("dialog", { name: "Review plan" });
    await within(sheet).findByText("to update");
    expect(sheet).toHaveTextContent(
      "Pulls nginx:latest again, and replaces web with what that names now, in place of the digest it's pinned to.",
    );
    expect(sheet).not.toHaveTextContent("only if");
    expect(sheet).not.toHaveTextContent("if newer");
    expect(sheet.querySelector(".plan-risk")).toBeNull();
  });

  it("says what it pulled when it found a newer image", async () => {
    const newer = applied(aRepull());
    newer.steps = [found("Replaced web on hz-1", PlanAction.REPLACE)];
    show(newer);
    const sheet = await screen.findByRole("dialog", { name: "Pulled nginx:latest again" });
    expect(within(sheet).getByRole("button", { name: /Undo until/ })).toBeVisible();
    expect(sheet).toHaveTextContent("1 updated");
  });

  it("names the image when the pull failed", async () => {
    show(moved(aRepull(), { state: ChangeState.FAILED, error: "no." }));
    await screen.findByRole("dialog", { name: "Couldn't pull nginx:latest" });
  });
});

describe("a plan refused for another change's sake", () => {
  it("drops the refusal once that change has ended, as this plan is ready again", async () => {
    const user = userEvent.setup();
    const planned = aDeploy();
    let others: Change[] = [moved(webPlanned(), { id: "chg_other", state: ChangeState.APPLYING })];
    const busy = new ConnectError(
      "another change is applying web now. Apply it again once that one ends",
      Code.Aborted,
    );
    busy.metadata.set("Dockwell-Error", "busy");
    const opening: Opening = { kind: "plan", make: () => Promise.resolve({ change: planned }) };
    renderWithApi(
      <ChangeSheetProvider>
        <Opener opening={opening} />
      </ChangeSheetProvider>,
      {
        changes: {
          watchChange: feed<WatchChangeResponse>().stream,
          applyChange: () => {
            throw busy;
          },
          listChanges: () => ({ changes: others }),
        },
      },
    );
    const sheet = await screen.findByRole("dialog", { name: "Review plan" });
    await user.click(await within(sheet).findByRole("button", { name: /Apply plan/ }));
    const refused = await within(sheet).findByRole("alert");
    expect(refused).toHaveTextContent("That didn't apply");
    // It stays while that is still so.
    await act(() => new Promise((resolve) => setTimeout(resolve, 1_700)));
    expect(within(sheet).getByRole("alert")).toHaveTextContent("That didn't apply");
    others = [];
    await waitFor(() => expect(within(sheet).queryByRole("alert")).toBeNull(), { timeout: 3_000 });
  });

  it("keeps any other refusal, which won't end by itself", async () => {
    const user = userEvent.setup();
    const list = vi.fn(() => ({ changes: [] }));
    const opening: Opening = { kind: "plan", make: () => Promise.resolve({ change: aDeploy() }) };
    renderWithApi(
      <ChangeSheetProvider>
        <Opener opening={opening} />
      </ChangeSheetProvider>,
      {
        changes: {
          watchChange: feed<WatchChangeResponse>().stream,
          applyChange: () => {
            throw new ConnectError("the plan moved", Code.FailedPrecondition);
          },
          listChanges: list,
        },
      },
    );
    const sheet = await screen.findByRole("dialog", { name: "Review plan" });
    await user.click(await within(sheet).findByRole("button", { name: /Apply plan/ }));
    await within(sheet).findByRole("alert");
    await act(() => new Promise((resolve) => setTimeout(resolve, 1_700)));
    expect(within(sheet).getByRole("alert")).toHaveTextContent("That didn't apply");
    expect(list).not.toHaveBeenCalled();
  });
});

describe("a plan from a Compose file", () => {
  it("leads with its apps, the one with a way in first, and folds what they only need", async () => {
    const user = userEvent.setup();
    show(aCompose());
    const sheet = await screen.findByRole("dialog", { name: "Review plan" });
    await within(sheet).findByText("to create");
    const also = within(sheet).getByRole("button", { name: /Also creates project blog, a network and 2 volumes/ });
    expect(rowNames(sheet)).toEqual(["web app", "db app", "Also creates project blog, a network and 2 volumes"]);
    expect(also).toHaveAttribute("aria-expanded", "false");
    // Apps start open, with what they run; what comes along doesn't show until asked.
    expect(within(sheet).getByText("wordpress:6.6-apache")).toBeVisible();
    expect(within(sheet).queryByText("bridge")).not.toBeVisible();
    await user.click(also);
    expect(also).toHaveAttribute("aria-expanded", "true");
    expect(within(sheet).getByText("default")).toBeVisible();
    expect(within(sheet).getByText("db-data")).toBeVisible();
    await expectAccessible(document.body);
  });

  it("keeps environments out of sight on a first Compose file, as ADR 0026 has it", async () => {
    const user = userEvent.setup();
    show(aCompose());
    const sheet = await screen.findByRole("dialog", { name: "Review plan" });
    await within(sheet).findByText("to create");
    await user.click(within(sheet).getByRole("button", { name: /Also creates/ }));
    expect(sheet).not.toHaveTextContent("production");
    expect(sheet).not.toHaveTextContent(/environment/i);
    expect(sheet).toHaveTextContent("6 to create");
  });

  it("arrives under the name you gave it, with the apps closed and the rest folded", async () => {
    show(applied(aCompose()));
    const done = await screen.findByRole("dialog", { name: "blog is deployed" });
    expect(within(done).getByRole("button", { name: /Also created project blog/ })).toHaveAttribute(
      "aria-expanded",
      "false",
    );
    expect(within(done).getByRole("button", { name: /web app/ })).toHaveAttribute("aria-expanded", "false");
    expect(done).not.toHaveTextContent("2 apps are deployed");
  });

  it("never folds what is at risk, or anything that isn't new", async () => {
    const planned = aCompose();
    const resources = planned.plan?.resources ?? [];
    const risky = resources.find((r) => r.resource?.name === "db-data");
    if (risky) {
      risky.hostAccess = [create(HostAccessSchema, { kind: HostAccessKind.VOLUME_MOUNT, type: "nfs" })];
      risky.changes = [create(FieldChangeSchema, { path: "driver", op: FieldOp.ADDED, to: "local" })];
    }
    const changed = resources.find((r) => r.resource?.name === "default");
    if (changed) changed.action = PlanAction.UPDATE;
    show(planned);
    const sheet = await screen.findByRole("dialog", { name: "Review plan" });
    await within(sheet).findByText("to create");
    expect(rowNames(sheet)).toEqual([
      "db-data volume",
      "web app",
      "db app",
      "default network",
      "Also creates project blog and a volume",
    ]);
  });
});

describe("a delete says what stops and what stays", () => {
  function aDelete(fields: { volume?: string; destroys?: boolean; kept?: string[] } = {}): Change {
    const change = aDeploy();
    const plan = change.plan;
    const resource = plan?.resources[0];
    const server = plan?.servers[0];
    const step = server?.steps[0];
    if (!plan || !resource || !server || !step) throw new Error("no step");
    plan.intent = undefined;
    plan.summary = create(PlanSummarySchema, { destroy: 1 });
    resource.action = PlanAction.DELETE;
    resource.changes = fields.volume
      ? [create(FieldChangeSchema, { path: "app.mounts[0].volume", op: FieldOp.REMOVED, from: fields.volume })]
      : [];
    step.action = PlanAction.DELETE;
    step.downtime = undefined;
    server.downtime = undefined;
    step.destroysData = fields.destroys ?? false;
    step.keptPaths = fields.kept ?? [];
    if (fields.destroys || fields.kept) plan.destructive = true;
    return change;
  }

  it("names the volume that stays, and until when it can be undone", async () => {
    show(aDelete({ volume: "pg-data" }));
    const sheet = await screen.findByRole("dialog", { name: "Review plan" });
    await within(sheet).findByText("to delete");
    expect(sheet).toHaveTextContent(/web stops\. pg-data stays, and you can undo this until \d+ \w+\./);
    expect(sheet).not.toHaveTextContent("doesn't come back");
    expect([...(sheet.querySelector(".plan-lead")?.querySelectorAll("code") ?? [])].map((c) => c.textContent)).toEqual([
      "hz-1",
      "web",
      "pg-data",
    ]);
  });

  it("doesn't offer an undo that may not bring back what it destroys", async () => {
    const planned = aDelete({ destroys: true });
    show(planned);
    const sheet = await screen.findByRole("dialog", { name: "Review plan" });
    await within(sheet).findByText("to delete");
    expect(sheet.querySelector(".plan-lead")).toHaveTextContent("web stops.");
    expect(sheet.querySelector(".plan-lead")).not.toHaveTextContent("undo");
  });
});

describe("what a plan asks of whoever applies it", () => {
  function reaching(access: Parameters<typeof create<typeof HostAccessSchema>>[1]): Change {
    const change = webPlanned();
    const plan = change.plan;
    const resource = plan?.resources[0];
    if (!plan || !resource) throw new Error("no resource");
    plan.hostAccess = true;
    resource.hostAccess = [create(HostAccessSchema, access)];
    resource.changes = [
      create(FieldChangeSchema, { path: "app.mounts[0].host_path", op: FieldOp.ADDED, to: access?.path ?? "" }),
      create(FieldChangeSchema, { path: "app.mounts[0].target", op: FieldOp.ADDED, to: "/site" }),
    ];
    return change;
  }

  it("says a mount relabels what's there, however read-only it is", async () => {
    show(reaching({ kind: HostAccessKind.SERVER_PATH, path: "/srv/site", readOnly: true, relabel: "Z" }));
    const sheet = await screen.findByRole("dialog", { name: "Review plan" });
    const notice = await within(sheet).findByText(/relabels everything there/);
    expect(notice).toHaveTextContent("It can read that path on the server.");
    expect(notice).toHaveTextContent("as private to this app, so nothing else can use it.");
  });

  it("says so of a shared relabel, and beside what's as good as root", async () => {
    show(reaching({ kind: HostAccessKind.SERVER_PATH, path: "/etc", relabel: "z", sameAsRoot: true }));
    const sheet = await screen.findByRole("dialog", { name: "Review plan" });
    const risk = await within(sheet).findByRole("alert");
    expect(risk).toHaveTextContent("so reaching it is as good as root.");
    expect(risk).toHaveTextContent("Docker relabels everything there, to share it between containers.");
  });

  it("warns about an ordinary folder in amber, with nothing to type, and still sends the fingerprint", async () => {
    const user = userEvent.setup();
    const { applies } = show(
      reaching({ kind: HostAccessKind.SERVER_PATH, path: "/srv/site/html", readOnly: true, sameAsRoot: false }),
    );
    const sheet = await screen.findByRole("dialog", { name: "Review plan" });
    await within(sheet).findByText("server path");
    const notice = sheet.querySelector(".plan-risk") as HTMLElement;
    expect(notice).toHaveClass("dw-tone-warning");
    expect(notice).toHaveTextContent("web reaches /srv/site/html on hz-1");
    expect(notice).toHaveTextContent("It can read that path on the server.");
    expect(notice).not.toHaveTextContent("trust");
    expect(within(sheet).queryByRole("alert")).toBeNull();
    expect(within(sheet).queryByLabelText(/to apply it/)).toBeNull();
    expect(within(sheet).getByText("server path")).toHaveClass("is-warning");
    expect(sheet.querySelector(".dw-diff-critical")).toBeNull();
    expect(sheet.querySelector(".dw-diff-warning")).toHaveTextContent("/srv/site/html");
    const apply = within(sheet).getByRole("button", { name: /Apply plan/ });
    expect(apply).toHaveClass("dw-btn-primary");
    expect(apply).toBeEnabled();
    await expectAccessible(document.body);
    await user.click(apply);
    await waitFor(() => expect(applies[0]?.fingerprint).toBe("a1b2c3d4e5f60718293a4b5c6d7e8f90"));
  });

  it("keeps the alarm for a system path, which is as good as root, read-only or not", async () => {
    show(reaching({ kind: HostAccessKind.SERVER_PATH, path: "/etc", readOnly: true, sameAsRoot: true }));
    const sheet = await screen.findByRole("dialog", { name: "Review plan" });
    const risk = await within(sheet).findByRole("alert");
    expect(risk).toHaveClass("dw-tone-critical");
    expect(risk).toHaveTextContent("web reaches /etc on hz-1");
    expect(risk).toHaveTextContent("The server's security rests on what is there, so reaching it is as good as root.");
    expect(within(sheet).getByLabelText(/to apply it/)).toBeVisible();
    expect(within(sheet).getByRole("button", { name: /Apply plan/ })).toHaveClass("dw-btn-danger");
    expect(sheet.querySelector(".dw-diff-critical")).toHaveTextContent("/etc");
  });

  it("says privileged mode, a capability or a group that is as good as root as root, with its confirmation", async () => {
    for (const [access, words] of [
      [{ kind: HostAccessKind.PRIVILEGED, sameAsRoot: true }, "It runs in privileged mode"],
      [{ kind: HostAccessKind.CAPABILITY, path: "SYS_ADMIN", sameAsRoot: true }, "the Linux capability SYS_ADMIN"],
      [{ kind: HostAccessKind.GROUP, path: "docker", sameAsRoot: true }, "the group docker"],
    ] as const) {
      const { unmount } = show(reaching(access));
      const sheet = await screen.findByRole("dialog", { name: "Review plan" });
      const risk = await within(sheet).findByRole("alert");
      expect(risk).toHaveClass("dw-tone-critical");
      expect(risk).toHaveTextContent("web gets root on hz-1");
      expect(risk).toHaveTextContent(words);
      expect(risk).toHaveTextContent("Apply this only if you trust what it runs.");
      expect(within(sheet).getByLabelText(/to apply it/)).toBeVisible();
      unmount();
    }
  });

  describe("with what's as good as root", () => {
    const root = () => reaching({ kind: HostAccessKind.PRIVILEGED, sameAsRoot: true });
    const needsStepUp = () => authError(Code.PermissionDenied, "needs_step_up");

    function applyWith(change: Change, applyChange: (req: ApplyChangeRequest) => unknown, auth = {}) {
      return renderWithApi(
        <ChangeSheetProvider>
          <Opener opening={{ kind: "plan", make: () => Promise.resolve({ change }) } as Opening} />
        </ChangeSheetProvider>,
        { changes: { watchChange: feed<WatchChangeResponse>().stream, applyChange } as never, auth },
      );
    }

    async function typeAndApply(change: Change) {
      const sheet = await screen.findByRole("dialog", { name: "Review plan" });
      await userEvent.type(within(sheet).getByLabelText(/to apply it/), shortFingerprint(change.plan));
      await userEvent.click(within(sheet).getByRole("button", { name: /Apply plan/ }));
      return sheet;
    }

    it("says it asks for a passkey, then confirms this change with one, and applies it with the proof", async () => {
      const passkeys = stubPasskeys();
      const change = root();
      const applies: ApplyChangeRequest[] = [];
      const beginStepUp = vi.fn(() => ({ ceremonyId: "cer_1", optionsJson: requestOptions }));
      applyWith(
        change,
        (req) => {
          applies.push(req);
          if (!req.proof) throw needsStepUp();
          return { change: moved(change, { state: ChangeState.APPLYING }) };
        },
        { beginStepUp, finishStepUp: () => ({ proof: "proof_1" }) },
      );
      const sheet = await screen.findByRole("dialog", { name: "Review plan" });
      expect(await within(sheet).findByText("Applying this asks for your passkey")).toBeVisible();
      await typeAndApply(change);
      await waitFor(() => expect(applies).toHaveLength(2));
      expect(applies[0]?.proof).toBe("");
      expect(applies[1]).toMatchObject({ id: change.id, proof: "proof_1", fingerprint: change.plan?.fingerprint });
      expect(beginStepUp).toHaveBeenCalledWith(
        expect.objectContaining({ action: StepUpAction.APPLY_ROOT_CHANGE, target: change.id }),
        expect.anything(),
      );
      expect(passkeys.get).toHaveBeenCalledTimes(1);
    });

    it("says why a plan for what keeps root asks again", async () => {
      const change = root();
      change.plan?.notes.push(
        create(PlanNoteSchema, {
          reason: "new_code_as_root",
          message:
            "web reaches what's as good as root on its server, so whatever else runs in it has that reach too. Applying this takes a person and their passkey",
        }),
      );
      show(change);
      const sheet = await screen.findByRole("dialog", { name: "Review plan" });
      expect(
        await within(sheet).findByText("whatever else runs in it has that reach too", { exact: false }),
      ).toBeVisible();
      expect(within(sheet).getByText("Applying this asks for your passkey")).toBeVisible();
    });

    it("stays planned if you close the passkey prompt, and says so in words", async () => {
      stubPasskeys({ get: "NotAllowedError" });
      const change = root();
      applyWith(
        change,
        () => {
          throw needsStepUp();
        },
        { beginStepUp: () => ({ ceremonyId: "cer_1", optionsJson: requestOptions }) },
      );
      const sheet = await typeAndApply(change);
      expect(
        await within(sheet).findByText(
          "The passkey prompt closed, so nothing was applied and the plan is still planned. Apply it again when you're ready.",
        ),
      ).toBeVisible();
      expect(within(sheet).getByRole("button", { name: /Apply plan/ })).toBeVisible();
    });

    it("says what to do with no passkey for this address", async () => {
      stubPasskeys();
      const change = root();
      applyWith(
        change,
        () => {
          throw needsStepUp();
        },
        {
          beginStepUp: () => {
            throw authError(
              Code.FailedPrecondition,
              "no_passkey_here",
              "confirming this needs a passkey for this address, and you have none here. Add one under Profile and security, then try again",
            );
          },
        },
      );
      const sheet = await typeAndApply(change);
      expect(await within(sheet).findByText("Add one under Profile and security", { exact: false })).toBeVisible();
    });

    it("asks nothing of a plan that isn't", async () => {
      const { applies } = show(reaching({ kind: HostAccessKind.SERVER_PATH, path: "/srv/site/html" }));
      const sheet = await screen.findByRole("dialog", { name: "Review plan" });
      await within(sheet).findByRole("button", { name: /Apply plan/ });
      expect(within(sheet).queryByText("Applying this asks for your passkey")).toBeNull();
      await userEvent.click(within(sheet).getByRole("button", { name: /Apply plan/ }));
      await waitFor(() => expect(applies).toHaveLength(1));
      expect(applies[0]?.proof).toBe("");
    });
  });

  it("says a capability, a group or another network that stops short of root in amber, in its own words", async () => {
    for (const [access, title, detail] of [
      [
        { kind: HostAccessKind.CAPABILITY, path: "NET_RAW" },
        "web has the Linux capability NET_RAW on hz-1",
        "by default",
      ],
      [{ kind: HostAccessKind.GROUP, path: "999" }, "web joins the group 999 on hz-1", "owns Docker's socket"],
      [
        { kind: HostAccessKind.EXTERNAL_NETWORK, path: "edge", detail: "db" },
        "web joins the network edge on hz-1",
        "other projects' services too",
      ],
      [
        { kind: HostAccessKind.EXTERNAL_NETWORK, path: "lan", leadsTo: "macvlan" },
        "web joins the macvlan network lan on hz-1",
        "reaches past the server's firewall",
      ],
    ] as const) {
      const { unmount } = show(reaching(access));
      const sheet = await screen.findByRole("dialog", { name: "Review plan" });
      await waitFor(() => expect(sheet.querySelector(".plan-risk")).toBeTruthy());
      const notice = sheet.querySelector(".plan-risk") as HTMLElement;
      expect(notice).toHaveClass("dw-tone-warning");
      expect(notice).toHaveTextContent(title);
      expect(notice).toHaveTextContent(detail);
      expect(within(sheet).queryByLabelText(/to apply it/)).toBeNull();
      unmount();
    }
  });

  it("says where a symlink leads, red when that is as good as root, with its confirmation", async () => {
    show(
      reaching({
        kind: HostAccessKind.SERVER_PATH,
        path: "/srv/conf",
        leadsTo: "/etc",
        readOnly: true,
        sameAsRoot: true,
      }),
    );
    const sheet = await screen.findByRole("dialog", { name: "Review plan" });
    const risk = await within(sheet).findByRole("alert");
    expect(risk).toHaveClass("dw-tone-critical");
    expect(risk).toHaveTextContent("web reaches /srv/conf on hz-1");
    expect(risk).toHaveTextContent("It leads to /etc through a symlink");
    expect(risk).toHaveTextContent("as good as root");
    expect(within(sheet).getByLabelText(/to apply it/)).toBeVisible();
    expect(within(sheet).getByRole("button", { name: /Apply plan/ })).toHaveClass("dw-btn-danger");
    expect(sheet.querySelector(".dw-diff-critical")).toHaveTextContent("/srv/conf");
  });

  it("says where an ordinary symlink leads too, in amber, with nothing to type", async () => {
    show(
      reaching({
        kind: HostAccessKind.SERVER_PATH,
        path: "/srv/current",
        leadsTo: "/srv/releases/9",
        readOnly: false,
        sameAsRoot: false,
      }),
    );
    const sheet = await screen.findByRole("dialog", { name: "Review plan" });
    await within(sheet).findByText("server path");
    const notice = sheet.querySelector(".plan-risk") as HTMLElement;
    expect(notice).toHaveClass("dw-tone-warning");
    expect(notice).toHaveTextContent(
      "It leads to /srv/releases/9 through a symlink, and it can read and write what is there.",
    );
    expect(within(sheet).queryByLabelText(/to apply it/)).toBeNull();
  });

  it("says a symlink to Docker's socket is the socket, red, with its confirmation", async () => {
    show(
      reaching({
        kind: HostAccessKind.DOCKER_SOCKET,
        path: "/srv/sock",
        leadsTo: "/run/docker.sock",
        sameAsRoot: true,
      }),
    );
    const sheet = await screen.findByRole("dialog", { name: "Review plan" });
    const risk = await within(sheet).findByRole("alert");
    expect(risk).toHaveClass("dw-tone-critical");
    expect(risk).toHaveTextContent("/srv/sock leads to /run/docker.sock through a symlink");
    expect(risk).toHaveTextContent("control of Docker");
    expect(within(sheet).getByLabelText(/to apply it/)).toBeVisible();
  });

  it("reads a volume plugin that mounts the system as red, and never folds it away", async () => {
    const change = webPlanned();
    const plan = change.plan;
    const web = plan?.resources[0];
    if (!plan || !web) throw new Error("no resource");
    plan.hostAccess = true;
    plan.resources.push(
      create(PlannedResourceSchema, {
        id: "res_data",
        action: PlanAction.CREATE,
        resource: { kind: "volume", scope: "env_1", name: "data" },
        changes: [
          create(FieldChangeSchema, { path: "driver", op: FieldOp.ADDED, to: "acme/plugin" }),
          create(FieldChangeSchema, { path: "options[mountpoint]", op: FieldOp.ADDED, to: "/etc" }),
        ],
        hostAccess: [
          create(HostAccessSchema, {
            kind: HostAccessKind.VOLUME_MOUNT,
            driver: "acme/plugin",
            path: "/etc",
            sameAsRoot: true,
          }),
        ],
      }),
    );
    show(change);
    const sheet = await screen.findByRole("dialog", { name: "Review plan" });
    const risk = await within(sheet).findByRole("alert");
    expect(risk).toHaveClass("dw-tone-critical");
    expect(risk).toHaveTextContent("acme/plugin");
    expect(risk).toHaveTextContent("as good as root");
    expect(risk).toHaveTextContent("Apply this only if you trust what it runs.");
    expect(within(sheet).getByLabelText(/to apply it/)).toBeVisible();
    expect(within(sheet).getByRole("button", { name: /data/ })).toBeVisible();
    expect(within(sheet).queryByText(/Also creates/)).toBeNull();
    expect(sheet.querySelector(".dw-diff-critical")).toBeTruthy();
  });

  it("keeps a volume that binds a server path out of the fold, marked, as it is the risk", async () => {
    const change = webPlanned();
    const plan = change.plan;
    if (!plan) throw new Error("no plan");
    plan.hostAccess = true;
    plan.resources.push(
      create(PlannedResourceSchema, {
        id: "res_data",
        action: PlanAction.CREATE,
        resource: { kind: "volume", scope: "env_1", name: "data" },
        changes: [create(FieldChangeSchema, { path: "options[device]", op: FieldOp.ADDED, to: "/run/docker.sock" })],
        hostAccess: [create(HostAccessSchema, { kind: HostAccessKind.DOCKER_SOCKET, path: "/run/docker.sock" })],
      }),
    );
    show(change);
    const sheet = await screen.findByRole("dialog", { name: "Review plan" });
    await within(sheet).findByRole("alert");
    expect(within(sheet).queryByText(/Also creates/)).toBeNull();
    expect(sheet.querySelector(".dw-diff-critical")).toHaveTextContent("/run/docker.sock");
  });

  it("keeps a read and a write of one path apart, as they say different things", async () => {
    const change = reaching({ kind: HostAccessKind.SERVER_PATH, path: "/srv/x", readOnly: true });
    change.plan?.resources[0]?.hostAccess.push(
      create(HostAccessSchema, { kind: HostAccessKind.SERVER_PATH, path: "/srv/x", readOnly: false }),
    );
    show(change);
    const sheet = await screen.findByRole("dialog", { name: "Review plan" });
    await within(sheet).findByText("It can read that path on the server.");
    openHowItWorks(sheet);
    expect(within(sheet).getByText("It can read that path on the server.")).toBeVisible();
    expect(within(sheet).getByText("It can read and write that path on the server.")).toBeVisible();
  });

  it("keeps the alarm for Docker's socket and the host network, whatever a plan says of them", async () => {
    show(reaching({ kind: HostAccessKind.DOCKER_SOCKET, path: "/var/run/docker.sock" }));
    const sheet = await screen.findByRole("dialog", { name: "Review plan" });
    expect(await within(sheet).findByRole("alert")).toHaveTextContent("web gets root on hz-1");
    expect(within(sheet).getByLabelText(/to apply it/)).toBeVisible();
  });

  it("types nothing for a plan that says it reaches the server only if it doesn't say what", async () => {
    const vague = webPlanned();
    if (vague.plan) vague.plan.hostAccess = true;
    show(vague);
    const sheet = await screen.findByRole("dialog", { name: "Review plan" });
    expect(await within(sheet).findByLabelText(/to apply it/)).toBeVisible();
  });

  it("keeps saying what it reaches while it runs and once it has, in the right tense", async () => {
    const planned = reaching({ kind: HostAccessKind.DOCKER_SOCKET, path: "/var/run/docker.sock" });
    show(applied(planned));
    const done = await screen.findByRole("dialog", { name: /is replaced|Applied|web is deployed/ });
    const risk = within(done).getByRole("alert");
    expect(risk).toHaveTextContent("web gets root on hz-1");
    expect(risk).toHaveTextContent("Keep this only if you trust what it runs.");
    expect(risk).not.toHaveTextContent("Apply this");
  });
});

describe("a change that failed", () => {
  const failing = (fields: Partial<Change> = {}) =>
    moved(aDeploy(), {
      state: ChangeState.FAILED,
      error: "web stopped right after it started, with exit code 1. Check its command and variables, then try again.",
      steps: [
        create(StepProgressSchema, {
          serverId: "srv_1",
          resourceId: "res_web",
          state: StepState.FAILED,
          message: "x",
          reason: "exits",
        }),
      ],
      ...fields,
    });

  it("offers Done beside the way back, and × and Escape leave too", async () => {
    const user = userEvent.setup();
    const back = vi.fn();
    const done = vi.fn();
    show(failing(), { retryLabel: "Edit and try again", onClose: back, onDone: done });
    const sheet = await screen.findByRole("dialog", { name: "Couldn't deploy nginx:1.28" });
    const retry = within(sheet).getByRole("button", { name: /Edit and try again/ });
    expect(retry).toHaveClass("dw-btn-primary");
    await user.click(within(sheet).getByRole("button", { name: /^Done/ }));
    expect(done).toHaveBeenCalledOnce();
    expect(back).not.toHaveBeenCalled();
    await user.click(within(sheet).getByRole("button", { name: "Close" }));
    expect(done).toHaveBeenCalledTimes(2);
    await user.keyboard("{Escape}");
    expect(done).toHaveBeenCalledTimes(3);
    await user.click(retry);
    expect(back).toHaveBeenCalledOnce();
    expect(done).toHaveBeenCalledTimes(3);
  });

  it("says what to do in what the form in front of you has, from what it comes down to", async () => {
    const fix = vi.fn((reason: string) =>
      reason === "exits" ? "Check the image and tag, then deploy again." : undefined,
    );
    show(failing(), { fix });
    const sheet = await screen.findByRole("dialog", { name: "Couldn't deploy nginx:1.28" });
    const notice = within(sheet).getByRole("alert");
    expect(fix).toHaveBeenCalledWith("exits");
    expect(notice).toHaveTextContent("web stopped right after it started, with exit code 1");
    expect(notice).toHaveTextContent("Check the image and tag, then deploy again.");
    expect(notice).not.toHaveTextContent("Check its command and variables");
  });

  it("says what a failed check answered, with what to do, on the deploy", async () => {
    show(
      failing({
        error:
          "web runs, but fails its health check: GET /healthz on port 8080 answered 503 Service Unavailable. Fix what it answers, or the check, then try again.",
      }),
    );
    const sheet = await screen.findByRole("dialog", { name: "Couldn't deploy nginx:1.28" });
    const notice = within(sheet).getByRole("alert");
    expect(notice).toHaveTextContent(
      "web runs, but fails its health check: GET /healthz on port 8080 answered 503 Service Unavailable",
    );
    expect(notice).toHaveTextContent("Fix what it answers, or the check, then try again.");
    expect(within(notice).getByText("/healthz").tagName).toBe("CODE");
  });

  it("quotes what a command printed, masked, without reading its full stops as the server's", async () => {
    show(
      failing({
        error:
          'web runs, but fails its health check: the command exited with 1: "the database refuses connections. Its address: •••". Fix what it reports, or the check, then try again.',
      }),
    );
    const sheet = await screen.findByRole("dialog", { name: "Couldn't deploy nginx:1.28" });
    const notice = within(sheet).getByRole("alert");
    expect(notice).toHaveTextContent('"the database refuses connections. Its address: •••"');
    expect(notice).toHaveTextContent("Fix what it reports, or the check, then try again.");
  });

  it("says why first, on a rollback Dockwell made", async () => {
    const said =
      "web runs, but fails its health check: GET /healthz on port 8080 answered 503 Service Unavailable. Fix what it answers, or the check, then try again.";
    show(moved(aDeploy(), { state: ChangeState.APPLIED, reverts: "chg_failed", rollback: true, because: said }));
    const sheet = await screen.findByRole("dialog");
    const why = await waitFor(() => {
      const found = sheet.querySelector(".plan-because");
      if (!found) throw new Error("no reason yet");
      return found as HTMLElement;
    });
    expect(why).toHaveTextContent("Dockwell made this change because of what failed");
    expect(why).toHaveTextContent(said);
    expect(sheet.querySelector(".plan-body")?.firstElementChild).toBe(why);
  });

  it("says nothing of why on a change someone made", async () => {
    show(moved(aDeploy(), { state: ChangeState.APPLIED }));
    const sheet = await screen.findByRole("dialog");
    await waitFor(() => expect(sheet.querySelector(".plan-body")).not.toBeNull());
    expect(sheet.querySelector(".plan-because")).toBeNull();
  });

  it("leaves the server's words when the form has nothing better", async () => {
    show(failing(), { fix: () => undefined });
    const sheet = await screen.findByRole("dialog", { name: "Couldn't deploy nginx:1.28" });
    expect(within(sheet).getByRole("alert")).toHaveTextContent("Check its command and variables, then try again.");
  });

  it("leads with what runs now, then why", async () => {
    show(
      failing({
        rolledBackBy: "chg_back",
        rollbackStatus: create(RollbackStatusSchema, {
          id: "chg_back",
          state: RollbackState.APPLIED,
          steps: 1,
          restored: ["web"],
        }),
      }),
    );
    const sheet = await screen.findByRole("dialog", { name: "Couldn't deploy nginx:1.28" });
    const line = sheet.querySelector(".plan-rollback") as HTMLElement;
    const why = sheet.querySelector(".plan-failure") as HTMLElement;
    expect(line).toHaveTextContent("Dockwell put nginx:1.27 back on web");
    expect(line.compareDocumentPosition(why) & Node.DOCUMENT_POSITION_FOLLOWING).toBeTruthy();
  });

  it("says a failed first deploy's name, and falls back on saying the change failed", async () => {
    const many = moved(aCompose(), { state: ChangeState.FAILED, error: "no." });
    show(many);
    await screen.findByRole("dialog", { name: "Couldn't create blog" });
  });
});

describe("the logs of a deploy that failed", () => {
  it("are one link away, under why it failed", async () => {
    show(moved(aDeploy(), { state: ChangeState.FAILED, error: "web didn't start." }));
    const sheet = await screen.findByRole("dialog", { name: /Couldn't/ });
    const link = await within(sheet).findByRole("link", { name: "Read this deploy's logs" });
    const notice = within(sheet).getByRole("alert");
    expect(notice.compareDocumentPosition(link) & Node.DOCUMENT_POSITION_FOLLOWING).toBeTruthy();
  });

  it("aren't offered when its server went away, as they can't be read", async () => {
    show(
      moved(aDeploy(), {
        state: ChangeState.FAILED,
        error: "hz-1 went offline during this step.",
        steps: [
          create(StepProgressSchema, { serverId: "srv_1", resourceId: "res_web", state: StepState.FAILED, lost: true }),
        ],
      }),
    );
    const sheet = await screen.findByRole("dialog", { name: "hz-1 went offline during this deploy" });
    expect(within(sheet).queryByRole("link", { name: "Read this deploy's logs" })).toBeNull();
  });
});

describe("a server lost during a step", () => {
  const lost = (status: Parameters<typeof create<typeof RollbackStatusSchema>>[1]) =>
    moved(aDeploy(), {
      state: ChangeState.FAILED,
      error: "hz-1 went offline during this step, so Dockwell can't tell whether it ran. Check that hz-1 is up.",
      rolledBackBy: "chg_back",
      rollbackStatus: create(RollbackStatusSchema, { id: "chg_back", ...status }),
      steps: [
        create(StepProgressSchema, {
          serverId: "srv_1",
          resourceId: "res_web",
          state: StepState.FAILED,
          lost: true,
          message: "x",
          detail: "hz-1's agent disconnected during this step",
        }),
      ],
    });

  it("gives one answer: what it can't tell, what comes next, and Done", async () => {
    show(lost({ state: RollbackState.WAITING, steps: 1, restored: ["web"], waitingFor: ["hz-1"] }), {
      retryLabel: "Edit and try again",
    });
    const sheet = await screen.findByRole("dialog", { name: "hz-1 went offline during this deploy" });
    const line = sheet.querySelector(".plan-rollback") as HTMLElement;
    expect(line).toHaveClass("dw-tone-warning");
    expect(line).toHaveTextContent("Dockwell can't tell whether nginx:1.28 started");
    expect(line).toHaveTextContent("Once hz-1 reconnects, Dockwell puts nginx:1.27 back.");
    expect(line).toHaveTextContent("Check that the server and its agent are up.");
    // It hasn't failed so much as gone quiet, so nothing says it failed, or sends you back to a form.
    expect(within(sheet).queryByRole("alert")).toBeNull();
    expect(sheet).not.toHaveTextContent("The change failed");
    expect(within(sheet).queryByRole("button", { name: /Edit and try again/ })).toBeNull();
    expect(within(sheet).getByRole("button", { name: /^Done/ })).toHaveClass("dw-btn-primary");
    expect(line.querySelector("details")).toHaveTextContent("hz-1's agent disconnected during this step");
    await expectAccessible(document.body);
  });

  it("says what came of it once the server is back", async () => {
    show(lost({ state: RollbackState.APPLIED, steps: 1, restored: ["web"] }));
    const sheet = await screen.findByRole("dialog", { name: "hz-1 went offline during this deploy" });
    const line = sheet.querySelector(".plan-rollback") as HTMLElement;
    expect(line).toHaveTextContent("Dockwell put nginx:1.27 back on web");
    expect(line).toHaveTextContent("Dockwell put it back once it could reach the server.");
    expect(line).not.toHaveTextContent("was down");
  });
});

describe("Ctrl+Enter with something above the plan", () => {
  /** The bar as the shell has it, for this one key, above whatever is open. */
  function WithBar() {
    const [open, setOpen] = useState(false);
    useEffect(() => {
      const onKey = (event: KeyboardEvent) => {
        if (event.ctrlKey && event.key === "k") setOpen(true);
      };
      window.addEventListener("keydown", onKey);
      return () => window.removeEventListener("keydown", onKey);
    }, []);
    return <CommandBar open={open} items={[{ id: "overview", label: "Overview" }]} onClose={() => setOpen(false)} />;
  }

  function showWithBar(change: Change) {
    const applies: ApplyChangeRequest[] = [];
    const opening = { kind: "plan", make: () => Promise.resolve({ change }) } as Opening;
    renderWithApi(
      <ChangeSheetProvider>
        <Opener opening={opening} />
        <WithBar />
      </ChangeSheetProvider>,
      {
        changes: {
          watchChange: feed<WatchChangeResponse>().stream,
          applyChange: (req) => {
            applies.push(req);
            return { change: moved(change, { state: ChangeState.APPLYING }) };
          },
        },
      },
    );
    return applies;
  }

  it("applies nothing while the command bar is open, and applies once it has closed", async () => {
    const user = userEvent.setup();
    const applies = showWithBar(aDeploy());
    const sheet = await screen.findByRole("dialog", { name: "Review plan" });
    await within(sheet).findByText("to update");

    await user.keyboard("{Control>}k{/Control}");
    await screen.findByRole("combobox");
    await user.keyboard("{Control>}{Enter}{/Control}");
    await act(async () => {
      await Promise.resolve();
    });
    expect(applies).toHaveLength(0);
    expect(screen.getByRole("dialog", { name: "Review plan", hidden: true })).toBe(sheet);

    await user.keyboard("{Escape}");
    await waitFor(() => expect(screen.queryByRole("combobox")).toBeNull());
    await user.keyboard("{Control>}{Enter}{/Control}");
    await waitFor(() => expect(applies).toHaveLength(1));
  });

  it("doesn't take the way on from an arrival while the command bar is open", async () => {
    const user = userEvent.setup();
    const onDone = vi.fn();
    const change = moved(aDeploy(), { state: ChangeState.APPLIED });
    const opening = { kind: "plan", make: () => Promise.resolve({ change }), onDone } as Opening;
    renderWithApi(
      <ChangeSheetProvider>
        <Opener opening={opening} />
        <WithBar />
      </ChangeSheetProvider>,
      { changes: { watchChange: feed<WatchChangeResponse>().stream } },
    );
    const sheet = await screen.findByRole("dialog");
    await user.keyboard("{Control>}k{/Control}");
    await screen.findByRole("combobox");
    await user.keyboard("{Control>}{Enter}{/Control}");
    await act(async () => {
      await Promise.resolve();
    });
    expect(sheet).toBeInTheDocument();
    expect(onDone).not.toHaveBeenCalled();
  });
});

describe("a route with no certificate, on a fleet", () => {
  it("says on each server's step what stops and what to know, and takes away what it takes away", async () => {
    const change = webPlanned();
    const plan = change.plan;
    const first = plan?.servers[0];
    const step = first?.steps[0];
    if (!plan || !first || !step) throw new Error("no step");
    step.unencrypted = true;
    step.answers = create(RouteAnswersSchema, { reach: RouteReach.THIS_SERVER, plain: true });
    step.hostAccess = [
      create(HostAccessSchema, { kind: HostAccessKind.HTTPS_OFF, path: "live.example.com" }),
      create(HostAccessSchema, { kind: HostAccessKind.TRUSTED_PROXY, path: "cloudflare" }),
    ];
    plan.servers.push(create(ServerPlanSchema, { serverId: "srv_2", serverName: "hz-2", steps: [] }));
    show(change);
    const sheet = await screen.findByRole("dialog", { name: "Review plan" });
    const steps = await within(sheet).findByRole("region", { name: "Steps by server" });
    expect(steps).toHaveTextContent("Takes away HTTPS on live.example.com");
    expect(steps).not.toHaveTextContent("Reaches HTTPS");
    expect(steps).toHaveTextContent("Reaches cloudflare as a proxy");
    const known = within(steps).getByText(/Traffic for this route reaches hz-1 unencrypted/);
    expect(known).toHaveClass("plan-step-notice");
    expect(within(steps).getByText(/It answers only what runs on hz-1 itself/)).toHaveClass("plan-step-notice");
  });

  it("names a network's driver on its step, as the notice above it does", async () => {
    const change = webPlanned();
    const plan = change.plan;
    const step = plan?.servers[0]?.steps[0];
    if (!plan || !step) throw new Error("no step");
    step.hostAccess = [
      create(HostAccessSchema, { kind: HostAccessKind.EXTERNAL_NETWORK, path: "lan", leadsTo: "macvlan" }),
      create(HostAccessSchema, { kind: HostAccessKind.EXTERNAL_NETWORK, path: "wan", leadsTo: "ipvlan", detail: "db" }),
      create(HostAccessSchema, { kind: HostAccessKind.EXTERNAL_NETWORK, path: "edge" }),
    ];
    plan.servers.push(create(ServerPlanSchema, { serverId: "srv_2", serverName: "hz-2", steps: [] }));
    show(change);
    const sheet = await screen.findByRole("dialog", { name: "Review plan" });
    const steps = await within(sheet).findByRole("region", { name: "Steps by server" });
    expect(steps).toHaveTextContent(
      "the macvlan network lan, which reaches past the server's firewall and is on the server already",
    );
    expect(steps).toHaveTextContent("the ipvlan network wan, which reaches past the server's firewall");
    expect(steps).toHaveTextContent("where it answers to its name and to db");
    expect(steps).toHaveTextContent("the network edge, which is on the server already");
  });

  it("stands what to know apart with one server, where no steps show, and asks nothing more for it", async () => {
    const change = webPlanned();
    const step = change.plan?.servers[0]?.steps[0];
    if (!step) throw new Error("no step");
    step.unencrypted = true;
    show(change);
    const sheet = await screen.findByRole("dialog", { name: "Review plan" });
    const known = await within(sheet).findByText(/Traffic for this route reaches hz-1 unencrypted/);
    expect(known).toHaveClass("plan-note", "plan-step-notice");
    expect(within(sheet).queryByLabelText(/to apply it/)).toBeNull();
  });
});

describe("the sheet holds still", () => {
  it("keeps the body from review to arrival, with the diff where it was and the steps under it", async () => {
    const user = userEvent.setup();
    const planned = aDeploy();
    const watch = feed<WatchChangeResponse>();
    show(planned, {}, watch);
    const sheet = await screen.findByRole("dialog", { name: "Review plan" });
    await within(sheet).findByText("to update");
    const body = sheet.querySelector(".plan-body");
    const lead = sheet.querySelector(".plan-lead");
    const row = sheet.querySelector(".dw-plan-res");
    expect(body?.firstElementChild?.firstElementChild).toBe(lead);
    act(() => watch.push(watched(planned)));
    await user.click(within(sheet).getByRole("button", { name: /Apply plan/ }));
    const running = moved(planned, {
      state: ChangeState.APPLYING,
      steps: [create(StepProgressSchema, { serverId: "srv_1", resourceId: "res_web", state: StepState.RUNNING })],
    });
    act(() => watch.push(watched(running)));
    const applying = await screen.findByRole("dialog", { name: "Deploying nginx:1.28" });
    expect(applying.querySelector(".plan-body")).toBe(body);
    expect(applying.querySelector(".dw-plan-res")).toBe(row);
    const steps = within(applying).getByRole("region", { name: "Steps" });
    expect((row as Element).compareDocumentPosition(steps) & Node.DOCUMENT_POSITION_FOLLOWING).toBeTruthy();
    act(() => watch.push(watched(applied(running))));
    const done = await screen.findByRole("dialog", { name: "web is deployed" });
    expect(done.querySelector(".plan-body")).toBe(body);
    expect(done.querySelector(".dw-plan-res")).toBe(row);
  });

  it("gives Details a thumb's room on a touch screen and a phone", () => {
    const css = readFileSync(resolve(__dirname, "ChangeSheet.css"), "utf8");
    const rule =
      /(@media \(pointer: coarse\) \{\s*)?[^{}]*\.plan-details summary \{[^}]*min-height: 44px;[^}]*min-width: 44px|\.plan-details summary \{[^}]*min-width: 44px;[^}]*min-height: 44px/g;
    expect(css.match(rule)).toHaveLength(2);
    expect(css).toMatch(/@media \(pointer: coarse\)/);
    expect(css).toMatch(/\.plan-view\.is-phone \.plan-details summary/);
  });

  it("keeps its title to one line, as a title that wraps moves the plan under it", () => {
    const css = readFileSync(resolve(__dirname, "ChangeSheet.css"), "utf8");
    expect(css).toMatch(/\.plan-view \.dw-sheet-title,\s*\.plan-title-words \{[^}]*white-space: nowrap;/);
  });

  it("doesn't hold what its subtitle names to one line, which would clip it", () => {
    const css = readFileSync(resolve(__dirname, "ChangeSheet.css"), "utf8");
    expect(css).toMatch(/\.plan-view \.dw-sheet-sub code \{[^}]*overflow-wrap: anywhere;/);
  });

  it("sizes its type on the scale", () => {
    const css = readFileSync(resolve(__dirname, "ChangeSheet.css"), "utf8");
    // 16 px is a phone's own: a field any smaller makes it zoom in.
    expect(css.match(/font-size: [\d.]+px/g)?.filter((size) => !/ (12|13|14|16)px$/.test(size))).toEqual([]);
  });
});

describe("in Chinese", () => {
  it("reads as what it is for, and what a failed change set out to do reads as Chinese", async () => {
    await activateLocale("zh-CN");
    show(moved(aDeploy(), { state: ChangeState.FAILED, error: "no." }));
    const sheet = await screen.findByRole("dialog", { name: "无法部署 nginx:1.28" });
    expect(within(sheet).getByText("原定计划")).toBeVisible();
    expect(sheet).not.toHaveTextContent("它原本");
    await expectAccessible(document.body);
  });

  it("says what comes along in its own words", async () => {
    await activateLocale("zh-CN");
    show(aCompose());
    const sheet = await screen.findByRole("dialog", { name: "审查计划" });
    expect(await within(sheet).findByRole("button", { name: /还会创建项目 blog/ })).toBeVisible();
    await expectAccessible(document.body);
  });
});

describe("a plan in a protected environment", () => {
  const protectedPlan = () => moved(webPlanned(), { protected: true });
  const why =
    "As it changes web in a protected environment, an admin or the owner applies this. Leave it planned for them";
  const waiting = () => moved(webPlanned(), { protected: true, waits: why });

  it("tells someone below an admin that it waits for one, and why, and leaves it planned for them", async () => {
    const discardChange = vi.fn(() => ({}));
    const applyChange = vi.fn(() => ({}));
    const change = waiting();
    renderWithApi(
      <ChangeSheetProvider>
        <Opener opening={{ kind: "plan", make: () => Promise.resolve({ change }) } as Opening} />
      </ChangeSheetProvider>,
      {
        changes: { watchChange: feed<WatchChangeResponse>().stream, discardChange, applyChange } as never,
        auth: { getAuthState: () => authState({ user: { ...signedIn, role: Role.DEVELOPER } }) },
      },
    );
    const sheet = await screen.findByRole("dialog", { name: "Review plan" });
    expect(await within(sheet).findByText("An admin or the owner applies this")).toBeVisible();
    expect(within(sheet).getByText("Waits for an admin or the owner")).toBeVisible();
    expect(within(sheet).getByText(why)).toBeVisible();
    expect(within(sheet).queryByRole("button", { name: /Apply plan/ })).toBeNull();
    // ⌘↵ takes what the footer offers: it leaves the plan for an admin, and applies nothing.
    await userEvent.keyboard("{Control>}{Enter}{/Control}");
    await waitFor(() => expect(screen.queryByRole("dialog", { name: "Review plan" })).toBeNull());
    expect(applyChange).not.toHaveBeenCalled();
    expect(discardChange).not.toHaveBeenCalled();
  });

  it("leaves it for an admin from the button too", async () => {
    const discardChange = vi.fn(() => ({}));
    const change = waiting();
    renderWithApi(
      <ChangeSheetProvider>
        <Opener opening={{ kind: "plan", make: () => Promise.resolve({ change }) } as Opening} />
      </ChangeSheetProvider>,
      {
        changes: { watchChange: feed<WatchChangeResponse>().stream, discardChange } as never,
        auth: { getAuthState: () => authState({ user: { ...signedIn, role: Role.DEVELOPER } }) },
      },
    );
    const sheet = await screen.findByRole("dialog", { name: "Review plan" });
    await userEvent.click(await within(sheet).findByRole("button", { name: "Leave it for an admin" }));
    await waitFor(() => expect(screen.queryByRole("dialog", { name: "Review plan" })).toBeNull());
    expect(discardChange).not.toHaveBeenCalled();
  });

  it("discards their own waiting plan, reopened from Activity, and says it did; someone else's it leaves", async () => {
    const mine = moved(waiting(), {
      proposedBy: create(ActorSchema, { kind: ActorKind.USER, id: signedIn.id, name: signedIn.name }),
    });
    const discardChange = vi.fn(() => ({ change: moved(mine, { state: ChangeState.DISCARDED }) }));
    renderWithApi(
      <ChangeSheetProvider>
        <Opener opening={{ kind: "change", id: mine.id }} />
      </ChangeSheetProvider>,
      {
        changes: {
          getChange: () => ({ change: mine }),
          watchChange: feed<WatchChangeResponse>().stream,
          discardChange,
        } as never,
        auth: { getAuthState: () => authState({ user: { ...signedIn, role: Role.DEVELOPER } }) },
      },
    );
    const sheet = await screen.findByRole("dialog", { name: "Review plan" });
    await userEvent.click(await within(sheet).findByRole("button", { name: "Discard" }));
    await waitFor(() =>
      expect(discardChange).toHaveBeenCalledWith(expect.objectContaining({ id: mine.id }), expect.anything()),
    );
    expect(await screen.findByText("Discarded")).toBeVisible();
  });

  it("offers no Discard on someone else's waiting plan", async () => {
    const theirs = moved(waiting(), {
      proposedBy: create(ActorSchema, { kind: ActorKind.USER, id: "usr_other", name: "Mira Chen" }),
    });
    renderWithApi(
      <ChangeSheetProvider>
        <Opener opening={{ kind: "change", id: theirs.id }} />
      </ChangeSheetProvider>,
      {
        changes: { getChange: () => ({ change: theirs }), watchChange: feed<WatchChangeResponse>().stream } as never,
        auth: { getAuthState: () => authState({ user: { ...signedIn, role: Role.DEVELOPER } }) },
      },
    );
    const sheet = await screen.findByRole("dialog", { name: "Review plan" });
    expect(await within(sheet).findByRole("button", { name: "Leave it for an admin" })).toBeVisible();
    expect(within(sheet).queryByRole("button", { name: "Discard" })).toBeNull();
  });

  it("says applying a plan made with someone's API token approves it, as the API says", async () => {
    const change = moved(protectedPlan(), {
      approves: true,
      proposedBy: create(ActorSchema, { kind: ActorKind.TOKEN, id: "tok_1", name: "deploy" }),
    });
    renderWithApi(
      <ChangeSheetProvider>
        <Opener opening={{ kind: "plan", make: () => Promise.resolve({ change }) } as Opening} />
      </ChangeSheetProvider>,
      {
        changes: { watchChange: feed<WatchChangeResponse>().stream } as never,
        auth: { getAuthState: () => authState() },
      },
    );
    const sheet = await screen.findByRole("dialog", { name: "Review plan" });
    expect(
      await within(sheet).findByText(
        "Applying this approves a change planned with the API token deploy, with your passkey",
      ),
    ).toBeVisible();
  });

  it("approves someone else's with the passkey the refusal asks for, bound to its plan", async () => {
    stubPasskeys();
    const change = moved(protectedPlan(), { approves: true });
    const target = `${change.id}:${change.plan?.fingerprint}`;
    const beginStepUp = vi.fn(() => ({ ceremonyId: "cer_1", optionsJson: requestOptions }));
    const applies: ApplyChangeRequest[] = [];
    renderWithApi(
      <ChangeSheetProvider>
        <Opener opening={{ kind: "plan", make: () => Promise.resolve({ change }) } as Opening} />
      </ChangeSheetProvider>,
      {
        changes: {
          watchChange: feed<WatchChangeResponse>().stream,
          applyChange: (req: ApplyChangeRequest) => {
            applies.push(req);
            if (!req.proof) {
              throw new ConnectError(
                "approving it takes your passkey",
                Code.PermissionDenied,
                new Headers({ "Dockwell-Error": "needs_step_up" }),
                [
                  {
                    desc: StepUpNeededSchema,
                    value: create(StepUpNeededSchema, { action: StepUpAction.APPROVE_CHANGE, target }),
                  },
                ],
              );
            }
            return { change: moved(change, { state: ChangeState.APPLYING }) };
          },
        } as never,
        auth: { getAuthState: () => authState(), beginStepUp, finishStepUp: () => ({ proof: "proof_1" }) },
      },
    );
    const sheet = await screen.findByRole("dialog", { name: "Review plan" });
    expect(
      await within(sheet).findByText("Applying this approves Mira Chen's change, with your passkey"),
    ).toBeVisible();
    await userEvent.click(within(sheet).getByRole("button", { name: /Apply plan/ }));
    await waitFor(() => expect(applies).toHaveLength(2));
    expect(beginStepUp).toHaveBeenCalledWith(
      expect.objectContaining({ action: StepUpAction.APPROVE_CHANGE, target }),
      expect.anything(),
    );
    expect(applies[1]).toMatchObject({ id: change.id, proof: "proof_1", fingerprint: change.plan?.fingerprint });
  });
});
