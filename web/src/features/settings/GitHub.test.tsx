// SPDX-License-Identifier: AGPL-3.0-only

import { create, type MessageInitShape } from "@bufbuild/protobuf";
import { timestampFromDate } from "@bufbuild/protobuf/wkt";
import { Code, ConnectError } from "@connectrpc/connect";
import {
  ChangeSchema,
  ChangeState,
  type GitHubAppInfo,
  GitHubAppInfoSchema,
  GitHubDelivery_State,
  GitLabConnectionInfoSchema,
  GitLabConnectionState,
  type ProposeChangeRequest,
  type WatchChangeResponse,
} from "@dockwell/api";
import { screen, waitFor, within } from "@testing-library/react";
import userEvent from "@testing-library/user-event";
import { afterEach, describe, expect, it, vi } from "vitest";

import { expectAccessible } from "../../test/a11y";
import { authError } from "../../test/auth";
import { type Api, feed, renderAt } from "../../test/render";

function acme(init: MessageInitShape<typeof GitHubAppInfoSchema> = {}): GitHubAppInfo {
  const base: MessageInitShape<typeof GitHubAppInfoSchema> = {
    id: "gha_1",
    name: "dockwell-deploy",
    appId: 42n,
    owner: "acme",
    ownerIsOrganization: true,
    htmlUrl: "https://github.com/apps/dockwell-deploy",
    settingsUrl: "https://github.com/organizations/acme/settings/apps/dockwell-deploy",
    keyFingerprint: "SHA256:fF5LErC5ltNwSYgliMSP+LT5B8Py/TXWlu0Jcctm2lI=",
    installations: [
      {
        id: 7n,
        account: "acme",
        organization: true,
        repositoryCount: 23,
        everyProject: true,
        settingsUrl: "https://github.com/organizations/acme/settings/installations/7",
      },
    ],
    delivery: { state: GitHubDelivery_State.REACHED },
    revision: 3n,
  };
  return create(GitHubAppInfoSchema, { ...base, ...init } as MessageInitShape<typeof GitHubAppInfoSchema>);
}

function api(apps: GitHubAppInfo[], more: Api = {}): Api {
  return {
    backups: { listBackupDestinations: () => ({ destinations: [] }) },
    ...more,
    gitProviders: { listGitProviders: () => ({ apps }), ...more.gitProviders },
  };
}

afterEach(() => {
  vi.unstubAllGlobals();
  vi.restoreAllMocks();
});

