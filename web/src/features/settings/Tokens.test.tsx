// SPDX-License-Identifier: AGPL-3.0-only

import { create } from "@bufbuild/protobuf";
import { timestampFromDate } from "@bufbuild/protobuf/wkt";
import { Code } from "@connectrpc/connect";
import {
  type BeginStepUpRequest,
  type CreateTokenRequest,
  type FinishStepUpRequest,
  StepUpAction,
  TokenGrant,
  TokenSchema,
  TokenScope,
} from "@dockwell/api";
import { PHONE_QUERY } from "@dockwell/ui";
import { act, screen, waitFor, within } from "@testing-library/react";
import userEvent from "@testing-library/user-event";
import { afterEach, describe, expect, it, vi } from "vitest";

import { activateLocale } from "../../app/i18n";
import { expectAccessible } from "../../test/a11y";
import { authError, authState, requestOptions, stubPasskeys } from "../../test/auth";
import { onPhone, renderAt } from "../../test/render";

const DAY = 86_400_000;
const minutesAgo = (m: number) => timestampFromDate(new Date(Date.now() - m * 60_000));

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
  grants: [TokenGrant.LOGS],
  createTime: timestampFromDate(new Date(2026, 7, 14)),
});
// The prefix, 43 random digits and a 6-digit checksum, which the hint never shows.
const secret = "dwt_4f9K2xQ7Lr8eVb3NcR5tYw1ZaD6gHj0kMp9sUq2x9TaQz81Lm";

/** The proof the stand-in control plane gives a step-up for action. */
const proofFor = (action: StepUpAction) => `proof-${action}`;

/** An action a step-up confirms: refused with needs_step_up without a proof, and done with one. */
function confirmed<Req extends { proof: string }, Res>(done: (req: Req) => Res) {
  return vi.fn((req: Req) => {
    if (req.proof === "") throw authError(Code.PermissionDenied, "needs_step_up");
    return done(req);
  });
}

function api(tokens: Record<string, unknown> = {}) {
  return {
    auth: {
      getAuthState: () => authState(),
      beginStepUp: vi.fn((req: BeginStepUpRequest) => ({
        ceremonyId: `cer-${req.action}`,
        optionsJson: requestOptions,
      })),
      finishStepUp: vi.fn((req: FinishStepUpRequest) => ({ proof: `proof-${req.ceremonyId.slice(4)}` })),
    },
    tokens: {
      listTokens: vi.fn(() => ({ tokens: [deploy, grafana] })),
      ...tokens,
    },
  };
}

afterEach(async () => {
  await activateLocale("en");
});

