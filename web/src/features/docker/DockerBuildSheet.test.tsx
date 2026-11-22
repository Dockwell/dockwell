// SPDX-License-Identifier: AGPL-3.0-only

import { create } from "@bufbuild/protobuf";
import { Code, ConnectError } from "@connectrpc/connect";
import { DockerImageActionSchema, DockerImageOutcome, StepProgressSchema, StepState } from "@dockwell/api";
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
