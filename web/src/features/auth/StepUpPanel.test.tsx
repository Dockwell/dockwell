// SPDX-License-Identifier: AGPL-3.0-only

import { timestampFromDate } from "@bufbuild/protobuf/wkt";
import { Code } from "@connectrpc/connect";
import {
  type BeginStepUpRequest,
  type FinishStepUpRequest,
  type SetStepUpPolicyRequest,
  StepUpAction,
  StepUpPolicy,
} from "@dockwell/api";
import { screen, waitFor, within } from "@testing-library/react";
import userEvent from "@testing-library/user-event";
import { afterEach, describe, expect, it, vi } from "vitest";

import { activateLocale } from "../../app/i18n";
import { expectAccessible } from "../../test/a11y";
import { authError, requestOptions, stubPasskeys } from "../../test/auth";
import { renderAt } from "../../test/render";

afterEach(async () => {
  await activateLocale("en");
});

const at = (h: number, m: number) => timestampFromDate(new Date(2026, 10, 12, h, m));

interface Choice {
  policy: StepUpPolicy;
  floor?: StepUpPolicy;
  effective?: StepUpPolicy;
  coverTime?: ReturnType<typeof at>;
  coverExpireTime?: ReturnType<typeof at>;
  coverAtSignIn?: boolean;
}

/** The stand-in control plane: the choice it holds, and SetStepUpPolicy, which asks for a passkey first. */
function api(first: Choice, refuse?: string) {
  let choice = { floor: StepUpPolicy.AT_SIGN_IN, effective: first.policy, ...first };
  return {
    auth: {
      listPasskeys: () => ({ passkeys: [] }),
      listSessions: () => ({ sessions: [] }),
      getTotp: () => ({}),
      getStepUpPolicy: vi.fn(() => ({ choice })),
      beginStepUp: vi.fn((req: BeginStepUpRequest) => ({
        ceremonyId: `cer-${req.action}`,
        optionsJson: requestOptions,
      })),
      finishStepUp: vi.fn((req: FinishStepUpRequest) => ({ proof: `proof-${req.ceremonyId.slice(4)}` })),
      setStepUpPolicy: vi.fn((req: SetStepUpPolicyRequest) => {
        if (refuse) throw authError(Code.FailedPrecondition, refuse);
        if (req.proof === "") throw authError(Code.PermissionDenied, "needs_step_up");
        choice = {
          ...choice,
          policy: req.policy,
          effective: req.policy,
          coverTime: at(14, 2),
          coverExpireTime: at(15, 2),
        };
        return { choice };
      }),
    },
    tokens: { listTokens: () => ({ tokens: [] }) },
  };
}

const panel = async () => within(await screen.findByRole("region", { name: "Ask for my passkey" }));

