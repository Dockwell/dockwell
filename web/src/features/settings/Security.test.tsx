// SPDX-License-Identifier: AGPL-3.0-only

import { create } from "@bufbuild/protobuf";
import { Code } from "@connectrpc/connect";
import {
  type BeginStepUpRequest,
  type FinishStepUpRequest,
  Role,
  type SetStepUpFloorRequest,
  SettingsSchema,
  StepUpAction,
  StepUpPolicy,
} from "@dockwell/api";
import { screen, waitFor, within } from "@testing-library/react";
import userEvent from "@testing-library/user-event";
import { afterEach, describe, expect, it, vi } from "vitest";

import { activateLocale } from "../../app/i18n";
import { expectAccessible } from "../../test/a11y";
import { authError, authState, mira, requestOptions, stubPasskeys } from "../../test/auth";
import { renderAt } from "../../test/render";

afterEach(async () => {
  await activateLocale("en");
});

const settings = (stepUpFloor: StepUpPolicy) =>
  create(SettingsSchema, { panel: { listen: "127.0.0.1:7600" }, stepUpFloor });

/** The stand-in control plane, with the floor it holds, for someone holding role. */
function api(role = Role.OWNER) {
  let floor = StepUpPolicy.AT_SIGN_IN;
  return {
    auth: {
      getAuthState: () => authState({ user: { ...mira, role } }),
      beginStepUp: vi.fn((req: BeginStepUpRequest) => ({
        ceremonyId: `cer-${req.action}`,
        optionsJson: requestOptions,
      })),
      finishStepUp: vi.fn((req: FinishStepUpRequest) => ({ proof: `proof-${req.ceremonyId.slice(4)}` })),
    },
    settings: {
      getSettings: vi.fn(() => ({ settings: settings(floor) })),
      setStepUpFloor: vi.fn((req: SetStepUpFloorRequest) => {
        if (req.proof === "") throw authError(Code.PermissionDenied, "needs_step_up");
        floor = req.floor;
        return { settings: settings(floor) };
      }),
    },
  };
}

describe("Security", () => {
  it("is in Settings beside General", async () => {
    renderAt("/settings/security", api());
    const nav = await screen.findByRole("navigation", { name: "Settings" });
    expect(within(nav).getByRole("link", { name: "Security" })).toHaveAttribute("aria-current", "page");
    expect(await screen.findByRole("heading", { level: 1, name: "Security" })).toBeVisible();
  });

  it("lets the owner set the least anyone may choose, with a passkey", async () => {
    const passkeys = stubPasskeys();
    const calls = api();
    const { container } = renderAt("/settings/security", calls);
    const group = await screen.findByRole("group", { name: "The least anyone may choose" });
    await waitFor(() => expect(within(group).getByRole("radio", { name: /Let each person choose/ })).toBeChecked());
    await expectAccessible(container);
    await userEvent.click(within(group).getByRole("radio", { name: /At least once an hour/ }));
    await waitFor(() => expect(calls.settings.setStepUpFloor).toHaveBeenCalledTimes(2));
    expect(calls.auth.beginStepUp).toHaveBeenCalledWith(
      expect.objectContaining({ action: StepUpAction.SET_STEP_UP_FLOOR, target: "" }),
      expect.anything(),
    );
    expect(passkeys.get).toHaveBeenCalledOnce();
    expect(calls.settings.setStepUpFloor).toHaveBeenLastCalledWith(
      expect.objectContaining({ floor: StepUpPolicy.HOURLY, proof: `proof-${StepUpAction.SET_STEP_UP_FLOOR}` }),
      expect.anything(),
    );
    await waitFor(() => expect(within(group).getByRole("radio", { name: /At least once an hour/ })).toBeChecked());
    const region = screen.getByRole("region", { name: "Ask for a passkey" });
    expect(within(region).getByRole("link", { name: "Profile and security" })).toHaveAttribute("href", "/profile");
  });

  it("shows anyone else the floor, off, with who sets it", async () => {
    const calls = api(Role.ADMIN);
    renderAt("/settings/security", calls);
    expect(await screen.findByText("Only the owner sets this.")).toBeVisible();
    const group = screen.getByRole("group", { name: "The least anyone may choose" });
    expect(group).toBeDisabled();
    await waitFor(() => expect(within(group).getByRole("radio", { name: /Let each person choose/ })).toBeChecked());
    expect(calls.settings.setStepUpFloor).not.toHaveBeenCalled();
  });

  it("keeps the floor when the passkey prompt closes", async () => {
    stubPasskeys({ get: "NotAllowedError" });
    renderAt("/settings/security", api());
    const group = await screen.findByRole("group", { name: "The least anyone may choose" });
    await waitFor(() => expect(within(group).getByRole("radio", { name: /Let each person choose/ })).toBeChecked());
    await userEvent.click(within(group).getByRole("radio", { name: /^Each time/ }));
    expect(await screen.findByText("The passkey prompt closed, so the floor didn't change.")).toBeVisible();
    expect(within(group).getByRole("radio", { name: /Let each person choose/ })).toBeChecked();
  });
});
