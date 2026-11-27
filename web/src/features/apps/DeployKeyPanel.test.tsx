// SPDX-License-Identifier: AGPL-3.0-only

import { create, type MessageInitShape } from "@bufbuild/protobuf";
import { timestampFromDate } from "@bufbuild/protobuf/wkt";
import { Code, ConnectError } from "@connectrpc/connect";
import {
  type GitDeployKeyInfo,
  GitDeployKeyInfoSchema,
  GitDeployKeyTestStatus,
  Role,
  ServiceSchema,
  type WatchServersResponse,
} from "@dockwell/api";
import { screen, waitFor, within } from "@testing-library/react";
import userEvent from "@testing-library/user-event";
import { describe, expect, it } from "vitest";

import { expectAccessible } from "../../test/a11y";
import { anApp, places, SERVER } from "../../test/apps";
import { authState } from "../../test/auth";
import { type Api, feed, renderAt } from "../../test/render";
import { aServer, changed, snapshotComplete } from "../../test/servers";

const hz1 = aServer({ id: SERVER, name: "hz-1" });

function makeKey(fields: MessageInitShape<typeof GitDeployKeyInfoSchema> = {}): GitDeployKeyInfo {
  return create(GitDeployKeyInfoSchema, {
    id: "gdk_01TESTKEY0000000000000001",
    name: "acme-web",
    projectId: "prj_1",
    repository: "ssh://git@github.com/acme/web.git",
    host: "github.com",
    port: 22,
    user: "git",
    keyType: "ssh-ed25519",
    publicKey: "ssh-ed25519 AAAAC3NzaC1lZDI1NTE5AAAAITestPublicKeyMaterial00000000000000000000000 dockwell",
    keyFingerprint: "SHA256:KeyFingerprint000000000000000000000000000000",
    hostKeyAlgorithm: "",
    hostKeyFingerprint: "",
    hostKeyPinned: false,
    hostKeyChanged: false,
    deployKeysUrl: "https://github.com/acme/web/settings/keys",
    hostName: "GitHub",
    gitLogin: "ssh:acme-web",
    createTime: timestampFromDate(new Date("2026-11-27T10:00:00Z")),
    ...fields,
  });
}

function baseApi(more: Api = {}): Api {
  const watch = feed<WatchServersResponse>();
  watch.push(changed(hz1), snapshotComplete);
  return {
    resources: places(),
    services: { listServices: () => ({}), listServiceRevisions: () => ({}) },
    servers: { watchServers: watch.stream },
    ...more,
  };
}

describe("SSH deploy keys in New app -> From Git", () => {
  it("guides an SSH address through key generation, host-key pinning, test and branch listing", async () => {
    const user = userEvent.setup();
    let storedKey: GitDeployKeyInfo | undefined;
    let testCalls = 0;

    const { container } = renderAt(
      "/apps/new?source=git",
      baseApi({
        gitKeys: {
          listGitDeployKeys: () => ({ keys: storedKey ? [storedKey] : [] }),
          createGitDeployKey: () => {
            storedKey = makeKey();
            return { key: storedKey, changeId: "chg_create" };
          },
          testGitDeployKey: (req) => {
            testCalls += 1;
            if (!req.acceptHostKey) {
              return {
                status: GitDeployKeyTestStatus.HOST_KEY_UNVERIFIED,
                observedHostKeyAlgorithm: "ssh-ed25519",
                observedHostKeyFingerprint: "SHA256:HostKeyFP00000000000000000000000000000000000",
                key: storedKey,
              };
            }
            storedKey = makeKey({
              hostKeyAlgorithm: req.hostKeyAlgorithm,
              hostKeyFingerprint: req.hostKeyFingerprint,
              hostKeyPinned: true,
            });
            return {
              status: GitDeployKeyTestStatus.OK,
              defaultBranch: "main",
              branches: ["main", "staging"],
              key: storedKey,
            };
          },
        },
        builds: {
          listRepositoryBranches: () => ({
            branches: ["main", "staging"],
            defaultBranch: "main",
            gitLogin: "ssh:acme-web",
          }),
        },
      }),
    );

    const sheet = await screen.findByRole("dialog", { name: "New app" });
    await user.type(within(sheet).getByLabelText("Repository"), "git@github.com:acme/web.git");

    const offer = await within(sheet).findByTestId("deploy-key-offer");
    await user.click(within(offer).getByRole("button", { name: "Use a deploy key" }));

    const setup = await within(sheet).findByTestId("deploy-key-setup");
    expect(within(setup).getByTestId("deploy-key-pubkey")).toHaveTextContent("ssh-ed25519 AAAAC3");
    expect(within(setup).getByRole("link", { name: "Open GitHub deploy keys" })).toHaveAttribute(
      "href",
      "https://github.com/acme/web/settings/keys",
    );

    await user.click(within(setup).getByRole("button", { name: "Test" }));
    expect(await within(setup).findByText("SHA256:HostKeyFP00000000000000000000000000000000000")).toBeVisible();

    await user.click(within(setup).getByRole("button", { name: "Accept host key and test" }));
    await waitFor(() => expect(testCalls).toBe(2));
    expect(await within(sheet).findByTestId("deploy-key-ready")).toBeVisible();

    // Branch selector now offers main (default) and staging.
    const branchSelect = within(sheet).getByLabelText("Branch");
    expect(within(branchSelect).getByRole("option", { name: "main (default)" })).toBeInTheDocument();
    expect(within(branchSelect).getByRole("option", { name: "staging" })).toBeInTheDocument();

    await expectAccessible(container);
  });

  it("offers a deploy key in place when an HTTPS repository answers auth_required", async () => {
    const user = userEvent.setup();
    renderAt(
      "/apps/new?source=git",
      baseApi({
        builds: {
          listRepositoryBranches: () => {
            throw new ConnectError("Private repository", Code.InvalidArgument, { "Dockwell-Error": "auth_required" });
          },
        },
      }),
    );

    const sheet = await screen.findByRole("dialog", { name: "New app" });
    await user.type(within(sheet).getByLabelText("Repository"), "https://github.com/acme/private.git");

    const offer = await within(sheet).findByTestId("deploy-key-offer");
    expect(within(offer).getByRole("button", { name: "Use a deploy key" })).toBeVisible();
  });
});

