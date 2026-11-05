// SPDX-License-Identifier: AGPL-3.0-only

import { create } from "@bufbuild/protobuf";
import { Code, ConnectError, type HandlerContext } from "@connectrpc/connect";
import { useQuery } from "@connectrpc/connect-query";
import {
  type Change,
  ChangeState,
  type GetChangeRequest,
  IntentKind,
  IntentSchema,
  RollbackState,
  type RollbackStatus,
  RollbackStatusSchema,
  ServiceService,
  StepProgressSchema,
  StepState,
  type WatchChangeResponse,
} from "@dockwell/api";
import { act, cleanup, screen, waitFor, within } from "@testing-library/react";
import { useEffect } from "react";
import { afterEach, describe, expect, it, vi } from "vitest";

import { activateLocale } from "../../app/i18n";
import { expectAccessible } from "../../test/a11y";
import { moved, shopPlanned, watched, webPlanned } from "../../test/changes";
import { feed, renderWithApi } from "../../test/render";
import { ChangeSheetProvider, type Opening, useChangeSheet } from "./ChangeSheet";
import { RollbackLine, type RollbackSubject, rollbackSubject, useRollbackWord } from "./RollbackLine";

type Status = Partial<Pick<RollbackStatus, "state" | "steps" | "restored" | "waitingFor" | "error" | "detail">>;

const status = (fields: Status) => create(RollbackStatusSchema, { id: "chg_back", ...fields });

/** A deploy of nginx:1.28 over a web running nginx:1.27 that failed, and its rollback as it stands. */
function aFailedDeploy(fields?: Status): Change {
  const change = webPlanned();
  const resource = change.plan?.resources[0];
  if (resource?.resource) resource.resource.kind = "service";
  if (change.plan) {
    change.plan.intent = create(IntentSchema, { kind: IntentKind.DEPLOY, resourceId: "res_web", image: "nginx:1.28" });
  }
  return moved(change, {
    state: ChangeState.FAILED,
    error: "web stopped right after it started, with exit code 1. Check its command and variables, then try again.",
    rolledBackBy: "chg_back",
    ...(fields ? { rollbackStatus: status(fields) } : {}),
  });
}

function aFailedFirstDeploy(fields: Status): Change {
  const change = shopPlanned();
  return moved(change, {
    state: ChangeState.FAILED,
    error: "it failed",
    rolledBackBy: "chg_back",
    rollbackStatus: status(fields),
  });
}

function AListOfApps() {
  useQuery(ServiceService.method.listServices, {});
  return null;
}

function Opener({ opening }: { opening: Opening }) {
  const sheet = useChangeSheet();
  useEffect(() => {
    sheet.open(opening);
  }, [sheet, opening]);
  return null;
}

/** The failed change in the plan sheet, as a person sees it once it has failed. */
async function inTheSheet(change: Change, name = "Couldn't deploy nginx:1.28") {
  const opening: Opening = { kind: "plan", make: () => Promise.resolve({ change }) };
  renderWithApi(
    <ChangeSheetProvider>
      <Opener opening={opening} />
    </ChangeSheetProvider>,
    { changes: { watchChange: feed<WatchChangeResponse>().stream } },
  );
  return screen.findByRole("dialog", { name });
}

afterEach(async () => {
  await activateLocale("en");
});