describe("Ask for my passkey", () => {
  it("offers the three choices, each time first and chosen, and says what always asks", async () => {
    const { container } = renderAt("/profile", api({ policy: StepUpPolicy.EACH_TIME }));
    const p = await panel();
    const choices = await p.findAllByRole("radio");
    expect(choices.map((c) => c.closest("label")?.textContent)).toEqual([
      "Each timeEvery sensitive action asks.",
      "Once an hourA passkey covers this session for the next hour.",
      "Only when I sign inSigning in covers the whole session.",
    ]);
    await waitFor(() => expect(p.getByRole("radio", { name: /Each time/ })).toBeChecked());
    expect(
      p.getByText("Changing how you sign in, this choice, and approving someone else's change always ask."),
    ).toBeVisible();
    expect(p.queryByText(/Confirmed/)).toBeNull();
    await expectAccessible(container);
  });

  it("confirms a new choice with the passkey, and says what this session has of it", async () => {
    const passkeys = stubPasskeys();
    const calls = api({ policy: StepUpPolicy.EACH_TIME });
    renderAt("/profile", calls);
    const p = await panel();
    await userEvent.click(await p.findByRole("radio", { name: /Once an hour/ }));
    await waitFor(() => expect(calls.auth.setStepUpPolicy).toHaveBeenCalledTimes(2));
    expect(calls.auth.beginStepUp).toHaveBeenCalledWith(
      expect.objectContaining({ action: StepUpAction.SET_STEP_UP_POLICY, target: "" }),
      expect.anything(),
    );
    expect(passkeys.get).toHaveBeenCalledOnce();
    expect(calls.auth.setStepUpPolicy).toHaveBeenLastCalledWith(
      expect.objectContaining({ policy: StepUpPolicy.HOURLY, proof: `proof-${StepUpAction.SET_STEP_UP_POLICY}` }),
      expect.anything(),
    );
    expect(await p.findByText("Confirmed at 14:02 · asks again at 15:02")).toBeVisible();
    expect(p.getByRole("radio", { name: /Once an hour/ })).toBeChecked();
  });

  it("says a sign-in covers the session for as long as it lasts", async () => {
    renderAt(
      "/profile",
      api({ policy: StepUpPolicy.AT_SIGN_IN, coverTime: at(9, 15), coverExpireTime: at(21, 15), coverAtSignIn: true }),
    );
    const p = await panel();
    expect(
      await p.findByText("Confirmed when you signed in at 09:15, for as long as this session lasts"),
    ).toBeVisible();
  });

  it("says the next passkey opens the hour when nothing covers this session now", async () => {
    renderAt("/profile", api({ policy: StepUpPolicy.HOURLY }));
    const p = await panel();
    expect(await p.findByText("Your next passkey covers this session for an hour.")).toBeVisible();
  });

  it("offers what the owner's floor rules out off, with why, and a choice below it reads as the floor", async () => {
    renderAt(
      "/profile",
      api({ policy: StepUpPolicy.AT_SIGN_IN, floor: StepUpPolicy.HOURLY, effective: StepUpPolicy.HOURLY }),
    );
    const p = await panel();
    const signIn = await p.findByRole("radio", { name: /Only when I sign in/ });
    expect(signIn).toBeDisabled();
    expect(signIn.closest("label")).toHaveTextContent("The owner asks at least once an hour.");
    await waitFor(() => expect(p.getByRole("radio", { name: /Once an hour/ })).toBeChecked());
    expect(p.getByText("Your choice reads as once an hour, the least the owner allows.")).toBeVisible();
    expect(p.getByRole("radio", { name: /Each time/ })).toBeEnabled();
  });

  it("keeps the choice when the passkey prompt closes, and says so", async () => {
    stubPasskeys({ get: "NotAllowedError" });
    const calls = api({ policy: StepUpPolicy.EACH_TIME });
    renderAt("/profile", calls);
    const p = await panel();
    await userEvent.click(await p.findByRole("radio", { name: /Only when I sign in/ }));
    expect(await p.findByRole("alert")).toHaveTextContent("The passkey prompt closed, so your choice didn't change.");
    expect(p.getByRole("radio", { name: /Each time/ })).toBeChecked();
    expect(calls.auth.setStepUpPolicy).toHaveBeenCalledOnce();
  });

  it("explains a choice the floor refused meanwhile", async () => {
    stubPasskeys();
    renderAt("/profile", api({ policy: StepUpPolicy.EACH_TIME }, "below_floor"));
    const p = await panel();
    await userEvent.click(await p.findByRole("radio", { name: /Once an hour/ }));
    expect(await p.findByRole("alert")).toHaveTextContent(
      "The owner asks everyone for their passkey at least as often",
    );
  });

  it("speaks Chinese", async () => {
    await activateLocale("zh-CN");
    renderAt("/profile", api({ policy: StepUpPolicy.HOURLY, coverTime: at(14, 2), coverExpireTime: at(15, 2) }));
    const p = within(await screen.findByRole("region", { name: "何时要求验证通行密钥" }));
    expect(await p.findAllByRole("radio")).toHaveLength(3);
    expect(p.queryByText(/Each time|Once an hour/)).toBeNull();
  });
});
