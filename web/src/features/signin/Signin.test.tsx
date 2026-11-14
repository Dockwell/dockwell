// SPDX-License-Identifier: AGPL-3.0-only

import { create } from "@bufbuild/protobuf";
import {
  ChangeSchema,
  ChangeState,
  type CheckSigninProviderRequest,
  type ProposeChangeRequest,
  SigninProviderInfoSchema,
  SigninProviderState,
  type WatchChangeResponse,
} from "@dockwell/api";
import { screen, waitFor, within } from "@testing-library/react";
import userEvent from "@testing-library/user-event";
import { describe, expect, it, vi } from "vitest";

import { expectAccessible } from "../../test/a11y";
import { feed, renderAt } from "../../test/render";
import { freeProviderName, issuerHost, mailboxDomain, publicIssuer, scopesOf } from "./providers";

const authentik = create(SigninProviderInfoSchema, {
  id: "sip_1",
  name: "our-authentik",
  displayName: "Our Authentik",
  issuer: "https://auth.example.com/application/o/dockwell/",
  clientId: "dockwell",
  hasSecret: true,
  scopes: ["groups"],
});

function planned() {
  return vi.fn((req: ProposeChangeRequest) => {
    void req;
    return {
      change: create(ChangeSchema, { id: "chg_provider", state: ChangeState.PLANNED, plan: { fingerprint: "abc" } }),
    };
  });
}