describe("GitHub in Integrations", () => {
  it("connects with one choice and one button: Create on GitHub posts the App's manifest to GitHub", async () => {
    const user = userEvent.setup();
    const submitted: { action: string; manifest: string }[] = [];
    vi.spyOn(HTMLFormElement.prototype, "submit").mockImplementation(function (this: HTMLFormElement) {
      submitted.push({
        action: this.action,
        manifest: (this.elements.namedItem("manifest") as HTMLInputElement).value,
      });
    });
    const started = vi.fn((req: { organization: string }) => ({
      actionUrl: `https://github.com/organizations/${req.organization}/settings/apps/new?state=s1`,
      manifest: '{"name":"Dockwell"}',
    }));
    const { container } = renderAt("/settings/integrations", api([], { gitProviders: { startGitHubApp: started } }));
    const card = await screen.findByRole("article", { name: "GitHub" });
    expect(card).toHaveTextContent("Not connected");
    expect(card).toHaveTextContent("Private repositories, deploy on push, and PR status");
    // What comes later is a line, never a card to connect.
    expect(screen.getByText("Cloud providers come later.")).toBeVisible();
    await user.click(within(card).getByRole("button", { name: "Connect" }));
    const sheet = await screen.findByRole("dialog", { name: "Connect GitHub" });
    // Your own account first; an organization is one step away, and needs its name.
    expect(within(sheet).getByRole("radio", { name: "Your account" })).toBeChecked();
    await user.click(within(sheet).getByRole("radio", { name: "An organization" }));
    const create = within(sheet).getByRole("button", { name: "Create on GitHub" });
    expect(create).toBeDisabled();
    await user.type(within(sheet).getByRole("textbox", { name: "Organization" }), "acme");
    expect(create).toBeEnabled();
    await user.click(create);
    await waitFor(() => expect(submitted).toHaveLength(1));
    expect(started).toHaveBeenCalledWith(expect.objectContaining({ organization: "acme" }), expect.anything());
    expect(submitted[0]).toEqual({
      action: "https://github.com/organizations/acme/settings/apps/new?state=s1",
      manifest: '{"name":"Dockwell"}',
    });
    await expectAccessible(container);
  });

  it("says where a connected App is installed, and, as one line, that GitHub can't reach the panel", async () => {
    const polling = acme({ delivery: { state: GitHubDelivery_State.POLLING, checkEverySeconds: 60 } });
    const { container } = renderAt("/settings/integrations", api([polling]));
    const card = await screen.findByRole("article", { name: "GitHub" });
    expect(card).toHaveTextContent("Connected");
    expect(card).toHaveTextContent("GitHub App on acme · 23 repositories");
    // The page counts it with the rest of what's connected.
    expect(
      await screen.findByText(
        "1 connected · the services Dockwell builds from, deploys with, routes through and backs up to",
      ),
    ).toBeVisible();
    expect(card).toHaveTextContent("GitHub can't reach this panel, so Dockwell checks for pushes every minute.");
    expect(within(card).getByRole("link", { name: "Give the panel a domain" })).toHaveAttribute(
      "href",
      "/settings/general",
    );
    await expectAccessible(container);
  });

  it("says when the panel refuses GitHub's pushes for their secret, and gives a new one beside it", async () => {
    const user = userEvent.setup();
    let refused = true;
    const rotated = vi.fn(() => {
      refused = false;
      return { changeId: "chg_secret" };
    });
    renderAt(
      "/settings/integrations",
      api([], {
        gitProviders: {
          listGitProviders: () => ({
            apps: [
              acme({
                delivery: { state: GitHubDelivery_State.POLLING, checkEverySeconds: 60, secretRefused: refused },
              }),
            ],
          }),
          rotateGitHubWebhookSecret: rotated,
        },
      }),
    );
    const card = await screen.findByRole("article", { name: "GitHub" });
    await within(card).findByText("GitHub's webhook secret isn't Dockwell's, so Dockwell checks for pushes itself.");
    // No domain fixes this: the action is the one that does.
    expect(within(card).queryByRole("link", { name: "Give the panel a domain" })).not.toBeInTheDocument();
    await user.click(within(card).getByRole("button", { name: "New webhook secret" }));
    await waitFor(() =>
      expect(rotated).toHaveBeenCalledWith(expect.objectContaining({ id: "gha_1" }), expect.anything()),
    );
    await waitFor(() =>
      expect(
        within(card).queryByText("GitHub's webhook secret isn't Dockwell's, so Dockwell checks for pushes itself."),
      ).not.toBeInTheDocument(),
    );
  });

  it("says when GitHub asked Dockwell to slow down, until when, and when the App was deleted on GitHub", async () => {
    const until = new Date();
    until.setHours(21, 40, 0, 0);
    const slowed = acme({
      delivery: { state: GitHubDelivery_State.BACKING_OFF, backoffUntil: timestampFromDate(until) },
    });
    const { unmount } = renderAt("/settings/integrations", api([slowed]));
    expect(await screen.findByRole("article", { name: "GitHub" })).toHaveTextContent(
      "GitHub asked Dockwell to slow down, so pushes aren't checked until 9:40 PM.",
    );
    unmount();
    renderAt("/settings/integrations", api([acme({ gone: true })]));
    const card = await screen.findByRole("article", { name: "GitHub" });
    expect(card).toHaveTextContent("Deleted on GitHub");
    expect(card).toHaveTextContent("The GitHub App was deleted on GitHub.");
    expect(within(card).getByRole("button", { name: "Connect again" })).toBeVisible();
  });

  it("narrows which projects an installation reaches, as a plan", async () => {
    const user = userEvent.setup();
    const proposed = vi.fn((req: ProposeChangeRequest) => {
      void req;
      return {
        change: create(ChangeSchema, { id: "chg_reach", state: ChangeState.PLANNED, plan: { fingerprint: "a" } }),
      };
    });
    renderAt(
      "/settings/integrations",
      api([acme()], {
        changes: { proposeChange: proposed, watchChange: feed<WatchChangeResponse>().stream },
        resources: {
          listResources: () => ({
            resources: [
              { id: "prj_shop", resource: { kind: "project", name: "shop" } },
              { id: "prj_blog", resource: { kind: "project", name: "blog" } },
            ],
          }),
        },
      }),
    );
    const card = await screen.findByRole("article", { name: "GitHub" });
    await user.click(within(card).getByRole("button", { name: "Manage" }));
    const sheet = await screen.findByRole("dialog", { name: "GitHub App dockwell-deploy" });
    const row = within(sheet).getByRole("listitem");
    expect(row).toHaveTextContent("23 repositories · for every project");
    expect(within(row).getByRole("link", { name: "Repositories on GitHub" })).toHaveAttribute(
      "href",
      "https://github.com/organizations/acme/settings/installations/7",
    );
    await user.click(within(row).getByRole("button", { name: "Projects" }));
    await user.click(within(row).getByRole("switch", { name: "Only some projects" }));
    const review = within(row).getByRole("button", { name: "Review plan" });
    expect(review).toBeDisabled();
    await user.click(within(row).getByRole("checkbox", { name: "shop" }));
    await user.click(review);
    await waitFor(() => expect(proposed).toHaveBeenCalled());
    const edit = proposed.mock.calls[0]?.[0].edits[0];
    expect(edit?.resource).toMatchObject({ kind: "github.app", name: "dockwell-deploy" });
    expect(edit?.spec?.kind.value).toMatchObject({ reaches: [{ installationId: 7n, projects: ["prj_shop"] }] });
    // From the App as the sheet read it, so a narrowing made since isn't put back.
    expect(edit?.baseRevision).toBe(3n);
  });

  it("gives the App a new webhook secret, and a new key from its file, each with a passkey, and says which key to delete", async () => {
    const user = userEvent.setup();
    const rotated = vi.fn(() => ({ changeId: "chg_secret" }));
    const keys: string[] = [];
    renderAt(
      "/settings/integrations",
      api([acme()], {
        gitProviders: {
          rotateGitHubWebhookSecret: rotated,
          replaceGitHubPrivateKey: (req) => {
            keys.push(req.privateKey);
            return {
              fingerprint: "SHA256:new",
              oldFingerprint: "SHA256:fF5LErC5ltNwSYgliMSP+LT5B8Py/TXWlu0Jcctm2lI=",
              keysUrl: "https://github.com/organizations/acme/settings/apps/dockwell-deploy#private-key",
            };
          },
        },
      }),
    );
    await user.click(
      within(await screen.findByRole("article", { name: "GitHub" })).getByRole("button", { name: "Manage" }),
    );
    const sheet = await screen.findByRole("dialog", { name: "GitHub App dockwell-deploy" });
    await user.click(within(sheet).getByRole("button", { name: "New webhook secret" }));
    expect(await within(sheet).findByText("GitHub has the new webhook secret")).toBeVisible();
    expect(rotated).toHaveBeenCalledWith(expect.objectContaining({ id: "gha_1" }), expect.anything());

    await user.click(within(sheet).getByRole("button", { name: "New private key" }));
    expect(within(sheet).getByRole("link", { name: "Generate a key on GitHub" })).toHaveAttribute(
      "href",
      "https://github.com/organizations/acme/settings/apps/dockwell-deploy",
    );
    const file = new File(["-----BEGIN RSA PRIVATE KEY-----\nMII\n-----END RSA PRIVATE KEY-----\n"], "key.pem");
    const input = sheet.querySelector<HTMLInputElement>('input[type="file"]');
    if (!input) throw new Error("no file input");
    await user.upload(input, file);
    expect(await within(sheet).findByText("Dockwell signs with the new key")).toBeVisible();
    expect(sheet).toHaveTextContent(
      "Delete the old one on GitHub: SHA256:fF5LErC5ltNwSYgliMSP+LT5B8Py/TXWlu0Jcctm2lI=",
    );
    expect(keys[0]).toContain("BEGIN RSA PRIVATE KEY");
  });

  it("removes the App after a second yes, and leaves the link that deletes it on GitHub", async () => {
    const user = userEvent.setup();
    let apps = [acme()];
    const removed = vi.fn(() => {
      apps = [];
      return {
        uninstalled: 1,
        deleteUrl: "https://github.com/organizations/acme/settings/apps/dockwell-deploy/advanced",
      };
    });
    renderAt(
      "/settings/integrations",
      api([], { gitProviders: { listGitProviders: () => ({ apps }), removeGitHubApp: removed } }),
    );
    await user.click(
      within(await screen.findByRole("article", { name: "GitHub" })).getByRole("button", { name: "Manage" }),
    );
    const sheet = await screen.findByRole("dialog", { name: "GitHub App dockwell-deploy" });
    await user.click(within(sheet).getByRole("button", { name: "Remove" }));
    expect(within(sheet).getByText("Remove the GitHub App from Dockwell?")).toBeVisible();
    expect(removed).not.toHaveBeenCalled();
    await user.click(within(sheet).getByRole("button", { name: "Remove" }));
    await waitFor(() => expect(removed).toHaveBeenCalled());
    expect(await screen.findByText("GitHub keeps the App until you delete it there")).toBeVisible();
    expect(screen.getByRole("link", { name: "Delete it on GitHub" })).toHaveAttribute(
      "href",
      "https://github.com/organizations/acme/settings/apps/dockwell-deploy/advanced",
    );
    expect(await screen.findByRole("article", { name: "GitHub" })).toHaveTextContent("Not connected");
  });
});