describe("API tokens", () => {
  it("lists every token: what it may do, where it was last used, and when it expires", async () => {
    const { container } = renderAt("/settings/api-tokens", api());
    expect(await screen.findByRole("heading", { level: 1, name: "API tokens" })).toBeVisible();
    expect(
      within(screen.getByRole("navigation", { name: "Settings" })).getByRole("link", { name: "API tokens" }),
    ).toHaveAttribute("aria-current", "page");
    const table = await screen.findByRole("table", { name: "Your API tokens" });
    expect(screen.getByText("2 tokens · 1 expires soon")).toBeVisible();
    const [, first, second] = within(table).getAllByRole("row");
    const one = within(first as HTMLElement);
    // A name that runs past two lines is cut there, with the whole of it as its tooltip.
    expect(one.getByText("GitHub Actions deploy")).toHaveAttribute("title", "GitHub Actions deploy");
    expect(one.getByText("dwt_4f9K…x9Ta")).toBeVisible();
    expect(first).toHaveTextContent("read:*write:undoable");
    expect(one.getByText("8 min ago")).toBeVisible();
    expect(first).toHaveTextContent("from 203.0.113.9");
    expect(one.getByText("in 9 days")).toBeVisible();
    expect(within(table).getByRole("columnheader", { name: "Created" })).toBeVisible();
    const two = within(second as HTMLElement);
    expect(second).toHaveTextContent("read:*logs:read");
    expect(two.getAllByText("Never")).toHaveLength(2);
    await expectAccessible(container);
  });

  it("says when there are no tokens yet, and how to make one", async () => {
    renderAt("/settings/api-tokens", api({ listTokens: () => ({ tokens: [] }) }));
    expect(await screen.findByRole("heading", { name: "No tokens yet" })).toBeVisible();
    expect(screen.getByText(/Make one for each script or integration/)).toBeVisible();
    expect(screen.getByText("0 tokens")).toBeVisible();
  });

  it("creates a token, confirmed with a passkey, and shows its secret once, partly hidden until asked", async () => {
    stubPasskeys();
    const writeText = vi.fn(async () => {});
    vi.stubGlobal("navigator", { ...navigator, clipboard: { writeText } });
    const listTokens = vi.fn(() => ({ tokens: [grafana] }));
    const createToken = confirmed((req: CreateTokenRequest) => ({
      token: create(TokenSchema, {
        ...deploy,
        id: "tok_new",
        name: req.name,
        hint: "dwt_4f9K…x9Ta",
        expireTime:
          req.expiration.case === "ttl"
            ? timestampFromDate(new Date(Date.now() + Number(req.expiration.value.seconds) * 1_000))
            : undefined,
      }),
      secret,
    }));
    renderAt("/settings/api-tokens", api({ listTokens, createToken }));
    const form = within(await screen.findByRole("region", { name: "Create a token" }));
    expect(form.getByRole("checkbox", { name: /read:\*/ })).toBeChecked();
    expect(form.getByRole("checkbox", { name: /read:\*/ })).toBeDisabled();
    expect(form.getByRole("checkbox", { name: /write:undoable/ })).not.toBeChecked();
    expect(form.getByRole("combobox", { name: "Expires" })).toHaveValue("90");

    await userEvent.type(form.getByRole("textbox", { name: "Name" }), "  Nightly backup check ");
    await userEvent.click(form.getByRole("checkbox", { name: /write:undoable/ }));
    await userEvent.selectOptions(form.getByRole("combobox", { name: "Expires" }), "In 30 days");
    await userEvent.click(form.getByRole("button", { name: "Create token" }));

    await waitFor(() => expect(createToken).toHaveBeenCalledTimes(2));
    const sent = createToken.mock.calls[1]?.[0];
    expect(sent?.name).toBe("Nightly backup check");
    expect(sent?.scope).toBe(TokenScope.READ_WRITE);
    expect(sent?.grants).toEqual([]);
    expect(sent?.proof).toBe(proofFor(StepUpAction.CREATE_TOKEN));
    // A ttl, so the control plane's clock sets the expiry, not the browser's.
    expect(sent?.expiration.case).toBe("ttl");
    expect(sent?.expiration.case === "ttl" ? Number(sent.expiration.value.seconds) * 1_000 : 0).toBe(30 * DAY);

    const made = (await screen.findByText("Copy this token now. It won't be shown again.")).closest(
      ".dw-callout",
    ) as HTMLElement;
    expect(made).toHaveTextContent(/Created Nightly backup check\. .* It expires \d+ \w+/);
    expect(within(made).queryByText(secret)).toBeNull();
    const hidden = made.querySelector(".tokens-secret code");
    // As its hint has it: 4 random digits at each end, and never the checksum.
    expect(hidden).toHaveTextContent(/^The new token, partly hidden: dwt_4f9K•+x9Ta$/);
    await userEvent.click(within(made).getByRole("button", { name: "Show the whole token" }));
    expect(within(made).getByText(secret).tagName).toBe("CODE");
    await userEvent.click(within(made).getByRole("button", { name: "Copy token" }));
    expect(writeText).toHaveBeenCalledWith(secret);
    expect(within(made).getByRole("button", { name: "Token copied" })).toBeVisible();
    expect(listTokens.mock.calls.length).toBeGreaterThan(1);
    // The form starts over for the next one.
    expect(form.getByRole("textbox", { name: "Name" })).toHaveValue("");

    await userEvent.click(within(made).getByRole("button", { name: "Done" }));
    expect(screen.queryByText(secret)).toBeNull();
    expect(screen.queryByText("Copy this token now. It won't be shown again.")).toBeNull();
  });

  it("keeps the new token's secret when the screen turns to a phone's width", async () => {
    stubPasskeys();
    // A screen that can change width while the page is open, as turning a phone does.
    let narrow = false;
    const listeners = new Set<() => void>();
    vi.stubGlobal("matchMedia", (query: string) => ({
      get matches() {
        return query === PHONE_QUERY && narrow;
      },
      media: query,
      addEventListener: (_: string, fn: () => void) => listeners.add(fn),
      removeEventListener: (_: string, fn: () => void) => listeners.delete(fn),
    }));
    const createToken = confirmed((req: CreateTokenRequest) => ({
      token: create(TokenSchema, { ...grafana, id: "tok_new", name: req.name }),
      secret,
    }));
    renderAt("/settings/api-tokens", api({ createToken }));
    const form = within(await screen.findByRole("region", { name: "Create a token" }));
    await userEvent.type(form.getByRole("textbox", { name: "Name" }), "CI");
    await userEvent.click(form.getByRole("button", { name: "Create token" }));
    expect(await screen.findByText("Copy this token now. It won't be shown again.")).toBeVisible();

    act(() => {
      narrow = true;
      for (const fn of listeners) fn();
    });
    expect(await screen.findByRole("list", { name: "Your API tokens" })).toBeVisible();
    expect(screen.getByText("Copy this token now. It won't be shown again.")).toBeVisible();
  });

  it("makes a token that doesn't expire, and one that reads logs, only when asked", async () => {
    stubPasskeys();
    const createToken = confirmed((req: CreateTokenRequest) => ({
      token: create(TokenSchema, { ...grafana, id: "tok_new", name: req.name }),
      secret,
    }));
    renderAt("/settings/api-tokens", api({ createToken }));
    const form = within(await screen.findByRole("region", { name: "Create a token" }));
    await userEvent.type(form.getByRole("textbox", { name: "Name" }), "Log shipper");
    await userEvent.click(form.getByRole("checkbox", { name: /logs:read/ }));
    await userEvent.selectOptions(form.getByRole("combobox", { name: "Expires" }), "Never");
    await userEvent.click(form.getByRole("button", { name: "Create token" }));
    await waitFor(() => expect(createToken).toHaveBeenCalledTimes(2));
    const sent = createToken.mock.calls[1]?.[0];
    expect(sent).toMatchObject({ name: "Log shipper", scope: TokenScope.READ_ONLY, grants: [TokenGrant.LOGS] });
    expect(sent?.expiration.case).toBeUndefined();
    expect(await screen.findByText(/It doesn't expire\./)).toBeVisible();
  });

  it("asks for a name before making a token, and says why one wasn't made", async () => {
    const passkeys = stubPasskeys();
    const createToken = vi.fn(() =>
      Promise.reject(
        authError(
          Code.InvalidArgument,
          "invalid",
          "you have 100 API tokens, the most you can have. Revoke one you no longer use, then try again",
        ),
      ),
    );
    renderAt("/settings/api-tokens", api({ createToken }));
    const form = within(await screen.findByRole("region", { name: "Create a token" }));
    await userEvent.click(form.getByRole("button", { name: "Create token" }));
    expect(form.getByText("Name the token after what will use it, such as GitHub Actions deploy.")).toBeVisible();
    expect(createToken).not.toHaveBeenCalled();

    await userEvent.type(form.getByRole("textbox", { name: "Name" }), "CI");
    await userEvent.click(form.getByRole("button", { name: "Create token" }));
    expect(await form.findByRole("alert")).toHaveTextContent(
      "You have 100 API tokens, the most you can have. Revoke one you no longer use, then try again.",
    );
    // Refused before any passkey is asked for.
    expect(passkeys.get).not.toHaveBeenCalled();
  });

  it("asks before revoking a token, and can keep it after all", async () => {
    const revokeToken = vi.fn(() => ({}));
    renderAt("/settings/api-tokens", api({ revokeToken }));
    await userEvent.click(await screen.findByRole("button", { name: "Revoke GitHub Actions deploy" }));
    await userEvent.click(screen.getByRole("button", { name: "Keep" }));
    expect(revokeToken).not.toHaveBeenCalled();
    await userEvent.click(screen.getByRole("button", { name: "Revoke GitHub Actions deploy" }));
    await userEvent.click(screen.getByRole("button", { name: "Revoke" }));
    await waitFor(() =>
      expect(revokeToken).toHaveBeenCalledWith(expect.objectContaining({ id: "tok_1" }), expect.anything()),
    );
  });

  it("says when it can't list the tokens, and tries again", async () => {
    const user = userEvent.setup();
    let fail = true;
    renderAt(
      "/settings/api-tokens",
      api({
        listTokens: () => {
          if (fail) throw authError(Code.Unavailable, "unavailable", "the control plane's store doesn't answer");
          return { tokens: [deploy] };
        },
      }),
    );
    expect(await screen.findByText("Dockwell can't list your tokens")).toBeVisible();
    fail = false;
    await user.click(screen.getByRole("button", { name: "Try again" }));
    expect(await screen.findByText("GitHub Actions deploy")).toBeVisible();
  });

  it("stacks each token's facts on a phone, under Settings", async () => {
    onPhone();
    renderAt("/settings/api-tokens", api());
    const list = await screen.findByRole("list", { name: "Your API tokens" });
    const [first] = within(list).getAllByRole("listitem");
    expect(first).toHaveTextContent("GitHub Actions deploy");
    expect(first).toHaveTextContent("Last used8 min ago");
    expect(within(first as HTMLElement).getByRole("button", { name: "Revoke GitHub Actions deploy" })).toBeVisible();
    expect(screen.getByRole("link", { name: "Back" })).toHaveAttribute("href", "/settings");
    expect(screen.queryByRole("table")).toBeNull();
  });

  it("speaks Simplified Chinese", async () => {
    await activateLocale("zh-CN");
    renderAt("/settings/api-tokens", api());
    expect(await screen.findByRole("heading", { level: 1, name: "API 令牌" })).toBeVisible();
    expect(await screen.findByRole("region", { name: "创建令牌" })).toBeVisible();
  });
});