/** What reading an issuer finds, for the issuers these tests type. */
function check(req: CheckSigninProviderRequest) {
  const typed = req.issuer.replace(/^https:\/\//, "").replace(/\/$/, "");
  switch (typed) {
    case "auth.example.com":
      return {
        state: SigninProviderState.FOUND,
        issuer: "https://auth.example.com/",
        host: "auth.example.com",
        algorithms: ["ES256", "RS256"],
      };
    case "accounts.google.com":
      return {
        state: SigninProviderState.FOUND,
        issuer: "https://accounts.google.com",
        host: "accounts.google.com",
        algorithms: ["RS256"],
        public: true,
      };
    default:
      return {
        state: SigninProviderState.UNUSABLE,
        issuer: `https://${typed}`,
        host: typed,
        detail: `${typed} doesn't take PKCE with S256, which keeps a sign-in from being replayed. Turn it on for Dockwell's client at the provider`,
      };
  }
}

const base = {
  backups: { listBackupDestinations: () => ({ destinations: [] }) },
  images: { listRegistryLogins: () => ({ logins: [] }) },
};

describe("Sign-in providers", () => {
  it("offers to connect one while there's none, and lists each by what it's called, its issuer and its name", async () => {
    const { container, unmount } = renderAt("/settings/integrations", base);
    const section = await screen.findByRole("region", { name: "Sign-in" });
    expect(await within(section).findByRole("article", { name: "Sign-in provider" })).toHaveTextContent(
      "Not connected",
    );
    await expectAccessible(container);
    unmount();

    renderAt("/settings/integrations", {
      ...base,
      domains: { listSigninProviders: () => ({ providers: [authentik] }) },
    });
    const listed = await screen.findByRole("region", { name: "Sign-in" });
    expect(await within(listed).findByText("1 provider · for private routes")).toBeVisible();
    const card = within(listed).getByRole("article", { name: "Our Authentik" });
    expect(card).toHaveTextContent("auth.example.com");
    expect(card).toHaveTextContent("our-authentik for every project");
    expect(card).toHaveTextContent("Client dockwell");
    expect(within(listed).queryByRole("article", { name: "Sign-in provider" })).toBeNull();
  });

  it("reads the issuer as it's typed, and plans the provider with the issuer its document names", async () => {
    const user = userEvent.setup();
    const checks: CheckSigninProviderRequest[] = [];
    const proposed = planned();
    renderAt("/settings/integrations", {
      ...base,
      domains: {
        checkSigninProvider: (req) => {
          checks.push(req);
          return check(req);
        },
      },
      changes: { proposeChange: proposed, watchChange: feed<WatchChangeResponse>().stream },
    });
    const card = await screen.findByRole("article", { name: "Sign-in provider" });
    await user.click(within(card).getByRole("button", { name: "Connect" }));
    const sheet = await screen.findByRole("dialog", { name: "Add a sign-in provider" });
    const review = within(sheet).getByRole("button", { name: "Review plan" });
    expect(review).toBeDisabled();

    await user.type(within(sheet).getByRole("textbox", { name: "Issuer" }), "pkce.example.com");
    expect(await within(sheet).findByText("Can't sign in with pkce.example.com", {}, { timeout: 3000 })).toBeVisible();
    expect(within(sheet).getByText(/doesn't take PKCE with S256/)).toBeVisible();
    await user.type(within(sheet).getByRole("textbox", { name: "Client ID" }), "dockwell");
    expect(review).toBeDisabled();

    await user.clear(within(sheet).getByRole("textbox", { name: "Issuer" }));
    await user.type(within(sheet).getByRole("textbox", { name: "Issuer" }), "auth.example.com");
    expect(
      await within(sheet).findByText("Found auth.example.com · signs with ES256", {}, { timeout: 3000 }),
    ).toBeVisible();
    expect(checks.at(-1)).toMatchObject({ issuer: "auth.example.com" });
    // Its name and what it's called start from the host, under Advanced.
    expect(within(sheet).getByText(/Named auth-example-com · Shown as auth.example.com · every project/)).toBeVisible();
    expect(within(sheet).getByLabelText("Client secret")).toHaveAttribute("placeholder", "None, for a public client");

    await user.click(review);
    await waitFor(() => expect(proposed).toHaveBeenCalled());
    const edit = proposed.mock.calls[0]?.[0].edits[0];
    expect(edit?.resource).toMatchObject({ kind: "signin.provider", name: "auth-example-com" });
    expect(edit?.spec?.kind.value).toMatchObject({
      issuer: "https://auth.example.com/",
      clientId: "dockwell",
      clientSecret: "",
      displayName: "auth.example.com",
      scopes: [],
      anyAccount: false,
      projects: [],
    });
  });

  it("keeps any account off where anyone can have one, and says why", async () => {
    const user = userEvent.setup();
    renderAt("/settings/integrations", { ...base, domains: { checkSigninProvider: check } });
    await user.click(
      within(await screen.findByRole("article", { name: "Sign-in provider" })).getByRole("button", { name: "Connect" }),
    );
    const sheet = await screen.findByRole("dialog", { name: "Add a sign-in provider" });
    await user.type(within(sheet).getByRole("textbox", { name: "Issuer" }), "accounts.google.com");
    expect(
      await within(sheet).findByText("Found accounts.google.com · signs with RS256", {}, { timeout: 3000 }),
    ).toBeVisible();
    await user.click(within(sheet).getByText(/Named accounts-google-com/));
    const any = within(sheet).getByRole("switch", { name: "Any account here is your team's" });
    expect(any).toBeDisabled();
    expect(any).not.toBeChecked();
    expect(within(sheet).getByText("Anyone can have an account at accounts.google.com.")).toBeVisible();
  });

  it("changes a provider's scopes without asking for its secret again", async () => {
    const user = userEvent.setup();
    const proposed = planned();
    renderAt("/settings/integrations", {
      ...base,
      domains: {
        listSigninProviders: () => ({ providers: [authentik] }),
        checkSigninProvider: () => ({
          state: SigninProviderState.FOUND,
          issuer: authentik.issuer,
          host: "auth.example.com",
          algorithms: ["ES256"],
        }),
      },
      changes: { proposeChange: proposed, watchChange: feed<WatchChangeResponse>().stream },
    });
    const card = await screen.findByRole("article", { name: "Our Authentik" });
    await user.click(within(card).getByRole("button", { name: "Manage" }));
    const sheet = await screen.findByRole("dialog", { name: "Sign-in provider Our Authentik" });
    expect(within(sheet).getByLabelText("Client secret")).toHaveAttribute("placeholder", "Kept unless you type one");
    await user.click(within(sheet).getByText(/Named our-authentik · Shown as Our Authentik · every project/));
    await user.type(within(sheet).getByRole("textbox", { name: "More scopes" }), " offline_access");
    const review = within(sheet).getByRole("button", { name: "Review plan" });
    await waitFor(() => expect(review).toBeEnabled(), { timeout: 3000 });
    await user.click(review);
    await waitFor(() => expect(proposed).toHaveBeenCalled());
    expect(proposed.mock.calls[0]?.[0].edits[0]?.spec?.kind.value).toMatchObject({
      issuer: authentik.issuer,
      clientId: "dockwell",
      clientSecret: "",
      displayName: "Our Authentik",
      scopes: ["groups", "offline_access"],
    });
  });
});

describe("providers", () => {
  it("names a provider from its host, free of those taken", () => {
    expect(freeProviderName("auth.example.com", [])).toBe("auth-example-com");
    expect(
      freeProviderName("auth.example.com:8443", [
        authentik,
        create(SigninProviderInfoSchema, { name: "auth-example-com" }),
      ]),
    ).toBe("auth-example-com-2");
    expect(freeProviderName("", [])).toBe("sign-in");
    expect(freeProviderName("10.0.0.5", [])).toBe("p-10-0-0-5");
  });

  it("reads an issuer's host, typed or saved, and scopes typed in one field", () => {
    expect(issuerHost("auth.example.com")).toBe("auth.example.com");
    expect(issuerHost(" https://Auth.Example.com/application/o/x/ ")).toBe("auth.example.com");
    expect(issuerHost("")).toBe("");
    expect(scopesOf("openid groups, offline_access groups  email")).toEqual(["groups", "offline_access"]);
  });

  it("knows an issuer anyone has an account at, and a domain anyone has an address at (#598's final check, L3)", () => {
    expect(publicIssuer("https://accounts.google.com")).toBe(true);
    expect(publicIssuer("https://login.microsoftonline.com/common/v2.0")).toBe(true);
    expect(publicIssuer("https://login.microsoftonline.com/2f1c4a3e-0000-4000-8000-000000000001/v2.0")).toBe(false);
    expect(publicIssuer("https://auth.example.com/application/o/dockwell/")).toBe(false);
    expect(publicIssuer("")).toBe(false);
    expect(mailboxDomain("Gmail.com")).toBe(true);
    expect(mailboxDomain("@outlook.com")).toBe(true);
    expect(mailboxDomain("example.com")).toBe(false);
  });
});