describe("SSH deploy keys on App Source panel", () => {
  it("rotates a deploy key and tells you to delete the old key once the new key passes Test", async () => {
    const user = userEvent.setup();
    let storedKey = makeKey({
      hostKeyAlgorithm: "ssh-ed25519",
      hostKeyFingerprint: "SHA256:HostKeyFP00000000000000000000000000000000000",
      hostKeyPinned: true,
      usedBy: ["web"],
    });
    const app = anApp(
      "web",
      "dockwell.local/built:1",
      {},
      {
        spec: create(ServiceSchema, {
          runs: { case: "app", value: { image: "dockwell.local/built:1" } },
          build: {
            source: { case: "git", value: { url: "ssh://git@github.com/acme/web.git", ref: "refs/heads/main" } },
            gitLogin: "ssh:acme-web",
          },
        }),
      },
    );

    const { container } = renderAt(
      "/apps/svc_web",
      baseApi({
        services: { listServices: () => ({ services: [app] }), listServiceRevisions: () => ({}) },
        gitKeys: {
          listGitDeployKeys: () => ({ keys: [storedKey] }),
          beginRotateGitDeployKey: () => {
            storedKey = makeKey({
              ...storedKey,
              pendingPublicKey:
                "ssh-ed25519 AAAAC3NzaC1lZDI1NTE5AAAAINewPendingKeyMaterial0000000000000000000000 dockwell",
              pendingKeyType: "ssh-ed25519",
              pendingKeyFingerprint: "SHA256:NewKeyFingerprint000000000000000000000000000",
            });
            return { key: storedKey, changeId: "chg_rot_begin" };
          },
          testGitDeployKey: (req) => {
            if (req.usePending) {
              const oldFP = storedKey.keyFingerprint;
              storedKey = makeKey({
                ...storedKey,
                publicKey: storedKey.pendingPublicKey,
                keyFingerprint: storedKey.pendingKeyFingerprint,
                pendingPublicKey: "",
                pendingKeyType: "",
                pendingKeyFingerprint: "",
                rotateTime: timestampFromDate(new Date("2026-11-27T11:00:00Z")),
              });
              return {
                status: GitDeployKeyTestStatus.OK,
                promoted: true,
                oldKeyFingerprint: oldFP,
                defaultBranch: "main",
                branches: ["main"],
                key: storedKey,
              };
            }
            return { status: GitDeployKeyTestStatus.OK, defaultBranch: "main", branches: ["main"], key: storedKey };
          },
        },
      }),
    );

    await user.click(await screen.findByRole("button", { name: "Manage deploy key" }));
    const sheet = await screen.findByRole("dialog", { name: "Deploy key acme-web" });
    await user.click(within(sheet).getByRole("button", { name: "Rotate key" }));

    expect(await within(sheet).findByTestId("deploy-key-pending-pubkey")).toHaveTextContent("INewPendingKeyMaterial");
    await user.click(within(sheet).getByRole("button", { name: "Test new key" }));

    expect(await within(sheet).findByText("Deploy key rotated")).toBeVisible();
    expect(within(sheet).getByText("SHA256:KeyFingerprint000000000000000000000000000000")).toBeVisible();
    await expectAccessible(container);
  });

  it("hides Accept new host key from a project developer when a host key changed", async () => {
    const user = userEvent.setup();
    const changedKey = makeKey({
      hostKeyAlgorithm: "ssh-ed25519",
      hostKeyFingerprint: "SHA256:OldHostKey0000000000000000000000000000000000",
      hostKeyPinned: true,
      hostKeyChanged: true,
      observedHostKeyAlgorithm: "ecdsa-sha2-nistp256",
      observedHostKeyFingerprint: "SHA256:NewHostKey0000000000000000000000000000000000",
      usedBy: ["web"],
    });
    const app = anApp(
      "web",
      "dockwell.local/built:1",
      {},
      {
        spec: create(ServiceSchema, {
          runs: { case: "app", value: { image: "dockwell.local/built:1" } },
          build: {
            source: { case: "git", value: { url: "ssh://git@github.com/acme/web.git", ref: "refs/heads/main" } },
            gitLogin: "ssh:acme-web",
          },
        }),
      },
    );

    renderAt(
      "/apps/svc_web",
      baseApi({
        auth: {
          getAuthState: () => authState({ grants: [{ projectId: "prj_1", role: Role.DEVELOPER }] }),
        },
        services: { listServices: () => ({ services: [app] }), listServiceRevisions: () => ({}) },
        gitKeys: { listGitDeployKeys: () => ({ keys: [changedKey] }) },
      }),
    );

    await user.click(await screen.findByRole("button", { name: "Review deploy key" }));
    const sheet = await screen.findByRole("dialog", { name: "Deploy key acme-web" });
    expect(within(sheet).getByText("The SSH host key for github.com changed")).toBeVisible();
    expect(within(sheet).queryByRole("button", { name: "Accept new host key" })).not.toBeInTheDocument();
  });
});
