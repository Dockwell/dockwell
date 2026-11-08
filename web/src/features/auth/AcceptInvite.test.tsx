// SPDX-License-Identifier: AGPL-3.0-only

import { create } from "@bufbuild/protobuf";
import { timestampFromDate } from "@bufbuild/protobuf/wkt";
import { Code } from "@connectrpc/connect";
import { type CheckInviteRequest, Role, UserSchema } from "@dockwell/api";
import { screen, waitFor } from "@testing-library/react";
import userEvent from "@testing-library/user-event";
import { afterEach, describe, expect, it, vi } from "vitest";

import { activateLocale } from "../../app/i18n";
import { authError, authState, creationOptions, mira, stubPasskeys } from "../../test/auth";
import { renderAt } from "../../test/render";

const SECRET = "Xq0b7oA9qM2rL4tY8vW1zN6pK3sD5fG7hJ9kL1mN3pQ";
const ada = create(UserSchema, { id: "usr_2", name: "Ada Lovelace", email: "ada@example.com" });

function api(overrides: Record<string, unknown> = {}) {
  let joined = false;
  const calls = {
    getAuthState: () => (joined ? authState({ user: ada }) : authState({ user: undefined })),
    checkInvite: vi.fn((_req: CheckInviteRequest) => ({
      email: "ada@example.com",
      role: Role.DEVELOPER,
      projectName: "shop",
      invitedBy: "Mira Chen",
      expireTime: timestampFromDate(new Date(Date.now() + 7 * 86_400_000)),
    })),
    beginAcceptInvite: vi.fn(() => ({ ceremonyId: "cer_1", optionsJson: creationOptions })),
    finishAcceptInvite: vi.fn(() => {
      joined = true;
      return { user: ada };
    }),
    ...overrides,
  };
  return { auth: calls };
}

afterEach(async () => {
  await activateLocale("en");
});

describe("AcceptInvitePage", () => {
  it("says who invited them, as what and where, and makes them with a passkey", async () => {
    stubPasskeys();
    const calls = api();
    const { router } = renderAt(`/invite#${SECRET}`, calls);
    expect(
      await screen.findByRole("heading", { level: 1, name: "Mira Chen invited you as a developer of shop" }),
    ).toBeVisible();
    expect(screen.getByText(/For ada@example\.com, until/)).toBeVisible();
    // The secret leaves the address bar once read.
    await waitFor(() => expect(router.state.location.hash).toBe(""));
    expect(calls.auth.checkInvite).toHaveBeenCalledWith(expect.objectContaining({ secret: SECRET }), expect.anything());

    await userEvent.click(screen.getByRole("button", { name: "Create passkey and join" }));
    expect(await screen.findByText("Enter your name.")).toBeVisible();
    expect(calls.auth.beginAcceptInvite).not.toHaveBeenCalled();
    await userEvent.type(screen.getByLabelText("Your name"), "Ada Lovelace");
    await userEvent.click(screen.getByRole("button", { name: "Create passkey and join" }));
    await waitFor(() => expect(router.state.location.pathname).toBe("/"));
    expect(calls.auth.beginAcceptInvite).toHaveBeenCalledWith(
      expect.objectContaining({ secret: SECRET, name: "Ada Lovelace" }),
      expect.anything(),
    );
    expect(calls.auth.finishAcceptInvite).toHaveBeenCalledWith(
      expect.objectContaining({ ceremonyId: "cer_1", secret: SECRET }),
      expect.anything(),
    );
  });

  it("asks someone signed in to sign out first, as an invite makes someone new", async () => {
    renderAt(`/invite#${SECRET}`, api({ getAuthState: () => authState({ user: mira }) }));
    expect(await screen.findByRole("heading", { level: 1, name: "You're signed in as Mira Chen" })).toBeVisible();
    expect(screen.getByRole("button", { name: "Sign out" })).toBeVisible();
    expect(screen.queryByRole("button", { name: "Create passkey and join" })).toBeNull();
  });

  it("says plainly when the invite no longer works", async () => {
    renderAt(
      `/invite#${SECRET}`,
      api({
        checkInvite: () => {
          throw authError(Code.PermissionDenied, "invite_rejected");
        },
      }),
    );
    expect(await screen.findByRole("heading", { level: 1, name: "This invite didn't work" })).toBeVisible();
    expect(
      screen.getByText(
        "This invite is invalid, already used, revoked or expired. Ask whoever invited you for a new one.",
      ),
    ).toBeVisible();
  });

  it("asks for the whole link when the part after # is missing", async () => {
    renderAt("/invite", api());
    expect(
      await screen.findByRole("heading", { level: 1, name: "This page needs the whole link from your invite" }),
    ).toBeVisible();
  });

  it("speaks Simplified Chinese", async () => {
    await activateLocale("zh-CN");
    renderAt(`/invite#${SECRET}`, api());
    expect(await screen.findByRole("heading", { level: 1, name: "Mira Chen 邀请你成为 shop 的开发者" })).toBeVisible();
  });
});
