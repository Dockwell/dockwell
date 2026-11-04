// SPDX-License-Identifier: AGPL-3.0-only

import { create } from "@bufbuild/protobuf";
import { Code } from "@connectrpc/connect";
import { GetVersionResponseSchema } from "@dockwell/api";
import { createRouter } from "@tanstack/react-router";
import { screen, waitFor } from "@testing-library/react";
import userEvent from "@testing-library/user-event";
import { describe, expect, it, vi } from "vitest";

import { activateLocale } from "../../app/i18n";
import { routeTree } from "../../routeTree.gen";
import { expectAccessible } from "../../test/a11y";
import { authError, authState, requestOptions, stubPasskeys } from "../../test/auth";
import { renderAt } from "../../test/render";
import { safeNext } from "./useAuthState";

function api(overrides: Record<string, unknown> = {}) {
  let signedIn = false;
  const auth = {
    getAuthState: () => authState(signedIn ? {} : { user: undefined }),
    beginPasskeySignIn: vi.fn(() => ({ ceremonyId: "cer_1", optionsJson: requestOptions })),
    finishPasskeySignIn: vi.fn(() => {
      signedIn = true;
      return {};
    }),
    signInWithCode: vi.fn(() => {
      signedIn = true;
      return {};
    }),
    ...overrides,
  };
  return { system: { getVersion: () => create(GetVersionResponseSchema, { build: { version: "0.9.4" } }) }, auth };
}

