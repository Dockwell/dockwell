// SPDX-License-Identifier: AGPL-3.0-only

import { create } from "@bufbuild/protobuf";
import { timestampFromDate } from "@bufbuild/protobuf/wkt";
import { Code, ConnectError } from "@connectrpc/connect";
import { MemberSchema, Role, SignInKind } from "@dockwell/api";
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
  signIn: [SignInKind.PASSKEY, SignInKind.AUTHENTICATOR_APP],
});
// Someone else, as the list gives them: who they are, the kinds of sign-in they
// have and when they were last active, and nothing more of how they sign in.
const other = create(MemberSchema, {
  user: { id: "usr_2", name: "Ada Park", email: "ada@example.com", role: Role.OWNER },
  signIn: [SignInKind.PASSKEY],
  lastActiveTime: timestampFromDate(new Date(Date.now() - 3 * 3_600_000)),
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

  it("says how anyone else signs in by its kinds alone, and how many sign in once it knows", async () => {
    let answer: (v: { members: (typeof owner)[] }) => void = () => {};
    renderAt("/settings/members", {
      members: { listMembers: () => new Promise((resolve) => (answer = resolve)) },
    });
    expect(await screen.findByRole("heading", { level: 1, name: "Members" })).toBeVisible();
    // Never "0 people" while it reads.
    expect(screen.queryByText(/people sign in to this install|1 person signs in/)).toBeNull();
    answer({ members: [owner, other] });
    expect(await screen.findByText("2 people sign in to this install")).toBeVisible();
    const table = screen.getByRole("table", { name: "Who signs in to this install" });
    const [, , row] = within(table).getAllByRole("row");
    expect(row).toHaveTextContent("Ada Park");
    expect(row).toHaveTextContent("Passkey");
    expect(row).not.toHaveTextContent("passkeys");
    expect(row).toHaveTextContent("3 h ago");
    expect(screen.getByText("all sign in with a passkey")).toBeVisible();
  });

  it("says when your code sign-in is locked, and where to lift it", async () => {
    const locked = create(MemberSchema, {
      ...owner,
      authenticator: false,
      codesLocked: true,
      signIn: [SignInKind.PASSKEY],
    });
    renderAt("/settings/members", { members: { listMembers: () => ({ members: [locked] }) } });
    expect(await screen.findByText("2 passkeys, with codes locked")).toBeVisible();
    expect(screen.getByRole("region", { name: "How you sign in" })).toHaveTextContent(
      "Locked after too many wrong codes, until you lift it in Profile and security",
    );
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
    expect(screen.queryByText(/sign in to this install|signs in to this install/)).toBeNull();
    fail = false;
    await user.click(screen.getByRole("button", { name: "Try again" }));
    expect(await screen.findByText("mira@example.com")).toBeVisible();
  });

  it("names each role on the install, and none for someone who holds projects alone", async () => {
    const as = (id: string, name: string, role: Role) =>
      create(MemberSchema, { user: { id, name, email: `${id}@example.com`, role }, signIn: [SignInKind.PASSKEY] });
    renderAt("/settings/members", {
      members: {
        listMembers: () => ({
          members: [
            owner,
            as("usr_3", "Sam Okafor", Role.ADMIN),
            as("usr_4", "Lin Wei", Role.DEVELOPER),
            as("usr_5", "Noor Haddad", Role.VIEWER),
            as("usr_6", "Tess Moreau", Role.UNSPECIFIED),
          ],
        }),
      },
    });
    const table = await screen.findByRole("table", { name: "Who signs in to this install" });
    const roleOf = (name: string) =>
      within(table)
        .getAllByRole("row")
        .find((row) => row.textContent?.includes(name))
        ?.querySelector(".members-role")?.textContent;
    expect(roleOf("Mira Chen")).toBe("Owner");
    expect(roleOf("Sam Okafor")).toBe("Admin");
    expect(roleOf("Lin Wei")).toBe("Developer");
    expect(roleOf("Noor Haddad")).toBe("Viewer");
    expect(roleOf("Tess Moreau")).toBe("–");
  });

  it("says when your role doesn't cover who signs in, with nothing to try again", async () => {
    const refused = () => {
      throw new ConnectError(
        "only an admin or the owner can do this, and you're a viewer, so it didn't happen. Ask an admin to change your role",
        Code.PermissionDenied,
        new Headers({ "Dockwell-Error": "needs_role" }),
      );
    };
    const { unmount } = renderAt("/settings/members", { members: { listMembers: refused } });
    expect(await screen.findByText("Your role doesn't cover this")).toBeVisible();
    expect(screen.getByText("Your role doesn't let you do this here. Ask an admin to change your role.")).toBeVisible();
    expect(screen.queryByText("Dockwell can't say who signs in")).toBeNull();
    expect(screen.queryByRole("button", { name: "Try again" })).toBeNull();
    unmount();

    // In Chinese it's all Chinese: the server's English never shows.
    await activateLocale("zh-CN");
    renderAt("/settings/members", { members: { listMembers: refused } });
    const callout = (await screen.findByText("你的角色无权访问这里")).closest(".dw-callout");
    expect(callout?.textContent).not.toMatch(/[A-Za-z]/);
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
