// SPDX-License-Identifier: AGPL-3.0-only

import { create } from "@bufbuild/protobuf";
import { Code, ConnectError } from "@connectrpc/connect";
import {
  DockerImageActionSchema,
  DockerImageOutcome,
  GitDeployKeyInfoSchema,
  GitLabConnectionState,
  GitLabProjectConnectionSchema,
  GitLabProjectSchema,
  ResourceSchema,
  StepProgressSchema,
  StepState,
} from "@dockwell/api";
import { fireEvent, screen, waitFor } from "@testing-library/react";
import { expect, it, vi } from "vitest";

import { authState } from "../../test/auth";
import { renderWithApi } from "../../test/render";
import { ChangeSheetProvider } from "../changes/ChangeSheet";
import { DockerBuildStep } from "../changes/DockerBuildStep";
import { DockerBuildSheet } from "./DockerBuildSheet";

it("shows the folder preparation state while a plan is in flight", async () => {
  let finish = () => {};
  const plan = new Promise<never>((_resolve, reject) => {
    finish = () => reject(new ConnectError("fixture finished", Code.Canceled));
  });
  const ask = vi.fn(() => plan);
  renderWithApi(
    <ChangeSheetProvider>
      <DockerBuildSheet servers={[{ id: "fixture", name: "Fixture server" }]} close={() => {}} refresh={() => {}} />
    </ChangeSheetProvider>,
    { docker: { planDockerMaintenance: ask }, files: { listFiles: ({ path }) => ({ path }) } },
  );
  fireEvent.click(screen.getByRole("button", { name: "Choose a folder" }));
  fireEvent.change(screen.getByLabelText("Path"), { target: { value: "/srv/fixture" } });
  fireEvent.click(screen.getByRole("button", { name: "Done" }));
  fireEvent.change(screen.getByLabelText("Local image name"), { target: { value: "fixture/output" } });
  fireEvent.click(screen.getByRole("button", { name: "Review plan" }));
  try {
    expect(
      await screen.findByText("Preparing the build folder and checking base images", { selector: "p" }),
    ).toBeVisible();
    expect(ask).toHaveBeenCalledWith(expect.objectContaining({ serverIds: ["fixture"] }), expect.anything());
  } finally {
    finish();
  }
});

it("shows publication after a late cancel and then shows the Engine's receipt", async () => {
  const action = create(DockerImageActionSchema, { build: { output: "fixture/output:dockwell-build" } });
  const step = (live: Parameters<typeof DockerBuildStep>[0]["live"]) => (
    <ol>
      <DockerBuildStep action={action} changeId="fixture-change" serverId="fixture" live={live} />
    </ol>
  );
  const { rerender } = renderWithApi(step(create(StepProgressSchema, { state: StepState.RUNNING })), {
    auth: { getAuthState: () => authState() },
    docker: {
      cancelDockerImageBuild: () => {
        throw new ConnectError("the build result is being published", Code.FailedPrecondition, {
          "Dockwell-Error": "build_publishing",
        });
      },
    },
  });
  fireEvent.click(await screen.findByRole("button", { name: "Cancel build" }));
  expect(await screen.findByText("Docker is already adding the image")).toBeVisible();
  expect(screen.queryByRole("button", { name: "Cancel build" })).not.toBeInTheDocument();
  rerender(
    step(
      create(StepProgressSchema, {
        state: StepState.DONE,
        dockerImageReceipts: [
          { outcome: DockerImageOutcome.APPLIED, imageKey: { imageId: `sha256:${"1".repeat(64)}` } },
        ],
      }),
    ),
  );
  await waitFor(() => expect(screen.getByText("Built")).toBeVisible());
  expect(screen.getByText("111111111111")).toBeVisible();
  expect(screen.queryByText("Docker is already adding the image")).not.toBeInTheDocument();
});

