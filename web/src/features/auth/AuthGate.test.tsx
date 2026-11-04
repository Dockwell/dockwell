// SPDX-License-Identifier: AGPL-3.0-only

import { create } from "@bufbuild/protobuf";
import { Code, ConnectError } from "@connectrpc/connect";
import { GetVersionResponseSchema } from "@dockwell/api";
import { focusManager } from "@tanstack/react-query";
import { act, screen, waitFor, within } from "@testing-library/react";
import userEvent from "@testing-library/user-event";
import { afterEach, describe, expect, it, vi } from "vitest";

import { authError, authState } from "../../test/auth";
import { renderAt } from "../../test/render";

const system = { getVersion: () => create(GetVersionResponseSchema, {}) };

afterEach(() => {
  focusManager.setFocused(undefined);
});

/** What a browser does when someone comes back to the tab: queries that may be stale are read again. */
function refocus() {
  act(() => {
    focusManager.setFocused(false);
    focusManager.setFocused(true);
  });
}

describe("the sign-in gate", () => {
  it("shows the overview to someone signed in, in the shell", async () => {
    renderAt("/", { system, auth: { getAuthState: () => authState() } });
    expect(await screen.findByRole("heading", { level: 1, name: "Overview" })).toBeVisible();
    const rail = screen.getByRole("navigation", { name: "Main" });
    expect(within(rail).getByRole("link", { name: "Mira Chen Owner" })).toHaveAttribute("href", "/profile");
  });

  it.each(["/", "/servers", "/more", "/backups", "/profile", "/status"])(
    "keeps %s behind it, shell or not",
    async (path) => {
      const { router } = renderAt(path, { system, auth: { getAuthState: () => authState({ user: undefined }) } });
      expect(await screen.findByRole("heading", { level: 1, name: "Sign in to Dockwell" })).toBeVisible();
      expect(screen.queryByRole("navigation", { name: "Main" })).toBeNull();
      expect(router.state.location.pathname).toBe(path);
      expect(document.title).toBe("Sign in · Dockwell");
    },
  );

  it.each([
    ["/sign-in", authState({ user: undefined }), "Sign in to Dockwell", "Sign in · Dockwell"],
    ["/claim", authState({ claimed: false, user: undefined }), "Claim hz-1", "Claim hz-1 · Dockwell"],
    ["/recover", authState({ user: undefined }), "This sign-in link didn't work", "Sign in with a link · Dockwell"],
  ])("shows %s on its own, outside the shell", async (path, state, heading, title) => {
    renderAt(path, { system, auth: { getAuthState: () => state } });
    expect(await screen.findByRole("heading", { level: 1, name: heading })).toBeVisible();
    expect(screen.queryByRole("navigation", { name: "Main" })).toBeNull();
    expect(screen.queryByRole("button", { name: "Search or run a command" })).toBeNull();
    expect(document.title).toBe(title);
  });

  it("asks everyone else to sign in, in place of the page", async () => {
    const { router } = renderAt("/profile?via=link", {
      system,
      auth: { getAuthState: () => authState({ user: undefined }) },
    });
    expect(await screen.findByRole("heading", { level: 1, name: "Sign in to Dockwell" })).toBeVisible();
    expect(router.state.location.href).toBe("/profile?via=link");
    expect(screen.queryByRole("heading", { name: "Profile and security" })).toBeNull();
  });

  it("explains an install nobody has claimed", async () => {
    renderAt("/", { system, auth: { getAuthState: () => authState({ claimed: false, user: undefined }) } });
    expect(await screen.findByRole("heading", { level: 1, name: "hz-1 has no owner yet" })).toBeVisible();
    expect(screen.getByText("dockwell claim")).toBeVisible();
  });

  it("lets the claim and sign-in screens through", async () => {
    renderAt("/claim", { system, auth: { getAuthState: () => authState({ claimed: false, user: undefined }) } });
    expect(await screen.findByRole("heading", { level: 1, name: "Claim hz-1" })).toBeVisible();
  });

  it("says when the control plane doesn't answer", async () => {
    renderAt("/", {
      system,
      auth: {
        getAuthState: () => {
          throw new ConnectError("connection refused", Code.Unavailable);
        },
      },
    });
    expect(await screen.findByRole("heading", { level: 1, name: "Can't reach the control plane" })).toBeVisible();
  });

  it.each([
    ["misdirected", Code.Unknown, "The control plane doesn't answer at this address."],
    ["origin", Code.PermissionDenied, "it didn't come from the panel's own address."],
    ["unavailable", Code.Unavailable, "The control plane couldn't check your session just now."],
  ])("says why when the control plane answers %s", async (reason, code, text) => {
    renderAt("/", {
      system,
      auth: {
        getAuthState: () => {
          throw authError(code, reason);
        },
      },
    });
    expect(await screen.findByRole("heading", { level: 1, name: "Can't check your session" })).toBeVisible();
    expect(screen.getByText(text, { exact: false })).toBeVisible();
    expect(screen.queryByText("didn't answer the API", { exact: false })).toBeNull();
    expect(document.title).toBe("Can't check your session · Dockwell");
  });

  it("says why when a later check is answered with a refusal", async () => {
    let up = true;
    renderAt("/", {
      system,
      auth: {
        getAuthState: () => {
          if (!up) throw authError(Code.Unavailable, "unavailable");
          return authState();
        },
      },
    });
    expect(await screen.findByRole("heading", { level: 1, name: "Overview" })).toBeVisible();
    up = false;
    refocus();
    const notice = await screen.findByText("The control plane couldn't check your session just now.", {
      exact: false,
    });
    expect(notice.closest("[role=status]")).toHaveTextContent("Can't check your session");
    expect(screen.queryByText("Can't reach the control plane")).toBeNull();
  });

  it("keeps the page when a later check fails, and says it may be out of date", async () => {
    let up = true;
    const getAuthState = vi.fn(() => {
      if (!up) throw new ConnectError("connection refused", Code.Unavailable);
      return authState();
    });
    renderAt("/", { system, auth: { getAuthState } });
    expect(await screen.findByRole("heading", { level: 1, name: "Overview" })).toBeVisible();

    up = false;
    refocus();
    const notice = await screen.findByText("What you see may be out of date.", { exact: false });
    const stale = notice.closest<HTMLElement>("[role=status]");
    if (!stale) throw new Error("the stale notice isn't a status");
    expect(stale).toHaveTextContent("Can't reach the control plane");
    expect(screen.getByRole("heading", { level: 1, name: "Overview" })).toBeVisible();
    expect(screen.queryByRole("heading", { name: "Can't reach the control plane" })).toBeNull();

    up = true;
    await userEvent.click(within(stale).getByRole("button", { name: "Retry" }));
    await waitFor(() => expect(screen.queryByText("What you see may be out of date.", { exact: false })).toBeNull());
  });

  it("returns to sign-in when the session ends mid-use", async () => {
    let signedIn = true;
    const { router } = renderAt("/profile", {
      system,
      auth: {
        getAuthState: () => authState(signedIn ? {} : { user: undefined }),
        listPasskeys: () => {
          signedIn = false;
          throw authError(Code.Unauthenticated, "session_ended");
        },
        listSessions: () => ({ sessions: [] }),
        getTotp: () => ({}),
      },
    });
    expect(await screen.findByRole("heading", { level: 1, name: "Sign in to Dockwell" })).toBeVisible();
    await waitFor(() => expect(router.state.location.pathname).toBe("/profile"));
  });

  it("returns to sign-in when an action finds the session ended", async () => {
    let signedIn = true;
    renderAt("/profile", {
      system,
      auth: {
        getAuthState: () => authState(signedIn ? {} : { user: undefined }),
        listPasskeys: () => ({ passkeys: [{ id: "pky_1", name: "YubiKey 5C", rpId: "localhost" }] }),
        listSessions: () => ({ sessions: [] }),
        getTotp: () => ({}),
        removePasskey: () => {
          signedIn = false;
          throw authError(Code.Unauthenticated, "session_ended");
        },
      },
    });
    await userEvent.click(await screen.findByRole("button", { name: "Remove YubiKey 5C" }));
    await userEvent.click(screen.getByRole("button", { name: "Remove" }));
    expect(await screen.findByRole("heading", { level: 1, name: "Sign in to Dockwell" })).toBeVisible();
  });

  it("reads again when a session ends, rather than wait for a read that left before", async () => {
    let signedIn = true;
    // Each read answers as the session was when it arrived, but only once the test lets it, after the first.
    const held: Array<() => void> = [];
    const release = () => {
      for (const answer of held.splice(0)) answer();
    };
    const getAuthState = vi.fn(async () => {
      const state = authState(signedIn ? {} : { user: undefined });
      if (getAuthState.mock.calls.length > 1) await new Promise<void>((resolve) => held.push(resolve));
      return state;
    });
    renderAt("/profile", {
      system,
      auth: {
        getAuthState,
        listPasskeys: () => ({ passkeys: [{ id: "pky_1", name: "YubiKey 5C", rpId: "localhost" }] }),
        listSessions: () => ({ sessions: [] }),
        getTotp: () => ({}),
        removePasskey: () => {
          signedIn = false;
          throw authError(Code.Unauthenticated, "session_ended");
        },
      },
    });
    await userEvent.click(await screen.findByRole("button", { name: "Remove YubiKey 5C" }));
    // Coming back to the tab sends a read while the session is still there.
    refocus();
    await waitFor(() => expect(getAuthState.mock.calls.length).toBeGreaterThan(1));
    await userEvent.click(screen.getByRole("button", { name: "Remove" }));
    await screen.findByText("Your session ended", { exact: false });

    await waitFor(() => {
      release();
      expect(screen.getByRole("heading", { level: 1, name: "Sign in to Dockwell" })).toBeVisible();
    });
  });

  it("returns to sign-in when a stream's session ends", async () => {
    let signedIn = true;
    renderAt("/", {
      system,
      auth: { getAuthState: () => authState(signedIn ? {} : { user: undefined }) },
      servers: {
        // biome-ignore lint/correctness/useYield: it fails before its first message
        async *watchServers() {
          signedIn = false;
          throw authError(Code.Unauthenticated, "session_ended", "your session ended");
        },
      },
    });
    expect(await screen.findByRole("heading", { level: 1, name: "Sign in to Dockwell" })).toBeVisible();
    expect(screen.queryByText("your session ended")).toBeNull();
  });

  it("stays put when a code is wrong, which is unauthenticated too", async () => {
    const getAuthState = vi.fn(() => authState({ user: undefined }));
    renderAt("/", {
      system,
      auth: {
        getAuthState,
        signInWithCode: () => Promise.reject(authError(Code.Unauthenticated, "code_rejected")),
      },
    });
    await userEvent.click(await screen.findByRole("button", { name: "Use a code from your authenticator app" }));
    await userEvent.type(screen.getByLabelText("Email"), "mira@acme.dev");
    await userEvent.type(screen.getByLabelText("Code"), "123456");
    const before = getAuthState.mock.calls.length;
    await userEvent.click(screen.getByRole("button", { name: "Sign in" }));
    expect(await screen.findByRole("alert")).toHaveTextContent("That code didn't work.");
    await new Promise((resolve) => setTimeout(resolve, 50));
    expect(getAuthState).toHaveBeenCalledTimes(before);
  });
});
