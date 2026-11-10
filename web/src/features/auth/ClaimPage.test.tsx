// SPDX-License-Identifier: AGPL-3.0-only

import { create } from "@bufbuild/protobuf";
import { timestampFromDate } from "@bufbuild/protobuf/wkt";
import { Code } from "@connectrpc/connect";
import {
  BuildInfoSchema,
  CheckClaimResponseSchema,
  Connection,
  GetVersionResponseSchema,
  ServerInventorySchema,
  SignInMethod,
} from "@dockwell/api";
import { act, screen, waitFor } from "@testing-library/react";
import userEvent from "@testing-library/user-event";
import { describe, expect, it, vi } from "vitest";

import { activateLocale } from "../../app/i18n";
import { expectAccessible } from "../../test/a11y";
import { authError, authState, creationOptions, stubPasskeys } from "../../test/auth";
import { renderAt } from "../../test/render";
import { aServer } from "../../test/servers";
import { tokenFrom } from "./ClaimPage";

const GiB = 1024 ** 3;
const TOKEN = "KsdPgWhdOz_WgRAMKAU2A8K-bec56b1zdwQ2RdPmCFQ";

const checked = () =>
  create(CheckClaimResponseSchema, {
    server: aServer({
      id: "srv_1",
      name: "hz-1",
      connection: Connection.CONNECTED,
      createTime: timestampFromDate(new Date(Date.now() - 2 * 60_000)),
      agentBuild: create(BuildInfoSchema, { version: "0.9.4" }),
      inventory: create(ServerInventorySchema, {
        osName: "Ubuntu 24.04 LTS",
        cpuCount: 4,
        memoryBytes: BigInt(8 * GiB),
        disks: [{ mountPoint: "/", sizeBytes: BigInt(160 * GiB) }],
      }),
    }),
    publicAddress: "49.12.33.7",
    expireTime: timestampFromDate(new Date(Date.now() + 23.5 * 3_600_000)),
  });

function api(overrides: Record<string, unknown> = {}) {
  return {
    system: { getVersion: () => create(GetVersionResponseSchema, { build: { version: "0.9.4" } }) },
    auth: {
      getAuthState: () => authState({ claimed: false, user: undefined }),
      checkClaim: vi.fn(checked),
      beginClaim: vi.fn(() => ({ ceremonyId: "cer_1", optionsJson: creationOptions })),
      finishClaim: vi.fn(() => ({})),
      ...overrides,
    },
  };
}

