// SPDX-License-Identifier: AGPL-3.0-only

import { create } from "@bufbuild/protobuf";
import { timestampFromDate } from "@bufbuild/protobuf/wkt";
import { Code } from "@connectrpc/connect";
import { PasskeySchema, SessionSchema, SignInMethod, TotpSchema } from "@dockwell/api";
import { screen, waitFor, within } from "@testing-library/react";
import userEvent from "@testing-library/user-event";
import { describe, expect, it, vi } from "vitest";

import { activateLocale } from "../../app/i18n";
import { expectAccessible } from "../../test/a11y";
import { authError, authState, creationOptions, stubPasskeys } from "../../test/auth";
import { renderAt } from "../../test/render";
import { groupSecret } from "./TotpPanel";

const minutesAgo = (m: number) => timestampFromDate(new Date(Date.now() - m * 60_000));

const laptop = create(PasskeySchema, {
  id: "pky_1",
  name: "Chrome on macOS",
  rpId: "localhost",
  createTime: timestampFromDate(new Date(2026, 2, 12)),
  lastUsedTime: minutesAgo(0),
  synced: true,
});
const yubikey = create(PasskeySchema, {
  id: "pky_2",
  name: "YubiKey 5C",
  rpId: "panel.acme.dev",
  createTime: timestampFromDate(new Date(2026, 5, 4)),
});

const sessions = [
  create(SessionSchema, {
    id: "ses_1",
    current: true,
    userAgent: "Mozilla/5.0 (Macintosh; Intel Mac OS X 10_15_7) Chrome/141.0.0.0 Safari/537.36",
    address: "127.0.0.1",
    lastSeenTime: minutesAgo(0),
    rpId: "localhost",
    signInMethod: SignInMethod.PASSKEY,
  }),
  create(SessionSchema, {
    id: "ses_2",
    userAgent: "Mozilla/5.0 (iPhone; CPU iPhone OS 18_1 like Mac OS X) Version/18.1 Mobile/15E148 Safari/604.1",
    address: "198.51.100.23",
    lastSeenTime: minutesAgo(120),
    rpId: "panel.acme.dev",
  }),
];

function api(overrides: Record<string, unknown> = {}) {
  return {
    auth: {
      getAuthState: () => authState(),
      listPasskeys: vi.fn(() => ({ passkeys: [laptop, yubikey] })),
      listSessions: vi.fn(() => ({ sessions })),
      getTotp: vi.fn(() => ({ totp: create(TotpSchema, { enabled: true, recoveryCodesLeft: 7 }) })),
      ...overrides,
    },
  };
}

