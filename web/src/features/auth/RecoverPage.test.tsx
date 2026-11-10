// SPDX-License-Identifier: AGPL-3.0-only

import { Code } from "@connectrpc/connect";
import { screen, waitFor } from "@testing-library/react";
import { describe, expect, it, vi } from "vitest";

import { authError, authState } from "../../test/auth";
import { renderAt } from "../../test/render";

const TOKEN = "QmZb5b5lkz3qybB8_eMamqLWfYZ5hwF3ylkXVpIhX-c";

describe("RecoverPage", () => {
  it("signs in once with the link and asks for a passkey for this address", async () => {
    let signedIn = false;
    const signInWithLink = vi.fn(() => {
      signedIn = true;
      return {};
    });
    const { router } = renderAt(`/recover#${TOKEN}`, {
      auth: {
        getAuthState: () => authState(signedIn ? { signInMethod: 5 } : { user: undefined }),
        signInWithLink,
        listPasskeys: () => ({ passkeys: [] }),
        listSessions: () => ({ sessions: [] }),
        getTotp: () => ({ totp: { enabled: false } }),
      },
    });
    await waitFor(() => expect(router.state.location.pathname).toBe("/profile"));
    expect(signInWithLink).toHaveBeenCalledOnce();
    expect(signInWithLink).toHaveBeenCalledWith(expect.objectContaining({ token: TOKEN }), expect.anything());
    expect(await screen.findByText("You signed in with a one-time link")).toBeVisible();
  });

  it("reads the sign-in state again when the first read answers after the link signed in", async () => {
    // The page's first read of the sign-in state left before the link signed in, and
    // answers after it: it says nobody is signed in, which is no longer so.
    let signedIn = false;
    let reads = 0;
    let answerFirst = () => {};
    const firstRead = new Promise<void>((resolve) => {
      answerFirst = resolve;
    });
    // With the panel's 30 s of freshness: at 0 the gate would read again as it appears, and hide this.
    const api = {
      auth: {
        getAuthState: async () => {
          reads += 1;
          if (reads > 1) return authState(signedIn ? { signInMethod: 5 } : { user: undefined });
          await firstRead;
          return authState({ user: undefined });
        },
        signInWithLink: () => {
          signedIn = true;
          setTimeout(answerFirst, 20);
          return {};
        },
        listPasskeys: () => ({ passkeys: [] }),
        listSessions: () => ({ sessions: [] }),
        getTotp: () => ({ totp: { enabled: false } }),
      },
    };
    renderAt(`/recover#${TOKEN}`, api, { staleTime: 30_000 });
    expect(await screen.findByText("You signed in with a one-time link")).toBeVisible();
    expect(screen.queryByRole("heading", { level: 1, name: "Sign in to Dockwell" })).toBeNull();
  });

  it("explains a link that doesn't work", async () => {
    renderAt(`/recover#${TOKEN}`, {
      auth: {
        getAuthState: () => authState({ user: undefined }),
        signInWithLink: () => Promise.reject(authError(Code.PermissionDenied, "sign_in_link_rejected")),
      },
    });
    expect(await screen.findByRole("heading", { level: 1, name: "This sign-in link didn't work" })).toBeVisible();
    expect(screen.getByText(/already used, replaced by a newer one, or expired/)).toBeVisible();
    expect(screen.getByText("dockwell recover")).toBeVisible();
  });

  it("asks for the whole link", async () => {
    const signInWithLink = vi.fn();
    renderAt("/recover", { auth: { getAuthState: () => authState({ user: undefined }), signInWithLink } });
    expect(await screen.findByText(/including the part after “#”/)).toBeVisible();
    expect(signInWithLink).not.toHaveBeenCalled();
  });
});