describe("ClaimPage", () => {
  it("reads the token from the link and describes the server", async () => {
    const calls = api();
    const { router } = renderAt(`/claim#${TOKEN}`, calls);
    expect(await screen.findByRole("heading", { level: 1, name: "Claim hz-1" })).toBeVisible();
    // The code came in the link and works: nothing to type, and another is a click away.
    expect(await screen.findByText("Claim link works")).toBeVisible();
    expect(screen.queryByLabelText("Claim code")).toBeNull();
    // The token leaves the address bar once read.
    await waitFor(() => expect(router.state.location.hash).toBe(""));

    const server = await screen.findByRole("region", { name: "This server" });
    expect(server).toHaveTextContent("Agent connected");
    expect(server).toHaveTextContent("49.12.33.7");
    expect(server).toHaveTextContent("Ubuntu 24.04 LTS");
    expect(server).toHaveTextContent("4 vCPU · 8 GB · 160 GB disk");
    expect(server).toHaveTextContent("0.9.4");
    expect(server).toHaveTextContent("2 min ago");
    expect(server).toHaveTextContent("in 23 h");
    expect(calls.auth.checkClaim).toHaveBeenCalledWith(expect.objectContaining({ token: TOKEN }), expect.anything());
    expect(screen.getByLabelText("Panel address")).toHaveValue(window.location.host);
    expect(screen.getByRole("region", { name: "What happens next" })).toHaveTextContent("Claim this server");
    await userEvent.click(screen.getByRole("button", { name: "Use another code" }));
    expect(screen.getByLabelText("Claim code")).toHaveValue(TOKEN);
  });

  it("creates the owner with a passkey and goes to where the first app comes from", async () => {
    const passkeys = stubPasskeys();
    let claimed = false;
    const calls = api({
      // Signed in once the claim finishes.
      getAuthState: () => (claimed ? authState() : authState({ claimed: false, user: undefined })),
      finishClaim: vi.fn(() => {
        claimed = true;
        return {};
      }),
    });
    const { router } = renderAt(`/claim#${TOKEN}`, calls);
    await userEvent.type(await screen.findByLabelText("Your name"), "Mira Chen");
    await userEvent.type(screen.getByLabelText("Email"), "mira@acme.dev");
    // The new owner is never told the install already has one.
    const seen = new Set<string>();
    const watch = new MutationObserver(() => {
      if (document.body.textContent?.includes("This install already has an owner")) seen.add("already claimed");
    });
    watch.observe(document.body, { childList: true, subtree: true, characterData: true });
    await userEvent.click(screen.getByRole("button", { name: "Create passkey" }));

    await waitFor(() => expect(router.state.location.pathname).toBe("/start"));
    expect(await screen.findByRole("heading", { level: 1, name: "What do you want to run?" })).toBeVisible();
    watch.disconnect();
    expect(seen).toEqual(new Set());
    // Back doesn't return to the spent claim page.
    expect(router.history.length).toBe(1);
    expect(router.history.canGoBack()).toBe(false);
    expect(calls.auth.beginClaim).toHaveBeenCalledWith(
      expect.objectContaining({ token: TOKEN, name: "Mira Chen", email: "mira@acme.dev" }),
      expect.anything(),
    );
    expect(passkeys.create).toHaveBeenCalledOnce();
    const finish = (calls.auth.finishClaim.mock.calls[0] as unknown[])[0] as {
      ceremonyId: string;
      token: string;
      credentialJson: string;
    };
    expect(finish.ceremonyId).toBe("cer_1");
    expect(finish.token).toBe(TOKEN);
    expect(JSON.parse(finish.credentialJson).response.attestationObject).toBe("Bgc");
  });

  it("doesn't check the spent link again on its way to the start", async () => {
    stubPasskeys();
    let spent = false;
    const calls = api({
      // Claimed and signed in once the claim finishes.
      getAuthState: () =>
        spent ? authState({ signInMethod: SignInMethod.CLAIM }) : authState({ claimed: false, user: undefined }),
      checkClaim: vi.fn(() =>
        spent ? Promise.reject(authError(Code.PermissionDenied, "claim_link_rejected")) : checked(),
      ),
      finishClaim: vi.fn(() => {
        spent = true;
        return {};
      }),
    });
    const { router } = renderAt(`/claim#${TOKEN}`, calls);
    await screen.findByRole("region", { name: "This server" });
    await userEvent.type(screen.getByLabelText("Your name"), "Mira Chen");
    await userEvent.type(screen.getByLabelText("Email"), "mira@acme.dev");
    const seen = new Set<string>();
    const watch = new MutationObserver(() => {
      if (document.body.textContent?.includes("The owner account wasn't created")) seen.add("not created");
    });
    watch.observe(document.body, { childList: true, subtree: true, characterData: true });
    await userEvent.click(screen.getByRole("button", { name: "Create passkey" }));

    await waitFor(() => expect(router.state.location.pathname).toBe("/start"));
    await screen.findByRole("heading", { level: 1, name: "What do you want to run?" });
    watch.disconnect();
    expect(seen).toEqual(new Set());
    expect(calls.auth.checkClaim).toHaveBeenCalledOnce();
  });

  it("keeps the link's expiry current, and says when it has expired", async () => {
    vi.useFakeTimers({ shouldAdvanceTime: true });
    try {
      const soon = () =>
        create(CheckClaimResponseSchema, {
          ...checked(),
          expireTime: timestampFromDate(new Date(Date.now() + 90_000)),
        });
      renderAt(`/claim#${TOKEN}`, api({ checkClaim: soon }));
      const server = await screen.findByRole("region", { name: "This server" });
      expect(server).toHaveTextContent("in 1 min");
      await act(() => vi.advanceTimersByTimeAsync(60_000));
      expect(server).toHaveTextContent("in under a minute");
      await act(() => vi.advanceTimersByTimeAsync(60_000));
      expect(server).toHaveTextContent("Link expiresExpired");
    } finally {
      vi.useRealTimers();
    }
  });

  it("says in a line where the passkey works, with a domain for later", async () => {
    renderAt(`/claim#${TOKEN}`, api());
    await screen.findByRole("heading", { level: 1, name: "Claim hz-1" });
    expect(screen.getByLabelText("Panel address")).toHaveAccessibleDescription(
      "Your passkey works at this address only. A domain with HTTPS can come later, under Settings.",
    );
  });

  it("checks the form before asking for a passkey", async () => {
    const passkeys = stubPasskeys();
    renderAt("/claim", api());
    await userEvent.click(await screen.findByRole("button", { name: "Create passkey" }));
    expect(screen.getByText("Enter your name.")).toBeVisible();
    expect(screen.getByText("Enter an email address, such as mira@acme.dev.")).toBeVisible();
    expect(screen.getByText("Paste the claim link or code you got on the server.")).toBeVisible();
    expect(passkeys.create).not.toHaveBeenCalled();
  });

  it("explains a link that doesn't work", async () => {
    stubPasskeys();
    renderAt(
      `/claim#${TOKEN}`,
      api({ checkClaim: () => Promise.reject(authError(Code.PermissionDenied, "claim_link_rejected")) }),
    );
    expect(await screen.findByRole("alert")).toHaveTextContent(
      "This claim link is invalid, already used, replaced by a newer one, or expired. Run dockwell claim on the server for a new link.",
    );
  });

  it("names the command that works for how Dockwell runs on the server", async () => {
    stubPasskeys();
    renderAt(
      `/claim#${TOKEN}`,
      api({
        getAuthState: () => authState({ claimed: false, user: undefined, claimCommand: "sudo dockwell claim" }),
        checkClaim: () => Promise.reject(authError(Code.PermissionDenied, "claim_link_rejected")),
      }),
    );
    expect(await screen.findByRole("alert")).toHaveTextContent(
      "This claim link is invalid, already used, replaced by a newer one, or expired. Run sudo dockwell claim on the server for a new link.",
    );
    expect(screen.getByText("sudo dockwell claim", { selector: "code" })).toBeVisible();
  });

  it("says so when the passkey prompt is closed", async () => {
    stubPasskeys({ create: "NotAllowedError" });
    const calls = api();
    renderAt(`/claim#${TOKEN}`, calls);
    await userEvent.type(await screen.findByLabelText("Your name"), "Mira");
    await userEvent.type(screen.getByLabelText("Email"), "mira@acme.dev");
    await userEvent.click(screen.getByRole("button", { name: "Create passkey" }));
    expect(await screen.findByRole("alert")).toHaveTextContent("The passkey prompt closed before it finished.");
    expect(calls.auth.finishClaim).not.toHaveBeenCalled();
  });

  it("warns where passkeys can't work", async () => {
    renderAt(`/claim#${TOKEN}`, api());
    expect(await screen.findByText("Passkeys don't work at this address")).toBeVisible();
    expect(screen.getByRole("button", { name: "Create passkey" })).toBeDisabled();
  });

  it("points an install that has an owner to sign-in", async () => {
    renderAt(`/claim#${TOKEN}`, api({ getAuthState: () => authState({ user: undefined }) }));
    expect(await screen.findByRole("heading", { level: 1, name: "This install already has an owner" })).toBeVisible();
    expect(screen.getByText("dockwell recover")).toBeVisible();
  });

  it("takes a pasted link as well as a code", () => {
    expect(tokenFrom(`http://localhost:7600/claim#${TOKEN}`)).toBe(TOKEN);
    expect(tokenFrom(`  ${TOKEN} `)).toBe(TOKEN);
  });

  it("is accessible", async () => {
    stubPasskeys();
    const { container } = renderAt(`/claim#${TOKEN}`, api());
    await screen.findByRole("region", { name: "This server" });
    await expectAccessible(container);
  });

  it("speaks Simplified Chinese", async () => {
    await activateLocale("zh-CN");
    stubPasskeys();
    renderAt(`/claim#${TOKEN}`, api());
    expect(await screen.findByRole("heading", { level: 1, name: "认领 hz-1" })).toBeVisible();
    expect(screen.getByRole("button", { name: "创建通行密钥" })).toBeVisible();
  });
});
