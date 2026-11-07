// SPDX-License-Identifier: AGPL-3.0-only

import { create } from "@bufbuild/protobuf";
import { timestampFromDate } from "@bufbuild/protobuf/wkt";
import { Code, ConnectError } from "@connectrpc/connect";
import {
  type BeginAddPasskeyRequest,
  type BeginStepUpRequest,
  type BeginTotpSetupRequest,
  type FinishStepUpRequest,
  PasskeySchema,
  SessionSchema,
  SignInMethod,
  StepUpAction,
  TokenSchema,
  TokenScope,
  TotpSchema,
} from "@dockwell/api";
import { act, screen, waitFor, within } from "@testing-library/react";
import userEvent from "@testing-library/user-event";
import { describe, expect, it, vi } from "vitest";

import { activateLocale } from "../../app/i18n";
import { expectAccessible } from "../../test/a11y";
import { authError, authState, creationOptions, requestOptions, stubPasskeys } from "../../test/auth";
import { onPhone, renderAt } from "../../test/render";
import { groupSecret } from "./TotpPanel";

const minutesAgo = (m: number) => timestampFromDate(new Date(Date.now() - m * 60_000));

const laptop = create(PasskeySchema, {
  id: "pky_1",
  name: "Chrome on macOS",
  rpId: "localhost",
  createTime: timestampFromDate(new Date(2026, 2, 12)),
  lastUsedTime: minutesAgo(5),
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

const DAY = 86_400_000;

const deploy = create(TokenSchema, {
  id: "tok_1",
  name: "GitHub Actions deploy",
  hint: "dwt_4f9K…x9Ta",
  scope: TokenScope.READ_WRITE,
  createTime: timestampFromDate(new Date(2026, 7, 12)),
  lastUsedTime: minutesAgo(8),
  lastAddress: "203.0.113.9",
  expireTime: timestampFromDate(new Date(Date.now() + 9 * DAY + 3_600_000)),
});
const grafana = create(TokenSchema, {
  id: "tok_2",
  name: "Grafana",
  hint: "dwt_Ld7w…c08b",
  scope: TokenScope.READ_ONLY,
  createTime: timestampFromDate(new Date(2026, 7, 14)),
});

/** The proof the stand-in control plane gives a step-up for action. */
const proofFor = (action: StepUpAction) => `proof-${action}`;

/**
 * An action a step-up confirms, as the control plane answers it: refused
 * with needs_step_up without a proof, once nothing else refuses it, and
 * done with one.
 */
function confirmed<Req extends { proof: string }, Res>(done: (req: Req) => Res) {
  return vi.fn((req: Req) => {
    if (req.proof === "") throw authError(Code.PermissionDenied, "needs_step_up");
    return done(req);
  });
}

function api(overrides: Record<string, unknown> = {}, tokens: Record<string, unknown> = {}) {
  return {
    auth: {
      getAuthState: () => authState(),
      // A step-up answers with a proof for the action it began with.
      beginStepUp: vi.fn((req: BeginStepUpRequest) => ({
        ceremonyId: `cer-${req.action}`,
        optionsJson: requestOptions,
      })),
      finishStepUp: vi.fn((req: FinishStepUpRequest) => ({ proof: `proof-${req.ceremonyId.slice(4)}` })),
      listPasskeys: vi.fn(() => ({ passkeys: [laptop, yubikey] })),
      listSessions: vi.fn(() => ({ sessions })),
      getTotp: vi.fn(() => ({ totp: create(TotpSchema, { enabled: true, recoveryCodesLeft: 7 }) })),
      ...overrides,
    },
    tokens: {
      listTokens: vi.fn(() => ({ tokens: [deploy, grafana] })),
      ...tokens,
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
    expect(first).toHaveTextContent("Works here · Added 12 Mar · used 5 min ago · Synced");
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

  it("takes who is signed in from the gate, without reading it again", async () => {
    const getAuthState = vi.fn(() => authState());
    renderAt("/profile", api({ getAuthState }));
    await within(await screen.findByRole("region", { name: "Sessions" })).findAllByRole("listitem");
    expect(screen.getByText("Mira Chen · mira@acme.dev · signed in with a passkey")).toBeVisible();
    await new Promise((resolve) => setTimeout(resolve, 50));
    expect(getAuthState).toHaveBeenCalledOnce();
  });

  it("leaves out how you signed in when the panel doesn't know the way", async () => {
    renderAt("/profile", api({ getAuthState: () => authState({ signInMethod: SignInMethod.UNSPECIFIED }) }));
    expect(await screen.findByText("Mira Chen · mira@acme.dev", { exact: true })).toHaveClass("page-lead");
  });

  it("words times as the server pages do, and keeps them current", async () => {
    vi.useFakeTimers({ shouldAdvanceTime: true });
    try {
      const secondsAgo = (s: number) => timestampFromDate(new Date(Date.now() - s * 1_000));
      renderAt(
        "/profile",
        api({
          listPasskeys: () => ({ passkeys: [create(PasskeySchema, { ...laptop, lastUsedTime: secondsAgo(30) })] }),
          listSessions: () => ({ sessions: [sessions[0], { ...sessions[1], lastSeenTime: secondsAgo(40) }] }),
        }),
      );
      const keys = within(await screen.findByRole("region", { name: "Passkeys" }));
      const [key] = await keys.findAllByRole("listitem");
      expect(key).toHaveTextContent(/used 3\d s ago/);
      const list = within(screen.getByRole("region", { name: "Sessions" }));
      const [, phone] = await list.findAllByRole("listitem");
      expect(phone).toHaveTextContent(/panel\.acme\.dev · 4\d s ago/);

      await act(() => vi.advanceTimersByTimeAsync(60_000));
      expect(key).toHaveTextContent("used 1 min ago");
      expect(phone).toHaveTextContent("panel.acme.dev · 1 min ago");
    } finally {
      vi.useRealTimers();
    }
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

  it("asks before removing a passkey, and confirms it with one", async () => {
    const passkeys = stubPasskeys();
    const removePasskey = confirmed(() => ({}));
    const calls = api({ removePasskey });
    renderAt("/profile", calls);
    await userEvent.click(await screen.findByRole("button", { name: "Remove YubiKey 5C" }));
    expect(removePasskey).not.toHaveBeenCalled();
    await userEvent.click(screen.getByRole("button", { name: "Remove" }));
    await waitFor(() => expect(removePasskey).toHaveBeenCalledTimes(2));
    expect(calls.auth.beginStepUp).toHaveBeenCalledWith(
      expect.objectContaining({ action: StepUpAction.REMOVE_PASSKEY, target: "pky_2" }),
      expect.anything(),
    );
    expect(passkeys.get).toHaveBeenCalledOnce();
    expect(removePasskey).toHaveBeenLastCalledWith(
      expect.objectContaining({ id: "pky_2", proof: proofFor(StepUpAction.REMOVE_PASSKEY) }),
      expect.anything(),
    );
  });

  it("explains when a passkey is the last way in, without asking for one first", async () => {
    const passkeys = stubPasskeys();
    const removePasskey = vi.fn(() => Promise.reject(authError(Code.FailedPrecondition, "last_way_in")));
    const calls = api({ removePasskey });
    renderAt("/profile", calls);
    await userEvent.click(await screen.findByRole("button", { name: "Remove YubiKey 5C" }));
    await userEvent.click(screen.getByRole("button", { name: "Remove" }));
    expect(await screen.findByRole("alert")).toHaveTextContent("That would leave you no way to sign in.");
    expect(removePasskey).toHaveBeenCalledOnce();
    expect(removePasskey).toHaveBeenCalledWith(expect.objectContaining({ id: "pky_2", proof: "" }), expect.anything());
    expect(calls.auth.beginStepUp).not.toHaveBeenCalled();
    expect(passkeys.get).not.toHaveBeenCalled();
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

  it("confirms adding a passkey with one you have, when the control plane asks", async () => {
    const passkeys = stubPasskeys();
    const asked: BeginAddPasskeyRequest[] = [];
    const finishAddPasskey = vi.fn(() => ({}));
    const calls = api({
      beginAddPasskey: (req: BeginAddPasskeyRequest) => {
        asked.push(req);
        if (req.proof === "") throw authError(Code.PermissionDenied, "needs_step_up");
        return { ceremonyId: "cer_9", optionsJson: creationOptions };
      },
      finishAddPasskey,
    });
    renderAt("/profile", calls);
    await userEvent.click(await screen.findByRole("button", { name: "Add passkey" }));
    await waitFor(() => expect(finishAddPasskey).toHaveBeenCalledOnce());
    expect(asked.map((r) => r.proof)).toEqual(["", proofFor(StepUpAction.ADD_PASSKEY)]);
    expect(passkeys.get).toHaveBeenCalledOnce();
    expect(passkeys.create).toHaveBeenCalledOnce();
  });

  it("says to sign in again when no passkey here can confirm adding one", async () => {
    stubPasskeys();
    renderAt(
      "/profile",
      api({
        beginAddPasskey: () => Promise.reject(authError(Code.PermissionDenied, "needs_step_up")),
        beginStepUp: () => Promise.reject(authError(Code.FailedPrecondition, "needs_fresh_sign_in")),
      }),
    );
    await userEvent.click(await screen.findByRole("button", { name: "Add passkey" }));
    expect(await screen.findByRole("alert")).toHaveTextContent(
      "Sign out, sign in again with a code, a recovery code or a link from dockwell recover, then add it within 10 minutes.",
    );
  });

  it("asks for a passkey on a new address", async () => {
    stubPasskeys();
    renderAt(
      "/profile",
      api({ getAuthState: () => authState({ rpId: "panel.example.com", domain: "panel.example.com" }) }),
    );
    expect(await screen.findByText("No passkey works at panel.example.com yet")).toBeVisible();
    expect(screen.getByText("dockwell recover").parentElement).toHaveTextContent(
      "run dockwell recover on the server: its link signs you in here, so you can add a passkey with no code.",
    );
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
            ...(locked ? { lockTime: minutesAgo(3.5) } : {}),
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

  it("says why unlocking failed, and stays locked", async () => {
    const unlockCodeSignIn = vi.fn(() => Promise.reject(new ConnectError("connection refused", Code.Unavailable)));
    renderAt(
      "/profile",
      api({
        getTotp: () => ({
          totp: create(TotpSchema, {
            enabled: true,
            recoveryCodesLeft: 10,
            failedAttempts: 10,
            lockTime: minutesAgo(3),
          }),
        }),
        unlockCodeSignIn,
      }),
    );
    const callout = await screen.findByRole("alert");
    await userEvent.click(within(callout).getByRole("button", { name: "Unlock" }));
    await waitFor(() => expect(callout).toHaveTextContent("Connection refused."));
    expect(callout).toHaveTextContent("Code sign-in is locked");
  });

  it("signs out without emptying the page or asking again on its way out", async () => {
    let signedIn = true;
    const signOut = vi.fn(() => {
      signedIn = false;
      return {};
    });
    const calls = api({ getAuthState: () => authState(signedIn ? {} : { user: undefined }), signOut });
    renderAt("/profile", calls);
    await within(await screen.findByRole("region", { name: "Sessions" })).findAllByRole("listitem");
    const seen = new Set<string>();
    const watch = new MutationObserver(() => {
      if (document.body.textContent?.includes("Loading")) seen.add("loading");
    });
    watch.observe(document.body, { childList: true, subtree: true, characterData: true });
    await userEvent.click(screen.getByRole("button", { name: "Sign out" }));

    expect(await screen.findByRole("heading", { level: 1, name: "Sign in to Dockwell" })).toBeVisible();
    await new Promise((resolve) => setTimeout(resolve, 50));
    watch.disconnect();
    expect(seen).toEqual(new Set());
    // Asked once each, and never again without a session, which would only send the gate to check once more.
    expect(calls.auth.listPasskeys).toHaveBeenCalledOnce();
    expect(calls.auth.listSessions).toHaveBeenCalledOnce();
    expect(calls.auth.getTotp).toHaveBeenCalledOnce();
  });

  it("shows nothing from the session before after signing in again", async () => {
    stubPasskeys();
    let signedIn = true;
    let passkeys = [create(PasskeySchema, { id: "pky_0", name: "Old laptop", rpId: "localhost" })];
    const removePasskey = vi.fn(() => {
      signedIn = false;
      return Promise.reject(authError(Code.Unauthenticated, "session_ended"));
    });
    const signInWithCode = vi.fn(() => {
      signedIn = true;
      return {};
    });
    renderAt(
      "/profile",
      api({
        getAuthState: () => authState(signedIn ? {} : { user: undefined }),
        listPasskeys: () => ({ passkeys }),
        removePasskey,
        signInWithCode,
      }),
    );
    await userEvent.click(await screen.findByRole("button", { name: "Remove Old laptop" }));
    await userEvent.click(screen.getByRole("button", { name: "Remove" }));
    await screen.findByRole("heading", { level: 1, name: "Sign in to Dockwell" });

    passkeys = [yubikey];
    await userEvent.click(screen.getByRole("button", { name: "Use a code from your authenticator app" }));
    await userEvent.type(screen.getByLabelText("Email"), "mira@acme.dev");
    await userEvent.type(screen.getByLabelText("Code"), "123456");
    const seen = new Set<string>();
    const watch = new MutationObserver(() => {
      if (document.body.textContent?.includes("Old laptop")) seen.add("old passkey");
    });
    watch.observe(document.body, { childList: true, subtree: true, characterData: true });
    await userEvent.click(screen.getByRole("button", { name: "Sign in" }));

    const keys = within(await screen.findByRole("region", { name: "Passkeys" }));
    expect(await keys.findByText("YubiKey 5C")).toBeVisible();
    watch.disconnect();
    expect(seen).toEqual(new Set());
  });

  it("says why signing out failed, and stays signed in", async () => {
    const signOut = vi.fn(() => Promise.reject(new ConnectError("connection refused", Code.Unavailable)));
    renderAt("/profile", api({ signOut }));
    await userEvent.click(await screen.findByRole("button", { name: "Sign out" }));
    expect(await screen.findByRole("alert")).toHaveTextContent("Connection refused.");
    expect(screen.getByRole("heading", { level: 1, name: "Profile and security" })).toBeVisible();
  });

  it("sits in the shell, reached from you at the foot of the rail", async () => {
    const { router } = renderAt("/", api());
    await screen.findByRole("heading", { level: 1, name: "Overview" });
    const rail = screen.getByRole("navigation", { name: "Main" });
    await userEvent.click(within(rail).getByRole("link", { name: "Mira Chen Owner" }));

    await waitFor(() => expect(router.state.location.pathname).toBe("/profile"));
    expect(await screen.findByRole("heading", { level: 1, name: "Profile and security" })).toBeVisible();
    expect(within(rail).getByRole("link", { name: "Mira Chen Owner" })).toHaveAttribute("aria-current", "page");
    expect(within(rail).getByRole("link", { name: "Overview" })).not.toHaveAttribute("aria-current");
    expect(screen.getByRole("main")).toContainElement(screen.getByRole("heading", { level: 1 }));
    await waitFor(() => expect(screen.getByRole("main")).toHaveFocus());
    expect(document.title).toBe("Profile and security · Dockwell");
  });

  it("is under More on phones, with a way back to Settings, which it's a section of", async () => {
    onPhone();
    const { router } = renderAt("/more", api());
    await userEvent.click(await screen.findByRole("link", { name: "Profile and security Mira Chen" }));

    await waitFor(() => expect(router.state.location.pathname).toBe("/profile"));
    expect(await screen.findByRole("heading", { level: 1, name: "Profile and security" })).toBeVisible();
    const tabs = screen.getByRole("navigation", { name: "Main" });
    expect(within(tabs).getByRole("link", { name: "More" })).toHaveAttribute("aria-current", "page");
    expect(screen.getByRole("link", { name: "Back" })).toHaveAttribute("href", "/settings");
    // Signing out is on More, as the phone mock has it, not in the header.
    expect(screen.queryByRole("button", { name: "Sign out" })).toBeNull();
  });

  it("signs out from More on phones, then asks to sign in", async () => {
    onPhone();
    let signedIn = true;
    const signOut = vi.fn(() => {
      signedIn = false;
      return {};
    });
    const { router } = renderAt(
      "/more",
      api({ getAuthState: () => authState(signedIn ? {} : { user: undefined }), signOut }),
    );
    await userEvent.click(await screen.findByRole("button", { name: "Sign out" }));

    expect(await screen.findByRole("heading", { level: 1, name: "Sign in to Dockwell" })).toBeVisible();
    expect(signOut).toHaveBeenCalledOnce();
    expect(screen.queryByRole("navigation", { name: "Main" })).toBeNull();
    expect(router.state.location.pathname).toBe("/more");
  });

  it("says on More why signing out failed, and stays signed in", async () => {
    onPhone();
    const signOut = vi.fn(() => Promise.reject(new ConnectError("connection refused", Code.Unavailable)));
    renderAt("/more", api({ signOut }));
    await userEvent.click(await screen.findByRole("button", { name: "Sign out" }));
    expect(await screen.findByRole("alert")).toHaveTextContent("Connection refused.");
    expect(screen.getByRole("heading", { level: 1, name: "More" })).toBeVisible();
  });

  it("sets up an authenticator app, confirmed with a passkey, and shows the recovery codes once", async () => {
    stubPasskeys();
    const codes = Array.from({ length: 10 }, (_, i) => `7KQ2-M9XD-4TPA-ZR3${i}`);
    const confirmTotpSetup = vi.fn(() => ({ recoveryCodes: codes }));
    const cancelTotpSetup = vi.fn(() => ({}));
    const beginTotpSetup = confirmed((_req: BeginTotpSetupRequest) => ({
      secret: "JBSWY3DPEHPK3PXPJBSWY3DPEHPK3PXP",
      uri: "otpauth://totp/Dockwell:mira%40acme.dev?secret=JBSWY3DP",
    }));
    renderAt(
      "/profile",
      api({
        getTotp: () => ({ totp: create(TotpSchema, { enabled: false }) }),
        beginTotpSetup,
        confirmTotpSetup,
        cancelTotpSetup,
      }),
    );
    await userEvent.click(await screen.findByRole("button", { name: "Set up" }));
    const sheet = await screen.findByRole("dialog", { name: "Set up an authenticator app" });
    expect(await within(sheet).findByRole("img", { name: "QR code for your authenticator app" })).toBeVisible();
    expect(within(sheet).getByText("JBSW Y3DP EHPK 3PXP JBSW Y3DP EHPK 3PXP")).toBeVisible();

    await userEvent.type(within(sheet).getByLabelText("Enter the 6-digit code it shows"), "123\u00a0456");
    await userEvent.click(within(sheet).getByRole("button", { name: "Turn on" }));
    const saved = await screen.findByRole("dialog", { name: "Save your recovery codes" });
    expect(within(saved).getAllByRole("listitem")).toHaveLength(10);
    const download = within(saved).getByRole("link", { name: "Download" });
    expect(download).toHaveAttribute("download", "dockwell-recovery-codes.txt");
    expect(download).toHaveClass("dw-btn", "dw-btn-secondary", "dw-btn-sm");
    expect(confirmTotpSetup).toHaveBeenCalledWith(expect.objectContaining({ code: "123456" }), expect.anything());
    expect(beginTotpSetup.mock.calls.map((c) => c[0].proof)).toEqual(["", proofFor(StepUpAction.SET_UP_CODES)]);
    await userEvent.click(within(saved).getByRole("button", { name: "I saved them" }));
    await waitFor(() => expect(screen.queryByRole("dialog")).toBeNull());
    // Finished, so there's nothing to forget.
    expect(cancelTotpSetup).not.toHaveBeenCalled();
  });

  it("forgets the new key when you close the setup before finishing it", async () => {
    stubPasskeys();
    const cancelTotpSetup = vi.fn(() => ({}));
    renderAt(
      "/profile",
      api({
        getTotp: () => ({ totp: create(TotpSchema, { enabled: false }) }),
        beginTotpSetup: confirmed(() => ({
          secret: "JBSWY3DPEHPK3PXP",
          uri: "otpauth://totp/Dockwell?secret=JBSWY3DP",
        })),
        cancelTotpSetup,
      }),
    );
    await userEvent.click(await screen.findByRole("button", { name: "Set up" }));
    const sheet = await screen.findByRole("dialog", { name: "Set up an authenticator app" });
    expect(cancelTotpSetup).not.toHaveBeenCalled();
    await userEvent.click(within(sheet).getByRole("button", { name: "Close" }));
    await waitFor(() => expect(cancelTotpSetup).toHaveBeenCalledOnce());
    await waitFor(() => expect(screen.queryByRole("dialog")).toBeNull());
  });

  it("says when wrong codes ended the setup, and that it takes the passkey again", async () => {
    stubPasskeys();
    renderAt(
      "/profile",
      api({
        getTotp: () => ({ totp: create(TotpSchema, { enabled: false }) }),
        beginTotpSetup: confirmed(() => ({
          secret: "JBSWY3DPEHPK3PXP",
          uri: "otpauth://totp/Dockwell?secret=JBSWY3DP",
        })),
        confirmTotpSetup: () => Promise.reject(authError(Code.FailedPrecondition, "totp_setup_ended")),
        cancelTotpSetup: () => ({}),
      }),
    );
    await userEvent.click(await screen.findByRole("button", { name: "Set up" }));
    const sheet = await screen.findByRole("dialog", { name: "Set up an authenticator app" });
    await userEvent.type(within(sheet).getByLabelText("Enter the 6-digit code it shows"), "123456");
    await userEvent.click(within(sheet).getByRole("button", { name: "Turn on" }));
    expect(await within(sheet).findByRole("alert")).toHaveTextContent(
      "That was the last try. 5 wrong codes ended this setup, and its key no longer works.",
    );
  });

  it("confirms turning codes off with a passkey", async () => {
    stubPasskeys();
    const disableTotp = confirmed(() => ({}));
    renderAt("/profile", api({ disableTotp }));
    await userEvent.click(await screen.findByRole("button", { name: "Turn off" }));
    await userEvent.click(screen.getByRole("button", { name: "Turn off and delete codes" }));
    await waitFor(() =>
      expect(disableTotp).toHaveBeenCalledWith(
        expect.objectContaining({ proof: proofFor(StepUpAction.DISABLE_CODES) }),
        expect.anything(),
      ),
    );
  });

  it("copies the recovery codes, says so to screen readers, and is ready to copy again", async () => {
    vi.useFakeTimers({ shouldAdvanceTime: true });
    try {
      stubPasskeys();
      const writeText = vi.fn(async () => {});
      vi.stubGlobal("navigator", { ...navigator, clipboard: { writeText } });
      const codes = Array.from({ length: 10 }, (_, i) => `7KQ2-M9XD-4TPA-ZR3${i}`);
      const regenerateRecoveryCodes = confirmed(() => ({ recoveryCodes: codes }));
      renderAt("/profile", api({ regenerateRecoveryCodes }));
      await userEvent.click(await screen.findByRole("button", { name: "New recovery codes" }));
      await userEvent.click(await screen.findByRole("button", { name: "Replace codes" }));
      const saved = await screen.findByRole("dialog", { name: "Save your recovery codes" });
      expect(regenerateRecoveryCodes).toHaveBeenCalledWith(
        expect.objectContaining({ proof: proofFor(StepUpAction.REGENERATE_RECOVERY_CODES) }),
        expect.anything(),
      );

      await userEvent.click(within(saved).getByRole("button", { name: "Copy codes" }));
      expect(writeText).toHaveBeenCalledWith(codes.join("\n"));
      expect(within(saved).getByRole("button", { name: "Codes copied" })).toBeVisible();
      const live = saved.querySelector("[aria-live=polite]");
      expect(live).toHaveTextContent("Codes copied");

      await act(() => vi.advanceTimersByTimeAsync(2_000));
      expect(within(saved).getByRole("button", { name: "Copy codes" })).toBeVisible();
      expect(live).toHaveTextContent("");
    } finally {
      vi.useRealTimers();
    }
  });

  it("keeps the key you scanned while the sheet is open, whatever the language does", async () => {
    stubPasskeys();
    let made = 0;
    const beginTotpSetup = vi.fn(() => {
      made += 1;
      return { secret: `JBSWY3DPEHPK3PX${made}`, uri: `otpauth://totp/Dockwell:mira%40acme.dev?secret=${made}` };
    });
    renderAt("/profile", api({ getTotp: () => ({ totp: create(TotpSchema, { enabled: false }) }), beginTotpSetup }));
    await userEvent.click(await screen.findByRole("button", { name: "Set up" }));
    const sheet = await screen.findByRole("dialog", { name: "Set up an authenticator app" });
    expect(await within(sheet).findByText("JBSW Y3DP EHPK 3PX1")).toBeVisible();

    await act(() => activateLocale("zh-CN"));
    expect(await screen.findByRole("dialog", { name: "设置身份验证器应用" })).toBeVisible();
    await new Promise((resolve) => setTimeout(resolve, 50));
    expect(beginTotpSetup).toHaveBeenCalledOnce();
    expect(screen.getByText("JBSW Y3DP EHPK 3PX1")).toBeVisible();
  });

  it("says why the authenticator app's setup couldn't start", async () => {
    stubPasskeys();
    renderAt(
      "/profile",
      api({
        getTotp: () => ({ totp: create(TotpSchema, { enabled: false }) }),
        beginTotpSetup: () => Promise.reject(new ConnectError("connection refused", Code.Unavailable)),
      }),
    );
    await userEvent.click(await screen.findByRole("button", { name: "Set up" }));
    const panel = within(await screen.findByRole("region", { name: "Authenticator app" }));
    expect(await panel.findByRole("alert")).toHaveTextContent("Connection refused.");
    expect(screen.queryByRole("dialog")).toBeNull();
  });

  it("points to API tokens in Settings, saying how many act for you", async () => {
    renderAt("/profile", api());
    const panel = within(await screen.findByRole("region", { name: "API tokens" }));
    expect(await panel.findByText(/2 act for you, and 1 expires within two weeks\./)).toBeVisible();
    expect(panel.getByRole("link", { name: "Manage" })).toHaveAttribute("href", "/settings/api-tokens");
    expect(panel.getByRole("link", { name: "Settings, under API tokens" })).toHaveAttribute(
      "href",
      "/settings/api-tokens",
    );
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