describe("what a failed change says of its rollback", () => {
  it("says the old version runs again, and that it was down, once a deploy that broke it is put back", async () => {
    const sheet = await inTheSheet(aFailedDeploy({ state: RollbackState.APPLIED, steps: 1, restored: ["web"] }));
    expect(sheet).toHaveTextContent("web runs nginx:1.27 again");
    expect(sheet).toHaveTextContent("Dockwell put it back after this failed, so it was down while that happened.");
    // What it set out to do is on the record, and nothing says it never happened.
    expect(within(sheet).getByText("What it set out to do")).toBeVisible();
    expect(sheet).not.toHaveTextContent("What it would have done");
    await expectAccessible(document.body);
  });

  it("says only that nothing moved when a deploy failed before it changed anything, not that it runs", async () => {
    const sheet = await inTheSheet(aFailedDeploy({ state: RollbackState.APPLIED }));
    expect(sheet).toHaveTextContent("web stays on nginx:1.27");
    expect(sheet).toHaveTextContent("This stopped before it changed anything on the server.");
    expect(sheet).not.toHaveTextContent("was down");
    // It may have stopped, or crashed, long before, and nothing here has read that.
    expect(sheet).not.toHaveTextContent("runs");
    // Green is for what is known to be well.
    const line = sheet.querySelector(".plan-rollback");
    expect(line).toHaveClass("dw-tone-idle");
    expect(line).not.toHaveClass("dw-tone-healthy");
  });

  it("says no outage for an app the rollback didn't have to touch, when it put back what else changed", async () => {
    const sheet = await inTheSheet(aFailedDeploy({ state: RollbackState.APPLIED, steps: 1, restored: ["web-data"] }));
    expect(sheet).toHaveTextContent("web stays on nginx:1.27");
    expect(sheet).toHaveTextContent("This stopped before it reached that, and Dockwell put back the rest.");
    expect(sheet).not.toHaveTextContent("was down");
    expect(sheet).not.toHaveTextContent("runs");
  });

  it("says nothing ran before a first deploy, by saying nothing once it is cleared away", async () => {
    const sheet = await inTheSheet(
      aFailedFirstDeploy({ state: RollbackState.APPLIED, steps: 2, restored: ["shop"] }),
      "Couldn't create shop",
    );
    expect(sheet.querySelector(".plan-rollback")).toBeNull();
    expect(sheet).not.toHaveTextContent("again");
  });

  it("says it is putting the old version back, and that the app may be down, while it does", async () => {
    const sheet = await inTheSheet(aFailedDeploy({ state: RollbackState.APPLYING, steps: 1, restored: ["web"] }));
    expect(sheet).toHaveTextContent("Dockwell is putting nginx:1.27 back");
    expect(sheet).toHaveTextContent("web may be down until that finishes.");
    expect(sheet).not.toHaveTextContent("runs nginx:1.27 again");
    expect(sheet.querySelector(".plan-rollback")).toHaveClass("dw-tone-cobalt");
    await expectAccessible(document.body);
  });

  it("says who it waits for, and that it can't tell what runs there", async () => {
    const sheet = await inTheSheet(
      aFailedDeploy({ state: RollbackState.WAITING, steps: 1, restored: ["web"], waitingFor: ["hz-1"] }),
    );
    expect(sheet).toHaveTextContent("Once hz-1 reconnects, Dockwell puts nginx:1.27 back");
    expect(sheet).toHaveTextContent(
      "It can't tell what is running there until then. Check that the server and its agent are up.",
    );
    expect(sheet).not.toHaveTextContent("runs nginx:1.27");
    expect(sheet.querySelector(".plan-rollback")).toHaveClass("dw-tone-warning");
    await expectAccessible(document.body);
  });

  it("says when putting it back failed too, why, what may be left, and what a person can do", async () => {
    const sheet = await inTheSheet(
      aFailedDeploy({
        state: RollbackState.FAILED,
        steps: 1,
        restored: ["web"],
        error: "Port 8080 is already in use on this server. Choose another port, or stop what uses it, then try again.",
        detail: "hz-1: can't run web: raw chain",
      }),
    );
    const line = sheet.querySelector(".plan-rollback") as HTMLElement;
    expect(line).toHaveAttribute("role", "alert");
    expect(line).toHaveClass("dw-tone-critical");
    expect(line).toHaveTextContent("Putting web back failed too");
    expect(line).toHaveTextContent("Port 8080 is already in use on this server.");
    expect(line).toHaveTextContent("web may not be running, or may be running the new version.");
    expect(line).toHaveTextContent("Check it, then deploy what should run, or fix it on the server.");
    const details = line.querySelector("details") as HTMLDetailsElement;
    expect(details.open).toBe(false);
    expect(details).toHaveTextContent("hz-1: can't run web: raw chain");
    expect(sheet).not.toHaveTextContent("runs nginx:1.27 again");
    await expectAccessible(document.body);
  });

  it("says the same of a first deploy in its own words, as there is nothing to put back", async () => {
    const { rerender } = renderWithApi(
      <RollbackLine
        change={aFailedFirstDeploy({ state: RollbackState.APPLYING })}
        subject={rollbackSubject(shopPlanned().plan)}
      />,
      { changes: { watchChange: feed<WatchChangeResponse>().stream } },
    );
    expect(screen.getByRole("status")).toHaveTextContent("Dockwell is clearing this away");
    expect(screen.getByRole("status")).not.toHaveTextContent("back");
    rerender(
      <RollbackLine
        change={aFailedFirstDeploy({ state: RollbackState.WAITING, waitingFor: ["hz-1"] })}
        subject={rollbackSubject(shopPlanned().plan)}
      />,
    );
    expect(screen.getByRole("status")).toHaveTextContent("Once hz-1 reconnects, Dockwell clears this away");
    rerender(
      <RollbackLine
        change={aFailedFirstDeploy({ state: RollbackState.FAILED, error: "it stuck" })}
        subject={rollbackSubject(shopPlanned().plan)}
      />,
    );
    expect(screen.getByRole("alert")).toHaveTextContent("Clearing this away failed too");
    expect(screen.getByRole("alert")).toHaveTextContent("Parts of it may still be on the server.");
  });

  it("reads for what doesn't run in its own words, and for many things at once", async () => {
    const rule = rollbackSubject(webPlanned().plan);
    const asRule: RollbackSubject = { ...rule, name: "http", previous: undefined, serves: false };
    const many: RollbackSubject = { name: undefined, serves: true, first: false };
    const applied = (restored: string[]) =>
      moved(aFailedDeploy({ state: RollbackState.APPLIED, steps: restored.length, restored }), {});
    const seen = (subject: RollbackSubject, change: Change) => {
      const { container, unmount } = renderWithApi(<RollbackLine change={change} subject={subject} record={false} />, {
        changes: { watchChange: feed<WatchChangeResponse>().stream },
      });
      const text = container.textContent;
      unmount();
      return text;
    };
    expect(seen(asRule, applied(["http"]))).toContain("http is back as it was");
    expect(seen(asRule, applied(["http"]))).not.toContain("was down");
    expect(seen(asRule, applied([]))).toContain("http stays as it was");
    expect(seen(many, applied(["a", "b"]))).toContain("What ran before runs again");
    expect(seen(many, applied(["a", "b"]))).toContain("some of it was down");
    expect(seen({ ...many, serves: false }, applied(["a", "b"]))).toContain("Everything is back as it was");
    expect(seen(many, applied([]))).toContain("Everything stays as it was");
    const going = aFailedDeploy({ state: RollbackState.APPLYING });
    expect(seen(many, going)).toContain("Dockwell is putting things back as they were");
    expect(seen(many, going)).toContain("Some of this may be down until that finishes.");
    expect(seen(asRule, going)).toContain("Dockwell is putting http back as it was");
    expect(seen(asRule, going)).not.toContain("may be down");
    const stuck = aFailedDeploy({ state: RollbackState.FAILED, error: "it stuck" });
    expect(seen(many, stuck)).toContain("Putting things back failed too");
    expect(seen(many, stuck)).toContain("The server may be left between the two.");
    const away = aFailedDeploy({ state: RollbackState.WAITING, waitingFor: ["hz-1", "hz-2"] });
    expect(seen(many, away)).toContain("Once hz-1, hz-2 reconnect, Dockwell puts things back as they were");
  });

  it("says what Dockwell did, not what runs now, when it is read later on the record", () => {
    const rule = rollbackSubject(webPlanned().plan);
    const applied = (restored: string[]) =>
      moved(aFailedDeploy({ state: RollbackState.APPLIED, steps: restored.length, restored }), {});
    const seen = (subject: RollbackSubject, change: Change) => {
      const { container, unmount } = renderWithApi(<RollbackLine change={change} subject={subject} />, {
        changes: { watchChange: feed<WatchChangeResponse>().stream },
      });
      const text = container.textContent;
      unmount();
      return text;
    };
    const web = { ...rule, name: "web", previous: "nginx:1.27", serves: true };
    expect(seen(web, applied(["web"]))).toContain("Dockwell put nginx:1.27 back on web");
    expect(seen(web, applied(["web"]))).not.toContain("runs");
    expect(seen(web, applied([]))).toContain("web stayed on nginx:1.27");
    expect(seen({ ...web, previous: undefined }, applied([]))).toContain("web stayed as it was");
    expect(seen({ ...web, previous: undefined, serves: false }, applied(["web"]))).toContain(
      "Dockwell put web back as it was",
    );
    expect(seen({ ...web, name: undefined }, applied(["a", "b"]))).toContain("Dockwell put things back as they were");
    expect(seen({ ...web, name: undefined }, applied([]))).toContain("Everything stayed as it was");
  });

  it("doesn't guess when the API hasn't said how it went", async () => {
    const sheet = await inTheSheet(aFailedDeploy());
    expect(sheet).toHaveTextContent("Dockwell made a rollback of this, and can't say yet how it went.");
    expect(sheet).not.toHaveTextContent("runs nginx:1.27");
  });

  /** The rollback's own change as a watch brings it. */
  const rollbackAt = (state: ChangeState, step: StepState) =>
    watched(
      moved(webPlanned(), {
        id: "chg_back",
        rollback: true,
        state,
        steps: [create(StepProgressSchema, { serverId: "srv_1", resourceId: "res_web", state: step })],
      }),
    );

  it("follows the rollback as it runs, saying what the API says at each move, and stops once it is over", async () => {
    const rollback = feed<WatchChangeResponse>();
    const following: string[] = [];
    const parts = { steps: 1, restored: ["web"] };
    let now = status({ state: RollbackState.APPLYING, ...parts });
    const asked: GetChangeRequest[] = [];
    const failed = aFailedDeploy({ state: RollbackState.APPLYING, ...parts });
    const over = vi.fn();
    renderWithApi(<RollbackLine change={failed} subject={rollbackSubject(failed.plan)} onSettled={over} />, {
      changes: {
        watchChange: (req: { id: string }, ctx: HandlerContext) => {
          following.push(req.id);
          return rollback.stream(req, ctx);
        },
        getChange: (req: GetChangeRequest) => {
          asked.push(req);
          return { change: moved(failed, { rollbackStatus: now }) };
        },
      },
    });
    expect(screen.getByRole("status")).toHaveTextContent("Dockwell is putting nginx:1.27 back");
    await waitFor(() => expect(following).toEqual(["chg_back"]));
    expect(over).not.toHaveBeenCalled();

    // Its server drops away mid-step.
    now = status({ state: RollbackState.WAITING, waitingFor: ["hz-1"], ...parts });
    act(() => rollback.push(rollbackAt(ChangeState.APPLYING, StepState.WAITING)));
    await waitFor(() =>
      expect(screen.getByRole("status")).toHaveTextContent("Once hz-1 reconnects, Dockwell puts nginx:1.27 back"),
    );
    // It asks about the change it is showing, and goes by what comes back.
    expect(asked[0]?.id).toBe("chg_web");

    // It comes back, and the rollback finishes.
    now = status({ state: RollbackState.APPLIED, ...parts });
    act(() => rollback.push(rollbackAt(ChangeState.APPLIED, StepState.DONE)));
    await waitFor(() => expect(screen.getByRole("status")).toHaveTextContent("web runs nginx:1.27 again"));
    await waitFor(() => expect(over).toHaveBeenCalledOnce());
  });

  it("reads again when a read fails, so it never sticks on a rollback that has finished", async () => {
    const rollback = feed<WatchChangeResponse>();
    const parts = { steps: 1, restored: ["web"] };
    const failed = aFailedDeploy({ state: RollbackState.APPLYING, ...parts });
    let reads = 0;
    renderWithApi(<RollbackLine change={failed} subject={rollbackSubject(failed.plan)} />, {
      changes: {
        watchChange: rollback.stream,
        getChange: () => {
          reads += 1;
          if (reads === 1) throw new ConnectError("no answer", Code.Unavailable);
          return { change: moved(failed, { rollbackStatus: status({ state: RollbackState.APPLIED, ...parts }) }) };
        },
      },
    });
    act(() => rollback.push(rollbackAt(ChangeState.APPLIED, StepState.DONE)));
    await waitFor(() => expect(screen.getByRole("status")).toHaveTextContent("web runs nginx:1.27 again"), {
      timeout: 4_000,
    });
  });

  it("keeps what has settled, whichever read comes last, and doesn't follow what is over", async () => {
    const applied = aFailedDeploy({ state: RollbackState.APPLIED, steps: 1, restored: ["web"] });
    const rollback = feed<WatchChangeResponse>();
    let reads = 0;
    const going = aFailedDeploy({ state: RollbackState.APPLYING, steps: 1, restored: ["web"] });
    const { rerender } = renderWithApi(<RollbackLine change={going} subject={rollbackSubject(going.plan)} />, {
      changes: {
        watchChange: rollback.stream,
        getChange: () => {
          reads += 1;
          return { change: going };
        },
      },
    });
    act(() => rollback.push(rollbackAt(ChangeState.APPLYING, StepState.RUNNING)));
    // What it read says it is still going, and is what it goes by, until the page knows better.
    await waitFor(() => expect(reads).toBeGreaterThan(0));
    await act(() => new Promise((resolve) => setTimeout(resolve, 20)));
    rerender(<RollbackLine change={applied} subject={rollbackSubject(applied.plan)} />);
    await waitFor(() => expect(screen.getByRole("status")).toHaveTextContent("web runs nginx:1.27 again"));
    cleanup();
    let watches = 0;
    renderWithApi(<RollbackLine change={applied} subject={rollbackSubject(applied.plan)} />, {
      changes: {
        watchChange: (req: unknown, ctx: HandlerContext) => {
          watches += 1;
          return rollback.stream(req, ctx);
        },
      },
    });
    await act(() => new Promise((resolve) => setTimeout(resolve, 20)));
    expect(watches).toBe(0);
  });

  it("reads the pages again once the rollback is over, as what it put back moves them on", async () => {
    const rollback = feed<WatchChangeResponse>();
    const parts = { steps: 1, restored: ["web"] };
    const failed = aFailedDeploy({ state: RollbackState.APPLYING, ...parts });
    let lists = 0;
    let now = status({ state: RollbackState.APPLYING, ...parts });
    const opening: Opening = { kind: "plan", make: () => Promise.resolve({ change: failed }) };
    renderWithApi(
      <ChangeSheetProvider>
        <Opener opening={opening} />
        <AListOfApps />
      </ChangeSheetProvider>,
      {
        services: {
          listServices: () => {
            lists += 1;
            return {};
          },
        },
        changes: {
          watchChange: (req: { id: string }, ctx: HandlerContext) =>
            req.id === "chg_back" ? rollback.stream(req, ctx) : feed<WatchChangeResponse>().stream(req, ctx),
          getChange: () => ({ change: moved(failed, { rollbackStatus: now }) }),
        },
      },
    );
    const sheet = await screen.findByRole("dialog", { name: "Couldn't deploy nginx:1.28" });
    await waitFor(() => expect(lists).toBeGreaterThan(0));
    await act(() => new Promise((resolve) => setTimeout(resolve, 20)));
    const before = lists;
    now = status({ state: RollbackState.APPLIED, ...parts });
    act(() => rollback.push(rollbackAt(ChangeState.APPLIED, StepState.DONE)));
    await waitFor(() => expect(sheet).toHaveTextContent("web runs nginx:1.27 again"));
    await waitFor(() => expect(lists).toBeGreaterThan(before));
  });

  it("names how a rollback stands for a list, in a word or two", () => {
    function Words() {
      const word = useRollbackWord();
      return (
        <ul>
          {[RollbackState.APPLIED, RollbackState.APPLYING, RollbackState.WAITING, RollbackState.FAILED].map((state) => (
            <li key={state}>{word(aFailedDeploy({ state }))}</li>
          ))}
          <li>{word(aFailedDeploy())}</li>
          <li>{word(moved(aFailedDeploy(), { rolledBackBy: "" })) || "none"}</li>
        </ul>
      );
    }
    renderWithApi(<Words />, {});
    expect(screen.getAllByRole("listitem").map((li) => li.textContent)).toEqual([
      "Rolled back",
      "Rolling back",
      "Rollback waits",
      "Rollback failed",
      "Rollback made",
      "none",
    ]);
  });

  it("says nothing when there is no rollback", async () => {
    const change = aFailedDeploy();
    change.rolledBackBy = "";
    const sheet = await inTheSheet(change);
    expect(sheet.querySelector(".plan-rollback")).toBeNull();
  });

  it("reads in Chinese, with the names as they are", async () => {
    await activateLocale("zh-CN");
    const applied = await inTheSheet(
      aFailedDeploy({ state: RollbackState.APPLIED, steps: 1, restored: ["web"] }),
      "无法部署 nginx:1.28",
    );
    const line = applied.querySelector(".plan-rollback") as HTMLElement;
    expect(line).toHaveTextContent("web");
    expect(line).toHaveTextContent("nginx:1.27");
    expect(line.textContent).toMatch(/[一-鿿]/);
    expect(line).not.toHaveTextContent("again");
    expect(line).not.toHaveTextContent("Dockwell put it back");
    await expectAccessible(document.body);
  });
});
