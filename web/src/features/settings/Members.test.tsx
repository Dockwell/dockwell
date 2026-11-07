// SPDX-License-Identifier: AGPL-3.0-only

import { create } from "@bufbuild/protobuf";
import { timestampFromDate } from "@bufbuild/protobuf/wkt";
import { Code, ConnectError } from "@connectrpc/connect";
import { MemberSchema, Role } from "@dockwell/api";
import { screen, within } from "@testing-library/react";
import userEvent from "@testing-library/user-event";
import { afterEach, describe, expect, it } from "vitest";

import { activateLocale } from "../../app/i18n";
import { expectAccessible } from "../../test/a11y";
import { onPhone, renderAt } from "../../test/render";

const owner = create(MemberSchema, {
  user: { id: "usr_1", name: "Mira Chen", email: "mira@example.com", role: Role.OWNER },
  you: true,
  passkeys: 2,
  passkeyAddresses: ["localhost", "panel.example.com"],
  authenticator: true,
  recoveryCodesLeft: 7,
  sessions: 2,
  lastActiveTime: timestampFromDate(new Date()),
});

afterEach(async () => {
  await activateLocale("en");
});

describe("Members", () => {
  it("says who signs in, their role and how, and that inviting comes later", async () => {
    const { container } = renderAt("/settings/members", { members: { listMembers: () => ({ members: [owner] }) } });
    expect(await screen.findByRole("heading", { level: 1, name: "Members" })).toBeVisible();
    expect(
      within(screen.getByRole("navigation", { name: "Settings" })).getByRole("link", { name: "Members" }),
    ).toHaveAttribute("aria-current", "page");
    expect(screen.getByText("Inviting someone comes in a later release")).toBeVisible();
    expect(screen.queryByRole("textbox")).toBeNull();
    const table = await screen.findByRole("table", { name: "Who signs in to this install" });
    const [, row] = within(table).getAllByRole("row");
    expect(row).toHaveTextContent("Mira Chen");
    expect(row).toHaveTextContent("You");
    expect(row).toHaveTextContent("mira@example.com");
    expect(row).toHaveTextContent("Owner");
    expect(row).toHaveTextContent("2 passkeys and an authenticator app");
    expect(row).toHaveTextContent("Now");
    expect(within(row as HTMLElement).getByRole("link", { name: "Profile and security" })).toHaveAttribute(
      "href",
      "/profile",
    );
    expect(screen.getByText("all sign in with a passkey")).toBeVisible();

    const signIn = screen.getByRole("region", { name: "How you sign in" });
    expect(signIn).toHaveTextContent("2 · localhost · panel.example.com");
    expect(signIn).toHaveTextContent("On, 7 recovery codes left");
    expect(signIn).toHaveTextContent("2 browsers");
    expect(screen.getByRole("region", { name: "What the owner can do" })).toHaveTextContent("Read the audit log");
    await expectAccessible(container);
  });

  it("says when it can't say who signs in, and tries again", async () => {
    const user = userEvent.setup();
    let fail = true;
    renderAt("/settings/members", {
      members: {
        listMembers: () => {
          if (fail) throw new ConnectError("the store doesn't answer", Code.Unavailable);
          return { members: [owner] };
        },
      },
    });
    expect(await screen.findByText("Dockwell can't say who signs in")).toBeVisible();
    fail = false;
    await user.click(screen.getByRole("button", { name: "Try again" }));
    expect(await screen.findByText("mira@example.com")).toBeVisible();
  });

  it("stacks a person's facts on a phone, under Settings", async () => {
    onPhone();
    renderAt("/settings/members", { members: { listMembers: () => ({ members: [owner] }) } });
    const list = await screen.findByRole("list", { name: "Who signs in" });
    expect(within(list).getByText("Mira Chen")).toBeVisible();
    expect(screen.getByRole("link", { name: "Back" })).toHaveAttribute("href", "/settings");
    expect(screen.queryByRole("table", { name: "Who signs in to this install" })).toBeNull();
  });

  it("speaks Simplified Chinese", async () => {
    await activateLocale("zh-CN");
    renderAt("/settings/members", { members: { listMembers: () => ({ members: [owner] }) } });
    expect(await screen.findByRole("heading", { level: 1, name: "成员" })).toBeVisible();
    expect(await screen.findByText("邀请他人将在之后的版本中推出")).toBeVisible();
  });
});