describe("ProfilePage", () => {
  it("lists passkeys with the address each works on, and sessions", async () => {
    renderAt("/profile", api());
    expect(await screen.findByRole("heading", { level: 1, name: "Profile and security" })).toBeVisible();
    expect(screen.getByText("Mira Chen · mira@acme.dev · signed in with a passkey")).toBeVisible();

    const keys = within(screen.getByRole("region", { name: "Passkeys" }));
    const [first, second] = await keys.findAllByRole("listitem");
    expect(first).toHaveTextContent("Chrome on macOS");
    expect(first).toHaveTextContent("Works here · Added 12 Mar · used just now · Synced");
    expect(second).toHaveTextContent("Works on panel.acme.dev · Added 4 Jun · never used");

    const list = within(screen.getByRole("region", { name: "Sessions" }));
    const [current, phone] = await list.findAllByRole("listitem");
    expect(current).toHaveTextContent("This browserCurrent");
    expect(current).toHaveTextContent("Chrome on macOS · 127.0.0.1 · localhost · now");
    expect(phone).toHaveTextContent("Safari on iOS · 198.51.100.23 · panel.acme.dev · 2 h ago");
    expect(within(current as HTMLElement).queryByRole("button")).toBeNull();

    expect(screen.getByRole("region", { name: "Authenticator app" })).toHaveTextContent(
      "7 of 10 recovery codes are left",
    );
  });

  it("signs out another session", async () => {
    const revokeSession = vi.fn(() => ({}));
    renderAt("/profile", api({ revokeSession }));
    await userEvent.click(await screen.findByRole("button", { name: "Sign out Safari on iOS" }));
    await waitFor(() =>
      expect(revokeSession).toHaveBeenCalledWith(expect.objectContaining({ id: "ses_2" }), expect.anything()),
    );
  });

  it("signs out everywhere else", async () => {
    const revokeOtherSessions = vi.fn(() => ({ revoked: 1 }));
    renderAt("/profile", api({ revokeOtherSessions }));
    await userEvent.click(await screen.findByRole("button", { name: "Sign out everywhere else" }));
    await waitFor(() => expect(revokeOtherSessions).toHaveBeenCalledOnce());
  });

  it("asks before removing a passkey, and explains when it's the last way in", async () => {
    const removePasskey = vi.fn(() => Promise.reject(authError(Code.FailedPrecondition, "last_way_in")));
    renderAt("/profile", api({ removePasskey }));
    await userEvent.click(await screen.findByRole("button", { name: "Remove YubiKey 5C" }));
    expect(removePasskey).not.toHaveBeenCalled();
    await userEvent.click(screen.getByRole("button", { name: "Remove" }));
    expect(await screen.findByRole("alert")).toHaveTextContent("That would leave you no way to sign in.");
    expect(removePasskey).toHaveBeenCalledWith(expect.objectContaining({ id: "pky_2" }), expect.anything());
  });

  it("adds a passkey for this address", async () => {
    const passkeys = stubPasskeys();
    const finishAddPasskey = vi.fn(() => ({}));
    renderAt(
      "/profile",
      api({ beginAddPasskey: () => ({ ceremonyId: "cer_9", optionsJson: creationOptions }), finishAddPasskey }),
    );
    await userEvent.click(await screen.findByRole("button", { name: "Add passkey" }));
    await waitFor(() => expect(finishAddPasskey).toHaveBeenCalledOnce());
    expect(passkeys.create).toHaveBeenCalledOnce();
  });

  it("asks for a passkey on a new address", async () => {
    stubPasskeys();
    renderAt(
      "/profile",
      api({ getAuthState: () => authState({ rpId: "panel.example.com", domain: "panel.example.com" }) }),
    );
    expect(await screen.findByText("No passkey works at panel.example.com yet")).toBeVisible();
  });

  it("warns that code sign-in is locked, and unlocks it", async () => {
    let locked = true;
    const unlockCodeSignIn = vi.fn(() => {
      locked = false;
      return {};
    });
    renderAt(
      "/profile",
      api({
        getTotp: () => ({
          totp: create(TotpSchema, {
            enabled: true,
            recoveryCodesLeft: 10,
            failedAttempts: 10,
            ...(locked ? { lockTime: minutesAgo(3) } : {}),
          }),
        }),
        unlockCodeSignIn,
      }),
    );
    const callout = await screen.findByRole("alert");
    expect(callout).toHaveTextContent("Code sign-in is locked");
    expect(callout).toHaveTextContent("Someone entered 10 wrong codes in a row for your account, the last 3 min ago.");
    await userEvent.click(within(callout).getByRole("button", { name: "Unlock" }));
    await waitFor(() => expect(screen.queryByText("Code sign-in is locked")).toBeNull());
    expect(unlockCodeSignIn).toHaveBeenCalledOnce();
  });

  it("sets up an authenticator app and shows the recovery codes once", async () => {
    const codes = Array.from({ length: 10 }, (_, i) => `7KQ2-M9XD-4TPA-ZR3${i}`);
    const confirmTotpSetup = vi.fn(() => ({ recoveryCodes: codes }));
    renderAt(
      "/profile",
      api({
        getTotp: () => ({ totp: create(TotpSchema, { enabled: false }) }),
        beginTotpSetup: () => ({
          secret: "JBSWY3DPEHPK3PXPJBSWY3DPEHPK3PXP",
          uri: "otpauth://totp/Dockwell:mira%40acme.dev?secret=JBSWY3DP",
        }),
        confirmTotpSetup,
      }),
    );
    await userEvent.click(await screen.findByRole("button", { name: "Set up" }));
    const sheet = await screen.findByRole("dialog", { name: "Set up an authenticator app" });
    expect(await within(sheet).findByRole("img", { name: "QR code for your authenticator app" })).toBeVisible();
    expect(within(sheet).getByText("JBSW Y3DP EHPK 3PXP JBSW Y3DP EHPK 3PXP")).toBeVisible();

    await userEvent.type(within(sheet).getByLabelText("Enter the 6-digit code it shows"), "123456");
    await userEvent.click(within(sheet).getByRole("button", { name: "Turn on" }));
    const saved = await screen.findByRole("dialog", { name: "Save your recovery codes" });
    expect(within(saved).getAllByRole("listitem")).toHaveLength(10);
    expect(within(saved).getByRole("link", { name: "Download" })).toHaveAttribute(
      "download",
      "dockwell-recovery-codes.txt",
    );
    expect(confirmTotpSetup).toHaveBeenCalledWith(expect.objectContaining({ code: "123456" }), expect.anything());
  });

  it("groups a secret in fours", () => {
    expect(groupSecret("JBSWY3DPEHPK3PXP")).toBe("JBSW Y3DP EHPK 3PXP");
  });

  it("is accessible", async () => {
    stubPasskeys();
    const { container } = renderAt("/profile", api());
    await screen.findAllByRole("listitem");
    await expectAccessible(container);
  });

  it("speaks Simplified Chinese", async () => {
    await activateLocale("zh-CN");
    renderAt("/profile", api());
    expect(await screen.findByRole("heading", { level: 1, name: "个人资料和安全" })).toBeVisible();
    expect(await screen.findByRole("region", { name: "通行密钥" })).toBeVisible();
  });
});