describe("SignInPage", () => {
  it("follows the mock: passkey first, another device second, and how to recover", async () => {
    stubPasskeys();
    renderAt("/sign-in", api());
    expect(await screen.findByRole("heading", { level: 1, name: "Sign in to Dockwell" })).toBeVisible();
    await waitFor(() =>
      expect(screen.getByRole("heading", { level: 1 }).nextElementSibling).toHaveTextContent(
        `${window.location.host} · the panel for hz-1`,
      ),
    );
    expect(screen.getByRole("button", { name: "Sign in with a passkey" })).toBeEnabled();
    expect(screen.getByRole("button", { name: "Use a security key or another device" })).toBeEnabled();
    expect(screen.getByRole("heading", { name: "Lost your passkey?" })).toBeVisible();
    expect(screen.getByText("dockwell recover")).toBeVisible();
    expect(await screen.findByText("0.9.4")).toBeVisible();
  });

  it("signs in with a passkey and goes where the visit was headed", async () => {
    const passkeys = stubPasskeys();
    const calls = api();
    const { router } = renderAt("/sign-in?next=%2Fprofile", calls);
    await userEvent.click(await screen.findByRole("button", { name: "Sign in with a passkey" }));
    await waitFor(() => expect(router.state.location.pathname).toBe("/profile"));
    const options = passkeys.get.mock.calls[0]?.[0].publicKey as { hints?: string[] };
    expect(options.hints).toEqual(["client-device"]);
    expect(calls.auth.finishPasskeySignIn).toHaveBeenCalledWith(
      expect.objectContaining({ ceremonyId: "cer_1" }),
      expect.anything(),
    );
  });

  it("offers a security key or a phone on the second button", async () => {
    const passkeys = stubPasskeys();
    renderAt("/sign-in", api());
    await userEvent.click(await screen.findByRole("button", { name: "Use a security key or another device" }));
    await waitFor(() => expect(passkeys.get).toHaveBeenCalled());
    const publicKey = passkeys.get.mock.calls[0]?.[0]?.publicKey as { hints?: string[] } | undefined;
    expect(publicKey?.hints).toEqual(["security-key", "hybrid"]);
  });

  it("explains on a new domain why a passkey from the tunnel isn't offered", async () => {
    stubPasskeys({ get: "NotAllowedError" });
    renderAt(
      "/sign-in",
      api({ getAuthState: () => authState({ user: undefined, rpId: "panel.acme.dev", domain: "panel.acme.dev" }) }),
    );
    await userEvent.click(await screen.findByRole("button", { name: "Sign in with a passkey" }));
    expect(await screen.findByRole("status")).toHaveTextContent(
      "No passkey for this address? Passkeys belong to the address they were made on",
    );
  });

  it("explains a refused passkey", async () => {
    stubPasskeys();
    renderAt(
      "/sign-in",
      api({ finishPasskeySignIn: () => Promise.reject(authError(Code.Unauthenticated, "passkey_rejected")) }),
    );
    await userEvent.click(await screen.findByRole("button", { name: "Sign in with a passkey" }));
    expect(await screen.findByRole("alert")).toHaveTextContent("That passkey isn't registered for this address");
  });

  it("signs in with a code from an authenticator app", async () => {
    stubPasskeys();
    const calls = api();
    const { router } = renderAt("/sign-in", calls);
    await userEvent.click(await screen.findByRole("button", { name: "Use a code from your authenticator app" }));
    await userEvent.type(screen.getByLabelText("Email"), "mira@acme.dev");
    await userEvent.type(screen.getByLabelText("Code"), "123 456");
    await userEvent.click(screen.getByRole("button", { name: "Sign in" }));
    await waitFor(() => expect(router.state.location.pathname).toBe("/"));
    expect(calls.auth.signInWithCode).toHaveBeenCalledWith(
      expect.objectContaining({ email: "mira@acme.dev", code: "123456" }),
      expect.anything(),
    );
  });

  it("says how long to wait after too many wrong codes", async () => {
    stubPasskeys();
    renderAt(
      "/sign-in",
      api({ signInWithCode: () => Promise.reject(authError(Code.ResourceExhausted, "slow_down", "x", 32)) }),
    );
    await userEvent.click(await screen.findByRole("button", { name: "Use a code from your authenticator app" }));
    await userEvent.type(screen.getByLabelText("Email"), "mira@acme.dev");
    await userEvent.type(screen.getByLabelText("Code"), "123456");
    await userEvent.click(screen.getByRole("button", { name: "Sign in" }));
    expect(await screen.findByRole("alert")).toHaveTextContent("Too many wrong codes. Wait 32 s, then try again.");
  });

  it("checks a code's shape before sending it", async () => {
    stubPasskeys();
    const calls = api();
    renderAt("/sign-in", calls);
    await userEvent.click(await screen.findByRole("button", { name: "Use a code from your authenticator app" }));
    await userEvent.type(screen.getByLabelText("Code"), "12345");
    await userEvent.click(screen.getByRole("button", { name: "Sign in" }));
    expect(screen.getByText("Enter the 6-digit code from your authenticator app, or a recovery code.")).toBeVisible();
    expect(calls.auth.signInWithCode).not.toHaveBeenCalled();
  });

  it("sends a pasted code as it checked it, in the alphabet recovery codes use", async () => {
    stubPasskeys();
    const calls = api({ signInWithCode: vi.fn(() => ({ usedRecoveryCode: true })) });
    renderAt("/sign-in", calls);
    await userEvent.click(await screen.findByRole("button", { name: "Use a code from your authenticator app" }));
    await userEvent.type(screen.getByLabelText("Email"), "mira@acme.dev");
    // Copied from a note: a non-breaking space, an en dash, small letters, and an O where a 0 was meant.
    await userEvent.click(screen.getByLabelText("Code"));
    await userEvent.paste("7kq2\u00a0m9xd\u20134tpa-zr3o");
    await userEvent.click(screen.getByRole("button", { name: "Sign in" }));
    await waitFor(() =>
      expect(calls.auth.signInWithCode).toHaveBeenCalledWith(
        expect.objectContaining({ code: "7KQ2M9XD4TPAZR30" }),
        expect.anything(),
      ),
    );
  });

  it("refuses a letter recovery codes never have, before sending it", async () => {
    stubPasskeys();
    const calls = api();
    renderAt("/sign-in", calls);
    await userEvent.click(await screen.findByRole("button", { name: "Use a code from your authenticator app" }));
    await userEvent.type(screen.getByLabelText("Email"), "mira@acme.dev");
    await userEvent.type(screen.getByLabelText("Code"), "7KQ2-M9XD-4TPA-ZR3U");
    await userEvent.click(screen.getByRole("button", { name: "Sign in" }));
    expect(screen.getByText("Enter the 6-digit code from your authenticator app, or a recovery code.")).toBeVisible();
    expect(calls.auth.signInWithCode).not.toHaveBeenCalled();
  });

  /** An API where a recovery code signs in, and the profile has nothing to list. */
  function recoveryApi() {
    let signedIn = false;
    return {
      ...api(),
      auth: {
        getAuthState: () => authState(signedIn ? {} : { user: undefined }),
        signInWithCode: vi.fn(() => {
          signedIn = true;
          return { usedRecoveryCode: true, recoveryCodesLeft: 9 };
        }),
        listPasskeys: () => ({}),
        listSessions: () => ({}),
        getTotp: () => ({}),
      },
    };
  }

  async function signInWithRecoveryCode() {
    await userEvent.click(await screen.findByRole("button", { name: "Use a code from your authenticator app" }));
    await userEvent.type(screen.getByLabelText("Email"), "mira@acme.dev");
    await userEvent.type(screen.getByLabelText("Code"), "7KQ2-M9XD-4TPA-ZR3C");
    await userEvent.click(screen.getByRole("button", { name: "Sign in" }));
  }

  it("sends a recovery code sign-in to the profile", async () => {
    stubPasskeys();
    const { router } = renderAt("/sign-in?next=%2F", recoveryApi());
    await signInWithRecoveryCode();
    expect(await screen.findByRole("heading", { level: 1, name: "Profile and security" })).toBeVisible();
    expect(router.state.location.pathname).toBe("/profile");
    expect(router.state.location.search).toEqual({ via: "recovery-code" });
  });

  it("sends a recovery code sign-in to the profile, never where the visit was headed", async () => {
    stubPasskeys();
    const { router } = renderAt("/sign-in?next=%2Fstatus", recoveryApi());
    const visited: string[] = [];
    router.subscribe("onBeforeNavigate", (e) => visited.push(e.toLocation.pathname));
    // The profile's code loads on demand, so getting there can take a moment,
    // in which the page sees the new sign-in state.
    const navigate = router.navigate.bind(router);
    router.navigate = async (opts) => {
      if (opts.to === "/profile") await new Promise((resolve) => setTimeout(resolve, 50));
      return navigate(opts);
    };
    await signInWithRecoveryCode();
    expect(await screen.findByRole("heading", { level: 1, name: "Profile and security" })).toBeVisible();
    // Long enough for a second navigation to land, if there were one.
    await new Promise((resolve) => setTimeout(resolve, 50));
    expect(router.state.location.pathname).toBe("/profile");
    expect(visited).not.toContain("/status");
  });

  it("explains why a call was refused for want of a session", async () => {
    stubPasskeys();
    for (const [reason, text] of [
      ["session_ended", "Your session ended, so this didn't happen. Sign in again, then try again."],
      ["unauthenticated", "You're not signed in. Sign in, then try again."],
      ["origin", "The control plane refused this because it didn't come from the panel's own address."],
    ] as const) {
      const { unmount } = renderAt(
        "/sign-in",
        api({ beginPasskeySignIn: () => Promise.reject(authError(Code.Unauthenticated, reason)) }),
      );
      await userEvent.click(await screen.findByRole("button", { name: "Sign in with a passkey" }));
      expect(await screen.findByRole("alert")).toHaveTextContent(text);
      unmount();
    }
  });

  it("never sends the visit to another site", () => {
    expect(safeNext("/servers/hz-1?x=1")).toBe("/servers/hz-1?x=1");
    for (const bad of ["https://evil.test", "//evil.test", "/\\evil.test", undefined, 3]) {
      expect(safeNext(bad)).toBe("/");
    }
  });

  it("never sends the visit back to a sign-in screen", () => {
    for (const screen of [
      "/sign-in?next=%2Fservers",
      "/claim#token",
      "/recover",
      "/Sign-In/",
      "/%73ign-in",
      "/x/../claim",
    ]) {
      expect(safeNext(screen)).toBe("/");
    }
    expect(safeNext("/sign-ins")).toBe("/sign-ins");
    expect(safeNext("/%E0%A4%A")).toBe("/%E0%A4%A");
  });

  it("knows every screen that stands outside the gate", () => {
    const router = createRouter({ routeTree });
    const open = Object.values(router.routesById).filter((route) => route.options.staticData?.public);
    expect(open.map((route) => route.fullPath).sort()).toEqual(["/claim", "/recover", "/sign-in"]);
    for (const route of open) expect(safeNext(route.fullPath)).toBe("/");
  });

  it("goes to the overview after signing in on a link to a sign-in screen", async () => {
    stubPasskeys();
    const { router } = renderAt("/sign-in?next=%2Fclaim", api());
    await userEvent.click(await screen.findByRole("button", { name: "Sign in with a passkey" }));
    expect(await screen.findByRole("heading", { level: 1, name: "Overview" })).toBeVisible();
    expect(router.state.location.pathname).toBe("/");
  });

  it("is accessible", async () => {
    stubPasskeys();
    const { container } = renderAt("/sign-in", api());
    await screen.findByText("0.9.4");
    await expectAccessible(container);
    await userEvent.click(screen.getByRole("button", { name: "Use a code from your authenticator app" }));
    await expectAccessible(container);
  });

  it("speaks Simplified Chinese", async () => {
    await activateLocale("zh-CN");
    stubPasskeys();
    renderAt("/sign-in", api());
    expect(await screen.findByRole("heading", { level: 1, name: "登录 Dockwell" })).toBeVisible();
    expect(screen.getByRole("button", { name: "使用通行密钥登录" })).toBeVisible();
  });
});
