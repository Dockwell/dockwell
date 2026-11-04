// SPDX-License-Identifier: AGPL-3.0-only

import { create } from "@bufbuild/protobuf";
import { Code, ConnectError } from "@connectrpc/connect";
import { GetVersionResponseSchema } from "@dockwell/api";
import { screen, waitFor } from "@testing-library/react";
import { describe, expect, it } from "vitest";

import { authState } from "../../test/auth";
import { renderAt } from "../../test/render";

const system = { getVersion: () => create(GetVersionResponseSchema, {}) };

describe("the sign-in gate", () => {
  it("shows the overview to someone signed in", async () => {
    renderAt("/", { system, auth: { getAuthState: () => authState() } });
    expect(await screen.findByRole("heading", { level: 1, name: "The control plane is running" })).toBeVisible();
    expect(screen.getByRole("region", { name: "Your account" })).toHaveTextContent(
      "Signed in as Mira Chen (mira@acme.dev)",
    );
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

  it("returns to sign-in when the session ends mid-use", async () => {
    let signedIn = true;
    const { router } = renderAt("/profile", {
      system,
      auth: {
        getAuthState: () => authState(signedIn ? {} : { user: undefined }),
        listPasskeys: () => {
          signedIn = false;
          throw new ConnectError("ended", Code.Unauthenticated);
        },
        listSessions: () => ({ sessions: [] }),
        getTotp: () => ({}),
      },
    });
    expect(await screen.findByRole("heading", { level: 1, name: "Sign in to Dockwell" })).toBeVisible();
    await waitFor(() => expect(router.state.location.pathname).toBe("/profile"));
  });
});
