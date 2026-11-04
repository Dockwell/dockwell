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
