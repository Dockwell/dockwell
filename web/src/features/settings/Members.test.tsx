// SPDX-License-Identifier: AGPL-3.0-only

import { create } from "@bufbuild/protobuf";
import { timestampFromDate } from "@bufbuild/protobuf/wkt";
import { Code, ConnectError } from "@connectrpc/connect";
import {
  type CreateInviteRequest,
  MemberSchema,
  type ProposeChangeRequest,
  Protection,
  Role,
  SignInKind,
  StepUpAction,
} from "@dockwell/api";
import { screen, waitFor, within } from "@testing-library/react";
import userEvent from "@testing-library/user-event";
import { afterEach, describe, expect, it, vi } from "vitest";

import { activateLocale } from "../../app/i18n";
import { expectAccessible } from "../../test/a11y";
import { authError, authState, requestOptions, stubPasskeys } from "../../test/auth";
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
  it("says who signs in, their role and how, and keeps the invite form behind a button for one person", async () => {
    const { container } = renderAt("/settings/members", { members: { listMembers: () => ({ members: [owner] }) } });
    expect(await screen.findByRole("heading", { level: 1, name: "Members" })).toBeVisible();
    expect(
      within(screen.getByRole("navigation", { name: "Settings" })).getByRole("link", { name: "Members" }),
    ).toHaveAttribute("aria-current", "page");
    expect(await screen.findByText("1 person signs in to this install, its owner")).toBeVisible();
    expect(screen.getByRole("button", { name: "Invite someone" })).toBeVisible();
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
    // Where a role is held, and what each can do, wait for a second person (ADR 0049).
    expect(within(table).queryByRole("columnheader", { name: "Projects" })).toBeNull();
    expect(screen.queryByRole("region", { name: "What each role can do" })).toBeNull();

    const signIn = screen.getByRole("region", { name: "How you sign in" });
    expect(signIn).toHaveTextContent("2 · localhost · panel.example.com");
    expect(signIn).toHaveTextContent("On, 7 recovery codes left");
    expect(signIn).toHaveTextContent("2 browsers");
    await expectAccessible(container);

    await userEvent.setup().click(screen.getByRole("button", { name: "Invite someone" }));
    expect(screen.getByRole("region", { name: "What each role can do" })).toHaveTextContent("Transfer ownership");
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
    expect(await screen.findByText("1 人可以登录这个安装，即其所有者")).toBeVisible();
    expect(screen.getByRole("button", { name: "邀请他人" })).toBeVisible();
  });

  it("invites someone with a passkey bound to what it's for, and shows the link once", async () => {
    const user = userEvent.setup();
    stubPasskeys();
    const beginStepUp = vi.fn(() => ({ ceremonyId: "cer_1", optionsJson: requestOptions }));
    const createInvite = vi.fn((req: CreateInviteRequest) => {
      if (!req.proof) throw authError(Code.PermissionDenied, "needs_step_up");
      return {
        link: "https://panel.example.com/invite#s3cr3t",
        invite: {
          id: "inv_1",
          // As the control plane writes it: in small letters, its way.
          email: req.email.toLowerCase(),
          role: req.role,
          expireTime: timestampFromDate(new Date(Date.now() + 7 * 86_400_000)),
        },
      };
    });
    renderAt("/settings/members", {
      auth: {
        getAuthState: () => authState({ domain: "panel.example.com" }),
        beginStepUp,
        finishStepUp: () => ({ proof: "proof_1" }),
      },
      members: { listMembers: () => ({ members: [owner] }), createInvite },
      resources: { listResources: () => ({ resources: [] }) },
    });
    await user.click(await screen.findByRole("button", { name: "Invite someone" }));
    await user.type(screen.getByRole("textbox", { name: "Invite by email" }), " Ada@Example.com ");
    // The first invite waits for the owner to check each environment's protection.
    await user.click(screen.getByRole("button", { name: "Invite" }));
    expect(
      await screen.findByText(
        "Check each environment's protection first, then tick the box: it decides whose changes wait.",
      ),
    ).toBeVisible();
    expect(createInvite).not.toHaveBeenCalled();
    await user.click(screen.getByRole("checkbox", { name: "I've checked which environments are protected" }));
    await user.click(screen.getByRole("button", { name: "Invite" }));
    expect(await screen.findByText("Send this link to ada@example.com")).toBeVisible();
    expect(screen.getByText("https://panel.example.com/invite#s3cr3t")).toBeVisible();
    // The email goes as typed, in the passkey's target and in the invite alike: the control
    // plane writes it its one way for both, so a letter such as İ can't part the two.
    expect(beginStepUp).toHaveBeenCalledWith(
      expect.objectContaining({ action: StepUpAction.INVITE, target: "Ada@Example.com developer install" }),
      expect.anything(),
    );
    expect(createInvite).toHaveBeenLastCalledWith(
      expect.objectContaining({ email: "Ada@Example.com", role: Role.DEVELOPER, project: "", proof: "proof_1" }),
      expect.anything(),
    );
  });

  it("lists each environment's protection before the first invite, and plans a change to one", async () => {
    const user = userEvent.setup();
    const proposeChange = vi.fn((_: ProposeChangeRequest) => ({}));
    const environment = (id: string, name: string, project: string, isProtected: boolean, displayName = "") => ({
      id,
      resource: { kind: "environment", scope: project, name },
      displayName,
      protected: isProtected,
    });
    renderAt("/settings/members", {
      auth: { getAuthState: () => authState({ domain: "panel.example.com" }) },
      members: { listMembers: () => ({ members: [owner] }) },
      changes: { proposeChange },
      resources: {
        listResources: (req: { kind: string }) =>
          req.kind === "environment"
            ? {
                resources: [
                  environment("env_1", "production", "prj_1", true),
                  environment("env_2", "staging", "prj_1", false, "Staging"),
                  environment("env_3", "production", "prj_2", true),
                ],
              }
            : {
                resources: [
                  { id: "prj_1", resource: { kind: "project", name: "shop" } },
                  { id: "prj_2", resource: { kind: "project", name: "blog" } },
                ],
              },
      },
    });
    await user.click(await screen.findByRole("button", { name: "Invite someone" }));
    const list = await screen.findByRole("list", { name: "Environments" });
    const items = within(list).getAllByRole("listitem");
    expect(items.map((i) => i.textContent)).toEqual([
      "production in shopProtected",
      "Staging in shopNot protected",
      "production in blogProtected",
    ]);
    expect(within(list).getByRole("switch", { name: "Protect production in shop" })).toBeChecked();
    const staging = within(list).getByRole("switch", { name: "Protect Staging in shop" });
    expect(staging).not.toBeChecked();

    await user.click(staging);
    await waitFor(() => expect(proposeChange).toHaveBeenCalled());
    const edit = proposeChange.mock.calls[0]?.[0].edits[0];
    expect(edit?.resource).toMatchObject({ kind: "environment", scope: "prj_1", name: "staging" });
    // Its display name stays as it is, as an edit replaces the whole spec.
    expect(edit?.spec?.kind).toMatchObject({
      case: "environment",
      value: { displayName: "Staging", protection: Protection.PROTECTED },
    });
    // It changes once the plan is applied, not before.
    expect(staging).not.toBeChecked();
  });

  it("doesn't ask about protection once someone else signs in", async () => {
    const user = userEvent.setup();
    renderAt("/settings/members", {
      auth: { getAuthState: () => authState({ domain: "panel.example.com" }) },
      members: { listMembers: () => ({ members: [owner, other] }) },
      resources: { listResources: () => ({ resources: [] }) },
    });
    expect(await screen.findByRole("textbox", { name: "Invite by email" })).toBeVisible();
    await user.type(screen.getByRole("textbox", { name: "Invite by email" }), "sam@example.com");
    expect(screen.queryByRole("checkbox", { name: "I've checked which environments are protected" })).toBeNull();
    expect(screen.queryByRole("list", { name: "Environments" })).toBeNull();
  });

  it("says invites need the panel's domain on an install without one", async () => {
    const user = userEvent.setup();
    renderAt("/settings/members", {
      auth: { getAuthState: () => authState({ domain: "" }) },
      members: { listMembers: () => ({ members: [owner] }) },
    });
    await user.click(await screen.findByRole("button", { name: "Invite someone" }));
    expect(await screen.findByText("Invites need the panel's domain")).toBeVisible();
    expect(screen.queryByRole("textbox", { name: "Invite by email" })).toBeNull();
  });

  it("lists open invites with who made them and when they end, and revokes one", async () => {
    const user = userEvent.setup();
    const revokeInvite = vi.fn(() => ({}));
    const ada = create(MemberSchema, {
      user: { id: "usr_2", name: "Ada Park", email: "ada@example.com", role: Role.UNSPECIFIED },
      signIn: [SignInKind.PASSKEY],
      grants: [{ projectId: "prj_1", projectName: "shop", role: Role.DEVELOPER }],
    });
    const { container } = renderAt("/settings/members", {
      auth: { getAuthState: () => authState({ domain: "panel.example.com" }) },
      members: {
        listMembers: () => ({
          members: [owner, ada],
          invites: [
            {
              id: "inv_1",
              email: "sam@example.com",
              role: Role.VIEWER,
              projectId: "prj_1",
              projectName: "shop",
              invitedBy: "Mira Chen",
              createTime: timestampFromDate(new Date(Date.now() - 3_600_000)),
              expireTime: timestampFromDate(new Date(Date.now() + 6 * 86_400_000)),
            },
          ],
        }),
        revokeInvite,
      },
      resources: { listResources: () => ({ resources: [] }) },
    });
    expect(await screen.findByText("2 people sign in to this install · 1 open invite")).toBeVisible();
    const table = await screen.findByRole("table", { name: "Who signs in to this install" });
    const rows = within(table).getAllByRole("row");
    const adaRow = rows.find((r) => r.textContent?.includes("Ada Park"));
    expect(adaRow).toHaveTextContent("Developer");
    expect(adaRow).toHaveTextContent("shop");
    const invited = rows.find((r) => r.textContent?.includes("sam@example.com"));
    expect(invited).toHaveTextContent("Invited by Mira Chen");
    expect(invited).toHaveTextContent("Viewer");
    expect(invited).toHaveTextContent("Not accepted yet");
    expect(invited).toHaveTextContent("Expires in 6 days");
    await expectAccessible(container);
    await user.click(
      within(invited as HTMLElement).getByRole("button", { name: "Revoke the invite for sam@example.com" }),
    );
    await waitFor(() =>
      expect(revokeInvite).toHaveBeenCalledWith(expect.objectContaining({ id: "inv_1" }), expect.anything()),
    );
  });
});
