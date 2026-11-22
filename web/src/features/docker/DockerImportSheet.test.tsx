// SPDX-License-Identifier: AGPL-3.0-only

import { create } from "@bufbuild/protobuf";
import {
  DockerArtifactInfoSchema,
  DockerArtifactKind,
  DockerArtifactState,
  StreamError,
  uploadDockerArtifactPart,
} from "@dockwell/api";
import { fireEvent, screen, waitFor } from "@testing-library/react";
import { afterEach, expect, it, vi } from "vitest";

import { renderWithApi } from "../../test/render";
import { DockerImportSheet } from "./DockerArchiveSheets";

vi.mock("@dockwell/api", async (load) => ({
  ...(await load<typeof import("@dockwell/api")>()),
  uploadDockerArtifactPart: vi.fn(),
}));
vi.mock("./archiveTransfer", async (load) => ({
  ...(await load<typeof import("./archiveTransfer")>()),
  archiveHash: () => ({ digest: async () => "prefix", close: () => {} }),
}));

afterEach(() => {
  sessionStorage.clear();
  vi.resetAllMocks();
});

it("shows it is waiting and retries the same extent when the transfer lane is full", async () => {
  const file = new File(["some archive bytes"], "images.tar");
  const lease = create(DockerArtifactInfoSchema, {
    artifactId: "archive",
    state: DockerArtifactState.RECEIVING,
    kind: DockerArtifactKind.IMAGE_IMPORT,
    sizeBytes: BigInt(file.size),
    committedPrefixSha256: "prefix",
  });
  const checking = create(DockerArtifactInfoSchema, {
    ...lease,
    state: DockerArtifactState.CHECKING,
    receivedBytes: BigInt(file.size),
  });
  const upload = vi.mocked(uploadDockerArtifactPart);
  upload.mockRejectedValueOnce(new StreamError(429, "waiting for a transfer slot", "artifact_transfer_slots"));
  upload.mockResolvedValueOnce(checking);
  const finish = vi.fn(() => checking);
  renderWithApi(
    <DockerImportSheet servers={[{ id: "fixture", name: "Fixture server" }]} close={() => {}} refresh={() => {}} />,
    {
      docker: {
        beginDockerArtifactUpload: () => lease,
        getDockerArtifact: () => checking,
        finishDockerArtifactUpload: finish,
      },
    },
  );
  fireEvent.change(screen.getByLabelText("Docker image archive"), { target: { files: [file] } });
  fireEvent.click(screen.getByRole("button", { name: "Upload archive" }));
  expect(await screen.findByText("Waiting for a transfer slot", { selector: "p" })).toBeVisible();
  expect(screen.getByRole("progressbar", { name: "images.tar" })).not.toHaveAttribute("value");
  expect(screen.getByRole("button", { name: "Pause" })).toBeEnabled();
  expect(screen.queryByText(/artifact changed since/i)).not.toBeInTheDocument();
  await waitFor(() => expect(finish).toHaveBeenCalledOnce(), { timeout: 3000 });
  expect(upload.mock.calls.map(([part]) => [part.offset, part.prefixSHA256])).toEqual([
    [0, "prefix"],
    [0, "prefix"],
  ]);
});

it("resumes from the verified offset when a paused request committed after the offset read", async () => {
  const file = new File(["some archive bytes"], "images.tar");
  const lease = create(DockerArtifactInfoSchema, {
    artifactId: "archive",
    state: DockerArtifactState.RECEIVING,
    kind: DockerArtifactKind.IMAGE_IMPORT,
    sizeBytes: BigInt(file.size),
    committedPrefixSha256: "prefix",
  });
  const committed = create(DockerArtifactInfoSchema, { ...lease, receivedBytes: 5n });
  const checking = create(DockerArtifactInfoSchema, {
    ...lease,
    receivedBytes: BigInt(file.size),
    state: DockerArtifactState.CHECKING,
  });
  const upload = vi.mocked(uploadDockerArtifactPart);
  upload.mockRejectedValueOnce(new StreamError(412, "artifact changed since the last transfer", ""));
  upload.mockResolvedValueOnce(checking);
  const finish = vi.fn(() => checking);
  renderWithApi(
    <DockerImportSheet servers={[{ id: "fixture", name: "Fixture server" }]} close={() => {}} refresh={() => {}} />,
    {
      docker: {
        beginDockerArtifactUpload: () => lease,
        getDockerArtifact: () => committed,
        finishDockerArtifactUpload: finish,
      },
    },
  );
  fireEvent.change(screen.getByLabelText("Docker image archive"), { target: { files: [file] } });
  fireEvent.click(screen.getByRole("button", { name: "Upload archive" }));
  await waitFor(() => expect(finish).toHaveBeenCalledOnce());
  expect(upload.mock.calls.map(([part]) => part.offset)).toEqual([0, 5]);
  expect(screen.queryByText(/artifact changed since/i)).not.toBeInTheDocument();
});