describe("Back from GitHub", () => {
  it("keeps the App GitHub made, once, then sends the browser on to install it", async () => {
    const assign = vi.fn();
    vi.stubGlobal("location", { ...window.location, assign });
    const finished = vi.fn(() => ({ installUrl: "https://github.com/apps/dockwell-deploy/installations/new" }));
    renderAt(
      "/settings/integrations/github/created?code=c1&state=s1",
      api([], { gitProviders: { finishGitHubApp: finished } }),
    );
    expect(await screen.findByRole("heading", { level: 1, name: "Connecting GitHub" })).toBeVisible();
    await waitFor(() =>
      expect(assign).toHaveBeenCalledWith("https://github.com/apps/dockwell-deploy/installations/new"),
    );
    expect(finished).toHaveBeenCalledTimes(1);
    expect(finished).toHaveBeenCalledWith(expect.objectContaining({ code: "c1", state: "s1" }), expect.anything());
  });

  it("tries again from the same page when GitHub was out of reach, as the code still works", async () => {
    const user = userEvent.setup();
    const assign = vi.fn();
    vi.stubGlobal("location", { ...window.location, assign });
    let calls = 0;
    renderAt(
      "/settings/integrations/github/created?code=c1&state=s1",
      api([], {
        gitProviders: {
          finishGitHubApp: () => {
            calls++;
            if (calls === 1) throw authError(Code.Unavailable, "github_unreachable");
            return { installUrl: "https://github.com/apps/dockwell-deploy/installations/new" };
          },
        },
      }),
    );
    expect(await screen.findByText("GitHub isn't connected")).toBeVisible();
    await user.click(screen.getByRole("button", { name: "Try again" }));
    await waitFor(() =>
      expect(assign).toHaveBeenCalledWith("https://github.com/apps/dockwell-deploy/installations/new"),
    );
    expect(calls).toBe(2);
  });

  it("says why it couldn't keep the App, with the way back", async () => {
    renderAt(
      "/settings/integrations/github/created?code=c1&state=old",
      api([], {
        gitProviders: {
          finishGitHubApp: () => {
            throw new ConnectError(
              "the connection to GitHub was started over 10 minutes ago, or in another session. Connect GitHub again",
              Code.FailedPrecondition,
            );
          },
        },
      }),
    );
    expect(await screen.findByText("GitHub isn't connected")).toBeVisible();
    expect(screen.getByRole("link", { name: "Back to Integrations" })).toHaveAttribute(
      "href",
      "/settings/integrations",
    );
  });

  it("keeps the installation GitHub sent back once GitHub says it's the App's, then shows Integrations", async () => {
    const added = vi.fn(() => ({}));
    const { router } = renderAt(
      "/settings/integrations/github/gha_1/installed?installation_id=7&setup_action=install",
      api([acme()], { gitProviders: { addGitHubInstallation: added } }),
    );
    await waitFor(() => expect(router.state.location.pathname).toBe("/settings/integrations"));
    expect(added).toHaveBeenCalledWith(expect.objectContaining({ id: "gha_1", installationId: 7n }), expect.anything());
  });

  it("says an organization's owner approves an installation first, and records nothing yet", async () => {
    const added = vi.fn(() => ({}));
    renderAt(
      "/settings/integrations/github/gha_1/installed?setup_action=request",
      api([acme()], { gitProviders: { addGitHubInstallation: added } }),
    );
    expect(await screen.findByText("The organization's owner approves it first")).toBeVisible();
    expect(added).not.toHaveBeenCalled();
  });

  it("flags missing Checks permission on the GitHub card and refreshes via Check again", async () => {
    const user = userEvent.setup();
    let missing = true;
    const refreshed = vi.fn(() => {
      missing = false;
      return { app: acme({ checksPermissionMissing: false }) };
    });
    renderAt(
      "/settings/integrations",
      api([], {
        gitProviders: {
          listGitProviders: () => ({
            apps: [
              acme({
                checksPermissionMissing: missing,
                permissionsUrl: "https://github.com/organizations/acme/settings/apps/dockwell-deploy/permissions",
              }),
            ],
          }),
          refreshGitHubApp: refreshed,
        },
      }),
    );
    const card = await screen.findByRole("article", { name: "GitHub" });
    expect(card).toHaveTextContent("Missing Checks");
    expect(within(card).getByRole("link", { name: "Update permissions on GitHub" })).toHaveAttribute(
      "href",
      "https://github.com/organizations/acme/settings/apps/dockwell-deploy/permissions",
    );
    await user.click(within(card).getByRole("button", { name: "Check again" }));
    await waitFor(() =>
      expect(refreshed).toHaveBeenCalledWith(expect.objectContaining({ id: "gha_1" }), expect.anything()),
    );
    await waitFor(() => expect(card).toHaveTextContent("Connected"));
  });

  it("connects a GitLab instance with live token check and proposes a gitlab.connection edit", async () => {
    const user = userEvent.setup();
    const checked = vi.fn(() => ({
      state: GitLabConnectionState.CONNECTED,
      url: "https://gitlab.example.com",
      tokenName: "dockwell-ci",
      scopes: ["api"],
    }));
    const proposed = vi.fn((req: ProposeChangeRequest) => {
      void req;
      return {
        change: create(ChangeSchema, { id: "chg_gl", state: ChangeState.PLANNED, plan: { fingerprint: "gl1" } }),
      };
    });
    const existing = create(GitLabConnectionInfoSchema, {
      id: "glc_1",
      name: "team-gitlab",
      url: "https://gitlab.example.com",
      pathPrefix: "acme",
      everyProject: true,
      state: GitLabConnectionState.CONNECTED,
      revision: 1n,
    });
    const { unmount } = renderAt(
      "/settings/integrations",
      api([], {
        changes: { proposeChange: proposed, watchChange: feed<WatchChangeResponse>().stream },
        gitProviders: { checkGitLabConnection: checked },
      }),
    );
    const card = await screen.findByRole("article", { name: "GitLab" });
    await user.click(within(card).getByRole("button", { name: "Connect" }));
    const sheet = await screen.findByRole("dialog", { name: "Connect GitLab" });
    await user.clear(within(sheet).getByLabelText("Name"));
    await user.type(within(sheet).getByLabelText("Name"), "self-managed");
    await user.clear(within(sheet).getByLabelText("GitLab URL"));
    await user.type(within(sheet).getByLabelText("GitLab URL"), "https://gitlab.example.com");
    await user.type(within(sheet).getByLabelText("Access token"), "glpat-test-token");
    await user.click(within(sheet).getByRole("button", { name: "Check connection" }));
    expect(await within(sheet).findByText("Connected to GitLab")).toBeVisible();
    expect(checked).toHaveBeenCalledWith(
      expect.objectContaining({ url: "https://gitlab.example.com", token: "glpat-test-token" }),
      expect.anything(),
    );
    await user.click(within(sheet).getByRole("button", { name: "Review plan" }));
    await waitFor(() => expect(proposed).toHaveBeenCalled());
    const edit = proposed.mock.calls[0]?.[0].edits[0];
    expect(edit?.resource).toMatchObject({ kind: "gitlab.connection", name: "self-managed" });
    expect(edit?.spec?.kind.case).toBe("gitlabConnection");

    unmount();
    renderAt(
      "/settings/integrations",
      api([], { gitProviders: { listGitProviders: () => ({ apps: [], gitlabConnections: [existing] }) } }),
    );
    const saved = await screen.findByRole("article", { name: "GitLab team-gitlab" });
    expect(saved).toHaveTextContent("gitlab.example.com/acme");
    expect(saved).toHaveTextContent("Connected");
  });
});