it("picks a GitLab project and passes its gitlab login (preferring it over a matching deploy key)", async () => {
  const ask = vi.fn(() => {
    throw new ConnectError("fixture finished", Code.Canceled);
  });
  renderWithApi(
    <ChangeSheetProvider>
      <DockerBuildSheet servers={[{ id: "fixture", name: "Fixture server" }]} close={() => {}} refresh={() => {}} />
    </ChangeSheetProvider>,
    {
      resources: {
        listResources: () => ({
          resources: [
            create(ResourceSchema, {
              id: "prj_1",
              displayName: "acme",
              resource: { kind: "project", name: "acme" },
            }),
          ],
        }),
      },
      gitProviders: {
        listGitLabProjects: () => ({
          connections: [
            create(GitLabProjectConnectionSchema, {
              id: "glc_1",
              name: "corp",
              url: "https://gitlab.example.com",
              state: GitLabConnectionState.CONNECTED,
            }),
          ],
          projects: [
            create(GitLabProjectSchema, {
              pathWithNamespace: "acme/web",
              url: "https://gitlab.example.com/acme/web",
              connectionId: "glc_1",
              connectionName: "corp",
              defaultBranch: "main",
            }),
          ],
        }),
      },
      gitKeys: {
        listGitDeployKeys: () => ({
          keys: [
            create(GitDeployKeyInfoSchema, {
              id: "gdk_1",
              name: "acme-web",
              projectId: "prj_1",
              repository: "ssh://git@gitlab.example.com/acme/web.git",
              gitLogin: "ssh:acme-web",
              hostKeyPinned: true,
              verified: true,
            }),
          ],
        }),
      },
      docker: { planDockerMaintenance: ask },
    },
  );

  fireEvent.change(screen.getByLabelText("Source"), { target: { value: "git" } });
  await waitFor(() => expect(screen.getByRole("option", { name: "acme" })).toBeInTheDocument());
  fireEvent.change(screen.getByLabelText("Git access"), { target: { value: "prj_1" } });

  const repoInput = await screen.findByLabelText("Repository");
  fireEvent.focus(repoInput);
  const option = await screen.findByRole("option", { name: /acme\/web/ });
  expect(option).toHaveTextContent("Prefers connection");
  fireEvent.click(option);

  expect(await screen.findByText(/Through GitLab \(corp, preferred over deploy key acme-web\)/)).toBeVisible();

  fireEvent.change(screen.getByLabelText("Local image name"), { target: { value: "example/gl-img" } });
  fireEvent.click(screen.getByRole("button", { name: "Review plan" }));

  await waitFor(() =>
    expect(ask).toHaveBeenCalledWith(
      expect.objectContaining({
        operation: expect.objectContaining({
          case: "build",
          value: expect.objectContaining({
            git: expect.objectContaining({
              repository: "https://gitlab.example.com/acme/web",
              gitLogin: "gitlab:corp",
              project: "prj_1",
            }),
          }),
        }),
      }),
      expect.anything(),
    ),
  );
});

it("uses a project SSH deploy key for a standalone Docker image build", async () => {
  const ask = vi.fn(() => {
    throw new ConnectError("fixture finished", Code.Canceled);
  });
  renderWithApi(
    <ChangeSheetProvider>
      <DockerBuildSheet servers={[{ id: "fixture", name: "Fixture server" }]} close={() => {}} refresh={() => {}} />
    </ChangeSheetProvider>,
    {
      resources: {
        listResources: () => ({
          resources: [
            create(ResourceSchema, {
              id: "prj_1",
              displayName: "acme",
              resource: { kind: "project", name: "acme" },
            }),
          ],
        }),
      },
      gitKeys: {
        listGitDeployKeys: () => ({
          keys: [
            create(GitDeployKeyInfoSchema, {
              id: "gdk_1",
              name: "ssh-repo",
              projectId: "prj_1",
              repository: "ssh://git@git.example.com/acme/ssh-repo.git",
              gitLogin: "ssh:ssh-repo",
              hostKeyPinned: true,
              hostKeyFingerprint: "SHA256:testfp",
              verified: true,
            }),
          ],
        }),
      },
      docker: { planDockerMaintenance: ask },
    },
  );

  fireEvent.change(screen.getByLabelText("Source"), { target: { value: "git" } });
  await waitFor(() => expect(screen.getByRole("option", { name: "acme" })).toBeInTheDocument());
  fireEvent.change(screen.getByLabelText("Git access"), { target: { value: "prj_1" } });

  const repoInput = await screen.findByLabelText("Repository");
  fireEvent.change(repoInput, { target: { value: "ssh://git@git.example.com/acme/ssh-repo.git" } });
  expect(await screen.findByTestId("deploy-key-ready")).toBeVisible();

  fireEvent.change(screen.getByLabelText("Local image name"), { target: { value: "example/ssh-img" } });
  fireEvent.click(screen.getByRole("button", { name: "Review plan" }));

  await waitFor(() =>
    expect(ask).toHaveBeenCalledWith(
      expect.objectContaining({
        operation: expect.objectContaining({
          case: "build",
          value: expect.objectContaining({
            git: expect.objectContaining({
              repository: "ssh://git@git.example.com/acme/ssh-repo.git",
              gitLogin: "ssh:ssh-repo",
              project: "prj_1",
            }),
          }),
        }),
      }),
      expect.anything(),
    ),
  );
});
