// SPDX-License-Identifier: AGPL-3.0-only

import { create } from "@bufbuild/protobuf";
import {
  ChangeSchema,
  ChangeState,
  type CheckDnsProviderRequest,
  DnsProviderInfoSchema,
  DnsProviderState,
  type ProposeChangeRequest,
  type WatchChangeResponse,
} from "@dockwell/api";
import { screen, waitFor, within } from "@testing-library/react";
import userEvent from "@testing-library/user-event";
import { afterEach, describe, expect, it, vi } from "vitest";

import { activateLocale } from "../../app/i18n";
import { expectAccessible } from "../../test/a11y";
import { feed, renderAt } from "../../test/render";
import { freeProviderName } from "./dns";

const saved = create(DnsProviderInfoSchema, {
  id: "dnp_1",
  name: "cloudflare",
  provider: "cloudflare",
  zones: ["example.com"],
});

afterEach(async () => {
  await activateLocale("en");
});

function planned() {
  return vi.fn((req: ProposeChangeRequest) => {
    void req;
    return {
      change: create(ChangeSchema, { id: "chg_dns", state: ChangeState.PLANNED, plan: { fingerprint: "abc" } }),
    };
  });
}

/** Cloudflare as the control plane checks it: right-token reaches two zones and writes in both. */
function cloudflare(checks: CheckDnsProviderRequest[]) {
  return (req: CheckDnsProviderRequest) => {
    checks.push(req);
    if (req.apiToken !== "right-token" && req.name === "") {
      return { state: DnsProviderState.REFUSED, detail: "Cloudflare: Invalid API Token (1000)" };
    }
    const reaches = ["example.com", "example.org"];
    if (req.zones.length === 0) return { state: DnsProviderState.REACHES, reaches };
    return {
      state: DnsProviderState.WRITES,
      reaches,
      zones: req.zones.map((zone) => ({ zone, writes: true })),
    };
  };
}

