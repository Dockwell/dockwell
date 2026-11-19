// SPDX-License-Identifier: AGPL-3.0-only

import { Code, createRouterTransport } from "@connectrpc/connect";
import { AuthService, StepUpAction } from "@dockwell/api";
import { afterEach, describe, expect, it, vi } from "vitest";

import { reasonOf } from "../../lib/errors";
import { authError, creationOptions, requestOptions, stubPasskeys } from "../../test/auth";
import { offerPasskeyHereWith } from "./passkeyHere";
import { stepUp } from "./stepUp";

/**
 * The control plane for someone with no passkey for this address: each step-up is
 * refused until one is added, and adding one begins as ask says. calls keeps what was
 * asked, in order.
 */
function noPasskeyHere(
  ask: () => { ceremonyId: string; optionsJson: string } = () => ({
    ceremonyId: "cer_1",
    optionsJson: creationOptions,
  }),
) {
  const calls: string[] = [];
  const state = { added: false };
  const transport = createRouterTransport(({ service }) =>
    service(AuthService, {
      beginStepUp: (req) => {
        calls.push(`beginStepUp ${req.action} ${req.target}`);
        if (!state.added) throw authError(Code.FailedPrecondition, "no_passkey_here");
        return { ceremonyId: `step_${req.action}_${req.target}`, optionsJson: requestOptions };
      },
      finishStepUp: (req) => ({ proof: `proof for ${req.ceremonyId}` }),
      beginAddPasskey: () => {
        calls.push("beginAddPasskey");
        return ask();
      },
    }),
  );
  return { calls, state, transport };
}

let stop: (() => void) | undefined;
afterEach(() => {
  stop?.();
  stop = undefined;
});

describe("the offer of a passkey for this address (#869)", () => {
  it("is one for two step-ups refused at once, and each goes on with its own proof once it's added", async () => {
    stubPasskeys();
    const { calls, state, transport } = noPasskeyHere();
    let offers = 0;
    let add = () => {};
    stop = offerPasskeyHereWith((first) => {
      offers++;
      expect(first.state).toBe("add");
      return new Promise<boolean>((resolve) => {
        add = () => {
          state.added = true;
          resolve(true);
        };
      });
    });

    const reveal = stepUp(transport, StepUpAction.REVEAL_SECRET, "sec_1");
    const server = stepUp(transport, StepUpAction.ADD_SERVER);
    await vi.waitFor(() => expect(calls.filter((c) => c.startsWith("beginStepUp"))).toHaveLength(2));
    await vi.waitFor(() => expect(offers).toBe(1));
    add();

    await expect(reveal).resolves.toBe(`proof for step_${StepUpAction.REVEAL_SECRET}_sec_1`);
    await expect(server).resolves.toBe(`proof for step_${StepUpAction.ADD_SERVER}_`);
    expect(offers).toBe(1);
    expect(calls.filter((c) => c === "beginAddPasskey")).toHaveLength(1);
  });

  it("isn't made with no offer in place, and the refusal stands", async () => {
    stubPasskeys();
    const { calls, transport } = noPasskeyHere();
    const refused = await stepUp(transport, StepUpAction.ADD_SERVER).catch((err: unknown) => err);
    expect(reasonOf(refused)).toBe("no_passkey_here");
    expect(calls).not.toContain("beginAddPasskey");
  });

  it("isn't made where the control plane can't say what adding one takes, and the refusal stands", async () => {
    stubPasskeys();
    const { transport } = noPasskeyHere(() => {
      throw authError(Code.Unavailable, "unavailable");
    });
    const offer = vi.fn(async () => true);
    stop = offerPasskeyHereWith(offer);
    const refused = await stepUp(transport, StepUpAction.ADD_SERVER).catch((err: unknown) => err);
    expect(reasonOf(refused)).toBe("no_passkey_here");
    expect(offer).not.toHaveBeenCalled();
  });

  it("turned down, leaves the refusal standing", async () => {
    stubPasskeys();
    const { calls, transport } = noPasskeyHere();
    stop = offerPasskeyHereWith(async () => false);
    const refused = await stepUp(transport, StepUpAction.ADD_SERVER).catch((err: unknown) => err);
    expect(reasonOf(refused)).toBe("no_passkey_here");
    expect(calls.filter((c) => c.startsWith("beginStepUp"))).toHaveLength(1);
  });
});
