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
    const { container } = renderAt("/settings/members", {
      members: { listMembers: () => ({ members: [owner] }) },
      auth: { listProviderLinks: () => ({ links: [{ id: "plk_1", providerName: "Our Authentik" }] }) },
    });
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
    // The accounts you link at sign-in providers, as Profile and security lists them (ADR 0095).
    expect(await within(signIn).findByText("Our Authentik")).toBeVisible();
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
    let confirmed = false;
    const confirmProtections = vi.fn(() => {
      confirmed = true;
      return { confirmTime: timestampFromDate(new Date(2026, 10, 10, 9, 0)) };
    });
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
      members: {
        // The control plane's record of the confirmation, which the page reads back.
        listMembers: () => ({
          members: [owner],
          ...(confirmed
            ? {
                protectionsConfirmTime: timestampFromDate(new Date(2026, 10, 10, 9, 0)),
                protectionsConfirmedBy: "Mira Chen",
              }
            : {}),
        }),
        createInvite,
        confirmProtections,
      },
      resources: { listResources: () => ({ resources: [] }) },
    });
    await user.click(await screen.findByRole("button", { name: "Invite someone" }));
    await user.type(screen.getByRole("textbox", { name: "Invite by email" }), " Ada@Example.com ");
    // The first invite waits until someone confirmed each environment's protection, on record:
    // one state, with the button that does it, where an early Invite takes you.
    expect(screen.getByText("Protected environments aren't confirmed")).toBeVisible();
    await user.click(screen.getByRole("button", { name: "Invite" }));
    await waitFor(() => expect(screen.getByRole("button", { name: /^Confirm: / })).toHaveFocus());
    expect(createInvite).not.toHaveBeenCalled();
    await user.click(screen.getByRole("button", { name: /^Confirm: / }));
    expect(await screen.findByText(/^Protection confirmed by Mira Chen, /)).toBeVisible();
    expect(screen.queryByText("Protected environments aren't confirmed")).toBeNull();
    expect(confirmProtections).toHaveBeenCalledTimes(1);
    expect(screen.queryByRole("button", { name: /^Confirm: / })).toBeNull();
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

  it("keeps each environment's protection one step away before the first invite, and plans a change to one", async () => {
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
    // The button says what it puts on record, and the list is a step away.
    expect(await screen.findByRole("button", { name: "Confirm: 2 protected, 1 open" })).toBeVisible();
    expect(screen.queryByRole("list", { name: "Environments" })).toBeNull();
    const review = screen.getByRole("button", { name: "Review" });
    expect(review).toHaveAttribute("aria-expanded", "false");
    await user.click(review);
    expect(review).toHaveAttribute("aria-expanded", "true");
    const list = await screen.findByRole("list", { name: "Environments" });
    const items = within(list).getAllByRole("listitem");
    expect(items.map((i) => i.textContent)).toEqual([
      "production in shopProtected",
      "Staging in shopOpen",
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
    expect(screen.queryByRole("button", { name: /^Confirm: / })).toBeNull();
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
    // The state comes with the way to fix it, for the owner, who sets the domain from the panel.
    expect(screen.getByRole("link", { name: "Set the panel's domain" })).toHaveAttribute("href", "/settings/general");
    expect(screen.queryByText(/DOCKWELL_DOMAIN/)).toBeNull();
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
    // Its link stops at once and can't come back, so a second press says so, and Keep leaves it.
    expect(revokeInvite).not.toHaveBeenCalled();
    await user.click(within(invited as HTMLElement).getByRole("button", { name: "Keep" }));
    expect(revokeInvite).not.toHaveBeenCalled();
    await user.click(
      within(invited as HTMLElement).getByRole("button", { name: "Revoke the invite for sam@example.com" }),
    );
    await user.click(
      within(invited as HTMLElement).getByRole("button", {
        name: "Revoke the invite for sam@example.com: its link stops working",
      }),
    );
    await waitFor(() =>
      expect(revokeInvite).toHaveBeenCalledWith(expect.objectContaining({ id: "inv_1" }), expect.anything()),
    );
  });

  describe("managing someone", () => {
    const ada = create(MemberSchema, {
      user: { id: "usr_2", name: "Ada Park", email: "ada@example.com", role: Role.DEVELOPER },
      signIn: [SignInKind.PASSKEY],
      grants: [{ projectId: "prj_1", projectName: "shop", role: Role.DEVELOPER }],
      invitedBy: "Mira Chen",
      joinedFrom: "203.0.113.7",
    });
    const projects = {
      listResources: () => ({ resources: [{ id: "prj_1", resource: { kind: "project", name: "shop" } }] }),
    };
    const stepUp = () => ({
      beginStepUp: vi.fn(() => ({ ceremonyId: "cer_1", optionsJson: requestOptions })),
      finishStepUp: () => ({ proof: "proof_1" }),
    });

    it("says on whose word someone joined and from where, and gives everyone but you a menu", async () => {
      renderAt("/settings/members", {
        auth: { getAuthState: () => authState({ domain: "panel.example.com" }) },
        members: { listMembers: () => ({ members: [owner, ada] }) },
      });
      const table = await screen.findByRole("table", { name: "Who signs in to this install" });
      const row = within(table).getByRole("row", { name: /Ada Park/ });
      expect(row).toHaveTextContent("Invited by Mira Chen · joined from 203.0.113.7");
      expect(within(row).getByRole("button", { name: "Manage Ada Park" })).toBeVisible();
      // Nobody manages themselves: your own row leads to your profile.
      const mine = within(table).getByRole("row", { name: /mira@example\.com/ });
      expect(within(mine).queryByRole("button", { name: /^Manage/ })).toBeNull();
    });

    it("gives an admin no way to transfer ownership, and nobody a menu on the owner", async () => {
      const admin = create(MemberSchema, {
        user: { id: "usr_1", name: "Mira Chen", email: "mira@example.com", role: Role.ADMIN },
        you: true,
        signIn: [SignInKind.PASSKEY],
      });
      const theOwner = create(MemberSchema, {
        user: { id: "usr_3", name: "Sam Lee", email: "sam@example.com", role: Role.OWNER },
        signIn: [SignInKind.PASSKEY],
      });
      const user = userEvent.setup();
      renderAt("/settings/members", {
        auth: {
          getAuthState: () =>
            authState({
              domain: "panel.example.com",
              user: { id: "usr_1", name: "Mira Chen", email: "mira@example.com", role: Role.ADMIN },
              grants: [{ role: Role.ADMIN }],
            }),
        },
        members: { listMembers: () => ({ members: [admin, ada, theOwner] }) },
      });
      const table = await screen.findByRole("table", { name: "Who signs in to this install" });
      expect(
        within(within(table).getByRole("row", { name: /Sam Lee/ })).queryByRole("button", { name: /^Manage/ }),
      ).toBeNull();
      await user.click(within(table).getByRole("button", { name: "Manage Ada Park" }));
      const menu = screen.getByRole("menu", { name: "Manage Ada Park" });
      expect(within(menu).getByRole("menuitem", { name: "Change role" })).toBeVisible();
      expect(within(menu).getByRole("menuitem", { name: "Remove" })).toBeVisible();
      expect(within(menu).queryByRole("menuitem", { name: "Transfer ownership" })).toBeNull();
    });

    it("offers an admin no project's role, as they hold every project already", async () => {
      const user = userEvent.setup();
      const lee = create(MemberSchema, {
        user: { id: "usr_4", name: "Lee Admin", email: "lee@example.com", role: Role.ADMIN },
        signIn: [SignInKind.PASSKEY],
      });
      renderAt("/settings/members", {
        auth: { getAuthState: () => authState({ domain: "panel.example.com" }) },
        members: { listMembers: () => ({ members: [owner, lee] }) },
        resources: projects,
      });
      await user.click(await screen.findByRole("button", { name: "Manage Lee Admin" }));
      await user.click(screen.getByRole("menuitem", { name: "Change role" }));
      const sheet = await screen.findByRole("dialog", { name: "Change what Lee Admin holds" });
      const where = within(sheet).getByRole("combobox", { name: "Where" });
      expect(
        within(where)
          .getAllByRole("option")
          .map((o) => o.textContent),
      ).toEqual(["The whole install"]);
    });

    it("changes a role in a place with a passkey for exactly that, and says what a lowering ends", async () => {
      const user = userEvent.setup();
      stubPasskeys();
      const auth = stepUp();
      const setMemberRole = vi.fn((req: { proof: string }) => {
        if (!req.proof) throw authError(Code.PermissionDenied, "needs_step_up");
        return { member: ada, lowered: true, sessionsEnded: 1 };
      });
      renderAt("/settings/members", {
        auth: { getAuthState: () => authState({ domain: "panel.example.com" }), ...auth },
        members: { listMembers: () => ({ members: [owner, ada] }), setMemberRole },
        resources: projects,
      });
      await user.click(await screen.findByRole("button", { name: "Manage Ada Park" }));
      await user.click(screen.getByRole("menuitem", { name: "Change role" }));
      const sheet = await screen.findByRole("dialog", { name: "Change what Ada Park holds" });
      // What they hold now changes nothing, so there's nothing to press yet.
      expect(within(sheet).getByRole("button", { name: "Change role" })).toBeDisabled();
      await user.selectOptions(within(sheet).getByLabelText("Role"), "Viewer");
      // What a lowering ends, as rows, before the passkey.
      const effects = within(sheet).getByLabelText("What this changes for Ada Park");
      expect(effects).toHaveTextContent("SessionsSigned out everywhere");
      expect(effects).toHaveTextContent("API tokensRevoked");
      await user.click(within(sheet).getByRole("button", { name: "Change role" }));
      await waitFor(() =>
        expect(setMemberRole).toHaveBeenLastCalledWith(
          expect.objectContaining({ userId: "usr_2", role: Role.VIEWER, project: "", proof: "proof_1" }),
          expect.anything(),
        ),
      );
      expect(auth.beginStepUp).toHaveBeenCalledWith(
        expect.objectContaining({ action: StepUpAction.CHANGE_ROLE, target: "usr_2 viewer install" }),
        expect.anything(),
      );
    });

    it("sends taking back the last thing someone holds to Remove", async () => {
      const user = userEvent.setup();
      const sam = create(MemberSchema, {
        user: { id: "usr_4", name: "Sam Lee", email: "sam@example.com", role: Role.VIEWER },
      });
      renderAt("/settings/members", {
        auth: { getAuthState: () => authState({ domain: "panel.example.com" }) },
        members: { listMembers: () => ({ members: [owner, sam] }) },
        resources: projects,
      });
      await user.click(await screen.findByRole("button", { name: "Manage Sam Lee" }));
      await user.click(screen.getByRole("menuitem", { name: "Change role" }));
      const sheet = await screen.findByRole("dialog", { name: "Change what Sam Lee holds" });
      await user.selectOptions(within(sheet).getByLabelText("Role"), "No role there");
      expect(within(sheet).getByText("That's the last thing Sam Lee holds")).toBeVisible();
      expect(within(sheet).getByRole("button", { name: "Change role" })).toBeDisabled();
      // The way to do it is a button, which opens Remove in this sheet's place.
      await user.click(within(sheet).getByRole("button", { name: "Remove Sam Lee instead" }));
      expect(await screen.findByRole("dialog", { name: "Remove Sam Lee" })).toBeVisible();
    });

    it("says what removing someone ends before it asks, and removes them with a passkey for them", async () => {
      const user = userEvent.setup();
      stubPasskeys();
      const auth = stepUp();
      const removeMember = vi.fn((req: { proof: string }) => {
        if (!req.proof) throw authError(Code.PermissionDenied, "needs_step_up");
        return { sessionsEnded: 2, tokensRevoked: 1, invitesRevoked: 1, changesDiscarded: 1 };
      });
      renderAt("/settings/members", {
        auth: { getAuthState: () => authState({ domain: "panel.example.com" }), ...auth },
        members: {
          listMembers: () => ({ members: [owner, ada] }),
          planMemberRemoval: () => ({
            member: ada,
            sessions: 2,
            tokens: [{ id: "tok_1", name: "GitHub Actions deploy" }],
            invites: [{ id: "inv_1", email: "lin@example.com", role: Role.VIEWER }],
            changes: [{ id: "chg_1", summary: "Deploy web 1.5" }],
            links: [{ provider: "Our Authentik", account: "ada@example.com" }],
          }),
          removeMember,
        },
      });
      await user.click(await screen.findByRole("button", { name: "Manage Ada Park" }));
      await user.click(screen.getByRole("menuitem", { name: "Remove" }));
      const sheet = await screen.findByRole("dialog", { name: "Remove Ada Park" });
      expect(await within(sheet).findByText("2 browsers are signed out")).toBeVisible();
      expect(sheet).toHaveTextContent("GitHub Actions deploy · never used");
      expect(sheet).toHaveTextContent("lin@example.com");
      expect(sheet).toHaveTextContent("Deploy web 1.5");
      expect(sheet).toHaveTextContent("Sign-inPasskeys and codes deleted");
      expect(sheet).toHaveTextContent("Provider accounts unlinkedOur Authentik · ada@example.com");
      expect(sheet).toHaveTextContent("Apps and tasks they set upKeep running");
      expect(removeMember).not.toHaveBeenCalled();
      await user.click(within(sheet).getByRole("button", { name: "Remove Ada Park" }));
      await waitFor(() =>
        expect(removeMember).toHaveBeenLastCalledWith(
          expect.objectContaining({ userId: "usr_2", proof: "proof_1" }),
          expect.anything(),
        ),
      );
      expect(auth.beginStepUp).toHaveBeenCalledWith(
        expect.objectContaining({ action: StepUpAction.REMOVE_MEMBER, target: "usr_2" }),
        expect.anything(),
      );
    });

    it("lets the owner make someone else the owner, saying what changes for both", async () => {
      const user = userEvent.setup();
      stubPasskeys();
      const auth = stepUp();
      const transferOwnership = vi.fn((req: { proof: string }) => {
        if (!req.proof) throw authError(Code.PermissionDenied, "needs_step_up");
        return { member: ada };
      });
      renderAt("/settings/members", {
        auth: { getAuthState: () => authState({ domain: "panel.example.com" }), ...auth },
        members: { listMembers: () => ({ members: [owner, ada] }), transferOwnership },
      });
      await user.click(await screen.findByRole("button", { name: "Manage Ada Park" }));
      await user.click(screen.getByRole("menuitem", { name: "Transfer ownership" }));
      const sheet = await screen.findByRole("dialog", { name: "Make Ada Park the owner" });
      const effects = within(sheet).getByLabelText("What this changes");
      expect(effects).toHaveTextContent("Ada ParkBecomes the owner");
      expect(effects).toHaveTextContent("YouBecome an admin, and stay signed in");
      expect(effects).toHaveTextContent("Undoing thisOnly Ada Park can");
      await user.click(within(sheet).getByRole("button", { name: "Transfer ownership" }));
      await waitFor(() =>
        expect(transferOwnership).toHaveBeenLastCalledWith(
          expect.objectContaining({ userId: "usr_2", proof: "proof_1" }),
          expect.anything(),
        ),
      );
      expect(auth.beginStepUp).toHaveBeenCalledWith(
        expect.objectContaining({ action: StepUpAction.TRANSFER_OWNERSHIP, target: "usr_2" }),
        expect.anything(),
      );
    });
  });
});