describe("DNS providers", () => {
  it("checks a token as it's pasted, picks every zone it reaches, and plans it once it writes", async () => {
    const user = userEvent.setup();
    const checks: CheckDnsProviderRequest[] = [];
    const proposed = planned();
    const { container } = renderAt("/settings/integrations", {
      backups: { listBackupDestinations: () => ({ destinations: [] }) },
      domains: { listDnsProviders: () => ({ providers: [] }), checkDnsProvider: cloudflare(checks) },
      changes: { proposeChange: proposed, watchChange: feed<WatchChangeResponse>().stream },
    });
    const card = await screen.findByRole("article", { name: "Cloudflare" });
    expect(card).toHaveTextContent("Not connected");
    expect(within(card).getByRole("link", { name: "How to route behind it" })).toBeVisible();
    await user.click(within(card).getByRole("button", { name: "Connect" }));
    const sheet = await screen.findByRole("dialog", { name: "Connect Cloudflare" });
    const review = within(sheet).getByRole("button", { name: "Review plan" });
    expect(review).toBeDisabled();
    expect(sheet).toHaveTextContent("An API token with Zone:Read and DNS:Edit for your zones.");

    await user.type(within(sheet).getByLabelText("API token"), "wrong");
    expect(await within(sheet).findByText("Wrong token", {}, { timeout: 3000 })).toBeVisible();
    expect(sheet).toHaveTextContent("Cloudflare: Invalid API Token (1000)");
    expect(review).toBeDisabled();

    await user.clear(within(sheet).getByLabelText("API token"));
    await user.type(within(sheet).getByLabelText("API token"), "right-token");
    // Of several zones, none is picked, as a check writes in each one picked, and the
    // token reaching more than it's used for says so at once.
    expect(await within(sheet).findByText("Token works · pick its zones", {}, { timeout: 5000 })).toBeVisible();
    expect(within(sheet).getByRole("checkbox", { name: "example.com" })).not.toBeChecked();
    expect(within(sheet).getByRole("checkbox", { name: "example.org" })).not.toBeChecked();
    expect(within(sheet).getByRole("link", { name: "Scope it to these zones" })).toBeVisible();
    expect(review).toBeDisabled();
    expect(checks.filter((c) => c.zones.length > 0)).toHaveLength(0);
    await expectAccessible(container);

    await user.click(within(sheet).getByRole("checkbox", { name: "example.com" }));
    expect(await within(sheet).findByText("Can write DNS for example.com", {}, { timeout: 3000 })).toBeVisible();
    expect(checks.at(-1)).toMatchObject({ apiToken: "right-token", zones: ["example.com"] });
    expect(within(sheet).getByRole("link", { name: "Scope it to these zones" })).toBeVisible();

    await user.click(review);
    await waitFor(() => expect(proposed).toHaveBeenCalled());
    const edit = proposed.mock.calls[0]?.[0].edits[0];
    expect(edit?.resource).toMatchObject({ kind: "dns.provider", name: "cloudflare" });
    expect(edit?.spec?.kind.value).toMatchObject({
      provider: "cloudflare",
      zones: ["example.com"],
      apiToken: "right-token",
    });
  });

  it("picks the one zone a token reaches, and says nothing of scope", async () => {
    const user = userEvent.setup();
    const checks: CheckDnsProviderRequest[] = [];
    renderAt("/settings/integrations", {
      backups: { listBackupDestinations: () => ({ destinations: [] }) },
      domains: {
        listDnsProviders: () => ({ providers: [] }),
        checkDnsProvider: (req) => {
          checks.push(req);
          if (req.zones.length === 0) return { state: DnsProviderState.REACHES, reaches: ["example.com"] };
          return {
            state: DnsProviderState.WRITES,
            reaches: ["example.com"],
            zones: [{ zone: "example.com", writes: true }],
          };
        },
      },
    });
    const card = await screen.findByRole("article", { name: "Cloudflare" });
    await user.click(within(card).getByRole("button", { name: "Connect" }));
    const sheet = await screen.findByRole("dialog", { name: "Connect Cloudflare" });
    await user.type(within(sheet).getByLabelText("API token"), "one-zone-token");
    expect(await within(sheet).findByText("Can write DNS for example.com", {}, { timeout: 5000 })).toBeVisible();
    expect(within(sheet).getByRole("checkbox", { name: "example.com" })).toBeChecked();
    expect(within(sheet).queryByRole("link", { name: "Scope it to these zones" })).toBeNull();
    expect(within(sheet).getByRole("button", { name: "Review plan" })).toBeEnabled();
  });

  it("changes a provider's zones without asking for its token again", async () => {
    const user = userEvent.setup();
    const checks: CheckDnsProviderRequest[] = [];
    const proposed = planned();
    renderAt("/settings/integrations", {
      backups: { listBackupDestinations: () => ({ destinations: [] }) },
      domains: { listDnsProviders: () => ({ providers: [saved] }), checkDnsProvider: cloudflare(checks) },
      changes: { proposeChange: proposed, watchChange: feed<WatchChangeResponse>().stream },
    });
    const card = await screen.findByRole("article", { name: "Cloudflare" });
    expect(within(card).getByText("Connected")).toBeVisible();
    expect(card).toHaveTextContent("cloudflare proves names by DNS");
    expect(card).toHaveTextContent("example.com");
    await user.click(within(card).getByRole("button", { name: "Manage" }));
    const sheet = await screen.findByRole("dialog", { name: "DNS provider cloudflare" });
    expect(within(sheet).getByLabelText("API token")).toHaveAttribute("placeholder", "Kept unless you paste one");
    expect(await within(sheet).findByText("Can write DNS for example.com", {}, { timeout: 3000 })).toBeVisible();
    // Nothing changed yet, so there's nothing to plan.
    expect(within(sheet).getByRole("button", { name: "Review plan" })).toBeDisabled();
    await user.click(await within(sheet).findByRole("checkbox", { name: "example.org" }));
    expect(await within(sheet).findByText("Can write DNS for 2 zones", {}, { timeout: 3000 })).toBeVisible();
    await user.click(within(sheet).getByRole("button", { name: "Review plan" }));
    await waitFor(() => expect(proposed).toHaveBeenCalled());
    expect(proposed.mock.calls[0]?.[0].edits[0]?.spec?.kind.value).toMatchObject({
      zones: ["example.com", "example.org"],
      apiToken: "",
    });
    // The saved token was checked by name; none was sent.
    expect(checks.every((c) => c.name === "cloudflare" && c.apiToken === "")).toBe(true);
  });
});

describe("dns", () => {
  it("names a new provider after Cloudflare, and never one that's taken", () => {
    expect(freeProviderName("cloudflare", [])).toBe("cloudflare");
    expect(freeProviderName("cloudflare", [saved])).toBe("cloudflare-2");
  });
});
