// SPDX-License-Identifier: AGPL-3.0-only

import { create } from "@bufbuild/protobuf";
import { timestampNow } from "@bufbuild/protobuf/wkt";
import { PlanSchema, StepProgressSchema, StepState } from "@dockwell/api";
import { describe, expect, it } from "vitest";

import { factAndRest, shownQuotes, stepGroups, waveProgress, withoutIds } from "./plan";

describe("factAndRest", () => {
  it("leads with what a check answered, and follows with what to do", () => {
    expect(
      factAndRest(
        "app runs, but fails its health check: GET /healthz on port 8080 answered 503 Service Unavailable. Fix what it answers, or the check, then try again.",
      ),
    ).toEqual([
      "app runs, but fails its health check: GET /healthz on port 8080 answered 503 Service Unavailable",
      "Fix what it answers, or the check, then try again.",
    ]);
  });

  it("doesn't end a sentence at a full stop in what a command printed", () => {
    expect(
      factAndRest(
        'app runs, but fails its health check: the command exited with 1: "no. Not \\"yet\\". Really: •••". Fix what it reports, or the check, then try again.',
      ),
    ).toEqual([
      'app runs, but fails its health check: the command exited with 1: "no. Not \\"yet\\". Really: •••"',
      "Fix what it reports, or the check, then try again.",
    ]);
  });

  it("keeps a machine value whole, and a message with one sentence", () => {
    expect(factAndRest("nginx:1.27 can't be pulled. Check the name.")).toEqual([
      "nginx:1.27 can't be pulled",
      "Check the name.",
    ]);
    expect(factAndRest("it failed")).toEqual(["it failed", ""]);
  });
});

describe("a build's failing step, quoted", () => {
  it("splits after the step however many quotes it has, and shows them plainly", () => {
    const said =
      'The build failed at step 4 of 4, "RUN echo \\"npm ERR! missing script: build\\" && exit 1": its log marks what it printed last. Fix it in the repository, then deploy again';
    const [fact, rest] = factAndRest(said).map(shownQuotes);
    expect(fact).toBe(
      'The build failed at step 4 of 4, "RUN echo "npm ERR! missing script: build" && exit 1": its log marks what it printed last',
    );
    expect(rest).toBe("Fix it in the repository, then deploy again");
    // An odd number of quotes in the step doesn't swallow the fix.
    const odd = 'The build failed at step 2 of 3, "RUN echo \\"it\'s done": its log marks what it printed last. Fix it';
    expect(factAndRest(odd)[1]).toBe("Fix it");
  });
});

describe("a firewall change refused at planning", () => {
  it("says why in the server's plain words, without the machine's words for where", () => {
    expect(
      withoutIds(
        "can't plan on web-1: can't plan firewall.policy default in srv_01M72392RAKQ0A5FQHPWBRX2YV: nobody is signed in to this server over SSH from outside it now, so Dockwell can't tell whose address to keep reaching SSH. Sign in over SSH from where you'll come in, or add a rule that allows SSH's port, then plan this again",
      ),
    ).toBe(
      "Nobody is signed in to this server over SSH from outside it now, so Dockwell can't tell whose address to keep reaching SSH. Sign in over SSH from where you'll come in, or add a rule that allows SSH's port, then plan this again",
    );
    expect(withoutIds("can't plan on web-1: its agent is away")).toBe("can't plan on web-1: its agent is away");
  });
});

describe("waves", () => {
  const plan = create(PlanSchema, {
    servers: ["a", "b", "c", "x"].map((id) => ({ serverId: id, serverName: id, steps: [{ resourceId: `res_${id}` }] })),
    rollout: { waves: [{ serverIds: ["a"] }, { serverIds: ["b", "c"] }] },
  });
  const step = (serverId: string, state: StepState, started = false) =>
    create(StepProgressSchema, { serverId, state, ...(started ? { startTime: timestampNow() } : {}) });

  it("groups a plan's steps by wave, with a server in none after them", () => {
    expect(stepGroups(plan).map((g) => [g.wave, g.servers.map((s) => s.serverId)])).toEqual([
      [1, ["a"]],
      [2, ["b", "c"]],
      [undefined, ["x"]],
    ]);
    expect(stepGroups(create(PlanSchema, { servers: plan.servers })).map((g) => g.key)).toEqual(["all"]);
  });

  it("says how a wave stands from its steps: waiting through a pause, running while one waits for its server", () => {
    const [first, second] = stepGroups(plan);
    if (!first || !second) throw new Error("no waves");
    expect(waveProgress(first, [step("a", StepState.DONE)])).toEqual({ state: "done", done: 1, total: 1 });
    // The pause before a wave holds its first step waiting, not yet started.
    expect(waveProgress(second, [step("b", StepState.WAITING), step("c", StepState.QUEUED)]).state).toBe("waiting");
    // A reboot's step waits for its server once it started.
    expect(waveProgress(second, [step("b", StepState.WAITING, true), step("c", StepState.QUEUED)])).toEqual({
      state: "running",
      done: 0,
      total: 2,
    });
    expect(waveProgress(second, [step("b", StepState.FAILED), step("c", StepState.SKIPPED)]).state).toBe("failed");
    expect(waveProgress(second, [step("b", StepState.SKIPPED), step("c", StepState.SKIPPED)]).state).toBe("skipped");
  });
});
